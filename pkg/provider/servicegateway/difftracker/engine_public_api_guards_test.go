/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
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

// TestGuardDeletePod_InvalidParamsIsNoOp verifies the validation rule.
func TestGuardDeletePod_InvalidParamsIsNoOp(t *testing.T) {
	dt := newTestDiffTracker()
	res := dt.DeletePod("", "loc", []string{"addr"}, "ns", "p", "")
	assert.False(t, res.IsLastPod)
	res = dt.DeletePod("uid", "", []string{"addr"}, "ns", "p", "")
	assert.False(t, res.IsLastPod)
	res = dt.DeletePod("uid", "loc", []string{""}, "ns", "p", "")
	assert.False(t, res.IsLastPod)
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
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := dt.WaitForInitialSync(ctx)
	assert.Error(t, err, "WaitForInitialSync must error if initialization never started")
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
func TestGuardIsServiceTracked_HandlesNilNRPSets(t *testing.T) {
	dt := &DiffTracker{
		pendingServiceOps: map[string]*ServiceOperationState{
			"only-in-pending": {ServiceUID: "only-in-pending"},
		},
	}
	assert.True(t, dt.IsServiceTracked("only-in-pending"))
	assert.False(t, dt.IsServiceTracked("missing"))
}
