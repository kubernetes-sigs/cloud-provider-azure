# ETag & Cache Invalidation

## Problem Overview

Azure uses ETags for optimistic concurrency control. When the SDK sends a
PUT/PATCH request for a resource, the `azclient/policy/etag` policy extracts
the `etag` field from the request body JSON and sets the `If-Match` HTTP header.
If the server-side ETag has changed since the resource was last fetched, Azure
returns HTTP 412 PreconditionFailed.

The subtle problem: **updating resource A can silently change resource B's ETag**.
If resource B is cached, the cache now holds a stale ETag. The next update to B
will fail with 412.

## Known Cross-Resource Contamination Pairs

| Trigger Operation | Affected Resource | Mechanism |
|-------------------|-------------------|-----------|
| External LB frontend IP config change (update/remove PIP reference) | Public IP (PIP) | LB update modifies `pip.properties.ipConfiguration.id`, which changes the PIP's ETag |
| VMSS VM instance update | VMSS | VM update triggers an internal Azure VMSS operation; the VMSS ETag changes afterwards (not immediately) |
| LB removal | Public IP (PIP) | Removing the LB removes the frontend IP config, which changes the PIP's reference and ETag |

This list covers known pairs discovered through production issues. There may be
undiscovered pairs — watch for unexpected 412 errors after cross-resource operations.

## Handling Strategies

The codebase uses three strategies to handle ETag mismatches:

### Strategy 1: Reactive — Invalidate cache on HTTP 412

When a create/update operation gets `StatusPreconditionFailed`, delete the cache
entry so the next attempt fetches a fresh resource with a current ETag.

**Code locations:**

| Resource | File | Functions |
|----------|------|-----------|
| Load Balancer | `pkg/provider/azure_loadbalancer_repo.go` | `CreateOrUpdateLB`, `CreateOrUpdateLBBackendPool`, `DeleteLBBackendPool` |
| Public IP | `pkg/provider/azure_publicip_repo.go` | `CreateOrUpdatePIP` |
| Security Group | `pkg/provider/securitygroup/azure_securitygroup_repo.go` | `CreateOrUpdateSecurityGroup` |

**Pattern:**
```go
if respError.StatusCode == http.StatusPreconditionFailed {
    logger.V(3).Info("Cache cleanup because of http.StatusPreconditionFailed", ...)
    _ = az.xxxCache.Delete(key)
}
```

### Strategy 2: Preventive — Invalidate related resource caches

When updating resource A is known to change resource B's ETag, proactively
invalidate B's cache after A's update succeeds.

**Code locations:**

| Trigger | Invalidated Cache | File | Context |
|---------|------------------|------|---------|
| External LB frontend IP config change | PIP cache (`pipCache`) | `pkg/provider/azure_loadbalancer.go` | After `reconcileLoadBalancer` updates an external LB with `fipChanged=true` |
| VMSS VM instance updates | VMSS cache (`vmssCache`) | `pkg/provider/azure_vmss.go` | After batch VM updates in `ensureHostsInPool` and `ensureBackendPoolDeleted` |
| VMSS pool update | VMSS cache (`vmssCache`) | `pkg/provider/azure_vmss.go` | After `ensureVMSSInPool` and `EnsureBackendPoolDeletedFromVMSets` |
| LB removal (`elbRemoved`) | PIP cache (full reinit) | `pkg/provider/azure_loadbalancer_repo.go` | Reinitializes `pipCache` entirely via `newPIPCache()` |

**Pattern (LB → PIP):**
```go
// Only external LBs affect PIPs. Internal LB changes (subnet/private IP) don't.
if fipChanged && !requiresInternalLoadBalancer(service) {
    pipResourceGroup := az.getPublicIPAddressResourceGroup(service)
    _ = az.pipCache.Delete(pipResourceGroup)
}
```

**Pattern (VMSS VM → VMSS):**
```go
hasVMUpdates := false
defer func() {
    if hasVMUpdates {
        _ = ss.vmssCache.Delete(consts.VMSSKey)
    }
}()
// ... after VM batch updates succeed:
hasVMUpdates = true
```

### Strategy 3: Reactive — Invalidate cache on operation cancellation

When concurrent operations cancel each other, the cached resource may be stale.

**Code locations:** Same `*_repo.go` files as Strategy 1 (LB, PIP, NSG repos).

