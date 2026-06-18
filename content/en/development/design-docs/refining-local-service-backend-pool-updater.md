---
title: "Refining Local Service Backend Pool Updater"
linkTitle: "Refining Local Service Backend Pool Updater"
type: docs
description: >
    Retry and error handling design for the local service backend pool updater.
---

# Refining Local Service Backend Pool Updater

## Purpose

The local service backend pool updater batches EndpointSlice-driven IP changes for local Services and applies them asynchronously to Azure Load Balancer backend pools. Today, when an updater batch observes an error from `Get` or `CreateOrUpdate`, it drops the batch after emitting a failure event, except for backend pool `404`, which is dropped without emitting a failure event (though a failure metric is currently recorded).

This design adds bounded retry for errors that are worth retrying at the updater layer. It does not duplicate Azure SDK retries for the status codes already handled by the SDK retry policy. The retry behavior is local to the updater, reuses the existing updater tick loop, and avoids unbounded requeue.

## Scope

In scope:

- Retry failed local-service backend-pool update operations when the error is classified as retriable.
- Add retry state to queued backend-pool update operations.
- Add a new max-retries configuration field for this updater.
- Emit retry and failure events with bounded event volume.
- Preserve current stale `404` event behavior. Stop recording `404` as a failure metric.
- Enrich `ErrTooManyRequest` with a `ThrottleError` type to expose `Retry-After` duration to callers. The `ThrottleError` change in `pkg/azclient/policy/retryrepectthrottled/` requires a separate `azclient` release and dependency bump before the retry implementation can use it.
- Add unit tests for retry, exhaustion, non-retry, and stale cases.

Out of scope:

- Redesigning multiple standard load balancer selection.
- Changing main Service reconciliation retries.
- Changing cleanup behavior for local service backend pools.

## Configuration

Add a new config field near `LoadBalancerBackendPoolUpdateIntervalInSeconds` in `pkg/provider/config/azure.go`:

```go
// LoadBalancerBackendPoolUpdateMaxRetries is the maximum number of retries for
// retriable local-service backend-pool update failures. Defaults to 3.
LoadBalancerBackendPoolUpdateMaxRetries *int `json:"loadBalancerBackendPoolUpdateMaxRetries,omitempty" yaml:"loadBalancerBackendPoolUpdateMaxRetries,omitempty"`
```

Add a default constant near `DefaultLoadBalancerBackendPoolUpdateIntervalInSeconds`:

```go
DefaultLoadBalancerBackendPoolUpdateMaxRetries = 3
```

Semantics:

- `nil` uses the default value `3`.
- Explicit `0` disables updater-level retry and preserves today's drop-after-failure behavior for retriable errors.
- Negative values are normalized to `0`.
- The count means extra retries after the first failed attempt. With the default `3`, a retriable error can be observed up to four times: one initial attempt plus three requeued retry attempts.

Use a pointer so the config loader can distinguish "unset" from "explicitly set to 0". Normalization happens next to the existing `LoadBalancerBackendPoolUpdateIntervalInSeconds` defaulting in `pkg/provider/azure.go`: if the pointer is nil, use `DefaultLoadBalancerBackendPoolUpdateMaxRetries`; if the pointer is non-nil and negative, clamp it to `0`.

The `omitempty` tag is intentional with the pointer field: a nil pointer is omitted and therefore defaults, while a non-nil pointer to `0` is preserved across marshal/unmarshal and keeps retry disabled.

## Retry Model

Use requeue-on-retriable-error. Each `loadBalancerBackendPoolUpdateOperation` carries retry metadata:

- `retryCount int`, tracking how many retryable failures have already been consumed
- `nextEligibleAt time.Time`, tracking when a throttled operation is eligible to be processed again

On each updater tick:

