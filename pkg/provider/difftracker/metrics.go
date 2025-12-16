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
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	diffTrackerSubsystem = "difftracker_engine"
)

var (
	// Engine lifecycle metrics
	engineOperationDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "operation_duration_seconds",
			Help:           "Duration of Engine operations in seconds",
			Buckets:        []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation", "result"},
	)

	// Service state tracking metrics
	pendingServiceOperations = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "pending_service_operations",
			Help:           "Number of service operations pending by state",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"state", "service_type"},
	)

	// Buffered updates metrics
	bufferedUpdates = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "buffered_updates",
			Help:           "Number of buffered updates waiting for service creation",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"update_type"},
	)

	// Service creation/deletion metrics
	serviceOperationTotal = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_operations_total",
			Help:           "Total number of service creation/deletion operations",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"operation", "service_type", "result"},
	)

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

	// Retry metrics
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

	// Pending deletions metrics
	pendingDeletions = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "pending_deletions",
			Help:           "Number of services pending deletion waiting for location cleanup",
			StabilityLevel: metrics.ALPHA,
		},
	)

	deletionCheckDuration = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "deletion_check_duration_seconds",
			Help:           "Duration of deletion checker runs in seconds",
			Buckets:        []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5},
			StabilityLevel: metrics.ALPHA,
		},
	)

	servicesBlockedByLocations = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "services_blocked_by_locations",
			Help:           "Number of services blocked from deletion due to active locations",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// ServiceUpdater metrics
	serviceUpdaterConcurrentOps = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_updater_concurrent_operations",
			Help:           "Number of concurrent operations in ServiceUpdater",
			StabilityLevel: metrics.ALPHA,
		},
	)

	serviceUpdaterBatchSize = metrics.NewHistogram(
		&metrics.HistogramOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "service_updater_batch_size",
			Help:           "Number of services processed per ServiceUpdater batch",
			Buckets:        []float64{0, 1, 2, 5, 10, 20, 50, 100},
			StabilityLevel: metrics.ALPHA,
		},
	)

	// LocationsUpdater metrics
	locationsUpdateDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "locations_update_duration_seconds",
			Help:           "Duration of LocationsUpdater sync operations in seconds",
			Buckets:        []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"result"},
	)

	locationsTotal = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "locations_total",
			Help:           "Total number of locations tracked in NRP",
			StabilityLevel: metrics.ALPHA,
		},
	)

	addressesTotal = metrics.NewGauge(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "addresses_total",
			Help:           "Total number of addresses tracked across all locations",
			StabilityLevel: metrics.ALPHA,
		},
	)

	// State tracking metrics
	trackedServices = metrics.NewGaugeVec(
		&metrics.GaugeOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "tracked_services_total",
			Help:           "Number of services tracked by state",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"state"},
	)

	stateTransitions = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Subsystem:      diffTrackerSubsystem,
			Name:           "state_transitions_total",
			Help:           "Total number of service state transitions",
			StabilityLevel: metrics.ALPHA,
		},
		[]string{"from_state", "to_state"},
	)
)

func init() {
	registerMetrics()
}

func registerMetrics() {
	legacyregistry.MustRegister(engineOperationDuration)
	legacyregistry.MustRegister(pendingServiceOperations)
	legacyregistry.MustRegister(bufferedUpdates)
	legacyregistry.MustRegister(serviceOperationTotal)
	legacyregistry.MustRegister(serviceOperationDuration)
	legacyregistry.MustRegister(serviceOperationRetries)
	legacyregistry.MustRegister(pendingDeletions)
	legacyregistry.MustRegister(deletionCheckDuration)
	legacyregistry.MustRegister(servicesBlockedByLocations)
	legacyregistry.MustRegister(serviceUpdaterConcurrentOps)
	legacyregistry.MustRegister(serviceUpdaterBatchSize)
	legacyregistry.MustRegister(locationsUpdateDuration)
	legacyregistry.MustRegister(locationsTotal)
	legacyregistry.MustRegister(addressesTotal)
	legacyregistry.MustRegister(trackedServices)
	legacyregistry.MustRegister(stateTransitions)
}

// recordEngineOperation records the duration and result of an Engine operation
func recordEngineOperation(operation string, startTime time.Time, err error) {
	duration := time.Since(startTime).Seconds()
	result := "success"
	if err != nil {
		result = "error"
	}
	engineOperationDuration.WithLabelValues(operation, result).Observe(duration)
}

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

// updateBufferedUpdatesMetric updates the gauge for buffered updates
func updateBufferedUpdatesMetric(dt *DiffTracker) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	endpointCount := 0
	for _, updates := range dt.pendingEndpoints {
		endpointCount += len(updates)
	}

	podCount := 0
	for _, updates := range dt.pendingPods {
		podCount += len(updates)
	}

	bufferedUpdates.WithLabelValues("endpoints").Set(float64(endpointCount))
	bufferedUpdates.WithLabelValues("pods").Set(float64(podCount))
}

// recordServiceOperation records a service creation/deletion operation
func recordServiceOperation(operation string, isInbound bool, startTime time.Time, err error) {
	serviceType := "outbound"
	if isInbound {
		serviceType = "inbound"
	}

	result := "success"
	if err != nil {
		result = "error"
	}

	serviceOperationTotal.WithLabelValues(operation, serviceType, result).Inc()

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

// updatePendingDeletionsMetric updates the gauge for pending deletions
func updatePendingDeletionsMetric(dt *DiffTracker) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	pendingDeletions.Set(float64(len(dt.pendingDeletions)))
}

// recordDeletionCheck records a deletion checker run
func recordDeletionCheck(startTime time.Time, blockedCount int) {
	duration := time.Since(startTime).Seconds()
	deletionCheckDuration.Observe(duration)
	servicesBlockedByLocations.Set(float64(blockedCount))
}

// updateServiceUpdaterConcurrentOps updates the gauge for concurrent operations
func updateServiceUpdaterConcurrentOps(count int) {
	serviceUpdaterConcurrentOps.Set(float64(count))
}

// recordServiceUpdaterBatch records the size of a ServiceUpdater batch
func recordServiceUpdaterBatch(batchSize int) {
	serviceUpdaterBatchSize.Observe(float64(batchSize))
}

// recordLocationsUpdate records a LocationsUpdater sync operation
func recordLocationsUpdate(startTime time.Time, locationCount, addressCount int, err error) {
	duration := time.Since(startTime).Seconds()
	result := "success"
	if err != nil {
		result = "error"
	}
	locationsUpdateDuration.WithLabelValues(result).Observe(duration)
	if err == nil {
		locationsTotal.Set(float64(locationCount))
		addressesTotal.Set(float64(addressCount))
	}
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

// recordStateTransition records a state transition
func recordStateTransition(fromState, toState ResourceState) {
	stateTransitions.WithLabelValues(resourceStateToString(fromState), resourceStateToString(toState)).Inc()
}

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
