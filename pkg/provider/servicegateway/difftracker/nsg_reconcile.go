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
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
)

type nsgLocationOwner struct {
	name       string
	uid        types.UID
	providerID string
	draining   bool
}

// nsgPreparedNIC holds one NIC's result for the current pass; each pass may handle many NICs.
type nsgPreparedNIC struct {
	owner            nsgLocationOwner
	nodeIPs          []string
	locations        []string
	currentLocations []string
	services         map[string]nsgService
	target           *arm.ResourceID
	group            *armnetwork.SecurityGroup
	lbOnly           bool
}

func (dt *DiffTracker) markNICPolicyChangeLocked(config ServiceConfig) {
	if !config.IsInbound {
		return
	}
	uid := strings.ToLower(config.ServiceName)
	if nsgPolicyEnabled(config.InboundConfig) {
		dt.mayHaveNICPolicies = true
	}
	if op := dt.pendingServiceOps[uid]; op != nil {
		if nsgPoliciesEqual(op.Config.InboundConfig, config.InboundConfig) {
			return
		}
		op.NSGPrepared = false
	}
	dt.markServiceNICPoliciesDirtyLocked(uid)
	if dt.dirtyNICPolicies.Len() > 0 {
		dt.triggerLocationsUpdater()
	}
}

func (dt *DiffTracker) markServiceNICPoliciesDirtyLocked(uid string) {
	if !dt.mayHaveNICPolicies {
		return
	}
	for location, node := range dt.K8sResources.Nodes {
		for _, pod := range node.Pods {
			if pod.InboundIdentities.Has(uid) {
				dt.dirtyNICPolicies.Insert(location)
				break
			}
		}
	}
	for _, resources := range []map[string]NRPLocation{dt.NRPResources.Locations, dt.pendingLocationUpdates} {
		for location, node := range resources {
			for _, address := range node.Addresses {
				if address.Services.Has(uid) {
					dt.dirtyNICPolicies.Insert(location)
					break
				}
			}
		}
	}
}

func nodeInternalIPs(node *v1.Node) []string {
	var ips []string
	if node != nil {
		for _, address := range node.Status.Addresses {
			if strings.EqualFold(string(address.Type), string(v1.NodeInternalIP)) {
				ips = append(ips, canonicalIP(address.Address))
			}
		}
	}
	slices.Sort(ips)
	return slices.Compact(ips)
}

// ReconcileNodeChange records NIC-policy identity changes before the existing IP replay.
func (dt *DiffTracker) ReconcileNodeChange(oldNode, newNode *v1.Node) {
	node := newNode
	if node == nil {
		node = oldNode
	}
	if dt == nil || node == nil || node.Name == "" {
		return
	}
	nodeName := node.Name
	oldNodeIPs, newNodeIPs := nodeInternalIPs(oldNode), nodeInternalIPs(newNode)
	if oldNode != nil && newNode != nil && oldNode.UID == newNode.UID &&
		oldNode.Spec.ProviderID == newNode.Spec.ProviderID && slices.Equal(oldNodeIPs, newNodeIPs) {
		return
	}

	dt.mu.Lock()
	if newNode == nil && dt.nodeLister != nil {
		if current, err := dt.nodeLister.Get(nodeName); err == nil && current.UID != oldNode.UID {
			dt.mu.Unlock()
			return
		}
	}
	if newNode == nil && oldNode != nil {
		for _, resources := range []map[string]NRPLocation{dt.NRPResources.Locations, dt.pendingLocationUpdates} {
			for _, location := range oldNodeIPs {
				owner := resources[location].nodeOwner
				if owner != nil && !owner.draining && owner.name == oldNode.Name && owner.uid != oldNode.UID {
					dt.mu.Unlock()
					return
				}
			}
		}
	}
	if oldNode != nil {
		dt.markPreviousLocationOwnerLocked(
			nsgLocationOwner{name: oldNode.Name, uid: oldNode.UID, providerID: oldNode.Spec.ProviderID}, oldNodeIPs, newNode)
	}
	if dt.mayHaveNICPolicies {
		locations := sets.New(oldNodeIPs...)
		locations.Insert(newNodeIPs...)
		for location := range locations {
			dt.dirtyNICPolicies.Insert(location)
			for _, pod := range dt.K8sResources.Nodes[location].Pods {
				for _, uid := range pod.InboundIdentities.UnsortedList() {
					if op := dt.pendingServiceOps[uid]; op != nil && op.Config.IsInbound {
						op.NSGPrepared = false
					}
				}
			}
			for _, resources := range []map[string]NRPLocation{dt.NRPResources.Locations, dt.pendingLocationUpdates} {
				for _, address := range resources[location].Addresses {
					for _, uid := range address.Services.UnsortedList() {
						if op := dt.pendingServiceOps[uid]; op != nil && op.Config.IsInbound {
							op.NSGPrepared = false
						}
					}
				}
			}
		}
		dt.triggerLocationsUpdater()
	}
	dt.mu.Unlock()
	if !slices.Equal(oldNodeIPs, newNodeIPs) {
		dt.ReconcileNodeIPChange(nodeName, oldNodeIPs, newNodeIPs)
	}
}

