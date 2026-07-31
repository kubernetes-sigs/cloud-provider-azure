---
title: "Proactive Traffic Redirection with Azure Load Balancer Admin State"
linkTitle: "Load Balancer Admin State"
type: docs
description: >
    Use the Azure Load Balancer administrative state to immediately exclude nodes from load balancing during maintenance or spot VM evictions.
---

## Overview

The cloud-provider-azure component will leverage the Azure Load Balancer (ALB) administrative state feature to instantly stop routing new connections to nodes that are out of service, being evicted, or otherwise leaving service. When the provider detects any of the following durable signals on a node, it immediately marks that node's backend entry as `Down` on every managed load balancer:

- The GA `node.kubernetes.io/out-of-service` taint. This is the user-facing trigger intended to be applied by operators or maintenance automation (for example during planned maintenance or node replacement). It can also be applied manually as a user-side override if other automated signals fail.
- The cloud shutdown taint `node.cloudprovider.kubernetes.io/shutdown` (`cloudproviderapi.TaintNodeShutdown`). This is applied by the cloud-node-lifecycle-controller when the cloud reports the node is shutdown, and it serves as a direct cloud-derived trigger so draining does not depend on successfully adding any derived taints.
- A `PreemptScheduled` event signaling an imminent Spot eviction. This path is persisted by adding the taint `cloudprovider.azure.microsoft.com/draining=spot-eviction` so the signal survives controller restarts.

This bypasses ALB's probe-based detection window (typically two failed probes at a five-second interval) and reduces the time unhealthy nodes receive traffic by up to ten seconds. Because the change is transparent to the Kubernetes API surface and applies uniformly to Standard Load Balancer deployments, dual-stack services, and both NIC- and IP-based backend pools, every Kubernetes cluster using cloud-provider-azure benefits without any configuration updates. If the out-of-service, shutdown, or spot-eviction taint is later removed, the provider restores the admin state to `None` so traffic resumes normally.

## Motivation

- **Eliminate probe lag:** Today ALB may continue to forward traffic to an out-of-service node until health probes fail, extending the window of client errors and timeouts.
- **Protect upgrades and spot workloads:** During planned maintenance and spot VM evictions, multiple services may suffer cascaded failures because traffic still lands on nodes that are already leaving service.
- **Preserve platform simplicity:** Customers should not have to modify Service manifests, CLI invocations, or policies to get better traffic cutover behavior. The feature must “just work.”

## Goals

- Cut over traffic away from out-of-service or evicted nodes immediately by setting ALB admin state to `Down`.
- Apply uniformly to all load-balanced Kubernetes services regardless of `externalTrafficPolicy` or backend pool type.
- Restore admin state to `None` when a node returns to service so steady-state behavior is unaffected.
- Avoid additional user-facing APIs, annotations, or CLI switches.

## Non-Goals

