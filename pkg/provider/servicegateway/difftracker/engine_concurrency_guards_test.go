/*
Copyright 2026 The Kubernetes Authors.

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

// Concurrency tests for the DiffTracker engine.

package difftracker

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Public engine methods should remain race-safe under concurrent load.
func TestGuardConcurrency_PublicMethodStorm(t *testing.T) {
	dt := newTestDiffTracker()

	const (
		serviceCount = 64
		repeats      = 4
		workersPer   = 8
	)

	var wg sync.WaitGroup
	start := make(chan struct{})

	launch := func(fn func(int)) {
		for i := 0; i < workersPer; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				for r := 0; r < repeats; r++ {
					for u := 0; u < serviceCount; u++ {
						fn(u * (idx + 1))
					}
				}
			}(i)
		}
	}

	launch(func(u int) {
		serviceName := fmt.Sprintf("svc-%d", u%serviceCount)
		assert.NoError(t, dt.AddService(NewInboundServiceConfig(serviceName, makeInboundConfig(80))))
	})
	launch(func(u int) {
		serviceName := fmt.Sprintf("svc-%d", u%serviceCount)
		assert.NoError(t, dt.UpdateService(NewInboundServiceConfig(serviceName, makeInboundConfig(8080))))
	})
	launch(func(u int) {
		serviceName := fmt.Sprintf("svc-%d", u%serviceCount)
		dt.UpdateEndpoints(serviceName, nil, map[string]string{"10.0.0.1": "node1"})
	})
	launch(func(u int) {
		egressName := fmt.Sprintf("egress-%d", u%serviceCount)
		assert.NoError(t, dt.AddService(NewOutboundServiceConfig(egressName, nil)))
	})
	launch(func(u int) {
		egressName := fmt.Sprintf("egress-%d", u%serviceCount)
		addr := fmt.Sprintf("10.244.0.%d", (u%250)+1)
		loc := fmt.Sprintf("10.0.0.%d", (u%200)+1)
		key := fmt.Sprintf("ns/p-%d", u)
		dt.AddPod(egressName, key, loc, addr)
	})
	launch(func(u int) {
		egressName := fmt.Sprintf("egress-%d", u%serviceCount)
		addr := fmt.Sprintf("10.244.0.%d", (u%250)+1)
		loc := fmt.Sprintf("10.0.0.%d", (u%200)+1)
		dt.DeletePod(egressName, loc, []string{addr}, "ns", fmt.Sprintf("p-%d", u), "")
	})
	launch(func(u int) {
		serviceName := fmt.Sprintf("svc-%d", u%serviceCount)
		_ = dt.IsServiceTracked(serviceName)
	})
	launch(func(u int) {
		serviceName := fmt.Sprintf("svc-%d", u%serviceCount)
		dt.DeleteService(serviceName, true, false)
	})
	launch(func(_ int) {
		dt.CheckPendingServiceDeletions()
	})
	launch(func(u int) {
		serviceName := fmt.Sprintf("svc-%d", u%serviceCount)
		// Should be a safe no-op for UIDs that are not currently tracked.
		dt.OnServiceCreationComplete(serviceName, true, nil)
	})

	close(start)
	wg.Wait()

	// Every map should remain in a self-consistent state.
	dt.mu.Lock()
	defer dt.mu.Unlock()

	for serviceName, op := range dt.pendingServiceOps {
		assert.NotNil(t, op, "pendingServiceOps[%s] must not be nil", serviceName)
		assert.Equal(t, serviceName, op.ServiceName, "pendingServiceOps key/value UID mismatch")
		assert.GreaterOrEqual(t, op.RetryCount, 0, "RetryCount must be >= 0 (no negative)")
	}
	for serviceName, pd := range dt.pendingServiceDeletions {
		assert.NotNil(t, pd, "pendingServiceDeletions[%s] must not be nil", serviceName)
		// Every pending deletion MUST have a matching tracking entry (else
		// CheckPendingServiceDeletions would dereference into nothing).
		_, hasOp := dt.pendingServiceOps[serviceName]
		assert.True(t, hasOp, "pendingServiceDeletions entry %s has no pendingServiceOps row (orphan)", serviceName)
	}
	for serviceName, buf := range dt.pendingEndpoints {
		assert.NotNil(t, buf, "pendingEndpoints[%s] must not be nil slice", serviceName)
	}
	for serviceName, buf := range dt.pendingPods {
		assert.NotNil(t, buf, "pendingPods[%s] must not be nil slice", serviceName)
	}
	for key, ppd := range dt.pendingPodDeletions {
		assert.NotNil(t, ppd, "pendingPodDeletions[%s] must not be nil", key)
	}
}

// AddPod/DeletePod waves should leave no remaining references for the service.
func TestGuardConcurrency_AddPodDeletePod_RefCountSymmetry(t *testing.T) {
	dt := newTestDiffTracker()
	egressName := "egress-refcount"
	dt.NRPResources.NATGateways.Insert(egressName)
	dt.pendingServiceOps[egressName] = &ServiceOperationState{
		ServiceName: egressName,
		Config:      NewOutboundServiceConfig(egressName, nil),
		State:       StateCreated,
	}

	const N = 256
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			loc := fmt.Sprintf("10.0.0.%d", (idx%50)+1)
			addr := fmt.Sprintf("10.244.0.%d", (idx%200)+1)
			dt.AddPod(egressName, fmt.Sprintf("ns/p-%d", idx), loc, addr)
		}(i)
	}
	close(start)
	wg.Wait()

	// Now wave of deletes (all unique keys)
	wg2 := sync.WaitGroup{}
	start2 := make(chan struct{})
	for i := 0; i < N; i++ {
		wg2.Add(1)
		go func(idx int) {
			defer wg2.Done()
			<-start2
			loc := fmt.Sprintf("10.0.0.%d", (idx%50)+1)
			addr := fmt.Sprintf("10.244.0.%d", (idx%200)+1)
			dt.DeletePod(egressName, loc, []string{addr}, "ns", fmt.Sprintf("p-%d", idx), "")
		}(i)
	}
	close(start2)
	wg2.Wait()

	// Any surviving nodes should have no pods referencing this egress service.
	dt.mu.Lock()
	defer dt.mu.Unlock()
	for nodeIP, node := range dt.K8sResources.Nodes {
		for podIP, pod := range node.Pods {
			assert.NotEqual(t, egressName, pod.PublicOutboundIdentity,
				"node=%s pod=%s still references egress %s after symmetric delete", nodeIP, podIP, egressName)
		}
	}

	// The loop above is vacuous on its own: removePod drops a node entry once its last pod goes,
	// so after a symmetric delete wave there are no nodes left to iterate. Assert the emptiness
	// directly, and — the point of "RefCountSymmetry" — that the outbound ref-counter came back
	// to zero. A counter that never decrements leaks the egress identity forever and keeps the
	// NAT Gateway alive after the last pod is gone.
	assert.Empty(t, dt.K8sResources.Nodes, "every node entry must be dropped once its last pod is deleted")

	_, stillCounted := dt.outboundIdentityPodRefCount.Load(egressName)
	assert.False(t, stillCounted,
		"outbound ref-count for %s must be released after an equal number of AddPod/DeletePod calls", egressName)
}

// Concurrent AddService/DeleteService should not panic and should keep valid state.
func TestGuardConcurrency_NoPanicOnDeleteServiceWhileAddService(t *testing.T) {
	dt := newTestDiffTracker()
	serviceName := "race-uid"

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			assert.NoError(t, dt.AddService(NewInboundServiceConfig(serviceName, makeInboundConfig(80))))
		}()
		go func() {
			defer wg.Done()
			<-start
			dt.DeleteService(serviceName, true, false)
		}()
	}
	close(start)
	wg.Wait()

	// Whatever state we end up in must be legal. A range check over the enum cannot fail (the
	// zero value is the first member and the last member is the maximum), so assert the
	// *reachable* set instead: an Add racing a Delete must always settle in a deletion state —
	// ending in StateCreated or StateUpdateInProgress would mean the delete was lost and the
	// Azure LB/PIP leaked.
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if op, ok := dt.pendingServiceOps[serviceName]; ok {
		assert.Contains(t,
			[]ResourceState{StateDeletionPending, StateDeletionInProgress},
			op.State,
			"an AddService/DeleteService race must settle in a deletion state, got %v", op.State)
	}
	if _, hasPending := dt.pendingServiceDeletions[serviceName]; hasPending {
		_, hasOp := dt.pendingServiceOps[serviceName]
		assert.True(t, hasOp, "pendingServiceDeletions without pendingServiceOps is an invariant break")
	}
}
