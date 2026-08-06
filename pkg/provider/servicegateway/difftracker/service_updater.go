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

package difftracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// ServiceUpdater processes service creation/deletion in parallel
type ServiceUpdater struct {
	diffTracker *DiffTracker
	onComplete  func(serviceUID string, success bool, err error)
	trigger     <-chan bool
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	semaphore   chan struct{}   // Limits concurrent operations to 10
	mu          sync.Mutex      // Protects activeOperations
	activeOps   map[string]bool // Tracks which services are being processed
	// retryTimers holds the pending re-dispatch timer per service. A parked or backed-off operation
	// has no external driver, so it self-arms with time.AfterFunc; those timers are not tracked by wg
	// nor bound to ctx, so they must be stopped explicitly or they keep the whole DiffTracker
	// reachable and fire into a stopped updater. Keyed by service UID so re-arming replaces its
	// pending revisit. Guarded by mu.
	retryTimers map[string]*time.Timer
	logger      logr.Logger
}

// NewServiceUpdater creates a new ServiceUpdater instance
func NewServiceUpdater(ctx context.Context, diffTracker *DiffTracker, onComplete func(string, bool, error), triggerChan <-chan bool) *ServiceUpdater {
	if diffTracker == nil {
		panic("ServiceUpdater: diffTracker must not be nil")
	}
	if onComplete == nil {
		panic("ServiceUpdater: onComplete callback must not be nil")
	}
	if triggerChan == nil {
		panic("ServiceUpdater: triggerChan must not be nil")
	}
	if diffTracker.networkClientFactory == nil {
		panic("ServiceUpdater: diffTracker.networkClientFactory must not be nil")
	}
	childCtx, cancel := context.WithCancel(ctx)
	return &ServiceUpdater{
		diffTracker: diffTracker,
		onComplete:  onComplete,
		trigger:     triggerChan,
		ctx:         childCtx,
		cancel:      cancel,
		semaphore:   make(chan struct{}, 10), // Max 10 concurrent operations
		activeOps:   make(map[string]bool),
		retryTimers: make(map[string]*time.Timer),
		logger:      diffTracker.logger.WithName("ServiceUpdater"),
	}
}

// terminalError marks a creation failure as non-retryable (a deterministic error
// such as an invalid Service spec). The engine parks such services instead of
// retrying them forever.
type terminalError struct{ err error }

func (e *terminalError) Error() string { return e.err.Error() }
func (e *terminalError) Unwrap() error { return e.err }

// newTerminalError wraps err so isTerminalError reports true.
func newTerminalError(err error) error { return &terminalError{err: err} }

// isTerminalError reports whether err (or anything it wraps) is a terminalError.
func isTerminalError(err error) bool {
	var t *terminalError
	return errors.As(err, &t)
}

// Run starts the ServiceUpdater main loop
func (s *ServiceUpdater) Run() {
	s.logger.V(2).Info("Started ServiceUpdater")

	// Periodic ticker to keep the operation gauges fresh even when no new operations arrive
	ageTicker := time.NewTicker(30 * time.Second)
	defer ageTicker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			s.logger.V(2).Info("Stopping ServiceUpdater")
			s.wg.Wait() // Wait for all goroutines to finish
			return
		case <-s.trigger:
			s.logger.V(5).Info("Processing triggered service batch")
			s.processBatch()
		case <-ageTicker.C:
			s.refreshOperationGauges()
		}
	}
}

// refreshOperationGauges recomputes the operation gauges from pendingServiceOps. They are only
// refreshed from AddService/UpdateService/DeleteService/OnServiceCreationComplete, so the dispatcher
// and the pod-driven paths mutate that map without updating them; outbound services are created only
// by pod events, so without a periodic recompute the counts miss the egress fleet entirely and keep
// a phantom after an aborted operation is untracked.
func (s *ServiceUpdater) refreshOperationGauges() {
	updatePendingOperationOldestAgeMetric(s.diffTracker)
	updatePendingServiceOperationsMetric(s.diffTracker)
	updateTrackedServicesMetric(s.diffTracker)
}

// Stop gracefully shuts down the ServiceUpdater
func (s *ServiceUpdater) Stop() {
	s.logger.V(2).Info("Stopping ServiceUpdater")
	s.cancel()
	// Cancel pending re-dispatch timers so they stop holding the DiffTracker. A timer that has
	// already fired is harmless: its callback checks ctx.
	s.mu.Lock()
	for uid, timer := range s.retryTimers {
		timer.Stop()
		delete(s.retryTimers, uid)
	}
	s.mu.Unlock()
	s.wg.Wait()
	s.logger.V(2).Info("Stopped ServiceUpdater")
}

// scheduleRetry arms a single re-dispatch for a service after delay, replacing any revisit already
// pending for it so repeated backoff passes cannot accumulate timers. The callback is a no-op once
// the updater's context is done. Takes s.mu.
func (s *ServiceUpdater) scheduleRetry(serviceUID string, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retryTimers == nil {
		// Tolerate an updater built as a struct literal rather than through NewServiceUpdater.
		s.retryTimers = make(map[string]*time.Timer)
	}
	if existing, ok := s.retryTimers[serviceUID]; ok {
		existing.Stop()
	}
	s.retryTimers[serviceUID] = time.AfterFunc(delay, func() {
		s.mu.Lock()
		delete(s.retryTimers, serviceUID)
		s.mu.Unlock()
		if s.ctx != nil && s.ctx.Err() != nil {
			return
		}
		s.diffTracker.triggerServiceUpdater()
	})
}

