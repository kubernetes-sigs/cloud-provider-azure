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
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privatelinkserviceclient/mockprivatelinkserviceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/publicipclient/mockpublicipclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func TestExtractNotFound(t *testing.T) {
	notFound := &retry.Error{HTTPStatusCode: http.StatusNotFound}
	otherHTTP := &retry.Error{HTTPStatusCode: http.StatusForbidden}
	otherErr := &retry.Error{HTTPStatusCode: http.StatusTooManyRequests}

	tests := []struct {
		err         *retry.Error
		expectedErr *retry.Error
		exists      bool
	}{
		{nil, nil, true},
		{otherErr, otherErr, false},
		{notFound, nil, false},
		{otherHTTP, otherHTTP, false},
	}

	for _, test := range tests {
		exists, err := checkResourceExistsFromError(test.err)
		if test.exists != exists {
			t.Errorf("expected: %v, saw: %v", test.exists, exists)
		}
		if !reflect.DeepEqual(test.expectedErr, err) {
			t.Errorf("expected err: %v, saw: %v", test.expectedErr, err)
		}
	}
}

func TestIsNodeUnmanaged(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		name           string
		unmanagedNodes *utilsets.IgnoreCaseSet
		node           string
		expected       bool
		expectErr      bool
	}{
		{
			name:           "unmanaged node should return true",
			unmanagedNodes: utilsets.NewString("node1", "node2"),
			node:           "node1",
			expected:       true,
		},
		{
			name:           "managed node should return false",
			unmanagedNodes: utilsets.NewString("node1", "node2"),
			node:           "node3",
			expected:       false,
		},
		{
			name:           "empty unmanagedNodes should return true",
			unmanagedNodes: utilsets.NewString(),
			node:           "node3",
			expected:       false,
		},
		{
			name:           "no synced informer should report error",
			unmanagedNodes: utilsets.NewString(),
			node:           "node1",
			expectErr:      true,
		},
	}

	az := GetTestCloud(ctrl)
	for _, test := range tests {
		az.unmanagedNodes = test.unmanagedNodes
		if test.expectErr {
			az.nodeInformerSynced = func() bool {
				return false
			}
		}

		real, err := az.IsNodeUnmanaged(test.node)
		if test.expectErr {
			assert.Error(t, err, test.name)
			continue
		}

		assert.NoError(t, err, test.name)
		assert.Equal(t, test.expected, real, test.name)
	}
}

func TestIsNodeUnmanagedByProviderID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		providerID string
		expected   bool
		name       string
	}{
		{
			providerID: consts.CloudProviderName + ":///subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-0",
			expected:   false,
		},
		{
			providerID: consts.CloudProviderName + "://",
			expected:   true,
		},
		{
			providerID: ":///subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-0",
			expected:   true,
		},
		{
			providerID: "aws:///subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-0",
			expected:   true,
		},
		{
			providerID: "k8s-agent-AAAAAAAA-0",
			expected:   true,
		},
	}

	az := GetTestCloud(ctrl)
	for _, test := range tests {
		isUnmanagedNode := az.IsNodeUnmanagedByProviderID(test.providerID)
		assert.Equal(t, test.expected, isUnmanagedNode, test.providerID)
	}
}

