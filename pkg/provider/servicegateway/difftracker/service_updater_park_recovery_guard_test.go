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

// Guards for transient-failure park recovery. retryGate parks a service operation after
// maxServiceRetries transient failures (RetriesExhausted). A parked op is re-armed by a fresh
// external intent (a spec-changing UpdateService or a DeleteService) or, for a stable Service, by
// an ordinary resync once the park cooldown has elapsed. Without recovery a service whose budget
// was exhausted during a sustained-but-transient ARM/NRP outage stays stranded until a CCM restart;
// the delete case is the most damaging, since a parked delete never runs, leaking the Azure load
// balancer and public IP and leaving the Service stuck Terminating.

package difftracker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	servicehelper "k8s.io/cloud-provider/service/helpers"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
)

// gatedByRetry reports whether the dispatcher would skip the operation this pass. It mirrors the
// processBatch dispatch decision without spawning a worker goroutine.
func gatedByRetry(su *ServiceUpdater, dt *DiffTracker, uid string) bool {
	su.mu.Lock()
	su.activeOps[uid] = true
	su.mu.Unlock()
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return su.retryGate(uid, dt.pendingServiceOps[uid])
}

// TestGuardRetriesExhaustedPark_RecoveredBySpecChange verifies that a spec-changing UpdateService
// (the path EnsureLoadBalancer resync takes for a tracked service) clears a transient-exhausted
// park so the operation is dispatched again, while an unchanged-spec resync leaves the park intact
// so a normal resync does not defeat the backoff.
func TestGuardRetriesExhaustedPark_RecoveredBySpecChange(t *testing.T) {
	dt := newTestDiffTracker()
	su := newTestServiceUpdater(dt)
	uid := "svc-parked"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:       uid,
		Config:           NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:            StateNotStarted,
		RetryCount:       maxServiceRetries,
		RetriesExhausted: true,
		NextRetryAt:      time.Now().Add(time.Hour),
	}

	// An unchanged-spec resync must NOT reset the park (it would defeat the backoff under a
	// normal resync storm).
	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(80)))
	op := dt.pendingServiceOps[uid]
	assert.Equal(t, maxServiceRetries, op.RetryCount, "unchanged-spec resync must not reset the retry budget")
	assert.True(t, op.RetriesExhausted, "unchanged-spec resync must leave the park intact")
	assert.True(t, gatedByRetry(su, dt, uid), "the still-parked op must remain gated")

	// A spec change is fresh intent: the park must be cleared and the op dispatchable again.
	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(8080)))
	op = dt.pendingServiceOps[uid]
	assert.Equal(t, 0, op.RetryCount, "a spec-changing resync must reset the retry budget")
	assert.False(t, op.RetriesExhausted, "a spec-changing resync must clear the park")
	assert.True(t, op.NextRetryAt.IsZero(), "a spec-changing resync must clear the backoff deadline")
	assert.False(t, gatedByRetry(su, dt, uid), "the recovered op must no longer be gated by retryGate")
}

// TestGuardRetriesExhaustedPark_RecoveredByDelete verifies that deleting a service whose create
// budget was exhausted gives the delete a clean retry budget instead of inheriting the parked
// create budget. Without the reset the delete is parked too: deleteInboundService never runs and
// the Azure load balancer and public IP are leaked while the Service stays Terminating.
func TestGuardRetriesExhaustedPark_RecoveredByDelete(t *testing.T) {
	dt := newTestDiffTracker()
	su := newTestServiceUpdater(dt)
	uid := "svc-parked-delete"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:       uid,
		Config:           NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:            StateNotStarted,
		RetryCount:       maxServiceRetries,
		RetriesExhausted: true,
		NextRetryAt:      time.Now().Add(time.Hour),
	}

	dt.DeleteService(uid, true, false)

	op := dt.pendingServiceOps[uid]
	assert.Equal(t, 0, op.RetryCount, "a delete must reset the inherited create retry budget")
	assert.False(t, op.RetriesExhausted, "a delete must clear the inherited park")
	assert.True(t, op.NextRetryAt.IsZero(), "a delete must clear the inherited backoff deadline")
	assert.False(t, gatedByRetry(su, dt, uid), "the delete must be dispatchable, not gated by retryGate")
}

