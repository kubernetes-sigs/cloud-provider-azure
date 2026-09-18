/*
Copyright 2024 The Kubernetes Authors.

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
	"net/netip"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer"
	fnutil "sigs.k8s.io/cloud-provider-azure/pkg/util/collectionutil"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// filterServicesSharingDestinations returns the Services that may have rules for any of the
// destinations. disableFloatingIP means destinations can hold backend node IPs. The ingress status
// does not carry the nodes a Service targets, so every Service with the floating IP disabled is
// taken instead.
func filterServicesSharingDestinations(services []*v1.Service, destinations []netip.Addr, disableFloatingIP bool) []*v1.Service {
	targetIPs := fnutil.Map(func(ip netip.Addr) string { return ip.String() }, destinations)

	return fnutil.Filter(func(svc *v1.Service) bool {
		if disableFloatingIP && consts.IsK8sServiceDisableLoadBalancerFloatingIP(svc) {
			return true
		}

		ingressIPs := fnutil.Map(func(ing v1.LoadBalancerIngress) string { return ing.IP }, svc.Status.LoadBalancer.Ingress)

		ingressIPs = fnutil.Filter(func(ip string) bool { return ip != "" }, ingressIPs)

		return len(fnutil.Intersection(ingressIPs, targetIPs)) > 0
	}, services)
}

// backendNodeIPsOfService returns the IPs of the nodes backing svc under multiple standard load
// balancers, where each service no longer shares one cluster-wide backend pool. The result is
// empty when the placement of svc is not known yet.
func (az *Cloud) backendNodeIPsOfService(svc *v1.Service) []netip.Addr {
	var nodeNames *utilsets.IgnoreCaseSet
	if isLocalService(svc) {
		nodeNames = az.getLocalServiceEndpointsNodeNames(svc)
	} else {
		for _, multiSLBConfig := range az.MultipleStandardLoadBalancerConfigurations {
			if az.isLoadBalancerInUseByService(svc, multiSLBConfig) {
				nodeNames = az.getActiveNodesByLoadBalancerName(multiSLBConfig.Name)
				break
			}
		}
	}

	rv := make([]netip.Addr, 0, nodeNames.Len())
	for _, nodeName := range nodeNames.UnsortedList() {
		for _, ip := range az.nodePrivateIPs[strings.ToLower(nodeName)].UnsortedList() {
			if addr, err := netip.ParseAddr(ip); err == nil {
				rv = append(rv, addr)
			}
		}
	}
	return rv
}

// listSharedIPPortMapping lists the shared IP port mapping and the shared deny all destinations for
// the service excluding the service itself.
// There are scenarios where multiple services share the same public IP,
// and in order to clean up the security rules, we need to know the port mapping of the shared IP.
// The deny all rules are shared the same way, so a destination has to survive the cleanup while any
// of those services still requires it. backendNodeIPs are the nodes behind svc.
func (az *Cloud) listSharedIPPortMapping(
	ctx context.Context,
	svc *v1.Service,
	dstAddresses []netip.Addr,
	backendNodeIPs []netip.Addr,
) (map[armnetwork.SecurityRuleProtocol][]int32, []netip.Addr, error) {
	var (
		logger = log.FromContextOrBackground(ctx).WithName("listSharedIPPortMapping")
		rv     = make(map[armnetwork.SecurityRuleProtocol][]int32)

		isDstAddress = make(map[netip.Addr]bool, len(dstAddresses))
		// The same address may be appended more than once. SetDestinationPrefixes will deduplicate.
		denyAllDestinations []netip.Addr
	)
	for _, addr := range dstAddresses {
		isDstAddress[addr] = true
	}

	// Under a single load balancer every service shares one backend pool, otherwise each one has
	// its own.
	sharedBackendNodeIPs := func(s *v1.Service) []netip.Addr {
		if !az.UseMultipleStandardLoadBalancers() {
			return backendNodeIPs
		}
		return az.backendNodeIPsOfService(s)
	}

	var services []*v1.Service
	{
		var err error
		logger.V(5).Info("Listing all services")
		services, err = az.serviceLister.List(labels.Everything())
		if err != nil {
			logger.Error(err, "Failed to list all services")
			return nil, nil, fmt.Errorf("list all services: %w", err)
		}
		logger.V(5).Info("Listed all services", "num-all-services", len(services))

		// Filter services by ingress IPs, or by the floating IP being disabled when svc disables it
		disableFloatingIP := consts.IsK8sServiceDisableLoadBalancerFloatingIP(svc)
		if disableFloatingIP {
			logger.V(5).Info("Filter service by disableFloatingIP")
		} else {
			logger.V(5).Info("Filter service by external IPs")
		}
		services = filterServicesSharingDestinations(services, dstAddresses, disableFloatingIP)
	}
	logger.V(5).Info("Filtered services", "num-filtered-services", len(services))

	for _, s := range services {
		logger.V(5).Info("Iterating service", "service", s.Name, "namespace", s.Namespace)
		if svc.Namespace == s.Namespace && svc.Name == s.Name {
			// skip the service itself
			continue
		}

		portsByProtocol, err := loadbalancer.SecurityRuleDestinationPortsByProtocol(s)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch security rule dst ports for %s: %w", s.Name, err)
		}

		for protocol, ports := range portsByProtocol {
			rv[protocol] = append(rv[protocol], ports...)
		}

		if consts.IsK8sServiceDisableLoadBalancerNSGRule(s) || !loadbalancer.RequiresDenyAllExceptSourceRanges(s) {
			continue
		}

		if additionalIPs, err := loadbalancer.AdditionalPublicIPs(s); err == nil {
			for _, addr := range additionalIPs {
				if isDstAddress[addr] {
					denyAllDestinations = append(denyAllDestinations, addr)
				}
			}
		}

		// A service that disables the floating IP has no rule for its frontend IP; its rules
		// target the nodes backing it instead.
		if consts.IsK8sServiceDisableLoadBalancerFloatingIP(s) {
			for _, addr := range sharedBackendNodeIPs(s) {
				if isDstAddress[addr] {
					denyAllDestinations = append(denyAllDestinations, addr)
				}
			}
			continue
		}

		for _, ing := range s.Status.LoadBalancer.Ingress {
			if addr, err := netip.ParseAddr(ing.IP); err == nil && isDstAddress[addr] {
				denyAllDestinations = append(denyAllDestinations, addr)
			}
		}
	}

	logger.V(5).Info("Retain port mapping", "port-mapping", rv, "deny-all-destinations", denyAllDestinations)

	return rv, denyAllDestinations, nil
}

func (az *Cloud) listAvailableSecurityGroupDestinations(_ context.Context) ([]netip.Addr, error) {
	services, err := az.serviceLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("list all services: %w", err)
	}

	nodes, err := az.nodeLister.List(labels.NewSelector())
	if err != nil {
		return nil, fmt.Errorf("list all nodes: %w", err)
	}

	var rv []netip.Addr
	for _, svc := range services {
		// Add additional public IPs
		{
			ips, err := loadbalancer.AdditionalPublicIPs(svc)
			if err == nil {
				rv = append(rv, ips...)
			}
		}

		// Add ingress IPs
		{
			for _, ing := range svc.Status.LoadBalancer.Ingress {
				ip, err := netip.ParseAddr(ing.IP)
				if err == nil {
					rv = append(rv, ip)
				}
			}
		}
	}

	// Add backend node IPs
	{
		for _, node := range nodes {
			if !az.isNodeManagedByCloudProvider(node) {
				continue
			}
			for _, addr := range node.Status.Addresses {
				if addr.Type != v1.NodeInternalIP {
					continue
				}
				ip, err := netip.ParseAddr(addr.Address)
				if err == nil {
					rv = append(rv, ip)
				}
			}
		}
	}

	return rv, nil
}

func (az *Cloud) isNodeManagedByCloudProvider(node *v1.Node) bool {
	az.nodeCachesLock.Lock()
	defer az.nodeCachesLock.Unlock()

	return !az.unmanagedNodes.Has(node.Name)
}
