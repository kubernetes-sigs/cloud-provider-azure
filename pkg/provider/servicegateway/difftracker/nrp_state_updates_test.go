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

func TestUpdateNRPLoadBalancers(t *testing.T) {
	tests := []struct {
		name         string
		initialState *DiffTracker
		expectedNRP  *sets.IgnoreCaseSet
	}{
		{
			name: "add services from K8s to NRP",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("service1", "service2", "service3"),
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1"),
				},
			},
			expectedNRP: sets.NewString("service1", "service2", "service3"),
		},
		{
			name: "remove services from NRP that are not in K8s",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("service1"),
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1", "service2", "service3"),
				},
			},
			expectedNRP: sets.NewString("service1"),
		},
		{
			name: "add and remove services to sync K8s and NRP",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("service1", "service2", "service4"),
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1", "service3", "service5"),
				},
			},
			expectedNRP: sets.NewString("service1", "service2", "service4"),
		},
		{
			name: "no changes needed when K8s and NRP are in sync",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("service1", "service2"),
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1", "service2"),
				},
			},
			expectedNRP: sets.NewString("service1", "service2"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syncServices := tt.initialState.GetSyncLoadBalancerServices()
			// Execute the update
			tt.initialState.UpdateNRPLoadBalancers(syncServices)

			// Verify the NRP state was updated correctly
			assert.True(t, tt.expectedNRP.Equals(tt.initialState.NRPResources.LoadBalancers),
				"Expected NRP LoadBalancers %v, but got %v",
				tt.expectedNRP.UnsortedList(),
				tt.initialState.NRPResources.LoadBalancers.UnsortedList())
		})
	}
}
func TestUpdateNRPNATGateways(t *testing.T) {
	tests := []struct {
		name         string
		initialState *DiffTracker
		expectedNRP  *sets.IgnoreCaseSet
	}{
		{
			name: "add egresses from K8s to NRP",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Egresses: sets.NewString("egress1", "egress2", "egress3"),
				},
				NRPResources: NRPState{
					NATGateways: sets.NewString("egress1"),
				},
			},
			expectedNRP: sets.NewString("egress1", "egress2", "egress3"),
		},
		{
			name: "remove egresses from NRP that are not in K8s",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Egresses: sets.NewString("egress1"),
				},
				NRPResources: NRPState{
					NATGateways: sets.NewString("egress1", "egress2", "egress3"),
				},
			},
			expectedNRP: sets.NewString("egress1"),
		},
		{
			name: "add and remove egresses to sync K8s and NRP",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Egresses: sets.NewString("egress1", "egress2", "egress4"),
				},
				NRPResources: NRPState{
					NATGateways: sets.NewString("egress1", "egress3", "egress5"),
				},
			},
			expectedNRP: sets.NewString("egress1", "egress2", "egress4"),
		},
		{
			name: "no changes needed when K8s and NRP are in sync",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Egresses: sets.NewString("egress1", "egress2"),
				},
				NRPResources: NRPState{
					NATGateways: sets.NewString("egress1", "egress2"),
				},
			},
			expectedNRP: sets.NewString("egress1", "egress2"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syncServices := tt.initialState.GetSyncNRPNATGateways()
			// Execute the update
			tt.initialState.UpdateNRPNATGateways(syncServices)

			// Verify the NRP state was updated correctly
			assert.True(t, tt.expectedNRP.Equals(tt.initialState.NRPResources.NATGateways),
				"Expected NRP NATGateways %v, but got %v",
				tt.expectedNRP.UnsortedList(),
				tt.initialState.NRPResources.NATGateways.UnsortedList())
		})
	}
}
func TestUpdateLocationsAddresses(t *testing.T) {
	tests := []struct {
		name         string
		initialState *DiffTracker
		expectedNRP  map[string]map[string][]string // location -> address -> services
	}{
		{
			name: "sync empty states",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Nodes: map[string]Node{},
				},
				NRPResources: NRPState{
					Locations: map[string]NRPLocation{},
				},
			},
			expectedNRP: map[string]map[string][]string{},
		},
		{
			name: "add new location and address",
			initialState: &DiffTracker{
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
			},
			expectedNRP: map[string]map[string][]string{
				"node1": {
					"10.0.0.1": {"service1", "public1"},
				},
			},
		},
		{
			name: "update existing address with new identity",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities:      sets.NewString("service1", "service2"),
									PublicOutboundIdentity: "public1",
								},
							},
						},
					},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("service1", "service2"), // Services must exist in NRP to pass filtering
					NATGateways:   sets.NewString("public1"),              // NAT Gateway must exist in NRP to pass filtering
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
			expectedNRP: map[string]map[string][]string{
				"node1": {
					"10.0.0.1": {"service1", "service2", "public1"},
				},
			},
		},
		{
			name: "remove address that no longer exists in K8s",
			initialState: &DiffTracker{
				K8sResources: K8sState{
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
					LoadBalancers: sets.NewString("service1", "service2"), // Services must exist in NRP to pass filtering
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
			expectedNRP: map[string]map[string][]string{
				"node1": {
					"10.0.0.1": {"service1"},
				},
			},
		},
		{
			name: "remove location that no longer exists in K8s",
			initialState: &DiffTracker{
				K8sResources: K8sState{
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
					LoadBalancers: sets.NewString("service1", "service2"), // Services must exist in NRP to pass filtering
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: sets.NewString("service1"),
								},
							},
						},
						"node2": {
							Addresses: map[string]NRPAddress{
								"10.0.0.2": {
									Services: sets.NewString("service2"),
								},
							},
						},
					},
				},
			},
			expectedNRP: map[string]map[string][]string{
				"node1": {
					"10.0.0.1": {"service1"},
				},
			},
		},
		{
			name: "complex case with multiple operations",
			initialState: &DiffTracker{
				K8sResources: K8sState{
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities:      sets.NewString("service1", "service3"),
									PublicOutboundIdentity: "public1",
								},
							},
						},
						"node3": {
							Pods: map[string]Pod{
								"10.0.0.5": {
									InboundIdentities: sets.NewString("service5"),
								},
							},
						},
					},
				},
				NRPResources: NRPState{
					// Services must exist in NRP to pass filtering
					LoadBalancers: sets.NewString("service1", "service2", "service3", "service4", "service5"),
					NATGateways:   sets.NewString("public1"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: sets.NewString("service1", "service2"),
								},
								"10.0.0.2": {
									Services: sets.NewString("service4"),
								},
							},
						},
						"node2": {
							Addresses: map[string]NRPAddress{
								"10.0.0.3": {
									Services: sets.NewString("service3"),
								},
							},
						},
					},
				},
			},
			expectedNRP: map[string]map[string][]string{
				"node1": {
					"10.0.0.1": {"service1", "service3", "public1"},
				},
				"node3": {
					"10.0.0.5": {"service5"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Get necessary sync operations
			locationData := tt.initialState.GetSyncLocationsAddresses()

			// Execute the update
			tt.initialState.UpdateLocationsAddresses(locationData)

			// Verify the NRP state was updated correctly
			// First check if the number of locations matches
			assert.Equal(t, len(tt.expectedNRP), len(tt.initialState.NRPResources.Locations),
				"Expected %d locations, got %d", len(tt.expectedNRP), len(tt.initialState.NRPResources.Locations))

			// Then check each location and its addresses
			for locName, expectedAddressMap := range tt.expectedNRP {
				nrpLoc, exists := tt.initialState.NRPResources.Locations[locName]
				assert.True(t, exists, "Expected location %s not found in NRP", locName)

				// Check number of addresses
				assert.Equal(t, len(expectedAddressMap), len(nrpLoc.Addresses),
					"Expected %d addresses in location %s, got %d", len(expectedAddressMap), locName, len(nrpLoc.Addresses))

				// Check each address and its services
				for addr, expectedServices := range expectedAddressMap {
					nrpAddr, exists := nrpLoc.Addresses[addr]
					assert.True(t, exists, "Expected address %s not found in location %s", addr, locName)

					// Check if all expected services exist
					assert.Equal(t, len(expectedServices), nrpAddr.Services.Len(),
						"Expected %d services for address %s in location %s, got %d",
						len(expectedServices), addr, locName, nrpAddr.Services.Len())

					for _, service := range expectedServices {
						assert.True(t, nrpAddr.Services.Has(service),
							"Expected service %s not found for address %s in location %s", service, addr, locName)
					}
				}
			}

			// Check that there are no additional locations in NRP
			for locName := range tt.initialState.NRPResources.Locations {
				_, exists := tt.expectedNRP[locName]
				assert.True(t, exists, "Unexpected location %s found in NRP", locName)
			}
		})
	}
}
