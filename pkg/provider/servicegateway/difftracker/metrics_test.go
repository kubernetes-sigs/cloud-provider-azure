/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package difftracker

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/stretchr/testify/assert"
	"k8s.io/component-base/metrics/testutil"
)

// TestResourceStateToString tests the resourceStateToString function
func TestResourceStateToString(t *testing.T) {
	tests := []struct {
		name     string
		state    ResourceState
		expected string
	}{
		{"StateNotStarted", StateNotStarted, "not_started"},
		{"StateCreationInProgress", StateCreationInProgress, "creation_in_progress"},
		{"StateCreated", StateCreated, "created"},
		{"StateDeletionPending", StateDeletionPending, "deletion_pending"},
		{"StateDeletionInProgress", StateDeletionInProgress, "deletion_in_progress"},
		{"Unknown state", ResourceState(999), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resourceStateToString(tt.state)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestExtractAzureErrorInfo tests the extractAzureErrorInfo function
func TestExtractAzureErrorInfo(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		expectedStatus int
		expectedCode   string
	}{
		{
			name:           "nil error",
			err:            nil,
			expectedStatus: 0,
			expectedCode:   "",
		},
		{
			name: "quota exceeded",
			err: &azcore.ResponseError{
				StatusCode: http.StatusForbidden,
				ErrorCode:  "QuotaExceeded",
			},
			expectedStatus: http.StatusForbidden,
			expectedCode:   "quota_exceeded",
		},
		{
			name: "throttled 429",
			err: &azcore.ResponseError{
				StatusCode: http.StatusTooManyRequests,
				ErrorCode:  "TooManyRequests",
			},
			expectedStatus: http.StatusTooManyRequests,
			expectedCode:   "throttled",
		},
		{
			name: "conflict 409",
			err: &azcore.ResponseError{
				StatusCode: http.StatusConflict,
				ErrorCode:  "Conflict",
			},
			expectedStatus: http.StatusConflict,
			expectedCode:   "conflict",
		},
		{
			name: "not found 404",
			err: &azcore.ResponseError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "ResourceNotFound",
			},
			expectedStatus: http.StatusNotFound,
			expectedCode:   "not_found",
		},
		{
			name: "internal error 500",
			err: &azcore.ResponseError{
				StatusCode: http.StatusInternalServerError,
				ErrorCode:  "InternalServerError",
			},
			expectedStatus: http.StatusInternalServerError,
			expectedCode:   "internal_error",
		},
		{
			name: "internal error 503",
			err: &azcore.ResponseError{
				StatusCode: http.StatusServiceUnavailable,
				ErrorCode:  "ServiceUnavailable",
			},
			expectedStatus: http.StatusServiceUnavailable,
			expectedCode:   "internal_error",
		},
		{
			name: "unknown Azure error",
			err: &azcore.ResponseError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  "SomeUnknownError",
			},
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "unknown",
		},
		{
			name:           "context deadline exceeded",
			err:            fmt.Errorf("operation failed: %w", errors.New("context deadline exceeded")),
			expectedStatus: 0,
			expectedCode:   "timeout",
		},
		{
			name:           "wrapped Azure error",
			err:            fmt.Errorf("failed to create LB: %w", &azcore.ResponseError{StatusCode: 429, ErrorCode: "TooManyRequests"}),
			expectedStatus: 429,
			expectedCode:   "throttled",
		},
		{
			name:           "generic error",
			err:            errors.New("something went wrong"),
			expectedStatus: 0,
			expectedCode:   "unknown",
		},
		{
			name: "PublicIPCountLimitReached as quota_exceeded",
			err: &azcore.ResponseError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  "PublicIPCountLimitReached",
			},
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "quota_exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, code := extractAzureErrorInfo(tt.err)
			assert.Equal(t, tt.expectedStatus, status, "HTTP status mismatch")
			assert.Equal(t, tt.expectedCode, code, "error code mismatch")
		})
	}
}

