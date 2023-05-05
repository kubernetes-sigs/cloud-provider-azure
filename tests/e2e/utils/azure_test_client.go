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
	"regexp"

	v1 "k8s.io/api/core/v1"

	azauth "github.com/Azure/azure-sdk-for-go/services/authorization/mgmt/2015-07-01/authorization"
	azcompute "github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	acr "github.com/Azure/azure-sdk-for-go/services/containerregistry/mgmt/2019-05-01/containerregistry"
	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	azresources "github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2018-05-01/resources"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
)

var (
	azureResourceGroupNameRE = regexp.MustCompile(`.*/subscriptions/(?:.*)/resourceGroups/(.+)/providers/(?:.*)`)
	nodeLabelLocation        = "failure-domain.beta.kubernetes.io/region"
	defaultLocation          = "eastus2"
)

// AzureTestClient configs Azure specific clients
type AzureTestClient struct {
	location       string
	resourceGroup  string
	authConfig     AzureAuthConfig
	networkClient  aznetwork.BaseClient
	resourceClient azresources.BaseClient
	acrClient      acr.BaseClient
	authClient     azauth.BaseClient
	computeClient  azcompute.BaseClient

	IPFamily IPFamily
}

// CreateAzureTestClient makes a new AzureTestClient
// Only consider PublicCloud Environment
func CreateAzureTestClient() (*AzureTestClient, error) {
	authConfig, err := azureAuthConfigFromTestProfile()
	if err != nil {
		return nil, err
	}
	servicePrincipleToken, err := getServicePrincipalToken(authConfig)
	if err != nil {
		return nil, err
	}
	baseClient := aznetwork.NewWithBaseURI(azure.PublicCloud.TokenAudience, authConfig.SubscriptionID)
	baseClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipleToken)

	resourceBaseClient := azresources.BaseClient(baseClient)
	resourceBaseClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipleToken)

	acrClient := acr.NewWithBaseURI(azure.PublicCloud.TokenAudience, authConfig.SubscriptionID)
	acrClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipleToken)

	authClient := azauth.NewWithBaseURI(azure.PublicCloud.TokenAudience, authConfig.SubscriptionID)
	authClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipleToken)

	computeClient := azcompute.NewWithBaseURI(azure.PublicCloud.TokenAudience, authConfig.SubscriptionID)
	computeClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipleToken)

	kubeClient, err := CreateKubeClientSet()
	if err != nil {
		return nil, err
	}

	nodes, err := WaitGetAgentNodes(kubeClient)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes available for the cluster")
	}

	resourceGroup, err := getResourceGroupFromProviderID(nodes[0].Spec.ProviderID)
	if err != nil {
		return nil, err
	}
	location := getLocationFromNodeLabels(&nodes[0])

	ipFamily, err := GetClusterServiceIPFamily()
	if err != nil {
		return nil, err
	}

	c := &AzureTestClient{
		location:       location,
		resourceGroup:  resourceGroup,
		authConfig:     *authConfig,
		networkClient:  baseClient,
		resourceClient: resourceBaseClient,
		acrClient:      acrClient,
		authClient:     authClient,
		computeClient:  computeClient,
		IPFamily:       ipFamily,
	}

	return c, nil
}

// GetResourceGroup get RG name which is same of cluster name as definite in k8s-azure
func (tc *AzureTestClient) GetResourceGroup() string {
	return tc.resourceGroup
}

// GetLocation get location which is same of cluster name as definite in k8s-azure
func (tc *AzureTestClient) GetLocation() string {
	return tc.location
}

// GetAuthConfig gets the authorization configuration information
func (tc *AzureTestClient) GetAuthConfig() AzureAuthConfig {
	return tc.authConfig
}

// CreateSubnetsClient generates subnet client with the same baseclient as azure test client
func (tc *AzureTestClient) createSubnetsClient() *aznetwork.SubnetsClient {
	return &aznetwork.SubnetsClient{BaseClient: tc.networkClient}
}

// CreateVirtualNetworksClient generates virtual network client with the same baseclient as azure test client
func (tc *AzureTestClient) createVirtualNetworksClient() *aznetwork.VirtualNetworksClient {
	return &aznetwork.VirtualNetworksClient{BaseClient: tc.networkClient}
}

