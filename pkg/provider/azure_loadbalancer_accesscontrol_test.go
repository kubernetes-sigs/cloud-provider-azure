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
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/internal/testutil"
	"sigs.k8s.io/cloud-provider-azure/internal/testutil/fixture"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/securitygroupclient/mock_securitygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/securitygroup"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/iputil"
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
		ctx     = log.NewContext(context.Background(), log.Noop())
	)

	var (
		makeNodesByIPs = func(ips []string) []runtime.Object {
			rv := make([]runtime.Object, len(ips))
			for i, ip := range ips {
				rv[i] = &v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("node-%d", i),
					},
					Status: v1.NodeStatus{
						Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: ip}},
					},
				}
			}
			return rv
		}
	)

	t.Run("internal Load Balancer", func(t *testing.T) {
		t.Run("noop when no allow list specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			svc := k8sFx.Service().WithInternalEnabled().Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			sg, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
			testutil.ExpectEqualInJSON(t, azureFx.SecurityGroup().Build(), sg)
		})

		t.Run("do not add Internet allow rules if allow all", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			svc := k8sFx.Service().WithInternalEnabled().
				WithAllowedIPRanges("0.0.0.0/0").
				Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				DoAndReturn(func(
					_ context.Context,
					_, _ string,
					properties armnetwork.SecurityGroup,
				) (*armnetwork.SecurityGroup, error) {
					rules := []*armnetwork.SecurityRule{
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"0.0.0.0/0"}, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{"0.0.0.0/0"}, k8sFx.Service().UDPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)
					return nil, nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("add rules with a mix of settings", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				DoAndReturn(func(
					_ context.Context,
					_, _ string,
					properties armnetwork.SecurityGroup,
				) (*armnetwork.SecurityGroup, error) {
					rules := []*armnetwork.SecurityRule{
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{azureFx.ServiceTag()}, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"0.0.0.0/0"}, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{azureFx.ServiceTag()}, k8sFx.Service().TCPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{azureFx.ServiceTag()}, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{"0.0.0.0/0"}, k8sFx.Service().UDPPorts()).
							WithPriority(504).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{azureFx.ServiceTag()}, k8sFx.Service().UDPPorts()).
							WithPriority(505).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)
					return nil, nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})
	})

	t.Run("public Load Balancer", func(t *testing.T) {
		t.Run("add Internet allow rules if no allow list specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				DoAndReturn(func(
					_ context.Context,
					_, _ string,
					properties armnetwork.SecurityGroup,
				) (*armnetwork.SecurityGroup, error) {
					assert.Len(t, properties.Properties.SecurityRules, 4, "expect exact 4 (2 TCP + 2 UDP) rule for allowing Internet")

					serviceTags := []string{securitygroup.ServiceTagInternet}
					rules := []*armnetwork.SecurityRule{
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)
					return nil, nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})
	})

	t.Run("add rules - when no rules exist", func(t *testing.T) {
		t.Run("with `service.beta.kubernetes.io/azure-additional-public-ips` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			svc.Annotations[consts.ServiceAnnotationAdditionalPublicIPs] = strings.Join(azureFx.LoadBalancer().AdditionalAddresses(), ",")

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				DoAndReturn(func(
					_ context.Context,
					_, _ string,
					properties armnetwork.SecurityGroup,
				) (*armnetwork.SecurityGroup, error) {
					assert.Len(t, properties.Properties.SecurityRules, 4, "expect exact 4 rule for allowing Internet")

					var (
						dstIPv4Addresses = append(azureFx.LoadBalancer().IPv4Addresses(), azureFx.LoadBalancer().AdditionalIPv4Addresses()...)
						dstIPv6Addresses = append(azureFx.LoadBalancer().IPv6Addresses(), azureFx.LoadBalancer().AdditionalIPv6Addresses()...)
					)

					serviceTags := []string{securitygroup.ServiceTagInternet}
					rules := []*armnetwork.SecurityRule{
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(dstIPv4Addresses...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(dstIPv6Addresses...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPPorts()).
							WithPriority(502).
							WithDestination(dstIPv4Addresses...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(dstIPv6Addresses...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)
					return nil, nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			{
				kubeClient := fake.NewSimpleClientset(makeNodesByIPs(
					append(azureFx.LoadBalancer().BackendPoolIPv4Addresses(), azureFx.LoadBalancer().BackendPoolIPv6Addresses()...),
				)...)
				informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
				az.serviceLister = informerFactory.Core().V1().Services().Lister()
				az.nodeLister = informerFactory.Core().V1().Nodes().Lister()
				informerFactory.Start(wait.NeverStop)
				informerFactory.WaitForCacheSync(wait.NeverStop)
			}

			svc.Annotations[consts.ServiceAnnotationDisableLoadBalancerFloatingIP] = "true"

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				DoAndReturn(func(
					_ context.Context,
					_, _ string,
					properties armnetwork.SecurityGroup,
				) (*armnetwork.SecurityGroup, error) {
					assert.Len(t, properties.Properties.SecurityRules, 4, "expect exact 4 (2 TCP + 2 UDP) rule for allowing Internet on IPv4 and IPv6")

					serviceTags := []string{securitygroup.ServiceTagInternet}
					rules := []*armnetwork.SecurityRule{
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPNodePorts()). // use NodePort
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...). // Use backend pool IPs
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPNodePorts()). // use NodePort
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...). // Use backend pool IPs
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPNodePorts()). // use NodePort
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...). // Use backend pool IPs
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPNodePorts()). // use NodePort
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...). // Use backend pool IPs
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)
					return nil, nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with disabled floating IP and NSG rule management disabled", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().
							WithDisableFloatingIP().
							WithDisableLoadBalancerNSGRule().
							Build()
				securityGroup = azureFx.SecurityGroup().Build()
				loadBalancer  = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			{
				kubeClient := fake.NewSimpleClientset(makeNodesByIPs(
					append(azureFx.LoadBalancer().BackendPoolIPv4Addresses(), azureFx.LoadBalancer().BackendPoolIPv6Addresses()...),
				)...)
				informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
				az.serviceLister = informerFactory.Core().V1().Services().Lister()
				az.nodeLister = informerFactory.Core().V1().Nodes().Lister()
				informerFactory.Start(wait.NeverStop)
				informerFactory.WaitForCacheSync(wait.NeverStop)
			}

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			sg, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
			testutil.ExpectEqualInJSON(t, azureFx.SecurityGroup().Build(), sg)
		})

		t.Run("with `service.beta.kubernetes.io/azure-allowed-ip-ranges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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

			{
				ipRanges := append(allowedIPv4Ranges, allowedIPv6Ranges...)
				ipRanges = append(ipRanges, "172.30.0.1/32", "2607:f0d0:1002:51::1/128") // with overlapping CIDRs
				svc.Annotations[consts.ServiceAnnotationAllowedIPRanges] = strings.Join(ipRanges, ",")
			}

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				DoAndReturn(func(
					_ context.Context,
					_, _ string,
					properties armnetwork.SecurityGroup,
				) (*armnetwork.SecurityGroup, error) {
					assert.Len(t, properties.Properties.SecurityRules, 4, "expect exact 4 rules for allowing on IPv4 and IPv6")

					rules := []*armnetwork.SecurityRule{
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)

					return nil, nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-allowed-service-tags` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			var allowedServiceTags = []string{"AzureCloud", "AzureDatabricks"}

			svc.Annotations[consts.ServiceAnnotationAllowedServiceTags] = strings.Join(allowedServiceTags, ",")

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				DoAndReturn(func(
					_ context.Context,
					_, _ string,
					properties armnetwork.SecurityGroup,
				) (*armnetwork.SecurityGroup, error) {
					assert.Len(t, properties.Properties.SecurityRules, 8, "<2 service tags> * <2 IP stack> * <2 Protocol[TCP/UDP]>")

					rules := []*armnetwork.SecurityRule{
						// TCP + IPv4
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),
						// TCP + IPv6
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
						// UDP + IPv4
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts()).
							WithPriority(504).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts()).
							WithPriority(505).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),
						// UDP + IPv6
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts()).
							WithPriority(506).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts()).
							WithPriority(507).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)

					return nil, nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `spec.loadBalancerSourceRanges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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

			{
				ipRanges := append(allowedIPv4Ranges, allowedIPv6Ranges...)
				ipRanges = append(ipRanges, "172.30.0.1/32", "2607:f0d0:1002:51::1/128") // with overlapping CIDRs
				svc.Spec.LoadBalancerSourceRanges = ipRanges
			}

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				DoAndReturn(func(
					_ context.Context,
					_, _ string,
					properties armnetwork.SecurityGroup,
				) (*armnetwork.SecurityGroup, error) {
					assert.Len(t, properties.Properties.SecurityRules, 4, "expect exact 4 rules for allowing on IPv4 and IPv6")

					rules := []*armnetwork.SecurityRule{
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
							WithPriority(503).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),
					}

					testutil.ExpectExactSecurityRules(t, &properties, rules)
					return nil, nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				DoAndReturn(func(
					_ context.Context,
					_, _ string,
					properties armnetwork.SecurityGroup,
				) (*armnetwork.SecurityGroup, error) {
					assert.Len(t, properties.Properties.SecurityRules, 6, "4 allow rules + 2 deny all rules")

					rules := []*armnetwork.SecurityRule{
						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(500).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
							WithPriority(501).
							WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
							WithPriority(502).
							WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
							Build(),

						azureFx.
							AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
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

					return nil, nil
				}).Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})
	})

	t.Run("skip - when rules are up-to-date", func(t *testing.T) {
		t.Run("with `service.beta.kubernetes.io/azure-additional-public-ips` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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
			rules := append(azureFx.NoiseSecurityRules(), // with irrelevant rules
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPPorts()).
					WithPriority(500).
					WithDestination(dstIPv4Addresses...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPPorts()).
					WithPriority(501).
					WithDestination(dstIPv6Addresses...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPPorts()).
					WithPriority(502).
					WithDestination(dstIPv4Addresses...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPPorts()).
					WithPriority(503).
					WithDestination(dstIPv6Addresses...).
					Build(),
			)
			securityGroup := azureFx.SecurityGroup().WithRules(rules).Build()
			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			{
				kubeClient := fake.NewSimpleClientset(makeNodesByIPs(
					append(azureFx.LoadBalancer().BackendPoolIPv4Addresses(), azureFx.LoadBalancer().BackendPoolIPv6Addresses()...),
				)...)
				informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
				az.serviceLister = informerFactory.Core().V1().Services().Lister()
				az.nodeLister = informerFactory.Core().V1().Nodes().Lister()
				informerFactory.Start(wait.NeverStop)
				informerFactory.WaitForCacheSync(wait.NeverStop)
			}

			svc.Annotations[consts.ServiceAnnotationDisableLoadBalancerFloatingIP] = "true"

			serviceTags := []string{securitygroup.ServiceTagInternet}
			rules := append(azureFx.NoiseSecurityRules(), // with irrelevant rules
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, serviceTags, k8sFx.Service().TCPNodePorts()). // use NodePort
					WithPriority(500).
					WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...). // Use backend pool IPs
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, serviceTags, k8sFx.Service().TCPNodePorts()). // use NodePort
					WithPriority(501).
					WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...). // Use backend pool IPs
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, serviceTags, k8sFx.Service().UDPNodePorts()). // use NodePort
					WithPriority(502).
					WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...). // Use backend pool IPs
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, serviceTags, k8sFx.Service().UDPNodePorts()). // use NodePort
					WithPriority(503).
					WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...). // Use backend pool IPs
					Build(),
			)
			securityGroup := azureFx.SecurityGroup().WithRules(rules).Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-allowed-ip-ranges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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

			rules := append(azureFx.NoiseSecurityRules(), // with irrelevant rules
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(500).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(501).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(502).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(503).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
			)
			securityGroup := azureFx.SecurityGroup().WithRules(rules).Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-allowed-service-tags` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			var allowedServiceTags = []string{"AzureCloud", "AzureDatabricks"}

			svc.Annotations[consts.ServiceAnnotationAllowedServiceTags] = strings.Join(allowedServiceTags, ",")

			rules := append(azureFx.NoiseSecurityRules(), // with irrelevant rules
				// TCP + IPv4
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts()).
					WithPriority(500).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts()).
					WithPriority(501).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),
				// TCP + IPv6
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().TCPPorts()).
					WithPriority(502).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().TCPPorts()).
					WithPriority(503).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
				// UDP + IPv4
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts()).
					WithPriority(504).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts()).
					WithPriority(505).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),
				// UDP + IPv6
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[0]}, k8sFx.Service().UDPPorts()).
					WithPriority(506).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTags[1]}, k8sFx.Service().UDPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
			)
			securityGroup := azureFx.SecurityGroup().WithRules(rules).Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `spec.loadBalancerSourceRanges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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

			rules := append(azureFx.NoiseSecurityRules(), // with irrelevant rules
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(500).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(501).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(502).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(503).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
			)

			securityGroup := azureFx.SecurityGroup().WithRules(rules).Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("with `service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges` specified", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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

			rules := append(azureFx.NoiseSecurityRules(), // with irrelevant rules
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(500).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(501).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(502).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
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
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})

		t.Run("expected rules with random priority", func(t *testing.T) {
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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
				noiseRules  = azureFx.NoiseSecurityRules()
				targetRules = []*armnetwork.SecurityRule{
					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(505).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(507).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(509).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(520).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(530).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(607).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(709).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),

					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(3000).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
				}
			)

			securityGroup := azureFx.SecurityGroup().WithRules(
				append(noiseRules, targetRules...),
			).Build()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.NoError(t, err)
		})
	})

	t.Run("update rules - add to related rules", func(t *testing.T) {
		var (
			ctrl                    = gomock.NewController(t)
			az                      = GetTestCloud(ctrl)
			securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
			loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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
			noiseRules = azureFx.NoiseSecurityRules()
			staleRules = []*armnetwork.SecurityRule{
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(505).
					WithDestination("foo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(520).
					WithDestination("baz", "quo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(530).
					WithDestination("bar"). // should add to this rule
					Build(),
			}
			upToDateRules = []*armnetwork.SecurityRule{

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(509).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(607).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(709).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(3000).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),
			}
		)

		securityGroup := azureFx.SecurityGroup().WithRules(
			append(append(noiseRules, upToDateRules...), staleRules...),
		).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
			Return(securityGroup, nil).
			Times(1)
		securityGroupClient.EXPECT().
			CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			DoAndReturn(func(
				_ context.Context,
				_, _ string,
				properties armnetwork.SecurityGroup,
			) (*armnetwork.SecurityGroup, error) {
				rules := append(append(noiseRules, upToDateRules...),
					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(505).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // should add to this rule
						Build(),

					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(530).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // should add to this rule
						Build(),

					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(520).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...). // should add to this rule
						Build(),
				)

				testutil.ExpectExactSecurityRules(t, &properties, rules)

				return nil, nil
			}).Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, nil).
			Times(1)
		loadBalancerBackendPool.EXPECT().
			GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
			Return(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
			).
			Times(1)

		_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
		assert.NoError(t, err)
	})

	t.Run("update rules - remove and add", func(t *testing.T) {
		var (
			ctrl                    = gomock.NewController(t)
			az                      = GetTestCloud(ctrl)
			securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
			loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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
			noiseRules = azureFx.NoiseSecurityRules()
			staleRules = []*armnetwork.SecurityRule{
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{8000}).
					WithPriority(4000).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // Should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
					WithPriority(4001).
					WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "foo", "bar")...). // Should keep foo and bar but clean the rest
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
					WithPriority(4002).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "baz")...). // Should keep baz but clean the rest
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(505).
					WithDestination("foo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(520).
					WithDestination("baz", "quo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(530).
					WithDestination("bar"). // should add to this rule
					Build(),
			}
			upToDateRules = []*armnetwork.SecurityRule{

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(509).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(607).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(709).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(3000).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				{
					Name: ptr.To("foo"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      to.SliceOfPtrs("foo"),
						DestinationPortRanges:      to.SliceOfPtrs("4000", "6000"),
						DestinationAddressPrefixes: to.SliceOfPtrs(azureFx.LoadBalancer().Addresses()...),
						Priority:                   ptr.To(int32(4003)),
					},
				},
				{
					Name: ptr.To("bar"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      to.SliceOfPtrs("bar"),
						DestinationPortRanges:      to.SliceOfPtrs("5000", "6000"),
						DestinationAddressPrefixes: to.SliceOfPtrs(append(azureFx.LoadBalancer().Addresses(), "bar")...),
						Priority:                   ptr.To(int32(4004)),
					},
				},
			}
		)

		securityGroup := azureFx.SecurityGroup().WithRules(
			append(append(noiseRules, upToDateRules...), staleRules...),
		).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
			Return(securityGroup, nil).
			Times(1)
		securityGroupClient.EXPECT().
			CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			DoAndReturn(func(
				_ context.Context,
				_, _ string,
				properties armnetwork.SecurityGroup,
			) (*armnetwork.SecurityGroup, error) {
				rules := append(append(noiseRules, upToDateRules...),
					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(505).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // should add to this rule
						Build(),

					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(530).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // should add to this rule
						Build(),

					azureFx.
						AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(520).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...). // should add to this rule
						Build(),
				)

				testutil.ExpectExactSecurityRules(t, &properties, rules)

				return nil, nil
			}).Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, nil).
			Times(1)
		loadBalancerBackendPool.EXPECT().
			GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
			Return(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
			).
			Times(1)

		_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
		assert.NoError(t, err)
	})

	t.Run("update rules - keep retain ports - external IPs", func(t *testing.T) {
		var (
			ingressIPs   = azureFx.LoadBalancer().IPv4Addresses()
			loadBalancer = azureFx.LoadBalancer().Build()

			allowedServiceTag = azureFx.ServiceTag()
			allowedIPv4Ranges = fx.RandomIPv4PrefixStrings(3)
			allowedIPv6Ranges = fx.RandomIPv6PrefixStrings(3)
			allowedRanges     = append(allowedIPv4Ranges, allowedIPv6Ranges...)
			svc               = k8sFx.Service().WithNamespace("ns-01").WithName("svc-01").
						WithAllowedServiceTags(allowedServiceTag).WithAllowedIPRanges(allowedRanges...).
						WithIngressIPs(ingressIPs).
						Build()
			sharedIPSvc = k8sFx.Service().
					WithNamespace("ns-02").
					WithName("svc-02").
					WithIngressIPs(ingressIPs).
					Build()
		)

		sharedIPSvc.Spec.Ports = []v1.ServicePort{
			{
				Name:     "port-1",
				Protocol: v1.ProtocolTCP,
				Port:     18000,
				NodePort: 48000,
			},
			{
				Name:     "port2",
				Protocol: v1.ProtocolTCP,
				Port:     19000,
				NodePort: 49000,
			},
		}

		tests := []struct {
			Name                 string
			RulesBeforeReconcile []*armnetwork.SecurityRule
			RulesAfterReconcile  []*armnetwork.SecurityRule
		}{
			{
				Name:                 "add rules",
				RulesBeforeReconcile: azureFx.NoiseSecurityRules(),
				RulesAfterReconcile: append(azureFx.NoiseSecurityRules(), []*armnetwork.SecurityRule{
					// TCP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(500).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),
					// TCP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(501).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					// TCP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(502).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
					// TCP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(503).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),

					// UDP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(504).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),
					// UDP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(505).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					// UDP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(506).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
					// UDP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(507).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
				}...),
			},
			{
				Name: "update rules - for load balancer IP only",
				RulesBeforeReconcile: append(azureFx.NoiseSecurityRules(), []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"Internet"}, []int32{18000, 19000, 80}).
						WithPriority(500).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),

					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{"Internet"}, []int32{18000, 19000, 80}).
						WithPriority(501).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),
				}...),
				RulesAfterReconcile: append(azureFx.NoiseSecurityRules(), []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"Internet"}, []int32{18000, 19000, 80}).
						WithPriority(500).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),

					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{"Internet"}, []int32{18000, 19000, 80}).
						WithPriority(501).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),

					// TCP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(502).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),
					// TCP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(503).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					// TCP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(504).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
					// TCP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(505).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),

					// UDP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(506).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),
					// UDP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(507).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					// UDP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(508).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
					// UDP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(509).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
				}...),
			},
			{
				Name: "update rules",
				RulesBeforeReconcile: append(azureFx.NoiseSecurityRules(), []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"Internet"}, []int32{18000, 19000}).
						WithPriority(500).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{"Internet"}, []int32{18000, 19000}).
						WithPriority(501).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
				}...),
				RulesAfterReconcile: append(azureFx.NoiseSecurityRules(), []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"Internet"}, []int32{18000, 19000}).
						WithPriority(500).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{"Internet"}, []int32{18000, 19000}).
						WithPriority(501).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),

					// TCP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(502).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),
					// TCP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(503).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					// TCP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
						WithPriority(504).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
					// TCP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
						WithPriority(505).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),

					// UDP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(506).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),
					// UDP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(507).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					// UDP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
						WithPriority(508).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
					// UDP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
						WithPriority(509).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
				}...),
			},
		}

		for _, tt := range tests {
			t.Run(tt.Name, func(t *testing.T) {
				var (
					ctrl                    = gomock.NewController(t)
					az                      = GetTestCloud(ctrl)
					securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
					loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
					loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				)
				defer ctrl.Finish()

				runtimeObjects := []runtime.Object{
					&sharedIPSvc, &svc,
				}
				runtimeObjects = append(runtimeObjects, makeNodesByIPs(
					append(
						azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
						azureFx.LoadBalancer().BackendPoolIPv6Addresses()...,
					))...,
				)
				kubeClient := fake.NewSimpleClientset(runtimeObjects...)
				informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
				az.serviceLister = informerFactory.Core().V1().Services().Lister()
				az.nodeLister = informerFactory.Core().V1().Nodes().Lister()
				informerFactory.Start(wait.NeverStop)
				informerFactory.WaitForCacheSync(wait.NeverStop)

				securityGroup := azureFx.SecurityGroup().WithRules(tt.RulesBeforeReconcile).Build()

				securityGroupClient.EXPECT().
					Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
					Return(securityGroup, nil).
					Times(1)
				securityGroupClient.EXPECT().
					CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
					DoAndReturn(func(
						_ context.Context,
						_, _ string,
						properties armnetwork.SecurityGroup,
					) (*armnetwork.SecurityGroup, error) {
						testutil.ExpectExactSecurityRules(t, &properties, tt.RulesAfterReconcile)
						return nil, nil
					}).Times(1)
				loadBalancerClient.EXPECT().
					Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
					Return(loadBalancer, nil).
					Times(1)
				loadBalancerBackendPool.EXPECT().
					GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
					Return(
						azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
						azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
					).
					Times(1)

				_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
				assert.NoError(t, err)
			})
		}
	})

	t.Run("update rules - disabled floating IP and NSG rule management disabled", func(t *testing.T) {
		var (
			ctrl                    = gomock.NewController(t)
			az                      = GetTestCloud(ctrl)
			securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
			loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
			loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
			loadBalancer            = azureFx.LoadBalancer().Build()

			svc = k8sFx.Service().
				WithNamespace("ns-01").
				WithName("svc-01").
				WithDisableFloatingIP().
				WithDisableLoadBalancerNSGRule().
				Build()
		)
		defer ctrl.Finish()

		runtimeObjects := []runtime.Object{&svc}
		runtimeObjects = append(runtimeObjects, makeNodesByIPs(
			append(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses()...,
			))...,
		)
		kubeClient := fake.NewSimpleClientset(runtimeObjects...)
		informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
		az.serviceLister = informerFactory.Core().V1().Services().Lister()
		az.nodeLister = informerFactory.Core().V1().Nodes().Lister()
		informerFactory.Start(wait.NeverStop)
		informerFactory.WaitForCacheSync(wait.NeverStop)

		noiseRules := azureFx.NoiseSecurityRules()
		staleRules := []*armnetwork.SecurityRule{
			azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{securitygroup.ServiceTagInternet}, k8sFx.Service().TCPNodePorts()).
				WithPriority(500).
				WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
				Build(),
			azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{securitygroup.ServiceTagInternet}, k8sFx.Service().TCPNodePorts()).
				WithPriority(501).
				WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
				Build(),
			azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{securitygroup.ServiceTagInternet}, k8sFx.Service().UDPNodePorts()).
				WithPriority(502).
				WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
				Build(),
			azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{securitygroup.ServiceTagInternet}, k8sFx.Service().UDPNodePorts()).
				WithPriority(503).
				WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
				Build(),
		}
		securityGroup := azureFx.SecurityGroup().WithRules(append(noiseRules, staleRules...)).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
			Return(securityGroup, nil).
			Times(1)
		securityGroupClient.EXPECT().
			CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			DoAndReturn(func(
				_ context.Context,
				_, _ string,
				properties armnetwork.SecurityGroup,
			) (*armnetwork.SecurityGroup, error) {
				testutil.ExpectExactSecurityRules(t, &properties, noiseRules)
				return nil, nil
			}).Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, nil).
			Times(1)
		loadBalancerBackendPool.EXPECT().
			GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
			Return(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
			).
			Times(1)

		_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
		assert.NoError(t, err)
	})

	t.Run("update rules - NSG rule management disabled", func(t *testing.T) {
		var (
			ctrl                    = gomock.NewController(t)
			az                      = GetTestCloud(ctrl)
			securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
			loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
			loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
			loadBalancer            = azureFx.LoadBalancer().Build()

			svc = k8sFx.Service().
				WithNamespace("ns-01").
				WithName("svc-01").
				WithDisableLoadBalancerNSGRule().
				Build()
		)
		defer ctrl.Finish()

		runtimeObjects := []runtime.Object{&svc}
		runtimeObjects = append(runtimeObjects, makeNodesByIPs(
			append(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses()...,
			))...,
		)
		kubeClient := fake.NewSimpleClientset(runtimeObjects...)
		informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
		az.serviceLister = informerFactory.Core().V1().Services().Lister()
		az.nodeLister = informerFactory.Core().V1().Nodes().Lister()
		informerFactory.Start(wait.NeverStop)
		informerFactory.WaitForCacheSync(wait.NeverStop)

		noiseRules := azureFx.NoiseSecurityRules()
		staleRules := []*armnetwork.SecurityRule{
			azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{securitygroup.ServiceTagInternet}, k8sFx.Service().TCPPorts()).
				WithPriority(500).
				WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
				Build(),
			azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{securitygroup.ServiceTagInternet}, k8sFx.Service().TCPPorts()).
				WithPriority(501).
				WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
				Build(),
			azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{securitygroup.ServiceTagInternet}, k8sFx.Service().UDPPorts()).
				WithPriority(502).
				WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
				Build(),
			azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{securitygroup.ServiceTagInternet}, k8sFx.Service().UDPPorts()).
				WithPriority(503).
				WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
				Build(),
		}
		securityGroup := azureFx.SecurityGroup().WithRules(append(noiseRules, staleRules...)).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
			Return(securityGroup, nil).
			Times(1)
		securityGroupClient.EXPECT().
			CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			DoAndReturn(func(
				_ context.Context,
				_, _ string,
				properties armnetwork.SecurityGroup,
			) (*armnetwork.SecurityGroup, error) {
				testutil.ExpectExactSecurityRules(t, &properties, noiseRules)
				return nil, nil
			}).Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, nil).
			Times(1)
		loadBalancerBackendPool.EXPECT().
			GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
			Return(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
			).
			Times(1)

		_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
		assert.NoError(t, err)
	})

	t.Run("update rules - keep retain ports - disable floating IP", func(t *testing.T) {
		var (
			ingressIPs   = azureFx.LoadBalancer().IPv4Addresses()
			loadBalancer = azureFx.LoadBalancer().Build()

			allowedServiceTag = azureFx.ServiceTag()
			allowedIPv4Ranges = fx.RandomIPv4PrefixStrings(3)
			allowedIPv6Ranges = fx.RandomIPv6PrefixStrings(3)
			allowedRanges     = append(allowedIPv4Ranges, allowedIPv6Ranges...)
			svc               = k8sFx.Service().WithNamespace("ns-01").WithName("svc-01").
						WithAllowedServiceTags(allowedServiceTag).WithAllowedIPRanges(allowedRanges...).
						WithDisableFloatingIP().
						WithIngressIPs(ingressIPs).
						Build()
			sharedIPSvc = k8sFx.Service().
					WithNamespace("ns-02").
					WithName("svc-02").
					WithDisableFloatingIP().
					WithIngressIPs(ingressIPs).
					Build()
		)

		sharedIPSvc.Spec.Ports = []v1.ServicePort{
			{
				Name:     "port-1",
				Protocol: v1.ProtocolTCP,
				Port:     18000,
				NodePort: 48000,
			},
			{
				Name:     "port2",
				Protocol: v1.ProtocolTCP,
				Port:     19000,
				NodePort: 49000,
			},
		}

		tests := []struct {
			Name                 string
			RulesBeforeReconcile []*armnetwork.SecurityRule
			RulesAfterReconcile  []*armnetwork.SecurityRule
		}{
			{
				Name:                 "add rules",
				RulesBeforeReconcile: azureFx.NoiseSecurityRules(),
				RulesAfterReconcile: append(azureFx.NoiseSecurityRules(), []*armnetwork.SecurityRule{
					// TCP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPNodePorts()).
						WithPriority(500).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),
					// TCP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPNodePorts()).
						WithPriority(501).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),

					// TCP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPNodePorts()).
						WithPriority(502).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),
					// TCP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPNodePorts()).
						WithPriority(503).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),

					// UDP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPNodePorts()).
						WithPriority(504).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),
					// UDP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPNodePorts()).
						WithPriority(505).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),

					// UDP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPNodePorts()).
						WithPriority(506).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),
					// UDP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPNodePorts()).
						WithPriority(507).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),
				}...),
			},
			{
				Name: "update rules - for backend pool IP only",
				RulesBeforeReconcile: append(azureFx.NoiseSecurityRules(), []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"Internet"}, []int32{18000, 19000, 80}).
						WithPriority(500).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{"Internet"}, []int32{18000, 19000, 80}).
						WithPriority(501).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
				}...),
				RulesAfterReconcile: append(azureFx.NoiseSecurityRules(), []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"Internet"}, []int32{18000, 19000, 80}).
						WithPriority(500).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{"Internet"}, []int32{18000, 19000, 80}).
						WithPriority(501).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
					// TCP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPNodePorts()).
						WithPriority(502).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),
					// TCP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPNodePorts()).
						WithPriority(503).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),

					// TCP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPNodePorts()).
						WithPriority(504).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),
					// TCP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPNodePorts()).
						WithPriority(505).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),

					// UDP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPNodePorts()).
						WithPriority(506).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),
					// UDP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPNodePorts()).
						WithPriority(507).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),

					// UDP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPNodePorts()).
						WithPriority(508).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),
					// UDP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPNodePorts()).
						WithPriority(509).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),
				}...),
			},
			{
				Name: "update rules",
				RulesBeforeReconcile: append(azureFx.NoiseSecurityRules(), []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"Internet"}, []int32{48000, 49000}).
						WithPriority(500).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{"Internet"}, []int32{48000, 49000}).
						WithPriority(501).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
				}...),
				RulesAfterReconcile: append(azureFx.NoiseSecurityRules(), []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"Internet"}, []int32{48000, 49000}).
						WithPriority(500).
						WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
						Build(),

					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{"Internet"}, []int32{48000, 49000}).
						WithPriority(501).
						WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
						Build(),
					// TCP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPNodePorts()).
						WithPriority(502).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),
					// TCP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPNodePorts()).
						WithPriority(503).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),

					// TCP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPNodePorts()).
						WithPriority(504).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),
					// TCP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPNodePorts()).
						WithPriority(505).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),

					// UDP + IPv4 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPNodePorts()).
						WithPriority(506).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),
					// UDP + IPv4 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().UDPNodePorts()).
						WithPriority(507).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv4Addresses()...).
						Build(),

					// UDP + IPv6 + ServiceTag
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().UDPNodePorts()).
						WithPriority(508).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),
					// UDP + IPv6 + IPs
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPNodePorts()).
						WithPriority(509).
						WithDestination(azureFx.LoadBalancer().BackendPoolIPv6Addresses()...).
						Build(),
				}...),
			},
		}

		for _, tt := range tests {
			t.Run(tt.Name, func(t *testing.T) {
				t.Parallel()
				var (
					ctrl                    = gomock.NewController(t)
					az                      = GetTestCloud(ctrl)
					securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
					loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
					loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				)
				defer ctrl.Finish()

				runtimeObjects := []runtime.Object{
					&sharedIPSvc, &svc,
				}
				runtimeObjects = append(runtimeObjects, makeNodesByIPs(
					append(
						azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
						azureFx.LoadBalancer().BackendPoolIPv6Addresses()...,
					))...,
				)
				kubeClient := fake.NewSimpleClientset(runtimeObjects...)
				informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
				az.serviceLister = informerFactory.Core().V1().Services().Lister()
				az.nodeLister = informerFactory.Core().V1().Nodes().Lister()
				informerFactory.Start(wait.NeverStop)
				informerFactory.WaitForCacheSync(wait.NeverStop)

				securityGroup := azureFx.SecurityGroup().WithRules(tt.RulesBeforeReconcile).Build()

				securityGroupClient.EXPECT().
					Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
					Return(securityGroup, nil).
					Times(1)
				securityGroupClient.EXPECT().
					CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
					DoAndReturn(func(
						_ context.Context,
						_, _ string,
						properties armnetwork.SecurityGroup,
					) (*armnetwork.SecurityGroup, error) {
						testutil.ExpectExactSecurityRules(t, &properties, tt.RulesAfterReconcile)
						return nil, nil
					}).Times(1)
				loadBalancerClient.EXPECT().
					Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
					Return(loadBalancer, nil).
					Times(1)
				loadBalancerBackendPool.EXPECT().
					GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
					Return(
						azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
						azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
					).
					Times(1)

				_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
				assert.NoError(t, err)
			})
		}
	})

	t.Run("clean rules - when deleting LB / AzureLoadBalancer had been created", func(t *testing.T) {
		var (
			ctrl                    = gomock.NewController(t)
			az                      = GetTestCloud(ctrl)
			securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
			loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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
			noiseRules = azureFx.NoiseSecurityRules()
			staleRules = []*armnetwork.SecurityRule{
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(509).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...). // should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(3000).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "foo")...). // should keep foo
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{8000}).
					WithPriority(4000).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...). // Should remove the rule
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
					WithPriority(4001).
					WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "foo", "bar")...). // Should keep foo and bar but clean the rest
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
					WithPriority(4002).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "baz")...). // Should keep baz but clean the rest
					Build(),

				azureFx.DenyAllSecurityRule(iputil.IPv4).
					WithPriority(4095).
					WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "5.5.5.5/32")...).
					Build(),
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(505).
					WithDestination("foo"). // should keep it
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(520).
					WithDestination("baz", "quo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(530).
					WithDestination("bar"). // should add to this rule
					Build(),
			}
			upToDateRules = []*armnetwork.SecurityRule{

				{
					Name: ptr.To("foo"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolTCP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      to.SliceOfPtrs("foo"),
						DestinationPortRanges:      to.SliceOfPtrs("4000", "6000"),
						DestinationAddressPrefixes: to.SliceOfPtrs(azureFx.LoadBalancer().Addresses()...), // Should remove the rule
						Priority:                   ptr.To(int32(4003)),
					},
				},
				{
					Name: ptr.To("bar"),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						Protocol:                   to.Ptr(armnetwork.SecurityRuleProtocolUDP),
						Access:                     to.Ptr(armnetwork.SecurityRuleAccessAllow),
						Direction:                  to.Ptr(armnetwork.SecurityRuleDirectionInbound),
						SourcePortRange:            ptr.To("*"),
						SourceAddressPrefixes:      to.SliceOfPtrs("bar"),
						DestinationPortRanges:      to.SliceOfPtrs("5000", "6000"),
						DestinationAddressPrefixes: to.SliceOfPtrs(append(azureFx.LoadBalancer().Addresses(), "bar")...), // Should keep bar but clean the rest
						Priority:                   ptr.To(int32(4004)),
					},
				},
			}
		)

		securityGroup := azureFx.SecurityGroup().WithRules(
			append(append(noiseRules, upToDateRules...), staleRules...),
		).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
			Return(securityGroup, nil).
			Times(1)
		securityGroupClient.EXPECT().
			CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			DoAndReturn(func(
				_ context.Context,
				_, _ string,
				properties armnetwork.SecurityGroup,
			) (*armnetwork.SecurityGroup, error) {
				rules := append(noiseRules, upToDateRules...)
				testutil.ExpectExactSecurityRules(t, &properties, rules)
				return nil, nil
			}).Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, nil).
			Times(1)
		loadBalancerBackendPool.EXPECT().
			GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
			Return(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
			).
			Times(1)

		_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), false) // deleting
		assert.NoError(t, err)
	})

	t.Run("clean rules - when deleting LB / AzureLoadBalancer had been created / service with invalid annotation", func(t *testing.T) {
		var (
			ctrl                    = gomock.NewController(t)
			az                      = GetTestCloud(ctrl)
			securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
			loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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
			noiseRules = azureFx.NoiseSecurityRules()
			staleRules = []*armnetwork.SecurityRule{
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, allowedIPv4Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(509).
					WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().UDPPorts()).
					WithPriority(3000).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "foo")...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{8000}).
					WithPriority(4000).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, []int32{6000, 3000}).
					WithPriority(4001).
					WithDestination(append(azureFx.LoadBalancer().IPv4Addresses(), "foo", "bar")...).
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, allowedIPv6Ranges, []int32{9000}).
					WithPriority(4002).
					WithDestination(append(azureFx.LoadBalancer().IPv6Addresses(), "baz")...).
					Build(),
			}
			upToDateRules = []*armnetwork.SecurityRule{
				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().TCPPorts()).
					WithPriority(505).
					WithDestination("foo"). // should keep it
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, allowedIPv6Ranges, k8sFx.Service().TCPPorts()).
					WithPriority(520).
					WithDestination("baz", "quo"). // should add to this rule
					Build(),

				azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{allowedServiceTag}, k8sFx.Service().UDPPorts()).
					WithPriority(530).
					WithDestination("bar"). // should add to this rule
					Build(),
			}
		)

		securityGroup := azureFx.SecurityGroup().WithRules(
			append(append(noiseRules, upToDateRules...), staleRules...),
		).Build()

		securityGroupClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
			Return(securityGroup, nil).
			Times(1)
		securityGroupClient.EXPECT().
			CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
			DoAndReturn(func(
				_ context.Context,
				_, _ string,
				properties armnetwork.SecurityGroup,
			) (*armnetwork.SecurityGroup, error) {
				testutil.ExpectExactSecurityRules(t, &properties, noiseRules)

				return nil, nil
			}).Times(1)
		loadBalancerClient.EXPECT().
			Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
			Return(loadBalancer, nil).
			Times(1)
		loadBalancerBackendPool.EXPECT().
			GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
			Return(
				azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
				azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
			).
			Times(1)

		_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), false) // deleting
		assert.NoError(t, err)
	})

	t.Run("validation events", func(t *testing.T) {
		reconcile := func(
			t *testing.T, svc *v1.Service, lbIPs []string, wantLb bool,
			staleRules ...*armnetwork.SecurityRule,
		) (*armnetwork.SecurityGroup, []string, error) {
			t.Helper()
			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				securityGroup           = azureFx.SecurityGroup().WithRules(append(azureFx.NoiseSecurityRules(), staleRules...)).Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			// GetTestCloud's zero-value FakeRecorder has a nil channel that discards events.
			recorder := record.NewFakeRecorder(10)
			az.eventRecorder = recorder

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				AnyTimes()
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(nil, nil).
				AnyTimes()
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				AnyTimes()
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				AnyTimes()

			rv, err := az.reconcileSecurityGroup(ctx, ClusterName, svc, *loadBalancer.Name, lbIPs, wantLb)

			var events []string
			for len(recorder.Events) > 0 {
				events = append(events, <-recorder.Events)
			}
			return rv, events, err
		}

		for _, tt := range []struct {
			name           string
			families       []v1.IPFamily
			ipRanges       []string
			sourceRanges   []string
			serviceTags    []string
			expectedReason string
			expectedRules  []*armnetwork.SecurityRule
		}{
			{
				name:           "it should emit IPRangeFamilyMismatch and leave the frontend with no allow rule when ensuring an IPv4 Service with IPv6-only azure-allowed-ip-ranges",
				families:       []v1.IPFamily{v1.IPv4Protocol},
				ipRanges:       []string{"2001:db8:85a3::/64"},
				expectedReason: "IPRangeFamilyMismatch",
			},
			{
				name:           "it should emit IPRangeFamilyMismatch and leave the frontend with no allow rule when ensuring an IPv6 Service with IPv4-only azure-allowed-ip-ranges",
				families:       []v1.IPFamily{v1.IPv6Protocol},
				ipRanges:       []string{"10.0.0.0/24"},
				expectedReason: "IPRangeFamilyMismatch",
			},
			{
				name:           "it should emit IPRangeFamilyMismatch and not open the frontend to the Internet when ensuring an IPv4 Service with an allow-all IPv6 range in azure-allowed-ip-ranges",
				families:       []v1.IPFamily{v1.IPv4Protocol},
				ipRanges:       []string{"::/0"},
				expectedReason: "IPRangeFamilyMismatch",
			},
			{
				name:           "it should emit IPRangeFamilyMismatch and not open the frontend to the Internet when ensuring an IPv6 Service with an allow-all IPv4 range in azure-allowed-ip-ranges",
				families:       []v1.IPFamily{v1.IPv6Protocol},
				ipRanges:       []string{"0.0.0.0/0"},
				expectedReason: "IPRangeFamilyMismatch",
			},
			{
				name:           "it should emit IPRangeFamilyMismatch and still add the service tag rules when ensuring an IPv4 Service with IPv6-only azure-allowed-ip-ranges and a service tag",
				families:       []v1.IPFamily{v1.IPv4Protocol},
				ipRanges:       []string{"2001:db8:85a3::/64"},
				serviceTags:    []string{"AzureCloud"},
				expectedReason: "IPRangeFamilyMismatch",
				expectedRules: []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"AzureCloud"}, k8sFx.Service().TCPPorts()).
						WithPriority(500).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{"AzureCloud"}, k8sFx.Service().UDPPorts()).
						WithPriority(501).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
				},
			},
			{
				name:           "it should emit IPRangeFamilyMismatch and still add the service tag rules when ensuring an IPv6 Service with IPv4-only azure-allowed-ip-ranges and a service tag",
				families:       []v1.IPFamily{v1.IPv6Protocol},
				ipRanges:       []string{"10.0.0.0/24"},
				serviceTags:    []string{"AzureCloud"},
				expectedReason: "IPRangeFamilyMismatch",
				expectedRules: []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{"AzureCloud"}, k8sFx.Service().TCPPorts()).
						WithPriority(500).WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{"AzureCloud"}, k8sFx.Service().UDPPorts()).
						WithPriority(501).WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).Build(),
				},
			},
			{
				name:     "it should not emit any event and add rules for both families when ensuring a dual-stack Service with both IPv4 and IPv6 azure-allowed-ip-ranges",
				families: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				ipRanges: []string{"10.0.0.0/24", "2001:db8:85a3::/64"},
				expectedRules: []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"10.0.0.0/24"}, k8sFx.Service().TCPPorts()).
						WithPriority(500).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{"2001:db8:85a3::/64"}, k8sFx.Service().TCPPorts()).
						WithPriority(501).WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{"10.0.0.0/24"}, k8sFx.Service().UDPPorts()).
						WithPriority(502).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{"2001:db8:85a3::/64"}, k8sFx.Service().UDPPorts()).
						WithPriority(503).WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).Build(),
				},
			},
			{
				name:           "it should emit InvalidAllowedIPRanges and add DenyAll rules when ensuring a Service with an invalid range in azure-allowed-ip-ranges",
				ipRanges:       []string{"foo", "10.0.0.0/24"},
				expectedReason: "InvalidAllowedIPRanges",
				expectedRules: []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"10.0.0.0/24"}, k8sFx.Service().TCPPorts()).
						WithPriority(500).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{"10.0.0.0/24"}, k8sFx.Service().UDPPorts()).
						WithPriority(501).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
					azureFx.DenyAllSecurityRule(iputil.IPv6).
						WithPriority(4094).WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).Build(),
					azureFx.DenyAllSecurityRule(iputil.IPv4).
						WithPriority(4095).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
				},
			},
			{
				name:           "it should emit ConflictConfiguration and add rules for both the spec ranges and the service tag when ensuring a Service with spec.loadBalancerSourceRanges and service tags",
				sourceRanges:   []string{"20.0.0.1/32"},
				serviceTags:    []string{"AKS"},
				expectedReason: "ConflictConfiguration",
				expectedRules: []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"AKS"}, k8sFx.Service().TCPPorts()).
						WithPriority(500).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"20.0.0.1/32"}, k8sFx.Service().TCPPorts()).
						WithPriority(501).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv6, []string{"AKS"}, k8sFx.Service().TCPPorts()).
						WithPriority(502).WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{"AKS"}, k8sFx.Service().UDPPorts()).
						WithPriority(503).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{"20.0.0.1/32"}, k8sFx.Service().UDPPorts()).
						WithPriority(504).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv6, []string{"AKS"}, k8sFx.Service().UDPPorts()).
						WithPriority(505).WithDestination(azureFx.LoadBalancer().IPv6Addresses()...).Build(),
				},
			},
			{
				name:     "it should not emit any event and add the allow rules when ensuring a Service with a valid range in azure-allowed-ip-ranges",
				ipRanges: []string{"10.0.0.0/24"},
				expectedRules: []*armnetwork.SecurityRule{
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"10.0.0.0/24"}, k8sFx.Service().TCPPorts()).
						WithPriority(500).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
					azureFx.AllowSecurityRule(armnetwork.SecurityRuleProtocolUDP, iputil.IPv4, []string{"10.0.0.0/24"}, k8sFx.Service().UDPPorts()).
						WithPriority(501).WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).Build(),
				},
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				builder := k8sFx.Service().WithIPFamilies(tt.families...)
				if len(tt.ipRanges) > 0 {
					builder = builder.WithAllowedIPRanges(tt.ipRanges...)
				}
				if len(tt.sourceRanges) > 0 {
					builder = builder.WithLoadBalancerSourceRanges(tt.sourceRanges...)
				}
				if len(tt.serviceTags) > 0 {
					builder = builder.WithAllowedServiceTags(tt.serviceTags...)
				}
				svc := builder.Build()

				// A single-stack Service has an LB frontend of only its own family.
				lbIPs := azureFx.LoadBalancer().Addresses()
				switch {
				case len(tt.families) == 1 && tt.families[0] == v1.IPv4Protocol:
					lbIPs = azureFx.LoadBalancer().IPv4Addresses()
				case len(tt.families) == 1 && tt.families[0] == v1.IPv6Protocol:
					lbIPs = azureFx.LoadBalancer().IPv6Addresses()
				}

				sg, events, err := reconcile(t, &svc, lbIPs, EnsureLB)
				assert.NoError(t, err, "a rejected configuration must not block reconcile")
				if tt.expectedReason != "" {
					assert.Contains(t, strings.Join(events, "\n"), tt.expectedReason)
				} else {
					assert.Empty(t, events)
				}
				testutil.ExpectExactSecurityRules(t, sg, append(azureFx.NoiseSecurityRules(), tt.expectedRules...))
			})
		}

		for _, tt := range []struct {
			name string
			svc  v1.Service
		}{
			{
				name: "it should skip validation and clean up when deleting a Service with an invalid range in azure-allowed-ip-ranges",
				svc:  k8sFx.Service().WithAllowedIPRanges("foo", "10.0.0.0/24").Build(),
			},
			{
				name: "it should skip validation and clean up when deleting a Service with spec.loadBalancerSourceRanges and service tags",
				svc:  k8sFx.Service().WithLoadBalancerSourceRanges("20.0.0.1/32").WithAllowedServiceTags("AKS").Build(),
			},
			{
				name: "it should skip validation and clean up when deleting a Service with an IP family mismatch in azure-allowed-ip-ranges",
				svc:  k8sFx.Service().WithIPFamilies(v1.IPv4Protocol).WithAllowedIPRanges("2001:db8:85a3::/64").Build(),
			},
			{
				name: "it should skip validation and clean up when deleting a Service with both spec.loadBalancerSourceRanges and azure-allowed-ip-ranges",
				svc:  k8sFx.Service().WithLoadBalancerSourceRanges("20.0.0.1/32").WithAllowedIPRanges("10.0.0.1/32").Build(),
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				svc := tt.svc

				// Without a rule to clean up, the assertion below cannot fail.
				staleRule := azureFx.
					AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, iputil.IPv4, []string{"10.0.0.0/24"}, k8sFx.Service().TCPPorts()).
					WithPriority(507).
					WithDestination(azureFx.LoadBalancer().IPv4Addresses()...).
					Build()

				sg, events, err := reconcile(t, &svc, azureFx.LoadBalancer().IPv4Addresses(), false, staleRule)
				assert.NoError(t, err, "configuration validation must not block deletion")
				assert.Empty(t, events)
				testutil.ExpectExactSecurityRules(t, sg, azureFx.NoiseSecurityRules())
			})
		}
	})

	t.Run("negative cases", func(t *testing.T) {
		t.Run("with both `service.beta.kubernetes.io/azure-allowed-ip-ranges` and `spec.loadBalancerSourceRanges` specified", func(t *testing.T) {
			var (
				ctrl                = gomock.NewController(t)
				az                  = GetTestCloud(ctrl)
				securityGroupClient = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
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
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.Error(t, err)
			assert.ErrorIs(t, err, loadbalancer.ErrSetBothLoadBalancerSourceRangesAndAllowedIPRanges)
		})

		t.Run("when SecurityGroupClient.Get returns error", func(t *testing.T) {
			var (
				ctrl                = gomock.NewController(t)
				az                  = GetTestCloud(ctrl)
				securityGroupClient = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				svc                 = k8sFx.Service().Build()
				securityGroup       = azureFx.SecurityGroup().Build()
				loadBalancer        = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()
			expectedErr := &azcore.ResponseError{
				ErrorCode: "foo",
				RawResponse: &http.Response{
					Body: io.NopCloser(strings.NewReader("foo")),
				},
			}

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, expectedErr).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.Error(t, err)
			assert.ErrorIs(t, err, expectedErr)
		})

		t.Run("when NetworkClientFactory.GetLoadBalancerClient().Get returns error", func(t *testing.T) {
			var (
				ctrl                = gomock.NewController(t)
				az                  = GetTestCloud(ctrl)
				securityGroupClient = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient  = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				svc                 = k8sFx.Service().Build()
				securityGroup       = azureFx.SecurityGroup().Build()
				loadBalancer        = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			expectedErr := &azcore.ResponseError{ErrorCode: "foo"}

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, expectedErr).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.Error(t, err)
			assert.ErrorIs(t, err, expectedErr)
		})

		t.Run("when SecurityGroupClient.CreateOrUpdate returns error", func(t *testing.T) {

			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			expectedErr := &azcore.ResponseError{
				ErrorCode: "foo",
				RawResponse: &http.Response{
					Body: io.NopCloser(strings.NewReader("foo")),
				},
			}
			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			securityGroupClient.EXPECT().
				CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
				Return(nil, expectedErr).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.Error(t, err)
			assert.ErrorIs(t, err, expectedErr)
		})

		t.Run("when the number of rules exceeds the limit", func(t *testing.T) {

			var (
				ctrl                    = gomock.NewController(t)
				az                      = GetTestCloud(ctrl)
				securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
				loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
				svc                     = k8sFx.Service().Build()
				securityGroup           = azureFx.SecurityGroup().WithRules(azureFx.NNoiseSecurityRules(securitygroup.MaxSecurityRulesPerGroup)).Build()
				loadBalancer            = azureFx.LoadBalancer().Build()
			)
			defer ctrl.Finish()

			securityGroupClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
				Return(securityGroup, nil).
				Times(1)
			loadBalancerClient.EXPECT().
				Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
				Return(loadBalancer, nil).
				Times(1)
			loadBalancerBackendPool.EXPECT().
				GetBackendPrivateIPs(gomock.Any(), ClusterName, &svc, loadBalancer).
				Return(
					azureFx.LoadBalancer().BackendPoolIPv4Addresses(),
					azureFx.LoadBalancer().BackendPoolIPv6Addresses(),
				).
				Times(1)

			_, err := az.reconcileSecurityGroup(ctx, ClusterName, &svc, *loadBalancer.Name, azureFx.LoadBalancer().Addresses(), EnsureLB)
			assert.Error(t, err)
		})
	})

	// There is one deny-all rule per IP family, so every Service's destinations share it. A
	// destination must stay in the rule while any Service still needs it.
	t.Run("deny all rules - destinations other Services still need", func(t *testing.T) {
		const (
			// Addresses that more than one Service can be on.
			sharedIP          = "10.0.0.1"
			sharedIPv6        = "2001:db8::1"
			unmanagedPublicIP = "203.0.113.5"

			// Addresses that belong to a single Service.
			svcAUnsharedIP        = "10.0.0.8"
			svcAUnmanagedPublicIP = "203.0.113.6"
			svcBUnsharedIP        = "10.0.0.9"
			svcBUnmanagedPublicIP = "203.0.113.7"

			backendNodeIP = "192.168.10.1"

			// Services sharing an IP must expose distinct ports, so each gets its own allow rule.
			portA = int32(18080)
			portB = int32(18081)
			portC = int32(18082)
		)

		var (
			sourceRanges   = []string{"198.51.100.0/24"}
			sourceRangesV6 = []string{"2001:db8:1::/48"}
			dualStackRange = append(append([]string{}, sourceRanges...), sourceRangesV6...)
		)

		service := func(namespace, name string, ingressIPs ...string) *fixture.KubernetesServiceFixture {
			return k8sFx.Service().WithNamespace(namespace).WithName(name).
				WithIngressIPs(ingressIPs).
				WithLoadBalancerSourceRanges(sourceRanges...)
		}

		denyAll := func(namespace, name string, ingressIPs ...string) *fixture.KubernetesServiceFixture {
			return service(namespace, name, ingressIPs...).WithDenyAllExceptLoadBalancerSourceRanges()
		}

		onPort := func(f *fixture.KubernetesServiceFixture, port int32) *v1.Service {
			svc := f.Build()
			svc.Spec.Ports = []v1.ServicePort{
				{Name: "p", Protocol: v1.ProtocolTCP, Port: port, NodePort: 30000 + port},
			}
			return &svc
		}

		withAdditionalPublicIPs := func(svc *v1.Service, ips ...string) *v1.Service {
			svc.Annotations[consts.ServiceAnnotationAdditionalPublicIPs] = strings.Join(ips, ",")
			return svc
		}

		var (
			svcADenyAll             = onPort(denyAll("ns-a", "svc-a", sharedIP), portA)
			svcBDenyAll             = onPort(denyAll("ns-b", "svc-b", sharedIP), portB)
			svcCDenyAll             = onPort(denyAll("ns-c", "svc-c", sharedIP), portC)
			svcADenyAllOnUnsharedIP = onPort(denyAll("ns-a", "svc-a", svcAUnsharedIP), portA)
			svcBDenyAllOnUnsharedIP = onPort(denyAll("ns-b", "svc-b", svcBUnsharedIP), portB)

			// Services that need no deny-all rule of their own.
			svcBWithoutDenyAll             = onPort(service("ns-b", "svc-b", sharedIP), portB)
			svcBDenyAllWithNSGRuleDisabled = onPort(denyAll("ns-b", "svc-b", sharedIP).WithDisableLoadBalancerNSGRule(), portB)
			// The annotation on its own requests nothing: there is no range to make an exception for.
			svcBWithDenyAllAnnotationOnly = onPort(k8sFx.Service().WithNamespace("ns-b").WithName("svc-b").
							WithIngressIPs([]string{sharedIP}).
							WithDenyAllExceptLoadBalancerSourceRanges(), portB)

			svcADenyAllDualStack = onPort(denyAll("ns-a", "svc-a", sharedIP, sharedIPv6).WithLoadBalancerSourceRanges(dualStackRange...), portA)
			svcBDenyAllDualStack = onPort(denyAll("ns-b", "svc-b", sharedIP, sharedIPv6).WithLoadBalancerSourceRanges(dualStackRange...), portB)

			// An additional public IP also lands in the ingress status.
			svcADenyAllOnSharedIPWithAdditionalPublicIP = withAdditionalPublicIPs(
				onPort(denyAll("ns-a", "svc-a", sharedIP, svcAUnmanagedPublicIP), portA), svcAUnmanagedPublicIP)
			svcBDenyAllOnSharedIPWithAdditionalPublicIP = withAdditionalPublicIPs(
				onPort(denyAll("ns-b", "svc-b", sharedIP, svcBUnmanagedPublicIP), portB), svcBUnmanagedPublicIP)
			svcADenyAllWithAdditionalPublicIP = withAdditionalPublicIPs(
				onPort(denyAll("ns-a", "svc-a", svcAUnsharedIP, unmanagedPublicIP), portA), unmanagedPublicIP)
			svcBDenyAllWithAdditionalPublicIP = withAdditionalPublicIPs(
				onPort(denyAll("ns-b", "svc-b", svcBUnsharedIP, unmanagedPublicIP), portB), unmanagedPublicIP)

			// Services whose rules target the nodes instead of a frontend IP.
			svcADenyAllWithFloatingIPDisabledOnUnsharedIP          = onPort(denyAll("ns-a", "svc-a", svcAUnsharedIP).WithDisableFloatingIP(), portA)
			svcBDenyAllWithFloatingIPDisabledOnUnsharedIP          = onPort(denyAll("ns-b", "svc-b", svcBUnsharedIP).WithDisableFloatingIP(), portB)
			svcBDenyAllWithFloatingIPDisabledOnSharedIP            = onPort(denyAll("ns-b", "svc-b", sharedIP).WithDisableFloatingIP(), portB)
			svcADenyAllWithFloatingIPDisabledAndAdditionalPublicIP = withAdditionalPublicIPs(
				onPort(denyAll("ns-a", "svc-a", svcAUnsharedIP, unmanagedPublicIP).WithDisableFloatingIP(), portA), unmanagedPublicIP)
			svcBDenyAllWithFloatingIPDisabledAndAdditionalPublicIP = withAdditionalPublicIPs(
				onPort(denyAll("ns-b", "svc-b", svcBUnsharedIP, unmanagedPublicIP).WithDisableFloatingIP(), portB), unmanagedPublicIP)
		)

		allowRule := func(family iputil.Family, port int32, priority int32, dsts ...string) *armnetwork.SecurityRule {
			srcs := sourceRanges
			if family == iputil.IPv6 {
				srcs = sourceRangesV6
			}
			return azureFx.
				AllowSecurityRule(armnetwork.SecurityRuleProtocolTCP, family, srcs, []int32{port}).
				WithPriority(priority).
				WithDestination(dsts...).
				Build()
		}

		denyRule := func(family iputil.Family, priority int32, dsts ...string) *armnetwork.SecurityRule {
			return azureFx.DenyAllSecurityRule(family).WithPriority(priority).WithDestination(dsts...).Build()
		}

		ingressIPs := func(svc *v1.Service) []string {
			var rv []string
			for _, ing := range svc.Status.LoadBalancer.Ingress {
				rv = append(rv, ing.IP)
			}
			return rv
		}

		denyDestinations := func(sg *armnetwork.SecurityGroup, family iputil.Family) []string {
			name := securitygroup.GenerateDenyAllSecurityRuleName(family)
			for _, rule := range sg.Properties.SecurityRules {
				if ptr.Deref(rule.Name, "") == name {
					return securitygroup.ListDestinationPrefixes(rule)
				}
			}
			return nil
		}

		tests := []struct {
			Name                        string
			ReconciledService           *v1.Service
			OtherServices               []*v1.Service
			WantLB                      bool
			ExistingRules               []*armnetwork.SecurityRule
			ExpectsSecurityGroupWrite   bool
			ExpectedDenyAllDestinations map[iputil.Family][]string
		}{
			{
				Name:              "keeps the shared IP in the deny-all rule when another deny-all Service is deleted",
				ReconciledService: svcBDenyAll,
				OtherServices:     []*v1.Service{svcADenyAll},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, sharedIP),
					allowRule(iputil.IPv4, portB, 501, sharedIP),
					denyRule(iputil.IPv4, 4095, sharedIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {sharedIP}},
			},
			{
				Name:              "keeps the shared IP in the deny-all rule when only one of the other Services needs it",
				ReconciledService: svcADenyAll,
				OtherServices:     []*v1.Service{svcBWithoutDenyAll, svcCDenyAll},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, sharedIP),
					allowRule(iputil.IPv4, portB, 501, sharedIP),
					allowRule(iputil.IPv4, portC, 502, sharedIP),
					denyRule(iputil.IPv4, 4095, sharedIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {sharedIP}},
			},
			{
				Name:              "removes the shared IP from the deny-all rule when the last deny-all Service is deleted",
				ReconciledService: svcADenyAll,
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, sharedIP),
					denyRule(iputil.IPv4, 4095, sharedIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: nil,
			},
			{
				Name:              "keeps the shared IP in the deny-all rule when a shared-IP Service without deny-all is reconciled",
				ReconciledService: svcBWithoutDenyAll,
				OtherServices:     []*v1.Service{svcADenyAll},
				WantLB:            EnsureLB,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, sharedIP),
					allowRule(iputil.IPv4, portB, 501, sharedIP),
					denyRule(iputil.IPv4, 4095, sharedIP),
				},
				// The rules are already correct, so the reconcile must not write.
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {sharedIP}},
			},
			{
				Name:              "keeps the shared IP in the deny-all rule when a shared-IP Service without deny-all is deleted",
				ReconciledService: svcBWithoutDenyAll,
				OtherServices:     []*v1.Service{svcADenyAll},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, sharedIP),
					allowRule(iputil.IPv4, portB, 501, sharedIP),
					denyRule(iputil.IPv4, 4095, sharedIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {sharedIP}},
			},
			{
				Name:              "removes the shared IP from the deny-all rule when the only other deny-all Service opts out of NSG rule management",
				ReconciledService: svcADenyAll,
				OtherServices:     []*v1.Service{svcBDenyAllWithNSGRuleDisabled},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, sharedIP),
					denyRule(iputil.IPv4, 4095, sharedIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: nil,
			},
			{
				Name:              "removes the shared IP from the deny-all rule when the only other Service sets the deny-all annotation without source ranges",
				ReconciledService: svcADenyAll,
				OtherServices:     []*v1.Service{svcBWithDenyAllAnnotationOnly},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, sharedIP),
					denyRule(iputil.IPv4, 4095, sharedIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: nil,
			},
			{
				Name:              "removes the unshared IP from the deny-all rule and keeps the shared IP in it",
				ReconciledService: svcADenyAllOnSharedIPWithAdditionalPublicIP,
				OtherServices:     []*v1.Service{svcBDenyAll},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, sharedIP, svcAUnmanagedPublicIP),
					allowRule(iputil.IPv4, portB, 501, sharedIP),
					denyRule(iputil.IPv4, 4095, sharedIP, svcAUnmanagedPublicIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {sharedIP}},
			},
			{
				// svc-a never removed that IP, so restoring it is svc-b's reconcile to do.
				Name:              "keeps the shared IP in the deny-all rule but does not add the other deny-all Service's unshared IP",
				ReconciledService: svcADenyAll,
				OtherServices:     []*v1.Service{svcBDenyAllOnSharedIPWithAdditionalPublicIP},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, sharedIP),
					allowRule(iputil.IPv4, portB, 501, sharedIP, svcBUnmanagedPublicIP),
					denyRule(iputil.IPv4, 4095, sharedIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {sharedIP}},
			},
			{
				Name:              "keeps the shared IPv4 and IPv6 addresses in their deny-all rules when a dual-stack Service is deleted",
				ReconciledService: svcBDenyAllDualStack,
				OtherServices:     []*v1.Service{svcADenyAllDualStack},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, sharedIP),
					allowRule(iputil.IPv4, portB, 501, sharedIP),
					allowRule(iputil.IPv6, portA, 502, sharedIPv6),
					allowRule(iputil.IPv6, portB, 503, sharedIPv6),
					denyRule(iputil.IPv4, 4095, sharedIP),
					denyRule(iputil.IPv6, 4094, sharedIPv6),
				},
				ExpectsSecurityGroupWrite: true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{
					iputil.IPv4: {sharedIP},
					iputil.IPv6: {sharedIPv6},
				},
			},
			{
				Name:              "keeps the additional public IP in the deny-all rule when another deny-all Service also lists it",
				ReconciledService: svcADenyAllWithAdditionalPublicIP,
				OtherServices:     []*v1.Service{svcBDenyAllWithAdditionalPublicIP},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, svcAUnsharedIP, unmanagedPublicIP),
					allowRule(iputil.IPv4, portB, 501, svcBUnsharedIP, unmanagedPublicIP),
					denyRule(iputil.IPv4, 4095, svcAUnsharedIP, svcBUnsharedIP, unmanagedPublicIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {svcBUnsharedIP, unmanagedPublicIP}},
			},

			{
				Name:              "keeps the backend node IP in the deny-all rule when another deny-all Service with the floating IP disabled still needs it",
				ReconciledService: svcADenyAllWithFloatingIPDisabledOnUnsharedIP,
				OtherServices:     []*v1.Service{svcBDenyAllWithFloatingIPDisabledOnUnsharedIP},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, 30000+portA, 500, backendNodeIP),
					allowRule(iputil.IPv4, 30000+portB, 501, backendNodeIP),
					denyRule(iputil.IPv4, 4095, backendNodeIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {backendNodeIP}},
			},
			{
				Name:              "removes the IP of a deny-all Service that uses the floating IP but keeps the node IP of deny-all Service that disables it",
				ReconciledService: svcADenyAllOnUnsharedIP,
				OtherServices:     []*v1.Service{svcBDenyAllWithFloatingIPDisabledOnUnsharedIP},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, svcAUnsharedIP),
					allowRule(iputil.IPv4, 30000+portB, 501, backendNodeIP),
					denyRule(iputil.IPv4, 4095, svcAUnsharedIP, backendNodeIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {backendNodeIP}},
			},
			{
				Name:              "removes the backend node IP from the deny-all rule but keeps the IP of a deny-all Service that uses the floating IP",
				ReconciledService: svcADenyAllWithFloatingIPDisabledOnUnsharedIP,
				OtherServices:     []*v1.Service{svcBDenyAllOnUnsharedIP},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, 30000+portA, 500, backendNodeIP),
					allowRule(iputil.IPv4, portB, 501, svcBUnsharedIP),
					denyRule(iputil.IPv4, 4095, backendNodeIP, svcBUnsharedIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {svcBUnsharedIP}},
			},
			{
				// The other Service shares the frontend IP but its rules target the nodes, so it does
				// not need the frontend IP.
				Name:              "removes the shared IP from the deny-all rule when the only other deny-all Service on it disables the floating IP",
				ReconciledService: svcADenyAll,
				OtherServices:     []*v1.Service{svcBDenyAllWithFloatingIPDisabledOnSharedIP},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, portA, 500, sharedIP),
					allowRule(iputil.IPv4, 30000+portB, 501, backendNodeIP),
					denyRule(iputil.IPv4, 4095, sharedIP, backendNodeIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {backendNodeIP}},
			},
			{
				Name:              "keeps the additional public IP in the deny-all rule when another deny-all Service with the floating IP disabled also lists it",
				ReconciledService: svcADenyAllWithFloatingIPDisabledAndAdditionalPublicIP,
				OtherServices:     []*v1.Service{svcBDenyAllWithFloatingIPDisabledAndAdditionalPublicIP},
				WantLB:            false,
				ExistingRules: []*armnetwork.SecurityRule{
					allowRule(iputil.IPv4, 30000+portA, 500, backendNodeIP, unmanagedPublicIP),
					allowRule(iputil.IPv4, 30000+portB, 501, backendNodeIP, unmanagedPublicIP),
					denyRule(iputil.IPv4, 4095, backendNodeIP, unmanagedPublicIP),
				},
				ExpectsSecurityGroupWrite:   true,
				ExpectedDenyAllDestinations: map[iputil.Family][]string{iputil.IPv4: {backendNodeIP, unmanagedPublicIP}},
			},
		}

		for _, tt := range tests {
			t.Run(tt.Name, func(t *testing.T) {
				var (
					ctrl                    = gomock.NewController(t)
					az                      = GetTestCloud(ctrl)
					securityGroupClient     = az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
					loadBalancerClient      = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
					loadBalancerBackendPool = az.LoadBalancerBackendPool.(*MockBackendPool)
					loadBalancer            = azureFx.LoadBalancer().Build()
				)
				defer ctrl.Finish()

				runtimeObjects := []runtime.Object{tt.ReconciledService}
				for _, other := range tt.OtherServices {
					runtimeObjects = append(runtimeObjects, other)
				}
				backendPrivateIPs := []string{backendNodeIP}
				runtimeObjects = append(runtimeObjects, makeNodesByIPs(backendPrivateIPs)...)

				kubeClient := fake.NewSimpleClientset(runtimeObjects...)
				informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
				az.serviceLister = informerFactory.Core().V1().Services().Lister()
				az.nodeLister = informerFactory.Core().V1().Nodes().Lister()
				informerFactory.Start(wait.NeverStop)
				informerFactory.WaitForCacheSync(wait.NeverStop)

				expectedWrites := 0
				if tt.ExpectsSecurityGroupWrite {
					expectedWrites = 1
				}

				securityGroupClient.EXPECT().
					Get(gomock.Any(), az.ResourceGroup, az.SecurityGroupName).
					Return(azureFx.SecurityGroup().WithRules(tt.ExistingRules).Build(), nil).
					Times(1)
				securityGroupClient.EXPECT().
					CreateOrUpdate(gomock.Any(), az.ResourceGroup, az.SecurityGroupName, gomock.Any()).
					Return(nil, nil).
					Times(expectedWrites)
				loadBalancerClient.EXPECT().
					Get(gomock.Any(), az.ResourceGroup, *loadBalancer.Name, gomock.Any()).
					Return(loadBalancer, nil).
					Times(1)
				loadBalancerBackendPool.EXPECT().
					GetBackendPrivateIPs(gomock.Any(), ClusterName, tt.ReconciledService, loadBalancer).
					Return(backendPrivateIPs, nil).
					Times(1)

				sg, err := az.reconcileSecurityGroup(ctx, ClusterName, tt.ReconciledService, *loadBalancer.Name, ingressIPs(tt.ReconciledService), tt.WantLB)
				assert.NoError(t, err)

				for _, family := range []iputil.Family{iputil.IPv4, iputil.IPv6} {
					assert.ElementsMatch(t, tt.ExpectedDenyAllDestinations[family], denyDestinations(sg, family),
						"unexpected destinations on the %s deny-all rule", family)
				}
			})
		}
	})
}
