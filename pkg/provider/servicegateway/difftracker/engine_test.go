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
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// Helper function to create a test DiffTracker
func newTestDiffTracker() *DiffTracker {
	return &DiffTracker{
		NRPResources: NRPState{
			LoadBalancers: utilsets.NewString(),
			NATGateways:   utilsets.NewString(),
			Locations:     make(map[string]NRPLocation),
		},
		K8sResources: K8sState{
			Services: utilsets.NewString(),
			Egresses: utilsets.NewString(),
			Nodes:    make(map[string]Node),
		},
		pendingServiceOps:       make(map[string]*ServiceOperationState),
		pendingEndpoints:        make(map[string][]PendingEndpointUpdate),
		pendingPods:             make(map[string][]PendingPodUpdate),
		pendingServiceDeletions: make(map[string]*PendingServiceDeletion),
		pendingPodDeletions:     make(map[string]*PendingPodDeletion),
		serviceUpdaterTrigger:   make(chan bool, 1),
		locationsUpdaterTrigger: make(chan bool, 1),
	}
}

func TestReconcileInboundService(t *testing.T) {
	newService := func(uid string, port int32) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "test-namespace",
				Name:      "test-service",
				UID:       types.UID(uid),
			},
			Spec: v1.ServiceSpec{
				Ports: []v1.ServicePort{{
					Port:     port,
					Protocol: v1.ProtocolTCP,
				}},
			},
		}
	}

	t.Run("adds a new service with provider identity", func(t *testing.T) {
		dt := newTestDiffTracker()
		service := newService("SERVICE-UID", 80)

		err := dt.ReconcileInboundService(service)

		if !assert.NoError(t, err) {
			t.FailNow()
		}
		opState, exists := dt.pendingServiceOps["service-uid"]
		if !assert.True(t, exists) {
			t.FailNow()
		}
		assert.Equal(t, service.Namespace, opState.Config.Namespace)
		assert.Equal(t, service.Name, opState.Config.Name)
		assert.Equal(t, int32(80), opState.Config.InboundConfig.FrontendPorts[0].Port)
	})

	t.Run("updates an already tracked service", func(t *testing.T) {
		dt := newTestDiffTracker()
		oldConfig := NewInboundServiceConfig("service-uid", &InboundConfig{
			FrontendPorts: []PortMapping{{Port: 80, Protocol: string(v1.ProtocolTCP)}},
			BackendPorts:  []PortMapping{{Port: 80, Protocol: string(v1.ProtocolTCP)}},
		})
		dt.pendingServiceOps["service-uid"] = &ServiceOperationState{
			ServiceUID:        "service-uid",
			Config:            oldConfig,
			LastAppliedConfig: &oldConfig,
			State:             StateCreated,
		}
		service := newService("service-uid", 443)

		err := dt.ReconcileInboundService(service)

		if !assert.NoError(t, err) {
			t.FailNow()
		}
		opState := dt.pendingServiceOps["service-uid"]
		assert.Equal(t, StateUpdateInProgress, opState.State)
		assert.Equal(t, int32(443), opState.Config.InboundConfig.FrontendPorts[0].Port)
		assert.Equal(t, service.Namespace, opState.Config.Namespace)
		assert.Equal(t, service.Name, opState.Config.Name)
	})

	t.Run("rejects an internal load balancer with event metadata", func(t *testing.T) {
		dt := newTestDiffTracker()
		service := newService("service-uid", 80)
		service.Annotations = map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal: consts.TrueAnnotationValue,
		}

		err := dt.ReconcileInboundService(service)

		var warningErr WarningEventError
		if assert.ErrorAs(t, err, &warningErr) {
			reason, message := warningErr.WarningEvent()
			assert.Equal(t, "UnsupportedInternalLoadBalancer", reason)
			assert.Contains(t, message, consts.ServiceAnnotationLoadBalancerInternal)
		}
		assert.False(t, dt.IsServiceTracked("service-uid"))
	})

	t.Run("ignores a port-less service", func(t *testing.T) {
		dt := newTestDiffTracker()
		service := newService("service-uid", 80)
		service.Spec.Ports = nil

		err := dt.ReconcileInboundService(service)

		assert.NoError(t, err)
		assert.False(t, dt.IsServiceTracked("service-uid"))
	})

	t.Run("rejects a missing service identity", func(t *testing.T) {
		dt := newTestDiffTracker()

		err := dt.ReconcileInboundService(newService("", 80))

		assert.Error(t, err)
	})
}

