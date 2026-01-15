package difftracker

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

const (
	// workerPoolSize controls the number of concurrent workers for orphaned resource cleanup.
	// Increase this value to speed up cleanup of large numbers of orphaned resources.
	// Decrease if Azure API rate limiting becomes an issue.
	workerPoolSize = 10

	// taskDelay is the delay between task submissions to prevent Azure API throttling.
	// Increase this value if experiencing rate limiting errors during cleanup.
	// Decrease to speed up cleanup (but may trigger throttling).
	taskDelay = 100 * time.Millisecond
)

// isValidServiceUUID checks if a name matches the standard UUID format (8-4-4-4-12 hexadecimal)
// Used to distinguish service LoadBalancers (UUID names) from system LoadBalancers (e.g., "kubernetes")
var uuidRegex = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isValidServiceUUID(name string) bool {
	return uuidRegex.MatchString(strings.ToLower(name))
}

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

	// Validate inputs
	if err := validateInitializationInputs(kubeClient, networkClientFactory); err != nil {
		return nil, err
	}

	// Build K8s state from cluster (also returns lists for reuse by recoverStuckFinalizers)
	k8s, serviceList, serviceUIDToService, endpointSliceList, egressPodList, localServiceNameToNRPServiceMap, nodeNameToIPMap, err := buildK8sState(ctx, kubeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to build K8s state: %w", err)
	}

	// Build NRP state from Azure
	nrp, currentLoadBalancersInNRP, currentNATGatewaysInNRP, err := buildNRPState(ctx, config, networkClientFactory)
	if err != nil {
		return nil, fmt.Errorf("failed to build NRP state: %w", err)
	}

	// Initialize DiffTracker with computed state
	diffTracker := initializeDiffTrackerWithState(k8s, nrp, config, networkClientFactory, kubeClient, localServiceNameToNRPServiceMap)

	// Enhance NRP state with orphaned resources
	enhanceNRPStateWithOrphans(&diffTracker.NRPResources, *currentLoadBalancersInNRP, *currentNATGatewaysInNRP)

	// Recover resources stuck with finalizers from a previous crash
	// This must happen BEFORE informers start to avoid race conditions
	// Reuse lists from buildK8sState (avoids duplicate API calls)
	recoverStuckFinalizers(ctx, diffTracker, nodeNameToIPMap, serviceList, egressPodList, endpointSliceList)

	// Counter already initialized from K8s state during buildK8sState
	// After initialization sync completes, NRP will match K8s, so counter reflects final state
	// Note: Counter is NOT updated during initialization sync - only during runtime via updateK8sPodLocked

	// Log state before computing sync operations
	klog.Infof("InitializeFromCluster: K8s Services (%d): %v", diffTracker.K8sResources.Services.Len(), diffTracker.K8sResources.Services.UnsortedList())
	klog.Infof("InitializeFromCluster: NRP LoadBalancers (%d): %v", diffTracker.NRPResources.LoadBalancers.Len(), diffTracker.NRPResources.LoadBalancers.UnsortedList())
	klog.Infof("InitializeFromCluster: K8s Egresses (%d): %v", diffTracker.K8sResources.Egresses.Len(), diffTracker.K8sResources.Egresses.UnsortedList())
	klog.Infof("InitializeFromCluster: NRP NATGateways (%d): %v", diffTracker.NRPResources.NATGateways.Len(), diffTracker.NRPResources.NATGateways.UnsortedList())

	// Get sync operations
	syncOperations := diffTracker.GetSyncOperations()
	logSyncOperations(syncOperations)

	// Setup initialization mode and start updaters
	if err := startInitialization(ctx, diffTracker); err != nil {
		return nil, err
	}

	// Reconcile services (create/delete LBs and NAT Gateways in Azure)
	klog.Infof("InitializeFromCluster: reconciling services")
	diffTracker.reconcileServices(syncOperations, serviceUIDToService)

	// Trigger initial location sync if needed:
	// - For deletions: Clear orphaned locations so services can be deleted
	// - For existing services (no additions/deletions): Sync any location changes
	// - For new services: OnServiceCreationComplete will trigger after creation
	// - For recovered stuck finalizers: pendingPodDeletions need processing
	hasDeletions := syncOperations.LoadBalancerUpdates.Removals.Len() > 0 || syncOperations.NATGatewayUpdates.Removals.Len() > 0
	// hasOnlyExistingServices is true when we have NO new services to create.
	// This covers two cases:
	//   1. All services already exist in NRP (need location sync for potential updates)
	//   2. NO services exist at all (no-op, but harmless to trigger)
	// The key insight: when Additions > 0, OnServiceCreationComplete will trigger the sync,
	// so we don't need an explicit trigger here. When Additions == 0, no such callback exists.
	hasOnlyExistingServices := syncOperations.LoadBalancerUpdates.Additions.Len() == 0 && syncOperations.NATGatewayUpdates.Additions.Len() == 0

	// Check if we have pending items from recoverStuckFinalizers
	diffTracker.mu.Lock()
	hasRecoveredItems := len(diffTracker.pendingPodDeletions) > 0
	diffTracker.mu.Unlock()

	if hasDeletions || hasOnlyExistingServices || hasRecoveredItems {
		klog.Infof("InitializeFromCluster: triggering initial location sync (deletions=%v, onlyExisting=%v, recoveredItems=%v)",
			hasDeletions, hasOnlyExistingServices, hasRecoveredItems)
		diffTracker.triggerLocationsUpdater()
	}

	// Wait for all async operations to complete (service creations/deletions + location syncs)
	// WaitForInitialSync monitors:
	//   - pendingServiceOps (ServiceUpdater work)
	//   - pendingUpdaterTriggers (LocationsUpdater work)
	// Note: bufferedEndpoints/bufferedPods are always empty during initialization
	klog.Infof("InitializeFromCluster: waiting for all async operations to complete")
	if err := diffTracker.WaitForInitialSync(ctx); err != nil {
		klog.Errorf("InitializeFromCluster: WaitForInitialSync failed: %v", err)
		cleanupOnError(diffTracker)
		return nil, fmt.Errorf("initialization sync failed: %w", err)
	}
	klog.Infof("InitializeFromCluster: all async operations completed")

	// Cleanup orphaned Public IPs (non-fatal)
	cleanupOrphanedPIPs(ctx, diffTracker)

	// Mark initialization complete
	diffTracker.InitialSyncDone = true
	isOperationSucceeded = true
	klog.Infof("InitializeFromCluster: completed successfully")

	return diffTracker, nil
}

// ================================================================================================
// Initialization helper functions - broken down from InitializeFromCluster
// ================================================================================================

