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
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	cloudvolume "k8s.io/cloud-provider/volume"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/diskclient/mockdiskclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

const (
	fakeGetDiskFailed = "fakeGetDiskFailed"
	disk1Name         = "disk1"
	disk1ID           = "diskid1"
)

func TestCreateManagedDisk(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	maxShare := int32(2)
	goodDiskEncryptionSetID := fmt.Sprintf("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/diskEncryptionSets/%s", "diskEncryptionSet-name")
	badDiskEncryptionSetID := "badDiskEncryptionSetID"
	testTags := make(map[string]*string)
	testTags[WriteAcceleratorEnabled] = pointer.String("true")
	testCases := []struct {
		desc                string
		diskID              string
		diskName            string
		storageAccountType  compute.DiskStorageAccountTypes
		diskIOPSReadWrite   string
		diskMBPSReadWrite   string
		diskEncryptionSetID string
		diskEncryptionType  string
		subscriptionID      string
		resouceGroup        string
		publicNetworkAccess compute.PublicNetworkAccess
		networkAccessPolicy compute.NetworkAccessPolicy
		diskAccessID        *string
		expectedDiskID      string
		existedDisk         compute.Disk
		expectedErr         bool
		expectedErrMsg      error
	}{
		{
			desc:                "disk Id and no error shall be returned if everything is good with UltraSSDLRS storage account",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.UltraSSDLRS,
			diskIOPSReadWrite:   "100",
			diskMBPSReadWrite:   "100",
			diskEncryptionSetID: goodDiskEncryptionSetID,
			expectedDiskID:      disk1ID,
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         false,
		},
		{
			desc:                "disk Id and no error shall be returned if everything is good with PremiumV2LRS storage account",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.PremiumV2LRS,
			diskIOPSReadWrite:   "100",
			diskMBPSReadWrite:   "100",
			diskEncryptionSetID: goodDiskEncryptionSetID,
			expectedDiskID:      disk1ID,
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         false,
		},
		{
			desc:                "disk Id and no error shall be returned if everything is good with PremiumV2LRS storage account",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.PremiumV2LRS,
			diskIOPSReadWrite:   "",
			diskMBPSReadWrite:   "",
			diskEncryptionSetID: goodDiskEncryptionSetID,
			expectedDiskID:      disk1ID,
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         false,
		},
		{
			desc:                "disk Id and no error shall be returned if everything is good with StandardLRS storage account",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.StandardLRS,
			diskIOPSReadWrite:   "",
			diskMBPSReadWrite:   "",
			diskEncryptionSetID: goodDiskEncryptionSetID,
			expectedDiskID:      disk1ID,
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         false,
		},
		{
			desc:                "empty diskid and an error shall be returned if everything is good with UltraSSDLRS storage account but DiskIOPSReadWrite is invalid",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.UltraSSDLRS,
			diskIOPSReadWrite:   "invalid",
			diskMBPSReadWrite:   "100",
			diskEncryptionSetID: goodDiskEncryptionSetID,
			expectedDiskID:      "",
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         true,
			expectedErrMsg:      fmt.Errorf("AzureDisk - failed to parse DiskIOPSReadWrite: strconv.Atoi: parsing \"invalid\": invalid syntax"),
		},
		{
			desc:                "empty diskid and an error shall be returned if everything is good with UltraSSDLRS storage account but DiskMBPSReadWrite is invalid",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.UltraSSDLRS,
			diskIOPSReadWrite:   "100",
			diskMBPSReadWrite:   "invalid",
			diskEncryptionSetID: goodDiskEncryptionSetID,
			expectedDiskID:      "",
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         true,
			expectedErrMsg:      fmt.Errorf("AzureDisk - failed to parse DiskMBpsReadWrite: strconv.Atoi: parsing \"invalid\": invalid syntax"),
		},
		{
			desc:                "empty diskid and an error shall be returned if everything is good with UltraSSDLRS storage account with bad Disk EncryptionSetID",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.UltraSSDLRS,
			diskIOPSReadWrite:   "100",
			diskMBPSReadWrite:   "100",
			diskEncryptionSetID: badDiskEncryptionSetID,
			expectedDiskID:      "",
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         true,
			expectedErrMsg:      fmt.Errorf("AzureDisk - format of DiskEncryptionSetID(%s) is incorrect, correct format: %s", badDiskEncryptionSetID, consts.DiskEncryptionSetIDFormat),
		},
		{
			desc:                "DiskEncryptionType should be empty when DiskEncryptionSetID is not set",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.StandardLRS,
			diskEncryptionSetID: "",
			diskEncryptionType:  "EncryptionAtRestWithCustomerKey",
			expectedDiskID:      "",
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         true,
			expectedErrMsg:      fmt.Errorf("AzureDisk - DiskEncryptionType(EncryptionAtRestWithCustomerKey) should be empty when DiskEncryptionSetID is not set"),
		},
		{
			desc:                "disk Id and no error shall be returned if everything is good with StandardLRS storage account with not empty diskIOPSReadWrite",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.StandardLRS,
			diskIOPSReadWrite:   "100",
			diskMBPSReadWrite:   "",
			diskEncryptionSetID: goodDiskEncryptionSetID,
			expectedDiskID:      "",
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         true,
			expectedErrMsg:      fmt.Errorf("AzureDisk - DiskIOPSReadWrite parameter is only applicable in UltraSSD_LRS disk type"),
		},
		{
			desc:                "disk Id and no error shall be returned if everything is good with StandardLRS storage account with not empty diskMBPSReadWrite",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.StandardLRS,
			diskIOPSReadWrite:   "",
			diskMBPSReadWrite:   "100",
			diskEncryptionSetID: goodDiskEncryptionSetID,
			expectedDiskID:      "",
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         true,
			expectedErrMsg:      fmt.Errorf("AzureDisk - DiskMBpsReadWrite parameter is only applicable in UltraSSD_LRS disk type"),
		},
		{
			desc:                "correct NetworkAccessPolicy(DenyAll) setting",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.StandardLRS,
			diskEncryptionSetID: goodDiskEncryptionSetID,
			networkAccessPolicy: compute.DenyAll,
			publicNetworkAccess: compute.Disabled,
			expectedDiskID:      disk1ID,
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         false,
		},
		{
			desc:                "correct NetworkAccessPolicy(AllowAll) setting",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.StandardLRS,
			diskEncryptionSetID: goodDiskEncryptionSetID,
			diskEncryptionType:  "EncryptionAtRestWithCustomerKey",
			networkAccessPolicy: compute.AllowAll,
			publicNetworkAccess: compute.Enabled,
			expectedDiskID:      disk1ID,
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         false,
		},
		{
			desc:                "DiskAccessID should not be empty when NetworkAccessPolicy is AllowPrivate",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.StandardLRS,
			diskEncryptionSetID: goodDiskEncryptionSetID,
			networkAccessPolicy: compute.AllowPrivate,
			expectedDiskID:      "",
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         true,
			expectedErrMsg:      fmt.Errorf("DiskAccessID should not be empty when NetworkAccessPolicy is AllowPrivate"),
		},
		{
			desc:                "DiskAccessID(%s) must be empty when NetworkAccessPolicy(%s) is not AllowPrivate",
			diskID:              disk1ID,
			diskName:            disk1Name,
			storageAccountType:  compute.StandardLRS,
			diskEncryptionSetID: goodDiskEncryptionSetID,
			networkAccessPolicy: compute.AllowAll,
			diskAccessID:        pointer.String("diskAccessID"),
			expectedDiskID:      "",
			existedDisk:         compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:         true,
			expectedErrMsg:      fmt.Errorf("DiskAccessID(diskAccessID) must be empty when NetworkAccessPolicy(AllowAll) is not AllowPrivate"),
		},
		{
			desc:           "resourceGroup must be specified when subscriptionID is not empty",
			diskID:         "",
			diskName:       disk1Name,
			subscriptionID: "abc",
			resouceGroup:   "",
			expectedDiskID: "",
			existedDisk:    compute.Disk{ID: pointer.String(disk1ID), Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{Encryption: &compute.Encryption{DiskEncryptionSetID: &goodDiskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey}, ProvisioningState: pointer.String("Succeeded")}, Tags: testTags},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("resourceGroup must be specified when subscriptionID(abc) is not empty"),
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		managedDiskController := testCloud.ManagedDiskController
		volumeOptions := &ManagedDiskOptions{
			DiskName:            test.diskName,
			StorageAccountType:  test.storageAccountType,
			ResourceGroup:       test.resouceGroup,
			SizeGB:              1,
			Tags:                map[string]string{"tag1": "azure-tag1"},
			AvailabilityZone:    "westus-testzone",
			DiskIOPSReadWrite:   test.diskIOPSReadWrite,
			DiskMBpsReadWrite:   test.diskMBPSReadWrite,
			DiskEncryptionSetID: test.diskEncryptionSetID,
			DiskEncryptionType:  test.diskEncryptionType,
			MaxShares:           maxShare,
			NetworkAccessPolicy: test.networkAccessPolicy,
			PublicNetworkAccess: test.publicNetworkAccess,
			DiskAccessID:        test.diskAccessID,
			SubscriptionID:      test.subscriptionID,
		}

		mockDisksClient := testCloud.DisksClient.(*mockdiskclient.MockInterface)
		//disk := getTestDisk(test.diskName)
		mockDisksClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), test.diskName, gomock.Any()).Return(nil).AnyTimes()
		mockDisksClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), test.diskName).Return(test.existedDisk, nil).AnyTimes()

		actualDiskID, err := managedDiskController.CreateManagedDisk(ctx, volumeOptions)
		assert.Equal(t, test.expectedDiskID, actualDiskID, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, return error: %v", i, test.desc, err)
		if test.expectedErr {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), "TestCase[%d]: %s, expected error: %v, return error: %v", i, test.desc, test.expectedErrMsg, err)
		}
	}
}

