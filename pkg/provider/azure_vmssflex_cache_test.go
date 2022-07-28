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

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	cloudprovider "k8s.io/cloud-provider"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

var (
	testVMWithoutInstanceView1 = compute.VirtualMachine{
		Name: to.StringPtr("testvm1"),
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			ProvisioningState: nil,
			VirtualMachineScaleSet: &compute.SubResource{
				ID: to.StringPtr("subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1"),
			},
		},
	}

	testVMWithoutInstanceView2 = compute.VirtualMachine{
		Name: to.StringPtr("testvm2"),
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			ProvisioningState: nil,
			VirtualMachineScaleSet: &compute.SubResource{
				ID: to.StringPtr("subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex2"),
			},
		},
	}
	testVMListWithoutInstanceView = []compute.VirtualMachine{testVMWithoutInstanceView1, testVMWithoutInstanceView2}

	testVMWithOnlyInstanceView1 = compute.VirtualMachine{
		Name: to.StringPtr("testvm1"),
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			InstanceView: &compute.VirtualMachineInstanceView{
				Statuses: &[]compute.InstanceViewStatus{
					{
						Code: to.StringPtr("PowerState/running"),
					},
				},
			},
		},
	}

	testVMWithOnlyInstanceView2 = compute.VirtualMachine{
		Name: to.StringPtr("testvm2"),
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			InstanceView: &compute.VirtualMachineInstanceView{
				Statuses: &[]compute.InstanceViewStatus{
					{
						Code: to.StringPtr("PowerState/running"),
					},
				},
			},
		},
	}
	testVMListWithOnlyInstanceView = []compute.VirtualMachine{testVMWithOnlyInstanceView1, testVMWithOnlyInstanceView2}

	testVM1 = compute.VirtualMachine{
		Name: to.StringPtr("testvm1"),
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			ProvisioningState: nil,
			VirtualMachineScaleSet: &compute.SubResource{
				ID: to.StringPtr("subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1"),
			},
			InstanceView: &compute.VirtualMachineInstanceView{
				Statuses: &[]compute.InstanceViewStatus{
					{
						Code: to.StringPtr("PowerState/running"),
					},
				},
			},
		},
	}

	testVmssFlex1 = compute.VirtualMachineScaleSet{
		ID:   to.StringPtr("subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1"),
		Name: to.StringPtr("vmssflex1"),
		VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
			VirtualMachineProfile: &compute.VirtualMachineScaleSetVMProfile{},
			OrchestrationMode:     compute.OrchestrationModeFlexible,
		},
	}

	testVmssFlex2 = compute.VirtualMachineScaleSet{
		ID:   to.StringPtr("subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex2"),
		Name: to.StringPtr("vmssflex2"),
		VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
			VirtualMachineProfile: &compute.VirtualMachineScaleSetVMProfile{},
			OrchestrationMode:     compute.OrchestrationModeFlexible,
		},
	}

	testVmssFlexList = []compute.VirtualMachineScaleSet{testVmssFlex1, testVmssFlex2}
)

func TestGetNodeVmssFlexID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       string
		testVM                         compute.VirtualMachine
		vmGetErr                       *retry.Error
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
		vmListErr                      error
		expectedVmssFlexID             string
		expectedErr                    error
	}{
		{
			description:                    "getNodeVmssFlexID should return the VmssFlex ID that the node belongs to",
			nodeName:                       "testvm1",
			testVM:                         testVMWithoutInstanceView1,
			vmGetErr:                       nil,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedVmssFlexID:             "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1",
			expectedErr:                    nil,
		},
		{
			description:                    "getNodeVmssFlexID should throw InstanceNotFound error if the VM cannot be found",
			nodeName:                       "testvm3",
			testVM:                         compute.VirtualMachine{},
			vmGetErr:                       &retry.Error{HTTPStatusCode: http.StatusNotFound},
			testVMListWithoutInstanceView:  []compute.VirtualMachine{},
			testVMListWithOnlyInstanceView: []compute.VirtualMachine{},
			vmListErr:                      nil,
			expectedVmssFlexID:             "",
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMClient := fs.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), fs.ResourceGroup, tc.nodeName, gomock.Any()).Return(tc.testVM, tc.vmGetErr).AnyTimes()

		mockVMClient.EXPECT().ListVmssFlexVMsWithoutInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		vmssFlexID, err := fs.getNodeVmssFlexID(tc.nodeName)
		assert.Equal(t, tc.expectedErr, err, tc.description)
		assert.Equal(t, tc.expectedVmssFlexID, vmssFlexID, tc.description)
	}
}

