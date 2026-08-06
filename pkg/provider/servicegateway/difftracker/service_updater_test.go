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
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	servicehelper "k8s.io/cloud-provider/service/helpers"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/natgatewayclient/mock_natgatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// newTestServiceUpdater builds a ServiceUpdater wired to dt for unit tests that exercise
// dispatcher logic without Azure clients (no goroutines are spawned for the cases tested).
func newTestServiceUpdater(dt *DiffTracker) *ServiceUpdater {
	return &ServiceUpdater{
		diffTracker: dt,
		onComplete:  func(string, bool, error) {},
		trigger:     dt.serviceUpdaterTrigger,
		ctx:         context.Background(),
		semaphore:   make(chan struct{}, 10),
		activeOps:   make(map[string]bool),
	}
}

// TestGuardServiceUpdater_BackoffAndTerminalCeiling verifies that a transient (non-terminal) create
// failure must schedule a backoff (NextRetryAt in the future, advancing per attempt); the dispatcher
// must skip the op while it is in backoff (no immediate re-dispatch hot-loop); and after
// maxServiceRetries the op is parked (RetriesExhausted) and no longer dispatched.
func TestGuardServiceUpdater_BackoffAndTerminalCeiling(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-backoff"
	transientErr := errors.New("transient ARM 429")

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateCreationInProgress,
	}

	// failOnce puts the op back in-flight (as the dispatcher would) and signals a transient
	// failure, returning the scheduled backoff delay.
	failOnce := func() time.Duration {
		op := dt.pendingServiceOps[uid]
		op.State = StateCreationInProgress
		snap := op.Config
		op.InFlightConfig = &snap
		before := time.Now()
		dt.OnServiceCreationComplete(uid, false, transientErr)
		return dt.pendingServiceOps[uid].NextRetryAt.Sub(before)
	}

	// First transient failure: RetryCount advances, NextRetryAt is set in the future.
	d1 := failOnce()
	assert.Equal(t, 1, dt.pendingServiceOps[uid].RetryCount)
	assert.Greater(t, d1, time.Duration(0), "a transient failure must schedule a future retry")
	assert.Equal(t, StateNotStarted, dt.pendingServiceOps[uid].State)

	// The dispatcher must SKIP the op while it is in backoff (now < NextRetryAt): not dispatched
	// and activeOps released - i.e. no immediate re-dispatch hot-loop.
	su := newTestServiceUpdater(dt)
	su.processBatch()
	assert.Equal(t, StateNotStarted, dt.pendingServiceOps[uid].State, "op in backoff must not be dispatched")
	su.mu.Lock()
	_, active := su.activeOps[uid]
	su.mu.Unlock()
	assert.False(t, active, "activeOps must be released for a backoff-skipped op")

	// Second failure: the backoff grows per attempt.
	d2 := failOnce()
	assert.Equal(t, 2, dt.pendingServiceOps[uid].RetryCount)
	assert.Greater(t, d2, d1, "backoff must grow per attempt")

	// Terminal ceiling: at maxServiceRetries the op is parked and no longer dispatched.
	op := dt.pendingServiceOps[uid]
	op.RetryCount = maxServiceRetries
	op.NextRetryAt = time.Time{} // exercise the ceiling, not the backoff window
	su.processBatch()
	assert.True(t, dt.pendingServiceOps[uid].RetriesExhausted, "op must park after exhausting the retry budget")
	assert.Equal(t, StateNotStarted, dt.pendingServiceOps[uid].State, "parked op must not be dispatched")
}

// TestCreateInboundService_TransientServiceLookupErrorDoesNotCreatePIP verifies that a transient
// (non-NotFound) error when looking up the Service in Step 0 aborts the create and reports a
// retryable failure, rather than proceeding to create the PIP/LB/SGW with no K8s cleanup-finalizer
// anchor. A genuine NotFound is handled separately (the service is gone).
func TestCreateInboundService_TransientServiceLookupErrorDoesNotCreatePIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// The Service List fails transiently, so getServiceByUID returns a generic wrapped error
	// (not a typed NotFound).
	kube := fake.NewSimpleClientset()
	kube.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("transient apiserver error")
	})

	// The PIP must never be created on this path; Times(0) fails the test if it is.
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	pip.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	factory := mock_azclient.NewMockClientFactory(ctrl)
	factory.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.kubeClient = kube
	dt.networkClientFactory = factory

	var gotSuccess *bool
	var gotErr error
	su := newTestServiceUpdater(dt)
	su.onComplete = func(_ string, ok bool, err error) {
		v := ok
		gotSuccess = &v
		gotErr = err
	}

	su.createInboundService("uid-x", makeInboundConfig(80), "corr-x")

	if assert.NotNil(t, gotSuccess, "onComplete must be called") {
		assert.False(t, *gotSuccess, "a transient service-lookup error must fail the op for retry")
	}
	assert.Error(t, gotErr, "the transient error must be propagated")
}

// TestCreateInboundService_ServiceGoneNotFoundAbortsWithoutCreatingResources verifies that when the
// K8s Service is gone (getServiceByUID returns a typed NotFound), createInboundService must abort -
// it must NOT fall through to create the PIP/LB/SGW (which would be orphaned with no Service object
// to ever clean them up). It drops tracking and does NOT call onComplete (which would loop on
// NotFound or falsely report Created); a still-live Service is re-added on the next resync.
func TestCreateInboundService_ServiceGoneNotFoundAbortsWithoutCreatingResources(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Empty kube (no Services): the List succeeds but no UID matches, so getServiceByUID returns a
	// typed NotFound.
	kube := fake.NewSimpleClientset()

	// The PIP must never be created on the abort path.
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	pip.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	factory := mock_azclient.NewMockClientFactory(ctrl)
	factory.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.kubeClient = kube
	dt.networkClientFactory = factory
	uid := "uid-gone"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateCreationInProgress,
	}

	completeCalled := false
	su := newTestServiceUpdater(dt)
	su.onComplete = func(string, bool, error) { completeCalled = true }

	su.createInboundService(uid, makeInboundConfig(80), "corr-gone")

	dt.mu.Lock()
	_, tracked := dt.pendingServiceOps[uid]
	dt.mu.Unlock()
	assert.False(t, tracked, "a NotFound (service gone) create must drop tracking, not orphan resources")
	assert.False(t, completeCalled, "the abort path must not call onComplete")
}

// TestServiceUpdaterWorker_RecoversFromPanic verifies that a panic inside a worker operation is
// recovered (so the CCM process survives) and reported as a failed op via onComplete, rather than
// crashing the whole process.
func TestServiceUpdaterWorker_RecoversFromPanic(t *testing.T) {
	// A panicking fake client: the Service List panics, so createInboundService Step 0 panics.
	kube := fake.NewSimpleClientset()
	kube.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		panic("simulated apiserver client panic")
	})

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.kubeClient = kube

	var gotSuccess *bool
	var gotErr error
	su := newTestServiceUpdater(dt)
	su.onComplete = func(_ string, ok bool, err error) {
		v := ok
		gotSuccess = &v
		gotErr = err
	}

	su.wg.Add(1)
	assert.NotPanics(t, func() {
		su.runWorker("uid-panic", func() {
			su.createInboundService("uid-panic", makeInboundConfig(80), "corr-panic")
		})
	}, "a panic in a worker operation must be recovered, not propagated")
	su.wg.Wait()

	if assert.NotNil(t, gotSuccess, "onComplete must be called after a recovered panic") {
		assert.False(t, *gotSuccess, "a panicking op must be reported as a failed operation")
	}
	if assert.Error(t, gotErr) {
		assert.Contains(t, gotErr.Error(), "panic", "the failure must carry the panic info")
	}
}

