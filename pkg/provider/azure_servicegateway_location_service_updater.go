package provider

import (
	"context"
	"encoding/json"
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

func newLocationAndNRPServiceBatchUpdater(az *Cloud) *locationAndNRPServiceBatchUpdater {
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

func (az *Cloud) TriggerLocationAndNRPServiceBatchUpdate() {
	select {
	case az.locationAndNRPServiceBatchUpdater.(*locationAndNRPServiceBatchUpdater).channelUpdateTrigger <- true:
		// trigger batch update
	default:
		// channel is full, do nothing
		klog.V(2).Info("az.locationAndNRPServiceBatchUpdater.channelUpdateTrigger is full. Batch update is already triggered.")
	}
}

func (updater *locationAndNRPServiceBatchUpdater) process(ctx context.Context) {
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process BEGIN: processing batch update\n")
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process BEGIN: difftracker:\n")
	logDiffTracker(updater.az.diffTracker)
	// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process BEGIN: subscription ID: %s\n", updater.az.SubscriptionID)
	// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process BEGIN: resource group: %s\n", updater.az.ResourceGroup)
	// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process BEGIN: location and NRP service batch updater started\n")

	serviceLoadBalancerList := updater.az.diffTracker.GetSyncLoadBalancerServices()
	serviceNATGatewayList := updater.az.diffTracker.GetSyncNRPNATGateways()

	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: serviceLoadBalancerList:\n")
	logObject(serviceLoadBalancerList)
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: serviceNATGatewayList:\n")
	logObject(serviceNATGatewayList)
	// Add services
	if serviceLoadBalancerList.Additions.Len() > 0 || serviceNATGatewayList.Additions.Len() > 0 {
		createServicesRequestDTO := difftracker.MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
			difftracker.SyncServicesReturnType{
				Additions: serviceLoadBalancerList.Additions,
				Removals:  nil,
			},
			difftracker.SyncServicesReturnType{
				Additions: serviceNATGatewayList.Additions,
				Removals:  nil,
			},
			updater.az.SubscriptionID,
			updater.az.ResourceGroup)
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: createServicesRequestDTO:\n")
		logObject(createServicesRequestDTO)
		// createServicesResponseDTO := NRPAPIClientUpdateNRPServices(ctx, createServicesRequestDTO, updater.az.SubscriptionID, updater.az.ResourceGroup)
		// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: createServicesResponseDTO:\n")
		err := updater.az.UpdateNRPSGWServices(ctx, updater.az.ServiceGatewayResourceName, createServicesRequestDTO)
		// logObject(err)
		if err != nil {
			klog.Errorf("locationAndNRPServiceBatchUpdater.process: failed to create services: %+v\n", err)
			return
		}
		if serviceLoadBalancerList.Additions.Len() > 0 {
			updater.az.diffTracker.UpdateNRPLoadBalancers(
				difftracker.SyncServicesReturnType{
					Additions: serviceLoadBalancerList.Additions,
					Removals:  nil,
				},
			)
		}
		if serviceNATGatewayList.Additions.Len() > 0 {
			updater.az.diffTracker.UpdateNRPNATGateways(
				difftracker.SyncServicesReturnType{
					Additions: serviceNATGatewayList.Additions,
					Removals:  nil,
				},
			)
		}

		// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: successfully created services\n")
	}

	// Update locations and addresses for the added and deleted services
	for _, serviceName := range serviceLoadBalancerList.Additions.UnsortedList() {
		updater.az.diffTracker.LocalServiceNameToNRPServiceMap.LoadOrStore(strings.ToLower(serviceName), -34)

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
		// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: Additions updateK8sEndpointsInputType after range:\n")
		// logObject(updateK8sEndpointsInputType)
		if len(updateK8sEndpointsInputType.NewAddresses) > 0 {
			updater.az.diffTracker.UpdateK8sEndpoints(updateK8sEndpointsInputType)
		}
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
		// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: Removals updateK8sEndpointsInputType after range:\n")
		// logObject(updateK8sEndpointsInputType)
		if len(updateK8sEndpointsInputType.OldAddresses) > 0 {
			updater.az.diffTracker.UpdateK8sEndpoints(updateK8sEndpointsInputType)
		}
	}

	// Update all locations and addresses
	locationData := updater.az.diffTracker.GetSyncLocationsAddresses()
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: locationData:\n")
	logObject(locationData)
	if len(locationData.Locations) > 0 {
		locationDataRequestDTO := difftracker.MapLocationDataToDTO(locationData)
		// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: locationDataRequestDTO:\n")
		// logObject(locationDataRequestDTO)
		// locationDataResponseDTO := NRPAPIClientUpdateNRPLocations(ctx, locationDataRequestDTO, updater.az.SubscriptionID, updater.az.ResourceGroup)
		locationDataResponseDTO := updater.az.UpdateNRPSGWAddressLocations(ctx, updater.az.ServiceGatewayResourceName, locationDataRequestDTO)
		// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: locationDataResponseDTO:\n")
		// logObject(locationDataResponseDTO)
		if locationDataResponseDTO == nil {
			updater.az.diffTracker.UpdateLocationsAddresses(locationData)
		} else {
			klog.Errorf("locationAndNRPServiceBatchUpdater.process: failed to update locations and addresses: %v\n", locationDataResponseDTO)
			return
		}
		// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: successfully updated locations and addresses\n")
	}

	// Remove services
	if serviceLoadBalancerList.Removals.Len() > 0 || serviceNATGatewayList.Removals.Len() > 0 {
		removeServicesRequestDTO := difftracker.MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
			difftracker.SyncServicesReturnType{
				Additions: nil,
				Removals:  serviceLoadBalancerList.Removals,
			},
			difftracker.SyncServicesReturnType{
				Additions: nil,
				Removals:  serviceNATGatewayList.Removals,
			},
			updater.az.SubscriptionID,
			updater.az.ResourceGroup)
		// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: removeServicesRequestDTO:\n")
		// logObject(removeServicesRequestDTO)
		removeServicesResponseDTO := updater.az.UpdateNRPSGWServices(ctx, updater.az.ServiceGatewayResourceName, removeServicesRequestDTO)
		if removeServicesResponseDTO == nil {
			if serviceLoadBalancerList.Removals.Len() > 0 {
				updater.az.diffTracker.UpdateNRPLoadBalancers(
					difftracker.SyncServicesReturnType{
						Additions: nil,
						Removals:  serviceLoadBalancerList.Removals,
					})
			}
			if serviceNATGatewayList.Removals.Len() > 0 {
				updater.az.diffTracker.UpdateNRPNATGateways(
					difftracker.SyncServicesReturnType{
						Additions: nil,
						Removals:  serviceNATGatewayList.Removals,
					})
			}
		} else {
			klog.Errorf("locationAndNRPServiceBatchUpdater.process: failed to remove services: %v\n", removeServicesResponseDTO)
			return
		}

		// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: successfully removed services: %v\n", serviceLoadBalancerList.Removals)
	}

	// klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process END: processing batch update")
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process END: difftracker:")
	logDiffTracker(updater.az.diffTracker)
}

func logDiffTracker(dt interface{}) {
	bytes, err := json.MarshalIndent(dt, "", " ")
	if err != nil {
		klog.Errorf("Failed to marshal diffTracker: %v", err)
		return
	}
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: diff tracker: %s", string(bytes))
}

func (updater *locationAndNRPServiceBatchUpdater) addOperation(operation batchOperation) batchOperation {
	// This is a no-op function. Add operation is handled via the DiffTracker APIs.
	return operation
}

func (updater *locationAndNRPServiceBatchUpdater) removeOperation(name string) {
	// This is a no-op function. Remove operation is handled via the DiffTracker APIs.
	return
}

func logObject(data interface{}) {
	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		klog.Errorf("Failed to marshal: %v", err)
		return
	}
	klog.Infof("CLB-ENECHITOAIA - above object: %s", string(bytes))
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
