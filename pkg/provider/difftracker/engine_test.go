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
	"testing"

	"github.com/stretchr/testify/assert"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// Helper function to create a test DiffTracker
func newTestDiffTracker() *DiffTracker {
	return &DiffTracker{
		NRPResources: NRP_State{
			LoadBalancers: utilsets.NewString(),
			NATGateways:   utilsets.NewString(),
			Locations:     make(map[string]NRPLocation),
		},
		K8sResources: K8s_State{
			Services: utilsets.NewString(),
			Egresses: utilsets.NewString(),
			Nodes:    make(map[string]Node),
		},
		pendingServiceOps:       make(map[string]*ServiceOperationState),
		pendingEndpoints:        make(map[string][]PendingEndpointUpdate),
		pendingPods:             make(map[string][]PendingPodUpdate),
		pendingServiceDeletions: make(map[string]*PendingServiceDeletion),
		serviceUpdaterTrigger:   make(chan bool, 1),
		locationsUpdaterTrigger: make(chan bool, 1),
	}
}

// TestEngineAddService_NewService tests adding a new service that doesn't exist in NRP
func TestEngineAddService_NewService(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-1"

	// Execute
	dt.AddService(NewInboundServiceConfig(serviceUID, nil))

	// Verify service is tracked
	opState, exists := dt.pendingServiceOps[serviceUID]
	assert.True(t, exists, "Service should be tracked")
	assert.Equal(t, serviceUID, opState.ServiceUID)
	assert.True(t, opState.Config.IsInbound)
	assert.Equal(t, StateNotStarted, opState.State)
	assert.Equal(t, 0, opState.RetryCount)

	// Verify trigger was sent
	select {
	case <-dt.serviceUpdaterTrigger:
		// Expected
	default:
		t.Error("Expected ServiceUpdater trigger")
	}
}

// TestEngineAddService_ExistsInNRP tests adding a service that already exists in NRP
func TestEngineAddService_ExistsInNRP(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-2"

	// Setup: service already exists in NRP
	dt.NRPResources.LoadBalancers.Insert(serviceUID)

	// Execute
	dt.AddService(NewInboundServiceConfig(serviceUID, nil))

	// Verify service is NOT tracked (since it exists)
	_, exists := dt.pendingServiceOps[serviceUID]
	assert.False(t, exists, "Service should not be tracked if it exists in NRP")

	// Verify no trigger
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Error("Unexpected ServiceUpdater trigger")
	default:
		// Expected
	}
}

// TestEngineAddService_AlreadyTracked tests adding a service that's already being created
func TestEngineAddService_AlreadyTracked(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-3"

	// Setup: service already tracked
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		Config:     NewInboundServiceConfig(serviceUID, nil),
		State:      StateCreationInProgress,
		RetryCount: 0,
	}

	// Execute
	dt.AddService(NewInboundServiceConfig(serviceUID, nil))

	// Verify state unchanged
	opState := dt.pendingServiceOps[serviceUID]
	assert.Equal(t, StateCreationInProgress, opState.State)

	// Verify no trigger
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Error("Unexpected ServiceUpdater trigger")
	default:
		// Expected
	}
}

// TestEngineDeleteService_NoLocations tests deleting a service without locations
func TestEngineDeleteService_NoLocations(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-4"

	// Setup: service exists in NRP
	dt.NRPResources.LoadBalancers.Insert(serviceUID)

	// Execute
	dt.DeleteService(serviceUID, true, false)

	// Verify service is tracked for deletion
	opState, exists := dt.pendingServiceOps[serviceUID]
	assert.True(t, exists, "Service should be tracked for deletion")
	assert.Equal(t, StateDeletionInProgress, opState.State)

	// Verify trigger was sent
	select {
	case <-dt.serviceUpdaterTrigger:
		// Expected
	default:
		t.Error("Expected ServiceUpdater trigger")
	}
}

