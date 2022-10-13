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
	"context"
	"fmt"
	"os"

	azauth "github.com/Azure/azure-sdk-for-go/services/authorization/mgmt/2015-07-01/authorization"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/to"

	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/klog/v2"
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
	// If "TEST_CCM" is true, the test is running on a CAPZ cluster.
	CAPZTestCCM = "TEST_CCM"
	// If "E2E_ON_AKS_CLUSTER" is true, the test is running on a AKS cluster.
	AKSTestCCM     = "E2E_ON_AKS_CLUSTER"
	AKSClusterType = "CLUSTER_TYPE"
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
	// The ID of the Azure Subscription that the cluster is deployed in
	SubscriptionID string
	// The Environment represents a set of endpoints for each of Azure's Clouds.
	Environment azure.Environment
}

// getServicePrincipalToken creates a new service principal token based on the configuration
func getServicePrincipalToken(config *AzureAuthConfig) (*adal.ServicePrincipalToken, error) {
	oauthConfig, err := adal.NewOAuthConfig(config.Environment.ActiveDirectoryEndpoint, config.TenantID)
	if err != nil {
		return nil, fmt.Errorf("creating the OAuth config: %w", err)
	}

	if len(config.AADClientSecret) > 0 {
		klog.V(2).Infoln("azure: using client_id+client_secret to retrieve access token")
		return adal.NewServicePrincipalToken(
			*oauthConfig,
			config.AADClientID,
			config.AADClientSecret,
			config.Environment.ServiceManagementEndpoint)
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
		TenantID:        os.Getenv(TenantIDEnv),
		AADClientID:     os.Getenv(ServicePrincipleIDEnv),
		AADClientSecret: os.Getenv(ServicePrincipleSecretEnv),
		SubscriptionID:  os.Getenv(SubscriptionEnv),
		Environment:     env,
	}
	return c, nil
}

// assignRoleByID assigns role by roleDefinitionID to the given scope
func (tc *AzureTestClient) assignRoleByID(scope, roleDefinitionID string) (result azauth.RoleAssignment, err error) {
	authconfig := tc.GetAuthConfig()
	roleAssignmentsClient := tc.createRoleAssignmentsClient()

	return roleAssignmentsClient.Create(
		context.Background(),
		scope,
		string(uuid.NewUUID()),
		azauth.RoleAssignmentCreateParameters{
			Properties: &azauth.RoleAssignmentProperties{
				PrincipalID:      to.StringPtr(authconfig.AADClientID),
				RoleDefinitionID: to.StringPtr(roleDefinitionID),
			},
		})
}
