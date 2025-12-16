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
// Panics if critical dependencies (config, networkClientFactory, kubeClient) are invalid.
func InitializeDiffTracker(K8s K8s_State, NRP NRP_State, config Config, networkClientFactory azclient.ClientFactory, kubeClient kubernetes.Interface) *DiffTracker {
	// Validate configuration
	if err := config.Validate(); err != nil {
		panic(fmt.Sprintf("InitializeDiffTracker: %v", err))
	}

	// Validate required dependencies
	if networkClientFactory == nil {
		panic("InitializeDiffTracker: networkClientFactory must not be nil")
	}
	if kubeClient == nil {
		panic("InitializeDiffTracker: kubeClient must not be nil")
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
		K8sResources:    K8s,
		NRPResources:    NRP,
		InitialSyncDone: false,

		// Configuration and clients
		config:               config,
		networkClientFactory: networkClientFactory,
		kubeClient:           kubeClient,

		// Initialize Engine state management maps
		pendingServiceOps: make(map[string]*ServiceOperationState),
		pendingEndpoints:  make(map[string][]PendingEndpointUpdate),
		pendingPods:       make(map[string][]PendingPodUpdate),
		pendingDeletions:  make(map[string]*PendingDeletion),

		// Initialize Engine communication channels
		serviceUpdaterTrigger:   make(chan bool, 1),
		locationsUpdaterTrigger: make(chan bool, 1),
	}

	return diffTracker
}

// GetServiceUpdaterTrigger returns the trigger channel for ServiceUpdater
func (dt *DiffTracker) GetServiceUpdaterTrigger() <-chan bool {
	return dt.serviceUpdaterTrigger
}

// GetLocationsUpdaterTrigger returns the trigger channel for LocationsUpdater
func (dt *DiffTracker) GetLocationsUpdaterTrigger() <-chan bool {
	return dt.locationsUpdaterTrigger
}
