package difftracker

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// triggerLocationsUpdater sends a non-blocking trigger to the LocationsUpdater.
func (dt *DiffTracker) triggerLocationsUpdater() {
	// Track triggers during initialization (check WITHOUT lock - use atomic read)
	// This function is called from contexts where dt.mu is already held,
	// so we can't acquire it again (even with recursive mutex, it's unnecessary)
	shouldTrack := atomic.LoadInt32(&dt.isInitializing) == 1

	// Increment the in-flight counter BEFORE the send so the trigger token never becomes
	// observable to the consumer ahead of the increment. Otherwise the consumer could
	// receive the token, run its decrement + checkInitializationComplete, and observe a
	// transient negative counter before this increment lands — skipping completion and
	// hanging WaitForInitialSync forever. On a coalesced (channel-full) send we undo the
	// increment, since the already-buffered token will drive exactly one decrement.
	if shouldTrack {
		atomic.AddInt32(&dt.pendingUpdaterTriggers, 1)
	}
	select {
	case dt.locationsUpdaterTrigger <- true:
		// Trigger sent; the matching decrement happens in LocationsUpdater.process.
	default:
		// Channel full, trigger coalesced into the pending one - undo the increment.
		if shouldTrack {
			atomic.AddInt32(&dt.pendingUpdaterTriggers, -1)
		}
	}
}

// triggerServiceUpdater sends a non-blocking trigger to the ServiceUpdater.
func (dt *DiffTracker) triggerServiceUpdater() {
	// Track triggers during initialization (check WITHOUT lock - use atomic read)
	// This function is called from contexts where dt.mu is already held,
	// so we can't acquire it again (even with recursive mutex, it's unnecessary)
	shouldTrack := atomic.LoadInt32(&dt.isInitializing) == 1

	// Increment the in-flight counter BEFORE the send (see triggerLocationsUpdater for the
	// full rationale): the trigger token must not become observable to the consumer ahead
	// of the increment, or the consumer's decrement + checkInitializationComplete could see
	// a transient negative counter, skip completion, and hang WaitForInitialSync forever.
	// On a coalesced (channel-full) send we undo the increment.
	if shouldTrack {
		atomic.AddInt32(&dt.pendingUpdaterTriggers, 1)
	}
	select {
	case dt.serviceUpdaterTrigger <- true:
		dt.logger.V(5).Info("Sent service updater trigger")
	default:
		// Channel full, trigger coalesced into the pending one - undo the increment.
		if shouldTrack {
			atomic.AddInt32(&dt.pendingUpdaterTriggers, -1)
		}
		dt.logger.V(4).Info("Dropped service updater trigger because channel is full")
	}
}

// ReconcileInboundService validates and translates a Kubernetes LoadBalancer Service into the
// desired configuration owned by the ServiceGateway engine.
func (dt *DiffTracker) ReconcileInboundService(service *v1.Service) error {
	if service == nil {
		return fmt.Errorf("cannot reconcile a nil inbound Service")
	}

	serviceUID := ServiceUID(service)
	if serviceUID == "" {
		return fmt.Errorf("cannot reconcile inbound Service %s/%s without a UID", service.Namespace, service.Name)
	}

	dt.logger.V(2).Info("Reconciling inbound Service", "serviceUID", serviceUID)

	if service.Annotations[consts.ServiceAnnotationLoadBalancerInternal] == consts.TrueAnnotationValue {
		return &InboundConfigValidationError{
			Reason:  "UnsupportedInternalLoadBalancer",
			Message: fmt.Sprintf("internal load balancer is not supported when ServiceGateway is enabled; remove the %q annotation", consts.ServiceAnnotationLoadBalancerInternal),
		}
	}

	inboundConfig := ExtractInboundConfigFromService(service)
	if inboundConfig == nil {
		return nil
	}
	if err := ValidateInboundConfig(inboundConfig); err != nil {
		return err
	}

	config := NewInboundServiceConfig(serviceUID, inboundConfig)
	config.Namespace = service.Namespace
	config.Name = service.Name

	if dt.IsServiceTracked(serviceUID) {
		dt.UpdateService(config)
	} else {
		dt.AddService(config)
	}
	return nil
}

// DeleteInboundService translates a Kubernetes LoadBalancer Service deletion into the inbound
// deletion request understood by the ServiceGateway engine.
func (dt *DiffTracker) DeleteInboundService(service *v1.Service) error {
	if service == nil {
		return fmt.Errorf("cannot delete a nil inbound Service")
	}

	serviceUID := ServiceUID(service)
	if serviceUID == "" {
		return fmt.Errorf("cannot delete inbound Service %s/%s without a UID", service.Namespace, service.Name)
	}

	dt.logger.V(2).Info("Deleting inbound Service", "serviceUID", serviceUID)
	dt.DeleteService(serviceUID, true, false)
	return nil
}

// AddService handles service creation events for inbound (Load Balancer) services.
// If the service already exists in NRP, it does nothing (idempotent).
// If the service doesn't exist, it triggers service creation via XUpdater.
func (dt *DiffTracker) AddService(config ServiceConfig) {
	defer func() {
		updatePendingServiceOperationsMetric(dt)
		updateTrackedServicesMetric(dt)
		updatePendingOperationOldestAgeMetric(dt)
	}()

	dt.mu.Lock()

	// Validate configuration
	if err := config.Validate(); err != nil {
		dt.mu.Unlock()
		dt.logger.V(4).Info("Could not add service with invalid config", "err", err)
		return
	}

	serviceUID := config.UID
	dt.logger.V(5).Info("Added service request", "service", serviceUID, "isInbound", config.IsInbound)

	// Check if service already exists in NRP
	if config.IsInbound {
		if dt.NRPResources.LoadBalancers.Has(serviceUID) {
			dt.mu.Unlock()
			dt.logger.V(5).Info("Skipped existing LoadBalancer", "service", serviceUID)
			return
		}
	} else {
		if dt.NRPResources.NATGateways.Has(serviceUID) {
			dt.mu.Unlock()
			dt.logger.V(5).Info("Skipped existing NATGateway", "service", serviceUID)
			return
		}
	}

	// Check if service operation is already tracked
	opState, exists := dt.pendingServiceOps[serviceUID]
	if exists {
		state := opState.State
		dt.mu.Unlock()
		dt.logger.V(5).Info("Skipped tracked service", "service", serviceUID, "state", state)
		return
	}

	// Service doesn't exist - need to create it
	dt.logger.V(5).Info("Triggered service creation", "service", serviceUID)

	// Add service operation to pending list
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID:    serviceUID,
		Config:        config,
		State:         StateNotStarted,
		RetryCount:    0,
		LastAttempt:   time.Now().Format(time.RFC3339),
		CreatedAt:     time.Now(),
		CorrelationID: uuid.NewString(),
	}

	// Release lock before triggering to avoid lock contention
	dt.mu.Unlock()

	if config.IsInbound {
		dt.seedInboundEndpointsFromCache(serviceUID)
	}
	dt.triggerServiceUpdater()
}

