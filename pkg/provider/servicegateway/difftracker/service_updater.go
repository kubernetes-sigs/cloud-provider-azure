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
	"maps"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// ServiceUpdater processes service creation/deletion in parallel
type ServiceUpdater struct {
	diffTracker *DiffTracker
	onComplete  func(serviceUID string, success bool, err error)
	trigger     <-chan bool
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	semaphore   chan struct{} // Limits concurrent operations to 10
	mu          sync.Mutex    // Protects activeOperations
	// namedPublicIPLocks serializes Services resolving and creating the same named Public IP, so two of
	// them cannot both find it missing and both create it as their own.
	namedPublicIPLocks sync.Map
	// deferredReleases are Public IPs to release once the cluster name, needed to decide ownership, is
	// known. Guarded by mu.
	deferredReleases []deferredPublicIPRelease
	// pendingReleases are, per Service, the Public IPs the next delete attempt must release: one a delete could
	// not release because of a transient error, or the one a load balancer used when its delete failed (Azure
	// may still delete it, and the retry then cannot read it). Guarded by mu.
	pendingReleases map[string][]string
	// neverFrontend marks pending releases that were created for a load balancer write that failed, so
	// they never served as its frontend and are not reported as moved from. Guarded by mu.
	neverFrontend map[string]map[string]bool
	activeOps     map[string]bool // Tracks which services are being processed
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

	// Step 2: Create the Public IP, or update the one an earlier attempt created, and capture the
	// response to get the allocated IP address. A load balancer left by an earlier attempt keeps its
	// Public IP while the Service still chooses it; otherwise it moves to the chosen one and the old
	// one is released.
	var pipResponse *armnetwork.PublicIPAddress
	unlock := s.lockNamedPublicIP(config)
	target, pipResponse, oldTarget, changedPublicIP, err := s.prepareInboundPublicIP(ctx, serviceUID, config, &pipResource, true)
	unlock()
	if err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not create Public IP for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to create Public IP: %w", err))
		return
	}
	pipName := target.name
	setFrontendPublicIPID(&lbResource, s.inboundPublicIPID(target))
	s.logger.V(5).Info("Created Public IP for inbound service", "serviceUID", serviceUID, "publicIP", pipName)

	// Extract IP address from PIP response
	var pipIPAddress string
	if pipResponse != nil && pipResponse.Properties != nil && pipResponse.Properties.IPAddress != nil {
		pipIPAddress = *pipResponse.Properties.IPAddress
		s.logger.V(5).Info("Received Public IP address", "publicIP", pipName, "publicIPAddress", pipIPAddress)
	}

	// Step 3: Create LoadBalancer. The Public IP it moves off is recorded first: Azure may apply the write yet
	// return an error, after which nothing else knows it.
	if changedPublicIP {
		s.rememberPendingRelease(serviceUID, s.inboundPublicIPID(oldTarget))
	}
	if err := s.diffTracker.createOrUpdateLB(ctx, lbResource); err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not create LoadBalancer for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		// Keep the Public IP named after the Service for the retry; one created for a chosen name is removed.
		s.rollbackCreatedPublicIP(ctx, serviceUID, target)
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
	if changedPublicIP {
		s.recordServiceEvent(ctx, serviceUID, v1.EventTypeNormal, "PublicIPChanged", fmt.Sprintf(
			"load balancer frontend moved from Public IP %s to %s", oldTarget.name, target.name))
	}
	s.releasePendingInboundPublicIPs(ctx, serviceUID, s.inboundPublicIPID(target))

	// Step 6: Success callback
	s.onComplete(serviceUID, true, nil)
	s.logger.V(2).Info("Created inbound service", "serviceUID", serviceUID, "correlationID", correlationID)
}

// updateInboundService applies configuration changes to an existing inbound service.
func (s *ServiceUpdater) updateInboundService(serviceUID string, config *InboundConfig, correlationID string) {
	s.logger.V(5).Info("Started updating inbound service", "serviceUID", serviceUID, "correlationID", correlationID)

	ctx, cancel := context.WithTimeout(s.ctx, getNRPOperationTimeout())
	defer cancel()

	// Rebuild the LB ARM model from the new config. The PIP is reconciled from its current state
	// below, and the SGW service registration (which references the backend pool by ID) is stable.
	pipResource, lbResource, _, err := buildInboundServiceResources(serviceUID, config, s.diffTracker.config)
	if err != nil {
		s.logger.V(4).Info("Could not build inbound service resources for update", "serviceUID", serviceUID, "correlationID", correlationID, "err", err)
		// Building resources only fails on deterministic, spec-driven validation errors
		// (unsupported protocol, port/idle-timeout out of range, dual-stack). Retrying the
		// same spec cannot help, so mark the failure terminal; the engine parks the service
		// (its existing Azure resources keep the last-applied config) until its spec changes.
		s.onComplete(serviceUID, false, newTerminalError(fmt.Errorf("failed to build inbound resources: %w", err)))
		return
	}

	unlock := s.lockNamedPublicIP(config)
	target, pipResponse, oldTarget, changedPublicIP, err := s.prepareInboundPublicIP(ctx, serviceUID, config, &pipResource, false)
	unlock()
	if err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not update Public IP for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to update Public IP: %w", err))
		return
	}

	targetID := s.inboundPublicIPID(target)
	setFrontendPublicIPID(&lbResource, targetID)
	if changedPublicIP {
		s.rememberPendingRelease(serviceUID, s.inboundPublicIPID(oldTarget))
	}
	if err := s.diffTracker.createOrUpdateLB(ctx, lbResource); err != nil {
		httpStatus, errCode := extractAzureErrorInfo(err)
		s.logger.V(4).Info("Could not update LoadBalancer for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
		s.rollbackCreatedPublicIP(ctx, serviceUID, target)
		s.onComplete(serviceUID, false, fmt.Errorf("failed to update LoadBalancer: %w", err))
		return
	}
	lbRulesCount := 0
	if lbResource.Properties != nil && lbResource.Properties.LoadBalancingRules != nil {
		lbRulesCount = len(lbResource.Properties.LoadBalancingRules)
	}
	s.logger.V(5).Info("Updated LoadBalancer for inbound service", "serviceUID", serviceUID, "rules", lbRulesCount)

	pendingBeforeRelease := slices.DeleteFunc(s.pendingInboundPublicIPs(serviceUID), func(id string) bool {
		return strings.EqualFold(id, targetID) || s.neverServedAsFrontend(serviceUID, id)
	})
	var pipIPAddress string
	if pipResponse != nil && pipResponse.Properties != nil && pipResponse.Properties.IPAddress != nil {
		pipIPAddress = *pipResponse.Properties.IPAddress
	}
	if pipIPAddress == "" {
		s.onComplete(serviceUID, false, fmt.Errorf("public IP address unavailable for service %s", serviceUID))
		return
	}
	if err := s.diffTracker.updateServiceLoadBalancerStatus(ctx, serviceUID, pipIPAddress); err != nil {
		s.onComplete(serviceUID, false, fmt.Errorf("failed to update service status with external IP: %w", err))
		return
	}
	if changedPublicIP || len(pendingBeforeRelease) > 0 {
		oldName := ""
		if changedPublicIP {
			oldName = oldTarget.name
		} else if len(pendingBeforeRelease) > 0 {
			oldName = publicIPNameFromID(pendingBeforeRelease[0])
		}
		s.recordServiceEvent(ctx, serviceUID, v1.EventTypeNormal, "PublicIPChanged", fmt.Sprintf(
			"load balancer frontend moved from Public IP %s to %s", oldName, target.name))
	}
	s.releasePendingInboundPublicIPs(ctx, serviceUID, targetID)

	s.onComplete(serviceUID, true, nil)
	s.logger.V(2).Info("Updated inbound service", "serviceUID", serviceUID, "correlationID", correlationID)
}

