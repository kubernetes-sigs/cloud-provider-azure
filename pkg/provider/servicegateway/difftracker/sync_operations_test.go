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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func BenchmarkLocationSync(b *testing.B) {
	for _, count := range []int{100, 1000, 10000} {
		for _, change := range []bool{false, true} {
			b.Run(fmt.Sprintf("addresses=%d/change=%t", count, change), func(b *testing.B) {
				dt := newTestDiffTracker()
				dt.K8sResources.Services.Insert("svc-lb")
				dt.NRPResources.LoadBalancers.Insert("svc-lb")
				for index := 0; index < count; index++ {
					location := fmt.Sprintf("10.0.%d.1", index%10)
					address := fmt.Sprintf("10.224.%d.%d", index/256, index%256)
					node := dt.K8sResources.Nodes[location]
					if node.Pods == nil {
						node = newNode()
					}
					pod := newPod()
					pod.InboundIdentities.Insert("svc-lb")
					node.Pods[address] = pod
					dt.K8sResources.Nodes[location] = node
					ack := dt.NRPResources.Locations[location]
					if ack.Addresses == nil {
						ack.Addresses = make(map[string]NRPAddress)
					}
					ack.Addresses[address] = NRPAddress{Services: utilsets.NewString("svc-lb")}
					dt.NRPResources.Locations[location] = ack
				}
				if change {
					pod := newPod()
					pod.InboundIdentities.Insert("svc-lb")
					dt.K8sResources.Nodes["10.0.0.1"].Pods["10.225.0.1"] = pod
				}
				b.ReportAllocs()
				b.ResetTimer()
				for index := 0; index < b.N; index++ {
					result := dt.GetSyncLocationsAddresses()
					if change && len(result.Locations) != 1 || !change && len(result.Locations) != 0 {
						b.Fatal("unexpected membership delta")
					}
				}
			})
		}
	}
}

