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
	"fmt"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient/mock_virtualmachinescalesetclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

var (
	testVmssFlex1ID = "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1"
	testVmssFlex2ID = "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex2"

	testVM1Spec = VmssFlexTestVMSpec{
		VMName:              "testvm1",
		VMID:                "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm1",
		ComputerName:        "vmssflex1000001",
		ProvisioningState:   ptr.To("Succeeded"),
		VmssFlexID:          testVmssFlex1ID,
		Zones:               to.SliceOfPtrs("1", "2", "3"),
		PlatformFaultDomain: ptr.To(int32(1)),
		Status: []*armcompute.InstanceViewStatus{
			{
				Code: ptr.To("PowerState/running"),
			},
		},
		NicID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm1-nic",
	}
	testVMWithoutInstanceView1 = generateVmssFlexTestVMWithoutInstanceView(testVM1Spec)

	testVM2Spec = VmssFlexTestVMSpec{
		VMName:              "testvm2",
		VMID:                "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm2",
		ComputerName:        "vmssflex1000002",
		ProvisioningState:   ptr.To("Succeeded"),
		VmssFlexID:          testVmssFlex1ID,
		Zones:               nil,
		PlatformFaultDomain: ptr.To(int32(1)),
		Status: []*armcompute.InstanceViewStatus{
			{
				Code: ptr.To("PowerState/running"),
			},
		},
		NicID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm2-nic",
	}
	testVMWithoutInstanceView2  = generateVmssFlexTestVMWithoutInstanceView(testVM2Spec)
	testVMWithOnlyInstanceView2 = generateVmssFlexTestVMWithOnlyInstanceView(testVM2Spec)

	testVM3Spec = VmssFlexTestVMSpec{
		VMName:              "testvm3",
		VMID:                "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/testvm3",
		ComputerName:        "vmssflex1000003",
		ProvisioningState:   nil,
		VmssFlexID:          testVmssFlex1ID,
		Zones:               nil,
		PlatformFaultDomain: nil,
		Status:              []*armcompute.InstanceViewStatus{},
		NicID:               "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/testvm3-nic",
	}

	testVMListWithoutInstanceView = generateTestVMListWithoutInstanceView()

	testVMListWithOnlyInstanceView = generateTestVMListWithOnlyInstanceView()

	testVmssFlex1 = genreteTestVmssFlex("vmssflex1", testVmssFlex1ID)

	testVmssFlexList = genreateTestVmssFlexList()
)

func generateTestVMListWithoutInstanceView() []*armcompute.VirtualMachine {
	return []*armcompute.VirtualMachine{generateVmssFlexTestVMWithoutInstanceView(testVM1Spec), generateVmssFlexTestVMWithoutInstanceView(testVM2Spec), generateVmssFlexTestVMWithoutInstanceView(testVM3Spec)}
}

func generateTestVMListWithOnlyInstanceView() []*armcompute.VirtualMachine {
	return []*armcompute.VirtualMachine{generateVmssFlexTestVMWithOnlyInstanceView(testVM1Spec), generateVmssFlexTestVMWithOnlyInstanceView(testVM2Spec), generateVmssFlexTestVMWithOnlyInstanceView(testVM3Spec)}
}

func genreateTestVmssFlexList() []*armcompute.VirtualMachineScaleSet {
	return []*armcompute.VirtualMachineScaleSet{genreteTestVmssFlex("vmssflex1", testVmssFlex1ID)}
}

