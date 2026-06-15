package difftracker

import (
	"errors"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// ================================================================================================
// ERRORS
// ================================================================================================

var (
	// ErrServiceUIDEmpty is returned when a service UID is empty
	ErrServiceUIDEmpty = errors.New("service UID cannot be empty")
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

// ResourceState represents the lifecycle state of a service in the Engine
type ResourceState int

const (
	StateNotStarted ResourceState = iota
	StateCreationInProgress
	StateCreated
	StateDeletionPending
	StateDeletionInProgress
	StateUpdateInProgress
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

// ================================================================================================
// SERVICE CONFIGURATION TYPES
// ================================================================================================

// PortMapping represents a port mapping configuration
type PortMapping struct {
	Port     int32
	Protocol string // TCP or UDP
}

// HealthProbeConfig represents health probe configuration
type HealthProbeConfig struct {
	Protocol          string // TCP, HTTP, or HTTPS
	Port              int32
	IntervalInSeconds int32
	NumberOfProbes    int32
	RequestPath       *string // For HTTP/HTTPS probes
}

// InboundConfig contains Load Balancer configuration for inbound services
type InboundConfig struct {
	FrontendPorts      []PortMapping      // nullable for future use
	BackendPorts       []PortMapping      // nullable for future use
	Protocol           *string            // TCP/UDP, nullable
	IdleTimeoutMinutes *int32             // nullable
	SessionPersistence *string            // nullable
	HealthProbe        *HealthProbeConfig // nullable
}

// OutboundConfig contains NAT Gateway configuration for outbound services
type OutboundConfig struct {
	// Placeholder for future NAT Gateway options
}

// Equals returns true if two InboundConfigs describe the same desired LB shape.
// Used by UpdateService to short-circuit no-op reconciles.
// Comparison is order-sensitive for FrontendPorts/BackendPorts because the
// position of a port determines its pairing with a backend port in
// buildInboundServiceResources.
func (c *InboundConfig) Equals(other *InboundConfig) bool {
	if c == nil && other == nil {
		return true
	}
	if c == nil || other == nil {
		return false
	}
	if !portMappingsEqual(c.FrontendPorts, other.FrontendPorts) {
		return false
	}
	if !portMappingsEqual(c.BackendPorts, other.BackendPorts) {
		return false
	}
	if !strPtrEqual(c.Protocol, other.Protocol) {
		return false
	}
	if !int32PtrEqual(c.IdleTimeoutMinutes, other.IdleTimeoutMinutes) {
		return false
	}
	if !strPtrEqual(c.SessionPersistence, other.SessionPersistence) {
		return false
	}
	if !healthProbeEqual(c.HealthProbe, other.HealthProbe) {
		return false
	}
	return true
}

func portMappingsEqual(a, b []PortMapping) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func strPtrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func int32PtrEqual(a, b *int32) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func healthProbeEqual(a, b *HealthProbeConfig) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Protocol != b.Protocol || a.Port != b.Port ||
		a.IntervalInSeconds != b.IntervalInSeconds || a.NumberOfProbes != b.NumberOfProbes {
		return false
	}
	return strPtrEqual(a.RequestPath, b.RequestPath)
}

// ServiceConfig encapsulates all configuration needed for a service
type ServiceConfig struct {
	UID            string
	IsInbound      bool
	InboundConfig  *InboundConfig  // nil allowed for defaults
	OutboundConfig *OutboundConfig // nil allowed for defaults
}

// Validate checks if the ServiceConfig is valid
func (c *ServiceConfig) Validate() error {
	if c.UID == "" {
		return ErrServiceUIDEmpty
	}
	if c.IsInbound && c.InboundConfig == nil {
		// Allow nil InboundConfig - will use defaults
	}
	if !c.IsInbound && c.OutboundConfig == nil {
		// Allow nil OutboundConfig - will use defaults
	}
	return nil
}

// NewInboundServiceConfig creates a ServiceConfig for an inbound service
func NewInboundServiceConfig(uid string, inboundConfig *InboundConfig) ServiceConfig {
	return ServiceConfig{
		UID:           uid,
		IsInbound:     true,
		InboundConfig: inboundConfig,
	}
}

// NewOutboundServiceConfig creates a ServiceConfig for an outbound service
func NewOutboundServiceConfig(uid string, outboundConfig *OutboundConfig) ServiceConfig {
	return ServiceConfig{
		UID:            uid,
		IsInbound:      false,
		OutboundConfig: outboundConfig,
	}
}

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

// ================================================================================================
// ENGINE STATE TRACKING TYPES
// ================================================================================================

// ServiceOperationState tracks the lifecycle state of a service being created or deleted
type ServiceOperationState struct {
	ServiceUID string
	// Config is the desired configuration. For update flows it is overwritten by
	// the latest Update call before the updater goroutine picks it up.
	Config ServiceConfig
	// InFlightConfig is the configuration snapshot that the currently-running
	// (or last-dispatched) updater goroutine is applying. Set by the dispatcher
	// when it picks up the work; consumed by OnServiceCreationComplete to
	// (a) populate LastAppliedConfig on success and (b) detect whether a newer
	// Config arrived during the in-flight operation so we can reschedule.
	InFlightConfig *ServiceConfig
	// LastAppliedConfig is the configuration last successfully applied to Azure.
	// Used by UpdateService to short-circuit no-op updates.
	LastAppliedConfig *ServiceConfig
	State             ResourceState
	RetryCount        int
	LastAttempt       string // timestamp as string for serialization

	// CreatedAt is when this operation was first enqueued. Never overwritten on retry.
	// Used by the oldest-age metric to detect stuck operations.
	CreatedAt time.Time

	// IsOrphan indicates this is an orphaned Azure resource being cleaned up at startup.
	IsOrphan bool

	// CorrelationID is a unique ID for tracing all logs related to this operation.
	CorrelationID string

	// TriggeringPodNamespace and TriggeringPodName identify which pod triggered
	// a NAT Gateway creation (outbound services only).
	TriggeringPodNamespace string
	TriggeringPodName      string
}

// PendingEndpointUpdate represents endpoints waiting for their service to be created
type PendingEndpointUpdate struct {
	PodIPToNodeIP map[string]string // podIP -> nodeIP
	Timestamp     string            // When buffered
}

// PendingServiceDeletion tracks a service waiting for locations to clear before deletion
type PendingServiceDeletion struct {
	ServiceUID string
	IsInbound  bool
	Timestamp  string
}

// DiffTracker is the main struct that contains the state of the K8s and NRP services
type DiffTracker struct {
	mu sync.Mutex // Protects concurrent access to DiffTracker

	K8sResources K8s_State
	NRPResources NRP_State

	LocalServiceNameToNRPServiceMap sync.Map

	InitialSyncDone bool

	// Configuration and clients
	config               Config
	networkClientFactory azclient.ClientFactory
	kubeClient           kubernetes.Interface

	// Engine state management
	pendingServiceOps       map[string]*ServiceOperationState
	pendingEndpoints        map[string][]PendingEndpointUpdate
	pendingPods             map[string][]PendingPodUpdate
	pendingServiceDeletions map[string]*PendingServiceDeletion
	pendingPodDeletions     map[string]*PendingPodDeletion // key = "namespace/name"

	// Communication channels
	serviceUpdaterTrigger   chan bool
	locationsUpdaterTrigger chan bool

	// Initialization tracking
	isInitializing         int32 // Atomic: 1 during initialization, 0 after
	initCompletionChecker  chan struct{}
	pendingUpdaterTriggers int32 // Atomic counter for in-flight updater triggers
	initCompletionOnce     sync.Once

	// Updater references (started during initialization, kept running)
	serviceUpdater   *ServiceUpdater
	locationsUpdater *LocationsUpdater
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
	PodOperation           Operation
	PublicOutboundIdentity string
	Location               string
	Address                string
}

// PendingPodUpdate represents a pod waiting for its service to be created
type PendingPodUpdate struct {
	PodKey    string // namespace/name for logging
	Location  string // HostIP
	Address   string // PodIP
	Timestamp string // When buffered (for debugging/metrics)
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

// LocationsDataDTO represents the DTO for LocationData
type LocationsDataDTO struct {
	Action    UpdateAction  `json:"Action"`
	Locations []LocationDTO `json:"Locations"`
}

// ================================================================================================
// Data Transfer Objects (DTOs) for ServiceData (following the ServiceGateway API documentation)
// ================================================================================================

type ServiceType string

const (
	Inbound  ServiceType = "Inbound"
	Outbound ServiceType = "Outbound"
)

type LoadBalancerBackendPoolDTO struct {
	Id string `json:"Id"`
}

type NatGatewayDTO struct {
	Id string `json:"Id"`
}

type ServiceDTO struct {
	Service                  string                       `json:"Service"`
	ServiceType              ServiceType                  `json:"ServiceType"`
	LoadBalancerBackendPools []LoadBalancerBackendPoolDTO `json:"LoadBalancerBackendPools"`
	PublicNatGateway         NatGatewayDTO                `json:"PublicNatGateway"`
	IsDelete                 bool
}

type ServicesDataDTO struct {
	Action   UpdateAction `json:"Action"`
	Services []ServiceDTO `json:"Services"`
}
