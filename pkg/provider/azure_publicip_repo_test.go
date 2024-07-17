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
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/publicipclient/mockpublicipclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

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
		az.pipCache.Set(az.ResourceGroup, []network.PublicIPAddress{{Name: ptr.To("test")}})
		mockPIPClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
		mockPIPClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, "nic", gomock.Any()).Return(test.clientErr)
		if test.cacheExpectedEmpty {
			mockPIPClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]network.PublicIPAddress{}, nil)
		}

		err := az.CreateOrUpdatePIP(&v1.Service{}, az.ResourceGroup, network.PublicIPAddress{Name: ptr.To("nic")})
		assert.EqualError(t, test.expectedErr, err.Error())

		cachedPIP, err := az.pipCache.GetWithDeepCopy(az.ResourceGroup, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		if test.cacheExpectedEmpty {
			assert.Empty(t, cachedPIP)
		} else {
			assert.NotEmpty(t, cachedPIP)
		}
	}
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
				{Name: ptr.To("pip1")},
				{Name: ptr.To("pip2")},
			},
		},
		{
			desc:          "listPIP should return data from arm list call",
			expectPIPList: true,
			existingPIPs:  []network.PublicIPAddress{{Name: ptr.To("pip")}},
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			if test.pipCache != nil {
				pipCache := &sync.Map{}
				for _, pip := range test.pipCache {
					pip := pip
					pipCache.Store(ptr.Deref(pip.Name, ""), &pip)
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
			pipCache:     []network.PublicIPAddress{{Name: ptr.To("pip")}},
			expectExists: true,
			expectedPIP:  network.PublicIPAddress{Name: ptr.To("pip")},
		},
		{
			desc:          "getPublicIPAddress should from list call when cache is empty",
			expectPIPList: true,
			existingPIPs: []network.PublicIPAddress{
				{Name: ptr.To("pip")},
				{Name: ptr.To("pip1")},
			},
			expectExists: true,
			expectedPIP:  network.PublicIPAddress{Name: ptr.To("pip")},
		},
		{
			desc:          "getPublicIPAddress should try listing when pip does not exist",
			pipCache:      []network.PublicIPAddress{{Name: ptr.To("pip1")}},
			expectPIPList: true,
			existingPIPs:  []network.PublicIPAddress{{Name: ptr.To("pip1")}},
			expectExists:  false,
			expectedPIP:   network.PublicIPAddress{},
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			pipCache := &sync.Map{}
			for _, pip := range test.pipCache {
				pip := pip
				pipCache.Store(ptr.Deref(pip.Name, ""), &pip)
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

func TestFindMatchedPIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testPIP := network.PublicIPAddress{
		Name: ptr.To("pipName"),
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			IPAddress: ptr.To("1.2.3.4"),
		},
	}

	for _, tc := range []struct {
		description         string
		pips                []network.PublicIPAddress
		pipsSecondTime      []network.PublicIPAddress
		pipName             string
		loadBalancerIP      string
		shouldRefreshCache  bool
		listError           *retry.Error
		listErrorSecondTime *retry.Error
		expectedPIP         *network.PublicIPAddress
		expectedError       error
	}{
		{
			description:        "should ignore pipName if loadBalancerIP is specified",
			pips:               []network.PublicIPAddress{testPIP},
			pipsSecondTime:     []network.PublicIPAddress{testPIP},
			shouldRefreshCache: true,
			loadBalancerIP:     "2.3.4.5",
			pipName:            "pipName",
			expectedError:      errors.New("findMatchedPIPByLoadBalancerIP: cannot find public IP with IP address 2.3.4.5 in resource group rg"),
		},
		{
			description:   "should report an error if failed to list pip",
			listError:     retry.NewError(false, errors.New("list error")),
			expectedError: errors.New("findMatchedPIPByLoadBalancerIP: failed to listPIP: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: list error"),
		},
		{
			description:        "should refresh the cache if failed to search by name",
			pips:               []network.PublicIPAddress{},
			pipsSecondTime:     []network.PublicIPAddress{testPIP},
			shouldRefreshCache: true,
			pipName:            "pipName",
			expectedPIP:        &testPIP,
		},
		{
			description: "should return the expected pip by name",
			pips:        []network.PublicIPAddress{testPIP},
			pipName:     "pipName",
			expectedPIP: &testPIP,
		},
		{
			description:         "should report an error if failed to list pip second time",
			pips:                []network.PublicIPAddress{},
			listErrorSecondTime: retry.NewError(false, errors.New("list error")),
			shouldRefreshCache:  true,
			expectedError:       errors.New("findMatchedPIPByName: failed to listPIP force refresh: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: list error"),
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(tc.pips, tc.listError)
			if tc.shouldRefreshCache {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(tc.pipsSecondTime, tc.listErrorSecondTime)
			}

			pip, err := az.findMatchedPIP(tc.loadBalancerIP, tc.pipName, "rg")
			assert.Equal(t, tc.expectedPIP, pip)
			if tc.expectedError != nil {
				assert.Equal(t, tc.expectedError.Error(), err.Error())
			}
		})
	}
}

func TestFindMatchedPIPByLoadBalancerIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testPIP := network.PublicIPAddress{
		Name: ptr.To("pipName"),
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			IPAddress: ptr.To("1.2.3.4"),
		},
	}
	testCases := []struct {
		desc               string
		pips               []network.PublicIPAddress
		pipsSecondTime     []network.PublicIPAddress
		shouldRefreshCache bool
		expectedPIP        *network.PublicIPAddress
		expectedError      bool
	}{
		{
			desc:        "findMatchedPIPByLoadBalancerIP shall return the matched ip",
			pips:        []network.PublicIPAddress{testPIP},
			expectedPIP: &testPIP,
		},
		{
			desc:               "findMatchedPIPByLoadBalancerIP shall return error if ip is not found",
			pips:               []network.PublicIPAddress{},
			shouldRefreshCache: true,
			expectedError:      true,
		},
		{
			desc:               "findMatchedPIPByLoadBalancerIP should refresh cache if no matched ip is found",
			pipsSecondTime:     []network.PublicIPAddress{testPIP},
			shouldRefreshCache: true,
			expectedPIP:        &testPIP,
		},
	}
	for _, test := range testCases {
		test := test
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)

			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			if test.shouldRefreshCache {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(test.pipsSecondTime, nil)
			}
			pip, err := az.findMatchedPIPByLoadBalancerIP(&test.pips, "1.2.3.4", "rg")
			assert.Equal(t, test.expectedPIP, pip)
			assert.Equal(t, test.expectedError, err != nil)
		})
	}
}
