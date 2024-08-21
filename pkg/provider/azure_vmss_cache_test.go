/*
Copyright 2020 The Kubernetes Authors.

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
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/interfaceclient/mockinterfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssvmclient/mockvmssvmclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestVMSSVMCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmList := []string{"vmssee6c2000000", "vmssee6c2000001", "vmssee6c2000002"}
	c := GetTestCloud(ctrl)
	c.DisableAvailabilitySetNodes = true
	vmSet, err := newScaleSet(context.Background(), c)
	assert.NoError(t, err)
	ss := vmSet.(*ScaleSet)

	mockVMSSClient := mockvmssclient.NewMockInterface(ctrl)
	mockVMSSVMClient := mockvmssvmclient.NewMockInterface(ctrl)
	ss.cloud.VirtualMachineScaleSetsClient = mockVMSSClient
	ss.cloud.VirtualMachineScaleSetVMsClient = mockVMSSVMClient

	expectedScaleSet := buildTestVMSS(testVMSSName, "vmssee6c2")
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expectedScaleSet}, nil).AnyTimes()

	expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.cloud, testVMSSName, "", 0, vmList, "", false)
	mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()

	// validate getting VMSS VM via cache.
	for i := range expectedVMs {
		vm := expectedVMs[i]
		vmName := pointer.StringDeref(vm.OsProfile.ComputerName, "")
		realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.NotNil(t, realVM)
		assert.Equal(t, "vmss", realVM.VMSSName)
		assert.Equal(t, pointer.StringDeref(vm.InstanceID, ""), realVM.InstanceID)
		assert.Equal(t, &vm, realVM.AsVirtualMachineScaleSetVM())
	}

	// validate DeleteCacheForNode().
	vm := expectedVMs[0]
	vmName := pointer.StringDeref(vm.OsProfile.ComputerName, "")
	err = ss.DeleteCacheForNode(vmName)
	assert.NoError(t, err)

	// the VM should be in cache after refresh.
	realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "vmss", realVM.VMSSName)
	assert.Equal(t, pointer.StringDeref(vm.InstanceID, ""), realVM.InstanceID)
	assert.Equal(t, &vm, realVM.AsVirtualMachineScaleSetVM())
}

func TestVMSSVMCacheWithDeletingNodes(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmList := []string{"vmssee6c2000000", "vmssee6c2000001", "vmssee6c2000002"}
	ss, err := newTestScaleSetWithState(ctrl)
	assert.NoError(t, err)

	mockVMSSClient := mockvmssclient.NewMockInterface(ctrl)
	mockVMSSVMClient := mockvmssvmclient.NewMockInterface(ctrl)
	ss.cloud.VirtualMachineScaleSetsClient = mockVMSSClient
	ss.cloud.VirtualMachineScaleSetVMsClient = mockVMSSVMClient

	expectedScaleSet := compute.VirtualMachineScaleSet{
		Name:                             pointer.String(testVMSSName),
		VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{},
	}
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expectedScaleSet}, nil).AnyTimes()

	expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.cloud, testVMSSName, "", 0, vmList, string(consts.ProvisioningStateDeleting), false)
	mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()

	for i := range expectedVMs {
		vm := expectedVMs[i]
		vmName := pointer.StringDeref(vm.OsProfile.ComputerName, "")
		assert.Equal(t, vm.ProvisioningState, pointer.String(string(consts.ProvisioningStateDeleting)))

		realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
		assert.Nil(t, realVM)
		assert.Equal(t, cloudprovider.InstanceNotFound, err)
	}
}

func TestVMSSVMCacheClearedWhenRGDeleted(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmList := []string{"vmssee6c2000000", "vmssee6c2000001", "vmssee6c2000002"}
	ss, err := NewTestScaleSet(ctrl)
	assert.NoError(t, err)

	mockVMSSClient := mockvmssclient.NewMockInterface(ctrl)
	mockVMSSVMClient := mockvmssvmclient.NewMockInterface(ctrl)
	ss.cloud.VirtualMachineScaleSetsClient = mockVMSSClient
	ss.cloud.VirtualMachineScaleSetVMsClient = mockVMSSVMClient

	expectedScaleSet := buildTestVMSS(testVMSSName, "vmssee6c2")
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expectedScaleSet}, nil).Times(1)

	expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.cloud, testVMSSName, "", 0, vmList, "", false)
	mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).Times(1)

	// validate getting VMSS VM via cache.
	vm := expectedVMs[0]
	vmName := pointer.StringDeref(vm.OsProfile.ComputerName, "")
	realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "vmss", realVM.VMSSName)
	assert.Equal(t, pointer.StringDeref(vm.InstanceID, ""), realVM.InstanceID)
	assert.Equal(t, &vm, realVM.AsVirtualMachineScaleSetVM())

	// verify cache has test vmss.
	cacheKey := getVMSSVMCacheKey("rg", testVMSSName)
	_, err = ss.vmssVMCache.Get(cacheKey, azcache.CacheReadTypeDefault)
	assert.Nil(t, err)

	// refresh the cache with error.
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{}, &retry.Error{HTTPStatusCode: http.StatusNotFound}).Times(2)
	mockVMSSVMClient.EXPECT().List(gomock.Any(), "rg", testVMSSName, gomock.Any()).Return([]compute.VirtualMachineScaleSetVM{}, &retry.Error{HTTPStatusCode: http.StatusNotFound}).Times(1)
	realVM, err = ss.getVmssVM(vmName, azcache.CacheReadTypeForceRefresh)
	assert.Nil(t, realVM)
	assert.Equal(t, cloudprovider.InstanceNotFound, err)

	// verify cache is cleared.
	_, err = ss.vmssVMCache.Get(cacheKey, azcache.CacheReadTypeDefault)
	assert.NotNil(t, err)
}

func TestGetVMManagementTypeByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testVM1 := generateVmssFlexTestVMWithoutInstanceView(testVM1Spec)
	testVM2 := generateVmssFlexTestVMWithoutInstanceView(testVM2Spec)
	testVM2.VirtualMachineScaleSet = nil

	testVMList := []compute.VirtualMachine{
		testVM1,
		testVM2,
	}

	testCases := []struct {
		description                 string
		nodeName                    string
		DisableAvailabilitySetNodes bool
		EnableVmssFlexNodes         bool
		vmListErr                   *retry.Error
		expectedVMManagementType    VMManagementType
		expectedErr                 error
	}{
		{
			description:              "getVMManagementTypeByNodeName should return ManagedByVmssFlex for vmss flex node",
			nodeName:                 "vmssflex1000001",
			vmListErr:                nil,
			expectedVMManagementType: ManagedByVmssFlex,
			expectedErr:              nil,
		},
		{
			description:              "getVMManagementTypeByNodeName should return ManagedByAvSet for availabilityset node",
			nodeName:                 "vmssflex1000002",
			vmListErr:                nil,
			expectedVMManagementType: ManagedByAvSet,
			expectedErr:              nil,
		},
		{
			description:              "getVMManagementTypeByNodeName should return ManagedByVmssUniform for vmss uniform node",
			nodeName:                 "vmssflex1000003",
			vmListErr:                nil,
			expectedVMManagementType: ManagedByVmssUniform,
			expectedErr:              nil,
		},
		{
			description:                 "getVMManagementTypeByNodeName should return ManagedByVmssUniform if DisableAvailabilitySetNodes is set to true and EnableVmssFlexNodes is set to false",
			nodeName:                    "anyName",
			DisableAvailabilitySetNodes: true,
			EnableVmssFlexNodes:         false,
			vmListErr:                   nil,
			expectedVMManagementType:    ManagedByVmssUniform,
			expectedErr:                 nil,
		},
		{
			description:              "getVMManagementTypeByNodeName should return ManagedByUnknownVMSet if error happens",
			nodeName:                 "fakeName",
			vmListErr:                &retry.Error{RawError: fmt.Errorf("failed to list VMs")},
			expectedVMManagementType: ManagedByUnknownVMSet,
			expectedErr:              fmt.Errorf("getter function of nonVmssUniformNodesCache: failed to list vms in the resource group rg: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: failed to list VMs"),
		},
	}

	for _, tc := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, tc.description)

		ss.DisableAvailabilitySetNodes = tc.DisableAvailabilitySetNodes
		ss.EnableVmssFlexNodes = tc.EnableVmssFlexNodes

		mockVMClient := ss.cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVMList, tc.vmListErr).AnyTimes()

		vmManagementType, err := ss.getVMManagementTypeByNodeName(tc.nodeName, azcache.CacheReadTypeDefault)
		assert.Equal(t, tc.expectedVMManagementType, vmManagementType, tc.description)
		if tc.expectedErr != nil {
			assert.EqualError(t, err, tc.expectedErr.Error(), tc.description)
		}

	}
}

func TestGetVMManagementTypeByProviderID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testVM1 := generateVmssFlexTestVMWithoutInstanceView(testVM1Spec)
	testVM2 := generateVmssFlexTestVMWithoutInstanceView(testVM2Spec)
	testVM2.VirtualMachineScaleSet = nil

	testVMList := []compute.VirtualMachine{
		testVM1,
		testVM2,
	}

	testCases := []struct {
		description                 string
		providerID                  string
		DisableAvailabilitySetNodes bool
		EnableVmssFlexNodes         bool
		vmListErr                   *retry.Error
		expectedVMManagementType    VMManagementType
		expectedErr                 error
	}{
		{
			description:              "getVMManagementTypeByProviderID should return ManagedByVmssFlex for vmss flex node",
			providerID:               "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1",
			vmListErr:                nil,
			expectedVMManagementType: ManagedByVmssFlex,
			expectedErr:              nil,
		},
		{
			description:              "getVMManagementTypeByProviderID should return ManagedByAvSet for availabilityset node",
			providerID:               "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm2",
			vmListErr:                nil,
			expectedVMManagementType: ManagedByAvSet,
			expectedErr:              nil,
		},
		{
			description:              "getVMManagementTypeByProviderID should return ManagedByVmssUniform for vmss uniform node",
			providerID:               "azure:///subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/1",
			vmListErr:                nil,
			expectedVMManagementType: ManagedByVmssUniform,
			expectedErr:              nil,
		},
		{
			description:                 "getVMManagementTypeByProviderID should return ManagedByVmssUniform if DisableAvailabilitySetNodes is set to true and EnableVmssFlexNodes is set to false",
			providerID:                  "anyName",
			DisableAvailabilitySetNodes: true,
			EnableVmssFlexNodes:         false,
			vmListErr:                   nil,
			expectedVMManagementType:    ManagedByVmssUniform,
			expectedErr:                 nil,
		},
		{
			description:              "getVMManagementTypeByProviderID should return ManagedByUnknownVMSet if error happens",
			providerID:               "fakeName",
			vmListErr:                &retry.Error{RawError: fmt.Errorf("failed to list VMs")},
			expectedVMManagementType: ManagedByUnknownVMSet,
			expectedErr:              fmt.Errorf("getter function of nonVmssUniformNodesCache: failed to list vms in the resource group rg: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: failed to list VMs"),
		},
	}

	for _, tc := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, tc.description)

		ss.DisableAvailabilitySetNodes = tc.DisableAvailabilitySetNodes
		ss.EnableVmssFlexNodes = tc.EnableVmssFlexNodes

		mockVMClient := ss.cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVMList, tc.vmListErr).AnyTimes()

		vmManagementType, err := ss.getVMManagementTypeByProviderID(tc.providerID, azcache.CacheReadTypeDefault)
		assert.Equal(t, tc.expectedVMManagementType, vmManagementType, tc.description)
		if tc.expectedErr != nil {
			assert.EqualError(t, err, tc.expectedErr.Error(), tc.description)
		}

	}
}

func buildTestNICWithVMName(vmName string) network.Interface {
	return network.Interface{
		Name: &vmName,
		InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
			VirtualMachine: &network.SubResource{
				ID: pointer.String(fmt.Sprintf("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/%s", vmName)),
			},
		},
	}
}

func TestGetVMManagementTypeByIPConfigurationID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testVM1 := generateVmssFlexTestVMWithoutInstanceView(testVM1Spec)
	testVM2 := generateVmssFlexTestVMWithoutInstanceView(testVM2Spec)
	testVM2.VirtualMachineScaleSet = nil
	testVM2.VirtualMachineProperties.OsProfile.ComputerName = pointer.String("testvm2")

	testVMList := []compute.VirtualMachine{
		testVM1,
		testVM2,
	}

	testVM1NIC := buildTestNICWithVMName("testvm1")
	testVM2NIC := buildTestNICWithVMName("testvm2")
	testVM3NIC := buildTestNICWithVMName("testvm3")
	testVM3NIC.VirtualMachine = nil

	testCases := []struct {
		description                 string
		ipConfigurationID           string
		DisableAvailabilitySetNodes bool
		EnableVmssFlexNodes         bool
		vmListErr                   *retry.Error
		nicGetErr                   *retry.Error
		expectedNIC                 string
		expectedVMManagementType    VMManagementType
		expectedErr                 error
	}{
		{
			description:              "getVMManagementTypeByIPConfigurationID should return ManagedByVmssFlex for vmss flex node",
			ipConfigurationID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm1-nic/ipConfigurations/pipConfig",
			expectedNIC:              "testvm1",
			expectedVMManagementType: ManagedByVmssFlex,
		},
		{
			description:              "getVMManagementTypeByIPConfigurationID should return ManagedByAvSet for availabilityset node",
			ipConfigurationID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm2-nic/ipConfigurations/pipConfig",
			expectedVMManagementType: ManagedByAvSet,
		},
		{
			description:              "getVMManagementTypeByIPConfigurationID should return ManagedByAvSet for availabilityset node from nic.VirtualMachine.ID",
			ipConfigurationID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm2-interface/ipConfigurations/pipConfig",
			expectedNIC:              "testvm2",
			expectedVMManagementType: ManagedByAvSet,
		},
		{
			description:              "getVMManagementTypeByIPConfigurationID should return an error if nic.VirtualMachine.ID is empty",
			ipConfigurationID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm3-interface/ipConfigurations/pipConfig",
			expectedNIC:              "testvm3",
			expectedVMManagementType: ManagedByUnknownVMSet,
			expectedErr:              fmt.Errorf("failed to get vm name by ip config ID /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm3-interface/ipConfigurations/pipConfig: %w", errors.New("failed to get vm ID of nic testvm3")),
		},
		{
			description:              "getVMManagementTypeByIPConfigurationID should return an error if failed to get nic",
			ipConfigurationID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm1-nic/ipConfigurations/pipConfig",
			expectedNIC:              "testvm1",
			nicGetErr:                &retry.Error{RawError: fmt.Errorf("failed to get nic")},
			expectedVMManagementType: ManagedByUnknownVMSet,
			expectedErr:              fmt.Errorf("failed to get vm name by ip config ID /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm1-nic/ipConfigurations/pipConfig: %w", errors.New("failed to get interface of name testvm1-nic: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: failed to get nic")),
		},
		{
			description:              "getVMManagementTypeByIPConfigurationID should return ManagedByVmssUniform for vmss uniform node",
			ipConfigurationID:        "/subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/1/networkInterfaces/nic1",
			vmListErr:                nil,
			expectedVMManagementType: ManagedByVmssUniform,
			expectedErr:              nil,
		},
		{
			description:                 "getVMManagementTypeByIPConfigurationID should return ManagedByVmssUniform if DisableAvailabilitySetNodes is set to true and EnableVmssFlexNodes is set to false",
			ipConfigurationID:           "anyID",
			DisableAvailabilitySetNodes: true,
			EnableVmssFlexNodes:         false,
			vmListErr:                   nil,
			expectedVMManagementType:    ManagedByVmssUniform,
			expectedErr:                 nil,
		},
		{
			description:              "getVMManagementTypeByIPConfigurationID should return ManagedByUnknownVMSet if error happens",
			ipConfigurationID:        "fakeID",
			vmListErr:                &retry.Error{RawError: fmt.Errorf("failed to list VMs")},
			expectedVMManagementType: ManagedByUnknownVMSet,
			expectedErr:              fmt.Errorf("getter function of nonVmssUniformNodesCache: failed to list vms in the resource group rg: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: failed to list VMs"),
		},
	}

	for _, tc := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, tc.description)

		ss.DisableAvailabilitySetNodes = tc.DisableAvailabilitySetNodes
		ss.EnableVmssFlexNodes = tc.EnableVmssFlexNodes

		mockVMClient := ss.cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVMList, tc.vmListErr).AnyTimes()

		if tc.expectedNIC != "" {
			mockNICClient := ss.InterfacesClient.(*mockinterfaceclient.MockInterface)
			mockNICClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ string, _ string) (network.Interface, *retry.Error) {
				switch tc.expectedNIC {
				case "testvm1":
					return testVM1NIC, tc.nicGetErr
				case "testvm2":
					return testVM2NIC, tc.nicGetErr
				case "testvm3":
					return testVM3NIC, tc.nicGetErr
				default:
					return network.Interface{}, retry.NewError(false, errors.New("failed to get nic"))
				}
			})
		}

		vmManagementType, err := ss.getVMManagementTypeByIPConfigurationID(tc.ipConfigurationID, azcache.CacheReadTypeDefault)
		assert.Equal(t, tc.expectedVMManagementType, vmManagementType, tc.description)
		if tc.expectedErr != nil {
			assert.EqualError(t, err, tc.expectedErr.Error(), tc.description)
		}

	}
}

func TestVMSSUpdateCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ss, err := NewTestScaleSet(ctrl)
	assert.NoError(t, err, "unexpected error when creating test ScaleSet")

	testCases := []struct {
		description                 string
		nodeName, resourceGroupName string
		vmssName, instanceID        string
		vm                          *compute.VirtualMachineScaleSetVM
		disableUpdateCache          bool
		expectedErr                 error
	}{
		{
			description:        "disableUpdateCache is set",
			disableUpdateCache: true,
			expectedErr:        nil,
		},
	}

	for _, test := range testCases {
		ss.DisableUpdateCache = test.disableUpdateCache
		err = ss.updateCache(test.nodeName, test.nodeName, test.resourceGroupName, test.instanceID, test.vm)
		assert.Equal(t, test.expectedErr, err, test.description)
	}
}
