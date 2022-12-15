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
	"errors"
	"fmt"
	"net/http"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudprovider "k8s.io/cloud-provider"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-03-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/interfaceclient/mockinterfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/loadbalancerclient/mockloadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/publicipclient/mockpublicipclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/routeclient/mockrouteclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/routetableclient/mockroutetableclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/securitygroupclient/mocksecuritygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

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

		vm, err := az.GetVirtualMachineWithRetry("vm", cache.CacheReadTypeDefault)
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
			AvailabilitySet: &compute.SubResource{ID: to.StringPtr("availability-set")},
			NetworkProfile: &compute.NetworkProfile{
				NetworkInterfaces: &[]compute.NetworkInterfaceReference{
					{
						NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
							Primary: to.BoolPtr(true),
						},
						ID: to.StringPtr("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic"),
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
						PrivateIPAddress: to.StringPtr("1.2.3.4"),
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

		privateIPs, err := az.getPrivateIPsForMachine("vm")
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
			AvailabilitySet: &compute.SubResource{ID: to.StringPtr("availability-set")},
			NetworkProfile: &compute.NetworkProfile{
				NetworkInterfaces: &[]compute.NetworkInterfaceReference{
					{
						NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
							Primary: to.BoolPtr(true),
						},
						ID: to.StringPtr("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic"),
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
						PrivateIPAddress: to.StringPtr("1.2.3.4"),
						PublicIPAddress: &network.PublicIPAddress{
							ID: to.StringPtr("test/pip"),
						},
					},
				},
			},
		},
	}

	expectedPIP := network.PublicIPAddress{
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			IPAddress: to.StringPtr("5.6.7.8"),
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		mockVMClient := az.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "vm", gomock.Any()).Return(expectedVM, test.clientErr)

		mockInterfaceClient := az.InterfacesClient.(*mockinterfaceclient.MockInterface)
		mockInterfaceClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "nic", gomock.Any()).Return(expectedInterface, nil).MaxTimes(1)

		mockPIPClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
		mockPIPClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "pip", gomock.Any()).Return(expectedPIP, nil).MaxTimes(1)

		privateIP, publicIP, err := az.GetIPForMachineWithRetry("vm")
		assert.Equal(t, test.expectedErr, err)
		assert.Equal(t, test.expectedPrivateIP, privateIP)
		assert.Equal(t, test.expectedPublicIP, publicIP)
	}
}

func TestCreateOrUpdateSecurityGroupCanceled(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.nsgCache.Set("sg", "test")

	mockSGClient := az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
	mockSGClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(&retry.Error{
		RawError: fmt.Errorf(consts.OperationCanceledErrorMessage),
	})
	mockSGClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "sg", gomock.Any()).Return(network.SecurityGroup{}, nil)

	err := az.CreateOrUpdateSecurityGroup(network.SecurityGroup{Name: to.StringPtr("sg")})
	assert.EqualError(t, fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: %w", fmt.Errorf("canceledandsupersededduetoanotheroperation")), err.Error())

	// security group should be removed from cache if the operation is canceled
	shouldBeEmpty, err := az.nsgCache.GetWithDeepCopy("sg", cache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Empty(t, shouldBeEmpty)
}

func TestCreateOrUpdateLB(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	referencedResourceNotProvisionedRawErrorString := `Code="ReferencedResourceNotProvisioned" Message="Cannot proceed with operation because resource /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip used by resource /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb is not in Succeeded state. Resource is in Failed state and the last operation that updated/is updating the resource is PutPublicIpAddressOperation."`

	tests := []struct {
		clientErr   *retry.Error
		expectedErr error
	}{
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusPreconditionFailed},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 412, RawError: %w", error(nil)),
		},
		{
			clientErr:   &retry.Error{RawError: fmt.Errorf(consts.OperationCanceledErrorMessage)},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: %w", fmt.Errorf("canceledandsupersededduetoanotheroperation")),
		},
		{
			clientErr:   &retry.Error{RawError: fmt.Errorf(referencedResourceNotProvisionedRawErrorString)},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: %w", fmt.Errorf(referencedResourceNotProvisionedRawErrorString)),
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		az.lbCache.Set("lb", "test")

		mockLBClient := az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
		mockLBClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(test.clientErr)
		mockLBClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "lb", gomock.Any()).Return(network.LoadBalancer{}, nil)

		mockPIPClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
		mockPIPClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, "pip", gomock.Any()).Return(nil).AnyTimes()
		mockPIPClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "pip", gomock.Any()).Return(network.PublicIPAddress{
			Name: to.StringPtr("pip"),
			PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
				ProvisioningState: network.ProvisioningStateSucceeded,
			},
		}, nil).AnyTimes()

		err := az.CreateOrUpdateLB(&v1.Service{}, network.LoadBalancer{
			Name: to.StringPtr("lb"),
			Etag: to.StringPtr("etag"),
		})
		assert.EqualError(t, test.expectedErr, err.Error())

		// loadbalancer should be removed from cache if the etag is mismatch or the operation is canceled
		shouldBeEmpty, err := az.lbCache.GetWithDeepCopy("lb", cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Empty(t, shouldBeEmpty)

		// public ip cache should be populated since there's GetPIP
		shouldNotBeEmpty, err := az.pipCache.GetWithDeepCopy(az.getPIPCacheKey(az.ResourceGroup, "pip"), cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.NotEmpty(t, shouldNotBeEmpty)
	}
}

