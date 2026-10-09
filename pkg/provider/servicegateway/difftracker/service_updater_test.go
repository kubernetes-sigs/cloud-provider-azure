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
	"maps"
	"net/http"
	"slices"
	"strings"
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
	"k8s.io/client-go/tools/record"
	servicehelper "k8s.io/cloud-provider/service/helpers"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/natgatewayclient/mock_natgatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipprefixclient/mock_publicipprefixclient"
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
	mockLB.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	m.expectNoDisassociation()
	m.pip.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
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
	assert.NotNil(t, dt.pendingServiceOps["not-started"].AttemptedCreateConfig,
		"the dispatched create config must be remembered across failed attempts")
	assert.Equal(t, dt.pendingServiceOps["not-started"].InFlightConfig, dt.pendingServiceOps["not-started"].AttemptedCreateConfig)

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
	mockLB.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	f.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	f.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()

	// PIP returns a populated response so pipIPAddress is non-empty and Step 5 actually runs.
	mockPIP.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
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
	mockLB.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	f.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	f.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()

	mockPIP.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
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

func TestServiceUpdaterCreateInboundService_ChecksPublicIPPrefix(t *testing.T) {
	const uid = "44444444-4444-4444-4444-444444444444"
	var events []string
	var pipReadErr error
	prefix := func(sku armnetwork.PublicIPPrefixSKUName, version armnetwork.IPVersion, location string) *armnetwork.PublicIPPrefix {
		return &armnetwork.PublicIPPrefix{
			Location:   ptr.To(location),
			SKU:        &armnetwork.PublicIPPrefixSKU{Name: ptr.To(sku)},
			Properties: &armnetwork.PublicIPPrefixPropertiesFormat{PublicIPAddressVersion: ptr.To(version)},
		}
	}
	run := func(t *testing.T, prefixID string, got *armnetwork.PublicIPPrefix, getErr error, expectRead bool, existing ...*armnetwork.PublicIPAddress) (created *armnetwork.PublicIPAddress, success bool, deleted []string, err error) {
		ctrl := gomock.NewController(t)
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default", UID: types.UID(uid)},
			Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		}
		f := mock_azclient.NewMockClientFactory(ctrl)
		mockPrefix := mock_publicipprefixclient.NewMockInterface(ctrl)
		mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		mockLB.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
		mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
		f.EXPECT().GetPublicIPPrefixClient().Return(mockPrefix).AnyTimes()
		f.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
		f.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		f.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
		if expectRead {
			mockPrefix.EXPECT().Get(gomock.Any(), "rg", "prefix", gomock.Any()).Return(got, getErr).AnyTimes()
		}
		if len(existing) > 0 {
			mockPIP.EXPECT().Get(gomock.Any(), "rg", PublicIPName(uid), gomock.Any()).Return(existing[0], nil).AnyTimes()
		} else {
			readErr := notFoundError()
			if pipReadErr != nil {
				readErr = pipReadErr
			}
			mockPIP.EXPECT().Get(gomock.Any(), "rg", PublicIPName(uid), gomock.Any()).Return(nil, readErr).AnyTimes()
		}
		mockPIP.EXPECT().CreateOrUpdate(gomock.Any(), "rg", PublicIPName(uid), gomock.Any()).
			DoAndReturn(func(_ context.Context, _, _ string, pip armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
				created = &pip
				pip.Properties.IPAddress = ptr.To("20.0.0.1")
				return &pip, nil
			}).MaxTimes(1)
		mockPIP.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, rg, name string) error {
			deleted = append(deleted, rg+"/"+name)
			return nil
		}).AnyTimes()
		mockLB.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
		mockSGW.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		dt := newTestDiffTracker()
		dt.config = testConfig()
		dt.kubeClient = fake.NewSimpleClientset(svc)
		dt.networkClientFactory = f
		recorder := record.NewFakeRecorder(10)
		dt.SetEventRecorder(recorder)
		t.Cleanup(func() {
			close(recorder.Events)
			for event := range recorder.Events {
				events = append(events, event)
			}
		})
		config := makeInboundConfig(80)
		config.PIPPrefixID = prefixID
		su := newTestServiceUpdater(dt)
		su.onComplete = func(_ string, ok bool, e error) { success, err = ok, e }
		su.createInboundService(uid, config, "corr")
		return created, success, deleted, err
	}

	t.Run("a matching StandardV2 prefix is used", func(t *testing.T) {
		created, success, deleted, err := run(t, testPrefixID, prefix(armnetwork.PublicIPPrefixSKUNameStandardV2, armnetwork.IPVersionIPv4, "East US"), nil, true)
		assert.True(t, success, "%v", err)
		assert.Empty(t, deleted)
		if assert.NotNil(t, created) {
			assert.Equal(t, testPrefixID, *created.Properties.PublicIPPrefix.ID)
		}
	})

	for name, tc := range map[string]*armnetwork.PublicIPPrefix{
		"a Standard prefix":        prefix(armnetwork.PublicIPPrefixSKUNameStandard, armnetwork.IPVersionIPv4, "eastus"),
		"an IPv6 prefix":           prefix(armnetwork.PublicIPPrefixSKUNameStandardV2, armnetwork.IPVersionIPv6, "eastus"),
		"a prefix in other region": prefix(armnetwork.PublicIPPrefixSKUNameStandardV2, armnetwork.IPVersionIPv4, "westus"),
	} {
		t.Run(name+" is rejected terminally before creating the Public IP", func(t *testing.T) {
			created, success, deleted, err := run(t, testPrefixID, tc, nil, true)
			assert.False(t, success)
			assert.True(t, isTerminalError(err), "%v", err)
			assert.Nil(t, created)
			assert.Empty(t, deleted)
		})
	}

	t.Run("a failed Public IP read is retried without creating one", func(t *testing.T) {
		pipReadErr = errors.New("throttled")
		defer func() { pipReadErr = nil }()
		created, success, deleted, err := run(t, testPrefixID, nil, nil, false)
		assert.False(t, success)
		assert.False(t, isTerminalError(err))
		assert.Nil(t, created)
		assert.Empty(t, deleted)
	})

	t.Run("a failed prefix read is retried", func(t *testing.T) {
		created, success, deleted, err := run(t, testPrefixID, nil, notFoundError(), true)
		assert.False(t, success)
		assert.False(t, isTerminalError(err))
		assert.Nil(t, created)
		assert.Empty(t, deleted)
	})

	t.Run("a retry keeps an unattached owned Public IP from the same prefix in another case", func(t *testing.T) {
		existing := &armnetwork.PublicIPAddress{
			Name: ptr.To(PublicIPName(uid)),
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{
				IPAddress:      ptr.To("20.0.0.9"),
				PublicIPPrefix: &armnetwork.SubResource{ID: ptr.To(strings.Replace(testPrefixID, "/resourceGroups/rg/", "/resourceGroups/RG/", 1))},
			},
		}
		var deleted []string
		var success bool
		var err error
		t.Run("run", func(t *testing.T) {
			_, success, deleted, err = run(t, testPrefixID, prefix(armnetwork.PublicIPPrefixSKUNameStandardV2, armnetwork.IPVersionIPv4, "eastus"), nil, true, existing)
		})
		assert.True(t, success, "%v", err)
		assert.Empty(t, deleted, "the address must be kept")
	})

	t.Run("a retry recreates an unattached owned Public IP from the requested prefix", func(t *testing.T) {
		otherPrefix := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPPrefixes/other"
		existing := &armnetwork.PublicIPAddress{
			Name: ptr.To(PublicIPName(uid)),
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{
				IPAddress:      ptr.To("20.0.0.9"),
				PublicIPPrefix: &armnetwork.SubResource{ID: ptr.To(otherPrefix)},
			},
		}
		events = nil
		var created *armnetwork.PublicIPAddress
		var success bool
		var err error
		var deleted []string
		t.Run("run", func(t *testing.T) {
			created, success, deleted, err = run(t, testPrefixID, prefix(armnetwork.PublicIPPrefixSKUNameStandardV2, armnetwork.IPVersionIPv4, "eastus"), nil, true, existing)
		})

		t.Run("an invalid requested prefix does not delete an unattached owned Public IP", func(t *testing.T) {
			otherPrefix := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPPrefixes/other"
			existing := &armnetwork.PublicIPAddress{
				Name: ptr.To(PublicIPName(uid)),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress:              ptr.To("20.0.0.9"),
					PublicIPAddressVersion: ptr.To(armnetwork.IPVersionIPv4),
					PublicIPPrefix:         &armnetwork.SubResource{ID: ptr.To(otherPrefix)},
				},
			}
			for name, requested := range map[string]*armnetwork.PublicIPPrefix{
				"wrong SKU":        prefix(armnetwork.PublicIPPrefixSKUNameStandard, armnetwork.IPVersionIPv4, "eastus"),
				"wrong IP version": prefix(armnetwork.PublicIPPrefixSKUNameStandardV2, armnetwork.IPVersionIPv6, "eastus"),
			} {
				created, success, deleted, err := run(t, testPrefixID, requested, nil, true, existing)
				assert.False(t, success, name)
				assert.True(t, isTerminalError(err), "%s: %v", name, err)
				assert.Nil(t, created, name)
				assert.Empty(t, deleted, name)
			}
		})
		assert.True(t, success, "%v", err)
		if assert.NotNil(t, created) {
			assert.Equal(t, testPrefixID, *created.Properties.PublicIPPrefix.ID, "the unattached owned PIP must be recreated from the requested prefix")
		}
		assert.Equal(t, []string{"rg/" + PublicIPName(uid)}, deleted)
		assert.Empty(t, events)
	})

	t.Run("a retry rewrites a Public IP an earlier attempt left failed", func(t *testing.T) {
		failed := &armnetwork.PublicIPAddress{
			Name: ptr.To(PublicIPName(uid)),
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{
				ProvisioningState: ptr.To(armnetwork.ProvisioningStateFailed),
				PublicIPPrefix:    &armnetwork.SubResource{ID: ptr.To(testPrefixID)},
			},
		}
		created, success, deleted, err := run(t, testPrefixID, nil, nil, false, failed)
		assert.True(t, success, "%v", err)
		assert.Empty(t, deleted)
		if assert.NotNil(t, created, "a failed Public IP must be written again") {
			assert.Equal(t, testPrefixID, *created.Properties.PublicIPPrefix.ID)
		}
	})

	t.Run("a prefix in another subscription is left to Azure", func(t *testing.T) {
		otherSub := "/subscriptions/other/resourceGroups/rg/providers/Microsoft.Network/publicIPPrefixes/prefix"
		created, success, deleted, err := run(t, otherSub, nil, nil, false)
		assert.True(t, success, "%v", err)
		assert.Empty(t, deleted)
		if assert.NotNil(t, created) {
			assert.Equal(t, otherSub, *created.Properties.PublicIPPrefix.ID)
		}
	})
}

