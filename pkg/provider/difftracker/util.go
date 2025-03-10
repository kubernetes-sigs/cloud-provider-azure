package difftracker

import (
	"encoding/json"
	"fmt"

	"k8s.io/klog/v2"
)

func (operation Operation) String() string {
	return [...]string{"ADD", "REMOVE", "UPDATE"}[operation]
}

func (operation Operation) MarshalJSON() ([]byte, error) {
	return json.Marshal(operation.String())
}

func (updateAction UpdateAction) String() string {
	return [...]string{"PartialUpdate", "FullUpdate"}[updateAction]
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
	return [...]string{"ALREADY_IN_SYNC", "SUCCESS"}[syncStatus]
}

func (syncStatus SyncStatus) MarshalJSON() ([]byte, error) {
	return json.Marshal(syncStatus.String())
}

func (node *Node) HasPods() bool { return len(node.Pods) > 0 }

func (pod *Pod) HasIdentities() bool {
	return pod.InboundIdentities.Len() > 0 || pod.PublicOutboundIdentity != "" || pod.PrivateOutboundIdentity != ""
}

// DeepEqual compares the K8s and NRP states to check if they are in sync
func (dt *DiffTrackerState) DeepEqual() bool {
	// Compare Services with LoadBalancers
	if dt.K8s.Services.Len() != dt.NRP.LoadBalancers.Len() {
		klog.Errorf("Services and LoadBalancers length mismatch")
		return false
	}
	for _, service := range dt.K8s.Services.UnsortedList() {
		if !dt.NRP.LoadBalancers.Has(service) {
			klog.Errorf("Service %s not found in LoadBalancers\n", service)
			return false
		}
	}
	for _, service := range dt.NRP.LoadBalancers.UnsortedList() {
		if !dt.K8s.Services.Has(service) {
			klog.Errorf("Service %s not found in Services\n", service)
			return false
		}
	}

	// Compare Egresses with NATGateways
	if dt.K8s.Egresses.Len() != dt.NRP.NATGateways.Len() {
		klog.Errorf("Egresses and NATGateways length mismatch")
		return false
	}
	for _, egress := range dt.K8s.Egresses.UnsortedList() {
		if !dt.NRP.NATGateways.Has(egress) {
			klog.Errorf("Egress %s not found in NATGateways\n", egress)
			return false
		}
	}
	for _, egress := range dt.NRP.NATGateways.UnsortedList() {
		if !dt.K8s.Egresses.Has(egress) {
			klog.Errorf("Egress %s not found in Egresses\n", egress)
			return false
		}
	}

	// Compare Nodes with NRPLocations
	if len(dt.K8s.Nodes) != len(dt.NRP.NRPLocations) {
		klog.Errorf("Nodes and NRPLocations length mismatch")
		return false
	}
	for nodeKey, node := range dt.K8s.Nodes {
		nrpLocation, exists := dt.NRP.NRPLocations[nodeKey]
		if !exists {
			klog.Errorf("Node %s not found in NRPLocations\n", nodeKey)
			return false
		}

		// Compare Pods with NRPAddresses
		if len(node.Pods) != len(nrpLocation.NRPAddresses) {
			klog.Errorf("Pods and NRPAddresses length mismatch for node %s\n", nodeKey)
			return false
		}
		for podKey, pod := range node.Pods {
			nrpAddress, exists := nrpLocation.NRPAddresses[podKey]
			if !exists {
				klog.Errorf("Pod %s not found in NRPAddresses for node %s\n", podKey, nodeKey)
				return false
			}

			// Compare [...InboundIdentities, PublicOutboundIdentity, PrivateOutboundIdentity] with NRPServices
			combinedIdentities := []string{}
			combinedIdentities = append(combinedIdentities, pod.InboundIdentities.UnsortedList()...)
			if pod.PublicOutboundIdentity != "" {
				combinedIdentities = append(combinedIdentities, pod.PublicOutboundIdentity)
			}
			if pod.PrivateOutboundIdentity != "" {
				combinedIdentities = append(combinedIdentities, pod.PrivateOutboundIdentity)
			}

			if len(combinedIdentities) != nrpAddress.NRPServices.Len() {
				klog.Errorf("Combined identities length mismatch for pod %s in node %s\n", podKey, nodeKey)
				return false
			}

			for _, identity := range combinedIdentities {
				if !nrpAddress.NRPServices.Has(identity) {
					klog.Errorf("Identity %s not found in NRPServices for pod %s in node %s\n", identity, podKey, nodeKey)
					return false
				}
			}
		}
	}

	return true
}

