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

// Public API behavior tests for the DiffTracker engine.

package difftracker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// ================================================================================================
// AddService — input validation & idempotency
// ================================================================================================

// TestGuardAddService_EmptyUIDIsNoOp verifies that a nil/empty-UID config is
// rejected by Validate() and does NOT corrupt pendingServiceOps or fire a
// trigger. A future caller passing an unsanitized UID must not poison state.
func TestGuardAddService_EmptyUIDIsNoOp(t *testing.T) {
	dt := newTestDiffTracker()
	dt.AddService(NewInboundServiceConfig("", nil))
	assert.Empty(t, dt.pendingServiceOps, "empty-UID config must not be tracked")
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Fatal("empty-UID AddService must not fire serviceUpdaterTrigger")
	default:
	}
}

// TestGuardAddService_IdempotentForExistingNRPLB verifies that AddService is a
// no-op when the LB already exists in NRP (recovery / restart safety): no
// pendingServiceOps entry, no trigger.
func TestGuardAddService_IdempotentForExistingNRPLB(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-existing"
	dt.NRPResources.LoadBalancers.Insert(uid)

	dt.AddService(NewInboundServiceConfig(uid, makeInboundConfig(80)))
	_, tracked := dt.pendingServiceOps[uid]
	assert.False(t, tracked, "AddService must be idempotent when LB exists in NRP")
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Fatal("AddService for existing NRP LB must not fire trigger")
	default:
	}
}

// TestGuardAddService_IdempotentForExistingNRPNAT same as above for the
// outbound (NAT Gateway) path.
func TestGuardAddService_IdempotentForExistingNRPNAT(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-existing"
	dt.NRPResources.NATGateways.Insert(uid)

	dt.AddService(NewOutboundServiceConfig(uid, nil))
	_, tracked := dt.pendingServiceOps[uid]
	assert.False(t, tracked, "AddService must be idempotent when NAT Gateway exists in NRP")
}

// ================================================================================================
// UpdateEndpoints — input validation & buffering contract
// ================================================================================================

// TestGuardUpdateEndpoints_EmptyUIDIsNoOp verifies the empty-UID guard so a
// callbacks-with-bad-data scenario does not poison pendingEndpoints.
func TestGuardUpdateEndpoints_EmptyUIDIsNoOp(t *testing.T) {
	dt := newTestDiffTracker()
	dt.UpdateEndpoints("", map[string]string{}, map[string]string{"10.0.0.1": "node1"})
	assert.Empty(t, dt.pendingEndpoints, "empty-UID UpdateEndpoints must not buffer")
}

// TestGuardUpdateEndpoints_BuffersWhenNotStarted verifies the buffering behaviour
// for a tracked service in StateNotStarted: the endpoint update must be
// preserved (old+new) so promotion replays the correct add-then-remove order.
func TestGuardUpdateEndpoints_BuffersWhenNotStarted(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-buffer"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateNotStarted,
	}

	old := map[string]string{}
	newM := map[string]string{"10.0.0.1": "node1"}
	dt.UpdateEndpoints(uid, old, newM)

	buf, ok := dt.pendingEndpoints[uid]
	assert.True(t, ok, "endpoint update must be buffered")
	if assert.Len(t, buf, 1) {
		// Both halves must hold: both old & new must be preserved (otherwise
		// add-then-remove during creation leaks stale IPs in NRP).
		assert.Equal(t, newM, buf[0].PodIPToNodeIP, "new addresses must be preserved exactly")
		assert.Equal(t, old, buf[0].OldPodIPToNodeIP, "old addresses must be preserved for replay ordering")
	}
}

// TestGuardUpdateEndpoints_DeletionInProgressIgnored verifies that endpoints
// arriving while a service is already in deletion-in-progress are dropped
// (otherwise a late update could resurrect K8sResources state for a doomed UID).
func TestGuardUpdateEndpoints_DeletionInProgressIgnored(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-late-endpoint"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateDeletionInProgress,
	}

	dt.UpdateEndpoints(uid, nil, map[string]string{"10.0.0.1": "node1"})
	// No buffered update and no LocationsUpdater trigger.
	assert.Empty(t, dt.pendingEndpoints[uid])
	select {
	case <-dt.locationsUpdaterTrigger:
		t.Fatal("StateDeletionInProgress UpdateEndpoints must not fire LocationsUpdater")
	default:
	}
}

// ================================================================================================
// UpdateService — fall-through & deletion-state ignore
// ================================================================================================

