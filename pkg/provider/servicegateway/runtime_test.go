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

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/servicegateway/difftracker"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
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
	assert.Contains(t, diffTrackerConfig.ServiceGatewayResourceID(), "/subscriptions/network-subscription/")
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
	service.UID = "service-uid"
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

// TestRegisterInformers_DeliversEndpointSliceEventsToTracker pins that RegisterInformers wires
// handlers that actually reach the DiffTracker, by driving a real EndpointSlice through the shared
// informer and observing the pod address land in the tracker's sync state.
//
// RegisterInformers had no test reference anywhere: dropping its AddEventHandler calls — silently
// discarding every EndpointSlice and Node event the tracker relies on to keep Azure in sync — left
// the whole package green. Asserting registration merely "succeeded", or that the informer's own
// store filled, is not enough: both hold even when no handler is attached. This asserts the
// tracker's observable state instead.
func TestRegisterInformers_DeliversEndpointSliceEventsToTracker(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		serviceUID = "svc-informer-probe"
		nodeName   = "node-informer-probe"
		nodeIP     = "10.0.0.42"
		podIP      = "10.244.0.42"
	)

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: nodeIP}},
		},
	}
	kubeClient := fake.NewSimpleClientset(node)

	tracker, err := difftracker.New(
		logr.Discard(),
		difftracker.K8sState{
			Services: utilsets.NewString(serviceUID),
			Egresses: utilsets.NewString(),
			Nodes:    map[string]difftracker.Node{},
		},
		difftracker.NRPState{
			LoadBalancers: utilsets.NewString(serviceUID),
			NATGateways:   utilsets.NewString(),
			Locations:     map[string]difftracker.NRPLocation{},
		},
		testDiffTrackerConfig(),
		mock_azclient.NewMockClientFactory(ctrl),
		kubeClient,
	)
	if !assert.NoError(t, err) {
		return
	}

	informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	tracker.SetNodeLister(informerFactory.Core().V1().Nodes().Lister())

	unregister, err := RegisterInformers(informerFactory, tracker)
	if !assert.NoError(t, err) {
		return
	}
	assert.NotNil(t, unregister, "a successful registration must return an unregister func")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	informerFactory.Start(ctx.Done())
	informerFactory.WaitForCacheSync(ctx.Done())

	ready := true
	_, err = kubeClient.DiscoveryV1().EndpointSlices("default").Create(ctx, &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "probe-slice",
			Namespace: "default",
			Labels:    map[string]string{discoveryv1.LabelServiceName: "probe-svc"},
			// The tracker resolves the owning Service by OwnerReference UID.
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Service",
				Name: "probe-svc",
				UID:  types.UID(serviceUID),
			}},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{podIP},
			NodeName:   ptr.To(nodeName),
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		}},
	}, metav1.CreateOptions{})
	assert.NoError(t, err)

	assert.Eventually(t, func() bool {
		locations := tracker.GetSyncLocationsAddresses()
		loc, ok := locations.Locations[nodeIP]
		if !ok {
			return false
		}
		_, hasAddr := loc.Addresses[podIP]
		return hasAddr
	}, 15*time.Second, 50*time.Millisecond,
		"the EndpointSlice handler registered by RegisterInformers must forward the event to the "+
			"tracker; without it the pod address never reaches Azure")

	unregister()
}

func testDiffTrackerConfig() difftracker.Config {
	cfg := providerconfig.Config{}
	cfg.SubscriptionID = "sub"
	cfg.ResourceGroup = "rg"
	cfg.Location = "eastus"
	cfg.VnetName = "vnet"
	cfg.VnetResourceGroup = "rg"
	return diffTrackerConfig(cfg)
}

// TestNodePrivateIPAddresses_SelectsOnlyInternalIPs pins that node locations are derived from
// InternalIP addresses alone. A node's address list also carries ExternalIP and Hostname entries;
// registering those as locations would publish backends under an address NRP cannot route to.
func TestNodePrivateIPAddresses_SelectsOnlyInternalIPs(t *testing.T) {
	node := &v1.Node{
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{Type: v1.NodeHostName, Address: "node-1"},
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeExternalIP, Address: "52.1.2.3"},
				{Type: v1.NodeInternalIP, Address: "fd00::1"},
				{Type: v1.NodeExternalDNS, Address: "node-1.example.com"},
			},
		},
	}

	assert.Equal(t, []string{"10.0.0.1", "fd00::1"}, nodePrivateIPAddresses(node),
		"only InternalIP addresses are node locations, in the order the node reports them")

	assert.Empty(t, nodePrivateIPAddresses(&v1.Node{}),
		"a node with no addresses yields no locations")
}

// TestNodeFromDeleteEvent_DecodesTombstones pins that a delete delivered as a tombstone is decoded.
// An informer that has fallen behind wraps the object in DeletedFinalStateUnknown, so a handler that
// only accepts the bare type silently drops those deletions.
func TestNodeFromDeleteEvent_DecodesTombstones(t *testing.T) {
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}

	got, err := nodeFromDeleteEvent(node)
	assert.NoError(t, err)
	assert.Equal(t, "node-1", got.Name, "a bare object decodes")

	got, err = nodeFromDeleteEvent(cache.DeletedFinalStateUnknown{Key: "node-1", Obj: node})
	assert.NoError(t, err)
	assert.Equal(t, "node-1", got.Name, "a tombstone decodes to the object it wraps")

	_, err = nodeFromDeleteEvent(cache.DeletedFinalStateUnknown{Key: "node-1", Obj: "not-a-node"})
	assert.Error(t, err, "a tombstone wrapping the wrong type is an error, not a silent drop")

	_, err = nodeFromDeleteEvent("not-a-node")
	assert.Error(t, err, "an unexpected object is an error")
}
