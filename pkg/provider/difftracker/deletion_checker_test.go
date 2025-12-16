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

// TestCheckPendingDeletions_NoLocations tests deletion when service has no locations
func TestCheckPendingDeletions_NoLocations(t *testing.T) {
	dt := newTestDiffTracker()

	// Setup: Service marked for deletion with no locations in NRP
	serviceUID := "service-1"
	dt.pendingDeletions[serviceUID] = &PendingDeletion{
		ServiceUID: serviceUID,
		IsInbound:  true,
	}
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		IsInbound:  true,
		State:      StateDeletionPending,
		RetryCount: 0,
	}

	// Call CheckPendingDeletions
	dt.CheckPendingDeletions()

	// Verify: Service should be moved to DeletionInProgress
	dt.mu.Lock()
	opState, exists := dt.pendingServiceOps[serviceUID]
	dt.mu.Unlock()

	assert.True(t, exists, "Service should still be in pendingServiceOps")
	assert.Equal(t, StateDeletionInProgress, opState.State, "Service state should be DeletionInProgress")

	// Verify: Service removed from pendingDeletions
	dt.mu.Lock()
	_, stillPending := dt.pendingDeletions[serviceUID]
	dt.mu.Unlock()

	assert.False(t, stillPending, "Service should be removed from pendingDeletions")

	// Verify: ServiceUpdater trigger was sent (channel should have value or be full)
	select {
	case <-dt.serviceUpdaterTrigger:
		// Expected - trigger was sent
	default:
		t.Error("ServiceUpdater trigger should have been sent")
	}
}

// TestCheckPendingDeletions_WithLocations tests deletion blocked by existing locations
func TestCheckPendingDeletions_WithLocations(t *testing.T) {
	dt := newTestDiffTracker()

	// Setup: Service marked for deletion but still has locations in NRP
	serviceUID := "service-2"
	dt.pendingDeletions[serviceUID] = &PendingDeletion{
		ServiceUID: serviceUID,
		IsInbound:  true,
	}
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		IsInbound:  true,
		State:      StateDeletionPending,
		RetryCount: 0,
	}

	// Add locations that reference this service
	dt.NRPResources.Locations["eastus"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.0.1.5": {
				Services: utilsets.NewString(serviceUID),
			},
		},
	}

	// Call CheckPendingDeletions
	dt.CheckPendingDeletions()

	// Verify: Service should remain in DeletionPending state (not moved to DeletionInProgress)
	dt.mu.Lock()
	opState, exists := dt.pendingServiceOps[serviceUID]
	dt.mu.Unlock()

	assert.True(t, exists, "Service should still be in pendingServiceOps")
	assert.Equal(t, StateDeletionPending, opState.State, "Service state should remain DeletionPending")

	// Verify: Service remains in pendingDeletions
	dt.mu.Lock()
	_, stillPending := dt.pendingDeletions[serviceUID]
	dt.mu.Unlock()

	assert.True(t, stillPending, "Service should remain in pendingDeletions")

	// Verify: ServiceUpdater trigger was NOT sent
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Error("ServiceUpdater trigger should NOT have been sent while locations exist")
	default:
		// Expected - no trigger sent
	}
}

