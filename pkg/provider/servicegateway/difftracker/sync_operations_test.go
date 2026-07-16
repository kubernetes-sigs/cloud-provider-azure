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
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func TestGetSyncLoadBalancerServices(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{
			Services: sets.NewString("service1", "service2", "service3"),
		},
		NRPResources: NRPState{
			LoadBalancers: sets.NewString("service2", "service3", "service4"),
		},
	}

	result := dt.GetSyncLoadBalancerServices()

	// Check additions (in K8s but not in NRP)
	assert.True(t, result.Additions.Has("service1"))
	assert.Equal(t, 1, result.Additions.Len())

	// Check removals (in NRP but not in K8s)
	assert.True(t, result.Removals.Has("service4"))
	assert.Equal(t, 1, result.Removals.Len())
}
func TestGetSyncLocationsAddresses(t *testing.T) {
	// Setup a diff tracker with some pods and services
	// Note: Services must be "ready" (exist in NRP or be in StateCreated) to be included in sync
	dt := &DiffTracker{
		K8sResources: K8sState{
			Nodes: map[string]Node{
				"node1": {
					Pods: map[string]Pod{
						"10.0.0.1": {
							InboundIdentities:      sets.NewString("service1"),
							PublicOutboundIdentity: "public1",
						},
					},
				},
			},
		},
		NRPResources: NRPState{
			LoadBalancers: sets.NewString("service1"), // Service must exist in NRP to pass filtering
			NATGateways:   sets.NewString("public1"),  // NAT Gateway must exist in NRP to pass filtering
			Locations:     map[string]NRPLocation{},
		},
	}

	// Get sync data
	result := dt.GetSyncLocationsAddresses()

	// Verify the result
	assert.Equal(t, PartialUpdate, result.Action)
	assert.Len(t, result.Locations, 1)

	// Use a key from the map instead of an index
	location := result.Locations["node1"]
	assert.NotNil(t, location)
	assert.Equal(t, FullUpdate, location.AddressUpdateAction)
	assert.Len(t, location.Addresses, 1)

	// Since location.Addresses is a map, we need to get the key first
	var address string
	for addr := range location.Addresses {
		address = addr
		break
	}

	assert.Equal(t, "10.0.0.1", address)
	assert.True(t, location.Addresses[address].ServiceRef.Has("service1"))
	assert.True(t, location.Addresses[address].ServiceRef.Has("public1"))
}
func TestGetSyncNRPNATGateways(t *testing.T) {
	tests := []struct {
		name              string
		k8sEgresses       []string
		nrpNATGateways    []string
		expectedAdditions []string
		expectedRemovals  []string
	}{
		{
			name:              "empty states",
			k8sEgresses:       []string{},
			nrpNATGateways:    []string{},
			expectedAdditions: []string{},
			expectedRemovals:  []string{},
		},
		{
			name:              "egresses in K8s but not in NRP",
			k8sEgresses:       []string{"egress1", "egress2"},
			nrpNATGateways:    []string{},
			expectedAdditions: []string{"egress1", "egress2"},
			expectedRemovals:  []string{},
		},
		{
			name:              "egresses in NRP but not in K8s",
			k8sEgresses:       []string{},
			nrpNATGateways:    []string{"egress1", "egress2"},
			expectedAdditions: []string{},
			expectedRemovals:  []string{"egress1", "egress2"},
		},
		{
			name:              "same egresses in both K8s and NRP",
			k8sEgresses:       []string{"egress1", "egress2"},
			nrpNATGateways:    []string{"egress1", "egress2"},
			expectedAdditions: []string{},
			expectedRemovals:  []string{},
		},
		{
			name:              "mixed state with additions and removals",
			k8sEgresses:       []string{"egress1", "egress3", "egress5"},
			nrpNATGateways:    []string{"egress1", "egress2", "egress4"},
			expectedAdditions: []string{"egress3", "egress5"},
			expectedRemovals:  []string{"egress2", "egress4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Initialize DiffTracker with the test case data
			dt := &DiffTracker{
				K8sResources: K8sState{
					Egresses: sets.NewString(tt.k8sEgresses...),
				},
				NRPResources: NRPState{
					NATGateways: sets.NewString(tt.nrpNATGateways...),
				},
			}

			// Call the function being tested
			result := dt.GetSyncNRPNATGateways()

			// Check additions
			assert.Equal(t, len(tt.expectedAdditions), result.Additions.Len(),
				"Expected %d additions, got %d", len(tt.expectedAdditions), result.Additions.Len())
			for _, addition := range tt.expectedAdditions {
				assert.True(t, result.Additions.Has(addition),
					"Expected Additions to contain %s", addition)
			}

			// Check removals
			assert.Equal(t, len(tt.expectedRemovals), result.Removals.Len(),
				"Expected %d removals, got %d", len(tt.expectedRemovals), result.Removals.Len())
			for _, removal := range tt.expectedRemovals {
				assert.True(t, result.Removals.Has(removal),
					"Expected Removals to contain %s", removal)
			}
		})
	}
}
func TestGetSyncOperations(t *testing.T) {
	tests := []struct {
		name                    string
		initialState            *DiffTracker
		expectedSyncStatus      SyncStatus
		expectedLoadBalancerOps bool
		expectedNATGatewayOps   bool
		expectedLocationOps     bool
	}{
		{
			name: "states already in sync",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("service1"),
					Egresses: sets.NewString("egress1"),
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities: sets.NewString("service1"),
								},
							},
						},
					},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1"),
					NATGateways:   sets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: sets.NewString("service1"),
								},
							},
						},
					},
				},
			},
			expectedSyncStatus:      AlreadyInSync,
			expectedLoadBalancerOps: false,
			expectedNATGatewayOps:   false,
			expectedLocationOps:     false,
		},
		{
			name: "services out of sync",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("service1", "service2"),
					Egresses: sets.NewString("egress1"),
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities: sets.NewString("service1"),
								},
							},
						},
					},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1"),
					NATGateways:   sets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: sets.NewString("service1"),
								},
							},
						},
					},
				},
			},
			expectedSyncStatus:      Success,
			expectedLoadBalancerOps: true,
			expectedNATGatewayOps:   false,
			expectedLocationOps:     false,
		},
		{
			name: "egresses out of sync",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("service1"),
					Egresses: sets.NewString("egress1", "egress2"),
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities: sets.NewString("service1"),
								},
							},
						},
					},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1"),
					NATGateways:   sets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: sets.NewString("service1"),
								},
							},
						},
					},
				},
			},
			expectedSyncStatus:      Success,
			expectedLoadBalancerOps: false,
			expectedNATGatewayOps:   true,
			expectedLocationOps:     false,
		},
		{
			name: "locations out of sync",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("service1", "service2"), // service2 must also exist in K8s to avoid removal
					Egresses: sets.NewString("egress1"),
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities: sets.NewString("service1", "service2"),
								},
							},
						},
					},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1", "service2"), // Both services exist in NRP (pass filtering)
					NATGateways:   sets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: sets.NewString("service1"), // Only service1 in location, service2 needs sync
								},
							},
						},
					},
				},
			},
			expectedSyncStatus:      Success,
			expectedLoadBalancerOps: false, // Both services already in sync between K8s and NRP
			expectedNATGatewayOps:   false,
			expectedLocationOps:     true, // service2 needs to be added to location
		},
		{
			name: "multiple components out of sync",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("service1", "service3"),
					Egresses: sets.NewString("egress1", "egress3"),
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities:      sets.NewString("service1"),
									PublicOutboundIdentity: "public1",
								},
								"10.0.0.3": {
									InboundIdentities: sets.NewString("service3"),
								},
							},
						},
					},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1", "service2"),
					NATGateways:   sets.NewString("egress1", "egress2"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: sets.NewString("service1"),
								},
								"10.0.0.2": {
									Services: sets.NewString("service2"),
								},
							},
						},
					},
				},
			},
			expectedSyncStatus:      Success,
			expectedLoadBalancerOps: true,
			expectedNATGatewayOps:   true,
			expectedLocationOps:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.initialState.GetSyncOperations()

			assert.Equal(t, tt.expectedSyncStatus, result.SyncStatus)

			if tt.expectedSyncStatus == AlreadyInSync {
				return
			}

			if tt.expectedLoadBalancerOps {
				assert.True(t, result.LoadBalancerUpdates.Additions.Len() > 0 || result.LoadBalancerUpdates.Removals.Len() > 0,
					"Expected LoadBalancer operations")
			} else {
				assert.Equal(t, 0, result.LoadBalancerUpdates.Additions.Len())
				assert.Equal(t, 0, result.LoadBalancerUpdates.Removals.Len())
			}

			if tt.expectedNATGatewayOps {
				assert.True(t, result.NATGatewayUpdates.Additions.Len() > 0 || result.NATGatewayUpdates.Removals.Len() > 0,
					"Expected NATGateway operations")
			} else {
				assert.Equal(t, 0, result.NATGatewayUpdates.Additions.Len())
				assert.Equal(t, 0, result.NATGatewayUpdates.Removals.Len())
			}

			if tt.expectedLocationOps {
				hasAddresses := false
				for _, loc := range result.LocationData.Locations {
					if len(loc.Addresses) > 0 {
						hasAddresses = true
						break
					}
				}
				assert.True(t, hasAddresses, "Expected location operations")
			}
		})
	}
}

