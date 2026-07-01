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

	assert.True(t, result.Additions.Has("service1"))
	assert.Equal(t, 1, result.Additions.Len())

	assert.True(t, result.Removals.Has("service4"))
	assert.Equal(t, 1, result.Removals.Len())
}
func TestGetSyncLocationsAddresses(t *testing.T) {
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
			LoadBalancers: sets.NewString("service1"),
			NATGateways:   sets.NewString("public1"),
			Locations:     map[string]NRPLocation{},
		},
	}

	result := dt.GetSyncLocationsAddresses()

	assert.Equal(t, PartialUpdate, result.Action)
	assert.Len(t, result.Locations, 1)

	location := result.Locations["node1"]
	assert.NotNil(t, location)
	assert.Equal(t, FullUpdate, location.AddressUpdateAction)
	assert.Len(t, location.Addresses, 1)

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
			name:              "mixed state with additions and removals",
			k8sEgresses:       []string{"egress1", "egress3", "egress5"},
			nrpNATGateways:    []string{"egress1", "egress2", "egress4"},
			expectedAdditions: []string{"egress3", "egress5"},
			expectedRemovals:  []string{"egress2", "egress4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dt := &DiffTracker{
				K8sResources: K8sState{
					Egresses: sets.NewString(tt.k8sEgresses...),
				},
				NRPResources: NRPState{
					NATGateways: sets.NewString(tt.nrpNATGateways...),
				},
			}

			result := dt.GetSyncNRPNATGateways()

			assert.Equal(t, len(tt.expectedAdditions), result.Additions.Len())
			for _, addition := range tt.expectedAdditions {
				assert.True(t, result.Additions.Has(addition))
			}

			assert.Equal(t, len(tt.expectedRemovals), result.Removals.Len())
			for _, removal := range tt.expectedRemovals {
				assert.True(t, result.Removals.Has(removal))
			}
		})
	}
}
func TestGetSyncOperations(t *testing.T) {
	tests := []struct {
		name               string
		initialState       *DiffTracker
		expectedSyncStatus SyncStatus
	}{
		{
			name: "states already in sync",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("service1"),
					Egresses: sets.NewString("egress1"),
					Nodes: map[string]Node{
						"node1": {Pods: map[string]Pod{
							"10.0.0.1": {InboundIdentities: sets.NewString("service1")},
						}},
					},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1"),
					NATGateways:   sets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {Addresses: map[string]NRPAddress{
							"10.0.0.1": {Services: sets.NewString("service1")},
						}},
					},
				},
			},
			expectedSyncStatus: AlreadyInSync,
		},
		{
			name: "services out of sync",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("service1", "service2"),
					Egresses: sets.NewString("egress1"),
					Nodes: map[string]Node{
						"node1": {Pods: map[string]Pod{
							"10.0.0.1": {InboundIdentities: sets.NewString("service1")},
						}},
					},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1"),
					NATGateways:   sets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {Addresses: map[string]NRPAddress{
							"10.0.0.1": {Services: sets.NewString("service1")},
						}},
					},
				},
			},
			expectedSyncStatus: Success,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.initialState.GetSyncOperations()
			assert.Equal(t, tt.expectedSyncStatus, result.SyncStatus)
		})
	}
}

// Real Scenario: CloudProvider is down and K8s Cluster is subject to continuous updates.
// This test verifies if the DiffTracker is able to sync K8s Cluster and NRP correctly
// when there is a huge discrepancy between K8s Cluster and NRP.
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
	assert.Empty(t, loc.Addresses)

	dt.UpdateLocationsAddresses(result)
	_, ok = dt.NRPResources.Locations["node1"]
	assert.False(t, ok, "gone node's location must be removed locally")
}