func (dt *DiffTracker) readyForLBUpdateLocked(uid string) bool {
	op := dt.pendingServiceOps[uid]
	if !dt.needsLBPreparationLocked(op) || op.NSGPrepared || !dt.serviceHasLocationsInNRP(uid) {
		return true
	}
	dt.markServiceNICPoliciesDirtyLocked(uid)
	dt.triggerLocationsUpdater()
	return false
}

func (dt *DiffTracker) needsLBPreparationLocked(op *ServiceOperationState) bool {
	if op == nil || !op.Config.IsInbound || op.Config.InboundConfig == nil || !dt.mayHaveNICPolicies ||
		!dt.NRPResources.LoadBalancers.Has(op.ServiceName) ||
		(op.State != StateNotStarted && op.State != StateUpdateInProgress) {
		return false
	}
	if op.LastAppliedConfig != nil {
		oldPorts, oldErr := nsgBackendPorts(nsgPortMappings(op.LastAppliedConfig.InboundConfig))
		newPorts, newErr := nsgBackendPorts(nsgPortMappings(op.Config.InboundConfig))
		if oldErr == nil && newErr == nil && maps.EqualFunc(oldPorts, newPorts, slices.Equal[[]int32]) {
			return false
		}
	}
	return true
}

func (dt *DiffTracker) completeLBWriteLocked(uid string) {
	if op := dt.pendingServiceOps[uid]; op != nil {
		op.RetainedBackendPorts = nil
		op.NSGPrepared = false
	}
	dt.markServiceNICPoliciesDirtyLocked(uid)
	if dt.dirtyNICPolicies.Len() > 0 {
		dt.triggerLocationsUpdater()
	}
}

func (dt *DiffTracker) nicPolicyContributionsLocked(currentLocations, retainedLocations []string, observed map[string]nsgService) map[string]nsgService {
	addresses := make(map[string]sets.Set[netip.Addr])
	add := func(uid, address string, published bool) {
		uid = strings.ToLower(uid)
		op := dt.pendingServiceOps[uid]
		if op != nil && !op.Config.IsInbound || op == nil && !dt.NRPResources.LoadBalancers.Has(uid) && observed[uid].ports == nil {
			return
		}
		if !published && (op == nil || op.Config.InboundConfig == nil || !dt.isServiceReadyToSync(uid, true)) {
			return
		}
		ip, err := netip.ParseAddr(address)
		if err != nil {
			return
		}
		if addresses[uid] == nil {
			addresses[uid] = sets.New[netip.Addr]()
		}
		addresses[uid].Insert(ip.Unmap())
	}
	for _, location := range currentLocations {
		for address, pod := range dt.K8sResources.Nodes[location].Pods {
			for _, uid := range pod.InboundIdentities.UnsortedList() {
				add(uid, address, false)
			}
		}
	}
	for _, location := range retainedLocations {
		for _, resources := range []map[string]NRPLocation{dt.NRPResources.Locations, dt.pendingLocationUpdates} {
			for address, item := range resources[location].Addresses {
				for _, uid := range item.Services.UnsortedList() {
					add(uid, address, true)
				}
			}
		}
	}
	if dt.locationRecoveryPending {
		for uid, service := range observed {
			for _, address := range service.addresses {
				add(uid, address.String(), true)
			}
		}
	}
	result := make(map[string]nsgService)
	for uid, ips := range addresses {
		service := observed[uid]
		if op := dt.pendingServiceOps[uid]; op != nil && op.Config.InboundConfig != nil {
			service.sources = op.Config.InboundConfig.AllowedIPRanges
			service.ports = nsgPortMappings(op.Config.InboundConfig)
			previous := observed[uid].ports
			if op.LastAppliedConfig != nil {
				previous = nsgPortMappings(op.LastAppliedConfig.InboundConfig)
			}
			portSets := [][]PortMapping{previous, op.RetainedBackendPorts}
			if op.InFlightConfig != nil {
				portSets = append(portSets, nsgPortMappings(op.InFlightConfig.InboundConfig))
			}
			for _, extra := range portSets {
				for _, port := range extra {
					if !slices.Contains(service.ports, port) {
						service.ports = append(service.ports, port)
					}
				}
			}
		}
		service.addresses = slices.SortedFunc(maps.Keys(ips), netip.Addr.Compare)
		slices.SortFunc(service.ports, func(a, b PortMapping) int {
			if order := strings.Compare(a.Protocol, b.Protocol); order != 0 {
				return order
			}
			return int(a.Port - b.Port)
		})
		result[uid] = service
	}
	return result
}

