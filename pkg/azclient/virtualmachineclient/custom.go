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

package virtualmachineclient

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
)

// Get gets the VirtualMachine
func (client *Client) Get(ctx context.Context, resourceGroupName string, resourceName string, expand *string) (result *armcompute.VirtualMachine, rerr error) {
	var ops *armcompute.VirtualMachinesClientGetOptions
	if expand != nil {
		expand := armcompute.InstanceViewTypes(*expand)
		ops = &armcompute.VirtualMachinesClientGetOptions{Expand: &expand}
	}

	resp, err := client.VirtualMachinesClient.Get(ctx, resourceGroupName, resourceName, ops)
	if err != nil {
		return nil, err
	}
	//handle statuscode
	return &resp.VirtualMachine, nil
}
