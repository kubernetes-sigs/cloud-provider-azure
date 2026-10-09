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
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// deletionTestFactory returns a ClientFactory whose LoadBalancer/PublicIP/ServiceGateway
// delete operations all succeed, so the only failure under test is the K8s finalizer removal.
func deletionTestFactory(ctrl *gomock.Controller) *mock_azclient.MockClientFactory {
	f := mock_azclient.NewMockClientFactory(ctrl)
	sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
	lb := mock_loadbalancerclient.NewMockInterface(ctrl)
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	f.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(lb).AnyTimes()
	f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
	sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	lb.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	lb.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	return f
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

func TestServiceUpdaterDeleteInboundService_ReleasesOwnedPublicIPs(t *testing.T) {
	owned := map[string]*string{consts.ServiceTagKey: ptr.To("default/svc"), consts.ClusterNameKey: ptr.To("cluster")}
	named := map[string]string{
		consts.ServiceAnnotationPIPNameDualStack[false]:   "mine",
		consts.ServiceAnnotationLoadBalancerResourceGroup: "other-rg",
	}
	mineID := "/subscriptions/sub/resourceGroups/other-rg/providers/Microsoft.Network/publicIPAddresses/mine"
	oldID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/old"
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		serviceGone bool
		frontend    string
		pips        map[string]map[string]*string
		readErr     error
		deleteErr   error
		noCluster   bool
		attached    []string
		listErr     error
		wantDeleted []string
		wantDefer   []string
		wantEvent   bool
		wantRetry   bool
	}{
		{name: "a Public IP already gone", annotations: named, frontend: mineID},
		{name: "a Public IP deleted meanwhile", annotations: named, frontend: mineID, pips: map[string]map[string]*string{"other-rg/mine": owned},
			deleteErr: notFoundError()},
		{name: "ownership waits for the cluster name", annotations: named, frontend: mineID, noCluster: true,
			pips: map[string]map[string]*string{"other-rg/mine": owned}, wantDefer: []string{"other-rg/mine"}},
		{name: "the Service is gone and ownership in another resource group waits for the cluster name", serviceGone: true, frontend: mineID, noCluster: true,
			pips: map[string]map[string]*string{"other-rg/mine": owned}, wantDefer: []string{"other-rg/mine"}},
		{name: "the Service is gone and the cluster resource group only holds this cluster's Public IPs", serviceGone: true, frontend: oldID, noCluster: true,
			pips: map[string]map[string]*string{"rg/old": owned}, wantDeleted: []string{"rg/old"}},
		{name: "the cluster name is not needed for a Public IP without a cluster tag", annotations: named, frontend: mineID, noCluster: true,
			pips: map[string]map[string]*string{"other-rg/mine": {consts.ServiceTagKey: ptr.To("default/svc")}}, wantDeleted: []string{"other-rg/mine"}},
		{name: "the Service is gone, the cluster name is unknown and the tags have no cluster", serviceGone: true, frontend: oldID, noCluster: true,
			pips: map[string]map[string]*string{"rg/old": {consts.ServiceTagKey: ptr.To("default/svc")}}},
		{name: "a named Public IP the controller created", annotations: named, frontend: mineID,
			pips: map[string]map[string]*string{"other-rg/mine": owned}, wantDeleted: []string{"other-rg/mine"}},
		{name: "found by name when a retried delete no longer has the load balancer", annotations: named,
			pips: map[string]map[string]*string{"other-rg/mine": owned}, wantDeleted: []string{"other-rg/mine"}},
		{name: "a user's named Public IP", annotations: named, frontend: mineID,
			pips: map[string]map[string]*string{"other-rg/mine": nil}},
		{name: "a Public IP created for another Service", annotations: named, frontend: mineID,
			pips: map[string]map[string]*string{"other-rg/mine": {consts.ServiceTagKey: ptr.To("default/other"), consts.ClusterNameKey: ptr.To("cluster")}}},
		{name: "a Public IP in the cluster resource group created by another cluster", annotations: map[string]string{consts.ServiceAnnotationPIPNameDualStack[false]: "old"},
			frontend: oldID, pips: map[string]map[string]*string{"rg/old": {consts.ServiceTagKey: ptr.To("default/svc"), consts.ClusterNameKey: ptr.To("other")}}},
		{name: "a Public IP created by another cluster", annotations: named, frontend: mineID,
			pips: map[string]map[string]*string{"other-rg/mine": {consts.ServiceTagKey: ptr.To("default/svc"), consts.ClusterNameKey: ptr.To("other")}}},
		{name: "an owned Public IP now chosen by address", annotations: map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "20.0.0.7"},
			frontend: oldID, pips: map[string]map[string]*string{"rg/old": owned}, wantDeleted: []string{"rg/old"}},
		{name: "an owned Public IP in use while the Service chooses another", annotations: map[string]string{consts.ServiceAnnotationPIPNameDualStack[false]: "user"},
			frontend: oldID, pips: map[string]map[string]*string{"rg/old": owned, "rg/user": nil}, wantDeleted: []string{"rg/old"}},
		{name: "the Service is gone and the tags name this cluster", serviceGone: true, frontend: oldID,
			pips: map[string]map[string]*string{"rg/old": owned}, wantDeleted: []string{"rg/old"}},
		{name: "the Service is gone and the tags have no cluster", serviceGone: true, frontend: oldID,
			pips: map[string]map[string]*string{"rg/old": {consts.ServiceTagKey: ptr.To("default/svc")}}},
		{name: "the Service is gone and the tags have only the cluster", serviceGone: true, frontend: oldID,
			pips: map[string]map[string]*string{"rg/old": {consts.ClusterNameKey: ptr.To("cluster")}}},
		{name: "Public IPs a move left in the Service's resource group", annotations: named, frontend: mineID, attached: []string{"other-rg/attached"},
			pips: map[string]map[string]*string{"other-rg/mine": owned, "other-rg/left": owned, "other-rg/attached": owned, "other-rg/user": nil,
				"other-rg/foreign":    {consts.ServiceTagKey: ptr.To("default/svc"), consts.ClusterNameKey: ptr.To("other")},
				"other-rg/no-cluster": {consts.ServiceTagKey: ptr.To("default/svc")}},
			wantDeleted: []string{"other-rg/mine", "other-rg/left"}},
		{name: "Public IPs a move left in the Service's resource group wait for the cluster name", annotations: named, frontend: mineID, noCluster: true,
			pips: map[string]map[string]*string{"other-rg/mine": owned, "other-rg/left": owned}, wantDefer: []string{"other-rg/mine", "other-rg/left"}},
		{name: "a failed listing of the Service's resource group is retried", annotations: named, frontend: mineID, listErr: &azcore.ResponseError{StatusCode: http.StatusServiceUnavailable},
			pips: map[string]map[string]*string{"other-rg/mine": owned}, wantDeleted: []string{"other-rg/mine"}, wantRetry: true},
		{name: "a lasting failure to list the Service's resource group does not hold the deletion", annotations: named, frontend: mineID, listErr: &azcore.ResponseError{StatusCode: http.StatusForbidden},
			pips: map[string]map[string]*string{"other-rg/mine": owned}, wantDeleted: []string{"other-rg/mine"}},
		{name: "unreadable", annotations: named, frontend: mineID, readErr: &azcore.ResponseError{StatusCode: http.StatusForbidden}, wantEvent: true},
		{name: "not deletable", annotations: named, frontend: mineID, pips: map[string]map[string]*string{"other-rg/mine": owned},
			deleteErr: &azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "PublicIPAddressCannotBeDeleted"}, wantEvent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			f := mock_azclient.NewMockClientFactory(ctrl)
			sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
			lb := mock_loadbalancerclient.NewMockInterface(ctrl)
			pip := mock_publicipaddressclient.NewMockInterface(ctrl)
			f.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
			f.EXPECT().GetLoadBalancerClient().Return(lb).AnyTimes()
			f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
			sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			lb.EXPECT().Get(gomock.Any(), "rg", "uid-1", gomock.Any()).DoAndReturn(func(context.Context, string, string, *string) (*armnetwork.LoadBalancer, error) {
				if tc.frontend == "" {
					return nil, notFoundError()
				}
				return &armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{
					FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To(tc.frontend)},
					}}},
				}}, nil
			})
			lb.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
			pip.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, rg, name string, _ *string) (*armnetwork.PublicIPAddress, error) {
				if tc.readErr != nil {
					return nil, tc.readErr
				}
				tags, ok := tc.pips[rg+"/"+name]
				if !ok {
					return nil, notFoundError()
				}
				return &armnetwork.PublicIPAddress{Name: ptr.To(name), Tags: tags}, nil
			}).AnyTimes()
			pip.EXPECT().List(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, rg string) ([]*armnetwork.PublicIPAddress, error) {
				if tc.listErr != nil {
					return nil, tc.listErr
				}
				var pips []*armnetwork.PublicIPAddress
				for key, tags := range tc.pips {
					if name, ok := strings.CutPrefix(key, rg+"/"); ok {
						listed := &armnetwork.PublicIPAddress{Name: ptr.To(name), Tags: tags, Properties: &armnetwork.PublicIPAddressPropertiesFormat{}}
						if slices.Contains(tc.attached, key) {
							listed.Properties.IPConfiguration = &armnetwork.IPConfiguration{ID: ptr.To("ipconfig")}
						}
						pips = append(pips, listed)
					}
				}
				return pips, nil
			}).AnyTimes()
			var deletedMu sync.Mutex
			var deleted []string
			deletedNow := func() []string {
				deletedMu.Lock()
				defer deletedMu.Unlock()
				return slices.Clone(deleted)
			}
			pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, rg, name string) error {
				if name == PublicIPName("uid-1") {
					return nil
				}
				if tc.deleteErr != nil {
					return tc.deleteErr
				}
				deletedMu.Lock()
				deleted = append(deleted, rg+"/"+name)
				deletedMu.Unlock()
				return nil
			}).AnyTimes()

			svc := deletionTestService()
			svc.Annotations = tc.annotations
			svc.Spec.Ports = []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}
			kube := fake.NewSimpleClientset(svc)
			if tc.serviceGone {
				kube = fake.NewSimpleClientset()
			}
			dt := deletionTestDiffTracker(kube, f)
			if !tc.noCluster {
				dt.SetClusterName("cluster")
			}
			recorder := record.NewFakeRecorder(10)
			dt.SetEventRecorder(recorder)
			var success bool
			su := deletionTestUpdater(dt, func(_ string, ok bool, _ error) { success = ok })
			su.deleteInboundService("uid-1", "corr")
			assert.Equal(t, !tc.wantRetry, success, "a Public IP that cannot be released for a lasting reason does not hold the Service's deletion")
			assert.ElementsMatch(t, tc.wantDeleted, deletedNow())
			if tc.noCluster {
				dt.serviceUpdater = su
				dt.SetClusterName("cluster")
				want := append(slices.Clone(tc.wantDeleted), tc.wantDefer...)
				assert.Eventually(t, func() bool { return len(deletedNow()) == len(want) }, 5*time.Second, 10*time.Millisecond)
				assert.ElementsMatch(t, want, deletedNow(), "a deferred release runs once the cluster name is known")
			}
			close(recorder.Events)
			var events []string
			for event := range recorder.Events {
				events = append(events, event)
			}
			assert.Equal(t, tc.wantEvent, slices.ContainsFunc(events, func(e string) bool { return strings.Contains(e, "PublicIPCleanupFailed") }), "%v", events)
		})
	}

	t.Run("Public IPs are not released while the load balancer still uses them", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		f := mock_azclient.NewMockClientFactory(ctrl)
		sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
		lb := mock_loadbalancerclient.NewMockInterface(ctrl)
		pip := mock_publicipaddressclient.NewMockInterface(ctrl)
		f.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
		f.EXPECT().GetLoadBalancerClient().Return(lb).AnyTimes()
		f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
		sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		lb.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To(mineID)},
			}}},
		}}, nil)
		lb.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(errors.New("conflict"))
		pip.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
		pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName("uid-1")).Return(nil).AnyTimes()

		svc := deletionTestService()
		svc.Annotations = named
		svc.Spec.Ports = []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}
		dt := deletionTestDiffTracker(fake.NewSimpleClientset(svc), f)
		dt.SetClusterName("cluster")
		var success bool
		deletionTestUpdater(dt, func(_ string, ok bool, _ error) { success = ok }).deleteInboundService("uid-1", "corr")
		assert.False(t, success)
	})

	t.Run("an unreadable load balancer is kept so the retry still learns its Public IP", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		f := mock_azclient.NewMockClientFactory(ctrl)
		sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
		lb := mock_loadbalancerclient.NewMockInterface(ctrl)
		pip := mock_publicipaddressclient.NewMockInterface(ctrl)
		f.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
		f.EXPECT().GetLoadBalancerClient().Return(lb).AnyTimes()
		f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
		sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		lb.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, errors.New("throttled"))
		lb.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
		pip.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		dt := deletionTestDiffTracker(fake.NewSimpleClientset(deletionTestService()), f)
		var success bool
		deletionTestUpdater(dt, func(_ string, ok bool, _ error) { success = ok }).deleteInboundService("uid-1", "corr")
		assert.False(t, success)
	})
}

