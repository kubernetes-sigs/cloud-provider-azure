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
