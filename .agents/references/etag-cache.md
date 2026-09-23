# ETag & Cache Invalidation

## Problem Overview

Azure uses ETags for optimistic concurrency control. When the SDK sends a
PUT/PATCH request for a resource, the `azclient/policy/etag` policy extracts
the `etag` field from the request body JSON and sets the `If-Match` HTTP header.
If the server-side ETag has changed since the resource was last fetched, Azure
returns HTTP 412 PreconditionFailed.

The Interface client also uses this policy for NIC updates. Backend-pool
membership changes read the NIC (including its ETag) before writing it. Without
`If-Match`, a delayed update can recreate a NIC after its VM and NIC were
deleted, leaving a VM-less NIC in a load-balancer backend pool. The conditional
update fails instead; a later reconciliation must fetch current state before
trying again. NIC creations without an ETag remain unconditional. This prevents
new stale-write orphans but does not remove ones that already exist. Because
`pkg/azclient` is a separate Go module, cloud-controller-manager builds need a
new azclient release and dependency bump before they pick up this protection.

NIC validation from `pkg/azclient`:

```sh
go test ./interfaceclient ./policy/etag -count=1
```

The handwritten offline NIC tests cover accepted conditional writes, stale
ETags and recovery after a fresh read, deletion between GET and PUT, and new
NIC creation with nil or empty ETags. They exercise the real SDK pipeline
against a simulated server; they do not establish live NRP behavior.

Every ETag-enabled client with generated create/update operations verifies the
outgoing `If-Match` header in its generated tests. A fake credential and request
interceptor exercise the SDK pipeline without Azure or cassette interactions.
The test requires the exact interception error, so a missing recording or an
unrelated failure cannot satisfy it. It copies the resource fixture rather
than changing the shared fixture's ETag.

The separate NIC live spec requires an Azure response error with status 412 and code
`PreconditionFailed`, and checks GET returns 404 before and after a delayed
PUT to a deleted NIC. It also checks accepted updates and fresh-read recovery.
This spec explicitly skips during replay. To execute it, record the existing
NIC integration suite with the selected `interfaceclient/testdata/<AZURE_CLOUD>.yaml`
cassette moved aside, using `AZURE_SUBSCRIPTION_ID`, `AZURE_TENANT_ID`, and
authorized Azure credentials for a disposable test subscription. The suite
creates and deletes a resource group and VNet; do not run it against a
production subscription. No synthetic Azure recording is supplied as live
evidence.

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
