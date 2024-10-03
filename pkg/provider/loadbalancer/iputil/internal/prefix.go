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
