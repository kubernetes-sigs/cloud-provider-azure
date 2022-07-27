/*
Copyright 2018 The Kubernetes Authors.

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

// Package app implements a server that runs a set of active
// components.  This includes node controllers, service and
// route controller, and so on.
package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	cloudprovider "k8s.io/cloud-provider"
	nodecontroller "k8s.io/cloud-provider/controllers/node"
	nodelifecyclecontroller "k8s.io/cloud-provider/controllers/nodelifecycle"
	routecontroller "k8s.io/cloud-provider/controllers/route"
	servicecontroller "k8s.io/cloud-provider/controllers/service"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"

	cloudcontrollerconfig "sigs.k8s.io/cloud-provider-azure/cmd/cloud-controller-manager/app/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	nodeipamcontroller "sigs.k8s.io/cloud-provider-azure/pkg/nodeipam"
	nodeipamconfig "sigs.k8s.io/cloud-provider-azure/pkg/nodeipam/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/nodeipam/ipam"
)

func startCloudNodeController(ctx context.Context, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface, stopCh <-chan struct{}) (http.Handler, bool, error) {
	// Start the CloudNodeController
	nodeController, err := nodecontroller.NewCloudNodeController(
		completedConfig.SharedInformers.Core().V1().Nodes(),
		// cloud node controller uses existing cluster role from node-controller
		completedConfig.ClientBuilder.ClientOrDie("node-controller"),
		cloud,
		completedConfig.ComponentConfig.NodeStatusUpdateFrequency.Duration,
	)
	if err != nil {
		klog.Warningf("failed to start cloud node controller: %s", err)
		return nil, false, nil
	}

	go nodeController.Run(stopCh)

	return nil, true, nil
}

func startCloudNodeLifecycleController(ctx context.Context, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface, stopCh <-chan struct{}) (http.Handler, bool, error) {
	// Start the cloudNodeLifecycleController
	cloudNodeLifecycleController, err := nodelifecyclecontroller.NewCloudNodeLifecycleController(
		completedConfig.SharedInformers.Core().V1().Nodes(),
		// cloud node lifecycle controller uses existing cluster role from node-controller
		completedConfig.ClientBuilder.ClientOrDie("node-controller"),
		cloud,
		completedConfig.ComponentConfig.KubeCloudShared.NodeMonitorPeriod.Duration,
	)
	if err != nil {
		klog.Warningf("failed to start cloud node lifecycle controller: %s", err)
		return nil, false, nil
	}

	go cloudNodeLifecycleController.Run(ctx)

	return nil, true, nil
}

func startServiceController(ctx context.Context, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface, stopCh <-chan struct{}) (http.Handler, bool, error) {
	// Start the service controller
	serviceController, err := servicecontroller.New(
		cloud,
		completedConfig.ClientBuilder.ClientOrDie("service-controller"),
		completedConfig.SharedInformers.Core().V1().Services(),
		completedConfig.SharedInformers.Core().V1().Nodes(),
		completedConfig.ComponentConfig.KubeCloudShared.ClusterName,
		utilfeature.DefaultFeatureGate,
	)
	if err != nil {
		// This error shouldn't fail. It lives like this as a legacy.
		klog.Errorf("Failed to start service controller: %v", err)
		return nil, false, nil
	}

	go serviceController.Run(ctx, int(completedConfig.ComponentConfig.ServiceController.ConcurrentServiceSyncs))

	return nil, true, nil
}

func startRouteController(ctx context.Context, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface, stopCh <-chan struct{}) (http.Handler, bool, error) {
	if !completedConfig.ComponentConfig.KubeCloudShared.ConfigureCloudRoutes {
		klog.Infof("Will not configure cloud provider routes, --configure-cloud-routes: %v.", completedConfig.ComponentConfig.KubeCloudShared.ConfigureCloudRoutes)
		return nil, false, nil
	}

	// If CIDRs should be allocated for pods and set on the CloudProvider, then start the route controller
	routes, ok := cloud.Routes()
	if !ok {
		klog.Warning("--configure-cloud-routes is set, but cloud provider does not support routes. Will not configure cloud provider routes.")
		return nil, false, nil
	}

	// failure: bad cidrs in config
	clusterCIDRs, dualStack, err := processCIDRs(completedConfig.ComponentConfig.KubeCloudShared.ClusterCIDR)
	if err != nil {
		return nil, false, err
	}

	// failure: more than one cidr but they are not configured as dual stack
	if len(clusterCIDRs) > 1 && !dualStack {
		return nil, false, fmt.Errorf("len of ClusterCIDRs==%v and they are not configured as dual stack (at least one from each IPFamily", len(clusterCIDRs))
	}

	// failure: more than cidrs is not allowed even with dual stack
	if len(clusterCIDRs) > 2 {
		return nil, false, fmt.Errorf("length of clusterCIDRs is:%v more than max allowed of 2", len(clusterCIDRs))
	}

	routeController := routecontroller.New(
		routes,
		completedConfig.ClientBuilder.ClientOrDie("route-controller"),
		completedConfig.SharedInformers.Core().V1().Nodes(),
		completedConfig.ComponentConfig.KubeCloudShared.ClusterName,
		clusterCIDRs,
	)
	go routeController.Run(ctx, completedConfig.ComponentConfig.KubeCloudShared.RouteReconciliationPeriod.Duration)

	return nil, true, nil
}

func startNodeIpamController(ctx context.Context, completedConfig *cloudcontrollerconfig.CompletedConfig, cloud cloudprovider.Interface, stopCh <-chan struct{}) (http.Handler, bool, error) {
	var serviceCIDR *net.IPNet
	var secondaryServiceCIDR *net.IPNet

	// should we start nodeIPAM
	if !completedConfig.ComponentConfig.KubeCloudShared.AllocateNodeCIDRs {
		return nil, false, nil
	}

	// failure: bad cidrs in config
	clusterCIDRs, dualStack, err := processCIDRs(completedConfig.ComponentConfig.KubeCloudShared.ClusterCIDR)
	if err != nil {
		return nil, false, err
	}

	// failure: more than one cidr but they are not configured as dual stack
	if len(clusterCIDRs) > 1 && !dualStack {
		return nil, false, fmt.Errorf("len of ClusterCIDRs==%v and they are not configured as dual stack (at least one from each IPFamily", len(clusterCIDRs))
	}

	// failure: more than cidrs is not allowed even with dual stack
	if len(clusterCIDRs) > 2 {
		return nil, false, fmt.Errorf("len of clusters is:%v > more than max allowed of 2", len(clusterCIDRs))
	}

	// service cidr processing
	if len(strings.TrimSpace(completedConfig.NodeIPAMControllerConfig.ServiceCIDR)) != 0 {
		_, serviceCIDR, err = net.ParseCIDR(completedConfig.NodeIPAMControllerConfig.ServiceCIDR)
		if err != nil {
			klog.Warningf("Unsuccessful parsing of service CIDR %v: %v", completedConfig.NodeIPAMControllerConfig.ServiceCIDR, err)
		}
	}

	if len(strings.TrimSpace(completedConfig.NodeIPAMControllerConfig.SecondaryServiceCIDR)) != 0 {
		_, secondaryServiceCIDR, err = net.ParseCIDR(completedConfig.NodeIPAMControllerConfig.SecondaryServiceCIDR)
		if err != nil {
			klog.Warningf("Unsuccessful parsing of service CIDR %v: %v", completedConfig.NodeIPAMControllerConfig.SecondaryServiceCIDR, err)
		}
	}

	// the following checks are triggered if both serviceCIDR and secondaryServiceCIDR are provided
	if serviceCIDR != nil && secondaryServiceCIDR != nil {
		// should be dual stack (from different IPFamilies)
		dualstackServiceCIDR, err := netutils.IsDualStackCIDRs([]*net.IPNet{serviceCIDR, secondaryServiceCIDR})
		if err != nil {
			return nil, false, fmt.Errorf("failed to perform dualstack check on serviceCIDR and secondaryServiceCIDR error:%w", err)
		}
		if !dualstackServiceCIDR {
			return nil, false, fmt.Errorf("serviceCIDR and secondaryServiceCIDR are not dualstack (from different IPfamiles)")
		}
	}

	nodeCIDRMaskSizeIPv4, nodeCIDRMaskSizeIPv6, err := setNodeCIDRMaskSizesDualStack(completedConfig.NodeIPAMControllerConfig)

	if err != nil {
		return nil, false, err
	}

	// get list of node cidr mask sizes
	nodeCIDRMaskSizes := getNodeCIDRMaskSizes(clusterCIDRs, nodeCIDRMaskSizeIPv4, nodeCIDRMaskSizeIPv6)

	nodeIpamController, err := nodeipamcontroller.NewNodeIpamController(
		completedConfig.SharedInformers.Core().V1().Nodes(),
		cloud,
		completedConfig.ClientBuilder.ClientOrDie("node-controller"),
		clusterCIDRs,
		serviceCIDR,
		secondaryServiceCIDR,
		nodeCIDRMaskSizes,
		ipam.CIDRAllocatorType(completedConfig.ComponentConfig.KubeCloudShared.CIDRAllocatorType),
	)
	if err != nil {
		return nil, true, err
	}
	go nodeIpamController.Run(stopCh)
	return nil, true, nil
}

// setNodeCIDRMaskSizesDualStack returns the IPv4 and IPv6 node cidr mask sizes to the value provided
// for --node-cidr-mask-size-ipv4 and --node-cidr-mask-size-ipv6 respectively. If value not provided,
// then it will return default IPv4 and IPv6 cidr mask sizes.
func setNodeCIDRMaskSizesDualStack(cfg nodeipamconfig.NodeIPAMControllerConfiguration) (int, int, error) {
	ipv4Mask, ipv6Mask := consts.DefaultNodeMaskCIDRIPv4, consts.DefaultNodeMaskCIDRIPv6

	if cfg.NodeCIDRMaskSize != 0 {
		klog.Warningf("setNodeCIDRMaskSizesDualStack: --node-cidr-mask-size is set to %d, but it would be ignored because the dualstack is enabled", cfg.NodeCIDRMaskSize)
	}

	if cfg.NodeCIDRMaskSizeIPv4 != 0 {
		ipv4Mask = int(cfg.NodeCIDRMaskSizeIPv4)
	}

	if cfg.NodeCIDRMaskSizeIPv6 != 0 {
		ipv6Mask = int(cfg.NodeCIDRMaskSizeIPv6)
	}

	return ipv4Mask, ipv6Mask, nil
}

// getNodeCIDRMaskSizes is a helper function that helps the generate the node cidr mask
// sizes slice based on the cluster cidr slice
func getNodeCIDRMaskSizes(clusterCIDRs []*net.IPNet, maskSizeIPv4, maskSizeIPv6 int) []int {
	nodeMaskCIDRs := make([]int, len(clusterCIDRs))

	for idx, clusterCIDR := range clusterCIDRs {
		if netutils.IsIPv6CIDR(clusterCIDR) {
			nodeMaskCIDRs[idx] = maskSizeIPv6
		} else {
			nodeMaskCIDRs[idx] = maskSizeIPv4
		}
	}
	return nodeMaskCIDRs
}

// processCIDRs is a helper function that works on a comma separated cidrs and returns
// a list of typed cidrs
// a flag if cidrs represents a dual stack
// error if failed to parse any of the cidrs
func processCIDRs(cidrsList string) ([]*net.IPNet, bool, error) {
	cidrsSplit := strings.Split(strings.TrimSpace(cidrsList), ",")

	cidrs, err := netutils.ParseCIDRs(cidrsSplit)
	if err != nil {
		return nil, false, err
	}

	// if cidrs has an error then the previous call will fail
	// safe to ignore error checking on next call
	dualstack, _ := netutils.IsDualStackCIDRs(cidrs)

	return cidrs, dualstack, nil
}
