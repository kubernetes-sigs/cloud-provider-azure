package difftracker

import (
	"k8s.io/klog/v2"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func (dt *DiffTrackerState) GetSyncLoadBalancerNRPServices() SyncNRPServicesReturnType {
	return syncServices(dt.K8s.Services, dt.NRP.LoadBalancers)
}

func (dt *DiffTrackerState) GetSyncNRPNATGateways() SyncNRPServicesReturnType {
	return syncServices(dt.K8s.Egresses, dt.NRP.NATGateways)
}

// SyncServices handles the synchronization of services between K8s and NRP
func syncServices(k8sServices, nrpServices *utilsets.IgnoreCaseSet) SyncNRPServicesReturnType {
	syncServices := SyncNRPServicesReturnType{
		Additions: utilsets.NewString(),
		Removals:  utilsets.NewString(),
	}

	for _, service := range k8sServices.UnsortedList() {
		if nrpServices.Has(service) {
			continue
		}
		syncServices.Additions.Insert(service)
		klog.V(2).Infof("Added service %s to syncing object\n", service)
	}

	for _, service := range nrpServices.UnsortedList() {
		if k8sServices.Has(service) {
			continue
		}
		syncServices.Removals.Insert(service)
		klog.V(2).Infof("Removed service %s from syncing object\n", service)
	}

	return syncServices
}

//==============================================================================

func (dt *DiffTrackerState) GetSyncNRPLocationsAddresses() LocationData {
	result := LocationData{
		Action:    PartialUpdate,
		Locations: make(map[string]Location),
	}

	// Iterate over all nodes in the K8s state
	for nodeIp, node := range dt.K8s.Nodes {
		nrpLocation, exists := dt.NRP.NRPLocations[nodeIp]
		location := initializeLocation(exists)

		for address, pod := range node.Pods {
			serviceRef := createServiceRef(pod)
			addressData := Address{ServiceRef: serviceRef}

			if !exists || !serviceRef.Equals(nrpLocation.NRPAddresses[address].NRPServices) {
				location.Addresses[address] = addressData
			}
		}

		result.Locations[nodeIp] = location
	}

	// Iterate over all locations in the NRP state
	for location, nrpLocation := range dt.NRP.NRPLocations {
		node, exists := dt.K8s.Nodes[location]
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
			for address := range nrpLocation.NRPAddresses {
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

func (dt *DiffTrackerState) GetSyncDiffTrackerState() *SyncDiffTrackerStateReturnType {
	if dt.DeepEqual() {
		return &SyncDiffTrackerStateReturnType{SyncStatus: ALREADY_IN_SYNC}
	}

	return &SyncDiffTrackerStateReturnType{
		SyncStatus:          SUCCESS,
		LoadBalancerUpdates: dt.GetSyncLoadBalancerNRPServices(),
		NATGatewayUpdates:   dt.GetSyncNRPNATGateways(),
		LocationData:        dt.GetSyncNRPLocationsAddresses(),
	}
}
