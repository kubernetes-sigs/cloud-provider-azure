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

package iputil

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFamilyOfAddr(t *testing.T) {
	tests := []struct {
		input  netip.Addr
		output Family
	}{
		{
			input:  netip.MustParseAddr("10.0.0.1"),
			output: IPv4,
		},
		{
			input:  netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
			output: IPv6,
		},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.output, FamilyOfAddr(tt.input))
	}
}

func TestAreAddressesFromSameFamily(t *testing.T) {
	type args struct {
		addresses []netip.Addr
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "1 IPv4",
			args: args{
				addresses: []netip.Addr{
					netip.MustParseAddr("10.0.0.1"),
				},
			},
			want: true,
		},

		{
			name: "N IPv4",
			args: args{
				addresses: []netip.Addr{
					netip.MustParseAddr("10.0.0.1"),
					netip.MustParseAddr("10.0.0.2"),
					netip.MustParseAddr("192.168.0.0"),
				},
			},
			want: true,
		},
		{
			name: "1 IPv6",
			args: args{
				addresses: []netip.Addr{
					netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
				},
			},
			want: true,
		},
		{
			name: "N IPv6",
			args: args{
				addresses: []netip.Addr{
					netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
					netip.MustParseAddr("fe80:0000:0000:0000:0204:61ff:fe9d:f156"),
					netip.MustParseAddr("2607:f0d0:1002:0051:0000:0000:0000:0004"),
				},
			},
			want: true,
		},
		{
			name: "IPv4 with IPv6",
			args: args{
				addresses: []netip.Addr{
					netip.MustParseAddr("2001:0db8:85a3:0000:0000:8a2e:0370:7334"),
					netip.MustParseAddr("10.0.0.1"),
					netip.MustParseAddr("fe80:0000:0000:0000:0204:61ff:fe9d:f156"),
					netip.MustParseAddr("2607:f0d0:1002:0051:0000:0000:0000:0004"),
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equalf(t, tt.want, AreAddressesFromSameFamily(tt.args.addresses), "AreAddressesFromSameFamily(%v)", tt.args.addresses)
		})
	}
}

func TestArePrefixesFromSameFamily(t *testing.T) {
	type args struct {
		prefixes []netip.Prefix
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "1 IPv4",
			args: args{
				prefixes: []netip.Prefix{
					netip.MustParsePrefix("10.0.0.1/32"),
				},
			},
			want: true,
		},
		{
			name: "N IPv4",
			args: args{
				prefixes: []netip.Prefix{
					netip.MustParsePrefix("10.0.0.1/32"),
					netip.MustParsePrefix("10.0.0.2/32"),
					netip.MustParsePrefix("192.168.0.0/16"),
				},
			},
			want: true,
		},
		{
			name: "1 IPv6",
			args: args{
				prefixes: []netip.Prefix{
					netip.MustParsePrefix("2001:0db8:85a3:0000:0000:8a2e:0370:7334/64"),
				},
			},
			want: true,
		},
		{
			name: "N IPv6",
			args: args{
				prefixes: []netip.Prefix{
					netip.MustParsePrefix("2001:0db8:85a3:0000:0000:8a2e:0370:7334/64"),
					netip.MustParsePrefix("fe80:0000:0000:0000:0202:b3ff:fe1e:8329/96"),
					netip.MustParsePrefix("2607:f8b0:4005:0809:0000:0000:0000:200e/48"),
				},
			},
			want: true,
		},
		{
			name: "IPv4 with IPv6",
			args: args{
				prefixes: []netip.Prefix{
					netip.MustParsePrefix("2001:0db8:85a3:0000:0000:8a2e:0370:7334/64"),
					netip.MustParsePrefix("fe80:0000:0000:0000:0202:b3ff:fe1e:8329/96"),
					netip.MustParsePrefix("192.168.0.0/16"),
					netip.MustParsePrefix("2607:f8b0:4005:0809:0000:0000:0000:200e/48"),
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equalf(t, tt.want, ArePrefixesFromSameFamily(tt.args.prefixes), "ArePrefixesFromSameFamily(%v)", tt.args.prefixes)
		})
	}
}
