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

package securitygroup_test

import (
	"net/netip"
	"sort"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/internal/testutil"
	"sigs.k8s.io/cloud-provider-azure/internal/testutil/fixture"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/fnutil"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/iputil"
	. "sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/securitygroup" //nolint:revive
)

func ExpectNewSecurityGroupHelper(t *testing.T, sg *network.SecurityGroup) *RuleHelper {
	t.Helper()
	helper, err := NewSecurityGroupHelper(sg)
	if err != nil {
		assert.NoError(t, err)
	}
	return helper
}

func TestNewSecurityGroupHelper(t *testing.T) {
	{
		_, err := NewSecurityGroupHelper(nil)
		assert.ErrorIs(t, err, ErrInvalidSecurityGroup)
	}
	{
		_, err := NewSecurityGroupHelper(&network.SecurityGroup{})
		assert.ErrorIs(t, err, ErrInvalidSecurityGroup)
	}
	{
		_, err := NewSecurityGroupHelper(&network.SecurityGroup{
			Name: ptr.To("nsg"),
		})
		assert.ErrorIs(t, err, ErrInvalidSecurityGroup)
	}
	{
		_, err := NewSecurityGroupHelper(&network.SecurityGroup{
			Name:                          ptr.To("nsg"),
			SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
		})
		assert.ErrorIs(t, err, ErrInvalidSecurityGroup)
	}
	{
		helper, err := NewSecurityGroupHelper(&network.SecurityGroup{
			Name: ptr.To("nsg"),
			SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
				SecurityRules: &[]network.SecurityRule{},
			},
		})
		assert.NoError(t, err)

		rv, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
		testutil.ExpectEqualInJSON(t, &network.SecurityGroup{
			Name: ptr.To("nsg"),
			SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
				SecurityRules: &[]network.SecurityRule{},
			},
		}, rv)
	}
}

