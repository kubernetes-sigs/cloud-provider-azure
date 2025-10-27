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
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func TestCreateOrUpdatePIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		clientErr          error
		expectedErr        error
		cacheExpectedEmpty bool
	}{
		{
			clientErr:          &azcore.ResponseError{StatusCode: http.StatusPreconditionFailed},
			expectedErr:        fmt.Errorf("UNAVAILABLE"),
			cacheExpectedEmpty: true,
		},
		{
			clientErr:          &azcore.ResponseError{ErrorCode: consts.OperationCanceledErrorMessage},
			expectedErr:        fmt.Errorf("canceledandsupersededduetoanotheroperation"),
			cacheExpectedEmpty: true,
		},
		{
			clientErr:          &azcore.ResponseError{StatusCode: http.StatusInternalServerError},
			expectedErr:        fmt.Errorf("UNAVAILABLE"),
			cacheExpectedEmpty: false,
		},
	}

	for _, test := range tests {
		az := GetTestCloud(ctrl)
		az.pipCache.Set(az.ResourceGroup, []*armnetwork.PublicIPAddress{{Name: ptr.To("test")}})
		mockPIPClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
		mockPIPClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, "nic", gomock.Any()).Return(nil, test.clientErr)
		if test.cacheExpectedEmpty {
			mockPIPClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]*armnetwork.PublicIPAddress{}, nil)
		}

		err := az.CreateOrUpdatePIP(&v1.Service{}, az.ResourceGroup, &armnetwork.PublicIPAddress{Name: ptr.To("nic")})
		assert.Contains(t, err.Error(), test.expectedErr.Error())

		cachedPIP, err := az.pipCache.GetWithDeepCopy(context.TODO(), az.ResourceGroup, azcache.CacheReadTypeDefault)
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
	mockPIPClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
	mockPIPClient.EXPECT().Delete(gomock.Any(), az.ResourceGroup, "pip").Return(&azcore.ResponseError{StatusCode: http.StatusInternalServerError})

	err := az.DeletePublicIP(&v1.Service{}, az.ResourceGroup, "pip")
	assert.Contains(t, err.Error(), "UNAVAILABLE")
}

func TestListPIP(t *testing.T) {
	tests := []struct {
		desc          string
		pipCache      []*armnetwork.PublicIPAddress
		expectPIPList bool
		existingPIPs  []*armnetwork.PublicIPAddress
	}{
		{
			desc:     "listPIP should return data from cache, when data is empty slice",
			pipCache: []*armnetwork.PublicIPAddress{},
		},
		{
			desc: "listPIP should return data from cache",
			pipCache: []*armnetwork.PublicIPAddress{
				{Name: ptr.To("pip1")},
				{Name: ptr.To("pip2")},
			},
		},
		{
			desc:          "listPIP should return data from arm list call",
			expectPIPList: true,
			existingPIPs:  []*armnetwork.PublicIPAddress{{Name: ptr.To("pip")}},
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			az := GetTestCloud(ctrl)
			if test.pipCache != nil {
				pipCache := &sync.Map{}
				for _, pip := range test.pipCache {
					pip := pip
					pipCache.Store(ptr.Deref(pip.Name, ""), pip)
				}
				az.pipCache.Set(az.ResourceGroup, pipCache)
			}
			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			if test.expectPIPList {
				mockPIPsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(test.existingPIPs, nil).MaxTimes(2)
			}
			pips, err := az.listPIP(context.TODO(), az.ResourceGroup, azcache.CacheReadTypeDefault)
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

	tests := []struct {
		desc          string
		pipCache      []*armnetwork.PublicIPAddress
		expectPIPList bool
		existingPIPs  []*armnetwork.PublicIPAddress
		expectExists  bool
		expectedPIP   *armnetwork.PublicIPAddress
	}{
		{
			desc:         "getPublicIPAddress should return pip from cache when it exists",
			pipCache:     []*armnetwork.PublicIPAddress{{Name: ptr.To("pip")}},
			expectExists: true,
			expectedPIP:  &armnetwork.PublicIPAddress{Name: ptr.To("pip")},
		},
		{
			desc:          "getPublicIPAddress should from list call when cache is empty",
			expectPIPList: true,
			existingPIPs: []*armnetwork.PublicIPAddress{
				{Name: ptr.To("pip")},
				{Name: ptr.To("pip1")},
			},
			expectExists: true,
			expectedPIP:  &armnetwork.PublicIPAddress{Name: ptr.To("pip")},
		},
		{
			desc:          "getPublicIPAddress should try listing when pip does not exist",
			pipCache:      []*armnetwork.PublicIPAddress{{Name: ptr.To("pip1")}},
			expectPIPList: true,
			existingPIPs:  []*armnetwork.PublicIPAddress{{Name: ptr.To("pip1")}},
			expectExists:  false,
			expectedPIP:   nil,
		},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			pipCache := &sync.Map{}
			for _, pip := range test.pipCache {
				pip := pip
				pipCache.Store(ptr.Deref(pip.Name, ""), pip)
			}
			az := GetTestCloud(ctrl)
			az.pipCache.Set(az.ResourceGroup, pipCache)
			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			if test.expectPIPList {
				mockPIPsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(test.existingPIPs, nil).MaxTimes(2)
			}
			pip, pipExists, err := az.getPublicIPAddress(context.TODO(), az.ResourceGroup, "PIP", azcache.CacheReadTypeDefault)
			if test.expectedPIP != nil {
				assert.Equal(t, *test.expectedPIP, *pip)
			}
			assert.Equal(t, test.expectExists, pipExists)
			assert.NoError(t, err)
		})
	}
}

func TestFindMatchedPIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testPIP := &armnetwork.PublicIPAddress{
		Name: ptr.To("pipName"),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			IPAddress: ptr.To("1.2.3.4"),
		},
	}

	for _, tc := range []struct {
		description         string
		pips                []*armnetwork.PublicIPAddress
		pipsSecondTime      []*armnetwork.PublicIPAddress
		pipName             string
		loadBalancerIP      string
		shouldRefreshCache  bool
		listError           error
		listErrorSecondTime error
		expectedPIP         *armnetwork.PublicIPAddress
		expectedError       error
	}{
		{
			description:        "should ignore pipName if loadBalancerIP is specified",
			pips:               []*armnetwork.PublicIPAddress{testPIP},
			pipsSecondTime:     []*armnetwork.PublicIPAddress{testPIP},
			shouldRefreshCache: true,
			loadBalancerIP:     "2.3.4.5",
			pipName:            "pipName",
			expectedError:      errors.New("findMatchedPIPByLoadBalancerIP: cannot find public IP with IP address 2.3.4.5 in resource group rg"),
		},
		{
			description:   "should report an error if failed to list pip",
			listError:     &azcore.ResponseError{ErrorCode: "list error"},
			expectedError: errors.New("list error"),
		},
		{
			description:        "should refresh the cache if failed to search by name",
			pips:               []*armnetwork.PublicIPAddress{},
			pipsSecondTime:     []*armnetwork.PublicIPAddress{testPIP},
			shouldRefreshCache: true,
			pipName:            "pipName",
			expectedPIP:        testPIP,
		},
		{
			description: "should return the expected pip by name",
			pips:        []*armnetwork.PublicIPAddress{testPIP},
			pipName:     "pipName",
			expectedPIP: testPIP,
		},
		{
			description:         "should report an error if failed to list pip second time",
			pips:                []*armnetwork.PublicIPAddress{},
			listErrorSecondTime: &azcore.ResponseError{ErrorCode: "list error"},
			shouldRefreshCache:  true,
			expectedError:       errors.New("list error"),
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(tc.pips, tc.listError)
			if tc.shouldRefreshCache {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(tc.pipsSecondTime, tc.listErrorSecondTime)
			}

			pip, err := az.findMatchedPIP(context.TODO(), tc.loadBalancerIP, tc.pipName, "rg")
			assert.Equal(t, tc.expectedPIP, pip)
			if tc.expectedError != nil {
				assert.Contains(t, err.Error(), tc.expectedError.Error())
			}
		})
	}
}

func TestFindMatchedPIPByLoadBalancerIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testPIP := &armnetwork.PublicIPAddress{
		Name: ptr.To("pipName"),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			IPAddress: ptr.To("1.2.3.4"),
		},
	}
	testCases := []struct {
		desc               string
		pips               []*armnetwork.PublicIPAddress
		pipsSecondTime     []*armnetwork.PublicIPAddress
		shouldRefreshCache bool
		expectedPIP        *armnetwork.PublicIPAddress
		expectedError      bool
	}{
		{
			desc:        "findMatchedPIPByLoadBalancerIP shall return the matched ip",
			pips:        []*armnetwork.PublicIPAddress{testPIP},
			expectedPIP: testPIP,
		},
		{
			desc:               "findMatchedPIPByLoadBalancerIP shall return error if ip is not found",
			pips:               []*armnetwork.PublicIPAddress{},
			shouldRefreshCache: true,
			expectedError:      true,
		},
		{
			desc:               "findMatchedPIPByLoadBalancerIP should refresh cache if no matched ip is found",
			pipsSecondTime:     []*armnetwork.PublicIPAddress{testPIP},
			shouldRefreshCache: true,
			expectedPIP:        testPIP,
		},
	}
	for _, test := range testCases {
		test := test
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)

			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			if test.shouldRefreshCache {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(test.pipsSecondTime, nil)
			}
			pip, err := az.findMatchedPIPByLoadBalancerIP(context.TODO(), test.pips, "1.2.3.4", "rg")
			assert.Equal(t, test.expectedPIP, pip)
			assert.Equal(t, test.expectedError, err != nil)
		})
	}
}
