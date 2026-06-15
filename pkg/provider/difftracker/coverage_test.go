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
	"go.uber.org/mock/gomock"

	"k8s.io/client-go/kubernetes/fake"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func validTestConfig() Config {
	return Config{
		SubscriptionID:             "test-subscription",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		VNetName:                   "test-vnet",
		ServiceGatewayResourceName: "test-sgw",
		ServiceGatewayID:           "/subscriptions/test-subscription/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}
}

// TestInitializeDiffTrackerErrorPaths covers the validation/error branches of
// InitializeDiffTracker and the nil-field initialization.
func TestInitializeDiffTrackerErrorPaths(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockKubeClient := fake.NewSimpleClientset()

	// Invalid config (empty) -> validation error.
	_, err := InitializeDiffTracker(K8sState{}, NRPState{}, Config{}, mockFactory, mockKubeClient)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InitializeDiffTracker")

	// Nil networkClientFactory.
	_, err = InitializeDiffTracker(K8sState{}, NRPState{}, validTestConfig(), nil, mockKubeClient)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "networkClientFactory must not be nil")

	// Nil kubeClient.
	_, err = InitializeDiffTracker(K8sState{}, NRPState{}, validTestConfig(), mockFactory, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "kubeClient must not be nil")

	// Valid call with empty states -> nil fields get initialized.
	dt, err := InitializeDiffTracker(K8sState{}, NRPState{}, validTestConfig(), mockFactory, mockKubeClient)
	assert.NoError(t, err)
	assert.NotNil(t, dt)
	assert.NotNil(t, dt.K8sResources.Services)
	assert.NotNil(t, dt.K8sResources.Egresses)
	assert.NotNil(t, dt.K8sResources.Nodes)
	assert.NotNil(t, dt.NRPResources.LoadBalancers)
	assert.NotNil(t, dt.NRPResources.NATGateways)
	assert.NotNil(t, dt.NRPResources.Locations)
}

