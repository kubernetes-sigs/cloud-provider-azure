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

package snapshotclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
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
	testResourceID     = "/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Compute/snapshots/sn1"
	testResourcePrefix = "/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Compute/snapshots"
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

	snClient := New(config)
	assert.Equal(t, "sub", snClient.subscriptionID)
	assert.NotEmpty(t, snClient.rateLimiterReader)
	assert.NotEmpty(t, snClient.rateLimiterWriter)
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

	snClient := New(config)
	assert.Equal(t, "AZURESTACKCLOUD", snClient.cloudName)
	assert.Equal(t, "sub", snClient.subscriptionID)
}

func TestGet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResource(gomock.Any(), testResourceID).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	expected := compute.Snapshot{Response: autorest.Response{Response: response}}
	snClient := getTestSnapshotClient(armClient)
	result, rerr := snClient.Get(context.TODO(), "", "rg", "sn1")
	assert.Equal(t, expected, result)
	assert.Nil(t, rerr)
}

func TestGetNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	snGetErr := &retry.Error{
		RawError:  fmt.Errorf("azure cloud provider rate limited(%s) for operation %q", "read", "SnapshotGet"),
		Retriable: true,
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	snClient := getTestSnapshotClientWithNeverRateLimiter(armClient)
	expected := compute.Snapshot{}
	result, rerr := snClient.Get(context.TODO(), "", "rg", "sn1")
	assert.Equal(t, expected, result)
	assert.Equal(t, snGetErr, rerr)
}

func TestGetRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	snGetErr := &retry.Error{
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "SnapshotGet", "client throttled"),
		Retriable:  true,
		RetryAfter: getFutureTime(),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	snClient := getTestSnapshotClientWithRetryAfterReader(armClient)
	expected := compute.Snapshot{}
	result, rerr := snClient.Get(context.TODO(), "", "rg", "sn1")
	assert.Equal(t, expected, result)
	assert.Equal(t, snGetErr, rerr)
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

	snClient := getTestSnapshotClient(armClient)
	expected := compute.Snapshot{Response: autorest.Response{}}
	result, rerr := snClient.Get(context.TODO(), "", "rg", "sn1")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusNotFound, rerr.HTTPStatusCode)
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

	snClient := getTestSnapshotClient(armClient)
	result, rerr := snClient.Get(context.TODO(), "", "rg", "sn1")
	assert.Empty(t, result)
	assert.Equal(t, throttleErr, rerr)
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

	snClient := getTestSnapshotClient(armClient)
	expected := compute.Snapshot{Response: autorest.Response{}}
	result, rerr := snClient.Get(context.TODO(), "", "rg", "sn1")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusInternalServerError, rerr.HTTPStatusCode)
}

func TestListByResourceGroup(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	snList := []compute.Snapshot{getTestSnapshot("sn1"), getTestSnapshot("sn2"), getTestSnapshot("sn3")}
	responseBody, err := json.Marshal(compute.SnapshotList{Value: &snList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(responseBody)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	snClient := getTestSnapshotClient(armClient)
	result, rerr := snClient.ListByResourceGroup(context.TODO(), "", "rg")
	assert.Nil(t, rerr)
	assert.Equal(t, 3, len(result))
}

func TestListByResourceGroupNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	snClient := getTestSnapshotClient(armClient)
	expected := []compute.Snapshot{}
	result, rerr := snClient.ListByResourceGroup(context.TODO(), "", "rg")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusNotFound, rerr.HTTPStatusCode)
}

func TestListByResourceGroupInternalError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	snClient := getTestSnapshotClient(armClient)
	expected := []compute.Snapshot{}
	result, rerr := snClient.ListByResourceGroup(context.TODO(), "", "rg")
	assert.Equal(t, expected, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, http.StatusInternalServerError, rerr.HTTPStatusCode)
}

func TestListByResourceGroupThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
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
	snClient := getTestSnapshotClient(armClient)
	result, rerr := snClient.ListByResourceGroup(context.TODO(), "", "rg")
	assert.Empty(t, result)
	assert.NotNil(t, rerr)
	assert.Equal(t, throttleErr, rerr)
}