// validateInitializationInputs validates required inputs for initialization
func validateInitializationInputs(kubeClient kubernetes.Interface, networkClientFactory azclient.ClientFactory) error {
	if kubeClient == nil {
		return fmt.Errorf("KubeClient is nil; initialize the cloud provider with a Kubernetes client before diff tracker setup")
	}
	if networkClientFactory == nil {
		return fmt.Errorf("NetworkClientFactory is nil; cannot initialize diff tracker without Azure network clients")
	}
	return nil
}

// buildK8sState fetches and constructs the complete K8s state (services, endpoints, egresses)
// Also returns nodeNameToIPMap for reuse in recovery operations.
func buildK8sState(
	ctx context.Context,
	kubeClient kubernetes.Interface,
) (K8s_State, *v1.ServiceList, map[string]*v1.Service, *discoveryv1.EndpointSliceList, *v1.PodList, map[string]int, map[string]string, error) {
	k8s := K8s_State{
		Services: utilsets.NewString(),
		Egresses: utilsets.NewString(),
		Nodes:    make(map[string]Node),
	}
	localServiceNameToNRPServiceMap := make(map[string]int)

	// Build node name to IP mapping
	nodeNameToIPMap, err := buildNodeNameToIPMap(ctx, kubeClient)
	if err != nil {
		return K8s_State{}, nil, nil, nil, nil, nil, nil, err
	}

	// Fetch and process services
	serviceList, serviceUIDToService, err := processK8sServices(ctx, kubeClient, &k8s, localServiceNameToNRPServiceMap)
	if err != nil {
		return K8s_State{}, nil, nil, nil, nil, nil, nil, err
	}

	// Fetch and process endpoint slices
	endpointSliceList, err := processK8sEndpoints(ctx, kubeClient, &k8s, nodeNameToIPMap)
	if err != nil {
		return K8s_State{}, nil, nil, nil, nil, nil, nil, err
	}

	// Fetch and process egress pods
	egressPodList, err := processK8sEgresses(ctx, kubeClient, &k8s, nodeNameToIPMap, localServiceNameToNRPServiceMap)
	if err != nil {
		return K8s_State{}, nil, nil, nil, nil, nil, nil, err
	}

	return k8s, serviceList, serviceUIDToService, endpointSliceList, egressPodList, localServiceNameToNRPServiceMap, nodeNameToIPMap, nil
}

// buildNodeNameToIPMap creates a mapping from node names to their internal IPs
func buildNodeNameToIPMap(ctx context.Context, kubeClient kubernetes.Interface) (map[string]string, error) {
	nodeList, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	klog.Infof("buildNodeNameToIPMap: found %d nodes", len(nodeList.Items))

	nodeNameToIPMap := make(map[string]string, len(nodeList.Items))
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		for _, addr := range n.Status.Addresses {
			if addr.Type == v1.NodeInternalIP {
				nodeNameToIPMap[n.Name] = addr.Address
				break
			}
		}
	}
	klog.V(4).Infof("buildNodeNameToIPMap: built map with %d entries", len(nodeNameToIPMap))
	return nodeNameToIPMap, nil
}

// ================================================================================================
// RESTART RECOVERY - FINALIZER CLEANUP FOR ORPHANED RESOURCES
// ================================================================================================

