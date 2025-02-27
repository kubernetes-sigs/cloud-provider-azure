package difftracker

import (
	"reflect"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func TestDiffTrackerState_DeepEqual(t *testing.T) {
	tests := []struct {
		name     string
		dt       *DiffTrackerState
		expected bool
	}{
		{
			name: "equal empty states",
			dt: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString(),
					Egresses: utilsets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString(),
					NATGateways:   utilsets.NewString(),
					NRPLocations:  map[string]NRPLocation{},
				},
			},
			expected: true,
		},
		{
			name: "equal states with services",
			dt: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString("service1", "service2"),
					Egresses: utilsets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString("service1", "service2"),
					NATGateways:   utilsets.NewString(),
					NRPLocations:  map[string]NRPLocation{},
				},
			},
			expected: true,
		},
		{
			name: "services not equal",
			dt: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString("service1", "service2"),
					Egresses: utilsets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString("service1"),
					NATGateways:   utilsets.NewString(),
					NRPLocations:  map[string]NRPLocation{},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.dt.DeepEqual()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestUpdateK8service(t *testing.T) {
	dt := &DiffTrackerState{
		K8s: K8sState{
			Services: utilsets.NewString(),
		},
	}

	// Test ADD operation
	err := dt.UpdateK8service(UpdateK8sResource{
		Operation: ADD,
		ID:        "service1",
	})
	assert.NoError(t, err)
	assert.True(t, dt.K8s.Services.Has("service1"))

	// Test REMOVE operation
	err = dt.UpdateK8service(UpdateK8sResource{
		Operation: REMOVE,
		ID:        "service1",
	})
	assert.NoError(t, err)
	assert.False(t, dt.K8s.Services.Has("service1"))

	// Test invalid operation
	err = dt.UpdateK8service(UpdateK8sResource{
		Operation: UPDATE,
		ID:        "service1",
	})
	assert.Error(t, err)
}

func TestGetSyncLoadBalancerNRPServices(t *testing.T) {
	dt := &DiffTrackerState{
		K8s: K8sState{
			Services: utilsets.NewString("service1", "service2", "service3"),
		},
		NRP: NRPState{
			LoadBalancers: utilsets.NewString("service2", "service3", "service4"),
		},
	}

	result := dt.GetSyncLoadBalancerNRPServices()

	// Check additions (in K8s but not in NRP)
	assert.True(t, result.Additions.Has("service1"))
	assert.Equal(t, 1, result.Additions.Len())

	// Check removals (in NRP but not in K8s)
	assert.True(t, result.Removals.Has("service4"))
	assert.Equal(t, 1, result.Removals.Len())
}

func TestUpdateK8sEndpoints(t *testing.T) {
	dt := &DiffTrackerState{
		K8s: K8sState{
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
	assert.Contains(t, dt.K8s.Nodes, "node1")
	assert.Contains(t, dt.K8s.Nodes["node1"].Pods, "10.0.0.1")
	assert.True(t, dt.K8s.Nodes["node1"].Pods["10.0.0.1"].InboundIdentities.Has("service1"))

	// Test removing an endpoint
	input = UpdateK8sEndpointsInputType{
		InboundIdentity: "service1",
		OldAddresses:    map[string]string{"10.0.0.1": "node1"},
		NewAddresses:    map[string]string{},
	}

	errs = dt.UpdateK8sEndpoints(input)
	assert.Empty(t, errs)

	// Verify the endpoint was removed
	assert.NotContains(t, dt.K8s.Nodes["node1"].Pods, "10.0.0.1")
}

func TestUpdateK8sPod(t *testing.T) {
	dt := &DiffTrackerState{
		K8s: K8sState{
			Nodes: map[string]Node{},
		},
	}

	// Test adding new egress assignment
	input := UpdatePodInputType{
		PodOperation:            ADD,
		PublicOutboundIdentity:  "public1",
		PrivateOutboundIdentity: "private1",
		Location:                "node1",
		Address:                 "10.0.0.1",
	}

	err := dt.UpdateK8sPod(input)
	assert.NoError(t, err)

	// Verify the egress assignment was added
	assert.Contains(t, dt.K8s.Nodes, "node1")
	assert.Contains(t, dt.K8s.Nodes["node1"].Pods, "10.0.0.1")
	assert.Equal(t, "public1", dt.K8s.Nodes["node1"].Pods["10.0.0.1"].PublicOutboundIdentity)
	assert.Equal(t, "private1", dt.K8s.Nodes["node1"].Pods["10.0.0.1"].PrivateOutboundIdentity)

	// Test removing egress assignment
	input = UpdatePodInputType{
		PodOperation: REMOVE,
		Location:     "node1",
		Address:      "10.0.0.1",
	}

	err = dt.UpdateK8sPod(input)
	assert.NoError(t, err)

	// Verify the egress assignment was removed
	assert.NotContains(t, dt.K8s.Nodes["node1"].Pods, "10.0.0.1")
}

func TestGetSyncNRPLocationsAddresses(t *testing.T) {
	// Setup a diff tracker with some pods and services
	dt := &DiffTrackerState{
		K8s: K8sState{
			Nodes: map[string]Node{
				"node1": {
					Pods: map[string]Pod{
						"10.0.0.1": {
							InboundIdentities:      utilsets.NewString("service1"),
							PublicOutboundIdentity: "public1",
						},
					},
				},
			},
		},
		NRP: NRPState{
			NRPLocations: map[string]NRPLocation{},
		},
	}

	// Get sync data
	result := dt.GetSyncNRPLocationsAddresses()

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

func TestOperation_String(t *testing.T) {
	assert.Equal(t, "ADD", ADD.String())
	assert.Equal(t, "REMOVE", REMOVE.String())
	assert.Equal(t, "UPDATE", UPDATE.String())
}
func TestUpdateNRPLoadBalancers(t *testing.T) {
	tests := []struct {
		name         string
		initialState *DiffTrackerState
		expectedNRP  *utilsets.IgnoreCaseSet
	}{
		{
			name: "add services from K8s to NRP",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString("service1", "service2", "service3"),
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString("service1"),
				},
			},
			expectedNRP: utilsets.NewString("service1", "service2", "service3"),
		},
		{
			name: "remove services from NRP that are not in K8s",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString("service1"),
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString("service1", "service2", "service3"),
				},
			},
			expectedNRP: utilsets.NewString("service1"),
		},
		{
			name: "add and remove services to sync K8s and NRP",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString("service1", "service2", "service4"),
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString("service1", "service3", "service5"),
				},
			},
			expectedNRP: utilsets.NewString("service1", "service2", "service4"),
		},
		{
			name: "no changes needed when K8s and NRP are in sync",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString("service1", "service2"),
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString("service1", "service2"),
				},
			},
			expectedNRP: utilsets.NewString("service1", "service2"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Execute the update
			tt.initialState.UpdateNRPLoadBalancers()

			// Verify the NRP state was updated correctly
			assert.True(t, tt.expectedNRP.Equals(tt.initialState.NRP.LoadBalancers),
				"Expected NRP LoadBalancers %v, but got %v",
				tt.expectedNRP.UnsortedList(),
				tt.initialState.NRP.LoadBalancers.UnsortedList())
		})
	}
}
func TestUpdateK8sEgress(t *testing.T) {
	dt := &DiffTrackerState{
		K8s: K8sState{
			Egresses: utilsets.NewString(),
		},
	}

	// Test ADD operation
	err := dt.UpdateK8sEgress(UpdateK8sResource{
		Operation: ADD,
		ID:        "egress1",
	})
	assert.NoError(t, err)
	assert.True(t, dt.K8s.Egresses.Has("egress1"))

	// Test REMOVE operation
	err = dt.UpdateK8sEgress(UpdateK8sResource{
		Operation: REMOVE,
		ID:        "egress1",
	})
	assert.NoError(t, err)
	assert.False(t, dt.K8s.Egresses.Has("egress1"))

	// Test invalid operation
	err = dt.UpdateK8sEgress(UpdateK8sResource{
		Operation: UPDATE,
		ID:        "egress1",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "error UpdateEgress")
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
			// Initialize DiffTrackerState with the test case data
			dt := &DiffTrackerState{
				K8s: K8sState{
					Egresses: utilsets.NewString(tt.k8sEgresses...),
				},
				NRP: NRPState{
					NATGateways: utilsets.NewString(tt.nrpNATGateways...),
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
func TestUpdateNRPNATGateways(t *testing.T) {
	tests := []struct {
		name         string
		initialState *DiffTrackerState
		expectedNRP  *utilsets.IgnoreCaseSet
	}{
		{
			name: "add egresses from K8s to NRP",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Egresses: utilsets.NewString("egress1", "egress2", "egress3"),
				},
				NRP: NRPState{
					NATGateways: utilsets.NewString("egress1"),
				},
			},
			expectedNRP: utilsets.NewString("egress1", "egress2", "egress3"),
		},
		{
			name: "remove egresses from NRP that are not in K8s",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Egresses: utilsets.NewString("egress1"),
				},
				NRP: NRPState{
					NATGateways: utilsets.NewString("egress1", "egress2", "egress3"),
				},
			},
			expectedNRP: utilsets.NewString("egress1"),
		},
		{
			name: "add and remove egresses to sync K8s and NRP",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Egresses: utilsets.NewString("egress1", "egress2", "egress4"),
				},
				NRP: NRPState{
					NATGateways: utilsets.NewString("egress1", "egress3", "egress5"),
				},
			},
			expectedNRP: utilsets.NewString("egress1", "egress2", "egress4"),
		},
		{
			name: "no changes needed when K8s and NRP are in sync",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Egresses: utilsets.NewString("egress1", "egress2"),
				},
				NRP: NRPState{
					NATGateways: utilsets.NewString("egress1", "egress2"),
				},
			},
			expectedNRP: utilsets.NewString("egress1", "egress2"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Execute the update
			tt.initialState.UpdateNRPNATGateways()

			// Verify the NRP state was updated correctly
			assert.True(t, tt.expectedNRP.Equals(tt.initialState.NRP.NATGateways),
				"Expected NRP NATGateways %v, but got %v",
				tt.expectedNRP.UnsortedList(),
				tt.initialState.NRP.NATGateways.UnsortedList())
		})
	}
}
func TestUpdateNRPLocationsAddresses(t *testing.T) {
	tests := []struct {
		name         string
		initialState *DiffTrackerState
		expectedNRP  map[string]map[string][]string // location -> address -> services
	}{
		{
			name: "sync empty states",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Nodes: map[string]Node{},
				},
				NRP: NRPState{
					NRPLocations: map[string]NRPLocation{},
				},
			},
			expectedNRP: map[string]map[string][]string{},
		},
		{
			name: "add new location and address",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities:      utilsets.NewString("service1"),
									PublicOutboundIdentity: "public1",
								},
							},
						},
					},
				},
				NRP: NRPState{
					NRPLocations: map[string]NRPLocation{},
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
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities:       utilsets.NewString("service1", "service2"),
									PublicOutboundIdentity:  "public1",
									PrivateOutboundIdentity: "private1",
								},
							},
						},
					},
				},
				NRP: NRPState{
					NRPLocations: map[string]NRPLocation{
						"node1": {
							NRPAddresses: map[string]NRPAddress{
								"10.0.0.1": {
									NRPServices: utilsets.NewString("service1"),
								},
							},
						},
					},
				},
			},
			expectedNRP: map[string]map[string][]string{
				"node1": {
					"10.0.0.1": {"service1", "service2", "public1", "private1"},
				},
			},
		},
		{
			name: "remove address that no longer exists in K8s",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities: utilsets.NewString("service1"),
								},
							},
						},
					},
				},
				NRP: NRPState{
					NRPLocations: map[string]NRPLocation{
						"node1": {
							NRPAddresses: map[string]NRPAddress{
								"10.0.0.1": {
									NRPServices: utilsets.NewString("service1"),
								},
								"10.0.0.2": {
									NRPServices: utilsets.NewString("service2"),
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
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities: utilsets.NewString("service1"),
								},
							},
						},
					},
				},
				NRP: NRPState{
					NRPLocations: map[string]NRPLocation{
						"node1": {
							NRPAddresses: map[string]NRPAddress{
								"10.0.0.1": {
									NRPServices: utilsets.NewString("service1"),
								},
							},
						},
						"node2": {
							NRPAddresses: map[string]NRPAddress{
								"10.0.0.2": {
									NRPServices: utilsets.NewString("service2"),
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
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities:      utilsets.NewString("service1", "service3"),
									PublicOutboundIdentity: "public1",
								},
							},
						},
						"node3": {
							Pods: map[string]Pod{
								"10.0.0.5": {
									InboundIdentities:       utilsets.NewString("service5"),
									PrivateOutboundIdentity: "private5",
								},
							},
						},
					},
				},
				NRP: NRPState{
					NRPLocations: map[string]NRPLocation{
						"node1": {
							NRPAddresses: map[string]NRPAddress{
								"10.0.0.1": {
									NRPServices: utilsets.NewString("service1", "service2"),
								},
								"10.0.0.2": {
									NRPServices: utilsets.NewString("service4"),
								},
							},
						},
						"node2": {
							NRPAddresses: map[string]NRPAddress{
								"10.0.0.3": {
									NRPServices: utilsets.NewString("service3"),
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
					"10.0.0.5": {"service5", "private5"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Execute the update
			tt.initialState.UpdateNRPLocationsAddresses()

			// Verify the NRP state was updated correctly
			// First check if the number of locations matches
			assert.Equal(t, len(tt.expectedNRP), len(tt.initialState.NRP.NRPLocations),
				"Expected %d locations, got %d", len(tt.expectedNRP), len(tt.initialState.NRP.NRPLocations))

			// Then check each location and its addresses
			for locName, expectedAddressMap := range tt.expectedNRP {
				nrpLoc, exists := tt.initialState.NRP.NRPLocations[locName]
				assert.True(t, exists, "Expected location %s not found in NRP", locName)

				// Check number of addresses
				assert.Equal(t, len(expectedAddressMap), len(nrpLoc.NRPAddresses),
					"Expected %d addresses in location %s, got %d", len(expectedAddressMap), locName, len(nrpLoc.NRPAddresses))

				// Check each address and its services
				for addr, expectedServices := range expectedAddressMap {
					nrpAddr, exists := nrpLoc.NRPAddresses[addr]
					assert.True(t, exists, "Expected address %s not found in location %s", addr, locName)

					// Check if all expected services exist
					assert.Equal(t, len(expectedServices), nrpAddr.NRPServices.Len(),
						"Expected %d services for address %s in location %s, got %d",
						len(expectedServices), addr, locName, nrpAddr.NRPServices.Len())

					for _, service := range expectedServices {
						assert.True(t, nrpAddr.NRPServices.Has(service),
							"Expected service %s not found for address %s in location %s", service, addr, locName)
					}
				}
			}

			// Check that there are no additional locations in NRP
			for locName := range tt.initialState.NRP.NRPLocations {
				_, exists := tt.expectedNRP[locName]
				assert.True(t, exists, "Unexpected location %s found in NRP", locName)
			}
		})
	}
}

func TestHandleService(t *testing.T) {
	dt := createTestDiffTrackerState()

	// Test handleService
	serviceResult := dt.handleService(UpdateK8sResource{
		Operation: ADD,
		ID:        "newService",
	})
	assert.True(t, serviceResult.Additions.Has("newService"))
	assert.Equal(t, 1, serviceResult.Additions.Len())
	assert.Equal(t, 0, serviceResult.Removals.Len())

	dt.UpdateNRPLoadBalancers()
	// Test removal operation
	serviceResult = dt.handleService(UpdateK8sResource{
		Operation: REMOVE,
		ID:        "newService",
	})
	assert.True(t, serviceResult.Removals.Has("newService"))
	assert.Equal(t, 0, serviceResult.Additions.Len())
	assert.Equal(t, 1, serviceResult.Removals.Len())
}

func TestHandleEgress(t *testing.T) {
	dt := createTestDiffTrackerState()

	// Test handleEgress with ADD operation
	egressResult := dt.handleEgress(UpdateK8sResource{
		Operation: ADD,
		ID:        "newEgress",
	})
	assert.True(t, egressResult.Additions.Has("newEgress"))
	assert.Equal(t, 1, egressResult.Additions.Len())
	assert.Equal(t, 0, egressResult.Removals.Len())

	dt.UpdateNRPNATGateways()
	// Test with REMOVE operation
	egressResult = dt.handleEgress(UpdateK8sResource{
		Operation: REMOVE,
		ID:        "newEgress",
	})
	assert.True(t, egressResult.Removals.Has("newEgress"))
	assert.Equal(t, 0, egressResult.Additions.Len())
	assert.Equal(t, 1, egressResult.Removals.Len())
}

func TestHandleEndpoints(t *testing.T) {
	dt := createTestDiffTrackerState()

	// Test adding new endpoint
	endpointsResult := dt.handleEndpoints(UpdateK8sEndpointsInputType{
		InboundIdentity: "newService",
		OldAddresses:    map[string]string{},
		NewAddresses:    map[string]string{"10.0.0.1": "node1", "10.0.0.2": "node1"},
	})
	assert.Equal(t, 1, len(endpointsResult.Locations))
	loc, exists := endpointsResult.Locations["node1"]
	assert.True(t, exists)
	assert.Equal(t, PartialUpdate, endpointsResult.Locations["node1"].AddressUpdateAction)
	assert.Equal(t, 2, len(loc.Addresses))
	assert.True(t, loc.Addresses["10.0.0.2"].ServiceRef.Has("newService"))
	assert.True(t, loc.Addresses["10.0.0.1"].ServiceRef.Has("newService"))

	// Sync NRP state
	dt.UpdateNRPLocationsAddresses()

	// Test updating existing endpoint
	endpointsResult = dt.handleEndpoints(UpdateK8sEndpointsInputType{
		InboundIdentity: "existingService",
		OldAddresses:    map[string]string{"10.0.0.1": "node1"},
		NewAddresses:    map[string]string{"10.0.0.1": "node1", "10.0.0.3": "node1"},
	})
	assert.Equal(t, 1, len(endpointsResult.Locations))
	loc, exists = endpointsResult.Locations["node1"]
	assert.True(t, exists)
	assert.Equal(t, 1, len(loc.Addresses))
	assert.True(t, loc.Addresses["10.0.0.3"].ServiceRef.Has("existingService"))
}

func TestHandleEgressAssignment(t *testing.T) {
	dt := createTestDiffTrackerState()

	// Test adding egress assignment
	egressAssignmentResult := dt.handleEgressAssignment(UpdatePodInputType{
		PodOperation:            ADD,
		PublicOutboundIdentity:  "existingPublicOutboundIdentity",
		PrivateOutboundIdentity: "newPrivateOutboundIdentity",
		Location:                "node1",
		Address:                 "10.0.0.1",
	})
	assert.Equal(t, 1, len(egressAssignmentResult.Locations))
	locationInfo, exists := egressAssignmentResult.Locations["node1"]
	assert.True(t, exists)
	assert.Equal(t, 1, len(locationInfo.Addresses))
	address, exists := locationInfo.Addresses["10.0.0.1"]
	assert.True(t, exists)
	assert.True(t, address.ServiceRef.Has("existingService"))
	assert.True(t, address.ServiceRef.Has("existingPublicOutboundIdentity"))
	assert.True(t, address.ServiceRef.Has("newPrivateOutboundIdentity"))

	// Sync NRP state
	dt.UpdateNRPLocationsAddresses()

	// Test removing egress assignment
	egressAssignmentResult = dt.handleEgressAssignment(UpdatePodInputType{
		PodOperation:            UPDATE,
		PublicOutboundIdentity:  "",
		PrivateOutboundIdentity: "newPrivateOutboundIdentity",
		Location:                "node1",
		Address:                 "10.0.0.1",
	})
	assert.Equal(t, 1, len(egressAssignmentResult.Locations))
	locationInfo = egressAssignmentResult.Locations["node1"]
	assert.NotNil(t, locationInfo)
	assert.True(t, locationInfo.Addresses["10.0.0.1"].ServiceRef.Has("newPrivateOutboundIdentity"))
	assert.True(t, locationInfo.Addresses["10.0.0.1"].ServiceRef.Has("existingService"))
	assert.False(t, locationInfo.Addresses["10.0.0.1"].ServiceRef.Has("existingPublicOutboundIdentity"))
}

func createTestDiffTrackerState() *DiffTrackerState {
	return &DiffTrackerState{
		K8s: K8sState{
			Services: utilsets.NewString(),
			Egresses: utilsets.NewString(),
			Nodes: map[string]Node{
				"node1": {
					Pods: map[string]Pod{
						"10.0.0.1": {
							InboundIdentities:      utilsets.NewString("existingService"),
							PublicOutboundIdentity: "existingPublicOutboundIdentity",
						},
					},
				},
			},
		},
		NRP: NRPState{
			LoadBalancers: utilsets.NewString(),
			NATGateways:   utilsets.NewString(),
			NRPLocations: map[string]NRPLocation{
				"node1": {
					NRPAddresses: map[string]NRPAddress{
						"10.0.0.1": {
							NRPServices: utilsets.NewString("existingService", "existingPublicOutboundIdentity"),
						},
					},
				},
			},
		},
	}
}

func TestGetSyncDiffTrackerState(t *testing.T) {
	tests := []struct {
		name                    string
		initialState            *DiffTrackerState
		expectedSyncStatus      SyncStatus
		expectedLoadBalancerOps bool
		expectedNATGatewayOps   bool
		expectedLocationOps     bool
	}{
		{
			name: "states already in sync",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString("service1"),
					Egresses: utilsets.NewString("egress1"),
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities: utilsets.NewString("service1"),
								},
							},
						},
					},
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString("service1"),
					NATGateways:   utilsets.NewString("egress1"),
					NRPLocations: map[string]NRPLocation{
						"node1": {
							NRPAddresses: map[string]NRPAddress{
								"10.0.0.1": {
									NRPServices: utilsets.NewString("service1"),
								},
							},
						},
					},
				},
			},
			expectedSyncStatus:      ALREADY_IN_SYNC,
			expectedLoadBalancerOps: false,
			expectedNATGatewayOps:   false,
			expectedLocationOps:     false,
		},
		{
			name: "services out of sync",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString("service1", "service2"),
					Egresses: utilsets.NewString("egress1"),
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities: utilsets.NewString("service1"),
								},
							},
						},
					},
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString("service1"),
					NATGateways:   utilsets.NewString("egress1"),
					NRPLocations: map[string]NRPLocation{
						"node1": {
							NRPAddresses: map[string]NRPAddress{
								"10.0.0.1": {
									NRPServices: utilsets.NewString("service1"),
								},
							},
						},
					},
				},
			},
			expectedSyncStatus:      SUCCESS,
			expectedLoadBalancerOps: true,
			expectedNATGatewayOps:   false,
			expectedLocationOps:     false,
		},
		{
			name: "egresses out of sync",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString("service1"),
					Egresses: utilsets.NewString("egress1", "egress2"),
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities: utilsets.NewString("service1"),
								},
							},
						},
					},
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString("service1"),
					NATGateways:   utilsets.NewString("egress1"),
					NRPLocations: map[string]NRPLocation{
						"node1": {
							NRPAddresses: map[string]NRPAddress{
								"10.0.0.1": {
									NRPServices: utilsets.NewString("service1"),
								},
							},
						},
					},
				},
			},
			expectedSyncStatus:      SUCCESS,
			expectedLoadBalancerOps: false,
			expectedNATGatewayOps:   true,
			expectedLocationOps:     false,
		},
		{
			name: "locations out of sync",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString("service1"),
					Egresses: utilsets.NewString("egress1"),
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities: utilsets.NewString("service1", "service2"),
								},
							},
						},
					},
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString("service1"),
					NATGateways:   utilsets.NewString("egress1"),
					NRPLocations: map[string]NRPLocation{
						"node1": {
							NRPAddresses: map[string]NRPAddress{
								"10.0.0.1": {
									NRPServices: utilsets.NewString("service1"),
								},
							},
						},
					},
				},
			},
			expectedSyncStatus:      SUCCESS,
			expectedLoadBalancerOps: false,
			expectedNATGatewayOps:   false,
			expectedLocationOps:     true,
		},
		{
			name: "multiple components out of sync",
			initialState: &DiffTrackerState{
				K8s: K8sState{
					Services: utilsets.NewString("service1", "service3"),
					Egresses: utilsets.NewString("egress1", "egress3"),
					Nodes: map[string]Node{
						"node1": {
							Pods: map[string]Pod{
								"10.0.0.1": {
									InboundIdentities:      utilsets.NewString("service1"),
									PublicOutboundIdentity: "public1",
								},
								"10.0.0.3": {
									InboundIdentities: utilsets.NewString("service3"),
								},
							},
						},
					},
				},
				NRP: NRPState{
					LoadBalancers: utilsets.NewString("service1", "service2"),
					NATGateways:   utilsets.NewString("egress1", "egress2"),
					NRPLocations: map[string]NRPLocation{
						"node1": {
							NRPAddresses: map[string]NRPAddress{
								"10.0.0.1": {
									NRPServices: utilsets.NewString("service1"),
								},
								"10.0.0.2": {
									NRPServices: utilsets.NewString("service2"),
								},
							},
						},
					},
				},
			},
			expectedSyncStatus:      SUCCESS,
			expectedLoadBalancerOps: true,
			expectedNATGatewayOps:   true,
			expectedLocationOps:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.initialState.GetSyncDiffTrackerState()

			assert.Equal(t, tt.expectedSyncStatus, result.SyncStatus)

			if tt.expectedSyncStatus == ALREADY_IN_SYNC {
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
func TestInitializeDiffTrackerState(t *testing.T) {
	k8sState := K8sState{
		Services: utilsets.NewString("Service0", "Service1", "Service2"),
		Egresses: utilsets.NewString("Egress0", "Egress1", "Egress2"),
		Nodes: map[string]Node{
			"Node1": {
				Pods: map[string]Pod{
					"Pod34": {
						InboundIdentities:       utilsets.NewString("Service0"),
						PublicOutboundIdentity:  "",
						PrivateOutboundIdentity: "",
					},
					"Pod0": {
						InboundIdentities:       utilsets.NewString("Service0"),
						PublicOutboundIdentity:  "Egress0",
						PrivateOutboundIdentity: "",
					},
					"Pod1": {
						InboundIdentities:       utilsets.NewString("Service1", "Service2"),
						PublicOutboundIdentity:  "Egress1",
						PrivateOutboundIdentity: "",
					},
					"Pod3": {
						InboundIdentities:       utilsets.NewString(),
						PublicOutboundIdentity:  "Egress2",
						PrivateOutboundIdentity: "",
					},
				},
			},
			"Node2": {
				Pods: map[string]Pod{
					"Pod2": {
						InboundIdentities:       utilsets.NewString("Service1"),
						PublicOutboundIdentity:  "Egress2",
						PrivateOutboundIdentity: "",
					},
				},
			},
		},
	}

	nrpState := NRPState{
		LoadBalancers: utilsets.NewString("Service0", "Service6", "Service5"),
		NATGateways:   utilsets.NewString("Egress0", "Egress6", "Egress5"),
		NRPLocations: map[string]NRPLocation{
			"Node1": {
				NRPAddresses: map[string]NRPAddress{
					"Pod34": {
						NRPServices: utilsets.NewString("Service0", "Service5"),
					},
					"Pod00": {
						NRPServices: utilsets.NewString("Service6", "Egress5"),
					},
					"Pod0": {
						NRPServices: utilsets.NewString("Service0", "Egress0"),
					},
				},
			},
			"Node3": {
				NRPAddresses: map[string]NRPAddress{
					"Pod4": {
						NRPServices: utilsets.NewString("Service6", "Eggres6"),
					},
					"Pod5": {
						NRPServices: utilsets.NewString("Egress5"),
					},
				},
			},
		},
	}

	expectedSyncOperations := &SyncDiffTrackerStateReturnType{
		SyncStatus: SUCCESS,
		LoadBalancerUpdates: SyncNRPServicesReturnType{
			Additions: utilsets.NewString("Service1", "Service2"),
			Removals:  utilsets.NewString("Service6", "Service5"),
		},
		NATGatewayUpdates: SyncNRPServicesReturnType{
			Additions: utilsets.NewString("Egress1", "Egress2"),
			Removals:  utilsets.NewString("Egress6", "Egress5"),
		},
		LocationData: LocationData{
			Action: PartialUpdate,
			Locations: map[string]Location{
				"Node1": {
					AddressUpdateAction: PartialUpdate,
					Addresses: map[string]Address{
						"Pod00": {
							ServiceRef: utilsets.NewString(),
						},
						"Pod34": {
							ServiceRef: utilsets.NewString("Service0"),
						},
						"Pod1": {
							ServiceRef: utilsets.NewString("Service1", "Service2", "Egress1"),
						},
						"Pod3": {
							ServiceRef: utilsets.NewString("Egress2"),
						},
					},
				},
				"Node2": {
					AddressUpdateAction: FullUpdate,
					Addresses: map[string]Address{
						"Pod2": {
							ServiceRef: utilsets.NewString("Service1", "Egress2"),
						},
					},
				},
				"Node3": {
					AddressUpdateAction: PartialUpdate,
					Addresses:           map[string]Address{},
				},
			},
		},
	}

	diffTrackerState, syncOperations := InitializeDiffTrackerState(k8sState, nrpState)

	// It follows a call to ServiceGateway API and if it is successful we can proceed with syncing difftracker.NRP
	diffTrackerState.UpdateNRPLoadBalancers()
	diffTrackerState.UpdateNRPNATGateways()
	diffTrackerState.UpdateNRPLocationsAddresses()

	assert.True(t, syncOperations.Equals(expectedSyncOperations),
		"Sync operations do not match expected values")

	// Check if the DiffTrackerState is updated correctly
	expectedDiffTrackerState := &DiffTrackerState{
		K8s: k8sState,
		NRP: NRPState{
			LoadBalancers: utilsets.NewString("Service0", "Service1", "Service2"),
			NATGateways:   utilsets.NewString("Egress0", "Egress1", "Egress2"),
			NRPLocations: map[string]NRPLocation{
				"Node1": {
					NRPAddresses: map[string]NRPAddress{
						"Pod34": {
							NRPServices: utilsets.NewString("Service0"),
						},
						"Pod0": {
							NRPServices: utilsets.NewString("Service0", "Egress0"),
						},
						"Pod1": {
							NRPServices: utilsets.NewString("Service1", "Service2", "Egress1"),
						},
						"Pod3": {
							NRPServices: utilsets.NewString("Egress2"),
						},
					},
				},
				"Node2": {
					NRPAddresses: map[string]NRPAddress{
						"Pod2": {
							NRPServices: utilsets.NewString("Service1", "Egress2"),
						},
					},
				},
			},
		},
	}

	assert.True(t, diffTrackerState.Equals(expectedDiffTrackerState),
		"DiffTrackerState does not match expected state")
}

func TestMapLocationDataToDTO(t *testing.T) {
	tests := []struct {
		name         string
		locationData LocationData
		expected     LocationDataDTO
	}{
		{
			name: "Empty location data",
			locationData: LocationData{
				Action:    PartialUpdate,
				Locations: map[string]Location{},
			},
			expected: LocationDataDTO{
				Action:    PartialUpdate,
				Locations: []LocationDTO{},
			},
		},
		{
			name: "Single location with no addresses",
			locationData: LocationData{
				Action: PartialUpdate,
				Locations: map[string]Location{
					"location1": {
						AddressUpdateAction: FullUpdate,
						Addresses:           map[string]Address{},
					},
				},
			},
			expected: LocationDataDTO{
				Action: PartialUpdate,
				Locations: []LocationDTO{
					{
						Location:            "location1",
						AddressUpdateAction: FullUpdate,
						Addresses:           []AddressDTO{},
					},
				},
			},
		},
		{
			name: "Multiple locations",
			locationData: LocationData{
				Action: PartialUpdate,
				Locations: map[string]Location{
					"location0": {
						AddressUpdateAction: PartialUpdate,
						Addresses:           map[string]Address{},
					},
					"location1": {
						AddressUpdateAction: PartialUpdate,
						Addresses: map[string]Address{
							"addr1": {
								ServiceRef: utilsets.NewString("service1", "service2"),
							},
							"addr2": {
								ServiceRef: utilsets.NewString("service3"),
							},
						},
					},
					"location2": {
						AddressUpdateAction: FullUpdate,
						Addresses: map[string]Address{
							"addr3": {
								ServiceRef: utilsets.NewString("service4"),
							},
						},
					},
				},
			},
			expected: LocationDataDTO{
				Action: PartialUpdate,
				Locations: []LocationDTO{
					{
						Location:            "location0",
						AddressUpdateAction: PartialUpdate,
						Addresses:           []AddressDTO{},
					},
					{
						Location:            "location1",
						AddressUpdateAction: PartialUpdate,
						Addresses: []AddressDTO{
							{
								Address:      "addr1",
								ServiceNames: utilsets.NewString("service1", "service2"),
							},
							{
								Address:      "addr2",
								ServiceNames: utilsets.NewString("service3"),
							},
						},
					},
					{
						Location:            "location2",
						AddressUpdateAction: FullUpdate,
						Addresses: []AddressDTO{
							{
								Address:      "addr3",
								ServiceNames: utilsets.NewString("service4"),
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mapLocationDataToDTO(tt.locationData)

			// Sort locations for deterministic comparison
			sort.Slice(result.Locations, func(i, j int) bool {
				return result.Locations[i].Location < result.Locations[j].Location
			})

			// Sort expected locations for deterministic comparison
			sort.Slice(tt.expected.Locations, func(i, j int) bool {
				return tt.expected.Locations[i].Location < tt.expected.Locations[j].Location
			})

			// For each location, sort addresses for deterministic comparison
			for i := range result.Locations {
				sort.Slice(result.Locations[i].Addresses, func(j, k int) bool {
					return result.Locations[i].Addresses[j].Address < result.Locations[i].Addresses[k].Address
				})
			}

			for i := range tt.expected.Locations {
				sort.Slice(tt.expected.Locations[i].Addresses, func(j, k int) bool {
					return tt.expected.Locations[i].Addresses[j].Address < tt.expected.Locations[i].Addresses[k].Address
				})
			}

			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("mapLocationDataToDTO() = %v, want %v", result, tt.expected)
			}
		})
	}
}
