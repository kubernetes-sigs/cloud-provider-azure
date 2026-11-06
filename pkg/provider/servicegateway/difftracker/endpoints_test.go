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

package difftracker

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	k8scache "k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func setTestNodeLister(t *testing.T, dt *DiffTracker, nodes ...*v1.Node) {
	t.Helper()
	indexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{})
	for _, node := range nodes {
		if err := indexer.Add(node); err != nil {
			t.Fatalf("failed to add node to test indexer: %v", err)
		}
	}
	dt.SetNodeLister(corelisters.NewNodeLister(indexer))
}

func TestReconcileNodeIPChange(t *testing.T) {
	const (
		svcUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		podIP  = "1.1.1.1"
		oldLoc = "10.0.0.1"
		newLoc = "10.0.0.2"
	)

	// newTracker builds an engine that already tracks svcUID as an NRP load balancer, so
	// UpdateEndpoints applies the addresses to K8sResources synchronously (rather than buffering),
	// letting the tests assert the resulting desired state directly.
	newTracker := func(t *testing.T) *DiffTracker {
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)
		dt := seedDiffTracker(t, mock_azclient.NewMockClientFactory(ctrl), fake.NewSimpleClientset(),
			K8sState{Services: utilsets.NewString(svcUID), Egresses: utilsets.NewString(), Nodes: map[string]Node{}},
			NRPState{LoadBalancers: utilsets.NewString(svcUID), NATGateways: utilsets.NewString(), Locations: map[string]NRPLocation{}})
		return dt
	}

	singleEndpointSlice := func() *discovery_v1.EndpointSlice {
		return &discovery_v1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "eps1",
				Namespace:       "test",
				OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: types.UID(svcUID)}},
			},
			AddressType: discovery_v1.AddressTypeIPv4,
			Endpoints: []discovery_v1.Endpoint{{
				Addresses:  []string{podIP},
				NodeName:   ptr.To("node1"),
				Conditions: discovery_v1.EndpointConditions{Ready: ptr.To(true)},
			}},
		}
	}

	t.Run("node InternalIP change moves the pod off the stale location", func(t *testing.T) {
		dt := newTracker(t)
		dt.ReconcileEndpointSlice(nil, singleEndpointSlice())
		dt.UpdateEndpoints(svcUID, nil, map[string]string{podIP: oldLoc})

		dt.ReconcileNodeIPChange("node1", []string{oldLoc}, []string{newLoc})

		nodes := dt.K8sResources.Nodes
		assert.Contains(t, nodes, newLoc, "pod must move to the new node IP")
		assert.Contains(t, nodes[newLoc].Pods, podIP)
		if stale, ok := nodes[oldLoc]; ok {
			assert.NotContains(t, stale.Pods, podIP, "pod must be removed from the old node IP")
		}
	})

	t.Run("node addition registers a pod dropped while its node was uncached", func(t *testing.T) {
		dt := newTracker(t)
		dt.ReconcileEndpointSlice(nil, singleEndpointSlice())

		dt.ReconcileNodeIPChange("node1", nil, []string{oldLoc})

		nodes := dt.K8sResources.Nodes
		assert.Contains(t, nodes, oldLoc, "pod must be registered once its node appears")
		assert.Contains(t, nodes[oldLoc].Pods, podIP)
	})

	t.Run("node deletion drains the pod", func(t *testing.T) {
		dt := newTracker(t)
		dt.ReconcileEndpointSlice(nil, singleEndpointSlice())
		dt.UpdateEndpoints(svcUID, nil, map[string]string{podIP: oldLoc})

		dt.ReconcileNodeIPChange("node1", []string{oldLoc}, nil)

		if stale, ok := dt.K8sResources.Nodes[oldLoc]; ok {
			assert.NotContains(t, stale.Pods, podIP, "pod must drain when its node is deleted")
		}
	})

	// The reconcile must be nil-safe: node handlers may fire before the engine is constructed.
	t.Run("does not panic when the diff tracker is not yet initialized", func(t *testing.T) {
		var dt *DiffTracker
		assert.NotPanics(t, func() {
			dt.ReconcileNodeIPChange("node1", []string{oldLoc}, []string{newLoc})
		})
	})
}

func TestAddServiceSeedsInboundEndpointsFromCache(t *testing.T) {
	const (
		svcUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		podIP  = "1.1.1.1"
		nodeIP = "10.0.0.1"
	)
	dt := newTestDiffTracker()
	endpointSlice := &discovery_v1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "eps1",
			Namespace:       "test",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: types.UID(svcUID)}},
		},
		AddressType: discovery_v1.AddressTypeIPv4,
		Endpoints: []discovery_v1.Endpoint{{
			Addresses:  []string{podIP},
			NodeName:   ptr.To("node1"),
			Conditions: discovery_v1.EndpointConditions{Ready: ptr.To(true)},
		}},
	}
	dt.ReconcileEndpointSlice(nil, endpointSlice)
	endpointSlice.Endpoints[0].Addresses[0] = "9.9.9.9"
	setTestNodeLister(t, dt, &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: nodeIP},
		}},
	})

	dt.AddService(NewInboundServiceConfig(svcUID, makeInboundConfig(80)))

	updates := dt.pendingEndpoints[svcUID]
	if assert.Len(t, updates, 1, "AddService must replay unchanged EndpointSlices into the creation buffer") {
		assert.Equal(t, map[string]string{podIP: nodeIP}, updates[0].PodIPToNodeIP)
	}
}