// TestGuardRetriesExhaustedPark_CreateRecoversAfterCooldown verifies that a create which exhausted
// its retry budget during a transient outage is re-armed by an ordinary same-spec resync once the
// park cooldown has elapsed, so a stable Service eventually gets its load balancer and public IP
// without a spec edit, a delete, or a CCM restart. While the cooldown is still pending the park is
// held instead, to avoid a per-resync retry storm.
func TestGuardRetriesExhaustedPark_CreateRecoversAfterCooldown(t *testing.T) {
	dt := newTestDiffTracker()
	su := newTestServiceUpdater(dt)
	uid := "svc-parked-create-stable-spec"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:       uid,
		Config:           NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:            StateNotStarted,
		RetryCount:       maxServiceRetries,
		RetriesExhausted: true,
		NextRetryAt:      time.Now().Add(-time.Hour), // cooldown elapsed
	}

	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(80))) // ordinary same-spec resync

	op := dt.pendingServiceOps[uid]
	assert.False(t, op.RetriesExhausted, "a same-spec resync after the cooldown must clear the park")
	assert.Equal(t, 0, op.RetryCount, "the retry budget must be reset so the create can be dispatched")
	assert.False(t, gatedByRetry(su, dt, uid), "the recovered create must be dispatchable")
}

// TestGuardRetriesExhaustedPark_UpdateInProgressRecoversOnSpecChange verifies that an op which
// exhausted its retry budget while updating is re-armed by a genuine spec change. A parked op has no
// in-flight worker to pick up the new config, so the spec change must clear the park and re-dispatch
// rather than be silently dropped.
func TestGuardRetriesExhaustedPark_UpdateInProgressRecoversOnSpecChange(t *testing.T) {
	dt := newTestDiffTracker()
	su := newTestServiceUpdater(dt)
	uid := "svc-parked-update"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:       uid,
		Config:           NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:            StateUpdateInProgress,
		RetryCount:       maxServiceRetries,
		RetriesExhausted: true,
		NextRetryAt:      time.Now().Add(-time.Hour),
	}

	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(8080))) // genuine spec change

	op := dt.pendingServiceOps[uid]
	assert.False(t, op.RetriesExhausted, "a spec change must clear a parked updating op")
	assert.Equal(t, 0, op.RetryCount, "a spec change must reset the parked update retry budget")
	assert.False(t, gatedByRetry(su, dt, uid), "the recovered update must be dispatchable")
}

// TestGuardRetriesExhaustedPark_UpdateRecoversAfterCooldown verifies that an op which exhausted its
// retry budget while updating is re-armed by an ordinary same-spec resync once the park cooldown has
// elapsed, so a stable Service applies the pending update instead of serving stale config until a
// CCM restart. While the cooldown is still pending the park is held, to avoid a per-resync retry storm.
func TestGuardRetriesExhaustedPark_UpdateRecoversAfterCooldown(t *testing.T) {
	dt := newTestDiffTracker()
	su := newTestServiceUpdater(dt)
	uid := "svc-parked-update-stable-spec"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:       uid,
		Config:           NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:            StateUpdateInProgress,
		RetryCount:       maxServiceRetries,
		RetriesExhausted: true,
		NextRetryAt:      time.Now().Add(time.Hour), // still within cooldown
	}

	// While the cooldown is pending, a same-spec resync must leave the park intact.
	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(80)))
	op := dt.pendingServiceOps[uid]
	assert.True(t, op.RetriesExhausted, "a same-spec resync within the cooldown must leave the park intact")
	assert.True(t, gatedByRetry(su, dt, uid), "the still-parked update must remain gated")

	// Once the cooldown has elapsed, a same-spec resync must re-arm it.
	op.NextRetryAt = time.Now().Add(-time.Hour)
	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(80)))
	op = dt.pendingServiceOps[uid]
	assert.False(t, op.RetriesExhausted, "a same-spec resync after the cooldown must clear the park")
	assert.Equal(t, 0, op.RetryCount, "the retry budget must be reset so the update can be dispatched")
	assert.False(t, gatedByRetry(su, dt, uid), "the recovered update must be dispatchable")
}