// recoverStuckFinalizers finds services and pods that have our finalizer + DeletionTimestamp
// (indicating a crash during cleanup) and re-triggers the appropriate cleanup flows.
// This runs during initialization, BEFORE informers start, so it's safe from race conditions.
//
// IMPORTANT: Since processK8sServices and processK8sEgresses skip resources
// with DeletionTimestamp, these stuck resources are NOT in K8s state or counters. We just need to:
// 1. Track them in pending deletions for finalizer removal
// 2. Trigger LocationsUpdater to sync their addresses out of NRP
//
// Recovery strategy:
//   - For Services: The diff mechanism handles LB/NAT deletion (not in K8s.Services → marked for removal)
//     Just log for visibility; no explicit pendingServiceDeletions needed as GetSyncOperations will handle it
//   - For Pods with valid addresses: Track in pendingPodDeletions (don't call DeletePod - counters are clean)
//   - For Pods with missing addresses: Directly remove finalizer (nothing to sync)
//   - For malformed resources (no egress label): Directly remove finalizer
//
// NOTE: EndpointSlices do not use finalizers - their deletion is handled directly by the informer.
//
// Optimization: This function receives pre-fetched lists from buildK8sState to avoid duplicate API calls.
func recoverStuckFinalizers(
	ctx context.Context,
	dt *DiffTracker,
	nodeNameToIPMap map[string]string,
	services *v1.ServiceList,
	egressPods *v1.PodList,
	endpointSlices *discoveryv1.EndpointSliceList,
) {
	klog.Infof("recoverStuckFinalizers: scanning for resources stuck with finalizers")

	servicesRecovered := 0
	podsRecovered := 0
	podsDirectCleaned := 0

	// Collect pending items in local maps first, then batch-insert with a single lock
	pendingPods := make(map[string]*PendingPodDeletion)

	// Recover stuck services (LoadBalancer services with our finalizer + DeletionTimestamp)
	// NOTE: Unlike pods, services don't need explicit pendingServiceDeletions tracking here.
	// The diff mechanism handles it: processK8sServices skips this service → not in K8s.Services
	// → GetSyncOperations sees LB in NRP but not K8s → marked for Removal → reconcileServices deletes it
	// Service finalizer is removed in OnServiceDeletionComplete after LB/NAT deletion
	if services == nil {
		klog.Errorf("recoverStuckFinalizers: services list is nil, skipping service recovery")
	} else {
		for i := range services.Items {
			svc := &services.Items[i]

			// Only process LoadBalancer services
			if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
				continue
			}

			// Check if service has our finalizer AND is being deleted
			if svc.DeletionTimestamp == nil {
				continue
			}
			if !hasFinalizer(svc.Finalizers, ServiceGatewayServiceCleanupFinalizer) {
				continue
			}

			uid := strings.ToLower(string(svc.UID))
			klog.Infof("recoverStuckFinalizers: found stuck service %s/%s (uid=%s), will be handled by diff mechanism",
				svc.Namespace, svc.Name, uid)
			servicesRecovered++
		}
	}

	// Recover stuck pods (egress pods with our finalizer + DeletionTimestamp)
	if egressPods == nil {
		klog.Errorf("recoverStuckFinalizers: egressPods list is nil, skipping pod recovery")
	} else {
		for i := range egressPods.Items {
			pod := &egressPods.Items[i]

			// Check if pod has our finalizer AND is being deleted
			if pod.DeletionTimestamp == nil {
				continue
			}
			if !hasFinalizer(pod.Finalizers, ServiceGatewayPodCleanupFinalizer) {
				continue
			}


			// This pod was mid-deletion when we crashed - re-trigger cleanup
			egressLabel := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
			if egressLabel == "" {
				// No egress label = nothing to track, just remove finalizer directly
				klog.Warningf("recoverStuckFinalizers: pod %s/%s has finalizer but missing egress label, removing finalizer directly",
					pod.Namespace, pod.Name)
				if err := dt.removePodFinalizer(ctx, pod); err != nil {
					klog.Errorf("recoverStuckFinalizers: failed to remove finalizer from pod %s/%s: %v",
						pod.Namespace, pod.Name, err)
				} else {
					podsDirectCleaned++
				}
				continue
			}

			// Get addresses - may be empty if pod was already terminating
			podIP := pod.Status.PodIP
			nodeIP := ""
			if pod.Spec.NodeName != "" {
				nodeIP = nodeNameToIPMap[pod.Spec.NodeName]
			}

			// If we don't have addresses, we can't track for sync through DeletePod
			// (DeletePod rejects empty location/address). Directly remove finalizer since
			// there's nothing to sync out of NRP anyway.
			if podIP == "" || nodeIP == "" {
				klog.Warningf("recoverStuckFinalizers: pod %s/%s has finalizer but missing addresses (podIP=%s, nodeIP=%s), removing finalizer directly",
					pod.Namespace, pod.Name, podIP, nodeIP)
				if err := dt.removePodFinalizer(ctx, pod); err != nil {
					klog.Errorf("recoverStuckFinalizers: failed to remove finalizer from pod %s/%s: %v",
						pod.Namespace, pod.Name, err)
				} else {
					podsDirectCleaned++
				}
				continue
			}

			klog.Infof("recoverStuckFinalizers: recovering stuck pod %s/%s (egress=%s, location=%s, address=%s)",
				pod.Namespace, pod.Name, egressLabel, nodeIP, podIP)

			// Collect for batch insertion (Issue 2.4: avoid lock contention)
			// NOTE: We do NOT call DeletePod() because:
			// 1. The pod was not counted in processK8sEgresses (we skip pods with DeletionTimestamp)
			// 2. DeletePod would try to decrement a counter that doesn't include this pod
			// 3. We just need to sync the address out of NRP and remove the finalizer
			podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			pendingPods[podKey] = &PendingPodDeletion{
				Namespace:  pod.Namespace,
				Name:       pod.Name,
				ServiceUID: egressLabel,
				Address:    podIP,
				Location:   nodeIP,
				// IsLastPod=false is INTENTIONAL even if this was actually the last pod.
				// During initialization, NAT Gateway deletion is handled by the DIFF mechanism:
				// - processK8sEgresses skips this pod → egress NOT in K8s.Egresses
				// - GetSyncOperations sees NAT in NRP but not K8s → marks for Removal
				// - reconcileServices queues the NAT Gateway deletion
				// So we don't need IsLastPod=true to trigger service deletion - the diff does it.
				// Setting IsLastPod=false allows the finalizer to be removed immediately after
				// the address sync, rather than waiting for NAT Gateway deletion callback.
				IsLastPod: false,
				Timestamp: time.Now().Format(time.RFC3339),
			}
			podsRecovered++
		}
	}

	// NOTE: EndpointSlices do not use finalizers - their deletion is handled directly
	// by the endpointSlice informer's DeleteFunc calling UpdateEndpoints.
	// The endpointSlices parameter is only used for building initial state.
	_ = endpointSlices

	// Batch-insert all pending items with a single lock (Issue 2.4: reduce lock contention)
	if len(pendingPods) > 0 {
		dt.mu.Lock()
		for key, val := range pendingPods {
			dt.pendingPodDeletions[key] = val
		}
		dt.mu.Unlock()
	}

	// NOTE: We do NOT trigger LocationsUpdater here because it's not started yet.
	// The pending items will be picked up when the existing location sync trigger
	// fires after startInitialization() completes.

	if servicesRecovered > 0 || podsRecovered > 0 || podsDirectCleaned > 0 {
		klog.Infof("recoverStuckFinalizers: found %d stuck services (handled by diff), recovered %d pods, direct-cleaned %d pods",
			servicesRecovered, podsRecovered, podsDirectCleaned)
	} else {
		klog.Infof("recoverStuckFinalizers: no stuck resources found")
	}
}

// processK8sServices fetches and processes LoadBalancer services from K8s
// NOTE: Services with DeletionTimestamp are EXCLUDED because they are being deleted.
// Their cleanup is handled by recoverStuckFinalizers.
func processK8sServices(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	k8s *K8s_State,
	localServiceNameToNRPServiceMap map[string]int,
) (*v1.ServiceList, map[string]*v1.Service, error) {
	services, err := kubeClient.CoreV1().Services(v1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list services: %w", err)
	}

	serviceUIDToService := make(map[string]*v1.Service)
	for i, service := range services.Items {
		if service.Spec.Type == v1.ServiceTypeLoadBalancer {
			// Skip services that are being deleted - they shouldn't count in K8s state
			// Their deletion will be handled by recoverStuckFinalizers or the diff mechanism
			if service.DeletionTimestamp != nil {
				klog.V(4).Infof("processK8sServices: skipping service %s/%s with DeletionTimestamp", service.Namespace, service.Name)
				continue
			}
			uid := strings.ToLower(string(service.UID))
			k8s.Services.Insert(uid)
			// Initialize reference counter with sentinel value -34
			localServiceNameToNRPServiceMap[uid] = -34
			serviceUIDToService[uid] = &services.Items[i]
		}
	}
	klog.Infof("processK8sServices: found %d LoadBalancer services", k8s.Services.Len())
	return services, serviceUIDToService, nil
}

// processK8sEndpoints fetches endpoint slices and populates K8s nodes/pods with inbound identities
// NOTE: EndpointSlices with DeletionTimestamp are EXCLUDED because they are being deleted.
// Their addresses will be synced out by recoverStuckFinalizers.
func processK8sEndpoints(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	k8s *K8s_State,
	nodeNameToIPMap map[string]string,
) (*discoveryv1.EndpointSliceList, error) {
	endpointSliceList, err := kubeClient.DiscoveryV1().EndpointSlices(v1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list endpointslices: %w", err)
	}

	processedCount := 0
	for _, endpointSlice := range endpointSliceList.Items {
		// Skip EndpointSlices that are being deleted - their addresses shouldn't be in K8s state
		// Their cleanup is handled by recoverStuckFinalizers
		if endpointSlice.DeletionTimestamp != nil {
			klog.V(4).Infof("processK8sEndpoints: skipping EndpointSlice %s/%s with DeletionTimestamp",
				endpointSlice.Namespace, endpointSlice.Name)
			continue
		}

		serviceUID := extractServiceUIDFromEndpointSlice(&endpointSlice)
		if serviceUID == "" || !k8s.Services.Has(serviceUID) {
			continue
		}

		for _, endpoint := range endpointSlice.Endpoints {
			if endpoint.NodeName == nil || len(endpoint.Addresses) == 0 {
				continue
			}

			nodeIP, exists := nodeNameToIPMap[*endpoint.NodeName]
			if !exists {
				klog.V(4).Infof("processK8sEndpoints: could not find IP for node %s", *endpoint.NodeName)
				continue
			}

			ensureNodeExists(k8s, nodeIP)
			for _, podIP := range endpoint.Addresses {
				addInboundIdentityToPod(k8s, nodeIP, podIP, serviceUID)
			}
		}
		processedCount++
	}
	klog.Infof("processK8sEndpoints: processed %d endpointslices (total %d, skipped deleting)",
		processedCount, len(endpointSliceList.Items))
	return endpointSliceList, nil
}

