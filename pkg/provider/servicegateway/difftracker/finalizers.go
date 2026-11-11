/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
	"maps"
	"slices"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	servicehelper "k8s.io/cloud-provider/service/helpers"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
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

// PendingPodDeletion tracks egress cleanup and finalizer retries for one Pod.
type PendingPodDeletion struct {
	Namespace           string   // Pod namespace
	Name                string   // Pod name
	UID                 string   // Pod UID; guards against stripping a same-name replacement pod's finalizer
	ServiceName         string   // Egress service this pod belongs to
	Addresses           []string // PodIPs; a dual-stack pod contributes one address per IP family
	VerifyServiceDrain  bool     // No PodIPs were available; wait until no unbacked NRP address remains for the service
	IsLastPod           bool     // True if this was the last pod for the service (finalizer removed after NAT GW deletion)
	RecoveredAtStartup  bool     // Finalizer was found stuck at startup; removing it closes the recovery gap
	Timestamp           string
	ResolutionPending   bool
	LocationSyncPending bool // Applies to ServiceName unless ServiceCleanup is present.
	ResolutionAddresses []string
	ResolutionServices  []string
	// No identity/address was recoverable; verify unbacked outbound mappings without guessing ownership.
	VerifyUnknownEgressDrain   bool
	UnknownLocationSyncPending bool
	// When present, ServiceCleanup replaces the single-service fields above.
	ServiceCleanup map[string]podCleanupState
	// Reused namespace/name keys retain independent records by raw Pod UID.
	// These entries are leaves: they never contain their own OtherUIDs.
	OtherUIDs map[string]*PendingPodDeletion
}

type podCleanupState struct {
	Addresses              []string
	VerifyServiceDrain     bool
	WaitForServiceDeletion bool
	LocationSyncPending    bool
}

type podFinalizerGuard struct {
	token chan struct{}
	users int
}

// An empty UID retains the legacy namespace/name-only lookup.
func (pending *PendingPodDeletion) forUID(uid string) *PendingPodDeletion {
	if pending == nil || uid == "" || pending.UID == uid {
		return pending
	}
	return pending.OtherUIDs[uid]
}

func (pending *PendingPodDeletion) withUID(record *PendingPodDeletion) *PendingPodDeletion {
	if pending == nil {
		return record
	}
	updated := *record
	updated.OtherUIDs = nil
	if pending.UID == record.UID {
		updated.OtherUIDs = pending.OtherUIDs
		return &updated
	}
	group := *pending
	group.OtherUIDs = maps.Clone(pending.OtherUIDs)
	if group.OtherUIDs == nil {
		group.OtherUIDs = make(map[string]*PendingPodDeletion)
	}
	group.OtherUIDs[record.UID] = &updated
	return &group
}

func (pending *PendingPodDeletion) withoutUID(uid string) *PendingPodDeletion {
	others := maps.Clone(pending.OtherUIDs)
	if pending.UID == uid {
		for nextUID, next := range others {
			updated := *next
			delete(others, nextUID)
			updated.OtherUIDs = others
			return &updated
		}
		return nil
	}
	updated := *pending
	delete(others, uid)
	updated.OtherUIDs = others
	return &updated
}

// Never acquire a Pod guard while holding dt.mu. API calls hold only the Pod guard.
func (dt *DiffTracker) lockPodFinalizer(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return func() {}, nil
	}
	dt.mu.Lock()
	if dt.podFinalizerGuards == nil {
		dt.podFinalizerGuards = make(map[string]*podFinalizerGuard)
	}
	guard := dt.podFinalizerGuards[key]
	if guard == nil {
		guard = &podFinalizerGuard{token: make(chan struct{}, 1)}
		guard.token <- struct{}{}
		dt.podFinalizerGuards[key] = guard
	}
	guard.users++
	dt.mu.Unlock()

	dropReference := func() {
		dt.mu.Lock()
		guard.users--
		if guard.users == 0 {
			delete(dt.podFinalizerGuards, key)
		}
		dt.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		dropReference()
		return nil, ctx.Err()
	case <-guard.token:
		return func() {
			guard.token <- struct{}{}
			dropReference()
		}, nil
	}
}

func (pending *PendingPodDeletion) cleanupFor(serviceName string) (podCleanupState, bool) {
	if pending.ServiceCleanup != nil {
		cleanup, ok := pending.ServiceCleanup[serviceName]
		return cleanup, ok
	}
	if pending.ServiceName != serviceName {
		return podCleanupState{}, false
	}
	return podCleanupState{
		Addresses:              pending.Addresses,
		VerifyServiceDrain:     pending.VerifyServiceDrain,
		WaitForServiceDeletion: pending.IsLastPod,
		LocationSyncPending:    pending.LocationSyncPending,
	}, true
}

func (pending *PendingPodDeletion) waitsForService(serviceName string) bool {
	cleanup, ok := pending.cleanupFor(serviceName)
	if ok && cleanup.WaitForServiceDeletion {
		return true
	}
	for _, other := range pending.OtherUIDs {
		if cleanup, ok := other.cleanupFor(serviceName); ok && cleanup.WaitForServiceDeletion {
			return true
		}
	}
	return false
}

func (pending *PendingPodDeletion) hasPendingLocationSync() bool {
	if pending.UnknownLocationSyncPending || pending.ServiceCleanup == nil && pending.LocationSyncPending {
		return true
	}
	for _, cleanup := range pending.ServiceCleanup {
		if cleanup.LocationSyncPending {
			return true
		}
	}
	for _, other := range pending.OtherUIDs {
		if other.hasPendingLocationSync() {
			return true
		}
	}
	return false
}

func (pending *PendingPodDeletion) withCleanup(serviceName string, cleanup podCleanupState) *PendingPodDeletion {
	updated := *pending
	if pending.ServiceCleanup == nil && (pending.ServiceName == "" || pending.ServiceName == serviceName) {
		updated.ServiceName = serviceName
		updated.Addresses = cleanup.Addresses
		updated.VerifyServiceDrain = cleanup.VerifyServiceDrain
		updated.IsLastPod = cleanup.WaitForServiceDeletion
		updated.LocationSyncPending = cleanup.LocationSyncPending
		return &updated
	}
	updated.ServiceCleanup = maps.Clone(pending.ServiceCleanup)
	if updated.ServiceCleanup == nil {
		updated.ServiceCleanup = make(map[string]podCleanupState)
		updated.ServiceCleanup[pending.ServiceName], _ = pending.cleanupFor(pending.ServiceName)
	}
	updated.ServiceCleanup[serviceName] = cleanup
	updated.ServiceName, updated.Addresses = "", nil
	updated.VerifyServiceDrain, updated.IsLastPod = false, false
	updated.LocationSyncPending = false
	return &updated
}

