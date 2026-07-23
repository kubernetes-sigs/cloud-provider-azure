/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/retryrepectthrottled"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/errutils"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// batchProcessor collects operations in a certain interval and then processes them in batches.
type batchProcessor interface {
	// run starts the batchProcessor, and stops if the context exits.
	run(ctx context.Context)

	// addOperation adds an operation to the batchProcessor.
	addOperation(operation batchOperation) batchOperation

	// removeOperation removes all operations targeting to the specified service.
	removeOperation(name string)
}

// batchOperation is an operation that can be added to a batchProcessor.
type batchOperation interface {
	wait() batchOperationResult
}

// loadBalancerBackendPoolUpdateOperation is an operation that updates the backend pool of a load balancer.
type loadBalancerBackendPoolUpdateOperation struct {
	serviceName      string
	loadBalancerName string
	backendPoolName  string
	nodeIPs          []string
	retryAttempts    int
	nextEligibleAt   time.Time
}

func (op *loadBalancerBackendPoolUpdateOperation) wait() batchOperationResult {
	return batchOperationResult{}
}

// loadBalancerBackendPoolUpdater is a batchProcessor that updates the backend pool of a load balancer.
type loadBalancerBackendPoolUpdater struct {
	az         *Cloud
	interval   time.Duration
	lock       sync.Mutex
	operations []batchOperation
}

// newLoadBalancerBackendPoolUpdater creates a new loadBalancerBackendPoolUpdater.
func newLoadBalancerBackendPoolUpdater(az *Cloud, interval time.Duration) *loadBalancerBackendPoolUpdater {
	return &loadBalancerBackendPoolUpdater{
		az:         az,
		interval:   interval,
		operations: make([]batchOperation, 0),
	}
}

// run starts the loadBalancerBackendPoolUpdater, and stops if the context exits.
func (updater *loadBalancerBackendPoolUpdater) run(ctx context.Context) {
	logger := log.FromContextOrBackground(ctx).WithName("loadBalancerBackendPoolUpdater.run")
	logger.V(2).Info("started")
	err := wait.PollUntilContextCancel(ctx, updater.interval, false, func(ctx context.Context) (bool, error) {
		updater.process(ctx)
		return false, nil
	})
	logger.Error(err, "stopped")
}

// getDesiredStateOperation creates a new loadBalancerBackendPoolUpdateOperation
// representing the full desired IP set for a backend pool.
func getDesiredStateOperation(serviceName, loadBalancerName, backendPoolName string, nodeIPs []string) *loadBalancerBackendPoolUpdateOperation {
	return &loadBalancerBackendPoolUpdateOperation{
		serviceName:      serviceName,
		loadBalancerName: loadBalancerName,
		backendPoolName:  backendPoolName,
		nodeIPs:          nodeIPs,
	}
}

// addOperation adds or replaces an operation in the loadBalancerBackendPoolUpdater.
// If an operation with the same (serviceName, loadBalancerName, backendPoolName) already
// exists and the desired IPs differ, its nodeIPs are replaced and retryAttempts is reset.
// If the desired IPs are unchanged, the existing operation is left untouched to preserve
// retry state. nextEligibleAt is always preserved because ARM throttling is independent
// of desired state content.
func (updater *loadBalancerBackendPoolUpdater) addOperation(operation batchOperation) batchOperation {
	logger := log.Background().WithName("loadBalancerBackendPoolUpdater.addOperation")
	updater.lock.Lock()
	defer updater.lock.Unlock()

	op := operation.(*loadBalancerBackendPoolUpdateOperation)
	for _, existing := range updater.operations {
		existingOp := existing.(*loadBalancerBackendPoolUpdateOperation)
		if strings.EqualFold(existingOp.serviceName, op.serviceName) &&
			strings.EqualFold(existingOp.loadBalancerName, op.loadBalancerName) &&
			strings.EqualFold(existingOp.backendPoolName, op.backendPoolName) {
			// Skips if desired IPs are unchanged, don't reset retry state.
			if areNodeIPsEqual(existingOp.nodeIPs, op.nodeIPs) {
				return existing
			}
			logger.V(4).Info("Replacing operation in load balancer backend pool updater",
				"serviceName", op.serviceName,
				"loadBalancerName", op.loadBalancerName,
				"backendPoolName", op.backendPoolName,
				"nodeIPs", strings.Join(op.nodeIPs, ","))
			existingOp.nodeIPs = op.nodeIPs
			existingOp.retryAttempts = 0
			return existing
		}
	}
	logger.V(4).Info("Add operation to load balancer backend pool updater",
		"serviceName", op.serviceName,
		"loadBalancerName", op.loadBalancerName,
		"backendPoolName", op.backendPoolName,
		"nodeIPs", strings.Join(op.nodeIPs, ","))
	updater.operations = append(updater.operations, operation)
	return operation
}

