package internal

import (
	"net/netip"
)

func ListAddresses(prefixes ...netip.Prefix) []netip.Addr {
	var rv []netip.Addr
	for _, p := range prefixes {
		for addr := p.Addr(); p.Contains(addr); addr = addr.Next() {
			rv = append(rv, addr)
		}
	}
	return rv
}
