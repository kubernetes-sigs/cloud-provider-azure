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
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"

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
	UnknownOperation Operation = iota
	Add
	Remove
	Update
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
	UnknownUpdateAction UpdateAction = iota
	PartialUpdate
	FullUpdate
)

type SyncStatus int

const (
	UnknownSyncStatus SyncStatus = iota
	AlreadyInSync
	Success
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
	// IPFamilies mirrors Service.Spec.IPFamilies ("IPv4"/"IPv6"). A single entry selects the
	// Public IP version; more than one (dual-stack) is unsupported for PodIP backend pools and
	// is rejected as a terminal error in buildInboundServiceResources.
	IPFamilies []string
	// NamedTargetPorts holds any string (named) Service targetPort values. Named target ports
	// cannot be resolved to a PodIP backend port here, so their presence is rejected as a
	// terminal error in buildInboundServiceResources rather than silently mis-routing traffic.
	NamedTargetPorts []string
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
	if !stringSlicesEqual(c.IPFamilies, other.IPFamilies) {
		return false
	}
	if !stringSlicesEqual(c.NamedTargetPorts, other.NamedTargetPorts) {
		return false
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
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

	// Namespace and Name identify the originating Kubernetes Service. They let the engine
	// resolve a UID to a cached serviceLister read instead of scanning every Service. They
	// are empty for entries recovered from NRP without a Service object.
	Namespace string
	Name      string
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
// NRPAddress holds the NRP-side state for a single pod address (pod IP).
type NRPAddress struct {
	// Services holds the SGW service identities (LBs for inbound, NATGWs for
	// outbound) currently associated with this address on the NRP side.
	// These are SGW service identities, not Kubernetes Service names.
	Services *utilsets.IgnoreCaseSet
}

// NRPLocation holds the NRP-side state for a single node/VM and groups the
// pod addresses running on it.
type NRPLocation struct {
	// Addresses is keyed by pod IP. Each pod IP is added to the ServiceGateway
	// as an address under this location once the pod is created.
	Addresses map[string]NRPAddress
}

type NRPState struct {
	// LoadBalancers holds the UIDs of inbound services that have a LoadBalancer
	// registered on the NRP side. These are SGW service identities, not Azure
	// LoadBalancer resource names.
	LoadBalancers *utilsets.IgnoreCaseSet
	// NATGateways holds the UIDs of outbound/egress services that have a NAT
	// Gateway registered on the NRP side (SGW service identities, not Azure
	// resource names).
	NATGateways *utilsets.IgnoreCaseSet
	// Locations is keyed by node/VM IP (e.g. "10.0.0.1"). "Location" here is
	// an SGW concept identifying a node, not an Azure region (e.g. "eastus2").
	Locations map[string]NRPLocation
}

type Pod struct {
	// InboundIdentities holds the UIDs of the inbound ServiceGateway services
	// (LoadBalancers) this pod backs. A pod may back several, hence a set.
	InboundIdentities *utilsets.IgnoreCaseSet
	// PublicOutboundIdentity is the UID of the single outbound/egress ServiceGateway
	// service (NAT Gateway) this pod uses for egress; empty if the pod has no egress.
	PublicOutboundIdentity string
}

// newPod returns a Pod with its InboundIdentities set initialized.
func newPod() Pod {
	return Pod{InboundIdentities: utilsets.NewString()}
}

type Node struct {
	Pods map[string]Pod
}

// newNode returns a Node with its Pods map initialized.
func newNode() Node {
	return Node{Pods: make(map[string]Pod)}
}

type K8sState struct {
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

	// NextRetryAt is the earliest time the dispatcher may re-dispatch this operation after a
	// transient failure. Set in the failure branches of OnServiceCreationComplete to now+backoff;
	// the dispatcher skips (and schedules a revisit) while now is before it. Prevents the
	// no-backoff hot-loop on a persistent transient ARM/NRP error or a recovered worker panic.
	NextRetryAt time.Time

	// RetriesExhausted is set once the operation has been retried maxServiceRetries times; the
	// dispatcher then stops re-dispatching it (a "gave up" park) instead of looping unbounded.
	RetriesExhausted bool

	// CreationFailedTerminal is set when creation failed with a non-retryable
	// (deterministic) error such as an invalid Service spec (unsupported protocol,
	// port out of range). The dispatcher skips such entries instead of retrying
	// forever; a later UpdateService with a changed spec clears it.
	CreationFailedTerminal bool

	// CreatedAt is when this operation was first enqueued. Never overwritten on retry.
	// Used by the oldest-age metric to detect stuck operations.
	CreatedAt time.Time

	// OperationStartedAt is when the current Azure operation was dispatched in
	// processBatch (refreshed on every dispatch, including retries). Used to measure
	// real operation latency rather than the time spent in the completion callback.
	OperationStartedAt time.Time

	// IsOrphan indicates this is an orphaned Azure resource being cleaned up at startup.
	IsOrphan bool

	// CorrelationID is a unique ID for tracing all logs related to this operation.
	CorrelationID string

	// TriggeringPodNamespace and TriggeringPodName identify which pod triggered
	// a NAT Gateway creation (outbound services only).
	TriggeringPodNamespace string
	TriggeringPodName      string

	// RecreateAfterDeletion is set when an UpdateService re-create arrives while the
	// service is being deleted; the deletion-success path replays it as a fresh create.
	RecreateAfterDeletion bool
}

// PendingEndpointUpdate represents endpoints waiting for their service to be created
type PendingEndpointUpdate struct {
	OldPodIPToNodeIP map[string]string // podIP -> nodeIP removed by this event
	PodIPToNodeIP    map[string]string // podIP -> nodeIP added/present by this event
	Timestamp        string            // When buffered
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

	K8sResources K8sState
	NRPResources NRPState

	// outboundIdentityPodRefCount counts how many pods reference each outbound
	// (egress) identity, keyed by lowercased PublicOutboundIdentity. It lets the
	// engine delete a NAT Gateway when its last egress pod is removed. Inbound
	// (LoadBalancer) services are not tracked here; their lifecycle follows the
	// Kubernetes Service object.
	outboundIdentityPodRefCount sync.Map

	InitialSyncDone bool

	// Configuration and clients
	config               Config
	networkClientFactory azclient.ClientFactory
	kubeClient           kubernetes.Interface

	// serviceLister is the provider's shared-informer-backed Service lister, published via
	// SetServiceLister once SetInformers has run. It is nil until then (and in tests), in which
	// case getServiceByUID falls back to a direct apiserver read. Guarded by mu.
	serviceLister corelisters.ServiceLister
	// nodeLister is the provider's shared-informer-backed Node lister used to resolve EndpointSlice
	// node names to InternalIPs. Guarded by mu.
	nodeLister corelisters.NodeLister

	// eventRecorder emits Service Gateway pod events; set post-init before the egress informer starts.
	eventRecorder record.EventRecorder
	// endpointSlicesCache is owned by difftracker and stores snapshots from forwarded informer
	// events. It is replayed when a service is registered and by ReconcileNodeIPChange.
	endpointSlicesCache sync.Map

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

	// logger is the component logger, backed by klog via pkg/log. It is supplied
	// to New and enriched with WithName("difftracker").
	logger logr.Logger
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
	Addresses           map[string]Address // key is the pod IP
}

type LocationData struct {
	Action    UpdateAction
	Locations map[string]Location // key is the node IP
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
