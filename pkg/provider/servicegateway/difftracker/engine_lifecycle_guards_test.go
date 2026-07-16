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

package difftracker

import (
	"fmt"
	"testing"
)

// A delete arriving during an in-flight create must route the create's completion
// into the deletion flow, never leaving the service in StateCreated.
func TestGuardLifecycle_DeleteDuringCreateRoutesToDeletion(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-del-during-create"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	inflight := cfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid, Config: cfg, InFlightConfig: &inflight, State: StateCreationInProgress,
	}

	dt.DeleteService(uid, true, false)
	dt.OnServiceCreationComplete(uid, true, nil)

	if op, ok := dt.pendingServiceOps[uid]; ok && op.State == StateCreated {
		t.Fatalf("delete-during-create: service ended StateCreated; the create completion must route to deletion")
	}
}

// A stale/duplicate DeletePod for a pod no longer in live state must be a no-op
// (IsLastPod=false) and must not drive the ref-counter negative.
func TestGuardLifecycle_StaleDeletePodIsNoOp(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "egress-nat"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid, Config: NewOutboundServiceConfig(uid, nil), State: StateCreated,
	}
	dt.NRPResources.NATGateways.Insert(uid)

	dt.AddPod(uid, "ns/pod-a", "10.0.0.1", "10.244.0.5")

	first := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.5"}, "ns", "pod-a", "")
	if !first.IsLastPod {
		t.Fatalf("delete of the only pod should report IsLastPod=true, got false")
	}
	second := dt.DeletePod(uid, "10.0.0.1", []string{"10.244.0.5"}, "ns", "pod-a", "")
	if second.IsLastPod {
		t.Fatalf("duplicate delete must be a no-op (IsLastPod=false), got true")
	}
	if v, ok := dt.outboundIdentityPodRefCount.Load("egress-nat"); ok && v.(int) < 0 {
		t.Fatalf("ref-counter went negative (%d) after duplicate delete", v.(int))
	}
}

// A non-retryable (terminal) create failure must park the service in StateNotStarted
// with CreationFailedTerminal set, so the dispatcher stops retrying it.
func TestGuardLifecycle_TerminalCreateFailureParksService(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-terminal-create"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	inflight := cfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid, Config: cfg, InFlightConfig: &inflight, State: StateCreationInProgress,
	}

	dt.OnServiceCreationComplete(uid, false, newTerminalError(fmt.Errorf("unsupported protocol")))

	op := dt.pendingServiceOps[uid]
	if op == nil {
		t.Fatalf("terminal create failure must keep the service tracked, got removed")
	}
	if op.State != StateNotStarted || !op.CreationFailedTerminal {
		t.Fatalf("terminal create failure must park the service; got state=%v terminal=%v",
			op.State, op.CreationFailedTerminal)
	}
}

// Endpoints added then removed during creation must not leak the removed pod IP into
// K8s state after the buffered updates are replayed on completion.
func TestGuardLifecycle_BufferedEndpointAddRemoveNoLeak(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-buffer-replay"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	inflight := cfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid, Config: cfg, InFlightConfig: &inflight, State: StateCreationInProgress,
	}

	dt.UpdateEndpoints(uid, nil, map[string]string{"10.244.0.7": "10.0.0.1"})
	dt.UpdateEndpoints(uid, map[string]string{"10.244.0.7": "10.0.0.1"}, nil)

	dt.OnServiceCreationComplete(uid, true, nil)

	if node, ok := dt.K8sResources.Nodes["10.0.0.1"]; ok {
		if _, leaked := node.Pods["10.244.0.7"]; leaked {
			t.Fatalf("add-then-remove during create leaked pod IP 10.244.0.7 into K8s state after replay")
		}
	}
	if len(dt.pendingEndpoints[uid]) != 0 {
		t.Fatalf("pendingEndpoints must be drained after promotion, got %d", len(dt.pendingEndpoints[uid]))
	}
}

// Endpoint additions arriving while a service is pending deletion must not re-insert
// pod refs into K8s state (which would never be cleaned up on delete-success).
func TestGuardLifecycle_AdditionsDuringDeletionPendingNoLeak(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-del-pending"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid, Config: NewInboundServiceConfig(uid, makeInboundConfig(80)), State: StateDeletionPending,
	}

	dt.UpdateEndpoints(uid, nil, map[string]string{"10.244.0.11": "10.0.0.2"})
	dt.pendingServiceOps[uid].State = StateDeletionInProgress
	dt.OnServiceCreationComplete(uid, true, nil)

	if node, ok := dt.K8sResources.Nodes["10.0.0.2"]; ok {
		if _, leaked := node.Pods["10.244.0.11"]; leaked {
			t.Fatalf("addition during deletion leaked pod IP 10.244.0.11 into K8s state after delete-success")
		}
	}
}
