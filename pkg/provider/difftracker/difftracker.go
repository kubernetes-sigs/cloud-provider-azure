/*
Copyright 2026 The Kubernetes Authors.

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

package difftracker

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// InitializeDiffTracker creates and initializes a new DiffTracker with the given state and configuration.
// It validates the configuration and ensures all required dependencies are present.
// Returns an error if the configuration is invalid or if any required dependency is nil.
func InitializeDiffTracker(K8s K8sState, NRP NRPState, config Config, networkClientFactory azclient.ClientFactory, kubeClient kubernetes.Interface) (*DiffTracker, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("InitializeDiffTracker: %w", err)
	}

	if networkClientFactory == nil {
		return nil, fmt.Errorf("InitializeDiffTracker: networkClientFactory must not be nil")
	}
	if kubeClient == nil {
		return nil, fmt.Errorf("InitializeDiffTracker: kubeClient must not be nil")
	}

	klog.V(2).Infof("InitializeDiffTracker: initializing with config: subscription=%s, resourceGroup=%s, location=%s",
		config.SubscriptionID, config.ResourceGroup, config.Location)

	// If any field is nil, initialize it
	if K8s.Services == nil {
		K8s.Services = utilsets.NewString()
	}
	if K8s.Egresses == nil {
		K8s.Egresses = utilsets.NewString()
	}
	if K8s.Nodes == nil {
		K8s.Nodes = make(map[string]Node)
	}
	if NRP.LoadBalancers == nil {
		NRP.LoadBalancers = utilsets.NewString()
	}
	if NRP.NATGateways == nil {
		NRP.NATGateways = utilsets.NewString()
	}
	if NRP.Locations == nil {
		NRP.Locations = make(map[string]NRPLocation)
	}

	diffTracker := &DiffTracker{
		K8sResources: K8s,
		NRPResources: NRP,

		// Configuration and clients
		config:               config,
		networkClientFactory: networkClientFactory,
		kubeClient:           kubeClient,
	}

	return diffTracker, nil
}
