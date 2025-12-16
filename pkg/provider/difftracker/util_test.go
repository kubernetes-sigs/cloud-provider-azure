package difftracker

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// TestOperationStringAndJSON tests Operation String() and MarshalJSON()
func TestOperationStringAndJSON(t *testing.T) {
	tests := []struct {
		name     string
		op       Operation
		expected string
	}{
		{"ADD operation", ADD, "ADD"},
		{"REMOVE operation", REMOVE, "REMOVE"},
		{"UPDATE operation", UPDATE, "UPDATE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test String()
			assert.Equal(t, tt.expected, tt.op.String())

			// Test MarshalJSON()
			data, err := tt.op.MarshalJSON()
			assert.NoError(t, err)
			assert.Equal(t, `"`+tt.expected+`"`, string(data))
		})
	}
}

// TestUpdateActionStringAndJSON tests UpdateAction String(), MarshalJSON(), and UnmarshalJSON()
func TestUpdateActionStringAndJSON(t *testing.T) {
	tests := []struct {
		name     string
		action   UpdateAction
		expected string
	}{
		{"PartialUpdate", PartialUpdate, "PartialUpdate"},
		{"FullUpdate", FullUpdate, "FullUpdate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test String()
			assert.Equal(t, tt.expected, tt.action.String())

			// Test MarshalJSON()
			data, err := tt.action.MarshalJSON()
			assert.NoError(t, err)
			assert.Equal(t, `"`+tt.expected+`"`, string(data))

			// Test UnmarshalJSON() round-trip
			var unmarshaled UpdateAction
			err = unmarshaled.UnmarshalJSON(data)
			assert.NoError(t, err)
			assert.Equal(t, tt.action, unmarshaled)
		})
	}
}

// TestUpdateActionUnmarshalJSON_InvalidValue tests error handling
func TestUpdateActionUnmarshalJSON_InvalidValue(t *testing.T) {
	var action UpdateAction
	err := action.UnmarshalJSON([]byte(`"InvalidAction"`))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown UpdateAction")
}

// TestUpdateActionUnmarshalJSON_InvalidJSON tests JSON parsing errors
func TestUpdateActionUnmarshalJSON_InvalidJSON(t *testing.T) {
	var action UpdateAction
	err := action.UnmarshalJSON([]byte(`{invalid json`))
	assert.Error(t, err)
}

// TestSyncStatusStringAndJSON tests SyncStatus String() and MarshalJSON()
func TestSyncStatusStringAndJSON(t *testing.T) {
	tests := []struct {
		name     string
		status   SyncStatus
		expected string
	}{
		{"ALREADY_IN_SYNC", ALREADY_IN_SYNC, "ALREADY_IN_SYNC"},
		{"SUCCESS", SUCCESS, "SUCCESS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test String()
			assert.Equal(t, tt.expected, tt.status.String())

			// Test MarshalJSON()
			data, err := tt.status.MarshalJSON()
			assert.NoError(t, err)
			assert.Equal(t, `"`+tt.expected+`"`, string(data))
		})
	}
}

// TestNodeHasPods tests Node.HasPods()
func TestNodeHasPods(t *testing.T) {
	tests := []struct {
		name     string
		node     Node
		expected bool
	}{
		{
			name:     "node with pods",
			node:     Node{Pods: map[string]Pod{"pod1": {}}},
			expected: true,
		},
		{
			name:     "node without pods",
			node:     Node{Pods: map[string]Pod{}},
			expected: false,
		},
		{
			name:     "node with nil pods map",
			node:     Node{Pods: nil},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.node.HasPods())
		})
	}
}

// TestPodHasIdentities tests Pod.HasIdentities()
func TestPodHasIdentities(t *testing.T) {
	tests := []struct {
		name     string
		pod      Pod
		expected bool
	}{
		{
			name:     "pod with inbound identities",
			pod:      Pod{InboundIdentities: sets.NewString("id1", "id2")},
			expected: true,
		},
		{
			name:     "pod with public outbound identity",
			pod:      Pod{PublicOutboundIdentity: "outbound-id"},
			expected: true,
		},
		{
			name:     "pod with both identities",
			pod:      Pod{InboundIdentities: sets.NewString("id1"), PublicOutboundIdentity: "outbound-id"},
			expected: true,
		},
		{
			name:     "pod without identities",
			pod:      Pod{InboundIdentities: sets.NewString(), PublicOutboundIdentity: ""},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.pod.HasIdentities())
		})
	}
}

