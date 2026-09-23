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

package interfaceclient

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
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

type nicTestCredential struct{}

func (nicTestCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

type nicTestTransport func(*http.Request) (*http.Response, error)

func (transport nicTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func newNICClientForTest(t *testing.T, transport nicTestTransport) Interface {
	t.Helper()
	options := utils.GetDefaultOption()
	options.ClientOptions.Transport = &http.Client{Transport: transport}
	client, err := New("00000000-0000-0000-0000-000000000000", nicTestCredential{}, options)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func nicTestResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestCreateOrUpdateNICETag(t *testing.T) {
	t.Run("a delayed update cannot recreate a deleted NIC", func(t *testing.T) {
		const oldETag = `W/"old"`
		const nicBody = `{"name":"nic","location":"eastus","etag":"W/\"old\"","properties":{"provisioningState":"Succeeded"}}`
		exists := true
		gets, puts := 0, 0
		client := newNICClientForTest(t, func(request *http.Request) (*http.Response, error) {
			switch request.Method {
			case http.MethodGet:
				gets++
				if !exists {
					return nicTestResponse(request, http.StatusNotFound, `{"error":{"code":"NotFound"}}`), nil
				}
				return nicTestResponse(request, http.StatusOK, nicBody), nil
			case http.MethodPut:
				puts++
				if got := request.Header.Get("If-Match"); got != oldETag {
					t.Errorf("If-Match = %q, want %q", got, oldETag)
					exists = true
					return nicTestResponse(request, http.StatusOK, nicBody), nil
				}
				return nicTestResponse(request, http.StatusPreconditionFailed, `{"error":{"code":"PreconditionFailed","message":"NIC no longer exists"}}`), nil
			default:
				return nil, fmt.Errorf("unexpected request method %s", request.Method)
			}
		})

		nic, err := client.Get(context.Background(), "rg", "nic", nil)
		if err != nil || nic == nil {
			t.Fatalf("get NIC: %v", err)
		}
		if nic.Etag == nil || *nic.Etag != oldETag {
			t.Fatalf("retrieved NIC ETag = %v, want %q", nic.Etag, oldETag)
		}
		exists = false
		_, err = client.CreateOrUpdate(context.Background(), "rg", "nic", *nic)
		var responseError *azcore.ResponseError
		if !errors.As(err, &responseError) || responseError.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("delayed update error = %v, want HTTP 412", err)
		}
		if exists || gets != 1 || puts != 1 {
			t.Errorf("NIC state after delayed update: exists=%t GET=%d PUT=%d", exists, gets, puts)
		}
	})

	t.Run("new NIC creation remains unconditional", func(t *testing.T) {
		requests := 0
		client := newNICClientForTest(t, func(request *http.Request) (*http.Response, error) {
			requests++
			if request.Method != http.MethodPut {
				t.Errorf("request method = %s, want PUT", request.Method)
			}
			if got := request.Header.Get("If-Match"); got != "" {
				t.Errorf("unexpected If-Match header %q", got)
			}
			return nicTestResponse(request, http.StatusOK, `{"name":"nic","location":"eastus","etag":"new","properties":{"provisioningState":"Succeeded"}}`), nil
		})

		nic := armnetwork.Interface{Location: to.Ptr("eastus")}
		_, err := client.CreateOrUpdate(context.Background(), "rg", "nic", nic)
		if err != nil {
			t.Fatalf("create NIC: %v", err)
		}
		if requests != 1 {
			t.Errorf("request count = %d, want 1", requests)
		}
	})
}
