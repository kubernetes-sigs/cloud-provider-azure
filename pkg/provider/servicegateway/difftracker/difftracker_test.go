package difftracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/client-go/kubernetes/fake"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func createTestDiffTracker() *DiffTracker {
	return &DiffTracker{
		K8sResources: K8sState{
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
		NRPResources: NRPState{
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
func TestInitializeDiffTracker(t *testing.T) {
	K8sResources := K8sState{
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

	NRPResources := NRPState{
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
		SyncStatus: Success,
		LoadBalancerUpdates: SyncServicesReturnType{
			Additions: sets.NewString("Service1", "Service2"),
			Removals:  sets.NewString("Service6", "Service5"),
		},
		NATGatewayUpdates: SyncServicesReturnType{
			Additions: sets.NewString("Egress1", "Egress2"),
			Removals:  sets.NewString("Egress6", "Egress5"),
		},
		// LocationData: Only services that exist in NRP pass the filtering.
		// Service1, Service2, Egress1, Egress2 are being ADDED (don't exist in NRP yet),
		// so they won't appear in location sync until after they're created.
		// Only Service0 and Egress0 (which exist in NRP) will be synced.
		LocationData: LocationData{
			Action: PartialUpdate,
			Locations: map[string]Location{
				"Node1": {
					AddressUpdateAction: PartialUpdate,
					Addresses: map[string]Address{
						"Pod00": {
							ServiceRef: sets.NewString(), // Address in NRP not in K8s - will be removed
						},
						"Pod34": {
							ServiceRef: sets.NewString("Service0"), // Service0 exists in NRP
						},
						// Pod1: Service1, Service2, Egress1 don't exist in NRP yet - no sync
						// Pod3: Egress2 doesn't exist in NRP yet - no sync
					},
				},
				// Node2: Pod2 has Service1 and Egress2, but neither exist in NRP yet - no location data
				"Node3": {
					AddressUpdateAction: PartialUpdate,
					Addresses:           map[string]Address{}, // Node3 in NRP but not in K8s - will be cleared
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
		VNetName:       "test-vnet",

		ServiceGatewayResourceName: "test-sgw",
	}

	// For this test, we're only testing state tracking logic, not Azure API calls
	// Provide mock clients to satisfy validation (they won't be called)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockKubeClient := fake.NewSimpleClientset()
	diffTracker, err := New(log.Noop(), K8sResources, NRPResources, config, mockFactory, mockKubeClient)
	assert.NoError(t, err)
	syncOperations := diffTracker.GetSyncOperations()
	// It follows a call to ServiceGateway API and if it is successful we can proceed with syncing difftracker.NRP
	diffTracker.UpdateNRPLoadBalancers(syncOperations.LoadBalancerUpdates)
	diffTracker.UpdateNRPNATGateways(syncOperations.NATGatewayUpdates)
	diffTracker.UpdateLocationsAddresses(syncOperations.LocationData)

	assert.True(t, syncOperations.Equals(expectedSyncOperations),
		"Sync operations do not match expected values")

	// Check if the DiffTracker is updated correctly
	// Note: Location updates only affect services that exist in NRP.
	// Service1, Service2, Egress1, Egress2 are being added but don't exist in NRP yet,
	// so they won't appear in locations until after they're created and location sync runs again.
	//
	// UpdateLocationsAddresses behavior:
	// - Node1 (PartialUpdate): Pod00 deleted (empty ServiceRef), Pod34 updated to Service0,
	//   Pod0 unchanged (not in LocationData, PartialUpdate preserves existing addresses)
	// - Node3 (PartialUpdate with empty Addresses): Entire location deleted
	expectedDiffTracker := &DiffTracker{
		K8sResources: K8sResources,
		NRPResources: NRPState{
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
						// Pod00 deleted (empty ServiceRef in LocationData)
						// Pod1, Pod3 not added because their services don't exist in NRP yet
					},
				},
				// Node3 deleted because LocationData has empty Addresses for it
			},
		},
	}

	assert.True(t, diffTracker.Equals(expectedDiffTracker),
		"DiffTracker does not match expected state")
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
func validTestConfig() Config {
	return Config{
		SubscriptionID:             "test-subscription",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		VNetName:                   "test-vnet",
		ServiceGatewayResourceName: "test-sgw",
	}
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

	dt, err := New(log.Noop(), k8s, emptyNRPState(), validTestConfig(), mockFactory, mockKubeClient)
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

// TestUpdateK8sEndpointsRelocation covers the case where the same pod IP appears
// in both OldAddresses and NewAddresses but on a different node (relocation): the
// pod must be removed from the old node and added to the new one.
