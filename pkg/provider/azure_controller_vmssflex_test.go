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
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient/mock_virtualmachinescalesetclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
)

func TestAttachDiskWithVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		description                    string
		nodeName                       types.NodeName
		vmName                         string
		inconsistentLUN                bool
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		vmssFlexVMUpdateError          error
		expectedErr                    error
	}{
		{
			description:                    "AttachDisk should work as expected with managed disk",
			nodeName:                       types.NodeName(testVM1Spec.ComputerName),
			vmName:                         testVM1Spec.VMName,
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			expectedErr:                    nil,
		},
		{
			description:                    "AttachDisk should should throw InstanceNotFound error if the VM cannot be found",
			nodeName:                       types.NodeName(nonExistingNodeName),
			testVMListWithoutInstanceView:  []*armcompute.VirtualMachine{},
			testVMListWithOnlyInstanceView: []*armcompute.VirtualMachine{},
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
		{
			description:                    "AttachDisk should return error if update VM fails",
			nodeName:                       types.NodeName(testVM1Spec.ComputerName),
			vmName:                         testVM1Spec.VMName,
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()},
			expectedErr:                    fmt.Errorf("instance not found"),
		},
		{
			description:                    "error should be returned when disk lun is inconsistent",
			nodeName:                       types.NodeName(testVM1Spec.ComputerName),
			vmName:                         testVM1Spec.VMName,
			inconsistentLUN:                true,
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			expectedErr:                    fmt.Errorf("disk(uri) already attached to node(vmssflex1000001) on LUN(1), but target LUN is 63"),
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

		mockVMClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), tc.vmName, gomock.Any()).Return(nil, tc.vmssFlexVMUpdateError).AnyTimes()
		options := AttachDiskOptions{
			Lun:                     1,
			DiskName:                "diskname",
			CachingMode:             armcompute.CachingTypesReadOnly,
			DiskEncryptionSetID:     "",
			WriteAcceleratorEnabled: false,
		}
		if tc.inconsistentLUN {
			options.Lun = 63
		}
		diskMap := map[string]*AttachDiskOptions{
			"uri": &options,
		}

		err = fs.AttachDisk(ctx, tc.nodeName, diskMap)
		if tc.expectedErr == nil {
			assert.NoError(t, err)
		} else {
			assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
		}
	}
}

func TestDettachDiskWithVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		description                    string
		nodeName                       types.NodeName
		vmName                         string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		forceDetach                    bool
		vmListErr                      error
		vmssFlexVMUpdateError          error
		diskMap                        map[string]string
		expectedErr                    error
	}{
		{
			description:                    "DetachDisk should work as expected with managed disk",
			nodeName:                       types.NodeName(testVM1Spec.ComputerName),
			vmName:                         testVM1Spec.VMName,
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			diskMap:                        map[string]string{"diSKUri1": "dataDisktestvm1"},
			expectedErr:                    nil,
		},
		{
			description:                    "DetachDisk should work as expected with managed disk with forceDetach",
			nodeName:                       types.NodeName(testVM1Spec.ComputerName),
			vmName:                         testVM1Spec.VMName,
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			forceDetach:                    true,
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			diskMap:                        map[string]string{"diSKUri1": "dataDisktestvm1"},
			expectedErr:                    nil,
		},
		{
			description:                    "AttachDisk should should do nothing if the VM cannot be found",
			nodeName:                       types.NodeName(nonExistingNodeName),
			testVMListWithoutInstanceView:  []*armcompute.VirtualMachine{},
			testVMListWithOnlyInstanceView: []*armcompute.VirtualMachine{},
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			diskMap:                        map[string]string{"diSKUri1": "dataDisktestvm1"},
			expectedErr:                    nil,
		},
		{
			description:                    "DetachDisk should should do nothing if there's a corresponding disk",
			nodeName:                       types.NodeName(testVM1Spec.ComputerName),
			vmName:                         testVM1Spec.VMName,
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			diskMap:                        map[string]string{"diSKUri1": "dataDisktestvm3"},
			expectedErr:                    nil,
		},
		{
			description:                    "AttachDisk should return error if update VM fails",
			nodeName:                       types.NodeName(testVM1Spec.ComputerName),
			vmName:                         testVM1Spec.VMName,
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()},
			diskMap:                        map[string]string{"diSKUri1": "dataDisktestvm1"},
			expectedErr:                    fmt.Errorf("instance not found"),
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

		mockVMClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), tc.vmName, gomock.Any()).Return(nil, tc.vmssFlexVMUpdateError).AnyTimes()

		err = fs.DetachDisk(ctx, tc.nodeName, tc.diskMap, tc.forceDetach)
		if tc.expectedErr == nil {
			assert.NoError(t, err)
		} else {
			assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
		}
	}

}

func TestUpdateVMWithVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		description                    string
		nodeName                       types.NodeName
		vmName                         string
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		vmssFlexVMUpdateError          error
		expectedErr                    error
	}{
		{
			description:                    "UpdateVM should work as expected if vm client update succeeds",
			nodeName:                       types.NodeName(testVM1Spec.ComputerName),
			vmName:                         testVM1Spec.VMName,
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			expectedErr:                    nil,
		},
		{
			description:                    "UpdateVM should return error if update VM fails",
			nodeName:                       types.NodeName(testVM1Spec.ComputerName),
			vmName:                         testVM1Spec.VMName,
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()},
			expectedErr:                    fmt.Errorf("instance not found"),
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

		mockVMClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), tc.vmName, gomock.Any()).Return(nil, tc.vmssFlexVMUpdateError).AnyTimes()
		mockVMClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), tc.vmName, gomock.Any()).Return(nil, tc.vmssFlexVMUpdateError).AnyTimes()

		err = fs.UpdateVM(ctx, tc.nodeName)

		if tc.expectedErr == nil {
			assert.NoError(t, err)
		} else {
			assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
		}
	}

}

func TestGetDataDisksWithVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       types.NodeName
		testVMListWithoutInstanceView  []*armcompute.VirtualMachine
		testVMListWithOnlyInstanceView []*armcompute.VirtualMachine
		vmListErr                      error
		expectedDataDisks              []*armcompute.DataDisk
		expectedErr                    error
	}{
		{
			description:                    "GetDataDisks should work as expected with managed disk",
			nodeName:                       "vmssflex1000001",
			testVMListWithoutInstanceView:  generateTestVMListWithoutInstanceView(),
			testVMListWithOnlyInstanceView: generateTestVMListWithOnlyInstanceView(),
			vmListErr:                      nil,
			expectedDataDisks: []*armcompute.DataDisk{
				{
					Lun:         ptr.To(int32(1)),
					Name:        ptr.To("dataDisktestvm1"),
					ManagedDisk: &armcompute.ManagedDiskParameters{ID: ptr.To("uri")},
				},
			},
			expectedErr: nil,
		},
		{
			description:                    "GetDataDisks should should throw InstanceNotFound error if the VM cannot be found",
			nodeName:                       types.NodeName(nonExistingNodeName),
			testVMListWithoutInstanceView:  []*armcompute.VirtualMachine{},
			testVMListWithOnlyInstanceView: []*armcompute.VirtualMachine{},
			vmListErr:                      nil,
			expectedDataDisks:              nil,
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

		dataDisks, _, err := fs.GetDataDisks(context.TODO(), tc.nodeName, azcache.CacheReadTypeDefault)
		assert.Equal(t, len(tc.expectedDataDisks), len(dataDisks))
		if tc.expectedErr != nil {
			assert.Contains(t, err.Error(), tc.expectedErr.Error(), tc.description)
		}
	}
}

func TestVMSSFlexUpdateCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	fs, err := NewTestFlexScaleSet(ctrl)
	assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

	testCases := []struct {
		description string
		nodeName    string
		vm          *armcompute.VirtualMachine
		expectedErr error
	}{
		{
			description: "vm is nil",
			nodeName:    "vmssflex1000001",
			expectedErr: fmt.Errorf("vm is nil"),
		},
		{
			description: "vm.Properties is nil",
			nodeName:    "vmssflex1000001",
			vm:          &armcompute.VirtualMachine{Name: ptr.To("vmssflex1000001")},
			expectedErr: fmt.Errorf("vm.Properties is nil"),
		},
		{
			description: "vm.Properties.OSProfile.ComputerName is nil",
			nodeName:    "vmssflex1000001",
			vm: &armcompute.VirtualMachine{
				Name:       ptr.To("vmssflex1000001"),
				Properties: &armcompute.VirtualMachineProperties{},
			},
			expectedErr: fmt.Errorf("vm.Properties.OSProfile.ComputerName is nil"),
		},
		{
			description: "vm.Properties.OSProfile.ComputerName is nil",
			nodeName:    "vmssflex1000001",
			vm: &armcompute.VirtualMachine{
				Name: ptr.To("vmssflex1000001"),
				Properties: &armcompute.VirtualMachineProperties{
					OSProfile: &armcompute.OSProfile{},
				},
			},
			expectedErr: fmt.Errorf("vm.Properties.OSProfile.ComputerName is nil"),
		},
	}

	for _, test := range testCases {
		err = fs.updateCache(context.TODO(), test.nodeName, test.vm)
		assert.Equal(t, test.expectedErr, err, test.description)
	}
}
