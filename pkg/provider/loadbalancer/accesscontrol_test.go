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
	"net/netip"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/internal/testutil"
	"sigs.k8s.io/cloud-provider-azure/internal/testutil/fixture"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/fnutil"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/iputil"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/securitygroup"
)

func TestAccessControl_IsAllowFromInternet(t *testing.T) {

	var (
		azureFx = fixture.NewFixture().Azure()
		k8sFx   = fixture.NewFixture().Kubernetes()
	)
	tests := []struct {
		name           string
		svc            v1.Service
		expectedOutput bool
	}{
		{
			name:           "public LB with no access control configuration",
			svc:            k8sFx.Service().Build(),
			expectedOutput: true,
		},
		{
			name:           "internal LB with no access control configuration",
			svc:            k8sFx.Service().WithInternalEnabled().Build(),
			expectedOutput: false,
		},
		{
			name:           "public LB with invalid access control configuration",
			svc:            k8sFx.Service().WithLoadBalancerSourceRanges("10.10.10.1/24").Build(),
			expectedOutput: false,
		},
		{
			name: "internal LB with invalid access control configuration",
			svc: k8sFx.Service().WithInternalEnabled().
				WithLoadBalancerSourceRanges("10.10.10.1/24").
				Build(),
			expectedOutput: false,
		},
		{
			name:           "public LB with spec.LoadBalancerSourceRanges specified but not allow all",
			svc:            k8sFx.Service().WithLoadBalancerSourceRanges("10.10.10.0/24").Build(),
			expectedOutput: false,
		},
		{
			name:           "public LB with spec.LoadBalancerSourceRanges specified and allow all",
			svc:            k8sFx.Service().WithLoadBalancerSourceRanges("0.0.0.0/0").Build(),
			expectedOutput: true,
		},
		{
			name:           "public LB with annotation allowedIPRanges specified but not allow all",
			svc:            k8sFx.Service().WithAllowedIPRanges("10.10.10.0/24").Build(),
			expectedOutput: false,
		},
		{
			name:           "public LB with annotation allowedIPRanges specified and allow all",
			svc:            k8sFx.Service().WithAllowedIPRanges("0.0.0.0/0").Build(),
			expectedOutput: true,
		},
		{
			name: "public LB with annotation allowedIPRanges and allowedServiceTags specified and allow all",
			svc: k8sFx.Service().WithAllowedIPRanges("0.0.0.0/0").
				WithAllowedServiceTags(azureFx.ServiceTag()).
				Build(),
			expectedOutput: false,
		},
		{
			name: "internal LB with annotation allowedIPRanges specified",
			svc: k8sFx.Service().WithAllowedIPRanges("0.0.0.0/0").
				WithInternalEnabled().
				Build(),
			expectedOutput: true,
		},
		{
			name: "internal LB with spec.LoadBalancerSourceRanges specified",
			svc: k8sFx.Service().WithLoadBalancerSourceRanges("0.0.0.0/0").
				WithInternalEnabled().
				Build(),
			expectedOutput: true,
		},
		{
			name: "internal LB with annotation allowedIPRanges and allowedServiceTags specified",
			svc: k8sFx.Service().WithLoadBalancerSourceRanges("0.0.0.0/0").
				WithAllowedServiceTags(azureFx.ServiceTag()).
				WithInternalEnabled().
				Build(),
			expectedOutput: false,
		},
	}

	for i := range tests {
		tt := tests[i]
		sg := azureFx.SecurityGroup().WithRules(azureFx.NoiseSecurityRules()).Build()
		ac, err := NewAccessControl(log.Noop(), &tt.svc, sg)
		assert.NoError(t, err)
		actual := ac.IsAllowFromInternet()
		assert.Equal(t, tt.expectedOutput, actual, "[%s] expecting IsAllowFromInternet returns %v", tt.name, tt.expectedOutput)
	}
}

func TestNewAccessControl(t *testing.T) {
	var (
		azureFx = fixture.NewFixture().Azure()
		k8sFx   = fixture.NewFixture().Kubernetes()
		sg      = azureFx.SecurityGroup().WithRules(azureFx.NoiseSecurityRules()).Build()
	)

	t.Run("it should return error if using both spec.LoadBalancerSourceRanges and service annotation service.beta.kubernetes.io/azure-allowed-ip-ranges", func(t *testing.T) {
		svc := k8sFx.Service().
			WithLoadBalancerSourceRanges("20.0.0.1/32").
			WithAllowedIPRanges("10.0.0.1/32").
			Build()

		_, err := NewAccessControl(log.Noop(), &svc, sg)
		assert.ErrorIs(t, err, ErrSetBothLoadBalancerSourceRangesAndAllowedIPRanges)
	})
}

