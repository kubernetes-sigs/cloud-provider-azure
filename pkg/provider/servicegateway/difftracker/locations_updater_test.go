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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/component-base/metrics/testutil"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
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

// addInboundPod adds a pod of the tracked inbound Service so the next pass has a location diff.
func addInboundPod(dt *DiffTracker, nodeName, podIP string) {
	serviceName := inboundServiceName("svc")
	dt.mu.Lock()
	defer dt.mu.Unlock()
	node, ok := dt.K8sResources.Nodes[nodeName]
	if !ok {
		node = newNode()
		dt.K8sResources.Nodes[nodeName] = node
	}
	pod := newPod()
	pod.InboundIdentities = utilsets.NewString(serviceName)
	node.Pods[podIP] = pod
	dt.NRPResources.LoadBalancers.Insert(serviceName)
	dt.pendingServiceOps[serviceName] = &ServiceOperationState{
		ServiceName: serviceName, State: StateCreated,
		Config: NewInboundServiceConfig(serviceName, &InboundConfig{
			FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
			BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
		}),
	}
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

func mockNSGLocationUpdates(t *testing.T, dt *DiffTracker, update func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error) {
	t.Helper()
	factory := dt.networkClientFactory.(*mock_azclient.MockClientFactory)
	gateway := mock_servicegatewayclient.NewMockInterface(gomock.NewController(t))
	factory.EXPECT().GetServiceGatewayClient().Return(gateway).AnyTimes()
	gateway.EXPECT().UpdateAddressLocations(gomock.Any(), "network-rg", "sgw", gomock.Any()).
		DoAndReturn(func(ctx context.Context, _, _ string, request armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			return update(ctx, request)
		}).AnyTimes()
}

func startNSGTestLocations(t *testing.T, dt *DiffTracker, update func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error) *LocationsUpdater {
	t.Helper()
	mockNSGLocationUpdates(t, dt, update)
	updater := NewLocationsUpdater(context.Background(), dt)
	dt.locationsUpdater = updater
	go updater.Run()
	t.Cleanup(updater.Stop)
	return updater
}

func TestLocationsUpdaterDoesNotDeriveOwnerFromLBOnlyPreparation(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprintf("lbOnly=%t", limited), func(t *testing.T) {
			first, second := nsgKubeService("first-lb", ""), nsgKubeService("second-lb", "")
			first.Annotations, second.Annotations = nil, nil
			if !limited {
				second.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
			}
			dt, cloud, _ := newNSGTestTracker(t, first, second)
			addNSGTestBackend(dt, "first-lb", "10.0.0.1", "10.1.0.1", "")
			addNSGTestBackend(dt, "second-lb", "10.0.0.1", "10.1.0.1", "")
			dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, nil))
			assert.Nil(t, dt.NRPResources.Locations["10.0.0.1"].nodeOwner)

			first.Annotations = map[string]string{consts.ServiceAnnotationAllowedIPRanges: "20.0.0.0/8"}
			assert.NoError(t, dt.ReconcileInboundService(first))
			second.Spec.Ports[0].TargetPort = intstr.FromInt32(10080)
			assert.NoError(t, dt.ReconcileInboundService(second))
			addNSGTestBackend(dt, "second-lb", "10.0.0.1", "10.1.0.1", "egress-natgw")
			calls := 0
			mockNSGLocationUpdates(t, dt, func(_ context.Context, request armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
				calls++
				if assert.Len(t, request.AddressLocations, 1) && assert.Len(t, request.AddressLocations[0].Addresses, 1) {
					assert.Len(t, request.AddressLocations[0].Addresses[0].Services, 3)
				}
				if limited {
					assert.Nil(t, dt.pendingLocationUpdates["10.0.0.1"].nodeOwner, "LB-only evidence must not become publication ownership")
					assert.Zero(t, cloud.groupWrites)
				} else {
					assert.NotNil(t, dt.pendingLocationUpdates["10.0.0.1"].nodeOwner)
					assert.Equal(t, 2, cloud.groupWrites)
				}
				return nil
			})
			updater := &LocationsUpdater{diffTracker: dt, ctx: context.Background(), logger: dt.logger}
			updater.process(context.Background())

			assert.Equal(t, 1, calls)
			assert.True(t, dt.pendingServiceOps["second-lb"].NSGPrepared, "the safe LB target must remain permitted")
			assert.Empty(t, dt.pendingLocationUpdates)
			location := dt.NRPResources.Locations["10.0.0.1"]
			assert.True(t, location.Addresses["10.1.0.1"].Services.Equals(utilsets.NewString("first-lb", "second-lb", "egress-natgw")))
			if limited {
				assert.Equal(t, 1, updater.failureCount)
				assert.Nil(t, location.nodeOwner, "an unrelated membership ACK must not promote LB-only evidence")
			} else {
				assert.Zero(t, updater.failureCount)
				assert.NotNil(t, location.nodeOwner)
			}
		})
	}
}