// TestGuardRetriesExhaustedPark_ParkedDeleteRecoversOnRedelete verifies that a delete which itself
// exhausted its retry budget (parked in StateDeletionInProgress) is re-armed by a repeated delete,
// so the deletion eventually runs instead of leaking the Azure load balancer and public IP and
// leaving the Service stuck Terminating.
func TestGuardRetriesExhaustedPark_ParkedDeleteRecoversOnRedelete(t *testing.T) {
	dt := newTestDiffTracker()
	su := newTestServiceUpdater(dt)
	uid := "svc-parked-delete-inflight"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:       uid,
		Config:           NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:            StateDeletionInProgress,
		RetryCount:       maxServiceRetries,
		RetriesExhausted: true,
		NextRetryAt:      time.Now().Add(-time.Hour),
	}

	dt.DeleteService(uid, true, false) // repeated delete while the finalizer is still present

	op := dt.pendingServiceOps[uid]
	assert.False(t, op.RetriesExhausted, "a repeated delete must clear a parked deletion")
	assert.Equal(t, 0, op.RetryCount, "a repeated delete must reset the parked delete retry budget")
	assert.False(t, gatedByRetry(su, dt, uid), "the recovered delete must be dispatchable")
}

// TestParkedDelete_SelfReArmsAfterCooldown verifies that a delete parked after exhausting its retry
// budget re-arms itself once the cooldown has elapsed, without depending on a second DeleteService
// call. The upstream controller removes its own load-balancer finalizer on the first (async) delete
// and never re-calls EnsureLoadBalancerDeleted, so retryGate must drive the recovery itself.
func TestParkedDelete_SelfReArmsAfterCooldown(t *testing.T) {
	dt := newTestDiffTracker()
	su := newTestServiceUpdater(dt)
	uid := "svc-parked-delete-cooldown"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:       uid,
		Config:           NewInboundServiceConfig(uid, nil),
		State:            StateDeletionInProgress,
		RetryCount:       maxServiceRetries,
		RetriesExhausted: true,
		NextRetryAt:      time.Now().Add(-time.Minute), // cooldown elapsed
	}

	assert.False(t, gatedByRetry(su, dt, uid),
		"a parked delete past its cooldown must be re-armed and dispatchable without a second DeleteService")
	op := dt.pendingServiceOps[uid]
	assert.False(t, op.RetriesExhausted, "the parked delete must be re-armed (park cleared)")
	assert.Equal(t, 0, op.RetryCount, "the parked delete must get a fresh retry budget")
}

