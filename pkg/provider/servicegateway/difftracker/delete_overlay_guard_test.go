/*
Copyright 2024 The Kubernetes Authors.

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
	"fmt"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/stretchr/testify/assert"
)

// TestIsServiceOverlayMappingsError verifies detection of the NRP ServiceGateway
// "service still has overlay address mappings" rejection, whether the code arrives as a
// typed azcore ErrorCode or only in the raw response-body text.
func TestIsServiceOverlayMappingsError(t *testing.T) {
	assert.False(t, isServiceOverlayMappingsError(nil), "nil error is not the overlay rejection")
	assert.False(t, isServiceOverlayMappingsError(fmt.Errorf("some unrelated error")), "unrelated error is not the overlay rejection")
	assert.False(t, isServiceOverlayMappingsError(&azcore.ResponseError{StatusCode: http.StatusNotFound}), "404 is not the overlay rejection")

	// Typed ErrorCode form.
	assert.True(t, isServiceOverlayMappingsError(&azcore.ResponseError{
		StatusCode: http.StatusBadRequest,
		ErrorCode:  serviceOverlayMappingsErrorCode,
	}), "typed ErrorCode must be detected")

	// Body-carried form (azcore surfaces the body in Error()), possibly wrapped.
	wrapped := fmt.Errorf("failed to unregister from ServiceGateway: %w",
		fmt.Errorf("RESPONSE 400: 400 Bad Request\nERROR CODE: %s", serviceOverlayMappingsErrorCode))
	assert.True(t, isServiceOverlayMappingsError(wrapped), "body-carried code in a wrapped error must be detected")
}

// TestOnServiceCreationComplete_OverlayMappingsErrorReDrainsLocations verifies that when an
// inbound service deletion fails because NRP still has its overlay address mappings, the
// engine re-gates the deletion behind a fresh locations drain (instead of storming the
// unregister): it moves the op back to StateDeletionPending, re-adds it to
// pendingServiceDeletions, triggers the LocationsUpdater, and sets a retry backoff.
func TestOnServiceCreationComplete_OverlayMappingsErrorReDrainsLocations(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-overlay"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, nil),
		State:      StateDeletionInProgress,
		RetryCount: 0,
	}

	overlayErr := &azcore.ResponseError{
		StatusCode: http.StatusBadRequest,
		ErrorCode:  serviceOverlayMappingsErrorCode,
	}

	dt.OnServiceCreationComplete(uid, false, overlayErr)

	dt.mu.Lock()
	op := dt.pendingServiceOps[uid]
	_, pending := dt.pendingServiceDeletions[uid]
	retry := op.RetryCount
	state := op.State
	nextRetryAt := op.NextRetryAt
	dt.mu.Unlock()

	assert.Equal(t, StateDeletionPending, state, "overlay-mappings delete failure must re-gate the deletion behind a locations drain")
	assert.True(t, pending, "service must be re-added to pendingServiceDeletions for the drain gating")
	assert.Equal(t, 1, retry, "retry count should advance")
	assert.False(t, nextRetryAt.IsZero(), "overlay re-drain must set a backoff (NextRetryAt) so a persistent rejection cannot storm NRP")

	// The LocationsUpdater must have been triggered to drain the orphaned NRP addresses.
	select {
	case <-dt.locationsUpdaterTrigger:
		// expected
	default:
		t.Fatal("expected LocationsUpdater to be triggered to re-drain orphaned overlay addresses")
	}
}

// TestOnServiceCreationComplete_NonOverlayDeleteErrorRetriesNormally verifies that a
// generic (non-overlay) deletion failure keeps the existing direct-retry behaviour and
// does NOT re-gate behind a locations drain.
func TestOnServiceCreationComplete_NonOverlayDeleteErrorRetriesNormally(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-generic"

	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid,
		Config:     NewInboundServiceConfig(uid, nil),
		State:      StateDeletionInProgress,
		RetryCount: 0,
	}

	dt.OnServiceCreationComplete(uid, false, fmt.Errorf("transient ServiceGateway error"))

	dt.mu.Lock()
	op := dt.pendingServiceOps[uid]
	_, pending := dt.pendingServiceDeletions[uid]
	state := op.State
	dt.mu.Unlock()

	assert.Equal(t, StateDeletionInProgress, state, "generic delete failure must stay in DeletionInProgress for a direct retry")
	assert.False(t, pending, "generic delete failure must not re-gate behind locations")

	// The ServiceUpdater (not the LocationsUpdater) must be triggered for a direct retry.
	select {
	case <-dt.serviceUpdaterTrigger:
		// expected
	default:
		t.Fatal("expected ServiceUpdater to be triggered for a direct deletion retry")
	}
}