1. Acquire `serviceReconcileLock` to serialize with the main service reconciliation loop.
2. If the queue is empty (`countOperations() == 0`), release the lock and return.
3. Acquire `azureResourceLocker` (distributed lease lock) to serialize with other components that update load balancer resources. If the lease lock fails, return without draining. Operations remain in the queue for the next tick.
4. Drain the queued operations via `drainOperations()` (acquires `updater.lock`, moves all operations out, clears the queue, releases `updater.lock`).
5. Filter and group operations via `groupOperations()` under `serviceReconcileLock`:
   - the Service is still in `localServiceNameToServiceInfoMap`
   - the Service still points to the same load balancer
   - group relevant operations by `loadBalancerName/backendPoolName`
6. If any operation in a group has `nextEligibleAt` in the future, requeue the whole group. Do not call ARM, emit an event, record a metric, or increment retry state for skipped groups.
7. Process each eligible group once.
8. On retriable failure of a group, increment each constituent operation's `retryCount` uniformly.
9. Requeue operations whose retry budget remains (acquire `updater.lock` to prepend to the live queue, before any fresh operations).
10. Drop exhausted operations and emit `LoadBalancerBackendPoolUpdateFailed` once per distinct Service in the group.

Retries do not carry over the in-memory backend pool object. The next tick re-issues `Get` for the group before re-applying queued operations, which is required for `CreateOrUpdate` conflicts and stale etags.

The updater must re-acquire the lock only when prepending retry operations to the live queue. New EndpointSlice events can continue adding fresh operations while a previous snapshot is doing Azure calls.

New retry-path logging should use `pkg/log` contextual loggers. Do not add new direct `klog` calls while touching this file.

### Merge Rule

Fresh operations and requeued operations for the same `loadBalancerName/backendPoolName` merge on the next updater tick through the existing grouping logic. Each operation keeps its own `retryCount` and `nextEligibleAt`; fresh operations start at `retryCount=0` and no `nextEligibleAt`.

The group's eligibility is determined by the most restrictive member: if any operation in the merged group has a future `nextEligibleAt`, the whole group is preserved without ARM calls. When an eligible merged group fails, the ARM error applies to the group, not to an individual operation. Per-operation error attribution is not possible at this layer, so every constituent operation consumes one retry. Exhausted operations are dropped and reported, while operations with remaining budget are requeued.

The throttling delay is group-level. If one operation for `lbName/backendPoolName` is parked by `nextEligibleAt`, all operations in that same group are preserved until the group becomes eligible. This avoids splitting add/remove operations for the same backend pool and preserves batching order. Unrelated groups continue processing normally.

Retried operations are prepended to `updater.operations` so they appear before fresh operations added during processing. This preserves chronological diff ordering: old transitions are applied first, then newer ones, producing the correct final state. Without this, stale retried operations appended after fresh operations can overwrite the desired state.

Before requeueing or emitting retry/failure events, re-check that each operation is still relevant. If the Service is gone or now maps to a different load balancer, drop the stale operation quietly.

## Error Classification

Classify updater errors into three categories:

- `stale`: drop quietly
- `retriable`: emit retrying event and requeue while budget remains
- `terminal`: emit failed event and drop

Rules:

- ARM resource-not-found errors as detected by `errutils.CheckResourceExistsFromAzcoreError` are `stale`.
- `429 TooManyRequests` is `retriable`. The error reaches the updater through two paths: ARM 429 responses intercepted by `ThrottlingPolicy`, or local gate blocks when the throttle timer is still active. Both paths return `ThrottleError` with `RetryAfter` (see ThrottleError below). If `RetryAfter` is in the future, set `nextEligibleAt` on the requeued operation. Otherwise, fall back to the next updater tick.
- ARM `409 Conflict` and `412 PreconditionFailed` responses are `retriable`; no special payload handling is needed because the next updater tick already runs through a fresh `Get` before the next `CreateOrUpdate`.
- ARM response statuses in `retryrepectthrottled.GetRetriableStatusCode()` are `terminal` for this updater. The Azure SDK already retries those statuses inside the individual ARM call, so if the updater sees one of those errors, the SDK retry budget has already been exhausted. The updater emits `LoadBalancerBackendPoolUpdateFailed` and drops the operation instead of adding a second retry layer for the same condition.
- Non-ARM errors can opt into retry through a small wrapper/helper in `pkg/provider/azure_local_services.go`. The helper should use `errors.As` and `errors.Is` so wrapped errors remain classifiable.
- `context.Canceled` and `context.DeadlineExceeded` are `terminal` when `ctx.Err()` is non-nil.
- Other timeout-like non-ARM errors are `terminal` unless explicitly wrapped by the local retriable-error helper.
- All other errors are `terminal`.

