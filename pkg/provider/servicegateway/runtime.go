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
	"fmt"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/servicegateway/difftracker"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

var (
	initializeRuntimeDiffTracker = difftracker.InitializeFromCluster
	startRuntimePodInformer      = func(ctx context.Context, tracker *difftracker.DiffTracker) error {
		return tracker.SetUpPodInformer(ctx.Done())
	}
)

// Runtime owns ServiceGateway startup dependencies and its LoadBalancer implementation.
type Runtime struct {
	enabled bool
	config  difftracker.Config

	loadBalancer         *difftracker.LoadBalancer
	networkClientFactory azclient.ClientFactory
	kubeClient           kubernetes.Interface
	eventRecorder        record.EventRecorder

	mu      sync.Mutex
	tracker *difftracker.DiffTracker
}

// NewRuntime creates the ServiceGateway runtime from the cloud-provider configuration.
func NewRuntime(config providerconfig.Config, networkClientFactory azclient.ClientFactory, kubeClient kubernetes.Interface) *Runtime {
	runtime := &Runtime{
		enabled:              config.ServiceGatewayEnabled,
		config:               diffTrackerConfig(config),
		networkClientFactory: networkClientFactory,
		kubeClient:           kubeClient,
	}
	if runtime.enabled {
		runtime.loadBalancer = difftracker.NewLoadBalancer(nil)
	}
	return runtime
}

func diffTrackerConfig(config providerconfig.Config) difftracker.Config {
	networkSubscriptionID := config.SubscriptionID
	if config.UsesNetworkResourceInDifferentSubscription() {
		networkSubscriptionID = config.NetworkResourceSubscriptionID
	}

	serviceGatewayName := consts.DefaultServiceGatewayResourceName
	return difftracker.Config{
		SubscriptionID:                config.SubscriptionID,
		NetworkResourceSubscriptionID: networkSubscriptionID,
		ResourceGroup:                 config.ResourceGroup,
		Location:                      config.Location,
		VNetName:                      config.VnetName,
		VNetResourceGroup:             config.VnetResourceGroup,
		ServiceGatewayResourceName:    serviceGatewayName,
		ServiceGatewayID: difftracker.ServiceGatewayResourceID(
			networkSubscriptionID,
			config.ResourceGroup,
			serviceGatewayName,
		),
	}
}

// Enabled reports whether ServiceGateway is enabled.
func (r *Runtime) Enabled() bool {
	return r != nil && r.enabled
}

// SetEventRecorder publishes the event recorder created during cloud initialization.
func (r *Runtime) SetEventRecorder(eventRecorder record.EventRecorder) {
	if r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.eventRecorder = eventRecorder
	}
}

// SetKubeClient publishes a Kubernetes client supplied during cloud initialization.
func (r *Runtime) SetKubeClient(kubeClient kubernetes.Interface) {
	if r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.kubeClient = kubeClient
	}
}

// Start initializes the ServiceGateway runtime.
func (r *Runtime) Start(ctx context.Context, informerFactory informers.SharedInformerFactory) (err error) {
	if r == nil || r.loadBalancer == nil {
		return fmt.Errorf("ServiceGateway runtime is not configured")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.tracker != nil {
		return fmt.Errorf("ServiceGateway runtime is already started")
	}
	if informerFactory == nil {
		return fmt.Errorf("ServiceGateway runtime requires a shared informer factory")
	}
	if r.kubeClient == nil {
		return fmt.Errorf("ServiceGateway runtime requires an initialized Kubernetes client")
	}
	if r.eventRecorder == nil {
		return fmt.Errorf("ServiceGateway runtime requires an initialized event recorder")
	}

	runtimeCtx, cancel := context.WithCancel(ctx)
	var unregisterInformers func()
	defer func() {
		if err != nil {
			cancel()
			if unregisterInformers != nil {
				unregisterInformers()
			}
		}
	}()

	difftracker.RegisterMetrics()

	tracker, err := initializeRuntimeDiffTracker(runtimeCtx, r.config, r.networkClientFactory, r.kubeClient)
	if err != nil {
		return fmt.Errorf("initialize difftracker: %w", err)
	}

	tracker.SetServiceLister(informerFactory.Core().V1().Services().Lister())
	tracker.SetNodeLister(informerFactory.Core().V1().Nodes().Lister())
	tracker.SetEventRecorder(r.eventRecorder)

	unregisterInformers, err = RegisterInformers(informerFactory, tracker)
	if err != nil {
		return err
	}
	if err := startRuntimePodInformer(runtimeCtx, tracker); err != nil {
		return fmt.Errorf("start filtered Pod informer: %w", err)
	}
	if err := r.loadBalancer.SetTracker(tracker); err != nil {
		return fmt.Errorf("initialize ServiceGateway LoadBalancer: %w", err)
	}

	r.tracker = tracker
	difftracker.RecordServiceGatewayEnabled()
	return nil
}

// LoadBalancer returns the ServiceGateway LoadBalancer when enabled.
func (r *Runtime) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	if r == nil || !r.enabled || r.loadBalancer == nil {
		return nil, false
	}
	return r.loadBalancer, true
}

