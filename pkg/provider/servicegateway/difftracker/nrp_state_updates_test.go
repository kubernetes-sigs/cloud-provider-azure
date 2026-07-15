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
