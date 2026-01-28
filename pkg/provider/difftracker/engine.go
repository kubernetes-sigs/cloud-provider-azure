package difftracker

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
)

// triggerLocationsUpdater sends a non-blocking trigger to the LocationsUpdater.
func (dt *DiffTracker) triggerLocationsUpdater() {
	// Track triggers during initialization (check WITHOUT lock - use atomic read)
	// This function is called from contexts where dt.mu is already held,
	// so we can't acquire it again (even with recursive mutex, it's unnecessary)
	shouldTrack := atomic.LoadInt32(&dt.isInitializing) == 1

	// Only increment counter if trigger is successfully sent
	select {
	case dt.locationsUpdaterTrigger <- true:
		if shouldTrack {
			atomic.AddInt32(&dt.pendingUpdaterTriggers, 1)
		}
	default:
		// Channel full, trigger dropped - don't increment counter
	}
}

// triggerServiceUpdater sends a non-blocking trigger to the ServiceUpdater.
func (dt *DiffTracker) triggerServiceUpdater() {
	// Track triggers during initialization (check WITHOUT lock - use atomic read)
	// This function is called from contexts where dt.mu is already held,
	// so we can't acquire it again (even with recursive mutex, it's unnecessary)
	shouldTrack := atomic.LoadInt32(&dt.isInitializing) == 1

	// Only increment counter if trigger is successfully sent
	select {
	case dt.serviceUpdaterTrigger <- true:
		if shouldTrack {
			atomic.AddInt32(&dt.pendingUpdaterTriggers, 1)
		}
		klog.V(4).Infof("Engine.triggerServiceUpdater: trigger sent successfully")
	default:
		// Channel full, trigger dropped - don't increment counter
		klog.Warningf("Engine.triggerServiceUpdater: trigger DROPPED - channel full!")
	}
}

// AddService handles service creation events for inbound (Load Balancer) services.
// If the service already exists in NRP, it does nothing (idempotent).
// If the service doesn't exist, it triggers service creation via XUpdater.
func (dt *DiffTracker) AddService(config ServiceConfig) {
	startTime := time.Now()
	defer func() {
		recordEngineOperation("add_service", startTime, nil)
		updatePendingServiceOperationsMetric(dt)
		updateTrackedServicesMetric(dt)
	}()

	dt.mu.Lock()

	// Validate configuration
	if err := config.Validate(); err != nil {
		dt.mu.Unlock()
		klog.Errorf("Engine.AddService: invalid config: %v", err)
		return
	}

	serviceUID := config.UID
	klog.V(4).Infof("Engine.AddService: serviceUID=%s, isInbound=%v", serviceUID, config.IsInbound)

	// Check if service already exists in NRP
	if config.IsInbound {
		if dt.NRPResources.LoadBalancers.Has(serviceUID) {
			dt.mu.Unlock()
			klog.V(2).Infof("Engine.AddService: Load Balancer %s already exists in NRP", serviceUID)
			return
		}
	} else {
		if dt.NRPResources.NATGateways.Has(serviceUID) {
			dt.mu.Unlock()
			klog.V(2).Infof("Engine.AddService: NAT Gateway %s already exists in NRP", serviceUID)
			return
		}
	}

	// Check if service operation is already tracked
	opState, exists := dt.pendingServiceOps[serviceUID]
	if exists {
		dt.mu.Unlock()
		klog.V(2).Infof("Engine.AddService: Service %s already tracked with state %v", serviceUID, opState.State)
		return
	}

	// Service doesn't exist - need to create it
	klog.V(2).Infof("Engine.AddService: Service %s doesn't exist, triggering creation", serviceUID)

	// Add service operation to pending list
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID:  serviceUID,
		Config:      config,
		State:       StateNotStarted,
		RetryCount:  0,
		LastAttempt: time.Now().Format(time.RFC3339),
	}

	// Release lock before triggering to avoid lock contention
	dt.mu.Unlock()

	dt.triggerServiceUpdater()
}

