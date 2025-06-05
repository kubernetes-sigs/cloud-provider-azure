package difftracker

import (
	"encoding/json"
	"fmt"

	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
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
func (dt *DiffTracker) DeepEqual() bool {
	// Compare Services with LoadBalancers
	if dt.K8sResources.Services.Len() != dt.NRPResources.LoadBalancers.Len() {
		klog.V(2).Infof("DiffTracker.DeepEqual: Services and LoadBalancers length mismatch")
		return false
	}
	for _, service := range dt.K8sResources.Services.UnsortedList() {
		if !dt.NRPResources.LoadBalancers.Has(service) {
			klog.V(2).Infof("DiffTracker.DeepEqual: Service %s not found in LoadBalancers\n", service)
			return false
		}
	}
	for _, service := range dt.NRPResources.LoadBalancers.UnsortedList() {
		if !dt.K8sResources.Services.Has(service) {
			klog.V(2).Infof("DiffTracker.DeepEqual: Service %s not found in Services\n", service)
			return false
		}
	}

	// Compare Egresses with NATGateways
	if dt.K8sResources.Egresses.Len() != dt.NRPResources.NATGateways.Len() {
		klog.V(2).Infof("DiffTracker.DeepEqual: Egresses and NATGateways length mismatch")
		return false
	}
	for _, egress := range dt.K8sResources.Egresses.UnsortedList() {
		if !dt.NRPResources.NATGateways.Has(egress) {
			klog.V(2).Infof("DiffTracker.DeepEqual: Egress %s not found in NATGateways\n", egress)
			return false
		}
	}
	for _, egress := range dt.NRPResources.NATGateways.UnsortedList() {
		if !dt.K8sResources.Egresses.Has(egress) {
			klog.V(2).Infof("DiffTracker.DeepEqual: Egress %s not found in Egresses\n", egress)
			return false
		}
	}

	// Compare Nodes with Locations
	if len(dt.K8sResources.Nodes) != len(dt.NRPResources.Locations) {
		klog.V(2).Infof("DiffTracker.DeepEqual: Nodes and Locations length mismatch")
		return false
	}
	for nodeKey, node := range dt.K8sResources.Nodes {
		nrpLocation, exists := dt.NRPResources.Locations[nodeKey]
		if !exists {
			klog.V(2).Infof("DiffTracker.DeepEqual: Node %s not found in Locations\n", nodeKey)
			return false
		}

		// Compare Pods with Addresses
		if len(node.Pods) != len(nrpLocation.Addresses) {
			klog.V(2).Infof("DiffTracker.DeepEqual: Pods and Addresses length mismatch for node %s\n", nodeKey)
			return false
		}
		for podKey, pod := range node.Pods {
			nrpAddress, exists := nrpLocation.Addresses[podKey]
			if !exists {
				klog.V(2).Infof("DiffTracker.DeepEqual: Pod %s not found in Addresses for node %s\n", podKey, nodeKey)
				return false
			}

			// Compare [...InboundIdentities, PublicOutboundIdentity, PrivateOutboundIdentity] with Services
			combinedIdentities := []string{}
			combinedIdentities = append(combinedIdentities, pod.InboundIdentities.UnsortedList()...)
			if pod.PublicOutboundIdentity != "" {
				combinedIdentities = append(combinedIdentities, pod.PublicOutboundIdentity)
			}
			if pod.PrivateOutboundIdentity != "" {
				combinedIdentities = append(combinedIdentities, pod.PrivateOutboundIdentity)
			}

			if len(combinedIdentities) != nrpAddress.Services.Len() {
				klog.V(2).Infof("DiffTracker.DeepEqual: Combined identities length mismatch for pod %s in node %s\n", podKey, nodeKey)
				return false
			}

			for _, identity := range combinedIdentities {
				if !nrpAddress.Services.Has(identity) {
					klog.V(2).Infof("DiffTracker.DeepEqual: Identity %s not found in Services for pod %s in node %s\n", identity, podKey, nodeKey)
					return false
				}
			}
		}
	}

	return true
}

func (syncServicesReturnType *SyncServicesReturnType) Equals(other *SyncServicesReturnType) bool {
	return syncServicesReturnType.Additions.Equals(other.Additions) && syncServicesReturnType.Removals.Equals(other.Removals)
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

// Equals compares two SyncDiffTrackerReturnType objects for equality
func (sdts *SyncDiffTrackerReturnType) Equals(other *SyncDiffTrackerReturnType) bool {
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

// Equals compares two DiffTracker objects for equality
func (dt *DiffTracker) Equals(other *DiffTracker) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	other.mu.Lock()
	defer other.mu.Unlock()

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

			if pod.PublicOutboundIdentity != otherPod.PublicOutboundIdentity {
				return false
			}

			if pod.PrivateOutboundIdentity != otherPod.PrivateOutboundIdentity {
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

// Map LocationData to LocationsDataDTO to be used as payload in ServiceGateway API calls
func MapLocationDataToDTO(locationData LocationData) LocationsDataDTO {
	var LocationsDataDTO LocationsDataDTO
	LocationsDataDTO.Action = locationData.Action
	LocationsDataDTO.Locations = []LocationDTO{}
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

		LocationsDataDTO.Locations = append(LocationsDataDTO.Locations, locationDTO)
	}

	return LocationsDataDTO
}

func RemoveBackendPoolReferenceFromServicesDTO(loadBalancerUpdates SyncServicesReturnType, subscriptionID string, resourceGroup string) ServicesDataDTO {
	var ServicesDataDTO ServicesDataDTO
	ServicesDataDTO.Action = PartialUpdate
	ServicesDataDTO.Services = []ServiceDTO{}
	for _, service := range loadBalancerUpdates.Removals.UnsortedList() {
		serviceDTO := ServiceDTO{
			Service:                  service,
			ServiceType:              Inbound,
			LoadBalancerBackendPools: []LoadBalancerBackendPoolDTO{},
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
	return ServicesDataDTO
}

// Map LoadBalancerUpdates to ServicesDataDTO to be used as payload in ServiceGateway API calls
func MapLoadBalancerUpdatesToServicesDataDTO(loadBalancerUpdates SyncServicesReturnType, subscriptionID string, resourceGroup string) ServicesDataDTO {
	var ServicesDataDTO ServicesDataDTO
	ServicesDataDTO.Action = PartialUpdate
	ServicesDataDTO.Services = []ServiceDTO{}
	for _, service := range loadBalancerUpdates.Additions.UnsortedList() {
		serviceDTO := ServiceDTO{
			Service:     service,
			ServiceType: Inbound,
			LoadBalancerBackendPools: []LoadBalancerBackendPoolDTO{
				{
					Id: fmt.Sprintf(
						consts.BackendPoolIDTemplate,
						subscriptionID,
						resourceGroup,
						service,
						fmt.Sprintf("%s-backendpool", service),
					),
				},
			},
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
	for _, service := range loadBalancerUpdates.Removals.UnsortedList() {
		serviceDTO := ServiceDTO{
			Service:  service,
			isDelete: true,
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
	return ServicesDataDTO
}

// Map NATGatewayUpdates to ServicesDataDTO to be used as payload in ServiceGateway API calls
func MapNATGatewayUpdatesToServicesDataDTO(natGatewayUpdates SyncServicesReturnType, subscriptionID string, resourceGroup string) ServicesDataDTO {
	var ServicesDataDTO ServicesDataDTO
	ServicesDataDTO.Action = PartialUpdate
	ServicesDataDTO.Services = []ServiceDTO{}
	for _, service := range natGatewayUpdates.Additions.UnsortedList() {
		serviceDTO := ServiceDTO{
			Service:     service,
			ServiceType: Outbound,
			PublicNatGateway: NatGatewayDTO{
				Id: fmt.Sprintf(
					consts.NatGatewayIDTemplate,
					subscriptionID,
					resourceGroup,
					service,
				),
			},
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
	for _, service := range natGatewayUpdates.Removals.UnsortedList() {
		serviceDTO := ServiceDTO{
			Service:  service,
			isDelete: true,
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
	return ServicesDataDTO
}
