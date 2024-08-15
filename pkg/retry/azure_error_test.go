/*
Copyright 2020 The Kubernetes Authors.

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

package retry

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// LBInUseRawError is the LoadBalancerInUseByVirtualMachineScaleSet raw error
const LBInUseRawError = `{
	"error": {
    	"code": "LoadBalancerInUseByVirtualMachineScaleSet",
    	"message": "Cannot delete load balancer /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb since its child resources lb are in use by virtual machine scale set /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss.",
    	"details": []
  	}
}`

func TestNewError(t *testing.T) {
	rawErr := fmt.Errorf("HTTP status code (404)")
	newErr := NewError(true, rawErr)
	assert.Equal(t, true, newErr.Retriable)
	assert.Equal(t, rawErr, newErr.RawError)
}

func TestGetRetriableError(t *testing.T) {
	rawErr := fmt.Errorf("HTTP status code (404)")
	newErr := GetRetriableError(rawErr)
	assert.Equal(t, true, newErr.Retriable)
	assert.Equal(t, rawErr, newErr.RawError)
}

func TestGetRateLimitError(t *testing.T) {
	opType := "write"
	opName := "opNameTest"
	rawErr := fmt.Errorf("azure cloud provider rate limited(%s) for operation %q", opType, opName)
	newErr := GetRateLimitError(true, opName)
	assert.Equal(t, true, newErr.Retriable)
	assert.Equal(t, rawErr, newErr.RawError)
}

func TestGetThrottlingError(t *testing.T) {
	operation := "operationtest"
	reason := "reasontest"
	rawErr := fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", operation, reason)
	onehourlater := time.Now().Add(time.Hour * 1)
	newErr := GetThrottlingError(operation, reason, onehourlater)
	assert.Equal(t, true, newErr.Retriable)
	assert.Equal(t, rawErr, newErr.RawError)
	assert.Equal(t, onehourlater, newErr.RetryAfter)
}

func TestGetError(t *testing.T) {
	now = func() time.Time {
		return time.Time{}
	}

	tests := []struct {
		code       int
		retryAfter int
		err        error
		expected   *Error
	}{
		{
			code:     http.StatusOK,
			expected: nil,
		},
		{
			code: http.StatusOK,
			err:  fmt.Errorf("unknown error"),
			expected: &Error{
				Retriable:      true,
				HTTPStatusCode: http.StatusOK,
				RawError:       fmt.Errorf("unknown error"),
			},
		},
		{
			code: http.StatusBadRequest,
			expected: &Error{
				Retriable:      false,
				HTTPStatusCode: http.StatusBadRequest,
				RawError:       fmt.Errorf("some error"),
			},
		},
		{
			code: http.StatusInternalServerError,
			expected: &Error{
				Retriable:      true,
				HTTPStatusCode: http.StatusInternalServerError,
				RawError:       fmt.Errorf("some error"),
			},
		},
		{
			code: http.StatusSeeOther,
			err:  fmt.Errorf("some error"),
			expected: &Error{
				Retriable:      false,
				HTTPStatusCode: http.StatusSeeOther,
				RawError:       fmt.Errorf("some error"),
			},
		},
		{
			code:       http.StatusTooManyRequests,
			retryAfter: 100,
			expected: &Error{
				Retriable:      false,
				HTTPStatusCode: http.StatusTooManyRequests,
				RetryAfter:     now().Add(100 * time.Second),
				RawError:       fmt.Errorf("some error"),
			},
		},
	}

	for _, test := range tests {
		resp := &http.Response{
			StatusCode: test.code,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader([]byte("some error"))),
		}
		if test.retryAfter != 0 {
			resp.Header.Add("Retry-After", fmt.Sprintf("%d", test.retryAfter))
		}
		rerr := GetError(resp, test.err)
		assert.Equal(t, test.expected, rerr)
	}
}

func TestGetErrorNil(t *testing.T) {
	rerr := GetError(nil, nil)
	assert.Nil(t, rerr)

	// null body
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       nil,
	}
	rerr = GetError(resp, nil)
	assert.Equal(t, fmt.Errorf("empty HTTP response"), rerr.RawError)
}

func TestGetStatusNotFoundAndForbiddenIgnoredError(t *testing.T) {
	now = func() time.Time {
		return time.Time{}
	}

	tests := []struct {
		code       int
		retryAfter int
		err        error
		expected   *Error
	}{
		{
			code:     http.StatusOK,
			expected: nil,
		},
		{
			code:     http.StatusNotFound,
			expected: nil,
		},
		{
			code:     http.StatusForbidden,
			expected: nil,
		},
		{
			code: http.StatusOK,
			err:  fmt.Errorf("some error"),
			expected: &Error{
				Retriable:      true,
				HTTPStatusCode: http.StatusOK,
				RawError:       fmt.Errorf("some error"),
			},
		},
		{
			code: http.StatusBadRequest,
			expected: &Error{
				Retriable:      false,
				HTTPStatusCode: http.StatusBadRequest,
				RawError:       fmt.Errorf("some error"),
			},
		},
		{
			code: http.StatusInternalServerError,
			expected: &Error{
				Retriable:      true,
				HTTPStatusCode: http.StatusInternalServerError,
				RawError:       fmt.Errorf("some error"),
			},
		},
		{
			code: http.StatusSeeOther,
			err:  fmt.Errorf("some error"),
			expected: &Error{
				Retriable:      false,
				HTTPStatusCode: http.StatusSeeOther,
				RawError:       fmt.Errorf("some error"),
			},
		},
		{
			code:       http.StatusTooManyRequests,
			retryAfter: 100,
			expected: &Error{
				Retriable:      false,
				HTTPStatusCode: http.StatusTooManyRequests,
				RetryAfter:     now().Add(100 * time.Second),
				RawError:       fmt.Errorf("some error"),
			},
		},
	}

	for _, test := range tests {
		resp := &http.Response{
			StatusCode: test.code,
			Header:     http.Header{},
			Body:       io.NopCloser(bytes.NewReader([]byte("some error"))),
		}
		if test.retryAfter != 0 {
			resp.Header.Add("Retry-After", fmt.Sprintf("%d", test.retryAfter))
		}
		rerr := GetStatusNotFoundAndForbiddenIgnoredError(resp, test.err)
		assert.Equal(t, test.expected, rerr)
	}
}

func TestShouldRetryHTTPRequest(t *testing.T) {
	tests := []struct {
		code     int
		err      error
		expected bool
	}{
		{
			code:     http.StatusBadRequest,
			expected: false,
		},
		{
			code:     http.StatusInternalServerError,
			expected: true,
		},
		{
			code:     http.StatusOK,
			err:      fmt.Errorf("some error"),
			expected: true,
		},
		{
			code:     http.StatusOK,
			expected: false,
		},
		{
			code:     399,
			expected: false,
		},
	}
	for _, test := range tests {
		resp := &http.Response{
			StatusCode: test.code,
		}
		res := shouldRetryHTTPRequest(resp, test.err)
		if res != test.expected {
			t.Errorf("expected: %v, saw: %v", test.expected, res)
		}
	}
}

func TestIsSuccessResponse(t *testing.T) {
	tests := []struct {
		code     int
		expected bool
	}{
		{
			code:     http.StatusNotFound,
			expected: false,
		},
		{
			code:     http.StatusInternalServerError,
			expected: false,
		},
		{
			code:     http.StatusOK,
			expected: true,
		},
	}

	for _, test := range tests {
		resp := http.Response{
			StatusCode: test.code,
		}
		res := IsSuccessHTTPResponse(&resp)
		if res != test.expected {
			t.Errorf("expected: %v, saw: %v", test.expected, res)
		}
	}
}

func TestIsSuccessResponseNil(t *testing.T) {
	res := IsSuccessHTTPResponse(nil)
	assert.Equal(t, false, res)
}

func TestIsThrottled(t *testing.T) {
	tests := []struct {
		err      *Error
		expected bool
	}{
		{
			err:      nil,
			expected: false,
		},
		{
			err: &Error{
				HTTPStatusCode: http.StatusOK,
			},
			expected: false,
		},
		{
			err: &Error{
				HTTPStatusCode: http.StatusTooManyRequests,
			},
			expected: true,
		},
		{
			err: &Error{
				RetryAfter: time.Now().Add(time.Hour),
			},
			expected: true,
		},
		{
			err: &Error{
				RetryAfter: time.Now().Add(0),
			},
			expected: true,
		},
	}

	for _, test := range tests {
		realValue := test.err.IsThrottled()
		assert.Equal(t, test.expected, realValue)
	}
}

func TestIsErrorRetriable(t *testing.T) {
	// false case
	result := IsErrorRetriable(nil)
	assert.Equal(t, false, result)

	// true case
	result = IsErrorRetriable(fmt.Errorf("Retriable: true"))
	assert.Equal(t, true, result)
}

func TestHasErrorCode(t *testing.T) {
	// false case
	result := HasStatusForbiddenOrIgnoredError(fmt.Errorf("HTTPStatusCode: 408"))
	assert.False(t, result)

	// true case 404
	result = HasStatusForbiddenOrIgnoredError(fmt.Errorf("HTTPStatusCode: %d", http.StatusNotFound))
	assert.True(t, result)

	// true case 403
	result = HasStatusForbiddenOrIgnoredError(fmt.Errorf("HTTPStatusCode: %d", http.StatusForbidden))
	assert.True(t, result)
}

func TestGetVMSSNameByRawError(t *testing.T) {
	rgName, vmssName, err := GetVMSSMetadataByRawError(&Error{RawError: errors.New(LBInUseRawError)})
	assert.NoError(t, err)
	assert.Equal(t, "rg", rgName)
	assert.Equal(t, "vmss", vmssName)
}

func TestServiceServiceErrorMessage(t *testing.T) {
	now = func() time.Time {
		return time.Time{}
	}

	tests := []struct {
		err      *Error
		expected string
	}{
		{
			err:      nil,
			expected: "",
		},
		{
			err: &Error{
				Retriable:      true,
				HTTPStatusCode: http.StatusOK,
				RawError:       nil,
			},
			expected: "",
		},
		{
			err: &Error{
				RawError: fmt.Errorf("%s", "{\"error\":{\"message\": \"\"}}"),
			},
			expected: "",
		},
		{
			err: &Error{
				RawError: fmt.Errorf("%s", "{\"error\":{\"message\": \"Some error message\"}}"),
			},
			expected: "Some error message",
		},
	}

	for _, test := range tests {
		assert.Equal(t, test.expected, test.err.ServiceErrorMessage())
	}
}

func TestServiceErrorCode(t *testing.T) {
	now = func() time.Time {
		return time.Time{}
	}

	tests := []struct {
		err      *Error
		expected string
	}{
		{
			err:      nil,
			expected: "",
		},
		{
			err: &Error{
				Retriable:      true,
				HTTPStatusCode: http.StatusOK,
				RawError:       nil,
			},
			expected: "",
		},
		{
			err: &Error{
				RawError: fmt.Errorf("%s", "{\"error\":{\"code\": \"\",\"message\": \"Some error message\"}}"),
			},
			expected: "",
		},
		{
			err: &Error{
				RawError: fmt.Errorf("%s", "{\"error\":{\"code\": \"ReadOnlyDisabledSubscription\",\"message\": \"Some error message\"}}"),
			},
			expected: "ReadOnlyDisabledSubscription",
		},
		{
			err: &Error{
				RawError: fmt.Errorf("%s", "{\"error\":{\"code\": \"OperationNotAllowed\",\"message\": \"Another operation is in progress\"}}"),
			},
			expected: "OperationNotAllowed",
		},
		{
			err: &Error{
				RawError: fmt.Errorf("%s", "{\"error\":{\"code\": \"OperationNotAllowed\",\"message\": \"Submit a request for Quota increase at\"}}"),
			},
			expected: "QuotaExceeded",
		},
	}

	for _, test := range tests {
		assert.Equal(t, test.expected, test.err.ServiceErrorCode())
	}
}
