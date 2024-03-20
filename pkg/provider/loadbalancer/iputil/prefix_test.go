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
