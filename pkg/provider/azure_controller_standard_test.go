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
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

var (
	fakeCacheTTL = 2 * time.Second
)

func TestStandardAttachDisk(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		desc         string
		nodeName     types.NodeName
		isAttachFail bool
		expectedErr  bool
	}{
		{
			desc:        "an error shall be returned if there's no corresponding vms",
			nodeName:    "vm2",
			expectedErr: true,
		},
		{
			desc:        "no error shall be returned if everything's good",
			nodeName:    "vm1",
			expectedErr: false,
		},
		{
			desc:        "no error shall be returned if everything's good with non managed disk",
			nodeName:    "vm1",
			expectedErr: false,
		},
		{
			desc:         "an error shall be returned if update attach disk failed",
			nodeName:     "vm1",
			isAttachFail: true,
			expectedErr:  true,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		vmSet := testCloud.VMSet
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			vm.StorageProfile = &compute.StorageProfile{
				OsDisk: &compute.OSDisk{
					Name: to.StringPtr("osdisk1"),
					ManagedDisk: &compute.ManagedDiskParameters{
						ID: to.StringPtr("ManagedID"),
						DiskEncryptionSet: &compute.DiskEncryptionSetParameters{
							ID: to.StringPtr("DiskEncryptionSetID"),
						},
					},
				},
				DataDisks: &[]compute.DataDisk{},
			}
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, "vm2", gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		if test.isAttachFail {
			mockVMsClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		} else {
			mockVMsClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
		}

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
		_, err := vmSet.AttachDisk(ctx, test.nodeName, diskMap)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, err: %v", i, test.desc, err)
	}
}

func TestStandardDetachDisk(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		desc          string
		nodeName      types.NodeName
		disks         []string
		isDetachFail  bool
		expectedError bool
	}{
		{
			desc:          "no error shall be returned if there's no corresponding vm",
			nodeName:      "vm2",
			expectedError: false,
		},
		{
			desc:          "no error shall be returned if there's no corresponding disk",
			nodeName:      "vm1",
			disks:         []string{"diskx"},
			expectedError: false,
		},
		{
			desc:          "no error shall be returned if there's a corresponding disk",
			nodeName:      "vm1",
			disks:         []string{"disk1"},
			expectedError: false,
		},
		{
			desc:          "no error shall be returned if there's 2 corresponding disks",
			nodeName:      "vm1",
			disks:         []string{"disk1", "disk2"},
			expectedError: false,
		},
		{
			desc:          "an error shall be returned if detach disk failed",
			nodeName:      "vm1",
			isDetachFail:  true,
			expectedError: true,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		vmSet := testCloud.VMSet
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, "vm2", gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		if test.isDetachFail {
			mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(&retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		} else {
			mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, "vm1", gomock.Any(), "detach_disk").Return(nil).AnyTimes()
		}

		diskMap := map[string]string{}
		for _, diskName := range test.disks {
			diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
				testCloud.SubscriptionID, testCloud.ResourceGroup, diskName)
			diskMap[diskURI] = diskName
		}
		err := vmSet.DetachDisk(ctx, test.nodeName, diskMap)
		assert.Equal(t, test.expectedError, err != nil, "TestCase[%d]: %s", i, test.desc)
		if !test.expectedError && len(test.disks) > 0 {
			dataDisks, _, err := vmSet.GetDataDisks(test.nodeName, azcache.CacheReadTypeDefault)
			assert.Equal(t, true, len(dataDisks) == 3, "TestCase[%d]: %s, err: %v", i, test.desc, err)
		}
	}
}