// inboundPublicIP is the Public IP a Service uses. owned means the controller manages and deletes it;
// otherwise it belongs to the user and is only referenced.
type inboundPublicIP struct {
	resourceGroup string
	name          string
	existing      *armnetwork.PublicIPAddress
	owned         bool
}

func (s *ServiceUpdater) inboundPublicIPID(target *inboundPublicIP) string {
	return publicIPAddressID(s.diffTracker.config.networkResourceSubscriptionID(), target.resourceGroup, target.name)
}

// lockNamedPublicIP holds the lock of the Public IP the Service chooses by name; it returns the unlock.
func (s *ServiceUpdater) lockNamedPublicIP(config *InboundConfig) func() {
	if config == nil || config.PIPName == "" {
		return func() {}
	}
	resourceGroup := config.PIPResourceGroup
	if resourceGroup == "" {
		resourceGroup = s.diffTracker.config.ResourceGroup
	}
	lock, _ := s.namedPublicIPLocks.LoadOrStore(strings.ToLower(resourceGroup+"/"+config.PIPName), &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	return lock.(*sync.Mutex).Unlock
}

// rollbackCreatedPublicIP deletes a Public IP this attempt created for a name the Service chose when the
// load balancer could not be written, so a later change of choice does not leave it behind.
func (s *ServiceUpdater) rollbackCreatedPublicIP(ctx context.Context, serviceUID string, target *inboundPublicIP) {
	if target == nil || target.existing != nil || !target.owned {
		return
	}
	if s.isManagedPublicIPName(serviceUID, target.resourceGroup, target.name) {
		// Kept for the retry; released by the next successful write if the Service has chosen another by then.
		id := s.inboundPublicIPID(target)
		s.rememberPendingRelease(serviceUID, id)
		s.mu.Lock()
		if s.neverFrontend == nil {
			s.neverFrontend = map[string]map[string]bool{}
		}
		if s.neverFrontend[serviceUID] == nil {
			s.neverFrontend[serviceUID] = map[string]bool{}
		}
		s.neverFrontend[serviceUID][strings.ToLower(id)] = true
		s.mu.Unlock()
		return
	}
	if err := s.diffTracker.deletePublicIP(ctx, target.resourceGroup, target.name); err != nil {
		s.logger.Error(err, "Could not delete the Public IP created for a load balancer that was not updated", "serviceUID", serviceUID, "publicIP", target.name)
		s.warnService(ctx, serviceUID, "PublicIPCleanupFailed", fmt.Sprintf("Public IP %s was created but the load balancer could not use it, and it could not be deleted: %v", target.name, err))
	}
}

// inboundPublicIPInUse returns the Public IP the Service's load balancer uses and whether it matches the
// Service's current choice.
func (s *ServiceUpdater) inboundPublicIPInUse(ctx context.Context, serviceUID string, config *InboundConfig) (*inboundPublicIP, bool, error) {
	lb, err := s.diffTracker.networkClientFactory.GetLoadBalancerClient().Get(ctx, s.diffTracker.config.ResourceGroup, serviceUID, nil)
	if isNotFoundError(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to read LoadBalancer %s: %w", serviceUID, err)
	}
	id, err := arm.ParseResourceID(frontendPublicIPID(lb))
	if err != nil {
		return nil, false, nil
	}
	existing, err := s.diffTracker.networkClientFactory.GetPublicIPAddressClient().Get(ctx, id.ResourceGroupName, id.Name, nil)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read Public IP %s of LoadBalancer %s: %w", id.Name, serviceUID, err)
	}
	inUse := &inboundPublicIP{resourceGroup: id.ResourceGroupName, name: id.Name, existing: existing}
	inUse.owned = s.isManagedPublicIPName(serviceUID, inUse.resourceGroup, inUse.name) || s.ownsPublicIP(config, existing, inUse.resourceGroup)

	resourceGroup := s.diffTracker.config.ResourceGroup
	if config != nil && config.PIPResourceGroup != "" {
		resourceGroup = config.PIPResourceGroup
	}
	var chosen bool
	switch {
	case config != nil && config.PIPName != "":
		chosen = strings.EqualFold(config.PIPName, inUse.name) && strings.EqualFold(resourceGroup, inUse.resourceGroup)
	case config != nil && config.LoadBalancerIP != "":
		chosen = existing != nil && existing.Properties != nil && net.ParseIP(derefString(existing.Properties.IPAddress)).Equal(net.ParseIP(config.LoadBalancerIP))
	default:
		chosen = s.isManagedPublicIPName(serviceUID, inUse.resourceGroup, inUse.name)
	}
	return inUse, chosen, nil
}

