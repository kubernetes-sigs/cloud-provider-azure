---
title: "Proactive Traffic Redirection with Azure Load Balancer Admin State"
linkTitle: "Load Balancer Admin State"
type: docs
description: >
    Use the Azure Load Balancer administrative state to drain nodes immediately during maintenance or spot VM evictions.
---

## Overview

The cloud-provider-azure component will leverage the Azure Load Balancer (ALB) administrative state feature to instantly stop routing new connections to nodes that are draining, evicted, or otherwise leaving service. When the provider detects either (a) the addition of the GA `node.kubernetes.io/out-of-service` taint or (b) a `PreemptScheduled` event signaling an imminent Spot eviction, it immediately marks that node's backend entry as `Down` on every managed load balancer. The latter path is persisted by adding the taint `cloudprovider.azure.microsoft.com/draining=spot-eviction` so the signal survives controller restarts. This bypasses ALB's probe-based detection window (typically two failed probes at a five-second interval) and reduces the time unhealthy nodes receive traffic by up to ten seconds. Because the change is transparent to the Kubernetes API surface and applies uniformly to Standard Load Balancer deployments, dual-stack services, and both NIC- and IP-based backend pools, every Kubernetes cluster using cloud-provider-azure benefits without any configuration updates. If the out-of-service or draining taint is later removed, the provider restores the admin state to `None` so traffic resumes normally.

## Motivation

- **Eliminate probe lag:** Today ALB may continue to forward traffic to a draining node until health probes fail, extending the window of client errors and timeouts.
- **Protect upgrades and spot workloads:** During planned maintenance and spot VM evictions, multiple services may suffer cascaded failures because traffic still lands on nodes that are already draining.
- **Preserve platform simplicity:** Customers should not have to modify Service manifests, CLI invocations, or policies to get better drain behavior. The feature must “just work.”

## Goals

- Cut over traffic away from draining or evicted nodes immediately by setting ALB admin state to `Down`.
- Apply uniformly to all load-balanced Kubernetes services regardless of `externalTrafficPolicy` or backend pool type.
- Restore admin state to `None` when a node returns to service so steady-state behavior is unaffected.
- Avoid additional user-facing APIs, annotations, or CLI switches.

## Non-Goals

- Introducing user-tunable knobs for admin state behavior or probe intervals.
- Supporting freeze events (~5s host pauses); these remain probe-driven to avoid flapping.
- Changing Kubernetes pod eviction semantics; the feature complements, but does not replace, in-cluster drains.

## User Scenarios

- **`node.kubernetes.io/out-of-service` taint added (upgrade/scale-down/manual maintenance):** When maintenance tooling adds the GA out-of-service taint to a node, ALB entries are marked down immediately, eliminating the probe delay during node removal.
- **Spot VM eviction (`PreemptScheduled` event):** The first eviction event observed for a node causes the controller to taint the node with `cloudprovider.azure.microsoft.com/draining=spot-eviction`, which in turn triggers `AdminState = Down` to prevent new flows while the node is being preempted. Manually applying this taint has the same effect.
- **Out-of-service or draining taint removed:** When maintenance completes and the taint is cleared, admin state is restored to `None` so the node re-enters rotation.

For `PreemptScheduled` events, only the first event per node is acted upon; subsequent duplicate events are ignored until the node is recreated or the taint is cleared.

## Proposed Design

### High-Level Flow

1. Kubernetes adds the `node.kubernetes.io/out-of-service` taint to a node or emits a `PreemptScheduled` event indicating an imminent Spot eviction.
2. For Spot events, cloud-provider-azure patches the node with `cloudprovider.azure.microsoft.com/draining=spot-eviction` (one-time) so the signal persists beyond controller restarts; manual application of this taint is treated the same way.
3. The node informer observes either taint and records the node as draining, then a dedicated admin-state controller enqueues the node for immediate backend updates (independent of the regular `reconcileService` path).
4. The provider resolves all backend pool entries referencing the node across every managed load balancer (handles multi-SLB, dual-stack).
5. For each entry, the provider issues an Azure Network API call to set `AdminState = Down`.
6. Existing TCP connections follow ALB semantics (established flows are honored); new flows are steered to healthy nodes.
7. When the taint is removed or the node is deleted/replaced, the backend entries either have their admin state restored to `None` or disappear as part of normal reconciliation.

