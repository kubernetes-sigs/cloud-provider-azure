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

	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-07-01/network"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
)

var (
	azureResourceGroupNameRE = regexp.MustCompile(`.*/subscriptions/(?:.*)/resourceGroups/(.+)/providers/(?:.*)`)
)

// AzureTestClient configs Azure specific clients
type AzureTestClient struct {
	resourceGroup string
	networkClient aznetwork.BaseClient
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

	c := &AzureTestClient{
		resourceGroup: resourceGroup,
		networkClient: baseClient,
	}

	return c, nil
}

// getResourceGroup get RG name which is same of cluster name as definite in k8s-azure
func (tc *AzureTestClient) getResourceGroup() string {
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

// getResourceGroupFromProviderID gets the resource group name in the provider ID.
func getResourceGroupFromProviderID(providerID string) (string, error) {
	matches := azureResourceGroupNameRE.FindStringSubmatch(providerID)
	if len(matches) != 2 {
		return "", fmt.Errorf("%q isn't in Azure resource ID format %q", providerID, azureResourceGroupNameRE.String())
	}

	return matches[1], nil
}
