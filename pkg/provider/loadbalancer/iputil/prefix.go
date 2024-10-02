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
)

// IsPrefixesAllowAll returns true if one of the prefixes allows all addresses.
// FIXME: it should return true if the aggregated prefix allows all addresses. Now it only checks one by one.
func IsPrefixesAllowAll(prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Bits() == 0 {
			return true
		}
	}
	return false
}

// AddressesAsPrefixes converts a list of IP addresses to a list of prefixes.
// Each address is converted to a prefix with a mask length equal to its bit length.
//
// Examples:
//   - 192.168.1.2 becomes 192.168.1.2/32
//   - 2001:db8::1 becomes 2001:db8::1/128
func AddressesAsPrefixes(addresses []netip.Addr) []netip.Prefix {
	var rv []netip.Prefix
	for _, addr := range addresses {
		rv = append(rv, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return rv
}

// ParsePrefix parses a CIDR string and returns a Prefix.
func ParsePrefix(v string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(v)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid CIDR `%s`: %w", v, err)
	}
	masked := prefix.Masked()
	if prefix.Addr().Compare(masked.Addr()) != 0 {
		return netip.Prefix{}, fmt.Errorf("invalid CIDR `%s`: not a valid network prefix, should be properly masked like %s", v, masked)
	}
	return prefix, nil
}

// GroupPrefixesByFamily groups prefixes by IP family.
func GroupPrefixesByFamily(vs []netip.Prefix) ([]netip.Prefix, []netip.Prefix) {
	var (
		v4 []netip.Prefix
		v6 []netip.Prefix
	)
	for _, v := range vs {
		if v.Addr().Is4() {
			v4 = append(v4, v)
		} else {
			v6 = append(v6, v)
		}
	}
	return v4, v6
}

// AggregatePrefixes aggregates prefixes.
// Overlapping prefixes are merged.
func AggregatePrefixes(prefixes []netip.Prefix) []netip.Prefix {
	var (
		v4, v6 = GroupPrefixesByFamily(prefixes)
		v4Tree = newPrefixTreeForIPv4()
		v6Tree = newPrefixTreeForIPv6()
	)

	for _, p := range v4 {
		v4Tree.Add(p)
	}
	for _, p := range v6 {
		v6Tree.Add(p)
	}

	return append(v4Tree.List(), v6Tree.List()...)
}

// ExcludePrefixes excludes prefixes from the given prefixes.
func ExcludePrefixes(prefixes []netip.Prefix, exclude []netip.Prefix) []netip.Prefix {
	var (
		v4Tree = newPrefixTreeForIPv4()
		v6Tree = newPrefixTreeForIPv6()
	)

	// Build the prefix tree for the prefixes.
	{
		v4, v6 := GroupPrefixesByFamily(prefixes)
		for _, p := range v4 {
			v4Tree.Add(p)
		}
		for _, p := range v6 {
			v6Tree.Add(p)
		}
	}

	// Exclude the prefixes.
	v4, v6 := GroupPrefixesByFamily(exclude)
	for _, p := range v4 {
		v4Tree.Remove(p)
	}
	for _, p := range v6 {
		v6Tree.Remove(p)
	}

	// Return the remaining prefixes.
	return append(v4Tree.List(), v6Tree.List()...)
}
