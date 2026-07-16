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

// Regression tests for single-stack (IPv4 OR IPv6) services and egress pods running on
// DUAL-STACK nodes. The cold-start importer (processK8sEndpoints / processK8sEgresses) must key
// each pod under the node InternalIP whose IP family matches the address, exactly as the runtime
// EndpointSlice/pod informer path does (endpointSliceAddresses / AddPod uses
// pod.Status.HostIP). A family-blind key (e.g. the node's first InternalIP) would land an IPv6
// pod under the node's IPv4 location at init while runtime keys it under the IPv6 location,
// orphaning the IPv6 location across a CCM restart.
package difftracker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// dualStackNode returns a node with both an IPv4 and an IPv6 InternalIP, IPv4 listed first
// (the order that previously caused IPv6 pods to be mis-keyed).
func dualStackNode(name, ipv4, ipv6 string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: ipv4},
			{Type: v1.NodeInternalIP, Address: ipv6},
		}},
	}
}

// locationForAddr returns the location (node-key) under which a pod address is registered in the
// seeded K8s state, or "" if absent.
func locationForAddr(k8s *K8sState, addr string) string {
	for nodeIP, node := range k8s.Nodes {
		if _, ok := node.Pods[addr]; ok {
			return nodeIP
		}
	}
	return ""
}

// TestColdStart_DualStackNode_InboundFamilyMatchedLocationKey verifies that an IPv4 inbound
// service and an IPv6 inbound service whose pods run on the SAME dual-stack node are each seeded
// under the node InternalIP of the matching family.
func TestColdStart_DualStackNode_InboundFamilyMatchedLocationKey(t *testing.T) {
	const (
		nodeName = "node-ds"
		nodeV4   = "10.0.0.10"
		nodeV6   = "fd00::10"
		svcV4UID = "11111111-1111-1111-1111-111111111111"
		svcV6UID = "22222222-2222-2222-2222-222222222222"
		podV4    = "10.244.0.5"
		podV6    = "fd00::205"
	)

	esV4 := newServiceOwnedEndpointSlice("svc-v4-eps", "default", svcV4UID, discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{podV4}, NodeName: ptr.To(nodeName)},
	})
	esV6 := newServiceOwnedEndpointSlice("svc-v6-eps", "default", svcV6UID, discoveryv1.AddressTypeIPv6, []discoveryv1.Endpoint{
		{Addresses: []string{podV6}, NodeName: ptr.To(nodeName)},
	})
	kube := fake.NewSimpleClientset(dualStackNode(nodeName, nodeV4, nodeV6), esV4, esV6)

	nodeIPs, err := buildNodeNameToIPsMap(context.Background(), kube)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{nodeV4, nodeV6}, nodeIPs[nodeName],
		"both node InternalIPs must be retained for family matching")

	k8s := newK8sStateForSeeders(svcV4UID, svcV6UID)
	_, err = processK8sEndpoints(context.Background(), kube, &k8s, nodeIPs)
	assert.NoError(t, err)

	assert.Equal(t, nodeV4, locationForAddr(&k8s, podV4),
		"IPv4 pod must be keyed under the node's IPv4 InternalIP")
	assert.Equal(t, nodeV6, locationForAddr(&k8s, podV6),
		"IPv6 pod must be keyed under the node's IPv6 InternalIP")

	// The two families must occupy distinct location keys (the physical node appears once per family).
	assert.NotEqual(t, locationForAddr(&k8s, podV4), locationForAddr(&k8s, podV6),
		"v4 and v6 pods on the same node must use distinct family-matched location keys")
}