func TestListByResourceGroupWithListResponderError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	snList := []compute.Snapshot{getTestSnapshot("sn1"), getTestSnapshot("sn2"), getTestSnapshot("sn3")}
	responseBody, err := json.Marshal(compute.SnapshotList{Value: &snList})
	assert.NoError(t, err)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(
		&http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(bytes.NewReader(responseBody)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)
	snClient := getTestSnapshotClient(armClient)
	result, rerr := snClient.ListByResourceGroup(context.TODO(), "", "rg")
	assert.NotNil(t, rerr)
	assert.Equal(t, 0, len(result))
}

func TestListWithNextPage(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	snList := []compute.Snapshot{getTestSnapshot("sn1"), getTestSnapshot("sn2"), getTestSnapshot("sn3")}
	partialResponse, err := json.Marshal(compute.SnapshotList{Value: &snList, NextLink: ptr.To("nextLink")})
	assert.NoError(t, err)
	pagedResponse, err := json.Marshal(compute.SnapshotList{Value: &snList})
	assert.NoError(t, err)
	armClient.EXPECT().PrepareGetRequest(gomock.Any(), gomock.Any()).Return(&http.Request{}, nil)
	armClient.EXPECT().Send(gomock.Any(), gomock.Any()).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(pagedResponse)),
		}, nil)
	armClient.EXPECT().GetResource(gomock.Any(), testResourcePrefix).Return(
		&http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(partialResponse)),
		}, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(2)
	snClient := getTestSnapshotClient(armClient)
	result, rerr := snClient.ListByResourceGroup(context.TODO(), "", "rg")
	assert.Nil(t, rerr)
	assert.Equal(t, 6, len(result))
}

func TestListByResourceGroupNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	snListErr := &retry.Error{
		RawError:  fmt.Errorf("azure cloud provider rate limited(%s) for operation %q", "read", "SnapshotListByResourceGroup"),
		Retriable: true,
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	snClient := getTestSnapshotClientWithNeverRateLimiter(armClient)
	result, rerr := snClient.ListByResourceGroup(context.TODO(), "", "rg")
	assert.Equal(t, 0, len(result))
	assert.NotNil(t, rerr)
	assert.Equal(t, snListErr, rerr)
}

func TestListByResourceGroupRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	snListErr := &retry.Error{
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "SnapshotListByResourceGroup", "client throttled"),
		Retriable:  true,
		RetryAfter: getFutureTime(),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	snClient := getTestSnapshotClientWithRetryAfterReader(armClient)
	result, rerr := snClient.ListByResourceGroup(context.TODO(), "", "rg")
	assert.Equal(t, 0, len(result))
	assert.NotNil(t, rerr)
	assert.Equal(t, snListErr, rerr)
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

	lastResult := compute.SnapshotList{
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

		snClient := getTestSnapshotClient(armClient)
		result, err := snClient.listNextResults(context.TODO(), lastResult)
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

func TestListNextResultsMultiPagesWithListResponderError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	test := struct {
		prepareErr error
		sendErr    *retry.Error
	}{
		prepareErr: nil,
		sendErr:    nil,
	}

	lastResult := compute.SnapshotList{
		NextLink: ptr.To("next"),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	req := &http.Request{
		Method: "GET",
	}
	armClient.EXPECT().PrepareGetRequest(gomock.Any(), gomock.Any()).Return(req, test.prepareErr)
	if test.prepareErr == nil {
		armClient.EXPECT().Send(gomock.Any(), req).Return(&http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"foo":"bar"}`))),
		}, test.sendErr)
		armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any())
	}

	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(bytes.NewBuffer([]byte(`{"foo":"bar"}`))),
	}
	expected := compute.SnapshotList{}
	expected.Response = autorest.Response{Response: response}
	snClient := getTestSnapshotClient(armClient)
	result, err := snClient.listNextResults(context.TODO(), lastResult)
	assert.Error(t, err)
	assert.Equal(t, expected, result)
}

func TestCreateOrUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	sn := getTestSnapshot("sn1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PutResource(gomock.Any(), ptr.Deref(sn.ID, ""), sn).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	snClient := getTestSnapshotClient(armClient)
	rerr := snClient.CreateOrUpdate(context.TODO(), "", "rg", "sn1", sn)
	assert.Nil(t, rerr)
}

func TestCreateOrUpdateWithCreateOrUpdateResponderError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sn := getTestSnapshot("sn1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(bytes.NewReader([]byte(""))),
	}
	armClient.EXPECT().PutResource(gomock.Any(), ptr.Deref(sn.ID, ""), sn).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	snClient := getTestSnapshotClient(armClient)
	rerr := snClient.CreateOrUpdate(context.TODO(), "", "rg", "sn1", sn)
	assert.NotNil(t, rerr)
}

func TestCreateOrUpdateNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	snCreateOrUpdateErr := retry.GetRateLimitError(true, "SnapshotCreateOrUpdate")

	armClient := mockarmclient.NewMockInterface(ctrl)
	snClient := getTestSnapshotClientWithNeverRateLimiter(armClient)
	sn := getTestSnapshot("sn1")
	rerr := snClient.CreateOrUpdate(context.TODO(), "", "rg", "sn1", sn)
	assert.NotNil(t, rerr)
	assert.Equal(t, snCreateOrUpdateErr, rerr)
}

func TestCreateOrUpdateRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	snCreateOrUpdateErr := retry.GetThrottlingError("SnapshotCreateOrUpdate", "client throttled", getFutureTime())

	sn := getTestSnapshot("sn1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	snClient := getTestSnapshotClientWithRetryAfterReader(armClient)
	rerr := snClient.CreateOrUpdate(context.TODO(), "", "rg", "sn1", sn)
	assert.NotNil(t, rerr)
	assert.Equal(t, snCreateOrUpdateErr, rerr)
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

	sn := getTestSnapshot("sn1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().PutResource(gomock.Any(), ptr.Deref(sn.ID, ""), sn).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	snClient := getTestSnapshotClient(armClient)
	rerr := snClient.CreateOrUpdate(context.TODO(), "", "rg", "sn1", sn)
	assert.NotNil(t, rerr)
	assert.Equal(t, throttleErr, rerr)
}

func TestDelete(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	r := getTestSnapshot("sn1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().DeleteResource(gomock.Any(), ptr.Deref(r.ID, "")).Return(nil).Times(1)

	rtClient := getTestSnapshotClient(armClient)
	rerr := rtClient.Delete(context.TODO(), "", "rg", "sn1")
	assert.Nil(t, rerr)
}

func TestDeleteNeverRateLimiter(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	snDeleteErr := &retry.Error{
		RawError:  fmt.Errorf("azure cloud provider rate limited(%s) for operation %q", "write", "SnapshotDelete"),
		Retriable: true,
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	snClient := getTestSnapshotClientWithNeverRateLimiter(armClient)
	rerr := snClient.Delete(context.TODO(), "", "rg", "sn1")
	assert.NotNil(t, rerr)
	assert.Equal(t, snDeleteErr, rerr)
}

func TestDeleteRetryAfterReader(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	snDeleteErr := &retry.Error{
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "SnapshotDelete", "client throttled"),
		Retriable:  true,
		RetryAfter: getFutureTime(),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)
	snClient := getTestSnapshotClientWithRetryAfterReader(armClient)
	rerr := snClient.Delete(context.TODO(), "", "rg", "sn1")
	assert.NotNil(t, rerr)
	assert.Equal(t, snDeleteErr, rerr)
}

func TestDeleteThrottle(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}

	sn := getTestSnapshot("sn1")
	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().DeleteResource(gomock.Any(), ptr.Deref(sn.ID, "")).Return(throttleErr).Times(1)

	snClient := getTestSnapshotClient(armClient)
	rerr := snClient.Delete(context.TODO(), "", "rg", "sn1")
	assert.NotNil(t, rerr)
	assert.Equal(t, throttleErr, rerr)
}

func getTestSnapshot(name string) compute.Snapshot {
	return compute.Snapshot{
		ID:       ptr.To(fmt.Sprintf("/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Compute/snapshots/%s", name)),
		Name:     ptr.To(name),
		Location: ptr.To("eastus"),
	}
}

func getTestSnapshotClient(armClient armclient.Interface) *Client {
	rateLimiterReader, rateLimiterWriter := azclients.NewRateLimiter(&azclients.RateLimitConfig{})
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}

func getTestSnapshotClientWithNeverRateLimiter(armClient armclient.Interface) *Client {
	rateLimiterReader := flowcontrol.NewFakeNeverRateLimiter()
	rateLimiterWriter := flowcontrol.NewFakeNeverRateLimiter()
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
	}
}

func getTestSnapshotClientWithRetryAfterReader(armClient armclient.Interface) *Client {
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

func getFutureTime() time.Time {
	return time.Unix(3000000000, 0)
}