func TestSecurityGroupHelper_AddRuleForAllowedIPRanges(t *testing.T) {
	fx := fixture.NewFixture()
	t.Run("when prerequisites are not met, it should return error", func(t *testing.T) {
		t.Run("when source IP ranges are not from the same IP family", func(t *testing.T) {
			var (
				sg     = fx.Azure().SecurityGroup().Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)

				protocol     = network.SecurityRuleProtocolTCP
				srcIPRanges  = append(fx.RandomIPv4Prefixes(2), fx.RandomIPv6Prefixes(2)...)
				dstAddresses = fx.RandomIPv4Addresses(2)
				dstPorts     = []int32{80, 443}
			)
			err := helper.AddRuleForAllowedIPRanges(srcIPRanges, protocol, dstAddresses, dstPorts)
			assert.Error(t, err)
			assert.ErrorIs(t, err, ErrSecurityRuleSourceAddressesNotFromSameIPFamily)
		})
		t.Run("when destination addresses are not from the same IP family", func(t *testing.T) {
			var (
				sg     = fx.Azure().SecurityGroup().Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)

				protocol     = network.SecurityRuleProtocolTCP
				srcIPRanges  = fx.RandomIPv4Prefixes(2)
				dstAddresses = append(fx.RandomIPv4Addresses(2), fx.RandomIPv6Addresses(2)...)
				dstPorts     = []int32{80, 443}
			)
			err := helper.AddRuleForAllowedIPRanges(srcIPRanges, protocol, dstAddresses, dstPorts)
			assert.Error(t, err)
			assert.ErrorIs(t, err, ErrSecurityRuleDestinationAddressesNotFromSameIPFamily)
		})
		t.Run("when source IP ranges and destination addresses are not from the same IP family", func(t *testing.T) {
			var (
				sg     = fx.Azure().SecurityGroup().Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)

				protocol     = network.SecurityRuleProtocolTCP
				srcIPRanges  = fx.RandomIPv4Prefixes(2)
				dstAddresses = fx.RandomIPv6Addresses(2)
				dstPorts     = []int32{80, 443}
			)
			err := helper.AddRuleForAllowedIPRanges(srcIPRanges, protocol, dstAddresses, dstPorts)
			assert.Error(t, err)
			assert.ErrorIs(t, err, ErrSecurityRuleSourceAndDestinationNotFromSameIPFamily)
		})
	})

	t.Run("when no rule exists, it should add one", func(t *testing.T) {
		cases := []struct {
			TestName     string
			IPFamily     iputil.Family
			Protocol     network.SecurityRuleProtocol
			SrcIPRanges  []netip.Prefix
			DstAddresses []netip.Addr
			DstPorts     []int32
		}{
			{
				TestName:     "TCP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     network.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "TCP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     network.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "UDP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     network.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "UDP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     network.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "ANY / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     network.SecurityRuleProtocolAsterisk,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{53},
			},
			{
				TestName:     "ANY / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     network.SecurityRuleProtocolAsterisk,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{53},
			},
		}

		for _, c := range cases {
			var (
				protocol     = c.Protocol
				ipFamily     = c.IPFamily
				srcIPRanges  = c.SrcIPRanges
				dstAddresses = c.DstAddresses
				dstPorts     = c.DstPorts

				rules  = fx.Azure().NoiseSecurityRules()
				sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)
			)

			err := helper.AddRuleForAllowedIPRanges(srcIPRanges, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should add 1 rule", c.TestName)
			assert.Equal(t, len(*outputSG.SecurityRules), len(rules)+1, "[`%s`] should add 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, rules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []network.SecurityRule{
				{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, fnutil.Map(func(v netip.Prefix) string { return v.String() }, srcIPRanges), dstPorts)),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   protocol,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes:      ptr.To(fnutil.Map(func(v netip.Prefix) string { return v.String() }, srcIPRanges)),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To(fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)),
						DestinationPortRanges:      ptr.To(NormalizeDestinationPortRanges(dstPorts)),
						Priority:                   ptr.To(int32(500)),
					},
				},
			}, "[`%s`] 1 allow rule should be created", c.TestName)
		}
	})

	t.Run("when rules exists and rules outdated, it should update the one", func(t *testing.T) {
		cases := []struct {
			TestName     string
			IPFamily     iputil.Family
			Protocol     network.SecurityRuleProtocol
			SrcIPRanges  []netip.Prefix
			DstAddresses []netip.Addr
			DstPorts     []int32
		}{
			{
				TestName:     "TCP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     network.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "TCP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     network.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "UDP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     network.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "UDP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     network.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "ANY / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     network.SecurityRuleProtocolAsterisk,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{53},
			},
			{
				TestName:     "ANY / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     network.SecurityRuleProtocolAsterisk,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{53},
			},
		}

		for _, c := range cases {
			var (
				protocol     = c.Protocol
				ipFamily     = c.IPFamily
				srcIPRanges  = c.SrcIPRanges
				dstAddresses = c.DstAddresses
				dstPorts     = c.DstPorts

				targetRule = network.SecurityRule{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, fnutil.Map(func(v netip.Prefix) string { return v.String() }, srcIPRanges), dstPorts)),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   protocol,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes:      ptr.To(fnutil.Map(func(v netip.Prefix) string { return v.String() }, srcIPRanges)),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"foo", "bar"}), // Should append the dstAddresses.
						DestinationPortRanges:      ptr.To(NormalizeDestinationPortRanges(dstPorts)),
						Priority:                   ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, &sg)
			)

			err := helper.AddRuleForAllowedIPRanges(srcIPRanges, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should update 1 rule", c.TestName)
			assert.Equal(t, len(*outputSG.SecurityRules), len(irrelevantRules)+1, "[`%s`] should only update 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)

			expectedTargetRule := targetRule
			{
				// It should append the new destination addresses.
				*expectedTargetRule.DestinationAddressPrefixes = append(
					*expectedTargetRule.DestinationAddressPrefixes,
					fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)...,
				)
				sort.Strings(*expectedTargetRule.DestinationAddressPrefixes)
			}
			testutil.ExpectHasSecurityRules(t, outputSG, []network.SecurityRule{
				expectedTargetRule,
			}, "[`%s`] 1 allow rule should be updated", c.TestName)
		}
	})

	t.Run("when rules exists and rules updated, it should not update", func(t *testing.T) {
		cases := []struct {
			TestName     string
			IPFamily     iputil.Family
			Protocol     network.SecurityRuleProtocol
			SrcIPRanges  []netip.Prefix
			DstAddresses []netip.Addr
			DstPorts     []int32
		}{
			{
				TestName:     "TCP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     network.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "TCP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     network.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "UDP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     network.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "UDP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     network.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "ANY / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     network.SecurityRuleProtocolAsterisk,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{53},
			},
			{
				TestName:     "ANY / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     network.SecurityRuleProtocolAsterisk,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{53},
			},
		}

		for _, c := range cases {
			var (
				protocol     = c.Protocol
				ipFamily     = c.IPFamily
				srcIPRanges  = c.SrcIPRanges
				dstAddresses = c.DstAddresses
				dstPorts     = c.DstPorts

				targetRule = network.SecurityRule{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, fnutil.Map(func(v netip.Prefix) string { return v.String() }, srcIPRanges), dstPorts)),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:              protocol,
						Access:                network.SecurityRuleAccessAllow,
						Direction:             network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes: ptr.To(fnutil.Map(func(v netip.Prefix) string { return v.String() }, srcIPRanges)),
						SourcePortRange:       ptr.To("*"),
						DestinationAddressPrefixes: ptr.To(
							NormalizeSecurityRuleAddressPrefixes(
								append([]string{"foo", "bar"}, fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)...),
							),
						),
						DestinationPortRanges: ptr.To(NormalizeDestinationPortRanges(dstPorts)),
						Priority:              ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, &sg)
			)

			err := helper.AddRuleForAllowedIPRanges(srcIPRanges, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.False(t, updated, "[`%s`] should not update any rules", c.TestName)
			assert.Equal(t, len(*outputSG.SecurityRules), len(irrelevantRules)+1, "[`%s`] all rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []network.SecurityRule{targetRule}, "[`%s`] the target rule remain unchanged", c.TestName)
		}
	})
}