func genreteTestVmssFlex(vmssFlexName string, testVmssFlexID string) *armcompute.VirtualMachineScaleSet {
	return &armcompute.VirtualMachineScaleSet{
		ID:   ptr.To(testVmssFlexID),
		Name: ptr.To(vmssFlexName),
		Properties: &armcompute.VirtualMachineScaleSetProperties{
			VirtualMachineProfile: &armcompute.VirtualMachineScaleSetVMProfile{
				OSProfile: &armcompute.VirtualMachineScaleSetOSProfile{
					ComputerNamePrefix: ptr.To(vmssFlexName),
				},
				NetworkProfile: &armcompute.VirtualMachineScaleSetNetworkProfile{
					NetworkInterfaceConfigurations: []*armcompute.VirtualMachineScaleSetNetworkConfiguration{
						{
							Properties: &armcompute.VirtualMachineScaleSetNetworkConfigurationProperties{
								IPConfigurations: []*armcompute.VirtualMachineScaleSetIPConfiguration{
									{
										Properties: &armcompute.VirtualMachineScaleSetIPConfigurationProperties{
											LoadBalancerBackendAddressPools: []*armcompute.SubResource{
												{
													ID: ptr.To(testBackendPoolID0),
												},
											},
											Primary: ptr.To(true),
										},
									},
								},
							},
						},
					},
				},
			},
			OrchestrationMode: to.Ptr(armcompute.OrchestrationModeFlexible),
		},
		Tags: map[string]*string{
			consts.VMSetCIDRIPV4TagKey: ptr.To("24"),
			consts.VMSetCIDRIPV6TagKey: ptr.To("64"),
		},
	}
}

type VmssFlexTestVMSpec struct {
	VMName              string
	VMID                string
	ComputerName        string
	ProvisioningState   *string
	VmssFlexID          string
	Zones               []*string
	PlatformFaultDomain *int32
	Status              []*armcompute.InstanceViewStatus
	NicID               string
}

func generateVmssFlexTestVMWithoutInstanceView(spec VmssFlexTestVMSpec) (testVMWithoutInstanceView *armcompute.VirtualMachine) {
	return &armcompute.VirtualMachine{
		Name: ptr.To(spec.VMName),
		ID:   ptr.To(spec.VMID),
		Properties: &armcompute.VirtualMachineProperties{
			OSProfile: &armcompute.OSProfile{
				ComputerName: ptr.To(spec.ComputerName),
			},
			ProvisioningState: spec.ProvisioningState,
			VirtualMachineScaleSet: &armcompute.SubResource{
				ID: ptr.To(spec.VmssFlexID),
			},
			StorageProfile: &armcompute.StorageProfile{
				OSDisk: &armcompute.OSDisk{
					Name: ptr.To("OSDisk" + spec.VMName),
					ManagedDisk: &armcompute.ManagedDiskParameters{
						ID: ptr.To("ManagedID" + spec.VMName),
						DiskEncryptionSet: &armcompute.DiskEncryptionSetParameters{
							ID: ptr.To("DiskEncryptionSetID" + spec.VMName),
						},
					},
				},
				DataDisks: []*armcompute.DataDisk{
					{
						Lun:         ptr.To(int32(1)),
						Name:        ptr.To("dataDisk" + spec.VMName),
						ManagedDisk: &armcompute.ManagedDiskParameters{ID: ptr.To("uri")},
					},
				},
			},
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: to.Ptr(armcompute.VirtualMachineSizeTypesStandardD2SV3),
			},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
					{
						ID: ptr.To(spec.NicID),
					},
				},
			},
		},
		Zones:    spec.Zones,
		Location: ptr.To("EastUS"),
	}
}

func generateVmssFlexTestVMWithOnlyInstanceView(spec VmssFlexTestVMSpec) (testVMWithOnlyInstanceView *armcompute.VirtualMachine) {
	return &armcompute.VirtualMachine{
		Name: ptr.To(spec.VMName),
		ID:   ptr.To(spec.VMID),
		Properties: &armcompute.VirtualMachineProperties{
			InstanceView: &armcompute.VirtualMachineInstanceView{
				PlatformFaultDomain: spec.PlatformFaultDomain,
				Statuses:            spec.Status,
			},
		},
	}
}

