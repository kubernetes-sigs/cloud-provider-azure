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

package loadbalancerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/Azure/go-autorest/autorest"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	"k8s.io/utils/ptr"

	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient/mockarmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

const (
	testResourceID            = "/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1"
	testBackendPoolResourceID = "/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1/backendAddressPools/lb1"
	testResourcePrefix        = "/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/loadBalancers"
)

func TestNew(t *testing.T) {
	config := &azclients.ClientConfig{
		SubscriptionID:          "sub",
		ResourceManagerEndpoint: "endpoint",
		Location:                "eastus",
		RateLimitConfig: &azclients.RateLimitConfig{
			CloudProviderRateLimit:            true,
			CloudProviderRateLimitQPS:         0.5,
			CloudProviderRateLimitBucket:      1,
			CloudProviderRateLimitQPSWrite:    0.5,
			CloudProviderRateLimitBucketWrite: 1,
		},
		Backoff: &retry.Backoff{Steps: 1},
	}

	lbClient := New(config)
	assert.Equal(t, "sub", lbClient.subscriptionID)
	assert.NotEmpty(t, lbClient.rateLimiterReader)
	assert.NotEmpty(t, lbClient.rateLimiterWriter)
}

func TestNewAzureStack(t *testing.T) {
	config := &azclients.ClientConfig{
		CloudName:               "AZURESTACKCLOUD",
		SubscriptionID:          "sub",
		ResourceManagerEndpoint: "endpoint",
		Location:                "eastus",
		RateLimitConfig: &azclients.RateLimitConfig{
			CloudProviderRateLimit:            true,
			CloudProviderRateLimitQPS:         0.5,
			CloudProviderRateLimitBucket:      1,
			CloudProviderRateLimitQPSWrite:    0.5,
			CloudProviderRateLimitBucketWrite: 1,
		},
		Backoff: &retry.Backoff{Steps: 1},
	}

	lbClient := New(config)
	assert.Equal(t, "AZURESTACKCLOUD", lbClient.cloudName)
	assert.Equal(t, "sub", lbClient.subscriptionID)
}

func TestGetNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), testResourceID, "").Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	lbClient := getTestLoadBalancerClient(armClient)
	expected := network.LoadBalancer{Response: autorest.Response{}}
	result, rerr := lbClient.Get(context.TODO(), "rg", "lb1", "")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusNotFound, rerr.HTTPStatusCode)
}

func TestGetInternalError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), testResourceID, "").Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	lbClient := getTestLoadBalancerClient(armClient)
	expected := network.LoadBalancer{Response: autorest.Response{}}
	result, rerr := lbClient.Get(context.TODO(), "rg", "lb1", "")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusInternalServerError, rerr.HTTPStatusCode)
}

func TestGetThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), testResourceID, "").Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	lbClient := getTestLoadBalancerClient(armClient)
	result, rerr := lbClient.Get(context.TODO(), "rg", "lb1", "")
	assert.Empty(t, result)
	assert.Equal(t, throttleErr, rerr)
}

func TestList(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	lbList := []network.LoadBalancer{getTestLoadBalancer("lb1"), getTestLoadBalancer("lb2"), getTestLoadBalancer("lb3")}
	responseBody, err := json.Marshal(network.LoadBalancerListResult{Value: &lbList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(responseBody)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	lbClient := getTestLoadBalancerClient(armClient)
	result, rerr := lbClient.List(context.TODO(), "rg")
	assert.Nil(t, rerr)
	assert.Equal(t, 3, len(result))

	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)
	_, rerr = lbClient.List(context.TODO(), "rg")
	assert.Equal(t, throttleErr, rerr)
}

func TestListWithNextPage(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	lbList := []network.LoadBalancer{getTestLoadBalancer("lb1"), getTestLoadBalancer("lb2"), getTestLoadBalancer("lb3")}
	partialResponse, err := json.Marshal(network.LoadBalancerListResult{Value: &lbList, NextLink: ptr.To("nextLink")})
	assert.NoError(t, err)
	_, err = json.Marshal(network.LoadBalancerListResult{Value: &lbList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(partialResponse)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	lbClient := getTestLoadBalancerClient(armClient)
	result, rerr := lbClient.List(context.TODO(), "rg")
	assert.Nil(t, rerr)
	assert.Equal(t, 3, len(result))
}

func TestListNextResultsMultiPages(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		prepareErr error
		sendErr    *retry.Error
	}{
		{
			prepareErr: nil,
			sendErr:    nil,
		},
		{
			prepareErr: fmt.Errorf("error"),
		},
		{
			sendErr: &retry.Error{RawError: fmt.Errorf("error")},
		},
	}

	lastResult := network.LoadBalancerListResult{
		NextLink: ptr.To("next"),
	}

	for _, test := range tests {
		armClient := mockarmclient.NewMockInterface(ctrl)
		req := &http.Request{
			Method: "GET",
		}
		armClient.EXPECT().PrepareGetRequest(gomock.Any(), gomock.Any()).Return(req, test.prepareErr)
		if test.prepareErr == nil {
			armClient.EXPECT().Send(gomock.Any(), req).Return(&http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader([]byte(`{"foo":"bar"}`))),
			}, test.sendErr)
			armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any())
		}

		lbClient := getTestLoadBalancerClient(armClient)
		result, err := lbClient.listNextResults(context.TODO(), lastResult)
		if test.prepareErr != nil || test.sendErr != nil {
			assert.Error(t, err)
		} else {
			assert.NoError(t, err)
		}
		if test.prepareErr != nil {
			assert.Empty(t, result)
		} else {
			assert.NotEmpty(t, result)
		}
	}
}

func TestCreateOrUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := getTestLoadBalancer("lb1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PutResource(gomock.Any(), ptr.Deref(lb.ID, ""), lb, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	lbClient := getTestLoadBalancerClient(armClient)
	rerr := lbClient.CreateOrUpdate(context.TODO(), "rg", "lb1", lb, "")
	assert.Nil(t, rerr)
}

func TestCreateOrUpdateBackendPools(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	backendAddressPool := getTestBackendAddressPool("lb1", "backendAddressPool1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PutResource(gomock.Any(), ptr.Deref(backendAddressPool.ID, ""), backendAddressPool, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	lbClient := getTestLoadBalancerClient(armClient)
	rerr := lbClient.CreateOrUpdateBackendPools(context.TODO(), "rg", "lb1", "backendAddressPool1", backendAddressPool, "")
	assert.Nil(t, rerr)
}

func TestDelete(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tests := []struct {
		description  string
		armClientErr *retry.Error
		expectedErr  *retry.Error
	}{
		{
			description:  "Delete should report the throttling error",
			armClientErr: &retry.Error{HTTPStatusCode: http.StatusTooManyRequests},
			expectedErr:  &retry.Error{HTTPStatusCode: http.StatusTooManyRequests},
		},
		{
			description: "Delete should not report any error if there's no error from arm client",
		},
	}

	lb := getTestLoadBalancer("lb1")

	for _, test := range tests {
		armClient := mockarmclient.NewMockInterface(ctrl)
		armClient.EXPECT().DeleteResource(gomock.Any(), ptr.Deref(lb.ID, "")).Return(test.armClientErr)

		lbClient := getTestLoadBalancerClient(armClient)
		rerr := lbClient.Delete(context.TODO(), "rg", "lb1")
		assert.Equal(t, test.expectedErr, rerr)
	}
}

func TestGetLBBackendPoolInternalError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), testBackendPoolResourceID, "").Return(response, nil)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any())

	lbClient := getTestLoadBalancerClient(armClient)
	expected := network.BackendAddressPool{Response: autorest.Response{}}
	result, rerr := lbClient.GetLBBackendPool(context.TODO(), "rg", "lb1", "lb1", "")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusInternalServerError, rerr.HTTPStatusCode)
}

func TestGetLBBackendPoolThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), testBackendPoolResourceID, "").Return(response, throttleErr)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any())

	lbClient := getTestLoadBalancerClient(armClient)
	result, rerr := lbClient.GetLBBackendPool(context.TODO(), "rg", "lb1", "lb1", "")
	assert.Empty(t, result)
	assert.Equal(t, throttleErr, rerr)
}

func TestMigrateToIpBasedBackendPools(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PostResource(gomock.Any(), testResourceID, "migrateToIpBased", backendPoolsToBeMigrated{BackendPoolNames: []string{"lb1"}}, gomock.Any()).Return(response, nil)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any())

	lbClient := getTestLoadBalancerClient(armClient)
	rerr := lbClient.MigrateToIPBasedBackendPool(context.TODO(), "rg", "lb1", []string{"lb1"})
	assert.Nil(t, rerr)
}

func getTestLoadBalancer(name string) network.LoadBalancer {
	return network.LoadBalancer{
		ID:       ptr.To(fmt.Sprintf("/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/%s", name)),
		Name:     ptr.To(name),
		Location: ptr.To("eastus"),
	}
}

func getTestBackendAddressPool(lbName, backendPoolName string) network.BackendAddressPool {
	return network.BackendAddressPool{
		ID:   ptr.To(fmt.Sprintf("/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/%s/backendAddressPools/%s", lbName, backendPoolName)),
		Name: ptr.To(backendPoolName),
		BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
			Location: ptr.To("eastus"),
		},
	}
}

func getTestLoadBalancerClient(armClient armclient.Interface) *Client {
	rateLimiterReader, rateLimiterWriter := azclients.NewRateLimiter(&azclients.RateLimitConfig{})
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}
