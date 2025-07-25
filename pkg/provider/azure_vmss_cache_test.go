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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/interfaceclient/mock_interfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient/mock_virtualmachinescalesetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetvmclient/mock_virtualmachinescalesetvmclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func TestVMSSVMCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmList := []string{"vmssee6c2000000", "vmssee6c2000001", "vmssee6c2000002"}
	c := GetTestCloud(ctrl)
	c.DisableAvailabilitySetNodes = true
	vmSet, err := newScaleSet(c)
	assert.NoError(t, err)
	ss := vmSet.(*ScaleSet)

	mockVMSSClient := ss.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
	mockVMSSVMClient := ss.ComputeClientFactory.GetVirtualMachineScaleSetVMClient().(*mock_virtualmachinescalesetvmclient.MockInterface)

	expectedScaleSet := buildTestVMSS(testVMSSName, "vmssee6c2")
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]*armcompute.VirtualMachineScaleSet{expectedScaleSet}, nil).AnyTimes()

	expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, vmList, "", false)
	mockVMSSVMClient.EXPECT().ListVMInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()

	// validate getting VMSS VM via cache.
	for i := range expectedVMs {
		vm := expectedVMs[i]
		vmName := ptr.Deref(vm.Properties.OSProfile.ComputerName, "")
		realVM, err := ss.getVmssVM(context.TODO(), vmName, azcache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.NotNil(t, realVM)
		assert.Equal(t, "vmss", realVM.VMSSName)
		assert.Equal(t, ptr.Deref(vm.InstanceID, ""), realVM.InstanceID)
		assert.Equal(t, *vm, *realVM.AsVirtualMachineScaleSetVM())
	}

	// validate DeleteCacheForNode().
	vm := expectedVMs[0]
	vmName := ptr.Deref(vm.Properties.OSProfile.ComputerName, "")
	err = ss.DeleteCacheForNode(context.TODO(), vmName)
	assert.NoError(t, err)

	// the VM should be in cache after refresh.
	realVM, err := ss.getVmssVM(context.TODO(), vmName, azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "vmss", realVM.VMSSName)
	assert.Equal(t, ptr.Deref(vm.InstanceID, ""), realVM.InstanceID)
	assert.Equal(t, vm, realVM.AsVirtualMachineScaleSetVM())
}

