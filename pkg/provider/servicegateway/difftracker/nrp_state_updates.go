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

func (dt *DiffTracker) UpdateNRPLoadBalancers(syncServicesReturnType SyncServicesReturnType) {
	defer dt.lockWithLatency("UpdateNRPLoadBalancers")()

	for _, service := range syncServicesReturnType.Additions.UnsortedList() {
		dt.NRPResources.LoadBalancers.Insert(service)
		dt.logger.V(5).Info("Added service to NRP LoadBalancers", "service", service)
	}

	for _, service := range syncServicesReturnType.Removals.UnsortedList() {
		dt.NRPResources.LoadBalancers.Delete(service)
		dt.logger.V(5).Info("Removed service from NRP LoadBalancers", "service", service)
	}
}

func (dt *DiffTracker) UpdateNRPNATGateways(syncServicesReturnType SyncServicesReturnType) {
	defer dt.lockWithLatency("UpdateNRPNATGateways")()

	for _, service := range syncServicesReturnType.Additions.UnsortedList() {
		dt.NRPResources.NATGateways.Insert(service)
		dt.logger.V(5).Info("Added service to NRP NATGateways", "service", service)
	}

	for _, service := range syncServicesReturnType.Removals.UnsortedList() {
		dt.NRPResources.NATGateways.Delete(service)
		dt.logger.V(5).Info("Removed service from NRP NATGateways", "service", service)
	}
}

func (dt *DiffTracker) UpdateLocationsAddresses(locationData LocationData) {
	defer dt.lockWithLatency("UpdateLocationsAddresses")()

	for locationKey, locationValue := range locationData.Locations {
		// Remove empty locations
		if len(locationValue.Addresses) == 0 {
			delete(dt.NRPResources.Locations, locationKey)
			continue
		}

		// Get or create location
		nrpLocation := dt.NRPResources.Locations[locationKey]
		isFullUpdate := locationValue.AddressUpdateAction == FullUpdate

		// For full updates, start with a fresh location. For partial updates,
		// the location may be absent from NRP (zero value with a nil Addresses
		// map), so initialize it to keep the writes below nil-safe.
		if isFullUpdate || nrpLocation.Addresses == nil {
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
