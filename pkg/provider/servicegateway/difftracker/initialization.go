package difftracker

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
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
	logger := log.FromContextOrBackground(ctx)
	initStartTime := time.Now()
	mc := metrics.NewMetricContext("services", "InitializeFromCluster", config.ResourceGroup, config.SubscriptionID, config.ServiceGatewayResourceName)
	isOperationSucceeded := false
	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded)
	}()

	logger.V(2).Info("Started DiffTracker initialization")

	// Validate inputs
	if err := validateInitializationInputs(kubeClient, networkClientFactory); err != nil {
		return nil, err
	}

	// Build K8s state from cluster (also returns lists for reuse by recoverStuckFinalizers).
	// The node-IPs map is consumed inside buildK8sState (for family-matched location keys) and is
	// not needed again here.
	k8s, serviceList, serviceUIDToService, endpointSliceList, egressPodList, _, err := buildK8sState(ctx, kubeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to build K8s state: %w", err)
	}

	// Build NRP state from Azure (includes pipNameToIP for External IP recovery)
	// We keep currentLoadBalancersInNRP and currentNATGatewaysInNRP to identify and clean up orphaned Azure resources
	nrp, currentLoadBalancersInNRP, currentNATGatewaysInNRP, azurePIPs, pipNameToIP, err := buildNRPState(ctx, config, networkClientFactory)
	if err != nil {
		return nil, fmt.Errorf("failed to build NRP state: %w", err)
	}

	// Existence set of every Azure PIP name, independent of whether its address has been allocated
	// yet. pipNameToIP only contains PIPs with an allocated IPAddress, so it must NOT be used to
	// decide whether a PIP resource exists: a crash right after PIP creation (address still nil)
	// would otherwise make finalizer/orphan recovery treat the PIP as absent and leak it.
	pipNamesInAzure := pipNamesInAzureFromList(azurePIPs)

	// Initialize DiffTracker with computed state
	diffTracker, err := initializeDiffTrackerWithState(log.FromContextOrBackground(ctx), k8s, nrp, config, networkClientFactory, kubeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize DiffTracker: %w", err)
	}
	diffTracker.initializeEndpointSlicesCache(endpointSliceList)

	// FIX FOR ORPHANED SERVICES: We intentionally do NOT enhance NRP state with orphaned Azure resources.
	// Orphaned resources are LBs/NATs that exist in Azure but are NOT registered in ServiceGateway.
	// This happens when CCM crashes after creating Azure resources but before registering with ServiceGateway.
	// By NOT adding them to NRPResources, GetSyncOperations will compute them as "additions" and
	// reconcileServices will re-register them with ServiceGateway. The Azure resource creation is
	// idempotent (CreateOrUpdate), so the PIP and LB will be updated rather than recreated.

	// Recover resources stuck with finalizers from a previous crash
	// This must happen BEFORE informers start to avoid race conditions
	// Reuse lists from buildK8sState (avoids duplicate API calls)
	recoverStuckFinalizers(ctx, diffTracker, serviceList, egressPodList, endpointSliceList, currentLoadBalancersInNRP, currentNATGatewaysInNRP, pipNamesInAzure)

	// Counter already initialized from K8s state during buildK8sState
	// After initialization sync completes, NRP will match K8s, so counter reflects final state
	// Note: Counter is NOT updated during initialization sync - only during runtime via updateK8sPodLocked

	// Log state before computing sync operations
	logger.V(5).Info("Listed K8s services", "services", diffTracker.K8sResources.Services.Len(), "serviceUIDs", diffTracker.K8sResources.Services.UnsortedList())
	logger.V(5).Info("Listed NRP load balancers", "loadBalancers", diffTracker.NRPResources.LoadBalancers.Len(), "loadBalancerNames", diffTracker.NRPResources.LoadBalancers.UnsortedList())
	logger.V(5).Info("Listed K8s egresses", "egresses", diffTracker.K8sResources.Egresses.Len(), "egressNames", diffTracker.K8sResources.Egresses.UnsortedList())
	logger.V(5).Info("Listed NRP NAT gateways", "natGateways", diffTracker.NRPResources.NATGateways.Len(), "natGatewayNames", diffTracker.NRPResources.NATGateways.UnsortedList())

	// Get sync operations
	syncOperations := diffTracker.GetSyncOperations()
	logSyncOperations(syncOperations)

	// Setup initialization mode and start updaters
	if err := startInitialization(ctx, diffTracker); err != nil {
		return nil, err
	}

	// Reconcile services (create/delete LBs and NAT Gateways in Azure)
	diffTracker.reconcileServices(syncOperations, serviceUIDToService)

	// Schedule deletion of orphaned Azure resources via ServiceUpdater.
	// Orphaned resources are LBs/NATs/PIPs that exist in Azure but NOT in ServiceGateway.
	// This happens when services are deleted while CCM is down, or from failed operations.
	// We add them to NRPResources and call DeleteService to use the standard async deletion flow.
	scheduleOrphanedResourceDeletions(diffTracker, currentLoadBalancersInNRP, currentNATGatewaysInNRP, pipNamesInAzure)

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

	// Check if we have pending items from recoverStuckFinalizers, and whether NRP already tracks a
	// service. An existing NRP service may carry endpoint/location drift from cluster changes during
	// downtime; its sync must not hinge on a new addition's OnServiceCreationComplete callback, which
	// never fires when every addition terminal-parks (leaving the drift unsynced until an unrelated
	// future event). So trigger the initial sync whenever any service is already tracked in NRP.
	diffTracker.mu.Lock()
	hasRecoveredItems := len(diffTracker.pendingPodDeletions) > 0
	hasExistingNRPServices := diffTracker.NRPResources.LoadBalancers.Len() > 0 || diffTracker.NRPResources.NATGateways.Len() > 0
	diffTracker.mu.Unlock()

	if shouldTriggerInitialLocationSync(hasDeletions, hasOnlyExistingServices, hasRecoveredItems, hasExistingNRPServices) {
		logger.V(2).Info("Triggered initial location sync", "deletions", hasDeletions, "onlyExisting", hasOnlyExistingServices, "recoveredItems", hasRecoveredItems, "existingNRPServices", hasExistingNRPServices)
		diffTracker.triggerLocationsUpdater()
	}

	// Wait for all async operations to complete (service creations/deletions + location syncs + orphan deletions)
	// WaitForInitialSync monitors:
	//   - pendingServiceOps (ServiceUpdater work - includes orphan deletions)
	//   - pendingUpdaterTriggers (LocationsUpdater work)
	// Note: bufferedEndpoints/bufferedPods are always empty during initialization
	if err := diffTracker.WaitForInitialSync(ctx); err != nil {
		cleanupOnError(diffTracker)
		return nil, fmt.Errorf("waiting for initial sync: %w", err)
	}
	logger.V(2).Info("Completed initialization operations")

	// Recover External IPs for services that were mid-provisioning when CCM crashed.
	// This handles the case where Azure resources (PIP, LB) were created but CCM crashed
	// before updateServiceLoadBalancerStatus could patch the K8s Service with the IP.
	recoverServiceExternalIPs(ctx, diffTracker, serviceUIDToService, pipNameToIP)

	// Cleanup any remaining orphaned PIPs (PIPs without an associated LB/NAT).
	// This catches PIPs where the LB deletion succeeded but PIP deletion failed. Reuse the PIP list
	// already fetched by buildNRPState rather than re-listing: a transient failure on a second List
	// is swallowed (cleanupOrphanedPIPs is non-fatal) and would leak orphan PIPs that were already
	// visible in the fetched list.
	cleanupOrphanedPIPs(ctx, diffTracker, azurePIPs)

	// Mark initialization complete
	diffTracker.InitialSyncDone = true
	isOperationSucceeded = true
	recordInitializationDuration(initStartTime)
	logger.V(2).Info("Completed DiffTracker initialization")

	return diffTracker, nil
}

