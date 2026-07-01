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
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

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
	diffTracker, err := New(log.Noop(), K8sResources, NRPResources, config, mockFactory, mockKubeClient)
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
	_, err := New(log.Noop(), K8sState{}, NRPState{}, Config{}, mockFactory, mockKubeClient)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "New")

	// Nil networkClientFactory.
	_, err = New(log.Noop(), K8sState{}, NRPState{}, validTestConfig(), nil, mockKubeClient)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "networkClientFactory must not be nil")

	// Nil kubeClient.
	_, err = New(log.Noop(), K8sState{}, NRPState{}, validTestConfig(), mockFactory, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "kubeClient must not be nil")

	// Uninitialized state field -> error out instead of silently initializing.
	k8sMissingServices := emptyK8sState()
	k8sMissingServices.Services = nil
	_, err = New(log.Noop(), k8sMissingServices, emptyNRPState(), validTestConfig(), mockFactory, mockKubeClient)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "k8s.Services must not be nil")

	nrpMissingLocations := emptyNRPState()
	nrpMissingLocations.Locations = nil
	_, err = New(log.Noop(), emptyK8sState(), nrpMissingLocations, validTestConfig(), mockFactory, mockKubeClient)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nrp.Locations must not be nil")

	// Valid call with fully initialized (empty) states.
	dt, err := New(log.Noop(), emptyK8sState(), emptyNRPState(), validTestConfig(), mockFactory, mockKubeClient)
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

// TestEnqueueK8sResourceOperationErrors covers the empty-ID and invalid-operation
// branches of enqueueK8sResourceOperation (via the public wrappers).