func (s *ServiceUpdater) prepareInboundPublicIP(ctx context.Context, serviceUID string, config *InboundConfig, pip *armnetwork.PublicIPAddress, forceIfNotReady bool) (*inboundPublicIP, *armnetwork.PublicIPAddress, *inboundPublicIP, bool, error) {
	inUse, chosen, err := s.inboundPublicIPInUse(ctx, serviceUID, config)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if inUse == nil {
		target, err := s.resolveInboundPublicIP(ctx, serviceUID, config)
		if err != nil {
			return nil, nil, nil, false, err
		}
		var version *armnetwork.IPVersion
		if pip != nil && pip.Properties != nil {
			version = pip.Properties.PublicIPAddressVersion
		}
		if err := s.recreateOwnedPublicIPForPrefixChange(ctx, serviceUID, config, target, version); err != nil {
			return nil, nil, nil, false, err
		}
		response, err := s.ensureInboundPublicIP(ctx, serviceUID, config, pip, target, forceIfNotReady)
		return target, response, nil, false, err
	}
	if chosen {
		if s.publicIPPrefixChangeRejected(config, inUse) {
			address := ""
			if inUse.existing != nil && inUse.existing.Properties != nil {
				address = derefString(inUse.existing.Properties.IPAddress)
			}
			return nil, nil, nil, false, newTerminalError(fmt.Errorf(
				"the Public IP prefix of an existing Service cannot be changed when ServiceGateway is enabled; the Service keeps Public IP %s (%s); revert the annotation or recreate the Service",
				inUse.name, address))
		}
		response, err := s.ensureInboundPublicIP(ctx, serviceUID, config, pip, inUse, forceIfNotReady)
		return inUse, response, nil, false, err
	}

	target, err := s.resolveInboundPublicIP(ctx, serviceUID, config)
	if err != nil {
		return nil, nil, nil, false, err
	}
	response, err := s.ensureInboundPublicIP(ctx, serviceUID, config, pip, target, true)
	if err != nil {
		return nil, nil, nil, false, err
	}
	return target, response, inUse, !strings.EqualFold(s.inboundPublicIPID(inUse), s.inboundPublicIPID(target)), nil
}

func (s *ServiceUpdater) publicIPPrefixChangeRejected(config *InboundConfig, inUse *inboundPublicIP) bool {
	if config == nil || config.PIPPrefixID == "" || inUse == nil || !inUse.owned {
		return false
	}
	current := ""
	if inUse.existing != nil && inUse.existing.Properties != nil && inUse.existing.Properties.PublicIPPrefix != nil {
		current = derefString(inUse.existing.Properties.PublicIPPrefix.ID)
	}
	return !strings.EqualFold(current, config.PIPPrefixID)
}

func (s *ServiceUpdater) recreateOwnedPublicIPForPrefixChange(ctx context.Context, serviceUID string, config *InboundConfig, target *inboundPublicIP, version *armnetwork.IPVersion) error {
	if config == nil || config.PIPPrefixID == "" || target == nil || target.existing == nil || !target.owned {
		return nil
	}
	pip := target.existing
	current := ""
	if pip.Properties != nil {
		if pip.Properties.PublicIPPrefix != nil {
			current = derefString(pip.Properties.PublicIPPrefix.ID)
		}
		if pip.Properties.IPConfiguration != nil || pip.Properties.NatGateway != nil {
			return nil
		}
	}
	if strings.EqualFold(current, config.PIPPrefixID) {
		return nil
	}
	if err := s.checkPublicIPPrefix(ctx, config.PIPPrefixID, version); err != nil {
		return err
	}
	if err := s.diffTracker.deletePublicIP(ctx, target.resourceGroup, target.name); err != nil {
		return fmt.Errorf("failed to delete Public IP %s before recreating it from prefix %s: %w", target.name, config.PIPPrefixID, err)
	}
	s.logger.V(2).Info("Deleted owned Public IP so it can be recreated from the requested prefix", "serviceUID", serviceUID, "publicIP", target.name, "prefix", config.PIPPrefixID)
	target.existing = nil
	target.owned = true
	return nil
}

func isNotFoundError(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}

func (s *ServiceUpdater) ownsPublicIP(config *InboundConfig, pip *armnetwork.PublicIPAddress, resourceGroup string) bool {
	return config != nil && ownsPublicIPByTags(pip, config.ServiceName, s.clusterNameFor(config, pip, resourceGroup))
}

// clusterNameFor returns the cluster name to judge the Public IP's ownership tags by. It is not known yet at
// startup; as in releasePublicIP, a cluster tag in the cluster resource group is then this cluster's, since
// only this cluster's controller writes Public IPs there.
func (s *ServiceUpdater) clusterNameFor(config *InboundConfig, pip *armnetwork.PublicIPAddress, resourceGroup string) string {
	if name := s.clusterName(config); name != "" || !strings.EqualFold(resourceGroup, s.diffTracker.config.ResourceGroup) {
		return name
	}
	return clusterOwnershipTag(pip)
}

func (s *ServiceUpdater) clusterName(config *InboundConfig) string {
	if config != nil && config.ClusterName != "" {
		return config.ClusterName
	}
	return s.diffTracker.getClusterName()
}

