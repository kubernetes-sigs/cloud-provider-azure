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

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/interfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/privatelinkserviceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipprefixclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/registryclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/resourcegroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/routetableclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/securitygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/subnetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualnetworkclient"
)

var (
	azureResourceGroupNameRE = regexp.MustCompile(`.*/subscriptions/(?:.*)/resourceGroups/(.+)/providers/(?:.*)`)
	nodeLabelLocation        = "failure-domain.beta.kubernetes.io/region"
	defaultLocation          = "eastus2"
)

// AzureTestClient configs Azure specific clients
type AzureTestClient struct {
	location        string
	resourceGroup   string
	authConfig      *azclient.AzureAuthConfig
	client          azclient.ClientFactory
	azFactoryConfig *azclient.ClientFactoryConfig
	IPFamily        IPFamily
	HasWindowsNodes bool
}

// CreateAzureTestClient makes a new AzureTestClient
// Only consider PublicCloud Environment
func CreateAzureTestClient() (*AzureTestClient, error) {
	authConfig, armclientConfig, clientFactoryConfig, err := azureAuthConfigFromTestProfile()
	if err != nil {
		return nil, err
	}
	authProvider, err := azclient.NewAuthProvider(armclientConfig, authConfig)
	if err != nil {
		return nil, err
	}
	cred := authProvider.GetAzIdentity()

	azFactory, err := azclient.NewClientFactory(clientFactoryConfig, armclientConfig, cred)
	if err != nil {
		return nil, err
	}
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

	hasWindowsNodes := false
	for _, node := range nodes {
		if os, ok := node.Labels["kubernetes.io/os"]; ok && os == "windows" {
			hasWindowsNodes = true
			break
		}
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
		location:        location,
		resourceGroup:   resourceGroup,
		IPFamily:        ipFamily,
		HasWindowsNodes: hasWindowsNodes,
		authConfig:      authConfig,
		azFactoryConfig: clientFactoryConfig,
		client:          azFactory,
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
func (tc *AzureTestClient) GetSubscriptionID() string {
	return tc.azFactoryConfig.SubscriptionID
}

// CreateSubnetsClient generates subnet client with the same baseclient as azure test client
func (tc *AzureTestClient) createSubnetsClient() subnetclient.Interface {
	return tc.client.GetSubnetClient()
}

// CreateVirtualNetworksClient generates virtual network client with the same baseclient as azure test client
func (tc *AzureTestClient) createVirtualNetworksClient() virtualnetworkclient.Interface {
	return tc.client.GetVirtualNetworkClient()
}

// CreateSecurityGroupsClient generates security group client with the same baseclient as azure test client
func (tc *AzureTestClient) CreateSecurityGroupsClient() securitygroupclient.Interface {
	return tc.client.GetSecurityGroupClient()
}

// createPublicIPAddressesClient generates public IP addresses client with the same baseclient as azure test client
func (tc *AzureTestClient) createPublicIPAddressesClient() publicipaddressclient.Interface {
	return tc.client.GetPublicIPAddressClient()
}

// createPublicIPPrefixesClient generates public IP prefixes client with the same baseclient as azure test client
func (tc *AzureTestClient) createPublicIPPrefixesClient() publicipprefixclient.Interface {
	return tc.client.GetPublicIPPrefixClient()
}

// createLoadBalancerClient generates loadbalancer client with the same baseclient as azure test client
func (tc *AzureTestClient) createLoadBalancerClient() loadbalancerclient.Interface {
	return tc.client.GetLoadBalancerClient()
}

// createPrivateLinkServiceClient generates private link service client with the same baseclient as azure test client
func (tc *AzureTestClient) createPrivateLinkServiceClient() privatelinkserviceclient.Interface {
	return tc.client.GetPrivateLinkServiceClient()
}

// createInterfacesClient generates network interface client with the same baseclient as azure test client
func (tc *AzureTestClient) createInterfacesClient() interfaceclient.Interface {
	return tc.client.GetInterfaceClient()
}

// createResourceGroupClient generates resource group client with the same baseclient as azure test client
func (tc *AzureTestClient) createResourceGroupClient() resourcegroupclient.Interface {
	return tc.client.GetResourceGroupClient()
}

// createACRClient generates ACR client with the same baseclient as azure test client
func (tc *AzureTestClient) createACRClient() registryclient.Interface {
	return tc.client.GetRegistryClient()
}

// createVMSSClient generates VMSS client with the same baseclient as azure test client
func (tc *AzureTestClient) createVMSSClient() virtualmachinescalesetclient.Interface {
	return tc.client.GetVirtualMachineScaleSetClient()
}

// createVMSSVMClient generates VMSS VM client with the same baseclient as azure test client
func (tc *AzureTestClient) createVMSSVMClient() virtualmachinescalesetvmclient.Interface {
	return tc.client.GetVirtualMachineScaleSetVMClient()
}

// createVMSSVMClient generates route table client with the same baseclient as azure test client
func (tc *AzureTestClient) createRouteTableClient() routetableclient.Interface {
	return tc.client.GetRouteTableClient()
}

// createVMClient generates virtual machine client with the same baseclient as azure test client
func (tc *AzureTestClient) createVMClient() virtualmachineclient.Interface {
	return tc.client.GetVirtualMachineClient()
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