func TestDeleteInboundService(t *testing.T) {
	newService := func(uid string) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "test-namespace",
				Name:      "test-service",
				UID:       types.UID(uid),
			},
		}
	}

	t.Run("deletes the lowercased inbound service identity", func(t *testing.T) {
		dt := newTestDiffTracker()
		dt.NRPResources.LoadBalancers.Insert("service-uid")

		err := dt.DeleteInboundService(newService("SERVICE-UID"))

		if !assert.NoError(t, err) {
			t.FailNow()
		}
		opState, exists := dt.pendingServiceOps["service-uid"]
		if !assert.True(t, exists) {
			t.FailNow()
		}
		assert.True(t, opState.Config.IsInbound)
		assert.Equal(t, StateDeletionInProgress, opState.State)
	})

	t.Run("ignores an inbound service absent from engine and NRP state", func(t *testing.T) {
		dt := newTestDiffTracker()

		err := dt.DeleteInboundService(newService("service-uid"))

		assert.NoError(t, err)
		assert.False(t, dt.IsServiceTracked("service-uid"))
	})

	t.Run("rejects a missing service identity", func(t *testing.T) {
		dt := newTestDiffTracker()

		err := dt.DeleteInboundService(newService(""))

		assert.Error(t, err)
	})
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

// TestDeletePod_BufferedStaleDeleteKeepsSameIPReplacement checks that a delayed delete for one
// buffered pod does not cancel a different pod that reused the same IP while both are buffered for an
// in-flight egress service, which would strand the live replacement without egress.
func TestDeletePod_BufferedStaleDeleteKeepsSameIPReplacement(t *testing.T) {
	dt := newTestDiffTracker()
	const (
		egressUID = "egress-reuse"
		location  = "10.0.0.1"
		address   = "10.244.0.11"
	)

	// Service is mid-creation, so pods buffer rather than reaching live state.
	dt.pendingServiceOps[egressUID] = &ServiceOperationState{
		ServiceUID: egressUID,
		Config:     NewOutboundServiceConfig(egressUID, nil),
		State:      StateCreationInProgress,
	}

	// Old pod buffers the address; its IP is then reused by a new pod that also buffers it.
	dt.AddPod(egressUID, "ns1/old-pod", location, address)
	dt.AddPod(egressUID, "ns1/new-pod", location, address)
	assert.Len(t, dt.pendingPods[egressUID], 2, "both pods must be buffered")

	// A delayed delete for the OLD pod arrives.
	dt.DeletePod(egressUID, location, []string{address}, "ns1", "old-pod", "uid-old")

	buffered := dt.pendingPods[egressUID]
	if assert.Len(t, buffered, 1, "only the old pod's buffered entry must be cancelled, not the same-IP replacement") {
		assert.Equal(t, "ns1/new-pod", buffered[0].PodKey,
			"the same-IP replacement pod must remain buffered so it still gets egress on creation")
	}
}