func TestSecurityGroupHelper_AddRuleForAllowedServiceTag(t *testing.T) {
	fx := fixture.NewFixture()
	t.Run("when prerequisites are not met, it should return error", func(t *testing.T) {
		t.Run("when destination addresses are not from the same IP family", func(t *testing.T) {
			var (
				sg     = fx.Azure().SecurityGroup().Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)

				protocol     = network.SecurityRuleProtocolTCP
				serviceTag   = "AzureCloud"
				dstAddresses = append(fx.RandomIPv4Addresses(2), fx.RandomIPv6Addresses(2)...)
				dstPorts     = []int32{80, 443}
			)
			err := helper.AddRuleForAllowedServiceTag(serviceTag, protocol, dstAddresses, dstPorts)
			assert.Error(t, err)
			assert.ErrorIs(t, err, ErrSecurityRuleDestinationAddressesNotFromSameIPFamily)
		})
	})

	t.Run("when no rule exists, it should add one", func(t *testing.T) {
		cases := []struct {
			TestName      string
			IPFamily      iputil.Family
			Protocol      network.SecurityRuleProtocol
			SrcServiceTag string
			DstAddresses  []netip.Addr
			DstPorts      []int32
		}{
			{
				TestName:      "TCP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      network.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "TCP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      network.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "UDP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      network.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "UDP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      network.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "ANY / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      network.SecurityRuleProtocolAsterisk,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{53},
			},
			{
				TestName:      "ANY / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      network.SecurityRuleProtocolAsterisk,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{53},
			},
		}

		for _, c := range cases {
			var (
				protocol      = c.Protocol
				ipFamily      = c.IPFamily
				srcServiceTag = c.SrcServiceTag
				dstAddresses  = c.DstAddresses
				dstPorts      = c.DstPorts

				rules  = fx.Azure().NoiseSecurityRules()
				sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)
			)

			err := helper.AddRuleForAllowedServiceTag(srcServiceTag, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should add 1 rule", c.TestName)
			assert.Equal(t, len(*outputSG.SecurityRules), len(rules)+1, "[`%s`] should add 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, rules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []network.SecurityRule{
				{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, []string{srcServiceTag}, dstPorts)),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   protocol,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefix:        ptr.To(srcServiceTag),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To(fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)),
						DestinationPortRanges:      ptr.To(NormalizeDestinationPortRanges(dstPorts)),
						Priority:                   ptr.To(int32(500)),
					},
				},
			}, "[`%s`] 1 allow rule should be created", c.TestName)
		}
	})

	t.Run("when rule exists and outdated, it should update the one", func(t *testing.T) {
		cases := []struct {
			TestName      string
			IPFamily      iputil.Family
			Protocol      network.SecurityRuleProtocol
			SrcServiceTag string
			DstAddresses  []netip.Addr
			DstPorts      []int32
		}{
			{
				TestName:      "TCP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      network.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "TCP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      network.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "UDP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      network.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "UDP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      network.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "ANY / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      network.SecurityRuleProtocolAsterisk,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{53},
			},
			{
				TestName:      "ANY / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      network.SecurityRuleProtocolAsterisk,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{53},
			},
		}

		for _, c := range cases {
			var (
				protocol      = c.Protocol
				ipFamily      = c.IPFamily
				srcServiceTag = c.SrcServiceTag
				dstAddresses  = c.DstAddresses
				dstPorts      = c.DstPorts

				targetRule = network.SecurityRule{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, []string{srcServiceTag}, dstPorts)),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   protocol,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefix:        ptr.To(srcServiceTag),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"foo", "bar"}), // Should append the dstAddresses.
						DestinationPortRanges:      ptr.To(NormalizeDestinationPortRanges(dstPorts)),
						Priority:                   ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, &sg)
			)

			err := helper.AddRuleForAllowedServiceTag(srcServiceTag, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should update 1 rule", c.TestName)
			assert.Equal(t, len(*outputSG.SecurityRules), len(irrelevantRules)+1, "[`%s`] should only update 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)

			expectedTargetRule := testutil.CloneInJSON(targetRule)
			{
				// It should append the new destination addresses.
				*expectedTargetRule.DestinationAddressPrefixes = append(
					*expectedTargetRule.DestinationAddressPrefixes,
					fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)...,
				)
				sort.Strings(*expectedTargetRule.DestinationAddressPrefixes)
			}
			testutil.ExpectHasSecurityRules(t, outputSG, []network.SecurityRule{
				expectedTargetRule,
			}, "[`%s`] 1 allow rule should be updated", c.TestName)
		}
	})

	t.Run("when rules exists and rules up-to-update, it should remain the same", func(t *testing.T) {
		cases := []struct {
			TestName      string
			IPFamily      iputil.Family
			Protocol      network.SecurityRuleProtocol
			SrcServiceTag string
			DstAddresses  []netip.Addr
			DstPorts      []int32
		}{
			{
				TestName:      "TCP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      network.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "TCP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      network.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "UDP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      network.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "UDP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      network.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "ANY / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      network.SecurityRuleProtocolAsterisk,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{53},
			},
			{
				TestName:      "ANY / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      network.SecurityRuleProtocolAsterisk,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{53},
			},
		}

		for _, c := range cases {
			var (
				protocol      = c.Protocol
				ipFamily      = c.IPFamily
				srcServiceTag = c.SrcServiceTag
				dstAddresses  = c.DstAddresses
				dstPorts      = c.DstPorts

				targetRule = network.SecurityRule{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, []string{srcServiceTag}, dstPorts)),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:            protocol,
						Access:              network.SecurityRuleAccessAllow,
						Direction:           network.SecurityRuleDirectionInbound,
						SourceAddressPrefix: ptr.To(srcServiceTag),
						SourcePortRange:     ptr.To("*"),
						DestinationAddressPrefixes: ptr.To(
							NormalizeSecurityRuleAddressPrefixes(
								append([]string{"foo", "bar"}, fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)...),
							),
						),
						DestinationPortRanges: ptr.To(NormalizeDestinationPortRanges(dstPorts)),
						Priority:              ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, &sg)
			)

			err := helper.AddRuleForAllowedServiceTag(srcServiceTag, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.False(t, updated, "[`%s`] should not update any rules", c.TestName)
			assert.Equal(t, len(*outputSG.SecurityRules), len(irrelevantRules)+1, "[`%s`] all rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []network.SecurityRule{targetRule}, "[`%s`] the target rule remain unchanged", c.TestName)
		}
	})

}

