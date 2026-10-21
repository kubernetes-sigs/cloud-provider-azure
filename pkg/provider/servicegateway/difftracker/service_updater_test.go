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
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	servicehelper "k8s.io/cloud-provider/service/helpers"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/natgatewayclient/mock_natgatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
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
	m.pip.EXPECT().Get(gomock.Any(), "rg", gomock.Any(), nil).Return(nil, notFoundError()).AnyTimes()
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

	mockPIP.EXPECT().Get(gomock.Any(), "rg", "uid-status-pip", nil).Return(nil, notFoundError()).Times(1)
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

	mockPIP.EXPECT().Get(gomock.Any(), "rg", "uid-status-ok-pip", nil).Return(nil, notFoundError()).Times(1)
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

func (m *outboundMocks) expectOwnedPublicIPs(identity string, calls int) {
	for _, name := range OutboundPublicIPNames(identity) {
		m.pip.EXPECT().Get(gomock.Any(), "rg", name, nil).
			Return(&armnetwork.PublicIPAddress{Name: ptr.To(name), Tags: egressIdentityTags(identity)}, nil).Times(calls)
	}
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

	m.expectOwnedPublicIPs(uid, 1)
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
	m.expectOwnedPublicIPs(uid, 1)
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
	m.expectOwnedPublicIPs(uid, 1)
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
			m.expectOwnedPublicIPs(uid, 1)
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
	for _, name := range OutboundPublicIPNames(uid) {
		m.pip.EXPECT().Get(gomock.Any(), "rg", name, nil).Return(nil, notFoundError()).Times(1)
	}
	m.pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	dt := newOutboundDiffTracker(uid, m, fake.NewSimpleClientset())
	got := &outboundCompletion{}
	outboundUpdater(dt, got).deleteOutboundService(uid, "corr")

	called, success, completionErr := got.result()
	assert.True(t, called)
	assert.True(t, success, "404 on every resource means the teardown is already complete: %v", completionErr)
	assert.False(t, dt.NRPResources.NATGateways.Has(uid))
}

func TestServiceUpdaterDeleteOutboundService_RejectedBYOKeepsUnownedPublicIPs(t *testing.T) {
	const uid = "payments"
	for _, tc := range []struct {
		name    string
		linkErr error
		tags    map[string]*string
	}{
		{name: "missing gateway and untagged Public IPs"},
		{name: "missing gateway and inbound-owned Public IPs",
			tags: map[string]*string{InboundServiceUIDTagKey: ptr.To(uid)}},
		{name: "refused link and another identity's Public IPs",
			linkErr: &azcore.ResponseError{StatusCode: http.StatusForbidden},
			tags:    egressIdentityTags("someone-else")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)
			if tc.linkErr == nil {
				m.nat.EXPECT().Get(gomock.Any(), byoNATGatewayRG, uid, gomock.Any()).Return(nil, notFoundError()).Times(1)
			} else {
				m.nat.EXPECT().Get(gomock.Any(), byoNATGatewayRG, uid, gomock.Any()).Return(byoNATGateway(uid), nil).Times(1)
				m.nat.EXPECT().CreateOrUpdate(gomock.Any(), byoNATGatewayRG, uid, gomock.Any()).Return(nil, initialNATPutError(tc.linkErr)).Times(1)
			}
			m.expectNoDisassociation()
			m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).Times(1)
			m.nat.EXPECT().Delete(gomock.Any(), "rg", uid).Return(notFoundError()).Times(1)
			for _, name := range OutboundPublicIPNames(uid) {
				m.pip.EXPECT().Get(gomock.Any(), "rg", name, gomock.Any()).
					Return(&armnetwork.PublicIPAddress{Name: ptr.To(name), Tags: tc.tags}, nil).Times(1)
			}
			m.pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

			dt := newTestDiffTracker()
			dt.config = testConfig()
			dt.networkClientFactory = m.factory
			dt.kubeClient = fake.NewSimpleClientset(byoPod(uid, byoNATGatewayARMID(uid)))
			created := &outboundCompletion{}
			outboundUpdater(dt, created).createOutboundService(uid, &OutboundConfig{}, "corr", "ns", "pod")
			called, success, err := created.result()
			assert.True(t, called)
			assert.False(t, success)
			assert.True(t, isEgressRejected(err), "%v", err)
			assert.Empty(t, dt.byoNATGatewayID(uid))

			deleted := &outboundCompletion{}
			outboundUpdater(dt, deleted).deleteOutboundService(uid, "corr")
			called, success, err = deleted.result()
			assert.True(t, called)
			assert.True(t, success, "%v", err)
		})
	}
}

// TestServiceUpdaterCreateOutboundService_HappyPath asserts the provisioning order (PIP, then NAT
// Gateway, then ServiceGateway registration) and that NRP state is recorded only on success.
func TestServiceUpdaterCreateOutboundService_HappyPath(t *testing.T) {
	const uid = "egress-d"
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	m := newOutboundMocks(ctrl)
	m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, notFoundError()).Times(1)
	m.pip.EXPECT().Get(gomock.Any(), "rg", PublicIPName(uid), nil).Return(nil, notFoundError()).Times(1)
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
			m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, notFoundError()).Times(1)
			m.pip.EXPECT().Get(gomock.Any(), "rg", PublicIPName(uid), nil).Return(nil, notFoundError()).Times(1)
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
	m.expectOwnedPublicIPs(uid, 1)
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
	m.expectOwnedPublicIPs(uid, 2)
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
		m.pip.EXPECT().Get(gomock.Any(), "rg", PublicIPName(uid), nil).Return(nil, notFoundError()).Times(1)
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
			m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, notFoundError()).AnyTimes()
		} else {
			m.expectNoDisassociation()
		}
		m.sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		m.nat.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		m.expectOwnedPublicIPs(uid, 1)
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

// byoNATGatewayRG is the resource group of the BYO NAT Gateways in these tests: any resource group of
// the cluster's subscription but the cluster resource group ("rg").
const byoNATGatewayRG = "byo-rg"

func byoNATGatewayARMID(name string) string {
	return "/subscriptions/sub/resourceGroups/" + byoNATGatewayRG + "/providers/Microsoft.Network/natGateways/" + name
}

// wantedCalls is the number of calls a test expects: 1 when the call is wanted, else 0.
func wantedCalls(wanted bool) int {
	if wanted {
		return 1
	}
	return 0
}

func byoNATGateway(name string) *armnetwork.NatGateway {
	return &armnetwork.NatGateway{
		ID:       ptr.To(byoNATGatewayARMID(name)),
		Name:     ptr.To(name),
		Etag:     ptr.To(`W/"byo-etag"`),
		SKU:      &armnetwork.NatGatewaySKU{Name: ptr.To(armnetwork.NatGatewaySKUNameStandardV2)},
		Location: ptr.To("eastus"),
		Tags:     map[string]*string{"owner": ptr.To("team-a")},
		Zones:    []*string{ptr.To("1")},
		Properties: &armnetwork.NatGatewayPropertiesFormat{
			IdleTimeoutInMinutes: ptr.To[int32](10),
			Nat64:                ptr.To(armnetwork.Nat64StateEnabled),
			SourceVirtualNetwork: &armnetwork.SubResource{ID: ptr.To("/subscriptions/sub/resourceGroups/owner-rg/providers/Microsoft.Network/virtualNetworks/owner-vnet")},
			PublicIPAddresses:    []*armnetwork.SubResource{{ID: ptr.To("pip-v4")}},
			PublicIPAddressesV6:  []*armnetwork.SubResource{{ID: ptr.To("pip-v6")}},
			PublicIPPrefixes:     []*armnetwork.SubResource{{ID: ptr.To("prefix-v4")}},
			PublicIPPrefixesV6:   []*armnetwork.SubResource{{ID: ptr.To("prefix-v6")}},
			ProvisioningState:    ptr.To(armnetwork.ProvisioningStateSucceeded),
		},
	}
}

// byoPod returns the triggering pod ns/pod of the tests, a member of egress identity identity, asking for
// NAT Gateway natGatewayID with the annotation consts.PodAnnotationServiceEgressGatewayNATGatewayID, or
// without it when natGatewayID is "".
func byoPod(identity, natGatewayID string) *v1.Pod {
	return egressTestPod("pod", "pod-uid", identity, natGatewayID)
}

// egressTestPod returns pod ns/name of egress identity identity (none when ""), naming NAT Gateway
// natGatewayID (none when "").
func egressTestPod(name string, uid types.UID, identity, natGatewayID string) *v1.Pod {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, UID: uid}}
	if identity != "" {
		pod.Labels = map[string]string{consts.PodLabelServiceEgressGateway: identity}
	}
	if natGatewayID != "" {
		pod.Annotations = map[string]string{consts.PodAnnotationServiceEgressGatewayNATGatewayID: natGatewayID}
	}
	return pod
}

