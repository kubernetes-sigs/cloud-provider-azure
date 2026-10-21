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
	"net/netip"
	"strings"

	v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// serviceUIDOfEndpointSlice returns the owning Service UID of an EndpointSlice, if any.
func serviceUIDOfEndpointSlice(es *discovery_v1.EndpointSlice) (uid string, loaded bool) {
	if es == nil {
		return "", false
	}
	for _, owner := range es.OwnerReferences {
		if owner.Kind == "Service" {
			return string(owner.UID), true
		}
	}
	return "", false
}

func endpointSliceAddresses(es *discovery_v1.EndpointSlice, nodeLister corelisters.NodeLister) map[string]string {
	return endpointSliceAddressesWithStartupIPs(es, nodeLister, nil)
}

// endpointSliceAddressesWithStartupIPs is endpointSliceAddresses, resolving a node in startupNodeIPs
// through those IPs instead of the lister.
func endpointSliceAddressesWithStartupIPs(es *discovery_v1.EndpointSlice, nodeLister corelisters.NodeLister, startupNodeIPs map[string][]string) map[string]string {
	addresses := make(map[string]string)
	if es == nil || nodeLister == nil {
		return addresses
	}

	for _, endpoint := range es.Endpoints {
		if !ptr.Deref(endpoint.Conditions.Ready, true) {
			continue
		}
		nodeName := ptr.Deref(endpoint.NodeName, "")
		if nodeName == "" {
			continue
		}
		nodeIPs, ok := startupNodeIPs[nodeName]
		if !ok {
			node, err := nodeLister.Get(nodeName)
			if err != nil {
				continue
			}
			for _, address := range node.Status.Addresses {
				if address.Type == v1.NodeInternalIP {
					nodeIPs = append(nodeIPs, address.Address)
				}
			}
		}
		for _, podIP := range endpoint.Addresses {
			addr, err := netip.ParseAddr(podIP)
			if err != nil {
				klog.Warningf("EndpointSlice %s/%s has a malformed endpoint address %q; skipping", es.Namespace, es.Name, podIP)
				continue
			}
			// Resolve the node location from the address's own family rather than the slice's
			// declared AddressType. NRP requires every address to sit under a same-family
			// location, and AddressType describes the slice, not a guarantee about each address
			// it carries.
			nodeIP, ok := SelectSameFamilyNodeIP(nodeIPs, addr.Is6())
			if !ok {
				continue
			}
			addresses[addr.String()] = nodeIP
		}
	}
	return addresses
}

func endpointSliceCacheKey(es *discovery_v1.EndpointSlice) string {
	if es == nil {
		return ""
	}
	return strings.ToLower(es.Namespace + "/" + es.Name)
}

func (dt *DiffTracker) updateEndpointSliceCache(oldSlice, newES *discovery_v1.EndpointSlice) {
	oldKey := endpointSliceCacheKey(oldSlice)
	newKey := endpointSliceCacheKey(newES)
	if oldKey != "" && oldKey != newKey {
		dt.endpointSlicesCache.Delete(oldKey)
	}
	if newKey == "" {
		return
	}
	dt.endpointSlicesCache.Store(newKey, newES.DeepCopy())
}

func (dt *DiffTracker) initializeEndpointSlicesCache(endpointSlices *discovery_v1.EndpointSliceList) {
	if dt == nil || endpointSlices == nil {
		return
	}
	for i := range endpointSlices.Items {
		dt.updateEndpointSliceCache(nil, &endpointSlices.Items[i])
	}
}