// TestEngineDeleteService_WithLocations tests deleting a service that has locations
func TestEngineDeleteService_WithLocations(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-5"

	// Setup: service exists in NRP
	dt.NRPResources.NATGateways.Insert(serviceUID)

	// Setup: Add a location that contains this service
	locationKey := "node1-10.0.0.1"
	dt.NRPResources.Locations[locationKey] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.0.0.1": {
				Services: utilsets.NewString(serviceUID),
			},
		},
	}

	// Execute
	dt.DeleteService(serviceUID, false, false)

	// Verify service is in pending deletions (not immediate deletion)
	pendingDel, exists := dt.pendingServiceDeletions[serviceUID]
	assert.True(t, exists, "Service should be in pending deletions")
	if exists {
		assert.Equal(t, serviceUID, pendingDel.ServiceUID)
		assert.False(t, pendingDel.IsInbound)
	}

	// Verify no immediate trigger
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Error("Unexpected ServiceUpdater trigger - should wait for locations to clear")
	default:
		// Expected
	}
}

// TestEngineUpdateEndpoints_ServiceExists tests endpoint updates for existing service
// Note: This test is skipped as it requires mocking UpdateK8sEndpoints which has complex dependencies
func TestEngineUpdateEndpoints_ServiceExists(t *testing.T) {
	t.Skip("Requires mocking UpdateK8sEndpoints - covered by integration tests")
}

// TestEngineUpdateEndpoints_ServiceCreating tests endpoint buffering during service creation
func TestEngineUpdateEndpoints_ServiceCreating(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-7"

	// Setup: service is being created
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		Config:     NewInboundServiceConfig(serviceUID, nil),
		State:      StateCreationInProgress,
		RetryCount: 0,
	}

	// Execute
	oldEndpoints := map[string]string{}
	newEndpoints := map[string]string{"10.0.0.1": "node1"}
	dt.UpdateEndpoints(serviceUID, oldEndpoints, newEndpoints)

	// Verify endpoints were buffered
	buffered, exists := dt.pendingEndpoints[serviceUID]
	assert.True(t, exists, "Endpoints should be buffered")
	assert.Greater(t, len(buffered), 0, "Should have buffered updates")

	// Verify no immediate trigger
	select {
	case <-dt.locationsUpdaterTrigger:
		t.Error("Unexpected LocationsUpdater trigger - endpoints should be buffered")
	default:
		// Expected
	}
}

// TestEngineOnServiceCreationComplete_Success tests successful service creation callback
// Note: This test is skipped as promotion requires mocking UpdateK8sEndpoints/UpdateK8sPod
func TestEngineOnServiceCreationComplete_Success(t *testing.T) {
	t.Skip("Requires mocking UpdateK8sEndpoints/UpdateK8sPod - covered by integration tests")
}

// TestEngineOnServiceCreationComplete_Failure tests failed service creation callback
func TestEngineOnServiceCreationComplete_Failure(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-9"

	// Setup: service is being created
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		Config:     NewInboundServiceConfig(serviceUID, nil),
		State:      StateCreationInProgress,
		RetryCount: 0,
	}

	// Execute
	dt.OnServiceCreationComplete(serviceUID, false, nil)

	// Verify retry count increased
	opState, exists := dt.pendingServiceOps[serviceUID]
	assert.True(t, exists, "Failed service should remain in pending")
	assert.Greater(t, opState.RetryCount, 0, "Retry count should increase")
}

// TestEngineAddPod_ServiceExists tests adding a pod to an existing service
// Note: This test is skipped as it requires mocking UpdatePod which has complex dependencies
func TestEngineAddPod_ServiceExists(t *testing.T) {
	t.Skip("Requires mocking UpdatePod - covered by integration tests")
}

// TestEngineAddPod_ServiceCreating tests pod buffering during service creation
func TestEngineAddPod_ServiceCreating(t *testing.T) {
	dt := newTestDiffTracker()
	egressUID := "egress-2"

	// Setup: service is being created
	dt.pendingServiceOps[egressUID] = &ServiceOperationState{
		ServiceUID: egressUID,
		Config:     NewOutboundServiceConfig(egressUID, nil),
		State:      StateCreationInProgress,
		RetryCount: 0,
	}

	// Execute
	dt.AddPod(egressUID, "ns1/pod2", "node2", "10.0.0.2")

	// Verify pod was buffered
	buffered, exists := dt.pendingPods[egressUID]
	assert.True(t, exists, "Pods should be buffered")
	assert.Greater(t, len(buffered), 0, "Should have buffered pod updates")

	// Verify no immediate trigger
	select {
	case <-dt.locationsUpdaterTrigger:
		t.Error("Unexpected LocationsUpdater trigger - pods should be buffered")
	default:
		// Expected
	}
}

