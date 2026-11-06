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
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// deletionTestFactory returns a ClientFactory whose LoadBalancer/PublicIP/ServiceGateway
// delete operations all succeed, so the only failure under test is the K8s finalizer removal.
// The Public IP reads as already gone, so no address is withdrawn from the Service status.
func deletionTestFactory(ctrl *gomock.Controller) *mock_azclient.MockClientFactory {
	f, pip := publicIPTestFactory(ctrl)
	pip.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	return f
}

// publicIPTestFactory returns a ClientFactory whose LoadBalancer and ServiceGateway operations
// succeed, together with its Public IP client so the test can script that step.
func publicIPTestFactory(ctrl *gomock.Controller) (*mock_azclient.MockClientFactory, *mock_publicipaddressclient.MockInterface) {
	f := mock_azclient.NewMockClientFactory(ctrl)
	sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
	lb := mock_loadbalancerclient.NewMockInterface(ctrl)
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	f.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(lb).AnyTimes()
	f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
	sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	lb.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	lb.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	return f, pip
}

// publicIPWithAddress is a Public IP as Azure returns it once its address is allocated.
func publicIPWithAddress(address string) *armnetwork.PublicIPAddress {
	return &armnetwork.PublicIPAddress{
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{IPAddress: ptr.To(address)},
	}
}

// statusWrites counts the writes to the Service status subresource the clientset received.
func statusWrites(kube *fake.Clientset) int {
	writes := 0
	for _, action := range kube.Actions() {
		verb := action.GetVerb()
		if (verb == "update" || verb == "patch") && action.GetResource().Resource == "services" && action.GetSubresource() == "status" {
			writes++
		}
	}
	return writes
}

func serviceLists(kube *fake.Clientset) int {
	lists := 0
	for _, action := range kube.Actions() {
		if action.Matches("list", "services") {
			lists++
		}
	}
	return lists
}

func deletionTestService() *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "svc", Namespace: "default", UID: "uid-1",
			Finalizers: []string{ServiceGatewayServiceCleanupFinalizer},
		},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
	}
}

func deletionTestDiffTracker(kube *fake.Clientset, f *mock_azclient.MockClientFactory) *DiffTracker {
	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.kubeClient = kube
	dt.networkClientFactory = f
	dt.NRPResources.LoadBalancers = utilsets.NewString("uid-1")
	dt.pendingServiceOps["uid-1"] = &ServiceOperationState{
		ServiceUID: "uid-1", Config: NewInboundServiceConfig("uid-1", nil), State: StateDeletionInProgress,
	}
	dt.pendingServiceDeletions["uid-1"] = &PendingServiceDeletion{ServiceUID: "uid-1", IsInbound: true}
	return dt
}

func deletionTestUpdater(dt *DiffTracker, onComplete func(string, bool, error)) *ServiceUpdater {
	return &ServiceUpdater{
		diffTracker: dt,
		onComplete:  onComplete,
		trigger:     dt.serviceUpdaterTrigger,
		ctx:         context.Background(),
		semaphore:   make(chan struct{}, 10),
		activeOps:   make(map[string]bool),
	}
}

// TestServiceUpdaterDeleteInboundService_FinalizerFailureRetries verifies that when the
// Azure cleanup succeeds but removing the ServiceGateway finalizer from the K8s Service
// fails, the deletion is reported as a failure and the service stays tracked. Reporting
// success here would clear tracking and the NRP entry, after which a retried DeleteService
// is a no-op and the finalizer is stranded (Service stuck Terminating).
func TestServiceUpdaterDeleteInboundService_FinalizerFailureRetries(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	kube := fake.NewSimpleClientset(deletionTestService())
	failFinalizer := func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("transient apiserver error")
	}
	kube.PrependReactor("update", "services", failFinalizer)
	kube.PrependReactor("patch", "services", failFinalizer)

	dt := deletionTestDiffTracker(kube, deletionTestFactory(ctrl))
	var reportedSuccess *bool
	su := deletionTestUpdater(dt, func(uid string, ok bool, err error) {
		v := ok
		reportedSuccess = &v
		dt.OnServiceCreationComplete(uid, ok, err)
	})

	su.deleteInboundService("uid-1", "corr-1")

	if assert.NotNil(t, reportedSuccess, "onComplete should be called") {
		assert.False(t, *reportedSuccess, "deletion must not report success when finalizer removal fails")
	}
	_, stillTracked := dt.pendingServiceOps["uid-1"]
	assert.True(t, stillTracked, "service must remain tracked for retry when finalizer removal fails")
}

