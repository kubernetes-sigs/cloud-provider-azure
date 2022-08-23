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
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

var (
	testVmssFlexID1 = "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1"
	testNodeName1   = "vmssflex1000001"
	testNode1       = &v1.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: testNodeName1,
		},
	}

	testVmssFlexID2 = "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex2"
	testNodeName2   = "vmssflex2000001"
	testNode2       = &v1.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: testNodeName2,
		},
	}

	nonExistingNodeName = "NonExistingNodeName"
)

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

		vmSetName, err := fs.GetNodeVMSetName(testNode1)
		assert.Equal(t, tc.expectedVMSetName, vmSetName, tc.description)
		assert.Equal(t, tc.expectedErr, err, tc.description)
	}

}

func TestGetAgentPoolVMSetNamesVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                 string
		nodes                       []*v1.Node
		expectedAgentPoolVMSetNames *[]string
		expectedErr                 error
	}{
		{
			description:                 "GetNodeVMSetName should return the correct VMSetName of the node",
			nodes:                       []*v1.Node{testNode1, testNode2},
			expectedAgentPoolVMSetNames: &[]string{"vmssflex1", "vmssflex2"},
			expectedErr:                 nil,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")
		fs.vmssFlexVMNameToVmssID.Store(testNodeName1, testVmssFlexID1)
		fs.vmssFlexVMNameToVmssID.Store(testNodeName2, testVmssFlexID2)

		agentPoolVMSetNames, err := fs.GetAgentPoolVMSetNames(tc.nodes)
		assert.Equal(t, tc.expectedAgentPoolVMSetNames, agentPoolVMSetNames, tc.description)
		assert.Equal(t, tc.expectedErr, err, tc.description)
	}
}

func TestGetVMSetNamesVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description        string
		service            *v1.Service
		nodes              []*v1.Node
		useSingleSLB       bool
		expectedVMSetNames *[]string
		expectedErr        error
	}{
		{
			description:        "GetVMSetNames should return the primary vm set name if the service has no mode annotation",
			service:            &v1.Service{},
			expectedVMSetNames: &[]string{"vmss"},
		},
		{
			description: "GetVMSetNames should return the primary vm set name when using the single SLB",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: consts.ServiceAnnotationLoadBalancerAutoModeValue}},
			},
			useSingleSLB:       true,
			expectedVMSetNames: &[]string{"vmss"},
		},
		{
			description: "GetVMSetNames should return all scale sets if the service has auto mode annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: consts.ServiceAnnotationLoadBalancerAutoModeValue}},
			},
			nodes:              []*v1.Node{testNode1, testNode2},
			expectedVMSetNames: &[]string{"vmssflex1", "vmssflex2"},
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
			expectedVMSetNames: &[]string{"vmssflex1"},
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")
		fs.vmssFlexVMNameToVmssID.Store(testNodeName1, testVmssFlexID1)
		fs.vmssFlexVMNameToVmssID.Store(testNodeName2, testVmssFlexID2)

		if tc.useSingleSLB {
			fs.EnableMultipleStandardLoadBalancers = false
			fs.LoadBalancerSku = consts.LoadBalancerSkuStandard
		}

		vmSetNames, err := fs.GetVMSetNames(tc.service, tc.nodes)
		assert.Equal(t, tc.expectedVMSetNames, vmSetNames, tc.description)
		assert.Equal(t, tc.expectedErr, err, tc.description)
	}
}

func TestGetNodeNameByProviderIDVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		providerID                     string
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
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
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithoutInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		nodeName, err := fs.GetNodeNameByProviderID(tc.providerID)
		assert.Equal(t, tc.expectedNodeName, nodeName)
		assert.Equal(t, tc.expectedErr, err)
	}

}

func TestGetInstanceIDByNodeNameVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       string
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
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
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithoutInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		instanceID, err := fs.GetInstanceIDByNodeName(tc.nodeName)
		assert.Equal(t, tc.expectedInstanceID, instanceID)
		assert.Equal(t, tc.expectedErr, err)
	}
}

func TestGetInstanceTypeByNodeNameVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       string
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
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

		mockVMSSClient := fs.cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithoutInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		instanceType, err := fs.GetInstanceTypeByNodeName(tc.nodeName)
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
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
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

		mockVMSSClient := fs.cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithoutInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		zone, err := fs.GetZoneByNodeName(tc.nodeName)
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
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
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

		mockVMSSClient := fs.cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithoutInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		provisioningState, err := fs.GetProvisioningStateByNodeName(tc.nodeName)
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
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
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
			description:                    "GetPowerStatusByNodeName should return stopped if the node powerstate is nil",
			nodeName:                       "vmssflex1000003",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedPowerStatus:            "stopped",
			expectedErr:                    nil,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVmssFlexList, nil).AnyTimes()

		mockVMClient := fs.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().ListVmssFlexVMsWithoutInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		powerStatus, err := fs.GetPowerStatusByNodeName(tc.nodeName)
		assert.Equal(t, tc.expectedPowerStatus, powerStatus)
		assert.Equal(t, tc.expectedErr, err)
	}

}

func TestGetPrimaryInterfaceVmssFlex(t *testing.T) {

}

func TestGetIPByNodeNameVmssFlex(t *testing.T) {

}

func TestGetPrivateIPsByNodeNameVmssFlex(t *testing.T) {

}

func TestGetNodeNameByIPConfigurationIDVmssFlex(t *testing.T) {

}

func TestGetNodeCIDRMasksByProviderIDVmssFlex(t *testing.T) {

}

func TestEnsureHostInPoolVmssFlex(t *testing.T) {

}

func TestEnsureHostsInPoolVmssFlex(t *testing.T) {

}

func TestEnsureBackendPoolDeletedFromVMSetsVmssFlex(t *testing.T) {

}

func TestEnsureBackendPoolDeletedVmssFlex(t *testing.T) {

}