// makeInboundConfig builds an InboundConfig with the given TCP frontend ports
// and matching backend ports, for tests.
func makeInboundConfig(frontendPorts ...int32) *InboundConfig {
	cfg := &InboundConfig{}
	for _, p := range frontendPorts {
		cfg.FrontendPorts = append(cfg.FrontendPorts, PortMapping{Port: p, Protocol: "TCP"})
		cfg.BackendPorts = append(cfg.BackendPorts, PortMapping{Port: p, Protocol: "TCP"})
	}
	return cfg
}

func TestInboundConfig_Equals(t *testing.T) {
	a := makeInboundConfig(80, 443)
	b := makeInboundConfig(80, 443)
	assert.True(t, a.Equals(b), "identical configs should be equal")

	c := makeInboundConfig(80, 8080)
	assert.False(t, a.Equals(c), "different ports should be unequal")

	d := makeInboundConfig(443, 80)
	assert.False(t, a.Equals(d), "ordered comparison: reversed ports must be unequal")

	assert.True(t, (*InboundConfig)(nil).Equals(nil), "nil-nil equal")
	assert.False(t, a.Equals(nil), "non-nil vs nil unequal")
}

// TestEngineUpdateService_NewServiceFallsThrough verifies UpdateService delegates
// to AddService when the service is not yet known.
func TestEngineUpdateService_NewServiceFallsThrough(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-new"

	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(80)))

	opState, exists := dt.pendingServiceOps[uid]
	assert.True(t, exists, "service should be tracked via AddService fallthrough")
	assert.Equal(t, StateNotStarted, opState.State)

	select {
	case <-dt.serviceUpdaterTrigger:
	default:
		t.Error("expected serviceUpdater trigger")
	}
}

// TestEngineUpdateService_NoOpWhenConfigUnchanged verifies that an UpdateService call
// matching LastAppliedConfig does NOT change state and does NOT fire a trigger.
func TestEngineUpdateService_NoOpWhenConfigUnchanged(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-noop"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))

	applied := cfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:        uid,
		Config:            cfg,
		LastAppliedConfig: &applied,
		State:             StateCreated,
	}

	dt.UpdateService(cfg)

	opState := dt.pendingServiceOps[uid]
	assert.Equal(t, StateCreated, opState.State, "state should remain Created")
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Error("expected NO trigger for no-op update")
	default:
	}
}

// TestEngineUpdateService_PortChangeSchedulesUpdate verifies a port change on a
// service in StateCreated transitions to StateUpdateInProgress and triggers the updater.
func TestEngineUpdateService_PortChangeSchedulesUpdate(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-port"
	oldCfg := NewInboundServiceConfig(uid, makeInboundConfig(80))

	appliedCopy := oldCfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:        uid,
		Config:            oldCfg,
		LastAppliedConfig: &appliedCopy,
		State:             StateCreated,
	}

	newCfg := NewInboundServiceConfig(uid, makeInboundConfig(8080))
	dt.UpdateService(newCfg)

	opState := dt.pendingServiceOps[uid]
	assert.Equal(t, StateUpdateInProgress, opState.State)
	assert.True(t, opState.Config.InboundConfig.Equals(newCfg.InboundConfig))

	select {
	case <-dt.serviceUpdaterTrigger:
	default:
		t.Error("expected serviceUpdater trigger")
	}
}

// TestEngineUpdateService_LBInNRPNoTrackingEntry verifies that when an LB exists in
// NRP but has no pendingServiceOps entry (e.g., post-restart recovery), UpdateService
// creates an entry in StateUpdateInProgress and triggers.
func TestEngineUpdateService_LBInNRPNoTrackingEntry(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-recovered"
	dt.NRPResources.LoadBalancers.Insert(uid)

	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(80)))

	opState, exists := dt.pendingServiceOps[uid]
	assert.True(t, exists)
	assert.Equal(t, StateUpdateInProgress, opState.State)
	select {
	case <-dt.serviceUpdaterTrigger:
	default:
		t.Error("expected serviceUpdater trigger")
	}
}

