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

package provider

import (
	"context"
	"encoding/json"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// CreateOrUpdateLB invokes az.NetworkClientFactory.GetLoadBalancerClient().CreateOrUpdate with exponential backoff retry
func (az *Cloud) createOrUpdateNatGateway(ctx context.Context, natGatewayResourceGroup string, natGateway armnetwork.NatGateway) error {
	natGatewayName := ptr.Deref(natGateway.Name, "")

	_, err := az.NetworkClientFactory.GetNatGatewayClient().CreateOrUpdate(ctx, natGatewayResourceGroup, natGatewayName, natGateway)
	if err != nil {
		natGatewayJSON, _ := json.Marshal(natGateway)
		klog.Warningf("NatGatewayClient.CreateOrUpdate(%s) failed: %v, NatGateway request: %s", natGatewayName, err, string(natGatewayJSON))
	} else {
		klog.V(10).Infof("NatGatewayClient.CreateOrUpdate(%s): success", natGatewayName)
	}

	return err
}

func (az *Cloud) deleteNatGateway(ctx context.Context, natGatewayResourceGroup string, natGatewayName string) error {
	err := az.NetworkClientFactory.GetNatGatewayClient().Delete(ctx, natGatewayResourceGroup, natGatewayName)
	if err != nil {
		klog.Errorf("NatGatewayClient.Delete(%s) in resource group %s failed: %v", natGatewayName, natGatewayResourceGroup, err)
	} else {
		klog.V(10).Infof("NatGatewayClient.Delete(%s) in resource group %s: success", natGatewayName, natGatewayResourceGroup)
	}
	return err
}
