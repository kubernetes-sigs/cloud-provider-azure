# Complete CLB Processing Flow: DiffTracker Engine Implementation Plan

## Executive Summary

This document provides a **COMPLETE** implementation plan for the new CLB (Cloud Load Balancer) processing flow with DiffTracker Engine, replacing the synchronous `EnsureLoadBalancer()` pattern with an asynchronous, non-blocking architecture. This includes:

- **Inbound Flow**: Load Balancer creation for Kubernetes Services
- **Outbound Flow**: NAT Gateway creation for Egress/Pods
- **Engine**: Central coordinator managing service lifecycle states
- **XUpdater**: Parallel resource creator (PIP → LB/NAT → ServiceGateway)
- **LocationsUpdater**: Location/address synchronization with NRP
- **DeletionChecker**: Ordered deletion (locations first, then services)

## Problem Statement

### Current Synchronous Flow Issues

1. **Blocking operations**: `EnsureLoadBalancer()` blocks for 5-10 seconds per service
2. **No parallelization**: Services created sequentially
3. **Race conditions**: Pods/endpoints arrive before services created
4. **No buffering**: Early EndpointSlice/Pod events lost or cause errors

### New Asynchronous Flow Benefits

1. **Non-blocking**: Returns immediately, processes in background
2. **Parallel execution**: One goroutine per service identity
3. **Buffering**: Early endpoints/pods buffered until service ready
4. **Ordered deletion**: Locations cleared before service deletion
5. **Retry logic**: Per-step retry with exponential backoff

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────┐
│                         KUBERNETES EVENTS                            │
└─────────────────────────────────────────────────────────────────────┘
         │                          │                         │
         │ Service Add/Delete       │ EndpointSlice Updates   │ Pod Add/Delete
         ▼                          ▼                         ▼
┌──────────────────┐    ┌──────────────────┐    ┌──────────────────┐
│ Service Informer │    │Endpoint Informer │    │  Pod Informer    │
└──────────────────┘    └──────────────────┘    └──────────────────┘
         │                          │                         │
         ▼                          ▼                         ▼
┌─────────────────────────────────────────────────────────────────────┐
│                         DIFFTRACKER ENGINE                           │
│  ┌────────────────────────────────────────────────────────────────┐ │
│  │ Public API Methods:                                            │ │
│  │ • AddService(uid, isInbound)                                   │ │
│  │ • DeleteService(uid, isInbound)                                │ │
│  │ • UpdateEndpoints(uid, podIPToNodeIP)                          │ │
│  │ • AddPod(egressUID, podKey, location, address)                 │ │
│  │ • DeletePod(egressUID, location, address)                      │ │
│  │ • OnServiceCreationComplete(uid, success, err)                 │ │
│  └────────────────────────────────────────────────────────────────┘ │
│                                                                       │
│  State Management:                                                   │
│  • pendingServiceOps map[string]*ServiceOperationState              │
│  • bufferedEndpoints map[string][]BufferedEndpointUpdate            │
│  • bufferedPods map[string][]BufferedPodUpdate                      │
│  • pendingDeletions map[string]*PendingDeletion                     │
│                                                                       │
│  Internal Methods (DeletionChecker):                                 │
│  • checkPendingDeletions(ctx) - Called by LocationsUpdater          │
│  • serviceHasLocationsInNRP(uid) - Verifies location cleanup        │
└─────────────────────────────────────────────────────────────────────┘
         │                          │                         
         ▼                          ▼                         
┌──────────────────┐    ┌──────────────────┐    
│    XUpdater      │    │LocationsUpdater  │    
│  (parallel       │    │  (locations/     │    
│   goroutines)    │    │   addresses)     │───┐
└──────────────────┘    └──────────────────┘   │
         │                          │           │
         │                          │           │ Calls after sync
         │                          ▼           ▼
         │                   diffTracker.checkPendingDeletions()
         │                          │
         ▼                          ▼
┌─────────────────────────────────────────────────────────────────────┐
│                      NRP SERVICE GATEWAY API                         │
│  • UpdateServices (create/delete LB/NAT)                            │
│  • UpdateAddressLocations (sync locations/addresses)                │
└─────────────────────────────────────────────────────────────────────┘
```

**Key Architecture Points**:
- **Only 2 Goroutines**: XUpdater and LocationsUpdater
- **DeletionChecker**: Simple methods inside DiffTracker Engine (no separate goroutine)
- **Event-Driven Deletion**: LocationsUpdater calls `checkPendingDeletions()` after each sync
- **No Polling**: Pure event-driven architecture

## Complete Processing Flows

### Inbound Flow: Kubernetes Service → Load Balancer

```
T0: Kubernetes Service created (UID: abc-123, Type: LoadBalancer)
    ↓
T1: Service Informer detects add event
    ↓
T2: azure_loadbalancer.go: EnsureLoadBalancer() called
    ↓
T3: Check if ServiceGatewayEnabled:
    ├─→ NO: Call existing createOrUpdateLoadBalancer() [SYNCHRONOUS]
    └─→ YES: Continue with async flow ↓
    
T4: Engine.AddService("abc-123", isInbound=true)
    ├─→ Creates pendingServiceOps["abc-123"] = StateCreationInProgress
    └─→ Triggers XUpdater via xUpdaterTrigger channel
    
T5: Return immediately to Kubernetes (return &v1.LoadBalancerStatus{})
    
T6: XUpdater.processBatch() receives "abc-123"
    ↓
T7: Spawn goroutine → createInboundService("abc-123")
    ├─→ Step 1: Create/Get Public IP (2-3 seconds)
    ├─→ Step 2: CreateOrUpdateLB("abc-123", lb) (5-8 seconds)
    └─→ Step 3: UpdateNRPSGWServices(add "abc-123" inbound) (1-2 seconds)
    
T8: OnServiceCreationComplete("abc-123", success=true, nil)
    ├─→ Updates pendingServiceOps["abc-123"].State = StateCreated
    └─→ Calls promoteBufferedEndpointsLocked("abc-123")
    
T9: promoteBufferedEndpointsLocked("abc-123")
    ├─→ For each buffered endpoint: UpdateK8sEndpoints()
    └─→ Triggers LocationsUpdater via locationsUpdaterTrigger channel
    
T10: LocationsUpdater.process()
    ├─→ Calls GetSyncLocationsAddresses() → computes diff
    └─→ Calls UpdateNRPSGWAddressLocations(locationsDTO) (1-2 seconds)
    
T11: COMPLETE - Load Balancer created, endpoints synced

PARALLEL TIMELINE (EndpointSlice arrives early):
T2.5: EndpointSlice created for Service abc-123
T2.6: Endpoint Informer calls Engine.UpdateEndpoints("abc-123", {pod1→node1, pod2→node2})
T2.7: Engine checks pendingServiceOps["abc-123"] → StateCreationInProgress
T2.8: Engine buffers endpoints in bufferedEndpoints["abc-123"]
T8: OnServiceCreationComplete() → promotes buffered endpoints
```

### Outbound Flow: Pod with Egress Label → NAT Gateway

```
T0: Pod created with label "egress-gateway-name: my-egress"
    ↓
T1: Pod Informer detects add event (HostIP and PodIP assigned)
    ↓
T2: podInformerAddPod() called
    ↓
T3: Extract egress name: "my-egress"
    ↓
T4: Engine.AddPod("my-egress", "ns/pod", "10.1.1.1", "10.2.2.2")
    ├─→ Check pendingServiceOps["my-egress"]:
    │   ├─→ EXISTS with StateCreated: UpdateK8sPod(ADD) immediately, trigger LocationsUpdater
    │   ├─→ EXISTS with StateCreationInProgress: Buffer in bufferedPods
    │   └─→ NOT EXISTS: Create service operation, buffer pod, trigger XUpdater
    ↓
T5: (IF NOT EXISTS) XUpdater triggered to create NAT Gateway
    
T6: Engine.AddPod() completes (pod buffered or added depending on service state)
    ├─→ Check pendingServiceOps["my-egress"]: NOT FOUND
    ├─→ Create pendingServiceOps["my-egress"] = StateCreationInProgress
    ├─→ Buffer pod in bufferedPods["my-egress"]
    └─→ Trigger XUpdater via xUpdaterTrigger channel
    
T7: XUpdater.processBatch() receives "my-egress"
    ↓
T8: Spawn goroutine → createOutboundService("my-egress")
    ├─→ Step 1: Create/Get Public IP (2-3 seconds)
    ├─→ Step 2: CreateOrUpdateNatGateway("my-egress", natGW) (5-8 seconds)
    └─→ Step 3: UpdateNRPSGWServices(add "my-egress" outbound) (1-2 seconds)
    
T9: OnServiceCreationComplete("my-egress", success=true, nil)
    ├─→ Updates pendingServiceOps["my-egress"].State = StateCreated
    ├─→ Calls promoteBufferedEndpointsLocked("my-egress") [none for outbound]
    └─→ Calls promoteBufferedPodsLocked("my-egress")
    
T10: promoteBufferedPodsLocked("my-egress")
    ├─→ For each buffered pod: UpdateK8sPod(ADD, "my-egress", location, address)
    ├─→ Updates LocalServiceNameToNRPServiceMap counter
    └─→ Triggers LocationsUpdater via locationsUpdaterTrigger channel
    
T11: LocationsUpdater.process()
    ├─→ Calls GetSyncLocationsAddresses() → computes diff
    └─→ Calls UpdateNRPSGWAddressLocations(locationsDTO) (1-2 seconds)
    
T12: COMPLETE - NAT Gateway created, pods synced

PARALLEL TIMELINE (Multiple pods with same egress):
T0: Pod-A created with "my-egress"
T1: Pod-B created with "my-egress" (before NAT Gateway ready)
T2: Pod-C created with "my-egress" (before NAT Gateway ready)
T7: All 3 pods buffered in bufferedPods["my-egress"]
T11: All 3 pods promoted together after NAT Gateway creation
```

### Deletion Flow: Service/NAT Gateway Cleanup

```
INBOUND DELETION (Service deleted):

T0: Kubernetes Service deleted
    ↓
T1: Service Informer detects delete event
    ↓
T2: azure_loadbalancer.go: EnsureLoadBalancerDeleted() called
    ↓
T3: Check if ServiceGatewayEnabled:
    ├─→ NO: Call existing deleteLoadBalancer() [SYNCHRONOUS]
    └─→ YES: Continue with async flow ↓
    
T4: Engine.DeleteService("abc-123", isInbound=true)
    ├─→ Updates pendingServiceOps["abc-123"].State = StateDeletionPending
    ├─→ Adds to pendingDeletions["abc-123"]
    └─→ Checks if locations already cleared (immediate attempt)
    
T5: Return immediately to Kubernetes
    
T6: LocationsUpdater syncs (triggered by other events or this deletion)
    └─→ After sync, calls diffTracker.checkPendingDeletions()
    ↓
T7: For each pending deletion, check serviceHasLocationsInNRP("abc-123")
    ├─→ YES: Locations still exist → wait, retry later
    └─→ NO: Locations cleared → proceed with deletion ↓
    
T8: Update pendingServiceOps["abc-123"].State = StateDeletionInProgress
    ↓
T9: Trigger XUpdater via xUpdaterTrigger channel
    
T10: XUpdater.processBatch() receives "abc-123"
    ↓
T11: Spawn goroutine → deleteInboundService("abc-123")
    ├─→ Step 1: UpdateNRPSGWServices(delete "abc-123" inbound) (1-2 seconds)
    ├─→ Step 2: DeleteLB("abc-123") (2-3 seconds)
    └─→ Step 3: Delete Public IP if not shared (1-2 seconds)
    
T12: Remove from pendingServiceOps and pendingDeletions
    
T13: COMPLETE - Load Balancer deleted

OUTBOUND DELETION (Last pod removed):

T0: Last pod with "my-egress" deleted
    ↓
T1: Pod Informer detects delete event
    ↓
T2: podInformerRemovePod() called
    ↓
T3: podInformerRemovePod() calls Engine.DeletePod("my-egress", location, address)
    ├─→ Calls removePod() to update DiffTracker state
    ├─→ Triggers LocationsUpdater to remove pod location/address
    ├─→ IF counter reaches 0 → remove from LocalServiceNameToNRPServiceMap
    ├─→ IF last pod: Create/Update pendingServiceOps["my-egress"].State = StateDeletionPending
    └─→ IF last pod: Add to pendingDeletions["my-egress"]
    
T5: LocationsUpdater.process() removes pod from NRP
    └─→ After sync, calls diffTracker.checkPendingDeletions()
    ↓
T6: checkPendingDeletions() (IF last pod)
    ↓
T7: Check serviceHasLocationsInNRP("my-egress")
    ├─→ YES: Locations still exist → wait, retry later
    └─→ NO: All pods removed, locations cleared → proceed ↓
    
T8: Update pendingServiceOps["my-egress"].State = StateDeletionInProgress
    ↓
T9: Trigger XUpdater via xUpdaterTrigger channel
    
