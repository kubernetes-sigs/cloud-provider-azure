package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/difftracker"
)

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

		// Create metrics context for each queue item processing
		mc := metrics.NewMetricContext("services", "podEgressResourceUpdater.process", updater.az.ResourceGroup, updater.az.getNetworkResourceSubscriptionID(), "pod-egress")
		isOperationSucceeded := false

		switch event.EventType {
		case "Add":
			eventData := event.AddPodEvent
			// klog.Infof("Processing event: %s for pod: %s", event.EventType, eventData.Key)

			namespace, name, err := cache.SplitMetaNamespaceKey(eventData.Key)
			if err != nil {
				klog.Errorf("podEgressResourceUpdater.process: Failed to split key %s: %v", eventData.Key, err)
				mc.ObserveOperationWithResult(isOperationSucceeded)
				updater.az.diffTracker.PodEgressQueue.Done(item)
				continue
			}

			pod, err := updater.az.podLister.Pods(namespace).Get(name)
			if err != nil {
				klog.Errorf("podEgressResourceUpdater.process: Pod %s/%s not found, skipping event processing: %v", namespace, name, err)
				mc.ObserveOperationWithResult(isOperationSucceeded)
				updater.az.diffTracker.PodEgressQueue.Done(item)
				continue
			}

			location := pod.Status.HostIP
			address := pod.Status.PodIP
			service := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
			// TBD(enechitoaia): Handle any future pod annotations here

			klog.Infof("podEgressResourceUpdater.process: Pod %s/%s - Location: %s, Address: %s, Service: %s",
				namespace, name, location, address, service)

			_, ok := updater.az.diffTracker.LocalServiceNameToNRPServiceMap.Load(service)
			if ok {
				klog.Infof("podEgressResourceUpdater.process: Pod %s/%s service %s already exists in localServiceNameToNRPServiceMap",
					namespace, name, service)
				updater.az.diffTracker.UpdateK8sPod(difftracker.UpdatePodInputType{
					PodOperation:           difftracker.ADD,
					PublicOutboundIdentity: service,
					Location:               location,
					Address:                address,
				})
				updater.az.TriggerLocationAndNRPServiceBatchUpdate()
			} else {
				klog.Infof("podEgressResourceUpdater.process: Pod %s/%s service %s does not exist in localServiceNameToNRPServiceMap. Creating resources.",
					namespace, name, service)
				klog.Infof("podEgressResourceUpdater.process: Creating Public IP %s in resource group %s", service, updater.az.ResourceGroup)
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
				updater.az.CreateOrUpdatePIPOutbound(ctx, updater.az.ResourceGroup, &pipResource)
				klog.Infof("podEgressResourceUpdater.process: Creating NAT Gateway %s in resource group %s", service, updater.az.ResourceGroup)
				natGatewayResource := armnetwork.NatGateway{
					Name: to.Ptr(service),
					ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/natGateways/%s",
						updater.az.SubscriptionID, updater.az.ResourceGroup, service)),
					SKU: &armnetwork.NatGatewaySKU{
						Name: to.Ptr(armnetwork.NatGatewaySKUNameStandardV2)},
					Location: to.Ptr(updater.az.Location),
					Properties: &armnetwork.NatGatewayPropertiesFormat{ // TODO (enechitoaia): What properties should we use for the Nat Gateway
						ServiceGateway: &armnetwork.ServiceGateway{
							ID: to.Ptr(updater.az.GetServiceGatewayID()),
						},
						PublicIPAddresses: []*armnetwork.SubResource{
							{
								ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s-pip",
									updater.az.SubscriptionID, updater.az.ResourceGroup, service)),
							},
						},
					},
				}
				updater.az.createOrUpdateNatGateway(ctx, updater.az.ResourceGroup, natGatewayResource)

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
				klog.Infof("podEgressResourceUpdater.process: successfully added pod %s/%s for service %s", namespace, name, service)
			}
			isOperationSucceeded = true
		case "Delete":
			eventData := event.DeletePodEvent

			location := eventData.Location
			address := eventData.Address
			service := eventData.Service

			klog.Infof("podEgressResourceUpdater.process: Processing event: %s, pod %s/%s for service %s", event.EventType, location, address, service)
			err := updater.az.DisassociateNatGatewayFromServiceGateway(ctx, updater.az.ServiceGatewayResourceName, service)
			klog.Infof("podEgressResourceUpdater.process: removeNATGatewayResponseDTO:\n")
			if err == nil {
				klog.Infof("podEgressResourceUpdater.process: removeNATGatewayResponseDTO is nil")
				klog.Infof("podEgressResourceUpdater.process: delete NAT Gateway %s in resource group %s", service, updater.az.ResourceGroup)
				updater.az.deleteNatGateway(ctx, updater.az.ResourceGroup, service)
				klog.Infof("podEgressResourceUpdater.process: delete Public IP %s in resource group %s", service, updater.az.ResourceGroup)
				updater.az.DeletePublicIPOutbound(ctx, updater.az.ResourceGroup, fmt.Sprintf("%s-pip", service))
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
				klog.Infof("podEgressResourceUpdater.process: successfully deleted pod %s/%s for service %s", location, address, service)
				isOperationSucceeded = true
			} else {
				klog.Errorf("podEgressResourceUpdater.process: failed to disassociate NAT Gateway %s from Service Gateway: %v", service, err)
			}

		default:
			klog.Warningf("Unknown event type: %s", event.EventType)
		}

		mc.ObserveOperationWithResult(isOperationSucceeded)
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