func TestServiceUpdaterUpdateInboundService_ReconcilesPublicIP(t *testing.T) {
	const uid = "33333333-3333-3333-3333-333333333333"
	config := func() *InboundConfig {
		return &InboundConfig{
			FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
			BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
			ServiceName:   "ns/svc",
			ClusterName:   "cluster",
			PIPTags:       map[string]string{"team": "a"},
			DNSLabel:      ptr.To("app"),
		}
	}
	existing := func() *armnetwork.PublicIPAddress {
		return &armnetwork.PublicIPAddress{
			Name: ptr.To(PublicIPName(uid)),
			Tags: map[string]*string{"Team": ptr.To("old"), "policy": ptr.To("keep")},
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{
				IPAddress: ptr.To("20.0.0.1"),
				IPTags:    []*armnetwork.IPTag{{IPTagType: ptr.To("RoutingPreference"), Tag: ptr.To("Internet")}},
			},
		}
	}
	var putErr error
	var lbCalls int
	run := func(t *testing.T, cfg *InboundConfig, current *armnetwork.PublicIPAddress, getErr error) (put *armnetwork.PublicIPAddress, success bool, recorder *record.FakeRecorder, opErr error) {
		ctrl := gomock.NewController(t)
		m := newOutboundMocks(ctrl)
		m.pip.EXPECT().Get(gomock.Any(), "rg", PublicIPName(uid), gomock.Any()).Return(current, getErr)
		m.pip.EXPECT().CreateOrUpdate(gomock.Any(), "rg", PublicIPName(uid), gomock.Any()).
			DoAndReturn(func(_ context.Context, _, _ string, pip armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
				put = &pip
				return &pip, putErr
			}).MaxTimes(1)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		expectCurrentLB(mockLB, uid)
		lbCalls = 0
		mockLB.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
			DoAndReturn(func(_ context.Context, _, _ string, _ armnetwork.LoadBalancer) (*armnetwork.LoadBalancer, error) {
				lbCalls++
				return nil, nil
			}).AnyTimes()

		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: types.UID(uid)},
			Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		}
		for _, family := range cfg.IPFamilies {
			svc.Spec.IPFamilies = append(svc.Spec.IPFamilies, v1.IPFamily(family))
		}
		dt := newTestDiffTracker()
		dt.config = testConfig()
		dt.networkClientFactory = m.factory
		dt.kubeClient = fake.NewSimpleClientset(svc)
		recorder = record.NewFakeRecorder(10)
		dt.SetEventRecorder(recorder)
		got := &outboundCompletion{}
		outboundUpdater(dt, got).updateInboundService(uid, cfg, "corr")
		_, success, opErr = got.result()
		return put, success, recorder, opErr
	}

	t.Run("adds missing tags and the DNS label, keeping foreign tags and IP tags", func(t *testing.T) {
		put, success, _, _ := run(t, config(), existing(), nil)
		assert.True(t, success)
		if assert.NotNil(t, put) {
			assert.Equal(t, map[string]*string{
				"Team":                ptr.To("a"),
				"policy":              ptr.To("keep"),
				consts.ServiceTagKey:  ptr.To("ns/svc"),
				consts.ClusterNameKey: ptr.To("cluster"),
			}, put.Tags)
			assert.Equal(t, "app", *put.Properties.DNSSettings.DomainNameLabel)
			assert.Equal(t, existing().Properties.IPTags, put.Properties.IPTags)
		}
	})

	t.Run("an up-to-date Public IP is not written", func(t *testing.T) {
		current := existing()
		current.Tags = map[string]*string{"team": ptr.To("a"), consts.ServiceTagKey: ptr.To("ns/svc"), consts.ClusterNameKey: ptr.To("cluster")}
		current.Properties.DNSSettings = &armnetwork.PublicIPAddressDNSSettings{DomainNameLabel: ptr.To("app")}
		put, success, _, _ := run(t, config(), current, nil)
		assert.True(t, success)
		assert.Nil(t, put)
	})

	t.Run("an empty DNS label removes it", func(t *testing.T) {
		cfg := config()
		cfg.DNSLabel = ptr.To("")
		current := existing()
		current.Properties.DNSSettings = &armnetwork.PublicIPAddressDNSSettings{DomainNameLabel: ptr.To("app")}
		put, success, _, _ := run(t, cfg, current, nil)
		assert.True(t, success)
		if assert.NotNil(t, put) {
			assert.Nil(t, put.Properties.DNSSettings)
		}
	})

	t.Run("a FirstPartyUsage IP tag change is applied in place", func(t *testing.T) {
		cfg := config()
		cfg.IPTags = map[string]string{"FirstPartyUsage": "/NonProd"}
		current := existing()
		current.Properties.IPTags = []*armnetwork.IPTag{{IPTagType: ptr.To("FirstPartyUsage"), Tag: ptr.To("/Prod")}}
		put, success, recorder, _ := run(t, cfg, current, nil)
		assert.True(t, success)
		if assert.NotNil(t, put) {
			assert.Equal(t, map[string]string{"FirstPartyUsage": "/NonProd"}, ipTagMap(put.Properties.IPTags))
		}
		select {
		case event := <-recorder.Events:
			t.Fatalf("unexpected event: %s", event)
		default:
		}
	})

	t.Run("an empty IP tag annotation removes the IP tags", func(t *testing.T) {
		cfg := config()
		cfg.IPTags = map[string]string{}
		current := existing()
		current.Properties.IPTags = []*armnetwork.IPTag{{IPTagType: ptr.To("FirstPartyUsage"), Tag: ptr.To("/Prod")}}
		put, success, _, _ := run(t, cfg, current, nil)
		assert.True(t, success)
		if assert.NotNil(t, put) {
			assert.Empty(t, put.Properties.IPTags)
		}
	})

	t.Run("a non-FirstPartyUsage IP tag change is rejected", func(t *testing.T) {
		cfg := config()
		cfg.IPTags = map[string]string{"RoutingPreference": "MicrosoftNetwork"}
		put, success, _, err := run(t, cfg, existing(), nil)
		assert.False(t, success)
		assert.True(t, isTerminalError(err), "%v", err)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "only FirstPartyUsage IP tags can be changed on an existing Public IP")
			assert.Contains(t, err.Error(), "RoutingPreference")
		}
		assert.Nil(t, put)
		assert.Zero(t, lbCalls)
	})

	t.Run("adding a non-FirstPartyUsage IP tag is rejected", func(t *testing.T) {
		cfg := config()
		cfg.IPTags = map[string]string{"RoutingPreference": "Internet"}
		current := existing()
		current.Properties.IPTags = nil
		put, success, _, err := run(t, cfg, current, nil)
		assert.False(t, success)
		assert.True(t, isTerminalError(err), "%v", err)
		assert.Nil(t, put)
		assert.Zero(t, lbCalls)
	})

	t.Run("removing a non-FirstPartyUsage IP tag is rejected", func(t *testing.T) {
		cfg := config()
		cfg.IPTags = map[string]string{}
		put, success, _, err := run(t, cfg, existing(), nil)
		assert.False(t, success)
		assert.True(t, isTerminalError(err), "%v", err)
		assert.Nil(t, put)
		assert.Zero(t, lbCalls)
	})

	t.Run("an IP tag change on a Public IP from a prefix is rejected", func(t *testing.T) {
		cfg := config()
		cfg.IPTags = map[string]string{}
		current := existing()
		current.Properties.PublicIPPrefix = &armnetwork.SubResource{ID: ptr.To(testPrefixID)}
		put, success, _, err := run(t, cfg, current, nil)
		assert.False(t, success)
		assert.True(t, isTerminalError(err), "%v", err)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "the IP tags of a Public IP allocated from a prefix cannot be changed")
		}
		assert.Nil(t, put)
		assert.Zero(t, lbCalls)
	})

	t.Run("matching IP tags on a Public IP from a prefix are accepted", func(t *testing.T) {
		cfg := config()
		cfg.IPTags = map[string]string{"RoutingPreference": "Internet"}
		current := existing()
		current.Properties.PublicIPPrefix = &armnetwork.SubResource{ID: ptr.To(testPrefixID)}
		put, success, _, err := run(t, cfg, current, nil)
		assert.True(t, success)
		assert.NoError(t, err)
		if assert.NotNil(t, put) {
			assert.Equal(t, existing().Properties.IPTags, put.Properties.IPTags)
		}
	})

	t.Run("an absent DNS annotation keeps the label", func(t *testing.T) {
		cfg := config()
		cfg.DNSLabel = nil
		current := existing()
		current.Properties.DNSSettings = &armnetwork.PublicIPAddressDNSSettings{DomainNameLabel: ptr.To("keep")}
		put, success, _, _ := run(t, cfg, current, nil)
		assert.True(t, success)
		if assert.NotNil(t, put) {
			assert.Equal(t, "keep", *put.Properties.DNSSettings.DomainNameLabel)
		}
	})

	t.Run("a prefix change is rejected without writes", func(t *testing.T) {
		cfg := config()
		cfg.PIPPrefixID = testPrefixID
		put, success, _, err := run(t, cfg, existing(), nil)
		assert.False(t, success)
		assert.True(t, isTerminalError(err), "%v", err)
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), "the Public IP prefix of an existing Service cannot be changed")
		}
		assert.Nil(t, put)
		assert.Zero(t, lbCalls)
	})

	t.Run("the same prefix in another case is not a change", func(t *testing.T) {
		cfg := config()
		cfg.PIPPrefixID = testPrefixID
		current := existing()
		current.Properties.PublicIPPrefix = &armnetwork.SubResource{ID: ptr.To(strings.Replace(testPrefixID, "/resourceGroups/rg/", "/resourceGroups/RG/", 1))}
		_, success, _, err := run(t, cfg, current, nil)
		assert.True(t, success, "%v", err)
	})

	t.Run("a removed prefix annotation is a no-op", func(t *testing.T) {
		current := existing()
		current.Properties.PublicIPPrefix = &armnetwork.SubResource{ID: ptr.To(testPrefixID)}
		put, success, _, _ := run(t, config(), current, nil)
		assert.True(t, success)
		if assert.NotNil(t, put) {
			assert.Equal(t, testPrefixID, derefString(put.Properties.PublicIPPrefix.ID))
		}
	})

	t.Run("a failed Public IP read is retried without touching the load balancer", func(t *testing.T) {
		put, success, _, _ := run(t, config(), nil, errors.New("boom"))
		assert.False(t, success)
		assert.Nil(t, put)
		assert.Zero(t, lbCalls)
	})

	t.Run("a failed Public IP write fails the update before the load balancer", func(t *testing.T) {
		putErr = errors.New("conflict")
		defer func() { putErr = nil }()
		put, success, _, _ := run(t, config(), existing(), nil)
		assert.False(t, success)
		assert.NotNil(t, put)
		assert.Zero(t, lbCalls)
	})
}

// inboundLB returns a load balancer whose frontend uses the given Public IP.
func inboundLB(pipID string) *armnetwork.LoadBalancer {
	return &armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{
		FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{
			Name:       ptr.To("frontend"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To(pipID)}},
		}},
	}}
}

