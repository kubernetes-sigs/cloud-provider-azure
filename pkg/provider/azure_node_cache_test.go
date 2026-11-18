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

package provider

import (
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/backendaddresspoolclient/mock_backendaddresspoolclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func TestNodeIPCacheConcurrentAccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(*testing.T, *Cloud, *v1.Service, *v1.Node)
	}{
		{
			name: "backendPoolTypeNodeIPConfig.GetBackendPrivateIPs",
			read: func(t *testing.T, az *Cloud, svc *v1.Service, _ *v1.Node) {
				lb := buildDefaultTestLB(testClusterName, []string{"ipconfig1"})
				ipv4, ipv6 := newBackendPoolTypeNodeIPConfig(az).GetBackendPrivateIPs(t.Context(), testClusterName, svc, &lb)
				assert.Equal(t, []string{"10.0.0.1"}, ipv4)
				assert.Equal(t, []string{"fd00::1"}, ipv6)
			},
		},
		{
			name: "Cloud.checkAndApplyLocalServiceBackendPoolUpdates",
			read: func(t *testing.T, az *Cloud, svc *v1.Service, _ *v1.Node) {
				lb := armnetwork.LoadBalancer{
					Name: ptr.To(testClusterName),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							buildTestLoadBalancerBackendPoolWithIPs("test-svc1", []string{"10.0.0.2"}),
						},
					},
				}
				assert.NoError(t, az.checkAndApplyLocalServiceBackendPoolUpdates(lb, svc))
				updater := az.backendPoolUpdater.(*loadBalancerBackendPoolUpdater)
				updater.drainOperations()
			},
		},
		{
			name: "Cloud.backendNodeIPsOfService",
			read: func(t *testing.T, az *Cloud, svc *v1.Service, _ *v1.Node) {
				local := svc.DeepCopy()
				local.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
				assert.ElementsMatch(t, []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("fd00::1")}, az.backendNodeIPsOfService(local))
			},
		},
		{
			name: "backendPoolTypeNodeIP.ReconcileBackendPools",
			read: func(t *testing.T, az *Cloud, svc *v1.Service, _ *v1.Node) {
				bp := buildTestLoadBalancerBackendPoolWithIPs(testClusterName, []string{"10.0.0.1", "10.0.0.2"})
				bp.Properties.VirtualNetwork = &armnetwork.SubResource{ID: ptr.To(az.getVnetResourceID())}
				lb := &armnetwork.LoadBalancer{
					Name: ptr.To(testClusterName),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{bp},
					},
				}
				expectedBackendPool := buildTestLoadBalancerBackendPoolWithIPs(testClusterName, []string{"10.0.0.1"})
				expectedBackendPool.Properties.VirtualNetwork = bp.Properties.VirtualNetwork
				bpClient := az.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
				bpClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, testClusterName, testClusterName, *expectedBackendPool).Return(nil, nil)
				lbClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				lbClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, testClusterName, gomock.Any()).Return(lb, nil)

				_, _, updatedLB, err := newBackendPoolTypeNodeIP(az).ReconcileBackendPools(t.Context(), testClusterName, svc, lb)
				if assert.NoError(t, err) && assert.NotNil(t, updatedLB) {
					assert.Equal(t, expectedBackendPool, updatedLB.Properties.BackendAddressPools[0])
				}
			},
		},
		{
			name: "backendPoolTypeNodeIP.EnsureHostsInPool",
			read: func(t *testing.T, az *Cloud, svc *v1.Service, node *v1.Node) {
				bp := buildTestLoadBalancerBackendPoolWithIPs(testClusterName, []string{"10.0.0.1"})
				assert.NoError(t, newBackendPoolTypeNodeIP(az).EnsureHostsInPool(t.Context(), svc, []*v1.Node{node}, "", "", testClusterName, testClusterName, bp))
			},
		},
		{
			name: "backendPoolTypeNodeIP.getBackendPoolNodeNames",
			read: func(t *testing.T, az *Cloud, _ *v1.Service, _ *v1.Node) {
				bp := buildTestLoadBalancerBackendPoolWithIPs(testClusterName, []string{"10.0.0.1", "fd00::1"})
				bi := &backendPoolTypeNodeIP{az}
				assert.Equal(t, []string{"node1"}, bi.getBackendPoolNodeNames(bp))
			},
		},
		{
			name: "Cloud.addNodeIPAddressesToBackendPool",
			read: func(t *testing.T, az *Cloud, _ *v1.Service, _ *v1.Node) {
				bp := buildTestLoadBalancerBackendPoolWithIPs(testClusterName, nil)
				assert.True(t, az.addNodeIPAddressesToBackendPool(bp, []string{"10.0.0.1"}))
				assert.Equal(t, "node1", ptr.Deref(bp.Properties.LoadBalancerBackendAddresses[0].Name, ""))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			az := GetTestCloud(ctrl)
			az.backendPoolUpdater = newLoadBalancerBackendPoolUpdater(az, time.Hour)
			az.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			az.MultipleStandardLoadBalancerConfigurations = []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: testClusterName,
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString("node1"),
					},
				},
			}
			vmSet := NewMockVMSet(ctrl)
			vmSet.EXPECT().GetNodeNameByIPConfigurationID(gomock.Any(), "ipconfig1").Return("node1", "", nil).AnyTimes()
			vmSet.EXPECT().GetPrimaryVMSetName().Return("vmss").AnyTimes()
			vmSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(false, nil).AnyTimes()
			az.VMSet = vmSet

			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node1"},
				Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
					{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
					{Type: v1.NodeInternalIP, Address: "fd00::1"},
				}},
			}
			az.updateNodeCaches(nil, node)
			excludedNode := node.DeepCopy()
			excludedNode.Name = "node2"
			excludedNode.Labels = map[string]string{v1.LabelNodeExcludeBalancers: ""}
			excludedNode.Status.Addresses = []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.2"}}
			az.updateNodeCaches(nil, excludedNode)
			svc := getTestService("svc1", v1.ProtocolTCP, nil, false, 80)
			svc.Namespace = "test"
			svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}
			az.endpointSlicesCache.Store("test/eps1", getTestEndpointSlice("eps1", "test", "svc1", "node1"))

			// Yield between accesses so the other loop can run even on one CPU.
			writerDone := make(chan struct{})
			go func() {
				defer close(writerDone)
				for range 1000 {
					az.updateNodeCaches(node, node)
					az.updateNodeCaches(excludedNode, excludedNode)
					runtime.Gosched()
				}
			}()
			t.Cleanup(func() { <-writerDone })
			for range 1000 {
				tc.read(t, az, &svc, node)
				runtime.Gosched()
			}
		})
	}
}

