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
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/pointer"
)

type IPFamily string

var (
	IPv4      IPFamily = "IPv4"
	IPv6      IPFamily = "IPv6"
	DualStack IPFamily = "DualStack"

	Suffixes = map[bool]string{
		false: "",
		true:  "-IPv6",
	}

	UnwantedTagKeys = []string{"DateCreated"}
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
			Logf("error when listing virtual network list: %w", err)
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
func (azureTestClient *AzureTestClient) CreateSubnet(vnet aznetwork.VirtualNetwork, subnetName *string, prefixes *[]string, waitUntilComplete bool) (network.Subnet, error) {
	Logf("creating a new subnet %s, %v", *subnetName, *prefixes)
	subnetParameter := (*vnet.Subnets)[0]
	subnetParameter.Name = subnetName
	if len(*prefixes) == 1 {
		subnetParameter.AddressPrefix = &(*prefixes)[0]
	} else {
		subnetParameter.AddressPrefixes = prefixes
	}
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
			Logf("unexpected error %q while deleting subnet %s", err.Error(), subnetName)
			return false, nil
		}

		subnet, err := subnetClient.Get(
			context.Background(),
			azureTestClient.GetResourceGroup(),
			vnetName,
			subnetName,
			"")
		if err == nil {
			ipConfigIDs := []string{}
			if subnet.IPConfigurations != nil {
				for _, ipConfig := range *subnet.IPConfigurations {
					ipConfigIDs = append(ipConfigIDs, pointer.StringDeref(ipConfig.ID, ""))
				}
			}

			Logf("subnet %s still exists with IP config IDs %q, will retry", subnetName, ipConfigIDs)
			return false, nil
		} else if strings.Contains(err.Error(), "StatusCode=404") {
			Logf("subnet %s has been deleted", subnetName)
			return true, nil
		}
		Logf("encountered unexpected error %w while getting subnet %s", err, subnetName)
		return true, nil
	})
}

// GetNextSubnetCIDRs obtains a new ip address which has no overlap with existing subnets.
func GetNextSubnetCIDRs(vnet aznetwork.VirtualNetwork, ipFamily IPFamily) ([]*net.IPNet, error) {
	if len(*vnet.AddressSpace.AddressPrefixes) == 0 {
		return nil, fmt.Errorf("vNet has no prefix")
	}
	// Because of Azure vNet limitation, underlying vNet is dual-stack for
	// those single stack IPv6 clusters. Pods and Services are single stack
	// IPv6 while Nodes and routes are dual-stack.
	vnetCIDRs := []string{}
	if ipFamily == IPv6 {
		for i := range *vnet.AddressSpace.AddressPrefixes {
			addrPrefix := (*vnet.AddressSpace.AddressPrefixes)[i]
			ip, _, err := net.ParseCIDR(addrPrefix)
			if err != nil {
				return nil, fmt.Errorf("failed to parse address prefix CIDR: %w", err)
			}
			if ip.To4() == nil {
				vnetCIDRs = append(vnetCIDRs, addrPrefix)
				break
			}
		}
	} else if ipFamily == IPv4 {
		vnetCIDRs = append(vnetCIDRs, (*vnet.AddressSpace.AddressPrefixes)[0])
	} else {
		vnetCIDRs = append(vnetCIDRs, *vnet.AddressSpace.AddressPrefixes...)
	}

	var existSubnets []string
	for _, subnet := range *vnet.Subnets {
		if subnet.AddressPrefix != nil {
			existSubnets = append(existSubnets, *subnet.AddressPrefix)
			continue
		}
		if subnet.AddressPrefixes == nil {
			return nil, fmt.Errorf("subnet AddressPrefix and AddressPrefixes shouldn't be both nil")
		}
		existSubnets = append(existSubnets, *subnet.AddressPrefixes...)
	}
	var nextSubnets []*net.IPNet
	for _, vnetCIDR := range vnetCIDRs {
		nextSubnet, err := getNextSubnet(vnetCIDR, existSubnets)
		if err != nil {
			return nextSubnets, err
		}
		nextSubnets = append(nextSubnets, nextSubnet)
	}
	return nextSubnets, nil
}

// isCIDRIPv6 checks if the provided CIDR is an IPv6 one.
func isCIDRIPv6(cidr string) (bool, error) {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return false, fmt.Errorf("failed to parse CIDR: %q", cidr)
	}
	return ip.To4() == nil, nil
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
	if errors.Is(err, wait.ErrWaitTimeout) {
		err = fmt.Errorf("could not find the cluster security group in resource group %s", azureTestClient.GetResourceGroup())
	}
	return
}