// A transient failure releasing a Public IP the controller owns fails the delete so it is retried. The load
// balancer that pointed to the Public IP is already gone then, so the retry must still know which one to release.
func TestServiceUpdaterDeleteInboundService_RetriesTransientPublicIPRelease(t *testing.T) {
	const mineID = "/subscriptions/sub/resourceGroups/other-rg/providers/Microsoft.Network/publicIPAddresses/mine"
	owned := map[string]*string{consts.ServiceTagKey: ptr.To("default/svc"), consts.ClusterNameKey: ptr.To("cluster")}
	for _, transient := range []error{
		&azcore.ResponseError{StatusCode: http.StatusTooManyRequests},
		&azcore.ResponseError{StatusCode: http.StatusServiceUnavailable},
		context.DeadlineExceeded,
	} {
		t.Run(transient.Error(), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			f := mock_azclient.NewMockClientFactory(ctrl)
			sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
			lb := mock_loadbalancerclient.NewMockInterface(ctrl)
			pip := mock_publicipaddressclient.NewMockInterface(ctrl)
			f.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
			f.EXPECT().GetLoadBalancerClient().Return(lb).AnyTimes()
			f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
			sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			lbGone := false
			lb.EXPECT().Get(gomock.Any(), "rg", "uid-1", gomock.Any()).DoAndReturn(func(context.Context, string, string, *string) (*armnetwork.LoadBalancer, error) {
				if lbGone {
					return nil, notFoundError()
				}
				return &armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{
					FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To(mineID)},
					}}},
				}}, nil
			}).AnyTimes()
			lb.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(context.Context, string, string) error {
				lbGone = true
				return nil
			}).AnyTimes()
			pip.EXPECT().Get(gomock.Any(), "other-rg", "mine", gomock.Any()).Return(&armnetwork.PublicIPAddress{Name: ptr.To("mine"), Tags: owned}, nil).AnyTimes()
			pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName("uid-1")).Return(nil).AnyTimes()
			released := 0
			gomock.InOrder(
				pip.EXPECT().Delete(gomock.Any(), "other-rg", "mine").Return(transient),
				pip.EXPECT().Delete(gomock.Any(), "other-rg", "mine").DoAndReturn(func(context.Context, string, string) error {
					released++
					return nil
				}),
			)

			// Chosen by address, so the Service's annotations cannot name the Public IP again on the retry.
			svc := deletionTestService()
			svc.Spec.LoadBalancerIP = "20.0.0.7"
			svc.Spec.Ports = []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}
			dt := deletionTestDiffTracker(fake.NewSimpleClientset(svc), f)
			dt.SetClusterName("cluster")
			var success bool
			su := deletionTestUpdater(dt, func(_ string, ok bool, _ error) { success = ok })

			su.deleteInboundService("uid-1", "corr")
			assert.False(t, success, "a transient failure must be retried, not leak the Public IP")
			su.deleteInboundService("uid-1", "corr")
			assert.True(t, success)
			assert.Equal(t, 1, released, "the retry releases the Public IP although the load balancer is gone")
			assert.Empty(t, su.pendingReleases, "nothing is left to release")
		})
	}
}