func TestAccessControl_DenyAllExceptSourceRanges(t *testing.T) {

	var (
		azureFx = fixture.NewFixture().Azure()
		k8sFx   = fixture.NewFixture().Kubernetes()
	)

	tests := []struct {
		name           string
		svc            v1.Service
		expectedOutput bool
	}{
		{
			name:           "empty service",
			svc:            k8sFx.Service().Build(),
			expectedOutput: false,
		},
		{
			name:           "with annotation specified but no allowed IP ranges specified",
			svc:            k8sFx.Service().WithDenyAllExceptLoadBalancerSourceRanges().Build(),
			expectedOutput: false,
		},
		{
			name: "with annotation and spec.loadBalancerSourceRanges specified",
			svc: k8sFx.Service().
				WithDenyAllExceptLoadBalancerSourceRanges().
				WithLoadBalancerSourceRanges("10.0.0.1/32").
				Build(),
			expectedOutput: true,
		},
		{
			name: "with annotation and allowedIPRanges specified",
			svc: k8sFx.Service().
				WithDenyAllExceptLoadBalancerSourceRanges().
				WithAllowedIPRanges("10.0.0.1/32").
				Build(),
			expectedOutput: true,
		},
		{
			name:           "without annotation but invalid allowedIPRanges specified",
			svc:            k8sFx.Service().WithAllowedIPRanges("10.0.0.1/24").Build(),
			expectedOutput: true,
		},
	}

	for i := range tests {
		tt := tests[i]
		sg := azureFx.SecurityGroup().Build()
		ac, err := NewAccessControl(log.Noop(), &tt.svc, sg)
		assert.NoError(t, err)
		actual := ac.DenyAllExceptSourceRanges()
		assert.Equal(t, tt.expectedOutput, actual, "[%s] expecting DenyAllExceptSourceRanges returns %v", tt.name, tt.expectedOutput)
	}
}

