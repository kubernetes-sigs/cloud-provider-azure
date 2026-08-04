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
	"k8s.io/component-base/metrics/testutil"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// TestLocationsUpdaterRetriesAfterSyncFailure verifies that a failed NRP location sync is
// retried automatically (with backoff) rather than left unsynced until an unrelated future
// trigger.
func TestLocationsUpdaterRetriesAfterSyncFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	var calls int32
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			if atomic.AddInt32(&calls, 1) == 1 {
				return errors.New("service gateway unavailable")
			}
			return nil
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceUID: "svc", State: StateCreated}

	lu := NewLocationsUpdater(context.Background(), dt)
	stopped := make(chan struct{})
	go func() {
		lu.Run()
		close(stopped)
	}()
	defer func() {
		lu.Stop()
		<-stopped // wait for the Run goroutine to fully exit before the test returns
	}()

	dt.triggerLocationsUpdater()

	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) >= 2
	}, 5*time.Second, 20*time.Millisecond, "a failed location sync should be retried automatically")
}

// TestLocationsUpdater_HungSyncTimesOutAndRecovers proves the single LocationsUpdater worker is not
// pinned by a hung NRP call. The first UpdateAddressLocations blocks until its context is cancelled;
// only the per-attempt timeout (not the deadline-less component context) can unblock it, so the
// worker recovers and the backoff retry issues a second sync. Without the timeout the worker would
// stall on the first call forever, starving all other location/finalizer syncs cluster-wide.
func TestLocationsUpdater_HungSyncTimesOutAndRecovers(t *testing.T) {
	oldTimeout := getNRPOperationTimeout()
	nrpOperationTimeout.Store(int64(150 * time.Millisecond))
	defer nrpOperationTimeout.Store(int64(oldTimeout))

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	var calls int32
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(ctx context.Context, _, _ string, _ armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			if atomic.AddInt32(&calls, 1) == 1 {
				<-ctx.Done() // hang until the per-attempt timeout cancels this attempt
				return ctx.Err()
			}
			return nil
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceUID: "svc", State: StateCreated}

	lu := NewLocationsUpdater(context.Background(), dt)
	stopped := make(chan struct{})
	go func() {
		lu.Run()
		close(stopped)
	}()
	defer func() {
		lu.Stop()
		<-stopped
	}()

	dt.triggerLocationsUpdater()

	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) >= 2
	}, 5*time.Second, 20*time.Millisecond, "a hung location sync must time out so the worker recovers and retries")
}

// TestLocationsUpdaterBackoffShortCircuitsOnTrigger verifies that, post-initialization, a trigger
// buffered by a fresh cluster change during a failed sync's backoff wakes the retry immediately
// instead of waiting the full (up to locationsRetryMaxDelay) delay.
func TestLocationsUpdaterBackoffShortCircuitsOnTrigger(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.networkClientFactory = mock_azclient.NewMockClientFactory(ctrl)
	lu := NewLocationsUpdater(context.Background(), dt)

	// Force the delay to its ~30s cap so an early wake is unambiguous; isInitializing is 0 (runtime).
	lu.failureCount = 6

	done := make(chan struct{})
	start := time.Now()
	go func() {
		lu.backoffAndRetry()
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	dt.triggerLocationsUpdater()

	select {
	case <-done:
		assert.Less(t, time.Since(start), 5*time.Second, "a buffered trigger must short-circuit the backoff")
	case <-time.After(5 * time.Second):
		t.Fatal("backoffAndRetry did not wake on a buffered trigger within 5s")
	}
}

// TestLocationsUpdater_TerminalErrorNotRetried checks that a deterministic NRP rejection (HTTP 400)
// abandons the batch instead of retrying the identical payload forever.
func TestLocationsUpdater_TerminalErrorNotRetried(t *testing.T) {
	RegisterMetrics()
	before, err := testutil.GetCounterMetricValue(locationSyncTerminalErrorsTotal)
	assert.NoError(t, err)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	var calls int32
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			atomic.AddInt32(&calls, 1)
			return &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "InvalidRequest"}
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceUID: "svc", State: StateCreated}

	lu := NewLocationsUpdater(context.Background(), dt)
	stopped := make(chan struct{})
	go func() {
		lu.Run()
		close(stopped)
	}()
	defer func() {
		lu.Stop()
		<-stopped
	}()

	dt.triggerLocationsUpdater()

	// A self-reschedule would retry within the ~1s base backoff; wait past that so the single attempt
	// is unambiguous.
	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) == 1
	}, 2*time.Second, 20*time.Millisecond, "the terminal batch must be attempted once")
	assert.Never(t, func() bool {
		return atomic.LoadInt32(&calls) > 1
	}, 1500*time.Millisecond, 50*time.Millisecond, "a deterministic 400 must not be retried")

	after, err := testutil.GetCounterMetricValue(locationSyncTerminalErrorsTotal)
	assert.NoError(t, err)
	assert.Equal(t, float64(1), after-before, "a terminal location-sync error must be counted exactly once")
}