// ================================================================================================
// Initialization helper functions - broken down from InitializeFromCluster
// ================================================================================================

// shouldTriggerInitialLocationSync reports whether initialization must fire an explicit location
// sync. A new addition normally triggers the sync via its OnServiceCreationComplete callback, but
// that callback never fires when every addition terminal-parks, so an existing NRP service's
// endpoint/location drift (hasExistingNRPServices) must force the sync independently of additions.
func shouldTriggerInitialLocationSync(hasDeletions, hasOnlyExistingServices, hasRecoveredItems, hasExistingNRPServices bool) bool {
	return hasDeletions || hasOnlyExistingServices || hasRecoveredItems || hasExistingNRPServices
}

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

// buildK8sState fetches and constructs the complete K8s state (services, endpoints, egresses).
// Also returns the node-name -> all-internal-IPs map (used internally for family-matched location
// keys); callers may ignore it.
func buildK8sState(
	ctx context.Context,
	kubeClient kubernetes.Interface,
) (K8sState, *v1.ServiceList, map[string]*v1.Service, *discoveryv1.EndpointSliceList, *v1.PodList, map[string][]string, error) {
	k8s := K8sState{
		Services: utilsets.NewString(),
		Egresses: utilsets.NewString(),
		Nodes:    make(map[string]Node),
	}

	// Build node name to IPs mapping (all internal IPs per node, both families on dual-stack)
	nodeNameToIPsMap, err := buildNodeNameToIPsMap(ctx, kubeClient)
	if err != nil {
		return K8sState{}, nil, nil, nil, nil, nil, err
	}

	// Fetch and process services
	serviceList, serviceUIDToService, err := processK8sServices(ctx, kubeClient, &k8s)
	if err != nil {
		return K8sState{}, nil, nil, nil, nil, nil, err
	}

	// Fetch and process endpoint slices
	endpointSliceList, err := processK8sEndpoints(ctx, kubeClient, &k8s, nodeNameToIPsMap)
	if err != nil {
		return K8sState{}, nil, nil, nil, nil, nil, err
	}

	// Fetch and process egress pods
	egressPodList, err := processK8sEgresses(ctx, kubeClient, &k8s)
	if err != nil {
		return K8sState{}, nil, nil, nil, nil, nil, err
	}

	return k8s, serviceList, serviceUIDToService, endpointSliceList, egressPodList, nodeNameToIPsMap, nil
}

// buildNodeNameToIPsMap maps each node name to ALL of its NodeInternalIPs. On a dual-stack node
// this includes both the IPv4 and IPv6 internal IP. Keeping every family (rather than only the
// first one) lets init pick the IP-family-matched location key, mirroring the runtime EndpointSlice
// path (endpointSliceAddresses) so init and runtime agree on the location key for the
// same pod. A mismatch would orphan IPv6 (or IPv4) locations across a CCM restart.
func buildNodeNameToIPsMap(ctx context.Context, kubeClient kubernetes.Interface) (map[string][]string, error) {
	logger := log.FromContextOrBackground(ctx)
	nodeList, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}
	logger.V(2).Info("Listed Kubernetes nodes", "nodes", len(nodeList.Items))

	nodeNameToIPsMap := make(map[string][]string, len(nodeList.Items))
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		for _, addr := range n.Status.Addresses {
			if addr.Type == v1.NodeInternalIP {
				// Canonicalize so location keys match runtime (endpointSliceAddresses)
				// and NRP state; a non-canonical IPv6 InternalIP would otherwise orphan the location
				// across a restart diff.
				nodeNameToIPsMap[n.Name] = append(nodeNameToIPsMap[n.Name], canonicalIP(addr.Address))
			}
		}
	}
	logger.V(4).Info("Built node IPs map", "nodes", len(nodeNameToIPsMap))
	return nodeNameToIPsMap, nil
}

// SelectSameFamilyNodeIP returns the deterministic node location key for a pod address of the given
// family (wantIPv6) from a node's InternalIPs. It parses and canonicalizes each candidate, skipping
// malformed entries so a bad node IP never becomes a location key (which would poison the whole NRP
// batch). When a node exposes more than one InternalIP of the family, it returns the smallest
// canonical form so init and runtime - and repeated syncs - always agree on ONE location key; a
// non-deterministic pick (e.g. from an unordered cache list) would otherwise flap the location across
// reconciles and restarts. ok is false when the node has no valid InternalIP of the requested family.
func SelectSameFamilyNodeIP(nodeIPs []string, wantIPv6 bool) (string, bool) {
	best := ""
	for _, ip := range nodeIPs {
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			continue
		}
		if addr.Is6() != wantIPv6 {
			continue
		}
		if canonical := addr.String(); best == "" || canonical < best {
			best = canonical
		}
	}
	return best, best != ""
}

