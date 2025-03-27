package difftracker

func (dt *DiffTracker) handleService(input UpdateK8sResource) SyncServicesReturnType {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.UpdateK8service(input)
	SyncDiffTrackerReturnType := dt.GetSyncLoadBalancerServices()
	return SyncDiffTrackerReturnType
}

func (dt *DiffTracker) handleEgress(input UpdateK8sResource) SyncServicesReturnType {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.UpdateK8sEgress(input)
	SyncDiffTrackerReturnType := dt.GetSyncNRPNATGateways()
	return SyncDiffTrackerReturnType
}

func (dt *DiffTracker) handleEndpoints(input UpdateK8sEndpointsInputType) LocationData {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.UpdateK8sEndpoints(input)
	locationData := dt.GetSyncLocationsAddresses()
	return locationData
}

func (dt *DiffTracker) handleEgressAssignment(input UpdatePodInputType) LocationData {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.UpdateK8sPod(input)
	locationData := dt.GetSyncLocationsAddresses()
	return locationData
}