func TestLocationsUpdaterAdvancesResolvedPodServiceCleanup(t *testing.T) {
	const a, b = "egress-a-natgw", "egress-b-natgw"
	for _, tt := range []struct {
		name           string
		alreadyPending bool
		initializing   bool
	}{
		{name: "quiet post-init resolution"},
		{name: "already pending cleanup", alreadyPending: true},
		{name: "initialization retry", initializing: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			label := "egress-b"
			pod := unlabelledFinalizerPod(&label, "uid-p")
			now := metav1.Now()
			pod.DeletionTimestamp = &now
			kube := fake.NewSimpleClientset(pod)
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			dt.networkClientFactory = mock_azclient.NewMockClientFactory(gomock.NewController(t))
			dt.config = testConfig()
			dt.NRPResources.NATGateways.Insert(a, b)
			dt.K8sResources.Egresses.Insert(a, b)
			dt.K8sResources.Nodes["10.0.0.1"] = Node{Pods: map[string]Pod{
				"10.244.0.9": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: b, OutboundPodKey: "ns/q", OutboundPodUID: "uid-q"},
			}}
			dt.outboundIdentityPodRefCount.Store(b, 1)
			dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
				"10.244.0.9": {Services: utilsets.NewString(b)},
			}}
			for _, service := range []string{a, b} {
				dt.pendingServiceOps[service] = &ServiceOperationState{
					ServiceName: service, Config: NewOutboundServiceConfig(service, nil), State: StateCreated,
				}
			}
			dt.pendingPodDeletions["ns/p"] = &PendingPodDeletion{
				Namespace: "ns", Name: "p", UID: "uid-p",
				ResolutionPending:   true,
				ResolutionAddresses: []string{"10.244.0.7"}, ResolutionServices: []string{a, b},
				ServiceCleanup: map[string]podCleanupState{
					a: {Addresses: []string{"10.244.0.7"}, WaitForServiceDeletion: tt.alreadyPending},
					b: {Addresses: []string{"10.244.0.7"}},
				},
			}
			if tt.alreadyPending {
				dt.mu.Lock()
				dt.markOutboundServiceForDeletionLocked(a)
				dt.mu.Unlock()
			}
			if tt.initializing {
				atomic.StoreInt32(&dt.isInitializing, 1)
			}
			assert.Empty(t, dt.GetSyncLocationsAddresses().Locations)
			assert.Empty(t, dt.locationsUpdaterTrigger)
			assert.Empty(t, dt.serviceUpdaterTrigger)
			lu := NewLocationsUpdater(context.Background(), dt)
			go lu.Run()
			defer lu.Stop()
			dt.triggerLocationsUpdater()
			assert.Eventually(t, func() bool {
				dt.mu.Lock()
				defer dt.mu.Unlock()
				return dt.pendingServiceOps[a].State == StateDeletionInProgress
			}, 3*time.Second, 10*time.Millisecond, "resolved cleanup must advance without another informer event")
			got, err := kube.CoreV1().Pods("ns").Get(context.Background(), "p", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.True(t, hasPodFinalizer(got), "A's worker still owns finalizer completion")
		})
	}
}

func TestLocationsUpdaterRecoverySnapshotPreservesOnlyEligibleReferences(t *testing.T) {
	dt, _, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.2", "")
	node := dt.K8sResources.Nodes["10.0.0.1"]
	outbound := newPod()
	outbound.PublicOutboundIdentity = "egress-natgw"
	node.Pods["10.1.0.3"] = outbound
	dt.K8sResources.Nodes["10.0.0.1"] = node
	dt.NRPResources.NATGateways.Insert("egress-natgw")
	dt.NRPResources.Locations = map[string]NRPLocation{
		"10.0.0.1":  {Addresses: map[string]NRPAddress{"10.1.0.1": {Services: utilsets.NewString("svc-lb")}}},
		"10.0.0.9":  {Addresses: map[string]NRPAddress{"10.9.0.1": {Services: utilsets.NewString("old-lb", "default-natgw")}}},
		"10.0.0.99": {Addresses: map[string]NRPAddress{"10.99.0.1": {Services: utilsets.NewString("old-lb")}}},
	}
	dt.locationRecoveryPending = true
	snapshot := nsgTestLocationSync(dt, nil)
	assert.Equal(t, FullUpdate, snapshot.Action)
	assert.Len(t, snapshot.Locations, 2)
	assert.True(t, snapshot.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
	assert.True(t, snapshot.Locations["10.0.0.1"].Addresses["10.1.0.3"].ServiceRef.Has("egress-natgw"))
	assert.NotContains(t, snapshot.Locations["10.0.0.1"].Addresses, "10.1.0.2", "unconfirmed protected additions stay withheld")
	assert.True(t, snapshot.Locations["10.0.0.9"].Addresses["10.9.0.1"].ServiceRef.Equals(utilsets.NewString("default-natgw")))
	assert.Contains(t, dt.NRPResources.Locations, "10.0.0.99", "an attempt is not an ACK")
	assert.True(t, dt.locationRecoveryPending)
	dt.UpdateLocationsAddresses(snapshot)
	assert.Len(t, dt.NRPResources.Locations, 2, "root FullUpdate removes omitted locations")
	assert.NotContains(t, dt.NRPResources.Locations, "10.0.0.99")
	assert.True(t, dt.NRPResources.Locations["10.0.0.9"].Addresses["10.9.0.1"].Services.Equals(utilsets.NewString("default-natgw")))
	assert.False(t, dt.locationRecoveryPending)
	assert.Empty(t, dt.pendingLocationUpdates)
	next := nsgTestLocationSync(dt, nil)
	assert.Equal(t, PartialUpdate, next.Action)
	assert.Empty(t, next.Locations, "reserved references must not cause repeated no-op PUTs")
}

func TestLocationsUpdaterWithdrawsDiscoveredOwnerOutsideWork(t *testing.T) {
	ctx := context.Background()
	dt, _, node := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))

	other := node.DeepCopy()
	other.Name, other.UID, other.Spec.ProviderID = "other", "other-uid", ""
	other.Status.Addresses = []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.2"}}
	_, err := dt.kubeClient.CoreV1().Nodes().Create(ctx, other, metav1.CreateOptions{})
	assert.NoError(t, err)
	assert.NoError(t, dt.kubeClient.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{}))
	setTestNodeLister(t, dt, other)
	dt.dirtyNICPolicies.Insert("10.0.0.2")
	assert.Empty(t, dt.GetSyncLocationsAddresses().Locations, "the ordinary membership values have not changed")

	calls := 0
	mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls++
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
	updater.process(ctx)

	assert.Equal(t, 1, calls, "a departed owner discovered outside the selected work must still be withdrawn")
	assert.NotContains(t, dt.NRPResources.Locations, "10.0.0.1")
	assert.Contains(t, dt.K8sResources.Nodes["10.0.0.1"].Pods, "10.1.0.1", "the test must not rely on an EndpointSlice removal")
}