func TestSecurityGroupHelper_AddRuleForDenyAll(t *testing.T) {
	fx := fixture.NewFixture()
	t.Run("when prerequisites are not met, it should return error", func(t *testing.T) {
		t.Run("when destination addresses are not from the same IP family", func(t *testing.T) {
			var (
				sg     = fx.Azure().SecurityGroup().Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)

				dstAddresses = append(fx.RandomIPv4Addresses(2), fx.RandomIPv6Addresses(2)...)
			)
			err := helper.AddRuleForDenyAll(dstAddresses)
			assert.Error(t, err)
			assert.ErrorIs(t, err, ErrSecurityRuleDestinationAddressesNotFromSameIPFamily)
		})
	})

	t.Run("when no rules exists, it should add one", func(t *testing.T) {
		cases := []struct {
			TestName     string
			IPFamily     iputil.Family
			DstAddresses []netip.Addr
		}{
			{
				TestName:     "TCP / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "TCP / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
			{
				TestName:     "UDP / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "UDP / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
			{
				TestName:     "ANY / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "ANY / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
		}

		for _, c := range cases {
			var (
				ipFamily     = c.IPFamily
				dstAddresses = c.DstAddresses

				rules  = fx.Azure().NoiseSecurityRules()
				sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)
			)

			err := helper.AddRuleForDenyAll(dstAddresses)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should add 1 rule", c.TestName)
			assert.Equal(t, len(*outputSG.SecurityRules), len(rules)+1, "[`%s`] should add 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, rules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []network.SecurityRule{
				{
					Name: ptr.To(GenerateDenyAllSecurityRuleName(ipFamily)),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolAsterisk,
						Access:                     network.SecurityRuleAccessDeny,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To(fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)),
						DestinationPortRange:       ptr.To("*"),
						Priority:                   ptr.To(int32(4095)),
					},
				},
			}, "[`%s`] 1 allow rule should be created", c.TestName)
		}
	})

	t.Run("when rule exists and outdated, it should update the one", func(t *testing.T) {
		cases := []struct {
			TestName     string
			IPFamily     iputil.Family
			DstAddresses []netip.Addr
		}{
			{
				TestName:     "TCP / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "TCP / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
			{
				TestName:     "UDP / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "UDP / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
			{
				TestName:     "ANY / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "ANY / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
		}

		for _, c := range cases {
			var (
				ipFamily     = c.IPFamily
				dstAddresses = c.DstAddresses

				targetRule = network.SecurityRule{
					Name: ptr.To(GenerateDenyAllSecurityRuleName(ipFamily)),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolAsterisk,
						Access:                     network.SecurityRuleAccessDeny,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"foo", "bar"}), // Should append the dstAddresses.
						DestinationPortRange:       ptr.To("*"),
						Priority:                   ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, &sg)
			)

			err := helper.AddRuleForDenyAll(dstAddresses)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should update 1 rule", c.TestName)
			assert.Equal(t, len(*outputSG.SecurityRules), len(irrelevantRules)+1, "[`%s`] should only update 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)

			expectedTargetRule := testutil.CloneInJSON(targetRule)
			{
				// It should append the new destination addresses.
				*expectedTargetRule.DestinationAddressPrefixes = append(
					*expectedTargetRule.DestinationAddressPrefixes,
					fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)...,
				)
				sort.Strings(*expectedTargetRule.DestinationAddressPrefixes)
			}
			testutil.ExpectHasSecurityRules(t, outputSG, []network.SecurityRule{
				expectedTargetRule,
			}, "[`%s`] 1 allow rule should be updated", c.TestName)
		}
	})

	t.Run("when rule exists and outdated, it should update the one - dst prefix corner case", func(t *testing.T) {
		cases := []struct {
			TestName     string
			IPFamily     iputil.Family
			DstAddresses []netip.Addr
		}{
			{
				TestName:     "TCP / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "TCP / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
			{
				TestName:     "UDP / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "UDP / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
			{
				TestName:     "ANY / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "ANY / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
		}

		for _, c := range cases {
			var (
				ipFamily     = c.IPFamily
				dstAddresses = c.DstAddresses

				targetRule = network.SecurityRule{
					Name: ptr.To(GenerateDenyAllSecurityRuleName(ipFamily)),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolAsterisk,
						Access:                   network.SecurityRuleAccessDeny,
						Direction:                network.SecurityRuleDirectionInbound,
						SourceAddressPrefix:      ptr.To("*"),
						SourcePortRange:          ptr.To("*"),
						DestinationAddressPrefix: ptr.To("foo"), // Should append the dstAddresses.
						DestinationPortRange:     ptr.To("*"),
						Priority:                 ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, &sg)
			)

			err := helper.AddRuleForDenyAll(dstAddresses)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should update 1 rule", c.TestName)
			assert.Equal(t, len(*outputSG.SecurityRules), len(irrelevantRules)+1, "[`%s`] should only update 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)

			expectedTargetRule := testutil.CloneInJSON(targetRule)
			{
				// It should append the new destination addresses.
				ps := append(
					fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses),
					*expectedTargetRule.DestinationAddressPrefix,
				)
				expectedTargetRule.DestinationAddressPrefixes = &ps
				sort.Strings(*expectedTargetRule.DestinationAddressPrefixes)

				expectedTargetRule.DestinationAddressPrefix = nil
			}
			testutil.ExpectHasSecurityRules(t, outputSG, []network.SecurityRule{
				expectedTargetRule,
			}, "[`%s`] 1 allow rule should be updated", c.TestName)
		}
	})

	t.Run("when rules exists and rules up-to-update, it should remain the same", func(t *testing.T) {
		cases := []struct {
			TestName     string
			IPFamily     iputil.Family
			DstAddresses []netip.Addr
		}{
			{
				TestName:     "TCP / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "TCP / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
			{
				TestName:     "UDP / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "UDP / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
			{
				TestName:     "ANY / IPv4",
				IPFamily:     iputil.IPv4,
				DstAddresses: fx.RandomIPv4Addresses(2),
			},
			{
				TestName:     "ANY / IPv6",
				IPFamily:     iputil.IPv6,
				DstAddresses: fx.RandomIPv6Addresses(2),
			},
		}

		for _, c := range cases {
			var (
				ipFamily     = c.IPFamily
				dstAddresses = c.DstAddresses

				targetRule = network.SecurityRule{
					Name: ptr.To(GenerateDenyAllSecurityRuleName(ipFamily)),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:            network.SecurityRuleProtocolAsterisk,
						Access:              network.SecurityRuleAccessDeny,
						Direction:           network.SecurityRuleDirectionInbound,
						SourceAddressPrefix: ptr.To("*"),
						SourcePortRange:     ptr.To("*"),
						DestinationAddressPrefixes: ptr.To(
							NormalizeSecurityRuleAddressPrefixes(
								append([]string{"foo", "bar"}, fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)...),
							),
						),
						DestinationPortRange: ptr.To("*"),
						Priority:             ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, &sg)
			)

			err := helper.AddRuleForDenyAll(dstAddresses)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.False(t, updated, "[`%s`] should not update any rules", c.TestName)
			assert.Equal(t, len(*outputSG.SecurityRules), len(irrelevantRules)+1, "[`%s`] all rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []network.SecurityRule{targetRule}, "[`%s`] the target rule remain unchanged", c.TestName)
		}
	})
}

func TestRuleHelper_RemoveDestinationFromRules(t *testing.T) {
	fx := fixture.NewFixture()

	t.Run("it should not patch rules if no rules exist", func(t *testing.T) {
		var (
			sg           = fx.Azure().SecurityGroup().Build()
			helper       = ExpectNewSecurityGroupHelper(t, &sg)
			dstAddresses = fnutil.Map(func(p netip.Addr) string {
				return p.String()
			}, fx.RandomIPv4Addresses(2))
		)
		err := helper.RemoveDestinationFromRules(network.SecurityRuleProtocolTCP, dstAddresses, []int32{})
		assert.NoError(t, err)

		_, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("it should not patch rules if no rules match", func(t *testing.T) {
		var (
			rules = []network.SecurityRule{
				{
					Name: ptr.To("test-rule-0"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes:      ptr.To([]string{"src_foo", "src_bar"}),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"10.0.0.1", "10.0.0.2"}),
						DestinationPortRanges:      ptr.To([]string{"443", "80"}),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("test-rule-1"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolUDP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes:      ptr.To([]string{"src_baz", "src_quo"}),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"20.0.0.1", "20.0.0.2"}),
						DestinationPortRanges:      ptr.To([]string{"53"}),
						Priority:                   ptr.To(int32(501)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, &sg)
			dstAddresses = []string{
				"192.168.0.1",
				"192.168.0.2",
			}
		)
		assert.NoError(t, helper.RemoveDestinationFromRules(network.SecurityRuleProtocolTCP, dstAddresses, []int32{}))
		assert.NoError(t, helper.RemoveDestinationFromRules(network.SecurityRuleProtocolUDP, dstAddresses, []int32{}))

		_, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("it should patch the matched rules", func(t *testing.T) {
		var (
			rules = []network.SecurityRule{
				{
					Name: ptr.To("test-rule-0"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes:      ptr.To([]string{"src_foo", "src_bar"}),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"10.0.0.1", "10.0.0.2", "192.168.0.1"}),
						DestinationPortRanges:      ptr.To([]string{"443", "80"}),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("test-rule-1"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolUDP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes:      ptr.To([]string{"src_baz", "src_quo"}),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"20.0.0.1", "192.168.0.1", "192.168.0.2", "20.0.0.2"}),
						DestinationPortRanges:      ptr.To([]string{"53"}),
						Priority:                   ptr.To(int32(501)),
					},
				},
				{
					Name: ptr.To("test-rule-2"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolAsterisk,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"8.8.8.8"}),
						DestinationPortRanges:      ptr.To([]string{"5000"}),
						Priority:                   ptr.To(int32(502)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, &sg)
			dstAddresses = []string{
				"192.168.0.1",
				"192.168.0.2",
			}
		)

		assert.NoError(t, helper.RemoveDestinationFromRules(network.SecurityRuleProtocolTCP, dstAddresses, []int32{}))
		assert.NoError(t, helper.RemoveDestinationFromRules(network.SecurityRuleProtocolUDP, dstAddresses, []int32{}))

		outputSG, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.True(t, updated)
		testutil.ExpectEqualInJSON(t, []network.SecurityRule{
			{
				Name: ptr.To("test-rule-0"),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                   network.SecurityRuleProtocolTCP,
					Access:                     network.SecurityRuleAccessAllow,
					Direction:                  network.SecurityRuleDirectionInbound,
					SourceAddressPrefixes:      ptr.To([]string{"src_foo", "src_bar"}),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: ptr.To([]string{"10.0.0.1", "10.0.0.2"}),
					DestinationPortRanges:      ptr.To([]string{"443", "80"}),
					Priority:                   ptr.To(int32(500)),
				},
			},
			{
				Name: ptr.To("test-rule-1"),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                   network.SecurityRuleProtocolUDP,
					Access:                     network.SecurityRuleAccessAllow,
					Direction:                  network.SecurityRuleDirectionInbound,
					SourceAddressPrefixes:      ptr.To([]string{"src_baz", "src_quo"}),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: ptr.To([]string{"20.0.0.1", "20.0.0.2"}),
					DestinationPortRanges:      ptr.To([]string{"53"}),
					Priority:                   ptr.To(int32(501)),
				},
			},
			{
				Name: ptr.To("test-rule-2"),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                   network.SecurityRuleProtocolAsterisk,
					Access:                     network.SecurityRuleAccessAllow,
					Direction:                  network.SecurityRuleDirectionInbound,
					SourceAddressPrefix:        ptr.To("*"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: ptr.To([]string{"8.8.8.8"}),
					DestinationPortRanges:      ptr.To([]string{"5000"}),
					Priority:                   ptr.To(int32(502)),
				},
			},
		}, outputSG.SecurityRules)
	})

	t.Run("it should remove the matched rules if no destination addresses left", func(t *testing.T) {
		var (
			rules = []network.SecurityRule{
				{
					Name: ptr.To("test-rule-0"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes:      ptr.To([]string{"src_foo", "src_bar"}),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"10.0.0.1", "10.0.0.2", "192.168.0.1"}),
						DestinationPortRanges:      ptr.To([]string{"443", "80"}),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("test-rule-1"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolUDP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes:      ptr.To([]string{"src_baz", "src_quo"}),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"192.168.0.1", "192.168.0.2"}),
						DestinationPortRanges:      ptr.To([]string{"53"}),
						Priority:                   ptr.To(int32(501)),
					},
				},
				{
					Name: ptr.To("test-rule-2"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"8.8.8.8"}),
						DestinationPortRanges:      ptr.To([]string{"5000"}),
						Priority:                   ptr.To(int32(502)),
					},
				},
				{
					Name: ptr.To("test-rule-3"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolUDP,
						Access:                   network.SecurityRuleAccessAllow,
						Direction:                network.SecurityRuleDirectionInbound,
						SourceAddressPrefix:      ptr.To("*"),
						SourcePortRange:          ptr.To("*"),
						DestinationAddressPrefix: ptr.To("192.168.0.1"),
						DestinationPortRanges:    ptr.To([]string{"8000"}),
						Priority:                 ptr.To(int32(2000)),
					},
				},
				{
					Name: ptr.To("test-rule-4"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{}),
						DestinationAddressPrefix:   ptr.To("192.168.0.1"),
						DestinationPortRanges:      ptr.To([]string{"8000"}),
						Priority:                   ptr.To(int32(2001)),
					},
				},
				{
					Name: ptr.To("test-rule-5"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolAsterisk,
						Access:                     network.SecurityRuleAccessDeny,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{}),
						DestinationAddressPrefix:   ptr.To("*"), // Should not overwrite the DestinationAddressPrefixes.
						DestinationPortRanges:      ptr.To([]string{"8000"}),
						Priority:                   ptr.To(int32(2002)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, &sg)
			dstAddresses = []string{
				"192.168.0.1",
				"192.168.0.2",
			}
		)
		assert.NoError(t, helper.RemoveDestinationFromRules(network.SecurityRuleProtocolTCP, dstAddresses, []int32{}))
		assert.NoError(t, helper.RemoveDestinationFromRules(network.SecurityRuleProtocolUDP, dstAddresses, []int32{}))

		outputSG, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.True(t, updated)
		testutil.ExpectEqualInJSON(t, []network.SecurityRule{
			{
				Name: ptr.To("test-rule-0"),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                   network.SecurityRuleProtocolTCP,
					Access:                     network.SecurityRuleAccessAllow,
					Direction:                  network.SecurityRuleDirectionInbound,
					SourceAddressPrefixes:      ptr.To([]string{"src_foo", "src_bar"}),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: ptr.To([]string{"10.0.0.1", "10.0.0.2"}),
					DestinationPortRanges:      ptr.To([]string{"443", "80"}),
					Priority:                   ptr.To(int32(500)),
				},
			},
			{
				Name: ptr.To("test-rule-2"),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                   network.SecurityRuleProtocolTCP,
					Access:                     network.SecurityRuleAccessAllow,
					Direction:                  network.SecurityRuleDirectionInbound,
					SourceAddressPrefix:        ptr.To("*"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: ptr.To([]string{"8.8.8.8"}),
					DestinationPortRanges:      ptr.To([]string{"5000"}),
					Priority:                   ptr.To(int32(502)),
				},
			},
			{
				Name: ptr.To("test-rule-5"),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                   network.SecurityRuleProtocolAsterisk,
					Access:                     network.SecurityRuleAccessDeny,
					Direction:                  network.SecurityRuleDirectionInbound,
					SourceAddressPrefix:        ptr.To("*"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: ptr.To([]string{}),
					DestinationAddressPrefix:   ptr.To("*"),
					DestinationPortRanges:      ptr.To([]string{"8000"}),
					Priority:                   ptr.To(int32(2002)),
				},
			},
		}, outputSG.SecurityRules)
	})

	t.Run("it should retain the port ranges if specified - all ports retained - nothing changed", func(t *testing.T) {
		var (
			rules = []network.SecurityRule{
				{
					Name: ptr.To("test-rule-0"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes:      ptr.To([]string{"src_foo", "src_bar"}),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"10.0.0.1", "10.0.0.2", "192.168.0.1"}),
						DestinationPortRanges:      ptr.To([]string{"443", "80"}),
						Priority:                   ptr.To(int32(500)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, &sg)
			dstAddresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
		)

		assert.NoError(t, helper.RemoveDestinationFromRules(network.SecurityRuleProtocolTCP, dstAddresses, []int32{443, 80}))

		_, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("it should retain the port ranges if specified - part of ports retained - split the rule", func(t *testing.T) {
		var (
			rules = []network.SecurityRule{
				{
					Name: ptr.To("test-rule-0"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes:      ptr.To([]string{"bar", "foo"}),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"10.0.0.1", "10.0.0.2", "192.168.0.1"}),
						DestinationPortRanges:      ptr.To([]string{"443", "80"}),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("test-rule-1"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolUDP, // Different protocol, should not be touched.
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourceAddressPrefixes:      ptr.To([]string{"baz"}),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: ptr.To([]string{"10.0.0.1", "10.0.0.2", "192.168.0.1"}),
						DestinationPortRanges:      ptr.To([]string{"443", "80"}),
						Priority:                   ptr.To(int32(501)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, &sg)
			dstAddresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
		)

		assert.NoError(t, helper.RemoveDestinationFromRules(network.SecurityRuleProtocolTCP, dstAddresses, []int32{443}))

		outputSG, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.True(t, updated)
		testutil.ExpectEqualInJSON(t, []network.SecurityRule{
			{
				Name: ptr.To("test-rule-0"),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                 network.SecurityRuleProtocolTCP,
					Access:                   network.SecurityRuleAccessAllow,
					Direction:                network.SecurityRuleDirectionInbound,
					SourceAddressPrefixes:    ptr.To([]string{"bar", "foo"}),
					SourcePortRange:          ptr.To("*"),
					DestinationAddressPrefix: ptr.To("192.168.0.1"),
					DestinationPortRanges:    ptr.To([]string{"443", "80"}),
					Priority:                 ptr.To(int32(500)),
				},
			},
			{
				Name: ptr.To("test-rule-1"),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                   network.SecurityRuleProtocolUDP, // Different protocol, should not be touched.
					Access:                     network.SecurityRuleAccessAllow,
					Direction:                  network.SecurityRuleDirectionInbound,
					SourceAddressPrefixes:      ptr.To([]string{"baz"}),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: ptr.To([]string{"10.0.0.1", "10.0.0.2", "192.168.0.1"}),
					DestinationPortRanges:      ptr.To([]string{"443", "80"}),
					Priority:                   ptr.To(int32(501)),
				},
			},
			{
				Name: ptr.To("k8s-azure-lb_allow_IPv4_b5ae07e8a4177ea2d37162cdf2badf8b"),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                   network.SecurityRuleProtocolTCP,
					Access:                     network.SecurityRuleAccessAllow,
					Direction:                  network.SecurityRuleDirectionInbound,
					SourceAddressPrefixes:      ptr.To([]string{"bar", "foo"}),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: ptr.To([]string{"10.0.0.1", "10.0.0.2"}),
					DestinationPortRanges:      ptr.To([]string{"443"}),
					Priority:                   ptr.To(int32(502)),
				},
			},
		}, outputSG.SecurityRules)
	})
}

func TestSecurityGroupHelper_SecurityGroup(t *testing.T) {
	fx := fixture.NewFixture()
	t.Run("when no rule applied, it should return the original security group", func(t *testing.T) {
		var (
			rules  = fx.Azure().NoiseSecurityRules()
			sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper = ExpectNewSecurityGroupHelper(t, &sg)
		)

		outputSG, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)

		testutil.ExpectEqualInJSON(t, fx.Azure().SecurityGroup().WithRules(fx.Azure().NoiseSecurityRules()).Build(), outputSG)
	})

	t.Run("when the number of rules exceeds the limit, it should return error", func(t *testing.T) {
		var (
			rules  = fx.Azure().NNoiseSecurityRules(MaxSecurityRulesPerGroup + 1)
			sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper = ExpectNewSecurityGroupHelper(t, &sg)
		)

		outputSG, updated, err := helper.SecurityGroup()
		assert.Error(t, err)
		assert.False(t, updated)
		assert.Nil(t, outputSG)
	})

	t.Run("when the number of source prefixes exceeds the limit, it should return error", func(t *testing.T) {

		t.Run("1 rule with max source prefixes", func(t *testing.T) {
			var (
				rules = fx.Azure().NNoiseSecurityRules(1)
			)

			rules[0].SourceAddressPrefixes = ptr.To(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup + 1))

			var (
				sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)
			)

			outputSG, updated, err := helper.SecurityGroup()
			assert.Error(t, err)
			assert.False(t, updated)
			assert.Nil(t, outputSG)
		})

		t.Run("2 rules with max source prefixes", func(t *testing.T) {
			// The limit is per security group, not per rule.
			var (
				rules = fx.Azure().NNoiseSecurityRules(2)
			)

			rules[0].SourceAddressPrefixes = ptr.To(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup / 2))
			rules[1].SourceAddressPrefixes = ptr.To(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup/2 + 1))

			var (
				sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)
			)

			outputSG, updated, err := helper.SecurityGroup()
			assert.Error(t, err)
			assert.False(t, updated)
			assert.Nil(t, outputSG)
		})
	})

	t.Run("when the number of destination prefixes exceeds the limit, it should return error", func(t *testing.T) {

		t.Run("1 rule with max destination prefixes", func(t *testing.T) {
			var (
				rules = fx.Azure().NNoiseSecurityRules(1)
			)

			rules[0].DestinationAddressPrefixes = ptr.To(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup + 1))

			var (
				sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)
			)

			outputSG, updated, err := helper.SecurityGroup()
			assert.Error(t, err)
			assert.False(t, updated)
			assert.Nil(t, outputSG)
		})

		t.Run("2 rules with max destination prefixes", func(t *testing.T) {
			// The limit is per security group, not per rule.
			var (
				rules = fx.Azure().NNoiseSecurityRules(2)
			)

			rules[0].DestinationAddressPrefixes = ptr.To(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup / 2))
			rules[1].DestinationAddressPrefixes = ptr.To(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup/2 + 1))

			var (
				sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
				helper = ExpectNewSecurityGroupHelper(t, &sg)
			)

			outputSG, updated, err := helper.SecurityGroup()
			assert.Error(t, err)
			assert.False(t, updated)
			assert.Nil(t, outputSG)
		})
	})
}

