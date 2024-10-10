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

package securitygroup

import (
	"net/netip"
	"sort"

	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/fnutil"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/iputil"
)

type AddressPrefixes struct {
	ipPrefixes      []netip.Prefix
	serviceTagIndex map[string]struct{}
}

// NewAddressPrefixes creates a new AddressPrefixes instance from a slice of strings.
// It is designed to be used with networkSecurityGroup.SourceAddressPrefixes or
// networkSecurityGroup.DestinationAddressPrefixes, which are of type []string and may contain
// both IP addresses/ranges and Azure service tags.
func NewAddressPrefixes(s []string) *AddressPrefixes {
	ipPrefixes, serviceTags := SeparateIPsAndServiceTags(s)
	rv := &AddressPrefixes{
		ipPrefixes:      ipPrefixes,
		serviceTagIndex: fnutil.SliceToSet(serviceTags),
	}
	rv.tidyIPPrefixes()
	return rv
}

// tidyIPPrefixes aggregates and sorts the IP prefixes in the AddressPrefixes instance.
func (ap *AddressPrefixes) tidyIPPrefixes() {
	ap.ipPrefixes = iputil.AggregatePrefixes(ap.ipPrefixes)
}

// AddIPAddresses adds one or more IP addresses to the AddressPrefixes instance.
func (ap *AddressPrefixes) AddIPAddresses(addresses ...netip.Addr) {
	for _, addr := range addresses {
		ap.ipPrefixes = append(ap.ipPrefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	ap.tidyIPPrefixes()
}

// RemoveIPAddresses removes one or more IP addresses from the AddressPrefixes instance.
func (ap *AddressPrefixes) RemoveIPAddresses(addresses ...netip.Addr) {
	ap.RemoveIPPrefixes(iputil.AddressesAsPrefixes(addresses)...)
}

// RemoveIPPrefixes removes one or more IP prefixes from the AddressPrefixes instance.
func (ap *AddressPrefixes) RemoveIPPrefixes(prefixes ...netip.Prefix) {
	ap.ipPrefixes = iputil.ExcludePrefixes(ap.ipPrefixes, prefixes)
	// No need to tidyIPPrefixes here, as ExcludePrefixes already does that.
}

// AddIPPrefixes adds one or more IP prefixes to the AddressPrefixes instance.
func (ap *AddressPrefixes) AddIPPrefixes(prefixes ...netip.Prefix) {
	ap.ipPrefixes = append(ap.ipPrefixes, prefixes...)
	ap.tidyIPPrefixes()
}

// AddServiceTags adds one or more service tags to the AddressPrefixes instance.
func (ap *AddressPrefixes) AddServiceTags(tags ...string) {
	for _, tag := range tags {
		ap.serviceTagIndex[tag] = struct{}{}
	}
}

// RemoveServiceTags removes one or more service tags from the AddressPrefixes instance.
func (ap *AddressPrefixes) RemoveServiceTags(tags ...string) {
	for _, tag := range tags {
		delete(ap.serviceTagIndex, tag)
	}
}

// StringSlice returns a slice of strings representing all IP addresses, prefixes, and service tags.
func (ap *AddressPrefixes) StringSlice() []string {
	var rv []string

	for _, ip := range ap.ipPrefixes {
		if ip.Bits() == ip.Addr().BitLen() {
			// Prefer IP address over IP range if possible.
			rv = append(rv, ip.Addr().String())
		} else {
			rv = append(rv, ip.String())
		}
	}
	sort.Slice(ap.ipPrefixes, func(i, j int) bool {
		return ap.ipPrefixes[i].String() < ap.ipPrefixes[j].String()
	})

	return append(rv, fnutil.SetToSlice(ap.serviceTagIndex)...)
}

// SeparateIPsAndServiceTags divides a list of prefixes into IP addresses/ranges and Azure service tags.
//
// The input prefixes can be sourced from networkSecurityGroup.SourceAddressPrefixes or
// networkSecurityGroup.DestinationAddressPrefixes, which are of type []string and may contain
// both IP addresses/ranges and Azure service tags.
//
// Returns:
//   - []netip.Prefix: A slice of IP addresses and ranges parsed as netip.Prefix
//   - []string: A slice of Azure service tags
func SeparateIPsAndServiceTags(prefixes []string) ([]netip.Prefix, []string) {
	var (
		ips         []netip.Prefix
		serviceTags []string
	)

	for _, p := range prefixes {
		if addr, err := netip.ParseAddr(p); err == nil {
			ips = append(ips, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		if prefix, err := netip.ParsePrefix(p); err == nil {
			ips = append(ips, prefix)
			continue
		}
		serviceTags = append(serviceTags, p)
	}

	return ips, serviceTags
}