// expectCurrentLB makes the load balancer read return one that uses the Service's own Public IP.
func expectCurrentLB(lb *mock_loadbalancerclient.MockInterface, uid string) {
	lb.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).Return(inboundLB(publicIPAddressID("sub", "rg", PublicIPName(uid))), nil).AnyTimes()
}

// publicIPWorld is an in-memory Azure for the Public IP selection tests.
type publicIPWorld struct {
	pips      map[string]*armnetwork.PublicIPAddress
	currentLB *armnetwork.LoadBalancer
	deleteErr error
	created   []string
	deleted   []string
	lbPuts    int
	lbReads   int
	events    []string
	ingress   string
	lbGetErr  error
	lbPutErr  error
	// lbPutApplied makes a failed load balancer write still take effect, as when Azure applies it but the
	// operation times out.
	lbPutApplied bool
	patchErr     error
	listErr      error
	// ops records ServiceGateway registrations ("attach"/"detach") and load balancer writes ("lb").
	ops []string
	// tracked reports whether the run left the load balancer tracked as live in NRP.
	tracked bool
	// others are further Services in the cluster; deleting are Service UIDs the engine is deleting.
	others        []*v1.Service
	deleting      []string
	pending       map[string][]string
	neverFrontend map[string]map[string]bool
	// families are the Service's IP families when the unit run is one of several; by default the Service has
	// only the unit's family.
	families []v1.IPFamily
}

const otherServiceUID = "99999999-9999-9999-9999-999999999999"

func loadBalancerService(namespace, name, uid string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(uid)},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
}

func pipKey(resourceGroup, name string) string { return strings.ToLower(resourceGroup + "/" + name) }

func newPublicIPWorld(pips map[string]*armnetwork.PublicIPAddress) *publicIPWorld {
	w := &publicIPWorld{pips: map[string]*armnetwork.PublicIPAddress{}}
	for key, pip := range pips {
		w.pips[strings.ToLower(key)] = pip
	}
	return w
}

func userPIP(name, address string) *armnetwork.PublicIPAddress {
	return &armnetwork.PublicIPAddress{
		Name:     ptr.To(name),
		Location: ptr.To("eastus"),
		SKU:      &armnetwork.PublicIPAddressSKU{Name: ptr.To(armnetwork.PublicIPAddressSKUNameStandardV2)},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			IPAddress:              ptr.To(address),
			PublicIPAddressVersion: ptr.To(armnetwork.IPVersionIPv4),
			ProvisioningState:      ptr.To(armnetwork.ProvisioningStateSucceeded),
		},
	}
}

func ownedPIP(name, address string) *armnetwork.PublicIPAddress {
	pip := userPIP(name, address)
	pip.Tags = map[string]*string{consts.ServiceTagKey: ptr.To("ns/svc"), consts.ClusterNameKey: ptr.To("cluster")}
	return pip
}

func withLoadBalancerIP(address string) *InboundConfig {
	config := chosenPIPConfig()
	config.LoadBalancerIP = address
	return config
}

func chosenPIPConfig() *InboundConfig {
	config := makeInboundConfig(80)
	config.ServiceName, config.ClusterName = "ns/svc", "cluster"
	return config
}

// run creates, or when update is set updates, the inbound Service with config against the world.
func (w *publicIPWorld) run(t *testing.T, uid string, update bool, config *InboundConfig) (bool, *v1.Service, error) {
	ctrl := gomock.NewController(t)
	f := mock_azclient.NewMockClientFactory(ctrl)
	pipClient := mock_publicipaddressclient.NewMockInterface(ctrl)
	lbClient := mock_loadbalancerclient.NewMockInterface(ctrl)
	sgwClient := mock_servicegatewayclient.NewMockInterface(ctrl)
	f.EXPECT().GetPublicIPAddressClient().Return(pipClient).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(lbClient).AnyTimes()
	f.EXPECT().GetServiceGatewayClient().Return(sgwClient).AnyTimes()
	pipClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, rg, name string, _ *string) (*armnetwork.PublicIPAddress, error) {
			if pip, ok := w.pips[pipKey(rg, name)]; ok {
				copied := *pip
				return &copied, nil
			}
			return nil, notFoundError()
		}).AnyTimes()
	pipClient.EXPECT().List(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, rg string) ([]*armnetwork.PublicIPAddress, error) {
			if w.listErr != nil {
				return nil, w.listErr
			}
			var pips []*armnetwork.PublicIPAddress
			for key, pip := range w.pips {
				if strings.HasPrefix(key, strings.ToLower(rg)+"/") {
					pips = append(pips, pip)
				}
			}
			return pips, nil
		}).AnyTimes()
	pipClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, rg, name string, pip armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
			if derefString(pip.Properties.IPAddress) == "" {
				pip.Properties.IPAddress = ptr.To("20.0.0.1")
			}
			w.pips[pipKey(rg, name)] = &pip
			w.created = append(w.created, pipKey(rg, name))
			return &pip, nil
		}).AnyTimes()
	pipClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, rg, name string) error {
			if w.deleteErr != nil {
				return w.deleteErr
			}
			delete(w.pips, pipKey(rg, name))
			w.deleted = append(w.deleted, pipKey(rg, name))
			return nil
		}).AnyTimes()
	lbClient.EXPECT().Get(gomock.Any(), "rg", uid, gomock.Any()).DoAndReturn(
		func(context.Context, string, string, *string) (*armnetwork.LoadBalancer, error) {
			w.lbReads++
			if w.lbGetErr != nil {
				return nil, w.lbGetErr
			}
			if w.currentLB == nil {
				return nil, notFoundError()
			}
			return w.currentLB, nil
		}).AnyTimes()
	lbClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", uid, gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, lb armnetwork.LoadBalancer) (*armnetwork.LoadBalancer, error) {
			w.lbPuts++
			w.ops = append(w.ops, "lb")
			if w.lbPutErr != nil {
				if w.lbPutApplied {
					w.currentLB = &lb
				}
				return nil, w.lbPutErr
			}
			w.currentLB = &lb
			return &lb, nil
		}).AnyTimes()
	sgwClient.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, req armnetwork.ServiceGatewayUpdateServicesRequest) error {
			op := "attach"
			if len(req.ServiceRequests) != 1 || req.ServiceRequests[0].Service == nil || derefString(req.ServiceRequests[0].Service.Name) != uid {
				op = "other-service"
			} else if props := req.ServiceRequests[0].Service.Properties; props == nil || len(props.LoadBalancerBackendPools) == 0 {
				op = "detach"
			} else if !strings.HasSuffix(derefString(props.LoadBalancerBackendPools[0].ID), "/loadBalancers/"+uid+"/backendAddressPools/"+uid) {
				op = "attach-wrong-pool"
			}
			w.ops = append(w.ops, op)
			return nil
		}).AnyTimes()

	parentUID, _ := ParentServiceUID(uid)
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: types.UID(parentUID)},
		Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, IPFamilies: slices.Clone(w.families)},
	}
	if w.families == nil {
		for _, family := range config.IPFamilies {
			svc.Spec.IPFamilies = append(svc.Spec.IPFamilies, v1.IPFamily(family))
		}
	}
	if w.ingress != "" {
		svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: w.ingress}}
	}
	objects := []runtime.Object{svc}
	for _, other := range w.others {
		objects = append(objects, other)
	}
	kube := fake.NewSimpleClientset(objects...)
	if w.patchErr != nil {
		kube.PrependReactor("patch", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, w.patchErr
		})
	}
	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.kubeClient = kube
	dt.networkClientFactory = f
	for _, deleting := range w.deleting {
		dt.pendingServiceOps[deleting] = &ServiceOperationState{ServiceUID: deleting, State: StateDeletionPending}
	}
	recorder := record.NewFakeRecorder(20)
	dt.SetEventRecorder(recorder)
	got := &outboundCompletion{}
	su := outboundUpdater(dt, got)
	su.pendingReleases = maps.Clone(w.pending)
	su.neverFrontend = maps.Clone(w.neverFrontend)
	if update {
		su.updateInboundService(uid, config, "corr")
	} else {
		su.createInboundService(uid, config, "corr")
	}
	su.mu.Lock()
	w.pending = maps.Clone(su.pendingReleases)
	w.neverFrontend = maps.Clone(su.neverFrontend)
	su.mu.Unlock()
	close(recorder.Events)
	for event := range recorder.Events {
		w.events = append(w.events, event)
	}
	dt.mu.Lock()
	w.tracked = dt.NRPResources.LoadBalancers.Has(uid)
	dt.mu.Unlock()
	current, _ := kube.CoreV1().Services("ns").Get(context.Background(), "svc", metav1.GetOptions{})
	w.ingress = ingressIP(current)
	_, success, err := got.result()
	return success, current, err
}

func (w *publicIPWorld) frontend() string {
	return frontendPublicIPID(w.currentLB)
}

func (w *publicIPWorld) hasEvent(reason string) bool {
	return slices.ContainsFunc(w.events, func(e string) bool { return strings.Contains(e, reason) })
}

func ingressIP(svc *v1.Service) string {
	if svc == nil || len(svc.Status.LoadBalancer.Ingress) == 0 {
		return ""
	}
	return svc.Status.LoadBalancer.Ingress[0].IP
}

