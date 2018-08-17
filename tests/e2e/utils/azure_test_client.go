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
	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2017-09-01/network"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
)

// AzureTestClient configs Azure specific clients
type AzureTestClient struct {
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

	c := &AzureTestClient{
		networkClient: baseClient,
	}

	return c, nil
}

// getResourceGroup get RG name which is same of cluster name as definited in k8s-azure
func getResourceGroup() string {
	return ExtractDNSPrefix()
}

// CreateSubnetsClient generates subnet client with the same baseclient as azure test client
func (tc *AzureTestClient) createSubnetsClient() *aznetwork.SubnetsClient {
	return &aznetwork.SubnetsClient{BaseClient: tc.networkClient}
}

// CreateVirtualNetworksClient generates virtual network client with the same baseclient as azure test client
func (tc *AzureTestClient) createVirtualNetworksClient() *aznetwork.VirtualNetworksClient {
	return &aznetwork.VirtualNetworksClient{BaseClient: tc.networkClient}
}