// requeueIfMoreWork is fired from a per-service goroutine's defer chain AFTER
// activeOps[uid] has been cleared. It pokes the dispatcher to re-scan
// pendingServiceOps. This closes a race where onComplete (running INSIDE the
// goroutine, while activeOps[uid] was still true) already enqueued a
// follow-up trigger that the dispatcher consumed during the activeOps-held
// window — leaving a service stuck with no active worker and no pending trigger.
//
// The trigger send is non-blocking; the channel buffer dedupes consecutive
// triggers, and an empty pendingServiceOps scan is a cheap O(N) no-op.
//
// It must route through triggerServiceUpdater (not a raw channel send) so that,
// during initialization, the in-flight trigger counter is incremented to match the
// decrement performed by the processBatch this requeue will cause. A raw send would
// leave that decrement unmatched, driving pendingUpdaterTriggers negative and making
// WaitForInitialSync hang forever. Post-init, triggerServiceUpdater does not increment,
// so steady-state behavior is unchanged.
func (s *ServiceUpdater) requeueIfMoreWork(uid string) {
	s.logger.V(5).Info("Queued follow-up service updater trigger", "serviceUID", uid)
	s.diffTracker.triggerServiceUpdater()
}

// maxServiceRetries bounds how many times a transient create/update/delete failure is retried
// before the dispatcher gives up and parks the operation (RetriesExhausted) instead of looping
// unbounded. Paired with the per-attempt backoff recorded in ServiceOperationState.NextRetryAt.
const maxServiceRetries = 12

// parkReArmCooldown is how long a parked operation stays parked before a resync may re-arm it, so a
// transient outage self-heals on the next resync of an unchanged Service while bounding retries to
// one burst per cooldown instead of a per-resync PUT storm. It must exceed the worst-case time to
// exhaust maxServiceRetries (~4.5 min at the 30s backoff cap) so a re-arm never overlaps the burst
// that parked it.
const parkReArmCooldown = 5 * time.Minute

// retryGate decides whether a retryable operation should be skipped this dispatch pass. It returns
// true (caller must `continue`) when the op is still within its post-failure backoff window
// (scheduling a guaranteed revisit via time.AfterFunc) or has exhausted its current retry burst
// (a park-and-re-arm cooldown). It must be called with s.diffTracker.mu held and after
// activeOps[uid] has been set; it releases activeOps[uid] on skip.
//
// Note the op is never abandoned: exhausting maxServiceRetries parks it for parkReArmCooldown and
// schedules a revisit, after which it re-arms and retries again. A permanently failing op therefore
// retries forever at one burst per cooldown rather than stranding until CCM restart.
func (s *ServiceUpdater) retryGate(serviceUID string, opState *ServiceOperationState) bool {
	// Retry-burst ceiling: pause re-dispatching until the park cooldown elapses.
	if opState.RetryCount >= maxServiceRetries {
		// A parked op has no external driver on a stable cluster, so it must self-arm or it strands
		// until CCM restart: the controller does not re-drive an unchanged Service (resync's
		// UpdateFunc(obj, obj) -> needsUpdate=false; UpdateLoadBalancer is a no-op in ServiceGateway
		// mode), and a parked delete also loses its caller once the controller drops its own
		// load-balancer finalizer. Park for a cooldown, schedule a revisit, then re-arm once it
		// elapses; a still-failing op re-parks, bounding retries to one burst per cooldown.
		if !opState.RetriesExhausted {
			opState.RetriesExhausted = true
			opState.NextRetryAt = time.Now().Add(parkReArmCooldown)
			recordServiceParked(parkReasonRetriesExceeded)
			recordServiceOperationRetries(operationLabelForState(opState.State), opState.Config.IsInbound, opState.RetryCount)
			s.logger.Info("Parked service operation after exhausting its retry burst; will re-arm and retry after the cooldown",
				"serviceUID", serviceUID, "state", opState.State, "retries", opState.RetryCount,
				"reArmAfter", parkReArmCooldown)
			s.scheduleRetry(serviceUID, parkReArmCooldown)

		} else if !opState.NextRetryAt.IsZero() && time.Now().After(opState.NextRetryAt) {
			// Cooldown elapsed: re-arm once so the op dispatches again this pass.
			resetRetryStateLocked(opState)
			return false
		}
		s.mu.Lock()
		delete(s.activeOps, serviceUID)
		s.mu.Unlock()
		return true
	}

	// Backoff: not yet time to retry. Release the slot and guarantee a revisit when the backoff
	// elapses; a buffered trigger only coalesces, so without this the op could otherwise wait for
	// an unrelated future trigger on a quiet cluster.
	if !opState.NextRetryAt.IsZero() && time.Now().Before(opState.NextRetryAt) {
		delay := time.Until(opState.NextRetryAt)
		s.mu.Lock()
		delete(s.activeOps, serviceUID)
		s.mu.Unlock()
		s.scheduleRetry(serviceUID, delay)
		return true
	}

	return false
}

