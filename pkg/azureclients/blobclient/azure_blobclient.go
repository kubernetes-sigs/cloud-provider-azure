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
	"fmt"

	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2021-09-01/storage"

	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

// Client implements the blobclient interface
type Client struct {
	subscriptionID       string
	blobContainersClient storage.BlobContainersClient
}

// New creates a blobContainersClient
func New(config *azclients.ClientConfig) *Client {
	blobContainersClient := storage.NewBlobContainersClientWithBaseURI(config.ResourceManagerEndpoint, config.SubscriptionID)
	blobContainersClient.Authorizer = config.Authorizer
	return &Client{
		subscriptionID:       config.SubscriptionID,
		blobContainersClient: blobContainersClient,
	}
}

// CreateContainer creates a blob container
func (c *Client) CreateContainer(ctx context.Context, resourceGroupName, accountName, containerName string, blobContainer storage.BlobContainer) error {
	if resourceGroupName == "" || accountName == "" || containerName == "" {
		return fmt.Errorf("empty value in resourceGroupName(%s), accountName(%s), containerName(%s)", resourceGroupName, accountName, containerName)
	}
	mc := metrics.NewMetricContext("blob_container", "create", resourceGroupName, c.subscriptionID, "")
	_, err := c.blobContainersClient.Create(ctx, resourceGroupName, accountName, containerName, blobContainer)
	var rerr *retry.Error
	if err != nil {
		rerr = &retry.Error{
			RawError: err,
		}
	}
	mc.Observe(rerr)
	return err
}

// DeleteContainer deletes a blob container
func (c *Client) DeleteContainer(ctx context.Context, resourceGroupName, accountName, containerName string) error {
	if resourceGroupName == "" || accountName == "" || containerName == "" {
		return fmt.Errorf("empty value in resourceGroupName(%s), accountName(%s), containerName(%s)", resourceGroupName, accountName, containerName)
	}
	mc := metrics.NewMetricContext("blob_container", "delete", resourceGroupName, c.subscriptionID, "")
	_, err := c.blobContainersClient.Delete(ctx, resourceGroupName, accountName, containerName)
	var rerr *retry.Error
	if err != nil {
		rerr = &retry.Error{
			RawError: err,
		}
	}
	mc.Observe(rerr)
	return err
}

// GetContainer gets a blob container
func (c *Client) GetContainer(ctx context.Context, resourceGroupName, accountName, containerName string) (storage.BlobContainer, error) {
	if resourceGroupName == "" || accountName == "" || containerName == "" {
		return storage.BlobContainer{}, fmt.Errorf("empty value in resourceGroupName(%s), accountName(%s), containerName(%s)", resourceGroupName, accountName, containerName)
	}
	mc := metrics.NewMetricContext("blob_container", "get", resourceGroupName, c.subscriptionID, "")
	blobContainer, err := c.blobContainersClient.Get(ctx, resourceGroupName, accountName, containerName)
	var rerr *retry.Error
	if err != nil {
		rerr = &retry.Error{
			RawError: err,
		}
	}
	mc.Observe(rerr)
	return blobContainer, err
}
