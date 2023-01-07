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
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2021-09-01/storage"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
	"k8s.io/klog/v2"

	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/armclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

var _ Interface = &Client{}

// Client implements the blobclient interface
type Client struct {
	armClient      armclient.Interface
	subscriptionID string
	cloudName      string
}

// New creates a blobContainersClient
func New(config *azclients.ClientConfig) *Client {
	baseURI := config.ResourceManagerEndpoint
	authorizer := config.Authorizer
	apiVersion := APIVersion

	if strings.EqualFold(config.CloudName, AzureStackCloudName) && !config.DisableAzureStackCloud {
		apiVersion = AzureStackCloudAPIVersion
	}

	klog.V(2).Infof("Azure BlobClient using API version: %s", apiVersion)
	armClient := armclient.New(authorizer, *config, baseURI, apiVersion)

	client := &Client{
		armClient:      armClient,
		subscriptionID: config.SubscriptionID,
		cloudName:      config.CloudName,
	}

	return client
}

// CreateContainer creates a blob container
func (c *Client) CreateContainer(ctx context.Context, subsID, resourceGroupName, accountName, containerName string, parameters storage.BlobContainer) *retry.Error {
	if subsID == "" {
		subsID = c.subscriptionID
	}

	mc := metrics.NewMetricContext("blob_container", "create", resourceGroupName, subsID, "")

	rerr := c.createContainer(ctx, subsID, resourceGroupName, accountName, containerName, parameters)
	mc.Observe(rerr)
	if retry.IsClientRateLimited(rerr) {
		mc.RateLimitedCount()
	}
	if retry.IsClientThrottled(rerr) {
		mc.ThrottledCount()
	}

	return rerr
}

func (c *Client) createContainer(ctx context.Context, subsID, resourceGroupName, accountName, containerName string, parameters storage.BlobContainer) *retry.Error {
	// resourceID format: "/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Storage/storageAccounts/{accountName}/blobServices/default/containers/{containerName}"
	resourceID := armclient.GetChildResourceID(
		subsID,
		resourceGroupName,
		"Microsoft.Storage/storageAccounts",
		accountName,
		"blobServices/default/containers",
		containerName,
	)

	response, rerr := c.armClient.PutResource(ctx, resourceID, parameters)
	defer c.armClient.CloseResponse(ctx, response)
	if rerr != nil {
		klog.V(5).Infof("Received error in %s: resourceID: %s, error: %s", "blob_container.put.request", resourceID, rerr.Error())
		return rerr
	}

	container := storage.BlobContainer{}
	err := autorest.Respond(
		response,
		azure.WithErrorUnlessStatusCode(http.StatusOK, http.StatusCreated),
		autorest.ByUnmarshallingJSON(&container))
	container.Response = autorest.Response{Response: response}

	return retry.GetError(response, err)
}

// DeleteContainer deletes a blob container
func (c *Client) DeleteContainer(ctx context.Context, subsID, resourceGroupName, accountName, containerName string) *retry.Error {
	if subsID == "" {
		subsID = c.subscriptionID
	}

	mc := metrics.NewMetricContext("blob_container", "delete", resourceGroupName, subsID, "")

	rerr := c.deleteContainer(ctx, subsID, resourceGroupName, accountName, containerName)
	mc.Observe(rerr)
	if retry.IsClientRateLimited(rerr) {
		mc.RateLimitedCount()
	}
	if retry.IsClientThrottled(rerr) {
		mc.ThrottledCount()
	}

	return rerr
}

func (c *Client) deleteContainer(ctx context.Context, subsID, resourceGroupName, accountName, containerName string) *retry.Error {
	// resourceID format: "/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Storage/storageAccounts/{accountName}/blobServices/default/containers/{containerName}"
	resourceID := armclient.GetChildResourceID(
		subsID,
		resourceGroupName,
		"Microsoft.Storage/storageAccounts",
		accountName,
		"blobServices/default/containers",
		containerName,
	)

	return c.armClient.DeleteResource(ctx, resourceID)
}

// GetContainer gets a blob container
func (c *Client) GetContainer(ctx context.Context, subsID, resourceGroupName, accountName, containerName string) (storage.BlobContainer, *retry.Error) {
	if subsID == "" {
		subsID = c.subscriptionID
	}

	mc := metrics.NewMetricContext("blob_container", "get", resourceGroupName, subsID, "")

	container, rerr := c.getContainer(ctx, subsID, resourceGroupName, accountName, containerName)
	mc.Observe(rerr)
	if retry.IsClientRateLimited(rerr) {
		mc.RateLimitedCount()
	}
	if retry.IsClientThrottled(rerr) {
		mc.ThrottledCount()
	}

	return container, rerr
}

func (c *Client) getContainer(ctx context.Context, subsID, resourceGroupName, accountName, containerName string) (storage.BlobContainer, *retry.Error) {
	// resourceID format: "/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Storage/storageAccounts/{accountName}/blobServices/default/containers/{containerName}"
	resourceID := armclient.GetChildResourceID(
		subsID,
		resourceGroupName,
		"Microsoft.Storage/storageAccounts",
		accountName,
		"blobServices/default/containers",
		containerName,
	)

	response, rerr := c.armClient.GetResource(ctx, resourceID)
	defer c.armClient.CloseResponse(ctx, response)
	if rerr != nil {
		klog.V(5).Infof("Received error in %s: resourceID: %s, error: %s", "blob_container.get.request", resourceID, rerr.Error())
		return storage.BlobContainer{}, rerr
	}

	container := storage.BlobContainer{}

	err := autorest.Respond(
		response,
		azure.WithErrorUnlessStatusCode(http.StatusOK),
		autorest.ByUnmarshallingJSON(&container))
	if err != nil {
		klog.V(5).Infof("Received error in %s: resourceID: %s, error: %s", "blob_container.get.request", resourceID, err)
		return container, retry.GetError(response, err)
	}

	container.Response = autorest.Response{Response: response}
	return container, nil
}