// UpdateEndpoints handles endpoint updates for inbound (Load Balancer) services.
// If the service is already created in NRP, endpoints are immediately updated.
// If the service is being created, endpoints are buffered until creation completes.
// If the service doesn't exist, this shouldn't happen (AddService should be called first).
func (dt *DiffTracker) UpdateEndpoints(serviceUID string, oldPodIPToNodeIP, newPodIPToNodeIP map[string]string) {
	startTime := time.Now()
	defer func() {
		recordEngineOperation("update_endpoints", startTime, nil)
		updateBufferedUpdatesMetric(dt)
	}()

	dt.mu.Lock()
	defer dt.mu.Unlock()

	if serviceUID == "" {
		klog.Error("Engine.UpdateEndpoints: serviceUID cannot be empty")
		return
	}

	klog.V(4).Infof("Engine.UpdateEndpoints: serviceUID=%s, old=%d, new=%d", serviceUID, len(oldPodIPToNodeIP), len(newPodIPToNodeIP))

	// Check if service operation is tracked
	opState, exists := dt.pendingServiceOps[serviceUID]

	if !exists {
		// Check if service exists in NRP (created outside Engine)
		if dt.NRPResources.LoadBalancers.Has(serviceUID) {
			klog.V(2).Infof("Engine.UpdateEndpoints: Service %s exists in NRP, updating endpoints immediately", serviceUID)
			errs := dt.updateK8sEndpointsLocked(UpdateK8sEndpointsInputType{
				InboundIdentity: serviceUID,
				OldAddresses:    oldPodIPToNodeIP,
				NewAddresses:    newPodIPToNodeIP,
			})
			if len(errs) > 0 {
				klog.Errorf("Engine.UpdateEndpoints: Failed to update endpoints for service %s: %v", serviceUID, errs)
				// Still trigger LocationsUpdater even if some endpoints failed
			}
			// Trigger LocationsUpdater to sync the changes
			dt.triggerLocationsUpdater()
			return
		}

		// Service doesn't exist and not tracked - this shouldn't happen
		klog.Warningf("Engine.UpdateEndpoints: Service %s not found in NRP or pending operations, buffering endpoints anyway", serviceUID)
		dt.pendingEndpoints[serviceUID] = append(dt.pendingEndpoints[serviceUID], PendingEndpointUpdate{
			PodIPToNodeIP: newPodIPToNodeIP,
			Timestamp:     time.Now().Format(time.RFC3339),
		})
		return
	}

	// Service operation exists - check state
	switch opState.State {
	case StateNotStarted, StateCreationInProgress:
		// Service is being created or waiting to be created - buffer the endpoints (only store new state, old state will be empty when promoting)
		klog.V(2).Infof("Engine.UpdateEndpoints: Service %s is being created (state=%v), buffering %d endpoints", serviceUID, opState.State, len(newPodIPToNodeIP))
		dt.pendingEndpoints[serviceUID] = append(dt.pendingEndpoints[serviceUID], PendingEndpointUpdate{
			PodIPToNodeIP: newPodIPToNodeIP,
			Timestamp:     time.Now().Format(time.RFC3339),
		})

	case StateCreated:
		// Service is ready - update endpoints immediately
		klog.V(2).Infof("Engine.UpdateEndpoints: Service %s is ready, updating endpoints immediately (old=%d, new=%d)", serviceUID, len(oldPodIPToNodeIP), len(newPodIPToNodeIP))
		errs := dt.updateK8sEndpointsLocked(UpdateK8sEndpointsInputType{
			InboundIdentity: serviceUID,
			OldAddresses:    oldPodIPToNodeIP,
			NewAddresses:    newPodIPToNodeIP,
		})
		if len(errs) > 0 {
			klog.Errorf("Engine.UpdateEndpoints: Failed to update endpoints for service %s: %v", serviceUID, errs)
			// Still trigger LocationsUpdater even if some endpoints failed
		}
		// Trigger LocationsUpdater to sync the changes
		dt.triggerLocationsUpdater()

	case StateDeletionPending:
		// Service is pending deletion - still process endpoint removals to clear NRP locations
		klog.V(2).Infof("Engine.UpdateEndpoints: Service %s is pending deletion, processing endpoint update to clear locations (old=%d, new=%d)", serviceUID, len(oldPodIPToNodeIP), len(newPodIPToNodeIP))
		errs := dt.updateK8sEndpointsLocked(UpdateK8sEndpointsInputType{
			InboundIdentity: serviceUID,
			OldAddresses:    oldPodIPToNodeIP,
			NewAddresses:    newPodIPToNodeIP,
		})
		if len(errs) > 0 {
			klog.Errorf("Engine.UpdateEndpoints: Failed to update endpoints for service %s: %v", serviceUID, errs)
		}
		dt.triggerLocationsUpdater()

	case StateDeletionInProgress:
		// Service deletion already in progress - ignore endpoint updates
		klog.V(4).Infof("Engine.UpdateEndpoints: Service %s deletion in progress, ignoring endpoint update", serviceUID)

	default:
		klog.Errorf("Engine.UpdateEndpoints: Unknown state %v for service %s", opState.State, serviceUID)
	}
}

