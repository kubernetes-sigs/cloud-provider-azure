// Package difftracker provides state tracking and synchronization between Kubernetes
// resources and Azure Network Resource Provider (NRP) resources.
//
// This file contains all finalizer-related functionality for ServiceGateway resources:
// - Service finalizers: prevent service deletion until Azure LB/NAT Gateway resources are cleaned up
// - Pod finalizers: prevent egress pod deletion until location/address is synced out of NRP
package difftracker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	servicehelper "k8s.io/cloud-provider/service/helpers"
)

// Retry configuration for finalizer removal operations
var finalizerRetryBackoff = wait.Backoff{
	Duration: 100 * time.Millisecond, // Initial delay
	Factor:   2.0,                    // Exponential factor
	Jitter:   0.1,                    // 10% jitter
	Steps:    5,                      // Max retries
	Cap:      5 * time.Second,        // Max delay
}

// ================================================================================================
// FINALIZER CONSTANTS
// ================================================================================================

const (
	// ServiceGatewayServiceCleanupFinalizer is added to services managed by ServiceGateway
	// to ensure Azure resources are cleaned up before the K8s service is deleted.
	// This is distinct from the standard LoadBalancerCleanupFinalizer used by the non-SG path.
	ServiceGatewayServiceCleanupFinalizer = "servicegateway.azure.com/service-cleanup"

	// ServiceGatewayPodCleanupFinalizer is added to pods with egress labels
	// to ensure their location/address is synced to NRP before the pod is deleted.
	// For non-last pods: removed after location sync completes
	// For last pod: removed after NAT Gateway deletion completes
	ServiceGatewayPodCleanupFinalizer = "servicegateway.azure.com/pod-cleanup"
)

// ================================================================================================
// PENDING DELETION TYPES
// ================================================================================================

// PendingPodDeletion tracks a pod waiting for its location to be synced to NRP before finalizer removal
type PendingPodDeletion struct {
	Namespace  string   // Pod namespace
	Name       string   // Pod name
	UID        string   // Pod UID; guards against stripping a same-name replacement pod's finalizer
	ServiceUID string   // Egress service this pod belongs to
	Addresses  []string // PodIPs; a dual-stack pod contributes one address per IP family
	IsLastPod  bool     // True if this was the last pod for the service (finalizer removed after NAT GW deletion)
	Timestamp  string
}

// ================================================================================================
// HELPER FUNCTIONS
// ================================================================================================

// removeFinalizerString returns a new slice with the specified string removed
func removeFinalizerString(slice []string, s string) []string {
	return slices.DeleteFunc(slice, func(item string) bool {
		return item == s
	})
}

// hasFinalizer checks if the given finalizer exists in the slice
func hasFinalizer(finalizers []string, finalizer string) bool {
	return slices.Contains(finalizers, finalizer)
}

// ================================================================================================
// SERVICE FINALIZER OPERATIONS
// ================================================================================================

// hasServiceGatewayFinalizer checks if service has the ServiceGateway cleanup finalizer
func hasServiceGatewayFinalizer(service *v1.Service) bool {
	return hasFinalizer(service.ObjectMeta.Finalizers, ServiceGatewayServiceCleanupFinalizer)
}