// processK8sEgresses fetches egress pods and populates K8s nodes/pods with outbound identities
// NOTE: Pods with DeletionTimestamp are EXCLUDED because they are being deleted and should not
// contribute to the pod counter. Their cleanup is handled by recoverStuckFinalizers.
// Returns the raw pod list for reuse by recoverStuckFinalizers to avoid duplicate API calls.
func processK8sEgresses(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	k8s *K8s_State,
	nodeNameToIPMap map[string]string,
	localServiceNameToNRPServiceMap map[string]int,
) (*v1.PodList, error) {
	egressPods, err := kubeClient.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: consts.PodLabelServiceEgressGateway,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods with egress label: %w", err)
	}

	for _, pod := range egressPods.Items {
		// Skip pods that are being deleted - they shouldn't count toward the service
		// Their addresses will be synced out by recoverStuckFinalizers or during normal deletion
		if pod.DeletionTimestamp != nil {
			klog.V(4).Infof("processK8sEgresses: skipping pod %s/%s with DeletionTimestamp", pod.Namespace, pod.Name)
			continue
		}

		egressVal := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
		if egressVal == "" || pod.Status.PodIP == "" || pod.Spec.NodeName == "" {
			continue
		}

		k8s.Egresses.Insert(egressVal)

		nodeIP, exists := nodeNameToIPMap[pod.Spec.NodeName]
		if !exists {
			klog.V(4).Infof("processK8sEgresses: could not find IP for node %s", pod.Spec.NodeName)
			continue
		}

		ensureNodeExists(k8s, nodeIP)
		addOutboundIdentityToPod(k8s, nodeIP, pod.Status.PodIP, egressVal)
		localServiceNameToNRPServiceMap[egressVal] = localServiceNameToNRPServiceMap[egressVal] + 1
	}
	klog.Infof("processK8sEgresses: found %d egress services", k8s.Egresses.Len())
	return egressPods, nil
}

// buildNRPState fetches and constructs the complete NRP state (services, locations, LBs, NATs)
func buildNRPState(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
) (NRP_State, *utilsets.IgnoreCaseSet, *utilsets.IgnoreCaseSet, error) {
	nrp := NRP_State{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(),
		Locations:     make(map[string]NRPLocation),
	}

	// Fetch ServiceGateway services
	if err := fetchServiceGatewayServices(ctx, config, networkClientFactory, &nrp); err != nil {
		return NRP_State{}, nil, nil, err
	}

	// Fetch ServiceGateway locations
	if err := fetchServiceGatewayLocations(ctx, config, networkClientFactory, &nrp); err != nil {
		return NRP_State{}, nil, nil, err
	}

	// Fetch Azure LoadBalancers
	currentLBs, err := fetchAzureLoadBalancers(ctx, config, networkClientFactory)
	if err != nil {
		return NRP_State{}, nil, nil, err
	}

	// Fetch Azure NAT Gateways
	currentNATs, err := fetchAzureNATGateways(ctx, config, networkClientFactory)
	if err != nil {
		return NRP_State{}, nil, nil, err
	}

	return nrp, currentLBs, currentNATs, nil
}

// fetchServiceGatewayServices fetches services from ServiceGateway API
func fetchServiceGatewayServices(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
	nrp *NRP_State,
) error {
	sgwClient := networkClientFactory.GetServiceGatewayClient()
	servicesDTO, err := sgwClient.GetServices(ctx, config.ResourceGroup, config.ServiceGatewayResourceName)
	if err != nil {
		return fmt.Errorf("failed to get services from ServiceGateway API: %w", err)
	}

	for _, service := range servicesDTO {
		if service == nil || service.Properties == nil || service.Properties.ServiceType == nil || service.Name == nil {
			klog.V(4).Infof("fetchServiceGatewayServices: skipping service entry with nil fields")
			continue
		}

		switch *service.Properties.ServiceType {
		case "Inbound":
			nrp.LoadBalancers.Insert(*service.Name)
		case "Outbound":
			if *service.Name != "default-natgw-v2" {
				nrp.NATGateways.Insert(*service.Name)
			}
		}
	}
	klog.Infof("fetchServiceGatewayServices: fetched %d services (%d LBs, %d NATs)",
		len(servicesDTO), nrp.LoadBalancers.Len(), nrp.NATGateways.Len())
	return nil
}

// fetchServiceGatewayLocations fetches address locations from ServiceGateway API
func fetchServiceGatewayLocations(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
	nrp *NRP_State,
) error {
	sgwClient := networkClientFactory.GetServiceGatewayClient()
	locationsDTO, err := sgwClient.GetAddressLocations(ctx, config.ResourceGroup, config.ServiceGatewayResourceName)
	if err != nil {
		return fmt.Errorf("failed to get locations from ServiceGateway API: %w", err)
	}
	klog.Infof("fetchServiceGatewayLocations: fetched %d locations", len(locationsDTO))

	for _, location := range locationsDTO {
		if location == nil || location.AddressLocation == nil || *location.AddressLocation == "" {
			klog.V(4).Infof("fetchServiceGatewayLocations: skipping invalid location entry")
			continue
		}

		addresses := parseLocationAddresses(location)
		nrp.Locations[*location.AddressLocation] = NRPLocation{Addresses: addresses}
	}
	klog.Infof("fetchServiceGatewayLocations: processed %d locations", len(nrp.Locations))
	return nil
}

// fetchAzureLoadBalancers fetches LoadBalancers from Azure
func fetchAzureLoadBalancers(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
) (*utilsets.IgnoreCaseSet, error) {
	lbclient := networkClientFactory.GetLoadBalancerClient()
	lbs, err := lbclient.List(ctx, config.ResourceGroup)
	if err != nil {
		return nil, fmt.Errorf("failed to list load balancers: %w", err)
	}

	currentLBs := utilsets.NewString()
	for _, lb := range lbs {
		if lb.Name != nil {
			currentLBs.Insert(strings.ToLower(*lb.Name))
		}
	}
	klog.Infof("fetchAzureLoadBalancers: found %d LoadBalancers", currentLBs.Len())
	return currentLBs, nil
}