// TestGuardUpdateService_RejectsOutboundCall verifies that UpdateService is a
// no-op for outbound (NAT Gateway) configs — only inbound LB updates are
// supported by this method.
func TestGuardUpdateService_RejectsOutboundCall(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-update"
	dt.UpdateService(NewOutboundServiceConfig(uid, nil))
	_, tracked := dt.pendingServiceOps[uid]
	assert.False(t, tracked, "UpdateService must ignore outbound configs (no fall-through to AddService for NAT)")
}

// TestGuardUpdateService_LBInNRPCreatesTrackingEntry verifies the recovery
// behaviour: an LB that exists in NRP but not in pendingServiceOps must be
// re-adopted into StateUpdateInProgress so the update path can take over.
func TestGuardUpdateService_LBInNRPCreatesTrackingEntry(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-recovered"
	dt.NRPResources.LoadBalancers.Insert(uid)

	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(80)))

	op, tracked := dt.pendingServiceOps[uid]
	if assert.True(t, tracked, "UpdateService must adopt the recovered LB") {
		assert.Equal(t, StateUpdateInProgress, op.State)
	}
	select {
	case <-dt.serviceUpdaterTrigger:
	default:
		t.Fatal("recovered LB must trigger the service updater")
	}
}

// ================================================================================================
// DeleteService — input validation & double-delete idempotency
// ================================================================================================

// TestGuardDeleteService_EmptyUIDIsNoOp verifies the empty-UID guard.
func TestGuardDeleteService_EmptyUIDIsNoOp(t *testing.T) {
	dt := newTestDiffTracker()
	dt.DeleteService("", true, false)
	assert.Empty(t, dt.pendingServiceOps)
	assert.Empty(t, dt.pendingServiceDeletions)
}

// TestGuardDeleteService_UnknownInboundUntrackedNoOp verifies that DeleteService
// for an unknown (not tracked, not in NRP) non-orphan UID is a no-op: it must
// not synthesize a phantom tracking entry that would later wrongly drive a
// deletion against Azure.
func TestGuardDeleteService_UnknownInboundUntrackedNoOp(t *testing.T) {
	dt := newTestDiffTracker()
	dt.DeleteService("phantom-uid", true, false)
	_, tracked := dt.pendingServiceOps["phantom-uid"]
	assert.False(t, tracked, "non-orphan delete of unknown UID must not synthesize a tracking entry")
	_, pending := dt.pendingServiceDeletions["phantom-uid"]
	assert.False(t, pending, "non-orphan delete of unknown UID must not enqueue a deletion")
}

// TestGuardDeleteService_OrphanForcesTrackingEntry verifies that an orphan delete
// bypasses the "not in NRP" early-return and creates a deletion tracking entry
// so the orphan cleanup goroutine can do its work.
func TestGuardDeleteService_OrphanForcesTrackingEntry(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "orphan-uid"
	dt.DeleteService(uid, true, true /* isOrphan */)
	op, tracked := dt.pendingServiceOps[uid]
	if assert.True(t, tracked, "orphan delete must create tracking entry even when NRP is empty") {
		assert.True(t, op.IsOrphan)
		// No locations exist anywhere → must short-circuit to StateDeletionInProgress.
		assert.Equal(t, StateDeletionInProgress, op.State)
	}
}

// TestGuardDeleteService_DoubleDeleteIsIdempotent verifies that calling
// DeleteService twice in a row on a service already in DeletionPending /
// DeletionInProgress does NOT bump retry counters, reset state, or duplicate
// the pendingServiceDeletions entry.
func TestGuardDeleteService_DoubleDeleteIsIdempotent(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-double-delete"
	dt.NRPResources.LoadBalancers.Insert(uid)
	dt.NRPResources.Locations["loc1"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.0.0.1": {Services: utilsets.NewString(uid)},
		},
	}
	// Seed a live K8s pod so removeServiceFromK8sStateLocked doesn't empty the
	// location before serviceHasLocationsInNRP runs (which would short-circuit
	// to StateDeletionInProgress).
	node := newNode()
	pod := newPod()
	pod.InboundIdentities.Insert(uid)
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["loc1"] = node

	dt.DeleteService(uid, true, false)
	dt.DeleteService(uid, true, false) // duplicate, must be a no-op

	op := dt.pendingServiceOps[uid]
	if assert.NotNil(t, op) {
		// First call set DeletionPending; second call must early-return without state change.
		assert.Equal(t, StateDeletionPending, op.State)
	}
	// Exactly one pending deletion entry.
	assert.Len(t, dt.pendingServiceDeletions, 1)
}

// ================================================================================================
// OnServiceCreationComplete — guard against ghost callbacks
// ================================================================================================

