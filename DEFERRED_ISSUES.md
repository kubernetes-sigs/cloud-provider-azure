# Deferred Issues for CLB DiffTracker Engine

This document catalogs design decisions and improvements that should be addressed in future phases.

## Critical Issues (Resolved)
- ✅ **Engine goroutines startup** - ServiceUpdater and LocationsUpdater now started in `InitializeCloudFromConfig`
- ✅ **sync.Map return bug** - Fixed to return `*sync.Map` pointer in `azure_servicegateway_difftracker_init.go`
- ✅ **Test compilation errors** - Fixed old batch updater references in test files

## Design Issues (Deferred)

### 1. EnsureLoadBalancer Return Status Strategy
**Location**: `pkg/provider/azure_loadbalancer.go:368`

**Current State**: Returns empty `LoadBalancerStatus{}` when ServiceGatewayEnabled  
**Problem**: Kubernetes expects LoadBalancer IP/hostname to be populated in status  
**Impact**: Services may not get external IPs until subsequent reconciliation loops

**Proposed Solutions**:
1. **Status Watcher Pattern**: Implement a goroutine that watches `pendingServiceOps` and updates K8s Service status when `StateCreated` is reached
2. **Blocking Wait**: Make EnsureLoadBalancer wait for Engine completion (contradicts async design)
3. **Status Reconciliation Loop**: Periodically sync Engine state to K8s Service objects

**Recommendation**: Implement Status Watcher (option 1) to maintain async benefits while providing timely status updates

**Code Location**:
```go
// pkg/provider/azure_loadbalancer.go:368
if az.ServiceGatewayEnabled && az.diffTracker != nil {
    serviceUID := getServiceUID(service)
    az.diffTracker.AddService(serviceUID, true)
    // TODO: Implement status watcher to update LoadBalancerStatus after async creation completes
    return &v1.LoadBalancerStatus{}, nil
}
```

---

### 2. Cleanup Code Placement in EnsureLoadBalancerDeleted
**Location**: `pkg/provider/azure_loadbalancer.go:605-615`

**Current State**: Cleanup code (`reconcilePublicIPs`, `localServiceNameToServiceInfoMap.Delete`) exists after synchronous deletion path  
**Problem**: This cleanup should also run for ServiceGateway mode deletions, but currently only runs for non-ServiceGateway mode

**Impact**: May leave stale data in `localServiceNameToServiceInfoMap` after async deletions

**Proposed Solution**: Move cleanup code before the ServiceGateway early-return check, or add it to Engine's deletion completion callback

**Code Location**:
```go
// pkg/provider/azure_loadbalancer.go:605-615
// Synchronous cleanup for non-ServiceGateway mode
if _, err = az.reconcilePublicIPs(ctx, clusterName, service, "", false); err != nil {
    return err
}
if az.UseMultipleStandardLoadBalancers() && isLocalService(service) {
    key := strings.ToLower(svcName)
    az.localServiceNameToServiceInfoMap.Delete(key)
}
```

---

## Minor Issues (Deferred)

### 3. GetClusterName() Returns Empty String
**Location**: `pkg/provider/azure_servicegateway_cloud_provider_interface.go:22`

**Current State**: Returns `""` because clusterName is passed as parameter, not stored in Cloud struct  
**Impact**: May cause issues if Engine or difftracker code expects non-empty cluster name

**Proposed Solution**: Store clusterName in Cloud struct during initialization for easier access

---

### 4. Duplicate Config Import
**Location**: `pkg/provider/azure.go:47-48`

**Current State**: `"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"` imported twice  
**Impact**: Go linter warning (ST1019)

**Solution**: Remove duplicate import line

---

### 5. Phase 6 Work Deferred
**Status**: Files renamed to `.phase6` extension

**Scope**: Outbound flow migration to Engine pattern  
**Files**:
- `azure_servicegateway_pod_egress_resource_updater.go.phase6`
- `azure_servicegateway_pods.go.phase6`

**Work Required**:
1. Restore files (remove `.phase6` extension)
2. Wire pod informer to call `Engine.AddPod()` / `Engine.UpdatePod()` / `Engine.DeletePod()`
3. Update Engine to handle pod egress assignments
4. Remove `podEgressResourceUpdater` references (old batch updater)

**Commented Code**:
```go
// pkg/provider/azure.go:789
// TODO Phase 6: Restore pod informer setup for egress after migrating to Engine pattern
// if az.ServiceGatewayEnabled {
//     az.setUpPodInformerForEgress()
// }
```

---

## Testing Work (Deferred)

### 6. Update Tests to Use Engine Pattern
**Location**: Multiple test files

**Current State**: Tests have old batch updater code commented out with TODOs  
**Files**:
- `pkg/provider/azure_loadbalancer_test.go:857, 958`
- `pkg/provider/azure_local_services_test.go:660`