func (syncNRPServicesReturnType *SyncNRPServicesReturnType) Equals(other *SyncNRPServicesReturnType) bool {
	return syncNRPServicesReturnType.Additions.Equals(other.Additions) && syncNRPServicesReturnType.Removals.Equals(other.Removals)
}

// Equals compares two LocationData objects for equality
func (ld *LocationData) Equals(other *LocationData) bool {
	if ld.Action != other.Action {
		return false
	}

	// Both maps should have the same number of locations
	if len(ld.Locations) != len(other.Locations) {
		return false
	}

	// Compare each location in both maps
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

		// Compare each address
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

// Equals compares two SyncDiffTrackerStateReturnType objects for equality
func (sdts *SyncDiffTrackerStateReturnType) Equals(other *SyncDiffTrackerStateReturnType) bool {
	if sdts.SyncStatus != other.SyncStatus {
		return false
	}

	if !sdts.LoadBalancerUpdates.Additions.Equals(other.LoadBalancerUpdates.Additions) {
		return false
	}

	if !sdts.LoadBalancerUpdates.Removals.Equals(other.LoadBalancerUpdates.Removals) {
		return false
	}

	if !sdts.NATGatewayUpdates.Additions.Equals(other.NATGatewayUpdates.Additions) {
		return false
	}

	if !sdts.NATGatewayUpdates.Removals.Equals(other.NATGatewayUpdates.Removals) {
		return false
	}

	if !sdts.LocationData.Equals(&other.LocationData) {
		return false
	}

	return true
}

// Equals compares two DiffTrackerState objects for equality
func (dt *DiffTrackerState) Equals(other *DiffTrackerState) bool {
	if !dt.K8s.Services.Equals(other.K8s.Services) {
		return false
	}

	if !dt.K8s.Egresses.Equals(other.K8s.Egresses) {
		return false
	}

	if len(dt.K8s.Nodes) != len(other.K8s.Nodes) {
		return false
	}

	for nodeKey, node := range dt.K8s.Nodes {
		otherNode, exists := other.K8s.Nodes[nodeKey]
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

			if pod.PublicOutboundIdentity != otherPod.PublicOutboundIdentity {
				return false
			}

			if pod.PrivateOutboundIdentity != otherPod.PrivateOutboundIdentity {
				return false
			}
		}
	}

	// Compare NRP state
	if !dt.NRP.LoadBalancers.Equals(other.NRP.LoadBalancers) {
		return false
	}

	if !dt.NRP.NATGateways.Equals(other.NRP.NATGateways) {
		return false
	}

	if len(dt.NRP.NRPLocations) != len(other.NRP.NRPLocations) {
		return false
	}

	for location, nrpLocation := range dt.NRP.NRPLocations {
		otherNrpLocation, exists := other.NRP.NRPLocations[location]
		if !exists {
			return false
		}

		if len(nrpLocation.NRPAddresses) != len(otherNrpLocation.NRPAddresses) {
			return false
		}

		for address, nrpAddress := range nrpLocation.NRPAddresses {
			otherNrpAddress, exists := otherNrpLocation.NRPAddresses[address]
			if !exists {
				return false
			}

			if !nrpAddress.NRPServices.Equals(otherNrpAddress.NRPServices) {
				return false
			}
		}
	}

	return true
}

// Map LocationData to LocationDataDTO to be used as payload in ServiceGateway API calls
func mapLocationDataToDTO(locationData LocationData) LocationDataDTO {
	var locationDataDTO LocationDataDTO
	locationDataDTO.Action = locationData.Action
	locationDataDTO.Locations = []LocationDTO{}
	for locKey, loc := range locationData.Locations {
		var locationDTO LocationDTO
		locationDTO.Location = locKey
		locationDTO.AddressUpdateAction = loc.AddressUpdateAction
		locationDTO.Addresses = []AddressDTO{}
		for addrKey, addr := range loc.Addresses {
			addressDTO := AddressDTO{
				Address:      addrKey,
				ServiceNames: addr.ServiceRef,
			}
			locationDTO.Addresses = append(locationDTO.Addresses, addressDTO)
		}

		locationDataDTO.Locations = append(locationDataDTO.Locations, locationDTO)
	}

	return locationDataDTO
}
