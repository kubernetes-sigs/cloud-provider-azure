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
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func (operation Operation) String() string {
	switch operation {
	case UnknownOperation:
		return "UnknownOperation"
	case Add:
		return "Add"
	case Remove:
		return "Remove"
	case Update:
		return "Update"
	default:
		return fmt.Sprintf("Operation(%d)", int(operation))
	}
}

func (operation Operation) MarshalJSON() ([]byte, error) {
	return json.Marshal(operation.String())
}

func (updateAction UpdateAction) String() string {
	switch updateAction {
	case UnknownUpdateAction:
		return "UnknownUpdateAction"
	case PartialUpdate:
		return "PartialUpdate"
	case FullUpdate:
		return "FullUpdate"
	default:
		return fmt.Sprintf("UpdateAction(%d)", int(updateAction))
	}
}

func (updateAction UpdateAction) MarshalJSON() ([]byte, error) {
	return json.Marshal(updateAction.String())
}

func (updateAction *UpdateAction) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}

	switch s {
	case "PartialUpdate":
		*updateAction = PartialUpdate
	case "FullUpdate":
		*updateAction = FullUpdate
	default:
		return fmt.Errorf("unknown UpdateAction: %q", s)
	}

	return nil
}

func (syncStatus SyncStatus) String() string {
	switch syncStatus {
	case UnknownSyncStatus:
		return "UnknownSyncStatus"
	case AlreadyInSync:
		return "AlreadyInSync"
	case Success:
		return "Success"
	default:
		return fmt.Sprintf("SyncStatus(%d)", int(syncStatus))
	}
}

func (syncStatus SyncStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(syncStatus.String())
}

func (node *Node) HasPods() bool { return len(node.Pods) > 0 }

func (pod *Pod) HasIdentities() bool {
	return pod.InboundIdentities.Len() > 0 || pod.PublicOutboundIdentity != ""
}

// deepEqualLocked compares the K8s and NRP states to check if they are in sync.
// Callers must hold dt.mu.
func (dt *DiffTracker) deepEqualLocked() bool {
	dt.logger.V(5).Info("Comparing K8s and NRP state for equality",
		"k8sServices", dt.K8sResources.Services.Len(), "nrpLoadBalancers", dt.NRPResources.LoadBalancers.Len(),
		"k8sEgresses", dt.K8sResources.Egresses.Len(), "nrpNATGateways", dt.NRPResources.NATGateways.Len(),
		"k8sNodes", len(dt.K8sResources.Nodes), "nrpLocations", len(dt.NRPResources.Locations))

	// Compare Services with LoadBalancers and Egresses with NATGateways.
	if !dt.K8sResources.Services.Equals(dt.NRPResources.LoadBalancers) {
		dt.logger.V(4).Info("Services did not match LoadBalancers")
		return false
	}
	if !dt.K8sResources.Egresses.Equals(dt.NRPResources.NATGateways) {
		dt.logger.V(4).Info("Egresses did not match NATGateways")
		return false
	}

	// Compare Nodes with Locations.
	if len(dt.K8sResources.Nodes) != len(dt.NRPResources.Locations) {
		dt.logger.V(4).Info("Node count did not match Location count")
		return false
	}
	for nodeKey, node := range dt.K8sResources.Nodes {
		nrpLocation, exists := dt.NRPResources.Locations[nodeKey]
		if !exists {
			dt.logger.V(4).Info("Could not find node in Locations", "node", nodeKey)
			return false
		}

		// Compare Pods with Addresses.
		if len(node.Pods) != len(nrpLocation.Addresses) {
			dt.logger.V(4).Info("Pod count did not match Address count for node", "node", nodeKey)
			return false
		}
		for podKey, pod := range node.Pods {
			nrpAddress, exists := nrpLocation.Addresses[podKey]
			if !exists {
				dt.logger.V(4).Info("Could not find pod in Addresses for node", "pod", podKey, "node", nodeKey)
				return false
			}

			// Compare [...InboundIdentities, PublicOutboundIdentity] with Services.
			combinedIdentities := utilsets.NewString(pod.InboundIdentities.UnsortedList()...)
			if pod.PublicOutboundIdentity != "" {
				combinedIdentities.Insert(pod.PublicOutboundIdentity)
			}
			if !combinedIdentities.Equals(nrpAddress.Services) {
				dt.logger.V(4).Info("Identities did not match Services for pod", "pod", podKey, "node", nodeKey)
				return false
			}
		}
	}

	return true
}

func (s *SyncServicesReturnType) Equals(other *SyncServicesReturnType) bool {
	return s.Additions.Equals(other.Additions) && s.Removals.Equals(other.Removals)
}

