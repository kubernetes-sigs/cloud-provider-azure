package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/difftracker"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func dumpStringIntSyncMap(m *sync.Map) map[string]int {
	out := make(map[string]int)
	m.Range(func(k, v any) bool {
		ks, ok := k.(string)
		if !ok {
			return true
		}
		switch vv := v.(type) {
		case int:
			out[ks] = vv
		case int32:
			out[ks] = int(vv)
		case int64:
			out[ks] = int(vv)
		case *int:
			if vv != nil {
				out[ks] = *vv
			}
		default:
			// fallback: try fmt
			out[ks] = 0
		}
		return true
	})
	return out
}

func logSyncStringIntMap(prefix string, m *sync.Map) {
	tmp := dumpStringIntSyncMap(m)
	keys := make([]string, 0, len(tmp))
	for k := range tmp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	klog.Infof("%s size=%d", prefix, len(keys))
	for _, k := range keys {
		klog.Infof("%s entry: %s -> %d", prefix, k, tmp[k])
	}
}

func (az *Cloud) initializeDiffTracker() error {
	mc := metrics.NewMetricContext("services", "initializeDiffTracker", az.ResourceGroup, az.getNetworkResourceSubscriptionID(), az.ServiceGatewayResourceName)
	isOperationSucceeded := false
	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded)
	}()

	klog.Infof("initializeDiffTracker: starting initialization of diff tracker")
	ctx := context.Background()

	// Defensive guard
	if az.KubeClient == nil {
		return fmt.Errorf("initializeDiffTracker: KubeClient is nil; initialize the cloud provider with a Kubernetes client before diff tracker setup")
	}

	localServiceNameToNRPServiceMap := make(map[string]int, 0) // used to initialize sync map in diff tracker

	// Initialize K8S Resource state
	k8s := difftracker.K8s_State{
		Services: utilsets.NewString(),
		Egresses: utilsets.NewString(),
		Nodes:    make(map[string]difftracker.Node),
	}
	// List nodes once (cache not initialized yet)
	nodeList, err := az.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("initializeDiffTracker: failed to list nodes: %w", err)
	}
	klog.Infof("initializeDiffTracker: found %d nodes", len(nodeList.Items))

	// allNodes will be passed to ensureServiceLoadBalancerByUID
	allNodes := make([]*v1.Node, 0, len(nodeList.Items))
	nodeNameToIPMap := make(map[string]string, len(nodeList.Items))

	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		allNodes = append(allNodes, n)

		// Grab first InternalIP
		for _, addr := range n.Status.Addresses {
			if addr.Type == v1.NodeInternalIP {
				nodeNameToIPMap[n.Name] = addr.Address
				break
			}
		}
	}
	klog.Infof("initializeDiffTracker: nodeNameToIPMap: ")
	//logObject(nodeNameToIPMap)

	// 1. Fetch all services and update difftracker.K8sResources.Services
	services, err := az.KubeClient.CoreV1().Services(v1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("initializeDiffTracker: failed to list services: %w", err)
	}

	for _, service := range services.Items {
		if service.Spec.Type == v1.ServiceTypeLoadBalancer {
			k8s.Services.Insert(strings.ToLower(string(service.UID)))
			localServiceNameToNRPServiceMap[strings.ToLower(string(service.UID))] = -34
		}
	}
	// END 1.

	// 2. Fetch all endpointSlices and set up difftracker.K8sResources.Nodes and difftracker.K8sResources.Pods with the inbound identities
	endpointSliceList, err := az.KubeClient.DiscoveryV1().EndpointSlices(v1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("initializeDiffTracker: failed to list endpointslices: %w", err)
	}
	for _, endpointSlice := range endpointSliceList.Items {
		serviceUID := ""
		for _, ownerRef := range endpointSlice.OwnerReferences {
			if ownerRef.Kind == "Service" {
				serviceUID = string(ownerRef.UID)
				break
			}
		}
		if serviceUID == "" || !k8s.Services.Has(serviceUID) {
			continue
		}

		for _, endpoint := range endpointSlice.Endpoints {
			if endpoint.NodeName == nil || len(endpoint.Addresses) == 0 {
				continue
			}

			nodeIP, exists := nodeNameToIPMap[*endpoint.NodeName]
			if !exists {
				klog.Warningf("initializeDiffTracker: Could not find IP for node %s", *endpoint.NodeName)
				continue
			}

			if _, exists := k8s.Nodes[nodeIP]; !exists {
				k8s.Nodes[nodeIP] = difftracker.Node{
					Pods: make(map[string]difftracker.Pod),
				}
			}

			for _, podIP := range endpoint.Addresses {
				pod, exists := k8s.Nodes[nodeIP].Pods[podIP]
				if !exists {
					pod = difftracker.Pod{
						InboundIdentities:      utilsets.NewString(),
						PublicOutboundIdentity: "",
					}
				}
				pod.InboundIdentities.Insert(serviceUID)
				k8s.Nodes[nodeIP].Pods[podIP] = pod
			}
		}
	}
	// END 2.

	// 3. Fetch all egress resources and update difftracker.K8sResources.Egresses along with pods that have outbound identities
	egressPods, err := az.KubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: consts.PodLabelServiceEgressGateway,
	})
	if err != nil {
		return fmt.Errorf("initializeDiffTracker: failed to list pods with egress label: %w", err)
	}

	for _, pod := range egressPods.Items {
		// Get the egress label value
		egressVal := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
		if egressVal == "" || pod.Status.PodIP == "" || pod.Spec.NodeName == "" {
			continue
		}
		// Add to egresses set
		k8s.Egresses.Insert(egressVal)

		// Get nodeIP from nodeName
		nodeIP, exists := nodeNameToIPMap[pod.Spec.NodeName]
		if !exists {
			klog.Warningf("initializeDiffTracker: Could not find IP for node %s", pod.Spec.NodeName)
			continue
		}

		// Initialize node if it doesn't exist
		if _, exists := k8s.Nodes[nodeIP]; !exists {
			k8s.Nodes[nodeIP] = difftracker.Node{
				Pods: make(map[string]difftracker.Pod),
			}
		}

		// Get or create the pod entry
		podEntry, exists := k8s.Nodes[nodeIP].Pods[pod.Status.PodIP]
		if !exists {
			podEntry = difftracker.Pod{
				InboundIdentities:      utilsets.NewString(),
				PublicOutboundIdentity: "",
			}
		}

		// Set the public outbound identity
		podEntry.PublicOutboundIdentity = egressVal
		localServiceNameToNRPServiceMap[strings.ToLower(egressVal)] = localServiceNameToNRPServiceMap[strings.ToLower(egressVal)] + 1

		// Update the pod in the node's pods map
		k8s.Nodes[nodeIP].Pods[pod.Status.PodIP] = podEntry
	}
	klog.Infof("initializeDiffTracker: k8s object: ")
	// logObject(k8s)
	// END 3.

	// 4. Fetch all ServiceGateway.Services and update difftracker.NRPResources.LoadBalancers and difftracker.NRPResources.NATGateways
	nrp := difftracker.NRP_State{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(),
		Locations:     make(map[string]difftracker.NRPLocation),
	}

	servicesDTO, err := az.GetServices(ctx, az.ServiceGatewayResourceName)
	if err != nil {
		return fmt.Errorf("initializeDiffTracker: failed to get services from ServiceGateway API: %w", err)
	}
	for _, service := range servicesDTO {
		switch *service.Properties.ServiceType {
		case "Inbound":
			nrp.LoadBalancers.Insert(*service.Name)
			// localServiceNameToNRPServiceMap[strings.ToLower(*service.Name)] = 0
		case "Outbound":
			if service.Name != nil && *service.Name == "default-natgw-v2" {
				continue
			}
			nrp.NATGateways.Insert(*service.Name)
		}
		// if service.Name != nil && *service.Name != "" {
		// }
	}
	// klog.Infof("initializeDiffTracker: fetched %d services from ServiceGateway API", len(servicesDTO))
	// logObject(servicesDTO)
	// END 4.

	// 5. Fetch all locations from ServiceGateway API and update difftracker.NRPResources.Locations

	locationsDTO, err := az.GetAddressLocations(ctx, az.ServiceGatewayResourceName)
	klog.Infof("initializeDiffTracker: fetched %d locations from ServiceGateway API", len(locationsDTO))
	//logObject(locationsDTO)
	if err != nil {
		return fmt.Errorf("initializeDiffTracker: failed to get locations from ServiceGateway API: %w", err)
	}
	for _, location := range locationsDTO {
		// Defensive guards for potentially incomplete data coming from the ServiceGateway API
		if location == nil {
			klog.Warningf("initializeDiffTracker: encountered nil location entry, skipping")
			continue
		}
		if location.AddressLocation == nil || *location.AddressLocation == "" {
			klog.Warningf("initializeDiffTracker: location with nil/empty AddressLocation encountered, skipping: %+v", location)
			continue
		}
		addresses := make(map[string]difftracker.NRPAddress, 0)
		for _, addr := range location.Addresses {
			if addr == nil || addr.Address == nil || *addr.Address == "" { // skip invalid address entries
				klog.V(4).Infof("initializeDiffTracker: skipping nil/empty address in location %s", *location.AddressLocation)
				continue
			}
			services := utilsets.NewString()
			for _, svc := range addr.Services {
				if svc != nil {
					services.Insert(*svc)
					// localServiceNameToNRPServiceMap[strings.ToLower(*svc)] = localServiceNameToNRPServiceMap[strings.ToLower(*svc)] + 1
				}
			}
			addresses[*addr.Address] = difftracker.NRPAddress{
				Services: services,
			}
		}
		// Only set the location if we actually collected any addresses (could also allow empty) ; we allow empty to reflect source of truth.
		nrp.Locations[*location.AddressLocation] = difftracker.NRPLocation{
			Addresses: addresses,
		}
	}

	// klog.Infof("initializeDiffTracker: fetched %d locations from ServiceGateway API", len(nrp.Locations))
	// logObject(nrp.Locations)
	// END 5.

	// Initialize the diff tracker state and get the necessary operations to sync the cluster with NRP
	config := difftracker.Config{
		SubscriptionID:             az.getNetworkResourceSubscriptionID(),
		ResourceGroup:              az.ResourceGroup,
		Location:                   az.Location,
		ServiceGatewayResourceName: az.ServiceGatewayResourceName,
		ServiceGatewayID:           az.GetServiceGatewayID(),
	}
	az.diffTracker = difftracker.InitializeDiffTracker(k8s, nrp, config, az.NetworkClientFactory, az.KubeClient)
	az.diffTracker.LocalServiceNameToNRPServiceMap = *syncMapFromMap(localServiceNameToNRPServiceMap)
	klog.Infof("initializeDiffTracker: initialized diff tracker localServiceNameToNRPServiceMap with %d entries", localServiceNameToNRPServiceMap)
	//logObject(localServiceNameToNRPServiceMap)
	logSyncStringIntMap("initializeDiffTracker: LocalServiceNameToNRPServiceMap", &az.diffTracker.LocalServiceNameToNRPServiceMap)
	klog.Infof("initializeDiffTracker: initialized diff tracker")
	//logObject(az.diffTracker)
	// 6. Fetch LoadBalancers from NRP
	lbclient := az.NetworkClientFactory.GetLoadBalancerClient()
	lbs, err := lbclient.List(ctx, az.ResourceGroup)
	currentLoadBalancersInNRP := utilsets.NewString()
	if err != nil {
		return fmt.Errorf("initializeDiffTracker: failed to list load balancers: %w", err)
	}
	for _, lb := range lbs {
		if lb.Name == nil {
			continue
		}
		currentLoadBalancersInNRP.Insert(strings.ToLower(*lb.Name))
	}

	// 7. Fetch NATGateways from NRP
	ngclient := az.NetworkClientFactory.GetNatGatewayClient()
	ngs, err := ngclient.List(ctx, az.ResourceGroup)
	currentNATGatewaysInNRP := utilsets.NewString()
	if err != nil {
		return fmt.Errorf("initializeDiffTracker: failed to list nat gateways: %w", err)
	}
	for _, ng := range ngs {
		if ng.Name == nil {
			continue
		}
		currentNATGatewaysInNRP.Insert(strings.ToLower(*ng.Name))
	}
	// END 6. and 7.

	// 8. Get sync operations to sync the cluster with NRP
	syncOperations := az.diffTracker.GetSyncOperations()
	// END 8.

	// 9. Create LoadBalancers and NATGateways in NRP
	for _, uid := range syncOperations.LoadBalancerUpdates.Additions.UnsortedList() {
		if !currentLoadBalancersInNRP.Has(uid) {
			// Fallback cluster name (tags still expect a non-empty value in some helper paths):
			clusterName := az.LoadBalancerName
			if clusterName == "" {
				clusterName = "kubernetes"
			}

			err = az.ensureServiceLoadBalancerByUID(ctx, uid, clusterName, allNodes)
			if err != nil {
				klog.Errorf("initializeDiffTracker: failed ensuring LB for service uid %s: %v", uid, err)
				continue
			}
			klog.V(3).Infof("initializeDiffTracker: ensured LoadBalancer for service uid %s", uid)
		}
	}

	for _, natGatewayId := range syncOperations.NATGatewayUpdates.Additions.UnsortedList() {
		if !currentNATGatewaysInNRP.Has(natGatewayId) {
			klog.Infof("initializeDiffTracker: Creating NAT Gateway %s in NRP", natGatewayId)

			pipResourceName := fmt.Sprintf("%s-pip", natGatewayId)
			pipResource := armnetwork.PublicIPAddress{
				Name: to.Ptr(pipResourceName),
				ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
					az.SubscriptionID, az.ResourceGroup, pipResourceName)),
				SKU: &armnetwork.PublicIPAddressSKU{
					Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandardV2),
				},
				Location: to.Ptr(az.Location),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{ // TODO (enechitoaia): What properties should we use for the Public IP

					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
				},
			}
			az.CreateOrUpdatePIPOutbound(ctx, az.ResourceGroup, &pipResource)

			natGatewayResource := armnetwork.NatGateway{
				Name: to.Ptr(natGatewayId),
				ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/natGateways/%s",
					az.SubscriptionID, az.ResourceGroup, natGatewayId)),
				SKU: &armnetwork.NatGatewaySKU{
					Name: to.Ptr(armnetwork.NatGatewaySKUNameStandardV2)},
				Location: to.Ptr(az.Location),
				Properties: &armnetwork.NatGatewayPropertiesFormat{
					ServiceGateway: &armnetwork.ServiceGateway{
						ID: to.Ptr(az.GetServiceGatewayID()),
					},
					PublicIPAddresses: []*armnetwork.SubResource{
						{
							ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
								az.SubscriptionID, az.ResourceGroup, pipResourceName)),
						},
					},
				},
			}
			az.createOrUpdateNatGateway(ctx, az.ResourceGroup, natGatewayResource)
		}
	}
	// END 9.

	// 10. Add Services in ServiceGateway API for created LBs and NATGateways
	addServicesDTO := difftracker.MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
		difftracker.SyncServicesReturnType{
			Additions: syncOperations.LoadBalancerUpdates.Additions,
			Removals:  nil,
		},
		difftracker.SyncServicesReturnType{
			Additions: syncOperations.NATGatewayUpdates.Additions,
			Removals:  nil,
		}, az.SubscriptionID, az.ResourceGroup)
	if len(addServicesDTO.Services) > 0 {
		if err := az.UpdateNRPSGWServices(ctx, az.ServiceGatewayResourceName, addServicesDTO); err != nil {
			return fmt.Errorf("initializeDiffTracker: failed to add services in ServiceGateway API: %w", err)
		}
		// Reflect additions in local NRP state
		if syncOperations.LoadBalancerUpdates.Additions.Len() > 0 {
			az.diffTracker.UpdateNRPLoadBalancers(difftracker.SyncServicesReturnType{
				Additions: syncOperations.LoadBalancerUpdates.Additions,
				Removals:  utilsets.NewString(),
			})
		}
		if syncOperations.NATGatewayUpdates.Additions.Len() > 0 {
			az.diffTracker.UpdateNRPNATGateways(difftracker.SyncServicesReturnType{
				Additions: syncOperations.NATGatewayUpdates.Additions,
				Removals:  utilsets.NewString(),
			})
		}
	}
	// END 10.

	// 11. Update Addresses and Locations
	if len(syncOperations.LocationData.Locations) > 0 {
		locationsDTO := difftracker.MapLocationDataToDTO(syncOperations.LocationData)
		if err := az.UpdateNRPSGWAddressLocations(ctx, az.ServiceGatewayResourceName, locationsDTO); err != nil {
			return fmt.Errorf("initializeDiffTracker: failed to update locations in ServiceGateway API: %w", err)
		}
		az.diffTracker.UpdateLocationsAddresses(syncOperations.LocationData)
	}
	// END 11.

	// 12. Delete Services in ServiceGateway API for deleted LBs and NATGateways
	removeServicesDTO := difftracker.MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
		difftracker.SyncServicesReturnType{
			Additions: nil,
			Removals:  syncOperations.LoadBalancerUpdates.Removals,
		},
		difftracker.SyncServicesReturnType{
			Additions: nil,
			Removals:  syncOperations.NATGatewayUpdates.Removals,
		},
		az.SubscriptionID,
		az.ResourceGroup,
	)
	if len(removeServicesDTO.Services) > 0 {
		if err := az.UpdateNRPSGWServices(ctx, az.ServiceGatewayResourceName, removeServicesDTO); err != nil {
			return fmt.Errorf("initializeDiffTracker: failed to remove services in ServiceGateway API: %w", err)
		}
		// Reflect removals
		if syncOperations.LoadBalancerUpdates.Removals.Len() > 0 {
			az.diffTracker.UpdateNRPLoadBalancers(difftracker.SyncServicesReturnType{
				Additions: utilsets.NewString(),
				Removals:  syncOperations.LoadBalancerUpdates.Removals,
			})
		}
		if syncOperations.NATGatewayUpdates.Removals.Len() > 0 {
			az.diffTracker.UpdateNRPNATGateways(difftracker.SyncServicesReturnType{
				Additions: utilsets.NewString(),
				Removals:  syncOperations.NATGatewayUpdates.Removals,
			})
		}
	}
	// END 12.

	// 13. Delete LoadBalancers and NATGateways in NRP
	for _, lb := range syncOperations.LoadBalancerUpdates.Removals.UnsortedList() {
		// az.diffTracker.LocalServiceNameToNRPServiceMap.Delete(lb)
		if currentLoadBalancersInNRP.Has(lb) {
			klog.Infof("initializeDiffTracker: Deleting Load Balancer %s from NRP", lb)
			// Fallback cluster name (only needed for tag/SNAT helper paths reused in EnsureLoadBalancerDeleted)
			clusterName := az.LoadBalancerName
			if clusterName == "" {
				clusterName = "kubernetes"
			}
			klog.Infof("initializeDiffTracker: Deleting Load Balancer %s from NRP", lb)
			if err := az.ensureServiceLoadBalancerDeletedByUID(ctx, lb, clusterName); err != nil {
				klog.Errorf("initializeDiffTracker: failed deleting LB %s: %v", lb, err)
				continue
			}
			// Update local view to avoid redundant attempts in the same run.
			currentLoadBalancersInNRP.Delete(lb)
			klog.Infof("initializeDiffTracker: Successfully deleted Load Balancer %s from NRP", lb)
		}
	}

	for _, natGatewayId := range syncOperations.NATGatewayUpdates.Removals.UnsortedList() {
		// az.diffTracker.LocalServiceNameToNRPServiceMap.Delete(natGatewayId)
		if currentNATGatewaysInNRP.Has(natGatewayId) {
			klog.Infof("initializeDiffTracker: Deleting NAT Gateway %s from NRP", natGatewayId)
			// az.DetachNatGatewayFromServiceGateway(ctx, az.ResourceGroup, natGatewayId)
			az.deleteNatGateway(ctx, az.ResourceGroup, natGatewayId)
			az.DeletePublicIPOutbound(ctx, az.ResourceGroup, fmt.Sprintf("%s-pip", natGatewayId))
		}
	}
	// END 9. and 10.

	// 	Address (delete) orphaned PIP resources
	pipclient := az.NetworkClientFactory.GetPublicIPAddressClient()
	pips, err := pipclient.List(ctx, az.ResourceGroup)
	if err != nil {
		return fmt.Errorf("initializeDiffTracker: failed to list public IP addresses: %w", err)
	}
	for _, pip := range pips {
		if pip.Name == nil || !strings.HasSuffix(*pip.Name, "-pip") || *pip.Name == "default-natgw-v2-pip" {
			continue
		}

		associatedResourceName := strings.TrimSuffix(*pip.Name, "-pip")
		lbExists := az.diffTracker.NRPResources.LoadBalancers.Has(associatedResourceName)
		ngExists := az.diffTracker.NRPResources.NATGateways.Has(associatedResourceName)
		if !lbExists && !ngExists {
			klog.Infof("initializeDiffTracker: Deleting orphaned Public IP %s from NRP", *pip.Name)
			az.DeletePublicIPOutbound(ctx, az.ResourceGroup, *pip.Name)
		}
	}
	// Mark initial sync as done

	az.diffTracker.InitialSyncDone = true
	isOperationSucceeded = true
	// klog.Infof("initializeDiffTracker: az.diffTracker.InitialSyncDone: %v", az.diffTracker.InitialSyncDone)
	// klog.Infof("initializeDiffTracker: completed ordered ServiceGateway sync and initialization")
	return nil
}

func syncMapFromMap(localServiceNameToNRPServiceMap map[string]int) *sync.Map {
	var syncMap sync.Map
	for k, v := range localServiceNameToNRPServiceMap {
		syncMap.Store(k, v)
	}
	return &syncMap
}
