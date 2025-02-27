package difftracker

import (
	"fmt"

	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

const (
	ResourceTypeService = "Service"
	ResourceTypeEgress  = "Egress"
)

func (dt *DiffTrackerState) UpdateK8service(input UpdateK8sResource) error {
	return updateK8Resource(input, dt.K8s.Services, ResourceTypeService)
}

func (dt *DiffTrackerState) UpdateK8sEgress(input UpdateK8sResource) error {
	return updateK8Resource(input, dt.K8s.Egresses, ResourceTypeEgress)
}

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
		return fmt.Errorf("error Update%s, Operation=%s and ID=%s", resourceType, input.Operation, input.ID)
	}
	return nil
}

func (dt *DiffTrackerState) UpdateK8sEndpoints(input UpdateK8sEndpointsInputType) []error {
	var errs []error
	for address, location := range input.NewAddresses {

		if _, exists := input.OldAddresses[address]; exists {
			continue
		}

		if location == "" {
			errs = append(errs, fmt.Errorf("error UpdateK8sEndpoints, address=%s does not have a node associated", address))
		}

		nodeState, exists := dt.K8s.Nodes[location]
		if !exists {
			nodeState = Node{
				Pods: make(map[string]Pod),
			}
			dt.K8s.Nodes[location] = nodeState
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

		node, nodeExists := dt.K8s.Nodes[location]
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
				delete(dt.K8s.Nodes, location)
			}
		}
	}

	return errs
}

func (dt *DiffTrackerState) UpdateK8sPod(input UpdatePodInputType) error {
	switch input.PodOperation {
	case ADD, UPDATE:
		return dt.addOrUpdatePod(input)
	case REMOVE:
		return dt.removePod(input)
	default:
		return fmt.Errorf("invalid pod operation: %s for pod at %s:%s",
			input.PodOperation, input.Location, input.Address)
	}
}

func (dt *DiffTrackerState) addOrUpdatePod(input UpdatePodInputType) error {
	node, exists := dt.K8s.Nodes[input.Location]
	if !exists {
		node = Node{Pods: make(map[string]Pod)}
		dt.K8s.Nodes[input.Location] = node
	}

	pod, exists := node.Pods[input.Address]
	if !exists {
		pod = Pod{InboundIdentities: utilsets.NewString()}
	}

	pod.PublicOutboundIdentity = input.PublicOutboundIdentity
	pod.PrivateOutboundIdentity = input.PrivateOutboundIdentity
	node.Pods[input.Address] = pod

	return nil
}

func (dt *DiffTrackerState) removePod(input UpdatePodInputType) error {
	node, exists := dt.K8s.Nodes[input.Location]
	if !exists {
		return nil
	}

	delete(node.Pods, input.Address)
	if !node.HasPods() {
		delete(dt.K8s.Nodes, input.Location)
	}

	return nil
}