// UpdateEndpoints handles endpoint updates for inbound (Load Balancer) services.
// If the service is already created in NRP, endpoints are immediately updated.
// If the service is being created, endpoints are buffered until creation completes.
// If the service doesn't exist, this shouldn't happen (AddService should be called first).
func (dt *DiffTracker) UpdateEndpoints(serviceUID string, oldPodIPToNodeIP, newPodIPToNodeIP map[string]string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	if serviceUID == "" {
		dt.logger.V(4).Info("Could not update endpoints without service")
		return
	}

	dt.logger.V(5).Info("Updated endpoints request", "service", serviceUID, "oldCount", len(oldPodIPToNodeIP), "newCount", len(newPodIPToNodeIP))

	// Check if service operation is tracked
	opState, exists := dt.pendingServiceOps[serviceUID]

	if !exists {
		// Check if service exists in NRP (created outside Engine)
		if dt.NRPResources.LoadBalancers.Has(serviceUID) {
			dt.logger.V(5).Info("Updated endpoints for existing service", "service", serviceUID)
			errs := dt.updateK8sEndpointsLocked(UpdateK8sEndpointsInputType{
				InboundIdentity: serviceUID,
				OldAddresses:    oldPodIPToNodeIP,
				NewAddresses:    newPodIPToNodeIP,
			})
			if len(errs) > 0 {
				dt.logger.V(4).Info("Could not update endpoints", "err", errs, "service", serviceUID)
				// Still trigger LocationsUpdater even if some endpoints failed
			}
			// Trigger LocationsUpdater to sync the changes
			dt.triggerLocationsUpdater()
			return
		}

		// Untracked and absent from NRP: in ServiceGateway mode the informer fires for every Service
		// (ClusterIP, headless, or a not-yet-created LoadBalancer), so buffering would grow
		// pendingEndpoints unbounded. Drop it; AddService re-seeds from the EndpointSlice cache.
		dt.logger.V(5).Info("Dropped endpoints for untracked service", "service", serviceUID)
		return
	}

	// Service operation exists - check state
	switch opState.State {
	case StateNotStarted, StateCreationInProgress:
		// A terminal UPDATE failure parks a live service in StateNotStarted while its LB stays in NRP
		// and keeps serving. Its backend pool must keep tracking endpoint changes, so when the LB
		// already exists in NRP apply the update immediately rather than buffering it (which would let
		// the live pool go stale until a spec change un-parks the op). A not-yet-created service, or a
		// recreate-after-deletion whose LB was already torn down, has no LB in NRP and buffers below.
		if opState.State == StateNotStarted && dt.NRPResources.LoadBalancers != nil && dt.NRPResources.LoadBalancers.Has(serviceUID) {
			dt.logger.V(5).Info("Applied endpoints to live LB parked after terminal update", "service", serviceUID, "oldCount", len(oldPodIPToNodeIP), "newCount", len(newPodIPToNodeIP))
			errs := dt.updateK8sEndpointsLocked(UpdateK8sEndpointsInputType{
				InboundIdentity: serviceUID,
				OldAddresses:    oldPodIPToNodeIP,
				NewAddresses:    newPodIPToNodeIP,
			})
			if len(errs) > 0 {
				dt.logger.V(4).Info("Could not update endpoints", "err", errs, "service", serviceUID)
			}
			dt.triggerLocationsUpdater()
			return
		}
		// Service is being created or waiting to be created - buffer the endpoints.
		// Store both old and new so the intervening removals are replayed on promotion
		// (otherwise an add-then-remove during creation would leak the removed IP).
		dt.logger.V(5).Info("Buffered endpoints while service is being created", "service", serviceUID, "state", opState.State, "count", len(newPodIPToNodeIP))
		dt.pendingEndpoints[serviceUID] = append(dt.pendingEndpoints[serviceUID], PendingEndpointUpdate{
			OldPodIPToNodeIP: oldPodIPToNodeIP,
			PodIPToNodeIP:    newPodIPToNodeIP,
			Timestamp:        time.Now().Format(time.RFC3339),
		})

	case StateCreated, StateUpdateInProgress:
		// Service is ready - update endpoints immediately. During an in-flight LB update
		// (port change) the LB and SGW Service entry are stable; pod-IP endpoint sync
		// must continue without interruption.
		dt.logger.V(5).Info("Updated endpoints for ready service", "service", serviceUID, "state", opState.State, "oldCount", len(oldPodIPToNodeIP), "newCount", len(newPodIPToNodeIP))
		errs := dt.updateK8sEndpointsLocked(UpdateK8sEndpointsInputType{
			InboundIdentity: serviceUID,
			OldAddresses:    oldPodIPToNodeIP,
			NewAddresses:    newPodIPToNodeIP,
		})
		if len(errs) > 0 {
			dt.logger.V(4).Info("Could not update endpoints", "err", errs, "service", serviceUID)
			// Still trigger LocationsUpdater even if some endpoints failed
		}
		// Trigger LocationsUpdater to sync the changes
		dt.triggerLocationsUpdater()

	case StateDeletionPending:
		if opState.RecreateAfterDeletion {
			// A recreate is queued (the Service toggled ClusterIP->LoadBalancer while the delete
			// was in flight). DeleteService already wiped the endpoint state, so buffer these
			// endpoints (both old and new, like the creation-in-progress path) and let
			// promotePendingEndpointsLocked replay them when the service is re-created. Without
			// this the recreated LB comes up with an empty backend pool until the next
			// EndpointSlice event.
			dt.logger.V(5).Info("Buffered endpoints for service pending recreate-after-deletion", "service", serviceUID, "count", len(newPodIPToNodeIP))
			dt.pendingEndpoints[serviceUID] = append(dt.pendingEndpoints[serviceUID], PendingEndpointUpdate{
				OldPodIPToNodeIP: oldPodIPToNodeIP,
				PodIPToNodeIP:    newPodIPToNodeIP,
				Timestamp:        time.Now().Format(time.RFC3339),
			})
			return
		}
		// Service is pending deletion - process removals only. NewAddresses is dropped so
		// a service being torn down cannot re-insert pod refs that delete-success never scrubs.
		dt.logger.V(5).Info("Processed endpoint removals for service pending deletion", "service", serviceUID, "oldCount", len(oldPodIPToNodeIP), "ignoredNewCount", len(newPodIPToNodeIP))
		errs := dt.updateK8sEndpointsLocked(UpdateK8sEndpointsInputType{
			InboundIdentity: serviceUID,
			OldAddresses:    oldPodIPToNodeIP,
			NewAddresses:    nil,
		})
		if len(errs) > 0 {
			dt.logger.V(4).Info("Could not update endpoints", "err", errs, "service", serviceUID)
		}
		dt.triggerLocationsUpdater()

	case StateDeletionInProgress:
		if opState.RecreateAfterDeletion {
			// As above, but the delete is already dispatched. Still buffer for the queued recreate
			// (replayed by promotePendingEndpointsLocked on the post-deletion create).
			dt.logger.V(5).Info("Buffered endpoints while service deletion is in progress (recreate queued)", "service", serviceUID, "count", len(newPodIPToNodeIP))
			dt.pendingEndpoints[serviceUID] = append(dt.pendingEndpoints[serviceUID], PendingEndpointUpdate{
				OldPodIPToNodeIP: oldPodIPToNodeIP,
				PodIPToNodeIP:    newPodIPToNodeIP,
				Timestamp:        time.Now().Format(time.RFC3339),
			})
			return
		}
		// Service deletion already in progress - ignore endpoint updates
		dt.logger.V(5).Info("Ignored endpoint update while service deletion is in progress", "service", serviceUID)

	default:
		dt.logger.V(4).Info("Found unknown service operation state while updating endpoints", "state", opState.State, "service", serviceUID)
	}
}

// UpdateService handles spec updates (e.g., port changes) for an existing inbound service.
// Behavior:
//   - If the service is not yet tracked AND not in NRP: falls through to AddService.
//   - If currently being created (StateNotStarted/CreationInProgress): the latest config
//     overwrites the desired config; the in-flight creation will use the newer config when
//     it picks up the work. (For an already-running creation goroutine, a follow-up update
//     will be enqueued via StateUpdateInProgress on creation success.)
//   - If StateCreated: diff the new config against LastAppliedConfig. Equal => no-op.
//     Different => transition to StateUpdateInProgress and trigger the ServiceUpdater.
//   - If StateUpdateInProgress: overwrite Config so the next dispatch sees the latest desired
//     state; an in-flight updater will re-PUT with the freshest config on completion if needed.
//   - If StateDeletionPending/InProgress: ignore — deletion wins.
//
// Outbound services are not currently supported by this path.
// resetRetryStateLocked clears a transient-failure park so a fresh external intent (a spec-changing
// UpdateService, or a DeleteService) gets a clean retry budget instead of inheriting an exhausted or
// still-backing-off one. Without this, a service parked after maxServiceRetries transient failures
// would stay stranded until the CCM process restarts. Must be called with dt.mu held.
func resetRetryStateLocked(op *ServiceOperationState) {
	op.RetryCount = 0
	op.RetriesExhausted = false
	op.NextRetryAt = time.Time{}
}

