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
