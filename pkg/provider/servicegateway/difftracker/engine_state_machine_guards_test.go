/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// State transition tests for the DiffTracker engine.

package difftracker

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// ================================================================================================
// LEGAL TRANSITIONS — UpdateService dispatch
// ================================================================================================

// UpdateService should dispatch correctly across known states.
func TestGuardStateTransitions_UpdateServiceDispatch(t *testing.T) {
	type row struct {
		name        string
		startState  ResourceState
		nrpHasLB    bool
		afterState  ResourceState
		mustTrigger bool
	}
	tests := []row{
		{
			name:        "StateNotStarted -> kept (config overwritten, no trigger)",
			startState:  StateNotStarted,
			nrpHasLB:    true,
			afterState:  StateNotStarted,
			mustTrigger: false,
		},
		{
			name:        "StateCreationInProgress -> kept (config overwritten, no trigger)",
			startState:  StateCreationInProgress,
			nrpHasLB:    true,
			afterState:  StateCreationInProgress,
			mustTrigger: false,
		},
		{
			name:        "StateCreated -> StateUpdateInProgress + trigger (config changed)",
			startState:  StateCreated,
			nrpHasLB:    true,
			afterState:  StateUpdateInProgress,
			mustTrigger: true,
		},
		{
			name:        "StateUpdateInProgress -> kept (config overwritten, no new trigger)",
			startState:  StateUpdateInProgress,
			nrpHasLB:    true,
			afterState:  StateUpdateInProgress,
			mustTrigger: false,
		},
		{
			name:        "StateDeletionPending -> kept; update ignored",
			startState:  StateDeletionPending,
			nrpHasLB:    true,
			afterState:  StateDeletionPending,
			mustTrigger: false,
		},
		{
			name:        "StateDeletionInProgress -> kept; update ignored",
			startState:  StateDeletionInProgress,
			nrpHasLB:    true,
			afterState:  StateDeletionInProgress,
			mustTrigger: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dt := newTestDiffTracker()
			uid := "svc-statetx"
			oldCfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
			dt.pendingServiceOps[uid] = &ServiceOperationState{
				ServiceUID:        uid,
				Config:            oldCfg,
				State:             tc.startState,
				LastAppliedConfig: &oldCfg,
			}
			if tc.nrpHasLB {
				dt.NRPResources.LoadBalancers.Insert(uid)
			}

			newCfg := NewInboundServiceConfig(uid, makeInboundConfig(8080))
			dt.UpdateService(newCfg)

			op := dt.pendingServiceOps[uid]
			assert.Equal(t, tc.afterState, op.State, "post-state mismatch")
			if tc.mustTrigger {
				assert.Len(t, dt.serviceUpdaterTrigger, 1, "expected ServiceUpdater trigger")
			} else {
				assert.Len(t, dt.serviceUpdaterTrigger, 0, "did NOT expect ServiceUpdater trigger")
			}
		})
	}
}

// ================================================================================================
// LEGAL TRANSITIONS — DeleteService dispatch
// ================================================================================================

// DeleteService should dispatch correctly across known states with locations.
func TestGuardStateTransitions_DeleteServiceDispatch(t *testing.T) {
	type row struct {
		name          string
		startState    ResourceState
		afterState    ResourceState
		mustBePending bool
	}
	tests := []row{
		{"StateNotStarted -> DeletionPending", StateNotStarted, StateDeletionPending, true},
		{"StateCreationInProgress -> DeletionPending", StateCreationInProgress, StateDeletionPending, true},
		{"StateCreated -> DeletionPending", StateCreated, StateDeletionPending, true},
		{"StateUpdateInProgress -> DeletionPending (preserves InFlightConfig)", StateUpdateInProgress, StateDeletionPending, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dt := newTestDiffTracker()
			uid := "svc-deltx"
			inflight := NewInboundServiceConfig(uid, makeInboundConfig(80))
			dt.pendingServiceOps[uid] = &ServiceOperationState{
				ServiceUID:     uid,
				Config:         inflight,
				InFlightConfig: &inflight,
				State:          tc.startState,
			}
			dt.NRPResources.LoadBalancers.Insert(uid)
			dt.NRPResources.Locations["loc"] = NRPLocation{
				Addresses: map[string]NRPAddress{
					"10.0.0.1": {Services: utilsets.NewString(uid)},
				},
			}
			// Seed a live K8s state pod so removeServiceFromK8sStateLocked does
			// not also empty the location and short-circuit to DeletionInProgress.
			node := newNode()
			pod := newPod()
			pod.InboundIdentities.Insert(uid)
			node.Pods["10.0.0.1"] = pod
			dt.K8sResources.Nodes["loc"] = node

			dt.DeleteService(uid, true, false)

			op := dt.pendingServiceOps[uid]
			assert.Equal(t, tc.afterState, op.State)
			if tc.startState == StateUpdateInProgress {
				assert.NotNil(t, op.InFlightConfig, "DeleteService during update must preserve InFlightConfig so OnServiceCreationComplete can route to deletion")
			}
			if tc.mustBePending {
				_, pending := dt.pendingServiceDeletions[uid]
				assert.True(t, pending, "service must be queued in pendingServiceDeletions")
			}
		})
	}
}

