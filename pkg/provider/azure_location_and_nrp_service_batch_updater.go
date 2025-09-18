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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/difftracker"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
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
		createServicesResponseDTO := NRPAPIClientUpdateNRPServices(ctx, createServicesRequestDTO, updater.az.SubscriptionID, updater.az.ResourceGroup)
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: createServicesResponseDTO:\n")
		logObject(createServicesResponseDTO)
		if createServicesResponseDTO == nil {
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
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: removeServicesRequestDTO:\n")
		logObject(removeServicesRequestDTO)
		// TODO (enechitoaia): discuss about ServiceGatewayName (VnetName?)
		removeServicesResponseDTO := NRPAPIClientUpdateNRPServices(ctx, removeServicesRequestDTO, updater.az.SubscriptionID, updater.az.ResourceGroup)
		klog.Infof("CLB-ENECHITOAIA-locationAndNRPServiceBatchUpdater.process: removeServicesResponseDTO:\n")
		logObject(removeServicesResponseDTO)
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
	return nil // TODO (enechitoaia): re-enable when NRP bypass is ready
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
	return nil // TODO (enechitoaia): re-enable when NRP bypass is ready
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

func (az *Cloud) podInformerAddPod(pod *v1.Pod) {
	klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has been added\n", pod.Namespace, pod.Name)
	if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
		klog.Errorf("Pod %s/%s has no labels or staticGatewayConfiguration label. Cannot process add event.",
			pod.Namespace, pod.Name)
		return
	}
	// logObject(pod)
	if pod.Status.HostIP == "" || pod.Status.PodIP == "" {
		klog.Errorf("Pod %s/%s has no HostIP or PodIP. Cannot process add event.",
			pod.Namespace, pod.Name)
		return
	}
	klog.Infof("CLB-ENECHITOAIA- Pod %s/%s has Location: %s, PodIP: %s\n", pod.Namespace, pod.Name, pod.Status.HostIP, pod.Status.PodIP)
	staticGatewayConfigurationName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
	klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has static gateway configuration: %s", pod.Namespace, pod.Name, staticGatewayConfigurationName)
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
		key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pod)
		if err != nil {
			klog.Errorf("Failed to get key for pod %s/%s: %v", pod.Namespace, pod.Name, err)
			return
		}
		az.diffTracker.PodEgressQueue.Add(difftracker.PodCrudEvent{
			AddPodEvent: difftracker.AddPodEvent{
				Key: key,
			},
			EventType: "Add",
		})
	}
}

