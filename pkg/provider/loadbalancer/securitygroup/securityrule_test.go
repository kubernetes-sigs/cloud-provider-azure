/*
Copyright 2024 The Kubernetes Authors.

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

package securitygroup

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestSeparateIPsAndServiceTags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		input        []string
		expectedIPs  []netip.Prefix
		expectedTags []string
	}{
		{
			name:  "Mixed IPs and service tags",
			input: []string{"192.168.0.1", "10.0.0.0/24", "Internet", "172.16.0.1/32", "AzureLoadBalancer"},
			expectedIPs: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.1/32"),
				netip.MustParsePrefix("10.0.0.0/24"),
				netip.MustParsePrefix("172.16.0.1/32"),
			},
			expectedTags: []string{"Internet", "AzureLoadBalancer"},
		},
		{
			name:  "Only IPs",
			input: []string{"192.168.0.1", "10.0.0.0/24", "172.16.0.1/32"},
			expectedIPs: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.1/32"),
				netip.MustParsePrefix("10.0.0.0/24"),
				netip.MustParsePrefix("172.16.0.1/32"),
			},
		},
		{
			name:         "Only service tags",
			input:        []string{"Internet", "AzureLoadBalancer", "VirtualNetwork"},
			expectedTags: []string{"Internet", "AzureLoadBalancer", "VirtualNetwork"},
		},
		{
			name:  "Empty input",
			input: []string{},
		},
		{
			name:  "With Asterisk",
			input: []string{"192.168.0.1", "*", "10.0.0.0/24", "Internet"},
			expectedIPs: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.1/32"),
				netip.MustParsePrefix("10.0.0.0/24"),
			},
			expectedTags: []string{"*", "Internet"},
		},
		{
			name:  "IPv6 addresses and prefixes",
			input: []string{"2001:db8::1", "2001:db8::/32", "fe80::1234:5678:9abc:def0", "Internet"},
			expectedIPs: []netip.Prefix{
				netip.MustParsePrefix("2001:db8::1/128"),
				netip.MustParsePrefix("2001:db8::/32"),
				netip.MustParsePrefix("fe80::1234:5678:9abc:def0/128"),
			},
			expectedTags: []string{"Internet"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ips, tags := SeparateIPsAndServiceTags(tc.input)

			if !reflect.DeepEqual(ips, tc.expectedIPs) {
				t.Errorf("Expected IPs %v, but got %v", tc.expectedIPs, ips)
			}

			if !reflect.DeepEqual(tags, tc.expectedTags) {
				t.Errorf("Expected tags %v, but got %v", tc.expectedTags, tags)
			}
		})
	}
}