func (dt *DiffTracker) discoverNICPolicies(ctx context.Context, nodeIPs map[string][]string) error {
	groups, err := dt.networkClientFactory.GetSecurityGroupClient().List(ctx, dt.config.ResourceGroup)
	if err != nil {
		return fmt.Errorf("discover managed NIC NSGs: %w", err)
	}
	for _, group := range groups {
		if !dt.ownsClusterGroup(group) {
			continue
		}
		if _, err := dt.targetFromTags(group); err != nil {
			return err
		}
		if group.Properties == nil {
			return fmt.Errorf("managed NIC NSG has no properties")
		}
		if len(group.Properties.NetworkInterfaces) == 0 && len(group.Properties.Subnets) == 0 {
			deleted, err := dt.deleteOrphanGroup(ctx, *group.Name)
			if err != nil {
				return fmt.Errorf("clean startup NIC NSG %q: %w", *group.Name, err)
			}
			if deleted {
				continue
			}
		}
		if _, err := decodeNSGServices(group.Properties.SecurityRules); err != nil {
			return err
		}
		name := nsgTagValue(group.Tags, consts.ServiceGatewayNSGNodeTag)
		if name == "" {
			return fmt.Errorf("managed NIC NSG has no node identity")
		}
		dt.mayHaveNICPolicies, dt.locationRecoveryPending = true, true
		dt.dirtyNICPolicies.Insert(nodeIPs[name]...)
	}
	return nil
}