func TestLocationsUpdaterCorrectsUnknownMembershipWithDefaultReference(t *testing.T) {
	ctx := context.Background()
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.1.0.9": {Services: utilsets.NewString("default-natgw")},
		},
	}
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	attempt := nsgTestLocationSync(dt, prepareNSGTest(t, dt))
	assert.True(t, attempt.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
	dt.DeleteService("svc-lb", true, false)

	calls := 0
	mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls++
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
	updater.process(ctx)
	assert.Equal(t, 1, calls, "an unknown publication requires a corrective ACK even when the last acknowledged state is unchanged")
	assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.9"].Services.Equals(utilsets.NewString("default-natgw")))
	assert.NotContains(t, dt.NRPResources.Locations["10.0.0.1"].Addresses, "10.1.0.1")
	assert.Empty(t, dt.pendingLocationUpdates)
	assert.Equal(t, StateDeletionInProgress, dt.pendingServiceOps["svc-lb"].State)
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 2)
	}
}

func TestLocationsUpdaterRetainsPodFinalizersForUnknownPublications(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		for _, noIP := range []bool{false, true} {
			t.Run(fmt.Sprintf("unknown=%t/no-IP=%t", unknown, noIP), func(t *testing.T) {
				ctx := context.Background()
				const service, location, deleted, survivor = "egress-natgw", "10.0.0.1", "10.1.0.1", "10.1.0.2"
				dt, _, _ := newNSGTestTracker(t)
				pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: "deleted", Namespace: "test", UID: "deleted-uid",
					Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
				}}
				_, err := dt.kubeClient.CoreV1().Pods("test").Create(ctx, pod, metav1.CreateOptions{})
				assert.NoError(t, err)
				dt.NRPResources.NATGateways.Insert(service)
				dt.pendingServiceOps[service] = &ServiceOperationState{
					ServiceName: service, Config: NewOutboundServiceConfig(service, nil), State: StateCreated,
				}
				dt.AddPod(service, "test/deleted", location, deleted)
				dt.AddPod(service, "test/survivor", location, survivor)
				remote := map[string]bool{}
				calls := 0
				mockNSGLocationUpdates(t, dt, func(_ context.Context, request armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
					calls++
					if unknown && calls == 2 {
						return &azcore.ResponseError{StatusCode: http.StatusBadRequest}
					}
					for _, loc := range request.AddressLocations {
						if *loc.AddressUpdateAction == armnetwork.AddressUpdateActionFullUpdate {
							clear(remote)
						}
						for _, address := range loc.Addresses {
							delete(remote, *address.Address)
							for _, name := range address.Services {
								if *name == service {
									remote[*address.Address] = true
								}
							}
						}
					}
					if unknown && calls == 1 {
						return errors.New("addition applied but response lost")
					}
					return nil
				})
				updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
				updater.process(ctx)
				assert.True(t, remote[deleted])
				if noIP {
					// No-IP recovery can encounter an already-retired local Pod entry.
					dt.mu.Lock()
					dt.removeLivePodsByKeyLocked(service, "test/deleted", string(pod.UID))
					dt.mu.Unlock()
					result := dt.DeletePodWithoutAddresses(service, "test", "deleted", string(pod.UID))
					assert.True(t, result.Enqueued)
					assert.False(t, result.IsLastPod)
					assert.True(t, dt.pendingPodDeletions["test/deleted"].VerifyServiceDrain)
				} else {
					result := dt.DeletePod(service, location, []string{deleted}, "test", "deleted", string(pod.UID))
					assert.True(t, result.Enqueued)
					assert.False(t, result.IsLastPod)
				}
				for len(dt.locationsUpdaterTrigger) > 0 {
					<-dt.locationsUpdaterTrigger
				}
				updater.process(ctx)
				if unknown {
					current, err := dt.kubeClient.CoreV1().Pods("test").Get(ctx, "deleted", metav1.GetOptions{})
					assert.NoError(t, err)
					assert.True(t, remote[deleted], "the rejected correction did not change the remote mapping")
					assert.Contains(t, current.Finalizers, ServiceGatewayPodCleanupFinalizer, "uncertain publication still owns the Pod drain")
					assert.Contains(t, dt.pendingPodDeletions, "test/deleted")
					assert.Positive(t, updater.failureCount, "a journal-backed Pod drain must retain the existing retry")
					select {
					case <-dt.locationsUpdaterTrigger:
						updater.process(ctx)
					default:
						t.Fatal("the rejected Pod drain did not schedule its corrective retry")
					}
				}
				current, err := dt.kubeClient.CoreV1().Pods("test").Get(ctx, "deleted", metav1.GetOptions{})
				assert.NoError(t, err)
				assert.NotContains(t, current.Finalizers, ServiceGatewayPodCleanupFinalizer)
				assert.Empty(t, dt.pendingPodDeletions)
				assert.Empty(t, dt.pendingLocationUpdates)
				assert.Equal(t, map[string]bool{survivor: true}, remote)
			})
		}
	}
}

func TestLocationsUpdaterCompletesLBPreparationAfterWithdrawal(t *testing.T) {
	ctx := context.Background()
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, _, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
	dt.NRPResources.Locations["10.0.0.2"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.2.0.1": {Services: utilsets.NewString("svc-lb")},
		},
	}
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	assert.NoError(t, dt.ReconcileInboundService(service))
	for len(dt.serviceUpdaterTrigger) > 0 {
		<-dt.serviceUpdaterTrigger
	}
	worker := newTestServiceUpdater(dt)
	worker.processBatch()
	worker.wg.Wait()
	for len(dt.locationsUpdaterTrigger) > 0 {
		<-dt.locationsUpdaterTrigger
	}
	calls := 0
	mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls++
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
	updater.process(ctx)
	assert.NotContains(t, dt.NRPResources.Locations, "10.0.0.2")
	assert.True(t, dt.pendingServiceOps["svc-lb"].NSGPrepared, "the post-ACK gate can complete in the same pass")
	assert.NotEmpty(t, dt.serviceUpdaterTrigger)
	updater.process(ctx)
	assert.True(t, dt.pendingServiceOps["svc-lb"].NSGPrepared)
	assert.NotEmpty(t, dt.serviceUpdaterTrigger, "a completed gate must wake the existing ServiceUpdater")
	assert.Empty(t, dt.dirtyNICPolicies)
	assert.Equal(t, 1, calls)
}

