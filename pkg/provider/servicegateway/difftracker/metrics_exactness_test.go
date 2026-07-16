/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Metrics exactness tests.
//
// These tests verify two cross-cutting metric invariants:
//
//   * service_operation_total — INCREMENT EXACTLY ONCE per logical
//     operation (create / update / delete). A regression that double-records
//     (e.g. once in the dispatch, once in the completion handler) would
//     silently corrupt service-level SLOs.
//
//   * pendingServiceDeletions gauge — NEVER NEGATIVE and tracks
//     len(dt.pendingServiceDeletions) exactly. A negative value means
//     the gauge was decremented without a matching enqueue.

package difftracker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/component-base/metrics/testutil"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// TestGuardMetrics_ServiceOperationTotal_IncrementsOnce verifies that
// recordServiceOperation increments serviceOperationTotal exactly once per
// call (and once only). A regression that double-fires the counter would
// fail this test.
func TestGuardMetrics_ServiceOperationTotal_IncrementsOnce(t *testing.T) {
	RegisterMetrics()
	serviceOperationTotal.Reset()

	recordServiceOperation("create", true, time.Now(), nil, "", false)

	got, err := testutil.GetCounterMetricValue(
		serviceOperationTotal.WithLabelValues("create", "inbound", "success", "", "false"),
	)
	assert.NoError(t, err)
	assert.Equal(t, 1.0, got, "single recordServiceOperation call must increment counter by exactly 1")

	// Second call must produce exactly 2 (monotonic counter), not 3+ (no
	// hidden double-increment).
	recordServiceOperation("create", true, time.Now(), nil, "", false)
	got, _ = testutil.GetCounterMetricValue(
		serviceOperationTotal.WithLabelValues("create", "inbound", "success", "", "false"),
	)
	assert.Equal(t, 2.0, got, "two recordServiceOperation calls must increment counter by exactly 2")
}

// TestGuardMetrics_ServiceOperationTotal_SeparatesErrorAndSuccess verifies that
// success and error are emitted as DISTINCT label series (so error rate is
// observable) and that an error path does NOT also increment the success
// series. Regression: a future refactor that fires both labels would
// corrupt success/failure ratios.
func TestGuardMetrics_ServiceOperationTotal_SeparatesErrorAndSuccess(t *testing.T) {
	RegisterMetrics()
	serviceOperationTotal.Reset()

	recordServiceOperation("delete", false, time.Now(), assertErrorForMetrics{}, "throttled", false)
	successCount, _ := testutil.GetCounterMetricValue(
		serviceOperationTotal.WithLabelValues("delete", "outbound", "success", "throttled", "false"),
	)
	errorCount, _ := testutil.GetCounterMetricValue(
		serviceOperationTotal.WithLabelValues("delete", "outbound", "error", "throttled", "false"),
	)
	assert.Equal(t, 0.0, successCount, "error path MUST NOT emit success series")
	assert.Equal(t, 1.0, errorCount, "error path MUST emit error series exactly once")
}

// TestGuardMetrics_PendingServiceDeletionsGauge_NeverNegative verifies the
// never-negative invariant: the pendingServiceDeletions gauge is computed
// from len(dt.pendingServiceDeletions), which is always >= 0. Two calls in
// a row, after we mutate the underlying map, must both yield non-negative
// values that exactly reflect the map size.
func TestGuardMetrics_PendingServiceDeletionsGauge_NeverNegative(t *testing.T) {
	RegisterMetrics()
	dt := newTestDiffTracker()

	// Empty → gauge must be 0 (NOT negative, NOT NaN).
	updatePendingServiceDeletionsMetric(dt)
	v, err := testutil.GetGaugeMetricValue(pendingServiceDeletions)
	assert.NoError(t, err)
	assert.GreaterOrEqual(t, v, 0.0, "pendingServiceDeletions gauge must never be negative (empty map case)")
	assert.Equal(t, 0.0, v, "empty pendingServiceDeletions must report 0")

	// Add two pending deletions → gauge must be 2.
	dt.pendingServiceDeletions["a"] = &PendingServiceDeletion{ServiceUID: "a"}
	dt.pendingServiceDeletions["b"] = &PendingServiceDeletion{ServiceUID: "b"}
	updatePendingServiceDeletionsMetric(dt)
	v, _ = testutil.GetGaugeMetricValue(pendingServiceDeletions)
	assert.Equal(t, 2.0, v, "gauge must exactly equal len(pendingServiceDeletions)")
	assert.GreaterOrEqual(t, v, 0.0, "never negative")

	// Drain back to zero → gauge MUST go back to 0, never below.
	delete(dt.pendingServiceDeletions, "a")
	delete(dt.pendingServiceDeletions, "b")
	updatePendingServiceDeletionsMetric(dt)
	v, _ = testutil.GetGaugeMetricValue(pendingServiceDeletions)
	assert.Equal(t, 0.0, v, "draining map must drive gauge to 0")
	assert.GreaterOrEqual(t, v, 0.0, "never negative on drain")
}

