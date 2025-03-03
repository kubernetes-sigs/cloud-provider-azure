package difftracker

import (
	"fmt"

	"k8s.io/klog/v2"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func (dt *DiffTrackerState) UpdateNRPLoadBalancers() {
	for _, service := range dt.K8s.Services.UnsortedList() {
		if dt.NRP.LoadBalancers.Has(service) {
			continue
		}
		dt.NRP.LoadBalancers.Insert(service)
		klog.V(2).Infof("Added service %s to NRP LoadBalancers", service)
	}

	for _, service := range dt.NRP.LoadBalancers.UnsortedList() {
		if dt.K8s.Services.Has(service) {
			continue
		}
		dt.NRP.LoadBalancers.Delete(service)
		klog.V(2).Infof("Removed service %s from NRP LoadBalancers", service)
	}
}

func (dt *DiffTrackerState) UpdateNRPNATGateways() {
	for _, egress := range dt.K8s.Egresses.UnsortedList() {
		if dt.NRP.NATGateways.Has(egress) {
			continue
		}
		dt.NRP.NATGateways.Insert(egress)
		fmt.Printf("Added egress %s to NRP NATGateways\n", egress)
	}

	for _, egress := range dt.NRP.NATGateways.UnsortedList() {
		if dt.K8s.Egresses.Has(egress) {
			continue
		}
		dt.NRP.NATGateways.Delete(egress)
		fmt.Printf("Removed egress %s from NRP NATGateways\n", egress)
	}
}

func (dt *DiffTrackerState) UpdateNRPLocationsAddresses() {
	// Update DiffTracker.NRPState
	for nodeIp, node := range dt.K8s.Nodes {
		nrpLocation, exists := dt.NRP.NRPLocations[nodeIp]
		if !exists {
			nrpLocation = NRPLocation{
				NRPAddresses: make(map[string]NRPAddress),
			}
			dt.NRP.NRPLocations[nodeIp] = nrpLocation
		}
		for address, pod := range node.Pods {
			nrpAddress := NRPAddress{
				NRPServices: utilsets.NewString(),
			}
			nrpLocation.NRPAddresses[address] = nrpAddress

			for _, identity := range pod.InboundIdentities.UnsortedList() {
				nrpAddress.NRPServices.Insert(identity)
			}
			if pod.PublicOutboundIdentity != "" {
				nrpAddress.NRPServices.Insert(pod.PublicOutboundIdentity)
			}
			if pod.PrivateOutboundIdentity != "" {
				nrpAddress.NRPServices.Insert(pod.PrivateOutboundIdentity)
			}
		}
	}

	for location, nrpLocation := range dt.NRP.NRPLocations {
		node, exists := dt.K8s.Nodes[location]
		if !exists {
			delete(dt.NRP.NRPLocations, location)
			continue
		}
		for address := range nrpLocation.NRPAddresses {
			_, exists := node.Pods[address]
			if !exists {
				delete(nrpLocation.NRPAddresses, address)
			}

		}
		if len(nrpLocation.NRPAddresses) == 0 {
			delete(dt.NRP.NRPLocations, location)
		}
	}
}