func TestLocationsUpdaterInitializationDrainWakesLBUpdate(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	op := dt.pendingServiceOps["svc-lb"]
	op.State, op.LastAppliedConfig = StateUpdateInProgress, nil
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{
		Addresses: map[string]NRPAddress{"10.1.0.1": {Services: utilsets.NewString("svc-lb")}},
	}
	dt.mu.Lock()
	assert.False(t, dt.readyForLBUpdateLocked("svc-lb"))
	dt.mu.Unlock()
	for len(dt.serviceUpdaterTrigger) > 0 {
		<-dt.serviceUpdaterTrigger
	}
	dt.locationRecoveryPending = true
	dt.initCompletionChecker = make(chan struct{})
	atomic.StoreInt32(&dt.isInitializing, 1)
	atomic.StoreInt32(&dt.pendingUpdaterTriggers, 1)
	mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: context.Background(), logger: dt.logger}
	updater.process(context.Background())
	assert.Empty(t, dt.NRPResources.Locations)
	assert.NotEmpty(t, dt.serviceUpdaterTrigger, "the initial drain must re-drive the formerly blocked LB operation")
	assert.False(t, dt.InitialSyncDone)
	assert.Nil(t, op.LastAppliedConfig, "a wakeup is not an LB configuration ACK")
	assert.Zero(t, cloud.groupReads, "initialization must not perform per-Service NSG work")
}

func TestLocationsUpdaterRetriesIncompleteLBPreparation(t *testing.T) {
	ctx := context.Background()
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, _, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.2", "10.2.0.1", "")
	dt.NRPResources.Locations["10.0.0.2"] = NRPLocation{
		Addresses: map[string]NRPAddress{"10.2.0.1": {Services: utilsets.NewString("svc-lb")}},
	}
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	assert.NoError(t, dt.ReconcileInboundService(service))
	worker := newTestServiceUpdater(dt)
	worker.processBatch()
	worker.wg.Wait()
	calls := 0
	mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls++
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
	updater.process(ctx)
	assert.Zero(t, calls)
	assert.False(t, dt.pendingServiceOps["svc-lb"].NSGPrepared)
	assert.True(t, dt.dirtyNICPolicies.Has("10.0.0.1"), "a partial gate must retain the successful NIC's obligation too")
	assert.True(t, dt.dirtyNICPolicies.Has("10.0.0.2"))

	dt.UpdateEndpoints("svc-lb", map[string]string{"10.2.0.1": "10.0.0.2"}, nil)
	updater.process(ctx)
	assert.Equal(t, 1, calls)
	assert.True(t, dt.pendingServiceOps["svc-lb"].NSGPrepared)
	assert.NotEmpty(t, dt.serviceUpdaterTrigger)
	assert.Empty(t, dt.dirtyNICPolicies)
}