func TestInitializeEndpointSlicesCacheStoresIndependentSnapshots(t *testing.T) {
	endpointSlices := &discovery_v1.EndpointSliceList{
		Items: []discovery_v1.EndpointSlice{{
			ObjectMeta: metav1.ObjectMeta{Name: "eps1", Namespace: "test"},
			Endpoints: []discovery_v1.Endpoint{{
				Addresses: []string{"1.1.1.1"},
			}},
		}},
	}
	dt := newTestDiffTracker()

	dt.initializeEndpointSlicesCache(endpointSlices)
	endpointSlices.Items[0].Endpoints[0].Addresses[0] = "9.9.9.9"

	cached, loaded := dt.endpointSlicesCache.Load("test/eps1")
	if assert.True(t, loaded) {
		assert.Equal(t, "1.1.1.1", cached.(*discovery_v1.EndpointSlice).Endpoints[0].Addresses[0])
	}
}

func TestReconcileEndpointSlice(t *testing.T) {
	const (
		svcUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		nodeIP = "10.0.0.1"
		oldPod = "1.1.1.1"
		newPod = "1.1.1.2"
	)

	dt := newTestDiffTracker()
	dt.NRPResources.LoadBalancers.Insert(svcUID)
	setTestNodeLister(t, dt, &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: nodeIP},
		}},
	})
	endpointSlice := func(podIP string) *discovery_v1.EndpointSlice {
		return &discovery_v1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "eps1",
				Namespace:       "test",
				OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: types.UID(svcUID)}},
			},
			AddressType: discovery_v1.AddressTypeIPv4,
			Endpoints: []discovery_v1.Endpoint{{
				Addresses:  []string{podIP},
				NodeName:   ptr.To("node1"),
				Conditions: discovery_v1.EndpointConditions{Ready: ptr.To(true)},
			}},
		}
	}

	oldSlice := endpointSlice(oldPod)
	dt.ReconcileEndpointSlice(nil, oldSlice)
	assert.Contains(t, dt.K8sResources.Nodes[nodeIP].Pods, oldPod)
	cached, loaded := dt.endpointSlicesCache.Load("test/eps1")
	if assert.True(t, loaded) {
		assert.NotSame(t, oldSlice, cached, "difftracker must store its own EndpointSlice snapshot")
	}

	newES := endpointSlice(newPod)
	dt.ReconcileEndpointSlice(oldSlice, newES)
	assert.NotContains(t, dt.K8sResources.Nodes[nodeIP].Pods, oldPod)
	assert.Contains(t, dt.K8sResources.Nodes[nodeIP].Pods, newPod)

	dt.ReconcileEndpointSlice(newES, nil)
	_, loaded = dt.endpointSlicesCache.Load("test/eps1")
	assert.False(t, loaded)
	if node, ok := dt.K8sResources.Nodes[nodeIP]; ok {
		assert.NotContains(t, node.Pods, newPod)
	}
}

func TestEndpointSliceAddresses(t *testing.T) {
	nodeIndexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{})
	err := nodeIndexer.Add(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: "192.168.1.1"},
			{Type: v1.NodeInternalIP, Address: "2001:DB8::00AB"},
		}},
	})
	assert.NoError(t, err)
	nodeLister := corelisters.NewNodeLister(nodeIndexer)

	for _, tc := range []struct {
		name     string
		slice    *discovery_v1.EndpointSlice
		expected map[string]string
	}{
		{
			name: "nil Ready is included and IPv6 addresses are canonicalized",
			slice: &discovery_v1.EndpointSlice{
				AddressType: discovery_v1.AddressTypeIPv6,
				Endpoints: []discovery_v1.Endpoint{{
					Addresses: []string{"2001:DB8::0001"},
					NodeName:  ptr.To("node1"),
				}},
			},
			expected: map[string]string{"2001:db8::1": "2001:db8::ab"},
		},
		{
			name: "explicitly unready endpoint is excluded",
			slice: &discovery_v1.EndpointSlice{
				AddressType: discovery_v1.AddressTypeIPv4,
				Endpoints: []discovery_v1.Endpoint{{
					Addresses:  []string{"10.0.0.1"},
					NodeName:   ptr.To("node1"),
					Conditions: discovery_v1.EndpointConditions{Ready: ptr.To(false)},
				}},
			},
			expected: map[string]string{},
		},
		{
			name: "malformed address is skipped without dropping valid peers",
			slice: &discovery_v1.EndpointSlice{
				AddressType: discovery_v1.AddressTypeIPv4,
				Endpoints: []discovery_v1.Endpoint{{
					Addresses:  []string{"10.0.0.1", "not-an-ip"},
					NodeName:   ptr.To("node1"),
					Conditions: discovery_v1.EndpointConditions{Ready: ptr.To(true)},
				}},
			},
			expected: map[string]string{"10.0.0.1": "192.168.1.1"},
		},
		{
			name: "endpoint without a node is excluded",
			slice: &discovery_v1.EndpointSlice{
				AddressType: discovery_v1.AddressTypeIPv4,
				Endpoints: []discovery_v1.Endpoint{{
					Addresses:  []string{"10.0.0.1"},
					Conditions: discovery_v1.EndpointConditions{Ready: ptr.To(true)},
				}},
			},
			expected: map[string]string{},
		},
		{
			name:     "nil slice is empty",
			expected: map[string]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, endpointSliceAddresses(tc.slice, nodeLister))
		})
	}
}

