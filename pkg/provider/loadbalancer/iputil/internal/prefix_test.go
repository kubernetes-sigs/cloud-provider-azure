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

package internal

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestListAddresses(t *testing.T) {
	tests := []struct {
		Name     string
		Prefixes []netip.Prefix
		Expected []netip.Addr
	}{
		{
			Name: "Empty",
		},
		{
			Name:     "Single IPv4 Address",
			Prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.1/32")},
			Expected: []netip.Addr{netip.MustParseAddr("192.168.1.1")},
		},
		{
			Name:     "IPv4 Subnet",
			Prefixes: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/30")},
			Expected: []netip.Addr{
				netip.MustParseAddr("192.168.1.0"),
				netip.MustParseAddr("192.168.1.1"),
				netip.MustParseAddr("192.168.1.2"),
				netip.MustParseAddr("192.168.1.3"),
			},
		},
		{
			Name:     "Single IPv6 Address",
			Prefixes: []netip.Prefix{netip.MustParsePrefix("2001:db8::1/128")},
			Expected: []netip.Addr{netip.MustParseAddr("2001:db8::1")},
		},
		{
			Name:     "IPv6 Subnet",
			Prefixes: []netip.Prefix{netip.MustParsePrefix("2001:db8::/126")},
			Expected: []netip.Addr{
				netip.MustParseAddr("2001:db8::"),
				netip.MustParseAddr("2001:db8::1"),
				netip.MustParseAddr("2001:db8::2"),
				netip.MustParseAddr("2001:db8::3"),
			},
		},
		{
			Name: "Multiple Prefixes",
			Prefixes: []netip.Prefix{
				netip.MustParsePrefix("192.168.1.0/31"),
				netip.MustParsePrefix("2001:db8::/127"),
			},
			Expected: []netip.Addr{
				netip.MustParseAddr("192.168.1.0"),
				netip.MustParseAddr("192.168.1.1"),
				netip.MustParseAddr("2001:db8::"),
				netip.MustParseAddr("2001:db8::1"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			actual := ListAddresses(tt.Prefixes...)
			assert.Equal(t, tt.Expected, actual)
		})
	}
}