### ThrottleError

The `ThrottlingPolicy` (a per-retry policy in the SDK pipeline) intercepts 429 responses, reads the `Retry-After` header, sets an internal gate timer, and returns `ErrTooManyRequest`. The SDK pipeline executes per-retry policies inside the retry loop. When ARM returns 429, `ThrottlingPolicy` sets the gate and returns the error. The SDK retry loop makes up to 4 attempts (1 initial + `MaxRetries` 3). If the gate timer has not expired, subsequent retries are blocked by the local gate without reaching ARM. After all attempts are exhausted, the error passes through the generated client to the updater.

Today `ErrTooManyRequest` is a plain sentinel (`errors.New(...)`) with no metadata. The updater cannot extract the `Retry-After` duration from it.

Without enrichment, a long `Retry-After` can exhaust the updater's retry budget against the local gate without ever making a real ARM attempt after the throttle window.

Enrich the existing sentinel with a `ThrottleError` type in `pkg/azclient/policy/retryrepectthrottled/throttle.go`:

```go
type ThrottleError struct {
    RetryAfter time.Time
}
func (e *ThrottleError) Error() string { return ErrTooManyRequest.Error() }
func (e *ThrottleError) Unwrap() error { return ErrTooManyRequest }
```

Both `ThrottlingPolicy` code paths return `&ThrottleError{...}` instead of bare `ErrTooManyRequest`:

- ARM 429 path: `RetryAfter` from the parsed `Retry-After` header (integer seconds or RFC1123 time, already computed as absolute time in `processThrottlePolicy`).
- Local gate path: `RetryAfter` = the existing gate timer value directly.

Existing callers using `errors.Is(err, ErrTooManyRequest)` continue to work unchanged via `Unwrap`.

The updater extracts the retry-after time:

```go
var throttleErr *retryrepectthrottled.ThrottleError
if errors.As(err, &throttleErr) && throttleErr.RetryAfter.After(time.Now()) {
    op.nextEligibleAt = throttleErr.RetryAfter
}
```

Add unit tests in `pkg/azclient/policy/retryrepectthrottled/throttle_test.go`:

1. ARM 429 with integer seconds `Retry-After` returns `ThrottleError` with matching `RetryAfter`.
2. ARM 429 with RFC1123 `Retry-After` returns `ThrottleError` with matching `RetryAfter`.
3. ARM 429 without `Retry-After` header returns `ThrottleError` with `RetryAfter` set to the current time (not in the future).
4. ARM 429 with unparsable `Retry-After` returns `ThrottleError` with `RetryAfter` unchanged from the previous timer value.
5. Local gate path with active timer returns `ThrottleError` with `RetryAfter` in the future.
6. `errors.Is(err, ErrTooManyRequest)` returns true for `ThrottleError`.
7. `errors.As(err, &ThrottleError{})` extracts the correct `RetryAfter`.
8. `ThrottleError.Error()` returns the same string as `ErrTooManyRequest.Error()`.

## Event Behavior

Use distinct events for retry and final failure:

- `LoadBalancerBackendPoolUpdateRetrying`: emitted on every retryable failed attempt that will be requeued.
- `LoadBalancerBackendPoolUpdateFailed`: emitted when a terminal error occurs or the retry budget is exhausted. The event message should include the error and guidance: *"Backend pool update failed: \<error\>. To retrigger, cause an endpoint change for the Service (e.g., restart or scale a backing pod)."*
- `LoadBalancerBackendPoolUpdated`: keep existing success behavior.
- ARM resource-not-found: no event.

