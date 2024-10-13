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
	"fmt"
	"net/netip"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/fnutil"
)

func TestIsPrefixesAllowAll(t *testing.T) {
	tests := []struct {
		input  []netip.Prefix
		output bool
	}{
		{
			input: []netip.Prefix{
				netip.MustParsePrefix("10.0.0.1/32"),
				netip.MustParsePrefix("10.0.0.2/32"),
			},
			output: false,
		},
		{
			input: []netip.Prefix{
				netip.MustParsePrefix("2001:db8::/32"),
			},
			output: false,
		},
		{
			input: []netip.Prefix{
				netip.MustParsePrefix("10.0.0.2/32"),
				netip.MustParsePrefix("2001:db8::/32"),
			},
			output: false,
		},
		{
			input: []netip.Prefix{
				netip.MustParsePrefix("0.0.0.0/0"),
				netip.MustParsePrefix("10.0.0.2/32"),
			},
			output: true,
		},
		{
			input: []netip.Prefix{
				netip.MustParsePrefix("10.0.0.1/0"),
				netip.MustParsePrefix("10.0.0.2/32"),
			},
			output: true,
		},
		{
			input: []netip.Prefix{
				netip.MustParsePrefix("::/0"),
				netip.MustParsePrefix("2001:db8::/32"),
			},
			output: true,
		},
		{
			input: []netip.Prefix{
				netip.MustParsePrefix("::/0"),
				netip.MustParsePrefix("10.0.0.2/32"),
				netip.MustParsePrefix("2001:db8::/32"),
			},
			output: true,
		},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.output, IsPrefixesAllowAll(tt.input), "expect IsPrefixesAllowAll(%v) = %v", tt.input, tt.output)
	}
}

func TestParsePrefix(t *testing.T) {
	t.Run("1 ipv4 cidr", func(t *testing.T) {
		actual, err := ParsePrefix("10.10.10.0/24")
		assert.NoError(t, err)
		assert.Equal(t, netip.MustParsePrefix("10.10.10.0/24"), actual)
	})
	t.Run("1 ipv6 cidr", func(t *testing.T) {
		actual, err := ParsePrefix("2001:db8::/32")
		assert.NoError(t, err)
		assert.Equal(t, netip.MustParsePrefix("2001:db8::/32"), actual)
	})
	t.Run("invalid cidr", func(t *testing.T) {
		{
			_, err := ParsePrefix("")
			assert.Error(t, err)
		}
		{
			_, err := ParsePrefix("foo")
			assert.Error(t, err)
		}
		// below two tests check for valid cidr but not valid network prefix
		{
			_, err := ParsePrefix("10.10.10.1/24")
			assert.Error(t, err)
		}
		{
			_, err := ParsePrefix("2001:db8::5/32")
			assert.Error(t, err)
		}
	})
}

func TestGroupPrefixesByFamily(t *testing.T) {
	tests := []struct {
		Name  string
		Input []netip.Prefix
		IPv4  []netip.Prefix
		IPv6  []netip.Prefix
	}{
		{
			Name:  "Empty",
			Input: []netip.Prefix{},
		},
		{
			Name: "IPv4",
			Input: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.1/32"),
				netip.MustParsePrefix("10.0.0.0/8"),
			},
			IPv4: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.1/32"),
				netip.MustParsePrefix("10.0.0.0/8"),
			},
		},
		{
			Name: "IPv6",
			Input: []netip.Prefix{
				netip.MustParsePrefix("2001:db8::1/128"),
				netip.MustParsePrefix("::/0"),
			},
			IPv6: []netip.Prefix{
				netip.MustParsePrefix("2001:db8::1/128"),
				netip.MustParsePrefix("::/0"),
			},
		},
		{
			Name: "Mixed",
			Input: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.1/32"),
				netip.MustParsePrefix("10.0.0.0/8"),
				netip.MustParsePrefix("2001:db8::1/128"),
				netip.MustParsePrefix("::/0"),
			},
			IPv4: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.1/32"),
				netip.MustParsePrefix("10.0.0.0/8"),
			},
			IPv6: []netip.Prefix{
				netip.MustParsePrefix("2001:db8::1/128"),
				netip.MustParsePrefix("::/0"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			ipv4, ipv6 := GroupPrefixesByFamily(tt.Input)
			assert.Equal(t, tt.IPv4, ipv4)
			assert.Equal(t, tt.IPv6, ipv6)
		})
	}
}

