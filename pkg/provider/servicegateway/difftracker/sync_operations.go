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
	"slices"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/sets"

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
	return dt.getSyncServicesLocked(dt.K8sResources.Services, dt.NRPResources.LoadBalancers)
}

func (dt *DiffTracker) GetSyncNRPNATGateways() SyncServicesReturnType {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	return dt.getSyncNRPNATGatewaysLocked()
}

// getSyncNRPNATGatewaysLocked is the lock-free body. Callers must hold dt.mu.
func (dt *DiffTracker) getSyncNRPNATGatewaysLocked() SyncServicesReturnType {
	return dt.getSyncServicesLocked(dt.K8sResources.Egresses, dt.NRPResources.NATGateways)
}

func (dt *DiffTracker) getSyncServicesLocked(desired, registered *utilsets.IgnoreCaseSet) SyncServicesReturnType {
	result := GetServicesToSync(dt.logger, desired, registered)
	result.Additions.Delete(DefaultOutboundNATGatewayName)
	result.Removals.Delete(DefaultOutboundNATGatewayName)
	return result
}

func (dt *DiffTracker) GetSyncLocationsAddresses() LocationData {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	return dt.getSyncLocationsAddressesLocked()
}

// Recompute affected locations under the current policy before recording the request.
func (dt *DiffTracker) applyNICPoliciesToLocationSync(baseline LocationData, preparation map[string]*nsgPreparedNIC) LocationData {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	locations := sets.New[string]()
	for location := range baseline.Locations {
		locations.Insert(location)
	}
	for location := range dt.dirtyNICPolicies {
		locations.Insert(location)
	}
	for location := range dt.pendingLocationUpdates {
		locations.Insert(location)
	}
	if dt.locationRecoveryPending {
		for location := range dt.K8sResources.Nodes {
			locations.Insert(location)
		}
	}
	for location, previous := range dt.NRPResources.Locations {
		if dt.locationRecoveryPending || previous.nodeOwner != nil && previous.nodeOwner.draining {
			locations.Insert(location)
		}
	}
	result := dt.getSyncNICPolicyLocationsLocked(preparation, locations)
	dt.noteLocationAttemptLocked(result, preparation)
	return result
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
			// Filter services: only include services that are StateCreated or exist in NRP
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

func (dt *DiffTracker) getSyncNICPolicyLocationsLocked(preparation map[string]*nsgPreparedNIC, locations sets.Set[string]) LocationData {
	result := LocationData{
		Action:    PartialUpdate,
		Locations: make(map[string]Location),
	}
	if dt.locationRecoveryPending {
		result.Action = FullUpdate
	}
	for location := range locations {
		node, nodeExists := dt.K8sResources.Nodes[location]
		previous, known := dt.NRPResources.Locations[location]
		pending, uncertain := dt.pendingLocationUpdates[location]
		full := result.Action == FullUpdate || uncertain || !known
		change := Location{AddressUpdateAction: PartialUpdate, Addresses: make(map[string]Address)}
		if full {
			change.AddressUpdateAction = FullUpdate
		}
		hasTarget := false
		visitAddress := func(address string, pod Pod) {
			refs := dt.createServiceRefFiltered(pod)
			for _, uid := range pod.InboundIdentities.UnsortedList() {
				if refs.Has(uid) && !dt.canPublishInboundLocked(preparation, location, address, uid) {
					refs.Delete(uid)
				}
			}
			for _, uid := range previous.Addresses[address].Services.UnsortedList() {
				if dt.isReservedServiceLocked(uid) {
					refs.Insert(uid)
				}
			}
			hasTarget = hasTarget || refs.Len() > 0
			if full {
				if refs.Len() > 0 {
					change.Addresses[address] = Address{ServiceRef: refs}
				}
			} else if !refs.Equals(previous.Addresses[address].Services) {
				change.Addresses[address] = Address{ServiceRef: refs}
			}
		}
		for address, pod := range node.Pods {
			visitAddress(address, pod)
		}
		for address := range previous.Addresses {
			if _, desired := node.Pods[address]; !desired {
				visitAddress(address, Pod{})
			}
		}
		if uncertain {
			for address := range pending.Addresses {
				_, desired := node.Pods[address]
				_, acknowledged := previous.Addresses[address]
				if !desired && !acknowledged {
					visitAddress(address, Pod{})
				}
			}
		}
		if !hasTarget && result.Action == FullUpdate {
			continue
		}
		if !hasTarget && (uncertain || !nodeExists) {
			if known || uncertain {
				result.Locations[location] = Location{AddressUpdateAction: PartialUpdate, Addresses: make(map[string]Address)}
			}
		} else if len(change.Addresses) > 0 {
			result.Locations[location] = change
		}
	}
	return result
}

func (dt *DiffTracker) noteLocationAttemptLocked(data LocationData, preparation map[string]*nsgPreparedNIC) {
	for location, change := range data.Locations {
		pending := dt.pendingLocationUpdates[location]
		if pending.nodeOwner == nil {
			pending.nodeOwner = dt.NRPResources.Locations[location].nodeOwner
		}
		if plan := preparation[location]; pending.nodeOwner == nil && plan != nil && !plan.lbOnly && slices.Contains(plan.currentLocations, location) {
			owner := plan.owner
			pending.nodeOwner = &owner
		}
		if pending.Addresses == nil {
			pending.Addresses = make(map[string]NRPAddress)
		}
		for address, value := range change.Addresses {
			additions := value.ServiceRef.Difference(dt.NRPResources.Locations[location].Addresses[address].Services)
			if additions.Len() == 0 {
				continue
			}
			known := pending.Addresses[address]
			if known.Services == nil {
				known.Services = utilsets.NewString()
			}
			for _, uid := range additions.UnsortedList() {
				known.Services.Insert(uid)
			}
			pending.Addresses[address] = known
		}
		dt.pendingLocationUpdates[location] = pending
	}
}

func (dt *DiffTracker) completeLocationSyncLocked(data LocationData) {
	if data.Action == FullUpdate {
		clear(dt.pendingLocationUpdates)
		dt.locationRecoveryPending = false
	}
	for location := range data.Locations {
		delete(dt.pendingLocationUpdates, location)
	}
}

// countTrackedLocationsAndAddresses returns the total number of locations (nodes) and pod IP
// addresses currently tracked in NRP. These back the locations_total / addresses_total gauges,
// which are documented and alerted on as live totals (not per-sync diff sizes). It acquires
// dt.mu, so callers must not already hold it.
func (dt *DiffTracker) countTrackedLocationsAndAddresses() (locations int, addresses int) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	locations = len(dt.NRPResources.Locations)
	for _, loc := range dt.NRPResources.Locations {
		addresses += len(loc.Addresses)
	}
	return locations, addresses
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

// createServiceRefFiltered creates ServiceRef but only includes services that are StateCreated
// This prevents LocationsUpdater from trying to sync locations for services still being created
// Must be called with dt.mu held
func (dt *DiffTracker) createServiceRefFiltered(pod Pod) *utilsets.IgnoreCaseSet {
	serviceRef := utilsets.NewString()

	// Check inbound services (LoadBalancers)
	for _, serviceName := range pod.InboundIdentities.UnsortedList() {
		if dt.isServiceReadyToSync(serviceName, true) {
			serviceRef.Insert(serviceName)
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

// isServiceReadyToSync reports whether a service is ready for location sync. A service is ready when
// its LB/NAT is live in NRP, even if its engine op is parked (e.g. StateNotStarted after a terminal
// update) or still being created, so the live resource keeps tracking endpoint changes instead of
// having its backend pool drained. A service with no NRP resource yet is not synced.
// Must be called with dt.mu held
func (dt *DiffTracker) isServiceReadyToSync(serviceName string, isInbound bool) bool {
	existsInNRP := (isInbound && dt.NRPResources.LoadBalancers.Has(serviceName)) ||
		(!isInbound && dt.NRPResources.NATGateways.Has(serviceName))

	if opState, exists := dt.pendingServiceOps[serviceName]; exists {
		// The key is shared by inbound (Service UID) and outbound (egress label) identities, so this
		// entry may belong to the other kind. Answering from it would let an egress identity ride an
		// inbound service's readiness. Fall back to the NRP resource of the kind asked about.
		if opState.Config.IsInbound != isInbound {
			return existsInNRP
		}
		switch opState.State {
		case StateCreated, StateUpdateInProgress:
			return true
		case StateNotStarted, StateCreationInProgress:
			// A live LB parked after a terminal update, or one still being created, must keep
			// syncing its backends rather than have them drained; a not-yet-provisioned service
			// has no NRP resource and is not synced.
			return existsInNRP
		default:
			// Deletion states: addresses should drain, not sync.
			return false
		}
	}

	// Service not tracked - ready iff it exists in NRP (created outside the Engine).
	return existsInNRP
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
