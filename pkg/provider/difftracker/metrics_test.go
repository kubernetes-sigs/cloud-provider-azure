package difftracker

import (
	"testing"

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