### Signal Detection

- **Event detection:** Extend cloud-provider-azure to watch for the first `PreemptScheduled` event emitted for a node. When observed, patch the node with the taint `cloudprovider.azure.microsoft.com/draining=spot-eviction` if it is not already present and ignore subsequent events until the node is recreated or the taint is cleared.
- **Taint detection:** Use the node informer to detect addition/removal of either `node.kubernetes.io/out-of-service` or `cloudprovider.azure.microsoft.com/draining=spot-eviction`. These taints are the durable triggers for admin state changes and can also be applied manually by operators.
- **Trigger surface:** Use the existing Kubernetes informers; no new orchestrators or APIs are required.

### Provider Changes (pkg/provider/azure_loadbalancer.go)

- Introduce an `AdminStateManager` interface responsible for:
  - Translating a Kubernetes node into all relevant backend pool IDs (leveraging existing helpers such as `reconcileBackendPoolHosts`, `getBackendPoolIDsForService`, and the multiple-SLB bookkeeping).
  - Calling the Azure SDK (`LoadBalancerBackendAddressPoolsClient`) to update the specific backend address with `AdminState`.
  - Caching the last-known state to avoid redundant calls and to surface metrics.
- Extend reconciliation paths:
  - `EnsureLoadBalancer` / `UpdateLoadBalancer`: remain unchanged but reuse the manager so periodic reconciliations can self-heal any missed admin-state updates.
  - `EnsureLoadBalancerDeleted`: rely on the same manager so nodes being deleted trigger state flips prior to resource removal.
- Add a lightweight `AdminStateController` with its own work queue: node/taint events enqueue node keys, and workers immediately resolve backend pools and apply the desired admin state without waiting for service/controller churn.
- Maintain thread-safety by reusing `serviceReconcileLock` around Azure SDK calls and by batching updates per backend pool invocation to minimize ARM round-trips without introducing extra delay.

### Admin State Controller Responsibilities

- Subscribe to the node informer and react only to supported taint transitions.
- On each event, enqueue a node key into a dedicated rate-limited work queue.
- Worker flow:
  1. Fetch the node object from the informer cache and compute desired admin state (`Down` when tainted, `None` when cleared).
  2. Resolve backend entries using existing pool-mapping helpers.
  3. Issue Azure SDK calls immediately (within the same event) so the time between taint observation and traffic cutover is bounded only by ARM latency.
  4. Record success/failure metrics and retry via the queue if needed.

### Admin State Lifecycle

| Transition | Trigger | Action |
|------------|---------|--------|
| `None → Down` | `node.kubernetes.io/out-of-service` **or** `cloudprovider.azure.microsoft.com/draining=spot-eviction` taint observed on the node | Update every backend address entry for the node to `AdminState = Down`. |
| `Down → None` | All draining taints removed from the node | Restore admin state to `None` so the node re-enters rotation. |
| `Down → Removed` | Node deleted/replaced after being tainted | Normal reconciliation removes backend entries; no extra call required. |

Duplicate `PreemptScheduled` events are ignored once the draining taint exists; failure to reach the Azure API leaves the node in its previous state, after which the controller logs, emits an event, and retries with exponential backoff aligned with existing `reconcileLoadBalancer` cadence.

## Detailed Flow

### Out-of-Service Taint Added

