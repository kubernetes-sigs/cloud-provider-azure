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
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// newIntervalTestUpdater returns a LocationsUpdater whose tracker is configured with interval and
// whose ServiceGateway client is sgw. The updater is not started.
func newIntervalTestUpdater(ctx context.Context, t *testing.T, interval time.Duration, sgw *mock_servicegatewayclient.MockInterface) (*LocationsUpdater, *DiffTracker) {
	t.Helper()
	factory := mock_azclient.NewMockClientFactory(gomock.NewController(t))
	factory.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()

	dt := newTestDiffTracker()
	dt.networkClientFactory = factory
	dt.config = testConfig()
	dt.config.LocationsUpdateInterval = interval
	return NewLocationsUpdater(ctx, dt), dt
}

// addInboundPod adds a pod of the tracked inbound Service "svc" so the next pass has a location diff.
func addInboundPod(dt *DiffTracker, nodeName, podIP string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	node, ok := dt.K8sResources.Nodes[nodeName]
	if !ok {
		node = newNode()
		dt.K8sResources.Nodes[nodeName] = node
	}
	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node.Pods[podIP] = pod
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceUID: "svc", State: StateCreated}
}

func TestWaitForRunInterval(t *testing.T) {
	const interval = 200 * time.Millisecond
	sgw := mock_servicegatewayclient.NewMockInterface(gomock.NewController(t))

	t.Run("no wait before the first call", func(t *testing.T) {
		lu, _ := newIntervalTestUpdater(context.Background(), t, interval, sgw)
		start := time.Now()
		assert.True(t, lu.waitForRunInterval())
		assert.Less(t, time.Since(start), interval/2)
	})

	t.Run("no wait once the interval has passed", func(t *testing.T) {
		lu, _ := newIntervalTestUpdater(context.Background(), t, interval, sgw)
		lu.lastRunEnd = time.Now().Add(-2 * interval)
		start := time.Now()
		assert.True(t, lu.waitForRunInterval())
		assert.Less(t, time.Since(start), interval/2)
	})

	t.Run("waits for the rest of the interval", func(t *testing.T) {
		lu, _ := newIntervalTestUpdater(context.Background(), t, interval, sgw)
		lu.lastRunEnd = time.Now()
		assert.True(t, lu.waitForRunInterval())
		assert.GreaterOrEqual(t, time.Since(lu.lastRunEnd), interval)
	})

	t.Run("zero interval disables the wait", func(t *testing.T) {
		lu, _ := newIntervalTestUpdater(context.Background(), t, 0, sgw)
		lu.lastRunEnd = time.Now()
		start := time.Now()
		assert.True(t, lu.waitForRunInterval())
		assert.Less(t, time.Since(start), 50*time.Millisecond)
	})

	t.Run("returns promptly when stopped", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		lu, _ := newIntervalTestUpdater(ctx, t, 10*time.Second, sgw)
		lu.lastRunEnd = time.Now()
		time.AfterFunc(50*time.Millisecond, cancel)
		start := time.Now()
		assert.False(t, lu.waitForRunInterval())
		assert.Less(t, time.Since(start), 2*time.Second)
	})
}

// TestLocationsUpdater_NoOpRunsAlsoStartTheInterval checks that the spacing is between runs, not only
// between NRP calls: a run with nothing to send still delays the next run by the interval.
func TestLocationsUpdater_NoOpRunsAlsoStartTheInterval(t *testing.T) {
	const interval = 300 * time.Millisecond
	var (
		mu    sync.Mutex
		start time.Time
	)
	sgw := mock_servicegatewayclient.NewMockInterface(gomock.NewController(t))
	sgw.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			mu.Lock()
			defer mu.Unlock()
			start = time.Now()
			return nil
		}).Times(1)
	lu, dt := newIntervalTestUpdater(context.Background(), t, interval, sgw)

	stopped := make(chan struct{})
	go func() {
		lu.Run()
		close(stopped)
	}()
	defer func() {
		lu.Stop()
		<-stopped
	}()

	// The first run has no location diff, so it makes no NRP call.
	firstTrigger := time.Now()
	dt.triggerLocationsUpdater()
	time.Sleep(50 * time.Millisecond)

	addInboundPod(dt, "node-1", "10.0.0.1")
	dt.triggerLocationsUpdater()
	assert.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return !start.IsZero()
	}, 5*time.Second, 5*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, start.Sub(firstTrigger), interval,
		"the run after a no-op run must still wait for the interval")
}

// TestLocationsUpdater_SpacesConsecutiveNRPCalls drives Run: the first run's update is sent at once,
// and the next run starts no sooner than the interval after the previous run finished.
func TestLocationsUpdater_SpacesConsecutiveNRPCalls(t *testing.T) {
	const interval = 300 * time.Millisecond
	var (
		mu     sync.Mutex
		starts []time.Time
		ends   []time.Time
	)
	sgw := mock_servicegatewayclient.NewMockInterface(gomock.NewController(t))
	sgw.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _ string, _ armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			mu.Lock()
			defer mu.Unlock()
			starts = append(starts, time.Now())
			ends = append(ends, time.Now())
			return nil
		}).Times(2)
	lu, dt := newIntervalTestUpdater(context.Background(), t, interval, sgw)

	stopped := make(chan struct{})
	go func() {
		lu.Run()
		close(stopped)
	}()
	defer func() {
		lu.Stop()
		<-stopped
	}()
	calls := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(starts)
	}

	addInboundPod(dt, "node-1", "10.0.0.1")
	triggered := time.Now()
	dt.triggerLocationsUpdater()
	assert.Eventually(t, func() bool { return calls() == 1 }, 5*time.Second, 5*time.Millisecond)
	mu.Lock()
	assert.Less(t, starts[0].Sub(triggered), interval/2, "the first update must not be delayed")
	mu.Unlock()

	addInboundPod(dt, "node-2", "10.0.0.2")
	dt.triggerLocationsUpdater()
	assert.Eventually(t, func() bool { return calls() == 2 }, 5*time.Second, 5*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, starts[1].Sub(ends[0]), interval,
		"the next update must start at least the interval after the previous one returned")
}