// addServiceGatewayFinalizer adds the ServiceGateway cleanup finalizer to the service
// This prevents Kubernetes from deleting the service until Azure resources are cleaned up
// IMPORTANT: We also add the K8s LoadBalancerCleanupFinalizer so that the upstream
// service controller's needsCleanup() returns true when the service is being deleted.
// This ensures EnsureLoadBalancerDeleted is called, which triggers our async deletion flow.
// Implements retry with exponential backoff for resilience against transient API failures.
func (dt *DiffTracker) addServiceGatewayFinalizer(ctx context.Context, service *v1.Service) error {
	if hasServiceGatewayFinalizer(service) {
		return nil
	}

	namespace := service.Namespace
	name := service.Name
	var lastErr error

	retryErr := wait.ExponentialBackoff(finalizerRetryBackoff, func() (bool, error) {
		// Get fresh service to avoid conflicts
		currentSvc, err := dt.kubeClient.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Service deleted, nothing to do
				return true, nil
			}
			lastErr = err
			dt.logger.V(4).Info("Transient error getting service, will retry", "namespace", namespace, "name", name, "err", err)
			return false, nil // Retry
		}

		// Check if already has finalizer (may have been added by concurrent operation)
		if hasServiceGatewayFinalizer(currentSvc) {
			return true, nil
		}

		// Make a copy so we don't mutate the shared informer cache
		updated := currentSvc.DeepCopy()
		updated.ObjectMeta.Finalizers = append(updated.ObjectMeta.Finalizers, ServiceGatewayServiceCleanupFinalizer)

		// Also add the K8s LoadBalancerCleanupFinalizer if not present.
		// This is critical: the upstream K8s service controller uses HasLBFinalizer()
		// in needsCleanup() to determine if a service being deleted needs cleanup.
		// Without this finalizer, the controller tries to add it (which fails since
		// the service is being deleted), and never calls EnsureLoadBalancerDeleted.
		if !servicehelper.HasLBFinalizer(currentSvc) {
			updated.ObjectMeta.Finalizers = append(updated.ObjectMeta.Finalizers, servicehelper.LoadBalancerCleanupFinalizer)
		}

		dt.logger.V(5).Info("Adding ServiceGateway finalizer to service", "namespace", namespace, "name", name)
		_, err = servicehelper.PatchService(dt.kubeClient.CoreV1(), currentSvc, updated)
		if err != nil {
			lastErr = err
			dt.logger.V(4).Info("Transient error patching service, will retry", "namespace", namespace, "name", name, "err", err)
			return false, nil // Retry
		}

		return true, nil // Success
	})

	if retryErr != nil {
		return fmt.Errorf("failed to add finalizer after retries: %v (last error: %v)", retryErr, lastErr)
	}
	return nil
}

// removeServiceGatewayFinalizer removes the ServiceGateway cleanup finalizer from the service
// This allows Kubernetes to complete the service deletion after Azure resources are cleaned up
// NOTE: We also remove the K8s LoadBalancerCleanupFinalizer that we added in addServiceGatewayFinalizer
// Implements retry with exponential backoff for resilience against transient API failures.
func (dt *DiffTracker) removeServiceGatewayFinalizer(ctx context.Context, service *v1.Service) error {
	if !hasServiceGatewayFinalizer(service) {
		return nil
	}

	namespace := service.Namespace
	name := service.Name
	var lastErr error

	retryErr := wait.ExponentialBackoff(finalizerRetryBackoff, func() (bool, error) {
		// Get fresh service to avoid conflicts
		currentSvc, err := dt.kubeClient.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Service already deleted, finalizer effectively removed
				return true, nil
			}
			lastErr = err
			dt.logger.V(4).Info("Transient error getting service, will retry", "namespace", namespace, "name", name, "err", err)
			return false, nil // Retry
		}

		// Check if finalizer already removed (may have been removed by concurrent operation)
		if !hasServiceGatewayFinalizer(currentSvc) {
			return true, nil
		}

		// Make a copy so we don't mutate the shared informer cache
		updated := currentSvc.DeepCopy()
		updated.ObjectMeta.Finalizers = removeFinalizerString(updated.ObjectMeta.Finalizers, ServiceGatewayServiceCleanupFinalizer)
		// Also remove the K8s LoadBalancerCleanupFinalizer that we added
		updated.ObjectMeta.Finalizers = removeFinalizerString(updated.ObjectMeta.Finalizers, servicehelper.LoadBalancerCleanupFinalizer)

		dt.logger.V(5).Info("Removing ServiceGateway finalizer from service", "namespace", namespace, "name", name)
		_, err = servicehelper.PatchService(dt.kubeClient.CoreV1(), currentSvc, updated)
		if err != nil {
			lastErr = err
			dt.logger.V(4).Info("Transient error patching service, will retry", "namespace", namespace, "name", name, "err", err)
			return false, nil // Retry
		}

		return true, nil // Success
	})

	if retryErr != nil {
		return fmt.Errorf("failed to remove finalizer after retries: %v (last error: %v)", retryErr, lastErr)
	}
	return nil
}

