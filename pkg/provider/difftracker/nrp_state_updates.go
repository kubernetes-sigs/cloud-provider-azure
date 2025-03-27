package difftracker

import (
	"fmt"

	"k8s.io/klog/v2"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func (dt *DiffTracker) UpdateNRPLoadBalancers() {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	for _, service := range dt.K8sResources.Services.UnsortedList() {
		if dt.NRPResources.LoadBalancers.Has(service) {
			continue
		}
		dt.NRPResources.LoadBalancers.Insert(service)
		klog.V(2).Infof("Added service %s to NRP LoadBalancers", service)
	}

	for _, service := range dt.NRPResources.LoadBalancers.UnsortedList() {
		if dt.K8sResources.Services.Has(service) {
			continue
		}
		dt.NRPResources.LoadBalancers.Delete(service)
		klog.V(2).Infof("Removed service %s from NRP LoadBalancers", service)
	}
}

func (dt *DiffTracker) UpdateNRPNATGateways() {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	for _, egress := range dt.K8sResources.Egresses.UnsortedList() {
		if dt.NRPResources.NATGateways.Has(egress) {
			continue
		}
		dt.NRPResources.NATGateways.Insert(egress)
		fmt.Printf("Added egress %s to NRP NATGateways\n", egress)
	}

	for _, egress := range dt.NRPResources.NATGateways.UnsortedList() {
		if dt.K8sResources.Egresses.Has(egress) {
			continue
		}
		dt.NRPResources.NATGateways.Delete(egress)
		fmt.Printf("Removed egress %s from NRP NATGateways\n", egress)
	}
}

func (dt *DiffTracker) UpdateLocationsAddresses() {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	// Update DiffTracker.NRP
	for nodeIp, node := range dt.K8sResources.Nodes {
		nrpLocation, exists := dt.NRPResources.Locations[nodeIp]
		if !exists {
			nrpLocation = NRPLocation{
				Addresses: make(map[string]NRPAddress),
			}
			dt.NRPResources.Locations[nodeIp] = nrpLocation
		}
		for address, pod := range node.Pods {
			nrpAddress := NRPAddress{
				Services: utilsets.NewString(),
			}
			nrpLocation.Addresses[address] = nrpAddress

			for _, identity := range pod.InboundIdentities.UnsortedList() {
				nrpAddress.Services.Insert(identity)
			}
			if pod.PublicOutboundIdentity != "" {
				nrpAddress.Services.Insert(pod.PublicOutboundIdentity)
			}
			if pod.PrivateOutboundIdentity != "" {
				nrpAddress.Services.Insert(pod.PrivateOutboundIdentity)
			}
		}
	}

	for location, nrpLocation := range dt.NRPResources.Locations {
		node, exists := dt.K8sResources.Nodes[location]
		if !exists {
			delete(dt.NRPResources.Locations, location)
			continue
		}
		for address := range nrpLocation.Addresses {
			_, exists := node.Pods[address]
			if !exists {
				delete(nrpLocation.Addresses, address)
			}

		}
		if len(nrpLocation.Addresses) == 0 {
			delete(dt.NRPResources.Locations, location)
		}
	}
}