// isManagedPublicIPName reports the Public IP the controller names after the Service.
func (s *ServiceUpdater) isManagedPublicIPName(serviceUID, resourceGroup, name string) bool {
	return strings.EqualFold(name, PublicIPName(serviceUID)) && strings.EqualFold(resourceGroup, s.diffTracker.config.ResourceGroup)
}

func (s *ServiceUpdater) warnService(ctx context.Context, serviceUID, reason, message string) {
	s.recordServiceEvent(ctx, serviceUID, v1.EventTypeWarning, reason, message)
}

func (s *ServiceUpdater) recordServiceEvent(ctx context.Context, serviceUID, eventType, reason, message string) {
	if svc, err := s.diffTracker.getServiceByUID(ctx, serviceUID); err == nil {
		s.diffTracker.recordEvent(svc, eventType, reason, message)
	}
}

// resolveInboundPublicIP finds the Public IP the Service asks for: by name, by address, or the one named
// after the Service. A named Public IP that does not exist yet is created and owned by the controller.
func (s *ServiceUpdater) resolveInboundPublicIP(ctx context.Context, serviceUID string, config *InboundConfig) (*inboundPublicIP, error) {
	target := &inboundPublicIP{resourceGroup: s.diffTracker.config.ResourceGroup, name: PublicIPName(serviceUID)}
	if config != nil && config.PIPResourceGroup != "" {
		target.resourceGroup = config.PIPResourceGroup
	}
	client := s.diffTracker.networkClientFactory.GetPublicIPAddressClient()
	switch {
	case config != nil && config.PIPName != "":
		target.name = config.PIPName
	case config != nil && config.LoadBalancerIP != "":
		pips, err := client.List(ctx, target.resourceGroup)
		if err != nil {
			return nil, fmt.Errorf("failed to list Public IPs in resource group %s: %w", target.resourceGroup, err)
		}
		for _, pip := range pips {
			if pip != nil && pip.Name != nil && pip.Properties != nil && net.ParseIP(derefString(pip.Properties.IPAddress)).Equal(net.ParseIP(config.LoadBalancerIP)) {
				target.name, target.existing = *pip.Name, pip
				target.owned = s.ownsPublicIP(config, pip, target.resourceGroup)
				return target, nil
			}
		}
		message := fmt.Sprintf("no Public IP with address %s exists in resource group %s", config.LoadBalancerIP, target.resourceGroup)
		s.warnService(ctx, serviceUID, "PublicIPNotFound", message)
		return nil, errors.New(message)
	default:
		target.resourceGroup = s.diffTracker.config.ResourceGroup
	}

	existing, err := client.Get(ctx, target.resourceGroup, target.name, nil)
	switch {
	case isNotFoundError(err):
		target.owned = true
		return target, nil
	case err != nil:
		return nil, err
	case existing == nil:
		return nil, fmt.Errorf("public IP %s not found", target.name)
	}
	target.existing = existing
	target.owned = s.isManagedPublicIPName(serviceUID, target.resourceGroup, target.name) || s.ownsPublicIP(config, existing, target.resourceGroup)
	return target, nil
}

// checkExistingPublicIP rejects an existing Public IP the load balancer cannot use. A Public IP the user
// owns is only referenced, so settings that would change it are rejected, and it must be free.
func (s *ServiceUpdater) checkExistingPublicIP(ctx context.Context, serviceUID string, config *InboundConfig, target *inboundPublicIP, version *armnetwork.IPVersion) error {
	pip := target.existing
	if pip.SKU != nil && pip.SKU.Name != nil && *pip.SKU.Name != armnetwork.PublicIPAddressSKUNameStandardV2 {
		return newTerminalError(fmt.Errorf("public IP %s has SKU %s; ServiceGateway needs a StandardV2 Public IP", target.name, *pip.SKU.Name))
	}
	if pip.Properties != nil && pip.Properties.PublicIPAddressVersion != nil && version != nil && *pip.Properties.PublicIPAddressVersion != *version {
		return newTerminalError(fmt.Errorf("public IP %s is %s but the Service needs %s", target.name, *pip.Properties.PublicIPAddressVersion, *version))
	}
	normalize := func(location string) string { return strings.ReplaceAll(strings.ToLower(location), " ", "") }
	if pip.Location != nil && normalize(*pip.Location) != normalize(s.diffTracker.config.Location) {
		return newTerminalError(fmt.Errorf("public IP %s is in %s but the cluster is in %s", target.name, *pip.Location, s.diffTracker.config.Location))
	}
	var usedBy string
	// owner is the Service (UID or namespace/name) that holds the Public IP, when it is another Service.
	owner := ""
	if pip.Properties != nil && pip.Properties.IPConfiguration != nil {
		lbFrontends := strings.ToLower(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/",
			s.diffTracker.config.networkResourceSubscriptionID(), s.diffTracker.config.ResourceGroup))
		id := derefString(pip.Properties.IPConfiguration.ID)
		rest, inClusterRG := strings.CutPrefix(strings.ToLower(id), lbFrontends)
		lbName, _, isFrontend := strings.Cut(rest, "/frontendipconfigurations/")
		if !inClusterRG || !isFrontend || !strings.EqualFold(lbName, serviceUID) {
			usedBy = id
			if inClusterRG && isFrontend && isValidServiceUUID(lbName) {
				owner = lbName
			}
		}
	}
	if pip.Properties != nil && pip.Properties.NatGateway != nil {
		usedBy = derefString(pip.Properties.NatGateway.ID)
	}
	// The managed Public IP of another Service or egress identity is never taken over, even while unattached:
	// its own delete removes it by name.
	if identity, ok := identityFromPublicIPName(target.name); ok && usedBy == "" && !strings.EqualFold(identity, serviceUID) &&
		strings.EqualFold(target.resourceGroup, s.diffTracker.config.ResourceGroup) && (isValidServiceUUID(identity) || taggedForEgressIdentity(pip, identity)) {
		usedBy = identity
		if isValidServiceUUID(identity) {
			owner = identity
		}
	}
	// So is one the controller created for another Service of this cluster: that Service's delete releases it.
	if usedBy == "" && !target.owned && ownedByClusterTags(pip, s.clusterNameFor(config, pip, target.resourceGroup)) {
		usedBy, owner = "another Service of this cluster", publicIPServiceTag(pip)
	}
	// Each Service has its own load balancer, so a Public IP cannot be shared by two Services. The Service is
	// not provisioned while the other one keeps the Public IP, and is retried so it can take it once released.
	if owner != "" && s.diffTracker.serviceKeepsLoadBalancer(ctx, owner, serviceUID) {
		message := fmt.Sprintf("public IP %s is already used by %s; several Services cannot share a Public IP when ServiceGateway is enabled", target.name, usedBy)
		s.warnService(ctx, serviceUID, "SharedPublicIPNotSupported", message)
		return errors.New(message)
	}
	if usedBy != "" {
		message := fmt.Sprintf("public IP %s is already used by %s", target.name, usedBy)
		s.warnService(ctx, serviceUID, "PublicIPInUse", message)
		return errors.New(message)
	}
	if target.owned {
		return nil
	}

	if config != nil {
		var settings []string
		if len(config.PIPTags) > 0 {
			settings = append(settings, consts.ServiceAnnotationAzurePIPTags)
		}
		if config.IPTags != nil {
			settings = append(settings, consts.ServiceAnnotationIPTagsForPublicIP)
		}
		if config.DNSLabel != nil {
			settings = append(settings, consts.ServiceAnnotationDNSLabelName)
		}
		if config.PIPPrefixID != "" {
			settings = append(settings, "Public IP prefix")
		}
		if len(settings) > 0 {
			return newTerminalError(fmt.Errorf("public IP %s was not created for this Service, so it is not changed; remove %s", target.name, strings.Join(settings, ", ")))
		}
	}

	if pip.Properties == nil || derefString(pip.Properties.IPAddress) == "" ||
		(pip.Properties.ProvisioningState != nil && *pip.Properties.ProvisioningState != armnetwork.ProvisioningStateSucceeded) {
		return fmt.Errorf("public IP %s has no allocated address yet", target.name)
	}
	return nil
}