T10: XUpdater.processBatch() receives "my-egress"
    ↓
T11: Spawn goroutine → deleteOutboundService("my-egress")
    ├─→ Step 1: UpdateNRPSGWServices(delete "my-egress" outbound) (1-2 seconds)
    ├─→ Step 2: DeleteNatGateway("my-egress") (2-3 seconds)
    └─→ Step 3: Delete Public IP if not shared (1-2 seconds)
    
T12: Remove from pendingServiceOps and pendingDeletions
    
T13: COMPLETE - NAT Gateway deleted
```

## Key Components

### 1. Engine (`pkg/provider/difftracker/engine.go`)

**Purpose**: Central coordinator managing service lifecycle and buffering

**Methods**:
- `AddService(serviceUID, isInbound)`: Initiates service creation
- `DeleteService(serviceUID, isInbound)`: Initiates service deletion
- `UpdateEndpoints(serviceUID, podIPToNodeIP)`: Updates inbound endpoints
- `AddPod(egressUID, podKey, location, address)`: Adds outbound pod
- `DeletePod(egressUID, location, address)`: Removes outbound pod
- `OnServiceCreationComplete(serviceUID, success, err)`: Callback from XUpdater
- `promoteBufferedEndpointsLocked(serviceUID)`: Flushes endpoint buffer
- `promoteBufferedPodsLocked(serviceUID)`: Flushes pod buffer

**State Machine**:
```
StateNotStarted → StateCreationInProgress → StateCreated
                                                 ↓
                      StateDeletionPending ← (when delete requested)
                                ↓
                      StateDeletionInProgress → DELETED (removed from map)
```

### 2. XUpdater (`pkg/provider/difftracker/x_updater.go`)

**Purpose**: Parallel resource creator/deleter

**Methods**:
- `processBatch()`: Reads from xUpdaterTrigger channel
- `createInboundService(serviceUID)`: Creates LB in parallel goroutine
- `createOutboundService(natUID)`: Creates NAT Gateway in parallel goroutine
- `deleteInboundService(serviceUID)`: Deletes LB in parallel goroutine
- `deleteOutboundService(natUID)`: Deletes NAT Gateway in parallel goroutine

**Execution Model**:
- One goroutine spawned per service identity
- Sequential steps within each goroutine (PIP → LB/NAT → ServiceGateway)
- Uses sync.WaitGroup for goroutine tracking
- Calls `OnServiceCreationComplete()` callback after completion
- ARM client handles retries automatically - no custom retry logic needed

### 3. LocationsUpdater (`pkg/provider/azure_servicegateway_location_service_updater.go`)

**Purpose**: Syncs location/address changes to NRP Service Gateway

**Methods**:
- `process()`: Triggered by locationsUpdaterTrigger channel
- Calls `GetSyncLocationsAddresses()` to compute diff
- Calls `UpdateNRPSGWAddressLocations(locationsDTO)`
- Triggers DeletionChecker after updating (for pending deletions)

**When Triggered**:
- After endpoint buffer promotion
- After pod buffer promotion
- After pod deletion
- After any UpdateK8sEndpoints/UpdateK8sPod call

### 4. DeletionChecker (`pkg/provider/difftracker/engine.go`)

**Purpose**: Ensures locations cleared before service deletion

**Methods**:
- `checkPendingDeletions(ctx)`: Checks each pending deletion
- `serviceHasLocationsInNRP(serviceUID)`: Verifies no locations reference service

**Logic**:
1. Called directly after LocationsUpdater syncs locations
2. For each pending deletion, check if locations cleared
3. If cleared, trigger XUpdater to delete service
4. If not cleared, do nothing (will be checked again after next location sync)

### 5. DiffTracker Extensions (`pkg/provider/difftracker/types.go`)

**New State Tracking**:
```go
type ServiceOperationState struct {
    ServiceUID  string
    IsInbound   bool
    State       ResourceState // StateCreationInProgress, StateCreated, etc.
    RetryCount  int
    LastAttempt time.Time
}

type BufferedEndpointUpdate struct {
    PodIPToNodeIP map[string]string
    Timestamp     time.Time
}

type BufferedPodUpdate struct {
    PodKey    string
    Location  string
    Address   string
    Timestamp time.Time
}

type PendingDeletion struct {
    ServiceUID string
    IsInbound  bool
    Timestamp  time.Time
}
```

**New DiffTracker Fields**:
```go
type DiffTracker struct {
    // ... existing fields ...
    
    // Engine state management
    pendingServiceOps       map[string]*ServiceOperationState
    bufferedEndpoints       map[string][]BufferedEndpointUpdate
    bufferedPods            map[string][]BufferedPodUpdate
    pendingDeletions        map[string]*PendingDeletion
    
    // Communication channels
    xUpdaterTrigger         chan string
    locationsUpdaterTrigger chan struct{}
}
```

### 6. Integration Points

**azure_loadbalancer.go**:
```go
func (az *Cloud) EnsureLoadBalancer(ctx context.Context, ...) (*v1.LoadBalancerStatus, error) {
    if az.ServiceGatewayEnabled {
        serviceUID := string(service.UID)
        az.diffTracker.Engine.AddService(serviceUID, isInbound=true)
        return &v1.LoadBalancerStatus{}, nil // Return immediately
    }
    // ... existing synchronous logic ...
}

func (az *Cloud) EnsureLoadBalancerDeleted(ctx context.Context, ...) error {
    if az.ServiceGatewayEnabled {
        serviceUID := string(service.UID)
        az.diffTracker.Engine.DeleteService(serviceUID, isInbound=true)
        return nil // Return immediately
    }
    // ... existing synchronous logic ...
}
```

**azure_servicegateway_pods.go**:
```go
func (az *Cloud) podInformerAddPod(pod *v1.Pod) {
    if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
        return
    }
    if pod.Status.HostIP == "" || pod.Status.PodIP == "" {
        return
    }
    
    egressName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
    podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
    
    // Call Engine directly - it handles service creation if needed
    az.diffTracker.Engine.AddPod(egressName, podKey, pod.Status.HostIP, pod.Status.PodIP)
}

func (az *Cloud) podInformerRemovePod(pod *v1.Pod) {
    if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
        return
    }
    
    egressName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
    
    // Call Engine directly - it handles last-pod deletion logic
    az.diffTracker.Engine.DeletePod(egressName, pod.Status.HostIP, pod.Status.PodIP)
}
```

## Data Structures

### New Types in `pkg/provider/difftracker/types.go`

```go
// BufferedPodUpdate represents a pod waiting for its service to be created
type BufferedPodUpdate struct {
    PodKey    string    // namespace/name for logging
    Location  string    // HostIP
    Address   string    // PodIP
    Timestamp time.Time // When buffered (for debugging/metrics)
}

