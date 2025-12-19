package difftracker

import (
	"k8s.io/klog/v2"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// SyncServices handles the synchronization of services between K8s and NRP
func GetServicesToSync(k8sServices, Services *utilsets.IgnoreCaseSet) SyncServicesReturnType {
	klog.Infof("GetServicesToSync: K8s services (%d): %v", k8sServices.Len(), k8sServices.UnsortedList())
	klog.Infof("GetServicesToSync: NRP services (%d): %v", Services.Len(), Services.UnsortedList())

	syncServices := SyncServicesReturnType{
		Additions: utilsets.NewString(),
		Removals:  utilsets.NewString(),
	}

	for _, service := range k8sServices.UnsortedList() {
		if Services.Has(service) {
			continue
		}
		syncServices.Additions.Insert(service)
		klog.Infof("GetServicesToSync: Added service %s to additions", service)
	}

	for _, service := range Services.UnsortedList() {
		if k8sServices.Has(service) {
			continue
		}
		syncServices.Removals.Insert(service)
		klog.Infof("GetServicesToSync: Added service %s to removals", service)
	}

	klog.Infof("GetServicesToSync: Result - Additions: %d, Removals: %d", syncServices.Additions.Len(), syncServices.Removals.Len())
	return syncServices
}

func (dt *DiffTracker) GetSyncLoadBalancerServices() SyncServicesReturnType {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	return GetServicesToSync(dt.K8sResources.Services, dt.NRPResources.LoadBalancers)
}

func (dt *DiffTracker) GetSyncNRPNATGateways() SyncServicesReturnType {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	return GetServicesToSync(dt.K8sResources.Egresses, dt.NRPResources.NATGateways)
}

//==============================================================================

func (dt *DiffTracker) GetSyncLocationsAddresses() LocationData {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	result := LocationData{
		Action:    PartialUpdate,
		Locations: make(map[string]Location),
	}

	// Iterate over all nodes in the K8s state
	for nodeIp, node := range dt.K8sResources.Nodes {
		nrpLocation, locationExists := dt.NRPResources.Locations[nodeIp]
		location := initializeLocation(locationExists)
		locationUpdated := false

		for address, pod := range node.Pods {
			// Filter services: only include services that are StateCreated or exist in NRP
			serviceRef := dt.createServiceRefFiltered(pod)

			// Check if address exists in NRP and if service list changed
			nrpAddressData, addressExists := nrpLocation.Addresses[address]

			// Skip this address if:
			// 1. No ready services AND address doesn't exist in NRP (nothing to sync)
			// 2. ServiceRef matches what's already in NRP (no change)
			if serviceRef.Len() == 0 && !addressExists {
				// No services and address not in NRP - nothing to do
				continue
			}

			if addressExists && serviceRef.Equals(nrpAddressData.Services) {
				// ServiceRef matches NRP - no change needed
				continue
			}

			// ServiceRef changed (or address is new) - need to sync
			addressData := Address{ServiceRef: serviceRef}
			location.Addresses[address] = addressData
			locationUpdated = true
		}
		if locationUpdated {
			result.Locations[nodeIp] = location
		}
	}

	// Iterate over all locations in the NRP state
	for location, nrpLocation := range dt.NRPResources.Locations {
		node, exists := dt.K8sResources.Nodes[location]
		if !exists {
			result.Locations[location] = Location{
				AddressUpdateAction: PartialUpdate,
				Addresses:           make(map[string]Address),
			}
		} else {
			locationData := findLocationData(result, location)
			if locationData == nil {
				locationData = &Location{
					AddressUpdateAction: PartialUpdate,
					Addresses:           make(map[string]Address),
				}
			}
			for address := range nrpLocation.Addresses {
				if _, exists := node.Pods[address]; !exists {
					addressData := Address{ServiceRef: utilsets.NewString()}
					locationData.Addresses[address] = addressData
					result.Locations[location] = *locationData
				}
			}
		}
	}
	return result
}

// Helper function to initialize Location based on existence in NRP
func initializeLocation(exists bool) Location {
	if !exists {
		return Location{
			AddressUpdateAction: FullUpdate,
			Addresses:           make(map[string]Address),
		}
	}
	return Location{
		AddressUpdateAction: PartialUpdate,
		Addresses:           make(map[string]Address),
	}
}

// Helper function to create ServiceRef from Pod
func createServiceRef(pod Pod) *utilsets.IgnoreCaseSet {
	serviceRef := utilsets.NewString()
	for _, identity := range pod.InboundIdentities.UnsortedList() {
		serviceRef.Insert(identity)
	}
	if pod.PublicOutboundIdentity != "" {
		serviceRef.Insert(pod.PublicOutboundIdentity)
	}
	return serviceRef
}

// createServiceRefFiltered creates ServiceRef but only includes services that are StateCreated
// This prevents LocationsUpdater from trying to sync locations for services still being created
// Must be called with dt.mu held
func (dt *DiffTracker) createServiceRefFiltered(pod Pod) *utilsets.IgnoreCaseSet {
	serviceRef := utilsets.NewString()

	// Check inbound services (LoadBalancers)
	for _, serviceUID := range pod.InboundIdentities.UnsortedList() {
		if dt.isServiceReady(serviceUID, true) {
			serviceRef.Insert(serviceUID)
		}
	}

	// Check outbound service (NAT Gateway)
	if pod.PublicOutboundIdentity != "" {
		if dt.isServiceReady(pod.PublicOutboundIdentity, false) {
			serviceRef.Insert(pod.PublicOutboundIdentity)
		}
	}

	return serviceRef
}

// isServiceReady checks if a service is ready for location sync
// Returns true if service is StateCreated or exists in NRP (but not tracked)
// Must be called with dt.mu held
func (dt *DiffTracker) isServiceReady(serviceUID string, isInbound bool) bool {
	// Check if service is tracked in pendingServiceOps
	if opState, exists := dt.pendingServiceOps[serviceUID]; exists {
		// Only sync if service is StateCreated
		return opState.State == StateCreated
	}

	// Service not tracked - check if it exists in NRP (created outside Engine)
	if isInbound {
		return dt.NRPResources.LoadBalancers.Has(serviceUID)
	}
	return dt.NRPResources.NATGateways.Has(serviceUID)
}

// Helper function to find LocationData in result
func findLocationData(result LocationData, location string) *Location {
	for keyCurrentLocation := range result.Locations {
		if keyCurrentLocation == location {
			loc := result.Locations[keyCurrentLocation]
			return &loc
		}
	}
	return nil
}

//==============================================================================

func (dt *DiffTracker) GetSyncOperations() *SyncDiffTrackerReturnType {
	if dt.DeepEqual() {
		return &SyncDiffTrackerReturnType{SyncStatus: ALREADY_IN_SYNC}
	}

	return &SyncDiffTrackerReturnType{
		SyncStatus:          SUCCESS,
		LoadBalancerUpdates: dt.GetSyncLoadBalancerServices(),
		NATGatewayUpdates:   dt.GetSyncNRPNATGateways(),
		LocationData:        dt.GetSyncLocationsAddresses(),
	}
}