The new `LoadBalancerBackendPoolUpdateRetrying` reason should be added as a constant in `pkg/consts/`, consistent with the repository convention for shared constants. If the implementation touches the existing `LoadBalancerBackendPoolUpdateFailed` or `LoadBalancerBackendPoolUpdated` literals, move those reasons to `pkg/consts/` in the same narrow change.

Events should be emitted once per distinct Service in a backend-pool group, not once per raw add/remove operation. This avoids duplicate retry events when multiple operations for the same Service are batched together.

This intentionally fixes the current `notify()` behavior that reports only the first operation in a batch because of its `break` after the first loop iteration. Add a regression test so future changes preserve one event per distinct Service in the group.

When an operation is waiting for `nextEligibleAt`, the updater should not emit another retrying event on each skipped tick. The retrying event belongs to the failed attempt that scheduled the retry.

## Locking And Queue Safety

The `process()` method acquires locks in a fixed order that matches the main service reconciliation loop:

1. `serviceReconcileLock`: serializes with the main reconcile path. Held for the entire `process()` call so that `localServiceNameToServiceInfoMap` reads in `groupOperations()` are consistent and `removeOperation()` can cancel queued operations before they are drained.
2. `azureResourceLocker`: distributed lease lock, serializes with other components that update Azure load balancer resources. If the lease cannot be acquired, `process()` returns without draining. Operations remain in the queue for the next tick.
3. `updater.lock`: protects `updater.operations`.
   - lock to snapshot and clear `updater.operations` via `drainOperations()`
   - unlock while doing filtering, grouping, and Azure calls
   - lock to prepend retry operations back to `updater.operations`
   - keep `addOperation` and `removeOperation` protected by the same lock

This lock ordering (`serviceReconcileLock` before `updater.lock`) prevents deadlocks: the main reconcile path holds `serviceReconcileLock` and calls `removeOperation()` (which acquires `updater.lock`).

`addOperation` and `removeOperation` only acquire `updater.lock`. `removeOperation()` is called from the main reconcile path under `serviceReconcileLock`. `addOperation()` is called from both the main reconcile path (under `serviceReconcileLock`) and the EndpointSlice informer (without `serviceReconcileLock`). This prevents slow Azure calls from blocking EndpointSlice event handlers that need to enqueue newer operations.

The `removeOperation(serviceName)` method cannot remove operations already in a processing snapshot. To avoid stale retry behavior, the processing path must re-check service relevance before requeueing and before sending retry/failure events.

Parked operations waiting for `nextEligibleAt` live in `updater.operations`, so `removeOperation(serviceName)` can remove them while they are parked. If removal races with the short snapshot-to-requeue window, the next tick's relevance check drops the stale operation quietly.

The relevance re-check depends on the Service cleanup path also removing the Service from `localServiceNameToServiceInfoMap`. The implementation must preserve that invariant: a deleted Service should either be removed from the live queue by `removeOperation(serviceName)` or be rejected by the map-based relevance check before a retry is requeued or reported.

### Queue Growth And Stale Operations

The current operation model is diff-based: each EndpointSlice event enqueues add/remove operations for the IPs that changed. This has two limitations when combined with retry:

**Queue growth**: during a long `Retry-After` period, EndpointSlice events continue to arrive and `addOperation()` appends new operations to the queue. The queue grows with each EndpointSlice event.

**Stale state after retry exhaustion**: the informer computes diffs between old and new EndpointSlice states. If a prior write failed and its operations were dropped, the changes from that write are lost. Subsequent diffs only capture newer EndpointSlice changes and do not replicate the lost operations. This is the same behavior as today's no-retry model. The main reconciliation loop corrects the drift on the next Service reconciliation triggered by `EnsureLoadBalancer`.

