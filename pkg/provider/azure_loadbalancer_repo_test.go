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
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/backendaddresspoolclient/mock_backendaddresspoolclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
)

func TestDeleteLB(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	mockLBClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
	mockLBClient.EXPECT().Delete(gomock.Any(), az.ResourceGroup, "lb").Return(&azcore.ResponseError{StatusCode: http.StatusInternalServerError})

	err := az.DeleteLB(context.TODO(), &v1.Service{}, "lb")
	assert.Contains(t, err.Error(), "UNAVAILABLE")
}

func TestListManagedLBs(t *testing.T) {

	tests := []struct {
		name            string
		existingLBs     []*armnetwork.LoadBalancer
		expectedLBs     []*armnetwork.LoadBalancer
		callTimes       int
		multiSLBConfigs []config.MultipleStandardLoadBalancerConfiguration
		clientErr       error
		expectedErr     error
	}{
		{
			name:        "Internal Server Error",
			clientErr:   &azcore.ResponseError{StatusCode: http.StatusInternalServerError},
			expectedErr: fmt.Errorf("UNAVAILABLE"),
		},
		{
			name:        "Resource Not Found",
			clientErr:   &azcore.ResponseError{StatusCode: http.StatusNotFound},
			expectedErr: nil,
		},
		{
			name: "filtered the result",
			existingLBs: []*armnetwork.LoadBalancer{
				{Name: ptr.To("kubernetes")},
				{Name: ptr.To("kubernetes-internal")},
				{Name: ptr.To("vmas-1")},
				{Name: ptr.To("vmas-1-internal")},
				{Name: ptr.To("unmanaged")},
				{Name: ptr.To("unmanaged-internal")},
			},
			expectedLBs: []*armnetwork.LoadBalancer{
				{Name: ptr.To("kubernetes")},
				{Name: ptr.To("kubernetes-internal")},
				{Name: ptr.To("vmas-1")},
				{Name: ptr.To("vmas-1-internal")},
			},
			callTimes: 1,
		},
		{
			name: "filtered the result with multiple standard load balancer configurations",
			existingLBs: []*armnetwork.LoadBalancer{
				{Name: ptr.To("kubernetes")},
				{Name: ptr.To("kubernetes-internal")},
				{Name: ptr.To("lb1-internal")},
				{Name: ptr.To("lb2")},
			},
			multiSLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{Name: "kubernetes"},
				{Name: "lb1"},
			},
			expectedLBs: []*armnetwork.LoadBalancer{
				{Name: ptr.To("kubernetes")},
				{Name: ptr.To("kubernetes-internal")},
				{Name: ptr.To("lb1-internal")},
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			az := GetTestCloud(ctrl)
			if len(test.multiSLBConfigs) > 0 {
				az.LoadBalancerSKU = consts.LoadBalancerSKUStandard
				az.MultipleStandardLoadBalancerConfigurations = test.multiSLBConfigs
			} else {
				az.LoadBalancerSKU = consts.LoadBalancerSKUBasic
			}

			mockLBClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
			mockLBClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(test.existingLBs, test.clientErr)
			mockVMSet := NewMockVMSet(ctrl)
			mockVMSet.EXPECT().GetAgentPoolVMSetNames(gomock.Any(), gomock.Any()).Return(to.SliceOfPtrs("vmas-0", "vmas-1"), nil).Times(test.callTimes)
			mockVMSet.EXPECT().GetPrimaryVMSetName().Return("vmas-0").AnyTimes()
			az.VMSet = mockVMSet

			lbs, err := az.ListManagedLBs(context.TODO(), &v1.Service{}, []*v1.Node{}, "kubernetes")
			if test.expectedErr != nil {
				assert.NotNil(t, err)
				assert.Contains(t, err.Error(), test.expectedErr.Error())
			} else {
				assert.Nil(t, err)
			}
			assert.Equal(t, test.expectedLBs, lbs)

			ctrl.Finish()
		})
	}
}

