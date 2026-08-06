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
