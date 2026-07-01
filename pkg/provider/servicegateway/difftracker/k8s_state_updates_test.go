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

func TestEnqueueK8sServiceOperation(t *testing.T) {
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
	assert.NotContains(t, dt.K8sResources.Nodes["node1"].Pods, "10.0.0.1")
}

// TestUpdateK8sPodIdentityChangeDecrementsOld verifies that when a pod's egress
// identity changes (X -> Y), the old identity's ref-counter is decremented so it
// doesn't leak, while the new identity is counted.
func TestUpdateK8sPodIdentityChangeDecrementsOld(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}

	// Pod starts using egress "X".
	assert.NoError(t, dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation:           Add,
		PublicOutboundIdentity: "X",
		Location:               "node1",
		Address:                "10.0.0.1",
	}))
	val, ok := dt.outboundIdentityPodRefCount.Load("x")
	assert.True(t, ok)
	assert.Equal(t, 1, val.(int))

	// Pod's egress changes to "Y": X must be released, Y counted.
	assert.NoError(t, dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation:           Update,
		PublicOutboundIdentity: "Y",
		Location:               "node1",
		Address:                "10.0.0.1",
	}))
	_, ok = dt.outboundIdentityPodRefCount.Load("x")
	assert.False(t, ok, "old identity X must be decremented on identity change, not leaked")
	val, ok = dt.outboundIdentityPodRefCount.Load("y")
	assert.True(t, ok)
	assert.Equal(t, 1, val.(int))
}

// TestUpdateK8sPodCaseInsensitiveReAdd verifies that re-adding the same pod with a
// different-cased identity is treated as the same identity (no double counting),
// since the counter is keyed case-insensitively.
func TestUpdateK8sPodCaseInsensitiveReAdd(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}

	assert.NoError(t, dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation:           Add,
		PublicOutboundIdentity: "Svc1",
		Location:               "node1",
		Address:                "10.0.0.1",
	}))
	// Informer resync delivers the same identity with different casing.
	assert.NoError(t, dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation:           Add,
		PublicOutboundIdentity: "svc1",
		Location:               "node1",
		Address:                "10.0.0.1",
	}))

	val, ok := dt.outboundIdentityPodRefCount.Load("svc1")
	assert.True(t, ok)
	assert.Equal(t, 1, val.(int), "case-only re-ADD must not double-count the identity")
}

// TestUpdateK8sPodRemoveDecrementsCounter verifies that removing a pod decrements
// the outbound-identity ref-counter for the identity passed in the remove input
// (matching the engine's DeletePod, which always passes the service UID), clearing
// the entry when the last pod for that identity is removed.
func TestUpdateK8sPodRemoveDecrementsCounter(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{Nodes: map[string]Node{}},
	}

	// Add a pod with outbound identity "public1".
	assert.NoError(t, dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation:           Add,
		PublicOutboundIdentity: "public1",
		Location:               "node1",
		Address:                "10.0.0.1",
	}))
	val, ok := dt.outboundIdentityPodRefCount.Load("public1")
	assert.True(t, ok)
	assert.Equal(t, 1, val.(int))

	// Remove passing the same identity; the counter must be cleared.
	assert.NoError(t, dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation:           Remove,
		PublicOutboundIdentity: "public1",
		Location:               "node1",
		Address:                "10.0.0.1",
	}))

	_, ok = dt.outboundIdentityPodRefCount.Load("public1")
	assert.False(t, ok, "counter for public1 should be removed after the last pod is removed")
	_, ok = dt.outboundIdentityPodRefCount.Load("")
	assert.False(t, ok, "no counter should be created for an empty identity")
}
func TestEnqueueK8sEgressOperation(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{
			Egresses: sets.NewString(),
		},
	}

	err := dt.EnqueueK8sEgressOperation(UpdateK8sResource{Operation: Add, ID: "egress1"})
	assert.NoError(t, err)
	assert.True(t, dt.K8sResources.Egresses.Has("egress1"))

	err = dt.EnqueueK8sEgressOperation(UpdateK8sResource{Operation: Remove, ID: "egress1"})
	assert.NoError(t, err)
	assert.False(t, dt.K8sResources.Egresses.Has("egress1"))

	err = dt.EnqueueK8sEgressOperation(UpdateK8sResource{Operation: Update, ID: "egress1"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "error - ResourceType=Egress, Operation=Update and ID=egress1")
}
func TestEnqueueK8sResourceOperationErrors(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{Services: sets.NewString(), Egresses: sets.NewString()},
	}

	// Empty ID.
	err := dt.EnqueueK8sServiceOperation(UpdateK8sResource{Operation: Add, ID: ""})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty ID")

	// Invalid operation (UPDATE is not valid for the resource set).
	err = dt.EnqueueK8sEgressOperation(UpdateK8sResource{Operation: Update, ID: "egress1"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Operation=Update")

	// Successful Add then Remove.
	assert.NoError(t, dt.EnqueueK8sServiceOperation(UpdateK8sResource{Operation: Add, ID: "svc1"}))
	assert.True(t, dt.K8sResources.Services.Has("svc1"))
	assert.NoError(t, dt.EnqueueK8sServiceOperation(UpdateK8sResource{Operation: Remove, ID: "svc1"}))
	assert.False(t, dt.K8sResources.Services.Has("svc1"))
}

// TestUpdateK8sPodInvalidOperation covers the default branch of updateK8sPodLocked.
func TestUpdateK8sPodInvalidOperation(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}
	err := dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation: Operation(99),
		Location:     "node1",
		Address:      "10.0.0.1",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid pod operation")
}

