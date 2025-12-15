# CLB DiffTracker Engine - Architecture Review

## Overview

Asynchronous, non-blocking CLB processing with **event-driven buffering** and **ordered deletion**.

**Key Changes**:
- Returns immediately to Kubernetes (no blocking)
- Parallel resource creation (one goroutine per service)
- Early events buffered until service ready
- Locations cleared before service deletion

---

## Architecture

```
K8s Informers → DiffTracker Engine → XUpdater / LocationsUpdater → NRP API
                     ↓
              5 maps + 2 channels
```

**Components**:
- **DiffTracker Engine**: State management, buffering logic (no goroutines)
- **XUpdater**: Creates/deletes LB/NAT Gateway (1 goroutine + N spawned)
- **LocationsUpdater**: Syncs locations to NRP (1 goroutine)

---

## Core Data Structures

```go
// State machine: 5 states
type ResourceState int
const (
    StateNotStarted           // Initial
    StateCreationInProgress   // Buffering enabled
    StateCreated              // Buffering disabled, immediate updates
    StateDeletionPending      // Waiting for locations cleared
    StateDeletionInProgress   // XUpdater deleting
)

// Service tracking
type ServiceOperationState struct {
    ServiceUID  string
    IsInbound   bool  // true=LB, false=NAT
    State       ResourceState
    RetryCount  int   // Max 3
    LastAttempt time.Time
}

// Buffering structures
type BufferedEndpointUpdate struct {
    PodIPToNodeIP map[string]string
    Timestamp     time.Time
}

type BufferedPodUpdate struct {
    PodKey    string  // "ns/name"
    Location  string  // HostIP
    Address   string  // PodIP
    Timestamp time.Time
}
```

---

## Key Maps & State Flow

### 1. `pendingServiceOps` (serviceUID → ServiceOperationState)

**Behavior by State**:

| State | Endpoints/Pods | Action |
|-------|----------------|--------|
| `StateCreationInProgress` | **Buffer** data | Wait for XUpdater |
| `StateCreated` | **Immediate** update | Apply directly |
| `StateDeletionPending` | Reject | Wait for locations cleared |

**Lifecycle**:
```
Create → StateCreationInProgress (buffer) 
       → StateCreated (promote buffers)
       → StateDeletionPending (wait)
       → StateDeletionInProgress (XUpdater deletes)
       → DELETED (removed from map)
```

### 2. `bufferedEndpoints` / `bufferedPods` (serviceUID → []Updates)

**Purpose**: Hold early arrivals until service ready

**Cleared When**:
- Service creation succeeds → promoted to DiffTracker
- Service creation fails (after 3 retries) → discarded
- Service deleted → discarded

### 3. `pendingDeletions` (serviceUID → PendingDeletion)

**Purpose**: Services waiting for location cleanup

**Flow**: 
```
DeleteService → pendingDeletions[uid] created
              → LocationsUpdater syncs removal
              → checkPendingDeletions() verifies cleared
              → Triggers XUpdater deletion
```

### 4. `LocalServiceNameToNRPServiceMap` (egress → pod count)

**Purpose**: Track pods per NAT Gateway

**Last Pod Deletion**:
```go
if counter == 1 {
    Delete(egress)  // Last pod
    Mark for deletion → pendingDeletions[egress]
}
```

---

## API Contracts

### Engine Public API (Called by K8s Informers)

```go
// Inbound (LB)
AddService(serviceUID, isInbound bool)
UpdateEndpoints(serviceUID, podIPToNodeIP map[string]string)
DeleteService(serviceUID, isInbound bool)

// Outbound (NAT)
AddPod(serviceUID, podKey, location, address string)
DeletePod(serviceUID, location, address string)
```

**Guarantees**: Thread-safe, idempotent, non-blocking

### XUpdater Callback (After Service Creation)

```go
OnServiceCreationComplete(serviceUID, success bool, err error)
```

**Purpose**: Promote buffered data or retry on failure (max 3 retries)

### Communication Channels

```go
xUpdaterTrigger         chan string      // Buffer: 100 (serviceUID)
locationsUpdaterTrigger chan struct{}    // Buffer: 1 (signal only)
```

**xUpdaterTrigger**: DiffTracker → XUpdater (create/delete service)
**locationsUpdaterTrigger**: DiffTracker → LocationsUpdater (sync locations)

### DeletionChecker (Called by LocationsUpdater)

```go
checkPendingDeletions(ctx)          // After each NRP sync
serviceHasLocationsInNRP(uid) bool  // Check if locations cleared
```

---

## Flow Examples

### 1. Service Creation with Early Endpoints (Inbound)

```
Service created → AddService() 
                → State = CreationInProgress
                → Trigger XUpdater

EndpointSlice arrives early → UpdateEndpoints()
                            → State check: CreationInProgress
                            → bufferedEndpoints[uid] stores data

XUpdater completes → OnServiceCreationComplete(success=true)
                   → State = Created
                   → promoteBufferedEndpointsLocked()
                   → UpdateK8sEndpoints()
                   → Trigger LocationsUpdater
```

### 2. Pod Triggers NAT Gateway Creation (Outbound)

```
Pod with egress label → AddPod()
                      → Check pendingServiceOps: NOT FOUND
                      → Create State = CreationInProgress
                      → bufferedPods[egress] stores pod
                      → Trigger XUpdater

XUpdater creates NAT → OnServiceCreationComplete(success=true)
                     → promoteBufferedPodsLocked()
                     → addOrUpdatePod()
                     → LocalServiceNameToNRPServiceMap[egress]++
                     → Trigger LocationsUpdater
```

### 3. Last Pod Deletion (Ordered Cleanup)

```
Last pod deleted → DeletePod()
                 → Counter check: == 1 (last pod)
                 → State = StateDeletionPending
                 → pendingDeletions[egress] created
                 → Trigger LocationsUpdater

LocationsUpdater syncs removal → checkPendingDeletions()
                               → serviceHasLocationsInNRP() = false
                               → State = DeletionInProgress
                               → Trigger XUpdater to delete NAT Gateway
```

---

## Implementation Details

### Thread Safety
- All Engine methods acquire `dt.mu`
- XUpdater/LocationsUpdater acquire lock when accessing DiffTracker

### Channel Buffers
- `xUpdaterTrigger`: 100 (queue multiple services)
- `locationsUpdaterTrigger`: 1 (signal coalescing)

### Idempotency
All Engine methods check existence before modifying state.

### Retry Logic
```go
maxRetries = 3
OnServiceCreationComplete(fail) → RetryCount++
  If RetryCount >= 3 → delete all state (give up)
  Else → trigger XUpdater for retry
```

---

## Concurrency

| Component | Goroutines |
|-----------|-----------|
| DiffTracker Engine | 0 (synchronous) |
| XUpdater | 1 + N (max 10 concurrent creates) |
| LocationsUpdater | 1 |

**Total**: 2 goroutines at steady state

---

## Review Checklist

1. **State Transitions**: Valid? (CreationInProgress → Created → DeletionPending → DeletionInProgress)
2. **Retry Logic**: maxRetries=3 sufficient? Exponential backoff needed?
3. **Edge Cases**: Pod deleted while buffered? Add `deletedWhileBuffered` map?
4. **Stuck Deletions**: Add timeout (5 min) to force deletion if locations not clearing?
5. **Thread Safety**: All critical sections protected by `dt.mu`?
6. **Metrics**: Which metrics are critical for production monitoring?