func generateVmssFlexTestVM(spec VmssFlexTestVMSpec) *armcompute.VirtualMachine {
	testVM := generateVmssFlexTestVMWithoutInstanceView(spec)
	testVM.Properties.InstanceView = generateVmssFlexTestVMWithOnlyInstanceView(spec).Properties.InstanceView
	return testVM
}

func TestGetNodeNameByVMName(t *testing.T) {

	testCases := []struct {
		description                    string
		vmName                         string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		expectedNodeName               string
		expectedErr                    error
	}{
		{
			description:                    "getNodeNameByVMName should return the nodeName of the corresponding vm by the vm name",
			vmName:                         "testvm1",
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			expectedNodeName:               "vmssflex1000001",
			expectedErr:                    nil,
		},
		{
			description:                    "getNodeVmssFlexID should throw InstanceNotFound error if the VM cannot be found",
			vmName:                         nonExistingNodeName,
			testVMListWithoutInstanceView:  []*armcompute.VirtualMachine{},
			testVMListWithOnlyInstanceView: []*armcompute.VirtualMachine{},
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

			nodeName, err := fs.getNodeNameByVMName(context.TODO(), tc.vmName)
			assert.Equal(t, tc.expectedErr, err, tc.description)
			assert.Equal(t, tc.expectedNodeName, nodeName, tc.description)
		})
	}
}

func TestGetNodeVmssFlexID(t *testing.T) {

	testCases := []struct {
		description                    string
		nodeName                       string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		expectedVmssFlexID             string
		expectedErr                    error
	}{
		{
			description:                    "getNodeVmssFlexID should return the VmssFlex ID that the node belongs to",
			nodeName:                       "vmssflex1000001",
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			expectedVmssFlexID:             "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1",
			expectedErr:                    nil,
		},
		{
			description:                    "getNodeVmssFlexID should throw InstanceNotFound error if the VM cannot be found",
			nodeName:                       "NonExistingNodeName",
			testVMListWithoutInstanceView:  []*armcompute.VirtualMachine{},
			testVMListWithOnlyInstanceView: []*armcompute.VirtualMachine{},
			vmListErr:                      nil,
			expectedVmssFlexID:             "",
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

			vmssFlexID, err := fs.getNodeVmssFlexID(context.TODO(), tc.nodeName)
			assert.Equal(t, tc.expectedErr, err, tc.description)
			assert.Equal(t, tc.expectedVmssFlexID, vmssFlexID, tc.description)
		})
	}
}

func TestGetVmssFlexVM(t *testing.T) {
	testCases := []struct {
		description                    string
		nodeName                       string
		testVM                         *armcompute.VirtualMachine
		vmGetErr                       error
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		expectedVmssFlexVM             *armcompute.VirtualMachine
		expectedErr                    error
	}{
		{
			description:                    "getVmssFlexVM should return the VmssFlex VM",
			nodeName:                       "vmssflex1000001",
			testVM:                         testVMWithoutInstanceView1,
			vmGetErr:                       nil,
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			expectedVmssFlexVM:             generateVmssFlexTestVM(testVM1Spec),
			expectedErr:                    nil,
		},
		{
			description:                    "getVmssFlexVM should throw InstanceNotFound error if the VM cannot be found",
			nodeName:                       "vmssflex1000001",
			testVM:                         &armcompute.VirtualMachine{},
			vmGetErr:                       &azcore.ResponseError{StatusCode: http.StatusNotFound},
			testVMListWithoutInstanceView:  []*armcompute.VirtualMachine{},
			testVMListWithOnlyInstanceView: []*armcompute.VirtualMachine{},
			vmListErr:                      nil,
			expectedVmssFlexVM:             nil,
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
		{
			description:                    "getVmssFlexVM should throw InstanceNotFound error if the VM is removed from VMSS Flex",
			nodeName:                       "vmssflex1000001",
			testVM:                         testVMWithoutInstanceView1,
			vmGetErr:                       nil,
			testVMListWithoutInstanceView:  []*armcompute.VirtualMachine{testVMWithoutInstanceView2},
			testVMListWithOnlyInstanceView: []*armcompute.VirtualMachine{testVMWithOnlyInstanceView2},
			vmListErr:                      nil,
			expectedVmssFlexVM:             nil,
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

			vmssFlexVM, err := fs.getVmssFlexVM(context.TODO(), tc.nodeName, azcache.CacheReadTypeDefault)
			if tc.expectedErr != nil {
				assert.Equal(t, tc.expectedErr, err, tc.description)
			}
			if tc.expectedVmssFlexVM != nil {
				assert.Equal(t, *tc.expectedVmssFlexVM, *vmssFlexVM, tc.description)
			} else {
				assert.Nil(t, vmssFlexVM, tc.description)
			}

		})
	}

}

