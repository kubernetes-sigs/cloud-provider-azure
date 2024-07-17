/*
Copyright 2021 The Kubernetes Authors.

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
package privatelinkserviceclient

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
	testResourceID     = "/subscriptions/subscriptionID/resourceGroups/rg/providers/" + PLSResourceType + "/pls1"
	testResourcePrefix = "/subscriptions/subscriptionID/resourceGroups/rg/providers/" + PLSResourceType
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

	plsClient := New(config)
	assert.Equal(t, "sub", plsClient.subscriptionID)
	assert.NotEmpty(t, plsClient.rateLimiterReader)
	assert.NotEmpty(t, plsClient.rateLimiterWriter)
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

	plsClient := New(config)
	assert.Equal(t, "AZURESTACKCLOUD", plsClient.cloudName)
	assert.Equal(t, "sub", plsClient.subscriptionID)
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

	plsClient := getTestPrivateLinkServiceClient(armClient)
	expected := network.PrivateLinkService{Response: autorest.Response{}}
	result, rerr := plsClient.Get(context.TODO(), "rg", "pls1", "")
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

	plsClient := getTestPrivateLinkServiceClient(armClient)
	expected := network.PrivateLinkService{Response: autorest.Response{}}
	result, rerr := plsClient.Get(context.TODO(), "rg", "pls1", "")
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

	plsClient := getTestPrivateLinkServiceClient(armClient)
	result, rerr := plsClient.Get(context.TODO(), "rg", "pls1", "")
	assert.Empty(t, result)
	assert.Equal(t, throttleErr, rerr)
}

func TestList(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	plsList := []network.PrivateLinkService{getTestPrivateLinkService("pls1"), getTestPrivateLinkService("pls2"), getTestPrivateLinkService("pls3")}
	responseBody, err := json.Marshal(network.PrivateLinkServiceListResult{Value: &plsList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(responseBody)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	plsClient := getTestPrivateLinkServiceClient(armClient)
	result, rerr := plsClient.List(context.TODO(), "rg")
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
	_, rerr = plsClient.List(context.TODO(), "rg")
	assert.Equal(t, throttleErr, rerr)
}

func TestListWithNextPage(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	plsList := []network.PrivateLinkService{getTestPrivateLinkService("pls1"), getTestPrivateLinkService("pls2"), getTestPrivateLinkService("pls3")}
	partialResponse, err := json.Marshal(network.PrivateLinkServiceListResult{Value: &plsList, NextLink: ptr.To("nextLink")})
	assert.NoError(t, err)
	_, err = json.Marshal(network.PrivateLinkServiceListResult{Value: &plsList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(partialResponse)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	plsClient := getTestPrivateLinkServiceClient(armClient)
	result, rerr := plsClient.List(context.TODO(), "rg")
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

	lastResult := network.PrivateLinkServiceListResult{
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

		plsClient := getTestPrivateLinkServiceClient(armClient)
		result, err := plsClient.listNextResults(context.TODO(), lastResult)
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

	pls := getTestPrivateLinkService("pls1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PutResource(gomock.Any(), ptr.Deref(pls.ID, ""), pls, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	plsClient := getTestPrivateLinkServiceClient(armClient)
	rerr := plsClient.CreateOrUpdate(context.TODO(), "rg", "pls1", pls, "")
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

	pls := getTestPrivateLinkService("pls1")

	for _, test := range tests {
		armClient := mockarmclient.NewMockInterface(ctrl)
		armClient.EXPECT().DeleteResource(gomock.Any(), ptr.Deref(pls.ID, "")).Return(test.armClientErr)

		plsClient := getTestPrivateLinkServiceClient(armClient)
		rerr := plsClient.Delete(context.TODO(), "rg", "pls1")
		assert.Equal(t, test.expectedErr, rerr)
	}
}

func TestDeletePEConnection(t *testing.T) {
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

	peConn := getTestPrivateEndpointConnection("pls1", "peconn")

	for _, test := range tests {
		armClient := mockarmclient.NewMockInterface(ctrl)
		armClient.EXPECT().DeleteResource(gomock.Any(), ptr.Deref(peConn.ID, "")).Return(test.armClientErr)

		plsClient := getTestPrivateLinkServiceClient(armClient)
		rerr := plsClient.DeletePEConnection(context.TODO(), "rg", "pls1", "peconn")
		assert.Equal(t, test.expectedErr, rerr)
	}
}

func getTestPrivateLinkService(name string) network.PrivateLinkService {
	return network.PrivateLinkService{
		ID:       ptr.To(fmt.Sprintf("/subscriptions/subscriptionID/resourceGroups/rg/providers/%s/%s", PLSResourceType, name)),
		Name:     ptr.To(name),
		Location: ptr.To("eastus"),
	}
}

func getTestPrivateEndpointConnection(PLSName string, PEConnName string) network.PrivateEndpointConnection {
	return network.PrivateEndpointConnection{
		ID:   ptr.To(fmt.Sprintf("/subscriptions/subscriptionID/resourceGroups/rg/providers/%s/%s/%s/%s", PLSResourceType, PLSName, PEConnResourceType, PEConnName)),
		Name: ptr.To(PEConnName),
	}
}

func getTestPrivateLinkServiceClient(armClient armclient.Interface) *Client {
	rateLimiterReader, rateLimiterWriter := azclients.NewRateLimiter(&azclients.RateLimitConfig{})
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}