func TestCreateManagedDiskWithExtendedLocation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCloud := GetTestCloudWithExtendedLocation(ctrl)
	diskName := disk1Name
	expectedDiskID := disk1ID
	el := &compute.ExtendedLocation{
		Name: pointer.String("microsoftlosangeles1"),
		Type: compute.ExtendedLocationTypesEdgeZone,
	}

	diskreturned := compute.Disk{
		ID:               pointer.String(expectedDiskID),
		Name:             pointer.String(diskName),
		ExtendedLocation: el,
		DiskProperties: &compute.DiskProperties{
			ProvisioningState: pointer.String("Succeeded"),
		},
	}

	managedDiskController := testCloud.ManagedDiskController
	volumeOptions := &ManagedDiskOptions{
		DiskName:           diskName,
		StorageAccountType: compute.PremiumLRS,
		ResourceGroup:      "",
		SizeGB:             1,
		AvailabilityZone:   "westus-testzone",
	}

	mockDisksClient := testCloud.DisksClient.(*mockdiskclient.MockInterface)
	mockDisksClient.EXPECT().CreateOrUpdate(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, diskName, gomock.Any()).
		Do(func(ctx interface{}, subsID, rg, dn string, disk compute.Disk) {
			assert.Equal(t, el.Name, disk.ExtendedLocation.Name, "The extended location name should match.")
			assert.Equal(t, el.Type, disk.ExtendedLocation.Type, "The extended location type should match.")
		}).Return(nil)

	mockDisksClient.EXPECT().Get(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, diskName).Return(diskreturned, nil).AnyTimes()

	actualDiskID, err := managedDiskController.CreateManagedDisk(ctx, volumeOptions)
	assert.Equal(t, expectedDiskID, actualDiskID, "Disk ID does not match.")
	assert.Nil(t, err, "There should not be an error.")
}

