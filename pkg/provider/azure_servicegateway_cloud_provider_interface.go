/*
Copyright 2024 The Kubernetes Authors.

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

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	v1 "k8s.io/api/core/v1"
)

// GetClusterName returns the cluster name (empty for now, passed as parameter to methods)
func (az *Cloud) GetClusterName() string {
	// Cluster name is passed as parameter to LoadBalancer methods, not stored in config
	// This method is here to satisfy the CloudProvider interface
	// TODO: Consider storing cluster name in Cloud struct for easier access
	return ""
}

// GetResourceGroup returns the resource group from the config
func (az *Cloud) GetResourceGroup() string {
	return az.ResourceGroup
}

// GetServiceGatewayResourceName returns the ServiceGateway resource name
func (az *Cloud) GetServiceGatewayResourceName() string {
	return az.ServiceGatewayResourceName
}

// GetSubscriptionID returns the subscription ID
func (az *Cloud) GetSubscriptionID() string {
	return az.SubscriptionID
}

// CreateOrUpdateNatGateway wraps the unexported method
func (az *Cloud) CreateOrUpdateNatGateway(ctx context.Context, natGatewayResourceGroup string, natGateway armnetwork.NatGateway) error {
	return az.createOrUpdateNatGateway(ctx, natGatewayResourceGroup, natGateway)
}

// DeleteNatGateway wraps the unexported method
func (az *Cloud) DeleteNatGateway(ctx context.Context, natGatewayResourceGroup string, natGatewayName string) error {
	return az.deleteNatGateway(ctx, natGatewayResourceGroup, natGatewayName)
}

// EnsureServiceLoadBalancerDeletedByUID wraps the unexported method
func (az *Cloud) EnsureServiceLoadBalancerDeletedByUID(ctx context.Context, uid string, clusterName string) error {
	return az.ensureServiceLoadBalancerDeletedByUID(ctx, uid, clusterName)
}

// GetServiceByUID wraps the unexported method
func (az *Cloud) GetServiceByUID(ctx context.Context, uid string) (*v1.Service, error) {
	return az.getServiceByUID(ctx, uid)
}
