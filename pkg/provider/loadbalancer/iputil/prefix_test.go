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
				netip.MustParsePrefix("192.169.0.0/16"),
				netip.MustParsePrefix("10.10.0.1/32"),

				netip.MustParsePrefix("192.168.1.0/24"),
				netip.MustParsePrefix("192.168.1.1/32"),
			},
			Output: []netip.Prefix{
				netip.MustParsePrefix("192.168.0.0/16"),
				netip.MustParsePrefix("192.169.0.0/16"),
				netip.MustParsePrefix("10.10.0.1/32"),
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
