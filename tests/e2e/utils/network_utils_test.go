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

package utils

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	aznetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"
	"github.com/stretchr/testify/assert"

	"k8s.io/utils/ptr"
)

func TestSelectSubnets(t *testing.T) {
	testcases := []struct {
		desc            string
		ipFamily        IPFamily
		vNetSubnets     []*aznetwork.Subnet
		expectedSubnets []*string
	}{
		{
			"only control-plane subnet",
			IPv4,
			[]*aznetwork.Subnet{
				{Name: ptr.To("control-plane")},
			},
			[]*string{},
		},
		{
			"IPv4",
			IPv4,
			[]*aznetwork.Subnet{
				{Name: ptr.To("subnet0"), Properties: &aznetwork.SubnetPropertiesFormat{AddressPrefix: ptr.To("10.0.0.0/24")}},
			},
			[]*string{to.Ptr("10.0.0.0/24")},
		},
		{
			"IPv6",
			IPv6,
			[]*aznetwork.Subnet{
				{
					Name: ptr.To("subnet0"),
					Properties: &aznetwork.SubnetPropertiesFormat{
						AddressPrefix:   ptr.To("10.0.0.0/24"),
						AddressPrefixes: []*string{to.Ptr("10.0.0.0/24"), to.Ptr("2001::1/96")},
					},
				},
			},
			[]*string{to.Ptr("2001::1/96")},
		},
		{
			"DualStack",
			DualStack,
			[]*aznetwork.Subnet{
				{
					Name: ptr.To("subnet0"),
					Properties: &aznetwork.SubnetPropertiesFormat{
						AddressPrefix:   ptr.To("10.0.0.0/24"),
						AddressPrefixes: []*string{to.Ptr("10.0.0.0/24"), to.Ptr("2001::1/96")},
					},
				},
			},
			[]*string{to.Ptr("10.0.0.0/24"), to.Ptr("2001::1/96")},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			subnets, err := selectSubnets(tc.ipFamily, tc.vNetSubnets)
			assert.Nil(t, err)
			assert.True(t, CompareStrings(subnets, tc.expectedSubnets))
		})
	}
}
