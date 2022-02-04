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

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssvmclient/mockvmssvmclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestAttachDiskWithVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	fakeStatusNotFoundVMSSName := types.NodeName("FakeStatusNotFoundVMSSName")
	testCases := []struct {
		desc           string
		vmssName       types.NodeName
		vmssvmName     types.NodeName
		disks          []string
		vmList         map[string]string
		vmssVMList     []string
		expectedErr    bool
		expectedErrMsg error
	}{
		{
			desc:        "no error shall be returned if everything is good with one managed disk",
			vmssVMList:  []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:    "vmss00",
			vmssvmName:  "vmss00-vm-000000",
			disks:       []string{"disk-name"},
			expectedErr: false,
		},
		{
			desc:        "no error shall be returned if everything is good with 2 managed disks",
			vmssVMList:  []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:    "vmss00",
			vmssvmName:  "vmss00-vm-000000",
			disks:       []string{"disk-name", "disk-name2"},
			expectedErr: false,
		},
		{
			desc:        "no error shall be returned if everything is good with non-managed disk",
			vmssVMList:  []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:    "vmss00",
			vmssvmName:  "vmss00-vm-000000",
			disks:       []string{"disk-name"},
			expectedErr: false,
		},
	}

	for i, test := range testCases {
		scaleSetName := string(test.vmssName)
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.desc)
		testCloud := ss.cloud
		testCloud.PrimaryScaleSetName = scaleSetName
		expectedVMSS := buildTestVMSSWithLB(scaleSetName, "vmss00-vm-", []string{testLBBackendpoolID0}, false)
		mockVMSSClient := testCloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()
		mockVMSSClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, scaleSetName).Return(expectedVMSS, nil).MaxTimes(1)
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(nil).MaxTimes(1)

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(testCloud, scaleSetName, "", 0, test.vmssVMList, "succeeded", false)
		for _, vmssvm := range expectedVMSSVMs {
			vmssvm.StorageProfile = &compute.StorageProfile{
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
		}
		mockVMSSVMClient := testCloud.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()
		if scaleSetName == string(fakeStatusNotFoundVMSSName) {
			mockVMSSVMClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		} else {
			mockVMSSVMClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
		}

		diskMap := map[string]*AttachDiskOptions{}
		for i, diskName := range test.disks {
			options := AttachDiskOptions{
				lun:                     int32(i),
				diskName:                diskName,
				cachingMode:             compute.CachingTypesReadWrite,
				diskEncryptionSetID:     "",
				writeAcceleratorEnabled: true,
			}

			diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
				testCloud.SubscriptionID, testCloud.ResourceGroup, diskName)
			diskMap[diskURI] = &options
		}
		_, err = ss.AttachDisk(ctx, test.vmssvmName, diskMap)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, return error: %v", i, test.desc, err)
		if test.expectedErr {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), "TestCase[%d]: %s, expected error: %v, return error: %v", i, test.desc, test.expectedErrMsg, err)
		}
	}
}

func TestDetachDiskWithVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	fakeStatusNotFoundVMSSName := types.NodeName("FakeStatusNotFoundVMSSName")
	diskName := "disk-name"
	testCases := []struct {
		desc           string
		vmList         map[string]string
		vmssVMList     []string
		vmssName       types.NodeName
		vmssvmName     types.NodeName
		disks          []string
		expectedErr    bool
		expectedErrMsg error
	}{
		{
			desc:           "an error shall be returned if it is invalid vmss name",
			vmssVMList:     []string{"vmss-vm-000001"},
			vmssName:       "vm1",
			vmssvmName:     "vm1",
			disks:          []string{diskName},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("not a vmss instance"),
		},
		{
			desc:        "no error shall be returned if everything is good",
			vmssVMList:  []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:    "vmss00",
			vmssvmName:  "vmss00-vm-000000",
			disks:       []string{diskName},
			expectedErr: false,
		},
		{
			desc:        "no error shall be returned if everything is good",
			vmssVMList:  []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:    "vmss00",
			vmssvmName:  "vmss00-vm-000000",
			disks:       []string{diskName, "disk2"},
			expectedErr: false,
		},
		{
			desc:           "an error shall be returned if response StatusNotFound",
			vmssVMList:     []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:       fakeStatusNotFoundVMSSName,
			vmssvmName:     "vmss00-vm-000000",
			disks:          []string{diskName},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 404, RawError: %w", fmt.Errorf("instance not found")),
		},
		{
			desc:        "no error shall be returned if everything is good and the attaching disk does not match data disk",
			vmssVMList:  []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:    "vmss00",
			vmssvmName:  "vmss00-vm-000000",
			disks:       []string{"disk-name-err"},
			expectedErr: false,
		},
	}

	for i, test := range testCases {
		scaleSetName := string(test.vmssName)
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.desc)
		testCloud := ss.cloud
		testCloud.PrimaryScaleSetName = scaleSetName
		expectedVMSS := buildTestVMSSWithLB(scaleSetName, "vmss00-vm-", []string{testLBBackendpoolID0}, false)
		mockVMSSClient := testCloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()
		mockVMSSClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, scaleSetName).Return(expectedVMSS, nil).MaxTimes(1)
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(nil).MaxTimes(1)

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(testCloud, scaleSetName, "", 0, test.vmssVMList, "succeeded", false)
		for _, vmssvm := range expectedVMSSVMs {
			vmssvm.StorageProfile = &compute.StorageProfile{
				OsDisk: &compute.OSDisk{
					Name: to.StringPtr("osdisk1"),
					ManagedDisk: &compute.ManagedDiskParameters{
						ID: to.StringPtr("ManagedID"),
						DiskEncryptionSet: &compute.DiskEncryptionSetParameters{
							ID: to.StringPtr("DiskEncryptionSetID"),
						},
					},
				},
				DataDisks: &[]compute.DataDisk{
					{
						Lun:  to.Int32Ptr(0),
						Name: to.StringPtr(diskName),
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
			}
		}
		mockVMSSVMClient := testCloud.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()
		if scaleSetName == string(fakeStatusNotFoundVMSSName) {
			mockVMSSVMClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(&retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		} else {
			mockVMSSVMClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		}

		diskMap := map[string]string{}
		for _, diskName := range test.disks {
			diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
				testCloud.SubscriptionID, testCloud.ResourceGroup, diskName)
			diskMap[diskURI] = diskName
		}
		err = ss.DetachDisk(ctx, test.vmssvmName, diskMap)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, err: %v", i, test.desc, err)
		if test.expectedErr {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), "TestCase[%d]: %s, expected error: %v, return error: %v", i, test.desc, test.expectedErrMsg, err)
		}

		if !test.expectedErr {
			dataDisks, _, err := ss.GetDataDisks(test.vmssvmName, azcache.CacheReadTypeDefault)
			assert.Equal(t, true, len(dataDisks) == 3, "TestCase[%d]: %s, actual data disk num: %d, err: %v", i, test.desc, len(dataDisks), err)
		}
	}
}

func TestUpdateVMWithVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	fakeStatusNotFoundVMSSName := types.NodeName("FakeStatusNotFoundVMSSName")
	diskName := "disk-name"
	testCases := []struct {
		desc           string
		vmList         map[string]string
		vmssVMList     []string
		vmssName       types.NodeName
		vmssvmName     types.NodeName
		existedDisk    compute.Disk
		expectedErr    bool
		expectedErrMsg error
	}{
		{
			desc:           "an error shall be returned if it is invalid vmss name",
			vmssVMList:     []string{"vmss-vm-000001"},
			vmssName:       "vm1",
			vmssvmName:     "vm1",
			existedDisk:    compute.Disk{Name: to.StringPtr(diskName)},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("not a vmss instance"),
		},
		{
			desc:        "no error shall be returned if everything is good",
			vmssVMList:  []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:    "vmss00",
			vmssvmName:  "vmss00-vm-000000",
			existedDisk: compute.Disk{Name: to.StringPtr(diskName)},
			expectedErr: false,
		},
		{
			desc:           "an error shall be returned if response StatusNotFound",
			vmssVMList:     []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:       fakeStatusNotFoundVMSSName,
			vmssvmName:     "vmss00-vm-000000",
			existedDisk:    compute.Disk{Name: to.StringPtr(diskName)},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 404, RawError: %w", fmt.Errorf("instance not found")),
		},
		{
			desc:        "no error shall be returned if everything is good and the attaching disk does not match data disk",
			vmssVMList:  []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:    "vmss00",
			vmssvmName:  "vmss00-vm-000000",
			existedDisk: compute.Disk{Name: to.StringPtr("disk-name-err")},
			expectedErr: false,
		},
	}

	for i, test := range testCases {
		scaleSetName := string(test.vmssName)
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.desc)
		testCloud := ss.cloud
		testCloud.PrimaryScaleSetName = scaleSetName
		expectedVMSS := buildTestVMSSWithLB(scaleSetName, "vmss00-vm-", []string{testLBBackendpoolID0}, false)
		mockVMSSClient := testCloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()
		mockVMSSClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, scaleSetName).Return(expectedVMSS, nil).MaxTimes(1)
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(nil).MaxTimes(1)

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(testCloud, scaleSetName, "", 0, test.vmssVMList, "succeeded", false)
		for _, vmssvm := range expectedVMSSVMs {
			vmssvm.StorageProfile = &compute.StorageProfile{
				OsDisk: &compute.OSDisk{
					Name: to.StringPtr("osdisk1"),
					ManagedDisk: &compute.ManagedDiskParameters{
						ID: to.StringPtr("ManagedID"),
						DiskEncryptionSet: &compute.DiskEncryptionSetParameters{
							ID: to.StringPtr("DiskEncryptionSetID"),
						},
					},
				},
				DataDisks: &[]compute.DataDisk{{
					Lun:  to.Int32Ptr(0),
					Name: to.StringPtr(diskName),
				}},
			}
		}
		mockVMSSVMClient := testCloud.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()
		if scaleSetName == string(fakeStatusNotFoundVMSSName) {
			mockVMSSVMClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(&retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		} else {
			mockVMSSVMClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		}

		err = ss.UpdateVM(ctx, test.vmssvmName)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, err: %v", i, test.desc, err)
		if test.expectedErr {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), "TestCase[%d]: %s, expected error: %v, return error: %v", i, test.desc, test.expectedErrMsg, err)
		}

		if !test.expectedErr {
			dataDisks, _, err := ss.GetDataDisks(test.vmssvmName, azcache.CacheReadTypeDefault)
			assert.Equal(t, true, len(dataDisks) == 1, "TestCase[%d]: %s, actual data disk num: %d, err: %v", i, test.desc, len(dataDisks), err)
		}
	}
}

func TestGetDataDisksWithVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	var testCases = []struct {
		desc              string
		crt               azcache.AzureCacheReadType
		nodeName          types.NodeName
		expectedDataDisks []compute.DataDisk
		isDataDiskNull    bool
		expectedErr       bool
		expectedErrMsg    error
	}{
		{
			desc:              "an error shall be returned if there's no corresponding vm",
			nodeName:          "vmss00-vm-000001",
			expectedDataDisks: nil,
			expectedErr:       true,
			expectedErrMsg:    fmt.Errorf("instance not found"),
			crt:               azcache.CacheReadTypeDefault,
		},
		{
			desc:     "correct list of data disks shall be returned if everything is good",
			nodeName: "vmss00-vm-000000",
			expectedDataDisks: []compute.DataDisk{
				{
					Lun:  to.Int32Ptr(0),
					Name: to.StringPtr("disk1"),
				},
			},
			expectedErr: false,
			crt:         azcache.CacheReadTypeDefault,
		},
		{
			desc:     "correct list of data disks shall be returned if everything is good",
			nodeName: "vmss00-vm-000000",
			expectedDataDisks: []compute.DataDisk{
				{
					Lun:  to.Int32Ptr(0),
					Name: to.StringPtr("disk1"),
				},
			},
			expectedErr: false,
			crt:         azcache.CacheReadTypeUnsafe,
		},
		{
			desc:              "nil shall be returned if DataDisk is null",
			nodeName:          "vmss00-vm-000000",
			isDataDiskNull:    true,
			expectedDataDisks: nil,
			expectedErr:       false,
			crt:               azcache.CacheReadTypeDefault,
		},
	}
	for i, test := range testCases {
		scaleSetName := string(test.nodeName)
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.desc)
		testCloud := ss.cloud
		testCloud.PrimaryScaleSetName = scaleSetName
		expectedVMSS := buildTestVMSSWithLB(scaleSetName, "vmss00-vm-", []string{testLBBackendpoolID0}, false)
		mockVMSSClient := testCloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()
		mockVMSSClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, scaleSetName).Return(expectedVMSS, nil).MaxTimes(1)
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(nil).MaxTimes(1)

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(testCloud, scaleSetName, "", 0, []string{"vmss00-vm-000000"}, "succeeded", false)
		if !test.isDataDiskNull {
			for _, vmssvm := range expectedVMSSVMs {
				vmssvm.StorageProfile = &compute.StorageProfile{
					DataDisks: &[]compute.DataDisk{{
						Lun:  to.Int32Ptr(0),
						Name: to.StringPtr("disk1"),
					}},
				}
			}
		}
		mockVMSSVMClient := testCloud.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()
		mockVMSSVMClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		dataDisks, _, err := ss.GetDataDisks(test.nodeName, test.crt)
		assert.Equal(t, test.expectedDataDisks, dataDisks, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedErrMsg, err, "TestCase[%d]: %s, expected error: %v, return error: %v", i, test.desc, test.expectedErrMsg, err)
	}
}
