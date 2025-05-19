package provider

import (
	"context"
	"strings"

	discovery_v1 "k8s.io/api/discovery/v1"
	"k8s.io/klog/v2"
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
	serviceLoadBalancerList := updater.az.difftracker.GetSyncLoadBalancerServices()

	// Add services
	createServicesRequestDTO := MapLoadBalancerUpdatesToServiceDataDTO(serviceLoadBalancerList.Additions, updater.az.SubscriptionID, updater.az.ResourceGroup)
	if len(createServicesRequestDTO) > 0 {
		createServicesResponseDTO := NRPAPIClient.UpdateNRPServices(ctx, createServicesRequestDTO)

		if createServicesResponseDTO.Error == nil {
			az.difftracker.UpdateNRPLoadBalancers(serviceLoadBalancerList.Additions)
		} else {
			klog.Errorf("locationAndNRPServiceBatchUpdater.process: failed to create services: %v", createServicesResponseDTO.Error)
			return
		}
	}

	// Update locations and addresses for the added and deleted services
	for _, serviceName := range serviceLoadBalancerList.Additions {
		updater.az.localServiceNameToNRPServiceMap.LoadOrStore(serviceName, struct{}{})

		updateK8sEndpointsInputType := UpdateK8sEndpointsInputType{
			inboundIdentity: serviceName,
			oldAddresses:    nil,
			newAddresses:    map[string]string{},
		}
		updater.az.endpointSlicesCache.Range(func(_, value interface{}) bool {
			endpointSlice := value.(*discovery_v1.EndpointSlice)
			serviceUID, loaded := getServiceUIDOfEndpointSlice(endpointSlice)
			if loaded && strings.EqualFold(serviceUID, serviceName) {
				updateK8sEndpointsInputType.newAddresses = mergeMaps(updateK8sEndpointsInputType.newAddresses, updater.az.getPodIPToNodeIPMapFromEndpointSlice(endpointSlice, false))
			}
			return true
		})

		updater.az.difftracker.UpdateK8sEndpoints(updateK8sEndpointsInputType)
	}

	for _, serviceName := range serviceLoadBalancerList.Deletions {
		updateK8sEndpointsInputType := UpdateK8sEndpointsInputType{
			inboundIdentity: serviceName,
			oldAddresses:    map[string]string{},
			newAddresses:    nil,
		}
		updater.az.endpointSlicesCache.Range(func(_, value interface{}) bool {
			endpointSlice := value.(*discovery_v1.EndpointSlice)
			serviceUID, loaded := getServiceUIDOfEndpointSlice(endpointSlice)
			if loaded && strings.EqualFold(serviceUID, serviceName) {
				updateK8sEndpointsInputType.oldAddresses = mergeMaps(updateK8sEndpointsInputType.oldAddresses, updater.az.getPodIPToNodeIPMapFromEndpointSlice(endpointSlice, false))
			}
			return true
		})

		updater.az.difftracker.UpdateK8sEndpoints(updateK8sEndpointsInputType)
	}

	// Update all locations and addresses
	locationData := updater.az.difftracker.GetSyncLocationsAddresses()
	if len(locationData) > 0 {
		locationDataRequestDTO := MapLocationDataToDTO(locationData)
		locationDataResponseDTO := NRPAPIClient.UpdateNRPLocations(ctx, locationDataRequestDTO)

		if locationDataResponseDTO.Error == nil {
			az.difftracker.UpdateLocationsAddresses(locationData)
		} else {
			klog.Errorf("locationAndNRPServiceBatchUpdater.process: failed to update locations and addresses: %v", locationDataResponseDTO.Error)
			return
		}
	}

	// Remove services
	removeServicesRequestDTO := MapLoadBalancerUpdatesToServiceDataDTO(serviceLoadBalancerList.Removals, updater.az.SubscriptionID, updater.az.ResourceGroup)
	if len(removeServicesRequestDTO) > 0 {
		removeServicesResponseDTO := NRPAPIClient.UpdateNRPServices(ctx, removeServicesRequestDTO)

		if removeServicesResponseDTO.Error == nil {
			az.difftracker.UpdateNRPLoadBalancers(serviceLoadBalancerList.Additions)
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
