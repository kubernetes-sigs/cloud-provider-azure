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

	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"

	"k8s.io/klog/v2"
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
	managedIdentityType       = "E2E_MANAGED_IDENTITY_TYPE"
	federatedTokenFile        = "AZURE_FEDERATED_TOKEN_FILE"

	userAssignedManagedIdentity = "userassigned"
)

// AzureAuthConfig holds auth related part of cloud config
// Only consider servicePrinciple now
type AzureAuthConfig struct {
	// The AAD Tenant ID for the Subscription that the cluster is deployed in
	TenantID string
	// The ClientID for an AAD application with RBAC access to talk to Azure RM APIs
	AADClientID string
	// The ClientSecret for an AAD application with RBAC access to talk to Azure RM APIs
	AADClientSecret string
	// MSI client ID
	UserAssignedIdentityID string
	// The ID of the Azure Subscription that the cluster is deployed in
	SubscriptionID string
	// The Environment represents a set of endpoints for each of Azure's Clouds.
	Environment azure.Environment
	// AADFederatedTokenFile is the path to the federated token file
	AADFederatedTokenFile string
	// UseFederatedWorkloadIdentityExtension is a flag to enable the federated workload identity extension
	UseFederatedWorkloadIdentityExtension bool
}

// getServicePrincipalToken creates a new service principal token based on the configuration
func getServicePrincipalToken(config *AzureAuthConfig) (*adal.ServicePrincipalToken, error) {
	oauthConfig, err := adal.NewOAuthConfig(config.Environment.ActiveDirectoryEndpoint, config.TenantID)
	if err != nil {
		return nil, fmt.Errorf("creating the OAuth config: %w", err)
	}

	if len(config.AADFederatedTokenFile) > 0 {
		klog.Infoln("azure: using federated token to retrieve access token")
		jwtCallback := func() (string, error) {
			jwt, err := os.ReadFile(config.AADFederatedTokenFile)
			if err != nil {
				return "", fmt.Errorf("failed to read a file with a federated token: %w", err)
			}
			return string(jwt), nil
		}

		token, err := adal.NewServicePrincipalTokenFromFederatedTokenCallback(
			*oauthConfig, config.AADClientID, jwtCallback, config.Environment.ResourceManagerEndpoint,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create a workload identity token: %w", err)
		}
		return token, nil
	}

	if len(config.AADClientSecret) > 0 {
		klog.Infoln("azure: using client_id+client_secret to retrieve access token")
		return adal.NewServicePrincipalToken(
			*oauthConfig,
			config.AADClientID,
			config.AADClientSecret,
			config.Environment.ServiceManagementEndpoint)
	}
	if len(config.UserAssignedIdentityID) > 0 {
		klog.Infoln("azure: using MSI client ID to retrieve access token")
		miOptions := adal.ManagedIdentityOptions{
			ClientID: config.UserAssignedIdentityID,
		}
		return adal.NewServicePrincipalTokenFromManagedIdentity(
			config.Environment.ServiceManagementEndpoint,
			&miOptions)
	}
	if len(config.AADFederatedTokenFile) > 0 {
		klog.Infoln("azure: using federated token to retrieve access token")
	}

	return nil, fmt.Errorf("No credentials provided for AAD application %s", config.AADClientID)
}

// azureAuthConfigFromTestProfile obtains azure config from Environment
func azureAuthConfigFromTestProfile() (*AzureAuthConfig, error) {
	var env azure.Environment
	envStr := os.Getenv(ClusterEnvironment)
	if len(envStr) == 0 {
		env = azure.PublicCloud
	} else {
		var err error
		env, err = azure.EnvironmentFromName(envStr)
		if err != nil {
			return nil, err
		}
	}

	c := &AzureAuthConfig{
		TenantID:       os.Getenv(TenantIDEnv),
		SubscriptionID: os.Getenv(SubscriptionEnv),
		Environment:    env,
	}

	aadClientIDEnv := os.Getenv(AADClientIDEnv)
	servicePrincipleSecretEnv := os.Getenv(ServicePrincipleSecretEnv)
	managedIdentityTypeEnv := os.Getenv(managedIdentityType)
	managedIdentityClientIDEnv := os.Getenv(managedIdentityClientID)
	federatedTokenFileEnv := os.Getenv(federatedTokenFile)
	if aadClientIDEnv != "" && servicePrincipleSecretEnv != "" {
		c.AADClientID = aadClientIDEnv
		c.AADClientSecret = servicePrincipleSecretEnv
	} else if managedIdentityTypeEnv != "" {
		if managedIdentityTypeEnv == userAssignedManagedIdentity {
			c.UserAssignedIdentityID = managedIdentityClientIDEnv
		}
	} else if federatedTokenFileEnv != "" {
		c = &AzureAuthConfig{
			AADFederatedTokenFile:                 federatedTokenFileEnv,
			UseFederatedWorkloadIdentityExtension: true,
			AADClientID:                           aadClientIDEnv,
		}
	} else {
		return c, fmt.Errorf("failed to get Azure auth config from environment")
	}

	return c, nil
}
