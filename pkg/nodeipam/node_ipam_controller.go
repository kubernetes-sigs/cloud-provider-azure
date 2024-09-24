/*
Copyright 2021 The Kubernetes Authors.

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

package nodeipam

import (
	"context"
	"fmt"
	"net"
	"sync"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/nodeipam/ipam"
)

var (
	metricsLock        sync.Mutex
	rateLimiterMetrics = make(map[string]*rateLimiterMetric)
)

type rateLimiterMetric struct {
	metric metrics.GaugeMetric
}

// Controller is the controller that manages node ipam state.
type Controller struct {
	allocatorType ipam.CIDRAllocatorType

	cloud                cloudprovider.Interface
	clusterCIDRs         []*net.IPNet
	serviceCIDR          *net.IPNet
	secondaryServiceCIDR *net.IPNet
	kubeClient           clientset.Interface
	// Method for easy mocking in unittest.
	lookupIP func(host string) ([]net.IP, error)

	nodeLister         corelisters.NodeLister
	nodeInformerSynced cache.InformerSynced

	cidrAllocator ipam.CIDRAllocator
}

func registerRateLimiterMetric(ownerName string) error {
	metricsLock.Lock()
	defer metricsLock.Unlock()

	if _, ok := rateLimiterMetrics[ownerName]; ok {
		// only register once in Prometheus. We happen to see an ownerName reused in parallel integration tests.
		return nil
	}
	metric := metrics.NewGauge(&metrics.GaugeOpts{
		Name:           "rate_limiter_use",
		Subsystem:      ownerName,
		Help:           fmt.Sprintf("A metric measuring the saturation of the rate limiter for %v", ownerName),
		StabilityLevel: metrics.ALPHA,
	})
	if err := legacyregistry.Register(metric); err != nil {
		return fmt.Errorf("error registering rate limiter usage metric: %w", err)
	}
	rateLimiterMetrics[ownerName] = &rateLimiterMetric{
		metric: metric,
	}
	return nil
}

// RegisterMetricAndTrackRateLimiterUsage registers a metric ownerName_rate_limiter_use in prometheus to track
// how much used rateLimiter is and starts a goroutine that updates this metric every updatePeriod
func RegisterMetricAndTrackRateLimiterUsage(ownerName string, _ flowcontrol.RateLimiter) error {
	if err := registerRateLimiterMetric(ownerName); err != nil {
		return err
	}
	// TODO: determine how to track rate limiter saturation
	// See discussion at https://go-review.googlesource.com/c/time/+/29958#message-4caffc11669cadd90e2da4c05122cfec50ea6a22
	// go wait.Until(func() {
	//   metricsLock.Lock()
	//   defer metricsLock.Unlock()
	//   rateLimiterMetrics[ownerName].metric.Set()
	// }, updatePeriod, rateLimiterMetrics[ownerName].stopCh)
	return nil
}

// NewNodeIpamController returns a new node IP Address Management controller to
// sync instances from cloudprovider.
// This method returns an error if it is unable to initialize the CIDR bitmap with
// podCIDRs it has already allocated to nodes. Since we don't allow podCIDR changes
// currently, this should be handled as a fatal error.
func NewNodeIpamController(
	nodeInformer coreinformers.NodeInformer,
	cloud cloudprovider.Interface,
	kubeClient clientset.Interface,
	clusterCIDRs []*net.IPNet,
	serviceCIDR *net.IPNet,
	secondaryServiceCIDR *net.IPNet,
	nodeCIDRMaskSizes []int,
	allocatorType ipam.CIDRAllocatorType) (*Controller, error) {

	if kubeClient == nil {
		klog.Fatalf("kubeClient is nil when starting Controller")
	}

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)

	klog.Infof("Sending events to api server.")
	eventBroadcaster.StartRecordingToSink(
		&v1core.EventSinkImpl{
			Interface: kubeClient.CoreV1().Events(""),
		})

	if kubeClient.CoreV1().RESTClient().GetRateLimiter() != nil {
		_ = RegisterMetricAndTrackRateLimiterUsage("node_ipam_controller", kubeClient.CoreV1().RESTClient().GetRateLimiter())
	}

	// Cloud CIDR allocator does not rely on clusterCIDR or nodeCIDRMaskSize for allocation.
	if allocatorType != ipam.CloudAllocatorType {
		if len(clusterCIDRs) == 0 {
			klog.Fatal("Controller: Must specify --cluster-cidr if --allocate-node-cidrs is set")
		}

		// TODO: (khenidak) IPv6DualStack beta:
		// - modify mask to allow flexible masks for IPv4 and IPv6
		// - for alpha status they are the same

		// for each cidr, node mask size must be <= cidr mask
		for idx, cidr := range clusterCIDRs {
			mask := cidr.Mask
			if maskSize, _ := mask.Size(); maskSize > nodeCIDRMaskSizes[idx] {
				klog.Fatal("Controller: Invalid --cluster-cidr, mask size of cluster CIDR must be less than or equal to --node-cidr-mask-size configured for CIDR family")
			}
		}
	}

	ic := &Controller{
		cloud:                cloud,
		kubeClient:           kubeClient,
		lookupIP:             net.LookupIP,
		clusterCIDRs:         clusterCIDRs,
		serviceCIDR:          serviceCIDR,
		secondaryServiceCIDR: secondaryServiceCIDR,
		allocatorType:        allocatorType,
	}

	// TODO: Abstract this check into a generic controller manager should run method.
	var err error

	allocatorParams := ipam.CIDRAllocatorParams{
		ClusterCIDRs:         clusterCIDRs,
		ServiceCIDR:          ic.serviceCIDR,
		SecondaryServiceCIDR: ic.secondaryServiceCIDR,
		NodeCIDRMaskSizes:    nodeCIDRMaskSizes,
	}

	ic.cidrAllocator, err = ipam.New(kubeClient, cloud, nodeInformer, ic.allocatorType, allocatorParams)
	if err != nil {
		return nil, err
	}

	ic.nodeLister = nodeInformer.Lister()
	ic.nodeInformerSynced = nodeInformer.Informer().HasSynced

	return ic, nil
}

// Run starts an asynchronous loop that monitors the status of cluster nodes.
func (nc *Controller) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting ipam controller")
	defer klog.Infof("Shutting down ipam controller")

	if !cache.WaitForNamedCacheSync("node", ctx.Done(), nc.nodeInformerSynced) {
		return
	}

	nc.cidrAllocator.Run(ctx)
}