func (dt *DiffTracker) mergePodDeletionLocked(key string, incoming *PendingPodDeletion) {
	group := dt.pendingPodDeletions[key]
	prior := group.forUID(incoming.UID)
	if prior == nil {
		dt.pendingPodDeletions[key] = group.withUID(incoming)
		return
	}
	updated := *prior
	updated.OtherUIDs = nil
	updated.RecoveredAtStartup = prior.RecoveredAtStartup || incoming.RecoveredAtStartup
	updated.ResolutionPending = prior.ResolutionPending || incoming.ResolutionPending
	updated.UnknownLocationSyncPending = prior.UnknownLocationSyncPending || incoming.UnknownLocationSyncPending
	updated.VerifyUnknownEgressDrain = prior.VerifyUnknownEgressDrain || incoming.VerifyUnknownEgressDrain
	updated.ResolutionAddresses = slices.Clone(prior.ResolutionAddresses)
	for _, address := range incoming.ResolutionAddresses {
		updated.ResolutionAddresses = appendAddressIfAbsent(updated.ResolutionAddresses, address)
	}
	updated.ResolutionServices = slices.Clone(prior.ResolutionServices)
	for _, serviceName := range incoming.ResolutionServices {
		if !slices.Contains(updated.ResolutionServices, serviceName) {
			updated.ResolutionServices = append(updated.ResolutionServices, serviceName)
		}
	}
	updated.Timestamp = incoming.Timestamp
	merged := &updated
	mergeCleanup := func(serviceName string, cleanup podCleanupState) {
		old, _ := merged.cleanupFor(serviceName)
		addresses := slices.Clone(old.Addresses)
		for _, address := range cleanup.Addresses {
			addresses = appendAddressIfAbsent(addresses, address)
		}
		cleanup.Addresses = addresses
		cleanup.VerifyServiceDrain = cleanup.VerifyServiceDrain || old.VerifyServiceDrain
		cleanup.WaitForServiceDeletion = cleanup.WaitForServiceDeletion || old.WaitForServiceDeletion
		cleanup.LocationSyncPending = cleanup.LocationSyncPending || old.LocationSyncPending
		merged = merged.withCleanup(serviceName, cleanup)
	}
	if incoming.ServiceCleanup != nil {
		for serviceName, cleanup := range incoming.ServiceCleanup {
			mergeCleanup(serviceName, cleanup)
		}
	} else if incoming.ServiceName != "" || prior.ServiceCleanup == nil && prior.ServiceName == "" {
		cleanup, _ := incoming.cleanupFor(incoming.ServiceName)
		mergeCleanup(incoming.ServiceName, cleanup)
	}
	dt.pendingPodDeletions[key] = group.withUID(merged)
}

func (dt *DiffTracker) cancelPodCleanupLocked(key, uid, serviceName string) bool {
	group := dt.pendingPodDeletions[key]
	pending := group.forUID(uid)
	if pending == nil && group != nil && group.UID == "" {
		pending = group
	}
	if pending == nil {
		return false
	}
	if pending.ServiceCleanup == nil && pending.ServiceName != "" && pending.ServiceName != serviceName {
		return false
	}
	_, changed := pending.cleanupFor(serviceName)
	if pending.ServiceCleanup == nil && pending.ServiceName == "" {
		changed = len(pending.Addresses) != 0 || pending.VerifyServiceDrain || pending.IsLastPod || pending.LocationSyncPending
	}
	if !changed && (pending.ResolutionPending || pending.UnknownLocationSyncPending || pending.VerifyUnknownEgressDrain || len(pending.ServiceCleanup) != 0) {
		return false
	}
	updated := *pending
	updated.OtherUIDs = nil
	if pending.ServiceCleanup != nil {
		updated.ServiceCleanup = maps.Clone(pending.ServiceCleanup)
		delete(updated.ServiceCleanup, serviceName)
	} else {
		updated.ServiceName, updated.Addresses = "", nil
		updated.VerifyServiceDrain, updated.IsLastPod = false, false
		updated.LocationSyncPending = false
	}
	if !updated.ResolutionPending && !updated.UnknownLocationSyncPending && !updated.VerifyUnknownEgressDrain && len(updated.ServiceCleanup) == 0 {
		if remaining := group.withoutUID(pending.UID); remaining != nil {
			dt.pendingPodDeletions[key] = remaining
		} else {
			delete(dt.pendingPodDeletions, key)
		}
		if pending.RecoveredAtStartup {
			recordFinalizerRecovered()
		}
		return true
	}
	dt.pendingPodDeletions[key] = group.withUID(&updated)
	return true
}

// enqueuePodFinalizerRetry records a pod whose cleanup finalizer could not be removed so
// CheckPendingPodDeletions retries it, and nudges the LocationsUpdater to run that check.
//
// The removal paths that call this have already established there is nothing to drain, so the record
// carries no addresses and is immediately ready. Without it a transient apiserver error is simply
// lost: no pending record exists, and the informer only re-drives a pod whose state changes, so the
// pod stays Terminating until the CCM restarts and blocks node drain and namespace deletion.
func (dt *DiffTracker) enqueuePodFinalizerRetry(pod *v1.Pod, serviceName string) {
	if pod == nil {
		return
	}
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	dt.mu.Lock()
	if group := dt.pendingPodDeletions[podKey]; group.forUID(string(pod.UID)) == nil {
		dt.pendingPodDeletions[podKey] = group.withUID(&PendingPodDeletion{
			Namespace:   pod.Namespace,
			Name:        pod.Name,
			UID:         string(pod.UID),
			ServiceName: serviceName,
			Timestamp:   time.Now().Format(time.RFC3339),
		})
	}
	dt.mu.Unlock()

	dt.triggerLocationsUpdater()
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

// ErrServiceGoneOrReplaced means the Service targeted by an async operation no longer exists under
// its original UID. Callers must stop that operation rather than create resources without finalizers.
var ErrServiceGoneOrReplaced = errors.New("service gone or replaced by a same-name UID")

// hasServiceGatewayFinalizer checks if service has the ServiceGateway cleanup finalizer
func hasServiceGatewayFinalizer(service *v1.Service) bool {
	return hasFinalizer(service.Finalizers, ServiceGatewayServiceCleanupFinalizer)
}

// addServiceGatewayFinalizer adds the ServiceGateway cleanup finalizer to the service
// This prevents Kubernetes from deleting the service until Azure resources are cleaned up
// IMPORTANT: We also add the K8s LoadBalancerCleanupFinalizer so that the upstream
// service controller's needsCleanup() returns true when the service is being deleted.
// This ensures EnsureLoadBalancerDeleted is called, which triggers our async deletion flow.
// Implements retry with exponential backoff for resilience against transient API failures.
func (dt *DiffTracker) addServiceGatewayFinalizer(ctx context.Context, service *v1.Service) error {
	namespace := service.Namespace
	name := service.Name
	intendedUID := string(service.UID)
	var lastErr error
	goneOrReplaced := false

	retryErr := wait.ExponentialBackoff(finalizerRetryBackoff, func() (bool, error) {
		// Get fresh service to avoid conflicts
		currentSvc, err := dt.kubeClient.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				goneOrReplaced = true
				return true, nil
			}
			lastErr = err
			dt.logger.V(4).Info("Transient error getting service, will retry", "namespace", namespace, "name", name, "err", err)
			return false, nil // Retry
		}

		if intendedUID != "" && string(currentSvc.UID) != intendedUID {
			dt.logger.V(4).Info("Service UID changed (replacement Service); not adding finalizers", "namespace", namespace, "name", name, "wantUID", intendedUID, "gotUID", string(currentSvc.UID))
			goneOrReplaced = true
			return true, nil
		}

		// Decide from the fresh object, not the informer snapshot passed by the caller. During a
		// rapid delete/recreate the cache can still show finalizers that the live Service lost.
		if hasServiceGatewayFinalizer(currentSvc) && servicehelper.HasLBFinalizer(currentSvc) {
			return true, nil
		}

		// Make a copy so we don't mutate the shared informer cache
		updated := currentSvc.DeepCopy()
		if !hasServiceGatewayFinalizer(currentSvc) {
			updated.Finalizers = append(updated.Finalizers, ServiceGatewayServiceCleanupFinalizer)
		}

		// Also add the K8s LoadBalancerCleanupFinalizer if not present.
		// This is critical: the upstream K8s service controller uses HasLBFinalizer()
		// in needsCleanup() to determine if a service being deleted needs cleanup.
		// Without this finalizer, the controller tries to add it (which fails since
		// the service is being deleted), and never calls EnsureLoadBalancerDeleted.
		if !servicehelper.HasLBFinalizer(currentSvc) {
			updated.Finalizers = append(updated.Finalizers, servicehelper.LoadBalancerCleanupFinalizer)
		}

		dt.logger.V(5).Info("Adding ServiceGateway finalizer to service", "namespace", namespace, "name", name)
		// Update carries the fresh object's resourceVersion, so a concurrent same-name
		// replacement or metadata change is rejected atomically instead of patching it.
		_, err = dt.kubeClient.CoreV1().Services(namespace).Update(ctx, updated, metav1.UpdateOptions{})
		if err != nil {
			lastErr = err
			dt.logger.V(4).Info("Transient error updating service, will retry", "namespace", namespace, "name", name, "err", err)
			return false, nil // Retry
		}

		return true, nil // Success
	})

	if retryErr != nil {
		return fmt.Errorf("failed to add finalizer after retries: %w (last error: %w)", retryErr, lastErr)
	}
	if goneOrReplaced {
		return ErrServiceGoneOrReplaced
	}
	return nil
}