// ================================================================================================
// POD FINALIZER OPERATIONS
// ================================================================================================

// HasPodFinalizer checks if pod has the ServiceGateway pod cleanup finalizer.
// This is exported for use by provider layer to check pod state during recovery.
func HasPodFinalizer(pod *v1.Pod) bool {
	return hasFinalizer(pod.ObjectMeta.Finalizers, ServiceGatewayPodCleanupFinalizer)
}

// hasPodFinalizer is an alias for internal use
func hasPodFinalizer(pod *v1.Pod) bool {
	return HasPodFinalizer(pod)
}

// getPodByNamespaceName retrieves a pod from the API server
func (dt *DiffTracker) getPodByNamespaceName(ctx context.Context, namespace, name string) (*v1.Pod, error) {
	pod, err := dt.kubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getPodByNamespaceName: get failed: %w", err)
	}
	return pod, nil
}

// AddPodFinalizer adds the ServiceGateway pod cleanup finalizer, gating pod deletion until the
// address is synced out of NRP. It always GETs a fresh copy (retrying with backoff on conflicts;
// a missing pod is success) rather than trusting the informer-cache finalizer list, so it re-adds a
// concurrently-stripped finalizer. It is UID-guarded so a same-name replacement pod is never given
// the finalizer (removePodFinalizer would then refuse to strip it, stranding the replacement).
// ErrPodGoneOrReplaced is returned by AddPodFinalizer when the target pod no longer exists or has
// been replaced by a same-name pod with a different UID. The finalizer is intentionally not added
// (removePodFinalizer is UID-guarded), so the caller must skip registering the stale event pod
// rather than treat this as a successful add.
var ErrPodGoneOrReplaced = errors.New("pod gone or replaced by a same-name UID; skip egress registration")

func (dt *DiffTracker) AddPodFinalizer(ctx context.Context, pod *v1.Pod) error {
	namespace := pod.Namespace
	name := pod.Name
	intendedUID := string(pod.UID)
	var lastErr error
	goneOrReplaced := false

	retryErr := wait.ExponentialBackoff(finalizerRetryBackoff, func() (bool, error) {
		// Get fresh pod to avoid conflicts with concurrent status updates
		currentPod, err := dt.kubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Pod deleted: nothing to finalize, and the caller must not register it.
				goneOrReplaced = true
				return true, nil
			}
			lastErr = err
			dt.logger.V(4).Info("Transient error getting pod, will retry", "namespace", namespace, "name", name, "err", err)
			return false, nil // Retry
		}

		// The named pod is now a different instance (the original target was deleted and a same-name
		// pod recreated). Do NOT add this finalizer to the replacement: removePodFinalizer is
		// UID-guarded and would refuse to strip it, stranding the replacement in Terminating. Signal
		// the caller so it skips registering the stale event pod's addresses.
		if intendedUID != "" && string(currentPod.UID) != intendedUID {
			dt.logger.V(4).Info("Pod UID changed (replacement pod); not adding finalizer", "namespace", namespace, "name", name, "wantUID", intendedUID, "gotUID", string(currentPod.UID))
			goneOrReplaced = true
			return true, nil
		}

		// Check if already has finalizer (may have been added by a concurrent operation)
		if hasPodFinalizer(currentPod) {
			return true, nil
		}

		// Make a copy so we don't mutate the shared informer cache
		updated := currentPod.DeepCopy()
		updated.ObjectMeta.Finalizers = append(updated.ObjectMeta.Finalizers, ServiceGatewayPodCleanupFinalizer)

		dt.logger.V(5).Info("Adding ServiceGateway pod finalizer to pod", "namespace", namespace, "name", name)
		if _, err = dt.kubeClient.CoreV1().Pods(namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			lastErr = err
			dt.logger.V(4).Info("Transient error updating pod, will retry", "namespace", namespace, "name", name, "err", err)
			return false, nil // Retry
		}

		return true, nil // Success
	})

	if retryErr != nil {
		return fmt.Errorf("failed to add pod finalizer after retries: %v (last error: %v)", retryErr, lastErr)
	}
	if goneOrReplaced {
		return ErrPodGoneOrReplaced
	}
	return nil
}