func (az *Cloud) podInformerRemovePod(pod *v1.Pod) {
	klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has been deleted\n", pod.Namespace, pod.Name)
	if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
		klog.Errorf("Pod %s/%s has no labels. Cannot process delete event.",
			pod.Namespace, pod.Name)
		return
	}

	staticGatewayConfigurationName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
	counter, ok := az.diffTracker.LocalServiceNameToNRPServiceMap.Load(staticGatewayConfigurationName)
	klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has static gateway configuration: %s", pod.Namespace, pod.Name, staticGatewayConfigurationName)
	klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has Location: %s, PodIP: %s\n", pod.Namespace, pod.Name, pod.Status.HostIP, pod.Status.PodIP)
	// logObject(pod)
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
			az.diffTracker.PodEgressQueue.Add(difftracker.PodCrudEvent{
				DeletePodEvent: difftracker.DeletePodEvent{
					Location: pod.Status.HostIP,
					Address:  pod.Status.PodIP,
					Service:  staticGatewayConfigurationName,
				},
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
}

func (az *Cloud) setUpPodInformerForEgress(informerFactory informers.SharedInformerFactory) {
	klog.Infof("CLB-ENECHITOAIA-setUpPodInformerForEgress: setting up pod informer for egress with label selector: %s\n", consts.PodLabelServiceEgressGateway)
	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		az.KubeClient,
		ResyncPeriod(1*time.Second)(),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = consts.PodLabelServiceEgressGateway
		}),
	)
	podInformer := podInformerFactory.Core().V1().Pods().Informer()
	klog.Infof("podInformer: %v\n", podInformer)
	_, err := podInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				az.podInformerAddPod(pod)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldPod := oldObj.(*v1.Pod)
				newPod := newObj.(*v1.Pod)
				klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has been updated\n", newPod.Namespace, newPod.Name)
				// logObject(oldObj)
				// logObject(newObj)

				var (
					prevEgressGatewayName = ""
					currEgressGatewayName = ""
				)
				if oldPod.Labels != nil {
					prevEgressGatewayName = strings.ToLower(oldPod.Labels[consts.PodLabelServiceEgressGateway])
				}
				if newPod.Labels != nil {
					currEgressGatewayName = strings.ToLower(newPod.Labels[consts.PodLabelServiceEgressGateway])
				}

				podJustCompletedNodeIPAndPodIPInitialization := (oldPod.Status.HostIP == "" || oldPod.Status.PodIP == "") && (newPod.Status.HostIP != "" && newPod.Status.PodIP != "")
				if currEgressGatewayName != "" && podJustCompletedNodeIPAndPodIPInitialization {
					klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has just completed NodeIP and PodIP initialization. Treating as an Add event.",
						newPod.Namespace, newPod.Name)
					az.podInformerAddPod(newPod)
					return
				}

				if prevEgressGatewayName == currEgressGatewayName {
					klog.Infof("Pod %s/%s has no change in static gateway configuration. No action needed.",
						newPod.Namespace, newPod.Name)
					return
				}

				klog.Infof("CLB-ENECHITOAIA-Pod %s/%s has changed static gateway configuration from %s to %s",
					newPod.Namespace, newPod.Name, prevEgressGatewayName, currEgressGatewayName)
				if prevEgressGatewayName != "" {
					az.podInformerRemovePod(oldPod)
				}

				if currEgressGatewayName != "" {
					az.podInformerAddPod(newPod)
				}
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				az.podInformerRemovePod(pod)
			},
		})
	if err != nil {
		klog.Errorf("CLB-ENECHITOAIA-setUpPodInformerForEgress: failed to add event handlers to pod informer: %v\n", err)
		return
	} else {
		klog.Infof("CLB-ENECHITOAIA-setUpPodInformerForEgress: successfully added event handlers to pod informer\n")
	}

	az.podLister = podInformerFactory.Core().V1().Pods().Lister() // TODO (move it)
	klog.Infof("az.podLister: %v\n", az.podLister)
	klog.Infof("CLB-ENECHITOAIA-setUpPodInformerForEgress: starting pod informer factory\n")
	podInformerFactory.Start(wait.NeverStop)
	podInformerFactory.WaitForCacheSync(wait.NeverStop)
	klog.Infof("CLB-ENECHITOAIA-setUpPodInformerForEgress: end of setUpPodInformerForEgress\n")
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

		switch event.EventType {
		case "Add":
			eventData := event.AddPodEvent
			klog.Infof("Processing event: %s for pod: %s", event.EventType, eventData.Key)

			namespace, name, err := cache.SplitMetaNamespaceKey(eventData.Key)
			if err != nil {
				klog.Errorf("Failed to split key %s: %v", eventData.Key, err)
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
			service := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
			// TBD(enechitoaia): Handle any future pod annotations here

			klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: Pod %s/%s - Location: %s, Address: %s, Service: %s",
				namespace, name, location, address, service)

			_, ok := updater.az.diffTracker.LocalServiceNameToNRPServiceMap.Load(service)
			if ok {
				klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: Pod %s/%s service %s already exists in localServiceNameToNRPServiceMap",
					namespace, name, service)
				updater.az.diffTracker.UpdateK8sPod(difftracker.UpdatePodInputType{
					PodOperation:           difftracker.ADD,
					PublicOutboundIdentity: service,
					Location:               location,
					Address:                address,
				})
				updater.az.TriggerLocationAndNRPServiceBatchUpdate()
			} else {
				klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: Pod %s/%s service %s does not exist in localServiceNameToNRPServiceMap. Creating resources.",
					namespace, name, service)
				klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: Creating Public IP %s in resource group %s", service, updater.az.ResourceGroup)
				pipResource := armnetwork.PublicIPAddress{
					Name: to.Ptr(fmt.Sprintf("%s-pip", service)),
					ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
						updater.az.SubscriptionID, updater.az.ResourceGroup, service)),
					SKU: &armnetwork.PublicIPAddressSKU{
						Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandardV2),
					},
					Location: to.Ptr(updater.az.Location),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{ // TODO (enechitoaia): What properties should we use for the Public IP

						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
					},
				}
				updater.az.CreateOrUpdatePIPOutbound(updater.az.ResourceGroup, &pipResource)
				klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: Creating NAT Gateway %s in resource group %s", service, updater.az.ResourceGroup)
				natGatewayResource := armnetwork.NatGateway{
					Name: to.Ptr(service),
					ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/natGateways/%s",
						updater.az.SubscriptionID, updater.az.ResourceGroup, service)),
					SKU: &armnetwork.NatGatewaySKU{
						Name: to.Ptr(armnetwork.NatGatewaySKUNameStandardV2)},
					Location: to.Ptr(updater.az.Location),
					Properties: &armnetwork.NatGatewayPropertiesFormat{ // TODO (enechitoaia): What properties should we use for the Nat Gateway
						PublicIPAddresses: []*armnetwork.SubResource{
							{
								ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s-pip",
									updater.az.SubscriptionID, updater.az.ResourceGroup, service)),
							},
						},
					},
				}
				updater.az.createOrUpdateNatGateway(ctx, updater.az.ResourceGroup, natGatewayResource)
				// CALL NRP API TO CREATE THE SERVICE
				updater.az.diffTracker.UpdateK8sEgress(difftracker.UpdateK8sResource{
					Operation: difftracker.ADD,
					ID:        service,
				})

				// createOrUpdateService(service, natGatewayResource.ID)
				updater.az.diffTracker.UpdateK8sPod(difftracker.UpdatePodInputType{
					PodOperation:           difftracker.ADD,
					PublicOutboundIdentity: service,
					Location:               location,
					Address:                address,
				})
				updater.az.TriggerLocationAndNRPServiceBatchUpdate()
				klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: successfully added pod %s/%s for service %s", namespace, name, service)
			}
		case "Delete":
			eventData := event.DeletePodEvent

			location := eventData.Location
			address := eventData.Address
			service := eventData.Service

			klog.Infof("Processing event: %s, pod %s/%s for service %s", event.EventType, location, address, service)

			removeNATGatewayRequestDTO := difftracker.MapNATGatewayUpdatesToServicesDataDTO(
				difftracker.SyncServicesReturnType{
					Additions: nil,
					Removals:  utilsets.NewString(service),
				},
				updater.az.SubscriptionID,
				updater.az.ResourceGroup,
			)
			klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: removeNATGatewayRequestDTO:\n")
			logObject(removeNATGatewayRequestDTO)
			err := NRPAPIClientUpdateNRPServices(ctx, removeNATGatewayRequestDTO, updater.az.SubscriptionID, updater.az.ResourceGroup)
			klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: removeNATGatewayResponseDTO:\n")
			if err == nil {
				klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: removeNATGatewayResponseDTO is nil")
				klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: delete NAT Gateway %s in resource group %s", service, updater.az.ResourceGroup)
				updater.az.deleteNatGateway(ctx, updater.az.ResourceGroup, service)
				klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: delete Public IP %s in resource group %s", service, updater.az.ResourceGroup)
				updater.az.DeletePublicIPOutbound(updater.az.ResourceGroup, fmt.Sprintf("%s-pip", service))
				updater.az.diffTracker.UpdateK8sEgress(difftracker.UpdateK8sResource{
					Operation: difftracker.REMOVE,
					ID:        service,
				})
				updater.az.diffTracker.UpdateK8sPod(difftracker.UpdatePodInputType{
					PodOperation:           difftracker.REMOVE,
					PublicOutboundIdentity: service,
					Location:               location,
					Address:                address,
				})
				updater.az.TriggerLocationAndNRPServiceBatchUpdate()
			}
			klog.Infof("CLB-ENECHITOAIA-podEgressResourceUpdater.process: successfully deleted pod %s/%s for service %s", location, address, service)

		default:
			klog.Warningf("Unknown event type: %s", event.EventType)
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