// Add to DiffTracker struct:
type DiffTracker struct {
    // ... existing fields ...
    
    // New fields for pod buffering
    bufferedPods map[string][]BufferedPodUpdate // key: serviceUID (outbound NAT Gateway UID)
}
```

### State Tracking

The Engine already tracks service states in `pendingServiceOps`:

- `StateNotStarted`: Service deletion/creation not yet started
- `StateCreationInProgress`: Service being created (pods should buffer)
- `StateCreated`: Service exists in NRP (pods can be added immediately)
- `StateDeletionPending`: Waiting for locations to clear before deletion
- `StateDeletionInProgress`: Service being deleted

## Implementation Steps

### Step 1: Extend DiffTracker Types

**File**: `pkg/provider/difftracker/types.go`

Add new type definition:

```go
// BufferedPodUpdate represents a pod waiting for its service to be created
type BufferedPodUpdate struct {
    PodKey    string    // namespace/name for logging
    Location  string    // HostIP
    Address   string    // PodIP
    Timestamp time.Time // When buffered (for debugging/metrics)
}
```

Add field to DiffTracker struct (after `bufferedEndpoints`):

```go
type DiffTracker struct {
    mu sync.Mutex

    K8sResources K8s_State
    NRPResources NRP_State

    PodEgressQueue                  workqueue.TypedRateLimitingInterface[PodCrudEvent]
    LocalServiceNameToNRPServiceMap sync.Map

    InitialSyncDone bool

    // Engine-related fields
    pendingServiceOps   map[string]*ServiceOperationState
    bufferedEndpoints   map[string][]BufferedEndpointUpdate
    bufferedPods        map[string][]BufferedPodUpdate  // NEW: key is serviceUID
    pendingDeletions    map[string]*PendingDeletion
    xUpdaterTrigger     chan string
    locationsUpdaterTrigger chan struct{}
}
```

### Step 2: Initialize bufferedPods Map

**File**: `pkg/provider/difftracker/difftracker.go`

In `InitializeDiffTracker()`, add initialization (after `bufferedEndpoints`):

```go
func InitializeDiffTracker() *DiffTracker {
    return &DiffTracker{
        K8sResources: K8s_State{
            Services: utilsets.NewString(),
            Egresses: utilsets.NewString(),
            Nodes:    make(map[string]Node),
        },
        NRPResources: NRP_State{
            LoadBalancers: utilsets.NewString(),
            NATGateways:   utilsets.NewString(),
            Locations:     make(map[string]NRPLocation),
        },
        PodEgressQueue: workqueue.NewTypedRateLimitingQueue(
            workqueue.DefaultTypedControllerRateLimiter[PodCrudEvent](),
        ),
        pendingServiceOps:       make(map[string]*ServiceOperationState),
        bufferedEndpoints:       make(map[string][]BufferedEndpointUpdate),
        bufferedPods:            make(map[string][]BufferedPodUpdate),  // NEW
        pendingDeletions:        make(map[string]*PendingDeletion),
        xUpdaterTrigger:         make(chan string, 100),
        locationsUpdaterTrigger: make(chan struct{}, 1),
    }
}
```

### Step 3: Implement Engine.AddService()

**File**: `pkg/provider/difftracker/engine.go`

Add the `AddService()` method:

```go
// AddService handles service creation events for inbound (Load Balancer) services.
// If the service already exists in NRP, it does nothing (idempotent).
// If the service doesn't exist, it triggers service creation via XUpdater.
func (dt *DiffTracker) AddService(serviceUID string, isInbound bool) {
    dt.mu.Lock()
    defer dt.mu.Unlock()

    klog.V(4).Infof("Engine.AddService: serviceUID=%s, isInbound=%v", serviceUID, isInbound)

    // Check if service already exists in NRP
    if isInbound {
        if dt.NRPResources.LoadBalancers.Has(serviceUID) {
            klog.V(2).Infof("Engine.AddService: Load Balancer %s already exists in NRP, skipping", serviceUID)
            return
        }
    } else {
        if dt.NRPResources.NATGateways.Has(serviceUID) {
            klog.V(2).Infof("Engine.AddService: NAT Gateway %s already exists in NRP, skipping", serviceUID)
            return
        }
    }

    // Check if service operation is already tracked
    opState, exists := dt.pendingServiceOps[serviceUID]
    if exists {
        klog.V(2).Infof("Engine.AddService: Service %s already tracked with state %v", serviceUID, opState.State)
        return
    }

    // Service doesn't exist - need to create it
    klog.V(2).Infof("Engine.AddService: Service %s doesn't exist, triggering creation", serviceUID)

    // Add service operation to pending list
    dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
        ServiceUID:  serviceUID,
        IsInbound:   isInbound,
        State:       StateCreationInProgress,
        RetryCount:  0,
        LastAttempt: time.Now(),
    }

    // Trigger XUpdater to create the service
    select {
    case dt.xUpdaterTrigger <- serviceUID:
        klog.V(4).Infof("Engine.AddService: Triggered XUpdater for service %s", serviceUID)
    default:
        klog.V(2).Infof("Engine.AddService: XUpdater channel full for service %s", serviceUID)
    }
}
```

### Step 4: Implement Engine.UpdateEndpoints()

**File**: `pkg/provider/difftracker/engine.go`

Add the `UpdateEndpoints()` method (after `AddService()` method):

```go
// UpdateEndpoints handles endpoint updates for inbound (Load Balancer) services.
// If the service is already created in NRP, endpoints are immediately updated.
// If the service is being created, endpoints are buffered until creation completes.
// If the service doesn't exist, this shouldn't happen (AddService should be called first).
func (dt *DiffTracker) UpdateEndpoints(serviceUID string, podIPToNodeIP map[string]string) {
    dt.mu.Lock()
    defer dt.mu.Unlock()

    klog.V(4).Infof("Engine.UpdateEndpoints: serviceUID=%s, endpoints=%d", serviceUID, len(podIPToNodeIP))

    // Check if service operation is tracked
    opState, exists := dt.pendingServiceOps[serviceUID]

    if !exists {
        // Check if service exists in NRP (created outside Engine)
        if dt.NRPResources.LoadBalancers.Has(serviceUID) {
            klog.V(2).Infof("Engine.UpdateEndpoints: Service %s exists in NRP, updating endpoints immediately", serviceUID)

            err := dt.UpdateK8sEndpoints(serviceUID, podIPToNodeIP)
            if err != nil {
                klog.Errorf("Engine.UpdateEndpoints: Failed to update endpoints for service %s: %v", serviceUID, err)
                return
            }

            // Trigger LocationsUpdater
            select {
            case dt.locationsUpdaterTrigger <- struct{}{}:
            default:
            }
            return
        }

        // Service doesn't exist - this is unexpected, but buffer anyway
        klog.Warningf("Engine.UpdateEndpoints: Service %s not tracked and not in NRP, buffering endpoints", serviceUID)
        dt.bufferedEndpoints[serviceUID] = append(dt.bufferedEndpoints[serviceUID], BufferedEndpointUpdate{
            PodIPToNodeIP: podIPToNodeIP,
            Timestamp:     time.Now(),
        })
        return
    }

    // Service operation exists - check state
    switch opState.State {
    case StateCreationInProgress:
        // Service is being created - buffer the endpoints
        klog.V(2).Infof("Engine.UpdateEndpoints: Service %s creation in progress, buffering %d endpoints",
            serviceUID, len(podIPToNodeIP))

        dt.bufferedEndpoints[serviceUID] = append(dt.bufferedEndpoints[serviceUID], BufferedEndpointUpdate{
            PodIPToNodeIP: podIPToNodeIP,
            Timestamp:     time.Now(),
        })

    case StateCreated:
        // Service is ready - update endpoints immediately
        klog.V(2).Infof("Engine.UpdateEndpoints: Service %s is ready, updating %d endpoints immediately",
            serviceUID, len(podIPToNodeIP))

        err := dt.UpdateK8sEndpoints(serviceUID, podIPToNodeIP)
        if err != nil {
            klog.Errorf("Engine.UpdateEndpoints: Failed to update endpoints for service %s: %v", serviceUID, err)
            return
        }

        // Trigger LocationsUpdater
        select {
        case dt.locationsUpdaterTrigger <- struct{}{}:
        default:
        }

    case StateDeletionPending, StateDeletionInProgress:
        // Service is being deleted - ignore endpoint updates
        klog.Warningf("Engine.UpdateEndpoints: Cannot update endpoints for service %s which is being deleted", serviceUID)

    default:
        klog.Errorf("Engine.UpdateEndpoints: Unknown state %v for service %s", opState.State, serviceUID)
    }
}
```

### Step 5: Implement Engine.DeleteService()

**File**: `pkg/provider/difftracker/engine.go`

Add the `DeleteService()` method (after `UpdateEndpoints()` method):

```go
// DeleteService handles service deletion events for inbound (Load Balancer) services.
// It marks the service for deletion and triggers DeletionChecker to verify locations are cleared.
func (dt *DiffTracker) DeleteService(serviceUID string, isInbound bool) {
    dt.mu.Lock()
    defer dt.mu.Unlock()

    klog.V(4).Infof("Engine.DeleteService: serviceUID=%s, isInbound=%v", serviceUID, isInbound)

    // Check if service exists in pending operations
    opState, exists := dt.pendingServiceOps[serviceUID]

    if !exists {
        // Service not tracked - check if it exists in NRP
        serviceExists := false
        if isInbound {
            serviceExists = dt.NRPResources.LoadBalancers.Has(serviceUID)
        } else {
            serviceExists = dt.NRPResources.NATGateways.Has(serviceUID)
        }

        if !serviceExists {
            klog.V(2).Infof("Engine.DeleteService: Service %s doesn't exist in NRP, nothing to delete", serviceUID)
            return
        }

        // Service exists in NRP but not tracked - add it for deletion
        klog.V(2).Infof("Engine.DeleteService: Service %s exists in NRP, adding for deletion", serviceUID)
        dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
            ServiceUID:  serviceUID,
            IsInbound:   isInbound,
            State:       StateDeletionPending,
            RetryCount:  0,
            LastAttempt: time.Now(),
        }
    } else {
        // Service is tracked - update state based on current state
        switch opState.State {
        case StateCreationInProgress:
            // Service is still being created - mark for deletion after creation
            klog.V(2).Infof("Engine.DeleteService: Service %s is being created, marking for deletion after creation", serviceUID)
            opState.State = StateDeletionPending

        case StateCreated:
            // Service exists - mark for deletion
            klog.V(2).Infof("Engine.DeleteService: Service %s is created, marking for deletion", serviceUID)
            opState.State = StateDeletionPending

        case StateDeletionPending, StateDeletionInProgress:
            // Already being deleted
            klog.V(2).Infof("Engine.DeleteService: Service %s already marked for deletion with state %v", serviceUID, opState.State)
            return

        default:
            klog.Errorf("Engine.DeleteService: Unknown state %v for service %s", opState.State, serviceUID)
            return
        }
    }

    // Clear any buffered endpoints/pods for this service
    delete(dt.bufferedEndpoints, serviceUID)
    delete(dt.bufferedPods, serviceUID)

    // Add to pending deletions (will be checked by LocationsUpdater after next sync)
    dt.pendingDeletions[serviceUID] = &PendingDeletion{
        ServiceUID: serviceUID,
        IsInbound:  isInbound,
        Timestamp:  time.Now(),
    }

    // Check immediately if locations are already clear
    // Will be re-checked after each location sync
    hasLocations := dt.serviceHasLocationsInNRP(serviceUID)
    if !hasLocations {
        klog.V(2).Infof("Engine.DeleteService: Service %s has no locations, triggering immediate deletion", serviceUID)
        opState.State = StateDeletionInProgress
        select {
        case dt.xUpdaterTrigger <- serviceUID:
        default:
        }
        delete(dt.pendingDeletions, serviceUID)
    }
}
```

### Step 6: Implement promoteBufferedEndpointsLocked()

**File**: `pkg/provider/difftracker/engine.go`

Add helper method:

```go
// promoteBufferedEndpointsLocked flushes all buffered endpoints for a service after it's created.
// Must be called with dt.mu held.
func (dt *DiffTracker) promoteBufferedEndpointsLocked(serviceUID string) {
    bufferedEndpoints, exists := dt.bufferedEndpoints[serviceUID]
    if !exists || len(bufferedEndpoints) == 0 {
        return
    }

    klog.V(2).Infof("Engine.promoteBufferedEndpointsLocked: Promoting %d buffered endpoint updates for service %s",
        len(bufferedEndpoints), serviceUID)

    // Merge all buffered endpoint updates (last one wins for each pod IP)
    mergedEndpoints := make(map[string]string)
    for _, update := range bufferedEndpoints {
        for podIP, nodeIP := range update.PodIPToNodeIP {
            mergedEndpoints[podIP] = nodeIP
        }
    }

    klog.V(4).Infof("Engine.promoteBufferedEndpointsLocked: Merged to %d unique endpoints", len(mergedEndpoints))

    err := dt.UpdateK8sEndpoints(serviceUID, mergedEndpoints)
    if err != nil {
        klog.Errorf("Engine.promoteBufferedEndpointsLocked: Failed to update endpoints for service %s: %v",
            serviceUID, err)
        return
    }

    // Clear buffered endpoints
    delete(dt.bufferedEndpoints, serviceUID)

    // Trigger LocationsUpdater to sync all the promoted endpoints
    select {
    case dt.locationsUpdaterTrigger <- struct{}{}:
    default:
    }
}
```

### Step 7: Implement Engine.AddPod()

**File**: `pkg/provider/difftracker/engine.go`

Add the `AddPod()` method (after `promoteBufferedEndpointsLocked()` method):

```go
// AddPod handles pod addition events for outbound (NAT Gateway) services.
// If the service is already created in NRP, the pod is immediately added to DiffTracker.
// If the service is being created, the pod is buffered until creation completes.
// If the service doesn't exist, it triggers service creation and buffers the pod.
func (dt *DiffTracker) AddPod(serviceUID, podKey, location, address string) {
    dt.mu.Lock()
    defer dt.mu.Unlock()

    klog.V(4).Infof("Engine.AddPod: serviceUID=%s, podKey=%s, location=%s, address=%s",
        serviceUID, podKey, location, address)

    // Check if service operation is tracked
    opState, exists := dt.pendingServiceOps[serviceUID]
    
    if !exists {
        // Service doesn't exist - need to create it first
        klog.V(2).Infof("Engine.AddPod: Service %s doesn't exist, triggering creation", serviceUID)
        
        // Add service operation to pending list
        dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
            ServiceUID:  serviceUID,
            IsInbound:   false, // NAT Gateway is outbound
            State:       StateCreationInProgress,
            RetryCount:  0,
            LastAttempt: time.Now(),
        }
        
        // Buffer the pod
        dt.bufferedPods[serviceUID] = append(dt.bufferedPods[serviceUID], BufferedPodUpdate{
            PodKey:    podKey,
            Location:  location,
            Address:   address,
            Timestamp: time.Now(),
        })
        
        // Trigger XUpdater to create the service
        select {
        case dt.xUpdaterTrigger <- serviceUID:
            klog.V(4).Infof("Engine.AddPod: Triggered XUpdater for service %s", serviceUID)
        default:
            klog.V(2).Infof("Engine.AddPod: XUpdater channel full for service %s", serviceUID)
        }
        
        return
    }

    // Service operation exists - check state
    switch opState.State {
    case StateCreationInProgress:
        // Service is being created - buffer the pod
        klog.V(2).Infof("Engine.AddPod: Service %s creation in progress, buffering pod %s",
            serviceUID, podKey)
        
        dt.bufferedPods[serviceUID] = append(dt.bufferedPods[serviceUID], BufferedPodUpdate{
            PodKey:    podKey,
            Location:  location,
            Address:   address,
            Timestamp: time.Now(),
        })

    case StateCreated:
        // Service is ready - add pod immediately
        klog.V(2).Infof("Engine.AddPod: Service %s is ready, adding pod %s immediately",
            serviceUID, podKey)
        
        err := dt.addOrUpdatePod(UpdatePodInputType{
            PodOperation:           ADD,
            PublicOutboundIdentity: serviceUID,
            Location:               location,
            Address:                address,
        })
        
        if err != nil {
            klog.Errorf("Engine.AddPod: Failed to add pod %s: %v", podKey, err)
            return
        }
        
        // Update LocalServiceNameToNRPServiceMap counter
        counter := 0
        if val, ok := dt.LocalServiceNameToNRPServiceMap.Load(strings.ToLower(serviceUID)); ok {
            counter = val.(int)
        }
        dt.LocalServiceNameToNRPServiceMap.Store(strings.ToLower(serviceUID), counter+1)
        
        // Trigger LocationsUpdater
        select {
        case dt.locationsUpdaterTrigger <- struct{}{}:
        default:
        }

    case StateDeletionPending, StateDeletionInProgress:
        // Service is being deleted - this shouldn't happen
        klog.Warningf("Engine.AddPod: Cannot add pod %s to service %s which is being deleted",
            podKey, serviceUID)

    default:
        klog.Errorf("Engine.AddPod: Unknown state %v for service %s", opState.State, serviceUID)
    }
}
```

### Step 8: Implement Engine.DeletePod()

**File**: `pkg/provider/difftracker/engine.go`

Add the `DeletePod()` method (after `AddPod()` method):

```go
// DeletePod handles pod deletion events for outbound (NAT Gateway) services.
// It immediately removes the pod from DiffTracker and triggers LocationsUpdater.
// If this is the last pod for the service, it checks if the service can be deleted.
func (dt *DiffTracker) DeletePod(serviceUID, location, address string) {
    dt.mu.Lock()
    defer dt.mu.Unlock()

    klog.V(4).Infof("Engine.DeletePod: serviceUID=%s, location=%s, address=%s",
        serviceUID, location, address)

    // Remove pod from DiffTracker
    err := dt.removePod(UpdatePodInputType{
        PodOperation:           REMOVE,
        PublicOutboundIdentity: serviceUID,
        Location:               location,
        Address:                address,
    })

    if err != nil {
        klog.Errorf("Engine.DeletePod: Failed to remove pod: %v", err)
        return
    }

    // Update LocalServiceNameToNRPServiceMap counter
    val, ok := dt.LocalServiceNameToNRPServiceMap.Load(strings.ToLower(serviceUID))
    if !ok {
        klog.Warningf("Engine.DeletePod: Service %s not found in LocalServiceNameToNRPServiceMap", serviceUID)
        return
    }

    counter := val.(int)
    if counter <= 0 {
        klog.Errorf("Engine.DeletePod: Service %s has invalid counter: %d", serviceUID, counter)
        return
    }

    if counter == 1 {
        // This was the last pod - remove from map
        dt.LocalServiceNameToNRPServiceMap.Delete(strings.ToLower(serviceUID))
        
        klog.V(2).Infof("Engine.DeletePod: Last pod removed for service %s, can now delete service", serviceUID)
        
        // Check if service exists in pending operations
        opState, exists := dt.pendingServiceOps[serviceUID]
        if !exists {
            // Service not tracked - add it for deletion
            dt.pendingServiceOps[serviceUID] = &ServiceOperationState{
                ServiceUID:  serviceUID,
                IsInbound:   false,
                State:       StateDeletionPending,
                RetryCount:  0,
                LastAttempt: time.Now(),
            }
        } else if opState.State == StateCreated {
            // Service exists and is created - mark for deletion
            opState.State = StateDeletionPending
        }
        
        // Add to pending deletions (will be checked by LocationsUpdater after sync)
        dt.pendingDeletions[serviceUID] = &PendingDeletion{
            ServiceUID: serviceUID,
            IsInbound:  false,
            Timestamp:  time.Now(),
        }
        
        // LocationsUpdater will check pending deletions after syncing the removal
    } else {
        // Still have pods - decrement counter
        dt.LocalServiceNameToNRPServiceMap.Store(strings.ToLower(serviceUID), counter-1)
        klog.V(4).Infof("Engine.DeletePod: Decremented counter for service %s to %d", serviceUID, counter-1)
    }

    // Trigger LocationsUpdater to sync the change
    select {
    case dt.locationsUpdaterTrigger <- struct{}{}:
    default:
    }
}
```

### Step 9: Implement promoteBufferedPodsLocked()

**File**: `pkg/provider/difftracker/engine.go`

Add helper method (after `DeletePod()` method):

```go
// promoteBufferedPodsLocked flushes all buffered pods for a service after it's created.
// Must be called with dt.mu held.
func (dt *DiffTracker) promoteBufferedPodsLocked(serviceUID string) {
    bufferedPods, exists := dt.bufferedPods[serviceUID]
    if !exists || len(bufferedPods) == 0 {
        return
    }

    klog.V(2).Infof("Engine.promoteBufferedPodsLocked: Promoting %d buffered pods for service %s",
        len(bufferedPods), serviceUID)

    for _, pod := range bufferedPods {
        klog.V(4).Infof("Engine.promoteBufferedPodsLocked: Adding pod %s (location=%s, address=%s)",
            pod.PodKey, pod.Location, pod.Address)

        err := dt.addOrUpdatePod(UpdatePodInputType{
            PodOperation:           ADD,
            PublicOutboundIdentity: serviceUID,
            Location:               pod.Location,
            Address:                pod.Address,
        })

        if err != nil {
            klog.Errorf("Engine.promoteBufferedPodsLocked: Failed to add pod %s: %v",
                pod.PodKey, err)
            continue
        }

        // Update LocalServiceNameToNRPServiceMap counter
        counter := 0
        if val, ok := dt.LocalServiceNameToNRPServiceMap.Load(strings.ToLower(serviceUID)); ok {
            counter = val.(int)
        }
        dt.LocalServiceNameToNRPServiceMap.Store(strings.ToLower(serviceUID), counter+1)
    }

    // Clear buffered pods
    delete(dt.bufferedPods, serviceUID)

    // Trigger LocationsUpdater to sync all the promoted pods
    select {
    case dt.locationsUpdaterTrigger <- struct{}{}:
    default:
    }
}
```

### Step 10: Update OnServiceCreationComplete()

**File**: `pkg/provider/difftracker/engine.go`

Modify `OnServiceCreationComplete()` to call both `promoteBufferedEndpointsLocked()` and `promoteBufferedPodsLocked()`:

Find the existing method and update it to include pod promotion:

```go
func (dt *DiffTracker) OnServiceCreationComplete(serviceUID string, success bool, err error) {
    dt.mu.Lock()
    defer dt.mu.Unlock()

    opState, exists := dt.pendingServiceOps[serviceUID]
    if !exists {
        klog.Warningf("Engine.OnServiceCreationComplete: Service %s not found in pending operations", serviceUID)
        return
    }

    if success {
        klog.V(2).Infof("Engine.OnServiceCreationComplete: Service %s created successfully", serviceUID)
        opState.State = StateCreated
        opState.LastAttempt = time.Now()

        // Promote buffered endpoints (existing logic for inbound services)
        dt.promoteBufferedEndpointsLocked(serviceUID)

        // Promote buffered pods (NEW for outbound services)
        dt.promoteBufferedPodsLocked(serviceUID)

    } else {
        klog.Errorf("Engine.OnServiceCreationComplete: Service %s creation failed: %v", serviceUID, err)
        opState.RetryCount++
        opState.LastAttempt = time.Now()

        // Check if max retries exceeded
        if opState.RetryCount >= maxRetries {
            klog.Errorf("Engine.OnServiceCreationComplete: Service %s exceeded max retries, giving up", serviceUID)
            delete(dt.pendingServiceOps, serviceUID)
            delete(dt.bufferedEndpoints, serviceUID)
            delete(dt.bufferedPods, serviceUID)  // NEW: Also clear buffered pods
            return
        }

        // Retry creation
        klog.V(2).Infof("Engine.OnServiceCreationComplete: Retrying service %s creation (attempt %d/%d)",
            serviceUID, opState.RetryCount, maxRetries)

        // Trigger XUpdater for retry
        select {
        case dt.xUpdaterTrigger <- serviceUID:
        default:
        }
    }
}
```

### Step 11: Update XUpdater's createInboundService() and createOutboundService()

**File**: `pkg/provider/difftracker/x_updater.go`

Modify both `createInboundService()` and `createOutboundService()` to call `OnServiceCreationComplete()`:

**For inbound (Load Balancer)**:

```go
func (xu *XUpdater) createInboundService(ctx context.Context, serviceUID string) {
    klog.V(2).Infof("XUpdater.createInboundService: Creating Load Balancer for service %s", serviceUID)

    // Step 1: Create Public IP (if not exists)
    // ... existing PIP creation logic ...

    // Step 2: Create Load Balancer
    lb := armnetwork.LoadBalancer{
        // ... existing LB creation logic ...
    }

    err := xu.az.CreateOrUpdateLB(ctx, xu.az.ResourceGroup, lb)
    if err != nil {
        klog.Errorf("XUpdater.createInboundService: Failed to create Load Balancer %s: %v", serviceUID, err)
        xu.diffTracker.OnServiceCreationComplete(serviceUID, false, err)
        return
    }

    klog.V(2).Infof("XUpdater.createInboundService: Load Balancer %s created successfully", serviceUID)

    // Step 3: Register with Service Gateway
    err = xu.az.UpdateNRPSGWServices(ctx, xu.az.ServiceGatewayResourceName, servicesDTO)
    if err != nil {
        klog.Errorf("XUpdater.createInboundService: Failed to register Load Balancer %s with Service Gateway: %v",
            serviceUID, err)
        xu.diffTracker.OnServiceCreationComplete(serviceUID, false, err)
        return
    }

    klog.V(2).Infof("XUpdater.createInboundService: Load Balancer %s registered with Service Gateway", serviceUID)

    // Notify Engine that creation succeeded
    xu.diffTracker.OnServiceCreationComplete(serviceUID, true, nil)
}
```

**For outbound (NAT Gateway)**:

Modify `createOutboundService()` to call `OnServiceCreationComplete()`:

Find where NAT Gateway creation completes and add the callback:

```go
func (xu *XUpdater) createOutboundService(ctx context.Context, natUID string) {
    klog.V(2).Infof("XUpdater.createOutboundService: Creating NAT Gateway for egress %s", natUID)

    // Step 1: Create Public IP (if not exists)
    // ... existing PIP creation logic ...

    // Step 2: Create NAT Gateway
    natGateway := armnetwork.NatGateway{
        // ... existing NAT Gateway creation logic ...
    }

    err := xu.az.createOrUpdateNatGateway(ctx, xu.az.ResourceGroup, natGateway)
    if err != nil {
        klog.Errorf("XUpdater.createOutboundService: Failed to create NAT Gateway %s: %v", natUID, err)
        xu.diffTracker.OnServiceCreationComplete(natUID, false, err)  // NEW
        return
    }

    klog.V(2).Infof("XUpdater.createOutboundService: NAT Gateway %s created successfully", natUID)

    // Step 3: Register with Service Gateway
    err = xu.az.UpdateNRPSGWServices(ctx, xu.az.ServiceGatewayResourceName, servicesDTO)
    if err != nil {
        klog.Errorf("XUpdater.createOutboundService: Failed to register NAT Gateway %s with Service Gateway: %v",
            natUID, err)
        xu.diffTracker.OnServiceCreationComplete(natUID, false, err)  // NEW
        return
    }

    klog.V(2).Infof("XUpdater.createOutboundService: NAT Gateway %s registered with Service Gateway", natUID)

    // Notify Engine that creation succeeded
    xu.diffTracker.OnServiceCreationComplete(natUID, true, nil)  // NEW
}
```

### Step 12: Wire EnsureLoadBalancer to Engine

**File**: `pkg/provider/azure_loadbalancer.go`

Modify `EnsureLoadBalancer()` to call `Engine.AddService()`:

```go
func (az *Cloud) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
    // Check if Service Gateway is enabled
    if az.ServiceGatewayEnabled {
        serviceUID := string(service.UID)
        klog.V(2).Infof("EnsureLoadBalancer: Using async Engine for service %s", serviceUID)

        // Call Engine.AddService - returns immediately
        az.diffTracker.Engine.AddService(serviceUID, true) // isInbound=true

        // Return placeholder status immediately
        return &v1.LoadBalancerStatus{}, nil
    }

    // Fall back to synchronous logic for non-ServiceGateway mode
    klog.V(2).Infof("EnsureLoadBalancer: Using synchronous logic for service %s", service.Name)
    return az.createOrUpdateLoadBalancer(ctx, clusterName, service, nodes)
}
```

Modify `EnsureLoadBalancerDeleted()` to call `Engine.DeleteService()`:

```go
func (az *Cloud) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) error {
    // Check if Service Gateway is enabled
    if az.ServiceGatewayEnabled {
        serviceUID := string(service.UID)
        klog.V(2).Infof("EnsureLoadBalancerDeleted: Using async Engine for service %s", serviceUID)

        // Call Engine.DeleteService - returns immediately
        az.diffTracker.Engine.DeleteService(serviceUID, true) // isInbound=true

        return nil
    }

    // Fall back to synchronous logic for non-ServiceGateway mode
    klog.V(2).Infof("EnsureLoadBalancerDeleted: Using synchronous logic for service %s", service.Name)
    return az.deleteLoadBalancer(ctx, clusterName, service)
}
```

### Step 13: Wire EndpointSlice Informer to Engine

**File**: `pkg/provider/azure_endpoints.go` (or wherever endpoint informer is implemented)

Wire EndpointSlice informer to call `Engine.UpdateEndpoints()`:

```go
func (az *Cloud) endpointSliceUpdateHandler(oldObj, newObj interface{}) {
    newEPS := newObj.(*discovery.EndpointSlice)
    
    // Get service UID from owner reference
    serviceUID := getServiceUIDFromEndpointSlice(newEPS)
    if serviceUID == "" {
        return
    }

    // Extract pod IP to node IP mapping
    podIPToNodeIP := make(map[string]string)
    for _, endpoint := range newEPS.Endpoints {
        if endpoint.Addresses == nil || len(endpoint.Addresses) == 0 {
            continue
        }
        if endpoint.NodeName == nil {
            continue
        }
        
        podIP := endpoint.Addresses[0]
        nodeIP := az.getNodeIPByName(*endpoint.NodeName)
        if nodeIP != "" {
            podIPToNodeIP[podIP] = nodeIP
        }
    }

    // Call Engine.UpdateEndpoints - it handles buffering if service not ready
    az.diffTracker.Engine.UpdateEndpoints(serviceUID, podIPToNodeIP)
}
```

### Step 14: Wire podInformerAddPod to Engine

**File**: `pkg/provider/azure_servicegateway_pods.go`

Replace `UpdateK8sPod()` calls with `Engine.AddPod()`:

```go
func (az *Cloud) podInformerAddPod(pod *v1.Pod) {
    // Validate pod has required label and IPs
    if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
        klog.Errorf("podInformerAddPod: Pod %s/%s has no labels or staticGatewayConfiguration label. Cannot process add event.",
            pod.Namespace, pod.Name)
        return
    }

    if pod.Status.HostIP == "" || pod.Status.PodIP == "" {
        klog.Errorf("podInformerAddPod: Pod %s/%s has no HostIP or PodIP. Cannot process add event.",
            pod.Namespace, pod.Name)
        return
    }

    staticGatewayConfigurationName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
    klog.Infof("podInformerAddPod: Pod %s/%s has static gateway configuration: %s",
        pod.Namespace, pod.Name, staticGatewayConfigurationName)

    // Get pod key for logging
    podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

    klog.V(2).Infof("podInformerAddPod: Pod %s added with egress %s",
        podKey, staticGatewayConfigurationName)

    // Call Engine.AddPod directly - Engine handles all states:
    // - Service doesn't exist → Engine creates it and buffers pod
    // - Service being created → Engine buffers pod
    // - Service ready → Engine adds pod immediately
    az.diffTracker.Engine.AddPod(
        staticGatewayConfigurationName, // serviceUID (egress name)
        podKey,                         // podKey for logging
        pod.Status.HostIP,              // location
        pod.Status.PodIP,               // address
    )
}
```

### Step 15: Wire podInformerRemovePod to Engine

**File**: `pkg/provider/azure_servicegateway_pods.go`

Replace `UpdateK8sPod()` calls with `Engine.DeletePod()`:

```go
func (az *Cloud) podInformerRemovePod(pod *v1.Pod) {
    if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
        klog.Errorf("Pod %s/%s has no labels. Cannot process delete event.",
            pod.Namespace, pod.Name)
        return
    }

    staticGatewayConfigurationName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
    counter, ok := az.diffTracker.LocalServiceNameToNRPServiceMap.Load(staticGatewayConfigurationName)

    podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
    klog.V(2).Infof("podInformerRemovePod: Pod %s removed from egress %s",
        podKey, staticGatewayConfigurationName)

    if ok {
        // Call Engine.DeletePod directly - Engine handles all cases:
        // - Not last pod → Engine decrements counter, removes pod, triggers LocationsUpdater
        // - Last pod → Engine removes pod, triggers deletion checker, eventually deletes NAT Gateway
        az.diffTracker.Engine.DeletePod(
            staticGatewayConfigurationName, // serviceUID (egress name)
            pod.Status.HostIP,              // location
            pod.Status.PodIP,               // address
        )
    } else {
        klog.Errorf("podInformerRemovePod: Pod %s has service %s not found in localServiceNameToNRPServiceMap",
            podKey, staticGatewayConfigurationName)
    }
}
```

### Step 16: Cleanup - Remove PodEgressQueue and Related Code

**Files to Modify**:

1. **`pkg/provider/difftracker/types.go`**
   - Remove `PodEgressQueue` field from `DiffTracker` struct
   - Remove `PodCrudEvent`, `AddPodEvent`, `DeletePodEvent` types (no longer needed)

2. **`pkg/provider/difftracker/difftracker.go`**
   - Remove `PodEgressQueue` initialization from `InitializeDiffTracker()`

3. **`pkg/provider/azure_servicegateway_difftracker_init.go`**
   - Remove `podEgressResourceUpdater` creation and startup
   - Remove `go updater.run(ctx)` call

**Files to Delete**:

4. **`pkg/provider/azure_servicegateway_pod_egress_resource_updater.go`**
   - Delete entire file (~200 lines)
   - Functionality fully replaced by Engine.AddPod() and Engine.DeletePod()

## Testing Strategy

### Unit Tests

1. **TestEngineAddPod_ServiceExists**
   - Service in StateCreated → pod added immediately
   - Verify `UpdateK8sPod()` called
   - Verify LocationsUpdater triggered

2. **TestEngineAddPod_ServiceCreating**
   - Service in StateCreationInProgress → pod buffered
   - Verify pod in `bufferedPods` map
   - Verify LocationsUpdater NOT triggered

3. **TestEngineAddPod_ServiceNotExists**
   - No service operation tracked → creates service and buffers pod
   - Verify service added to `pendingServiceOps`
   - Verify XUpdater triggered

4. **TestEngineDeletePod_NotLastPod**
   - Counter > 1 → pod removed, counter decremented
   - Verify `removePod()` called
   - Verify LocationsUpdater triggered

5. **TestEngineDeletePod_LastPod**
   - Counter == 1 → pod removed, service marked for deletion
   - Verify service in `pendingDeletions`
   - Verify DeletionChecker triggered

6. **TestPromoteBufferedPods**
   - Service created → all buffered pods promoted
   - Verify `bufferedPods` map cleared
   - Verify LocationsUpdater triggered

### Integration Tests

1. **TestPodAddedBeforeNATGateway**
   - Create pod with egress label
   - Verify pod buffered
   - NAT Gateway creation completes
   - Verify pod promoted and location/address synced

2. **TestMultiplePodsBuffered**
   - Create 5 pods with same egress label
   - Verify all 5 pods buffered
   - NAT Gateway creation completes
   - Verify all 5 pods promoted together

3. **TestLastPodDeletion**
   - Create pod → NAT Gateway created
   - Delete pod
   - Verify DeletionChecker waits for locations cleared
   - Verify NAT Gateway deleted after locations removed

4. **TestPodAddedAfterNATGateway**
   - Create NAT Gateway first
   - Add pod
   - Verify pod immediately added (no buffering)

## Metrics

Add the following metrics for observability:

1. `pod_buffer_duration_seconds` - Histogram of time pods spend buffered
2. `buffered_pods_total` - Gauge of currently buffered pods per service
3. `pod_promotion_total` - Counter of successful pod promotions
4. `pod_promotion_failures_total` - Counter of failed pod promotions

## Error Handling

### Failure Scenarios

1. **NAT Gateway creation fails**
   - Pods remain buffered
   - Retry logic in XUpdater continues
   - After max retries, clear buffered pods and log error

2. **Pod added after service deletion started**
   - Reject pod addition with warning
   - Don't buffer the pod

3. **Location/address sync fails after pod promotion**
   - LocationsUpdater has retry logic
   - Pods remain in DiffTracker state
   - Will be retried on next LocationsUpdater trigger

## Migration Path

This implementation is backward compatible. Existing code paths continue to work:

1. Direct `UpdateK8sPod()` calls still work (for non-Engine flows)
2. Engine methods are additive (new functionality)
3. Can be rolled out incrementally by changing one call site at a time

## Timeline Example

```
T0: Pod created with egress label "my-egress"
T1: podInformerAddPod() called → Engine.AddPod("my-egress", "ns/pod", "10.1.1.1", "10.2.2.2")
T2: Engine checks pendingServiceOps → not found
T3: Engine creates ServiceOperationState{State: StateCreationInProgress}
T4: Engine buffers pod in bufferedPods["my-egress"]
T5: Engine triggers XUpdater
T6: XUpdater.processBatch() spawns goroutine for "my-egress"
T7: Goroutine creates Public IP
T8: Goroutine creates NAT Gateway (5-10 seconds)
T9: Goroutine registers with Service Gateway
T10: Goroutine calls OnServiceCreationComplete("my-egress", true, nil)
T11: Engine calls promoteBufferedPodsLocked("my-egress")
T12: Engine calls UpdateK8sPod(ADD) for pod
T13: Engine triggers LocationsUpdater
T14: LocationsUpdater syncs location/address to NRP
T15: Pod successfully registered in Service Gateway
```

## Open Questions

1. **Buffered pod cleanup on failure**: If NAT Gateway creation fails after max retries, should we:
   - A) Clear buffered pods and log errors ✅ (chosen)
   - B) Keep buffered indefinitely and retry manually
   - C) Move pods to dead-letter queue for manual intervention

2. **Race between pod deletion and NAT Gateway creation**: If a pod is deleted while buffered, should we:
   - A) Remove it from buffer immediately ✅ (chosen)
   - B) Let it promote and immediately delete
   - C) Track deletion separately

3. **Metrics granularity**: Should pod buffer metrics be:
   - A) Per-service (high cardinality)
   - B) Aggregate (all services combined) ✅ (chosen)
   - C) Both with separate metric names

## Complete Component Implementation Order

### Phase 0: Prerequisites - Missing Type Definitions

**File**: `pkg/provider/difftracker/types.go`

Before implementing the Engine, add these missing type definitions:

```go
// ResourceState represents the lifecycle state of a service
type ResourceState int

