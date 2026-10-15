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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/internal/testutil"
	"sigs.k8s.io/cloud-provider-azure/internal/testutil/fixture"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	. "sigs.k8s.io/cloud-provider-azure/pkg/provider/securitygroup" //nolint:revive
	fnutil "sigs.k8s.io/cloud-provider-azure/pkg/util/collectionutil"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/iputil"
)

func ExpectNewSecurityGroupHelper(t *testing.T, sg *armnetwork.SecurityGroup) *RuleHelper {
	t.Helper()
	helper, err := NewSecurityGroupHelper(log.Noop(), sg)
	if err != nil {
		assert.NoError(t, err)
	}
	return helper
}

func TestRetainedRulePorts(t *testing.T) {
	ipv4 := netip.MustParseAddr("203.0.113.5")
	otherIPv4 := netip.MustParseAddr("203.0.113.6")
	ipv6 := netip.MustParseAddr("2001:db8::5")
	sources := []string{"198.51.100.0/24", "192.0.2.0/24"}

	for _, tt := range []struct {
		name        string
		setup       func(RetainedRulePorts)
		protocol    armnetwork.SecurityRuleProtocol
		sources     []string
		destination netip.Addr
		port        int32
		expected    bool
	}{
		{
			name: "adding another port does not replace existing ports for the same destination",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{ipv4}, []int32{80})
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{ipv4}, []int32{443})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     sources,
			destination: ipv4,
			port:        80,
			expected:    true,
		},
		{
			name: "a port added for one destination does not match another destination",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{ipv4}, []int32{80})
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{otherIPv4}, []int32{81})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     sources,
			destination: ipv4,
			port:        81,
			expected:    false,
		},
		{
			name: "adding another destination does not replace an existing destination's ports",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{otherIPv4}, []int32{81})
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{ipv4}, []int32{80})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     sources,
			destination: otherIPv4,
			port:        81,
			expected:    true,
		},
		{
			name: "a port added only for TCP is not retained for UDP",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{ipv4}, []int32{80})
				mapping.Add(armnetwork.SecurityRuleProtocolUDP, sources,
					[]netip.Addr{ipv4}, []int32{53})
			},
			protocol:    armnetwork.SecurityRuleProtocolUDP,
			sources:     sources,
			destination: ipv4,
			port:        80,
			expected:    false,
		},
		{
			name: "adding TCP ports does not replace existing UDP ports",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolUDP, sources,
					[]netip.Addr{ipv4}, []int32{53})
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{ipv4}, []int32{80})
			},
			protocol:    armnetwork.SecurityRuleProtocolUDP,
			sources:     sources,
			destination: ipv4,
			port:        53,
			expected:    true,
		},
		{
			name: "retains a port for an IPv6 destination",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, []string{"Internet"},
					[]netip.Addr{ipv6}, []int32{443})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{"Internet"},
			destination: ipv6,
			port:        443,
			expected:    true,
		},
		{
			name: "an IPv6 destination does not match an IPv4 destination",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, []string{"Internet"},
					[]netip.Addr{ipv6}, []int32{443})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{"Internet"},
			destination: ipv4,
			port:        443,
			expected:    false,
		},
		{
			name: "source prefix order and duplicates do not affect matching",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{ipv4}, []int32{80})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{"192.0.2.0/24", "198.51.100.0/24", "192.0.2.0/24"},
			destination: ipv4,
			port:        80,
			expected:    true,
		},
		{
			name: "does not retain a port for different source prefixes",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{ipv4}, []int32{80})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{"Internet"},
			destination: ipv4,
			port:        80,
			expected:    false,
		},
		{
			name: "retains a port for matching nil source prefixes",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, nil,
					[]netip.Addr{ipv4}, []int32{8081})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     nil,
			destination: ipv4,
			port:        8081,
			expected:    true,
		},
		{
			name: "does not retain a port for different source prefixes when added with nil sources",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, nil,
					[]netip.Addr{ipv4}, []int32{8081})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{"Internet"},
			destination: ipv4,
			port:        8081,
			expected:    false,
		},
		{
			name: "retains a port for a matching empty source string",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, []string{""},
					[]netip.Addr{ipv4}, []int32{8082})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{""},
			destination: ipv4,
			port:        8082,
			expected:    true,
		},
		{
			name: "does not retain a port for different source prefixes when added with an empty source string",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, []string{""},
					[]netip.Addr{ipv4}, []int32{8082})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{"Internet"},
			destination: ipv4,
			port:        8082,
			expected:    false,
		},
		{
			name: "retains a port for a matching empty source list",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, []string{},
					[]netip.Addr{ipv4}, []int32{8083})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{},
			destination: ipv4,
			port:        8083,
			expected:    true,
		},
		{
			name: "does not retain a port for different source prefixes when added with an empty source list",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, []string{},
					[]netip.Addr{ipv4}, []int32{8083})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{"Internet"},
			destination: ipv4,
			port:        8083,
			expected:    false,
		},
		{
			name: "AddIgnoringSourcePrefixes retains the port without matching source prefixes",
			setup: func(mapping RetainedRulePorts) {
				mapping.AddIgnoringSourcePrefixes(armnetwork.SecurityRuleProtocolTCP,
					[]netip.Addr{otherIPv4}, []int32{8080})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{"Internet"},
			destination: otherIPv4,
			port:        8080,
			expected:    true,
		},
		{
			name: "Add does not replace ports added with AddIgnoringSourcePrefixes",
			setup: func(mapping RetainedRulePorts) {
				mapping.AddIgnoringSourcePrefixes(armnetwork.SecurityRuleProtocolTCP,
					[]netip.Addr{otherIPv4}, []int32{8080})
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{otherIPv4}, []int32{81})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     sources,
			destination: otherIPv4,
			port:        8080,
			expected:    true,
		},
		{
			name: "AddIgnoringSourcePrefixes still requires a matching destination",
			setup: func(mapping RetainedRulePorts) {
				mapping.AddIgnoringSourcePrefixes(armnetwork.SecurityRuleProtocolTCP,
					[]netip.Addr{otherIPv4}, []int32{8080})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{"Internet"},
			destination: ipv4,
			port:        8080,
			expected:    false,
		},
		{
			name: "AddIgnoringSourcePrefixes still requires a matching protocol",
			setup: func(mapping RetainedRulePorts) {
				mapping.AddIgnoringSourcePrefixes(armnetwork.SecurityRuleProtocolTCP,
					[]netip.Addr{otherIPv4}, []int32{8080})
			},
			protocol:    armnetwork.SecurityRuleProtocolUDP,
			sources:     []string{"Internet"},
			destination: otherIPv4,
			port:        8080,
			expected:    false,
		},
		{
			name: "ignoring source prefixes for port 8080 does not change source matching for port 81",
			setup: func(mapping RetainedRulePorts) {
				mapping.Add(armnetwork.SecurityRuleProtocolTCP, sources,
					[]netip.Addr{otherIPv4}, []int32{81})
				mapping.AddIgnoringSourcePrefixes(armnetwork.SecurityRuleProtocolTCP,
					[]netip.Addr{otherIPv4}, []int32{8080})
			},
			protocol:    armnetwork.SecurityRuleProtocolTCP,
			sources:     []string{"Internet"},
			destination: otherIPv4,
			port:        81,
			expected:    false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mapping := make(RetainedRulePorts)
			tt.setup(mapping)
			assert.Equal(t, tt.expected, mapping.Retains(tt.protocol, tt.sources, tt.destination, tt.port))
		})
	}
	assert.Equal(t, []string{"198.51.100.0/24", "192.0.2.0/24"}, sources)
}

