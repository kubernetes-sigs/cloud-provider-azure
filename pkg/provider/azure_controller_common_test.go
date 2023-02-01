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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-03-01/compute"
	"github.com/Azure/go-autorest/autorest/azure"
	autorestmocks "github.com/Azure/go-autorest/autorest/mocks"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/diskclient/mockdiskclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssvmclient/mockvmssvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

var (
	operationPreemptedError      = retry.NewError(false, errors.New(`Code="OperationPreempted" Message="Operation execution has been preempted by a more recent operation."`))
	conflictingUserInputError    = retry.NewError(false, errors.New(`Code="ConflictingUserInput" Message="Cannot attach the disk pvc-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxx to VM /subscriptions/xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxx/resourceGroups/test-rg/providers/Microsoft.Compute/virtualMachineScaleSets/aks-nodepool0-00000000-vmss/virtualMachines/aks-nodepool0-00000000-vmss_0 because it is already attached to VM /subscriptions/xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxx/resourceGroups/test-rg/providers/Microsoft.Compute/virtualMachineScaleSets/aks-nodepool0-00000000-vmss/virtualMachines/aks-nodepool0-00000000-vmss_1. A disk can be attached to only one VM at a time."`))
	vmExtensionProvisioningError = retry.NewError(false, errors.New(`Code="VMExtensionProvisioningError" Message="Multiple VM extensions failed to be provisioned on the VM. Please see the VM extension instance view for other failures. The first extension failed due to the error: VM 'aks-nodepool0-00000000-vmss_00' has not reported status for VM agent or extensions. Verify that the OS is up and healthy, the VM has a running VM agent, and that it can establish outbound connections to Azure storage. Please refer to https://aka.ms/vmextensionlinuxtroubleshoot for additional VM agent troubleshooting information."`))
)

func fakeUpdateAsync(statusCode int) func(context.Context, string, string, compute.VirtualMachineUpdate, string) (*azure.Future, *retry.Error) {
	return func(ctx context.Context, resourceGroup, nodeName string, parameters compute.VirtualMachineUpdate, source string) (*azure.Future, *retry.Error) {
		vm := &compute.VirtualMachine{
			Name:                     &nodeName,
			Plan:                     parameters.Plan,
			VirtualMachineProperties: parameters.VirtualMachineProperties,
			Identity:                 parameters.Identity,
			Zones:                    parameters.Zones,
			Tags:                     parameters.Tags,
		}
		s, err := json.Marshal(vm)
		if err != nil {
			return nil, retry.NewError(false, err)
		}

		body := autorestmocks.NewBodyWithBytes(s)

		r := autorestmocks.NewResponseWithBodyAndStatus(body, statusCode, strconv.Itoa(statusCode))
		r.Request.Method = http.MethodPut

		f, err := azure.NewFutureFromResponse(r)
		if err != nil {
			return nil, retry.NewError(false, err)
		}

		return &f, nil
	}
}