const (
    StateNotStarted ResourceState = iota
    StateCreationInProgress
    StateCreated
    StateDeletionPending
    StateDeletionInProgress
)

// PodOperation represents the operation type for pod updates
type PodOperation int

const (
    ADD PodOperation = iota
    REMOVE
    UPDATE
)

// UpdatePodInputType contains parameters for pod add/remove operations
type UpdatePodInputType struct {
    PodOperation           PodOperation
    PublicOutboundIdentity string
    Location               string // HostIP
    Address                string // PodIP
}
```

**File**: `pkg/provider/difftracker/engine.go`

Add constants for retry logic:

```go
const (
    maxRetries = 3
)
```

**File**: `pkg/provider/azure_endpoints.go` (or appropriate location)

Add helper function to extract Service UID from EndpointSlice:

```go
// getServiceUIDFromEndpointSlice extracts the Service UID from EndpointSlice owner references
func getServiceUIDFromEndpointSlice(eps *discovery.EndpointSlice) string {
    for _, ownerRef := range eps.OwnerReferences {
        if ownerRef.Kind == "Service" {
            return string(ownerRef.UID)
        }
    }
    return ""
}

// getNodeIPByName retrieves the internal IP address for a node by name
func (az *Cloud) getNodeIPByName(nodeName string) string {
    az.diffTracker.mu.Lock()
    defer az.diffTracker.mu.Unlock()
    
    if node, exists := az.diffTracker.K8sResources.Nodes[nodeName]; exists {
        return node.InternalIP
    }
    return ""
}
```

**File**: `pkg/provider/difftracker/x_updater.go`

Add missing import:

```go
import (
    "context"
    "fmt"
    "sync"
    "time"

    "github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
    "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
    "k8s.io/klog/v2"
    "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)
```

**File**: `pkg/provider/azure_servicegateway_difftracker_init.go`

Add initialization code:

```go
// initializeDiffTrackerEngine creates the XUpdater and LocationsUpdater instances
func (az *Cloud) initializeDiffTrackerEngine() {
    az.diffTracker.xUpdater = NewXUpdater(az.diffTracker, az)
    az.diffTracker.locationsUpdater = NewLocationsUpdater(az.diffTracker, az)
}

// startDiffTrackerEngine starts the Engine goroutines
func (az *Cloud) startDiffTrackerEngine(ctx context.Context) {
    go az.diffTracker.xUpdater.Run(ctx)
    go az.diffTracker.locationsUpdater.Run(ctx)
    klog.Infof("DiffTracker Engine started: XUpdater and LocationsUpdater goroutines running")
}
```

**File**: `pkg/provider/difftracker/types.go`

Add updater fields to DiffTracker:

```go
type DiffTracker struct {
    mu sync.Mutex

    K8sResources K8s_State
    NRPResources NRP_State

    PodEgressQueue                  workqueue.TypedRateLimitingInterface[PodCrudEvent]
    LocalServiceNameToNRPServiceMap sync.Map

    InitialSyncDone bool

    // Engine-related fields
    pendingServiceOps       map[string]*ServiceOperationState
    bufferedEndpoints       map[string][]BufferedEndpointUpdate
    bufferedPods            map[string][]BufferedPodUpdate
    pendingDeletions        map[string]*PendingDeletion
    xUpdaterTrigger         chan string
    locationsUpdaterTrigger chan struct{}
    
    // Updater instances
    xUpdater          *XUpdater
    locationsUpdater  *LocationsUpdater
}
```

### Phase 1: Core Infrastructure (DiffTracker Extensions)

**Files to Create/Modify**:

1. `pkg/provider/difftracker/types.go`
   - Add `ResourceState` enum (StateNotStarted, StateCreationInProgress, etc.) - ✅ DONE IN PHASE 0
   - Add `ServiceOperationState` struct
   - Add `BufferedEndpointUpdate` struct
   - Add `BufferedPodUpdate` struct
   - Add `PendingDeletion` struct
   - Add new fields to `DiffTracker` struct

2. `pkg/provider/difftracker/difftracker.go`
   - Update `InitializeDiffTracker()` to initialize new maps and channels

### Phase 2: Engine Implementation

**Files to Create**:

3. `pkg/provider/difftracker/engine.go` (NEW FILE)
   ```go
   // Core methods:
   - AddService(serviceUID string, isInbound bool)
   - DeleteService(serviceUID string, isInbound bool)
   - UpdateEndpoints(serviceUID string, podIPToNodeIP map[string]string)
   - AddPod(egressUID, podKey, location, address string)
   - DeletePod(egressUID, location, address string)
   - OnServiceCreationComplete(serviceUID string, success bool, err error)
   
   // Helper methods:
   - promoteBufferedEndpointsLocked(serviceUID string)
   - promoteBufferedPodsLocked(serviceUID string)
   - checkPendingDeletions(ctx context.Context)
   - serviceHasLocationsInNRP(serviceUID string) bool
   ```

**Add DeletionChecker Methods** (same file `engine.go`):

```go
// checkPendingDeletions checks each pending deletion to see if locations are cleared
// This method is called by LocationsUpdater after syncing location changes
func (dt *DiffTracker) checkPendingDeletions(ctx context.Context) {
    dt.mu.Lock()
    defer dt.mu.Unlock()

    if len(dt.pendingDeletions) == 0 {
        klog.V(6).Infof("Engine.checkPendingDeletions: No pending deletions")
        return
    }

    klog.V(4).Infof("Engine.checkPendingDeletions: Checking %d pending deletions", len(dt.pendingDeletions))

    for serviceUID, pendingDel := range dt.pendingDeletions {
        opState, exists := dt.pendingServiceOps[serviceUID]
        if !exists {
            klog.Warningf("Engine.checkPendingDeletions: Service %s in pendingDeletions but not in pendingServiceOps, removing", serviceUID)
            delete(dt.pendingDeletions, serviceUID)
            continue
        }

        // Only process services in StateDeletionPending
        if opState.State != StateDeletionPending {
            klog.V(4).Infof("Engine.checkPendingDeletions: Service %s is in state %v, not StateDeletionPending", serviceUID, opState.State)
            continue
        }

        // Check if service still has locations in NRP
        hasLocations := dt.serviceHasLocationsInNRP(serviceUID)

        if hasLocations {
            elapsed := time.Since(pendingDel.Timestamp)
            klog.V(4).Infof("Engine.checkPendingDeletions: Service %s still has locations in NRP (waiting %v), will retry",
                serviceUID, elapsed)
            continue
        }

        // Locations cleared - proceed with deletion
        klog.V(2).Infof("Engine.checkPendingDeletions: Service %s has no locations in NRP, proceeding with deletion", serviceUID)

        // Update state to DeletionInProgress
        opState.State = StateDeletionInProgress
        opState.LastAttempt = time.Now()

        // Trigger XUpdater to delete the service
        select {
        case dt.xUpdaterTrigger <- serviceUID:
            klog.V(4).Infof("Engine.checkPendingDeletions: Triggered XUpdater to delete service %s", serviceUID)
        default:
            klog.V(2).Infof("Engine.checkPendingDeletions: XUpdater channel full for service %s", serviceUID)
        }

        // Remove from pendingDeletions (will be removed from pendingServiceOps by XUpdater after deletion)
        delete(dt.pendingDeletions, serviceUID)
    }
}

// serviceHasLocationsInNRP checks if any locations in NRP reference this service
func (dt *DiffTracker) serviceHasLocationsInNRP(serviceUID string) bool {
    // Iterate through all NRP locations
    for locationKey, nrpLocation := range dt.NRPResources.Locations {
        // Check addresses within the location
        for addrKey, addr := range nrpLocation.Addresses {
            if addr.InboundServiceIdentity == serviceUID {
                klog.V(4).Infof("Engine.serviceHasLocationsInNRP: Address %s in location %s references inbound service %s",
                    addrKey, locationKey, serviceUID)
                return true
            }

            if addr.OutboundServiceIdentity == serviceUID {
                klog.V(4).Infof("Engine.serviceHasLocationsInNRP: Address %s in location %s references outbound service %s",
                    addrKey, locationKey, serviceUID)
                return true
            }
        }
    }

    klog.V(4).Infof("Engine.serviceHasLocationsInNRP: No locations reference service %s", serviceUID)
    return false
}
```

### Phase 3: XUpdater Implementation

**Files to Create**:

4. `pkg/provider/difftracker/x_updater.go` (NEW FILE)

**Complete Implementation**:

```go
package difftracker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

const (
	// maxConcurrentCreations limits parallel service creation goroutines
	maxConcurrentCreations = 10
)

// XUpdater handles parallel creation and deletion of Azure resources (LB, NAT Gateway)
type XUpdater struct {
	diffTracker *DiffTracker
	az          *provider.Cloud
	wg          sync.WaitGroup
	semaphore   chan struct{} // Limits concurrent operations
}

// NewXUpdater creates a new XUpdater
func NewXUpdater(dt *DiffTracker, az *provider.Cloud) *XUpdater {
	return &XUpdater{
		diffTracker: dt,
		az:          az,
		semaphore:   make(chan struct{}, maxConcurrentCreations),
	}
}

// Run is the main loop that processes service creation/deletion requests
func (xu *XUpdater) Run(ctx context.Context) {
	klog.Infof("XUpdater: Starting with max %d concurrent operations", maxConcurrentCreations)

	for {
		select {
		case <-ctx.Done():
			klog.Infof("XUpdater: Context cancelled, waiting for goroutines to finish")
			xu.wg.Wait()
			klog.Infof("XUpdater: All goroutines finished, stopping")
			return

		case serviceUID := <-xu.diffTracker.xUpdaterTrigger:
			klog.V(4).Infof("XUpdater: Received trigger for service %s", serviceUID)
			xu.processBatch(ctx, serviceUID)
		}
	}
}

// processBatch spawns a goroutine to handle service creation or deletion
func (xu *XUpdater) processBatch(ctx context.Context, serviceUID string) {
	xu.diffTracker.mu.Lock()
	opState, exists := xu.diffTracker.pendingServiceOps[serviceUID]
	if !exists {
		xu.diffTracker.mu.Unlock()
		klog.Warningf("XUpdater: Service %s not found in pendingServiceOps", serviceUID)
		return
	}

	isInbound := opState.IsInbound
	state := opState.State
	xu.diffTracker.mu.Unlock()

	// Acquire semaphore to limit concurrent operations
	select {
	case xu.semaphore <- struct{}{}:
		// Got slot
	case <-ctx.Done():
		return
	}

	// Spawn goroutine for this service
	xu.wg.Add(1)
	go func() {
		defer xu.wg.Done()
		defer func() { <-xu.semaphore }() // Release semaphore

		switch state {
		case StateCreationInProgress:
			if isInbound {
				xu.createInboundService(ctx, serviceUID)
			} else {
				xu.createOutboundService(ctx, serviceUID)
			}

		case StateDeletionInProgress:
			if isInbound {
				xu.deleteInboundService(ctx, serviceUID)
			} else {
				xu.deleteOutboundService(ctx, serviceUID)
			}

		default:
			klog.Warningf("XUpdater: Unexpected state %v for service %s", state, serviceUID)
		}
	}()
}

// createInboundService creates a Load Balancer for inbound traffic
func (xu *XUpdater) createInboundService(ctx context.Context, serviceUID string) {
	klog.V(2).Infof("XUpdater.createInboundService: Creating Load Balancer for service %s", serviceUID)

	// Step 1: Create or get Public IP
	klog.V(4).Infof("XUpdater.createInboundService: Step 1/3 - Creating Public IP for %s", serviceUID)
	pipName := fmt.Sprintf("%s-pip", serviceUID)
	pip := armnetwork.PublicIPAddress{
		Location: &xu.az.Location,
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
			PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
		},
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
		},
	}

	err := xu.az.CreateOrUpdatePIP(ctx, xu.az.ResourceGroup, pipName, pip)
	if err != nil {
		klog.Errorf("XUpdater.createInboundService: Failed to create Public IP for %s: %v", serviceUID, err)
		xu.diffTracker.OnServiceCreationComplete(serviceUID, false, err)
		return
	}

	// Step 2: Create Load Balancer
	klog.V(4).Infof("XUpdater.createInboundService: Step 2/3 - Creating Load Balancer for %s", serviceUID)
	lbName := serviceUID
	lb := armnetwork.LoadBalancer{
		Location: &xu.az.Location,
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
				{
					Name: to.Ptr("frontend"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
								xu.az.SubscriptionID, xu.az.ResourceGroup, pipName)),
						},
					},
				},
			},
			BackendAddressPools: []*armnetwork.BackendAddressPool{
				{
					Name: to.Ptr("backend"),
				},
			},
		},
		SKU: &armnetwork.LoadBalancerSKU{
			Name: to.Ptr(armnetwork.LoadBalancerSKUNameStandard),
		},
	}

	err = xu.az.CreateOrUpdateLB(ctx, xu.az.ResourceGroup, lbName, lb)
	if err != nil {
		klog.Errorf("XUpdater.createInboundService: Failed to create Load Balancer %s: %v", serviceUID, err)
		xu.diffTracker.OnServiceCreationComplete(serviceUID, false, err)
		return
	}

	klog.V(2).Infof("XUpdater.createInboundService: Load Balancer %s created successfully", serviceUID)

	// Step 3: Register with NRP Service Gateway
	klog.V(4).Infof("XUpdater.createInboundService: Step 3/3 - Registering %s with Service Gateway", serviceUID)

	// Get current services from Service Gateway
	xu.diffTracker.mu.Lock()
	servicesDTO := xu.buildServicesDTO()
	xu.diffTracker.mu.Unlock()

	err = xu.az.UpdateNRPSGWServices(ctx, xu.az.ServiceGatewayResourceName, servicesDTO)
	if err != nil {
		klog.Errorf("XUpdater.createInboundService: Failed to register Load Balancer %s with Service Gateway: %v",
			serviceUID, err)
		xu.diffTracker.OnServiceCreationComplete(serviceUID, false, err)
		return
	}

	klog.V(2).Infof("XUpdater.createInboundService: Load Balancer %s registered with Service Gateway", serviceUID)

	// Add to NRPResources to reflect creation
	xu.diffTracker.mu.Lock()
	xu.diffTracker.NRPResources.LoadBalancers.Insert(serviceUID)
	xu.diffTracker.mu.Unlock()

	// Notify Engine that creation succeeded
	xu.diffTracker.OnServiceCreationComplete(serviceUID, true, nil)
}

