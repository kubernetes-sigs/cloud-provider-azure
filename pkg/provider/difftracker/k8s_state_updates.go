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

func updateK8Resource(input UpdateK8sResource, set *utilsets.IgnoreCaseSet, resourceType string) error {
	if input.ID == "" {
		return fmt.Errorf("%s: empty ID not allowed", resourceType)
	}

	switch input.Operation {
	case ADD:
		set.Insert(input.ID)
	case REMOVE:
		set.Delete(input.ID)
	default:
		return fmt.Errorf("error - ResourceType=%s, Operation=%s and ID=%s", resourceType, input.Operation, input.ID)
	}
	return nil
}

func (dt *DiffTracker) UpdateK8sService(input UpdateK8sResource) error {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	return updateK8Resource(input, dt.K8sResources.Services, ResourceTypeService)
}

func (dt *DiffTracker) UpdateK8sEgress(input UpdateK8sResource) error {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	return updateK8Resource(input, dt.K8sResources.Egresses, ResourceTypeEgress)
}

// updateK8sEndpointsLocked updates K8s endpoints state. Assumes lock is already held.
func (dt *DiffTracker) updateK8sEndpointsLocked(input UpdateK8sEndpointsInputType) []error {
	var errs []error
	for address, location := range input.NewAddresses {

		if location == "" {
			errs = append(errs, fmt.Errorf("error UpdateK8sEndpoints, address=%s does not have a node associated", address))
			continue
		}

		if _, exists := input.OldAddresses[address]; exists {
			continue
		}

		nodeState, exists := dt.K8sResources.Nodes[location]
		if !exists {
			nodeState = Node{
				Pods: make(map[string]Pod),
			}
			dt.K8sResources.Nodes[location] = nodeState
		}

		pod, exists := nodeState.Pods[address]
		if !exists {
			pod = Pod{
				InboundIdentities: utilsets.NewString(),
			}
			nodeState.Pods[address] = pod
		}
		pod.InboundIdentities.Insert(input.InboundIdentity)
	}

	for address, location := range input.OldAddresses {
		if _, exists := input.NewAddresses[address]; exists {
			continue
		}

		if location == "" {
			errs = append(errs, fmt.Errorf("error UpdateK8sEndpoints, address=%s does not have a node associated", address))
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

		if !pod.HasIdentities() {
			delete(node.Pods, address)
			if !node.HasPods() {
				delete(dt.K8sResources.Nodes, location)
			}
		}
	}

	return errs
}

// UpdateK8sEndpoints is a public wrapper that acquires lock before calling updateK8sEndpointsLocked.
func (dt *DiffTracker) UpdateK8sEndpoints(input UpdateK8sEndpointsInputType) []error {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.updateK8sEndpointsLocked(input)
}

func (dt *DiffTracker) addOrUpdatePod(input UpdatePodInputType) error {
	node, exists := dt.K8sResources.Nodes[input.Location]
	if !exists {
		node = Node{Pods: make(map[string]Pod)}
		dt.K8sResources.Nodes[input.Location] = node
	}

	pod, exists := node.Pods[input.Address]
	if !exists {
		pod = Pod{InboundIdentities: utilsets.NewString()}
	}

	pod.PublicOutboundIdentity = input.PublicOutboundIdentity
	node.Pods[input.Address] = pod

	return nil
}

func (dt *DiffTracker) removePod(input UpdatePodInputType) error {
	node, exists := dt.K8sResources.Nodes[input.Location]
	if !exists {
		return nil
	}

	delete(node.Pods, input.Address)
	if !node.HasPods() {
		delete(dt.K8sResources.Nodes, input.Location)
	}

	return nil
}

// updateK8sPodLocked updates K8s pod state. Assumes lock is already held.
func (dt *DiffTracker) updateK8sPodLocked(input UpdatePodInputType) error {
	switch input.PodOperation {
	case ADD, UPDATE:
		counter := 0
		if val, ok := dt.LocalServiceNameToNRPServiceMap.Load(strings.ToLower(input.PublicOutboundIdentity)); ok {
			counter = val.(int)
		}
		dt.LocalServiceNameToNRPServiceMap.Store(strings.ToLower(input.PublicOutboundIdentity), counter+1)
		return dt.addOrUpdatePod(input)
	case REMOVE:
		if val, ok := dt.LocalServiceNameToNRPServiceMap.Load(strings.ToLower(input.PublicOutboundIdentity)); ok {
			counter := val.(int)
			if counter <= 0 {
				return fmt.Errorf("error - PublicOutboundIdentity %s has a negative count: %d", input.PublicOutboundIdentity, counter)
			}
			if counter == 1 {
				dt.LocalServiceNameToNRPServiceMap.Delete(strings.ToLower(input.PublicOutboundIdentity))
			} else {
				dt.LocalServiceNameToNRPServiceMap.Store(strings.ToLower(input.PublicOutboundIdentity), counter-1)
			}
		}
		return dt.removePod(input)
	default:
		return fmt.Errorf("invalid pod operation: %s for pod at %s:%s",
			input.PodOperation, input.Location, input.Address)
	}
}

// UpdateK8sPod is a public wrapper that acquires lock before calling updateK8sPodLocked.
func (dt *DiffTracker) UpdateK8sPod(input UpdatePodInputType) error {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.updateK8sPodLocked(input)
}
