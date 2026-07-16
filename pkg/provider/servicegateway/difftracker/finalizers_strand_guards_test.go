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

// Regression guards for egress pod-finalizer strand conditions. Each pins the required behaviour;
// the buffered-pod case (TestGuardDeletePod_PodDeletedWhileBuffered_FinalizerNotStranded) remains
// skipped pending its fix.

package difftracker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// TestGuardCheckPendingPodDeletions_TransientGetErrorKeepsEntryForRetry verifies that in
// CheckPendingPodDeletions Phase 2, getPodByNamespaceName returning ANY error (including a transient
// 503/timeout/etcd error that is NOT a typed NotFound) is misread as "pod gone, clean up tracking",
// so the entry is deleted from pendingPodDeletions and the finalizer is never removed, permanently
// stranding the terminating egress pod until a CCM restart. The sibling RemoveLastPodFinalizers does
// this correctly (it special-cases apierrors.IsNotFound and keeps the entry on transient errors).
func TestGuardCheckPendingPodDeletions_TransientGetErrorKeepsEntryForRetry(t *testing.T) {
	ctx := context.Background()
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "p",
			Namespace:  "ns",
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
	}
	kube := fake.NewSimpleClientset(pod)
	// The pod GET always returns a transient (non-NotFound) server error.
	kube.PrependReactor("get", "pods", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(fmt.Errorf("transient server error"))
	})

	dt := &DiffTracker{
		kubeClient:          kube,
		pendingPodDeletions: make(map[string]*PendingPodDeletion),
		NRPResources:        NRPState{Locations: make(map[string]NRPLocation)}, // address already drained from NRP
	}
	dt.pendingPodDeletions["ns/p"] = &PendingPodDeletion{
		Namespace:  "ns",
		Name:       "p",
		ServiceUID: "egress-1",
		Addresses:  []string{"10.244.0.1"},
		IsLastPod:  false,
		Timestamp:  time.Now().Format(time.RFC3339),
	}

	dt.CheckPendingPodDeletions(ctx)

	// A transient GET error must NOT drop the entry; it must remain so a later cycle retries.
	assert.Len(t, dt.pendingPodDeletions, 1,
		"a transient (non-NotFound) GET error must not drop the pending pod deletion, else the pod is stranded Terminating")
}

// TestGuardDeletePod_PodDeletedWhileBuffered_EnqueuesNoRecord verifies the contract for a pod deleted
// while still buffered for an in-flight service creation: the pod never reached live state or NRP, so
// DeletePod cancels the buffered add and enqueues NO drain-gated record (Enqueued=false,
// IsLastPod=false). There is nothing to drain, so the caller (podInformerRemovePod) removes the
// finalizer directly - see TestPodInformerRemovePod_UntrackedPodFinalizerRemovedDirectly. Enqueuing a
// record here would strand the pod Terminating forever, because no NRP address will ever release it.
func TestGuardDeletePod_PodDeletedWhileBuffered_EnqueuesNoRecord(t *testing.T) {
	dt := newTestDiffTracker()
	const svc, ns, name, location, address = "egress-buffered", "default", "pod-buf", "10.0.0.1", "10.244.0.7"

	// Service creation has not reached Azure yet; the pod is buffered (never reached live state/NRP).
	dt.pendingServiceOps[svc] = &ServiceOperationState{
		ServiceUID: svc,
		Config:     NewOutboundServiceConfig(svc, nil),
		State:      StateNotStarted,
	}
	dt.pendingPods[svc] = []PendingPodUpdate{{
		PodKey:    ns + "/" + name,
		Location:  location,
		Address:   address,
		Timestamp: time.Now().Format(time.RFC3339),
	}}

	res := dt.DeletePod(svc, location, []string{address}, ns, name, "")

	assert.False(t, res.Enqueued,
		"a buffered pod has nothing in NRP to drain, so DeletePod must not enqueue a drain-gated record; the provider removes the finalizer directly instead")
	assert.False(t, res.IsLastPod)
	_, tracked := dt.pendingPodDeletions[ns+"/"+name]
	assert.False(t, tracked,
		"a buffered pod must not be drain-gated, else its finalizer is stranded (no NRP address will ever release it)")
}

