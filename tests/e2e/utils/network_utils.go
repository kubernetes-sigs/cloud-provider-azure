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
	"net"
	"strings"

	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-07-01/network"
	"github.com/Azure/go-autorest/autorest/to"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const k8sVNetPrefix = "k8s-vnet-"

// getVirtualNetworkList is a wapper around listing VirtualNetwork
func (azureTestClient *AzureTestClient) getVirtualNetworkList() (result aznetwork.VirtualNetworkListResultPage, err error) {
	Logf("Getting virtual network list")
	vNetClient := azureTestClient.createVirtualNetworksClient()
	err = wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		result, err = vNetClient.List(context.Background(), azureTestClient.GetResourceGroup())
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
func (azureTestClient *AzureTestClient) GetClusterVirtualNetwork() (virtualNetwork aznetwork.VirtualNetwork, err error) {
	vNetList, err := azureTestClient.getVirtualNetworkList()
	if err != nil {
		return
	}

	k8sVNetCount := 0
	returnIndex := 0
	for i, vNet := range vNetList.Values() {
		if strings.HasPrefix(to.String(vNet.Name), k8sVNetPrefix) {
			Logf("found one k8s virtual network %s", to.String(vNet.Name))
			k8sVNetCount++
			returnIndex = i
		} else {
			Logf("found other virtual network %s, skip", to.String(vNet.Name))
		}
	}
	switch k8sVNetCount {
	case 0:
		err = fmt.Errorf("Found no virtual network of the corresponding cluster in resource group")
		return
	case 1:
		virtualNetwork = vNetList.Values()[returnIndex]
		return
	default:
		err = fmt.Errorf("Found more than one virtual network of the corresponding cluster in resource group")
		return
	}
}

// CreateSubnet will create a new subnet in certain virtual network
func (azureTestClient *AzureTestClient) CreateSubnet(vnet aznetwork.VirtualNetwork, subnetName *string, prefix *string) error {
	Logf("creating a new subnet %s, %s", *subnetName, *prefix)
	subnetParameter := (*vnet.Subnets)[0]
	subnetParameter.Name = subnetName
	subnetParameter.AddressPrefix = prefix
	subnetsClient := azureTestClient.createSubnetsClient()
	_, err := subnetsClient.CreateOrUpdate(context.Background(), azureTestClient.GetResourceGroup(), *vnet.Name, *subnetName, subnetParameter)
	return err
}

// DeleteSubnet delete a subnet with retry
func (azureTestClient *AzureTestClient) DeleteSubnet(vnetName string, subnetName string) error {
	subnetClient := azureTestClient.createSubnetsClient()
	return wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		_, err := subnetClient.Delete(context.Background(), azureTestClient.GetResourceGroup(), vnetName, subnetName)
		if err != nil {
			return false, nil
		}

		_, err = subnetClient.Get(
			context.Background(),
			azureTestClient.GetResourceGroup(),
			vnetName,
			subnetName,
			"")
		if err == nil {
			Logf("subnet %s still exists, will retry", subnetName)
			return false, nil
		} else if strings.Contains(err.Error(), "StatusCode=404") {
			Logf("subnet %s has been deleted", subnetName)
			return true, nil
		}
		Logf("encountered unexpected error %v during deleting subnet %s", err, subnetName)
		return true, nil
	})
}

// GetNextSubnetCIDR obtains a new ip address which has no overlapping with other subnet
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

// getSecurityGroupList is a wrapper around listing VirtualNetwork
func (azureTestClient *AzureTestClient) getSecurityGroupList() (result aznetwork.SecurityGroupListResultPage, err error) {
	Logf("Getting virtual network list")
	securityGroupsClient := azureTestClient.CreateSecurityGroupsClient()
	err = wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		result, err = securityGroupsClient.List(context.Background(), azureTestClient.GetResourceGroup())
		if err != nil {
			Logf("error when listing sgs: %v", err)
			if !IsRetryableAPIError(err) {
				return false, err
			}
			return false, nil
		}
		return true, nil
	})
	return
}

// GetClusterSecurityGroup gets the only vnet of the cluster
func (azureTestClient *AzureTestClient) GetClusterSecurityGroup() (ret *aznetwork.SecurityGroup, err error) {
	securityGroupsList, err := azureTestClient.getSecurityGroupList()
	if err != nil {
		return
	}
	Logf("got sg list, length = %d", len(securityGroupsList.Values()))

	// Assume there is only one cluster in one resource group
	if len(securityGroupsList.Values()) != 1 {
		err = fmt.Errorf("Found no or more than 1 virtual network in resource group same as cluster name")
		return
	}
	ret = &securityGroupsList.Values()[0]
	return
}

// CreateLoadBalancerServiceManifest return the specific service to be created
func CreateLoadBalancerServiceManifest(name string, annotation map[string]string, labels map[string]string, namespace string, ports []v1.ServicePort) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotation,
		},
		Spec: v1.ServiceSpec{
			Selector: labels,
			Ports:    ports,
			Type:     "LoadBalancer",
		},
	}
}