**Work Required**:
1. Start Engine goroutines in test setup
2. Mock ServiceUpdater/LocationsUpdater if needed
3. Add Engine state assertions to verify async operations
4. Test buffered endpoint scenarios

---

### 7. Comprehensive Engine Testing
**Status**: No dedicated Engine tests exist

**Work Required**:
1. Unit tests for `engine.go` methods (AddService, UpdateEndpoints, DeleteService, OnServiceCreationComplete, CheckPendingDeletions)
2. Integration tests for ServiceUpdater parallel execution (test semaphore, retries, error handling)
3. Integration tests for LocationsUpdater sync logic
4. Race condition testing (concurrent AddService + UpdateEndpoints + DeleteService)
5. Buffering behavior tests (endpoints arrive before service creation complete)

---

## Metrics Work (Deferred)

### 8. Add Engine Metrics
**Status**: Placeholder metrics in locations_updater.go, no comprehensive instrumentation

**Metrics Needed**:
1. **Service Operation Metrics**:
   - `engine_service_operations_total{operation="create|delete", status="success|failure"}`
   - `engine_service_operation_duration_seconds{operation="create|delete"}`
   - `engine_pending_service_operations{state="not_started|creation_in_progress|created|deletion_pending|deletion_in_progress"}`

2. **Buffering Metrics**:
   - `engine_buffered_endpoints_total`
   - `engine_buffered_pods_total`
   - `engine_buffer_duration_seconds` (time endpoints/pods spend buffered)

3. **LocationsUpdater Metrics**:
   - `engine_locations_sync_total{status="success|failure"}`
   - `engine_locations_sync_duration_seconds`
   - `engine_location_addresses_synced{action="add|remove|update"}`

4. **Retry Metrics**:
   - `engine_operation_retries_total{operation="create|delete"}`

---

## Documentation Work (Deferred)

### 9. Engine Architecture Documentation
**Status**: No formal documentation exists

**Documentation Needed**:
1. Architecture diagram showing Engine components (ServiceUpdater, LocationsUpdater, channels, state machine)
2. State transition diagram (StateNotStarted → StateCreationInProgress → StateCreated → StateDeletionPending → StateDeletionInProgress)
3. Sequence diagrams for:
   - Service creation flow (EnsureLoadBalancer → AddService → ServiceUpdater → OnServiceCreationComplete)
   - Service deletion flow (EnsureLoadBalancerDeleted → DeleteService → LocationsUpdater → CheckPendingDeletions)
   - Endpoint update flow (EndpointSlice handler → UpdateEndpoints → LocationsUpdater)
4. Buffering behavior explanation (when/why endpoints are buffered)
5. Migration guide from old batch updater pattern to Engine pattern

---

## Future Enhancements

### 10. Status Watcher Implementation
**Status**: Not implemented

**Purpose**: Update K8s Service status after async LoadBalancer creation completes  
**Design**: Goroutine watching `pendingServiceOps` map, triggers K8s client updates when state transitions to `StateCreated`

---

### 11. Graceful Shutdown
**Status**: Context cancellation exists, but no drain logic

**Work Required**:
1. Drain `serviceUpdaterTrigger` and `locationsUpdaterTrigger` channels before shutdown
2. Wait for in-flight operations to complete (use sync.WaitGroup in ServiceUpdater)
3. Persist pending operations to disk or allow them to retry on restart

---

### 12. Observability Improvements
**Status**: Basic klog statements exist

**Work Required**:
1. Structured logging with consistent fields (serviceUID, operation, state, duration)
2. Trace context propagation through Engine operations
3. Debug endpoints for inspecting Engine state (`/debug/difftracker/services`, `/debug/difftracker/buffered`)

---

## Phase Status Summary

| Phase | Status | Description |
|-------|--------|-------------|
| Phase 0-4 | ✅ Complete | Engine infrastructure (types, ServiceUpdater, LocationsUpdater) |
| Phase 5 | ✅ Complete | Inbound flow integration (AddService, DeleteService, UpdateEndpoints) |
| Phase 6 | ⏸️ Deferred | Outbound flow migration (pod egress to Engine pattern) |
| Phase 7 | ✅ Complete | Old code cleanup (batch updaters removed) |
| Phase 8 | ✅ Complete | Engine goroutine startup |
| Phase 9 | ⏸️ Deferred | Metrics instrumentation |
| Phase 10 | ⏸️ Deferred | Comprehensive testing |

---

## Priority Recommendations

**High Priority** (Address before production):
1. EnsureLoadBalancer return status strategy (#1)
2. Cleanup code placement (#2)
3. Engine metrics (#8)
4. Basic Engine testing (#7)

**Medium Priority** (Address in next iteration):
5. Phase 6 - Outbound flow migration (#5)
6. Status watcher implementation (#10)
7. Test updates (#6)
8. Documentation (#9)

**Low Priority** (Nice to have):
9. GetClusterName fix (#3)
10. Duplicate import cleanup (#4)
11. Graceful shutdown (#11)
12. Observability improvements (#12)
