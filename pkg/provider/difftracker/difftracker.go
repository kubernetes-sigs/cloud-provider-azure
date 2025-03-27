package difftracker

import (
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func InitializeDiffTracker(K8s K8s, NRP NRP) *DiffTracker {
	// If any field is nil, initialize it
	if K8s.Services == nil {
		K8s.Services = utilsets.NewString()
	}
	if K8s.Egresses == nil {
		K8s.Egresses = utilsets.NewString()
	}
	if K8s.Nodes == nil {
		K8s.Nodes = make(map[string]Node)
	}
	if NRP.LoadBalancers == nil {
		NRP.LoadBalancers = utilsets.NewString()
	}
	if NRP.NATGateways == nil {
		NRP.NATGateways = utilsets.NewString()
	}
	if NRP.Locations == nil {
		NRP.Locations = make(map[string]NRPLocation)
	}

	diffTracker := &DiffTracker{
		K8sResources: K8s,
		NRPResources: NRP,
	}

	return diffTracker
}
