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
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
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

	t.Run("no wait before the first update", func(t *testing.T) {
		lu, _ := newIntervalTestUpdater(context.Background(), t, interval, sgw)
		start := time.Now()
		assert.True(t, lu.waitForRunInterval())
		assert.Less(t, time.Since(start), interval/2)
	})

	t.Run("no wait once the interval has passed", func(t *testing.T) {
		lu, _ := newIntervalTestUpdater(context.Background(), t, interval, sgw)
		lu.lastUpdateEnd = time.Now().Add(-2 * interval)
		start := time.Now()
		assert.True(t, lu.waitForRunInterval())
		assert.Less(t, time.Since(start), interval/2)
	})

	t.Run("waits for the rest of the interval", func(t *testing.T) {
		const longInterval = 400 * time.Millisecond
		lu, _ := newIntervalTestUpdater(context.Background(), t, longInterval, sgw)
		lu.lastUpdateEnd = time.Now().Add(-longInterval * 3 / 4)
		start := time.Now()
		assert.True(t, lu.waitForRunInterval())
		assert.GreaterOrEqual(t, time.Since(lu.lastUpdateEnd), longInterval)
		assert.Less(t, time.Since(start), longInterval/2, "only the rest of the interval is waited")
	})

	t.Run("zero interval disables the wait", func(t *testing.T) {
		lu, _ := newIntervalTestUpdater(context.Background(), t, 0, sgw)
		lu.lastUpdateEnd = time.Now()
		start := time.Now()
		assert.True(t, lu.waitForRunInterval())
		assert.Less(t, time.Since(start), 50*time.Millisecond)
	})

	t.Run("returns promptly when stopped", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		lu, _ := newIntervalTestUpdater(ctx, t, 10*time.Second, sgw)
		lu.lastUpdateEnd = time.Now()
		time.AfterFunc(50*time.Millisecond, cancel)
		start := time.Now()
		assert.False(t, lu.waitForRunInterval())
		assert.Less(t, time.Since(start), 2*time.Second)
	})
}

// runIntervalTestUpdater starts lu and stops it when the test ends.
func runIntervalTestUpdater(t *testing.T, lu *LocationsUpdater) {
	t.Helper()
	stopped := make(chan struct{})
	go func() {
		lu.Run()
		close(stopped)
	}()
	t.Cleanup(func() {
		lu.Stop()
		<-stopped
	})
}

// updateRecorder records when each address-location update reaches NRP and when it returns, after
// latency, answering with the errors in answers in turn (nil once they run out).
type updateRecorder struct {
	mu           sync.Mutex
	starts, ends []time.Time
	answers      []error
	latency      time.Duration
}

func (r *updateRecorder) update(_ context.Context, _, _ string, _ armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
	r.mu.Lock()
	r.starts = append(r.starts, time.Now())
	var err error
	if len(r.answers) > 0 {
		err, r.answers = r.answers[0], r.answers[1:]
	}
	r.mu.Unlock()
	time.Sleep(r.latency)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends = append(r.ends, time.Now())
	return err
}

// calls returns how many updates have returned.
func (r *updateRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ends)
}

// TestLocationsUpdater_RunsWithNothingToSendDoNotDelayTheNextUpdate checks that the interval spaces the
// updates sent to NRP, not the runs: a run with nothing to send, which makes no NRP call, neither waits
// for an interval of its own making nor delays the next update.
func TestLocationsUpdater_RunsWithNothingToSendDoNotDelayTheNextUpdate(t *testing.T) {
	const interval = 600 * time.Millisecond

	t.Run("a run with nothing to send before the first update", func(t *testing.T) {
		rec := &updateRecorder{}
		sgw := mock_servicegatewayclient.NewMockInterface(gomock.NewController(t))
		sgw.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(rec.update).Times(1)
		lu, dt := newIntervalTestUpdater(context.Background(), t, interval, sgw)
		runIntervalTestUpdater(t, lu)

		dt.triggerLocationsUpdater() // no location diff: no NRP call
		time.Sleep(50 * time.Millisecond)
		addInboundPod(dt, "node-1", "10.0.0.1")
		triggered := time.Now()
		dt.triggerLocationsUpdater()

		assert.Eventually(t, func() bool { return rec.calls() == 1 }, 5*time.Second, 5*time.Millisecond)
		rec.mu.Lock()
		defer rec.mu.Unlock()
		assert.Less(t, rec.starts[0].Sub(triggered), interval/2, "a run that sent nothing must not delay the next update")
	})

	t.Run("a run with nothing to send after an update", func(t *testing.T) {
		rec := &updateRecorder{}
		sgw := mock_servicegatewayclient.NewMockInterface(gomock.NewController(t))
		sgw.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(rec.update).Times(2)
		lu, dt := newIntervalTestUpdater(context.Background(), t, interval, sgw)
		runIntervalTestUpdater(t, lu)

		addInboundPod(dt, "node-1", "10.0.0.1")
		dt.triggerLocationsUpdater()
		assert.Eventually(t, func() bool { return rec.calls() == 1 }, 5*time.Second, 5*time.Millisecond)
		rec.mu.Lock()
		firstEnd := rec.ends[0]
		rec.mu.Unlock()

		// This run waits for the interval after the update, then finds nothing to send.
		dt.triggerLocationsUpdater()
		time.Sleep(time.Until(firstEnd.Add(interval + 200*time.Millisecond)))
		addInboundPod(dt, "node-2", "10.0.0.2")
		triggered := time.Now()
		dt.triggerLocationsUpdater()

		assert.Eventually(t, func() bool { return rec.calls() == 2 }, 5*time.Second, 5*time.Millisecond)
		rec.mu.Lock()
		defer rec.mu.Unlock()
		assert.Less(t, rec.starts[1].Sub(triggered), interval/3,
			"the interval since the last update has passed: the run that sent nothing in between must not restart it")
	})
}

