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

package servicegateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/servicegateway/difftracker"
)

func TestDiffTrackerConfig(t *testing.T) {
	config := providerconfig.Config{}
	config.SubscriptionID = "compute-subscription"
	config.NetworkResourceSubscriptionID = "network-subscription"
	config.ResourceGroup = "resource-group"
	config.Location = "eastus"
	config.VnetName = "vnet"
	config.VnetResourceGroup = "vnet-resource-group"

	diffTrackerConfig := diffTrackerConfig(config)

	assert.Equal(t, "compute-subscription", diffTrackerConfig.SubscriptionID)
	assert.Equal(t, "network-subscription", diffTrackerConfig.NetworkResourceSubscriptionID)
	assert.Equal(t, "resource-group", diffTrackerConfig.ResourceGroup)
	assert.Equal(t, "eastus", diffTrackerConfig.Location)
	assert.Equal(t, "vnet", diffTrackerConfig.VNetName)
	assert.Equal(t, "vnet-resource-group", diffTrackerConfig.VNetResourceGroup)
	assert.Contains(t, diffTrackerConfig.ServiceGatewayID, "/subscriptions/network-subscription/")
}

func TestRuntimeEnablementAndLoadBalancer(t *testing.T) {
	config := providerconfig.Config{}
	runtime := NewRuntime(config, nil, nil)

	assert.False(t, runtime.Enabled())
	loadBalancer, supported := runtime.LoadBalancer()
	assert.False(t, supported)
	assert.Nil(t, loadBalancer)

	config.ServiceGatewayEnabled = true
	runtime = NewRuntime(config, nil, nil)

	assert.True(t, runtime.Enabled())
	first, supported := runtime.LoadBalancer()
	assert.True(t, supported)
	assert.NotNil(t, first)
	second, supported := runtime.LoadBalancer()
	assert.True(t, supported)
	assert.Same(t, first, second)
}

func TestRuntimeStartRequiresDependencies(t *testing.T) {
	config := providerconfig.Config{ServiceGatewayEnabled: true}
	runtime := NewRuntime(config, nil, nil)

	err := runtime.Start(context.Background(), nil)

	assert.EqualError(t, err, "ServiceGateway runtime requires a shared informer factory")
}

func TestRuntimeAcceptsDeferredKubeClient(t *testing.T) {
	config := providerconfig.Config{ServiceGatewayEnabled: true}
	runtime := NewRuntime(config, nil, nil)
	kubeClient := fake.NewSimpleClientset()

	runtime.SetKubeClient(kubeClient)

	assert.Same(t, kubeClient, runtime.kubeClient)
}

func TestRuntimeStart(t *testing.T) {
	originalInitialize := initializeRuntimeDiffTracker
	originalStartPodInformer := startRuntimePodInformer
	t.Cleanup(func() {
		initializeRuntimeDiffTracker = originalInitialize
		startRuntimePodInformer = originalStartPodInformer
	})

	var runtimeCtx context.Context
	tracker := &difftracker.DiffTracker{}
	initializeRuntimeDiffTracker = func(
		ctx context.Context,
		_ difftracker.Config,
		_ azclient.ClientFactory,
		_ kubernetes.Interface,
	) (*difftracker.DiffTracker, error) {
		runtimeCtx = ctx
		return tracker, nil
	}
	startRuntimePodInformer = func(ctx context.Context, got *difftracker.DiffTracker) error {
		assert.Same(t, tracker, got)
		assert.Same(t, runtimeCtx, ctx)
		return nil
	}

	kubeClient := fake.NewSimpleClientset()
	runtime := NewRuntime(providerconfig.Config{ServiceGatewayEnabled: true}, nil, kubeClient)
	runtime.SetEventRecorder(record.NewFakeRecorder(1))

	ctx, cancel := context.WithCancel(context.Background())
	if !assert.NoError(t, runtime.Start(ctx, informers.NewSharedInformerFactory(kubeClient, 0))) {
		cancel()
		return
	}
	assert.Same(t, tracker, runtime.tracker)

	loadBalancer, supported := runtime.LoadBalancer()
	assert.True(t, supported)
	service := new(v1.Service)
	_, err := loadBalancer.EnsureLoadBalancer(context.Background(), "cluster", service, nil)
	assert.NoError(t, err)

	err = runtime.Start(ctx, informers.NewSharedInformerFactory(kubeClient, 0))
	assert.EqualError(t, err, "ServiceGateway runtime is already started")

	cancel()
	select {
	case <-runtimeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("ServiceGateway runtime context was not cancelled")
	}
}

func TestRuntimeStartFailureRollsBack(t *testing.T) {
	originalInitialize := initializeRuntimeDiffTracker
	originalStartPodInformer := startRuntimePodInformer
	t.Cleanup(func() {
		initializeRuntimeDiffTracker = originalInitialize
		startRuntimePodInformer = originalStartPodInformer
	})

	tracker := &difftracker.DiffTracker{}
	var runtimeCtx context.Context
	initializeRuntimeDiffTracker = func(
		context.Context,
		difftracker.Config,
		azclient.ClientFactory,
		kubernetes.Interface,
	) (*difftracker.DiffTracker, error) {
		return tracker, nil
	}
	startRuntimePodInformer = func(ctx context.Context, _ *difftracker.DiffTracker) error {
		runtimeCtx = ctx
		return errors.New("pod informer failed")
	}

	kubeClient := fake.NewSimpleClientset()
	runtime := NewRuntime(providerconfig.Config{ServiceGatewayEnabled: true}, nil, kubeClient)
	runtime.SetEventRecorder(record.NewFakeRecorder(1))
	err := runtime.Start(
		context.Background(),
		informers.NewSharedInformerFactory(kubeClient, 0),
	)
	assert.EqualError(t, err, "start filtered Pod informer: pod informer failed")
	assert.Nil(t, runtime.tracker)

	loadBalancer, supported := runtime.LoadBalancer()
	assert.True(t, supported)
	_, _, dependencyErr := loadBalancer.GetLoadBalancer(context.Background(), "cluster", new(v1.Service))
	assert.EqualError(t, dependencyErr, "ServiceGateway LoadBalancer is not initialized")
	select {
	case <-runtimeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("failed ServiceGateway runtime context was not cancelled")
	}
}