// TestGuardOnServiceCreationComplete_UnknownServiceIsNoOp verifies that a callback
// for a UID with no pendingServiceOps entry is a safe no-op (does not panic,
// does not synthesize state, does not fire triggers).
func TestGuardOnServiceCreationComplete_UnknownServiceIsNoOp(t *testing.T) {
	dt := newTestDiffTracker()
	dt.OnServiceCreationComplete("ghost-uid", true, nil)
	assert.Empty(t, dt.pendingServiceOps)
	assert.Empty(t, dt.pendingServiceDeletions)
}

// ================================================================================================
// AddPod — input validation & idempotent recovery for NRP-existing service
// ================================================================================================

// TestGuardAddPod_InvalidParamsIsNoOp verifies the validation rule (empty
// serviceUID / location / address are rejected without state mutation).
func TestGuardAddPod_InvalidParamsIsNoOp(t *testing.T) {
	dt := newTestDiffTracker()
	dt.AddPod("", "ns/p", "loc", "addr")
	dt.AddPod("uid", "ns/p", "", "addr")
	dt.AddPod("uid", "ns/p", "loc", "")
	assert.Empty(t, dt.pendingServiceOps, "invalid-param AddPod must not synthesize a service")
	assert.Empty(t, dt.pendingPods, "invalid-param AddPod must not buffer anything")
}

// TestGuardAddPod_ExistingNRPNATGatewayAddsImmediately verifies the recovery path:
// when the NAT Gateway exists in NRP but no tracking entry exists, AddPod must
// add the pod to live state without buffering and fire LocationsUpdater.
func TestGuardAddPod_ExistingNRPNATGatewayAddsImmediately(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-recovered"
	dt.NRPResources.NATGateways.Insert(uid)

	dt.AddPod(uid, "ns/pod", "10.0.0.1", "10.244.0.5")

	dt.mu.Lock()
	defer dt.mu.Unlock()
	node, ok := dt.K8sResources.Nodes["10.0.0.1"]
	if !assert.True(t, ok, "node must be inserted into live state") {
		return
	}
	pod, ok := node.Pods["10.244.0.5"]
	if assert.True(t, ok, "pod must be inserted into live state") {
		assert.Equal(t, uid, pod.PublicOutboundIdentity)
	}
	assert.Empty(t, dt.pendingPods[uid], "must NOT buffer when NRP already has the NAT Gateway")
}

// ================================================================================================
// DeletePod — input validation & non-last contract
// ================================================================================================

// TestGuardDeletePod_InvalidParamsIsNoOp verifies the validation rule: a malformed DeletePod must
// change nothing and must not tell the caller it may strip the pod's cleanup finalizer.
//
// Asserting only IsLastPod==false is not enough — that is the zero value of DeletePodResult, so it
// also holds if deletePod became a total no-op. The safety-critical field is FinalizerDecision:
// its zero value is deliberately not releasable, and a validation path that started returning
// ReleaseNoDrain would let the informer strip the finalizer off a pod whose addresses are still
// routed in NRP.
func TestGuardDeletePod_InvalidParamsIsNoOp(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		uid, location, ns, podName, podUID string
		addresses                          []string
	}{
		{name: "empty service UID", uid: "", location: "loc", addresses: []string{"addr"}, ns: "ns", podName: "p"},
		{name: "empty location", uid: "uid", location: "", addresses: []string{"addr"}, ns: "ns", podName: "p"},
		{name: "no addresses", uid: "uid", location: "loc", addresses: nil, ns: "ns", podName: "p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dt := newTestDiffTracker()
			res := dt.DeletePod(tc.uid, tc.location, tc.addresses, tc.ns, tc.podName, tc.podUID)

			assert.False(t, res.IsLastPod)
			assert.NotEqual(t, PodFinalizerDecisionReleaseNoDrain, res.FinalizerDecision,
				"a rejected DeletePod must never authorise releasing the pod's cleanup finalizer")

			dt.mu.Lock()
			defer dt.mu.Unlock()
			assert.Empty(t, dt.pendingPodDeletions, "a rejected DeletePod must not enqueue a drain gate")
			assert.Empty(t, dt.K8sResources.Nodes, "a rejected DeletePod must not mutate node state")
		})
	}
}