func TestServiceUpdaterCreateInboundService_UsesChosenPublicIP(t *testing.T) {
	const uid = "55555555-5555-5555-5555-555555555555"
	pipID := func(rg, name string) string { return publicIPAddressID("sub", rg, name) }
	withConfig := func(mutate func(*InboundConfig)) *InboundConfig {
		config := chosenPIPConfig()
		mutate(config)
		return config
	}
	byName := withConfig(func(c *InboundConfig) { c.PIPName = "mine" })

	t.Run("a missing named Public IP is created, owned and used", func(t *testing.T) {
		w := newPublicIPWorld(nil)
		success, svc, err := w.run(t, uid, false, withConfig(func(c *InboundConfig) { c.PIPName = "mine"; c.PIPResourceGroup = "other" }))
		assert.True(t, success, "%v", err)
		assert.Equal(t, []string{"other/mine"}, w.created)
		assert.Equal(t, "ns/svc", *w.pips["other/mine"].Tags[consts.ServiceTagKey])
		assert.Equal(t, "cluster", *w.pips["other/mine"].Tags[consts.ClusterNameKey])
		assert.Equal(t, pipID("other", "mine"), w.frontend())
		assert.Equal(t, "20.0.0.1", ingressIP(svc))
	})

	t.Run("a user's Public IP chosen by name is used without being changed", func(t *testing.T) {
		displayed := userPIP("mine", "20.0.0.7")
		displayed.Location = ptr.To("East US")
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": displayed})
		success, svc, err := w.run(t, uid, false, byName)
		assert.True(t, success, "%v", err)
		assert.Empty(t, w.created)
		assert.Equal(t, pipID("rg", "mine"), w.frontend())
		assert.Equal(t, "20.0.0.7", ingressIP(svc))
	})

	t.Run("a Public IP chosen by address is looked up in the chosen resource group", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{
			"rg/decoy":   userPIP("decoy", "20.0.0.7"),
			"other/mine": userPIP("mine", "20.0.0.7"),
		})
		success, svc, err := w.run(t, uid, false, withConfig(func(c *InboundConfig) { c.LoadBalancerIP = "20.0.0.7"; c.PIPResourceGroup = "other" }))
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("other", "mine"), w.frontend())
		assert.Equal(t, "20.0.0.7", ingressIP(svc))
	})

	t.Run("a user's Public IP chosen by address is used without being changed", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{
			"rg/other": userPIP("other", "20.0.0.8"),
			"rg/mine":  userPIP("mine", "20.0.0.7"),
		})
		success, svc, err := w.run(t, uid, false, withConfig(func(c *InboundConfig) { c.LoadBalancerIP = "20.0.0.7" }))
		assert.True(t, success, "%v", err)
		assert.Empty(t, w.created)
		assert.Equal(t, pipID("rg", "mine"), w.frontend())
		assert.Equal(t, "20.0.0.7", ingressIP(svc))
	})

	t.Run("a Public IP the controller created earlier is managed", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": ownedPIP("mine", "20.0.0.7")})
		success, _, err := w.run(t, uid, false, withConfig(func(c *InboundConfig) { c.PIPName = "mine"; c.DNSLabel = ptr.To("app") }))
		assert.True(t, success, "%v", err)
		assert.Equal(t, []string{"rg/mine"}, w.created)
		assert.Equal(t, "app", *w.pips["rg/mine"].Properties.DNSSettings.DomainNameLabel)
	})

	natAttached := userPIP("mine", "20.0.0.7")
	natAttached.Properties.NatGateway = &armnetwork.NatGateway{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/natGateways/nat")}
	updating := userPIP("mine", "20.0.0.7")
	updating.Properties.ProvisioningState = ptr.To(armnetwork.ProvisioningStateUpdating)
	ownedInUse := ownedPIP("mine", "20.0.0.7")
	ownedInUse.Properties.IPConfiguration = &armnetwork.IPConfiguration{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/other/frontendIPConfigurations/frontend")}
	inUse := userPIP("mine", "20.0.0.7")
	inUse.Properties.IPConfiguration = &armnetwork.IPConfiguration{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/other/frontendIPConfigurations/frontend")}
	ours := userPIP("mine", "20.0.0.7")
	ours.Properties.IPConfiguration = &armnetwork.IPConfiguration{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/" + uid + "/frontendIPConfigurations/frontend")}
	standard := userPIP("mine", "20.0.0.7")
	standard.SKU.Name = ptr.To(armnetwork.PublicIPAddressSKUNameStandard)
	ipv6 := userPIP("mine", "2001:db8::1")
	ipv6.Properties.PublicIPAddressVersion = ptr.To(armnetwork.IPVersionIPv6)
	elsewhere := userPIP("mine", "20.0.0.7")
	elsewhere.Location = ptr.To("westus")
	unallocated := userPIP("mine", "")
	otherCluster := ownedPIP("mine", "20.0.0.7")
	otherCluster.Tags[consts.ClusterNameKey] = ptr.To("other-cluster")
	otherService := ownedPIP("mine", "20.0.0.7")
	otherService.Tags[consts.ServiceTagKey] = ptr.To("ns/other")
	otherServiceLegacyTag := ownedPIP("mine", "20.0.0.7")
	delete(otherServiceLegacyTag.Tags, consts.ServiceTagKey)
	otherServiceLegacyTag.Tags[consts.LegacyServiceTagKey] = ptr.To("ns/other")
	otherServiceLB := userPIP("mine", "20.0.0.7")
	otherServiceLB.Properties.IPConfiguration = &armnetwork.IPConfiguration{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/" + otherServiceUID + "/frontendIPConfigurations/frontend")}
	otherServiceSecondaryLB := userPIP("mine", "20.0.0.7")
	otherServiceSecondaryLB.Properties.IPConfiguration = &armnetwork.IPConfiguration{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/" + otherServiceUID + "-v6/frontendIPConfigurations/frontend")}
	ownOtherFamilyLB := userPIP("mine", "20.0.0.7")
	ownOtherFamilyLB.Properties.IPConfiguration = &armnetwork.IPConfiguration{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/" + uid + "-v6/frontendIPConfigurations/frontend")}
	other := loadBalancerService("ns", "other", otherServiceUID)
	otherBeingDeleted := loadBalancerService("ns", "other", otherServiceUID)
	otherBeingDeleted.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	otherNowClusterIP := loadBalancerService("ns", "other", otherServiceUID)
	otherNowClusterIP.Spec.Type = v1.ServiceTypeClusterIP
	otherRGServiceLB := userPIP("mine", "20.0.0.7")
	otherRGServiceLB.Properties.IPConfiguration = &armnetwork.IPConfiguration{ID: ptr.To("/subscriptions/sub/resourceGroups/other-rg/providers/Microsoft.Network/loadBalancers/99999999-9999-9999-9999-999999999999/frontendIPConfigurations/frontend")}

	for _, tc := range []struct {
		name     string
		pip      *armnetwork.PublicIPAddress
		config   *InboundConfig
		terminal bool
		event    string
		others   []*v1.Service
		deleting []string
	}{
		{name: "a Public IP used by another resource waits", pip: inUse, config: byName, event: "PublicIPInUse"},
		{name: "a Public IP used by a NAT gateway waits", pip: natAttached, config: byName, event: "PublicIPInUse"},
		{name: "a Public IP created for this Service but used elsewhere waits", pip: ownedInUse, config: byName, event: "PublicIPInUse"},
		{name: "a Public IP created for another Service of this cluster is rejected", pip: otherService, config: byName, event: "SharedPublicIPNotSupported", others: []*v1.Service{other}},
		{name: "a Public IP created for another Service with the legacy ownership tag is rejected", pip: otherServiceLegacyTag, config: byName, event: "SharedPublicIPNotSupported", others: []*v1.Service{other}},
		{name: "a Public IP created for a deleted Service of this cluster waits", pip: otherService, config: byName, event: "PublicIPInUse"},
		{name: "a Public IP created for a Service being deleted waits", pip: otherService, config: byName, event: "PublicIPInUse", others: []*v1.Service{otherBeingDeleted}},
		{name: "a Public IP used by another Service's load balancer is rejected", pip: otherServiceLB, config: byName, event: "SharedPublicIPNotSupported", others: []*v1.Service{other}},
		{name: "a Public IP used by a deleted Service's load balancer waits", pip: otherServiceLB, config: byName, event: "PublicIPInUse"},
		{name: "a Public IP used by the load balancer of a Service being deleted waits", pip: otherServiceLB, config: byName, event: "PublicIPInUse", others: []*v1.Service{otherBeingDeleted}},
		{name: "a Public IP used by the load balancer of a Service the controller is deleting waits", pip: otherServiceLB, config: byName, event: "PublicIPInUse", others: []*v1.Service{other}, deleting: []string{otherServiceUID}},
		{name: "a Public IP used by the load balancer of a Service no longer of type LoadBalancer waits", pip: otherServiceLB, config: byName, event: "PublicIPInUse", others: []*v1.Service{otherNowClusterIP}},
		{name: "a Public IP used by another Service's secondary unit load balancer is rejected", pip: otherServiceSecondaryLB, config: byName, event: "SharedPublicIPNotSupported", others: []*v1.Service{other}},
		{name: "a Public IP used by a load balancer of another resource group waits", pip: otherRGServiceLB, config: byName, event: "PublicIPInUse", others: []*v1.Service{other}},
		{name: "a Public IP still provisioning waits", pip: updating, config: byName},
		{name: "a Standard Public IP is rejected", pip: standard, config: byName, terminal: true},
		{name: "an IPv6 Public IP for an IPv4 Service is rejected", pip: ipv6, config: byName, terminal: true},
		{name: "a Public IP in another region is rejected", pip: elsewhere, config: byName, terminal: true},
		{name: "a Public IP without an address waits", pip: unallocated, config: byName},
		{name: "a user's Public IP is not given tags", pip: userPIP("mine", "20.0.0.7"), terminal: true,
			config: withConfig(func(c *InboundConfig) { c.PIPName = "mine"; c.PIPTags = map[string]string{"team": "a"} })},
		{name: "a user's Public IP is not given a DNS label", pip: userPIP("mine", "20.0.0.7"), terminal: true,
			config: withConfig(func(c *InboundConfig) { c.PIPName = "mine"; c.DNSLabel = ptr.To("") })},
		{name: "a user's Public IP is not given IP tags", pip: userPIP("mine", "20.0.0.7"), terminal: true,
			config: withConfig(func(c *InboundConfig) { c.PIPName = "mine"; c.IPTags = map[string]string{} })},
		{name: "a user's Public IP cannot come from a prefix", pip: userPIP("mine", "20.0.0.7"), terminal: true,
			config: withConfig(func(c *InboundConfig) { c.PIPName = "mine"; c.PIPPrefixID = testPrefixID })},
		{name: "another cluster's Public IP is not changed", pip: otherCluster, terminal: true,
			config: withConfig(func(c *InboundConfig) { c.PIPName = "mine"; c.DNSLabel = ptr.To("app") })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": tc.pip})
			w.others, w.deleting = tc.others, tc.deleting
			success, _, err := w.run(t, uid, false, tc.config)
			assert.False(t, success)
			assert.Equal(t, tc.terminal, isTerminalError(err), "%v", err)
			assert.Empty(t, w.created, "the Public IP must not be written")
			assert.Zero(t, w.lbPuts, "the load balancer must not be written")
			if tc.event != "" {
				assert.True(t, w.hasEvent(tc.event), "expected a %s event, got %v", tc.event, w.events)
			}
		})
	}

	t.Run("at startup the cluster name is not known yet and the cluster tag in the cluster resource group decides", func(t *testing.T) {
		atStartup := func(mutate func(*InboundConfig)) *InboundConfig {
			config := withConfig(mutate)
			config.ClusterName = ""
			return config
		}
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": ownedPIP("mine", "20.0.0.7")})
		success, _, err := w.run(t, uid, false, atStartup(func(c *InboundConfig) { c.PIPName = "mine"; c.DNSLabel = ptr.To("app") }))
		assert.True(t, success, "the Service's own Public IP is managed, not refused as a user's: %v", err)
		assert.Equal(t, "app", *w.pips["rg/mine"].Properties.DNSSettings.DomainNameLabel)

		other := ownedPIP("mine", "20.0.0.7")
		other.Tags[consts.ServiceTagKey] = ptr.To("ns/other")
		w = newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": other})
		w.others = []*v1.Service{loadBalancerService("ns", "other", otherServiceUID)}
		success, _, err = w.run(t, uid, false, atStartup(func(c *InboundConfig) { c.PIPName = "mine" }))
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "retried so it can take the Public IP once released: %v", err)
		assert.True(t, w.hasEvent("SharedPublicIPNotSupported"), "another Service's Public IP cannot be shared: %v", w.events)
		assert.Zero(t, w.lbPuts, "another Service's Public IP is not attached")

		// Elsewhere another cluster may have tagged it, so it waits for the cluster name.
		w = newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"other-rg/mine": ownedPIP("mine", "20.0.0.7")})
		success, _, _ = w.run(t, uid, false, atStartup(func(c *InboundConfig) {
			c.PIPName = "mine"
			c.PIPResourceGroup = "other-rg"
			c.DNSLabel = ptr.To("app")
		}))
		assert.False(t, success)
		assert.Empty(t, w.created, "a Public IP in another resource group is not changed before its ownership is known")
	})

	t.Run("the managed Public IP of another Service or egress identity is never taken", func(t *testing.T) {
		otherService := PublicIPName("99999999-9999-9999-9999-999999999999")
		otherSecondary := PublicIPName("99999999-9999-9999-9999-999999999999-v4")
		egress := userPIP("team-egress-pip", "20.0.0.8")
		egress.Tags = egressIdentityTags("team-egress")
		for name, pip := range map[string]*armnetwork.PublicIPAddress{
			otherService: userPIP(otherService, "20.0.0.7"), otherSecondary: userPIP(otherSecondary, "20.0.0.9"), "team-egress-pip": egress,
		} {
			for _, config := range []*InboundConfig{
				withConfig(func(c *InboundConfig) { c.PIPName = name }),
				withLoadBalancerIP(*pip.Properties.IPAddress),
			} {
				w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/" + name: pip})
				w.others = []*v1.Service{loadBalancerService("ns", "other", otherServiceUID)}
				success, _, err := w.run(t, uid, false, config)
				assert.False(t, success, name)
				assert.Empty(t, w.created, name)
				assert.Zero(t, w.lbPuts, name)
				if name == otherService || name == otherSecondary {
					assert.False(t, isTerminalError(err), "%s: retried so it can take the Public IP once released: %v", name, err)
					assert.True(t, w.hasEvent("SharedPublicIPNotSupported"), "%s: several Services cannot share a Public IP: %v", name, w.events)

					w = newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/" + name: pip})
					success, _, err = w.run(t, uid, false, config)
					assert.False(t, success, name)
					assert.False(t, isTerminalError(err), "%s: waits for the deleted Service's own delete to remove it: %v", name, err)
					assert.True(t, w.hasEvent("PublicIPInUse"), "%v", w.events)
					continue
				}
				assert.False(t, isTerminalError(err), "%s: retried while it exists: %v", name, err)
				assert.True(t, w.hasEvent("PublicIPInUse"), "%s: expected a PublicIPInUse event, got %v", name, w.events)
			}
		}
	})

	t.Run("a Public IP already used by this Service's load balancer is used", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": ours})
		success, _, err := w.run(t, uid, false, byName)
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("rg", "mine"), w.frontend())
	})

	t.Run("a Public IP attached to this Service's other-family unit is not another Service", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": ownOtherFamilyLB})
		success, _, err := w.run(t, uid, false, byName)
		assert.True(t, success, "%v", err)
		assert.False(t, w.hasEvent("SharedPublicIPNotSupported"), "%v", w.events)
	})

	t.Run("an attached owned Public IP from another prefix is not recreated", func(t *testing.T) {
		attached := ownedPIP(PublicIPName(uid), "20.0.0.7")
		attached.Properties.PublicIPPrefix = &armnetwork.SubResource{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPPrefixes/other")}
		attached.Properties.IPConfiguration = &armnetwork.IPConfiguration{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic/ipConfigurations/ipconfig1")}
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/" + PublicIPName(uid): attached})
		cfg := chosenPIPConfig()
		cfg.PIPPrefixID = testPrefixID
		success, _, err := w.run(t, uid, false, cfg)
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.Empty(t, w.created)
		assert.Empty(t, w.deleted)
		assert.True(t, w.hasEvent("PublicIPInUse"), "%v", w.events)
	})

	t.Run("an owned Public IP attached to a NAT gateway from another prefix is not recreated", func(t *testing.T) {
		attached := ownedPIP(PublicIPName(uid), "20.0.0.7")
		attached.Properties.PublicIPPrefix = &armnetwork.SubResource{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPPrefixes/other")}
		attached.Properties.NatGateway = &armnetwork.NatGateway{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/natGateways/nat")}
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/" + PublicIPName(uid): attached})
		cfg := chosenPIPConfig()
		cfg.PIPPrefixID = testPrefixID
		success, _, err := w.run(t, uid, false, cfg)
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.Empty(t, w.created)
		assert.Empty(t, w.deleted)
		assert.True(t, w.hasEvent("PublicIPInUse"), "%v", w.events)
	})

	t.Run("a create that finds a load balancer on another Public IP moves it", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/old": ownedPIP("old", "20.0.0.5"), "rg/mine": userPIP("mine", "20.0.0.7")})
		w.currentLB = inboundLB(pipID("rg", "old"))
		success, svc, err := w.run(t, uid, false, byName)
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("rg", "mine"), w.frontend())
		assert.Equal(t, []string{"rg/old"}, w.deleted)
		assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
		assert.Equal(t, "20.0.0.7", ingressIP(svc))
	})

	t.Run("an IPv6 Service uses IPv6 Public IPs", func(t *testing.T) {
		v6 := func(name, address string) *armnetwork.PublicIPAddress {
			pip := userPIP(name, address)
			pip.Properties.PublicIPAddressVersion = ptr.To(armnetwork.IPVersionIPv6)
			return pip
		}
		ipv6 := func(mutate func(*InboundConfig)) *InboundConfig {
			return withConfig(func(c *InboundConfig) { c.IPFamilies = []string{"IPv6"}; mutate(c) })
		}

		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": v6("mine", "2001:db8::7")})
		success, svc, err := w.run(t, uid, false, ipv6(func(c *InboundConfig) { c.LoadBalancerIP = "2001:DB8:0::7" }))
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("rg", "mine"), w.frontend(), "addresses are matched by value, not by spelling")
		assert.Equal(t, "2001:db8::7", ingressIP(svc))

		w = newPublicIPWorld(nil)
		success, _, err = w.run(t, uid, false, ipv6(func(c *InboundConfig) { c.PIPName = "mine-v6" }))
		assert.True(t, success, "%v", err)
		assert.Equal(t, armnetwork.IPVersionIPv6, *w.pips["rg/mine-v6"].Properties.PublicIPAddressVersion)
		assert.Equal(t, pipID("rg", "mine-v6"), w.frontend())

		w = newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": userPIP("mine", "20.0.0.7")})
		success, _, err = w.run(t, uid, false, ipv6(func(c *InboundConfig) { c.PIPName = "mine" }))
		assert.False(t, success)
		assert.True(t, isTerminalError(err), "an IPv4 Public IP cannot serve an IPv6 Service: %v", err)
	})

	t.Run("an unattached managed Public IP of the wrong family is recreated", func(t *testing.T) {
		managed := ownedPIP(PublicIPName(uid), "20.0.0.7")
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/" + PublicIPName(uid): managed})
		config := withConfig(func(c *InboundConfig) { c.IPFamilies = []string{"IPv6"} })

		success, _, err := w.run(t, uid, false, config)

		assert.True(t, success, "%v", err)
		assert.Contains(t, w.deleted, "rg/"+PublicIPName(uid))
		assert.Contains(t, w.created, "rg/"+PublicIPName(uid))
		assert.Equal(t, armnetwork.IPVersionIPv6, *w.pips["rg/"+PublicIPName(uid)].Properties.PublicIPAddressVersion)
	})

	t.Run("a managed Public IP of the wrong family that cannot be deleted is retried", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/" + PublicIPName(uid): ownedPIP(PublicIPName(uid), "20.0.0.7")})
		w.deleteErr = errors.New("busy")

		success, _, err := w.run(t, uid, false, withConfig(func(c *InboundConfig) { c.IPFamilies = []string{"IPv6"} }))

		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.Empty(t, w.created)
		assert.Zero(t, w.lbPuts, "no load balancer may be written on the wrong-family Public IP")
	})

	t.Run("an attached managed Public IP of the wrong family is still rejected", func(t *testing.T) {
		attached := ownedPIP(PublicIPName(uid), "20.0.0.7")
		attached.Properties.IPConfiguration = &armnetwork.IPConfiguration{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/" + uid + "/frontendIPConfigurations/frontend")}
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/" + PublicIPName(uid): attached})
		config := withConfig(func(c *InboundConfig) { c.IPFamilies = []string{"IPv6"} })

		success, _, err := w.run(t, uid, false, config)

		assert.False(t, success)
		assert.True(t, isTerminalError(err), "an attached IPv4 Public IP cannot serve an IPv6 Service: %v", err)
		assert.Empty(t, w.deleted)
	})

	t.Run("a NAT-attached managed Public IP of the wrong family is still rejected", func(t *testing.T) {
		attached := ownedPIP(PublicIPName(uid), "20.0.0.7")
		attached.Properties.NatGateway = &armnetwork.NatGateway{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/natGateways/nat")}
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/" + PublicIPName(uid): attached})
		config := withConfig(func(c *InboundConfig) { c.IPFamilies = []string{"IPv6"} })

		success, _, err := w.run(t, uid, false, config)

		assert.False(t, success)
		assert.True(t, isTerminalError(err), "a NAT-attached IPv4 Public IP cannot serve an IPv6 Service: %v", err)
		assert.Empty(t, w.deleted)
		assert.Empty(t, w.created)
	})

	t.Run("a named Public IP created for a load balancer that could not be written is deleted", func(t *testing.T) {
		w := newPublicIPWorld(nil)
		w.lbPutErr = errors.New("conflict")
		success, _, err := w.run(t, uid, false, byName)
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.Equal(t, []string{"rg/mine"}, w.created)
		assert.Equal(t, []string{"rg/mine"}, w.deleted)
	})

	t.Run("the Public IP named after the Service is kept for the retry", func(t *testing.T) {
		w := newPublicIPWorld(nil)
		w.lbPutErr = errors.New("conflict")
		success, _, _ := w.run(t, uid, false, chosenPIPConfig())
		assert.False(t, success)
		assert.Empty(t, w.deleted)
	})

	t.Run("an existing named Public IP is kept when the load balancer could not be written", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": ownedPIP("mine", "20.0.0.7")})
		w.lbPutErr = errors.New("conflict")
		success, _, _ := w.run(t, uid, false, byName)
		assert.False(t, success)
		assert.Empty(t, w.deleted)
	})

	t.Run("a retried create keeps the Public IP the load balancer already uses", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/" + PublicIPName(uid): userPIP(PublicIPName(uid), "20.0.0.1")})
		w.currentLB = inboundLB(pipID("rg", PublicIPName(uid)))
		success, _, err := w.run(t, uid, false, chosenPIPConfig())
		assert.True(t, success, "%v", err)
		assert.Empty(t, w.deleted)
		assert.Equal(t, pipID("rg", PublicIPName(uid)), w.frontend())
	})

	t.Run("a Public IP the controller created in another resource group is managed there", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"other/mine": ownedPIP("mine", "20.0.0.7")})
		success, _, err := w.run(t, uid, false, withConfig(func(c *InboundConfig) { c.PIPName = "mine"; c.PIPResourceGroup = "other"; c.DNSLabel = ptr.To("app") }))
		assert.True(t, success, "%v", err)
		assert.Equal(t, []string{"other/mine"}, w.created)
		assert.Equal(t, pipID("other", "mine"), w.frontend())
	})

	t.Run("a Public IP created in another resource group is deleted there when the load balancer could not be written", func(t *testing.T) {
		w := newPublicIPWorld(nil)
		w.lbPutErr = errors.New("conflict")
		success, _, _ := w.run(t, uid, false, withConfig(func(c *InboundConfig) { c.PIPName = "mine"; c.PIPResourceGroup = "other" }))
		assert.False(t, success)
		assert.Equal(t, []string{"other/mine"}, w.deleted)
	})

	t.Run("a failed rollback is reported", func(t *testing.T) {
		w := newPublicIPWorld(nil)
		w.lbPutErr = errors.New("conflict")
		w.deleteErr = errors.New("busy")
		success, _, _ := w.run(t, uid, false, byName)
		assert.False(t, success)
		assert.True(t, w.hasEvent("PublicIPCleanupFailed"), "%v", w.events)
	})

	t.Run("a Public IP the controller created, chosen by address, is managed", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": ownedPIP("mine", "20.0.0.7")})
		success, _, err := w.run(t, uid, false, withConfig(func(c *InboundConfig) { c.LoadBalancerIP = "20.0.0.7"; c.DNSLabel = ptr.To("app") }))
		assert.True(t, success, "%v", err)
		assert.Equal(t, []string{"rg/mine"}, w.created)
		assert.Equal(t, "app", *w.pips["rg/mine"].Properties.DNSSettings.DomainNameLabel)
	})

	t.Run("a create parked without a load balancer has nothing to attach", func(t *testing.T) {
		standard := userPIP("mine", "20.0.0.7")
		standard.SKU.Name = ptr.To(armnetwork.PublicIPAddressSKUNameStandard)
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": standard})
		success, _, err := w.run(t, uid, false, byName)
		assert.False(t, success)
		assert.True(t, isTerminalError(err), "%v", err)
		assert.Empty(t, w.ops)
	})

	t.Run("a failed load balancer read on create is retried before any write", func(t *testing.T) {
		w := newPublicIPWorld(nil)
		w.lbGetErr = errors.New("throttled")
		success, _, err := w.run(t, uid, false, withConfig(func(c *InboundConfig) { c.PIPName = "new" }))
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.Zero(t, w.lbPuts)
		assert.Empty(t, w.created, "no Public IP is created before the load balancer is known")
	})

	t.Run("a failed Public IP listing is retried without claiming the address is missing", func(t *testing.T) {
		w := newPublicIPWorld(nil)
		w.listErr = errors.New("throttled")
		success, _, err := w.run(t, uid, false, withConfig(func(c *InboundConfig) { c.LoadBalancerIP = "20.0.0.7" }))
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.False(t, w.hasEvent("PublicIPNotFound"), "%v", w.events)
	})

	t.Run("an address no Public IP has waits", func(t *testing.T) {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{"rg/mine": userPIP("mine", "20.0.0.7")})
		success, _, err := w.run(t, uid, false, withConfig(func(c *InboundConfig) { c.LoadBalancerIP = "20.0.0.9" }))
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.Zero(t, w.lbPuts)
		assert.True(t, w.hasEvent("PublicIPNotFound"), "%v", w.events)
	})
}

