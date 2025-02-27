package difftracker

import (
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func InitializeDiffTrackerState(k8sState K8sState, nrpState NRPState) (*DiffTrackerState, *SyncDiffTrackerStateReturnType) {
	// If any field is nil, initialize it
	if k8sState.Services == nil {
		k8sState.Services = utilsets.NewString()
	}
	if k8sState.Egresses == nil {
		k8sState.Egresses = utilsets.NewString()
	}
	if k8sState.Nodes == nil {
		k8sState.Nodes = make(map[string]Node)
	}
	if nrpState.LoadBalancers == nil {
		nrpState.LoadBalancers = utilsets.NewString()
	}
	if nrpState.NATGateways == nil {
		nrpState.NATGateways = utilsets.NewString()
	}
	if nrpState.NRPLocations == nil {
		nrpState.NRPLocations = make(map[string]NRPLocation)
	}

	diffTrackerState := &DiffTrackerState{
		K8s: k8sState,
		NRP: nrpState,
	}
	syncOperations := diffTrackerState.GetSyncDiffTrackerState()
	return diffTrackerState, syncOperations
}