// TestGuardDeletePod_NonLastEnqueuesPendingPodDeletion verifies the contract that a
// non-last DeletePod enqueues a drain-gated PendingPodDeletion (IsLastPod=false) instead of
// stripping the finalizer inline, so the finalizer is removed by CheckPendingPodDeletions only
// after the pod's address has left NRP. CheckPendingPodDeletions' Phase 3 cleanup keeps the map
// bounded.
func TestGuardDeletePod_NonLastEnqueuesPendingPodDeletion(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-nonlast"
	dt.NRPResources.NATGateways.Insert(uid)
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewOutboundServiceConfig(uid, nil),
		State:      StateCreated,
	}
	dt.AddPod(uid, "ns/a", "10.0.0.1", "10.244.0.1")
	dt.AddPod(uid, "ns/b", "10.0.0.1", "10.244.0.2")

	res := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.1"}, "ns", "a", "")
	assert.False(t, res.IsLastPod)
	assert.True(t, res.Enqueued, "a tracked non-last delete must report Enqueued so the caller defers to drain-gated removal")
	ppd, ok := dt.pendingPodDeletions["ns/a"]
	if assert.True(t, ok, "non-last DeletePod must enqueue a drain-gated PendingPodDeletion") {
		assert.False(t, ppd.IsLastPod, "non-last entry must have IsLastPod=false")
		assert.Equal(t, []string{"10.244.0.1"}, ppd.Addresses)
	}
}

// TestGuardDeletePod_LastPodWithNamespaceNameTracksLastPodEntry verifies the
// "last pod with ns/name" path: a PendingPodDeletion record with IsLastPod=true
// MUST be created so RemoveLastPodFinalizers can strip the finalizer after the
// NAT Gateway is gone.
func TestGuardDeletePod_LastPodWithNamespaceNameTracksLastPodEntry(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-last"
	dt.NRPResources.NATGateways.Insert(uid)
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewOutboundServiceConfig(uid, nil),
		State:      StateCreated,
	}
	dt.AddPod(uid, "ns/only", "10.0.0.1", "10.244.0.7")

	res := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.7"}, "ns", "only", "")
	assert.True(t, res.IsLastPod, "removing the sole pod must be the last-pod case")
	entry, ok := dt.pendingPodDeletions["ns/only"]
	if assert.True(t, ok, "last pod with ns/name must enqueue PendingPodDeletion") {
		assert.True(t, entry.IsLastPod)
		assert.Equal(t, uid, entry.ServiceUID)
		assert.Equal(t, []string{"10.244.0.7"}, entry.Addresses)
	}
}

func TestGuardDeletePod_LocalStateMissingNRPOnlySurvivorSchedulesServiceCleanup(t *testing.T) {
	dt := newTestDiffTracker()
	const uid, location, deleted, survivor = "egress-recovered", "10.0.0.1", "10.244.0.7", "10.244.0.8"
	dt.NRPResources.NATGateways.Insert(uid)
	dt.NRPResources.Locations[location] = NRPLocation{
		Addresses: map[string]NRPAddress{
			deleted:  {Services: utilsets.NewString(uid)},
			survivor: {Services: utilsets.NewString(uid)},
		},
	}
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewOutboundServiceConfig(uid, nil),
		State:      StateCreated,
	}

	res := dt.DeletePod(uid, location, []string{deleted}, "ns", "deleted", "deleted-uid")

	assert.True(t, res.IsLastPod,
		"NRP-only addresses are not desired pods and will drain in the same locations sync")
	assert.True(t, res.Enqueued)
	assert.Equal(t, PodFinalizerDecisionHoldForServiceDeletion, res.FinalizerDecision)
	assert.Equal(t, StateDeletionPending, dt.pendingServiceOps[uid].State,
		"once all desired pods are gone, cleanup must also remove the NAT Gateway after draining NRP-only addresses")
	assert.Contains(t, dt.pendingServiceDeletions, uid)
	entry := dt.pendingPodDeletions["ns/deleted"]
	if assert.NotNil(t, entry) {
		assert.Equal(t, []string{deleted}, entry.Addresses)
		assert.True(t, entry.IsLastPod)
	}
}

func TestGuardDeletePod_LocalStateMissingNRPWithLiveOrBufferedPodIsNonLast(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*DiffTracker)
	}{
		{
			name: "another live pod",
			setup: func(dt *DiffTracker) {
				dt.K8sResources.Nodes["10.0.0.2"] = Node{
					Pods: map[string]Pod{
						"10.244.0.8": {
							InboundIdentities:      utilsets.NewString(),
							PublicOutboundIdentity: "egress-recovered",
						},
					},
				}
			},
		},
		{
			name: "another buffered pod",
			setup: func(dt *DiffTracker) {
				dt.pendingPods["egress-recovered"] = []PendingPodUpdate{{
					PodKey:   "ns/survivor",
					Location: "10.0.0.2",
					Address:  "10.244.0.8",
				}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dt := newTestDiffTracker()
			const uid, location, deleted = "egress-recovered", "10.0.0.1", "10.244.0.7"
			dt.NRPResources.NATGateways.Insert(uid)
			dt.NRPResources.Locations[location] = NRPLocation{
				Addresses: map[string]NRPAddress{
					deleted: {Services: utilsets.NewString(uid)},
				},
			}
			dt.pendingServiceOps[uid] = &ServiceOperationState{
				ServiceUID: uid,
				Config:     NewOutboundServiceConfig(uid, nil),
				State:      StateCreated,
			}
			tc.setup(dt)

			res := dt.DeletePod(uid, location, []string{deleted}, "ns", "deleted", "deleted-uid")

			assert.False(t, res.IsLastPod)
			assert.True(t, res.Enqueued)
			assert.Equal(t, PodFinalizerDecisionHoldForDrain, res.FinalizerDecision)
			assert.Equal(t, StateCreated, dt.pendingServiceOps[uid].State)
			assert.NotContains(t, dt.pendingServiceDeletions, uid)
		})
	}
}

