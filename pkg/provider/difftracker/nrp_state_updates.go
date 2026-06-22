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
	"k8s.io/klog/v2"
)

func (dt *DiffTracker) UpdateNRPLoadBalancers(syncServicesReturnType SyncServicesReturnType) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	for _, service := range syncServicesReturnType.Additions.UnsortedList() {
		dt.NRPResources.LoadBalancers.Insert(service)
		klog.V(2).Infof("UpdateNRPLoadBalancers: Added service %s to NRP LoadBalancers", service)
	}

	for _, service := range syncServicesReturnType.Removals.UnsortedList() {
		dt.NRPResources.LoadBalancers.Delete(service)
		klog.V(2).Infof("UpdateNRPLoadBalancers: Removed service %s from NRP LoadBalancers", service)
	}
}

func (dt *DiffTracker) UpdateNRPNATGateways(syncServicesReturnType SyncServicesReturnType) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	for _, service := range syncServicesReturnType.Additions.UnsortedList() {
		dt.NRPResources.NATGateways.Insert(service)
		klog.V(2).Infof("UpdateNRPNATGateways: Added service %s to NRP NATGateways", service)
	}

	for _, service := range syncServicesReturnType.Removals.UnsortedList() {
		dt.NRPResources.NATGateways.Delete(service)
		klog.V(2).Infof("UpdateNRPNATGateways: Removed service %s from NRP NATGateways", service)
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
				if !isFullUpdate {
					delete(nrpLocation.Addresses, addressKey)
				}
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