// removeServiceGatewayFinalizer removes the ServiceGateway cleanup finalizer from the service
// This allows Kubernetes to complete the service deletion after Azure resources are cleaned up
// NOTE: We also remove the K8s LoadBalancerCleanupFinalizer that we added in addServiceGatewayFinalizer
// Implements retry with exponential backoff for resilience against transient API failures.
func (dt *DiffTracker) removeServiceGatewayFinalizer(ctx context.Context, service *v1.Service) error {
	namespace := service.Namespace
	name := service.Name
	intendedUID := string(service.UID)
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

		if intendedUID != "" && string(currentSvc.UID) != intendedUID {
			dt.logger.V(4).Info("Service UID changed (replacement Service); not removing finalizers", "namespace", namespace, "name", name, "wantUID", intendedUID, "gotUID", string(currentSvc.UID))
			return true, nil
		}

		// Decide from the fresh object. A stale lister object can omit finalizers that still exist on
		// the live Service, and treating that as success would strand the Service in Terminating.
		if !hasServiceGatewayFinalizer(currentSvc) && !servicehelper.HasLBFinalizer(currentSvc) {
			return true, nil
		}

		// Make a copy so we don't mutate the shared informer cache
		updated := currentSvc.DeepCopy()
		updated.Finalizers = removeFinalizerString(updated.Finalizers, ServiceGatewayServiceCleanupFinalizer)
		// Also remove the K8s LoadBalancerCleanupFinalizer that we added
		updated.Finalizers = removeFinalizerString(updated.Finalizers, servicehelper.LoadBalancerCleanupFinalizer)

		dt.logger.V(5).Info("Removing ServiceGateway finalizer from service", "namespace", namespace, "name", name)
		// Update carries the fresh object's resourceVersion, so a concurrent same-name
		// replacement or metadata change is rejected atomically instead of patching it.
		_, err = dt.kubeClient.CoreV1().Services(namespace).Update(ctx, updated, metav1.UpdateOptions{})
		if err != nil {
			lastErr = err
			dt.logger.V(4).Info("Transient error updating service, will retry", "namespace", namespace, "name", name, "err", err)
			return false, nil // Retry
		}

		return true, nil // Success
	})

	if retryErr != nil {
		return fmt.Errorf("failed to remove finalizer after retries: %w (last error: %w)", retryErr, lastErr)
	}
	return nil
}

// ================================================================================================
// POD FINALIZER OPERATIONS
// ================================================================================================