// CreateLoadBalancerServiceManifest return the specific service to be created
func CreateLoadBalancerServiceManifest(name string, annotation map[string]string, labels map[string]string, namespace string, ports []v1.ServicePort) *v1.Service {
	ipFamilyPreferDS := v1.IPFamilyPolicyPreferDualStack
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotation,
		},
		Spec: v1.ServiceSpec{
			Selector:       labels,
			Ports:          ports,
			Type:           "LoadBalancer",
			IPFamilyPolicy: &ipFamilyPreferDS, // TODO: This should be updated if there're single stack Service tests in dual-stack setup.
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
	if err == nil {
		pip.Tags = cleanupTags(pip.Tags, UnwantedTagKeys)
	}
	return pip, err
}

func cleanupTags(tags map[string]*string, unwantedKeys []string) map[string]*string {
	for _, key := range unwantedKeys {
		delete(tags, key)
	}
	return tags
}

func WaitCreatePIPPrefix(
	cli *AzureTestClient,
	name, rgName string,
	parameter aznetwork.PublicIPPrefix,
) (aznetwork.PublicIPPrefix, error) {
	Logf("Creating PublicIPPrefix named %s", name)

	var prefix aznetwork.PublicIPPrefix

	resourceClient := cli.createPublicIPPrefixesClient()
	_, err := resourceClient.CreateOrUpdate(context.Background(), rgName, name, parameter)
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
				return true, fmt.Errorf("get pip by prefix = [%s], err = [%w], number of IP = [%d]", prefixName, err, numOfIPs)
			}
			return false, nil
		}

		pipID := pointer.StringDeref((*prefix.PublicIPAddresses)[0].ID, "")
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
	if ipName == "" {
		return nil
	}
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
	if err == nil {
		pip.Tags = cleanupTags(pip.Tags, UnwantedTagKeys)
	}
	return
}

func selectSubnets(ipFamily IPFamily, vNetSubnets *[]aznetwork.Subnet) ([]string, error) {
	subnets := []string{}
	for _, sn := range *vNetSubnets {
		// if there is more than one subnet (non-control-plane), select the first one we find.
		if strings.Contains(*sn.Name, "controlplane") || strings.Contains(*sn.Name, "control-plane") {
			continue
		}
		if ipFamily == DualStack {
			if len(*sn.AddressPrefixes) < 2 {
				return []string{}, fmt.Errorf("failed to find correct sn.AddressPrefixes %q when IP family is dualstack", *sn.AddressPrefixes)
			}
			subnets = []string{(*sn.AddressPrefixes)[0], (*sn.AddressPrefixes)[1]}
		} else if ipFamily == IPv4 {
			subnets = []string{*sn.AddressPrefix}
		} else {
			for i := range *sn.AddressPrefixes {
				addrPrefix := (*sn.AddressPrefixes)[i]
				isIPv6, err := isCIDRIPv6(addrPrefix)
				if err != nil {
					return []string{}, err
				}
				if isIPv6 {
					subnets = []string{addrPrefix}
					break
				}
			}
		}
		break
	}
	return subnets, nil
}

func findIPInSubnet(vNetClient *aznetwork.VirtualNetworksClient, rg, subnet, vNetName string) (string, error) {
	Logf("Handling subnet %s", subnet)
	ip, _, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("failed to parse subnet CIDR in vNet %s: %w", vNetName, err)
	}

	baseIP := ip.To4()
	pos := 3
	if ip.To4() == nil {
		baseIP = ip.To16()
		pos = 15
	}
	for i := 0; i <= 254; i++ {
		baseIP[pos]++
		IP := baseIP.String()
		ret, err := vNetClient.CheckIPAddressAvailability(context.Background(), rg, vNetName, IP)
		if err != nil {
			// just ignore
			continue
		}
		if ret.Available != nil && *ret.Available {
			return IP, nil
		}
	}
	return "", fmt.Errorf("find no availabePrivateIP in subnet CIDR %s", subnet)
}

