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
	"net/netip"
	"strings"

	v1 "k8s.io/api/core/v1"
)

// An inbound unit is the Public IP, Service load balancer, backend pool and ServiceGateway service
// that serve one IP family of a LoadBalancer Service. A Service load balancer has a single backend
// pool and that pool receives every address of its ServiceGateway service, so a dual-stack Service
// gets one unit per family. The primary unit (ipFamilies[0]) is named after the Service UID, exactly
// as a single-stack Service is; the secondary unit adds a family suffix.
const (
	secondaryUnitSuffixIPv4 = "-v4"
	secondaryUnitSuffixIPv6 = "-v6"
)

// InboundUnit names one unit of a Service and the IP family it serves.
type InboundUnit struct {
	Name    string
	Family  v1.IPFamily
	Primary bool
}

// InboundUnitConfig is the admitted configuration of one unit of a Service.
type InboundUnitConfig struct {
	Unit   InboundUnit
	Config *InboundConfig
}

// InboundUnits returns the units of a Service: the primary one, plus a secondary one for dual-stack.
func InboundUnits(service *v1.Service) []InboundUnit {
	if service == nil {
		return nil
	}
	uid := ServiceUID(service)
	families := service.Spec.IPFamilies
	primary := v1.IPv4Protocol
	if len(families) > 0 {
		primary = families[0]
	}
	units := []InboundUnit{{Name: uid, Family: primary, Primary: true}}
	if len(families) > 1 && families[1] != primary {
		units = append(units, InboundUnit{Name: SecondaryUnitName(uid, families[1]), Family: families[1]})
	}
	return units
}

// SecondaryUnitName returns the name of the unit serving family as a Service's secondary family.
func SecondaryUnitName(serviceUID string, family v1.IPFamily) string {
	if family == v1.IPv6Protocol {
		return serviceUID + secondaryUnitSuffixIPv6
	}
	return serviceUID + secondaryUnitSuffixIPv4
}

// SecondaryUnitNames returns every name a secondary unit of the Service can have.
func SecondaryUnitNames(serviceUID string) []string {
	return []string{serviceUID + secondaryUnitSuffixIPv4, serviceUID + secondaryUnitSuffixIPv6}
}

// ParentServiceUID returns the UID of the Service a unit belongs to, and whether the unit is a
// secondary one. Any other name, including a primary unit, is returned unchanged.
func ParentServiceUID(name string) (serviceUID string, secondary bool) {
	for _, suffix := range []string{secondaryUnitSuffixIPv4, secondaryUnitSuffixIPv6} {
		if parent, ok := strings.CutSuffix(strings.ToLower(name), suffix); ok && isValidServiceUUID(parent) {
			return parent, true
		}
	}
	return name, false
}

// IsInboundUnitName reports whether name is the name of a Service's primary or secondary unit.
func IsInboundUnitName(name string) bool {
	parent, _ := ParentServiceUID(name)
	return isValidServiceUUID(parent)
}

// unitForAddress returns the unit of the Service that serves the family of address, if any.
func unitForAddress(serviceUID string, families []v1.IPFamily, address string) (string, bool) {
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return "", false
	}
	family := v1.IPv4Protocol
	if addr.Unmap().Is6() {
		family = v1.IPv6Protocol
	}
	for i, served := range families {
		if served != family {
			continue
		}
		if i == 0 {
			return serviceUID, true
		}
		return SecondaryUnitName(serviceUID, family), true
	}
	return "", false
}

// setInboundFamiliesLocked records the IP families a Service is provisioned for (IPv4 when it has no
// spec.ipFamilies, as its Public IP is). Must be called with dt.mu held.
func (dt *DiffTracker) setInboundFamiliesLocked(service *v1.Service) {
	serviceUID := ServiceUID(service)
	if serviceUID == "" {
		return
	}
	if dt.inboundFamilies == nil {
		dt.inboundFamilies = make(map[string][]v1.IPFamily)
	}
	dt.inboundFamilies[serviceUID] = servedFamilies(service)
}

// forgetInboundFamiliesLocked drops the IP families of a Service. Must be called with dt.mu held.
func (dt *DiffTracker) forgetInboundFamiliesLocked(serviceUID string) {
	delete(dt.inboundFamilies, strings.ToLower(serviceUID))
}

type endpointDelta struct {
	oldAddresses map[string]string
	newAddresses map[string]string
}

