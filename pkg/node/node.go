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

package node

import (
	"bytes"
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	azureprovider "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

// IMDSNodeProvider implements nodemanager.NodeProvider.
type IMDSNodeProvider struct {
	azure *azureprovider.Cloud
}

// NewIMDSNodeProvider creates a new IMDSNodeProvider.
func NewIMDSNodeProvider() *IMDSNodeProvider {
	az, err := azureprovider.NewCloud(bytes.NewBuffer([]byte(`{
			"useInstanceMetadata": true,
			"vmType": "vmss"
		}`)), false)
	if err != nil {
		klog.Fatalf("Failed to initialize Azure cloud provider: %v", err)
	}

	return &IMDSNodeProvider{
		azure: az.(*azureprovider.Cloud),
	}
}

// NodeAddresses returns the addresses of the specified instance.
func (np *IMDSNodeProvider) NodeAddresses(ctx context.Context, name types.NodeName) ([]v1.NodeAddress, error) {
	return np.azure.NodeAddresses(ctx, name)
}

// InstanceID returns the cloud provider ID of the specified instance.
// Note that if the instance does not exist or is no longer running, we must return ("", cloudprovider.InstanceNotFound)
func (np *IMDSNodeProvider) InstanceID(ctx context.Context, name types.NodeName) (string, error) {
	instanceID, err := np.azure.InstanceID(ctx, name)
	if err != nil {
		return "", err
	}

	return np.azure.ProviderName() + "://" + instanceID, nil
}

// InstanceType returns the type of the specified instance.
// Note that if the instance does not exist or is no longer running, we must return ("", cloudprovider.InstanceNotFound)
// (Implementer Note): This is used by kubelet. Kubelet will label the node. Real log from kubelet:
// Adding node label from cloud provider: beta.kubernetes.io/instance-type=[value]
func (np *IMDSNodeProvider) InstanceType(ctx context.Context, name types.NodeName) (string, error) {
	return np.azure.InstanceType(ctx, name)
}

// GetZone returns the Zone containing the current failure zone and locality region that the program is running in
// In most cases, this method is called from the kubelet querying a local metadata service to acquire its zone.
// If the node is not running with availability zones, then it will fall back to fault domain.
func (np *IMDSNodeProvider) GetZone(ctx context.Context, name types.NodeName) (cloudprovider.Zone, error) {
	return np.azure.GetZone(ctx)
}

// GetPlatformSubFaultDomain returns the PlatformSubFaultDomain from IMDS if set.
func (np *IMDSNodeProvider) GetPlatformSubFaultDomain() (string, error) {
	return np.azure.GetPlatformSubFaultDomain()
}