// fetchAzureNATGateways fetches NAT Gateways from Azure
func fetchAzureNATGateways(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
) (*utilsets.IgnoreCaseSet, error) {
	ngclient := networkClientFactory.GetNatGatewayClient()
	ngs, err := ngclient.List(ctx, config.ResourceGroup)
	if err != nil {
		return nil, fmt.Errorf("failed to list nat gateways: %w", err)
	}

	currentNATs := utilsets.NewString()
	for _, ng := range ngs {
		if ng.Name != nil {
			currentNATs.Insert(strings.ToLower(*ng.Name))
		}
	}
	klog.Infof("fetchAzureNATGateways: found %d NAT Gateways", currentNATs.Len())
	return currentNATs, nil
}

// initializeDiffTrackerWithState creates a DiffTracker and populates initial state
func initializeDiffTrackerWithState(
	k8s K8s_State,
	nrp NRP_State,
	config Config,
	networkClientFactory azclient.ClientFactory,
	kubeClient kubernetes.Interface,
	localServiceNameToNRPServiceMap map[string]int,
) *DiffTracker {
	diffTracker := InitializeDiffTracker(k8s, nrp, config, networkClientFactory, kubeClient)
	diffTracker.LocalServiceNameToNRPServiceMap = *syncMapFromMap(localServiceNameToNRPServiceMap)
	LogSyncStringIntMap("initializeDiffTrackerWithState: LocalServiceNameToNRPServiceMap", &diffTracker.LocalServiceNameToNRPServiceMap)
	return diffTracker
}

// logSyncOperations logs the sync operations summary
func logSyncOperations(syncOps *SyncDiffTrackerReturnType) {
	klog.Infof("Sync operations - LB additions: %d, LB removals: %d, NAT additions: %d, NAT removals: %d, locations: %d",
		syncOps.LoadBalancerUpdates.Additions.Len(),
		syncOps.LoadBalancerUpdates.Removals.Len(),
		syncOps.NATGatewayUpdates.Additions.Len(),
		syncOps.NATGatewayUpdates.Removals.Len(),
		len(syncOps.LocationData.Locations))
}

// startInitialization sets up initialization mode and starts updaters
func startInitialization(ctx context.Context, diffTracker *DiffTracker) error {
	diffTracker.mu.Lock()
	atomic.StoreInt32(&diffTracker.isInitializing, 1)
	diffTracker.initCompletionChecker = make(chan struct{})
	diffTracker.mu.Unlock()

	klog.Infof("startInitialization: starting ServiceUpdater and LocationsUpdater")
	diffTracker.serviceUpdater = NewServiceUpdater(ctx, diffTracker, diffTracker.OnServiceCreationComplete, diffTracker.GetServiceUpdaterTrigger())
	diffTracker.locationsUpdater = NewLocationsUpdater(ctx, diffTracker)
	go diffTracker.serviceUpdater.Run()
	go diffTracker.locationsUpdater.Run()

	// Give updaters time to start their event loops
	time.Sleep(50 * time.Millisecond)
	return nil
}

// performReconciliation reconciles all resources using Engine flows
// COMMENTED OUT FOR VERIFICATION: Endpoint/pod reconciliation appears redundant
// since state is already fully populated and GetSyncLocationsAddresses() computes diffs
/*
func performReconciliation(
	diffTracker *DiffTracker,
	syncOps *SyncDiffTrackerReturnType,
	serviceUIDToService map[string]*v1.Service,
	nrp NRP_State,
	endpointSliceList *discoveryv1.EndpointSliceList,
	k8sNodes map[string]Node,
) error {
	klog.Infof("performReconciliation: starting reconciliation using Engine flows")

	// Reconcile services (deletions first, then additions)
	diffTracker.reconcileServices(syncOps, serviceUIDToService)

	// Reconcile inbound endpoints
	nodeNameToIPMap, err := buildNodeNameToIPMap(context.Background(), diffTracker.kubeClient)
	if err != nil {
		return fmt.Errorf("failed to rebuild node name map: %w", err)
	}
	diffTracker.reconcileInboundEndpoints(nrp, endpointSliceList, nodeNameToIPMap)

	// Reconcile outbound pods
	diffTracker.reconcileOutboundPods(nrp, k8sNodes)

	return nil
}
*/

// cleanupOnError cleans up initialization state on failure
func cleanupOnError(diffTracker *DiffTracker) {
	if diffTracker.serviceUpdater != nil {
		diffTracker.serviceUpdater.Stop()
	}
	if diffTracker.locationsUpdater != nil {
		diffTracker.locationsUpdater.Stop()
	}

	diffTracker.mu.Lock()
	defer diffTracker.mu.Unlock()
	if atomic.LoadInt32(&diffTracker.isInitializing) == 1 {
		atomic.StoreInt32(&diffTracker.isInitializing, 0)
		if diffTracker.initCompletionChecker != nil {
			close(diffTracker.initCompletionChecker)
		}
	}
}

// cleanupOrphanedPIPs attempts to cleanup orphaned Public IPs (non-fatal)
func cleanupOrphanedPIPs(ctx context.Context, diffTracker *DiffTracker) {
	klog.Infof("cleanupOrphanedPIPs: checking for orphaned Public IPs")
	if err := diffTracker.cleanupOrphanedPublicIPs(ctx); err != nil {
		klog.Warningf("cleanupOrphanedPIPs: failed to cleanup orphaned Public IPs: %v", err)
	}
}

// ================================================================================================
// K8s state helper functions
// ================================================================================================

// extractServiceUIDFromEndpointSlice extracts the service UID from an endpoint slice
func extractServiceUIDFromEndpointSlice(endpointSlice *discoveryv1.EndpointSlice) string {
	for _, ownerRef := range endpointSlice.OwnerReferences {
		if ownerRef.Kind == "Service" {
			return string(ownerRef.UID)
		}
	}
	return ""
}

// ensureNodeExists ensures a node entry exists in K8s state
func ensureNodeExists(k8s *K8s_State, nodeIP string) {
	if _, exists := k8s.Nodes[nodeIP]; !exists {
		k8s.Nodes[nodeIP] = Node{Pods: make(map[string]Pod)}
	}
}