// processBatch scans pendingServiceOps and spawns goroutines for services that need processing
func (s *ServiceUpdater) processBatch() {
	// Collect work to do while holding lock, then spawn goroutines after releasing lock
	type workItem struct {
		serviceUID             string
		config                 ServiceConfig
		state                  ResourceState
		correlationID          string
		triggeringPodNamespace string
		triggeringPodName      string
	}
	var workToDo []workItem

	s.diffTracker.mu.Lock()
	for serviceUID, opState := range s.diffTracker.pendingServiceOps {
		// Check if already being processed
		s.mu.Lock()
		if s.activeOps[serviceUID] {
			s.mu.Unlock()
			continue
		}
		s.activeOps[serviceUID] = true
		s.mu.Unlock()

		// Collect work based on state
		switch opState.State {
		case StateNotStarted:
			if opState.CreationFailedTerminal {
				// Parked after a non-retryable creation error; do not re-dispatch.
				s.mu.Lock()
				delete(s.activeOps, serviceUID)
				s.mu.Unlock()
				s.logger.V(4).Info("Skipped parked service", "serviceUID", serviceUID)
				continue
			}
			// Transition to CreationInProgress
			if s.retryGate(serviceUID, opState) {
				continue
			}
			opState.State = StateCreationInProgress
			opState.OperationStartedAt = time.Now()
			configSnapshot := opState.Config
			opState.InFlightConfig = &configSnapshot
			workToDo = append(workToDo, workItem{serviceUID, configSnapshot, StateCreationInProgress, opState.CorrelationID, opState.TriggeringPodNamespace, opState.TriggeringPodName})

		case StateCreationInProgress:
			// Already being processed by another goroutine, skip
			s.mu.Lock()
			delete(s.activeOps, serviceUID)
			s.mu.Unlock()
			s.logger.V(4).Info("Skipped service already being created", "serviceUID", serviceUID, "state", StateCreationInProgress)

		case StateCreated:
			// Service successfully created, nothing to do
			s.mu.Lock()
			delete(s.activeOps, serviceUID)
			s.mu.Unlock()
			s.logger.V(4).Info("Skipped already created service", "serviceUID", serviceUID, "state", StateCreated)

		case StateDeletionPending:
			// Services in StateDeletionPending are waiting for LocationsUpdater to clear their addresses.
			// They will be moved to pendingServiceDeletions map and checkPendingServiceDeletions() will transition
			// them to StateDeletionInProgress once locations are cleared. Skip processing here.
			s.mu.Lock()
			delete(s.activeOps, serviceUID)
			s.mu.Unlock()
			s.logger.V(4).Info("Skipped service waiting for locations to clear", "serviceUID", serviceUID, "state", StateDeletionPending)

		case StateDeletionInProgress:
			if s.retryGate(serviceUID, opState) {
				continue
			}
			opState.OperationStartedAt = time.Now()
			workToDo = append(workToDo, workItem{serviceUID, opState.Config, StateDeletionInProgress, opState.CorrelationID, opState.TriggeringPodNamespace, opState.TriggeringPodName})

		case StateUpdateInProgress:
			if s.retryGate(serviceUID, opState) {
				continue
			}
			// Snapshot the desired config so OnServiceCreationComplete can detect drift.
			opState.OperationStartedAt = time.Now()
			configSnapshot := opState.Config
			opState.InFlightConfig = &configSnapshot
			workToDo = append(workToDo, workItem{serviceUID, configSnapshot, StateUpdateInProgress, opState.CorrelationID, opState.TriggeringPodNamespace, opState.TriggeringPodName})
		}
	}
	s.diffTracker.mu.Unlock()

	if len(workToDo) > 0 {
		s.logger.V(4).Info("Collected services to process", "count", len(workToDo))
	}

	// Decrement in-flight trigger counter and check initialization completion
	// Run this asynchronously to avoid blocking goroutines waiting on completion callbacks
	defer func() {
		s.diffTracker.mu.Lock()
		shouldCheck := atomic.LoadInt32(&s.diffTracker.isInitializing) == 1
		s.diffTracker.mu.Unlock()

		if shouldCheck {
			atomic.AddInt32(&s.diffTracker.pendingUpdaterTriggers, -1)
			// Trigger check asynchronously to avoid holding the lock while goroutines complete
			go s.diffTracker.checkInitializationComplete()
		}
	}()

	// Spawn goroutines after releasing diffTracker lock
	for _, work := range workToDo {
		switch work.state {
		case StateCreationInProgress:
			s.wg.Add(1)
			go func(uid string, cfg ServiceConfig, corrID string, podNS string, podName string) {
				s.runWorker(uid, func() {
					if cfg.IsInbound {
						s.createInboundService(uid, cfg.InboundConfig, corrID)
					} else {
						s.createOutboundService(uid, cfg.OutboundConfig, corrID, podNS, podName)
					}
				})
			}(work.serviceUID, work.config, work.correlationID, work.triggeringPodNamespace, work.triggeringPodName)
		case StateDeletionInProgress:
			s.wg.Add(1)
			go func(uid string, cfg ServiceConfig, corrID string) {
				s.runWorker(uid, func() {
					if cfg.IsInbound {
						s.deleteInboundService(uid, corrID)
					} else {
						s.deleteOutboundService(uid, corrID)
					}
				})
			}(work.serviceUID, work.config, work.correlationID)
		case StateUpdateInProgress:
			s.wg.Add(1)
			go func(uid string, cfg ServiceConfig, corrID string) {
				s.runWorker(uid, func() {
					if cfg.IsInbound {
						s.updateInboundService(uid, cfg.InboundConfig, corrID)
					} else {
						s.logger.Info("Skipped outbound service update; egress identities cannot be updated in place", "serviceUID", uid)
						recordOutboundServiceUpdateSkipped()
						s.onComplete(uid, true, nil)
					}
				})
			}(work.serviceUID, work.config, work.correlationID)
		}
	}
}

