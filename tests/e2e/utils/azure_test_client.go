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

	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-07-01/network"
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
	networkClient  aznetwork.BaseClient
	resourceClient azresources.BaseClient
}

// CreateAzureTestClient makes a new AzureTestClient
// Only consider PublicCloud Environment
func CreateAzureTestClient() (*AzureTestClient, error) {
	authconfig, err := azureAuthConfigFromTestProfile()
	if err != nil {
		return nil, err
	}
	servicePrincipleToken, err := getServicePrincipalToken(authconfig)
	if err != nil {
		return nil, err
	}
	baseClient := aznetwork.NewWithBaseURI(azure.PublicCloud.TokenAudience, authconfig.SubscriptionID)
	baseClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipleToken)

	resourceBaseClient := azresources.BaseClient(baseClient)
	resourceBaseClient.Authorizer = autorest.NewBearerAuthorizer(servicePrincipleToken)

	kubeClient, err := CreateKubeClientSet()
	if err != nil {
		return nil, err
	}

	nodes, err := GetAgentNodes(kubeClient)
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

	c := &AzureTestClient{
		location:       location,
		resourceGroup:  resourceGroup,
		networkClient:  baseClient,
		resourceClient: resourceBaseClient,
	}

	return c, nil
}

// GetResourceGroup get RG name which is same of cluster name as definite in k8s-azure
func (tc *AzureTestClient) GetResourceGroup() string {
	return tc.resourceGroup
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

// createPublicIPAddressesClient generates virtual network client with the same baseclient as azure test client
func (tc *AzureTestClient) createPublicIPAddressesClient() *aznetwork.PublicIPAddressesClient {
	return &aznetwork.PublicIPAddressesClient{BaseClient: tc.networkClient}
}

// creteLoadBalancerClient generates loadbalancer client with the same baseclient as azure test client
func (tc *AzureTestClient) creteLoadBalancerClient() *aznetwork.LoadBalancersClient {
	return &aznetwork.LoadBalancersClient{BaseClient: tc.networkClient}
}

// createResourceGroupClient generates resource group client with the same baseclient as azure test client
func (tc *AzureTestClient) createResourceGroupClient() *azresources.GroupsClient {
	return &azresources.GroupsClient{BaseClient: tc.resourceClient}
}

// getResourceGroupFromProviderID gets the resource group name in the provider ID.
func getResourceGroupFromProviderID(providerID string) (string, error) {
	matches := azureResourceGroupNameRE.FindStringSubmatch(providerID)
	if len(matches) != 2 {
		return "", fmt.Errorf("%q isn't in Azure resource ID format %q", providerID, azureResourceGroupNameRE.String())
	}

	return matches[1], nil
}

func getLocationFromNodeLabels(node *v1.Node) string {
	if location, ok := node.Labels[nodeLabelLocation]; ok {
		return location
	}
	return defaultLocation
}
