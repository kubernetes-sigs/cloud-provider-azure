---
title: "Refining Local Service Backend Pool Updater"
linkTitle: "Refining Local Service Backend Pool Updater"
type: docs
description: >
    Retry and error handling design for the local service backend pool updater.
---

# Refining Local Service Backend Pool Updater

## Purpose

The local service backend pool updater batches EndpointSlice-driven IP changes for local Services and applies them asynchronously to Azure Load Balancer backend pools. Today, when an updater batch observes an error from `Get` or `CreateOrUpdate`, it drops the batch after emitting a failure event, except for backend pool `404`, which is treated as stale work and skipped quietly.

This design adds bounded retry for errors that are worth retrying at the updater layer. It does not duplicate Azure SDK retries for the status codes already handled by the SDK retry policy. The retry behavior is local to the updater, reuses the existing updater tick loop, and avoids unbounded requeue.

## Scope

In scope:

- Retry failed local-service backend-pool update operations when the error is classified as retriable.
- Add retry state to queued backend-pool update operations.
- Add a new retry-count configuration field for this updater.
- Emit retry and failure events with bounded event volume.
- Preserve current stale `404` behavior.
- Add unit tests for retry, exhaustion, non-retry, and stale cases.

Out of scope:

- Redesigning multiple standard load balancer selection.
- Changing main Service reconciliation retries.
- Changing Azure SDK retry policy.
- Changing cleanup behavior for local service backend pools.

## Configuration

Add a new config field near `LoadBalancerBackendPoolUpdateIntervalInSeconds`:

```go
// LoadBalancerBackendPoolUpdateRetryCount is the number of retries for retriable
// local-service backend-pool update failures. Defaults to 3.
LoadBalancerBackendPoolUpdateRetryCount *int `json:"loadBalancerBackendPoolUpdateRetryCount,omitempty" yaml:"loadBalancerBackendPoolUpdateRetryCount,omitempty"`
```

Add a default constant near `DefaultLoadBalancerBackendPoolUpdateIntervalInSeconds`:

```go
DefaultLoadBalancerBackendPoolUpdateRetryCount = 3
```

Semantics:

- `nil` uses the default value `3`.
- Explicit `0` disables updater-level retry and preserves today's drop-after-failure behavior for retriable errors.
- Negative values are normalized to `0`.
- The count means extra retries after the first failed attempt. With the default `3`, a retriable error can be observed up to four times: one initial attempt plus three requeued retry attempts.

Use a pointer so the config loader can distinguish "unset" from "explicitly set to 0". Normalization happens next to the existing `LoadBalancerBackendPoolUpdateIntervalInSeconds` defaulting in `pkg/provider/azure.go`: if the pointer is nil, use `DefaultLoadBalancerBackendPoolUpdateRetryCount`; if the pointer is non-nil and negative, clamp it to `0`.

The `omitempty` tag is intentional with the pointer field: a nil pointer is omitted and therefore defaults, while a non-nil pointer to `0` is preserved across marshal/unmarshal and keeps retry disabled.

## Retry Model

Use requeue-on-retriable-error. Each `loadBalancerBackendPoolUpdateOperation` carries retry metadata:

- `retryCount int`, tracking how many retryable failures have already been consumed
- `nextEligibleAt time.Time`, tracking when a throttled operation is eligible to be processed again

On each updater tick:

1. Snapshot the queued operations under `updater.lock`.
2. Clear the live queue while holding the lock.
3. Release the lock before filtering, grouping, or calling Azure.
4. Filter operations using the existing relevance checks:
   - the Service is still in `localServiceNameToServiceInfoMap`
   - the Service still points to the same load balancer
5. Group otherwise relevant operations by `loadBalancerName/backendPoolName`.
6. If any operation in a group has `nextEligibleAt` in the future, requeue the whole group. Do not call ARM, emit an event, record a metric, or increment retry state for skipped groups.
7. Process each eligible group once.
8. On retriable failure of a group, increment each constituent operation's `retryCount` uniformly.
9. Requeue operations whose retry budget remains.
10. Drop exhausted operations and emit `LoadBalancerBackendPoolUpdateFailed` once per distinct Service in the group.