// TestGuardMetrics_PendingServiceOperationsGauge_NeverNegative verifies that
// the per-state pendingServiceOperations gauge is non-negative and exactly
// reflects the count of pendingServiceOps in each known state.
func TestGuardMetrics_PendingServiceOperationsGauge_NeverNegative(t *testing.T) {
	RegisterMetrics()
	pendingServiceOperations.Reset()
	dt := newTestDiffTracker()

	dt.pendingServiceOps["a"] = &ServiceOperationState{
		ServiceUID: "a",
		Config:     NewInboundServiceConfig("a", nil),
		State:      StateCreationInProgress,
	}
	dt.pendingServiceOps["b"] = &ServiceOperationState{
		ServiceUID: "b",
		Config:     NewInboundServiceConfig("b", nil),
		State:      StateCreated,
	}
	dt.pendingServiceOps["c"] = &ServiceOperationState{
		ServiceUID: "c",
		Config:     NewOutboundServiceConfig("c", nil),
		State:      StateDeletionPending,
	}

	updatePendingServiceOperationsMetric(dt)

	v, _ := testutil.GetGaugeMetricValue(pendingServiceOperations.WithLabelValues("creation_in_progress", "inbound"))
	assert.Equal(t, 1.0, v)
	assert.GreaterOrEqual(t, v, 0.0)
	v, _ = testutil.GetGaugeMetricValue(pendingServiceOperations.WithLabelValues("created", "inbound"))
	assert.Equal(t, 1.0, v)
	v, _ = testutil.GetGaugeMetricValue(pendingServiceOperations.WithLabelValues("deletion_pending", "outbound"))
	assert.Equal(t, 1.0, v)
	// Untouched buckets must read 0 (not NaN, not negative).
	v, _ = testutil.GetGaugeMetricValue(pendingServiceOperations.WithLabelValues("deletion_in_progress", "outbound"))
	assert.Equal(t, 0.0, v)
	assert.GreaterOrEqual(t, v, 0.0)
}

// TestGuardMetrics_PendingServiceOperationsGauge_OutOfRangeStateNoPanic verifies that an
// out-of-range ResourceState does not panic updatePendingServiceOperationsMetric (which runs in a
// deferred metric path on the caller's goroutine, where a panic would crash the CCM) and that the
// op is counted under the "unknown" state label.
func TestGuardMetrics_PendingServiceOperationsGauge_OutOfRangeStateNoPanic(t *testing.T) {
	RegisterMetrics()
	pendingServiceOperations.Reset()
	dt := newTestDiffTracker()

	dt.pendingServiceOps["bad"] = &ServiceOperationState{
		ServiceUID: "bad",
		Config:     NewInboundServiceConfig("bad", nil),
		State:      ResourceState(99), // out of the seeded enum range
	}

	assert.NotPanics(t, func() {
		updatePendingServiceOperationsMetric(dt)
	}, "an out-of-range ResourceState must not panic the metric update")

	v, err := testutil.GetGaugeMetricValue(pendingServiceOperations.WithLabelValues("unknown", "inbound"))
	assert.NoError(t, err)
	assert.Equal(t, 1.0, v, "an out-of-range state must be counted under the 'unknown' label")
}

// assertErrorForMetrics is a tiny error type used only by the error-series
// assertion above (we don't want to depend on a specific Azure-SDK error).
type assertErrorForMetrics struct{}

func (assertErrorForMetrics) Error() string { return "metrics-test-error" }