// HasPodFinalizer checks if pod has the ServiceGateway pod cleanup finalizer.
// This is exported for use by provider layer to check pod state during recovery.
func HasPodFinalizer(pod *v1.Pod) bool {
	return hasFinalizer(pod.Finalizers, ServiceGatewayPodCleanupFinalizer)
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

// ErrPodGoneOrReplaced is returned by AddPodFinalizer when the event no longer describes the live
// pod: it no longer exists, was replaced by a same-name pod with a different UID, or its egress label
// changed or was emptied, it is deleting, or it is no longer Pending or Running. The finalizer is
// intentionally not added, so the caller must skip registering the stale event pod rather than treat
// this as a successful add; a later event for the live pod reconciles it.
var ErrPodGoneOrReplaced = errors.New("pod gone, replaced or no longer an egress pod; skip egress registration")

// AddPodFinalizer adds the ServiceGateway pod cleanup finalizer, gating pod deletion until the
// address is synced out of NRP. Each retry checks fresh UID and egress intent rather than trusting
// the informer snapshot; a gone, replaced or no-longer-eligible Pod returns ErrPodGoneOrReplaced.
func (dt *DiffTracker) AddPodFinalizer(ctx context.Context, pod *v1.Pod) error {
	unlock, err := dt.lockPodFinalizer(ctx, pod.Namespace+"/"+pod.Name)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = dt.addPodFinalizerGuarded(ctx, pod)
	return err
}

func (dt *DiffTracker) addPodFinalizerGuarded(ctx context.Context, pod *v1.Pod) (*v1.Pod, error) {
	namespace := pod.Namespace
	name := pod.Name
	intendedUID := string(pod.UID)
	intendedEgress := podEgressGatewayName(pod)
	var lastErr error
	var latest *v1.Pod
	goneOrReplaced := false

	retryErr := wait.ExponentialBackoffWithContext(ctx, finalizerRetryBackoff, func(ctx context.Context) (bool, error) {
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

		currentEgress := podEgressGatewayName(currentPod)
		if intendedEgress == "" || currentEgress != intendedEgress ||
			currentPod.DeletionTimestamp != nil ||
			(currentPod.Status.Phase != v1.PodRunning && currentPod.Status.Phase != v1.PodPending) {
			dt.logger.V(4).Info("Pod no longer matches the egress add intent; not adding finalizer",
				"namespace", namespace,
				"name", name,
				"wantEgress", intendedEgress,
				"gotEgress", currentEgress,
				"deleting", currentPod.DeletionTimestamp != nil,
				"phase", currentPod.Status.Phase)
			goneOrReplaced = true
			return true, nil
		}

		latest = currentPod
		// Check if already has finalizer (may have been added by a concurrent operation)
		if hasPodFinalizer(currentPod) {
			return true, nil
		}

		// Make a copy so we don't mutate the shared informer cache
		updated := currentPod.DeepCopy()
		updated.Finalizers = append(updated.Finalizers, ServiceGatewayPodCleanupFinalizer)

		dt.logger.V(5).Info("Adding ServiceGateway pod finalizer to pod", "namespace", namespace, "name", name)
		protected, err := dt.kubeClient.CoreV1().Pods(namespace).Update(ctx, updated, metav1.UpdateOptions{})
		if err != nil {
			lastErr = err
			dt.logger.V(4).Info("Transient error updating pod, will retry", "namespace", namespace, "name", name, "err", err)
			return false, nil // Retry
		}
		latest = protected

		return true, nil // Success
	})

	if retryErr != nil {
		return latest, fmt.Errorf("failed to add pod finalizer after retries: %w (last error: %w)", retryErr, lastErr)
	}
	if goneOrReplaced {
		return nil, ErrPodGoneOrReplaced
	}
	return latest, nil
}

// markServiceFinalizerRecovering records that startup found a stuck finalizer on this Service and
// left its removal to the diff, so the removal can later be counted as recovered.
func (dt *DiffTracker) markServiceFinalizerRecovering(serviceName string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if dt.recoveredServiceFinalizers == nil {
		dt.recoveredServiceFinalizers = make(map[string]struct{})
	}
	dt.recoveredServiceFinalizers[serviceName] = struct{}{}
}

// recordServiceFinalizerRecoveryDone counts a finalizer that startup scheduled for recovery once it
// is actually off the Service. Routine deletions are not counted: only a UID that startup marked
// matches, and it matches once.
func (dt *DiffTracker) recordServiceFinalizerRecoveryDone(serviceName string) {
	dt.mu.Lock()
	_, scheduled := dt.recoveredServiceFinalizers[serviceName]
	delete(dt.recoveredServiceFinalizers, serviceName)
	dt.mu.Unlock()

	if scheduled {
		recordFinalizerRecovered()
	}
}

// removePodFinalizer removes the ServiceGateway pod cleanup finalizer from the pod
// This allows Kubernetes to complete the pod deletion after location is synced to NRP
// Uses retry logic to handle concurrent modifications during bulk pod deletions
func (dt *DiffTracker) removePodFinalizer(ctx context.Context, pod *v1.Pod) error {
	return dt.removePodFinalizerIf(ctx, pod, nil)
}

func (dt *DiffTracker) removePodFinalizerIf(ctx context.Context, pod *v1.Pod, ready func(*v1.Pod) bool) error {
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
		if ready != nil && !ready(currentPod) {
			return nil
		}

		// Make a copy so we don't mutate the cache
		updated := currentPod.DeepCopy()
		updated.Finalizers = removeFinalizerString(updated.Finalizers, ServiceGatewayPodCleanupFinalizer)

		dt.logger.V(5).Info("Removing ServiceGateway pod finalizer from pod", "namespace", namespace, "name", name)
		_, err = dt.kubeClient.CoreV1().Pods(namespace).Update(ctx, updated, metav1.UpdateOptions{})
		return err
	})
}

// RemovePodFinalizerByPod rechecks current intent and cleanup before releasing this Pod's finalizer.
func (dt *DiffTracker) RemovePodFinalizerByPod(ctx context.Context, pod *v1.Pod) error {
	unlock, err := dt.lockPodFinalizer(ctx, pod.Namespace+"/"+pod.Name)
	if err != nil {
		return err
	}
	key := pod.Namespace + "/" + pod.Name
	dt.mu.Lock()
	if prior := dt.pendingPodDeletions[key]; prior != nil && prior.UID != "" && prior.forUID(string(pod.UID)) == nil {
		dt.mu.Unlock()
		defer unlock()
		err = dt.removePodFinalizerIf(ctx, pod, func(current *v1.Pod) bool {
			dt.mu.Lock()
			defer dt.mu.Unlock()
			if dt.podWantsEgressLocked(current) {
				return false
			}
			if len(dt.unlabelledPodReleasePlansLocked(current, nil, "")) != 0 {
				dt.queuePodResolutionLocked(current, false)
				dt.triggerLocationsUpdater()
				return false
			}
			return true
		})
		if err != nil {
			dt.mu.Lock()
			dt.queuePodResolutionLocked(pod, false)
			dt.mu.Unlock()
			dt.triggerLocationsUpdater()
		}
		return err
	}
	if dt.pendingPodDeletions[key] == nil {
		dt.pendingPodDeletions[key] = &PendingPodDeletion{
			Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID),
		}
	}
	entry := pendingPodToProcess{Key: key, Record: dt.pendingPodDeletions[key]}
	dt.mu.Unlock()
	_, _, err = dt.processPodFinalizerGuarded(ctx, &entry, "")
	unlock()
	if err != nil {
		dt.triggerLocationsUpdater()
	}
	return err
}

type unlabelledPodReleasePlan struct {
	serviceName string
	location    string
	addresses   []string
}

func podEgressGatewayName(pod *v1.Pod) string {
	if pod == nil || pod.Labels == nil {
		return ""
	}
	return outboundServiceName(pod.Labels[consts.PodLabelServiceEgressGateway])
}

func addReleaseAddress(plans map[string]map[string][]string, serviceName, location, address string) {
	if serviceName == "" || address == "" {
		return
	}
	if location == "" {
		location = address
	}
	if plans[serviceName] == nil {
		plans[serviceName] = make(map[string][]string)
	}
	if !slices.Contains(plans[serviceName][location], address) {
		plans[serviceName][location] = append(plans[serviceName][location], address)
	}
}

func podIdentityMatches(statePod Pod, podKey, podUID, address string, podAddresses map[string]struct{}) bool {
	if podUID != "" && statePod.OutboundPodUID != "" {
		return statePod.OutboundPodUID == podUID
	}
	if podKey != "" && statePod.OutboundPodKey != "" {
		return statePod.OutboundPodKey == podKey
	}
	if _, ok := podAddresses[address]; ok {
		return statePod.OutboundPodKey == "" && statePod.OutboundPodUID == ""
	}
	return false
}

func pendingPodIdentityMatches(pending PendingPodUpdate, podKey, podUID string) bool {
	if podUID != "" && pending.PodUID != "" {
		return pending.PodUID == podUID
	}
	return podKey != "" && pending.PodKey == podKey
}

func (dt *DiffTracker) isOutboundIdentityLocked(serviceName string) bool {
	_, kind, valid := serviceIdentity(serviceName)
	if !valid || kind != Outbound || dt.isReservedServiceLocked(serviceName) {
		return false
	}
	if (dt.NRPResources.NATGateways != nil && dt.NRPResources.NATGateways.Has(serviceName)) ||
		(dt.K8sResources.Egresses != nil && dt.K8sResources.Egresses.Has(serviceName)) {
		return true
	}
	if opState, ok := dt.pendingServiceOps[serviceName]; ok {
		return !opState.Config.IsInbound
	}
	_, hasBufferedPods := dt.pendingPods[serviceName]
	return hasBufferedPods
}