// TestColdStart_NonCanonicalIPv6_CanonicalizesLocationKeys verifies that a non-canonical IPv6 node
// InternalIP or endpoint address is canonicalized during cold-start seeding, so the location/address
// keys match the runtime path and NRP state (both canonical). A raw key would diff as a spurious
// add/delete against the canonical NRP location after a restart.
func TestColdStart_NonCanonicalIPv6_CanonicalizesLocationKeys(t *testing.T) {
	const (
		nodeName    = "node-nc"
		nodeV4      = "10.0.0.10"
		nodeV6Raw   = "2001:DB8::0010" // uppercase + leading zeros
		nodeV6Canon = "2001:db8::10"
		svcV6UID    = "22222222-2222-2222-2222-222222222222"
		podV6Raw    = "2001:DB8::0205"
		podV6Canon  = "2001:db8::205"
	)

	esV6 := newServiceOwnedEndpointSlice("svc-v6-eps", "default", svcV6UID, discoveryv1.AddressTypeIPv6, []discoveryv1.Endpoint{
		{Addresses: []string{podV6Raw}, NodeName: ptr.To(nodeName)},
	})
	kube := fake.NewSimpleClientset(dualStackNode(nodeName, nodeV4, nodeV6Raw), esV6)

	nodeIPs, err := buildNodeNameToIPsMap(context.Background(), kube)
	assert.NoError(t, err)
	assert.Contains(t, nodeIPs[nodeName], nodeV6Canon, "node InternalIP must be canonicalized")

	k8s := newK8sStateForSeeders(svcV6UID)
	_, err = processK8sEndpoints(context.Background(), kube, &k8s, nodeIPs)
	assert.NoError(t, err)

	assert.Equal(t, nodeV6Canon, locationForAddr(&k8s, podV6Canon),
		"IPv6 pod must be keyed under the canonical node InternalIP and canonical pod address")
}

// TestColdStart_DualStackNode_EgressUsesHostIP verifies an egress pod on a dual-stack node is
// seeded under pod.Status.HostIP (the runtime AddPod/DeletePod key), regardless of which family
// the node lists first.
func TestColdStart_DualStackNode_EgressUsesHostIP(t *testing.T) {
	const (
		nodeName = "node-ds"
		nodeV4   = "10.0.0.20"
		nodeV6   = "fd00::20"
		egress   = "egressds"
		podIP    = "fd00::301" // IPv6-primary egress pod
		hostIP   = "fd00::20"  // pod.Status.HostIP is the IPv6 underlay IP
	)

	pod := newEgressPod("egress-pod", "default", egress, nodeName, podIP, hostIP, v1.PodRunning)
	kube := fake.NewSimpleClientset(dualStackNode(nodeName, nodeV4, nodeV6), pod)

	k8s := newK8sStateForSeeders()
	_, err := processK8sEgresses(context.Background(), kube, &k8s)
	assert.NoError(t, err)

	assert.Equal(t, hostIP, locationForAddr(&k8s, podIP),
		"egress pod must be keyed under pod.Status.HostIP (runtime location key)")
	assert.NotContains(t, []string{locationForAddr(&k8s, podIP)}, nodeV4,
		"egress pod must NOT be keyed under the node's first (mismatched-family) InternalIP")
	assert.True(t, k8s.Egresses.Has(egress), "egress identity must be tracked")
}

// TestColdStart_EgressPodNoSameFamilyLocation_NotDesired verifies cold-start does not mark an egress
// NAT Gateway desired for a pod whose PodIP has no same-family node location (an IPv6 PodIP on a node
// exposing only an IPv4 InternalIP). Marking it desired would provision a NAT Gateway + PIP with no
// backing pod address, diverging from the runtime path (podInformerAddPod), which only tracks the
// egress once an address is actually registered.
func TestColdStart_EgressPodNoSameFamilyLocation_NotDesired(t *testing.T) {
	const (
		nodeName = "node-v4only"
		egress   = "egressnoloc"
		podV6    = "fd00::501" // IPv6 egress PodIP
		hostV4   = "10.0.0.40" // pod.Status.HostIP is IPv4 only -> no same-family (IPv6) location
	)

	pod := newEgressPod("orphan-egress-pod", "default", egress, nodeName, podV6, hostV4, v1.PodRunning)
	kube := fake.NewSimpleClientset(pod)

	k8s := newK8sStateForSeeders()
	_, err := processK8sEgresses(context.Background(), kube, &k8s)
	assert.NoError(t, err)

	assert.False(t, k8s.Egresses.Has(egress),
		"an egress pod with no same-family node location must not mark the NAT Gateway desired (no orphan NAT)")
	assert.False(t, podIPTracked(&k8s, podV6),
		"the unlocatable IPv6 PodIP must not be registered")
}