// nodeIPForEndpointSlice returns the node InternalIP whose IP family matches the EndpointSlice's
// AddressType, mirroring the runtime endpointSliceAddresses selection. ok is false
// for an unsupported AddressType (e.g. FQDN — not a PodIP backend address) or when the node has no
// internal IP of the required family. This keeps the init-time location key identical to the
// runtime key so the restart diff is empty for an unchanged dual-stack cluster.
func nodeIPForEndpointSlice(nodeIPs []string, addressType discoveryv1.AddressType) (string, bool) {
	switch addressType {
	case discoveryv1.AddressTypeIPv4:
		return SelectSameFamilyNodeIP(nodeIPs, false)
	case discoveryv1.AddressTypeIPv6:
		return SelectSameFamilyNodeIP(nodeIPs, true)
	default:
		// FQDN or unknown AddressType: not a PodIP backend address, skip.
		return "", false
	}
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
	services *v1.ServiceList,
	egressPods *v1.PodList,
	endpointSlices *discoveryv1.EndpointSliceList,
	currentLBsInAzure *utilsets.IgnoreCaseSet,
	currentNATsInAzure *utilsets.IgnoreCaseSet,
	azurePIPNames *utilsets.IgnoreCaseSet,
) {
	logger := log.FromContextOrBackground(ctx)

	servicesRecovered := 0
	podsRecovered := 0
	podsDirectCleaned := 0

	// Collect pending items in local maps first, then batch-insert with a single lock
	pendingPods := make(map[string]*PendingPodDeletion)

	// Recover stuck services (LoadBalancer services with our finalizer + DeletionTimestamp)
	//
	// SAFETY ANALYSIS: When is it safe to remove the finalizer directly?
	// - processK8sServices() SKIPS services with a DeletionTimestamp, so they are NOT in K8s.Services
	//   and GetSyncOperations() (Additions = K8s.Services - NRP) will never re-create their resources.
	// - So once we confirm no Azure resource currently EXISTS for the service, the finalizer is just
	//   blocking deletion and can be removed.
	// - "Exists" must be judged against actual Azure state, not just ServiceGateway registration: a
	//   crash after the PIP/LB were created but before SGW registration leaves a real resource that is
	//   absent from NRPResources. We therefore also consult the Azure LB/NAT/PIP enumeration below so
	//   the finalizer (the only anchor to those resources) is not stripped before their cleanup runs.
	//
	// Two cases for stuck services:
	// 1. A real Azure resource EXISTS (registered in NRP or found by the Azure enumeration) → leave the
	//    finalizer in place; the diff/orphan cleanup deletes the resource and then removes the finalizer.
	// 2. No Azure resource exists → directly remove the finalizer (nothing to clean up).
	servicesDirectCleaned := 0
	if services == nil {
		logger.V(4).Info("Skipped service finalizer recovery because service list was nil")
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

			// Check whether a real Azure resource exists for this service. NRPResources only reflects
			// services that completed ServiceGateway registration (Step 4 of creation), so a crash
			// after the PIP/LB were created but before registration leaves the resource present in
			// Azure yet absent from NRPResources. Also consult the actual Azure LB/NAT/PIP enumeration
			// so we do not strip the finalizer before the resource's cleanup is scheduled. PIP existence
			// is checked by name (not the allocated-IP map) so an address-less PIP still counts.
			pipExistsInAzure := azurePIPNames != nil && azurePIPNames.Has(uid+"-pip")
			hasAzureResource := dt.NRPResources.LoadBalancers.Has(uid) || dt.NRPResources.NATGateways.Has(uid) ||
				(currentLBsInAzure != nil && currentLBsInAzure.Has(uid)) ||
				(currentNATsInAzure != nil && currentNATsInAzure.Has(uid)) ||
				pipExistsInAzure

			if hasAzureResource {
				// Azure resource exists - diff mechanism will handle deletion
				logger.V(2).Info("Found stuck service finalizer", "namespace", svc.Namespace, "service", svc.Name, "uid", uid)
				servicesRecovered++
				recordFinalizerRecovered()
			} else {
				// No Azure resource - directly remove finalizer since there's nothing to clean up
				logger.V(4).Info("Removed service finalizer without Azure resource", "namespace", svc.Namespace, "service", svc.Name, "uid", uid)
				if err := dt.removeServiceGatewayFinalizer(ctx, svc); err != nil {
					logger.V(4).Info("Could not remove service finalizer", "namespace", svc.Namespace, "service", svc.Name, "err", err)
				} else {
					servicesDirectCleaned++
					recordFinalizerRecovered()
				}
			}
		}
	}

	// Recover stuck pods (egress pods with our finalizer + DeletionTimestamp)
	if egressPods == nil {
		logger.V(4).Info("Skipped pod finalizer recovery because pod list was nil")
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
				logger.V(4).Info("Removed pod finalizer with missing egress label", "namespace", pod.Namespace, "pod", pod.Name)
				if err := dt.removePodFinalizer(ctx, pod); err != nil {
					logger.V(4).Info("Could not remove pod finalizer", "namespace", pod.Namespace, "pod", pod.Name, "err", err)
				} else {
					podsDirectCleaned++
				}
				continue
			}

			// Addresses may be empty if the pod was already terminating. The drain-gated entry holds
			// the finalizer until every address leaves NRP (checked across all node locations).
			podIPs := PodEgressAddresses(pod)
			nodeIP := pod.Status.HostIP

			// If we don't have addresses, we can't track for sync through DeletePod
			// (DeletePod rejects empty location/address). Directly remove finalizer since
			// there's nothing to sync out of NRP anyway.
			if len(podIPs) == 0 || nodeIP == "" {
				logger.V(4).Info("Removed pod finalizer with missing addresses", "namespace", pod.Namespace, "pod", pod.Name, "podIPs", podIPs, "nodeIP", nodeIP)
				if err := dt.removePodFinalizer(ctx, pod); err != nil {
					logger.V(4).Info("Could not remove pod finalizer", "namespace", pod.Namespace, "pod", pod.Name, "err", err)
				} else {
					podsDirectCleaned++
				}
				continue
			}

			logger.V(2).Info("Recovered stuck pod finalizer", "namespace", pod.Namespace, "pod", pod.Name, "egress", egressLabel, "location", nodeIP, "addresses", podIPs)
			recordFinalizerRecovered()

			// Collect for batch insertion (Issue 2.4: avoid lock contention)
			// NOTE: We do NOT call DeletePod() because:
			// 1. The pod was not counted in processK8sEgresses (we skip pods with DeletionTimestamp)
			// 2. DeletePod would try to decrement a counter that doesn't include this pod
			// 3. We just need to sync the address out of NRP and remove the finalizer
			podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
			pendingPods[podKey] = &PendingPodDeletion{
				Namespace:  pod.Namespace,
				Name:       pod.Name,
				UID:        string(pod.UID),
				ServiceUID: egressLabel,
				Addresses:  podIPs,
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

	if servicesRecovered > 0 || servicesDirectCleaned > 0 || podsRecovered > 0 || podsDirectCleaned > 0 {
		logger.V(2).Info("Recovered stuck finalizers", "services", servicesRecovered, "directCleanedServices", servicesDirectCleaned, "pods", podsRecovered, "directCleanedPods", podsDirectCleaned)
	} else {
		logger.V(2).Info("Found no stuck finalizers")
	}
}

// processK8sServices fetches and processes LoadBalancer services from K8s
// NOTE: Services with DeletionTimestamp are EXCLUDED because they are being deleted.
// Their cleanup is handled by recoverStuckFinalizers.
func processK8sServices(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	k8s *K8sState,
) (*v1.ServiceList, map[string]*v1.Service, error) {
	logger := log.FromContextOrBackground(ctx)
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
				logger.V(5).Info("Skipped deleting service", "namespace", service.Namespace, "service", service.Name)
				continue
			}
			uid := ServiceUID(&services.Items[i])
			k8s.Services.Insert(uid)
			serviceUIDToService[uid] = &services.Items[i]
		}
	}
	logger.V(2).Info("Processed Kubernetes LoadBalancer services", "services", k8s.Services.Len())
	return services, serviceUIDToService, nil
}

