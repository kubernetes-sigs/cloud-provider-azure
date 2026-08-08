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
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
)

// New creates and initializes a new DiffTracker with the given state and configuration.
// It validates the configuration and ensures all required dependencies are present.
// Returns an error if the configuration is invalid or if any required dependency is nil.
func New(logger logr.Logger, k8s K8sState, nrp NRPState, config Config, networkClientFactory azclient.ClientFactory, kubeClient kubernetes.Interface) (*DiffTracker, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("difftracker.New: %w", err)
	}

	if networkClientFactory == nil {
		return nil, fmt.Errorf("difftracker.New: networkClientFactory must not be nil")
	}
	if kubeClient == nil {
		return nil, fmt.Errorf("difftracker.New: kubeClient must not be nil")
	}

	logger = logger.WithName("difftracker")

	// The caller is expected to pass fully initialized state structs. A nil
	// field is unexpected and indicates a programming error, so error out.
	if k8s.Services == nil {
		return nil, fmt.Errorf("difftracker.New: k8s.Services must not be nil")
	}
	if k8s.Egresses == nil {
		return nil, fmt.Errorf("difftracker.New: k8s.Egresses must not be nil")
	}
	if k8s.Nodes == nil {
		return nil, fmt.Errorf("difftracker.New: k8s.Nodes must not be nil")
	}
	if nrp.LoadBalancers == nil {
		return nil, fmt.Errorf("difftracker.New: nrp.LoadBalancers must not be nil")
	}
	if nrp.NATGateways == nil {
		return nil, fmt.Errorf("difftracker.New: nrp.NATGateways must not be nil")
	}
	if nrp.Locations == nil {
		return nil, fmt.Errorf("difftracker.New: nrp.Locations must not be nil")
	}

	diffTracker := &DiffTracker{
		K8sResources: k8s,
		NRPResources: nrp,

		logger: logger,

		// Configuration and clients
		config:               config,
		networkClientFactory: networkClientFactory,
		kubeClient:           kubeClient,
	}

	// Seed the outbound ref-counter from egress pods already in the initial state
	// so a later REMOVE can drive the counter to zero.
	for _, node := range k8s.Nodes {
		for _, pod := range node.Pods {
			diffTracker.incrementOutboundRefCount(pod.PublicOutboundIdentity)
		}
	}

	logger.V(2).Info("Initialized DiffTracker",
		"subscription", config.SubscriptionID,
		"resourceGroup", config.ResourceGroup,
		"location", config.Location,
		"serviceGatewayResourceName", config.ServiceGatewayResourceName,
		"serviceGatewayID", config.ServiceGatewayID,
		"vnetName", config.VNetName)

	return diffTracker, nil
}

// lockWithLatency acquires dt.mu and returns a release function that unlocks it and,
// at V(4), logs how long the caller waited to acquire the lock and how long it was
// held. It is the instrumented equivalent of `dt.mu.Lock(); defer dt.mu.Unlock()`:
//
//	defer dt.lockWithLatency("MethodName")()
//
// The critical sections it guards are in-memory map mutations with no Azure calls, so
// these durations are expected to be in the microsecond range.
func (dt *DiffTracker) lockWithLatency(method string) func() {
	waitStart := time.Now()
	dt.mu.Lock()
	wait := time.Since(waitStart)
	holdStart := time.Now()
	return func() {
		hold := time.Since(holdStart)
		dt.mu.Unlock()
		dt.logger.V(4).Info("DiffTracker state update latency",
			"method", method,
			"lockWaitMicros", wait.Microseconds(),
			"lockHoldMicros", hold.Microseconds())
	}
}