// TestGuardLocationsUpdaterReschedulesOnReadyFinalizerRemovalFailure verifies that when a ready
// (address already drained from NRP) non-last pod finalizer removal fails transiently in steady
// state (post-init), LocationsUpdater.process() must NOT report success - it must reschedule a
// retry via backoffAndRetry. Otherwise, on a quiet cluster with no further triggers, the pod is
// stranded Terminating until some unrelated future event. We observe the reschedule via
// failureCount (incremented at the start of backoffAndRetry).
func TestGuardLocationsUpdaterReschedulesOnReadyFinalizerRemovalFailure(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "p",
			Namespace:  "ns",
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
	}
	kube := fake.NewSimpleClientset(pod)
	// The finalizer-removing Update fails with a transient (non-conflict) server error.
	kube.PrependReactor("update", "pods", func(_ ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(fmt.Errorf("transient server error"))
	})

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.kubeClient = kube
	// A ready, non-last pending pod deletion: its address is NOT in NRP (empty Locations), so
	// CheckPendingPodDeletions attempts the removal this cycle and fails transiently.
	dt.pendingPodDeletions["ns/p"] = &PendingPodDeletion{
		Namespace:  "ns",
		Name:       "p",
		ServiceUID: "egress-1",
		Addresses:  []string{"10.244.0.1"},
		IsLastPod:  false,
		Timestamp:  time.Now().Format(time.RFC3339),
	}
	// No K8s nodes / NRP locations -> GetSyncLocationsAddresses returns no diff -> no-diff branch.

	// Cancel the updater context so backoffAndRetry returns immediately after incrementing
	// failureCount (skipping the delay and the re-trigger); failureCount is the observable signal.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lu := &LocationsUpdater{
		diffTracker: dt,
		ctx:         ctx,
		cancel:      cancel,
		logger:      dt.logger.WithName("LocationsUpdater"),
	}

	// Post-init: isInitializing defaults to 0, so initPodFinalizersStillPending() is false and the
	// retry must come from the readyRemovalPending signal.
	lu.process(context.Background())

	assert.Equal(t, 1, lu.failureCount,
		"a ready non-last finalizer removal that fails transiently post-init must reschedule a retry (backoffAndRetry), not report success")
}

// TestGuardOrphanCleanup_PIPOnlyServiceScheduledForDeletion verifies that scheduleOrphanedResourceDeletions
// schedules a PIP-only orphan (a "<uid>-pip" with no LB/NAT in NRP or Azure and not desired in K8s)
// through the inbound orphan-delete path. deleteInboundService then deletes the PIP and removes the
// stuck Service finalizer, so the Service no longer strands in Terminating after a restart.
func TestGuardOrphanCleanup_PIPOnlyServiceScheduledForDeletion(t *testing.T) {
	uid := "11111111-1111-1111-1111-111111111111" // must be a valid service UUID
	dt := newTestDiffTracker()

	scheduleOrphanedResourceDeletions(dt, utilsets.NewString(), utilsets.NewString(), utilsets.NewString(uid+"-pip"))

	op, ok := dt.pendingServiceOps[uid]
	if !assert.True(t, ok, "a PIP-only orphan must be scheduled for deletion, not ignored") {
		return
	}
	assert.Equal(t, StateDeletionInProgress, op.State,
		"a PIP-only orphan with no locations must go straight to StateDeletionInProgress so deleteInboundService runs")
	assert.True(t, op.IsOrphan, "the scheduled deletion must be marked as an orphan cleanup")
}

// TestGuardOrphanCleanup_PIPNotScheduledWhenDesiredOrHasLB guards the exclusions so the PIP-only path
// never deletes a live service's PIP: a service desired in K8s, or one with a registered NRP
// LoadBalancer, must not be scheduled by the PIP path, and an orphaned LB is scheduled exactly once
// via the LB path (its PIP is deleted there, not double-scheduled).
func TestGuardOrphanCleanup_PIPNotScheduledWhenDesiredOrHasLB(t *testing.T) {
	uidDesired := "22222222-2222-2222-2222-222222222222"
	uidHasLB := "33333333-3333-3333-3333-333333333333"
	uidHasNRPLB := "44444444-4444-4444-4444-444444444444"

	dt := newTestDiffTracker()
	dt.K8sResources.Services.Insert(uidDesired)       // desired in K8s -> reconcileServices owns it
	dt.NRPResources.LoadBalancers.Insert(uidHasNRPLB) // registered LB -> not orphaned

	scheduleOrphanedResourceDeletions(dt, utilsets.NewString(uidHasLB), utilsets.NewString(), utilsets.NewString(
		uidDesired+"-pip",
		uidHasLB+"-pip", // its LB is the orphan; the LB path deletes the PIP
		uidHasNRPLB+"-pip",
	))

	_, desiredScheduled := dt.pendingServiceOps[uidDesired]
	assert.False(t, desiredScheduled, "a PIP whose service is desired in K8s must not be scheduled for deletion")

	_, nrpLBScheduled := dt.pendingServiceOps[uidHasNRPLB]
	assert.False(t, nrpLBScheduled, "a PIP whose service has a registered NRP LoadBalancer must not be scheduled by the PIP path")

	op, ok := dt.pendingServiceOps[uidHasLB]
	assert.True(t, ok, "an orphaned LB (with its PIP) must still be scheduled via the LB path")
	if ok {
		assert.True(t, op.IsOrphan)
	}
}
