package difftracker

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/stretchr/testify/assert"
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