func TestCreateOrUpdateLB(t *testing.T) {

	const referencedResourceNotProvisionedRawErrorString = `Code="ReferencedResourceNotProvisioned" Message="Cannot proceed with operation because resource /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip used by resource /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb is not in Succeeded state. Resource is in Failed state and the last operation that updated/is updating the resource is PutPublicIpAddressOperation."`

	tests := []struct {
		name        string
		clientErr   error
		expectedErr error
	}{
		{
			name:        "StatusPreconditionFailed",
			clientErr:   &azcore.ResponseError{StatusCode: http.StatusPreconditionFailed, ErrorCode: "412"},
			expectedErr: fmt.Errorf("412"),
		},
		{
			name:        "OperationCanceled",
			clientErr:   &azcore.ResponseError{ErrorCode: consts.OperationCanceledErrorMessage},
			expectedErr: fmt.Errorf("canceledandsupersededduetoanotheroperation"),
		},
		{
			name:        "ReferencedResourceNotProvisioned",
			clientErr:   &azcore.ResponseError{ErrorCode: referencedResourceNotProvisionedRawErrorString},
			expectedErr: errors.New(referencedResourceNotProvisionedRawErrorString),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			az := GetTestCloud(ctrl)
			az.lbCache.Set("lb", "test")

			mockLBClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
			mockLBClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil, test.clientErr)
			mockLBClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "lb", gomock.Any()).Return(&armnetwork.LoadBalancer{}, nil)

			mockPIPClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			mockPIPClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, "pip", gomock.Any()).Return(nil, nil).MaxTimes(1)
			mockPIPClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]*armnetwork.PublicIPAddress{{
				Name: ptr.To("pip"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					ProvisioningState: to.Ptr(armnetwork.ProvisioningStateSucceeded),
				},
			}}, nil).MaxTimes(2)

			err := az.CreateOrUpdateLB(context.TODO(), &v1.Service{}, armnetwork.LoadBalancer{
				Name: ptr.To("lb"),
				Etag: ptr.To("etag"),
			})
			assert.Contains(t, err.Error(), test.expectedErr.Error())

			// loadbalancer should be removed from cache if the etag is mismatch or the operation is canceled
			shouldBeEmpty, err := az.lbCache.GetWithDeepCopy(context.TODO(), "lb", cache.CacheReadTypeDefault)
			assert.NoError(t, err)
			assert.Empty(t, shouldBeEmpty)

			// public ip cache should be populated since there's GetPIP
			shouldNotBeEmpty, err := az.pipCache.Get(context.TODO(), az.ResourceGroup, cache.CacheReadTypeDefault)
			assert.NoError(t, err)
			assert.NotEmpty(t, shouldNotBeEmpty)
			ctrl.Finish()
		})
	}
}

func TestCreateOrUpdateLBBackendPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description       string
		createOrUpdateErr error
		expectedErr       bool
	}{
		{
			description: "CreateOrUpdateLBBackendPool should not report an error if the api call succeeds",
		},
		{
			description: "CreateOrUpdateLBBackendPool should report an error if the api call fails",
			createOrUpdateErr: &azcore.ResponseError{
				StatusCode: http.StatusPreconditionFailed,
				ErrorCode:  consts.OperationCanceledErrorMessage,
			},
			expectedErr: true,
		},
	} {
		az := GetTestCloud(ctrl)
		lbClient := az.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
		lbClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, tc.createOrUpdateErr)

		err := az.CreateOrUpdateLBBackendPool(context.TODO(), "kubernetes", &armnetwork.BackendAddressPool{})
		assert.Equal(t, tc.expectedErr, err != nil)
	}
}

func TestDeleteLBBackendPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description string
		deleteErr   error
		expectedErr bool
	}{
		{
			description: "DeleteLBBackendPool should not report an error if the api call succeeds",
		},
		{
			description: "DeleteLBBackendPool should report an error if the api call fails",
			deleteErr: &azcore.ResponseError{
				StatusCode: http.StatusPreconditionFailed,
				ErrorCode:  consts.OperationCanceledErrorMessage,
			},
			expectedErr: true,
		},
	} {
		az := GetTestCloud(ctrl)
		backendClient := az.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
		backendClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.deleteErr)

		err := az.DeleteLBBackendPool(context.TODO(), "kubernetes", "kubernetes")
		assert.Equal(t, tc.expectedErr, err != nil)
	}
}