func TestCommonAttachDisk(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := getContextWithCancel()
	defer cancel()

	initVM := func(testCloud *Cloud, expectedVMs []compute.VirtualMachine) {
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		if len(expectedVMs) == 0 {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		}
	}

	defaultSetup := func(testCloud *Cloud, expectedVMs []compute.VirtualMachine, statusCode int, result *retry.Error) {
		initVM(testCloud, expectedVMs)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMsClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(fakeUpdateAsync(statusCode)).MaxTimes(1)
		mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).MaxTimes(1)
		mockVMsClient.EXPECT().WaitForUpdateResult(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, gomock.Any()).Return(nil, result).MaxTimes(1)
	}

	maxShare := int32(1)
	goodInstanceID := fmt.Sprintf("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/%s", "vm1")
	diskEncryptionSetID := fmt.Sprintf("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/diskEncryptionSets/%s", "diskEncryptionSet-name")
	testTags := make(map[string]*string)
	testTags[WriteAcceleratorEnabled] = pointer.String("true")
	testCases := []struct {
		desc                 string
		diskName             string
		existedDisk          *compute.Disk
		nodeName             types.NodeName
		vmList               map[string]string
		isDataDisksFull      bool
		isBadDiskURI         bool
		isDiskUsed           bool
		isLunChUsed          bool
		setup                func(testCloud *Cloud, expectedVMs []compute.VirtualMachine, statusCode int, result *retry.Error)
		expectErr            bool
		isAcceptedErr        bool
		isContextDeadlineErr bool
		statusCode           int
		waitResult           *retry.Error
		expectedLun          int32
		contextDuration      time.Duration
	}{
		{
			desc:        "correct LUN and no error shall be returned if disk is nil",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk-name",
			existedDisk: nil,
			expectedLun: 3,
			expectErr:   false,
			statusCode:  200,
		},
		{
			desc:        "LUN -1 and error shall be returned if there's no such instance corresponding to given nodeName",
			nodeName:    "vm1",
			diskName:    "disk-name",
			existedDisk: &compute.Disk{Name: pointer.String("disk-name")},
			expectedLun: -1,
			expectErr:   true,
		},
		{
			desc:            "LUN -1 and error shall be returned if there's no available LUN for instance",
			vmList:          map[string]string{"vm1": "PowerState/Running"},
			nodeName:        "vm1",
			isDataDisksFull: true,
			diskName:        "disk-name",
			existedDisk:     &compute.Disk{Name: pointer.String("disk-name")},
			expectedLun:     -1,
			expectErr:       true,
		},
		{
			desc:        "correct LUN and no error shall be returned if everything is good",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk-name",
			isLunChUsed: true,
			existedDisk: &compute.Disk{Name: to.StringPtr("disk-name"),
				DiskProperties: &compute.DiskProperties{
					Encryption: &compute.Encryption{DiskEncryptionSetID: &diskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey},
					DiskSizeGB: to.Int32Ptr(4096),
					DiskState:  compute.Unattached,
				},
				Tags: testTags},
			expectedLun: 3,
			expectErr:   false,
			statusCode:  200,
		},
		{
			desc:     "early assigned lun value should be available via lun channel if lun channel is given",
			vmList:   map[string]string{"vm1": "PowerState/Running"},
			nodeName: "vm1",
			diskName: "disk-name",
			existedDisk: &compute.Disk{Name: pointer.String("disk-name"),
				DiskProperties: &compute.DiskProperties{
					Encryption: &compute.Encryption{DiskEncryptionSetID: &diskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey},
					DiskSizeGB: pointer.Int32(4096),
					DiskState:  compute.Unattached,
				},
				Tags: testTags},
			expectedLun: 3,
			expectErr:   false,
			statusCode:  200,
		},
		{
			desc:     "an error shall be returned if disk state is not Unattached",
			vmList:   map[string]string{"vm1": "PowerState/Running"},
			nodeName: "vm1",
			diskName: "disk-name",
			existedDisk: &compute.Disk{Name: pointer.String("disk-name"),
				DiskProperties: &compute.DiskProperties{
					Encryption: &compute.Encryption{DiskEncryptionSetID: &diskEncryptionSetID, Type: compute.EncryptionTypeEncryptionAtRestWithCustomerKey},
					DiskSizeGB: pointer.Int32(4096),
					DiskState:  compute.Attached,
				},
				Tags: testTags},
			expectedLun: -1,
			expectErr:   true,
		},
		{
			desc:        "an error shall be returned if attach an already attached disk with good ManagedBy instance id",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk-name",
			existedDisk: &compute.Disk{Name: pointer.String("disk-name"), ManagedBy: pointer.String(goodInstanceID), DiskProperties: &compute.DiskProperties{MaxShares: &maxShare}},
			expectedLun: -1,
			expectErr:   true,
		},
		{
			desc:          "should return a PartialUpdateError type when storage configuration was accepted but wait fails with an error",
			vmList:        map[string]string{"vm1": "PowerState/Running"},
			nodeName:      "vm1",
			diskName:      "disk-name",
			existedDisk:   nil,
			expectedLun:   -1,
			expectErr:     true,
			statusCode:    200,
			waitResult:    conflictingUserInputError,
			isAcceptedErr: true,
		},
		{
			desc:        "should not return a PartialUpdateError type when storage configuration was not accepted",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk-name",
			existedDisk: nil,
			expectedLun: -1,
			expectErr:   true,
			statusCode:  400,
			waitResult:  conflictingUserInputError,
		},
		{
			desc:        "should retry on OperationPreempted error and succeed",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk-name",
			existedDisk: nil,
			expectedLun: 3,
			expectErr:   false,
			statusCode:  200,
			waitResult:  operationPreemptedError,
			setup: func(testCloud *Cloud, expectedVMs []compute.VirtualMachine, statusCode int, result *retry.Error) {
				defaultSetup(testCloud, expectedVMs, statusCode, result)
				mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
				mockVMsClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(fakeUpdateAsync(200)).AnyTimes()
				gomock.InOrder(
					mockVMsClient.EXPECT().WaitForUpdateResult(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, gomock.Any()).Return(nil, result),
					mockVMsClient.EXPECT().WaitForUpdateResult(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, gomock.Any()).Return(nil, nil),
				)
			},
		},
		{
			desc:        "should retry on OperationPreempted error until context deadline and return context.DeadlineExceeded error",
			vmList:      map[string]string{"vm1": "PowerState/Running"},
			nodeName:    "vm1",
			diskName:    "disk-name",
			existedDisk: nil,
			expectedLun: -1,
			expectErr:   true,
			statusCode:  200,
			waitResult:  operationPreemptedError,
			setup: func(testCloud *Cloud, expectedVMs []compute.VirtualMachine, statusCode int, result *retry.Error) {
				defaultSetup(testCloud, expectedVMs, statusCode, result)
				mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
				mockVMsClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(fakeUpdateAsync(200)).AnyTimes()
				mockVMsClient.EXPECT().WaitForUpdateResult(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, gomock.Any()).Return(nil, result).AnyTimes()
			},
			contextDuration:      time.Second,
			isContextDeadlineErr: true,
		},
	}

	for i, test := range testCases {
		tt := test
		t.Run(tt.desc, func(t *testing.T) {
			testCloud := GetTestCloud(ctrl)
			diskURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
				testCloud.SubscriptionID, testCloud.ResourceGroup, test.diskName)
			if tt.isBadDiskURI {
				diskURI = fmt.Sprintf("/baduri/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/disks/%s",
					testCloud.SubscriptionID, testCloud.ResourceGroup, tt.diskName)
			}
			expectedVMs := setTestVirtualMachines(testCloud, tt.vmList, tt.isDataDisksFull)
			if tt.isDiskUsed {
				vm0 := setTestVirtualMachines(testCloud, map[string]string{"vm0": "PowerState/Running"}, tt.isDataDisksFull)[0]
				expectedVMs = append(expectedVMs, vm0)
			}
			if tt.setup == nil {
				defaultSetup(testCloud, expectedVMs, tt.statusCode, tt.waitResult)
			} else {
				tt.setup(testCloud, expectedVMs, tt.statusCode, tt.waitResult)
			}

			if tt.contextDuration > 0 {
				oldCtx := ctx
				ctx, cancel = context.WithTimeout(oldCtx, tt.contextDuration)
				defer cancel()
				defer func() {
					ctx = oldCtx
				}()
			}

			complete := make(chan bool)
			defer close(complete)
			if tt.isLunChUsed {
				lunCh := make(chan int32, 1)
				oldCtx := ctx
				ctx = context.WithValue(ctx, LunChannelContextKey, lunCh)
				go func() {
					t.Run("listen to lun channel", func(t *testing.T) {
						lun := <-lunCh
						assert.Equal(t, tt.expectedLun, lun)
						ctx = oldCtx
					})
				}()
			}

			lun, err := testCloud.AttachDisk(ctx, true, "", diskURI, tt.nodeName, compute.CachingTypesReadOnly, tt.existedDisk)

			assert.Equal(t, tt.expectedLun, lun, "TestCase[%d]: %s", i, tt.desc)
			assert.Equal(t, tt.expectErr, err != nil, "TestCase[%d]: %s, return error: %v", i, tt.desc, err)

			if tt.isAcceptedErr {
				assert.IsType(t, &retry.PartialUpdateError{}, err)
			} else {
				var partialUpdateError *retry.PartialUpdateError
				ok := errors.As(err, &partialUpdateError)
				assert.False(t, ok, "the returned error should not be AcceptedError type")
			}

			assert.Equal(t, tt.isContextDeadlineErr, errors.Is(err, context.DeadlineExceeded))
		})
	}
}

