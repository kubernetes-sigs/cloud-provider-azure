package difftracker

func (dt *DiffTrackerState) handleService(input UpdateK8sResource) SyncNRPServicesReturnType {
	dt.UpdateK8service(input)
	SyncDiffTrackerStateReturnType := dt.GetSyncLoadBalancerNRPServices()
	return SyncDiffTrackerStateReturnType
}

func (dt *DiffTrackerState) handleEgress(input UpdateK8sResource) SyncNRPServicesReturnType {
	dt.UpdateK8sEgress(input)
	SyncDiffTrackerStateReturnType := dt.GetSyncNRPNATGateways()
	return SyncDiffTrackerStateReturnType
}

func (dt *DiffTrackerState) handleEndpoints(input UpdateK8sEndpointsInputType) LocationData {
	dt.UpdateK8sEndpoints(input)
	locationData := dt.GetSyncNRPLocationsAddresses()
	return locationData
}

func (dt *DiffTrackerState) handleEgressAssignment(input UpdatePodInputType) LocationData {
	dt.UpdateK8sPod(input)
	locationData := dt.GetSyncNRPLocationsAddresses()
	return locationData
}