// DeleteService handles service deletion events for inbound (Load Balancer) services.
// It marks the service for deletion and triggers DeletionChecker to verify locations are cleared.
// DeleteService schedules a service for deletion. If isOrphan is true, the service is an orphaned
// Azure resource (exists in Azure but not in ServiceGateway) and we skip the NRP existence check.
func (dt *DiffTracker) DeleteService(serviceUID string, isInbound bool, isOrphan bool) {
	startTime := time.Now()
	defer func() {
		recordEngineOperation("delete_service", startTime, nil)
		updatePendingServiceOperationsMetric(dt)
		updatePendingServiceDeletionsMetric(dt)
	}()

	dt.mu.Lock()

	if serviceUID == "" {
		dt.mu.Unlock()
		klog.Error("Engine.DeleteService: serviceUID cannot be empty")
		return
	}

	klog.V(4).Infof("Engine.DeleteService: serviceUID=%s, isInbound=%v, isOrphan=%v", serviceUID, isInbound, isOrphan)

	// Check if service exists in pending operations
	opState, exists := dt.pendingServiceOps[serviceUID]

	if !exists {
		// Service not tracked - check if it exists in NRP (skip for orphans)
		var existsInNRP bool
		if !isOrphan {
			if isInbound {
				existsInNRP = dt.NRPResources.LoadBalancers.Has(serviceUID)
			} else {
				existsInNRP = dt.NRPResources.NATGateways.Has(serviceUID)
			}

			if !existsInNRP {
				dt.mu.Unlock()
				klog.V(2).Infof("Engine.DeleteService: Service %s doesn't exist in NRP or pending operations, nothing to delete", serviceUID)
				return
			}
		}

		// Service exists in NRP (or is orphan) but not tracked - create tracking entry
		if isOrphan {
			klog.V(2).Infof("Engine.DeleteService: Orphaned service %s, marking for deletion", serviceUID)
		} else {
			klog.V(2).Infof("Engine.DeleteService: Service %s exists in NRP, marking for deletion", serviceUID)
		}
		var config ServiceConfig
		if isInbound {
			config = NewInboundServiceConfig(serviceUID, nil)
		} else {
			config = NewOutboundServiceConfig(serviceUID, nil)
		}
		dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
			ServiceUID:  serviceUID,
			Config:      config,
			State:       StateDeletionPending,
			RetryCount:  0,
			LastAttempt: time.Now().Format(time.RFC3339),
		}
	} else {
		// Service is tracked - update state based on current state
		switch opState.State {
		case StateNotStarted:
			klog.V(2).Infof("Engine.DeleteService: Service %s not yet started, marking for deletion", serviceUID)
			opState.State = StateDeletionPending

		case StateCreationInProgress:
			klog.Warningf("Engine.DeleteService: Service %s is being created but deletion requested, marking for deletion", serviceUID)
			opState.State = StateDeletionPending

		case StateCreated:
			klog.V(2).Infof("Engine.DeleteService: Service %s is ready, marking for deletion", serviceUID)
			opState.State = StateDeletionPending

		case StateDeletionPending, StateDeletionInProgress:
			dt.mu.Unlock()
			klog.V(2).Infof("Engine.DeleteService: Service %s is already being deleted", serviceUID)
			return

		default:
			dt.mu.Unlock()
			klog.Errorf("Engine.DeleteService: Unknown state %v for service %s", opState.State, serviceUID)
			return
		}
	}

	// Clear any buffered endpoints/pods for this service
	delete(dt.pendingEndpoints, serviceUID)
	delete(dt.pendingPods, serviceUID)

	// Add to pending deletions (will be checked by LocationsUpdater after next sync)
	dt.pendingServiceDeletions[serviceUID] = &PendingServiceDeletion{
		ServiceUID: serviceUID,
		IsInbound:  isInbound,
		Timestamp:  time.Now().Format(time.RFC3339),
	}

	// Proactively remove service from K8s state to trigger location cleanup
	// This ensures LocationsUpdater will sync the removal to NRP without waiting for EndpointSlice events
	dt.removeServiceFromK8sStateLocked(serviceUID, isInbound)

	// Check immediately if locations are already clear
	// Will be re-checked after each location sync
	hasLocations := dt.serviceHasLocationsInNRP(serviceUID)
	shouldTriggerServiceUpdater := false
	if !hasLocations {
		klog.V(2).Infof("Engine.DeleteService: Service %s has no locations, ready for immediate deletion", serviceUID)
		// Get the state pointer (may be newly created or from earlier in this function)
		if opState, exists := dt.pendingServiceOps[serviceUID]; exists {
			opState.State = StateDeletionInProgress
		}
		delete(dt.pendingServiceDeletions, serviceUID)
		shouldTriggerServiceUpdater = true
	}

	// Release lock before triggering to avoid lock contention
	dt.mu.Unlock()

	if shouldTriggerServiceUpdater {
		dt.triggerServiceUpdater()
	} else {
		// Trigger LocationsUpdater to sync the K8s state changes to NRP
		// This will clear locations and then CheckPendingServiceDeletions will transition to StateDeletionInProgress
		dt.triggerLocationsUpdater()
	}
}

