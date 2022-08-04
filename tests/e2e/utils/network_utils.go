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
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	"github.com/Azure/go-autorest/autorest/to"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// getVirtualNetworkList returns the list of virtual networks in the cluster resource group.
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

// GetClusterVirtualNetwork returns the cluster's virtual network.
func (azureTestClient *AzureTestClient) GetClusterVirtualNetwork() (virtualNetwork aznetwork.VirtualNetwork, err error) {
	vNetList, err := azureTestClient.getVirtualNetworkList()
	if err != nil {
		return
	}

	switch len(vNetList.Values()) {
	case 0:
		err = fmt.Errorf("found no virtual network in resource group %s", azureTestClient.GetResourceGroup())
		return
	case 1:
		virtualNetwork = vNetList.Values()[0]
		return
	default:
		err = fmt.Errorf("found more than one virtual network in resource group %s", azureTestClient.GetResourceGroup())
		return
	}
}

// CreateSubnet creates a new subnet in the specified virtual network.
func (azureTestClient *AzureTestClient) CreateSubnet(vnet aznetwork.VirtualNetwork, subnetName *string, prefix *string, waitUntilComplete bool) (network.Subnet, error) {
	Logf("creating a new subnet %s, %s", *subnetName, *prefix)
	subnetParameter := (*vnet.Subnets)[0]
	subnetParameter.Name = subnetName
	subnetParameter.AddressPrefix = prefix
	subnetsClient := azureTestClient.createSubnetsClient()
	_, err := subnetsClient.CreateOrUpdate(context.Background(), azureTestClient.GetResourceGroup(), *vnet.Name, *subnetName, subnetParameter)
	var subnet network.Subnet
	if err != nil || !waitUntilComplete {
		return subnet, err
	}
	err = wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		subnet, err = subnetsClient.Get(context.Background(), azureTestClient.GetResourceGroup(), *vnet.Name, *subnetName, "")
		if err != nil {
			if !IsRetryableAPIError(err) {
				return false, err
			}
			return false, nil
		}
		return subnet.ID != nil, nil
	})
	return subnet, err
}

// DeleteSubnet deletes a subnet with retry.
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
		Logf("encountered unexpected error %v while deleting subnet %s", err, subnetName)
		return true, nil
	})
}

// GetNextSubnetCIDR obtains a new ip address which has no overlap with existing subnets.
func GetNextSubnetCIDR(vnet aznetwork.VirtualNetwork) (string, error) {
	if len(*vnet.AddressSpace.AddressPrefixes) == 0 {
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

// getSecurityGroupList returns the list of security groups in the cluster resource group.
func (azureTestClient *AzureTestClient) getSecurityGroupList() (result aznetwork.SecurityGroupListResultPage, err error) {
	Logf("Getting virtual network list")
	securityGroupsClient := azureTestClient.CreateSecurityGroupsClient()
	err = wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		result, err = securityGroupsClient.List(context.Background(), azureTestClient.GetResourceGroup())
		if err != nil {
			Logf("error when listing security groups: %w", err)
			if !IsRetryableAPIError(err) {
				return false, err
			}
			return false, nil
		}
		return true, nil
	})
	return
}

// GetClusterSecurityGroups gets the security groups of the cluster.
func (azureTestClient *AzureTestClient) GetClusterSecurityGroups() (ret []aznetwork.SecurityGroup, err error) {
	err = wait.PollImmediate(time.Second, time.Minute, func() (bool, error) {
		securityGroupsList, err := azureTestClient.getSecurityGroupList()
		if err != nil {
			return false, err
		}

		sgListLength := len(securityGroupsList.Values())
		Logf("got sg list, length = %d", sgListLength)
		if sgListLength != 0 {
			ret = securityGroupsList.Values()
			return true, nil
		}
		return false, nil
	})
	if err == wait.ErrWaitTimeout {
		err = fmt.Errorf("could not find the cluster security group in resource group %s", azureTestClient.GetResourceGroup())
	}
	return
}

// CreateLoadBalancerServiceManifest return the specific service to be created
func CreateLoadBalancerServiceManifest(name string, annotation map[string]string, labels map[string]string, namespace string, ports []v1.ServicePort) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotation,
		},
		Spec: v1.ServiceSpec{
			Selector: labels,
			Ports:    ports,
			Type:     "LoadBalancer",
		},
	}
}

// WaitCreatePIP waits to create a public ip resource in a specific resource group
func WaitCreatePIP(azureTestClient *AzureTestClient, ipName, rgName string, ipParameter aznetwork.PublicIPAddress) (aznetwork.PublicIPAddress, error) {
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

func WaitCreatePIPPrefix(
	cli *AzureTestClient,
	name, rgName string,
	parameter aznetwork.PublicIPPrefix,
) (aznetwork.PublicIPPrefix, error) {
	Logf("Creating PublicIPPrefix named %s", name)

	resourceClient := cli.createPublicIPPrefixesClient()
	_, err := resourceClient.CreateOrUpdate(context.Background(), rgName, name, parameter)
	var prefix aznetwork.PublicIPPrefix
	if err != nil {
		return prefix, err
	}
	err = wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		prefix, err = resourceClient.Get(context.Background(), rgName, name, "")
		if err != nil {
			if !IsRetryableAPIError(err) {
				return false, err
			}
			return false, nil
		}
		return prefix.IPPrefix != nil, nil
	})
	return prefix, err
}

