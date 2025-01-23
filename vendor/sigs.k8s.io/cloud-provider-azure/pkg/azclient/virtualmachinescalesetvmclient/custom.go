/*
Copyright 2023 The Kubernetes Authors.

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

package virtualmachinescalesetvmclient

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

// Update updates a VirtualMachine.
func (client *Client) Update(ctx context.Context, resourceGroupName string, VMScaleSetName string, instanceID string, parameters armcompute.VirtualMachineScaleSetVM) (*armcompute.VirtualMachineScaleSetVM, error) {
	resp, err := utils.NewPollerWrapper(client.VirtualMachineScaleSetVMsClient.BeginUpdate(ctx, resourceGroupName, VMScaleSetName, instanceID, parameters, nil)).WaitforPollerResp(ctx)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		return &resp.VirtualMachineScaleSetVM, nil
	}
	return nil, nil
}

// List gets a list of VirtualMachineScaleSetVM in the resource group.
func (client *Client) List(ctx context.Context, resourceGroupName string, parentResourceName string) (result []*armcompute.VirtualMachineScaleSetVM, rerr error) {
	pager := client.VirtualMachineScaleSetVMsClient.NewListPager(resourceGroupName, parentResourceName, nil)
	for pager.More() {
		nextResult, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, nextResult.Value...)
	}
	return result, nil
}

// GetInstanceView gets the instance view of the VirtualMachineScaleSetVM.
func (client *Client) GetInstanceView(ctx context.Context, resourceGroupName string, vmScaleSetName string, instanceID string) (*armcompute.VirtualMachineScaleSetVMInstanceView, error) {
	resp, err := client.VirtualMachineScaleSetVMsClient.GetInstanceView(ctx, resourceGroupName, vmScaleSetName, instanceID, nil)
	if err != nil {
		return nil, err
	}
	return &resp.VirtualMachineScaleSetVMInstanceView, nil
}

// List gets a list of VirtualMachineScaleSetVM in the resource group.
func (client *Client) ListVMInstanceView(ctx context.Context, resourceGroupName string, parentResourceName string) (result []*armcompute.VirtualMachineScaleSetVM, rerr error) {
	pager := client.VirtualMachineScaleSetVMsClient.NewListPager(resourceGroupName, parentResourceName, &armcompute.VirtualMachineScaleSetVMsClientListOptions{
		Expand: to.Ptr(string(armcompute.InstanceViewTypesInstanceView)),
	})
	for pager.More() {
		nextResult, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, nextResult.Value...)
	}
	return result, nil
}