func TestMigrateToIPBasedBackendPoolAndWaitForCompletion(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		desc                  string
		migrationError        error
		backendPool           *armnetwork.BackendAddressPool
		backendPoolAfterRetry *armnetwork.BackendAddressPool
		getBackendPoolError   error
		expectedError         error
	}{
		{
			desc:           "MigrateToIPBasedBackendPoolAndWaitForCompletion should return the error if the migration fails",
			migrationError: &azcore.ResponseError{ErrorCode: "error"},
			expectedError:  &azcore.ResponseError{ErrorCode: "error"},
		},
		{
			desc:                "MigrateToIPBasedBackendPoolAndWaitForCompletion should return the error if failed to get the backend pool",
			getBackendPoolError: &azcore.ResponseError{ErrorCode: "error"},
			expectedError:       &azcore.ResponseError{ErrorCode: "error"},
		},
		{
			desc: "MigrateToIPBasedBackendPoolAndWaitForCompletion should retry if the number IPs on the backend pool is not expected",
			backendPool: &armnetwork.BackendAddressPool{
				Name: ptr.To(testClusterName),
				Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
					LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
						{
							Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: ptr.To("1.2.3.4"),
							},
						},
					},
				},
			},
			backendPoolAfterRetry: &armnetwork.BackendAddressPool{
				Name: ptr.To(testClusterName),
				Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
					LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
						{
							Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: ptr.To("1.2.3.4"),
							},
						},
						{
							Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: ptr.To("2.3.4.5"),
							},
						},
					},
				},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			lbClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
			lbClient.EXPECT().MigrateToIPBased(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(armnetwork.LoadBalancersClientMigrateToIPBasedResponse{}, tc.migrationError)
			backendPoolClient := az.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)

			if tc.migrationError == nil {
				backendPoolClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.backendPool, tc.getBackendPoolError)
			}
			if tc.backendPoolAfterRetry != nil {
				backendPoolClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.backendPoolAfterRetry, nil)
			}

			lbName := testClusterName
			backendPoolNames := []string{testClusterName}
			nicsCountMap := map[string]int{testClusterName: 2}
			err := az.MigrateToIPBasedBackendPoolAndWaitForCompletion(context.TODO(), lbName, backendPoolNames, nicsCountMap)
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

func TestIsNICPool(t *testing.T) {
	tests := []struct {
		desc     string
		bp       *armnetwork.BackendAddressPool
		expected bool
	}{
		{
			desc: "nil BackendAddressPoolPropertiesFormat",
			bp: &armnetwork.BackendAddressPool{
				Name: ptr.To("pool1"),
			},
			expected: false,
		},
		{
			desc: "nil LoadBalancerBackendAddresses",
			bp: &armnetwork.BackendAddressPool{
				Name:       ptr.To("pool1"),
				Properties: &armnetwork.BackendAddressPoolPropertiesFormat{},
			},
			expected: false,
		},
		{
			desc: "empty LoadBalancerBackendAddresses",
			bp: &armnetwork.BackendAddressPool{
				Name: ptr.To("pool1"),
				Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
					LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{},
				},
			},
			expected: false,
		},
		{
			desc: "LoadBalancerBackendAddress with empty IPAddress",
			bp: &armnetwork.BackendAddressPool{
				Name: ptr.To("pool1"),
				Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
					LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
						{
							Name: ptr.To("addr1"),
							Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: ptr.To(""),
							},
						},
					},
				},
			},
			expected: true,
		},
		{
			desc: "LoadBalancerBackendAddress with non-empty IPAddress",
			bp: &armnetwork.BackendAddressPool{
				Name: ptr.To("pool1"),
				Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
					LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
						{
							Name: ptr.To("addr1"),
							Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: ptr.To("10.0.0.1"),
							},
						},
					},
				},
			},
			expected: false,
		},
		{
			desc: "LoadBalancerBackendAddress with both empty and non-empty IPAddress",
			bp: &armnetwork.BackendAddressPool{
				Name: ptr.To("pool1"),
				Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
					LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
						{
							Name: ptr.To("addr1"),
							Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: ptr.To(""),
							},
						},
						{
							Name: ptr.To("addr2"),
							Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: ptr.To("10.0.0.2"),
							},
						},
					},
				},
			},
			expected: true,
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			result := isNICPool(test.bp)
			assert.Equal(t, test.expected, result)
		})
	}
}
