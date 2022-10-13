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
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-12-01/compute"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/flowcontrol"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/diskclient/mockdiskclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssvmclient/mockvmssvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestCommonAttachDisk(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	maxShare := int32(1)
	goodInstanceID := fmt.Sprintf("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/%s", "vm1")
	diskEncryptionSetID := fmt.Sprintf("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/diskEncryptionSets/%s", "diskEncryptionSet-name")
	testTags := make(map[string]*string)
	testTags[WriteAcceleratorEnabled] = to.StringPtr("true")
	testCases := []struct {
		desc            string
		diskName        string
		existedDisk     *compute.Disk
		nodeName        types.NodeName
		vmList          map[string]string
		isDataDisksFull bool
		isBadDiskURI    bool
		isDiskUsed      bool
		expectedErr     bool
		expectedLun     int32
	}{
		{
			desc:        "correct LUN and no error shall be returned if disk is nil",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk-name",
			existedDisk: nil,
			expectedLun: 3,
			expectedErr: false,
		},
		{
			desc:        "LUN -1 and error shall be returned if there's no such instance corresponding to given nodeName",
			nodeName:    "vm1",
			diskName:    "disk-name",
			existedDisk: &compute.Disk{Name: to.StringPtr("disk-name")},
			expectedLun: -1,
			expectedErr: true,
		},
		{
			desc:            "LUN -1 and error shall be returned if there's no available LUN for instance",
			vmList:          map[string]string{"vm1": "PowerState/Running"},
			nodeName:        "vm1",
			isDataDisksFull: true,
			diskName:        "disk-name",
			existedDisk:     &compute.Disk{Name: to.StringPtr("disk-name")},
			expectedLun:     -1,
			expectedErr:     true,
		},
		{
			desc:     "correct LUN and no error shall be returned if everything is good",
			vmList:   map[string]string{"vm1": "PowerState/Running"},
			nodeName: "vm1",
			diskName: "disk-name",
			existedDisk: &compute.Disk{Name: to.StringPtr("disk-name"),
				DiskProperties: &compute.DiskProperties{
					Encryption: &compute.Encryption{DiskEncryptionSetID: &diskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey},
					DiskSizeGB: to.Int32Ptr(4096),
					DiskState:  compute.DiskStateUnattached,
				},
				Tags: testTags},
			expectedLun: 3,
			expectedErr: false,
		},
		{
			desc:     "an error shall be returned if disk state is not Unattached",
			vmList:   map[string]string{"vm1": "PowerState/Running"},
			nodeName: "vm1",
			diskName: "disk-name",
			existedDisk: &compute.Disk{Name: to.StringPtr("disk-name"),
				DiskProperties: &compute.DiskProperties{
					Encryption: &compute.Encryption{DiskEncryptionSetID: &diskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey},
					DiskSizeGB: to.Int32Ptr(4096),
					DiskState:  compute.DiskStateAttached,
				},
				Tags: testTags},
			expectedLun: -1,
			expectedErr: true,
		},
		{
			desc:        "an error shall be returned if attach an already attached disk with good ManagedBy instance id",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk-name",
			existedDisk: &compute.Disk{Name: to.StringPtr("disk-name"), ManagedBy: to.StringPtr(goodInstanceID), DiskProperties: &compute.DiskProperties{MaxShares: &maxShare}},
			expectedLun: -1,
			expectedErr: true,
		},
		{
			desc:        "an error shall be returned if attach an already attached disk with bad ManagedBy instance id",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk-name",
			existedDisk: &compute.Disk{Name: to.StringPtr("disk-name"), ManagedBy: to.StringPtr("test"), DiskProperties: &compute.DiskProperties{MaxShares: &maxShare}},
			expectedLun: -1,
			expectedErr: true,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		common := &controllerCommon{
			location:              testCloud.Location,
			storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
			resourceGroup:         testCloud.ResourceGroup,
			subscriptionID:        testCloud.SubscriptionID,
			cloud:                 testCloud,
			lockMap:               newLockMap(),
			diskOpRateLimiter:     flowcontrol.NewTokenBucketRateLimiter(10, 20),
		}
		diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
			testCloud.SubscriptionID, testCloud.ResourceGroup, test.diskName)
		if test.isBadDiskURI {
			diskURI = fmt.Sprintf("/baduri/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
				testCloud.SubscriptionID, testCloud.ResourceGroup, test.diskName)
		}
		expectedVMs := setTestVirtualMachines(testCloud, test.vmList, test.isDataDisksFull)
		if test.isDiskUsed {
			vm0 := setTestVirtualMachines(testCloud, map[string]string{"vm0": "PowerState/Running"}, test.isDataDisksFull)[0]
			expectedVMs = append(expectedVMs, vm0)
		}
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		if len(expectedVMs) == 0 {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		}
		mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		mockVMsClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(&azure.Future{}, nil).AnyTimes()
		mockVMsClient.EXPECT().WaitForUpdateResult(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, gomock.Any()).Return(nil).AnyTimes()

		lun, err := common.AttachDisk(ctx, true, "", diskURI, test.nodeName, compute.CachingTypesReadOnly, test.existedDisk)
		assert.Equal(t, test.expectedLun, lun, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, return error: %v", i, test.desc, err)
	}
}

func TestCommonAttachDiskWithVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		desc            string
		diskName        string
		nodeName        types.NodeName
		expectedLun     int32
		isVMSS          bool
		isManagedBy     bool
		isDataDisksFull bool
		expectedErr     bool
		vmList          map[string]string
		vmssList        []string
		existedDisk     *compute.Disk
	}{
		{
			desc:        "an error shall be returned if convert vmSet to ScaleSet failed",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			isVMSS:      false,
			isManagedBy: false,
			diskName:    "disk-name",
			existedDisk: &compute.Disk{Name: to.StringPtr("disk-name")},
			expectedLun: -1,
			expectedErr: true,
		},
		{
			desc:        "an error shall be returned if convert vmSet to ScaleSet success but node is not managed by availability set",
			vmssList:    []string{"vmss-vm-000001"},
			nodeName:    "vmss1",
			isVMSS:      true,
			isManagedBy: false,
			diskName:    "disk-name",
			existedDisk: &compute.Disk{Name: to.StringPtr("disk-name")},
			expectedLun: -1,
			expectedErr: true,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		testCloud.VMType = consts.VMTypeVMSS
		if test.isVMSS {
			if test.isManagedBy {
				testCloud.DisableAvailabilitySetNodes = false
				expectedVMSS := compute.VirtualMachineScaleSet{Name: to.StringPtr(testVMSSName)}
				mockVMSSClient := testCloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
				mockVMSSClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

				expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(testCloud, testVMSSName, "", 0, test.vmssList, "", false)
				mockVMSSVMClient := testCloud.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
				mockVMSSVMClient.EXPECT().List(gomock.Any(), testCloud.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

				mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
				mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()
			} else {
				testCloud.DisableAvailabilitySetNodes = true
			}
			ss, err := newScaleSet(testCloud)
			assert.NoError(t, err)
			testCloud.VMSet = ss
		}

		common := &controllerCommon{
			location:              testCloud.Location,
			storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
			resourceGroup:         testCloud.ResourceGroup,
			subscriptionID:        testCloud.SubscriptionID,
			cloud:                 testCloud,
			lockMap:               newLockMap(),
			diskOpRateLimiter:     flowcontrol.NewTokenBucketRateLimiter(10, 20),
		}
		diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
			testCloud.SubscriptionID, testCloud.ResourceGroup, test.diskName)
		if !test.isVMSS {
			expectedVMs := setTestVirtualMachines(testCloud, test.vmList, test.isDataDisksFull)
			mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
			for _, vm := range expectedVMs {
				mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
			}
			if len(expectedVMs) == 0 {
				mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
			}
			mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		}

		lun, err := common.AttachDisk(ctx, true, "test", diskURI, test.nodeName, compute.CachingTypesReadOnly, test.existedDisk)
		assert.Equal(t, test.expectedLun, lun, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, return error: %v", i, test.desc, err)
	}
}

func TestCommonDetachDisk(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		desc        string
		vmList      map[string]string
		nodeName    types.NodeName
		diskName    string
		expectedErr bool
	}{
		{
			desc:        "error should not be returned if there's no such instance corresponding to given nodeName",
			nodeName:    "vm1",
			expectedErr: false,
		},
		{
			desc:        "no error shall be returned if there's no matching disk according to given diskName",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk2",
			expectedErr: false,
		},
		{
			desc:        "no error shall be returned if the disk exists",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk1",
			expectedErr: false,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		common := &controllerCommon{
			location:              testCloud.Location,
			storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
			resourceGroup:         testCloud.ResourceGroup,
			subscriptionID:        testCloud.SubscriptionID,
			cloud:                 testCloud,
			lockMap:               newLockMap(),
			diskOpRateLimiter:     flowcontrol.NewTokenBucketRateLimiter(10, 20),
		}
		diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/disk-name",
			testCloud.SubscriptionID, testCloud.ResourceGroup)
		expectedVMs := setTestVirtualMachines(testCloud, test.vmList, false)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		if len(expectedVMs) == 0 {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		}
		mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		err := common.DetachDisk(ctx, test.diskName, diskURI, test.nodeName)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, err: %v", i, test.desc, err)
	}
}

