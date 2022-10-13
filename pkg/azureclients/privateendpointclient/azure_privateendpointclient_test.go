/*
Copyright 2022 The Kubernetes Authors.

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

// Package privatelinkserviceclient implements the client for PrivateLinkService.
package privateendpointclient

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"
	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient/mockarmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

const (
	testResourceID     = "/subscriptions/subscriptionID/resourceGroups/rg/providers/" + PEResourceType + "/pe1"
	testResourcePrefix = "/subscriptions/subscriptionID/resourceGroups/rg/providers/" + PEResourceType
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

	peClient := New(config)
	assert.Equal(t, "sub", peClient.subscriptionID)
	assert.NotEmpty(t, peClient.rateLimiterReader)
	assert.NotEmpty(t, peClient.rateLimiterWriter)
}

func TestGetNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), testResourceID, "").Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	peClient := getTestPrivateEndpointClient(armClient)
	expected := network.PrivateEndpoint{Response: autorest.Response{}}
	result, rerr := peClient.Get(context.TODO(), "rg", "pe1", "")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusNotFound, rerr.HTTPStatusCode)
}

func TestGetInternalError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResourceWithExpandQuery(gomock.Any(), testResourceID, "").Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	peClient := getTestPrivateEndpointClient(armClient)
	expected := network.PrivateEndpoint{Response: autorest.Response{}}
	result, rerr := peClient.Get(context.TODO(), "rg", "pe1", "")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusInternalServerError, rerr.HTTPStatusCode)
}

func TestGetThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
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

	peClient := getTestPrivateEndpointClient(armClient)
	result, rerr := peClient.Get(context.TODO(), "rg", "pe1", "")
	assert.Empty(t, result)
	assert.Equal(t, throttleErr, rerr)
}

func TestCreateOrUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pe := getTestPrivateEndpoint("pe1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PutResource(gomock.Any(), to.String(pe.ID), pe, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	peClient := getTestPrivateEndpointClient(armClient)
	rerr := peClient.CreateOrUpdate(context.TODO(), "rg", "pe1", pe, "", true)
	assert.Nil(t, rerr)
}

func TestCreateOrUpdateAsync(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pe := getTestPrivateEndpoint("pe1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().PutResourceAsync(gomock.Any(), to.String(pe.ID), pe, gomock.Any()).Return(&azure.Future{}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	peClient := getTestPrivateEndpointClient(armClient)
	rerr := peClient.CreateOrUpdate(context.TODO(), "rg", "pe1", pe, "", false)
	assert.Nil(t, rerr)
}

func getTestPrivateEndpoint(name string) network.PrivateEndpoint {
	return network.PrivateEndpoint{
		ID:       to.StringPtr(fmt.Sprintf("%s/%s", testResourcePrefix, name)),
		Name:     to.StringPtr(name),
		Location: to.StringPtr("eastus"),
	}
}

func getTestPrivateEndpointClient(armClient armclient.Interface) *Client {
	rateLimiterReader, rateLimiterWriter := azclients.NewRateLimiter(&azclients.RateLimitConfig{})
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}
