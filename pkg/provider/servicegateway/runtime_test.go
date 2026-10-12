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
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/natgatewayclient/mock_natgatewayclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
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

	unregister, synced, err := RegisterInformers(informerFactory, tracker)
	if !assert.NoError(t, err) {
		return
	}
	assert.NotNil(t, unregister, "a successful registration must return an unregister func")
	assert.NotNil(t, synced, "a successful registration must return a synced func")

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

// TestRegisterInformers_SyncedWaitsForInitialList pins that the returned synced func reports true
// only once both handlers have received the initial list. Startup reconciliation runs as soon as it
// does and treats every cached slice and startup node the handlers have not seen as deleted.
func TestRegisterInformers_SyncedWaitsForInitialList(t *testing.T) {
	testCases := []struct {
		name string
		// failingList is the resource whose list fails, so its handler never receives the initial list.
		failingList string
	}{
		{name: "both lists succeed"},
		{name: "node list fails", failingList: "nodes"},
		{name: "endpointslice list fails", failingList: "endpointslices"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			kubeClient := fake.NewSimpleClientset(
				&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
				&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "eps1", Namespace: "default"}},
			)
			if tc.failingList != "" {
				kubeClient.PrependReactor("list", tc.failingList, func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("list failed")
				})
			}
			tracker, err := difftracker.New(
				logr.Discard(),
				difftracker.K8sState{Services: utilsets.NewString(), Egresses: utilsets.NewString(), Nodes: map[string]difftracker.Node{}},
				difftracker.NRPState{LoadBalancers: utilsets.NewString(), NATGateways: utilsets.NewString(), Locations: map[string]difftracker.NRPLocation{}},
				testDiffTrackerConfig(),
				mock_azclient.NewMockClientFactory(ctrl),
				kubeClient,
			)
			if !assert.NoError(t, err) {
				return
			}
			informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
			tracker.SetNodeLister(informerFactory.Core().V1().Nodes().Lister())

			_, synced, err := RegisterInformers(informerFactory, tracker)
			if !assert.NoError(t, err) {
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			informerFactory.Start(ctx.Done())
			if tc.failingList == "" {
				assert.True(t, cache.WaitForCacheSync(ctx.Done(), synced), "handlers that received the initial list must report synced")
				return
			}
			assert.Never(t, synced, time.Second, 20*time.Millisecond,
				"handlers must not report synced while one has not received the initial list")
		})
	}
}

// blockingNodeLister blocks Get until release is closed. entered is closed by the first Get.
type blockingNodeLister struct {
	corelisters.NodeLister
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
}

func (l *blockingNodeLister) Get(name string) (*v1.Node, error) {
	l.enteredOnce.Do(func() { close(l.entered) })
	<-l.release
	return l.NodeLister.Get(name)
}

// TestRegisterInformers_SyncedWaitsForHandlers pins that synced waits for each handler to process
// the initial list, not only for its informer's store to hold it. A handler lags its store, and
// startup reconciliation would then drain the startup nodes whose Add is still queued.
func TestRegisterInformers_SyncedWaitsForHandlers(t *testing.T) {
	newSlice := func(name string) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       "default",
				OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: "svc-handler-sync"}},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{"10.244.1.10"},
				NodeName:   ptr.To("node1"),
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
			}},
		}
	}
	testCases := []struct {
		name string
		// listed is the only object listed, so only the handler of its kind has an initial list.
		listed runtime.Object
	}{
		{name: "Node handler behind its store", listed: &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}},
		{name: "EndpointSlice handler behind its store", listed: newSlice("eps1")},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			kubeClient := fake.NewSimpleClientset(tc.listed)
			tracker, err := difftracker.New(
				logr.Discard(),
				difftracker.K8sState{Services: utilsets.NewString(), Egresses: utilsets.NewString(), Nodes: map[string]difftracker.Node{}},
				difftracker.NRPState{LoadBalancers: utilsets.NewString(), NATGateways: utilsets.NewString(), Locations: map[string]difftracker.NRPLocation{}},
				testDiffTrackerConfig(),
				mock_azclient.NewMockClientFactory(ctrl),
				kubeClient,
			)
			if !assert.NoError(t, err) {
				return
			}
			informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
			nodeLister := &blockingNodeLister{
				NodeLister: informerFactory.Core().V1().Nodes().Lister(),
				entered:    make(chan struct{}),
				release:    make(chan struct{}),
			}
			tracker.SetNodeLister(nodeLister)
			_, synced, err := RegisterInformers(informerFactory, tracker)
			if !assert.NoError(t, err) {
				return
			}

			// A reconciliation blocked in the lister holds the tracker lock, so the handler with an
			// initial list blocks on it.
			reconcileDone := make(chan struct{})
			go func() {
				defer close(reconcileDone)
				tracker.ReconcileEndpointSlice(nil, newSlice("held"))
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			select {
			case <-nodeLister.entered:
			case <-ctx.Done():
				t.Fatal("the reconciliation did not reach the node lister")
			}

			informerFactory.Start(ctx.Done())
			for informerType, ok := range informerFactory.WaitForCacheSync(ctx.Done()) {
				assert.True(t, ok, "the %v store must sync", informerType)
			}
			assert.Never(t, synced, time.Second, 20*time.Millisecond,
				"synced must wait for the handlers to process the initial list, not only for the stores to hold it")

			close(nodeLister.release)
			assert.True(t, cache.WaitForCacheSync(ctx.Done(), synced), "handlers that processed the initial list must report synced")
			<-reconcileDone
		})
	}
}