func TestDeleteManagedDisk(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc           string
		diskName       string
		diskState      string
		existedDisk    compute.Disk
		expectedErr    bool
		expectedErrMsg error
	}{
		{
			desc:           "an error shall be returned if delete an attaching disk",
			diskName:       disk1Name,
			diskState:      "attaching",
			existedDisk:    compute.Disk{Name: pointer.String(disk1Name)},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("failed to delete disk(/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/disks/disk1) since it's in attaching state"),
		},
		{
			desc:        "no error shall be returned if everything is good",
			diskName:    disk1Name,
			existedDisk: compute.Disk{Name: pointer.String(disk1Name)},
			expectedErr: false,
		},
		{
			desc:           "an error shall be returned if get disk failed",
			diskName:       fakeGetDiskFailed,
			existedDisk:    compute.Disk{Name: pointer.String(fakeGetDiskFailed)},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: Get Disk failed"),
		},
	}
	ctx, cancel := getContextWithCancel()
	defer cancel()

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		managedDiskController := testCloud.ManagedDiskController
		diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
			testCloud.SubscriptionID, testCloud.ResourceGroup, *test.existedDisk.Name)
		if test.diskState == "attaching" {
			managedDiskController.diskStateMap.Store(strings.ToLower(diskURI), test.diskState)
		}

		mockDisksClient := testCloud.DisksClient.(*mockdiskclient.MockInterface)
		if test.diskName == fakeGetDiskFailed {
			mockDisksClient.EXPECT().Get(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, test.diskName).Return(test.existedDisk, &retry.Error{RawError: fmt.Errorf("Get Disk failed")}).AnyTimes()
		} else {
			mockDisksClient.EXPECT().Get(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, test.diskName).Return(test.existedDisk, nil).AnyTimes()
		}
		mockDisksClient.EXPECT().Delete(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, test.diskName).Return(nil).AnyTimes()

		err := managedDiskController.DeleteManagedDisk(ctx, diskURI)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, return error: %v", i, test.desc, err)
		if test.expectedErr {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), "TestCase[%d]: %s, expected: %v, return: %v", i, test.desc, test.expectedErrMsg, err)
		}
	}
}

