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
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/securitygroup"
)

func TestParseNSGSourceRanges(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "IPv4 host", value: "20.1.2.3", want: []string{"20.1.2.3/32"}},
		{name: "IPv6 host", value: "2001:DB8::1", want: []string{"2001:db8::1/128"}},
		{name: "mapped host", value: "::ffff:20.1.2.3", want: []string{"20.1.2.3/32"}},
		{name: "mapped prefix", value: "::ffff:20.0.0.0/104", want: []string{"20.0.0.0/8"}},
		{name: "deduplicated and aggregated", value: " 20.1.2.3,20.0.0.0/8,20.0.0.0/8 ", want: []string{"20.0.0.0/8"}},
		{name: "adjacent hosts", value: "20.1.2.0,20.1.2.1", want: []string{"20.1.2.0/31"}},
		{name: "both families", value: "2001:DB8::/32,20.0.0.0/8", want: []string{"20.0.0.0/8", "2001:db8::/32"}},
		{name: "both unrestricted", value: "::/0,0.0.0.0/0", want: []string{"0.0.0.0/0", "::/0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNSGSourceRanges(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			actual := make([]string, len(got))
			for index, prefix := range got {
				actual[index] = prefix.String()
			}
			assert.Equal(t, tt.want, actual)
		})
	}
	for _, value := range []string{"", " ", ",", "20.0.0.0/8,", "20.1.2.3/8", "2001:db8::1/32", "fe80::1%eth0", "bad", "20.0.0.0/8,bad"} {
		t.Run(value, func(t *testing.T) {
			got, err := parseNSGSourceRanges(value)
			assert.Error(t, err)
			assert.Nil(t, got)
		})
	}
}

func nsgTestService(port int32, protocol string, sources string, addresses ...string) nsgService {
	service := nsgService{ports: []PortMapping{{Port: port, Protocol: strings.ToUpper(protocol)}}}
	if sources != "" {
		var err error
		service.sources, err = parseNSGSourceRanges(sources)
		if err != nil {
			panic(err)
		}
	}
	for _, address := range addresses {
		service.addresses = append(service.addresses, netip.MustParseAddr(address))
	}
	return service
}

func nsgTestNRMSRule(priority int32) *armnetwork.SecurityRule {
	return &armnetwork.SecurityRule{
		Name: ptr.To(fmt.Sprintf("NRMS-Rule-%d", priority)),
		Properties: &armnetwork.SecurityRulePropertiesFormat{
			Description:              ptr.To("Operational network policy"),
			Direction:                ptr.To(armnetwork.SecurityRuleDirectionInbound),
			Access:                   ptr.To(armnetwork.SecurityRuleAccessAllow),
			Protocol:                 ptr.To(armnetwork.SecurityRuleProtocolAsterisk),
			Priority:                 ptr.To(priority),
			SourceAddressPrefix:      ptr.To("VirtualNetwork"),
			SourcePortRange:          ptr.To("*"),
			DestinationAddressPrefix: ptr.To("*"),
			DestinationPortRange:     ptr.To("*"),
		},
	}
}

func nsgTestAccess(t *testing.T, rules []*armnetwork.SecurityRule, source, destination string, port int32, protocol armnetwork.SecurityRuleProtocol) armnetwork.SecurityRuleAccess {
	t.Helper()
	matches := func(prefixes []string, address netip.Addr) bool {
		for _, prefix := range prefixes {
			if prefix == "*" {
				return true
			}
			if parsed, err := netip.ParsePrefix(prefix); err == nil && parsed.Contains(address) {
				return true
			}
			if parsed, err := netip.ParseAddr(prefix); err == nil && parsed == address {
				return true
			}
		}
		return false
	}
	for _, rule := range rules {
		p := rule.Properties
		if *p.Direction != armnetwork.SecurityRuleDirectionInbound ||
			(*p.Protocol != protocol && *p.Protocol != armnetwork.SecurityRuleProtocolAsterisk) {
			continue
		}
		if !matches(securitygroup.ListSourcePrefixes(rule), netip.MustParseAddr(source)) ||
			!matches(securitygroup.ListDestinationPrefixes(rule), netip.MustParseAddr(destination)) {
			continue
		}
		if ptr.Deref(p.DestinationPortRange, "") != "*" {
			ports, err := securitygroup.ListDestinationPortRanges(rule)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(ports, port) {
				continue
			}
		}
		return *p.Access
	}
	t.Fatal("no rule matched")
	return ""
}