// Real Scenario: CloudProvider is down and K8s Cluster is subject to continuous updates. While CloudProvider is down,
// NRP is not synced. When CloudProvider is back up, it should be able to track all the changes that happened in K8s
// Cluster and fully sync NRP accordingly. This test verifies if the DiffTracker is able to sync K8s Cluster and NRP
// correctly when there is a huge discrepancy between K8s Cluster and NRP.
func TestGetSyncLocationsAddressesRemovesGoneNode(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{Nodes: map[string]Node{}},
		NRPResources: NRPState{
			LoadBalancers: sets.NewString("service1", "service2"),
			NATGateways:   sets.NewString(),
			Locations: map[string]NRPLocation{
				"node1": {
					Addresses: map[string]NRPAddress{
						"10.0.0.1": {Services: sets.NewString("service1")},
						"10.0.0.2": {Services: sets.NewString("service2")},
					},
				},
			},
		},
	}

	result := dt.GetSyncLocationsAddresses()

	loc, ok := result.Locations["node1"]
	assert.True(t, ok)
	assert.Equal(t, PartialUpdate, loc.AddressUpdateAction)
	assert.Empty(t, loc.Addresses, "gone node must be emitted with an empty Addresses map")

	dt.UpdateLocationsAddresses(result)
	_, ok = dt.NRPResources.Locations["node1"]
	assert.False(t, ok, "gone node's location must be removed locally")
}