// removePodFinalizer removes the ServiceGateway pod cleanup finalizer from the pod
// This allows Kubernetes to complete the pod deletion after location is synced to NRP
// Uses retry logic to handle concurrent modifications during bulk pod deletions
func (dt *DiffTracker) removePodFinalizer(ctx context.Context, pod *v1.Pod) error {
	namespace := pod.Namespace
	name := pod.Name
	// Capture the UID of the pod we were asked to act on. A namespace/name can be reused by a
	// same-name replacement pod after the original is deleted (UIDs are unique in time and space),
	// so the Get-fresh below may return a different pod instance. Stripping that replacement's
	// finalizer would drop its NRP-drain protection. An empty intendedUID preserves the legacy
	// ns/name behaviour for callers that do not carry a UID.
	intendedUID := string(pod.UID)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Always get the latest version of the pod to avoid conflicts
		currentPod, err := dt.kubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Pod already deleted, finalizer effectively removed
				dt.logger.V(4).Info("Pod not found, finalizer already removed", "namespace", namespace, "name", name)
				return nil
			}
			return err
		}

		// The named pod is now a different instance (the original target was deleted and a
		// same-name pod recreated). Do not strip the replacement's finalizer; the original's
		// finalizer left with it when it was deleted.
		if intendedUID != "" && string(currentPod.UID) != intendedUID {
			dt.logger.V(4).Info("Pod UID changed (replacement pod); not stripping finalizer", "namespace", namespace, "name", name, "wantUID", intendedUID, "gotUID", string(currentPod.UID))
			return nil
		}

		if !hasPodFinalizer(currentPod) {
			return nil
		}

		// Make a copy so we don't mutate the cache
		updated := currentPod.DeepCopy()
		updated.ObjectMeta.Finalizers = removeFinalizerString(updated.ObjectMeta.Finalizers, ServiceGatewayPodCleanupFinalizer)

		dt.logger.V(5).Info("Removing ServiceGateway pod finalizer from pod", "namespace", namespace, "name", name)
		_, err = dt.kubeClient.CoreV1().Pods(namespace).Update(ctx, updated, metav1.UpdateOptions{})
		return err
	})
}

// RemovePodFinalizerByPod removes the ServiceGateway pod cleanup finalizer from a pod object
// directly (Get-fresh + retry under the hood). It is the fallback for the cases the drain-gated
// path does not cover: a pod the engine is not (or no longer) tracking, such as a stale/duplicate
// delete or a pod with no live address after a restart, has no overlay address to drain from NRP,
// so its finalizer can be removed immediately rather than waiting on a drain that will never fire.
func (dt *DiffTracker) RemovePodFinalizerByPod(ctx context.Context, pod *v1.Pod) error {
	return dt.removePodFinalizer(ctx, pod)
}

// ================================================================================================
// PENDING DELETION TRACKING - POD FINALIZERS
// ================================================================================================

// pendingPodToProcess is used internally to collect pending pods for processing
// without holding the lock during API calls
type pendingPodToProcess struct {
	Key       string
	Namespace string
	Name      string
	UID       string
}

