// Package difftracker provides state tracking and synchronization between Kubernetes
// resources and Azure Network Resource Provider (NRP) resources.
//
// This file contains all finalizer-related functionality for ServiceGateway resources:
// - Service finalizers: prevent service deletion until Azure LB/NAT Gateway resources are cleaned up
// - Pod finalizers: prevent egress pod deletion until location/address is synced out of NRP
package difftracker

import (
	"context"
	"fmt"
	"slices"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	servicehelper "k8s.io/cloud-provider/service/helpers"
	"k8s.io/klog/v2"
)

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
	Namespace  string // Pod namespace
	Name       string // Pod name
	ServiceUID string // Egress service this pod belongs to
	Address    string // PodIP
	Location   string // HostIP (NodeIP)
	IsLastPod  bool   // True if this was the last pod for the service (finalizer removed after NAT GW deletion)
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
func (dt *DiffTracker) addServiceGatewayFinalizer(ctx context.Context, service *v1.Service) error {
	if hasServiceGatewayFinalizer(service) {
		return nil
	}

	// Make a copy so we don't mutate the shared informer cache
	updated := service.DeepCopy()
	updated.ObjectMeta.Finalizers = append(updated.ObjectMeta.Finalizers, ServiceGatewayServiceCleanupFinalizer)

	// Also add the K8s LoadBalancerCleanupFinalizer if not present.
	// This is critical: the upstream K8s service controller uses HasLBFinalizer()
	// in needsCleanup() to determine if a service being deleted needs cleanup.
	// Without this finalizer, the controller tries to add it (which fails since
	// the service is being deleted), and never calls EnsureLoadBalancerDeleted.
	if !servicehelper.HasLBFinalizer(service) {
		updated.ObjectMeta.Finalizers = append(updated.ObjectMeta.Finalizers, servicehelper.LoadBalancerCleanupFinalizer)
	}

	klog.V(2).Infof("Adding ServiceGateway finalizer to service %s/%s", service.Namespace, service.Name)
	_, err := servicehelper.PatchService(dt.kubeClient.CoreV1(), service, updated)
	return err
}

// removeServiceGatewayFinalizer removes the ServiceGateway cleanup finalizer from the service
// This allows Kubernetes to complete the service deletion after Azure resources are cleaned up
// NOTE: We also remove the K8s LoadBalancerCleanupFinalizer that we added in addServiceGatewayFinalizer
func (dt *DiffTracker) removeServiceGatewayFinalizer(ctx context.Context, service *v1.Service) error {
	if !hasServiceGatewayFinalizer(service) {
		return nil
	}

	// Make a copy so we don't mutate the shared informer cache
	updated := service.DeepCopy()
	updated.ObjectMeta.Finalizers = removeFinalizerString(updated.ObjectMeta.Finalizers, ServiceGatewayServiceCleanupFinalizer)
	// Also remove the K8s LoadBalancerCleanupFinalizer that we added
	updated.ObjectMeta.Finalizers = removeFinalizerString(updated.ObjectMeta.Finalizers, servicehelper.LoadBalancerCleanupFinalizer)

	klog.V(2).Infof("Removing ServiceGateway finalizer from service %s/%s", service.Namespace, service.Name)
	_, err := servicehelper.PatchService(dt.kubeClient.CoreV1(), service, updated)
	return err
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

// AddPodFinalizer adds the ServiceGateway pod cleanup finalizer to the pod.
// This is called from pod informer before registering the pod with the engine.
// It prevents Kubernetes from deleting the pod until location is synced to NRP.
func (dt *DiffTracker) AddPodFinalizer(ctx context.Context, pod *v1.Pod) error {
	if hasPodFinalizer(pod) {
		return nil
	}

	// Make a copy so we don't mutate the shared informer cache
	updated := pod.DeepCopy()
	updated.ObjectMeta.Finalizers = append(updated.ObjectMeta.Finalizers, ServiceGatewayPodCleanupFinalizer)

	klog.V(2).Infof("Adding ServiceGateway pod finalizer to pod %s/%s", pod.Namespace, pod.Name)
	_, err := dt.kubeClient.CoreV1().Pods(pod.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

// removePodFinalizer removes the ServiceGateway pod cleanup finalizer from the pod
// This allows Kubernetes to complete the pod deletion after location is synced to NRP
// Uses retry logic to handle concurrent modifications during bulk pod deletions
func (dt *DiffTracker) removePodFinalizer(ctx context.Context, pod *v1.Pod) error {
	namespace := pod.Namespace
	name := pod.Name

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Always get the latest version of the pod to avoid conflicts
		currentPod, err := dt.kubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Pod already deleted, finalizer effectively removed
				klog.V(4).Infof("Pod %s/%s not found, finalizer already removed", namespace, name)
				return nil
			}
			return err
		}

		if !hasPodFinalizer(currentPod) {
			return nil
		}

		// Make a copy so we don't mutate the cache
		updated := currentPod.DeepCopy()
		updated.ObjectMeta.Finalizers = removeFinalizerString(updated.ObjectMeta.Finalizers, ServiceGatewayPodCleanupFinalizer)

		klog.V(2).Infof("Removing ServiceGateway pod finalizer from pod %s/%s", namespace, name)
		_, err = dt.kubeClient.CoreV1().Pods(namespace).Update(ctx, updated, metav1.UpdateOptions{})
		return err
	})
}