func TestAccessControl_AllowedRanges(t *testing.T) {

	var (
		azureFx = fixture.NewFixture().Azure()
		k8sFx   = fixture.NewFixture().Kubernetes()
	)

	tests := []struct {
		name         string
		svc          v1.Service
		expectedIPv4 []string
		expectedIPv6 []string
	}{
		{
			name:         "empty service",
			svc:          k8sFx.Service().Build(),
			expectedIPv4: nil,
			expectedIPv6: nil,
		},
		{
			name:         "spec.LoadBalancerSourceRanges = 1 IPv4",
			svc:          k8sFx.Service().WithLoadBalancerSourceRanges("10.10.0.0/16").Build(),
			expectedIPv4: []string{"10.10.0.0/16"},
			expectedIPv6: nil,
		},
		{
			name: "spec.LoadBalancerSourceRanges = N IPv4",
			svc: k8sFx.Service().WithLoadBalancerSourceRanges(
				"10.10.0.0/16",
				"10.11.0.0/16",
				"10.0.0.1/32",
			).Build(),
			expectedIPv4: []string{
				"10.10.0.0/16",
				"10.11.0.0/16",
				"10.0.0.1/32",
			},
		},
		{
			name: "spec.LoadBalancerSourceRanges = 1 IPv6",
			svc: k8sFx.Service().WithLoadBalancerSourceRanges(
				"2001:db8:85a3::/64",
			).Build(),
			expectedIPv6: []string{
				"2001:db8:85a3::/64",
			},
		},
		{
			name: "spec.LoadBalancerSourceRanges = N IPv6",
			svc: k8sFx.Service().WithLoadBalancerSourceRanges(
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"fe80:1234::/32",
			).Build(),
			expectedIPv6: []string{
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"fe80:1234::/32",
			},
		},
		{
			name: "spec.LoadBalancerSourceRanges = N IPv4 and N IPv6",
			svc: k8sFx.Service().WithLoadBalancerSourceRanges(
				"10.10.0.0/16",
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"10.11.0.0/16",
				"10.0.0.1/32",
				"fe80:1234:abcd::1c2d:3e4f/128",
			).Build(),
			expectedIPv4: []string{
				"10.10.0.0/16",
				"10.11.0.0/16",
				"10.0.0.1/32",
			},
			expectedIPv6: []string{
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"fe80:1234:abcd::1c2d:3e4f/128",
			},
		},
		{
			name: "spec.LoadBalancerSourceRanges = N IPv4 and N IPv6 and invalid ranges",
			svc: k8sFx.Service().WithLoadBalancerSourceRanges(
				"10.10.0.0/16",
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"10.11.0.0/16",
				"10.0.0.1/32",
				"fe80:1234:abcd::1c2d:3e4f/128",
				"20.10.0.1/16",                 // invalid
				"fe80:1234:abcd::1c2d:3e4f/64", // invalid
			).Build(),
			expectedIPv4: []string{
				"10.10.0.0/16",
				"10.11.0.0/16",
				"10.0.0.1/32",
			},
			expectedIPv6: []string{
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"fe80:1234:abcd::1c2d:3e4f/128",
			},
		},
		{
			name: "annotation allowedIPRanges = 1 IPv4",
			svc: k8sFx.Service().WithAllowedIPRanges(
				"10.10.0.0/16",
			).Build(),
			expectedIPv4: []string{"10.10.0.0/16"},
			expectedIPv6: nil,
		},
		{
			name: "annotation allowedIPRanges = N IPv4",
			svc: k8sFx.Service().WithAllowedIPRanges(
				"10.10.0.0/16",
				"10.11.0.0/16",
				"10.0.0.1/32",
			).Build(),
			expectedIPv4: []string{
				"10.10.0.0/16",
				"10.11.0.0/16",
				"10.0.0.1/32",
			},
		},
		{
			name: "annotation allowedIPRanges = 1 IPv6",
			svc: k8sFx.Service().WithAllowedIPRanges(
				"2001:db8:85a3::/64",
			).Build(),
			expectedIPv6: []string{
				"2001:db8:85a3::/64",
			},
		},
		{
			name: "annotation allowedIPRanges = N IPv6",
			svc: k8sFx.Service().WithAllowedIPRanges(
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"fe80:1234:abcd::1c2d:3e4f/128",
			).Build(),
			expectedIPv6: []string{
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"fe80:1234:abcd::1c2d:3e4f/128",
			},
		},
		{
			name: "annotation allowedIPRanges = N IPv4 and N IPv6",
			svc: k8sFx.Service().WithAllowedIPRanges(
				"10.10.0.0/16",
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"10.11.0.0/16",
				"10.0.0.1/32",
				"fe80:1234:abcd::1c2d:3e4f/128",
			).Build(),
			expectedIPv4: []string{
				"10.10.0.0/16",
				"10.11.0.0/16",
				"10.0.0.1/32",
			},
			expectedIPv6: []string{
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"fe80:1234:abcd::1c2d:3e4f/128",
			},
		},
		{
			name: "annotation allowedIPRanges = N IPv4 and N IPv6 with invalid ranges",
			svc: k8sFx.Service().WithAllowedIPRanges(
				"10.10.0.0/16",
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"10.11.0.0/16",
				"10.0.0.1/32",
				"fe80:1234:abcd::1c2d:3e4f/128",
				"20.10.0.1/16",                 // invalid
				"fe80:1234:abcd::1c2d:3e4f/64", // invalid
			).Build(),
			expectedIPv4: []string{
				"10.10.0.0/16",
				"10.11.0.0/16",
				"10.0.0.1/32",
			},
			expectedIPv6: []string{
				"2001:db8:85a3::/64",
				"fd12:3456:789a::/48",
				"fe80:1234:abcd::1c2d:3e4f/128",
			},
		},
	}

	for i := range tests {
		tt := tests[i]
		sg := azureFx.SecurityGroup().Build()
		ac, err := NewAccessControl(log.Noop(), &tt.svc, sg)
		assert.NoError(t, err)
		var (
			ipv4         = ac.AllowedIPv4Ranges()
			ipv6         = ac.AllowedIPv6Ranges()
			expectedIPv4 = fnutil.Map(func(s string) netip.Prefix {
				return netip.MustParsePrefix(s)
			}, tt.expectedIPv4)
			expectedIPv6 = fnutil.Map(func(s string) netip.Prefix {
				return netip.MustParsePrefix(s)
			}, tt.expectedIPv6)
		)
		if ipv4 == nil {
			ipv4 = []netip.Prefix{}
		}

		if ipv6 == nil {
			ipv6 = []netip.Prefix{}
		}

		assert.Equal(t, expectedIPv4, ipv4, "[%s] expecting AllowedIPv4Ranges returns %v", tt.name, tt.expectedIPv4)
		assert.Equal(t, expectedIPv6, ipv6, "[%s] expecting AllowedIPv6Ranges returns %v", tt.name, tt.expectedIPv6)
	}
}