// TestLocationsUpdater_TerminalErrorStillDrainsUnrelatedDeletions verifies that abandoning a batch
// NRP rejected deterministically still advances unrelated deletions.
//
// LocationsUpdater.process is the only caller of CheckPendingServiceDeletions and
// CheckPendingPodDeletions, so returning early on a terminal (400/422) rejection without running
// them leaves every pending Service and egress-pod finalizer in place until the CCM restarts,
// blocking node drain and namespace deletion. A Service whose addresses are already absent from
// NRP is independently deletable and must still advance when another Service contributes a
// malformed entry to the shared batch.
func TestLocationsUpdater_TerminalErrorStillDrainsUnrelatedDeletions(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	var calls int32
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			atomic.AddInt32(&calls, 1)
			return &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "InvalidRequest"}
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	// "poison" contributes the location diff that NRP rejects with a deterministic 400.
	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("poison")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("poison")
	dt.pendingServiceOps["poison"] = &ServiceOperationState{ServiceUID: "poison", State: StateCreated}

	// "innocent" is unrelated: it holds no NRP addresses at all, so its drain is already complete
	// and CheckPendingServiceDeletions must promote it to StateDeletionInProgress.
	dt.pendingServiceOps["innocent"] = &ServiceOperationState{ServiceUID: "innocent", State: StateDeletionPending}
	dt.pendingServiceDeletions["innocent"] = &PendingServiceDeletion{ServiceUID: "innocent", IsInbound: true}

	lu := NewLocationsUpdater(context.Background(), dt)
	stopped := make(chan struct{})
	go func() {
		lu.Run()
		close(stopped)
	}()
	defer func() {
		lu.Stop()
		<-stopped
	}()

	dt.triggerLocationsUpdater()

	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) >= 1
	}, 2*time.Second, 20*time.Millisecond, "the terminal batch must be attempted")

	assert.Eventually(t, func() bool {
		dt.mu.Lock()
		defer dt.mu.Unlock()
		return dt.pendingServiceOps["innocent"].State == StateDeletionInProgress
	}, 2*time.Second, 20*time.Millisecond,
		"a Service with no NRP addresses must still drain when an unrelated Service poisons the batch")
}

// TestLocationsUpdater_InitDoesNotHangOnSustainedTransientError verifies that a retryable NRP error
// which never clears cannot block initialization indefinitely.
//
// backoffAndRetry re-triggers before the in-flight trigger counter is decremented, so
// initialization stays blocked until a sync succeeds. Unbounded, a sustained 503 keeps
// pendingUpdaterTriggers above zero, WaitForInitialSync never returns, and InitializeFromCluster,
// Runtime.Start and startServiceController all hang. startControllers runs sequentially and starts
// informers only afterwards, so every remaining CCM controller is left unstarted.
// TestLocationsUpdater_InitFailsLoudlyOnSustainedTransientError pins that a never-clearing
// transient NRP error is never abandoned during initialization. Abandoning retired the in-flight
// trigger, which let WaitForInitialSync return success and startup proceed against NRP state the
// sync had never reconciled - and discarded an NRP recovery arriving moments later.
//
// The contract is now: keep retrying, and if NRP has not recovered by the initial-sync deadline,
// surface that as an error so InitializeFromCluster fails loudly rather than silently continuing.
func TestLocationsUpdater_InitFailsLoudlyOnSustainedTransientError(t *testing.T) {
	prev := maxInitLocationSyncAttempts.Load()
	maxInitLocationSyncAttempts.Store(1)
	defer maxInitLocationSyncAttempts.Store(prev)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	var attempts int32
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	// 503 is retryable (not terminal): the updater must keep re-attempting it, not park it.
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			atomic.AddInt32(&attempts, 1)
			return &azcore.ResponseError{StatusCode: http.StatusServiceUnavailable, ErrorCode: "ServiceUnavailable"}
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceUID: "svc", State: StateCreated}

	// Put the tracker in the initializing state the real startup path uses.
	dt.initCompletionChecker = make(chan struct{})
	atomic.StoreInt32(&dt.isInitializing, 1)

	lu := NewLocationsUpdater(context.Background(), dt)
	stopped := make(chan struct{})
	go func() {
		lu.Run()
		close(stopped)
	}()
	defer func() {
		lu.Stop()
		<-stopped
	}()

	dt.triggerLocationsUpdater()

	// Stands in for the CCM root context ending (shutdown) while NRP is still down. The production
	// wait is unbounded, so this is what a caller-side cancellation looks like.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	assert.Error(t, dt.WaitForInitialSync(ctx),
		"initialization must not report success while the NRP location sync has never succeeded; "+
			"it must keep waiting and surface the cancellation instead")

	assert.Greater(t, atomic.LoadInt32(&attempts), int32(1),
		"the sync must keep being retried past the reporting threshold, not abandoned after it")
}

