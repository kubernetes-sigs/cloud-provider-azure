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
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/securitygroup"
)

// NSGNodeProvider exposes the existing VMSet's node-cache invalidation.
type NSGNodeProvider interface {
	DeleteCacheForNode(context.Context, string) error
}

func (dt *DiffTracker) targetFromID(value string) (*arm.ResourceID, error) {
	id, err := arm.ParseResourceID(value)
	if err != nil {
		return nil, fmt.Errorf("parse node NIC identity: %w", err)
	}
	if !strings.EqualFold(id.SubscriptionID, dt.config.SubscriptionID) || id.ResourceGroupName == "" || id.Name == "" {
		return nil, fmt.Errorf("node NIC identity has an unexpected subscription or resource scope")
	}
	switch {
	case strings.EqualFold(id.ResourceType.String(), consts.ServiceGatewayNSGNICResourceType):
	case strings.EqualFold(id.ResourceType.String(), consts.ServiceGatewayNSGVMSSNICResourceType):
		if id.Parent == nil || id.Parent.Parent == nil {
			return nil, fmt.Errorf("VMSS NIC identity has no instance or scale set")
		}
	default:
		return nil, fmt.Errorf("unsupported node NIC resource type %q", id.ResourceType)
	}
	return id, nil
}

func (dt *DiffTracker) targetFromTags(group *armnetwork.SecurityGroup) (*arm.ResourceID, error) {
	rg := nsgTagValue(group.Tags, consts.ServiceGatewayNSGTargetRGTag)
	name := nsgTagValue(group.Tags, consts.ServiceGatewayNSGTargetNameTag)
	vmss := nsgTagValue(group.Tags, consts.ServiceGatewayNSGTargetVMSSTag)
	instance := nsgTagValue(group.Tags, consts.ServiceGatewayNSGTargetInstanceTag)
	if rg == "" || name == "" || (vmss == "") != (instance == "") {
		return nil, fmt.Errorf("managed NSG %q has incomplete NIC identity tags", ptr.Deref(group.Name, ""))
	}
	id := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/%s/%s",
		dt.config.SubscriptionID, rg, consts.ServiceGatewayNSGNICResourceType, name)
	if vmss != "" {
		id = fmt.Sprintf(consts.VmssMachineIDTemplate, dt.config.SubscriptionID, rg, vmss, instance) + "/networkInterfaces/" + name
	}
	target, err := dt.targetFromID(id)
	if err != nil {
		return target, err
	}
	if !dt.owns(group, target) {
		return target, fmt.Errorf("managed NSG %q does not match its NIC identity", ptr.Deref(group.Name, ""))
	}
	return target, nil
}

func (dt *DiffTracker) groupName(target *arm.ResourceID) string {
	return consts.ServiceGatewayNSGNamePrefix + nsgIdentity(dt.config.ServiceGatewayResourceID())[:16] + "-" + nsgIdentity(target.String())[:32]
}

func (dt *DiffTracker) groupID(target *arm.ResourceID) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/networkSecurityGroups/%s",
		dt.config.networkResourceSubscriptionID(), dt.config.ResourceGroup, dt.groupName(target))
}

func (dt *DiffTracker) ownsClusterGroup(group *armnetwork.SecurityGroup) bool {
	return group != nil && strings.HasPrefix(strings.ToLower(ptr.Deref(group.Name, "")), consts.ServiceGatewayNSGNamePrefix) &&
		nsgTagValue(group.Tags, consts.ServiceGatewayNSGOwnerTag) == nsgIdentity(dt.config.ServiceGatewayResourceID())
}

func (dt *DiffTracker) owns(group *armnetwork.SecurityGroup, target *arm.ResourceID) bool {
	return dt.ownsClusterGroup(group) && strings.EqualFold(ptr.Deref(group.Name, ""), dt.groupName(target)) &&
		strings.EqualFold(ptr.Deref(group.ID, ""), dt.groupID(target)) &&
		nsgTagValue(group.Tags, consts.ServiceGatewayNSGNICTag) == nsgIdentity(target.String())
}