// WaitCreatePIP waits to create a public ip resource
func WaitCreatePIP(azureTestClient *AzureTestClient, ipName string, ipParameter aznetwork.PublicIPAddress) (aznetwork.PublicIPAddress, error) {
	Logf("Creating public IP resource named %s", ipName)
	pipClient := azureTestClient.createPublicIPAddressesClient()
	_, err := pipClient.CreateOrUpdate(context.Background(), azureTestClient.GetResourceGroup(), ipName, ipParameter)
	var pip aznetwork.PublicIPAddress
	if err != nil {
		return pip, err
	}
	err = wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		pip, err = pipClient.Get(context.Background(), azureTestClient.GetResourceGroup(), ipName, "")
		if err != nil {
			if !IsRetryableAPIError(err) {
				return false, err
			}
			return false, nil
		}
		return pip.IPAddress != nil, nil
	})
	return pip, err
}

// WaitCreateNewPIP waits to create a public ip resource in a specific resource group
func WaitCreateNewPIP(azureTestClient *AzureTestClient, ipName, rgName string, ipParameter aznetwork.PublicIPAddress) (aznetwork.PublicIPAddress, error) {
	Logf("Creating public IP resource named %s", ipName)
	pipClient := azureTestClient.createPublicIPAddressesClient()
	_, err := pipClient.CreateOrUpdate(context.Background(), rgName, ipName, ipParameter)
	var pip aznetwork.PublicIPAddress
	if err != nil {
		return pip, err
	}
	err = wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		pip, err = pipClient.Get(context.Background(), rgName, ipName, "")
		if err != nil {
			if !IsRetryableAPIError(err) {
				return false, err
			}
			return false, nil
		}
		return pip.IPAddress != nil, nil
	})
	return pip, err
}

// DeletePIPWithRetry tries to delete a public ip resource
func DeletePIPWithRetry(azureTestClient *AzureTestClient, ipName, rgName string) error {
	if rgName == "" {
		rgName = azureTestClient.GetResourceGroup()
	}
	Logf("Deleting public IP resource named %s in resource group %s", ipName, rgName)
	pipClient := azureTestClient.createPublicIPAddressesClient()
	err := wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		_, err := pipClient.Delete(context.Background(), rgName, ipName)
		if err != nil {
			Logf("error: %v, will retry soon", err)
			return false, nil
		}
		return true, nil
	})
	return err
}

// WaitGetPIP waits to get a specific public ip resource
func WaitGetPIP(azureTestClient *AzureTestClient, ipName string) (err error) {
	pipClient := azureTestClient.createPublicIPAddressesClient()
	err = wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		_, err = pipClient.Get(context.Background(), azureTestClient.GetResourceGroup(), ipName, "")
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

// SelectAvailablePrivateIP selects a private IP address in Azure subnet.
func SelectAvailablePrivateIP(tc *AzureTestClient) (string, error) {
	vNet, err := tc.GetClusterVirtualNetwork()
	vNetClient := tc.createVirtualNetworksClient()
	if err != nil {
		return "", err
	}
	if vNet.Subnets == nil || len(*vNet.Subnets) == 0 {
		return "", fmt.Errorf("failed to find a subnet in vNet %s", to.String(vNet.Name))
	}
	subnet := to.String((*vNet.Subnets)[0].AddressPrefix)
	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("failed to parse subnet CIDR in vNet %s: %v", to.String(vNet.Name), err)
	}
	baseIP := ip.To4()
	for i := 0; i <= 254; i++ {
		baseIP[3]++
		IP := baseIP.String()
		ret, err := vNetClient.CheckIPAddressAvailability(context.Background(), tc.GetResourceGroup(), to.String(vNet.Name), IP)
		if err != nil {
			// just ignore
			continue
		}
		if ret.Available != nil && *ret.Available {
			return IP, nil
		}
	}
	return "", fmt.Errorf("Find no availabePrivateIP in subnet CIDR %s", subnet)
}

// ListPublicIPs lists all the publicIP addresses active
func (azureTestClient *AzureTestClient) ListPublicIPs(resourceGroupName string) ([]aznetwork.PublicIPAddress, error) {
	pipClient := azureTestClient.createPublicIPAddressesClient()

	iterator, err := pipClient.ListComplete(context.Background(), resourceGroupName)
	if err != nil {
		return nil, err
	}

	result := make([]aznetwork.PublicIPAddress, 0)
	for ; iterator.NotDone(); err = iterator.Next() {
		if err != nil {
			return nil, err
		}

		result = append(result, iterator.Value())
	}

	return result, nil
}

// GetLoadBalancer gets aznetwork.LoadBalancer by loadBalancer name.
func (azureTestClient *AzureTestClient) GetLoadBalancer(resourceGroupName, lbName string) (aznetwork.LoadBalancer, error) {
	lbClient := azureTestClient.creteLoadBalancerClient()
	return lbClient.Get(context.Background(), resourceGroupName, lbName, "")
}
