/*
Copyright 2022 The Kubernetes Authors.

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

package provider

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
)

var (
	// ErrorVmssIDIsEmpty indicates the vmss id is empty.
	ErrorVmssIDIsEmpty = errors.New("VMSS ID is empty")
)

// FlexScaleSet implements VMSet interface for Azure Flexible VMSS.
type FlexScaleSet struct {
	*Cloud

	vmssFlexCache *azcache.TimedCache

	vmssFlexVMNameToVmssID   *sync.Map
	vmssFlexVMNameToNodeName *sync.Map
	vmssFlexVMCache          *azcache.TimedCache

	// lockMap in cache refresh
	lockMap *lockMap
}

func newFlexScaleSet(az *Cloud) (VMSet, error) {
	fs := &FlexScaleSet{
		Cloud:                    az,
		vmssFlexVMNameToVmssID:   &sync.Map{},
		vmssFlexVMNameToNodeName: &sync.Map{},
		lockMap:                  newLockMap(),
	}

	var err error
	fs.vmssFlexCache, err = fs.newVmssFlexCache()
	if err != nil {
		return nil, err
	}
	fs.vmssFlexVMCache, err = fs.newVmssFlexVMCache()
	if err != nil {
		return nil, err
	}

	return fs, nil
}

// GetPrimaryVMSetName returns the VM set name depending on the configured vmType.
// It returns config.PrimaryScaleSetName for vmss and config.PrimaryAvailabilitySetName for standard vmType.
func (fs *FlexScaleSet) GetPrimaryVMSetName() string {
	return fs.Config.PrimaryScaleSetName
}

// getNodeVMSetName returns the vmss flex name by the node name.
func (fs *FlexScaleSet) getNodeVmssFlexName(nodeName string) (string, error) {
	vmssFlexID, err := fs.getNodeVmssFlexID(nodeName)
	if err != nil {
		return "", err
	}
	vmssFlexName, err := getLastSegment(vmssFlexID, "/")
	if err != nil {
		return "", err
	}
	return vmssFlexName, nil

}

// GetNodeVMSetName returns the availability set or vmss name by the node name.
// It will return empty string when using standalone vms.
func (fs *FlexScaleSet) GetNodeVMSetName(node *v1.Node) (string, error) {
	return fs.getNodeVmssFlexName(node.Name)
}

// GetAgentPoolVMSetNames returns all vmSet names according to the nodes
func (fs *FlexScaleSet) GetAgentPoolVMSetNames(nodes []*v1.Node) (*[]string, error) {
	vmSetNames := make([]string, 0)
	for _, node := range nodes {
		vmSetName, err := fs.GetNodeVMSetName(node)
		if err != nil {
			klog.Errorf("Unable to get the vmss flex name by node name %s: %v", node.Name, err)
			continue
		}
		vmSetNames = append(vmSetNames, vmSetName)
	}
	return &vmSetNames, nil
}

// GetVMSetNames selects all possible availability sets or scale sets
// (depending vmType configured) for service load balancer, if the service has
// no loadbalancer mode annotation returns the primary VMSet. If service annotation
// for loadbalancer exists then returns the eligible VMSet. The mode selection
// annotation would be ignored when using one SLB per cluster.
func (fs *FlexScaleSet) GetVMSetNames(service *v1.Service, nodes []*v1.Node) (*[]string, error) {
	hasMode, isAuto, serviceVMSetName := fs.getServiceLoadBalancerMode(service)
	useSingleSLB := fs.useStandardLoadBalancer() && !fs.EnableMultipleStandardLoadBalancers
	if !hasMode || useSingleSLB {
		// no mode specified in service annotation or use single SLB mode
		// default to PrimaryScaleSetName
		vmssFlexNames := &[]string{fs.Config.PrimaryScaleSetName}
		return vmssFlexNames, nil
	}

	vmssFlexNames, err := fs.GetAgentPoolVMSetNames(nodes)
	if err != nil {
		klog.Errorf("fs.GetVMSetNames - GetAgentPoolVMSetNames failed err=(%v)", err)
		return nil, err
	}

	if !isAuto {
		found := false
		for asx := range *vmssFlexNames {
			if strings.EqualFold((*vmssFlexNames)[asx], serviceVMSetName) {
				found = true
				serviceVMSetName = (*vmssFlexNames)[asx]
				break
			}
		}
		if !found {
			klog.Errorf("fs.GetVMSetNames - scale set (%s) in service annotation not found", serviceVMSetName)
			return nil, fmt.Errorf("scale set (%s) - not found", serviceVMSetName)
		}
		return &[]string{serviceVMSetName}, nil
	}
	return vmssFlexNames, nil
}

// GetNodeNameByProviderID gets the node name by provider ID.
// providerID example:
// azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/flexprofile-mp-0_df53ee36
// Different from vmas where vm name is always equal to nodeName, we need to further map vmName to actual nodeName in vmssflex.
// Note: nodeName is always equal to strings.ToLower(*vm.OsProfile.ComputerName)
func (fs *FlexScaleSet) GetNodeNameByProviderID(providerID string) (types.NodeName, error) {
	// NodeName is part of providerID for standard instances.
	matches := providerIDRE.FindStringSubmatch(providerID)
	if len(matches) != 2 {
		return "", errors.New("error splitting providerID")
	}

	nodeName, err := fs.getNodeNameByVMName(matches[1])
	if err != nil {
		return "", err
	}
	return types.NodeName(nodeName), nil
}

// GetInstanceIDByNodeName gets the cloud provider ID by node name.
// It must return ("", cloudprovider.InstanceNotFound) if the instance does
// not exist or is no longer running.
func (fs *FlexScaleSet) GetInstanceIDByNodeName(name string) (string, error) {
	machine, err := fs.getVmssFlexVM(name, azcache.CacheReadTypeUnsafe)
	if err != nil {
		return "", err
	}
	if machine.ID == nil {
		return "", fmt.Errorf("ProviderID of node(%s) is nil", name)
	}
	resourceID := *machine.ID
	convertedResourceID, err := ConvertResourceGroupNameToLower(resourceID)
	if err != nil {
		klog.Errorf("ConvertResourceGroupNameToLower failed with error: %v", err)
		return "", err
	}
	return convertedResourceID, nil

}

// GetInstanceTypeByNodeName gets the instance type by node name.
func (fs *FlexScaleSet) GetInstanceTypeByNodeName(name string) (string, error) {
	machine, err := fs.getVmssFlexVM(name, azcache.CacheReadTypeUnsafe)
	if err != nil {
		klog.Errorf("fs.GetInstanceTypeByNodeName(%s) failed: fs.getVmssFlexVMWithoutInstanceView(%s) err=%v", name, name, err)
		return "", err
	}

	if machine.HardwareProfile == nil {
		return "", fmt.Errorf("HardwareProfile of node(%s) is nil", name)
	}
	return string(machine.HardwareProfile.VMSize), nil
}

// GetZoneByNodeName gets availability zone for the specified node. If the node is not running
// with availability zone, then it returns fault domain.
// for details, refer to https://kubernetes-sigs.github.io/cloud-provider-azure/topics/availability-zones/#node-labels
func (fs *FlexScaleSet) GetZoneByNodeName(name string) (cloudprovider.Zone, error) {
	vm, err := fs.getVmssFlexVM(name, azcache.CacheReadTypeUnsafe)
	if err != nil {
		klog.Errorf("fs.GetZoneByNodeName(%s) failed: fs.getVmssFlexVMWithoutInstanceView(%s) err=%v", name, name, err)
		return cloudprovider.Zone{}, err
	}

	var failureDomain string
	if vm.Zones != nil && len(*vm.Zones) > 0 {
		// Get availability zone for the node.
		zones := *vm.Zones
		zoneID, err := strconv.Atoi(zones[0])
		if err != nil {
			return cloudprovider.Zone{}, fmt.Errorf("failed to parse zone %q: %w", zones, err)
		}

		failureDomain = fs.makeZone(to.String(vm.Location), zoneID)
	} else if vm.VirtualMachineProperties.InstanceView != nil && vm.VirtualMachineProperties.InstanceView.PlatformFaultDomain != nil {
		// Availability zone is not used for the node, falling back to fault domain.
		failureDomain = strconv.Itoa(int(to.Int32(vm.VirtualMachineProperties.InstanceView.PlatformFaultDomain)))
	} else {
		err = fmt.Errorf("failed to get zone info")
		klog.Errorf("GetZoneByNodeName: got unexpected error %v", err)
		return cloudprovider.Zone{}, err
	}

	zone := cloudprovider.Zone{
		FailureDomain: strings.ToLower(failureDomain),
		Region:        strings.ToLower(to.String(vm.Location)),
	}
	return zone, nil
}

// GetProvisioningStateByNodeName returns the provisioningState for the specified node.
func (fs *FlexScaleSet) GetProvisioningStateByNodeName(name string) (provisioningState string, err error) {
	vm, err := fs.getVmssFlexVM(name, azcache.CacheReadTypeDefault)
	if err != nil {
		return provisioningState, err
	}

	if vm.VirtualMachineProperties == nil || vm.VirtualMachineProperties.ProvisioningState == nil {
		return provisioningState, nil
	}

	return to.String(vm.VirtualMachineProperties.ProvisioningState), nil
}

// GetPowerStatusByNodeName returns the powerState for the specified node.
func (fs *FlexScaleSet) GetPowerStatusByNodeName(name string) (powerState string, err error) {
	vm, err := fs.getVmssFlexVM(name, azcache.CacheReadTypeDefault)
	if err != nil {
		return powerState, err
	}

	if vm.InstanceView != nil && vm.InstanceView.Statuses != nil {
		statuses := *vm.InstanceView.Statuses
		for _, status := range statuses {
			state := to.String(status.Code)
			if strings.HasPrefix(state, vmPowerStatePrefix) {
				return strings.TrimPrefix(state, vmPowerStatePrefix), nil
			}
		}
	}

	// vm.InstanceView or vm.InstanceView.Statuses are nil when the VM is under deleting.
	klog.V(3).Infof("InstanceView for node %q is nil, assuming it's stopped", name)
	return vmPowerStateStopped, nil
}

// GetPrimaryInterface gets machine primary network interface by node name.
func (fs *FlexScaleSet) GetPrimaryInterface(nodeName string) (network.Interface, error) {
	return network.Interface{}, nil
}

// GetIPByNodeName gets machine private IP and public IP by node name.
func (fs *FlexScaleSet) GetIPByNodeName(name string) (string, string, error) {
	return "", "", nil

}

// GetPrivateIPsByNodeName returns a slice of all private ips assigned to node (ipv6 and ipv4)
// TODO (khenidak): This should read all nics, not just the primary
// allowing users to split ipv4/v6 on multiple nics
func (fs *FlexScaleSet) GetPrivateIPsByNodeName(name string) ([]string, error) {
	ips := make([]string, 0)
	return ips, nil
}

// GetNodeNameByIPConfigurationID gets the nodeName and vmSetName by IP configuration ID.
func (fs *FlexScaleSet) GetNodeNameByIPConfigurationID(ipConfigurationID string) (string, string, error) {
	return "", "", nil
}

// GetNodeCIDRMaskByProviderID returns the node CIDR subnet mask by provider ID.
func (fs *FlexScaleSet) GetNodeCIDRMasksByProviderID(providerID string) (int, int, error) {
	return 0, 0, nil
}

// EnsureHostInPool ensures the given VM's Primary NIC's Primary IP Configuration is
// participating in the specified LoadBalancer Backend Pool, which returns (resourceGroup, vmasName, instanceID, vmssVM, error).
func (fs *FlexScaleSet) EnsureHostInPool(service *v1.Service, nodeName types.NodeName, backendPoolID string, vmSetNameOfLB string) (string, string, string, *compute.VirtualMachineScaleSetVM, error) {
	return "", "", "", nil, nil

}

// EnsureHostsInPool ensures the given Node's primary IP configurations are
// participating in the specified LoadBalancer Backend Pool.
func (fs *FlexScaleSet) EnsureHostsInPool(service *v1.Service, nodes []*v1.Node, backendPoolID string, vmSetNameOfLB string) error {
	return nil
}

// EnsureBackendPoolDeletedFromVMSets ensures the loadBalancer backendAddressPools deleted from the specified VMSS Flex
func (fs *FlexScaleSet) EnsureBackendPoolDeletedFromVMSets(vmssNamesMap map[string]bool, backendPoolID string) error {
	return nil
}

// EnsureBackendPoolDeleted ensures the loadBalancer backendAddressPools deleted from the specified nodes.
func (fs *FlexScaleSet) EnsureBackendPoolDeleted(service *v1.Service, backendPoolID, vmSetName string, backendAddressPools *[]network.BackendAddressPool, deleteFromVMSet bool) error {
	return nil

}
