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
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	cloudprovider "k8s.io/cloud-provider"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssvmclient/mockvmssvmclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestExtractVmssVMName(t *testing.T) {
	cases := []struct {
		description        string
		vmName             string
		expectError        bool
		expectedScaleSet   string
		expectedInstanceID string
	}{
		{
			description: "wrong vmss VM name should report error",
			vmName:      "vm1234",
			expectError: true,
		},
		{
			description: "wrong VM name separator should report error",
			vmName:      "vm-1234",
			expectError: true,
		},
		{
			description:        "correct vmss VM name should return correct ScaleSet and instanceID",
			vmName:             "vm_1234",
			expectedScaleSet:   "vm",
			expectedInstanceID: "1234",
		},
		{
			description:        "correct vmss VM name with Extra Separator should return correct ScaleSet and instanceID",
			vmName:             "vm_test_1234",
			expectedScaleSet:   "vm_test",
			expectedInstanceID: "1234",
		},
	}

	for _, c := range cases {
		ssName, instanceID, err := extractVmssVMName(c.vmName)
		if c.expectError {
			assert.Error(t, err, c.description)
			continue
		}

		assert.Equal(t, c.expectedScaleSet, ssName, c.description)
		assert.Equal(t, c.expectedInstanceID, instanceID, c.description)
	}
}

func TestVMSSVMCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmList := []string{"vmssee6c2000000", "vmssee6c2000001", "vmssee6c2000002"}
	c := GetTestCloud(ctrl)
	c.DisableAvailabilitySetNodes = true
	vmSet, err := newScaleSet(c)
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
		vmName := to.String(vm.OsProfile.ComputerName)
		realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, "vmss", realVM.VMSSName)
		assert.Equal(t, to.String(vm.InstanceID), realVM.InstanceID)
		assert.Equal(t, &vm, realVM.AsVirtualMachineScaleSetVM())
	}

	// validate DeleteCacheForNode().
	vm := expectedVMs[0]
	vmName := to.String(vm.OsProfile.ComputerName)
	err = ss.DeleteCacheForNode(vmName)
	assert.NoError(t, err)

	// the VM should be removed from cache after DeleteCacheForNode().
	cacheKey, cache, err := ss.getVMSSVMCache("rg", testVMSSName)
	assert.NoError(t, err)
	cached, err := cache.Get(cacheKey, azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	cachedVirtualMachines := cached.(*sync.Map)
	_, ok := cachedVirtualMachines.Load(vmName)
	assert.Equal(t, false, ok)

	// the VM should be back after another cache refresh.
	realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "vmss", realVM.VMSSName)
	assert.Equal(t, to.String(vm.InstanceID), realVM.InstanceID)
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
		Name:                             to.StringPtr(testVMSSName),
		VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{},
	}
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expectedScaleSet}, nil).AnyTimes()

	expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.cloud, testVMSSName, "", 0, vmList, string(compute.ProvisioningStateDeleting), false)
	mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()

	for i := range expectedVMs {
		vm := expectedVMs[i]
		vmName := to.String(vm.OsProfile.ComputerName)
		assert.Equal(t, vm.ProvisioningState, to.StringPtr(string(compute.ProvisioningStateDeleting)))

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
	mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()

	// validate getting VMSS VM via cache.
	vm := expectedVMs[0]
	vmName := to.String(vm.OsProfile.ComputerName)
	realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "vmss", realVM.VMSSName)
	assert.Equal(t, to.String(vm.InstanceID), realVM.InstanceID)
	assert.Equal(t, &vm, realVM.AsVirtualMachineScaleSetVM())

	// verify cache has test vmss.
	cacheKey := strings.ToLower(fmt.Sprintf("%s/%s", "rg", testVMSSName))
	_, ok := ss.vmssVMCache.Load(cacheKey)
	assert.Equal(t, true, ok)

	// refresh the cache with error.
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{}, &retry.Error{HTTPStatusCode: http.StatusNotFound}).Times(2)
	realVM, err = ss.getVmssVM(vmName, azcache.CacheReadTypeForceRefresh)
	assert.Nil(t, realVM)
	assert.Equal(t, cloudprovider.InstanceNotFound, err)

	// verify cache is cleared.
	_, ok = ss.vmssVMCache.Load(cacheKey)
	assert.Equal(t, false, ok)
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

func TestGetVMManagementTypeByIPConfigurationID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testVM1 := generateVmssFlexTestVMWithoutInstanceView(testVM1Spec)
	testVM2 := generateVmssFlexTestVMWithoutInstanceView(testVM2Spec)
	testVM2.VirtualMachineScaleSet = nil
	testVM2.VirtualMachineProperties.OsProfile.ComputerName = to.StringPtr("testvm2")

	testVMList := []compute.VirtualMachine{
		testVM1,
		testVM2,
	}

	testCases := []struct {
		description                 string
		ipConfigurationID           string
		DisableAvailabilitySetNodes bool
		EnableVmssFlexNodes         bool
		vmListErr                   *retry.Error
		expectedVMManagementType    VMManagementType
		expectedErr                 error
	}{
		{
			description:              "getVMManagementTypeByIPConfigurationID should return ManagedByVmssFlex for vmss flex node",
			ipConfigurationID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm1-nic/ipConfigurations/pipConfig",
			vmListErr:                nil,
			expectedVMManagementType: ManagedByVmssFlex,
			expectedErr:              nil,
		},
		{
			description:              "getVMManagementTypeByIPConfigurationID should return ManagedByAvSet for availabilityset node",
			ipConfigurationID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm2-nic/ipConfigurations/pipConfig",
			vmListErr:                nil,
			expectedVMManagementType: ManagedByAvSet,
			expectedErr:              nil,
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

		vmManagementType, err := ss.getVMManagementTypeByIPConfigurationID(tc.ipConfigurationID, azcache.CacheReadTypeDefault)
		assert.Equal(t, tc.expectedVMManagementType, vmManagementType, tc.description)
		if tc.expectedErr != nil {
			assert.EqualError(t, err, tc.expectedErr.Error(), tc.description)
		}

	}
}