func TestListAgentPoolLBs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		existingLBs, expectedLBs []network.LoadBalancer
		callTimes                int
		clientErr                *retry.Error
		expectedErr              error
	}{
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusInternalServerError},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)),
		},
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusNotFound},
			expectedErr: nil,
		},
		{
			existingLBs: []network.LoadBalancer{
				{Name: to.StringPtr("kubernetes")},
				{Name: to.StringPtr("kubernetes-internal")},
				{Name: to.StringPtr("vmas-1")},
				{Name: to.StringPtr("vmas-1-internal")},
				{Name: to.StringPtr("unmanaged")},
				{Name: to.StringPtr("unmanaged-internal")},
			},
			expectedLBs: []network.LoadBalancer{
				{Name: to.StringPtr("kubernetes")},
				{Name: to.StringPtr("kubernetes-internal")},
				{Name: to.StringPtr("vmas-1")},
				{Name: to.StringPtr("vmas-1-internal")},
			},
			callTimes: 1,
		},
	}
	for _, test := range tests {
		az := GetTestCloud(ctrl)

		mockLBClient := az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
		mockLBClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(test.existingLBs, test.clientErr)
		mockVMSet := NewMockVMSet(ctrl)
		mockVMSet.EXPECT().GetAgentPoolVMSetNames(gomock.Any()).Return(&[]string{"vmas-0", "vmas-1"}, nil).Times(test.callTimes)
		mockVMSet.EXPECT().GetPrimaryVMSetName().Return("vmas-0").AnyTimes()
		az.VMSet = mockVMSet

		lbs, err := az.ListManagedLBs(&v1.Service{}, []*v1.Node{}, "kubernetes")
		assert.Equal(t, test.expectedErr, err)
		assert.Equal(t, test.expectedLBs, lbs)
	}
}

func TestListPIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		clientErr   *retry.Error
		expectedErr error
	}{
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusInternalServerError},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)),
		},
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusNotFound},
			expectedErr: nil,
		},
	}
	for _, test := range tests {
		az := GetTestCloud(ctrl)
		mockPIPClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
		mockPIPClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(nil, test.clientErr)

		pips, err := az.ListPIP(&v1.Service{}, az.ResourceGroup)
		assert.Equal(t, test.expectedErr, err)
		assert.Empty(t, pips)
	}
}

func TestCreateOrUpdatePIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		clientErr          *retry.Error
		expectedErr        error
		cacheExpectedEmpty bool
	}{
		{
			clientErr:          &retry.Error{HTTPStatusCode: http.StatusPreconditionFailed},
			expectedErr:        fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 412, RawError: %w", error(nil)),
			cacheExpectedEmpty: true,
		},
		{
			clientErr:          &retry.Error{RawError: fmt.Errorf(consts.OperationCanceledErrorMessage)},
			expectedErr:        fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: %w", fmt.Errorf("canceledandsupersededduetoanotheroperation")),
			cacheExpectedEmpty: true,
		},
		{
			clientErr:          &retry.Error{HTTPStatusCode: http.StatusInternalServerError},
			expectedErr:        fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)),
			cacheExpectedEmpty: false,
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		cacheKey := az.getPIPCacheKey(az.ResourceGroup, "nic")
		az.pipCache.Set(cacheKey, "test")
		mockPIPClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
		mockPIPClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, "nic", gomock.Any()).Return(test.clientErr)
		if test.cacheExpectedEmpty {
			mockPIPClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "nic", gomock.Any()).Return(network.PublicIPAddress{}, nil)
		}

		err := az.CreateOrUpdatePIP(&v1.Service{}, az.ResourceGroup, network.PublicIPAddress{Name: to.StringPtr("nic")})
		assert.EqualError(t, test.expectedErr, err.Error())

		cachedPIP, err := az.pipCache.GetWithDeepCopy(az.getPIPCacheKey(az.ResourceGroup, "nic"), cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		if test.cacheExpectedEmpty {
			assert.Empty(t, cachedPIP)
		} else {
			assert.NotEmpty(t, cachedPIP)
		}
	}
}

func TestCreateOrUpdateInterface(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	mockInterfaceClient := az.InterfacesClient.(*mockinterfaceclient.MockInterface)
	mockInterfaceClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, "nic", gomock.Any()).Return(&retry.Error{HTTPStatusCode: http.StatusInternalServerError})

	err := az.CreateOrUpdateInterface(&v1.Service{}, network.Interface{Name: to.StringPtr("nic")})
	assert.EqualError(t, fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)), err.Error())
}

func TestDeletePublicIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	mockPIPClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
	mockPIPClient.EXPECT().Delete(gomock.Any(), az.ResourceGroup, "pip").Return(&retry.Error{HTTPStatusCode: http.StatusInternalServerError})

	err := az.DeletePublicIP(&v1.Service{}, az.ResourceGroup, "pip")
	assert.EqualError(t, fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)), err.Error())
}

func TestDeleteLB(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	mockLBClient := az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
	mockLBClient.EXPECT().Delete(gomock.Any(), az.ResourceGroup, "lb").Return(&retry.Error{HTTPStatusCode: http.StatusInternalServerError})

	err := az.DeleteLB(&v1.Service{}, "lb")
	assert.EqualError(t, fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)), fmt.Sprintf("%s", err.Error()))
}

func TestCreateOrUpdateRouteTable(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		clientErr   *retry.Error
		expectedErr error
	}{
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusPreconditionFailed},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 412, RawError: %w", error(nil)),
		},
		{
			clientErr:   &retry.Error{RawError: fmt.Errorf(consts.OperationCanceledErrorMessage)},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: %w", fmt.Errorf("canceledandsupersededduetoanotheroperation")),
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		az.rtCache.Set("rt", "test")

		mockRTClient := az.RouteTablesClient.(*mockroutetableclient.MockInterface)
		mockRTClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(test.clientErr)
		mockRTClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "rt", gomock.Any()).Return(network.RouteTable{}, nil)

		err := az.CreateOrUpdateRouteTable(network.RouteTable{
			Name: to.StringPtr("rt"),
			Etag: to.StringPtr("etag"),
		})
		assert.EqualError(t, test.expectedErr, err.Error())

		// route table should be removed from cache if the etag is mismatch or the operation is canceled
		shouldBeEmpty, err := az.rtCache.GetWithDeepCopy("rt", cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Empty(t, shouldBeEmpty)
	}
}