// removeOperation removes all operations targeting to the specified service.
func (updater *loadBalancerBackendPoolUpdater) removeOperation(serviceName string) {
	logger := log.Background().WithName("loadBalancerBackendPoolUpdater.removeOperation")
	updater.lock.Lock()
	defer updater.lock.Unlock()

	for i := len(updater.operations) - 1; i >= 0; i-- {
		op := updater.operations[i].(*loadBalancerBackendPoolUpdateOperation)
		if strings.EqualFold(op.serviceName, serviceName) {
			logger.V(4).Info("Remove all operations targeting to the specific service",
				"serviceName", op.serviceName,
				"loadBalancerName", op.loadBalancerName,
				"backendPoolName", op.backendPoolName,
				"nodeIPs", strings.Join(op.nodeIPs, ","))
			updater.operations = append(updater.operations[:i], updater.operations[i+1:]...)
		}
	}
}

// countOperations returns the number of pending operations in the queue.
func (updater *loadBalancerBackendPoolUpdater) countOperations() int {
	updater.lock.Lock()
	defer updater.lock.Unlock()
	return len(updater.operations)
}

// drainOperations drains all pending operations from the queue and clears it.
func (updater *loadBalancerBackendPoolUpdater) drainOperations() []batchOperation {
	updater.lock.Lock()
	defer updater.lock.Unlock()

	if len(updater.operations) == 0 {
		return nil
	}

	ops := updater.operations
	updater.operations = make([]batchOperation, 0)
	return ops
}

// groupOperations filters and groups operations by loadBalancerName:backendPoolName.
// Must be called under serviceReconcileLock so that
// localServiceNameToServiceInfoMap reads are consistent.
func (updater *loadBalancerBackendPoolUpdater) groupOperations(ctx context.Context, ops []batchOperation) map[string][]batchOperation {
	logger := log.FromContextOrBackground(ctx).WithName("loadBalancerBackendPoolUpdater.groupOperations")

	groups := make(map[string][]batchOperation)
	for _, op := range ops {
		lbOp := op.(*loadBalancerBackendPoolUpdateOperation)
		si, found := updater.az.getLocalServiceInfo(strings.ToLower(lbOp.serviceName))
		if !found {
			logger.V(4).Info("service is not a local service, skip the operation", "service", lbOp.serviceName)
			continue
		}
		if !strings.EqualFold(si.lbName, lbOp.loadBalancerName) {
			logger.V(4).Info("service is not associated with the load balancer, skip the operation",
				"service", lbOp.serviceName,
				"previous load balancer", lbOp.loadBalancerName,
				"current load balancer", si.lbName)
			continue
		}

		// Check if the Service still exists in the informer. Catches the case where
		// EnsureLoadBalancerDeleted failed partway, leaving a stale map entry.
		if _, exists, err := updater.az.getLatestService(lbOp.serviceName, false); err == nil && !exists {
			logger.V(4).Info("Service not found in informer, skip the operation", "service", lbOp.serviceName)
			continue
		}

		key := fmt.Sprintf("%s:%s", lbOp.loadBalancerName, lbOp.backendPoolName)
		groups[key] = append(groups[key], op)
	}

	return groups
}

// hasParkedOperations returns true if any operation has a nextEligibleAt in the future.
func (updater *loadBalancerBackendPoolUpdater) hasParkedOperations(ops []batchOperation) bool {
	now := time.Now()
	for _, op := range ops {
		lbOp := op.(*loadBalancerBackendPoolUpdateOperation)
		if lbOp.nextEligibleAt.After(now) {
			return true
		}
	}
	return false
}