// ================================================================================================
// ILLEGAL / OUT-OF-ORDER TRANSITIONS — must be safe no-ops
// ================================================================================================

// Unknown states in UpdateService should be handled without panic.
func TestGuardStateTransitions_UpdateService_UnknownStateNoPanic(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-bad-state"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      ResourceState(99), // illegal
	}

	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(8080)))
	op := dt.pendingServiceOps[uid]
	assert.Equal(t, ResourceState(99), op.State, "unknown state must not be mutated")
	assert.Len(t, dt.serviceUpdaterTrigger, 0, "unknown state must NOT fire trigger")
}

// Unknown states in DeleteService should be handled without panic.
func TestGuardStateTransitions_DeleteService_UnknownStateNoPanic(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-bad-state-delete"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, nil),
		State:      ResourceState(99),
	}

	dt.DeleteService(uid, true, false)
	op := dt.pendingServiceOps[uid]
	assert.Equal(t, ResourceState(99), op.State, "unknown state must not be mutated")
	_, pending := dt.pendingServiceDeletions[uid]
	assert.False(t, pending, "unknown state must NOT enqueue pending deletion")
}

// Unknown states in AddPod should be a no-op.
func TestGuardStateTransitions_AddPod_UnknownStateIsNoOp(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-bad-state"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewOutboundServiceConfig(uid, nil),
		State:      ResourceState(99),
	}
	dt.AddPod(uid, "ns/p", "loc1", "10.0.0.1")
	assert.Empty(t, dt.pendingPods[uid], "unknown state must not buffer pod")
	select {
	case <-dt.locationsUpdaterTrigger:
		t.Fatal("unknown state must not fire LocationsUpdater")
	default:
	}
}

// Duplicate AddService should not reset state.
func TestGuardStateTransitions_DoubleCreate_DoesNotResetState(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-double-add"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateCreationInProgress,
		RetryCount: 3,
	}

	dt.AddService(NewInboundServiceConfig(uid, makeInboundConfig(8080)))

	op := dt.pendingServiceOps[uid]
	assert.Equal(t, 3, op.RetryCount, "RetryCount must not be reset by duplicate AddService")
	assert.Equal(t, StateCreationInProgress, op.State, "state must not change on duplicate AddService")
	assert.True(t, op.Config.InboundConfig.Equals(makeInboundConfig(80)),
		"original Config must not be overwritten by duplicate AddService (use UpdateService for that)")
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Fatal("duplicate AddService must NOT fire trigger")
	default:
	}
}

// UpdateService during deletion should buffer recreate intent.
func TestGuardStateTransitions_CreateAfterDelete_BuffersRecreate(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-recreate-during-delete"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     cfg,
		State:      StateDeletionInProgress,
	}

	newCfg := NewInboundServiceConfig(uid, makeInboundConfig(8080))
	dt.UpdateService(newCfg)

	op := dt.pendingServiceOps[uid]
	assert.Equal(t, StateDeletionInProgress, op.State, "in-flight deletion still wins (no mid-deletion race)")
	assert.True(t, op.RecreateAfterDeletion,
		"recreate intent must be buffered, not dropped")
	assert.True(t, op.Config.InboundConfig.Equals(makeInboundConfig(8080)),
		"buffered recreate must capture the latest desired config")
}

// AddPod during deletion pending should revive the service.
func TestGuardStateTransitions_AddPodDuringDeletionPending_RevivesService(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-revive"
	dt.NRPResources.NATGateways.Insert(uid)
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewOutboundServiceConfig(uid, nil),
		State:      StateDeletionPending,
	}
	dt.pendingServiceDeletions[uid] = &PendingServiceDeletion{ServiceUID: uid}

	dt.AddPod(uid, "ns/p", "10.0.0.1", "10.244.0.1")

	op := dt.pendingServiceOps[uid]
	assert.Equal(t, StateCreated, op.State, "AddPod during DeletionPending must revive the service")
	_, stillPending := dt.pendingServiceDeletions[uid]
	assert.False(t, stillPending, "pending deletion must be cancelled on revive")
}