// TestServiceConfigValidate tests ServiceConfig.Validate()
func TestServiceConfigValidate(t *testing.T) {
	tests := []struct {
		name        string
		config      ServiceConfig
		shouldError bool
		errorMsg    string
	}{
		{
			name: "valid inbound config",
			config: ServiceConfig{
				UID:       "test-uid",
				IsInbound: true,
			},
			shouldError: false,
		},
		{
			name: "valid outbound config",
			config: ServiceConfig{
				UID:       "test-uid",
				IsInbound: false,
			},
			shouldError: false,
		},
		{
			name: "empty UID",
			config: ServiceConfig{
				UID:       "",
				IsInbound: true,
			},
			shouldError: true,
			errorMsg:    "service UID cannot be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.shouldError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestUpdatePendingOperationOldestAgeMetric_IncludesUpdateInProgress verifies that the oldest
// pending-operation age gauge is emitted for the update_in_progress state, so a stuck update is
// observable to its alert.
func TestUpdatePendingOperationOldestAgeMetric_IncludesUpdateInProgress(t *testing.T) {
	RegisterMetrics()
	dt := newTestDiffTracker()
	dt.pendingServiceOps["svc"] = &ServiceOperationState{
		ServiceUID: "svc",
		Config:     NewInboundServiceConfig("svc", nil),
		State:      StateUpdateInProgress,
		CreatedAt:  time.Now().Add(-time.Hour),
	}

	updatePendingOperationOldestAgeMetric(dt)

	v, err := testutil.GetGaugeMetricValue(pendingOperationOldestAgeSeconds.WithLabelValues("update_in_progress", "inbound"))
	assert.NoError(t, err)
	assert.Greater(t, v, 3000.0, "the update_in_progress oldest-age series must be emitted")
}

// TestOnServiceCreationComplete_PreEmptRecordsCompletedInFlightOperation verifies the preempt branch:
// when a delete arrived while a create/update was in flight, OnServiceCreationComplete routes the
// completed in-flight operation to the deletion flow and records that create/update exactly once
// (the subsequent delete records its own metric when it completes).
func TestOnServiceCreationComplete_PreEmptRecordsCompletedInFlightOperation(t *testing.T) {
	RegisterMetrics()
	serviceOperationTotal.Reset()

	dt := newTestDiffTracker()
	uid := "svc-preempt-metric"
	// Preempt fixture: a concurrent DeleteService changed the state while a create was in flight
	// (InFlightConfig != nil). StateDeletionPending is one of the preempt triggers.
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	inflight := cfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:     uid,
		Config:         cfg,
		InFlightConfig: &inflight,
		State:          StateDeletionPending,
	}
	dt.pendingServiceDeletions[uid] = &PendingServiceDeletion{ServiceUID: uid, IsInbound: true}

	// The in-flight create completes successfully. The preempt branch fires.
	dt.OnServiceCreationComplete(uid, true, nil)

	// The preempt branch took the path (InFlightConfig is cleared and state moved off pending).
	op := dt.pendingServiceOps[uid]
	if assert.NotNil(t, op, "op must remain tracked through the preempt") {
		assert.Nil(t, op.InFlightConfig, "preempt must clear InFlightConfig")
	}

	// The completed in-flight create is recorded exactly once. LastAppliedConfig was nil, so the
	// op is a CREATE; err was nil, so the success series is incremented.
	got, err := testutil.GetCounterMetricValue(
		serviceOperationTotal.WithLabelValues("create", "inbound", "success", "", "false"),
	)
	assert.NoError(t, err)
	assert.Equal(t, 1.0, got,
		"the preempt path records the completed in-flight create once")

	// The error series stays 0: a successful completion is not double-counted as an error.
	gotErr, err := testutil.GetCounterMetricValue(
		serviceOperationTotal.WithLabelValues("create", "inbound", "error", "", "false"),
	)
	assert.NoError(t, err)
	assert.Equal(t, 0.0, gotErr,
		"a successful preempt completion does not touch the error series")
}

func TestOnServiceCreationComplete_PreEmptRecordsInFlightUpdate(t *testing.T) {
	RegisterMetrics()
	serviceOperationTotal.Reset()

	dt := newTestDiffTracker()
	uid := "svc-preempt-update-metric"
	// A service that was already applied once (LastAppliedConfig != nil) had an UPDATE in flight
	// (InFlightConfig != nil) when a Delete preempted it. The completed in-flight op is an UPDATE.
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	applied := cfg
	inflight := cfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:        uid,
		Config:            cfg,
		LastAppliedConfig: &applied,
		InFlightConfig:    &inflight,
		State:             StateDeletionPending,
	}
	dt.pendingServiceDeletions[uid] = &PendingServiceDeletion{ServiceUID: uid, IsInbound: true}

	// The in-flight update completes successfully; the preempt branch fires.
	dt.OnServiceCreationComplete(uid, true, nil)

	got, err := testutil.GetCounterMetricValue(
		serviceOperationTotal.WithLabelValues("update", "inbound", "success", "", "false"),
	)
	assert.NoError(t, err)
	assert.Equal(t, 1.0, got,
		"the preempt path records the completed in-flight update once, on the update series")

	// The create series is untouched: LastAppliedConfig != nil means the op is classified as update.
	gotCreate, err := testutil.GetCounterMetricValue(
		serviceOperationTotal.WithLabelValues("create", "inbound", "success", "", "false"),
	)
	assert.NoError(t, err)
	assert.Equal(t, 0.0, gotCreate,
		"an already-applied service's preempted op is an update, not a create")
}

func TestOnServiceCreationComplete_OrphanDeleteSuccessCountsOrphanCleanup(t *testing.T) {
	RegisterMetrics()

	dt := newTestDiffTracker()
	uid := "svc-orphan-delete-metric"
	cfg := NewInboundServiceConfig(uid, makeInboundConfig(80))
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     cfg,
		State:      StateDeletionInProgress,
		IsOrphan:   true,
	}

	// orphanedResourcesCleanedTotal has no labels and cannot be Reset between tests, so measure a delta.
	before, err := testutil.GetCounterMetricValue(orphanedResourcesCleanedTotal)
	assert.NoError(t, err)

	// The orphan's asynchronous deletion completes successfully.
	dt.OnServiceCreationComplete(uid, true, nil)

	after, err := testutil.GetCounterMetricValue(orphanedResourcesCleanedTotal)
	assert.NoError(t, err)
	assert.Equal(t, 1.0, after-before,
		"a successful orphan deletion counts exactly one orphaned_resources_cleaned_total")
}

