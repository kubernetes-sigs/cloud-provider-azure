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

	// serviceOperationTotal counts successful and failed operations, with error code for triage.
	serviceOperationTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_operations_total",
			Help:           "Total count of service creation/deletion operations by result, error code, and orphan status",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation", "service_type", "result", "error_code", "is_orphan"},
	)

	// serviceOperationDuration tracks how long LB/NAT creation/deletion takes.
	serviceOperationDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_operation_duration_seconds",
			Help:           "Duration of service creation/deletion operations in seconds",
			Buckets:        []float64{0.1, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0, 60.0, 120.0},
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation", "service_type"},
	)

	// serviceOperationRetries tracks how many retry attempts were needed.
	serviceOperationRetries = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_operation_retries",
			Help:           "Number of retries for service operations",
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
	trackedServices = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "tracked_services_total",
			Help:           "Number of services tracked by state (k8s_services, nrp_loadbalancers, nrp_natgateways)",
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

	// finalizersRecoveredTotal counts stuck pod/service finalizers recovered at startup.
	finalizersRecoveredTotal = metrics.NewCounter(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "finalizers_recovered_total",
			Help:           "Total count of stuck pod/service finalizers recovered at startup",
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

	stateCounts := make(map[ResourceState]map[string]int)
	for state := StateNotStarted; state <= StateDeletionInProgress; state++ {
		stateCounts[state] = map[string]int{"inbound": 0, "outbound": 0}
	}

	for _, opState := range dt.pendingServiceOps {
		serviceType := "outbound"
		if opState.Config.IsInbound {
			serviceType = "inbound"
		}
		stateCounts[opState.State][serviceType]++
	}

	for state, typeCounts := range stateCounts {
		for serviceType, count := range typeCounts {
			pendingServiceOperations.WithLabelValues(resourceStateToString(state), serviceType).Set(float64(count))
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

// recordServiceOperationRetry records retry count for service operations
func recordServiceOperationRetry(operation string, isInbound bool, retryCount int) {
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
	// Set values for all state/type combinations, zeroing groups with no entries
	for state := StateNotStarted; state <= StateDeletionInProgress; state++ {
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
	default:
		return "unknown"
	}
}
