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

	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/provider/publicip"
)

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
			listError:     errors.New("list error"),
			expectedError: errors.New("findMatchedPIPByLoadBalancerIP: failed to listPIP: list error"),
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
			listErrorSecondTime: errors.New("list error"),
			shouldRefreshCache:  true,
			expectedError:       errors.New("findMatchedPIPByName: failed to listPIP force refresh: list error"),
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			mockPIPsClient := az.pipRepo.(*publicip.MockRepository)
			mockPIPsClient.EXPECT().ListByResourceGroup(gomock.Any(), "rg", gomock.Any()).Return(tc.pips, tc.listError)
			if tc.shouldRefreshCache {
				mockPIPsClient.EXPECT().ListByResourceGroup(gomock.Any(), "rg", gomock.Any()).Return(tc.pipsSecondTime, tc.listErrorSecondTime)
			}

			pip, err := az.findMatchedPIP(context.TODO(), tc.loadBalancerIP, tc.pipName, "rg")
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

			mockPIPsClient := az.pipRepo.(*publicip.MockRepository)
			if test.shouldRefreshCache {
				mockPIPsClient.EXPECT().ListByResourceGroup(gomock.Any(), "rg", gomock.Any()).Return(test.pipsSecondTime, nil)
			}
			pip, err := az.findMatchedPIPByLoadBalancerIP(context.TODO(), test.pips, "1.2.3.4", "rg")
			assert.Equal(t, test.expectedPIP, pip)
			assert.Equal(t, test.expectedError, err != nil)
		})
	}
}