func TestGetDisk(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	testCases := []struct {
		desc                      string
		diskName                  string
		existedDisk               compute.Disk
		expectedErr               bool
		expectedErrMsg            error
		expectedProvisioningState string
		expectedDiskID            string
	}{
		{
			desc:                      "no error shall be returned if get a normal disk without DiskProperties",
			diskName:                  disk1Name,
			existedDisk:               compute.Disk{Name: pointer.String(disk1Name)},
			expectedErr:               false,
			expectedProvisioningState: "",
			expectedDiskID:            "",
		},
		{
			desc:                      "an error shall be returned if get disk failed",
			diskName:                  fakeGetDiskFailed,
			existedDisk:               compute.Disk{Name: pointer.String(fakeGetDiskFailed)},
			expectedErr:               true,
			expectedErrMsg:            fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: Get Disk failed"),
			expectedProvisioningState: "",
			expectedDiskID:            "",
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		managedDiskController := testCloud.ManagedDiskController

		mockDisksClient := testCloud.DisksClient.(*mockdiskclient.MockInterface)
		if test.diskName == fakeGetDiskFailed {
			mockDisksClient.EXPECT().Get(gomock.Any(), "", testCloud.ResourceGroup, test.diskName).Return(test.existedDisk, &retry.Error{RawError: fmt.Errorf("Get Disk failed")}).AnyTimes()
		} else {
			mockDisksClient.EXPECT().Get(gomock.Any(), "", testCloud.ResourceGroup, test.diskName).Return(test.existedDisk, nil).AnyTimes()
		}

		provisioningState, diskid, err := managedDiskController.GetDisk(ctx, "", testCloud.ResourceGroup, test.diskName)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, return error: %v", i, test.desc, err)
		if test.expectedErr {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), "TestCase[%d]: %s, expected: %v, return: %v", i, test.desc, test.expectedErrMsg, err)
		}
		assert.Equal(t, test.expectedProvisioningState, provisioningState, "TestCase[%d]: %s, expected ProvisioningState: %v, return ProvisioningState: %v", i, test.desc, test.expectedProvisioningState, provisioningState)
		assert.Equal(t, test.expectedDiskID, diskid, "TestCase[%d]: %s, expected DiskID: %v, return DiskID: %v", i, test.desc, test.expectedDiskID, diskid)
	}
}