func TestStandardUpdateVM(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		desc          string
		nodeName      types.NodeName
		diskName      string
		isDetachFail  bool
		expectedError bool
	}{
		{
			desc:          "no error shall be returned if there's no corresponding vm",
			nodeName:      "vm2",
			expectedError: false,
		},
		{
			desc:          "no error shall be returned if there's no corresponding disk",
			nodeName:      "vm1",
			diskName:      "disk2",
			expectedError: false,
		},
		{
			desc:          "no error shall be returned if there's a corresponding disk",
			nodeName:      "vm1",
			diskName:      "disk1",
			expectedError: false,
		},
		{
			desc:          "an error shall be returned if detach disk failed",
			nodeName:      "vm1",
			isDetachFail:  true,
			expectedError: true,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		vmSet := testCloud.VMSet
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, "vm2", gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		if test.isDetachFail {
			mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(&retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		} else {
			mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		}

		err := vmSet.UpdateVM(ctx, test.nodeName)
		assert.Equal(t, test.expectedError, err != nil, "TestCase[%d]: %s", i, test.desc)
		if !test.expectedError && test.diskName != "" {
			dataDisks, _, err := vmSet.GetDataDisks(test.nodeName, azcache.CacheReadTypeDefault)
			assert.Equal(t, true, len(dataDisks) == 3, "TestCase[%d]: %s, err: %v", i, test.desc, err)
		}
	}
}

func TestGetDataDisks(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	var testCases = []struct {
		desc              string
		nodeName          types.NodeName
		crt               azcache.AzureCacheReadType
		isDataDiskNull    bool
		expectedError     bool
		expectedDataDisks []compute.DataDisk
	}{
		{
			desc:              "an error shall be returned if there's no corresponding vm",
			nodeName:          "vm2",
			expectedDataDisks: nil,
			expectedError:     true,
			crt:               azcache.CacheReadTypeDefault,
		},
		{
			desc:     "correct list of data disks shall be returned if everything is good",
			nodeName: "vm1",
			expectedDataDisks: []compute.DataDisk{
				{
					Lun:  to.Int32Ptr(0),
					Name: to.StringPtr("disk1"),
				},
				{
					Lun:  to.Int32Ptr(1),
					Name: to.StringPtr("disk2"),
				},
				{
					Lun:  to.Int32Ptr(2),
					Name: to.StringPtr("disk3"),
				},
			},
			expectedError: false,
			crt:           azcache.CacheReadTypeDefault,
		},
		{
			desc:     "correct list of data disks shall be returned if everything is good",
			nodeName: "vm1",
			expectedDataDisks: []compute.DataDisk{
				{
					Lun:  to.Int32Ptr(0),
					Name: to.StringPtr("disk1"),
				},
				{
					Lun:  to.Int32Ptr(1),
					Name: to.StringPtr("disk2"),
				},
				{
					Lun:  to.Int32Ptr(2),
					Name: to.StringPtr("disk3"),
				},
			},
			expectedError: false,
			crt:           azcache.CacheReadTypeUnsafe,
		},
		{
			desc:              "nil shall be returned if DataDisk is null",
			nodeName:          "vm1",
			isDataDiskNull:    true,
			expectedDataDisks: nil,
			expectedError:     false,
			crt:               azcache.CacheReadTypeDefault,
		},
	}
	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		vmSet := testCloud.VMSet
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			if test.isDataDiskNull {
				vm.StorageProfile = &compute.StorageProfile{}
			}
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, gomock.Not("vm1"), gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		dataDisks, _, err := vmSet.GetDataDisks(test.nodeName, test.crt)
		assert.Equal(t, test.expectedDataDisks, dataDisks, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedError, err != nil, "TestCase[%d]: %s", i, test.desc)

		if test.crt == azcache.CacheReadTypeUnsafe {
			time.Sleep(fakeCacheTTL)
			dataDisks, _, err := vmSet.GetDataDisks(test.nodeName, test.crt)
			assert.Equal(t, test.expectedDataDisks, dataDisks, "TestCase[%d]: %s", i, test.desc)
			assert.Equal(t, test.expectedError, err != nil, "TestCase[%d]: %s", i, test.desc)
		}
	}
}
