/*
Copyright 2021 The Kubernetes Authors.

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
	"context"
	"runtime"

	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	azureprovider "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

// ARMNodeProvider implements nodemanager.NodeProvider.
type ARMNodeProvider struct {
	*azureprovider.Cloud
}

// NewARMNodeProvider creates a new ARMNodeProvider.
func NewARMNodeProvider(ctx context.Context, cloudConfigFilePath string) *ARMNodeProvider {
	var err error
	var az cloudprovider.Interface
	az, err = azureprovider.NewCloudFromConfigFile(ctx, cloudConfigFilePath, false)
	if err != nil {
		klog.Fatalf("Failed to initialize Azure cloud provider: %v", err)
	}

	return &ARMNodeProvider{
		az.(*azureprovider.Cloud),
	}
}

// InstanceID returns the cloud provider ID of the specified instance.
// Note that if the instance does not exist or is no longer running, we must return ("", cloudprovider.InstanceNotFound)
func (np *ARMNodeProvider) InstanceID(ctx context.Context, name types.NodeName) (string, error) {
	instanceID, err := np.Cloud.InstanceID(ctx, name)
	if err != nil {
		return "", err
	}

	return np.Cloud.ProviderName() + "://" + instanceID, nil
}

// GetZone returns the Zone containing the current failure zone and locality region that the program is running in
// In most cases, this method is called from the kubelet querying a local metadata service to acquire its zone.
// If the node is not running with availability zones, then it will fall back to fault domain.
func (np *ARMNodeProvider) GetZone(ctx context.Context, name types.NodeName) (cloudprovider.Zone, error) {
	// Needed for cloud-node-manager on windows nodes where hostname of the pod is different from node name
	if runtime.GOOS == "windows" {
		return np.Cloud.GetZoneByNodeName(ctx, name)
	}

	return np.Cloud.GetZone(ctx)
}

// GetPlatformSubFaultDomain returns the PlatformSubFaultDomain from IMDS if set.
func (np *ARMNodeProvider) GetPlatformSubFaultDomain() (string, error) {
	return "", nil
}
