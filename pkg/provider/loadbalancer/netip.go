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

package loadbalancer

import (
	"fmt"
	"net/netip"
)

const (
	IPv4AllowedAll = "0.0.0.0/0"
	IPv6AllowedAll = "::/0"
)

// IsCIDRsAllowAll return true if the given IP Ranges covers all IPs.
// It returns false if the given IP Ranges is empty.
func IsCIDRsAllowAll(cidrs []netip.Prefix) bool {
	for _, cidr := range cidrs {
		if cidr.String() == IPv4AllowedAll || cidr.String() == IPv6AllowedAll {
			return true
		}
	}
	return false
}

func ParseCIDRs(parts []string) ([]netip.Prefix, error) {
	var rv []netip.Prefix
	for _, part := range parts {
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("invalid IP range %s: %w", part, err)
		}
		rv = append(rv, prefix)
	}
	return rv, nil
}