// TestDeepEqual tests DiffTracker.DeepEqual()
func TestDeepEqual(t *testing.T) {
	tests := []struct {
		name     string
		dt       DiffTracker
		expected bool
	}{
		{
			name: "in sync - matching services and load balancers",
			dt: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("svc1", "svc2"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString("svc1", "svc2"),
					NATGateways:   sets.NewString(),
					Locations:     map[string]NRPLocation{},
				},
			},
			expected: true,
		},
		{
			name: "not in sync - service count mismatch",
			dt: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("svc1"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString("svc1", "svc2"),
					NATGateways:   sets.NewString(),
					Locations:     map[string]NRPLocation{},
				},
			},
			expected: false,
		},
		{
			name: "not in sync - service name mismatch",
			dt: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("svc1"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString("svc2"),
					NATGateways:   sets.NewString(),
					Locations:     map[string]NRPLocation{},
				},
			},
			expected: false,
		},
		{
			name: "in sync - matching egresses and NAT gateways",
			dt: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString(),
					Egresses: sets.NewString("egress1", "egress2"),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString(),
					NATGateways:   sets.NewString("egress1", "egress2"),
					Locations:     map[string]NRPLocation{},
				},
			},
			expected: true,
		},
		{
			name: "not in sync - egress count mismatch",
			dt: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString(),
					Egresses: sets.NewString("egress1"),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRP_State{
					LoadBalancers: sets.NewString(),
					NATGateways:   sets.NewString("egress1", "egress2"),
					Locations:     map[string]NRPLocation{},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.dt.DeepEqual())
		})
	}
}

// TestLocationDataEquals tests LocationData.Equals()
func TestLocationDataEquals(t *testing.T) {
	tests := []struct {
		name     string
		ld1      LocationData
		ld2      LocationData
		expected bool
	}{
		{
			name: "equal location data - empty locations",
			ld1: LocationData{
				Action:    PartialUpdate,
				Locations: map[string]Location{},
			},
			ld2: LocationData{
				Action:    PartialUpdate,
				Locations: map[string]Location{},
			},
			expected: true,
		},
		{
			name: "equal location data - with addresses",
			ld1: LocationData{
				Action: PartialUpdate,
				Locations: map[string]Location{
					"loc1": {
						AddressUpdateAction: PartialUpdate,
						Addresses: map[string]Address{
							"addr1": {ServiceRef: sets.NewString("svc1")},
						},
					},
				},
			},
			ld2: LocationData{
				Action: PartialUpdate,
				Locations: map[string]Location{
					"loc1": {
						AddressUpdateAction: PartialUpdate,
						Addresses: map[string]Address{
							"addr1": {ServiceRef: sets.NewString("svc1")},
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "different action",
			ld1: LocationData{
				Action:    PartialUpdate,
				Locations: map[string]Location{},
			},
			ld2: LocationData{
				Action:    FullUpdate,
				Locations: map[string]Location{},
			},
			expected: false,
		},
		{
			name: "different location count",
			ld1: LocationData{
				Action: PartialUpdate,
				Locations: map[string]Location{
					"loc1": {AddressUpdateAction: PartialUpdate, Addresses: map[string]Address{}},
				},
			},
			ld2: LocationData{
				Action: PartialUpdate,
				Locations: map[string]Location{
					"loc1": {AddressUpdateAction: PartialUpdate, Addresses: map[string]Address{}},
					"loc2": {AddressUpdateAction: PartialUpdate, Addresses: map[string]Address{}},
				},
			},
			expected: false,
		},
		{
			name: "different address update action",
			ld1: LocationData{
				Action: PartialUpdate,
				Locations: map[string]Location{
					"loc1": {AddressUpdateAction: PartialUpdate, Addresses: map[string]Address{}},
				},
			},
			ld2: LocationData{
				Action: PartialUpdate,
				Locations: map[string]Location{
					"loc1": {AddressUpdateAction: FullUpdate, Addresses: map[string]Address{}},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.ld1.Equals(&tt.ld2))
		})
	}
}

// TestDiffTrackerEquals tests DiffTracker.Equals()
func TestDiffTrackerEquals(t *testing.T) {
	tests := []struct {
		name     string
		dt1      DiffTracker
		dt2      DiffTracker
		expected bool
	}{
		{
			name: "equal diff trackers",
			dt1: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("svc1"),
					Egresses: sets.NewString("egress1"),
					Nodes:    map[string]Node{},
				},
			},
			dt2: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("svc1"),
					Egresses: sets.NewString("egress1"),
					Nodes:    map[string]Node{},
				},
			},
			expected: true,
		},
		{
			name: "different services",
			dt1: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("svc1"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
			},
			dt2: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString("svc2"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
			},
			expected: false,
		},
		{
			name: "different egresses",
			dt1: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString(),
					Egresses: sets.NewString("egress1"),
					Nodes:    map[string]Node{},
				},
			},
			dt2: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString(),
					Egresses: sets.NewString("egress2"),
					Nodes:    map[string]Node{},
				},
			},
			expected: false,
		},
		{
			name: "different node count",
			dt1: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString(),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{"node1": {}},
				},
			},
			dt2: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString(),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
			},
			expected: false,
		},
		{
			name: "different pod count in node",
			dt1: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString(),
					Egresses: sets.NewString(),
					Nodes: map[string]Node{
						"node1": {Pods: map[string]Pod{"pod1": {}}},
					},
				},
			},
			dt2: DiffTracker{
				K8sResources: K8s_State{
					Services: sets.NewString(),
					Egresses: sets.NewString(),
					Nodes: map[string]Node{
						"node1": {Pods: map[string]Pod{}},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.dt1.Equals(&tt.dt2))
		})
	}
}

