/*
Copyright 2019 The Kubernetes Authors.

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
	"regexp"
	"strings"

	compute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	network "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
)

var (
	nicIDConfigurationRE = regexp.MustCompile(`^/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Network/networkInterfaces/(.+)`)
	vmasNamePrefixRE     = regexp.MustCompile(`(.+)-nic-\d+`)
)

// ListNICs returns the NIC list in the given resource group
func ListNICs(tc *AzureTestClient, rgName string) ([]*network.Interface, error) {
	Logf("getting network interfaces list in resource group %s", rgName)

	ic := tc.createInterfacesClient()

	list, err := ic.List(context.Background(), rgName)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// ListVMSSNICs returns the NIC list in the VMSS
func ListVMSSNICs(tc *AzureTestClient, vmssName string) ([]*network.Interface, error) {
	ic := tc.createInterfacesClient()

	list, err := ic.ListVirtualMachineScaleSetNetworkInterfaces(context.Background(), tc.GetResourceGroup(), vmssName)
	if err != nil {
		return nil, err
	}
	return list, nil
}

// virtual machine availability set only, NIC on VMSS has another naming pattern
func getVMNamePrefixFromNICID(nicID string) (string, error) {
	nicMatches := nicIDConfigurationRE.FindStringSubmatch(nicID)
	if len(nicMatches) != 2 {
		return "", fmt.Errorf("cannot obtain the name of virtual machine from nicID")
	}
	vmMatches := vmasNamePrefixRE.FindStringSubmatch(nicMatches[1])
	if len(vmMatches) != 2 {
		return "", fmt.Errorf("cannot obtain the name of virtual machine from nicID")
	}
	return vmMatches[1], nil
}

// GetTargetNICFromList pick the target virtual machine's NIC from the given NIC list
func GetTargetNICFromList(list []*network.Interface, targetVMNamePrefix string) (*network.Interface, error) {
	if list == nil {
		Logf("empty list given, skip finding target NIC")
		return nil, nil
	}
	for _, nic := range list {
		nic := nic
		vmNamePrefix, err := getVMNamePrefixFromNICID(*nic.ID)
		if err != nil {
			return nil, err
		}
		if vmNamePrefix == targetVMNamePrefix {
			return nic, err
		}
	}
	Logf("cannot find the target NIC in the given NIC list")
	return nil, nil
}

// GetNicIDsFromVM returns the NIC ID in the VM
func GetNicIDsFromVM(vm *compute.VirtualMachine) (map[string]interface{}, error) {
	if vm.Properties.NetworkProfile == nil || vm.Properties.NetworkProfile.NetworkInterfaces == nil ||
		len(vm.Properties.NetworkProfile.NetworkInterfaces) == 0 {
		return nil, fmt.Errorf("cannot obtain NIC on VM %s", *vm.Name)
	}

	nicIDSet := make(map[string]interface{})
	for _, nic := range vm.Properties.NetworkProfile.NetworkInterfaces {
		nicIDSet[*nic.ID] = true
	}

	return nicIDSet, nil
}

// GetNicIDsFromVMSSVM returns the NIC ID in the VMSS VM
func GetNicIDsFromVMSSVM(vm *compute.VirtualMachineScaleSetVM) (map[string]interface{}, error) {
	if vm.Properties.NetworkProfile == nil || vm.Properties.NetworkProfile.NetworkInterfaces == nil ||
		len(vm.Properties.NetworkProfile.NetworkInterfaces) == 0 {
		return nil, fmt.Errorf("cannot obtain NIC on VMSS VM %s", *vm.Name)
	}

	nicIDSet := make(map[string]interface{})
	for _, nic := range vm.Properties.NetworkProfile.NetworkInterfaces {
		nicIDSet[*nic.ID] = true
	}

	return nicIDSet, nil
}

// GetNICByID returns the network interface with the input ID among the list
func GetNICByID(nicID string, nicList []*network.Interface) (*network.Interface, error) {
	for _, nic := range nicList {
		nic := nic
		if strings.EqualFold(*nic.ID, nicID) {
			return nic, nil
		}
	}

	return nil, fmt.Errorf("network interface not found with ID %s", nicID)
}