// requeueOperations prepends operations back to the front of the queue.
// Operations targeting a pool that already has a pending op in the queue are
// dropped because the queued op represents a fresher desired state.
func (updater *loadBalancerBackendPoolUpdater) requeueOperations(ops []batchOperation) {
	logger := log.Background().WithName("loadBalancerBackendPoolUpdater.requeueOperations")
	updater.lock.Lock()
	defer updater.lock.Unlock()

	var toRequeue []batchOperation
	for _, op := range ops {
		lbOp := op.(*loadBalancerBackendPoolUpdateOperation)
		stale := false
		for _, existing := range updater.operations {
			existingOp := existing.(*loadBalancerBackendPoolUpdateOperation)
			if strings.EqualFold(existingOp.serviceName, lbOp.serviceName) &&
				strings.EqualFold(existingOp.loadBalancerName, lbOp.loadBalancerName) &&
				strings.EqualFold(existingOp.backendPoolName, lbOp.backendPoolName) {
				// Transfer the later nextEligibleAt to respect ARM throttling.
				if lbOp.nextEligibleAt.After(existingOp.nextEligibleAt) {
					existingOp.nextEligibleAt = lbOp.nextEligibleAt
				}
				stale = true
				break
			}
		}
		if stale {
			logger.V(4).Info("Skipping stale requeue, fresher op already queued",
				"serviceName", lbOp.serviceName,
				"loadBalancerName", lbOp.loadBalancerName,
				"backendPoolName", lbOp.backendPoolName)
			continue
		}
		toRequeue = append(toRequeue, op)
	}
	updater.operations = append(toRequeue, updater.operations...)
}

// process processes all operations in the loadBalancerBackendPoolUpdater.
// It merges operations that have the same loadBalancerName and backendPoolName,
// and then processes them in batches. If an operation fails, it will be retried
// if it is retriable, otherwise all operations in the batch targeting to
// this backend pool will fail.
func (updater *loadBalancerBackendPoolUpdater) process(ctx context.Context) {
	logger := log.FromContextOrBackground(ctx).WithName("loadBalancerBackendPoolUpdater.process")

	// Acquire serviceReconcileLock before draining operations so that
	// removeOperation can cancel queued operations and localServiceNameToServiceInfoMap
	// reads in groupOperations are consistent. The lock ordering
	// (serviceReconcileLock, azureResourceLocker, updater.lock) matches
	// the main reconciliation loop.
	updater.az.serviceReconcileLock.Lock()
	defer updater.az.serviceReconcileLock.Unlock()

	if updater.countOperations() == 0 {
		return
	}

	// Serialize with other components that may update Azure load balancer resources.
	if updater.az.azureResourceLocker != nil {
		if err := updater.az.azureResourceLocker.Lock(ctx); err != nil {
			return
		}
		defer func() { _ = updater.az.azureResourceLocker.Unlock(ctx) }()
	}

	ops := updater.drainOperations()
	groups := updater.groupOperations(ctx, ops)

	for key, ops := range groups {
		if ctx.Err() != nil {
			break
		}

		parts := strings.Split(key, ":")
		lbName, poolName := parts[0], parts[1]
		operationName := fmt.Sprintf("%s/%s", lbName, poolName)

		// If any op in the group has nextEligibleAt in the future,
		// requeue the entire group without making ARM calls or emitting events.
		if updater.hasParkedOperations(ops) {
			updater.requeueOperations(ops)
			continue
		}

		mc := metrics.NewMetricContext(
			"services_local",      // prefix name, differ from "services" in main reconciliation loop
			"update_backend_pool", // request name
			updater.az.ResourceGroup,
			updater.az.getNetworkResourceSubscriptionID(),
			"local_service_backend_pool_updater", // source name, use a constant source name for aggregation
		)

		bp, err := updater.az.NetworkClientFactory.GetBackendAddressPoolClient().Get(ctx, updater.az.ResourceGroup, lbName, poolName)
		if err != nil {
			updater.handleGroupError(ctx, mc, err, operationName, ops)
			continue
		}

		var changed bool
		// Compute desired IPs from all ops in this group.
		desiredIPs := sets.New[string]()
		for _, op := range ops {
			lbOp := op.(*loadBalancerBackendPoolUpdateOperation)
			desiredIPs.Insert(lbOp.nodeIPs...)
		}

		// Compare with current pool IPs.
		currentIPs := sets.New[string]()
		for _, addr := range bp.Properties.LoadBalancerBackendAddresses {
			if addr.Properties != nil && addr.Properties.IPAddress != nil {
				currentIPs.Insert(*addr.Properties.IPAddress)
			}
		}

		// Remove IPs not in desired set, add IPs not in current pool.
		ipsToRemove := currentIPs.Difference(desiredIPs).UnsortedList()
		ipsToAdd := desiredIPs.Difference(currentIPs).UnsortedList()
		// Sort for stable test assertions; order is not significant for ARM.
		slices.Sort(ipsToAdd)

		if len(ipsToRemove) > 0 {
			changed = removeNodeIPAddressesFromBackendPool(bp, ipsToRemove, false, true, true)
		}
		if len(ipsToAdd) > 0 {
			added := updater.az.addNodeIPAddressesToBackendPool(bp, ipsToAdd)
			changed = changed || added
		}

		if changed {
			logger.V(2).Info("updating backend pool", "loadBalancer", lbName, "backendPool", poolName)
			_, err = updater.az.NetworkClientFactory.GetBackendAddressPoolClient().CreateOrUpdate(ctx, updater.az.ResourceGroup, lbName, poolName, *bp)
			if err != nil {
				updater.handleGroupError(ctx, mc, err, operationName, ops)
				continue
			}
			mc.ObserveOperationWithResult(true)
			updater.notify(ctx, v1.EventTypeNormal, consts.LoadBalancerBackendPoolUpdated, "Load balancer backend pool updated successfully", ops...)
		}
	}
}