// A release deferred until the cluster name is known runs after the Service's delete has completed, so a
// transient failure there is retried on its own.
func TestServiceUpdaterReleaseDeferredPublicIPs_RetriesTransientFailure(t *testing.T) {
	defer func(d time.Duration) { deferredReleaseRetryDelay = d }(deferredReleaseRetryDelay)
	deferredReleaseRetryDelay = 10 * time.Millisecond

	const mineID = "/subscriptions/sub/resourceGroups/other-rg/providers/Microsoft.Network/publicIPAddresses/mine"
	owned := map[string]*string{consts.ServiceTagKey: ptr.To("default/svc"), consts.ClusterNameKey: ptr.To("cluster")}
	ctrl := gomock.NewController(t)
	f := mock_azclient.NewMockClientFactory(ctrl)
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
	pip.EXPECT().Get(gomock.Any(), "other-rg", "mine", gomock.Any()).Return(&armnetwork.PublicIPAddress{Name: ptr.To("mine"), Tags: owned}, nil).AnyTimes()
	released := make(chan struct{}, 1)
	gomock.InOrder(
		pip.EXPECT().Delete(gomock.Any(), "other-rg", "mine").Return(&azcore.ResponseError{StatusCode: http.StatusTooManyRequests}),
		pip.EXPECT().Delete(gomock.Any(), "other-rg", "mine").DoAndReturn(func(context.Context, string, string) error {
			released <- struct{}{}
			return nil
		}),
	)

	dt := deletionTestDiffTracker(fake.NewSimpleClientset(), f)
	su := deletionTestUpdater(dt, func(string, bool, error) {})
	su.ctx, su.cancel = context.WithCancel(context.Background())
	defer su.Stop()
	su.deferredReleases = []deferredPublicIPRelease{{serviceUID: "uid-1", publicIPID: mineID}}
	dt.SetClusterName("cluster")
	su.releaseDeferredPublicIPs()

	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("a deferred release that failed transiently was not retried")
	}
	assert.Eventually(t, func() bool {
		su.mu.Lock()
		defer su.mu.Unlock()
		return len(su.deferredReleases) == 0
	}, time.Second, 10*time.Millisecond)
}