// OnServiceCreationComplete is called by ServiceUpdater after service creation or deletion completes.
// For creation: promotes buffered endpoints/pods and updates the service state.
// For deletion: cleans up Engine state.
func (dt *DiffTracker) OnServiceCreationComplete(serviceUID string, success bool, err error) {
	startTime := time.Now()
	defer func() {
		recordEngineOperation("service_creation_complete", startTime, err)
		updatePendingServiceOperationsMetric(dt)
		updateBufferedUpdatesMetric(dt)
	}()

	dt.mu.Lock()
	defer dt.mu.Unlock()

	opState, exists := dt.pendingServiceOps[serviceUID]
	if !exists {
		klog.Warningf("Engine.OnServiceCreationComplete: Service %s not found in pending operations", serviceUID)
		return
	}

	// Determine if this is creation or deletion based on current state
	isDeletion := (opState.State == StateDeletionInProgress)

	if isDeletion {
		// Handle deletion completion
		if success {
			klog.V(2).Infof("Engine.OnServiceCreationComplete: Service %s deleted successfully", serviceUID)
			recordServiceOperation("delete", opState.Config.IsInbound, startTime, nil)
			// Clean up all state
			delete(dt.pendingServiceOps, serviceUID)
			delete(dt.pendingEndpoints, serviceUID)
			delete(dt.pendingPods, serviceUID)
			delete(dt.pendingServiceDeletions, serviceUID)

			// Check if initialization is complete after service deletion
			dt.checkInitializationCompleteLocked()
		} else {
			klog.Errorf("Engine.OnServiceCreationComplete: Service %s deletion failed: %v", serviceUID, err)
			recordServiceOperation("delete", opState.Config.IsInbound, startTime, err)
			opState.RetryCount++
			opState.LastAttempt = time.Now().Format(time.RFC3339)
			recordServiceOperationRetry("delete", opState.Config.IsInbound, opState.RetryCount)

			klog.V(2).Infof("Engine.OnServiceCreationComplete: Service %s deletion will be retried (attempt %d)", serviceUID, opState.RetryCount)
			// Trigger ServiceUpdater for retry
			dt.triggerServiceUpdater()
		}
	} else {
		// Handle creation completion
		if success {
			// RACE CONDITION FIX: Check if deletion was requested while creation was in progress
			// If StateDeletionPending, don't mark as Created - instead trigger deletion flow
			if opState.State == StateDeletionPending {
				klog.Warningf("Engine.OnServiceCreationComplete: Service %s creation completed but deletion pending, triggering deletion", serviceUID)
				recordStateTransition(opState.State, StateCreated)
				opState.State = StateCreated // Briefly mark as created so deletion flow works correctly

				// Check if locations are already clear (they should be since we clear K8s state in DeleteService)
				hasLocations := dt.serviceHasLocationsInNRP(serviceUID)
				if !hasLocations {
					// Ready for immediate deletion
					opState.State = StateDeletionInProgress
					delete(dt.pendingServiceDeletions, serviceUID)
					dt.triggerServiceUpdater()
				} else {
					// Need to wait for locations to clear - trigger LocationsUpdater
					// CheckPendingServiceDeletions will handle the transition
					dt.triggerLocationsUpdater()
				}
				return
			}

			klog.V(2).Infof("Engine.OnServiceCreationComplete: Service %s created successfully", serviceUID)
			recordStateTransition(opState.State, StateCreated)
			recordServiceOperation("create", opState.Config.IsInbound, startTime, nil)
			opState.State = StateCreated
			opState.RetryCount = 0

			// Promote any pending endpoints and pods
			dt.promotePendingEndpointsLocked(serviceUID)
			dt.promotePendingPodsLocked(serviceUID)

			// Trigger LocationsUpdater to sync the service state (whether buffers existed or not)
			dt.triggerLocationsUpdater()

			// Check if initialization is complete after service creation
			dt.checkInitializationCompleteLocked()

		} else {
			klog.Errorf("Engine.OnServiceCreationComplete: Service %s creation failed: %v", serviceUID, err)
			recordServiceOperation("create", opState.Config.IsInbound, startTime, err)
			opState.RetryCount++
			opState.LastAttempt = time.Now().Format(time.RFC3339)
			recordServiceOperationRetry("create", opState.Config.IsInbound, opState.RetryCount)

			klog.V(2).Infof("Engine.OnServiceCreationComplete: Service %s creation will be retried (attempt %d)", serviceUID, opState.RetryCount)
			// Reset to NotStarted for retry
			recordStateTransition(opState.State, StateNotStarted)
			opState.State = StateNotStarted
			// Trigger ServiceUpdater for retry
			dt.triggerServiceUpdater()
		}
	}
}

