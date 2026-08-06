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
	"testing"

	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func TestUpdateK8sService(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{
			Services: sets.NewString(),
		},
	}

	// Test Add operation
	err := dt.EnqueueK8sServiceOperation(UpdateK8sResource{
		Operation: Add,
		ID:        "service1",
	})
	assert.NoError(t, err)
	assert.True(t, dt.K8sResources.Services.Has("service1"))

	// Test Remove operation
	err = dt.EnqueueK8sServiceOperation(UpdateK8sResource{
		Operation: Remove,
		ID:        "service1",
	})
	assert.NoError(t, err)
	assert.False(t, dt.K8sResources.Services.Has("service1"))

	// Test invalid operation
	err = dt.EnqueueK8sServiceOperation(UpdateK8sResource{
		Operation: Update,
		ID:        "service1",
	})
	assert.Error(t, err)
}
func TestUpdateK8sEndpoints(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{
			Nodes: map[string]Node{},
		},
	}

	// Test adding new endpoint
	input := UpdateK8sEndpointsInputType{
		InboundIdentity: "service1",
		OldAddresses:    map[string]string{},
		NewAddresses:    map[string]string{"10.0.0.1": "node1"},
	}

	errs := dt.UpdateK8sEndpoints(input)
	assert.Empty(t, errs)

	// Verify the endpoint was added
	assert.Contains(t, dt.K8sResources.Nodes, "node1")
	assert.Contains(t, dt.K8sResources.Nodes["node1"].Pods, "10.0.0.1")
	assert.True(t, dt.K8sResources.Nodes["node1"].Pods["10.0.0.1"].InboundIdentities.Has("service1"))

	// Test removing an endpoint
	input = UpdateK8sEndpointsInputType{
		InboundIdentity: "service1",
		OldAddresses:    map[string]string{"10.0.0.1": "node1"},
		NewAddresses:    map[string]string{},
	}

	errs = dt.UpdateK8sEndpoints(input)
	assert.Empty(t, errs)

	// Verify the endpoint was removed
	assert.NotContains(t, dt.K8sResources.Nodes["node1"].Pods, "10.0.0.1")
}
func TestUpdateK8sPod(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{
			Nodes: map[string]Node{},
		},
	}

	// Test adding new egress assignment
	input := UpdatePodInputType{
		PodOperation:           Add,
		PublicOutboundIdentity: "public1",
		Location:               "node1",
		Address:                "10.0.0.1",
	}

	err := dt.UpdateK8sPod(input)
	assert.NoError(t, err)

	// Verify the egress assignment was added
	assert.Contains(t, dt.K8sResources.Nodes, "node1")
	assert.Contains(t, dt.K8sResources.Nodes["node1"].Pods, "10.0.0.1")
	assert.Equal(t, "public1", dt.K8sResources.Nodes["node1"].Pods["10.0.0.1"].PublicOutboundIdentity)

	// Test removing egress assignment
	input = UpdatePodInputType{
		PodOperation:           Remove,
		PublicOutboundIdentity: "public1",
		Location:               "node1",
		Address:                "10.0.0.1",
	}

	err = dt.UpdateK8sPod(input)
	assert.NoError(t, err)

	// Verify the egress assignment was removed
	assert.NotContains(t, dt.K8sResources.Nodes["node1"].Pods, "10.0.0.1")
}
func TestUpdateK8sEgress(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{
			Egresses: sets.NewString(),
		},
	}

	// Test Add operation
	err := dt.EnqueueK8sEgressOperation(UpdateK8sResource{
		Operation: Add,
		ID:        "egress1",
	})
	assert.NoError(t, err)
	assert.True(t, dt.K8sResources.Egresses.Has("egress1"))

	// Test Remove operation
	err = dt.EnqueueK8sEgressOperation(UpdateK8sResource{
		Operation: Remove,
		ID:        "egress1",
	})
	assert.NoError(t, err)
	assert.False(t, dt.K8sResources.Egresses.Has("egress1"))

	// Test invalid operation
	err = dt.EnqueueK8sEgressOperation(UpdateK8sResource{
		Operation: Update,
		ID:        "egress1",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "error - ResourceType=Egress, Operation=Update and ID=egress1")
}
func TestUpdateK8sEndpointsRelocation(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}

	errs := dt.UpdateK8sEndpoints(UpdateK8sEndpointsInputType{
		InboundIdentity: "svc1",
		NewAddresses:    map[string]string{"10.0.0.1": "node1"},
	})
	assert.Empty(t, errs)
	assert.True(t, dt.K8sResources.Nodes["node1"].Pods["10.0.0.1"].InboundIdentities.Has("svc1"))

	// Same pod IP moves from node1 to node2.
	errs = dt.UpdateK8sEndpoints(UpdateK8sEndpointsInputType{
		InboundIdentity: "svc1",
		OldAddresses:    map[string]string{"10.0.0.1": "node1"},
		NewAddresses:    map[string]string{"10.0.0.1": "node2"},
	})
	assert.Empty(t, errs)

	// Old node is gone, new node holds the pod with svc1.
	_, ok := dt.K8sResources.Nodes["node1"]
	assert.False(t, ok, "pod must be removed from the old node")
	pod, ok := dt.K8sResources.Nodes["node2"].Pods["10.0.0.1"]
	assert.True(t, ok, "pod must be added to the new node")
	assert.True(t, pod.InboundIdentities.Has("svc1"))
}
func TestUpdateK8sPodRejectsEmptyLocationOrAddress(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}

	err := dt.UpdateK8sPod(UpdatePodInputType{PodOperation: Add, Location: "", Address: "10.0.0.1"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must not be empty")

	err = dt.UpdateK8sPod(UpdatePodInputType{PodOperation: Add, Location: "node1", Address: ""})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must not be empty")
}

// TestUpdateK8sPodRemovePreservesInboundIdentities verifies that removing a pod's
// egress assignment clears only the outbound identity and keeps the pod while it
// still backs an inbound (LoadBalancer) service, whereas an egress-only pod is
// removed entirely and its ref-counter released.
func TestUpdateK8sPodRemovePreservesInboundIdentities(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}

	errs := dt.UpdateK8sEndpoints(UpdateK8sEndpointsInputType{
		InboundIdentity: "lb1",
		NewAddresses:    map[string]string{"10.0.0.1": "node1"},
	})
	assert.Empty(t, errs)
	assert.NoError(t, dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation:           Add,
		PublicOutboundIdentity: "egressA",
		Location:               "node1",
		Address:                "10.0.0.1",
	}))
	val, ok := dt.outboundIdentityPodRefCount.Load("egressa")
	assert.True(t, ok)
	assert.Equal(t, 1, val.(int))

	assert.NoError(t, dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation:           Remove,
		PublicOutboundIdentity: "egressA",
		Location:               "node1",
		Address:                "10.0.0.1",
	}))
	pod, ok := dt.K8sResources.Nodes["node1"].Pods["10.0.0.1"]
	assert.True(t, ok, "pod backing an inbound service must be kept")
	assert.True(t, pod.InboundIdentities.Has("lb1"))
	assert.Equal(t, "", pod.PublicOutboundIdentity)
	_, ok = dt.outboundIdentityPodRefCount.Load("egressa")
	assert.False(t, ok, "egress ref-counter must be released")

	assert.NoError(t, dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation:           Add,
		PublicOutboundIdentity: "egressB",
		Location:               "node2",
		Address:                "10.0.0.2",
	}))
	assert.NoError(t, dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation:           Remove,
		PublicOutboundIdentity: "egressB",
		Location:               "node2",
		Address:                "10.0.0.2",
	}))
	_, nodeOK := dt.K8sResources.Nodes["node2"]
	assert.False(t, nodeOK, "egress-only pod and its empty node must be removed")
	_, ok = dt.outboundIdentityPodRefCount.Load("egressb")
	assert.False(t, ok)
}