// TestUpdateK8sPodRemoveNonExistent covers the duplicate-removal branch.
func TestUpdateK8sPodRemoveNonExistent(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}
	// Removing a pod that was never added is a no-op (no error, no counter change).
	err := dt.UpdateK8sPod(UpdatePodInputType{
		PodOperation:           Remove,
		PublicOutboundIdentity: "public1",
		Location:               "node1",
		Address:                "10.0.0.1",
	})
	assert.NoError(t, err)
	_, ok := dt.outboundIdentityPodRefCount.Load("public1")
	assert.False(t, ok)
}

// TestUpdateK8sEndpointsMissingLocation covers the error branch where an address
// has no associated node location.
func TestUpdateK8sEndpointsMissingLocation(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}
	errs := dt.UpdateK8sEndpoints(UpdateK8sEndpointsInputType{
		InboundIdentity: "svc1",
		NewAddresses:    map[string]string{"10.0.0.1": ""},
	})
	assert.NotEmpty(t, errs)
	assert.Contains(t, errs[0].Error(), "does not have a node associated")
}

// TestRemoveServiceFromK8sStateLocked covers removeServiceFromK8sStateLocked for
// both inbound and outbound identities, including empty-pod/node cleanup.
func TestRemoveServiceFromK8sStateLocked(t *testing.T) {
	t.Run("inbound removal cleans up empty pods and nodes", func(t *testing.T) {
		dt := &DiffTracker{
			K8sResources: K8sState{
				Nodes: map[string]Node{
					"node1": {Pods: map[string]Pod{
						// Pod backs only svc1 inbound -> becomes empty and is removed.
						"10.0.0.1": {InboundIdentities: sets.NewString("svc1")},
						// Pod backs svc1 and svc2 -> keeps svc2.
						"10.0.0.2": {InboundIdentities: sets.NewString("svc1", "svc2")},
					}},
				},
			},
		}
		dt.removeServiceFromK8sStateLocked("svc1", true)

		// 10.0.0.1 had only svc1 -> removed.
		_, ok := dt.K8sResources.Nodes["node1"].Pods["10.0.0.1"]
		assert.False(t, ok)
		// 10.0.0.2 still has svc2.
		pod, ok := dt.K8sResources.Nodes["node1"].Pods["10.0.0.2"]
		assert.True(t, ok)
		assert.True(t, pod.InboundIdentities.Has("svc2"))
		assert.False(t, pod.InboundIdentities.Has("svc1"))
	})

	t.Run("outbound removal clears identity and removes empty node", func(t *testing.T) {
		dt := &DiffTracker{
			K8sResources: K8sState{
				Nodes: map[string]Node{
					"node1": {Pods: map[string]Pod{
						"10.0.0.1": {InboundIdentities: sets.NewString(), PublicOutboundIdentity: "egress1"},
					}},
				},
			},
		}
		dt.removeServiceFromK8sStateLocked("egress1", false)

		// Pod had only the outbound identity -> pod and node removed.
		_, nodeOK := dt.K8sResources.Nodes["node1"]
		assert.False(t, nodeOK)
	})

	t.Run("public wrapper acquires lock and removes inbound identity", func(t *testing.T) {
		dt := &DiffTracker{
			K8sResources: K8sState{
				Nodes: map[string]Node{
					"node1": {Pods: map[string]Pod{
						"10.0.0.1": {InboundIdentities: sets.NewString("svc1")},
					}},
				},
			},
		}
		dt.RemoveServiceFromK8sState("svc1", true)

		_, nodeOK := dt.K8sResources.Nodes["node1"]
		assert.False(t, nodeOK)
	})

	t.Run("outbound service removal decrements ref-counter", func(t *testing.T) {
		dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}
		// Add an egress pod so the counter is seeded.
		assert.NoError(t, dt.UpdateK8sPod(UpdatePodInputType{
			PodOperation:           Add,
			PublicOutboundIdentity: "egress1",
			Location:               "node1",
			Address:                "10.0.0.1",
		}))
		val, ok := dt.outboundIdentityPodRefCount.Load("egress1")
		assert.True(t, ok)
		assert.Equal(t, 1, val.(int))

		// Removing the egress service must release the counter, not leak it.
		dt.RemoveServiceFromK8sState("egress1", false)
		_, ok = dt.outboundIdentityPodRefCount.Load("egress1")
		assert.False(t, ok, "service removal must decrement the outbound ref-counter")
	})
}