// TestServiceUpdaterProcessBatchFlow asserts how processBatch categorises each pending operation:
// which states it promotes and dispatches, and which it leaves untouched.
//
// The state transitions asserted below are made synchronously by processBatch while it holds the
// lock, before any worker goroutine is spawned, and the completion callback used here records
// results without mutating engine state. The Azure clients are permissive because the workers'
// outcome is not what is under test.
func TestServiceUpdaterProcessBatchFlow(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	m := newOutboundMocks(ctrl)
	mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
	m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	m.expectNoDisassociation()
	m.pip.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&armnetwork.PublicIPAddress{Name: ptr.To("pip")}, nil).AnyTimes()
	m.pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLB.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mockLB.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	m.sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	m.nat.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	// The Services must exist: a dispatched operation looks its Service up by UID, and an operation
	// whose Service is gone is dropped from tracking rather than dispatched.
	uids := []string{"not-started", "creation-in-progress", "created", "deletion-pending", "deletion-in-progress", "parked"}
	objects := make([]runtime.Object, 0, len(uids))
	for _, uid := range uids {
		objects = append(objects, &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: uid, Namespace: "default", UID: types.UID(uid),
				Finalizers: []string{ServiceGatewayServiceCleanupFinalizer},
			},
			Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		})
	}

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.networkClientFactory = m.factory
	dt.kubeClient = fake.NewSimpleClientset(objects...)

	newOp := func(uid string, state ResourceState) *ServiceOperationState {
		return &ServiceOperationState{ServiceUID: uid, Config: NewInboundServiceConfig(uid, nil), State: state}
	}
	dt.pendingServiceOps = map[string]*ServiceOperationState{
		"not-started":          newOp("not-started", StateNotStarted),
		"creation-in-progress": newOp("creation-in-progress", StateCreationInProgress),
		"created":              newOp("created", StateCreated),
		"deletion-pending":     newOp("deletion-pending", StateDeletionPending),
		"deletion-in-progress": newOp("deletion-in-progress", StateDeletionInProgress),
		"parked":               newOp("parked", StateNotStarted),
	}
	dt.pendingServiceOps["parked"].CreationFailedTerminal = true

	updater := outboundUpdater(dt, &outboundCompletion{})
	updater.processBatch()
	updater.wg.Wait()

	dt.mu.Lock()
	defer dt.mu.Unlock()

	// Promoted and dispatched.
	assert.Equal(t, StateCreationInProgress, dt.pendingServiceOps["not-started"].State,
		"an unstarted operation must be promoted to CreationInProgress and dispatched")
	assert.NotNil(t, dt.pendingServiceOps["not-started"].InFlightConfig,
		"the dispatched config must be snapshotted as in-flight")

	// Left untouched.
	assert.Equal(t, StateCreationInProgress, dt.pendingServiceOps["creation-in-progress"].State,
		"a creation already in flight must not be dispatched again")
	assert.Nil(t, dt.pendingServiceOps["creation-in-progress"].InFlightConfig,
		"a skipped operation must not have a config snapshotted for it")
	assert.Equal(t, StateCreated, dt.pendingServiceOps["created"].State,
		"a completed service must not be re-dispatched")
	assert.Equal(t, StateDeletionPending, dt.pendingServiceOps["deletion-pending"].State,
		"a deletion still waiting for its locations to drain must not be dispatched")
	assert.Equal(t, StateNotStarted, dt.pendingServiceOps["parked"].State,
		"an operation parked after a terminal failure must not be re-dispatched")
	assert.Nil(t, dt.pendingServiceOps["parked"].InFlightConfig,
		"a parked operation must not have a config snapshotted for it")
}

// TestServiceUpdaterRequeueKeepsInitTriggerCounterBalanced verifies that the follow-up
// trigger emitted by requeueIfMoreWork is accounted for in the initialization in-flight
// counter. During initialization, every processBatch decrements pendingUpdaterTriggers,
// so a requeue that did not increment it would drive the counter negative and prevent
// WaitForInitialSync from ever completing.
func TestServiceUpdaterRequeueKeepsInitTriggerCounterBalanced(t *testing.T) {
	dt := newTestDiffTracker()
	atomic.StoreInt32(&dt.isInitializing, 1)
	dt.initCompletionChecker = make(chan struct{}) // production sets this in startInitialization
	su := newTestServiceUpdater(dt)

	atomic.StoreInt32(&dt.pendingUpdaterTriggers, 0)
	su.requeueIfMoreWork("svc")
	assert.Equal(t, int32(1), atomic.LoadInt32(&dt.pendingUpdaterTriggers),
		"requeue during initialization should increment the in-flight trigger counter")

	<-dt.serviceUpdaterTrigger // worker consumes the follow-up trigger
	su.processBatch()
	assert.Equal(t, int32(0), atomic.LoadInt32(&dt.pendingUpdaterTriggers),
		"requeue + processBatch should net zero (no counter poisoning)")
}

// TestServiceUpdaterProcessBatchSkipsParkedService verifies that a service parked after a
// non-retryable creation error (CreationFailedTerminal) is not re-dispatched, preventing
// an infinite retry loop on deterministic (invalid-spec) failures.
func TestServiceUpdaterProcessBatchSkipsParkedService(t *testing.T) {
	dt := newTestDiffTracker()
	dt.pendingServiceOps["svc"] = &ServiceOperationState{
		ServiceUID:             "svc",
		Config:                 NewInboundServiceConfig("svc", nil),
		State:                  StateNotStarted,
		CreationFailedTerminal: true,
	}
	su := newTestServiceUpdater(dt)

	su.processBatch()

	assert.Equal(t, StateNotStarted, dt.pendingServiceOps["svc"].State,
		"parked service must not be transitioned/dispatched")
	assert.Len(t, dt.serviceUpdaterTrigger, 0, "parked service must not enqueue further work")
}