// processK8sEndpoints fetches endpoint slices and populates K8s nodes/pods with inbound identities
// NOTE: EndpointSlices with DeletionTimestamp are EXCLUDED because they are being deleted.
// Their addresses will be synced out by recoverStuckFinalizers.
func processK8sEndpoints(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	k8s *K8sState,
	nodeNameToIPsMap map[string][]string,
) (*discoveryv1.EndpointSliceList, error) {
	logger := log.FromContextOrBackground(ctx)
	endpointSliceList, err := kubeClient.DiscoveryV1().EndpointSlices(v1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list endpointslices: %w", err)
	}

	processedCount := 0
	for _, endpointSlice := range endpointSliceList.Items {
		// Skip EndpointSlices that are being deleted - their addresses shouldn't be in K8s state
		// Their cleanup is handled by recoverStuckFinalizers
		if endpointSlice.DeletionTimestamp != nil {
			logger.V(5).Info("Skipped deleting EndpointSlice", "namespace", endpointSlice.Namespace, "endpointSlice", endpointSlice.Name)
			continue
		}

		serviceUID := extractServiceUIDFromEndpointSlice(&endpointSlice)
		if serviceUID == "" || !k8s.Services.Has(serviceUID) {
			continue
		}

		for _, endpoint := range endpointSlice.Endpoints {
			// Skip endpoints that are not ready, matching the runtime EndpointSlice informer
			// path (endpointSliceAddresses). Per the EndpointSlice API contract a
			// nil Ready condition is interpreted as "true", so only an explicit Ready=false
			// endpoint is excluded. Without this, a CCM restart imports not-ready pod IPs as LB
			// backends that the runtime diff can never remove (they were never in its snapshots),
			// leaving stale/blackhole backends until the next restart.
			if !ptr.Deref(endpoint.Conditions.Ready, true) {
				continue
			}
			if endpoint.NodeName == nil || len(endpoint.Addresses) == 0 {
				continue
			}

			// Use the node InternalIP whose family matches the EndpointSlice AddressType,
			// matching the runtime path (endpointSliceAddresses). Without this an
			// IPv6 slice would be keyed under the node's IPv4 InternalIP at init while runtime
			// keys it under the IPv6 InternalIP, orphaning the IPv6 location across a restart.
			nodeIP, ok := nodeIPForEndpointSlice(nodeNameToIPsMap[*endpoint.NodeName], endpointSlice.AddressType)
			if !ok {
				logger.V(5).Info("Skipped endpoint: no family-matched node IP or unsupported AddressType", "node", *endpoint.NodeName, "addressType", endpointSlice.AddressType)
				continue
			}

			ensureNodeExists(k8s, nodeIP)
			for _, podIP := range endpoint.Addresses {
				// Skip malformed addresses; a bad value would poison the AddressLocations payload and
				// make NRP reject the whole batch (matches endpointSliceAddresses).
				// Canonicalize the address so the key matches the runtime path and NRP state.
				addr, err := netip.ParseAddr(podIP)
				if err != nil {
					logger.V(4).Info("Skipped endpoint with malformed address", "namespace", endpointSlice.Namespace, "endpointSlice", endpointSlice.Name, "address", podIP)
					continue
				}
				addInboundIdentityToPod(k8s, nodeIP, addr.String(), serviceUID)
			}
		}
		processedCount++
	}
	logger.V(2).Info("Processed Kubernetes EndpointSlices", "processed", processedCount, "total", len(endpointSliceList.Items))
	return endpointSliceList, nil
}

// processK8sEgresses fetches egress pods and populates K8s nodes/pods with outbound identities
// NOTE: Pods with DeletionTimestamp are EXCLUDED because they are being deleted and should not
// contribute to the pod counter. Their cleanup is handled by recoverStuckFinalizers. Pods not in
// Running or Pending phase are also EXCLUDED, matching the runtime egress admission gate.
// Returns the raw pod list for reuse by recoverStuckFinalizers to avoid duplicate API calls.
func processK8sEgresses(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	k8s *K8sState,
) (*v1.PodList, error) {
	logger := log.FromContextOrBackground(ctx)
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
			logger.V(5).Info("Skipped deleting pod", "namespace", pod.Namespace, "pod", pod.Name)
			continue
		}

		// Import only Running/Pending pods, matching the runtime path (podInformerAddPod).
		// Succeeded/Failed containers have terminated and Unknown is unobtainable, so none is a live
		// egress backend; importing one would program a stale NRP address no later event clears.
		if pod.Status.Phase != v1.PodRunning && pod.Status.Phase != v1.PodPending {
			logger.V(5).Info("Skipped egress pod not in Running/Pending phase", "namespace", pod.Namespace, "pod", pod.Name, "phase", pod.Status.Phase)
			continue
		}

		egressVal := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
		if egressVal == "" || len(PodEgressAddresses(&pod)) == 0 || pod.Status.HostIP == "" || pod.Spec.NodeName == "" || !IsValidEgressIdentity(egressVal) {
			logger.V(5).Info("Skipped invalid egress pod", "namespace", pod.Namespace, "pod", pod.Name, "label", egressVal, "podIP", pod.Status.PodIP, "hostIP", pod.Status.HostIP, "node", pod.Spec.NodeName)
			continue
		}

		// Validate HostIP before it becomes the location key (matches podInformerAddPod); a malformed
		// value would make NRP reject the whole batch and stall location sync.
		if _, err := netip.ParseAddr(pod.Status.HostIP); err != nil {
			logger.V(4).Info("Skipped egress pod with malformed HostIP", "namespace", pod.Namespace, "pod", pod.Name, "hostIP", pod.Status.HostIP)
			continue
		}

		// A dual-stack pod contributes one address per IP family (Status.PodIPs); register each so the
		// secondary family egresses through the NAT Gateway too. Skip individually malformed addresses.
		var validAddrs []string
		for _, podIP := range PodEgressAddresses(&pod) {
			if _, err := netip.ParseAddr(podIP); err != nil {
				logger.V(4).Info("Skipped egress pod with malformed PodIP", "namespace", pod.Namespace, "pod", pod.Name, "podIP", podIP)
				continue
			}
			validAddrs = append(validAddrs, podIP)
		}
		if len(validAddrs) == 0 {
			continue
		}

		// Seed each address under its same-family node location (see PodNodeLocationsByFamily). Mark the
		// egress desired only once at least one address maps to a same-family location, matching the
		// runtime path (podInformerAddPod), so init never provisions a NAT Gateway with no backing pod
		// address (e.g. an IPv6 PodIP on a node exposing only an IPv4 InternalIP).
		hostByFamily := PodNodeLocationsByFamily(&pod)
		seeded := false
		for _, podIP := range validAddrs {
			nodeIP, ok := NodeLocationForAddress(hostByFamily, podIP)
			if !ok {
				logger.V(4).Info("Skipped egress pod address with no same-family node IP", "namespace", pod.Namespace, "pod", pod.Name, "podIP", podIP, "hostIPs", pod.Status.HostIPs)
				continue
			}
			ensureNodeExists(k8s, nodeIP)
			addOutboundIdentityToPod(k8s, nodeIP, podIP, egressVal)
			seeded = true
		}
		if seeded {
			k8s.Egresses.Insert(egressVal)
		}
	}
	logger.V(2).Info("Processed Kubernetes egress services", "egresses", k8s.Egresses.Len())
	return egressPods, nil
}

// buildNRPState fetches and constructs the complete NRP state (services, locations, LBs, NATs)
// Also returns:
// - azurePIPs: raw PIP slice for orphan cleanup
// - pipNameToIP: map for External IP recovery
func buildNRPState(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
) (NRPState, *utilsets.IgnoreCaseSet, *utilsets.IgnoreCaseSet, []*armnetwork.PublicIPAddress, map[string]string, error) {
	nrp := NRPState{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(),
		Locations:     make(map[string]NRPLocation),
	}

	// Fetch ServiceGateway services
	if err := fetchServiceGatewayServices(ctx, config, networkClientFactory, &nrp); err != nil {
		return NRPState{}, nil, nil, nil, nil, err
	}

	// Fetch ServiceGateway locations
	if err := fetchServiceGatewayLocations(ctx, config, networkClientFactory, &nrp); err != nil {
		return NRPState{}, nil, nil, nil, nil, err
	}

	// Fetch Azure LoadBalancers
	currentLBs, err := fetchAzureLoadBalancers(ctx, config, networkClientFactory)
	if err != nil {
		return NRPState{}, nil, nil, nil, nil, err
	}

	// Fetch Azure NAT Gateways
	currentNATs, err := fetchAzureNATGateways(ctx, config, networkClientFactory)
	if err != nil {
		return NRPState{}, nil, nil, nil, nil, err
	}

	// Fetch Azure Public IPs - both for External IP recovery (map) and orphan cleanup (raw slice).
	// This is fatal like the LB/NAT fetches above: the PIP enumeration is required to backfill a
	// crashed-mid-provisioning Service's ingress IP (recoverServiceExternalIPs is the only recovery
	// path, and it runs once at init) and to clean up orphaned PIPs. Silently continuing with an
	// empty list would permanently drop those recoveries until the next restart, so fail init and
	// let it be retried instead.
	azurePIPs, pipNameToIP, err := fetchAzurePublicIPs(ctx, config, networkClientFactory)
	if err != nil {
		return NRPState{}, nil, nil, nil, nil, fmt.Errorf("failed to fetch Azure public IPs: %w", err)
	}

	return nrp, currentLBs, currentNATs, azurePIPs, pipNameToIP, nil
}