Retries do not carry over the in-memory backend pool object. The next tick re-issues `Get` for the group before re-applying queued operations, which is required for `CreateOrUpdate` conflicts and stale etags.

The updater must re-acquire the lock only when appending retry operations back to the live queue. New EndpointSlice events can continue adding fresh operations while a previous snapshot is doing Azure calls.

### Merge Rule

Fresh operations and requeued operations for the same `loadBalancerName/backendPoolName` merge on the next updater tick through the existing grouping logic. Each operation keeps its own `retryCount` and `nextEligibleAt`; fresh operations start at `retryCount=0` and no `nextEligibleAt`.

The group's eligibility is determined by the most restrictive member: if any operation in the merged group has a future `nextEligibleAt`, the whole group is preserved without ARM calls. When an eligible merged group fails, the ARM error applies to the group, not to an individual operation. Per-operation error attribution is not possible at this layer, so every constituent operation consumes one retry. Exhausted operations are dropped and reported, while operations with remaining budget are requeued.

The throttling delay is group-level. If one operation for `lbName/backendPoolName` is parked by `nextEligibleAt`, all operations in that same group are preserved until the group becomes eligible. This avoids splitting add/remove operations for the same backend pool and preserves batching order. Unrelated groups continue processing normally.

Before requeueing or emitting retry/failure events, re-check that each operation is still relevant. If the Service is gone or now maps to a different load balancer, drop the stale operation quietly.

## Error Classification

Classify updater errors into three categories:

- `stale`: drop quietly
- `retriable`: emit retrying event and requeue while budget remains
- `terminal`: emit failed event and drop

Rules:

- ARM resource-not-found errors as detected by `errutils.CheckResourceExistsFromAzcoreError` are `stale`.
- ARM wire `429 TooManyRequests` responses are `retriable`.
  - Classify them by checking `errors.As(err, &respErr)` for `*azcore.ResponseError` with `StatusCode == http.StatusTooManyRequests`.
  - Extract `Retry-After` from `respErr.RawResponse.Header` when `RawResponse` is non-nil.
  - Parse `Retry-After` the same way `retryrepectthrottled.ThrottlingPolicy.processThrottlePolicy` does: integer seconds or RFC1123 time.
  - If the response has no parseable `Retry-After`, fall back to the next updater tick.
- Local throttle-gate `retryrepectthrottled.ErrTooManyRequest` is `retriable`.
  - This path is detected with `errors.Is(err, retryrepectthrottled.ErrTooManyRequest)` when no `*azcore.ResponseError.RawResponse` is available.
  - The local throttle gate returns the sentinel error without a response, header, or exported gate timestamp, so the updater cannot compute the policy's gate expiry.
  - Fall back to the next updater tick for this path.
  - The updater does not read `ThrottlingPolicy.RetryAfterReader` or `RetryAfterWriter` directly. The policy instance is not exposed through the backend-pool client factory.
- ARM `409 Conflict` and `412 PreconditionFailed` responses are `retriable`; no special payload handling is needed because the next updater tick already runs through a fresh `Get` before the next `CreateOrUpdate`.
- ARM response statuses in `retryrepectthrottled.GetRetriableStatusCode()` are `terminal` for this updater. The Azure SDK already retries those statuses inside the individual ARM call, so if the updater sees one of those errors, the SDK retry budget has already been exhausted. The updater emits `LoadBalancerBackendPoolUpdateFailed` and drops the operation instead of adding a second retry layer for the same condition.
- Non-ARM errors can opt into retry through a small wrapper/helper, such as:

```go
func newRetriableBackendPoolUpdateError(err error) error
func isRetriableBackendPoolUpdateError(err error) bool
```

Keep this helper local to `pkg/provider/azure_local_services.go` unless another caller needs the same marker.