func TestVMSSVMCacheWithDeletingNodes(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vmList := []string{"vmssee6c2000000", "vmssee6c2000001", "vmssee6c2000002"}
	ss, err := newTestScaleSetWithState(ctrl)
	assert.NoError(t, err)

	mockVMSSClient := ss.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
	mockVMSSVMClient := ss.ComputeClientFactory.GetVirtualMachineScaleSetVMClient().(*mock_virtualmachinescalesetvmclient.MockInterface)

	expectedScaleSet := &armcompute.VirtualMachineScaleSet{
		Name:       ptr.To(testVMSSName),
		Properties: &armcompute.VirtualMachineScaleSetProperties{},
	}
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]*armcompute.VirtualMachineScaleSet{expectedScaleSet}, nil).AnyTimes()

	expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, vmList, string(consts.ProvisioningStateDeleting), false)
	mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()

	for i := range expectedVMs {
		vm := expectedVMs[i]
		vmName := ptr.Deref(vm.Properties.OSProfile.ComputerName, "")
		assert.Equal(t, *vm.Properties.ProvisioningState, string(consts.ProvisioningStateDeleting))

		realVM, err := ss.getVmssVM(context.TODO(), vmName, azcache.CacheReadTypeDefault)
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
	mockVMSSClient := ss.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
	mockVMSSVMClient := ss.ComputeClientFactory.GetVirtualMachineScaleSetVMClient().(*mock_virtualmachinescalesetvmclient.MockInterface)

	expectedScaleSet := buildTestVMSS(testVMSSName, "vmssee6c2")
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]*armcompute.VirtualMachineScaleSet{expectedScaleSet}, nil).Times(1)

	expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, vmList, "", false)
	mockVMSSVMClient.EXPECT().ListVMInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).Times(1)

	// validate getting VMSS VM via cache.
	vm := expectedVMs[0]
	vmName := ptr.Deref(vm.Properties.OSProfile.ComputerName, "")
	realVM, err := ss.getVmssVM(context.TODO(), vmName, azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "vmss", realVM.VMSSName)
	assert.Equal(t, ptr.Deref(vm.InstanceID, ""), realVM.InstanceID)
	assert.Equal(t, *vm, *realVM.AsVirtualMachineScaleSetVM())

	// verify cache has test vmss.
	cacheKey := getVMSSVMCacheKey("rg", testVMSSName)
	_, err = ss.vmssVMCache.Get(context.TODO(), cacheKey, azcache.CacheReadTypeDefault)
	assert.Nil(t, err)

	// refresh the cache with error.
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]*armcompute.VirtualMachineScaleSet{}, &azcore.ResponseError{StatusCode: http.StatusNotFound}).Times(2)
	mockVMSSVMClient.EXPECT().ListVMInstanceView(gomock.Any(), "rg", testVMSSName).Return([]*armcompute.VirtualMachineScaleSetVM{}, &azcore.ResponseError{StatusCode: http.StatusNotFound}).Times(1)
	realVM, err = ss.getVmssVM(context.TODO(), vmName, azcache.CacheReadTypeForceRefresh)
	assert.Nil(t, realVM)
	assert.Equal(t, cloudprovider.InstanceNotFound, err)

	// verify cache is cleared.
	_, err = ss.vmssVMCache.Get(context.TODO(), cacheKey, azcache.CacheReadTypeDefault)
	assert.NotNil(t, err)
}

func TestGetVMManagementTypeByNodeName(t *testing.T) {

	testVM1 := generateVmssFlexTestVMWithoutInstanceView(testVM1Spec)
	testVM2 := generateVmssFlexTestVMWithoutInstanceView(testVM2Spec)
	testVM2.Properties.VirtualMachineScaleSet = nil

	testVMList := []*armcompute.VirtualMachine{
		testVM1,
		testVM2,
	}

	testCases := []struct {
		description                 string
		nodeName                    string
		DisableAvailabilitySetNodes bool
		EnableVmssFlexNodes         bool
		vmListErr                   error
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
			vmListErr:                &azcore.ResponseError{ErrorCode: "failed to list VMs"},
			expectedVMManagementType: ManagedByUnknownVMSet,
			expectedErr:              fmt.Errorf("failed to list VMs"),
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			ss, err := NewTestScaleSet(ctrl)
			assert.NoError(t, err, tc.description)

			ss.DisableAvailabilitySetNodes = tc.DisableAvailabilitySetNodes
			ss.EnableVmssFlexNodes = tc.EnableVmssFlexNodes

			mockVMClient := ss.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
			mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVMList, tc.vmListErr).AnyTimes()

			vmManagementType, err := ss.getVMManagementTypeByNodeName(context.TODO(), tc.nodeName, azcache.CacheReadTypeDefault)
			assert.Equal(t, tc.expectedVMManagementType, vmManagementType, tc.description)
			if tc.expectedErr != nil {
				assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
			}
		})
	}
}