// TestEndpointSliceAddresses_ResolvesLocationPerAddressFamily pins that the node location is
// selected from each address's own family rather than the slice's declared AddressType.
//
// NRP requires every address to sit under a same-family location: an IPv4 location cannot hold an
// IPv6 address. AddressType describes the slice, not a guarantee about each address it carries, so
// deriving one location per endpoint and applying it to every address can register an address under
// the wrong family. The egress path already resolves per address (PodNodeLocationsByFamily).
func TestEndpointSliceAddresses_ResolvesLocationPerAddressFamily(t *testing.T) {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
			{Type: v1.NodeInternalIP, Address: "fd00::1"},
		}},
	}
	indexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{})
	assert.NoError(t, indexer.Add(node))
	nodeLister := corelisters.NewNodeLister(indexer)

	newSlice := func(addressType discovery_v1.AddressType, addresses ...string) *discovery_v1.EndpointSlice {
		return &discovery_v1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name: "es", Namespace: "ns",
				OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: types.UID("svc-1")}},
			},
			AddressType: addressType,
			Endpoints: []discovery_v1.Endpoint{{
				Addresses:  addresses,
				NodeName:   ptr.To("node-1"),
				Conditions: discovery_v1.EndpointConditions{Ready: ptr.To(true)},
			}},
		}
	}

	t.Run("address family wins over a mismatched AddressType", func(t *testing.T) {
		got := endpointSliceAddresses(newSlice(discovery_v1.AddressTypeIPv4, "fd00::99"), nodeLister)
		assert.Equal(t, map[string]string{"fd00::99": "fd00::1"}, got,
			"an IPv6 address must resolve to the node's IPv6 location even in an IPv4-typed slice")
	})

	t.Run("mixed families each resolve to their own location", func(t *testing.T) {
		got := endpointSliceAddresses(newSlice(discovery_v1.AddressTypeIPv4, "10.244.0.9", "fd00::99"), nodeLister)
		assert.Equal(t, map[string]string{"10.244.0.9": "10.0.0.1", "fd00::99": "fd00::1"}, got)
	})

	t.Run("no same-family node IP skips the address", func(t *testing.T) {
		v4Only := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
			Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.2"}}},
		}
		v4Indexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{})
		assert.NoError(t, v4Indexer.Add(v4Only))
		slice := newSlice(discovery_v1.AddressTypeIPv6, "fd00::99")
		slice.Endpoints[0].NodeName = ptr.To("node-2")
		assert.Empty(t, endpointSliceAddresses(slice, corelisters.NewNodeLister(v4Indexer)),
			"an IPv6 address must not fall back to an IPv4 node location")
	})

	t.Run("conforming single-family slice is unchanged", func(t *testing.T) {
		got := endpointSliceAddresses(newSlice(discovery_v1.AddressTypeIPv4, "10.244.0.9"), nodeLister)
		assert.Equal(t, map[string]string{"10.244.0.9": "10.0.0.1"}, got)
	})
}

// blockingNodeLister parks the first Get so a concurrent EndpointSlice removal can be applied while
// the seed is mid-snapshot.
type blockingNodeLister struct {
	corelisters.NodeLister
	entered chan struct{}
	release chan struct{}
	gate    atomic.Bool
}

// Get parks only the FIRST caller. Later callers pass straight through, so the concurrent removal
// is free to apply while the seed is stalled.
func (b *blockingNodeLister) Get(name string) (*v1.Node, error) {
	if b.gate.CompareAndSwap(false, true) {
		close(b.entered)
		<-b.release
	}
	return b.NodeLister.Get(name)
}