func (dt *DiffTracker) outboundAddressOwnedByDifferentUIDLocked(serviceName, location, address, uid string) bool {
	if uid == "" {
		return false
	}
	pod := dt.K8sResources.Nodes[location].Pods[address]
	if pod.OutboundPodUID != "" && pod.OutboundPodUID != uid &&
		pod.PublicOutboundIdentity != "" && strings.EqualFold(pod.PublicOutboundIdentity, serviceName) {
		return true
	}
	for _, pending := range dt.pendingPods[serviceName] {
		if pending.Location == location && pending.Address == address &&
			pending.PodUID != "" && pending.PodUID != uid {
			return true
		}
	}
	return false
}

func (dt *DiffTracker) unlabelledPodReleasePlansLocked(pod *v1.Pod, prior *PendingPodDeletion, completedService string) []unlabelledPodReleasePlan {
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	podUID := string(pod.UID)
	podAddresses := make(map[string]struct{})
	for _, address := range PodEgressAddresses(pod) {
		if address != "" {
			podAddresses[address] = struct{}{}
		}
	}
	if prior != nil {
		for _, address := range prior.Addresses {
			podAddresses[address] = struct{}{}
		}
		for _, address := range prior.ResolutionAddresses {
			podAddresses[address] = struct{}{}
		}
		for _, cleanup := range prior.ServiceCleanup {
			for _, address := range cleanup.Addresses {
				podAddresses[address] = struct{}{}
			}
		}
	}

	plansByService := make(map[string]map[string][]string)

	for location, node := range dt.K8sResources.Nodes {
		for address, statePod := range node.Pods {
			serviceName := statePod.PublicOutboundIdentity
			if serviceName == completedService || !dt.isOutboundIdentityLocked(serviceName) || !podIdentityMatches(statePod, podKey, podUID, address, podAddresses) {
				continue
			}
			addReleaseAddress(plansByService, serviceName, location, address)
			podAddresses[address] = struct{}{}
		}
	}

	for serviceName, pendingPods := range dt.pendingPods {
		if serviceName == completedService || !dt.isOutboundIdentityLocked(serviceName) {
			continue
		}
		for _, pending := range pendingPods {
			if pendingPodIdentityMatches(pending, podKey, podUID) {
				addReleaseAddress(plansByService, serviceName, pending.Location, pending.Address)
				podAddresses[pending.Address] = struct{}{}
			}
		}
	}

	for location, nrpLocation := range dt.NRPResources.Locations {
		for address := range podAddresses {
			nrpAddress := nrpLocation.Addresses[address]
			if nrpAddress.Services == nil {
				continue
			}
			for _, serviceName := range nrpAddress.Services.UnsortedList() {
				if serviceName != completedService && dt.isOutboundIdentityLocked(serviceName) &&
					dt.NRPResources.NATGateways != nil && dt.NRPResources.NATGateways.Has(serviceName) &&
					!dt.outboundAddressOwnedByDifferentUIDLocked(serviceName, location, address, podUID) {
					addReleaseAddress(plansByService, serviceName, location, address)
				}
			}
		}
	}

	plans := make([]unlabelledPodReleasePlan, 0)
	for serviceName, byLocation := range plansByService {
		for location, addresses := range byLocation {
			plans = append(plans, unlabelledPodReleasePlan{
				serviceName: serviceName,
				location:    location,
				addresses:   addresses,
			})
		}
	}
	return plans
}

// releaseUnlabelledPodFinalizer drains any egress associations still tied to a pod whose egress
// label is absent or empty, then releases only this controller's finalizer.
func (dt *DiffTracker) releaseUnlabelledPodFinalizer(ctx context.Context, pod *v1.Pod, recoveredAtStartup bool) (pending, released bool) {
	if pod == nil || !hasPodFinalizer(pod) || podEgressGatewayName(pod) != "" {
		return false, false
	}
	key := pod.Namespace + "/" + pod.Name
	unlock, err := dt.lockPodFinalizer(ctx, key)
	if err != nil {
		return false, false
	}
	dt.mu.Lock()
	prior := dt.pendingPodDeletions[key].forUID(string(pod.UID))
	wasRecovering := prior != nil && prior.RecoveredAtStartup
	dt.queuePodResolutionLocked(pod, recoveredAtStartup)
	entry := pendingPodToProcess{Key: key, Record: dt.pendingPodDeletions[key]}
	dt.mu.Unlock()
	retired, readd, err := dt.processPodFinalizerGuarded(ctx, &entry, "")
	unlock()
	released = retired && readd == nil
	dt.mu.Lock()
	pending = dt.pendingPodDeletions[key] != nil
	if pending && recoveredAtStartup && !wasRecovering {
		recordFinalizerRecoveryScheduled()
	}
	dt.mu.Unlock()
	if err != nil {
		RecordPodFinalizerRemoveFailed()
		dt.logger.V(4).Info("Could not resolve or release unlabelled pod finalizer; queued for retry",
			"namespace", pod.Namespace, "pod", pod.Name, "err", err)
	}
	if pending {
		dt.triggerLocationsUpdater()
	}
	return pending, released
}

func (dt *DiffTracker) queuePodResolutionLocked(pod *v1.Pod, recoveredAtStartup bool) {
	key := pod.Namespace + "/" + pod.Name
	group := dt.pendingPodDeletions[key]
	prior := group.forUID(string(pod.UID))
	pending := &PendingPodDeletion{
		Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID),
	}
	if prior != nil {
		updated := *prior
		updated.OtherUIDs = nil
		pending = &updated
	}
	addresses := PodEgressAddresses(pod)
	for _, address := range pending.ResolutionAddresses {
		addresses = appendAddressIfAbsent(addresses, address)
	}
	pending.ResolutionAddresses = addresses
	pending.ResolutionPending = true
	pending.RecoveredAtStartup = pending.RecoveredAtStartup || recoveredAtStartup
	pending.Timestamp = time.Now().Format(time.RFC3339)
	remember := func(identity string, addresses []string) {
		if !dt.isOutboundIdentityLocked(identity) {
			return
		}
		if !slices.Contains(pending.ResolutionServices, identity) {
			pending.ResolutionServices = append(slices.Clone(pending.ResolutionServices), identity)
		}
		for _, address := range addresses {
			pending.ResolutionAddresses = appendAddressIfAbsent(pending.ResolutionAddresses, address)
		}
		cleanup, _ := pending.cleanupFor(identity)
		cleanup.Addresses = slices.Clone(cleanup.Addresses)
		for _, address := range addresses {
			cleanup.Addresses = appendAddressIfAbsent(cleanup.Addresses, address)
		}
		cleanup.VerifyServiceDrain = cleanup.VerifyServiceDrain || len(cleanup.Addresses) == 0
		pending = pending.withCleanup(identity, cleanup)
	}
	// Retain this UID's associations across failed reads without changing desired state.
	for _, plan := range dt.unlabelledPodReleasePlansLocked(pod, pending, "") {
		remember(plan.serviceName, plan.addresses)
	}
	remember(podEgressGatewayName(pod), PodEgressAddresses(pod))
	if podEgressGatewayName(pod) == "" && len(pending.ResolutionAddresses) == 0 &&
		pending.ServiceName == "" && len(pending.ServiceCleanup) == 0 {
		pending.VerifyUnknownEgressDrain = true
		pending.UnknownLocationSyncPending = true
	}
	dt.pendingPodDeletions[key] = group.withUID(pending)
}

