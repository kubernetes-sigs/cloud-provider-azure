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
	mockNP.EXPECT().GetPlatformSubFaultDomain(ctx).Return("1", nil)
	mockNP.EXPECT().GetMetadataLabels(ctx).Return(map[string]string{
		consts.LabelPlatformInterconnectGroup:    "group-123",
		consts.LabelPlatformInterconnectSubgroup: "subgroup-123",
	}, nil)

	cloudNodeController := NewCloudNodeController(
		"node0",
		factory.Core().V1().Nodes(),
		fnh,
		mockNP,
		time.Second,
		false,
		false)

	err := cloudNodeController.handleNodeEvent(ctx, fnh.Existing[0])
	assert.NoError(t, err)

	assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
	assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
	assert.Equal(t, 0, len(fnh.UpdatedNodes[0].Spec.Taints), "Node Taint was not removed after cloud init")
	assert.Equal(t, "1", fnh.UpdatedNodes[0].Labels[consts.LabelPlatformSubFaultDomain])
	assert.Equal(t, "group-123", fnh.UpdatedNodes[0].Labels[consts.LabelPlatformInterconnectGroup])
	assert.Equal(t, "subgroup-123", fnh.UpdatedNodes[0].Labels[consts.LabelPlatformInterconnectSubgroup])
}

func TestMetadataLabelModifiers(t *testing.T) {
	providerErr := errors.New("metadata evaluation error")
	for _, tc := range []struct {
		name         string
		labels       map[string]string
		err          error
		want         string
		wantSubgroup string
		wantError    string
	}{
		{name: "group", labels: map[string]string{consts.LabelPlatformInterconnectGroup: "new-group"}, want: "new-group", wantSubgroup: "existing-subgroup"},
		{name: "subgroup", labels: map[string]string{consts.LabelPlatformInterconnectSubgroup: "new-subgroup"}, want: "existing-group", wantSubgroup: "new-subgroup"},
		{name: "both", labels: map[string]string{consts.LabelPlatformInterconnectGroup: "new-group", consts.LabelPlatformInterconnectSubgroup: "new-subgroup"}, want: "new-group", wantSubgroup: "new-subgroup"},
		{name: "M1 aliases", labels: map[string]string{consts.LabelPlatformInterconnectGroup: "legacy", consts.LabelPlatformInterconnectSubgroup: "legacy"}, want: "legacy", wantSubgroup: "legacy"},
		{name: "omitted", want: "existing-group", wantSubgroup: "existing-subgroup"},
		{name: "empty", labels: map[string]string{consts.LabelPlatformInterconnectGroup: "", consts.LabelPlatformInterconnectSubgroup: ""}, want: "existing-group", wantSubgroup: "existing-subgroup"},
		{name: "error", err: providerErr, wantError: "get metadata labels: metadata evaluation error"},
		{name: "unmanaged key", labels: map[string]string{v1.LabelZoneRegionStable: "unexpected"}, wantError: "unexpected metadata label key"},
		{name: "mixed unmanaged key", labels: map[string]string{consts.LabelPlatformInterconnectSubgroup: "new-subgroup", v1.LabelZoneRegionStable: "unexpected"}, wantError: "unexpected metadata label key"},
		{name: "partial labels with error", labels: map[string]string{consts.LabelPlatformInterconnectGroup: "new-group"}, err: providerErr, wantError: "get metadata labels: metadata evaluation error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mockNP := mocknodeprovider.NewMockNodeProvider(gomock.NewController(t))
			mockNP.EXPECT().NodeAddresses(ctx, types.NodeName("node0")).Return(nil, nil)
			mockNP.EXPECT().InstanceType(ctx, types.NodeName("node0")).Return("Standard_D2_v3", nil)
			mockNP.EXPECT().GetZone(ctx, types.NodeName("node0")).Return(cloudprovider.Zone{Region: "eastus", FailureDomain: "1"}, nil)
			mockNP.EXPECT().GetPlatformSubFaultDomain(ctx).Return("2", nil)
			mockNP.EXPECT().GetMetadataLabels(ctx).Return(tc.labels, tc.err)
			cnc := &CloudNodeController{nodeProvider: mockNP}
			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node0",
					Labels: map[string]string{
						"test.example/unrelated":                 "preserved",
						consts.LabelPlatformInterconnectGroup:    "existing-group",
						consts.LabelPlatformInterconnectSubgroup: "existing-subgroup",
					},
				},
				Spec: v1.NodeSpec{ProviderID: "node0"},
			}
			modifiers, err := cnc.getNodeModifiersFromCloudProvider(ctx, node)
			if tc.wantError != "" {
				assert.ErrorContains(t, err, tc.wantError)
				if tc.err != nil {
					assert.ErrorIs(t, err, tc.err)
				}
				assert.Nil(t, modifiers)
				assert.Equal(t, "existing-group", node.Labels[consts.LabelPlatformInterconnectGroup])
				assert.Equal(t, "existing-subgroup", node.Labels[consts.LabelPlatformInterconnectSubgroup])
				return
			}
			assert.NoError(t, err)
			for _, modifier := range modifiers {
				modifier(node)
			}
			assert.Equal(t, tc.want, node.Labels[consts.LabelPlatformInterconnectGroup])
			assert.Equal(t, tc.wantSubgroup, node.Labels[consts.LabelPlatformInterconnectSubgroup])
			assert.Equal(t, "preserved", node.Labels["test.example/unrelated"])
			assert.Equal(t, "Standard_D2_v3", node.Labels[v1.LabelInstanceTypeStable])
			assert.Equal(t, "eastus", node.Labels[v1.LabelZoneRegionStable])
			assert.Equal(t, "1", node.Labels[v1.LabelZoneFailureDomainStable])
			assert.Equal(t, "2", node.Labels[consts.LabelPlatformSubFaultDomain])
		})
	}
}