// promotePendingEndpointsLocked flushes all pending endpoints for a service after it's created.
// Must be called with dt.mu held.
func (dt *DiffTracker) promotePendingEndpointsLocked(serviceUID string) {
	pendingEndpoints, exists := dt.pendingEndpoints[serviceUID]
	if !exists || len(pendingEndpoints) == 0 {
		return
	}

	klog.V(2).Infof("Engine.promotePendingEndpointsLocked: Promoting %d pending endpoint updates for service %s",
		len(pendingEndpoints), serviceUID)

	// Merge all pending endpoint updates (last one wins for each pod IP)
	mergedEndpoints := make(map[string]string)
	for _, update := range pendingEndpoints {
		for podIP, nodeIP := range update.PodIPToNodeIP {
			mergedEndpoints[podIP] = nodeIP
		}
	}

	klog.V(4).Infof("Engine.promotePendingEndpointsLocked: Merged to %d unique endpoints", len(mergedEndpoints))

	// When promoting buffered endpoints, OldAddresses should be empty since service was just created
	errs := dt.updateK8sEndpointsLocked(UpdateK8sEndpointsInputType{
		InboundIdentity: serviceUID,
		OldAddresses:    make(map[string]string),
		NewAddresses:    mergedEndpoints,
	})
	if len(errs) > 0 {
		klog.Errorf("Engine.promotePendingEndpointsLocked: Failed to update endpoints for service %s: %v",
			serviceUID, errs)
		// Continue to clear buffer and trigger LocationsUpdater for partial success
	}

	// Clear pending endpoints
	delete(dt.pendingEndpoints, serviceUID)
}

// AddPod handles pod addition events for outbound (NAT Gateway) services.
// If the service is already created in NRP, the pod is immediately added to DiffTracker.
// If the service is being created, the pod is buffered until creation completes.
// If the service doesn't exist, it triggers service creation and buffers the pod.
func (dt *DiffTracker) AddPod(serviceUID, podKey, location, address string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	if serviceUID == "" || location == "" || address == "" {
		klog.Errorf("Engine.AddPod: invalid parameters - serviceUID=%s, location=%s, address=%s", serviceUID, location, address)
		return
	}

	klog.V(4).Infof("Engine.AddPod: serviceUID=%s, podKey=%s, location=%s, address=%s",
		serviceUID, podKey, location, address)

	// Check if service operation is tracked
	opState, exists := dt.pendingServiceOps[serviceUID]

	if !exists {

		// Check if service exists in NRP first (handles restart scenario and is more authoritative)
		if dt.NRPResources.NATGateways.Has(serviceUID) {
			klog.V(2).Infof("Engine.AddPod: Service %s exists in NRP, adding pod %s immediately", serviceUID, podKey)
			err := dt.updateK8sPodLocked(UpdatePodInputType{
				PodOperation:           ADD,
				PublicOutboundIdentity: serviceUID,
				Location:               location,
				Address:                address,
			})
			if err != nil {
				klog.Errorf("Engine.AddPod: Failed to add pod %s: %v", podKey, err)
				// Still trigger LocationsUpdater even if pod add failed
			}
			// Trigger LocationsUpdater to sync the change
			dt.triggerLocationsUpdater()
			return
		}
		// Service doesn't exist - need to create it first
		klog.V(2).Infof("Engine.AddPod: Service %s doesn't exist, creating it and buffering pod %s", serviceUID, podKey)

		// Create service operation
		dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
			ServiceUID:  serviceUID,
			Config:      NewOutboundServiceConfig(serviceUID, nil),
			State:       StateNotStarted,
			RetryCount:  0,
			LastAttempt: time.Now().Format(time.RFC3339),
		}

		// Buffer the pod
		dt.pendingPods[serviceUID] = append(dt.pendingPods[serviceUID], PendingPodUpdate{
			PodKey:    podKey,
			Location:  location,
			Address:   address,
			Timestamp: time.Now().Format(time.RFC3339),
		})

		// Trigger ServiceUpdater to create the service
		dt.triggerServiceUpdater()
		return
	}

	// Service operation exists - check state
	switch opState.State {
	case StateNotStarted, StateCreationInProgress:
		// Service is being created or waiting to be created - buffer the pod
		klog.V(2).Infof("Engine.AddPod: Service %s is being created (state=%v), buffering pod %s", serviceUID, opState.State, podKey)
		dt.pendingPods[serviceUID] = append(dt.pendingPods[serviceUID], PendingPodUpdate{
			PodKey:    podKey,
			Location:  location,
			Address:   address,
			Timestamp: time.Now().Format(time.RFC3339),
		})

	case StateCreated:
		// Service is ready - add pod immediately
		klog.V(2).Infof("Engine.AddPod: Service %s is ready, adding pod %s immediately", serviceUID, podKey)
		err := dt.updateK8sPodLocked(UpdatePodInputType{
			PodOperation:           ADD,
			PublicOutboundIdentity: serviceUID,
			Location:               location,
			Address:                address,
		})
		if err != nil {
			klog.Errorf("Engine.AddPod: Failed to add pod %s: %v", podKey, err)
			// Still trigger LocationsUpdater even if pod add failed
		}

		// Trigger LocationsUpdater to sync the change
		dt.triggerLocationsUpdater()

	case StateDeletionPending, StateDeletionInProgress:
		// Service is being deleted - ignore pod additions
		klog.Warningf("Engine.AddPod: Cannot add pod %s to service %s which is being deleted", podKey, serviceUID)

	default:
		klog.Errorf("Engine.AddPod: Unknown state %v for service %s", opState.State, serviceUID)
	}
}

