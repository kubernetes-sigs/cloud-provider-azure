/*
Copyright 2018 The Kubernetes Authors.

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

package utils

import (
	"fmt"
	"os"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
)

// Environmental variables for validating Azure resource status.
const (
	TenantIDEnv               = "AZURE_TENANT_ID"
	SubscriptionEnv           = "AZURE_SUBSCRIPTION_ID"
	AADClientIDEnv            = "AZURE_CLIENT_ID"
	ServicePrincipleSecretEnv = "AZURE_CLIENT_SECRET" // #nosec G101
	ClusterLocationEnv        = "AZURE_LOCATION"
	ClusterEnvironment        = "AZURE_ENVIRONMENT"
	LoadBalancerSkuEnv        = "AZURE_LOADBALANCER_SKU"
	managedIdentityClientID   = "AZURE_MANAGED_IDENTITY_CLIENT_ID"
	federatedTokenFile        = "AZURE_FEDERATED_TOKEN_FILE"
	managedIdentityType       = "E2E_MANAGED_IDENTITY_TYPE"

	userAssignedManagedIdentity = "userassigned"
)

// azureAuthConfigFromTestProfile obtains azure config from Environment
func azureAuthConfigFromTestProfile() (*azclient.AzureAuthConfig, *azclient.ARMClientConfig, *azclient.ClientFactoryConfig, error) {
	envStr := os.Getenv(ClusterEnvironment)
	if len(envStr) == 0 {
		envStr = "AZUREPUBLICCLOUD"
	}

	var azureAuthConfig azclient.AzureAuthConfig
	aadClientIDEnv := os.Getenv(AADClientIDEnv)
	servicePrincipleSecretEnv := os.Getenv(ServicePrincipleSecretEnv)
	managedIdentityTypeEnv := os.Getenv(managedIdentityType)
	managedIdentityClientIDEnv := os.Getenv(managedIdentityClientID)
	federatedTokenFileEnv := os.Getenv(federatedTokenFile)
	if aadClientIDEnv != "" && servicePrincipleSecretEnv != "" {
		azureAuthConfig = azclient.AzureAuthConfig{
			AADClientID:     aadClientIDEnv,
			AADClientSecret: servicePrincipleSecretEnv,
		}
	} else if managedIdentityTypeEnv != "" {
		azureAuthConfig = azclient.AzureAuthConfig{
			UseManagedIdentityExtension: true,
		}
		if managedIdentityTypeEnv == userAssignedManagedIdentity {
			azureAuthConfig.UserAssignedIdentityID = managedIdentityClientIDEnv

		}
	} else if federatedTokenFileEnv != "" {
		azureAuthConfig = azclient.AzureAuthConfig{
			AADFederatedTokenFile:                 federatedTokenFileEnv,
			UseFederatedWorkloadIdentityExtension: true,
			AADClientID:                           aadClientIDEnv,
		}
	} else {
		return nil, nil, nil, fmt.Errorf("failed to get Azure auth config from environment")
	}

	return &azureAuthConfig, &azclient.ARMClientConfig{
			Cloud:    envStr,
			TenantID: os.Getenv(TenantIDEnv),
		}, &azclient.ClientFactoryConfig{
			SubscriptionID: os.Getenv(SubscriptionEnv),
		}, nil
}
