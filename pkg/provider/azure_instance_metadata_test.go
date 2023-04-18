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
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestFillNetInterfacePublicIPs tests if IPv6 IPs from imds load balancer are
// properly handled.
func TestFillNetInterfacePublicIPs(t *testing.T) {
	testcases := []struct {
		desc                 string
		publicIPs            []PublicIPMetadata
		netInterface         *NetworkInterface
		expectedNetInterface *NetworkInterface
	}{
		{
			desc: "IPv6/DualStack",
			publicIPs: []PublicIPMetadata{
				{
					FrontendIPAddress: "20.0.0.0",
					PrivateIPAddress:  "10.244.0.0",
				},
				{
					FrontendIPAddress: "[2001::1]",
					PrivateIPAddress:  "[fd00::1]",
				},
			},
			netInterface: &NetworkInterface{
				IPV4: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "10.244.0.0",
						},
					},
				},
				IPV6: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "fd00::1",
						},
					},
				},
			},
			expectedNetInterface: &NetworkInterface{
				IPV4: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "10.244.0.0",
							PublicIP:  "20.0.0.0",
						},
					},
				},
				IPV6: NetworkData{
					IPAddress: []IPAddress{
						{
							PrivateIP: "fd00::1",
							PublicIP:  "2001::1",
						},
					},
				},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			fillNetInterfacePublicIPs(tc.publicIPs, tc.netInterface)
			assert.Equal(t, tc.expectedNetInterface, tc.netInterface)
		})
	}
}