// DeletePodResult contains the result of a DeletePod operation
type DeletePodResult struct {
	IsLastPod bool // True if this was the last pod for the service
}

// DeletePod handles pod deletion events for outbound (NAT Gateway) services.
// It immediately removes the pod from DiffTracker and triggers LocationsUpdater.
// If this is the last pod for the service, it marks the service for deletion.
// namespace and name are optional - if provided, they enable pod finalizer tracking for last pods.
// Returns DeletePodResult indicating if this was the last pod.
//
// Finalizer handling:
// - Non-last pods: Caller should remove finalizer immediately (no need to wait)
// - Last pods: Tracked in pendingPodDeletions, finalizer removed after NAT Gateway deletion
func (dt *DiffTracker) DeletePod(serviceUID, location, address, namespace, name string) DeletePodResult {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	result := DeletePodResult{IsLastPod: false}

	if serviceUID == "" || location == "" || address == "" {
		klog.Errorf("Engine.DeletePod: invalid parameters - serviceUID=%s, location=%s, address=%s", serviceUID, location, address)
		return result
	}

	klog.V(4).Infof("Engine.DeletePod: serviceUID=%s, location=%s, address=%s, namespace=%s, name=%s",
		serviceUID, location, address, namespace, name)

	// Check counter BEFORE removing pod to determine if this is the last pod
	val, ok := dt.LocalServiceNameToNRPServiceMap.Load(strings.ToLower(serviceUID))
	if !ok {
		klog.Warningf("Engine.DeletePod: Service %s not found in LocalServiceNameToNRPServiceMap", serviceUID)
		// Still try to remove pod from DiffTracker
		err := dt.updateK8sPodLocked(UpdatePodInputType{
			PodOperation:           REMOVE,
			PublicOutboundIdentity: serviceUID,
			Location:               location,
			Address:                address,
		})
		if err != nil {
			klog.Errorf("Engine.DeletePod: Failed to remove pod: %v", err)
		}
		// Trigger LocationsUpdater to sync the change
		dt.triggerLocationsUpdater()
		return result
	}

	counter := val.(int)
	if counter <= 0 {
		klog.Errorf("Engine.DeletePod: Service %s has invalid counter: %d", serviceUID, counter)
		return result
	}

	// Check if this is the last pod BEFORE removing it
	// counter == 1 means "I'm about to remove the last registered pod"
	// After removal, counter becomes 0 → service should be deleted
	isLastPod := (counter == 1)

	// Remove pod from DiffTracker (this also updates the counter via UpdateK8sPod)
	err := dt.updateK8sPodLocked(UpdatePodInputType{
		PodOperation:           REMOVE,
		PublicOutboundIdentity: serviceUID,
		Location:               location,
		Address:                address,
	})

	if err != nil {
		klog.Errorf("Engine.DeletePod: Failed to remove pod: %v", err)
		return result
	}

	result.IsLastPod = isLastPod

	if isLastPod {
		// This was the last pod - mark service for deletion
		// Note: LocalServiceNameToNRPServiceMap already updated by UpdateK8sPod
		klog.V(2).Infof("Engine.DeletePod: Last pod removed for service %s, marking for deletion", serviceUID)

		// Check if service is tracked
		opState, exists := dt.pendingServiceOps[serviceUID]
		if !exists {
			// Service not tracked but exists in NRP - create tracking entry
			dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
				ServiceUID:  serviceUID,
				Config:      NewOutboundServiceConfig(serviceUID, nil),
				State:       StateDeletionPending,
				RetryCount:  0,
				LastAttempt: time.Now().Format(time.RFC3339),
			}
		} else {
			// Update existing tracking
			opState.State = StateDeletionPending
		}

		// Add to pending deletions
		dt.pendingServiceDeletions[serviceUID] = &PendingServiceDeletion{
			ServiceUID: serviceUID,
			IsInbound:  false,
			Timestamp:  time.Now().Format(time.RFC3339),
		}
	}

	// Track pending pod deletion for finalizer removal - ONLY for last pods
	// Non-last pods: Caller removes finalizer immediately after DeletePod returns
	// Last pod: Track here, finalizer removed after NAT Gateway deletion in RemoveLastPodFinalizers
	if isLastPod && namespace != "" && name != "" {
		podKey := fmt.Sprintf("%s/%s", namespace, name)
		dt.pendingPodDeletions[podKey] = &PendingPodDeletion{
			Namespace:  namespace,
			Name:       name,
			ServiceUID: serviceUID,
			Address:    address,
			Location:   location,
			IsLastPod:  true,
			Timestamp:  time.Now().Format(time.RFC3339),
		}
		klog.V(3).Infof("Engine.DeletePod: Added pending last pod deletion for %s", podKey)
		// Update metric after adding
		pendingPodDeletions.Set(float64(len(dt.pendingPodDeletions)))
	}
	// Note: Counter is managed by UpdateK8sPod for both last pod and non-last pod cases

	// Trigger LocationsUpdater to sync the change
	dt.triggerLocationsUpdater()

	return result
}