// ReconcileEndpointSlice converts an EndpointSlice informer event into an endpoint delta. A nil
// old slice is an add, a nil new slice is a delete, and two slices represent an update.
func (dt *DiffTracker) ReconcileEndpointSlice(oldSlice, newES *discovery_v1.EndpointSlice) {
	if dt == nil {
		return
	}

	// The cache swap and the apply are held under one lock, like seedInboundEndpointsFromCache, so a
	// concurrent node replay of the cache cannot re-add an address this event removes.
	dt.mu.Lock()
	defer dt.mu.Unlock()

	if oldSlice == nil && newES != nil {
		// The informer's first event for a slice initialization already listed is an Add. Diff it
		// against that snapshot, or endpoints removed or made not-ready while initialization ran
		// stay registered.
		if value, ok := dt.endpointSlicesCache.Load(endpointSliceCacheKey(newES)); ok {
			if cached, ok := value.(*discovery_v1.EndpointSlice); ok && cached != nil {
				oldSlice = cached
			}
		}
	}
	dt.reconcileEndpointSliceLocked(oldSlice, newES)
}

// reconcileEndpointSliceLocked is ReconcileEndpointSlice without the lock. Must be called with dt.mu
// held.
func (dt *DiffTracker) reconcileEndpointSliceLocked(oldSlice, newES *discovery_v1.EndpointSlice) {
	// Keep an internal snapshot even when dependencies or the owning Service are not available yet.
	// A later Service registration or node event can then replay the current EndpointSlice state.
	dt.updateEndpointSliceCache(oldSlice, newES)

	nodeLister := dt.nodeLister
	if nodeLister == nil {
		dt.logger.V(4).Info("Skipped EndpointSlice reconciliation because Node lister is unavailable")
		return
	}

	oldUID, oldLoaded := serviceUIDOfEndpointSlice(oldSlice)
	newUID, newLoaded := serviceUIDOfEndpointSlice(newES)
	oldAddresses := make(map[string]string)
	newAddresses := make(map[string]string)

	switch {
	case newES == nil:
		if oldLoaded {
			oldAddresses = endpointSliceAddressesWithStartupIPs(oldSlice, nodeLister, dt.startupNodeIPs)
		}
	case oldSlice == nil:
		if newLoaded && newES.DeletionTimestamp == nil {
			newAddresses = endpointSliceAddressesWithStartupIPs(newES, nodeLister, dt.startupNodeIPs)
		}
	default:
		if oldLoaded && oldSlice.DeletionTimestamp == nil {
			oldAddresses = endpointSliceAddressesWithStartupIPs(oldSlice, nodeLister, dt.startupNodeIPs)
		}
		if newLoaded && newES.DeletionTimestamp == nil {
			newAddresses = endpointSliceAddressesWithStartupIPs(newES, nodeLister, dt.startupNodeIPs)
		}
	}

	if oldLoaded && (!newLoaded || !strings.EqualFold(oldUID, newUID)) {
		if len(oldAddresses) > 0 {
			dt.updateEndpointsLocked(oldUID, oldAddresses, nil)
		}
		oldAddresses = nil
	}
	if newLoaded && (!oldLoaded || !strings.EqualFold(oldUID, newUID)) {
		if len(newAddresses) > 0 {
			dt.updateEndpointsLocked(newUID, nil, newAddresses)
		}
		return
	}

	serviceUID := newUID
	if !newLoaded {
		serviceUID = oldUID
	}
	if serviceUID == "" || len(oldAddresses) == 0 && len(newAddresses) == 0 {
		return
	}
	dt.updateEndpointsLocked(serviceUID, oldAddresses, newAddresses)
}