// RemovePodFinalizerByPod removes the finalizer from a pod object directly.
// This is used for non-last pods where we remove the finalizer immediately without tracking.
// For last pods, use RemoveLastPodFinalizers after NAT Gateway deletion.
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
}

// CheckPendingPodDeletions checks pending pod deletions and removes finalizers for non-last pods
// whose addresses have been synced to NRP.
// For non-last pods (isLastPod=false): remove finalizer immediately after location sync
// For last pods (isLastPod=true): finalizer is removed in deleteOutboundService after NAT Gateway deletion
// Must be called AFTER CheckPendingServiceDeletions to ensure locations have been processed
func (dt *DiffTracker) CheckPendingPodDeletions(ctx context.Context) {
	// Phase 1: Collect pods ready for finalizer removal (with lock)
	dt.mu.Lock()

	if len(dt.pendingPodDeletions) == 0 {
		dt.mu.Unlock()
		return
	}

	klog.V(3).Infof("CheckPendingPodDeletions: checking %d pending pod deletions", len(dt.pendingPodDeletions))

	var toProcess []pendingPodToProcess

	for podKey, pending := range dt.pendingPodDeletions {
		// For last pods, don't remove finalizer here - it will be removed after NAT Gateway deletion
		if pending.IsLastPod {
			klog.V(4).Infof("CheckPendingPodDeletions: skipping last pod %s (will be handled by deleteOutboundService)", podKey)
			continue
		}

		// For non-last pods, check if the address has been removed from NRP
		// This means the location sync is complete
		addressInNRP := dt.isAddressInNRPLocked(pending.ServiceUID, pending.Location, pending.Address)
		if addressInNRP {
			klog.V(4).Infof("CheckPendingPodDeletions: address %s still in NRP for pod %s, waiting", pending.Address, podKey)
			continue
		}

		// Address is no longer in NRP, collect for finalizer removal
		klog.V(2).Infof("CheckPendingPodDeletions: address %s removed from NRP, will remove finalizer for pod %s",
			pending.Address, podKey)

		toProcess = append(toProcess, pendingPodToProcess{
			Key:       podKey,
			Namespace: pending.Namespace,
			Name:      pending.Name,
		})
	}

	dt.mu.Unlock()

	if len(toProcess) == 0 {
		return
	}

	// Phase 2: Remove finalizers without holding lock (API calls)
	var processed []string

	for _, p := range toProcess {
		// Get the pod and remove finalizer
		pod, err := dt.getPodByNamespaceName(ctx, p.Namespace, p.Name)
		if err != nil {
			// Pod not found - already deleted, clean up tracking
			klog.V(3).Infof("CheckPendingPodDeletions: pod %s not found, cleaning up tracking", p.Key)
			processed = append(processed, p.Key)
			continue
		}

		if err := dt.removePodFinalizer(ctx, pod); err != nil {
			klog.Warningf("CheckPendingPodDeletions: failed to remove finalizer from pod %s: %v", p.Key, err)
			// Don't add to processed, will retry next cycle
			continue
		}

		klog.V(2).Infof("CheckPendingPodDeletions: successfully removed finalizer from pod %s", p.Key)
		processed = append(processed, p.Key)
	}

	// Phase 3: Clean up processed entries (with lock)
	if len(processed) > 0 {
		dt.mu.Lock()
		for _, podKey := range processed {
			delete(dt.pendingPodDeletions, podKey)
		}
		remaining := len(dt.pendingPodDeletions)
		dt.mu.Unlock()

		klog.V(2).Infof("CheckPendingPodDeletions: processed %d pod deletions, %d remaining",
			len(processed), remaining)
	}
}

