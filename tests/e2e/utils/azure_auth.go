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
	"os"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
)

// Environmental variables for validating Azure resource status.
const (
	TenantIDEnv               = "AZURE_TENANT_ID"
	SubscriptionEnv           = "AZURE_SUBSCRIPTION_ID"
	ServicePrincipleIDEnv     = "AZURE_CLIENT_ID"
	ServicePrincipleSecretEnv = "AZURE_CLIENT_SECRET" // #nosec G101
	ClusterLocationEnv        = "AZURE_LOCATION"
	ClusterEnvironment        = "AZURE_ENVIRONMENT"
	LoadBalancerSkuEnv        = "AZURE_LOADBALANCER_SKU"
)

// azureAuthConfigFromTestProfile obtains azure config from Environment
func azureAuthConfigFromTestProfile() (*azclient.AzureAuthConfig, *azclient.ARMClientConfig, *azclient.ClientFactoryConfig, error) {
	envStr := os.Getenv(ClusterEnvironment)
	if len(envStr) == 0 {
		envStr = "AZUREPUBLICCLOUD"
	}
	return &azclient.AzureAuthConfig{
			AADClientID:     os.Getenv(ServicePrincipleIDEnv),
			AADClientSecret: os.Getenv(ServicePrincipleSecretEnv),
		}, &azclient.ARMClientConfig{
			Cloud:    envStr,
			TenantID: os.Getenv(TenantIDEnv),
		}, &azclient.ClientFactoryConfig{
			SubscriptionID: os.Getenv(SubscriptionEnv),
		}, nil
}