func TestLocationSyncKeepsNICPolicyPublicationSeparateFromBaseline(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")

	baseline := dt.GetSyncLocationsAddresses()
	assert.True(t, baseline.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
	assert.Empty(t, dt.pendingLocationUpdates, "the baseline diagnostic calculation does not record a publication")
	assert.Empty(t, dt.applyNICPoliciesToLocationSync(baseline, nil).Locations, "the actual publisher requires NIC preparation")
	assert.True(t, baseline.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"), "NIC processing must not mutate the baseline")
	assert.Empty(t, dt.pendingLocationUpdates)
	assert.Zero(t, cloud.groupWrites)

	prepared := prepareNSGTest(t, dt)
	request := nsgTestLocationSync(dt, prepared)
	assert.True(t, request.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
	assert.True(t, dt.pendingLocationUpdates["10.0.0.1"].Addresses["10.1.0.1"].Services.Has("svc-lb"))
	dt.UpdateLocationsAddresses(request)
	assert.Empty(t, dt.GetSyncLocationsAddresses().Locations)
	assert.Empty(t, dt.pendingLocationUpdates)
}

func TestLocationSyncRevalidatesPolicyAfterBaseline(t *testing.T) {
	dt, cloud, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	prepared := prepareNSGTest(t, dt)
	baseline := dt.GetSyncLocationsAddresses()
	assert.NoError(t, dt.ReconcileInboundService(nsgKubeService("svc-lb", "30.0.0.0/8")))
	writes := cloud.groupWrites

	request := dt.applyNICPoliciesToLocationSync(baseline, prepared)
	assert.Empty(t, request.Locations, "an older preparation must not publish after a policy change")
	assert.Empty(t, dt.pendingLocationUpdates, "only the final request belongs in the journal")
	assert.Equal(t, writes, cloud.groupWrites, "the location step does not perform Azure I/O")

	prepared = prepareNSGTest(t, dt)
	request = dt.applyNICPoliciesToLocationSync(baseline, prepared)
	assert.True(t, request.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
}

func TestLocationSyncIncludesWithdrawalAfterBaseline(t *testing.T) {
	dt, _, _ := newNSGTestTracker(t, nsgKubeService("svc-lb", "20.0.0.0/8"))
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, prepareNSGTest(t, dt)))
	baseline := dt.GetSyncLocationsAddresses()
	assert.Empty(t, baseline.Locations)

	dt.UpdateEndpoints("svc-lb", map[string]string{"10.1.0.1": "10.0.0.1"}, nil)
	request := dt.applyNICPoliciesToLocationSync(baseline, nil)
	assert.Contains(t, request.Locations, "10.0.0.1", "incoming dirty work must be checked after the baseline calculation")
	dt.UpdateLocationsAddresses(request)
	assert.Empty(t, dt.NRPResources.Locations)
	assert.Empty(t, baseline.Locations)
}

func TestLocationSyncRecoveryIncludesUnchangedLocations(t *testing.T) {
	service := nsgKubeService("svc-lb", "")
	service.Annotations = nil
	dt, _, _ := newNSGTestTracker(t, service)
	for location, address := range map[string]string{"10.0.0.1": "10.1.0.1", "10.0.0.2": "10.2.0.1"} {
		addNSGTestBackend(dt, "svc-lb", location, address, "")
	}
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, nil))
	dt.mayHaveNICPolicies, dt.locationRecoveryPending = true, true
	baseline := dt.GetSyncLocationsAddresses()
	assert.Empty(t, baseline.Locations)

	request := dt.applyNICPoliciesToLocationSync(baseline, nil)
	assert.Equal(t, FullUpdate, request.Action)
	assert.Len(t, request.Locations, 2, "a root replacement must retain unchanged acknowledged locations")
	assert.True(t, request.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
	assert.True(t, request.Locations["10.0.0.2"].Addresses["10.2.0.1"].ServiceRef.Has("svc-lb"))
	dt.UpdateLocationsAddresses(request)
	assert.False(t, dt.locationRecoveryPending)
	assert.Empty(t, dt.pendingLocationUpdates)
	assert.Len(t, dt.NRPResources.Locations, 2)
}

func TestLocationSyncCorrectsUnacknowledgedNoOp(t *testing.T) {
	service := nsgKubeService("svc-lb", "")
	service.Annotations = nil
	dt, _, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	attempt := nsgTestLocationSync(dt, nil)
	assert.Contains(t, attempt.Locations, "10.0.0.1")

	dt.UpdateEndpoints("svc-lb", map[string]string{"10.1.0.1": "10.0.0.1"}, nil)
	baseline := dt.GetSyncLocationsAddresses()
	assert.Empty(t, baseline.Locations)
	assert.Empty(t, dt.dirtyNICPolicies)
	request := dt.applyNICPoliciesToLocationSync(baseline, nil)
	assert.Contains(t, request.Locations, "10.0.0.1", "an unacknowledged addition requires correction even when desired equals the old ACK")
	assert.Empty(t, request.Locations["10.0.0.1"].Addresses)
	assert.True(t, dt.pendingLocationUpdates["10.0.0.1"].Addresses["10.1.0.1"].Services.Has("svc-lb"))
	dt.UpdateLocationsAddresses(request)
	assert.Empty(t, dt.pendingLocationUpdates)
	assert.Empty(t, dt.NRPResources.Locations)
}

func TestLocationSyncCorrectsOnlyUnacknowledgedLocations(t *testing.T) {
	service := nsgKubeService("svc-lb", "")
	service.Annotations = nil
	dt, _, _ := newNSGTestTracker(t, service)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.1", "")
	addNSGTestBackend(dt, "svc-lb", "10.0.0.2", "10.2.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, nil))
	dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.9"] = NRPAddress{Services: utilsets.NewString("default-natgw")}

	addNSGTestBackend(dt, "svc-lb", "10.0.0.1", "10.1.0.2", "")
	attempt := nsgTestLocationSync(dt, nil)
	assert.Equal(t, PartialUpdate, attempt.Locations["10.0.0.1"].AddressUpdateAction)
	assert.NotContains(t, attempt.Locations, "10.0.0.2")
	assert.Len(t, dt.pendingLocationUpdates["10.0.0.1"].Addresses, 1)

	dt.UpdateEndpoints("svc-lb", map[string]string{"10.1.0.2": "10.0.0.1"}, nil)
	addNSGTestBackend(dt, "svc-lb", "10.0.0.2", "10.2.0.2", "")
	correction := nsgTestLocationSync(dt, nil)
	assert.Equal(t, PartialUpdate, correction.Action)
	first := correction.Locations["10.0.0.1"]
	assert.Equal(t, FullUpdate, first.AddressUpdateAction)
	assert.Len(t, first.Addresses, 2)
	assert.True(t, first.Addresses["10.1.0.1"].ServiceRef.Has("svc-lb"))
	assert.True(t, first.Addresses["10.1.0.9"].ServiceRef.Equals(utilsets.NewString("default-natgw")))
	assert.NotContains(t, first.Addresses, "10.1.0.2")
	second := correction.Locations["10.0.0.2"]
	assert.Equal(t, PartialUpdate, second.AddressUpdateAction)
	assert.Len(t, second.Addresses, 1)
	assert.True(t, second.Addresses["10.2.0.2"].ServiceRef.Has("svc-lb"))

	dt.UpdateLocationsAddresses(correction)
	assert.Empty(t, dt.pendingLocationUpdates)
	assert.Len(t, dt.NRPResources.Locations["10.0.0.2"].Addresses, 2)
	assert.NotContains(t, dt.NRPResources.Locations["10.0.0.1"].Addresses, "10.1.0.2")
	assert.Empty(t, nsgTestLocationSync(dt, nil).Locations)
}

func TestLocationSyncJournalDoesNotCopyAcknowledgedBindings(t *testing.T) {
	first, second := nsgKubeService("first-lb", ""), nsgKubeService("second-lb", "")
	first.Annotations, second.Annotations = nil, nil
	dt, _, _ := newNSGTestTracker(t, first, second)
	addNSGTestBackend(dt, "first-lb", "10.0.0.1", "10.1.0.1", "")
	dt.UpdateLocationsAddresses(nsgTestLocationSync(dt, nil))
	addNSGTestBackend(dt, "second-lb", "10.0.0.1", "10.1.0.1", "")
	attempt := nsgTestLocationSync(dt, nil)
	assert.True(t, attempt.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Equals(utilsets.NewString("first-lb", "second-lb")))
	assert.True(t, dt.pendingLocationUpdates["10.0.0.1"].Addresses["10.1.0.1"].Services.Equals(utilsets.NewString("second-lb")))
	dt.UpdateLocationsAddresses(attempt)
	assert.Empty(t, dt.pendingLocationUpdates)
}

func TestNICLocationSyncPreservesBYOAndRPDefaultReferences(t *testing.T) {
	const serviceName, byoName = "svc-lb", "egress-natgw"
	const byoID = "/subscriptions/network-sub/resourceGroups/owner-rg/providers/Microsoft.Network/natGateways/egress"
	for _, rootRecovery := range []bool{false, true} {
		for _, reserved := range []string{"default-natgw", "rp-default-special"} {
			t.Run(fmt.Sprintf("rootRecovery=%t/default=%s", rootRecovery, reserved), func(t *testing.T) {
				dt, _, _ := newNSGTestTracker(t, nsgKubeService(serviceName, "20.0.0.0/8"))
				dt.NRPResources.ReservedOutboundServices = utilsets.NewString(reserved)
				dt.setBYONATGatewayID(byoName, byoID)
				addNSGTestBackend(dt, serviceName, "10.0.0.1", "10.1.0.1", byoName)
				dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
					"10.1.0.9": {Services: utilsets.NewString(reserved)},
				}}
				prepared := prepareNSGTest(t, dt)
				attempt := nsgTestLocationSync(dt, prepared)
				assert.True(t, attempt.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Equals(utilsets.NewString(serviceName, byoName)))

				// No ACK: remove only the inbound intent while the selected BYO Pod remains.
				dt.UpdateEndpoints(serviceName, map[string]string{"10.1.0.1": "10.0.0.1"}, nil)
				dt.locationRecoveryPending = rootRecovery
				correction := nsgTestLocationSync(dt, nil)
				assert.Equal(t, FullUpdate, correction.Locations["10.0.0.1"].AddressUpdateAction)
				assert.Len(t, correction.Locations["10.0.0.1"].Addresses, 2)
				assert.True(t, correction.Locations["10.0.0.1"].Addresses["10.1.0.1"].ServiceRef.Equals(utilsets.NewString(byoName)))
				assert.True(t, correction.Locations["10.0.0.1"].Addresses["10.1.0.9"].ServiceRef.Equals(utilsets.NewString(reserved)))
				if rootRecovery {
					assert.Equal(t, FullUpdate, correction.Action)
				} else {
					assert.Equal(t, PartialUpdate, correction.Action)
				}

				dt.UpdateLocationsAddresses(correction)
				assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.1"].Services.Equals(utilsets.NewString(byoName)))
				assert.True(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.1.0.9"].Services.Equals(utilsets.NewString(reserved)))
				assert.Equal(t, byoID, dt.byoNATGatewayID(byoName), "NIC reconciliation must not replace the retained owner target")
				assert.Empty(t, dt.pendingLocationUpdates)
				assert.Empty(t, nsgTestLocationSync(dt, nil).Locations)
			})
		}
	}
}

func TestGetSyncLoadBalancerServices(t *testing.T) {
	dt := &DiffTracker{
		K8sResources: K8sState{
			Services: utilsets.NewString("service1", "service2", "service3"),
		},
		NRPResources: NRPState{
			LoadBalancers: utilsets.NewString("service2", "service3", "service4"),
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
							InboundIdentities:      utilsets.NewString("service1"),
							PublicOutboundIdentity: "public1",
						},
					},
				},
			},
		},
		NRPResources: NRPState{
			LoadBalancers: utilsets.NewString("service1"), // Service must exist in NRP to pass filtering
			NATGateways:   utilsets.NewString("public1"),  // NAT Gateway must exist in NRP to pass filtering
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
		{
			name:              "default addition is excluded",
			k8sEgresses:       []string{"DEFAULT-NATGW", "ordinary-natgw"},
			expectedAdditions: []string{"ordinary-natgw"},
		},
		{
			name:             "default removal is excluded",
			nrpNATGateways:   []string{"DEFAULT-NATGW", "ordinary-natgw"},
			expectedRemovals: []string{"ordinary-natgw"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Initialize DiffTracker with the test case data
			dt := &DiffTracker{
				K8sResources: K8sState{
					Egresses: utilsets.NewString(tt.k8sEgresses...),
				},
				NRPResources: NRPState{
					NATGateways: utilsets.NewString(tt.nrpNATGateways...),
				},
			}
			// Call the function being tested
			result := dt.GetSyncNRPNATGateways()
			assert.ElementsMatch(t, utilsets.NewString(tt.k8sEgresses...).UnsortedList(), dt.K8sResources.Egresses.UnsortedList())
			assert.ElementsMatch(t, utilsets.NewString(tt.nrpNATGateways...).UnsortedList(), dt.NRPResources.NATGateways.UnsortedList())

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
				NRPResources: NRPState{
					LoadBalancers: utilsets.NewString("service1"),
					NATGateways:   utilsets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: utilsets.NewString("service1"),
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
				NRPResources: NRPState{
					LoadBalancers: utilsets.NewString("service1"),
					NATGateways:   utilsets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: utilsets.NewString("service1"),
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
				NRPResources: NRPState{
					LoadBalancers: utilsets.NewString("service1"),
					NATGateways:   utilsets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: utilsets.NewString("service1"),
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
					Services: utilsets.NewString("service1", "service2"), // service2 must also exist in K8s to avoid removal
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
				NRPResources: NRPState{
					LoadBalancers: utilsets.NewString("service1", "service2"), // Both services exist in NRP (pass filtering)
					NATGateways:   utilsets.NewString("egress1"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: utilsets.NewString("service1"), // Only service1 in location, service2 needs sync
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
				NRPResources: NRPState{
					LoadBalancers: utilsets.NewString("service1", "service2"),
					NATGateways:   utilsets.NewString("egress1", "egress2"),
					Locations: map[string]NRPLocation{
						"node1": {
							Addresses: map[string]NRPAddress{
								"10.0.0.1": {
									Services: utilsets.NewString("service1"),
								},
								"10.0.0.2": {
									Services: utilsets.NewString("service2"),
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
			LoadBalancers: utilsets.NewString("service1", "service2"),
			NATGateways:   utilsets.NewString(),
			Locations: map[string]NRPLocation{
				"node1": {
					Addresses: map[string]NRPAddress{
						"10.0.0.1": {Services: utilsets.NewString("service1")},
						"10.0.0.2": {Services: utilsets.NewString("service2")},
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
	serviceName := "svc-last-pod"
	const node = "10.0.0.1"
	const podIP = "10.244.0.5"

	// Service is created and known to NRP. NRP already reflects the single pod on this node.
	dt.pendingServiceOps[serviceName] = &ServiceOperationState{
		ServiceName: serviceName,
		Config:      NewInboundServiceConfig(serviceName, makeInboundConfig(80)),
		State:       StateCreated,
	}
	dt.NRPResources.LoadBalancers.Insert(serviceName)
	dt.NRPResources.Locations[node] = NRPLocation{
		Addresses: map[string]NRPAddress{
			podIP: {Services: utilsets.NewString(serviceName)},
		},
	}

	// Step 1: simulate the EndpointSlice that originally added the pod, so K8s state has the
	// node entry. We drive it through the production entry point so the node is created by a real
	// informer flow rather than synthetic state stitching.
	errs := dt.UpdateK8sEndpoints(UpdateK8sEndpointsInputType{
		InboundIdentity: serviceName,
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
		InboundIdentity: serviceName,
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