// promotePendingPodsLocked flushes all pending pods for a service after it's created.
// Must be called with dt.mu held.
func (dt *DiffTracker) promotePendingPodsLocked(serviceUID string) {
	pendingPods, exists := dt.pendingPods[serviceUID]
	if !exists || len(pendingPods) == 0 {
		return
	}

	klog.V(2).Infof("Engine.promotePendingPodsLocked: Promoting %d pending pods for service %s",
		len(pendingPods), serviceUID)

	for _, pod := range pendingPods {
		klog.V(4).Infof("Engine.promotePendingPodsLocked: Adding pod %s (location=%s, address=%s)",
			pod.PodKey, pod.Location, pod.Address)

		err := dt.updateK8sPodLocked(UpdatePodInputType{
			PodOperation:           ADD,
			PublicOutboundIdentity: serviceUID,
			Location:               pod.Location,
			Address:                pod.Address,
		})
		if err != nil {
			klog.Errorf("Engine.promotePendingPodsLocked: Failed to add pod %s: %v", pod.PodKey, err)
			continue
		}
	}

	// Clear pending pods
	delete(dt.pendingPods, serviceUID)
}

// serviceHasLocationsInNRP checks if any locations in NRP reference this service.
// Must be called with dt.mu held.
func (dt *DiffTracker) serviceHasLocationsInNRP(serviceUID string) bool {
	// Iterate through all NRP locations
	for _, nrpLocation := range dt.NRPResources.Locations {
		for _, nrpAddress := range nrpLocation.Addresses {
			if nrpAddress.Services.Has(serviceUID) {
				return true
			}
		}
	}
	return false
}

