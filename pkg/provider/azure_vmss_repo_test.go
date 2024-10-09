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

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/interfaceclient/mockinterfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/publicipclient/mockpublicipclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestCreateOrUpdateVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		vmss        compute.VirtualMachineScaleSet
		clientErr   *retry.Error
		expectedErr *retry.Error
	}{
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusInternalServerError},
			expectedErr: &retry.Error{HTTPStatusCode: http.StatusInternalServerError},
		},
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusTooManyRequests},
			expectedErr: &retry.Error{HTTPStatusCode: http.StatusTooManyRequests},
		},
		{
			clientErr:   &retry.Error{RawError: fmt.Errorf("azure cloud provider rate limited(write) for operation CreateOrUpdate")},
			expectedErr: &retry.Error{RawError: fmt.Errorf("azure cloud provider rate limited(write) for operation CreateOrUpdate")},
		},
		{
			vmss: compute.VirtualMachineScaleSet{
				VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
					ProvisioningState: ptr.To(consts.ProvisionStateDeleting),
				},
			},
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)

		mockVMSSClient := az.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, testVMSSName).Return(test.vmss, test.clientErr)

		err := az.CreateOrUpdateVMSS(az.ResourceGroup, testVMSSName, compute.VirtualMachineScaleSet{})
		assert.Equal(t, test.expectedErr, err)
	}
}

func TestGetVirtualMachineWithRetry(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		vmClientErr *retry.Error
		expectedErr error
	}{
		{
			vmClientErr: &retry.Error{HTTPStatusCode: http.StatusNotFound},
			expectedErr: cloudprovider.InstanceNotFound,
		},
		{
			vmClientErr: &retry.Error{HTTPStatusCode: http.StatusInternalServerError},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)),
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		mockVMClient := az.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "vm", gomock.Any()).Return(compute.VirtualMachine{}, test.vmClientErr)

		vm, err := az.GetVirtualMachineWithRetry(context.TODO(), "vm", cache.CacheReadTypeDefault)
		assert.Empty(t, vm)
		if err != nil {
			assert.EqualError(t, test.expectedErr, err.Error())
		}
	}
}

func TestGetPrivateIPsForMachine(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		vmClientErr        *retry.Error
		expectedPrivateIPs []string
		expectedErr        error
	}{
		{
			expectedPrivateIPs: []string{"1.2.3.4"},
		},
		{
			vmClientErr:        &retry.Error{HTTPStatusCode: http.StatusNotFound},
			expectedErr:        cloudprovider.InstanceNotFound,
			expectedPrivateIPs: []string{},
		},
		{
			vmClientErr:        &retry.Error{HTTPStatusCode: http.StatusInternalServerError},
			expectedErr:        wait.ErrWaitTimeout,
			expectedPrivateIPs: []string{},
		},
	}

	expectedVM := compute.VirtualMachine{
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			AvailabilitySet: &compute.SubResource{ID: ptr.To("availability-set")},
			NetworkProfile: &compute.NetworkProfile{
				NetworkInterfaces: &[]compute.NetworkInterfaceReference{
					{
						NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
							Primary: ptr.To(true),
						},
						ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic"),
					},
				},
			},
		},
	}

	expectedInterface := network.Interface{
		InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
			IPConfigurations: &[]network.InterfaceIPConfiguration{
				{
					InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
						PrivateIPAddress: ptr.To("1.2.3.4"),
					},
				},
			},
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		mockVMClient := az.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "vm", gomock.Any()).Return(expectedVM, test.vmClientErr)

		mockInterfaceClient := az.InterfacesClient.(*mockinterfaceclient.MockInterface)
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
		clientErr         *retry.Error
		expectedPrivateIP string
		expectedPublicIP  string
		expectedErr       error
	}{
		{
			expectedPrivateIP: "1.2.3.4",
			expectedPublicIP:  "5.6.7.8",
		},
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusNotFound},
			expectedErr: wait.ErrWaitTimeout,
		},
	}

	expectedVM := compute.VirtualMachine{
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			AvailabilitySet: &compute.SubResource{ID: ptr.To("availability-set")},
			NetworkProfile: &compute.NetworkProfile{
				NetworkInterfaces: &[]compute.NetworkInterfaceReference{
					{
						NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
							Primary: ptr.To(true),
						},
						ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic"),
					},
				},
			},
		},
	}

	expectedInterface := network.Interface{
		InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
			IPConfigurations: &[]network.InterfaceIPConfiguration{
				{
					InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
						PrivateIPAddress: ptr.To("1.2.3.4"),
						PublicIPAddress: &network.PublicIPAddress{
							ID: ptr.To("test/pip"),
						},
					},
				},
			},
		},
	}

	expectedPIP := network.PublicIPAddress{
		Name: ptr.To("pip"),
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			IPAddress: ptr.To("5.6.7.8"),
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		mockVMClient := az.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "vm", gomock.Any()).Return(expectedVM, test.clientErr)

		mockInterfaceClient := az.InterfacesClient.(*mockinterfaceclient.MockInterface)
		mockInterfaceClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "nic", gomock.Any()).Return(expectedInterface, nil).MaxTimes(1)

		mockPIPClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
		mockPIPClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]network.PublicIPAddress{expectedPIP}, nil).MaxTimes(1)

		privateIP, publicIP, err := az.GetIPForMachineWithRetry(context.Background(), "vm")
		assert.Equal(t, test.expectedErr, err)
		assert.Equal(t, test.expectedPrivateIP, privateIP)
		assert.Equal(t, test.expectedPublicIP, publicIP)
	}
}