func drainEvents(rec *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-rec.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

func TestClaimBYONATGateway(t *testing.T) {
	dt := newTestDiffTracker()
	id := byoNATGatewayARMID("shared")
	type claim struct {
		identity string
		fresh    bool
		owner    string
	}
	const contenders = 8
	start := make(chan struct{})
	results := make(chan claim, contenders)
	for i := 0; i < contenders; i++ {
		go func() {
			<-start
			identity := fmt.Sprintf("identity-%d", i)
			target := id
			if i%2 == 0 {
				target = strings.ToUpper(target)
			}
			fresh, owner := dt.claimBYONATGateway(identity, target)
			results <- claim{identity, fresh, owner}
		}()
	}
	close(start)
	var claims []claim
	var winner string
	for i := 0; i < contenders; i++ {
		result := <-results
		claims = append(claims, result)
		if result.fresh {
			assert.Empty(t, winner, "only one contender may reserve the gateway")
			winner = result.identity
		}
	}
	assert.NotEmpty(t, winner)
	for _, result := range claims {
		if result.identity != winner {
			assert.Equal(t, winner, result.owner)
			assert.Empty(t, dt.byoNATGatewayID(result.identity))
		}
	}
	fresh, owner := dt.claimBYONATGateway(strings.ToUpper(winner), id)
	assert.False(t, fresh, "the existing identity keeps its claim")
	assert.Empty(t, owner)
	fresh, owner = dt.claimBYONATGateway("other-rg", strings.Replace(id, "/"+byoNATGatewayRG+"/", "/another-rg/", 1))
	assert.True(t, fresh, "a same-named gateway in another resource group is a different resource")
	assert.Empty(t, owner)
	dt.setBYONATGatewayID(winner, "")
	fresh, owner = dt.claimBYONATGateway("next-owner", id)
	assert.True(t, fresh)
	assert.Empty(t, owner)
}

func TestBYONATGatewayClaim_SerializesCreationAndDeletion(t *testing.T) {
	for _, unlinkFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("holdUnlink=%t", unlinkFirst), func(t *testing.T) {
			const first, next, gateway = "payments", "reports", "shared"
			id := byoNATGatewayARMID(gateway)
			cfg := testConfig()
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)
			dt := newTestDiffTracker()
			dt.config, dt.networkClientFactory = cfg, m.factory
			dt.kubeClient = fake.NewSimpleClientset(
				egressTestPod("first", "pod-a", first, id), egressTestPod("next", "pod-b", next, id))
			var mu sync.Mutex
			link := ""
			registrations := map[string]string{}
			if unlinkFirst {
				link = cfg.ServiceGatewayResourceID()
				registrations[first] = id
				dt.setBYONATGatewayID(first, id)
				dt.NRPResources.NATGateways.Insert(first)
			}
			m.nat.EXPECT().Get(gomock.Any(), byoNATGatewayRG, gateway, nil).DoAndReturn(
				func(context.Context, string, string, *string) (*armnetwork.NatGateway, error) {
					mu.Lock()
					defer mu.Unlock()
					n := byoNATGateway(gateway)
					if link != "" {
						n.Properties.ServiceGateway = &armnetwork.SubResource{ID: ptr.To(link)}
					}
					return n, nil
				}).AnyTimes()
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var released sync.Once
			defer released.Do(func() { close(release) })
			blocked := false
			m.nat.EXPECT().CreateOrUpdate(gomock.Any(), byoNATGatewayRG, gateway, gomock.Any()).DoAndReturn(
				func(_ context.Context, _, _ string, n armnetwork.NatGateway) (*armnetwork.NatGateway, error) {
					mu.Lock()
					hold := !blocked
					blocked = true
					mu.Unlock()
					if hold {
						close(entered)
						<-release
					}
					mu.Lock()
					link = natGatewayServiceGatewayID(&n)
					mu.Unlock()
					return &n, nil
				}).AnyTimes()
			m.sgw.EXPECT().GetServices(gomock.Any(), "rg", "sgw").DoAndReturn(
				func(context.Context, string, string) ([]*armnetwork.ServiceGatewayService, error) {
					mu.Lock()
					defer mu.Unlock()
					var services []*armnetwork.ServiceGatewayService
					for name, natID := range registrations {
						services = append(services, &armnetwork.ServiceGatewayService{Name: ptr.To(name),
							Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{PublicNatGatewayID: ptr.To(natID)}})
					}
					return services, nil
				}).AnyTimes()
			m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).DoAndReturn(
				func(_ context.Context, _, _ string, req armnetwork.ServiceGatewayUpdateServicesRequest) error {
					mu.Lock()
					defer mu.Unlock()
					for _, service := range req.ServiceRequests {
						name := derefString(service.Service.Name)
						if ptr.Deref(service.IsDelete, false) {
							delete(registrations, name)
						} else {
							registrations[name] = derefString(service.Service.Properties.PublicNatGatewayID)
						}
					}
					return nil
				}).AnyTimes()
			firstResult := &outboundCompletion{}
			go func() {
				defer close(finished)
				updater := outboundUpdater(dt, firstResult)
				if unlinkFirst {
					updater.deleteOutboundService(first, "corr")
				} else {
					updater.createOutboundService(first, &OutboundConfig{}, "corr", "ns", "first")
				}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first identity did not reach the held Azure call")
			}
			rejected := &outboundCompletion{}
			outboundUpdater(dt, rejected).createOutboundService(next, &OutboundConfig{}, "corr", "ns", "next")
			_, success, err := rejected.result()
			assert.False(t, success)
			assert.True(t, isEgressRejected(err), "%v", err)
			assert.Empty(t, dt.byoNATGatewayID(next))
			mu.Lock()
			assert.NotContains(t, registrations, next)
			mu.Unlock()
			released.Do(func() { close(release) })
			<-finished
			_, success, err = firstResult.result()
			assert.True(t, success, "%v", err)
			if !unlinkFirst {
				deleted := &outboundCompletion{}
				outboundUpdater(dt, deleted).deleteOutboundService(first, "corr")
				_, success, err = deleted.result()
				assert.True(t, success, "%v", err)
			}
			assert.Empty(t, dt.byoNATGatewayID(first))
			created := &outboundCompletion{}
			outboundUpdater(dt, created).createOutboundService(next, &OutboundConfig{}, "corr", "ns", "next")
			_, success, err = created.result()
			assert.True(t, success, "%v", err)
			assert.Equal(t, id, dt.byoNATGatewayID(next))
			mu.Lock()
			assert.Equal(t, cfg.ServiceGatewayResourceID(), link)
			assert.Equal(t, id, registrations[next])
			mu.Unlock()
		})
	}
}

func TestManagedCreationCannotManufacturePublicIPOwnership(t *testing.T) {
	const identity = "payments"
	for _, tc := range []struct {
		name  string
		tags  map[string]*string
		owned bool
	}{
		{name: "untagged"},
		{name: "foreign egress owner", tags: egressIdentityTags("other")},
		{name: "inbound owner", tags: map[string]*string{InboundServiceUIDTagKey: ptr.To(identity)}},
		{name: "owned retry", tags: map[string]*string{EgressIdentityTagKey: ptr.To(identity), "extra": ptr.To("keep")}, owned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)
			m.expectNoDisassociation()
			stored := &armnetwork.PublicIPAddress{Name: ptr.To(PublicIPName(identity)), Etag: ptr.To(`W/"pip-version"`), Tags: tc.tags}
			pipWrites, natWrites, pipDeletes := 0, 0, 0
			m.pip.EXPECT().Get(gomock.Any(), "rg", gomock.Any(), nil).DoAndReturn(
				func(_ context.Context, _, name string, _ *string) (*armnetwork.PublicIPAddress, error) {
					if name != PublicIPName(identity) {
						return nil, notFoundError()
					}
					result := *stored
					return &result, nil
				}).AnyTimes()
			m.pip.EXPECT().CreateOrUpdate(gomock.Any(), "rg", PublicIPName(identity), gomock.Any()).DoAndReturn(
				func(_ context.Context, _, _ string, value armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
					pipWrites++
					stored = &value
					return stored, nil
				}).AnyTimes()
			m.nat.EXPECT().CreateOrUpdate(gomock.Any(), "rg", identity, gomock.Any()).DoAndReturn(
				func(context.Context, string, string, armnetwork.NatGateway) (*armnetwork.NatGateway, error) {
					natWrites++
					return nil, nil
				}).AnyTimes()
			m.nat.EXPECT().Delete(gomock.Any(), "rg", identity).Return(nil).AnyTimes()
			m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName(identity)).DoAndReturn(
				func(context.Context, string, string) error { pipDeletes++; return nil }).AnyTimes()
			m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).AnyTimes()
			dt := newTestDiffTracker()
			dt.config, dt.networkClientFactory = testConfig(), m.factory
			dt.kubeClient = fake.NewSimpleClientset(byoPod(identity, ""))
			created := &outboundCompletion{}
			outboundUpdater(dt, created).createOutboundService(identity, &OutboundConfig{}, "corr", "ns", "pod")
			_, success, err := created.result()
			assert.Equal(t, tc.owned, success, "%v", err)
			assert.Equal(t, wantedCalls(tc.owned), pipWrites)
			assert.Equal(t, wantedCalls(tc.owned), natWrites)
			assert.Equal(t, tc.tags, stored.Tags, "a managed request must not adopt an existing PIP or discard its tags")
			assert.Equal(t, `W/"pip-version"`, derefString(stored.Etag))
			if !tc.owned {
				assert.True(t, isEgressRejected(err), "%v", err)
			}
			deleted := &outboundCompletion{}
			outboundUpdater(dt, deleted).deleteOutboundService(identity, "corr")
			_, success, err = deleted.result()
			assert.True(t, success, "%v", err)
			assert.Equal(t, wantedCalls(tc.owned), pipDeletes, "only an address already owned before creation may be deleted")
		})
	}
}