// TestGuardMetrics_LocationsAndAddressesTotals_ReflectNRPTotalsEveryCycle verifies that the
// locations_total / addresses_total gauges report the live NRP-tracked totals on every reconcile
// cycle — both a no-change cycle (no diff to sync) and a changed cycle — rather than the per-sync
// diff size. They are documented and alerted on as totals, so a no-change cycle must still publish
// the full totals and a changed cycle must publish the post-sync totals (not just the diff).
func TestGuardMetrics_LocationsAndAddressesTotals_ReflectNRPTotalsEveryCycle(t *testing.T) {
	RegisterMetrics()

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.networkClientFactory = mockFactory
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceUID: "svc", State: StateCreated}
	dt.NRPResources.LoadBalancers.Insert("svc")

	// In-sync state: one location (node 10.0.0.1) with one address (10.244.0.1) present in BOTH
	// K8s and NRP, so GetSyncLocationsAddresses produces no diff.
	podA := newPod()
	podA.InboundIdentities = utilsets.NewString("svc")
	nodeA := newNode()
	nodeA.Pods["10.244.0.1"] = podA
	dt.K8sResources.Nodes["10.0.0.1"] = nodeA
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.244.0.1": {Services: utilsets.NewString("svc")},
		},
	}

	lu := NewLocationsUpdater(context.Background(), dt)

	// No-change cycle: there is no diff to sync, but the gauges must still report the totals
	// (1 location, 1 address). Seed a deliberately-wrong sentinel first to prove process() sets
	// them on this path (the old code returned early without touching them).
	updateLocationsAndAddressesMetric(999, 999)
	lu.process(context.Background())
	gotLoc, err := testutil.GetGaugeMetricValue(locationsTotal)
	assert.NoError(t, err)
	assert.Equal(t, 1.0, gotLoc, "locations_total must equal the NRP total on a no-change cycle")
	gotAddr, err := testutil.GetGaugeMetricValue(addressesTotal)
	assert.NoError(t, err)
	assert.Equal(t, 1.0, gotAddr, "addresses_total must equal the NRP total on a no-change cycle")

	// Changed cycle: add a second pod on a new node. The diff is only 1 location/1 address, but
	// the gauges must report the post-sync TOTAL of 2 locations / 2 addresses.
	podB := newPod()
	podB.InboundIdentities = utilsets.NewString("svc")
	nodeB := newNode()
	nodeB.Pods["10.244.0.2"] = podB
	dt.K8sResources.Nodes["10.0.0.2"] = nodeB

	updateLocationsAndAddressesMetric(999, 999)
	lu.process(context.Background())
	gotLoc, err = testutil.GetGaugeMetricValue(locationsTotal)
	assert.NoError(t, err)
	assert.Equal(t, 2.0, gotLoc, "locations_total must equal the post-sync NRP total, not the diff size")
	gotAddr, err = testutil.GetGaugeMetricValue(addressesTotal)
	assert.NoError(t, err)
	assert.Equal(t, 2.0, gotAddr, "addresses_total must equal the post-sync NRP total, not the diff size")
}

// TestTrackedServicesMetric_RefreshedOnServiceCompletion verifies the tracked_services gauge is
// refreshed to match the NRP tracked sets when a service operation completes. The NRP
// LoadBalancer/NATGateway sets are mutated on async create/delete success, and without a refresh in
// OnServiceCreationComplete the gauge stays stale (e.g. never decremented after the last delete).
func TestTrackedServicesMetric_RefreshedOnServiceCompletion(t *testing.T) {
	RegisterMetrics()
	dt := newTestDiffTracker()
	uid := "svc-tracked-refresh"

	dt.NRPResources.LoadBalancers.Insert(uid)
	updateTrackedServicesMetric(dt) // baseline: gauge reflects one tracked NRP LB
	v, err := testutil.GetGaugeMetricValue(trackedServices.WithLabelValues("nrp_loadbalancers"))
	assert.NoError(t, err)
	assert.Equal(t, float64(1), v)

	// The NRP set shrinks on delete (as UpdateNRPLoadBalancers does) with no explicit metric refresh.
	dt.NRPResources.LoadBalancers.Delete(uid)

	// A service completion callback must refresh the tracked gauge to match the mutated set.
	dt.OnServiceCreationComplete(uid, true, nil)

	v, err = testutil.GetGaugeMetricValue(trackedServices.WithLabelValues("nrp_loadbalancers"))
	assert.NoError(t, err)
	assert.Equal(t, float64(0), v,
		"tracked_services{nrp_loadbalancers} must be refreshed to match the NRP set after a service completion")
}