func TestGetVMManagementTypeByProviderID(t *testing.T) {

	testVM1 := generateVmssFlexTestVMWithoutInstanceView(testVM1Spec)
	testVM2 := generateVmssFlexTestVMWithoutInstanceView(testVM2Spec)
	testVM2.Properties.VirtualMachineScaleSet = nil

	testVMList := []*armcompute.VirtualMachine{
		testVM1,
		testVM2,
	}

	testCases := []struct {
		description                 string
		providerID                  string
		DisableAvailabilitySetNodes bool
		EnableVmssFlexNodes         bool
		vmListErr                   error
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
			vmListErr:                &azcore.ResponseError{ErrorCode: "failed to list VMs"},
			expectedVMManagementType: ManagedByUnknownVMSet,
			expectedErr:              fmt.Errorf("failed to list VMs"),
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			ss, err := NewTestScaleSet(ctrl)
			assert.NoError(t, err, tc.description)

			ss.DisableAvailabilitySetNodes = tc.DisableAvailabilitySetNodes
			ss.EnableVmssFlexNodes = tc.EnableVmssFlexNodes

			mockVMClient := ss.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
			mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVMList, tc.vmListErr).AnyTimes()

			vmManagementType, err := ss.getVMManagementTypeByProviderID(context.TODO(), tc.providerID, azcache.CacheReadTypeDefault)
			assert.Equal(t, tc.expectedVMManagementType, vmManagementType, tc.description)
			if tc.expectedErr != nil {
				assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
			}
		})
	}
}

func buildTestNICWithVMName(vmName string) *armnetwork.Interface {
	return &armnetwork.Interface{
		Name: &vmName,
		Properties: &armnetwork.InterfacePropertiesFormat{
			VirtualMachine: &armnetwork.SubResource{
				ID: ptr.To(fmt.Sprintf("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/%s", vmName)),
			},
		},
	}
}

func TestGetVMManagementTypeByIPConfigurationID(t *testing.T) {

	testVM1 := generateVmssFlexTestVMWithoutInstanceView(testVM1Spec)
	testVM2 := generateVmssFlexTestVMWithoutInstanceView(testVM2Spec)
	testVM2.Properties.VirtualMachineScaleSet = nil
	testVM2.Properties.OSProfile.ComputerName = ptr.To("testvm2")

	testVMList := []*armcompute.VirtualMachine{
		testVM1,
		testVM2,
	}

	testVM1NIC := buildTestNICWithVMName("testvm1")
	testVM2NIC := buildTestNICWithVMName("testvm2")
	testVM3NIC := buildTestNICWithVMName("testvm3")
	testVM3NIC.Properties.VirtualMachine = nil

	testCases := []struct {
		description                 string
		ipConfigurationID           string
		DisableAvailabilitySetNodes bool
		EnableVmssFlexNodes         bool
		vmListErr                   error
		nicGetErr                   error
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
			nicGetErr:                &azcore.ResponseError{ErrorCode: "failed to get nic"},
			expectedVMManagementType: ManagedByUnknownVMSet,
			expectedErr:              fmt.Errorf("failed to get nic"),
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
			vmListErr:                &azcore.ResponseError{ErrorCode: "failed to list VMs"},
			expectedVMManagementType: ManagedByUnknownVMSet,
			expectedErr:              fmt.Errorf("failed to list VMs"),
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			ss, err := NewTestScaleSet(ctrl)
			assert.NoError(t, err, tc.description)

			ss.DisableAvailabilitySetNodes = tc.DisableAvailabilitySetNodes
			ss.EnableVmssFlexNodes = tc.EnableVmssFlexNodes

			mockVMClient := ss.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
			mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(testVMList, tc.vmListErr).AnyTimes()

			if tc.expectedNIC != "" {
				mockNICClient := ss.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
				mockNICClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ string, _ *string) (*armnetwork.Interface, error) {
					switch tc.expectedNIC {
					case "testvm1":
						return testVM1NIC, tc.nicGetErr
					case "testvm2":
						return testVM2NIC, tc.nicGetErr
					case "testvm3":
						return testVM3NIC, tc.nicGetErr
					default:
						return &armnetwork.Interface{}, &azcore.ResponseError{ErrorCode: "failed to get nic"}
					}
				})
			}

			vmManagementType, err := ss.getVMManagementTypeByIPConfigurationID(context.TODO(), tc.ipConfigurationID, azcache.CacheReadTypeDefault)
			assert.Equal(t, tc.expectedVMManagementType, vmManagementType, tc.description)
			if tc.expectedErr != nil {
				assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
			}
		})
	}
}