func TestServiceUpdaterCreateOutboundService_BYONATGateway(t *testing.T) {
	const uid = "byo-egress"
	cfg := testConfig()
	serviceGatewayID := cfg.ServiceGatewayResourceID()
	forbidden := &azcore.ResponseError{StatusCode: http.StatusForbidden}
	boom := errors.New("ARM failure")
	named := byoNATGatewayARMID(uid)

	for _, tc := range []struct {
		name string
		// annotation is the NAT Gateway ID the triggering pod asks for; "" for a managed identity.
		annotation string
		// natName is the name of the annotated NAT Gateway, uid when empty.
		natName       string
		families      []string
		nodeRGExists  bool
		nodeRGForeign bool
		nodeRGErr     error
		staleSnapshot bool
		byo           func() *armnetwork.NatGateway
		byoErr        error
		// noRead: the annotation is rejected before any Azure call.
		noRead       bool
		services     []*armnetwork.ServiceGatewayService
		servicesErr  error
		linkErr      error
		registerErr  error
		wantManaged  bool
		wantLink     bool
		wantRegister bool
		wantSuccess  bool
		wantRecorded bool
		wantEvent    string
		// wantEventTexts are phrases the event must contain; the e2e specs match some of them.
		wantEventTexts []string
	}{
		{name: "without the annotation its own cluster resource group NAT Gateway is reused", nodeRGExists: true, wantManaged: true, wantSuccess: true},
		{name: "without the annotation a managed NAT Gateway is created and nothing else is read", wantManaged: true, wantSuccess: true},
		{name: "a removed unmanaged gateway no longer blocks managed creation", staleSnapshot: true, wantManaged: true, wantSuccess: true},
		{name: "without the annotation a transient cluster resource group lookup error is retried without an event", nodeRGErr: boom},
		{name: "without the annotation a cluster resource group NAT Gateway not created by this controller is rejected", nodeRGForeign: true,
			wantEvent: egressNATGatewayRejectedReason},
		{name: "links and registers the annotated NAT Gateway", annotation: named, wantRecorded: true, byo: func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "an unrelated node resource group name does not block explicit BYO", annotation: named, staleSnapshot: true,
			wantRecorded: true, byo: func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "resource ID separators are normalized before recording", annotation: strings.Replace(named, "/resourceGroups/", "//resourceGroups/", 1) + "/",
			wantRecorded: true, byo: func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "whitespace around the annotation, as a YAML block scalar leaves it, is ignored", annotation: " " + named + "\n", wantRecorded: true,
			byo:      func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "the annotated NAT Gateway may have any name", annotation: byoNATGatewayARMID("their-nat"), natName: "their-nat", wantRecorded: true,
			byo:      func() *armnetwork.NatGateway { return byoNATGateway("their-nat") },
			wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "a fresh claim reasserts an already linked gateway", annotation: named, wantRecorded: true, byo: func() *armnetwork.NatGateway {
			n := byoNATGateway(uid)
			n.Properties.ServiceGateway = &armnetwork.SubResource{ID: ptr.To(strings.ToUpper(serviceGatewayID))}
			return n
		}, wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "linked gateway that is not Succeeded is written again", annotation: named, wantRecorded: true, byo: func() *armnetwork.NatGateway {
			n := byoNATGateway(uid)
			n.Properties.ServiceGateway = &armnetwork.SubResource{ID: ptr.To(serviceGatewayID)}
			n.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateFailed)
			return n
		}, wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "an annotated NAT Gateway that does not exist is rejected, never replaced by a managed one", annotation: named, byoErr: notFoundError(),
			wantEvent: egressNATGatewayRejectedReason, wantEventTexts: []string{`Egress identity "byo-egress" cannot be used`, "does not exist"}},
		{name: "an annotated NAT Gateway that cannot be read is rejected, never replaced by a managed one", annotation: named, byoErr: forbidden,
			wantEvent: egressNATGatewayRejectedReason, wantEventTexts: []string{`Egress identity "byo-egress" cannot be used`, "access denied when reading"}},
		{name: "a transient read failure of the annotated NAT Gateway is retried without an event", annotation: named, byoErr: boom},
		{name: "an annotation that is not a resource ID is rejected without an Azure call", annotation: "their-nat", noRead: true,
			wantEvent: egressNATGatewayRejectedReason, wantEventTexts: []string{"is not an Azure resource ID"}},
		{name: "an annotation that is not a NAT Gateway is rejected without an Azure call", noRead: true,
			annotation: "/subscriptions/sub/resourceGroups/" + byoNATGatewayRG + "/providers/Microsoft.Network/publicIPAddresses/x",
			wantEvent:  egressNATGatewayRejectedReason, wantEventTexts: []string{"is not the ID of a NAT Gateway in a resource group"}},
		{name: "a NAT Gateway in another subscription is rejected without an Azure call", noRead: true,
			annotation: "/subscriptions/other/resourceGroups/" + byoNATGatewayRG + "/providers/Microsoft.Network/natGateways/x",
			wantEvent:  egressNATGatewayRejectedReason, wantEventTexts: []string{"is not in the cluster's network subscription"}},
		{name: "a NAT Gateway in the cluster resource group is rejected without an Azure call", noRead: true,
			annotation: "/subscriptions/sub/resourceGroups/RG/providers/Microsoft.Network/natGateways/x",
			wantEvent:  egressNATGatewayRejectedReason, wantEventTexts: []string{"is in the cluster resource group"}},
		{name: "wrong SKU is rejected, also while the Service Gateway services cannot be read", annotation: named, byo: func() *armnetwork.NatGateway {
			n := byoNATGateway(uid)
			n.SKU.Name = ptr.To(armnetwork.NatGatewaySKUNameStandard)
			return n
		}, servicesErr: boom, wantEvent: egressNATGatewayRejectedReason},
		{name: "gateway linked to another Service Gateway is rejected", annotation: named, byo: func() *armnetwork.NatGateway {
			n := byoNATGateway(uid)
			n.Properties.ServiceGateway = &armnetwork.SubResource{ID: ptr.To("/subscriptions/sub/resourceGroups/other/providers/Microsoft.Network/serviceGateways/sgw")}
			return n
		}, wantEvent: egressNATGatewayRejectedReason},
		{name: "a gateway without IPv6 addresses is linked on a dual-stack cluster, its owner adds them", annotation: named, wantRecorded: true,
			families: []string{"IPv4", "IPv6"}, byo: func() *armnetwork.NatGateway {
				n := byoNATGateway(uid)
				n.Properties.PublicIPAddressesV6, n.Properties.PublicIPPrefixesV6 = nil, nil
				return n
			}, wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "a gateway without addresses is linked, its owner adds them", annotation: named, wantRecorded: true, byo: func() *armnetwork.NatGateway {
			n := byoNATGateway(uid)
			n.Properties.PublicIPAddresses, n.Properties.PublicIPAddressesV6 = nil, nil
			n.Properties.PublicIPPrefixes, n.Properties.PublicIPPrefixesV6 = nil, nil
			return n
		}, wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "default outbound gateway is rejected", annotation: named, byo: func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			services: []*armnetwork.ServiceGatewayService{{Name: ptr.To("default-natgw"), Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
				IsDefault: ptr.To(true), PublicNatGatewayID: ptr.To(strings.ToLower(byoNATGatewayARMID(uid)))}}},
			wantEvent: egressNATGatewayRejectedReason},
		{name: "the validated lookup identity overrides an invalid response ID and name", annotation: named, byo: func() *armnetwork.NatGateway {
			n := byoNATGateway(uid)
			n.ID = ptr.To("not-an-arm-id")
			n.Name = ptr.To("wrong-response-name")
			return n
		}, wantRecorded: true, wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "a missing response ID cannot lose the validated lookup identity", annotation: named, byo: func() *armnetwork.NatGateway {
			n := byoNATGateway(uid)
			n.ID = nil
			return n
		}, wantRecorded: true, wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "an empty successful read never falls back to managed creation", annotation: named},
		{name: "its own earlier registration does not block a retry", annotation: named, wantRecorded: true, byo: func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			services: []*armnetwork.ServiceGatewayService{{Name: ptr.To(uid), Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{PublicNatGatewayID: ptr.To(byoNATGatewayARMID(uid))}}},
			wantLink: true, wantRegister: true, wantSuccess: true, wantEvent: egressNATGatewayLinkedReason},
		{name: "a transient link failure is retried without an event", annotation: named, wantRecorded: true, byo: func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			linkErr: boom, wantLink: true},
		{name: "unreadable Service Gateway services are retried, nothing linked", annotation: named, byo: func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			servicesErr: boom},
		{name: "a throttled read of the annotated NAT Gateway is retried without an event", annotation: named,
			byoErr: &azcore.ResponseError{StatusCode: http.StatusTooManyRequests}},
		{name: "a gateway already linked here that cannot be used is rejected and stays recorded for the unlink", annotation: named,
			wantRecorded: true, byo: func() *armnetwork.NatGateway {
				n := byoNATGateway(uid)
				n.SKU.Name = ptr.To(armnetwork.NatGatewaySKUNameStandard)
				n.Properties.ServiceGateway = &armnetwork.SubResource{ID: ptr.To(serviceGatewayID)}
				return n
			}, wantEvent: egressNATGatewayRejectedReason},
		{name: "a gateway already linked here stays recorded for the unlink when the Service Gateway services cannot be read", annotation: named,
			wantRecorded: true, byo: func() *armnetwork.NatGateway {
				n := byoNATGateway(uid)
				n.Properties.ServiceGateway = &armnetwork.SubResource{ID: ptr.To(serviceGatewayID)}
				return n
			}, servicesErr: boom},
		{name: "a link denied by Azure is rejected and forgotten, so the annotation is read again", annotation: named,
			byo: func() *armnetwork.NatGateway { return byoNATGateway(uid) }, linkErr: forbidden, wantLink: true, wantEvent: egressNATGatewayRejectedReason,
			wantEventTexts: []string{`Egress identity "byo-egress" cannot use NAT Gateway`, "access denied when linking"}},
		{name: "a link refused with another 4xx is rejected with Azure's error code and forgotten", annotation: named,
			byo:     func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			linkErr: &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "InvalidNatGatewayReference"}, wantLink: true,
			wantEvent: egressNATGatewayRejectedReason, wantEventTexts: []string{"Azure refused to link it to the Service Gateway (HTTP 400 InvalidNatGatewayReference)"}},
		{name: "a refused link of a gateway already linked here keeps it recorded for the unlink", annotation: named, wantRecorded: true,
			byo: func() *armnetwork.NatGateway {
				n := byoNATGateway(uid)
				n.Properties.ServiceGateway = &armnetwork.SubResource{ID: ptr.To(serviceGatewayID)}
				n.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateFailed)
				return n
			}, linkErr: forbidden, wantLink: true, wantEvent: egressNATGatewayRejectedReason},
		{name: "a link timeout is retried without an event and stays recorded", annotation: named, wantRecorded: true,
			byo:     func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			linkErr: &azcore.ResponseError{StatusCode: http.StatusRequestTimeout}, wantLink: true},
		{name: "a conflicting operation on the link is retried without an event and stays recorded", annotation: named, wantRecorded: true,
			byo:     func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			linkErr: &azcore.ResponseError{StatusCode: http.StatusConflict, ErrorCode: "AnotherOperationInProgress"}, wantLink: true},
		{name: "a stale first link is retried without an event or ownership record", annotation: named,
			byo:     func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			linkErr: &azcore.ResponseError{StatusCode: http.StatusPreconditionFailed, ErrorCode: "PreconditionFailed"}, wantLink: true},
		{name: "a gateway without an ETag is retried without writing or recording a new link", annotation: named,
			byo: func() *armnetwork.NatGateway {
				n := byoNATGateway(uid)
				n.Etag = nil
				return n
			}},
		{name: "a link answered 404, its resource group gone, is rejected and forgotten", annotation: named,
			byo:     func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			linkErr: &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "ResourceGroupNotFound"}, wantLink: true,
			wantEvent: egressNATGatewayRejectedReason, wantEventTexts: []string{"(HTTP 404 ResourceGroupNotFound)"}},
		{name: "a link blocked by a resource lock is rejected and forgotten", annotation: named,
			byo:     func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			linkErr: &azcore.ResponseError{StatusCode: http.StatusConflict, ErrorCode: "ScopeLocked"}, wantLink: true,
			wantEvent: egressNATGatewayRejectedReason, wantEventTexts: []string{"Azure refused to link it to the Service Gateway (HTTP 409 ScopeLocked)"}},
		{name: "a throttled link is retried without an event and stays recorded", annotation: named, wantRecorded: true,
			byo:     func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			linkErr: &azcore.ResponseError{StatusCode: http.StatusTooManyRequests}, wantLink: true},
		{name: "a server error on the link is retried without an event and stays recorded, as the write may have been applied", annotation: named,
			wantRecorded: true, byo: func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			linkErr: &azcore.ResponseError{StatusCode: http.StatusInternalServerError, ErrorCode: "InternalServerError"}, wantLink: true},
		{name: "a gateway read without its properties is rejected, nothing written", annotation: named, byo: func() *armnetwork.NatGateway {
			n := byoNATGateway(uid)
			n.Properties = nil
			return n
		}, wantEvent: egressNATGatewayRejectedReason, wantEventTexts: []string{"was read without its properties"}},
		{name: "registration failure is retried", annotation: named, wantRecorded: true, byo: func() *armnetwork.NatGateway { return byoNATGateway(uid) },
			wantLink: true, registerErr: boom, wantRegister: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)
			natName := tc.natName
			if natName == "" {
				natName = uid
			}

			// Strict mocks: a read the row does not expect, such as a BYO lookup of a managed identity,
			// fails the test.
			switch {
			case tc.annotation == "" && tc.nodeRGExists:
				m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(&armnetwork.NatGateway{Name: ptr.To(uid), Tags: egressIdentityTags(uid)}, nil).Times(1)
			case tc.annotation == "" && tc.nodeRGErr != nil:
				m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, tc.nodeRGErr).Times(1)
			case tc.annotation == "" && tc.nodeRGForeign:
				m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(&armnetwork.NatGateway{Name: ptr.To(uid)}, nil).Times(1)
			case tc.annotation == "":
				m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, notFoundError()).Times(1)
			case !tc.noRead:
				var gateway *armnetwork.NatGateway
				if tc.byo != nil {
					gateway = tc.byo()
				}
				m.nat.EXPECT().Get(gomock.Any(), byoNATGatewayRG, natName, gomock.Any()).Return(gateway, tc.byoErr).Times(1)
			}
			servicesReads := 0
			m.sgw.EXPECT().GetServices(gomock.Any(), "rg", "sgw").DoAndReturn(func(context.Context, string, string) ([]*armnetwork.ServiceGatewayService, error) {
				servicesReads++
				return tc.services, tc.servicesErr
			}).MaxTimes(1)

			linkCalls := 0
			m.nat.EXPECT().CreateOrUpdate(gomock.Any(), byoNATGatewayRG, natName, gomock.Any()).
				DoAndReturn(func(_ context.Context, _, _ string, n armnetwork.NatGateway) (*armnetwork.NatGateway, error) {
					linkCalls++
					want := tc.byo()
					want.ID = ptr.To(byoNATGatewayARMID(natName))
					want.Name = ptr.To(natName)
					want.Properties.ServiceGateway = &armnetwork.SubResource{ID: ptr.To(serviceGatewayID)}
					assert.Equal(t, *want, n, "only the link may change on the owner's NAT Gateway")
					return &n, initialNATPutError(tc.linkErr)
				}).AnyTimes()
			registered := 0
			if tc.wantManaged {
				m.pip.EXPECT().Get(gomock.Any(), "rg", PublicIPName(uid), nil).Return(nil, notFoundError()).Times(1)
				m.pip.EXPECT().CreateOrUpdate(gomock.Any(), "rg", PublicIPName(uid), gomock.Any()).
					Return(&armnetwork.PublicIPAddress{Name: ptr.To(PublicIPName(uid))}, nil).Times(1)
				m.nat.EXPECT().CreateOrUpdate(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, nil).Times(1)
				m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).Times(1)
			} else {
				m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).
					DoAndReturn(func(_ context.Context, _, _ string, req armnetwork.ServiceGatewayUpdateServicesRequest) error {
						registered++
						assert.Equal(t, byoNATGatewayARMID(natName), derefString(req.ServiceRequests[0].Service.Properties.PublicNatGatewayID))
						return tc.registerErr
					}).AnyTimes()
			}

			dt := newTestDiffTracker()
			dt.config = testConfig()
			dt.networkClientFactory = m.factory
			dt.kubeClient = fake.NewSimpleClientset(byoPod(uid, tc.annotation))
			if tc.staleSnapshot {
				dt.NRPResources.UnmanagedNATGateways = utilsets.NewString(uid)
			}
			rec := &captureRecorder{FakeRecorder: *record.NewFakeRecorder(10)}
			dt.eventRecorder = rec
			got := &outboundCompletion{}
			outboundUpdater(dt, got).createOutboundService(uid, &OutboundConfig{IPFamilies: tc.families}, "corr", "ns", "pod")
			for _, object := range rec.objects {
				assert.Equal(t, types.UID("pod-uid"), object.(*v1.Pod).UID, "events must be recorded on the pod whose annotation decided")
			}

			called, success, err := got.result()
			assert.True(t, called)
			assert.Equal(t, tc.wantSuccess, success, "completion error: %v", err)
			if !tc.wantSuccess {
				assert.False(t, isTerminalError(err), "a failed attempt must be retried, so a fixed annotation is used: %v", err)
				assert.Equal(t, tc.wantEvent == egressNATGatewayRejectedReason, isEgressRejected(err),
					"only BYO admission rejections are classified as egressRejectedError: %v", err)
			}
			assert.Equal(t, tc.wantSuccess, dt.NRPResources.NATGateways.Has(uid))
			assert.Equal(t, wantedCalls(tc.wantLink), linkCalls, "link writes")
			if tc.noRead || tc.byoErr != nil {
				assert.Zero(t, servicesReads, "the Service Gateway is read only for a NAT Gateway that was read")
			}
			if !tc.wantManaged {
				assert.Equal(t, wantedCalls(tc.wantRegister), registered, "registrations")
			}
			if tc.wantRecorded {
				assert.Equal(t, byoNATGatewayARMID(natName), dt.byoNATGatewayID(uid), "a linked gateway must be recorded so a delete unlinks it")
			} else {
				assert.Empty(t, dt.byoNATGatewayID(uid), "a gateway that was never linked must not be unlinked on delete")
			}
			events := drainEvents(&rec.FakeRecorder)
			if tc.wantEvent == "" {
				assert.Empty(t, events)
			} else if assert.Len(t, events, 1) {
				assert.Contains(t, events[0], tc.wantEvent)
				for _, text := range tc.wantEventTexts {
					assert.Contains(t, events[0], text)
				}
				wantType := v1.EventTypeWarning
				if tc.wantEvent != egressNATGatewayRejectedReason {
					wantType = v1.EventTypeNormal
				}
				assert.True(t, strings.HasPrefix(events[0], wantType), "event %q should be %s", events[0], wantType)
			}
		})
	}
}