func TestCommonUpdateVM(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		desc             string
		vmList           map[string]string
		nodeName         types.NodeName
		diskName         string
		isErrorRetriable bool
		expectedErr      bool
	}{
		{
			desc:        "error should not be returned if there's no such instance corresponding to given nodeName",
			nodeName:    "vm1",
			expectedErr: false,
		},
		{
			desc:             "an error should be returned if vmset detach failed with isErrorRetriable error",
			vmList:           map[string]string{"vm1": "PowerState/Running"},
			nodeName:         "vm1",
			isErrorRetriable: true,
			expectedErr:      true,
		},
		{
			desc:        "no error shall be returned if there's no matching disk according to given diskName",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "diskx",
			expectedErr: false,
		},
		{
			desc:        "no error shall be returned if the disk exists",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk1",
			expectedErr: false,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		common := &controllerCommon{
			location:              testCloud.Location,
			storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
			resourceGroup:         testCloud.ResourceGroup,
			subscriptionID:        testCloud.SubscriptionID,
			cloud:                 testCloud,
			lockMap:               newLockMap(),
			diskOpRateLimiter:     flowcontrol.NewTokenBucketRateLimiter(10, 20),
		}
		expectedVMs := setTestVirtualMachines(testCloud, test.vmList, false)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		if len(expectedVMs) == 0 {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		}
		if test.isErrorRetriable {
			testCloud.CloudProviderBackoff = true
			testCloud.ResourceRequestBackoff = wait.Backoff{Steps: 1}
			mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(&retry.Error{HTTPStatusCode: http.StatusBadRequest, Retriable: true, RawError: fmt.Errorf("Retriable: true")}).AnyTimes()
		} else {
			mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		}

		err := common.UpdateVM(ctx, test.nodeName)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, err: %v", i, test.desc, err)
	}
}

