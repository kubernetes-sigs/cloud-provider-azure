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
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/loadbalancerclient/mockloadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/publicipclient/mockpublicipclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestDeleteLB(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	mockLBClient := az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
	mockLBClient.EXPECT().Delete(gomock.Any(), az.ResourceGroup, "lb").Return(&retry.Error{HTTPStatusCode: http.StatusInternalServerError})

	err := az.DeleteLB(&v1.Service{}, "lb")
	assert.EqualError(t, fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: %w", error(nil)), fmt.Sprintf("%s", err.Error()))
}

func TestListManagedLBs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		existingLBs     []network.LoadBalancer
		expectedLBs     *[]network.LoadBalancer
		callTimes       int
		multiSLBConfigs []MultipleStandardLoadBalancerConfiguration
		clientErr       *retry.Error
		expectedErr     error
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
				{Name: pointer.String("kubernetes")},
				{Name: pointer.String("kubernetes-internal")},
				{Name: pointer.String("vmas-1")},
				{Name: pointer.String("vmas-1-internal")},
				{Name: pointer.String("unmanaged")},
				{Name: pointer.String("unmanaged-internal")},
			},
			expectedLBs: &[]network.LoadBalancer{
				{Name: pointer.String("kubernetes")},
				{Name: pointer.String("kubernetes-internal")},
				{Name: pointer.String("vmas-1")},
				{Name: pointer.String("vmas-1-internal")},
			},
			callTimes: 1,
		},
		{
			existingLBs: []network.LoadBalancer{
				{Name: pointer.String("kubernetes")},
				{Name: pointer.String("kubernetes-internal")},
				{Name: pointer.String("lb1-internal")},
				{Name: pointer.String("lb2")},
			},
			multiSLBConfigs: []MultipleStandardLoadBalancerConfiguration{
				{Name: "kubernetes"},
				{Name: "lb1"},
			},
			expectedLBs: &[]network.LoadBalancer{
				{Name: pointer.String("kubernetes")},
				{Name: pointer.String("kubernetes-internal")},
				{Name: pointer.String("lb1-internal")},
			},
		},
	}
	for _, test := range tests {
		az := GetTestCloud(ctrl)
		if len(test.multiSLBConfigs) > 0 {
			az.LoadBalancerSku = consts.LoadBalancerSkuStandard
			az.MultipleStandardLoadBalancerConfigurations = test.multiSLBConfigs
		} else {
			az.LoadBalancerSku = consts.LoadBalancerSkuBasic
		}

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
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: %w", errors.New("canceledandsupersededduetoanotheroperation")),
		},
		{
			clientErr:   &retry.Error{RawError: errors.New(referencedResourceNotProvisionedRawErrorString)},
			expectedErr: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: %w", errors.New(referencedResourceNotProvisionedRawErrorString)),
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		az.lbCache.Set("lb", "test")

		mockLBClient := az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
		mockLBClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(test.clientErr)
		mockLBClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "lb", gomock.Any()).Return(network.LoadBalancer{}, nil)

		mockPIPClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
		mockPIPClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, "pip", gomock.Any()).Return(nil).MaxTimes(1)
		mockPIPClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]network.PublicIPAddress{{
			Name: pointer.String("pip"),
			PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
				ProvisioningState: network.ProvisioningStateSucceeded,
			},
		}}, nil).MaxTimes(2)

		err := az.CreateOrUpdateLB(&v1.Service{}, network.LoadBalancer{
			Name: pointer.String("lb"),
			Etag: pointer.String("etag"),
		})
		assert.EqualError(t, test.expectedErr, err.Error())

		// loadbalancer should be removed from cache if the etag is mismatch or the operation is canceled
		shouldBeEmpty, err := az.lbCache.GetWithDeepCopy("lb", cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Empty(t, shouldBeEmpty)

		// public ip cache should be populated since there's GetPIP
		shouldNotBeEmpty, err := az.pipCache.Get(az.ResourceGroup, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.NotEmpty(t, shouldNotBeEmpty)
	}
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

func TestMigrateToIPBasedBackendPoolAndWaitForCompletion(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		desc                  string
		migrationError        *retry.Error
		backendPool           network.BackendAddressPool
		backendPoolAfterRetry *network.BackendAddressPool
		getBackendPoolError   *retry.Error
		expectedError         error
	}{
		{
			desc:           "MigrateToIPBasedBackendPoolAndWaitForCompletion should return the error if the migration fails",
			migrationError: retry.NewError(false, errors.New("error")),
			expectedError:  retry.NewError(false, errors.New("error")).Error(),
		},
		{
			desc:                "MigrateToIPBasedBackendPoolAndWaitForCompletion should return the error if failed to get the backend pool",
			getBackendPoolError: retry.NewError(false, errors.New("error")),
			expectedError:       retry.NewError(false, errors.New("error")).Error(),
		},
		{
			desc: "MigrateToIPBasedBackendPoolAndWaitForCompletion should retry if the number IPs on the backend pool is not expected",
			backendPool: network.BackendAddressPool{
				Name: pointer.String(testClusterName),
				BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
					LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{
						{
							LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: pointer.String("1.2.3.4"),
							},
						},
					},
				},
			},
			backendPoolAfterRetry: &network.BackendAddressPool{
				Name: pointer.String(testClusterName),
				BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
					LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{
						{
							LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: pointer.String("1.2.3.4"),
							},
						},
						{
							LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: pointer.String("2.3.4.5"),
							},
						},
					},
				},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			lbClient := mockloadbalancerclient.NewMockInterface(ctrl)
			lbClient.EXPECT().MigrateToIPBasedBackendPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.migrationError)
			if tc.migrationError == nil {
				lbClient.EXPECT().GetLBBackendPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.backendPool, tc.getBackendPoolError)
			}
			if tc.backendPoolAfterRetry != nil {
				lbClient.EXPECT().GetLBBackendPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(*tc.backendPoolAfterRetry, nil)
			}
			az.LoadBalancerClient = lbClient

			lbName := testClusterName
			backendPoolNames := []string{testClusterName}
			nicsCountMap := map[string]int{testClusterName: 2}
			err := az.MigrateToIPBasedBackendPoolAndWaitForCompletion(lbName, backendPoolNames, nicsCountMap)
			if tc.expectedError != nil {
				assert.EqualError(t, err, tc.expectedError.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestIsBackendPoolOnSameLB(t *testing.T) {
	tests := []struct {
		backendPoolID        string
		expectedLBName       string
		existingBackendPools []string
		expected             bool
		expectError          bool
	}{
		{
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1/backendAddressPools/pool1",
			existingBackendPools: []string{
				"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1/backendAddressPools/pool2",
			},
			expected:       true,
			expectedLBName: "",
		},
		{
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1-internal/backendAddressPools/pool1",
			existingBackendPools: []string{
				"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1/backendAddressPools/pool2",
			},
			expected:       true,
			expectedLBName: "",
		},
		{
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1/backendAddressPools/pool1",
			existingBackendPools: []string{
				"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1-internal/backendAddressPools/pool2",
			},
			expected:       true,
			expectedLBName: "",
		},
		{
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1/backendAddressPools/pool1",
			existingBackendPools: []string{
				"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb2/backendAddressPools/pool2",
			},
			expected:       false,
			expectedLBName: "lb2",
		},
		{
			backendPoolID: "wrong-backendpool-id",
			existingBackendPools: []string{
				"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1/backendAddressPools/pool2",
			},
			expectError: true,
		},
		{
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1/backendAddressPools/pool1",
			existingBackendPools: []string{
				"wrong-existing-backendpool-id",
			},
			expectError: true,
		},
		{
			backendPoolID: "wrong-backendpool-id",
			existingBackendPools: []string{
				"wrong-existing-backendpool-id",
			},
			expectError: true,
		},
		{
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/malformed-lb1-internal/backendAddressPools/pool1",
			existingBackendPools: []string{
				"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/malformed-lb1-lanretni/backendAddressPools/pool2",
			},
			expected:       false,
			expectedLBName: "malformed-lb1-lanretni",
		},
	}

	for _, test := range tests {
		isSameLB, lbName, err := isBackendPoolOnSameLB(test.backendPoolID, test.existingBackendPools)
		if test.expectError {
			assert.Error(t, err)
			continue
		}

		assert.Equal(t, test.expected, isSameLB)
		assert.Equal(t, test.expectedLBName, lbName)
	}
}

func TestServiceOwnsRuleSharedProbe(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		desc string
	}{
		{
			desc: "should count in the shared probe",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			svc := getTestService("test", v1.ProtocolTCP, nil, false)
			assert.True(t, az.serviceOwnsRule(&svc, consts.SharedProbeName))
		})
	}
}