// SelectAvailablePrivateIPs selects private IP addresses in Azure subnet.
func SelectAvailablePrivateIPs(tc *AzureTestClient) ([]string, error) {
	vNet, err := tc.GetClusterVirtualNetwork()
	if err != nil {
		return []string{}, err
	}
	if len(*vNet.Subnets) == 0 {
		return []string{}, fmt.Errorf("failed to find a subnet in vNet %s", pointer.StringDeref(vNet.Name, ""))
	}
	subnets, err := selectSubnets(tc.IPFamily, vNet.Subnets)
	if err != nil {
		return []string{}, err
	}

	vNetClient := tc.createVirtualNetworksClient()
	if err != nil {
		return []string{}, err
	}
	privateIPs := []string{}
	for _, subnet := range subnets {
		ip, err := findIPInSubnet(vNetClient, tc.resourceGroup, subnet, pointer.StringDeref(vNet.Name, ""))
		if err != nil {
			return privateIPs, err
		}
		privateIPs = append(privateIPs, ip)
	}

	expectedPrivateIPCount := 1
	if tc.IPFamily == DualStack {
		expectedPrivateIPCount = 2
	}
	if len(privateIPs) != expectedPrivateIPCount {
		return []string{}, fmt.Errorf("failed to find all availabePrivateIPs in subnet CIDRs %q, got privateIPs %q", subnets, privateIPs)
	}
	return privateIPs, nil
}

// GetPublicIPFromAddress finds public ip according to ip address
func (azureTestClient *AzureTestClient) GetPublicIPFromAddress(resourceGroupName, ipAddr string) (pip aznetwork.PublicIPAddress, err error) {
	pipList, err := azureTestClient.ListPublicIPs(resourceGroupName)
	if err != nil {
		return pip, err
	}
	for _, pip := range pipList {
		if strings.EqualFold(pointer.StringDeref(pip.IPAddress, ""), ipAddr) {
			return pip, err
		}
	}
	return
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

func retrieveCIDRs(cmd string, reg string) ([]string, error) {
	res := make([]string, 2)
	stdout, err := RunKubectlNoPrint("", strings.Split(cmd, " ")...)
	if err != nil {
		return res, fmt.Errorf("error when running the following kubectl command %q: %w, %s", cmd, err, stdout)
	}
	re := regexp.MustCompile(reg)
	matches := re.FindStringSubmatch(stdout)
	if len(matches) == 0 {
		return res, fmt.Errorf("cannot retrieve CIDR, unexpected kubectl output: %s", stdout)
	}
	cidrs := strings.Split(matches[1], ",")
	if len(cidrs) == 1 {
		_, cidr, err := net.ParseCIDR(cidrs[0])
		if err != nil {
			return res, fmt.Errorf("CIDR cannot be parsed: %s", cidrs[0])
		}
		if cidr.IP.To4() != nil {
			res[0] = cidrs[0]
		} else {
			res[1] = cidrs[0]
		}
	} else if len(cidrs) == 2 {
		_, cidr, err := net.ParseCIDR(cidrs[0])
		if err != nil {
			return res, fmt.Errorf("CIDR cannot be parsed: %s", cidrs[0])
		}
		if cidr.IP.To4() != nil {
			res[0] = cidrs[0]
			res[1] = cidrs[1]
		} else {
			res[0] = cidrs[1]
			res[1] = cidrs[0]
		}
	} else {
		return res, fmt.Errorf("unexpected cluster CIDR: %s", matches[1])
	}
	return res, nil
}

// GetClusterServiceIPFamily gets cluster's Service IPFamily according to Service CIDRs.
func GetClusterServiceIPFamily() (IPFamily, error) {
	// Only test IPv4 in AKS pipeline
	if os.Getenv(AKSTestCCM) != "" {
		Logf("Cluster IP family for AKS pipeline: IPv4")
		return IPv4, nil
	}

	svcCIDRs, err := retrieveCIDRs("cluster-info dump | grep service-cluster-ip-range", `service-cluster-ip-range=([^"]+)`)
	if err != nil {
		return "", err
	}
	ipFamily := DualStack
	if svcCIDRs[0] != "" && svcCIDRs[1] == "" {
		ipFamily = IPv4
	}
	if svcCIDRs[0] == "" && svcCIDRs[1] != "" {
		ipFamily = IPv6
	}
	Logf("Cluster IP family: %s", ipFamily)
	return ipFamily, nil
}

func IfIPFamiliesEnabled(ipFamily IPFamily) (v4Enabled bool, v6Enabled bool) {
	if ipFamily == DualStack || ipFamily == IPv4 {
		v4Enabled = true
	}
	if ipFamily == DualStack || ipFamily == IPv6 {
		v6Enabled = true
	}
	return
}

// GetNameWithSuffix returns resource name with IP family suffix.
func GetNameWithSuffix(name, suffix string) string {
	return name + suffix
}