func TestPublicIPPrefixChangeTerminalErrorParksAndRevertUnparks(t *testing.T) {
	const uid = "88888888-8888-8888-8888-888888888888"
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: types.UID(uid)}}
	kube := fake.NewSimpleClientset(svc)
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	dt.NRPResources.LoadBalancers.Insert(uid)
	recorder := record.NewFakeRecorder(10)
	dt.SetEventRecorder(recorder)

	bad := NewInboundServiceConfig(uid, chosenPIPConfig())
	bad.InboundConfig.PIPPrefixID = testPrefixID
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:     uid,
		Config:         bad,
		InFlightConfig: &bad,
		State:          StateUpdateInProgress,
	}
	dt.OnServiceCreationComplete(uid, false, newTerminalError(errors.New("the Public IP prefix of an existing Service cannot be changed when ServiceGateway is enabled")))

	op := dt.pendingServiceOps[uid]
	if assert.NotNil(t, op) {
		assert.True(t, op.CreationFailedTerminal)
		assert.Equal(t, StateNotStarted, op.State)
	}
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "ServiceGatewayConfigurationRejected")
	default:
		t.Fatal("expected ServiceGatewayConfigurationRejected event")
	}

	dt.UpdateService(NewInboundServiceConfig(uid, chosenPIPConfig()))
	op = dt.pendingServiceOps[uid]
	if assert.NotNil(t, op) {
		assert.False(t, op.CreationFailedTerminal)
		assert.Equal(t, StateNotStarted, op.State)
	}
}