// runWorker executes a single service operation with the shared worker lifecycle: it manages the
// wait group, active-op cleanup, and the follow-up requeue, bounds concurrency with the semaphore,
// and recovers panics. A panicking operation is reported as a failed op via onComplete (so the
// existing retry path handles it) instead of crashing the whole process. op is only invoked once
// the semaphore is acquired; if the context is cancelled first, the op is skipped.
func (s *ServiceUpdater) runWorker(uid string, op func()) {
	defer s.wg.Done()
	defer s.requeueIfMoreWork(uid)
	defer func() {
		s.mu.Lock()
		delete(s.activeOps, uid)
		s.mu.Unlock()
	}()
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error(fmt.Errorf("%v", r), "Recovered from panic in ServiceUpdater worker",
				"serviceUID", uid, "stack", string(debug.Stack()))
			s.onComplete(uid, false, fmt.Errorf("panic in service worker: %v", r))
		}
	}()

	// Acquire semaphore with context awareness
	select {
	case s.semaphore <- struct{}{}:
		defer func() {
			<-s.semaphore
		}()
	case <-s.ctx.Done():
		s.logger.V(4).Info("Skipped service because context was canceled before acquiring semaphore", "serviceUID", uid)
		return
	}

	op()
}

// createInboundService creates LoadBalancer resources for inbound service
func (s *ServiceUpdater) createInboundService(serviceUID string, config *InboundConfig, correlationID string) {
	s.logger.V(5).Info("Started creating inbound service", "serviceUID", serviceUID, "correlationID", correlationID)

	// Bound the operation so a hung/slow Azure call fails into retry instead of holding the
	// ServiceUpdater semaphore slot forever (see nrpOperationTimeout).
	ctx, cancel := context.WithTimeout(s.ctx, getNRPOperationTimeout())
	defer cancel()
	dropGoneService := func() {
		s.diffTracker.mu.Lock()
		delete(s.diffTracker.pendingServiceOps, serviceUID)
		delete(s.diffTracker.pendingEndpoints, serviceUID)
		delete(s.diffTracker.pendingPods, serviceUID)
		s.diffTracker.checkInitializationCompleteLocked()
		s.diffTracker.mu.Unlock()
	}

	// Step 0: Add finalizer to K8s service to prevent deletion until Azure resources are cleaned up
	svc, err := s.diffTracker.getServiceByUID(ctx, serviceUID)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			// A transient lookup failure (e.g. an apiserver List error) is NOT "service gone":
			// proceeding would create the PIP/LB/SGW without ever adding the K8s cleanup
			// finalizers, leaving Azure resources with no anchor for EnsureLoadBalancerDeleted.
			// Fail the operation so it is retried once the apiserver recovers.
			s.logger.V(4).Info("Could not get service for finalizer, will retry", "serviceUID", serviceUID, "correlationID", correlationID, "err", err)
			s.onComplete(serviceUID, false, fmt.Errorf("failed to get service for finalizer: %w", err))
			return
		}
		// Typed NotFound: the K8s Service is gone. Do NOT fall through to create the PIP/LB/SGW -
		// those would be orphaned (no Service object means no delete event ever cleans them up,
		// only the next restart's orphan-cleanup). Abort and drop tracking instead; if this was a
		// stale-List false-NotFound for a still-live Service, the cloud-provider re-syncs and
		// re-calls EnsureLoadBalancer, which re-adds the operation. We deliberately do NOT call
		// onComplete here: onComplete(false) would re-hit NotFound and loop, and onComplete(true)
		// would falsely report the service as Created.
		s.logger.V(4).Info("Service gone (NotFound) before create; aborting to avoid orphaned resources", "serviceUID", serviceUID, "correlationID", correlationID)
		dropGoneService()
		return
	}

	// Service exists: add the cleanup finalizers before creating any Azure resources.
	if err := s.diffTracker.addServiceGatewayFinalizer(ctx, svc); err != nil {
		if errors.Is(err, ErrServiceGoneOrReplaced) {
			s.logger.V(4).Info("Service gone or replaced before finalizer add; aborting to avoid orphaned resources", "serviceUID", serviceUID, "correlationID", correlationID)
			dropGoneService()
			return
		}
		s.logger.V(4).Info("Could not add finalizer to service", "serviceUID", serviceUID, "err", err)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to add finalizer: %w", err))
		return
	}
	s.logger.V(5).Info("Added finalizer to service", "serviceUID", serviceUID)

	// Step 1: Build resources using shared helper
	pipResource, lbResource, servicesDTO, err := buildInboundServiceResources(serviceUID, config, s.diffTracker.config)
	if err != nil {
		s.logger.V(4).Info("Could not build inbound service resources", "serviceUID", serviceUID, "correlationID", correlationID, "err", err)
		// Building resources only fails on deterministic, spec-driven validation errors
		// (unsupported protocol, port/idle-timeout out of range). Retrying cannot help, so
		// mark the failure terminal; the engine parks the service until its spec changes.
		s.onComplete(serviceUID, false, newTerminalError(fmt.Errorf("failed to build inbound resources: %w", err)))
		return
	}

	// Step 2: Create Public IP and capture the response to get the allocated IP address
	pipResponse, err := s.diffTracker.createOrUpdatePIPWithResponse(ctx, s.diffTracker.config.ResourceGroup, &pipResource)
	if err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not create Public IP for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create Public IP: %w", err))
		return
	}
	pipName := PublicIPName(serviceUID)
	s.logger.V(5).Info("Created Public IP for inbound service", "serviceUID", serviceUID, "publicIP", pipName)

	// Extract IP address from PIP response
	var pipIPAddress string
	if pipResponse != nil && pipResponse.Properties != nil && pipResponse.Properties.IPAddress != nil {
		pipIPAddress = *pipResponse.Properties.IPAddress
		s.logger.V(5).Info("Received Public IP address", "publicIP", pipName, "publicIPAddress", pipIPAddress)
	}

	// Step 3: Create LoadBalancer
	if err := s.diffTracker.createOrUpdateLB(ctx, lbResource); err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not create LoadBalancer for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		// Don't delete PIP here - retry will use existing PIP
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create LoadBalancer: %w", err))
		return
	}
	lbRulesCount := 0
	if lbResource.Properties != nil && lbResource.Properties.LoadBalancingRules != nil {
		lbRulesCount = len(lbResource.Properties.LoadBalancingRules)
	}
	s.logger.V(5).Info("Created LoadBalancer for inbound service", "serviceUID", serviceUID, "rules", lbRulesCount)

	// Step 4: Register service with ServiceGateway API
	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, servicesDTO); err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not register inbound service with ServiceGateway", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		// Don't delete resources - retry will reconcile
		s.onComplete(serviceUID, false, fmt.Errorf("failed to register with ServiceGateway: %w", err))
		return
	}
	s.logger.V(5).Info("Registered inbound service with ServiceGateway", "serviceUID", serviceUID, "correlationID", correlationID)

	// The LoadBalancer is live from here: Azure holds the PIP and LB, and NRP holds the registration.
	// Record that before the status write below, which touches only Kubernetes. Leaving it until
	// after would let a failed status patch demote a provisioned service back to StateNotStarted with
	// no LoadBalancer tracked, and isServiceReadyToSync then withholds its endpoints - a public IP
	// serving nothing until the patch eventually succeeds.
	s.diffTracker.UpdateNRPLoadBalancers(SyncServicesReturnType{
		Additions: newIgnoreCaseSetFromSlice([]string{serviceUID}),
		Removals:  nil,
	})

	// Step 5: Update K8s Service status with the external IP.
	// EnsureLoadBalancer returns the Service's status unchanged, so this is the only writer of the
	// ingress IP; without it Service.Status.LoadBalancer.Ingress stays empty and the load balancer
	// appears permanently pending despite the Azure resources existing.
	if pipIPAddress == "" {
		// The Public IP create response carried no allocated address. Fail the op so it is retried;
		// the Azure resources are idempotent and a later attempt returns the allocated address.
		s.logger.V(4).Info("Public IP address unavailable, will retry to populate service status", "serviceUID", serviceUID, "correlationID", correlationID)
		s.onComplete(serviceUID, false, fmt.Errorf("public IP address unavailable for service %s", serviceUID))
		return
	}
	if err := s.diffTracker.updateServiceLoadBalancerStatus(ctx, serviceUID, pipIPAddress); err != nil {
		// The Azure resources are created and idempotent, but the Service status was not written.
		// Fail the op so the existing retry path re-runs and re-attempts the status update instead of
		// reporting a false success that would leave the service stranded without an ingress IP.
		s.logger.V(4).Info("Could not update service status with external IP, will retry", "serviceUID", serviceUID, "correlationID", correlationID, "publicIPAddress", pipIPAddress, "err", err)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to update service status with external IP: %w", err))
		return
	}
	s.logger.V(5).Info("Updated service status with external IP", "serviceUID", serviceUID, "publicIPAddress", pipIPAddress)

	// Step 6: Success callback
	s.onComplete(serviceUID, true, nil)
	s.logger.V(2).Info("Created inbound service", "serviceUID", serviceUID, "correlationID", correlationID)
}

