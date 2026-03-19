package difftracker

import (
	"sync"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
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

type NRP_State struct {
	LoadBalancers *utilsets.IgnoreCaseSet
	NATGateways   *utilsets.IgnoreCaseSet
	Locations     map[string]NRPLocation
}

type Pod struct {
	InboundIdentities      *utilsets.IgnoreCaseSet
	PublicOutboundIdentity string
}

type Node struct {
	Pods map[string]Pod
}

type K8s_State struct {
	Services *utilsets.IgnoreCaseSet
	Egresses *utilsets.IgnoreCaseSet
	Nodes    map[string]Node
}

// DiffTracker is the main struct that contains the state of the K8s and NRP services
type DiffTracker struct {
	mu sync.Mutex // Protects concurrent access to DiffTracker

	K8sResources K8s_State
	NRPResources NRP_State

	LocalServiceNameToNRPServiceMap sync.Map

	// Configuration and clients
	config               Config
	networkClientFactory azclient.ClientFactory
	kubeClient           kubernetes.Interface
}

// --------------------------------------------------------------------------------
// Types that are used while events are received and processed in order to update K8s state
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
	PodOperation           Operation
	PublicOutboundIdentity string
	Location               string
	Address                string
}

// --------------------------------------------------------------------------------
// Types that are used while syncing NRP state to K8s state
// --------------------------------------------------------------------------------
type Address struct {
	ServiceRef *utilsets.IgnoreCaseSet
}

// Location uses a map for Addresses
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
