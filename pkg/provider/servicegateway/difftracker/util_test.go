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
		{"Add operation", Add, "Add"},
		{"Remove operation", Remove, "Remove"},
		{"Update operation", Update, "Update"},
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

// TestEnumStringOutOfRange verifies String() does not panic for out-of-range enum
// values and returns a descriptive fallback instead.
func TestEnumStringOutOfRange(t *testing.T) {
	assert.Equal(t, "Operation(99)", Operation(99).String())
	assert.Equal(t, "Operation(-1)", Operation(-1).String())
	assert.Equal(t, "UpdateAction(99)", UpdateAction(99).String())
	assert.Equal(t, "SyncStatus(99)", SyncStatus(99).String())
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
		{"AlreadyInSync", AlreadyInSync, "AlreadyInSync"},
		{"Success", Success, "Success"},
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

// TestDeepEqual tests DiffTracker.deepEqualLocked()
func TestDeepEqual(t *testing.T) {
	tests := []struct {
		name     string
		dt       *DiffTracker
		expected bool
	}{
		{
			name: "in sync - matching services and load balancers",
			dt: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("svc1", "svc2"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("svc1", "svc2"),
					NATGateways:   sets.NewString(),
					Locations:     map[string]NRPLocation{},
				},
			},
			expected: true,
		},
		{
			name: "not in sync - service count mismatch",
			dt: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("svc1"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("svc1", "svc2"),
					NATGateways:   sets.NewString(),
					Locations:     map[string]NRPLocation{},
				},
			},
			expected: false,
		},
		{
			name: "not in sync - service name mismatch",
			dt: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("svc1"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString("svc2"),
					NATGateways:   sets.NewString(),
					Locations:     map[string]NRPLocation{},
				},
			},
			expected: false,
		},
		{
			name: "in sync - matching egresses and NAT gateways",
			dt: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString(),
					Egresses: sets.NewString("egress1", "egress2"),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRPState{
					LoadBalancers: sets.NewString(),
					NATGateways:   sets.NewString("egress1", "egress2"),
					Locations:     map[string]NRPLocation{},
				},
			},
			expected: true,
		},
		{
			name: "not in sync - egress count mismatch",
			dt: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString(),
					Egresses: sets.NewString("egress1"),
					Nodes:    map[string]Node{},
				},
				NRPResources: NRPState{
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
			assert.Equal(t, tt.expected, tt.dt.deepEqualLocked())
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
		dt1      *DiffTracker
		dt2      *DiffTracker
		expected bool
	}{
		{
			name: "equal diff trackers",
			dt1: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("svc1"),
					Egresses: sets.NewString("egress1"),
					Nodes:    map[string]Node{},
				},
			},
			dt2: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("svc1"),
					Egresses: sets.NewString("egress1"),
					Nodes:    map[string]Node{},
				},
			},
			expected: true,
		},
		{
			name: "different services",
			dt1: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("svc1"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
			},
			dt2: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString("svc2"),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
			},
			expected: false,
		},
		{
			name: "different egresses",
			dt1: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString(),
					Egresses: sets.NewString("egress1"),
					Nodes:    map[string]Node{},
				},
			},
			dt2: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString(),
					Egresses: sets.NewString("egress2"),
					Nodes:    map[string]Node{},
				},
			},
			expected: false,
		},
		{
			name: "different node count",
			dt1: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString(),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{"node1": {}},
				},
			},
			dt2: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString(),
					Egresses: sets.NewString(),
					Nodes:    map[string]Node{},
				},
			},
			expected: false,
		},
		{
			name: "different pod count in node",
			dt1: &DiffTracker{
				K8sResources: K8sState{
					Services: sets.NewString(),
					Egresses: sets.NewString(),
					Nodes: map[string]Node{
						"node1": {Pods: map[string]Pod{"pod1": {}}},
					},
				},
			},
			dt2: &DiffTracker{
				K8sResources: K8sState{
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
			assert.Equal(t, tt.expected, tt.dt1.Equals(tt.dt2))
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
		original := Add
		data, err := json.Marshal(original)
		assert.NoError(t, err)
		assert.Equal(t, `"Add"`, string(data))
	})

	t.Run("SyncStatus round trip", func(t *testing.T) {
		original := Success
		data, err := json.Marshal(original)
		assert.NoError(t, err)
		assert.Equal(t, `"Success"`, string(data))
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

	assert.True(t, inSync().deepEqualLocked())

	// LoadBalancer present in NRP but not in K8s Services (reverse-direction check).
	d := inSync()
	d.NRPResources.LoadBalancers = sets.NewString("svc1", "extra")
	d.K8sResources.Services = sets.NewString("svc1", "different")
	assert.False(t, d.deepEqualLocked())

	// Egress name mismatch (reverse direction): lengths equal (1==1) but names
	// differ -> mismatch.
	d = inSync()
	d.K8sResources.Egresses = sets.NewString("egr2")
	d.NRPResources.NATGateways = sets.NewString("egr2x")
	assert.False(t, d.deepEqualLocked())

	// Nodes vs Locations length mismatch.
	d = inSync()
	d.NRPResources.Locations["node2"] = NRPLocation{Addresses: map[string]NRPAddress{}}
	assert.False(t, d.deepEqualLocked())

	// Node missing in Locations (same count, different key).
	d = inSync()
	delete(d.NRPResources.Locations, "node1")
	d.NRPResources.Locations["nodeX"] = NRPLocation{Addresses: map[string]NRPAddress{
		"10.0.0.1": {Services: sets.NewString("svc1", "egr1")},
	}}
	assert.False(t, d.deepEqualLocked())

	// Pods vs Addresses length mismatch.
	d = inSync()
	loc := d.NRPResources.Locations["node1"]
	loc.Addresses["10.0.0.2"] = NRPAddress{Services: sets.NewString("svc1")}
	assert.False(t, d.deepEqualLocked())

	// Pod missing in Addresses (same count, different key).
	d = inSync()
	loc = d.NRPResources.Locations["node1"]
	delete(loc.Addresses, "10.0.0.1")
	loc.Addresses["10.0.0.9"] = NRPAddress{Services: sets.NewString("svc1", "egr1")}
	assert.False(t, d.deepEqualLocked())

	// Combined identities length mismatch.
	d = inSync()
	loc = d.NRPResources.Locations["node1"]
	loc.Addresses["10.0.0.1"] = NRPAddress{Services: sets.NewString("svc1")}
	assert.False(t, d.deepEqualLocked())

	// Identity not found in Services (same count, different identity).
	d = inSync()
	loc = d.NRPResources.Locations["node1"]
	loc.Addresses["10.0.0.1"] = NRPAddress{Services: sets.NewString("svc1", "egrX")}
	assert.False(t, d.deepEqualLocked())
}

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
func TestOperation_String(t *testing.T) {
	assert.Equal(t, "Add", Add.String())
	assert.Equal(t, "Remove", Remove.String())
	assert.Equal(t, "Update", Update.String())
}