func TestMetadataLabelsInitializedNodeNoRefresh(t *testing.T) {
	for _, tc := range []struct {
		name       string
		labels     map[string]string
		staleEvent bool
	}{
		{name: "no labels"},
		{name: "legacy group is not backfilled", labels: map[string]string{consts.LabelPlatformInterconnectGroup: "legacy"}},
		{name: "existing labels are not refreshed", labels: map[string]string{
			consts.LabelPlatformInterconnectGroup:    "old-group",
			consts.LabelPlatformInterconnectSubgroup: "old-subgroup",
		}},
		{name: "stale tainted event does not reinitialize", labels: map[string]string{consts.LabelPlatformInterconnectGroup: "legacy"}, staleEvent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node0", Labels: tc.labels},
				Spec:       v1.NodeSpec{ProviderID: "node0"},
			}
			client := fake.NewSimpleClientset(node)
			nodeInformer := informers.NewSharedInformerFactory(client, 0).Core().V1().Nodes()
			assert.NoError(t, nodeInformer.Informer().GetIndexer().Add(node.DeepCopy()))
			mockNP := mocknodeprovider.NewMockNodeProvider(gomock.NewController(t))
			mockNP.EXPECT().NodeAddresses(ctx, types.NodeName("node0")).Return(nil, nil).Times(2)
			// No metadata-label calls are expected from either event or periodic paths.
			cnc := &CloudNodeController{
				nodeName: "node0", nodeInformer: nodeInformer, kubeClient: client,
				nodeProvider: mockNP, labelReconcileInfo: betaTopologyLabels,
			}
			eventNode := node.DeepCopy()
			if tc.staleEvent {
				eventNode.Spec.Taints = []v1.Taint{{
					Key: cloudproviderapi.TaintExternalCloudProvider, Value: "true", Effect: v1.TaintEffectNoSchedule,
				}}
			}
			for range 2 {
				assert.NoError(t, cnc.handleNodeEvent(ctx, eventNode))
				cnc.UpdateNodeStatus(ctx)
			}
			for _, action := range client.Actions() {
				assert.Equal(t, "get", action.GetVerb(), "already initialized nodes must not be relabeled")
			}
			persisted, err := client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Equal(t, node, persisted)
			cached, err := nodeInformer.Lister().Get(node.Name)
			assert.NoError(t, err)
			assert.Equal(t, node, cached, "do not mutate the informer snapshot")
		})
	}
}