// fetchServiceGatewayServices fetches services from ServiceGateway API
func fetchServiceGatewayServices(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
	nrp *NRPState,
) error {
	logger := log.FromContextOrBackground(ctx)
	sgwClient := networkClientFactory.GetServiceGatewayClient()
	servicesDTO, err := sgwClient.GetServices(ctx, config.ResourceGroup, config.ServiceGatewayResourceName)
	if err != nil {
		return fmt.Errorf("failed to get services from ServiceGateway API: %w", err)
	}

	for _, service := range servicesDTO {
		if service == nil || service.Properties == nil || service.Properties.ServiceType == nil || service.Name == nil {
			logger.V(5).Info("Skipped invalid ServiceGateway service")
			continue
		}

		switch *service.Properties.ServiceType {
		case "Inbound":
			nrp.LoadBalancers.Insert(*service.Name)
		case "Outbound":
			// Skip the RP-owned default outbound NAT Gateway. AKS RP provisions
			// it (name: "default-natgw", IsDefault=true) before CCM starts; if
			// CCM inserts it here, the diff vs K8s Egresses (count=0) marks it
			// for removal and the subsequent disassociate call returns
			// HTTP 400 MultipleDefaultServicesNotAllowedInServiceGateway,
			// then deletes the NAT GW + PIP and recreates them. Case-insensitive
			// because Azure may normalize naming differently across endpoints.
			if strings.EqualFold(*service.Name, "default-natgw") {
				logger.V(4).Info("Skipped RP-owned default outbound service", "service", *service.Name)
				continue
			}
			nrp.NATGateways.Insert(*service.Name)
		}
	}
	logger.V(2).Info("Fetched ServiceGateway services", "services", len(servicesDTO), "loadBalancers", nrp.LoadBalancers.Len(), "natGateways", nrp.NATGateways.Len())
	return nil
}

// fetchServiceGatewayLocations fetches address locations from ServiceGateway API
func fetchServiceGatewayLocations(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
	nrp *NRPState,
) error {
	logger := log.FromContextOrBackground(ctx)
	sgwClient := networkClientFactory.GetServiceGatewayClient()
	locationsDTO, err := sgwClient.GetAddressLocations(ctx, config.ResourceGroup, config.ServiceGatewayResourceName)
	if err != nil {
		return fmt.Errorf("failed to get locations from ServiceGateway API: %w", err)
	}
	logger.V(2).Info("Fetched ServiceGateway locations", "locations", len(locationsDTO))

	for _, location := range locationsDTO {
		if location == nil || location.AddressLocation == nil || *location.AddressLocation == "" {
			logger.V(5).Info("Skipped invalid ServiceGateway location")
			continue
		}

		addresses := parseLocationAddresses(location)
		nrp.Locations[canonicalIP(*location.AddressLocation)] = NRPLocation{Addresses: addresses}
	}
	logger.V(2).Info("Processed ServiceGateway locations", "locations", len(nrp.Locations))
	return nil
}

// fetchAzureLoadBalancers fetches LoadBalancers from Azure
func fetchAzureLoadBalancers(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
) (*utilsets.IgnoreCaseSet, error) {
	logger := log.FromContextOrBackground(ctx)
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
	logger.V(2).Info("Fetched Azure load balancers", "loadBalancers", currentLBs.Len())
	return currentLBs, nil
}

// fetchAzureNATGateways fetches NAT Gateways from Azure
func fetchAzureNATGateways(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
) (*utilsets.IgnoreCaseSet, error) {
	logger := log.FromContextOrBackground(ctx)
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
	logger.V(2).Info("Fetched Azure NAT gateways", "natGateways", currentNATs.Len())
	return currentNATs, nil
}

// fetchAzurePublicIPs fetches all Public IPs from Azure and returns:
// 1. A map of name -> IP address (for External IP recovery)
// 2. The raw slice of PIPs (for orphaned PIP cleanup)
// This is used during initialization to recover External IPs for services that were mid-provisioning
// when CCM crashed. By fetching all PIPs once, we avoid duplicate List calls.
func fetchAzurePublicIPs(
	ctx context.Context,
	config Config,
	networkClientFactory azclient.ClientFactory,
) ([]*armnetwork.PublicIPAddress, map[string]string, error) {
	logger := log.FromContextOrBackground(ctx)
	pipClient := networkClientFactory.GetPublicIPAddressClient()
	pips, err := pipClient.List(ctx, config.ResourceGroup)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list public IPs: %w", err)
	}

	pipNameToIP := make(map[string]string)
	for _, pip := range pips {
		if pip.Name != nil && pip.Properties != nil && pip.Properties.IPAddress != nil {
			pipNameToIP[strings.ToLower(*pip.Name)] = *pip.Properties.IPAddress
		}
	}
	logger.V(2).Info("Fetched Azure Public IPs", "publicIPs", len(pips), "allocatedAddresses", len(pipNameToIP))
	return pips, pipNameToIP, nil
}

// pipNamesInAzureFromList collects every Public IP name from an Azure PIP enumeration, independent of
// whether its address has been allocated. It is the resource-existence oracle for finalizer/orphan
// recovery: unlike pipNameToIP (built only from PIPs with a non-nil IPAddress), it also counts a PIP
// that was created but whose static address has not been allocated yet (a crash-window state), so
// recovery neither strips a Service finalizer nor skips an orphan PIP that actually exists.
func pipNamesInAzureFromList(pips []*armnetwork.PublicIPAddress) *utilsets.IgnoreCaseSet {
	names := utilsets.NewString()
	for _, pip := range pips {
		if pip.Name != nil {
			names.Insert(*pip.Name)
		}
	}
	return names
}

// initializeDiffTrackerWithState creates a DiffTracker and populates initial state
// initializeDiffTrackerWithState creates a DiffTracker and populates initial state.
// The outbound ref-counter (outboundIdentityPodRefCount) is seeded inside New() from the
// egress pods already present in k8s state, so no further seeding is required here.
func initializeDiffTrackerWithState(
	logger logr.Logger,
	k8s K8sState,
	nrp NRPState,
	config Config,
	networkClientFactory azclient.ClientFactory,
	kubeClient kubernetes.Interface,
) (*DiffTracker, error) {
	return New(logger, k8s, nrp, config, networkClientFactory, kubeClient)
}

