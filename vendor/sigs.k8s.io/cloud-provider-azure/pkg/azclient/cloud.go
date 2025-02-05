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

package azclient

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
)

var EnvironmentMapping = map[string]*cloud.Configuration{
	"AZURECHINACLOUD":        &cloud.AzureChina,
	"AZURECLOUD":             &cloud.AzurePublic,
	"AZUREPUBLICCLOUD":       &cloud.AzurePublic,
	"AZUREUSGOVERNMENT":      &cloud.AzureGovernment,
	"AZUREUSGOVERNMENTCLOUD": &cloud.AzureGovernment, //TODO: deprecate
}

const AzureStackCloudName = "AZURESTACKCLOUD"
const (
	// EnvironmentFilepathName captures the name of the environment variable containing the path to the file
	// to be used while populating the Azure Environment.
	EnvironmentFilepathName = "AZURE_ENVIRONMENT_FILEPATH"
)

func AzureCloudConfigFromName(cloudName string) *cloud.Configuration {
	cloudName = strings.ToUpper(strings.TrimSpace(cloudName))
	if cloudConfig, ok := EnvironmentMapping[cloudName]; ok {
		return cloudConfig
	}
	return &cloud.AzurePublic
}

// OverrideAzureCloudConfigAndEnvConfigFromMetadataService returns cloud config and environment config from url
// track2 sdk will add this one in the near future https://github.com/Azure/azure-sdk-for-go/issues/20959
// cloud and env should not be empty
// it should never return an empty config
func OverrideAzureCloudConfigAndEnvConfigFromMetadataService(endpoint, cloudName string, cloudConfig *cloud.Configuration, env *Environment) error {
	// If the ResourceManagerEndpoint is not set, we should not query the metadata service
	if endpoint == "" {
		return nil
	}

	managementEndpoint := fmt.Sprintf("%s%s", strings.TrimSuffix(endpoint, "/"), "/metadata/endpoints?api-version=2019-05-01")
	res, err := http.Get(managementEndpoint) //nolint
	if err != nil {
		return err
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	metadata := []struct {
		Name            string `json:"name"`
		ResourceManager string `json:"resourceManager,omitempty"`
		Authentication  struct {
			Audiences     []string `json:"audiences"`
			LoginEndpoint string   `json:"loginEndpoint,omitempty"`
		} `json:"authentication"`
		Suffixes struct {
			AcrLoginServer *string `json:"acrLoginServer,omitempty"`
			Storage        *string `json:"storage,omitempty"`
		} `json:"suffixes,omitempty"`
	}{}
	err = json.Unmarshal(body, &metadata)
	if err != nil {
		return err
	}

	for _, item := range metadata {
		if cloudName == "" || strings.EqualFold(item.Name, cloudName) {
			// We use the endpoint to build our config, but on ASH the config returned
			// does not contain the endpoint, and this is not accounted for. This
			// ultimately unsets it for the returned config, causing the bootstrap of
			// the provider to fail. Instead, check if the endpoint is returned, and if
			// It is not then set it.
			if item.ResourceManager == "" {
				item.ResourceManager = endpoint
			}
			cloudConfig.Services[cloud.ResourceManager] = cloud.ServiceConfiguration{
				Endpoint: item.ResourceManager,
				Audience: item.Authentication.Audiences[0],
			}
			env.ResourceManagerEndpoint = item.ResourceManager
			env.TokenAudience = item.Authentication.Audiences[0]
			if item.Authentication.LoginEndpoint != "" {
				cloudConfig.ActiveDirectoryAuthorityHost = item.Authentication.LoginEndpoint
				env.ActiveDirectoryEndpoint = item.Authentication.LoginEndpoint
			}
			if item.Suffixes.Storage != nil {
				env.StorageEndpointSuffix = *item.Suffixes.Storage
			}
			if item.Suffixes.AcrLoginServer != nil {
				env.ContainerRegistryDNSSuffix = *item.Suffixes.AcrLoginServer
			}
			return nil
		}
	}
	return nil
}

func OverrideAzureCloudConfigFromEnv(cloudName string, config *cloud.Configuration, env *Environment) error {
	if !strings.EqualFold(cloudName, AzureStackCloudName) {
		return nil
	}
	envFilePath, ok := os.LookupEnv(EnvironmentFilepathName)
	if !ok {
		return nil
	}
	content, err := os.ReadFile(envFilePath)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(content, env); err != nil {
		return err
	}
	if len(env.ResourceManagerEndpoint) > 0 && len(env.TokenAudience) > 0 {
		config.Services[cloud.ResourceManager] = cloud.ServiceConfiguration{
			Endpoint: env.ResourceManagerEndpoint,
			Audience: env.TokenAudience,
		}
	}
	if len(env.ActiveDirectoryEndpoint) > 0 {
		config.ActiveDirectoryAuthorityHost = env.ActiveDirectoryEndpoint
	}
	return nil
}

func GetAzureCloudConfigAndEnvConfig(armConfig *ARMClientConfig) (cloud.Configuration, *Environment, error) {
	env := &Environment{}
	var cloudName string
	if armConfig != nil {
		cloudName = armConfig.Cloud
	}
	config := AzureCloudConfigFromName(cloudName)
	if armConfig == nil {
		return *config, nil, nil
	}

	err := OverrideAzureCloudConfigAndEnvConfigFromMetadataService(armConfig.ResourceManagerEndpoint, cloudName, config, env)
	if err != nil {
		return *config, nil, err
	}
	err = OverrideAzureCloudConfigFromEnv(cloudName, config, env)
	return *config, env, err
}

// Environment represents a set of endpoints for each of Azure's Clouds.
type Environment struct {
	Name                       string `json:"name"`
	ServiceManagementEndpoint  string `json:"serviceManagementEndpoint"`
	ResourceManagerEndpoint    string `json:"resourceManagerEndpoint"`
	ActiveDirectoryEndpoint    string `json:"activeDirectoryEndpoint"`
	StorageEndpointSuffix      string `json:"storageEndpointSuffix"`
	ContainerRegistryDNSSuffix string `json:"containerRegistryDNSSuffix"`
	TokenAudience              string `json:"tokenAudience"`
}