// TestCheckPendingDeletions_MultipleServices tests handling multiple pending deletions
func TestCheckPendingDeletions_MultipleServices(t *testing.T) {
	dt := newTestDiffTracker()

	// Setup: Multiple services - some with locations, some without
	service1 := "service-ready"
	service2 := "service-blocked"
	service3 := "service-ready2"

	// Service 1: No locations, ready for deletion
	dt.pendingDeletions[service1] = &PendingDeletion{ServiceUID: service1, IsInbound: true}
	dt.pendingServiceOps[service1] = &ServiceOperationState{
		ServiceUID: service1,
		IsInbound:  true,
		State:      StateDeletionPending,
		RetryCount: 0,
	}

	// Service 2: Has locations, blocked from deletion
	dt.pendingDeletions[service2] = &PendingDeletion{ServiceUID: service2, IsInbound: true}
	dt.pendingServiceOps[service2] = &ServiceOperationState{
		ServiceUID: service2,
		IsInbound:  true,
		State:      StateDeletionPending,
		RetryCount: 0,
	}
	dt.NRPResources.Locations["eastus"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.0.1.5": {
				Services: utilsets.NewString(service2),
			},
		},
	}

	// Service 3: No locations, ready for deletion
	dt.pendingDeletions[service3] = &PendingDeletion{ServiceUID: service3, IsInbound: false}
	dt.pendingServiceOps[service3] = &ServiceOperationState{
		ServiceUID: service3,
		IsInbound:  false,
		State:      StateDeletionPending,
		RetryCount: 0,
	}

	// Call CheckPendingDeletions
	dt.CheckPendingDeletions()

	// Verify: Service 1 should be moved to DeletionInProgress
	dt.mu.Lock()
	op1, exists1 := dt.pendingServiceOps[service1]
	dt.mu.Unlock()
	assert.True(t, exists1, "Service 1 should be in pendingServiceOps")
	assert.Equal(t, StateDeletionInProgress, op1.State, "Service 1 should be in DeletionInProgress")

	dt.mu.Lock()
	_, pending1 := dt.pendingDeletions[service1]
	dt.mu.Unlock()
	assert.False(t, pending1, "Service 1 should be removed from pendingDeletions")

	// Verify: Service 2 should remain in DeletionPending
	dt.mu.Lock()
	op2, exists2 := dt.pendingServiceOps[service2]
	dt.mu.Unlock()
	assert.True(t, exists2, "Service 2 should be in pendingServiceOps")
	assert.Equal(t, StateDeletionPending, op2.State, "Service 2 should remain in DeletionPending")

	dt.mu.Lock()
	_, pending2 := dt.pendingDeletions[service2]
	dt.mu.Unlock()
	assert.True(t, pending2, "Service 2 should remain in pendingDeletions")

	// Verify: Service 3 should be moved to DeletionInProgress
	dt.mu.Lock()
	op3, exists3 := dt.pendingServiceOps[service3]
	dt.mu.Unlock()
	assert.True(t, exists3, "Service 3 should be in pendingServiceOps")
	assert.Equal(t, StateDeletionInProgress, op3.State, "Service 3 should be in DeletionInProgress")

	dt.mu.Lock()
	_, pending3 := dt.pendingDeletions[service3]
	dt.mu.Unlock()
	assert.False(t, pending3, "Service 3 should be removed from pendingDeletions")
}

// TestCheckPendingDeletions_EmptyMap tests behavior with no pending deletions
func TestCheckPendingDeletions_EmptyMap(t *testing.T) {
	dt := newTestDiffTracker()

	// No pending deletions
	assert.Empty(t, dt.pendingDeletions, "pendingDeletions should be empty")

	// Call CheckPendingDeletions - should return early without errors
	dt.CheckPendingDeletions()

	// Verify: No trigger sent
	select {
	case <-dt.serviceUpdaterTrigger:
		t.Error("ServiceUpdater trigger should NOT have been sent with no pending deletions")
	default:
		// Expected - no trigger sent
	}
}

// TestCheckPendingDeletions_MissingFromPendingServiceOps tests when service is in pendingDeletions but not pendingServiceOps
func TestCheckPendingDeletions_MissingFromPendingServiceOps(t *testing.T) {
	dt := newTestDiffTracker()

	// Setup: Service in pendingDeletions but NOT in pendingServiceOps
	serviceUID := "orphaned-service"
	dt.pendingDeletions[serviceUID] = &PendingDeletion{
		ServiceUID: serviceUID,
		IsInbound:  true,
	}

	// Call CheckPendingDeletions
	dt.CheckPendingDeletions()

	// Verify: Service should be added to pendingServiceOps with DeletionInProgress state
	dt.mu.Lock()
	opState, exists := dt.pendingServiceOps[serviceUID]
	dt.mu.Unlock()

	assert.True(t, exists, "Service should be added to pendingServiceOps")
	assert.Equal(t, StateDeletionInProgress, opState.State, "Service state should be DeletionInProgress")
	assert.Equal(t, serviceUID, opState.ServiceUID, "ServiceUID should match")
	assert.True(t, opState.IsInbound, "IsInbound should be true")

	// Verify: Service removed from pendingDeletions
	dt.mu.Lock()
	_, stillPending := dt.pendingDeletions[serviceUID]
	dt.mu.Unlock()

	assert.False(t, stillPending, "Service should be removed from pendingDeletions")
}

