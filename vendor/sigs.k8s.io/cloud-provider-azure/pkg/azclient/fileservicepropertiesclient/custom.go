/*
Copyright 2023 The Kubernetes Authors.

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

package fileservicepropertiesclient

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	armstorage "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/metrics"
)

const GetOperationName = "FileServicesClient.GetServiceProperties"
const SetOperationName = "FileServicesClient.SetServiceProperties"

// Get gets the FileServiceProperties
func (client *Client) Get(ctx context.Context, resourceGroupName string, resourceName string) (*armstorage.FileServiceProperties, error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "FileService", "getServiceProperties")
	defer func() { metricsCtx.Observe(ctx, nil) }()
	ctx, endSpan := runtime.StartSpan(ctx, GetOperationName, client.tracer, nil)
	defer endSpan(nil)

	resp, err := client.FileServicesClient.GetServiceProperties(ctx, resourceGroupName, resourceName, nil)
	if err != nil {
		return nil, err
	}
	//handle statuscode
	return &resp.FileServiceProperties, nil
}

func (client *Client) Set(ctx context.Context, resourceGroupName string, resourceName string, parameters armstorage.FileServiceProperties) (*armstorage.FileServiceProperties, error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "FileService", "setServiceProperties")
	defer func() { metricsCtx.Observe(ctx, nil) }()
	ctx, endSpan := runtime.StartSpan(ctx, SetOperationName, client.tracer, nil)
	defer endSpan(nil)

	resp, err := client.FileServicesClient.SetServiceProperties(ctx, resourceGroupName, resourceName, parameters, nil)
	if err != nil {
		return nil, err
	}
	return &resp.FileServiceProperties, nil
}