- Introducing user-tunable knobs for admin state behavior or probe intervals.
- Supporting freeze events (~5s host pauses); these remain probe-driven to avoid flapping.
- Changing Kubernetes pod eviction semantics; the feature complements, but does not replace, in-cluster pod evictions.
- Reacting to `kubectl drain` or node cordoning. The `kubectl drain` command evicts pods but does not apply the `node.kubernetes.io/out-of-service` taint; it only marks the node as unschedulable. This feature intentionally ignores generic unschedulable signals (see [Discussion: Generic Unschedulable Taints](#discussion-generic-unschedulable-taints)) because they toggle frequently and do not necessarily indicate that traffic should stop immediately.

## User Scenarios

- **`node.kubernetes.io/out-of-service` taint added (operator/automation trigger for upgrade/scale-down/manual maintenance):** When maintenance tooling (or an operator) adds the GA out-of-service taint to a node, ALB entries are marked down immediately, eliminating the probe delay during node removal. This is the primary user-facing trigger and can also be used as a manual override if other automated signals fail.
- **Cloud shutdown taint added (`node.cloudprovider.kubernetes.io/shutdown`):** When the cloud reports a node is shutdown, the cloud-node-lifecycle-controller applies `node.cloudprovider.kubernetes.io/shutdown` to the node. cloud-provider-azure reacts to this taint directly and marks backend entries down immediately, even if any derived taint updates (such as adding `node.kubernetes.io/out-of-service`) fail.
- **Spot VM eviction (`PreemptScheduled` event):** The first eviction event observed for a node causes the controller to taint the node with `cloudprovider.azure.microsoft.com/draining=spot-eviction`, which in turn triggers `AdminState = Down` to prevent new flows while the node is being preempted. Manually applying this taint has the same effect.
- **Out-of-service, shutdown, or spot-eviction taint removed:** When maintenance completes (or a node returns to service) and the relevant taints are cleared, admin state is restored to `None` so the node re-enters rotation.

For `PreemptScheduled` events, only the first event per node is acted upon; subsequent duplicate events are ignored until the node is recreated or the taint is cleared.

## Proposed Design

### High-Level Flow

1. Operators or maintenance automation add the GA `node.kubernetes.io/out-of-service` taint to a node, the cloud-node-lifecycle-controller adds the shutdown taint `node.cloudprovider.kubernetes.io/shutdown`, or Kubernetes emits a `PreemptScheduled` event indicating an imminent Spot eviction.
2. For Spot events, cloud-provider-azure patches the node with `cloudprovider.azure.microsoft.com/draining=spot-eviction` (one-time) so the signal persists beyond controller restarts; manual application of this taint is treated the same way.
3. The node informer observes the supported taints and records the node as out of service, then a dedicated admin-state controller enqueues the node for immediate backend updates (independent of the regular `reconcileService` path).
4. The provider resolves all impacted backend pools across every managed load balancer (handles multi-SLB, dual-stack) and identifies backend address entries that correspond to the node.
5. For each impacted backend pool, the provider updates the backend address pool via the Azure Network API (`LoadBalancerBackendAddressPoolsClient`) to set `AdminState = Down` on the matching backend address entries (one `CreateOrUpdate` per backend pool, only when changes are needed).
6. Existing TCP connections follow ALB semantics (established flows are honored); new flows are steered to healthy nodes.
7. When the taint is removed or the node is deleted/replaced, the backend entries either have their admin state restored to `None` or disappear as part of normal reconciliation.

### Signal Detection

- **Event detection:** Extend cloud-provider-azure to watch for the first `PreemptScheduled` event emitted for a node. When observed, patch the node with the taint `cloudprovider.azure.microsoft.com/draining=spot-eviction` if it is not already present and ignore subsequent events until the node is recreated or the taint is cleared.
- **Taint detection:** Use the node informer to detect addition/removal of `node.kubernetes.io/out-of-service`, `node.cloudprovider.kubernetes.io/shutdown`, or `cloudprovider.azure.microsoft.com/draining=spot-eviction`. These taints are the durable triggers for admin state changes; `node.kubernetes.io/out-of-service` and `cloudprovider.azure.microsoft.com/draining=spot-eviction` can also be applied manually by operators.
- **Trigger surface:** Use the existing Kubernetes informers; no new orchestrators or APIs are required.

### Provider Changes (pkg/provider/azure_loadbalancer.go)

- Introduce an `AdminStateManager` interface responsible for:
  - Translating a Kubernetes node into all relevant backend pool IDs (leveraging existing helpers such as `reconcileBackendPoolHosts`, `getBackendPoolIDsForService`, and the multiple-SLB bookkeeping).
  - Calling the Azure SDK (`LoadBalancerBackendAddressPoolsClient`) to update backend address pools (pool-scoped `Get`/`CreateOrUpdate`) and set `AdminState` on the matching backend address entries.
  - Caching the last-known state to avoid redundant calls and to surface metrics.
- Extend reconciliation paths:
  - `EnsureLoadBalancer` / `UpdateLoadBalancer`: remain unchanged but reuse the manager so periodic reconciliations can self-heal any missed admin-state updates.
  - `EnsureLoadBalancerDeleted`: rely on the same manager so nodes being deleted trigger state flips prior to resource removal.
- Add a lightweight `AdminStateController` with its own work queue: node/taint events enqueue node keys, and workers immediately resolve backend pools and apply the desired admin state without waiting for service/controller churn.
- Maintain thread-safety by reusing `serviceReconcileLock` around backend pool updates and by batching updates per backend pool invocation to minimize ARM round-trips without introducing extra delay.

### Admin State Controller Responsibilities

- Subscribe to the node informer and react only to supported taint transitions.
- On each event, enqueue a node key into a dedicated rate-limited work queue.
- Worker flow:
  1. Fetch the node object from the informer cache and compute desired admin state (`Down` when tainted, `None` when cleared).
  2. Resolve impacted backend pools across all managed load balancers and group desired changes by backend pool (not by individual backend address).
  3. Under `serviceReconcileLock`, fetch each impacted backend pool, apply `AdminState` changes to the matching backend address entries, and issue at most one `CreateOrUpdate` per backend pool (only when changes are needed). When multiple nodes are out of service concurrently, a single backend pool update may apply changes for multiple backend addresses in that pool.
  4. Record success/failure metrics and retry via the queue if needed.
  5. Emit a Kubernetes event on the node after the Azure updates complete (Normal on success, Warning on failure) so cluster admins can observe the effective state change.

### Backend Pool Update Strategy

Azure Load Balancer admin state is configured per backend address entry (`LoadBalancerBackendAddresses[*].Properties.AdminState`), but the update surface is the backend address pool subresource. As a result, admin state changes are applied by fetching the backend pool and issuing a pool-scoped `CreateOrUpdate` after mutating the relevant backend address entries.

To minimize Azure Network API calls while keeping cutover latency low, the `AdminStateManager` updates at backend pool granularity:

- Group desired changes by backend pool ID.
- For each backend pool, do a fresh `Get`, mutate `AdminState` on the matching backend address entries, and issue at most one `CreateOrUpdate` if anything changed.
- When multiple nodes are out of service concurrently, the manager can converge faster by applying `AdminState = Down` for all currently out-of-service nodes found in the pool in a single `CreateOrUpdate`, rather than performing separate updates per node.

### Concurrency and Locking

Backend pool updates are whole-resource updates (read-modify-write). To avoid lost updates when multiple controllers update backend pools at the same time, admin state updates are serialized with the existing `serviceReconcileLock`. Any other code paths that update backend pools (including the local-service backend pool updater in multiple-SLB mode) must also hold this lock around backend pool `Get`/`CreateOrUpdate` operations.

To make lock contention visible (and to catch regressions where a path forgets to take the lock), record a dedicated metric for time spent waiting to acquire `serviceReconcileLock` (see "Observability & Telemetry").

### Admin State Lifecycle

| Transition | Trigger | Action |
|------------|---------|--------|
| `None → Down` | `node.kubernetes.io/out-of-service`, `node.cloudprovider.kubernetes.io/shutdown`, **or** `cloudprovider.azure.microsoft.com/draining=spot-eviction` taint observed on the node | Update every backend address entry for the node to `AdminState = Down`. |
| `Down → None` | All supported draining taints removed from the node | Restore admin state to `None` so the node re-enters rotation. |
| `Down → Removed` | Node deleted/replaced after being tainted | Normal reconciliation removes backend entries; no extra call required. |

Duplicate `PreemptScheduled` events are ignored once the spot-eviction taint exists; failure to reach the Azure API leaves the node in its previous state, after which the controller logs, emits an event, and retries via the rate-limiting work queue's exponential backoff (matching the `CloudNodeController` pattern).

## Detailed Flow

### Out-of-Service Taint Added

1. The node informer observes the GA `node.kubernetes.io/out-of-service` taint being added to node `N` (typically by an operator or maintenance automation).
2. Cloud-provider-azure records `N` (tagged with the taint reason) in an in-memory out-of-service set for metrics and deduplication.
3. The admin state controller immediately enqueues `N`, resolves backend pools, and asks the admin manager to set `AdminState = Down` across them.
4. The manager computes per-IP-family backend IDs, respects multi-SLB configurations, and calls the Azure SDK to update admin state. Metrics and events confirm the action.

### `PreemptScheduled` Event

1. The events informer surfaces a `PreemptScheduled` warning for node `N`.
2. If the node does not already carry the taint `cloudprovider.azure.microsoft.com/draining=spot-eviction`, the controller patches the node to add it and records the Spot-eviction reason; duplicate events are ignored.
3. The node informer then observes the spot-eviction taint, which follows the same admin-state-controller path as the out-of-service flow, resulting in `AdminState = Down` across all backend pools.
4. When an operator removes the spot-eviction taint (for example after debugging), the controller restores the admin state to `None`. If the node object is deleted (as in a completed preemption), backend entries disappear naturally and no explicit restore call is necessary.

### Shutdown Taint Added

1. The node informer observes the cloud shutdown taint `node.cloudprovider.kubernetes.io/shutdown` being added to node `N` (applied by the cloud-node-lifecycle-controller when the cloud reports the node is shutdown).
2. The admin state controller immediately enqueues `N`, resolves backend pools, and asks the admin manager to set `AdminState = Down` across them.
3. If any other controllers also attempt to add derived taints (for example `node.kubernetes.io/out-of-service`) and that patch fails, the shutdown taint still provides a direct cloud-derived trigger so the backend admin state converges to `Down`.

### Out-of-Service / Shutdown / Spot-Eviction Taints Removed

1. The node informer detects that all supported draining taints (`node.kubernetes.io/out-of-service`, `node.cloudprovider.kubernetes.io/shutdown`, and/or `cloudprovider.azure.microsoft.com/draining=spot-eviction`) have been removed from node `N`.
2. Cloud-provider-azure clears `N` from the out-of-service set and enqueues reconciliation.
3. The admin manager sets `AdminState = None` on each backend entry via the dedicated controller so the node resumes receiving traffic. If the node was instead deleted (e.g., after a preemption), the backend entries disappear during normal reconciliation and no admin-state call is required.

## API and Configuration Impact

- No new Kubernetes APIs, annotations, or CLI flags.
- Azure surface remains the existing Load Balancer REST/ARM contract; only the admin state property is updated.
- Works with both NIC- and IP-based backend pools; creation-time limitation for NIC pools is handled by issuing updates post-creation.
- Introduce a provider configuration toggle (new boolean in `azure.json`/`azure.Config`) so operators can enable or disable the admin-state controller per cluster.

## Edge Cases and Mitigations

- **Single-pod services:** Downtime advances by a few seconds, but the service would fail once the node exits regardless; document expectation.
- **`externalTrafficPolicy: Local`:** The node-level admin state change affects all services equally; clients experience the same or better recovery because traffic stops sooner.
- **Azure VM freeze events:** Explicitly excluded—freeze duration (~5s) is shorter than the benefit and could introduce flapping.
- **Multiple standard load balancers:** Use existing placement metadata (`MultipleStandardLoadBalancerConfigurations`) to locate all backend entries and keep the active node/service maps accurate.
- **Dual-stack clusters:** Update both IPv4 and IPv6 backend pools using the per-family IDs already tracked during reconciliation.

## Observability & Telemetry

- Emit Kubernetes events on the node after admin state updates complete so cluster admins can observe the effective state:
  - Normal `LoadBalancerAdminStateDown` when `AdminState = Down` has been applied for the node across all managed load balancers/backend pools (and IP families).
  - Normal `LoadBalancerAdminStateNone` when `AdminState = None` has been restored.
  - Warning `LoadBalancerAdminStateUpdateFailed` when any Azure update fails; include the error and rely on the controller's rate-limiting work queue for exponential backoff and eventual convergence.
- Avoid per-Service events: a single node transition can affect many Services and would be too noisy; the node event is the canonical operator signal.
- Admin-state Azure SDK calls use the existing metrics helper (`pkg/metrics/azure_metrics.go`) with a dedicated prefix (for example `loadbalancer_adminstate`). Each operation wraps `MetricContext.ObserveOperationWithResult`, so latency, error, throttling, and rate-limit counters flow into the same Prometheus series already consumed by operations teams.
- **Lock contention metric:** Record time spent waiting to acquire `serviceReconcileLock`:
  - **Name:** `cloudprovider_azure_lock_wait_duration_seconds`
  - **Type:** Histogram
  - **Labels:** `lock` (e.g., `service_reconcile`), `caller` (e.g., `EnsureLoadBalancer`, `UpdateLoadBalancer`, `EnsureLoadBalancerDeleted`, `AdminStateController`)
  - **Buckets:** `ExponentialBuckets(0.001, 2, 14)` covering 1ms to ~8s (matches etcd/Kubernetes patterns for lock contention)
  - **Pattern:** Record `time.Since(start)` after `Lock()` returns, where `start` is captured immediately before the blocking call
- Include trace spans around Azure SDK calls via `trace.BeginReconcile` so admin state updates appear in standard diagnostics.

## Testing Strategy

- **Unit tests:** Cover admin manager logic (state transitions, backend ID resolution, retry/backoff).
- **Integration tests:** Fake Azure SDK to validate that reconciliations invoke the expected API calls under upgrade, taint removal, and eviction scenarios.
- **E2E tests:** Run upgrade/eviction workflows in test clusters, validate that admin state flips within milliseconds and that new connections are rejected while existing ones persist.
- **Regression tests:** Ensure services without out-of-service nodes behave identically (admin state remains `None`).

## Risks and Mitigations

- **Azure API throttling / rate limiting:**
  - **Azure client layer:** Backend pool and load balancer calls use the existing azclient token-bucket rate limiter (`rateLimitKey=loadBalancerRateLimit`). It is enabled by `cloudProviderRatelimit` (or `cloudProviderRateLimit`) and configured via `cloudProviderRateLimit{QPS,Bucket}{,Write}`; this can be overridden specifically for load balancer operations via the `loadBalancerRateLimit:` stanza in `azure.json`.
  - **Controller layer:** Transient failures (including API throttling) are retried via a standard rate-limiting work queue (exponential backoff; matching the `CloudNodeController` pattern in `pkg/nodemanager`) to avoid hot-loop retries during mass node out-of-service events.
  - **Call reduction:** One `CreateOrUpdate` per backend pool when changes are needed, skip no-ops, and coalesce multiple out-of-service nodes in the same pool into a single backend pool update.
- **Stale state:** Maintain an out-of-service set keyed by node UID so that replacement nodes with the same name do not inherit `Down` state.
- **Partial updates:** Log and raise Kubernetes events when the controller fails to update a backend so operators can intervene.
- **Concurrency with backend pool changes:** Serialize backend pool updates with `serviceReconcileLock` (see "Concurrency and Locking") and invalidate the LB cache entry (`lbCache.Delete`) before issuing Azure SDK calls, forcing a fresh GET that reflects any recent `reconcileBackendPoolHosts` write so etag conflicts are avoided even though the controller runs outside the service reconciliation loop.

## Discussion: Generic Unschedulable Taints

- **High churn:** Generic unschedulable signals (e.g., cordons or `node.kubernetes.io/unschedulable`) toggle frequently during node creation, kubelet upgrades, or short-term operator maintenance. Automatically reacting to every flip would enqueue repeated admin-state updates and risk ARM throttling without delivering customer value.
- **Ambiguous intent:** An unschedulable flag means “do not place new pods,” not necessarily “evict existing traffic right now.” Some operators cordon a node for debugging while keeping workloads running; forcing ALB `Down` in that window could reduce capacity unnecessarily.
- **Action:** The feature therefore limits itself to intentional, durable signals (`node.kubernetes.io/out-of-service`, `node.cloudprovider.kubernetes.io/shutdown`, and `cloudprovider.azure.microsoft.com/draining=spot-eviction`) and treats generic unschedulable signals as out of scope. If customers need a manual trigger, they can apply one of the supported taints explicitly once their node maintenance workflow is ready to cut traffic.