func TestGuardDeletePod_LocalStateMissingNRPDualStackLastReconstructsServiceCleanup(t *testing.T) {
	dt := newTestDiffTracker()
	const uid, v4Location, v6Location, v4, v6 = "egress-recovered", "10.0.0.1", "fd00::a", "10.244.0.7", "fd00::7"
	dt.NRPResources.NATGateways.Insert(uid)
	dt.NRPResources.Locations[v4Location] = NRPLocation{
		Addresses: map[string]NRPAddress{v4: {Services: utilsets.NewString(uid)}},
	}
	dt.NRPResources.Locations[v6Location] = NRPLocation{
		Addresses: map[string]NRPAddress{v6: {Services: utilsets.NewString(uid)}},
	}
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewOutboundServiceConfig(uid, nil),
		State:      StateCreated,
	}

	res := dt.DeletePod(uid, v4Location, []string{v4, v6}, "ns", "dual", "dual-uid")

	assert.True(t, res.IsLastPod)
	assert.True(t, res.Enqueued)
	assert.Equal(t, PodFinalizerDecisionHoldForServiceDeletion, res.FinalizerDecision)
	assert.Equal(t, StateDeletionPending, dt.pendingServiceOps[uid].State)
	assert.Contains(t, dt.pendingServiceDeletions, uid)
	entry := dt.pendingPodDeletions["ns/dual"]
	if assert.NotNil(t, entry) {
		assert.ElementsMatch(t, []string{v4, v6}, entry.Addresses)
		assert.True(t, entry.IsLastPod)
	}
}

func TestGuardDeletePod_RecoveredDeletePreservesInFlightDeletionForLiveReregistration(t *testing.T) {
	dt := newTestDiffTracker()
	const uid, location, oldAddress, newAddress = "egress-recovered", "10.0.0.1", "10.244.0.7", "10.244.0.8"
	dt.NRPResources.NATGateways.Insert(uid)
	dt.NRPResources.Locations[location] = NRPLocation{
		Addresses: map[string]NRPAddress{
			oldAddress: {Services: utilsets.NewString(uid)},
		},
	}
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewOutboundServiceConfig(uid, nil),
		State:      StateDeletionInProgress,
	}

	// Live re-registration drains the old address without a pod finalizer record, then immediately
	// adds the replacement address. The recovered drain must not make AddPod revive a delete worker
	// that is already deleting Azure resources.
	res := dt.DeletePod(uid, location, []string{oldAddress}, "", "", "")
	assert.True(t, res.IsLastPod)
	assert.Equal(t, PodFinalizerDecisionHoldForServiceDeletion, res.FinalizerDecision)
	assert.Equal(t, StateDeletionInProgress, dt.pendingServiceOps[uid].State)
	assert.NotContains(t, dt.pendingServiceDeletions, uid)

	dt.AddPod(uid, "ns/live", location, newAddress)

	assert.Equal(t, StateDeletionInProgress, dt.pendingServiceOps[uid].State,
		"an in-flight Azure deletion must not be revived as Created")
	if assert.Len(t, dt.pendingPods[uid], 1, "the replacement pod must wait for delete completion and recreation") {
		assert.Equal(t, newAddress, dt.pendingPods[uid][0].Address)
	}
}

