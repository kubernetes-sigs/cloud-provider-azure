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

package difftracker

import (
	"context"
	"fmt"
	"sync"

	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"

	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/trace"
)

// LoadBalancer implements the Kubernetes cloud-provider LoadBalancer interface over a DiffTracker.
type LoadBalancer struct {
	mu      sync.RWMutex
	tracker *DiffTracker
}

var _ cloudprovider.LoadBalancer = (*LoadBalancer)(nil)

// NewLoadBalancer creates a ServiceGateway LoadBalancer backed by tracker.
func NewLoadBalancer(tracker *DiffTracker) *LoadBalancer {
	return &LoadBalancer{tracker: tracker}
}

// SetTracker publishes the initialized DiffTracker used by LoadBalancer callbacks.
func (lb *LoadBalancer) SetTracker(tracker *DiffTracker) error {
	if tracker == nil {
		return fmt.Errorf("DiffTracker must not be nil")
	}

	lb.mu.Lock()
	defer lb.mu.Unlock()
	if lb.tracker != nil {
		return fmt.Errorf("DiffTracker is already initialized")
	}
	lb.tracker = tracker
	return nil
}

func (lb *LoadBalancer) diffTracker() (*DiffTracker, error) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	if lb.tracker == nil {
		return nil, fmt.Errorf("ServiceGateway LoadBalancer is not initialized")
	}
	return lb.tracker, nil
}

func (lb *LoadBalancer) GetLoadBalancer(ctx context.Context, _ string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	const operation = "GetLoadBalancer"
	ctx, span := trace.BeginReconcile(ctx, trace.DefaultTracer(), operation)
	defer func() { span.Observe(ctx, err) }()

	tracker, err := lb.diffTracker()
	if err != nil {
		return nil, false, err
	}
	if !tracker.IsServiceTracked(ServiceUID(service)) {
		return nil, false, nil
	}

	log.FromContextOrBackground(ctx).WithName(operation).V(5).Info(
		"ServiceGateway service is tracked; reporting it as existing so deletion is engine-driven",
		"service", service.Name,
	)
	return service.Status.LoadBalancer.DeepCopy(), true, nil
}

func (lb *LoadBalancer) GetLoadBalancerName(_ context.Context, _ string, service *v1.Service) string {
	return cloudprovider.DefaultLoadBalancerName(service)
}

func (lb *LoadBalancer) EnsureLoadBalancer(ctx context.Context, _ string, service *v1.Service, _ []*v1.Node) (status *v1.LoadBalancerStatus, err error) {
	const operation = "EnsureLoadBalancer"
	ctx, span := trace.BeginReconcile(ctx, trace.DefaultTracer(), operation)
	defer func() { span.Observe(ctx, err) }()

	tracker, err := lb.diffTracker()
	if err != nil {
		return nil, err
	}

	serviceName := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	metricContext := newLoadBalancerMetricContext(tracker, "ensure_loadbalancer", serviceName)
	defer func() { metricContext.ObserveOperationWithResult(err == nil) }()

	if err = tracker.ReconcileInboundService(service); err != nil {
		return nil, err
	}

	return service.Status.LoadBalancer.DeepCopy(), nil
}

func (lb *LoadBalancer) UpdateLoadBalancer(context.Context, string, *v1.Service, []*v1.Node) error {
	_, err := lb.diffTracker()
	return err
}

func (lb *LoadBalancer) EnsureLoadBalancerDeleted(ctx context.Context, _ string, service *v1.Service) (err error) {
	const operation = "EnsureLoadBalancerDeleted"
	ctx, span := trace.BeginReconcile(ctx, trace.DefaultTracer(), operation)
	defer func() { span.Observe(ctx, err) }()

	tracker, err := lb.diffTracker()
	if err != nil {
		return err
	}

	serviceName := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
	metricContext := newLoadBalancerMetricContext(tracker, "ensure_loadbalancer_deleted", serviceName)
	err = tracker.DeleteInboundService(service)
	metricContext.ObserveOperationWithResult(err == nil)
	return err
}

func newLoadBalancerMetricContext(tracker *DiffTracker, operation, serviceName string) *metrics.MetricContext {
	return metrics.NewMetricContext(
		"services",
		operation,
		tracker.config.ResourceGroup,
		tracker.config.networkResourceSubscriptionID(),
		serviceName,
	)
}
