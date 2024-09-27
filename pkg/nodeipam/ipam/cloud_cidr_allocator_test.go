/*
Copyright 2021 The Kubernetes Authors.

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

package ipam

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	azureprovider "sigs.k8s.io/cloud-provider-azure/pkg/provider"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/controller/testutil"
)

func hasNodeInProcessing(ca *cloudCIDRAllocator, name string) bool {
	ca.lock.Lock()
	defer ca.lock.Unlock()

	_, found := ca.nodesInProcessing[name]
	return found
}

func TestBoundedRetries(_ *testing.T) {
	clientSet := fake.NewSimpleClientset()
	updateChan := make(chan nodeReservedCIDRs, 1) // need to buffer as we are using only on go routine
	sharedInfomer := informers.NewSharedInformerFactory(clientSet, 1*time.Hour)
	ca := &cloudCIDRAllocator{
		client:            clientSet,
		nodeUpdateChannel: updateChan,
		nodeLister:        sharedInfomer.Core().V1().Nodes().Lister(),
		nodesSynced:       sharedInfomer.Core().V1().Nodes().Informer().HasSynced,
		nodesInProcessing: map[string]struct{}{},
	}
	go ca.worker(context.Background())
	nodeName := "testNode"
	_ = ca.AllocateOrOccupyCIDR(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
	})
	for hasNodeInProcessing(ca, nodeName) {
		// wait for node to finish processing (should terminate and not time out)
	}
}

func TestNewCloudCIDRAllocator(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clientSet := fake.NewSimpleClientset()
	fakeNodeHandler := &testutil.FakeNodeHandler{
		Existing: []*v1.Node{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node0",
				},
			},
		},
		Clientset: clientSet,
	}
	cloud := azureprovider.GetTestCloud(ctrl)
	nodeInformer := getFakeNodeInformer(fakeNodeHandler)
	allocatorParams := CIDRAllocatorParams{
		ClusterCIDRs: func() []*net.IPNet {
			_, clusterCIDRv4, _ := net.ParseCIDR("10.10.0.0/24")
			return []*net.IPNet{clusterCIDRv4}
		}(),
		ServiceCIDR: func() *net.IPNet {
			_, clusterCIDRv4, _ := net.ParseCIDR("10.10.0.0/25")
			return clusterCIDRv4
		}(),
		SecondaryServiceCIDR: func() *net.IPNet {
			_, clusterCIDRv4, _ := net.ParseCIDR("10.10.1.0/25")
			return clusterCIDRv4
		}(),
	}

	ca, err := NewCloudCIDRAllocator(clientSet, cloud, nodeInformer, allocatorParams, nil)
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	_, ok := ca.(*cloudCIDRAllocator)
	if !ok {
		t.Fatalf("expected a cloud allocator")
	}
}

func TestUpdateMaxSubnetSizes(t *testing.T) {
	for _, tc := range []struct {
		description                                    string
		clusterCIDRs                                   []*net.IPNet
		nodeNameSubnetMaskSizesMap                     map[string][]int
		maxSubnetMaskSizes, expectedMaxSubnetMaskSizes []int
	}{
		{
			description: "updateMaxSubnetSizes should calculate the correct max sizes",
			clusterCIDRs: func() []*net.IPNet {
				_, clusterCIDR, _ := net.ParseCIDR("10.240.0.0/16")
				return []*net.IPNet{clusterCIDR}
			}(),
			nodeNameSubnetMaskSizesMap: map[string][]int{"node0": {24}, "node1": {26}},
			maxSubnetMaskSizes:         []int{27},
			expectedMaxSubnetMaskSizes: []int{26},
		},
		{
			description: "updateMaxSubnetSizes should work with the dual stack cidrs",
			clusterCIDRs: func() []*net.IPNet {
				_, cidrIPV4, _ := net.ParseCIDR("10.240.0.0/16")
				_, cidrIPV6, _ := net.ParseCIDR("beef::/48")
				return []*net.IPNet{cidrIPV4, cidrIPV6}
			}(),
			nodeNameSubnetMaskSizesMap: map[string][]int{"node0": {26, 64}, "node1": {24, 66}},
			maxSubnetMaskSizes:         []int{27, 67},
			expectedMaxSubnetMaskSizes: []int{26, 66},
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			ca := cloudCIDRAllocator{
				clusterCIDRs:               tc.clusterCIDRs,
				nodeNameSubnetMaskSizesMap: tc.nodeNameSubnetMaskSizesMap,
				maxSubnetMaskSizes:         tc.maxSubnetMaskSizes,
			}
			ca.updateMaxSubnetMaskSizes()
			assert.Equal(t, tc.expectedMaxSubnetMaskSizes, ca.maxSubnetMaskSizes)
		})
	}
}

func TestUpdateNodeSubnetMaskSizes(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description, providerID            string
		tags                               map[string]*string
		expectedNodeNameSubnetMaskSizesMap map[string][]int
		expectedErr                        error
	}{
		{
			description: "updateNodeSubnetMaskSizes should put the correct mask sizes on the map",
			providerID:  "azure:///subscriptions/sub/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("25"),
				consts.VMSetCIDRIPV6TagKey: ptr.To("65"),
			},
			expectedNodeNameSubnetMaskSizesMap: map[string][]int{"vmss-0": {25, 65}},
		},
		{
			description: "updateNodeSubnetMaskSizes should put the default mask sizes on the map if the providerID is invalid",
			providerID:  "invalid",
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("24"),
				consts.VMSetCIDRIPV6TagKey: ptr.To("64"),
			},
			expectedNodeNameSubnetMaskSizesMap: map[string][]int{"vmss-0": {24, 64}},
		},
		{
			description: "updateNodeSubnetMaskSizes should report an error if the ipv4 mask is smaller than the cluster mask",
			providerID:  "azure:///subscriptions/sub/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("15"),
				consts.VMSetCIDRIPV6TagKey: ptr.To("65"),
			},
			expectedNodeNameSubnetMaskSizesMap: map[string][]int{},
			expectedErr:                        fmt.Errorf("updateNodeSubnetMaskSizes: invalid ipv4 mask size %d of node %s because it is out of the range of the cluster CIDR with the mask size %d", 15, "vmss-0", 16),
		},
		{
			description: "updateNodeSubnetMaskSizes should report an error if the ipv6 mask is smaller than the cluster mask",
			providerID:  "azure:///subscriptions/sub/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("25"),
				consts.VMSetCIDRIPV6TagKey: ptr.To("45"),
			},
			expectedNodeNameSubnetMaskSizesMap: map[string][]int{},
			expectedErr:                        fmt.Errorf("updateNodeSubnetMaskSizes: invalid ipv6 mask size %d of node %s because it is out of the range of the cluster CIDR with the mask size %d", 45, "vmss-0", 48),
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			cloud := azureprovider.GetTestCloud(ctrl)
			ss, err := azureprovider.NewTestScaleSet(ctrl)
			assert.NoError(t, err)

			expectedVMSS := compute.VirtualMachineScaleSet{
				Name: ptr.To("vmss"),
				Tags: tc.tags,
				VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
					OrchestrationMode: compute.Uniform,
				},
			}
			mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), cloud.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).MaxTimes(1)
			cloud.VMSet = ss
			mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
			mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

			clusterCIDRs := func() []*net.IPNet {
				_, cidrIPV4, _ := net.ParseCIDR("10.240.0.0/16")
				_, cidrIPV6, _ := net.ParseCIDR("beef::/48")
				return []*net.IPNet{cidrIPV4, cidrIPV6}
			}()
			ca := cloudCIDRAllocator{
				cloud:                      cloud,
				clusterCIDRs:               clusterCIDRs,
				nodeNameSubnetMaskSizesMap: make(map[string][]int),
			}

			err = ca.updateNodeSubnetMaskSizes("vmss-0", tc.providerID)
			assert.Equal(t, tc.expectedErr, err)
			assert.Equal(t, tc.expectedNodeNameSubnetMaskSizesMap, ca.nodeNameSubnetMaskSizesMap)
		})
	}
}
