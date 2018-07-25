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

	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2017-09-01/network"
	"k8s.io/apimachinery/pkg/util/wait"
)

// getVirtualNetworkList is a wapper around listing VirtualNetwork
func (azureTestClient *AzureTestClient) getVirtualNetworkList() (result aznetwork.VirtualNetworkListResultPage, err error) {
	Logf("Getting virtural network list")
	vNetClient := azureTestClient.createVirtualNetworksClient()
	err = wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		result, err = vNetClient.List(context.Background(), getResourceGroup())
		if err != nil {
			if !IsRetryableAPIError(err) {
				return false, err
			}
			return false, nil
		}
		return true, nil
	})
	return
}

// GetClusterVirtualNetwork gets the only vnet of the cluster
func (azureTestClient *AzureTestClient) GetClusterVirtualNetwork() (ret aznetwork.VirtualNetwork, err error) {
	vNetList, err := azureTestClient.getVirtualNetworkList()
	if err != nil {
		return
	}
	// Assume there is only one cluster in one resource group
	if len(vNetList.Values()) != 1 {
		err = fmt.Errorf("Found no or more than 1 virtual network in resource group same as cluster name")
		return
	}
	ret = vNetList.Values()[0]
	return
}

// CreateSubnet will create a new subnet in certain virtual network
func (azureTestClient *AzureTestClient) CreateSubnet(vnet aznetwork.VirtualNetwork, subnetName *string, prefix *string) error {
	Logf("creating a new subnet %s, %s", *subnetName, *prefix)
	subnetParameter := (*vnet.Subnets)[0]
	subnetParameter.Name = subnetName
	subnetParameter.AddressPrefix = prefix
	subnetsClient := azureTestClient.createSubnetsClient()
	_, err := subnetsClient.CreateOrUpdate(context.Background(), getResourceGroup(), *vnet.Name, *subnetName, subnetParameter)
	return err
}

// DeleteSubnet delete a subnet with retry
func (azureTestClient *AzureTestClient) DeleteSubnet(vnetName string, subnetName string) error {
	subnetClient := azureTestClient.createSubnetsClient()
	return wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		_, err := subnetClient.Delete(context.Background(), getResourceGroup(), vnetName, subnetName)
		if err != nil {
			return false, nil
		}
		return true, nil
	})
}

// GetNextSubnetCIDR obatins a new ip address which has no overlapping with other subnet
func GetNextSubnetCIDR(vnet aznetwork.VirtualNetwork) (string, error) {
	if len((*vnet.AddressSpace.AddressPrefixes)) == 0 {
		return "", fmt.Errorf("vNet has no prefix")
	}
	vnetCIDR := (*vnet.AddressSpace.AddressPrefixes)[0]
	var existSubnets []string
	for _, subnet := range *vnet.Subnets {
		subnet := *subnet.AddressPrefix
		existSubnets = append(existSubnets, subnet)
	}
	return getNextSubnet(vnetCIDR, existSubnets)
}
