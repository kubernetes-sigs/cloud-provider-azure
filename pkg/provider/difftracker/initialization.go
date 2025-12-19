package difftracker

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// InitializeFromCluster initializes a DiffTracker by fetching K8s and NRP state, computing the diff,
// and synchronizing resources. This replaces the provider-level initializeDiffTracker function.
func InitializeFromCluster(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
	kubeClient kubernetes.Interface,
) (*DiffTracker, error) {
	mc := metrics.NewMetricContext("services", "InitializeFromCluster", config.ResourceGroup, config.SubscriptionID, config.ServiceGatewayResourceName)
	isOperationSucceeded := false
	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded)
	}()

	klog.Infof("InitializeFromCluster: starting initialization of diff tracker")

	// Defensive guard
	if kubeClient == nil {
		return nil, fmt.Errorf("InitializeFromCluster: KubeClient is nil; initialize the cloud provider with a Kubernetes client before diff tracker setup")
	}
	if networkClientFactory == nil {
		return nil, fmt.Errorf("InitializeFromCluster: NetworkClientFactory is nil; cannot initialize diff tracker without Azure network clients")
	}

	localServiceNameToNRPServiceMap := make(map[string]int, 0) // used to initialize sync map in diff tracker

	// Initialize K8S Resource state
	k8s := K8s_State{
		Services: utilsets.NewString(),
		Egresses: utilsets.NewString(),
		Nodes:    make(map[string]Node),
	}

	// List nodes once (cache not initialized yet)
	nodeList, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("InitializeFromCluster: failed to list nodes: %w", err)
	}
	klog.Infof("InitializeFromCluster: found %d nodes", len(nodeList.Items))

	// Build nodeNameToIPMap for pod-to-node mapping
	nodeNameToIPMap := make(map[string]string, len(nodeList.Items))

	for i := range nodeList.Items {
		n := &nodeList.Items[i]

		// Grab first InternalIP
		for _, addr := range n.Status.Addresses {
			if addr.Type == v1.NodeInternalIP {
				nodeNameToIPMap[n.Name] = addr.Address
				break
			}
		}
	}
	klog.V(4).Infof("InitializeFromCluster: built nodeNameToIPMap with %d entries", len(nodeNameToIPMap))

	// 1. Fetch all services and update K8sResources.Services
	services, err := kubeClient.CoreV1().Services(v1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("InitializeFromCluster: failed to list services: %w", err)
	}

	// Store service UIDs to Service objects for later port extraction
	serviceUIDToService := make(map[string]*v1.Service)
	for i, service := range services.Items {
		if service.Spec.Type == v1.ServiceTypeLoadBalancer {
			uid := strings.ToLower(string(service.UID))
			k8s.Services.Insert(uid)
			// Initialize reference counter with sentinel value -34 (matches original implementation)
			// This value is used to track service references and will be updated during endpoint processing
			localServiceNameToNRPServiceMap[uid] = -34
			serviceUIDToService[uid] = &services.Items[i]
		}
	}
	klog.Infof("InitializeFromCluster: found %d LoadBalancer services", k8s.Services.Len())

	// 2. Fetch all endpointSlices and set up K8sResources.Nodes and K8sResources.Pods with the inbound identities
	endpointSliceList, err := kubeClient.DiscoveryV1().EndpointSlices(v1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("InitializeFromCluster: failed to list endpointslices: %w", err)
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
				klog.Warningf("InitializeFromCluster: Could not find IP for node %s", *endpoint.NodeName)
				continue
			}

			if _, exists := k8s.Nodes[nodeIP]; !exists {
				k8s.Nodes[nodeIP] = Node{
					Pods: make(map[string]Pod),
				}
			}

			for _, podIP := range endpoint.Addresses {
				pod, exists := k8s.Nodes[nodeIP].Pods[podIP]
				if !exists {
					pod = Pod{
						InboundIdentities:      utilsets.NewString(),
						PublicOutboundIdentity: "",
					}
				}
				pod.InboundIdentities.Insert(serviceUID)
				k8s.Nodes[nodeIP].Pods[podIP] = pod
			}
		}
	}
	klog.Infof("InitializeFromCluster: processed %d endpointslices", len(endpointSliceList.Items))

	// 3. Fetch all egress resources and update K8sResources.Egresses along with pods that have outbound identities
	egressPods, err := kubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: consts.PodLabelServiceEgressGateway,
	})
	if err != nil {
		return nil, fmt.Errorf("InitializeFromCluster: failed to list pods with egress label: %w", err)
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
			klog.Warningf("InitializeFromCluster: Could not find IP for node %s", pod.Spec.NodeName)
			continue
		}

		// Initialize node if it doesn't exist
		if _, exists := k8s.Nodes[nodeIP]; !exists {
			k8s.Nodes[nodeIP] = Node{
				Pods: make(map[string]Pod),
			}
		}

		// Get or create the pod entry
		podEntry, exists := k8s.Nodes[nodeIP].Pods[pod.Status.PodIP]
		if !exists {
			podEntry = Pod{
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
	klog.Infof("InitializeFromCluster: found %d egress services", k8s.Egresses.Len())

	// 4. Fetch all ServiceGateway.Services and update NRPResources.LoadBalancers and NRPResources.NATGateways
	nrp := NRP_State{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(),
		Locations:     make(map[string]NRPLocation),
	}

	// Fetch services from ServiceGateway API
	sgwClient := networkClientFactory.GetServiceGatewayClient()
	servicesDTO, err := sgwClient.GetServices(ctx, config.ResourceGroup, config.ServiceGatewayResourceName)
	if err != nil {
		return nil, fmt.Errorf("InitializeFromCluster: failed to get services from ServiceGateway API: %w", err)
	}
	for _, service := range servicesDTO {
		switch *service.Properties.ServiceType {
		case "Inbound":
			nrp.LoadBalancers.Insert(*service.Name)
		case "Outbound":
			if service.Name != nil && *service.Name == "default-natgw-v2" {
				continue
			}
			nrp.NATGateways.Insert(*service.Name)
		}
	}
	klog.Infof("InitializeFromCluster: fetched %d services from ServiceGateway API (%d LBs, %d NATs)",
		len(servicesDTO), nrp.LoadBalancers.Len(), nrp.NATGateways.Len())

	// 5. Fetch all locations from ServiceGateway API and update NRPResources.Locations
	locationsDTO, err := sgwClient.GetAddressLocations(ctx, config.ResourceGroup, config.ServiceGatewayResourceName)
	if err != nil {
		return nil, fmt.Errorf("InitializeFromCluster: failed to get locations from ServiceGateway API: %w", err)
	}
	klog.Infof("InitializeFromCluster: fetched %d locations from ServiceGateway API", len(locationsDTO))

	for _, location := range locationsDTO {
		// Defensive guards for potentially incomplete data coming from the ServiceGateway API
		if location == nil {
			klog.Warningf("InitializeFromCluster: encountered nil location entry, skipping")
			continue
		}
		if location.AddressLocation == nil || *location.AddressLocation == "" {
			klog.Warningf("InitializeFromCluster: location with nil/empty AddressLocation encountered, skipping: %+v", location)
			continue
		}
		addresses := make(map[string]NRPAddress, 0)
		for _, addr := range location.Addresses {
			if addr == nil || addr.Address == nil || *addr.Address == "" { // skip invalid address entries
				klog.V(4).Infof("InitializeFromCluster: skipping nil/empty address in location %s", *location.AddressLocation)
				continue
			}
			services := utilsets.NewString()
			for _, svc := range addr.Services {
				if svc != nil {
					services.Insert(*svc)
				}
			}
			addresses[*addr.Address] = NRPAddress{
				Services: services,
			}
		}
		// Only set the location if we actually collected any addresses (could also allow empty) ; we allow empty to reflect source of truth.
		nrp.Locations[*location.AddressLocation] = NRPLocation{
			Addresses: addresses,
		}
	}
	klog.Infof("InitializeFromCluster: processed %d locations with addresses", len(nrp.Locations))

	// Initialize the diff tracker state
	diffTracker := InitializeDiffTracker(k8s, nrp, config, networkClientFactory, kubeClient)
	diffTracker.LocalServiceNameToNRPServiceMap = *syncMapFromMap(localServiceNameToNRPServiceMap)
	LogSyncStringIntMap("InitializeFromCluster: LocalServiceNameToNRPServiceMap", &diffTracker.LocalServiceNameToNRPServiceMap)

	// 6. Fetch LoadBalancers and NAT Gateways from NRP
	lbclient := networkClientFactory.GetLoadBalancerClient()
	lbs, err := lbclient.List(ctx, config.ResourceGroup)
	currentLoadBalancersInNRP := utilsets.NewString()
	if err != nil {
		return nil, fmt.Errorf("InitializeFromCluster: failed to list load balancers: %w", err)
	}
	for _, lb := range lbs {
		if lb.Name == nil {
			continue
		}
		currentLoadBalancersInNRP.Insert(strings.ToLower(*lb.Name))
	}
	klog.Infof("InitializeFromCluster: found %d LoadBalancers in NRP", currentLoadBalancersInNRP.Len())

	ngclient := networkClientFactory.GetNatGatewayClient()
	ngs, err := ngclient.List(ctx, config.ResourceGroup)
	currentNATGatewaysInNRP := utilsets.NewString()
	if err != nil {
		return nil, fmt.Errorf("InitializeFromCluster: failed to list nat gateways: %w", err)
	}
	for _, ng := range ngs {
		if ng.Name == nil {
			continue
		}
		currentNATGatewaysInNRP.Insert(strings.ToLower(*ng.Name))
	}
	klog.Infof("InitializeFromCluster: found %d NAT Gateways in NRP", currentNATGatewaysInNRP.Len())

	// 8. Get sync operations to sync the cluster with NRP
	syncOperations := diffTracker.GetSyncOperations()
	klog.Infof("InitializeFromCluster: sync operations - LB additions: %d, LB removals: %d, NAT additions: %d, NAT removals: %d, locations: %d",
		syncOperations.LoadBalancerUpdates.Additions.Len(),
		syncOperations.LoadBalancerUpdates.Removals.Len(),
		syncOperations.NATGatewayUpdates.Additions.Len(),
		syncOperations.NATGatewayUpdates.Removals.Len(),
		len(syncOperations.LocationData.Locations))

	// 9. Create LoadBalancers and NATGateways in NRP
	var lastErr error
	for _, uid := range syncOperations.LoadBalancerUpdates.Additions.UnsortedList() {
		if currentLoadBalancersInNRP.Has(uid) {
			klog.V(3).Infof("InitializeFromCluster: LoadBalancer %s already exists in NRP, skipping creation", uid)
			continue
		}

		// Get the service to extract port configuration
		svc, exists := serviceUIDToService[uid]
		if !exists {
			klog.Errorf("InitializeFromCluster: failed to find service for uid %s, creating LB without rules", uid)
			svc = nil
		}

		// Extract inbound config from service (same logic as EnsureLoadBalancer)
		var inboundConfig *InboundConfig
		if svc != nil {
			inboundConfig = ExtractInboundConfigFromService(svc)
		}

		// Build and create inbound service resources
		pipResource, lbResource, servicesDTO := buildInboundServiceResources(uid, inboundConfig, config)

		if err := diffTracker.createOrUpdatePIP(ctx, config.ResourceGroup, &pipResource); err != nil {
			klog.Errorf("InitializeFromCluster: failed to create Public IP for service uid %s: %v", uid, err)
			lastErr = err
			continue
		}
		klog.V(3).Infof("InitializeFromCluster: created Public IP for service uid %s", uid)

		if err := diffTracker.createOrUpdateLB(ctx, lbResource); err != nil {
			klog.Errorf("InitializeFromCluster: failed to create LoadBalancer for service uid %s: %v", uid, err)
			lastErr = err
			continue
		}
		klog.V(3).Infof("InitializeFromCluster: created LoadBalancer for service uid %s", uid)

		if err := diffTracker.updateNRPSGWServices(ctx, config.ServiceGatewayResourceName, servicesDTO); err != nil {
			klog.Errorf("InitializeFromCluster: failed to register service uid %s with ServiceGateway: %v", uid, err)
			lastErr = err
			continue
		}
		klog.V(3).Infof("InitializeFromCluster: registered service uid %s with ServiceGateway", uid)
	}

	for _, natGatewayId := range syncOperations.NATGatewayUpdates.Additions.UnsortedList() {
		if currentNATGatewaysInNRP.Has(natGatewayId) {
			klog.V(3).Infof("InitializeFromCluster: NAT Gateway %s already exists in NRP, skipping creation", natGatewayId)
			continue
		}

		klog.Infof("InitializeFromCluster: Creating NAT Gateway %s in NRP", natGatewayId)

		// Build and create outbound service resources
		pipResource, natGatewayResource, servicesDTO := buildOutboundServiceResources(natGatewayId, nil, config)

		if err := diffTracker.createOrUpdatePIP(ctx, config.ResourceGroup, &pipResource); err != nil {
			klog.Errorf("InitializeFromCluster: failed to create Public IP for NAT Gateway %s: %v", natGatewayId, err)
			lastErr = err
			continue
		}

		if err := diffTracker.createOrUpdateNatGateway(ctx, config.ResourceGroup, natGatewayResource); err != nil {
			klog.Errorf("InitializeFromCluster: failed to create NAT Gateway %s: %v", natGatewayId, err)
			lastErr = err
			continue
		}

		if err := diffTracker.updateNRPSGWServices(ctx, config.ServiceGatewayResourceName, servicesDTO); err != nil {
			klog.Errorf("InitializeFromCluster: failed to register NAT Gateway %s with ServiceGateway: %v", natGatewayId, err)
			lastErr = err
			continue
		}
		klog.V(3).Infof("InitializeFromCluster: created NAT Gateway %s", natGatewayId)
	}

	// 10. Add Services in ServiceGateway API for created LBs and NATGateways
	if syncOperations.LoadBalancerUpdates.Additions.Len() > 0 || syncOperations.NATGatewayUpdates.Additions.Len() > 0 {
		diffTracker.UpdateNRPLoadBalancers(SyncServicesReturnType{
			Additions: syncOperations.LoadBalancerUpdates.Additions,
			Removals:  utilsets.NewString(),
		})
		diffTracker.UpdateNRPNATGateways(SyncServicesReturnType{
			Additions: syncOperations.NATGatewayUpdates.Additions,
			Removals:  utilsets.NewString(),
		})
	}

	// 11. Update Addresses and Locations
	if len(syncOperations.LocationData.Locations) > 0 {
		locationsDTO := MapLocationDataToDTO(syncOperations.LocationData)
		if err := diffTracker.updateNRPSGWAddressLocations(ctx, config.ServiceGatewayResourceName, locationsDTO); err != nil {
			klog.Errorf("InitializeFromCluster: failed to update locations in ServiceGateway API: %v", err)
			lastErr = err
		} else {
			diffTracker.UpdateLocationsAddresses(syncOperations.LocationData)
			klog.V(3).Infof("InitializeFromCluster: updated %d locations in ServiceGateway API", len(syncOperations.LocationData.Locations))
		}
	}

	// 12. Delete Services in ServiceGateway API for deleted LBs and NATGateways
	if syncOperations.LoadBalancerUpdates.Removals.Len() > 0 || syncOperations.NATGatewayUpdates.Removals.Len() > 0 {
		removeServicesDTO := MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
			SyncServicesReturnType{
				Additions: nil,
				Removals:  syncOperations.LoadBalancerUpdates.Removals,
			},
			SyncServicesReturnType{
				Additions: nil,
				Removals:  syncOperations.NATGatewayUpdates.Removals,
			},
			config.SubscriptionID,
			config.ResourceGroup,
		)
		if len(removeServicesDTO.Services) > 0 {
			if err := diffTracker.updateNRPSGWServices(ctx, config.ServiceGatewayResourceName, removeServicesDTO); err != nil {
				klog.Errorf("InitializeFromCluster: failed to remove services in ServiceGateway API: %v", err)
				lastErr = err
			} else {
				// Reflect removals
				diffTracker.UpdateNRPLoadBalancers(SyncServicesReturnType{
					Additions: utilsets.NewString(),
					Removals:  syncOperations.LoadBalancerUpdates.Removals,
				})
				diffTracker.UpdateNRPNATGateways(SyncServicesReturnType{
					Additions: utilsets.NewString(),
					Removals:  syncOperations.NATGatewayUpdates.Removals,
				})
			}
		}
	}

	// 13. Delete LoadBalancers and NATGateways in NRP
	for _, lb := range syncOperations.LoadBalancerUpdates.Removals.UnsortedList() {
		klog.Infof("InitializeFromCluster: deleteInboundService started for %s", lb)
		lbExistsInAzure := currentLoadBalancersInNRP.Has(lb)

		// Step 1: Remove backend pool references from ServiceGateway (if LB exists in Azure)
		if lbExistsInAzure {
			removeBackendPoolDTO := RemoveBackendPoolReferenceFromServicesDTO(
				SyncServicesReturnType{
					Additions: nil,
					Removals:  newIgnoreCaseSetFromSlice([]string{lb}),
				},
				config.SubscriptionID,
				config.ResourceGroup,
			)
			if err := diffTracker.updateNRPSGWServices(ctx, config.ServiceGatewayResourceName, removeBackendPoolDTO); err != nil {
				klog.Warningf("InitializeFromCluster: failed to remove backend pool reference for %s: %v", lb, err)
				// Don't fail - continue with LoadBalancer deletion
			} else {
				klog.V(3).Infof("InitializeFromCluster: removed backend pool reference for %s", lb)
			}
		}

		// Step 2: Delete LoadBalancer (if exists in Azure)
		if lbExistsInAzure {
			if err := diffTracker.deleteLB(ctx, lb); err != nil {
				klog.Errorf("InitializeFromCluster: failed to delete LoadBalancer %s: %v", lb, err)
				lastErr = err
				// Continue with unregistration
			} else {
				klog.V(3).Infof("InitializeFromCluster: deleted LoadBalancer %s", lb)
			}
		}

		// Step 3: Fully unregister service from ServiceGateway (ALWAYS, even if LB doesn't exist in Azure)
		unregisterDTO := buildServiceGatewayRemovalDTO(lb, true, config)
		if err := diffTracker.updateNRPSGWServices(ctx, config.ServiceGatewayResourceName, unregisterDTO); err != nil {
			klog.Errorf("InitializeFromCluster: failed to fully unregister service %s from ServiceGateway: %v", lb, err)
			lastErr = err
			// Continue with PIP deletion
		} else {
			klog.V(3).Infof("InitializeFromCluster: fully unregistered service %s from ServiceGateway", lb)
		}

		// Step 4: Delete Public IP (if exists in Azure)
		if lbExistsInAzure {
			_, pipName, _ := buildInboundResourceNames(lb)
			if err := diffTracker.deletePublicIP(ctx, config.ResourceGroup, pipName); err != nil {
				klog.Errorf("InitializeFromCluster: failed to delete Public IP %s: %v", pipName, err)
				lastErr = err
			} else {
				klog.V(3).Infof("InitializeFromCluster: deleted Public IP %s", pipName)
			}
		}

		if lastErr == nil {
			klog.Infof("InitializeFromCluster: deleteInboundService completed successfully for %s", lb)
		} else {
			klog.Warningf("InitializeFromCluster: deleteInboundService completed with errors for %s", lb)
		}
	}

	for _, natGatewayId := range syncOperations.NATGatewayUpdates.Removals.UnsortedList() {
		klog.Infof("InitializeFromCluster: deleteOutboundService started for %s", natGatewayId)
		ngExistsInAzure := currentNATGatewaysInNRP.Has(natGatewayId)

		// Step 1: Disassociate NAT Gateway from ServiceGateway (ALWAYS)
		if err := diffTracker.disassociateNatGatewayFromServiceGateway(ctx, config.ServiceGatewayResourceName, natGatewayId); err != nil {
			klog.Warningf("InitializeFromCluster: failed to disassociate NAT Gateway %s from ServiceGateway: %v", natGatewayId, err)
			// Non-fatal - continue with deletion
		} else {
			klog.V(3).Infof("InitializeFromCluster: disassociated NAT Gateway %s from ServiceGateway", natGatewayId)
		}

		// Step 2: Unregister from ServiceGateway API (ALWAYS)
		unregisterDTO := buildServiceGatewayRemovalDTO(natGatewayId, false, config)
		if err := diffTracker.updateNRPSGWServices(ctx, config.ServiceGatewayResourceName, unregisterDTO); err != nil {
			klog.Errorf("InitializeFromCluster: failed to unregister outbound service %s from ServiceGateway: %v", natGatewayId, err)
			lastErr = err
			// Continue with deletion
		} else {
			klog.V(3).Infof("InitializeFromCluster: unregistered outbound service %s from ServiceGateway", natGatewayId)
		}

		// Step 3: Delete NAT Gateway (if exists in Azure)
		if ngExistsInAzure {
			if err := diffTracker.deleteNatGateway(ctx, config.ResourceGroup, natGatewayId); err != nil {
				klog.Errorf("InitializeFromCluster: failed to delete NAT Gateway %s: %v", natGatewayId, err)
				lastErr = err
				// Continue with PIP deletion
			} else {
				klog.V(3).Infof("InitializeFromCluster: deleted NAT Gateway %s", natGatewayId)
			}
		}

		// Step 4: Delete Public IP (if exists in Azure)
		if ngExistsInAzure {
			_, pipName := buildOutboundResourceNames(natGatewayId)
			if err := diffTracker.deletePublicIP(ctx, config.ResourceGroup, pipName); err != nil {
				klog.Errorf("InitializeFromCluster: failed to delete Public IP %s: %v", pipName, err)
				lastErr = err
			} else {
				klog.V(3).Infof("InitializeFromCluster: deleted Public IP %s", pipName)
			}
		}

		if lastErr == nil {
			klog.Infof("InitializeFromCluster: deleteOutboundService completed successfully for %s", natGatewayId)
		} else {
			klog.Warningf("InitializeFromCluster: deleteOutboundService completed with errors for %s", natGatewayId)
		}
	}

	// 14. Address orphaned PIP resources
	pipclient := networkClientFactory.GetPublicIPAddressClient()
	pips, err := pipclient.List(ctx, config.ResourceGroup)
	if err != nil {
		klog.Errorf("InitializeFromCluster: failed to list public IP addresses: %v", err)
		lastErr = err
	} else {
		for _, pip := range pips {
			if pip.Name == nil || !strings.HasSuffix(*pip.Name, "-pip") || *pip.Name == "default-natgw-v2-pip" {
				continue
			}

			associatedResourceName := strings.TrimSuffix(*pip.Name, "-pip")
			lbExists := diffTracker.NRPResources.LoadBalancers.Has(associatedResourceName)
			ngExists := diffTracker.NRPResources.NATGateways.Has(associatedResourceName)
			if !lbExists && !ngExists {
				klog.Infof("InitializeFromCluster: Deleting orphaned Public IP %s from NRP", *pip.Name)
				if err := diffTracker.deletePublicIP(ctx, config.ResourceGroup, *pip.Name); err != nil {
					klog.Warningf("InitializeFromCluster: failed to delete orphaned Public IP %s: %v", *pip.Name, err)
				}
			}
		}
	}

	// Mark initial sync as done
	diffTracker.InitialSyncDone = true
	isOperationSucceeded = lastErr == nil

	if lastErr != nil {
		klog.Warningf("InitializeFromCluster: completed with errors: %v", lastErr)
	} else {
		klog.Infof("InitializeFromCluster: completed successfully")
	}

	return diffTracker, lastErr
}

// Helper functions

func syncMapFromMap(localServiceNameToNRPServiceMap map[string]int) *sync.Map {
	var syncMap sync.Map
	for k, v := range localServiceNameToNRPServiceMap {
		syncMap.Store(k, v)
	}
	return &syncMap
}

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
			out[ks] = 0
		}
		return true
	})
	return out
}

// LogSyncStringIntMap logs the contents of a sync.Map of string keys to int values
func LogSyncStringIntMap(prefix string, m *sync.Map) {
	tmp := dumpStringIntSyncMap(m)
	keys := make([]string, 0, len(tmp))
	for k := range tmp {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	klog.Infof("%s size=%d", prefix, len(keys))
	for _, k := range keys {
		klog.V(4).Infof("%s entry: %s -> %d", prefix, k, tmp[k])
	}
}