// logSyncOperations logs the sync operations summary
func logSyncOperations(syncOps *SyncDiffTrackerReturnType) {
	logger := log.Background().WithName("difftracker")
	logger.V(2).Info("Computed sync operations", "loadBalancerAdditions", syncOps.LoadBalancerUpdates.Additions.Len(), "loadBalancerRemovals", syncOps.LoadBalancerUpdates.Removals.Len(), "natGatewayAdditions", syncOps.NATGatewayUpdates.Additions.Len(), "natGatewayRemovals", syncOps.NATGatewayUpdates.Removals.Len(), "locations", len(syncOps.LocationData.Locations))
}

// startInitialization sets up initialization mode and starts updaters
func startInitialization(ctx context.Context, diffTracker *DiffTracker) error {
	logger := log.FromContextOrBackground(ctx)
	diffTracker.mu.Lock()
	atomic.StoreInt32(&diffTracker.isInitializing, 1)
	diffTracker.initCompletionChecker = make(chan struct{})
	diffTracker.mu.Unlock()

	logger.V(2).Info("Started ServiceUpdater and LocationsUpdater")
	diffTracker.serviceUpdater = NewServiceUpdater(ctx, diffTracker, diffTracker.OnServiceCreationComplete, diffTracker.GetServiceUpdaterTrigger())
	diffTracker.locationsUpdater = NewLocationsUpdater(ctx, diffTracker)
	go diffTracker.serviceUpdater.Run()
	go diffTracker.locationsUpdater.Run()

	// Give updaters time to start their event loops
	time.Sleep(50 * time.Millisecond)
	return nil
}

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

// recoverServiceExternalIPs recovers External IPs for services that were mid-provisioning when CCM crashed.
// This handles the case where Azure resources (PIP, LB) were created successfully but CCM crashed
// before updateServiceLoadBalancerStatus could patch the K8s Service with the External IP.
//
// The scenario:
//  1. User creates LoadBalancer service
//  2. ServiceUpdater creates PIP and LB in Azure
//  3. CCM crashes BEFORE updateServiceLoadBalancerStatus patches the K8s Service
//  4. On restart, AddService sees the service exists in NRP and returns early
//  5. EnsureLoadBalancer returns empty status (service already exists)
//  6. patchStatus skips patching (both previous and new status are empty)
//  7. Service never gets its External IP
//
// This function fixes step 4-7 by looking up IPs from the pre-fetched PIP map.
func recoverServiceExternalIPs(ctx context.Context, diffTracker *DiffTracker, serviceUIDToService map[string]*v1.Service, pipNameToIP map[string]string) {
	logger := log.FromContextOrBackground(ctx)

	recoveredCount := 0
	checkedCount := 0

	// Find services that exist in BOTH K8s AND NRP but have empty status
	for serviceUID, svc := range serviceUIDToService {
		// Only process inbound (LoadBalancer) services
		if svc.Spec.Type != v1.ServiceTypeLoadBalancer {
			continue
		}

		// Only check services that exist in NRP
		if !diffTracker.NRPResources.LoadBalancers.Has(serviceUID) {
			continue
		}

		checkedCount++

		// Check if service already has an External IP
		if len(svc.Status.LoadBalancer.Ingress) > 0 && svc.Status.LoadBalancer.Ingress[0].IP != "" {
			logger.V(5).Info("Skipped service with existing External IP", "namespace", svc.Namespace, "service", svc.Name, "ip", svc.Status.LoadBalancer.Ingress[0].IP)
			continue
		}

		// Service exists in NRP but has no External IP in K8s - need to recover
		logger.V(2).Info("Found service missing External IP", "namespace", svc.Namespace, "service", svc.Name, "uid", serviceUID)

		// Look up IP from pre-fetched PIP map (no API call needed)
		pipName := PublicIPName(serviceUID)
		ipAddress, exists := pipNameToIP[strings.ToLower(pipName)]
		if !exists || ipAddress == "" {
			logger.V(4).Info("Could not recover service External IP", "publicIP", pipName, "serviceUID", serviceUID)
			continue
		}

		logger.V(5).Info("Found service External IP", "ip", ipAddress, "serviceUID", serviceUID)

		// Update K8s Service status with the External IP
		if err := diffTracker.updateServiceLoadBalancerStatus(ctx, serviceUID, ipAddress); err != nil {
			logger.V(4).Info("Could not update service LoadBalancer status", "namespace", svc.Namespace, "service", svc.Name, "ip", ipAddress, "err", err)
			continue
		}

		recoveredCount++
		logger.V(2).Info("Recovered service External IP", "ip", ipAddress, "namespace", svc.Namespace, "service", svc.Name)
	}

	logger.V(2).Info("Checked services for External IP recovery", "checked", checkedCount, "recovered", recoveredCount)
}