- `context.Canceled` and `context.DeadlineExceeded` are terminal when `ctx.Err()` is non-nil.
- Other timeout-like non-ARM errors are terminal unless explicitly wrapped by the local retriable-error helper.
- All other errors are terminal.

The helper should use `errors.As` and `errors.Is` so wrapped errors remain classifiable.

The call site does not need generated backend-pool client wrappers to return raw `*http.Response` values. It should inspect the returned error directly:

```go
var respErr *azcore.ResponseError
if errors.As(err, &respErr) && respErr.RawResponse != nil {
    retryAfter := respErr.RawResponse.Header.Get(retryrepectthrottled.HeaderRetryAfter)
    // parse retryAfter for 429 handling
}
```

## Event Behavior

Use distinct events for retry and final failure:

- `LoadBalancerBackendPoolUpdateRetrying`: emitted on every retryable failed attempt that will be requeued.
- `LoadBalancerBackendPoolUpdateFailed`: emitted when a terminal error occurs or the retry budget is exhausted.
- `LoadBalancerBackendPoolUpdated`: keep existing success behavior.
- ARM resource-not-found: no event.

The new `LoadBalancerBackendPoolUpdateRetrying` reason should be added as a constant in `pkg/consts/`, consistent with the repository convention for shared constants. If the implementation touches the existing `LoadBalancerBackendPoolUpdateFailed` or `LoadBalancerBackendPoolUpdated` literals, move those reasons to `pkg/consts/` in the same narrow change.

Events should be emitted once per distinct Service in a backend-pool group, not once per raw add/remove operation. This avoids duplicate retry events when multiple operations for the same Service are batched together.

This intentionally fixes the current `notify()` behavior that reports only the first operation in a batch because of its `break` after the first loop iteration. Add a regression test so future changes preserve one event per distinct Service in the group.

When an operation is waiting for `nextEligibleAt`, the updater should not emit another retrying event on each skipped tick. The retrying event belongs to the failed attempt that scheduled the retry.

## Locking And Queue Safety

Keep `updater.lock`, but narrow it to queue access:

- lock to snapshot and clear `updater.operations`
- unlock while doing filtering, grouping, and Azure calls
- lock to append retry operations
- keep `addOperation` and `removeOperation` protected by the same lock

This prevents slow Azure calls and retry paths from blocking EndpointSlice event handlers that need to enqueue newer operations.

The `removeOperation(serviceName)` method cannot remove operations already in a processing snapshot. To avoid stale retry behavior, the processing path must re-check service relevance before requeueing and before sending retry/failure events.

Parked operations waiting for `nextEligibleAt` live in `updater.operations`, so `removeOperation(serviceName)` can remove them while they are parked. If removal races with the short snapshot-to-requeue window, the next tick's relevance check drops the stale operation quietly.

The relevance re-check depends on the Service cleanup path also removing the Service from `localServiceNameToServiceInfoMap`. The implementation must preserve that invariant: a deleted Service should either be removed from the live queue by `removeOperation(serviceName)` or be rejected by the map-based relevance check before a retry is requeued or reported.

New retry-path logging should use `pkg/log` contextual loggers. Do not add new direct `klog` calls while touching this file.

## Cancellation And Shutdown

If the updater context is canceled, the `run()` loop exits and parked in-memory operations are discarded with the process. The updater should not emit retry or failure events for operations that are only abandoned because the controller is shutting down.

If an in-flight ARM call returns `context.Canceled` or `context.DeadlineExceeded` after the updater context is done, treat it as terminal shutdown behavior and do not requeue. If `process()` observes cancellation before re-appending a snapshot, it should drop the snapshot instead of preserving retry state for a loop that is stopping.

## Metrics

The updater metric should describe terminal outcomes, not intermediate retry attempts.