// TestCreateInboundService_StatusUpdateFailureRetriesInsteadOfFalseSuccess drives createInboundService
// with all Azure steps succeeding but the Service-status patch (Step 5) returning a transient non-409
// error. Because the load balancer would otherwise appear permanently pending, the op must report
// failure so the existing retry path re-runs (the Azure resources are idempotent), rather than
// reporting success and moving to StateCreated with an empty Ingress.
func TestCreateInboundService_StatusUpdateFailureRetriesInsteadOfFalseSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Pre-load the K8s Service WITH the SGW + LB finalizers so addServiceGatewayFinalizer
	// short-circuits on the Get (no Patch needed) — this isolates the Patch reactor below to
	// the Step 5 status patch only.
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-status", Namespace: "default", UID: "uid-status",
			Finalizers: []string{ServiceGatewayServiceCleanupFinalizer, servicehelper.LoadBalancerCleanupFinalizer},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	kube := fake.NewSimpleClientset(svc)
	// Force every Service patch to fail with a generic (non-409, non-NotFound) transient error.
	// retry.RetryOnConflict only retries on Conflict, so this propagates as a hard error.
	kube.PrependReactor("patch", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("transient apiserver patch error")
	})

	f := mock_azclient.NewMockClientFactory(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
	mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	f.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	f.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()

	// PIP returns a populated response so pipIPAddress is non-empty and Step 5 actually runs.
	mockPIP.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "uid-status-pip", gomock.Any()).Return(
		&armnetwork.PublicIPAddress{
			Name: ptr.To("uid-status-pip"),
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{
				IPAddress: ptr.To("10.1.2.3"),
			},
		}, nil)
	mockLB.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "uid-status", gomock.Any()).Return(nil, nil)
	mockSGW.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil)

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.kubeClient = kube
	dt.networkClientFactory = f
	uid := "uid-status"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateCreationInProgress,
		InFlightConfig: func() *ServiceConfig {
			c := NewInboundServiceConfig(uid, makeInboundConfig(80))
			return &c
		}(),
	}

	var gotSuccess *bool
	var gotErr error
	su := newTestServiceUpdater(dt)
	su.onComplete = func(serviceUID string, ok bool, err error) {
		b := ok
		gotSuccess = &b
		gotErr = err
		// Route through the engine completion to drive the StateCreated transition.
		dt.OnServiceCreationComplete(serviceUID, ok, err)
	}

	su.createInboundService(uid, makeInboundConfig(80), "corr-status")

	if assert.NotNil(t, gotSuccess, "onComplete must be called") {
		assert.False(t, *gotSuccess, "a status-patch failure must fail the op, not report success")
	}
	if assert.Error(t, gotErr, "the status-patch failure must be propagated") {
		assert.Contains(t, gotErr.Error(), "failed to update service status with external IP")
	}

	op := dt.pendingServiceOps[uid]
	if assert.NotNil(t, op, "op must remain tracked") {
		assert.Equal(t, StateNotStarted, op.State, "a status-patch failure must reset the op for retry, not promote it to StateCreated")
		assert.Equal(t, 1, op.RetryCount, "a status-patch failure must schedule a retry")
	}

	// The status patch failed, so Ingress stays empty for this attempt; the scheduled retry repopulates it.
	got, err := kube.CoreV1().Services("default").Get(context.Background(), "svc-status", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Empty(t, got.Status.LoadBalancer.Ingress)
}

// TestCreateInboundService_PopulatesIngressOnSuccess confirms the success path writes the allocated
// public IP into Service.Status.LoadBalancer.Ingress and promotes the op to StateCreated, so a later
// retry caused by a transient status failure eventually surfaces the external IP.
func TestCreateInboundService_PopulatesIngressOnSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc-status-ok", Namespace: "default", UID: "uid-status-ok",
			Finalizers: []string{ServiceGatewayServiceCleanupFinalizer, servicehelper.LoadBalancerCleanupFinalizer},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
	kube := fake.NewSimpleClientset(svc)

	f := mock_azclient.NewMockClientFactory(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
	mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	f.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	f.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()

	mockPIP.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "uid-status-ok-pip", gomock.Any()).Return(
		&armnetwork.PublicIPAddress{
			Name:       ptr.To("uid-status-ok-pip"),
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{IPAddress: ptr.To("10.1.2.3")},
		}, nil)
	mockLB.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "uid-status-ok", gomock.Any()).Return(nil, nil)
	mockSGW.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil)

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.kubeClient = kube
	dt.networkClientFactory = f
	uid := "uid-status-ok"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateCreationInProgress,
		InFlightConfig: func() *ServiceConfig {
			c := NewInboundServiceConfig(uid, makeInboundConfig(80))
			return &c
		}(),
	}

	var gotSuccess *bool
	su := newTestServiceUpdater(dt)
	su.onComplete = func(serviceUID string, ok bool, err error) {
		b := ok
		gotSuccess = &b
		dt.OnServiceCreationComplete(serviceUID, ok, err)
	}

	su.createInboundService(uid, makeInboundConfig(80), "corr-status-ok")

	if assert.NotNil(t, gotSuccess, "onComplete must be called") {
		assert.True(t, *gotSuccess, "a create with a successful status patch must report success")
	}
	op := dt.pendingServiceOps[uid]
	if assert.NotNil(t, op, "op must remain tracked") {
		assert.Equal(t, StateCreated, op.State, "a successful create must promote the op to StateCreated")
	}

	got, err := kube.CoreV1().Services("default").Get(context.Background(), "svc-status-ok", metav1.GetOptions{})
	assert.NoError(t, err)
	if assert.Len(t, got.Status.LoadBalancer.Ingress, 1, "the allocated IP must be written to the Service status") {
		assert.Equal(t, "10.1.2.3", got.Status.LoadBalancer.Ingress[0].IP)
	}
}

// TestCreateInboundServiceClearsBuffersWhenServiceGone verifies that aborting createInboundService
// because the Service no longer exists also drops the endpoints and pods buffered for its in-flight
// creation, so they do not leak until the next restart.
func TestCreateInboundServiceClearsBuffersWhenServiceGone(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const uid = "11111111-1111-1111-1111-111111111111"
	dt := newTestDiffTracker()
	dt.kubeClient = fake.NewSimpleClientset() // empty: getServiceByUID returns a typed NotFound
	dt.networkClientFactory = mock_azclient.NewMockClientFactory(ctrl)

	dt.pendingServiceOps[uid] = &ServiceOperationState{ServiceUID: uid, State: StateCreationInProgress}
	dt.pendingEndpoints[uid] = []PendingEndpointUpdate{{PodIPToNodeIP: map[string]string{"10.244.0.1": "10.0.0.1"}}}
	dt.pendingPods[uid] = []PendingPodUpdate{{PodKey: "ns/p", Location: "10.0.0.1", Address: "10.244.0.1"}}

	su := NewServiceUpdater(context.Background(), dt, func(string, bool, error) {}, dt.GetServiceUpdaterTrigger())
	su.createInboundService(uid, &InboundConfig{}, "corr")

	dt.mu.Lock()
	defer dt.mu.Unlock()
	if _, ok := dt.pendingServiceOps[uid]; ok {
		t.Fatalf("aborted create must drop the service operation")
	}
	if _, ok := dt.pendingEndpoints[uid]; ok {
		t.Fatalf("aborted create must drop buffered endpoints")
	}
	if _, ok := dt.pendingPods[uid]; ok {
		t.Fatalf("aborted create must drop buffered pods")
	}
}

// ---------------------------------------------------------------------------------------------
// Outbound (egress) lifecycle.
//
// deleteOutboundService is the most destructive operation in the feature: it disassociates the NAT
// Gateway from the ServiceGateway, unregisters it from NRP, deletes the NAT Gateway and its Public
// IP, and only then releases the last-pod finalizers holding egress pods - and therefore node
// drains and namespace deletions - open. Every failing step must report failure and retain NRP
// state so the operation is retried instead of leaking the Azure resource.
// ---------------------------------------------------------------------------------------------