func TestResizeDisk(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	diskSizeGB := int32(2)
	diskName := disk1Name
	fakeCreateDiskFailed := "fakeCreateDiskFailed"
	testCases := []struct {
		desc             string
		diskName         string
		oldSize          resource.Quantity
		newSize          resource.Quantity
		existedDisk      compute.Disk
		expectedQuantity resource.Quantity
		expectedErr      bool
		expectedErrMsg   error
	}{
		{
			desc:             "new quantity and no error shall be returned if everything is good",
			diskName:         diskName,
			oldSize:          *resource.NewQuantity(2*(1024*1024*1024), resource.BinarySI),
			newSize:          *resource.NewQuantity(3*(1024*1024*1024), resource.BinarySI),
			existedDisk:      compute.Disk{Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB, DiskState: compute.Unattached}},
			expectedQuantity: *resource.NewQuantity(3*(1024*1024*1024), resource.BinarySI),
			expectedErr:      false,
		},
		{
			desc:             "new quantity and no error shall be returned if everything is good with DiskProperties is null",
			diskName:         diskName,
			oldSize:          *resource.NewQuantity(2*(1024*1024*1024), resource.BinarySI),
			newSize:          *resource.NewQuantity(3*(1024*1024*1024), resource.BinarySI),
			existedDisk:      compute.Disk{Name: pointer.String(disk1Name)},
			expectedQuantity: *resource.NewQuantity(2*(1024*1024*1024), resource.BinarySI),
			expectedErr:      true,
			expectedErrMsg:   fmt.Errorf("DiskProperties of disk(%s) is nil", diskName),
		},
		{
			desc:             "new quantity and no error shall be returned if everything is good with disk already of greater or equal size than requested",
			diskName:         diskName,
			oldSize:          *resource.NewQuantity(1*(1024*1024*1024), resource.BinarySI),
			newSize:          *resource.NewQuantity(2*(1024*1024*1024), resource.BinarySI),
			existedDisk:      compute.Disk{Name: pointer.String(disk1Name), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB, DiskState: compute.Unattached}},
			expectedQuantity: *resource.NewQuantity(2*(1024*1024*1024), resource.BinarySI),
			expectedErr:      false,
		},
		{
			desc:             "an error shall be returned if everything is good but get disk failed",
			diskName:         fakeGetDiskFailed,
			oldSize:          *resource.NewQuantity(2*(1024*1024*1024), resource.BinarySI),
			newSize:          *resource.NewQuantity(3*(1024*1024*1024), resource.BinarySI),
			existedDisk:      compute.Disk{Name: pointer.String(fakeGetDiskFailed), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB, DiskState: compute.Unattached}},
			expectedQuantity: *resource.NewQuantity(2*(1024*1024*1024), resource.BinarySI),
			expectedErr:      true,
			expectedErrMsg:   fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: Get Disk failed"),
		},
		{
			desc:             "an error shall be returned if everything is good but create disk failed",
			diskName:         fakeCreateDiskFailed,
			oldSize:          *resource.NewQuantity(2*(1024*1024*1024), resource.BinarySI),
			newSize:          *resource.NewQuantity(3*(1024*1024*1024), resource.BinarySI),
			existedDisk:      compute.Disk{Name: pointer.String(fakeCreateDiskFailed), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB, DiskState: compute.Unattached}},
			expectedQuantity: *resource.NewQuantity(2*(1024*1024*1024), resource.BinarySI),
			expectedErr:      true,
			expectedErrMsg:   fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: Create Disk failed"),
		},
		{
			desc:             "an error shall be returned if disk is not in Unattached state",
			diskName:         fakeCreateDiskFailed,
			oldSize:          *resource.NewQuantity(2*(1024*1024*1024), resource.BinarySI),
			newSize:          *resource.NewQuantity(3*(1024*1024*1024), resource.BinarySI),
			existedDisk:      compute.Disk{Name: pointer.String(fakeCreateDiskFailed), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB, DiskState: compute.Attached}},
			expectedQuantity: *resource.NewQuantity(2*(1024*1024*1024), resource.BinarySI),
			expectedErr:      true,
			expectedErrMsg:   fmt.Errorf("azureDisk - disk resize is only supported on Unattached disk, current disk state: Attached, already attached to "),
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		managedDiskController := testCloud.ManagedDiskController
		diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
			testCloud.SubscriptionID, testCloud.ResourceGroup, *test.existedDisk.Name)

		mockDisksClient := testCloud.DisksClient.(*mockdiskclient.MockInterface)
		if test.diskName == fakeGetDiskFailed {
			mockDisksClient.EXPECT().Get(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, test.diskName).Return(test.existedDisk, &retry.Error{RawError: fmt.Errorf("Get Disk failed")}).AnyTimes()
		} else {
			mockDisksClient.EXPECT().Get(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, test.diskName).Return(test.existedDisk, nil).AnyTimes()
		}
		if test.diskName == fakeCreateDiskFailed {
			mockDisksClient.EXPECT().Update(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, test.diskName, gomock.Any()).Return(&retry.Error{RawError: fmt.Errorf("Create Disk failed")}).AnyTimes()
		} else {
			mockDisksClient.EXPECT().Update(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, test.diskName, gomock.Any()).Return(nil).AnyTimes()
		}

		result, err := managedDiskController.ResizeDisk(ctx, diskURI, test.oldSize, test.newSize, false)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, return error: %v", i, test.desc, err)
		if test.expectedErr {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), "TestCase[%d]: %s, expected: %v, return: %v", i, test.desc, test.expectedErrMsg, err)
		}
		assert.Equal(t, test.expectedQuantity.Value(), result.Value(), "TestCase[%d]: %s, expected Quantity: %v, return Quantity: %v", i, test.desc, test.expectedQuantity, result)
	}
}