// addInboundIdentityToPod adds an inbound identity to a pod
func addInboundIdentityToPod(k8s *K8s_State, nodeIP, podIP, serviceUID string) {
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

// addOutboundIdentityToPod adds an outbound identity to a pod
func addOutboundIdentityToPod(k8s *K8s_State, nodeIP, podIP, egressVal string) {
	pod, exists := k8s.Nodes[nodeIP].Pods[podIP]
	if !exists {
		pod = Pod{
			InboundIdentities:      utilsets.NewString(),
			PublicOutboundIdentity: "",
		}
	}
	pod.PublicOutboundIdentity = egressVal
	k8s.Nodes[nodeIP].Pods[podIP] = pod
}

// ================================================================================================
// NRP state helper functions
// ================================================================================================

// parseLocationAddresses parses addresses from a location DTO
// Note: This expects the same location type returned by sgwClient.GetAddressLocations
func parseLocationAddresses(location interface{}) map[string]NRPAddress {
	// Type definition matching the actual DTO structure from ServiceGateway API
	// This should match the type returned by sgwClient.GetAddressLocations
	type AddressDTO struct {
		Address  *string
		Services []*string
	}
	type LocationDTO struct {
		AddressLocation *string
		Addresses       []*AddressDTO
	}

	// Use reflection to access fields since we don't know the exact concrete type
	// The caller passes the location object from the API response
	v := reflect.ValueOf(location)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return make(map[string]NRPAddress)
	}

	addresses := make(map[string]NRPAddress)
	addressesField := v.FieldByName("Addresses")
	if !addressesField.IsValid() || addressesField.Kind() != reflect.Slice {
		return addresses
	}

	for i := 0; i < addressesField.Len(); i++ {
		addrVal := addressesField.Index(i)
		if addrVal.Kind() == reflect.Ptr {
			if addrVal.IsNil() {
				continue
			}
			addrVal = addrVal.Elem()
		}

		addrField := addrVal.FieldByName("Address")
		if !addrField.IsValid() {
			continue
		}
		if addrField.Kind() == reflect.Ptr {
			if addrField.IsNil() {
				continue
			}
			addrField = addrField.Elem()
		}
		address := addrField.String()
		if address == "" {
			continue
		}

		services := utilsets.NewString()
		servicesField := addrVal.FieldByName("Services")
		if servicesField.IsValid() && servicesField.Kind() == reflect.Slice {
			for j := 0; j < servicesField.Len(); j++ {
				svcVal := servicesField.Index(j)
				if svcVal.Kind() == reflect.Ptr {
					if svcVal.IsNil() {
						continue
					}
					svcVal = svcVal.Elem()
				}
				services.Insert(svcVal.String())
			}
		}
		addresses[address] = NRPAddress{Services: services}
	}
	return addresses
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

// WorkerPool manages a pool of worker goroutines for parallel task execution
type WorkerPool struct {
	tasks chan func() error
	wg    sync.WaitGroup
	mu    sync.Mutex
	err   error
}

// NewWorkerPool creates a new worker pool with the specified number of workers
func NewWorkerPool(workers int) *WorkerPool {
	p := &WorkerPool{
		tasks: make(chan func() error),
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for task := range p.tasks {
				if err := task(); err != nil {
					p.mu.Lock()
					if p.err == nil {
						p.err = err
					}
					p.mu.Unlock()
				}
			}
		}()
	}
	return p
}

// Submit adds a task to the worker pool with inter-task delay for rate limiting
func (p *WorkerPool) Submit(task func() error) {
	time.Sleep(taskDelay)
	p.tasks <- task
}

// Wait closes the task channel and waits for all workers to complete, returning the first error encountered
func (p *WorkerPool) Wait() error {
	close(p.tasks)
	p.wg.Wait()
	return p.err
}

// ================================================================================================
// Helper functions for initialization reconciliation
// ================================================================================================

// extractOldEndpointsFromNRP extracts the current endpoint state from NRP for a given inbound service
// Returns map[podIP]nodeIP
func extractOldEndpointsFromNRP(serviceUID string, nrp NRP_State) map[string]string {
	oldEndpoints := make(map[string]string)

	// Iterate through all NRP locations to find addresses with this service's InboundIdentity
	for location, nrpLocation := range nrp.Locations {
		for address, nrpAddress := range nrpLocation.Addresses {
			if nrpAddress.Services.Has(serviceUID) {
				// location is nodeIP, address is podIP
				oldEndpoints[address] = location
			}
		}
	}

	return oldEndpoints
}

// extractNewEndpointsFromK8s extracts endpoint state from K8s endpointslices for a given service
// Returns map[podIP]nodeIP
func extractNewEndpointsFromK8s(serviceUID string, endpointSliceList *discoveryv1.EndpointSliceList, nodeNameToIPMap map[string]string) map[string]string {
	newEndpoints := make(map[string]string)

	for _, endpointSlice := range endpointSliceList.Items {
		// Check if this endpointSlice belongs to our service
		ownerServiceUID := ""
		for _, ownerRef := range endpointSlice.OwnerReferences {
			if ownerRef.Kind == "Service" {
				ownerServiceUID = strings.ToLower(string(ownerRef.UID))
				break
			}
		}

		if ownerServiceUID != serviceUID {
			continue
		}

		// Extract endpoints from this slice
		for _, endpoint := range endpointSlice.Endpoints {
			if endpoint.NodeName == nil || len(endpoint.Addresses) == 0 {
				continue
			}

			nodeIP, exists := nodeNameToIPMap[*endpoint.NodeName]
			if !exists {
				klog.V(4).Infof("extractNewEndpointsFromK8s: Could not find IP for node %s", *endpoint.NodeName)
				continue
			}

			for _, podIP := range endpoint.Addresses {
				newEndpoints[podIP] = nodeIP
			}
		}
	}

	return newEndpoints
}

// extractOldPodsFromNRP extracts the current pod state from NRP for a given outbound service
// Returns map[location:address]struct{} as a set
func extractOldPodsFromNRP(serviceUID string, nrp NRP_State) map[string]struct{} {
	oldPods := make(map[string]struct{})

	// Iterate through all NRP locations to find addresses with this service's PublicOutboundIdentity
	for location, nrpLocation := range nrp.Locations {
		for address, nrpAddress := range nrpLocation.Addresses {
			if nrpAddress.Services.Has(serviceUID) {
				// Create composite key for location:address
				key := location + ":" + address
				oldPods[key] = struct{}{}
			}
		}
	}

	return oldPods
}

// extractNewPodsFromK8s extracts pod state from K8s nodes for a given egress service
// Returns map[location:address]podKey where podKey is used for logging
func extractNewPodsFromK8s(serviceUID string, k8sNodes map[string]Node) map[string]string {
	newPods := make(map[string]string)

	// Iterate through all nodes and pods to find pods with this PublicOutboundIdentity
	for nodeIP, node := range k8sNodes {
		for podIP, pod := range node.Pods {
			if strings.ToLower(pod.PublicOutboundIdentity) == serviceUID {
				// Create composite key for location:address
				key := nodeIP + ":" + podIP
				// Store podKey (using podIP as key for now since we don't have full pod metadata)
				newPods[key] = podIP
			}
		}
	}

	return newPods
}

