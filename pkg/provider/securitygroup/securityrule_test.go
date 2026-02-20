/*
Copyright 2025 The Kubernetes Authors.

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
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/iputil"
)

func TestSetDestinationPortRanges(t *testing.T) {
	t.Parallel()

	var (
		makeSecurityRule = func(mutates ...func(*armnetwork.SecurityRule)) *armnetwork.SecurityRule {
			rv := &armnetwork.SecurityRule{
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Priority:                   to.Ptr(int32(100)),
					SourcePortRange:            to.Ptr("*"),
					SourceAddressPrefixes:      to.SliceOfPtrs("10.0.0.1", "192.168.0.0/16", "AzureLoadBalancer"),
					DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.2", "20.0.0.0/16", "AzureContainerRegistry"),
					DestinationPortRanges:      to.SliceOfPtrs("80", "443"),
				},
			}
			for _, mutate := range mutates {
				mutate(rv)
			}
			return rv
		}
	)

	tests := []struct {
		Name       string
		Rule       *armnetwork.SecurityRule
		PortRanges []int32
		Assertions []func(t *testing.T, rule *armnetwork.SecurityRule)
	}{
		{
			Name:       "set one port",
			Rule:       makeSecurityRule(),
			PortRanges: []int32{80},
			Assertions: []func(t *testing.T, rule *armnetwork.SecurityRule){
				func(t *testing.T, actual *armnetwork.SecurityRule) {
					expected := makeSecurityRule(func(rule *armnetwork.SecurityRule) {
						rule.Properties.DestinationPortRanges = to.SliceOfPtrs("80")
					})
					assert.Equal(t, expected, actual)
				},
			},
		},
		{
			Name:       "set multiple ports",
			Rule:       makeSecurityRule(),
			PortRanges: []int32{80, 443},
			Assertions: []func(t *testing.T, rule *armnetwork.SecurityRule){
				func(t *testing.T, actual *armnetwork.SecurityRule) {
					expected := makeSecurityRule(func(rule *armnetwork.SecurityRule) {
						rule.Properties.DestinationPortRanges = to.SliceOfPtrs("443", "80")
					})
					assert.Equal(t, expected, actual)
				},
			},
		},
		{
			Name:       "set empty ports",
			Rule:       makeSecurityRule(),
			PortRanges: []int32{},
			Assertions: []func(t *testing.T, rule *armnetwork.SecurityRule){
				func(t *testing.T, actual *armnetwork.SecurityRule) {
					expected := makeSecurityRule(func(rule *armnetwork.SecurityRule) {
						rule.Properties.DestinationPortRanges = []*string{}
					})
					assert.Equal(t, expected, actual)
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			SetDestinationPortRanges(tt.Rule, tt.PortRanges)

			for _, assertion := range tt.Assertions {
				assertion(t, tt.Rule)
			}
		})
	}
}

func TestSetAsteriskDestinationPortRanges(t *testing.T) {
	t.Parallel()

	var (
		makeSecurityRule = func(mutates ...func(*armnetwork.SecurityRule)) *armnetwork.SecurityRule {
			rv := &armnetwork.SecurityRule{
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Priority:                   to.Ptr(int32(100)),
					SourcePortRange:            to.Ptr("*"),
					SourceAddressPrefixes:      to.SliceOfPtrs("10.0.0.1", "192.168.0.0/16", "AzureLoadBalancer"),
					DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.2", "20.0.0.0/16", "AzureContainerRegistry"),
					DestinationPortRanges:      to.SliceOfPtrs("80", "443"),
				},
			}
			for _, mutate := range mutates {
				mutate(rv)
			}
			return rv
		}
	)

	tests := []struct {
		Name       string
		Rule       *armnetwork.SecurityRule
		Assertions []func(t *testing.T, rule *armnetwork.SecurityRule)
	}{
		{
			Name: "set asterisk ports",
			Rule: makeSecurityRule(),
			Assertions: []func(t *testing.T, rule *armnetwork.SecurityRule){
				func(t *testing.T, actual *armnetwork.SecurityRule) {
					expected := makeSecurityRule(func(rule *armnetwork.SecurityRule) {
						rule.Properties.DestinationPortRange = to.Ptr("*")
						rule.Properties.DestinationPortRanges = nil
					})
					assert.Equal(t, expected, actual)
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			SetAsteriskDestinationPortRange(tt.Rule)

			for _, assertion := range tt.Assertions {
				assertion(t, tt.Rule)
			}
		})
	}
}

func TestGenerateDenyBlockedSecurityRuleName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		Name        string
		Protocol    armnetwork.SecurityRuleProtocol
		IPFamily    iputil.Family
		SrcPrefixes []string
		DstPorts    []int32
		Expected    string
	}{
		{
			Name:        "TCP with IPv4",
			Protocol:    armnetwork.SecurityRuleProtocolTCP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"10.0.0.1", "10.0.0.2"},
			DstPorts:    []int32{80, 443},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_0070730d5e225d645e76e1c23e2cf80f",
		},
		{
			Name:        "TCP with IPv6",
			Protocol:    armnetwork.SecurityRuleProtocolTCP,
			IPFamily:    iputil.IPv6,
			SrcPrefixes: []string{"::1"},
			DstPorts:    []int32{443},
			Expected:    "k8s-azure-lb_deny-blocked_IPv6_7cdb7a69bd6ce190cf1c1bbdaef78abe",
		},
		{
			Name:        "UDP with IPv4",
			Protocol:    armnetwork.SecurityRuleProtocolUDP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"192.168.1.0/24"},
			DstPorts:    []int32{53},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_0fc4a31a46f267e7bb265f58235b91e2",
		},
		{
			Name:        "Asterisk protocol with IPv4",
			Protocol:    armnetwork.SecurityRuleProtocolAsterisk,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"0.0.0.0/0"},
			DstPorts:    []int32{22},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_d05a837b8427e7f73f5b500ab4f42ba5",
		},
		{
			Name:        "order-insensitive for srcPrefixes",
			Protocol:    armnetwork.SecurityRuleProtocolTCP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"bar", "foo"},
			DstPorts:    []int32{80},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_e9f1431f8e7d696203a35c2424eb98ee",
		},
		{
			Name:        "order-insensitive for srcPrefixes reversed",
			Protocol:    armnetwork.SecurityRuleProtocolTCP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"foo", "bar"},
			DstPorts:    []int32{80},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_e9f1431f8e7d696203a35c2424eb98ee",
		},
		{
			Name:        "order-insensitive for dstPorts",
			Protocol:    armnetwork.SecurityRuleProtocolTCP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"test"},
			DstPorts:    []int32{443, 80},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_e9f186e3c1c2ae7997154387a55790e8",
		},
		{
			Name:        "order-insensitive for dstPorts reversed",
			Protocol:    armnetwork.SecurityRuleProtocolTCP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"test"},
			DstPorts:    []int32{80, 443},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_e9f186e3c1c2ae7997154387a55790e8",
		},
		{
			Name:        "empty srcPrefixes",
			Protocol:    armnetwork.SecurityRuleProtocolTCP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{},
			DstPorts:    []int32{80},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_a17e1d096eba6a1f917b0d4060ab0274",
		},
		{
			Name:        "empty dstPorts",
			Protocol:    armnetwork.SecurityRuleProtocolTCP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"10.0.0.1"},
			DstPorts:    []int32{},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_091b9c8f2039b672941e530e6b95dccc",
		},
		{
			Name:        "baseline for parameter change tests",
			Protocol:    armnetwork.SecurityRuleProtocolTCP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"10.0.0.0/8"},
			DstPorts:    []int32{80},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_2e80b3cfd2a6a84924746e76e404cce0",
		},
		{
			Name:        "changing protocol produces different name",
			Protocol:    armnetwork.SecurityRuleProtocolUDP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"10.0.0.0/8"},
			DstPorts:    []int32{80},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_ecbcbf9c2a64eaaeca9e08da2c629f69",
		},
		{
			Name:        "changing srcPrefixes produces different name",
			Protocol:    armnetwork.SecurityRuleProtocolTCP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"172.16.0.0/12"},
			DstPorts:    []int32{80},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_32431d2a7416344890bba9ad9df36cf4",
		},
		{
			Name:        "changing dstPorts produces different name",
			Protocol:    armnetwork.SecurityRuleProtocolTCP,
			IPFamily:    iputil.IPv4,
			SrcPrefixes: []string{"10.0.0.0/8"},
			DstPorts:    []int32{443},
			Expected:    "k8s-azure-lb_deny-blocked_IPv4_a062a047507bb6d8c00fa9c4ce7515ab",
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			result := GenerateDenyBlockedSecurityRuleName(tt.Protocol, tt.IPFamily, tt.SrcPrefixes, tt.DstPorts)
			assert.Equal(t, tt.Expected, result)
		})
	}
}