// AddPod during deletion in progress should be buffered.
func TestGuardStateTransitions_AddPodDuringDeletionInProgress_Buffers(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-recreate"
	dt.NRPResources.NATGateways.Insert(uid)
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewOutboundServiceConfig(uid, nil),
		State:      StateDeletionInProgress,
	}

	dt.AddPod(uid, "ns/p", "10.0.0.1", "10.244.0.1")
	assert.Len(t, dt.pendingPods[uid], 1, "late AddPod during DeletionInProgress must be buffered, not dropped")
	op := dt.pendingServiceOps[uid]
	assert.Equal(t, StateDeletionInProgress, op.State, "service must stay in DeletionInProgress until delete finishes")
}

// TestUpdateService_RecreateAfterDeletionReplays verifies the LoadBalancer -> ClusterIP -> LoadBalancer
// toggle contract: an UpdateService arriving while a delete is in flight must not race the delete;
// instead it records RecreateAfterDeletion and buffers the new Config, and the deletion-success branch
// then replays it as a fresh create (StateNotStarted, retry/inflight/last-applied cleared,
// RecreateAfterDeletion cleared, trigger fired).
func TestUpdateService_RecreateAfterDeletionReplays(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-recreate"

	// Service is mid-delete. NRP still has the LB so UpdateService takes the
	// existing-tracking path rather than delegating to AddService.
	dt.NRPResources.LoadBalancers.Insert(uid)
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:        uid,
		Config:            NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:             StateDeletionInProgress,
		LastAppliedConfig: func() *ServiceConfig { c := NewInboundServiceConfig(uid, makeInboundConfig(80)); return &c }(),
		// InFlightConfig is nil — the delete worker doesn't set it on the engine state,
		// so a genuine delete-success completion will NOT fire the pre-empt branch.
	}
	dt.pendingServiceDeletions[uid] = &PendingServiceDeletion{ServiceUID: uid, IsInbound: true}
	// Drain any pre-existing trigger so we can assert the replay fires a fresh one.
	select {
	case <-dt.serviceUpdaterTrigger:
	default:
	}

	// UpdateService with a CHANGED spec while deletion is in progress.
	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(8080)))

	op := dt.pendingServiceOps[uid]
	assert.True(t, op.RecreateAfterDeletion,
		"UpdateService during StateDeletionInProgress MUST set RecreateAfterDeletion")
	assert.Equal(t, int32(8080), op.Config.InboundConfig.FrontendPorts[0].Port,
		"UpdateService during deletion MUST buffer the new desired Config for the replay")
	// State must stay in deletion — the re-create cannot race the in-flight delete.
	assert.Equal(t, StateDeletionInProgress, op.State,
		"UpdateService during deletion must NOT change state (deletion still wins until success)")

	// Delete success arrives. The replay branch must reset the op to a fresh create and trigger
	// the dispatcher.
	dt.OnServiceCreationComplete(uid, true, nil)
	op = dt.pendingServiceOps[uid]
	if !assert.NotNil(t, op, "replay must keep the op tracked (not delete it like a normal delete-success)") {
		return
	}
	assert.Equal(t, StateNotStarted, op.State,
		"delete-success with RecreateAfterDeletion MUST replay as StateNotStarted")
	assert.False(t, op.RecreateAfterDeletion, "the flag MUST be cleared after the replay")
	assert.Nil(t, op.InFlightConfig, "InFlightConfig MUST be cleared on replay")
	assert.Nil(t, op.LastAppliedConfig, "LastAppliedConfig MUST be cleared on replay (fresh create)")
	assert.Equal(t, 0, op.RetryCount, "RetryCount MUST be reset for the fresh create")
	_, stillQueued := dt.pendingServiceDeletions[uid]
	assert.False(t, stillQueued, "PendingServiceDeletion MUST be cleared on replay")

	select {
	case <-dt.serviceUpdaterTrigger:
	default:
		t.Fatal("replay path MUST nudge the ServiceUpdater so the fresh create dispatches")
	}
}