func TestGuardDeletePod_StaleSameIPDeleteDoesNotRemoveReplacementUID(t *testing.T) {
	dt := newTestDiffTracker()
	const serviceUID, location, address, podKey = "egress-replacement", "10.0.0.1", "10.244.0.7", "ns/pod"
	dt.NRPResources.NATGateways.Insert(serviceUID)
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		Config:     NewOutboundServiceConfig(serviceUID, nil),
		State:      StateCreated,
	}
	dt.AddPodWithUID(serviceUID, podKey, "uid-new", location, address)

	res := dt.DeletePod(serviceUID, location, []string{address}, "ns", "pod", "uid-old")

	assert.Equal(t, PodFinalizerDecisionReleaseNoDrain, res.FinalizerDecision)
	assert.False(t, res.Enqueued)
	assert.Empty(t, dt.pendingPodDeletions)
	assert.Equal(t, StateCreated, dt.pendingServiceOps[serviceUID].State)
	assert.NotContains(t, dt.pendingServiceDeletions, serviceUID)
	live := dt.K8sResources.Nodes[location].Pods[address]
	assert.Equal(t, serviceUID, live.PublicOutboundIdentity)
	assert.Equal(t, podKey, live.OutboundPodKey)
	assert.Equal(t, "uid-new", live.OutboundPodUID)
	if count, ok := dt.outboundIdentityPodRefCount.Load(serviceUID); assert.True(t, ok) {
		assert.Equal(t, 1, count)
	}
}

func TestGuardDeletePodForReplacement_DoesNotDeleteSameServiceNATGateway(t *testing.T) {
	dt := newTestDiffTracker()
	const serviceUID, location, oldAddress, newAddress, podKey = "egress-replacement", "10.0.0.1", "10.244.0.7", "10.244.0.8", "ns/pod"
	dt.NRPResources.NATGateways.Insert(serviceUID)
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		Config:     NewOutboundServiceConfig(serviceUID, nil),
		State:      StateCreated,
	}
	dt.AddPodWithUID(serviceUID, podKey, "uid-old", location, oldAddress)

	res := dt.DeletePodForReplacement(serviceUID, location, []string{oldAddress}, "", "", "")

	assert.False(t, res.IsLastPod)
	assert.Equal(t, StateCreated, dt.pendingServiceOps[serviceUID].State)
	assert.NotContains(t, dt.pendingServiceDeletions, serviceUID)

	dt.AddPodWithUID(serviceUID, podKey, "uid-new", location, newAddress)

	assert.Equal(t, StateCreated, dt.pendingServiceOps[serviceUID].State)
	assert.NotContains(t, dt.pendingServiceDeletions, serviceUID)
	assert.Equal(t, "uid-new", dt.K8sResources.Nodes[location].Pods[newAddress].OutboundPodUID)
	assert.NotContains(t, dt.K8sResources.Nodes[location].Pods, oldAddress)
	if count, ok := dt.outboundIdentityPodRefCount.Load(serviceUID); assert.True(t, ok) {
		assert.Equal(t, 1, count)
	}
}

func TestGuardDeletePodWithoutAddresses_StaleUIDDoesNotRemoveReplacement(t *testing.T) {
	dt := newTestDiffTracker()
	const serviceUID, location, oldAddress, newAddress, podKey = "egress-replacement", "10.0.0.1", "10.244.0.7", "10.244.0.8", "ns/pod"
	dt.NRPResources.NATGateways.Insert(serviceUID)
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		Config:     NewOutboundServiceConfig(serviceUID, nil),
		State:      StateCreated,
	}
	dt.AddPodWithUID(serviceUID, podKey, "uid-old", location, oldAddress)
	dt.AddPodWithUID(serviceUID, podKey, "uid-new", location, newAddress)

	res := dt.DeletePodWithoutAddresses(serviceUID, "ns", "pod", "uid-old")

	assert.Equal(t, PodFinalizerDecisionReleaseNoDrain, res.FinalizerDecision)
	assert.False(t, res.Enqueued)
	assert.NotContains(t, dt.K8sResources.Nodes[location].Pods, oldAddress)
	live := dt.K8sResources.Nodes[location].Pods[newAddress]
	assert.Equal(t, serviceUID, live.PublicOutboundIdentity)
	assert.Equal(t, "uid-new", live.OutboundPodUID)
	assert.NotContains(t, dt.pendingPodDeletions, podKey)
	assert.NotContains(t, dt.pendingServiceDeletions, serviceUID)
	if count, ok := dt.outboundIdentityPodRefCount.Load(serviceUID); assert.True(t, ok) {
		assert.Equal(t, 1, count)
	}
}

func TestGuardDeletePod_UntrackedAndAbsentFromNRPExplicitlyReleases(t *testing.T) {
	dt := newTestDiffTracker()

	res := dt.DeletePod("egress-absent", "10.0.0.1", []string{"10.244.0.7"}, "ns", "pod", "pod-uid")

	assert.False(t, res.IsLastPod)
	assert.False(t, res.Enqueued)
	assert.Equal(t, PodFinalizerDecisionReleaseNoDrain, res.FinalizerDecision)
	assert.Empty(t, dt.pendingPodDeletions)
}