func TestServiceUpdaterCreateOutboundService_KeepsLinkedBYONATGateway(t *testing.T) {
	const uid = "byo-linked"
	forbidden := &azcore.ResponseError{StatusCode: http.StatusForbidden}
	cfg := testConfig()
	linkedHere := func() *armnetwork.NatGateway {
		n := byoNATGateway(uid)
		n.Properties.ServiceGateway = &armnetwork.SubResource{ID: ptr.To(cfg.ServiceGatewayResourceID())}
		return n
	}

	recorded := byoNATGatewayARMID(uid)
	gone := func() (*armnetwork.NatGateway, error) { return nil, notFoundError() }

	for _, tc := range []struct {
		name     string
		linked   func() (*armnetwork.NatGateway, error)
		services []*armnetwork.ServiceGatewayService
		// named is the BYO NAT Gateway the pods name now ("" for none); it is read (answering namedErr) only
		// when readsNamed, that is once the recorded gateway is gone.
		named        string
		namedErr     error
		readsNamed   bool
		wantManaged  bool
		wantSuccess  bool
		wantRecord   string
		wantRejected bool
	}{
		{name: "reuses the linked gateway without resolving again", linked: func() (*armnetwork.NatGateway, error) { return linkedHere(), nil },
			named: "other", wantSuccess: true, wantRecord: recorded},
		{name: "missing properties do not erase the recorded gateway", linked: func() (*armnetwork.NatGateway, error) {
			n := linkedHere()
			n.Properties = nil
			return n, nil
		}, wantRecord: recorded, wantRejected: true},
		{name: "missing response ID does not erase the recorded gateway", linked: func() (*armnetwork.NatGateway, error) {
			n := linkedHere()
			n.ID = nil
			return n, nil
		}, wantRecord: recorded, wantSuccess: true},
		{name: "an empty successful read retains the recorded gateway", linked: func() (*armnetwork.NatGateway, error) {
			return nil, nil
		}, wantRecord: recorded},
		{name: "a gateway its owner deleted is forgotten and pods without the annotation get a managed one", linked: gone,
			wantManaged: true, wantSuccess: true},
		{name: "a gateway its owner deleted is forgotten and the gateway the pods name now is used", linked: gone,
			named: "replacement", readsNamed: true, wantSuccess: true, wantRecord: byoNATGatewayARMID("replacement")},
		{name: "a gateway its owner deleted is forgotten, and a missing gateway the pods name is rejected, never replaced by a managed one",
			linked: gone, named: "missing", namedErr: notFoundError(), readsNamed: true, wantRejected: true},
		{name: "a recorded gateway now used by another service is rejected but stays recorded for the unlink",
			linked: func() (*armnetwork.NatGateway, error) { return linkedHere(), nil },
			services: []*armnetwork.ServiceGatewayService{{Name: ptr.To("other"), Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
				PublicNatGatewayID: ptr.To(byoNATGatewayARMID(uid))}}},
			wantRecord: recorded, wantRejected: true},
		{name: "an unusable recorded gateway that carries no link of ours is rejected and forgotten", linked: func() (*armnetwork.NatGateway, error) {
			n := byoNATGateway(uid)
			n.SKU.Name = ptr.To(armnetwork.NatGatewaySKUNameStandard)
			return n, nil
		}, wantRejected: true},
		{name: "a recorded gateway its owner linked to another Service Gateway is rejected and forgotten", linked: func() (*armnetwork.NatGateway, error) {
			n := byoNATGateway(uid)
			n.Properties.ServiceGateway = &armnetwork.SubResource{ID: ptr.To("/subscriptions/sub/resourceGroups/other-rg/providers/Microsoft.Network/serviceGateways/other")}
			return n, nil
		}, wantRejected: true},
		{name: "an unreadable recorded gateway is rejected, stays recorded, and is never replaced by a managed one",
			linked: func() (*armnetwork.NatGateway, error) { return nil, forbidden }, wantRecord: recorded, wantRejected: true},
		{name: "a recorded gateway that cannot be read for now is retried and stays recorded",
			linked: func() (*armnetwork.NatGateway, error) {
				return nil, &azcore.ResponseError{StatusCode: http.StatusInternalServerError, ErrorCode: "InternalServerError"}
			}, wantRecord: recorded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)
			linked, linkedErr := tc.linked()
			m.nat.EXPECT().Get(gomock.Any(), byoNATGatewayRG, uid, gomock.Any()).Return(linked, linkedErr).Times(1)
			m.sgw.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(tc.services, nil).AnyTimes()
			// The recorded gateway stays selected, but creation still requires a successful reassertion.
			if tc.wantSuccess && !tc.wantManaged && !tc.readsNamed {
				m.nat.EXPECT().CreateOrUpdate(gomock.Any(), byoNATGatewayRG, uid, gomock.Any()).Return(nil, nil).Times(1)
			}
			annotation := ""
			if tc.named != "" {
				annotation = byoNATGatewayARMID(tc.named)
			}
			if tc.readsNamed {
				var named *armnetwork.NatGateway
				if tc.namedErr == nil {
					named = byoNATGateway(tc.named)
				}
				m.nat.EXPECT().Get(gomock.Any(), byoNATGatewayRG, tc.named, gomock.Any()).Return(named, tc.namedErr).Times(1)
				m.nat.EXPECT().CreateOrUpdate(gomock.Any(), byoNATGatewayRG, tc.named, gomock.Any()).Return(nil, nil).Times(wantedCalls(tc.namedErr == nil))
			}
			if tc.wantManaged {
				m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, notFoundError()).Times(1)
				m.pip.EXPECT().Get(gomock.Any(), "rg", PublicIPName(uid), nil).Return(nil, notFoundError()).Times(1)
				m.pip.EXPECT().CreateOrUpdate(gomock.Any(), "rg", PublicIPName(uid), gomock.Any()).Return(&armnetwork.PublicIPAddress{}, nil).Times(1)
				m.nat.EXPECT().CreateOrUpdate(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, nil).Times(1)
			}
			m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).AnyTimes()

			dt := newTestDiffTracker()
			dt.config = testConfig()
			dt.networkClientFactory = m.factory
			dt.kubeClient = fake.NewSimpleClientset(byoPod(uid, annotation))
			rec := &captureRecorder{FakeRecorder: *record.NewFakeRecorder(10)}
			dt.eventRecorder = rec
			dt.setBYONATGatewayID(uid, recorded)
			got := &outboundCompletion{}
			outboundUpdater(dt, got).createOutboundService(uid, &OutboundConfig{}, "corr", "ns", "pod")

			_, success, err := got.result()
			assert.Equal(t, tc.wantSuccess, success, "completion error: %v", err)
			assert.Equal(t, tc.wantRejected, isEgressRejected(err), "%v", err)
			assert.Equal(t, tc.wantRecord, dt.byoNATGatewayID(uid))
			events := drainEvents(&rec.FakeRecorder)
			switch {
			case tc.wantRejected:
				if assert.Len(t, events, 1) {
					assert.Contains(t, events[0], egressNATGatewayRejectedReason)
				}
			case tc.wantSuccess && !tc.wantManaged:
				if assert.Len(t, events, 1) {
					assert.Contains(t, events[0], egressNATGatewayLinkedReason)
				}
			default:
				assert.Empty(t, events)
			}
			for _, object := range rec.objects {
				assert.Equal(t, types.UID("pod-uid"), object.(*v1.Pod).UID, "the identity's events go to its member pod")
			}
		})
	}
}