// scheduleOrphanedResourceDeletions schedules deletion of orphaned Azure resources via ServiceUpdater.
// Orphaned resources are LBs/NATs that:
//  1. Exist in Azure (currentLBsInAzure/currentNATsInAzure)
//  2. Are NOT registered in ServiceGateway (NRPResources)
//  3. Are NOT desired in Kubernetes (K8sResources)
//
// If a resource exists in K8s, reconcileServices will handle it - don't delete it!
// This uses the Engine's DeleteService flow with isOrphan=true to bypass the NRP existence check.
func scheduleOrphanedResourceDeletions(diffTracker *DiffTracker, currentLBsInAzure, currentNATsInAzure, pipNamesInAzure *utilsets.IgnoreCaseSet) {
	logger := log.Background().WithName("difftracker")

	var orphanedLBs, orphanedNATs, orphanedPIPOnly []string

	diffTracker.mu.Lock()

	// Find orphaned LBs: exist in Azure but not in ServiceGateway AND not in K8s
	if currentLBsInAzure != nil {
		for _, lbName := range currentLBsInAzure.UnsortedList() {
			// Only consider UUID-named LBs (our managed LBs have UUID names)
			if !isValidServiceUUID(lbName) {
				logger.V(5).Info("Skipped non-UUID load balancer", "loadBalancer", lbName)
				continue
			}
			// If LB is desired in K8s, reconcileServices will handle it - NOT orphaned
			if diffTracker.K8sResources.Services.Has(lbName) {
				logger.V(5).Info("Skipped Kubernetes load balancer", "loadBalancer", lbName)
				continue
			}
			// If LB exists in Azure but not in ServiceGateway AND not in K8s, it's orphaned
			if !diffTracker.NRPResources.LoadBalancers.Has(lbName) {
				orphanedLBs = append(orphanedLBs, lbName)
			}
		}
	}

	// Find orphaned NAT Gateways: exist in Azure but not in ServiceGateway AND not in K8s
	if currentNATsInAzure != nil {
		for _, natName := range currentNATsInAzure.UnsortedList() {
			// Skip the default NAT Gateway
			if natName == "default-natgw" {
				continue
			}
			// If NAT is desired in K8s, reconcileServices will handle it - NOT orphaned
			if diffTracker.K8sResources.Egresses.Has(natName) {
				logger.V(5).Info("Skipped Kubernetes NAT gateway", "natGateway", natName)
				continue
			}
			// If NAT exists in Azure but not in ServiceGateway AND not in K8s, it's orphaned
			if !diffTracker.NRPResources.NATGateways.Has(natName) {
				orphanedNATs = append(orphanedNATs, natName)
			}
		}
	}

	// Find orphaned PIP-ONLY services: a "<uid>-pip" Public IP exists in Azure with NO associated
	// LoadBalancer or NAT Gateway anywhere (NRP or Azure) and the service is not desired in K8s.
	// This is the crash-after-PIP-before-LB case: recoverStuckFinalizers KEEPS the stuck Service's
	// finalizer because the PIP exists, expecting orphan cleanup to delete the resource AND remove
	// the finalizer. cleanupOrphanedPublicIPs deletes the PIP but does NOT remove the finalizer, so
	// without scheduling a real deletion here the Service would strand in Terminating forever.
	// Routing through DeleteService(isOrphan) runs deleteInboundService, which deletes the PIP
	// (Step 4) AND removes the finalizer (Step 6); the missing LB/SGW steps are 404-safe no-ops.
	orphanLBSet := utilsets.NewString(orphanedLBs...)
	for _, pipName := range pipNamesInAzure.UnsortedList() {
		pipName = strings.ToLower(pipName)
		if !strings.HasSuffix(pipName, "-pip") || pipName == "default-natgw-pip" {
			continue
		}
		uid := strings.TrimSuffix(pipName, "-pip")
		// Only our managed UUID-named services own a "<uid>-pip".
		if !isValidServiceUUID(uid) {
			continue
		}
		// Desired in K8s (inbound or egress) → reconcileServices re-creates idempotently; not orphaned.
		if diffTracker.K8sResources.Services.Has(uid) || diffTracker.K8sResources.Egresses.Has(uid) {
			continue
		}
		// Has an LB/NAT registered in NRP or present in Azure → not PIP-only (handled elsewhere,
		// and those deletion paths delete the PIP themselves).
		if diffTracker.NRPResources.LoadBalancers.Has(uid) || diffTracker.NRPResources.NATGateways.Has(uid) {
			continue
		}
		if (currentLBsInAzure != nil && currentLBsInAzure.Has(uid)) ||
			(currentNATsInAzure != nil && currentNATsInAzure.Has(uid)) {
			continue
		}
		// Already scheduled as an orphaned LB (its deletion deletes the PIP too) → don't double-schedule.
		if orphanLBSet.Has(uid) {
			continue
		}
		orphanedPIPOnly = append(orphanedPIPOnly, uid)
	}

	diffTracker.mu.Unlock()

	totalOrphans := len(orphanedLBs) + len(orphanedNATs) + len(orphanedPIPOnly)
	if totalOrphans == 0 {
		logger.V(2).Info("Found no orphaned Azure resources")
		return
	}

	logger.V(2).Info("Found orphaned Azure resources", "loadBalancers", len(orphanedLBs), "natGateways", len(orphanedNATs), "pipOnly", len(orphanedPIPOnly))

	// Schedule orphaned LBs for deletion
	for _, lbName := range orphanedLBs {
		logger.V(5).Info("Scheduled orphaned load balancer deletion", "loadBalancer", lbName)
		diffTracker.DeleteService(lbName, true, true) // inbound, isOrphan=true
	}

	// Schedule orphaned NAT Gateways for deletion
	for _, natName := range orphanedNATs {
		logger.V(5).Info("Scheduled orphaned NAT gateway deletion", "natGateway", natName)
		diffTracker.DeleteService(natName, false, true) // outbound, isOrphan=true
	}

	// Schedule orphaned PIP-only services for deletion. Use the inbound path: deleteInboundService
	// deletes the "<uid>-pip" Public IP and removes the service finalizer, which is the anchor a
	// crash-after-PIP left stranded. (An egress service's finalizer lives on its pods, handled by
	// the pod-finalizer recovery; an inbound service's finalizer is on the Service object.)
	for _, uid := range orphanedPIPOnly {
		logger.V(5).Info("Scheduled orphaned PIP-only service deletion", "service", uid)
		diffTracker.DeleteService(uid, true, true) // inbound, isOrphan=true
	}

	logger.V(2).Info("Scheduled orphaned resource deletions", "resources", totalOrphans)
}