func TestMetadataLabelsInitializationErrorPreservesNode(t *testing.T) {
	providerErr := errors.New("metadata evaluation error")
	for _, tc := range []struct {
		name      string
		labels    map[string]string
		err       error
		wantError string
	}{
		{name: "provider error with partial labels", labels: map[string]string{consts.LabelPlatformInterconnectGroup: "new-group"}, err: providerErr, wantError: "get metadata labels: metadata evaluation error"},
		{name: "unmanaged key mixed with managed labels", labels: map[string]string{
			consts.LabelPlatformInterconnectGroup:    "new-group",
			consts.LabelPlatformInterconnectSubgroup: "new-subgroup",
			v1.LabelZoneRegionStable:                 "unexpected",
		}, wantError: "unexpected metadata label key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node0", Labels: map[string]string{
					consts.LabelPlatformInterconnectGroup:    "existing-group",
					consts.LabelPlatformInterconnectSubgroup: "existing-subgroup",
				}},
				Spec: v1.NodeSpec{ProviderID: "node0", Taints: []v1.Taint{{
					Key: cloudproviderapi.TaintExternalCloudProvider, Value: "true", Effect: v1.TaintEffectNoSchedule,
				}}},
			}
			client := fake.NewSimpleClientset(node)
			mockNP := mocknodeprovider.NewMockNodeProvider(gomock.NewController(t))
			mockNP.EXPECT().NodeAddresses(ctx, types.NodeName("node0")).Return(nil, nil)
			mockNP.EXPECT().InstanceType(ctx, types.NodeName("node0")).Return("Standard_D2_v3", nil)
			mockNP.EXPECT().GetZone(ctx, types.NodeName("node0")).Return(cloudprovider.Zone{Region: "eastus"}, nil)
			mockNP.EXPECT().GetPlatformSubFaultDomain(ctx).Return("2", nil)
			mockNP.EXPECT().GetMetadataLabels(ctx).Return(tc.labels, tc.err)
			cnc := &CloudNodeController{kubeClient: client, nodeProvider: mockNP}
			err := cnc.handleNodeEvent(ctx, node.DeepCopy())
			assert.ErrorContains(t, err, tc.wantError)
			if tc.err != nil {
				assert.ErrorIs(t, err, tc.err)
			}
			for _, action := range client.Actions() {
				assert.Equal(t, "get", action.GetVerb(), "no labels or taints may be written on metadata errors")
			}
			persisted, err := client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Equal(t, node, persisted)
		})
	}
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
	mockNP.EXPECT().GetPlatformSubFaultDomain(ctx).Return("1", nil)
	mockNP.EXPECT().GetMetadataLabels(ctx).Return(map[string]string{
		consts.LabelPlatformInterconnectGroup:    "group-456",
		consts.LabelPlatformInterconnectSubgroup: "subgroup-456",
	}, nil)

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

	err := cloudNodeController.handleNodeEvent(ctx, fnh.Existing[0])
	assert.NoError(t, err)

	assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
	assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
	assert.Equal(t, 0, len(fnh.UpdatedNodes[0].Spec.Taints), "Node Taint was not removed after cloud init")
	assert.Equal(t, 2, len(fnh.UpdatedNodes[0].Status.Conditions), "Node Contions was not updated")
	assert.Equal(t, "NetworkUnavailable", string(fnh.UpdatedNodes[0].Status.Conditions[0].Type), "Node Condition NetworkUnavailable was not updated")
	assert.Equal(t, "1", fnh.UpdatedNodes[0].Labels[consts.LabelPlatformSubFaultDomain])
	assert.Equal(t, "group-456", fnh.UpdatedNodes[0].Labels[consts.LabelPlatformInterconnectGroup])
	assert.Equal(t, "subgroup-456", fnh.UpdatedNodes[0].Labels[consts.LabelPlatformInterconnectSubgroup])
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

	err := cloudNodeController.handleNodeEvent(context.TODO(), fnh.Existing[0])
	assert.NoError(t, err)
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
		mockNP.EXPECT().GetPlatformSubFaultDomain(ctx).Return("", nil)
		mockNP.EXPECT().GetMetadataLabels(ctx).Return(nil, nil)

		eventBroadcaster := record.NewBroadcaster()
		cloudNodeController := &CloudNodeController{
			kubeClient:   fnh,
			nodeName:     "node0",
			nodeProvider: mockNP,
			nodeInformer: factory.Core().V1().Nodes(),
			recorder:     eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cloud-node-controller"}),
		}
		eventBroadcaster.StartLogging(klog.Infof)

		err := cloudNodeController.handleNodeEvent(context.TODO(), fnh.Existing[0])
		assert.NoError(t, err)

		assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
		assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
		assert.Equal(t, 3, len(fnh.UpdatedNodes[0].Labels),
			"Node label for Region and Zone were not set")
		assert.Equal(t, "eastus", fnh.UpdatedNodes[0].Labels[v1.LabelZoneRegionStable],
			"Node Region not correctly updated")
		assert.Equal(t, "eastus-1", fnh.UpdatedNodes[0].Labels[v1.LabelZoneFailureDomainStable],
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
		mockNP.EXPECT().GetPlatformSubFaultDomain(ctx).Return("", nil)
		mockNP.EXPECT().GetMetadataLabels(ctx).Return(nil, nil)

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

		err := cloudNodeController.handleNodeEvent(context.TODO(), fnh.Existing[0])
		assert.NoError(t, err)

		assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
		assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
		assert.Equal(t, 6, len(fnh.UpdatedNodes[0].Labels),
			"Node label for Region and Zone were not set")
		assert.Equal(t, "eastus", fnh.UpdatedNodes[0].Labels[v1.LabelZoneRegionStable],
			"Node Region not correctly updated")
		assert.Equal(t, "eastus-1", fnh.UpdatedNodes[0].Labels[v1.LabelZoneFailureDomainStable],
			"Node FailureDomain not correctly updated")
		assert.Equal(t, "eastus", fnh.UpdatedNodes[0].Labels[v1.LabelZoneRegion],
			"Node Region not correctly updated")
		assert.Equal(t, "eastus-1", fnh.UpdatedNodes[0].Labels[v1.LabelZoneFailureDomain],
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
	mockNP.EXPECT().GetPlatformSubFaultDomain(gomock.Any()).Return("", nil)
	mockNP.EXPECT().GetMetadataLabels(gomock.Any()).Return(nil, nil)

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

	err := cloudNodeController.handleNodeEvent(ctx, fnh.Existing[0])
	assert.NoError(t, err)
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
	mockNP.EXPECT().GetPlatformSubFaultDomain(ctx).Return("", nil)
	mockNP.EXPECT().GetMetadataLabels(ctx).Return(nil, nil)

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

	err := cloudNodeController.handleNodeEvent(context.TODO(), fnh.Existing[0])
	assert.NoError(t, err)
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
				// Test using the beta topology labels.
				labelReconcileInfo: betaTopologyLabels,
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
	mockNP.EXPECT().GetPlatformSubFaultDomain(ctx).Return("", nil).AnyTimes()
	mockNP.EXPECT().GetMetadataLabels(ctx).Return(nil, nil).AnyTimes()

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

	err := cloudNodeController.handleNodeEvent(context.TODO(), fnh.Existing[0])
	assert.NoError(t, err)

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
	mockNP.EXPECT().GetPlatformSubFaultDomain(ctx).Return("", nil).AnyTimes()
	mockNP.EXPECT().GetMetadataLabels(ctx).Return(nil, nil).AnyTimes()

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

	err := cloudNodeController.handleNodeEvent(context.TODO(), fnh.Existing[0])
	assert.NoError(t, err)

	assert.Equal(t, 1, len(fnh.UpdatedNodes), "Node was not updated")
	assert.Equal(t, "node0", fnh.UpdatedNodes[0].Name, "Node was not updated")
	// CCM node controller should not overwrite provider if it's already set
	assert.Equal(t, "test-provider-id", fnh.UpdatedNodes[0].Spec.ProviderID, "Node ProviderID not set correctly")
}

// This test checks that a node manager should retry 20 times when failing to get providerID and then return an error
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

	// Expect handleNodeEvent() to return an error when providerID is not set properly
	err := cloudNodeController.handleNodeEvent(context.TODO(), fnh.Existing[0])
	assert.Error(t, err, "handleNodeEvent() should return an error when providerID not found")
	assert.Contains(t, err.Error(), "failed to set node provider id", "Error should mention failed to set node provider id")

	// Node update should fail
	assert.Equal(t, 0, len(fnh.UpdatedNodes), "Node was updated (unexpected)")
}