func TestCreateOrUpdateRoute(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		clientErr   *retry.Error
		expectedErr error
	}{
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusPreconditionFailed},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 412, RawError: %w", error(nil)),
		},
		{
			clientErr:   &retry.Error{RawError: fmt.Errorf(consts.OperationCanceledErrorMessage)},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: %w", fmt.Errorf("canceledandsupersededduetoanotheroperation")),
		},
		{
			clientErr:   nil,
			expectedErr: nil,
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		az.rtCache.Set("rt", "test")

		mockRTClient := az.RoutesClient.(*mockrouteclient.MockInterface)
		mockRTClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, "rt", gomock.Any(), gomock.Any(), gomock.Any()).Return(test.clientErr)

		mockRTableClient := az.RouteTablesClient.(*mockroutetableclient.MockInterface)
		mockRTableClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "rt", gomock.Any()).Return(network.RouteTable{}, nil)

		err := az.CreateOrUpdateRoute(network.Route{
			Name: to.StringPtr("rt"),
			Etag: to.StringPtr("etag"),
		})
		if test.expectedErr != nil {
			assert.EqualError(t, test.expectedErr, err.Error())
		}

		shouldBeEmpty, err := az.rtCache.GetWithDeepCopy("rt", cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Empty(t, shouldBeEmpty)
	}
}

func TestDeleteRouteWithName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		clientErr   *retry.Error
		expectedErr error
	}{
		{
			clientErr:   &retry.Error{HTTPStatusCode: http.StatusInternalServerError},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)),
		},
		{
			clientErr:   nil,
			expectedErr: nil,
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)

		mockRTClient := az.RoutesClient.(*mockrouteclient.MockInterface)
		mockRTClient.EXPECT().Delete(gomock.Any(), az.ResourceGroup, "rt", "rt").Return(test.clientErr)

		err := az.DeleteRouteWithName("rt")
		if test.expectedErr != nil {
			assert.EqualError(t, test.expectedErr, err.Error())
		}
	}
}

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
					ProvisioningState: to.StringPtr(consts.VirtualMachineScaleSetsDeallocating),
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

func TestRequestBackoff(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.CloudProviderBackoff = true
	az.ResourceRequestBackoff = wait.Backoff{Steps: 3}

	backoff := az.RequestBackoff()
	assert.Equal(t, wait.Backoff{Steps: 3}, backoff)

}

func TestCreateOrUpdateLBBackendPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description       string
		createOrUpdateErr *retry.Error
		expectedErr       bool
	}{
		{
			description: "CreateOrUpdateLBBackendPool should not report an error if the api call succeeds",
		},
		{
			description: "CreateOrUpdateLBBackendPool should report an error if the api call fails",
			createOrUpdateErr: &retry.Error{
				HTTPStatusCode: http.StatusPreconditionFailed,
				RawError:       errors.New(consts.OperationCanceledErrorMessage),
			},
			expectedErr: true,
		},
	} {
		az := GetTestCloud(ctrl)
		lbClient := mockloadbalancerclient.NewMockInterface(ctrl)
		lbClient.EXPECT().CreateOrUpdateBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.createOrUpdateErr)
		az.LoadBalancerClient = lbClient

		err := az.CreateOrUpdateLBBackendPool("kubernetes", network.BackendAddressPool{})
		assert.Equal(t, tc.expectedErr, err != nil)
	}
}

func TestDeleteLBBackendPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description string
		deleteErr   *retry.Error
		expectedErr bool
	}{
		{
			description: "DeleteLBBackendPool should not report an error if the api call succeeds",
		},
		{
			description: "DeleteLBBackendPool should report an error if the api call fails",
			deleteErr: &retry.Error{
				HTTPStatusCode: http.StatusPreconditionFailed,
				RawError:       errors.New(consts.OperationCanceledErrorMessage),
			},
			expectedErr: true,
		},
	} {
		az := GetTestCloud(ctrl)
		lbClient := mockloadbalancerclient.NewMockInterface(ctrl)
		lbClient.EXPECT().DeleteLBBackendPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.deleteErr)
		az.LoadBalancerClient = lbClient

		err := az.DeleteLBBackendPool("kubernetes", "kubernetes")
		assert.Equal(t, tc.expectedErr, err != nil)
	}
}
