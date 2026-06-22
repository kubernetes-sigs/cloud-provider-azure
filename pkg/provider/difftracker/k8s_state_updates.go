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
	"fmt"
	"strings"

	"k8s.io/klog/v2"

	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

const (
	ResourceTypeService = "Service"
	ResourceTypeEgress  = "Egress"
)

// enqueueK8sResourceOperation applies the requested operation (Add/Remove) to the
// in-memory K8s resource set. It does not perform any Azure update calls; it only
// mutates the local desired-state model that will later be reconciled with NRP.
func (dt *DiffTracker) enqueueK8sResourceOperation(input UpdateK8sResource, set *utilsets.IgnoreCaseSet, resourceType string) error {
	if input.ID == "" {
		return fmt.Errorf("%s: empty ID not allowed", resourceType)
	}

	switch input.Operation {
	case Add:
		set.Insert(input.ID)
		klog.V(2).Infof("enqueueK8sResourceOperation: Added %s %s to K8s state", resourceType, input.ID)
	case Remove:
		set.Delete(input.ID)
		klog.V(2).Infof("enqueueK8sResourceOperation: Removed %s %s from K8s state", resourceType, input.ID)
	default:
		return fmt.Errorf("error - ResourceType=%s, Operation=%s and ID=%s", resourceType, input.Operation, input.ID)
	}
	return nil
}

// EnqueueK8sServiceOperation records a service Add/Remove in the local K8s state set.
// The change is reconciled with NRP later by the sync operations; this method itself
// performs no Azure calls.
func (dt *DiffTracker) EnqueueK8sServiceOperation(input UpdateK8sResource) error {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	return dt.enqueueK8sResourceOperation(input, dt.K8sResources.Services, ResourceTypeService)
}

// EnqueueK8sEgressOperation records an egress Add/Remove in the local K8s state set.
// The change is reconciled with NRP later by the sync operations; this method itself
// performs no Azure calls.
func (dt *DiffTracker) EnqueueK8sEgressOperation(input UpdateK8sResource) error {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	return dt.enqueueK8sResourceOperation(input, dt.K8sResources.Egresses, ResourceTypeEgress)
}

// updateK8sEndpointsLocked updates K8s endpoints state. Assumes lock is already held.
// Terminology: an "endpoint" here is a Kubernetes EndpointSlice endpoint, i.e. a pod IP
// (the map key "address"), and its "location" is the IP of the node/VM hosting that pod
// (the map value). Both NewAddresses and OldAddresses are address(podIP) -> location(nodeIP).
func (dt *DiffTracker) updateK8sEndpointsLocked(input UpdateK8sEndpointsInputType) []error {
	var errs []error
	for address, location := range input.NewAddresses {
		if location == "" {
			errs = append(errs, fmt.Errorf("error UpdateK8sEndpoints, address=%s does not have a node associated", address))
			continue
		}

		if oldLocation, exists := input.OldAddresses[address]; exists && oldLocation == location {
			continue
		}

		nodeState, exists := dt.K8sResources.Nodes[location]
		if !exists {
			nodeState = newNode()
			dt.K8sResources.Nodes[location] = nodeState
		}

		pod, exists := nodeState.Pods[address]
		if !exists {
			pod = newPod()
			nodeState.Pods[address] = pod
		}
		pod.InboundIdentities.Insert(input.InboundIdentity)
		klog.V(2).Infof("updateK8sEndpointsLocked: Added inbound identity %s to pod %s on node %s", input.InboundIdentity, address, location)
	}

	for address, location := range input.OldAddresses {
		if newLocation, exists := input.NewAddresses[address]; exists && newLocation == location {
			continue
		}

		if location == "" {
			errs = append(errs, fmt.Errorf("error UpdateK8sEndpoints, address=%s does not have a node associated", address))
			continue
		}

		node, nodeExists := dt.K8sResources.Nodes[location]
		if !nodeExists {
			continue
		}

		pod, podExists := node.Pods[address]
		if !podExists {
			continue
		}

		pod.InboundIdentities.Delete(input.InboundIdentity)
		klog.V(2).Infof("updateK8sEndpointsLocked: Removed inbound identity %s from pod %s on node %s", input.InboundIdentity, address, location)

		if !pod.HasIdentities() {
			delete(node.Pods, address)
			if !node.HasPods() {
				delete(dt.K8sResources.Nodes, location)
			}
		}
	}

	return errs
}

