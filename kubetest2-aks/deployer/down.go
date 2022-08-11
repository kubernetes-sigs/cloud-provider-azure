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

package deployer

import (
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"k8s.io/klog"
)

func (d *deployer) deleteResourceGroup(subscriptionID string, credential azcore.TokenCredential) error {
	klog.Infof("Deleting resource group %q", d.ResourceGroupName)
	rgClient, _ := armresources.NewResourceGroupsClient(subscriptionID, credential, nil)

	poller, err := rgClient.BeginDelete(ctx, d.ResourceGroupName, nil)
	if err != nil {
		return fmt.Errorf("failed to begin deleting resource group %q: %v", d.ResourceGroupName, err)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to poll until deletion of resource group %q is done: %v", d.ResourceGroupName, err)
	}
	return nil
}

func (d *deployer) Down() error {
	// Create a credentials object.
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		klog.Fatalf("failed to authenticate: %v", err)
	}

	err = d.deleteResourceGroup(subscriptionID, cred)
	if err != nil {
		klog.Fatalf("failed to delete resource group %q: %v", d.ResourceGroupName, err)
	}

	klog.Infof("Resource group %q deleted", d.ResourceGroupName)
	return nil
}
