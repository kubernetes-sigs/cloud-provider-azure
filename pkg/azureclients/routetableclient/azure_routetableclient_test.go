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

package routetableclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/Azure/go-autorest/autorest"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/utils/pointer"

	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient/mockarmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

const (
	resourceID = "/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/routeTables/rt1"
)

// 2065-01-24 05:20:00 +0000 UTC
func getFutureTime() time.Time {
	return time.Unix(3000000000, 0)
}

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

	routetableClient := New(config)
	assert.Equal(t, "sub", routetableClient.subscriptionID)
	assert.NotEmpty(t, routetableClient.rateLimiterReader)
	assert.NotEmpty(t, routetableClient.rateLimiterWriter)
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

	routetableClient := New(config)
	assert.Equal(t, "AZURESTACKCLOUD", routetableClient.cloudName)
	assert.Equal(t, "sub", routetableClient.subscriptionID)
}

func TestGet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), resourceID, "").Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	routetableClient := getTestRouteTableClient(armClient)
	expected := network.RouteTable{}
	expected.Response = autorest.Response{Response: response}
	result, rerr := routetableClient.Get(context.TODO(), "rg", "rt1", "")
	assert.Equal(t, expected, result)
	assert.Nil(t, rerr)
}

func TestGetNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), resourceID, "").Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	rtClient := getTestRouteTableClient(armClient)
	expected := network.RouteTable{Response: autorest.Response{}}
	result, rerr := rtClient.Get(context.TODO(), "rg", "rt1", "")
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
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), resourceID, "").Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	rtClient := getTestRouteTableClient(armClient)
	expected := network.RouteTable{Response: autorest.Response{}}
	result, rerr := rtClient.Get(context.TODO(), "rg", "rt1", "")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusInternalServerError, rerr.HTTPStatusCode)
}

func TestGetNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	rtGetErr := &retry.Error{
		RawError:  fmt.Errorf("azure cloud provider rate limited(%s) for operation %q", "read", "RouteTableGet"),
		Retriable: true,
	}

	armClient := mockarmclient.NewMockInterface(ctrl)

	routetableClient := getTestRouteTableClientWithNeverRateLimiter(armClient)
	expected := network.RouteTable{}
	result, rerr := routetableClient.Get(context.TODO(), "rg", "rt1", "")
	assert.Equal(t, expected, result)
	assert.Equal(t, rtGetErr, rerr)
}

func TestGetRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	rtGetErr := &retry.Error{
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "RouteTableGet", "client throttled"),
		Retriable:  true,
		RetryAfter: getFutureTime(),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)

	routetableClient := getTestRouteTableClientWithRetryAfterReader(armClient)
	expected := network.RouteTable{}
	result, rerr := routetableClient.Get(context.TODO(), "rg", "rt1", "")
	assert.Equal(t, expected, result)
	assert.Equal(t, rtGetErr, rerr)
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
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), resourceID, "").Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	routetableClient := getTestRouteTableClient(armClient)
	result, rerr := routetableClient.Get(context.TODO(), "rg", "rt1", "")
	assert.Empty(t, result)
	assert.Equal(t, throttleErr, rerr)
}

func TestCreateOrUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	rt1 := getTestRouteTable("rt1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PutResource(gomock.Any(), pointer.StringDeref(rt1.ID, ""), rt1, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	rtClient := getTestRouteTableClient(armClient)
	rerr := rtClient.CreateOrUpdate(context.TODO(), "rg", "rt1", rt1, "*")
	assert.Nil(t, rerr)
}

func TestCreateOrUpdateWithNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	rcCreateOrUpdateErr := retry.GetRateLimitError(true, "RouteTableCreateOrUpdate")

	rt1 := getTestRouteTable("rt1")
	armClient := mockarmclient.NewMockInterface(ctrl)

	routetableClient := getTestRouteTableClientWithNeverRateLimiter(armClient)
	rerr := routetableClient.CreateOrUpdate(context.TODO(), "rg", "rt1", rt1, "")
	assert.Equal(t, rcCreateOrUpdateErr, rerr)
}

func TestCreateOrUpdateRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	rcCreateOrUpdateErr := retry.GetThrottlingError("RouteTableCreateOrUpdate", "client throttled", getFutureTime())

	rt1 := getTestRouteTable("rt1")
	armClient := mockarmclient.NewMockInterface(ctrl)

	routetableClient := getTestRouteTableClientWithRetryAfterReader(armClient)
	rerr := routetableClient.CreateOrUpdate(context.TODO(), "rg", "rt1", rt1, "")
	assert.NotNil(t, rerr)
	assert.Equal(t, rcCreateOrUpdateErr, rerr)
}

func TestCreateOrUpdateThrottle(t *testing.T) {
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

	rt1 := getTestRouteTable("rt1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().PutResource(gomock.Any(), pointer.StringDeref(rt1.ID, ""), rt1, gomock.Any()).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	routetableClient := getTestRouteTableClient(armClient)
	rerr := routetableClient.CreateOrUpdate(context.TODO(), "rg", "rt1", rt1, "")
	assert.Equal(t, throttleErr, rerr)
}

func TestCreateOrUpdateWithCreateOrUpdateResponderError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	rt1 := getTestRouteTable("rt1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(bytes.NewReader([]byte(""))),
	}

	armClient.EXPECT().PutResource(gomock.Any(), pointer.StringDeref(rt1.ID, ""), rt1, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	routetableClient := getTestRouteTableClient(armClient)
	rerr := routetableClient.CreateOrUpdate(context.TODO(), "rg", "rt1", rt1, "")
	assert.NotNil(t, rerr)
}

func getTestRouteTable(name string) network.RouteTable {
	return network.RouteTable{
		ID:       pointer.String(fmt.Sprintf("/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/routeTables/%s", name)),
		Name:     pointer.String(name),
		Location: pointer.String("eastus"),
	}
}

func getTestRouteTableClient(armClient armclient.Interface) *Client {
	rateLimiterReader, rateLimiterWriter := azclients.NewRateLimiter(&azclients.RateLimitConfig{})
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}

func getTestRouteTableClientWithNeverRateLimiter(armClient armclient.Interface) *Client {
	rateLimiterReader := flowcontrol.NewFakeNeverRateLimiter()
	rateLimiterWriter := flowcontrol.NewFakeNeverRateLimiter()
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}

func getTestRouteTableClientWithRetryAfterReader(armClient armclient.Interface) *Client {
	rateLimiterReader := flowcontrol.NewFakeAlwaysRateLimiter()
	rateLimiterWriter := flowcontrol.NewFakeAlwaysRateLimiter()
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
		RetryAfterReader:  getFutureTime(),
		RetryAfterWriter:  getFutureTime(),
	}
}