// updateInboundService re-PUTs the LoadBalancer for an existing inbound service to apply
// configuration changes (e.g., port edits). It is idempotent: re-running with the same
// config produces no Azure changes. PIP and ServiceGateway service registration are NOT
// touched here because:
//   - the PIP allocation is independent of LB rules
//   - the SGW Service entry references the LB backend pool (whose ID is stable across
//     port edits), so the SGW state does not need to be re-pushed for port-only changes.
//
// If, in the future, port changes need to surface to NRP (e.g., for backend-port routing
// metadata in the SGW Service DTO), call updateNRPSGWServices here as well.
func (s *ServiceUpdater) updateInboundService(serviceUID string, config *InboundConfig, correlationID string) {
	s.logger.V(5).Info("Started updating inbound service", "serviceUID", serviceUID, "correlationID", correlationID)

	ctx, cancel := context.WithTimeout(s.ctx, getNRPOperationTimeout())
	defer cancel()

	// Rebuild the LB ARM model from the new config. We discard the PIP and DTO portions
	// because we are doing a port-only/spec update; the underlying PIP is unchanged and
	// the SGW service registration (which references the backend pool by ID) is stable.
	_, lbResource, _, err := buildInboundServiceResources(serviceUID, config, s.diffTracker.config)
	if err != nil {
		s.logger.V(4).Info("Could not build inbound service resources for update", "serviceUID", serviceUID, "correlationID", correlationID, "err", err)
		// Building resources only fails on deterministic, spec-driven validation errors
		// (unsupported protocol, port/idle-timeout out of range, dual-stack). Retrying the
		// same spec cannot help, so mark the failure terminal; the engine parks the service
		// (its existing Azure resources keep the last-applied config) until its spec changes.
		s.onComplete(serviceUID, false, newTerminalError(fmt.Errorf("failed to build inbound resources: %w", err)))
		return
	}

	if err := s.diffTracker.createOrUpdateLB(ctx, lbResource); err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not update LoadBalancer for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to update LoadBalancer: %w", err))
		return
	}
	lbRulesCount := 0
	if lbResource.Properties != nil && lbResource.Properties.LoadBalancingRules != nil {
		lbRulesCount = len(lbResource.Properties.LoadBalancingRules)
	}
	s.logger.V(5).Info("Updated LoadBalancer for inbound service", "serviceUID", serviceUID, "rules", lbRulesCount)

	s.onComplete(serviceUID, true, nil)
	s.logger.V(2).Info("Updated inbound service", "serviceUID", serviceUID, "correlationID", correlationID)
}