func TestAggregatePrefixes(t *testing.T) {
	tests := []struct {
		Name   string
		Input  []netip.Prefix
		Output []netip.Prefix
	}{
		{
			Name:  "Empty",
			Input: []netip.Prefix{},
		},
		{
			Name: "NoOverlap IPv4",
			Input: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.0/16"),
				netip.MustParsePrefix("10.10.0.1/32"),
			},
			Output: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.0/16"),
				netip.MustParsePrefix("10.10.0.1/32"),
			},
		},
		{
			Name: "Overlap IPv4",
			Input: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.0/16"),
				netip.MustParsePrefix("192.170.0.0/16"),
				netip.MustParsePrefix("10.10.0.1/32"),

				netip.MustParsePrefix("192.168.1.0/24"),
				netip.MustParsePrefix("192.168.1.1/32"),
			},
			Output: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.0/16"),
				netip.MustParsePrefix("192.170.0.0/16"),
				netip.MustParsePrefix("10.10.0.1/32"),
			},
		},
		{
			Name: "Collapse IPv4",
			Input: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.0/24"),
				netip.MustParsePrefix("192.168.1.0/24"),
				netip.MustParsePrefix("192.168.2.0/24"),
				netip.MustParsePrefix("192.168.3.0/24"),
				netip.MustParsePrefix("10.0.0.0/8"),
				netip.MustParsePrefix("172.16.0.0/12"),
				netip.MustParsePrefix("192.168.4.0/24"),
				netip.MustParsePrefix("192.168.5.0/24"),
			},
			Output: []netip.Prefix{
				netip.MustParsePrefix("10.0.0.0/8"),
				netip.MustParsePrefix("172.16.0.0/12"),
				netip.MustParsePrefix("192.168.0.0/22"),
				netip.MustParsePrefix("192.168.4.0/23"),
			},
		},
		{
			Name: "Collapse IPv6",
			Input: []netip.Prefix{
				netip.MustParsePrefix("2001:db8::/32"),
				netip.MustParsePrefix("2001:db8:1::/48"),
				netip.MustParsePrefix("2001:db8:2::/48"),
				netip.MustParsePrefix("2001:db8:3::/48"),
				netip.MustParsePrefix("2001:db8:4::/48"),
				netip.MustParsePrefix("2001:db8:5::/48"),
				netip.MustParsePrefix("2001:db8:6::/48"),
				netip.MustParsePrefix("2001:db8:7::/48"),
				netip.MustParsePrefix("2001:dbf::/32"),
				netip.MustParsePrefix("2001:dba::/32"),
			},
			Output: []netip.Prefix{
				netip.MustParsePrefix("2001:db8::/32"),
				netip.MustParsePrefix("2001:dbf::/32"),
				netip.MustParsePrefix("2001:dba::/32"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			var got = AggregatePrefixes(tt.Input)
			sort.Slice(got, func(i, j int) bool {
				return got[i].String() < got[j].String()
			})
			sort.Slice(tt.Output, func(i, j int) bool {
				return tt.Output[i].String() < tt.Output[j].String()
			})
			assert.Equal(t, tt.Output, got)

			{
				// Test the prefix tree implementation
				var got = AggregatePrefixesWithPrefixTree(tt.Input)

				sort.Slice(got, func(i, j int) bool {
					return got[i].String() < got[j].String()
				})
				sort.Slice(tt.Output, func(i, j int) bool {
					return tt.Output[i].String() < tt.Output[j].String()
				})
				assert.Equal(t, tt.Output, got)
			}
		})
	}
}

