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
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/go-autorest/autorest/azure"
	autorestmocks "github.com/Azure/go-autorest/autorest/mocks"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
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

	diskname := "disk-name" //nolint: goconst

	fakeStatusNotFoundVMSSName := types.NodeName("FakeStatusNotFoundVMSSName")
	testCases := []struct {
		desc            string
		vmssName        types.NodeName
		vmssvmName      types.NodeName
		disks           []string
		vmList          map[string]string
		vmssVMList      []string
		inconsistentLUN bool
		expectedErr     error
	}{
		{
			desc:       "no error shall be returned if everything is good with one managed disk",
			vmssVMList: []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:   "vmss00",
			vmssvmName: "vmss00-vm-000000",
			disks:      []string{"disk-name"},
		},
		{
			desc:       "no error shall be returned if everything is good with 2 managed disks",
			vmssVMList: []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:   "vmss00",
			vmssvmName: "vmss00-vm-000000",
			disks:      []string{"disk-name", "disk-name2"},
		},
		{
			desc:       "no error shall be returned if everything is good with non-managed disk",
			vmssVMList: []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:   "vmss00",
			vmssvmName: "vmss00-vm-000000",
			disks:      []string{"disk-name"},
		},
		{
			desc:            "error should be returned when disk lun is inconsistent",
			vmssVMList:      []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:        "vmss00",
			vmssvmName:      "vmss00-vm-000000",
			disks:           []string{"disk-name", "disk-name2"},
			inconsistentLUN: true,
			expectedErr:     fmt.Errorf("disk(/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/disks/disk-name) already attached to node(vmss00-vm-000000) on LUN(0), but target LUN is 63"),
		},
	}

	for i, test := range testCases {
		scaleSetName := string(test.vmssName)
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.desc)
		testCloud := ss.Cloud
		testCloud.PrimaryScaleSetName = scaleSetName
		expectedVMSS := buildTestVMSSWithLB(scaleSetName, "vmss00-vm-", []string{testLBBackendpoolID0}, false)
		mockVMSSClient := testCloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()
		mockVMSSClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, scaleSetName).Return(expectedVMSS, nil).MaxTimes(1)
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(nil).MaxTimes(1)
		mockVMClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(testCloud, scaleSetName, "", 0, test.vmssVMList, "succeeded", false)
		for _, vmssvm := range expectedVMSSVMs {
			vmssvm.StorageProfile = &compute.StorageProfile{
				OsDisk: &compute.OSDisk{
					Name: pointer.String("osdisk1"),
					ManagedDisk: &compute.ManagedDiskParameters{
						ID: pointer.String("ManagedID"),
						DiskEncryptionSet: &compute.DiskEncryptionSetParameters{
							ID: pointer.String("DiskEncryptionSetID"),
						},
					},
				},
				DataDisks: &[]compute.DataDisk{},
			}
			if test.inconsistentLUN {
				diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
					testCloud.SubscriptionID, testCloud.ResourceGroup, diskname)
				vmssvm.StorageProfile.DataDisks = &[]compute.DataDisk{
					{Lun: pointer.Int32(0), Name: &diskname, ManagedDisk: &compute.ManagedDiskParameters{ID: &diskURI}},
				}
			}
		}
		mockVMSSVMClient := testCloud.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()
		if scaleSetName == string(fakeStatusNotFoundVMSSName) {
			mockVMSSVMClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		} else {
			mockVMSSVMClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
			mockVMSSVMClient.EXPECT().WaitForUpdateResult(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
		}

		diskMap := map[string]*AttachDiskOptions{}
		for i, diskName := range test.disks {
			options := AttachDiskOptions{
				Lun:                     int32(i),
				DiskName:                diskName,
				CachingMode:             compute.CachingTypesReadWrite,
				DiskEncryptionSetID:     "",
				WriteAcceleratorEnabled: true,
			}
			if test.inconsistentLUN {
				options.Lun = 63
			}

			diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
				testCloud.SubscriptionID, testCloud.ResourceGroup, diskName)
			diskMap[diskURI] = &options
		}
		err = ss.AttachDisk(ctx, test.vmssvmName, diskMap)
		assert.Equal(t, test.expectedErr, err, "TestCase[%d]: %s, expected error: %v, return error: %v", i, test.desc, test.expectedErr, err)
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
		forceDetach    bool
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
			desc:        "no error shall be returned with force detach",
			vmssVMList:  []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:    "vmss00",
			vmssvmName:  "vmss00-vm-000000",
			disks:       []string{diskName, "disk2"},
			forceDetach: true,
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
		scaleSetName := strings.ToLower(string(test.vmssName))
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.desc)
		testCloud := ss.Cloud
		testCloud.PrimaryScaleSetName = scaleSetName
		expectedVMSS := buildTestVMSSWithLB(scaleSetName, "vmss00-vm-", []string{testLBBackendpoolID0}, false)
		mockVMSSClient := testCloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()
		mockVMSSClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, scaleSetName).Return(expectedVMSS, nil).MaxTimes(1)
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(nil).MaxTimes(1)

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(testCloud, scaleSetName, "", 0, test.vmssVMList, "succeeded", false)
		var updatedVMSSVM *compute.VirtualMachineScaleSetVM
		for itr, vmssvm := range expectedVMSSVMs {
			vmssvm.StorageProfile = &compute.StorageProfile{
				OsDisk: &compute.OSDisk{
					Name: pointer.String("osdisk1"),
					ManagedDisk: &compute.ManagedDiskParameters{
						ID: pointer.String("ManagedID"),
						DiskEncryptionSet: &compute.DiskEncryptionSetParameters{
							ID: pointer.String("DiskEncryptionSetID"),
						},
					},
				},
				DataDisks: &[]compute.DataDisk{
					{
						Lun:  pointer.Int32(0),
						Name: pointer.String(diskName),
					},
					{
						Lun:  pointer.Int32(1),
						Name: pointer.String("disk2"),
					},
					{
						Lun:  pointer.Int32(2),
						Name: pointer.String("disk3"),
					},
				},
			}

			if string(test.vmssvmName) == *vmssvm.VirtualMachineScaleSetVMProperties.OsProfile.ComputerName {
				updatedVMSSVM = &expectedVMSSVMs[itr]
			}
		}
		mockVMSSVMClient := testCloud.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()
		if scaleSetName == strings.ToLower(string(fakeStatusNotFoundVMSSName)) {
			mockVMSSVMClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(updatedVMSSVM, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		} else {
			mockVMSSVMClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(updatedVMSSVM, nil).AnyTimes()
		}

		mockVMClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		diskMap := map[string]string{}
		for _, diskName := range test.disks {
			diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
				testCloud.SubscriptionID, testCloud.ResourceGroup, diskName)
			diskMap[diskURI] = diskName
		}
		err = ss.DetachDisk(ctx, test.vmssvmName, diskMap, test.forceDetach)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, err: %v", i, test.desc, err)
		if test.expectedErr {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), "TestCase[%d]: %s, expected error: %v, return error: %v", i, test.desc, test.expectedErrMsg, err)
		}

		if !test.expectedErr {
			dataDisks, _, err := ss.GetDataDisks(context.TODO(), test.vmssvmName, azcache.CacheReadTypeDefault)
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
			existedDisk:    compute.Disk{Name: pointer.String(diskName)},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("not a vmss instance"),
		},
		{
			desc:        "no error shall be returned if everything is good",
			vmssVMList:  []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:    "vmss00",
			vmssvmName:  "vmss00-vm-000000",
			existedDisk: compute.Disk{Name: pointer.String(diskName)},
			expectedErr: false,
		},
		{
			desc:           "an error shall be returned if response StatusNotFound",
			vmssVMList:     []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:       fakeStatusNotFoundVMSSName,
			vmssvmName:     "vmss00-vm-000000",
			existedDisk:    compute.Disk{Name: pointer.String(diskName)},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 404, RawError: %w", fmt.Errorf("instance not found")),
		},
		{
			desc:        "no error shall be returned if everything is good and the attaching disk does not match data disk",
			vmssVMList:  []string{"vmss00-vm-000000", "vmss00-vm-000001", "vmss00-vm-000002"},
			vmssName:    "vmss00",
			vmssvmName:  "vmss00-vm-000000",
			existedDisk: compute.Disk{Name: pointer.String("disk-name-err")},
			expectedErr: false,
		},
	}

	for i, test := range testCases {
		scaleSetName := strings.ToLower(string(test.vmssName))
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.desc)
		testCloud := ss.Cloud
		testCloud.PrimaryScaleSetName = scaleSetName
		expectedVMSS := buildTestVMSSWithLB(scaleSetName, "vmss00-vm-", []string{testLBBackendpoolID0}, false)
		mockVMSSClient := testCloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()
		mockVMSSClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, scaleSetName).Return(expectedVMSS, nil).MaxTimes(1)
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(nil).MaxTimes(1)

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(testCloud, scaleSetName, "", 0, test.vmssVMList, "succeeded", false)
		var updatedVMSSVM *compute.VirtualMachineScaleSetVM

		for itr, vmssvm := range expectedVMSSVMs {
			vmssvm.StorageProfile = &compute.StorageProfile{
				OsDisk: &compute.OSDisk{
					Name: pointer.String("osdisk1"),
					ManagedDisk: &compute.ManagedDiskParameters{
						ID: pointer.String("ManagedID"),
						DiskEncryptionSet: &compute.DiskEncryptionSetParameters{
							ID: pointer.String("DiskEncryptionSetID"),
						},
					},
				},
				DataDisks: &[]compute.DataDisk{{
					Lun:  pointer.Int32(0),
					Name: pointer.String(diskName),
				}},
			}

			if string(test.vmssvmName) == *vmssvm.VirtualMachineScaleSetVMProperties.OsProfile.ComputerName {
				updatedVMSSVM = &expectedVMSSVMs[itr]
			}
		}
		mockVMSSVMClient := testCloud.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

		r := autorestmocks.NewResponseWithStatus("200", 200)
		r.Request.Method = http.MethodPut

		future, err := azure.NewFutureFromResponse(r)

		mockVMSSVMClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&future, err).AnyTimes()

		if scaleSetName == strings.ToLower(string(fakeStatusNotFoundVMSSName)) {
			mockVMSSVMClient.EXPECT().WaitForUpdateResult(gomock.Any(), &future, testCloud.ResourceGroup, gomock.Any()).Return(updatedVMSSVM, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		} else {
			mockVMSSVMClient.EXPECT().WaitForUpdateResult(gomock.Any(), &future, testCloud.ResourceGroup, gomock.Any()).Return(updatedVMSSVM, nil).AnyTimes()
		}

		mockVMClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		err = ss.UpdateVM(ctx, test.vmssvmName)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, err: %v", i, test.desc, err)
		if test.expectedErr {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), "TestCase[%d]: %s, expected error: %v, return error: %v", i, test.desc, test.expectedErrMsg, err)
		}

		if !test.expectedErr {
			dataDisks, _, err := ss.GetDataDisks(context.TODO(), test.vmssvmName, azcache.CacheReadTypeDefault)
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
		expectedDataDisks []*armcompute.DataDisk
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
			expectedDataDisks: []*armcompute.DataDisk{
				{
					Lun:  pointer.Int32(0),
					Name: pointer.String("disk1"),
				},
			},
			expectedErr: false,
			crt:         azcache.CacheReadTypeDefault,
		},
		{
			desc:     "correct list of data disks shall be returned if everything is good",
			nodeName: "vmss00-vm-000000",
			expectedDataDisks: []*armcompute.DataDisk{
				{
					Lun:  pointer.Int32(0),
					Name: pointer.String("disk1"),
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
		testCloud := ss.Cloud
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
						Lun:  pointer.Int32(0),
						Name: pointer.String("disk1"),
					}},
				}
			}
		}
		updatedVMSSVM := &expectedVMSSVMs[0]
		mockVMSSVMClient := testCloud.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()
		mockVMSSVMClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, scaleSetName, gomock.Any(), gomock.Any(), gomock.Any()).Return(updatedVMSSVM, nil).AnyTimes()
		dataDisks, _, err := ss.GetDataDisks(context.TODO(), test.nodeName, test.crt)
		assert.Equal(t, test.expectedDataDisks, dataDisks, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedErrMsg, err, "TestCase[%d]: %s, expected error: %v, return error: %v", i, test.desc, test.expectedErrMsg, err)
	}
}