// TestLocationsUpdater_TerminalErrorRetriedWhileDeletionDrainPending verifies that a deterministic
// NRP rejection is still retried while a deletion waits on the abandoned batch. Abandoning it
// without rescheduling leaves the Service Terminating until the CCM restarts, because nothing
// re-drives the sync on a quiet cluster.
func TestLocationsUpdater_TerminalErrorRetriedWhileDeletionDrainPending(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	var calls int32
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			atomic.AddInt32(&calls, 1)
			return &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "InvalidRequest"}
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	// The pod is gone from Kubernetes but NRP still holds its address, so this batch is the removal
	// the pending deletion waits on.
	dt.K8sResources.Nodes["node-1"] = newNode()
	dt.NRPResources.Locations["node-1"] = NRPLocation{
		Addresses: map[string]NRPAddress{"10.0.0.1": {Services: utilsets.NewString("svc")}},
	}
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceUID: "svc", State: StateDeletionPending}
	dt.pendingServiceDeletions["svc"] = &PendingServiceDeletion{ServiceUID: "svc", IsInbound: true}

	lu := NewLocationsUpdater(context.Background(), dt)
	stopped := make(chan struct{})
	go func() {
		lu.Run()
		close(stopped)
	}()
	defer func() {
		lu.Stop()
		<-stopped
	}()

	dt.triggerLocationsUpdater()

	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) > 1
	}, 6*time.Second, 50*time.Millisecond,
		"a deletion waiting on the abandoned batch must keep the sync rescheduled, not strand it")
}

// TestLocationsUpdater_DrainKeepsRetryingPastAttemptCap pins that spending the retry budget does
// not end the retries while a finalizer is still blocked. Nothing else re-drives a drain-gated
// deletion, so giving up leaves the Service Terminating until some unrelated cluster change.
func TestLocationsUpdater_DrainKeepsRetryingPastAttemptCap(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	prev := maxInitLocationSyncAttempts.Load()
	maxInitLocationSyncAttempts.Store(1)
	defer maxInitLocationSyncAttempts.Store(prev)

	var calls int32
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			atomic.AddInt32(&calls, 1)
			return &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "InvalidRequest"}
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	dt.K8sResources.Nodes["node-1"] = newNode()
	dt.NRPResources.Locations["node-1"] = NRPLocation{
		Addresses: map[string]NRPAddress{"10.0.0.1": {Services: utilsets.NewString("svc")}},
	}
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceUID: "svc", State: StateDeletionPending}
	dt.pendingServiceDeletions["svc"] = &PendingServiceDeletion{ServiceUID: "svc", IsInbound: true}

	lu := NewLocationsUpdater(context.Background(), dt)
	stopped := make(chan struct{})
	go func() {
		lu.Run()
		close(stopped)
	}()
	defer func() {
		lu.Stop()
		<-stopped
	}()

	dt.triggerLocationsUpdater()

	// The cap is 1, so anything beyond a couple of attempts can only come from retries that
	// continued after the budget was spent.
	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) >= 3
	}, 10*time.Second, 50*time.Millisecond,
		"a drain-blocked deletion must keep retrying after the attempt cap, not be abandoned")
}