// TestRecordServiceParked_CountsByReason pins that a parked service operation is observable.
//
// A parked operation has stopped making progress: EnsureLoadBalancer has already returned nil, so
// the service controller reports success and the Service stays pending indefinitely. Without this
// counter the condition is visible only in logs, so an operator has no way to alert on it.
func TestRecordServiceParked_CountsByReason(t *testing.T) {
	RegisterMetrics()

	for _, reason := range []string{parkReasonTerminalError, parkReasonRetriesExceeded} {
		before, err := testutil.GetCounterMetricValue(serviceOperationsParkedTotal.WithLabelValues(reason))
		assert.NoError(t, err)

		recordServiceParked(reason)

		after, err := testutil.GetCounterMetricValue(serviceOperationsParkedTotal.WithLabelValues(reason))
		assert.NoError(t, err)
		assert.Equal(t, float64(1), after-before, "parking with reason %q must be counted exactly once", reason)
	}
}

// TestServiceOperationRetries_ObservedOncePerOperation pins the meaning of
// service_operation_retries: one observation per operation, taken when the operation reaches a
// terminal outcome, carrying the number of retries it needed. Observing on every failed attempt
// instead would make the sum a running total of attempt numbers and would drop first-attempt
// successes from the distribution entirely.
func TestServiceOperationRetries_ObservedOncePerOperation(t *testing.T) {
	RegisterMetrics()
	serviceOperationRetries.Reset()

	dt := newTestDiffTracker()
	cfg := NewInboundServiceConfig("svc-retry-metric", makeInboundConfig(80))
	dt.pendingServiceOps["svc-retry-metric"] = &ServiceOperationState{
		ServiceUID: "svc-retry-metric",
		Config:     cfg,
		State:      StateCreationInProgress,
	}

	// Two retryable failures, then a success on the third attempt.
	transient := errors.New("transient azure failure")
	for i := 0; i < 2; i++ {
		dt.OnServiceCreationComplete("svc-retry-metric", false, transient)
		dt.mu.Lock()
		dt.pendingServiceOps["svc-retry-metric"].State = StateCreationInProgress
		dt.mu.Unlock()
	}
	dt.OnServiceCreationComplete("svc-retry-metric", true, nil)

	observer := serviceOperationRetries.WithLabelValues("create", "inbound")
	count, err := testutil.GetHistogramMetricCount(observer)
	assert.NoError(t, err)
	assert.Equal(t, uint64(1), count,
		"one completed operation must contribute exactly one observation, not one per failed attempt")

	sum, err := testutil.GetHistogramMetricValue(observer)
	assert.NoError(t, err)
	assert.Equal(t, 2.0, sum,
		"the observation must be the retries the operation needed (2), not the sum of attempt numbers")

	// A first-attempt success must still be represented, as an explicit zero.
	dt.pendingServiceOps["svc-retry-first-try"] = &ServiceOperationState{
		ServiceUID: "svc-retry-first-try",
		Config:     NewInboundServiceConfig("svc-retry-first-try", makeInboundConfig(80)),
		State:      StateCreationInProgress,
	}
	dt.OnServiceCreationComplete("svc-retry-first-try", true, nil)

	count, err = testutil.GetHistogramMetricCount(observer)
	assert.NoError(t, err)
	assert.Equal(t, uint64(2), count,
		"an operation that succeeds without retrying must be observed too, otherwise the distribution is conditioned on failure")

	sum, err = testutil.GetHistogramMetricValue(observer)
	assert.NoError(t, err)
	assert.Equal(t, 2.0, sum, "a first-attempt success contributes 0 retries")
}

