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

package iputil

import (
	"math"
	"net/netip"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPrefixTreeIPv4(t *testing.T) {
	tests := []struct {
		Name   string
		Input  []string
		Output []string
	}{
		{
			"Empty",
			[]string{},
			nil,
		},
		{
			"NoOverlap",
			[]string{
				"192.168.0.0/16",
				"10.10.0.1/32",
			},
			[]string{
				"192.168.0.0/16",
				"10.10.0.1/32",
			},
		},
		{
			"Overlap",
			[]string{
				"192.168.0.0/16",
				"192.169.0.0/16",
				"10.10.0.1/32",

				"192.168.1.0/24",
				"192.168.1.1/32",
			},
			[]string{
				"192.168.0.0/16",
				"192.169.0.0/16",
				"10.10.0.1/32",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			var tree = newPrefixTreeForIPv4()
			for _, ip := range tt.Input {
				p := netip.MustParsePrefix(ip)
				tree.Add(p)
			}

			var got []string
			for _, ip := range tree.List() {
				got = append(got, ip.String())
			}

			sort.Strings(got)
			sort.Strings(tt.Output)

			assert.Equal(t, tt.Output, got)
		})
	}
}

func TestPrefixTreeIPv6(t *testing.T) {
	tests := []struct {
		Name   string
		Input  []string
		Output []string
	}{
		{
			"Empty",
			[]string{},
			nil,
		},
		{
			"NoOverlap",
			[]string{
				"2001:db8:0:1::/64",
				"2001:db8:0:2::/64",
				"2001:db8:0:3::/64",
			},
			[]string{
				"2001:db8:0:1::/64",
				"2001:db8:0:2::/64",
				"2001:db8:0:3::/64",
			},
		},
		{
			"Overlap",
			[]string{
				"2001:db8::/32",
				"2001:db8:0:1::/64",
				"2001:db8:0:2::/64",
				"2001:db8:0:3::/64",
			},
			[]string{
				"2001:db8::/32",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			var tree = newPrefixTreeForIPv6()
			for _, ip := range tt.Input {
				p := netip.MustParsePrefix(ip)
				tree.Add(p)
			}

			var got []string
			for _, ip := range tree.List() {
				got = append(got, ip.String())
			}

			sort.Strings(got)
			sort.Strings(tt.Output)

			assert.Equal(t, tt.Output, got)
		})
	}
}

func BenchmarkPrefixTree_Add(b *testing.B) {
	b.Run("IPv4", func(b *testing.B) {
		var tree = newPrefixTreeForIPv4()
		for i := 0; i < b.N; i++ {
			addr := netip.AddrFrom4([4]byte{
				byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i),
			})
			prefix, _ := addr.Prefix(32)

			tree.Add(prefix)
		}
	})

	b.Run("IPv6", func(b *testing.B) {
		var tree = newPrefixTreeForIPv6()
		for i := 0; i < b.N; i++ {
			addr := netip.AddrFrom16([16]byte{
				0, 0, 0, 0,
				0, 0, 0, 0,
				byte(i >> 56), byte(i >> 48), byte(i >> 40), byte(i >> 32),
				byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i),
			})
			prefix, _ := addr.Prefix(128)

			tree.Add(prefix)
		}
	})
}

func BenchmarkPrefixTree_List(b *testing.B) {

	b.Run("IPv4", func(b *testing.B) {
		b.StopTimer()
		var tree = newPrefixTreeForIPv4()
		for i := 0; i < math.MaxInt8; i++ {
			addr := netip.AddrFrom4([4]byte{
				byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i),
			})
			prefix, err := addr.Prefix(32)
			assert.NoError(b, err)

			tree.Add(prefix)
		}
		b.StartTimer()
		for i := 0; i < b.N; i++ {
			tree.List()
		}
	})

	b.Run("IPv6", func(b *testing.B) {
		b.StopTimer()
		var tree = newPrefixTreeForIPv6()
		for i := 0; i < math.MaxInt8; i++ {
			addr := netip.AddrFrom16([16]byte{
				0, 0, 0, 0,
				0, 0, 0, 0,
				byte(i >> 56), byte(i >> 48), byte(i >> 40), byte(i >> 32),
				byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i),
			})
			prefix, err := addr.Prefix(128)
			assert.NoError(b, err)

			tree.Add(prefix)
		}
		b.StartTimer()
		for i := 0; i < b.N; i++ {
			tree.List()
		}
	})
}
