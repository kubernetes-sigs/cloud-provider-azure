package difftracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
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

	assert.True(t, result.Additions.Has("service1"))
	assert.Equal(t, 1, result.Additions.Len())

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
	assert.NotContains(t, dt.K8sResources.Nodes["node1"].Pods, "10.0.0.1")
}

func TestGetSyncLocationsAddresses(t *testing.T) {
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
			tt.initialState.UpdateNRPLoadBalancers(syncServices)

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

	err := dt.UpdateK8sEgress(UpdateK8sResource{Operation: ADD, ID: "egress1"})
	assert.NoError(t, err)
	assert.True(t, dt.K8sResources.Egresses.Has("egress1"))

	err = dt.UpdateK8sEgress(UpdateK8sResource{Operation: REMOVE, ID: "egress1"})
	assert.NoError(t, err)
	assert.False(t, dt.K8sResources.Egresses.Has("egress1"))

	err = dt.UpdateK8sEgress(UpdateK8sResource{Operation: UPDATE, ID: "egress1"})
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
				K8sResources: K8s_State{
					Egresses: sets.NewString(tt.k8sEgresses...),
				},
				NRPResources: NRP_State{
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

func TestUpdateNRPNATGateways(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8s_State{
			Egresses: sets.NewString("egress1", "egress2", "egress4"),
		},
		NRPResources: NRP_State{
			NATGateways: sets.NewString("egress1", "egress3", "egress5"),
		},
	}

	syncServices := dt.GetSyncNRPNATGateways()
	dt.UpdateNRPNATGateways(syncServices)

	expectedNRP := sets.NewString("egress1", "egress2", "egress4")
	assert.True(t, expectedNRP.Equals(dt.NRPResources.NATGateways),
		"Expected NRP NATGateways %v, but got %v",
		expectedNRP.UnsortedList(),
		dt.NRPResources.NATGateways.UnsortedList())
}

func TestUpdateLocationsAddresses(t *testing.T) {
	tests := []struct {
		name         string
		initialState *DiffTracker
		expectedNRP  map[string]map[string][]string
	}{
		{
			name: "sync empty states",
			initialState: &DiffTracker{
				K8sResources: K8s_State{Nodes: map[string]Node{}},
				NRPResources: NRP_State{Locations: map[string]NRPLocation{}},
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
					LoadBalancers: sets.NewString("service1"),
					NATGateways:   sets.NewString("public1"),
					Locations:     map[string]NRPLocation{},
				},
			},
			expectedNRP: map[string]map[string][]string{
				"node1": {"10.0.0.1": {"service1", "public1"}},
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
								"10.0.0.5": {InboundIdentities: sets.NewString("service5")},
							},
						},
					},
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString("service1", "service2", "service3", "service4", "service5"),
					NATGateways:   sets.NewString("public1"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {Services: sets.NewString("service1", "service2")},
								"10.0.0.2": {Services: sets.NewString("service4")},
							},
						},
						"node2": {
							Addresses: map[string]NRPAddress{
								"10.0.0.3": {Services: sets.NewString("service3")},
							},
						},
					},
				},
			},
			expectedNRP: map[string]map[string][]string{
				"node1": {"10.0.0.1": {"service1", "service3", "public1"}},
				"node3": {"10.0.0.5": {"service5"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			locationData := tt.initialState.GetSyncLocationsAddresses()
			tt.initialState.UpdateLocationsAddresses(locationData)

			assert.Equal(t, len(tt.expectedNRP), len(tt.initialState.NRPResources.Locations))

			for locName, expectedAddressMap := range tt.expectedNRP {
				nrpLoc, exists := tt.initialState.NRPResources.Locations[locName]
				assert.True(t, exists, "Expected location %s not found", locName)
				assert.Equal(t, len(expectedAddressMap), len(nrpLoc.Addresses))

				for addr, expectedServices := range expectedAddressMap {
					nrpAddr, exists := nrpLoc.Addresses[addr]
					assert.True(t, exists, "Expected address %s not found in %s", addr, locName)
					assert.Equal(t, len(expectedServices), nrpAddr.Services.Len())
					for _, svc := range expectedServices {
						assert.True(t, nrpAddr.Services.Has(svc))
					}
				}
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
				K8sResources: K8s_State{
					Services: sets.NewString("service1"),
					Egresses: sets.NewString("egress1"),
					Nodes: map[string]Node{
						"node1": {Pods: map[string]Pod{
							"10.0.0.1": {InboundIdentities: sets.NewString("service1")},
						}},
					},
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString("service1"),
					NATGateways:   sets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {Addresses: map[string]NRPAddress{
							"10.0.0.1": {Services: sets.NewString("service1")},
						}},
					},
				},
			},
			expectedSyncStatus: ALREADY_IN_SYNC,
		},
		{
			name: "services out of sync",
			initialState: &DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("service1", "service2"),
					Egresses: sets.NewString("egress1"),
					Nodes: map[string]Node{
						"node1": {Pods: map[string]Pod{
							"10.0.0.1": {InboundIdentities: sets.NewString("service1")},
						}},
					},
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString("service1"),
					NATGateways:   sets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {Addresses: map[string]NRPAddress{
							"10.0.0.1": {Services: sets.NewString("service1")},
						}},
					},
				},
			},
			expectedSyncStatus: SUCCESS,
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
func TestInitializeDiffTracker(t *testing.T) {
	K8sResources := K8s_State{
		Services: sets.NewString("Service0", "Service1", "Service2"),
		Egresses: sets.NewString("Egress0", "Egress1", "Egress2"),
		Nodes: map[string]Node{
			"Node1": {
				Pods: map[string]Pod{
					"Pod34": {InboundIdentities: sets.NewString("Service0"), PublicOutboundIdentity: ""},
					"Pod0":  {InboundIdentities: sets.NewString("Service0"), PublicOutboundIdentity: "Egress0"},
					"Pod1":  {InboundIdentities: sets.NewString("Service1", "Service2"), PublicOutboundIdentity: "Egress1"},
					"Pod3":  {InboundIdentities: sets.NewString(), PublicOutboundIdentity: "Egress2"},
				},
			},
			"Node2": {
				Pods: map[string]Pod{
					"Pod2": {InboundIdentities: sets.NewString("Service1"), PublicOutboundIdentity: "Egress2"},
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
					"Pod34": {Services: sets.NewString("Service0", "Service5")},
					"Pod00": {Services: sets.NewString("Service6", "Egress5")},
					"Pod0":  {Services: sets.NewString("Service0", "Egress0")},
				},
			},
			"Node3": {
				Addresses: map[string]NRPAddress{
					"Pod4": {Services: sets.NewString("Service6", "Eggres6")},
					"Pod5": {Services: sets.NewString("Egress5")},
				},
			},
		},
	}

	config := Config{
		SubscriptionID:             "test-subscription",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		VNetName:                   "test-vnet",
		ServiceGatewayResourceName: "test-sgw",
		ServiceGatewayID:           "/subscriptions/test-subscription/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockKubeClient := fake.NewSimpleClientset()
	diffTracker := InitializeDiffTracker(K8sResources, NRPResources, config, mockFactory, mockKubeClient)
	syncOperations := diffTracker.GetSyncOperations()

	diffTracker.UpdateNRPLoadBalancers(syncOperations.LoadBalancerUpdates)
	diffTracker.UpdateNRPNATGateways(syncOperations.NATGatewayUpdates)
	diffTracker.UpdateLocationsAddresses(syncOperations.LocationData)

	assert.Equal(t, SUCCESS, syncOperations.SyncStatus)

	expectedDiffTracker := &DiffTracker{
		K8sResources: K8sResources,
		NRPResources: NRP_State{
			LoadBalancers: sets.NewString("Service0", "Service1", "Service2"),
			NATGateways:   sets.NewString("Egress0", "Egress1", "Egress2"),
			Locations: map[string]NRPLocation{
				"Node1": {
					Addresses: map[string]NRPAddress{
						"Pod34": {Services: sets.NewString("Service0")},
						"Pod0":  {Services: sets.NewString("Service0", "Egress0")},
					},
				},
			},
		},
	}

	assert.True(t, diffTracker.Equals(expectedDiffTracker),
		"DiffTracker does not match expected state")
}