// HasPendingPodDeletion reports whether a drain-gated deletion is still pending for the pod, i.e.
// an earlier delete event recorded addresses that have not yet drained from NRP. The UID check
// (an empty uid matches any) prevents a stale record left by a same-name replacement from matching.
// Callers use it to avoid stripping the pod-cleanup finalizer inline while a drain is in flight -
// notably when the kubelet has cleared the terminating pod's IPs, so the delete event carries none.
func (dt *DiffTracker) HasPendingPodDeletion(namespace, name, uid string) bool {
	if namespace == "" || name == "" {
		return false
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	existing, ok := dt.pendingPodDeletions[fmt.Sprintf("%s/%s", namespace, name)]
	return ok && (uid == "" || existing.UID == uid)
}

// CheckPendingPodDeletions checks pending pod deletions and removes finalizers for non-last pods
// whose addresses have been synced to NRP.
// For non-last pods (isLastPod=false): remove finalizer immediately after location sync
// For last pods (isLastPod=true): finalizer is removed in deleteOutboundService after NAT Gateway deletion
// Must be called AFTER CheckPendingServiceDeletions to ensure locations have been processed.
//
// It returns readyRemovalPending=true when at least one ready (address-drained, non-last) finalizer
// removal did NOT complete this cycle (a transient GET/Update failure), so the caller can reschedule
// a retry instead of reporting success. It is false when there is nothing ready to remove (last-pod
// entries waiting on NAT Gateway deletion, or non-last entries still waiting on the NRP drain, do
// not count - they must not force a retry spin).
func (dt *DiffTracker) CheckPendingPodDeletions(ctx context.Context) (readyRemovalPending bool) {
	// Phase 1: Collect pods ready for finalizer removal (with lock)
	dt.mu.Lock()

	if len(dt.pendingPodDeletions) == 0 {
		dt.mu.Unlock()
		return
	}

	dt.logger.V(5).Info("Checking pending pod deletions", "count", len(dt.pendingPodDeletions))

	var toProcess []pendingPodToProcess

	for podKey, pending := range dt.pendingPodDeletions {
		// For last pods, don't remove finalizer here - it will be removed after NAT Gateway deletion
		if pending.IsLastPod {
			dt.logger.V(4).Info("Skipped last pod, will be handled by outbound service deletion", "pod", podKey)
			continue
		}

		// For non-last pods, remove the finalizer only once ALL of the pod's addresses have drained
		// from NRP. A dual-stack pod registers one address per IP family under the same location, so
		// stripping the finalizer while any address is still mapped would let the pod (and that IP) be
		// reclaimed while NRP still routes it.
		if addr, waiting := dt.podAddressStillInNRPLocked(pending); waiting {
			dt.logger.V(4).Info("Address still in NRP for pod, waiting", "address", addr, "pod", podKey)
			continue
		}

		// All addresses are no longer in NRP, collect for finalizer removal
		dt.logger.V(5).Info("All addresses removed from NRP, will remove finalizer from pod", "addresses", pending.Addresses, "pod", podKey)

		toProcess = append(toProcess, pendingPodToProcess{
			Key:       podKey,
			Namespace: pending.Namespace,
			Name:      pending.Name,
			UID:       pending.UID,
		})
	}

	dt.mu.Unlock()

	if len(toProcess) == 0 {
		return
	}

	// Phase 2: Remove finalizers without holding lock (API calls)
	var processed []pendingPodToProcess

	for _, p := range toProcess {
		// Get the pod and remove finalizer
		pod, err := dt.getPodByNamespaceName(ctx, p.Namespace, p.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Pod genuinely gone - finalizer effectively removed; clean up tracking.
				dt.logger.V(4).Info("Pod not found, cleaning up tracking", "pod", p.Key)
				processed = append(processed, p)
				continue
			}
			// Transient error (5xx/429/etcd timeout - not a typed NotFound): keep the entry so a
			// later cycle retries. Dropping it here would forget the pending finalizer removal and
			// permanently strand the pod Terminating until a CCM restart re-seeds it.
			dt.logger.V(4).Info("Transient error getting pod, will retry next cycle", "pod", p.Key, "err", err)
			continue
		}

		// Guard against stripping a same-name replacement pod (e.g. a StatefulSet pod recreated
		// with the same namespace/name before this stale entry was processed): if the live pod's
		// UID differs from the one recorded at delete time, it is a different pod that still needs
		// its own NRP-drain protection - drop the stale entry without touching it. An empty
		// recorded UID preserves the legacy ns/name behaviour (e.g. recovered entries).
		if p.UID != "" && string(pod.UID) != p.UID {
			dt.logger.V(4).Info("Pod UID changed (replacement pod); dropping stale finalizer entry without stripping", "pod", p.Key, "wantUID", p.UID, "gotUID", string(pod.UID))
			processed = append(processed, p)
			continue
		}

		if err := dt.removePodFinalizer(ctx, pod); err != nil {
			dt.logger.V(4).Info("Could not remove finalizer from pod", "pod", p.Key, "err", err)
			// Don't add to processed, will retry next cycle
			continue
		}

		processed = append(processed, p)
	}

	// Phase 3: Clean up processed entries (with lock). Compare-and-delete on the recorded UID so a
	// replacement pod re-added to pendingPodDeletions during the unlocked phase 2 (delete -> recreate
	// -> re-delete of the same namespace/name) is not clobbered and keeps its own NRP-drain tracking.
	if len(processed) > 0 {
		dt.mu.Lock()
		for _, p := range processed {
			if cur, ok := dt.pendingPodDeletions[p.Key]; ok && cur.UID == p.UID {
				delete(dt.pendingPodDeletions, p.Key)
			}
		}
		remaining := len(dt.pendingPodDeletions)
		dt.mu.Unlock()

		dt.logger.V(2).Info("Processed pod deletions", "processed", len(processed), "remaining", remaining)
	}

	// A ready removal that was not processed this cycle (transient GET/Update failure) means the
	// caller should reschedule a retry rather than report success.
	return len(toProcess) > len(processed)
}