// outboundMocks bundles the clients deleteOutboundService/createOutboundService drive.
type outboundMocks struct {
	factory *mock_azclient.MockClientFactory
	sgw     *mock_servicegatewayclient.MockInterface
	nat     *mock_natgatewayclient.MockInterface
	pip     *mock_publicipaddressclient.MockInterface
}

func newOutboundMocks(ctrl *gomock.Controller) *outboundMocks {
	m := &outboundMocks{
		factory: mock_azclient.NewMockClientFactory(ctrl),
		sgw:     mock_servicegatewayclient.NewMockInterface(ctrl),
		nat:     mock_natgatewayclient.NewMockInterface(ctrl),
		pip:     mock_publicipaddressclient.NewMockInterface(ctrl),
	}
	m.factory.EXPECT().GetServiceGatewayClient().Return(m.sgw).AnyTimes()
	m.factory.EXPECT().GetNatGatewayClient().Return(m.nat).AnyTimes()
	m.factory.EXPECT().GetPublicIPAddressClient().Return(m.pip).AnyTimes()
	return m
}

// expectNoDisassociation makes Step 1 of deleteOutboundService a clean no-op: the ServiceGateway
// reports no matching service and the NAT Gateway is already gone.
func (m *outboundMocks) expectNoDisassociation() {
	m.sgw.EXPECT().GetServices(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*armnetwork.ServiceGatewayService{}, nil).AnyTimes()
	m.nat.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil, notFoundError()).AnyTimes()
}

func newOutboundDiffTracker(uid string, m *outboundMocks, kube *fake.Clientset) *DiffTracker {
	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.networkClientFactory = m.factory
	if kube != nil {
		dt.kubeClient = kube
	}
	dt.NRPResources.NATGateways = utilsets.NewString(uid)
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid, Config: NewOutboundServiceConfig(uid, nil), State: StateDeletionInProgress,
	}
	return dt
}

// outboundCompletion records the completion callback. It is mutex-guarded because processBatch can
// dispatch several operations concurrently, so more than one worker may report into it.
type outboundCompletion struct {
	mu      sync.Mutex
	called  bool
	success bool
	err     error
}

func (c *outboundCompletion) record(success bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.called, c.success, c.err = true, success, err
}

func (c *outboundCompletion) result() (called, success bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.called, c.success, c.err
}

func outboundUpdater(dt *DiffTracker, got *outboundCompletion) *ServiceUpdater {
	return &ServiceUpdater{
		diffTracker: dt,
		onComplete: func(_ string, success bool, err error) {
			got.record(success, err)
		},
		trigger:   dt.serviceUpdaterTrigger,
		ctx:       context.Background(),
		semaphore: make(chan struct{}, 10),
		activeOps: make(map[string]bool),
	}
}

// TestServiceUpdaterDeleteOutboundService_HappyPath asserts the exact Azure teardown sequence and
// that NRP state is only cleared once every step succeeded.
// TestGuardDeleteOutboundService_RemovesLastPodFinalizerOnlyAfterNATGatewayDeletion pins the
// ordering that the whole egress teardown design rests on: the last pod's cleanup finalizer must
// not be released until the NAT Gateway is actually gone from Azure.
//
// Releasing it first hands the pod (and its IP) back to Kubernetes while NRP still routes that IP
// through a live NAT Gateway, which is exactly the stranding the finalizer exists to prevent.
//
// Every other last-pod test hand-calls RemoveLastPodFinalizers, so none of them observes the
// ordering; this drives the real deleteOutboundService and asserts, from inside the NAT delete
// call itself, that the finalizer is still attached at that moment.
func TestGuardDeleteOutboundService_RemovesLastPodFinalizerOnlyAfterNATGatewayDeletion(t *testing.T) {
	const (
		uid     = "egress-ordering"
		podNS   = "default"
		podName = "egress-last-pod"
		podUID  = "pod-uid-1"
		podKey  = podNS + "/" + podName
	)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       podName,
			Namespace:  podNS,
			UID:        types.UID(podUID),
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
	}
	kube := fake.NewSimpleClientset(pod)

	m := newOutboundMocks(ctrl)
	m.expectNoDisassociation()
	m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).Times(1)

	// Captured at the instant the NAT Gateway delete runs.
	var finalizerHeldDuringNATDelete bool
	m.nat.EXPECT().Delete(gomock.Any(), "rg", uid).DoAndReturn(func(ctx context.Context, _, _ string) error {
		live, err := kube.CoreV1().Pods(podNS).Get(ctx, podName, metav1.GetOptions{})
		if err == nil {
			finalizerHeldDuringNATDelete = hasPodFinalizer(live)
		}
		return nil
	}).Times(1)

	m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName(uid)).Return(nil).Times(1)
	m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPNameV6(uid)).Return(nil).Times(1)

	dt := newOutboundDiffTracker(uid, m, kube)
	dt.pendingPodDeletions[podKey] = &PendingPodDeletion{
		Namespace:  podNS,
		Name:       podName,
		UID:        podUID,
		ServiceUID: uid,
		Addresses:  []string{"10.244.0.5"},
		IsLastPod:  true,
	}

	got := &outboundCompletion{}
	outboundUpdater(dt, got).deleteOutboundService(uid, "corr")

	called, success, completionErr := got.result()
	assert.True(t, called)
	assert.True(t, success, "a fully successful teardown must report success: %v", completionErr)

	assert.True(t, finalizerHeldDuringNATDelete,
		"the last pod must still carry its cleanup finalizer while the NAT Gateway is being deleted")

	live, err := kube.CoreV1().Pods(podNS).Get(context.Background(), podName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.False(t, hasPodFinalizer(live),
		"the last pod's finalizer must be released once the NAT Gateway is gone")
}

// TestGuardDeleteOutboundService_FinalizerRemovalFailureReportsFailure pins the other half of the
// contract: if the finalizer sweep cannot complete, the delete must be reported as failed so it is
// retried (the NAT/PIP deletes are idempotent on 404). Reporting success would drop the operation
// while the pod stays stuck Terminating forever.
func TestGuardDeleteOutboundService_FinalizerRemovalFailureReportsFailure(t *testing.T) {
	const (
		uid     = "egress-ordering-fail"
		podNS   = "default"
		podName = "egress-last-pod"
		podUID  = "pod-uid-2"
	)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	kube := fake.NewSimpleClientset(&v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       podName,
			Namespace:  podNS,
			UID:        types.UID(podUID),
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
	})
	// Every attempt to strip the finalizer fails, exhausting the retry budget.
	kube.PrependReactor("update", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver down")
	})

	m := newOutboundMocks(ctrl)
	m.expectNoDisassociation()
	m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).Times(1)
	m.nat.EXPECT().Delete(gomock.Any(), "rg", uid).Return(nil).Times(1)
	m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName(uid)).Return(nil).Times(1)
	m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPNameV6(uid)).Return(nil).Times(1)

	dt := newOutboundDiffTracker(uid, m, kube)
	dt.pendingPodDeletions[podNS+"/"+podName] = &PendingPodDeletion{
		Namespace:  podNS,
		Name:       podName,
		UID:        podUID,
		ServiceUID: uid,
		Addresses:  []string{"10.244.0.5"},
		IsLastPod:  true,
	}

	got := &outboundCompletion{}
	outboundUpdater(dt, got).deleteOutboundService(uid, "corr")

	called, success, completionErr := got.result()
	assert.True(t, called)
	assert.False(t, success,
		"a delete whose last-pod finalizer sweep failed must be reported as failed so it retries")
	assert.Error(t, completionErr)
}