// CompleteStartupReconciliation reconciles the deletions made while initialization ran, which never
// produce a Delete event: cached EndpointSlices the synced lister no longer has, and nodes whose
// initial-list Add never arrived. It must run once the Node handler has received the initial list.
func (dt *DiffTracker) CompleteStartupReconciliation(lister discoverylisters.EndpointSliceLister) {
	if dt == nil || lister == nil {
		return
	}
	dt.endpointSlicesCache.Range(func(key, _ interface{}) bool {
		dt.mu.Lock()
		defer dt.mu.Unlock()
		// Reload under the lock: a Delete event may have removed the entry since Range read it.
		value, ok := dt.endpointSlicesCache.Load(key)
		if !ok {
			return true
		}
		cached, ok := value.(*discovery_v1.EndpointSlice)
		if !ok || cached == nil {
			return true
		}
		if _, err := lister.EndpointSlices(cached.Namespace).Get(cached.Name); !apierrors.IsNotFound(err) {
			return true
		}
		dt.logger.V(2).Info("Pruned EndpointSlice deleted during initialization", "namespace", cached.Namespace, "endpointSlice", cached.Name)
		dt.reconcileEndpointSliceLocked(cached, nil)
		return true
	})

	dt.mu.Lock()
	defer dt.mu.Unlock()
	for nodeName, nodeIPs := range dt.startupNodeIPs {
		dt.logger.V(2).Info("Drained node deleted during initialization", "node", nodeName)
		dt.reconcileNodeIPChangeLocked(nodeName, nodeIPs, nil)
	}
	dt.startupNodeIPs = nil
}

// seedInboundEndpointsFromCache replays the current ready endpoints for a newly registered or
// recreated inbound service. EndpointSlice informers only report changes, so an unchanged slice
// cannot repopulate endpoint state after a ClusterIP-to-LoadBalancer transition or an in-flight
// delete/recreate.
func (dt *DiffTracker) seedInboundEndpointsFromCache(serviceUID string) {
	if dt == nil || serviceUID == "" {
		return
	}

	// The cache walk and the apply are held under one lock. The snapshot can only add addresses, so
	// releasing the lock between them lets a concurrent EndpointSlice removal be overwritten by an
	// older snapshot. The resurrected address is then unreachable: later deltas derive their removal
	// set from the cache, which no longer lists it, so it stays registered until the process
	// restarts and is served as a live backend. Holding the lock orders the two: either the removal
	// lands first and this snapshot already reflects it, or it applies after and removes it.
	dt.mu.Lock()
	defer dt.mu.Unlock()

	nodeLister := dt.nodeLister
	if nodeLister == nil {
		dt.logger.V(4).Info("Skipped endpoint cache replay because dependencies are unavailable",
			"nodeListerAvailable", false)
		return
	}

	addresses := make(map[string]string)
	dt.endpointSlicesCache.Range(func(_, value interface{}) bool {
		es, ok := value.(*discovery_v1.EndpointSlice)
		if !ok || es == nil || es.DeletionTimestamp != nil {
			return true
		}
		uid, loaded := serviceUIDOfEndpointSlice(es)
		if !loaded || !strings.EqualFold(uid, serviceUID) {
			return true
		}

		for podIP, nodeIP := range endpointSliceAddressesWithStartupIPs(es, nodeLister, dt.startupNodeIPs) {
			addresses[podIP] = nodeIP
		}
		return true
	})

	if len(addresses) == 0 {
		return
	}
	dt.logger.V(2).Info("Seeded endpoints for registered service", "service", serviceUID, "endpoints", len(addresses))
	dt.updateEndpointsLocked(serviceUID, nil, addresses)
}

