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

package privatednsclient

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
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/utils/ptr"

	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient/mockarmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

const (
	sub0 = "sub0"
	rg0  = "rg0"
	pz0  = "pz0"

	testResourceID     = "/subscriptions/sub0/resourceGroups/rg0/providers/" + privateDNSZoneResourceType + "/" + pz0
	testResourcePrefix = "/subscriptions/sub0/resourceGroups/rg0/providers/" + privateDNSZoneResourceType
)

func TestNew(t *testing.T) {
	config := &azclients.ClientConfig{
		SubscriptionID:          sub0,
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

	pzClient := New(config)
	assert.Equal(t, sub0, pzClient.subscriptionID)
	assert.NotEmpty(t, pzClient.rateLimiterReader)
	assert.NotEmpty(t, pzClient.rateLimiterWriter)
}

func TestCreateOrUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pz := getTestPrivateDNSZone(pz0)
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PutResource(gomock.Any(), ptr.Deref(pz.ID, ""), pz, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	pzClient := getTestPrivateDNSZoneClient(armClient)
	rerr := pzClient.CreateOrUpdate(context.TODO(), rg0, pz0, pz, "", true)
	assert.Nil(t, rerr)
}

func TestCreateOrUpdateWithNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	rcCreateOrUpdateErr := retry.GetRateLimitError(true, "PrivateDNSZoneCreateOrUpdate")

	pz := getTestPrivateDNSZone(pz0)
	armClient := mockarmclient.NewMockInterface(ctrl)

	pzClient := getTestPrivateDNSZoneClientWithNeverRateLimiter(armClient)
	rerr := pzClient.CreateOrUpdate(context.TODO(), rg0, pz0, pz, "", true)
	assert.Equal(t, rcCreateOrUpdateErr, rerr)
}

func TestCreateOrUpdateRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	rcCreateOrUpdateErr := retry.GetThrottlingError("PrivateDNSZoneCreateOrUpdate", "client throttled", getFutureTime())

	pz := getTestPrivateDNSZone(pz0)
	armClient := mockarmclient.NewMockInterface(ctrl)

	pzClient := getTestPrivateDNSZoneClientWithRetryAfterReader(armClient)
	rerr := pzClient.CreateOrUpdate(context.TODO(), rg0, pz0, pz, "", true)
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

	pz := getTestPrivateDNSZone(pz0)
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().PutResource(gomock.Any(), ptr.Deref(pz.ID, ""), pz, gomock.Any()).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	pzClient := getTestPrivateDNSZoneClient(armClient)
	rerr := pzClient.CreateOrUpdate(context.TODO(), rg0, pz0, pz, "", true)
	assert.Equal(t, throttleErr, rerr)
}

func TestCreateOrUpdateWithCreateOrUpdateResponderError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pz := getTestPrivateDNSZone(pz0)
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(bytes.NewReader([]byte(""))),
	}

	armClient.EXPECT().PutResource(gomock.Any(), ptr.Deref(pz.ID, ""), pz, gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	pzClient := getTestPrivateDNSZoneClient(armClient)
	rerr := pzClient.CreateOrUpdate(context.TODO(), rg0, pz0, pz, "", true)
	assert.NotNil(t, rerr)
}

func getTestPrivateDNSZone(name string) privatedns.PrivateZone {
	return privatedns.PrivateZone{
		ID:       ptr.To(fmt.Sprintf("%s/%s", testResourcePrefix, name)),
		Name:     ptr.To(name),
		Location: ptr.To("eastus"),
	}
}

func getTestPrivateDNSZoneClient(armClient armclient.Interface) *Client {
	rateLimiterReader, rateLimiterWriter := azclients.NewRateLimiter(&azclients.RateLimitConfig{})
	return &Client{
		armClient:         armClient,
		subscriptionID:    sub0,
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}

func getTestPrivateDNSZoneClientWithNeverRateLimiter(armClient armclient.Interface) *Client {
	rateLimiterReader := flowcontrol.NewFakeNeverRateLimiter()
	rateLimiterWriter := flowcontrol.NewFakeNeverRateLimiter()
	return &Client{
		armClient:         armClient,
		subscriptionID:    sub0,
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}

func getTestPrivateDNSZoneClientWithRetryAfterReader(armClient armclient.Interface) *Client {
	rateLimiterReader := flowcontrol.NewFakeAlwaysRateLimiter()
	rateLimiterWriter := flowcontrol.NewFakeAlwaysRateLimiter()
	return &Client{
		armClient:         armClient,
		subscriptionID:    sub0,
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
		RetryAfterReader:  getFutureTime(),
		RetryAfterWriter:  getFutureTime(),
	}
}

func TestGetNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResource(gomock.Any(), testResourceID).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	pzClient := getTestPrivateDNSZoneClient(armClient)
	expected := privatedns.PrivateZone{Response: autorest.Response{}}
	result, rerr := pzClient.Get(context.TODO(), rg0, pz0)
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
	armClient.EXPECT().GetResource(gomock.Any(), testResourceID).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	pzClient := getTestPrivateDNSZoneClient(armClient)
	expected := privatedns.PrivateZone{Response: autorest.Response{}}
	result, rerr := pzClient.Get(context.TODO(), rg0, pz0)
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
	armClient.EXPECT().GetResource(gomock.Any(), testResourceID).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	pzClient := getTestPrivateDNSZoneClient(armClient)
	result, rerr := pzClient.Get(context.TODO(), rg0, pz0)
	assert.Empty(t, result)
	assert.Equal(t, throttleErr, rerr)
}

// 2065-01-24 05:20:00 +0000 UTC
func getFutureTime() time.Time {
	return time.Unix(3000000000, 0)
}
