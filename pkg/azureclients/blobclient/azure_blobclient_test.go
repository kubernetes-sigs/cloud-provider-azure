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
	"context"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2021-09-01/storage"
	"github.com/stretchr/testify/assert"

	"github.com/golang/mock/gomock"
	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/blobclient/mockblobclient"
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

	blobclient := mockblobclient.NewMockInterface(ctrl)
	blobclient.EXPECT().CreateContainer(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	err := blobclient.CreateContainer(context.TODO(), "rg", "accountname", "containername", storage.BlobContainer{})
	assert.Nil(t, err)
}

func TestDeleteContainer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	blobclient := mockblobclient.NewMockInterface(ctrl)
	blobclient.EXPECT().DeleteContainer(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	err := blobclient.DeleteContainer(context.TODO(), "rg", "accountname", "containername")
	assert.Nil(t, err)
}

func TestGetContainer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	blobclient := mockblobclient.NewMockInterface(ctrl)
	blobclient.EXPECT().GetContainer(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(storage.BlobContainer{}, nil)

	_, err := blobclient.GetContainer(context.TODO(), "rg", "accountname", "containername")
	assert.Nil(t, err)
}