func (dt *DiffTracker) UpdateService(config ServiceConfig) {
	defer func() {
		updatePendingServiceOperationsMetric(dt)
		updateTrackedServicesMetric(dt)
		updatePendingOperationOldestAgeMetric(dt)
	}()

	if err := config.Validate(); err != nil {
		dt.logger.V(4).Info("Could not update service with invalid config", "err", err)
		return
	}

	if !config.IsInbound {
		dt.logger.V(5).Info("Ignored unsupported outbound service update", "service", config.UID)
		return
	}

	serviceUID := config.UID

	dt.mu.Lock()

	opState, exists := dt.pendingServiceOps[serviceUID]
	existsInNRP := dt.NRPResources.LoadBalancers.Has(serviceUID)

	if !exists && !existsInNRP {
		// Service is unknown to the engine - treat as a creation.
		dt.mu.Unlock()
		dt.logger.V(5).Info("Delegated untracked service update to add", "service", serviceUID)
		dt.AddService(config)
		return
	}

	if !exists && existsInNRP {
		// LB exists in NRP (e.g., recovered after CCM restart) but no engine tracking entry.
		// Create one in StateCreated so the update path can take over.
		dt.logger.V(5).Info("Created tracking entry for existing service update", "service", serviceUID)
		opState = &ServiceOperationState{
			ServiceUID:    serviceUID,
			Config:        config,
			State:         StateCreated,
			RetryCount:    0,
			LastAttempt:   time.Now().Format(time.RFC3339),
			CreatedAt:     time.Now(),
			CorrelationID: uuid.NewString(),
		}
		dt.pendingServiceOps[serviceUID] = opState
		// Force an update PUT (we have no LastAppliedConfig to compare against).
		opState.State = StateUpdateInProgress
		opState.Config = config
		dt.mu.Unlock()
		dt.triggerServiceUpdater()
		return
	}

	// opState exists - dispatch on current state.
	switch opState.State {
	case StateNotStarted, StateCreationInProgress:
		// Creation hasn't reached Azure yet (or is in flight). Latest config wins; the
		// running goroutine already captured a snapshot at dispatch time, so we also
		// queue a follow-up update by leaving the freshly-overwritten Config; if creation
		// completes with stale data, OnServiceCreationComplete will see Config != LastAppliedConfig
		// and schedule an UpdateInProgress.
		if opState.CreationFailedTerminal {
			// Service was parked after a non-retryable creation error. Only re-attempt if
			// the spec actually changed; a resync with the same invalid spec stays parked.
			if configsEqualForUpdate(&opState.Config, &config) {
				dt.mu.Unlock()
				dt.logger.V(5).Info("Skipped parked service with unchanged spec", "service", serviceUID)
				return
			}
			dt.logger.V(5).Info("Reattempted service creation after terminal failure and spec change", "service", serviceUID)
			opState.Config = config
			opState.CreationFailedTerminal = false
			resetRetryStateLocked(opState)
			opState.State = StateNotStarted
			opState.LastAttempt = time.Now().Format(time.RFC3339)
			dt.mu.Unlock()
			dt.triggerServiceUpdater()
			return
		}
		dt.logger.V(5).Info("Overwrote desired service config", "service", serviceUID, "state", opState.State)
		recoverPark := false
		if !configsEqualForUpdate(&opState.Config, &config) {
			// A genuine spec change is fresh intent: clear any transient-failure park so the new
			// config starts with a clean retry budget rather than inheriting an exhausted one.
			recoverPark = opState.RetriesExhausted
			resetRetryStateLocked(opState)
		} else if opState.RetriesExhausted && time.Now().After(opState.NextRetryAt) {
			// Same spec, but the cooldown since the op parked has elapsed: the transient outage is
			// likely over, so re-arm it. Without this a stable Service whose create exhausted its
			// budget would never get its load balancer or public IP until the CCM restarts. The
			// cooldown bounds this to one retry burst per parkReArmCooldown, not a per-resync storm.
			recoverPark = true
			resetRetryStateLocked(opState)
		}
		opState.Config = config
		dt.mu.Unlock()
		if recoverPark {
			// The op had stopped dispatching after exhausting its budget; nudge the updater so the
			// recovered op is picked up promptly instead of waiting for an unrelated trigger.
			dt.triggerServiceUpdater()
		}

	case StateCreated:
		if opState.LastAppliedConfig != nil &&
			opState.LastAppliedConfig.IsInbound == config.IsInbound &&
			opState.LastAppliedConfig.InboundConfig.Equals(config.InboundConfig) {
			dt.mu.Unlock()
			dt.logger.V(5).Info("Skipped unchanged service config", "service", serviceUID)
			return
		}
		dt.logger.V(5).Info("Scheduled service update", "service", serviceUID)
		opState.Config = config
		opState.State = StateUpdateInProgress
		resetRetryStateLocked(opState)
		opState.LastAttempt = time.Now().Format(time.RFC3339)
		dt.mu.Unlock()
		dt.triggerServiceUpdater()

	case StateUpdateInProgress:
		// An updater is (or will be) processing this service. Overwrite with the latest desired
		// config. A live in-flight worker re-checks Config on completion via OnServiceCreationComplete
		// and reschedules if it changed; a parked op (retries exhausted, no worker) has nothing to
		// reschedule it, so it is re-armed here on a spec change or once the park cooldown has elapsed.
		dt.logger.V(5).Info("Overwrote desired config while service is updating", "service", serviceUID)
		recoverPark := false
		if !configsEqualForUpdate(&opState.Config, &config) {
			recoverPark = opState.RetriesExhausted
			resetRetryStateLocked(opState)
		} else if opState.RetriesExhausted && time.Now().After(opState.NextRetryAt) {
			// Same spec, but the cooldown since the op parked has elapsed: re-arm so a stable Service
			// applies the pending update instead of serving stale config until the CCM restarts.
			recoverPark = true
			resetRetryStateLocked(opState)
		}
		opState.Config = config
		dt.mu.Unlock()
		if recoverPark {
			// The parked op has no worker to pick up the latest config; nudge the updater so the
			// recovered op dispatches instead of being silently dropped.
			dt.triggerServiceUpdater()
		}

	case StateDeletionPending, StateDeletionInProgress:
		// A re-create (e.g. a LoadBalancer->ClusterIP->LoadBalancer toggle) arrived while
		// the service is being deleted. Record the desired config and let the
		// deletion-success path replay it as a fresh create, so it cannot race the delete.
		opState.Config = config
		opState.RecreateAfterDeletion = true
		dt.mu.Unlock()
		dt.seedInboundEndpointsFromCache(serviceUID)
		dt.logger.V(5).Info("Buffered service recreate intent during deletion", "service", serviceUID)

	default:
		state := opState.State
		dt.mu.Unlock()
		dt.logger.V(4).Info("Found unknown service operation state while updating service", "state", state, "service", serviceUID)
	}
}