// TestUpdateK8sPodAddNoOutboundIdentity verifies that adding a pod with no
// outbound access (empty PublicOutboundIdentity) tracks the pod in state but
// does not create a bogus ref-counter entry under the empty-string key. This
// keeps the Add path symmetric with the Remove path, which skips the decrement
// for an empty identity.
func TestUpdateK8sPodAddNoOutboundIdentity(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}
	in := UpdatePodInputType{
		PodOperation:           Add,
		PublicOutboundIdentity: "",
		Location:               "node1",
		Address:                "10.0.0.1",
	}
	assert.NoError(t, dt.UpdateK8sPod(in))

	// The pod must be tracked in state.
	pod, ok := dt.K8sResources.Nodes["node1"].Pods["10.0.0.1"]
	assert.True(t, ok)
	assert.Equal(t, "", pod.PublicOutboundIdentity)

	// No counter entry should be created for the empty identity.
	_, ok = dt.outboundIdentityPodRefCount.Load("")
	assert.False(t, ok, "no counter should be created for an empty outbound identity")
}

// TestUpdateK8sPodAddIdempotent covers the alreadyExists branch of updateK8sPodLocked:
// a repeated Add for the same pod+identity must not double-count the counter.
func TestUpdateK8sPodAddIdempotent(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}
	in := UpdatePodInputType{
		PodOperation:           Add,
		PublicOutboundIdentity: "public1",
		Location:               "node1",
		Address:                "10.0.0.1",
	}
	assert.NoError(t, dt.UpdateK8sPod(in))
	// Second identical Add (e.g. informer resync) must be a no-op for the counter.
	assert.NoError(t, dt.UpdateK8sPod(in))

	val, ok := dt.outboundIdentityPodRefCount.Load("public1")
	assert.True(t, ok)
	assert.Equal(t, 1, val.(int))
}

// TestUpdateK8sEndpointsAddThenRemove covers the OldAddresses removal path of
// updateK8sEndpointsLocked, including empty-pod/node cleanup.
func TestUpdateK8sEndpointsAddThenRemove(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}

	// Add endpoint for svc1 at pod 10.0.0.1 on node1.
	errs := dt.UpdateK8sEndpoints(UpdateK8sEndpointsInputType{
		InboundIdentity: "svc1",
		NewAddresses:    map[string]string{"10.0.0.1": "node1"},
	})
	assert.Empty(t, errs)
	assert.True(t, dt.K8sResources.Nodes["node1"].Pods["10.0.0.1"].InboundIdentities.Has("svc1"))

	// Remove the same endpoint (now in OldAddresses, absent from NewAddresses).
	errs = dt.UpdateK8sEndpoints(UpdateK8sEndpointsInputType{
		InboundIdentity: "svc1",
		OldAddresses:    map[string]string{"10.0.0.1": "node1"},
	})
	assert.Empty(t, errs)
	// Pod had only svc1 -> pod and node cleaned up.
	_, ok := dt.K8sResources.Nodes["node1"]
	assert.False(t, ok)
}

// TestUpdateK8sEndpointsRelocation covers the case where the same pod IP appears
// in both OldAddresses and NewAddresses but on a different node (relocation): the
// pod must be removed from the old node and added to the new one.
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
// from K8s but still present in NRP, the location is emitted with an empty
// Addresses map (a PartialUpdate that deletes the whole location on the Service
// Gateway), and applying the result drops the location locally.