// TestEnqueueK8sResourceOperationErrors covers the empty-ID and invalid-operation
// branches of enqueueK8sResourceOperation (via the public wrappers).
func TestEnqueueK8sResourceOperationErrors(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{Services: sets.NewString(), Egresses: sets.NewString()},
	}

	// Empty ID.
	err := dt.EnqueueK8sServiceOperation(UpdateK8sResource{Operation: ADD, ID: ""})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty ID")

	// Invalid operation (UPDATE is not valid for the resource set).
	err = dt.EnqueueK8sEgressOperation(UpdateK8sResource{Operation: UPDATE, ID: "egress1"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Operation=UPDATE")

	// Successful ADD then REMOVE.
	assert.NoError(t, dt.EnqueueK8sServiceOperation(UpdateK8sResource{Operation: ADD, ID: "svc1"}))
	assert.True(t, dt.K8sResources.Services.Has("svc1"))
	assert.NoError(t, dt.EnqueueK8sServiceOperation(UpdateK8sResource{Operation: REMOVE, ID: "svc1"}))
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
		PodOperation:           REMOVE,
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
}

// TestLocationDataEqualsMoreCases covers additional LocationData.Equals branches.
func TestLocationDataEqualsMoreCases(t *testing.T) {
	base := LocationData{
		Action: PartialUpdate,
		Locations: map[string]Location{
			"node1": {
				AddressUpdateAction: PartialUpdate,
				Addresses:           map[string]Address{"10.0.0.1": {ServiceRef: sets.NewString("svc1")}},
			},
		},
	}
	equal := LocationData{
		Action: PartialUpdate,
		Locations: map[string]Location{
			"node1": {
				AddressUpdateAction: PartialUpdate,
				Addresses:           map[string]Address{"10.0.0.1": {ServiceRef: sets.NewString("svc1")}},
			},
		},
	}
	assert.True(t, base.Equals(&equal))

	// Different top-level Action.
	diffAction := equal
	diffAction.Action = FullUpdate
	assert.False(t, base.Equals(&diffAction))

	// Different number of locations.
	diffLen := LocationData{Action: PartialUpdate, Locations: map[string]Location{}}
	assert.False(t, base.Equals(&diffLen))

	// Missing location name.
	diffName := LocationData{
		Action:    PartialUpdate,
		Locations: map[string]Location{"node2": base.Locations["node1"]},
	}
	assert.False(t, base.Equals(&diffName))

	// Different AddressUpdateAction.
	diffAUA := LocationData{
		Action: PartialUpdate,
		Locations: map[string]Location{
			"node1": {AddressUpdateAction: FullUpdate, Addresses: base.Locations["node1"].Addresses},
		},
	}
	assert.False(t, base.Equals(&diffAUA))

	// Different addresses length.
	diffAddrLen := LocationData{
		Action: PartialUpdate,
		Locations: map[string]Location{
			"node1": {AddressUpdateAction: PartialUpdate, Addresses: map[string]Address{}},
		},
	}
	assert.False(t, base.Equals(&diffAddrLen))

	// Missing address name.
	diffAddrName := LocationData{
		Action: PartialUpdate,
		Locations: map[string]Location{
			"node1": {AddressUpdateAction: PartialUpdate, Addresses: map[string]Address{"10.0.0.2": {ServiceRef: sets.NewString("svc1")}}},
		},
	}
	assert.False(t, base.Equals(&diffAddrName))

	// Different ServiceRef.
	diffRef := LocationData{
		Action: PartialUpdate,
		Locations: map[string]Location{
			"node1": {AddressUpdateAction: PartialUpdate, Addresses: map[string]Address{"10.0.0.1": {ServiceRef: sets.NewString("svc2")}}},
		},
	}
	assert.False(t, base.Equals(&diffRef))
}

// TestSyncDiffTrackerReturnTypeEquals covers SyncDiffTrackerReturnType.Equals branches.
func TestSyncDiffTrackerReturnTypeEquals(t *testing.T) {
	mk := func() SyncDiffTrackerReturnType {
		return SyncDiffTrackerReturnType{
			SyncStatus:          Success,
			LoadBalancerUpdates: SyncServicesReturnType{Additions: sets.NewString("a"), Removals: sets.NewString("b")},
			NATGatewayUpdates:   SyncServicesReturnType{Additions: sets.NewString("c"), Removals: sets.NewString("d")},
			LocationData:        LocationData{Action: PartialUpdate, Locations: map[string]Location{}},
		}
	}

	base := mk()
	equal := mk()
	assert.True(t, base.Equals(&equal))

	// Different SyncStatus.
	diffStatus := mk()
	diffStatus.SyncStatus = AlreadyInSync
	assert.False(t, base.Equals(&diffStatus))

	// Different LB additions.
	diffLBAdd := mk()
	diffLBAdd.LoadBalancerUpdates.Additions = sets.NewString("x")
	assert.False(t, base.Equals(&diffLBAdd))

	// Different LB removals.
	diffLBRem := mk()
	diffLBRem.LoadBalancerUpdates.Removals = sets.NewString("x")
	assert.False(t, base.Equals(&diffLBRem))

	// Different NATGW additions.
	diffNGAdd := mk()
	diffNGAdd.NATGatewayUpdates.Additions = sets.NewString("x")
	assert.False(t, base.Equals(&diffNGAdd))

	// Different NATGW removals.
	diffNGRem := mk()
	diffNGRem.NATGatewayUpdates.Removals = sets.NewString("x")
	assert.False(t, base.Equals(&diffNGRem))

	// Different LocationData.
	diffLoc := mk()
	diffLoc.LocationData.Action = FullUpdate
	assert.False(t, base.Equals(&diffLoc))
}

// TestDiffTrackerEqualsMoreCases covers additional DiffTracker.Equals branches.
func TestDiffTrackerEqualsMoreCases(t *testing.T) {
	mk := func() *DiffTracker {
		return &DiffTracker{
			K8sResources: K8sState{
				Services: sets.NewString("svc1"),
				Egresses: sets.NewString("egr1"),
				Nodes: map[string]Node{
					"node1": {Pods: map[string]Pod{
						"10.0.0.1": {InboundIdentities: sets.NewString("svc1"), PublicOutboundIdentity: "egr1"},
					}},
				},
			},
			NRPResources: NRPState{
				LoadBalancers: sets.NewString("svc1"),
				NATGateways:   sets.NewString("egr1"),
				Locations: map[string]NRPLocation{
					"node1": {Addresses: map[string]NRPAddress{
						"10.0.0.1": {Services: sets.NewString("svc1", "egr1")},
					}},
				},
			},
		}
	}

	assert.True(t, mk().Equals(mk()))

	// Different Services.
	a := mk()
	b := mk()
	b.K8sResources.Services = sets.NewString("other")
	assert.False(t, a.Equals(b))

	// Different Egresses.
	b = mk()
	b.K8sResources.Egresses = sets.NewString("other")
	assert.False(t, mk().Equals(b))

	// Different node count.
	b = mk()
	b.K8sResources.Nodes["node2"] = Node{Pods: map[string]Pod{}}
	assert.False(t, mk().Equals(b))

	// Missing node name.
	b = mk()
	delete(b.K8sResources.Nodes, "node1")
	b.K8sResources.Nodes["nodeX"] = Node{Pods: map[string]Pod{
		"10.0.0.1": {InboundIdentities: sets.NewString("svc1"), PublicOutboundIdentity: "egr1"},
	}}
	assert.False(t, mk().Equals(b))

	// Different pod count.
	b = mk()
	n := b.K8sResources.Nodes["node1"]
	n.Pods["10.0.0.2"] = Pod{InboundIdentities: sets.NewString()}
	assert.False(t, mk().Equals(b))

	// Missing pod address.
	b = mk()
	n = b.K8sResources.Nodes["node1"]
	delete(n.Pods, "10.0.0.1")
	n.Pods["10.0.0.9"] = Pod{InboundIdentities: sets.NewString("svc1"), PublicOutboundIdentity: "egr1"}
	assert.False(t, mk().Equals(b))

	// Different InboundIdentities.
	b = mk()
	n = b.K8sResources.Nodes["node1"]
	n.Pods["10.0.0.1"] = Pod{InboundIdentities: sets.NewString("other"), PublicOutboundIdentity: "egr1"}
	assert.False(t, mk().Equals(b))

	// Different PublicOutboundIdentity.
	b = mk()
	n = b.K8sResources.Nodes["node1"]
	n.Pods["10.0.0.1"] = Pod{InboundIdentities: sets.NewString("svc1"), PublicOutboundIdentity: "other"}
	assert.False(t, mk().Equals(b))

	// Different NRP LoadBalancers.
	b = mk()
	b.NRPResources.LoadBalancers = sets.NewString("other")
	assert.False(t, mk().Equals(b))

	// Different NRP NATGateways.
	b = mk()
	b.NRPResources.NATGateways = sets.NewString("other")
	assert.False(t, mk().Equals(b))

	// Different NRP location count.
	b = mk()
	b.NRPResources.Locations["node2"] = NRPLocation{Addresses: map[string]NRPAddress{}}
	assert.False(t, mk().Equals(b))
}

// TestUpdateK8sPodAddIdempotent covers the alreadyExists branch of updateK8sPodLocked:
// a repeated ADD for the same pod+identity must not double-count the counter.
func TestUpdateK8sPodAddIdempotent(t *testing.T) {
	dt := &DiffTracker{K8sResources: K8sState{Nodes: map[string]Node{}}}
	in := UpdatePodInputType{
		PodOperation:           ADD,
		PublicOutboundIdentity: "public1",
		Location:               "node1",
		Address:                "10.0.0.1",
	}
	assert.NoError(t, dt.UpdateK8sPod(in))
	// Second identical ADD (e.g. informer resync) must be a no-op for the counter.
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

// TestDeepEqualMoreCases covers the node/pod/identity mismatch branches of DeepEqual.
func TestDeepEqualMoreCases(t *testing.T) {
	// Helper to build a DiffTracker that is in-sync by construction.
	inSync := func() *DiffTracker {
		return &DiffTracker{
			K8sResources: K8sState{
				Services: sets.NewString("svc1"),
				Egresses: sets.NewString("egr1"),
				Nodes: map[string]Node{
					"node1": {Pods: map[string]Pod{
						"10.0.0.1": {InboundIdentities: sets.NewString("svc1"), PublicOutboundIdentity: "egr1"},
					}},
				},
			},
			NRPResources: NRPState{
				LoadBalancers: sets.NewString("svc1"),
				NATGateways:   sets.NewString("egr1"),
				Locations: map[string]NRPLocation{
					"node1": {Addresses: map[string]NRPAddress{
						"10.0.0.1": {Services: sets.NewString("svc1", "egr1")},
					}},
				},
			},
		}
	}

	assert.True(t, inSync().DeepEqual())

	// LoadBalancer present in NRP but not in K8s Services (reverse-direction check).
	d := inSync()
	d.NRPResources.LoadBalancers = sets.NewString("svc1", "extra")
	d.K8sResources.Services = sets.NewString("svc1", "different")
	assert.False(t, d.DeepEqual())

	// Egress name mismatch (reverse direction).
	d = inSync()
	d.NRPResources.NATGateways = sets.NewString("other")
	d.K8sResources.Egresses = sets.NewString("egr1")
	// lengths equal (1==1) but names differ -> mismatch
	d.NRPResources.NATGateways = sets.NewString("egr1")
	d.K8sResources.Egresses = sets.NewString("egr2")
	d.NRPResources.NATGateways = sets.NewString("egr2x")
	assert.False(t, d.DeepEqual())

	// Nodes vs Locations length mismatch.
	d = inSync()
	d.NRPResources.Locations["node2"] = NRPLocation{Addresses: map[string]NRPAddress{}}
	assert.False(t, d.DeepEqual())

	// Node missing in Locations (same count, different key).
	d = inSync()
	delete(d.NRPResources.Locations, "node1")
	d.NRPResources.Locations["nodeX"] = NRPLocation{Addresses: map[string]NRPAddress{
		"10.0.0.1": {Services: sets.NewString("svc1", "egr1")},
	}}
	assert.False(t, d.DeepEqual())

	// Pods vs Addresses length mismatch.
	d = inSync()
	loc := d.NRPResources.Locations["node1"]
	loc.Addresses["10.0.0.2"] = NRPAddress{Services: sets.NewString("svc1")}
	assert.False(t, d.DeepEqual())

	// Pod missing in Addresses (same count, different key).
	d = inSync()
	loc = d.NRPResources.Locations["node1"]
	delete(loc.Addresses, "10.0.0.1")
	loc.Addresses["10.0.0.9"] = NRPAddress{Services: sets.NewString("svc1", "egr1")}
	assert.False(t, d.DeepEqual())

	// Combined identities length mismatch.
	d = inSync()
	loc = d.NRPResources.Locations["node1"]
	loc.Addresses["10.0.0.1"] = NRPAddress{Services: sets.NewString("svc1")}
	assert.False(t, d.DeepEqual())

	// Identity not found in Services (same count, different identity).
	d = inSync()
	loc = d.NRPResources.Locations["node1"]
	loc.Addresses["10.0.0.1"] = NRPAddress{Services: sets.NewString("svc1", "egrX")}
	assert.False(t, d.DeepEqual())
}
