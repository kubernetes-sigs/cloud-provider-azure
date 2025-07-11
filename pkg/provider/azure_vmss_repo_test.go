/*
Copyright 2023 The Kubernetes Authors.

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
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/interfaceclient/mock_interfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient/mock_virtualmachinescalesetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func TestCreateOrUpdateVMSS(t *testing.T) {

	tests := []struct {
		vmss        *armcompute.VirtualMachineScaleSet
		clientErr   error
		expectedErr error
	}{
		{
			clientErr:   &azcore.ResponseError{StatusCode: http.StatusInternalServerError},
			expectedErr: &azcore.ResponseError{StatusCode: http.StatusInternalServerError},
		},
		{
			clientErr:   &azcore.ResponseError{StatusCode: http.StatusTooManyRequests},
			expectedErr: &azcore.ResponseError{StatusCode: http.StatusTooManyRequests},
		},
		{
			clientErr:   &azcore.ResponseError{ErrorCode: "azure cloud provider rate limited(write) for operation CreateOrUpdate"},
			expectedErr: &azcore.ResponseError{ErrorCode: "azure cloud provider rate limited(write) for operation CreateOrUpdate"},
		},
		{
			vmss: &armcompute.VirtualMachineScaleSet{
				Properties: &armcompute.VirtualMachineScaleSetProperties{
					ProvisioningState: ptr.To(consts.ProvisionStateDeleting),
				},
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(fmt.Sprintf("TestCreateOrUpdateVMSS-%v", test.expectedErr), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			az := GetTestCloud(ctrl)

			mockVMSSClient := az.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
			mockVMSSClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, testVMSSName, nil).Return(test.vmss, test.clientErr)

			err := az.CreateOrUpdateVMSS(az.ResourceGroup, testVMSSName, armcompute.VirtualMachineScaleSet{})
			if test.expectedErr != nil {
				assert.Contains(t, err.Error(), test.expectedErr.Error())
			} else {
				assert.Nil(t, err)
			}
		})
	}
}

func TestGetVirtualMachineWithRetry(t *testing.T) {

	tests := []struct {
		vmClientErr error
		expectedErr error
	}{
		{
			vmClientErr: &azcore.ResponseError{StatusCode: http.StatusNotFound},
			expectedErr: cloudprovider.InstanceNotFound,
		},
		{
			vmClientErr: &azcore.ResponseError{StatusCode: http.StatusInternalServerError},
			expectedErr: fmt.Errorf("UNAVAILABLE"),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(fmt.Sprintf("TestGetVirtualMachineWithRetry-%v", test.expectedErr), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			az := GetTestCloud(ctrl)
			mockVMClient := az.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
			mockVMClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "vm", gomock.Any()).Return(&armcompute.VirtualMachine{}, test.vmClientErr)

			vm, err := az.GetVirtualMachineWithRetry(context.TODO(), "vm", cache.CacheReadTypeDefault)
			assert.Empty(t, vm)
			if err != nil {
				assert.Contains(t, err.Error(), test.expectedErr.Error())
			}
		})
	}
}

func TestGetPrivateIPsForMachine(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		vmClientErr        error
		expectedPrivateIPs []string
		expectedErr        error
	}{
		{
			expectedPrivateIPs: []string{"1.2.3.4"},
		},
		{
			vmClientErr:        &azcore.ResponseError{StatusCode: http.StatusNotFound},
			expectedErr:        cloudprovider.InstanceNotFound,
			expectedPrivateIPs: []string{},
		},
		{
			vmClientErr:        &azcore.ResponseError{StatusCode: http.StatusInternalServerError},
			expectedErr:        wait.ErrWaitTimeout,
			expectedPrivateIPs: []string{},
		},
	}

	expectedVM := &armcompute.VirtualMachine{
		Properties: &armcompute.VirtualMachineProperties{
			AvailabilitySet: &armcompute.SubResource{ID: ptr.To("availability-set")},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
					{
						Properties: &armcompute.NetworkInterfaceReferenceProperties{
							Primary: ptr.To(true),
						},
						ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic"),
					},
				},
			},
		},
	}

	expectedInterface := &armnetwork.Interface{
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
				{
					Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
						PrivateIPAddress: ptr.To("1.2.3.4"),
					},
				},
			},
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		mockVMClient := az.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "vm", gomock.Any()).Return(expectedVM, test.vmClientErr)

		mockInterfaceClient := az.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
		mockInterfaceClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "nic", gomock.Any()).Return(expectedInterface, nil).MaxTimes(1)

		privateIPs, err := az.getPrivateIPsForMachine(context.Background(), "vm")
		assert.Equal(t, test.expectedErr, err)
		assert.Equal(t, test.expectedPrivateIPs, privateIPs)
	}
}

func TestGetIPForMachineWithRetry(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		clientErr         error
		expectedPrivateIP string
		expectedPublicIP  string
		expectedErr       error
	}{
		{
			expectedPrivateIP: "1.2.3.4",
			expectedPublicIP:  "5.6.7.8",
		},
		{
			clientErr:   &azcore.ResponseError{StatusCode: http.StatusNotFound},
			expectedErr: wait.ErrWaitTimeout,
		},
	}

	expectedVM := &armcompute.VirtualMachine{
		Properties: &armcompute.VirtualMachineProperties{
			AvailabilitySet: &armcompute.SubResource{ID: ptr.To("availability-set")},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
					{
						Properties: &armcompute.NetworkInterfaceReferenceProperties{
							Primary: ptr.To(true),
						},
						ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic"),
					},
				},
			},
		},
	}

	expectedInterface := &armnetwork.Interface{
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
				{
					Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
						PrivateIPAddress: ptr.To("1.2.3.4"),
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: ptr.To("test/pip"),
						},
					},
				},
			},
		},
	}

	expectedPIP := &armnetwork.PublicIPAddress{
		Name: ptr.To("pip"),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			IPAddress: ptr.To("5.6.7.8"),
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		mockVMClient := az.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "vm", gomock.Any()).Return(expectedVM, test.clientErr)

		mockInterfaceClient := az.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
		mockInterfaceClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "nic", gomock.Any()).Return(expectedInterface, nil).MaxTimes(1)

		mockPIPClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
		mockPIPClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]*armnetwork.PublicIPAddress{expectedPIP}, nil).MaxTimes(1)

		privateIP, publicIP, err := az.GetIPForMachineWithRetry(context.Background(), "vm")
		assert.Equal(t, test.expectedErr, err)
		assert.Equal(t, test.expectedPrivateIP, privateIP)
		assert.Equal(t, test.expectedPublicIP, publicIP)
	}
}
