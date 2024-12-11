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
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer"
)

func TestListManagedLBs(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		existingLBs     []*armnetwork.LoadBalancer
		expectedLBs     []*armnetwork.LoadBalancer
		callTimes       int
		multiSLBConfigs []config.MultipleStandardLoadBalancerConfiguration
		clientErr       error
		expectedErr     error
	}{
		{
			clientErr:   errors.New("some error"),
			expectedErr: errors.New("some error"),
		},
		{
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
		az := GetTestCloud(ctrl)
		if len(test.multiSLBConfigs) > 0 {
			az.LoadBalancerSku = consts.LoadBalancerSkuStandard
			az.MultipleStandardLoadBalancerConfigurations = test.multiSLBConfigs
		} else {
			az.LoadBalancerSku = consts.LoadBalancerSkuBasic
		}

		mockLBClient := az.lbRepo.(*loadbalancer.MockRepository)
		mockLBClient.EXPECT().ListByResourceGroup(gomock.Any(), az.ResourceGroup).Return(test.existingLBs, test.clientErr)
		mockVMSet := NewMockVMSet(ctrl)
		mockVMSet.EXPECT().GetAgentPoolVMSetNames(gomock.Any(), gomock.Any()).Return(&[]string{"vmas-0", "vmas-1"}, nil).Times(test.callTimes)
		mockVMSet.EXPECT().GetPrimaryVMSetName().Return("vmas-0").AnyTimes()
		az.VMSet = mockVMSet

		lbs, err := az.ListManagedLBs(context.TODO(), &v1.Service{}, []*v1.Node{}, "kubernetes")
		assert.Equal(t, test.expectedErr, err)
		assert.Equal(t, test.expectedLBs, lbs)
	}
}

func TestMigrateToIPBasedBackendPoolAndWaitForCompletion(t *testing.T) {
	t.Parallel()
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
			migrationError: errors.New("error"),
			expectedError:  errors.New("error"),
		},
		{
			desc:                "MigrateToIPBasedBackendPoolAndWaitForCompletion should return the error if failed to get the backend pool",
			getBackendPoolError: errors.New("error"),
			expectedError:       errors.New("error"),
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
			t.Parallel()
			az := GetTestCloud(ctrl)
			lbClient := loadbalancer.NewMockRepository(ctrl)
			lbClient.EXPECT().MigrateToIPBased(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, tc.migrationError)
			if tc.migrationError == nil {
				lbClient.EXPECT().GetBackendPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.backendPool, tc.getBackendPoolError)
			}
			if tc.backendPoolAfterRetry != nil {
				lbClient.EXPECT().GetBackendPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.backendPoolAfterRetry, nil)
			}
			az.lbRepo = lbClient

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
	t.Parallel()
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
	t.Parallel()
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
			t.Parallel()
			az := GetTestCloud(ctrl)
			svc := getTestService("test", v1.ProtocolTCP, nil, false)
			assert.True(t, az.serviceOwnsRule(&svc, consts.SharedProbeName))
		})
	}
}

func TestIsNICPool(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			result := isNICPool(test.bp)
			assert.Equal(t, test.expected, result)
		})
	}
}