func (dt *DiffTracker) prepareNICPolicyInitialization(services map[string]*v1.Service, loadBalancers map[string]*armnetwork.LoadBalancer) {
	for uid, service := range services {
		if service == nil || service.DeletionTimestamp != nil || service.Spec.LoadBalancerClass != nil {
			continue
		}
		dt.mu.Lock()
		registered := dt.NRPResources.LoadBalancers.Has(uid)
		dt.mu.Unlock()
		if registered {
			inbound, err := AdmitInboundService(service)
			if err != nil {
				dt.logger.Error(err, "Kept a Service rejected by inbound admission without provisioning or changing it", "serviceUID", uid)
				continue
			}
			if inbound == nil {
				continue
			}
			config := NewInboundServiceConfig(uid, inbound)
			config.Namespace, config.Name = service.Namespace, service.Name
			if err := dt.AddService(config); err != nil {
				dt.logger.V(4).Info("Could not recover Service NIC policy", "service", uid, "err", err)
				continue
			}
		}
		dt.mu.Lock()
		if op := dt.pendingServiceOps[uid]; op != nil && op.Config.IsInbound {
			op.Config.Namespace, op.Config.Name = service.Namespace, service.Name
		}
		dt.mu.Unlock()
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	for name, lb := range loadBalancers {
		if op := dt.pendingServiceOps[name]; op != nil && op.Config.IsInbound {
			ports, err := loadBalancerBackendPorts(lb)
			if err != nil {
				dt.logger.V(4).Info("Could not recover LB backend ports; NIC preparation will retry", "service", name, "err", err)
			} else {
				op.RetainedBackendPorts = ports
			}
		}
	}
	dt.locationRecoveryPending = dt.mayHaveNICPolicies
}

func (dt *DiffTracker) takeNICPolicyWork() sets.Set[string] {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	work := dt.dirtyNICPolicies
	dt.dirtyNICPolicies = sets.New[string]()
	return work
}

func (dt *DiffTracker) prepareNICPolicies(ctx context.Context, work sets.Set[string]) (map[string]*nsgPreparedNIC, error) {
	result := make(map[string]*nsgPreparedNIC)
	locations := sets.List(work)
	if len(locations) == 0 {
		return result, nil
	}
	dt.mu.Lock()
	lister := dt.nodeLister
	aliases := make(map[string]nsgLocationOwner)
	for _, resources := range []map[string]NRPLocation{dt.NRPResources.Locations, dt.pendingLocationUpdates} {
		for location, current := range resources {
			if current.nodeOwner != nil {
				aliases[location] = *current.nodeOwner
			}
		}
	}
	dt.mu.Unlock()
	var nodes []*v1.Node
	var err error
	needLiveList := lister == nil
	if lister != nil {
		nodes, err = lister.List(labels.Everything())
		if err == nil {
			missing := work.Clone()
			for location := range aliases {
				missing.Delete(location)
			}
			for _, node := range nodes {
				missing.Delete(nodeInternalIPs(node)...)
			}
			needLiveList = missing.Len() > 0
		}
	}
	if err == nil && needLiveList {
		var list *v1.NodeList
		list, err = dt.kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err == nil {
			nodes = nil
			for index := range list.Items {
				nodes = append(nodes, &list.Items[index])
			}
		}
	}
	if err != nil {
		return result, fmt.Errorf("resolve NIC policy nodes: %w", err)
	}
	byName := make(map[string]*v1.Node)
	for _, node := range nodes {
		byName[node.Name] = node
	}
	var combined error
	unresolved := sets.New[string]()
	if lister != nil {
		checked := sets.New[string]()
		for location, owner := range aliases {
			node := byName[owner.name]
			if node != nil && node.UID == owner.uid && node.Spec.ProviderID == owner.providerID &&
				(owner.draining || slices.Contains(nodeInternalIPs(node), location)) {
				continue
			}
			if checked.Has(owner.name) {
				continue
			}
			checked.Insert(owner.name)
			live, err := dt.kubeClient.CoreV1().Nodes().Get(ctx, owner.name, metav1.GetOptions{})
			if err == nil {
				byName[owner.name] = live
			} else {
				delete(byName, owner.name)
				if !apierrors.IsNotFound(err) {
					unresolved.Insert(owner.name)
					err = fmt.Errorf("verify live Node %q before changing publication ownership: %w", owner.name, err)
					dt.reportNICPolicyError([]string{location}, err)
					combined = errors.Join(combined, err)
				}
			}
		}
	}
	byLocation := make(map[string]*v1.Node)
	ambiguous := sets.New[string]()
	for _, node := range byName {
		for _, ip := range nodeInternalIPs(node) {
			if previous := byLocation[ip]; previous != nil && previous.UID != node.UID {
				ambiguous.Insert(ip)
			}
			byLocation[ip] = node
		}
	}
	for location := range ambiguous {
		delete(byLocation, location)
	}
	for location, owner := range aliases {
		if node := byName[owner.name]; node != nil && node.UID == owner.uid && node.Spec.ProviderID == owner.providerID {
			byLocation[location] = node
		}
	}
	dt.mu.Lock()
	for location, owner := range aliases {
		if !unresolved.Has(owner.name) {
			dt.markPreviousLocationOwnerLocked(owner, []string{location}, byName[owner.name])
		}
	}
	dt.mu.Unlock()
	selected := make(map[string][]string)
	for _, location := range locations {
		if unresolved.Has(aliases[location].name) {
			continue
		}
		if ambiguous.Has(location) {
			err := fmt.Errorf("source-policy location %q has multiple current Node owners", location)
			dt.reportNICPolicyError([]string{location}, err)
			combined = errors.Join(combined, err)
		}
		if node := byLocation[location]; node != nil {
			selected[node.Name] = nil
		} else {
			dt.mu.Lock()
			needed := len(dt.nicPolicyContributionsLocked([]string{location}, nil, nil)) > 0
			dt.mu.Unlock()
			if needed {
				err := fmt.Errorf("no current node identity for source-policy location %q", location)
				dt.reportNICPolicyError([]string{location}, err)
				combined = errors.Join(combined, err)
			} else {
				work.Delete(location)
			}
		}
	}
	for location, owner := range byLocation {
		if _, needed := selected[owner.Name]; needed {
			selected[owner.Name] = append(selected[owner.Name], location)
		}
	}
	portReads := make(map[string]error)
	for _, name := range slices.Sorted(maps.Keys(selected)) {
		node := byName[name]
		ownedLocations := selected[name]
		currentLocations := nodeInternalIPs(node)
		currentLocations = slices.DeleteFunc(currentLocations, ambiguous.Has)
		work.Insert(ownedLocations...)
		work.Insert(currentLocations...)
		if node.Spec.ProviderID == "" {
			dt.mu.Lock()
			uninvolved := len(dt.nicPolicyContributionsLocked(currentLocations, ownedLocations, nil)) == 0
			dt.mu.Unlock()
			if uninvolved {
				work.Delete(ownedLocations...)
				work.Delete(currentLocations...)
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return result, errors.Join(combined, err)
		}
		mc := metrics.NewMetricContext("nsg", "reconcile_nic", dt.config.ResourceGroup, dt.config.networkResourceSubscriptionID(), dt.config.ServiceGatewayResourceName)
		plan, err := dt.prepareNICPolicy(ctx, node, currentLocations, ownedLocations, portReads)
		mc.ObserveOperationWithResult(err == nil)
		if err != nil {
			dt.reportNICPolicyError(ownedLocations, err)
			combined = errors.Join(combined, fmt.Errorf("node %q source policies: %w", name, err))
			if plan == nil || !plan.lbOnly {
				continue
			}
		}
		for _, location := range ownedLocations {
			result[location] = plan
		}
		if plan.lbOnly {
			continue
		}
		dt.mu.Lock()
		for _, location := range plan.currentLocations {
			current := dt.NRPResources.Locations[location]
			if current.nodeOwner == nil && dt.hasInboundReferencesLocked(current) && !dt.dirtyNICPolicies.Has(location) {
				current.nodeOwner = &nsgLocationOwner{name: node.Name, uid: node.UID, providerID: node.Spec.ProviderID}
				dt.NRPResources.Locations[location] = current
			}
		}
		dt.mu.Unlock()
	}
	return result, combined
}

func (dt *DiffTracker) finishNICPolicyWorkLocked(work sets.Set[string], preparation map[string]*nsgPreparedNIC) {
	readiness := make(map[string]bool)
	for uid, op := range dt.pendingServiceOps {
		if dt.needsLBPreparationLocked(op) && !op.NSGPrepared {
			readiness[uid] = true
		}
	}
	if len(readiness) > 0 {
		for _, resources := range []map[string]NRPLocation{dt.NRPResources.Locations, dt.pendingLocationUpdates} {
			for location, current := range resources {
				for address, item := range current.Addresses {
					for _, uid := range item.Services.UnsortedList() {
						if ready, tracked := readiness[uid]; tracked && ready &&
							!dt.nicPreparationCoversLocked(preparation, location, address, uid) {
							readiness[uid] = false
						}
					}
				}
			}
		}
		for uid, ready := range readiness {
			if ready {
				dt.pendingServiceOps[uid].NSGPrepared = true
				dt.triggerServiceUpdater()
			}
		}
		for _, resources := range []map[string]NRPLocation{dt.NRPResources.Locations, dt.pendingLocationUpdates} {
			for location, current := range resources {
				for _, item := range current.Addresses {
					for _, uid := range item.Services.UnsortedList() {
						if ready, tracked := readiness[uid]; tracked && !ready {
							work.Insert(location)
						}
					}
				}
			}
		}
	}
	for location := range work {
		dt.dirtyNICPolicies.Insert(location)
	}
}

func (dt *DiffTracker) prepareNICPolicy(ctx context.Context, node *v1.Node, currentLocations, locations []string, portReads map[string]error) (*nsgPreparedNIC, error) {
	owner := nsgLocationOwner{name: node.Name, uid: node.UID, providerID: node.Spec.ProviderID}
	nodeIPs := nodeInternalIPs(node)
	target, nic, vmss, err := dt.primaryNIC(ctx, node)
	if err != nil {
		return nil, err
	}
	current, err := dt.readGroup(ctx, dt.groupName(target))
	if err != nil {
		return nil, err
	}
	var existing []*armnetwork.SecurityRule
	if current != nil {
		if !dt.owns(current, target) {
			return nil, fmt.Errorf("NIC NSG %q is not owned by this ServiceGateway", ptr.Deref(current.Name, ""))
		}
		existing = current.Properties.SecurityRules
	}
	observed, err := decodeNSGServices(existing)
	if err != nil {
		return nil, err
	}
	dt.mu.Lock()
	services := dt.nicPolicyContributionsLocked(currentLocations, locations, observed)
	missingPorts := make(map[string]*ServiceOperationState)
	for uid := range services {
		op := dt.pendingServiceOps[uid]
		if op != nil && op.Config.InboundConfig != nil && op.LastAppliedConfig == nil &&
			(dt.serviceHasLocationsInNRP(uid) || !nsgPolicyEnabled(op.Config.InboundConfig)) {
			missingPorts[uid] = op
		}
	}
	dt.mu.Unlock()
	for uid, expected := range missingPorts {
		if readErr, read := portReads[uid]; read {
			err = errors.Join(err, readErr)
			continue
		}
		ports, readErr := dt.readLBBackendPorts(ctx, uid)
		portReads[uid] = readErr
		if readErr != nil {
			err = errors.Join(err, readErr)
			continue
		}
		dt.mu.Lock()
		if op := dt.pendingServiceOps[uid]; op == expected && op.LastAppliedConfig == nil {
			if op.RetainedBackendPorts == nil {
				op.RetainedBackendPorts = ports
			} else {
				for _, port := range ports {
					if !slices.Contains(op.RetainedBackendPorts, port) {
						op.RetainedBackendPorts = append(op.RetainedBackendPorts, port)
					}
				}
			}
		}
		dt.mu.Unlock()
	}
	if len(missingPorts) > 0 {
		dt.mu.Lock()
		services = dt.nicPolicyContributionsLocked(currentLocations, locations, observed)
		dt.mu.Unlock()
	}
	plan := &nsgPreparedNIC{
		owner: owner, nodeIPs: nodeIPs, locations: locations, currentLocations: currentLocations,
		services: services, target: target, group: current,
	}
	needRules := false
	for _, service := range services {
		needRules = needRules || service.sources != nil
	}
	state := ptr.Deref(nic.Properties.ProvisioningState, "")
	if state != armnetwork.ProvisioningStateSucceeded && state != armnetwork.ProvisioningStateFailed {
		return nil, fmt.Errorf("NIC association has unsettled provisioning state %q", state)
	}
	if !needRules && current == nil && err == nil {
		if state != armnetwork.ProvisioningStateSucceeded {
			return nil, fmt.Errorf("NIC association has provisioning state %q", state)
		}
		return plan, dt.checkNICNodeIdentity(ctx, owner, nodeIPs)
	}
	if needRules && nic.Properties.NetworkSecurityGroup != nil {
		if err := dt.validateAssociation(target, ptr.Deref(nic.Properties.NetworkSecurityGroup.ID, "")); err != nil {
			return nil, err
		}
	}
	attached := nic.Properties.NetworkSecurityGroup != nil &&
		strings.EqualFold(ptr.Deref(nic.Properties.NetworkSecurityGroup.ID, ""), dt.groupID(target))
	var rules []*armnetwork.SecurityRule
	if err == nil {
		rules, err = buildNICSecurityRules(services, existing)
	}
	if err != nil {
		if state != armnetwork.ProvisioningStateSucceeded || attached && current == nil {
			return nil, err
		}
		if nic.Properties.NetworkSecurityGroup != nil {
			if associationErr := dt.validateAssociation(target, ptr.Deref(nic.Properties.NetworkSecurityGroup.ID, "")); associationErr != nil {
				return nil, errors.Join(err, associationErr)
			}
		}
		if current != nil {
			confirmed, observationErr := buildNICSecurityRules(observed, existing)
			if observationErr != nil {
				return nil, errors.Join(err, observationErr)
			}
			if ptr.Deref(current.Properties.ProvisioningState, "") != armnetwork.ProvisioningStateSucceeded ||
				!strings.EqualFold(ptr.Deref(current.Location, ""), ptr.Deref(nic.Location, dt.config.Location)) ||
				!nsgRulesEqual(confirmed, existing) {
				return nil, err
			}
		}
		if !attached {
			observed = nil
		}
		candidates := make(map[string]nsgService)
		dt.mu.Lock()
		for uid, service := range dt.nicPolicyContributionsLocked(nil, locations, nil) {
			if op := dt.pendingServiceOps[uid]; dt.needsLBPreparationLocked(op) {
				candidates[uid] = nsgService{
					addresses: service.addresses,
					sources:   op.Config.InboundConfig.AllowedIPRanges,
					ports:     nsgPortMappings(op.Config.InboundConfig),
				}
			}
		}
		dt.mu.Unlock()
		for uid, candidate := range candidates {
			if nsgPolicyAllowsUnrestricted(observed, candidate) {
				continue
			}
			covered := len(candidate.addresses) > 0
			for _, address := range candidate.addresses {
				if !nsgPolicyCovers(observed[uid], candidate.sources, candidate.ports, address) {
					covered = false
					break
				}
			}
			if !covered {
				delete(candidates, uid)
			}
		}
		if len(candidates) == 0 {
			return nil, err
		}
		if identityErr := dt.checkNICNodeIdentity(ctx, owner, nodeIPs); identityErr != nil {
			return nil, errors.Join(err, identityErr)
		}
		plan.services, plan.lbOnly = candidates, true
		return plan, err
	}
	desired, changed, err := dt.desiredGroup(current, target, node.Name, ptr.Deref(nic.Location, dt.config.Location), rules)
	if err != nil {
		return nil, err
	}
	updateAssociation := needRules && !attached || attached && state == armnetwork.ProvisioningStateFailed
	if changed || updateAssociation {
		dt.mu.Lock()
		for uid := range services {
			if op := dt.pendingServiceOps[uid]; op != nil {
				op.NSGPrepared = false
			}
		}
		dt.mu.Unlock()
	}
	if changed {
		plan.group, err = dt.writeGroup(ctx, desired)
		if err != nil {
			return nil, err
		}
	}
	if updateAssociation {
		if err := dt.associate(ctx, node.Name, target, nic, vmss); err != nil {
			return nil, err
		}
		nic, err = dt.readNIC(ctx, target)
		if err != nil {
			return nil, fmt.Errorf("verify NIC NSG association: %w", err)
		}
		attached = nic.Properties.NetworkSecurityGroup != nil &&
			strings.EqualFold(ptr.Deref(nic.Properties.NetworkSecurityGroup.ID, ""), dt.groupID(target))
	}
	if ptr.Deref(nic.Properties.ProvisioningState, "") != armnetwork.ProvisioningStateSucceeded || needRules && !attached {
		return nil, fmt.Errorf("NIC NSG association has not converged")
	}
	return plan, dt.checkNICNodeIdentity(ctx, owner, nodeIPs)
}

func (dt *DiffTracker) markPreviousLocationOwnerLocked(previous nsgLocationOwner, previousIPs []string, current *v1.Node) {
	currentIPs := nodeInternalIPs(current)
	replaced := current == nil || current.UID != previous.uid || current.Spec.ProviderID != previous.providerID
	for _, location := range previousIPs {
		if !replaced && slices.Contains(currentIPs, location) {
			continue
		}
		for _, resources := range []map[string]NRPLocation{dt.NRPResources.Locations, dt.pendingLocationUpdates} {
			value := resources[location]
			owner := value.nodeOwner
			if owner == nil && !dt.hasInboundReferencesLocked(value) {
				continue
			}
			if owner != nil && (owner.name != previous.name || owner.uid != previous.uid || owner.providerID != previous.providerID) {
				continue
			}
			draining := previous
			draining.draining = true
			value.nodeOwner = &draining
			resources[location] = value
			for _, address := range value.Addresses {
				for _, uid := range address.Services.UnsortedList() {
					if op := dt.pendingServiceOps[uid]; op != nil && op.Config.IsInbound {
						op.NSGPrepared = false
					}
				}
			}
		}
	}
}

func (dt *DiffTracker) checkNICNodeIdentity(ctx context.Context, expected nsgLocationOwner, expectedIPs []string) error {
	current, err := dt.kubeClient.CoreV1().Nodes().Get(ctx, expected.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			dt.mu.Lock()
			dt.markPreviousLocationOwnerLocked(expected, expectedIPs, nil)
			dt.mu.Unlock()
		}
		return fmt.Errorf("verify node identity: %w", err)
	}
	if current.UID != expected.uid || current.Spec.ProviderID != expected.providerID ||
		!slices.Equal(nodeInternalIPs(current), expectedIPs) {
		dt.mu.Lock()
		dt.markPreviousLocationOwnerLocked(expected, expectedIPs, current)
		dt.mu.Unlock()
		return fmt.Errorf("node NIC reconciliation was superseded")
	}
	return nil
}

func (dt *DiffTracker) nicPreparationCoversLocked(preparation map[string]*nsgPreparedNIC, location, address, uid string) bool {
	op := dt.pendingServiceOps[uid]
	if preparation == nil || op == nil || dt.dirtyNICPolicies.Has(location) {
		return false
	}
	plan := preparation[location]
	ip, err := netip.ParseAddr(address)
	if plan == nil || err != nil || !slices.Contains(plan.currentLocations, location) {
		return false
	}
	for _, resources := range []map[string]NRPLocation{dt.NRPResources.Locations, dt.pendingLocationUpdates} {
		if owner := resources[location].nodeOwner; owner != nil && *owner != plan.owner {
			return false
		}
	}
	config := op.Config.InboundConfig
	if config == nil || !nsgPolicyCovers(plan.services[uid], config.AllowedIPRanges, nsgPortMappings(config), ip.Unmap()) {
		return false
	}
	if plan.lbOnly {
		return true
	}
	portSets := [][]PortMapping{op.RetainedBackendPorts}
	if op.InFlightConfig != nil {
		portSets = append(portSets, nsgPortMappings(op.InFlightConfig.InboundConfig))
	}
	for _, ports := range portSets {
		for _, port := range ports {
			if !slices.Contains(plan.services[uid].ports, port) {
				return false
			}
		}
	}
	return true
}

func (dt *DiffTracker) canPublishInboundLocked(preparation map[string]*nsgPreparedNIC, location, address, uid string) bool {
	op := dt.pendingServiceOps[uid]
	if op != nil && op.Config.InboundConfig != nil && !dt.mayHaveNICPolicies && !nsgPolicyEnabled(op.Config.InboundConfig) {
		return true
	}
	for _, resources := range []map[string]NRPLocation{dt.NRPResources.Locations, dt.pendingLocationUpdates} {
		if owner := resources[location].nodeOwner; owner != nil && owner.draining {
			dt.dirtyNICPolicies.Insert(location)
			return false
		}
	}
	for _, resources := range []map[string]NRPLocation{dt.NRPResources.Locations, dt.pendingLocationUpdates} {
		if current := resources[location].Addresses[address]; current.Services != nil && current.Services.Has(uid) {
			return true
		}
	}
	if op == nil || op.Config.InboundConfig == nil {
		return false
	}
	if plan := preparation[location]; plan != nil && plan.lbOnly {
		return false
	}
	if !dt.nicPreparationCoversLocked(preparation, location, address, uid) {
		dt.dirtyNICPolicies.Insert(location)
		return false
	}
	return !nsgPolicyEnabled(op.Config.InboundConfig) || op.LastAppliedConfig != nil
}

func (dt *DiffTracker) pruneNICPolicies(ctx context.Context, preparation map[string]*nsgPreparedNIC, work sets.Set[string]) error {
	if preparation == nil {
		return nil
	}
	seen := sets.New[*nsgPreparedNIC]()
	var combined error
	for _, plan := range preparation {
		if plan.lbOnly {
			continue
		}
		if seen.Has(plan) {
			continue
		}
		seen.Insert(plan)
		dt.mu.Lock()
		stale := false
		for _, location := range plan.locations {
			stale = stale || dt.dirtyNICPolicies.Has(location)
		}
		services := dt.nicPolicyContributionsLocked(plan.currentLocations, plan.locations, plan.services)
		dt.mu.Unlock()
		if stale {
			continue
		}
		if plan.group == nil || maps.EqualFunc(services, plan.services, func(a, b nsgService) bool {
			return (a.sources == nil) == (b.sources == nil) &&
				slices.Equal(a.sources, b.sources) && slices.Equal(a.ports, b.ports) && slices.Equal(a.addresses, b.addresses)
		}) {
			work.Delete(plan.currentLocations...)
			continue
		}
		rules, err := buildNICSecurityRules(services, plan.group.Properties.SecurityRules)
		if err == nil && !nsgRulesEqual(rules, plan.group.Properties.SecurityRules) {
			err = dt.checkNICNodeIdentity(ctx, plan.owner, plan.nodeIPs)
			if err == nil {
				dt.mu.Lock()
				for uid := range services {
					if op := dt.pendingServiceOps[uid]; op != nil {
						op.NSGPrepared = false
					}
				}
				dt.mu.Unlock()
				var desired armnetwork.SecurityGroup
				desired, _, err = dt.desiredGroup(plan.group, plan.target, plan.owner.name, ptr.Deref(plan.group.Location, ""), rules)
				if err == nil {
					plan.group, err = dt.writeGroup(ctx, desired)
				}
			}
		}
		if err != nil {
			dt.reportNICPolicyError(plan.locations, err)
			combined = errors.Join(combined, err)
			for _, location := range plan.locations {
				delete(preparation, location)
			}
		} else {
			plan.services = services
			work.Delete(plan.currentLocations...)
		}
	}
	return combined
}

func (dt *DiffTracker) reportNICPolicyError(locations []string, err error) {
	dt.logger.V(4).Info("Could not reconcile NIC source policies; retrying with locations", "err", err)
	dt.mu.Lock()
	services := dt.nicPolicyContributionsLocked(locations, locations, nil)
	configs := make([]ServiceConfig, 0, len(services))
	for uid := range services {
		if op := dt.pendingServiceOps[uid]; op != nil {
			configs = append(configs, op.Config)
		}
	}
	dt.mu.Unlock()
	reason, message := consts.ServiceGatewayNSGReconcileFailedReason, err.Error()
	var warning WarningEventError
	if errors.As(err, &warning) {
		reason, message = warning.WarningEvent()
	}
	for _, config := range configs {
		uid, kind, valid := serviceIdentity(config.ServiceName)
		if valid && kind == Inbound && config.Namespace != "" && config.Name != "" {
			dt.recordEvent(&v1.Service{ObjectMeta: metav1.ObjectMeta{
				Namespace: config.Namespace, Name: config.Name, UID: types.UID(uid),
			}}, v1.EventTypeWarning, reason, message)
		}
	}
}

func (dt *DiffTracker) wakeNICPolicyWorkLocked() {
	if (dt.dirtyNICPolicies.Len() > 0 || dt.locationRecoveryPending) && atomic.LoadInt32(&dt.isInitializing) == 0 {
		dt.triggerLocationsUpdater()
	}
}
