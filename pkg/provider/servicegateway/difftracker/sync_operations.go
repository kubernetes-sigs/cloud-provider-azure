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
	"github.com/go-logr/logr"

	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// GetServicesToSync handles the synchronization of services between K8s and NRP
func GetServicesToSync(logger logr.Logger, k8sServices, nrpServices *utilsets.IgnoreCaseSet) SyncServicesReturnType {
	logger.V(5).Info("Comparing services for sync",
		"k8sCount", k8sServices.Len(), "k8sServices", k8sServices.UnsortedList(),
		"nrpCount", nrpServices.Len(), "nrpServices", nrpServices.UnsortedList())

	syncServices := SyncServicesReturnType{
		// Additions are in K8s but not yet in NRP; removals are in NRP but no
		// longer in K8s.
		Additions: k8sServices.Difference(nrpServices),
		Removals:  nrpServices.Difference(k8sServices),
	}
	logger.V(5).Info("Computed service sync sets",
		"additions", syncServices.Additions.UnsortedList(), "removals", syncServices.Removals.UnsortedList())

	logger.V(4).Info("Computed services to sync",
		"additions", syncServices.Additions.Len(), "removals", syncServices.Removals.Len())
	return syncServices
}

func (dt *DiffTracker) GetSyncLoadBalancerServices() SyncServicesReturnType {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	return dt.getSyncLoadBalancerServicesLocked()
}

// getSyncLoadBalancerServicesLocked is the lock-free body. Callers must hold dt.mu.
func (dt *DiffTracker) getSyncLoadBalancerServicesLocked() SyncServicesReturnType {
	return GetServicesToSync(dt.logger, dt.K8sResources.Services, dt.NRPResources.LoadBalancers)
}

func (dt *DiffTracker) GetSyncNRPNATGateways() SyncServicesReturnType {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	return dt.getSyncNRPNATGatewaysLocked()
}

// getSyncNRPNATGatewaysLocked is the lock-free body. Callers must hold dt.mu.
func (dt *DiffTracker) getSyncNRPNATGatewaysLocked() SyncServicesReturnType {
	return GetServicesToSync(dt.logger, dt.K8sResources.Egresses, dt.NRPResources.NATGateways)
}

func (dt *DiffTracker) GetSyncLocationsAddresses() LocationData {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	return dt.getSyncLocationsAddressesLocked()
}

// getSyncLocationsAddressesLocked is the lock-free body. Callers must hold dt.mu.
func (dt *DiffTracker) getSyncLocationsAddressesLocked() LocationData {
	result := LocationData{
		Action:    PartialUpdate,
		Locations: make(map[string]Location),
	}

	// Iterate over all nodes in the K8s state
	for nodeIP, node := range dt.K8sResources.Nodes {
		nrpLocation, locationExists := dt.NRPResources.Locations[nodeIP]
		location := initializeLocation(locationExists)
		locationUpdated := false

		for address, pod := range node.Pods {
			// Filter services: only include services that exist in NRP
			serviceRef := dt.createServiceRefFiltered(pod)

			// Check if address exists in NRP and if service list changed
			nrpAddressData, nrpAddrExists := nrpLocation.Addresses[address]

			// Skip this address if:
			// 1. No ready services AND address doesn't exist in NRP (nothing to sync)
			// 2. ServiceRef matches what's already in NRP (no change)
			if serviceRef.Len() == 0 && !nrpAddrExists {
				continue
			}

			if nrpAddrExists && serviceRef.Equals(nrpAddressData.Services) {
				continue
			}

			// ServiceRef changed (or address is new) - need to sync
			addressData := Address{ServiceRef: serviceRef}
			location.Addresses[address] = addressData
			locationUpdated = true
		}
		if locationUpdated {
			result.Locations[nodeIP] = location
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

// createServiceRefFiltered creates ServiceRef but only includes services that exist in NRP.
// Must be called with dt.mu held.
func (dt *DiffTracker) createServiceRefFiltered(pod Pod) *utilsets.IgnoreCaseSet {
	serviceRef := utilsets.NewString()

	// Check inbound services (LoadBalancers)
	for _, serviceUID := range pod.InboundIdentities.UnsortedList() {
		if dt.isServiceReadyToSync(serviceUID, true) {
			serviceRef.Insert(serviceUID)
		}
	}

	// Check outbound service (NAT Gateway)
	if pod.PublicOutboundIdentity != "" {
		if dt.isServiceReadyToSync(pod.PublicOutboundIdentity, false) {
			serviceRef.Insert(pod.PublicOutboundIdentity)
		}
	}

	return serviceRef
}

// isServiceReadyToSync reports whether a service is ready to be synced to the
// Service Gateway, i.e. its NRP resource exists. Must be called with dt.mu held.
func (dt *DiffTracker) isServiceReadyToSync(serviceUID string, isInbound bool) bool {
	if isInbound {
		return dt.NRPResources.LoadBalancers.Has(serviceUID)
	}
	return dt.NRPResources.NATGateways.Has(serviceUID)
}

// findLocationData returns a pointer to the Location stored under the given key
// in data, or nil if no such location exists.
func findLocationData(data LocationData, location string) *Location {
	if loc, ok := data.Locations[location]; ok {
		return &loc
	}
	return nil
}

func (dt *DiffTracker) GetSyncOperations() *SyncDiffTrackerReturnType {
	// Take the lock once so DeepEqual and all three sync computations observe a
	// single consistent snapshot of the state (avoids a data race with mutating
	// methods and inconsistency between the individual GetSync* results).
	dt.mu.Lock()
	defer dt.mu.Unlock()

	if dt.deepEqualLocked() {
		return &SyncDiffTrackerReturnType{SyncStatus: AlreadyInSync}
	}

	return &SyncDiffTrackerReturnType{
		SyncStatus:          Success,
		LoadBalancerUpdates: dt.getSyncLoadBalancerServicesLocked(),
		NATGatewayUpdates:   dt.getSyncNRPNATGatewaysLocked(),
		LocationData:        dt.getSyncLocationsAddressesLocked(),
	}
}
