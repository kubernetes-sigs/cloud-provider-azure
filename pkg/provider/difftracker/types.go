package difftracker

import (
	"sync"

	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// ================================================================================================
// ENUMS
// ================================================================================================
type Operation int

const (
	ADD Operation = iota
	REMOVE
	UPDATE
)

type UpdateAction int

const (
	PartialUpdate UpdateAction = iota
	FullUpdate
)

type SyncStatus int

const (
	ALREADY_IN_SYNC SyncStatus = iota
	SUCCESS
)

// --------------------------------------------------------------------------------
// DiffTracker keeps track of the state of the K8s cluster and NRP
// --------------------------------------------------------------------------------
type NRPAddress struct {
	Services *utilsets.IgnoreCaseSet // all inbound and outbound identities
}

type NRPLocation struct {
	Addresses map[string]NRPAddress
}

type NRP struct {
	LoadBalancers *utilsets.IgnoreCaseSet
	NATGateways   *utilsets.IgnoreCaseSet
	Locations     map[string]NRPLocation
}

type Pod struct {
	InboundIdentities       *utilsets.IgnoreCaseSet
	PublicOutboundIdentity  string
	PrivateOutboundIdentity string
}

type Node struct {
	Pods map[string]Pod
}

type K8s struct {
	Services *utilsets.IgnoreCaseSet
	Egresses *utilsets.IgnoreCaseSet
	Nodes    map[string]Node
}

// DiffTracker is the main struct that contains the state of the K8s and NRP services
type DiffTracker struct {
	mu           sync.Mutex // Protects concurrent access to DiffTracker
	K8sResources K8s
	NRPResources NRP
}

// --------------------------------------------------------------------------------
// Types that are used while events are received and proccessed in order to update K8s state
// --------------------------------------------------------------------------------

// UpdateK8sResource represents input for K8s service or egress updates
type UpdateK8sResource struct {
	Operation Operation
	ID        string
}

// UpdateK8sEndpointsInputType represents input for K8s endpoints updates
type UpdateK8sEndpointsInputType struct {
	InboundIdentity string
	OldAddresses    map[string]string // address -> location
	NewAddresses    map[string]string // address -> location
}

// UpdatePodInputType represents input for K8s pod updates (egress assignments)
type UpdatePodInputType struct {
	PodOperation            Operation
	PublicOutboundIdentity  string
	PrivateOutboundIdentity string
	Location                string
	Address                 string
}

// --------------------------------------------------------------------------------
// Types that are used while syncing NRP state to K8s state
// --------------------------------------------------------------------------------
type Address struct {
	ServiceRef *utilsets.IgnoreCaseSet
}

// Update Location to use a map for Addresses
type Location struct {
	AddressUpdateAction UpdateAction
	Addresses           map[string]Address // key is Address.Address
}

type LocationData struct {
	Action    UpdateAction
	Locations map[string]Location // key is Location.Location
}

type SyncServicesReturnType struct {
	Additions *utilsets.IgnoreCaseSet
	Removals  *utilsets.IgnoreCaseSet
}

type SyncDiffTrackerReturnType struct {
	SyncStatus          SyncStatus
	LoadBalancerUpdates SyncServicesReturnType
	NATGatewayUpdates   SyncServicesReturnType
	LocationData        LocationData
}

// --------------------------------------------------------------------------------
// Data Transfer Objects (DTOs) for LocationData (following the ServiceGateway API documentation)
// --------------------------------------------------------------------------------

// AddressDTO represents the DTO for Address
type AddressDTO struct {
	Address      string                  `json:"Address"`
	ServiceNames *utilsets.IgnoreCaseSet `json:"ServiceNames"`
}

// LocationDTO represents the DTO for Location
type LocationDTO struct {
	Location            string       `json:"Location"`
	AddressUpdateAction UpdateAction `json:"AddressUpdateAction"`
	Addresses           []AddressDTO `json:"Addresses"`
}

// LocationDataDTO represents the DTO for LocationData
type LocationDataDTO struct {
	Action    UpdateAction  `json:"Action"`
	Locations []LocationDTO `json:"Locations"`
}

// ================================================================================================
// Data Transfer Objects (DTOs) for ServiceData (following the ServiceGateway API documentation)
// ================================================================================================

type LoadBalancerBackendPoolDTO struct {
	Id string `json:"Id"`
}

type NatGatewayDTO struct {
	Id string `json:"Id"`
}

type ServiceDTO struct {
	Service                  string                       `json:"Service"`
	LoadBalancerBackendPools []LoadBalancerBackendPoolDTO `json:"LoadBalancerBackendPools"`
	PublicNatGateway         NatGatewayDTO                `json:"PublicNatGateway"`
	isDelete                 bool
}

type ServiceDataDTO struct {
	Action   UpdateAction `json:"Action"`
	Services []ServiceDTO `json:"Services"`
}