// updateInboundPublicIP applies tag, IP tag and DNS label changes to an existing Public IP and returns it.
// Tags are only added or updated, and the prefix keeps the value the Public IP was created with.
// force writes the Public IP even when nothing changed.
func (s *ServiceUpdater) updateInboundPublicIP(ctx context.Context, _ string, config *InboundConfig, target *inboundPublicIP, force bool) (*armnetwork.PublicIPAddress, error) {
	pip := target.existing
	if pip.Properties == nil {
		pip.Properties = &armnetwork.PublicIPAddressPropertiesFormat{}
	}

	changed := false
	if pip.Tags == nil {
		pip.Tags = map[string]*string{}
	}
	for key, value := range inboundPublicIPTags(config) {
		for existing := range pip.Tags {
			if strings.EqualFold(existing, key) {
				key = existing
				break
			}
		}
		if current := pip.Tags[key]; current == nil || *current != *value {
			pip.Tags[key] = value
			changed = true
		}
	}

	if config != nil && config.DNSLabel != nil {
		current := ""
		if pip.Properties.DNSSettings != nil {
			current = derefString(pip.Properties.DNSSettings.DomainNameLabel)
		}
		if current != *config.DNSLabel {
			if *config.DNSLabel == "" {
				pip.Properties.DNSSettings = nil
			} else {
				if pip.Properties.DNSSettings == nil {
					pip.Properties.DNSSettings = &armnetwork.PublicIPAddressDNSSettings{}
				}
				pip.Properties.DNSSettings.DomainNameLabel = to.Ptr(*config.DNSLabel)
			}
			changed = true
		}
	}

	if config != nil && config.IPTags != nil && !maps.Equal(ipTagMap(pip.Properties.IPTags), config.IPTags) {
		if pip.Properties.PublicIPPrefix != nil {
			return nil, newTerminalError(errors.New("the IP tags of a Public IP allocated from a prefix cannot be changed; it takes the prefix's IP tags"))
		}
		if blocked := unsupportedExistingPublicIPTagChanges(ipTagMap(pip.Properties.IPTags), config.IPTags); len(blocked) > 0 {
			return nil, newTerminalError(fmt.Errorf("only FirstPartyUsage IP tags can be changed on an existing Public IP; %s cannot", strings.Join(blocked, ", ")))
		}
		pip.Properties.IPTags = ipTagsFromMap(config.IPTags)
		changed = true
	}

	if !changed && !force {
		return pip, nil
	}
	return s.diffTracker.createOrUpdatePIPWithResponse(ctx, target.resourceGroup, pip)
}

// ensureInboundPublicIP makes the resolved Public IP ready for the load balancer. A missing one is created
// from pip; an owned one is updated, and rewritten when forceIfNotReady finds it failed or without an
// address; a user's one is checked and left unchanged.
func (s *ServiceUpdater) ensureInboundPublicIP(ctx context.Context, serviceUID string, config *InboundConfig, pip *armnetwork.PublicIPAddress, target *inboundPublicIP, forceIfNotReady bool) (*armnetwork.PublicIPAddress, error) {
	if existing := target.existing; existing != nil {
		if err := s.checkExistingPublicIP(ctx, serviceUID, config, target, pip.Properties.PublicIPAddressVersion); err != nil {
			return nil, err
		}
		if !target.owned {
			return existing, nil
		}
		ready := existing.Properties != nil && derefString(existing.Properties.IPAddress) != "" &&
			(existing.Properties.ProvisioningState == nil || *existing.Properties.ProvisioningState == armnetwork.ProvisioningStateSucceeded)
		return s.updateInboundPublicIP(ctx, serviceUID, config, target, forceIfNotReady && !ready)
	}
	if config != nil && config.PIPPrefixID != "" {
		if err := s.checkPublicIPPrefix(ctx, config.PIPPrefixID, pip.Properties.PublicIPAddressVersion); err != nil {
			return nil, err
		}
	}
	pip.Name = to.Ptr(target.name)
	pip.ID = to.Ptr(s.inboundPublicIPID(target))
	return s.diffTracker.createOrUpdatePIPWithResponse(ctx, target.resourceGroup, pip)
}