// TestGetSyncLocationsAddressesRemovesGoneNode verifies that when a node is gone
// from K8s but still present in NRP, the location is emitted with an empty Addresses
// map (AddressUpdateAction PartialUpdate). The Service Gateway treats an empty
// addresses array as null and deletes the location; applying the result also drops
// the location locally.

// TestUpdateEndpoints_AddIsIdempotentWhenPriorAddWasMissed asserts an update reporting an unchanged
// pod location (old == new) still records the pod when the engine is not yet tracking it: an add
// whose node IP was not yet cached produces no state, and skipping the later update would strand it.
func TestUpdateEndpoints_AddIsIdempotentWhenPriorAddWasMissed(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-endpoint-readd"
	dt.NRPResources.LoadBalancers.Insert(uid)

	const podIP = "10.244.1.7"
	const node = "10.0.0.20"

	dt.UpdateEndpoints(uid, map[string]string{podIP: node}, map[string]string{podIP: node})

	n, ok := dt.K8sResources.Nodes[node]
	if assert.True(t, ok, "node must be registered by the self-healing add") {
		pod, onNode := n.Pods[podIP]
		if assert.True(t, onNode, "pod must be recorded even though the update reported old == new") {
			assert.True(t, pod.InboundIdentities.Has(uid), "pod must carry the inbound identity")
		}
	}
}