func TestGetDiskLun(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc        string
		diskName    string
		diskURI     string
		expectedLun int32
		expectedErr bool
	}{
		{
			desc:        "LUN -1 and error shall be returned if diskName != disk.Name or diskURI != disk.Vhd.URI",
			diskName:    "diskx",
			expectedLun: -1,
			expectedErr: true,
		},
		{
			desc:        "correct LUN and no error shall be returned if diskName = disk.Name",
			diskName:    "disk1",
			expectedLun: 0,
			expectedErr: false,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		common := &controllerCommon{
			location:              testCloud.Location,
			storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
			resourceGroup:         testCloud.ResourceGroup,
			subscriptionID:        testCloud.SubscriptionID,
			cloud:                 testCloud,
			lockMap:               newLockMap(),
			diskOpRateLimiter:     flowcontrol.NewTokenBucketRateLimiter(10, 20),
		}
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}

		lun, _, err := common.GetDiskLun(test.diskName, test.diskURI, "vm1")
		assert.Equal(t, test.expectedLun, lun, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestSetDiskLun(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc            string
		nodeName        string
		diskURI         string
		diskMap         map[string]*AttachDiskOptions
		isDataDisksFull bool
		expectedErr     bool
		expectedLun     int32
	}{
		{
			desc:        "the minimal LUN shall be returned if there's enough room for extra disks",
			nodeName:    "nodeName",
			diskURI:     "diskURI",
			diskMap:     map[string]*AttachDiskOptions{"diskURI": {}},
			expectedLun: 3,
			expectedErr: false,
		},
		{
			desc:            "LUN -1 and error shall be returned if there's no available LUN",
			nodeName:        "nodeName",
			diskURI:         "diskURI",
			diskMap:         map[string]*AttachDiskOptions{"diskURI": {}},
			isDataDisksFull: true,
			expectedLun:     -1,
			expectedErr:     true,
		},
		{
			desc:        "diskURI1 is not in VM data disk list nor in diskMap",
			nodeName:    "nodeName",
			diskURI:     "diskURI1",
			diskMap:     map[string]*AttachDiskOptions{"diskURI2": {}},
			expectedLun: -1,
			expectedErr: true,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		common := &controllerCommon{
			location:              testCloud.Location,
			storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
			resourceGroup:         testCloud.ResourceGroup,
			subscriptionID:        testCloud.SubscriptionID,
			cloud:                 testCloud,
			lockMap:               newLockMap(),
			diskOpRateLimiter:     flowcontrol.NewTokenBucketRateLimiter(10, 20),
		}
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{test.nodeName: "PowerState/Running"}, test.isDataDisksFull)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}

		lun, err := common.SetDiskLun(types.NodeName(test.nodeName), test.diskURI, test.diskMap)
		assert.Equal(t, test.expectedLun, lun, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestDisksAreAttached(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc             string
		diskNames        []string
		nodeName         types.NodeName
		expectedAttached map[string]bool
		expectedErr      bool
	}{
		{
			desc:             "an error shall be returned if there's no such instance corresponding to given nodeName",
			diskNames:        []string{"disk1"},
			nodeName:         "vm2",
			expectedAttached: map[string]bool{"disk1": false},
			expectedErr:      false,
		},
		{
			desc:             "proper attach map shall be returned if everything is good",
			diskNames:        []string{"disk1", "diskx"},
			nodeName:         "vm1",
			expectedAttached: map[string]bool{"disk1": true, "diskx": false},
			expectedErr:      false,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		common := &controllerCommon{
			location:              testCloud.Location,
			storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
			resourceGroup:         testCloud.ResourceGroup,
			subscriptionID:        testCloud.SubscriptionID,
			cloud:                 testCloud,
			lockMap:               newLockMap(),
			diskOpRateLimiter:     flowcontrol.NewTokenBucketRateLimiter(10, 20),
		}
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, "vm2", gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()

		attached, err := common.DisksAreAttached(test.diskNames, test.nodeName)
		assert.Equal(t, test.expectedAttached, attached, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestFilteredDetachingDisks(t *testing.T) {

	disks := []compute.DataDisk{
		{
			Name:         pointer.StringPtr("DiskName1"),
			ToBeDetached: pointer.BoolPtr(false),
			ManagedDisk: &compute.ManagedDiskParameters{
				ID: pointer.StringPtr("ManagedID"),
			},
		},
		{
			Name:         pointer.StringPtr("DiskName2"),
			ToBeDetached: pointer.BoolPtr(true),
		},
		{
			Name:         pointer.StringPtr("DiskName3"),
			ToBeDetached: nil,
		},
		{
			Name:         pointer.StringPtr("DiskName4"),
			ToBeDetached: nil,
		},
	}

	filteredDisks := filterDetachingDisks(disks)
	assert.Equal(t, 3, len(filteredDisks))
	assert.Equal(t, "DiskName1", *filteredDisks[0].Name)
	assert.Equal(t, "ManagedID", *filteredDisks[0].ManagedDisk.ID)
	assert.Equal(t, "DiskName3", *filteredDisks[1].Name)

	disks = []compute.DataDisk{}
	filteredDisks = filterDetachingDisks(disks)
	assert.Equal(t, 0, len(filteredDisks))
}

func TestGetValidCreationData(t *testing.T) {
	sourceResourceSnapshotID := "/subscriptions/xxx/resourceGroups/xxx/providers/Microsoft.Compute/snapshots/xxx"
	sourceResourceVolumeID := "/subscriptions/xxx/resourceGroups/xxx/providers/Microsoft.Compute/disks/xxx"

	tests := []struct {
		subscriptionID   string
		resourceGroup    string
		sourceResourceID string
		sourceType       string
		expected1        compute.CreationData
		expected2        error
	}{
		{
			subscriptionID:   "",
			resourceGroup:    "",
			sourceResourceID: "",
			sourceType:       "",
			expected1: compute.CreationData{
				CreateOption: compute.DiskCreateOptionEmpty,
			},
			expected2: nil,
		},
		{
			subscriptionID:   "",
			resourceGroup:    "",
			sourceResourceID: "/subscriptions/xxx/resourceGroups/xxx/providers/Microsoft.Compute/snapshots/xxx",
			sourceType:       sourceSnapshot,
			expected1: compute.CreationData{
				CreateOption:     compute.DiskCreateOptionCopy,
				SourceResourceID: &sourceResourceSnapshotID,
			},
			expected2: nil,
		},
		{
			subscriptionID:   "xxx",
			resourceGroup:    "xxx",
			sourceResourceID: "xxx",
			sourceType:       sourceSnapshot,
			expected1: compute.CreationData{
				CreateOption:     compute.DiskCreateOptionCopy,
				SourceResourceID: &sourceResourceSnapshotID,
			},
			expected2: nil,
		},
		{
			subscriptionID:   "",
			resourceGroup:    "",
			sourceResourceID: "/subscriptions/23/providers/Microsoft.Compute/disks/name",
			sourceType:       sourceSnapshot,
			expected1:        compute.CreationData{},
			expected2:        fmt.Errorf("sourceResourceID(%s) is invalid, correct format: %s", "/subscriptions//resourceGroups//providers/Microsoft.Compute/snapshots//subscriptions/23/providers/Microsoft.Compute/disks/name", diskSnapshotPathRE),
		},
		{
			subscriptionID:   "",
			resourceGroup:    "",
			sourceResourceID: "http://test.com/vhds/name",
			sourceType:       sourceSnapshot,
			expected1:        compute.CreationData{},
			expected2:        fmt.Errorf("sourceResourceID(%s) is invalid, correct format: %s", "/subscriptions//resourceGroups//providers/Microsoft.Compute/snapshots/http://test.com/vhds/name", diskSnapshotPathRE),
		},
		{
			subscriptionID:   "",
			resourceGroup:    "",
			sourceResourceID: "/subscriptions/xxx/snapshots/xxx",
			sourceType:       sourceSnapshot,
			expected1:        compute.CreationData{},
			expected2:        fmt.Errorf("sourceResourceID(%s) is invalid, correct format: %s", "/subscriptions//resourceGroups//providers/Microsoft.Compute/snapshots//subscriptions/xxx/snapshots/xxx", diskSnapshotPathRE),
		},
		{
			subscriptionID:   "",
			resourceGroup:    "",
			sourceResourceID: "/subscriptions/xxx/resourceGroups/xxx/providers/Microsoft.Compute/snapshots/xxx/snapshots/xxx/snapshots/xxx",
			sourceType:       sourceSnapshot,
			expected1:        compute.CreationData{},
			expected2:        fmt.Errorf("sourceResourceID(%s) is invalid, correct format: %s", "/subscriptions/xxx/resourceGroups/xxx/providers/Microsoft.Compute/snapshots/xxx/snapshots/xxx/snapshots/xxx", diskSnapshotPathRE),
		},
		{
			subscriptionID:   "",
			resourceGroup:    "",
			sourceResourceID: "xxx",
			sourceType:       "",
			expected1: compute.CreationData{
				CreateOption: compute.DiskCreateOptionEmpty,
			},
			expected2: nil,
		},
		{
			subscriptionID:   "",
			resourceGroup:    "",
			sourceResourceID: "/subscriptions/xxx/resourceGroups/xxx/providers/Microsoft.Compute/disks/xxx",
			sourceType:       sourceVolume,
			expected1: compute.CreationData{
				CreateOption:     compute.DiskCreateOptionCopy,
				SourceResourceID: &sourceResourceVolumeID,
			},
			expected2: nil,
		},
		{
			subscriptionID:   "xxx",
			resourceGroup:    "xxx",
			sourceResourceID: "xxx",
			sourceType:       sourceVolume,
			expected1: compute.CreationData{
				CreateOption:     compute.DiskCreateOptionCopy,
				SourceResourceID: &sourceResourceVolumeID,
			},
			expected2: nil,
		},
		{
			subscriptionID:   "",
			resourceGroup:    "",
			sourceResourceID: "/subscriptions/xxx/resourceGroups/xxx/providers/Microsoft.Compute/snapshots/xxx",
			sourceType:       sourceVolume,
			expected1:        compute.CreationData{},
			expected2:        fmt.Errorf("sourceResourceID(%s) is invalid, correct format: %s", "/subscriptions//resourceGroups//providers/Microsoft.Compute/disks//subscriptions/xxx/resourceGroups/xxx/providers/Microsoft.Compute/snapshots/xxx", managedDiskPathRE),
		},
	}

	for _, test := range tests {
		result, err := getValidCreationData(test.subscriptionID, test.resourceGroup, test.sourceResourceID, test.sourceType)
		if !reflect.DeepEqual(result, test.expected1) || !reflect.DeepEqual(err, test.expected2) {
			t.Errorf("input sourceResourceID: %v, sourceType: %v, getValidCreationData result: %v, expected1 : %v, err: %v, expected2: %v", test.sourceResourceID, test.sourceType, result, test.expected1, err, test.expected2)
		}
	}
}

func TestCheckDiskExists(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCloud := GetTestCloud(ctrl)
	common := &controllerCommon{
		location:              testCloud.Location,
		storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
		resourceGroup:         testCloud.ResourceGroup,
		subscriptionID:        testCloud.SubscriptionID,
		cloud:                 testCloud,
		lockMap:               newLockMap(),
	}
	// create a new disk before running test
	newDiskName := "newdisk"
	newDiskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
		testCloud.SubscriptionID, testCloud.ResourceGroup, newDiskName)

	mockDisksClient := testCloud.DisksClient.(*mockdiskclient.MockInterface)
	mockDisksClient.EXPECT().Get(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, newDiskName).Return(compute.Disk{}, nil).AnyTimes()
	mockDisksClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Not(testCloud.ResourceGroup), gomock.Any()).Return(compute.Disk{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()

	testCases := []struct {
		diskURI        string
		expectedResult bool
		expectedErr    bool
	}{
		{
			diskURI:        "incorrect disk URI format",
			expectedResult: false,
			expectedErr:    true,
		},
		{
			diskURI:        "/subscriptions/xxx/resourceGroups/xxx/providers/Microsoft.Compute/disks/non-existing-disk",
			expectedResult: false,
			expectedErr:    false,
		},
		{
			diskURI:        newDiskURI,
			expectedResult: true,
			expectedErr:    false,
		},
	}

	for i, test := range testCases {
		exist, err := common.checkDiskExists(ctx, test.diskURI)
		assert.Equal(t, test.expectedResult, exist, "TestCase[%d]", i, exist)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d], return error: %v", i, err)
	}
}

func TestFilterNonExistingDisks(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCloud := GetTestCloud(ctrl)
	common := &controllerCommon{
		location:              testCloud.Location,
		storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
		resourceGroup:         testCloud.ResourceGroup,
		subscriptionID:        testCloud.SubscriptionID,
		cloud:                 testCloud,
		lockMap:               newLockMap(),
	}
	// create a new disk before running test
	diskURIPrefix := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/",
		testCloud.SubscriptionID, testCloud.ResourceGroup)
	newDiskName := "newdisk"
	newDiskURI := diskURIPrefix + newDiskName

	mockDisksClient := testCloud.DisksClient.(*mockdiskclient.MockInterface)
	mockDisksClient.EXPECT().Get(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, newDiskName).Return(compute.Disk{}, nil).AnyTimes()
	mockDisksClient.EXPECT().Get(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, gomock.Not(newDiskName)).Return(compute.Disk{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()

	disks := []compute.DataDisk{
		{
			Name: &newDiskName,
			ManagedDisk: &compute.ManagedDiskParameters{
				ID: &newDiskURI,
			},
		},
		{
			Name: pointer.StringPtr("DiskName2"),
			ManagedDisk: &compute.ManagedDiskParameters{
				ID: pointer.StringPtr(diskURIPrefix + "DiskName2"),
			},
		},
		{
			Name: pointer.StringPtr("DiskName3"),
			ManagedDisk: &compute.ManagedDiskParameters{
				ID: pointer.StringPtr(diskURIPrefix + "DiskName3"),
			},
		},
		{
			Name: pointer.StringPtr("DiskName4"),
			ManagedDisk: &compute.ManagedDiskParameters{
				ID: pointer.StringPtr(diskURIPrefix + "DiskName4"),
			},
		},
	}

	filteredDisks := common.filterNonExistingDisks(ctx, disks)
	assert.Equal(t, 1, len(filteredDisks))
	assert.Equal(t, newDiskName, *filteredDisks[0].Name)

	disks = []compute.DataDisk{}
	filteredDisks = filterDetachingDisks(disks)
	assert.Equal(t, 0, len(filteredDisks))
}

func TestFilterNonExistingDisksWithSpecialHTTPStatusCode(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCloud := GetTestCloud(ctrl)
	common := &controllerCommon{
		location:              testCloud.Location,
		storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
		resourceGroup:         testCloud.ResourceGroup,
		subscriptionID:        testCloud.SubscriptionID,
		cloud:                 testCloud,
		lockMap:               newLockMap(),
	}
	// create a new disk before running test
	diskURIPrefix := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/",
		testCloud.SubscriptionID, testCloud.ResourceGroup)
	newDiskName := "specialdisk"
	newDiskURI := diskURIPrefix + newDiskName

	mockDisksClient := testCloud.DisksClient.(*mockdiskclient.MockInterface)
	mockDisksClient.EXPECT().Get(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, gomock.Eq(newDiskName)).Return(compute.Disk{}, &retry.Error{HTTPStatusCode: http.StatusBadRequest, RawError: cloudprovider.InstanceNotFound}).AnyTimes()

	disks := []compute.DataDisk{
		{
			Name: &newDiskName,
			ManagedDisk: &compute.ManagedDiskParameters{
				ID: &newDiskURI,
			},
		},
	}

	filteredDisks := common.filterNonExistingDisks(ctx, disks)
	assert.Equal(t, 1, len(filteredDisks))
	assert.Equal(t, newDiskName, *filteredDisks[0].Name)
}

func TestIsInstanceNotFoundError(t *testing.T) {
	testCases := []struct {
		errMsg         string
		expectedResult bool
	}{
		{
			errMsg:         "",
			expectedResult: false,
		},
		{
			errMsg:         "other error",
			expectedResult: false,
		},
		{
			errMsg:         "The provided instanceId 857 is not an active Virtual Machine Scale Set VM instanceId.",
			expectedResult: true,
		},
		{
			errMsg:         `compute.VirtualMachineScaleSetVMsClient#Update: Failure sending request: StatusCode=400 -- Original Error: Code="InvalidParameter" Message="The provided instanceId 1181 is not an active Virtual Machine Scale Set VM instanceId." Target="instanceIds"`,
			expectedResult: true,
		},
	}

	for i, test := range testCases {
		result := isInstanceNotFoundError(fmt.Errorf(test.errMsg))
		assert.Equal(t, test.expectedResult, result, "TestCase[%d]", i, result)
	}
}

func TestAttachDiskRequestFuncs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc                 string
		diskURI              string
		nodeName             string
		diskName             string
		diskNum              int
		duplicateDiskRequest bool
		expectedErr          bool
	}{
		{
			desc:        "one disk request in queue",
			diskURI:     "diskURI",
			nodeName:    "nodeName",
			diskName:    "diskName",
			diskNum:     1,
			expectedErr: false,
		},
		{
			desc:        "multiple disk requests in queue",
			diskURI:     "diskURI",
			nodeName:    "nodeName",
			diskName:    "diskName",
			diskNum:     10,
			expectedErr: false,
		},
		{
			desc:        "zero disk request in queue",
			diskURI:     "diskURI",
			nodeName:    "nodeName",
			diskName:    "diskName",
			diskNum:     0,
			expectedErr: false,
		},
		{
			desc:                 "multiple disk requests in queue",
			diskURI:              "diskURI",
			nodeName:             "nodeName",
			diskName:             "diskName",
			duplicateDiskRequest: true,
			diskNum:              10,
			expectedErr:          false,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		common := &controllerCommon{
			location:              testCloud.Location,
			storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
			resourceGroup:         testCloud.ResourceGroup,
			subscriptionID:        testCloud.SubscriptionID,
			cloud:                 testCloud,
			lockMap:               newLockMap(),
			diskOpRateLimiter:     flowcontrol.NewTokenBucketRateLimiter(10, 20),
		}
		for i := 1; i <= test.diskNum; i++ {
			diskURI := fmt.Sprintf("%s%d", test.diskURI, i)
			diskName := fmt.Sprintf("%s%d", test.diskName, i)
			attachDiskOptions := &AttachDiskOptions{diskName: diskName}
			err := common.insertAttachDiskRequest(diskURI, test.nodeName, attachDiskOptions)
			assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s", i, test.desc)
			if test.duplicateDiskRequest {
				err := common.insertAttachDiskRequest(diskURI, test.nodeName, attachDiskOptions)
				assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s", i, test.desc)
			}
		}

		diskMap, err := common.cleanAttachDiskRequests(test.nodeName)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.diskNum, len(diskMap), "TestCase[%d]: %s", i, test.desc)
		for diskURI, opt := range diskMap {
			assert.Equal(t, strings.Contains(diskURI, test.diskURI), true, "TestCase[%d]: %s", i, test.desc)
			assert.Equal(t, strings.Contains(opt.diskName, test.diskName), true, "TestCase[%d]: %s", i, test.desc)
		}
	}
}

func TestDetachDiskRequestFuncs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc                 string
		diskURI              string
		nodeName             string
		diskName             string
		diskNum              int
		duplicateDiskRequest bool
		expectedErr          bool
	}{
		{
			desc:        "one disk request in queue",
			diskURI:     "diskURI",
			nodeName:    "nodeName",
			diskName:    "diskName",
			diskNum:     1,
			expectedErr: false,
		},
		{
			desc:        "multiple disk requests in queue",
			diskURI:     "diskURI",
			nodeName:    "nodeName",
			diskName:    "diskName",
			diskNum:     10,
			expectedErr: false,
		},
		{
			desc:        "zero disk request in queue",
			diskURI:     "diskURI",
			nodeName:    "nodeName",
			diskName:    "diskName",
			diskNum:     0,
			expectedErr: false,
		},
		{
			desc:                 "multiple disk requests in queue",
			diskURI:              "diskURI",
			nodeName:             "nodeName",
			diskName:             "diskName",
			duplicateDiskRequest: true,
			diskNum:              10,
			expectedErr:          false,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		common := &controllerCommon{
			location:              testCloud.Location,
			storageEndpointSuffix: testCloud.Environment.StorageEndpointSuffix,
			resourceGroup:         testCloud.ResourceGroup,
			subscriptionID:        testCloud.SubscriptionID,
			cloud:                 testCloud,
			lockMap:               newLockMap(),
			diskOpRateLimiter:     flowcontrol.NewTokenBucketRateLimiter(10, 20),
		}
		for i := 1; i <= test.diskNum; i++ {
			diskURI := fmt.Sprintf("%s%d", test.diskURI, i)
			diskName := fmt.Sprintf("%s%d", test.diskName, i)
			err := common.insertDetachDiskRequest(diskName, diskURI, test.nodeName)
			assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s", i, test.desc)
			if test.duplicateDiskRequest {
				err := common.insertDetachDiskRequest(diskName, diskURI, test.nodeName)
				assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s", i, test.desc)
			}
		}

		diskMap, err := common.cleanDetachDiskRequests(test.nodeName)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.diskNum, len(diskMap), "TestCase[%d]: %s", i, test.desc)
		for diskURI, diskName := range diskMap {
			assert.Equal(t, strings.Contains(diskURI, test.diskURI), true, "TestCase[%d]: %s", i, test.desc)
			assert.Equal(t, strings.Contains(diskName, test.diskName), true, "TestCase[%d]: %s", i, test.desc)
		}
	}
}
