package difftracker

import (
	"k8s.io/klog/v2"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func (dt *DiffTracker) UpdateNRPLoadBalancers(syncServicesReturnType SyncServicesReturnType) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	for _, service := range syncServicesReturnType.Additions.UnsortedList() {
		dt.NRPResources.LoadBalancers.Insert(service)
		klog.V(2).Infof("Added service %s to NRP LoadBalancers\n", service)
	}

	for _, service := range syncServicesReturnType.Removals.UnsortedList() {
		dt.NRPResources.LoadBalancers.Delete(service)
		klog.V(2).Infof("Removed service %s from NRP LoadBalancers\n", service)
	}
}

func (dt *DiffTracker) UpdateNRPNATGateways(syncServicesReturnType SyncServicesReturnType) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	for _, service := range syncServicesReturnType.Additions.UnsortedList() {
		dt.NRPResources.NATGateways.Insert(service)
		klog.V(2).Infof("Added service %s to NRP NATGateways\n", service)
	}

	for _, service := range syncServicesReturnType.Removals.UnsortedList() {
		dt.NRPResources.NATGateways.Delete(service)
		klog.V(2).Infof("Removed service %s from NRP NATGateways\n", service)
	}
}

func (dt *DiffTracker) UpdateLocationsAddresses(locationData LocationData) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	for locationKey, locationValue := range locationData.Locations {
		// Remove empty locations
		if len(locationValue.Addresses) == 0 {
			delete(dt.NRPResources.Locations, locationKey)
			continue
		}

		// Get or create location
		nrpLocation, exists := dt.NRPResources.Locations[locationKey]
		isFullUpdate := !exists || locationValue.AddressUpdateAction == FullUpdate

		// For full updates, start with a fresh location
		if isFullUpdate {
			nrpLocation = NRPLocation{
				Addresses: make(map[string]NRPAddress),
			}
		}

		// Process address updates
		for addressKey, addressValue := range locationValue.Addresses {
			// Remove empty addresses
			if addressValue.ServiceRef.Len() == 0 {
				delete(nrpLocation.Addresses, addressKey)
				continue
			}

			// Create new service references set
			serviceRefs := createServiceRefsFromAddress(addressValue)

			// For full update or when address doesn't exist, add new address
			if isFullUpdate || !addressExists(nrpLocation, addressKey) {
				nrpLocation.Addresses[addressKey] = NRPAddress{Services: serviceRefs}
				continue
			}

			// For partial updates with existing address
			existingAddress := nrpLocation.Addresses[addressKey]
			if !serviceRefs.Equals(existingAddress.Services) {
				nrpLocation.Addresses[addressKey] = NRPAddress{
					Services: serviceRefs,
				}
			}
		}

		// Save location if it has addresses, otherwise delete it
		if len(nrpLocation.Addresses) > 0 {
			dt.NRPResources.Locations[locationKey] = nrpLocation
		} else {
			delete(dt.NRPResources.Locations, locationKey)
		}
	}
}

// Helper function to check if address exists in a location
func addressExists(location NRPLocation, addressKey string) bool {
	_, exists := location.Addresses[addressKey]
	return exists
}

// Helper function to create service references from an address
func createServiceRefsFromAddress(addressValue Address) *utilsets.IgnoreCaseSet {
	serviceRefs := utilsets.NewString()
	for _, service := range addressValue.ServiceRef.UnsortedList() {
		serviceRefs.Insert(service)
	}
	return serviceRefs
}