// A transient failure to read the Service while releasing its Public IPs must not skip the one it chose by
// name: the delete is retried instead.
func TestServiceUpdaterDeleteInboundService_RetriesWhenServiceLookupFailsBeforeRelease(t *testing.T) {
	owned := map[string]*string{consts.ServiceTagKey: ptr.To("default/svc"), consts.ClusterNameKey: ptr.To("cluster")}
	ctrl := gomock.NewController(t)
	f := mock_azclient.NewMockClientFactory(ctrl)
	sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
	lb := mock_loadbalancerclient.NewMockInterface(ctrl)
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	f.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(lb).AnyTimes()
	f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
	sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	lb.EXPECT().Get(gomock.Any(), "rg", "uid-1", gomock.Any()).Return(nil, notFoundError()).AnyTimes()
	lb.EXPECT().Delete(gomock.Any(), "rg", "uid-1").Return(nil).AnyTimes()
	pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName("uid-1")).Return(nil).AnyTimes()
	pip.EXPECT().Get(gomock.Any(), "rg", "mine", gomock.Any()).Return(&armnetwork.PublicIPAddress{Name: ptr.To("mine"), Tags: owned}, nil).AnyTimes()
	pip.EXPECT().Delete(gomock.Any(), "rg", "mine").Return(nil).Times(1)

	svc := deletionTestService()
	svc.Annotations = map[string]string{consts.ServiceAnnotationPIPNameDualStack[false]: "mine"}
	svc.Spec.Ports = []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}
	kube := fake.NewSimpleClientset(svc)
	// Only the lookup made to find the Public IPs to release fails; the later one, before the finalizer is
	// removed, succeeds, so only the release can make this delete fail.
	failures := 1
	fail := func(k8stesting.Action) (bool, runtime.Object, error) {
		if failures == 0 {
			return false, nil, nil
		}
		failures--
		return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
	}
	kube.PrependReactor("get", "services", fail)
	kube.PrependReactor("list", "services", fail)
	dt := deletionTestDiffTracker(kube, f)
	dt.SetClusterName("cluster")
	var success bool
	su := deletionTestUpdater(dt, func(_ string, ok bool, _ error) { success = ok })

	su.deleteInboundService("uid-1", "corr")
	assert.False(t, success, "a transient Service lookup must retry the delete, not skip the named Public IP")
	su.deleteInboundService("uid-1", "corr")
	assert.True(t, success)
}