// UpdateK8sEndpoints is the public, lock-acquiring entry point for applying an
// EndpointSlice change to K8s state. It is called by the EndpointSlice informer
// handlers (Add/Update/Delete). Use this rather than updateK8sEndpointsLocked
// unless the caller already holds dt.mu.
func (dt *DiffTracker) UpdateK8sEndpoints(input UpdateK8sEndpointsInputType) []error {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.updateK8sEndpointsLocked(input)
}

func (dt *DiffTracker) addOrUpdatePod(input UpdatePodInputType) {
	node, exists := dt.K8sResources.Nodes[input.Location]
	if !exists {
		node = newNode()
		dt.K8sResources.Nodes[input.Location] = node
	}

	pod, exists := node.Pods[input.Address]
	if !exists {
		pod = newPod()
	}

	pod.PublicOutboundIdentity = input.PublicOutboundIdentity
	node.Pods[input.Address] = pod
	klog.V(2).Infof("addOrUpdatePod: Set outbound identity %q for pod %s on node %s", input.PublicOutboundIdentity, input.Address, input.Location)
}

// removePod removes a pod from K8s state. Returns true if the pod was actually
// removed, or false if it didn't exist (already removed by a previous call).
func (dt *DiffTracker) removePod(input UpdatePodInputType) bool {
	node, exists := dt.K8sResources.Nodes[input.Location]
	if !exists {
		return false
	}

	if _, podExists := node.Pods[input.Address]; !podExists {
		return false
	}

	delete(node.Pods, input.Address)
	if !node.HasPods() {
		delete(dt.K8sResources.Nodes, input.Location)
	}
	klog.V(2).Infof("removePod: Removed pod %s from node %s", input.Address, input.Location)

	return true
}

// incrementOutboundRefCount increments the ref-counter for a pod's outbound
// (egress) identity. Empty identities are not counted.
func (dt *DiffTracker) incrementOutboundRefCount(identity string) {
	if identity == "" {
		return
	}
	key := strings.ToLower(identity)
	counter := 0
	if val, ok := dt.outboundIdentityPodRefCount.Load(key); ok {
		counter = val.(int)
	}
	dt.outboundIdentityPodRefCount.Store(key, counter+1)
}

// decrementOutboundRefCount decrements the ref-counter for an outbound (egress)
// identity, deleting the entry when it reaches zero. Empty or unknown identities
// are a no-op. Returns an error if the counter is already non-positive.
func (dt *DiffTracker) decrementOutboundRefCount(identity string) error {
	if identity == "" {
		return nil
	}
	key := strings.ToLower(identity)
	val, ok := dt.outboundIdentityPodRefCount.Load(key)
	if !ok {
		return nil
	}
	counter := val.(int)
	if counter <= 0 {
		return fmt.Errorf("error - PublicOutboundIdentity %s has a non-positive count: %d", identity, counter)
	}
	if counter == 1 {
		dt.outboundIdentityPodRefCount.Delete(key)
	} else {
		dt.outboundIdentityPodRefCount.Store(key, counter-1)
	}
	return nil
}