// TestSyncServicesReturnTypeEquals tests SyncServicesReturnType.Equals()
func TestSyncServicesReturnTypeEquals(t *testing.T) {
	tests := []struct {
		name     string
		s1       SyncServicesReturnType
		s2       SyncServicesReturnType
		expected bool
	}{
		{
			name: "equal - both empty",
			s1: SyncServicesReturnType{
				Additions: sets.NewString(),
				Removals:  sets.NewString(),
			},
			s2: SyncServicesReturnType{
				Additions: sets.NewString(),
				Removals:  sets.NewString(),
			},
			expected: true,
		},
		{
			name: "equal - same additions",
			s1: SyncServicesReturnType{
				Additions: sets.NewString("svc1", "svc2"),
				Removals:  sets.NewString(),
			},
			s2: SyncServicesReturnType{
				Additions: sets.NewString("svc1", "svc2"),
				Removals:  sets.NewString(),
			},
			expected: true,
		},
		{
			name: "not equal - different additions",
			s1: SyncServicesReturnType{
				Additions: sets.NewString("svc1"),
				Removals:  sets.NewString(),
			},
			s2: SyncServicesReturnType{
				Additions: sets.NewString("svc2"),
				Removals:  sets.NewString(),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.s1.Equals(&tt.s2))
		})
	}
}

// TestMapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO tests the DTO mapping function
func TestMapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(t *testing.T) {
	tests := []struct {
		name        string
		lbUpdates   SyncServicesReturnType
		natUpdates  SyncServicesReturnType
		expectedLen int
	}{
		{
			name: "only inbound additions",
			lbUpdates: SyncServicesReturnType{
				Additions: sets.NewString("svc1", "svc2"),
			},
			natUpdates:  SyncServicesReturnType{},
			expectedLen: 2,
		},
		{
			name:      "only outbound additions",
			lbUpdates: SyncServicesReturnType{},
			natUpdates: SyncServicesReturnType{
				Additions: sets.NewString("egress1"),
			},
			expectedLen: 1,
		},
		{
			name: "mixed additions",
			lbUpdates: SyncServicesReturnType{
				Additions: sets.NewString("svc1"),
			},
			natUpdates: SyncServicesReturnType{
				Additions: sets.NewString("egress1"),
			},
			expectedLen: 2,
		},
		{
			name: "removals only",
			lbUpdates: SyncServicesReturnType{
				Removals: sets.NewString("svc1"),
			},
			natUpdates: SyncServicesReturnType{
				Removals: sets.NewString("egress1"),
			},
			expectedLen: 2,
		},
		{
			name:        "empty updates",
			lbUpdates:   SyncServicesReturnType{},
			natUpdates:  SyncServicesReturnType{},
			expectedLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(tt.lbUpdates, tt.natUpdates, "sub1", "rg1")
			assert.Equal(t, tt.expectedLen, len(result.Services))
		})
	}
}