// The same, for a Public IP chosen by address: only the load balancer named it, and it is gone on the retry.
func TestServiceUpdaterDeleteInboundService_RemembersFrontendWhenServiceLookupFails(t *testing.T) {
	const mineID = "/subscriptions/sub/resourceGroups/other-rg/providers/Microsoft.Network/publicIPAddresses/mine"
	owned := map[string]*string{consts.ServiceTagKey: ptr.To("default/svc"), consts.ClusterNameKey: ptr.To("cluster")}
	ctrl := gomock.NewController(t)
	f := mock_azclient.NewMockClientFactory(ctrl)
	sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
	lb := mock_loadbalancerclient.NewMockInterface(ctrl)
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	f.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(lb).AnyTimes()
	f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
	sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	gomock.InOrder(
		lb.EXPECT().Get(gomock.Any(), "rg", "uid-1", gomock.Any()).Return(&armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To(mineID)},
			}}},
		}}, nil),
		lb.EXPECT().Get(gomock.Any(), "rg", "uid-1", gomock.Any()).Return(nil, notFoundError()),
	)
	lb.EXPECT().Delete(gomock.Any(), "rg", "uid-1").Return(nil).AnyTimes()
	pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName("uid-1")).Return(nil).AnyTimes()
	pip.EXPECT().Get(gomock.Any(), "other-rg", "mine", gomock.Any()).Return(&armnetwork.PublicIPAddress{Name: ptr.To("mine"), Tags: owned}, nil).AnyTimes()
	pip.EXPECT().Delete(gomock.Any(), "other-rg", "mine").Return(nil).Times(1)

	svc := deletionTestService()
	svc.Spec.LoadBalancerIP = "20.0.0.7"
	svc.Spec.Ports = []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}
	kube := fake.NewSimpleClientset(svc)
	failures := 1
	fail := func(k8stesting.Action) (bool, runtime.Object, error) {
		if failures == 0 {
			return false, nil, nil
		}
		failures--
		return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
	}
	kube.PrependReactor("get", "services", fail)
	kube.PrependReactor("list", "services", fail)
	dt := deletionTestDiffTracker(kube, f)
	dt.SetClusterName("cluster")
	var success bool
	su := deletionTestUpdater(dt, func(_ string, ok bool, _ error) { success = ok })

	su.deleteInboundService("uid-1", "corr")
	assert.False(t, success)
	su.deleteInboundService("uid-1", "corr")
	assert.True(t, success)
	assert.Empty(t, su.pendingReleases)
}

