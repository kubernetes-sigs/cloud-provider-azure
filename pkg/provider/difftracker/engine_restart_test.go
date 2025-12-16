package difftracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEngineUpdateEndpoints_AfterRestart tests that UpdateEndpoints works correctly
// when the service exists in NRP but pendingServiceOps is empty (e.g., after restart)
func TestEngineUpdateEndpoints_AfterRestart(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-after-restart"

	// Simulate restart scenario:
	// 1. Service exists in NRP (LoadBalancers)
	dt.NRPResources.LoadBalancers.Insert(serviceUID)

	// 2. pendingServiceOps is empty (lost on restart)
	// This is already the case in newTestDiffTracker()

	// 3. UpdateEndpoints is called
	oldEndpoints := map[string]string{}
	newEndpoints := map[string]string{
		"10.0.0.1": "node1",
		"10.0.0.2": "node2",
	}

	dt.UpdateEndpoints(serviceUID, oldEndpoints, newEndpoints)

	// Verify endpoints were added to K8s state (not buffered)
	assert.Equal(t, 0, len(dt.pendingEndpoints), "Endpoints should not be buffered")
	assert.Equal(t, 2, len(dt.K8sResources.Nodes), "Should have 2 nodes")

	// Verify node1 has the pod with inbound identity
	node1, exists := dt.K8sResources.Nodes["node1"]
	assert.True(t, exists, "node1 should exist")
	pod1, exists := node1.Pods["10.0.0.1"]
	assert.True(t, exists, "pod 10.0.0.1 should exist")
	assert.True(t, pod1.InboundIdentities.Has(serviceUID), "pod should have inbound identity")
}

// TestEngineAddPod_AfterRestart tests that AddPod works correctly
// when the service exists in NRP but pendingServiceOps is empty (e.g., after restart)
func TestEngineAddPod_AfterRestart(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "natgateway-after-restart"
	podKey := "pod1"
	location := "node1"
	address := "10.0.0.1"

	// Simulate restart scenario:
	// 1. NAT Gateway exists in NRP
	dt.NRPResources.NATGateways.Insert(serviceUID)

	// 2. pendingServiceOps is empty (lost on restart)
	// This is already the case in newTestDiffTracker()

	// 3. AddPod is called
	dt.AddPod(serviceUID, podKey, location, address)

	// Verify pod was added to K8s state (not buffered)
	assert.Equal(t, 0, len(dt.pendingPods), "Pods should not be buffered")
	assert.Equal(t, 1, len(dt.K8sResources.Nodes), "Should have 1 node")

	// Verify node has the pod with outbound identity
	node, exists := dt.K8sResources.Nodes[location]
	assert.True(t, exists, "node should exist")
	pod, exists := node.Pods[address]
	assert.True(t, exists, "pod should exist")
	assert.Equal(t, serviceUID, pod.PublicOutboundIdentity, "pod should have outbound identity")

	// Verify counter was updated
	val, ok := dt.LocalServiceNameToNRPServiceMap.Load(serviceUID)
	assert.True(t, ok, "service should be in counter map")
	assert.Equal(t, 1, val.(int), "counter should be 1")
}

// TestEngineDeleteService_AfterRestart tests that DeleteService works correctly
// when the service exists in NRP but pendingServiceOps is empty (e.g., after restart)
func TestEngineDeleteService_AfterRestart(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-delete-after-restart"

	// Simulate restart scenario:
	// 1. Service exists in NRP (LoadBalancers)
	dt.NRPResources.LoadBalancers.Insert(serviceUID)

	// 2. pendingServiceOps is empty (lost on restart)
	// This is already the case in newTestDiffTracker()

	// 3. DeleteService is called
	dt.DeleteService(serviceUID, true)

	// Verify service was added to pendingServiceOps with StateDeletionInProgress
	// (goes directly to InProgress since service has no locations)
	opState, exists := dt.pendingServiceOps[serviceUID]
	assert.True(t, exists, "service should be in pendingServiceOps")
	assert.Equal(t, StateDeletionInProgress, opState.State, "state should be DeletionInProgress (no locations)")
	assert.True(t, opState.IsInbound, "should be marked as inbound")

	// Verify service was removed from pendingDeletions (already triggered for deletion)
	_, exists = dt.pendingDeletions[serviceUID]
	assert.False(t, exists, "service should not be in pendingDeletions (already triggered)")
}

// TestEngineAddService_AfterRestart tests that AddService correctly handles
// services that already exist in NRP (e.g., after restart)
func TestEngineAddService_AfterRestart(t *testing.T) {
	dt := newTestDiffTracker()
	serviceUID := "service-add-after-restart"

	// Simulate restart scenario:
	// 1. Service already exists in NRP (LoadBalancers)
	dt.NRPResources.LoadBalancers.Insert(serviceUID)

	// 2. pendingServiceOps is empty (lost on restart)
	// This is already the case in newTestDiffTracker()

	// 3. AddService is called
	dt.AddService(serviceUID, true)

	// Verify service was NOT added to pendingServiceOps (already exists in NRP)
	_, exists := dt.pendingServiceOps[serviceUID]
	assert.False(t, exists, "service should not be added to pendingServiceOps since it exists in NRP")

	// Verify service is still in NRP
	assert.True(t, dt.NRPResources.LoadBalancers.Has(serviceUID), "service should still be in NRP")
}

// TestEngineMultipleOperations_AfterRestart tests a complete flow after restart
func TestEngineMultipleOperations_AfterRestart(t *testing.T) {
	dt := newTestDiffTracker()
	inboundService := "lb-service"
	outboundService := "natgw-service"

	// Simulate restart: services exist in NRP but pendingServiceOps is empty
	dt.NRPResources.LoadBalancers.Insert(inboundService)
	dt.NRPResources.NATGateways.Insert(outboundService)

	// Test 1: UpdateEndpoints for inbound service
	dt.UpdateEndpoints(inboundService, map[string]string{}, map[string]string{
		"10.0.0.1": "node1",
	})
	assert.Equal(t, 0, len(dt.pendingEndpoints), "Should not buffer")
	assert.Equal(t, 1, len(dt.K8sResources.Nodes), "Should have 1 node")

	// Test 2: AddPod for outbound service
	dt.AddPod(outboundService, "pod1", "node2", "10.0.0.2")
	assert.Equal(t, 0, len(dt.pendingPods), "Should not buffer")
	assert.Equal(t, 2, len(dt.K8sResources.Nodes), "Should have 2 nodes")

	// Test 3: AddService for new service (should create)
	newService := "new-service"
	dt.AddService(newService, true)
	opState, exists := dt.pendingServiceOps[newService]
	assert.True(t, exists, "New service should be tracked")
	assert.Equal(t, StateNotStarted, opState.State, "Should be in NotStarted state")

	// Test 4: DeleteService for existing service
	dt.DeleteService(inboundService, true)
	opState, exists = dt.pendingServiceOps[inboundService]
	assert.True(t, exists, "Deleted service should be tracked")
	assert.Equal(t, StateDeletionInProgress, opState.State, "Should be in DeletionInProgress state (no locations)")
}
