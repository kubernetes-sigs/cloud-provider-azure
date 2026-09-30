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

package servicegatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	azlog "github.com/Azure/azure-sdk-for-go/sdk/azcore/log"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTransport answers every request with one fixed response and records what was sent.
type stubTransport struct {
	status   int
	header   http.Header
	body     string
	requests []*http.Request
	bodies   []string
}

func (s *stubTransport) Do(req *http.Request) (*http.Response, error) {
	s.requests = append(s.requests, req)
	var sent []byte
	if req.Body != nil {
		var err error
		if sent, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
	}
	s.bodies = append(s.bodies, string(sent))

	header := http.Header{"Content-Type": []string{"application/json"}}
	for key, values := range s.header {
		header[key] = values
	}
	return &http.Response{
		StatusCode: s.status,
		Status:     http.StatusText(s.status),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Request:    req,
	}, nil
}

func newStubbedClient(t *testing.T, transport *stubTransport) Interface {
	t.Helper()
	client, err := New("00000000-0000-0000-0000-000000000000", &azfake.TokenCredential{}, &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Transport: transport,
			// A single attempt keeps every assertion about the wire exchange exact.
			Retry: policy.RetryOptions{MaxRetries: -1},
		},
	})
	require.NoError(t, err)
	return client
}

type updateOperation struct {
	action string
	call   func(context.Context, Interface) error
}

var updateOperations = map[string]updateOperation{
	"UpdateAddressLocations": {
		action: "updateAddressLocations",
		call: func(ctx context.Context, client Interface) error {
			return client.UpdateAddressLocations(ctx, "rg", "sgw", armnetwork.ServiceGatewayUpdateAddressLocationsRequest{
				Action: to.Ptr(armnetwork.UpdateActionPartialUpdate),
				AddressLocations: []*armnetwork.ServiceGatewayAddressLocation{{
					AddressLocation: to.Ptr("10.0.0.4"),
				}},
			})
		},
	},
	"UpdateServices": {
		action: "updateServices",
		call: func(ctx context.Context, client Interface) error {
			return client.UpdateServices(ctx, "rg", "sgw", armnetwork.ServiceGatewayUpdateServicesRequest{
				Action: to.Ptr(armnetwork.ServiceUpdateActionFullUpdate),
			})
		},
	},
}

// TestUpdateOperationsCompleteSynchronously pins the contract NRP implements and API version
// 2025-09-01 documents: updateAddressLocations and updateServices finish inline and answer 200 OK.
// A 200 is success without any polling unless it names an error code; every other status,
// including 202 and 204, is an error.
func TestUpdateOperationsCompleteSynchronously(t *testing.T) {
	responses := map[string]struct {
		status    int
		header    http.Header
		body      string
		success   bool
		errorCode string
	}{
		"200 with empty body":  {status: http.StatusOK, success: true},
		"200 with status body": {status: http.StatusOK, body: `{"status":"Succeeded"}`, success: true},
		// NRP stamps polling headers on its already-complete 200; they must not start a poller.
		"200 with polling headers": {
			status: http.StatusOK,
			header: http.Header{
				"Azure-Asyncoperation": []string{"https://management.azure.com/poll"},
				"Location":             []string{"https://management.azure.com/poll"},
			},
			success: true,
		},
		// A 200 that names an error did not apply the change and must not be reported as success.
		"200 with error code header": {
			status:    http.StatusOK,
			header:    http.Header{"X-Ms-Error-Code": []string{"OperationFailed"}},
			errorCode: "OperationFailed",
		},
		"200 with error body": {
			status:    http.StatusOK,
			body:      `{"error":{"code":"OperationFailed","message":"the update was not applied"}}`,
			errorCode: "OperationFailed",
		},
		"202 Accepted":   {status: http.StatusAccepted},
		"204 No Content": {status: http.StatusNoContent},
		"409 Conflict": {
			status:    http.StatusConflict,
			body:      `{"error":{"code":"AnotherOperationInProgress","message":"another operation is in progress"}}`,
			errorCode: "AnotherOperationInProgress",
		},
	}

	for operationName, operation := range updateOperations {
		for responseName, response := range responses {
			t.Run(operationName+"/"+responseName, func(t *testing.T) {
				transport := &stubTransport{status: response.status, header: response.header, body: response.body}

				err := operation.call(context.Background(), newStubbedClient(t, transport))

				require.Len(t, transport.requests, 1, "the update must be a single request, never polled")
				request := transport.requests[0]
				assert.Equal(t, http.MethodPost, request.Method)
				assert.True(t, strings.HasSuffix(request.URL.Path, "/providers/Microsoft.Network/serviceGateways/sgw/"+operation.action),
					"unexpected request path %q", request.URL.Path)
				assert.GreaterOrEqual(t, request.URL.Query().Get("api-version"), "2025-09-01",
					"the synchronous contract starts at API version 2025-09-01")

				if response.success {
					assert.NoError(t, err)
					return
				}
				var respErr *azcore.ResponseError
				require.True(t, errors.As(err, &respErr), "expected an *azcore.ResponseError, got %v", err)
				assert.Equal(t, response.status, respErr.StatusCode)
				assert.Equal(t, response.errorCode, respErr.ErrorCode)
			})
		}
	}
}

// TestUpdateOperationsSendTheRequestedChange guards the wrapper against dropping or rewriting the
// caller's request on the way to NRP.
func TestUpdateOperationsSendTheRequestedChange(t *testing.T) {
	expected := map[string]map[string]any{
		"UpdateAddressLocations": {
			"action":           "PartialUpdate",
			"addressLocations": []any{map[string]any{"addressLocation": "10.0.0.4"}},
		},
		"UpdateServices": {"action": "FullUpdate"},
	}

	for operationName, operation := range updateOperations {
		t.Run(operationName, func(t *testing.T) {
			transport := &stubTransport{status: http.StatusOK}

			require.NoError(t, operation.call(context.Background(), newStubbedClient(t, transport)))

			require.Len(t, transport.bodies, 1)
			var sent map[string]any
			require.NoError(t, json.Unmarshal([]byte(transport.bodies[0]), &sent))
			assert.Equal(t, expected[operationName], sent)
		})
	}
}

// TestSuccessfulUpdateDoesNotLogAResponseError keeps a clean 200 out of the SDK's response-error
// log; only a 200 that names an error code is reported as one.
func TestSuccessfulUpdateDoesNotLogAResponseError(t *testing.T) {
	var logged []string
	azlog.SetEvents(azlog.EventResponseError)
	azlog.SetListener(func(_ azlog.Event, message string) { logged = append(logged, message) })
	t.Cleanup(func() {
		azlog.SetListener(nil)
		azlog.SetEvents()
	})

	for operationName, operation := range updateOperations {
		t.Run(operationName, func(t *testing.T) {
			logged = nil
			clean := newStubbedClient(t, &stubTransport{status: http.StatusOK})
			require.NoError(t, operation.call(context.Background(), clean))
			assert.Empty(t, logged, "a successful update must not log a response error")

			failed := newStubbedClient(t, &stubTransport{
				status: http.StatusOK,
				header: http.Header{"X-Ms-Error-Code": []string{"OperationFailed"}},
			})
			require.Error(t, operation.call(context.Background(), failed))
			assert.Len(t, logged, 1, "an error-bearing 200 is logged once as a response error")
		})
	}
}