// createOutboundService creates a NAT Gateway for outbound traffic
func (xu *XUpdater) createOutboundService(ctx context.Context, natUID string) {
	klog.V(2).Infof("XUpdater.createOutboundService: Creating NAT Gateway for egress %s", natUID)

	// Step 1: Create or get Public IP
	klog.V(4).Infof("XUpdater.createOutboundService: Step 1/3 - Creating Public IP for %s", natUID)
	pipName := fmt.Sprintf("%s-pip", natUID)
	pip := armnetwork.PublicIPAddress{
		Location: &xu.az.Location,
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
			PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
		},
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
		},
	}

	err := xu.az.CreateOrUpdatePIP(ctx, xu.az.ResourceGroup, pipName, pip)
	if err != nil {
		klog.Errorf("XUpdater.createOutboundService: Failed to create Public IP for %s: %v", natUID, err)
		xu.diffTracker.OnServiceCreationComplete(natUID, false, err)
		return
	}

	// Step 2: Create NAT Gateway
	klog.V(4).Infof("XUpdater.createOutboundService: Step 2/3 - Creating NAT Gateway for %s", natUID)
	natGatewayName := natUID
	natGateway := armnetwork.NatGateway{
		Location: &xu.az.Location,
		Properties: &armnetwork.NatGatewayPropertiesFormat{
			PublicIPAddresses: []*armnetwork.SubResource{
				{
					ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
						xu.az.SubscriptionID, xu.az.ResourceGroup, pipName)),
				},
			},
		},
		SKU: &armnetwork.NatGatewaySKU{
			Name: to.Ptr(armnetwork.NatGatewaySKUNameStandard),
		},
	}

	err = xu.az.CreateOrUpdateNatGateway(ctx, xu.az.ResourceGroup, natGatewayName, natGateway)
	if err != nil {
		klog.Errorf("XUpdater.createOutboundService: Failed to create NAT Gateway %s: %v", natUID, err)
		xu.diffTracker.OnServiceCreationComplete(natUID, false, err)
		return
	}

	klog.V(2).Infof("XUpdater.createOutboundService: NAT Gateway %s created successfully", natUID)

	// Step 3: Register with NRP Service Gateway
	klog.V(4).Infof("XUpdater.createOutboundService: Step 3/3 - Registering %s with Service Gateway", natUID)

	// Get current services from Service Gateway
	xu.diffTracker.mu.Lock()
	servicesDTO := xu.buildServicesDTO()
	xu.diffTracker.mu.Unlock()

	err = xu.az.UpdateNRPSGWServices(ctx, xu.az.ServiceGatewayResourceName, servicesDTO)
	if err != nil {
		klog.Errorf("XUpdater.createOutboundService: Failed to register NAT Gateway %s with Service Gateway: %v",
			natUID, err)
		xu.diffTracker.OnServiceCreationComplete(natUID, false, err)
		return
	}

	klog.V(2).Infof("XUpdater.createOutboundService: NAT Gateway %s registered with Service Gateway", natUID)

	// Add to NRPResources to reflect creation
	xu.diffTracker.mu.Lock()
	xu.diffTracker.NRPResources.NATGateways.Insert(natUID)
	xu.diffTracker.mu.Unlock()

	// Notify Engine that creation succeeded
	xu.diffTracker.OnServiceCreationComplete(natUID, true, nil)
}

