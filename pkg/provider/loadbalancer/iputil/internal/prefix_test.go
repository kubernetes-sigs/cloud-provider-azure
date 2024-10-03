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