// TestDeletePod_BufferedDeleteWithoutIdentityCancelsByAddress checks the identity-less fallback: a
// caller with no namespace/name cancels buffered entries by (location,address).
func TestDeletePod_BufferedDeleteWithoutIdentityCancelsByAddress(t *testing.T) {
	dt := newTestDiffTracker()
	const (
		egressUID = "egress-noident"
		location  = "10.0.0.1"
		address   = "10.244.0.12"
	)

	dt.pendingServiceOps[egressUID] = &ServiceOperationState{
		ServiceUID: egressUID,
		Config:     NewOutboundServiceConfig(egressUID, nil),
		State:      StateCreationInProgress,
	}
	dt.AddPod(egressUID, "ns1/pod", location, address)
	assert.Len(t, dt.pendingPods[egressUID], 1)

	// Empty namespace/name -> address-only cancellation.
	dt.DeletePod(egressUID, location, []string{address}, "", "", "")

	assert.Empty(t, dt.pendingPods[egressUID], "an identity-less delete must still cancel the buffered entry by address")
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
	assert.True(t, dt.isServiceReadyToSync(uid, true), "StateUpdateInProgress should be ready for inbound sync")

	// And StateCreated should still work.
	dt.pendingServiceOps[uid].State = StateCreated
	assert.True(t, dt.isServiceReadyToSync(uid, true), "StateCreated should be ready for inbound sync")

	// StateCreationInProgress should NOT be ready.
	dt.pendingServiceOps[uid].State = StateCreationInProgress
	assert.False(t, dt.isServiceReadyToSync(uid, true), "StateCreationInProgress should NOT be ready")
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
	assert.NotNil(t, opState.InFlightConfig, "InFlightConfig must be preserved so OnServiceCreationComplete pre-empt routes to deletion")

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

// TestEngineUpdateEndpoints_RemovalDuringCreationIsReplayed verifies that an endpoint
// removed while a service is still being created is not resurrected once creation
// completes. Endpoint events are buffered during StateCreationInProgress and replayed
// on completion; the replay must apply both additions and removals (not a union of the
// "new" snapshots), otherwise a removed pod IP would leak into the synced state.
func TestEngineUpdateEndpoints_RemovalDuringCreationIsReplayed(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "svc-endpoints-create"
	node := "node1"
	keep, removed := "10.0.0.1", "10.0.0.2"

	dt.AddService(NewInboundServiceConfig(serviceUID, nil))

	// While creating: both endpoints appear, then one is removed.
	dt.UpdateEndpoints(serviceUID, nil, map[string]string{keep: node, removed: node})
	dt.UpdateEndpoints(serviceUID, map[string]string{keep: node, removed: node}, map[string]string{keep: node})

	dt.OnServiceCreationComplete(serviceUID, true, nil)

	dt.mu.Lock()
	defer dt.mu.Unlock()
	n, ok := dt.K8sResources.Nodes[node]
	assert.True(t, ok, "node should exist after promotion")
	_, hasKeep := n.Pods[keep]
	_, hasRemoved := n.Pods[removed]
	assert.True(t, hasKeep, "surviving endpoint should be present after creation completes")
	assert.False(t, hasRemoved, "endpoint removed during creation must not be present after creation completes")
}

// TestEngineAddPod_DeletedDuringCreationNotResurrected verifies that an egress pod
// deleted while its service is still being created is not resurrected when creation
// completes. Such a pod lives only in the pending buffer (no ref-count entry yet), so
// DeletePod must cancel the buffered add.
func TestEngineAddPod_DeletedDuringCreationNotResurrected(t *testing.T) {
	dt := newTestDiffTracker()
	egressUID := "egress-create-delete"
	location, address := "192.168.0.1", "10.0.0.5"

	dt.AddPod(egressUID, "ns/pod", location, address) // buffers + creates StateNotStarted op
	dt.DeletePod(egressUID, location, []string{address}, "ns", "pod", "")
	dt.OnServiceCreationComplete(egressUID, true, nil)

	dt.mu.Lock()
	defer dt.mu.Unlock()
	if n, ok := dt.K8sResources.Nodes[location]; ok {
		_, live := n.Pods[address]
		assert.False(t, live, "pod deleted during creation must not be resurrected after promotion")
	}
	if v, ok := dt.outboundIdentityPodRefCount.Load(egressUID); ok {
		assert.Equal(t, 0, v.(int), "ref-count must not drift after cancelled buffered pod")
	}
}

// TestEngineAddPod_DuringDeletionPendingRevivesService verifies that a pod arriving
// while an egress service is pending deletion (e.g. the sole pod changing its IP, which
// the informer delivers as remove-then-add) revives the service instead of being dropped,
// avoiding an outbound outage when the NAT Gateway would otherwise be deleted.
func TestEngineAddPod_DuringDeletionPendingRevivesService(t *testing.T) {
	dt := newTestDiffTracker()
	egressUID := "egress-revive"
	location := "192.168.0.1"
	dt.NRPResources.NATGateways.Insert(egressUID)
	dt.pendingServiceOps[egressUID] = &ServiceOperationState{
		ServiceUID: egressUID,
		Config:     NewOutboundServiceConfig(egressUID, nil),
		State:      StateCreated,
	}

	dt.AddPod(egressUID, "ns/pod", location, "10.0.0.10")
	res := dt.DeletePod(egressUID, location, []string{"10.0.0.10"}, "ns", "pod", "")
	assert.True(t, res.IsLastPod, "removing the sole pod should be the last-pod case")
	assert.Equal(t, StateDeletionPending, dt.pendingServiceOps[egressUID].State)

	// New pod (same pod, new IP) arrives while the service is pending deletion.
	dt.AddPod(egressUID, "ns/pod", location, "10.0.0.11")

	dt.mu.Lock()
	defer dt.mu.Unlock()
	assert.Equal(t, StateCreated, dt.pendingServiceOps[egressUID].State, "service should be revived")
	_, stillPending := dt.pendingServiceDeletions[egressUID]
	assert.False(t, stillPending, "pending deletion should be cancelled on revive")
	n, ok := dt.K8sResources.Nodes[location]
	assert.True(t, ok)
	_, live := n.Pods["10.0.0.11"]
	assert.True(t, live, "the new pod should be live after revive (no egress outage)")
	v, ok := dt.outboundIdentityPodRefCount.Load(egressUID)
	assert.True(t, ok)
	assert.Equal(t, 1, v.(int), "ref-count should be 1 after revive")
}

// TestEngineOnServiceCreationComplete_TerminalErrorParksAndRecovers verifies that a
// deterministic (non-retryable) creation failure parks the service rather than retrying
// forever, that a transient failure still retries, and that fixing the Service spec via
// UpdateService clears the park and re-attempts creation.
func TestEngineOnServiceCreationComplete_TerminalErrorParksAndRecovers(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "svc-terminal"
	badConfig := NewInboundServiceConfig(serviceUID, &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 65535, Protocol: "TCP"}},
	})
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		Config:     badConfig,
		State:      StateCreationInProgress,
	}

	dt.OnServiceCreationComplete(serviceUID, false, newTerminalError(errors.New("frontend port 65535 out of range")))

	op := dt.pendingServiceOps[serviceUID]
	assert.True(t, op.CreationFailedTerminal, "deterministic error should park the service")
	assert.Len(t, dt.serviceUpdaterTrigger, 0, "parked service must not trigger a retry")

	// Fixing the spec re-attempts creation.
	goodConfig := NewInboundServiceConfig(serviceUID, &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
	})
	dt.UpdateService(goodConfig)
	assert.False(t, dt.pendingServiceOps[serviceUID].CreationFailedTerminal, "spec fix should clear the park")
	assert.Len(t, dt.serviceUpdaterTrigger, 1, "spec fix should trigger a fresh creation attempt")
}