func TestBYOCreationRejectsRecoveredInboundIdentityBeforeAzure(t *testing.T) {
	const uid = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	for _, source := range []string{"NRP", "Kubernetes"} {
		t.Run(source, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)
			dt := newTestDiffTracker()
			dt.config, dt.networkClientFactory = testConfig(), m.factory
			dt.kubeClient = fake.NewSimpleClientset(byoPod(uid, byoNATGatewayARMID("customer-nat")))
			if source == "NRP" {
				dt.NRPResources.LoadBalancers.Insert(uid)
			} else {
				dt.K8sResources.Services.Insert(uid)
			}
			result := &outboundCompletion{}
			outboundUpdater(dt, result).createOutboundService(uid, &OutboundConfig{}, "corr", "ns", "pod")
			called, success, err := result.result()
			assert.True(t, called)
			assert.False(t, success)
			assert.True(t, isEgressRejected(err), "%v", err)
			assert.Empty(t, dt.byoNATGatewayID(uid))
			assert.False(t, dt.NRPResources.NATGateways.Has(uid))
		})
	}
}

// RP-owned service identities remain reserved even when a pod selects a different BYO gateway.
func TestServiceUpdaterCreateOutboundService_RejectsReservedOutboundService(t *testing.T) {
	const uid = "custom-default"
	for name, annotation := range map[string]string{"managed": "", "BYO": byoNATGatewayARMID("their-nat")} {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl) // no expectations: any Azure call fails the test

			dt := newTestDiffTracker()
			dt.config = testConfig()
			dt.networkClientFactory = m.factory
			dt.NRPResources.ReservedOutboundServices = utilsets.NewString("Custom-Default")
			dt.kubeClient = fake.NewSimpleClientset(byoPod(uid, annotation))
			rec := &captureRecorder{FakeRecorder: *record.NewFakeRecorder(10)}
			dt.eventRecorder = rec
			got := &outboundCompletion{}
			outboundUpdater(dt, got).createOutboundService(uid, &OutboundConfig{}, "corr", "ns", "pod")

			_, success, err := got.result()
			assert.False(t, success)
			assert.True(t, isEgressRejected(err), "only the user can resolve it, so it must not hold the initial sync open: %v", err)
			assert.False(t, dt.NRPResources.NATGateways.Has(uid))
			events := drainEvents(&rec.FakeRecorder)
			if assert.Len(t, events, 1) {
				assert.Contains(t, events[0], egressNATGatewayRejectedReason)
				assert.True(t, strings.HasPrefix(events[0], v1.EventTypeWarning), events[0])
				assert.Equal(t, types.UID("pod-uid"), rec.objects[0].(*v1.Pod).UID, "the event must be on the pod whose annotation decided")
			}
		})
	}
}

