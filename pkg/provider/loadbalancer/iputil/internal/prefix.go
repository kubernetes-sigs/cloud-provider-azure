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
	"net/netip"
	"sort"
)

// ListAddresses returns all IP addresses contained within the given prefixes.
// Note: This function is not optimized for large address ranges.
// It may consume significant memory and perform poorly when listing
// a large number of addresses. Use with caution on large prefixes.
func ListAddresses(prefixes ...netip.Prefix) []netip.Addr {
	var rv []netip.Addr
	for _, p := range prefixes {
		for addr := p.Addr(); p.Contains(addr); addr = addr.Next() {
			rv = append(rv, addr)
		}
	}
	return rv
}

// ContainsPrefix checks if prefix p fully contains prefix o.
// It returns true if o is a subset of p, meaning all addresses in o are also in p.
// This is true when p overlaps with o and p has fewer or equal number of bits than o.
func ContainsPrefix(p netip.Prefix, o netip.Prefix) bool {
	return p.Overlaps(o) && p.Bits() <= o.Bits()
}

// IsAdjacentPrefixes checks if two prefixes are adjacent and can be merged into a single prefix.
// Two prefixes are considered adjacent if they have the same length and their
// addresses are consecutive.
//
// Examples:
//   - Adjacent:     192.168.0.0/32 and 192.168.0.1/32
//   - Adjacent:     192.168.0.0/24 and 192.168.1.0/24
//   - Not adjacent: 192.168.0.1/32 and 192.168.0.2/32 (cannot merge)
//   - Not adjacent: 192.168.0.0/24 and 192.168.0.0/25 (different lengths)
func IsAdjacentPrefixes(p1, p2 netip.Prefix) bool {
	if p1.Bits() != p2.Bits() {
		return false
	}

	p1Bytes := p1.Addr().AsSlice()
	p2Bytes := p2.Addr().AsSlice()

	if bitAt(p1Bytes, p1.Bits()-1) == 0 {
		setBitAt(p1Bytes, p1.Bits()-1, 1)
		addr, _ := netip.AddrFromSlice(p1Bytes)
		return addr == p2.Addr()
	} else {
		setBitAt(p2Bytes, p2.Bits()-1, 1)
		addr, _ := netip.AddrFromSlice(p2Bytes)
		return addr == p1.Addr()
	}
}

// AggregatePrefixesForSingleIPFamily merges overlapping or adjacent prefixes into a single prefix.
// The input prefixes must be the same IP family (IPv4 or IPv6).
// For example,
// - [192.168.0.0/32, 192.168.0.1/32] -> [192.168.0.0/31] (adjacent)
// - [192.168.0.0/24, 192.168.0.1/32] -> [192.168.1.0/24] (overlapping)
func AggregatePrefixesForSingleIPFamily(prefixes []netip.Prefix) []netip.Prefix {
	if len(prefixes) <= 1 {
		return prefixes
	}

	sort.Slice(prefixes, func(i, j int) bool {
		if prefixes[i].Addr() == prefixes[j].Addr() {
			return prefixes[i].Bits() < prefixes[j].Bits()
		}
		return prefixes[i].Addr().Less(prefixes[j].Addr())
	})

	var rv = []netip.Prefix{
		prefixes[0],
	}

	for i := 1; i < len(prefixes); {
		last, p := rv[len(rv)-1], prefixes[i]
		if ContainsPrefix(last, p) {
			// Skip overlapping prefixes
			i++
			continue
		}
		rv = append(rv, p)

		// Merge adjacent prefixes if possible
		for len(rv) >= 2 {
			p1, p2 := rv[len(rv)-2], rv[len(rv)-1]
			if !IsAdjacentPrefixes(p1, p2) {
				break
			}

			bits := p1.Bits() - 1
			p, _ := p1.Addr().Prefix(bits)
			rv = rv[:len(rv)-2]
			rv = append(rv, p)
		}
	}
	return rv
}