// TestSeedInboundEndpointsFromCache_DoesNotOvertakeRemoval pins that the endpoint cache replay
// cannot resurrect an address a concurrent EndpointSlice removal just deleted. The replay only adds,
// so an older snapshot applied afterwards re-registers the address, and no later delta can remove it
// (removals are derived from the cache, which no longer lists it) -- it stays a live backend until
// the process restarts, and is served once the CNI reassigns the IP.
func TestSeedInboundEndpointsFromCache_DoesNotOvertakeRemoval(t *testing.T) {
	const (
		svcUID = "cafe0000-0000-0000-0000-00000000000a"
		podIP  = "1.1.1.1"
		nodeIP = "10.0.0.1"
	)

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: nodeIP}}},
	}
	slice := &discovery_v1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "slice-1",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: types.UID(svcUID)}},
		},
		AddressType: discovery_v1.AddressTypeIPv4,
		Endpoints: []discovery_v1.Endpoint{{
			Addresses:  []string{podIP},
			NodeName:   ptr.To("node-a"),
			Conditions: discovery_v1.EndpointConditions{Ready: ptr.To(true)},
		}},
	}

	registered := func(dt *DiffTracker) bool {
		dt.mu.Lock()
		defer dt.mu.Unlock()
		n, ok := dt.K8sResources.Nodes[nodeIP]
		if !ok {
			return false
		}
		pod, ok := n.Pods[podIP]
		return ok && pod.InboundIdentities.Has(svcUID)
	}

	newTracker := func(t *testing.T) (*DiffTracker, *blockingNodeLister) {
		dt := newTestDiffTracker()
		dt.NRPResources.LoadBalancers.Insert(svcUID)
		setTestNodeLister(t, dt, node)
		// Register the endpoint with the plain lister first, so the blocking one is only consumed
		// by the seed under test.
		dt.ReconcileEndpointSlice(nil, slice)
		blocking := &blockingNodeLister{
			NodeLister: dt.nodeLister,
			entered:    make(chan struct{}),
			release:    make(chan struct{}),
		}
		dt.SetNodeLister(blocking)
		return dt, blocking
	}

	t.Run("a removal applied during the snapshot is not overwritten", func(t *testing.T) {
		dt, blocking := newTracker(t)

		seedDone := make(chan struct{})
		go func() {
			defer close(seedDone)
			dt.seedInboundEndpointsFromCache(svcUID)
		}()

		select {
		case <-blocking.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the seed never reached the node lookup")
		}

		// The pod is deleted while the seed holds its snapshot. Give the removal time to apply
		// before releasing the seed: without the lock it completes here and the older snapshot
		// then overwrites it, which is the defect. Holding the lock makes it wait instead.
		removalDone := make(chan struct{})
		go func() {
			defer close(removalDone)
			dt.ReconcileEndpointSlice(slice, nil)
		}()
		select {
		case <-removalDone:
		case <-time.After(time.Second):
		}

		close(blocking.release)
		for _, done := range []chan struct{}{seedDone, removalDone} {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("seed or removal did not finish")
			}
		}

		assert.False(t, registered(dt),
			"a deleted pod IP must not survive the endpoint cache replay")
	})

	t.Run("the seed still registers endpoints when nothing is removed", func(t *testing.T) {
		dt, blocking := newTracker(t)
		close(blocking.release)

		dt.seedInboundEndpointsFromCache(svcUID)

		assert.True(t, registered(dt),
			"the replay must still seed endpoints for a newly registered service")
	})
}

const (
	gapSvcUID  = "5afe0000-0000-0000-0000-00000000000b"
	gapPodA    = "10.244.1.10"
	gapPodB    = "10.244.2.20"
	gapNode1IP = "10.0.0.1"
	gapNode2IP = "10.0.0.2"
	gapNode3IP = "10.0.0.3"
)

type gapEndpoint struct {
	podIP    string
	nodeName string
	ready    bool
}

func gapNode(name, ip string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: ip}}},
	}
}

func gapSlice(name, resourceVersion string, endpoints ...gapEndpoint) *discovery_v1.EndpointSlice {
	es := &discovery_v1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "ns",
			Name:            name,
			ResourceVersion: resourceVersion,
			OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: types.UID(gapSvcUID)}},
		},
		AddressType: discovery_v1.AddressTypeIPv4,
	}
	for _, ep := range endpoints {
		es.Endpoints = append(es.Endpoints, discovery_v1.Endpoint{
			Addresses:  []string{ep.podIP},
			NodeName:   ptr.To(ep.nodeName),
			Conditions: discovery_v1.EndpointConditions{Ready: ptr.To(ep.ready)},
		})
	}
	return es
}

// startupSliceV1 is the EndpointSlice initialization listed: A on node1 and B on node2, both ready.
func startupSliceV1() *discovery_v1.EndpointSlice {
	return gapSlice("eps1", "1", gapEndpoint{gapPodA, "node1", true}, gapEndpoint{gapPodB, "node2", true})
}

// newStartupGapTracker mirrors InitializeFromCluster: the startup slices are registered under the
// node IPs initialization listed and seeded into the EndpointSlice cache before any informer event.
// listedNodes are the nodes the Node informer has listed when the events arrive.
func newStartupGapTracker(t *testing.T, startupSlices []*discovery_v1.EndpointSlice, listedNodes ...*v1.Node) *DiffTracker {
	t.Helper()
	dt := newTestDiffTracker()
	dt.NRPResources.LoadBalancers.Insert(gapSvcUID)
	dt.startupNodeIPs = map[string][]string{"node1": {gapNode1IP}, "node2": {gapNode2IP}, "node3": {gapNode3IP}}
	list := &discovery_v1.EndpointSliceList{}
	for _, es := range startupSlices {
		list.Items = append(list.Items, *es)
		for _, ep := range es.Endpoints {
			if ptr.Deref(ep.Conditions.Ready, true) {
				dt.UpdateEndpoints(gapSvcUID, nil, map[string]string{ep.Addresses[0]: dt.startupNodeIPs[*ep.NodeName][0]})
			}
		}
	}
	dt.initializeEndpointSlicesCache(list)
	setTestNodeLister(t, dt, listedNodes...)
	return dt
}