- Requeued attempts do not call `ObserveOperationWithResult(false)`.
- Queue-preservation ticks while waiting for `nextEligibleAt` do not record a metric.
- Success records one successful observation when `LoadBalancerBackendPoolUpdated` is emitted.
- Terminal failure records one failed observation when `LoadBalancerBackendPoolUpdateFailed` is emitted.
- Stale resource-not-found and stale Service/LB drops record no observation, matching the existing quiet-skip behavior.

This prevents a retried-then-succeeded operation from appearing as multiple failed operations followed by one success.

## Retry Timing

Updater-level retry uses the existing `LoadBalancerBackendPoolUpdateIntervalInSeconds` tick; this design does not add a separate sleep loop inside `process()`. Without `Retry-After`, default retry count `3` and default interval `30s` means a continuously failing retriable condition emits retrying events on the first three failed attempts and emits the final failed event on the fourth failed attempt, roughly 90 seconds after the first observed failure plus ARM call latency. Depending on where the first failure lands relative to the updater tick and how long each ARM call takes, the wall-clock time from the original EndpointSlice change to final failure can be close to or above two minutes.

For 429 throttling, `Retry-After` overrides the next normal updater tick by setting `nextEligibleAt`. Ticks before `nextEligibleAt` only preserve the operation in the queue after re-checking Service/LB relevance; they do not call ARM, emit retrying events, or consume retry budget. `LoadBalancerBackendPoolUpdateRetryCount` bounds failed processing attempts, not elapsed wall-clock time, so a long `Retry-After` can delay final success or failure beyond the normal interval-based timing.

During sustained Azure throttling, operators can reduce `LoadBalancerBackendPoolUpdateRetryCount` or increase `LoadBalancerBackendPoolUpdateIntervalInSeconds` to avoid retry amplification when `Retry-After` is unavailable.

## Tests

Add focused unit tests in `pkg/provider/azure_local_services_test.go`:

1. ARM wire `429` from `Get` with a parseable `Retry-After` in `azcore.ResponseError.RawResponse` sets `nextEligibleAt`; ticks before that time requeue quietly without ARM calls, retry events, retry-count increments, or metrics.
2. ARM wire `429` from `CreateOrUpdate` follows the same `Retry-After` and requeue behavior.
3. ARM wire `429` without a parseable `Retry-After` requeues, emits `LoadBalancerBackendPoolUpdateRetrying`, then succeeds on the next eligible tick.
4. Local-gate `retryrepectthrottled.ErrTooManyRequest` without a raw response falls back to the next updater tick and does not panic on nil response.
5. ARM `409` or `412` from `CreateOrUpdate` requeues, emits `LoadBalancerBackendPoolUpdateRetrying`, then succeeds on the next tick after a fresh `Get`.
6. If any operation in a `lbName/backendPoolName` group is waiting for `nextEligibleAt`, the whole group is preserved and no same-group operation is processed early.
7. A fresh operation merged with a requeued operation keeps its own retry counter; on group failure, all operations in the group consume one retry.
8. Retry budget exhaustion emits `LoadBalancerBackendPoolUpdateFailed` and leaves the queue empty.
9. Statuses from `retryrepectthrottled.GetRetriableStatusCode()` do not requeue and emit `LoadBalancerBackendPoolUpdateFailed`.
10. Other non-retriable errors do not requeue and emit `LoadBalancerBackendPoolUpdateFailed`.
11. ARM resource-not-found does not requeue and emits no event.
12. Stale service or changed load balancer before requeue is dropped quietly.
13. A Service that disappears while its operation is parked behind `nextEligibleAt` is dropped quietly on the next relevance check.
14. Multiple operations for multiple Services in a backend-pool group emit one retry/failure event per distinct Service, covering the current `notify()` break-after-first-operation bug.
15. `removeOperation(serviceName)` removes parked operations whose `nextEligibleAt` is in the future.
16. Updater shutdown with parked operations does not emit retry/failure events and does not requeue.
17. Explicit `LoadBalancerBackendPoolUpdateRetryCount: 0` remains non-nil through config load and disables updater retry.
18. A retried-then-succeeded operation records exactly one successful metric observation and no failed metric observations.

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