// deleteInboundService deletes a Load Balancer
func (xu *XUpdater) deleteInboundService(ctx context.Context, serviceUID string) {
	klog.V(2).Infof("XUpdater.deleteInboundService: Deleting Load Balancer %s", serviceUID)

	// Step 1: Unregister from NRP Service Gateway
	klog.V(4).Infof("XUpdater.deleteInboundService: Step 1/3 - Unregistering %s from Service Gateway", serviceUID)

	xu.diffTracker.mu.Lock()
	servicesDTO := xu.buildServicesDTO() // Will exclude services in deletion state
	xu.diffTracker.mu.Unlock()

	err := xu.az.UpdateNRPSGWServices(ctx, xu.az.ServiceGatewayResourceName, servicesDTO)
	if err != nil {
		klog.Errorf("XUpdater.deleteInboundService: Failed to unregister Load Balancer %s: %v", serviceUID, err)
		return // Don't proceed with deletion if unregister fails
	}

	// Step 2: Delete Load Balancer
	klog.V(4).Infof("XUpdater.deleteInboundService: Step 2/3 - Deleting Load Balancer %s", serviceUID)
	err = xu.az.DeleteLB(ctx, xu.az.ResourceGroup, serviceUID)
	if err != nil {
		klog.Errorf("XUpdater.deleteInboundService: Failed to delete Load Balancer %s: %v", serviceUID, err)
		return
	}

	klog.V(2).Infof("XUpdater.deleteInboundService: Load Balancer %s deleted successfully", serviceUID)

	// Step 3: Delete Public IP (if not shared)
	klog.V(4).Infof("XUpdater.deleteInboundService: Step 3/3 - Deleting Public IP for %s", serviceUID)
	pipName := fmt.Sprintf("%s-pip", serviceUID)
	err = xu.az.DeletePIP(ctx, xu.az.ResourceGroup, pipName)
	if err != nil {
		klog.Warningf("XUpdater.deleteInboundService: Failed to delete Public IP %s: %v", pipName, err)
		// Non-fatal - continue
	}

	// Remove from NRPResources
	xu.diffTracker.mu.Lock()
	xu.diffTracker.NRPResources.LoadBalancers.Delete(serviceUID)
	delete(xu.diffTracker.pendingServiceOps, serviceUID)
	delete(xu.diffTracker.pendingDeletions, serviceUID)
	xu.diffTracker.mu.Unlock()

	klog.V(2).Infof("XUpdater.deleteInboundService: Service %s fully deleted", serviceUID)
}