// TestParkedCreate_SelfReArmsAfterCooldown verifies that a create parked after exhausting its retry
// budget re-arms itself in retryGate once the cooldown has elapsed, without depending on any external
// driver. On a stable cluster the upstream controller does NOT re-drive an unchanged Service - its
// periodic resync calls UpdateFunc(obj, obj) -> needsUpdate=false, and UpdateLoadBalancer is a no-op
// in ServiceGateway mode - so without this self-rearm the parked create strands until a CCM restart
// and its load balancer and public IP are never provisioned. Within the cooldown it must stay gated,
// to bound retries to one burst per cooldown.
func TestParkedCreate_SelfReArmsAfterCooldown(t *testing.T) {
	dt := newTestDiffTracker()
	su := newTestServiceUpdater(dt)
	uid := "svc-parked-create-cooldown"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:       uid,
		Config:           NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:            StateNotStarted,
		RetryCount:       maxServiceRetries,
		RetriesExhausted: true,
		NextRetryAt:      time.Now().Add(time.Hour), // cooldown still pending
	}

	// While the cooldown is pending the parked create stays gated (no per-resync storm).
	assert.True(t, gatedByRetry(su, dt, uid),
		"a parked create within its cooldown must stay gated")
	assert.True(t, dt.pendingServiceOps[uid].RetriesExhausted, "the create must remain parked during the cooldown")

	// Once the cooldown elapses, retryGate must re-arm the parked create itself.
	dt.pendingServiceOps[uid].NextRetryAt = time.Now().Add(-time.Minute)
	assert.False(t, gatedByRetry(su, dt, uid),
		"a parked create past its cooldown must self-rearm in retryGate (no external driver re-drives it on a stable cluster)")
	op := dt.pendingServiceOps[uid]
	assert.False(t, op.RetriesExhausted, "the parked create must be re-armed (park cleared)")
	assert.Equal(t, 0, op.RetryCount, "the parked create must get a fresh retry budget")
}

// TestParkedUpdate_SelfReArmsAfterCooldown covers the update path: an op parked while updating
// re-arms itself in retryGate once the cooldown elapses, so a stable Service applies its pending
// config instead of serving stale config until a CCM restart.
func TestParkedUpdate_SelfReArmsAfterCooldown(t *testing.T) {
	dt := newTestDiffTracker()
	su := newTestServiceUpdater(dt)
	uid := "svc-parked-update-cooldown"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:       uid,
		Config:           NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:            StateUpdateInProgress,
		RetryCount:       maxServiceRetries,
		RetriesExhausted: true,
		NextRetryAt:      time.Now().Add(-time.Minute), // cooldown elapsed
	}

	assert.False(t, gatedByRetry(su, dt, uid),
		"a parked update past its cooldown must self-rearm in retryGate")
	op := dt.pendingServiceOps[uid]
	assert.False(t, op.RetriesExhausted, "the parked update must be re-armed (park cleared)")
	assert.Equal(t, 0, op.RetryCount, "the parked update must get a fresh retry budget")
}

// TestParkedDelete_SkippedDuringCooldown verifies that a delete which has exhausted its retry budget
// (RetriesExhausted=true) and whose park cooldown has not yet elapsed is skipped by the dispatcher:
// processBatch dispatches no worker, the SGW cleanup finalizer is left in place, and activeOps is
// released so retryGate can re-evaluate on a later pass. Once the cooldown elapses the delete re-arms
// itself (see TestParkedDelete_SelfReArmsAfterCooldown), so the deletion resumes without depending on
// a second EnsureLoadBalancerDeleted call from the upstream controller.
func TestParkedDelete_SkippedDuringCooldown(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// If any Azure client method is invoked at all, the test fails: that would mean processBatch
	// dispatched the delete, the opposite of the skip-during-cooldown behavior asserted here.
	f := mock_azclient.NewMockClientFactory(ctrl)
	sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
	lb := mock_loadbalancerclient.NewMockInterface(ctrl)
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	f.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(lb).AnyTimes()
	f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
	sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	lb.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	// Pre-load a K8s Service carrying the SGW finalizer so we can prove the finalizer is NOT
	// stripped while the op is parked.
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc", Namespace: "default", UID: "uid-parked-del",
			Finalizers: []string{ServiceGatewayServiceCleanupFinalizer, servicehelper.LoadBalancerCleanupFinalizer},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	kube := fake.NewSimpleClientset(svc)

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.kubeClient = kube
	dt.networkClientFactory = f
	dt.NRPResources.LoadBalancers.Insert("uid-parked-del")

	uid := "uid-parked-del"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:       uid,
		Config:           NewInboundServiceConfig(uid, nil),
		State:            StateDeletionInProgress,
		RetryCount:       maxServiceRetries,
		RetriesExhausted: true,
		// Park cooldown still in effect: the op is skipped this pass. It re-arms itself once the
		// cooldown elapses (see TestParkedDelete_SelfReArmsAfterCooldown).
		NextRetryAt: time.Now().Add(time.Hour),
	}
	dt.pendingServiceDeletions[uid] = &PendingServiceDeletion{ServiceUID: uid, IsInbound: true}

	su := newTestServiceUpdater(dt)

	// processBatch must SKIP the parked op: no dispatch (proven by the Times(0) expectations on
	// the SGW/LB/PIP clients above) and activeOps released so retryGate can re-evaluate later.
	su.processBatch()

	op := dt.pendingServiceOps[uid]
	assert.Equal(t, StateDeletionInProgress, op.State,
		"parked delete op must stay in StateDeletionInProgress (not advanced, not cleared)")
	assert.True(t, op.RetriesExhausted, "parked delete op must remain parked")

	su.mu.Lock()
	_, active := su.activeOps[uid]
	su.mu.Unlock()
	assert.False(t, active, "retryGate must release activeOps for the parked op")

	// The K8s Service finalizer must STILL be present (we never dispatched the worker, so
	// removeServiceGatewayFinalizer was never invoked).
	got, err := kube.CoreV1().Services("default").Get(context.Background(), "svc", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.ObjectMeta.Finalizers, ServiceGatewayServiceCleanupFinalizer,
		"parked delete: SGW cleanup finalizer must NOT have been removed (no worker dispatched)")
}

