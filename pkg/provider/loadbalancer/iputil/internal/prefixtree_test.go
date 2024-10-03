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
	"fmt"
	"math"
	"net/netip"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/fnutil"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

func Test_bitAt(t *testing.T) {
	bytes := []byte{0b1010_1010, 0b0101_0101}
	assert.Equal(t, uint8(1), bitAt(bytes, 0))
	assert.Equal(t, uint8(0), bitAt(bytes, 1))
	assert.Equal(t, uint8(1), bitAt(bytes, 2))
	assert.Equal(t, uint8(0), bitAt(bytes, 3))

	assert.Equal(t, uint8(1), bitAt(bytes, 4))
	assert.Equal(t, uint8(0), bitAt(bytes, 5))
	assert.Equal(t, uint8(1), bitAt(bytes, 6))
	assert.Equal(t, uint8(0), bitAt(bytes, 7))

	assert.Equal(t, uint8(0), bitAt(bytes, 8))
	assert.Equal(t, uint8(1), bitAt(bytes, 9))
	assert.Equal(t, uint8(0), bitAt(bytes, 10))
	assert.Equal(t, uint8(1), bitAt(bytes, 11))

	assert.Equal(t, uint8(0), bitAt(bytes, 12))
	assert.Equal(t, uint8(1), bitAt(bytes, 13))
	assert.Equal(t, uint8(0), bitAt(bytes, 14))
	assert.Equal(t, uint8(1), bitAt(bytes, 15))

	assert.Panics(t, func() { bitAt(bytes, 16) })
}

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
				"192.170.0.0/16",
				"10.10.0.1/32",

				"192.168.1.0/24",
				"192.168.1.1/32",
			},
			[]string{
				"192.168.0.0/16",
				"192.170.0.0/16",
				"10.10.0.1/32",
			},
		},
		{
			"Collapse",
			[]string{
				"192.168.0.0/24",
				"192.168.1.0/24",
				"192.168.2.0/24",
				"192.168.3.0/24",
				"10.0.0.0/8",
				"172.16.0.0/12",
				"192.168.4.0/24",
				"192.168.5.0/24",
			},
			[]string{
				"10.0.0.0/8",
				"172.16.0.0/12",
				"192.168.0.0/22",
				"192.168.4.0/23",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			var tree = NewPrefixTreeForIPv4()
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
				"2001:db8:0:3::/64",
				"2001:db8:0:5::/64",
			},
			[]string{
				"2001:db8:0:1::/64",
				"2001:db8:0:3::/64",
				"2001:db8:0:5::/64",
			},
		},
		{
			"Overlap",
			[]string{
				"2001:db8::/32",
				"2001:db8:0:1::/64",
				"2001:db8:0:3::/64",
			},
			[]string{
				"2001:db8::/32",
			},
		},
		{
			"Collapse",
			[]string{
				"2001:db8::/32",
				"2001:db8:1::/48",
				"2001:db8:2::/48",
				"2001:db8:3::/48",
				"2001:db8:4::/48",
				"2001:db8:5::/48",
				"2001:db8:6::/48",
				"2001:db8:7::/48",
				"2001:db8:8::/48",
				"2001:db8:9::/48",
				"2001:db8:a::/48",
				"2001:db8:b::/48",
				"2001:db8:c::/48",
				"2001:db8:d::/48",
				"2001:db8:e::/48",
				"2001:db8:f::/48",
				"2001:dbf::/32", // Noise data
				"2001:dba::/32", // Noise data
			},
			[]string{
				"2001:db8::/32",
				"2001:dbf::/32",
				"2001:dba::/32",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			var tree = NewPrefixTreeForIPv6()
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

func TestPrefixTree_Remove(t *testing.T) {
	tests := []struct {
		name     string
		add      []string
		remove   []string
		expected []string
	}{
		{
			name:     "Remove single IPv4 prefix",
			add:      []string{"192.168.0.0/16", "10.0.0.0/8"},
			remove:   []string{"192.168.0.0/16"},
			expected: []string{"10.0.0.0/8"},
		},
		{
			name:     "Remove non-existent IPv4 prefix",
			add:      []string{"192.168.0.0/16", "10.0.0.0/8"},
			remove:   []string{"172.16.0.0/12"},
			expected: []string{"192.168.0.0/16", "10.0.0.0/8"},
		},
		{
			name:     "Remove multiple IPv4 prefixes",
			add:      []string{"192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12"},
			remove:   []string{"192.168.0.0/16", "10.0.0.0/8"},
			expected: []string{"172.16.0.0/12"},
		},
		{
			name:     "Remove single IPv6 prefix",
			add:      []string{"2001:db8::/32", "2001::/32"},
			remove:   []string{"2001:db8::/32"},
			expected: []string{"2001::/32"},
		},
		{
			name:     "Remove non-existent IPv6 prefix",
			add:      []string{"2001:db8::/32", "2001::/32"},
			remove:   []string{"2001:abc::/32"},
			expected: []string{"2001:db8::/32", "2001::/32"},
		},
		{
			name:     "Remove multiple IPv6 prefixes",
			add:      []string{"2001:db8::/32", "2001::/32", "2001:abc::/32"},
			remove:   []string{"2001:db8::/32", "2001::/32"},
			expected: []string{"2001:abc::/32"},
		},
		{
			name:     "Remove subnet and split IPv4",
			add:      []string{"192.168.0.0/16"},
			remove:   []string{"192.168.1.0/24"},
			expected: []string{"192.168.0.0/24", "192.168.2.0/23", "192.168.4.0/22", "192.168.8.0/21", "192.168.16.0/20", "192.168.32.0/19", "192.168.64.0/18", "192.168.128.0/17"},
		},
		{
			name:     "Remove subnet and split IPv6",
			add:      []string{"2001:db8::/32"},
			remove:   []string{"2001:db8:1::/48"},
			expected: []string{"2001:db8::/48", "2001:db8:2::/47", "2001:db8:4::/46", "2001:db8:8::/45", "2001:db8:10::/44", "2001:db8:20::/43", "2001:db8:40::/42", "2001:db8:80::/41", "2001:db8:100::/40", "2001:db8:200::/39", "2001:db8:400::/38", "2001:db8:800::/37", "2001:db8:1000::/36", "2001:db8:2000::/35", "2001:db8:4000::/34", "2001:db8:8000::/33"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree := NewPrefixTreeForIPv4()
			if len(tt.add) > 0 && netip.MustParsePrefix(tt.add[0]).Addr().Is6() {
				tree = NewPrefixTreeForIPv6()
			}

			for _, prefix := range tt.add {
				tree.Add(netip.MustParsePrefix(prefix))
			}

			for _, prefix := range tt.remove {
				tree.Remove(netip.MustParsePrefix(prefix))
			}

			result := tree.List()
			var resultStrings []string
			for _, prefix := range result {
				resultStrings = append(resultStrings, prefix.String())
			}

			assert.ElementsMatch(t, tt.expected, resultStrings)
		})
	}
}

func benchmarkFixtureForIPv4(b *testing.B) *PrefixTree {
	b.StopTimer()
	defer b.StartTimer()

	var tree = NewPrefixTreeForIPv4()
	for i := 0; i < math.MaxInt8; i++ {
		prefix := netip.MustParsePrefix(fmt.Sprintf("%d.0.0.0/8", i))
		tree.Add(prefix)
	}
	return tree
}

func benchmarkFixtureForIPv6(b *testing.B) *PrefixTree {
	b.StopTimer()
	defer b.StartTimer()

	var tree = NewPrefixTreeForIPv6()
	for i := 0; i < math.MaxInt8; i++ {
		prefix := netip.MustParsePrefix(fmt.Sprintf("2001:db8:%x::/64", i))
		tree.Add(prefix)
	}
	return tree
}

func BenchmarkPrefixTree_AddAndRemove(b *testing.B) {
	b.Run("IPv4", func(b *testing.B) {
		tree := benchmarkFixtureForIPv4(b)
		for i := 0; i < b.N; i++ {
			prefix := netip.MustParsePrefix("10.10.10.0/24")
			tree.Remove(prefix)
			tree.Add(prefix)
		}
	})

	b.Run("IPv6", func(b *testing.B) {
		tree := benchmarkFixtureForIPv6(b)
		for i := 0; i < b.N; i++ {
			prefix := netip.MustParsePrefix("2001:db8:10:ff::/90")
			tree.Remove(prefix)
			tree.Add(prefix)
		}
	})
}

func BenchmarkPrefixTree_List(b *testing.B) {

	b.Run("IPv4", func(b *testing.B) {
		tree := benchmarkFixtureForIPv4(b)
		for i := 0; i < b.N; i++ {
			tree.List()
		}
	})

	b.Run("IPv6", func(b *testing.B) {
		tree := benchmarkFixtureForIPv6(b)
		for i := 0; i < b.N; i++ {
			tree.List()
		}
	})
}

func FuzzPrefixTree(f *testing.F) {
	// To reduce fuzzing time
	const (
		MinIPv4Bits = 20
		MinIPv6Bits = 118
	)
	var (
		InitialPrefixIPv4 = netip.MustParsePrefix(fmt.Sprintf("0.0.0.0/%d", MinIPv4Bits))
		InitialPrefixIPv6 = netip.MustParsePrefix(fmt.Sprintf("::/%d", MinIPv6Bits))
		InitialIPv4       = fnutil.Map(fnutil.AsString, ListAddresses(InitialPrefixIPv4))
		InitialIPv6       = fnutil.Map(fnutil.AsString, ListAddresses(InitialPrefixIPv6))
	)

	f.Add(
		netip.MustParseAddr("192.168.0.0").AsSlice(),
		24,
	)
	f.Add(
		netip.MustParseAddr("2001:db8::").AsSlice(),
		64,
	)

	f.Fuzz(func(t *testing.T, ip []byte, bits int) {
		var (
			targetPrefix    netip.Prefix
			targetAddresses []string
		)
		{
			addr, ok := netip.AddrFromSlice(ip)
			if !ok ||
				(addr.Is4() && !InitialPrefixIPv4.Contains(addr)) ||
				(addr.Is6() && !InitialPrefixIPv6.Contains(addr)) {
				// Skip invalid addresses
				t.SkipNow()
				return
			}
			if bits < 0 ||
				(addr.Is4() && (bits <= MinIPv4Bits || bits > 32)) ||
				(addr.Is6() && (bits <= MinIPv6Bits || bits > 128)) {
				// Skip invalid bit lengths
				t.SkipNow()
				return
			}
			p, err := addr.Prefix(bits)
			assert.NoError(t, err)
			targetPrefix = p
			targetAddresses = fnutil.Map(fnutil.AsString, ListAddresses(targetPrefix))
		}
		fmt.Printf("target-prefix: %s\n", targetPrefix.String())

		var (
			tree         *PrefixTree
			allAddresses []string
			initPrefix   netip.Prefix
		)
		if targetPrefix.Addr().Is4() {
			tree = NewPrefixTreeForIPv4()
			tree.Add(InitialPrefixIPv4)
			initPrefix = InitialPrefixIPv4
			allAddresses = InitialIPv4
		} else {
			tree = NewPrefixTreeForIPv6()
			tree.Add(InitialPrefixIPv6)
			initPrefix = InitialPrefixIPv6
			allAddresses = InitialIPv6
		}

		tree.Remove(targetPrefix)
		{
			prefixes := tree.List()
			addresses := fnutil.Map(fnutil.AsString, ListAddresses(prefixes...))

			assert.Empty(
				t, fnutil.Intersection(targetAddresses, addresses),
				"actual-prefixes: %s, target-prefixes: %s",
				fnutil.Map(fnutil.AsString, prefixes),
				targetPrefix.String(),
			)
			assert.ElementsMatch(
				t, addresses, fnutil.Difference(allAddresses, targetAddresses),
				"actual-prefixes: %s, target-prefixes: %s",
				fnutil.Map(fnutil.AsString, prefixes),
				targetPrefix.String(),
			)
		}

		tree.Add(targetPrefix)
		{
			prefixes := tree.List()
			addresses := fnutil.Map(fnutil.AsString, ListAddresses(prefixes...))

			assert.Equal(t, []string{initPrefix.String()}, fnutil.Map(fnutil.AsString, prefixes))
			assert.ElementsMatch(t, addresses, allAddresses)
		}
	})
}
