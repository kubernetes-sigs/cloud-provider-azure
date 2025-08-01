package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
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

func (updater *locationAndNRPServiceBatchUpdater) process(ctx context.Context) {
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process BEGIN: processing batch update\n")
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process BEGIN: difftracker:\n")
	logDiffTracker(updater.az.diffTracker)
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process BEGIN: subscription ID: %s\n", updater.az.SubscriptionID)
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process BEGIN: resource group: %s\n", updater.az.ResourceGroup)
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process BEGIN: location and NRP service batch updater started\n")

	serviceLoadBalancerList := updater.az.diffTracker.GetSyncLoadBalancerServices()
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: serviceLoadBalancerList:\n")
	logObject(serviceLoadBalancerList)
	// Add services
	if serviceLoadBalancerList.Additions.Len() > 0 {
		createServicesRequestDTO := difftracker.MapLoadBalancerUpdatesToServicesDataDTO(
			difftracker.SyncServicesReturnType{
				Additions: serviceLoadBalancerList.Additions,
				Removals:  nil,
			},
			updater.az.SubscriptionID,
			updater.az.ResourceGroup)
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: createServicesRequestDTO:\n")
		logObject(createServicesRequestDTO)
		createServicesResponseDTO := NRPAPIClientUpdateNRPServices(ctx, createServicesRequestDTO, updater.az.SubscriptionID, updater.az.ResourceGroup)
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: createServicesResponseDTO:\n")
		logObject(createServicesResponseDTO)
		if createServicesResponseDTO == nil {
			updater.az.diffTracker.UpdateNRPLoadBalancers(
				difftracker.SyncServicesReturnType{
					Additions: serviceLoadBalancerList.Additions,
					Removals:  nil,
				},
			)
		} else {
			klog.Errorf("locationAndNRPServiceBatchUpdater.process: failed to create services: %+v\n", createServicesResponseDTO)
			return
		}
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: successfully created services\n")
	}

	// Update locations and addresses for the added and deleted services
	for _, serviceName := range serviceLoadBalancerList.Additions.UnsortedList() {
		updater.az.diffTracker.LocalServiceNameToNRPServiceMap.LoadOrStore(serviceName, struct{}{})

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
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: Additions updateK8sEndpointsInputType after range:\n")
		logObject(updateK8sEndpointsInputType)
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
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: Removals updateK8sEndpointsInputType after range:\n")
		logObject(updateK8sEndpointsInputType)
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
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: locationDataRequestDTO:\n")
		logObject(locationDataRequestDTO)
		locationDataResponseDTO := NRPAPIClientUpdateNRPLocations(ctx, locationDataRequestDTO, updater.az.SubscriptionID, updater.az.ResourceGroup)
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: locationDataResponseDTO:\n")
		logObject(locationDataResponseDTO)
		if locationDataResponseDTO == nil {
			updater.az.diffTracker.UpdateLocationsAddresses(locationData)
		} else {
			klog.Errorf("locationAndNRPServiceBatchUpdater.process: failed to update locations and addresses: %v\n", locationDataResponseDTO)
			return
		}
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: successfully updated locations and addresses\n")
	}

	// Remove services
	if serviceLoadBalancerList.Removals.Len() > 0 {
		removeServicesRequestDTO := difftracker.MapLoadBalancerUpdatesToServicesDataDTO(
			difftracker.SyncServicesReturnType{
				Additions: nil,
				Removals:  serviceLoadBalancerList.Removals,
			},
			updater.az.SubscriptionID,
			updater.az.ResourceGroup)
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: removeServicesRequestDTO: %+v\n", removeServicesRequestDTO)
		// TODO (enechitoaia): discuss about ServiceGatewayName (VnetName?)
		removeServicesResponseDTO := NRPAPIClientUpdateNRPServices(ctx, removeServicesRequestDTO, updater.az.SubscriptionID, updater.az.ResourceGroup)
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: removeServicesResponseDTO: %+v\n", removeServicesResponseDTO)
		if removeServicesResponseDTO == nil {
			updater.az.diffTracker.UpdateNRPLoadBalancers(
				difftracker.SyncServicesReturnType{
					Additions: nil,
					Removals:  serviceLoadBalancerList.Removals,
				})
		} else {
			klog.Errorf("locationAndNRPServiceBatchUpdater.process: failed to remove services: %v\n", removeServicesResponseDTO)
			return
		}

		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: successfully removed services: %v\n", serviceLoadBalancerList.Removals)
	}

	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process END: processing batch update")
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

func logObject(data interface{}) {
	bytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		klog.Errorf("Failed to marshal: %v", err)
		return
	}
	klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: object: %s", string(bytes))
}