func TestServiceUpdaterDeleteOutboundService_HappyPath(t *testing.T) {
	const uid = "egress-a"
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	m := newOutboundMocks(ctrl)
	m.expectNoDisassociation()
	m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).Times(1)
	m.nat.EXPECT().Delete(gomock.Any(), "rg", uid).Return(nil).Times(1)
	m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName(uid)).Return(nil).Times(1)
	m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPNameV6(uid)).Return(nil).Times(1)

	dt := newOutboundDiffTracker(uid, m, fake.NewSimpleClientset())
	got := &outboundCompletion{}
	outboundUpdater(dt, got).deleteOutboundService(uid, "corr")

	called, success, completionErr := got.result()
	assert.True(t, called)
	assert.True(t, success, "a fully successful teardown must report success: %v", completionErr)
	assert.False(t, dt.NRPResources.NATGateways.Has(uid),
		"NRP NAT Gateway state must be cleared after a successful delete")
}

// TestServiceUpdaterDeleteOutboundService_StepFailuresRetain covers every failing Azure step. Each
// must report failure so the operation is retried, and must NOT clear the NRP entry - clearing it
// would make the retried delete a no-op and leak the Azure resource while the pod finalizer stays.
func TestServiceUpdaterDeleteOutboundService_StepFailuresRetain(t *testing.T) {
	const uid = "egress-b"
	boom := errors.New("ARM failure")

	for _, tc := range []struct {
		name                       string
		unregister, natDel, pipDel error
	}{
		{name: "ServiceGateway unregister fails", unregister: boom},
		{name: "NAT Gateway delete fails", natDel: boom},
		{name: "Public IP delete fails", pipDel: boom},
		{name: "every step fails", unregister: boom, natDel: boom, pipDel: boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			m := newOutboundMocks(ctrl)
			m.expectNoDisassociation()
			m.sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
				Return(tc.unregister).AnyTimes()
			m.nat.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.natDel).AnyTimes()
			m.pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.pipDel).AnyTimes()

			dt := newOutboundDiffTracker(uid, m, fake.NewSimpleClientset())
			got := &outboundCompletion{}
			outboundUpdater(dt, got).deleteOutboundService(uid, "corr")

			called, success, completionErr := got.result()
			assert.True(t, called)
			assert.False(t, success, "a failed teardown step must report failure so it is retried")
			assert.Error(t, completionErr)
			assert.True(t, dt.NRPResources.NATGateways.Has(uid),
				"NRP state must be retained on failure, otherwise the retry is a no-op and Azure leaks")
		})
	}
}

// TestServiceUpdaterDeleteOutboundService_ToleratesAlreadyDeleted asserts crash-after-delete
// convergence: an already-absent NAT Gateway and Public IP are a successful teardown, not a
// permanent failure that would strand the egress pod finalizer.
func TestServiceUpdaterDeleteOutboundService_ToleratesAlreadyDeleted(t *testing.T) {
	const uid = "egress-c"
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	m := newOutboundMocks(ctrl)
	m.expectNoDisassociation()
	m.sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&azcore.ResponseError{StatusCode: http.StatusNotFound}).AnyTimes()
	m.nat.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(notFoundError()).AnyTimes()
	m.pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(notFoundError()).AnyTimes()

	dt := newOutboundDiffTracker(uid, m, fake.NewSimpleClientset())
	got := &outboundCompletion{}
	outboundUpdater(dt, got).deleteOutboundService(uid, "corr")

	called, success, completionErr := got.result()
	assert.True(t, called)
	assert.True(t, success, "404 on every resource means the teardown is already complete: %v", completionErr)
	assert.False(t, dt.NRPResources.NATGateways.Has(uid))
}

// TestServiceUpdaterCreateOutboundService_HappyPath asserts the provisioning order (PIP, then NAT
// Gateway, then ServiceGateway registration) and that NRP state is recorded only on success.
func TestServiceUpdaterCreateOutboundService_HappyPath(t *testing.T) {
	const uid = "egress-d"
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	m := newOutboundMocks(ctrl)
	pipCall := m.pip.EXPECT().CreateOrUpdate(gomock.Any(), "rg", PublicIPName(uid), gomock.Any()).
		Return(&armnetwork.PublicIPAddress{Name: ptr.To(PublicIPName(uid))}, nil).Times(1)
	natCall := m.nat.EXPECT().CreateOrUpdate(gomock.Any(), "rg", uid, gomock.Any()).
		Return(nil, nil).Times(1).After(pipCall)
	m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).
		Return(nil).Times(1).After(natCall)

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.networkClientFactory = m.factory
	got := &outboundCompletion{}
	outboundUpdater(dt, got).createOutboundService(uid, &OutboundConfig{}, "corr", "ns", "pod")

	called, success, completionErr := got.result()
	assert.True(t, called)
	assert.True(t, success, "a fully successful create must report success: %v", completionErr)
	assert.True(t, dt.NRPResources.NATGateways.Has(uid),
		"NRP NAT Gateway state must be recorded after a successful create")
}

// TestServiceUpdaterCreateOutboundService_StepFailuresDoNotRecordNRPState covers each failing
// provisioning step. Recording NRP state after a partial create would make the tracker believe NRP
// holds a service it does not, and the diff would never re-create it.
func TestServiceUpdaterCreateOutboundService_StepFailuresDoNotRecordNRPState(t *testing.T) {
	const uid = "egress-e"
	boom := errors.New("ARM failure")

	for _, tc := range []struct {
		name                   string
		pipErr, natErr, sgwErr error
	}{
		{name: "Public IP create fails", pipErr: boom},
		{name: "NAT Gateway create fails", natErr: boom},
		{name: "ServiceGateway registration fails", sgwErr: boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			m := newOutboundMocks(ctrl)
			m.pip.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
				Return(&armnetwork.PublicIPAddress{Name: ptr.To(PublicIPName(uid))}, tc.pipErr).AnyTimes()
			m.nat.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
				Return(nil, tc.natErr).AnyTimes()
			m.sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
				Return(tc.sgwErr).AnyTimes()

			dt := newTestDiffTracker()
			dt.config = testConfig()
			dt.networkClientFactory = m.factory
			got := &outboundCompletion{}
			outboundUpdater(dt, got).createOutboundService(uid, &OutboundConfig{}, "corr", "ns", "pod")

			called, success, completionErr := got.result()
			assert.True(t, called)
			assert.False(t, success, "a failed create step must report failure so it is retried")
			assert.Error(t, completionErr)
			assert.False(t, dt.NRPResources.NATGateways.Has(uid),
				"a partially created outbound service must not be recorded as present in NRP")
		})
	}
}