// TestRuntimeStart_AppliesEndpointSliceChangesMadeDuringInitialization pins that the handlers
// installed after initialization reconcile what changed while it ran. The tracker is seeded with
// the slices initialization listed; one endpoint then goes not-ready and another slice is deleted
// before the informers start. Neither change produces an event the tracker could diff on its own:
// the first event per slice is an Add, and a deleted slice produces none.
func TestRuntimeStart_AppliesEndpointSliceChangesMadeDuringInitialization(t *testing.T) {
	originalInitialize := initializeRuntimeDiffTracker
	originalStartPodInformer := startRuntimePodInformer
	t.Cleanup(func() {
		initializeRuntimeDiffTracker = originalInitialize
		startRuntimePodInformer = originalStartPodInformer
	})
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		serviceUID = "svc-startup-gap"
		node1IP    = "10.0.0.1"
		node2IP    = "10.0.0.2"
		podA       = "10.244.1.10"
		podB       = "10.244.2.20"
		podC       = "10.244.2.30"
	)
	newNode := func(name, ip string) *v1.Node {
		return &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: ip}}},
		}
	}
	type endpoint struct {
		podIP, nodeName string
		ready           bool
	}
	newSlice := func(name, resourceVersion string, endpoints ...endpoint) *discoveryv1.EndpointSlice {
		es := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       "default",
				ResourceVersion: resourceVersion,
				OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: types.UID(serviceUID)}},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
		}
		for _, ep := range endpoints {
			es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{
				Addresses:  []string{ep.podIP},
				NodeName:   ptr.To(ep.nodeName),
				Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(ep.ready)},
			})
		}
		return es
	}

	node1, node2 := newNode("node1", node1IP), newNode("node2", node2IP)
	changed := newSlice("changed", "1", endpoint{podA, "node1", true}, endpoint{podB, "node2", true})
	deleted := newSlice("deleted", "1", endpoint{podC, "node2", true})
	kubeClient := fake.NewSimpleClientset(node1, node2, changed, deleted)

	tracker, err := difftracker.New(
		logr.Discard(),
		difftracker.K8sState{Services: utilsets.NewString(serviceUID), Egresses: utilsets.NewString(), Nodes: map[string]difftracker.Node{}},
		difftracker.NRPState{LoadBalancers: utilsets.NewString(serviceUID), NATGateways: utilsets.NewString(), Locations: map[string]difftracker.NRPLocation{}},
		testDiffTrackerConfig(),
		mock_azclient.NewMockClientFactory(ctrl),
		kubeClient,
	)
	if !assert.NoError(t, err) {
		return
	}
	registered := func() map[string]string {
		addresses := map[string]string{}
		for nodeIP, location := range tracker.GetSyncLocationsAddresses().Locations {
			for podIP := range location.Addresses {
				addresses[podIP] = nodeIP
			}
		}
		return addresses
	}

	// Initialization: the slices it listed are registered and cached.
	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	assert.NoError(t, nodeIndexer.Add(node1))
	assert.NoError(t, nodeIndexer.Add(node2))
	tracker.SetNodeLister(corelisters.NewNodeLister(nodeIndexer))
	tracker.ReconcileEndpointSlice(nil, changed.DeepCopy())
	tracker.ReconcileEndpointSlice(nil, deleted.DeepCopy())
	if !assert.Equal(t, map[string]string{podA: node1IP, podB: node2IP, podC: node2IP}, registered()) {
		return
	}

	// While initialization runs, B goes not-ready and the second slice is deleted.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = kubeClient.DiscoveryV1().EndpointSlices("default").Update(ctx,
		newSlice("changed", "2", endpoint{podA, "node1", true}, endpoint{podB, "node2", false}), metav1.UpdateOptions{})
	assert.NoError(t, err)
	assert.NoError(t, kubeClient.DiscoveryV1().EndpointSlices("default").Delete(ctx, "deleted", metav1.DeleteOptions{}))

	initializeRuntimeDiffTracker = func(context.Context, difftracker.Config, azclient.ClientFactory, kubernetes.Interface) (*difftracker.DiffTracker, error) {
		return tracker, nil
	}
	startRuntimePodInformer = func(context.Context, *difftracker.DiffTracker) error { return nil }
	runtime := NewRuntime(providerconfig.Config{ServiceGatewayEnabled: true}, nil, kubeClient)
	runtime.SetEventRecorder(record.NewFakeRecorder(1))
	// List nodes before slices so the slice events resolve every node through the lister. The
	// tracker was not built by InitializeFromCluster, so it has no startup node IPs to fall back on.
	informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	informerFactory.Core().V1().Nodes().Informer()
	informerFactory.Start(ctx.Done())
	informerFactory.WaitForCacheSync(ctx.Done())
	if !assert.NoError(t, runtime.Start(ctx, informerFactory)) {
		return
	}
	informerFactory.Start(ctx.Done())

	assert.Eventually(t, func() bool {
		addresses := registered()
		return len(addresses) == 1 && addresses[podA] == node1IP
	}, 15*time.Second, 50*time.Millisecond,
		"endpoints made not-ready or deleted during initialization must not stay registered")
}