func BenchmarkBuildNICSecurityRules(b *testing.B) {
	for _, count := range []int{100, 1000, 2000} {
		b.Run(fmt.Sprintf("addresses=%d", count), func(b *testing.B) {
			service := nsgTestService(8080, "TCP", "20.0.0.0/8")
			for index := 0; index < count; index++ {
				service.addresses = append(service.addresses, netip.AddrFrom4([4]byte{10, 224, byte(index / 256), byte(index)}))
			}
			services := map[string]nsgService{"svc": service}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				rules, err := buildNICSecurityRules(services, nil)
				if err != nil || len(rules) != 4 {
					b.Fatalf("unexpected rules: count=%d, err=%v", len(rules), err)
				}
			}
		})
	}
}

func TestLoadBalancerBackendPorts(t *testing.T) {
	valid := &armnetwork.LoadBalancingRule{Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
		Protocol: ptr.To(armnetwork.TransportProtocolTCP), BackendPort: ptr.To[int32](8080),
	}}
	for _, tt := range []struct {
		name string
		lb   *armnetwork.LoadBalancer
		want []PortMapping
		err  bool
	}{
		{name: "missing resource", err: true},
		{name: "missing properties", lb: &armnetwork.LoadBalancer{}, err: true},
		{name: "no active rules", lb: &armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{}}, want: []PortMapping{}},
		{name: "deduplicated observed backend", lb: &armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{LoadBalancingRules: []*armnetwork.LoadBalancingRule{valid, valid}}}, want: []PortMapping{{Port: 8080, Protocol: "TCP"}}},
		{name: "missing rule", lb: &armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{LoadBalancingRules: []*armnetwork.LoadBalancingRule{nil}}}, err: true},
		{name: "incomplete rule", lb: &armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{LoadBalancingRules: []*armnetwork.LoadBalancingRule{{Properties: &armnetwork.LoadBalancingRulePropertiesFormat{Protocol: ptr.To(armnetwork.TransportProtocolTCP)}}}}}, err: true},
		{name: "invalid backend port", lb: &armnetwork.LoadBalancer{Properties: &armnetwork.LoadBalancerPropertiesFormat{LoadBalancingRules: []*armnetwork.LoadBalancingRule{{Properties: &armnetwork.LoadBalancingRulePropertiesFormat{Protocol: ptr.To(armnetwork.TransportProtocolTCP), BackendPort: ptr.To[int32](0)}}}}}, err: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ports, err := loadBalancerBackendPorts(tt.lb)
			if tt.err {
				assert.Error(t, err)
				assert.Nil(t, ports)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, ports)
			}
		})
	}
}