func Test_ensureNodeProvidedIPsExists(t *testing.T) {
	testcases := []struct {
		name                    string
		node                    *v1.Node
		nodeAddresses           []v1.NodeAddress
		expectedExistingNodeIPs []v1.NodeAddress
		nodeIPsExists           bool
	}{
		{
			name: "return true when there's provide node ip address",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node0",
					Labels:      map[string]string{},
					Annotations: map[string]string{},
				},
			},
			nodeAddresses: []v1.NodeAddress{
				{
					Address: "10.0.0.1",
				},
			},
			expectedExistingNodeIPs: []v1.NodeAddress{},
			nodeIPsExists:           true,
		},
		{
			name: "return true when all provided IPv4 IP address are found",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node0",
					Labels: map[string]string{},
					Annotations: map[string]string{
						cloudproviderapi.AnnotationAlphaProvidedIPAddr: "10.0.0.1",
					},
				},
			},
			nodeAddresses: []v1.NodeAddress{
				{
					Address: "10.0.0.1",
				},
			},
			expectedExistingNodeIPs: []v1.NodeAddress{{Address: "10.0.0.1"}},
			nodeIPsExists:           true,
		},
		{
			name: "return true when all provided dual stack IP addresses are found",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node0",
					Labels: map[string]string{},
					Annotations: map[string]string{
						cloudproviderapi.AnnotationAlphaProvidedIPAddr: "10.0.0.1,fd47:c915:f8a8:e63d::5",
					},
				},
			},
			nodeAddresses: []v1.NodeAddress{
				{
					Address: "10.0.0.1",
				},
				{
					Address: "fd47:c915:f8a8:e63d::5",
				},
			},
			expectedExistingNodeIPs: []v1.NodeAddress{
				{
					Address: "10.0.0.1",
				},
				{
					Address: "fd47:c915:f8a8:e63d::5",
				},
			},
			nodeIPsExists: true,
		},
		{
			name: "return true when all provided dual stack IP addresses are found but joined with extra space",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node0",
					Labels: map[string]string{},
					Annotations: map[string]string{
						cloudproviderapi.AnnotationAlphaProvidedIPAddr: "10.0.0.1, fd47:c915:f8a8:e63d::5",
					},
				},
			},
			nodeAddresses: []v1.NodeAddress{
				{
					Address: "10.0.0.1",
				},
				{
					Address: "fd47:c915:f8a8:e63d::5",
				},
			},
			expectedExistingNodeIPs: []v1.NodeAddress{
				{
					Address: "10.0.0.1",
				},
				{
					Address: "fd47:c915:f8a8:e63d::5",
				},
			},
			nodeIPsExists: true,
		},
		{
			name: "return false when not all ip addresses are found for provided dual stack IP addresses",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node0",
					Labels: map[string]string{},
					Annotations: map[string]string{
						cloudproviderapi.AnnotationAlphaProvidedIPAddr: "10.0.0.1,fd47:c915:f8a8:e63d::5",
					},
				},
			},
			nodeAddresses: []v1.NodeAddress{
				{
					Address: "10.0.0.1",
				},
			},
			expectedExistingNodeIPs: []v1.NodeAddress{
				{
					Address: "10.0.0.1",
				},
			},
			nodeIPsExists: false,
		},
		{
			name: "return false when wrong ip addresses are provided for provided dual stack IP addresses",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node0",
					Labels: map[string]string{},
					Annotations: map[string]string{
						cloudproviderapi.AnnotationAlphaProvidedIPAddr: "10.0.0.1,fd47:c915:f8a8:e63d::10",
					},
				},
			},
			nodeAddresses: []v1.NodeAddress{
				{
					Address: "10.0.0.1",
				},
				{
					Address: "fd47:c915:f8a8:e63d::5",
				},
			},
			expectedExistingNodeIPs: []v1.NodeAddress{
				{
					Address: "10.0.0.1",
				},
			},
			nodeIPsExists: false,
		},
	}

	for _, test := range testcases {
		t.Run(test.name, func(t *testing.T) {

			actualNodeIP, actualNodeIPsExists := ensureNodeProvidedIPsExists(test.node, test.nodeAddresses)

			if !reflect.DeepEqual(actualNodeIP, test.expectedExistingNodeIPs) {
				t.Logf("Actual existing node IPs: %v", actualNodeIP)
				t.Logf("Expected existing  node IPs: %v", test.expectedExistingNodeIPs)
				t.Errorf("Actual existing  node IP does not match expected existing  node IP")
			}
			if actualNodeIPsExists != test.nodeIPsExists {
				t.Errorf("all node ip addresses exist result mismatch, got: %t, wanted: %t", actualNodeIPsExists, test.nodeIPsExists)
			}
		})
	}
}