// deleteOutboundService deletes a NAT Gateway
func (xu *XUpdater) deleteOutboundService(ctx context.Context, natUID string) {
	klog.V(2).Infof("XUpdater.deleteOutboundService: Deleting NAT Gateway %s", natUID)

	// Step 1: Unregister from NRP Service Gateway
	klog.V(4).Infof("XUpdater.deleteOutboundService: Step 1/3 - Unregistering %s from Service Gateway", natUID)

	xu.diffTracker.mu.Lock()
	servicesDTO := xu.buildServicesDTO() // Will exclude services in deletion state
	xu.diffTracker.mu.Unlock()

	err := xu.az.UpdateNRPSGWServices(ctx, xu.az.ServiceGatewayResourceName, servicesDTO)
	if err != nil {
		klog.Errorf("XUpdater.deleteOutboundService: Failed to unregister NAT Gateway %s: %v", natUID, err)
		return // Don't proceed with deletion if unregister fails
	}

	// Step 2: Delete NAT Gateway
	klog.V(4).Infof("XUpdater.deleteOutboundService: Step 2/3 - Deleting NAT Gateway %s", natUID)
	err = xu.az.DeleteNatGateway(ctx, xu.az.ResourceGroup, natUID)
	if err != nil {
		klog.Errorf("XUpdater.deleteOutboundService: Failed to delete NAT Gateway %s: %v", natUID, err)
		return
	}

	klog.V(2).Infof("XUpdater.deleteOutboundService: NAT Gateway %s deleted successfully", natUID)

	// Step 3: Delete Public IP (if not shared)
	klog.V(4).Infof("XUpdater.deleteOutboundService: Step 3/3 - Deleting Public IP for %s", natUID)
	pipName := fmt.Sprintf("%s-pip", natUID)
	err = xu.az.DeletePIP(ctx, xu.az.ResourceGroup, pipName)
	if err != nil {
		klog.Warningf("XUpdater.deleteOutboundService: Failed to delete Public IP %s: %v", pipName, err)
		// Non-fatal - continue
	}

	// Remove from NRPResources
	xu.diffTracker.mu.Lock()
	xu.diffTracker.NRPResources.NATGateways.Delete(natUID)
	delete(xu.diffTracker.pendingServiceOps, natUID)
	delete(xu.diffTracker.pendingDeletions, natUID)
	xu.diffTracker.mu.Unlock()

	klog.V(2).Infof("XUpdater.deleteOutboundService: Service %s fully deleted", natUID)
}

// buildServicesDTO constructs the services DTO from current DiffTracker state
// Excludes services in deletion state
func (xu *XUpdater) buildServicesDTO() map[string]interface{} {
	// Must be called with diffTracker.mu held
	servicesDTO := make(map[string]interface{})

	// Add inbound services (Load Balancers)
	for serviceUID := range xu.diffTracker.NRPResources.LoadBalancers {
		opState, exists := xu.diffTracker.pendingServiceOps[serviceUID]
		if exists && (opState.State == StateDeletionPending || opState.State == StateDeletionInProgress) {
			continue // Skip services being deleted
		}

		servicesDTO[serviceUID] = map[string]interface{}{
			"type":     "loadbalancer",
			"inbound":  true,
			"resource": fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/%s",
				xu.az.SubscriptionID, xu.az.ResourceGroup, serviceUID),
		}
	}

	// Add outbound services (NAT Gateways)
	for natUID := range xu.diffTracker.NRPResources.NATGateways {
		opState, exists := xu.diffTracker.pendingServiceOps[natUID]
		if exists && (opState.State == StateDeletionPending || opState.State == StateDeletionInProgress) {
			continue // Skip services being deleted
		}

		servicesDTO[natUID] = map[string]interface{}{
			"type":     "natgateway",
			"inbound":  false,
			"resource": fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/natGateways/%s",
				xu.az.SubscriptionID, xu.az.ResourceGroup, natUID),
		}
	}

	return servicesDTO
}
```

**Key Implementation Details**:

1. **Concurrency Control**: Semaphore limits max 10 concurrent operations
2. **Goroutine Tracking**: sync.WaitGroup ensures clean shutdown
3. **Sequential Steps**: Each service creation/deletion follows strict order
4. **Error Handling**: ARM client handles retries, XUpdater just reports success/failure
5. **State Synchronization**: Updates NRPResources after creation/deletion
6. **Callback Pattern**: Calls OnServiceCreationComplete() to promote buffered data

### Phase 4: LocationsUpdater Modifications

**Files to Modify**:

5. `pkg/provider/azure_servicegateway_location_service_updater.go`

**Complete Implementation**:

```go
package provider

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

// LocationsUpdater syncs location and address changes to NRP Service Gateway
type LocationsUpdater struct {
	diffTracker *DiffTracker
	az          *Cloud
}

// NewLocationsUpdater creates a new LocationsUpdater
func NewLocationsUpdater(dt *DiffTracker, az *Cloud) *LocationsUpdater {
	return &LocationsUpdater{
		diffTracker: dt,
		az:          az,
	}
}

// Run is the main loop that processes location update requests
func (lu *LocationsUpdater) Run(ctx context.Context) {
	klog.Infof("LocationsUpdater: Starting")

	for {
		select {
		case <-ctx.Done():
			klog.Infof("LocationsUpdater: Context cancelled, stopping")
			return

		case <-lu.diffTracker.locationsUpdaterTrigger:
			klog.V(4).Infof("LocationsUpdater: Triggered by channel")
			lu.process(ctx)
		}
	}
}

// process computes location/address diff and syncs to NRP
func (lu *LocationsUpdater) process(ctx context.Context) {
	startTime := time.Now()
	klog.V(2).Infof("LocationsUpdater: Starting location sync")

	// Get locations and addresses diff from DiffTracker
	lu.diffTracker.mu.Lock()
	locationsDTO, err := lu.diffTracker.GetSyncLocationsAddresses()
	lu.diffTracker.mu.Unlock()

	if err != nil {
		klog.Errorf("LocationsUpdater: Failed to compute diff: %v", err)
		return
	}

	if locationsDTO == nil || len(locationsDTO) == 0 {
		klog.V(4).Infof("LocationsUpdater: No changes to sync")
		return
	}

	klog.V(4).Infof("LocationsUpdater: Syncing %d location updates", len(locationsDTO))

	// Call NRP Service Gateway API to update locations/addresses
	err = lu.az.UpdateNRPSGWAddressLocations(ctx, lu.az.ServiceGatewayResourceName, locationsDTO)
	if err != nil {
		klog.Errorf("LocationsUpdater: Failed to update locations in NRP: %v", err)
		// ARM client will retry automatically
		return
	}

	duration := time.Since(startTime)
	klog.V(2).Infof("LocationsUpdater: Successfully synced locations to NRP in %v", duration)

	// Update NRPResources to reflect the sync
	lu.diffTracker.mu.Lock()
	for locationKey, locationData := range locationsDTO {
		// Update NRPResources.Locations with synced data
		if locationData.Operation == "delete" {
			delete(lu.diffTracker.NRPResources.Locations, locationKey)
		} else {
			lu.diffTracker.NRPResources.Locations[locationKey] = convertDTOToNRPLocation(locationData)
		}
	}
	lu.diffTracker.mu.Unlock()

	// Check pending deletions after location sync
	// This allows pending deletions to proceed if locations are now cleared
	lu.diffTracker.checkPendingDeletions(ctx)
}