// TestRuntimeStart_DrainsOnlyNodesDeletedDuringInitialization pins that after a restart a node the
// informer replays keeps the endpoints initialization registered for it. Initialization records
// every node it lists and startup reconciliation drains each one whose informer Add never arrived,
// so a Node handler that did not consume the record would drain every endpoint after each restart.
// node2 is deleted before the informers start; it is drained in the same pass as any other node,
// so its removal marks that the pass has run.
func TestRuntimeStart_DrainsOnlyNodesDeletedDuringInitialization(t *testing.T) {
	originalStartPodInformer := startRuntimePodInformer
	t.Cleanup(func() { startRuntimePodInformer = originalStartPodInformer })
	startRuntimePodInformer = func(context.Context, *difftracker.DiffTracker) error { return nil }
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		serviceUID = "svc-restart"
		node1IP    = "10.0.0.1"
		node2IP    = "10.0.0.2"
		podA       = "10.244.1.10"
		podB       = "10.244.2.20"
	)
	newNode := func(name, ip string) *v1.Node {
		return &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: ip}}},
		}
	}
	newEndpoint := func(podIP, nodeName string) discoveryv1.Endpoint {
		return discoveryv1.Endpoint{
			Addresses:  []string{podIP},
			NodeName:   ptr.To(nodeName),
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		}
	}
	kubeClient := fake.NewSimpleClientset(
		newNode("node1", node1IP),
		newNode("node2", node2IP),
		&v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "default", UID: types.UID(serviceUID)},
			Spec:       v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer},
		},
		&discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "svc-eps",
				Namespace:       "default",
				OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: types.UID(serviceUID)}},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Endpoints:   []discoveryv1.Endpoint{newEndpoint(podA, "node1"), newEndpoint(podB, "node2")},
		},
	)

	// NRP already has the Service's load balancer. nrpAddresses (pod IP -> node IP) replays the
	// location updates the tracker sends.
	var mu sync.Mutex
	nrpAddresses := map[string]string{}
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockLB := mock_loadbalancerclient.NewMockInterface(ctrl)
	mockNAT := mock_natgatewayclient.NewMockInterface(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockFactory.EXPECT().GetLoadBalancerClient().Return(mockLB).AnyTimes()
	mockFactory.EXPECT().GetNatGatewayClient().Return(mockNAT).AnyTimes()
	mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()
	mockSGW.EXPECT().GetServices(gomock.Any(), gomock.Any(), gomock.Any()).Return([]*armnetwork.ServiceGatewayService{{
		Name:       ptr.To(serviceUID),
		Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{ServiceType: ptr.To(armnetwork.ServiceTypeInbound)},
	}}, nil).AnyTimes()
	mockSGW.EXPECT().GetAddressLocations(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mockSGW.EXPECT().UpdateAddressLocations(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, _ string, request armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
			mu.Lock()
			defer mu.Unlock()
			for _, location := range request.AddressLocations {
				if len(location.Addresses) == 0 || *location.AddressUpdateAction == armnetwork.AddressUpdateActionFullUpdate {
					for podIP, nodeIP := range nrpAddresses {
						if nodeIP == *location.AddressLocation {
							delete(nrpAddresses, podIP)
						}
					}
				}
				for _, address := range location.Addresses {
					if len(address.Services) == 0 {
						delete(nrpAddresses, *address.Address)
					} else {
						nrpAddresses[*address.Address] = *location.AddressLocation
					}
				}
			}
			return nil
		}).AnyTimes()
	mockLB.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mockNAT.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mockPIP.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	registered := func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(nrpAddresses)
	}

	// Start runs the real InitializeFromCluster against the mocked Azure clients.
	config := providerconfig.Config{ServiceGatewayEnabled: true}
	config.SubscriptionID = "sub"
	config.ResourceGroup = "rg"
	config.Location = "eastus"
	config.VnetName = "vnet"
	runtime := NewRuntime(config, mockFactory, kubeClient)
	runtime.SetEventRecorder(record.NewFakeRecorder(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	if !assert.NoError(t, runtime.Start(ctx, informerFactory)) {
		return
	}
	if !assert.Equal(t, map[string]string{podA: node1IP, podB: node2IP}, registered()) {
		return
	}

	assert.NoError(t, kubeClient.CoreV1().Nodes().Delete(ctx, "node2", metav1.DeleteOptions{}))
	informerFactory.Start(ctx.Done())

	assert.Eventually(t, func() bool {
		_, ok := registered()[podB]
		return !ok
	}, 15*time.Second, 50*time.Millisecond, "a node deleted during initialization must be drained once the handlers sync")
	assert.Equal(t, map[string]string{podA: node1IP}, registered(),
		"a node the informer replayed must keep the endpoints initialization registered for it")
}

// TestCompleteStartupReconciliationOnSync_WaitsForHandlerSync pins that startup reconciliation does
// not run before the handlers have received the initial list: against an unsynced lister every
// cached slice looks deleted, so running early would drain live endpoints.
func TestCompleteStartupReconciliationOnSync_WaitsForHandlerSync(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		serviceUID = "svc-wait-for-sync"
		nodeIP     = "10.0.0.1"
		podIP      = "10.244.1.10"
	)
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: nodeIP}}},
	}
	kubeClient := fake.NewSimpleClientset()
	tracker, err := difftracker.New(
		logr.Discard(),
		difftracker.K8sState{Services: utilsets.NewString(serviceUID), Egresses: utilsets.NewString(), Nodes: map[string]difftracker.Node{}},
		difftracker.NRPState{LoadBalancers: utilsets.NewString(serviceUID), NATGateways: utilsets.NewString(), Locations: map[string]difftracker.NRPLocation{}},
		testDiffTrackerConfig(),
		mock_azclient.NewMockClientFactory(ctrl),
		kubeClient,
	)
	if !assert.NoError(t, err) {
		return
	}
	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	assert.NoError(t, nodeIndexer.Add(node))
	tracker.SetNodeLister(corelisters.NewNodeLister(nodeIndexer))
	tracker.ReconcileEndpointSlice(nil, &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "eps1",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Service", UID: types.UID(serviceUID)}},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{podIP},
			NodeName:   ptr.To("node1"),
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		}},
	})
	registered := func() bool {
		_, ok := tracker.GetSyncLocationsAddresses().Locations[nodeIP].Addresses[podIP]
		return ok
	}
	if !assert.True(t, registered()) {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	completeStartupReconciliationOnSync(ctx, func() bool { return false },
		informers.NewSharedInformerFactory(kubeClient, 0), tracker)

	assert.True(t, registered(), "startup reconciliation must not run before the handlers have synced")
	// The slice is still cached: a node replay re-registers it after a drain.
	tracker.ReconcileNodeIPChange("node1", []string{nodeIP}, nil)
	tracker.ReconcileNodeIPChange("node1", nil, []string{nodeIP})
	assert.True(t, registered(), "the cached slice must survive an aborted startup reconciliation")
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
