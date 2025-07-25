/*
Copyright 2022 The Kubernetes Authors.

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

package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/samber/lo"

	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/interfaceclient/mock_interfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient/mock_virtualmachinescalesetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

var (
	testVmssFlexID1 = "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1"
	testNodeName1   = "vmssflex1000001"
	testNode1       = &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeName1,
		},
	}

	testVmssFlexID2 = "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex2"
	testNodeName2   = "vmssflex2000001"
	testNode2       = &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeName2,
		},
	}

	nonExistingNodeName = "NonExistingNodeName"

	testIPConfigurationID = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm1-nic/ipConfigurations/pipConfig"
	testBackendPoolID0    = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0"

	testBackendPools = []*armnetwork.BackendAddressPool{
		{
			ID: ptr.To(testBackendPoolID0),
			Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
				BackendIPConfigurations: []*armnetwork.InterfaceIPConfiguration{
					{
						ID: ptr.To(testIPConfigurationID),
					},
				},
			},
		},
	}

	testNic1 = generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateSucceeded), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1")

	testNic2 = generateTestNic("testvm2-nic", true, to.Ptr(armnetwork.ProvisioningStateSucceeded), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm2")
)

func generateTestNic(nicName string, isIPConfigurationsNil bool, provisioningState *armnetwork.ProvisioningState, vmID string) *armnetwork.Interface {
	result := &armnetwork.Interface{
		ID:   ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/" + nicName),
		Name: ptr.To(nicName),
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
				{
					Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
						Primary:          ptr.To(true),
						PrivateIPAddress: ptr.To(nicName + "testPrivateIP"),
						LoadBalancerBackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								ID: ptr.To(testBackendPoolID0),
							},
						},
					},
				},
			},
			ProvisioningState: provisioningState,
			VirtualMachine: &armnetwork.SubResource{
				ID: ptr.To(vmID),
			},
		},
	}
	if isIPConfigurationsNil {
		result.Properties.IPConfigurations = nil
	}
	return result
}

func TestGetNodeVMSetNameVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description       string
		expectedVMSetName string
		expectedErr       error
	}{
		{
			description:       "GetNodeVMSetName should return the correct VMSetName of the node",
			expectedVMSetName: "vmssflex1",
			expectedErr:       nil,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")
		fs.vmssFlexVMNameToVmssID.Store(testNodeName1, testVmssFlexID1)
		fs.vmssFlexVMNameToVmssID.Store(testNodeName2, testVmssFlexID2)

		vmSetName, err := fs.GetNodeVMSetName(context.TODO(), testNode1)
		assert.Equal(t, tc.expectedVMSetName, vmSetName, tc.description)
		assert.Equal(t, tc.expectedErr, err, tc.description)
	}

}

func TestGetAgentPoolVMSetNamesVmssFlex(t *testing.T) {

	testCases := []struct {
		description                 string
		nodes                       []*v1.Node
		expectedAgentPoolVMSetNames []string
		expectedErr                 error
	}{
		{
			description:                 "GetNodeVMSetName should return the correct VMSetName of the node",
			nodes:                       []*v1.Node{testNode1, testNode2},
			expectedAgentPoolVMSetNames: []string{"vmssflex1", "vmssflex2"},
			expectedErr:                 nil,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			fs, err := NewTestFlexScaleSet(ctrl)
			assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")
			fs.vmssFlexVMNameToVmssID.Store(testNodeName1, testVmssFlexID1)
			fs.vmssFlexVMNameToVmssID.Store(testNodeName2, testVmssFlexID2)

			agentPoolVMSetNames, err := fs.GetAgentPoolVMSetNames(context.TODO(), tc.nodes)
			assert.Equal(t, tc.expectedAgentPoolVMSetNames, lo.FromSlicePtr(agentPoolVMSetNames), tc.description)
			assert.Equal(t, tc.expectedErr, err, tc.description)
		})
	}
}

func TestGetVMSetNamesVmssFlex(t *testing.T) {
	testCases := []struct {
		description        string
		service            *v1.Service
		nodes              []*v1.Node
		useSingleSLB       bool
		expectedVMSetNames []string
		expectedErr        error
	}{
		{
			description:        "GetVMSetNames should return the primary vm set name if the service has no mode annotation",
			service:            &v1.Service{},
			expectedVMSetNames: []string{"vmss"},
		},
		{
			description: "GetVMSetNames should return the primary vm set name when using the single SLB",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: consts.ServiceAnnotationLoadBalancerAutoModeValue}},
			},
			useSingleSLB:       true,
			expectedVMSetNames: []string{"vmss"},
		},
		{
			description: "GetVMSetNames should return all scale sets if the service has auto mode annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: consts.ServiceAnnotationLoadBalancerAutoModeValue}},
			},
			nodes:              []*v1.Node{testNode1, testNode2},
			expectedVMSetNames: []string{"vmssflex1", "vmssflex2"},
		},
		{
			description: "GetVMSetNames should report the error if there's no such vmss",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: "vmssflex3"}},
			},
			nodes:       []*v1.Node{testNode1, testNode2},
			expectedErr: fmt.Errorf("scale set (vmssflex3) - not found"),
		},
		{
			description: "GetVMSetNames should return the correct vmss names",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: "vmssflex1"}},
			},
			nodes:              []*v1.Node{testNode1, testNode2},
			expectedVMSetNames: []string{"vmssflex1"},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			fs, err := NewTestFlexScaleSet(ctrl)
			assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")
			fs.vmssFlexVMNameToVmssID.Store(testNodeName1, testVmssFlexID1)
			fs.vmssFlexVMNameToVmssID.Store(testNodeName2, testVmssFlexID2)

			if tc.useSingleSLB {
				fs.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			}

			vmSetNames, err := fs.GetVMSetNames(context.TODO(), tc.service, tc.nodes)
			if len(tc.expectedVMSetNames) == 0 {
				assert.Nil(t, vmSetNames, tc.description)
			} else {
				assert.Equal(t, tc.expectedVMSetNames, lo.FromSlicePtr(vmSetNames), tc.description)
			}
			assert.Equal(t, tc.expectedErr, err, tc.description)
		})
	}
}

func TestGetNodeNameByProviderIDVmssFlex(t *testing.T) {

	testCases := []struct {
		description                    string
		providerID                     string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		expectedNodeName               types.NodeName
		expectedErr                    error
	}{
		{
			description:                    "GetNodeNameByProviderID should return the correct nodeName by VMSS Flex VM ID",
			providerID:                     "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedNodeName:               types.NodeName("vmssflex1000001"),
			expectedErr:                    nil,
		},
		{
			description:                    "GetNodeNameByProviderID should throw error of instance not found if the vm is deleted",
			providerID:                     "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/" + nonExistingNodeName,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedNodeName:               "",
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			fs, err := NewTestFlexScaleSet(ctrl)
			assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

			mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

			mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
			mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
			mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

			nodeName, err := fs.GetNodeNameByProviderID(context.TODO(), tc.providerID)
			assert.Equal(t, tc.expectedNodeName, nodeName)
			assert.Equal(t, tc.expectedErr, err)
		})
	}

}

func TestGetInstanceIDByNodeNameVmssFlex(t *testing.T) {

	testCases := []struct {
		description                    string
		nodeName                       string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		expectedInstanceID             string
		expectedErr                    error
	}{
		{
			description:                    "GetInstanceIDByNodeName should return the correct InstanceID by nodeName",
			nodeName:                       testNodeName1,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedInstanceID:             "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1",
			expectedErr:                    nil,
		},
		{
			description:                    "GetNodeNameByProviderID should throw error of instance not found if the vm is deleted",
			nodeName:                       "nonExistingNodeName",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedInstanceID:             "",
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			fs, err := NewTestFlexScaleSet(ctrl)
			assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

			mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

			mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
			mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
			mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

			instanceID, err := fs.GetInstanceIDByNodeName(context.Background(), tc.nodeName)
			assert.Equal(t, tc.expectedInstanceID, instanceID)
			assert.Equal(t, tc.expectedErr, err)
		})
	}
}

func TestGetInstanceTypeByNodeNameVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		expectedInstanceType           string
		expectedErr                    error
	}{
		{
			description:                    "GetInstanceIDByNodeName should return the correct InstanceID by nodeName",
			nodeName:                       testNodeName1,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedInstanceType:           "Standard_D2s_v3",
			expectedErr:                    nil,
		},
		{
			description:                    "GetNodeNameByProviderID should throw error of instance not found if the vm is deleted",
			nodeName:                       "nonExistingNodeName",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedInstanceType:           "",
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		instanceType, err := fs.GetInstanceTypeByNodeName(context.Background(), tc.nodeName)
		assert.Equal(t, tc.expectedInstanceType, instanceType)
		assert.Equal(t, tc.expectedErr, err)
	}
}

func TestGetZoneByNodeNameVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		expectedZone                   cloudprovider.Zone
		expectedErr                    error
	}{
		{
			description:                    "GetZoneByNodeName should return the correct zone by nodeName",
			nodeName:                       testNodeName1,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedZone: cloudprovider.Zone{
				FailureDomain: "eastus-1",
				Region:        "eastus",
			},
			expectedErr: nil,
		},
		{
			description:                    "GetZoneByNodeName should return Instance Not Found if the node cannot be found",
			nodeName:                       nonExistingNodeName,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedZone:                   cloudprovider.Zone{},
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
		{
			description:                    "GetZoneByNodeName should return the correct zone if zone is nil but fault domain is not nil",
			nodeName:                       "vmssflex1000002",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedZone: cloudprovider.Zone{
				FailureDomain: "1",
				Region:        "eastus",
			},
			expectedErr: nil,
		},
		{
			description:                    "GetZoneByNodeName should return the error if both zone and fault domain are nil",
			nodeName:                       "vmssflex1000003",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedZone:                   cloudprovider.Zone{},
			expectedErr:                    fmt.Errorf("failed to get zone info"),
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		zone, err := fs.GetZoneByNodeName(context.TODO(), tc.nodeName)
		assert.Equal(t, tc.expectedZone, zone)
		assert.Equal(t, tc.expectedErr, err)
	}

}

func TestGetProvisioningStateByNodeNameVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		expectedProvisioningState      string
		expectedErr                    error
	}{
		{
			description:                    "GetProvisioningStateByNodeName should return the correct ProvisioningState by nodeName",
			nodeName:                       testNodeName1,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedProvisioningState:      "Succeeded",
			expectedErr:                    nil,
		},
		{
			description:                    "GetProvisioningStateByNodeName should return Instance Not Found if the node cannot be found",
			nodeName:                       nonExistingNodeName,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedProvisioningState:      "",
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
		{
			description:                    "GetProvisioningStateByNodeName should return empty provisioning state if the provisioning state is nil",
			nodeName:                       "vmssflex1000003",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedProvisioningState:      "",
			expectedErr:                    nil,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		provisioningState, err := fs.GetProvisioningStateByNodeName(context.TODO(), tc.nodeName)
		assert.Equal(t, tc.expectedProvisioningState, provisioningState)
		assert.Equal(t, tc.expectedErr, err)
	}

}

func TestGetPowerStatusByNodeNameVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		expectedPowerStatus            string
		expectedErr                    error
	}{
		{
			description:                    "GetPowerStatusByNodeName should return the correct PowerState by nodeName",
			nodeName:                       testNodeName1,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedPowerStatus:            "running",
			expectedErr:                    nil,
		},
		{
			description:                    "GetPowerStatusByNodeName should return Instance Not Found if the node cannot be found",
			nodeName:                       nonExistingNodeName,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedPowerStatus:            "",
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
		{
			description:                    "GetPowerStatusByNodeName should return unknown if the node powerstate is nil",
			nodeName:                       "vmssflex1000003",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedPowerStatus:            "unknown",
			expectedErr:                    nil,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		powerStatus, err := fs.GetPowerStatusByNodeName(context.TODO(), tc.nodeName)
		assert.Equal(t, tc.expectedPowerStatus, powerStatus)
		assert.Equal(t, tc.expectedErr, err)
	}

}

func TestGetPrimaryInterfaceVmssFlex(t *testing.T) {

	testCases := []struct {
		description                    string
		nodeName                       string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		nic                            *armnetwork.Interface
		nicGetErr                      error
		expectedNeworkInterface        *armnetwork.Interface
		expectedErr                    error
	}{
		{
			description:                    "GetPrimaryInterface should return the correct Nic by nodeName",
			nodeName:                       testNodeName1,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            testNic1,
			nicGetErr:                      nil,
			expectedNeworkInterface:        testNic1,
			expectedErr:                    nil,
		},
		{
			description:                    "GetPrimaryInterface should return Instance Not Found if the node cannot be found",
			nodeName:                       nonExistingNodeName,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            &armnetwork.Interface{},
			nicGetErr:                      nil,
			expectedNeworkInterface:        &armnetwork.Interface{},
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
		{
			description:                    "GetPrimaryInterface should return Instance Not Found if the NIC cannot be found",
			nodeName:                       "vmssflex1000002",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            &armnetwork.Interface{},
			nicGetErr:                      &azcore.ResponseError{ErrorCode: "NIC not found"},
			expectedNeworkInterface:        &armnetwork.Interface{},
			expectedErr:                    fmt.Errorf("NIC not found"),
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			fs, err := NewTestFlexScaleSet(ctrl)
			assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

			mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

			mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
			mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
			mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

			mockInterfacesClient := fs.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
			mockInterfacesClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.nic, tc.nicGetErr).AnyTimes()

			nic, err := fs.GetPrimaryInterface(context.Background(), tc.nodeName)
			if tc.expectedErr != nil {
				assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
			} else if tc.expectedNeworkInterface == nil {
				assert.Nil(t, nic, tc.description)
			} else {
				assert.Equal(t, *tc.expectedNeworkInterface, *nic, tc.description)
			}

		})
	}
}

// TestGetPrimaryInterfaceVmssFlexRefactoring tests that VmssFlex GetPrimaryInterface uses ComputeClientFactory
// instead of NetworkClientFactory after the client factory refactoring
func TestGetPrimaryInterfaceVmssFlexRefactoring(t *testing.T) {
	testCases := []struct {
		description         string
		nodeName            string
		interfaceCallsCount int
		expectSuccess       bool
		expectedErr         error
	}{
		{
			description:         "VmssFlex GetPrimaryInterface should use ComputeClientFactory.GetInterfaceClient successfully",
			nodeName:            testNodeName1,
			interfaceCallsCount: 1,
			expectSuccess:       true,
		},
		{
			description:         "VmssFlex GetPrimaryInterface should handle ComputeClientFactory interface client errors",
			nodeName:            testNodeName1,
			interfaceCallsCount: 1,
			expectSuccess:       false,
			expectedErr:         fmt.Errorf("interface client error"),
		},
		{
			description:         "VmssFlex GetPrimaryInterface should call ComputeClientFactory interface client exactly once per request",
			nodeName:            testNodeName1,
			interfaceCallsCount: 1,
			expectSuccess:       true,
		},
	}

	for _, test := range testCases {
		test := test
		t.Run(test.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			fs, err := NewTestFlexScaleSet(ctrl)
			assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

			// Setup expected VMSS Flex
			mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

			// Setup expected VMs
			mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
			mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(testVMListWithoutInstanceView, nil).AnyTimes()
			mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(testVMListWithOnlyInstanceView, nil).AnyTimes()

			// CRITICAL: Test that ComputeClientFactory.GetInterfaceClient() is called exactly the expected number of times
			mockInterfaceClient := fs.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)

			if test.expectSuccess {
				mockInterfaceClient.EXPECT().Get(
					gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
				).Return(testNic1, nil).Times(test.interfaceCallsCount)
			} else {
				mockInterfaceClient.EXPECT().Get(
					gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
				).Return(nil, &azcore.ResponseError{ErrorCode: "interface client error"}).Times(test.interfaceCallsCount)
			}

			// CRITICAL: Verify that NetworkClientFactory.GetInterfaceClient() is NEVER called
			// This ensures the refactoring is working correctly for VmssFlex as well
			mockNetworkInterfaceClient := fs.NetworkClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
			mockNetworkInterfaceClient.EXPECT().Get(
				gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(),
			).Times(0) // Should NEVER be called after refactoring

			// Execute the test
			nic, err := fs.GetPrimaryInterface(context.Background(), test.nodeName)

			// Verify results
			if test.expectSuccess {
				assert.NoError(t, err, test.description)
				assert.NotNil(t, nic, test.description)
				assert.Equal(t, testNic1.Name, nic.Name, test.description)
			} else {
				assert.Error(t, err, test.description)
				if test.expectedErr != nil {
					assert.Contains(t, err.Error(), test.expectedErr.Error(), test.description)
				}
			}
		})
	}
}

func TestGetIPByNodeNameVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		nic                            *armnetwork.Interface
		nicGetErr                      error
		expectedPrivateIP              string
		expectedPublicIP               string
		expectedErr                    error
	}{
		{
			description:                    "GetIPByNodeName should return the correct IP by nodeName",
			nodeName:                       testNodeName1,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            testNic1,
			nicGetErr:                      nil,
			expectedPrivateIP:              "testvm1-nictestPrivateIP",
			expectedPublicIP:               "",
			expectedErr:                    nil,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		mockInterfacesClient := fs.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
		mockInterfacesClient.EXPECT().Get(gomock.Any(), gomock.Any(), "testvm1-nic", gomock.Any()).Return(tc.nic, tc.nicGetErr).AnyTimes()

		privateIP, publicIP, err := fs.GetIPByNodeName(context.Background(), tc.nodeName)
		assert.Equal(t, tc.expectedPrivateIP, privateIP)
		assert.Equal(t, tc.expectedPublicIP, publicIP)
		assert.Equal(t, tc.expectedErr, err)
	}

}

func TestGetPrivateIPsByNodeNameVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		nic                            *armnetwork.Interface
		nicGetErr                      error
		expectedPrivateIPs             []string
		expectedErr                    error
	}{
		{
			description:                    "GetPrivateIPsByNodeName should return the correct Private IPs by nodeName",
			nodeName:                       testNodeName1,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            testNic1,
			nicGetErr:                      nil,
			expectedPrivateIPs:             []string{"testvm1-nictestPrivateIP"},
			expectedErr:                    nil,
		},
		{
			description:                    "GetPrivateIPsByNodeName should return the correct Private IPs by nodeName",
			nodeName:                       "vmssflex1000002",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            testNic2,
			nicGetErr:                      nil,
			expectedPrivateIPs:             []string{},
			expectedErr:                    fmt.Errorf("nic.Properties.IPConfigurations for nic (nicname=testvm2-nic) is nil"),
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		mockInterfacesClient := fs.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
		mockInterfacesClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.nic, tc.nicGetErr).AnyTimes()

		ips, err := fs.GetPrivateIPsByNodeName(context.Background(), tc.nodeName)
		assert.Equal(t, tc.expectedPrivateIPs, ips)
		assert.Equal(t, tc.expectedErr, err)
	}

}

func TestGetNodeNameByIPConfigurationIDVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		ipConfigurationID              string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		nic                            *armnetwork.Interface
		expectedNodeName               string
		expectedVMSetName              string
		expectedErr                    error
	}{
		{
			description:                    "GetNodeNameByIPConfigurationID should return the correct nodeName by IPConfig",
			ipConfigurationID:              testIPConfigurationID,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateSucceeded), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1"),
			expectedNodeName:               "vmssflex1000001",
			expectedVMSetName:              "vmssflex1",
			expectedErr:                    nil,
		},
		{
			description:                    "GetNodeNameByIPConfigurationID should return error if the VM does not exist",
			ipConfigurationID:              fmt.Sprintf("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/%s-nic/ipConfigurations/pipConfig", nonExistingNodeName),
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateSucceeded), fmt.Sprintf("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/%s", nonExistingNodeName)),
			expectedNodeName:               "",
			expectedVMSetName:              "",
			expectedErr:                    fmt.Errorf("failed to map VM Name to NodeName: VM Name NonExistingNodeName: %w", cloudprovider.InstanceNotFound),
		},
		{
			description:                    "GetNodeNameByIPConfigurationID should return error if the ipConfigurationID is in wrong format",
			ipConfigurationID:              "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces//ipConfigurations/pipConfig",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedNodeName:               "",
			expectedVMSetName:              "",
			expectedErr:                    fmt.Errorf("failed to get resource group and name from ip config ID /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces//ipConfigurations/pipConfig: %w", errors.New("invalid ip config ID /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces//ipConfigurations/pipConfig")),
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		mockInterfacesClient := fs.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
		mockInterfacesClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.nic, nil).AnyTimes()

		nodeName, vmSetName, err := fs.GetNodeNameByIPConfigurationID(context.TODO(), tc.ipConfigurationID)
		assert.Equal(t, tc.expectedNodeName, nodeName)
		assert.Equal(t, tc.expectedVMSetName, vmSetName)
		assert.Equal(t, tc.expectedErr, err)
	}
}

func TestGetNodeCIDRMasksByProviderIDVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		providerID                     string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		tags                           map[string]*string
		expectedNodeMaskCIDRIPv4       int
		expectedNodeMaskCIDRIPv6       int
		expectedErr                    error
	}{
		{
			description:                    "GetNodeCIDRMasksByProviderID should return the GetNodeCIDRMasksByProviderID",
			providerID:                     "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedNodeMaskCIDRIPv4:       24,
			expectedNodeMaskCIDRIPv6:       64,
			expectedErr:                    nil,
		},
		{
			description:                    "GetNodeCIDRMasksByProviderID should return error if the node does not exist",
			providerID:                     "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/" + nonExistingNodeName,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedNodeMaskCIDRIPv4:       0,
			expectedNodeMaskCIDRIPv6:       0,
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
		{
			description:                    "GetNodeCIDRMasksByProviderID should return error if providerID is invalid",
			providerID:                     "azure:///subscriptions//resourceGroups//providers/Microsoft.Compute/virtualMachines",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedNodeMaskCIDRIPv4:       0,
			expectedNodeMaskCIDRIPv6:       0,
			expectedErr:                    fmt.Errorf("error splitting providerID"),
		},
		{
			description:                    "GetNodeCIDRMasksByProviderID should return the correct mask sizes even if some of the tags are not specified",
			providerID:                     "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("24"),
			},
			expectedNodeMaskCIDRIPv4: 24,
			expectedNodeMaskCIDRIPv6: 0,
			expectedErr:              nil,
		},
		{
			description:                    "GetNodeCIDRMasksByProviderID should not fail even if some of the tag is invalid",
			providerID:                     "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("abc"),
				consts.VMSetCIDRIPV6TagKey: ptr.To("64"),
			},
			expectedNodeMaskCIDRIPv4: 0,
			expectedNodeMaskCIDRIPv6: 64,
			expectedErr:              nil,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		if tc.tags != nil {
			testVmssFlexList[0].Tags = tc.tags
		}
		mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		nodeMaskCIDRIPv4, nodeMaskCIDRIPv6, err := fs.GetNodeCIDRMasksByProviderID(context.TODO(), tc.providerID)
		assert.Equal(t, tc.expectedNodeMaskCIDRIPv4, nodeMaskCIDRIPv4)
		assert.Equal(t, tc.expectedNodeMaskCIDRIPv6, nodeMaskCIDRIPv6)
		assert.Equal(t, tc.expectedErr, err)
	}

}

func TestEnsureHostInPoolVmssFlex(t *testing.T) {

	testCases := []struct {
		description                    string
		nodeName                       types.NodeName
		service                        *v1.Service
		vmSetNameOfLB                  string
		backendPoolID                  string
		isStandardLB                   bool
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		nic                            *armnetwork.Interface
		nicGetErr                      error
		nicPutErr                      error
		expectedNodeResourceGroup      string
		expectedVMSetName              string
		expectedNodeName               string
		expectedErr                    error
	}{
		{
			description:                    "EnsureHostInPool should add a new backend pool to the vm",
			nodeName:                       "vmssflex1000001",
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            testNic1,
			nicGetErr:                      nil,
			expectedNodeResourceGroup:      "rg",
			expectedVMSetName:              "vmssflex1",
			expectedNodeName:               "vmssflex1000001",
			expectedErr:                    nil,
		},
		{
			description:                    "EnsureHostInPool should return nil if the nodeName does not exist",
			nodeName:                       types.NodeName(nonExistingNodeName),
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            &armnetwork.Interface{},
			nicGetErr:                      nil,
			expectedNodeResourceGroup:      "",
			expectedVMSetName:              "",
			expectedNodeName:               "",
			expectedErr:                    nil,
		},
		{
			description:                    "EnsureHostInPool should skip the current node if the network configs of the VMSS VM is nil",
			nodeName:                       "vmssflex1000001",
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            testNic2,
			nicGetErr:                      nil,
			expectedNodeResourceGroup:      "",
			expectedVMSetName:              "",
			expectedNodeName:               "",
			expectedErr:                    fmt.Errorf("nic.Properties.IPConfigurations for nic (nicname=\"testvm2-nic\") is nil"),
		},
		{
			description:                    "EnsureHostInPool should skip the current node if failing to get the PrimaryInterface",
			nodeName:                       "vmssflex1000001",
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            &armnetwork.Interface{},
			nicGetErr:                      &azcore.ResponseError{ErrorCode: "failed to get nic for node: vmssflex1000001"},
			expectedNodeResourceGroup:      "",
			expectedVMSetName:              "",
			expectedNodeName:               "",
			expectedErr:                    fmt.Errorf("failed to get nic for node: vmssflex1000001"),
		},
		{
			description:                    "EnsureHostInPool should return error if the nic update fails",
			nodeName:                       "vmssflex1000001",
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateSucceeded), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1"),
			nicGetErr:                      nil,
			nicPutErr:                      &azcore.ResponseError{ErrorCode: "failed to update nic"},
			expectedNodeResourceGroup:      "",
			expectedVMSetName:              "",
			expectedNodeName:               "",
			expectedErr:                    fmt.Errorf("failed to update nic"),
		},
		{
			description:                    "EnsureHostInPool should skip the node if primary nic is in Failed state",
			nodeName:                       "vmssflex1000001",
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateFailed), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1"),
			nicGetErr:                      nil,
			nicPutErr:                      nil,
			expectedNodeResourceGroup:      "",
			expectedVMSetName:              "",
			expectedNodeName:               "",
			expectedErr:                    nil,
		},
		{
			description:                    "EnsureHostInPool should skip the current node if the backend pool has existed",
			nodeName:                       "vmssflex1000001",
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  testBackendPoolID0,
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            testNic1,
			nicGetErr:                      nil,
			expectedNodeResourceGroup:      "",
			expectedVMSetName:              "",
			expectedNodeName:               "",
			expectedErr:                    nil,
		},
		{
			description:                    "EnsureHostInPool should skip the current node if it has already been added to another LB",
			nodeName:                       "vmssflex1000001",
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb2-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            testNic1,
			nicGetErr:                      nil,
			expectedNodeResourceGroup:      "",
			expectedVMSetName:              "",
			expectedNodeName:               "",
			expectedErr:                    nil,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			fs, err := NewTestFlexScaleSet(ctrl)
			assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")
			if tc.isStandardLB {
				fs.Config.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			}

			mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

			mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
			mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
			mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

			mockInterfacesClient := fs.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
			mockInterfacesClient.EXPECT().Get(gomock.Any(), gomock.Any(), "testvm1-nic", gomock.Any()).Return(tc.nic, tc.nicGetErr).AnyTimes()
			mockInterfacesClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, tc.nicPutErr).AnyTimes()

			rg, vmSetName, nodeName, _, err := fs.EnsureHostInPool(context.Background(), tc.service, tc.nodeName, tc.backendPoolID, tc.vmSetNameOfLB)
			assert.Equal(t, tc.expectedNodeResourceGroup, rg, tc.description)
			assert.Equal(t, tc.expectedVMSetName, vmSetName, tc.description)
			assert.Equal(t, tc.expectedNodeName, nodeName, tc.description)
			if tc.expectedErr != nil {
				assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
			}
		})
	}

}

func TestEnsureVMSSFlexInPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodes                          []*v1.Node
		service                        *v1.Service
		vmSetNameOfLB                  string
		backendPoolID                  string
		isStandardLB                   bool
		isVMSSDeallocating             bool
		hasDefaultVMProfile            bool
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		vmssPutErr                     error
		expectedErr                    error
	}{
		{
			description: "ensureVMSSFlexInPool should add a new backend pool to the vmss",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmssflex1000001",
					},
				},
			},
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			hasDefaultVMProfile:            true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedErr:                    nil,
		},
		{
			description: "ensureVMSSFlexInPool should skip the node if it isn't managed by VMSS",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: nonExistingNodeName,
					},
				},
			},
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			hasDefaultVMProfile:            true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedErr:                    nil,
		},
		{
			description: "ensureVMSSFlexInPool should skip the node if the corresponding VMSS does not have default VM profile",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmssflex1000001",
					},
				},
			},
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			isVMSSDeallocating:             false,
			hasDefaultVMProfile:            false,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedErr:                    nil,
		},
		{
			description: "ensureVMSSFlexInPool should skip the node if the backendpool ID has been added already",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmssflex1000001",
					},
				},
			},
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  testBackendPoolID0,
			isStandardLB:                   true,
			hasDefaultVMProfile:            true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedErr:                    nil,
		},
		{
			description: "ensureVMSSFlexInPool ensureVMSSInPool should skip the node if the VMSS has been added to another LB's backendpool",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmssflex1000001",
					},
				},
			},
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb2-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			hasDefaultVMProfile:            true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedErr:                    nil,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")
		if tc.isStandardLB {
			fs.Config.LoadBalancerSKU = consts.LoadBalancerSKUStandard
		}

		testVmssFlex := genreteTestVmssFlex("vmssflex1", testVmssFlex1ID)

		if tc.isVMSSDeallocating {
			testVmssFlex.Properties.ProvisioningState = ptr.To(consts.ProvisionStateDeleting)
		}
		if !tc.hasDefaultVMProfile {
			testVmssFlex.Properties.VirtualMachineProfile = nil
		}
		expectedestVmssFlexList := []*armcompute.VirtualMachineScaleSet{testVmssFlex}

		mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(expectedestVmssFlexList, nil).AnyTimes()
		mockVMSSClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(testVmssFlex1, nil).AnyTimes()
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, tc.vmssPutErr).AnyTimes()

		mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		err = fs.ensureVMSSFlexInPool(context.TODO(), tc.service, tc.nodes, tc.backendPoolID, tc.vmSetNameOfLB)

		if tc.expectedErr != nil {
			assert.EqualError(t, err, tc.expectedErr.Error(), tc.description)
		}
	}

}

func TestEnsureHostsInPoolVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodes                          []*v1.Node
		service                        *v1.Service
		vmSetNameOfLB                  string
		backendPoolID                  string
		isStandardLB                   bool
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		nic                            *armnetwork.Interface
		nicGetErr                      error
		vmssPutErr                     error
		expectedErr                    error
	}{
		{
			description: "EnsureHostsInPool should add a new backend pool to the vm and vmss",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmssflex1000001",
					},
				},
			},
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            testNic1,
			nicGetErr:                      nil,
			expectedErr:                    nil,
		},
		{
			description: "EnsureHostsInPool should return error if basic load balancer is used",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmssflex1000001",
					},
				},
			},
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   false,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            testNic1,
			nicGetErr:                      nil,
			expectedErr:                    fmt.Errorf("ensureVMSSFlexInPool: VMSS Flex does not support Basic Load Balancer"),
		},
		{
			description: "EnsureHostsInPool should return error if vmss update fails",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmssflex1000001",
					},
				},
			},
			service:                        &v1.Service{},
			vmSetNameOfLB:                  "",
			backendPoolID:                  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            testNic1,
			nicGetErr:                      nil,
			vmssPutErr:                     &azcore.ResponseError{ErrorCode: "failed to update nic"},
			expectedErr:                    fmt.Errorf("failed to update nic"),
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")
		if tc.isStandardLB {
			fs.Config.LoadBalancerSKU = consts.LoadBalancerSKUStandard
		}

		mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]*armcompute.VirtualMachineScaleSet{genreteTestVmssFlex("vmssflex1", testVmssFlex1ID)}, nil).AnyTimes()
		mockVMSSClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(testVmssFlex1, nil).AnyTimes()
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, tc.vmssPutErr).AnyTimes()

		mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		mockInterfacesClient := fs.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
		mockInterfacesClient.EXPECT().Get(gomock.Any(), gomock.Any(), "testvm1-nic", gomock.Any()).Return(tc.nic, tc.nicGetErr).AnyTimes()
		mockInterfacesClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

		err = fs.EnsureHostsInPool(context.Background(), tc.service, tc.nodes, tc.backendPoolID, tc.vmSetNameOfLB)

		if tc.expectedErr != nil {
			assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
		}
	}

}

func TestEnsureBackendPoolDeletedFromVMSetsVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description          string
		vmssNamesMap         map[string]bool
		backendPoolID        string
		isVMSSDeallocating   bool
		hasDefaultVMProfile  bool
		isNicConfigEmpty     bool
		isIPConfigEmpty      bool
		vmssListCallingTimes int
		vmssPutErr           error
		expectedErr          error
	}{
		{
			description: "EnsureBackendPoolDeletedFromVMSets should remove a backend pool from the vmss",
			vmssNamesMap: map[string]bool{
				"vmssflex1": true,
			},
			backendPoolID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0",
			hasDefaultVMProfile:  true,
			vmssListCallingTimes: 2,
			expectedErr:          nil,
		},
		{
			description: "EnsureBackendPoolDeletedFromVMSets should return error if the vmss does not exist",
			vmssNamesMap: map[string]bool{
				"NonExistingVmssflex": true,
			},
			backendPoolID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0",
			hasDefaultVMProfile:  true,
			vmssListCallingTimes: 1,
			expectedErr:          cloudprovider.InstanceNotFound,
		},
		{
			description: "EnsureBackendPoolDeletedFromVMSets should skip the vmss if it is deallocating",
			vmssNamesMap: map[string]bool{
				"vmssflex1": true,
			},
			backendPoolID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0",
			isVMSSDeallocating:   true,
			hasDefaultVMProfile:  true,
			vmssListCallingTimes: 1,
			expectedErr:          nil,
		},
		{
			description: "EnsureBackendPoolDeletedFromVMSets should skip the vmss does not have default VM profile",
			vmssNamesMap: map[string]bool{
				"vmssflex1": true,
			},
			backendPoolID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0",
			isVMSSDeallocating:   false,
			hasDefaultVMProfile:  false,
			vmssListCallingTimes: 1,
			expectedErr:          nil,
		},
		{
			description: "EnsureBackendPoolDeletedFromVMSets should skip the vmss has empty nic config",
			vmssNamesMap: map[string]bool{
				"vmssflex1": true,
			},
			backendPoolID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0",
			isVMSSDeallocating:   false,
			hasDefaultVMProfile:  true,
			isNicConfigEmpty:     true,
			vmssListCallingTimes: 1,
			expectedErr:          fmt.Errorf("failed to find a primary network configuration for the VMSS VM or VMSS \"vmssflex1\""),
		},
		{
			description: "EnsureBackendPoolDeletedFromVMSets should skip the vmss has empty IP config",
			vmssNamesMap: map[string]bool{
				"vmssflex1": true,
			},
			backendPoolID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0",
			isVMSSDeallocating:   false,
			hasDefaultVMProfile:  true,
			isNicConfigEmpty:     false,
			isIPConfigEmpty:      true,
			vmssListCallingTimes: 1,
			expectedErr:          fmt.Errorf("failed to find a primary IP configuration (IPv6=false) for the VMSS VM or VMSS \"vmssflex1\""),
		},
		{
			description: "EnsureBackendPoolDeletedFromVMSets should skip the vmss if the backend pool is not in the vmss's backend pool list",
			vmssNamesMap: map[string]bool{
				"vmssflex1": true,
			},
			backendPoolID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-1",
			hasDefaultVMProfile:  true,
			vmssListCallingTimes: 1,
			expectedErr:          nil,
		},
		{
			description: "EnsureBackendPoolDeletedFromVMSets should skip the vmss update fails",
			vmssNamesMap: map[string]bool{
				"vmssflex1": true,
			},
			backendPoolID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0",
			hasDefaultVMProfile:  true,
			vmssPutErr:           &azcore.ResponseError{ErrorCode: "failed to update nic"},
			vmssListCallingTimes: 2,
			expectedErr:          fmt.Errorf("failed to update nic"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			fs, err := NewTestFlexScaleSet(ctrl)
			assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

			testVmssFlex := genreteTestVmssFlex("vmssflex1", testVmssFlex1ID)

			if tc.isVMSSDeallocating {
				testVmssFlex.Properties.ProvisioningState = ptr.To(consts.ProvisionStateDeleting)
			}
			if !tc.hasDefaultVMProfile {
				testVmssFlex.Properties.VirtualMachineProfile = nil
			}
			if tc.isNicConfigEmpty {
				testVmssFlex.Properties.VirtualMachineProfile.NetworkProfile.NetworkInterfaceConfigurations = []*armcompute.VirtualMachineScaleSetNetworkConfiguration{}
			}
			if tc.isIPConfigEmpty {
				(testVmssFlex.Properties.VirtualMachineProfile.NetworkProfile.NetworkInterfaceConfigurations)[0].Properties.IPConfigurations = []*armcompute.VirtualMachineScaleSetIPConfiguration{}
			}

			vmssFlexList := []*armcompute.VirtualMachineScaleSet{testVmssFlex}

			mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(vmssFlexList, nil).Times(tc.vmssListCallingTimes)
			mockVMSSClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(testVmssFlex1, nil).AnyTimes()
			mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, tc.vmssPutErr).AnyTimes()

			err = fs.EnsureBackendPoolDeletedFromVMSets(context.TODO(), tc.vmssNamesMap, []string{tc.backendPoolID})
			_, _ = fs.getVmssFlexByName(context.TODO(), "vmssflex1")

			if tc.expectedErr != nil {
				assert.Contains(t, err.Error(), tc.expectedErr.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestEnsureBackendPoolDeletedFromNodeVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description         string
		vmssFlexVMNameMap   map[string]string
		backendPoolID       string
		nics                []*armnetwork.Interface
		expectedPutNICTimes int
		nicGetErr           error
		nicPutErr           error
		expectedErr         error
	}{
		{
			description: "EnsureBackendPoolDeletedFromNode should remove backend pools from the vmss flex vm",
			vmssFlexVMNameMap: map[string]string{
				"vmssflex1000001": "testvm1-nic",
				"vmssflex1000002": "testvm2-nic",
			},
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0",
			nics: []*armnetwork.Interface{
				generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateSucceeded), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1"),
				generateTestNic("testvm2-nic", false, to.Ptr(armnetwork.ProvisioningStateSucceeded), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm2"),
			},
			expectedPutNICTimes: 1,
			nicGetErr:           nil,
			expectedErr:         nil,
		},
		{
			description: "EnsureBackendPoolDeletedFromNode should remove a backend pool from the vmss flex vm",
			vmssFlexVMNameMap: map[string]string{
				"vmssflex1000001": "testvm1-nic",
			},
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0",
			nics:          []*armnetwork.Interface{generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateSucceeded), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1")},
			nicGetErr:     &azcore.ResponseError{ErrorCode: "failed to get nic"},
			expectedErr:   fmt.Errorf("failed to get nic"),
		},
		{
			description: "EnsureBackendPoolDeletedFromNode should skip the node if the NIC is in failed state",
			vmssFlexVMNameMap: map[string]string{
				"vmssflex1000001": "testvm1-nic",
			},
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0",
			nics:          []*armnetwork.Interface{generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateFailed), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1")},
			nicGetErr:     nil,
			expectedErr:   nil,
		},
		{
			description: "EnsureBackendPoolDeletedFromNode should return error if NIC update fails",
			vmssFlexVMNameMap: map[string]string{
				"vmssflex1000001": "testvm1-nic",
			},
			backendPoolID:       "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0",
			nics:                []*armnetwork.Interface{generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateSucceeded), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1")},
			expectedPutNICTimes: 1,
			nicGetErr:           nil,
			nicPutErr:           &azcore.ResponseError{ErrorCode: "failed to update nic"},
			expectedErr:         fmt.Errorf("failed to update nic"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			fs, err := NewTestFlexScaleSet(ctrl)
			assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

			mockInterfacesClient := fs.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
			for i := range tc.nics {
				nic := tc.nics[i]
				mockInterfacesClient.EXPECT().Get(gomock.Any(), gomock.Any(), *nic.Name, gomock.Any()).Return(nic, tc.nicGetErr).AnyTimes()
				mockInterfacesClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), *nic.Name, gomock.Any()).Return(nil, tc.nicPutErr).Times(tc.expectedPutNICTimes)
			}

			updated, err := fs.ensureBackendPoolDeletedFromNode(context.TODO(), tc.vmssFlexVMNameMap, []string{tc.backendPoolID})

			if tc.expectedErr != nil {
				assert.Contains(t, err.Error(), tc.expectedErr.Error())
			} else {
				assert.NoError(t, err)
				if tc.expectedPutNICTimes > 0 {
					assert.True(t, updated)
				}
			}
		})
	}
}

func TestEnsureBackendPoolDeletedVmssFlex(t *testing.T) {

	testCases := []struct {
		description         string
		service             *v1.Service
		vmSetName           string
		backendPoolID       string
		backendAddressPools []*armnetwork.BackendAddressPool
		deleteFromVMSet     bool
		isStandardLB        bool

		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		nic                            *armnetwork.Interface
		nicGetErr                      error
		nicPutErr                      error
		vmssPutErr                     error

		expectedErr error
	}{
		{
			description:                    "EnsureBackendPoolDeleted should delete a backend pool from the vm and vmss",
			service:                        &v1.Service{},
			vmSetName:                      "vmssflex1",
			backendPoolID:                  testBackendPoolID0,
			backendAddressPools:            testBackendPools,
			deleteFromVMSet:                true,
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateSucceeded), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1"),
			nicGetErr:                      nil,
			expectedErr:                    nil,
		},
		{
			description:                    "EnsureBackendPoolDeleted should do nothing if the backendPools is nil",
			service:                        &v1.Service{},
			vmSetName:                      "vmssflex1",
			backendPoolID:                  testBackendPoolID0,
			backendAddressPools:            nil,
			deleteFromVMSet:                true,
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateSucceeded), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1"),
			nicGetErr:                      nil,
			expectedErr:                    nil,
		},
		{
			description:                    "EnsureBackendPoolDeleted should return error if nic update fails",
			service:                        &v1.Service{},
			vmSetName:                      "vmssflex1",
			backendPoolID:                  testBackendPoolID0,
			backendAddressPools:            testBackendPools,
			deleteFromVMSet:                true,
			isStandardLB:                   true,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			nic:                            generateTestNic("testvm1-nic", false, to.Ptr(armnetwork.ProvisioningStateSucceeded), "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1"),
			nicGetErr:                      nil,
			nicPutErr:                      &azcore.ResponseError{ErrorCode: "failed to update nic"},
			expectedErr:                    fmt.Errorf("failed to update nic"),
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			fs, err := NewTestFlexScaleSet(ctrl)
			assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")
			if tc.isStandardLB {
				fs.Config.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			}

			testVmssFlex := genreteTestVmssFlex("vmssflex1", testVmssFlex1ID)
			vmssFlexList := []*armcompute.VirtualMachineScaleSet{testVmssFlex, genreteTestVmssFlex("vmssflex2", testVmssFlex2ID)}

			mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(vmssFlexList, nil).AnyTimes()
			mockVMSSClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(testVmssFlex1, nil).AnyTimes()
			mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, tc.vmssPutErr).AnyTimes()

			mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
			mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
			mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

			mockInterfacesClient := fs.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
			mockInterfacesClient.EXPECT().Get(gomock.Any(), gomock.Any(), "testvm1-nic", gomock.Any()).Return(tc.nic, tc.nicGetErr).AnyTimes()
			mockInterfacesClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, tc.nicPutErr).AnyTimes()

			_, err = fs.EnsureBackendPoolDeleted(context.TODO(), tc.service, []string{tc.backendPoolID}, tc.vmSetName, tc.backendAddressPools, tc.deleteFromVMSet)

			if tc.expectedErr != nil {
				assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
			} else {
				assert.NoError(t, err, tc.description)
			}

		})
	}
}