func NRPAPIClientUpdateNRPLocations(
	ctx context.Context,
	locationDataRequestDTO difftracker.LocationsDataDTO,
	subscriptionId string,
	resourceGroupName string,
) error {
	klog.V(2).Infof("NRPAPIClientUpdateNRPLocations: request DTO: %+v", locationDataRequestDTO)

	// Marshal the DTO to JSON
	payload, err := json.Marshal(locationDataRequestDTO)
	if err != nil {
		return fmt.Errorf("failed to marshal request DTO: %w", err)
	}

	// Construct the in-cluster service URL
	url := fmt.Sprintf("http://nrp-bypass/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/serviceGateways/ServiceGateway/UpdateLocations",
		subscriptionId, resourceGroupName)

	// Create the HTTP request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send the request
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check for non-2xx status codes
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("received non-success status code: %d", resp.StatusCode)
	}

	klog.V(2).Infof("NRPAPIClientUpdateNRPLocations: successfully updated locations")
	return nil
}

func NRPAPIClientUpdateNRPServices(ctx context.Context, createServicesRequestDTO difftracker.ServicesDataDTO, subscriptionId string, resourceGroupName string) error {
	klog.V(2).Infof("NRPAPIClientUpdateNRPServices: request DTO: %+v", createServicesRequestDTO)

	// Marshal the DTO to JSON
	payload, err := json.Marshal(createServicesRequestDTO)
	if err != nil {
		return fmt.Errorf("failed to marshal request DTO: %w", err)
	}

	// Construct the in-cluster service URL
	// Replace with actual values or inject them via config/env
	url := fmt.Sprintf("http://nrp-bypass/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/serviceGateways/ServiceGateway/UpdateServices",
		subscriptionId, resourceGroupName)

	// Create the HTTP request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send the request
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	// Check for non-2xx status codes
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("received non-success status code: %d", resp.StatusCode)
	}

	klog.V(2).Infof("NRPAPIClientUpdateNRPServices: successfully updated services")
	return nil
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

