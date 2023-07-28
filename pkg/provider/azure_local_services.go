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
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

// batchProcessor collects operations in a certain interval and then processes them in batches.
type batchProcessor interface {
	// run starts the batchProcessor, and stops if the context exits.
	run(ctx context.Context)

	// addOperation adds an operation to the batchProcessor.
	addOperation(operation batchOperation) batchOperation
}

// batchOperation is an operation that can be added to a batchProcessor.
type batchOperation interface {
	wait() batchOperationResult
}

// loadBalancerBackendPoolUpdateOperation is an operation that updates the backend pool of a load balancer.
type loadBalancerBackendPoolUpdateOperation struct {
	loadBalancerName string
	backendPoolName  string
	kind             consts.LoadBalancerBackendPoolUpdateOperation
	nodeIPs          []string
	result           chan batchOperationResult
}

func (op *loadBalancerBackendPoolUpdateOperation) wait() batchOperationResult {
	return <-op.result
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
	err := wait.PollUntilContextCancel(ctx, updater.interval, false, func(ctx context.Context) (bool, error) {
		updater.process()
		return false, nil
	})
	if err != nil {
		klog.Infof("loadBalancerBackendPoolUpdater.run: stopped due to %s", err.Error())
	}
}

// getAddIPsToBackendPoolOperation creates a new loadBalancerBackendPoolUpdateOperation
// that adds nodeIPs to the backend pool.
func getAddIPsToBackendPoolOperation(loadBalancerName, backendPoolName string, nodeIPs []string) *loadBalancerBackendPoolUpdateOperation {
	return &loadBalancerBackendPoolUpdateOperation{
		loadBalancerName: loadBalancerName,
		backendPoolName:  backendPoolName,
		kind:             consts.LoadBalancerBackendPoolUpdateOperationAdd,
		nodeIPs:          nodeIPs,
		result:           make(chan batchOperationResult),
	}
}

// getRemoveIPsFromBackendPoolOperation creates a new loadBalancerBackendPoolUpdateOperation
// that removes nodeIPs from the backend pool.
func getRemoveIPsFromBackendPoolOperation(loadBalancerName, backendPoolName string, nodeIPs []string) *loadBalancerBackendPoolUpdateOperation {
	return &loadBalancerBackendPoolUpdateOperation{
		loadBalancerName: loadBalancerName,
		backendPoolName:  backendPoolName,
		kind:             consts.LoadBalancerBackendPoolUpdateOperationRemove,
		nodeIPs:          nodeIPs,
		result:           make(chan batchOperationResult),
	}
}

// addOperation adds an operation to the loadBalancerBackendPoolUpdater.
func (updater *loadBalancerBackendPoolUpdater) addOperation(operation batchOperation) batchOperation {
	updater.lock.Lock()
	defer updater.lock.Unlock()

	updater.operations = append(updater.operations, operation)
	return operation
}

// process processes all operations in the loadBalancerBackendPoolUpdater.
// It merges operations that have the same loadBalancerName and backendPoolName,
// and then processes them in batches. If an operation fails, it will be retried
// if it is retriable, otherwise all operations in the batch targeting to
// this backend pool will fail.
func (updater *loadBalancerBackendPoolUpdater) process() {
	updater.lock.Lock()
	defer updater.lock.Unlock()

	if len(updater.operations) == 0 {
		klog.V(4).Infof("loadBalancerBackendPoolUpdater.process: no operations to process")
		return
	}

	// Group operations by loadBalancerName:backendPoolName
	groups := make(map[string][]batchOperation)
	for _, op := range updater.operations {
		lbOp := op.(*loadBalancerBackendPoolUpdateOperation)
		key := fmt.Sprintf("%s:%s", lbOp.loadBalancerName, lbOp.backendPoolName)
		groups[key] = append(groups[key], op)
	}

	// Clear all jobs.
	updater.operations = make([]batchOperation, 0)

	for key, ops := range groups {
		parts := strings.Split(key, ":")
		lbName, poolName := parts[0], parts[1]
		operationName := fmt.Sprintf("%s/%s", lbName, poolName)
		bp, rerr := updater.az.LoadBalancerClient.GetLBBackendPool(context.Background(), updater.az.ResourceGroup, lbName, poolName, "")
		if rerr != nil {
			updater.processError(rerr, operationName, ops...)
			continue
		}

		var changed bool
		for _, op := range ops {
			lbOp := op.(*loadBalancerBackendPoolUpdateOperation)
			switch lbOp.kind {
			case consts.LoadBalancerBackendPoolUpdateOperationRemove:
				changed = removeNodeIPAddressesFromBackendPool(bp, lbOp.nodeIPs, false, true)
			case consts.LoadBalancerBackendPoolUpdateOperationAdd:
				changed = updater.az.addNodeIPAddressesToBackendPool(&bp, lbOp.nodeIPs)
			default:
				panic("loadBalancerBackendPoolUpdater.process: unknown operation type")
			}
		}
		// To keep the code clean, ignore the case when `changed` is true
		// but the backend pool object is not changed after multiple times of removal and re-adding.
		if changed {
			rerr = updater.az.LoadBalancerClient.CreateOrUpdateBackendPools(context.Background(), updater.az.ResourceGroup, lbName, poolName, bp, pointer.StringDeref(bp.Etag, ""))
			if rerr != nil {
				updater.processError(rerr, operationName, ops...)
				continue
			}
		}
		notify(newBatchOperationResult(operationName, true, nil), ops...)
	}
}

// processError mark the operations as retriable if the error is retriable,
// and fail all operations if the error is not retriable.
func (updater *loadBalancerBackendPoolUpdater) processError(
	rerr *retry.Error,
	operationName string,
	operations ...batchOperation,
) {
	if rerr.Retriable {
		// Retry if retriable.
		updater.operations = append(updater.operations, operations...)
	} else {
		// Fail all operations if not retriable.
		notify(newBatchOperationResult(operationName, false, rerr.Error()), operations...)
	}
}

// notify notifies the operations with the result.
func notify(res batchOperationResult, operations ...batchOperation) {
	for _, op := range operations {
		lbOp := op.(*loadBalancerBackendPoolUpdateOperation)
		lbOp.result <- res
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
