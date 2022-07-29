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
	"os"
	"runtime"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	azureprovider "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

// ARMNodeProvider implements nodemanager.NodeProvider.
type ARMNodeProvider struct {
	azure *azureprovider.Cloud
}

// NewARMNodeProvider creates a new ARMNodeProvider.
func NewARMNodeProvider(cloudConfigFilePath string) *ARMNodeProvider {
	var err error
	var configFile *os.File
	var az cloudprovider.Interface

	if cloudConfigFilePath != "" {
		configFile, err = os.Open(cloudConfigFilePath)
		if err != nil {
			klog.Fatalf("Could not open cloud config file %s: %#v", cloudConfigFilePath, err)
		}
		defer configFile.Close()

		az, err = azureprovider.NewCloud(configFile, false)

		if err != nil {
			klog.Fatalf("Failed to initialize Azure cloud provider: %v", err)
		}

	} else {
		klog.Fatal("Cloud config file path is empty, use --cloud-config argument when using ARM node provider.")
	}

	return &ARMNodeProvider{
		azure: az.(*azureprovider.Cloud),
	}
}

// NodeAddresses returns the addresses of the specified instance.
func (np *ARMNodeProvider) NodeAddresses(ctx context.Context, name types.NodeName) ([]v1.NodeAddress, error) {
	return np.azure.NodeAddresses(ctx, name)
}

// InstanceID returns the cloud provider ID of the specified instance.
// Note that if the instance does not exist or is no longer running, we must return ("", cloudprovider.InstanceNotFound)
func (np *ARMNodeProvider) InstanceID(ctx context.Context, name types.NodeName) (string, error) {
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
func (np *ARMNodeProvider) InstanceType(ctx context.Context, name types.NodeName) (string, error) {
	return np.azure.InstanceType(ctx, name)
}

// GetZone returns the Zone containing the current failure zone and locality region that the program is running in
// In most cases, this method is called from the kubelet querying a local metadata service to acquire its zone.
// If the node is not running with availability zones, then it will fall back to fault domain.
func (np *ARMNodeProvider) GetZone(ctx context.Context, name types.NodeName) (cloudprovider.Zone, error) {
	// Needed for cloud-node-manager on windows nodes where hostname of the pod is different from node name
	if runtime.GOOS == "windows" {
		return np.azure.GetZoneByNodeName(ctx, name)
	}

	return np.azure.GetZone(ctx)
}

// GetPlatformSubFaultDomain returns the PlatformSubFaultDomain from IMDS if set.
func (np *ARMNodeProvider) GetPlatformSubFaultDomain() (string, error) {
	return "", nil
}