type deferredPublicIPRelease struct {
	serviceUID, serviceName, publicIPID string
}

var errClusterNameUnknown = errors.New("the cluster name is not known yet")

// releaseOrDefer releases the Public IP, or queues it when its ownership needs the cluster name, which the
// controller only learns from its first load balancer call after a restart.
func (s *ServiceUpdater) releaseOrDefer(ctx context.Context, serviceUID, serviceName, clusterName, publicIPID string) error {
	err := s.releasePublicIP(ctx, serviceUID, serviceName, clusterName, publicIPID)
	if !errors.Is(err, errClusterNameUnknown) {
		return err
	}
	s.mu.Lock()
	s.deferredReleases = append(s.deferredReleases, deferredPublicIPRelease{serviceUID, serviceName, publicIPID})
	s.mu.Unlock()
	s.logger.Info("Deferred releasing a Public IP until the cluster name is known from a load balancer call", "serviceUID", serviceUID, "publicIP", publicIPID)
	// The name may have arrived, and the queue been drained, while the Public IP was being read.
	if s.diffTracker.getClusterName() != "" {
		s.releaseDeferredPublicIPs()
	}
	return nil
}

// releaseDeferredPublicIPs releases the Public IPs queued while the cluster name was unknown.
func (s *ServiceUpdater) releaseDeferredPublicIPs() {
	s.mu.Lock()
	pending := s.deferredReleases
	s.deferredReleases = nil
	s.mu.Unlock()
	clusterName := s.diffTracker.getClusterName()
	var retry []deferredPublicIPRelease
	for _, release := range pending {
		ctx, cancel := context.WithTimeout(s.ctx, getNRPOperationTimeout())
		if err := s.releasePublicIP(ctx, release.serviceUID, release.serviceName, clusterName, release.publicIPID); err != nil {
			s.logger.Error(err, "Could not delete a Public IP the Service no longer uses", "serviceUID", release.serviceUID, "publicIP", release.publicIPID)
			s.warnService(ctx, release.serviceUID, "PublicIPCleanupFailed", fmt.Sprintf("Public IP %s could not be checked or deleted: %v", release.publicIPID, err))
			if isTransientAzureError(err) {
				retry = append(retry, release)
			}
		}
		cancel()
	}
	if len(retry) == 0 {
		return
	}
	// The Service's delete has already completed, so nothing else retries these.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deferredReleases = append(s.deferredReleases, retry...)
	if s.retryTimers == nil {
		s.retryTimers = make(map[string]*time.Timer)
	}
	if _, armed := s.retryTimers[deferredReleasesRetryKey]; !armed {
		s.retryTimers[deferredReleasesRetryKey] = time.AfterFunc(deferredReleaseRetryDelay, func() {
			s.mu.Lock()
			delete(s.retryTimers, deferredReleasesRetryKey)
			s.mu.Unlock()
			if s.ctx != nil && s.ctx.Err() != nil {
				return
			}
			s.releaseDeferredPublicIPs()
		})
	}
}

// deferredReleasesRetryKey keys the retry of deferred releases in retryTimers, so Stop also stops it.
const deferredReleasesRetryKey = "deferred-public-ip-releases"

// deferredReleaseRetryDelay is how long a deferred release that failed transiently waits before it is retried.
var deferredReleaseRetryDelay = 30 * time.Second

// releasePublicIP deletes the Public IP when the controller owns it for the Service: the one named after
// it, or one whose ownership tags name it. With the Service gone (serviceName empty), the tags must name
// a Service of this cluster.
func (s *ServiceUpdater) releasePublicIP(ctx context.Context, serviceUID, serviceName, clusterName, publicIPID string) error {
	id, err := arm.ParseResourceID(publicIPID)
	if err != nil || !strings.EqualFold(id.SubscriptionID, s.diffTracker.config.networkResourceSubscriptionID()) {
		return nil
	}
	if !s.isManagedPublicIPName(serviceUID, id.ResourceGroupName, id.Name) {
		pip, err := s.diffTracker.networkClientFactory.GetPublicIPAddressClient().Get(ctx, id.ResourceGroupName, id.Name, nil)
		if isNotFoundError(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if tagged := clusterOwnershipTag(pip); clusterName == "" && tagged != "" {
			// Only this cluster's controller writes Public IPs into the cluster resource group, so a
			// cluster tag there is this cluster's name; elsewhere it cannot be decided yet.
			if !strings.EqualFold(id.ResourceGroupName, s.diffTracker.config.ResourceGroup) {
				return errClusterNameUnknown
			}
			clusterName = tagged
		}
		owned := ownsPublicIPByTags(pip, serviceName, clusterName)
		if serviceName == "" {
			owned = ownedByClusterTags(pip, clusterName)
		}
		if !owned {
			return nil
		}
	}
	if err := s.diffTracker.deletePublicIP(ctx, id.ResourceGroupName, id.Name); err != nil {
		return err
	}
	s.logger.V(2).Info("Deleted the Public IP the Service used", "serviceUID", serviceUID, "publicIP", publicIPID)
	return nil
}

func publicIPNameFromID(publicIPID string) string {
	id, err := arm.ParseResourceID(publicIPID)
	if err != nil {
		return publicIPID
	}
	return id.Name
}

func (s *ServiceUpdater) neverServedAsFrontend(serviceUID, publicIPID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.neverFrontend[serviceUID][strings.ToLower(publicIPID)]
}

func (s *ServiceUpdater) pendingInboundPublicIPs(serviceUID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.pendingReleases[serviceUID])
}