// TestEngineUpdateService_IgnoredDuringDeletion verifies UpdateService is a no-op when
// the service is already being deleted.
func TestEngineUpdateService_IgnoredDuringDeletion(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-deleting"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     cfg,
		State:      StateDeletionPending,
	}

	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(8080)))

	opState := dt.pendingServiceOps[uid]
	assert.Equal(t, StateDeletionPending, opState.State, "deletion state must not be overwritten")
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Error("expected NO trigger when service is being deleted")
	default:
	}
}

// TestEngineUpdateService_OverwritesConfigDuringCreation verifies that an UpdateService
// call during creation simply overwrites the desired Config, with no state change.
func TestEngineUpdateService_OverwritesConfigDuringCreation(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-during-create"
	oldCfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     oldCfg,
		State:      StateCreationInProgress,
	}

	newCfg := NewInboundServiceConfig(uid, makeInboundConfig(8080))
	dt.UpdateService(newCfg)

	opState := dt.pendingServiceOps[uid]
	assert.Equal(t, StateCreationInProgress, opState.State)
	assert.True(t, opState.Config.InboundConfig.Equals(newCfg.InboundConfig),
		"Config should be overwritten with the latest desired state")
}

// TestIsServiceTracked covers the four code paths.
func TestIsServiceTracked(t *testing.T) {
	dt := newTestDiffTracker()
	assert.False(t, dt.IsServiceTracked("missing"))

	dt.NRPResources.LoadBalancers.Insert("lb-1")
	assert.True(t, dt.IsServiceTracked("lb-1"))

	dt.NRPResources.NATGateways.Insert("nat-1")
	assert.True(t, dt.IsServiceTracked("nat-1"))

	dt.pendingServiceOps["pending-1"] = &ServiceOperationState{ServiceUID: "pending-1"}
	assert.True(t, dt.IsServiceTracked("pending-1"))
}

// TestIsServiceReady_StateUpdateInProgress verifies that a service in StateUpdateInProgress
// is considered ready by the location-sync layer (LB and SGW Service entry are stable
// during port-only updates).
func TestIsServiceReady_StateUpdateInProgress(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-ready-update"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateUpdateInProgress,
	}
	assert.True(t, dt.isServiceReady(uid, true), "StateUpdateInProgress should be ready for inbound sync")

	// And StateCreated should still work.
	dt.pendingServiceOps[uid].State = StateCreated
	assert.True(t, dt.isServiceReady(uid, true), "StateCreated should be ready for inbound sync")

	// StateCreationInProgress should NOT be ready.
	dt.pendingServiceOps[uid].State = StateCreationInProgress
	assert.False(t, dt.isServiceReady(uid, true), "StateCreationInProgress should NOT be ready")
}

// TestEngineDeleteService_DuringUpdate verifies that DeleteService correctly transitions
// a StateUpdateInProgress service to StateDeletionPending and clears InFlightConfig.
func TestEngineDeleteService_DuringUpdate(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-delete-during-update"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	inflight := cfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:     uid,
		Config:         cfg,
		InFlightConfig: &inflight,
		State:          StateUpdateInProgress,
	}
	dt.NRPResources.LoadBalancers.Insert(uid)
	// Pre-populate a location so deletion goes to pending (vs immediate StateDeletionInProgress)
	dt.NRPResources.Locations["loc1"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.0.0.1": {Services: utilsets.NewString(uid)},
		},
	}

	dt.DeleteService(uid, true, false)

	opState := dt.pendingServiceOps[uid]
	assert.Equal(t, StateDeletionPending, opState.State, "should transition to DeletionPending")
	assert.Nil(t, opState.InFlightConfig, "InFlightConfig should be cleared")

	_, queued := dt.pendingServiceDeletions[uid]
	assert.True(t, queued, "service should be queued in pendingServiceDeletions")
}