func TestLocationsUpdaterRetainsEveryUnacknowledgedPublication(t *testing.T) {
	ctx := context.Background()
	first := nsgKubeService("first-lb", "20.0.0.0/8")
	second := nsgKubeService("second-lb", "40.0.0.0/8")
	second.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	dt, cloud, _ := newNSGTestTracker(t, first, second)
	addNSGTestBackend(dt, "first-lb", "10.0.0.1", "10.1.0.1", "")
	calls := 0
	mockNSGLocationUpdates(t, dt, func(_ context.Context, request armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls++
		if calls < 3 {
			return errors.New("membership response lost")
		}
		assert.Len(t, request.AddressLocations, 1)
		for _, group := range cloud.groups {
			for _, port := range []int32{8080, 9090} {
				assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
					nsgTestAccess(t, group.Properties.SecurityRules, "60.1.2.3", "10.1.0.1", port, armnetwork.SecurityRuleProtocolTCP))
			}
		}
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
	updater.process(ctx)
	dt.UpdateEndpoints("first-lb", map[string]string{"10.1.0.1": "10.0.0.1"}, nil)
	addNSGTestBackend(dt, "second-lb", "10.0.0.1", "10.1.0.1", "")
	updater.process(ctx)
	assert.True(t, dt.pendingLocationUpdates["10.0.0.1"].Addresses["10.1.0.1"].Services.Equals(utilsets.NewString("first-lb", "second-lb")))
	updater.process(ctx)
	assert.Equal(t, 3, calls)
	assert.Empty(t, dt.pendingLocationUpdates)
	assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Equals(utilsets.NewString("second-lb")))
	for _, group := range cloud.groups {
		assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
			nsgTestAccess(t, group.Properties.SecurityRules, "60.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
	}
}

func TestLocationsUpdaterCompletesProtectedPublicationInOnePass(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	for i := 1; i <= 100; i++ {
		addNSGTestBackend(dt, "svc-lb", "10.0.0.1", fmt.Sprintf("10.1.0.%d", i), "")
	}
	calls := 0
	mockNSGLocationUpdates(t, dt, func(_ context.Context, request armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls++
		if assert.Len(t, request.AddressLocations, 1) {
			assert.Len(t, request.AddressLocations[0].Addresses, 100)
		}
		assert.Equal(t, 2, cloud.groupWrites, "rules and durable NIC confirmation precede publication")
		assert.Equal(t, 1, cloud.nicWrites)
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: context.Background(), logger: dt.logger}
	updater.process(context.Background())
	assert.Equal(t, 1, calls, "the pass must prepare and publish, not finish and wait for another NSG worker")
	assert.Len(t, dt.NRPResources.Locations["10.0.0.1"].Addresses, 100)
	assert.Empty(t, dt.dirtyNICPolicies)
	assert.Equal(t, 1, cloud.groupReads, "a pure addition must not reread the NSG after its ACK")
}

func TestLocationsUpdaterRetriesFailedNSGWork(t *testing.T) {
	for _, tc := range []struct {
		name            string
		withdrawal      bool
		existingBinding bool
		nrpError        error
	}{
		{name: "no address diff"},
		{name: "successful withdrawal", withdrawal: true},
		{name: "terminal withdrawal", withdrawal: true, nrpError: &azcore.ResponseError{StatusCode: http.StatusBadRequest}},
		{name: "policy update while an existing binding gains egress", existingBinding: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restricted := nsgKubeService("restricted-lb", "20.0.0.0/8")
			unrestricted := nsgKubeService("unrestricted-lb", "")
			unrestricted.Annotations = nil
			dt, cloud, _ := newNSGTestTracker(t, restricted, unrestricted)
			if tc.withdrawal {
				addNSGTestBackend(dt, "unrestricted-lb", "10.0.0.1", "10.1.0.2", "")
				dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
				dt.UpdateEndpoints("unrestricted-lb", map[string]string{"10.1.0.2": "10.0.0.1"}, nil)
			}
			egress := ""
			if tc.existingBinding {
				addNSGTestBackend(dt, "restricted-lb", "10.0.0.1", "10.1.0.1", "")
				dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
				restricted.Annotations[consts.ServiceAnnotationAllowedIPRanges] = "40.0.0.0/8"
				assert.NoError(t, dt.ReconcileInboundService(restricted))
				egress = "egress-natgw"
			}
			addNSGTestBackend(dt, "restricted-lb", "10.0.0.1", "10.1.0.1", egress)
			cloud.groupError = errors.New("NSG temporarily unavailable")
			calls := 0
			mockNSGLocationUpdates(t, dt, func(_ context.Context, request armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
				calls++
				for _, location := range request.AddressLocations {
					for _, address := range location.Addresses {
						if tc.existingBinding {
							assert.Len(t, address.Services, 2, "safe egress progress must retain the existing inbound binding")
						} else {
							assert.Empty(t, address.Services, "only the already-safe withdrawal may proceed")
						}
					}
				}
				return tc.nrpError
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
			updater.process(ctx)
			if tc.withdrawal || tc.existingBinding {
				assert.Equal(t, 1, calls)
			} else {
				assert.Zero(t, calls)
			}
			assert.Equal(t, 1, updater.failureCount, "no-op or successful NRP work must not hide failed NSG work")
			assert.NotEmpty(t, dt.dirtyNICPolicies)
			if tc.existingBinding {
				assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Equals(utilsets.NewString("restricted-lb", "egress-natgw")))
			} else if tc.nrpError == nil {
				assert.Empty(t, dt.NRPResources.Locations)
			} else {
				assert.Len(t, dt.NRPResources.Locations, 1, "failed withdrawal must retain acknowledged state")
			}
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

func TestLocationsUpdaterRetriesUnresolvedPodWithoutLocationChanges(t *testing.T) {
	pod := unlabelledFinalizerPod(nil, "uid-p")
	kube := fake.NewSimpleClientset(pod)
	var gets atomic.Int32
	kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if gets.Add(1) <= 2 {
			return true, nil, apierrors.NewServiceUnavailable("transient Pod lookup")
		}
		return false, nil, nil
	})
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	dt.networkClientFactory = mock_azclient.NewMockClientFactory(gomock.NewController(t))
	dt.config = testConfig()
	dt.releaseUnlabelledPodFinalizer(context.Background(), pod, false)
	pending := dt.pendingPodDeletions["ns/p"]
	if !assert.NotNil(t, pending) {
		return
	}
	assert.True(t, pending.ResolutionPending)
	lu := NewLocationsUpdater(context.Background(), dt)
	done := make(chan struct{})
	go func() {
		defer close(done)
		lu.Run()
	}()
	defer func() {
		lu.Stop()
		<-done
	}()
	assert.Eventually(t, func() bool {
		obj, err := kube.Tracker().Get(v1.SchemeGroupVersion.WithResource("pods"), "ns", "p")
		if err != nil || hasPodFinalizer(obj.(*v1.Pod)) {
			return false
		}
		dt.mu.Lock()
		defer dt.mu.Unlock()
		return len(dt.pendingPodDeletions) == 0 && len(dt.podFinalizerGuards) == 0
	}, 5*time.Second, 20*time.Millisecond, "a quiet cluster must retry discovery without another event")
	assert.GreaterOrEqual(t, gets.Load(), int32(4))
	dt.mu.Lock()
	assert.Empty(t, dt.pendingPodDeletions)
	assert.Empty(t, dt.podFinalizerGuards)
	dt.mu.Unlock()
}

func TestLocationsUpdaterPrepares100BackendsBeforePublication(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		t.Run(fmt.Sprintf("restricted=%t", restricted), func(t *testing.T) {
			service := nsgKubeService("svc-lb", "20.0.0.0/8")
			if !restricted {
				service.Annotations = nil
			}
			dt, cloud, _ := newNSGTestTracker(t, service)
			for i := 1; i <= 100; i++ {
				addNSGTestBackend(dt, "svc-lb", "10.0.0.1", fmt.Sprintf("10.1.0.%d", i), "")
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			cloud.beforeWrite = func(ctx context.Context) {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			var calls atomic.Int32
			startNSGTestLocations(t, dt, func(_ context.Context, request armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
				if assert.Len(t, request.AddressLocations, 1) {
					assert.Len(t, request.AddressLocations[0].Addresses, 100)
					for _, address := range request.AddressLocations[0].Addresses {
						if assert.Len(t, address.Services, 1) {
							assert.Equal(t, "svc-lb", *address.Services[0])
						}
					}
				}
				cloud.mu.Lock()
				defer cloud.mu.Unlock()
				if restricted {
					assert.Equal(t, 1, cloud.nicWrites)
					assert.Len(t, cloud.groups, 1)
					for _, group := range cloud.groups {
						assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
							nsgTestAccess(t, group.Properties.SecurityRules, "20.1.2.3", "10.1.0.100", 8080, armnetwork.SecurityRuleProtocolTCP))
						assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
							nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.100", 8080, armnetwork.SecurityRuleProtocolTCP))
					}
				} else {
					assert.Zero(t, cloud.nicWrites)
					assert.Empty(t, cloud.groups)
				}
				calls.Add(1)
				return nil
			})
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			dt.triggerLocationsUpdater()
			if restricted {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("LocationsUpdater did not prepare the NSG")
				}
				assert.Zero(t, calls.Load(), "no backend publication while its NSG write is unfinished")
			}
			unblock()
			assert.Eventually(t, func() bool { return calls.Load() > 0 }, 5*time.Second, time.Millisecond)
		})
	}
}

func TestLocationsUpdaterRetriesFailedNSGPreparation(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	cloud.groupError = errors.New("NSG temporarily unavailable")
	var calls atomic.Int32
	startNSGTestLocations(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls.Add(1)
		return nil
	})
	dt.triggerLocationsUpdater()
	assert.Eventually(t, func() bool {
		cloud.mu.Lock()
		defer cloud.mu.Unlock()
		return cloud.groupWrites > 0
	}, 5*time.Second, time.Millisecond)
	assert.Zero(t, calls.Load())
	cloud.mu.Lock()
	cloud.groupError = nil
	cloud.mu.Unlock()
	assert.Eventually(t, func() bool { return calls.Load() > 0 }, 5*time.Second, time.Millisecond,
		"the existing location retry must retry NSG work without a separate updater")
}

func TestLocationsUpdaterKeepsNSGProtectionUntilWithdrawalAcknowledged(t *testing.T) {
	ctx := context.Background()
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.2", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
	dt.UpdateEndpoints("svc-lb", map[string]string{"10.1.0.2": "10.0.0.1"}, nil)
	for len(dt.locationsUpdaterTrigger) > 0 {
		<-dt.locationsUpdaterTrigger
	}
	calls := 0
	mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls++
		assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.2"].Services.Has("svc-lb"))
		assert.Equal(t, 2, cloud.groupWrites, "unacknowledged withdrawal must retain old rules")
		for _, group := range cloud.groups {
			assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
				nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.2", 8080, armnetwork.SecurityRuleProtocolTCP))
		}
		if calls == 1 {
			return errors.New("location update failed")
		}
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
	updater.process(ctx)
	assert.Equal(t, 2, cloud.groupWrites, "failed withdrawal must retain old rules")
	assert.NotEmpty(t, dt.locationsUpdaterTrigger, "failed withdrawal must schedule a retry")
	updater.process(ctx)
	assert.Equal(t, 2, calls)
	assert.Equal(t, 3, cloud.groupWrites, "the successful withdrawal pass must prune without a later preparation pass")
	assert.Empty(t, dt.pendingLocationUpdates)
	assert.Empty(t, dt.dirtyNICPolicies)
	for _, group := range cloud.groups {
		assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.2", 8080, armnetwork.SecurityRuleProtocolTCP))
		assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
			nsgTestAccess(t, group.Properties.SecurityRules, "40.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
	}
}

func TestLocationsUpdaterAppliesSourceOnlyChangesAndAllowAll(t *testing.T) {
	ctx := context.Background()
	service := nsgKubeService("svc-lb", "20.0.0.0/8")
	dt, cloud, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
	dt.dirtyNICPolicies.Clear()
	for len(dt.locationsUpdaterTrigger) > 0 {
		<-dt.locationsUpdaterTrigger
	}
	calls := 0
	mockNSGLocationUpdates(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls++
		return nil
	})
	updater := &LocationsUpdater{diffTracker: dt, ctx: ctx, logger: dt.logger}
	service.Annotations[consts.ServiceAnnotationAllowedIPRanges] = "40.0.0.0/8"
	assert.NoError(t, dt.ReconcileInboundService(service))
	assert.NotEmpty(t, dt.locationsUpdaterTrigger, "source-only changes must wake the existing worker")
	updater.process(ctx)
	assert.Equal(t, 3, cloud.groupWrites)
	assert.Empty(t, dt.dirtyNICPolicies, "a no-diff pass must finish its NIC work")
	for len(dt.locationsUpdaterTrigger) > 0 {
		<-dt.locationsUpdaterTrigger
	}
	delete(service.Annotations, consts.ServiceAnnotationAllowedIPRanges)
	assert.NoError(t, dt.ReconcileInboundService(service))
	assert.NotEmpty(t, dt.locationsUpdaterTrigger)
	updater.process(ctx)
	assert.Equal(t, 4, cloud.groupWrites)
	assert.Empty(t, dt.dirtyNICPolicies)
	for _, group := range cloud.groups {
		assert.Len(t, group.Properties.SecurityRules, 2)
		assert.Equal(t, armnetwork.SecurityRuleAccessAllow,
			nsgTestAccess(t, group.Properties.SecurityRules, "100.1.2.3", "10.1.0.1", 8080, armnetwork.SecurityRuleProtocolTCP))
	}
	assert.Zero(t, calls, "annotation-only changes need no address update")
}

func TestLocationsUpdaterStopWaitsForNSGWork(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cloud.beforeWrite = func(ctx context.Context) {
		close(entered)
		select {
		case <-ctx.Done():
			close(cancelled)
			<-release
		case <-release:
		}
	}
	var calls atomic.Int32
	updater := startNSGTestLocations(t, dt, func(context.Context, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
		calls.Add(1)
		return nil
	})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	dt.triggerLocationsUpdater()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("NSG work did not start")
	}
	stopped := make(chan struct{})
	go func() { updater.Stop(); close(stopped) }()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("NSG operation did not inherit LocationsUpdater cancellation")
	}
	select {
	case <-stopped:
		t.Error("LocationsUpdater stopped before its NSG work finished")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("LocationsUpdater did not stop")
	}
	assert.Zero(t, calls.Load())
}

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

	dt := seedDiffTracker(t, mockFactory, fake.NewSimpleClientset(), emptyK8sState(), emptyNRPState())

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	assert.NoError(t, dt.AddService(NewInboundServiceConfig("svc", makeInboundConfig(80))))

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

	dt := seedDiffTracker(t, mockFactory, fake.NewSimpleClientset(), emptyK8sState(), emptyNRPState())

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	assert.NoError(t, dt.AddService(NewInboundServiceConfig("svc", makeInboundConfig(80))))

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

	dt := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset())
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
func TestLocationsUpdaterAcknowledgesPodDrainBeforeRelease(t *testing.T) {
	const service = "egress-a-natgw"
	for _, family := range []struct {
		name  string
		nodes []string
		ips   []string
		other []string
		next  []string
	}{
		{name: "ipv4", nodes: []string{"10.0.0.1"}, ips: []string{"10.244.0.7"}, other: []string{"10.244.0.9"}, next: []string{"10.244.0.10"}},
		{name: "ipv6", nodes: []string{"fd00::1"}, ips: []string{"fd00::7"}, other: []string{"fd00::9"}, next: []string{"fd00::a"}},
		{name: "dual", nodes: []string{"10.0.0.1", "fd00::1"}, ips: []string{"10.244.0.7", "fd00::7"}, other: []string{"10.244.0.9", "fd00::9"}, next: []string{"10.244.0.10", "fd00::a"}},
	} {
		t.Run(family.name, func(t *testing.T) {
			for _, status := range []int{0, http.StatusBadRequest, http.StatusUnprocessableEntity} {
				t.Run(fmt.Sprintf("first_status=%d", status), func(t *testing.T) {
					ctx := context.Background()
					label := "egress-a"
					makePod := func(name, uid string, addresses []string) *v1.Pod {
						pod := unlabelledFinalizerPod(&label, uid, addresses...)
						pod.Name, pod.Status.HostIP = name, family.nodes[0]
						for _, node := range family.nodes {
							pod.Status.HostIPs = append(pod.Status.HostIPs, v1.HostIP{IP: node})
						}
						return pod
					}
					pod, other := makePod("p", "uid-p", family.ips), makePod("other", "uid-other", family.other)
					kube := fake.NewSimpleClientset(pod, other)
					dt := newTestDiffTracker()
					dt.kubeClient = kube
					dt.podInformerAddPod(other)
					op := dt.pendingServiceOps[service]
					if !assert.NotNil(t, op) {
						return
					}
					op.State = StateCreationInProgress
					dt.NRPResources.NATGateways.Insert(service)
					dt.OnServiceCreationComplete(service, true, nil)
					dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
					select {
					case <-dt.locationsUpdaterTrigger:
					default:
					}
					dt.podInformerAddPod(pod)
					ctrl := gomock.NewController(t)
					factory := mock_azclient.NewMockClientFactory(ctrl)
					sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
					factory.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
					addEntered, removeEntered := make(chan struct{}), make(chan struct{})
					releaseAdd, releaseRemove := make(chan struct{}), make(chan struct{})
					var addOnce, removeOnce sync.Once
					resumeAdd := func() { addOnce.Do(func() { close(releaseAdd) }) }
					resumeRemove := func() { removeOnce.Do(func() { close(releaseRemove) }) }
					var calls atomic.Int32
					sgw.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
						DoAndReturn(func(context.Context, string, string, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
							switch calls.Add(1) {
							case 1:
								close(addEntered)
								<-releaseAdd
								if status != 0 {
									return &azcore.ResponseError{StatusCode: status, ErrorCode: "InvalidRequest"}
								}
							case 2:
								close(removeEntered)
								<-releaseRemove
							}
							return nil
						}).AnyTimes()
					dt.networkClientFactory, dt.config = factory, testConfig()
					lu := NewLocationsUpdater(ctx, dt)
					stopped := make(chan struct{})
					go func() {
						defer close(stopped)
						lu.Run()
					}()
					defer func() {
						resumeAdd()
						resumeRemove()
						lu.Stop()
						<-stopped
					}()
					select {
					case <-addEntered:
					case <-time.After(3 * time.Second):
						t.Fatal("NRP add was not dispatched")
					}
					empty := pod.DeepCopy()
					empty.Labels[consts.PodLabelServiceEgressGateway] = ""
					_, err := kube.CoreV1().Pods("ns").Update(ctx, empty, metav1.UpdateOptions{})
					assert.NoError(t, err)
					dt.reconcileEgressPodUpdate(pod, empty)
					deleting := empty.DeepCopy()
					now := metav1.Now()
					deleting.DeletionTimestamp = &now
					_, err = kube.CoreV1().Pods("ns").Update(ctx, deleting, metav1.UpdateOptions{})
					assert.NoError(t, err)
					dt.reconcileEgressPodUpdate(empty, deleting)
					current, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
					assert.NoError(t, err)
					assert.True(t, hasPodFinalizer(current), "duplicate events cannot acknowledge an outstanding publication")
					if status != 0 {
						next := makePod("other", "uid-other", family.next)
						_, err := kube.CoreV1().Pods("ns").Update(ctx, next, metav1.UpdateOptions{})
						assert.NoError(t, err)
						dt.reconcileEgressPodUpdate(other, next)
					}
					resumeAdd()
					select {
					case <-removeEntered:
					case <-time.After(3 * time.Second):
						t.Fatal("cleanup did not continue after the first response")
					}
					current, err = kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
					assert.NoError(t, err)
					assert.True(t, hasPodFinalizer(current), "neither an acknowledged add nor a failed pass acknowledges the removal")
					dt.mu.Lock()
					pending := dt.pendingPodDeletions["ns/p"]
					if assert.NotNil(t, pending) {
						cleanup, found := pending.cleanupFor(service)
						assert.True(t, found)
						assert.True(t, cleanup.LocationSyncPending)
					}
					dt.mu.Unlock()
					resumeRemove()
					assert.Eventually(t, func() bool {
						current, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
						return err == nil && !hasPodFinalizer(current)
					}, 3*time.Second, time.Millisecond)
				})
			}
		})
	}
}

