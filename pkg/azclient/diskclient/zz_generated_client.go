// /*
// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// */

// Code generated by client-gen. DO NOT EDIT.
package diskclient

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/tracing"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

const AzureStackCloudAPIVersion = "2019-03-01"

type Client struct {
	*armcompute.DisksClient
	subscriptionID string
	tracer         tracing.Tracer
}

func New(subscriptionID string, credential azcore.TokenCredential, options *arm.ClientOptions) (Interface, error) {
	if options == nil {
		options = utils.GetDefaultOption()
	}
	tr := options.TracingProvider.NewTracer(utils.ModuleName, utils.ModuleVersion)

	client, err := armcompute.NewDisksClient(subscriptionID, credential, options)
	if err != nil {
		return nil, err
	}
	return &Client{
		DisksClient:    client,
		subscriptionID: subscriptionID,
		tracer:         tr,
	}, nil
}

const GetOperationName = "DisksClient.Get"

// Get gets the Disk
func (client *Client) Get(ctx context.Context, resourceGroupName string, diskName string) (result *armcompute.Disk, err error) {

	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "Disk", "get")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, GetOperationName, client.tracer, nil)
	defer endSpan(err)
	resp, err := client.DisksClient.Get(ctx, resourceGroupName, diskName, nil)
	if err != nil {
		return nil, err
	}
	//handle statuscode
	return &resp.Disk, nil
}

const CreateOrUpdateOperationName = "DisksClient.Create"

// CreateOrUpdate creates or updates a Disk.
func (client *Client) CreateOrUpdate(ctx context.Context, resourceGroupName string, diskName string, resource armcompute.Disk) (result *armcompute.Disk, err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "Disk", "create_or_update")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, CreateOrUpdateOperationName, client.tracer, nil)
	defer endSpan(err)
	resp, err := utils.NewPollerWrapper(client.DisksClient.BeginCreateOrUpdate(ctx, resourceGroupName, diskName, resource, nil)).WaitforPollerResp(ctx)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		return &resp.Disk, nil
	}
	return nil, nil
}

const DeleteOperationName = "DisksClient.Delete"

// Delete deletes a Disk by name.
func (client *Client) Delete(ctx context.Context, resourceGroupName string, diskName string) (err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "Disk", "delete")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, DeleteOperationName, client.tracer, nil)
	defer endSpan(err)
	_, err = utils.NewPollerWrapper(client.BeginDelete(ctx, resourceGroupName, diskName, nil)).WaitforPollerResp(ctx)
	return err
}

const ListOperationName = "DisksClient.List"

// List gets a list of Disk in the resource group.
func (client *Client) List(ctx context.Context, resourceGroupName string) (result []*armcompute.Disk, err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "Disk", "list")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, ListOperationName, client.tracer, nil)
	defer endSpan(err)
	pager := client.DisksClient.NewListByResourceGroupPager(resourceGroupName, nil)
	for pager.More() {
		nextResult, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, nextResult.Value...)
	}
	return result, nil
}