// createOutboundService creates NAT Gateway resources for outbound service
func (s *ServiceUpdater) createOutboundService(serviceUID string, config *OutboundConfig, correlationID string, triggeringPodNS string, triggeringPodName string) {
	s.logger.V(5).Info("Started creating outbound service", "serviceUID", serviceUID, "correlationID", correlationID, "pod", triggeringPodNS+"/"+triggeringPodName)

	ctx, cancel := context.WithTimeout(s.ctx, getNRPOperationTimeout())
	defer cancel()

	// Step 1: Build resources using shared helper
	pipResources, natGatewayResource, servicesDTO := buildOutboundServiceResources(serviceUID, config, s.diffTracker.config)

	// Step 2: Create every Public IP the NAT Gateway references. Creating them all before the
	// gateway keeps the retry idempotent: a partial failure here leaves addresses the next attempt
	// reuses, and the gateway is never PUT referencing an address that does not exist.
	for i := range pipResources {
		pipResource := pipResources[i]
		if err := s.diffTracker.createOrUpdatePIP(ctx, s.diffTracker.config.ResourceGroup, &pipResource); err != nil {
			httpStatus, errCode := extractAzureErrorInfo(err)
			s.logger.V(4).Info("Could not create Public IP for outbound service", "serviceUID", serviceUID, "correlationID", correlationID, "pod", triggeringPodNS+"/"+triggeringPodName, "publicIP", derefString(pipResource.Name), "httpStatus", httpStatus, "errorCode", errCode, "err", err)
			s.onComplete(serviceUID, false, fmt.Errorf("failed to create Public IP: %w", err))
			return
		}
		s.logger.V(5).Info("Created Public IP for outbound service", "serviceUID", serviceUID, "publicIP", derefString(pipResource.Name))
	}

	// Step 3: Create NAT Gateway
	if err := s.diffTracker.createOrUpdateNatGateway(ctx, s.diffTracker.config.ResourceGroup, natGatewayResource); err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not create NAT Gateway for outbound service", "serviceUID", serviceUID, "correlationID", correlationID, "pod", triggeringPodNS+"/"+triggeringPodName, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		// Don't delete PIP here - retry will use existing PIP
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create NAT Gateway: %w", err))
		return
	}
	s.logger.V(5).Info("Created NAT Gateway for outbound service", "serviceUID", serviceUID)

	// Step 4: Register service with ServiceGateway API
	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, servicesDTO); err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not register outbound service with ServiceGateway", "serviceUID", serviceUID, "correlationID", correlationID, "pod", triggeringPodNS+"/"+triggeringPodName, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		// Don't delete resources - retry will reconcile
		s.onComplete(serviceUID, false, fmt.Errorf("failed to register with ServiceGateway: %w", err))
		return
	}
	s.logger.V(5).Info("Registered outbound service with ServiceGateway", "serviceUID", serviceUID)

	// Update NRPResources to reflect the sync
	s.diffTracker.UpdateNRPNATGateways(SyncServicesReturnType{
		Additions: newIgnoreCaseSetFromSlice([]string{serviceUID}),
		Removals:  nil,
	})

	// Step 4: Success callback
	s.onComplete(serviceUID, true, nil)
	s.logger.V(2).Info("Created outbound service", "serviceUID", serviceUID, "correlationID", correlationID, "pod", triggeringPodNS+"/"+triggeringPodName)
}