func TestHasNodeName(t *testing.T) {
	for _, tc := range []struct {
		name          string
		nodeNames     *utilsets.IgnoreCaseSet
		nodeName      string
		expectedFound bool
	}{
		{name: "nil node name cache", nodeName: "node1"},
		{name: "empty node name cache", nodeNames: utilsets.NewString(), nodeName: "node1"},
		{name: "uncached node", nodeNames: utilsets.NewString("node1"), nodeName: "node2"},
		{name: "cached node", nodeNames: utilsets.NewString("node1"), nodeName: "node1", expectedFound: true},
		{name: "case-insensitive lookup", nodeNames: utilsets.NewString("node1"), nodeName: "NODE1", expectedFound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			az := &Cloud{nodeNames: tc.nodeNames}
			assert.Equal(t, tc.expectedFound, az.hasNodeName(tc.nodeName))
		})
	}
}

func TestGetNodePrivateIPs(t *testing.T) {
	az := &Cloud{nodePrivateIPs: map[string]*utilsets.IgnoreCaseSet{
		"node1": utilsets.NewString("10.0.0.1", "fd00::1"),
		"empty": utilsets.NewString(),
		"nil":   nil,
	}}
	for _, tc := range []struct {
		name          string
		nodeName      string
		expectedIPs   []string
		expectedFound bool
	}{
		{name: "missing node", nodeName: "missing"},
		{name: "empty cached IP set", nodeName: "empty", expectedFound: true},
		{name: "nil cached IP set", nodeName: "nil", expectedFound: true},
		{name: "case-insensitive dual-stack lookup", nodeName: "NODE1", expectedIPs: []string{"10.0.0.1", "fd00::1"}, expectedFound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ips, found := az.getNodePrivateIPs(tc.nodeName)
			assert.Equal(t, tc.expectedFound, found)
			assert.ElementsMatch(t, tc.expectedIPs, ips)
			if len(ips) > 0 {
				ips[0] = "changed"
				cachedIPs, _ := az.getNodePrivateIPs(tc.nodeName)
				assert.ElementsMatch(t, tc.expectedIPs, cachedIPs)
			}
		})
	}
}

func TestGetNodeNameByPrivateIP(t *testing.T) {
	az := &Cloud{nodePrivateIPToNodeNameMap: map[string]string{
		"10.0.0.1": "Node1",
		"fd00::1":  "Node1",
		"10.0.0.2": "",
	}}
	for _, tc := range []struct {
		name             string
		ip               string
		expectedNodeName string
		expectedFound    bool
	}{
		{name: "missing IP", ip: "10.0.0.3"},
		{name: "IPv4 lookup", ip: "10.0.0.1", expectedNodeName: "Node1", expectedFound: true},
		{name: "IPv6 lookup", ip: "fd00::1", expectedNodeName: "Node1", expectedFound: true},
		{name: "empty cached node name", ip: "10.0.0.2", expectedFound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeName, found := az.getNodeNameByPrivateIP(tc.ip)
			assert.Equal(t, tc.expectedNodeName, nodeName)
			assert.Equal(t, tc.expectedFound, found)
		})
	}
}