func TestNSGPolicyAllowsUnrestricted(t *testing.T) {
	for _, tt := range []struct {
		name      string
		observed  nsgService
		requested nsgService
		want      bool
	}{
		{"unrestricted", nsgService{}, nsgTestService(9090, "TCP", "", "10.1.0.1"), true},
		{"restricted tuple", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.1.0.1"), nsgTestService(8080, "TCP", "", "10.1.0.1"), false},
		{"different port", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.1.0.1"), nsgTestService(9090, "TCP", "", "10.1.0.1"), true},
		{"different protocol", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.1.0.1"), nsgTestService(8080, "UDP", "", "10.1.0.1"), true},
		{"different address", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.1.0.1"), nsgTestService(8080, "TCP", "", "10.1.0.2"), true},
		{"one of several addresses conflicts", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.1.0.1"), nsgTestService(8080, "TCP", "", "10.1.0.2", "10.1.0.1"), false},
		{"deny-only family", nsgTestService(8080, "TCP", "20.0.0.0/8", "fd00::1"), nsgTestService(8080, "TCP", "", "fd00::1"), false},
		{"observed allow-all", nsgTestService(8080, "TCP", "0.0.0.0/0", "10.1.0.1"), nsgTestService(8080, "TCP", "", "10.1.0.1"), true},
		{"requested restriction", nsgService{}, nsgTestService(8080, "TCP", "20.0.0.0/8", "10.1.0.1"), false},
		{"requested allow-all", nsgService{}, nsgTestService(8080, "TCP", "0.0.0.0/0", "10.1.0.1"), true},
		{"requested family remains denied", nsgService{}, nsgTestService(8080, "TCP", "0.0.0.0/0", "fd00::1"), false},
		{"no addresses", nsgService{}, nsgTestService(8080, "TCP", ""), false},
		{"invalid port", nsgService{}, nsgTestService(0, "TCP", "", "10.1.0.1"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, nsgPolicyAllowsUnrestricted(map[string]nsgService{"svc": tt.observed}, tt.requested))
		})
	}
}

func TestNSGPolicyCovers(t *testing.T) {
	for _, tt := range []struct {
		name     string
		applied  nsgService
		sources  string
		address  string
		port     int32
		protocol string
		want     bool
	}{
		{"same policy", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.1.0.1"), "20.0.0.0/8", "10.1.0.1", 8080, "TCP", true},
		{"other address", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.1.0.2"), "20.0.0.0/8", "10.1.0.1", 8080, "TCP", false},
		{"other port", nsgTestService(9090, "TCP", "20.0.0.0/8", "10.1.0.1"), "20.0.0.0/8", "10.1.0.1", 8080, "TCP", false},
		{"other protocol", nsgTestService(8080, "UDP", "20.0.0.0/8", "10.1.0.1"), "20.0.0.0/8", "10.1.0.1", 8080, "TCP", false},
		{"deny-only is not unrestricted", nsgTestService(8080, "TCP", "20.0.0.0/8", "fd00::1"), "", "fd00::1", 8080, "TCP", false},
		{"unrestricted is not deny-only", nsgTestService(8080, "TCP", "", "fd00::1"), "20.0.0.0/8", "fd00::1", 8080, "TCP", false},
		{"same deny-only family", nsgTestService(8080, "TCP", "20.0.0.0/8", "fd00::1"), "20.0.0.0/8", "fd00::1", 8080, "TCP", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := &InboundConfig{
				FrontendPorts: []PortMapping{{Port: 80, Protocol: tt.protocol}},
				BackendPorts:  []PortMapping{{Port: tt.port, Protocol: tt.protocol}},
			}
			if tt.sources != "" {
				var err error
				config.AllowedIPRanges, err = parseNSGSourceRanges(tt.sources)
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.want, nsgPolicyCovers(tt.applied, config.AllowedIPRanges, nsgPortMappings(config), netip.MustParseAddr(tt.address)))
		})
	}
}

func TestBuildNICSecurityRules(t *testing.T) {
	services := map[string]nsgService{
		"restricted":   nsgTestService(8080, "TCP", "20.0.0.0/8,2001:db8::/32", "10.224.1.10", "fd00:1::10"),
		"unrestricted": nsgTestService(8080, "TCP", "", "10.224.1.11"),
	}
	rules, err := buildNICSecurityRules(services, nil)
	if err != nil {
		t.Fatal(err)
	}
	assert.Len(t, rules, 6)
	priorities := make(map[string]bool)
	for _, rule := range rules {
		p := rule.Properties
		key := string(*p.Direction) + "/" + strconv.Itoa(int(*p.Priority))
		assert.NotContains(t, priorities, key)
		priorities[key] = true
		assert.GreaterOrEqual(t, *p.Priority, int32(consts.LoadBalancerMinimumPriority))
		assert.LessOrEqual(t, *p.Priority, int32(consts.LoadBalancerMaximumPriority))
	}
	for _, tt := range []struct {
		name        string
		source      string
		destination string
		port        int32
		protocol    armnetwork.SecurityRuleProtocol
		want        armnetwork.SecurityRuleAccess
	}{
		{"allowed IPv4", "20.1.2.3", "10.224.1.10", 8080, armnetwork.SecurityRuleProtocolTCP, armnetwork.SecurityRuleAccessAllow},
		{"denied IPv4", "40.1.2.3", "10.224.1.10", 8080, armnetwork.SecurityRuleProtocolTCP, armnetwork.SecurityRuleAccessDeny},
		{"direct traffic has no exemption", "10.224.1.20", "10.224.1.10", 8080, armnetwork.SecurityRuleProtocolTCP, armnetwork.SecurityRuleAccessDeny},
		{"other port", "40.1.2.3", "10.224.1.10", 9090, armnetwork.SecurityRuleProtocolTCP, armnetwork.SecurityRuleAccessAllow},
		{"frontend port is not restricted", "40.1.2.3", "10.224.1.10", 80, armnetwork.SecurityRuleProtocolTCP, armnetwork.SecurityRuleAccessAllow},
		{"other protocol", "40.1.2.3", "10.224.1.10", 8080, armnetwork.SecurityRuleProtocolUDP, armnetwork.SecurityRuleAccessAllow},
		{"unrestricted service", "40.1.2.3", "10.224.1.11", 8080, armnetwork.SecurityRuleProtocolTCP, armnetwork.SecurityRuleAccessAllow},
		{"allowed IPv6", "2001:db8::1", "fd00:1::10", 8080, armnetwork.SecurityRuleProtocolTCP, armnetwork.SecurityRuleAccessAllow},
		{"denied IPv6", "2001:db9::1", "fd00:1::10", 8080, armnetwork.SecurityRuleProtocolTCP, armnetwork.SecurityRuleAccessDeny},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, nsgTestAccess(t, rules, tt.source, tt.destination, tt.port, tt.protocol))
		})
	}
	again, err := buildNICSecurityRules(services, rules)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, rules, again)

	v4Only := map[string]nsgService{"restricted": nsgTestService(8080, "TCP", "20.0.0.0/8", "fd00:1::10")}
	rules, err = buildNICSecurityRules(v4Only, nil)
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, armnetwork.SecurityRuleAccessDeny,
		nsgTestAccess(t, rules, "2001:db8::1", "fd00:1::10", 8080, armnetwork.SecurityRuleProtocolTCP))
}