A stronger model would store the full desired IP set per `(service, loadBalancer, pool)` instead of individual add/remove diffs. `addOperation()` would replace the previous entry for the same `(service, loadBalancer, pool)`, bounding the queue to one entry per `(service, loadBalancer, pool)`. At processing time, `process()` would compute the diff from a fresh `Get` against the desired state, producing the correct result regardless of prior failures. The EndpointSlice informer path would need to compute the desired IP set from all EndpointSlices for the service, not just the one that changed. This can be added as a follow-up.

## Cancellation And Shutdown

If the updater context is canceled, the `run()` loop exits and parked in-memory operations are discarded with the process. The updater should not emit retry or failure events for operations that are only abandoned because the controller is shutting down.

If an in-flight ARM call returns `context.Canceled` or `context.DeadlineExceeded` after the updater context is done, treat it as terminal shutdown behavior and do not requeue. If `process()` observes cancellation before re-appending a snapshot, it should drop the snapshot instead of preserving retry state for a loop that is stopping.

## Metrics

The existing updater metric (`ObserveOperationWithResult`) should describe terminal outcomes, not intermediate retry attempts.

- Requeued attempts do not call `ObserveOperationWithResult(false)`.
- Queue-preservation ticks while waiting for `nextEligibleAt` do not record a metric.
- Success records one successful observation when `LoadBalancerBackendPoolUpdated` is emitted.
- Terminal failure records one failed observation when `LoadBalancerBackendPoolUpdateFailed` is emitted.
- Stale resource-not-found and stale Service/LB drops record no observation. Note: this changes current behavior where `ObserveOperationWithResult(false)` is called before `processError()` checks for 404, recording a failure metric for stale backend pools. The new behavior treats 404 as stale (no metric).

This prevents a retried-then-succeeded operation from appearing as multiple failed operations followed by one success.

## Retry Timing

Updater-level retry uses the existing `LoadBalancerBackendPoolUpdateIntervalInSeconds` tick; this design does not add a separate sleep loop inside `process()`. Without `Retry-After`, default retry count `3` and default interval `30s` means a continuously failing retriable condition emits retrying events on the first three failed attempts and emits the final failed event on the fourth failed attempt, roughly 90 seconds after the first observed failure plus ARM call latency. Depending on where the first failure lands relative to the updater tick and how long each ARM call takes, the wall-clock time from the original EndpointSlice change to final failure can be close to or above two minutes.

For 429 throttling, `Retry-After` overrides the next normal updater tick by setting `nextEligibleAt`. Ticks before `nextEligibleAt` only preserve the operation in the queue after re-checking Service/LB relevance; they do not call ARM, emit retrying events, or consume retry budget. `LoadBalancerBackendPoolUpdateMaxRetries` bounds failed processing attempts, not elapsed wall-clock time, so a long `Retry-After` can delay final success or failure beyond the normal interval-based timing.

During sustained Azure throttling, operators can reduce `LoadBalancerBackendPoolUpdateMaxRetries` or increase `LoadBalancerBackendPoolUpdateIntervalInSeconds` to avoid retry amplification when `Retry-After` is unavailable.

Exponential backoff is not used because the retriable errors at this layer either use server-directed delays (429 with `Retry-After`) or benefit from a fresh `Get` rather than longer waits (409/412). When all retries are exhausted without success, the operator receives a `LoadBalancerBackendPoolUpdateFailed` event with guidance to retrigger (see the Event Behavior section).

## Tests

Add focused unit tests in `pkg/provider/azure_local_services_test.go`:

1. `ThrottleError` with `RetryAfter` in the future from `Get` sets `nextEligibleAt` and emits `LoadBalancerBackendPoolUpdateRetrying`; subsequent ticks before `nextEligibleAt` requeue quietly without ARM calls, retry events, retry-count increments, or metrics.
2. `ThrottleError` with `RetryAfter` in the future from `CreateOrUpdate` follows the same parking and requeue behavior.
3. `ThrottleError` from the ARM 429 path with `RetryAfter` not in the future (no parseable `Retry-After`) requeues, emits `LoadBalancerBackendPoolUpdateRetrying`, then succeeds on the next eligible tick.
4. `ThrottleError` from the local gate path (throttle timer still active, no ARM call made) with `RetryAfter` in the future sets `nextEligibleAt` and parks the operation.
5. `ThrottleError` from the local gate path with `RetryAfter` not in the future (gate expired during SDK retries) falls back to the next updater tick.
6. ARM `409` or `412` from `CreateOrUpdate` requeues, emits `LoadBalancerBackendPoolUpdateRetrying`, then succeeds on the next tick after a fresh `Get`.
7. If any operation in a `lbName/backendPoolName` group is waiting for `nextEligibleAt`, the whole group is preserved and no same-group operation is processed early.
8. A fresh operation merged with a requeued operation keeps its own retry counter; on group failure, all operations in the group consume one retry.
9. A group with mixed retry budgets (one operation at max retries, one fresh): the exhausted operation is dropped and reported while the fresh operation is requeued.
10. Retry budget exhaustion emits `LoadBalancerBackendPoolUpdateFailed` with guidance text and leaves the queue empty.
11. Statuses from `retryrepectthrottled.GetRetriableStatusCode()` do not requeue and emit `LoadBalancerBackendPoolUpdateFailed`.
12. Other non-retriable errors do not requeue and emit `LoadBalancerBackendPoolUpdateFailed`.
13. ARM resource-not-found does not requeue and emits no event.
14. Stale service or changed load balancer before requeue is dropped quietly.
15. Retry requeue is serialized with LB migration under `serviceReconcileLock`: a retriable failure requeues the operation before the main reconcile path can call `removeOperation()`. After the lock is released, `removeOperation()` removes the requeued operation from the queue.
16. Retry requeue is serialized with service deletion under `serviceReconcileLock`: a retriable failure requeues the operation before the main reconcile path can delete from `localServiceNameToServiceInfoMap`. The requeued operation is dropped by `groupOperations()` on the next tick.
17. A Service that disappears while its operation is parked behind `nextEligibleAt` is dropped quietly on the next relevance check.
18. Multiple operations for multiple Services in a backend-pool group emit one retry/failure event per distinct Service, covering the current `notify()` break-after-first-operation bug.
19. `removeOperation(serviceName)` removes parked operations whose `nextEligibleAt` is in the future.
20. Updater shutdown with parked operations does not emit retry/failure events and does not requeue.
21. Explicit `LoadBalancerBackendPoolUpdateMaxRetries: 0` remains non-nil through config load and disables updater retry.
22. A retried-then-succeeded operation records exactly one successful metric observation and no failed metric observations.

Use a fake event recorder where needed to assert retrying and failed event reasons precisely.

## Expected Behavior

With default retry count `3`, a retriable updater failure behaves as:

1. First failed attempt: emit `LoadBalancerBackendPoolUpdateRetrying`, requeue.
2. Second failed attempt: emit `LoadBalancerBackendPoolUpdateRetrying`, requeue.
3. Third failed attempt: emit `LoadBalancerBackendPoolUpdateRetrying`, requeue.
4. Fourth failed attempt: emit `LoadBalancerBackendPoolUpdateFailed`, drop.

If a later retry succeeds, the updater emits `LoadBalancerBackendPoolUpdated` and drops the completed operations.

If the Service becomes stale during retry, the updater drops the operation quietly instead of emitting retry or failure events.

If an ARM call returns a status from `retryrepectthrottled.GetRetriableStatusCode()` after the SDK retry policy has already run, the updater emits `LoadBalancerBackendPoolUpdateFailed` immediately and drops the operation.

If a 429 response provides `Retry-After`, the updater respects that value before the next processing attempt. Intermediate updater ticks before `nextEligibleAt` are queue-preservation ticks, not retry attempts.