// TestServiceUpdaterUpdateInboundService covers the path that applies a spec change to a live
// LoadBalancer. It re-PUTs only the LoadBalancer: the Public IP allocation is independent of the
// rules, and the ServiceGateway registration references the backend pool by an ID that is stable
// across port edits.
func TestServiceUpdaterUpdateInboundService(t *testing.T) {
	const uid = "11111111-1111-1111-1111-111111111111"

	validConfig := func() *InboundConfig {
		return &InboundConfig{
			FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
			BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
		}
	}

	t.Run("re-PUTs the LoadBalancer and reports success", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		m := newOutboundMocks(ctrl)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		mockLB.EXPECT().CreateOrUpdate(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, nil).Times(1)
		// A port-only update must not touch the Public IP or re-register with the ServiceGateway.
		m.pip.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
		m.sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

		dt := newTestDiffTracker()
		dt.config = testConfig()
		dt.networkClientFactory = m.factory
		got := &outboundCompletion{}
		outboundUpdater(dt, got).updateInboundService(uid, validConfig(), "corr")

		called, success, completionErr := got.result()
		assert.True(t, called)
		assert.True(t, success, "a successful LoadBalancer update must report success: %v", completionErr)
	})

	t.Run("transient ARM failure is retryable, not terminal", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		m := newOutboundMocks(ctrl)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		mockLB.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil, errors.New("boom")).AnyTimes()

		dt := newTestDiffTracker()
		dt.config = testConfig()
		dt.networkClientFactory = m.factory
		got := &outboundCompletion{}
		outboundUpdater(dt, got).updateInboundService(uid, validConfig(), "corr")

		called, success, completionErr := got.result()
		assert.True(t, called)
		assert.False(t, success)
		assert.False(t, isTerminalError(completionErr),
			"an ARM failure must stay retryable so the update is re-attempted")
	})

	t.Run("unsupported spec parks instead of retrying forever", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		m := newOutboundMocks(ctrl)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		// The build fails first, so the LoadBalancer must never be PUT with an unsupported spec.
		mockLB.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

		dualStack := validConfig()
		dualStack.IPFamilies = []string{"IPv4", "IPv6"}

		dt := newTestDiffTracker()
		dt.config = testConfig()
		dt.networkClientFactory = m.factory
		got := &outboundCompletion{}
		outboundUpdater(dt, got).updateInboundService(uid, dualStack, "corr")

		called, success, completionErr := got.result()
		assert.True(t, called)
		assert.False(t, success)
		assert.True(t, isTerminalError(completionErr),
			"a deterministic spec failure must be terminal so the engine parks instead of looping")
	})
}

// TestServiceUpdaterUpdateInboundService_PortRemovalDropsOnlyThatRule pins the ARM payload for a
// port removal, which is the only way a Service can lose a load-balancing rule: Kubernetes rejects
// a type=LoadBalancer Service with an empty spec.ports, so a Service can never transition to
// "no ports at all" and the rule set can only shrink to a smaller non-empty set.
//
// updateInboundService rebuilds the LoadBalancer from the new config and PUTs it whole, so the
// rules it sends are the complete desired set and the removed port's rule disappears by omission.
// The sibling subtests above assert only that CreateOrUpdate happened, with gomock.Any() for the
// body; that still passes if the stale rule is carried into the payload, which would leave the
// removed port reachable on the frontend IP. This asserts the body itself.
func TestServiceUpdaterUpdateInboundService_PortRemovalDropsOnlyThatRule(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const uid = "22222222-2222-2222-2222-222222222222"

	m := newOutboundMocks(ctrl)
	mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
	m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()

	var puts []armnetwork.LoadBalancer
	mockLB.EXPECT().CreateOrUpdate(gomock.Any(), "rg", uid, gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, lb armnetwork.LoadBalancer) (*armnetwork.LoadBalancer, error) {
			puts = append(puts, lb)
			return nil, nil
		}).Times(1)

	// Dropping a port changes only the rule list. Re-allocating the Public IP would move the
	// Service's external address, and re-registering with the ServiceGateway is unnecessary
	// because the backend pool ID the registration points at does not change.
	m.pip.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	m.sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.networkClientFactory = m.factory

	// The Service previously published 80->8080 and 443->8443; 443 has been removed from spec.ports.
	remaining := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}

	got := &outboundCompletion{}
	outboundUpdater(dt, got).updateInboundService(uid, remaining, "corr-port-removal")

	called, success, completionErr := got.result()
	assert.True(t, called)
	assert.True(t, success, "removing a port must report success: %v", completionErr)

	if assert.Len(t, puts, 1, "the update must PUT the LoadBalancer exactly once") &&
		assert.NotNil(t, puts[0].Properties) {
		rules := puts[0].Properties.LoadBalancingRules
		if assert.Len(t, rules, 1, "the PUT must carry only the surviving rule, so the removed one is deleted by omission") {
			assert.Equal(t, "rule-tcp-80", *rules[0].Name)
			assert.Equal(t, int32(80), *rules[0].Properties.FrontendPort)
			assert.Equal(t, int32(8080), *rules[0].Properties.BackendPort)
		}
		for _, rule := range rules {
			assert.NotEqual(t, "rule-tcp-443", *rule.Name,
				"the removed port's rule must not survive in the payload sent to Azure")
		}
		// The frontend and backend pool are the anchors the surviving rule and the ServiceGateway
		// registration reference; a port edit that dropped either would tear down the data path.
		assert.Len(t, puts[0].Properties.FrontendIPConfigurations, 1,
			"a port removal must keep the frontend IP configuration")
		assert.Len(t, puts[0].Properties.BackendAddressPools, 1,
			"a port removal must keep the backend address pool")
	}
}

// TestServiceUpdaterOutboundUpdateIsCountedAsSkipped pins that an outbound update dispatched by
// processBatch is counted as skipped. The updater has no way to apply it, so the requested spec
// change is silently dropped and the service keeps its existing Azure configuration; this counter
// is the only signal an operator gets that the change was not applied.
func TestServiceUpdaterOutboundUpdateIsCountedAsSkipped(t *testing.T) {
	RegisterMetrics()

	dt := newTestDiffTracker()
	uid := "outbound-update"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewOutboundServiceConfig(uid, &OutboundConfig{}),
		State:      StateUpdateInProgress,
	}

	before, err := testutil.GetCounterMetricValue(outboundServiceUpdatesSkippedTotal)
	assert.NoError(t, err)

	got := &outboundCompletion{}
	updater := outboundUpdater(dt, got)
	updater.processBatch()
	updater.wg.Wait()

	called, success, opErr := got.result()
	assert.True(t, called, "the operation must be completed so the state machine does not strand")
	assert.True(t, success, "the completion is reported as success to release the operation")
	assert.NoError(t, opErr)

	after, err := testutil.GetCounterMetricValue(outboundServiceUpdatesSkippedTotal)
	assert.NoError(t, err)
	assert.Equal(t, 1.0, after-before, "a dropped outbound update must be counted exactly once")
}