// TestEngineOnServiceCreationComplete_TransientErrorRetries verifies a transient failure
// is still retried (not parked).
func TestEngineOnServiceCreationComplete_TransientErrorRetries(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "svc-transient"
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		Config:     NewInboundServiceConfig(serviceUID, nil),
		State:      StateCreationInProgress,
	}

	dt.OnServiceCreationComplete(serviceUID, false, errors.New("throttled, try again"))

	op := dt.pendingServiceOps[serviceUID]
	assert.False(t, op.CreationFailedTerminal, "transient error must not park the service")
	assert.Greater(t, op.RetryCount, 0, "transient error should increment retry count")
	assert.Len(t, dt.serviceUpdaterTrigger, 1, "transient error should trigger a retry")
}

// TestEngineOnServiceCreationComplete_DeleteDuringCreateRoutesToDeletion verifies that when a
// Delete arrives while a create is in flight (and the service has no NRP locations yet, so it
// is moved straight to StateDeletionInProgress), the create's success completion is routed to a
// real delete rather than being mistaken for a delete success. Otherwise the freshly created
// LB/PIP would be leaked and the Service left stuck terminating.
func TestEngineOnServiceCreationComplete_DeleteDuringCreateRoutesToDeletion(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-delete-during-create"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	inflight := cfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:     uid,
		Config:         cfg,
		InFlightConfig: &inflight,
		State:          StateCreationInProgress,
	}

	dt.DeleteService(uid, true, false)
	assert.Equal(t, StateDeletionInProgress, dt.pendingServiceOps[uid].State)

	dt.NRPResources.LoadBalancers.Insert(uid) // the in-flight create produced an LB
	drainTrigger(dt.serviceUpdaterTrigger)
	dt.OnServiceCreationComplete(uid, true, nil)

	opState, tracked := dt.pendingServiceOps[uid]
	assert.True(t, tracked, "tracking must not be dropped as a phantom delete success")
	assert.Equal(t, StateDeletionInProgress, opState.State, "the create success must be routed to a real delete")
	assert.Len(t, dt.serviceUpdaterTrigger, 1, "a delete must be dispatched")
}

// TestEngineOnServiceCreationComplete_GenuineDeletionCleansUp verifies that a real delete
// completion (no in-flight create/update config) still clears all tracking.
func TestEngineOnServiceCreationComplete_GenuineDeletionCleansUp(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-genuine-delete"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, nil),
		State:      StateDeletionInProgress,
	}
	dt.pendingServiceDeletions[uid] = &PendingServiceDeletion{ServiceUID: uid, IsInbound: true}

	dt.OnServiceCreationComplete(uid, true, nil)

	_, tracked := dt.pendingServiceOps[uid]
	assert.False(t, tracked, "a genuine delete success must clear tracking")
}