func (az *Cloud) setUpPodInformerForEgress() {
	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		az.KubeClient,
		ResyncPeriod(1*time.Second)(),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = consts.PodLabelServiceEgressGateway
		}),
	)

	az.podLister = podInformerFactory.Core().V1().Pods().Lister()

	podInformer := podInformerFactory.Core().V1().Pods()
	_, _ = podInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)

				if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
					klog.Errorf("Pod %s/%s has no labels or staticGatewayConfiguration label. Cannot process add event.",
						pod.Namespace, pod.Name)
					return
				}

				staticGatewayConfigurationName := pod.Labels[consts.PodLabelServiceEgressGateway]
				_, ok := az.diffTracker.LocalServiceNameToNRPServiceMap.Load(staticGatewayConfigurationName)
				if ok {
					klog.Infof("Pod %s/%s has static gateway configuration annotation. Found in localServiceNameToNRPServiceMap.",
						pod.Namespace, pod.Name)
					az.diffTracker.UpdateK8sPod(
						difftracker.UpdatePodInputType{
							PodOperation:           difftracker.ADD,
							PublicOutboundIdentity: staticGatewayConfigurationName,
							Location:               pod.Status.HostIP,
							Address:                pod.Status.PodIP,
						},
					)
					az.TriggerLocationAndNRPServiceBatchUpdate()
				} else {
					klog.Infof("Pod %s/%s has static gateway configuration annotation. Not found in localServiceNameToNRPServiceMap.",
						pod.Namespace, pod.Name)
					key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
					if err != nil {
						klog.Errorf("Failed to get key for pod %s/%s: %v", pod.Namespace, pod.Name, err)
						return
					}
					az.diffTracker.PodEgressQueue.Add(difftracker.PodCrudEvent{
						Key:       key,
						EventType: "Add",
					})
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldPod := oldObj.(*v1.Pod)
				newPod := newObj.(*v1.Pod)
				var (
					prevEgressGatewayName = ""
					currEgressGatewayName = ""
				)
				if oldPod.Labels != nil {
					prevEgressGatewayName = oldPod.Labels[consts.PodLabelServiceEgressGateway]
				}
				if newPod.Labels != nil {
					currEgressGatewayName = newPod.Labels[consts.PodLabelServiceEgressGateway]
				}

				if prevEgressGatewayName == currEgressGatewayName {
					klog.Infof("Pod %s/%s has no change in static gateway configuration. No action needed.",
						newPod.Namespace, newPod.Name)
					return
				}

				if prevEgressGatewayName != "" {
					counter, ok := az.diffTracker.LocalServiceNameToNRPServiceMap.Load(prevEgressGatewayName)
					if ok {
						if counter.(int) > 1 {
							az.diffTracker.UpdateK8sPod(
								difftracker.UpdatePodInputType{
									PodOperation:           difftracker.REMOVE,
									PublicOutboundIdentity: prevEgressGatewayName,
									Location:               oldPod.Status.HostIP,
									Address:                oldPod.Status.PodIP,
								},
							)
							az.TriggerLocationAndNRPServiceBatchUpdate()
						} else {
							key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(oldPod)
							if err != nil {
								klog.Errorf("Failed to get key for pod %s/%s: %v", oldPod.Namespace, oldPod.Name, err)
								return
							}
							az.diffTracker.PodEgressQueue.Add(difftracker.PodCrudEvent{
								Key:       key,
								EventType: "Delete",
							})
						}
					} else {
						// error
						klog.Errorf("Pod %s/%s has static gateway configuration %s, but it is not found in localServiceNameToNRPServiceMap. Cannot decrement its counter.",
							oldPod.Namespace, oldPod.Name, prevEgressGatewayName)
						return
					}
				}

				if currEgressGatewayName != "" {
					_, ok := az.diffTracker.LocalServiceNameToNRPServiceMap.Load(currEgressGatewayName)
					if ok {
						klog.Infof("Pod %s/%s has static gateway configuration annotation. Found in localServiceNameToNRPServiceMap.",
							newPod.Namespace, newPod.Name)
						az.diffTracker.UpdateK8sPod(
							difftracker.UpdatePodInputType{
								PodOperation:           difftracker.ADD,
								PublicOutboundIdentity: currEgressGatewayName,
								Location:               newPod.Status.HostIP,
								Address:                newPod.Status.PodIP,
							},
						)
						az.TriggerLocationAndNRPServiceBatchUpdate()
					} else {
						klog.Infof("Pod %s/%s has static gateway configuration annotation. Not found in localServiceNameToNRPServiceMap.",
							newPod.Namespace, newPod.Name)
						key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(newPod)
						if err != nil {
							klog.Errorf("Failed to get key for pod %s/%s: %v", newPod.Namespace, newPod.Name, err)
							return
						}
						az.diffTracker.PodEgressQueue.Add(difftracker.PodCrudEvent{
							Key:       key,
							EventType: "Add",
						})
					}
				}
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
					klog.Errorf("Pod %s/%s has no labels. Cannot process delete event.",
						pod.Namespace, pod.Name)
					return
				}

				staticGatewayConfigurationName := pod.Labels[consts.PodLabelServiceEgressGateway]
				counter, ok := az.diffTracker.LocalServiceNameToNRPServiceMap.Load(staticGatewayConfigurationName)
				if ok {
					if counter.(int) > 1 {
						az.diffTracker.UpdateK8sPod(
							difftracker.UpdatePodInputType{
								PodOperation:           difftracker.REMOVE,
								PublicOutboundIdentity: staticGatewayConfigurationName,
								Location:               pod.Status.HostIP,
								Address:                pod.Status.PodIP,
							},
						)
						az.TriggerLocationAndNRPServiceBatchUpdate()
					} else {
						key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pod)
						if err != nil {
							klog.Errorf("Failed to get key for pod %s/%s: %v", pod.Namespace, pod.Name, err)
							return
						}
						az.diffTracker.PodEgressQueue.Add(difftracker.PodCrudEvent{
							Key:       key,
							EventType: "Delete",
						})

						az.diffTracker.UpdateK8sEgress(difftracker.UpdateK8sResource{
							Operation: difftracker.REMOVE,
							ID:        staticGatewayConfigurationName,
						})
						az.diffTracker.UpdateK8sPod(difftracker.UpdatePodInputType{
							PodOperation:           difftracker.REMOVE,
							PublicOutboundIdentity: staticGatewayConfigurationName,
							Location:               pod.Status.HostIP,
							Address:                pod.Status.PodIP,
						})
						az.TriggerLocationAndNRPServiceBatchUpdate()
					}
				} else {
					// error
					klog.Errorf("Pod %s/%s has static gateway configuration %s, but it is not found in localServiceNameToNRPServiceMap. Cannot decrement its counter.",
						pod.Namespace, pod.Name, staticGatewayConfigurationName)
				}
			},
		})
}