// TestEngineOnServiceCreationComplete_DeletionPendingAfterUpdate verifies that when an
// update completes (success or failure) but DeleteService raced to StateDeletionPending,
// the deletion flow takes over uniformly via the pre-empt block at the top of
// OnServiceCreationComplete.
func TestEngineOnServiceCreationComplete_DeletionPendingAfterUpdate(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-then-delete"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     cfg,
		State:      StateDeletionPending, // simulating: DeleteService ran during update
	}
	// No locations -> immediate deletion should be triggered
	dt.pendingServiceDeletions[uid] = &PendingServiceDeletion{ServiceUID: uid, IsInbound: true}

	dt.OnServiceCreationComplete(uid, true, nil)

	opState := dt.pendingServiceOps[uid]
	assert.Equal(t, StateDeletionInProgress, opState.State, "should advance to DeletionInProgress when no locations remain")

	_, stillPending := dt.pendingServiceDeletions[uid]
	assert.False(t, stillPending, "pendingServiceDeletions entry should be consumed")
}

// TestEngineOnServiceCreationComplete_UpdateFailureKeepsState verifies that on update
// failure with state still in StateUpdateInProgress, the state is NOT reset (caller will
// retry from StateUpdateInProgress).
func TestEngineOnServiceCreationComplete_UpdateFailureKeepsState(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-failure"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	inflight := cfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:     uid,
		Config:         cfg,
		InFlightConfig: &inflight,
		State:          StateUpdateInProgress,
	}

	dt.OnServiceCreationComplete(uid, false, assertErr("simulated"))

	opState := dt.pendingServiceOps[uid]
	assert.Equal(t, StateUpdateInProgress, opState.State, "state must remain StateUpdateInProgress for retry")
	assert.Equal(t, 1, opState.RetryCount, "retry count incremented")
	assert.Nil(t, opState.InFlightConfig, "InFlightConfig cleared on failure")
}

// TestEngineOnServiceCreationComplete_UpdateSuccessPersistsConfig verifies that on
// update success, LastAppliedConfig is set to the in-flight snapshot and state returns
// to StateCreated.
func TestEngineOnServiceCreationComplete_UpdateSuccessPersistsConfig(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-success"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(8080))
	inflight := cfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:     uid,
		Config:         cfg,
		InFlightConfig: &inflight,
		State:          StateUpdateInProgress,
	}

	dt.OnServiceCreationComplete(uid, true, nil)

	opState := dt.pendingServiceOps[uid]
	assert.Equal(t, StateCreated, opState.State)
	assert.Nil(t, opState.InFlightConfig)
	if assert.NotNil(t, opState.LastAppliedConfig, "LastAppliedConfig must be persisted") {
		assert.True(t, opState.LastAppliedConfig.InboundConfig.Equals(makeInboundConfig(8080)))
	}
}

// TestEngineOnServiceCreationComplete_UpdateDriftReschedules verifies that when the
// desired Config drifts during an in-flight update, the completion handler reschedules
// another StateUpdateInProgress run.
func TestEngineOnServiceCreationComplete_UpdateDriftReschedules(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-drift"
	desired := NewInboundServiceConfig(uid, makeInboundConfig(8080))
	inflight := NewInboundServiceConfig(uid, makeInboundConfig(80)) // older
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:     uid,
		Config:         desired,
		InFlightConfig: &inflight,
		State:          StateUpdateInProgress,
	}

	dt.OnServiceCreationComplete(uid, true, nil)

	opState := dt.pendingServiceOps[uid]
	assert.Equal(t, StateUpdateInProgress, opState.State, "drift should re-enter StateUpdateInProgress")
	assert.Nil(t, opState.InFlightConfig, "InFlightConfig cleared between dispatch cycles")
	if assert.NotNil(t, opState.LastAppliedConfig) {
		// What we last APPLIED is the inflight config (port 80), not the desired (port 8080).
		assert.True(t, opState.LastAppliedConfig.InboundConfig.Equals(makeInboundConfig(80)))
	}

	select {
	case <-dt.serviceUpdaterTrigger:
	default:
		t.Error("expected serviceUpdater trigger after drift detection")
	}
}

// assertErr is a tiny error helper for tests.
type assertErr string

func (a assertErr) Error() string { return string(a) }
