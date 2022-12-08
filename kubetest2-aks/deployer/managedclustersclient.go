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

package deployer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	armruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	armcontainerservicev2 "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v2"
)

/*
Background:
AKS custom configuration feature is an internal feature used for testing only, and is not expected to be exposed to
customers, thus custom configuration field is not defined in AKS API and SDK.

Workaround:
To leverage this feature while reusing as many code in SDK as possible, we wrap ManagedClustersClient struct and extend
create functions with encodedCustomConfig variables. ManagedCluster parameter in SDK is not touched but transformed to
a managable map structure before marshalled to request body by `insertCustomConfigToParameter`.
*/

const (
	moduleName    = "armcontainerservice"
	moduleVersion = "v2.2.0"
	armAPIVersion = "2022-09-01"
)

// ManagedClustersClientWrapper is a wrapper of NewManagedClustersClient by supporting custom configuration.
type ManagedClustersClientWrapper struct {
	*armcontainerservicev2.ManagedClustersClient
	host           string
	subscriptionID string
	pl             runtime.Pipeline
}

// NewManagedClustersClientWrapper returns a ManagedClustersClientWrapper.
func NewManagedClustersClientWrapper(subscriptionID string, credential azcore.TokenCredential, options *arm.ClientOptions) (*ManagedClustersClientWrapper, error) {
	if options == nil {
		options = &arm.ClientOptions{}
	}
	ep := cloud.AzurePublic.Services[cloud.ResourceManager].Endpoint
	if c, ok := options.Cloud.Services[cloud.ResourceManager]; ok {
		ep = c.Endpoint
	}
	pl, err := armruntime.NewPipeline(moduleName, moduleVersion, credential, runtime.PipelineOptions{}, options)
	if err != nil {
		return nil, err
	}

	managedClustersClient, err := armcontainerservicev2.NewManagedClustersClient(subscriptionID, credential, options)
	if err != nil {
		return nil, err
	}
	return &ManagedClustersClientWrapper{
		ManagedClustersClient: managedClustersClient,
		subscriptionID:        subscriptionID,
		host:                  ep,
		pl:                    pl,
	}, nil
}

// BeginCreateOrUpdate - Creates or updates a managed cluster.
// If the operation fails it returns an *azcore.ResponseError type.
// Generated from API version 2022-09-01
// resourceGroupName - The name of the resource group. The name is case insensitive.
// resourceName - The name of the managed cluster resource.
// parameters - The managed cluster to create or update.
// options - ManagedClustersClientBeginCreateOrUpdateOptions contains the optional parameters for the ManagedClustersClient.BeginCreateOrUpdate
// method.
func (client *ManagedClustersClientWrapper) BeginCreateOrUpdate(ctx context.Context, resourceGroupName string, resourceName string, parameters armcontainerservicev2.ManagedCluster, encodedCustomConfig string, options *armcontainerservicev2.ManagedClustersClientBeginCreateOrUpdateOptions) (*runtime.Poller[armcontainerservicev2.ManagedClustersClientCreateOrUpdateResponse], error) {
	if options == nil || options.ResumeToken == "" {
		resp, err := client.createOrUpdate(ctx, resourceGroupName, resourceName, parameters, encodedCustomConfig, options)
		if err != nil {
			return nil, err
		}
		return runtime.NewPoller[armcontainerservicev2.ManagedClustersClientCreateOrUpdateResponse](resp, client.pl, nil)
	} else {
		return runtime.NewPollerFromResumeToken[armcontainerservicev2.ManagedClustersClientCreateOrUpdateResponse](options.ResumeToken, client.pl, nil)
	}
}

// CreateOrUpdate - Creates or updates a managed cluster.
// If the operation fails it returns an *azcore.ResponseError type.
// Generated from API version 2022-09-01
func (client *ManagedClustersClientWrapper) createOrUpdate(ctx context.Context, resourceGroupName string, resourceName string, parameters armcontainerservicev2.ManagedCluster, encodedCustomConfig string, options *armcontainerservicev2.ManagedClustersClientBeginCreateOrUpdateOptions) (*http.Response, error) {
	req, err := client.createOrUpdateCreateRequest(ctx, resourceGroupName, resourceName, parameters, encodedCustomConfig, options)
	if err != nil {
		return nil, err
	}
	resp, err := client.pl.Do(req)
	if err != nil {
		return nil, err
	}
	if !runtime.HasStatusCode(resp, http.StatusOK, http.StatusCreated) {
		return nil, runtime.NewResponseError(resp)
	}
	return resp, nil
}

// createOrUpdateCreateRequest creates the CreateOrUpdate request.
func (client *ManagedClustersClientWrapper) createOrUpdateCreateRequest(ctx context.Context, resourceGroupName string, resourceName string, parameters armcontainerservicev2.ManagedCluster, encodedCustomConfig string, options *armcontainerservicev2.ManagedClustersClientBeginCreateOrUpdateOptions) (*policy.Request, error) {
	urlPath := "/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.ContainerService/managedClusters/{resourceName}"
	if client.subscriptionID == "" {
		return nil, errors.New("parameter client.subscriptionID cannot be empty")
	}
	urlPath = strings.ReplaceAll(urlPath, "{subscriptionId}", url.PathEscape(client.subscriptionID))
	if resourceGroupName == "" {
		return nil, errors.New("parameter resourceGroupName cannot be empty")
	}
	urlPath = strings.ReplaceAll(urlPath, "{resourceGroupName}", url.PathEscape(resourceGroupName))
	if resourceName == "" {
		return nil, errors.New("parameter resourceName cannot be empty")
	}
	urlPath = strings.ReplaceAll(urlPath, "{resourceName}", url.PathEscape(resourceName))
	req, err := runtime.NewRequest(ctx, http.MethodPut, runtime.JoinPaths(client.host, urlPath))
	if err != nil {
		return nil, err
	}
	reqQP := req.Raw().URL.Query()
	reqQP.Set("api-version", armAPIVersion)
	req.Raw().URL.RawQuery = reqQP.Encode()
	req.Raw().Header["Accept"] = []string{"application/json"}

	parametersWithCustomConfig, err := insertCustomConfigToParameter(parameters, encodedCustomConfig)
	if err != nil {
		return req, err
	}

	return req, runtime.MarshalAsJSON(req, parametersWithCustomConfig)
}

// insertCustomConfigToParameter inserts custom configuration into the managed cluster and returns a map to be Marshaled into request body.
// To simply custom configuration insert, no new data structured is introduced on top of ManagedCluster, which contains no filed for
// custom configuration by design, and reuse as many code as possible, we transform ManagedCluster object into a managable map struct
// before being marshalled to request body. The idea is borrowed from the following link:
// https://stackoverflow.com/questions/51795678/add-a-new-key-value-pair-to-a-json-object
func insertCustomConfigToParameter(parameters armcontainerservicev2.ManagedCluster, encodedCustomConfiguration string) (map[string]interface{}, error) {
	mcBytes, err := json.Marshal(parameters)
	if err != nil {
		return map[string]interface{}{}, err
	}

	mcMap := map[string]interface{}{}
	if err := json.Unmarshal(mcBytes, &mcMap); err != nil {
		return map[string]interface{}{}, err
	}
	propertiesMap, ok := mcMap["properties"].(map[string]interface{})
	if !ok {
		return map[string]interface{}{}, errors.New("the properties is not a map type.")
	}

	propertiesMap["encodedCustomConfiguration"] = encodedCustomConfiguration
	mcMap["properties"] = propertiesMap

	return mcMap, nil
}