// The secondary unit of a dual-stack Service moves its own frontend in place when its family's Public IP choice
// changes, and releases the Public IP it moved off; the primary unit is not involved.
func TestServiceUpdaterUpdateInboundService_MovesSecondaryUnitFrontendInPlace(t *testing.T) {
	const uid = "77777777-7777-7777-7777-777777777777"
	unit := SecondaryUnitName(uid, v1.IPv6Protocol)
	managed := publicIPAddressID("sub", "rg", PublicIPName(unit))
	ipv6 := func(pip *armnetwork.PublicIPAddress) *armnetwork.PublicIPAddress {
		pip.Properties.PublicIPAddressVersion = ptr.To(armnetwork.IPVersionIPv6)
		return pip
	}
	unitConfig := func(name string) *InboundConfig {
		config := chosenPIPConfig()
		config.IPFamilies = []string{string(v1.IPv6Protocol)}
		config.PIPName = name
		return config
	}
	w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{
		"rg/" + PublicIPName(uid):  userPIP(PublicIPName(uid), "20.0.0.1"),
		"rg/" + PublicIPName(unit): ipv6(userPIP(PublicIPName(unit), "2001:db8::1")),
		"rg/user-v6":               ipv6(userPIP("user-v6", "2001:db8::8")),
	})
	w.families = []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}
	w.currentLB = inboundLB(managed)
	w.ingress = "2001:db8::1"

	success, svc, err := w.run(t, unit, true, unitConfig("user-v6"))
	assert.True(t, success, "%v", err)
	assert.Equal(t, publicIPAddressID("sub", "rg", "user-v6"), w.frontend(), "the unit's frontend moves in place")
	assert.Equal(t, "2001:db8::8", ingressIP(svc))
	assert.Empty(t, w.created)
	assert.Equal(t, []string{"rg/" + PublicIPName(unit)}, w.deleted, "only the unit's own Public IP is released")
	assert.Contains(t, w.pips, "rg/"+PublicIPName(uid), "the primary unit's Public IP is untouched")
	assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
	assert.Equal(t, []string{"lb"}, w.ops, "the ServiceGateway registration is not touched")

	w.events = nil
	success, _, err = w.run(t, unit, true, unitConfig(""))
	assert.True(t, success, "%v", err)
	assert.Equal(t, managed, w.frontend(), "removing the choice moves the unit back to its own Public IP")
	assert.Equal(t, []string{"rg/" + PublicIPName(unit)}, w.created)
	assert.Equal(t, armnetwork.IPVersionIPv6, *w.pips["rg/"+PublicIPName(unit)].Properties.PublicIPAddressVersion)
	assert.Equal(t, []string{"rg/" + PublicIPName(unit)}, w.deleted, "a user's Public IP is not deleted")
	assert.Contains(t, w.pips, "rg/user-v6")
	assert.Empty(t, w.pending[unit])
	assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
}