type updaterErrorClass int

const (
	errClassStale updaterErrorClass = iota
	errClassRetriable
	errClassSDKExhausted
	errClassTerminal
)

// classifyUpdaterError classifies an error for retry decisions.
func classifyUpdaterError(err error) updaterErrorClass {
	if exists, checkErr := errutils.CheckResourceExistsFromAzcoreError(err); !exists && checkErr == nil {
		return errClassStale
	}

	var throttleErr *retryrepectthrottled.ThrottleError
	if errors.As(err, &throttleErr) {
		return errClassRetriable
	}

	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		if respErr.StatusCode == http.StatusConflict || respErr.StatusCode == http.StatusPreconditionFailed {
			return errClassRetriable
		}
		// SDK-retriable statuses are terminal because the SDK already retried.
		if slices.Contains(retryrepectthrottled.GetRetriableStatusCode(), respErr.StatusCode) {
			return errClassSDKExhausted
		}
	}

	return errClassTerminal
}

// handleGroupError classifies the error and either drops, retries, or fails the group.
// For user-facing documentation of retry behavior, see
// https://cloud-provider-azure.sigs.k8s.io/topics/multislb/#retry-behavior
func (updater *loadBalancerBackendPoolUpdater) handleGroupError(ctx context.Context, mc *metrics.MetricContext, err error, operationName string, ops []batchOperation) {
	logger := log.FromContextOrBackground(ctx).WithName("loadBalancerBackendPoolUpdater.handleGroupError")

	// On shutdown, drop silently.
	if ctx.Err() != nil {
		logger.V(4).Info("Context canceled, dropping operations", "operation", operationName, "ops", len(ops))
		return
	}

	class := classifyUpdaterError(err)
	switch class {
	case errClassStale:
		logger.V(4).Info("Backend pool not found, skipped update", "operation", operationName)

	case errClassSDKExhausted:
		mc.ObserveOperationWithResult(false)
		updater.notify(ctx, v1.EventTypeWarning, consts.LoadBalancerBackendPoolUpdateFailed,
			fmt.Sprintf("Backend pool update failed (SDK retries exhausted): %v.", err), ops...)

	case errClassTerminal:
		mc.ObserveOperationWithResult(false)
		updater.notify(ctx, v1.EventTypeWarning, consts.LoadBalancerBackendPoolUpdateFailed,
			fmt.Sprintf("Backend pool update failed (non-retriable): %v.", err), ops...)

	case errClassRetriable:
		var throttleErr *retryrepectthrottled.ThrottleError
		errors.As(err, &throttleErr)

		var opsToRequeue []batchOperation
		var exhaustedOps []batchOperation
		for _, op := range ops {
			lbOp := op.(*loadBalancerBackendPoolUpdateOperation)

			// Re-check relevance before requeue.
			si, found := updater.az.getLocalServiceInfo(strings.ToLower(lbOp.serviceName))
			if !found || !strings.EqualFold(si.lbName, lbOp.loadBalancerName) {
				logger.V(4).Info("Operation is stale before requeue, dropped",
					"service", lbOp.serviceName, "loadBalancer", lbOp.loadBalancerName)
				continue
			}

			if lbOp.retryAttempts >= *updater.az.LoadBalancerBackendPoolUpdateMaxRetries {
				exhaustedOps = append(exhaustedOps, op)
			} else {
				lbOp.retryAttempts++
				if throttleErr != nil && throttleErr.RetryAfter.After(time.Now()) {
					lbOp.nextEligibleAt = throttleErr.RetryAfter
				}
				opsToRequeue = append(opsToRequeue, op)
			}
		}

		if len(exhaustedOps) > 0 {
			mc.ObserveOperationWithResult(false)
			updater.notify(ctx, v1.EventTypeWarning, consts.LoadBalancerBackendPoolUpdateFailed,
				fmt.Sprintf("Backend pool update failed after %d retries: %v. To retrigger, cause a reconciliation for the Service (e.g., add or update any annotation on the Service).", *updater.az.LoadBalancerBackendPoolUpdateMaxRetries, err), exhaustedOps...)
		}

		if len(opsToRequeue) > 0 {
			updater.notify(ctx, v1.EventTypeWarning, consts.LoadBalancerBackendPoolUpdateRetrying,
				fmt.Sprintf("Backend pool update failed, will retry: %v.", err), opsToRequeue...)
			updater.requeueOperations(opsToRequeue)
		}
	}
}