// ReconcileNodeAdd replays the cached EndpointSlices hosting a pod on an added node. Until its first
// Add, a node initialization listed keeps its endpoints under the IPs listed then; the replay moves
// them from those IPs, so an InternalIP change made meanwhile does not leave them behind, and later
// events resolve the node through the lister.
func (dt *DiffTracker) ReconcileNodeAdd(nodeName string, nodeIPs []string) {
	if dt == nil || nodeName == "" {
		return
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	oldNodeIPs := dt.startupNodeIPs[nodeName]
	delete(dt.startupNodeIPs, nodeName)
	dt.reconcileNodeIPChangeLocked(nodeName, oldNodeIPs, nodeIPs)
}

// ReconcileNodeIPChange replays the cached EndpointSlices hosting a pod on nodeName into the diff
// tracker when the node's InternalIP set changes, or the node is added or removed. The EndpointSlice
// informer never fires for a node-only change (the slice content is unchanged), and it derives an
// endpoint's "old" location from the live (already-updated) node cache, so it can never emit the
// removal of a pod from its previous node IP. oldNodeIPs/newNodeIPs are taken from the informer's node
// objects rather than the mutable cache, so the old location is accurate: each affected pod is moved
// from its old same-family location to its new one. Empty newNodeIPs (node deleted) drains the pods;
// empty oldNodeIPs (node added) registers a pod dropped while its node was not yet cached.
//
// Only inbound endpoints are handled here. Egress pod locations are owned by the pod informer, which
// re-resolves them from pod.Status.HostIPs whenever that field changes, so keeping them out of this
// path avoids two writers racing on the same pod. That relies on kubelet republishing HostIPs for a
// running pod when its node's InternalIP set changes; if it does not, an egress pod stays mapped to
// its previous location until the pod or the tracker is restarted.
func (dt *DiffTracker) ReconcileNodeIPChange(nodeName string, oldNodeIPs, newNodeIPs []string) {
	if dt == nil || nodeName == "" {
		return
	}

	// The cache walk and the apply are held under one lock, like seedInboundEndpointsFromCache: the
	// replay re-adds every cached address, so a concurrent EndpointSlice removal applied between them
	// would be overwritten.
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.reconcileNodeIPChangeLocked(nodeName, oldNodeIPs, newNodeIPs)
}

// reconcileNodeIPChangeLocked is ReconcileNodeIPChange without the lock. Must be called with dt.mu
// held.
func (dt *DiffTracker) reconcileNodeIPChangeLocked(nodeName string, oldNodeIPs, newNodeIPs []string) {
	type endpointDelta struct {
		oldAddresses map[string]string
		newAddresses map[string]string
	}
	perService := make(map[string]*endpointDelta)

	dt.endpointSlicesCache.Range(func(_, value interface{}) bool {
		es, ok := value.(*discovery_v1.EndpointSlice)
		if !ok || es == nil || es.DeletionTimestamp != nil {
			return true
		}
		serviceUID, loaded := serviceUIDOfEndpointSlice(es)
		if !loaded {
			return true
		}

		// The same-family location is resolved per address below: AddressType describes the slice,
		// not a guarantee about each address it carries, and NRP requires every address to sit
		// under a location of its own family.
		for _, ep := range es.Endpoints {
			if !ptr.Deref(ep.Conditions.Ready, true) {
				continue
			}
			if !strings.EqualFold(ptr.Deref(ep.NodeName, ""), nodeName) {
				continue
			}
			for _, podIP := range ep.Addresses {
				addr, err := netip.ParseAddr(podIP)
				if err != nil {
					continue
				}
				oldLocation, oldOK := SelectSameFamilyNodeIP(oldNodeIPs, addr.Is6())
				newLocation, newOK := SelectSameFamilyNodeIP(newNodeIPs, addr.Is6())
				if !oldOK && !newOK {
					continue
				}
				delta := perService[serviceUID]
				if delta == nil {
					delta = &endpointDelta{oldAddresses: map[string]string{}, newAddresses: map[string]string{}}
					perService[serviceUID] = delta
				}
				if oldOK {
					delta.oldAddresses[addr.String()] = oldLocation
				}
				if newOK {
					delta.newAddresses[addr.String()] = newLocation
				}
			}
		}
		return true
	})

	for serviceUID, delta := range perService {
		if len(delta.oldAddresses) == 0 && len(delta.newAddresses) == 0 {
			continue
		}
		klog.V(2).Infof("ReconcileNodeIPChange: node %s changed, moving pod addresses for service %s (old=%d new=%d)",
			nodeName, serviceUID, len(delta.oldAddresses), len(delta.newAddresses))
		dt.updateEndpointsLocked(serviceUID, delta.oldAddresses, delta.newAddresses)
	}
}