// deleteInboundService deletes LoadBalancer resources
func (s *ServiceUpdater) deleteInboundService(serviceUID string, correlationID string) {
	s.logger.V(5).Info("Started deleting inbound service", "serviceUID", serviceUID, "correlationID", correlationID)

	ctx, cancel := context.WithTimeout(s.ctx, getNRPOperationTimeout())
	defer cancel()
	var lastErr error

	// Step 1: Remove backend pool references from ServiceGateway
	// This should be done before deleting the LoadBalancer to properly clean up references
	removeBackendPoolDTO := RemoveBackendPoolReferenceFromServicesDTO(
		SyncServicesReturnType{
			Additions: nil,
			Removals:  newIgnoreCaseSetFromSlice([]string{serviceUID}),
		},
		s.diffTracker.config.networkResourceSubscriptionID(),
		s.diffTracker.config.ResourceGroup,
	)

	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, removeBackendPoolDTO); err != nil {
		// Continue: the later steps are what free the resources. Logged at error level and counted
		// because nothing retries this step, the deletion is still recorded as a success, and the
		// ServiceGateway keeps a stale backend pool reference.
		recordDeleteSubstepFailure(deleteStepRemoveBackendPool)
		s.logger.Error(err, "Could not remove backend pool reference for inbound service; continuing with deletion", "serviceUID", serviceUID)
	} else {
		s.logger.V(5).Info("Removed backend pool reference for inbound service", "serviceUID", serviceUID)
	}

	// Step 2: Delete LoadBalancer
	if err := s.diffTracker.deleteLB(ctx, serviceUID); err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not delete LoadBalancer for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		lastErr = fmt.Errorf("failed to delete LoadBalancer: %w", err)
		// Continue with PIP deletion
	} else {
		s.logger.V(5).Info("Deleted LoadBalancer for inbound service", "serviceUID", serviceUID)
	}

	// Step 3: Fully unregister service from ServiceGateway
	unregisterDTO := buildServiceGatewayRemovalDTO(serviceUID, true, s.diffTracker.config)

	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, unregisterDTO); err != nil {
		// Treat 404 NotFound as success - the service is already gone from ServiceGateway
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
			s.logger.V(4).Info("Skipped already unregistered inbound service", "serviceUID", serviceUID, "httpStatus", http.StatusNotFound)
		} else {
			httpStatus, errCode := extractAzureErrorInfo(err)
			s.logger.V(4).Info("Could not unregister inbound service from ServiceGateway", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
			lastErr = fmt.Errorf("failed to unregister from ServiceGateway: %w", err)
			// Continue with PIP deletion
		}
	} else {
		s.logger.V(5).Info("Unregistered inbound service from ServiceGateway", "serviceUID", serviceUID)
	}

	// Step 4: Delete Public IP
	_, pipName, _ := buildInboundResourceNames(serviceUID)
	if err := s.diffTracker.deletePublicIP(ctx, s.diffTracker.config.ResourceGroup, pipName); err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not delete Public IP for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "publicIP", pipName, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		lastErr = fmt.Errorf("failed to delete Public IP: %w", err)
	} else {
		s.logger.V(5).Info("Deleted Public IP for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "publicIP", pipName)
	}

	// Step 5: Update NRPResources and notify completion
	if lastErr != nil {
		s.logger.V(4).Info("Could not delete inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "err", lastErr)
		s.onComplete(serviceUID, false, lastErr)
	} else {
		s.logger.V(2).Info("Deleted inbound service", "serviceUID", serviceUID, "correlationID", correlationID)
		// Update NRPResources to reflect the deletion
		s.diffTracker.UpdateNRPLoadBalancers(SyncServicesReturnType{
			Additions: nil,
			Removals:  newIgnoreCaseSetFromSlice([]string{serviceUID}),
		})

		// Step 6: Remove finalizer from K8s service to allow deletion.
		// The finalizer is the contract that keeps the Service object alive until our
		// Azure cleanup is done. We must NOT report overall success until the finalizer
		// is actually removed (or the Service is already gone): reporting success clears
		// the engine's tracking and NRP entry, after which a retried DeleteService is a
		// no-op (see DeleteService guard) and the finalizer is stranded until the next
		// CCM restart (recoverStuckFinalizers). All Azure steps above are idempotent
		// (404 is treated as success), so failing here simply retries the whole delete.
		svc, err := s.diffTracker.getServiceByUID(ctx, serviceUID)
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Service object is already gone - nothing left to finalize.
				s.logger.V(4).Info("Skipped finalizer removal for deleted service", "serviceUID", serviceUID)
				s.diffTracker.recordServiceFinalizerRecoveryDone(serviceUID)
			} else {
				// Transient lookup failure - retry the deletion rather than assume success.
				s.logger.V(4).Info("Could not look up service for finalizer removal", "serviceUID", serviceUID, "err", err)
				s.onComplete(serviceUID, false, fmt.Errorf("failed to look up service for finalizer removal: %w", err))
				return
			}
		} else {
			if err := s.diffTracker.removeServiceGatewayFinalizer(ctx, svc); err != nil {
				s.logger.V(4).Info("Could not remove finalizer from service", "serviceUID", serviceUID, "err", err)
				s.onComplete(serviceUID, false, fmt.Errorf("failed to remove ServiceGateway finalizer: %w", err))
				return
			}
			s.logger.V(5).Info("Removed finalizer from service", "serviceUID", serviceUID)
			s.diffTracker.recordServiceFinalizerRecoveryDone(serviceUID)
		}

		s.onComplete(serviceUID, true, nil)
	}
}

