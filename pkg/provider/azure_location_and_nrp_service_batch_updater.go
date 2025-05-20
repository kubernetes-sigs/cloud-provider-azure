package provider

import (
	"context"
	"strings"

	discovery_v1 "k8s.io/api/discovery/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/difftracker"
)

// LocationAndNRPServiceBatchUpdater is a batch processor for updating NRP locations and services.
type locationAndNRPServiceBatchUpdater struct {
	az                   *Cloud
	channelUpdateTrigger chan bool
}

func newLocationAndNRPServiceBatchUpdater(az *Cloud) batchProcessor {
	return &locationAndNRPServiceBatchUpdater{
		az:                   az,
		channelUpdateTrigger: make(chan bool, 1),
	}
}

func (updater *locationAndNRPServiceBatchUpdater) run(ctx context.Context) {
	klog.V(2).Info("locationAndNRPServiceBatchUpdater.run: started")
	for {
		select {
		case <-updater.channelUpdateTrigger:
			updater.process(ctx)
		case <-ctx.Done():
			klog.Infof("locationAndNRPServiceBatchUpdater.run: stopped due to context cancellation")
			return
		}
	}
}

func (updater *locationAndNRPServiceBatchUpdater) process(ctx context.Context) {
	serviceLoadBalancerList := updater.az.diffTracker.GetSyncLoadBalancerServices()

	// Add services
	if serviceLoadBalancerList.Additions.Len() > 0 {
		// createServicesRequestDTO := difftracker.MapLoadBalancerUpdatesToServicesDataDTO(
		// 	difftracker.SyncServicesReturnType{
		// 		Additions: serviceLoadBalancerList.Additions,
		// 		Removals:  nil,
		// 	},
		// 	updater.az.SubscriptionID,
		// 	updater.az.ResourceGroup)
		// createServicesResponseDTO := NRPAPIClient.UpdateNRPServices(ctx, createServicesRequestDTO)

		createServicesResponseDTO := struct {
			Error error
		}{
			Error: nil,
		}
		if createServicesResponseDTO.Error == nil {
			updater.az.diffTracker.UpdateNRPLoadBalancers(
				difftracker.SyncServicesReturnType{
					Additions: serviceLoadBalancerList.Additions,
					Removals:  nil,
				},
			)
		} else {
			klog.Errorf("locationAndNRPServiceBatchUpdater.process: failed to create services: %v", createServicesResponseDTO.Error)
			return
		}
	}

	// Update locations and addresses for the added and deleted services
	for _, serviceName := range serviceLoadBalancerList.Additions.UnsortedList() {
		updater.az.localServiceNameToNRPServiceMap.LoadOrStore(serviceName, struct{}{})

		updateK8sEndpointsInputType := difftracker.UpdateK8sEndpointsInputType{
			InboundIdentity: serviceName,
			OldAddresses:    nil,
			NewAddresses:    map[string]string{},
		}
		updater.az.endpointSlicesCache.Range(func(_, value interface{}) bool {
			endpointSlice := value.(*discovery_v1.EndpointSlice)
			serviceUID, loaded := getServiceUIDOfEndpointSlice(endpointSlice)
			if loaded && strings.EqualFold(serviceUID, serviceName) {
				updateK8sEndpointsInputType.NewAddresses = mergeMaps(updateK8sEndpointsInputType.NewAddresses, updater.az.getPodIPToNodeIPMapFromEndpointSlice(endpointSlice, false))
			}
			return true
		})

		updater.az.diffTracker.UpdateK8sEndpoints(updateK8sEndpointsInputType)
	}

	for _, serviceName := range serviceLoadBalancerList.Removals.UnsortedList() {
		updateK8sEndpointsInputType := difftracker.UpdateK8sEndpointsInputType{
			InboundIdentity: serviceName,
			OldAddresses:    map[string]string{},
			NewAddresses:    nil,
		}
		updater.az.endpointSlicesCache.Range(func(_, value interface{}) bool {
			endpointSlice := value.(*discovery_v1.EndpointSlice)
			serviceUID, loaded := getServiceUIDOfEndpointSlice(endpointSlice)
			if loaded && strings.EqualFold(serviceUID, serviceName) {
				updateK8sEndpointsInputType.OldAddresses = mergeMaps(updateK8sEndpointsInputType.OldAddresses, updater.az.getPodIPToNodeIPMapFromEndpointSlice(endpointSlice, false))
			}
			return true
		})

		updater.az.diffTracker.UpdateK8sEndpoints(updateK8sEndpointsInputType)
	}

	// Update all locations and addresses
	locationData := updater.az.diffTracker.GetSyncLocationsAddresses()
	if len(locationData.Locations) > 0 {
		// locationDataRequestDTO := difftracker.MapLocationDataToDTO(locationData)
		// locationDataResponseDTO := NRPAPIClient.UpdateNRPLocations(ctx, locationDataRequestDTO)
		locationDataResponseDTO := struct {
			Error error
		}{
			Error: nil,
		}
		if locationDataResponseDTO.Error == nil {
			updater.az.diffTracker.UpdateLocationsAddresses(locationData)
		} else {
			klog.Errorf("locationAndNRPServiceBatchUpdater.process: failed to update locations and addresses: %v", locationDataResponseDTO.Error)
			return
		}
	}

	// Remove services
	if serviceLoadBalancerList.Removals.Len() > 0 {
		// removeServicesRequestDTO := difftracker.MapLoadBalancerUpdatesToServicesDataDTO(
		// 	difftracker.SyncServicesReturnType{
		// 		Additions: nil,
		// 		Removals:  serviceLoadBalancerList.Removals,
		// 	},
		// 	updater.az.SubscriptionID,
		// 	updater.az.ResourceGroup)
		// removeServicesResponseDTO := NRPAPIClient.UpdateNRPServices(ctx, removeServicesRequestDTO)

		removeServicesResponseDTO := struct {
			Error error
		}{
			Error: nil,
		}

		if removeServicesResponseDTO.Error == nil {
			updater.az.diffTracker.UpdateNRPLoadBalancers(
				difftracker.SyncServicesReturnType{
					Additions: nil,
					Removals:  serviceLoadBalancerList.Removals,
				})
		} else {
			klog.Errorf("locationAndNRPServiceBatchUpdater.process: failed to remove services: %v", removeServicesResponseDTO.Error)
			return
		}
	}
}

func mergeMaps[K comparable, V any](maps ...map[K]V) map[K]V {
	result := make(map[K]V)
	for _, m := range maps {
		for k, v := range m {
			result[k] = v
		}
	}
	return result
}

func (updater *locationAndNRPServiceBatchUpdater) addOperation(operation batchOperation) batchOperation {
	// This is a no-op function. Add operation is handled via the DiffTracker APIs.
	return operation
}

func (updater *locationAndNRPServiceBatchUpdater) removeOperation(name string) {
	// This is a no-op function. Remove operation is handled via the DiffTracker APIs.
	return
}