// TestEngineDeletePod_StaleDuplicateRemovalIsNoOp verifies that a delete event for a pod that
// is not in live state (a stale or duplicate informer delivery) is a no-op even when another
// pod still holds the ref-count at one, instead of being mistaken for the last-pod removal.
func TestEngineDeletePod_StaleDuplicateRemovalIsNoOp(t *testing.T) {
	dt := newTestDiffTracker()
	egressUID := "egress-stale-delete"
	dt.NRPResources.NATGateways.Insert(egressUID)
	dt.pendingServiceOps[egressUID] = &ServiceOperationState{
		ServiceUID: egressUID,
		Config:     NewOutboundServiceConfig(egressUID, nil),
		State:      StateCreated,
	}
	dt.AddPod(egressUID, "ns/live", "192.168.0.1", "10.0.0.1")

	res := dt.DeletePod(egressUID, "192.168.0.2", []string{"10.0.0.2"}, "ns", "gone", "")
	assert.False(t, res.IsLastPod, "a removal matching no live pod must not be the last-pod case")
	assert.False(t, res.Enqueued, "a stale/untracked delete must NOT enqueue a pending pod deletion; the caller removes the finalizer directly so the pod is not stranded")
	_, marked := dt.pendingServiceDeletions[egressUID]
	assert.False(t, marked, "the service must not be marked for deletion while a live pod remains")
	assert.Equal(t, StateCreated, dt.pendingServiceOps[egressUID].State)

	res = dt.DeletePod(egressUID, "192.168.0.1", []string{"10.0.0.1"}, "ns", "live", "")
	assert.True(t, res.IsLastPod, "the genuine last-pod removal must still tear the service down")
	assert.True(t, res.Enqueued, "a tracked pod removal must enqueue a pending pod deletion for drain-gated finalizer removal")
	_, marked = dt.pendingServiceDeletions[egressUID]
	assert.True(t, marked)
}

// TestEngineDeletePod_DualStackRemovesAllAddressesAtomically verifies that a dual-stack egress pod,
// registered as one AddPod call per IP family, is removed by a single atomic DeletePod call carrying
// every address, producing exactly one drain-gated PendingPodDeletion (one pod object carries one
// finalizer). The record must carry both addresses and be marked last-pod once the ref-count empties.
func TestEngineDeletePod_DualStackRemovesAllAddressesAtomically(t *testing.T) {
	dt := newTestDiffTracker()
	egressUID := "egress-dualstack"
	dt.NRPResources.NATGateways.Insert(egressUID)
	dt.pendingServiceOps[egressUID] = &ServiceOperationState{
		ServiceUID: egressUID,
		Config:     NewOutboundServiceConfig(egressUID, nil),
		State:      StateCreated,
	}

	const nodeIP, v4, v6 = "192.168.0.1", "10.0.0.1", "fd00::1"
	dt.AddPod(egressUID, "ns/ds", nodeIP, v4)
	dt.AddPod(egressUID, "ns/ds", nodeIP, v6)

	refCount, ok := dt.outboundIdentityPodRefCount.Load(egressUID)
	assert.True(t, ok)
	assert.Equal(t, 2, refCount.(int), "a dual-stack pod registers one address per IP family")
	assert.Len(t, dt.K8sResources.Nodes[nodeIP].Pods, 2, "both pod addresses must be tracked under the node")

	res := dt.DeletePod(egressUID, nodeIP, []string{v4, v6}, "ns", "ds", "uid-ds")
	assert.True(t, res.IsLastPod, "removing every address empties the service")
	assert.True(t, res.Enqueued)

	assert.Len(t, dt.pendingPodDeletions, 1, "both addresses must be recorded once (one pod, one finalizer)")
	entry := dt.pendingPodDeletions["ns/ds"]
	if assert.NotNil(t, entry) {
		assert.ElementsMatch(t, []string{v4, v6}, entry.Addresses, "the record must carry every address of the pod")
		assert.True(t, entry.IsLastPod, "IsLastPod must be set once the pod's removal empties the ref-count")
		assert.Equal(t, "uid-ds", entry.UID)
	}
	_, refExists := dt.outboundIdentityPodRefCount.Load(egressUID)
	assert.False(t, refExists, "the egress ref-count must be fully drained after both addresses are removed")
}