// enhanceNRPStateWithOrphans adds orphaned Azure resources to NRP state for deletion tracking
// Orphaned resources are those that exist in Azure but not in ServiceGateway
func enhanceNRPStateWithOrphans(nrp *NRP_State, currentLBs, currentNATs utilsets.IgnoreCaseSet) {
	orphanedLBCount := 0
	orphanedNATCount := 0

	// Add orphaned LoadBalancers (UUID names that exist in Azure but not in ServiceGateway)
	for _, lbName := range currentLBs.UnsortedList() {
		if !isValidServiceUUID(lbName) {
			continue // Skip system LoadBalancers
		}
		if !nrp.LoadBalancers.Has(lbName) {
			nrp.LoadBalancers.Insert(lbName)
			orphanedLBCount++
		}
	}

	// Add orphaned NAT Gateways (non-default that exist in Azure but not in ServiceGateway)
	for _, natName := range currentNATs.UnsortedList() {
		if natName == "default-natgw-v2" {
			continue // Skip default NAT Gateway
		}
		if !nrp.NATGateways.Has(natName) {
			nrp.NATGateways.Insert(natName)
			orphanedNATCount++
		}
	}

	if orphanedLBCount > 0 || orphanedNATCount > 0 {
		klog.Infof("enhanceNRPStateWithOrphans: found %d orphaned LBs and %d orphaned NAT Gateways", orphanedLBCount, orphanedNATCount)
	}
}

// ================================================================================================
// Reconciliation methods for initialization
// ================================================================================================

// reconcileServices reconciles service additions and deletions using Engine flows
// Calls DeleteService for removals, AddService for additions
// This is the ONLY reconciliation method still needed - endpoint/pod reconciliation is redundant
func (dt *DiffTracker) reconcileServices(syncOps *SyncDiffTrackerReturnType, serviceUIDToService map[string]*v1.Service) {
	klog.Infof("reconcileServices: starting service reconciliation")

	// Process deletions first (LB + NAT deletions)
	totalDeletions := syncOps.LoadBalancerUpdates.Removals.Len() + syncOps.NATGatewayUpdates.Removals.Len()
	if totalDeletions > 0 {
		klog.Infof("reconcileServices: processing %d service deletions", totalDeletions)

		// Call DeleteService for each service - they'll be batched by ServiceUpdater
		for _, serviceUID := range syncOps.LoadBalancerUpdates.Removals.UnsortedList() {
			klog.V(3).Infof("reconcileServices: calling DeleteService for LB %s", serviceUID)
			dt.DeleteService(serviceUID, true)
		}
		for _, serviceUID := range syncOps.NATGatewayUpdates.Removals.UnsortedList() {
			klog.V(3).Infof("reconcileServices: calling DeleteService for NAT %s", serviceUID)
			dt.DeleteService(serviceUID, false)
		}
	}

	// Process additions
	lbAdditions := syncOps.LoadBalancerUpdates.Additions.UnsortedList()
	natAdditions := syncOps.NATGatewayUpdates.Additions.UnsortedList()
	totalAdditions := len(lbAdditions) + len(natAdditions)

	if totalAdditions > 0 {
		klog.Infof("reconcileServices: processing %d service additions (%d LBs, %d NATs)",
			totalAdditions, len(lbAdditions), len(natAdditions))

		for _, serviceUID := range lbAdditions {
			svc, exists := serviceUIDToService[serviceUID]
			var inboundConfig *InboundConfig
			if exists && svc != nil {
				inboundConfig = ExtractInboundConfigFromService(svc)
			}
			config := NewInboundServiceConfig(serviceUID, inboundConfig)
			klog.V(3).Infof("reconcileServices: calling AddService for LB %s", serviceUID)
			dt.AddService(config)
		}

		for _, serviceUID := range natAdditions {
			config := NewOutboundServiceConfig(serviceUID, nil)
			klog.V(3).Infof("reconcileServices: calling AddService for NAT %s", serviceUID)
			dt.AddService(config)
		}
	}

	klog.Infof("reconcileServices: completed service reconciliation")
}

/*
// REDUNDANT METHOD - Commented out for verification
// This method is no longer needed because:
// 1. dt.K8sResources.InboundIdentities is already fully populated from processK8sEndpoints()
// 2. dt.NRPResources.Locations is already fully populated from fetchServiceGatewayLocations()
// 3. GetSyncLocationsAddresses() automatically computes the diff between these states
// 4. UpdateEndpoints() calls would modify the already-correct state unnecessarily
//
// reconcileInboundEndpoints reconciles endpoints for all inbound (LoadBalancer) services
// Calls UpdateEndpoints with old state from NRP and new state from K8s
func (dt *DiffTracker) reconcileInboundEndpoints(nrp NRP_State, endpointSliceList *discoveryv1.EndpointSliceList, nodeNameToIPMap map[string]string) {
	klog.Infof("reconcileInboundEndpoints: starting endpoint reconciliation")

	// Build union of all services: K8s services + NRP services
	// We need to process both to handle:
	// 1. Services in K8s (existing or being added) - update their endpoints
	// 2. Services in NRP but not K8s (being removed) - clear their endpoints to unblock deletion
	allServicesSet := utilsets.NewString()
	for _, svc := range dt.K8sResources.Services.UnsortedList() {
		allServicesSet.Insert(svc)
	}
	for _, svc := range dt.NRPResources.LoadBalancers.UnsortedList() {
		allServicesSet.Insert(svc)
	}

	processedCount := 0

	for _, serviceUID := range allServicesSet.UnsortedList() {
		// Extract old endpoints from NRP
		oldEndpoints := extractOldEndpointsFromNRP(serviceUID, nrp)

		// Extract new endpoints from K8s
		newEndpoints := extractNewEndpointsFromK8s(serviceUID, endpointSliceList, nodeNameToIPMap)

		// Only call UpdateEndpoints if there's a difference
		if len(oldEndpoints) > 0 || len(newEndpoints) > 0 {
			klog.V(3).Infof("reconcileInboundEndpoints: calling UpdateEndpoints for %s (old=%d, new=%d)",
				serviceUID, len(oldEndpoints), len(newEndpoints))
			dt.UpdateEndpoints(serviceUID, oldEndpoints, newEndpoints)
			processedCount++
		}
	}

	klog.Infof("reconcileInboundEndpoints: completed endpoint reconciliation for %d services", processedCount)
}
*/

