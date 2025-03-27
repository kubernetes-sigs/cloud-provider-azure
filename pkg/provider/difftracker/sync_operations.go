package difftracker

import (
	"k8s.io/klog/v2"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// SyncServices handles the synchronization of services between K8s and NRP
func syncServices(k8sServices, Services *utilsets.IgnoreCaseSet) SyncServicesReturnType {
	syncServices := SyncServicesReturnType{
		Additions: utilsets.NewString(),
		Removals:  utilsets.NewString(),
	}

	for _, service := range k8sServices.UnsortedList() {
		if Services.Has(service) {
			continue
		}
		syncServices.Additions.Insert(service)
		klog.V(2).Infof("Added service %s to syncing object\n", service)
	}

	for _, service := range Services.UnsortedList() {
		if k8sServices.Has(service) {
			continue
		}
		syncServices.Removals.Insert(service)
		klog.V(2).Infof("Removed service %s from syncing object\n", service)
	}

	return syncServices
}

func (dt *DiffTracker) GetSyncLoadBalancerServices() SyncServicesReturnType {
	return syncServices(dt.K8sResources.Services, dt.NRPResources.LoadBalancers)
}

func (dt *DiffTracker) GetSyncNRPNATGateways() SyncServicesReturnType {
	return syncServices(dt.K8sResources.Egresses, dt.NRPResources.NATGateways)
}

//==============================================================================

func (dt *DiffTracker) GetSyncLocationsAddresses() LocationData {
	result := LocationData{
		Action:    PartialUpdate,
		Locations: make(map[string]Location),
	}

	// Iterate over all nodes in the K8s state
	for nodeIp, node := range dt.K8sResources.Nodes {
		nrpLocation, exists := dt.NRPResources.Locations[nodeIp]
		location := initializeLocation(exists)

		for address, pod := range node.Pods {
			serviceRef := createServiceRef(pod)
			addressData := Address{ServiceRef: serviceRef}

			if !exists || !serviceRef.Equals(nrpLocation.Addresses[address].Services) {
				location.Addresses[address] = addressData
			}
		}

		result.Locations[nodeIp] = location
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
				continue
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
	if pod.PrivateOutboundIdentity != "" {
		serviceRef.Insert(pod.PrivateOutboundIdentity)
	}
	return serviceRef
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
	dt.mu.Lock()
	defer dt.mu.Unlock()

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