func nsgTagValue(tags map[string]*string, key string) string {
	var value string
	found := false
	for name, item := range tags {
		if !strings.EqualFold(name, key) {
			continue
		}
		current := ptr.Deref(item, "")
		if found && current != value {
			return ""
		}
		value, found = current, true
	}
	return value
}

func (dt *DiffTracker) validateAssociation(target *arm.ResourceID, currentID string) error {
	if currentID == "" || strings.EqualFold(currentID, dt.groupID(target)) {
		return nil
	}
	clusterRG := dt.config.SecurityGroupResourceGroup
	if clusterRG == "" {
		clusterRG = dt.config.ResourceGroup
	}
	clusterID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/networkSecurityGroups/%s",
		dt.config.networkResourceSubscriptionID(), clusterRG, dt.config.SecurityGroupName)
	if dt.config.SecurityGroupName != "" && strings.EqualFold(currentID, clusterID) {
		return nil
	}
	return fmt.Errorf("node NIC %q has a different NIC NSG association", target.Name)
}

func (dt *DiffTracker) primaryNIC(ctx context.Context, node *v1.Node) (*arm.ResourceID, *armnetwork.Interface, *armcompute.VirtualMachineScaleSetVM, error) {
	providerID := strings.TrimPrefix(node.Spec.ProviderID, consts.ServiceGatewayNSGProviderIDPrefix)
	id, err := arm.ParseResourceID(providerID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse node provider identity: %w", err)
	}
	if !strings.EqualFold(id.SubscriptionID, dt.config.SubscriptionID) || id.ResourceGroupName == "" {
		return nil, nil, nil, fmt.Errorf("node provider identity has an unexpected subscription or resource scope")
	}
	var vmID, location *string
	var profile *armcompute.NetworkProfile
	var vmssVM *armcompute.VirtualMachineScaleSetVM
	// Use the node clients directly so NIC reads retain the attempt context.
	switch {
	case strings.EqualFold(id.ResourceType.String(), consts.ServiceGatewayNSGVMResourceType):
		vm, readErr := dt.computeClientFactory.GetVirtualMachineClient().Get(ctx, id.ResourceGroupName, id.Name, nil)
		if readErr != nil {
			return nil, nil, nil, fmt.Errorf("read node VM: %w", readErr)
		}
		if vm != nil && vm.Properties != nil {
			vmID, location, profile = vm.ID, vm.Location, vm.Properties.NetworkProfile
		}
	case strings.EqualFold(id.ResourceType.String(), consts.ServiceGatewayNSGVMSSVMResourceType):
		vm, readErr := dt.computeClientFactory.GetVirtualMachineScaleSetVMClient().Get(ctx, id.ResourceGroupName, id.Parent.Name, id.Name)
		if readErr != nil {
			return nil, nil, nil, fmt.Errorf("read node VMSS instance: %w", readErr)
		}
		vmssVM = vm
		if vm != nil && vm.Properties != nil {
			vmID, location, profile = vm.ID, vm.Location, vm.Properties.NetworkProfile
		}
	default:
		return nil, nil, nil, fmt.Errorf("unsupported node provider resource type %q", id.ResourceType)
	}
	if !strings.EqualFold(ptr.Deref(vmID, ""), providerID) {
		return nil, nil, nil, fmt.Errorf("node %q provider identity does not match its VM", node.Name)
	}
	nicID, err := nsgPrimaryNICID(profile)
	if err != nil {
		return nil, nil, nil, err
	}
	target, err := dt.targetFromID(nicID)
	if err != nil {
		return nil, nil, nil, err
	}
	nic, err := dt.readNIC(ctx, target)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read node NIC: %w", err)
	}
	if nic.Properties.VirtualMachine == nil || !strings.EqualFold(ptr.Deref(nic.Properties.VirtualMachine.ID, ""), providerID) {
		return nil, nil, nil, fmt.Errorf("node %q provider identity does not match its NIC", node.Name)
	}
	if ptr.Deref(nic.Location, "") == "" {
		nic.Location = location
	}
	if ptr.Deref(nic.Location, "") == "" {
		return nil, nil, nil, fmt.Errorf("node %q NIC has no location", node.Name)
	}
	return target, nic, vmssVM, nil
}

