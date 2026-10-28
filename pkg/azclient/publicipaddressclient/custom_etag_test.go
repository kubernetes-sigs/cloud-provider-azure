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

package publicipaddressclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

type etagTransport func(*http.Request) (*http.Response, error)

func (f etagTransport) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCreateOrUpdateETag(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusPreconditionFailed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const staleETag = `W/"11111111-1111-1111-1111-111111111111"`
			calls := 0
			options := utils.GetDefaultOption()
			options.Retry = policy.RetryOptions{MaxRetries: -1}
			options.Transport = etagTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, http.MethodPut, req.Method)
				require.Equal(t, staleETag, req.Header.Get("If-Match"))
				var sent armnetwork.PublicIPAddress
				require.NoError(t, json.NewDecoder(req.Body).Decode(&sent))
				require.Equal(t, to.Ptr(staleETag), sent.Etag)
				require.NotNil(t, sent.Properties)
				require.Equal(t, to.Ptr(int32(5)), sent.Properties.IdleTimeoutInMinutes)
				body := `{"name":"pip","etag":"current","properties":{"provisioningState":"Succeeded","idleTimeoutInMinutes":5}}`
				if status == http.StatusPreconditionFailed {
					body = `{"error":{"code":"PreconditionFailed","message":"stale ETag"}}`
				}
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    req,
				}, nil
			})
			client, err := New("subscription", &azfake.TokenCredential{}, options)
			require.NoError(t, err)
			result, err := client.CreateOrUpdate(context.Background(), "rg", "pip", armnetwork.PublicIPAddress{
				Location: to.Ptr("eastus"),
				Etag:     to.Ptr(staleETag),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IdleTimeoutInMinutes: to.Ptr(int32(5)),
				},
			})
			require.Equal(t, 1, calls)
			if status == http.StatusPreconditionFailed {
				require.Nil(t, result)
				var responseError *azcore.ResponseError
				require.True(t, errors.As(err, &responseError))
				require.Equal(t, status, responseError.StatusCode)
				require.Equal(t, "PreconditionFailed", responseError.ErrorCode)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, to.Ptr("current"), result.Etag)
			}
		})
	}
}