func gapRegistered(dt *DiffTracker, nodeIP, podIP string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	node, ok := dt.K8sResources.Nodes[nodeIP]
	if !ok {
		return false
	}
	pod, ok := node.Pods[podIP]
	return ok && pod.InboundIdentities.Has(gapSvcUID)
}

// TestReconcileEndpointSlice_InitialAddDiffsAgainstStartupSnapshot pins that the informer's first
// Add for a slice initialization already listed applies what changed while initialization ran. The
// Add used to only add, so an endpoint removed or made not-ready in that window stayed registered
// (later updates saw it absent from both old and new) until the next restart.
func TestReconcileEndpointSlice_InitialAddDiffsAgainstStartupSnapshot(t *testing.T) {
	allNodes := []*v1.Node{gapNode("node1", gapNode1IP), gapNode("node2", gapNode2IP), gapNode("node3", gapNode3IP)}
	testCases := []struct {
		name        string
		live        *discovery_v1.EndpointSlice
		listedNodes []*v1.Node
		wantB       string
	}{
		{
			name:        "endpoint made not-ready",
			live:        gapSlice("eps1", "2", gapEndpoint{gapPodA, "node1", true}, gapEndpoint{gapPodB, "node2", false}),
			listedNodes: allNodes,
		},
		{
			name:        "endpoint removed",
			live:        gapSlice("eps1", "2", gapEndpoint{gapPodA, "node1", true}),
			listedNodes: allNodes,
		},
		{
			name:        "endpoint moved to another node",
			live:        gapSlice("eps1", "2", gapEndpoint{gapPodA, "node1", true}, gapEndpoint{gapPodB, "node3", true}),
			listedNodes: allNodes,
			wantB:       gapNode3IP,
		},
		{
			// The Node informer syncs independently; the removal must not depend on it having listed
			// the endpoint's node yet.
			name:        "endpoint made not-ready on a node the Node informer has not listed yet",
			live:        gapSlice("eps1", "2", gapEndpoint{gapPodA, "node1", true}, gapEndpoint{gapPodB, "node2", false}),
			listedNodes: []*v1.Node{gapNode("node1", gapNode1IP)},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dt := newStartupGapTracker(t, []*discovery_v1.EndpointSlice{startupSliceV1()}, tc.listedNodes...)

			dt.ReconcileEndpointSlice(nil, tc.live)

			assert.True(t, gapRegistered(dt, gapNode1IP, gapPodA), "an unchanged endpoint must stay registered")
			assert.False(t, gapRegistered(dt, gapNode2IP, gapPodB),
				"a change made while initialization ran must remove the endpoint from its startup location")
			if tc.wantB != "" {
				assert.True(t, gapRegistered(dt, tc.wantB, gapPodB), "a moved endpoint must be registered at its new location")
			}
			cached, ok := dt.endpointSlicesCache.Load("ns/eps1")
			if assert.True(t, ok) {
				assert.Equal(t, "2", cached.(*discovery_v1.EndpointSlice).ResourceVersion)
			}
		})
	}
}

// TestReconcileEndpointSlice_InitialAddKeepsEndpointsOnUnlistedNodes pins that the startup-snapshot
// diff resolves the live slice the way initialization registered it. The Node and EndpointSlice
// informers list in parallel, so a node may be missing from the lister when its slice's Add arrives;
// an unchanged ready endpoint there must not be read as removed.
func TestReconcileEndpointSlice_InitialAddKeepsEndpointsOnUnlistedNodes(t *testing.T) {
	const podC = "10.244.1.30"
	dt := newStartupGapTracker(t, []*discovery_v1.EndpointSlice{startupSliceV1()}, gapNode("node1", gapNode1IP))

	dt.ReconcileEndpointSlice(nil, gapSlice("eps1", "2",
		gapEndpoint{gapPodA, "node1", true}, gapEndpoint{gapPodB, "node2", true}, gapEndpoint{podC, "node1", true}))

	assert.True(t, gapRegistered(dt, gapNode2IP, gapPodB), "an unchanged endpoint on an unlisted node must stay registered")
	assert.True(t, gapRegistered(dt, gapNode1IP, podC))
}

