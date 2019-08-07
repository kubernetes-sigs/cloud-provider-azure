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

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-07-01/network"
)

var (
	nicIDConfigurationRE = regexp.MustCompile(`^/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Network/networkInterfaces/(.+)`)
	vmasNamePrefixRE     = regexp.MustCompile(`(.+)-nic-\d+`)
	nicNameRE            = regexp.MustCompile(`k8s-.+-\d+-.+`)
)

// ListNICs returns the NIC list in the given resource group
func ListNICs(tc *AzureTestClient, rgName string) (*[]network.Interface, error) {
	Logf("getting network interfaces list in resource group %s", rgName)

	ic := tc.createInterfacesClient()

	list, err := ic.List(context.Background(), rgName)
	if err != nil {
		return nil, err
	}
	if len(list.Values()) == 0 {
		Logf("cannot find the corresponding NIC in resource group %s", rgName)
		return nil, nil
	}
	value := list.Values()
	return &value, err
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
func GetTargetNICFromList(list *[]network.Interface, targetVMNamePrefix string) (*network.Interface, error) {
	if list == nil {
		Logf("empty list given, skip finding target NIC")
		return nil, nil
	}
	for _, nic := range *list {
		vmNamePrefix, err := getVMNamePrefixFromNICID(*nic.ID)
		if err != nil {
			return nil, err
		}
		if vmNamePrefix == targetVMNamePrefix {
			return &nic, err
		}
	}
	Logf("cannot find the target NIC in the given NIC list")
	return nil, nil
}