func TestGetVmssFlexVM(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	testCases := []struct {
		description                    string
		nodeName                       string
		testVM                         compute.VirtualMachine
		vmGetErr                       *retry.Error
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
		vmListErr                      error
		expectedVmssFlexVM             compute.VirtualMachine
		expectedErr                    error
	}{
		{
			description:                    "getVmssFlexVM should return the VmssFlex VM",
			nodeName:                       "testvm1",
			testVM:                         testVMWithoutInstanceView1,
			vmGetErr:                       nil,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			expectedVmssFlexVM:             testVM1,
			expectedErr:                    nil,
		},
		{
			description:                    "getVmssFlexVM should throw InstanceNotFound error if the VM cannot be found",
			nodeName:                       "testvm1",
			testVM:                         compute.VirtualMachine{},
			vmGetErr:                       &retry.Error{HTTPStatusCode: http.StatusNotFound},
			testVMListWithoutInstanceView:  []compute.VirtualMachine{},
			testVMListWithOnlyInstanceView: []compute.VirtualMachine{},
			vmListErr:                      nil,
			expectedVmssFlexVM:             compute.VirtualMachine{},
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
		{
			description:                    "getVmssFlexVM should throw InstanceNotFound error if the VM is removed from VMSS Flex",
			nodeName:                       "testvm1",
			testVM:                         testVMWithoutInstanceView1,
			vmGetErr:                       nil,
			testVMListWithoutInstanceView:  []compute.VirtualMachine{testVMWithoutInstanceView2},
			testVMListWithOnlyInstanceView: []compute.VirtualMachine{testVMWithOnlyInstanceView2},
			vmListErr:                      nil,
			expectedVmssFlexVM:             compute.VirtualMachine{},
			expectedErr:                    cloudprovider.InstanceNotFound,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMClient := fs.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), fs.ResourceGroup, tc.nodeName, gomock.Any()).Return(tc.testVM, tc.vmGetErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithoutInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()

		vmssFlexVM, err := fs.getVmssFlexVM(tc.nodeName, azcache.CacheReadTypeDefault)
		assert.Equal(t, tc.expectedErr, err, tc.description)
		assert.Equal(t, tc.expectedVmssFlexVM, vmssFlexVM, tc.description)
	}

}

func TestGetVmssFlexByVmssFlexID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description      string
		vmssFlexID       string
		testVmssFlexList []compute.VirtualMachineScaleSet
		vmssFlexListErr  *retry.Error
		expectedVmssFlex *compute.VirtualMachineScaleSet
		expectedErr      error
	}{
		{
			description:      "getVmssFlexByVmssFlexID should return the corresponding vmssFlex by its ID",
			vmssFlexID:       "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1",
			testVmssFlexList: testVmssFlexList,
			vmssFlexListErr:  nil,
			expectedVmssFlex: &testVmssFlex1,
			expectedErr:      nil,
		},
		{
			description:      "getVmssFlexByVmssFlexID should return cloudprovider.InstanceNotFound if there's no matching VMSS",
			vmssFlexID:       "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1",
			testVmssFlexList: []compute.VirtualMachineScaleSet{testVmssFlex2},
			vmssFlexListErr:  nil,
			expectedVmssFlex: nil,
			expectedErr:      cloudprovider.InstanceNotFound,
		},
		{
			description:      "getVmssFlexByVmssFlexID  should report an error if there's something wrong during an api call",
			vmssFlexID:       "subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssflex1",
			testVmssFlexList: []compute.VirtualMachineScaleSet{},
			vmssFlexListErr:  &retry.Error{RawError: fmt.Errorf("error during vmss list")},
			expectedVmssFlex: nil,
			expectedErr:      fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error during vmss list"),
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(tc.testVmssFlexList, tc.vmssFlexListErr).AnyTimes()

		vmssFlex, err := fs.getVmssFlexByVmssFlexID(tc.vmssFlexID, azcache.CacheReadTypeDefault)
		if tc.expectedErr != nil {
			assert.EqualError(t, tc.expectedErr, err.Error(), tc.description)
		}
		assert.Equal(t, tc.expectedVmssFlex, vmssFlex, tc.description)
	}
}