func TestServiceUpdaterUpdateInboundService_MovesFrontendWhenPublicIPChoiceChanges(t *testing.T) {
	const uid = "66666666-6666-6666-6666-666666666666"
	managed := publicIPAddressID("sub", "rg", PublicIPName(uid))
	pipID := func(name string) string { return publicIPAddressID("sub", "rg", name) }
	named := func(name string) *InboundConfig {
		config := chosenPIPConfig()
		config.PIPName = name
		return config
	}

	world := func(frontend string) *publicIPWorld {
		w := newPublicIPWorld(map[string]*armnetwork.PublicIPAddress{
			"rg/" + PublicIPName(uid): userPIP(PublicIPName(uid), "20.0.0.1"),
			"rg/mine":                 ownedPIP("mine", "20.0.0.7"),
			"rg/user":                 userPIP("user", "20.0.0.8"),
		})
		w.currentLB = inboundLB(frontend)
		w.ingress = "20.0.0.1"
		return w
	}

	for _, tc := range []struct {
		name         string
		frontend     string
		config       *InboundConfig
		wantFrontend string
		wantIngress  string
		wantCreated  []string
		wantDeleted  []string
		wantEvent    bool
	}{
		{name: "choosing a user's Public IP", frontend: managed, config: named("user"), wantFrontend: pipID("user"), wantIngress: "20.0.0.8", wantDeleted: []string{"rg/" + PublicIPName(uid)}, wantEvent: true},
		{name: "choosing a name that does not exist", frontend: managed, config: named("new"), wantFrontend: pipID("new"), wantIngress: "20.0.0.1", wantCreated: []string{"rg/new"}, wantDeleted: []string{"rg/" + PublicIPName(uid)}, wantEvent: true},
		{name: "choosing another address", frontend: managed, config: withLoadBalancerIP("20.0.0.8"), wantFrontend: pipID("user"), wantIngress: "20.0.0.8", wantDeleted: []string{"rg/" + PublicIPName(uid)}, wantEvent: true},
		{name: "removing the choice", frontend: pipID("mine"), config: chosenPIPConfig(), wantFrontend: managed, wantIngress: "20.0.0.1", wantCreated: []string{"rg/" + PublicIPName(uid)}, wantDeleted: []string{"rg/mine"}, wantEvent: true},
		{name: "the same name", frontend: pipID("mine"), config: named("Mine"), wantFrontend: pipID("mine"), wantIngress: "20.0.0.7"},
		{name: "the same name in another resource group", frontend: pipID("mine"), config: func() *InboundConfig {
			config := named("mine")
			config.PIPResourceGroup = "other"
			return config
		}(), wantFrontend: publicIPAddressID("sub", "other", "mine"), wantIngress: "20.0.0.1", wantCreated: []string{"other/mine"}, wantDeleted: []string{"rg/mine"}, wantEvent: true},
		{name: "the address of the Public IP in use", frontend: pipID("mine"), config: withLoadBalancerIP("20.0.0.7"), wantFrontend: pipID("mine"), wantIngress: "20.0.0.7"},
		{name: "no choice on the Public IP named after the Service", frontend: managed, config: chosenPIPConfig(), wantFrontend: managed, wantIngress: "20.0.0.1", wantCreated: []string{"rg/" + PublicIPName(uid)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := world(tc.frontend)
			success, svc, err := w.run(t, uid, true, tc.config)
			assert.True(t, success, "%v", err)
			assert.Equal(t, tc.wantFrontend, w.frontend(), "the load balancer frontend moves in place")
			assert.Equal(t, tc.wantEvent, w.hasEvent("PublicIPChanged"), "%v", w.events)
			assert.ElementsMatch(t, tc.wantCreated, w.created)
			assert.ElementsMatch(t, tc.wantDeleted, w.deleted)
			assert.Equal(t, tc.wantIngress, ingressIP(svc), "the Service status is updated to the new address")
			assert.Equal(t, []string{"lb"}, w.ops, "the ServiceGateway registration is not touched")
		})
	}

	t.Run("a create retried at startup moves the frontend in place", func(t *testing.T) {
		w := world(managed)
		success, svc, err := w.run(t, uid, false, named("user"))
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("user"), w.frontend())
		assert.Equal(t, "20.0.0.8", ingressIP(svc))
		assert.Empty(t, w.created)
		assert.Equal(t, []string{"rg/" + PublicIPName(uid)}, w.deleted)
		assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
	})

	t.Run("the default Public IP moves to a user's name, then a user's address, then back to the default", func(t *testing.T) {
		w := world(managed)
		w.pips["rg/other-user"] = userPIP("other-user", "20.0.0.9")

		success, svc, err := w.run(t, uid, true, named("user"))
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("user"), w.frontend())
		assert.Equal(t, "20.0.0.8", ingressIP(svc))
		assert.Equal(t, []string{"rg/" + PublicIPName(uid)}, w.deleted, "the owned default Public IP is deleted")

		w.events = nil
		success, svc, err = w.run(t, uid, true, withLoadBalancerIP("20.0.0.9"))
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("other-user"), w.frontend())
		assert.Equal(t, "20.0.0.9", ingressIP(svc))
		assert.Equal(t, []string{"rg/" + PublicIPName(uid)}, w.deleted, "a user's Public IP is not deleted")
		assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)

		w.events = nil
		success, svc, err = w.run(t, uid, true, chosenPIPConfig())
		assert.True(t, success, "%v", err)
		assert.Equal(t, managed, w.frontend())
		assert.Equal(t, "20.0.0.1", ingressIP(svc))
		assert.Equal(t, []string{"rg/" + PublicIPName(uid)}, w.created, "the default Public IP is recreated")
		assert.Equal(t, []string{"rg/" + PublicIPName(uid)}, w.deleted, "a user's Public IP is not deleted")
		assert.Contains(t, w.pips, "rg/user")
		assert.Contains(t, w.pips, "rg/other-user")
		assert.Empty(t, w.pending[uid])
		assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
		assert.Equal(t, []string{"lb", "lb", "lb"}, w.ops, "the ServiceGateway registration is not touched")
	})

	t.Run("a user's Public IP changes to another user's Public IP in place", func(t *testing.T) {
		w := world(pipID("user"))
		w.pips["rg/other-user"] = userPIP("other-user", "20.0.0.9")
		success, svc, err := w.run(t, uid, true, named("other-user"))
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("other-user"), w.frontend())
		assert.Equal(t, "20.0.0.9", ingressIP(svc))
		assert.Empty(t, w.created)
		assert.Empty(t, w.deleted)
		assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
	})

	t.Run("a user's Public IP changes to the managed Public IP in place", func(t *testing.T) {
		w := world(pipID("user"))
		success, svc, err := w.run(t, uid, true, chosenPIPConfig())
		assert.True(t, success, "%v", err)
		assert.Equal(t, managed, w.frontend())
		assert.Equal(t, "20.0.0.1", ingressIP(svc))
		assert.Equal(t, []string{"rg/" + PublicIPName(uid)}, w.created)
		assert.Empty(t, w.deleted)
		assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
	})

	t.Run("an owned named Public IP changes to another owned named Public IP in place", func(t *testing.T) {
		w := world(pipID("mine"))
		success, svc, err := w.run(t, uid, true, named("new"))
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("new"), w.frontend())
		assert.Equal(t, "20.0.0.1", ingressIP(svc))
		assert.Equal(t, []string{"rg/new"}, w.created)
		assert.Equal(t, []string{"rg/mine"}, w.deleted)
		assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
	})

	t.Run("a failed old Public IP release is reported and kept for retry", func(t *testing.T) {
		w := world(managed)
		w.deleteErr = errors.New("busy")
		success, svc, err := w.run(t, uid, true, named("user"))
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("user"), w.frontend())
		assert.Equal(t, "20.0.0.8", ingressIP(svc))
		assert.True(t, w.hasEvent("PublicIPCleanupFailed"), "%v", w.events)
		assert.Equal(t, []string{managed}, w.pending[uid])
	})

	for _, update := range []bool{true, false} {
		t.Run(fmt.Sprintf("a move whose load balancer write fails deletes the Public IP it created and keeps the old one (update %v)", update), func(t *testing.T) {
			w := world(managed)
			w.lbPutErr = errors.New("conflict")
			success, svc, err := w.run(t, uid, update, named("new"))
			assert.False(t, success)
			assert.Error(t, err)
			assert.False(t, isTerminalError(err), "%v", err)
			assert.Equal(t, []string{"rg/new"}, w.created)
			assert.Equal(t, []string{"rg/new"}, w.deleted)
			assert.Equal(t, managed, w.frontend())
			assert.Equal(t, "20.0.0.1", ingressIP(svc))
			assert.Equal(t, []string{managed}, w.pending[uid], "the Public IP in use is recorded before the write in case Azure applies it")
			assert.False(t, w.hasEvent("PublicIPChanged"), "%v", w.events)

			w.lbPutErr = nil
			success, svc, err = w.run(t, uid, update, chosenPIPConfig())
			assert.True(t, success, "%v", err)
			assert.Equal(t, managed, w.frontend())
			assert.Equal(t, "20.0.0.1", ingressIP(svc))
			assert.Equal(t, []string{"rg/new"}, w.deleted, "reverting must not delete the Public IP in use")
			assert.Contains(t, w.pips, "rg/"+PublicIPName(uid))
			assert.Empty(t, w.pending[uid])
			assert.False(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
		})

		t.Run(fmt.Sprintf("a default created for a failed move is released once the Service chooses another (update %v)", update), func(t *testing.T) {
			w := world(pipID("user"))
			delete(w.pips, "rg/"+PublicIPName(uid))
			w.lbPutErr = errors.New("conflict")
			success, _, _ := w.run(t, uid, update, chosenPIPConfig())
			assert.False(t, success)
			assert.Contains(t, w.created, "rg/"+PublicIPName(uid))
			assert.Equal(t, pipID("user"), w.frontend())
			assert.Contains(t, w.pips, "rg/"+PublicIPName(uid), "the default is kept for the retry")

			w.lbPutErr = nil
			success, _, err := w.run(t, uid, update, named("user"))
			assert.True(t, success, "%v", err)
			assert.Equal(t, pipID("user"), w.frontend())
			assert.Equal(t, []string{"rg/" + PublicIPName(uid)}, w.deleted, "the unused default is released and the user's kept")
			assert.Empty(t, w.pending[uid])
			assert.False(t, w.hasEvent("PublicIPChanged"), "the frontend never left the user's Public IP: %v", w.events)
		})

		t.Run(fmt.Sprintf("a default created for a failed move is kept when the retry still chooses it (update %v)", update), func(t *testing.T) {
			w := world(pipID("user"))
			delete(w.pips, "rg/"+PublicIPName(uid))
			w.lbPutErr = errors.New("conflict")
			success, _, _ := w.run(t, uid, update, chosenPIPConfig())
			assert.False(t, success)

			w.lbPutErr = nil
			success, _, err := w.run(t, uid, update, chosenPIPConfig())
			assert.True(t, success, "%v", err)
			assert.Equal(t, managed, w.frontend())
			assert.NotContains(t, w.deleted, "rg/"+PublicIPName(uid))
			assert.Contains(t, w.pips, "rg/"+PublicIPName(uid))
			assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)

			w.events = nil
			success, _, err = w.run(t, uid, update, named("user"))
			assert.True(t, success, "%v", err)
			assert.Equal(t, pipID("user"), w.frontend())
			assert.True(t, w.hasEvent("PublicIPChanged"), "a later move off the default it served is reported: %v", w.events)
		})

		t.Run(fmt.Sprintf("a failed move retried later completes it (update %v)", update), func(t *testing.T) {
			w := world(managed)
			w.lbPutErr = errors.New("conflict")
			success, _, _ := w.run(t, uid, update, named("user"))
			assert.False(t, success)
			assert.Equal(t, managed, w.frontend())
			assert.Empty(t, w.deleted)

			w.lbPutErr = nil
			success, svc, err := w.run(t, uid, update, named("user"))
			assert.True(t, success, "%v", err)
			assert.Equal(t, pipID("user"), w.frontend())
			assert.Equal(t, "20.0.0.8", ingressIP(svc))
			assert.Equal(t, []string{"rg/" + PublicIPName(uid)}, w.deleted)
			assert.Contains(t, w.pips, "rg/user")
			assert.Empty(t, w.pending[uid])
			assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
		})

		t.Run(fmt.Sprintf("a load balancer write Azure applied but reported failed releases the old Public IP on the next reconcile (update %v)", update), func(t *testing.T) {
			w := world(pipID("mine"))
			w.lbPutErr = &azcore.ResponseError{StatusCode: http.StatusGatewayTimeout}
			w.lbPutApplied = true
			success, _, err := w.run(t, uid, update, named("user"))
			assert.False(t, success)
			assert.False(t, isTerminalError(err), "%v", err)
			assert.Equal(t, pipID("user"), w.frontend(), "Azure applied the write")
			assert.Empty(t, w.deleted)
			assert.Equal(t, []string{pipID("mine")}, w.pending[uid])

			w.lbPutErr, w.lbPutApplied = nil, false
			w.events = nil
			success, svc, err := w.run(t, uid, update, named("user"))
			assert.True(t, success, "%v", err)
			assert.Equal(t, pipID("user"), w.frontend())
			assert.Equal(t, "20.0.0.8", ingressIP(svc))
			assert.Equal(t, []string{"rg/mine"}, w.deleted, "the controller-created Public IP the load balancer moved off is released")
			assert.Contains(t, w.pips, "rg/user")
			assert.Empty(t, w.pending[uid])
			if update {
				assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
			}
		})
	}

	t.Run("pending release cleanup keeps the newly selected Public IP", func(t *testing.T) {
		w := world(managed)
		w.pending = map[string][]string{uid: {pipID("mine")}}
		success, _, err := w.run(t, uid, true, named("mine"))
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("mine"), w.frontend())
		assert.NotContains(t, w.deleted, "rg/mine", "the new target must not be deleted as a stale pending release")
		assert.Contains(t, w.deleted, "rg/"+PublicIPName(uid), "the old managed PIP is still released")
	})

	t.Run("a failed status update is retried before releasing the old Public IP", func(t *testing.T) {
		w := world(managed)
		w.patchErr = errors.New("apiserver down")
		success, svc, err := w.run(t, uid, true, named("user"))
		assert.False(t, success)
		assert.Error(t, err)
		assert.Equal(t, pipID("user"), w.frontend(), "the first attempt already moved the load balancer")
		assert.Equal(t, "20.0.0.1", ingressIP(svc), "the failed status patch leaves the old advertised address")
		assert.Empty(t, w.deleted, "the old PIP must not be deleted until status is corrected")
		assert.Equal(t, []string{managed}, w.pending[uid])

		w.patchErr = nil
		success, svc, err = w.run(t, uid, true, named("user"))
		assert.True(t, success, "%v", err)
		assert.Equal(t, pipID("user"), w.frontend())
		assert.Equal(t, "20.0.0.8", ingressIP(svc), "retry fixes status even though the frontend is already on the new PIP")
		assert.Equal(t, []string{"rg/" + PublicIPName(uid)}, w.deleted)
		assert.Empty(t, w.pending[uid])
		assert.True(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
	})

	t.Run("the settings of the Service apply to the Public IP the controller created", func(t *testing.T) {
		w := world(pipID("mine"))
		config := named("mine")
		config.DNSLabel = ptr.To("app")
		success, _, err := w.run(t, uid, true, config)
		assert.True(t, success, "%v", err)
		assert.Equal(t, []string{"rg/mine"}, w.created, "the Public IP is written")
		assert.Equal(t, "app", *w.pips["rg/mine"].Properties.DNSSettings.DomainNameLabel)
	})

	t.Run("a user's Public IP in use is not given settings", func(t *testing.T) {
		w := world(pipID("user"))
		config := named("user")
		config.DNSLabel = ptr.To("app")
		success, _, err := w.run(t, uid, true, config)
		assert.False(t, success)
		assert.True(t, isTerminalError(err), "%v", err)
		assert.Zero(t, w.lbPuts)
	})

	t.Run("an invalid new Public IP keeps the current load balancer", func(t *testing.T) {
		w := world(managed)
		bad := userPIP("bad", "20.0.0.9")
		bad.SKU.Name = ptr.To(armnetwork.PublicIPAddressSKUNameStandard)
		w.pips["rg/bad"] = bad
		success, _, err := w.run(t, uid, true, named("bad"))
		assert.False(t, success)
		assert.True(t, isTerminalError(err), "%v", err)
		assert.Equal(t, managed, w.frontend())
		assert.Empty(t, w.created)
		assert.Zero(t, w.lbPuts)
		assert.False(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
	})

	t.Run("a new Public IP held by another live Service keeps the current load balancer", func(t *testing.T) {
		w := world(managed)
		shared := ownedPIP("shared", "20.0.0.9")
		shared.Tags[consts.ServiceTagKey] = ptr.To("ns/other")
		w.pips["rg/shared"] = shared
		w.others = []*v1.Service{loadBalancerService("ns", "other", otherServiceUID)}
		success, _, err := w.run(t, uid, true, named("shared"))
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.Equal(t, managed, w.frontend())
		assert.Empty(t, w.created)
		assert.Zero(t, w.lbPuts)
		assert.True(t, w.hasEvent("SharedPublicIPNotSupported"), "%v", w.events)
		assert.False(t, w.hasEvent("PublicIPChanged"), "%v", w.events)
	})

	t.Run("a failed load balancer read is retried before any write", func(t *testing.T) {
		w := world(managed)
		w.lbGetErr = errors.New("throttled")
		success, _, err := w.run(t, uid, true, named("user"))
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.Zero(t, w.lbPuts)
		assert.Empty(t, w.created)
	})

	t.Run("a Public IP in use that cannot be read is retried", func(t *testing.T) {
		w := world(pipID("gone"))
		success, _, err := w.run(t, uid, true, named("gone"))
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.Zero(t, w.lbPuts)
	})

	t.Run("a failed load balancer write keeps the Public IP in use", func(t *testing.T) {
		w := world(pipID("mine"))
		w.lbPutErr = errors.New("conflict")
		success, _, err := w.run(t, uid, true, named("mine"))
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.Empty(t, w.deleted)
	})

	t.Run("a Public IP created for a Service without a load balancer is deleted when the load balancer could not be written", func(t *testing.T) {
		w := newPublicIPWorld(nil)
		w.lbPutErr = errors.New("conflict")
		success, _, err := w.run(t, uid, true, named("new"))
		assert.False(t, success)
		assert.False(t, isTerminalError(err), "%v", err)
		assert.Equal(t, []string{"rg/new"}, w.deleted)
	})

	t.Run("a Service without a load balancer gets the chosen Public IP", func(t *testing.T) {
		w := newPublicIPWorld(nil)
		success, _, err := w.run(t, uid, true, named("new"))
		assert.True(t, success, "%v", err)
		assert.Equal(t, []string{"rg/new"}, w.created)
		assert.Equal(t, pipID("new"), w.frontend())
	})
}

// expectInboundPIP returns an existing inbound Public IP that already matches the desired state.
func (m *outboundMocks) expectInboundPIP(uid string) {
	m.pip.EXPECT().Get(gomock.Any(), "rg", PublicIPName(uid), gomock.Any()).
		Return(&armnetwork.PublicIPAddress{
			Name:       ptr.To(PublicIPName(uid)),
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{IPAddress: ptr.To("20.0.0.1")},
		}, nil).AnyTimes()
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
		m.expectInboundPIP(uid)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		expectCurrentLB(mockLB, uid)
		mockLB.EXPECT().CreateOrUpdate(gomock.Any(), "rg", uid, gomock.Any()).Return(nil, nil).Times(1)
		// A port-only update must not touch the Public IP or re-register with the ServiceGateway.
		m.pip.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
		m.sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

		dt := newTestDiffTracker()
		dt.config = testConfig()
		dt.networkClientFactory = m.factory
		dt.kubeClient = fake.NewSimpleClientset(&v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: types.UID(uid)}})
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
		m.expectInboundPIP(uid)
		mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
		m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
		expectCurrentLB(mockLB, uid)
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
		mockLB.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()

		unsupported := validConfig()
		unsupported.NamedTargetPorts = []string{"http"}

		dt := newTestDiffTracker()
		dt.config = testConfig()
		dt.networkClientFactory = m.factory
		got := &outboundCompletion{}
		outboundUpdater(dt, got).updateInboundService(uid, unsupported, "corr")

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
	m.expectInboundPIP(uid)
	mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
	m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	expectCurrentLB(mockLB, uid)

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
	dt.kubeClient = fake.NewSimpleClientset(&v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: types.UID(uid)}})

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
		mockLB.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
		m.factory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()

		// PIP and LoadBalancer create succeed, and the ServiceGateway registration succeeds, so the
		// service is genuinely live in Azure before the status write is attempted.
		m.pip.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
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