func TestGetVmssFlexByVmssFlexID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description      string
		vmssFlexID       string
		testVmssFlexList []*armcompute.VirtualMachineScaleSet
		vmssFlexListErr  error
		expectedVmssFlex *armcompute.VirtualMachineScaleSet
		expectedErr      error
	}{
		{
			description:      "getVmssFlexByVmssFlexID should return the corresponding vmssFlex by its ID",
			vmssFlexID:       "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1",
			testVmssFlexList: testVmssFlexList,
			vmssFlexListErr:  nil,
			expectedVmssFlex: testVmssFlex1,
			expectedErr:      nil,
		},
		{
			description:      "getVmssFlexByVmssFlexID should return cloudprovider.InstanceNotFound if there's no matching VMSS",
			vmssFlexID:       "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1",
			testVmssFlexList: []*armcompute.VirtualMachineScaleSet{},
			vmssFlexListErr:  nil,
			expectedVmssFlex: nil,
			expectedErr:      cloudprovider.InstanceNotFound,
		},
		{
			description:      "getVmssFlexByVmssFlexID  should report an error if there's something wrong during an api call",
			vmssFlexID:       "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1",
			testVmssFlexList: []*armcompute.VirtualMachineScaleSet{},
			vmssFlexListErr:  &azcore.ResponseError{ErrorCode: "error during vmss list"},
			expectedVmssFlex: nil,
			expectedErr:      fmt.Errorf("error during vmss list"),
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(tc.testVmssFlexList, tc.vmssFlexListErr).AnyTimes()

		vmssFlex, err := fs.getVmssFlexByVmssFlexID(context.TODO(), tc.vmssFlexID, azcache.CacheReadTypeDefault)
		if tc.expectedErr != nil {
			assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
		}
		assert.Equal(t, tc.expectedVmssFlex, vmssFlex, tc.description)
	}
}

func TestGetVmssFlexIDByName(t *testing.T) {

	testCases := []struct {
		description        string
		vmssFlexName       string
		testVmssFlexList   []*armcompute.VirtualMachineScaleSet
		vmssFlexListErr    error
		expectedVmssFlexID string
		expectedErr        error
	}{
		{
			description:        "getVmssFlexIDByName should return the corresponding vmssFlex by its ID",
			vmssFlexName:       "vmssflex1",
			testVmssFlexList:   testVmssFlexList,
			vmssFlexListErr:    nil,
			expectedVmssFlexID: "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1",
			expectedErr:        nil,
		},
		{
			description:        "getVmssFlexIDByName should return cloudprovider.InstanceNotFound if there's no matching VMSS",
			vmssFlexName:       "vmssflex1",
			testVmssFlexList:   []*armcompute.VirtualMachineScaleSet{},
			vmssFlexListErr:    nil,
			expectedVmssFlexID: "",
			expectedErr:        cloudprovider.InstanceNotFound,
		},
		{
			description:        "getVmssFlexIDByName should report an error if there's something wrong during an api call",
			vmssFlexName:       "vmssflex1",
			testVmssFlexList:   []*armcompute.VirtualMachineScaleSet{},
			vmssFlexListErr:    &azcore.ResponseError{ErrorCode: "error during vmss list"},
			expectedVmssFlexID: "",
			expectedErr:        fmt.Errorf("error during vmss list"),
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
			mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(tc.testVmssFlexList, tc.vmssFlexListErr).AnyTimes()

			vmssFlexID, err := fs.getVmssFlexIDByName(context.TODO(), tc.vmssFlexName)

			assert.Equal(t, tc.expectedVmssFlexID, vmssFlexID, tc.description)
			if tc.expectedErr != nil {
				assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
			}
		})
	}
}