// notify emits an event for each distinct Service among the provided operations.
func (updater *loadBalancerBackendPoolUpdater) notify(ctx context.Context, eventType, reason, msg string, operations ...batchOperation) {
	logger := log.FromContextOrBackground(ctx).WithName("loadBalancerBackendPoolUpdater.notify")
	seen := make(map[string]struct{})
	for _, op := range operations {
		lbOp := op.(*loadBalancerBackendPoolUpdateOperation)
		key := strings.ToLower(lbOp.serviceName)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		svc, _, _ := updater.az.getLatestService(lbOp.serviceName, false)
		if svc == nil {
			logger.V(4).Info("Service not found, skipped event", "service", lbOp.serviceName)
			continue
		}
		updater.az.Event(svc, eventType, reason, msg)
	}
}

// batchOperationResult is the result of a batch operation.
type batchOperationResult struct {
	name    string
	success bool
	err     error
}

// newBatchOperationResult creates a new batchOperationResult.
func newBatchOperationResult(name string, success bool, err error) batchOperationResult {
	return batchOperationResult{
		name:    name,
		success: success,
		err:     err,
	}
}

func (az *Cloud) getLocalServiceInfo(serviceName string) (*serviceInfo, bool) {
	data, ok := az.localServiceNameToServiceInfoMap.Load(serviceName)
	if !ok {
		return &serviceInfo{}, false
	}
	return data.(*serviceInfo), true
}

