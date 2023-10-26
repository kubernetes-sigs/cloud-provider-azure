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
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/util/sets"
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

	// ensure only one vm is removed from the cache
	cacheKey, timedcache, _ := ss.getVMSSVMCache("rg", "vmss")
	entry, _, _ := timedcache.Store.GetByKey(cacheKey)
	cached := entry.(*azcache.AzureCacheEntry).Data
	virtualMachines := cached.(*sync.Map)
	length := 0
	virtualMachines.Range(func(key, value interface{}) bool {
		length++
		assert.NotEqual(t, vmName, key)
		fmt.Println(key)
		return true
	})
	assert.Equal(t, 2, length)

	// the VM should be in cache after refresh.
	realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "vmss", realVM.VMSSName)
	assert.Equal(t, pointer.StringDeref(vm.InstanceID, ""), realVM.InstanceID)
	assert.Equal(t, &vm, realVM.AsVirtualMachineScaleSetVM())
}

func TestDeleteCacheForAvailabilitySetNodeInVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ss, err := NewTestScaleSet(ctrl)
	assert.NoError(t, err)

	mockVMClient := mockvmclient.NewMockInterface(ctrl)
	ss.cloud.VirtualMachinesClient = mockVMClient

	mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{
		{Name: pointer.String("vm1")},
	}, nil)
	mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()
	ss.cloud.nodeNames = sets.NewString("vm1")

	err = ss.DeleteCacheForNode("vm1")
	assert.NoError(t, err)

	ss.cloud.nodeNames = sets.NewString()
	cached, err := ss.availabilitySetNodesCache.Get(consts.AvailabilitySetNodesKey, azcache.CacheReadTypeUnsafe)
	assert.NoError(t, err)
	entry := cached.(*AvailabilitySetNodeEntry)
	assert.Equal(t, 0, len(entry.NodeNames))
	assert.Equal(t, 0, len(entry.VMNames))
	assert.Equal(t, 0, len(entry.VMs))
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
	mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()

	// validate getting VMSS VM via cache.
	vm := expectedVMs[0]
	vmName := pointer.StringDeref(vm.OsProfile.ComputerName, "")
	realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "vmss", realVM.VMSSName)
	assert.Equal(t, pointer.StringDeref(vm.InstanceID, ""), realVM.InstanceID)
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
			ipConfigurationID:        "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm3-nic/ipConfigurations/pipConfig",
			expectedNIC:              "testvm1",
			nicGetErr:                &retry.Error{RawError: fmt.Errorf("failed to get nic")},
			expectedVMManagementType: ManagedByUnknownVMSet,
			expectedErr:              fmt.Errorf("failed to get vm name by ip config ID /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm3-nic/ipConfigurations/pipConfig: %w", errors.New("failed to get interface of name testvm3-nic: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: failed to get nic")),
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
			expectedErr:              fmt.Errorf("newAvailabilitySetNodesCache: failed to list vms in the resource group rg: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: failed to list VMs"),
		},
	}
	for _, tc := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, tc.description)
		ss.DisableAvailabilitySetNodes = tc.DisableAvailabilitySetNodes
		mockVMClient := ss.cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVMList, tc.vmListErr).AnyTimes()

		if tc.expectedNIC != "" {
			mockNICClient := ss.InterfacesClient.(*mockinterfaceclient.MockInterface)
			mockNICClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, resourceGroupName string, nicName string, expand string) (network.Interface, *retry.Error) {
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