// TestServiceUpdaterDeleteInboundService_Succeeds verifies the happy path: Azure cleanup
// and finalizer removal both succeed, deletion is reported successful, and tracking is cleared.
func TestServiceUpdaterDeleteInboundService_Succeeds(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	kube := fake.NewSimpleClientset(deletionTestService())
	dt := deletionTestDiffTracker(kube, deletionTestFactory(ctrl))
	var reportedSuccess *bool
	su := deletionTestUpdater(dt, func(uid string, ok bool, err error) {
		v := ok
		reportedSuccess = &v
		dt.OnServiceCreationComplete(uid, ok, err)
	})

	su.deleteInboundService("uid-1", "corr-1")

	if assert.NotNil(t, reportedSuccess) {
		assert.True(t, *reportedSuccess, "deletion should succeed when finalizer removal works")
	}
	_, stillTracked := dt.pendingServiceOps["uid-1"]
	assert.False(t, stillTracked, "service tracking should be cleared after successful deletion")
}

// TestServiceUpdaterDeleteInboundService_WithdrawsDeletedPublicIPAddress pins that deleting an
// inbound Service's Public IP first withdraws that address from the Service status, and nothing else.
// A create that finished after the Service was marked for deletion publishes the address, and once
// the Public IP is gone nothing else removes it: the Service would advertise an address that serves
// nothing and that Azure can hand to another Service.
func TestServiceUpdaterDeleteInboundService_WithdrawsDeletedPublicIPAddress(t *testing.T) {
	const deletedIP = "20.1.2.3"
	kept := []v1.LoadBalancerIngress{{IP: "2001:db8::1"}, {Hostname: "example.com"}}
	service := func(uid types.UID, deleting bool, ingress ...v1.LoadBalancerIngress) *v1.Service {
		svc := deletionTestService()
		svc.UID = uid
		svc.Status.LoadBalancer.Ingress = ingress
		if deleting {
			svc.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		}
		return svc
	}

	for _, tc := range []struct {
		name        string
		svc         *v1.Service // nil when the Service is gone
		orphan      bool        // only the UID is known, as for a startup orphan deletion
		pipAddress  string      // empty when the Public IP is already gone
		wantIngress []v1.LoadBalancerIngress
		wantWrites  int
	}{
		{
			name:        "withdraws the deleted address and keeps every other entry",
			svc:         service("uid-1", false, append([]v1.LoadBalancerIngress{{IP: deletedIP}}, kept...)...),
			pipAddress:  deletedIP,
			wantIngress: kept,
			wantWrites:  1,
		},
		{
			name:        "leaves an unrelated address alone",
			svc:         service("uid-1", false, v1.LoadBalancerIngress{IP: "20.9.9.9"}),
			pipAddress:  deletedIP,
			wantIngress: []v1.LoadBalancerIngress{{IP: "20.9.9.9"}},
		},
		{
			name:        "writes nothing when the Public IP is already gone",
			svc:         service("uid-1", false, kept...),
			wantIngress: kept,
		},
		{
			name:        "skips a same-name Service that replaced the deleted one",
			svc:         service("uid-2", false, v1.LoadBalancerIngress{IP: deletedIP}),
			pipAddress:  deletedIP,
			wantIngress: []v1.LoadBalancerIngress{{IP: deletedIP}},
		},
		{
			name:        "skips a Service that is being deleted",
			svc:         service("uid-1", true, v1.LoadBalancerIngress{IP: deletedIP}),
			pipAddress:  deletedIP,
			wantIngress: []v1.LoadBalancerIngress{{IP: deletedIP}},
		},
		{name: "skips a Service that is gone", pipAddress: deletedIP},
		{
			name:        "withdraws the address from a Service known only by its UID",
			svc:         service("uid-1", false, append([]v1.LoadBalancerIngress{{IP: deletedIP}}, kept...)...),
			orphan:      true,
			pipAddress:  deletedIP,
			wantIngress: kept,
			wantWrites:  1,
		},
		{
			name:        "leaves another Service alone when an orphan's Service is gone",
			svc:         service("uid-2", false, v1.LoadBalancerIngress{IP: deletedIP}),
			orphan:      true,
			pipAddress:  deletedIP,
			wantIngress: []v1.LoadBalancerIngress{{IP: deletedIP}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			var objects []runtime.Object
			if tc.svc != nil {
				objects = append(objects, tc.svc)
			}
			kube := fake.NewSimpleClientset(objects...)
			f, pip := publicIPTestFactory(ctrl)
			if tc.pipAddress == "" {
				pip.EXPECT().Get(gomock.Any(), "rg", "uid-1-pip", nil).Return(nil, notFoundError())
			} else {
				pip.EXPECT().Get(gomock.Any(), "rg", "uid-1-pip", nil).Return(publicIPWithAddress(tc.pipAddress), nil)
			}
			pip.EXPECT().Delete(gomock.Any(), "rg", "uid-1-pip").DoAndReturn(func(context.Context, string, string) error {
				assert.Equal(t, tc.wantWrites, statusWrites(kube), "the address must be withdrawn before its Public IP is deleted")
				return nil
			})

			dt := deletionTestDiffTracker(kube, f)
			if !tc.orphan {
				dt.pendingServiceOps["uid-1"].Config.Namespace = "default"
				dt.pendingServiceOps["uid-1"].Config.Name = "svc"
			}
			var reportedSuccess *bool
			su := deletionTestUpdater(dt, func(uid string, ok bool, err error) {
				reportedSuccess = &ok
				dt.OnServiceCreationComplete(uid, ok, err)
			})

			su.deleteInboundService("uid-1", "corr-1")

			if assert.NotNil(t, reportedSuccess, "onComplete must be called") {
				assert.True(t, *reportedSuccess, "the deletion must succeed")
			}
			assert.Equal(t, tc.wantWrites, statusWrites(kube), "unexpected number of Service status writes")
			if !tc.orphan {
				assert.Zero(t, serviceLists(kube), "a Service whose name is known must be read by name, not by listing every Service")
			}
			if tc.svc != nil {
				got, err := kube.CoreV1().Services("default").Get(context.Background(), "svc", metav1.GetOptions{})
				if assert.NoError(t, err) {
					assert.Equal(t, tc.wantIngress, got.Status.LoadBalancer.Ingress)
				}
			}
		})
	}
}