func nsgPrimaryNICID(profile *armcompute.NetworkProfile) (string, error) {
	if profile == nil {
		return "", fmt.Errorf("node VM has no network profile")
	}
	var selected string
	for _, reference := range profile.NetworkInterfaces {
		if reference == nil || len(profile.NetworkInterfaces) != 1 &&
			(reference.Properties == nil || !ptr.Deref(reference.Properties.Primary, false)) {
			continue
		}
		if selected != "" || ptr.Deref(reference.ID, "") == "" {
			return "", fmt.Errorf("node VM has ambiguous or incomplete primary NIC references")
		}
		selected = *reference.ID
	}
	if selected == "" {
		return "", fmt.Errorf("node VM has no primary NIC")
	}
	return selected, nil
}

func (dt *DiffTracker) readNIC(ctx context.Context, target *arm.ResourceID) (*armnetwork.Interface, error) {
	client := dt.computeClientFactory.GetInterfaceClient()
	var nic *armnetwork.Interface
	var err error
	if strings.EqualFold(target.ResourceType.String(), consts.ServiceGatewayNSGNICResourceType) {
		nic, err = client.Get(ctx, target.ResourceGroupName, target.Name, nil)
	} else {
		nic, err = client.GetVirtualMachineScaleSetNetworkInterface(ctx, target.ResourceGroupName, target.Parent.Parent.Name, target.Parent.Name, target.Name)
	}
	if err != nil {
		return nil, err
	}
	if nic == nil || nic.Properties == nil || !strings.EqualFold(ptr.Deref(nic.ID, ""), target.String()) {
		return nil, fmt.Errorf("node NIC response does not match the requested resource")
	}
	return nic, nil
}

func (dt *DiffTracker) readLBBackendPorts(ctx context.Context, uid string) ([]PortMapping, error) {
	lb, err := dt.networkClientFactory.GetLoadBalancerClient().Get(ctx, dt.config.ResourceGroup, uid, nil)
	if isNSGNotFound(err) {
		return []PortMapping{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read LB backend ports for %q: %w", uid, err)
	}
	expectedID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/%s",
		dt.config.networkResourceSubscriptionID(), dt.config.ResourceGroup, uid)
	if lb == nil || !strings.EqualFold(ptr.Deref(lb.ID, ""), expectedID) {
		return nil, fmt.Errorf("LB backend-port response does not match service %q", uid)
	}
	if lb.Properties == nil || lb.Properties.ProvisioningState == nil ||
		!strings.EqualFold(string(*lb.Properties.ProvisioningState), string(armnetwork.ProvisioningStateSucceeded)) {
		return nil, fmt.Errorf("LB %q backend-port baseline is not in Succeeded provisioning state", uid)
	}
	ports, err := loadBalancerBackendPorts(lb)
	if err != nil {
		return nil, fmt.Errorf("read LB backend ports for %q: %w", uid, err)
	}
	return ports, nil
}

func (dt *DiffTracker) readGroup(ctx context.Context, name string) (*armnetwork.SecurityGroup, error) {
	group, err := dt.networkClientFactory.GetSecurityGroupClient().Get(ctx, dt.config.ResourceGroup, name)
	if isNSGNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get NIC NSG: %w", err)
	}
	if group == nil || group.Properties == nil {
		return nil, fmt.Errorf("get NIC NSG returned an incomplete resource")
	}
	state := string(ptr.Deref(group.Properties.ProvisioningState, ""))
	if !strings.EqualFold(state, string(armnetwork.ProvisioningStateSucceeded)) &&
		!strings.EqualFold(state, string(armnetwork.ProvisioningStateFailed)) {
		return nil, fmt.Errorf("NIC NSG %q has not reached a settled provisioning state (%q)", name, state)
	}
	return group, nil
}

