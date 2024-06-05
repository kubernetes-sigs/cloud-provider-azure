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

package fixture

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/fnutil"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/iputil"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/securitygroup"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/testutil"
)

// NoiseSecurityRules returns N non cloud-provider-specific security rules.
// It's not random, but it's good enough for testing.
func (f *AzureFixture) NoiseSecurityRules(nRules int) []network.SecurityRule {
	var (
		rv           = make([]network.SecurityRule, 0, nRules)
		protocolByID = func(id int) network.SecurityRuleProtocol {
			switch id % 3 {
			case 0:
				return network.SecurityRuleProtocolTCP
			case 1:
				return network.SecurityRuleProtocolUDP
			default:
				return network.SecurityRuleProtocolAsterisk
			}
		}
	)

	initPriority := int32(100)
	for i := 0; i < nRules; i++ {
		rule := network.SecurityRule{
			Name: ptr.To(fmt.Sprintf("test-security-rule_%d", i)),
			SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
				Priority:  ptr.To(initPriority),
				Protocol:  protocolByID(i),
				Direction: network.SecurityRuleDirectionInbound,
				Access:    network.SecurityRuleAccessAllow,
				SourceAddressPrefixes: ptr.To([]string{
					fmt.Sprintf("140.0.0.%d", i), // NOTE: keep the source IP / destination IP unique to LB ips.
					fmt.Sprintf("130.0.50.%d", i),
				}),
				SourcePortRange: ptr.To("*"),
				DestinationPortRanges: ptr.To([]string{
					fmt.Sprintf("4000%d", i),
					fmt.Sprintf("5000%d", i),
				}),
			},
		}

		switch i % 3 {
		case 0:
			rule.DestinationAddressPrefixes = ptr.To([]string{
				fmt.Sprintf("222.111.0.%d", i),
				fmt.Sprintf("200.0.50.%d", i),
			})
		case 1:
			rule.DestinationAddressPrefix = ptr.To(fmt.Sprintf("222.111.0.%d", i))
		case 2:
			rule.DestinationApplicationSecurityGroups = &[]network.ApplicationSecurityGroup{
				{
					ID: ptr.To(fmt.Sprintf("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/the-rg/providers/Microsoft.Network/applicationSecurityGroups/the-asg-%d", i)),
				},
			}
		}

		rv = append(rv, rule)

		initPriority++
		if initPriority == 103 {
			initPriority = consts.LoadBalancerMaximumPriority
		}
	}

	return rv
}

func (f *AzureFixture) SecurityGroup() *AzureSecurityGroupFixture {
	return &AzureSecurityGroupFixture{
		sg: &network.SecurityGroup{
			Name: ptr.To("nsg"),
			SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
				SecurityRules: &[]network.SecurityRule{},
			},
		},
	}
}

func (f *AzureFixture) AllowSecurityRule(
	protocol network.SecurityRuleProtocol,
	ipFamily iputil.Family,
	srcPrefixes []string,
	dstPorts []int32,
) *AzureAllowSecurityRuleFixture {
	dstPortRanges := fnutil.Map(func(p int32) string { return strconv.FormatInt(int64(p), 10) }, dstPorts)
	sort.Strings(dstPortRanges)

	rv := &AzureAllowSecurityRuleFixture{
		rule: &network.SecurityRule{
			Name: ptr.To(securitygroup.GenerateAllowSecurityRuleName(protocol, ipFamily, srcPrefixes, dstPorts)),
			SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
				Protocol:              protocol,
				Access:                network.SecurityRuleAccessAllow,
				Direction:             network.SecurityRuleDirectionInbound,
				SourcePortRange:       ptr.To("*"),
				DestinationPortRanges: ptr.To(dstPortRanges),
				Priority:              ptr.To(int32(consts.LoadBalancerMinimumPriority)),
			},
		},
	}

	if len(srcPrefixes) == 1 {
		rv.rule.SourceAddressPrefix = ptr.To(srcPrefixes[0])
	} else {
		rv.rule.SourceAddressPrefixes = ptr.To(srcPrefixes)
	}

	return rv
}

func (f *AzureFixture) DenyAllSecurityRule(ipFamily iputil.Family) *AzureDenyAllSecurityRuleFixture {
	return &AzureDenyAllSecurityRuleFixture{
		rule: &network.SecurityRule{
			Name: ptr.To(securitygroup.GenerateDenyAllSecurityRuleName(ipFamily)),
			SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
				Protocol:             network.SecurityRuleProtocolAsterisk,
				Access:               network.SecurityRuleAccessDeny,
				Direction:            network.SecurityRuleDirectionInbound,
				SourcePortRange:      ptr.To("*"),
				SourceAddressPrefix:  ptr.To("*"),
				DestinationPortRange: ptr.To("*"),
				Priority:             ptr.To(int32(consts.LoadBalancerMaximumPriority)),
			},
		},
	}
}

// AzureSecurityGroupFixture is a fixture for an Azure security group.
type AzureSecurityGroupFixture struct {
	sg *network.SecurityGroup
}

func (f *AzureSecurityGroupFixture) WithRules(rules []network.SecurityRule) *AzureSecurityGroupFixture {
	if rules == nil {
		rules = []network.SecurityRule{}
	}
	clonedRules := testutil.CloneInJSON(rules) // keep the original one immutable
	f.sg.SecurityRules = &clonedRules
	return f
}

func (f *AzureSecurityGroupFixture) Build() network.SecurityGroup {
	return *f.sg
}

// AzureAllowSecurityRuleFixture is a fixture for an allow security rule.
type AzureAllowSecurityRuleFixture struct {
	rule *network.SecurityRule
}

func (f *AzureAllowSecurityRuleFixture) WithPriority(p int32) *AzureAllowSecurityRuleFixture {
	f.rule.Priority = ptr.To(p)
	return f
}

func (f *AzureAllowSecurityRuleFixture) WithDestination(prefixes ...string) *AzureAllowSecurityRuleFixture {
	if len(prefixes) == 1 {
		f.rule.DestinationAddressPrefix = ptr.To(prefixes[0])
		f.rule.DestinationAddressPrefixes = nil
	} else {
		f.rule.DestinationAddressPrefix = nil
		f.rule.DestinationAddressPrefixes = ptr.To(securitygroup.NormalizeSecurityRuleAddressPrefixes(prefixes))
	}

	return f
}

func (f *AzureAllowSecurityRuleFixture) Build() network.SecurityRule {
	return *f.rule
}

// AzureDenyAllSecurityRuleFixture is a fixture for a deny-all security rule.
type AzureDenyAllSecurityRuleFixture struct {
	rule *network.SecurityRule
}

func (f *AzureDenyAllSecurityRuleFixture) WithPriority(p int32) *AzureDenyAllSecurityRuleFixture {
	f.rule.Priority = ptr.To(p)
	return f
}

func (f *AzureDenyAllSecurityRuleFixture) WithDestination(prefixes ...string) *AzureDenyAllSecurityRuleFixture {
	if len(prefixes) == 1 {
		f.rule.DestinationAddressPrefix = ptr.To(prefixes[0])
		f.rule.DestinationAddressPrefixes = nil
	} else {
		f.rule.DestinationAddressPrefix = nil
		f.rule.DestinationAddressPrefixes = ptr.To(securitygroup.NormalizeSecurityRuleAddressPrefixes(prefixes))
	}
	return f
}

func (f *AzureDenyAllSecurityRuleFixture) Build() network.SecurityRule {
	return *f.rule
}