// TestServiceUpdaterCreateOutboundService_RetriesWithoutAMemberPod: while no member pod of the identity
// can be read, nothing decides between a managed and a BYO NAT Gateway, so nothing is created in Azure
// and the creation is retried.
func TestServiceUpdaterCreateOutboundService_RetriesWithoutAMemberPod(t *testing.T) {
	const uid = "byo-egress"
	for _, tc := range []struct {
		name    string
		pods    []runtime.Object
		readErr error
		wantErr string
	}{
		{name: "the triggering pod cannot be read", pods: []runtime.Object{byoPod(uid, byoNATGatewayARMID(uid))},
			readErr: errors.New("apiserver down"), wantErr: "apiserver down"},
		{name: "no pod of the identity is left", wantErr: `no active member pod: "byo-egress"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl) // no expectations: any Azure call fails the test

			kube := fake.NewSimpleClientset(tc.pods...)
			if tc.readErr != nil {
				kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, tc.readErr })
			}
			dt := newTestDiffTracker()
			dt.config = testConfig()
			dt.networkClientFactory = m.factory
			dt.kubeClient = kube
			rec := &captureRecorder{FakeRecorder: *record.NewFakeRecorder(10)}
			dt.eventRecorder = rec
			got := &outboundCompletion{}
			outboundUpdater(dt, got).createOutboundService(uid, &OutboundConfig{}, "corr", "ns", "pod")

			_, success, err := got.result()
			assert.False(t, success)
			assert.ErrorContains(t, err, tc.wantErr)
			assert.Equal(t, tc.readErr == nil, errors.Is(err, errNoEgressMember))
			assert.False(t, isEgressRejected(err), "%v", err)
			assert.False(t, isTerminalError(err), "%v", err)
			assert.False(t, dt.NRPResources.NATGateways.Has(uid))
			assert.Empty(t, drainEvents(&rec.FakeRecorder))
		})
	}
}

// TestServiceUpdaterCreateOutboundService_NoBYOLookupWithoutTheAnnotation: an identity whose pod does not
// name a NAT Gateway is managed; no other resource group is read, not even the VNet's own.
func TestServiceUpdaterCreateOutboundService_NoBYOLookupWithoutTheAnnotation(t *testing.T) {
	const uid = "egress-managed"
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	m := newOutboundMocks(ctrl)
	m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, notFoundError()).Times(1) // the cluster resource group only
	m.pip.EXPECT().Get(gomock.Any(), "rg", PublicIPName(uid), nil).Return(nil, notFoundError()).Times(1)
	m.pip.EXPECT().CreateOrUpdate(gomock.Any(), "rg", PublicIPName(uid), gomock.Any()).
		Return(&armnetwork.PublicIPAddress{Name: ptr.To(PublicIPName(uid))}, nil).Times(1)
	m.nat.EXPECT().CreateOrUpdate(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, nil).Times(1)
	m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).Times(1)

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.config.VNetResourceGroup = "vnet-rg"
	dt.networkClientFactory = m.factory
	dt.kubeClient = fake.NewSimpleClientset(byoPod(uid, ""))
	got := &outboundCompletion{}
	outboundUpdater(dt, got).createOutboundService(uid, &OutboundConfig{}, "corr", "ns", "pod")

	_, success, err := got.result()
	assert.True(t, success, "%v", err)
}

// TestServiceUpdaterCreateOutboundService_UsesAFixedAnnotationAfterARejection: a rejected identity is
// retried and reads the annotation again, so a value fixed in place is used without recreating the pods.
// After a link failure that may have been applied, the identity keeps that gateway instead, so its
// deletion can unlink it.
func TestServiceUpdaterCreateOutboundService_UsesAFixedAnnotationAfterARejection(t *testing.T) {
	const uid = "byo-fixed"
	for _, tc := range []struct {
		name string
		// firstGetErr and firstLinkErr answer for the gateway the pod names first; it then names "second".
		firstGetErr  error
		firstLinkErr error
		want         string
	}{
		{name: "after a NAT Gateway that does not exist", firstGetErr: notFoundError(), want: "second"},
		{name: "after a refused link", firstLinkErr: initialNATPutError(&azcore.ResponseError{StatusCode: http.StatusForbidden}), want: "second"},
		{name: "a 403 without request provenance keeps its gateway", firstLinkErr: &azcore.ResponseError{StatusCode: http.StatusForbidden}, want: "first"},
		{name: "a 412 without request provenance keeps its gateway", firstLinkErr: &azcore.ResponseError{StatusCode: http.StatusPreconditionFailed}, want: "first"},
		{name: "a link failure that may have been applied keeps its gateway", firstLinkErr: errors.New("ARM failure"), want: "first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)
			m.nat.EXPECT().Get(gomock.Any(), byoNATGatewayRG, gomock.Any(), gomock.Any()).DoAndReturn(
				func(_ context.Context, _, name string, _ *string) (*armnetwork.NatGateway, error) {
					if name == "first" && tc.firstGetErr != nil {
						return nil, tc.firstGetErr
					}
					return byoNATGateway(name), nil
				}).AnyTimes()
			links := map[string]int{}
			m.nat.EXPECT().CreateOrUpdate(gomock.Any(), byoNATGatewayRG, gomock.Any(), gomock.Any()).DoAndReturn(
				func(_ context.Context, _, name string, n armnetwork.NatGateway) (*armnetwork.NatGateway, error) {
					links[name]++
					if name == "first" && links[name] == 1 && tc.firstLinkErr != nil {
						return nil, tc.firstLinkErr
					}
					return &n, nil
				}).AnyTimes()
			m.sgw.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(nil, nil).AnyTimes()
			m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).AnyTimes()

			dt := newTestDiffTracker()
			dt.config = testConfig()
			dt.networkClientFactory = m.factory
			kube := fake.NewSimpleClientset(byoPod(uid, byoNATGatewayARMID("first")))
			dt.kubeClient = kube
			got := &outboundCompletion{}
			updater := outboundUpdater(dt, got)

			updater.createOutboundService(uid, &OutboundConfig{}, "corr", "ns", "pod")
			_, success, err := got.result()
			if !assert.False(t, success, "the first attempt must fail") {
				return
			}
			assert.False(t, isTerminalError(err), "%v", err)

			_, err = kube.CoreV1().Pods("ns").Update(context.Background(), byoPod(uid, byoNATGatewayARMID("second")), metav1.UpdateOptions{})
			if !assert.NoError(t, err) {
				return
			}
			updater.createOutboundService(uid, &OutboundConfig{}, "corr", "ns", "pod")
			_, success, err = got.result()
			assert.True(t, success, "%v", err)
			assert.Equal(t, byoNATGatewayARMID(tc.want), dt.byoNATGatewayID(uid))
			assert.Equal(t, tc.want == "second", links["second"] > 0, "links of the gateway named second")
		})
	}
}

// TestServiceUpdaterCreateOutboundService_RecordsEventsOnTheDecidingPod: the identity's events go to the
// member pod whose annotation decided, also when the creation has no triggering pod (one started at
// start-up) or its triggering pod left the identity.
func TestServiceUpdaterCreateOutboundService_RecordsEventsOnTheDecidingPod(t *testing.T) {
	const uid = "byo-events"
	named := byoNATGatewayARMID("missing")
	for _, tc := range []struct {
		name     string
		trigger  string
		pods     []runtime.Object
		buffered []string
		known    []string
	}{
		{name: "a creation started at start-up", pods: []runtime.Object{egressTestPod("member", "member-uid", uid, named)}, known: []string{"ns/member"}},
		{name: "a triggering pod that left the identity", trigger: "pod",
			pods:     []runtime.Object{egressTestPod("pod", "pod-uid", "other", ""), egressTestPod("member", "member-uid", uid, named)},
			buffered: []string{"ns/member"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)
			m.nat.EXPECT().Get(gomock.Any(), byoNATGatewayRG, "missing", gomock.Any()).Return(nil, notFoundError()).Times(1)

			dt := newTestDiffTracker()
			dt.config = testConfig()
			dt.networkClientFactory = m.factory
			dt.kubeClient = fake.NewSimpleClientset(tc.pods...)
			for _, key := range tc.buffered {
				dt.pendingPods[uid] = append(dt.pendingPods[uid], PendingPodUpdate{PodKey: key})
			}
			node := newNode()
			for i, key := range tc.known {
				node.Pods[fmt.Sprintf("10.0.0.%d", i+1)] = Pod{PublicOutboundIdentity: uid, OutboundPodKey: key}
			}
			dt.K8sResources.Nodes = map[string]Node{"10.1.0.1": node}
			rec := &captureRecorder{FakeRecorder: *record.NewFakeRecorder(10)}
			dt.eventRecorder = rec
			got := &outboundCompletion{}
			podNS := ""
			if tc.trigger != "" {
				podNS = "ns"
			}
			outboundUpdater(dt, got).createOutboundService(uid, &OutboundConfig{}, "corr", podNS, tc.trigger)

			_, success, err := got.result()
			assert.False(t, success)
			assert.True(t, isEgressRejected(err), "%v", err)
			if assert.Len(t, rec.objects, 1) {
				assert.Equal(t, types.UID("member-uid"), rec.objects[0].(*v1.Pod).UID)
			}
			events := drainEvents(&rec.FakeRecorder)
			if assert.Len(t, events, 1) {
				assert.Contains(t, events[0], "does not exist")
			}
		})
	}
}

// TestEgressMemberPod pins which pod decides the NAT Gateway of an identity and receives its events: the
// first that is still a member (exists, is not being deleted, carries the identity's label), the
// triggering pod read first and alone when it is one; and that a pod that cannot be read is retried,
// never taken as "no annotation" (which would create a managed NAT Gateway).
func TestEgressMemberPod(t *testing.T) {
	const identity = "egress-a"
	member := func(name string) *v1.Pod { return egressTestPod(name, types.UID(name+"-uid"), "Egress-A", "id-"+name) }
	relabelled := func(name string) *v1.Pod { return egressTestPod(name, types.UID(name+"-uid"), "egress-b", "id-other") }
	deleting := member("deleting")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	deleting.Finalizers = []string{"test"}
	for _, tc := range []struct {
		name     string
		pods     []runtime.Object
		trigger  string
		buffered []string
		// known maps the keys of pods in the Kubernetes state (found at start-up) to their identity.
		known map[string]string
		// unreadable is the name of a pod whose read fails.
		unreadable string
		noClient   bool
		want       string
		wantReads  int
		// wantOrder, when set, is the order in which the pods are read.
		wantOrder []string
		wantErr   string
	}{
		{name: "the triggering pod decides and is the only pod read", trigger: "a", pods: []runtime.Object{member("a"), member("b"), member("k")},
			buffered: []string{"ns/b"}, known: map[string]string{"ns/k": identity}, want: "a", wantReads: 1},
		{name: "a buffered pod decides when the triggering pod is gone", trigger: "gone", pods: []runtime.Object{member("b")},
			buffered: []string{"ns/gone", "ns/b"}, want: "b", wantReads: 2},
		{name: "a triggering pod relabelled to another identity no longer decides", trigger: "a",
			pods: []runtime.Object{relabelled("a"), member("b")}, buffered: []string{"ns/b"}, want: "b", wantReads: 2},
		{name: "a triggering pod that lost the label no longer decides", trigger: "a",
			pods: []runtime.Object{egressTestPod("a", "a-uid", "", "id-a"), member("b")}, buffered: []string{"ns/b"}, want: "b", wantReads: 2},
		{name: "a triggering pod being deleted no longer decides", trigger: "deleting", pods: []runtime.Object{deleting, member("b")},
			buffered: []string{"ns/b"}, want: "b", wantReads: 2},
		{name: "buffered pods come before the pods found at start-up", trigger: "gone", pods: []runtime.Object{member("b"), member("a0")},
			buffered: []string{"ns/b"}, known: map[string]string{"ns/a0": identity}, want: "b", wantReads: 2},
		{name: "at start-up, without a triggering pod, the pods are read in name order and the first member decides",
			pods:  []runtime.Object{relabelled("k1"), relabelled("k2"), relabelled("k3"), relabelled("k4"), member("k5"), member("k6")},
			known: map[string]string{"ns/k6": identity, "ns/k3": identity, "ns/k5": identity, "ns/k1": identity, "ns/k4": identity, "ns/k2": identity},
			want:  "k5", wantReads: 5, wantOrder: []string{"k1", "k2", "k3", "k4", "k5"}},
		{name: "a pod both buffered and found at start-up is read once", trigger: "gone",
			pods: []runtime.Object{relabelled("b"), member("c")}, buffered: []string{"ns/b"},
			known: map[string]string{"ns/b": identity, "ns/c": identity}, want: "c", wantReads: 3},
		{name: "pods of another identity are not read", pods: []runtime.Object{member("k1"), relabelled("a0")},
			known: map[string]string{"ns/a0": "egress-b", "ns/k1": identity}, want: "k1", wantReads: 1},
		{name: "no member left is an error", trigger: "gone", wantErr: "no active member pod", wantReads: 1},
		{name: "a pod key without a name is never read", pods: []runtime.Object{member("k1")}, buffered: []string{"ns/"},
			known: map[string]string{"": identity, "ns/k1": identity}, want: "k1", wantReads: 1},
		{name: "a pod that cannot be read is an error", trigger: "a", pods: []runtime.Object{member("a")}, unreadable: "a",
			wantErr: "apiserver down", wantReads: 1},
		{name: "a buffered pod that cannot be read is an error, not skipped for a later pod", trigger: "gone",
			pods: []runtime.Object{member("a"), member("b")}, buffered: []string{"ns/a", "ns/b"}, unreadable: "a", wantErr: "apiserver down", wantReads: 2},
		{name: "without a Kubernetes client nothing decides", trigger: "a", noClient: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := fake.NewSimpleClientset(tc.pods...)
			kube.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				name := action.(k8stesting.GetAction).GetName()
				if name == "" {
					// A real API server refuses an empty name, where the fake would answer NotFound.
					return true, nil, errors.New("resource name may not be empty")
				}
				if name == tc.unreadable {
					return true, nil, errors.New("apiserver down")
				}
				return false, nil, nil
			})
			dt := newTestDiffTracker()
			if !tc.noClient {
				dt.kubeClient = kube
			}
			for _, key := range tc.buffered {
				dt.pendingPods[identity] = append(dt.pendingPods[identity], PendingPodUpdate{PodKey: key})
			}
			node := newNode()
			for key, id := range tc.known {
				node.Pods["10.0.0."+strings.TrimPrefix(key, "ns/")] = Pod{PublicOutboundIdentity: id, OutboundPodKey: key}
			}
			dt.K8sResources.Nodes = map[string]Node{"10.1.0.1": node}

			podNS := ""
			if tc.trigger != "" {
				podNS = "ns"
			}
			got, err := dt.egressMemberPod(context.Background(), identity, podNS, tc.trigger)
			var reads []string
			for _, action := range kube.Actions() {
				if action.GetVerb() == "get" {
					reads = append(reads, action.(k8stesting.GetAction).GetName())
				}
			}
			assert.Len(t, reads, tc.wantReads, "pods read: %v", reads)
			if tc.wantOrder != nil {
				assert.Equal(t, tc.wantOrder, reads, "read order")
			}
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			assert.NoError(t, err)
			if tc.want == "" {
				assert.Nil(t, got)
			} else if assert.NotNil(t, got) {
				assert.Equal(t, tc.want, got.Name)
			}
		})
	}
}

// The triggering pod decides most creation attempts, so it is read before dt.mu is taken: a slow API
// server must not hold up the DiffTracker.
func TestEgressMemberPod_ReadsTheTriggeringPodWithoutTheLock(t *testing.T) {
	dt := newTestDiffTracker()
	dt.kubeClient = fake.NewSimpleClientset(egressTestPod("a", "a-uid", "egress-a", "id-a"))
	dt.mu.Lock()
	defer dt.mu.Unlock()
	result := make(chan *v1.Pod, 1)
	go func() {
		pod, _ := dt.egressMemberPod(context.Background(), "egress-a", "ns", "a")
		result <- pod
	}()
	select {
	case pod := <-result:
		if assert.NotNil(t, pod) {
			assert.Equal(t, "a", pod.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the triggering pod was read only once dt.mu was released")
	}
}

// The identity's other pods are read without dt.mu too: only the copy of their keys is made under it.
func TestEgressMemberPod_ReadsOtherPodsWithoutTheLock(t *testing.T) {
	kube := fake.NewSimpleClientset(egressTestPod("b", "b-uid", "egress-a", "id-b"))
	reading, release := make(chan struct{}), make(chan struct{})
	kube.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetName() == "b" {
			close(reading)
			<-release
		}
		return false, nil, nil
	})
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	dt.pendingPods["egress-a"] = []PendingPodUpdate{{PodKey: "ns/b"}}
	result := make(chan *v1.Pod, 1)
	go func() {
		pod, _ := dt.egressMemberPod(context.Background(), "egress-a", "ns", "gone")
		result <- pod
	}()

	<-reading
	if dt.mu.TryLock() {
		dt.mu.Unlock()
	} else {
		t.Error("dt.mu was held while a buffered pod was read")
	}
	close(release)
	if pod := <-result; assert.NotNil(t, pod) {
		assert.Equal(t, "b", pod.Name)
	}
}

func TestBYONATGatewayIDProblem(t *testing.T) {
	cfg := testConfig()
	netCfg := cfg
	netCfg.NetworkResourceSubscriptionID = "netsub"
	natID := func(sub, rg string) string {
		return "/subscriptions/" + sub + "/resourceGroups/" + rg + "/providers/Microsoft.Network/natGateways/n"
	}
	const notInResourceGroup = "is not the ID of a NAT Gateway in a resource group"
	for _, tc := range []struct {
		id     string
		config Config
		want   string
	}{
		{id: natID("sub", "byo-rg"), config: cfg},
		{id: natID("SUB", "BYO-RG"), config: cfg},
		{id: "/subscriptions/sub/resourceGroups/byo-rg/providers/microsoft.network/NATGATEWAYS/n", config: cfg},
		{id: natID("netsub", "byo-rg"), config: netCfg},
		{id: "n", config: cfg, want: "is not an Azure resource ID"},
		{id: "/subscriptions/sub/resourceGroups/byo-rg/providers/Microsoft.Network/publicIPAddresses/n", config: cfg, want: notInResourceGroup},
		{id: natID("sub", "byo-rg") + "/child/x", config: cfg, want: notInResourceGroup},
		{id: "/subscriptions/sub/providers/Microsoft.Network/natGateways/n", config: cfg, want: notInResourceGroup},
		{id: natID("sub", "byo-rg") + "/providers/Microsoft.Network/natGateways/m", config: cfg, want: notInResourceGroup},
		{id: "/subscriptions/sub/resourceGroups/byo-rg/providers/Microsoft.Resources/deployments/d/providers/Microsoft.Network/natGateways/n", config: cfg,
			want: notInResourceGroup},
		{id: natID("other", "byo-rg"), config: cfg, want: `is not in the cluster's network subscription "sub"`},
		{id: natID("sub", "byo-rg"), config: netCfg, want: `is not in the cluster's network subscription "netsub"`},
		{id: natID("sub", "RG"), config: cfg, want: "is in the cluster resource group"},
	} {
		got := byoNATGatewayIDProblem(tc.id, tc.config)
		if tc.want == "" {
			assert.Empty(t, got, tc.id)
		} else {
			assert.Contains(t, got, tc.want, tc.id)
		}
	}
}

func TestServiceUpdaterDeleteOutboundService_BYONATGateway(t *testing.T) {
	const uid = "byo-egress"
	cfg := testConfig()
	serviceGatewayID := cfg.ServiceGatewayResourceID()
	boom := errors.New("ARM failure")

	for _, tc := range []struct {
		name          string
		linkedTo      string
		natGetErr     error
		servicesErr   error
		unregisterErr error
		linkErr       error
		noETag        bool
		noProperties  bool
		clearErr      error
		// referenceCleared: a previous attempt already cleared the registration's NAT Gateway reference.
		referenceCleared bool
		sharedWith       string
		wantNATCleared   bool
		wantUnregister   bool
		wantSuccess      bool
		// wantWrites is the order of the writes, when checked: NRP refuses the unlink while the
		// registration still references the gateway.
		wantWrites []string
	}{
		{name: "unlinks and unregisters without deleting", linkedTo: strings.ToUpper(serviceGatewayID), wantNATCleared: true, wantUnregister: true, wantSuccess: true,
			wantWrites: []string{"clear the reference", "unlink", "unregister"}},
		{name: "a failed clear of the registration's reference is retried before anything else is written", linkedTo: serviceGatewayID, clearErr: boom,
			wantWrites: []string{"clear the reference"}},
		{name: "a retry after the registration's reference was cleared still unlinks the gateway", linkedTo: serviceGatewayID, referenceCleared: true,
			wantNATCleared: true, wantUnregister: true, wantSuccess: true, wantWrites: []string{"unlink", "unregister"}},
		{name: "gateway linked elsewhere is left alone", linkedTo: "/subscriptions/sub/resourceGroups/x/providers/Microsoft.Network/serviceGateways/other",
			wantUnregister: true, wantSuccess: true},
		{name: "gateway already gone", natGetErr: notFoundError(), wantUnregister: true, wantSuccess: true},
		{name: "gateway shared with the default outbound service keeps its link", linkedTo: serviceGatewayID, sharedWith: "default-natgw",
			wantUnregister: true, wantSuccess: true},
		{name: "unlink failure is retried before unregistering", natGetErr: boom},
		{name: "access denied while unlinking does not block the delete", natGetErr: &azcore.ResponseError{StatusCode: http.StatusForbidden},
			wantUnregister: true, wantSuccess: true},
		{name: "write access denied while unlinking does not block the delete", linkedTo: serviceGatewayID, linkErr: &azcore.ResponseError{StatusCode: http.StatusForbidden},
			wantNATCleared: true, wantUnregister: true, wantSuccess: true},
		{name: "a resource lock while unlinking does not block the delete", linkedTo: serviceGatewayID, linkErr: &azcore.ResponseError{StatusCode: http.StatusConflict, ErrorCode: "ScopeLocked"},
			wantNATCleared: true, wantUnregister: true, wantSuccess: true},
		{name: "a conflict while unlinking is retried", linkedTo: serviceGatewayID, linkErr: &azcore.ResponseError{StatusCode: http.StatusConflict, ErrorCode: "AnotherOperationInProgress"},
			wantNATCleared: true},
		{name: "a stale unlink is retried without unregistering", linkedTo: serviceGatewayID,
			linkErr: &azcore.ResponseError{StatusCode: http.StatusPreconditionFailed}, wantNATCleared: true},
		{name: "missing ETag prevents an unprotected unlink", linkedTo: serviceGatewayID, noETag: true},
		{name: "missing properties leave link state unknown and retain the record", noProperties: true,
			wantWrites: []string{"clear the reference"}},
		{name: "unregister failure is retried", linkedTo: serviceGatewayID, wantNATCleared: true, unregisterErr: boom, wantUnregister: true},
		{name: "already unregistered", linkedTo: serviceGatewayID, wantNATCleared: true, unregisterErr: notFoundError(), wantUnregister: true, wantSuccess: true},
		{name: "Service Gateway gone: still unlinked and released", linkedTo: serviceGatewayID, servicesErr: notFoundError(), wantNATCleared: true,
			unregisterErr: notFoundError(), wantUnregister: true, wantSuccess: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)

			// The identity's own service, listed under another casing, still references the gateway when the
			// unlink starts; it must be found to clear that reference, and must not count as another user.
			services := []*armnetwork.ServiceGatewayService{
				{Name: ptr.To("11111111-1111-1111-1111-111111111111"), Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{ServiceType: ptr.To(armnetwork.ServiceTypeInbound)}},
				{Name: ptr.To(strings.ToUpper(uid)), Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
					ServiceType: ptr.To(armnetwork.ServiceTypeOutbound), PublicNatGatewayID: ptr.To(byoNATGatewayARMID(uid))}},
			}
			if tc.referenceCleared {
				services[1].Properties.PublicNatGatewayID = nil
			}
			if tc.sharedWith != "" {
				services = append(services, &armnetwork.ServiceGatewayService{Name: ptr.To(tc.sharedWith), Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
					IsDefault: ptr.To(true), PublicNatGatewayID: ptr.To(strings.ToUpper(byoNATGatewayARMID(uid)))}})
			}
			if tc.servicesErr != nil {
				services = nil
			}
			m.sgw.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(services, tc.servicesErr).Times(1)
			var natGateway *armnetwork.NatGateway
			if tc.natGetErr == nil {
				natGateway = byoNATGateway(uid)
				natGateway.Properties.ServiceGateway = &armnetwork.SubResource{ID: ptr.To(tc.linkedTo)}
				if tc.noETag {
					natGateway.Etag = nil
				}
				if tc.noProperties {
					natGateway.Properties = nil
				}
			}
			natReads := 1
			if tc.sharedWith != "" || tc.clearErr != nil {
				natReads = 0
			}
			m.nat.EXPECT().Get(gomock.Any(), byoNATGatewayRG, uid, gomock.Any()).Return(natGateway, tc.natGetErr).Times(natReads)
			cleared := false
			var writes []string
			m.nat.EXPECT().CreateOrUpdate(gomock.Any(), byoNATGatewayRG, uid, gomock.Any()).
				DoAndReturn(func(_ context.Context, _, _ string, n armnetwork.NatGateway) (*armnetwork.NatGateway, error) {
					cleared = true
					writes = append(writes, "unlink")
					want := byoNATGateway(uid)
					want.Properties.ServiceGateway = azcore.NullValue[*armnetwork.SubResource]()
					assert.Equal(t, *want, n, "only the link may change on the owner's NAT Gateway")
					return &n, tc.linkErr
				}).AnyTimes()
			unregistered := false
			m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).
				DoAndReturn(func(_ context.Context, _, _ string, req armnetwork.ServiceGatewayUpdateServicesRequest) error {
					if !ptr.Deref(req.ServiceRequests[0].IsDelete, false) {
						writes = append(writes, "clear the reference")
						// Only the identity's own registration, unchanged but for its NAT Gateway reference.
						assert.Equal(t, armnetwork.ServiceGatewayUpdateServicesRequest{
							Action: ptr.To(armnetwork.ServiceUpdateActionPartialUpdate),
							ServiceRequests: []*armnetwork.ServiceGatewayServiceRequest{{IsDelete: ptr.To(false), Service: &armnetwork.ServiceGatewayService{
								Name: ptr.To(strings.ToUpper(uid)), Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
									ServiceType: ptr.To(armnetwork.ServiceTypeOutbound), PublicNatGatewayID: azcore.NullValue[*string]()}}}},
						}, req)
						return tc.clearErr
					}
					writes = append(writes, "unregister")
					unregistered = true
					return tc.unregisterErr
				}).AnyTimes()

			dt := newOutboundDiffTracker(uid, m, fake.NewSimpleClientset())
			dt.config = testConfig()
			dt.setBYONATGatewayID(uid, byoNATGatewayARMID(uid))
			got := &outboundCompletion{}
			outboundUpdater(dt, got).deleteOutboundService(uid, "corr")

			_, success, err := got.result()
			assert.Equal(t, tc.wantSuccess, success, "completion error: %v", err)
			assert.Equal(t, tc.wantNATCleared, cleared, "NAT Gateway link cleared")
			assert.Equal(t, tc.wantUnregister, unregistered, "unregistered")
			if tc.wantWrites != nil {
				assert.Equal(t, tc.wantWrites, writes)
			}
			assert.Equal(t, !tc.wantSuccess, dt.NRPResources.NATGateways.Has(uid))
			if tc.wantSuccess {
				assert.Empty(t, dt.byoNATGatewayID(uid))
			} else {
				assert.Equal(t, byoNATGatewayARMID(uid), dt.byoNATGatewayID(uid), "a failed delete must keep the BYO gateway so the retry unlinks it")
			}
		})
	}
}