// A load balancer delete that fails on our side may still complete in Azure, so the retry, which then finds no
// load balancer, must still release the Public IP it used.
func TestServiceUpdaterDeleteInboundService_ReleasesPublicIPAfterFailedLoadBalancerDelete(t *testing.T) {
	const mineID = "/subscriptions/sub/resourceGroups/other-rg/providers/Microsoft.Network/publicIPAddresses/mine"
	owned := map[string]*string{consts.ServiceTagKey: ptr.To("default/svc"), consts.ClusterNameKey: ptr.To("cluster")}
	ctrl := gomock.NewController(t)
	f := mock_azclient.NewMockClientFactory(ctrl)
	sgw := mock_servicegatewayclient.NewMockInterface(ctrl)
	lb := mock_loadbalancerclient.NewMockInterface(ctrl)
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	f.EXPECT().GetServiceGatewayClient().Return(sgw).AnyTimes()
	f.EXPECT().GetLoadBalancerClient().Return(lb).AnyTimes()
	f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
	sgw.EXPECT().UpdateServices(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	gomock.InOrder(
		lb.EXPECT().Get(gomock.Any(), "rg", "uid-1", gomock.Any()).Return(&armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To(mineID)},
			}}},
		}}, nil),
		lb.EXPECT().Get(gomock.Any(), "rg", "uid-1", gomock.Any()).Return(nil, notFoundError()),
	)
	gomock.InOrder(
		lb.EXPECT().Delete(gomock.Any(), "rg", "uid-1").Return(context.DeadlineExceeded),
		lb.EXPECT().Delete(gomock.Any(), "rg", "uid-1").Return(nil),
	)
	pip.EXPECT().Get(gomock.Any(), "other-rg", "mine", gomock.Any()).Return(&armnetwork.PublicIPAddress{Name: ptr.To("mine"), Tags: owned}, nil).AnyTimes()
	pip.EXPECT().Delete(gomock.Any(), "rg", PublicIPName("uid-1")).Return(nil).AnyTimes()
	pip.EXPECT().Delete(gomock.Any(), "other-rg", "mine").Return(nil).Times(1)

	svc := deletionTestService()
	svc.Spec.LoadBalancerIP = "20.0.0.7"
	svc.Spec.Ports = []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}
	dt := deletionTestDiffTracker(fake.NewSimpleClientset(svc), f)
	dt.SetClusterName("cluster")
	var success bool
	su := deletionTestUpdater(dt, func(_ string, ok bool, _ error) { success = ok })

	su.deleteInboundService("uid-1", "corr")
	assert.False(t, success)
	su.deleteInboundService("uid-1", "corr")
	assert.True(t, success)
	assert.Empty(t, su.pendingReleases)
}

