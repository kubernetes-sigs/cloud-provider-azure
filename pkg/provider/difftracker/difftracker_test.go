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

func TestDiffTracker_DeepEqual(t *testing.T) {
	tests := []struct {
		name     string
		dt       *DiffTracker
		expected bool
	}{
		{
			name: "equal empty states",
			dt: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString(),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRPState{
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
				K8sResources: K8sState{
					Services: sets.NewString("service1", "service2"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRPState{
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
				K8sResources: K8sState{
					Services: sets.NewString("service1", "service2"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRPState{
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
			result := tt.dt.deepEqualLocked()
			assert.Equal(t, tt.expected, result)
		})
	}
}

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

func TestOperation_String(t *testing.T) {
	assert.Equal(t, "Add", Add.String())
	assert.Equal(t, "Remove", Remove.String())
	assert.Equal(t, "Update", Update.String())
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
			tt.initialState.UpdateNRPLoadBalancers(syncServices)

			assert.True(t, tt.expectedNRP.Equals(tt.initialState.NRPResources.LoadBalancers),
				"Expected NRP LoadBalancers %v, but got %v",
				tt.expectedNRP.UnsortedList(),
				tt.initialState.NRPResources.LoadBalancers.UnsortedList())
		})
	}
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

func TestUpdateNRPNATGateways(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{
			Egresses: sets.NewString("egress1", "egress2", "egress4"),
		},
		NRPResources: NRPState{
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
				K8sResources: K8sState{Nodes: map[string]Node{}},
				NRPResources: NRPState{Locations: map[string]NRPLocation{}},
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
								"10.0.0.5": {InboundIdentities: sets.NewString("service5")},
							},
						},
					},
				},
				NRPResources: NRPState{
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
func TestNew(t *testing.T) {
	K8sResources := K8sState{
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

	NRPResources := NRPState{
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
	diffTracker, err := New(K8sResources, NRPResources, config, mockFactory, mockKubeClient)
	assert.NoError(t, err)
	syncOperations := diffTracker.GetSyncOperations()

	diffTracker.UpdateNRPLoadBalancers(syncOperations.LoadBalancerUpdates)
	diffTracker.UpdateNRPNATGateways(syncOperations.NATGatewayUpdates)
	diffTracker.UpdateLocationsAddresses(syncOperations.LocationData)

	assert.Equal(t, Success, syncOperations.SyncStatus)

	expectedDiffTracker := &DiffTracker{
		K8sResources: K8sResources,
		NRPResources: NRPState{
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

func emptyK8sState() K8sState {
	return K8sState{
		Services: sets.NewString(),
		Egresses: sets.NewString(),
		Nodes:    make(map[string]Node),
	}
}

func emptyNRPState() NRPState {
	return NRPState{
		LoadBalancers: sets.NewString(),
		NATGateways:   sets.NewString(),
		Locations:     make(map[string]NRPLocation),
	}
}

// TestNewErrorPaths covers the validation/error branches of
// New and the successful initialization path.
func TestNewErrorPaths(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockKubeClient := fake.NewSimpleClientset()

	// Invalid config (empty) -> validation error.
	_, err := New(K8sState{}, NRPState{}, Config{}, mockFactory, mockKubeClient)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "New")

	// Nil networkClientFactory.
	_, err = New(K8sState{}, NRPState{}, validTestConfig(), nil, mockKubeClient)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "networkClientFactory must not be nil")

	// Nil kubeClient.
	_, err = New(K8sState{}, NRPState{}, validTestConfig(), mockFactory, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "kubeClient must not be nil")

	// Uninitialized state field -> error out instead of silently initializing.
	k8sMissingServices := emptyK8sState()
	k8sMissingServices.Services = nil
	_, err = New(k8sMissingServices, emptyNRPState(), validTestConfig(), mockFactory, mockKubeClient)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "k8s.Services must not be nil")

	nrpMissingLocations := emptyNRPState()
	nrpMissingLocations.Locations = nil
	_, err = New(emptyK8sState(), nrpMissingLocations, validTestConfig(), mockFactory, mockKubeClient)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nrp.Locations must not be nil")

	// Valid call with fully initialized (empty) states.
	dt, err := New(emptyK8sState(), emptyNRPState(), validTestConfig(), mockFactory, mockKubeClient)
	assert.NoError(t, err)
	assert.NotNil(t, dt)
	assert.NotNil(t, dt.K8sResources.Services)
	assert.NotNil(t, dt.K8sResources.Egresses)
	assert.NotNil(t, dt.K8sResources.Nodes)
	assert.NotNil(t, dt.NRPResources.LoadBalancers)
	assert.NotNil(t, dt.NRPResources.NATGateways)
	assert.NotNil(t, dt.NRPResources.Locations)
}

// TestNewSeedsOutboundRefCount verifies New seeds the outbound ref-counter from
// the egress pods already present in the initial state, so the counter is
// non-zero for identities that have backing pods at construction time.
func TestNewSeedsOutboundRefCount(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockKubeClient := fake.NewSimpleClientset()

	k8s := emptyK8sState()
	k8s.Nodes["node1"] = Node{Pods: map[string]Pod{
		"10.0.0.1": {InboundIdentities: sets.NewString(), PublicOutboundIdentity: "egress1"},
		"10.0.0.2": {InboundIdentities: sets.NewString(), PublicOutboundIdentity: "egress1"},
		"10.0.0.3": {InboundIdentities: sets.NewString("svc1"), PublicOutboundIdentity: ""},
	}}
	k8s.Nodes["node2"] = Node{Pods: map[string]Pod{
		"10.0.1.1": {InboundIdentities: sets.NewString(), PublicOutboundIdentity: "Egress2"},
	}}

	dt, err := New(k8s, emptyNRPState(), validTestConfig(), mockFactory, mockKubeClient)
	assert.NoError(t, err)

	val, ok := dt.outboundIdentityPodRefCount.Load("egress1")
	assert.True(t, ok)
	assert.Equal(t, 2, val.(int))

	val, ok = dt.outboundIdentityPodRefCount.Load("egress2")
	assert.True(t, ok, "identity key is lowercased")
	assert.Equal(t, 1, val.(int))

	_, ok = dt.outboundIdentityPodRefCount.Load("")
	assert.False(t, ok, "pods without an egress identity are not counted")
}

// TestEnqueueK8sResourceOperationErrors covers the empty-ID and invalid-operation
// branches of enqueueK8sResourceOperation (via the public wrappers).
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
