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
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/component-base/metrics/testutil"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

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
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceName: "svc", State: StateCreated}

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
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceName: "svc", State: StateCreated}

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

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceName: "svc", State: StateCreated}

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
	dt.pendingServiceOps["poison"] = &ServiceOperationState{ServiceName: "poison", State: StateCreated}

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
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceName: "svc", State: StateCreated}

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

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

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

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString(serviceName)
	node := newNode()
	node.Pods[podIP] = pod
	dt.K8sResources.Nodes[nodeIP] = node
	dt.NRPResources.LoadBalancers.Insert(serviceName)
	dt.pendingServiceOps[serviceName] = &ServiceOperationState{ServiceName: serviceName, State: StateCreated}

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

	dt := newTestDiffTracker()
	dt.networkClientFactory = mockFactory
	dt.config = testConfig()

	pod := newPod()
	pod.InboundIdentities = utilsets.NewString("svc")
	node := newNode()
	node.Pods["10.0.0.1"] = pod
	dt.K8sResources.Nodes["node-1"] = node
	dt.NRPResources.LoadBalancers.Insert("svc")
	dt.pendingServiceOps["svc"] = &ServiceOperationState{ServiceName: "svc", State: StateCreated}

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
