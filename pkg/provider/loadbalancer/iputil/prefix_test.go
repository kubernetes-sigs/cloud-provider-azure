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

			expected := fnutil.Map(fnutil.AsString, tt.Output)
			actual := fnutil.Map(fnutil.AsString, got)
			assert.Equal(t, expected, actual)
		})
	}
}

func BenchmarkAggregatePrefixes(b *testing.B) {
	fixtureIPv4Prefixes := func(n int64) []netip.Prefix {
		prefixes := make([]netip.Prefix, 0, n)
		for i := int64(0); i < n; i++ {
			addr := netip.AddrFrom4([4]byte{
				byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i),
			})
			prefix, err := addr.Prefix(32)
			assert.NoError(b, err)
			prefixes = append(prefixes, prefix)
		}

		return prefixes
	}

	fixtureIPv6Prefixes := func(n int64) []netip.Prefix {
		prefixes := make([]netip.Prefix, 0, n)
		for i := int64(0); i < n; i++ {
			addr := netip.AddrFrom16([16]byte{
				0, 0, 0, 0,
				0, 0, 0, 0,
				byte(i >> 56), byte(i >> 48), byte(i >> 40), byte(i >> 32),
				byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i),
			})
			prefix, err := addr.Prefix(128)
			assert.NoError(b, err)
			prefixes = append(prefixes, prefix)
		}
		return prefixes
	}

	runIPv4Tests := func(b *testing.B, n int64) {
		b.Run(fmt.Sprintf("IPv4-%d", n), func(b *testing.B) {
			b.StopTimer()
			prefixes := fixtureIPv4Prefixes(n)
			b.StartTimer()

			for i := 0; i < b.N; i++ {
				AggregatePrefixes(prefixes)
			}
		})
	}

	runIPv6Tests := func(b *testing.B, n int64) {
		b.Run(fmt.Sprintf("IPv6-%d", n), func(b *testing.B) {
			b.StopTimer()
			prefixes := fixtureIPv4Prefixes(n)
			b.StartTimer()

			for i := 0; i < b.N; i++ {
				AggregatePrefixes(prefixes)
			}
		})
	}

	runMixedTests := func(b *testing.B, n int64) {
		b.Run(fmt.Sprintf("IPv4-IPv6-%d", 2*n), func(b *testing.B) {
			b.StopTimer()
			prefixes := append(fixtureIPv4Prefixes(n), fixtureIPv6Prefixes(n)...)
			b.StartTimer()

			for i := 0; i < b.N; i++ {
				AggregatePrefixes(prefixes)
			}
		})
	}

	for _, n := range []int64{100, 1_000, 10_000} {
		runIPv4Tests(b, n)
		runIPv6Tests(b, n)
		runMixedTests(b, n)
	}
}

func TestExcludePrefixes(t *testing.T) {
	tests := []struct {
		name     string
		prefixes []string
		exclude  []string
		expected []string
	}{
		{
			name:     "Exclude single IPv4 prefix",
			prefixes: []string{"192.168.0.0/16", "10.0.0.0/8"},
			exclude:  []string{"192.168.0.0/16"},
			expected: []string{"10.0.0.0/8"},
		},
		{
			name:     "Exclude non-existent IPv4 prefix",
			prefixes: []string{"192.168.0.0/16", "10.0.0.0/8"},
			exclude:  []string{"172.16.0.0/12"},
			expected: []string{"192.168.0.0/16", "10.0.0.0/8"},
		},
		{
			name:     "Exclude multiple IPv4 prefixes",
			prefixes: []string{"192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12"},
			exclude:  []string{"192.168.0.0/16", "10.0.0.0/8"},
			expected: []string{"172.16.0.0/12"},
		},
		{
			name:     "Exclude single IPv6 prefix",
			prefixes: []string{"2001:db8::/32", "2001::/32"},
			exclude:  []string{"2001:db8::/32"},
			expected: []string{"2001::/32"},
		},
		{
			name:     "Exclude non-existent IPv6 prefix",
			prefixes: []string{"2001:db8::/32", "2001::/32"},
			exclude:  []string{"2001:abc::/32"},
			expected: []string{"2001:db8::/32", "2001::/32"},
		},
		{
			name:     "Exclude multiple IPv6 prefixes",
			prefixes: []string{"2001:db8::/32", "2001::/32", "2001:abc::/32"},
			exclude:  []string{"2001:db8::/32", "2001::/32"},
			expected: []string{"2001:abc::/32"},
		},
		{
			name:     "Exclude subnet and split IPv4",
			prefixes: []string{"192.168.0.0/16"},
			exclude:  []string{"192.168.1.0/24"},
			expected: []string{"192.168.0.0/24", "192.168.2.0/23", "192.168.4.0/22", "192.168.8.0/21", "192.168.16.0/20", "192.168.32.0/19", "192.168.64.0/18", "192.168.128.0/17"},
		},
		{
			name:     "Exclude subnet and split IPv6",
			prefixes: []string{"2001:db8::/32"},
			exclude:  []string{"2001:db8:1::/48"},
			expected: []string{"2001:db8::/48", "2001:db8:2::/47", "2001:db8:4::/46", "2001:db8:8::/45", "2001:db8:10::/44", "2001:db8:20::/43", "2001:db8:40::/42", "2001:db8:80::/41", "2001:db8:100::/40", "2001:db8:200::/39", "2001:db8:400::/38", "2001:db8:800::/37", "2001:db8:1000::/36", "2001:db8:2000::/35", "2001:db8:4000::/34", "2001:db8:8000::/33"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefixes := make([]netip.Prefix, len(tt.prefixes))
			for i, p := range tt.prefixes {
				prefixes[i] = netip.MustParsePrefix(p)
			}

			exclude := make([]netip.Prefix, len(tt.exclude))
			for i, p := range tt.exclude {
				exclude[i] = netip.MustParsePrefix(p)
			}

			result := ExcludePrefixes(prefixes, exclude)

			resultStrings := make([]string, len(result))
			for i, p := range result {
				resultStrings[i] = p.String()
			}

			assert.ElementsMatch(t, tt.expected, resultStrings)
		})
	}
}
