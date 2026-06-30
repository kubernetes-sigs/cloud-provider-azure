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
	"sync"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
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

	// Configuration and clients
	config               Config
	networkClientFactory azclient.ClientFactory
	kubeClient           kubernetes.Interface

	// logger is the component logger, backed by klog via pkg/log. It is supplied
	// to New and enriched with WithName("difftracker").
	logger logr.Logger
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
