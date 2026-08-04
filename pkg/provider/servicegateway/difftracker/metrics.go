/*
Copyright 2024 The Kubernetes Authors.

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
	"strconv"
	"sync"
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	diffTrackerSubsystem = "difftracker_engine"
)

var (
	// registerOnce ensures metrics are only registered once (when ServiceGatewayEnabled=true)
	registerOnce sync.Once

	// ========================================================================
	// Metrics from SLB Observability Guide — Section 1.1
	// ========================================================================

	// pendingServiceOperations tracks how many LB/NAT creations or deletions are currently in progress.
	pendingServiceOperations = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "pending_service_operations",
			Help:           "Number of LB/NAT Gateway creation or deletion operations currently in progress, by state and service type",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"state", "service_type"},
	)

	// serviceOperationTotal counts service operation ATTEMPTS, with error code for triage. A retried
	// operation records one error per failed attempt and then a success, so one logical create that
	// recovers from a transient failure appears here as several errors plus one success. A success
	// rate computed from these labels is therefore an attempt success rate, not a service one.
	serviceOperationTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_operations_total",
			Help:           "Total count of service creation/deletion operation attempts by result, error code, and orphan status; a retried operation records one attempt per try",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation", "service_type", "result", "error_code", "is_orphan"},
	)

	// serviceOperationDuration tracks how long a single LB/NAT create/delete ATTEMPT takes. The
	// operation clock restarts on each dispatch, so a retried operation reports only its final
	// attempt rather than the elapsed time a user waited.
	serviceOperationDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_operation_duration_seconds",
			Help:           "Duration of a single service creation/deletion attempt in seconds; a retried operation reports only its final attempt",
			Buckets:        []float64{0.1, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0, 60.0, 120.0},
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation", "service_type"},
	)

	// serviceOperationRetries records, once per completed service operation, how many retries it
	// needed. It is observed only at a terminal outcome (success, terminal-error park, or retry
	// budget exhausted), so a first-attempt success contributes an explicit 0.
	serviceOperationRetries = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_operation_retries",
			Help:           "Retries performed before a service operation reached a terminal outcome",
			Buckets:        []float64{0, 1, 2, 3, 5, 10, 20},
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation", "service_type"},
	)

	// pendingServiceDeletions tracks LBs/NAT Gateways waiting to be deleted.
	pendingServiceDeletions = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "pending_deletions",
			Help:           "Number of services pending deletion waiting for location cleanup",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// servicesBlockedByLocations tracks deletions blocked because pods are still attached.
	servicesBlockedByLocations = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "services_blocked_by_locations",
			Help:           "Number of services blocked from deletion due to active locations",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// trackedServices tracks the number of LBs and NAT Gateways in the cluster.
	//
	// The nrp_loadbalancers and nrp_natgateways series are live. k8s_services is not: the desired
	// Kubernetes set feeds the startup diff and is deliberately not mutated afterwards, so that
	// series reports what startup observed and does not move as Services come and go. Alert on the
	// NRP series, not on it.
	trackedServices = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "tracked_services_total",
			Help:           "Number of services tracked by state; nrp_loadbalancers and nrp_natgateways are live, k8s_services is the startup snapshot",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"state"},
	)

	// locationsTotal tracks the number of nodes with pods using LB/NAT.
	locationsTotal = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "locations_total",
			Help:           "Total number of locations (nodes) tracked in NRP",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// addressesTotal tracks the number of pod IPs tracked.
	addressesTotal = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "addresses_total",
			Help:           "Total number of pod IP addresses tracked across all locations",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// servicegatewayEnabled reports whether this cluster uses the ServiceGateway feature.
	servicegatewayEnabled = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "servicegateway_enabled",
			Help:           "Whether ServiceGateway is enabled for this cluster (1=yes, 0=no)",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// ========================================================================
	// Metrics from SLB Observability Guide — Section 10.1 (Proposed Recovery)
	// ========================================================================

	// orphanedResourcesCleanedTotal counts orphaned Azure LBs/NAT Gateways/PIPs cleaned up at startup.
	orphanedResourcesCleanedTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "orphaned_resources_cleaned_total",
			Help:           "Total count of orphaned Azure resources (LBs/NAT Gateways/PIPs) cleaned up at startup",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// finalizersRecoveredTotal counts stuck pod/service finalizers that recovery actually removed,
	// whether startup removed them directly or a later drain or diff completed the handoff. Work
	// that is only scheduled is not counted here: the finalizer is still on the object.
	finalizersRecoveredTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "finalizers_recovered_total",
			Help:           "Total count of stuck pod/service finalizers actually removed",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// finalizersRecoveryScheduledTotal counts stuck finalizers startup handed to another path rather
	// than removing: a Service whose Azure resource still exists is left to the diff, and a pod with
	// live addresses is enqueued for drain. Each of those is counted by finalizersRecoveredTotal once
	// that path removes it, so a gap that does not close means recovery is stalled.
	finalizersRecoveryScheduledTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "finalizers_recovery_scheduled_total",
			Help:           "Total count of stuck finalizers scheduled for cleanup by another path at startup",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// serviceFinalizerRemoveFailedTotal counts Services left Terminating because startup could not
	// remove the cleanup finalizer after its retries. Nothing re-drives it: a deleting Service is
	// excluded from desired state and has no NRP entry, so no operation is scheduled for it. It is
	// retried only on the next CCM restart, and blocks namespace deletion until then.
	serviceFinalizerRemoveFailedTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_finalizer_remove_failed_total",
			Help:           "Total count of Services left Terminating because cleanup finalizer removal failed at startup",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// podFinalizerAddFailedTotal counts egress pods registered WITHOUT the cleanup finalizer
	// because adding it failed even after retries (e.g. a sustained apiserver outage during the
	// pod's IP-assignment burst). Such pods lack NRP-drain protection on delete, so a non-zero
	// value should be alerted on.
	podFinalizerAddFailedTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "pod_finalizer_add_failed_total",
			Help:           "Total count of egress pods registered without a cleanup finalizer after add retries failed",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// podFinalizerRemoveFailedTotal counts egress pods left holding the cleanup finalizer because
	// removing it failed. The removal here is the direct, non-drain-gated path: the engine has
	// already proven nothing needs to drain, so there is no pending record to retry from and the
	// informer only re-drives a pod when its state actually changes - a resync delivers an
	// unchanged object and is skipped. The pod therefore stays Terminating until the CCM restarts,
	// blocking node drain and namespace deletion, so a non-zero value should be alerted on.
	podFinalizerRemoveFailedTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "pod_finalizer_remove_failed_total",
			Help:           "Total count of egress pods left Terminating because cleanup finalizer removal failed",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// serviceOperationsParkedTotal counts service operations that stopped making progress: either a
	// deterministic, spec-driven failure that cannot succeed on retry, or exhaustion of the retry
	// budget. A parked operation leaves the Service without its Azure resources indefinitely while
	// EnsureLoadBalancer has already reported success to the service controller, so without this
	// counter the condition has no signal beyond a log line.
	serviceOperationsParkedTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_operations_parked_total",
			Help:           "Total count of service operations parked without completing, by park reason",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"reason"},
	)

	// outboundServiceUpdatesSkippedTotal counts outbound (egress) service updates that the updater
	// drops because there is no NRP update operation for an egress identity. The service keeps its
	// previously applied Azure configuration, so this is the only signal that a requested outbound
	// spec change was not applied.
	outboundServiceUpdatesSkippedTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "outbound_service_updates_skipped_total",
			Help:           "Total count of outbound service updates dropped because they cannot be applied",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// initializationDurationSeconds reports how long SLB controller initialization took.
	initializationDurationSeconds = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "initialization_duration_seconds",
			Help:           "Duration of SLB controller initialization in seconds",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// locationSyncTerminalErrorsTotal counts NRP location syncs abandoned because NRP returned a
	// deterministic client error (e.g. HTTP 400) that retrying cannot fix. NRP address state then
	// diverges from desired until the offending change is corrected; a non-zero value must be alerted on.
	locationSyncTerminalErrorsTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "location_sync_terminal_errors_total",
			Help:           "Total count of NRP location syncs abandoned due to a deterministic (non-retryable) error",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// deleteSubstepFailuresTotal counts delete sub-steps that failed and were continued past. The
	// deletion still proceeds, because the later steps are what free the resources, but the skipped
	// step leaves a stale ServiceGateway reference behind and the operation is still recorded as a
	// successful delete. This is the only signal that happened.
	deleteSubstepFailuresTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "delete_substep_failures_total",
			Help:           "Total count of non-fatal delete sub-step failures by step",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"step"},
	)

	// serviceExternalIPRecoveryFailedTotal counts Services whose External IP could not be written back
	// at startup. Recovery runs once, so the Service keeps an empty ingress until the next restart
	// even though its Azure resources exist. A non-zero value must be alerted on.
	serviceExternalIPRecoveryFailedTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_external_ip_recovery_failed_total",
			Help:           "Total count of Services whose External IP could not be recovered at startup",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// locationSyncAbandonedTotal counts NRP location syncs given up on after exhausting retries.
	// NRP address state is then stale with nothing left to re-drive it, and blocked finalizers stay
	// pending. A non-zero value must be alerted on.
	locationSyncAbandonedTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "location_sync_abandoned_total",
			Help:           "Total count of NRP location syncs abandoned after exhausting retries, by reason",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"reason"},
	)

	// ========================================================================
	// Stuck detection metric (addresses David's review comment)
	// ========================================================================

	// pendingOperationOldestAgeSeconds reports the age of the oldest pending operation
	// per state/service_type. Enables alarms like:
	//   pending_operation_oldest_age_seconds{state="creation_in_progress"} > 600
	pendingOperationOldestAgeSeconds = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "pending_operation_oldest_age_seconds",
			Help:           "Age in seconds of the oldest pending operation, by state and service type",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"state", "service_type"},
	)
)

// RegisterMetrics registers all SLB observability metrics with the Prometheus registry.
// This is called only when ServiceGatewayEnabled=true, so non-SLB clusters don't
// have these metrics on /metrics. Safe to call multiple times (guarded by sync.Once).
func RegisterMetrics() {
	registerOnce.Do(func() {
		legacyregistry.MustRegister(pendingServiceOperations)
		legacyregistry.MustRegister(serviceOperationTotal)
		legacyregistry.MustRegister(serviceOperationDuration)
		legacyregistry.MustRegister(serviceOperationRetries)
		legacyregistry.MustRegister(pendingServiceDeletions)
		legacyregistry.MustRegister(servicesBlockedByLocations)
		legacyregistry.MustRegister(trackedServices)
		legacyregistry.MustRegister(locationsTotal)
		legacyregistry.MustRegister(addressesTotal)
		legacyregistry.MustRegister(servicegatewayEnabled)
		legacyregistry.MustRegister(orphanedResourcesCleanedTotal)
		legacyregistry.MustRegister(finalizersRecoveredTotal)
		legacyregistry.MustRegister(finalizersRecoveryScheduledTotal)
		legacyregistry.MustRegister(serviceFinalizerRemoveFailedTotal)
		legacyregistry.MustRegister(podFinalizerAddFailedTotal)
		legacyregistry.MustRegister(podFinalizerRemoveFailedTotal)
		legacyregistry.MustRegister(serviceOperationsParkedTotal)
		legacyregistry.MustRegister(outboundServiceUpdatesSkippedTotal)
		legacyregistry.MustRegister(locationSyncTerminalErrorsTotal)
		legacyregistry.MustRegister(locationSyncAbandonedTotal)
		legacyregistry.MustRegister(deleteSubstepFailuresTotal)
		legacyregistry.MustRegister(serviceExternalIPRecoveryFailedTotal)
		legacyregistry.MustRegister(initializationDurationSeconds)
		legacyregistry.MustRegister(pendingOperationOldestAgeSeconds)
	})
}

// ========================================================================
// Recording helpers — Section 1.1 metrics
// ========================================================================

// updatePendingServiceOperationsMetric updates the gauge for pending service operations
func updatePendingServiceOperationsMetric(dt *DiffTracker) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	// Key the buckets by the same resourceStateToString label used when publishing, and seed every
	// known state plus an "unknown" catch-all. resourceStateToString maps any out-of-range
	// ResourceState to "unknown" (seeded below), so the inner map is always non-nil and an
	// unrecognized state can never nil-map panic here. This matters because this runs in a deferred
	// metric path on the caller's goroutine (AddService/UpdateService/DeleteService/
	// OnServiceCreationComplete), where a panic would crash the CCM.
	stateCounts := make(map[string]map[string]int)
	for state := StateNotStarted; state <= StateUpdateInProgress; state++ {
		stateCounts[resourceStateToString(state)] = map[string]int{"inbound": 0, "outbound": 0}
	}
	stateCounts["unknown"] = map[string]int{"inbound": 0, "outbound": 0}

	for _, opState := range dt.pendingServiceOps {
		serviceType := "outbound"
		if opState.Config.IsInbound {
			serviceType = "inbound"
		}
		stateLabel := resourceStateToString(opState.State)
		counts, ok := stateCounts[stateLabel]
		if !ok {
			// Defensive: resourceStateToString already collapses unknowns to the seeded "unknown"
			// bucket, but never index a nil inner map even if its label set changes.
			counts = map[string]int{"inbound": 0, "outbound": 0}
			stateCounts[stateLabel] = counts
		}
		counts[serviceType]++
	}

	for stateLabel, typeCounts := range stateCounts {
		for serviceType, count := range typeCounts {
			pendingServiceOperations.WithLabelValues(stateLabel, serviceType).Set(float64(count))
		}
	}
}

// recordServiceOperation records a service creation/deletion operation with error code and orphan status
func recordServiceOperation(operation string, isInbound bool, startTime time.Time, err error, errorCode string, isOrphan bool) {
	serviceType := "outbound"
	if isInbound {
		serviceType = "inbound"
	}

	result := "success"
	if err != nil {
		result = "error"
	}

	orphanLabel := strconv.FormatBool(isOrphan)
	serviceOperationTotal.WithLabelValues(operation, serviceType, result, errorCode, orphanLabel).Inc()

	duration := time.Since(startTime).Seconds()
	serviceOperationDuration.WithLabelValues(operation, serviceType).Observe(duration)
}

// recordServiceOperationRetries records the number of retries a service operation needed. It must be
// called exactly once per operation, at the point the operation reaches a terminal outcome, so that
// operations which succeed on the first attempt are represented as 0.
func recordServiceOperationRetries(operation string, isInbound bool, retryCount int) {
	serviceType := "outbound"
	if isInbound {
		serviceType = "inbound"
	}
	serviceOperationRetries.WithLabelValues(operation, serviceType).Observe(float64(retryCount))
}

// updatePendingServiceDeletionsMetric updates the gauge for pending deletions
func updatePendingServiceDeletionsMetric(dt *DiffTracker) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	pendingServiceDeletions.Set(float64(len(dt.pendingServiceDeletions)))
}

// updateServicesBlockedByLocationsMetric updates gauge for services blocked by locations
func updateServicesBlockedByLocationsMetric(count int) {
	servicesBlockedByLocations.Set(float64(count))
}

// updateTrackedServicesMetric updates gauges for tracked services by state
func updateTrackedServicesMetric(dt *DiffTracker) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	// Startup snapshot, not a live count: see the trackedServices declaration.
	k8sServices := dt.K8sResources.Services.Len()
	nrpLoadBalancers := dt.NRPResources.LoadBalancers.Len()
	nrpNATGateways := dt.NRPResources.NATGateways.Len()

	trackedServices.WithLabelValues("k8s_services").Set(float64(k8sServices))
	trackedServices.WithLabelValues("nrp_loadbalancers").Set(float64(nrpLoadBalancers))
	trackedServices.WithLabelValues("nrp_natgateways").Set(float64(nrpNATGateways))
}

// updateLocationsAndAddressesMetric updates the locations and addresses gauges
func updateLocationsAndAddressesMetric(locationCount, addressCount int) {
	locationsTotal.Set(float64(locationCount))
	addressesTotal.Set(float64(addressCount))
}

// ========================================================================
// Recording helpers — Section 10.1 proposed recovery metrics
// ========================================================================

// RecordServiceGatewayEnabled sets the servicegateway_enabled gauge to 1
func RecordServiceGatewayEnabled() {
	servicegatewayEnabled.Set(1.0)
}

// recordOrphanedResourceCleaned increments the counter for each orphaned resource cleaned
func recordOrphanedResourceCleaned() {
	orphanedResourcesCleanedTotal.Inc()
}

// recordFinalizerRecovered increments the counter for each recovered finalizer
func recordFinalizerRecovered() {
	finalizersRecoveredTotal.Inc()
}

// recordFinalizerRecoveryScheduled counts a stuck finalizer handed to the diff or to drain tracking
// rather than removed. The finalizer is still on the object.
func recordFinalizerRecoveryScheduled() {
	finalizersRecoveryScheduledTotal.Inc()
}

// RecordServiceFinalizerRemoveFailed counts a Service left Terminating because startup could not
// remove its cleanup finalizer and no retry is scheduled before the next CCM restart.
func RecordServiceFinalizerRemoveFailed() {
	serviceFinalizerRemoveFailedTotal.Inc()
}

// recordLocationSyncTerminalError increments the counter of NRP location syncs abandoned due to a
// deterministic, non-retryable error.
func recordLocationSyncTerminalError() {
	locationSyncTerminalErrorsTotal.Inc()
}

// Reasons for abandoning an NRP location sync after its retry budget is spent.
const (
	locationSyncAbandonReasonInitAttempts = "init_attempts_exhausted"
	locationSyncAbandonReasonDrain        = "drain_attempts_exhausted"
)

// Delete sub-steps that are continued past on failure.
const (
	deleteStepRemoveBackendPool = "remove_backend_pool"
	deleteStepDisassociateNAT   = "disassociate_nat_gateway"
)

// recordServiceExternalIPRecoveryFailed counts a Service whose External IP could not be recovered.
func recordServiceExternalIPRecoveryFailed() {
	serviceExternalIPRecoveryFailedTotal.Inc()
}

// recordDeleteSubstepFailure counts a delete sub-step that failed and was continued past.
func recordDeleteSubstepFailure(step string) {
	deleteSubstepFailuresTotal.WithLabelValues(step).Inc()
}

// recordLocationSyncAbandoned counts an NRP location sync that has exhausted its retry budget.
// The sync is no longer abandoned when this fires - retries continue under a capped, jittered
// backoff - so this is a sustained-outage signal rather than a give-up signal. The metric name is
// retained for dashboard/alert compatibility; the operational meaning ("NRP sync is not
// converging, investigate") is unchanged.
func recordLocationSyncAbandoned(reason string) {
	locationSyncAbandonedTotal.WithLabelValues(reason).Inc()
}

// RecordPodFinalizerAddFailed increments the counter for an egress pod that was registered without
// its cleanup finalizer because adding the finalizer failed after retries. Exported for the pod
// informer in the provider package.
func RecordPodFinalizerAddFailed() {
	podFinalizerAddFailedTotal.Inc()
}

// RecordPodFinalizerRemoveFailed counts an egress pod left Terminating because removing its cleanup
// finalizer failed and no retry is scheduled.
func RecordPodFinalizerRemoveFailed() {
	podFinalizerRemoveFailedTotal.Inc()
}

// Park reasons for serviceOperationsParkedTotal. The label is a small closed set so cardinality
// stays bounded.
const (
	parkReasonTerminalError   = "terminal_error"
	parkReasonRetriesExceeded = "retries_exceeded"
)

// recordServiceParked counts a service operation that stopped making progress. Safe to call while
// dt.mu is held: it touches only the package-level counter.
func recordServiceParked(reason string) {
	serviceOperationsParkedTotal.WithLabelValues(reason).Inc()
}

// operationLabelForState maps the state a pending operation is dispatched from to the operation
// label used by the service operation metrics.
func operationLabelForState(state ResourceState) string {
	switch state {
	case StateDeletionPending, StateDeletionInProgress:
		return "delete"
	case StateUpdateInProgress:
		return "update"
	default:
		return "create"
	}
}

// recordOutboundServiceUpdateSkipped counts an outbound service update that was dropped because it
// cannot be applied to an existing egress identity.
func recordOutboundServiceUpdateSkipped() {
	outboundServiceUpdatesSkippedTotal.Inc()
}

// recordInitializationDuration records how long initialization took
func recordInitializationDuration(startTime time.Time) {
	initializationDurationSeconds.Set(time.Since(startTime).Seconds())
}

// ========================================================================
// Recording helpers — stuck detection (oldest-age gauge)
// ========================================================================

// updatePendingOperationOldestAgeMetric computes the age of the oldest pending operation
// per state/service_type and sets the gauge. Called on every AddService/DeleteService/
// OnServiceCreationComplete and on a 30s ticker in ServiceUpdater.Run() so the value
// stays fresh for alerting.
func updatePendingOperationOldestAgeMetric(dt *DiffTracker) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	type key struct {
		state       ResourceState
		serviceType string
	}

	oldest := make(map[key]time.Time)

	for _, opState := range dt.pendingServiceOps {
		serviceType := "outbound"
		if opState.Config.IsInbound {
			serviceType = "inbound"
		}
		k := key{state: opState.State, serviceType: serviceType}
		if existing, ok := oldest[k]; !ok || opState.CreatedAt.Before(existing) {
			oldest[k] = opState.CreatedAt
		}
	}

	now := time.Now()
	// Set values for all state/type combinations, zeroing groups with no entries.
	// Bound is StateUpdateInProgress (the last state) so update_in_progress is included;
	// StateUpdateInProgress > StateDeletionInProgress, so a tighter bound would silently
	// drop the update_in_progress series and hide a stuck update from its alert.
	for state := StateNotStarted; state <= StateUpdateInProgress; state++ {
		for _, serviceType := range []string{"inbound", "outbound"} {
			k := key{state: state, serviceType: serviceType}
			stateStr := resourceStateToString(state)
			if createdAt, ok := oldest[k]; ok {
				pendingOperationOldestAgeSeconds.WithLabelValues(stateStr, serviceType).Set(now.Sub(createdAt).Seconds())
			} else {
				pendingOperationOldestAgeSeconds.WithLabelValues(stateStr, serviceType).Set(0)
			}
		}
	}
}

// ========================================================================
// Utility
// ========================================================================

// resourceStateToString converts ResourceState to string
func resourceStateToString(state ResourceState) string {
	switch state {
	case StateNotStarted:
		return "not_started"
	case StateCreationInProgress:
		return "creation_in_progress"
	case StateCreated:
		return "created"
	case StateDeletionPending:
		return "deletion_pending"
	case StateDeletionInProgress:
		return "deletion_in_progress"
	case StateUpdateInProgress:
		return "update_in_progress"
	default:
		return "unknown"
	}
}