/*
// REDUNDANT METHOD - Commented out for verification
// This method is no longer needed because:
// 1. dt.K8sResources.OutboundIdentities is already fully populated from processK8sEgresses()
// 2. dt.NRPResources.Locations is already fully populated from fetchServiceGatewayLocations()
// 3. GetSyncLocationsAddresses() automatically computes the diff between these states
// 4. AddPod()/DeletePod() calls would modify the already-correct state unnecessarily
//
// reconcileOutboundPods reconciles pods for all outbound (NAT Gateway) services
// Calls DeletePod for NRP-only pods, AddPod for K8s-only pods
func (dt *DiffTracker) reconcileOutboundPods(nrp NRP_State, k8sNodes map[string]Node) {
	klog.Infof("reconcileOutboundPods: starting pod reconciliation")

	// Build union of all services: K8s egresses + NRP NAT Gateways
	// We need to process both to handle:
	// 1. Services in K8s (existing or being added) - update their pods
	// 2. Services in NRP but not K8s (being removed) - clear their pods to unblock deletion
	allEgressesSet := utilsets.NewString()
	for _, egr := range dt.K8sResources.Egresses.UnsortedList() {
		allEgressesSet.Insert(egr)
	}
	for _, nat := range dt.NRPResources.NATGateways.UnsortedList() {
		allEgressesSet.Insert(nat)
	}

	addCount := 0
	deleteCount := 0

	for _, serviceUID := range allEgressesSet.UnsortedList() {
		// Extract old pods from NRP (set of "location:address")
		oldPods := extractOldPodsFromNRP(serviceUID, nrp)

		// Extract new pods from K8s (map of "location:address" -> podKey)
		newPods := extractNewPodsFromK8s(serviceUID, k8sNodes)

		// Find pods to delete (in NRP but not in K8s)
		for key := range oldPods {
			if _, exists := newPods[key]; !exists {
				// Parse key back into location and address
				parts := strings.Split(key, ":")
				if len(parts) != 2 {
					klog.Warningf("reconcileOutboundPods: invalid key format %s", key)
					continue
				}
				location, address := parts[0], parts[1]
				klog.V(3).Infof("reconcileOutboundPods: calling DeletePod for %s (location=%s, address=%s)",
					serviceUID, location, address)
				// During initialization, we don't have namespace/name - pass empty strings
				// This is fine because orphaned NRP entries don't need pod finalizer tracking
				dt.DeletePod(serviceUID, location, address, "", "")
				deleteCount++
			}
		}

		// Find pods to add (in K8s but not in NRP)
		for key, podKey := range newPods {
			if _, exists := oldPods[key]; !exists {
				// Parse key back into location and address
				parts := strings.Split(key, ":")
				if len(parts) != 2 {
					klog.Warningf("reconcileOutboundPods: invalid key format %s", key)
					continue
				}
				location, address := parts[0], parts[1]
				klog.V(3).Infof("reconcileOutboundPods: calling AddPod for %s (podKey=%s, location=%s, address=%s)",
					serviceUID, podKey, location, address)
				dt.AddPod(serviceUID, podKey, location, address)
				addCount++
			}
		}
	}

	klog.Infof("reconcileOutboundPods: completed pod reconciliation (added=%d, deleted=%d)", addCount, deleteCount)
}
*/

// cleanupOrphanedPublicIPs identifies and deletes Public IPs that are not associated with any tracked service.
// This handles PIPs that were left behind when their associated LB/NAT Gateway was deleted outside the normal flow.
func (dt *DiffTracker) cleanupOrphanedPublicIPs(ctx context.Context) error {
	klog.V(3).Infof("cleanupOrphanedPublicIPs: starting orphaned PIP cleanup")

	// List all Public IPs in the resource group
	pipclient := dt.networkClientFactory.GetPublicIPAddressClient()
	pips, err := pipclient.List(ctx, dt.config.ResourceGroup)
	if err != nil {
		return fmt.Errorf("failed to list public IP addresses: %w", err)
	}

	orphanedPIPs := []string{}
	dt.mu.Lock()
	for _, pip := range pips {
		if pip.Name == nil {
			continue
		}

		pipName := *pip.Name

		// Skip PIPs that don't follow our naming convention (must end with "-pip")
		if !strings.HasSuffix(pipName, "-pip") {
			klog.V(4).Infof("cleanupOrphanedPublicIPs: skipping PIP %s (doesn't follow naming convention)", pipName)
			continue
		}

		// Skip the default NAT Gateway PIP
		if pipName == "default-natgw-v2-pip" {
			klog.V(4).Infof("cleanupOrphanedPublicIPs: skipping default NAT Gateway PIP")
			continue
		}

		// Skip PIPs that are still attached to a resource (will fail deletion with "PublicIPAddressCannotBeDeleted")
		if pip.Properties != nil && pip.Properties.IPConfiguration != nil {
			klog.V(4).Infof("cleanupOrphanedPublicIPs: skipping PIP %s (still attached to resource)", pipName)
			continue
		}

		// Extract the service name from PIP name (remove "-pip" suffix)
		serviceName := strings.TrimSuffix(pipName, "-pip")

		// Check if this PIP is associated with a tracked service
		lbExists := dt.NRPResources.LoadBalancers.Has(serviceName)
		natExists := dt.NRPResources.NATGateways.Has(serviceName)

		if !lbExists && !natExists {
			// PIP is orphaned - not associated with any tracked service
			orphanedPIPs = append(orphanedPIPs, pipName)
		}
	}
	dt.mu.Unlock()

	if len(orphanedPIPs) == 0 {
		klog.Infof("cleanupOrphanedPublicIPs: no orphaned Public IPs found")
		return nil
	}

	klog.Infof("cleanupOrphanedPublicIPs: found %d orphaned Public IPs, deleting them", len(orphanedPIPs))

	// Delete orphaned PIPs in parallel using WorkerPool
	pool := NewWorkerPool(workerPoolSize)
	var deletedCount int32
	for _, pipName := range orphanedPIPs {
		pipName := pipName // capture for closure
		pool.Submit(func() error {
			klog.V(3).Infof("cleanupOrphanedPublicIPs: deleting orphaned Public IP %s", pipName)
			if err := dt.deletePublicIP(ctx, dt.config.ResourceGroup, pipName); err != nil {
				klog.Warningf("cleanupOrphanedPublicIPs: failed to delete orphaned Public IP %s: %v", pipName, err)
				return err
			}
			atomic.AddInt32(&deletedCount, 1)
			klog.V(3).Infof("cleanupOrphanedPublicIPs: deleted orphaned Public IP %s", pipName)
			return nil
		})
	}

	if err := pool.Wait(); err != nil {
		klog.Warningf("cleanupOrphanedPublicIPs: completed with errors: %v", err)
		return err
	}

	klog.Infof("cleanupOrphanedPublicIPs: successfully deleted %d orphaned Public IPs", atomic.LoadInt32(&deletedCount))
	return nil
}
