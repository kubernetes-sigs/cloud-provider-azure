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

// Package difftracker declares the ServiceGateway diff-tracker API the provider integration compiles
// against. This is the API surface only; the engine implementation is delivered separately. Every
// method is a no-op.
package difftracker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
)

const publicIPNameSuffix = "-pip"

// ServiceUID returns the canonical ServiceGateway identity for a Kubernetes Service.
func ServiceUID(service *v1.Service) string {
	if service == nil {
		return ""
	}
	return strings.ToLower(string(service.UID))
}

// PublicIPName returns the Public IP resource name associated with a ServiceGateway identity.
func PublicIPName(identity string) string {
	return identity + publicIPNameSuffix
}

// ServiceGatewayResourceID returns the Azure resource ID for a ServiceGateway.
func ServiceGatewayResourceID(subscriptionID, resourceGroup, name string) string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/serviceGateways/%s",
		subscriptionID,
		resourceGroup,
		name,
	)
}

// WarningEventError describes an error that the provider should surface as a Kubernetes warning Event.
type WarningEventError interface {
	error
	WarningEvent() (reason, message string)
}

// New initializes the state container used by shared provider tests.
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
		K8sResources:         k8s,
		NRPResources:         nrp,
		logger:               logger.WithName("difftracker"),
		config:               config,
		networkClientFactory: networkClientFactory,
		kubeClient:           kubeClient,
	}
	for _, node := range k8s.Nodes {
		for _, pod := range node.Pods {
			diffTracker.incrementOutboundRefCount(pod.PublicOutboundIdentity)
		}
	}
	return diffTracker, nil
}

func (dt *DiffTracker) lockWithLatency(method string) func() {
	waitStart := time.Now()
	dt.mu.Lock()
	wait := time.Since(waitStart)
	holdStart := time.Now()
	return func() {
		hold := time.Since(holdStart)
		dt.mu.Unlock()
		dt.logger.V(4).Info(
			"DiffTracker state update latency",
			"method", method,
			"lockWaitMicros", wait.Microseconds(),
			"lockHoldMicros", hold.Microseconds(),
		)
	}
}

// InitializeFromCluster builds a DiffTracker from current cluster and NRP state.
func InitializeFromCluster(_ context.Context, _ Config, _ azclient.ClientFactory, _ kubernetes.Interface) (*DiffTracker, error) {
	return &DiffTracker{}, nil
}

// SetEventRecorder publishes the recorder used to emit ServiceGateway pod events.
func (dt *DiffTracker) SetEventRecorder(recorder record.EventRecorder) {
	dt.eventRecorder = recorder
}

// SetServiceLister publishes the provider's Service lister for cached UID resolution.
func (dt *DiffTracker) SetServiceLister(_ corelisters.ServiceLister) {}

// SetNodeLister publishes the provider's Node lister for cached InternalIP resolution.
func (dt *DiffTracker) SetNodeLister(_ corelisters.NodeLister) {}

// SetUpPodInformer starts the egress pod informer.
func (dt *DiffTracker) SetUpPodInformer() {}

// ReconcileEndpointSlice converts an EndpointSlice informer event into an endpoint delta.
func (dt *DiffTracker) ReconcileEndpointSlice(_, _ *discovery_v1.EndpointSlice) {}

// ReconcileNodeIPChange replays a node's endpoints when its InternalIP set changes.
func (dt *DiffTracker) ReconcileNodeIPChange(_ string, _, _ []string) {}

// IsServiceTracked reports whether the engine currently tracks the given service UID.
func (dt *DiffTracker) IsServiceTracked(_ string) bool { return false }

// ReconcileInboundService validates and submits a LoadBalancer Service to the engine.
func (dt *DiffTracker) ReconcileInboundService(_ *v1.Service) error { return nil }

// DeleteInboundService submits a LoadBalancer Service deletion to the engine.
func (dt *DiffTracker) DeleteInboundService(_ *v1.Service) error { return nil }

// UpdateEndpoints applies an inbound service's endpoint delta.
func (dt *DiffTracker) UpdateEndpoints(_ string, _, _ map[string]string) {}

// RegisterMetrics registers the ServiceGateway metrics with the component-base registry.
func RegisterMetrics() {}

// RecordServiceGatewayEnabled marks ServiceGateway as enabled for the cluster.
func RecordServiceGatewayEnabled() {}