// podAddressStillInNRPLocked reports whether any of a pending pod's addresses is still in NRP for its
// service, searching every location (a dual-stack pod's families live under per-family node
// locations). The caller keeps the finalizer until every address has left NRP. Requires dt.mu held.
func (dt *DiffTracker) podAddressStillInNRPLocked(pending *PendingPodDeletion) (string, bool) {
	for _, address := range pending.Addresses {
		if dt.outboundAddressInAnyNRPLocationLocked(pending.ServiceUID, address) {
			return address, true
		}
	}
	return "", false
}

// outboundAddressInAnyNRPLocationLocked reports whether the given address is registered in NRP for
// the service under any node location. Must be called with dt.mu held.
func (dt *DiffTracker) outboundAddressInAnyNRPLocationLocked(serviceUID, address string) bool {
	for _, nrpLocation := range dt.NRPResources.Locations {
		nrpAddress, ok := nrpLocation.Addresses[address]
		if !ok {
			continue
		}
		if nrpAddress.Services != nil && nrpAddress.Services.Has(serviceUID) {
			return true
		}
	}
	return false
}

// appendAddressIfAbsent returns addresses with addr appended, unless it is already present.
func appendAddressIfAbsent(addresses []string, addr string) []string {
	if slices.Contains(addresses, addr) {
		return addresses
	}
	return append(addresses, addr)
}

// ================================================================================================
// LAST POD FINALIZER REMOVAL
// ================================================================================================