// TestEngineAddPod_DuringDeletionInProgressBuffersForRecreate verifies that a pod arriving
// while a NAT Gateway delete is in flight is buffered (not dropped), and that the service is
// re-created and the pod promoted once the deletion completes, avoiding an egress outage.
func TestEngineAddPod_DuringDeletionInProgressBuffersForRecreate(t *testing.T) {
	dt := newTestDiffTracker()
	egressUID := "egress-recreate"
	dt.NRPResources.NATGateways.Insert(egressUID)
	dt.pendingServiceOps[egressUID] = &ServiceOperationState{
		ServiceUID: egressUID,
		Config:     NewOutboundServiceConfig(egressUID, nil),
		State:      StateDeletionInProgress,
	}
	dt.pendingServiceDeletions[egressUID] = &PendingServiceDeletion{ServiceUID: egressUID, IsInbound: false}

	dt.AddPod(egressUID, "ns/pod", "192.168.0.1", "10.0.0.9")
	assert.Len(t, dt.pendingPods[egressUID], 1, "the pod must be buffered for re-creation")

	drainTrigger(dt.serviceUpdaterTrigger)
	dt.OnServiceCreationComplete(egressUID, true, nil)
	op, tracked := dt.pendingServiceOps[egressUID]
	assert.True(t, tracked, "a service with a buffered pod must be re-created, not torn down")
	assert.Equal(t, StateNotStarted, op.State)
	assert.Len(t, dt.pendingPods[egressUID], 1)
	assert.Len(t, dt.serviceUpdaterTrigger, 1)

	op.State = StateCreationInProgress
	dt.OnServiceCreationComplete(egressUID, true, nil)
	v, ok := dt.outboundIdentityPodRefCount.Load(egressUID)
	assert.True(t, ok)
	assert.Equal(t, 1, v.(int), "the buffered pod must be promoted to live egress after re-creation")
}

// TestEngineDeletePod_LastBufferedPodSchedulesDeletion verifies that deleting the only buffered
// (pre-creation) pod while creation is in flight schedules a deletion, so the create's success
// is routed to a real delete and the NAT Gateway is not leaked as a pod-less orphan.
func TestEngineDeletePod_LastBufferedPodSchedulesDeletion(t *testing.T) {
	dt := newTestDiffTracker()
	egressUID := "egress-buffered-last"

	dt.AddPod(egressUID, "ns/pod", "192.168.0.1", "10.0.0.5")
	dt.pendingServiceOps[egressUID].State = StateCreationInProgress
	snap := dt.pendingServiceOps[egressUID].Config
	dt.pendingServiceOps[egressUID].InFlightConfig = &snap

	dt.DeletePod(egressUID, "192.168.0.1", []string{"10.0.0.5"}, "ns", "pod", "")
	assert.Equal(t, StateDeletionInProgress, dt.pendingServiceOps[egressUID].State,
		"cancelling the only buffered pod mid-create must schedule deletion")

	drainTrigger(dt.serviceUpdaterTrigger)
	dt.OnServiceCreationComplete(egressUID, true, nil)
	assert.Equal(t, StateDeletionInProgress, dt.pendingServiceOps[egressUID].State)
	assert.Len(t, dt.serviceUpdaterTrigger, 1)
}

// TestEngineDeletePod_LastBufferedPodBeforeCreateAbortsService verifies that deleting the only
// buffered pod before creation is dispatched aborts the service entirely (no Azure resource was
// ever created).
func TestEngineDeletePod_LastBufferedPodBeforeCreateAbortsService(t *testing.T) {
	dt := newTestDiffTracker()
	egressUID := "egress-buffered-abort"

	dt.AddPod(egressUID, "ns/pod", "192.168.0.1", "10.0.0.6")
	dt.DeletePod(egressUID, "192.168.0.1", []string{"10.0.0.6"}, "ns", "pod", "")

	_, tracked := dt.pendingServiceOps[egressUID]
	assert.False(t, tracked, "cancelling the only buffered pod before creation must abort the service")
}

// TestEngineOnServiceCreationComplete_UpdateTerminalErrorParks verifies that a deterministic
// (non-retryable) failure during an update parks the service rather than retrying forever.
func TestEngineOnServiceCreationComplete_UpdateTerminalErrorParks(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-terminal"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateUpdateInProgress,
	}

	dt.OnServiceCreationComplete(uid, false, newTerminalError(errors.New("unsupported protocol SCTP")))

	op := dt.pendingServiceOps[uid]
	assert.True(t, op.CreationFailedTerminal, "a deterministic update failure must park the service")
	assert.Equal(t, StateNotStarted, op.State)
	assert.Len(t, dt.serviceUpdaterTrigger, 0, "a parked service must not trigger a retry")
}

// TestEngineInitializationCompletesWithConcurrentTriggers exercises the in-flight trigger
// accounting that gates WaitForInitialSync: a trigger fired during initialization must be
// counted before its token is observable, so a consumer that decrements and checks for
// completion concurrently can never observe a transient negative count and strand init.
func TestEngineInitializationCompletesWithConcurrentTriggers(t *testing.T) {
	const rounds = 20000
	for i := 0; i < rounds; i++ {
		dt := newTestDiffTracker()
		dt.initCompletionChecker = make(chan struct{})
		atomic.StoreInt32(&dt.isInitializing, 1)

		done := make(chan struct{})
		go func() {
			<-dt.locationsUpdaterTrigger
			atomic.AddInt32(&dt.pendingUpdaterTriggers, -1)
			dt.checkInitializationComplete()
			close(done)
		}()

		dt.triggerLocationsUpdater()
		<-done

		select {
		case <-dt.initCompletionChecker:
		default:
			t.Fatalf("round %d: initialization did not complete (in-flight counter=%d)",
				i, atomic.LoadInt32(&dt.pendingUpdaterTriggers))
		}
	}
}