func TestAccessControl_PatchSecurityGroup(t *testing.T) {
	var (
		azureFx = fixture.NewFixture().Azure()
	)

	runTest := func(t *testing.T,
		svc v1.Service,
		originalRules []*armnetwork.SecurityRule,
		dstIPv4Addresses, dstIPv6Addresses []string,
		expectedUpdated bool,
		expectedRules []*armnetwork.SecurityRule,
	) {
		t.Helper()

		var (
			sg      = azureFx.SecurityGroup().WithRules(originalRules).Build()
			ac, err = NewAccessControl(log.Noop(), &svc, sg)
		)
		assert.NoError(t, err)

		err = ac.PatchSecurityGroup(
			fnutil.Map(func(s string) netip.Addr { return netip.MustParseAddr(s) }, dstIPv4Addresses),
			fnutil.Map(func(s string) netip.Addr { return netip.MustParseAddr(s) }, dstIPv6Addresses),
		)
		assert.NoError(t, err)

		actualSG, updated, err := ac.SecurityGroup()
		assert.NoError(t, err)
		assert.Equal(t, expectedUpdated, updated)
		testutil.ExpectHasSecurityRules(t, actualSG, expectedRules)
	}

	t.Run("patch service without access control configuration", func(t *testing.T) {
		var (
			k8sFx            = fixture.NewFixture().Kubernetes()
			svc              = k8sFx.Service().Build()
			originalRules    = azureFx.NoiseSecurityRules()
			serviceTags      = []string{securitygroup.ServiceTagInternet}
			dstIPv4Addresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
			dstIPv6Addresses = []string{
				"2001:db8::1428:57ab",
				"2002:fb8::1",
			}
			expectedRules = testutil.CloneInJSON(originalRules)
		)
		expectedRules = append(expectedRules,
			// TCP + IPv4 for Internet
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPPorts(),
				).
				WithPriority(500).
				WithDestination(dstIPv4Addresses...).
				Build(),
			// TCP + IPv6 for Internet
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPPorts(),
				).
				WithPriority(501).
				WithDestination(dstIPv6Addresses...).
				Build(),
			// UDP + IPv4 for Internet
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPPorts(),
				).
				WithPriority(502).
				WithDestination(dstIPv4Addresses...).
				Build(),
			// UDP + IPv6 for Internet
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPPorts(),
				).
				WithPriority(503).
				WithDestination(dstIPv6Addresses...).
				Build(),
		)

		runTest(t, svc, originalRules, dstIPv4Addresses, dstIPv6Addresses, true, expectedRules)
	})

	t.Run("patch service with allowedServiceTag", func(t *testing.T) {
		var (
			k8sFx        = fixture.NewFixture().Kubernetes()
			nServiceTags = 2
			serviceTags  = azureFx.ServiceTags(nServiceTags)
			svc          = k8sFx.Service().WithAllowedServiceTags(serviceTags...).
					Build()
			originalRules    = azureFx.NoiseSecurityRules()
			dstIPv4Addresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
			dstIPv6Addresses = []string{
				"2001:db8::1428:57ab",
				"2002:fb8::1",
			}
			expectedRules = testutil.CloneInJSON(originalRules)
		)
		expectedRules = append(expectedRules,
			// TCP + IPv4 for 2 service tags
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{serviceTags[0]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(500).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{serviceTags[1]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(501).
				WithDestination(dstIPv4Addresses...).
				Build(),
			// TCP + IPv6 for 2 service tags
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{serviceTags[0]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(502).
				WithDestination(dstIPv6Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{serviceTags[1]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(503).
				WithDestination(dstIPv6Addresses...).
				Build(),
			// UDP + IPv4 for 2 service tags
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{serviceTags[0]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(504).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{serviceTags[1]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(505).
				WithDestination(dstIPv4Addresses...).
				Build(),
			// UDP + IPv6 for 2 service tags
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{serviceTags[0]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(506).
				WithDestination(dstIPv6Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{serviceTags[1]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(507).
				WithDestination(dstIPv6Addresses...).
				Build(),
		)
		runTest(t, svc, originalRules, dstIPv4Addresses, dstIPv6Addresses, true, expectedRules)
	})

	t.Run("patch service with allowedIPRanges on IPv4", func(t *testing.T) {
		var (
			k8sFx           = fixture.NewFixture().Kubernetes()
			allowedIPRanges = []string{
				"192.168.0.0/16",
				"20.0.0.1/32",
			}
			svc              = k8sFx.Service().WithAllowedIPRanges(allowedIPRanges...).Build()
			originalRules    = azureFx.NoiseSecurityRules()
			dstIPv4Addresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
			dstIPv6Addresses = []string{
				"2001:db8::1428:57ab",
				"2002:fb8::1",
			}
			expectedRules = testutil.CloneInJSON(originalRules)
		)
		expectedRules = append(expectedRules,
			// TCP
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPRanges, k8sFx.Service().TCPPorts(),
				).
				WithPriority(500).
				WithDestination(dstIPv4Addresses...).
				Build(),
			// UDP
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPRanges, k8sFx.Service().UDPPorts(),
				).
				WithPriority(501).
				WithDestination(dstIPv4Addresses...).
				Build(),
		)
		runTest(t, svc, originalRules, dstIPv4Addresses, dstIPv6Addresses, true, expectedRules)
	})

	t.Run("patch service with allowedIPRanges on IPv6", func(t *testing.T) {
		var (
			k8sFx           = fixture.NewFixture().Kubernetes()
			allowedIPRanges = []string{
				"fd12:3456:789a::/48",
				"fe80:1234:abcd::1c2d:3e4f/128",
			}
			svc = k8sFx.Service().
				WithAllowedIPRanges(allowedIPRanges...).
				Build()
			originalRules    = azureFx.NoiseSecurityRules()
			dstIPv4Addresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
			dstIPv6Addresses = []string{
				"2001:db8::1428:57ab",
				"2002:fb8::1",
			}
			expectedRules = testutil.CloneInJSON(originalRules)
		)
		expectedRules = append(expectedRules,
			// TCP
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPRanges, k8sFx.Service().TCPPorts(),
				).
				WithPriority(500).
				WithDestination(dstIPv6Addresses...).
				Build(),
			// UDP
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPRanges, k8sFx.Service().UDPPorts(),
				).
				WithPriority(501).
				WithDestination(dstIPv6Addresses...).
				Build(),
		)
		runTest(t, svc, originalRules, dstIPv4Addresses, dstIPv6Addresses, true, expectedRules)
	})

	t.Run("patch service with allowedIPRanges on IPv4 and IPv6", func(t *testing.T) {
		var (
			k8sFx             = fixture.NewFixture().Kubernetes()
			allowedIPv4Ranges = []string{
				"192.168.0.0/16",
				"20.0.0.1/32",
			}
			allowedIPv6Ranges = []string{
				"fd12:3456:789a::/48",
				"fe80:1234:abcd::1c2d:3e4f/128",
			}
			svc = k8sFx.Service().
				WithAllowedIPRanges(append(allowedIPv4Ranges, allowedIPv6Ranges...)...).
				Build()
			originalRules    = azureFx.NoiseSecurityRules()
			dstIPv4Addresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
			dstIPv6Addresses = []string{
				"2001:db8::1428:57ab",
				"2002:fb8::1",
			}
			expectedRules = testutil.CloneInJSON(originalRules)
		)
		expectedRules = append(expectedRules,
			// TCP + IPv4
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts(),
				).
				WithPriority(500).
				WithDestination(dstIPv4Addresses...).
				Build(),
			// TCP + IPv6
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts(),
				).
				WithPriority(501).
				WithDestination(dstIPv6Addresses...).
				Build(),
			// UDP + IPv4
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts(),
				).
				WithPriority(502).
				WithDestination(dstIPv4Addresses...).
				Build(),
			// UDP + IPv6
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts(),
				).
				WithPriority(503).
				WithDestination(dstIPv6Addresses...).
				Build(),
		)
		runTest(t, svc, originalRules, dstIPv4Addresses, dstIPv6Addresses, true, expectedRules)
	})

	t.Run("patch service with allowedIPRanges and allowedServiceTags", func(t *testing.T) {
		var (
			k8sFx              = fixture.NewFixture().Kubernetes()
			nServiceTags       = 2
			allowedServiceTags = azureFx.ServiceTags(nServiceTags)
			allowedIPv4Ranges  = []string{
				"192.168.0.0/16",
				"20.0.0.1/32",
			}
			allowedIPv6Ranges = []string{
				"fd12:3456:789a::/48",
				"fe80:1234:abcd::1c2d:3e4f/128",
			}
			svc = k8sFx.Service().
				WithAllowedIPRanges(append(allowedIPv4Ranges, allowedIPv6Ranges...)...).
				WithAllowedServiceTags(allowedServiceTags...).
				Build()
			originalRules    = azureFx.NoiseSecurityRules()
			dstIPv4Addresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
			dstIPv6Addresses = []string{
				"2001:db8::1428:57ab",
				"2002:fb8::1",
			}
			expectedRules = testutil.CloneInJSON(originalRules)
		)
		expectedRules = append(expectedRules,
			// TCP + IPv4 for 2 service Tags + 1 IP Ranges
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(500).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(501).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts(),
				).
				WithPriority(502).
				WithDestination(dstIPv4Addresses...).
				Build(),

			// TCP + IPv6 for 2 service Tags + 1 IP Ranges
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(503).
				WithDestination(dstIPv6Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(504).
				WithDestination(dstIPv6Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts(),
				).
				WithPriority(505).
				WithDestination(dstIPv6Addresses...).
				Build(),

			// UDP + IPv4 for 2 service Tags + 1 IP Ranges
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(506).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(507).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts(),
				).
				WithPriority(508).
				WithDestination(dstIPv4Addresses...).
				Build(),

			// UDP + IPv6 for 2 service Tags + 1 IP Ranges
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(509).
				WithDestination(dstIPv6Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(510).
				WithDestination(dstIPv6Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts(),
				).
				WithPriority(511).
				WithDestination(dstIPv6Addresses...).
				Build(),
		)
		runTest(t, svc, originalRules, dstIPv4Addresses, dstIPv6Addresses, true, expectedRules)
	})

	t.Run("patch service with allowedIPRanges and allowedServiceTags and deny vnet traffic", func(t *testing.T) {
		var (
			k8sFx              = fixture.NewFixture().Kubernetes()
			nServiceTags       = 2
			allowedServiceTags = azureFx.ServiceTags(nServiceTags)
			allowedIPv4Ranges  = []string{
				"192.168.0.0/16",
				"20.0.0.1/32",
			}
			allowedIPv6Ranges = []string{
				"fd12:3456:789a::/48",
				"fe80:1234:abcd::1c2d:3e4f/128",
			}
			svc = k8sFx.Service().
				WithAllowedIPRanges(append(allowedIPv4Ranges, allowedIPv6Ranges...)...).
				WithAllowedServiceTags(allowedServiceTags...).
				WithDenyAllExceptLoadBalancerSourceRanges().
				Build()
			originalRules    = azureFx.NoiseSecurityRules()
			dstIPv4Addresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
			dstIPv6Addresses = []string{
				"2001:db8::1428:57ab",
				"2002:fb8::1",
			}
			expectedRules = testutil.CloneInJSON(originalRules)
		)
		expectedRules = append(expectedRules,

			// TCP + IPv4 for 2 service Tags + 1 IP Ranges
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(500).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(501).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts(),
				).
				WithPriority(502).
				WithDestination(dstIPv4Addresses...).
				Build(),

			// TCP + IPv6 for 2 service Tags + 1 IP Ranges
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(503).
				WithDestination(dstIPv6Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts(),
				).
				WithPriority(504).
				WithDestination(dstIPv6Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts(),
				).
				WithPriority(505).
				WithDestination(dstIPv6Addresses...).
				Build(),

			// UDP + IPv4 for 2 service Tags + 1 IP Ranges
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(506).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(507).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts(),
				).
				WithPriority(508).
				WithDestination(dstIPv4Addresses...).
				Build(),

			// UDP + IPv6 for 2 service Tags + 1 IP Ranges
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(509).
				WithDestination(dstIPv6Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts(),
				).
				WithPriority(510).
				WithDestination(dstIPv6Addresses...).
				Build(),
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts(),
				).
				WithPriority(511).
				WithDestination(dstIPv6Addresses...).
				Build(),

			// Deny ALL
			azureFx.
				DenyAllSecurityRule(iputil.IPv4).WithPriority(4095).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				DenyAllSecurityRule(iputil.IPv6).WithPriority(4094).
				WithDestination(dstIPv6Addresses...).
				Build(),
		)
		runTest(t, svc, originalRules, dstIPv4Addresses, dstIPv6Addresses, true, expectedRules)
	})

	t.Run("patch service with invalid allowedIPRanges", func(t *testing.T) {
		var (
			k8sFx           = fixture.NewFixture().Kubernetes()
			inputIPv4Ranges = []string{
				"192.168.0.0/16",
				"20.0.0.1/32",
				"10.0.0.1/16", // invalid
			}
			allowedIPv4Ranges = inputIPv4Ranges[:2]
			inputIPv6Ranges   = []string{
				"fd12:3456:789a::/48",
				"fe80:1234:abcd::1c2d:3e4f/128",
				"fe80:1234:abcd::3e4f/64", // invalid
			}
			allowedIPv6Ranges = inputIPv6Ranges[:2]
			svc               = k8sFx.Service().
						WithAllowedIPRanges(append(inputIPv4Ranges, inputIPv6Ranges...)...).
						WithDenyAllExceptLoadBalancerSourceRanges().
						Build()
			originalRules    = azureFx.NoiseSecurityRules()
			dstIPv4Addresses = []string{
				"10.0.0.1",
				"10.0.0.2",
			}
			dstIPv6Addresses = []string{
				"2001:db8::1428:57ab",
				"2002:fb8::1",
			}
			expectedRules = testutil.CloneInJSON(originalRules)
		)
		expectedRules = append(expectedRules,

			// TCP + IPv4 for 1 IP Range
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts(),
				).
				WithPriority(500).
				WithDestination(dstIPv4Addresses...).
				Build(),

			// TCP + IPv6 for 1 IP Range
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts(),
				).
				WithPriority(501).
				WithDestination(dstIPv6Addresses...).
				Build(),

			// UDP + IPv4 for 1 IP Range
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts(),
				).
				WithPriority(502).
				WithDestination(dstIPv4Addresses...).
				Build(),

			// UDP + IPv6 for 1 IP Range
			azureFx.
				AllowSecurityRule(
					armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts(),
				).
				WithPriority(503).
				WithDestination(dstIPv6Addresses...).
				Build(),

			// Deny ALL
			azureFx.
				DenyAllSecurityRule(iputil.IPv4).WithPriority(4095).
				WithDestination(dstIPv4Addresses...).
				Build(),
			azureFx.
				DenyAllSecurityRule(iputil.IPv6).WithPriority(4094).
				WithDestination(dstIPv6Addresses...).
				Build(),
		)
		runTest(t, svc, originalRules, dstIPv4Addresses, dstIPv6Addresses, true, expectedRules)
	})
}