// RegisterInformers installs the shared Node and EndpointSlice handlers owned by ServiceGateway.
func RegisterInformers(informerFactory informers.SharedInformerFactory, tracker *difftracker.DiffTracker) (func(), error) {
	logger := log.Background().WithName("RegisterInformers")
	removeHandler := func(informer cache.SharedIndexInformer, registration cache.ResourceEventHandlerRegistration, name string) {
		if err := informer.RemoveEventHandler(registration); err != nil {
			logger.Error(err, "Could not unregister ServiceGateway handlers", "informer", name)
		}
	}

	nodeInformer := informerFactory.Core().V1().Nodes().Informer()
	nodeRegistration, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*v1.Node)
			if !ok {
				logger.Error(nil, "ServiceGateway Node add received unexpected object", "objectType", fmt.Sprintf("%T", obj))
				return
			}
			tracker.ReconcileNodeIPChange(node.Name, nil, nodePrivateIPAddresses(node))
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldNode, oldOK := oldObj.(*v1.Node)
			newNode, newOK := newObj.(*v1.Node)
			if !oldOK || !newOK {
				logger.Error(nil, "ServiceGateway Node update received unexpected objects",
					"oldObjectType", fmt.Sprintf("%T", oldObj),
					"newObjectType", fmt.Sprintf("%T", newObj))
				return
			}
			oldIPs := nodePrivateIPAddresses(oldNode)
			newIPs := nodePrivateIPAddresses(newNode)
			if !utilsets.NewString(oldIPs...).Equals(utilsets.NewString(newIPs...)) {
				tracker.ReconcileNodeIPChange(newNode.Name, oldIPs, newIPs)
			}
		},
		DeleteFunc: func(obj interface{}) {
			node, err := nodeFromDeleteEvent(obj)
			if err != nil {
				logger.Error(err, "Could not process ServiceGateway Node deletion")
				return
			}
			tracker.ReconcileNodeIPChange(node.Name, nodePrivateIPAddresses(node), nil)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("register ServiceGateway Node handlers: %w", err)
	}

	endpointSliceInformer := informerFactory.Discovery().V1().EndpointSlices().Informer()
	endpointSliceRegistration, err := endpointSliceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			endpointSlice, ok := obj.(*discoveryv1.EndpointSlice)
			if !ok {
				logger.Error(nil, "ServiceGateway EndpointSlice add received unexpected object", "objectType", fmt.Sprintf("%T", obj))
				return
			}
			tracker.ReconcileEndpointSlice(nil, endpointSlice)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldEndpointSlice, oldOK := oldObj.(*discoveryv1.EndpointSlice)
			newEndpointSlice, newOK := newObj.(*discoveryv1.EndpointSlice)
			if !oldOK || !newOK {
				logger.Error(nil, "ServiceGateway EndpointSlice update received unexpected objects",
					"oldObjectType", fmt.Sprintf("%T", oldObj),
					"newObjectType", fmt.Sprintf("%T", newObj))
				return
			}
			tracker.ReconcileEndpointSlice(oldEndpointSlice, newEndpointSlice)
		},
		DeleteFunc: func(obj interface{}) {
			endpointSlice, err := endpointSliceFromDeleteEvent(obj)
			if err != nil {
				logger.Error(err, "Could not process ServiceGateway EndpointSlice deletion")
				return
			}
			tracker.ReconcileEndpointSlice(endpointSlice, nil)
		},
	})
	if err != nil {
		removeHandler(nodeInformer, nodeRegistration, "Node")
		return nil, fmt.Errorf("register ServiceGateway EndpointSlice handlers: %w", err)
	}

	return func() {
		removeHandler(endpointSliceInformer, endpointSliceRegistration, "EndpointSlice")
		removeHandler(nodeInformer, nodeRegistration, "Node")
	}, nil
}

func nodePrivateIPAddresses(node *v1.Node) []string {
	addresses := make([]string, 0)
	for _, address := range node.Status.Addresses {
		if strings.EqualFold(string(address.Type), string(v1.NodeInternalIP)) {
			addresses = append(addresses, address.Address)
		}
	}
	return addresses
}

func nodeFromDeleteEvent(obj interface{}) (*v1.Node, error) {
	return objectFromDeleteEvent[v1.Node](obj)
}

func endpointSliceFromDeleteEvent(obj interface{}) (*discoveryv1.EndpointSlice, error) {
	return objectFromDeleteEvent[discoveryv1.EndpointSlice](obj)
}

func objectFromDeleteEvent[T any](obj interface{}) (*T, error) {
	if value, ok := obj.(*T); ok {
		return value, nil
	}
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		return nil, fmt.Errorf("cannot convert %T to the expected type", obj)
	}
	value, ok := tombstone.Obj.(*T)
	if !ok {
		return nil, fmt.Errorf("cannot convert tombstone object %T to the expected type", tombstone.Obj)
	}
	return value, nil
}
