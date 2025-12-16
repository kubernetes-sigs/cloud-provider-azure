/*
Copyright 2024 The Kubernetes Authors.

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
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// Helper function to create a test DiffTracker
func newTestDiffTracker() *DiffTracker {
	return &DiffTracker{
		NRPResources: NRP_State{
			LoadBalancers: utilsets.NewString(),
			NATGateways:   utilsets.NewString(),
			Locations:     make(map[string]NRPLocation),
		},
		K8sResources: K8s_State{
			Services: utilsets.NewString(),
			Egresses: utilsets.NewString(),
			Nodes:    make(map[string]Node),
		},
		pendingServiceOps:       make(map[string]*ServiceOperationState),
		pendingEndpoints:        make(map[string][]PendingEndpointUpdate),
		pendingPods:             make(map[string][]PendingPodUpdate),
		pendingDeletions:        make(map[string]*PendingDeletion),
		serviceUpdaterTrigger:   make(chan bool, 1),
		locationsUpdaterTrigger: make(chan bool, 1),
	}
}

// TestEngineAddService_NewService tests adding a new service that doesn't exist in NRP
func TestEngineAddService_NewService(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-1"

	// Execute
	dt.AddService(serviceUID, true)

	// Verify service is tracked
	opState, exists := dt.pendingServiceOps[serviceUID]
	assert.True(t, exists, "Service should be tracked")
	assert.Equal(t, serviceUID, opState.ServiceUID)
	assert.True(t, opState.IsInbound)
	assert.Equal(t, StateNotStarted, opState.State)
	assert.Equal(t, 0, opState.RetryCount)

	// Verify trigger was sent
	select {
	case <-dt.serviceUpdaterTrigger:
		// Expected
	default:
		t.Error("Expected ServiceUpdater trigger")
	}
}

// TestEngineAddService_ExistsInNRP tests adding a service that already exists in NRP
func TestEngineAddService_ExistsInNRP(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-2"

	// Setup: service already exists in NRP
	dt.NRPResources.LoadBalancers.Insert(serviceUID)

	// Execute
	dt.AddService(serviceUID, true)

	// Verify service is NOT tracked (since it exists)
	_, exists := dt.pendingServiceOps[serviceUID]
	assert.False(t, exists, "Service should not be tracked if it exists in NRP")

	// Verify no trigger
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Error("Unexpected ServiceUpdater trigger")
	default:
		// Expected
	}
}

// TestEngineAddService_AlreadyTracked tests adding a service that's already being created
func TestEngineAddService_AlreadyTracked(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-3"

	// Setup: service already tracked
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		IsInbound:  true,
		State:      StateCreationInProgress,
		RetryCount: 0,
	}

	// Execute
	dt.AddService(serviceUID, true)

	// Verify state unchanged
	opState := dt.pendingServiceOps[serviceUID]
	assert.Equal(t, StateCreationInProgress, opState.State)

	// Verify no trigger
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Error("Unexpected ServiceUpdater trigger")
	default:
		// Expected
	}
}

// TestEngineDeleteService_NoLocations tests deleting a service without locations
func TestEngineDeleteService_NoLocations(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-4"

	// Setup: service exists in NRP
	dt.NRPResources.LoadBalancers.Insert(serviceUID)

	// Execute
	dt.DeleteService(serviceUID, true)

	// Verify service is tracked for deletion
	opState, exists := dt.pendingServiceOps[serviceUID]
	assert.True(t, exists, "Service should be tracked for deletion")
	assert.Equal(t, StateDeletionInProgress, opState.State)

	// Verify trigger was sent
	select {
	case <-dt.serviceUpdaterTrigger:
		// Expected
	default:
		t.Error("Expected ServiceUpdater trigger")
	}
}

// TestEngineDeleteService_WithLocations tests deleting a service that has locations
func TestEngineDeleteService_WithLocations(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-5"

	// Setup: service exists in NRP
	dt.NRPResources.NATGateways.Insert(serviceUID)

	// Setup: Add a location that contains this service
	locationKey := "node1-10.0.0.1"
	dt.NRPResources.Locations[locationKey] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.0.0.1": {
				Services: utilsets.NewString(serviceUID),
			},
		},
	}

	// Execute
	dt.DeleteService(serviceUID, false)

	// Verify service is in pending deletions (not immediate deletion)
	pendingDel, exists := dt.pendingDeletions[serviceUID]
	assert.True(t, exists, "Service should be in pending deletions")
	if exists {
		assert.Equal(t, serviceUID, pendingDel.ServiceUID)
		assert.False(t, pendingDel.IsInbound)
	}

	// Verify no immediate trigger
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Error("Unexpected ServiceUpdater trigger - should wait for locations to clear")
	default:
		// Expected
	}
}

// TestEngineUpdateEndpoints_ServiceExists tests endpoint updates for existing service
// Note: This test is skipped as it requires mocking UpdateK8sEndpoints which has complex dependencies
func TestEngineUpdateEndpoints_ServiceExists(t *testing.T) {
	t.Skip("Requires mocking UpdateK8sEndpoints - covered by integration tests")
}

// TestEngineUpdateEndpoints_ServiceCreating tests endpoint buffering during service creation
func TestEngineUpdateEndpoints_ServiceCreating(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-7"

	// Setup: service is being created
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		IsInbound:  true,
		State:      StateCreationInProgress,
		RetryCount: 0,
	}

	// Execute
	oldEndpoints := map[string]string{}
	newEndpoints := map[string]string{"10.0.0.1": "node1"}
	dt.UpdateEndpoints(serviceUID, oldEndpoints, newEndpoints)

	// Verify endpoints were buffered
	buffered, exists := dt.pendingEndpoints[serviceUID]
	assert.True(t, exists, "Endpoints should be buffered")
	assert.Greater(t, len(buffered), 0, "Should have buffered updates")

	// Verify no immediate trigger
	select {
	case <-dt.locationsUpdaterTrigger:
		t.Error("Unexpected LocationsUpdater trigger - endpoints should be buffered")
	default:
		// Expected
	}
}

// TestEngineOnServiceCreationComplete_Success tests successful service creation callback
// Note: This test is skipped as promotion requires mocking UpdateK8sEndpoints/UpdateK8sPod
func TestEngineOnServiceCreationComplete_Success(t *testing.T) {
	t.Skip("Requires mocking UpdateK8sEndpoints/UpdateK8sPod - covered by integration tests")
}

// TestEngineOnServiceCreationComplete_Failure tests failed service creation callback
func TestEngineOnServiceCreationComplete_Failure(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-9"

	// Setup: service is being created
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		IsInbound:  true,
		State:      StateCreationInProgress,
		RetryCount: 0,
	}

	// Execute
	dt.OnServiceCreationComplete(serviceUID, false, nil)

	// Verify retry count increased
	opState, exists := dt.pendingServiceOps[serviceUID]
	assert.True(t, exists, "Failed service should remain in pending")
	assert.Greater(t, opState.RetryCount, 0, "Retry count should increase")
}

// TestEngineAddPod_ServiceExists tests adding a pod to an existing service
// Note: This test is skipped as it requires mocking UpdatePod which has complex dependencies
func TestEngineAddPod_ServiceExists(t *testing.T) {
	t.Skip("Requires mocking UpdatePod - covered by integration tests")
}

// TestEngineAddPod_ServiceCreating tests pod buffering during service creation
func TestEngineAddPod_ServiceCreating(t *testing.T) {
	dt := newTestDiffTracker()
	egressUID := "egress-2"

	// Setup: service is being created
	dt.pendingServiceOps[egressUID] = &ServiceOperationState{
		ServiceUID: egressUID,
		IsInbound:  false,
		State:      StateCreationInProgress,
		RetryCount: 0,
	}

	// Execute
	dt.AddPod(egressUID, "ns1/pod2", "node2", "10.0.0.2")

	// Verify pod was buffered
	buffered, exists := dt.pendingPods[egressUID]
	assert.True(t, exists, "Pods should be buffered")
	assert.Greater(t, len(buffered), 0, "Should have buffered pod updates")

	// Verify no immediate trigger
	select {
	case <-dt.locationsUpdaterTrigger:
		t.Error("Unexpected LocationsUpdater trigger - pods should be buffered")
	default:
		// Expected
	}
}
