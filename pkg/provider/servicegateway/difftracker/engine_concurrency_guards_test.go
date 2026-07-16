/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
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
		uids       = 64
		repeats    = 4
		workersPer = 8
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
					for u := 0; u < uids; u++ {
						fn(u * (idx + 1))
					}
				}
			}(i)
		}
	}

	launch(func(u int) {
		uid := fmt.Sprintf("svc-%d", u%uids)
		dt.AddService(NewInboundServiceConfig(uid, makeInboundConfig(80)))
	})
	launch(func(u int) {
		uid := fmt.Sprintf("svc-%d", u%uids)
		dt.UpdateService(NewInboundServiceConfig(uid, makeInboundConfig(8080)))
	})
	launch(func(u int) {
		uid := fmt.Sprintf("svc-%d", u%uids)
		dt.UpdateEndpoints(uid, nil, map[string]string{"10.0.0.1": "node1"})
	})
	launch(func(u int) {
		euid := fmt.Sprintf("egress-%d", u%uids)
		dt.AddService(NewOutboundServiceConfig(euid, nil))
	})
	launch(func(u int) {
		euid := fmt.Sprintf("egress-%d", u%uids)
		addr := fmt.Sprintf("10.244.0.%d", (u%250)+1)
		loc := fmt.Sprintf("10.0.0.%d", (u%200)+1)
		key := fmt.Sprintf("ns/p-%d", u)
		dt.AddPod(euid, key, loc, addr)
	})
	launch(func(u int) {
		euid := fmt.Sprintf("egress-%d", u%uids)
		addr := fmt.Sprintf("10.244.0.%d", (u%250)+1)
		loc := fmt.Sprintf("10.0.0.%d", (u%200)+1)
		dt.DeletePod(euid, loc, []string{addr}, "ns", fmt.Sprintf("p-%d", u), "")
	})
	launch(func(u int) {
		uid := fmt.Sprintf("svc-%d", u%uids)
		_ = dt.IsServiceTracked(uid)
	})
	launch(func(u int) {
		uid := fmt.Sprintf("svc-%d", u%uids)
		dt.DeleteService(uid, true, false)
	})
	launch(func(u int) {
		dt.CheckPendingServiceDeletions()
	})
	launch(func(u int) {
		uid := fmt.Sprintf("svc-%d", u%uids)
		// Should be a safe no-op for UIDs that are not currently tracked.
		dt.OnServiceCreationComplete(uid, true, nil)
	})

	close(start)
	wg.Wait()

	// Every map should remain in a self-consistent state.
	dt.mu.Lock()
	defer dt.mu.Unlock()

	for uid, op := range dt.pendingServiceOps {
		assert.NotNil(t, op, "pendingServiceOps[%s] must not be nil", uid)
		assert.Equal(t, uid, op.ServiceUID, "pendingServiceOps key/value UID mismatch")
		assert.GreaterOrEqual(t, op.RetryCount, 0, "RetryCount must be >= 0 (no negative)")
	}
	for uid, pd := range dt.pendingServiceDeletions {
		assert.NotNil(t, pd, "pendingServiceDeletions[%s] must not be nil", uid)
		// Every pending deletion MUST have a matching tracking entry (else
		// CheckPendingServiceDeletions would dereference into nothing).
		_, hasOp := dt.pendingServiceOps[uid]
		assert.True(t, hasOp, "pendingServiceDeletions entry %s has no pendingServiceOps row (orphan)", uid)
	}
	for uid, buf := range dt.pendingEndpoints {
		assert.NotNil(t, buf, "pendingEndpoints[%s] must not be nil slice", uid)
	}
	for uid, buf := range dt.pendingPods {
		assert.NotNil(t, buf, "pendingPods[%s] must not be nil slice", uid)
	}
	for key, ppd := range dt.pendingPodDeletions {
		assert.NotNil(t, ppd, "pendingPodDeletions[%s] must not be nil", key)
	}
}

// AddPod/DeletePod waves should leave no remaining references for the service.
func TestGuardConcurrency_AddPodDeletePod_RefCountSymmetry(t *testing.T) {
	dt := newTestDiffTracker()
	euid := "egress-refcount"
	dt.NRPResources.NATGateways.Insert(euid)
	dt.pendingServiceOps[euid] = &ServiceOperationState{
		ServiceUID: euid,
		Config:     NewOutboundServiceConfig(euid, nil),
		State:      StateCreated,
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
			dt.AddPod(euid, fmt.Sprintf("ns/p-%d", idx), loc, addr)
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
			dt.DeletePod(euid, loc, []string{addr}, "ns", fmt.Sprintf("p-%d", idx), "")
		}(i)
	}
	close(start2)
	wg2.Wait()

	// Any surviving nodes should have no pods referencing this egress service.
	dt.mu.Lock()
	defer dt.mu.Unlock()
	for nodeIP, node := range dt.K8sResources.Nodes {
		for podIP, pod := range node.Pods {
			assert.NotEqual(t, euid, pod.PublicOutboundIdentity,
				"node=%s pod=%s still references egress %s after symmetric delete", nodeIP, podIP, euid)
		}
	}
}

// Concurrent AddService/DeleteService should not panic and should keep valid state.
func TestGuardConcurrency_NoPanicOnDeleteServiceWhileAddService(t *testing.T) {
	dt := newTestDiffTracker()
	uid := "race-uid"

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			dt.AddService(NewInboundServiceConfig(uid, makeInboundConfig(80)))
		}()
		go func() {
			defer wg.Done()
			<-start
			dt.DeleteService(uid, true, false)
		}()
	}
	close(start)
	wg.Wait()

	// Whatever state we end up in must be legal. If tracked, the state must
	// be in the known enum range; if a pending deletion exists, the op must
	// exist too.
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if op, ok := dt.pendingServiceOps[uid]; ok {
		assert.GreaterOrEqual(t, op.State, StateNotStarted)
		assert.LessOrEqual(t, op.State, StateUpdateInProgress)
	}
	if _, hasPending := dt.pendingServiceDeletions[uid]; hasPending {
		_, hasOp := dt.pendingServiceOps[uid]
		assert.True(t, hasOp, "pendingServiceDeletions without pendingServiceOps is an invariant break")
	}
}