// updateK8sPodLocked updates K8s pod state. Assumes lock is already held.
func (dt *DiffTracker) updateK8sPodLocked(input UpdatePodInputType) error {
	if input.Location == "" || input.Address == "" {
		return fmt.Errorf("updateK8sPodLocked: Location and Address must not be empty (location=%q, address=%q)", input.Location, input.Address)
	}
	switch input.PodOperation {
	case Add, Update:
		// Determine the pod's current (old) outbound identity, if any, and whether
		// this exact pod+identity is already counted (idempotent re-ADD, e.g. an
		// informer resync). The comparison is case-insensitive because the counter
		// is keyed on the lowercased identity.
		oldIdentity := ""
		alreadyExists := false
		if node, nodeExists := dt.K8sResources.Nodes[input.Location]; nodeExists {
			if pod, podExists := node.Pods[input.Address]; podExists {
				oldIdentity = pod.PublicOutboundIdentity
				if strings.EqualFold(oldIdentity, input.PublicOutboundIdentity) {
					alreadyExists = true
					klog.V(4).Infof("updateK8sPodLocked: Pod at %s:%s already exists for service %s, skipping counter update",
						input.Location, input.Address, input.PublicOutboundIdentity)
				}
			}
		}

		if !alreadyExists {
			// The pod's egress identity is changing (old -> new). Release the old
			// identity's count before counting the new one, otherwise the old
			// identity's counter would leak (a later remove decrements the new
			// identity, never the old).
			if !strings.EqualFold(oldIdentity, input.PublicOutboundIdentity) {
				if err := dt.decrementOutboundRefCount(oldIdentity); err != nil {
					return err
				}
			}
			dt.incrementOutboundRefCount(input.PublicOutboundIdentity)
		}
		dt.addOrUpdatePod(input)
		return nil
	case Remove:
		// First, try to remove the pod from K8s state.
		// removePod returns false if the pod doesn't exist (duplicate removal).
		if !dt.removePod(input) {
			klog.V(4).Infof("updateK8sPodLocked: Pod at %s:%s was already removed (duplicate delete), skipping counter decrement",
				input.Location, input.Address)
			return nil
		}

		// Decrement the ref-counter for the outbound identity passed in the
		// remove input. A missing key (e.g. an empty identity, which is never
		// counted) is a no-op.
		return dt.decrementOutboundRefCount(input.PublicOutboundIdentity)
	default:
		return fmt.Errorf("invalid pod operation: %s for pod at %s:%s",
			input.PodOperation, input.Location, input.Address)
	}
}

// UpdateK8sPod is the public, lock-acquiring entry point for applying a pod
// egress-assignment change to K8s state. It is called by the pod informer
// handlers (Add/Update/Delete). Use this rather than updateK8sPodLocked unless
// the caller already holds dt.mu.
func (dt *DiffTracker) UpdateK8sPod(input UpdatePodInputType) error {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.updateK8sPodLocked(input)
}

// removeServiceFromK8sStateLocked removes a service from all pod identities in K8s state.
// This is used during service deletion to proactively clear location/address references
// so the LocationsUpdater can sync the removal to NRP.
// Assumes lock is already held.
func (dt *DiffTracker) removeServiceFromK8sStateLocked(serviceUID string, isInbound bool) {
	for nodeIP, node := range dt.K8sResources.Nodes {
		for podIP, pod := range node.Pods {
			if isInbound {
				// Remove from inbound identities
				if pod.InboundIdentities != nil && pod.InboundIdentities.Has(serviceUID) {
					pod.InboundIdentities.Delete(serviceUID)
				}
			} else {
				// Clear outbound identity if it matches, releasing its ref-count so
				// the counter doesn't leak when a service is deleted before its pods
				// are removed.
				if strings.EqualFold(pod.PublicOutboundIdentity, serviceUID) {
					pod.PublicOutboundIdentity = ""
					node.Pods[podIP] = pod
					if err := dt.decrementOutboundRefCount(serviceUID); err != nil {
						klog.Warningf("removeServiceFromK8sStateLocked: %v", err)
					}
				}
			}

			// Clean up empty pods and nodes
			if !pod.HasIdentities() {
				delete(node.Pods, podIP)
				if !node.HasPods() {
					delete(dt.K8sResources.Nodes, nodeIP)
				}
			}
		}
	}
}

// RemoveServiceFromK8sState is the public, lock-acquiring entry point for
// removeServiceFromK8sStateLocked. Use it to clear a deleted service's references
// from pod identities when the caller does not already hold dt.mu.
func (dt *DiffTracker) RemoveServiceFromK8sState(serviceUID string, isInbound bool) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.removeServiceFromK8sStateLocked(serviceUID, isInbound)
}