func TestConvertResourceGroupNameToLower(t *testing.T) {
	tests := []struct {
		desc        string
		resourceID  string
		expected    string
		expectError bool
	}{
		{
			desc:        "empty string should report error",
			resourceID:  "",
			expectError: true,
		},
		{
			desc:        "resourceID not in Azure format should report error",
			resourceID:  "invalid-id",
			expectError: true,
		},
		{
			desc:        "providerID not in Azure format should report error",
			resourceID:  "azure://invalid-id",
			expectError: true,
		},
		{
			desc:       "resource group name in VM providerID should be converted",
			resourceID: consts.CloudProviderName + ":///subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-0",
			expected:   consts.CloudProviderName + ":///subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myresourcegroupname/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-0",
		},
		{
			desc:       "resource group name in VM resourceID should be converted",
			resourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-0",
			expected:   "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myresourcegroupname/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-0",
		},
		{
			desc:       "resource group name in VMSS providerID should be converted",
			resourceID: consts.CloudProviderName + ":///subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachineScaleSets/myScaleSetName/virtualMachines/156",
			expected:   consts.CloudProviderName + ":///subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myresourcegroupname/providers/Microsoft.Compute/virtualMachineScaleSets/myScaleSetName/virtualMachines/156",
		},
		{
			desc:       "resource group name in VMSS resourceID should be converted",
			resourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachineScaleSets/myScaleSetName/virtualMachines/156",
			expected:   "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myresourcegroupname/providers/Microsoft.Compute/virtualMachineScaleSets/myScaleSetName/virtualMachines/156",
		},
	}

	for _, test := range tests {
		real, err := ConvertResourceGroupNameToLower(test.resourceID)
		if test.expectError {
			assert.NotNil(t, err, test.desc)
			continue
		}

		assert.Nil(t, err, test.desc)
		assert.Equal(t, test.expected, real, test.desc)
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

func TestGetPublicIPAddress(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		desc          string
		pipCache      []network.PublicIPAddress
		expectPIPList bool
		existingPIPs  []network.PublicIPAddress
		expectExists  bool
		expectedPIP   network.PublicIPAddress
	}{
		{
			desc:         "getPublicIPAddress should return pip from cache when it exists",
			pipCache:     []network.PublicIPAddress{{Name: pointer.String("pip")}},
			expectExists: true,
			expectedPIP:  network.PublicIPAddress{Name: pointer.String("pip")},
		},
		{
			desc:          "getPublicIPAddress should from list call when cache is empty",
			expectPIPList: true,
			existingPIPs: []network.PublicIPAddress{
				{Name: pointer.String("pip")},
				{Name: pointer.String("pip1")},
			},
			expectExists: true,
			expectedPIP:  network.PublicIPAddress{Name: pointer.String("pip")},
		},
		{
			desc:          "getPublicIPAddress should try listing when pip does not exist",
			pipCache:      []network.PublicIPAddress{{Name: pointer.String("pip1")}},
			expectPIPList: true,
			existingPIPs:  []network.PublicIPAddress{{Name: pointer.String("pip1")}},
			expectExists:  false,
			expectedPIP:   network.PublicIPAddress{},
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			pipCache := &sync.Map{}
			for _, pip := range test.pipCache {
				pip := pip
				pipCache.Store(pointer.StringDeref(pip.Name, ""), &pip)
			}
			az := GetTestCloud(ctrl)
			az.pipCache.Set(az.ResourceGroup, pipCache)
			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			if test.expectPIPList {
				mockPIPsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(test.existingPIPs, nil).MaxTimes(2)
			}
			pip, pipExists, err := az.getPublicIPAddress(az.ResourceGroup, "PIP", azcache.CacheReadTypeDefault)
			assert.Equal(t, test.expectedPIP, pip)
			assert.Equal(t, test.expectExists, pipExists)
			assert.NoError(t, err)
		})
	}
}

func TestListPIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		desc          string
		pipCache      []network.PublicIPAddress
		expectPIPList bool
		existingPIPs  []network.PublicIPAddress
	}{
		{
			desc:     "listPIP should return data from cache, when data is empty slice",
			pipCache: []network.PublicIPAddress{},
		},
		{
			desc: "listPIP should return data from cache",
			pipCache: []network.PublicIPAddress{
				{Name: pointer.String("pip1")},
				{Name: pointer.String("pip2")},
			},
		},
		{
			desc:          "listPIP should return data from arm list call",
			expectPIPList: true,
			existingPIPs:  []network.PublicIPAddress{{Name: pointer.String("pip")}},
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			if test.pipCache != nil {
				pipCache := &sync.Map{}
				for _, pip := range test.pipCache {
					pip := pip
					pipCache.Store(pointer.StringDeref(pip.Name, ""), &pip)
				}
				az.pipCache.Set(az.ResourceGroup, pipCache)
			}
			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			if test.expectPIPList {
				mockPIPsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(test.existingPIPs, nil).MaxTimes(2)
			}
			pips, err := az.listPIP(az.ResourceGroup, azcache.CacheReadTypeDefault)
			if test.expectPIPList {
				assert.ElementsMatch(t, test.existingPIPs, pips)
			} else {
				assert.ElementsMatch(t, test.pipCache, pips)
			}
			assert.NoError(t, err)
		})
	}
}

func TestGetPrivateLinkService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.plsCache.Set("rg*frontendID", &network.PrivateLinkService{Name: pointer.String("pls")})

	// cache hit
	pls, err := az.getPrivateLinkService("rg", pointer.String("frontendID"), azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "pls", *pls.Name)

	// cache miss
	mockPLSClient := az.PrivateLinkServiceClient.(*mockprivatelinkserviceclient.MockInterface)
	mockPLSClient.EXPECT().List(gomock.Any(), "rg1").Return([]network.PrivateLinkService{
		{
			Name: pointer.String("pls1"),
			PrivateLinkServiceProperties: &network.PrivateLinkServiceProperties{
				LoadBalancerFrontendIPConfigurations: &[]network.FrontendIPConfiguration{
					{
						ID: pointer.String("frontendID1"),
					},
				},
			},
		},
	}, nil)
	pls, err = az.getPrivateLinkService("rg1", pointer.String("frontendID1"), azcache.CacheReadTypeDefault)
	assert.NoError(t, err)
	assert.Equal(t, "pls1", *pls.Name)
}

func TestGetPLSCacheKey(t *testing.T) {
	rg, frontendID := "rg", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/ipconfig"
	assert.Equal(t, "rg*/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/ipconfig", getPLSCacheKey(rg, frontendID))
}

func TestParsePLSCacheKey(t *testing.T) {
	key := "rg*/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/ipconfig"
	rg, frontendID := parsePLSCacheKey(key)
	assert.Equal(t, "rg", rg)
	assert.Equal(t, "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/ipconfig", frontendID)
}