func (dt *DiffTracker) deleteOrphanGroup(ctx context.Context, name string) (bool, error) {
	group, err := dt.readGroup(ctx, name)
	if err != nil {
		return false, err
	}
	if group == nil {
		return true, nil
	}
	target, err := dt.targetFromTags(group)
	if err != nil {
		return false, err
	}
	if len(group.Properties.NetworkInterfaces) != 0 || len(group.Properties.Subnets) != 0 {
		return false, nil
	}
	_, err = dt.readNIC(ctx, target)
	if isNSGNotFound(err) {
		if err := dt.networkClientFactory.GetSecurityGroupClient().Delete(ctx, dt.config.ResourceGroup, name); err != nil && !isNSGNotFound(err) {
			return false, fmt.Errorf("delete orphaned NIC NSG: %w", err)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("check orphan NIC: %w", err)
	}
	return false, nil
}

func (dt *DiffTracker) desiredGroup(current *armnetwork.SecurityGroup, target *arm.ResourceID, nodeName, location string, rules []*armnetwork.SecurityRule) (armnetwork.SecurityGroup, bool, error) {
	desired := armnetwork.SecurityGroup{
		Name: ptr.To(dt.groupName(target)), ID: ptr.To(dt.groupID(target)), Location: ptr.To(location),
		Properties: &armnetwork.SecurityGroupPropertiesFormat{},
		Tags:       make(map[string]*string),
	}
	if current != nil {
		if !dt.owns(current, target) {
			return desired, false, fmt.Errorf("NIC NSG %q is not owned by this ServiceGateway and NIC", ptr.Deref(current.Name, ""))
		}
		if !strings.EqualFold(ptr.Deref(current.Location, ""), location) {
			return desired, false, fmt.Errorf("NIC NSG %q has a different location", *current.Name)
		}
		desired = *current
		properties := *current.Properties
		desired.Properties = &properties
		desired.Tags = maps.Clone(current.Tags)
	}
	desired.Properties.SecurityRules = rules
	vmss, instance := "", ""
	if strings.EqualFold(target.ResourceType.String(), consts.ServiceGatewayNSGVMSSNICResourceType) {
		vmss, instance = target.Parent.Parent.Name, target.Parent.Name
	}
	for key, value := range map[string]string{
		consts.ServiceGatewayNSGOwnerTag:          nsgIdentity(dt.config.ServiceGatewayResourceID()),
		consts.ServiceGatewayNSGNICTag:            nsgIdentity(target.String()),
		consts.ServiceGatewayNSGNodeTag:           nodeName,
		consts.ServiceGatewayNSGTargetRGTag:       target.ResourceGroupName,
		consts.ServiceGatewayNSGTargetNameTag:     target.Name,
		consts.ServiceGatewayNSGTargetVMSSTag:     vmss,
		consts.ServiceGatewayNSGTargetInstanceTag: instance,
	} {
		for existing := range desired.Tags {
			if existing != key && strings.EqualFold(existing, key) {
				delete(desired.Tags, existing)
			}
		}
		desired.Tags[key] = ptr.To(value)
	}
	changed := current == nil || !nsgRulesEqual(current.Properties.SecurityRules, rules) ||
		!reflect.DeepEqual(current.Tags, desired.Tags) ||
		strings.EqualFold(string(ptr.Deref(current.Properties.ProvisioningState, "")), string(armnetwork.ProvisioningStateFailed))
	if current != nil && changed && ptr.Deref(current.Etag, "") == "" {
		return desired, false, fmt.Errorf("NIC NSG %q has no ETag for a conditional update", *current.Name)
	}
	return desired, changed, nil
}

func (dt *DiffTracker) writeGroup(ctx context.Context, desired armnetwork.SecurityGroup) (*armnetwork.SecurityGroup, error) {
	result, err := dt.networkClientFactory.GetSecurityGroupClient().CreateOrUpdate(ctx, dt.config.ResourceGroup, *desired.Name, desired)
	if err != nil {
		return nil, fmt.Errorf("update NIC NSG: %w", err)
	}
	if result == nil || result.Properties == nil || !strings.EqualFold(ptr.Deref(result.ID, ""), ptr.Deref(desired.ID, "")) {
		return nil, fmt.Errorf("update NIC NSG returned an incomplete or mismatched resource")
	}
	if result.Properties.ProvisioningState == nil || *result.Properties.ProvisioningState != armnetwork.ProvisioningStateSucceeded ||
		!nsgRulesEqual(result.Properties.SecurityRules, desired.Properties.SecurityRules) {
		return nil, fmt.Errorf("update NIC NSG did not confirm the requested source policies")
	}
	return result, nil
}

func (dt *DiffTracker) associate(ctx context.Context, nodeName string, target *arm.ResourceID, nic *armnetwork.Interface, vm *armcompute.VirtualMachineScaleSetVM) (err error) {
	if nic.Properties.NetworkSecurityGroup != nil {
		if err := dt.validateAssociation(target, ptr.Deref(nic.Properties.NetworkSecurityGroup.ID, "")); err != nil {
			return err
		}
	}
	defer func() {
		if cacheErr := dt.nodeProvider.DeleteCacheForNode(ctx, nodeName); cacheErr != nil {
			err = errors.Join(err, fmt.Errorf("invalidate node network cache: %w", cacheErr))
		}
	}()
	groupID := dt.groupID(target)
	if strings.EqualFold(target.ResourceType.String(), consts.ServiceGatewayNSGNICResourceType) {
		if ptr.Deref(nic.Etag, "") == "" {
			return fmt.Errorf("node NIC has no ETag for a conditional update")
		}
		value := *nic
		properties := *nic.Properties
		properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: ptr.To(groupID)}
		value.Properties = &properties
		ctx = policy.WithHTTPHeader(ctx, http.Header{consts.IfMatchHeader: {*nic.Etag}})
		result, updateErr := dt.computeClientFactory.GetInterfaceClient().CreateOrUpdate(ctx, target.ResourceGroupName, target.Name, value)
		if updateErr != nil {
			return fmt.Errorf("associate node NIC with NSG: %w", updateErr)
		}
		if result == nil || !strings.EqualFold(ptr.Deref(result.ID, ""), target.String()) ||
			result.Properties == nil || result.Properties.NetworkSecurityGroup == nil ||
			!strings.EqualFold(ptr.Deref(result.Properties.NetworkSecurityGroup.ID, ""), groupID) {
			return fmt.Errorf("node NIC update did not confirm the requested NSG association")
		}
		return nil
	}
	client := dt.computeClientFactory.GetVirtualMachineScaleSetVMClient()
	if vm == nil || vm.Properties == nil || vm.Properties.NetworkProfileConfiguration == nil || vm.Properties.NetworkProfile == nil ||
		ptr.Deref(vm.Etag, "") == "" || ptr.Deref(vm.Location, "") == "" {
		return fmt.Errorf("VMSS instance network configuration is incomplete")
	}
	vmID := target.Parent.String()
	if !strings.EqualFold(ptr.Deref(vm.ID, ""), vmID) {
		return fmt.Errorf("VMSS instance response does not match the requested resource")
	}
	primaryID, err := nsgPrimaryNICID(vm.Properties.NetworkProfile)
	if err != nil {
		return err
	}
	if !strings.EqualFold(primaryID, target.String()) {
		return fmt.Errorf("VMSS instance primary NIC changed during reconciliation")
	}
	configurations := slices.Clone(vm.Properties.NetworkProfileConfiguration.NetworkInterfaceConfigurations)
	selected := -1
	for index, config := range configurations {
		if config != nil && config.Properties != nil && (len(configurations) == 1 || ptr.Deref(config.Properties.Primary, false)) {
			if selected != -1 {
				return fmt.Errorf("VMSS instance has multiple primary NIC configurations")
			}
			selected = index
		}
	}
	if selected == -1 {
		return fmt.Errorf("VMSS instance has no primary NIC configuration")
	}
	configuration := *configurations[selected]
	properties := *configuration.Properties
	if properties.NetworkSecurityGroup != nil {
		if err := dt.validateAssociation(target, ptr.Deref(properties.NetworkSecurityGroup.ID, "")); err != nil {
			return err
		}
	}
	properties.NetworkSecurityGroup = &armcompute.SubResource{ID: ptr.To(groupID)}
	configuration.Properties = &properties
	configurations[selected] = &configuration
	parameters := armcompute.VirtualMachineScaleSetVM{
		Location: vm.Location,
		Etag:     vm.Etag,
		Properties: &armcompute.VirtualMachineScaleSetVMProperties{
			HardwareProfile: vm.Properties.HardwareProfile,
			NetworkProfileConfiguration: &armcompute.VirtualMachineScaleSetVMNetworkProfileConfiguration{
				NetworkInterfaceConfigurations: configurations,
			},
		},
	}
	result, err := utils.NewPollerWrapper(client.BeginUpdate(ctx, target.ResourceGroupName, target.Parent.Parent.Name, target.Parent.Name, parameters,
		&armcompute.VirtualMachineScaleSetVMsClientBeginUpdateOptions{IfMatch: vm.Etag})).WaitforPollerResp(ctx)
	if err != nil {
		return fmt.Errorf("associate VMSS instance NIC with NSG: %w", err)
	}
	if result == nil {
		return fmt.Errorf("VMSS instance update returned no resource")
	}
	return nil
}