// TestReconcileEndpointSlice_GapRemovalOnNodeWithChangedIP pins that until a node's first Add is
// replayed, its endpoints are resolved under the IPs initialization registered them under, even when
// the Node lister already has the node's new InternalIP; and through the lister once it is replayed.
func TestReconcileEndpointSlice_GapRemovalOnNodeWithChangedIP(t *testing.T) {
	const (
		changedIP = "10.0.0.22"
		podE      = "10.244.2.50"
	)
	startup := gapSlice("eps1", "1",
		gapEndpoint{gapPodA, "node1", true}, gapEndpoint{gapPodB, "node2", true}, gapEndpoint{podE, "node2", true})
	dt := newStartupGapTracker(t, []*discovery_v1.EndpointSlice{startup},
		gapNode("node1", gapNode1IP), gapNode("node2", changedIP))

	// B was removed while initialization ran; the slice Add arrives before node2's replay.
	dt.ReconcileEndpointSlice(nil, gapSlice("eps1", "2", gapEndpoint{gapPodA, "node1", true}, gapEndpoint{podE, "node2", true}))
	assert.False(t, gapRegistered(dt, gapNode2IP, gapPodB), "the removal must apply where initialization registered B")
	assert.True(t, gapRegistered(dt, gapNode2IP, podE))

	dt.ReconcileNodeAdd("node2", []string{changedIP})
	assert.False(t, gapRegistered(dt, gapNode2IP, gapPodB))
	assert.False(t, gapRegistered(dt, changedIP, gapPodB))
	assert.True(t, gapRegistered(dt, changedIP, podE), "the replay must move E to the node's current IP")
	assert.False(t, gapRegistered(dt, gapNode2IP, podE))

	// Once replayed, the node is resolved through the lister.
	dt.ReconcileEndpointSlice(gapSlice("eps1", "2", gapEndpoint{gapPodA, "node1", true}, gapEndpoint{podE, "node2", true}),
		gapSlice("eps1", "3", gapEndpoint{gapPodA, "node1", true}))
	assert.False(t, gapRegistered(dt, changedIP, podE), "a removal after the replay must apply at the node's current IP")
}

// TestReconcileEndpointSlice_NewSliceRemovalOnNodeWithChangedIP pins that a slice initialization did
// not list resolves an unreplayed node through its startup IPs as well, so a removal arriving before
// the node's replay finds the address the slice's Add registered.
func TestReconcileEndpointSlice_NewSliceRemovalOnNodeWithChangedIP(t *testing.T) {
	const changedIP = "10.0.0.22"
	dt := newStartupGapTracker(t, nil, gapNode("node2", changedIP))
	added := gapSlice("eps9", "1", gapEndpoint{gapPodB, "node2", true})

	dt.ReconcileEndpointSlice(nil, added)
	dt.ReconcileEndpointSlice(added, gapSlice("eps9", "2"))
	dt.ReconcileNodeAdd("node2", []string{changedIP})

	assert.False(t, gapRegistered(dt, gapNode2IP, gapPodB))
	assert.False(t, gapRegistered(dt, changedIP, gapPodB), "a removal before the replay must not leave the added address behind")
}

func TestReconcileEndpointSlice_RuntimeAddWithoutCacheUnchanged(t *testing.T) {
	dt := newStartupGapTracker(t, []*discovery_v1.EndpointSlice{startupSliceV1()},
		gapNode("node1", gapNode1IP), gapNode("node2", gapNode2IP), gapNode("node3", gapNode3IP))
	const podC = "10.244.3.30"

	dt.ReconcileEndpointSlice(nil, gapSlice("eps2", "7", gapEndpoint{podC, "node3", true}))

	assert.True(t, gapRegistered(dt, gapNode3IP, podC), "a new slice must register its endpoints")
	assert.True(t, gapRegistered(dt, gapNode1IP, gapPodA), "a new slice must not touch another slice's endpoints")
	assert.True(t, gapRegistered(dt, gapNode2IP, gapPodB), "a new slice must not touch another slice's endpoints")
}

// TestStartupNodeReplayDoesNotResurrectGapRemovals pins that the node replay the Node informer's
// initial list triggers cannot re-add an endpoint the startup-snapshot diff removed. The replay
// re-adds every cached address on the node, so it must see either the stale snapshot before the
// removal or the live slice after it, never apply the stale snapshot after the removal.
func TestStartupNodeReplayDoesNotResurrectGapRemovals(t *testing.T) {
	live := func() *discovery_v1.EndpointSlice {
		return gapSlice("eps1", "2", gapEndpoint{gapPodA, "node1", true}, gapEndpoint{gapPodB, "node2", false})
	}
	newTracker := func(t *testing.T) *DiffTracker {
		return newStartupGapTracker(t, []*discovery_v1.EndpointSlice{startupSliceV1()},
			gapNode("node1", gapNode1IP), gapNode("node2", gapNode2IP))
	}
	assertRemoved := func(t *testing.T, dt *DiffTracker) {
		assert.False(t, gapRegistered(dt, gapNode2IP, gapPodB), "the node replay must not resurrect a removed endpoint")
		assert.True(t, gapRegistered(dt, gapNode1IP, gapPodA))
	}

	t.Run("slice add then node replay", func(t *testing.T) {
		dt := newTracker(t)
		dt.ReconcileEndpointSlice(nil, live())
		dt.ReconcileNodeAdd("node2", []string{gapNode2IP})
		assertRemoved(t, dt)
	})

	t.Run("node replay then slice add", func(t *testing.T) {
		dt := newTracker(t)
		dt.ReconcileNodeAdd("node2", []string{gapNode2IP})
		dt.ReconcileEndpointSlice(nil, live())
		assertRemoved(t, dt)
	})

	t.Run("slice add applied while the node replay is in flight", func(t *testing.T) {
		dt := newTracker(t)

		// Hold the lock the slice add takes, start the replay, then apply the slice add exactly as
		// ReconcileEndpointSlice does under that lock. A replay that walks the cache before taking
		// the lock snapshots the stale slice here and re-adds B once the lock is released.
		// ReconcileNodeAdd runs the same walk, already under the lock it takes for startupNodeIPs.
		dt.mu.Lock()
		replayDone := make(chan struct{})
		go func() {
			defer close(replayDone)
			dt.ReconcileNodeIPChange("node2", []string{gapNode2IP}, []string{gapNode2IP})
		}()
		time.Sleep(200 * time.Millisecond)
		cached, _ := dt.endpointSlicesCache.Load("ns/eps1")
		dt.reconcileEndpointSliceLocked(cached.(*discovery_v1.EndpointSlice), live())
		dt.mu.Unlock()

		select {
		case <-replayDone:
		case <-time.After(10 * time.Second):
			t.Fatal("node replay did not finish")
		}
		assertRemoved(t, dt)
	})
}