// drainTrigger empties a cap-1 updater trigger channel so a test can assert whether a
// subsequent operation enqueues a fresh trigger.
func drainTrigger(ch chan bool) {
	for len(ch) > 0 {
		<-ch
	}
}

// TestOnServiceCreationCompleteClearsInFlightConfigOnFailure verifies that a failed create clears the
// in-flight config snapshot, matching the update path, so a later delete-completion is not misread as
// an in-flight create.
func TestOnServiceCreationCompleteClearsInFlightConfigOnFailure(t *testing.T) {
	dt := newTestDiffTracker()
	const uid = "svc-inflight"

	cfg := NewInboundServiceConfig(uid, makeInboundConfig(8080))
	op := &ServiceOperationState{
		ServiceUID:     uid,
		Config:         cfg,
		InFlightConfig: &cfg,
		State:          StateCreationInProgress,
	}
	dt.pendingServiceOps[uid] = op

	dt.OnServiceCreationComplete(uid, false, errors.New("transient failure"))

	assert.Nil(t, op.InFlightConfig, "a failed create must clear the in-flight config snapshot")
}

// TestRecreateAfterDeletion_ClearsCreationFailedTerminal asserts that deleting and recreating a
// service parked with a non-retryable creation error clears the terminal park, so the dispatcher
// provisions the new LoadBalancer instead of skipping it.
func TestRecreateAfterDeletion_ClearsCreationFailedTerminal(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-recreate-after-terminal-park"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:             uid,
		Config:                 NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:                  StateDeletionInProgress,
		CreationFailedTerminal: true,
		RecreateAfterDeletion:  true,
	}

	dt.OnServiceCreationComplete(uid, true, nil)

	op, ok := dt.pendingServiceOps[uid]
	if !assert.True(t, ok, "op must remain tracked to drive the recreate") {
		return
	}
	assert.Equal(t, StateNotStarted, op.State, "recreate branch must reset State to NotStarted")
	assert.False(t, op.RecreateAfterDeletion, "recreate flag must be cleared once consumed")
	assert.False(t, op.CreationFailedTerminal,
		"terminal park must be cleared on recreate, otherwise the dispatcher skips the op")

	su := newTestServiceUpdater(dt)
	su.processBatch()

	assert.Equal(t, StateCreationInProgress, dt.pendingServiceOps[uid].State,
		"dispatcher must transition the recreated op to CreationInProgress")
}

// TestDeleteService_RedundantDeleteDoesNotResurrectService asserts a second DeleteService on an
// already-deleting service clears recreate intent buffered by an interleaved UpdateService, so the
// deletion-success path does not resurrect a service meant to be deleted.
func TestDeleteService_RedundantDeleteDoesNotResurrectService(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-redundant-delete"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:            uid,
		Config:                NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:                 StateDeletionInProgress,
		RecreateAfterDeletion: true,
	}

	dt.DeleteService(uid, true, false)

	op, ok := dt.pendingServiceOps[uid]
	if !assert.True(t, ok, "op stays tracked while deletion is in flight") {
		return
	}
	assert.False(t, op.RecreateAfterDeletion,
		"a redundant delete of an already-deleting service must clear the recreate intent")

	dt.OnServiceCreationComplete(uid, true, nil)

	_, stillTracked := dt.pendingServiceOps[uid]
	assert.False(t, stillTracked,
		"deletion success must tear the service down after a user-issued delete, not recreate it")
}

// TestUpdateEndpoints_UntrackedServiceIsNotBuffered verifies endpoints for a Service that is neither
// tracked nor in NRP are dropped, not buffered: the informer fires for every Service, so buffering
// would grow pendingEndpoints unbounded. AddService re-seeds from the EndpointSlice cache.
func TestUpdateEndpoints_UntrackedServiceIsNotBuffered(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-untracked-clusterip"

	dt.UpdateEndpoints(uid, nil, map[string]string{"10.244.0.1": "10.0.0.1"})

	assert.Empty(t, dt.pendingEndpoints, "endpoints for an untracked, non-NRP service must not be buffered")
	assert.NotContains(t, dt.pendingEndpoints, uid)
}