// TestLastPodRemoval_EmitsNodeDeletionPayload verifies that when the last pod backing a service on a
// node is removed (updateK8sEndpointsLocked drops the node from K8sResources.Nodes while NRP still
// holds the address), getSyncLocationsAddresses emits a Location with AddressUpdateAction=PartialUpdate
// and an empty, non-nil Addresses map. The ServiceGateway treats an empty Addresses array as "delete
// the whole location/node", which is the intended payload for a fully drained node.
func TestLastPodRemoval_EmitsNodeDeletionPayload(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-last-pod"
	const node = "10.0.0.1"
	const podIP = "10.244.0.5"

	// Service is created and known to NRP. NRP already reflects the single pod on this node.
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateCreated,
	}
	dt.NRPResources.LoadBalancers.Insert(uid)
	dt.NRPResources.Locations[node] = NRPLocation{
		Addresses: map[string]NRPAddress{
			podIP: {Services: utilsets.NewString(uid)},
		},
	}

	// Step 1: simulate the EndpointSlice that originally added the pod, so K8s state has the
	// node entry. We drive it through the production entry point so the node is created by a real
	// informer flow rather than synthetic state stitching.
	errs := dt.UpdateK8sEndpoints(UpdateK8sEndpointsInputType{
		InboundIdentity: uid,
		OldAddresses:    nil,
		NewAddresses:    map[string]string{podIP: node},
	})
	assert.Empty(t, errs, "endpoint-add fixture must apply cleanly")
	// Sanity: the node entry was created (otherwise the path below is moot).
	_, hasNode := dt.K8sResources.Nodes[node]
	assert.True(t, hasNode, "fixture precondition: node must be present after endpoint add")

	// Step 2: the sole pod for this service is removed. With no other identities on the pod
	// and no other pods on the node, updateK8sEndpointsLocked deletes the node from K8s state.
	errs = dt.UpdateK8sEndpoints(UpdateK8sEndpointsInputType{
		InboundIdentity: uid,
		OldAddresses:    map[string]string{podIP: node},
		NewAddresses:    nil,
	})
	assert.Empty(t, errs)
	_, hasNodeAfter := dt.K8sResources.Nodes[node]
	assert.False(t, hasNodeAfter, "last-pod removal must drop the node entry from K8sResources.Nodes")

	// Step 3: observe the sync payload the LocationsUpdater would send.
	result := dt.GetSyncLocationsAddresses()
	loc, ok := result.Locations[node]
	if !assert.True(t, ok, "a location entry must be emitted for the now-gone node") {
		return
	}
	assert.Equal(t, PartialUpdate, loc.AddressUpdateAction,
		"the gone node is emitted with AddressUpdateAction=PartialUpdate")
	assert.NotNil(t, loc.Addresses, "Addresses map must be non-nil (else the JSON would omit the field)")
	assert.Empty(t, loc.Addresses,
		"a fully drained node is emitted with an empty Addresses map, which the ServiceGateway treats "+
			"as deleting the whole location/node")
}