// A delete that unlinked the gateway but could not release the last pod's finalizer is retried; the
// retry must still take the unlink-only path, never the managed path that deletes by name.
func TestServiceUpdaterDeleteOutboundService_BYONATGatewayKeptUntilFinalizersReleased(t *testing.T) {
	const (
		uid     = "byo-finalizer"
		podNS   = "default"
		podName = "egress-last-pod"
	)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	m := newOutboundMocks(ctrl)
	m.expectNoDisassociation()
	m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).Times(1)

	kube := fake.NewSimpleClientset(&v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: podName, Namespace: podNS, UID: "pod-uid", Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
	}})
	kube.PrependReactor("update", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver down")
	})
	dt := newOutboundDiffTracker(uid, m, kube)
	dt.config = testConfig()
	dt.setBYONATGatewayID(uid, byoNATGatewayARMID(uid))
	dt.pendingPodDeletions[podNS+"/"+podName] = &PendingPodDeletion{
		Namespace: podNS, Name: podName, UID: "pod-uid", ServiceUID: uid, Addresses: []string{"10.244.0.5"}, IsLastPod: true,
	}
	got := &outboundCompletion{}
	outboundUpdater(dt, got).deleteOutboundService(uid, "corr")

	_, success, _ := got.result()
	assert.False(t, success, "a failed finalizer release must be retried")
	assert.Equal(t, byoNATGatewayARMID(uid), dt.byoNATGatewayID(uid), "the retry must still take the unlink-only path")
}