// TestColdStart_DualStackNode_RestartConvergence is the core regression guard: after a CCM
// restart, the freshly imported K8s state plus the NRP locations that the previous run had
// published must be AlreadyInSync. A family-mismatched init key would make GetSyncOperations
// compute a spurious add (under the wrong family location) + drain (of the correct one), i.e.
// the restart would not converge.
func TestColdStart_DualStackNode_RestartConvergence(t *testing.T) {
	const (
		nodeName = "node-ds"
		nodeV4   = "10.0.0.30"
		nodeV6   = "fd00::30"
		svcV4UID = "33333333-3333-3333-3333-333333333333"
		svcV6UID = "44444444-4444-4444-4444-444444444444"
		podV4    = "10.244.1.7"
		podV6    = "fd00::407"
	)

	esV4 := newServiceOwnedEndpointSlice("svc-v4-eps", "default", svcV4UID, discoveryv1.AddressTypeIPv4, []discoveryv1.Endpoint{
		{Addresses: []string{podV4}, NodeName: ptr.To(nodeName)},
	})
	esV6 := newServiceOwnedEndpointSlice("svc-v6-eps", "default", svcV6UID, discoveryv1.AddressTypeIPv6, []discoveryv1.Endpoint{
		{Addresses: []string{podV6}, NodeName: ptr.To(nodeName)},
	})
	kube := fake.NewSimpleClientset(dualStackNode(nodeName, nodeV4, nodeV6), esV4, esV6)

	nodeIPs, err := buildNodeNameToIPsMap(context.Background(), kube)
	assert.NoError(t, err)
	k8s := newK8sStateForSeeders(svcV4UID, svcV6UID)
	_, err = processK8sEndpoints(context.Background(), kube, &k8s, nodeIPs)
	assert.NoError(t, err)

	dt := newTestDiffTracker()
	dt.K8sResources = k8s
	// Both services already exist in NRP and are tracked StateCreated (post-create steady state).
	dt.NRPResources.LoadBalancers.Insert(svcV4UID)
	dt.NRPResources.LoadBalancers.Insert(svcV6UID)
	dt.pendingServiceOps[svcV4UID] = &ServiceOperationState{ServiceUID: svcV4UID, State: StateCreated, Config: NewInboundServiceConfig(svcV4UID, nil)}
	dt.pendingServiceOps[svcV6UID] = &ServiceOperationState{ServiceUID: svcV6UID, State: StateCreated, Config: NewInboundServiceConfig(svcV6UID, nil)}

	// Reflect into NRP exactly the family-matched locations the previous run published.
	dt.NRPResources.Locations[nodeV4] = NRPLocation{Addresses: map[string]NRPAddress{
		podV4: {Services: newIgnoreCaseSetFromSlice([]string{svcV4UID})},
	}}
	dt.NRPResources.Locations[nodeV6] = NRPLocation{Addresses: map[string]NRPAddress{
		podV6: {Services: newIgnoreCaseSetFromSlice([]string{svcV6UID})},
	}}

	sync := dt.GetSyncOperations()
	assert.Equal(t, AlreadyInSync, sync.SyncStatus,
		"a dual-stack cluster must converge (no spurious add/drain) on restart; got a non-empty diff")
}

// TestColdStart_FQDNEndpointSlice_Skipped verifies the importer ignores FQDN-typed EndpointSlices
// (not PodIP backend addresses), matching the runtime AddressType filter. An FQDN address must not
// be imported as a backend pod IP.
func TestColdStart_FQDNEndpointSlice_Skipped(t *testing.T) {
	const (
		nodeName = "node-ds"
		nodeV4   = "10.0.0.40"
		nodeV6   = "fd00::40"
		svcUID   = "55555555-5555-5555-5555-555555555555"
		fqdnAddr = "example.svc.cluster.local"
	)

	esFQDN := newServiceOwnedEndpointSlice("svc-fqdn-eps", "default", svcUID, discoveryv1.AddressTypeFQDN, []discoveryv1.Endpoint{
		{Addresses: []string{fqdnAddr}, NodeName: ptr.To(nodeName)},
	})
	kube := fake.NewSimpleClientset(dualStackNode(nodeName, nodeV4, nodeV6), esFQDN)

	nodeIPs, err := buildNodeNameToIPsMap(context.Background(), kube)
	assert.NoError(t, err)
	k8s := newK8sStateForSeeders(svcUID)
	_, err = processK8sEndpoints(context.Background(), kube, &k8s, nodeIPs)
	assert.NoError(t, err)

	assert.Equal(t, "", locationForAddr(&k8s, fqdnAddr),
		"an FQDN EndpointSlice address must not be imported as a backend pod IP")
	assert.Empty(t, k8s.Nodes, "no location should be created for an FQDN-only slice")
}

