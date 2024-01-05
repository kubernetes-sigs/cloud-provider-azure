/*
Copyright 2019 The Kubernetes Authors.

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

package nodemanager

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	cloudproviderapi "k8s.io/cloud-provider/api"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	mocknodeprovider "sigs.k8s.io/cloud-provider-azure/pkg/nodemanager/mock"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/controller/testutil"
)

func TestEnsureNodeExistsByProviderID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		testName           string
		node               *v1.Node
		providerID         string
		expectedNodeExists bool
		nodeNameErr        error
		expectedErr        error
	}{
		{
			testName:           "node exists by provider id",
			nodeNameErr:        nil,
			expectedNodeExists: true,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node0",
				},
				Spec: v1.NodeSpec{
					ProviderID: "node0",
				},
			},
		},
		{
			testName:           "node exists by Azure provider",
			nodeNameErr:        nil,
			expectedNodeExists: true,
			providerID:         "node0",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node0",
				},
				Spec: v1.NodeSpec{},
			},
		},
		{
			testName:           "node does not exist",
			nodeNameErr:        cloudprovider.InstanceNotFound,
			expectedErr:        nil,
			expectedNodeExists: false,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node0",
				},
				Spec: v1.NodeSpec{},
			},
		},
		{
			testName:           "provider id returns error",
			nodeNameErr:        errors.New("UnknownError"),
			expectedErr:        errors.New("UnknownError"),
			expectedNodeExists: false,
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node0",
				},
				Spec: v1.NodeSpec{},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.testName, func(t *testing.T) {
			ctx := context.TODO()
			mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
			if tc.node.Spec.ProviderID == "" {
				mockNP.EXPECT().InstanceID(ctx, types.NodeName(tc.node.Name)).Return(tc.providerID, tc.nodeNameErr)
			}

			cnc := &CloudNodeController{nodeProvider: mockNP}
			exists, err := cnc.ensureNodeExistsByProviderID(ctx, tc.node)
			assert.Equal(t, err, tc.expectedErr)
			assert.Equal(t, tc.expectedNodeExists, exists)
		})
	}
}

// This test checks that a node with the external cloud provider taint is cloudprovider initialized
func TestNodeInitialized(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	fnh := &testutil.FakeNodeHandler{
		Existing: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node0",
					CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:               v1.NodeReady,
							Status:             v1.ConditionUnknown,
							LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
						},
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key:    cloudproviderapi.TaintExternalCloudProvider,
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
		Clientset:      fake.NewSimpleClientset(&v1.PodList{}),
		DeleteWaitChan: make(chan struct{}),
	}

	ctx := context.TODO()
	factory := informers.NewSharedInformerFactory(fnh, 0)
	mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
	mockNP.EXPECT().InstanceID(ctx, types.NodeName("node0")).Return("node0", nil)
	mockNP.EXPECT().InstanceType(ctx, types.NodeName("node0")).Return("Standard_D2_v3", nil)
	mockNP.EXPECT().GetZone(ctx, gomock.Any()).Return(cloudprovider.Zone{
		Region:        "eastus",
		FailureDomain: "1",
	}, nil)
	mockNP.EXPECT().NodeAddresses(ctx, types.NodeName("node0")).Return([]v1.NodeAddress{
		{
			Type:    v1.NodeHostName,
			Address: "node0.cloud.internal",
		},
		{
			Type:    v1.NodeInternalIP,
			Address: "10.0.0.1",
		},
		{
			Type:    v1.NodeExternalIP,
			Address: "132.143.154.163",
		},
	}, nil).AnyTimes()
	mockNP.EXPECT().GetPlatformSubFaultDomain().Return("1", nil)

	cloudNodeController := NewCloudNodeController(
		"node0",
		factory.Core().V1().Nodes(),
		fnh,
		mockNP,
		time.Second,
		false,
		false)

	cloudNodeController.AddCloudNode(ctx, fnh.Existing[0])

	assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
	assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
	assert.Equal(t, 0, len(fnh.UpdatedNodes[0].Spec.Taints), "Node Taint was not removed after cloud init")
	assert.Equal(t, "1", fnh.UpdatedNodes[0].Labels[consts.LabelPlatformSubFaultDomain])
}

func TestUpdateCloudNode(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	fnh := &testutil.FakeNodeHandler{
		Existing: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node0",
					CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:               v1.NodeReady,
							Status:             v1.ConditionUnknown,
							LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
						},
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key:    cloudproviderapi.TaintExternalCloudProvider,
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
		Clientset:      fake.NewSimpleClientset(&v1.PodList{}),
		DeleteWaitChan: make(chan struct{}),
	}

	ctx := context.TODO()
	factory := informers.NewSharedInformerFactory(fnh, 0)
	mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
	mockNP.EXPECT().InstanceID(ctx, types.NodeName("node0")).Return("node0", nil)
	mockNP.EXPECT().InstanceType(ctx, types.NodeName("node0")).Return("Standard_D2_v3", nil)
	mockNP.EXPECT().GetZone(ctx, gomock.Any()).Return(cloudprovider.Zone{
		Region:        "eastus",
		FailureDomain: "1",
	}, nil)
	mockNP.EXPECT().NodeAddresses(ctx, types.NodeName("node0")).Return([]v1.NodeAddress{
		{
			Type:    v1.NodeHostName,
			Address: "node0.cloud.internal",
		},
		{
			Type:    v1.NodeInternalIP,
			Address: "10.0.0.1",
		},
		{
			Type:    v1.NodeExternalIP,
			Address: "132.143.154.163",
		},
	}, nil).AnyTimes()
	mockNP.EXPECT().GetPlatformSubFaultDomain().Return("1", nil)

	eventBroadcaster := record.NewBroadcaster()
	cloudNodeController := NewCloudNodeController(
		"node0",
		factory.Core().V1().Nodes(),
		fnh,
		mockNP,
		time.Second,
		true,
		false)
	eventBroadcaster.StartLogging(klog.Infof)

	cloudNodeController.UpdateCloudNode(ctx, fnh.Existing[0], fnh.Existing[0])

	assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
	assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
	assert.Equal(t, 0, len(fnh.UpdatedNodes[0].Spec.Taints), "Node Taint was not removed after cloud init")
	assert.Equal(t, 2, len(fnh.UpdatedNodes[0].Status.Conditions), "Node Contions was not updated")
	assert.Equal(t, "NetworkUnavailable", string(fnh.UpdatedNodes[0].Status.Conditions[0].Type), "Node Condition NetworkUnavailable was not updated")
	assert.Equal(t, "1", fnh.UpdatedNodes[0].Labels[consts.LabelPlatformSubFaultDomain])
}

// This test checks that a node without the external cloud provider taint are NOT cloudprovider initialized
func TestNodeIgnored(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	fnh := &testutil.FakeNodeHandler{
		Existing: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node0",
					CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:               v1.NodeReady,
							Status:             v1.ConditionUnknown,
							LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
						},
					},
				},
			},
		},
		Clientset:      fake.NewSimpleClientset(&v1.PodList{}),
		DeleteWaitChan: make(chan struct{}),
	}

	factory := informers.NewSharedInformerFactory(fnh, 0)
	mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)

	eventBroadcaster := record.NewBroadcaster()
	cloudNodeController := NewCloudNodeController(
		"node0",
		factory.Core().V1().Nodes(),
		fnh,
		mockNP,
		time.Second,
		false,
		false)
	eventBroadcaster.StartLogging(klog.Infof)

	cloudNodeController.AddCloudNode(context.TODO(), fnh.Existing[0])
	assert.Equal(t, 0, len(fnh.UpdatedNodes), "Node was wrongly updated")

}

// This test checks that a node with the external cloud provider taint is cloudprovider initialized and
// and that zone labels are added correctly
func TestZoneInitialized(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	t.Run("with stable zone labels", func(t *testing.T) {
		fnh := &testutil.FakeNodeHandler{
			Existing: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "node0",
						CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
						Labels:            map[string]string{},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:               v1.NodeReady,
								Status:             v1.ConditionUnknown,
								LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
								LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							},
						},
					},
					Spec: v1.NodeSpec{
						Taints: []v1.Taint{
							{
								Key:    cloudproviderapi.TaintExternalCloudProvider,
								Value:  "true",
								Effect: v1.TaintEffectNoSchedule,
							},
						},
					},
				},
			},
			Clientset:      fake.NewSimpleClientset(&v1.PodList{}),
			DeleteWaitChan: make(chan struct{}),
		}

		ctx := context.TODO()
		factory := informers.NewSharedInformerFactory(fnh, 0)
		mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
		mockNP.EXPECT().InstanceID(ctx, types.NodeName("node0")).Return("node0", nil)
		mockNP.EXPECT().InstanceType(ctx, types.NodeName("node0")).Return("Standard_D2_v3", nil)
		mockNP.EXPECT().GetZone(ctx, gomock.Any()).Return(cloudprovider.Zone{
			Region:        "eastus",
			FailureDomain: "eastus-1",
		}, nil)
		mockNP.EXPECT().NodeAddresses(ctx, types.NodeName("node0")).Return([]v1.NodeAddress{
			{
				Type:    v1.NodeHostName,
				Address: "node0.cloud.internal",
			},
			{
				Type:    v1.NodeInternalIP,
				Address: "10.0.0.1",
			},
			{
				Type:    v1.NodeExternalIP,
				Address: "132.143.154.163",
			},
		}, nil).AnyTimes()
		mockNP.EXPECT().GetPlatformSubFaultDomain().Return("", nil)

		eventBroadcaster := record.NewBroadcaster()
		cloudNodeController := &CloudNodeController{
			kubeClient:   fnh,
			nodeName:     "node0",
			nodeProvider: mockNP,
			nodeInformer: factory.Core().V1().Nodes(),
			recorder:     eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cloud-node-controller"}),
		}
		eventBroadcaster.StartLogging(klog.Infof)

		cloudNodeController.AddCloudNode(context.TODO(), fnh.Existing[0])

		assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
		assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
		assert.Equal(t, 3, len(fnh.UpdatedNodes[0].ObjectMeta.Labels),
			"Node label for Region and Zone were not set")
		assert.Equal(t, "eastus", fnh.UpdatedNodes[0].ObjectMeta.Labels[v1.LabelZoneRegionStable],
			"Node Region not correctly updated")
		assert.Equal(t, "eastus-1", fnh.UpdatedNodes[0].ObjectMeta.Labels[v1.LabelZoneFailureDomainStable],
			"Node FailureDomain not correctly updated")
	})

	t.Run("with beta zone labels", func(t *testing.T) {
		fnh := &testutil.FakeNodeHandler{
			Existing: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "node0",
						CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
						Labels:            map[string]string{},
					},
					Status: v1.NodeStatus{
						Conditions: []v1.NodeCondition{
							{
								Type:               v1.NodeReady,
								Status:             v1.ConditionUnknown,
								LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
								LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							},
						},
					},
					Spec: v1.NodeSpec{
						Taints: []v1.Taint{
							{
								Key:    cloudproviderapi.TaintExternalCloudProvider,
								Value:  "true",
								Effect: v1.TaintEffectNoSchedule,
							},
						},
					},
				},
			},
			Clientset:      fake.NewSimpleClientset(&v1.PodList{}),
			DeleteWaitChan: make(chan struct{}),
		}

		ctx := context.TODO()
		factory := informers.NewSharedInformerFactory(fnh, 0)
		mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
		mockNP.EXPECT().InstanceID(ctx, types.NodeName("node0")).Return("node0", nil)
		mockNP.EXPECT().InstanceType(ctx, types.NodeName("node0")).Return("Standard_D2_v3", nil)
		mockNP.EXPECT().GetZone(ctx, gomock.Any()).Return(cloudprovider.Zone{
			Region:        "eastus",
			FailureDomain: "eastus-1",
		}, nil)
		mockNP.EXPECT().NodeAddresses(ctx, types.NodeName("node0")).Return([]v1.NodeAddress{
			{
				Type:    v1.NodeHostName,
				Address: "node0.cloud.internal",
			},
			{
				Type:    v1.NodeInternalIP,
				Address: "10.0.0.1",
			},
			{
				Type:    v1.NodeExternalIP,
				Address: "132.143.154.163",
			},
		}, nil).AnyTimes()
		mockNP.EXPECT().GetPlatformSubFaultDomain().Return("", nil)

		eventBroadcaster := record.NewBroadcaster()
		cloudNodeController := &CloudNodeController{
			kubeClient:               fnh,
			nodeName:                 "node0",
			nodeProvider:             mockNP,
			nodeInformer:             factory.Core().V1().Nodes(),
			recorder:                 eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cloud-node-controller"}),
			enableBetaTopologyLabels: true,
		}
		eventBroadcaster.StartLogging(klog.Infof)

		cloudNodeController.AddCloudNode(context.TODO(), fnh.Existing[0])

		assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
		assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
		assert.Equal(t, 6, len(fnh.UpdatedNodes[0].ObjectMeta.Labels),
			"Node label for Region and Zone were not set")
		assert.Equal(t, "eastus", fnh.UpdatedNodes[0].ObjectMeta.Labels[v1.LabelZoneRegionStable],
			"Node Region not correctly updated")
		assert.Equal(t, "eastus-1", fnh.UpdatedNodes[0].ObjectMeta.Labels[v1.LabelZoneFailureDomainStable],
			"Node FailureDomain not correctly updated")
		assert.Equal(t, "eastus", fnh.UpdatedNodes[0].ObjectMeta.Labels[v1.LabelZoneRegion],
			"Node Region not correctly updated")
		assert.Equal(t, "eastus-1", fnh.UpdatedNodes[0].ObjectMeta.Labels[v1.LabelZoneFailureDomain],
			"Node FailureDomain not correctly updated")
	})
}

// This test checks that a node with the external cloud provider taint is cloudprovider initialized and
// and nodeAddresses are updated from the cloudprovider
func TestAddCloudNode(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fnh := &testutil.FakeNodeHandler{
		Existing: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node0",
					CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
					Labels:            map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:               v1.NodeReady,
							Status:             v1.ConditionUnknown,
							LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
						},
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key:    "ImproveCoverageTaint",
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
						{
							Key:    cloudproviderapi.TaintExternalCloudProvider,
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
		Clientset:      fake.NewSimpleClientset(&v1.PodList{}),
		DeleteWaitChan: make(chan struct{}),
	}

	mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
	mockNP.EXPECT().InstanceID(gomock.Any(), types.NodeName("node0")).Return("node0", nil)
	mockNP.EXPECT().InstanceType(gomock.Any(), types.NodeName("node0")).Return("Standard_D2_v3", nil)
	mockNP.EXPECT().GetZone(gomock.Any(), gomock.Any()).Return(cloudprovider.Zone{
		Region:        "eastus",
		FailureDomain: "eastus-1",
	}, nil)
	mockNP.EXPECT().NodeAddresses(gomock.Any(), types.NodeName("node0")).Return([]v1.NodeAddress{
		{
			Type:    v1.NodeHostName,
			Address: "node0.cloud.internal",
		},
		{
			Type:    v1.NodeInternalIP,
			Address: "10.0.0.1",
		},
		{
			Type:    v1.NodeExternalIP,
			Address: "132.143.154.163",
		},
	}, nil).AnyTimes()
	mockNP.EXPECT().GetPlatformSubFaultDomain().Return("", nil)

	factory := informers.NewSharedInformerFactory(fnh, 0)
	nodeInformer := factory.Core().V1().Nodes()

	cloudNodeController := NewCloudNodeController(
		"node0",
		nodeInformer,
		fnh,
		mockNP,
		time.Second,
		false,
		false)
	factory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), nodeInformer.Informer().HasSynced)

	cloudNodeController.AddCloudNode(ctx, fnh.Existing[0])
	assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
	assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
	assert.Equal(t, 3, len(fnh.UpdatedNodes[0].Status.Addresses), "Node status not updated")
}

func TestUpdateNodeAddresses(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fnh := &testutil.FakeNodeHandler{
		Existing: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node0",
					CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
					Labels:            map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:               v1.NodeReady,
							Status:             v1.ConditionUnknown,
							LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
						},
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key:    "ImproveCoverageTaint",
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
		Clientset:      fake.NewSimpleClientset(&v1.PodList{}),
		DeleteWaitChan: make(chan struct{}),
	}

	mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
	factory := informers.NewSharedInformerFactory(fnh, 0)
	nodeInformer := factory.Core().V1().Nodes()

	cloudNodeController := NewCloudNodeController(
		"node0",
		nodeInformer,
		fnh,
		mockNP,
		time.Second,
		false,
		false)
	factory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(), nodeInformer.Informer().HasSynced)

	mockNP.EXPECT().InstanceID(gomock.Any(), types.NodeName("node0")).Return("node0", nil)
	mockNP.EXPECT().NodeAddresses(ctx, types.NodeName("node0")).Return([]v1.NodeAddress{
		{
			Type:    v1.NodeHostName,
			Address: "node0.cloud.internal",
		},
		{
			Type:    v1.NodeInternalIP,
			Address: "10.0.0.1",
		},
	}, nil)
	cloudNodeController.UpdateNodeStatus(ctx)
	updatedNodes := fnh.GetUpdatedNodesCopy()
	assert.Equal(t, 2, len(updatedNodes[0].Status.Addresses), "Node Addresses not correctly updated")
}

// This test checks that a node with the external cloud provider taint is cloudprovider initialized and
// and the provided node ip is validated with the cloudprovider and nodeAddresses are updated from the cloudprovider
func TestNodeProvidedIPAddresses(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	fnh := &testutil.FakeNodeHandler{
		Existing: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node0",
					CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
					Labels:            map[string]string{},
					Annotations: map[string]string{
						cloudproviderapi.AnnotationAlphaProvidedIPAddr: "10.0.0.1",
					},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:               v1.NodeReady,
							Status:             v1.ConditionUnknown,
							LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
						},
					},
					Addresses: []v1.NodeAddress{
						{
							Type:    v1.NodeHostName,
							Address: "node0.cloud.internal",
						},
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key:    "ImproveCoverageTaint",
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
						{
							Key:    cloudproviderapi.TaintExternalCloudProvider,
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
					ProviderID: "node0",
				},
			},
		},
		Clientset:      fake.NewSimpleClientset(&v1.PodList{}),
		DeleteWaitChan: make(chan struct{}),
	}

	ctx := context.TODO()
	factory := informers.NewSharedInformerFactory(fnh, 0)
	mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
	mockNP.EXPECT().InstanceType(ctx, types.NodeName("node0")).Return("Standard_D2_v3", nil)
	mockNP.EXPECT().GetZone(ctx, gomock.Any()).Return(cloudprovider.Zone{
		Region:        "eastus",
		FailureDomain: "eastus-1",
	}, nil)
	mockNP.EXPECT().NodeAddresses(ctx, types.NodeName("node0")).Return([]v1.NodeAddress{
		{
			Type:    v1.NodeInternalIP,
			Address: "10.0.0.1",
		},
		{
			Type:    v1.NodeExternalIP,
			Address: "132.143.154.163",
		},
	}, nil).AnyTimes()
	mockNP.EXPECT().GetPlatformSubFaultDomain().Return("", nil)

	eventBroadcaster := record.NewBroadcaster()
	cloudNodeController := NewCloudNodeController(
		"node0",
		factory.Core().V1().Nodes(),
		fnh,
		mockNP,
		time.Second,
		false,
		false)
	eventBroadcaster.StartLogging(klog.Infof)

	cloudNodeController.AddCloudNode(context.TODO(), fnh.Existing[0])
	assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
	assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
	assert.Equal(t, 3, len(fnh.UpdatedNodes[0].Status.Addresses), "Node status unexpectedly updated")

	cloudNodeController.UpdateNodeStatus(context.TODO())
	updatedNodes := fnh.GetUpdatedNodesCopy()
	assert.Equal(t, 3, len(updatedNodes[0].Status.Addresses), "Node Addresses not correctly updated")
	assert.Equal(t, "10.0.0.1", updatedNodes[0].Status.Addresses[0].Address, "Node Addresses not correctly updated")
}

func Test_reconcileNodeLabels(t *testing.T) {
	testcases := []struct {
		name           string
		labels         map[string]string
		expectedLabels map[string]string
		expectedErr    error
	}{
		{
			name:           "no labels",
			labels:         map[string]string{},
			expectedLabels: map[string]string{},
			expectedErr:    nil,
		},
		{
			name: "requires reconcile",
			labels: map[string]string{
				v1.LabelZoneFailureDomainStable: "foo",
				v1.LabelZoneRegionStable:        "bar",
				v1.LabelInstanceTypeStable:      "the-best-type",
			},
			expectedLabels: map[string]string{
				v1.LabelZoneFailureDomain:       "foo",
				v1.LabelZoneRegion:              "bar",
				v1.LabelZoneFailureDomainStable: "foo",
				v1.LabelZoneRegionStable:        "bar",
				v1.LabelInstanceType:            "the-best-type",
				v1.LabelInstanceTypeStable:      "the-best-type",
			},
			expectedErr: nil,
		},
		{
			name: "doesn't require reconcile",
			labels: map[string]string{
				v1.LabelZoneFailureDomain:       "foo",
				v1.LabelZoneRegion:              "bar",
				v1.LabelZoneFailureDomainStable: "foo",
				v1.LabelZoneRegionStable:        "bar",
				v1.LabelInstanceType:            "the-best-type",
				v1.LabelInstanceTypeStable:      "the-best-type",
			},
			expectedLabels: map[string]string{
				v1.LabelZoneFailureDomain:       "foo",
				v1.LabelZoneRegion:              "bar",
				v1.LabelZoneFailureDomainStable: "foo",
				v1.LabelZoneRegionStable:        "bar",
				v1.LabelInstanceType:            "the-best-type",
				v1.LabelInstanceTypeStable:      "the-best-type",
			},
			expectedErr: nil,
		},
		{
			name: "require reconcile -- secondary labels are different from primary",
			labels: map[string]string{
				v1.LabelZoneFailureDomainStable: "foo",
				v1.LabelZoneRegionStable:        "bar",
				v1.LabelZoneFailureDomain:       "wrongfoo",
				v1.LabelZoneRegion:              "wrongbar",
				v1.LabelInstanceTypeStable:      "the-best-type",
				v1.LabelInstanceType:            "the-wrong-type",
			},
			expectedLabels: map[string]string{
				v1.LabelZoneFailureDomain:       "foo",
				v1.LabelZoneRegion:              "bar",
				v1.LabelZoneFailureDomainStable: "foo",
				v1.LabelZoneRegionStable:        "bar",
				v1.LabelInstanceType:            "the-best-type",
				v1.LabelInstanceTypeStable:      "the-best-type",
			},
			expectedErr: nil,
		},
	}

	for _, test := range testcases {
		t.Run(test.name, func(t *testing.T) {
			testNode := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node01",
					Labels: test.labels,
				},
			}

			clientset := fake.NewSimpleClientset(testNode)
			factory := informers.NewSharedInformerFactory(clientset, 0)

			cnc := &CloudNodeController{
				kubeClient:   clientset,
				nodeInformer: factory.Core().V1().Nodes(),
				// Test using the beta toplogy labels.
				labelReconcileInfo: betaToplogyLabels,
			}

			// activate node informer
			factory.Core().V1().Nodes().Informer()
			factory.Start(nil)
			factory.WaitForCacheSync(nil)

			err := cnc.reconcileNodeLabels(testNode)
			if !errors.Is(err, test.expectedErr) {
				t.Logf("actual err: %v", err)
				t.Logf("expected err: %v", test.expectedErr)
				t.Errorf("unexpected error")
			}

			actualNode, err := clientset.CoreV1().Nodes().Get(context.TODO(), "node01", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("error getting updated node: %v", err)
			}

			if !reflect.DeepEqual(actualNode.Labels, test.expectedLabels) {
				t.Logf("actual node labels: %v", actualNode.Labels)
				t.Logf("expected node labels: %v", test.expectedLabels)
				t.Errorf("updated node did not match expected node")
			}
		})
	}
}

// Tests that node address changes are detected correctly
func TestNodeAddressesChangeDetected(t *testing.T) {
	testcases := []struct {
		desc            string
		addrSet1        []v1.NodeAddress
		addrSet2        []v1.NodeAddress
		expectedChanged bool
	}{
		{
			desc: "IPs not changed",
			addrSet1: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "fe::1",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "132.143.154.163",
				},
			},
			addrSet2: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "fe::1",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "132.143.154.163",
				},
			},
			expectedChanged: false,
		},
		{
			desc: "IPs changed",
			addrSet1: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "132.143.154.164",
				},
			},
			addrSet2: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "132.143.154.163",
				},
			},
			expectedChanged: true,
		},
		{
			desc: "IPs and hostname changed set1",
			addrSet1: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "132.143.154.164",
				},
				{
					Type:    v1.NodeHostName,
					Address: "hostname.aks.test",
				},
			},
			addrSet2: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "132.143.154.164",
				},
			},
			expectedChanged: true,
		},
		{
			desc: "IPs and hostname changed set2",
			addrSet1: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "132.143.154.164",
				},
			},
			addrSet2: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "132.143.154.164",
				},
				{
					Type:    v1.NodeHostName,
					Address: "hostname.aks.test",
				},
			},
			expectedChanged: true,
		},
		{
			desc: "IPs exchanged",
			addrSet1: []v1.NodeAddress{
				{
					Type:    v1.NodeExternalIP,
					Address: "10.0.0.1",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "132.143.154.163",
				},
			},
			addrSet2: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "132.143.154.163",
				},
			},
			expectedChanged: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			assert.Equal(t, tc.expectedChanged, nodeAddressesChangeDetected(tc.addrSet1, tc.addrSet2))
		})
	}
}

// This test checks that a node with the external cloud provider taint is cloudprovider initialized
// and node addresses will not be updated when node isn't present according to the cloudprovider
func TestNodeAddressesNotUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	fnh := &testutil.FakeNodeHandler{
		Clientset: fake.NewSimpleClientset(),
		Existing: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node0",
					CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
					Labels:            map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:               v1.NodeReady,
							Status:             v1.ConditionUnknown,
							LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
						},
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key:    "ImproveCoverageTaint",
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
	}

	ctx := context.TODO()
	factory := informers.NewSharedInformerFactory(fnh, 0)
	mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
	mockNP.EXPECT().InstanceID(ctx, types.NodeName("node0")).Return("", nil)

	eventBroadcaster := record.NewBroadcaster()
	cloudNodeController := &CloudNodeController{
		nodeName:                  "node0",
		kubeClient:                fnh,
		nodeInformer:              factory.Core().V1().Nodes(),
		nodeProvider:              mockNP,
		recorder:                  eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cloud-node-controller"}),
		nodeStatusUpdateFrequency: 1 * time.Second,
	}
	err := cloudNodeController.updateNodeAddress(context.TODO(), fnh.Existing[0])
	if err != nil {
		t.Errorf("unexpected error when updating node address: %v", err)
	}

	if len(fnh.UpdatedNodes) != 0 {
		t.Errorf("Node was not correctly updated, the updated len(nodes) got: %v, wanted=0", len(fnh.UpdatedNodes))
	}
}

// This test checks that a node is set with the correct providerID
func TestNodeProviderID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	fnh := &testutil.FakeNodeHandler{
		Existing: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node0",
					CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
					Labels:            map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:               v1.NodeReady,
							Status:             v1.ConditionUnknown,
							LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
						},
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key:    "ImproveCoverageTaint",
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
						{
							Key:    cloudproviderapi.TaintExternalCloudProvider,
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
		Clientset:      fake.NewSimpleClientset(&v1.PodList{}),
		DeleteWaitChan: make(chan struct{}),
	}

	ctx := context.TODO()
	factory := informers.NewSharedInformerFactory(fnh, 0)
	mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
	mockNP.EXPECT().InstanceID(ctx, types.NodeName("node0")).Return("test:///12345", nil)
	mockNP.EXPECT().InstanceType(ctx, types.NodeName("node0")).Return("Standard_D2_v3", nil)
	mockNP.EXPECT().GetZone(ctx, gomock.Any()).Return(cloudprovider.Zone{
		Region:        "eastus",
		FailureDomain: "eastus-1",
	}, nil)
	mockNP.EXPECT().NodeAddresses(ctx, types.NodeName("node0")).Return([]v1.NodeAddress{
		{
			Type:    v1.NodeHostName,
			Address: "node0",
		},
		{
			Type:    v1.NodeInternalIP,
			Address: "10.0.0.1",
		},
		{
			Type:    v1.NodeExternalIP,
			Address: "132.143.154.163",
		},
	}, nil).AnyTimes()
	mockNP.EXPECT().GetPlatformSubFaultDomain().Return("", nil).AnyTimes()

	eventBroadcaster := record.NewBroadcaster()
	cloudNodeController := &CloudNodeController{
		kubeClient:                fnh,
		nodeInformer:              factory.Core().V1().Nodes(),
		nodeName:                  "node0",
		nodeProvider:              mockNP,
		nodeStatusUpdateFrequency: 1 * time.Second,
		recorder:                  eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cloud-node-controller"}),
	}
	eventBroadcaster.StartLogging(klog.Infof)

	cloudNodeController.AddCloudNode(context.TODO(), fnh.Existing[0])

	assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
	assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
	assert.Equal(t, "test:///12345", fnh.UpdatedNodes[0].Spec.ProviderID, "Node ProviderID not set correctly")
}

// This test checks that a node's provider ID will not be overwritten
func TestNodeProviderIDAlreadySet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	fnh := &testutil.FakeNodeHandler{
		Existing: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node0",
					CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
					Labels:            map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:               v1.NodeReady,
							Status:             v1.ConditionUnknown,
							LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
						},
					},
				},
				Spec: v1.NodeSpec{
					ProviderID: "test-provider-id",
					Taints: []v1.Taint{
						{
							Key:    "ImproveCoverageTaint",
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
						{
							Key:    cloudproviderapi.TaintExternalCloudProvider,
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
		Clientset:      fake.NewSimpleClientset(&v1.PodList{}),
		DeleteWaitChan: make(chan struct{}),
	}

	ctx := context.TODO()
	factory := informers.NewSharedInformerFactory(fnh, 0)
	mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
	mockNP.EXPECT().InstanceType(ctx, types.NodeName("node0")).Return("Standard_D2_v3", nil)
	mockNP.EXPECT().GetZone(ctx, gomock.Any()).Return(cloudprovider.Zone{
		Region:        "eastus",
		FailureDomain: "eastus-1",
	}, nil)
	mockNP.EXPECT().NodeAddresses(ctx, types.NodeName("node0")).Return([]v1.NodeAddress{
		{
			Type:    v1.NodeHostName,
			Address: "node0",
		},
		{
			Type:    v1.NodeInternalIP,
			Address: "10.0.0.1",
		},
		{
			Type:    v1.NodeExternalIP,
			Address: "132.143.154.163",
		},
	}, nil).AnyTimes()
	mockNP.EXPECT().GetPlatformSubFaultDomain().Return("", nil).AnyTimes()

	eventBroadcaster := record.NewBroadcaster()
	cloudNodeController := &CloudNodeController{
		kubeClient:                fnh,
		nodeInformer:              factory.Core().V1().Nodes(),
		nodeName:                  "node0",
		nodeProvider:              mockNP,
		nodeStatusUpdateFrequency: 1 * time.Second,
		recorder:                  eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cloud-node-controller"}),
	}
	eventBroadcaster.StartLogging(klog.Infof)

	cloudNodeController.AddCloudNode(context.TODO(), fnh.Existing[0])

	assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
	assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
	// CCM node controller should not overwrite provider if it's already set
	assert.Equal(t, "test-provider-id", fnh.UpdatedNodes[0].Spec.ProviderID, "Node ProviderID not set correctly")
}

// This test checks that a node manager should retry 20 times when failing to get providerID and then panic
func TestNodeProviderIDNotSet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	fnh := &testutil.FakeNodeHandler{
		Existing: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "node0",
					CreationTimestamp: metav1.Date(2012, 1, 1, 0, 0, 0, 0, time.UTC),
					Labels:            map[string]string{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:               v1.NodeReady,
							Status:             v1.ConditionUnknown,
							LastHeartbeatTime:  metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
							LastTransitionTime: metav1.Date(2015, 1, 1, 12, 0, 0, 0, time.UTC),
						},
					},
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{
						{
							Key:    "ImproveCoverageTaint",
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
						{
							Key:    cloudproviderapi.TaintExternalCloudProvider,
							Value:  "true",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
		Clientset:      fake.NewSimpleClientset(&v1.PodList{}),
		DeleteWaitChan: make(chan struct{}),
	}

	ctx := context.TODO()
	factory := informers.NewSharedInformerFactory(fnh, 0)
	mockNP := mocknodeprovider.NewMockNodeProvider(ctrl)
	// InstanceID function should be retried for 20 times when error happens consistently
	mockNP.EXPECT().InstanceID(ctx, types.NodeName("node0")).Return("", cloudprovider.InstanceNotFound).MinTimes(20).MaxTimes(20)

	eventBroadcaster := record.NewBroadcaster()
	cloudNodeController := &CloudNodeController{
		kubeClient:                fnh,
		nodeInformer:              factory.Core().V1().Nodes(),
		nodeName:                  "node0",
		nodeProvider:              mockNP,
		nodeStatusUpdateFrequency: 1 * time.Second,
		recorder:                  eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cloud-node-controller"}),
	}
	eventBroadcaster.StartLogging(klog.Infof)

	// Expect AddCloudNode() to panic when providerID is not set properly
	func() {
		defer func() {
			err := recover()
			assert.NotNil(t, err, "AddCloudNode() didn't panic")
		}()

		cloudNodeController.AddCloudNode(context.TODO(), fnh.Existing[0])
		t.Errorf("AddCloudNode() didn't panic when providerID not found")
	}()

	// Node update should fail
	assert.Equal(t, 0, len(fnh.UpdatedNodes), "Node was updated (unexpected)")
}
