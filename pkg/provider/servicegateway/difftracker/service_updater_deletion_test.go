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
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
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
