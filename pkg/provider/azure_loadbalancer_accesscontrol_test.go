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

package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/loadbalancerclient/mockloadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/securitygroupclient/mocksecuritygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/iputil"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/securitygroup"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/testutil"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer/testutil/fixture"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestCloud_reconcileSecurityGroup(t *testing.T) {
	const (
		EnsureLB    = true
		ClusterName = "test-cluster"
	)

	var (
		fx      = fixture.NewFixture()
		k8sFx   = fx.Kubernetes()
		azureFx = fx.Azure()
	)

	t.Run("internal Load Balancer", func(t *testing.T) {
		t.Run("noop when no allow list specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			svc := k8sFx.Service().WithInternalEnabled().Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			sg, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
			testutil.ExpectEqualInJSON(t, azureFx.SecurityGroup().Build(), sg)
		})

		t.Run("add Internet allow rules if allow all", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			svc := k8sFx.Service().WithInternalEnabled().
				WithAllowedIPRanges("0.0.0.0/0").
				Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
				DoAndReturn(func(
					ctx context.Context,
					resourceGroupName, securityGroupName string,
					properties network.SecurityGroup,
					etag string,
				) *retry.Error {
					serviceTags := []string{securitygroup.ServiceTagInternet}
					rules := []network.SecurityRule{
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{"0.0.0.0/0"}, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{"0.0.0.0/0"}, k8sFx.Service().UDPPorts()).
							WithPriority(504).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPPorts()).
							WithPriority(505).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)
					return nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("add rules with a mix of settings", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			svc := k8sFx.Service().WithInternalEnabled().
				WithAllowedIPRanges("0.0.0.0/0", "8.8.8.8/32").
				WithAllowedServiceTags(azureFx.ServiceTag()).
				Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
				DoAndReturn(func(
					ctx context.Context,
					resourceGroupName, securityGroupName string,
					properties network.SecurityGroup,
					etag string,
				) *retry.Error {
					rules := []network.SecurityRule{
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{azureFx.ServiceTag()}, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{"0.0.0.0/0", "8.8.8.8/32"}, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{azureFx.ServiceTag()}, k8sFx.Service().TCPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{azureFx.ServiceTag()}, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{"0.0.0.0/0", "8.8.8.8/32"}, k8sFx.Service().UDPPorts()).
							WithPriority(504).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, []string{azureFx.ServiceTag()}, k8sFx.Service().UDPPorts()).
							WithPriority(505).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)
					return nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})
	})

	t.Run("public Load Balancer", func(t *testing.T) {
		t.Run("add Internet allow rules if no allow list specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
				DoAndReturn(func(
					ctx context.Context,
					resourceGroupName, securityGroupName string,
					properties network.SecurityGroup,
					etag string,
				) *retry.Error {
					assert.Len(t, *properties.SecurityRules, 4, "expect exact 4 (2 TCP + 2 UDP) rule for allowing Internet")

					serviceTags := []string{securitygroup.ServiceTagInternet}
					rules := []network.SecurityRule{
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)
					return nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})
	})

	t.Run("add rules - when no rules exist", func(t *testing.T) {
		t.Run("with `service.beta.kubernetes.io/azure-additional-public-ips` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			svc.Annotations[consts.ServiceAnnotationAdditionalPublicIPs] = strings.Join(azureFx.LoadBalancer().AdditionalAddresses(), ",")

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
				DoAndReturn(func(
					ctx context.Context,
					resourceGroupName, securityGroupName string,
					properties network.SecurityGroup,
					etag string,
				) *retry.Error {
					assert.Len(t, *properties.SecurityRules, 4, "expect exact 4 rule for allowing Internet")

					var (
						dstIPv4Addresses = append(azureFx.LoadBalancer().IPv4Addresses(), azureFx.LoadBalancer().AdditionalIPv4Addresses()...)
						dstIPv6Addresses = append(azureFx.LoadBalancer().IPv6Addresses(), azureFx.LoadBalancer().AdditionalIPv6Addresses()...)
					)

					serviceTags := []string{securitygroup.ServiceTagInternet}
					rules := []network.SecurityRule{
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(dstIPv4Addresses...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(dstIPv6Addresses...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPPorts()).
							WithPriority(502).
							WithDestination(dstIPv4Addresses...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(dstIPv6Addresses...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)
					return nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			svc.Annotations[consts.ServiceAnnotationDisableLoadBalancerFloatingIP] = "true"

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
				DoAndReturn(func(
					ctx context.Context,
					resourceGroupName, securityGroupName string,
					properties network.SecurityGroup,
					etag string,
				) *retry.Error {
					assert.Len(t, *properties.SecurityRules, 4, "expect exact 4 (2 TCP + 2 UDP) rule for allowing Internet on IPv4 and IPv6")

					serviceTags := []string{securitygroup.ServiceTagInternet}
					rules := []network.SecurityRule{
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPNodePorts()). // use NodePort
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...). // Use backend pool IPs
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPNodePorts()). // use NodePort
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...). // Use backend pool IPs
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPNodePorts()). // use NodePort
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...). // Use backend pool IPs
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPNodePorts()). // use NodePort
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...). // Use backend pool IPs
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)
					return nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-allowed-ip-ranges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			var (
				allowedIPv4Ranges = []string{"172.30.0.0/16", "172.31.0.1/32"}
				allowedIPv6Ranges = []string{"2607:f0d0:1002:51::/64", "fd00::/8"}
			)

			svc.Annotations[consts.ServiceAnnotationAllowedIPRanges] = strings.Join(append(allowedIPv4Ranges, allowedIPv6Ranges...), ",")

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
				DoAndReturn(func(
					ctx context.Context,
					resourceGroupName, securityGroupName string,
					properties network.SecurityGroup,
					etag string,
				) *retry.Error {
					assert.Len(t, *properties.SecurityRules, 4, "expect exact 4 rules for allowing on IPv4 and IPv6")

					rules := []network.SecurityRule{
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)

					return nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-allowed-service-tags` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			var allowedServiceTags = []string{"AzureCloud", "AzureDatabricks"}

			svc.Annotations[consts.ServiceAnnotationAllowedServiceTags] = strings.Join(allowedServiceTags, ",")

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
				DoAndReturn(func(
					ctx context.Context,
					resourceGroupName, securityGroupName string,
					properties network.SecurityGroup,
					etag string,
				) *retry.Error {
					assert.Len(t, *properties.SecurityRules, 8, "<2 service tags> * <2 IP stack> * <2 Protocol[TCP/UDP]>")

					rules := []network.SecurityRule{
						// TCP + IPv4
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),
						// TCP + IPv6
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
						// UDP + IPv4
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts()).
							WithPriority(504).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts()).
							WithPriority(505).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),
						// UDP + IPv6
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts()).
							WithPriority(506).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts()).
							WithPriority(507).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)

					return nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `spec.loadBalancerSourceRanges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			var (
				allowedIPv4Ranges = []string{"172.30.0.0/16", "172.31.0.1/32"}
				allowedIPv6Ranges = []string{"2607:f0d0:1002:51::/64", "fd00::/8"}
			)

			svc.Spec.LoadBalancerSourceRanges = append(allowedIPv4Ranges, allowedIPv6Ranges...)

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
				DoAndReturn(func(
					ctx context.Context,
					resourceGroupName, securityGroupName string,
					properties network.SecurityGroup,
					etag string,
				) *retry.Error {
					assert.Len(t, *properties.SecurityRules, 4, "expect exact 4 rules for allowing on IPv4 and IPv6")

					rules := []network.SecurityRule{
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)

					return nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			var (
				allowedIPv4Ranges = []string{"172.30.0.0/16", "172.31.0.1/32"}
				allowedIPv6Ranges = []string{"2607:f0d0:1002:51::/64", "fd00::/8"}
			)

			svc.Annotations[consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges] = "true"
			svc.Spec.LoadBalancerSourceRanges = append(allowedIPv4Ranges, allowedIPv6Ranges...)

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
				DoAndReturn(func(
					ctx context.Context,
					resourceGroupName, securityGroupName string,
					properties network.SecurityGroup,
					etag string,
				) *retry.Error {
					assert.Len(t, *properties.SecurityRules, 6, "4 allow rules + 2 deny all rules")

					rules := []network.SecurityRule{
						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							DenyAllSecurityRule(iputil.IPv4).
							WithPriority(4095).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							DenyAllSecurityRule(iputil.IPv6).
							WithPriority(4094).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)

					return nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})
	})

	t.Run("skip - when rules are up-to-date", func(t *testing.T) {

		t.Run("with `service.beta.kubernetes.io/azure-additional-public-ips` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			svc.Annotations[consts.ServiceAnnotationAdditionalPublicIPs] = strings.Join(azureFx.LoadBalancer().AdditionalAddresses(), ",")

			var (
				dstIPv4Addresses = append(azureFx.LoadBalancer().IPv4Addresses(), azureFx.LoadBalancer().AdditionalIPv4Addresses()...)
				dstIPv6Addresses = append(azureFx.LoadBalancer().IPv6Addresses(), azureFx.LoadBalancer().AdditionalIPv6Addresses()...)
			)
			serviceTags := []string{securitygroup.ServiceTagInternet}
			rules := append(azureFx.NoiseSecurityRules(10), // with irrelevant rules
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPPorts()).
					WithPriority(500).
					WithDestination(dstIPv4Addresses...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPPorts()).
					WithPriority(501).
					WithDestination(dstIPv6Addresses...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPPorts()).
					WithPriority(502).
					WithDestination(dstIPv4Addresses...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPPorts()).
					WithPriority(503).
					WithDestination(dstIPv6Addresses...).
					Build(),
			)
			securityGroup := azureFx.SecurityGroup().WithRules(rules).Build()
			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			svc.Annotations[consts.ServiceAnnotationDisableLoadBalancerFloatingIP] = "true"

			serviceTags := []string{securitygroup.ServiceTagInternet}
			rules := append(azureFx.NoiseSecurityRules(10), // with irrelevant rules
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPNodePorts()). // use NodePort
					WithPriority(500).
					WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...). // Use backend pool IPs
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPNodePorts()). // use NodePort
					WithPriority(501).
					WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...). // Use backend pool IPs
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPNodePorts()). // use NodePort
					WithPriority(502).
					WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...). // Use backend pool IPs
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPNodePorts()). // use NodePort
					WithPriority(503).
					WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...). // Use backend pool IPs
					Build(),
			)
			securityGroup := azureFx.SecurityGroup().WithRules(rules).Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-allowed-ip-ranges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			var (
				allowedIPv4Ranges = []string{"172.30.0.0/16", "172.31.0.1/32"}
				allowedIPv6Ranges = []string{"2607:f0d0:1002:51::/64", "fd00::/8"}
			)

			svc.Annotations[consts.ServiceAnnotationAllowedIPRanges] = strings.Join(append(allowedIPv4Ranges, allowedIPv6Ranges...), ",")

			rules := append(azureFx.NoiseSecurityRules(10), // with irrelevant rules
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(500).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(501).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(502).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(503).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
			)
			securityGroup := azureFx.SecurityGroup().WithRules(rules).Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-allowed-service-tags` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			var allowedServiceTags = []string{"AzureCloud", "AzureDatabricks"}

			svc.Annotations[consts.ServiceAnnotationAllowedServiceTags] = strings.Join(allowedServiceTags, ",")

			rules := append(azureFx.NoiseSecurityRules(10), // with irrelevant rules
				// TCP + IPv4
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts()).
					WithPriority(500).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts()).
					WithPriority(501).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),
				// TCP + IPv6
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts()).
					WithPriority(502).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts()).
					WithPriority(503).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
				// UDP + IPv4
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts()).
					WithPriority(504).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts()).
					WithPriority(505).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),
				// UDP + IPv6
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts()).
					WithPriority(506).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
			)
			securityGroup := azureFx.SecurityGroup().WithRules(rules).Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `spec.loadBalancerSourceRanges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			var (
				allowedIPv4Ranges = []string{"172.30.0.0/16", "172.31.0.1/32"}
				allowedIPv6Ranges = []string{"2607:f0d0:1002:51::/64", "fd00::/8"}
			)

			svc.Spec.LoadBalancerSourceRanges = append(allowedIPv4Ranges, allowedIPv6Ranges...)

			rules := append(azureFx.NoiseSecurityRules(10), // with irrelevant rules
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(500).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(501).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(502).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(503).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
			)

			securityGroup := azureFx.SecurityGroup().WithRules(rules).Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			var (
				allowedIPv4Ranges = []string{"172.30.0.0/16", "172.31.0.1/32"}
				allowedIPv6Ranges = []string{"2607:f0d0:1002:51::/64", "fd00::/8"}
			)

			svc.Annotations[consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges] = "true"
			svc.Spec.LoadBalancerSourceRanges = append(allowedIPv4Ranges, allowedIPv6Ranges...)

			rules := append(azureFx.NoiseSecurityRules(10), // with irrelevant rules
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(500).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(501).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(502).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(503).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					DenyAllSecurityRule(iputil.IPv4).
					WithPriority(4095).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					DenyAllSecurityRule(iputil.IPv6).
					WithPriority(4094).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
			)
			securityGroup := azureFx.SecurityGroup().WithRules(rules).Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("expected rules with random priority", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				loadBalancer            = azureFx.LoadBalancer().Build()

				allowedServiceTag = azureFx.ServiceTag()
				allowedIPv4Ranges = fx.RandomIPv4PrefixStrings(3)
				allowedIPv6Ranges = fx.RandomIPv6PrefixStrings(3)
				allowedRanges     = append(allowedIPv4Ranges, allowedIPv6Ranges...)
				svc               = k8sFx.Service().
							WithAllowedServiceTags(allowedServiceTag).
							WithAllowedIPRanges(allowedRanges...).
							Build()
			)
			defer ctrl.Finish()

			var (
				noiseRules  = azureFx.NoiseSecurityRules(10)
				targetRules = []network.SecurityRule{
					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(505).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(507).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(509).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(520).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(530).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(607).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(709).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(3000).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
				}
			)

			securityGroup := azureFx.SecurityGroup().WithRules(
				append(noiseRules, targetRules...),
			).Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})
	})

	t.Run("update rules - remove from unrelated rules", func(t *testing.T) {
		var (
			ctrl                    = gomock.NewController(t)
			az                      = GetTestCloud(ctrl)
			securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
			loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
			loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
			loadBalancer            = azureFx.LoadBalancer().Build()

			allowedServiceTag = azureFx.ServiceTag()
			allowedIPv4Ranges = fx.RandomIPv4PrefixStrings(3)
			allowedIPv6Ranges = fx.RandomIPv6PrefixStrings(3)
			allowedRanges     = append(allowedIPv4Ranges, allowedIPv6Ranges...)
			svc               = k8sFx.Service().
						WithAllowedServiceTags(allowedServiceTag).
						WithAllowedIPRanges(allowedRanges...).
						Build()
		)
		defer ctrl.Finish()

		var (
			noiseRules = azureFx.NoiseSecurityRules(10)
			staleRules = []network.SecurityRule{
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{8000}).
					WithPriority(4000).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // Should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
					WithPriority(4001).
					WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "foo", "bar")...). // Should keep foo and bar but clean the rest
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
					WithPriority(4002).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "baz")...). // Should keep baz but clean the rest
					Build(),

				{
					Name: ptr.To("foo"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      ptr.To([]string{"foo"}),
						DestinationPortRanges:      ptr.To([]string{"4000-6000"}),
						DestinationAddressPrefixes: ptr.To(azureFx.LoadBalancer().Addresses()), // Should remove the rule
						Priority:                   ptr.To(int32(4003)),
					},
				},
				{
					Name: ptr.To("bar"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolUDP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      ptr.To([]string{"bar"}),
						DestinationPortRanges:      ptr.To([]string{"5000-6000"}),
						DestinationAddressPrefixes: ptr.To(append(azureFx.LoadBalancer().Addresses(), "bar")), // Should keep bar but clean the rest
						Priority:                   ptr.To(int32(4004)),
					},
				},
			}
			targetRules = []network.SecurityRule{
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(505).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(509).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(520).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(530).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(607).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(709).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(3000).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
			}
		)

		securityGroup := azureFx.SecurityGroup().WithRules(
			append(append(noiseRules, targetRules...), staleRules...),
		).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			Return(securityGroup, nil).
			Times(1)
		securityGroupClient.EXPECT().
			CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
			DoAndReturn(func(
				ctx context.Context,
				resourceGroupName, securityGroupName string,
				properties network.SecurityGroup,
				etag string,
			) *retry.Error {
				rules := append(append(noiseRules, targetRules...),
					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
						WithPriority(4001).
						WithDestination("foo", "bar"). // Should keep foo and bar but clean the rest
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
						WithPriority(4002).
						WithDestination("baz"). // Should keep baz but clean the rest
						Build(),

					network.SecurityRule{
						Name: ptr.To("bar"),
						SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
							Protocol:                   network.SecurityRuleProtocolUDP,
							Access:                     network.SecurityRuleAccessAllow,
							Direction:                  network.SecurityRuleDirectionInbound,
							SourcePortRange:            ptr.To("*"),
							SourceAddressPrefixes:      ptr.To([]string{"bar"}),
							DestinationPortRanges:      ptr.To([]string{"5000-6000"}),
							DestinationAddressPrefixes: ptr.To([]string{"bar"}), // Should keep bar but clean the rest
							Priority:                   ptr.To(int32(4004)),
						},
					},
				)

				testutil.ExpectExactSecurityRules(t, &properties, rules)

				return nil
			}).Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, nil).
			Times(1)
		loadBalancerBackendPool.EXPECT().
			GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
			Return(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
			).
			Times(1)

		_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
		assert.NoError(t, err)
	})

	t.Run("update rules - add to related rules", func(t *testing.T) {
		var (
			ctrl                    = gomock.NewController(t)
			az                      = GetTestCloud(ctrl)
			securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
			loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
			loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
			loadBalancer            = azureFx.LoadBalancer().Build()

			allowedServiceTag = azureFx.ServiceTag()
			allowedIPv4Ranges = fx.RandomIPv4PrefixStrings(3)
			allowedIPv6Ranges = fx.RandomIPv6PrefixStrings(3)
			allowedRanges     = append(allowedIPv4Ranges, allowedIPv6Ranges...)
			svc               = k8sFx.Service().
						WithAllowedServiceTags(allowedServiceTag).
						WithAllowedIPRanges(allowedRanges...).
						Build()
		)
		defer ctrl.Finish()

		var (
			noiseRules = azureFx.NoiseSecurityRules(10)
			staleRules = []network.SecurityRule{
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(505).
					WithDestination("foo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(520).
					WithDestination("baz", "quo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(530).
					WithDestination("bar"). // should add to this rule
					Build(),
			}
			upToDateRules = []network.SecurityRule{

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(509).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(607).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(709).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(3000).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
			}
		)

		securityGroup := azureFx.SecurityGroup().WithRules(
			append(append(noiseRules, upToDateRules...), staleRules...),
		).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			Return(securityGroup, nil).
			Times(1)
		securityGroupClient.EXPECT().
			CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
			DoAndReturn(func(
				ctx context.Context,
				resourceGroupName, securityGroupName string,
				properties network.SecurityGroup,
				etag string,
			) *retry.Error {
				rules := append(append(noiseRules, upToDateRules...),
					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(505).
						WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "foo")...). // should add to this rule
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(530).
						WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "bar")...). // should add to this rule
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(520).
						WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "baz", "quo")...). // should add to this rule
						Build(),
				)

				testutil.ExpectExactSecurityRules(t, &properties, rules)

				return nil
			}).Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, nil).
			Times(1)
		loadBalancerBackendPool.EXPECT().
			GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
			Return(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
			).
			Times(1)

		_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
		assert.NoError(t, err)
	})

	t.Run("update rules - remove and add", func(t *testing.T) {
		var (
			ctrl                    = gomock.NewController(t)
			az                      = GetTestCloud(ctrl)
			securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
			loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
			loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
			loadBalancer            = azureFx.LoadBalancer().Build()

			allowedServiceTag = azureFx.ServiceTag()
			allowedIPv4Ranges = fx.RandomIPv4PrefixStrings(3)
			allowedIPv6Ranges = fx.RandomIPv6PrefixStrings(3)
			allowedRanges     = append(allowedIPv4Ranges, allowedIPv6Ranges...)
			svc               = k8sFx.Service().
						WithAllowedServiceTags(allowedServiceTag).
						WithAllowedIPRanges(allowedRanges...).
						Build()
		)
		defer ctrl.Finish()

		var (
			noiseRules = azureFx.NoiseSecurityRules(10)
			staleRules = []network.SecurityRule{
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{8000}).
					WithPriority(4000).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // Should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
					WithPriority(4001).
					WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "foo", "bar")...). // Should keep foo and bar but clean the rest
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
					WithPriority(4002).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "baz")...). // Should keep baz but clean the rest
					Build(),

				{
					Name: ptr.To("foo"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      ptr.To([]string{"foo"}),
						DestinationPortRanges:      ptr.To([]string{"4000-6000"}),
						DestinationAddressPrefixes: ptr.To(azureFx.LoadBalancer().Addresses()), // Should remove the rule
						Priority:                   ptr.To(int32(4003)),
					},
				},
				{
					Name: ptr.To("bar"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolUDP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      ptr.To([]string{"bar"}),
						DestinationPortRanges:      ptr.To([]string{"5000-6000"}),
						DestinationAddressPrefixes: ptr.To(append(azureFx.LoadBalancer().Addresses(), "bar")), // Should keep bar but clean the rest
						Priority:                   ptr.To(int32(4004)),
					},
				},
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(505).
					WithDestination("foo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(520).
					WithDestination("baz", "quo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(530).
					WithDestination("bar"). // should add to this rule
					Build(),
			}
			upToDateRules = []network.SecurityRule{

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(509).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(607).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(709).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(3000).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
			}
		)

		securityGroup := azureFx.SecurityGroup().WithRules(
			append(append(noiseRules, upToDateRules...), staleRules...),
		).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			Return(securityGroup, nil).
			Times(1)
		securityGroupClient.EXPECT().
			CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
			DoAndReturn(func(
				ctx context.Context,
				resourceGroupName, securityGroupName string,
				properties network.SecurityGroup,
				etag string,
			) *retry.Error {
				rules := append(append(noiseRules, upToDateRules...),
					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
						WithPriority(4001).
						WithDestination("foo", "bar"). // Should keep foo and bar but clean the rest
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
						WithPriority(4002).
						WithDestination("baz"). // Should keep baz but clean the rest
						Build(),

					network.SecurityRule{
						Name: ptr.To("bar"),
						SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
							Protocol:                   network.SecurityRuleProtocolUDP,
							Access:                     network.SecurityRuleAccessAllow,
							Direction:                  network.SecurityRuleDirectionInbound,
							SourcePortRange:            ptr.To("*"),
							SourceAddressPrefixes:      ptr.To([]string{"bar"}),
							DestinationPortRanges:      ptr.To([]string{"5000-6000"}),
							DestinationAddressPrefixes: ptr.To([]string{"bar"}), // Should keep bar but clean the rest
							Priority:                   ptr.To(int32(4004)),
						},
					},
					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(505).
						WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "foo")...). // should add to this rule
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(530).
						WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "bar")...). // should add to this rule
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(520).
						WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "baz", "quo")...). // should add to this rule
						Build(),
				)

				testutil.ExpectExactSecurityRules(t, &properties, rules)

				return nil
			}).Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, nil).
			Times(1)
		loadBalancerBackendPool.EXPECT().
			GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
			Return(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
			).
			Times(1)

		_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
		assert.NoError(t, err)
	})

	t.Run("clean rules - when deleting LB / AzureLoadBalancer had been created", func(t *testing.T) {
		var (
			ctrl                    = gomock.NewController(t)
			az                      = GetTestCloud(ctrl)
			securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
			loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
			loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
			loadBalancer            = azureFx.LoadBalancer().Build()

			allowedServiceTag = azureFx.ServiceTag()
			allowedIPv4Ranges = fx.RandomIPv4PrefixStrings(3)
			allowedIPv6Ranges = fx.RandomIPv6PrefixStrings(3)
			allowedRanges     = append(allowedIPv4Ranges, allowedIPv6Ranges...)
			svc               = k8sFx.Service().
						WithAllowedServiceTags(allowedServiceTag).
						WithAllowedIPRanges(allowedRanges...).
						Build()
		)
		defer ctrl.Finish()

		var (
			noiseRules = azureFx.NoiseSecurityRules(10)
			staleRules = []network.SecurityRule{
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(509).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...). // should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(3000).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "foo")...). // should keep foo
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{8000}).
					WithPriority(4000).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // Should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
					WithPriority(4001).
					WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "foo", "bar")...). // Should keep foo and bar but clean the rest
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
					WithPriority(4002).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "baz")...). // Should keep baz but clean the rest
					Build(),

				{
					Name: ptr.To("foo"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      ptr.To([]string{"foo"}),
						DestinationPortRanges:      ptr.To([]string{"4000-6000"}),
						DestinationAddressPrefixes: ptr.To(azureFx.LoadBalancer().Addresses()), // Should remove the rule
						Priority:                   ptr.To(int32(4003)),
					},
				},
				{
					Name: ptr.To("bar"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolUDP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      ptr.To([]string{"bar"}),
						DestinationPortRanges:      ptr.To([]string{"5000-6000"}),
						DestinationAddressPrefixes: ptr.To(append(azureFx.LoadBalancer().Addresses(), "bar")), // Should keep bar but clean the rest
						Priority:                   ptr.To(int32(4004)),
					},
				},
			}
			upToDateRules = []network.SecurityRule{
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(505).
					WithDestination("foo"). // should keep it
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(520).
					WithDestination("baz", "quo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(530).
					WithDestination("bar"). // should add to this rule
					Build(),
			}
		)

		securityGroup := azureFx.SecurityGroup().WithRules(
			append(append(noiseRules, upToDateRules...), staleRules...),
		).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			Return(securityGroup, nil).
			Times(1)
		securityGroupClient.EXPECT().
			CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
			DoAndReturn(func(
				ctx context.Context,
				resourceGroupName, securityGroupName string,
				properties network.SecurityGroup,
				etag string,
			) *retry.Error {
				rules := append(append(noiseRules, upToDateRules...),
					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
						WithPriority(4001).
						WithDestination("foo", "bar"). // Should keep foo and bar but clean the rest
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
						WithPriority(4002).
						WithDestination("baz"). // Should keep baz but clean the rest
						Build(),

					network.SecurityRule{
						Name: ptr.To("bar"),
						SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
							Protocol:                   network.SecurityRuleProtocolUDP,
							Access:                     network.SecurityRuleAccessAllow,
							Direction:                  network.SecurityRuleDirectionInbound,
							SourcePortRange:            ptr.To("*"),
							SourceAddressPrefixes:      ptr.To([]string{"bar"}),
							DestinationPortRanges:      ptr.To([]string{"5000-6000"}),
							DestinationAddressPrefixes: ptr.To([]string{"bar"}), // Should keep bar but clean the rest
							Priority:                   ptr.To(int32(4004)),
						},
					},

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(3000).
						WithDestination("foo"). // should keep foo
						Build(),
				)

				testutil.ExpectExactSecurityRules(t, &properties, rules)

				return nil
			}).Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, nil).
			Times(1)
		loadBalancerBackendPool.EXPECT().
			GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
			Return(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
			).
			Times(1)

		_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), false) // deleting
		assert.NoError(t, err)
	})

	t.Run("clean rules - when deleting LB / AzureLoadBalancer had been created / service with invalid annotation", func(t *testing.T) {
		var (
			ctrl                    = gomock.NewController(t)
			az                      = GetTestCloud(ctrl)
			securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
			loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
			loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
			loadBalancer            = azureFx.LoadBalancer().Build()

			allowedServiceTag = azureFx.ServiceTag()
			allowedIPv4Ranges = fx.RandomIPv4PrefixStrings(3)
			allowedIPv6Ranges = fx.RandomIPv6PrefixStrings(3)
			allowedRanges     = append(allowedIPv4Ranges, allowedIPv6Ranges...)
			svc               = k8sFx.Service().
						WithAllowedServiceTags(allowedServiceTag).
						WithAllowedIPRanges(allowedRanges...).
						Build()
		)
		defer ctrl.Finish()

		// mess svc
		svc.Annotations = map[string]string{
			consts.ServiceAnnotationAdditionalPublicIPs: "-=f oo;bar(%{[",
			consts.ServiceAnnotationAllowedServiceTags:  "-=f oo;bar(%{[",
			consts.ServiceAnnotationAllowedIPRanges:     "-=f oo;bar(%{[",
		}

		var (
			noiseRules = azureFx.NoiseSecurityRules(10)
			staleRules = []network.SecurityRule{
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(509).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...). // should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(3000).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "foo")...). // should keep foo
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{8000}).
					WithPriority(4000).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // Should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
					WithPriority(4001).
					WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "foo", "bar")...). // Should keep foo and bar but clean the rest
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
					WithPriority(4002).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "baz")...). // Should keep baz but clean the rest
					Build(),

				{
					Name: ptr.To("foo"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      ptr.To([]string{"foo"}),
						DestinationPortRanges:      ptr.To([]string{"4000-6000"}),
						DestinationAddressPrefixes: ptr.To(azureFx.LoadBalancer().Addresses()), // Should remove the rule
						Priority:                   ptr.To(int32(4003)),
					},
				},
				{
					Name: ptr.To("bar"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolUDP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      ptr.To([]string{"bar"}),
						DestinationPortRanges:      ptr.To([]string{"5000-6000"}),
						DestinationAddressPrefixes: ptr.To(append(azureFx.LoadBalancer().Addresses(), "bar")), // Should keep bar but clean the rest
						Priority:                   ptr.To(int32(4004)),
					},
				},
			}
			upToDateRules = []network.SecurityRule{
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(505).
					WithDestination("foo"). // should keep it
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(520).
					WithDestination("baz", "quo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(530).
					WithDestination("bar"). // should add to this rule
					Build(),
			}
		)

		securityGroup := azureFx.SecurityGroup().WithRules(
			append(append(noiseRules, upToDateRules...), staleRules...),
		).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			Return(securityGroup, nil).
			Times(1)
		securityGroupClient.EXPECT().
			CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
			DoAndReturn(func(
				ctx context.Context,
				resourceGroupName, securityGroupName string,
				properties network.SecurityGroup,
				etag string,
			) *retry.Error {
				rules := append(append(noiseRules, upToDateRules...),
					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
						WithPriority(4001).
						WithDestination("foo", "bar"). // Should keep foo and bar but clean the rest
						Build(),

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
						WithPriority(4002).
						WithDestination("baz"). // Should keep baz but clean the rest
						Build(),

					network.SecurityRule{
						Name: ptr.To("bar"),
						SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
							Protocol:                   network.SecurityRuleProtocolUDP,
							Access:                     network.SecurityRuleAccessAllow,
							Direction:                  network.SecurityRuleDirectionInbound,
							SourcePortRange:            ptr.To("*"),
							SourceAddressPrefixes:      ptr.To([]string{"bar"}),
							DestinationPortRanges:      ptr.To([]string{"5000-6000"}),
							DestinationAddressPrefixes: ptr.To([]string{"bar"}), // Should keep bar but clean the rest
							Priority:                   ptr.To(int32(4004)),
						},
					},

					azureFx.
						AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(3000).
						WithDestination("foo"). // should keep foo
						Build(),
				)

				testutil.ExpectExactSecurityRules(t, &properties, rules)

				return nil
			}).Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, nil).
			Times(1)
		loadBalancerBackendPool.EXPECT().
			GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
			Return(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
			).
			Times(1)

		_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), false) // deleting
		assert.NoError(t, err)
	})

	t.Run("clean rules - when deleting LB and AzureLoadBalancer not found", func(t *testing.T) {
		var (
			ctrl                = gomock.NewController(t)
			az                  = GetTestCloud(ctrl)
			securityGroupClient = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
			loadBalancerClient  = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
			loadBalancer        = azureFx.LoadBalancer().Build()

			allowedServiceTag = azureFx.ServiceTag()
			allowedIPv4Ranges = fx.RandomIPv4PrefixStrings(3)
			allowedIPv6Ranges = fx.RandomIPv6PrefixStrings(3)
			allowedRanges     = append(allowedIPv4Ranges, allowedIPv6Ranges...)
			svc               = k8sFx.Service().
						WithAllowedServiceTags(allowedServiceTag).
						WithAllowedIPRanges(allowedRanges...).
						Build()
		)
		defer ctrl.Finish()

		var (
			noiseRules = azureFx.NoiseSecurityRules(10)
			staleRules = []network.SecurityRule{
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(509).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...). // should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(3000).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "foo")...). // should keep foo
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{8000}).
					WithPriority(4000).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // Should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
					WithPriority(4001).
					WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "foo", "bar")...). // Should keep foo and bar but clean the rest
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
					WithPriority(4002).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "baz")...). // Should keep baz but clean the rest
					Build(),

				{
					Name: ptr.To("foo"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolTCP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      ptr.To([]string{"foo"}),
						DestinationPortRanges:      ptr.To([]string{"4000-6000"}),
						DestinationAddressPrefixes: ptr.To(azureFx.LoadBalancer().Addresses()), // Should remove the rule
						Priority:                   ptr.To(int32(4003)),
					},
				},
				{
					Name: ptr.To("bar"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                   network.SecurityRuleProtocolUDP,
						Access:                     network.SecurityRuleAccessAllow,
						Direction:                  network.SecurityRuleDirectionInbound,
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      ptr.To([]string{"bar"}),
						DestinationPortRanges:      ptr.To([]string{"5000-6000"}),
						DestinationAddressPrefixes: ptr.To(append(azureFx.LoadBalancer().Addresses(), "bar")), // Should keep bar but clean the rest
						Priority:                   ptr.To(int32(4004)),
					},
				},
			}
			upToDateRules = []network.SecurityRule{
				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(505).
					WithDestination("foo"). // should keep it
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(520).
					WithDestination("baz", "quo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(network.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(530).
					WithDestination("bar"). // should add to this rule
					Build(),
			}
		)

		securityGroup := azureFx.SecurityGroup().WithRules(
			append(append(noiseRules, upToDateRules...), staleRules...),
		).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			Return(securityGroup, nil).
			Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, &retry.Error{HTTPStatusCode: http.StatusNotFound}).
			Times(1)

		_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, nil, false) // deleting
		assert.NoError(t, err)
	})

	t.Run("negative cases", func(t *testing.T) {
		t.Run("with both `service.beta.kubernetes.io/azure-allowed-ip-ranges` and `spec.loadBalancerSourceRanges` specified", func(t *testing.T) {
			var (
				ctrl                = gomock.NewController(t)
				az                  = GetTestCloud(ctrl)
				securityGroupClient = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				svc                 = k8sFx.Service().Build()
				securityGroup       = azureFx.SecurityGroup().Build()
				loadBalancer        = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			var (
				allowedIPv4Ranges = []string{"172.30.0.0/16", "172.31.0.1/32"}
				allowedIPv6Ranges = []string{"2607:f0d0:1002:51::/64", "fd00::/8"}
			)

			svc.Annotations[consts.ServiceAnnotationAllowedIPRanges] = strings.Join(append(allowedIPv4Ranges, allowedIPv6Ranges...), ",")
			svc.Spec.LoadBalancerSourceRanges = append(allowedIPv4Ranges, allowedIPv6Ranges...)

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.Error(t, err)
			assert.ErrorIs(t, err, loadbalancer.ErrSetBothLoadBalancerSourceRangesAndAllowedIPRanges)
		})

		t.Run("when SecurityGroupClient.Get returns error", func(t *testing.T) {
			var (
				ctrl                = gomock.NewController(t)
				az                  = GetTestCloud(ctrl)
				securityGroupClient = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				svc                 = k8sFx.Service().Build()
				securityGroup       = azureFx.SecurityGroup().Build()
				loadBalancer        = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			expectedErr := &retry.Error{
				RawError: fmt.Errorf("foo"),
			}

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, expectedErr).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.Error(t, err)
			assert.ErrorIs(t, err, expectedErr.RawError)
		})

		t.Run("when LoadBalancerClient.Get returns error", func(t *testing.T) {
			var (
				ctrl                = gomock.NewController(t)
				az                  = GetTestCloud(ctrl)
				securityGroupClient = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient  = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				svc                 = k8sFx.Service().Build()
				securityGroup       = azureFx.SecurityGroup().Build()
				loadBalancer        = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			expectedErr := &retry.Error{
				RawError: fmt.Errorf("foo"),
			}

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, expectedErr).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.Error(t, err)
			assert.ErrorIs(t, err, expectedErr.RawError)
		})

		t.Run("when SecurityGroupClient.CreateOrUpdate returns error", func(t *testing.T) {

			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			expectedErr := &retry.Error{
				RawError: fmt.Errorf("foo"),
			}

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).
				Return(expectedErr).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.Error(t, err)
			assert.ErrorIs(t, err, expectedErr.RawError)
		})

		t.Run("when the number of rules exceeds the limit", func(t *testing.T) {

			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
				loadBalancerClient      = az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().WithRules(azureFx.NoiseSecurityRules(securitygroup.MaxSecurityRulesPerGroup)).Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(ClusterName, &svc, &loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.Error(t, err)
		})
	})
}