// TestLocationsUpdater_FailedUpdateAlsoStartsTheInterval checks that an update NRP did not accept counts as
// an update sent: the next one, which the next change starts, is still spaced by the interval.
func TestLocationsUpdater_FailedUpdateAlsoStartsTheInterval(t *testing.T) {
	const interval = 300 * time.Millisecond
	for _, tc := range []struct {
		name   string
		answer error
	}{
		// A 400 is final for that batch: nothing retries it, only the next change.
		{name: "refused", answer: &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "InvalidRequest"}},
		// A 429 is retried after a backoff, which a change arriving meanwhile ends at once.
		{name: "throttled", answer: &azcore.ResponseError{StatusCode: http.StatusTooManyRequests, ErrorCode: "TooManyRequests"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &updateRecorder{answers: []error{tc.answer}}
			sgw := mock_servicegatewayclient.NewMockInterface(gomock.NewController(t))
			sgw.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(rec.update).Times(2)
			lu, dt := newIntervalTestUpdater(context.Background(), t, interval, sgw)
			runIntervalTestUpdater(t, lu)

			addInboundPod(dt, "node-1", "10.0.0.1")
			dt.triggerLocationsUpdater()
			assert.Eventually(t, func() bool { return rec.calls() == 1 }, 5*time.Second, 5*time.Millisecond)
			addInboundPod(dt, "node-2", "10.0.0.2")
			dt.triggerLocationsUpdater()

			assert.Eventually(t, func() bool { return rec.calls() == 2 }, 5*time.Second, 5*time.Millisecond)
			rec.mu.Lock()
			defer rec.mu.Unlock()
			gap := rec.starts[1].Sub(rec.ends[0])
			assert.GreaterOrEqual(t, gap, interval, "the update after a failed one must wait for the interval")
			assert.Less(t, gap, locationsRetryBaseDelay, "control: the next change, not a retry backoff, started the update")
		})
	}
}

// TestLocationsUpdater_SpacesConsecutiveNRPCalls drives Run: the first update is sent at once, and the
// next one starts no sooner than the interval after the previous one returned.
func TestLocationsUpdater_SpacesConsecutiveNRPCalls(t *testing.T) {
	const interval = 300 * time.Millisecond
	// The latency tells the return of an update from its start.
	rec := &updateRecorder{latency: 150 * time.Millisecond}
	sgw := mock_servicegatewayclient.NewMockInterface(gomock.NewController(t))
	sgw.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(rec.update).Times(2)
	lu, dt := newIntervalTestUpdater(context.Background(), t, interval, sgw)
	runIntervalTestUpdater(t, lu)

	addInboundPod(dt, "node-1", "10.0.0.1")
	triggered := time.Now()
	dt.triggerLocationsUpdater()
	assert.Eventually(t, func() bool { return rec.calls() == 1 }, 5*time.Second, 5*time.Millisecond)
	rec.mu.Lock()
	assert.Less(t, rec.starts[0].Sub(triggered), interval/2, "the first update must not be delayed")
	rec.mu.Unlock()

	addInboundPod(dt, "node-2", "10.0.0.2")
	dt.triggerLocationsUpdater()
	assert.Eventually(t, func() bool { return rec.calls() == 2 }, 5*time.Second, 5*time.Millisecond)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.GreaterOrEqual(t, rec.starts[1].Sub(rec.ends[0]), interval,
		"the next update must start at least the interval after the previous one returned")
}