func TestLocationsUpdaterRetriesUnchangedPodDrainWithBackoff(t *testing.T) {
	const a, b = "egress-a-natgw", "egress-b-natgw"
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		for _, unknown := range []bool{false, true} {
			t.Run(fmt.Sprintf("status=%d/unknown=%t", status, unknown), func(t *testing.T) {
				label := "egress-b"
				pod := unlabelledFinalizerPod(&label, "uid-p", "10.244.0.7")
				dt := newTestDiffTracker()
				dt.kubeClient = fake.NewSimpleClientset(pod)
				dt.NRPResources.NATGateways.Insert(a, b)
				dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
					"10.244.0.7": {Services: utilsets.NewString(a)},
				}}
				dt.K8sResources.Nodes["10.0.0.1"] = Node{Pods: map[string]Pod{
					"10.244.0.7": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: b, OutboundPodKey: "ns/p", OutboundPodUID: "uid-p"},
				}}
				dt.outboundIdentityPodRefCount.Store(b, 1)
				dt.pendingServiceOps[a] = &ServiceOperationState{
					ServiceName: a, Config: NewOutboundServiceConfig(a, nil), State: StateDeletionPending,
				}
				dt.pendingServiceOps[b] = &ServiceOperationState{
					ServiceName: b, Config: NewOutboundServiceConfig(b, nil), State: StateCreated,
				}
				dt.pendingServiceDeletions[a] = &PendingServiceDeletion{ServiceName: a}
				dt.pendingPodDeletions["ns/p"] = &PendingPodDeletion{
					Namespace: "ns", Name: "p", UID: "uid-p", ServiceName: a,
					Addresses: []string{"10.244.0.7"}, IsLastPod: true, VerifyUnknownEgressDrain: unknown,
				}
				ctrl := gomock.NewController(t)
				factory := mock_azclient.NewMockClientFactory(ctrl)
				sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
				factory.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
				first, second := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				sgw.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
					DoAndReturn(func(context.Context, string, string, armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
						switch calls.Add(1) {
						case 1:
							close(first)
						case 2:
							close(second)
							cancel()
						}
						return &azcore.ResponseError{StatusCode: status, ErrorCode: "InvalidRequest"}
					}).AnyTimes()
				dt.networkClientFactory, dt.config = factory, testConfig()
				lu := NewLocationsUpdater(ctx, dt)
				lu.failureCount = 6
				stopped := make(chan struct{})
				go func() {
					defer close(stopped)
					lu.Run()
				}()
				defer func() {
					lu.Stop()
					<-stopped
				}()
				dt.triggerLocationsUpdater()
				select {
				case <-first:
				case <-time.After(3 * time.Second):
					t.Fatal("locations update was never attempted")
				}
				select {
				case <-second:
					t.Error("unchanged drain bypassed the scheduled backoff")
				case <-time.After(time.Second):
				}
				cancel()
				lu.Stop()
				<-stopped
				assert.Equal(t, int32(1), calls.Load())
				assert.Equal(t, 7, lu.failureCount, "cleanup must remain retryable, not take the terminal shortcut")
				assert.True(t, dt.pendingPodDeletions["ns/p"].waitsForService(a))
				assert.Empty(t, dt.podFinalizerGuards)
			})
		}
	}
}

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

	dt := seedDiffTracker(t, mockFactory, fake.NewSimpleClientset(), emptyK8sState(), emptyNRPState())

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	assert.NoError(t, dt.AddService(NewInboundServiceConfig("svc", makeInboundConfig(80))))

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

	dt := seedDiffTracker(t, mockFactory, fake.NewSimpleClientset(), emptyK8sState(), emptyNRPState())

	// "poison" contributes the location diff that NRP rejects with a deterministic 400.
	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("poison")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("poison")
	assert.NoError(t, dt.AddService(NewInboundServiceConfig("poison", makeInboundConfig(80))))

	// "innocent" is unrelated: it holds no NRP addresses at all, so its drain is already complete
	// and CheckPendingServiceDeletions must promote it to StateDeletionInProgress.
	dt.pendingServiceOps["innocent"] = &ServiceOperationState{ServiceName: "innocent", State: StateDeletionPending}
	dt.pendingServiceDeletions["innocent"] = &PendingServiceDeletion{ServiceName: "innocent", IsInbound: true}

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