// TestUpdateService_RecreateAfterDeletionPreservesEndpoints verifies that a
// LoadBalancer->ClusterIP->LoadBalancer toggle caught mid-deletion keeps its backend endpoints.
// UpdateService replays the unchanged EndpointSlice itself and buffers it during the deletion window;
// promotePendingEndpointsLocked then restores it when the recreate completes.
func TestUpdateService_RecreateAfterDeletionPreservesEndpoints(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-recreate-endpoints"
	const node, addr = "10.0.0.2", "10.244.0.2"
	dt.ReconcileEndpointSlice(nil, &discovery_v1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "eps1",
			Namespace:       "test",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: types.UID(uid)}},
		},
		AddressType: discovery_v1.AddressTypeIPv4,
		Endpoints: []discovery_v1.Endpoint{{
			Addresses:  []string{addr},
			NodeName:   ptr.To("node1"),
			Conditions: discovery_v1.EndpointConditions{Ready: ptr.To(true)},
		}},
	})
	setTestNodeLister(t, dt, &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: node},
		}},
	})

	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	applied := cfg
	dt.NRPResources.LoadBalancers.Insert(uid)
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:        uid,
		Config:            cfg,
		LastAppliedConfig: &applied,
		State:             StateCreated,
	}
	// One live endpoint backing the service, present in both K8s and NRP.
	pod := newPod()
	pod.InboundIdentities.Insert(uid)
	n := newNode()
	n.Pods[addr] = pod
	dt.K8sResources.Nodes[node] = n
	dt.NRPResources.Locations[node] = NRPLocation{
		Addresses: map[string]NRPAddress{addr: {Services: utilsets.NewString(uid)}},
	}

	// Service -> ClusterIP: delete (locations present -> StateDeletionPending, K8s state scrubbed).
	dt.DeleteService(uid, true, false)
	// Service -> LoadBalancer again while deleting: UpdateService queues the recreate.
	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(80)))
	assert.True(t, dt.pendingServiceOps[uid].RecreateAfterDeletion, "the recreate must be queued")
	assert.Len(t, dt.pendingEndpoints[uid], 1, "the engine must replay and buffer unchanged endpoints")

	// Locations drain; CheckPendingServiceDeletions promotes the op and the delete dispatches.
	delete(dt.NRPResources.Locations, node)
	dt.CheckPendingServiceDeletions()
	// Delete completes -> deletion-success replays the create (StateNotStarted).
	dt.OnServiceCreationComplete(uid, true, nil)
	// Create dispatch + success -> promotePendingEndpointsLocked replays the buffered endpoints.
	dt.mu.Lock()
	dt.pendingServiceOps[uid].State = StateCreationInProgress
	dt.mu.Unlock()
	dt.OnServiceCreationComplete(uid, true, nil)

	dt.mu.Lock()
	defer dt.mu.Unlock()
	got, ok := dt.K8sResources.Nodes[node]
	if !assert.True(t, ok, "the recreated service must retain its backing node/endpoints") {
		return
	}
	p, ok := got.Pods[addr]
	if !assert.True(t, ok, "the recreated service must retain its backing pod address") {
		return
	}
	assert.True(t, p.InboundIdentities.Has(uid),
		"the recreated service's backend identity must be preserved, not dropped on recreate")
}

// TestUpdateEndpoints_TerminalUpdateKeepsApplyingToLiveLB verifies that after a terminal update
// failure parks the op in StateNotStarted while its LB stays live in NRP, a later endpoint change is
// applied to the live backend pool rather than buffered behind the parked op (which would let the
// pool go stale until an unrelated spec change un-parks the op).
func TestUpdateEndpoints_TerminalUpdateKeepsApplyingToLiveLB(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-terminal-update-live-lb"
	const node, addr = "10.0.0.5", "10.244.0.5"

	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	applied := cfg
	dt.NRPResources.LoadBalancers.Insert(uid) // the LB exists and stays live
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:        uid,
		Config:            cfg,
		LastAppliedConfig: &applied,
		InFlightConfig:    &applied,
		State:             StateUpdateInProgress,
	}

	// A terminal update failure parks the op in StateNotStarted while the LB remains in NRP.
	dt.OnServiceCreationComplete(uid, false, newTerminalError(fmt.Errorf("dual-stack not supported")))
	assert.Equal(t, StateNotStarted, dt.pendingServiceOps[uid].State, "a terminal update must park the op in StateNotStarted")

	// A later endpoint update must still be applied to the live LB, not buffered.
	dt.UpdateEndpoints(uid, nil, map[string]string{addr: node})

	assert.Empty(t, dt.pendingEndpoints[uid],
		"with a live LB in NRP, an endpoint update must be applied, not buffered behind the parked update")
	n, ok := dt.K8sResources.Nodes[node]
	if !assert.True(t, ok, "the endpoint update must be applied to K8s state while the LB is live") {
		return
	}
	p, ok := n.Pods[addr]
	assert.True(t, ok && p.InboundIdentities.Has(uid), "the applied endpoint must back the live LB")
}