func TestWaitForUpdateResult(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defaultSetup := func(testCloud *Cloud, statusCode int, result *retry.Error) {
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMsClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(fakeUpdateAsync(statusCode)).MaxTimes(1)
		mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).MaxTimes(1)
		mockVMsClient.EXPECT().WaitForUpdateResult(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, gomock.Any()).Return(nil, result).MaxTimes(1)
	}

	testCases := []struct {
		desc              string
		nodeName          string
		statusCode        int
		waitResult        *retry.Error
		expectErr         bool
		expectedErrorType error
		setup             func(testCloud *Cloud, statusCode int, result *retry.Error)
		contextDuration   time.Duration
	}{
		{
			desc:              "should return a PartialUpdateError type when storage configuration was accepted but wait fails with an error",
			nodeName:          "vm1",
			expectErr:         true,
			statusCode:        200,
			waitResult:        conflictingUserInputError,
			expectedErrorType: &retry.PartialUpdateError{},
		},
		{
			desc:              "should return a PartialUpdateError type when storage configuration was accepted but wait fails with an error",
			nodeName:          "vm1",
			expectErr:         true,
			statusCode:        200,
			waitResult:        conflictingUserInputError,
			expectedErrorType: &retry.PartialUpdateError{},
		},
		{
			desc:              "should not return a PartialUpdateError type when storage configuration was not accepted",
			nodeName:          "vm1",
			expectErr:         true,
			statusCode:        400,
			waitResult:        conflictingUserInputError,
			expectedErrorType: conflictingUserInputError.Error(),
		},
		{
			desc:       "should retry on OperationPreempted error and succeed",
			nodeName:   "vm1",
			expectErr:  false,
			statusCode: 200,
			waitResult: operationPreemptedError,
			setup: func(testCloud *Cloud, statusCode int, result *retry.Error) {
				defaultSetup(testCloud, statusCode, result)
				mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
				mockVMsClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(fakeUpdateAsync(200))
				gomock.InOrder(
					mockVMsClient.EXPECT().WaitForUpdateResult(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, gomock.Any()).Return(nil, result),
					mockVMsClient.EXPECT().WaitForUpdateResult(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, gomock.Any()).Return(nil, nil),
				)
			},
		},
		{
			desc:     "should retry on OperationPreempted error until context deadline and return context.DeadlineExceeded error",
			nodeName: "vm1",

			expectErr:  true,
			statusCode: 200,
			waitResult: operationPreemptedError,
			setup: func(testCloud *Cloud, statusCode int, result *retry.Error) {
				defaultSetup(testCloud, statusCode, result)
				mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
				mockVMsClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(fakeUpdateAsync(200)).AnyTimes()
				mockVMsClient.EXPECT().WaitForUpdateResult(gomock.Any(), gomock.Any(), testCloud.ResourceGroup, gomock.Any()).Return(nil, result).AnyTimes()
			},
			contextDuration:   time.Second,
			expectedErrorType: context.DeadlineExceeded,
		},
	}

	for i, test := range testCases {
		tt := test
		t.Run(tt.desc, func(t *testing.T) {
			testCloud := GetTestCloud(ctrl)
			if tt.setup == nil {
				defaultSetup(testCloud, tt.statusCode, tt.waitResult)
			} else {
				tt.setup(testCloud, tt.statusCode, tt.waitResult)
			}

			if tt.contextDuration > 0 {
				oldCtx := ctx
				ctx, cancel = context.WithTimeout(oldCtx, tt.contextDuration)
				defer cancel()
				defer func() {
					ctx = oldCtx
				}()
			}

			r := autorestmocks.NewResponseWithStatus(strconv.Itoa(tt.statusCode), tt.statusCode)
			r.Request.Method = http.MethodPut
			future, _ := azure.NewFutureFromResponse(r)

			err := testCloud.waitForUpdateResult(ctx, testCloud.VMSet, types.NodeName(tt.nodeName), &future, nil)

			assert.Equal(t, tt.expectErr, err != nil, "TestCase[%d]: %s, return error: %v", i, tt.desc, err)

			if tt.expectErr {
				assert.IsType(t, tt.expectedErrorType, err)
			}
		})
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
			existedDisk: &compute.Disk{Name: pointer.String("disk-name")},
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
			existedDisk: &compute.Disk{Name: pointer.String("disk-name")},
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
				expectedVMSS := compute.VirtualMachineScaleSet{Name: pointer.String(testVMSSName)}
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
			ss, err := newScaleSet(context.Background(), testCloud)
			assert.NoError(t, err)
			testCloud.VMSet = ss
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
			mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
		}

		lun, err := testCloud.AttachDisk(ctx, true, "test", diskURI, test.nodeName, compute.CachingTypesReadOnly, test.existedDisk)
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
		mockVMsClient.EXPECT().Update(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

		err := testCloud.DetachDisk(ctx, test.diskName, diskURI, test.nodeName)
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
			cloud:   testCloud,
			lockMap: newLockMap(),
		}
		expectedVMs := setTestVirtualMachines(testCloud, test.vmList, false)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		if len(expectedVMs) == 0 {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		}
		r := autorestmocks.NewResponseWithStatus("200", 200)
		r.Request.Method = http.MethodPut

		future, err := azure.NewFutureFromResponse(r)

		mockVMsClient.EXPECT().UpdateAsync(gomock.Any(), testCloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(&future, err).AnyTimes()

		if test.isErrorRetriable {
			testCloud.CloudProviderBackoff = true
			testCloud.ResourceRequestBackoff = wait.Backoff{Steps: 1}
			mockVMsClient.EXPECT().WaitForUpdateResult(ctx, &future, testCloud.ResourceGroup, gomock.Any()).Return(nil, &retry.Error{HTTPStatusCode: http.StatusBadRequest, Retriable: true, RawError: fmt.Errorf("Retriable: true")}).AnyTimes()
		} else {
			mockVMsClient.EXPECT().WaitForUpdateResult(ctx, &future, testCloud.ResourceGroup, gomock.Any()).Return(nil, nil).AnyTimes()
		}

		err = common.UpdateVM(ctx, test.nodeName)
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
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}

		lun, _, err := testCloud.GetDiskLun(test.diskName, test.diskURI, "vm1")
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
			diskMap:     map[string]*AttachDiskOptions{"diskURI": {}},
			expectedErr: false,
		},
		{
			desc:            "LUN -1 and error shall be returned if there's no available LUN",
			nodeName:        "nodeName",
			diskMap:         map[string]*AttachDiskOptions{"diskURI": {}},
			isDataDisksFull: true,
			expectedErr:     true,
		},
	}

	for i, test := range testCases {
		testCloud := GetTestCloud(ctrl)
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{test.nodeName: "PowerState/Running"}, test.isDataDisksFull)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}

		err := testCloud.SetDiskLun(types.NodeName(test.nodeName), test.diskMap)
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
		expectedVMs := setTestVirtualMachines(testCloud, map[string]string{"vm1": "PowerState/Running"}, false)
		mockVMsClient := testCloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), testCloud.ResourceGroup, "vm2", gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()

		attached, err := testCloud.DisksAreAttached(test.diskNames, test.nodeName)
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
				CreateOption: compute.Empty,
			},
			expected2: nil,
		},
		{
			subscriptionID:   "",
			resourceGroup:    "",
			sourceResourceID: "/subscriptions/xxx/resourceGroups/xxx/providers/Microsoft.Compute/snapshots/xxx",
			sourceType:       sourceSnapshot,
			expected1: compute.CreationData{
				CreateOption:     compute.Copy,
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
				CreateOption:     compute.Copy,
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
				CreateOption: compute.Empty,
			},
			expected2: nil,
		},
		{
			subscriptionID:   "",
			resourceGroup:    "",
			sourceResourceID: "/subscriptions/xxx/resourceGroups/xxx/providers/Microsoft.Compute/disks/xxx",
			sourceType:       sourceVolume,
			expected1: compute.CreationData{
				CreateOption:     compute.Copy,
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
				CreateOption:     compute.Copy,
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
		exist, err := testCloud.checkDiskExists(ctx, test.diskURI)
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

	filteredDisks := testCloud.filterNonExistingDisks(ctx, disks)
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

	filteredDisks := testCloud.filterNonExistingDisks(ctx, disks)
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

func TestGetAzureErrorCode(t *testing.T) {
	testCases := []struct {
		desc           string
		err            error
		expectedResult string
	}{
		{
			desc:           "should return OperationPreempted",
			err:            operationPreemptedError.Error(),
			expectedResult: consts.OperationPreemptedErrorCode,
		},
		{
			desc:           "should return VMExtensionprovisioning",
			err:            vmExtensionProvisioningError.Error(),
			expectedResult: "VMExtensionProvisioningError",
		},
		{
			desc:           "should return ConflictingUserInput",
			err:            conflictingUserInputError.Error(),
			expectedResult: "ConflictingUserInput",
		},
	}
	for i, test := range testCases {
		tt := test
		t.Run(tt.desc, func(t *testing.T) {
			result := getAzureErrorCode(test.err)
			assert.Equal(t, test.expectedResult, result, "TestCase[%d]", i, result)
		})
	}
}

func TestVmUpdateRequired(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	r := autorestmocks.NewResponseWithStatus("200", 200)
	r.Request.Method = http.MethodPut

	acceptedFuture, _ := azure.NewFutureFromResponse(r)

	r = autorestmocks.NewResponseWithStatus("400", 400)
	r.Request.Method = http.MethodPut

	rejectedFuture, _ := azure.NewFutureFromResponse(r)

	testCases := []struct {
		desc           string
		configAccepted bool
		err            error
		expectedResult bool
	}{
		{
			desc:           "should return true if OperationPreemption error is returned with a http status code 2xx",
			configAccepted: true,
			err:            operationPreemptedError.Error(),
			expectedResult: true,
		},
		{
			desc:           "should return false if OperationPreemption error is returned with a http status code != 2xx",
			configAccepted: false,
			err:            operationPreemptedError.Error(),
			expectedResult: false,
		},
		{
			desc:           "should return false if ConflictingUserInputError error is returned even if http status code == 2xx",
			configAccepted: true,
			err:            conflictingUserInputError.Error(),
			expectedResult: false,
		},
	}
	for i, test := range testCases {
		tt := test
		t.Run(tt.desc, func(t *testing.T) {
			var future *azure.Future
			if tt.configAccepted {
				future = &acceptedFuture
			} else {
				future = &rejectedFuture
			}
			result := vmUpdateRequired(future, test.err)
			assert.Equalf(t, test.expectedResult, result, "TestCase[%d] returned %v", i, result)
		})
	}
}

func TestVMConfigAccepted(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc           string
		statusCode     int
		expectedResult bool
	}{
		{
			desc:           "should return true if HTTP status code is 2xx",
			statusCode:     200,
			expectedResult: true,
		},
		{
			desc:           "should return false if HTTP status code is not 2xx",
			statusCode:     400,
			expectedResult: false,
		},
	}

	for i, testCase := range testCases {
		tt := testCase
		t.Run(tt.desc, func(t *testing.T) {
			r := autorestmocks.NewResponseWithStatus(strconv.Itoa(tt.statusCode), tt.statusCode)
			r.Request.Method = http.MethodPut

			future, _ := azure.NewFutureFromResponse(r)

			result := VMConfigAccepted(&future)
			assert.Equalf(t, tt.expectedResult, result, "TestCase[%d] returned %v", i, result)
		})
	}
}