// TestServiceHasLocationsInNRP tests the helper function for checking location references
func TestServiceHasLocationsInNRP(t *testing.T) {
	dt := newTestDiffTracker()

	serviceUID := "service-test"

	// Initially no locations
	dt.mu.Lock()
	hasLocs := dt.serviceHasLocationsInNRP(serviceUID)
	dt.mu.Unlock()
	assert.False(t, hasLocs, "Service should have no locations initially")

	// Add a location that references the service
	dt.NRPResources.Locations["westus"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.0.2.10": {
				Services: utilsets.NewString(serviceUID, "other-service"),
			},
		},
	}

	dt.mu.Lock()
	hasLocs = dt.serviceHasLocationsInNRP(serviceUID)
	dt.mu.Unlock()
	assert.True(t, hasLocs, "Service should have locations")

	// Add another location without the service
	dt.NRPResources.Locations["eastus"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.0.1.20": {
				Services: utilsets.NewString("different-service"),
			},
		},
	}

	dt.mu.Lock()
	hasLocs = dt.serviceHasLocationsInNRP(serviceUID)
	dt.mu.Unlock()
	assert.True(t, hasLocs, "Service should still have locations (from westus)")

	// Remove service from all locations
	dt.NRPResources.Locations["westus"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.0.2.10": {
				Services: utilsets.NewString("other-service"),
			},
		},
	}

	dt.mu.Lock()
	hasLocs = dt.serviceHasLocationsInNRP(serviceUID)
	dt.mu.Unlock()
	assert.False(t, hasLocs, "Service should have no locations after removal")
}

// TestCheckPendingDeletions_LocationsClearedAfterCheck tests the flow of locations being cleared
func TestCheckPendingDeletions_LocationsClearedAfterCheck(t *testing.T) {
	dt := newTestDiffTracker()

	serviceUID := "service-flow"

	// Setup: Service with locations
	dt.pendingDeletions[serviceUID] = &PendingDeletion{ServiceUID: serviceUID, IsInbound: true}
	dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
		ServiceUID: serviceUID,
		IsInbound:  true,
		State:      StateDeletionPending,
		RetryCount: 0,
	}
	dt.NRPResources.Locations["eastus"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.0.1.5": {
				Services: utilsets.NewString(serviceUID),
			},
		},
	}

	// First check - should not proceed with deletion
	dt.CheckPendingDeletions()

	dt.mu.Lock()
	op := dt.pendingServiceOps[serviceUID]
	_, pending := dt.pendingDeletions[serviceUID]
	dt.mu.Unlock()

	assert.Equal(t, StateDeletionPending, op.State, "Should remain in DeletionPending")
	assert.True(t, pending, "Should remain in pendingDeletions")

	// Clear locations (simulating LocationsUpdater sync)
	dt.NRPResources.Locations["eastus"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			// Address removed
		},
	}

	// Drain any existing trigger
	select {
	case <-dt.serviceUpdaterTrigger:
	default:
	}

	// Second check - should now proceed with deletion
	dt.CheckPendingDeletions()

	dt.mu.Lock()
	op2 := dt.pendingServiceOps[serviceUID]
	_, pending2 := dt.pendingDeletions[serviceUID]
	dt.mu.Unlock()

	assert.Equal(t, StateDeletionInProgress, op2.State, "Should move to DeletionInProgress")
	assert.False(t, pending2, "Should be removed from pendingDeletions")

	// Verify trigger sent
	select {
	case <-dt.serviceUpdaterTrigger:
		// Expected
	default:
		t.Error("ServiceUpdater trigger should have been sent")
	}
}
