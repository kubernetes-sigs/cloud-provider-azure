/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Regression tests for engine edge cases.

package difftracker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// RemoveLastPodFinalizers should surface retry exhaustion and keep the entry pending.
func TestRemoveLastPodFinalizers_SurfacesRetryExhaustion(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "stuck",
			UID:        types.UID("uid-stuck"),
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
	}
	kube := fake.NewSimpleClientset(pod)
	// Force every Update (the finalizer strip) to fail with a permanent error.
	kube.PrependReactor("update", "pods", func(_ clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("permanent: kube-apiserver unreachable")
	})

	dt := newTestDiffTracker()
	dt.kubeClient = kube
	euid := "egress-leaked"
	dt.pendingPodDeletions["default/stuck"] = &PendingPodDeletion{
		Namespace:  "default",
		Name:       "stuck",
		ServiceUID: euid,
		Addresses:  []string{"10.244.0.1"},
		IsLastPod:  true,
	}

	err := dt.RemoveLastPodFinalizers(context.Background(), euid)
	assert.Error(t, err,
		"permanent finalizer-strip failure MUST be surfaced to the caller (otherwise the outbound delete reports false success and the pod finalizer leaks)")

	_, stillPending := dt.pendingPodDeletions["default/stuck"]
	assert.True(t, stillPending,
		"failed finalizer strip MUST keep the entry for retry (otherwise leak is unrecoverable)")
}

// Initialization should wait until pending pod deletions are drained.
func TestCheckInitializationComplete_WaitsForPendingPodDeletions(t *testing.T) {
	dt := newTestDiffTracker()
	dt.initCompletionChecker = make(chan struct{})
	dt.isInitializing = 1
	dt.pendingPodDeletions["default/foo"] = &PendingPodDeletion{
		Namespace:  "default",
		Name:       "foo",
		ServiceUID: "egress",
		Addresses:  []string{"10.244.0.1"},
		IsLastPod:  false,
	}

	dt.checkInitializationComplete()

	select {
	case <-dt.initCompletionChecker:
		t.Fatal("init signalled completion despite non-empty pendingPodDeletions")
	default:
	}
}

// Deleting during an in-flight update should still drive deletion to completion.
func TestDeleteService_DuringUpdateNoLocations_DispatchesDelete(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-update-then-delete"
	dt.NRPResources.LoadBalancers.Insert(uid)
	inflight := NewInboundServiceConfig(uid, makeInboundConfig(8080))
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:     uid,
		Config:         inflight,
		InFlightConfig: &inflight,
		State:          StateUpdateInProgress,
	}
	dt.DeleteService(uid, true, false)

	op := dt.pendingServiceOps[uid]
	assert.Equal(t, StateDeletionInProgress, op.State, "fast-path takes us straight to DeletionInProgress (current behaviour)")

	dt.OnServiceCreationComplete(uid, true, nil)

	_, stillTracked := dt.pendingServiceOps[uid]
	_, pending := dt.pendingServiceDeletions[uid]
	assert.True(t, stillTracked || pending,
		"after delete-during-update fast-path + update completion, "+
			"engine MUST still drive the Azure LB delete (either via pendingServiceOps or pendingServiceDeletions)")
}

// TestDeleteService_DuringUpdatePreemptKeepsPendingDeletion verifies that when a delete arrives
// during an in-flight update via the fast path (no NRP locations yet -> StateDeletionInProgress) and
// the in-flight LocationsUpdater then republishes the pod address, the completion callback's
// delete-preempt branch routes the op back to StateDeletionPending AND re-adds its
// pendingServiceDeletions entry. Without that entry CheckPendingServiceDeletions returns early on an
// empty map and never drives the deletion, leaking the LB/PIP and leaving the Service Terminating.
func TestDeleteService_DuringUpdatePreemptKeepsPendingDeletion(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-del-during-update"
	const node, addr = "10.0.0.1", "10.244.0.1"

	inflight := NewInboundServiceConfig(uid, makeInboundConfig(80))
	dt.NRPResources.LoadBalancers.Insert(uid)
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:     uid,
		Config:         inflight,
		InFlightConfig: &inflight,
		State:          StateUpdateInProgress,
	}

	// Delete fast path: no NRP locations yet -> StateDeletionInProgress, pendingServiceDeletions cleared.
	dt.DeleteService(uid, true, false)

	// The in-flight LocationsUpdater publishes the pod address just after the delete gated on
	// serviceHasLocationsInNRP.
	dt.NRPResources.Locations[node] = NRPLocation{
		Addresses: map[string]NRPAddress{addr: {Services: utilsets.NewString(uid)}},
	}

	// The in-flight update completes and is routed through the delete-preempt branch.
	dt.OnServiceCreationComplete(uid, true, nil)

	op := dt.pendingServiceOps[uid]
	if !assert.NotNil(t, op, "the op must remain tracked after the delete-preempt routing") {
		return
	}
	assert.Equal(t, StateDeletionPending, op.State, "locations reappeared, so the op must wait in StateDeletionPending")
	_, pending := dt.pendingServiceDeletions[uid]
	assert.True(t, pending,
		"an op routed to StateDeletionPending must have a pendingServiceDeletions entry so CheckPendingServiceDeletions can drive it")

	// Drain the locations: CheckPendingServiceDeletions must promote the op to dispatch.
	delete(dt.NRPResources.Locations, node)
	dt.CheckPendingServiceDeletions()
	assert.Equal(t, StateDeletionInProgress, dt.pendingServiceOps[uid].State,
		"once locations clear, CheckPendingServiceDeletions must promote the op to StateDeletionInProgress")
	_, stillPending := dt.pendingServiceDeletions[uid]
	assert.False(t, stillPending, "the pendingServiceDeletions entry must be consumed once the deletion dispatches")
}