func TestGetExcludedLoadBalancerNodeIPs(t *testing.T) {
	for _, tc := range []struct {
		name          string
		excludedNodes *utilsets.IgnoreCaseSet
		expectedIPs   []string
	}{
		{name: "no excluded nodes"},
		{name: "excluded nodes without cached IPs", excludedNodes: utilsets.NewString("missing", "empty", "nil")},
		{
			name:          "returns IPs only for excluded nodes",
			excludedNodes: utilsets.NewString("NODE1", "node2", "missing"),
			expectedIPs:   []string{"10.0.0.1", "fd00::1", "10.0.0.2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			az := &Cloud{
				excludeLoadBalancerNodes: tc.excludedNodes,
				nodePrivateIPs: map[string]*utilsets.IgnoreCaseSet{
					"node1": utilsets.NewString("10.0.0.1", "fd00::1"),
					"node2": utilsets.NewString("10.0.0.2"),
					"node3": utilsets.NewString("10.0.0.3"),
					"empty": utilsets.NewString(),
					"nil":   nil,
				},
			}
			ips := az.getExcludedLoadBalancerNodeIPs()
			assert.ElementsMatch(t, tc.expectedIPs, ips)
			if len(ips) > 0 {
				ips[0] = "changed"
				assert.ElementsMatch(t, tc.expectedIPs, az.getExcludedLoadBalancerNodeIPs())
			}
		})
	}
}

func TestUpdateNodeCachesPrivateIPs(t *testing.T) {
	az := GetTestCloud(gomock.NewController(t))
	excludedNode := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{v1.LabelNodeExcludeBalancers: ""}},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
			{Type: v1.NodeInternalIP, Address: "fd00::1"},
		}},
	}
	// Adding an excluded node caches both IPv4 and IPv6 addresses.
	az.updateNodeCaches(nil, excludedNode)
	initialIPs, found := az.getNodePrivateIPs("NODE1")
	assert.True(t, found)
	assert.ElementsMatch(t, []string{"10.0.0.1", "fd00::1"}, initialIPs)
	assert.ElementsMatch(t, initialIPs, az.getExcludedLoadBalancerNodeIPs())

	// The new IPv4 address should map to the node name; the old address should not.
	// Previously returned IP lists must stay unchanged.
	updatedNode := excludedNode.DeepCopy()
	updatedNode.Status.Addresses[0].Address = "10.0.0.2"
	az.updateNodeCaches(excludedNode, updatedNode)
	assert.ElementsMatch(t, []string{"10.0.0.1", "fd00::1"}, initialIPs)
	ips, found := az.getNodePrivateIPs("node1")
	assert.True(t, found)
	assert.ElementsMatch(t, []string{"10.0.0.2", "fd00::1"}, ips)
	assert.ElementsMatch(t, ips, az.getExcludedLoadBalancerNodeIPs())
	nodeName, found := az.getNodeNameByPrivateIP("10.0.0.1")
	assert.Empty(t, nodeName)
	assert.False(t, found)
	nodeName, found = az.getNodeNameByPrivateIP("10.0.0.2")
	assert.Equal(t, "node1", nodeName)
	assert.True(t, found)

	// Removing the exclusion label keeps the node's IPs cached.
	node := updatedNode.DeepCopy()
	node.Labels = nil
	az.updateNodeCaches(updatedNode, node)
	assert.Empty(t, az.getExcludedLoadBalancerNodeIPs())
	ips, found = az.getNodePrivateIPs("node1")
	assert.True(t, found)
	assert.ElementsMatch(t, []string{"10.0.0.2", "fd00::1"}, ips)

	// Deleting the node removes its IPs and the node name stored for each IP.
	az.updateNodeCaches(node, nil)
	ips, found = az.getNodePrivateIPs("node1")
	assert.Empty(t, ips)
	assert.False(t, found)
	assert.Empty(t, az.getExcludedLoadBalancerNodeIPs())
	nodeName, found = az.getNodeNameByPrivateIP("10.0.0.2")
	assert.Empty(t, nodeName)
	assert.False(t, found)
	nodeName, found = az.getNodeNameByPrivateIP("fd00::1")
	assert.Empty(t, nodeName)
	assert.False(t, found)
}