func TestGuardDeletePod_LocalAddressWithMissingOrInvalidRefCountKeepsDrainGate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setCounter bool
		counter    int
	}{
		{name: "missing ref-count"},
		{name: "zero ref-count", setCounter: true, counter: 0},
		{name: "negative ref-count", setCounter: true, counter: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dt := newTestDiffTracker()
			const uid, location, address = "egress-inconsistent", "10.0.0.1", "10.244.0.7"
			dt.NRPResources.NATGateways.Insert(uid)
			dt.NRPResources.Locations[location] = NRPLocation{
				Addresses: map[string]NRPAddress{
					address: {Services: utilsets.NewString(uid)},
				},
			}
			dt.K8sResources.Nodes[location] = Node{
				Pods: map[string]Pod{
					address: {
						InboundIdentities:      utilsets.NewString(),
						PublicOutboundIdentity: uid,
					},
				},
			}
			dt.pendingServiceOps[uid] = &ServiceOperationState{
				ServiceUID: uid,
				Config:     NewOutboundServiceConfig(uid, nil),
				State:      StateCreated,
			}
			if tc.setCounter {
				dt.outboundIdentityPodRefCount.Store(uid, tc.counter)
			}

			res := dt.DeletePod(uid, location, []string{address}, "ns", "pod", "pod-uid")

			assert.True(t, res.IsLastPod)
			assert.True(t, res.Enqueued,
				"a ref-count inconsistency must retain NRP drain protection instead of authorizing inline release")
			assert.Equal(t, PodFinalizerDecisionHoldForServiceDeletion, res.FinalizerDecision)
			assert.Equal(t, StateDeletionPending, dt.pendingServiceOps[uid].State)
			assert.Contains(t, dt.pendingPodDeletions, "ns/pod")
		})
	}
}

// TestGuardDeletePod_MixedDualStackInputAggregatesOnlyLiveAddresses verifies the atomic multi-address
// delete when the caller passes a mix of live, duplicate, and stale addresses (as can happen from an
// informer resync or an at-least-once event). Only the two genuinely live addresses (one per family)
// are drained and recorded; a duplicate is deduplicated and a never-registered address is a no-op.
// The single record must carry exactly the deduplicated live set and IsLastPod must reflect that the
// whole dual-stack pod was the service's last pod.
func TestGuardDeletePod_MixedDualStackInputAggregatesOnlyLiveAddresses(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-mixed"
	const location, v4, v6, stale = "10.0.0.1", "10.244.0.1", "fd00::1", "10.244.9.9"
	dt.NRPResources.NATGateways.Insert(uid)
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewOutboundServiceConfig(uid, nil),
		State:      StateCreated,
	}
	// One dual-stack pod: one address per family under the same location.
	dt.AddPod(uid, "ns/ds", location, v4)
	dt.AddPod(uid, "ns/ds", location, v6)

	// Delete input mixes both live addresses, a duplicate of one, and a never-registered address.
	res := dt.DeletePod(uid, location, []string{v4, v6, v6, stale}, "ns", "ds", "")

	assert.True(t, res.IsLastPod, "draining both live addresses of the only pod is the last-pod case")
	assert.True(t, res.Enqueued, "a live drain must enqueue exactly one drain-gated record")
	entry, ok := dt.pendingPodDeletions["ns/ds"]
	if assert.True(t, ok, "the dual-stack pod must enqueue a single record covering its live addresses") {
		assert.ElementsMatch(t, []string{v4, v6}, entry.Addresses,
			"the record must carry the deduplicated live address set (no duplicate, no stale address)")
		assert.True(t, entry.IsLastPod)
	}
}

// ================================================================================================
// CheckPendingServiceDeletions — empty / blocked / cleared
// ================================================================================================

// TestGuardCheckPendingServiceDeletions_EmptyIsNoOp verifies the early-return on
// empty pending map: no panics, no spurious triggers.
func TestGuardCheckPendingServiceDeletions_EmptyIsNoOp(t *testing.T) {
	dt := newTestDiffTracker()
	dt.CheckPendingServiceDeletions()
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Fatal("empty CheckPendingServiceDeletions must not trigger serviceUpdater")
	default:
	}
}

// TestGuardCheckPendingServiceDeletions_BlockedWhenLocationsRemain verifies the
// blocking contract: a service with live NRP locations must stay in
// pendingServiceDeletions and NOT be advanced to StateDeletionInProgress.
func TestGuardCheckPendingServiceDeletions_BlockedWhenLocationsRemain(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-blocked"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateDeletionPending,
	}
	dt.pendingServiceDeletions[uid] = &PendingServiceDeletion{ServiceUID: uid, IsInbound: true}
	dt.NRPResources.Locations["loc1"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.0.0.1": {Services: utilsets.NewString(uid)},
		},
	}

	dt.CheckPendingServiceDeletions()

	op := dt.pendingServiceOps[uid]
	assert.Equal(t, StateDeletionPending, op.State, "blocked deletion must NOT advance to DeletionInProgress")
	_, stillPending := dt.pendingServiceDeletions[uid]
	assert.True(t, stillPending, "blocked deletion must remain pending")
}