func TestGetVmssFlexByName(t *testing.T) {

	testCases := []struct {
		description      string
		vmssFlexName     string
		testVmssFlexList []*armcompute.VirtualMachineScaleSet
		vmssFlexListErr  error
		expectedVmssFlex *armcompute.VirtualMachineScaleSet
		expectedErr      error
	}{
		{
			description:      "getVmssFlexByName should return the corresponding vmssFlex by its ID",
			vmssFlexName:     "vmssflex1",
			testVmssFlexList: testVmssFlexList,
			vmssFlexListErr:  nil,
			expectedVmssFlex: testVmssFlex1,
			expectedErr:      nil,
		},
		{
			description:      "getVmssFlexByName should return cloudprovider.InstanceNotFound if there's no matching VMSS",
			vmssFlexName:     "vmssflex1",
			testVmssFlexList: []*armcompute.VirtualMachineScaleSet{},
			vmssFlexListErr:  nil,
			expectedVmssFlex: nil,
			expectedErr:      cloudprovider.InstanceNotFound,
		},
		{
			description:      "getVmssFlexByName should report an error if there's something wrong during an api call",
			vmssFlexName:     "vmssflex1",
			testVmssFlexList: []*armcompute.VirtualMachineScaleSet{},
			vmssFlexListErr:  &azcore.ResponseError{ErrorCode: "error during vmss list"},
			expectedVmssFlex: nil,
			expectedErr:      fmt.Errorf("error during vmss list"),
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
			mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(tc.testVmssFlexList, tc.vmssFlexListErr).AnyTimes()

			vmssFlex, err := fs.getVmssFlexByName(context.TODO(), tc.vmssFlexName)

			assert.Equal(t, tc.expectedVmssFlex, vmssFlex, tc.description)
			if tc.expectedErr != nil {
				assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
			}
		})
	}

}

func TestGetVmssFlexByNodeName(t *testing.T) {

	testCases := []struct {
		description                    string
		nodeName                       string
		testVM                         *armcompute.VirtualMachine
		vmGetErr                       error
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		testVmssFlexList               []*armcompute.VirtualMachineScaleSet
		vmssFlexListErr                error
		expectedVmssFlex               *armcompute.VirtualMachineScaleSet
		expectedErr                    error
	}{
		{
			description:                    "getVmssFlexByName should return the VmssFlex ID that the node belongs to",
			nodeName:                       "vmssflex1000001",
			testVM:                         testVMWithoutInstanceView1,
			vmGetErr:                       nil,
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			testVmssFlexList:               testVmssFlexList,
			vmssFlexListErr:                nil,
			expectedVmssFlex:               testVmssFlex1,
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

			mockVMClient := fs.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
			mockVMClient.EXPECT().Get(gomock.Any(), fs.ResourceGroup, tc.nodeName, gomock.Any()).Return(tc.testVM, tc.vmGetErr).AnyTimes()
			mockVMClient.EXPECT().ListVmssFlexVMsWithOutInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
			mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()
			mockVMSSClient := fs.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(tc.testVmssFlexList, tc.vmssFlexListErr).AnyTimes()

			vmssFlex, err := fs.getVmssFlexByNodeName(context.TODO(), tc.nodeName, azcache.CacheReadTypeDefault)
			assert.Equal(t, tc.expectedErr, err, tc.description)
			assert.Equal(t, tc.expectedVmssFlex, vmssFlex, tc.description)
		})
	}

}