// TestOnServiceCreationComplete_RefreshesPendingDeletionsGauge pins that the completion callback
// refreshes the pending-deletions gauge. It mutates pendingServiceDeletions on several paths, so a
// gauge refreshed only by DeleteService keeps reporting the count from the last delete request.
func TestOnServiceCreationComplete_RefreshesPendingDeletionsGauge(t *testing.T) {
	RegisterMetrics()

	dt := newTestDiffTracker()
	uid := "svc-gauge-delete"
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State:      StateDeletionInProgress,
	}
	dt.pendingServiceDeletions[uid] = &PendingServiceDeletion{ServiceUID: uid, IsInbound: true}
	dt.pendingServiceDeletions["other"] = &PendingServiceDeletion{ServiceUID: "other", IsInbound: true}

	// Prime the gauge to the pre-completion count, as DeleteService would have left it.
	updatePendingServiceDeletionsMetric(dt)
	primed, err := testutil.GetGaugeMetricValue(pendingServiceDeletions)
	assert.NoError(t, err)
	assert.Equal(t, 2.0, primed)

	// The deletion completes and its pendingServiceDeletions entry is removed.
	dt.OnServiceCreationComplete(uid, true, nil)

	dt.mu.Lock()
	remaining := len(dt.pendingServiceDeletions)
	dt.mu.Unlock()
	assert.Equal(t, 1, remaining, "the completed deletion must be removed from the pending map")

	got, err := testutil.GetGaugeMetricValue(pendingServiceDeletions)
	assert.NoError(t, err)
	assert.Equal(t, 1.0, got,
		"the gauge must track the map after a completion, not stay at the value the last DeleteService set")
}

// TestOnServiceCreationComplete_OutboundUpdateRecordsNoAzureWrite pins that an outbound update
// completion does not report a successful Azure operation. The updater cannot apply an outbound
// update, so it completes the operation without calling Azure; recording the operation counter and
// duration histogram there would report a write that never happened.
func TestOnServiceCreationComplete_OutboundUpdateRecordsNoAzureWrite(t *testing.T) {
	RegisterMetrics()
	serviceOperationTotal.Reset()
	serviceOperationDuration.Reset()

	dt := newTestDiffTracker()
	uid := "svc-outbound-update-metric"
	cfg := NewOutboundServiceConfig(uid, &OutboundConfig{})
	inflight := cfg
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID:     uid,
		Config:         cfg,
		InFlightConfig: &inflight,
		State:          StateUpdateInProgress,
	}

	dt.OnServiceCreationComplete(uid, true, nil)

	dt.mu.Lock()
	state := dt.pendingServiceOps[uid].State
	dt.mu.Unlock()
	assert.Equal(t, StateCreated, state, "the update completion must still advance the state machine")

	got, err := testutil.GetCounterMetricValue(
		serviceOperationTotal.WithLabelValues("update", "outbound", "success", "", "false"),
	)
	assert.NoError(t, err)
	assert.Equal(t, 0.0, got, "an outbound update performs no Azure write and must not be counted as one")

	durCount, err := testutil.GetHistogramMetricCount(
		serviceOperationDuration.WithLabelValues("update", "outbound"),
	)
	assert.NoError(t, err)
	assert.Equal(t, uint64(0), durCount, "no Azure call was made, so there is no duration to observe")
}
