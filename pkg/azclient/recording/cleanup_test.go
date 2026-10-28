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

package recording

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/privatedns/armprivatedns"
	"github.com/stretchr/testify/require"
)

type cleanupTransport func(*http.Request) (*http.Response, error)

func (f cleanupTransport) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDeletePrivateDNSZone(t *testing.T) {
	tests := []struct {
		name       string
		conflicts  int
		status     int
		errorCode  string
		timeout    time.Duration
		wantCalls  int
		wantCancel bool
	}{
		{name: "immediate success", status: http.StatusNoContent, wantCalls: 1},
		{name: "deleted link still visible", conflicts: 2, status: http.StatusNoContent, wantCalls: 3},
		{name: "forbidden", status: http.StatusForbidden, errorCode: "AuthorizationFailed", wantCalls: 1},
		{name: "wrong status", status: http.StatusForbidden, errorCode: "CannotDeleteResource", wantCalls: 1},
		{name: "unrelated conflict", status: http.StatusConflict, errorCode: "AnotherOperationInProgress", wantCalls: 1},
		{name: "caller deadline", conflicts: 1, timeout: 20 * time.Millisecond, wantCalls: 1, wantCancel: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			transport := cleanupTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, http.MethodDelete, req.Method)
				require.True(t, strings.HasSuffix(req.URL.Path, "/privateDnsZones/test.local"))
				status, code := tt.status, tt.errorCode
				if calls <= tt.conflicts {
					status, code = http.StatusConflict, "CannotDeleteResource"
				}
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"code":%q,"message":"test response"}}`, code))),
					Request:    req,
				}, nil
			})
			client, err := armprivatedns.NewPrivateZonesClient("subscription", &azfake.TokenCredential{}, &arm.ClientOptions{
				ClientOptions: policy.ClientOptions{
					Transport: transport,
					Retry:     policy.RetryOptions{MaxRetries: -1},
				},
			})
			require.NoError(t, err)
			ctx := context.Background()
			if tt.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.timeout)
				defer cancel()
			}
			err = DeletePrivateDNSZone(ctx, client, "rg", "test.local")
			require.Equal(t, tt.wantCalls, calls)
			switch {
			case tt.wantCancel:
				require.ErrorIs(t, err, context.DeadlineExceeded)
				var responseError *azcore.ResponseError
				require.True(t, errors.As(err, &responseError))
				require.Equal(t, "CannotDeleteResource", responseError.ErrorCode)
			case tt.errorCode != "":
				var responseError *azcore.ResponseError
				require.True(t, errors.As(err, &responseError))
				require.Equal(t, tt.status, responseError.StatusCode)
				require.Equal(t, tt.errorCode, responseError.ErrorCode)
			default:
				require.NoError(t, err)
			}
		})
	}
}

func TestDeletePrivateDNSZonePollerError(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		errorCode string
		wantCalls int
	}{
		{name: "fatal polling error", status: http.StatusForbidden, errorCode: "AuthorizationFailed", wantCalls: 2},
		{name: "retry nested resource conflict", status: http.StatusConflict, errorCode: "CannotDeleteResource", wantCalls: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			client, err := armprivatedns.NewPrivateZonesClient("subscription", &azfake.TokenCredential{}, &arm.ClientOptions{
				ClientOptions: policy.ClientOptions{
					Retry: policy.RetryOptions{MaxRetries: -1},
					Transport: cleanupTransport(func(req *http.Request) (*http.Response, error) {
						calls++
						switch calls {
						case 1:
							require.Equal(t, http.MethodDelete, req.Method)
							return &http.Response{
								StatusCode: http.StatusAccepted,
								Header:     http.Header{"Location": []string{"https://management.azure.com/operations/delete"}},
								Body:       http.NoBody,
								Request:    req,
							}, nil
						case 2:
							require.Equal(t, http.MethodGet, req.Method)
							return &http.Response{
								StatusCode: tt.status,
								Header:     http.Header{"Content-Type": []string{"application/json"}},
								Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"code":%q,"message":"test response"}}`, tt.errorCode))),
								Request:    req,
							}, nil
						}
						require.Equal(t, http.MethodDelete, req.Method)
						return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: req}, nil
					}),
				},
			})
			require.NoError(t, err)
			err = DeletePrivateDNSZone(context.Background(), client, "rg", "test.local")
			require.Equal(t, tt.wantCalls, calls)
			if tt.status == http.StatusConflict {
				require.NoError(t, err)
			} else {
				var responseError *azcore.ResponseError
				require.True(t, errors.As(err, &responseError))
				require.Equal(t, tt.status, responseError.StatusCode)
			}
		})
	}
}

func TestDeletePrivateDNSZoneCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	client, err := armprivatedns.NewPrivateZonesClient("subscription", &azfake.TokenCredential{}, &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{
			Retry: policy.RetryOptions{MaxRetries: -1},
			Transport: cleanupTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				cancel()
				return &http.Response{
					StatusCode: http.StatusConflict,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"CannotDeleteResource","message":"link remains"}}`)),
					Request:    req,
				}, nil
			}),
		},
	})
	require.NoError(t, err)
	defer cancel()
	err = DeletePrivateDNSZone(ctx, client, "rg", "test.local")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
}