// TestStartupNodeReconciliationDoesNotStrandGapRemovals pins that a node's startup replay or drain
// cannot run between a slice event's cache swap and its apply. The swap hides the removed endpoint
// from the node's cache walk, which then releases the node's startup IPs, so the apply resolves the
// endpoint through the lister and leaves it registered where initialization put it.
func TestStartupNodeReconciliationDoesNotStrandGapRemovals(t *testing.T) {
	const changedIP = "10.0.0.22"
	testCases := []struct {
		name        string
		listedNodes []*v1.Node
		// oldSlice is the event's old slice; nil makes it the slice's initial-list Add.
		oldSlice *discovery_v1.EndpointSlice
		// reconcileNode2 runs node2's startup reconciliation as its caller does under dt.mu.
		reconcileNode2 func(dt *DiffTracker)
	}{
		{
			name:        "initial-list add during the replay of a node whose InternalIP changed during initialization",
			listedNodes: []*v1.Node{gapNode("node1", gapNode1IP), gapNode("node2", changedIP)},
			reconcileNode2: func(dt *DiffTracker) { // ReconcileNodeAdd
				startupIPs := dt.startupNodeIPs["node2"]
				delete(dt.startupNodeIPs, "node2")
				dt.reconcileNodeIPChangeLocked("node2", startupIPs, []string{changedIP})
			},
		},
		{
			// The drain runs once the initial-list adds are processed, so it races later updates.
			name:        "update during the drain of a node deleted during initialization",
			listedNodes: []*v1.Node{gapNode("node1", gapNode1IP)},
			oldSlice:    startupSliceV1(),
			reconcileNode2: func(dt *DiffTracker) { // CompleteStartupReconciliation
				dt.reconcileNodeIPChangeLocked("node2", dt.startupNodeIPs["node2"], nil)
				dt.startupNodeIPs = nil
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dt := newStartupGapTracker(t, []*discovery_v1.EndpointSlice{startupSliceV1()}, tc.listedNodes...)

			// Hold the lock, start the event removing B, then reconcile node2 under the lock. An
			// event that swaps the cache before taking the lock hides B from this walk.
			dt.mu.Lock()
			eventDone := make(chan struct{})
			go func() {
				defer close(eventDone)
				dt.ReconcileEndpointSlice(tc.oldSlice, gapSlice("eps1", "2", gapEndpoint{gapPodA, "node1", true}))
			}()
			time.Sleep(200 * time.Millisecond)
			tc.reconcileNode2(dt)
			dt.mu.Unlock()

			select {
			case <-eventDone:
			case <-time.After(10 * time.Second):
				t.Fatal("slice event did not finish")
			}
			assert.False(t, gapRegistered(dt, gapNode2IP, gapPodB), "a removed endpoint must not stay at its startup IP")
			assert.False(t, gapRegistered(dt, changedIP, gapPodB))
			assert.True(t, gapRegistered(dt, gapNode1IP, gapPodA))
		})
	}
}

func TestReconcileNodeAdd(t *testing.T) {
	t.Run("initial-list add moves endpoints off an InternalIP changed during initialization", func(t *testing.T) {
		const changedIP = "10.0.0.22"
		dt := newStartupGapTracker(t, []*discovery_v1.EndpointSlice{startupSliceV1()},
			gapNode("node1", gapNode1IP), gapNode("node2", changedIP))

		dt.ReconcileNodeAdd("node2", []string{changedIP})

		assert.True(t, gapRegistered(dt, changedIP, gapPodB), "the endpoint must move to the node's current IP")
		assert.False(t, gapRegistered(dt, gapNode2IP, gapPodB), "the startup location must not be left behind")
	})

	t.Run("a node unknown to initialization registers its cached endpoints", func(t *testing.T) {
		const (
			node4IP = "10.0.0.4"
			podD    = "10.244.4.40"
		)
		dt := newStartupGapTracker(t, nil, gapNode("node4", node4IP))
		dt.initializeEndpointSlicesCache(&discovery_v1.EndpointSliceList{Items: []discovery_v1.EndpointSlice{
			*gapSlice("eps4", "1", gapEndpoint{podD, "node4", true}),
		}})

		dt.ReconcileNodeAdd("node4", []string{node4IP})

		assert.True(t, gapRegistered(dt, node4IP, podD))
	})
}