// endpointDeltasByUnitLocked splits an endpoint delta of a Service into one delta per unit, by the IP
// family of each address. Addresses of a family the Service does not serve are dropped; a Service not
// admitted yet keeps the whole delta. Must be called with dt.mu held.
func (dt *DiffTracker) endpointDeltasByUnitLocked(serviceUID string, oldAddresses, newAddresses map[string]string) map[string]*endpointDelta {
	families, known := dt.inboundFamilies[strings.ToLower(serviceUID)]
	if !known {
		return map[string]*endpointDelta{serviceUID: {oldAddresses: oldAddresses, newAddresses: newAddresses}}
	}
	deltas := map[string]*endpointDelta{}
	deltaOf := func(unit string) *endpointDelta {
		if deltas[unit] == nil {
			deltas[unit] = &endpointDelta{oldAddresses: map[string]string{}, newAddresses: map[string]string{}}
		}
		return deltas[unit]
	}
	for podIP, nodeIP := range oldAddresses {
		if unit, ok := unitForAddress(serviceUID, families, podIP); ok {
			deltaOf(unit).oldAddresses[podIP] = nodeIP
		}
	}
	for podIP, nodeIP := range newAddresses {
		if unit, ok := unitForAddress(serviceUID, families, podIP); ok {
			deltaOf(unit).newAddresses[podIP] = nodeIP
		}
	}
	return deltas
}

// unitEndpointsLocked returns the addresses, out of a Service's addresses, that belong to unit. Must be
// called with dt.mu held.
func (dt *DiffTracker) unitEndpointsLocked(unit string, addresses map[string]string) map[string]string {
	parent, _ := ParentServiceUID(unit)
	families, known := dt.inboundFamilies[parent]
	if !known {
		return addresses
	}
	filtered := map[string]string{}
	for podIP, nodeIP := range addresses {
		if owner, ok := unitForAddress(parent, families, podIP); ok && strings.EqualFold(owner, unit) {
			filtered[podIP] = nodeIP
		}
	}
	return filtered
}

// inboundUnitConfig returns the configuration of the named unit of the Service, or nil when the Service
// does not have that unit (any more).
func inboundUnitConfig(service *v1.Service, unitName string) *InboundConfig {
	for _, unit := range InboundUnits(service) {
		if strings.EqualFold(unit.Name, unitName) {
			return extractInboundUnitConfig(service, unit)
		}
	}
	return nil
}

// unitFamily returns the family the named unit of the Service serves. A unit the Service does not have is
// treated as the primary one.
func unitFamily(service *v1.Service, unitName string) v1.IPFamily {
	units := InboundUnits(service)
	if len(units) == 0 {
		return v1.IPv4Protocol
	}
	family := units[0].Family
	for _, unit := range units {
		if strings.EqualFold(unit.Name, unitName) {
			family = unit.Family
		}
	}
	return family
}

func currentInboundUnitFamily(service *v1.Service, unitName string) (v1.IPFamily, bool) {
	for _, unit := range InboundUnits(service) {
		if strings.EqualFold(unit.Name, unitName) {
			return unit.Family, true
		}
	}
	return "", false
}

// hasIngressIPOfUnit reports whether the Service's status has an ingress IP of the family the named unit serves.
func hasIngressIPOfUnit(service *v1.Service, unitName string) bool {
	family := unitFamily(service, unitName)
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		if addr, err := netip.ParseAddr(ingress.IP); err == nil && addr.Unmap().Is6() == (family == v1.IPv6Protocol) {
			return true
		}
	}
	return false
}

// IsInboundServiceTracked reports whether any unit of the Service is tracked.
func (dt *DiffTracker) IsInboundServiceTracked(serviceUID string) bool {
	if dt.IsServiceTracked(serviceUID) {
		return true
	}
	for _, name := range SecondaryUnitNames(serviceUID) {
		if dt.inboundUnitTracked(name) {
			return true
		}
	}
	return false
}

// inboundUnitTracked reports whether name is tracked as an inbound unit. An egress identity may carry the
// same name as a secondary unit and is never one.
func (dt *DiffTracker) inboundUnitTracked(name string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.inboundUnitTrackedLocked(name)
}

func (dt *DiffTracker) inboundUnitTrackedLocked(name string) bool {
	if op, ok := dt.pendingServiceOps[name]; ok {
		return op.Config.IsInbound
	}
	return dt.NRPResources.LoadBalancers.Has(name)
}

// isEgressIdentity reports whether name is tracked as an egress identity.
func (dt *DiffTracker) isEgressIdentity(name string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if op, ok := dt.pendingServiceOps[name]; ok {
		return !op.Config.IsInbound
	}
	return dt.NRPResources.NATGateways.Has(name)
}

// secondaryUnitLeft returns a secondary unit the primary unit of a Service that goes away must wait for:
// one that still exists. A primary that is recreated afterwards (the Service came back) waits for none.
func (dt *DiffTracker) secondaryUnitLeft(primary string) (string, bool) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if op := dt.pendingServiceOps[primary]; op != nil && op.RecreateAfterDeletion {
		return "", false
	}
	for _, name := range SecondaryUnitNames(primary) {
		if dt.inboundUnitTrackedLocked(name) {
			return name, true
		}
	}
	return "", false
}

// nameUsedBy reports whether name is used by a Service's unit (inbound) or by an egress identity, desired in
// Kubernetes or registered with the ServiceGateway.
func (dt *DiffTracker) nameUsedBy(name string, inbound bool) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if inbound {
		return dt.K8sResources.Services.Has(name) || dt.NRPResources.LoadBalancers.Has(name)
	}
	return dt.K8sResources.Egresses.Has(name) || dt.NRPResources.NATGateways.Has(name)
}