// Equals compares two LocationData objects for equality
func (ld *LocationData) Equals(other *LocationData) bool {
	if ld.Action != other.Action {
		return false
	}

	if len(ld.Locations) != len(other.Locations) {
		return false
	}

	for locName, location := range ld.Locations {
		otherLocation, exists := other.Locations[locName]
		if !exists {
			return false
		}

		if location.AddressUpdateAction != otherLocation.AddressUpdateAction {
			return false
		}

		if len(location.Addresses) != len(otherLocation.Addresses) {
			return false
		}

		for addrName, address := range location.Addresses {
			otherAddress, exists := otherLocation.Addresses[addrName]
			if !exists {
				return false
			}

			if !address.ServiceRef.Equals(otherAddress.ServiceRef) {
				return false
			}
		}
	}

	return true
}

// Equals compares two SyncDiffTrackerReturnType objects for equality
func (s *SyncDiffTrackerReturnType) Equals(other *SyncDiffTrackerReturnType) bool {
	if s.SyncStatus != other.SyncStatus {
		return false
	}

	if !s.LoadBalancerUpdates.Additions.Equals(other.LoadBalancerUpdates.Additions) {
		return false
	}

	if !s.LoadBalancerUpdates.Removals.Equals(other.LoadBalancerUpdates.Removals) {
		return false
	}

	if !s.NATGatewayUpdates.Additions.Equals(other.NATGatewayUpdates.Additions) {
		return false
	}

	if !s.NATGatewayUpdates.Removals.Equals(other.NATGatewayUpdates.Removals) {
		return false
	}

	if !s.LocationData.Equals(&other.LocationData) {
		return false
	}

	return true
}

// Equals compares two DiffTracker objects for equality
func (dt *DiffTracker) Equals(other *DiffTracker) bool {
	// Lock both trackers in a consistent order (by pointer address) so that
	// concurrent dt.Equals(other) and other.Equals(dt) calls can't deadlock.
	// If both refer to the same object, lock only once to avoid self-deadlock.
	first, second := dt, other
	if reflect.ValueOf(first).Pointer() > reflect.ValueOf(second).Pointer() {
		first, second = second, first
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	if second != first {
		second.mu.Lock()
		defer second.mu.Unlock()
	}

	if !dt.K8sResources.Services.Equals(other.K8sResources.Services) {
		return false
	}

	if !dt.K8sResources.Egresses.Equals(other.K8sResources.Egresses) {
		return false
	}

	if len(dt.K8sResources.Nodes) != len(other.K8sResources.Nodes) {
		return false
	}

	for nodeKey, node := range dt.K8sResources.Nodes {
		otherNode, exists := other.K8sResources.Nodes[nodeKey]
		if !exists {
			return false
		}

		if len(node.Pods) != len(otherNode.Pods) {
			return false
		}

		for podKey, pod := range node.Pods {
			otherPod, exists := otherNode.Pods[podKey]
			if !exists {
				return false
			}

			if !pod.InboundIdentities.Equals(otherPod.InboundIdentities) {
				return false
			}

			if !strings.EqualFold(pod.PublicOutboundIdentity, otherPod.PublicOutboundIdentity) {
				return false
			}
		}
	}

	// Compare NRP state
	if !dt.NRPResources.LoadBalancers.Equals(other.NRPResources.LoadBalancers) {
		return false
	}

	if !dt.NRPResources.NATGateways.Equals(other.NRPResources.NATGateways) {
		return false
	}

	if len(dt.NRPResources.Locations) != len(other.NRPResources.Locations) {
		return false
	}

	for location, nrpLocation := range dt.NRPResources.Locations {
		otherNrpLocation, exists := other.NRPResources.Locations[location]
		if !exists {
			return false
		}

		if len(nrpLocation.Addresses) != len(otherNrpLocation.Addresses) {
			return false
		}

		for address, nrpAddress := range nrpLocation.Addresses {
			otherNrpAddress, exists := otherNrpLocation.Addresses[address]
			if !exists {
				return false
			}

			if !nrpAddress.Services.Equals(otherNrpAddress.Services) {
				return false
			}
		}
	}

	return true
}

// addressExists reports whether an address with the given key exists in a location.
func addressExists(location NRPLocation, addressKey string) bool {
	_, exists := location.Addresses[addressKey]
	return exists
}

// createServiceRefsFromAddress returns a copy of the address's service references.
func createServiceRefsFromAddress(addressValue Address) *utilsets.IgnoreCaseSet {
	serviceRefs := utilsets.NewString()
	for _, service := range addressValue.ServiceRef.UnsortedList() {
		serviceRefs.Insert(service)
	}
	return serviceRefs
}