func WaitGetPIPPrefix(
	cli *AzureTestClient,
	name string,
) (aznetwork.PublicIPPrefix, error) {
	Logf("Getting PublicIPPrefix named %s", name)

	resourceClient := cli.createPublicIPPrefixesClient()
	var (
		prefix aznetwork.PublicIPPrefix
		err    error
	)
	err = wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		prefix, err = resourceClient.Get(context.Background(), cli.GetResourceGroup(), name, "")
		if err != nil {
			if !IsRetryableAPIError(err) {
				return false, err
			}
			return false, nil
		}
		return prefix.IPPrefix != nil, nil
	})
	return prefix, err
}

// WaitGetPIPByPrefix retrieves the ONLY one PIP that created by specified prefix.
// If untilPIPCreated is true, it will retry until 1 PIP is associated to the prefix.
func WaitGetPIPByPrefix(
	cli *AzureTestClient,
	prefixName string,
	untilPIPCreated bool,
) (network.PublicIPAddress, error) {

	var pip network.PublicIPAddress

	err := wait.Poll(10*time.Second, 5*time.Minute, func() (bool, error) {
		prefix, err := WaitGetPIPPrefix(cli, prefixName)
		if err != nil || prefix.PublicIPAddresses == nil || len(*prefix.PublicIPAddresses) != 1 {
			numOfIPs := 0
			if prefix.PublicIPAddresses != nil {
				numOfIPs = len(*prefix.PublicIPAddresses)
			}
			Logf("prefix = [%s] not ready with error = [%v] and number of IP = [%d]", prefixName, err, numOfIPs)
			if !untilPIPCreated {
				return true, fmt.Errorf("get pip by prefix = [%s], err = [%v], number of IP = [%d]", prefixName, err, numOfIPs)
			}
			return false, nil
		}

		pipID := to.String((*prefix.PublicIPAddresses)[0].ID)
		parts := strings.Split(pipID, "/")
		pipName := parts[len(parts)-1]
		pip, err = WaitGetPIP(cli, pipName)

		return true, err
	})

	return pip, err
}

func DeletePIPPrefixWithRetry(cli *AzureTestClient, name string) error {
	Logf("Deleting PublicIPPrefix named %s", name)

	resourceClient := cli.createPublicIPPrefixesClient()

	err := wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		_, err := resourceClient.Delete(context.Background(), cli.GetResourceGroup(), name)
		if err != nil {
			Logf("error: %s, will retry soon", err)
			return false, nil
		}
		return true, nil
	})
	return err
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
func WaitGetPIP(azureTestClient *AzureTestClient, ipName string) (pip aznetwork.PublicIPAddress, err error) {
	pipClient := azureTestClient.createPublicIPAddressesClient()
	err = wait.PollImmediate(poll, singleCallTimeout, func() (bool, error) {
		pip, err = pipClient.Get(context.Background(), azureTestClient.GetResourceGroup(), ipName, "")
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
	if len(*vNet.Subnets) > 1 {
		for _, sn := range *vNet.Subnets {
			// if there is more than one subnet, select the first one we find.
			if !strings.Contains(*sn.Name, "controlplane") && !strings.Contains(*sn.Name, "control-plane") {
				subnet = *sn.AddressPrefix
				break
			}
		}
	}
	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("failed to parse subnet CIDR in vNet %s: %w", to.String(vNet.Name), err)
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

// ListLoadBalancers lists all the load balancers active
func (azureTestClient *AzureTestClient) ListLoadBalancers(resourceGroupName string) ([]aznetwork.LoadBalancer, error) {
	lbClient := azureTestClient.createLoadBalancerClient()

	iterator, err := lbClient.ListComplete(context.Background(), resourceGroupName)
	if err != nil {
		return nil, err
	}

	result := make([]aznetwork.LoadBalancer, 0)
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
	lbClient := azureTestClient.createLoadBalancerClient()
	return lbClient.Get(context.Background(), resourceGroupName, lbName, "")
}

// GetPrivateLinkService gets aznetwork.PrivateLinkService by privateLinkService name.
func (azureTestClient *AzureTestClient) GetPrivateLinkService(resourceGroupName, plsName string) (aznetwork.PrivateLinkService, error) {
	plsClient := azureTestClient.createPrivateLinkServiceClient()
	return plsClient.Get(context.Background(), resourceGroupName, plsName, "")
}

// ListPrivateLinkServices lists all the private link services active
func (azureTestClient *AzureTestClient) ListPrivateLinkServices(resourceGroupName string) ([]aznetwork.PrivateLinkService, error) {
	plsClient := azureTestClient.createPrivateLinkServiceClient()

	iterator, err := plsClient.ListComplete(context.Background(), resourceGroupName)
	if err != nil {
		return nil, err
	}

	result := make([]aznetwork.PrivateLinkService, 0)
	for ; iterator.NotDone(); err = iterator.Next() {
		if err != nil {
			return nil, err
		}

		result = append(result, iterator.Value())
	}

	return result, nil
}