func (dt *DiffTracker) enqueuePodResolution(ctx context.Context, pod *v1.Pod) {
	key := pod.Namespace + "/" + pod.Name
	unlock, err := dt.lockPodFinalizer(ctx, key)
	if err != nil {
		return
	}
	defer unlock()
	dt.mu.Lock()
	dt.queuePodResolutionLocked(pod, false)
	dt.mu.Unlock()
	dt.triggerLocationsUpdater()
}

// ================================================================================================
// PENDING DELETION TRACKING - POD FINALIZERS
// ================================================================================================

// pendingPodToProcess is used internally to collect pending pods for processing
// without holding the lock during API calls
type pendingPodToProcess struct {
	Key    string
	Record *PendingPodDeletion
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
	return dt.pendingPodDeletions[fmt.Sprintf("%s/%s", namespace, name)].forUID(uid) != nil
}

// reservedPodFinalizerDecisionLocked leaves reserved resources untouched, but an earlier
// pod drain can still own its finalizer.
func (dt *DiffTracker) reservedPodFinalizerDecisionLocked(namespace, name, uid string) DeletePodResult {
	result := DeletePodResult{}
	if namespace == "" || name == "" {
		return result
	}
	if pending := dt.pendingPodDeletions[namespace+"/"+name].forUID(uid); pending != nil {
		result.IsLastPod, result.Enqueued = pending.IsLastPod, true
	}
	result.setFinalizerDecision()
	return result
}

func (dt *DiffTracker) podCleanupReadyLocked(pending *PendingPodDeletion) bool {
	if pending.UnknownLocationSyncPending ||
		pending.ServiceCleanup == nil && (pending.LocationSyncPending || pending.IsLastPod) {
		return false
	}
	for _, cleanup := range pending.ServiceCleanup {
		if cleanup.WaitForServiceDeletion || cleanup.LocationSyncPending {
			return false
		}
	}
	return dt.podLocationsDrainedLocked(pending)
}

func (dt *DiffTracker) podLocationsDrainedLocked(pending *PendingPodDeletion) bool {
	if pending.ResolutionPending {
		return false
	}
	if pending.VerifyUnknownEgressDrain && dt.hasUnbackedOutboundMappingLocked() {
		return false
	}
	ready := func(serviceName string, cleanup podCleanupState) bool {
		if cleanup.VerifyServiceDrain {
			if _, waiting := dt.serviceHasUnbackedNRPAddressLocked(serviceName); waiting {
				return false
			}
		}
		return true
	}
	if pending.ServiceCleanup == nil {
		cleanup, _ := pending.cleanupFor(pending.ServiceName)
		if !ready(pending.ServiceName, cleanup) {
			return false
		}
	} else {
		for serviceName, cleanup := range pending.ServiceCleanup {
			if !ready(serviceName, cleanup) {
				return false
			}
		}
	}
	_, waiting := dt.podAddressStillInNRPLocked(pending)
	return !waiting
}

func (dt *DiffTracker) podWantsEgressLocked(pod *v1.Pod) bool {
	identity := podEgressGatewayName(pod)
	return identity != "" && pod.DeletionTimestamp == nil &&
		(pod.Status.Phase == v1.PodRunning || pod.Status.Phase == v1.PodPending) &&
		egressIdentityRegex.MatchString(strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])) &&
		!dt.isReservedServiceLocked(identity)
}

func (dt *DiffTracker) queuePodServiceCleanupLocked(serviceName string) bool {
	if !dt.isOutboundIdentityLocked(serviceName) || dt.outboundServiceHasLiveOrBufferedPodLocked(serviceName) {
		return false
	}
	op := dt.pendingServiceOps[serviceName]
	if !dt.NRPResources.NATGateways.Has(serviceName) && !dt.serviceHasLocationsInNRP(serviceName) &&
		(op == nil || op.State == StateNotStarted) {
		return false
	}
	dt.markOutboundServiceForDeletionLocked(serviceName)
	dt.triggerLocationsUpdater()
	return true
}

func (dt *DiffTracker) drainDepartedPodLocked(entry *pendingPodToProcess, completedService string) {
	prior := *entry.Record
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: prior.Namespace, Name: prior.Name, UID: types.UID(prior.UID),
	}}
	for _, plan := range dt.podRegistrationDrainsLocked(pod, "", nil) {
		dt.deletePodLocked(plan.serviceName, plan.location, plan.addresses, prior.Namespace, prior.Name, prior.UID, false, true)
	}
	if prior.ResolutionPending {
		for _, plan := range dt.unlabelledPodReleasePlansLocked(pod, &prior, completedService) {
			if prior.waitsForService(plan.serviceName) {
				continue
			}
			dt.deletePodLocked(plan.serviceName, plan.location, plan.addresses, prior.Namespace, prior.Name, prior.UID, false, true)
		}
	}
	queueCleanup := func(serviceName string) {
		if serviceName != completedService && !prior.waitsForService(serviceName) {
			dt.queuePodServiceCleanupLocked(serviceName)
		}
	}
	for _, serviceName := range prior.ResolutionServices {
		queueCleanup(serviceName)
	}
	if prior.ServiceCleanup == nil {
		queueCleanup(prior.ServiceName)
	} else {
		for serviceName := range prior.ServiceCleanup {
			queueCleanup(serviceName)
		}
	}
	entry.Record = dt.pendingPodDeletions[entry.Key]
}

