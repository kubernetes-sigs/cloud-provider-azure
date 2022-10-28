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

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-03-01/compute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/util/sets"
	cloudprovider "k8s.io/cloud-provider"

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
		vmName := to.String(vm.OsProfile.ComputerName)
		ssName, instanceID, realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, "vmss", ssName)
		assert.Equal(t, to.String(vm.InstanceID), instanceID)
		assert.Equal(t, &vm, realVM)
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
	ssName, instanceID, realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "vmss", ssName)
	assert.Equal(t, to.String(vm.InstanceID), instanceID)
	assert.Equal(t, &vm, realVM)
}

func TestDeleteCacheForAvailabilitySetNodeInVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ss, err := NewTestScaleSet(ctrl)
	assert.NoError(t, err)

	mockVMClient := mockvmclient.NewMockInterface(ctrl)
	ss.cloud.VirtualMachinesClient = mockVMClient

	mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{
		{Name: to.StringPtr("vm1")},
	}, nil).AnyTimes()
	ss.cloud.nodeNames = sets.NewString("vm1")

	err = ss.DeleteCacheForNode("vm1")
	assert.NoError(t, err)

	cached, err := ss.availabilitySetNodesCache.Get(consts.AvailabilitySetNodesKey, azcache.CacheReadTypeUnsafe)
	assert.NoError(t, err)
	entry := cached.(*availabilitySetNodeEntry)
	assert.Equal(t, 0, len(entry.nodeNames))
	assert.Equal(t, 0, len(entry.vmNames))
	assert.Equal(t, 0, len(entry.vms))
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

		ssName, instanceID, realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
		assert.Nil(t, realVM)
		assert.Equal(t, "", ssName)
		assert.Equal(t, instanceID, ssName)
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
	ssName, instanceID, realVM, err := ss.getVmssVM(vmName, azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "vmss", ssName)
	assert.Equal(t, to.String(vm.InstanceID), instanceID)
	assert.Equal(t, &vm, realVM)

	// verify cache has test vmss.
	cacheKey := strings.ToLower(fmt.Sprintf("%s/%s", "rg", testVMSSName))
	_, ok := ss.vmssVMCache.Load(cacheKey)
	assert.Equal(t, true, ok)

	// refresh the cache with error.
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{}, &retry.Error{HTTPStatusCode: http.StatusNotFound}).Times(2)
	ssName, instanceID, realVM, err = ss.getVmssVM(vmName, azcache.CacheReadTypeForceRefresh)
	assert.Nil(t, realVM)
	assert.Equal(t, "", ssName)
	assert.Equal(t, "", instanceID)
	assert.Equal(t, cloudprovider.InstanceNotFound, err)

	// verify cache is cleared.
	_, ok = ss.vmssVMCache.Load(cacheKey)
	assert.Equal(t, false, ok)
}