// setUpEndpointSlicesInformer creates an informer for EndpointSlices of local services.
// It watches the update events and send backend pool update operations to the batch updater.
func (az *Cloud) setUpEndpointSlicesInformer(informerFactory informers.SharedInformerFactory) {
	logger := log.Background().WithName("setUpEndpointSlicesInformer")
	endpointSlicesInformer := informerFactory.Discovery().V1().EndpointSlices().Informer()
	_, _ = endpointSlicesInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				es := obj.(*discovery_v1.EndpointSlice)
				az.endpointSlicesCache.Store(strings.ToLower(fmt.Sprintf("%s/%s", es.Namespace, es.Name)), es)
			},
			UpdateFunc: func(_, newObj interface{}) {
				newES := newObj.(*discovery_v1.EndpointSlice)

				svcName := getServiceNameOfEndpointSlice(newES)
				if svcName == "" {
					logger.V(4).Info("EndpointSlice does not have service name label, skip updating load balancer backend pool", "namespace", newES.Namespace, "name", newES.Name)
					return
				}

				logger.V(4).Info("Detecting EndpointSlice update", "namespace", newES.Namespace, "name", newES.Name)
				az.endpointSlicesCache.Store(strings.ToLower(fmt.Sprintf("%s/%s", newES.Namespace, newES.Name)), newES)

				key := strings.ToLower(fmt.Sprintf("%s/%s", newES.Namespace, svcName))
				si, found := az.getLocalServiceInfo(key)
				if !found {
					logger.V(4).Info("EndpointSlice belongs to service, but the service is not a local service, or has not finished the initial reconciliation loop. Skip updating load balancer backend pool", "namespace", newES.Namespace, "name", newES.Name, "service", key)
					return
				}
				lbName, ipFamily := si.lbName, si.ipFamily

				// Compute full desired IP set from all cached EndpointSlices for this service.
				allNodeNames := utilsets.NewString()
				az.endpointSlicesCache.Range(func(_, value interface{}) bool {
					eps := value.(*discovery_v1.EndpointSlice)
					if strings.EqualFold(getServiceNameOfEndpointSlice(eps), svcName) &&
						strings.EqualFold(eps.Namespace, newES.Namespace) {
						for _, ep := range eps.Endpoints {
							allNodeNames.Insert(ptr.Deref(ep.NodeName, ""))
						}
					}
					return true
				})

				allIPs := make([]string, 0)
				for _, nodeName := range allNodeNames.UnsortedList() {
					nodeIPsSet := az.nodePrivateIPs[strings.ToLower(nodeName)]
					if nodeIPsSet != nil {
						allIPs = append(allIPs, nodeIPsSet.UnsortedList()...)
					}
				}

				if az.backendPoolUpdater != nil {
					bpNameIPv4 := getLocalServiceBackendPoolName(key, false)
					bpNameIPv6 := getLocalServiceBackendPoolName(key, true)
					currentIPsInBackendPools := make(map[string][]string)
					switch strings.ToLower(ipFamily) {
					case strings.ToLower(consts.IPVersionIPv4String):
						currentIPsInBackendPools[bpNameIPv4] = nil
					case strings.ToLower(consts.IPVersionIPv6String):
						currentIPsInBackendPools[bpNameIPv6] = nil
					default:
						currentIPsInBackendPools[bpNameIPv4] = nil
						currentIPsInBackendPools[bpNameIPv6] = nil
					}
					az.applyIPChangesAmongLocalServiceBackendPoolsByIPFamily(lbName, key, currentIPsInBackendPools, allIPs)
				}
			},
			DeleteFunc: func(obj interface{}) {
				var es *discovery_v1.EndpointSlice
				switch v := obj.(type) {
				case *discovery_v1.EndpointSlice:
					es = v
				case cache.DeletedFinalStateUnknown:
					// We may miss the deletion event if the watch stream is disconnected and the object is deleted.
					var ok bool
					es, ok = v.Obj.(*discovery_v1.EndpointSlice)
					if !ok {
						logger.Error(nil, "Cannot convert to *discovery_v1.EndpointSlice", "obj", v.Obj)
						return
					}
				default:
					logger.Error(nil, "Cannot convert to *discovery_v1.EndpointSlice", "obj.(type)", v)
					return
				}

				az.endpointSlicesCache.Delete(strings.ToLower(fmt.Sprintf("%s/%s", es.Namespace, es.Name)))
			},
		})
}

// getServiceNameOfEndpointSlice gets the service name of an EndpointSlice.
func getServiceNameOfEndpointSlice(es *discovery_v1.EndpointSlice) string {
	if es.Labels != nil {
		return es.Labels[consts.ServiceNameLabel]
	}
	return ""
}

// areNodeIPsEqual returns true if the two IP slices contain the same set of IPs.
func areNodeIPsEqual(currentIPs, expectedIPs []string) bool {
	return sets.NewString(currentIPs...).Equal(sets.NewString(expectedIPs...))
}

// getLocalServiceBackendPoolName gets the name of the backend pool of a local service.
func getLocalServiceBackendPoolName(serviceName string, ipv6 bool) string {
	serviceName = strings.ToLower(strings.ReplaceAll(serviceName, "/", "-"))
	if ipv6 {
		return fmt.Sprintf("%s-%s", serviceName, consts.IPVersionIPv6StringLower)
	}
	return serviceName
}

// getBackendPoolNameForService determine the expected backend pool name
// by checking the external traffic policy of the service.
func (az *Cloud) getBackendPoolNameForService(service *v1.Service, clusterName string, ipv6 bool) string {
	if !isLocalService(service) || !az.UseMultipleStandardLoadBalancers() {
		return getBackendPoolName(clusterName, ipv6)
	}
	return getLocalServiceBackendPoolName(getServiceName(service), ipv6)
}

