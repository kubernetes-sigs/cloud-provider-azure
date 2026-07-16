/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Wire-shape tests for sync_operations.go.

package difftracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// Per-address removal should emit an empty ServiceRef for that address.
func TestGuardSyncOps_PerAddressRemoval_EmptyServiceRef(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-removed"
	// NRP knows about the LB and one address backed by `uid`.
	dt.NRPResources.LoadBalancers.Insert(uid)
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.244.0.5": {Services: utilsets.NewString(uid)},
		},
	}
	// K8s side: node exists and pod exists at that address, but it no
	// longer backs ANY service (InboundIdentities is empty, no egress).
	node := newNode()
	pod := newPod() // empty InboundIdentities, empty PublicOutboundIdentity
	node.Pods["10.244.0.5"] = pod
	dt.K8sResources.Nodes["10.0.0.1"] = node

	result := dt.GetSyncLocationsAddresses()

	assert.Equal(t, PartialUpdate, result.Action, "top-level Action must be PartialUpdate")
	loc, ok := result.Locations["10.0.0.1"]
	if !assert.True(t, ok, "location 10.0.0.1 must be present in result") {
		return
	}
	assert.Equal(t, PartialUpdate, loc.AddressUpdateAction)
	addr, ok := loc.Addresses["10.244.0.5"]
	if !assert.True(t, ok, "address must be emitted (per-address removal contract)") {
		return
	}
	assert.NotNil(t, addr.ServiceRef, "ServiceRef must be a non-nil empty set, not nil")
	assert.Equal(t, 0, addr.ServiceRef.Len(),
		"per-address removal: ServiceRef MUST be empty so SGW unbinds this single address")
}

// Missing K8s nodes should emit PartialUpdate with an empty Addresses map.
func TestGuardSyncOps_WholeNodeRemoval_EmitsEmptyAddresses(t *testing.T) {
	dt := newTestDiffTracker()
	uid1, uid2 := "svc-a", "svc-b"
	dt.NRPResources.LoadBalancers.Insert(uid1, uid2)
	// NRP knows about two addresses on a node…
	dt.NRPResources.Locations["10.0.0.99"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.244.9.1": {Services: utilsets.NewString(uid1)},
			"10.244.9.2": {Services: utilsets.NewString(uid2)},
		},
	}
	// …but K8s has no record of node 10.0.0.99 (the node is gone).
	// (dt.K8sResources.Nodes intentionally empty for this location.)

	result := dt.GetSyncLocationsAddresses()

	loc, ok := result.Locations["10.0.0.99"]
	if !assert.True(t, ok, "whole-node removal: gone node must produce a location entry") {
		return
	}
	assert.Equal(t, PartialUpdate, loc.AddressUpdateAction,
		"whole-node removal contract: AddressUpdateAction must be PartialUpdate (current NRP API)")
	assert.NotNil(t, loc.Addresses, "Addresses must be a non-nil empty map, not nil")
	assert.Empty(t, loc.Addresses,
		"whole-node removal contract: Addresses map must be EMPTY. NRP treats this as 'delete location'. "+
			"Any change here is a wire-protocol change that REQUIRES coordinated NRP work.")
}

// Matching K8s and NRP state should produce no sync location entries.
func TestGuardSyncOps_NoSpuriousLocationEntries(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "svc-stable"
	dt.NRPResources.LoadBalancers.Insert(uid)
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.244.0.5": {Services: utilsets.NewString(uid)},
		},
	}
	dt.pendingServiceOps[uid] = &ServiceOperationState{
		ServiceUID: uid, Config: NewInboundServiceConfig(uid, makeInboundConfig(80)),
		State: StateCreated,
	}
	node := newNode()
	pod := newPod()
	pod.InboundIdentities.Insert(uid)
	node.Pods["10.244.0.5"] = pod
	dt.K8sResources.Nodes["10.0.0.1"] = node

	result := dt.GetSyncLocationsAddresses()

	assert.Empty(t, result.Locations,
		"K8s state matches NRP state — no spurious location entries should be emitted")
}

// A service parked at StateNotStarted after a terminal update, whose LB is still live in NRP, must keep
// syncing its backends (its pod address retains a non-empty ServiceRef) rather than being drained; a
// first-time create whose LB is not yet in NRP is excluded from the sync.
func TestSyncOps_ParkedLiveLBRemainsReadyToSync(t *testing.T) {
	dt := newTestDiffTracker()
	const node = "10.0.0.1"

	live, pending := "svc-parked-live", "svc-new-pending"
	dt.NRPResources.LoadBalancers.Insert(live) // live has an LB in NRP; pending does not.

	dt.pendingServiceOps[live] = &ServiceOperationState{
		ServiceUID: live, Config: NewInboundServiceConfig(live, nil), State: StateNotStarted,
	}
	dt.pendingServiceOps[pending] = &ServiceOperationState{
		ServiceUID: pending, Config: NewInboundServiceConfig(pending, nil), State: StateNotStarted,
	}

	livePod, pendingPod := newPod(), newPod()
	livePod.InboundIdentities.Insert(live)
	pendingPod.InboundIdentities.Insert(pending)
	n := newNode()
	n.Pods["10.244.0.10"] = livePod
	n.Pods["10.244.0.11"] = pendingPod
	dt.K8sResources.Nodes[node] = n

	result := dt.GetSyncLocationsAddresses()
	loc, ok := result.Locations[node]
	if !assert.True(t, ok, "location must be present for the live LB's pod") {
		return
	}

	liveAddr, ok := loc.Addresses["10.244.0.10"]
	if assert.True(t, ok, "the live LB's pod address must be synced") {
		assert.True(t, liveAddr.ServiceRef.Has(live),
			"a parked service whose LB is live in NRP must keep its backend bound (no drain)")
	}

	if pendingAddr, ok := loc.Addresses["10.244.0.11"]; ok {
		assert.False(t, pendingAddr.ServiceRef.Has(pending),
			"a first-time create with no NRP LB must not be synced yet")
	}
}
