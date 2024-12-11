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

const (
	// EnvironmentFilepathName captures the name of the environment variable containing the path to the file
	// to be used while populating the Azure Environment.
	EnvironmentFilepathName = "AZURE_ENVIRONMENT_FILEPATH"
)

func AzureCloudConfigFromName(cloudName string) *cloud.Configuration {
	if cloudName == "" {
		return &cloud.AzurePublic
	}
	cloudName = strings.ToUpper(strings.TrimSpace(cloudName))
	if cloudConfig, ok := EnvironmentMapping[cloudName]; ok {
		return cloudConfig
	}
	return nil
}

// OverrideAzureCloudConfigFromMetadataService returns cloud config from url
// track2 sdk will add this one in the near future https://github.com/Azure/azure-sdk-for-go/issues/20959
func OverrideAzureCloudConfigFromMetadataService(armconfig *ARMClientConfig, config *cloud.Configuration) error {
	if armconfig == nil || armconfig.ResourceManagerEndpoint == "" {
		return nil
	}

	managementEndpoint := fmt.Sprintf("%s%s", strings.TrimSuffix(armconfig.ResourceManagerEndpoint, "/"), "/metadata/endpoints?api-version=2019-05-01")
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
		if armconfig.Cloud == "" || strings.EqualFold(item.Name, armconfig.Cloud) {
			// We use the endpoint to build our config, but on ASH the config returned
			// does not contain the endpoint, and this is not accounted for. This
			// ultimately unsets it for the returned config, causing the bootstrap of
			// the provider to fail. Instead, check if the endpoint is returned, and if
			// It is not then set it.
			if item.ResourceManager == "" {
				item.ResourceManager = armconfig.ResourceManagerEndpoint
			}
			config.Services[cloud.ResourceManager] = cloud.ServiceConfiguration{
				Endpoint: item.ResourceManager,
				Audience: item.Authentication.Audiences[0],
			}
			if item.Authentication.LoginEndpoint != "" {
				config.ActiveDirectoryAuthorityHost = item.Authentication.LoginEndpoint
			}
			if item.Suffixes.Storage != nil && armconfig.StorageSuffix == nil {
				armconfig.StorageSuffix = item.Suffixes.Storage
			}
			if item.Suffixes.AcrLoginServer != nil && armconfig.ACRLoginServer == nil {
				armconfig.ACRLoginServer = item.Suffixes.AcrLoginServer
			}
			return nil
		}
	}
	return nil
}

func OverrideAzureCloudConfigFromEnv(armconfig *ARMClientConfig, config *cloud.Configuration) error {
	envFilePath, ok := os.LookupEnv(EnvironmentFilepathName)
	if !ok {
		return nil
	}
	content, err := os.ReadFile(envFilePath)
	if err != nil {
		return err
	}
	var envConfig Environment
	if err = json.Unmarshal(content, &envConfig); err != nil {
		return err
	}
	if len(envConfig.ActiveDirectoryEndpoint) > 0 {
		config.ActiveDirectoryAuthorityHost = envConfig.ActiveDirectoryEndpoint
	}
	if len(envConfig.ResourceManagerEndpoint) > 0 && len(envConfig.TokenAudience) > 0 {
		config.Services[cloud.ResourceManager] = cloud.ServiceConfiguration{
			Endpoint: envConfig.ResourceManagerEndpoint,
			Audience: envConfig.TokenAudience,
		}
	}
	if len(envConfig.StorageEndpointSuffix) > 0 {
		armconfig.StorageSuffix = &envConfig.StorageEndpointSuffix
	}
	if len(envConfig.ContainerRegistryDNSSuffix) > 0 {
		armconfig.ACRLoginServer = &envConfig.ContainerRegistryDNSSuffix
	}
	return nil
}

// GetAzureCloudConfigAndBackfillArmClientConfig retrieves the Azure cloud configuration based on the provided ARM client configuration.
// If the ARM client configuration is nil, it returns the default Azure public cloud configuration.
// It attempts to override the cloud configuration using metadata service and environment variables.
//
// Parameters:
// - armConfig: A pointer to an ARMClientConfig struct containing the ARM client configuration.
//
// Returns:
// - A pointer to a cloud.Configuration struct representing the Azure cloud configuration.
// - An error if there is an issue overriding the cloud configuration from metadata service or environment variables.
func GetAzureCloudConfigAndBackfillARMClientConfig(armConfig *ARMClientConfig) (*cloud.Configuration, error) {
	config := &cloud.AzurePublic
	if armConfig == nil {
		return config, nil
	}
	config = AzureCloudConfigFromName(armConfig.Cloud)
	if err := OverrideAzureCloudConfigFromMetadataService(armConfig, config); err != nil {
		return nil, err
	}
	err := OverrideAzureCloudConfigFromEnv(armConfig, config)
	return config, err
}

// Environment represents a set of endpoints for each of Azure's Clouds.
type Environment struct {
	Name                       string `json:"name"`
	ResourceManagerEndpoint    string `json:"resourceManagerEndpoint"`
	ActiveDirectoryEndpoint    string `json:"activeDirectoryEndpoint"`
	StorageEndpointSuffix      string `json:"storageEndpointSuffix"`
	ContainerRegistryDNSSuffix string `json:"containerRegistryDNSSuffix"`
	TokenAudience              string `json:"tokenAudience"`
}
