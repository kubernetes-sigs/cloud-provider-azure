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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"

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
	// The subscription ID and the full ARM resource ID that embeds it are omitted: this line is
	// routine startup output, and the resource group, location and resource names below identify the
	// deployment for support purposes without putting tenant identifiers into every log sink.
	logger.V(2).Info("Initialized DiffTracker",
		"resourceGroup", config.ResourceGroup,
		"location", config.Location,
		"serviceGatewayResourceName", config.ServiceGatewayResourceName,
		"vnetName", config.VNetName)

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
		K8sResources:    k8s,
		NRPResources:    nrp,
		InitialSyncDone: false,

		logger: logger,

		// Configuration and clients
		config:               config,
		networkClientFactory: networkClientFactory,
		kubeClient:           kubeClient,

		// Initialize Engine state management maps
		pendingServiceOps:       make(map[string]*ServiceOperationState),
		pendingEndpoints:        make(map[string][]PendingEndpointUpdate),
		pendingPods:             make(map[string][]PendingPodUpdate),
		pendingServiceDeletions: make(map[string]*PendingServiceDeletion),
		pendingPodDeletions:     make(map[string]*PendingPodDeletion),

		recoveredServiceFinalizers: make(map[string]struct{}),

		// Initialize Engine communication channels
		serviceUpdaterTrigger:   make(chan bool, 1),
		locationsUpdaterTrigger: make(chan bool, 1),
	}

	// Seed the outbound ref-counter from egress pods already in the initial state
	// so a later REMOVE can drive the counter to zero.
	for _, node := range k8s.Nodes {
		for _, pod := range node.Pods {
			diffTracker.incrementOutboundRefCount(pod.PublicOutboundIdentity)
		}
	}

	return diffTracker, nil
}

// SetServiceLister publishes the provider's shared-informer-backed Service lister to the engine.
// It is called from SetInformers once the lister exists; getServiceByUID uses it for O(1) cached
// reads instead of listing every Service. Safe to call from a different goroutine than the engine
// workers because the write is serialized under mu.
func (dt *DiffTracker) SetServiceLister(lister corelisters.ServiceLister) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.serviceLister = lister
}

// SetNodeLister publishes the provider's shared-informer-backed Node lister to the engine.
func (dt *DiffTracker) SetNodeLister(lister corelisters.NodeLister) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.nodeLister = lister
}

// SetEventRecorder publishes the recorder used to emit Service Gateway pod events. Set post-init,
// before the egress pod informer starts.
func (dt *DiffTracker) SetEventRecorder(recorder record.EventRecorder) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.eventRecorder = recorder
}

// recordEvent emits a Kubernetes Event for object, if a recorder has been published.
//
// It is the only supported way to reach dt.eventRecorder. SetEventRecorder writes the field under
// dt.mu after construction while informer handlers read it concurrently, so an unsynchronised read
// is a data race; the field can also still be nil on paths that never publish a recorder, which
// would panic the informer goroutine that dereferenced it. The recorder is snapshotted and the lock
// released before Event is called, because Event performs I/O.
func (dt *DiffTracker) recordEvent(object runtime.Object, eventType, reason, message string) {
	if dt == nil {
		return
	}
	dt.mu.Lock()
	recorder := dt.eventRecorder
	dt.mu.Unlock()
	if recorder == nil {
		return
	}
	recorder.Event(object, eventType, reason, message)
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

// GetServiceUpdaterTrigger returns the trigger channel for ServiceUpdater
func (dt *DiffTracker) GetServiceUpdaterTrigger() <-chan bool {
	return dt.serviceUpdaterTrigger
}

// GetLocationsUpdaterTrigger returns the trigger channel for LocationsUpdater
func (dt *DiffTracker) GetLocationsUpdaterTrigger() <-chan bool {
	return dt.locationsUpdaterTrigger
}
