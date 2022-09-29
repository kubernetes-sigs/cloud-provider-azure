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

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-12-01/compute"
)

// ListVMs returns all VMs in the resource group
func ListVMs(tc *AzureTestClient) (*[]compute.VirtualMachine, error) {
	vmClient := tc.createVMClient()

	list, err := vmClient.List(context.Background(), tc.GetResourceGroup())
	if err != nil {
		return nil, err
	}

	res := list.Values()
	return &res, nil
}

// GetVMComputerName returns the corresponding node name of the VM
func GetVMComputerName(vm compute.VirtualMachine) (string, error) {
	if vm.OsProfile == nil || vm.OsProfile.ComputerName == nil {
		return "", fmt.Errorf("cannot find computer name from vm %s", *vm.Name)
	}

	return *vm.OsProfile.ComputerName, nil
}
