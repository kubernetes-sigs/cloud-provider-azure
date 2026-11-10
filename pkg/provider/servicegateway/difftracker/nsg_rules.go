/*
Copyright 2026 The Kubernetes Authors.

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

package difftracker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/securitygroup"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/iputil"
)

type nsgService struct {
	addresses []netip.Addr
	sources   []netip.Prefix
	ports     []PortMapping
}

type nsgBackend struct {
	address  netip.Addr
	protocol armnetwork.SecurityRuleProtocol
	port     int32
}

func parseNSGSourceRanges(value string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for index, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("source entry %d is empty", index+1)
		}
		if address, err := netip.ParseAddr(part); err == nil && address.Zone() == "" {
			address = address.Unmap()
			prefixes = append(prefixes, netip.PrefixFrom(address, address.BitLen()))
			continue
		}
		prefix, err := iputil.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("source entry %d: %w", index+1, err)
		}
		if prefix.Addr().Is4In6() {
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		prefixes = append(prefixes, prefix)
	}
	return iputil.AggregatePrefixes(prefixes), nil
}

func nsgIdentity(value string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(value)))
	return hex.EncodeToString(sum[:])
}

func nsgPolicyEnabled(config *InboundConfig) bool {
	return config != nil && config.AllowedIPRanges != nil
}

func nsgPolicyRestricts(sources []netip.Prefix, address netip.Addr) bool {
	if sources == nil {
		return false
	}
	for _, prefix := range sources {
		if prefix.Addr().Is4() == address.Is4() && prefix.Bits() == 0 {
			return false
		}
	}
	return true
}

func nsgPolicyAllowsUnrestricted(observed map[string]nsgService, requested nsgService) bool {
	if len(requested.addresses) == 0 {
		return false
	}
	if _, err := nsgBackendPorts(requested.ports); err != nil {
		return false
	}
	for _, address := range requested.addresses {
		if nsgPolicyRestricts(requested.sources, address) {
			return false
		}
		for _, current := range observed {
			if !nsgPolicyRestricts(current.sources, address) || !slices.Contains(current.addresses, address) {
				continue
			}
			for _, port := range requested.ports {
				if slices.Contains(current.ports, port) {
					return false
				}
			}
		}
	}
	return true
}

func nsgBackendPorts(mappings []PortMapping) (map[armnetwork.SecurityRuleProtocol][]int32, error) {
	result := make(map[armnetwork.SecurityRuleProtocol][]int32)
	if len(mappings) == 0 {
		return nil, fmt.Errorf("inbound backend ports are unavailable")
	}
	for _, mapping := range mappings {
		protocol, err := securitygroup.ProtocolFromKubernetes(v1.Protocol(mapping.Protocol))
		if err != nil {
			return nil, err
		}
		if protocol != armnetwork.SecurityRuleProtocolTCP && protocol != armnetwork.SecurityRuleProtocolUDP {
			return nil, fmt.Errorf("source filtering does not support protocol %q", mapping.Protocol)
		}
		port := mapping.Port
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("backend port %d is invalid", port)
		}
		result[protocol] = append(result[protocol], port)
	}
	for protocol, ports := range result {
		slices.Sort(ports)
		result[protocol] = slices.Compact(ports)
	}
	return result, nil
}

func nsgPortMappings(config *InboundConfig) []PortMapping {
	if config == nil {
		return nil
	}
	result := make([]PortMapping, 0, len(config.FrontendPorts))
	for index, frontend := range config.FrontendPorts {
		mapping := PortMapping{Port: frontend.Port, Protocol: strings.ToUpper(frontend.Protocol)}
		if index < len(config.BackendPorts) {
			mapping.Port = config.BackendPorts[index].Port
		}
		if !slices.Contains(result, mapping) {
			result = append(result, mapping)
		}
	}
	return result
}

func loadBalancerBackendPorts(lb *armnetwork.LoadBalancer) ([]PortMapping, error) {
	if lb == nil || lb.Properties == nil {
		return nil, fmt.Errorf("load balancer has no properties")
	}
	result := make([]PortMapping, 0, len(lb.Properties.LoadBalancingRules))
	for _, rule := range lb.Properties.LoadBalancingRules {
		if rule == nil || rule.Properties == nil || rule.Properties.BackendPort == nil || rule.Properties.Protocol == nil {
			return nil, fmt.Errorf("load balancer has an incomplete backend rule")
		}
		port := PortMapping{Port: *rule.Properties.BackendPort, Protocol: strings.ToUpper(string(*rule.Properties.Protocol))}
		if !slices.Contains(result, port) {
			result = append(result, port)
		}
	}
	if len(result) > 0 {
		if _, err := nsgBackendPorts(result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func nsgSourcesForFamily(sources []netip.Prefix, address netip.Addr) []netip.Prefix {
	var result []netip.Prefix
	for _, prefix := range sources {
		if prefix.Addr().Is4() == address.Is4() {
			result = append(result, prefix)
		}
	}
	return result
}

func nsgPolicyCovers(applied nsgService, sources []netip.Prefix, ports []PortMapping, ip netip.Addr) bool {
	if len(ports) == 0 || !slices.Contains(applied.addresses, ip) ||
		(sources == nil) != (applied.sources == nil) ||
		!slices.Equal(nsgSourcesForFamily(sources, ip), nsgSourcesForFamily(applied.sources, ip)) {
		return false
	}
	for _, port := range ports {
		if !slices.Contains(applied.ports, port) {
			return false
		}
	}
	return true
}

func nsgPoliciesEqual(a, b *InboundConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return (a.AllowedIPRanges == nil) == (b.AllowedIPRanges == nil) &&
		slices.Equal(a.AllowedIPRanges, b.AllowedIPRanges) &&
		slices.Equal(nsgPortMappings(a), nsgPortMappings(b))
}

func nsgRuleServiceUID(rule *armnetwork.SecurityRule) string {
	if rule == nil || rule.Name == nil || rule.Properties == nil || rule.Properties.Description == nil ||
		!strings.HasPrefix(strings.ToLower(*rule.Name), consts.ServiceGatewayNSGRuleNamePrefix) ||
		!strings.HasPrefix(*rule.Properties.Description, consts.ServiceGatewayNSGServiceDescriptionPrefix) {
		return ""
	}
	return strings.TrimPrefix(*rule.Properties.Description, consts.ServiceGatewayNSGServiceDescriptionPrefix)
}

func isNSGOwnedRule(rule *armnetwork.SecurityRule) bool {
	if nsgRuleServiceUID(rule) != "" {
		return true
	}
	if rule == nil || rule.Properties == nil ||
		ptr.Deref(rule.Properties.Description, "") != consts.ServiceGatewayNSGAllowOtherDescription {
		return false
	}
	name := ptr.Deref(rule.Name, "")
	return strings.EqualFold(name, consts.ServiceGatewayNSGAllowInboundRuleName) ||
		strings.EqualFold(name, consts.ServiceGatewayNSGAllowOutboundRuleName)
}

func isNRMSRule(name string, priority int32) bool {
	return priority >= consts.ServiceGatewayNSGNRMSMinimumPriority &&
		priority < consts.LoadBalancerMinimumPriority &&
		strings.EqualFold(name, fmt.Sprintf("%s%d", consts.ServiceGatewayNSGNRMSRuleNamePrefix, priority))
}

func buildNICSecurityRules(services map[string]nsgService, existing []*armnetwork.SecurityRule) ([]*armnetwork.SecurityRule, error) {
	var rules []*armnetwork.SecurityRule
	for _, rule := range existing {
		if rule == nil || rule.Name == nil || rule.Properties == nil || rule.Properties.Direction == nil ||
			rule.Properties.Priority == nil || rule.Properties.Access == nil || rule.Properties.Protocol == nil {
			return nil, fmt.Errorf("existing NSG contains an incomplete rule")
		}
		if isNSGOwnedRule(rule) {
			continue
		}
		if !isNRMSRule(*rule.Name, *rule.Properties.Priority) {
			return nil, fmt.Errorf("managed NIC NSG contains an unexpected custom rule %q", *rule.Name)
		}
		rules = append(rules, rule)
	}
	allowPriority := int32(consts.LoadBalancerMinimumPriority)
	denyPriority := int32(consts.LoadBalancerMaximumPriority) - 1
	for _, direction := range []armnetwork.SecurityRuleDirection{armnetwork.SecurityRuleDirectionInbound, armnetwork.SecurityRuleDirectionOutbound} {
		name := consts.ServiceGatewayNSGAllowInboundRuleName
		if direction == armnetwork.SecurityRuleDirectionOutbound {
			name = consts.ServiceGatewayNSGAllowOutboundRuleName
		}
		rules = append(rules, &armnetwork.SecurityRule{
			Name: ptr.To(name),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Description:              ptr.To(consts.ServiceGatewayNSGAllowOtherDescription),
				Direction:                ptr.To(direction),
				Access:                   ptr.To(armnetwork.SecurityRuleAccessAllow),
				Protocol:                 ptr.To(armnetwork.SecurityRuleProtocolAsterisk),
				Priority:                 ptr.To[int32](consts.LoadBalancerMaximumPriority),
				SourceAddressPrefix:      ptr.To("*"),
				DestinationAddressPrefix: ptr.To("*"),
				SourcePortRange:          ptr.To("*"),
				DestinationPortRange:     ptr.To("*"),
			},
		})
	}

	unknown := make(map[netip.Addr]string)
	for _, uid := range slices.Sorted(maps.Keys(services)) {
		service := services[uid]
		if service.ports == nil {
			for _, address := range service.addresses {
				unknown[address.Unmap()] = uid
			}
		}
	}
	owners := make(map[nsgBackend]string)
	for _, uid := range slices.Sorted(maps.Keys(services)) {
		service := services[uid]
		if service.ports == nil {
			continue
		}
		ports, err := nsgBackendPorts(service.ports)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", uid, err)
		}
		for _, address := range service.addresses {
			if !address.IsValid() || address.Zone() != "" {
				return nil, fmt.Errorf("service %q has an invalid backend address", uid)
			}
			if other, ok := unknown[address.Unmap()]; ok && nsgPolicyRestricts(service.sources, address.Unmap()) {
				return nil, &InboundConfigValidationError{
					Reason:  consts.ServiceGatewaySourceRangeConflictReason,
					Message: fmt.Sprintf("service %q shares backend %s with service %q whose backend ports are unavailable", uid, address, other),
				}
			}
			for protocol, protocolPorts := range ports {
				for _, port := range protocolPorts {
					key := nsgBackend{address.Unmap(), protocol, port}
					if other, ok := owners[key]; ok && other != uid &&
						(nsgPolicyRestricts(service.sources, key.address) || nsgPolicyRestricts(services[other].sources, key.address)) {
						return nil, &InboundConfigValidationError{
							Reason: consts.ServiceGatewaySourceRangeConflictReason,
							Message: fmt.Sprintf("services %q and %q share backend %s:%d/%s with a source restriction",
								other, uid, key.address, port, protocol),
						}
					}
					owners[key] = uid
				}
			}
		}
		if service.sources == nil {
			continue
		}
		for _, ipv6 := range []bool{false, true} {
			var destinations []string
			for _, address := range service.addresses {
				address = address.Unmap()
				if address.Is6() == ipv6 {
					destinations = append(destinations, address.String())
				}
			}
			if len(destinations) == 0 {
				continue
			}
			var sources []string
			for _, prefix := range service.sources {
				if !prefix.IsValid() || prefix != prefix.Masked() {
					return nil, fmt.Errorf("service %q has an invalid source prefix", uid)
				}
				if prefix.Addr().Is6() == ipv6 {
					sources = append(sources, prefix.String())
				}
			}
			for _, protocol := range slices.Sorted(maps.Keys(ports)) {
				for _, access := range []armnetwork.SecurityRuleAccess{armnetwork.SecurityRuleAccessAllow, armnetwork.SecurityRuleAccessDeny} {
					if access == armnetwork.SecurityRuleAccessAllow && len(sources) == 0 {
						continue
					}
					family := v1.IPv4Protocol
					if ipv6 {
						family = v1.IPv6Protocol
					}
					name := fmt.Sprintf("%s%s-%s-%s-%s", consts.ServiceGatewayNSGRuleNamePrefix,
						nsgIdentity(uid)[:32], strings.ToLower(string(family)), strings.ToLower(string(protocol)), strings.ToLower(string(access)))
					if allowPriority > denyPriority {
						return nil, securitygroup.ErrSecurityRulePriorityExhausted
					}
					priority := allowPriority
					if access == armnetwork.SecurityRuleAccessDeny {
						priority = denyPriority
						denyPriority--
					} else {
						allowPriority++
					}
					rule := &armnetwork.SecurityRule{
						Name: ptr.To(name),
						Properties: &armnetwork.SecurityRulePropertiesFormat{
							Description:     ptr.To(consts.ServiceGatewayNSGServiceDescriptionPrefix + uid),
							Direction:       ptr.To(armnetwork.SecurityRuleDirectionInbound),
							Access:          ptr.To(access),
							Protocol:        ptr.To(protocol),
							Priority:        ptr.To(priority),
							SourcePortRange: ptr.To("*"),
						},
					}
					if access == armnetwork.SecurityRuleAccessDeny {
						rule.Properties.SourceAddressPrefix = ptr.To("*")
					} else {
						rule.Properties.SourceAddressPrefixes = to.SliceOfPtrs(sources...)
					}
					securitygroup.SetDestinationPrefixes(rule, destinations)
					securitygroup.SetDestinationPortRanges(rule, ports[protocol])
					rules = append(rules, rule)
				}
			}
		}
	}
	nRules, nSources, nDestinations := len(rules), 0, 0
	for _, rule := range rules {
		p := rule.Properties
		nSources += len(p.SourceAddressPrefixes)
		nDestinations += len(p.DestinationAddressPrefixes)
		if p.SourceAddressPrefix != nil {
			nSources++
		}
		if p.DestinationAddressPrefix != nil {
			nDestinations++
		}
	}
	if nRules > consts.ServiceGatewayNSGMaxRules || nSources > consts.ServiceGatewayNSGMaxSourcePrefixes ||
		nDestinations > consts.ServiceGatewayNSGMaxDestinationPrefixes {
		return nil, fmt.Errorf("NIC NSG capacity exceeded: rules=%d/%d, sources=%d/%d, destinations=%d/%d",
			nRules, consts.ServiceGatewayNSGMaxRules, nSources, consts.ServiceGatewayNSGMaxSourcePrefixes,
			nDestinations, consts.ServiceGatewayNSGMaxDestinationPrefixes)
	}
	slices.SortFunc(rules, func(a, b *armnetwork.SecurityRule) int {
		if value := strings.Compare(string(*a.Properties.Direction), string(*b.Properties.Direction)); value != 0 {
			return value
		}
		return int(*a.Properties.Priority - *b.Properties.Priority)
	})
	return rules, nil
}

func decodeNSGServices(rules []*armnetwork.SecurityRule) (map[string]nsgService, error) {
	result := make(map[string]nsgService)
	for _, rule := range rules {
		uid := strings.ToLower(nsgRuleServiceUID(rule))
		if uid == "" {
			continue
		}
		p := rule.Properties
		if p.Direction == nil || *p.Direction != armnetwork.SecurityRuleDirectionInbound || p.Protocol == nil || p.Access == nil {
			return nil, fmt.Errorf("managed NSG rule %q has incomplete packet fields", ptr.Deref(rule.Name, ""))
		}
		if *p.Protocol != armnetwork.SecurityRuleProtocolTCP && *p.Protocol != armnetwork.SecurityRuleProtocolUDP {
			return nil, fmt.Errorf("managed NSG rule %q has an unsupported protocol", *rule.Name)
		}
		service, exists := result[uid]
		if !exists {
			service.sources = []netip.Prefix{}
		}
		if *p.Access == armnetwork.SecurityRuleAccessAllow {
			for _, source := range securitygroup.ListSourcePrefixes(rule) {
				prefixes, err := parseNSGSourceRanges(source)
				if err != nil {
					return nil, fmt.Errorf("read source policy for service %q: %w", uid, err)
				}
				service.sources = append(service.sources, prefixes...)
			}
		} else if *p.Access != armnetwork.SecurityRuleAccessDeny {
			return nil, fmt.Errorf("managed NSG rule %q has an invalid action", *rule.Name)
		}
		ports, err := securitygroup.ListDestinationPortRanges(rule)
		if err != nil {
			return nil, fmt.Errorf("read backend ports for service %q: %w", uid, err)
		}
		if len(ports) == 0 {
			return nil, fmt.Errorf("managed NSG rule %q has no backend ports", *rule.Name)
		}
		for _, port := range ports {
			mapping := PortMapping{Port: port, Protocol: strings.ToUpper(string(*p.Protocol))}
			if !slices.Contains(service.ports, mapping) {
				service.ports = append(service.ports, mapping)
			}
		}
		for _, destination := range securitygroup.ListDestinationPrefixes(rule) {
			address, err := netip.ParseAddr(destination)
			if err != nil {
				prefix, parseErr := netip.ParsePrefix(destination)
				if parseErr != nil || prefix.Bits() != prefix.Addr().BitLen() {
					return nil, fmt.Errorf("managed NSG rule %q has a non-host destination", *rule.Name)
				}
				address = prefix.Addr()
			}
			if !slices.Contains(service.addresses, address.Unmap()) {
				service.addresses = append(service.addresses, address.Unmap())
			}
		}
		result[uid] = service
	}
	for uid, service := range result {
		service.sources = iputil.AggregatePrefixes(service.sources)
		if service.sources == nil {
			service.sources = []netip.Prefix{}
		}
		slices.SortFunc(service.addresses, netip.Addr.Compare)
		result[uid] = service
	}
	return result, nil
}
