/*
Copyright 2023 The Kubernetes Authors.

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

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/interfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/interfaceclient/mock_interfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

func TestUpdateInterfaceSendsIfMatchWildcardAndPropagatesErrors(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusPreconditionFailed, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			for _, etag := range []*string{nil, ptr.To(`W/"previous-version"`)} {
				t.Run(ptr.Deref(etag, "no-etag"), func(t *testing.T) {
					calls := 0
					options := utils.GetDefaultOption()
					options.Retry = policy.RetryOptions{MaxRetries: -1}
					options.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
						calls++
						assert.Equal(t, http.MethodPut, req.Method)
						assert.Equal(t, "*", req.Header.Get("If-Match"))
						assert.Equal(t, "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic", req.URL.Path)
						var sent armnetwork.Interface
						if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
							t.Fatal(err)
						}
						assert.Equal(t, etag, sent.Etag)
						assert.Equal(t, nicBackendPoolProperties(), sent.Properties)
						body := `{"name":"nic","properties":{"provisioningState":"Succeeded"}}`
						switch status {
						case http.StatusPreconditionFailed:
							body = `{"error":{"code":"PreconditionFailed","message":"NIC no longer exists","details":[{"code":"PreconditionFailedResourceDoesNotExist"}]}}`
						case http.StatusInternalServerError:
							body = `{"error":{"code":"InternalServerError","message":"NIC update failed"}}`
						}
						return &http.Response{
							StatusCode: status,
							Header:     http.Header{"Content-Type": []string{"application/json"}},
							Body:       io.NopCloser(strings.NewReader(body)),
							Request:    req,
						}, nil
					})
					nicClient, err := interfaceclient.New("subscription", staticTokenCredential("test"), options)
					if err != nil {
						t.Fatal(err)
					}
					factory := mock_azclient.NewMockClientFactory(gomock.NewController(t))
					factory.EXPECT().GetInterfaceClient().Return(nicClient)
					az := &Cloud{ComputeClientFactory: factory}
					az.ResourceGroup = "rg"
					nic := &armnetwork.Interface{
						Name:       ptr.To("nic"),
						Etag:       etag,
						Properties: nicBackendPoolProperties(),
					}
					result, err := az.updateInterface(context.Background(), nic)
					assert.Equal(t, 1, calls)
					assert.Equal(t, etag, nic.Etag)
					if status == http.StatusOK {
						assert.NoError(t, err)
						assert.NotNil(t, result)
					} else {
						assert.Nil(t, result)
						var responseError *azcore.ResponseError
						if assert.True(t, errors.As(err, &responseError)) {
							assert.Equal(t, status, responseError.StatusCode)
						}
					}
				})
			}
		})
	}
}

func nicBackendPoolProperties() *armnetwork.InterfacePropertiesFormat {
	return &armnetwork.InterfacePropertiesFormat{
		IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
			Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
				LoadBalancerBackendAddressPools: []*armnetwork.BackendAddressPool{{ID: ptr.To("pool")}},
			},
		}},
	}
}

func TestCreateOrUpdateInterface(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	mockInterfaceClient := az.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
	mockInterfaceClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, "nic", gomock.Any()).Return(nil, &azcore.ResponseError{StatusCode: http.StatusInternalServerError})

	err := az.CreateOrUpdateInterface(context.TODO(), &v1.Service{}, &armnetwork.Interface{Name: ptr.To("nic")})
	assert.Contains(t, err.Error(), "UNAVAILABLE")
}