**Pattern:**
```go
if strings.Contains(strings.ToLower(retryErrorMessage), consts.OperationCanceledErrorMessage) {
    logger.V(3).Info("Cache cleanup because CreateOrUpdate is canceled by another operation", ...)
    _ = az.xxxCache.Delete(key)
}
```

## ETag Pipeline Architecture

```
┌─────────────────┐    ┌────────────────────┐    ┌─────────────────────┐
│   pkg/cache     │    │    *_repo.go        │    │ azclient/policy/    │
│   TimedCache    │───►│ Cache read/write    │───►│ etag/etag.go        │
│                 │    │                     │    │                     │
│ .Get(key, crt)  │    │ Read: Get from      │    │ Intercepts PUT/     │
│ .Delete(key)    │    │   cache, resource   │    │ PATCH/POST requests │
│ .Update(key,    │    │   includes ETag     │    │                     │
│   data)         │    │                     │    │ Extracts "etag"     │
│                 │    │ Write: Invalidate   │    │ from JSON body →    │
│                 │    │   on 412, cancel,   │    │ sets If-Match       │
│                 │    │   or cross-resource  │    │ HTTP header         │
└─────────────────┘    └────────────────────┘    └─────────────────────┘
```

1. **pkg/cache** (`TimedCache`): stores Azure resources with their ETags as part of the cached object
2. **`*_repo.go`** files: read from cache for operations, invalidate on errors or cross-resource changes
3. **azclient/policy/etag**: HTTP pipeline policy that extracts the ETag from the request body JSON and sets the `If-Match` header for optimistic concurrency

## NIC Update-Only Writes

CCM only updates existing node NICs. All provider NIC writes go through
`Cloud.updateInterface` in `pkg/provider/azure_interface_repo.go`, which sets
`If-Match: *` through the Azure SDK request context. This includes adding and
removing backend-pool membership for standalone and VMSS Flex nodes. If a VM
deletion removes the NIC after CCM reads it, NRP rejects the delayed PUT with
HTTP 412 instead of recreating a VM-less NIC with stale backend membership.
Callers propagate the error through their existing reconciliation paths.

The wildcard checks existence, not the fetched resource version; it does not
prevent lost updates between concurrent writers. The shared azclient Interface
client remains create-capable for non-CCM consumers. Live validation against
NIC API version `2025-05-01` confirmed successful updates to an existing NIC
and `PreconditionFailedResourceDoesNotExist` for a deleted NIC, which remained
absent after the rejected PUT. This does not clean up previously orphaned NICs.

## Public-IP Recording Tests

The generated public-IP concurrency test uses a real stale ETag rather than a
malformed value: fetch the public IP, change its idle timeout using its current
ETag, then attempt another change using the old ETag and require HTTP 412.
A no-op update or malformed ETag is not a reliable concurrency check.

Check that the successful update changes the ETag only during live recording.
The recorder sanitizes ETag UUIDs to the same placeholder, so that inequality
cannot be asserted during playback. Changes to this scenario belong in the
client generator template as well as its generated test. Re-record both clouds
after changing the request sequence; do not synthesize successful recordings.

## Defer Ordering Pitfall (VMSS)

In `ensureHostsInPool` and `ensureBackendPoolDeleted`, there is a subtle defer
ordering issue. Per-node defers call `DeleteCacheForNode`, which triggers a cache
lookup that can **repopulate** the VMSS cache. If the VMSS cache invalidation
runs before these per-node defers, the cache ends up with stale data again.

**Fix:** Register the VMSS cache invalidation defer **early** in the function.
Go defers execute in LIFO (last-in, first-out) order, so an early-registered
defer runs **last** — after all per-node defers have completed.

```go
// Registered early in the function → runs LAST (after per-node defers)
hasVMUpdates := false
defer func() {
    if hasVMUpdates {
        _ = ss.vmssCache.Delete(consts.VMSSKey)
    }
}()

// ... later, per-node defers are registered in a loop:
defer func() {
    _ = ss.DeleteCacheForNode(ctx, nodeName)  // May repopulate vmss cache
}()

// ... after batch VM updates succeed:
hasVMUpdates = true  // Signals the early defer to clean up
```

If you modify these functions, ensure the VMSS cache invalidation defer remains
registered before any per-node defers.