func TestAccessControl_CleanSecurityGroup(t *testing.T) {

	var (
		fx      = fixture.NewFixture()
		azureFx = fx.Azure()
	)

	t.Run("it should not patch rules if no rules exist", func(t *testing.T) {
		var (
			sg      = azureFx.SecurityGroup().Build()
			svc     = fx.Kubernetes().Service().Build()
			ac, err = NewAccessControl(log.Noop(), &svc, sg)
		)
		assert.NoError(t, err)

		assert.NoError(t, ac.CleanSecurityGroup(fx.RandomIPv4Addresses(2), fx.RandomIPv6Addresses(2), make(map[armnetwork.SecurityRuleProtocol][]int32)))
		_, updated, err := ac.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("it should not patch rules if no rules match", func(t *testing.T) {
		var (
			rules = []*armnetwork.SecurityRule{
				{
					Name: ptr.To("test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2"),
						DestinationPortRanges:      to.SliceOfPtrs("80", "443"),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("test-rule-1"),
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

			sg               = azureFx.SecurityGroup().WithRules(rules).Build()
			dstIPv4Addresses = []netip.Addr{
				netip.MustParseAddr("192.168.0.1"),
				netip.MustParseAddr("192.168.0.2"),
			}
			svc     = fx.Kubernetes().Service().Build()
			ac, err = NewAccessControl(log.Noop(), &svc, sg)
		)
		assert.NoError(t, err)

		assert.NoError(t, ac.CleanSecurityGroup(dstIPv4Addresses, nil, make(map[armnetwork.SecurityRuleProtocol][]int32)))
		_, updated, err := ac.SecurityGroup()
		assert.NoError(t, err)
		assert.False(t, updated)
	})

	t.Run("it should patch the matched rules", func(t *testing.T) {
		var (
			rules = []*armnetwork.SecurityRule{
				{
					Name: ptr.To("test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2", "192.168.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("80", "443"),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("test-rule-1"),
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
					Name: ptr.To("test-rule-2"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("8.8.8.8"),
						DestinationPortRanges:      to.SliceOfPtrs("5000"),
						Priority:                   ptr.To(int32(502)),
					},
				},
			}

			sg               = azureFx.SecurityGroup().WithRules(rules).Build()
			dstIPv4Addresses = []netip.Addr{
				netip.MustParseAddr("192.168.0.1"),
				netip.MustParseAddr("192.168.0.2"),
			}
			svc     = fx.Kubernetes().Service().Build()
			ac, err = NewAccessControl(log.Noop(), &svc, sg)
		)
		assert.NoError(t, err)

		assert.NoError(t, ac.CleanSecurityGroup(dstIPv4Addresses, nil, make(map[armnetwork.SecurityRuleProtocol][]int32)))
		outputSG, updated, err := ac.SecurityGroup()
		assert.NoError(t, err)
		assert.True(t, updated)

		testutil.ExpectEqualInJSON(t, []*armnetwork.SecurityRule{
			{
				Name: ptr.To("test-rule-0"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2"),
					DestinationPortRanges:      to.SliceOfPtrs("80", "443"),
					Priority:                   ptr.To(int32(500)),
				},
			},
			{
				Name: ptr.To("test-rule-1"),
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
				Name: ptr.To("test-rule-2"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("*"),
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
					Name: ptr.To("test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2", "192.168.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("80", "443"),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("test-rule-1"),
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
					Name: ptr.To("test-rule-2"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("8.8.8.8"),
						DestinationPortRanges:      to.SliceOfPtrs("5000"),
						Priority:                   ptr.To(int32(502)),
					},
				},
				{
					Name: ptr.To("test-rule-3"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                 to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						Access:                   to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:    to.SliceOfPtrs("*"),
						SourcePortRange:          ptr.To("*"),
						DestinationAddressPrefix: ptr.To("192.168.0.1"),
						DestinationPortRanges:    to.SliceOfPtrs("8000"),
						Priority:                 ptr.To(int32(2000)),
					},
				},
			}

			sg               = azureFx.SecurityGroup().WithRules(rules).Build()
			dstIPv4Addresses = []netip.Addr{
				netip.MustParseAddr("192.168.0.1"),
				netip.MustParseAddr("192.168.0.2"),
			}
			svc     = fx.Kubernetes().Service().Build()
			ac, err = NewAccessControl(log.Noop(), &svc, sg)
		)
		assert.NoError(t, err)

		assert.NoError(t, ac.CleanSecurityGroup(dstIPv4Addresses, nil, make(map[armnetwork.SecurityRuleProtocol][]int32)))
		outputSG, updated, err := ac.SecurityGroup()
		assert.NoError(t, err)

		assert.True(t, updated)
		testutil.ExpectEqualInJSON(t, []*armnetwork.SecurityRule{
			{
				Name: ptr.To("test-rule-0"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2"),
					DestinationPortRanges:      to.SliceOfPtrs("80", "443"),
					Priority:                   ptr.To(int32(500)),
				},
			},
			{
				Name: ptr.To("test-rule-2"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("*"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("8.8.8.8"),
					DestinationPortRanges:      to.SliceOfPtrs("5000"),
					Priority:                   ptr.To(int32(502)),
				},
			},
		}, outputSG.Properties.SecurityRules)
	})

	t.Run("it should split rules if retainPorts is set", func(t *testing.T) {
		var (
			rules = []*armnetwork.SecurityRule{
				{
					Name: ptr.To("test-rule-0"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2", "192.168.0.1"),
						DestinationPortRanges:      to.SliceOfPtrs("80", "443"),
						Priority:                   ptr.To(int32(500)),
					},
				},
				{
					Name: ptr.To("test-rule-1"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("src_baz", "src_quo"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("20.0.0.1", "192.168.0.1", "192.168.0.2", "20.0.0.2"),
						DestinationPortRanges:      to.SliceOfPtrs("53", "54", "55", "56"),
						Priority:                   ptr.To(int32(501)),
					},
				},
				{
					Name: ptr.To("test-rule-2"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourceAddressPrefixes:      to.SliceOfPtrs("*"),
						SourcePortRange:            ptr.To("*"),
						DestinationAddressPrefixes: to.SliceOfPtrs("8.8.8.8"),
						DestinationPortRanges:      to.SliceOfPtrs("5000"),
						Priority:                   ptr.To(int32(502)),
					},
				},
			}

			sg               = azureFx.SecurityGroup().WithRules(rules).Build()
			dstIPv4Addresses = []netip.Addr{
				netip.MustParseAddr("192.168.0.1"),
				netip.MustParseAddr("192.168.0.2"),
			}
			svc     = fx.Kubernetes().Service().Build()
			ac, err = NewAccessControl(log.Noop(), &svc, sg)
		)
		assert.NoError(t, err)

		assert.NoError(t, ac.CleanSecurityGroup(dstIPv4Addresses, nil, map[armnetwork.SecurityRuleProtocol][]int32{
			armnetwork.SecurityRuleProtocolUDP: {56, 53},
		}))
		outputSG, updated, err := ac.SecurityGroup()
		assert.NoError(t, err)
		assert.True(t, updated)

		testutil.ExpectEqualInJSON(t, []*armnetwork.SecurityRule{
			{
				Name: ptr.To("test-rule-0"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("src_foo", "src_bar"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("10.0.0.1", "10.0.0.2"),
					DestinationPortRanges:      to.SliceOfPtrs("80", "443"),
					Priority:                   ptr.To(int32(500)),
				},
			},
			{
				Name: ptr.To("test-rule-1"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("src_baz", "src_quo"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("20.0.0.1", "20.0.0.2"),
					DestinationPortRanges:      to.SliceOfPtrs("53", "54", "55", "56"),
					Priority:                   ptr.To(int32(501)),
				},
			},
			{
				Name: ptr.To("test-rule-2"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolAsterisk),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("*"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("8.8.8.8"),
					DestinationPortRanges:      to.SliceOfPtrs("5000"),
					Priority:                   ptr.To(int32(502)),
				},
			},
			{
				Name: ptr.To("k8s-azure-lb_allow_IPv4_648b18e18a92d1a4b415033da37c79a5"),
				Properties: &armnetwork.SecurityRulePropertiesFormat{
					Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP),
					Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
					Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
					SourceAddressPrefixes:      to.SliceOfPtrs("src_baz", "src_quo"),
					SourcePortRange:            ptr.To("*"),
					DestinationAddressPrefixes: to.SliceOfPtrs("192.168.0.1", "192.168.0.2"),
					DestinationPortRanges:      to.SliceOfPtrs("53", "56"), // 53 and 56 are retained
					Priority:                   ptr.To(int32(503)),
				},
			},
		}, outputSG.Properties.SecurityRules)
	})
}