func TestGenerateAllowSecurityRuleName(t *testing.T) {
	t.Run("should be protocol-specific", func(t *testing.T) {
		var (
			ipFamily    = iputil.IPv4
			srcPrefixes = []string{"foo", "bar"}
			dstPorts    = []int32{80, 443}
		)

		assert.Len(t, map[string]bool{
			GenerateAllowSecurityRuleName(network.SecurityRuleProtocolTCP, ipFamily, srcPrefixes, dstPorts):      true,
			GenerateAllowSecurityRuleName(network.SecurityRuleProtocolUDP, ipFamily, srcPrefixes, dstPorts):      true,
			GenerateAllowSecurityRuleName(network.SecurityRuleProtocolAsterisk, ipFamily, srcPrefixes, dstPorts): true,
		}, 3)
	})
	t.Run("should be IPFamily-specific", func(t *testing.T) {
		var (
			protocol    = network.SecurityRuleProtocolTCP
			srcPrefixes = []string{"foo", "bar"}
			dstPorts    = []int32{80, 443}
		)

		assert.Len(t, map[string]bool{
			GenerateAllowSecurityRuleName(protocol, iputil.IPv4, srcPrefixes, dstPorts): true,
			GenerateAllowSecurityRuleName(protocol, iputil.IPv6, srcPrefixes, dstPorts): true,
		}, 2)
	})

	t.Run("should be SrcPrefixes-specific", func(t *testing.T) {
		var (
			protocol = network.SecurityRuleProtocolTCP
			ipFamily = iputil.IPv4
			dstPorts = []int32{80, 443}
		)

		assert.Len(t, map[string]bool{
			GenerateAllowSecurityRuleName(protocol, ipFamily, []string{"foo"}, dstPorts): true,
			GenerateAllowSecurityRuleName(protocol, ipFamily, []string{"bar"}, dstPorts): true,
		}, 2)

		t.Run("order-insensitive", func(t *testing.T) {
			assert.Equal(t,
				GenerateAllowSecurityRuleName(protocol, ipFamily, []string{"foo", "bar"}, dstPorts),
				GenerateAllowSecurityRuleName(protocol, ipFamily, []string{"bar", "foo"}, dstPorts),
			)
		})
	})

	t.Run("should be DstPorts-specific", func(t *testing.T) {
		var (
			protocol    = network.SecurityRuleProtocolTCP
			ipFamily    = iputil.IPv4
			srcPrefixes = []string{"foo", "bar"}
		)

		assert.Len(t, map[string]bool{
			GenerateAllowSecurityRuleName(protocol, ipFamily, srcPrefixes, []int32{80}):  true,
			GenerateAllowSecurityRuleName(protocol, ipFamily, srcPrefixes, []int32{443}): true,
		}, 2)

		t.Run("order-insensitive", func(t *testing.T) {
			assert.Equal(t,
				GenerateAllowSecurityRuleName(protocol, ipFamily, srcPrefixes, []int32{80, 443}),
				GenerateAllowSecurityRuleName(protocol, ipFamily, srcPrefixes, []int32{443, 80}),
			)
		})
	})
}

func TestGenerateDenyAllSecurityRuleName(t *testing.T) {
	assert.NotEqual(t, GenerateDenyAllSecurityRuleName(iputil.IPv4), GenerateDenyAllSecurityRuleName(iputil.IPv6))
	assert.NotEqual(t, GenerateDenyAllSecurityRuleName(iputil.IPv6), GenerateDenyAllSecurityRuleName(iputil.IPv4))
	assert.Equal(t, GenerateDenyAllSecurityRuleName(iputil.IPv4), GenerateDenyAllSecurityRuleName(iputil.IPv4))
	assert.Equal(t, GenerateDenyAllSecurityRuleName(iputil.IPv6), GenerateDenyAllSecurityRuleName(iputil.IPv6))
	assert.Equal(t, GenerateDenyAllSecurityRuleName(iputil.IPv4), "k8s-azure-lb_deny-all_IPv4")
	assert.Equal(t, GenerateDenyAllSecurityRuleName(iputil.IPv6), "k8s-azure-lb_deny-all_IPv6")
}