func isNSGNotFound(err error) bool {
	var response *azcore.ResponseError
	return errors.As(err, &response) && response.StatusCode == http.StatusNotFound
}

func nsgRulesEqual(first, second []*armnetwork.SecurityRule) bool {
	normalizePorts := func(single *string, multiple []*string) []*string {
		ports := slices.Clone(multiple)
		if single != nil {
			ports = append(ports, single)
		}
		if len(ports) == 0 {
			return nil
		}
		slices.SortFunc(ports, func(a, b *string) int { return strings.Compare(ptr.Deref(a, ""), ptr.Deref(b, "")) })
		return slices.CompactFunc(ports, func(a, b *string) bool { return ptr.Deref(a, "") == ptr.Deref(b, "") })
	}
	normalize := func(rules []*armnetwork.SecurityRule) []*armnetwork.SecurityRule {
		result := make([]*armnetwork.SecurityRule, 0, len(rules))
		for _, rule := range rules {
			if rule == nil || rule.Properties == nil {
				return nil
			}
			value := *rule
			properties := *rule.Properties
			value.ID, value.Etag, value.Type = nil, nil, nil
			properties.ProvisioningState = nil
			properties.SourcePortRanges = normalizePorts(properties.SourcePortRange, properties.SourcePortRanges)
			properties.SourcePortRange = nil
			properties.DestinationPortRanges = normalizePorts(properties.DestinationPortRange, properties.DestinationPortRanges)
			properties.DestinationPortRange = nil
			if len(properties.SourceApplicationSecurityGroups) == 0 {
				properties.SourceApplicationSecurityGroups = nil
			}
			if len(properties.DestinationApplicationSecurityGroups) == 0 {
				properties.DestinationApplicationSecurityGroups = nil
			}
			sources := securitygroup.NormalizeSecurityRuleAddressPrefixes(securitygroup.ListSourcePrefixes(rule))
			properties.SourceAddressPrefix = nil
			properties.SourceAddressPrefixes = nil
			for _, source := range sources {
				properties.SourceAddressPrefixes = append(properties.SourceAddressPrefixes, ptr.To(source))
			}
			value.Properties = &properties
			securitygroup.SetDestinationPrefixes(&value, securitygroup.ListDestinationPrefixes(rule))
			result = append(result, &value)
		}
		slices.SortFunc(result, func(a, b *armnetwork.SecurityRule) int {
			return strings.Compare(ptr.Deref(a.Name, ""), ptr.Deref(b.Name, ""))
		})
		return result
	}
	return reflect.DeepEqual(normalize(first), normalize(second))
}