// CreateSecurityGroupsClient generates security group client with the same baseclient as azure test client
func (tc *AzureTestClient) CreateSecurityGroupsClient() *aznetwork.SecurityGroupsClient {
	return &aznetwork.SecurityGroupsClient{BaseClient: tc.networkClient}
}

// createPublicIPAddressesClient generates public IP addresses client with the same baseclient as azure test client
func (tc *AzureTestClient) createPublicIPAddressesClient() *aznetwork.PublicIPAddressesClient {
	return &aznetwork.PublicIPAddressesClient{BaseClient: tc.networkClient}
}

// createPublicIPPrefixesClient generates public IP prefixes client with the same baseclient as azure test client
func (tc *AzureTestClient) createPublicIPPrefixesClient() *aznetwork.PublicIPPrefixesClient {
	return &aznetwork.PublicIPPrefixesClient{BaseClient: tc.networkClient}
}

// createLoadBalancerClient generates loadbalancer client with the same baseclient as azure test client
func (tc *AzureTestClient) createLoadBalancerClient() *aznetwork.LoadBalancersClient {
	return &aznetwork.LoadBalancersClient{BaseClient: tc.networkClient}
}

// createPrivateLinkServiceClient generates private link service client with the same baseclient as azure test client
func (tc *AzureTestClient) createPrivateLinkServiceClient() *aznetwork.PrivateLinkServicesClient {
	return &aznetwork.PrivateLinkServicesClient{BaseClient: tc.networkClient}
}

// createInterfacesClient generates network interface client with the same baseclient as azure test client
func (tc *AzureTestClient) createInterfacesClient() *aznetwork.InterfacesClient {
	return &aznetwork.InterfacesClient{BaseClient: tc.networkClient}
}

// createResourceGroupClient generates resource group client with the same baseclient as azure test client
func (tc *AzureTestClient) createResourceGroupClient() *azresources.GroupsClient {
	return &azresources.GroupsClient{BaseClient: tc.resourceClient}
}

// createACRClient generates ACR client with the same baseclient as azure test client
func (tc *AzureTestClient) createACRClient() *acr.RegistriesClient {
	return &acr.RegistriesClient{BaseClient: tc.acrClient}
}

// createRoleAssignmentsClient generates authorization client with the same baseclient as azure test client
func (tc *AzureTestClient) createRoleAssignmentsClient() *azauth.RoleAssignmentsClient {
	return &azauth.RoleAssignmentsClient{BaseClient: tc.authClient}
}

// createVMSSClient generates VMSS client with the same baseclient as azure test client
func (tc *AzureTestClient) createVMSSClient() *azcompute.VirtualMachineScaleSetsClient {
	return &azcompute.VirtualMachineScaleSetsClient{BaseClient: tc.computeClient}
}

// createVMSSVMClient generates VMSS VM client with the same baseclient as azure test client
func (tc *AzureTestClient) createVMSSVMClient() *azcompute.VirtualMachineScaleSetVMsClient {
	return &azcompute.VirtualMachineScaleSetVMsClient{BaseClient: tc.computeClient}
}

// createVMSSVMClient generates route table client with the same baseclient as azure test client
func (tc *AzureTestClient) createRouteTableClient() *aznetwork.RouteTablesClient {
	return &aznetwork.RouteTablesClient{BaseClient: tc.networkClient}
}

// createVMClient generates virtual machine client with the same baseclient as azure test client
func (tc *AzureTestClient) createVMClient() *azcompute.VirtualMachinesClient {
	return &azcompute.VirtualMachinesClient{BaseClient: tc.computeClient}
}

// getResourceGroupFromProviderID gets the resource group name in the provider ID.
func getResourceGroupFromProviderID(providerID string) (string, error) {
	matches := azureResourceGroupNameRE.FindStringSubmatch(providerID)
	if len(matches) != 2 {
		return "", fmt.Errorf("%q isn't in Azure resource ID format %q", providerID, azureResourceGroupNameRE.String())
	}

	return matches[1], nil
}

// GetNodeResourceGroup returns the resource group of the given node
func GetNodeResourceGroup(node *v1.Node) (string, error) {
	return getResourceGroupFromProviderID(node.Spec.ProviderID)
}

func getLocationFromNodeLabels(node *v1.Node) string {
	if location, ok := node.Labels[nodeLabelLocation]; ok {
		return location
	}
	return defaultLocation
}
