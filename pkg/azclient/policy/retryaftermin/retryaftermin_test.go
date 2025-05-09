/*
Copyright 2025 The Kubernetes Authors.

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

package retryaftermin_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/retryaftermin"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

// TestRetryAfterMinPolicy tests Policy functionality
func TestRetryAfterMinPolicy(t *testing.T) {
	t.Run("should remove retry-after headers below minimum threshold", func(t *testing.T) {
		minRetryAfter := 5 * time.Second
		retryPolicy := retryaftermin.NewRetryAfterMinPolicy(minRetryAfter)

		pipeline := runtime.NewPipeline("testmodule", "v0.1.0", runtime.PipelineOptions{}, &policy.ClientOptions{
			PerCallPolicies: []policy.Policy{
				retryPolicy,
				utils.FuncPolicyWrapper(
					func(_ *policy.Request) (*http.Response, error) {
						resp := &http.Response{
							StatusCode: http.StatusOK,
							Header:     make(http.Header),
							Body:       http.NoBody,
						}
						resp.Header.Set("Retry-After", "2")
						return resp, nil
					},
				),
			},
		})

		req, err := runtime.NewRequest(context.Background(), http.MethodGet, "https://management.microsoft.com")
		assert.NoError(t, err)

		response, err := pipeline.Do(req)
		assert.NoError(t, err)
		assert.Empty(t, response.Header.Get("Retry-After"))
	})

	t.Run("should keep retry-after headers above minimum threshold", func(t *testing.T) {
		minRetryAfter := 5 * time.Second
		retryPolicy := retryaftermin.NewRetryAfterMinPolicy(minRetryAfter)

		pipeline := runtime.NewPipeline("testmodule", "v0.1.0", runtime.PipelineOptions{}, &policy.ClientOptions{
			PerCallPolicies: []policy.Policy{
				retryPolicy,
				utils.FuncPolicyWrapper(
					func(_ *policy.Request) (*http.Response, error) {
						resp := &http.Response{
							StatusCode: http.StatusOK,
							Header:     make(http.Header),
							Body:       http.NoBody,
						}
						resp.Header.Set("Retry-After", "10")
						return resp, nil
					},
				),
			},
		})

		req, err := runtime.NewRequest(context.Background(), http.MethodGet, "https://management.microsoft.com")
		assert.NoError(t, err)

		response, err := pipeline.Do(req)
		assert.NoError(t, err)
		assert.Equal(t, "10", response.Header.Get("Retry-After"))
	})

	t.Run("should propagate errors", func(t *testing.T) {
		minRetryAfter := 5 * time.Second
		testErr := errors.New("test error")
		retryPolicy := retryaftermin.NewRetryAfterMinPolicy(minRetryAfter)

		pipeline := runtime.NewPipeline("testmodule", "v0.1.0", runtime.PipelineOptions{}, &policy.ClientOptions{
			PerCallPolicies: []policy.Policy{
				retryPolicy,
				utils.FuncPolicyWrapper(
					func(_ *policy.Request) (*http.Response, error) {
						return nil, testErr
					},
				),
			},
		})

		req, err := runtime.NewRequest(context.Background(), http.MethodGet, "https://management.microsoft.com")
		assert.NoError(t, err)

		_, err = pipeline.Do(req)
		assert.Error(t, err)
		assert.Equal(t, testErr, err)
	})

	t.Run("should handle duration format", func(t *testing.T) {
		minRetryAfter := 5 * time.Second
		retryPolicy := retryaftermin.NewRetryAfterMinPolicy(minRetryAfter)

		pipeline := runtime.NewPipeline("testmodule", "v0.1.0", runtime.PipelineOptions{}, &policy.ClientOptions{
			PerCallPolicies: []policy.Policy{
				retryPolicy,
				utils.FuncPolicyWrapper(
					func(_ *policy.Request) (*http.Response, error) {
						resp := &http.Response{
							StatusCode: http.StatusOK,
							Header:     make(http.Header),
							Body:       http.NoBody,
						}
						resp.Header.Set("Retry-After", "2s")
						return resp, nil
					},
				),
			},
		})

		req, err := runtime.NewRequest(context.Background(), http.MethodGet, "https://management.microsoft.com")
		assert.NoError(t, err)

		response, err := pipeline.Do(req)
		assert.NoError(t, err)
		assert.Empty(t, response.Header.Get("Retry-After"))
	})

	t.Run("should preserve unrecognized format", func(t *testing.T) {
		minRetryAfter := 5 * time.Second
		retryPolicy := retryaftermin.NewRetryAfterMinPolicy(minRetryAfter)

		pipeline := runtime.NewPipeline("testmodule", "v0.1.0", runtime.PipelineOptions{}, &policy.ClientOptions{
			PerCallPolicies: []policy.Policy{
				retryPolicy,
				utils.FuncPolicyWrapper(
					func(_ *policy.Request) (*http.Response, error) {
						resp := &http.Response{
							StatusCode: http.StatusOK,
							Header:     make(http.Header),
							Body:       http.NoBody,
						}
						resp.Header.Set("Retry-After", "invalid-format")
						return resp, nil
					},
				),
			},
		})

		req, err := runtime.NewRequest(context.Background(), http.MethodGet, "https://management.microsoft.com")
		assert.NoError(t, err)

		response, err := pipeline.Do(req)
		assert.NoError(t, err)
		assert.Equal(t, "invalid-format", response.Header.Get("Retry-After"))
	})
}

// TestNewRetryAfterMinPolicy tests the creation of retryaftermin.Policy
func TestNewRetryAfterMinPolicy(t *testing.T) {
	t.Parallel()

	minRetryAfter := 5 * time.Second
	policy := retryaftermin.NewRetryAfterMinPolicy(minRetryAfter)

	assert.NotNil(t, policy)

	// Type assertion to check the concrete type
	retryPolicy, ok := policy.(*retryaftermin.Policy)
	assert.True(t, ok)
	assert.Equal(t, minRetryAfter, retryPolicy.GetMinRetryAfter())
}