// TestLocationsUpdater_SendsExactAddressPayloadToNRP pins the CONTENT of the address-location
// request the updater sends to NRP, not merely that a request was sent.
//
// Every other LocationsUpdater test matches the request with gomock.Any(), so the updater could
// send a wrong node location, a wrong address, or an empty address list and the whole unit suite
// would still pass. This is the request that actually registers and drains pod IPs in Azure: a
// wrong location strands the address under a node that does not own it, and a wrong address list
// blackholes live traffic or leaks a dead pod's IP. Assert the exact wire payload.
func TestLocationsUpdater_SendsExactAddressPayloadToNRP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		nodeIP  = "10.0.0.1"
		podIP   = "10.244.0.7"
		svcUID  = "svc"
		wantRG  = "rg"
		wantSGW = "sgw"
	)

	captured := make(chan armnetwork.ServiceGatewayUpdateAddressLocationsRequest, 8)
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, rg, sgw string, req armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			assert.Equal(t, wantRG, rg, "the request must target the configured resource group")
			assert.Equal(t, wantSGW, sgw, "the request must target the configured ServiceGateway")
			captured <- req
			return nil
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString(svcUID)
	node := newNode()
	node.Pods[podIP] = pod
	dt.K8sResources.Nodes[nodeIP] = node
	dt.NRPResources.LoadBalancers.Insert(svcUID)
	dt.pendingServiceOps[svcUID] = &ServiceOperationState{ServiceUID: svcUID, State: StateCreated}

	lu := NewLocationsUpdater(context.Background(), dt)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		lu.Run()
	}()
	defer func() {
		lu.Stop()
		<-stopped
	}()

	dt.locationsUpdaterTrigger <- true

	var req armnetwork.ServiceGatewayUpdateAddressLocationsRequest
	select {
	case req = <-captured:
	case <-time.After(10 * time.Second):
		t.Fatal("the updater never sent an address-location request to NRP")
	}

	if !assert.Len(t, req.AddressLocations, 1, "exactly one node location must be sent") {
		return
	}
	loc := req.AddressLocations[0]
	if assert.NotNil(t, loc.AddressLocation) {
		assert.Equal(t, nodeIP, *loc.AddressLocation,
			"the address must be filed under the node that actually hosts the pod")
	}
	if !assert.Len(t, loc.Addresses, 1, "exactly the pod's address must be sent") {
		return
	}
	addr := loc.Addresses[0]
	if assert.NotNil(t, addr.Address) {
		assert.Equal(t, podIP, *addr.Address, "the registered address must be the pod IP")
	}
	names := make([]string, 0, len(addr.Services))
	for _, n := range addr.Services {
		if n != nil {
			names = append(names, *n)
		}
	}
	assert.Equal(t, []string{svcUID}, names,
		"the address must be attributed to the owning service, or NRP routes it for the wrong service")
}

// TestLocationsUpdater_StopWaitsForInFlightSync pins that Stop does not return while a sync is still
// running. Cancelling alone lets an initialization failure tear down and report while process is
// still inside an NRP call and about to mutate tracker state.
func TestLocationsUpdater_StopWaitsForInFlightSync(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once

	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			close(entered)
			<-release
			return nil
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceUID: "svc", State: StateCreated}

	lu := NewLocationsUpdater(context.Background(), dt)
	go lu.Run()
	defer releaseOnce.Do(func() { close(release) })

	dt.triggerLocationsUpdater()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sync never reached NRP")
	}

	stopped := make(chan struct{})
	go func() {
		lu.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while a sync was still in flight")
	case <-time.After(300 * time.Millisecond):
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the sync finished")
	}
}

// TestIsTerminalLocationSyncStatus_ClassifiesEveryStatus pins which NRP responses abandon a batch.
// Treating a retryable status as terminal drops the batch and leaves NRP stale; treating a
// deterministic rejection as retryable spins the single worker against a payload that can never be
// accepted.
func TestIsTerminalLocationSyncStatus_ClassifiesEveryStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		assert.True(t, isTerminalLocationSyncStatus(status),
			"status %d is a deterministic rejection and must not be retried", status)
	}
	for _, status := range []int{
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		http.StatusUnauthorized,
		http.StatusForbidden,
		0,
	} {
		assert.False(t, isTerminalLocationSyncStatus(status),
			"status %d is transient and must stay retryable", status)
	}
}