// deleteOutboundService deletes NAT Gateway resources
func (s *ServiceUpdater) deleteOutboundService(serviceUID string, correlationID string) {
	s.logger.V(5).Info("Started deleting outbound service", "serviceUID", serviceUID, "correlationID", correlationID)

	ctx, cancel := context.WithTimeout(s.ctx, getNRPOperationTimeout())
	defer cancel()
	var lastErr error

	// Step 1: Disassociate NAT Gateway from ServiceGateway
	if err := s.diffTracker.disassociateNatGatewayFromServiceGateway(ctx, s.diffTracker.config.ServiceGatewayResourceName, serviceUID); err != nil {
		// Continue: the later steps are what free the resources. Logged at error level and counted
		// because nothing retries this step, the deletion is still recorded as a success, and the
		// ServiceGateway keeps a stale NAT Gateway association.
		recordDeleteSubstepFailure(deleteStepDisassociateNAT)
		s.logger.Error(err, "Could not disassociate NAT Gateway from ServiceGateway; continuing with deletion", "serviceUID", serviceUID)
	} else {
		s.logger.V(5).Info("Disassociated NAT Gateway from ServiceGateway", "serviceUID", serviceUID)
	}

	// Step 2: Unregister from ServiceGateway API
	servicesDTO := buildServiceGatewayRemovalDTO(serviceUID, false, s.diffTracker.config)

	if err := s.diffTracker.updateNRPSGWServices(ctx, s.diffTracker.config.ServiceGatewayResourceName, servicesDTO); err != nil {
		// Treat 404 NotFound as success - the service is already gone from ServiceGateway
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
			s.logger.V(4).Info("Skipped already unregistered outbound service", "serviceUID", serviceUID, "httpStatus", http.StatusNotFound)
		} else {
			httpStatus, errCode := extractAzureErrorInfo(err)
			s.logger.V(4).Info("Could not unregister outbound service from ServiceGateway", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
			lastErr = fmt.Errorf("failed to unregister from ServiceGateway: %w", err)
			// Continue with deletion
		}
	} else {
		s.logger.V(5).Info("Unregistered outbound service from ServiceGateway", "serviceUID", serviceUID)
	}

	// Step 3: Delete NAT Gateway
	if err := s.diffTracker.deleteNatGateway(ctx, s.diffTracker.config.ResourceGroup, serviceUID); err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not delete NAT Gateway for outbound service", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		lastErr = fmt.Errorf("failed to delete NAT Gateway: %w", err)
		// Continue with PIP deletion
	} else {
		s.logger.V(5).Info("Deleted NAT Gateway for outbound service", "serviceUID", serviceUID)
	}

	// Step 4: Delete every Public IP this identity can own. Both names are reserved for this
	// controller, and the config that decided the families is not available here, so both are
	// always attempted. A delete for an address that was never created is a 404, which
	// deletePublicIP already treats as success.
	for _, pipName := range OutboundPublicIPNames(serviceUID) {
		if err := s.diffTracker.deletePublicIP(ctx, s.diffTracker.config.ResourceGroup, pipName); err != nil {
			httpStatus, errCode := extractAzureErrorInfo(err)
			s.logger.V(4).Info("Could not delete Public IP for outbound service", "serviceUID", serviceUID, "correlationID", correlationID, "publicIP", pipName, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
			lastErr = fmt.Errorf("failed to delete Public IP: %w", err)
		} else {
			s.logger.V(5).Info("Deleted Public IP for outbound service", "serviceUID", serviceUID, "publicIP", pipName)
		}
	}

	// Step 5: Update NRPResources and notify completion
	if lastErr != nil {
		s.logger.V(4).Info("Could not delete outbound service", "serviceUID", serviceUID, "correlationID", correlationID, "err", lastErr)
		s.onComplete(serviceUID, false, lastErr)
	} else {
		s.logger.V(2).Info("Deleted outbound service", "serviceUID", serviceUID, "correlationID", correlationID)
		// Update NRPResources to reflect the deletion
		s.diffTracker.UpdateNRPNATGateways(SyncServicesReturnType{
			Additions: nil,
			Removals:  newIgnoreCaseSetFromSlice([]string{serviceUID}),
		})

		// Remove finalizers from last-pod entries now that the NAT Gateway is deleted.
		// If that exhausts retries, report the delete as failed so it retries (the NAT/PIP
		// deletes above are idempotent on 404) instead of stranding the pod finalizer.
		if err := s.diffTracker.RemoveLastPodFinalizers(ctx, serviceUID); err != nil {
			s.logger.V(4).Info("Could not clean up last-pod finalizers for outbound service", "serviceUID", serviceUID, "correlationID", correlationID, "err", err)
			s.onComplete(serviceUID, false, err)
			return
		}

		s.onComplete(serviceUID, true, nil)
	}
}