// getBackendPoolNamesForService determine the expected backend pool names
// by checking the external traffic policy of the service.
func (az *Cloud) getBackendPoolNamesForService(service *v1.Service, clusterName string) map[bool]string {
	if !isLocalService(service) || !az.UseMultipleStandardLoadBalancers() {
		return getBackendPoolNames(clusterName)
	}
	return map[bool]string{
		consts.IPVersionIPv4: getLocalServiceBackendPoolName(getServiceName(service), false),
		consts.IPVersionIPv6: getLocalServiceBackendPoolName(getServiceName(service), true),
	}
}

// getBackendPoolIDsForService determine the expected backend pool IDs
// by checking the external traffic policy of the service.
func (az *Cloud) getBackendPoolIDsForService(service *v1.Service, clusterName, lbName string) map[bool]string {
	if !isLocalService(service) || !az.UseMultipleStandardLoadBalancers() {
		return az.getBackendPoolIDs(clusterName, lbName)
	}
	return map[bool]string{
		consts.IPVersionIPv4: az.getLocalServiceBackendPoolID(getServiceName(service), lbName, false),
		consts.IPVersionIPv6: az.getLocalServiceBackendPoolID(getServiceName(service), lbName, true),
	}
}

// getLocalServiceBackendPoolID gets the ID of the backend pool of a local service.
func (az *Cloud) getLocalServiceBackendPoolID(serviceName string, lbName string, ipv6 bool) string {
	return az.getBackendPoolID(lbName, getLocalServiceBackendPoolName(serviceName, ipv6))
}

// localServiceOwnsBackendPool checks if a backend pool is owned by a local service.
func localServiceOwnsBackendPool(serviceName, bpName string) bool {
	if strings.HasSuffix(strings.ToLower(bpName), consts.IPVersionIPv6StringLower) {
		return strings.EqualFold(getLocalServiceBackendPoolName(serviceName, true), bpName)
	}
	return strings.EqualFold(getLocalServiceBackendPoolName(serviceName, false), bpName)
}

type serviceInfo struct {
	ipFamily string
	lbName   string
}

func newServiceInfo(ipFamily, lbName string) *serviceInfo {
	return &serviceInfo{
		ipFamily: ipFamily,
		lbName:   lbName,
	}
}

// getLocalServiceEndpointsNodeNames gets the node names that host all endpoints of the local service.
func (az *Cloud) getLocalServiceEndpointsNodeNames(service *v1.Service) *utilsets.IgnoreCaseSet {
	logger := log.Background().WithName("getLocalServiceEndpointsNodeNames")
	var eps []*discovery_v1.EndpointSlice
	az.endpointSlicesCache.Range(func(_, value interface{}) bool {
		endpointSlice := value.(*discovery_v1.EndpointSlice)
		if strings.EqualFold(getServiceNameOfEndpointSlice(endpointSlice), service.Name) &&
			strings.EqualFold(endpointSlice.Namespace, service.Namespace) {
			eps = append(eps, endpointSlice)
		}
		return true
	})
	if len(eps) == 0 {
		klog.Warningf("getLocalServiceEndpointsNodeNames: failed to find EndpointSlice for service %s/%s", service.Namespace, service.Name)
		return nil
	}

	var nodeNames []string
	for _, ep := range eps {
		for _, endpoint := range ep.Endpoints {
			logger.V(4).Info("EndpointSlice has endpoint on node", "namespace", ep.Namespace, "name", ep.Name, "addresses", endpoint.Addresses, "nodeName", ptr.Deref(endpoint.NodeName, ""))
			nodeNames = append(nodeNames, ptr.Deref(endpoint.NodeName, ""))
		}
	}

	return utilsets.NewString(nodeNames...)
}

// cleanupLocalServiceBackendPool cleans up the backend pool of
// a local service among given load balancers.
func (az *Cloud) cleanupLocalServiceBackendPool(
	ctx context.Context,
	svc *v1.Service,
	nodes []*v1.Node,
	lbs []*armnetwork.LoadBalancer,
	clusterName string,
) (newLBs []*armnetwork.LoadBalancer, err error) {
	logger := log.FromContextOrBackground(ctx).WithName("cleanupLocalServiceBackendPool")
	var changed bool
	for _, lb := range lbs {
		lbName := ptr.Deref(lb.Name, "")
		if lb.Properties.BackendAddressPools != nil {
			for _, bp := range lb.Properties.BackendAddressPools {
				bpName := ptr.Deref(bp.Name, "")
				if localServiceOwnsBackendPool(getServiceName(svc), bpName) {
					if err := az.DeleteLBBackendPool(ctx, lbName, bpName); err != nil {
						return nil, err
					}
					changed = true
				}
			}
		}
	}

	if changed {
		// Refresh the list of existing LBs after cleanup to update etags for the LBs.
		logger.V(4).Info("Refreshing the list of existing LBs")
		lbs, err = az.ListManagedLBs(ctx, svc, nodes, clusterName)
		if err != nil {
			return nil, fmt.Errorf("reconcileLoadBalancer: failed to list managed LB: %w", err)
		}
	}
	return lbs, nil
}