// A pod labelled with the name of a NAT Gateway this identity does not own (the default outbound
// service's, which may carry any name, or any other one this controller did not create) must never
// make this controller delete that gateway or its addresses.
func TestServiceUpdaterDeleteOutboundService_KeepsNATGatewayNotOwnedByIdentity(t *testing.T) {
	const uid = "custom-default"
	usedByDefault := []*armnetwork.ServiceGatewayService{{
		Name: ptr.To("default-natgw"),
		Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
			IsDefault: ptr.To(true), PublicNatGatewayID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/natGateways/" + uid)},
	}}
	// Untagged, only its own "<name>-pip", as this controller created NAT Gateways before it tagged
	// them: not ours.
	ownAddressesOnly := &armnetwork.NatGateway{Name: ptr.To(uid), Properties: &armnetwork.NatGatewayPropertiesFormat{PublicIPAddresses: []*armnetwork.SubResource{
		{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/" + PublicIPName(uid))}}}}
	tagged := &armnetwork.NatGateway{Name: ptr.To(uid), Tags: egressIdentityTags(uid), Properties: &armnetwork.NatGatewayPropertiesFormat{}}
	// Linked to our Service Gateway (for example mid-attach by the RP), yet not ours: never written.
	foreign := &armnetwork.NatGateway{Name: ptr.To(uid), Properties: &armnetwork.NatGatewayPropertiesFormat{
		ServiceGateway: &armnetwork.SubResource{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/serviceGateways/sgw")}}}
	// Untagged, given a prefix by its user.
	userModified := &armnetwork.NatGateway{Name: ptr.To(uid), Properties: &armnetwork.NatGatewayPropertiesFormat{
		PublicIPAddresses: ownAddressesOnly.Properties.PublicIPAddresses,
		PublicIPPrefixes:  []*armnetwork.SubResource{{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPPrefixes/user")}}}}

	for _, tc := range []struct {
		name        string
		services    []*armnetwork.ServiceGatewayService
		servicesErr error
		gateway     *armnetwork.NatGateway
		unmanaged   bool
		rejected    bool
		reserved    bool
		wantRetry   bool
	}{
		{name: "untagged with only its own <name>-pip, as created before tagging", gateway: ownAddressesOnly},
		{name: "tagged as ours but used by the default outbound service", services: usedByDefault, gateway: tagged},
		{name: "tagged as ours but its users unreadable: retried, nothing deleted", gateway: tagged, servicesErr: errors.New("NRP unavailable"), wantRetry: true},
		{name: "created after start-up by someone else", gateway: foreign},
		{name: "recorded at start-up as not ours, creation rejected: live ownership keeps the gateway", unmanaged: true, rejected: true, gateway: userModified},
		{name: "recorded at start-up as not ours but registered: only its registration is removed", unmanaged: true, gateway: userModified},
		{name: "RP-owned service identity is never cleaned up", reserved: true, rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)
			unregistered := 0
			if !tc.reserved {
				m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(tc.gateway, nil).AnyTimes()
				m.sgw.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(tc.services, tc.servicesErr).AnyTimes()
				m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).
					DoAndReturn(func(_ context.Context, _, _ string, req armnetwork.ServiceGatewayUpdateServicesRequest) error {
						unregistered++
						assert.True(t, ptr.Deref(req.ServiceRequests[0].IsDelete, false), "only the identity's own registration may be removed")
						return nil
					}).AnyTimes()
			}
			// No NAT Gateway write and no deletes: any such call fails the test.

			dt := newOutboundDiffTracker(uid, m, fake.NewSimpleClientset())
			if tc.unmanaged {
				dt.NRPResources.UnmanagedNATGateways = utilsets.NewString(uid)
			}
			if tc.reserved {
				dt.NRPResources.ReservedOutboundServices = utilsets.NewString(uid)
			}
			if tc.rejected {
				dt.NRPResources.NATGateways.Delete(uid)
			}
			got := &outboundCompletion{}
			outboundUpdater(dt, got).deleteOutboundService(uid, "corr")

			_, success, err := got.result()
			assert.Equal(t, !tc.wantRetry, success, "%v", err)
			assert.Equal(t, tc.wantRetry, dt.NRPResources.NATGateways.Has(uid))
			wantUnregistered := 1
			if tc.reserved || tc.wantRetry {
				wantUnregistered = 0
			}
			assert.Equal(t, wantUnregistered, unregistered, "the identity's own registration must still be removed")
		})
	}
}

// TestServiceUpdaterDeleteOutboundService_DeletesItsOwnNATGateway: deleting a managed identity deletes
// its own (tagged) NAT Gateway and both Public IPs, also when its own registration still names the
// gateway or the Service Gateway is gone; while the gateway's users cannot be read, nothing is deleted.
func TestServiceUpdaterDeleteOutboundService_DeletesItsOwnNATGateway(t *testing.T) {
	const uid = "managed-egress"
	own := &armnetwork.NatGateway{Name: ptr.To(uid), Tags: egressIdentityTags(uid), Properties: &armnetwork.NatGatewayPropertiesFormat{}}
	for _, tc := range []struct {
		name          string
		services      []*armnetwork.ServiceGatewayService
		servicesErr   error
		wantDeleted   bool
		wantSuccess   bool
		staleSnapshot bool
	}{
		{name: "nothing registered any more: deleted", wantDeleted: true, wantSuccess: true},
		{name: "a stale unmanaged snapshot cannot veto live ownership", staleSnapshot: true, wantDeleted: true, wantSuccess: true},
		{name: "its own registration still names it: deleted", services: []*armnetwork.ServiceGatewayService{{Name: ptr.To(uid),
			Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
				PublicNatGatewayID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/natGateways/" + uid)}}},
			wantDeleted: true, wantSuccess: true},
		{name: "its users cannot be read: retried, nothing deleted", servicesErr: errors.New("NRP unavailable")},
		{name: "the Service Gateway is gone: deleted", servicesErr: &azcore.ResponseError{StatusCode: http.StatusNotFound}, wantDeleted: true, wantSuccess: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)
			m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(own, nil).AnyTimes()
			m.sgw.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(tc.services, tc.servicesErr).AnyTimes()
			m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).AnyTimes()
			if tc.wantDeleted {
				m.nat.EXPECT().Delete(gomock.Any(), "rg", uid).Return(nil).Times(1)
				m.expectOwnedPublicIPs(uid, 1)
				m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName(uid)).Return(nil).Times(1)
				m.pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPNameV6(uid)).Return(nil).Times(1)
			}

			dt := newOutboundDiffTracker(uid, m, fake.NewSimpleClientset())
			if tc.staleSnapshot {
				dt.NRPResources.UnmanagedNATGateways = utilsets.NewString(uid)
				dt.NRPResources.NATGateways.Delete(uid)
			}
			got := &outboundCompletion{}
			outboundUpdater(dt, got).deleteOutboundService(uid, "corr")

			_, success, err := got.result()
			assert.Equal(t, tc.wantSuccess, success, "%v", err)
		})
	}
}

// TestServiceUpdaterCreateOutboundService_RejectsClusterNATGatewayNotOurs: creating a managed identity
// whose name is taken in the cluster resource group by a NAT Gateway that is not ours (untagged, also
// one created before tagging) or that another service uses (the default outbound service) is rejected
// rather than overwrite that gateway.
func TestServiceUpdaterCreateOutboundService_RejectsClusterNATGatewayNotOurs(t *testing.T) {
	const uid = "custom-default"
	ownAddresses := &armnetwork.NatGatewayPropertiesFormat{PublicIPAddresses: []*armnetwork.SubResource{
		{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/" + PublicIPName(uid))}}}
	usedByDefault := []*armnetwork.ServiceGatewayService{{Name: ptr.To("default-natgw"), Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
		IsDefault: ptr.To(true), PublicNatGatewayID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/natGateways/" + uid)}}}
	for _, tc := range []struct {
		name     string
		gateway  *armnetwork.NatGateway
		services []*armnetwork.ServiceGatewayService
	}{
		{name: "untagged with only its own address, as created before tagging", gateway: &armnetwork.NatGateway{Name: ptr.To(uid), Properties: ownAddresses}},
		{name: "tagged as ours but used by the default outbound service", services: usedByDefault,
			gateway: &armnetwork.NatGateway{Name: ptr.To(uid), Tags: egressIdentityTags(uid), Properties: ownAddresses}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			m := newOutboundMocks(ctrl)
			m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(tc.gateway, nil)
			if tc.services != nil {
				m.sgw.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(tc.services, nil)
			}
			// No writes: any PUT or registration fails the test.

			dt := newTestDiffTracker()
			dt.config = testConfig()
			dt.networkClientFactory = m.factory
			dt.kubeClient = fake.NewSimpleClientset(byoPod(uid, ""))
			rec := &captureRecorder{FakeRecorder: *record.NewFakeRecorder(10)}
			dt.eventRecorder = rec
			got := &outboundCompletion{}
			outboundUpdater(dt, got).createOutboundService(uid, &OutboundConfig{}, "corr", "ns", "pod")

			_, success, err := got.result()
			assert.False(t, success)
			assert.ErrorIs(t, err, errNATGatewayNotOwned)
			events := drainEvents(&rec.FakeRecorder)
			if assert.Len(t, events, 1) {
				assert.Contains(t, events[0], egressNATGatewayRejectedReason)
			}
		})
	}
}

// A BYO unlink left behind because of missing access or a lock is counted like a swallowed managed
// disassociation failure, so a sustained problem is visible.
func TestDeleteBYOOutboundService_CountsSwallowedUnlinkFailure(t *testing.T) {
	const uid = "byo-metric"
	run := func(t *testing.T, readErr error) float64 {
		t.Helper()
		RegisterMetrics()
		deleteSubstepFailuresTotal.Reset()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		m := newOutboundMocks(ctrl)
		m.sgw.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(nil, nil)
		m.nat.EXPECT().Get(gomock.Any(), byoNATGatewayRG, uid, gomock.Any()).Return(nil, readErr)
		m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).AnyTimes()

		dt := newOutboundDiffTracker(uid, m, fake.NewSimpleClientset())
		dt.config = testConfig()
		dt.setBYONATGatewayID(uid, byoNATGatewayARMID(uid))
		NewServiceUpdater(context.Background(), dt, func(string, bool, error) {}, dt.serviceUpdaterTrigger).deleteOutboundService(uid, "corr")

		got, err := testutil.GetCounterMetricValue(deleteSubstepFailuresTotal.WithLabelValues(deleteStepDisassociateNAT))
		assert.NoError(t, err)
		return got
	}

	assert.Equal(t, float64(1), run(t, &azcore.ResponseError{StatusCode: http.StatusForbidden}), "a left-behind link must be counted")
	assert.Zero(t, run(t, errors.New("transient")), "control: a retried unlink failure is not a left-behind link")
	assert.Zero(t, run(t, notFoundError()), "control: an already-gone gateway counts nothing")
}

// captureRecorder keeps the objects events were recorded on, and passes the events on to the
// embedded FakeRecorder.
type captureRecorder struct {
	record.FakeRecorder
	objects []runtime.Object
}

func (r *captureRecorder) Event(object runtime.Object, eventType, reason, message string) {
	r.objects = append(r.objects, object)
	r.FakeRecorder.Event(object, eventType, reason, message)
}

// With network resources in their own subscription, a cluster-resource-group NAT Gateway ID carries
// that subscription: another service using it must still be recognised, on delete and on unlink.
func TestOutboundServiceWithSeparateNetworkSubscription(t *testing.T) {
	const uid = "custom-default"
	defaultUsing := func(natGatewayID string) []*armnetwork.ServiceGatewayService {
		return []*armnetwork.ServiceGatewayService{{Name: ptr.To("default-natgw"), Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
			IsDefault: ptr.To(true), PublicNatGatewayID: ptr.To(natGatewayID)}}}
	}
	netConfig := func(c Config) Config {
		c.NetworkResourceSubscriptionID = "netsub"
		return c
	}

	t.Run("managed delete keeps a gateway the default uses", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		m := newOutboundMocks(ctrl)
		m.nat.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(&armnetwork.NatGateway{Name: ptr.To(uid), Tags: egressIdentityTags(uid)}, nil).AnyTimes()
		m.sgw.EXPECT().GetServices(gomock.Any(), "rg", "sgw").
			Return(defaultUsing("/subscriptions/netsub/resourceGroups/rg/providers/Microsoft.Network/natGateways/"+uid), nil).AnyTimes()
		m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).AnyTimes()
		// No NAT Gateway write and no deletes.

		dt := newOutboundDiffTracker(uid, m, fake.NewSimpleClientset())
		dt.config = netConfig(dt.config)
		got := &outboundCompletion{}
		outboundUpdater(dt, got).deleteOutboundService(uid, "corr")
		_, success, err := got.result()
		assert.True(t, success, "%v", err)
	})

	t.Run("BYO unlink keeps a gateway the default uses", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		m := newOutboundMocks(ctrl)
		byoID := "/subscriptions/netsub/resourceGroups/" + byoNATGatewayRG + "/providers/Microsoft.Network/natGateways/" + uid
		m.sgw.EXPECT().GetServices(gomock.Any(), "rg", "sgw").Return(defaultUsing(byoID), nil).Times(1)
		m.sgw.EXPECT().UpdateServices(gomock.Any(), "rg", "sgw", gomock.Any()).Return(nil).Times(1)
		// No NAT Gateway read or write: the gateway stays linked for the default.

		dt := newOutboundDiffTracker(uid, m, fake.NewSimpleClientset())
		dt.config = netConfig(testConfig())
		dt.setBYONATGatewayID(uid, byoID)
		got := &outboundCompletion{}
		outboundUpdater(dt, got).deleteOutboundService(uid, "corr")
		_, success, err := got.result()
		assert.True(t, success, "%v", err)
	})
}