// cleanupOrphanedPIPs attempts to cleanup orphaned Public IPs (non-fatal)
// Uses pre-fetched PIPs from buildNRPState to avoid duplicate API calls.
func cleanupOrphanedPIPs(ctx context.Context, diffTracker *DiffTracker, azurePIPs []*armnetwork.PublicIPAddress) {
	logger := log.FromContextOrBackground(ctx)
	if err := diffTracker.cleanupOrphanedPublicIPs(ctx, azurePIPs); err != nil {
		logger.V(4).Info("Could not clean up orphaned Public IPs", "err", err)
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
func ensureNodeExists(k8s *K8sState, nodeIP string) {
	if _, exists := k8s.Nodes[nodeIP]; !exists {
		k8s.Nodes[nodeIP] = Node{Pods: make(map[string]Pod)}
	}
}

// addInboundIdentityToPod adds an inbound identity to a pod
func addInboundIdentityToPod(k8s *K8sState, nodeIP, podIP, serviceUID string) {
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
func addOutboundIdentityToPod(k8s *K8sState, nodeIP, podIP, egressVal string) {
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

// PodEgressAddresses returns a pod's egress IP addresses, preferring Status.PodIPs (which carries
// every IP family for a dual-stack pod) and falling back to the single Status.PodIP. Empty entries
// are skipped. Per the Kubernetes API, PodIPs[0] matches PodIP when both are set.
func PodEgressAddresses(pod *v1.Pod) []string {
	if len(pod.Status.PodIPs) > 0 {
		addrs := make([]string, 0, len(pod.Status.PodIPs))
		for _, podIP := range pod.Status.PodIPs {
			if podIP.IP != "" {
				addrs = append(addrs, canonicalIP(podIP.IP))
			}
		}
		if len(addrs) > 0 {
			return addrs
		}
	}
	if pod.Status.PodIP != "" {
		return []string{canonicalIP(pod.Status.PodIP)}
	}
	return nil
}

// canonicalIP returns the canonical text form of an IP (netip String: lowercase, zero-compressed for
// IPv6), or the input unchanged if it does not parse. Locations and addresses are keyed by this so
// the same IP in different representations (NRP's uppercase IPv6 vs the pod's lowercase) is a single
// key, avoiding a duplicate location/address across a CCM restart.
func canonicalIP(s string) string {
	if addr, err := netip.ParseAddr(s); err == nil {
		return addr.String()
	}
	return s
}

// PodNodeLocationsByFamily maps each IP family (false=IPv4, true=IPv6) to the node IP of that family
// from pod.Status.HostIPs. NRP registers a pod address under a node "location" and requires the
// location to be the SAME family as the address (an IPv4 location cannot hold an IPv6 address), so a
// dual-stack pod's IPv6 PodIP must be registered under the node's IPv6 IP, not its (IPv4) HostIP.
// Status.HostIPs carries every family of the node; it falls back to the single Status.HostIP when
// HostIPs is not yet populated (older kubelets / single-stack). The first entry of a family wins.
func PodNodeLocationsByFamily(pod *v1.Pod) map[bool]string {
	byFamily := make(map[bool]string, 2)
	for _, hostIP := range pod.Status.HostIPs {
		addr, err := netip.ParseAddr(hostIP.IP)
		if err != nil {
			continue
		}
		if _, ok := byFamily[addr.Is6()]; !ok {
			byFamily[addr.Is6()] = addr.String()
		}
	}
	if len(byFamily) == 0 && pod.Status.HostIP != "" {
		if addr, err := netip.ParseAddr(pod.Status.HostIP); err == nil {
			byFamily[addr.Is6()] = addr.String()
		}
	}
	return byFamily
}

// NodeLocationForAddress returns the same-family node location for a pod address from a
// PodNodeLocationsByFamily map, and whether one exists (an address whose family has no node IP
// cannot be registered).
func NodeLocationForAddress(byFamily map[bool]string, address string) (string, bool) {
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return "", false
	}
	loc, ok := byFamily[addr.Is6()]
	return loc, ok
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
		addresses[canonicalIP(address)] = NRPAddress{Services: services}
	}
	return addresses
}

// Helper functions

// WorkerPool manages a pool of worker goroutines for parallel task execution
type WorkerPool struct {
	ctx   context.Context
	tasks chan func() error
	wg    sync.WaitGroup
	mu    sync.Mutex
	err   error
}

// NewWorkerPool creates a worker pool with the given number of workers. The context
// cancels Submit's rate-limit wait and task hand-off so it stops accepting work on shutdown.
func NewWorkerPool(ctx context.Context, workers int) *WorkerPool {
	logger := log.FromContextOrBackground(ctx)
	p := &WorkerPool{
		ctx:   ctx,
		tasks: make(chan func() error),
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for task := range p.tasks {
				// Recover task panics so one failed task is recorded as a pool error
				// rather than crashing the process.
				err := func() (taskErr error) {
					defer func() {
						if r := recover(); r != nil {
							taskErr = fmt.Errorf("worker pool task panicked: %v", r)
							logger.V(4).Info("Recovered worker pool task panic", "err", r)
						}
					}()
					return task()
				}()
				if err != nil {
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

// Submit adds a task to the pool after a rate-limit delay. Both the delay and the
// hand-off are cancellable via the pool context so shutdown is not blocked.
func (p *WorkerPool) Submit(task func() error) {
	select {
	case <-p.ctx.Done():
		return
	case <-time.After(taskDelay):
	}
	select {
	case p.tasks <- task:
	case <-p.ctx.Done():
	}
}

// Wait closes the task channel and waits for all workers to complete, returning the first error encountered
func (p *WorkerPool) Wait() error {
	close(p.tasks)
	p.wg.Wait()
	return p.err
}

// ================================================================================================
// Reconciliation methods for initialization
// ================================================================================================

// reconcileServices reconciles service additions and deletions using Engine flows.
func (dt *DiffTracker) reconcileServices(syncOps *SyncDiffTrackerReturnType, serviceUIDToService map[string]*v1.Service) {
	logger := dt.logger
	logger.V(2).Info("Started service reconciliation")

	// Process deletions first (LB + NAT deletions)
	totalDeletions := syncOps.LoadBalancerUpdates.Removals.Len() + syncOps.NATGatewayUpdates.Removals.Len()
	if totalDeletions > 0 {
		logger.V(2).Info("Processed service deletions", "services", totalDeletions)

		// Call DeleteService for each service - they'll be batched by ServiceUpdater
		for _, serviceUID := range syncOps.LoadBalancerUpdates.Removals.UnsortedList() {
			logger.V(5).Info("Called DeleteService for load balancer", "serviceUID", serviceUID)
			dt.DeleteService(serviceUID, true, false) // inbound, not orphan
		}
		for _, serviceUID := range syncOps.NATGatewayUpdates.Removals.UnsortedList() {
			logger.V(5).Info("Called DeleteService for NAT gateway", "serviceUID", serviceUID)
			dt.DeleteService(serviceUID, false, false) // outbound, not orphan
		}
	}

	// Process additions
	lbAdditions := syncOps.LoadBalancerUpdates.Additions.UnsortedList()
	natAdditions := syncOps.NATGatewayUpdates.Additions.UnsortedList()
	totalAdditions := len(lbAdditions) + len(natAdditions)

	if totalAdditions > 0 {
		logger.V(2).Info("Processed service additions", "services", totalAdditions, "loadBalancers", len(lbAdditions), "natGateways", len(natAdditions))

		for _, serviceUID := range lbAdditions {
			svc, exists := serviceUIDToService[serviceUID]
			var inboundConfig *InboundConfig
			if exists && svc != nil {
				inboundConfig = ExtractInboundConfigFromService(svc)
			}
			config := NewInboundServiceConfig(serviceUID, inboundConfig)
			logger.V(5).Info("Called AddService for load balancer", "serviceUID", serviceUID)
			dt.AddService(config)
		}

		for _, serviceUID := range natAdditions {
			config := NewOutboundServiceConfig(serviceUID, nil)
			logger.V(5).Info("Called AddService for NAT gateway", "serviceUID", serviceUID)
			dt.AddService(config)
		}
	}

	logger.V(2).Info("Completed service reconciliation")
}

// cleanupOrphanedPublicIPs identifies and deletes Public IPs that are not associated with any tracked service.
// This handles PIPs that were left behind when their associated LB/NAT Gateway was deleted outside the normal flow.
// Uses pre-fetched PIPs from initialization to avoid duplicate API calls.
// If pips is nil, falls back to fetching from Azure (for non-initialization use cases).
func (dt *DiffTracker) cleanupOrphanedPublicIPs(ctx context.Context, pips []*armnetwork.PublicIPAddress) error {
	logger := dt.logger
	logger.V(2).Info("Started orphaned Public IP cleanup")

	// If PIPs not provided, fetch them (fallback for non-initialization calls)
	if pips == nil {
		pipclient := dt.networkClientFactory.GetPublicIPAddressClient()
		var err error
		pips, err = pipclient.List(ctx, dt.config.ResourceGroup)
		if err != nil {
			return fmt.Errorf("failed to list public IP addresses: %w", err)
		}
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
			logger.V(5).Info("Skipped Public IP with unexpected name", "publicIP", pipName)
			continue
		}

		// Skip the default NAT Gateway PIP
		if pipName == "default-natgw-pip" {
			logger.V(5).Info("Skipped default NAT gateway Public IP")
			continue
		}

		// Skip PIPs that are still attached to a resource (will fail deletion with "PublicIPAddressCannotBeDeleted")
		if pip.Properties != nil && pip.Properties.IPConfiguration != nil {
			logger.V(5).Info("Skipped attached Public IP", "publicIP", pipName)
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
		logger.V(2).Info("Found no orphaned Public IPs")
		return nil
	}

	logger.V(2).Info("Found orphaned Public IPs", "publicIPs", len(orphanedPIPs))

	// Delete orphaned PIPs in parallel using WorkerPool
	pool := NewWorkerPool(ctx, workerPoolSize)
	var deletedCount int32
	for _, pipName := range orphanedPIPs {
		pipName := pipName // capture for closure
		pool.Submit(func() error {
			if err := dt.deletePublicIP(ctx, dt.config.ResourceGroup, pipName); err != nil {
				return fmt.Errorf("deleting orphaned Public IP %s: %w", pipName, err)
			}
			atomic.AddInt32(&deletedCount, 1)
			logger.V(5).Info("Deleted orphaned Public IP", "publicIP", pipName)
			return nil
		})
	}

	if err := pool.Wait(); err != nil {
		return fmt.Errorf("waiting for orphaned Public IP cleanup: %w", err)
	}

	logger.V(2).Info("Deleted orphaned Public IPs", "publicIPs", atomic.LoadInt32(&deletedCount))
	return nil
}
