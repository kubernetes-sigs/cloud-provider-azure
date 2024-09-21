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

	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
)

// ListVMs returns all VMs in the resource group
func ListVMs(tc *AzureTestClient) ([]*armcompute.VirtualMachine, error) {
	vmClient := tc.createVMClient()

	list, err := vmClient.List(context.Background(), tc.GetResourceGroup())
	if err != nil {
		return nil, err
	}
	return list, nil
}

// GetVMComputerName returns the corresponding node name of the VM
func GetVMComputerName(vm *armcompute.VirtualMachine) (string, error) {
	if vm.Properties != nil && vm.Properties.OSProfile == nil || vm.Properties.OSProfile.ComputerName == nil {
		return "", fmt.Errorf("cannot find computer name from vm %s", *vm.Name)
	}

	return *vm.Properties.OSProfile.ComputerName, nil
}