// newRetryTimerTestUpdater builds an updater whose fired timer is observable as a buffered token.
func newRetryTimerTestUpdater(t *testing.T) (*ServiceUpdater, *DiffTracker) {
	t.Helper()
	dt := newTestDiffTracker()
	ctx, cancel := context.WithCancel(context.Background())
	s := &ServiceUpdater{
		diffTracker: dt,
		ctx:         ctx,
		cancel:      cancel,
		activeOps:   make(map[string]bool),
		retryTimers: make(map[string]*time.Timer),
		logger:      dt.logger,
	}
	return s, dt
}

// TestServiceUpdaterStop_CancelsPendingRetryTimers pins that a parked operation's self-arm cannot
// outlive the updater. Those timers are not tracked by wg nor bound to ctx, so unless Stop cancels
// them each keeps the whole DiffTracker reachable and then fires into a stopped updater. The
// still-running case is the control: the timer is the only thing that re-arms a parked service.
func TestServiceUpdaterStop_CancelsPendingRetryTimers(t *testing.T) {
	stopped, stoppedDT := newRetryTimerTestUpdater(t)
	stopped.scheduleRetry("svc", 60*time.Millisecond)
	stopped.Stop()

	time.Sleep(200 * time.Millisecond)
	assert.Empty(t, stoppedDT.serviceUpdaterTrigger,
		"a retry timer must not push a trigger token into a stopped updater")
	assert.Empty(t, len(stopped.retryTimers), "Stop must release the retained timers")

	// Control: an updater that is still running must still be re-armed by the same mechanism.
	running, runningDT := newRetryTimerTestUpdater(t)
	defer running.Stop()
	running.scheduleRetry("svc", 60*time.Millisecond)

	assert.Eventually(t, func() bool {
		return len(runningDT.serviceUpdaterTrigger) > 0
	}, 2*time.Second, 20*time.Millisecond,
		"control: the self-arm is the only driver for a parked service and must still fire")
}

// TestServiceUpdaterScheduleRetry_ReplacesPendingTimer pins that repeated backoff passes for the
// same service do not accumulate timers, each holding the DiffTracker until it fires.
func TestServiceUpdaterScheduleRetry_ReplacesPendingTimer(t *testing.T) {
	s, _ := newRetryTimerTestUpdater(t)
	defer s.Stop()

	for i := 0; i < 5; i++ {
		s.scheduleRetry("svc", time.Hour)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	assert.Len(t, s.retryTimers, 1, "re-arming the same service must replace its pending revisit, not add one")
}

// TestGuardDeleteOutboundService_NATDeleteFailureRetainsLastPodFinalizer pins the failure half of
// the drain-before-release ordering.
//
// The happy path is guarded by RemovesLastPodFinalizerOnlyAfterNATGatewayDeletion, but nothing
// covered the case where the NAT Gateway delete FAILS. The finalizer must stay attached: releasing
// it lets the pod disappear while its NAT Gateway is still live in Azure, which is exactly the
// traffic-blackhole the finalizer exists to prevent. Structurally the sweep sits inside the
// lastErr == nil branch, but without this test that guard can be deleted with no signal.
func TestGuardDeleteOutboundService_NATDeleteFailureRetainsLastPodFinalizer(t *testing.T) {
	const (
		uid     = "egress-nat-fail"
		podNS   = "default"
		podName = "egress-last-pod"
		podUID  = "pod-uid-nat-fail"
	)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       podName,
			Namespace:  podNS,
			UID:        types.UID(podUID),
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
	}
	kube := fake.NewSimpleClientset(pod)

	m := newOutboundMocks(ctrl)
	m.expectNoDisassociation()
	m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).AnyTimes()
	// The NAT Gateway teardown fails, so the Azure resource is still live afterwards.
	m.nat.EXPECT().Delete(gomock.Any(), "rg", uid).Return(errors.New("nat gateway delete failed")).Times(1)
	m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName(uid)).Return(nil).AnyTimes()
	m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPNameV6(uid)).Return(nil).AnyTimes()

	dt := newOutboundDiffTracker(uid, m, kube)
	dt.pendingPodDeletions[podNS+"/"+podName] = &PendingPodDeletion{
		Namespace:  podNS,
		Name:       podName,
		UID:        podUID,
		ServiceUID: uid,
		Addresses:  []string{"10.244.0.5"},
		IsLastPod:  true,
	}

	got := &outboundCompletion{}
	outboundUpdater(dt, got).deleteOutboundService(uid, "corr")

	called, success, _ := got.result()
	assert.True(t, called)
	assert.False(t, success, "a delete whose NAT Gateway teardown failed must be reported as failed")

	live, err := kube.CoreV1().Pods(podNS).Get(context.Background(), podName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.True(t, hasPodFinalizer(live),
		"the last pod must keep its cleanup finalizer while the NAT Gateway is still live in Azure")

	_, stillPending := dt.pendingPodDeletions[podNS+"/"+podName]
	assert.True(t, stillPending,
		"the last-pod entry must survive so the retried delete can release the finalizer")
}

// TestGuardDeleteOutboundService_FinalizerReleasedOnRetryAfterNATFailure pins the other direction of
// the same invariant: the pod must not be stranded Terminating forever.
//
// Holding the finalizer through a failed teardown is only correct if a later successful attempt
// actually releases it. Without this, a fix that permanently held the finalizer on any prior failure
// would look correct to the test above while blocking node drain and namespace deletion indefinitely.
func TestGuardDeleteOutboundService_FinalizerReleasedOnRetryAfterNATFailure(t *testing.T) {
	const (
		uid     = "egress-nat-retry"
		podNS   = "default"
		podName = "egress-last-pod"
		podUID  = "pod-uid-nat-retry"
	)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	kube := fake.NewSimpleClientset(&v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       podName,
			Namespace:  podNS,
			UID:        types.UID(podUID),
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
	})

	m := newOutboundMocks(ctrl)
	m.expectNoDisassociation()
	m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).AnyTimes()
	// First attempt fails, second succeeds.
	gomock.InOrder(
		m.nat.EXPECT().Delete(gomock.Any(), "rg", uid).Return(errors.New("nat gateway delete failed")).Times(1),
		m.nat.EXPECT().Delete(gomock.Any(), "rg", uid).Return(nil).Times(1),
	)
	m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName(uid)).Return(nil).AnyTimes()
	m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPNameV6(uid)).Return(nil).AnyTimes()

	dt := newOutboundDiffTracker(uid, m, kube)
	entry := &PendingPodDeletion{
		Namespace:  podNS,
		Name:       podName,
		UID:        podUID,
		ServiceUID: uid,
		Addresses:  []string{"10.244.0.5"},
		IsLastPod:  true,
	}
	dt.pendingPodDeletions[podNS+"/"+podName] = entry

	updater := outboundUpdater(dt, &outboundCompletion{})
	updater.deleteOutboundService(uid, "corr-1")

	live, err := kube.CoreV1().Pods(podNS).Get(context.Background(), podName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.True(t, hasPodFinalizer(live), "precondition: the failed attempt must retain the finalizer")

	got := &outboundCompletion{}
	outboundUpdater(dt, got).deleteOutboundService(uid, "corr-2")

	_, success, completionErr := got.result()
	assert.True(t, success, "the retried delete must succeed: %v", completionErr)

	live, err = kube.CoreV1().Pods(podNS).Get(context.Background(), podName, metav1.GetOptions{})
	assert.NoError(t, err)
	assert.False(t, hasPodFinalizer(live),
		"a successful retry must release the finalizer, or the pod stays Terminating forever")
}