func TestBuildNICSecurityRulesConflicts(t *testing.T) {
	for _, tt := range []struct {
		name      string
		first     nsgService
		second    nsgService
		wantError bool
	}{
		{"different policies", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.1"), nsgTestService(8080, "TCP", "40.0.0.0/8", "10.0.0.1"), true},
		{"identical restrictions are still shared", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.1"), nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.1"), true},
		{"restricted and unrestricted", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.1"), nsgTestService(8080, "TCP", "", "10.0.0.1"), true},
		{"unrestricted and restricted", nsgTestService(8080, "TCP", "", "10.0.0.1"), nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.1"), true},
		{"both unrestricted", nsgTestService(8080, "TCP", "", "10.0.0.1"), nsgTestService(8080, "TCP", "", "10.0.0.1"), false},
		{"allow all and unrestricted", nsgTestService(8080, "TCP", "0.0.0.0/0", "10.0.0.1"), nsgTestService(8080, "TCP", "", "10.0.0.1"), false},
		{"different ports", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.1"), nsgTestService(9090, "TCP", "", "10.0.0.1"), false},
		{"different protocols", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.1"), nsgTestService(8080, "UDP", "", "10.0.0.1"), false},
		{"different addresses", nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.1"), nsgTestService(8080, "TCP", "", "10.0.0.2"), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildNICSecurityRules(map[string]nsgService{"a": tt.first, "b": tt.second}, nil)
			if tt.wantError {
				var conflict *InboundConfigValidationError
				if !assert.ErrorAs(t, err, &conflict) {
					return
				}
				assert.Equal(t, consts.ServiceGatewaySourceRangeConflictReason, conflict.Reason)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestBuildNICSecurityRulesReplacesOwnedRules(t *testing.T) {
	existing, err := buildNICSecurityRules(map[string]nsgService{"old": nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.1")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := buildNICSecurityRules(map[string]nsgService{"new": nsgTestService(8080, "TCP", "40.0.0.0/8", "10.0.0.2")}, existing)
	if err != nil {
		t.Fatal(err)
	}
	if !assert.Len(t, rules, 4) {
		return
	}
	for _, rule := range rules {
		assert.NotEqual(t, "old", nsgRuleServiceUID(rule))
	}
	empty, err := buildNICSecurityRules(nil, rules)
	if err != nil {
		t.Fatal(err)
	}
	assert.Len(t, empty, 2)
}

func TestBuildNICSecurityRulesPreservesNRMS(t *testing.T) {
	operational := []*armnetwork.SecurityRule{nsgTestNRMSRule(103), nsgTestNRMSRule(107), nsgTestNRMSRule(109)}
	operational[1].Properties.Access = ptr.To(armnetwork.SecurityRuleAccessDeny)
	operational[1].Properties.Protocol = ptr.To(armnetwork.SecurityRuleProtocolTCP)
	operational[1].Properties.SourceAddressPrefix = ptr.To("Internet")
	operational[1].Properties.DestinationPortRange = nil
	operational[1].Properties.DestinationPortRanges = []*string{ptr.To("23"), ptr.To("135"), ptr.To("445")}
	operational[2].Properties.Direction = ptr.To(armnetwork.SecurityRuleDirectionOutbound)
	operational[2].Properties.Protocol = ptr.To(armnetwork.SecurityRuleProtocolUDP)
	operational[2].Properties.SourceAddressPrefix = nil
	operational[2].Properties.SourceAddressPrefixes = []*string{ptr.To("2001:db8:1::/48"), ptr.To("2001:db8:2::/48")}
	operational[2].Properties.SourcePortRange = nil
	operational[2].Properties.SourcePortRanges = []*string{ptr.To("443"), ptr.To("1024-2048")}

	for _, tt := range []struct {
		name     string
		services map[string]nsgService
	}{
		{"add", map[string]nsgService{"new": nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.2")}},
		{"source change", map[string]nsgService{"old": nsgTestService(8080, "TCP", "40.0.0.0/8", "10.0.0.1")}},
		{"port change", map[string]nsgService{"old": nsgTestService(9090, "TCP", "20.0.0.0/8", "10.0.0.1")}},
		{"annotation removal", map[string]nsgService{"old": nsgTestService(8080, "TCP", "", "10.0.0.1")}},
		{"service removal", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			existing, err := buildNICSecurityRules(map[string]nsgService{
				"old": nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.1"),
			}, nil)
			if !assert.NoError(t, err) {
				return
			}
			existing = append(existing, nsgCopy(operational)...)
			before := nsgCopy(existing)
			rules, err := buildNICSecurityRules(tt.services, existing)
			if !assert.NoError(t, err) {
				return
			}
			assert.Equal(t, before, existing, "building the next rules must not mutate the fetched resource")
			for _, expected := range operational {
				index := slices.IndexFunc(rules, func(rule *armnetwork.SecurityRule) bool {
					return ptr.Deref(rule.Name, "") == *expected.Name
				})
				if assert.NotEqual(t, -1, index) {
					assert.Equal(t, expected, rules[index])
					assert.False(t, isNSGOwnedRule(rules[index]), "preservation must not transfer rule ownership to CCM")
				}
			}
			owned, err := buildNICSecurityRules(tt.services, nil)
			assert.NoError(t, err)
			assert.Len(t, rules, len(owned)+len(operational))
			assert.True(t, nsgRulesEqual(append(owned, operational...), rules))
			again, err := buildNICSecurityRules(tt.services, rules)
			assert.NoError(t, err)
			assert.Equal(t, rules, again, "unchanged mixed rules must be stable")
		})
	}
}

func TestBuildNICSecurityRulesNRMSLayout(t *testing.T) {
	for _, tt := range []struct {
		name     string
		ruleName string
		priority int32
		valid    bool
	}{
		{"minimum", "NRMS-Rule-100", 100, true},
		{"observed", "NRMS-Rule-103", 103, true},
		{"last reserved priority", "NRMS-Rule-499", 499, true},
		{"name casing", "nrms-rule-103", 103, true},
		{"below Azure minimum", "NRMS-Rule-99", 99, false},
		{"negative priority", "NRMS-Rule--1", -1, false},
		{"CCM range", "NRMS-Rule-500", 500, false},
		{"allow-other collision", "NRMS-Rule-4096", 4096, false},
		{"name priority mismatch", "NRMS-Rule-103", 104, false},
		{"unknown custom rule", "platform-allow", 103, false},
		{"non-numeric suffix", "NRMS-Rule-custom", 103, false},
		{"extra suffix", "NRMS-Rule-103-custom", 103, false},
		{"leading zero", "NRMS-Rule-0103", 103, false},
		{"signed suffix", "NRMS-Rule-+103", 103, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rule := nsgTestNRMSRule(tt.priority)
			rule.Name = ptr.To(tt.ruleName)
			rules, err := buildNICSecurityRules(nil, []*armnetwork.SecurityRule{rule})
			if !tt.valid {
				assert.ErrorContains(t, err, "unexpected")
				assert.Nil(t, rules)
				return
			}
			if assert.NoError(t, err) {
				assert.Len(t, rules, 3)
				assert.Equal(t, rule, rules[0])
				assert.False(t, isNSGOwnedRule(rule))
			}
		})
	}
	for _, tt := range []struct {
		name   string
		change func(*armnetwork.SecurityRule)
	}{
		{"name", func(rule *armnetwork.SecurityRule) { rule.Name = nil }},
		{"properties", func(rule *armnetwork.SecurityRule) { rule.Properties = nil }},
		{"direction", func(rule *armnetwork.SecurityRule) { rule.Properties.Direction = nil }},
		{"priority", func(rule *armnetwork.SecurityRule) { rule.Properties.Priority = nil }},
		{"access", func(rule *armnetwork.SecurityRule) { rule.Properties.Access = nil }},
		{"protocol", func(rule *armnetwork.SecurityRule) { rule.Properties.Protocol = nil }},
	} {
		t.Run("missing "+tt.name, func(t *testing.T) {
			rule := nsgTestNRMSRule(103)
			tt.change(rule)
			_, err := buildNICSecurityRules(nil, []*armnetwork.SecurityRule{rule})
			assert.ErrorContains(t, err, "incomplete")
		})
	}
}

func TestBuildNICSecurityRulesNRMSPrecedence(t *testing.T) {
	allowV4, allowV6 := nsgTestNRMSRule(103), nsgTestNRMSRule(104)
	allowV4.Properties.SourceAddressPrefix = ptr.To("203.0.113.0/24")
	allowV6.Properties.SourceAddressPrefix = ptr.To("2001:db8:2::/48")
	denyV4, denyV6 := nsgTestNRMSRule(107), nsgTestNRMSRule(108)
	for _, rule := range []*armnetwork.SecurityRule{denyV4, denyV6} {
		rule.Properties.Access = ptr.To(armnetwork.SecurityRuleAccessDeny)
		rule.Properties.DestinationPortRange = ptr.To("8080")
	}
	denyV4.Properties.SourceAddressPrefix = ptr.To("20.1.0.0/16")
	denyV6.Properties.SourceAddressPrefix = ptr.To("2001:db8:1:1::/64")
	rules, err := buildNICSecurityRules(map[string]nsgService{
		"svc": nsgTestService(8080, "TCP", "20.0.0.0/8,2001:db8:1::/48", "10.0.0.1", "fd00::1"),
	}, []*armnetwork.SecurityRule{denyV6, allowV4, denyV4, allowV6})
	if !assert.NoError(t, err) {
		return
	}
	for _, tt := range []struct {
		name        string
		source      string
		destination string
		want        armnetwork.SecurityRuleAccess
	}{
		{"ordinary IPv4 allow", "20.2.3.4", "10.0.0.1", armnetwork.SecurityRuleAccessAllow},
		{"ordinary IPv4 deny", "40.1.2.3", "10.0.0.1", armnetwork.SecurityRuleAccessDeny},
		{"NRMS IPv4 allow overrides Service deny", "203.0.113.7", "10.0.0.1", armnetwork.SecurityRuleAccessAllow},
		{"NRMS IPv4 deny overrides Service allow", "20.1.2.3", "10.0.0.1", armnetwork.SecurityRuleAccessDeny},
		{"ordinary IPv6 allow", "2001:db8:1:2::1", "fd00::1", armnetwork.SecurityRuleAccessAllow},
		{"ordinary IPv6 deny", "2001:db8:3::1", "fd00::1", armnetwork.SecurityRuleAccessDeny},
		{"NRMS IPv6 allow overrides Service deny", "2001:db8:2::1", "fd00::1", armnetwork.SecurityRuleAccessAllow},
		{"NRMS IPv6 deny overrides Service allow", "2001:db8:1:1::1", "fd00::1", armnetwork.SecurityRuleAccessDeny},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, nsgTestAccess(t, rules, tt.source, tt.destination, 8080, armnetwork.SecurityRuleProtocolTCP))
		})
	}
}

func TestBuildNICSecurityRulesCapacity(t *testing.T) {
	for _, tt := range []struct {
		name      string
		sources   int
		backends  int
		services  int
		nrms      int
		wantError bool
	}{
		{"source boundary", 5997, 1, 1, 0, false},
		{"source overflow", 5998, 1, 1, 0, true},
		{"destination boundary", 1, 2999, 1, 0, false},
		{"destination overflow", 1, 3000, 1, 0, true},
		{"rule boundary", 1, 1, 999, 0, false},
		{"rule overflow", 1, 1, 1000, 0, true},
		{"source boundary with NRMS", 5996, 1, 1, 1, false},
		{"source overflow with NRMS", 5997, 1, 1, 1, true},
		{"destination boundary with NRMS", 1, 2998, 1, 2, false},
		{"destination overflow with NRMS", 1, 2999, 1, 1, true},
		{"rule boundary with NRMS", 1, 1, 998, 2, false},
		{"rule overflow with NRMS", 1, 1, 999, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			services := make(map[string]nsgService)
			for s := 0; s < tt.services; s++ {
				service := nsgTestService(8080, "TCP", "20.0.0.0/8")
				service.sources = nil
				for i := 0; i < tt.sources; i++ {
					service.sources = append(service.sources,
						netip.PrefixFrom(netip.AddrFrom4([4]byte{20, byte(i >> 8), byte(i), 1}), 32))
				}
				for i := 0; i < tt.backends; i++ {
					id := i + s*tt.backends
					service.addresses = append(service.addresses, netip.AddrFrom4([4]byte{10, byte(id >> 8), byte(id), 1}))
				}
				services[strconv.Itoa(s)] = service
			}
			var existing []*armnetwork.SecurityRule
			for i := 0; i < tt.nrms; i++ {
				existing = append(existing, nsgTestNRMSRule(int32(100+i)))
			}
			_, err := buildNICSecurityRules(services, existing)
			if tt.wantError {
				assert.ErrorContains(t, err, "capacity exceeded")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestBuildNICSecurityRulesRejectsUnownedRules(t *testing.T) {
	for _, tt := range []struct {
		name        string
		source      string
		destination string
		port        string
		protocol    armnetwork.SecurityRuleProtocol
		priority    int32
	}{
		{"broad earlier allow", "*", "*", "*", armnetwork.SecurityRuleProtocolAsterisk, 100},
		{"source service tag", "VirtualNetwork", "*", "8080", armnetwork.SecurityRuleProtocolTCP, 100},
		{"source subset", "20.1.0.0/16", "*", "8080", armnetwork.SecurityRuleProtocolTCP, 100},
		{"different destination", "*", "10.0.0.2", "8080", armnetwork.SecurityRuleProtocolTCP, 100},
		{"different port", "*", "*", "9090", armnetwork.SecurityRuleProtocolTCP, 100},
		{"port range", "*", "*", "8000-8100", armnetwork.SecurityRuleProtocolTCP, 100},
		{"different protocol", "*", "*", "8080", armnetwork.SecurityRuleProtocolUDP, 100},
		{"lower precedence", "*", "*", "*", armnetwork.SecurityRuleProtocolAsterisk, 4096},
	} {
		t.Run(tt.name, func(t *testing.T) {
			foreign := &armnetwork.SecurityRule{
				Name: ptr.To("platform-allow"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Direction:                ptr.To(armnetwork.SecurityRuleDirectionInbound),
					Access:                   ptr.To(armnetwork.SecurityRuleAccessAllow),
					Protocol:                 ptr.To(tt.protocol),
					Priority:                 ptr.To(tt.priority),
					SourceAddressPrefix:      ptr.To(tt.source),
					DestinationAddressPrefix: ptr.To(tt.destination),
					DestinationPortRange:     ptr.To(tt.port),
					SourcePortRange:          ptr.To("*"),
				},
			}
			_, err := buildNICSecurityRules(map[string]nsgService{
				"svc": nsgTestService(8080, "TCP", "20.0.0.0/8", "10.0.0.1"),
			}, []*armnetwork.SecurityRule{foreign})
			assert.ErrorContains(t, err, "unexpected")
			_, err = buildNICSecurityRules(nil, []*armnetwork.SecurityRule{foreign})
			assert.ErrorContains(t, err, "unexpected", "removing a policy must not overwrite foreign rules either")
		})
	}
}
