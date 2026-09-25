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
	"encoding/json"
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

type nicTestServer struct {
	resource *armnetwork.Interface
	gets     int
	puts     int
	ifMatch  string
}

func (server *nicTestServer) roundTrip(request *http.Request) (*http.Response, error) {
	status := http.StatusOK
	switch request.Method {
	case http.MethodGet:
		server.gets++
		if server.resource == nil {
			return nicTestResponse(request, http.StatusNotFound, `{"error":{"code":"NotFound"}}`), nil
		}
	case http.MethodPut:
		server.puts++
		server.ifMatch = request.Header.Get("If-Match")
		var resource armnetwork.Interface
		if err := json.NewDecoder(request.Body).Decode(&resource); err != nil {
			return nil, fmt.Errorf("decode NIC update: %w", err)
		}
		if server.ifMatch != "" && (server.resource == nil || server.ifMatch != *server.resource.Etag) {
			return nicTestResponse(request, http.StatusPreconditionFailed, `{"error":{"code":"PreconditionFailed"}}`), nil
		}
		if server.resource == nil {
			status = http.StatusCreated
		}
		resource.Etag = to.Ptr(fmt.Sprintf(`W/"updated-%d"`, server.puts))
		server.resource = &resource
	default:
		return nil, fmt.Errorf("unexpected request method %s", request.Method)
	}
	body, err := json.Marshal(server.resource)
	if err != nil {
		return nil, err
	}
	return nicTestResponse(request, status, string(body)), nil
}

func nicForETagTest(etag *string) *armnetwork.Interface {
	return &armnetwork.Interface{
		Name:     to.Ptr("nic"),
		Location: to.Ptr("eastus"),
		Etag:     etag,
		Properties: &armnetwork.InterfacePropertiesFormat{
			ProvisioningState: to.Ptr(armnetwork.ProvisioningStateSucceeded),
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name: to.Ptr("ipconfig"),
				Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					LoadBalancerBackendAddressPools: []*armnetwork.BackendAddressPool{
						{ID: to.Ptr("public-pool")},
						{ID: to.Ptr("internal-pool")},
					},
				},
			}},
		},
	}
}

func requireNICResponseError(t *testing.T, err error, status int, code string) {
	t.Helper()
	var responseError *azcore.ResponseError
	if !errors.As(err, &responseError) || responseError.StatusCode != status || responseError.ErrorCode != code {
		t.Fatalf("error = %v, want Azure HTTP %d (%s)", err, status, code)
	}
}

func TestCreateOrUpdateNICETag(t *testing.T) {
	const oldETag = `W/"old"`
	for _, test := range []struct {
		name       string
		change     string
		wantReject bool
	}{
		{name: "current ETag allows the membership update"},
		{name: "concurrent update is preserved and a fresh read recovers", change: "update", wantReject: true},
		{name: "a delayed update cannot recreate a deleted NIC", change: "delete", wantReject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			server := &nicTestServer{resource: nicForETagTest(to.Ptr(oldETag))}
			client := newNICClientForTest(t, server.roundTrip)
			nic, err := client.Get(ctx, "rg", "nic", nil)
			if err != nil || nic == nil {
				t.Fatalf("get NIC: %v", err)
			}
			if nic.Etag == nil || *nic.Etag != oldETag {
				t.Fatalf("retrieved NIC ETag = %v, want %q", nic.Etag, oldETag)
			}
			nic.Properties.IPConfigurations[0].Properties.LoadBalancerBackendAddressPools =
				nic.Properties.IPConfigurations[0].Properties.LoadBalancerBackendAddressPools[:1]

			switch test.change {
			case "update":
				server.resource.Etag = to.Ptr(`W/"concurrent"`)
				server.resource.Tags = map[string]*string{"concurrent": to.Ptr("preserved")}
			case "delete":
				server.resource = nil
			}
			updated, err := client.CreateOrUpdate(ctx, "rg", "nic", *nic)
			if server.ifMatch != oldETag {
				t.Errorf("If-Match = %q, want %q", server.ifMatch, oldETag)
			}
			if test.wantReject {
				requireNICResponseError(t, err, http.StatusPreconditionFailed, "PreconditionFailed")
				if updated != nil {
					t.Fatal("rejected update returned a NIC")
				}
			} else if err != nil || updated == nil {
				t.Fatalf("update with current ETag: %v", err)
			}
			if server.gets != 1 || server.puts != 1 {
				t.Fatalf("GET=%d PUT=%d, want one of each without a stale-write retry", server.gets, server.puts)
			}

			if test.change == "delete" {
				if server.resource != nil {
					t.Fatal("deleted NIC was recreated")
				}
				nic, err = client.Get(ctx, "rg", "nic", nil)
				requireNICResponseError(t, err, http.StatusNotFound, "NotFound")
				if nic != nil {
					t.Fatal("deleted NIC is still readable")
				}
				return
			}
			if test.change == "update" {
				if pools := server.resource.Properties.IPConfigurations[0].Properties.LoadBalancerBackendAddressPools; len(pools) != 2 {
					t.Fatal("rejected update changed backend memberships")
				}
				nic, err = client.Get(ctx, "rg", "nic", nil)
				if err != nil || nic == nil {
					t.Fatalf("refresh NIC: %v", err)
				}
				nic.Properties.IPConfigurations[0].Properties.LoadBalancerBackendAddressPools =
					nic.Properties.IPConfigurations[0].Properties.LoadBalancerBackendAddressPools[:1]
				updated, err = client.CreateOrUpdate(ctx, "rg", "nic", *nic)
				if err != nil || updated == nil {
					t.Fatalf("update with refreshed ETag: %v", err)
				}
				if server.ifMatch != `W/"concurrent"` || server.gets != 2 || server.puts != 2 {
					t.Fatalf("recovery did not use fresh state: If-Match=%q GET=%d PUT=%d", server.ifMatch, server.gets, server.puts)
				}
				if value := updated.Tags["concurrent"]; value == nil || *value != "preserved" {
					t.Fatal("recovery lost the concurrent update")
				}
			}
			pools := updated.Properties.IPConfigurations[0].Properties.LoadBalancerBackendAddressPools
			if len(pools) != 1 || pools[0].ID == nil || *pools[0].ID != "public-pool" {
				t.Fatalf("unexpected backend membership after update: %v", pools)
			}
		})
	}

	for _, test := range []struct {
		name string
		etag *string
	}{
		{name: "creation with nil ETag is unconditional"},
		{name: "creation with empty ETag is unconditional", etag: to.Ptr("")},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &nicTestServer{}
			client := newNICClientForTest(t, server.roundTrip)
			nic, err := client.CreateOrUpdate(context.Background(), "rg", "nic", *nicForETagTest(test.etag))
			if err != nil || nic == nil || server.resource == nil {
				t.Fatalf("create NIC: %v", err)
			}
			if server.ifMatch != "" || server.puts != 1 || server.gets != 0 {
				t.Errorf("unexpected create requests: If-Match=%q PUT=%d GET=%d", server.ifMatch, server.puts, server.gets)
			}
		})
	}
}