// TestGuardCheckPendingServiceDeletions_AdvancesWhenLocationsCleared verifies the
// completion contract: once locations are gone, the service moves to
// StateDeletionInProgress and the entry is removed from pendingServiceDeletions.
func TestGuardCheckPendingServiceDeletions_AdvancesWhenLocationsCleared(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-cleared"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateDeletionPending,
	}
	dt.pendingServiceDeletions[uid] = &PendingServiceDeletion{ServiceUID: uid, IsInbound: true}
	// No NRP locations → cleared.

	dt.CheckPendingServiceDeletions()

	op := dt.pendingServiceOps[uid]
	assert.Equal(t, StateDeletionInProgress, op.State)
	_, stillPending := dt.pendingServiceDeletions[uid]
	assert.False(t, stillPending, "cleared deletion must be removed from pendingServiceDeletions")
	select {
	case <-dt.serviceUpdaterTrigger:
	default:
		t.Fatal("cleared deletion must trigger the service updater")
	}
}

// ================================================================================================
// WaitForInitialSync — protocol guards
// ================================================================================================

// TestGuardWaitForInitialSync_NotInitializedReturnsError verifies the explicit
// error: calling WaitForInitialSync before initCompletionChecker is created
// (i.e. before initialization started) must return an error rather than block.
func TestGuardWaitForInitialSync_NotInitializedReturnsError(t *testing.T) {
	dt := newTestDiffTracker()
	// Deliberately no timeout: a context deadline would make "returned the explicit
	// not-initialized error" and "blocked until the caller gave up" indistinguishable, and the
	// real caller (InitializeFromCluster) waits on a context with no deadline — so losing this
	// guard hangs CCM initialization forever rather than surfacing an error.
	err := dt.WaitForInitialSync(context.Background())
	assert.ErrorContains(t, err, "before initialization started",
		"WaitForInitialSync must fail fast with an explicit error if initialization never started")
}

// TestGuardWaitForInitialSync_ReturnsOnContextCancel verifies that the wait
// honors context cancellation (no goroutine leak / hang on shutdown).
func TestGuardWaitForInitialSync_ReturnsOnContextCancel(t *testing.T) {
	dt := newTestDiffTracker()
	dt.initCompletionChecker = make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := dt.WaitForInitialSync(ctx)
	assert.Error(t, err, "context cancel must surface as error")
	assert.Less(t, time.Since(start), time.Second, "must not block past context deadline")
}

// TestGuardWaitForInitialSync_ReturnsOnCompletion verifies that closing the
// completion channel unblocks the wait.
func TestGuardWaitForInitialSync_ReturnsOnCompletion(t *testing.T) {
	dt := newTestDiffTracker()
	dt.initCompletionChecker = make(chan struct{})

	done := make(chan error, 1)
	go func() {
		done <- dt.WaitForInitialSync(context.Background())
	}()
	close(dt.initCompletionChecker)
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("WaitForInitialSync did not return after channel close")
	}
}

// ================================================================================================
// IsServiceTracked — handles nil sets safely
// ================================================================================================

// TestGuardIsServiceTracked_HandlesNilNRPSets verifies that a partially-initialized
// DiffTracker (nil LoadBalancers/NATGateways sets) does not panic when probed
// (only pendingServiceOps need be present).
// TestGuardIsServiceTracked_HandlesNilNRPSets pins that a DiffTracker whose NRP sets were never
// initialised is still queryable. The nil-safety itself lives in IgnoreCaseSet (Has returns false
// for a nil receiver), so this documents the contract IsServiceTracked relies on rather than a
// branch inside IsServiceTracked; a set type that started panicking on nil would fail here.
func TestGuardIsServiceTracked_HandlesNilNRPSets(t *testing.T) {
	dt := &DiffTracker{
		pendingServiceOps: map[string]*ServiceOperationState{
			"only-in-pending": {ServiceUID: "only-in-pending"},
		},
	}
	assert.Nil(t, dt.NRPResources.LoadBalancers, "this guard is only meaningful with uninitialised NRP sets")
	assert.Nil(t, dt.NRPResources.NATGateways, "this guard is only meaningful with uninitialised NRP sets")

	assert.True(t, dt.IsServiceTracked("only-in-pending"))
	assert.False(t, dt.IsServiceTracked("missing"))
}