// TestLocationsUpdater_InitKeepsRetryingOnSustainedTransientError pins that a never-clearing
// transient NRP error is never abandoned during initialization. Abandoning retired the in-flight
// trigger, which let WaitForInitialSync return success and startup proceed against NRP state the
// sync had never reconciled - and discarded an NRP recovery arriving moments later.
//
// The contract is now: keep retrying and keep initialization blocked until NRP recovers.
func TestLocationsUpdater_InitKeepsRetryingOnSustainedTransientError(t *testing.T) {
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

	dt := seedDiffTracker(t, mockFactory, fake.NewSimpleClientset(), emptyK8sState(), emptyNRPState())

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	assert.NoError(t, dt.AddService(NewInboundServiceConfig("svc", makeInboundConfig(80))))

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

	dt := seedDiffTracker(t, mockFactory, fake.NewSimpleClientset(), emptyK8sState(), emptyNRPState())

	// The pod is gone from Kubernetes but NRP still holds its address, so this batch is the removal
	// the pending deletion waits on.
	dt.K8sResources.Nodes["node-1"] = newNode()
	dt.NRPResources.Locations["node-1"] = NRPLocation{
		Addresses: map[string]NRPAddress{"10.0.0.1": {Services: utilsets.NewString("svc")}},
	}
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceName: "svc", State: StateDeletionPending}
	dt.pendingServiceDeletions["svc"] = &PendingServiceDeletion{ServiceName: "svc", IsInbound: true}

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

	dt := seedDiffTracker(t, mockFactory, fake.NewSimpleClientset(), emptyK8sState(), emptyNRPState())

	dt.K8sResources.Nodes["node-1"] = newNode()
	dt.NRPResources.Locations["node-1"] = NRPLocation{
		Addresses: map[string]NRPAddress{"10.0.0.1": {Services: utilsets.NewString("svc")}},
	}
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceName: "svc", State: StateDeletionPending}
	dt.pendingServiceDeletions["svc"] = &PendingServiceDeletion{ServiceName: "svc", IsInbound: true}

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
		nodeIP      = "10.0.0.1"
		podIP       = "10.244.0.7"
		serviceName = "svc"
		wantRG      = "rg"
		wantSGW     = "sgw"
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

	dt := seedDiffTracker(t, mockFactory, fake.NewSimpleClientset(), emptyK8sState(), emptyNRPState())

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString(serviceName)
	node := newNode()
	node.Pods[podIP] = pod
	dt.K8sResources.Nodes[nodeIP] = node
	dt.NRPResources.LoadBalancers.Insert(serviceName)
	assert.NoError(t, dt.AddService(NewInboundServiceConfig(serviceName, makeInboundConfig(80))))

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
	assert.Equal(t, []string{serviceName}, names,
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

	dt := seedDiffTracker(t, mockFactory, fake.NewSimpleClientset(), emptyK8sState(), emptyNRPState())

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	assert.NoError(t, dt.AddService(NewInboundServiceConfig("svc", makeInboundConfig(80))))

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