// isAddressInNRPLocked checks if a specific address exists in NRP for a service/location
// Must be called with dt.mu held
func (dt *DiffTracker) isAddressInNRPLocked(serviceUID, location, address string) bool {
	nrpLocation, exists := dt.NRPResources.Locations[location]
	if !exists {
		return false
	}

	nrpAddress, exists := nrpLocation.Addresses[address]
	if !exists {
		return false
	}

	// Check if this service still has this address registered
	if nrpAddress.Services == nil {
		return false
	}
	return nrpAddress.Services.Has(serviceUID)
}

// ================================================================================================
// LAST POD FINALIZER REMOVAL
// ================================================================================================

// RemoveLastPodFinalizers removes finalizers from pods that were marked as "last pod" for a service.
// This is called after the NAT Gateway has been successfully deleted.
// It uses the collect-unlock-process-relock pattern to avoid holding the mutex during API calls.
func (dt *DiffTracker) RemoveLastPodFinalizers(ctx context.Context, serviceUID string) {
	// Phase 1: Collect last-pod entries to process (with lock)
	dt.mu.Lock()

	type lastPodEntry struct {
		Key       string
		Namespace string
		Name      string
	}
	var toProcess []lastPodEntry

	for podKey, pending := range dt.pendingPodDeletions {
		// Only process last-pod entries for this service
		if !pending.IsLastPod || pending.ServiceUID != serviceUID {
			continue
		}

		klog.V(2).Infof("RemoveLastPodFinalizers: will remove finalizer from last pod %s after NAT Gateway %s deletion",
			podKey, serviceUID)

		toProcess = append(toProcess, lastPodEntry{
			Key:       podKey,
			Namespace: pending.Namespace,
			Name:      pending.Name,
		})
	}

	dt.mu.Unlock()

	if len(toProcess) == 0 {
		return
	}

	// Phase 2: Remove finalizers without holding lock (API calls)
	var processed []string

	for _, p := range toProcess {
		// Get the pod and remove finalizer
		pod, err := dt.getPodByNamespaceName(ctx, p.Namespace, p.Name)
		if err != nil {
			// Pod not found - already deleted, clean up tracking
			klog.V(3).Infof("RemoveLastPodFinalizers: last pod %s not found, cleaning up tracking", p.Key)
			processed = append(processed, p.Key)
			continue
		}

		if err := dt.removePodFinalizer(ctx, pod); err != nil {
			klog.Warningf("RemoveLastPodFinalizers: failed to remove finalizer from last pod %s: %v", p.Key, err)
			// Still add to processed - best effort, pod can be manually cleaned up
		} else {
			klog.V(2).Infof("RemoveLastPodFinalizers: successfully removed finalizer from last pod %s", p.Key)
		}

		processed = append(processed, p.Key)
	}

	// Phase 3: Clean up processed entries (with lock)
	if len(processed) > 0 {
		dt.mu.Lock()
		for _, podKey := range processed {
			delete(dt.pendingPodDeletions, podKey)
		}
		dt.mu.Unlock()

		klog.V(2).Infof("RemoveLastPodFinalizers: removed finalizers from %d last-pod entries for service %s",
			len(processed), serviceUID)
	}
}