func (s *ServiceUpdater) releasePendingInboundPublicIPs(ctx context.Context, serviceUID, keepID string) {
	ids := s.pendingInboundPublicIPs(serviceUID)
	if len(ids) == 0 {
		return
	}
	svc, err := s.diffTracker.getServiceByUID(ctx, serviceUID)
	if err != nil {
		return
	}
	serviceName := svc.Namespace + "/" + svc.Name
	var retry []string
	for i, id := range ids {
		if id == "" || strings.EqualFold(id, keepID) || slices.ContainsFunc(ids[:i], func(earlier string) bool { return strings.EqualFold(earlier, id) }) {
			continue
		}
		if err := s.releaseOrDefer(ctx, serviceUID, serviceName, s.diffTracker.getClusterName(), id); err != nil {
			s.diffTracker.recordEvent(svc, v1.EventTypeWarning, "PublicIPCleanupFailed", fmt.Sprintf("Public IP %s could not be checked or deleted: %v", id, err))
			if isTransientAzureError(err) {
				retry = append(retry, id)
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(retry) == 0 {
		delete(s.pendingReleases, serviceUID)
		delete(s.neverFrontend, serviceUID)
		return
	}
	if s.pendingReleases == nil {
		s.pendingReleases = map[string][]string{}
	}
	s.pendingReleases[serviceUID] = retry
}

// releaseInboundPublicIPs deletes, after the load balancer, the Public IPs besides the one named after the
// Service that the controller owns: the one the load balancer used, the one the Service chooses by name, and
// those an earlier attempt could not release. A transient failure is returned so the delete is retried; any
// other failure (for example the Public IP is in use elsewhere, or access is denied) is reported and does not
// hold the Service's deletion.
func (s *ServiceUpdater) releaseInboundPublicIPs(ctx context.Context, serviceUID, frontendID string) error {
	svc, err := s.diffTracker.getServiceByUID(ctx, serviceUID)
	if err != nil && !apierrors.IsNotFound(err) {
		// The Service may still name a Public IP to release; retry rather than skip it.
		s.rememberPendingRelease(serviceUID, frontendID)
		return fmt.Errorf("failed to look up service to release its Public IPs: %w", err)
	}
	if err != nil {
		svc = nil
	}
	serviceName := ""
	ids := []string{frontendID}
	var lastErr error
	if svc != nil {
		serviceName = svc.Namespace + "/" + svc.Name
		if config := ExtractInboundConfigFromService(svc); config != nil {
			if config.PIPName != "" {
				resourceGroup := s.diffTracker.config.ResourceGroup
				if config.PIPResourceGroup != "" {
					resourceGroup = config.PIPResourceGroup
				}
				ids = append(ids, s.inboundPublicIPID(&inboundPublicIP{resourceGroup: resourceGroup, name: config.PIPName}))
			}
			// The startup sweep only covers the cluster resource group, so a Public IP a move left behind in the
			// Service's own resource group, while the controller restarted, is found here by its tags.
			if config.PIPResourceGroup != "" && !strings.EqualFold(config.PIPResourceGroup, s.diffTracker.config.ResourceGroup) {
				leftovers, err := s.taggedUnattachedPublicIPs(ctx, config.PIPResourceGroup, serviceName)
				if err != nil {
					s.logger.Error(err, "Could not list the Public IPs the Service may have left", "serviceUID", serviceUID, "resourceGroup", config.PIPResourceGroup)
					if isTransientAzureError(err) {
						lastErr = err
					}
				}
				ids = append(ids, leftovers...)
			}
		}
	}
	s.mu.Lock()
	ids = append(ids, s.pendingReleases[serviceUID]...)
	s.mu.Unlock()

	var retry []string
	for i, id := range ids {
		if id == "" || slices.ContainsFunc(ids[:i], func(earlier string) bool { return strings.EqualFold(earlier, id) }) {
			continue
		}
		if err := s.releaseOrDefer(ctx, serviceUID, serviceName, s.diffTracker.getClusterName(), id); err != nil {
			s.logger.Error(err, "Could not delete the Public IP the Service used", "serviceUID", serviceUID, "publicIP", id)
			if svc != nil {
				s.diffTracker.recordEvent(svc, v1.EventTypeWarning, "PublicIPCleanupFailed", fmt.Sprintf("Public IP %s could not be checked or deleted: %v", id, err))
			}
			if isTransientAzureError(err) {
				retry = append(retry, id)
				lastErr = err
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(retry) == 0 {
		delete(s.pendingReleases, serviceUID)
		delete(s.neverFrontend, serviceUID)
		if lastErr != nil {
			return fmt.Errorf("failed to list Public IPs to release: %w", lastErr)
		}
		return nil
	}
	if s.pendingReleases == nil {
		s.pendingReleases = map[string][]string{}
	}
	s.pendingReleases[serviceUID] = retry
	return fmt.Errorf("failed to delete Public IP %s: %w", retry[0], lastErr)
}

// taggedUnattachedPublicIPs returns the unattached Public IPs in the resource group whose ownership tags name
// the Service and a cluster; releasePublicIP decides whether that cluster is this one.
func (s *ServiceUpdater) taggedUnattachedPublicIPs(ctx context.Context, resourceGroup, serviceName string) ([]string, error) {
	pips, err := s.diffTracker.networkClientFactory.GetPublicIPAddressClient().List(ctx, resourceGroup)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, pip := range pips {
		if pip == nil || pip.Name == nil || publicIPAttached(pip) || clusterOwnershipTag(pip) == "" || !ownsPublicIPByTags(pip, serviceName, clusterOwnershipTag(pip)) {
			continue
		}
		ids = append(ids, s.inboundPublicIPID(&inboundPublicIP{resourceGroup: resourceGroup, name: *pip.Name}))
	}
	return ids, nil
}

func publicIPAttached(pip *armnetwork.PublicIPAddress) bool {
	return pip.Properties != nil && (pip.Properties.IPConfiguration != nil || pip.Properties.NatGateway != nil)
}

// rememberPendingRelease records a Public IP for the Service's next successful update or delete to release.
func (s *ServiceUpdater) rememberPendingRelease(serviceUID, publicIPID string) {
	if publicIPID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.neverFrontend[serviceUID], strings.ToLower(publicIPID))
	if slices.ContainsFunc(s.pendingReleases[serviceUID], func(id string) bool { return strings.EqualFold(id, publicIPID) }) {
		return
	}
	if s.pendingReleases == nil {
		s.pendingReleases = map[string][]string{}
	}
	s.pendingReleases[serviceUID] = append(s.pendingReleases[serviceUID], publicIPID)
}

// isTransientAzureError reports an error that may clear by itself: throttling, a server error, or no
// response at all (timeout, connection error).
func isTransientAzureError(err error) bool {
	httpStatus, _ := extractAzureErrorInfo(err)
	return httpStatus == 0 || httpStatus == http.StatusTooManyRequests || httpStatus >= http.StatusInternalServerError
}

// checkPublicIPPrefix fails terminally when the prefix can never provide the Public IP, so the Service
// is reported instead of retrying a create Azure rejects. A prefix in another subscription is left to
// Azure to validate.
func (s *ServiceUpdater) checkPublicIPPrefix(ctx context.Context, prefixID string, version *armnetwork.IPVersion) error {
	id, err := arm.ParseResourceID(prefixID)
	if err != nil {
		return newTerminalError(fmt.Errorf("invalid Public IP prefix %s: %w", prefixID, err))
	}
	if !strings.EqualFold(id.SubscriptionID, s.diffTracker.config.networkResourceSubscriptionID()) {
		return nil
	}
	prefix, err := s.diffTracker.networkClientFactory.GetPublicIPPrefixClient().Get(ctx, id.ResourceGroupName, id.Name, nil)
	if err != nil {
		return fmt.Errorf("failed to read Public IP prefix %s: %w", prefixID, err)
	}
	if prefix == nil {
		return fmt.Errorf("public IP prefix %s not found", prefixID)
	}
	if prefix.SKU == nil || prefix.SKU.Name == nil || *prefix.SKU.Name != armnetwork.PublicIPPrefixSKUNameStandardV2 {
		sku := "unknown"
		if prefix.SKU != nil && prefix.SKU.Name != nil {
			sku = string(*prefix.SKU.Name)
		}
		return newTerminalError(fmt.Errorf("public IP prefix %s has SKU %s; ServiceGateway needs a StandardV2 prefix", prefixID, sku))
	}
	if prefix.Properties != nil && prefix.Properties.PublicIPAddressVersion != nil && version != nil && *prefix.Properties.PublicIPAddressVersion != *version {
		return newTerminalError(fmt.Errorf("public IP prefix %s is %s but the Service needs %s", prefixID, *prefix.Properties.PublicIPAddressVersion, *version))
	}
	normalize := func(location string) string { return strings.ReplaceAll(strings.ToLower(location), " ", "") }
	if prefix.Location != nil && normalize(*prefix.Location) != normalize(s.diffTracker.config.Location) {
		return newTerminalError(fmt.Errorf("public IP prefix %s is in %s but the cluster is in %s", prefixID, *prefix.Location, s.diffTracker.config.Location))
	}
	return nil
}

func ipTagMap(ipTags []*armnetwork.IPTag) map[string]string {
	tags := map[string]string{}
	for _, tag := range ipTags {
		if tag != nil {
			tags[derefString(tag.IPTagType)] = derefString(tag.Tag)
		}
	}
	return tags
}

func unsupportedExistingPublicIPTagChanges(current, desired map[string]string) []string {
	changed := map[string]struct{}{}
	for typ, value := range desired {
		if cur, ok := current[typ]; !ok || cur != value {
			changed[typ] = struct{}{}
		}
	}
	for typ := range current {
		if _, ok := desired[typ]; !ok {
			changed[typ] = struct{}{}
		}
	}
	var blocked []string
	for typ := range changed {
		if !strings.EqualFold(typ, "FirstPartyUsage") {
			blocked = append(blocked, typ)
		}
	}
	slices.Sort(blocked)
	return blocked
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

	// Step 2: Delete LoadBalancer, remembering the Public IP it used. If it cannot be read it is kept, so the
	// retried delete still learns which Public IP to release.
	var frontendID string
	lbDeleted := false
	current, err := s.diffTracker.networkClientFactory.GetLoadBalancerClient().Get(ctx, s.diffTracker.config.ResourceGroup, serviceUID, nil)
	switch {
	case err != nil && !isNotFoundError(err):
		s.logger.V(4).Info("Could not read LoadBalancer for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "err", err)
		lastErr = fmt.Errorf("failed to read LoadBalancer: %w", err)
	case err == nil && current != nil:
		frontendID = frontendPublicIPID(current)
		fallthrough
	default:
		if err := s.diffTracker.deleteLB(ctx, serviceUID); err != nil {
			httpStatus, errCode := extractAzureErrorInfo(err)
			s.logger.V(4).Info("Could not delete LoadBalancer for inbound service", "serviceUID", serviceUID, "correlationID", correlationID, "httpStatus", httpStatus, "errorCode", errCode, "err", err)
			lastErr = fmt.Errorf("failed to delete LoadBalancer: %w", err)
			// Azure may still delete it (a timeout while waiting for the operation), and the retry then cannot read
			// which Public IP it used.
			s.rememberPendingRelease(serviceUID, frontendID)
		} else {
			lbDeleted = true
			s.logger.V(5).Info("Deleted LoadBalancer for inbound service", "serviceUID", serviceUID)
		}
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

	if lbDeleted {
		if err := s.releaseInboundPublicIPs(ctx, serviceUID, frontendID); err != nil {
			lastErr = err
		}
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
