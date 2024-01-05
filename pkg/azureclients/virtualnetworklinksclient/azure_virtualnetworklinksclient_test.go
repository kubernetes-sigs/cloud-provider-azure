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

// Package virtualnetworklinksclient implements the client for VirtualNetworkLinks.
package virtualnetworklinksclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/privatedns/mgmt/2018-09-01/privatedns"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/utils/pointer"

	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient/mockarmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

const (
	testResourceID       = "/subscriptions/subscriptionID/resourceGroups/rg/providers/" + privateDNSZoneResourceType + "/pz1/" + virtualNetworkLinkResourceType + "/vnl1"
	testResourceIDFormat = "/subscriptions/subscriptionID/resourceGroups/rg/providers/" + privateDNSZoneResourceType + "/%s/" + virtualNetworkLinkResourceType + "/%s"
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

	vnlClient := New(config)
	assert.Equal(t, "sub", vnlClient.subscriptionID)
	assert.NotEmpty(t, vnlClient.rateLimiterReader)
	assert.NotEmpty(t, vnlClient.rateLimiterWriter)
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

	vnlClient := getTestVirtualNetworkLinkClient(armClient)
	expected := privatedns.VirtualNetworkLink{Response: autorest.Response{}}
	result, rerr := vnlClient.Get(context.TODO(), "rg", "pz1", "vnl1")
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

	vnlClient := getTestVirtualNetworkLinkClient(armClient)
	expected := privatedns.VirtualNetworkLink{Response: autorest.Response{}}
	result, rerr := vnlClient.Get(context.TODO(), "rg", "pz1", "vnl1")
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

	vnlClient := getTestVirtualNetworkLinkClient(armClient)
	result, rerr := vnlClient.Get(context.TODO(), "rg", "pz1", "vnl1")
	assert.Empty(t, result)
	assert.Equal(t, throttleErr, rerr)
}

func TestCreateOrUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vnl := getTestVirtualNetworkLink("pz1", "vnl1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PutResource(gomock.Any(), pointer.StringDeref(vnl.ID, ""), vnl, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vnlClient := getTestVirtualNetworkLinkClient(armClient)
	rerr := vnlClient.CreateOrUpdate(context.TODO(), "rg", "pz1", "vnl1", vnl, "", true)
	assert.Nil(t, rerr)
}

func TestCreateOrUpdateAsync(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	vnl := getTestVirtualNetworkLink("pz1", "vnl1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().PutResourceAsync(gomock.Any(), pointer.StringDeref(vnl.ID, ""), vnl, gomock.Any()).Return(&azure.Future{}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	vnlClient := getTestVirtualNetworkLinkClient(armClient)
	rerr := vnlClient.CreateOrUpdate(context.TODO(), "rg", "pz1", "vnl1", vnl, "", false)
	assert.Nil(t, rerr)
}

func getTestVirtualNetworkLink(privateZoneName, virtualNetworkLinkName string) privatedns.VirtualNetworkLink {
	return privatedns.VirtualNetworkLink{
		ID:       pointer.String(fmt.Sprintf(testResourceIDFormat, privateZoneName, virtualNetworkLinkName)),
		Name:     pointer.String(virtualNetworkLinkName),
		Location: pointer.String("eastus"),
	}
}

func getTestVirtualNetworkLinkClient(armClient armclient.Interface) *Client {
	rateLimiterReader, rateLimiterWriter := azclients.NewRateLimiter(&azclients.RateLimitConfig{})
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}