// TestUpdateService_CreationFailedTerminalRecoversOnSpecChange verifies that an op parked with
// CreationFailedTerminal (a non-retryable, spec-driven failure) recovers when an UpdateService
// arrives with a changed spec: the terminal flag clears, the retry budget resets, the state returns
// to StateNotStarted, and the dispatcher is nudged. An unchanged-spec resync leaves the park intact.
func TestUpdateService_CreationFailedTerminalRecoversOnSpecChange(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-terminal-recover"
	oldCfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:             uid,
		Config:                 oldCfg,
		State:                  StateNotStarted,
		CreationFailedTerminal: true,
		// Also pile on a real retry-budget exhaustion to prove the recovery clears BOTH
		// terminal AND backoff state, not just one.
		RetryCount:       maxServiceRetries,
		RetriesExhausted: true,
		NextRetryAt:      time.Now().Add(time.Hour),
	}
	// Drain any pre-existing trigger so we can assert the recovery fires a fresh one.
	select {
	case <-dt.serviceUpdaterTrigger:
	default:
	}

	dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(8080)))

	op := dt.pendingServiceOps[uid]
	assert.False(t, op.CreationFailedTerminal,
		"a spec-changing UpdateService MUST clear CreationFailedTerminal")
	assert.False(t, op.RetriesExhausted, "park (RetriesExhausted) MUST also be cleared on recovery")
	assert.Equal(t, 0, op.RetryCount, "RetryCount MUST be reset to 0 on recovery")
	assert.True(t, op.NextRetryAt.IsZero(), "NextRetryAt MUST be cleared so the dispatcher does not skip the recovered op")
	assert.Equal(t, StateNotStarted, op.State,
		"the recovered op MUST be StateNotStarted so the dispatcher picks it up next pass")
	// Config was overwritten with the new spec.
	assert.Equal(t, int32(8080), op.Config.InboundConfig.FrontendPorts[0].Port,
		"the new (changed) spec must be stored on the op")

	// The dispatcher must be nudged: without this nudge a parked op has no worker to pick it
	// up and would wait for an unrelated future trigger.
	select {
	case <-dt.serviceUpdaterTrigger:
	default:
		t.Fatal("recovery must enqueue a fresh ServiceUpdater trigger")
	}
}
