package difftracker

import (
	"reflect"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

var (
	subscriptionID    = "test-subscription-id"
	resourceGroupName = "test-resource-group-name"
)

func TestDiffTracker_DeepEqual(t *testing.T) {
	tests := []struct {
		name     string
		dt       *DiffTracker
		expected bool
	}{
		{
			name: "equal empty states",
			dt: &DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString(),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString(),
					NATGateways:   sets.NewString(),
					Locations:     map[string]NRPLocation{},
				},
			},
			expected: true,
		},
		{
			name: "equal states with services",
			dt: &DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("service1", "service2"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString("service1", "service2"),
					NATGateways:   sets.NewString(),
					Locations:     map[string]NRPLocation{},
				},
			},
			expected: true,
		},
		{
			name: "services not equal",
			dt: &DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("service1", "service2"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString("service1"),
					NATGateways:   sets.NewString(),
					Locations:     map[string]NRPLocation{},
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

func TestUpdateK8sService(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8s_State{
			Services: sets.NewString(),
		},
	}

	// Test ADD operation
	err := dt.UpdateK8sService(UpdateK8sResource{
		Operation: ADD,
		ID:        "service1",
	})
	assert.NoError(t, err)
	assert.True(t, dt.K8sResources.Services.Has("service1"))

	// Test REMOVE operation
	err = dt.UpdateK8sService(UpdateK8sResource{
		Operation: REMOVE,
		ID:        "service1",
	})
	assert.NoError(t, err)
	assert.False(t, dt.K8sResources.Services.Has("service1"))

	// Test invalid operation
	err = dt.UpdateK8sService(UpdateK8sResource{
		Operation: UPDATE,
		ID:        "service1",
	})
	assert.Error(t, err)
}

func TestGetSyncLoadBalancerServices(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8s_State{
			Services: sets.NewString("service1", "service2", "service3"),
		},
		NRPResources: NRP_State{
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

func TestUpdateK8sEndpoints(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8s_State{
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
		K8sResources: K8s_State{
			Nodes: map[string]Node{},
		},
	}

	// Test adding new egress assignment
	input := UpdatePodInputType{
		PodOperation:           ADD,
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
		PodOperation: REMOVE,
		Location:     "node1",
		Address:      "10.0.0.1",
	}

	err = dt.UpdateK8sPod(input)
	assert.NoError(t, err)

	// Verify the egress assignment was removed
	assert.NotContains(t, dt.K8sResources.Nodes["node1"].Pods, "10.0.0.1")
}

func TestGetSyncLocationsAddresses(t *testing.T) {
	// Setup a diff tracker with some pods and services
	dt := &DiffTracker{
		K8sResources: K8s_State{
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
		NRPResources: NRP_State{
			Locations: map[string]NRPLocation{},
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

func TestOperation_String(t *testing.T) {
	assert.Equal(t, "ADD", ADD.String())
	assert.Equal(t, "REMOVE", REMOVE.String())
	assert.Equal(t, "UPDATE", UPDATE.String())
}
func TestUpdateNRPLoadBalancers(t *testing.T) {
	tests := []struct {
		name         string
		initialState *DiffTracker
		expectedNRP  *sets.IgnoreCaseSet
	}{
		{
			name: "add services from K8s to NRP",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("service1", "service2", "service3"),
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString("service1"),
				},
			},
			expectedNRP: sets.NewString("service1", "service2", "service3"),
		},
		{
			name: "remove services from NRP that are not in K8s",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("service1"),
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString("service1", "service2", "service3"),
				},
			},
			expectedNRP: sets.NewString("service1"),
		},
		{
			name: "add and remove services to sync K8s and NRP",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("service1", "service2", "service4"),
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString("service1", "service3", "service5"),
				},
			},
			expectedNRP: sets.NewString("service1", "service2", "service4"),
		},
		{
			name: "no changes needed when K8s and NRP are in sync",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("service1", "service2"),
				},
				NRPResources: NRP_State{
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
func TestUpdateK8sEgress(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8s_State{
			Egresses: sets.NewString(),
		},
	}

	// Test ADD operation
	err := dt.UpdateK8sEgress(UpdateK8sResource{
		Operation: ADD,
		ID:        "egress1",
	})
	assert.NoError(t, err)
	assert.True(t, dt.K8sResources.Egresses.Has("egress1"))

	// Test REMOVE operation
	err = dt.UpdateK8sEgress(UpdateK8sResource{
		Operation: REMOVE,
		ID:        "egress1",
	})
	assert.NoError(t, err)
	assert.False(t, dt.K8sResources.Egresses.Has("egress1"))

	// Test invalid operation
	err = dt.UpdateK8sEgress(UpdateK8sResource{
		Operation: UPDATE,
		ID:        "egress1",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "error - ResourceType=Egress, Operation=UPDATE and ID=egress1")
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
				K8sResources: K8s_State{
					Egresses: sets.NewString(tt.k8sEgresses...),
				},
				NRPResources: NRP_State{
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
func TestUpdateNRPNATGateways(t *testing.T) {
	tests := []struct {
		name         string
		initialState *DiffTracker
		expectedNRP  *sets.IgnoreCaseSet
	}{
		{
			name: "add egresses from K8s to NRP",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
					Egresses: sets.NewString("egress1", "egress2", "egress3"),
				},
				NRPResources: NRP_State{
					NATGateways: sets.NewString("egress1"),
				},
			},
			expectedNRP: sets.NewString("egress1", "egress2", "egress3"),
		},
		{
			name: "remove egresses from NRP that are not in K8s",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
					Egresses: sets.NewString("egress1"),
				},
				NRPResources: NRP_State{
					NATGateways: sets.NewString("egress1", "egress2", "egress3"),
				},
			},
			expectedNRP: sets.NewString("egress1"),
		},
		{
			name: "add and remove egresses to sync K8s and NRP",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
					Egresses: sets.NewString("egress1", "egress2", "egress4"),
				},
				NRPResources: NRP_State{
					NATGateways: sets.NewString("egress1", "egress3", "egress5"),
				},
			},
			expectedNRP: sets.NewString("egress1", "egress2", "egress4"),
		},
		{
			name: "no changes needed when K8s and NRP are in sync",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
					Egresses: sets.NewString("egress1", "egress2"),
				},
				NRPResources: NRP_State{
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
				K8sResources: K8s_State{
					Nodes: map[string]Node{},
				},
				NRPResources: NRP_State{
					Locations: map[string]NRPLocation{},
				},
			},
			expectedNRP: map[string]map[string][]string{},
		},
		{
			name: "add new location and address",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
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
				NRPResources: NRP_State{
					Locations: map[string]NRPLocation{},
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
				K8sResources: K8s_State{
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
				NRPResources: NRP_State{
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
				K8sResources: K8s_State{
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
				NRPResources: NRP_State{
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
				K8sResources: K8s_State{
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
				NRPResources: NRP_State{
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
				K8sResources: K8s_State{
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
				NRPResources: NRP_State{
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

func createTestDiffTracker() *DiffTracker {
	return &DiffTracker{
		K8sResources: K8s_State{
			Services: sets.NewString(),
			Egresses: sets.NewString(),
			Nodes: map[string]Node{
				"node1": {
					Pods: map[string]Pod{
						"10.0.0.1": {
							InboundIdentities:      sets.NewString("existingService"),
							PublicOutboundIdentity: "existingPublicOutboundIdentity",
						},
					},
				},
			},
		},
		NRPResources: NRP_State{
			LoadBalancers: sets.NewString(),
			NATGateways:   sets.NewString(),
			Locations: map[string]NRPLocation{
				"node1": {
					Addresses: map[string]NRPAddress{
						"10.0.0.1": {
							Services: sets.NewString("existingService", "existingPublicOutboundIdentity"),
						},
					},
				},
			},
		},
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
				K8sResources: K8s_State{
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
				NRPResources: NRP_State{
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
			expectedSyncStatus:      ALREADY_IN_SYNC,
			expectedLoadBalancerOps: false,
			expectedNATGatewayOps:   false,
			expectedLocationOps:     false,
		},
		{
			name: "services out of sync",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
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
				NRPResources: NRP_State{
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
			expectedSyncStatus:      SUCCESS,
			expectedLoadBalancerOps: true,
			expectedNATGatewayOps:   false,
			expectedLocationOps:     false,
		},
		{
			name: "egresses out of sync",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
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
				NRPResources: NRP_State{
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
			expectedSyncStatus:      SUCCESS,
			expectedLoadBalancerOps: false,
			expectedNATGatewayOps:   true,
			expectedLocationOps:     false,
		},
		{
			name: "locations out of sync",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("service1"),
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
				NRPResources: NRP_State{
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
			expectedSyncStatus:      SUCCESS,
			expectedLoadBalancerOps: false,
			expectedNATGatewayOps:   false,
			expectedLocationOps:     true,
		},
		{
			name: "multiple components out of sync",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
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
				NRPResources: NRP_State{
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
			expectedSyncStatus:      SUCCESS,
			expectedLoadBalancerOps: true,
			expectedNATGatewayOps:   true,
			expectedLocationOps:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.initialState.GetSyncOperations()

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
func TestInitializeDiffTracker(t *testing.T) {
	K8sResources := K8s_State{
		Services: sets.NewString("Service0", "Service1", "Service2"),
		Egresses: sets.NewString("Egress0", "Egress1", "Egress2"),
		Nodes: map[string]Node{
			"Node1": {
				Pods: map[string]Pod{
					"Pod34": {
						InboundIdentities:      sets.NewString("Service0"),
						PublicOutboundIdentity: "",
					},
					"Pod0": {
						InboundIdentities:      sets.NewString("Service0"),
						PublicOutboundIdentity: "Egress0",
					},
					"Pod1": {
						InboundIdentities:      sets.NewString("Service1", "Service2"),
						PublicOutboundIdentity: "Egress1",
					},
					"Pod3": {
						InboundIdentities:      sets.NewString(),
						PublicOutboundIdentity: "Egress2",
					},
				},
			},
			"Node2": {
				Pods: map[string]Pod{
					"Pod2": {
						InboundIdentities:      sets.NewString("Service1"),
						PublicOutboundIdentity: "Egress2",
					},
				},
			},
		},
	}

	NRPResources := NRP_State{
		LoadBalancers: sets.NewString("Service0", "Service6", "Service5"),
		NATGateways:   sets.NewString("Egress0", "Egress6", "Egress5"),
		Locations: map[string]NRPLocation{
			"Node1": {
				Addresses: map[string]NRPAddress{
					"Pod34": {
						Services: sets.NewString("Service0", "Service5"),
					},
					"Pod00": {
						Services: sets.NewString("Service6", "Egress5"),
					},
					"Pod0": {
						Services: sets.NewString("Service0", "Egress0"),
					},
				},
			},
			"Node3": {
				Addresses: map[string]NRPAddress{
					"Pod4": {
						Services: sets.NewString("Service6", "Eggres6"),
					},
					"Pod5": {
						Services: sets.NewString("Egress5"),
					},
				},
			},
		},
	}

	expectedSyncOperations := &SyncDiffTrackerReturnType{
		SyncStatus: SUCCESS,
		LoadBalancerUpdates: SyncServicesReturnType{
			Additions: sets.NewString("Service1", "Service2"),
			Removals:  sets.NewString("Service6", "Service5"),
		},
		NATGatewayUpdates: SyncServicesReturnType{
			Additions: sets.NewString("Egress1", "Egress2"),
			Removals:  sets.NewString("Egress6", "Egress5"),
		},
		LocationData: LocationData{
			Action: PartialUpdate,
			Locations: map[string]Location{
				"Node1": {
					AddressUpdateAction: PartialUpdate,
					Addresses: map[string]Address{
						"Pod00": {
							ServiceRef: sets.NewString(),
						},
						"Pod34": {
							ServiceRef: sets.NewString("Service0"),
						},
						"Pod1": {
							ServiceRef: sets.NewString("Service1", "Service2", "Egress1"),
						},
						"Pod3": {
							ServiceRef: sets.NewString("Egress2"),
						},
					},
				},
				"Node2": {
					AddressUpdateAction: FullUpdate,
					Addresses: map[string]Address{
						"Pod2": {
							ServiceRef: sets.NewString("Service1", "Egress2"),
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

	// Create a minimal mock for testing - we're only testing the initialization logic
	// and sync operations calculation, not actual Azure API calls
	config := Config{
		SubscriptionID: "test-subscription",
		ResourceGroup:  "test-rg",
		Location:       "eastus",

		ServiceGatewayResourceName: "test-sgw",
		ServiceGatewayID:           "/subscriptions/test-subscription/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}

	// For this test, we're only testing state tracking logic, not Azure API calls
	// Provide mock clients to satisfy validation (they won't be called)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockKubeClient := fake.NewSimpleClientset()
	diffTracker := InitializeDiffTracker(K8sResources, NRPResources, config, mockFactory, mockKubeClient)
	syncOperations := diffTracker.GetSyncOperations()
	// It follows a call to ServiceGateway API and if it is successful we can proceed with syncing difftracker.NRP
	diffTracker.UpdateNRPLoadBalancers(syncOperations.LoadBalancerUpdates)
	diffTracker.UpdateNRPNATGateways(syncOperations.NATGatewayUpdates)
	diffTracker.UpdateLocationsAddresses(syncOperations.LocationData)

	assert.True(t, syncOperations.Equals(expectedSyncOperations),
		"Sync operations do not match expected values")

	// Check if the DiffTracker is updated correctly
	expectedDiffTracker := &DiffTracker{
		K8sResources: K8sResources,
		NRPResources: NRP_State{
			LoadBalancers: sets.NewString("Service0", "Service1", "Service2"),
			NATGateways:   sets.NewString("Egress0", "Egress1", "Egress2"),
			Locations: map[string]NRPLocation{
				"Node1": {
					Addresses: map[string]NRPAddress{
						"Pod34": {
							Services: sets.NewString("Service0"),
						},
						"Pod0": {
							Services: sets.NewString("Service0", "Egress0"),
						},
						"Pod1": {
							Services: sets.NewString("Service1", "Service2", "Egress1"),
						},
						"Pod3": {
							Services: sets.NewString("Egress2"),
						},
					},
				},
				"Node2": {
					Addresses: map[string]NRPAddress{
						"Pod2": {
							Services: sets.NewString("Service1", "Egress2"),
						},
					},
				},
			},
		},
	}

	assert.True(t, diffTracker.Equals(expectedDiffTracker),
		"DiffTracker does not match expected state")
}

func TestMapLocationDataToDTO(t *testing.T) {
	tests := []struct {
		name         string
		locationData LocationData
		expected     LocationsDataDTO
	}{
		{
			name: "Empty location data",
			locationData: LocationData{
				Action:    PartialUpdate,
				Locations: map[string]Location{},
			},
			expected: LocationsDataDTO{
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
			expected: LocationsDataDTO{
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
								ServiceRef: sets.NewString("service1", "service2"),
							},
							"addr2": {
								ServiceRef: sets.NewString("service3"),
							},
						},
					},
					"location2": {
						AddressUpdateAction: FullUpdate,
						Addresses: map[string]Address{
							"addr3": {
								ServiceRef: sets.NewString("service4"),
							},
						},
					},
				},
			},
			expected: LocationsDataDTO{
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
								ServiceNames: sets.NewString("service1", "service2"),
							},
							{
								Address:      "addr2",
								ServiceNames: sets.NewString("service3"),
							},
						},
					},
					{
						Location:            "location2",
						AddressUpdateAction: FullUpdate,
						Addresses: []AddressDTO{
							{
								Address:      "addr3",
								ServiceNames: sets.NewString("service4"),
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MapLocationDataToDTO(tt.locationData)

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

func TestMapLoadBalancerUpdatesToServicesDataDTO_EmptyUpdates(t *testing.T) {
	updates := SyncServicesReturnType{
		Additions: sets.NewString(),
		Removals:  sets.NewString(),
	}

	actual := MapLoadBalancerUpdatesToServicesDataDTO(updates, subscriptionID, resourceGroupName)

	expected := ServicesDataDTO{
		Action:   PartialUpdate,
		Services: []ServiceDTO{},
	}

	assert.Equal(t, expected, actual)
}

func TestMapLoadBalancerUpdatesToServicesDataDTO_OnlyAdditions(t *testing.T) {
	updates := SyncServicesReturnType{
		Additions: sets.NewString("service-1", "service-2"),
		Removals:  sets.NewString(),
	}

	actual := MapLoadBalancerUpdatesToServicesDataDTO(updates, subscriptionID, resourceGroupName)

	expected := ServicesDataDTO{
		Action: PartialUpdate,
		Services: []ServiceDTO{
			{
				Service:     "service-1",
				ServiceType: Inbound,
				IsDelete:    false,
				LoadBalancerBackendPools: []LoadBalancerBackendPoolDTO{
					{
						Id: "/subscriptions/test-subscription-id/resourceGroups/test-resource-group-name/providers/Microsoft.Network/loadBalancers/service-1/backendAddressPools/service-1",
					},
				},
			},
			{
				Service:     "service-2",
				ServiceType: Inbound,
				IsDelete:    false,
				LoadBalancerBackendPools: []LoadBalancerBackendPoolDTO{
					{
						Id: "/subscriptions/test-subscription-id/resourceGroups/test-resource-group-name/providers/Microsoft.Network/loadBalancers/service-2/backendAddressPools/service-2",
					},
				},
			},
		},
	}

	// Sort both slices for consistent comparison
	sortServiceDTOs(expected.Services)
	sortServiceDTOs(actual.Services)

	assert.Equal(t, expected, actual)
}

func TestMapLoadBalancerUpdatesToServicesDataDTO_OnlyRemovals(t *testing.T) {
	updates := SyncServicesReturnType{
		Additions: sets.NewString(),
		Removals:  sets.NewString("service-3", "service-4"),
	}

	actual := MapLoadBalancerUpdatesToServicesDataDTO(updates, subscriptionID, resourceGroupName)

	expected := ServicesDataDTO{
		Action: PartialUpdate,
		Services: []ServiceDTO{
			{
				Service:     "service-3",
				ServiceType: Inbound,
				IsDelete:    true,
			},
			{
				Service:     "service-4",
				ServiceType: Inbound,
				IsDelete:    true,
			},
		},
	}

	// Sort both slices for consistent comparison
	sortServiceDTOs(expected.Services)
	sortServiceDTOs(actual.Services)

	assert.Equal(t, expected, actual)
}

func TestMapLoadBalancerUpdatesToServicesDataDTO_AdditionsAndRemovals(t *testing.T) {
	updates := SyncServicesReturnType{
		Additions: sets.NewString("service-add-1", "service-add-2"),
		Removals:  sets.NewString("service-remove-1", "service-remove-2"),
	}

	actual := MapLoadBalancerUpdatesToServicesDataDTO(updates, subscriptionID, resourceGroupName)

	expected := ServicesDataDTO{
		Action: PartialUpdate,
		Services: []ServiceDTO{
			{
				Service:     "service-add-1",
				IsDelete:    false,
				ServiceType: Inbound,
				LoadBalancerBackendPools: []LoadBalancerBackendPoolDTO{
					{
						Id: "/subscriptions/test-subscription-id/resourceGroups/test-resource-group-name/providers/Microsoft.Network/loadBalancers/service-add-1/backendAddressPools/service-add-1",
					},
				},
			},
			{
				Service:     "service-add-2",
				ServiceType: Inbound,
				IsDelete:    false,
				LoadBalancerBackendPools: []LoadBalancerBackendPoolDTO{
					{
						Id: "/subscriptions/test-subscription-id/resourceGroups/test-resource-group-name/providers/Microsoft.Network/loadBalancers/service-add-2/backendAddressPools/service-add-2",
					},
				},
			},
			{
				Service:     "service-remove-1",
				ServiceType: Inbound,
				IsDelete:    true,
			},
			{
				Service:     "service-remove-2",
				ServiceType: Inbound,
				IsDelete:    true,
			},
		},
	}

	// Sort both slices for consistent comparison
	sortServiceDTOs(expected.Services)
	sortServiceDTOs(actual.Services)

	assert.Equal(t, expected, actual)
}

// Helper function to sort ServiceDTO slices by Service name
func sortServiceDTOs(services []ServiceDTO) {
	sort.Slice(services, func(i, j int) bool {
		return services[i].Service < services[j].Service
	})
}
func TestMapNATGatewayUpdatesToServicesDataDTO_EmptyUpdates(t *testing.T) {
	updates := SyncServicesReturnType{
		Additions: sets.NewString(),
		Removals:  sets.NewString(),
	}

	actual := MapNATGatewayUpdatesToServicesDataDTO(updates, subscriptionID, resourceGroupName)

	expected := ServicesDataDTO{
		Action:   PartialUpdate,
		Services: []ServiceDTO{},
	}

	assert.Equal(t, expected, actual)
}

func TestMapNATGatewayUpdatesToServicesDataDTO_OnlyAdditions(t *testing.T) {
	updates := SyncServicesReturnType{
		Additions: sets.NewString("natgw-1", "natgw-2"),
		Removals:  sets.NewString(),
	}

	actual := MapNATGatewayUpdatesToServicesDataDTO(updates, subscriptionID, resourceGroupName)

	expected := ServicesDataDTO{
		Action: PartialUpdate,
		Services: []ServiceDTO{
			{
				Service:     "natgw-1",
				ServiceType: Outbound,
				IsDelete:    false,
				PublicNatGateway: NatGatewayDTO{
					Id: "/subscriptions/test-subscription-id/resourceGroups/test-resource-group-name/providers/Microsoft.Network/natGateways/natgw-1",
				},
			},
			{
				Service:     "natgw-2",
				ServiceType: Outbound,
				IsDelete:    false,
				PublicNatGateway: NatGatewayDTO{
					Id: "/subscriptions/test-subscription-id/resourceGroups/test-resource-group-name/providers/Microsoft.Network/natGateways/natgw-2",
				},
			},
		},
	}

	// Sort both slices for consistent comparison
	sortServiceDTOs(expected.Services)
	sortServiceDTOs(actual.Services)

	assert.Equal(t, expected, actual)
}

func TestMapNATGatewayUpdatesToServicesDataDTO_OnlyRemovals(t *testing.T) {
	updates := SyncServicesReturnType{
		Additions: sets.NewString(),
		Removals:  sets.NewString("natgw-3", "natgw-4"),
	}

	actual := MapNATGatewayUpdatesToServicesDataDTO(updates, subscriptionID, resourceGroupName)

	expected := ServicesDataDTO{
		Action: PartialUpdate,
		Services: []ServiceDTO{
			{
				Service:     "natgw-3",
				ServiceType: Outbound,
				IsDelete:    true,
			},
			{
				Service:     "natgw-4",
				ServiceType: Outbound,
				IsDelete:    true,
			},
		},
	}

	// Sort both slices for consistent comparison
	sortServiceDTOs(expected.Services)
	sortServiceDTOs(actual.Services)

	assert.Equal(t, expected, actual)
}

func TestMapNATGatewayUpdatesToServicesDataDTO_AdditionsAndRemovals(t *testing.T) {
	updates := SyncServicesReturnType{
		Additions: sets.NewString("natgw-add-1", "natgw-add-2"),
		Removals:  sets.NewString("natgw-remove-1", "natgw-remove-2"),
	}

	actual := MapNATGatewayUpdatesToServicesDataDTO(updates, subscriptionID, resourceGroupName)

	expected := ServicesDataDTO{
		Action: PartialUpdate,
		Services: []ServiceDTO{
			{
				Service:     "natgw-add-1",
				ServiceType: Outbound,
				IsDelete:    false,
				PublicNatGateway: NatGatewayDTO{
					Id: "/subscriptions/test-subscription-id/resourceGroups/test-resource-group-name/providers/Microsoft.Network/natGateways/natgw-add-1",
				},
			},
			{
				Service:     "natgw-add-2",
				ServiceType: Outbound,
				IsDelete:    false,
				PublicNatGateway: NatGatewayDTO{
					Id: "/subscriptions/test-subscription-id/resourceGroups/test-resource-group-name/providers/Microsoft.Network/natGateways/natgw-add-2",
				},
			},
			{
				Service:     "natgw-remove-1",
				ServiceType: Outbound,
				IsDelete:    true,
			},
			{
				Service:     "natgw-remove-2",
				ServiceType: Outbound,
				IsDelete:    true,
			},
		},
	}

	// Sort both slices for consistent comparison
	sortServiceDTOs(expected.Services)
	sortServiceDTOs(actual.Services)

	assert.Equal(t, expected, actual)
}