1. The node informer observes the GA `node.kubernetes.io/out-of-service` taint being added to node `N`.
2. Cloud-provider-azure records `N` (tagged with the taint reason) in an in-memory draining set for metrics and deduplication.
3. The admin state controller immediately enqueues `N`, resolves backend pools, and asks the admin manager to set `AdminState = Down` across them.
4. The manager computes per-IP-family backend IDs, respects multi-SLB configurations, and calls the Azure SDK to update admin state. Metrics and events confirm the action.

### `PreemptScheduled` Event

1. The events informer surfaces a `PreemptScheduled` warning for node `N`.
2. If the node does not already carry the taint `cloudprovider.azure.microsoft.com/draining=spot-eviction`, the controller patches the node to add it and records the Spot-eviction reason; duplicate events are ignored.
3. The node informer then observes the draining taint, which follows the same admin-state-controller path as the out-of-service flow, resulting in `AdminState = Down` across all backend pools.
4. When an operator removes the draining taint (for example after debugging), the controller restores the admin state to `None`. If the node object is deleted (as in a completed preemption), backend entries disappear naturally and no explicit restore call is necessary.

### Draining Taint Removed

1. The node informer detects that all draining taints (`node.kubernetes.io/out-of-service` and/or `cloudprovider.azure.microsoft.com/draining=spot-eviction`) have been removed from node `N`.
2. Cloud-provider-azure clears `N` from the draining set and enqueues reconciliation.
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

- Emit structured events on the node and ClusterService to signal admin state transitions.
- Admin-state Azure SDK calls use the existing metrics helper (`pkg/metrics/azure_metrics.go`) with a dedicated prefix (for example `loadbalancer_adminstate`). Each operation wraps `MetricContext.ObserveOperationWithResult`, so latency, error, throttling, and rate-limit counters flow into the same Prometheus series already consumed by operations teams.
- Include trace spans around Azure SDK calls via `trace.BeginReconcile` so admin state updates appear in standard diagnostics.

## Testing Strategy

- **Unit tests:** Cover admin manager logic (state transitions, backend ID resolution, retry/backoff).
- **Integration tests:** Fake Azure SDK to validate that reconciliations invoke the expected API calls under upgrade, taint removal, and eviction scenarios.
- **E2E tests:** Run upgrade/eviction workflows in test clusters, validate that admin state flips within milliseconds and that new connections are rejected while existing ones persist.
- **Regression tests:** Ensure services without draining nodes behave identically (admin state remains `None`).

## Risks and Mitigations

- **Azure API throttling:** Batch updates per reconciliation, reuse SDK retry/backoff, and respect ARM rate limits. Graceful degradation is probe-based behavior.
- **Stale state:** Maintain a draining set keyed by node UID so that replacement nodes with the same name do not inherit `Down` state.
- **Partial updates:** Log and raise Kubernetes events when the controller fails to update a backend so operators can intervene.
- **Concurrency with backend pool changes:** The admin-state controller acquires the existing `serviceReconcileLock` and invalidates the LB cache entry (`lbCache.Delete`) before issuing Azure SDK calls, forcing a fresh GET that reflects any recent `reconcileBackendPoolHosts` write so etag conflicts are avoided even though the controller runs outside the service reconciliation loop.

## Discussion: Unschedulable Drain Taints

- **High churn:** Generic unschedulable signals (e.g., cordons or `node.kubernetes.io/unschedulable`) toggle frequently during node creation, kubelet upgrades, or short-term operator maintenance. Automatically reacting to every flip would enqueue repeated admin-state updates and risk ARM throttling without delivering customer value.
- **Ambiguous intent:** An unschedulable flag means “do not place new pods,” not necessarily “evict existing traffic right now.” Some operators cordon a node for debugging while keeping workloads running; forcing ALB `Down` in that window could reduce capacity unnecessarily.
- **Action:** The feature therefore limits itself to intentional, durable taints (`node.kubernetes.io/out-of-service` and `cloudprovider.azure.microsoft.com/draining=spot-eviction`) and treats generic unschedulable signals as out of scope. If customers need a manual trigger, they can apply one of the supported taints explicitly once their drain workflow is ready to cut traffic.