// TestCompleteStartupReconciliation_RemovesSlicesDeletedDuringInit pins that a slice deleted while
// initialization ran, which never produces a Delete event, is reconciled as deleted once the
// handlers have received the initial list.
func TestCompleteStartupReconciliation_RemovesSlicesDeletedDuringInit(t *testing.T) {
	t.Run("slice deleted", func(t *testing.T) {
		kept := gapSlice("eps1", "1", gapEndpoint{gapPodA, "node1", true})
		deleted := gapSlice("eps2", "1", gapEndpoint{gapPodB, "node2", true})
		dt := newStartupGapTracker(t, []*discovery_v1.EndpointSlice{kept, deleted},
			gapNode("node1", gapNode1IP), gapNode("node2", gapNode2IP))
		dt.ReconcileNodeAdd("node1", []string{gapNode1IP})
		dt.ReconcileNodeAdd("node2", []string{gapNode2IP})
		sliceIndexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{k8scache.NamespaceIndex: k8scache.MetaNamespaceIndexFunc})
		assert.NoError(t, sliceIndexer.Add(kept))

		dt.CompleteStartupReconciliation(discoverylisters.NewEndpointSliceLister(sliceIndexer))

		assert.False(t, gapRegistered(dt, gapNode2IP, gapPodB), "a deleted slice's endpoints must be removed")
		_, ok := dt.endpointSlicesCache.Load("ns/eps2")
		assert.False(t, ok, "a deleted slice must be evicted from the cache")
		assert.True(t, gapRegistered(dt, gapNode1IP, gapPodA), "a live slice must be kept")
		_, ok = dt.endpointSlicesCache.Load("ns/eps1")
		assert.True(t, ok)
	})

	// node2's drain walks the cache after the prune evicted the slice, so the prune itself must
	// resolve node2, which the lister no longer has, through its startup IPs.
	t.Run("slice deleted along with one of its nodes", func(t *testing.T) {
		dt := newStartupGapTracker(t, []*discovery_v1.EndpointSlice{startupSliceV1()}, gapNode("node1", gapNode1IP))
		dt.ReconcileNodeAdd("node1", []string{gapNode1IP})
		sliceIndexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{k8scache.NamespaceIndex: k8scache.MetaNamespaceIndexFunc})

		dt.CompleteStartupReconciliation(discoverylisters.NewEndpointSliceLister(sliceIndexer))

		assert.False(t, gapRegistered(dt, gapNode2IP, gapPodB), "a deleted slice's endpoints on a deleted node must be removed")
		assert.False(t, gapRegistered(dt, gapNode1IP, gapPodA))
	})
}

// TestCompleteStartupReconciliation_DrainsNodesDeletedDuringInit pins that a node deleted while
// initialization ran, which never produces a Node Delete, has its endpoints drained, and that the
// startup node IPs are released once startup reconciliation is done.
func TestCompleteStartupReconciliation_DrainsNodesDeletedDuringInit(t *testing.T) {
	dt := newStartupGapTracker(t, []*discovery_v1.EndpointSlice{startupSliceV1()}, gapNode("node1", gapNode1IP))
	dt.ReconcileNodeAdd("node1", []string{gapNode1IP})
	sliceIndexer := k8scache.NewIndexer(k8scache.MetaNamespaceKeyFunc, k8scache.Indexers{k8scache.NamespaceIndex: k8scache.MetaNamespaceIndexFunc})
	assert.NoError(t, sliceIndexer.Add(startupSliceV1()))

	dt.CompleteStartupReconciliation(discoverylisters.NewEndpointSliceLister(sliceIndexer))

	assert.False(t, gapRegistered(dt, gapNode2IP, gapPodB), "a deleted node's endpoints must be drained")
	assert.True(t, gapRegistered(dt, gapNode1IP, gapPodA), "a listed node's endpoints must be kept")
	assert.Nil(t, dt.startupNodeIPs)
}

// TestSeedInboundEndpointsFromCache_ResolvesUnreplayedNodesThroughStartupIPs pins that a service
// registered before a node's first Add is replayed is seeded under the IPs the slice events resolve
// that node through until then, so a removal arriving before the replay finds the address.
func TestSeedInboundEndpointsFromCache_ResolvesUnreplayedNodesThroughStartupIPs(t *testing.T) {
	const changedIP = "10.0.0.22"
	dt := newTestDiffTracker()
	dt.NRPResources.LoadBalancers.Insert(gapSvcUID)
	dt.startupNodeIPs = map[string][]string{"node2": {gapNode2IP}}
	dt.initializeEndpointSlicesCache(&discovery_v1.EndpointSliceList{Items: []discovery_v1.EndpointSlice{
		*gapSlice("eps1", "1", gapEndpoint{gapPodB, "node2", true}),
	}})
	setTestNodeLister(t, dt, gapNode("node2", changedIP))

	dt.seedInboundEndpointsFromCache(gapSvcUID)
	dt.ReconcileEndpointSlice(gapSlice("eps1", "1", gapEndpoint{gapPodB, "node2", true}), gapSlice("eps1", "2"))
	dt.ReconcileNodeAdd("node2", []string{changedIP})

	assert.False(t, gapRegistered(dt, gapNode2IP, gapPodB))
	assert.False(t, gapRegistered(dt, changedIP, gapPodB), "a removal before the replay must not leave the seeded address behind")
}