func TestServiceUpdater_OneServiceCreatesASharedNamedPublicIP(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(fmt.Sprintf("update=%v", update), func(t *testing.T) { testOneServiceCreatesASharedNamedPublicIP(t, update) })
	}
}

func testOneServiceCreatesASharedNamedPublicIP(t *testing.T, update bool) {
	ctrl := gomock.NewController(t)
	f := mock_azclient.NewMockClientFactory(ctrl)
	pipClient := mock_publicipaddressclient.NewMockInterface(ctrl)
	lbClient := mock_loadbalancerclient.NewMockInterface(ctrl)
	sgwClient := mock_servicegatewayclient.NewMockInterface(ctrl)
	f.EXPECT().GetPublicIPAddressClient().Return(pipClient).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(lbClient).AnyTimes()
	f.EXPECT().GetServiceGatewayClient().Return(sgwClient).AnyTimes()

	var mu sync.Mutex
	var shared *armnetwork.PublicIPAddress
	var creates, reads int32
	bothRead := make(chan struct{})
	// Azure names are case-insensitive, so the two Services spell the name differently.
	isShared := gomock.Cond(func(name any) bool { return strings.EqualFold(name.(string), "shared") })
	pipClient.EXPECT().Get(gomock.Any(), "rg", isShared, gomock.Any()).DoAndReturn(
		func(context.Context, string, string, *string) (*armnetwork.PublicIPAddress, error) {
			mu.Lock()
			var snapshot *armnetwork.PublicIPAddress
			if shared != nil {
				copied := *shared
				snapshot = &copied
			}
			mu.Unlock()
			// Without serialization both Services read before either creates, so both find the Public
			// IP missing; with it the second read only comes after the first create, so wait bounded.
			if atomic.AddInt32(&reads, 1) == 2 {
				close(bothRead)
			}
			select {
			case <-bothRead:
			case <-time.After(300 * time.Millisecond):
			}
			if snapshot == nil {
				return nil, notFoundError()
			}
			return snapshot, nil
		}).AnyTimes()
	pipClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", isShared, gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, pip armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
			atomic.AddInt32(&creates, 1)
			pip.Properties.IPAddress = ptr.To("20.0.0.1")
			mu.Lock()
			shared = &pip
			mu.Unlock()
			return &pip, nil
		}).AnyTimes()
	lbClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	lbClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	sgwClient.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	uids := []string{"77777777-7777-7777-7777-777777777771", "77777777-7777-7777-7777-777777777772"}
	kube := fake.NewSimpleClientset()
	for i, uid := range uids {
		_, err := kube.CoreV1().Services("ns").Create(context.Background(), &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("svc%d", i), Namespace: "ns", UID: types.UID(uid)},
		}, metav1.CreateOptions{})
		assert.NoError(t, err)
	}
	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.kubeClient = kube
	dt.networkClientFactory = f
	su := newTestServiceUpdater(dt)

	var wg sync.WaitGroup
	for i, uid := range uids {
		config := makeInboundConfig(80)
		config.ServiceName, config.ClusterName, config.PIPName = fmt.Sprintf("ns/svc%d", i), "cluster", []string{"shared", "Shared"}[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if update {
				su.updateInboundService(uid, config, "corr")
			} else {
				su.createInboundService(uid, config, "corr")
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), atomic.LoadInt32(&creates), "only one Service may create the Public IP it finds missing")
	if assert.NotNil(t, shared) {
		assert.True(t, ownsPublicIPByTags(shared, "ns/svc0", "cluster") != ownsPublicIPByTags(shared, "ns/svc1", "cluster"),
			"the Public IP must be owned by exactly one Service")
	}
}