// CheckPendingServiceDeletions checks each pending deletion to see if locations are cleared.
// This method is called by LocationsUpdater after syncing location changes.
func (dt *DiffTracker) CheckPendingServiceDeletions() {
	startTime := time.Now()
	blockedCount := 0
	defer func() {
		recordDeletionCheck(startTime, blockedCount)
		updatePendingServiceDeletionsMetric(dt)
	}()

	dt.mu.Lock()
	defer dt.mu.Unlock()

	if len(dt.pendingServiceDeletions) == 0 {
		return
	}

	klog.V(4).Infof("Engine.CheckPendingServiceDeletions: Checking %d pending deletions", len(dt.pendingServiceDeletions))

	// Iterate through all pending deletions
	for serviceUID, pendingDeletion := range dt.pendingServiceDeletions {
		klog.V(4).Infof("Engine.CheckPendingServiceDeletions: Checking service %s (isInbound=%v)",
			serviceUID, pendingDeletion.IsInbound)

		// Check if service still has locations in NRP
		hasLocations := dt.serviceHasLocationsInNRP(serviceUID)
		if hasLocations {
			klog.V(4).Infof("Engine.CheckPendingServiceDeletions: Service %s still has locations in NRP, waiting",
				serviceUID)
			blockedCount++
			continue
		}

		// Locations cleared - proceed with deletion
		klog.V(2).Infof("Engine.CheckPendingServiceDeletions: Service %s has no locations, triggering deletion",
			serviceUID)

		// Update service state to DeletionInProgress
		if opState, exists := dt.pendingServiceOps[serviceUID]; exists {
			recordStateTransition(opState.State, StateDeletionInProgress)
			opState.State = StateDeletionInProgress
		} else {
			// Service not in pendingServiceOps - create entry
			klog.Warningf("Engine.CheckPendingServiceDeletions: Service %s in pendingServiceDeletions but not in pendingServiceOps, creating entry",
				serviceUID)
			var config ServiceConfig
			if pendingDeletion.IsInbound {
				config = NewInboundServiceConfig(serviceUID, nil)
			} else {
				config = NewOutboundServiceConfig(serviceUID, nil)
			}
			dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
				ServiceUID:  serviceUID,
				Config:      config,
				State:       StateDeletionInProgress,
				RetryCount:  0,
				LastAttempt: time.Now().Format(time.RFC3339),
			}
		}

		// Trigger ServiceUpdater to delete the service
		dt.triggerServiceUpdater()

		// Remove from pending deletions
		delete(dt.pendingServiceDeletions, serviceUID)
	}

	// Update blocked services metric
	updateServicesBlockedByLocationsMetric(blockedCount)
}

// ================================================================================================
// Initialization synchronization methods
// ================================================================================================

// WaitForInitialSync blocks until initialization completes or context is cancelled
// Used during InitializeFromCluster to wait for all async operations to finish
func (dt *DiffTracker) WaitForInitialSync(ctx context.Context) error {
	dt.mu.Lock()
	ch := dt.initCompletionChecker
	dt.mu.Unlock()

	if ch == nil {
		return fmt.Errorf("WaitForInitialSync called before initialization started")
	}

	klog.Infof("Engine.WaitForInitialSync: waiting for initialization to complete")

	select {
	case <-ch:
		klog.Infof("Engine.WaitForInitialSync: initialization completed successfully")
		return nil
	case <-ctx.Done():
		klog.Warningf("Engine.WaitForInitialSync: context cancelled: %v", ctx.Err())
		return ctx.Err()
	}
}

// checkInitializationComplete checks if initialization is done and signals completion
// Must be called by updaters after completing their work
// This version acquires the lock
func (dt *DiffTracker) checkInitializationComplete() {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.checkInitializationCompleteLocked()
}

// checkInitializationCompleteLocked checks initialization completion
// Assumes dt.mu is already held by caller
func (dt *DiffTracker) checkInitializationCompleteLocked() {
	// Only check if we're still initializing
	if atomic.LoadInt32(&dt.isInitializing) == 0 {
		return
	}

	// Check if all work is complete:
	// 1. No pending service operations (only count services NOT in StateCreated)
	// 2. No in-flight updater triggers (LocationsUpdater work)
	// Services in StateCreated are done creating but remain tracked for runtime operations
	pendingOps := 0
	for _, opState := range dt.pendingServiceOps {
		if opState.State != StateCreated {
			pendingOps++
		}
	}
	inFlightTriggers := atomic.LoadInt32(&dt.pendingUpdaterTriggers)

	if pendingOps == 0 && inFlightTriggers == 0 {
		klog.Infof("Engine.checkInitializationComplete: all operations complete (pendingOps=%d, inFlightTriggers=%d), signaling completion",
			pendingOps, inFlightTriggers)

		// Mark initialization as done (idempotent using sync.Once)
		dt.initCompletionOnce.Do(func() {
			atomic.StoreInt32(&dt.isInitializing, 0)
			close(dt.initCompletionChecker)
		})
	} else {
		klog.V(4).Infof("Engine.checkInitializationComplete: still initializing (pendingOps=%d, inFlightTriggers=%d)",
			pendingOps, inFlightTriggers)
	}
}