func (dt *DiffTracker) preparePodFinalizerLocked(pod *v1.Pod, entry *pendingPodToProcess, completedService string) (ready, live bool) {
	if dt.pendingPodDeletions[entry.Key] != entry.Record {
		return false, false
	}
	live = dt.podWantsEgressLocked(pod)
	currentService := podEgressGatewayName(pod)
	currentAddresses := PodEgressAddresses(pod)
	preserveCurrentService := live && pod.Status.HostIP != "" && len(currentAddresses) > 0
	// Workers cannot inspect any intermediate per-identity record while this lock is held.
	plans := dt.unlabelledPodReleasePlansLocked(pod, entry.Record, completedService)
	for _, plan := range plans {
		if preserveCurrentService && plan.serviceName == currentService {
			// Registration publishes the replacement before pruning this service's old tuples.
			continue
		}
		dt.deletePodLocked(plan.serviceName, plan.location, plan.addresses, pod.Namespace, pod.Name, string(pod.UID), false, true)
	}
	if !live {
		identity := podEgressGatewayName(pod)
		if identity != "" && identity != completedService && (pod.Status.HostIP == "" || len(PodEgressAddresses(pod)) == 0) {
			dt.deletePodWithoutAddressesLocked(identity, pod.Namespace, pod.Name, string(pod.UID))
		}
	}
	current := *dt.pendingPodDeletions[entry.Key]
	if current.ResolutionPending {
		resolved := &current
		retainCleanup := func(serviceName string, cleanup podCleanupState) {
			if serviceName == completedService || preserveCurrentService && serviceName == currentService ||
				cleanup.WaitForServiceDeletion || !dt.queuePodServiceCleanupLocked(serviceName) {
				return
			}
			cleanup.WaitForServiceDeletion = true
			resolved = resolved.withCleanup(serviceName, cleanup)
		}
		for _, serviceName := range current.ResolutionServices {
			cleanup, exists := current.cleanupFor(serviceName)
			if !exists {
				cleanup.Addresses = slices.Clone(current.ResolutionAddresses)
				cleanup.VerifyServiceDrain = len(cleanup.Addresses) == 0
			}
			retainCleanup(serviceName, cleanup)
		}
		current = *resolved
	}
	current.ResolutionPending = false
	current.ResolutionAddresses = nil
	current.ResolutionServices = nil
	entry.Record = &current
	if completedService != "" {
		if _, exists := current.cleanupFor(completedService); exists {
			entry.Record = current.withCleanup(completedService, podCleanupState{})
		}
	}
	dt.pendingPodDeletions[entry.Key] = entry.Record
	return dt.podCleanupReadyLocked(entry.Record), live
}

func (dt *DiffTracker) retirePodDeletionLocked(entry *pendingPodToProcess) bool {
	if dt.pendingPodDeletions[entry.Key] != entry.Record {
		return false
	}
	delete(dt.pendingPodDeletions, entry.Key)
	if entry.Record.RecoveredAtStartup {
		recordFinalizerRecovered()
	}
	for _, other := range entry.Record.OtherUIDs {
		if other.RecoveredAtStartup {
			recordFinalizerRecovered()
		}
	}
	return true
}

// The caller holds the Pod guard, but not dt.mu, throughout the fresh checks and API write.
func (dt *DiffTracker) processPodFinalizerGuarded(ctx context.Context, entry *pendingPodToProcess, completedService string) (retired bool, readd *v1.Pod, err error) {
	dt.mu.Lock()
	if dt.pendingPodDeletions[entry.Key] != entry.Record {
		dt.mu.Unlock()
		return false, nil, nil
	}
	dt.mu.Unlock()

	pod, err := dt.getPodByNamespaceName(ctx, entry.Record.Namespace, entry.Record.Name)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return false, nil, err
		}
		pod = nil
	}

	dt.mu.Lock()
	if dt.pendingPodDeletions[entry.Key] != entry.Record {
		dt.mu.Unlock()
		return false, nil, nil
	}
	group := entry.Record
	var current *PendingPodDeletion
	if pod != nil {
		current = group.forUID(string(pod.UID))
		if current != nil && current.UID != "" && current.UID != string(pod.UID) {
			current = nil
		}
		if current == nil && group.UID == "" {
			current = group
		}
	}
	recoveredDeparted := 0
	drainDeparted := func(record *PendingPodDeletion) {
		if record == current {
			return
		}
		if record.UID != "" || record.ResolutionPending && pod == nil {
			departed := *record
			departed.OtherUIDs = nil
			entry.Record = &departed
			dt.pendingPodDeletions[entry.Key] = entry.Record
			dt.drainDepartedPodLocked(entry, "")
		}
		if record.RecoveredAtStartup {
			recoveredDeparted++
		}
	}
	drainDeparted(group)
	for _, other := range group.OtherUIDs {
		drainDeparted(other)
	}
	if current == nil {
		entry.Record = group
		dt.pendingPodDeletions[entry.Key] = group
		retired = dt.retirePodDeletionLocked(entry)
		dt.mu.Unlock()
		return retired, nil, nil
	}
	for range recoveredDeparted {
		recordFinalizerRecovered()
	}
	selected := *current
	selected.UID, selected.OtherUIDs = string(pod.UID), nil
	entry.Record = &selected
	dt.pendingPodDeletions[entry.Key] = entry.Record
	completedCleanup, _ := entry.Record.cleanupFor(completedService)
	if !completedCleanup.WaitForServiceDeletion {
		completedService = ""
	}
	ready, live := dt.preparePodFinalizerLocked(pod, entry, completedService)
	if live {
		readd = pod
	}
	dt.mu.Unlock()

	finished := ready
	if ready && !live && hasPodFinalizer(pod) {
		err = dt.removePodFinalizerIf(ctx, &v1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: pod.Namespace, Name: pod.Name, UID: types.UID(entry.Record.UID),
		}}, func(current *v1.Pod) bool {
			dt.mu.Lock()
			defer dt.mu.Unlock()
			ready, live := dt.preparePodFinalizerLocked(current, entry, completedService)
			finished = ready
			if live {
				readd = current
			}
			return ready && !live
		})
	}
	if readd != nil {
		dt.podInformerRegisterPodGuarded(ctx, readd)
		if err == nil {
			err = ctx.Err()
		}
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if err != nil {
		if completedCleanup.WaitForServiceDeletion && dt.pendingPodDeletions[entry.Key] == entry.Record {
			entry.Record = entry.Record.withCleanup(completedService, completedCleanup)
			dt.pendingPodDeletions[entry.Key] = entry.Record
		}
		if readd != nil && ctx.Err() != nil {
			dt.queuePodResolutionLocked(readd, false)
		}
		return false, readd, err
	}
	if finished {
		retired = dt.retirePodDeletionLocked(entry)
	}
	return retired, readd, nil
}

func (dt *DiffTracker) processPodFinalizer(ctx context.Context, entry *pendingPodToProcess, completedService string) error {
	unlock, err := dt.lockPodFinalizer(ctx, entry.Key)
	if err != nil {
		return err
	}
	_, _, err = dt.processPodFinalizerGuarded(ctx, entry, completedService)
	unlock()
	return err
}

func (dt *DiffTracker) podDeletionNeedsProcessingLocked(pending *PendingPodDeletion) bool {
	if pending.ResolutionPending || pending.VerifyUnknownEgressDrain || dt.podCleanupReadyLocked(pending) {
		return true
	}
	for _, other := range pending.OtherUIDs {
		if other.ResolutionPending || other.VerifyUnknownEgressDrain || dt.podCleanupReadyLocked(other) {
			return true
		}
	}
	return false
}

func (dt *DiffTracker) hasUnbackedOutboundMappingLocked() bool {
	for location, nrpLocation := range dt.NRPResources.Locations {
		for address, nrpAddress := range nrpLocation.Addresses {
			if nrpAddress.Services == nil {
				continue
			}
			for _, serviceName := range nrpAddress.Services.UnsortedList() {
				if !dt.NRPResources.NATGateways.Has(serviceName) || !dt.isOutboundIdentityLocked(serviceName) ||
					dt.outboundPodExistsLocked(serviceName, location, address) {
					continue
				}
				backed := false
				for _, pending := range dt.pendingPods[serviceName] {
					if pending.Location == location && pending.Address == address {
						backed = true
						break
					}
				}
				if !backed {
					return true
				}
			}
		}
	}
	return false
}