// TestNodeIPForEndpointSlice_FamilyMatchTable pins the family-matching helper directly.
func TestNodeIPForEndpointSlice_FamilyMatchTable(t *testing.T) {
	nodeIPs := []string{"10.0.0.50", "fd00::50"} // IPv4 first
	cases := []struct {
		name     string
		addrType discoveryv1.AddressType
		wantIP   string
		wantOK   bool
	}{
		{"ipv4 slice -> ipv4 node ip", discoveryv1.AddressTypeIPv4, "10.0.0.50", true},
		{"ipv6 slice -> ipv6 node ip", discoveryv1.AddressTypeIPv6, "fd00::50", true},
		{"fqdn slice -> skipped", discoveryv1.AddressTypeFQDN, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip, ok := nodeIPForEndpointSlice(nodeIPs, tc.addrType)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantIP, ip)
		})
	}

	// A node missing the requested family yields no match (e.g. IPv4-only node, IPv6 slice).
	ip, ok := nodeIPForEndpointSlice([]string{"10.0.0.51"}, discoveryv1.AddressTypeIPv6)
	assert.False(t, ok, "an IPv4-only node has no IPv6 location for an IPv6 slice")
	assert.Equal(t, "", ip)
}

// Compile-time guard that the egress label const is the one the importer filters on (keeps this
// regression file honest if the label is ever renamed).
var _ = consts.PodLabelServiceEgressGateway

// ensure types import is used (PodStatus phase enum reference for readers).
var _ = types.UID("")

// TestSelectSameFamilyNodeIP_DeterministicAndValidated covers the shared node-location selector used
// by both the init and runtime EndpointSlice paths. It must (1) pick the same same-family IP
// regardless of input order (a node with multiple same-family InternalIPs must not flap its location
// between reconciles/restarts), (2) skip malformed node IPs (a bad value must never become a location
// key and poison the NRP batch), and (3) canonicalize the result.
func TestSelectSameFamilyNodeIP_DeterministicAndValidated(t *testing.T) {
	// Order-independence: two orderings of the same set yield the same key.
	a, okA := SelectSameFamilyNodeIP([]string{"10.0.0.20", "10.0.0.3"}, false)
	b, okB := SelectSameFamilyNodeIP([]string{"10.0.0.3", "10.0.0.20"}, false)
	assert.True(t, okA && okB)
	assert.Equal(t, a, b, "selection must be order-independent for multiple same-family InternalIPs")

	// Malformed node IPs are skipped, not used as a location key.
	got, ok := SelectSameFamilyNodeIP([]string{"not-an-ip", "10.0.0.5"}, false)
	assert.True(t, ok)
	assert.Equal(t, "10.0.0.5", got, "a malformed node IP must be skipped, not used as a location key")

	_, ok = SelectSameFamilyNodeIP([]string{"not-an-ip"}, false)
	assert.False(t, ok, "a node with no valid same-family InternalIP yields ok=false")

	// Result is canonicalized and family-matched.
	v6, ok := SelectSameFamilyNodeIP([]string{"10.0.0.1", "2001:DB8::0010"}, true)
	assert.True(t, ok)
	assert.Equal(t, "2001:db8::10", v6, "the IPv6 family key must be canonical")

	_, ok = SelectSameFamilyNodeIP([]string{"10.0.0.1"}, true)
	assert.False(t, ok, "no IPv6 InternalIP -> ok=false for an IPv6 pod")
}