// TestServiceUpdaterDeleteInboundService_PublicIPAddressFailures pins the failure handling around
// that withdrawal. Only the Public IP records the address, so the deletion keeps the Public IP until
// the address is withdrawn: a failure before then fails the attempt, and the retry reads the address
// again. Each attempt is one run of the deletion, as the engine retries it.
func TestServiceUpdaterDeleteInboundService_PublicIPAddressFailures(t *testing.T) {
	const deletedIP = "20.1.2.3"
	advertised := []v1.LoadBalancerIngress{{IP: deletedIP}}

	type attempt struct {
		getErr      error // reading the Public IP fails
		pipGone     bool  // the Public IP reads as already deleted
		lookupFails bool  // reading the Service fails
		writeFails  bool  // writing the Service status fails
		deleted     bool  // the Public IP delete is called
		deleteErr   error
		wantSuccess bool
		wantIngress []v1.LoadBalancerIngress
	}
	retried := attempt{deleted: true, wantSuccess: true}

	for _, tc := range []struct {
		name     string
		attempts []attempt
	}{
		{
			name:     "a failed Public IP read keeps the Public IP for the retry",
			attempts: []attempt{{getErr: errors.New("throttled"), wantIngress: advertised}, retried},
		},
		{
			name:     "a failed Service lookup keeps the Public IP for the retry",
			attempts: []attempt{{lookupFails: true, wantIngress: advertised}, retried},
		},
		{
			name: "a Service lookup that keeps failing keeps the Public IP",
			attempts: []attempt{
				{lookupFails: true, wantIngress: advertised},
				{lookupFails: true, wantIngress: advertised},
			},
		},
		{
			name:     "a failed status write keeps the Public IP for the retry",
			attempts: []attempt{{writeFails: true, wantIngress: advertised}, retried},
		},
		{
			// Azure finished the delete after the poll gave up, so the retry finds the Public IP gone
			// and can no longer read the address.
			name: "a Public IP delete reported as failed after Azure completed it",
			attempts: []attempt{
				{deleted: true, deleteErr: context.DeadlineExceeded},
				{pipGone: true, deleted: true, deleteErr: notFoundError(), wantSuccess: true},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			svc := deletionTestService()
			svc.Status.LoadBalancer.Ingress = advertised
			kube := fake.NewSimpleClientset(svc)
			var current attempt
			failLookup := func(k8stesting.Action) (bool, runtime.Object, error) {
				if !current.lookupFails {
					return false, nil, nil
				}
				return true, nil, errors.New("apiserver unavailable")
			}
			kube.PrependReactor("get", "services", failLookup)
			kube.PrependReactor("list", "services", failLookup)
			kube.PrependReactor("*", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if !current.writeFails || action.GetSubresource() != "status" {
					return false, nil, nil
				}
				return true, nil, errors.New("apiserver unavailable")
			})

			f, pip := publicIPTestFactory(ctrl)
			dt := deletionTestDiffTracker(kube, f)
			dt.pendingServiceOps["uid-1"].Config.Namespace = "default"
			dt.pendingServiceOps["uid-1"].Config.Name = "svc"
			var reportedSuccess *bool
			su := deletionTestUpdater(dt, func(uid string, ok bool, err error) {
				reportedSuccess = &ok
				dt.OnServiceCreationComplete(uid, ok, err)
			})

			for i, a := range tc.attempts {
				switch {
				case a.getErr != nil:
					pip.EXPECT().Get(gomock.Any(), "rg", "uid-1-pip", nil).Return(nil, a.getErr)
				case a.pipGone:
					pip.EXPECT().Get(gomock.Any(), "rg", "uid-1-pip", nil).Return(nil, notFoundError())
				default:
					pip.EXPECT().Get(gomock.Any(), "rg", "uid-1-pip", nil).Return(publicIPWithAddress(deletedIP), nil)
				}
				if a.deleted {
					pip.EXPECT().Delete(gomock.Any(), "rg", "uid-1-pip").Return(a.deleteErr)
				}
				current, reportedSuccess = a, nil

				su.deleteInboundService("uid-1", "corr-1")

				current = attempt{}
				if assert.NotNil(t, reportedSuccess, "attempt %d: onComplete must be called", i+1) {
					assert.Equal(t, a.wantSuccess, *reportedSuccess, "attempt %d: deletion outcome", i+1)
				}
				got, err := kube.CoreV1().Services("default").Get(context.Background(), "svc", metav1.GetOptions{})
				if assert.NoError(t, err) {
					assert.ElementsMatch(t, a.wantIngress, got.Status.LoadBalancer.Ingress, "attempt %d: advertised addresses", i+1)
					assert.Equal(t, !a.wantSuccess, slices.Contains(got.Finalizers, ServiceGatewayServiceCleanupFinalizer),
						"attempt %d: only a completed deletion releases the Service", i+1)
				}
			}
		})
	}
}