func TestContainsPrefix(t *testing.T) {
	tests := []struct {
		name     string
		p        netip.Prefix
		o        netip.Prefix
		expected bool
	}{
		{
			name:     "IPv4: Exact match",
			p:        netip.MustParsePrefix("192.168.0.0/24"),
			o:        netip.MustParsePrefix("192.168.0.0/24"),
			expected: true,
		},
		{
			name:     "IPv4: Larger contains smaller",
			p:        netip.MustParsePrefix("192.168.0.0/16"),
			o:        netip.MustParsePrefix("192.168.1.0/24"),
			expected: true,
		},
		{
			name:     "IPv4: Smaller doesn't contain larger",
			p:        netip.MustParsePrefix("192.168.1.0/24"),
			o:        netip.MustParsePrefix("192.168.0.0/16"),
			expected: false,
		},
		{
			name:     "IPv4: Non-overlapping",
			p:        netip.MustParsePrefix("192.168.0.0/24"),
			o:        netip.MustParsePrefix("192.169.0.0/24"),
			expected: false,
		},
		{
			name:     "IPv6: Exact match",
			p:        netip.MustParsePrefix("2001:db8::/32"),
			o:        netip.MustParsePrefix("2001:db8::/32"),
			expected: true,
		},
		{
			name:     "IPv6: Larger contains smaller",
			p:        netip.MustParsePrefix("2001:db8::/32"),
			o:        netip.MustParsePrefix("2001:db8:1::/48"),
			expected: true,
		},
		{
			name:     "IPv6: Smaller doesn't contain larger",
			p:        netip.MustParsePrefix("2001:db8:1::/48"),
			o:        netip.MustParsePrefix("2001:db8::/32"),
			expected: false,
		},
		{
			name:     "IPv6: Non-overlapping",
			p:        netip.MustParsePrefix("2001:db8::/32"),
			o:        netip.MustParsePrefix("2001:db9::/32"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ContainsPrefix(tt.p, tt.o)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMergePrefixes(t *testing.T) {
	tests := []struct {
		name     string
		p1       netip.Prefix
		p2       netip.Prefix
		expected netip.Prefix
		ok       bool
	}{
		{
			name:     "IPv4: Overlapping prefixes",
			p1:       netip.MustParsePrefix("192.168.0.0/24"),
			p2:       netip.MustParsePrefix("192.168.0.0/25"),
			expected: netip.Prefix{},
			ok:       false,
		},
		{
			name:     "IPv4: Adjacent prefixes",
			p1:       netip.MustParsePrefix("192.168.0.0/25"),
			p2:       netip.MustParsePrefix("192.168.0.128/25"),
			expected: netip.MustParsePrefix("192.168.0.0/24"),
			ok:       true,
		},
		{
			name:     "IPv4: Non-mergeable prefixes",
			p1:       netip.MustParsePrefix("192.168.0.0/24"),
			p2:       netip.MustParsePrefix("192.168.2.0/24"),
			expected: netip.Prefix{},
			ok:       false,
		},
		{
			name:     "IPv6: Overlapping prefixes",
			p1:       netip.MustParsePrefix("2001:db8::/32"),
			p2:       netip.MustParsePrefix("2001:db8::/48"),
			expected: netip.Prefix{},
			ok:       false,
		},
		{
			name:     "IPv6: Adjacent prefixes",
			p1:       netip.MustParsePrefix("2001:db8::/33"),
			p2:       netip.MustParsePrefix("2001:db8:8000::/33"),
			expected: netip.MustParsePrefix("2001:db8::/32"),
			ok:       true,
		},
		{
			name:     "IPv6: Non-mergeable prefixes",
			p1:       netip.MustParsePrefix("2001:db8::/32"),
			p2:       netip.MustParsePrefix("2001:db10::/32"),
			expected: netip.Prefix{},
			ok:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := mergeAdjacentPrefixes(tt.p1, tt.p2)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// BenchmarkPrefixFixtures generates a list of prefixes for aggregation benchmarks.
// The second return value is the expected result of the aggregation.
func benchmarkPrefixFixtures() ([]netip.Prefix, []netip.Prefix) {
	var rv []netip.Prefix
	for i := 0; i <= 255; i++ {
		for j := 0; j <= 255; j++ {
			rv = append(rv, netip.MustParsePrefix(fmt.Sprintf("192.168.%d.%d/32", i, j)))
		}
	}

	return rv, []netip.Prefix{
		netip.MustParsePrefix("192.168.0.0/16"),
	}
}

func BenchmarkAggregatePrefixes(b *testing.B) {
	prefixes, expected := benchmarkPrefixFixtures()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		actual := AggregatePrefixes(prefixes)
		assert.Len(b, actual, 1)
		assert.Equal(b, expected, actual)
	}
}

func BenchmarkAggregatePrefixesWithPrefixTree(b *testing.B) {
	prefixes, expected := benchmarkPrefixFixtures()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		actual := AggregatePrefixesWithPrefixTree(prefixes)
		assert.Len(b, actual, 1)
		assert.Equal(b, expected, actual)
	}
}

func FuzzAggregatePrefixesIPv4(f *testing.F) {
	f.Add(
		netip.MustParseAddr("192.168.0.0").AsSlice(),
		24,
		netip.MustParseAddr("192.168.1.0").AsSlice(),
		24,
		netip.MustParseAddr("10.0.0.0").AsSlice(),
		8,
	)

	parsePrefix := func(bytes []byte, bits int) (netip.Prefix, error) {
		if bits < 0 || bits > 32 {
			return netip.Prefix{}, fmt.Errorf("invalid bits")
		}

		addr, ok := netip.AddrFromSlice(bytes)
		if !ok {
			return netip.Prefix{}, fmt.Errorf("invalid address")
		}

		return addr.Prefix(bits)
	}

	listAddressesAsString := func(prefixes ...netip.Prefix) []string {
		rv := make(map[string]struct{})
		for _, p := range prefixes {
			for addr := p.Addr(); p.Contains(addr); addr = addr.Next() {
				rv[addr.String()] = struct{}{}
			}
		}
		return fnutil.Keys(rv)
	}

	f.Fuzz(func(
		t *testing.T,
		p1Bytes []byte, p1Bits int,
		p2Bytes []byte, p2Bits int,
		p3Bytes []byte, p3Bits int,
	) {

		p1, err := parsePrefix(p1Bytes, p1Bits)
		if err != nil {
			return
		}
		p2, err := parsePrefix(p2Bytes, p2Bits)
		if err != nil {
			return
		}
		p3, err := parsePrefix(p3Bytes, p3Bits)
		if err != nil {
			return
		}

		input := []netip.Prefix{p1, p2, p3}
		output := AggregatePrefixes(input)

		prefixAsString := func(p netip.Prefix) string { return p.String() }
		t.Logf("input: %s", fnutil.Map(prefixAsString, input))
		t.Logf("output: %s", fnutil.Map(prefixAsString, output))

		expectedAddresses := listAddressesAsString(input...)
		actualAddresses := listAddressesAsString(output...)
		assert.Equal(t, len(expectedAddresses), len(actualAddresses))

		sort.Strings(expectedAddresses)
		sort.Strings(actualAddresses)
		assert.Equal(t, expectedAddresses, actualAddresses)
	})
}