func (dt *DiffTracker) acknowledgePodLocationSyncLocked() {
	acknowledge := func(pending *PendingPodDeletion) *PendingPodDeletion {
		updated := pending
		if pending.UnknownLocationSyncPending {
			completed := *pending
			completed.UnknownLocationSyncPending = false
			updated = &completed
		}
		complete := func(serviceName string, cleanup podCleanupState) {
			if cleanup.LocationSyncPending && dt.podLocationsDrainedLocked(&PendingPodDeletion{
				UID: pending.UID, ServiceName: serviceName, Addresses: cleanup.Addresses,
				VerifyServiceDrain: cleanup.VerifyServiceDrain,
			}) {
				cleanup.LocationSyncPending = false
				updated = updated.withCleanup(serviceName, cleanup)
			}
		}
		if pending.ServiceCleanup == nil {
			cleanup, _ := pending.cleanupFor(pending.ServiceName)
			complete(pending.ServiceName, cleanup)
		} else {
			for serviceName, cleanup := range pending.ServiceCleanup {
				complete(serviceName, cleanup)
			}
		}
		return updated
	}
	for podKey, pending := range dt.pendingPodDeletions {
		updated := acknowledge(pending)
		for _, other := range pending.OtherUIDs {
			if acknowledged := acknowledge(other); acknowledged != other {
				updated = updated.withUID(acknowledged)
			}
		}
		if updated != pending {
			dt.pendingPodDeletions[podKey] = updated
		}
	}
}

// CheckPendingPodDeletions follows a successful or no-diff pass of the serialized LocationsUpdater.
// A true result requests a paced retry for discovery, drain acknowledgment or ready API writes.
func (dt *DiffTracker) CheckPendingPodDeletions(ctx context.Context) (readyRemovalPending bool) {
	return dt.checkPendingPodDeletions(ctx, true)
}

func (dt *DiffTracker) checkPendingPodDeletions(ctx context.Context, acknowledgeLocations bool) (readyRemovalPending bool) {
	dt.mu.Lock()
	if acknowledgeLocations {
		dt.acknowledgePodLocationSyncLocked()
	}
	var toProcess []pendingPodToProcess
	for podKey, pending := range dt.pendingPodDeletions {
		if dt.podDeletionNeedsProcessingLocked(pending) {
			toProcess = append(toProcess, pendingPodToProcess{Key: podKey, Record: pending})
		}
	}
	dt.mu.Unlock()
	for i := range toProcess {
		if err := dt.processPodFinalizer(ctx, &toProcess[i], ""); err != nil {
			dt.logger.V(4).Info("Could not resolve or release pod finalizer; will retry", "pod", toProcess[i].Key, "err", err)
		}
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	for _, pending := range dt.pendingPodDeletions {
		if pending.hasPendingLocationSync() || dt.podDeletionNeedsProcessingLocked(pending) {
			return true
		}
	}
	return false
}

// serviceHasUnbackedNRPAddressLocked reports whether NRP maps an address to the service that is not
// backed by current live or buffered desired pod state. It is used for terminating pods first
// observed without PodIPs, where the exact old address set cannot be reconstructed. Requires dt.mu.
func (dt *DiffTracker) serviceHasUnbackedNRPAddressLocked(serviceName string) (string, bool) {
	for _, nrpLocation := range dt.NRPResources.Locations {
		for address, nrpAddress := range nrpLocation.Addresses {
			if nrpAddress.Services == nil || !nrpAddress.Services.Has(serviceName) {
				continue
			}
			if !dt.outboundAddressDesiredLocked(serviceName, address) {
				return address, true
			}
		}
	}
	return "", false
}

// outboundAddressDesiredLocked reports whether an address is still desired by a live or buffered pod
// for the service. Requires dt.mu.
func (dt *DiffTracker) outboundAddressDesiredLocked(serviceName, address string) bool {
	for _, node := range dt.K8sResources.Nodes {
		if pod, ok := node.Pods[address]; ok &&
			pod.PublicOutboundIdentity != "" &&
			strings.EqualFold(pod.PublicOutboundIdentity, serviceName) {
			return true
		}
	}
	for _, pending := range dt.pendingPods[serviceName] {
		if pending.Address == address {
			return true
		}
	}
	return false
}

// podAddressStillInNRPLocked reports whether any of a pending pod's addresses is still in NRP for its
// service, searching every location (a dual-stack pod's families live under per-family node
// locations). The caller keeps the finalizer until every address has left NRP. Requires dt.mu held.
func (dt *DiffTracker) podAddressStillInNRPLocked(pending *PendingPodDeletion) (string, bool) {
	if pending.ServiceCleanup != nil {
		for serviceName, cleanup := range pending.ServiceCleanup {
			for _, address := range cleanup.Addresses {
				if dt.outboundAddressInAnyNRPLocationLocked(serviceName, address, pending.UID) {
					return address, true
				}
			}
		}
		return "", false
	}
	for _, address := range pending.Addresses {
		if dt.outboundAddressInAnyNRPLocationLocked(pending.ServiceName, address, pending.UID) {
			return address, true
		}
	}
	return "", false
}

// outboundAddressInAnyNRPLocationLocked reports whether the given address is registered in NRP for
// the service under any node location. A known UID excludes mappings positively owned by another
// Pod; an empty UID keeps the service-wide check. Must be called with dt.mu held.
func (dt *DiffTracker) outboundAddressInAnyNRPLocationLocked(serviceName, address, uid string) bool {
	for location, nrpLocation := range dt.NRPResources.Locations {
		nrpAddress, ok := nrpLocation.Addresses[address]
		if !ok {
			continue
		}
		if nrpAddress.Services != nil && nrpAddress.Services.Has(serviceName) &&
			!dt.outboundAddressOwnedByDifferentUIDLocked(serviceName, location, address, uid) {
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

// RemoveLastPodFinalizers completes one service's cleanup condition. Other identities can still
// hold the Pod's finalizer; API failures retain this service's condition for a later retry.
func (dt *DiffTracker) RemoveLastPodFinalizers(ctx context.Context, serviceName string) error {
	dt.mu.Lock()
	var toProcess []pendingPodToProcess
	for podKey, pending := range dt.pendingPodDeletions {
		if pending.waitsForService(serviceName) {
			toProcess = append(toProcess, pendingPodToProcess{Key: podKey, Record: pending})
		}
	}
	dt.mu.Unlock()
	var failed []string
	for i := range toProcess {
		entry := &toProcess[i]
		var lastErr error
		retryErr := wait.ExponentialBackoffWithContext(ctx, finalizerRetryBackoff, func(ctx context.Context) (bool, error) {
			lastErr = dt.processPodFinalizer(ctx, entry, serviceName)
			return lastErr == nil, nil
		})
		if retryErr != nil {
			dt.logger.V(4).Info("Could not release last-pod finalizer after retries", "pod", entry.Key, "err", retryErr, "lastErr", lastErr)
			failed = append(failed, entry.Key)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("RemoveLastPodFinalizers: %d last-pod finalizer(s) for service %s could not be removed after retries: %v", len(failed), serviceName, failed)
	}
	return nil
}
