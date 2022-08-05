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

package blobclient

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2021-09-01/storage"
	"github.com/Azure/go-autorest/autorest"
	"github.com/stretchr/testify/assert"

	"github.com/golang/mock/gomock"
	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient/mockarmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
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

	blobclient := New(config)
	assert.Equal(t, "sub", blobclient.subscriptionID)
}

func TestCreateContainer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte(""))),
	}

	armClient.EXPECT().PutResource(gomock.Any(), gomock.Any(), gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	blobClient := getTestBlobClient(armClient)

	rerr := blobClient.CreateContainer(context.Background(), "subsID", "resourceGroupName", "accountName", "containerName", storage.BlobContainer{})
	assert.Nil(t, rerr)

	// test throttle error from server
	response = &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}

	armClient.EXPECT().PutResource(gomock.Any(), gomock.Any(), gomock.Any()).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)
	rerr = blobClient.CreateContainer(context.Background(), "", "resourceGroupName", "accountName", "containerName", storage.BlobContainer{})
	assert.Equal(t, throttleErr, rerr)

	// test throttle error from client
	rerr = blobClient.CreateContainer(context.Background(), "", "resourceGroupName", "accountName", "containerName", storage.BlobContainer{})
	throttleErr = &retry.Error{
		Retriable:  true,
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "CreateBlobContainer", "client throttled"),
		RetryAfter: time.Unix(100, 0),
	}
	assert.Equal(t, throttleErr, rerr)

	// test rate limit
	rerr = blobClient.CreateContainer(context.Background(), "", "resourceGroupName", "accountName", "containerName", storage.BlobContainer{})
	rateLimitedErr := &retry.Error{
		Retriable: true,
		RawError:  fmt.Errorf("azure cloud provider %s(%s) for operation %q", retry.RateLimited, "write", "CreateBlobContainer"),
	}
	assert.Equal(t, rateLimitedErr, rerr)
}

func TestDeleteContainer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	armClient := mockarmclient.NewMockInterface(ctrl)
	armClient.EXPECT().DeleteResource(gomock.Any(), gomock.Any()).Return(nil).Times(1)

	blobClient := getTestBlobClient(armClient)

	rerr := blobClient.DeleteContainer(context.TODO(), "subsID", "resourceGroupName", "accountName", "containerName")
	assert.Nil(t, rerr)

	// test throttle error from server
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}

	armClient.EXPECT().DeleteResource(gomock.Any(), gomock.Any()).Return(throttleErr).Times(1)
	rerr = blobClient.DeleteContainer(context.Background(), "", "resourceGroupName", "accountName", "containerName")
	assert.Equal(t, throttleErr, rerr)

	// test throttle error from client
	rerr = blobClient.DeleteContainer(context.Background(), "", "resourceGroupName", "accountName", "containerName")
	throttleErr = &retry.Error{
		Retriable:  true,
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "BlobContainerDelete", "client throttled"),
		RetryAfter: time.Unix(100, 0),
	}
	assert.Equal(t, throttleErr, rerr)

	// test rate limit
	rerr = blobClient.DeleteContainer(context.Background(), "", "resourceGroupName", "accountName", "containerName")
	rateLimitedErr := &retry.Error{
		Retriable: true,
		RawError:  fmt.Errorf("azure cloud provider %s(%s) for operation %q", retry.RateLimited, "write", "BlobContainerDelete"),
	}
	assert.Equal(t, rateLimitedErr, rerr)
}

func TestGetContainer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	response := &http.Response{
		StatusCode: http.StatusOK,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte(""))),
	}

	armClient := mockarmclient.NewMockInterface(ctrl)

	armClient.EXPECT().GetResource(gomock.Any(), gomock.Any()).Return(response, nil).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)

	blobClient := getTestBlobClient(armClient)

	container, rerr := blobClient.GetContainer(context.Background(), "subsID", "resourceGroupName", "accountName", "containerName")
	expectedContainer := storage.BlobContainer{
		Response: autorest.Response{
			Response: response,
		},
	}
	assert.Nil(t, rerr)
	assert.Equal(t, expectedContainer, container)

	// test throttle error from server
	response = &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       ioutil.NopCloser(bytes.NewReader([]byte("{}"))),
	}
	throttleErr := &retry.Error{
		HTTPStatusCode: http.StatusTooManyRequests,
		RawError:       fmt.Errorf("error"),
		Retriable:      true,
		RetryAfter:     time.Unix(100, 0),
	}

	armClient.EXPECT().GetResource(gomock.Any(), gomock.Any()).Return(response, throttleErr).Times(1)
	armClient.EXPECT().CloseResponse(gomock.Any(), gomock.Any()).Times(1)
	container, rerr = blobClient.GetContainer(context.Background(), "", "resourceGroupName", "accountName", "containerName")
	assert.Empty(t, container)
	assert.Equal(t, throttleErr, rerr)

	// test throttle error from client
	container, rerr = blobClient.GetContainer(context.Background(), "", "resourceGroupName", "accountName", "containerName")
	throttleErr = &retry.Error{
		Retriable:  true,
		RawError:   fmt.Errorf("azure cloud provider throttled for operation %s with reason %q", "GetBlobContainer", "client throttled"),
		RetryAfter: time.Unix(100, 0),
	}
	assert.Empty(t, container)
	assert.Equal(t, throttleErr, rerr)

	// test rate limit
	container, rerr = blobClient.GetContainer(context.Background(), "", "resourceGroupName", "accountName", "containerName")
	assert.Empty(t, container)
	rateLimitedErr := &retry.Error{
		Retriable: true,
		RawError:  fmt.Errorf("azure cloud provider %s(%s) for operation %q", retry.RateLimited, "read", "GetBlobContainer"),
	}
	assert.Equal(t, rateLimitedErr, rerr)
}

func getTestBlobClient(armClient armclient.Interface) *Client {
	rateLimiterReader, rateLimiterWriter := azclients.NewRateLimiter(
		&azclients.RateLimitConfig{
			CloudProviderRateLimit:            true,
			CloudProviderRateLimitQPS:         3,
			CloudProviderRateLimitBucket:      3,
			CloudProviderRateLimitQPSWrite:    3,
			CloudProviderRateLimitBucketWrite: 3,
		})
	return &Client{
		armClient:         armClient,
		subscriptionID:    "subscriptionID",
		rateLimiterReader: rateLimiterReader,
		rateLimiterWriter: rateLimiterWriter,
		now:               func() time.Time { return time.Unix(99, 0) },
	}
}
