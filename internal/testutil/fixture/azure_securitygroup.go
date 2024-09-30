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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/internal/testutil"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/fnutil"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/iputil"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/securitygroup"
)

// NoiseSecurityRules returns 3 non cloud-provider-specific security rules.
// Use NNoiseSecurityRules if you need more.
func (f *AzureFixture) NoiseSecurityRules() []*armnetwork.SecurityRule {
	return f.NNoiseSecurityRules(3)
}

// NNoiseSecurityRules returns N non cloud-provider-specific security rules.
// It's not random, but it's good enough for testing.
func (f *AzureFixture) NNoiseSecurityRules(nRules int) []*armnetwork.SecurityRule {
	var (
		rv           = make([]*armnetwork.SecurityRule, 0, nRules)
		protocolByID = func(id int) *armnetwork.SecurityRuleProtocol {
			switch id % 3 {
			case 0:
				return to.Ptr(armnetwork.SecurityRuleProtocolTCP)
			case 1:
				return to.Ptr(armnetwork.SecurityRuleProtocolUDP)
			default:
				return to.Ptr(armnetwork.SecurityRuleProtocolAsterisk)
			}
		}
	)

	initPriority := int32(100)
	for i := 0; i < nRules; i++ {
		rule := &armnetwork.SecurityRule{
			Name: ptr.To(fmt.Sprintf("test-security-rule_%d", i)),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Priority:  ptr.To(initPriority),
				Protocol:  protocolByID(i),
				Direction: to.Ptr(armnetwork.SecurityRuleDirectionInbound),
				Access:    to.Ptr(armnetwork.SecurityRuleAccessAllow),
				SourceAddressPrefixes: to.SliceOfPtrs(
					fmt.Sprintf("140.0.0.%d", i), // NOTE: keep the source IP / destination IP unique to LB ips.
					fmt.Sprintf("130.0.50.%d", i),
				),
				SourcePortRange: ptr.To("*"),
				DestinationPortRanges: to.SliceOfPtrs(
					fmt.Sprintf("4000%d", i),
					fmt.Sprintf("5000%d", i),
				),
			},
		}

		switch i % 3 {
		case 0:
			rule.Properties.DestinationAddressPrefixes = to.SliceOfPtrs(
				fmt.Sprintf("222.111.0.%d", i),
				fmt.Sprintf("200.0.50.%d", i),
			)
		case 1:
			rule.Properties.DestinationAddressPrefix = ptr.To(fmt.Sprintf("222.111.0.%d", i))
		case 2:
			rule.Properties.DestinationApplicationSecurityGroups = []*armnetwork.ApplicationSecurityGroup{
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
		sg: &armnetwork.SecurityGroup{
			Name: ptr.To("nsg"),
			Properties: &armnetwork.SecurityGroupPropertiesFormat{
				SecurityRules: []*armnetwork.SecurityRule{},
			},
		},
	}
}

func (f *AzureFixture) AllowSecurityRule(
	protocol armnetwork.SecurityRuleProtocol,
	ipFamily iputil.Family,
	srcPrefixes []string,
	dstPorts []int32,
) *AzureAllowSecurityRuleFixture {
	dstPortRanges := fnutil.Map(func(p int32) string { return strconv.FormatInt(int64(p), 10) }, dstPorts)
	sort.Strings(dstPortRanges)

	rv := &AzureAllowSecurityRuleFixture{
		rule: &armnetwork.SecurityRule{
			Name: ptr.To(securitygroup.GenerateAllowSecurityRuleName(protocol, ipFamily, srcPrefixes, dstPorts)),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Protocol:              to.Ptr(protocol),
				Access:                to.Ptr(armnetwork.SecurityRuleAccessAllow),
				Direction:             to.Ptr(armnetwork.SecurityRuleDirectionInbound),
				SourcePortRange:       ptr.To("*"),
				DestinationPortRanges: to.SliceOfPtrs(dstPortRanges...),
				Priority:              ptr.To(int32(consts.LoadBalancerMinimumPriority)),
			},
		},
	}

	if len(srcPrefixes) == 1 {
		rv.rule.Properties.SourceAddressPrefix = ptr.To(srcPrefixes[0])
	} else {
		rv.rule.Properties.SourceAddressPrefixes = to.SliceOfPtrs(srcPrefixes...)
	}

	return rv
}

func (f *AzureFixture) DenyAllSecurityRule(ipFamily iputil.Family) *AzureDenyAllSecurityRuleFixture {
	return &AzureDenyAllSecurityRuleFixture{
		rule: &armnetwork.SecurityRule{
			Name: ptr.To(securitygroup.GenerateDenyAllSecurityRuleName(ipFamily)),
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Protocol:             to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
				Access:               to.Ptr(armnetwork.SecurityRuleAccessDeny),
				Direction:            to.Ptr(armnetwork.SecurityRuleDirectionInbound),
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
	sg *armnetwork.SecurityGroup
}

func (f *AzureSecurityGroupFixture) WithRules(rules []*armnetwork.SecurityRule) *AzureSecurityGroupFixture {
	if rules == nil {
		rules = []*armnetwork.SecurityRule{}
	}
	clonedRules := testutil.CloneInJSON(rules) // keep the original one immutable
	f.sg.Properties.SecurityRules = clonedRules
	return f
}

func (f *AzureSecurityGroupFixture) Build() *armnetwork.SecurityGroup {
	return f.sg
}

// AzureAllowSecurityRuleFixture is a fixture for an allow security rule.
type AzureAllowSecurityRuleFixture struct {
	rule *armnetwork.SecurityRule
}

func (f *AzureAllowSecurityRuleFixture) WithPriority(p int32) *AzureAllowSecurityRuleFixture {
	f.rule.Properties.Priority = ptr.To(p)
	return f
}

func (f *AzureAllowSecurityRuleFixture) WithDestination(prefixes ...string) *AzureAllowSecurityRuleFixture {
	if len(prefixes) == 1 {
		f.rule.Properties.DestinationAddressPrefix = ptr.To(prefixes[0])
		f.rule.Properties.DestinationAddressPrefixes = nil
	} else {
		f.rule.Properties.DestinationAddressPrefix = nil
		f.rule.Properties.DestinationAddressPrefixes = to.SliceOfPtrs(securitygroup.NormalizeSecurityRuleAddressPrefixes(prefixes)...)
	}

	return f
}

func (f *AzureAllowSecurityRuleFixture) Build() *armnetwork.SecurityRule {
	return f.rule
}

// AzureDenyAllSecurityRuleFixture is a fixture for a deny-all security rule.
type AzureDenyAllSecurityRuleFixture struct {
	rule *armnetwork.SecurityRule
}

func (f *AzureDenyAllSecurityRuleFixture) WithPriority(p int32) *AzureDenyAllSecurityRuleFixture {
	f.rule.Properties.Priority = ptr.To(p)
	return f
}

func (f *AzureDenyAllSecurityRuleFixture) WithDestination(prefixes ...string) *AzureDenyAllSecurityRuleFixture {
	if len(prefixes) == 1 {
		f.rule.Properties.DestinationAddressPrefix = ptr.To(prefixes[0])
		f.rule.Properties.DestinationAddressPrefixes = nil
	} else {
		f.rule.Properties.DestinationAddressPrefix = nil
		f.rule.Properties.DestinationAddressPrefixes = to.SliceOfPtrs(securitygroup.NormalizeSecurityRuleAddressPrefixes(prefixes)...)
	}
	return f
}

func (f *AzureDenyAllSecurityRuleFixture) Build() *armnetwork.SecurityRule {
	return f.rule
}