// A terminating egress pod is delivered to the informer several times (deletionTimestamp
// set, then terminating status updates, duplicate deletes). Only the first event drains
// addresses; later events drain nothing but must still report Enqueued so the cleanup
// finalizer stays held until NRP drain completes. Otherwise the pod (and its egress IP)
// is reclaimed while NRP still maps the address to the NAT Gateway.
func TestEngineDeletePod_DuplicateEventKeepsDrainGate(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-nat"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid, Config: NewOutboundServiceConfig(uid, nil), State: StateCreated,
	}
	dt.NRPResources.NATGateways.Insert(uid)
	dt.AddPod(uid, "ns/pod-a", "10.0.0.1", "10.244.0.5")

	first := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.5"}, "ns", "pod-a", "pod-a-uid")
	assert.True(t, first.Enqueued, "first delete drains the address and must drain-gate the finalizer")

	second := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.5"}, "ns", "pod-a", "pod-a-uid")
	assert.False(t, second.IsLastPod, "duplicate delete drains nothing new")
	assert.True(t, second.Enqueued, "duplicate delete must keep the finalizer gated while the prior drain is pending")
	assert.Contains(t, dt.pendingPodDeletions, "ns/pod-a", "pending drain record must survive the duplicate event")
}

// A duplicate delete carrying a different UID (same-name replacement pod) must not be
// gated by the stale record left by the original pod, or the replacement's finalizer
// would be held against a drain that isn't its own.
func TestEngineDeletePod_ReplacementUIDNotGatedByStaleRecord(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-nat"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid, Config: NewOutboundServiceConfig(uid, nil), State: StateCreated,
	}
	dt.NRPResources.NATGateways.Insert(uid)
	dt.AddPod(uid, "ns/pod-a", "10.0.0.1", "10.244.0.5")
	dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.5"}, "ns", "pod-a", "pod-a-uid")

	replacement := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.5"}, "ns", "pod-a", "pod-b-uid")
	assert.False(t, replacement.Enqueued, "a different-UID delete must not inherit the prior pod's drain gate")
}

// TestEngineOnServiceCreationComplete_DriftDuringTerminalFailureReDispatches proves a terminal CREATE
// failure does not park the service when a mid-flight UpdateService already replaced the desired spec.
// The in-flight attempt used the old (invalid) spec; the failure is for that stale spec, so the new
// desired config must be re-dispatched (it may be valid) rather than parked forever.
func TestEngineOnServiceCreationComplete_DriftDuringTerminalFailureReDispatches(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-create-drift"
	inflight := NewInboundServiceConfig(uid, makeInboundConfig(80))  // stale spec that fails terminally
	desired := NewInboundServiceConfig(uid, makeInboundConfig(8080)) // new spec that landed mid-flight
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid, Config: desired, InFlightConfig: &inflight, State: StateCreationInProgress,
	}

	dt.OnServiceCreationComplete(uid, false, newTerminalError(errors.New("unsupported protocol")))

	op := dt.pendingServiceOps[uid]
	if op == nil {
		t.Fatalf("service must remain tracked after a terminal failure")
	}
	assert.False(t, op.CreationFailedTerminal,
		"a terminal failure for a stale spec must not park the service when the desired config drifted mid-flight")
	assert.Equal(t, StateNotStarted, op.State, "the drifted config must be re-dispatched")
	assert.True(t, configsEqualForUpdate(&op.Config, &desired), "the re-dispatch must target the new desired config")
}

// TestEngineOnServiceCreationComplete_DriftDuringTerminalUpdateReDispatches is the update-path analogue.
func TestEngineOnServiceCreationComplete_DriftDuringTerminalUpdateReDispatches(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-drift"
	inflight := NewInboundServiceConfig(uid, makeInboundConfig(80))
	desired := NewInboundServiceConfig(uid, makeInboundConfig(8080))
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid, Config: desired, InFlightConfig: &inflight, State: StateUpdateInProgress,
	}

	dt.OnServiceCreationComplete(uid, false, newTerminalError(errors.New("unsupported protocol")))

	op := dt.pendingServiceOps[uid]
	if op == nil {
		t.Fatalf("service must remain tracked after a terminal update failure")
	}
	assert.False(t, op.CreationFailedTerminal,
		"a terminal update failure for a stale spec must not park when the desired config drifted mid-flight")
	assert.Equal(t, StateNotStarted, op.State, "the drifted config must be re-dispatched")
	assert.True(t, configsEqualForUpdate(&op.Config, &desired), "the re-dispatch must target the new desired config")
}
