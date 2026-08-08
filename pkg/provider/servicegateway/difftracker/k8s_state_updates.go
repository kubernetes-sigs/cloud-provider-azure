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

	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

const (
	ResourceTypeService = "Service"
	ResourceTypeEgress  = "Egress"
)

// enqueueK8sResourceOperation applies the requested operation (Add/Remove) to the
// in-memory K8s resource set selected by resourceType. It does not perform any Azure
// update calls; it only mutates the local desired-state model that will later be
// reconciled with NRP.
func (dt *DiffTracker) enqueueK8sResourceOperation(input UpdateK8sResource, resourceType string) error {
	if input.ID == "" {
		return fmt.Errorf("%s: empty ID not allowed", resourceType)
	}

	var set *utilsets.IgnoreCaseSet
	switch resourceType {
	case ResourceTypeService:
		set = dt.K8sResources.Services
	case ResourceTypeEgress:
		set = dt.K8sResources.Egresses
	default:
		return fmt.Errorf("unknown resource type: %s", resourceType)
	}

	switch input.Operation {
	case Add:
		set.Insert(input.ID)
		dt.logger.V(5).Info("Added resource to K8s state", "resourceType", resourceType, "id", input.ID)
	case Remove:
		set.Delete(input.ID)
		dt.logger.V(5).Info("Removed resource from K8s state", "resourceType", resourceType, "id", input.ID)
	default:
		return fmt.Errorf("error - ResourceType=%s, Operation=%s and ID=%s", resourceType, input.Operation, input.ID)
	}
	return nil
}

// EnqueueK8sServiceOperation records a service Add/Remove in the local K8s state set.
// The change is reconciled with NRP later by the sync operations; this method itself
// performs no Azure calls.
//
// Deletion is a two-step protocol: a Remove here only updates the top-level Services
// set (which drives LoadBalancer add/remove). It does NOT clear the service from pod
// InboundIdentities; that cleanup (which drives the location/address sync) must be done
// separately via RemoveServiceFromK8sState. A full service deletion must do both.
func (dt *DiffTracker) EnqueueK8sServiceOperation(input UpdateK8sResource) error {
	defer dt.lockWithLatency("EnqueueK8sServiceOperation")()

	return dt.enqueueK8sResourceOperation(input, ResourceTypeService)
}

// EnqueueK8sEgressOperation records an egress Add/Remove in the local K8s state set.
// The change is reconciled with NRP later by the sync operations; this method itself
// performs no Azure calls.
//
// Deletion is a two-step protocol: a Remove here only updates the top-level Egresses
// set (which drives NAT Gateway add/remove). It does NOT clear the egress identity from
// pods; that cleanup must be done separately via RemoveServiceFromK8sState. A full
// egress deletion must do both.
func (dt *DiffTracker) EnqueueK8sEgressOperation(input UpdateK8sResource) error {
	defer dt.lockWithLatency("EnqueueK8sEgressOperation")()

	return dt.enqueueK8sResourceOperation(input, ResourceTypeEgress)
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
		dt.logger.V(5).Info("Added inbound identity to pod", "identity", input.InboundIdentity, "pod", address, "node", location)
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
		dt.logger.V(5).Info("Removed inbound identity from pod", "identity", input.InboundIdentity, "pod", address, "node", location)

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
	defer dt.lockWithLatency("UpdateK8sEndpoints")()
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
	dt.logger.V(5).Info("Set outbound identity for pod", "identity", input.PublicOutboundIdentity, "pod", input.Address, "node", input.Location)
}

// removePod clears the pod's outbound identity when it matches the input and
// deletes the pod only once it has no identities left, so an egress removal never
// drops a pod's inbound (LoadBalancer) backing. It returns whether the pod existed
// and any error from decrementing the ref-counter.
func (dt *DiffTracker) removePod(input UpdatePodInputType) (existed bool, err error) {
	node, nodeExists := dt.K8sResources.Nodes[input.Location]
	if !nodeExists {
		return false, nil
	}

	pod, podExists := node.Pods[input.Address]
	if !podExists {
		return false, nil
	}

	if pod.PublicOutboundIdentity != "" && strings.EqualFold(pod.PublicOutboundIdentity, input.PublicOutboundIdentity) {
		pod.PublicOutboundIdentity = ""
		node.Pods[input.Address] = pod
		err = dt.decrementOutboundRefCount(input.PublicOutboundIdentity)
	}

	if !pod.HasIdentities() {
		delete(node.Pods, input.Address)
		if !node.HasPods() {
			delete(dt.K8sResources.Nodes, input.Location)
		}
		dt.logger.V(5).Info("Removed pod from node", "pod", input.Address, "node", input.Location)
	}

	return true, err
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
	// Validate the identifying fields. Location and Address must be non-empty as they
	// key the node/pod maps. PodOperation is validated by the switch below (the default
	// case rejects UnknownOperation and any invalid value). PublicOutboundIdentity is
	// intentionally NOT required: an empty value is a valid "pod has no egress identity"
	// state, and the ref-count helpers (increment/decrementOutboundRefCount) no-op on "".
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
					dt.logger.V(4).Info("Pod already exists for service, skipping counter update",
						"node", input.Location, "pod", input.Address, "service", input.PublicOutboundIdentity)
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
		existed, err := dt.removePod(input)
		if !existed {
			dt.logger.V(4).Info("Pod was already removed, skipping counter decrement",
				"node", input.Location, "pod", input.Address)
			return nil
		}
		return err
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
	defer dt.lockWithLatency("UpdateK8sPod")()
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
						dt.logger.V(4).Info("Could not decrement outbound ref-count", "err", err, "service", serviceUID)
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
//
// This is the second step of the deletion protocol: EnqueueK8sServiceOperation /
// EnqueueK8sEgressOperation with Remove updates the top-level set, while this method
// clears the per-pod identity references. A full service/egress deletion must call
// both. The two stay separate because they feed different sync paths (set membership
// drives LoadBalancer/NAT Gateway removal; identity cleanup drives the location/address
// sync), and a later PR adds the engine entry point that sequences them under one lock.
func (dt *DiffTracker) RemoveServiceFromK8sState(serviceUID string, isInbound bool) {
	defer dt.lockWithLatency("RemoveServiceFromK8sState")()
	dt.removeServiceFromK8sStateLocked(serviceUID, isInbound)
}