// DeleteService handles service deletion events for inbound (Load Balancer) services.
// It marks the service for deletion and triggers DeletionChecker to verify locations are cleared.
// DeleteService schedules a service for deletion. If isOrphan is true, the service is an orphaned
// Azure resource (exists in Azure but not in ServiceGateway) and we skip the NRP existence check.
func (dt *DiffTracker) DeleteService(serviceUID string, isInbound bool, isOrphan bool) {
	defer func() {
		updatePendingServiceOperationsMetric(dt)
		updatePendingServiceDeletionsMetric(dt)
		updatePendingOperationOldestAgeMetric(dt)
		updateTrackedServicesMetric(dt)
	}()

	dt.mu.Lock()

	if serviceUID == "" {
		dt.mu.Unlock()
		dt.logger.V(4).Info("Could not delete service without service")
		return
	}

	dt.logger.V(5).Info("Deleted service request", "service", serviceUID, "isInbound", isInbound, "isOrphan", isOrphan)

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
				dt.logger.V(5).Info("Skipped missing service deletion", "service", serviceUID)
				return
			}
		}

		// Service exists in NRP (or is orphan) but not tracked - create tracking entry
		if isOrphan {
			dt.logger.V(5).Info("Marked orphaned service for deletion", "service", serviceUID)
		} else {
			dt.logger.V(5).Info("Marked existing service for deletion", "service", serviceUID)
		}
		var config ServiceConfig
		if isInbound {
			config = NewInboundServiceConfig(serviceUID, nil)
		} else {
			config = NewOutboundServiceConfig(serviceUID, nil)
		}
		dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
			ServiceUID:    serviceUID,
			Config:        config,
			State:         StateDeletionPending,
			RetryCount:    0,
			LastAttempt:   time.Now().Format(time.RFC3339),
			CreatedAt:     time.Now(),
			CorrelationID: uuid.NewString(),
			IsOrphan:      isOrphan,
		}
	} else {
		// Service is tracked - update state based on current state
		switch opState.State {
		case StateNotStarted:
			dt.logger.V(5).Info("Marked not-started service for deletion", "service", serviceUID)
			opState.State = StateDeletionPending

		case StateCreationInProgress:
			dt.logger.V(5).Info("Marked service for deletion while creation is in progress", "service", serviceUID)
			opState.State = StateDeletionPending

		case StateCreated:
			dt.logger.V(5).Info("Marked ready service for deletion", "service", serviceUID)
			opState.State = StateDeletionPending

		case StateUpdateInProgress:
			// An update is in flight; deletion wins. Preserve InFlightConfig so the
			// OnServiceCreationComplete pre-empt can recognize the in-flight update's
			// completion and route it to deletion (it clears InFlightConfig itself).
			dt.logger.V(5).Info("Marked service for deletion while update is in progress", "service", serviceUID)
			opState.State = StateDeletionPending

		case StateDeletionPending, StateDeletionInProgress:
			// A fresh delete supersedes any recreate intent buffered by an interleaved UpdateService
			// (a LoadBalancer->ClusterIP flap); otherwise the deletion-success path would resurrect a
			// service meant to be deleted, leaking its LB, public IP and ServiceGateway registration.
			opState.RecreateAfterDeletion = false
			if opState.RetriesExhausted {
				// A repeated delete is fresh intent: re-arm a delete that exhausted its retry budget
				// so it dispatches again instead of leaking the Azure load balancer and public IP and
				// leaving the Service stuck Terminating until the CCM restarts.
				resetRetryStateLocked(opState)
				dt.mu.Unlock()
				dt.triggerServiceUpdater()
			} else {
				dt.mu.Unlock()
				dt.logger.V(5).Info("Skipped service already being deleted", "service", serviceUID)
			}
			return

		default:
			state := opState.State
			dt.mu.Unlock()
			dt.logger.V(4).Info("Found unknown service operation state while deleting service", "state", state, "service", serviceUID)
			return
		}

		// A delete is fresh external intent: clear any transient-failure park so the deletion gets
		// a clean retry budget instead of inheriting an exhausted create budget. Otherwise a service
		// parked after repeated create failures would never run its delete, leaking the Azure load
		// balancer and public IP and leaving the Service stuck Terminating until the CCM restarts.
		resetRetryStateLocked(opState)
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
		dt.logger.V(5).Info("Marked service ready for immediate deletion", "service", serviceUID)
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
	defer func() {
		updatePendingServiceOperationsMetric(dt)
		updatePendingOperationOldestAgeMetric(dt)
		// The NRP LoadBalancer/NATGateway tracked sets are mutated (via UpdateNRPLoadBalancers/
		// UpdateNRPNATGateways) before this completion callback, so refresh the tracked_services gauge
		// here too - otherwise it stays stale after an async create/delete until the next Add/Update.
		updateTrackedServicesMetric(dt)
	}()

	dt.mu.Lock()
	defer dt.mu.Unlock()

	opState, exists := dt.pendingServiceOps[serviceUID]
	if !exists {
		dt.logger.V(4).Info("Could not complete service operation because pending operation was not found", "service", serviceUID)
		return
	}

	// Measure latency from when the operation was dispatched (in processBatch), not from
	// this callback which runs after the Azure work is done. Fall back to now if unset.
	startTime := opState.OperationStartedAt
	if startTime.IsZero() {
		startTime = time.Now()
	}

	// PRE-EMPT: if a Delete arrived during the in-flight create/update, DeleteService
	// changed the state to StateDeletionPending (service still had NRP locations) or
	// jumped straight to StateDeletionInProgress (no locations yet — the common case for
	// a service deleted mid-create). In BOTH cases the operation that just completed is
	// the in-flight CREATE/UPDATE (InFlightConfig != nil), NOT a delete: the LB/PIP/SGW
	// may have been created and must be cleaned up. Route to the deletion flow instead of
	// letting a create/update success fall through and be misread as a delete-success
	// (which would wipe tracking without ever dispatching deleteInboundService → Azure
	// LB/PIP/SGW leak + Service stuck Terminating). A genuine delete completion has
	// InFlightConfig == nil and is handled by the isDeletion branch below.
	if opState.State == StateDeletionPending ||
		(opState.State == StateDeletionInProgress && opState.InFlightConfig != nil) {
		dt.logger.V(4).Info("Routed completed in-flight service operation to deletion", "service", serviceUID, "success", success)
		opState.InFlightConfig = nil
		// The in-flight create/update just completed (success or err) before being routed to deletion;
		// record it once so the completion is counted like every other branch (the delete itself is
		// recorded separately when it completes). Infer the op type from LastAppliedConfig: a service
		// that was never successfully applied was mid-CREATE, otherwise mid-UPDATE.
		preemptOp := "create"
		if opState.LastAppliedConfig != nil {
			preemptOp = "update"
		}
		preemptErrCode := ""
		if err != nil {
			_, preemptErrCode = extractAzureErrorInfo(err)
		}
		recordServiceOperation(preemptOp, opState.Config.IsInbound, startTime, err, preemptErrCode, opState.IsOrphan)
		hasLocations := dt.serviceHasLocationsInNRP(serviceUID)
		if !hasLocations {
			// Ready for immediate deletion.
			opState.State = StateDeletionInProgress
			delete(dt.pendingServiceDeletions, serviceUID)
			dt.triggerServiceUpdater()
		} else {
			// Locations still present; LocationsUpdater will clear them and
			// CheckPendingServiceDeletions will transition opState.State.
			//
			// The pendingServiceDeletions entry is NOT guaranteed to still exist here: when the
			// Delete arrived, DeleteService gated on serviceHasLocationsInNRP — if that was
			// momentarily false (no locations yet) it took the fast path, jumped this op straight
			// to StateDeletionInProgress, and DELETED the pendingServiceDeletions entry. An
			// in-flight endpoint sync (a port-change UpdateEndpoints applies immediately for an
			// in-flight update) can then re-publish the service's pod address to NRP just after
			// that gate, so hasLocations is true again now. Without re-adding the entry,
			// CheckPendingServiceDeletions would have nothing to advance once the locations drain
			// and the op would strand in StateDeletionPending forever (Azure LB/PIP/SGW + finalizer
			// leaked, Service stuck Terminating). Re-add it idempotently.
			opState.State = StateDeletionPending
			if _, ok := dt.pendingServiceDeletions[serviceUID]; !ok {
				dt.pendingServiceDeletions[serviceUID] = &PendingServiceDeletion{
					ServiceUID: serviceUID,
					IsInbound:  opState.Config.IsInbound,
					Timestamp:  time.Now().Format(time.RFC3339),
				}
			}
			dt.triggerLocationsUpdater()
		}
		return
	}

	// Determine if this is creation, update, or deletion based on current state
	isDeletion := (opState.State == StateDeletionInProgress)
	isUpdate := (opState.State == StateUpdateInProgress)

	if isUpdate {
		if success {
			dt.logger.V(2).Info("Updated service", "service", serviceUID)
			recordServiceOperation("update", opState.Config.IsInbound, startTime, nil, "", opState.IsOrphan)
			// Persist the config that was actually applied (snapshot at dispatch time).
			if opState.InFlightConfig != nil {
				applied := *opState.InFlightConfig
				opState.LastAppliedConfig = &applied
			}
			opState.RetryCount = 0

			// If the desired Config drifted while the update was in flight, reschedule.
			if opState.InFlightConfig != nil && !configsEqualForUpdate(opState.InFlightConfig, &opState.Config) {
				dt.logger.V(5).Info("Rescheduled service update after desired config drifted", "service", serviceUID)
				opState.State = StateUpdateInProgress
				opState.LastAttempt = time.Now().Format(time.RFC3339)
				opState.InFlightConfig = nil
				dt.triggerServiceUpdater()
			} else {
				opState.State = StateCreated
				opState.InFlightConfig = nil
			}
			dt.checkInitializationCompleteLocked()
		} else {
			dt.logger.V(4).Info("Could not update service", "err", err, "service", serviceUID)
			// Capture the attempted config before clearing it, so the terminal branch can detect a
			// desired-spec drift that landed while this update was in flight.
			attempted := opState.InFlightConfig
			opState.InFlightConfig = nil

			if isTerminalError(err) {
				recordServiceOperation("update", opState.Config.IsInbound, startTime, err, "ValidationError", opState.IsOrphan)
				if attempted != nil && !configsEqualForUpdate(attempted, &opState.Config) {
					// The desired config changed while this failing update was in flight; re-dispatch
					// the new desired config (which may be valid) rather than parking on the replaced spec.
					resetRetryStateLocked(opState)
					opState.State = StateNotStarted
					opState.LastAttempt = time.Now().Format(time.RFC3339)
					dt.logger.V(5).Info("Re-dispatched service update after desired config drifted during a terminal failure", "service", serviceUID)
					dt.triggerServiceUpdater()
					dt.checkInitializationCompleteLocked()
				} else {
					// Deterministic, spec-driven update failure (e.g. unsupported protocol, port
					// out of range, dual-stack). Retrying cannot succeed, so park the service
					// instead of looping forever. Its existing Azure resources keep the
					// last-applied config; a later UpdateService with a changed spec clears the park.
					opState.CreationFailedTerminal = true
					opState.State = StateNotStarted
					opState.LastAttempt = time.Now().Format(time.RFC3339)
					dt.logger.V(4).Info("Parked service after non-retryable update error", "err", err, "service", serviceUID)
					dt.checkInitializationCompleteLocked()
				}
			} else {
				_, errCode := extractAzureErrorInfo(err)
				recordServiceOperation("update", opState.Config.IsInbound, startTime, err, errCode, opState.IsOrphan)
				opState.RetryCount++
				opState.LastAttempt = time.Now().Format(time.RFC3339)
				opState.NextRetryAt = time.Now().Add(computeRetryBackoff(opState.RetryCount))
				recordServiceOperationRetry("update", opState.Config.IsInbound, opState.RetryCount)

				dt.logger.V(4).Info("Scheduled service update retry", "service", serviceUID, "attempt", opState.RetryCount, "nextRetryAt", opState.NextRetryAt)
				// Stay in StateUpdateInProgress so dispatcher picks it up again.
				dt.triggerServiceUpdater()
			}
		}
		return
	}

	if isDeletion {
		// Handle deletion completion
		if success {
			dt.logger.V(2).Info("Deleted service", "service", serviceUID)
			recordServiceOperation("delete", opState.Config.IsInbound, startTime, nil, "", opState.IsOrphan)
			if opState.IsOrphan {
				// Count the orphan only once its async deletion actually succeeded (not at schedule
				// time), so a failed cleanup does not over-report orphaned_resources_cleaned_total.
				recordOrphanedResourceCleaned()
			}

			// If pods arrived while the deletion was in flight (buffered by the
			// StateDeletionInProgress branch of AddPod), or a re-create was requested
			// during deletion, the service must be re-created rather than torn down —
			// otherwise live pods are stranded or the LB silently disappears. The Azure
			// delete is now complete, so a fresh create cannot race it; buffered pods are
			// promoted in the create-success path.
			if opState.RecreateAfterDeletion || len(dt.pendingPods[serviceUID]) > 0 {
				dt.logger.V(5).Info("Recreated service after deletion", "service", serviceUID, "recreateAfterDeletion", opState.RecreateAfterDeletion, "bufferedPods", len(dt.pendingPods[serviceUID]))
				opState.State = StateNotStarted
				opState.RetryCount = 0
				// A post-deletion recreate is a fresh start: also clear the terminal/exhausted-retry
				// parks, else a service parked before the delete never dispatches (LB stranded until restart).
				opState.CreationFailedTerminal = false
				opState.RetriesExhausted = false
				opState.NextRetryAt = time.Time{}
				opState.InFlightConfig = nil
				opState.LastAppliedConfig = nil
				opState.RecreateAfterDeletion = false
				opState.LastAttempt = time.Now().Format(time.RFC3339)
				delete(dt.pendingServiceDeletions, serviceUID)
				dt.triggerServiceUpdater()
				return
			}

			// Clean up all state
			delete(dt.pendingServiceOps, serviceUID)
			delete(dt.pendingEndpoints, serviceUID)
			delete(dt.pendingPods, serviceUID)
			delete(dt.pendingServiceDeletions, serviceUID)

			// Check if initialization is complete after service deletion
			dt.checkInitializationCompleteLocked()
		} else {
			dt.logger.V(4).Info("Could not delete service", "err", err, "service", serviceUID)

			// A ServiceWithOverlayMappingsCannotBeDeleted rejection means NRP still has the
			// service's pod overlay address mappings. This happens in a race: an in-flight
			// endpoint sync pushes addresses to NRP just after DeleteService gated on
			// serviceHasLocationsInNRP (which was momentarily false) and jumped straight to
			// the unregister. Retrying the unregister directly cannot help and storms NRP
			// (the orphaned addresses never get drained). Instead, re-gate the deletion
			// behind a fresh locations drain: clear the service from K8s state, mark it
			// pending-on-locations, and trigger the LocationsUpdater. Its sync removes the
			// orphaned NRP addresses, then CheckPendingServiceDeletions retriggers the
			// delete once the overlay mappings are actually gone.
			if isServiceOverlayMappingsError(err) {
				_, errCode := extractAzureErrorInfo(err)
				recordServiceOperation("delete", opState.Config.IsInbound, startTime, err, errCode, opState.IsOrphan)
				opState.RetryCount++
				opState.LastAttempt = time.Now().Format(time.RFC3339)
				// Back off like the other retry paths: the re-drain re-dispatches deletion through
				// CheckPendingServiceDeletions -> retryGate, which honours NextRetryAt. Without it a
				// persistent overlay rejection tight-loops the drain/delete cycle and storms NRP.
				opState.NextRetryAt = time.Now().Add(computeRetryBackoff(opState.RetryCount))
				recordServiceOperationRetry("delete", opState.Config.IsInbound, opState.RetryCount)

				opState.State = StateDeletionPending
				dt.removeServiceFromK8sStateLocked(serviceUID, opState.Config.IsInbound)
				dt.pendingServiceDeletions[serviceUID] = &PendingServiceDeletion{
					ServiceUID: serviceUID,
					IsInbound:  opState.Config.IsInbound,
					Timestamp:  time.Now().Format(time.RFC3339),
				}
				dt.logger.V(4).Info("Re-draining locations before retrying service deletion blocked by overlay mappings", "service", serviceUID, "attempt", opState.RetryCount)
				dt.triggerLocationsUpdater()
				return
			}

			_, errCode := extractAzureErrorInfo(err)
			recordServiceOperation("delete", opState.Config.IsInbound, startTime, err, errCode, opState.IsOrphan)
			opState.RetryCount++
			opState.LastAttempt = time.Now().Format(time.RFC3339)
			opState.NextRetryAt = time.Now().Add(computeRetryBackoff(opState.RetryCount))
			recordServiceOperationRetry("delete", opState.Config.IsInbound, opState.RetryCount)

			dt.logger.V(4).Info("Scheduled service deletion retry", "service", serviceUID, "attempt", opState.RetryCount, "nextRetryAt", opState.NextRetryAt)
			// Trigger ServiceUpdater for retry
			dt.triggerServiceUpdater()
		}
	} else {
		// Handle creation completion
		if success {
			// Note: a delete requested during this in-flight create (StateDeletionPending,
			// or StateDeletionInProgress with InFlightConfig != nil) is handled by the
			// pre-empt block at the top of this function, so we know opState.State is still
			// StateCreationInProgress here.

			dt.logger.V(2).Info("Created service", "service", serviceUID)
			recordServiceOperation("create", opState.Config.IsInbound, startTime, nil, "", opState.IsOrphan)
			opState.State = StateCreated
			opState.RetryCount = 0
			// Persist applied config snapshot for future UpdateService diffing.
			if opState.InFlightConfig != nil {
				applied := *opState.InFlightConfig
				opState.LastAppliedConfig = &applied
			} else {
				appliedCopy := opState.Config
				opState.LastAppliedConfig = &appliedCopy
			}

			// If a config update arrived while creation was in flight, schedule an update now.
			if opState.InFlightConfig != nil && !configsEqualForUpdate(opState.InFlightConfig, &opState.Config) {
				dt.logger.V(5).Info("Scheduled service update after desired config drifted during creation", "service", serviceUID)
				opState.State = StateUpdateInProgress
				dt.triggerServiceUpdater()
			}
			opState.InFlightConfig = nil

			// Promote any pending endpoints and pods
			dt.promotePendingEndpointsLocked(serviceUID)
			dt.promotePendingPodsLocked(serviceUID)

			// Trigger LocationsUpdater to sync the service state (whether buffers existed or not)
			dt.triggerLocationsUpdater()

			// Check if initialization is complete after service creation
			dt.checkInitializationCompleteLocked()

		} else {
			dt.logger.V(4).Info("Could not create service", "err", err, "service", serviceUID)
			// Capture the attempted config before clearing it (a stale snapshot would misfire a later
			// delete-completion pre-empt), so the terminal branch can detect a desired-spec drift that
			// landed while this attempt was in flight.
			attempted := opState.InFlightConfig
			opState.InFlightConfig = nil

			if isTerminalError(err) {
				recordServiceOperation("create", opState.Config.IsInbound, startTime, err, "ValidationError", opState.IsOrphan)
				if attempted != nil && !configsEqualForUpdate(attempted, &opState.Config) {
					// The desired config changed while this failing attempt was in flight, so the
					// failure was for a stale spec; re-dispatch the new desired config (which may be
					// valid) rather than parking on a spec the user has already replaced.
					resetRetryStateLocked(opState)
					opState.State = StateNotStarted
					opState.LastAttempt = time.Now().Format(time.RFC3339)
					dt.logger.V(5).Info("Re-dispatched service creation after desired config drifted during a terminal failure", "service", serviceUID)
					dt.triggerServiceUpdater()
					dt.checkInitializationCompleteLocked()
				} else {
					// Deterministic, spec-driven failure (e.g. unsupported protocol, port out
					// of range). Retrying cannot succeed, so park the service instead of looping
					// forever. A later UpdateService with a changed spec clears the park.
					opState.CreationFailedTerminal = true
					opState.State = StateNotStarted
					opState.LastAttempt = time.Now().Format(time.RFC3339)
					dt.logger.V(4).Info("Parked service after non-retryable creation error", "err", err, "service", serviceUID)
					dt.checkInitializationCompleteLocked()
				}
			} else {
				_, errCode := extractAzureErrorInfo(err)
				recordServiceOperation("create", opState.Config.IsInbound, startTime, err, errCode, opState.IsOrphan)
				opState.RetryCount++
				opState.LastAttempt = time.Now().Format(time.RFC3339)
				opState.NextRetryAt = time.Now().Add(computeRetryBackoff(opState.RetryCount))
				recordServiceOperationRetry("create", opState.Config.IsInbound, opState.RetryCount)

				dt.logger.V(4).Info("Scheduled service creation retry", "service", serviceUID, "attempt", opState.RetryCount, "nextRetryAt", opState.NextRetryAt)
				// Reset to NotStarted for retry
				opState.State = StateNotStarted
				// Trigger ServiceUpdater for retry
				dt.triggerServiceUpdater()
			}
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

	dt.logger.V(5).Info("Promoted pending endpoint updates", "count", len(pendingEndpoints), "service", serviceUID)

	// Replay each buffered update in arrival order, applying both its removals and
	// additions, so the live state mirrors what a sequence of live events would have
	// produced. A simple union of the "new" snapshots would resurrect endpoints that
	// were removed during the creation window (they vanish from later snapshots but
	// were never explicitly deleted), leaking stale pod IPs into NRP.
	var firstErr []error
	for _, update := range pendingEndpoints {
		errs := dt.updateK8sEndpointsLocked(UpdateK8sEndpointsInputType{
			InboundIdentity: serviceUID,
			OldAddresses:    update.OldPodIPToNodeIP,
			NewAddresses:    update.PodIPToNodeIP,
		})
		if len(errs) > 0 {
			firstErr = append(firstErr, errs...)
		}
	}
	if len(firstErr) > 0 {
		dt.logger.V(4).Info("Could not update promoted endpoints", "err", firstErr, "service", serviceUID)
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
		dt.logger.V(4).Info("Could not add pod with invalid parameters", "service", serviceUID, "location", location, "address", address)
		return
	}

	dt.logger.V(5).Info("Added pod request", "service", serviceUID, "pod", podKey, "location", location, "address", address)

	// A pod reaching AddPod is live; drop any stale pending finalizer-removal record (e.g. from a
	// prior egress identity after a label or IP change) so CheckPendingPodDeletions cannot strip
	// its cleanup finalizer while it still backs a current egress service. DeletePod re-enqueues it.
	delete(dt.pendingPodDeletions, podKey)

	// Check if service operation is tracked
	opState, exists := dt.pendingServiceOps[serviceUID]

	if !exists {

		// Check if service exists in NRP first (handles restart scenario and is more authoritative)
		if dt.NRPResources.NATGateways.Has(serviceUID) {
			dt.logger.V(5).Info("Added pod for existing service", "service", serviceUID, "pod", podKey)
			err := dt.updateK8sPodLocked(UpdatePodInputType{
				PodOperation:           Add,
				PublicOutboundIdentity: serviceUID,
				Location:               location,
				Address:                address,
			})
			if err != nil {
				dt.logger.V(4).Info("Could not add pod", "err", err, "pod", podKey)
				// Still trigger LocationsUpdater even if pod add failed
			}
			// Trigger LocationsUpdater to sync the change
			dt.triggerLocationsUpdater()
			return
		}
		// Service doesn't exist - need to create it first
		dt.logger.V(5).Info("Buffered pod and triggered service creation", "service", serviceUID, "pod", podKey)

		// Create service operation
		podParts := strings.SplitN(podKey, "/", 2)
		podNS := podParts[0]
		podName := ""
		if len(podParts) == 2 {
			podName = podParts[1]
		}
		dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
			ServiceUID:             serviceUID,
			Config:                 NewOutboundServiceConfig(serviceUID, nil),
			State:                  StateNotStarted,
			RetryCount:             0,
			LastAttempt:            time.Now().Format(time.RFC3339),
			CreatedAt:              time.Now(),
			CorrelationID:          uuid.NewString(),
			TriggeringPodNamespace: podNS,
			TriggeringPodName:      podName,
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
		dt.logger.V(5).Info("Buffered pod while service is being created", "service", serviceUID, "state", opState.State, "pod", podKey)
		dt.pendingPods[serviceUID] = append(dt.pendingPods[serviceUID], PendingPodUpdate{
			PodKey:    podKey,
			Location:  location,
			Address:   address,
			Timestamp: time.Now().Format(time.RFC3339),
		})

	case StateCreated:
		// Service is ready - add pod immediately
		dt.logger.V(5).Info("Added pod for ready service", "service", serviceUID, "pod", podKey)
		err := dt.updateK8sPodLocked(UpdatePodInputType{
			PodOperation:           Add,
			PublicOutboundIdentity: serviceUID,
			Location:               location,
			Address:                address,
		})
		if err != nil {
			dt.logger.V(4).Info("Could not add pod", "err", err, "pod", podKey)
			// Still trigger LocationsUpdater even if pod add failed
		}

		// Trigger LocationsUpdater to sync the change
		dt.triggerLocationsUpdater()

	case StateUpdateInProgress:
		// Outbound updates are currently a fast-path no-op in the ServiceUpdater
		// (see updateInboundService dispatch); they should not produce a sustained
		// StateUpdateInProgress for NAT Gateway services. Treat the same as StateCreated
		// for safety in case the outbound update path is implemented later.
		dt.logger.V(5).Info("Added pod while service is updating", "service", serviceUID, "pod", podKey)
		err := dt.updateK8sPodLocked(UpdatePodInputType{
			PodOperation:           Add,
			PublicOutboundIdentity: serviceUID,
			Location:               location,
			Address:                address,
		})
		if err != nil {
			dt.logger.V(4).Info("Could not add pod during update", "err", err, "pod", podKey)
		}
		dt.triggerLocationsUpdater()

	case StateDeletionPending:
		// The service's last pod was just removed (e.g. a sole egress pod changing its
		// IP, which the informer delivers as remove-old-then-add-new), but the NAT Gateway
		// deletion has not been dispatched yet (that happens in StateDeletionInProgress).
		// Dropping this add would leave the new pod without egress once the NAT Gateway is
		// deleted, so cancel the pending deletion and revive the service instead.
		dt.logger.V(5).Info("Revived service pending deletion for pod", "pod", podKey, "service", serviceUID)
		opState.State = StateCreated
		delete(dt.pendingServiceDeletions, serviceUID)
		// The pod being re-added is alive: drop its own last-pod deletion record so its
		// finalizer is preserved (it must NOT be removed).
		delete(dt.pendingPodDeletions, podKey)
		// Any remaining last-pod records for this service belong to genuinely departed
		// pods. Since the NAT Gateway will no longer be deleted, demote them to normal
		// pending deletions so CheckPendingPodDeletions removes their finalizers once
		// their addresses leave NRP (instead of waiting on a NAT deletion that won't happen).
		for _, pending := range dt.pendingPodDeletions {
			if pending.ServiceUID == serviceUID && pending.IsLastPod {
				pending.IsLastPod = false
			}
		}
		if err := dt.updateK8sPodLocked(UpdatePodInputType{
			PodOperation:           Add,
			PublicOutboundIdentity: serviceUID,
			Location:               location,
			Address:                address,
		}); err != nil {
			dt.logger.V(4).Info("Could not revive pod for service", "err", err, "pod", podKey, "service", serviceUID)
		}
		dt.triggerLocationsUpdater()

	case StateDeletionInProgress:
		// Deletion has already been dispatched (the NAT Gateway delete may be in flight),
		// so reviving here would race the delete. Instead of dropping the pod (which would
		// strand a live pod without egress until the next informer resync, 12-24h), buffer
		// it: when the deletion completes, OnServiceCreationComplete re-creates the service
		// and promotePendingPodsLocked replays the buffered pod, so the new pod gets egress.
		dt.logger.V(5).Info("Buffered pod while service deletion is in progress", "pod", podKey, "service", serviceUID)
		dt.pendingPods[serviceUID] = append(dt.pendingPods[serviceUID], PendingPodUpdate{
			PodKey:    podKey,
			Location:  location,
			Address:   address,
			Timestamp: time.Now().Format(time.RFC3339),
		})

	default:
		dt.logger.V(4).Info("Found unknown service operation state while adding pod", "state", opState.State, "service", serviceUID)
	}
}

// DeletePodResult contains the result of a DeletePod operation
type DeletePodResult struct {
	IsLastPod bool // True if this was the last pod for the service
	Enqueued  bool // True if the pod was recorded in pendingPodDeletions for drain-gated finalizer removal
}

// deletePodAddressOutcome reports the effect of removing one of a pod's egress addresses, so DeletePod
// can build a single drain-gated finalizer record and trigger the LocationsUpdater once for the whole
// pod (keeping the per-pod deletion atomic).
type deletePodAddressOutcome struct {
	drainGated  bool // the address was removed from live state and needs drain-gated finalizer handling
	isLastPod   bool // removing this address emptied the service's egress ref-count
	triggerSync bool // a LocationsUpdater sync is required to push the removal to NRP
}

// deletePodAddressLocked removes a single egress address for a pod and reports what happened. It does
// NOT enqueue the pod's finalizer record or trigger the LocationsUpdater; DeletePod does both once,
// after every address has been processed, so CheckPendingPodDeletions can never observe a partial
// address set and strip the pod's single finalizer while another address is still registered in NRP.
// Must be called with dt.mu held.
func (dt *DiffTracker) deletePodAddressLocked(serviceUID, location, address, podKey string) deletePodAddressOutcome {
	// Resolve the location the address is actually registered under: the caller's hint is the pod's
	// primary node IP, which is wrong for a secondary-family address (see
	// resolveOutboundAddressLocationLocked). Without this the removal would no-op as stale and leak.
	location = dt.resolveOutboundAddressLocationLocked(serviceUID, location, address)

	// If the pod is still buffered for an in-flight service creation, it never reached live state or
	// the ref-counter. Cancel the buffered add so it is not resurrected on promotion. Match on podKey
	// (when known) so a same-IP replacement buffered under a different pod is not cancelled too.
	if dt.cancelBufferedPodLocked(serviceUID, location, address, podKey) {
		dt.logger.V(5).Info("Cancelled buffered pod before service creation", "service", serviceUID, "location", location, "address", address)
		// If that was the service's only pod, tear down the pod-less NAT Gateway so it is not leaked.
		dt.handleEmptyOutboundServiceLocked(serviceUID)
		return deletePodAddressOutcome{}
	}

	// A stale or duplicate delete (informer double-delivery, or a pod that already moved/was removed)
	// must be a no-op: otherwise isLastPod would be computed from OTHER still-live pods' ref-count and
	// could falsely tear down a still-served service.
	if !dt.outboundPodExistsLocked(serviceUID, location, address) {
		dt.logger.V(5).Info("Skipped stale pod delete", "service", serviceUID, "location", location, "address", address)
		return deletePodAddressOutcome{}
	}

	val, ok := dt.outboundIdentityPodRefCount.Load(strings.ToLower(serviceUID))
	if !ok {
		dt.logger.V(4).Info("Could not find service pod ref-count", "service", serviceUID)
		if err := dt.updateK8sPodLocked(UpdatePodInputType{
			PodOperation:           Remove,
			PublicOutboundIdentity: serviceUID,
			Location:               location,
			Address:                address,
		}); err != nil {
			dt.logger.V(4).Info("Could not remove pod", "err", err)
		}
		return deletePodAddressOutcome{triggerSync: true}
	}

	counter := val.(int)
	if counter <= 0 {
		dt.logger.V(4).Info("Found invalid service pod counter", "service", serviceUID, "count", counter)
		return deletePodAddressOutcome{}
	}

	// counter == 1 means this removes the last registered address, so the service becomes empty.
	isLastPod := counter == 1

	if err := dt.updateK8sPodLocked(UpdatePodInputType{
		PodOperation:           Remove,
		PublicOutboundIdentity: serviceUID,
		Location:               location,
		Address:                address,
	}); err != nil {
		dt.logger.V(4).Info("Could not remove pod", "err", err)
		return deletePodAddressOutcome{}
	}

	if isLastPod {
		dt.logger.V(5).Info("Marked service for deletion after last pod was removed", "service", serviceUID)
		opState, exists := dt.pendingServiceOps[serviceUID]
		if !exists {
			dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
				ServiceUID:    serviceUID,
				Config:        NewOutboundServiceConfig(serviceUID, nil),
				State:         StateDeletionPending,
				RetryCount:    0,
				LastAttempt:   time.Now().Format(time.RFC3339),
				CreatedAt:     time.Now(),
				CorrelationID: uuid.NewString(),
			}
		} else {
			opState.State = StateDeletionPending
		}
		dt.pendingServiceDeletions[serviceUID] = &PendingServiceDeletion{
			ServiceUID: serviceUID,
			IsInbound:  false,
			Timestamp:  time.Now().Format(time.RFC3339),
		}
	}

	return deletePodAddressOutcome{drainGated: true, isLastPod: isLastPod, triggerSync: true}
}

// DeletePod handles pod deletion events for outbound (NAT Gateway) services. A dual-stack pod is
// passed with one address per IP family; every address is removed atomically under a single lock so
// the pod's one cleanup finalizer is enqueued exactly once, covering all addresses, and is removed
// only after all of them have drained from NRP. namespace and name are optional - if provided, they
// enable drain-gated pod finalizer tracking. Returns DeletePodResult indicating whether this was the
// service's last pod and whether a drain-gated record was enqueued.
//
// Finalizer handling:
// - Non-last pod: the finalizer is removed by CheckPendingPodDeletions once every address drains.
// - Last pod: the finalizer is removed by RemoveLastPodFinalizers after the NAT Gateway is deleted.
func (dt *DiffTracker) DeletePod(serviceUID, location string, addresses []string, namespace, name, uid string) DeletePodResult {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	result := DeletePodResult{}

	if serviceUID == "" || location == "" || len(addresses) == 0 {
		dt.logger.V(4).Info("Could not delete pod with invalid parameters", "service", serviceUID, "location", location, "addresses", addresses)
		return result
	}

	dt.logger.V(5).Info("Deleted pod request", "service", serviceUID, "location", location, "addresses", addresses, "namespace", namespace, "name", name)

	var drainGated []string
	triggerSync := false
	// Identity of the pod being deleted, when the caller supplied it. Used to cancel only THIS pod's
	// buffered add (not a same-IP replacement). Empty for identity-less callers (the live
	// re-registration drain and init reconciliation), which fall back to address-only matching.
	var identityPodKey string
	if namespace != "" && name != "" {
		identityPodKey = fmt.Sprintf("%s/%s", namespace, name)
	}
	for _, address := range addresses {
		if address == "" {
			continue
		}
		outcome := dt.deletePodAddressLocked(serviceUID, location, address, identityPodKey)
		if outcome.triggerSync {
			triggerSync = true
		}
		if outcome.isLastPod {
			result.IsLastPod = true
		}
		if outcome.drainGated {
			drainGated = appendAddressIfAbsent(drainGated, address)
		}
	}

	// Enqueue a single drain-gated finalizer record for the whole pod (one pod object carries one
	// finalizer). The finalizer is removed only after every recorded address has drained from NRP,
	// never inline on the delete event; removing it earlier would let the pod (and its IPs) be
	// reclaimed while NRP still maps an address to this service's NAT Gateway.
	if namespace != "" && name != "" {
		podKey := fmt.Sprintf("%s/%s", namespace, name)
		if len(drainGated) > 0 {
			dt.pendingPodDeletions[podKey] = &PendingPodDeletion{
				Namespace:  namespace,
				Name:       name,
				UID:        uid,
				ServiceUID: serviceUID,
				Addresses:  drainGated,
				IsLastPod:  result.IsLastPod,
				Timestamp:  time.Now().Format(time.RFC3339),
			}
			result.Enqueued = true
			dt.logger.V(5).Info("Added pending pod deletion", "pod", podKey, "isLastPod", result.IsLastPod, "addresses", drainGated)
		} else if existing, ok := dt.pendingPodDeletions[podKey]; ok && (uid == "" || existing.UID == uid) {
			// A prior delete event already drain-gated this pod; a duplicate or subsequent
			// terminating-status event drains nothing new but must still report Enqueued so
			// podInformerRemovePod does not strip the finalizer while that drain is pending.
			// UID-guarded so a same-name replacement pod is not gated by a stale record.
			result.Enqueued = true
		}
	}

	if triggerSync {
		dt.triggerLocationsUpdater()
	}

	return result
}

// outboundPodExistsLocked reports whether a pod at the given location/address is
// currently tracked in live K8s state with the given outbound (egress) identity.
// It is used by DeletePod to distinguish a real removal from a stale/duplicate delete
// (which must be a no-op). Must be called with dt.mu held.
func (dt *DiffTracker) outboundPodExistsLocked(serviceUID, location, address string) bool {
	node, ok := dt.K8sResources.Nodes[location]
	if !ok {
		return false
	}
	pod, ok := node.Pods[address]
	if !ok {
		return false
	}
	return pod.PublicOutboundIdentity != "" && strings.EqualFold(pod.PublicOutboundIdentity, serviceUID)
}

// resolveOutboundAddressLocationLocked returns the node location an outbound address is actually
// tracked under. The caller's hint (the pod's primary node IP) is wrong for a dual-stack pod's
// secondary-family address, so on a miss it searches live state then buffered pending pods; an
// untracked address returns the hint (the caller no-ops). Requires dt.mu held.
func (dt *DiffTracker) resolveOutboundAddressLocationLocked(serviceUID, hintLocation, address string) string {
	if dt.outboundPodExistsLocked(serviceUID, hintLocation, address) {
		return hintLocation
	}
	for loc, node := range dt.K8sResources.Nodes {
		if pod, ok := node.Pods[address]; ok && pod.PublicOutboundIdentity != "" && strings.EqualFold(pod.PublicOutboundIdentity, serviceUID) {
			return loc
		}
	}
	for _, pending := range dt.pendingPods[serviceUID] {
		if pending.Address == address {
			return pending.Location
		}
	}
	return hintLocation
}

// handleEmptyOutboundServiceLocked tears down an outbound (NAT Gateway) service whose
// last buffered pod was just cancelled, so a service whose only pod disappeared before
// promotion does not leak an orphaned, pod-less NAT Gateway. It is a no-op if any buffered
// or live pods remain. Must be called with dt.mu held.
func (dt *DiffTracker) handleEmptyOutboundServiceLocked(serviceUID string) {
	if len(dt.pendingPods[serviceUID]) > 0 {
		return
	}
	if v, ok := dt.outboundIdentityPodRefCount.Load(strings.ToLower(serviceUID)); ok && v.(int) > 0 {
		return
	}
	opState, exists := dt.pendingServiceOps[serviceUID]
	if !exists {
		return
	}
	switch opState.State {
	case StateNotStarted:
		// Creation has not been dispatched yet (no Azure resource exists); abort it.
		dt.logger.V(5).Info("Aborted service creation after last buffered pod was removed", "service", serviceUID)
		delete(dt.pendingServiceOps, serviceUID)
		delete(dt.pendingEndpoints, serviceUID)
		delete(dt.pendingPods, serviceUID)
		delete(dt.pendingServiceDeletions, serviceUID)
		dt.checkInitializationCompleteLocked()
	case StateCreationInProgress:
		// The NAT Gateway create is in flight. Mark the service for deletion: when the create
		// completes, OnServiceCreationComplete's pre-empt (StateDeletionInProgress with
		// InFlightConfig != nil) routes it to a real delete, preventing an orphaned gateway.
		dt.logger.V(5).Info("Scheduled service deletion after last buffered pod was removed during creation", "service", serviceUID)
		opState.State = StateDeletionInProgress
		dt.pendingServiceDeletions[serviceUID] = &PendingServiceDeletion{
			ServiceUID: serviceUID,
			IsInbound:  opState.Config.IsInbound,
			Timestamp:  time.Now().Format(time.RFC3339),
		}
	}
}

// cancelBufferedPodLocked removes buffered (not-yet-promoted) pod entries for a service that match
// the given location/address. When podKey is non-empty it additionally requires the entry's PodKey
// to match, so a delayed delete for one pod cannot cancel a DIFFERENT pod that reused the same IP
// while buffered during in-flight service creation (which would strand the live replacement without
// egress). An empty podKey preserves address-only matching for identity-less callers (the live
// re-registration drain and init reconciliation). It returns true if at least one entry was removed.
// Pods buffered during StateNotStarted/StateCreationInProgress are not yet in live state or the
// ref-counter, so a deletion in that window must cancel the buffered add; otherwise
// promotePendingPodsLocked would resurrect the deleted pod. Must be called with dt.mu held.
func (dt *DiffTracker) cancelBufferedPodLocked(serviceUID, location, address, podKey string) bool {
	buffered, exists := dt.pendingPods[serviceUID]
	if !exists || len(buffered) == 0 {
		return false
	}
	kept := buffered[:0]
	removed := false
	for _, pod := range buffered {
		if pod.Location == location && pod.Address == address && (podKey == "" || pod.PodKey == podKey) {
			removed = true
			continue
		}
		kept = append(kept, pod)
	}
	if !removed {
		return false
	}
	if len(kept) == 0 {
		delete(dt.pendingPods, serviceUID)
	} else {
		dt.pendingPods[serviceUID] = kept
	}
	return true
}

// promotePendingPodsLocked flushes all pending pods for a service after it's created.
// Must be called with dt.mu held.
func (dt *DiffTracker) promotePendingPodsLocked(serviceUID string) {
	pendingPods, exists := dt.pendingPods[serviceUID]
	if !exists || len(pendingPods) == 0 {
		return
	}

	dt.logger.V(5).Info("Promoted pending pods", "count", len(pendingPods), "service", serviceUID)

	for _, pod := range pendingPods {
		dt.logger.V(5).Info("Added promoted pod", "pod", pod.PodKey, "location", pod.Location, "address", pod.Address)

		err := dt.updateK8sPodLocked(UpdatePodInputType{
			PodOperation:           Add,
			PublicOutboundIdentity: serviceUID,
			Location:               pod.Location,
			Address:                pod.Address,
		})
		if err != nil {
			dt.logger.V(4).Info("Could not add promoted pod", "err", err, "pod", pod.PodKey)
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
	blockedCount := 0
	defer func() {
		updateServicesBlockedByLocationsMetric(blockedCount)
		updatePendingServiceDeletionsMetric(dt)
	}()

	dt.mu.Lock()
	defer dt.mu.Unlock()

	if len(dt.pendingServiceDeletions) == 0 {
		return
	}

	dt.logger.V(4).Info("Checked pending service deletions", "count", len(dt.pendingServiceDeletions))

	// Iterate through all pending deletions
	for serviceUID, pendingDeletion := range dt.pendingServiceDeletions {
		dt.logger.V(5).Info("Checked pending service deletion", "service", serviceUID, "isInbound", pendingDeletion.IsInbound)

		// Check if service still has locations in NRP
		hasLocations := dt.serviceHasLocationsInNRP(serviceUID)
		if hasLocations {
			dt.logger.V(5).Info("Waited for service locations to clear before deletion", "service", serviceUID)
			blockedCount++
			continue
		}

		// Locations cleared - proceed with deletion
		dt.logger.V(5).Info("Triggered service deletion after locations cleared", "service", serviceUID)

		// Update service state to DeletionInProgress
		if opState, exists := dt.pendingServiceOps[serviceUID]; exists {
			opState.State = StateDeletionInProgress
		} else {
			// Service not in pendingServiceOps - create entry
			dt.logger.V(4).Info("Created missing pending service operation for deletion", "service", serviceUID)
			var config ServiceConfig
			if pendingDeletion.IsInbound {
				config = NewInboundServiceConfig(serviceUID, nil)
			} else {
				config = NewOutboundServiceConfig(serviceUID, nil)
			}
			dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
				ServiceUID:    serviceUID,
				Config:        config,
				State:         StateDeletionInProgress,
				RetryCount:    0,
				LastAttempt:   time.Now().Format(time.RFC3339),
				CreatedAt:     time.Now(),
				CorrelationID: uuid.NewString(),
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

	dt.logger.V(2).Info("Waited for initialization to complete")

	select {
	case <-ch:
		dt.logger.V(2).Info("Completed initialization")
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for initial sync: %w", ctx.Err())
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
	// StateCreated ops remain tracked for runtime operations. Parked ops (CreationFailedTerminal or
	// RetriesExhausted) self-heal in the background and must not hold initial sync open, or a single
	// un-provisionable service would block cloud-provider init, which waits on a no-timeout context.
	pendingOps := 0
	for _, opState := range dt.pendingServiceOps {
		if opState.State != StateCreated && !opState.CreationFailedTerminal && !opState.RetriesExhausted {
			pendingOps++
		}
	}
	inFlightTriggers := atomic.LoadInt32(&dt.pendingUpdaterTriggers)
	// Recovered pod deletions must drain before init is done, otherwise
	// WaitForInitialSync returns while their finalizers are still pending.
	pendingPodDeletions := len(dt.pendingPodDeletions)

	if pendingOps == 0 && inFlightTriggers == 0 && pendingPodDeletions == 0 {
		dt.logger.V(2).Info("Signaled initialization completion", "pendingOps", pendingOps, "inFlightTriggers", inFlightTriggers, "pendingPodDeletions", pendingPodDeletions)

		// Mark initialization as done (idempotent using sync.Once)
		dt.initCompletionOnce.Do(func() {
			atomic.StoreInt32(&dt.isInitializing, 0)
			close(dt.initCompletionChecker)
		})
	} else {
		dt.logger.V(4).Info("Still initializing", "pendingOps", pendingOps, "inFlightTriggers", inFlightTriggers, "pendingPodDeletions", pendingPodDeletions)
	}
}

// configsEqualForUpdate returns true if two ServiceConfigs describe the same desired state
// from the perspective of the update path. Only the inbound shape is compared today.
func configsEqualForUpdate(a, b *ServiceConfig) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.IsInbound != b.IsInbound {
		return false
	}
	if a.IsInbound {
		return a.InboundConfig.Equals(b.InboundConfig)
	}
	// Outbound update path is not implemented yet; treat as equal so we don't loop.
	return true
}

// IsServiceTracked reports whether the engine has any knowledge of this service —
// either an active operation in pendingServiceOps or an entry in NRPResources
// indicating the LB/NAT-Gateway already exists in Azure. Callers in the cloud
// provider use this to decide between AddService (first-time create) and
// UpdateService (apply spec edits to an existing service).
func (dt *DiffTracker) IsServiceTracked(serviceUID string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if _, ok := dt.pendingServiceOps[serviceUID]; ok {
		return true
	}
	if dt.NRPResources.LoadBalancers != nil && dt.NRPResources.LoadBalancers.Has(serviceUID) {
		return true
	}
	if dt.NRPResources.NATGateways != nil && dt.NRPResources.NATGateways.Has(serviceUID) {
		return true
	}
	return false
}