// ResyncPeriod returns a function that generates a randomized resync duration
// to prevent controllers from syncing in lock-step and overloading the API server.
func ResyncPeriod(base time.Duration) func() time.Duration {
	return func() time.Duration {
		n, _ := rand.Int(rand.Reader, big.NewInt(1000))
		factor := float64(n.Int64())/1000.0 + 1.0
		return time.Duration(float64(base.Nanoseconds()) * factor)
	}
}

type podEgressResourceUpdater struct {
	az *Cloud
}

func newPodEgressResourceUpdater(az *Cloud) *podEgressResourceUpdater {
	return &podEgressResourceUpdater{
		az: az,
	}
}

func (updater *podEgressResourceUpdater) run(ctx context.Context) {
	klog.V(2).Info("podEgressResourceUpdater.run: started")
	for {
		select {
		case <-ctx.Done():
			klog.Infof("podEgressResourceUpdater.run: stopped due to context cancellation")
			return
		default:
			updater.process(ctx)
		}
	}
}

func (updater *podEgressResourceUpdater) process(ctx context.Context) {
	klog.Infof("podEgressResourceUpdater.process BEGIN: processing pod egress updates")

	for {
		item, shutdown := updater.az.diffTracker.PodEgressQueue.Get()
		if shutdown {
			klog.Infof("podEgressResourceUpdater.process: queue is shutting down")
			return
		}

		event := item

		klog.Infof("Processing event: %s for pod: %s", event.EventType, event.Key)

		namespace, name, err := cache.SplitMetaNamespaceKey(event.Key)
		if err != nil {
			klog.Errorf("Failed to split key %s: %v", event.Key, err)
			updater.az.diffTracker.PodEgressQueue.Done(item)
			continue
		}

		pod, err := updater.az.podLister.Pods(namespace).Get(name)
		if err != nil {
			klog.Errorf("Pod %s/%s not found, skipping event processing: %v", namespace, name, err)
			updater.az.diffTracker.PodEgressQueue.Done(item)
			continue
		}

		location := pod.Status.HostIP
		address := pod.Status.PodIP
		service := pod.Labels[consts.PodLabelServiceEgressGateway]

		switch event.EventType {
		case "Add":
			_, ok := updater.az.diffTracker.LocalServiceNameToNRPServiceMap.Load(service)
			if ok {
				updater.az.diffTracker.UpdateK8sPod(difftracker.UpdatePodInputType{
					PodOperation:           difftracker.ADD,
					PublicOutboundIdentity: service,
					Location:               location,
					Address:                address,
				})
				updater.az.TriggerLocationAndNRPServiceBatchUpdate()
			} else {
				// TODO(enechitoaia): createOrUpdatePip()
				// TODO(enechitoaia): createOrUpdateNatGateway(NGW, ServiceName, SGW)
				updater.az.diffTracker.UpdateK8sEgress(difftracker.UpdateK8sResource{
					Operation: difftracker.ADD,
					ID:        service,
				})
				updater.az.diffTracker.UpdateK8sPod(difftracker.UpdatePodInputType{
					PodOperation:           difftracker.ADD,
					PublicOutboundIdentity: service,
					Location:               location,
					Address:                address,
				})
				updater.az.TriggerLocationAndNRPServiceBatchUpdate()
			}
		case "Delete":
			// TODO(enechitoaia): createOrUpdateService(Service, null)
			// TODO(enechitoaia): deleteNatGateway()
			//  TODO(enechitoaia): deletePip()
		default:
			klog.Warningf("Unknown event type: %s for pod: %s", event.EventType, event.Key)
		}

		updater.az.diffTracker.PodEgressQueue.Done(item)
	}
}

func (updater *podEgressResourceUpdater) addOperation(operation batchOperation) batchOperation {
	// This is a no-op function. Add operation is handled via the DiffTracker APIs.
	return operation
}

func (updater *podEgressResourceUpdater) removeOperation(name string) {
	// This is a no-op function. Remove operation is handled via the DiffTracker APIs.
	return
}
