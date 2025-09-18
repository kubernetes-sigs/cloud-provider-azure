package armnetwork

// ServiceGatewayAddressLocationAddressUpdateAction - Specifies the type of update operation to perform on addresses within
// the address location of service gateway.
// * FullUpdate: Replaces all existing address data with the new list provided in the request. Any previously defined addresses
// not included will be removed.
// * PartialUpdate: Updates only the specified addresses.
type ServiceGatewayAddressLocationAddressUpdateAction string

const (
	ServiceGatewayAddressLocationAddressUpdateActionFullUpdate    ServiceGatewayAddressLocationAddressUpdateAction = "FullUpdate"
	ServiceGatewayAddressLocationAddressUpdateActionPartialUpdate ServiceGatewayAddressLocationAddressUpdateAction = "PartialUpdate"
)

// PossibleServiceGatewayAddressLocationAddressUpdateActionValues returns the possible values for the ServiceGatewayAddressLocationAddressUpdateAction const type.
func PossibleServiceGatewayAddressLocationAddressUpdateActionValues() []ServiceGatewayAddressLocationAddressUpdateAction {
	return []ServiceGatewayAddressLocationAddressUpdateAction{
		ServiceGatewayAddressLocationAddressUpdateActionFullUpdate,
		ServiceGatewayAddressLocationAddressUpdateActionPartialUpdate,
	}
}

// ServiceGatewayServicePropertiesFormatServiceType - Name of the service.
type ServiceGatewayServicePropertiesFormatServiceType string

const (
	ServiceGatewayServicePropertiesFormatServiceTypeInbound         ServiceGatewayServicePropertiesFormatServiceType = "Inbound"
	ServiceGatewayServicePropertiesFormatServiceTypeInboundOutbound ServiceGatewayServicePropertiesFormatServiceType = "InboundOutbound"
	ServiceGatewayServicePropertiesFormatServiceTypeOutbound        ServiceGatewayServicePropertiesFormatServiceType = "Outbound"
)

// PossibleServiceGatewayServicePropertiesFormatServiceTypeValues returns the possible values for the ServiceGatewayServicePropertiesFormatServiceType const type.
func PossibleServiceGatewayServicePropertiesFormatServiceTypeValues() []ServiceGatewayServicePropertiesFormatServiceType {
	return []ServiceGatewayServicePropertiesFormatServiceType{
		ServiceGatewayServicePropertiesFormatServiceTypeInbound,
		ServiceGatewayServicePropertiesFormatServiceTypeInboundOutbound,
		ServiceGatewayServicePropertiesFormatServiceTypeOutbound,
	}
}

// ServiceGatewayUpdateAddressLocationsRequestAction - Specifies the type of update operation to perform on address locations
// within the service gateway.
// * FullUpdate: Replaces all existing address location data with the new list provided in the request. Any previously defined
// locations not included will be removed.
// * PartialUpdate: Updates only the specified address locations.
type ServiceGatewayUpdateAddressLocationsRequestAction string

const (
	ServiceGatewayUpdateAddressLocationsRequestActionFullUpdate    ServiceGatewayUpdateAddressLocationsRequestAction = "FullUpdate"
	ServiceGatewayUpdateAddressLocationsRequestActionPartialUpdate ServiceGatewayUpdateAddressLocationsRequestAction = "PartialUpdate"
)

// PossibleServiceGatewayUpdateAddressLocationsRequestActionValues returns the possible values for the ServiceGatewayUpdateAddressLocationsRequestAction const type.
func PossibleServiceGatewayUpdateAddressLocationsRequestActionValues() []ServiceGatewayUpdateAddressLocationsRequestAction {
	return []ServiceGatewayUpdateAddressLocationsRequestAction{
		ServiceGatewayUpdateAddressLocationsRequestActionFullUpdate,
		ServiceGatewayUpdateAddressLocationsRequestActionPartialUpdate,
	}
}

// ServiceGatewayUpdateServicesRequestAction - Specifies the type of update operation to perform on services within the service
// gateway.
// * FullUpdate: Replaces all existing services with the new list provided in the request. Any previously defined services
// not included will be removed.
// * PartialUpdate: Updates only the specified services.
type ServiceGatewayUpdateServicesRequestAction string

const (
	ServiceGatewayUpdateServicesRequestActionFullUpdate    ServiceGatewayUpdateServicesRequestAction = "FullUpdate"
	ServiceGatewayUpdateServicesRequestActionPartialUpdate ServiceGatewayUpdateServicesRequestAction = "PartialUpdate"
)

// PossibleServiceGatewayUpdateServicesRequestActionValues returns the possible values for the ServiceGatewayUpdateServicesRequestAction const type.
func PossibleServiceGatewayUpdateServicesRequestActionValues() []ServiceGatewayUpdateServicesRequestAction {
	return []ServiceGatewayUpdateServicesRequestAction{
		ServiceGatewayUpdateServicesRequestActionFullUpdate,
		ServiceGatewayUpdateServicesRequestActionPartialUpdate,
	}
}
