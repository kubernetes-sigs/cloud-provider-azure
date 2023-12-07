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

func TestParsePrefixes(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		actual, err := ParsePrefixes([]string{})
		assert.NoError(t, err)
		assert.Empty(t, actual)
	})
	t.Run("1 ipv4 cidr", func(t *testing.T) {
		actual, err := ParsePrefixes([]string{
			"10.10.10.0/24",
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{
			netip.MustParsePrefix("10.10.10.0/24"),
		}, actual)
	})
	t.Run("1 ipv6 cidr", func(t *testing.T) {
		actual, err := ParsePrefixes([]string{
			"2001:db8::/32",
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{
			netip.MustParsePrefix("2001:db8::/32"),
		}, actual)
	})
	t.Run("multiple cidrs", func(t *testing.T) {
		actual, err := ParsePrefixes([]string{
			"10.10.10.0/24",
			"2001:db8::/32",
		})
		assert.NoError(t, err)
		assert.Equal(t, []netip.Prefix{
			netip.MustParsePrefix("10.10.10.0/24"),
			netip.MustParsePrefix("2001:db8::/32"),
		}, actual)
	})
	t.Run("invalid cidr", func(t *testing.T) {
		{
			_, err := ParsePrefixes([]string{""})
			assert.Error(t, err)
		}
		{
			_, err := ParsePrefixes([]string{"foo"})
			assert.Error(t, err)
		}
		{
			_, err := ParsePrefixes([]string{"10.10.10.0/24", "foo"})
			assert.Error(t, err)
		}
	})
}
