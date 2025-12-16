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
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// func (az *Cloud) getNatGateway(ctx context.Context, natGatewayResourceGroup string, natGatewayName string) (*armnetwork.NatGateway, error) {
// 	klog.Infof("NatGatewayClient.Get(%s) in resource group %s: start", natGatewayName, natGatewayResourceGroup)
// 	result, err := az.NetworkClientFactory.GetNatGatewayClient().Get(ctx, natGatewayResourceGroup, natGatewayName, nil)
// 	if err != nil {
// 		klog.Errorf("NatGatewayClient.Get(%s) in resource group %s failed: %v", natGatewayName, natGatewayResourceGroup, err)
// 		return nil, err
// 	}
// 	klog.V(10).Infof("NatGatewayClient.Get(%s) in resource group %s: success", natGatewayName, natGatewayResourceGroup)
// 	klog.Infof("NatGatewayClient.Get(%s) in resource group %s: end, error: nil", natGatewayName, natGatewayResourceGroup)
// 	return result, nil
// }

// TODO(enechitoaia): remove when aks-rp
// CreateOrUpdateLB invokes az.NetworkClientFactory.GetLoadBalancerClient().CreateOrUpdate with exponential backoff retry
func (az *Cloud) createOrUpdateNatGateway(ctx context.Context, natGatewayResourceGroup string, natGateway armnetwork.NatGateway) error {
	natGatewayName := ptr.Deref(natGateway.Name, "")
	klog.Infof("NatGatewayClient.CreateOrUpdate(%s): start", natGatewayName)

	// Endless retry loop with 5-second intervals
	for {
		_, err := az.NetworkClientFactory.GetNatGatewayClient().CreateOrUpdate(ctx, natGatewayResourceGroup, natGatewayName, natGateway)
		if err == nil {
			klog.V(10).Infof("NatGatewayClient.CreateOrUpdate(%s): success", natGatewayName)
			klog.Infof("NatGatewayClient.CreateOrUpdate(%s): end, error: nil", natGatewayName)
			return nil
		}

		natGatewayJSON, _ := json.Marshal(natGateway)
		klog.Warningf("NatGatewayClient.CreateOrUpdate(%s) failed: %v, NatGateway request: %s", natGatewayName, err, string(natGatewayJSON))

		// Check if context is canceled
		select {
		case <-ctx.Done():
			klog.V(3).Infof("createOrUpdateNatGateway: context canceled, stopping retry")
			return fmt.Errorf("context canceled: %w", ctx.Err())
		default:
			// Continue with retry
		}

		// Wait 5 seconds before retrying
		klog.V(3).Infof("createOrUpdateNatGateway: retrying in 5 seconds for NAT Gateway %s", natGatewayName)
		time.Sleep(5 * time.Second)
	}
}

// func (az *Cloud) deleteNatGateway(ctx context.Context, natGatewayResourceGroup string, natGatewayName string) error {
// 	klog.Infof("NatGatewayClient.Delete(%s) in resource group %s: start", natGatewayName, natGatewayResourceGroup)

// 	// Endless retry loop with 5-second intervals
// 	for {
// 		err := az.NetworkClientFactory.GetNatGatewayClient().Delete(ctx, natGatewayResourceGroup, natGatewayName)
// 		if err == nil {
// 			klog.V(10).Infof("NatGatewayClient.Delete(%s) in resource group %s: success", natGatewayName, natGatewayResourceGroup)
// 			klog.Infof("NatGatewayClient.Delete(%s) in resource group %s: end, error: nil", natGatewayName, natGatewayResourceGroup)
// 			return nil
// 		}

// 		klog.Errorf("NatGatewayClient.Delete(%s) in resource group %s failed: %v", natGatewayName, natGatewayResourceGroup, err)

// 		// Check if context is canceled
// 		select {
// 		case <-ctx.Done():
// 			klog.V(3).Infof("deleteNatGateway: context canceled, stopping retry")
// 			return fmt.Errorf("context canceled: %w", ctx.Err())
// 		default:
// 			// Continue with retry
// 		}

// 		// Wait 5 seconds before retrying
// 		klog.V(3).Infof("deleteNatGateway: retrying in 5 seconds for NAT Gateway %s", natGatewayName)
// 		time.Sleep(5 * time.Second)
// 	}
// }