func TestGetVmssFlexIDByName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description        string
		vmssFlexName       string
		testVmssFlexList   []compute.VirtualMachineScaleSet
		vmssFlexListErr    *retry.Error
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
			testVmssFlexList:   []compute.VirtualMachineScaleSet{testVmssFlex2},
			vmssFlexListErr:    nil,
			expectedVmssFlexID: "",
			expectedErr:        cloudprovider.InstanceNotFound,
		},
		{
			description:        "getVmssFlexIDByName should report an error if there's something wrong during an api call",
			vmssFlexName:       "vmssflex1",
			testVmssFlexList:   []compute.VirtualMachineScaleSet{},
			vmssFlexListErr:    &retry.Error{RawError: fmt.Errorf("error during vmss list")},
			expectedVmssFlexID: "",
			expectedErr:        fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error during vmss list"),
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(tc.testVmssFlexList, tc.vmssFlexListErr).AnyTimes()

		vmssFlexID, err := fs.getVmssFlexIDByName(tc.vmssFlexName)

		assert.Equal(t, tc.expectedVmssFlexID, vmssFlexID, tc.description)
		if tc.expectedErr != nil {
			assert.EqualError(t, tc.expectedErr, err.Error(), tc.description)
		}
	}

}

func TestGetVmssFlexByName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description      string
		vmssFlexName     string
		testVmssFlexList []compute.VirtualMachineScaleSet
		vmssFlexListErr  *retry.Error
		expectedVmssFlex *compute.VirtualMachineScaleSet
		expectedErr      error
	}{
		{
			description:      "getVmssFlexByName should return the corresponding vmssFlex by its ID",
			vmssFlexName:     "vmssflex1",
			testVmssFlexList: testVmssFlexList,
			vmssFlexListErr:  nil,
			expectedVmssFlex: &testVmssFlex1,
			expectedErr:      nil,
		},
		{
			description:      "getVmssFlexByName should return cloudprovider.InstanceNotFound if there's no matching VMSS",
			vmssFlexName:     "vmssflex1",
			testVmssFlexList: []compute.VirtualMachineScaleSet{testVmssFlex2},
			vmssFlexListErr:  nil,
			expectedVmssFlex: nil,
			expectedErr:      cloudprovider.InstanceNotFound,
		},
		{
			description:      "getVmssFlexByName should report an error if there's something wrong during an api call",
			vmssFlexName:     "vmssflex1",
			testVmssFlexList: []compute.VirtualMachineScaleSet{},
			vmssFlexListErr:  &retry.Error{RawError: fmt.Errorf("error during vmss list")},
			expectedVmssFlex: nil,
			expectedErr:      fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error during vmss list"),
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMSSClient := fs.cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(tc.testVmssFlexList, tc.vmssFlexListErr).AnyTimes()

		vmssFlex, err := fs.getVmssFlexByName(tc.vmssFlexName)

		assert.Equal(t, tc.expectedVmssFlex, vmssFlex, tc.description)
		if tc.expectedErr != nil {
			assert.EqualError(t, tc.expectedErr, err.Error(), tc.description)
		}
	}

}

func TestGetVmssFlexByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		nodeName                       string
		testVM                         compute.VirtualMachine
		vmGetErr                       *retry.Error
		testVMListWithoutInstanceView  []compute.VirtualMachine
		testVMListWithOnlyInstanceView []compute.VirtualMachine
		vmListErr                      error
		testVmssFlexList               []compute.VirtualMachineScaleSet
		vmssFlexListErr                *retry.Error
		expectedVmssFlex               *compute.VirtualMachineScaleSet
		expectedErr                    error
	}{
		{
			description:                    "getVmssFlexByName should return the VmssFlex ID that the node belongs to",
			nodeName:                       "testvm1",
			testVM:                         testVMWithoutInstanceView1,
			vmGetErr:                       nil,
			testVMListWithoutInstanceView:  testVMListWithoutInstanceView,
			testVMListWithOnlyInstanceView: testVMListWithOnlyInstanceView,
			vmListErr:                      nil,
			testVmssFlexList:               testVmssFlexList,
			vmssFlexListErr:                nil,
			expectedVmssFlex:               &testVmssFlex1,
			expectedErr:                    nil,
		},
	}

	for _, tc := range testCases {
		fs, err := NewTestFlexScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test FlexScaleSet")

		mockVMClient := fs.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), fs.ResourceGroup, tc.nodeName, gomock.Any()).Return(tc.testVM, tc.vmGetErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithoutInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithoutInstanceView, tc.vmListErr).AnyTimes()
		mockVMClient.EXPECT().ListVmssFlexVMsWithOnlyInstanceView(gomock.Any(), gomock.Any()).Return(tc.testVMListWithOnlyInstanceView, tc.vmListErr).AnyTimes()
		mockVMSSClient := fs.cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(tc.testVmssFlexList, tc.vmssFlexListErr).AnyTimes()

		vmssFlex, err := fs.getVmssFlexByNodeName(tc.nodeName, azcache.CacheReadTypeDefault)
		assert.Equal(t, tc.expectedErr, err, tc.description)
		assert.Equal(t, tc.expectedVmssFlex, vmssFlex, tc.description)
	}

}
