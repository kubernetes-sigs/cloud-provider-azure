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
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-12-01/compute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestAttachDiskWithVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		description                    string
		nodeName                       types.NodeName
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
		vmListErr                      error
		vmssFlexVMUpdateError          *retry.Error
		expectedErr                    error
	}{
		{
			description:                    "AttachDisk should work as expected with managed disk",
			nodeName:                       "vmssflex1000001",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			expectedErr:                    nil,
		},
		{
			description:                    "AttachDisk should should throw InstanceNotFound error if the VM cannot be found",
			nodeName:                       types.NodeName(nonExistingNodeName),
			testVMListWithoutInstanceView:  []compute.VirtualMachine{},
			testVMListWithOnlyInstanceView: []compute.VirtualMachine{},
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
		{
			description:                    "AttachDisk should return error if update VM fails",
			nodeName:                       "vmssflex1000001",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound},
			expectedErr:                    fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 404, RawError: instance not found"),
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

		mockVMClient.EXPECT().UpdateAsync(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, tc.vmssFlexVMUpdateError).AnyTimes()

		options := AttachDiskOptions{
			lun:                     0,
			diskName:                "",
			cachingMode:             compute.CachingTypesReadOnly,
			diskEncryptionSetID:     "",
			writeAcceleratorEnabled: false,
		}
		diskMap := map[string]*AttachDiskOptions{
			"uri": &options,
		}

		_, err = fs.AttachDisk(ctx, tc.nodeName, diskMap)
		if tc.expectedErr == nil {
			assert.NoError(t, err)
		} else {
			assert.EqualError(t, err, tc.expectedErr.Error(), tc.description)
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
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
		vmListErr                      error
		vmssFlexVMUpdateError          *retry.Error
		diskMap                        map[string]string
		expectedErr                    error
	}{
		{
			description:                    "DetachDisk should work as expected with managed disk",
			nodeName:                       "vmssflex1000001",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			diskMap:                        map[string]string{"diskUri1": "dataDisktestvm1"},
			expectedErr:                    nil,
		},
		{
			description:                    "AttachDisk should should do nothing if the VM cannot be found",
			nodeName:                       types.NodeName(nonExistingNodeName),
			testVMListWithoutInstanceView:  []compute.VirtualMachine{},
			testVMListWithOnlyInstanceView: []compute.VirtualMachine{},
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			diskMap:                        map[string]string{"diskUri1": "dataDisktestvm1"},
			expectedErr:                    nil,
		},
		{
			description:                    "DetachDisk should should do nothing if there's a corresponding disk",
			nodeName:                       "vmssflex1000001",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			diskMap:                        map[string]string{"diskUri1": "dataDisktestvm3"},
			expectedErr:                    nil,
		},
		{
			description:                    "AttachDisk should return error if update VM fails",
			nodeName:                       "vmssflex1000001",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound},
			diskMap:                        map[string]string{"diskUri1": "dataDisktestvm1"},
			expectedErr:                    fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 404, RawError: instance not found"),
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

		mockVMClient.EXPECT().Update(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), "detach_disk").Return(tc.vmssFlexVMUpdateError).AnyTimes()

		err = fs.DetachDisk(ctx, tc.nodeName, tc.diskMap)
		if tc.expectedErr == nil {
			assert.NoError(t, err)
		} else {
			assert.EqualError(t, err, tc.expectedErr.Error(), tc.description)
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
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
		vmListErr                      error
		vmssFlexVMUpdateError          *retry.Error
		expectedErr                    error
	}{
		{
			description:                    "UpdateVM should work as expected if vm client update succeeds",
			nodeName:                       "vmssflex1000001",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          nil,
			expectedErr:                    nil,
		},
		{
			description:                    "UpdateVM should return error if update VM fails",
			nodeName:                       "vmssflex1000001",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			vmssFlexVMUpdateError:          &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound},
			expectedErr:                    fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 404, RawError: instance not found"),
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
		mockVMClient.EXPECT().Update(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), "update_vm").Return(tc.vmssFlexVMUpdateError).AnyTimes()

		err = fs.UpdateVM(ctx, tc.nodeName)

		if tc.expectedErr == nil {
			assert.NoError(t, err)
		} else {
			assert.EqualError(t, err, tc.expectedErr.Error(), tc.description)
		}
	}

}

func TestGetDataDisksWithVmssFlex(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       types.NodeName
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
		vmListErr                      error
		expectedDataDisks              []compute.DataDisk
		expectedErr                    error
	}{
		{
			description:                    "GetDataDisks should work as expected with managed disk",
			nodeName:                       "vmssflex1000001",
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedDataDisks: []compute.DataDisk{
				{
					Lun:  to.Int32Ptr(1),
					Name: to.StringPtr("dataDisktestvm1"),
				},
			},
			expectedErr: nil,
		},
		{
			description:                    "GetDataDisks should should throw InstanceNotFound error if the VM cannot be found",
			nodeName:                       types.NodeName(nonExistingNodeName),
			testVMListWithoutInstanceView:  []compute.VirtualMachine{},
			testVMListWithOnlyInstanceView: []compute.VirtualMachine{},
			vmListErr:                      nil,
			expectedDataDisks:              nil,
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

		dataDisks, _, err := fs.GetDataDisks(tc.nodeName, azcache.CacheReadTypeDefault)
		assert.Equal(t, tc.expectedDataDisks, dataDisks)
		if tc.expectedErr != nil {
			assert.EqualError(t, err, tc.expectedErr.Error(), tc.description)
		}
	}

}