// TestRemoveBackendPoolReferenceFromServicesDTO tests removing backend pool references
func TestRemoveBackendPoolReferenceFromServicesDTO(t *testing.T) {
	tests := []struct {
		name        string
		lbUpdates   SyncServicesReturnType
		expectedLen int
	}{
		{
			name: "remove services",
			lbUpdates: SyncServicesReturnType{
				Removals: sets.NewString("svc1", "svc2"),
			},
			expectedLen: 2,
		},
		{
			name: "empty removal list",
			lbUpdates: SyncServicesReturnType{
				Removals: sets.NewString(),
			},
			expectedLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := RemoveBackendPoolReferenceFromServicesDTO(tt.lbUpdates, "sub1", "rg1")
			assert.Equal(t, tt.expectedLen, len(result.Services))
			// Verify all have empty backend pools (backend pool reference removed)
			for _, service := range result.Services {
				assert.Equal(t, 0, len(service.LoadBalancerBackendPools))
				assert.Equal(t, Inbound, service.ServiceType)
			}
		})
	}
}

// TestConfigValidate tests Config.Validate()
func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		shouldError bool
		errorMsg    string
	}{
		{
			name: "valid config",
			config: Config{
				SubscriptionID:             "sub1",
				ResourceGroup:              "rg1",
				Location:                   "eastus",
				VNetName:                   "test-vnet",
				ServiceGatewayResourceName: "sgw",
				ServiceGatewayID:           "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/serviceGateways/sgw",
			},
			shouldError: false,
		},
		{
			name: "missing subscription ID",
			config: Config{
				ResourceGroup:              "rg1",
				Location:                   "eastus",
				ServiceGatewayResourceName: "sgw",
				ServiceGatewayID:           "/id",
			},
			shouldError: true,
			errorMsg:    "SubscriptionID is required",
		},
		{
			name: "missing resource group",
			config: Config{
				SubscriptionID:             "sub1",
				Location:                   "eastus",
				ServiceGatewayResourceName: "sgw",
				ServiceGatewayID:           "/id",
			},
			shouldError: true,
			errorMsg:    "ResourceGroup is required",
		},
		{
			name: "missing location",
			config: Config{
				SubscriptionID:             "sub1",
				ResourceGroup:              "rg1",
				ServiceGatewayResourceName: "sgw",
				ServiceGatewayID:           "/id",
			},
			shouldError: true,
			errorMsg:    "Location is required",
		},
		{
			name: "missing ServiceGatewayResourceName",
			config: Config{
				SubscriptionID:   "sub1",
				ResourceGroup:    "rg1",
				Location:         "eastus",
				ServiceGatewayID: "/id",
			},
			shouldError: true,
			errorMsg:    "ServiceGatewayResourceName is required",
		},
		{
			name: "missing ServiceGatewayID",
			config: Config{
				SubscriptionID:             "sub1",
				ResourceGroup:              "rg1",
				Location:                   "eastus",
				ServiceGatewayResourceName: "sgw",
			},
			shouldError: true,
			errorMsg:    "ServiceGatewayID is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.shouldError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestJSONRoundTrip tests JSON marshaling/unmarshaling for various types
func TestJSONRoundTrip(t *testing.T) {
	t.Run("UpdateAction round trip", func(t *testing.T) {
		original := PartialUpdate
		data, err := json.Marshal(original)
		assert.NoError(t, err)

		var unmarshaled UpdateAction
		err = json.Unmarshal(data, &unmarshaled)
		assert.NoError(t, err)
		assert.Equal(t, original, unmarshaled)
	})

	t.Run("Operation round trip", func(t *testing.T) {
		original := ADD
		data, err := json.Marshal(original)
		assert.NoError(t, err)
		assert.Equal(t, `"ADD"`, string(data))
	})

	t.Run("SyncStatus round trip", func(t *testing.T) {
		original := SUCCESS
		data, err := json.Marshal(original)
		assert.NoError(t, err)
		assert.Equal(t, `"SUCCESS"`, string(data))
	})
}