// TestRefreshOperationGauges_ReflectsPodDrivenAndAbortedOperations pins that the periodic recompute
// converges on the truth for operations the four event-driven refresh points never see.
//
// Outbound services are created only by pod events, so a gauge refreshed solely from
// AddService/UpdateService/DeleteService/OnServiceCreationComplete never counts the egress fleet.
// The abort path is the mirror: it untracks the operation, so a stale count has nothing left to
// bring it back down and pages forever on an idle cluster.
func TestRefreshOperationGauges_ReflectsPodDrivenAndAbortedOperations(t *testing.T) {
	RegisterMetrics()
	pendingServiceOperations.Reset()

	s, dt := newRetryTimerTestUpdater(t)
	defer s.Stop()

	// A pod-driven outbound operation: created by AddPod, which does not refresh the gauges.
	dt.AddPodWithUID("team-egress", "team/pod", "pod-uid", "10.0.0.1", "10.244.0.5")

	dt.mu.Lock()
	tracked := len(dt.pendingServiceOps)
	dt.mu.Unlock()
	assert.Equal(t, 1, tracked, "the pod must have created an outbound operation")

	s.refreshOperationGauges()
	after, err := testutil.GetGaugeMetricValue(pendingServiceOperations.WithLabelValues("not_started", "outbound"))
	assert.NoError(t, err)
	assert.Equal(t, float64(1), after, "the recompute must count a pod-driven outbound operation")

	// Aborting untracks it; the recompute must drop the count rather than leave a phantom.
	dt.mu.Lock()
	delete(dt.pendingServiceOps, "team-egress")
	dt.mu.Unlock()

	s.refreshOperationGauges()
	cleared, err := testutil.GetGaugeMetricValue(pendingServiceOperations.WithLabelValues("not_started", "outbound"))
	assert.NoError(t, err)
	assert.Zero(t, cleared, "an untracked operation must not leave a phantom count")
}

// TestCreateInboundService_StatusPatchFailureKeepsLoadBalancerLive pins that a failed Kubernetes
// status write does not un-provision a service Azure and NRP have already accepted.
//
// The status write touches only Kubernetes, so failing it leaves the PIP, the LoadBalancer and the
// ServiceGateway registration in place. Recording the registration only after that write would
// demote the operation to StateNotStarted with no LoadBalancer tracked, and isServiceReadyToSync
// then withholds the service's endpoints - leaving a public IP with no backends until the patch
// eventually succeeds. The successful write is the control.
func TestCreateInboundService_StatusPatchFailureKeepsLoadBalancerLive(t *testing.T) {
	const uid = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

	run := func(t *testing.T, statusPatchFails bool) *DiffTracker {
		t.Helper()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: types.UID(uid)},
			Spec: v1.ServiceSpec{
				Type:  v1.ServiceTypeLoadBalancer,
				Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}},
			},
		}
		kube := fake.NewSimpleClientset(svc)
		if statusPatchFails {
			// Only the status patch fails; the finalizer add earlier in the create uses update and
			// must still succeed so the run reaches the ServiceGateway registration.
			kube.PrependReactor("patch", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("apiserver unavailable")
			})
		}

		m := newOutboundMocks(ctrl)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()

		// PIP and LoadBalancer create succeed, and the ServiceGateway registration succeeds, so the
		// service is genuinely live in Azure before the status write is attempted.
		m.pip.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&armnetwork.PublicIPAddress{
				Name:       ptr.To(PublicIPName(uid)),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{IPAddress: ptr.To("20.30.40.50")},
			}, nil).AnyTimes()
		mockLB.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(&armnetwork.LoadBalancer{Name: ptr.To(uid)}, nil).AnyTimes()
		m.sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			Return(nil).AnyTimes()

		dt := newTestDiffTracker()
		dt.config = testConfig()
		dt.networkClientFactory = m.factory
		dt.kubeClient = kube

		s := NewServiceUpdater(context.Background(), dt, func(string, bool, error) {}, dt.serviceUpdaterTrigger)
		s.createInboundService(uid, makeInboundConfig(80), "corr")
		return dt
	}

	failed := run(t, true)
	failed.mu.Lock()
	liveAfterFailure := failed.NRPResources.LoadBalancers.Has(uid)
	readyAfterFailure := failed.isServiceReadyToSync(uid, true)
	failed.mu.Unlock()
	assert.True(t, liveAfterFailure,
		"the LoadBalancer is registered in NRP, so a failed Kubernetes status write must not un-track it")
	assert.True(t, readyAfterFailure,
		"its endpoints must still publish, or the public IP serves nothing")

	ok := run(t, false)
	ok.mu.Lock()
	liveAfterSuccess := ok.NRPResources.LoadBalancers.Has(uid)
	ok.mu.Unlock()
	assert.True(t, liveAfterSuccess, "control: a successful create tracks the LoadBalancer")
}

// TestDeleteOutboundService_CountsSwallowedDisassociationFailure pins that a delete sub-step which
// fails and is continued past is still counted.
//
// The deletion proceeds deliberately, because the later steps free the resources, and it is recorded
// as a successful delete. Logging that at V(4) left a sustained NRP failure invisible in both metrics
// and production logs while every deleted service kept a stale ServiceGateway association. The
// successful disassociation is the control.
func TestDeleteOutboundService_CountsSwallowedDisassociationFailure(t *testing.T) {
	const uid = "egress-uid"

	run := func(t *testing.T, disassociationFails bool) float64 {
		t.Helper()
		RegisterMetrics()
		deleteSubstepFailuresTotal.Reset()

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		m := newOutboundMocks(ctrl)

		if disassociationFails {
			m.sgw.EXPECT().GetServices(gomock.Any(), gomock.Any(), gomock.Any()).
				Return(nil, errors.New("NRP unavailable")).AnyTimes()
		} else {
			m.expectNoDisassociation()
		}
		m.sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		m.nat.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		m.pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		dt := newOutboundDiffTracker(uid, m, fake.NewSimpleClientset())
		s := NewServiceUpdater(context.Background(), dt, func(string, bool, error) {}, dt.serviceUpdaterTrigger)
		s.deleteOutboundService(uid, "corr")

		got, err := testutil.GetCounterMetricValue(deleteSubstepFailuresTotal.WithLabelValues(deleteStepDisassociateNAT))
		assert.NoError(t, err)
		return got
	}

	assert.Equal(t, float64(1), run(t, true),
		"a swallowed disassociation failure must be counted, or a sustained NRP failure is invisible")
	assert.Zero(t, run(t, false), "control: a successful disassociation counts nothing")
}