// TestDeleteDuringCreate_WithdrawsAddressPublishedByTheCreate reproduces the stale-ingress
// interleaving through the engine. The Service leaves LoadBalancer while its create is in flight,
// so the create is marked for deletion; it returns to LoadBalancer before the create publishes its
// Public IP address, and the create's completion is then routed to the deletion, which deletes that
// Public IP. A Service whose config is now rejected records no recreate, so unless the deletion
// withdraws the address the Service advertises it forever. When the Service is accepted again the
// recreate publishes a fresh address exactly as before.
func TestDeleteDuringCreate_WithdrawsAddressPublishedByTheCreate(t *testing.T) {
	const (
		uid         = "uid-1"
		firstIP     = "20.1.2.3"
		recreatedIP = "20.4.5.6"
	)

	for _, tc := range []struct {
		name        string
		recreate    bool
		wantIngress []v1.LoadBalancerIngress
	}{
		{name: "rejected config, no recreate"},
		{name: "recreate follows", recreate: true, wantIngress: []v1.LoadBalancerIngress{{IP: recreatedIP}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			svc := deletionTestService()
			svc.Finalizers = nil
			kube := fake.NewSimpleClientset(svc)
			ingress := func() []v1.LoadBalancerIngress {
				got, err := kube.CoreV1().Services("default").Get(context.Background(), "svc", metav1.GetOptions{})
				if err != nil {
					return nil
				}
				return got.Status.LoadBalancer.Ingress
			}

			f, pip := publicIPTestFactory(ctrl)
			createStarted := make(chan struct{})
			releaseCreate := make(chan struct{})
			var creates atomic.Int32
			pip.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "uid-1-pip", gomock.Any()).DoAndReturn(
				func(ctx context.Context, _, _ string, _ armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
					if creates.Add(1) > 1 {
						return publicIPWithAddress(recreatedIP), nil
					}
					close(createStarted)
					select {
					case <-releaseCreate:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return publicIPWithAddress(firstIP), nil
				}).AnyTimes()
			var mu sync.Mutex
			var advertisedAtRead, advertisedAtDelete []v1.LoadBalancerIngress
			pip.EXPECT().Get(gomock.Any(), "rg", "uid-1-pip", nil).DoAndReturn(
				func(context.Context, string, string, *string) (*armnetwork.PublicIPAddress, error) {
					mu.Lock()
					defer mu.Unlock()
					advertisedAtRead = ingress()
					return publicIPWithAddress(firstIP), nil
				})
			pip.EXPECT().Delete(gomock.Any(), "rg", "uid-1-pip").DoAndReturn(func(context.Context, string, string) error {
				mu.Lock()
				defer mu.Unlock()
				advertisedAtDelete = ingress()
				return nil
			})

			dt := newTestDiffTracker()
			dt.config = testConfig()
			dt.kubeClient = kube
			dt.networkClientFactory = f
			su := NewServiceUpdater(context.Background(), dt, dt.OnServiceCreationComplete, dt.serviceUpdaterTrigger)
			go su.Run()
			defer su.Stop()

			config := NewInboundServiceConfig(uid, makeInboundConfig(80))
			config.Namespace = "default"
			config.Name = "svc"
			dt.AddService(config)
			select {
			case <-createStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("the create was never dispatched")
			}

			dt.DeleteService(uid, true, false)
			if tc.recreate {
				dt.UpdateService(config)
			}
			close(releaseCreate)

			assert.Eventually(t, func() bool {
				dt.mu.Lock()
				op, tracked := dt.pendingServiceOps[uid]
				settled := !tracked
				if tc.recreate {
					settled = tracked && op.State == StateCreated
				}
				dt.mu.Unlock()
				return settled && apiequality.Semantic.DeepEqual(tc.wantIngress, ingress())
			}, 5*time.Second, 10*time.Millisecond, "the Service must not keep advertising the deleted Public IP address")

			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, []v1.LoadBalancerIngress{{IP: firstIP}}, advertisedAtRead,
				"the create must have published its address before the deletion read the Public IP")
			assert.Empty(t, advertisedAtDelete, "the deletion must withdraw the address before it deletes the Public IP")
		})
	}
}