func TestServiceUpdaterReleaseOrDefer_DrainsWhenTheClusterNameArrivedMeanwhile(t *testing.T) {
	ctrl := gomock.NewController(t)
	f := mock_azclient.NewMockClientFactory(ctrl)
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
	owned := map[string]*string{consts.ServiceTagKey: ptr.To("default/svc"), consts.ClusterNameKey: ptr.To("cluster")}
	pip.EXPECT().Get(gomock.Any(), "other-rg", "mine", gomock.Any()).Return(&armnetwork.PublicIPAddress{Name: ptr.To("mine"), Tags: owned}, nil).AnyTimes()
	pip.EXPECT().Delete(gomock.Any(), "other-rg", "mine").Return(nil).Times(1)

	dt := deletionTestDiffTracker(fake.NewSimpleClientset(), f)
	su := deletionTestUpdater(dt, func(string, bool, error) {})
	// The caller read the name before it arrived; the first load balancer call then drained an empty queue.
	dt.SetClusterName("cluster")
	dt.serviceUpdater = su
	id := "/subscriptions/sub/resourceGroups/other-rg/providers/Microsoft.Network/publicIPAddresses/mine"
	assert.NoError(t, su.releaseOrDefer(context.Background(), "uid-1", "default/svc", "", id))
	su.mu.Lock()
	defer su.mu.Unlock()
	assert.Empty(t, su.deferredReleases)
}

func TestServiceUpdaterReleaseDeferredPublicIPs_ReportsFailures(t *testing.T) {
	ctrl := gomock.NewController(t)
	f := mock_azclient.NewMockClientFactory(ctrl)
	pip := mock_publicipaddressclient.NewMockInterface(ctrl)
	f.EXPECT().GetPublicIPAddressClient().Return(pip).AnyTimes()
	owned := map[string]*string{consts.ServiceTagKey: ptr.To("default/svc"), consts.ClusterNameKey: ptr.To("cluster")}
	pip.EXPECT().Get(gomock.Any(), "other-rg", "mine", gomock.Any()).Return(&armnetwork.PublicIPAddress{Name: ptr.To("mine"), Tags: owned}, nil).AnyTimes()
	pip.EXPECT().Delete(gomock.Any(), "other-rg", "mine").Return(&azcore.ResponseError{StatusCode: http.StatusBadRequest, ErrorCode: "PublicIPAddressCannotBeDeleted"})

	svc := deletionTestService()
	dt := deletionTestDiffTracker(fake.NewSimpleClientset(svc), f)
	recorder := record.NewFakeRecorder(10)
	dt.SetEventRecorder(recorder)
	su := deletionTestUpdater(dt, func(string, bool, error) {})
	id := "/subscriptions/sub/resourceGroups/other-rg/providers/Microsoft.Network/publicIPAddresses/mine"
	assert.NoError(t, su.releaseOrDefer(context.Background(), "uid-1", "default/svc", "", id))
	dt.SetClusterName("cluster")
	su.releaseDeferredPublicIPs()
	su.mu.Lock()
	assert.Empty(t, su.deferredReleases, "a lasting failure is not retried")
	assert.NotContains(t, su.retryTimers, deferredReleasesRetryKey)
	su.mu.Unlock()
	close(recorder.Events)
	var events []string
	for event := range recorder.Events {
		events = append(events, event)
	}
	assert.True(t, slices.ContainsFunc(events, func(e string) bool { return strings.Contains(e, "PublicIPCleanupFailed") }), "%v", events)
}