// RemoveLastPodFinalizers removes finalizers from pods that were marked as "last pod" for a service.
// This is called after the NAT Gateway has been successfully deleted.
// It uses the collect-unlock-process-relock pattern to avoid holding the mutex during API calls.
// Implements retry with exponential backoff for resilience against transient failures.
func (dt *DiffTracker) RemoveLastPodFinalizers(ctx context.Context, serviceUID string) error {
	// Phase 1: Collect last-pod entries to process (with lock)
	dt.mu.Lock()

	type lastPodEntry struct {
		Key       string
		Namespace string
		Name      string
		UID       string
	}
	var toProcess []lastPodEntry

	for podKey, pending := range dt.pendingPodDeletions {
		// Only process last-pod entries for this service
		if !pending.IsLastPod || pending.ServiceUID != serviceUID {
			continue
		}

		dt.logger.V(5).Info("Will remove finalizer from last pod after NAT Gateway deletion", "pod", podKey, "serviceUID", serviceUID)

		toProcess = append(toProcess, lastPodEntry{
			Key:       podKey,
			Namespace: pending.Namespace,
			Name:      pending.Name,
			UID:       pending.UID,
		})
	}

	dt.mu.Unlock()

	if len(toProcess) == 0 {
		return nil
	}

	// Phase 2: Remove finalizers without holding lock (API calls with retry)
	var processed []lastPodEntry
	var failed []string

	for _, p := range toProcess {
		var lastErr error

		// Retry with exponential backoff
		retryErr := wait.ExponentialBackoff(finalizerRetryBackoff, func() (bool, error) {
			// Get the pod fresh each attempt
			pod, err := dt.getPodByNamespaceName(ctx, p.Namespace, p.Name)
			if err != nil {
				if apierrors.IsNotFound(err) {
					// Pod already deleted, finalizer effectively removed
					dt.logger.V(4).Info("Last pod not found, cleaning up tracking", "pod", p.Key)
					return true, nil
				}
				lastErr = err
				dt.logger.V(4).Info("Transient error getting pod, will retry", "pod", p.Key, "err", err)
				return false, nil // Retry
			}

			// Guard against stripping a same-name replacement pod (see CheckPendingPodDeletions):
			// if the live pod's UID differs from the one recorded at delete time, this is a
			// different pod - drop the stale entry without stripping. An empty recorded UID
			// preserves the legacy ns/name behaviour.
			if p.UID != "" && string(pod.UID) != p.UID {
				dt.logger.V(4).Info("Last pod UID changed (replacement pod); dropping stale entry without stripping", "pod", p.Key, "wantUID", p.UID, "gotUID", string(pod.UID))
				return true, nil // Done - do not strip the replacement
			}

			if err := dt.removePodFinalizer(ctx, pod); err != nil {
				lastErr = err
				dt.logger.V(4).Info("Transient error removing finalizer from pod, will retry", "pod", p.Key, "err", err)
				return false, nil // Retry
			}

			return true, nil // Success
		})

		if retryErr != nil {
			// Exhausted retries
			dt.logger.V(4).Info("Could not remove finalizer from last pod after retries", "pod", p.Key, "err", retryErr, "lastErr", lastErr)
			failed = append(failed, p.Key)
		} else {
			processed = append(processed, p)
		}
	}

	// Phase 3: Clean up processed entries (with lock). Compare-and-delete on the recorded UID so a
	// same-name replacement pod re-added during the unlocked phase 2 keeps its own drain tracking.
	// Only remove successfully processed entries; failed ones will be retried on next cycle.
	if len(processed) > 0 {
		dt.mu.Lock()
		for _, p := range processed {
			if cur, ok := dt.pendingPodDeletions[p.Key]; ok && cur.UID == p.UID {
				delete(dt.pendingPodDeletions, p.Key)
			}
		}
		remaining := len(dt.pendingPodDeletions)
		dt.mu.Unlock()

		dt.logger.V(2).Info("Removed finalizers from last-pod entries for service", "processed", len(processed), "serviceUID", serviceUID, "failed", len(failed), "remaining", remaining)
	}

	// Surface retry-exhaustion so the caller keeps the delete op tracked and
	// retries instead of reporting success while a pod finalizer is still set.
	if len(failed) > 0 {
		return fmt.Errorf("RemoveLastPodFinalizers: %d last-pod finalizer(s) for service %s could not be removed after retries: %v", len(failed), serviceUID, failed)
	}
	return nil
}