// convertDTOToNRPLocation converts location DTO to NRPLocation format
func convertDTOToNRPLocation(dto map[string]interface{}) NRPLocation {
	// Extract location data from DTO
	location := NRPLocation{
		Addresses: make(map[string]NRPAddress),
	}

	if inboundService, ok := dto["inboundServiceIdentity"].(string); ok {
		location.InboundServiceIdentity = inboundService
	}

	if outboundService, ok := dto["outboundServiceIdentity"].(string); ok {
		location.OutboundServiceIdentity = outboundService
	}

	if addresses, ok := dto["addresses"].(map[string]interface{}); ok {
		for addrKey, addrData := range addresses {
			if addrMap, ok := addrData.(map[string]interface{}); ok {
				addr := NRPAddress{}
				if inbound, ok := addrMap["inboundServiceIdentity"].(string); ok {
					addr.InboundServiceIdentity = inbound
				}
				if outbound, ok := addrMap["outboundServiceIdentity"].(string); ok {
					addr.OutboundServiceIdentity = outbound
				}
				location.Addresses[addrKey] = addr
			}
		}
	}

	return location
}
```

**Key Implementation Details**:

1. **Event-Driven**: Only runs when triggered via `locationsUpdaterTrigger` channel
2. **Diff Computation**: Uses existing `GetSyncLocationsAddresses()` to compute K8s vs NRP diff
3. **NRP Sync**: Calls `UpdateNRPSGWAddressLocations()` to sync changes
4. **State Update**: Updates `NRPResources.Locations` after successful sync
5. **DeletionChecker Trigger**: Notifies DeletionChecker after sync completes
6. **Integration**: Works with existing DiffTracker location tracking logic

**When Triggered**:
- After Engine promotes buffered endpoints (`promoteBufferedEndpointsLocked`)
- After Engine promotes buffered pods (`promoteBufferedPodsLocked`)
- After Engine adds pod immediately (`AddPod` when service ready)
- After Engine updates endpoints immediately (`UpdateEndpoints` when service ready)
- After Engine deletes pod (`DeletePod`)

### Phase 5: DeletionChecker Implementation

**Note**: DeletionChecker is implemented as simple methods in `engine.go` (Phase 2), not as a separate file or goroutine.

The following methods are added to the DiffTracker/Engine in `pkg/provider/difftracker/engine.go`:

1. `checkPendingDeletions(ctx context.Context)` - Called by LocationsUpdater after syncing
2. `serviceHasLocationsInNRP(serviceUID string) bool` - Helper to check if locations reference a service

**Key Implementation Details**:

1. **Event-Driven**: No polling, called directly by LocationsUpdater after each location sync
2. **Location Verification**: Checks address-level references to the service in NRP
3. **State Transition**: Only processes services in `StateDeletionPending`, transitions to `StateDeletionInProgress` when ready
4. **XUpdater Trigger**: Sends service UID to XUpdater for actual deletion once locations cleared
5. **Cleanup**: Removes from `pendingDeletions` map after triggering XUpdater
6. **No Goroutine**: Simple synchronous methods, no background processing

### Phase 6: Inbound Flow Integration

**Files to Modify**:

7. `pkg/provider/azure_loadbalancer.go`
   - Modify `EnsureLoadBalancer()` to use Engine when ServiceGatewayEnabled
   - Modify `EnsureLoadBalancerDeleted()` to use Engine when ServiceGatewayEnabled
   - Keep existing synchronous code as fallback

8. `pkg/provider/azure_endpoints.go` (if exists) or endpoint handler
   - Wire EndpointSlice informer to call `Engine.UpdateEndpoints()`

### Phase 7: Outbound Flow Integration

**Files to Modify**:

9. `pkg/provider/azure_servicegateway_pods.go`
   - Modify `podInformerAddPod()` to call `Engine.AddPod()` directly
   - Modify `podInformerRemovePod()` to call `Engine.DeletePod()` directly
   - Remove all `PodEgressQueue` references
   - Remove queue-based delayed processing logic

**Files to Delete**:

10. `pkg/provider/azure_servicegateway_pod_egress_resource_updater.go`
    - DELETE this file entirely - functionality replaced by Engine + XUpdater

11. `pkg/provider/azure_servicegateway_difftracker_init.go`
    - Initialize and start Engine goroutines (XUpdater, LocationsUpdater only)
    ```go
    func (az *Cloud) initializeDiffTrackerEngine() {
        az.diffTracker.xUpdater = NewXUpdater(az.diffTracker, az)
        az.diffTracker.locationsUpdater = NewLocationsUpdater(az.diffTracker, az)
    }
    
    func (az *Cloud) startDiffTrackerEngine(ctx context.Context) {
        go az.diffTracker.xUpdater.Run(ctx)
        go az.diffTracker.locationsUpdater.Run(ctx)
        klog.Infof("DiffTracker Engine started: XUpdater and LocationsUpdater goroutines running")
        // Note: DeletionChecker is not a goroutine - it's called directly by LocationsUpdater
    }
    ```

12. `pkg/provider/azure_cloud.go` or main initialization
    - Call `startDiffTrackerEngine()` during cloud provider initialization

### Phase 9: Azure Resource Operations

**Files to Verify/Create**:

13. `pkg/provider/azure_loadbalancer_repo.go`
    - Verify `CreateOrUpdateLB()` exists and works with Engine
    - Verify `DeleteLB()` exists

14. `pkg/provider/azure_natgateway_repo.go`
    - Verify `createOrUpdateNatGateway()` exists
    - Verify `deleteNatGateway()` exists

15. `pkg/provider/azure_publicip_repo.go`
    - Verify PIP creation/deletion methods exist

16. `pkg/provider/azure_servicegateway.go`
    - Verify `UpdateNRPSGWServices()` exists
    - Verify `UpdateNRPSGWAddressLocations()` exists

### Phase 10: Testing

**Test Files to Create**:

17. `pkg/provider/difftracker/engine_test.go`
    - TestEngineAddService
    - TestEngineDeleteService
    - TestEngineUpdateEndpoints_Buffering
    - TestEngineAddPod_ServiceExists
    - TestEngineAddPod_ServiceCreating
    - TestEngineAddPod_ServiceNotExists
    - TestEngineDeletePod_NotLastPod
    - TestEngineDeletePod_LastPod
    - TestPromoteBufferedEndpoints
    - TestPromoteBufferedPods

18. `pkg/provider/difftracker/x_updater_test.go`
    - TestXUpdaterCreateInboundService
    - TestXUpdaterCreateOutboundService
    - TestXUpdaterDeleteInboundService
    - TestXUpdaterDeleteOutboundService
    - TestXUpdaterRetryLogic
    - TestXUpdaterParallelExecution

19. `pkg/provider/difftracker/deletion_checker_test.go`
    - TestDeletionCheckerWaitsForLocations
    - TestDeletionCheckerTriggersXUpdater
    - TestDeletionCheckerMultipleDeletions

20. Integration tests in `tests/e2e/`
    - TestInboundServiceCreation_WithEarlyEndpoints
    - TestOutboundNATGatewayCreation_WithEarlyPods
    - TestServiceDeletion_OrderedCleanup
    - TestMultipleServicesParallel

## Complete Implementation Checklist

### Phase 0: Prerequisites
- [ ] 0.1: Add ResourceState enum to types.go
- [ ] 0.2: Add PodOperation enum to types.go
- [ ] 0.3: Add UpdatePodInputType struct to types.go
- [ ] 0.4: Add maxRetries constant to engine.go
- [ ] 0.5: Add getServiceUIDFromEndpointSlice() helper function
- [ ] 0.6: Add getNodeIPByName() helper method
- [ ] 0.7: Add to.Ptr import to x_updater.go
- [ ] 0.8: Add xUpdater and locationsUpdater fields to DiffTracker struct
- [ ] 0.9: Add initializeDiffTrackerEngine() function
- [ ] 0.10: Add startDiffTrackerEngine() function

### Phase 1: Core Infrastructure
- [ ] 1.1: Add ServiceOperationState struct to types.go
- [ ] 1.2: Add BufferedEndpointUpdate struct to types.go
- [ ] 1.3: Add BufferedPodUpdate struct to types.go
- [ ] 1.4: Add PendingDeletion struct to types.go
- [ ] 1.5: Add new fields to DiffTracker struct in types.go
- [ ] 1.6: Update InitializeDiffTracker() in difftracker.go

### Phase 2: Engine Implementation
- [ ] 2.1: Create engine.go file
- [ ] 2.2: Implement AddService() method
- [ ] 2.3: Implement DeleteService() method
- [ ] 2.4: Implement UpdateEndpoints() method
- [ ] 2.5: Implement AddPod() method
- [ ] 2.6: Implement DeletePod() method
- [ ] 2.7: Implement OnServiceCreationComplete() method
- [ ] 2.8: Implement promoteBufferedEndpointsLocked() helper
- [ ] 2.9: Implement promoteBufferedPodsLocked() helper
- [ ] 2.10: Implement checkPendingDeletions() method
- [ ] 2.11: Implement serviceHasLocationsInNRP() helper

### Phase 3: XUpdater Implementation
- [ ] 3.1: Create x_updater.go file
- [ ] 3.2: Implement Run() main loop
- [ ] 3.3: Implement processBatch() method
- [ ] 3.4: Implement createInboundService() goroutine
- [ ] 3.5: Implement createOutboundService() goroutine
- [ ] 3.6: Implement deleteInboundService() goroutine
- [ ] 3.7: Implement deleteOutboundService() goroutine
- [ ] 3.8: Add sync.WaitGroup for goroutine tracking

### Phase 4: LocationsUpdater Modifications
- [ ] 4.1: Modify process() to use locationsUpdaterTrigger channel
- [ ] 4.2: Add DeletionChecker trigger after location updates
- [ ] 4.3: Verify GetSyncLocationsAddresses() integration
- [ ] 4.4: Verify UpdateNRPSGWAddressLocations() calls

### Phase 5: DeletionChecker Implementation
- [ ] 5.1: Verify checkPendingDeletions() method added in Phase 2 (engine.go)
- [ ] 5.2: Verify serviceHasLocationsInNRP() helper added in Phase 2 (engine.go)
- [ ] 5.3: Verify Engine.DeleteService() calls serviceHasLocationsInNRP() for immediate check
- [ ] 5.4: Update LocationsUpdater.process() to call diffTracker.checkPendingDeletions() after sync

### Phase 6: Inbound Flow Integration
- [ ] 6.1: Modify EnsureLoadBalancer() in azure_loadbalancer.go
- [ ] 6.2: Add ServiceGatewayEnabled check
- [ ] 6.3: Call Engine.AddService() for async path
- [ ] 6.4: Return immediately with placeholder status
- [ ] 6.5: Modify EnsureLoadBalancerDeleted() in azure_loadbalancer.go
- [ ] 6.6: Call Engine.DeleteService() for async path
- [ ] 6.7: Wire EndpointSlice informer to Engine.UpdateEndpoints()

### Phase 7: Outbound Flow Integration
- [ ] 7.1: Modify podInformerAddPod() to call Engine.AddPod() directly
- [ ] 7.2: Add validation checks (labels, IPs) in podInformerAddPod()
- [ ] 7.3: Remove direct UpdateK8sPod() calls from podInformerAddPod()
- [ ] 7.4: Modify podInformerRemovePod() to call Engine.DeletePod() directly
- [ ] 7.5: Remove all PodEgressQueue references from azure_servicegateway_pods.gogo file
- [ ] 7.7: Remove podEgressResourceUpdater initialization from difftracker_init.go

### Phase 8: Startup and Initialization
- [ ] 8.1: Create initializeDiffTrackerEngine() in difftracker_init.go
- [ ] 8.2: Create startDiffTrackerEngine() in difftracker_init.go
- [ ] 8.3: Start XUpdater goroutine in startDiffTrackerEngine()
- [ ] 8.4: Start LocationsUpdater goroutine in startDiffTrackerEngine()
- [ ] 8.5: Call initializeDiffTrackerEngine() in cloud provider init
- [ ] 8.6: Call startDiffTrackerEngine() in cloud provider init
- [ ] 8.7: Add context cancellation handling

### Phase 9: Azure Resource Operations
- [ ] 9.1: Verify CreateOrUpdateLB() in azure_loadbalancer_repo.go
- [ ] 9.2: Verify DeleteLB() exists
- [ ] 9.3: Verify createOrUpdateNatGateway() in azure_natgateway_repo.go
- [ ] 9.4: Verify deleteNatGateway() exists
- [ ] 9.5: Verify PIP creation methods in azure_publicip_repo.go
- [ ] 9.6: Verify UpdateNRPSGWServices() in azure_servicegateway.go
- [ ] 9.7: Verify UpdateNRPSGWAddressLocations() exists
- [ ] 9.8: Add proper error handling in all resource operations

### Phase 10: Testing
- [ ] 10.1: Create engine_test.go
- [ ] 10.2: Write TestEngineAddService
- [ ] 10.3: Write TestEngineDeleteService
- [ ] 10.4: Write TestEngineUpdateEndpoints_Buffering
- [ ] 10.5: Write TestEngineAddPod_ServiceExists
- [ ] 10.6: Write TestEngineAddPod_ServiceCreating
- [ ] 10.7: Write TestEngineAddPod_ServiceNotExists
- [ ] 10.8: Write TestEngineDeletePod_NotLastPod
- [ ] 10.9: Write TestEngineDeletePod_LastPod
- [ ] 10.10: Write TestPromoteBufferedEndpoints
- [ ] 10.11: Write TestPromoteBufferedPods
- [ ] 10.12: Create x_updater_test.go
- [ ] 10.13: Write XUpdater unit tests
- [ ] 10.14: Add DeletionChecker tests to engine_test.go (checkPendingDeletions, serviceHasLocationsInNRP)
- [ ] 10.15: Write integration tests for inbound flow
- [ ] 10.17: Write integration tests for outbound flow
- [ ] 10.18: Write integration tests for deletion flow
- [ ] 10.19: Write integration tests for parallel execution

### Phase 11: Metrics and Observability
- [ ] 11.1: Add service_creation_duration_seconds histogram
- [ ] 11.2: Add buffered_endpoints_total gauge
- [ ] 11.3: Add buffered_pods_total gauge
- [ ] 11.4: Add endpoint_buffer_duration_seconds histogram
- [ ] 11.5: Add pod_buffer_duration_seconds histogram
- [ ] 11.6: Add service_creation_failures_total counter
- [ ] 11.7: Add service_deletion_duration_seconds histogram
- [ ] 11.8: Add pending_deletions_total gauge
- [ ] 11.9: Add x_updater_goroutines_active gauge
- [ ] 11.10: Add locations_sync_duration_seconds histogram

### Phase 12: Documentation
- [ ] 12.1: Update architecture diagrams
- [ ] 12.2: Document inbound flow
- [ ] 12.3: Document outbound flow
- [ ] 12.4: Document deletion flow
- [ ] 12.5: Document Engine API
- [ ] 12.6: Document XUpdater behavior
- [ ] 12.7: Document DeletionChecker logic
- [ ] 12.8: Add troubleshooting guide
- [ ] 12.9: Add migration guide from synchronous to async
- [ ] 12.10: Update README with new architecture

## File Creation Summary

### New Files to Create:
1. `pkg/provider/difftracker/engine.go` (~750 lines - includes all Engine methods + DeletionChecker methods)
2. `pkg/provider/difftracker/x_updater.go` (~800 lines)
3. `pkg/provider/difftracker/engine_test.go` (~1400 lines - includes all Engine tests + DeletionChecker tests)
4. `pkg/provider/difftracker/x_updater_test.go` (~800 lines)

### Files to Modify:
1. `pkg/provider/difftracker/types.go` (+~150 lines, remove PodEgressQueue field and deletionCheckerTrigger channel)
2. `pkg/provider/difftracker/difftracker.go` (+~50 lines, remove PodEgressQueue init and deletionCheckerTrigger init)
3. `pkg/provider/azure_loadbalancer.go` (+~30 lines)
4. `pkg/provider/azure_servicegateway_pods.go` (+~30 lines, modify ~50 lines, remove queue logic)
5. `pkg/provider/azure_servicegateway_location_service_updater.go` (+~150 lines - complete implementation)
6. `pkg/provider/azure_servicegateway_difftracker_init.go` (+~20 lines, remove updater start)
7. `pkg/provider/azure_cloud.go` (+~5 lines)

### Files to Delete:
1. `pkg/provider/azure_servicegateway_pod_egress_resource_updater.go` (DELETE entire file ~200 lines)

**Total Estimated Lines**: ~3,650 new lines of code (net reduction of ~395 lines)

## References

- **Sequence Diagrams**: See "Complete Processing Flows" section above
- **DiffTracker Design**: `pkg/provider/difftracker/types.go`
- **Engine Design**: To be created in `pkg/provider/difftracker/engine.go`
- **XUpdater Design**: To be created in `pkg/provider/difftracker/x_updater.go`
- **Integration Points**: `azure_loadbalancer.go`, `azure_servicegateway_pods.go`
- **Azure SDK References**: `armnetwork/v6` for LoadBalancer, NatGateway, PublicIP