func TestNewSecurityGroupHelper(t *testing.T) {
	{
		_, err := NewSecurityGroupHelper(log.Noop(), nil)
		assert.ErrorIs(t, err, ErrInvalidSecurityGroup)
	}
	{
		_, err := NewSecurityGroupHelper(log.Noop(), &armnetwork.SecurityGroup{})
		assert.ErrorIs(t, err, ErrInvalidSecurityGroup)
	}
	{
		_, err := NewSecurityGroupHelper(log.Noop(), &armnetwork.SecurityGroup{
			Name: ptr.To("nsg"),
		})
		assert.ErrorIs(t, err, ErrInvalidSecurityGroup)
	}
	{
		_, err := NewSecurityGroupHelper(log.Noop(), &armnetwork.SecurityGroup{
			Name:       ptr.To("nsg"),
			Properties: &armnetwork.SecurityGroupPropertiesFormat{},
		})
		assert.ErrorIs(t, err, ErrInvalidSecurityGroup)
	}
	{
		helper, err := NewSecurityGroupHelper(log.Noop(), &armnetwork.SecurityGroup{
			Name: ptr.To("nsg"),
			Properties: &armnetwork.SecurityGroupPropertiesFormat{
				SecurityRules: []*armnetwork.SecurityRule{},
			},
		})
		assert.NoError(t, err)

		rv, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
		testutil.ExpectEqualInJSON(t, &armnetwork.SecurityGroup{
			Name: ptr.To("nsg"),
			Properties: &armnetwork.SecurityGroupPropertiesFormat{
				SecurityRules: []*armnetwork.SecurityRule{},
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
				helper = ExpectNewSecurityGroupHelper(t, sg)

				protocol     = armnetwork.SecurityRuleProtocolTCP
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
				helper = ExpectNewSecurityGroupHelper(t, sg)

				protocol     = armnetwork.SecurityRuleProtocolTCP
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
				helper = ExpectNewSecurityGroupHelper(t, sg)

				protocol     = armnetwork.SecurityRuleProtocolTCP
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
			Protocol     armnetwork.SecurityRuleProtocol
			SrcIPRanges  []netip.Prefix
			DstAddresses []netip.Addr
			DstPorts     []int32
		}{
			{
				TestName:     "TCP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     armnetwork.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "TCP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     armnetwork.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "UDP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     armnetwork.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "UDP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     armnetwork.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "ANY / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     armnetwork.SecurityRuleProtocolAsterisk,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{53},
			},
			{
				TestName:     "ANY / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     armnetwork.SecurityRuleProtocolAsterisk,
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
				helper = ExpectNewSecurityGroupHelper(t, sg)
			)

			err := helper.AddRuleForAllowedIPRanges(srcIPRanges, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should add 1 rule", c.TestName)
			assert.Equal(t, len(outputSG.Properties.SecurityRules), len(rules)+1, "[`%s`] should add 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, rules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []*armnetwork.SecurityRule{
				{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, fnutil.Map(func(v netip.Prefix) string { return v.String() }, srcIPRanges), dstPorts)),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(protocol),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      fnutil.Map(func(v netip.Prefix) *string { return to.Ptr(v.String()) }, srcIPRanges),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: fnutil.Map(func(v netip.Addr) *string { return to.Ptr(v.String()) }, dstAddresses),
						DestinationPortRanges:      to.SliceOfPtrs(NormalizeDestinationPortRanges(dstPorts)...),
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
			Protocol     armnetwork.SecurityRuleProtocol
			SrcIPRanges  []netip.Prefix
			DstAddresses []netip.Addr
			DstPorts     []int32
		}{
			{
				TestName:     "TCP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     armnetwork.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "TCP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     armnetwork.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "UDP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     armnetwork.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "UDP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     armnetwork.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "ANY / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     armnetwork.SecurityRuleProtocolAsterisk,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{53},
			},
			{
				TestName:     "ANY / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     armnetwork.SecurityRuleProtocolAsterisk,
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

				targetRule = &armnetwork.SecurityRule{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, fnutil.Map(func(v netip.Prefix) string { return v.String() }, srcIPRanges), dstPorts)),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(protocol),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      fnutil.Map(func(v netip.Prefix) *string { return to.Ptr(v.String()) }, srcIPRanges),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("foo", "bar"), // Should append the dstAddresses.
						DestinationPortRanges:      to.SliceOfPtrs(NormalizeDestinationPortRanges(dstPorts)...),
						Priority:                   ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, sg)
			)

			err := helper.AddRuleForAllowedIPRanges(srcIPRanges, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should update 1 rule", c.TestName)
			assert.Equal(t, len(outputSG.Properties.SecurityRules), len(irrelevantRules)+1, "[`%s`] should only update 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)

			expectedTargetRule := targetRule
			{
				// It should append the new destination addresses.
				expectedTargetRule.Properties.DestinationAddressPrefixes = append(
					expectedTargetRule.Properties.DestinationAddressPrefixes,
					fnutil.Map(func(v netip.Addr) *string { return to.Ptr(v.String()) }, dstAddresses)...,
				)
				result := fnutil.Map(func(v *string) string { return *v }, expectedTargetRule.Properties.DestinationAddressPrefixes)
				sort.Strings(result)
				expectedTargetRule.Properties.DestinationAddressPrefixes = to.SliceOfPtrs(result...)
			}
			testutil.ExpectHasSecurityRules(t, outputSG, []*armnetwork.SecurityRule{
				expectedTargetRule,
			}, "[`%s`] 1 allow rule should be updated", c.TestName)
		}
	})

	t.Run("when rules exists and rules updated, it should not update", func(t *testing.T) {
		cases := []struct {
			TestName     string
			IPFamily     iputil.Family
			Protocol     armnetwork.SecurityRuleProtocol
			SrcIPRanges  []netip.Prefix
			DstAddresses []netip.Addr
			DstPorts     []int32
		}{
			{
				TestName:     "TCP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     armnetwork.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "TCP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     armnetwork.SecurityRuleProtocolTCP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{80, 443},
			},
			{
				TestName:     "UDP / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     armnetwork.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "UDP / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     armnetwork.SecurityRuleProtocolUDP,
				SrcIPRanges:  fx.RandomIPv6Prefixes(2),
				DstAddresses: fx.RandomIPv6Addresses(2),
				DstPorts:     []int32{5000},
			},
			{
				TestName:     "ANY / IPv4",
				IPFamily:     iputil.IPv4,
				Protocol:     armnetwork.SecurityRuleProtocolAsterisk,
				SrcIPRanges:  fx.RandomIPv4Prefixes(2),
				DstAddresses: fx.RandomIPv4Addresses(2),
				DstPorts:     []int32{53},
			},
			{
				TestName:     "ANY / IPv6",
				IPFamily:     iputil.IPv6,
				Protocol:     armnetwork.SecurityRuleProtocolAsterisk,
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

				targetRule = &armnetwork.SecurityRule{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, fnutil.Map(func(v netip.Prefix) string { return v.String() }, srcIPRanges), dstPorts)),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:              to.Ptr(protocol),
						Access:                to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:             to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes: fnutil.Map(func(v netip.Prefix) *string { return to.Ptr(v.String()) }, srcIPRanges),
						SourcePortRange:       ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs(
							NormalizeSecurityRuleAddressPrefixes(
								append([]string{"foo", "bar"}, fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)...),
							)...),
						DestinationPortRanges: to.SliceOfPtrs(NormalizeDestinationPortRanges(dstPorts)...),
						Priority:              ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, sg)
			)

			err := helper.AddRuleForAllowedIPRanges(srcIPRanges, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.False(t, updated, "[`%s`] should not update any rules", c.TestName)
			assert.Equal(t, len(outputSG.Properties.SecurityRules), len(irrelevantRules)+1, "[`%s`] all rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []*armnetwork.SecurityRule{targetRule}, "[`%s`] the target rule remain unchanged", c.TestName)
		}
	})
}

func TestSecurityGroupHelper_AddRuleForAllowedServiceTag(t *testing.T) {
	fx := fixture.NewFixture()
	t.Run("when prerequisites are not met, it should return error", func(t *testing.T) {
		t.Run("when destination addresses are not from the same IP family", func(t *testing.T) {
			var (
				sg     = fx.Azure().SecurityGroup().Build()
				helper = ExpectNewSecurityGroupHelper(t, sg)

				protocol     = armnetwork.SecurityRuleProtocolTCP
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
			Protocol      armnetwork.SecurityRuleProtocol
			SrcServiceTag string
			DstAddresses  []netip.Addr
			DstPorts      []int32
		}{
			{
				TestName:      "TCP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      armnetwork.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "TCP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      armnetwork.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "UDP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      armnetwork.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "UDP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      armnetwork.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "ANY / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      armnetwork.SecurityRuleProtocolAsterisk,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{53},
			},
			{
				TestName:      "ANY / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      armnetwork.SecurityRuleProtocolAsterisk,
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
				helper = ExpectNewSecurityGroupHelper(t, sg)
			)

			err := helper.AddRuleForAllowedServiceTag(srcServiceTag, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should add 1 rule", c.TestName)
			assert.Equal(t, len(outputSG.Properties.SecurityRules), len(rules)+1, "[`%s`] should add 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, rules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []*armnetwork.SecurityRule{
				{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, []string{srcServiceTag}, dstPorts)),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(protocol),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:        ptr.To(srcServiceTag),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: fnutil.Map(func(v netip.Addr) *string { return to.Ptr(v.String()) }, dstAddresses),
						DestinationPortRanges:      to.SliceOfPtrs(NormalizeDestinationPortRanges(dstPorts)...),
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
			Protocol      armnetwork.SecurityRuleProtocol
			SrcServiceTag string
			DstAddresses  []netip.Addr
			DstPorts      []int32
		}{
			{
				TestName:      "TCP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      armnetwork.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "TCP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      armnetwork.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "UDP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      armnetwork.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "UDP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      armnetwork.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "ANY / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      armnetwork.SecurityRuleProtocolAsterisk,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{53},
			},
			{
				TestName:      "ANY / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      armnetwork.SecurityRuleProtocolAsterisk,
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

				targetRule = &armnetwork.SecurityRule{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, []string{srcServiceTag}, dstPorts)),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(protocol),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:        ptr.To(srcServiceTag),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("foo", "bar"), // Should append the dstAddresses.
						DestinationPortRanges:      to.SliceOfPtrs(NormalizeDestinationPortRanges(dstPorts)...),
						Priority:                   ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, sg)
			)

			err := helper.AddRuleForAllowedServiceTag(srcServiceTag, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should update 1 rule", c.TestName)
			assert.Equal(t, len(outputSG.Properties.SecurityRules), len(irrelevantRules)+1, "[`%s`] should only update 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)

			expectedTargetRule := testutil.CloneInJSON(targetRule)
			{
				// It should append the new destination addresses.
				expectedTargetRule.Properties.DestinationAddressPrefixes = append(
					expectedTargetRule.Properties.DestinationAddressPrefixes,
					fnutil.Map(func(v netip.Addr) *string { return to.Ptr(v.String()) }, dstAddresses)...,
				)
				result := fnutil.Map(func(v *string) string { return *v }, expectedTargetRule.Properties.DestinationAddressPrefixes)
				sort.Strings(result)
				expectedTargetRule.Properties.DestinationAddressPrefixes = to.SliceOfPtrs(result...)
			}
			testutil.ExpectHasSecurityRules(t, outputSG, []*armnetwork.SecurityRule{
				expectedTargetRule,
			}, "[`%s`] 1 allow rule should be updated", c.TestName)
		}
	})

	t.Run("when rules exists and rules up-to-update, it should remain the same", func(t *testing.T) {
		cases := []struct {
			TestName      string
			IPFamily      iputil.Family
			Protocol      armnetwork.SecurityRuleProtocol
			SrcServiceTag string
			DstAddresses  []netip.Addr
			DstPorts      []int32
		}{
			{
				TestName:      "TCP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      armnetwork.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "TCP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      armnetwork.SecurityRuleProtocolTCP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{80, 443},
			},
			{
				TestName:      "UDP / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      armnetwork.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "UDP / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      armnetwork.SecurityRuleProtocolUDP,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv6Addresses(2),
				DstPorts:      []int32{5000},
			},
			{
				TestName:      "ANY / IPv4",
				IPFamily:      iputil.IPv4,
				Protocol:      armnetwork.SecurityRuleProtocolAsterisk,
				SrcServiceTag: fx.Azure().ServiceTag(),
				DstAddresses:  fx.RandomIPv4Addresses(2),
				DstPorts:      []int32{53},
			},
			{
				TestName:      "ANY / IPv6",
				IPFamily:      iputil.IPv6,
				Protocol:      armnetwork.SecurityRuleProtocolAsterisk,
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

				targetRule = &armnetwork.SecurityRule{
					Name: ptr.To(GenerateAllowSecurityRuleName(protocol, ipFamily, []string{srcServiceTag}, dstPorts)),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:            to.Ptr(protocol),
						Access:              to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:           to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix: ptr.To(srcServiceTag),
						SourcePortRange:     ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs(
							NormalizeSecurityRuleAddressPrefixes(
								append([]string{"foo", "bar"}, fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)...),
							)...),
						DestinationPortRanges: to.SliceOfPtrs(NormalizeDestinationPortRanges(dstPorts)...),
						Priority:              ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, sg)
			)

			err := helper.AddRuleForAllowedServiceTag(srcServiceTag, protocol, dstAddresses, dstPorts)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.False(t, updated, "[`%s`] should not update any rules", c.TestName)
			assert.Equal(t, len(outputSG.Properties.SecurityRules), len(irrelevantRules)+1, "[`%s`] all rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []*armnetwork.SecurityRule{targetRule}, "[`%s`] the target rule remain unchanged", c.TestName)
		}
	})

}

func TestSecurityGroupHelper_AddRuleForDenyAll(t *testing.T) {
	fx := fixture.NewFixture()
	t.Run("when prerequisites are not met, it should return error", func(t *testing.T) {
		t.Run("when destination addresses are not from the same IP family", func(t *testing.T) {
			var (
				sg     = fx.Azure().SecurityGroup().Build()
				helper = ExpectNewSecurityGroupHelper(t, sg)

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
				helper = ExpectNewSecurityGroupHelper(t, sg)
			)

			err := helper.AddRuleForDenyAll(dstAddresses)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should add 1 rule", c.TestName)
			assert.Equal(t, len(outputSG.Properties.SecurityRules), len(rules)+1, "[`%s`] should add 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, rules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []*armnetwork.SecurityRule{
				{
					Name: ptr.To(GenerateDenyAllSecurityRuleName(ipFamily)),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessDeny),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: fnutil.Map(func(v netip.Addr) *string { return to.Ptr(v.String()) }, dstAddresses),
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

				targetRule = &armnetwork.SecurityRule{
					Name: ptr.To(GenerateDenyAllSecurityRuleName(ipFamily)),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessDeny),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("foo", "bar"), // Should append the dstAddresses.
						DestinationPortRange:       ptr.To("*"),
						Priority:                   ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, sg)
			)

			err := helper.AddRuleForDenyAll(dstAddresses)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should update 1 rule", c.TestName)
			assert.Equal(t, len(outputSG.Properties.SecurityRules), len(irrelevantRules)+1, "[`%s`] should only update 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)

			expectedTargetRule := testutil.CloneInJSON(targetRule)
			{
				// It should append the new destination addresses.
				expectedTargetRule.Properties.DestinationAddressPrefixes = append(
					expectedTargetRule.Properties.DestinationAddressPrefixes,
					fnutil.Map(func(v netip.Addr) *string { return to.Ptr(v.String()) }, dstAddresses)...)
				result := fnutil.Map(func(v *string) string { return *v }, expectedTargetRule.Properties.DestinationAddressPrefixes)
				sort.Strings(result)
				expectedTargetRule.Properties.DestinationAddressPrefixes = to.SliceOfPtrs(result...)
			}
			testutil.ExpectHasSecurityRules(t, outputSG, []*armnetwork.SecurityRule{
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

				targetRule = &armnetwork.SecurityRule{
					Name: ptr.To(GenerateDenyAllSecurityRuleName(ipFamily)),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                 to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						Access:                   to.Ptr(armnetwork.SecurityRuleAccessDeny),
						Direction:                to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:      ptr.To("*"),
						SourcePortRange:          ptr.To("*"),
						DestinationAddressPrefix: ptr.To("foo"), // Should append the dstAddresses.
						DestinationPortRange:     ptr.To("*"),
						Priority:                 ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, sg)
			)

			err := helper.AddRuleForDenyAll(dstAddresses)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.True(t, updated, "[`%s`] should update 1 rule", c.TestName)
			assert.Equal(t, len(outputSG.Properties.SecurityRules), len(irrelevantRules)+1, "[`%s`] should only update 1 rule", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)

			expectedTargetRule := testutil.CloneInJSON(targetRule)
			{
				// It should append the new destination addresses.
				ps := append(
					fnutil.Map(func(v netip.Addr) *string { return to.Ptr(v.String()) }, dstAddresses),
					expectedTargetRule.Properties.DestinationAddressPrefix,
				)
				expectedTargetRule.Properties.DestinationAddressPrefixes = ps
				expectedTargetRule.Properties.DestinationAddressPrefix = nil
			}
			testutil.ExpectHasSecurityRules(t, outputSG, []*armnetwork.SecurityRule{
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

				targetRule = &armnetwork.SecurityRule{
					Name: ptr.To(GenerateDenyAllSecurityRuleName(ipFamily)),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:            to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						Access:              to.Ptr(armnetwork.SecurityRuleAccessDeny),
						Direction:           to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix: ptr.To("*"),
						SourcePortRange:     ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs(
							NormalizeSecurityRuleAddressPrefixes(
								append([]string{"foo", "bar"}, fnutil.Map(func(v netip.Addr) string { return v.String() }, dstAddresses)...),
							)...,
						),
						DestinationPortRange: ptr.To("*"),
						Priority:             ptr.To(int32(950)), // A random priority, should remain unchanged.
					},
				}
				irrelevantRules = fx.Azure().NoiseSecurityRules()
				sg              = fx.Azure().SecurityGroup().WithRules(append(irrelevantRules, targetRule)).Build()
				helper          = ExpectNewSecurityGroupHelper(t, sg)
			)

			err := helper.AddRuleForDenyAll(dstAddresses)
			assert.NoError(t, err)

			outputSG, updated, err := helper.SecurityGroup()

			assert.NoError(t, err)
			assert.False(t, updated, "[`%s`] should not update any rules", c.TestName)
			assert.Equal(t, len(outputSG.Properties.SecurityRules), len(irrelevantRules)+1, "[`%s`] all rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, irrelevantRules, "[`%s`] the original irrelevant rules should remain unchanged", c.TestName)
			testutil.ExpectHasSecurityRules(t, outputSG, []*armnetwork.SecurityRule{targetRule}, "[`%s`] the target rule remain unchanged", c.TestName)
		}
	})
}

func TestRuleHelper_RetainDestinationFromRules(t *testing.T) {
	fx := fixture.NewFixture()

	t.Run("it should not patch rules if no rules exist", func(t *testing.T) {
		var (
			sg           = fx.Azure().SecurityGroup().Build()
			helper       = ExpectNewSecurityGroupHelper(t, sg)
			dstAddresses = fnutil.Map(func(p netip.Addr) string {
				return p.String()
			}, fx.RandomIPv4Addresses(2))
		)
		err := helper.RetainDestinationFromRules(dstAddresses)
		assert.NoError(t, err)

		_, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("it should not patch rules if no rules match", func(t *testing.T) {
		var (
			rules = []*armnetwork.SecurityRule{
				{
					Name: ptr.To("k8s-azure-lb-test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2"),
						DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
						Priority:                   ptr.To(int32(300)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, sg)
			dstAddresses = []string{
				"192.168.0.1",
				"192.168.0.2",
			}
		)

		assert.NoError(t, helper.RetainDestinationFromRules(dstAddresses))

		_, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("it should retain matching destination prefixes and remove others", func(t *testing.T) {
		var (
			rules = []*armnetwork.SecurityRule{
				{
					Name: ptr.To("k8s-azure-lb-test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("8.8.8.8"),
						DestinationPortRanges:      to.SliceOfPtrs("5000"),
						Priority:                   ptr.To(int32(400)),
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-1"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2", "192.168.0.1", "192.168.0.2"),
						DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-2"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_baz", "src_quo"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("20.0.0.1", "192.168.0.1", "30.0.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("53"),
						Priority:                   ptr.To(int32(501)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, sg)
			dstAddresses = []string{
				"192.168.0.1",
				"192.168.0.2",
				"10.0.0.1",
			}
		)

		assert.NoError(t, helper.RetainDestinationFromRules(dstAddresses))

		outputSG, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.True(t, updated)
		testutil.ExpectEqualInJSON(t, []*armnetwork.SecurityRule{
			{
				Name: ptr.To("k8s-azure-lb-test-rule-0"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefix:        ptr.To("*"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("8.8.8.8"),
					DestinationPortRanges:      to.SliceOfPtrs("5000"),
					Priority:                   ptr.To(int32(400)),
				},
			},
			{
				Name: ptr.To("k8s-azure-lb-test-rule-1"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "192.168.0.1", "192.168.0.2"),
					DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
					Priority:                   ptr.To(int32(500)),
				},
			},
			{
				Name: ptr.To("k8s-azure-lb-test-rule-2"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                 to.Ptr(armnetwork.SecurityRuleProtocolUDP),
					Access:                   to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:    to.SliceOfPtrs("src_baz", "src_quo"),
					SourcePortRange:          ptr.To("*"),
					DestinationAddressPrefix: to.Ptr("192.168.0.1"),
					DestinationPortRanges:    to.SliceOfPtrs("53"),
					Priority:                 ptr.To(int32(501)),
				},
			},
		}, outputSG.Properties.SecurityRules)
	})

	t.Run("it should only retain destinations for rules in priority range", func(t *testing.T) {
		var (
			rules = []*armnetwork.SecurityRule{
				{
					Name: ptr.To("k8s-azure-lb-test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_foo"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("192.168.0.1", "10.0.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("80"),
						Priority:                   ptr.To(int32(100)), // Below LoadBalancerMinimumPriority
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-1"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_bar"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("192.168.0.1", "20.0.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("80"),
						Priority:                   ptr.To(int32(500)), // Within range
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, sg)
			dstAddresses = []string{"192.168.0.1"}
		)

		assert.NoError(t, helper.RetainDestinationFromRules(dstAddresses))

		outputSG, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.True(t, updated)

		// Only the rule within the priority range should be modified
		testutil.ExpectEqualInJSON(t, []*armnetwork.SecurityRule{
			{
				Name: ptr.To("k8s-azure-lb-test-rule-0"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("src_foo"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("192.168.0.1", "10.0.0.1"), // Unchanged
					DestinationPortRanges:      to.SliceOfPtrs("80"),
					Priority:                   ptr.To(int32(100)),
				},
			},
			{
				Name: ptr.To("k8s-azure-lb-test-rule-1"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                 to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                   to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:    to.SliceOfPtrs("src_bar"),
					SourcePortRange:          ptr.To("*"),
					DestinationAddressPrefix: to.Ptr("192.168.0.1"), // Only retained destination
					DestinationPortRanges:    to.SliceOfPtrs("80"),
					Priority:                 ptr.To(int32(500)),
				},
			},
		}, outputSG.Properties.SecurityRules)
	})
}

func TestRuleHelper_RemoveDestinationFromRulesByDestination(t *testing.T) {
	factory := fixture.NewFixture().Azure()
	const (
		firstIPv4       = "203.0.113.5"
		secondIPv4      = "203.0.113.6"
		unrelatedIPv4   = "198.51.100.10"
		existingIPv4    = "198.51.100.20"
		sourceRangeIPv4 = "192.0.2.0/24"
	)
	allowRuleTCP := func(source string, ports []int32, priority int32, destinations ...string) *armnetwork.SecurityRule {
		return factory.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{source}, ports).
			WithPriority(priority).WithDestination(destinations...).Build()
	}

	for _, tt := range []struct {
		name               string
		setupRetainedPorts func(RetainedRulePorts)
		dstPrefixes        []string
		existingRules      []*armnetwork.SecurityRule
		expectedRules      []*armnetwork.SecurityRule
		updated            bool
	}{
		{
			name: "leaves rules unchanged when all destination ports are retained",
			setupRetainedPorts: func(retainedPorts RetainedRulePorts) {
				retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{sourceRangeIPv4},
					[]netip.Addr{netip.MustParseAddr(firstIPv4)}, []int32{80})
				retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{sourceRangeIPv4},
					[]netip.Addr{netip.MustParseAddr(secondIPv4)}, []int32{81})
			},
			dstPrefixes: []string{firstIPv4, secondIPv4},
			existingRules: []*armnetwork.SecurityRule{
				allowRuleTCP(sourceRangeIPv4, []int32{80}, 700, firstIPv4),
				allowRuleTCP(sourceRangeIPv4, []int32{81}, 900, secondIPv4),
			},
			expectedRules: []*armnetwork.SecurityRule{
				allowRuleTCP(sourceRangeIPv4, []int32{80}, 700, firstIPv4),
				allowRuleTCP(sourceRangeIPv4, []int32{81}, 900, secondIPv4),
			},
			updated: false,
		},
		{
			name: "removes the Internet rule when the retained port has different source prefixes",
			setupRetainedPorts: func(retainedPorts RetainedRulePorts) {
				retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{sourceRangeIPv4},
					[]netip.Addr{netip.MustParseAddr(firstIPv4)}, []int32{80})
				retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{sourceRangeIPv4},
					[]netip.Addr{netip.MustParseAddr(secondIPv4)}, []int32{81})
			},
			dstPrefixes: []string{firstIPv4, secondIPv4},
			existingRules: []*armnetwork.SecurityRule{
				allowRuleTCP("Internet", []int32{80}, 700, firstIPv4),
				allowRuleTCP(sourceRangeIPv4, []int32{81}, 501, secondIPv4),
			},
			expectedRules: []*armnetwork.SecurityRule{
				allowRuleTCP(sourceRangeIPv4, []int32{81}, 501, secondIPv4),
			},
			updated: true,
		},
		{
			name: "duplicate cleanup addresses do not prevent removing the second IP from port 80",
			setupRetainedPorts: func(retainedPorts RetainedRulePorts) {
				retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{sourceRangeIPv4},
					[]netip.Addr{netip.MustParseAddr(firstIPv4)}, []int32{80})
				retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{sourceRangeIPv4},
					[]netip.Addr{netip.MustParseAddr(secondIPv4)}, []int32{81})
			},
			dstPrefixes: []string{firstIPv4, firstIPv4, secondIPv4},
			existingRules: []*armnetwork.SecurityRule{
				allowRuleTCP(sourceRangeIPv4, []int32{80}, 500, firstIPv4, secondIPv4),
			},
			expectedRules: []*armnetwork.SecurityRule{
				allowRuleTCP(sourceRangeIPv4, []int32{80}, 500, firstIPv4),
			},
			updated: true,
		},
		{
			name: "splits ports 80 and 81 by destination and reuses the existing port 81 rule",
			setupRetainedPorts: func(retainedPorts RetainedRulePorts) {
				retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{sourceRangeIPv4},
					[]netip.Addr{netip.MustParseAddr(firstIPv4)}, []int32{80})
				retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{sourceRangeIPv4},
					[]netip.Addr{netip.MustParseAddr(secondIPv4)}, []int32{81})
			},
			dstPrefixes: []string{firstIPv4, secondIPv4},
			existingRules: []*armnetwork.SecurityRule{
				allowRuleTCP(sourceRangeIPv4, []int32{80, 81}, 700, firstIPv4, secondIPv4, unrelatedIPv4),
				allowRuleTCP(sourceRangeIPv4, []int32{81}, 501, existingIPv4),
			},
			expectedRules: []*armnetwork.SecurityRule{
				allowRuleTCP(sourceRangeIPv4, []int32{80}, 500, firstIPv4),
				allowRuleTCP(sourceRangeIPv4, []int32{81}, 501, secondIPv4, existingIPv4),
				allowRuleTCP(sourceRangeIPv4, []int32{80, 81}, 700, unrelatedIPv4),
			},
			updated: true,
		},
		{
			name: "assigns freed priorities to split rules in rule-name order",
			setupRetainedPorts: func(retainedPorts RetainedRulePorts) {
				retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{sourceRangeIPv4},
					[]netip.Addr{netip.MustParseAddr(firstIPv4)}, []int32{80})
				retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{sourceRangeIPv4},
					[]netip.Addr{netip.MustParseAddr(secondIPv4)}, []int32{81})
			},
			dstPrefixes: []string{firstIPv4, secondIPv4},
			// Both original rules are deleted. Split rules get the lowest free priorities in
			// rule-name order: 500 for port 80, then 501 for port 81.
			existingRules: []*armnetwork.SecurityRule{
				allowRuleTCP(sourceRangeIPv4, []int32{80, 443}, 501, firstIPv4),
				allowRuleTCP(sourceRangeIPv4, []int32{81, 443}, 500, secondIPv4),
			},
			expectedRules: []*armnetwork.SecurityRule{
				allowRuleTCP(sourceRangeIPv4, []int32{80}, 500, firstIPv4),
				allowRuleTCP(sourceRangeIPv4, []int32{81}, 501, secondIPv4),
			},
			updated: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			retainedPorts := make(RetainedRulePorts)
			tt.setupRetainedPorts(retainedPorts)
			helper := ExpectNewSecurityGroupHelper(t, factory.SecurityGroup().WithRules(tt.existingRules).Build())
			assert.NoError(t, helper.RemoveDestinationFromRules(armnetwork.SecurityRuleProtocolTCP, tt.dstPrefixes, retainedPorts))
			securityGroup, updated, err := helper.SecurityGroup()
			assert.NoError(t, err)
			assert.Equal(t, tt.updated, updated)
			testutil.ExpectExactSecurityRules(t, securityGroup, tt.expectedRules)
		})
	}

	t.Run("adding a source-range allow rule reuses priority 500 released by deleting the Internet rule", func(t *testing.T) {
		retainedPorts := make(RetainedRulePorts)
		retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{sourceRangeIPv4},
			[]netip.Addr{netip.MustParseAddr(firstIPv4)}, []int32{80})
		helper := ExpectNewSecurityGroupHelper(t, factory.SecurityGroup().WithRules([]*armnetwork.SecurityRule{
			allowRuleTCP("Internet", []int32{80}, 500, firstIPv4),
		}).Build())
		assert.NoError(t, helper.RemoveDestinationFromRules(armnetwork.SecurityRuleProtocolTCP, []string{firstIPv4}, retainedPorts))
		assert.NoError(t, helper.AddRuleForAllowedIPRanges([]netip.Prefix{netip.MustParsePrefix(sourceRangeIPv4)}, armnetwork.SecurityRuleProtocolTCP,
			[]netip.Addr{netip.MustParseAddr(firstIPv4)}, []int32{80}))
		securityGroup, _, err := helper.SecurityGroup()
		assert.NoError(t, err)
		testutil.ExpectExactSecurityRules(t, securityGroup, []*armnetwork.SecurityRule{
			allowRuleTCP(sourceRangeIPv4, []int32{80}, 500, firstIPv4),
		})
	})
}

func TestRuleHelper_RemoveDestinationFromRules(t *testing.T) {
	fx := fixture.NewFixture()

	t.Run("it should not patch rules if no rules exist", func(t *testing.T) {
		var (
			sg           = fx.Azure().SecurityGroup().Build()
			helper       = ExpectNewSecurityGroupHelper(t, sg)
			dstAddresses = fnutil.Map(func(p netip.Addr) string {
				return p.String()
			}, fx.RandomIPv4Addresses(2))
		)
		err := helper.RemoveDestinationFromRules(armnetwork.SecurityRuleProtocolTCP, dstAddresses, nil)
		assert.NoError(t, err)

		_, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("it should not patch rules if no rules match", func(t *testing.T) {
		var (
			rules = []*armnetwork.SecurityRule{
				{
					Name: ptr.To("k8s-azure-lb-test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2"),
						DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-1"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_baz", "src_quo"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("20.0.0.1", "20.0.0.2"),
						DestinationPortRanges:      to.SliceOfPtrs("53"),
						Priority:                   ptr.To(int32(501)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, sg)
			dstAddresses = []string{
				"192.168.0.1",
				"192.168.0.2",
			}
		)
		assert.NoError(t, helper.RemoveDestinationFromRules(armnetwork.SecurityRuleProtocolTCP, dstAddresses, nil))
		assert.NoError(t, helper.RemoveDestinationFromRules(armnetwork.SecurityRuleProtocolUDP, dstAddresses, nil))

		_, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("it should patch the matched rules", func(t *testing.T) {
		var (
			rules = []*armnetwork.SecurityRule{
				{
					Name: ptr.To("k8s-azure-lb-test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2", "192.168.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-1"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_baz", "src_quo"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("20.0.0.1", "192.168.0.1", "192.168.0.2", "20.0.0.2"),
						DestinationPortRanges:      to.SliceOfPtrs("53"),
						Priority:                   ptr.To(int32(501)),
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-2"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("8.8.8.8"),
						DestinationPortRanges:      to.SliceOfPtrs("5000"),
						Priority:                   ptr.To(int32(502)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, sg)
			dstAddresses = []string{
				"192.168.0.1",
				"192.168.0.2",
			}
		)

		assert.NoError(t, helper.RemoveDestinationFromRules(armnetwork.SecurityRuleProtocolTCP, dstAddresses, nil))
		assert.NoError(t, helper.RemoveDestinationFromRules(armnetwork.SecurityRuleProtocolUDP, dstAddresses, nil))

		outputSG, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.True(t, updated)
		testutil.ExpectEqualInJSON(t, []*armnetwork.SecurityRule{
			{
				Name: ptr.To("k8s-azure-lb-test-rule-0"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2"),
					DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
					Priority:                   ptr.To(int32(500)),
				},
			},
			{
				Name: ptr.To("k8s-azure-lb-test-rule-1"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("src_baz", "src_quo"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("20.0.0.1", "20.0.0.2"),
					DestinationPortRanges:      to.SliceOfPtrs("53"),
					Priority:                   ptr.To(int32(501)),
				},
			},
			{
				Name: ptr.To("k8s-azure-lb-test-rule-2"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefix:        ptr.To("*"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("8.8.8.8"),
					DestinationPortRanges:      to.SliceOfPtrs("5000"),
					Priority:                   ptr.To(int32(502)),
				},
			},
		}, outputSG.Properties.SecurityRules)
	})

	t.Run("it should remove the matched rules if no destination addresses left", func(t *testing.T) {
		var (
			rules = []*armnetwork.SecurityRule{
				{
					Name: ptr.To("k8s-azure-lb-test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2", "192.168.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-1"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_baz", "src_quo"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("192.168.0.1", "192.168.0.2"),
						DestinationPortRanges:      to.SliceOfPtrs("53"),
						Priority:                   ptr.To(int32(501)),
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-2"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("8.8.8.8"),
						DestinationPortRanges:      to.SliceOfPtrs("5000"),
						Priority:                   ptr.To(int32(502)),
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-3"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                 to.Ptr(armnetwork.SecurityRuleProtocolUDP),
						Access:                   to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:      ptr.To("*"),
						SourcePortRange:          ptr.To("*"),
						DestinationAddressPrefix: ptr.To("192.168.0.1"),
						DestinationPortRanges:    to.SliceOfPtrs("8000"),
						Priority:                 ptr.To(int32(2000)),
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-4"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: []*string{},
						DestinationAddressPrefix:   ptr.To("192.168.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("8000"),
						Priority:                   ptr.To(int32(2001)),
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-5"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessDeny),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefix:        ptr.To("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: []*string{},
						DestinationAddressPrefix:   ptr.To("*"), // Should not overwrite the DestinationAddressPrefixes.
						DestinationPortRanges:      to.SliceOfPtrs("8000"),
						Priority:                   ptr.To(int32(2002)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, sg)
			dstAddresses = []string{
				"192.168.0.1",
				"192.168.0.2",
			}
		)
		assert.NoError(t, helper.RemoveDestinationFromRules(armnetwork.SecurityRuleProtocolTCP, dstAddresses, nil))
		assert.NoError(t, helper.RemoveDestinationFromRules(armnetwork.SecurityRuleProtocolUDP, dstAddresses, nil))

		outputSG, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.True(t, updated)
		testutil.ExpectEqualInJSON(t, []*armnetwork.SecurityRule{
			{
				Name: ptr.To("k8s-azure-lb-test-rule-0"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2"),
					DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
					Priority:                   ptr.To(int32(500)),
				},
			},
			{
				Name: ptr.To("k8s-azure-lb-test-rule-2"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefix:        ptr.To("*"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("8.8.8.8"),
					DestinationPortRanges:      to.SliceOfPtrs("5000"),
					Priority:                   ptr.To(int32(502)),
				},
			},
			{
				Name: ptr.To("k8s-azure-lb-test-rule-5"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessDeny),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefix:        ptr.To("*"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: []*string{},
					DestinationAddressPrefix:   ptr.To("*"),
					DestinationPortRanges:      to.SliceOfPtrs("8000"),
					Priority:                   ptr.To(int32(2002)),
				},
			},
		}, outputSG.Properties.SecurityRules)
	})

	t.Run("it should retain the port ranges if specified - all ports retained - nothing changed", func(t *testing.T) {
		var (
			rules = []*armnetwork.SecurityRule{
				{
					Name: ptr.To("k8s-azure-lb-test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2", "192.168.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
						Priority:                   ptr.To(int32(500)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, sg)
			dstAddresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
		)

		retainedPorts := make(RetainedRulePorts)
		addresses, err := iputil.ParseAddresses(dstAddresses)
		assert.NoError(t, err)
		retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{"src_foo", "src_bar"}, addresses, []int32{443, 80})
		assert.NoError(t, helper.RemoveDestinationFromRules(armnetwork.SecurityRuleProtocolTCP, dstAddresses, retainedPorts))

		_, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("it should retain the port ranges if specified - part of ports retained - split the rule", func(t *testing.T) {
		var (
			rules = []*armnetwork.SecurityRule{
				{
					Name: ptr.To("k8s-azure-lb-test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("bar", "foo"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2", "192.168.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("k8s-azure-lb-test-rule-1"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP), // Different protocol, should not be touched.
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("baz"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2", "192.168.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
						Priority:                   ptr.To(int32(501)),
					},
				},
			}

			sg           = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper       = ExpectNewSecurityGroupHelper(t, sg)
			dstAddresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
		)

		retainedPorts := make(RetainedRulePorts)
		addresses, err := iputil.ParseAddresses(dstAddresses)
		assert.NoError(t, err)
		retainedPorts.Add(armnetwork.SecurityRuleProtocolTCP, []string{"bar", "foo"}, addresses, []int32{443})
		assert.NoError(t, helper.RemoveDestinationFromRules(armnetwork.SecurityRuleProtocolTCP, dstAddresses, retainedPorts))

		outputSG, updated, err := helper.SecurityGroup()
		assert.NoError(t, err)
		assert.True(t, updated)
		testutil.ExpectEqualInJSON(t, []*armnetwork.SecurityRule{
			{
				Name: ptr.To("k8s-azure-lb-test-rule-0"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                 to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                   to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:    to.SliceOfPtrs("bar", "foo"),
					SourcePortRange:          ptr.To("*"),
					DestinationAddressPrefix: ptr.To("192.168.0.1"),
					DestinationPortRanges:    to.SliceOfPtrs("443", "80"),
					Priority:                 ptr.To(int32(500)),
				},
			},
			{
				Name: ptr.To("k8s-azure-lb-test-rule-1"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP), // Different protocol, should not be touched.
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("baz"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2", "192.168.0.1"),
					DestinationPortRanges:      to.SliceOfPtrs("443", "80"),
					Priority:                   ptr.To(int32(501)),
				},
			},
			{
				Name: ptr.To("k8s-azure-lb_allow_IPv4_b5ae07e8a4177ea2d37162cdf2badf8b"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("bar", "foo"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2"),
					DestinationPortRanges:      to.SliceOfPtrs("443"),
					Priority:                   ptr.To(int32(502)),
				},
			},
		}, outputSG.Properties.SecurityRules)
	})
}

func TestSecurityGroupHelper_SecurityGroup(t *testing.T) {
	fx := fixture.NewFixture()
	t.Run("when no rule applied, it should return the original security group", func(t *testing.T) {
		var (
			rules  = fx.Azure().NoiseSecurityRules()
			sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
			helper = ExpectNewSecurityGroupHelper(t, sg)
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
			helper = ExpectNewSecurityGroupHelper(t, sg)
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

			rules[0].Properties.SourceAddressPrefixes = to.SliceOfPtrs(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup + 1)...)

			var (
				sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
				helper = ExpectNewSecurityGroupHelper(t, sg)
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

			rules[0].Properties.SourceAddressPrefixes = to.SliceOfPtrs(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup / 2)...)
			rules[1].Properties.SourceAddressPrefixes = to.SliceOfPtrs(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup/2 + 1)...)

			var (
				sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
				helper = ExpectNewSecurityGroupHelper(t, sg)
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

			rules[0].Properties.DestinationAddressPrefixes = to.SliceOfPtrs(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup + 1)...)

			var (
				sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
				helper = ExpectNewSecurityGroupHelper(t, sg)
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

			rules[0].Properties.DestinationAddressPrefixes = to.SliceOfPtrs(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup / 2)...)
			rules[1].Properties.DestinationAddressPrefixes = to.SliceOfPtrs(fx.RandomIPv4PrefixStrings(MaxSecurityRuleSourceIPsPerGroup/2 + 1)...)

			var (
				sg     = fx.Azure().SecurityGroup().WithRules(rules).Build()
				helper = ExpectNewSecurityGroupHelper(t, sg)
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
			GenerateAllowSecurityRuleName(armnetwork.SecurityRuleProtocolTCP, ipFamily, srcPrefixes, dstPorts):      true,
			GenerateAllowSecurityRuleName(armnetwork.SecurityRuleProtocolUDP, ipFamily, srcPrefixes, dstPorts):      true,
			GenerateAllowSecurityRuleName(armnetwork.SecurityRuleProtocolAsterisk, ipFamily, srcPrefixes, dstPorts): true,
		}, 3)
	})
	t.Run("should be IPFamily-specific", func(t *testing.T) {
		var (
			protocol    = armnetwork.SecurityRuleProtocolTCP
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
			protocol = armnetwork.SecurityRuleProtocolTCP
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
			protocol    = armnetwork.SecurityRuleProtocolTCP
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