func TestGetLabelsForVolume(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCloud0 := GetTestCloud(ctrl)
	diskName := disk1Name
	diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
		testCloud0.SubscriptionID, testCloud0.ResourceGroup, diskName)
	diskSizeGB := int32(30)
	fakeGetDiskFailedDiskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
		testCloud0.SubscriptionID, testCloud0.ResourceGroup, fakeGetDiskFailed)
	testCases := []struct {
		desc           string
		diskName       string
		pv             *v1.PersistentVolume
		existedDisk    compute.Disk
		expected       map[string]string
		expectedErr    bool
		expectedErrMsg error
	}{
		{
			desc:     "labels and no error shall be returned if everything is good",
			diskName: diskName,
			pv: &v1.PersistentVolume{
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						AzureDisk: &v1.AzureDiskVolumeSource{
							DiskName:    diskName,
							DataDiskURI: diskURI,
						},
					},
				},
			},
			existedDisk: compute.Disk{Name: pointer.String(diskName), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB}, Zones: &[]string{"1"}},
			expected: map[string]string{
				consts.LabelFailureDomainBetaRegion: testCloud0.Location,
				consts.LabelFailureDomainBetaZone:   testCloud0.makeZone(testCloud0.Location, 1),
			},
			expectedErr: false,
		},
		{
			desc:     "an error shall be returned if everything is good with invalid zone",
			diskName: diskName,
			pv: &v1.PersistentVolume{
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						AzureDisk: &v1.AzureDiskVolumeSource{
							DiskName:    diskName,
							DataDiskURI: diskURI,
						},
					},
				},
			},
			existedDisk:    compute.Disk{Name: pointer.String(diskName), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB}, Zones: &[]string{"invalid"}},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("failed to parse zone [invalid] for AzureDisk %v: %v", diskName, "strconv.Atoi: parsing \"invalid\": invalid syntax"),
		},
		{
			desc:     "nil shall be returned if everything is good with null Zones",
			diskName: diskName,
			pv: &v1.PersistentVolume{
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						AzureDisk: &v1.AzureDiskVolumeSource{
							DiskName:    diskName,
							DataDiskURI: diskURI,
						},
					},
				},
			},
			existedDisk: compute.Disk{Name: pointer.String(diskName), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB}},
			expected: map[string]string{
				consts.LabelFailureDomainBetaRegion: testCloud0.Location,
			},
			expectedErr:    false,
			expectedErrMsg: nil,
		},
		{
			desc:     "an error shall be returned if everything is good with get disk failed",
			diskName: fakeGetDiskFailed,
			pv: &v1.PersistentVolume{
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						AzureDisk: &v1.AzureDiskVolumeSource{
							DiskName:    fakeGetDiskFailed,
							DataDiskURI: fakeGetDiskFailedDiskURI,
						},
					},
				},
			},
			existedDisk:    compute.Disk{Name: pointer.String(fakeGetDiskFailed), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB}, Zones: &[]string{"1"}},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: Get Disk failed"),
		},
		{
			desc:     "an error shall be returned if everything is good with invalid DiskURI",
			diskName: diskName,
			pv: &v1.PersistentVolume{
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						AzureDisk: &v1.AzureDiskVolumeSource{
							DiskName:    diskName,
							DataDiskURI: "invalidDiskURI",
						},
					},
				},
			},
			existedDisk:    compute.Disk{Name: pointer.String(diskName), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB}, Zones: &[]string{"1"}},
			expectedErr:    true,
			expectedErrMsg: fmt.Errorf("invalid disk URI: invalidDiskURI"),
		},
		{
			desc:     "nil shall be returned if everything is good but pv.Spec.AzureDisk.DiskName is cloudvolume.ProvisionedVolumeName",
			diskName: diskName,
			pv: &v1.PersistentVolume{
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{
						AzureDisk: &v1.AzureDiskVolumeSource{
							DiskName:    cloudvolume.ProvisionedVolumeName,
							DataDiskURI: diskURI,
						},
					},
				},
			},
			existedDisk: compute.Disk{Name: pointer.String(diskName), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB}},
			expected:    nil,
			expectedErr: false,
		},
		{
			desc:     "nil shall be returned if everything is good but pv.Spec.AzureDisk is nil",
			diskName: diskName,
			pv: &v1.PersistentVolume{
				Spec: v1.PersistentVolumeSpec{
					PersistentVolumeSource: v1.PersistentVolumeSource{},
				},
			},
			existedDisk: compute.Disk{Name: pointer.String(diskName), DiskProperties: &compute.DiskProperties{DiskSizeGB: &diskSizeGB}},
			expected:    nil,
			expectedErr: false,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		mockDisksClient := testCloud.DisksClient.(*mockdiskclient.MockInterface)
		if test.diskName == fakeGetDiskFailed {
			mockDisksClient.EXPECT().Get(gomock.Any(), testCloud.SubscriptionID, testCloud.ResourceGroup, test.diskName).Return(test.existedDisk, &retry.Error{RawError: fmt.Errorf("Get Disk failed")}).AnyTimes()
		} else {
			mockDisksClient.EXPECT().Get(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, test.diskName).Return(test.existedDisk, nil).AnyTimes()
		}
		mockDisksClient.EXPECT().CreateOrUpdate(gomock.Any(), "", testCloud.ResourceGroup, test.diskName, gomock.Any()).Return(nil).AnyTimes()

		result, err := testCloud.GetLabelsForVolume(context.TODO(), test.pv)
		assert.Equal(t, test.expected, result, "TestCase[%d]: %s, expected: %v, return: %v", i, test.desc, test.expected, result)
		assert.Equal(t, test.expectedErr, err != nil, "TestCase[%d]: %s, return error: %v", i, test.desc, err)
		if test.expectedErr {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), "TestCase[%d]: %s, expected: %v, return: %v", i, test.desc, test.expectedErrMsg, err)
		}
	}
}
