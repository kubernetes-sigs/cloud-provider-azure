package difftracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// extractAzureErrorInfo extracts HTTP status code and a canonical error code from an Azure SDK error.
// Maps Azure error codes to the canonical codes defined in slb-observability-guide.md Section 7:
//   - "quota_exceeded": Azure subscription quota limit reached (400/403)
//   - "throttled": Too many Azure API requests (429)
//   - "conflict": Resource already exists or is being modified (409)
//   - "internal_error": Azure internal error (5xx)
//   - "timeout": Operation timed out
//   - "not_found": Resource was deleted externally (404)
//   - "unknown": Unclassified error
//
// Returns (0, "") for nil errors.
func extractAzureErrorInfo(err error) (httpStatus int, errorCode string) {
	if err == nil {
		return 0, ""
	}

	// Check for Azure SDK ResponseError
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		httpStatus = respErr.StatusCode
		azureCode := respErr.ErrorCode

		// Map Azure error codes to canonical codes
		switch {
		case strings.EqualFold(azureCode, "QuotaExceeded") ||
			strings.EqualFold(azureCode, "PublicIPCountLimitReached") ||
			strings.EqualFold(azureCode, "MaxPublicIpCountLimitReached"):
			return httpStatus, "quota_exceeded"
		case httpStatus == 429:
			return httpStatus, "throttled"
		case httpStatus == 409:
			return httpStatus, "conflict"
		case httpStatus == 404:
			return httpStatus, "not_found"
		case httpStatus >= 500:
			return httpStatus, "internal_error"
		default:
			return httpStatus, "unknown"
		}
	}

	// Check for timeout errors
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return 0, "timeout"
	}

	// Fallback: check error message for timeout indicators
	errMsg := err.Error()
	if strings.Contains(errMsg, "context deadline exceeded") ||
		strings.Contains(errMsg, "context canceled") {
		return 0, "timeout"
	}

	return 0, "unknown"
}

// serviceOverlayMappingsErrorCode is the NRP ServiceGateway error code returned when an
// inbound service is unregistered (deleted) while its pod overlay address mappings are
// still present in NRP. The service entry cannot be removed until those address mappings
// have been drained from the address locations first.
const serviceOverlayMappingsErrorCode = "ServiceWithOverlayMappingsCannotBeDeleted"

// isServiceOverlayMappingsError reports whether err is the NRP ServiceGateway rejection
// that a service still has overlay address mappings and therefore cannot be deleted yet.
// NRP carries the code in the response body (azcore surfaces the body in Error()), so we
// match both the typed ErrorCode and the raw error text.
func isServiceOverlayMappingsError(err error) bool {
	if err == nil {
		return false
	}
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) && strings.EqualFold(respErr.ErrorCode, serviceOverlayMappingsErrorCode) {
		return true
	}
	return strings.Contains(err.Error(), serviceOverlayMappingsErrorCode)
}

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

func MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(loadBalancerUpdates SyncServicesReturnType, natGatewayUpdates SyncServicesReturnType, subscriptionID string, resourceGroup string) ServicesDataDTO {
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
						service,
					),
				},
			},
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
	for _, service := range loadBalancerUpdates.Removals.UnsortedList() {
		serviceDTO := ServiceDTO{
			Service:     service,
			IsDelete:    true,
			ServiceType: Inbound,
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
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
			Service:     service,
			IsDelete:    true,
			ServiceType: Outbound,
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
	return ServicesDataDTO
}