// checkAndApplyLocalServiceBackendPoolUpdates if the IPs in the backend pool are aligned
// with the corresponding endpointslice, and update the backend pool if necessary.
func (az *Cloud) checkAndApplyLocalServiceBackendPoolUpdates(lb armnetwork.LoadBalancer, service *v1.Service) error {
	serviceName := getServiceName(service)
	endpointsNodeNames := az.getLocalServiceEndpointsNodeNames(service)
	if endpointsNodeNames == nil {
		return nil
	}

	var expectedIPs []string
	for _, nodeName := range endpointsNodeNames.UnsortedList() {
		ips := az.nodePrivateIPs[strings.ToLower(nodeName)]
		expectedIPs = append(expectedIPs, ips.UnsortedList()...)
	}
	currentIPsInBackendPools := make(map[string][]string)
	for _, bp := range lb.Properties.BackendAddressPools {
		bpName := ptr.Deref(bp.Name, "")
		if localServiceOwnsBackendPool(serviceName, bpName) {
			currentIPs := make([]string, 0)
			for _, address := range bp.Properties.LoadBalancerBackendAddresses {
				if address.Properties != nil && address.Properties.IPAddress != nil {
					currentIPs = append(currentIPs, *address.Properties.IPAddress)
				}
			}
			currentIPsInBackendPools[bpName] = currentIPs
		}
	}
	az.applyIPChangesAmongLocalServiceBackendPoolsByIPFamily(*lb.Name, serviceName, currentIPsInBackendPools, expectedIPs)

	return nil
}

// applyIPChangesAmongLocalServiceBackendPoolsByIPFamily reconciles IPs by IP family
// amone the backend pools of a local service.
func (az *Cloud) applyIPChangesAmongLocalServiceBackendPoolsByIPFamily(
	lbName, serviceName string,
	currentIPsInBackendPools map[string][]string,
	expectedIPs []string,
) {
	currentIPsInBackendPoolsIPv4 := make(map[string][]string)
	currentIPsInBackendPoolsIPv6 := make(map[string][]string)
	for bpName, ips := range currentIPsInBackendPools {
		if managedResourceHasIPv6Suffix(bpName) {
			currentIPsInBackendPoolsIPv6[bpName] = ips
		} else {
			currentIPsInBackendPoolsIPv4[bpName] = ips
		}
	}

	ipv4 := make([]string, 0)
	ipv6 := make([]string, 0)
	for _, ip := range expectedIPs {
		if utilnet.IsIPv6String(ip) {
			ipv6 = append(ipv6, ip)
		} else {
			ipv4 = append(ipv4, ip)
		}
	}
	az.reconcileIPsInLocalServiceBackendPoolsAsync(lbName, serviceName, currentIPsInBackendPoolsIPv6, ipv6)
	az.reconcileIPsInLocalServiceBackendPoolsAsync(lbName, serviceName, currentIPsInBackendPoolsIPv4, ipv4)
}

// reconcileIPsInLocalServiceBackendPoolsAsync reconciles IPs in the backend pools of a local service.
func (az *Cloud) reconcileIPsInLocalServiceBackendPoolsAsync(
	lbName, serviceName string,
	currentIPsInBackendPools map[string][]string,
	expectedIPs []string,
) {
	logger := log.Background().WithName("reconcileIPsInLocalServiceBackendPoolsAsync")
	for bpName, currentIPs := range currentIPsInBackendPools {
		if currentIPs != nil && areNodeIPsEqual(currentIPs, expectedIPs) {
			logger.V(4).Info("No IP change detected for service, skip updating load balancer backend pool", "service", serviceName)
			return
		}
		az.backendPoolUpdater.addOperation(getDesiredStateOperation(serviceName, lbName, bpName, expectedIPs))
	}
}
