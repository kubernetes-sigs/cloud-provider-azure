/*
Copyright 2023 The Kubernetes Authors.

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
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	v1 "k8s.io/api/core/v1"
	servicehelpers "k8s.io/cloud-provider/service/helpers"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/pointer"

	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// This reconciles the Network Security Group similar to how the LB is reconciled.
// This entails adding required, missing SecurityRules and removing stale rules.
func (az *Cloud) reconcileSecurityGroup(clusterName string, service *v1.Service, lbIPs *[]string, lbName *string, wantLb bool) (*network.SecurityGroup, error) {
	serviceName := getServiceName(service)
	klog.V(5).Infof("reconcileSecurityGroup(%s): START clusterName=%q", serviceName, clusterName)

	ports := service.Spec.Ports
	if ports == nil {
		if useSharedSecurityRule(service) {
			klog.V(2).Infof("Attempting to reconcile security group for service %s, but service uses shared rule and we don't know which port it's for", service.Name)
			return nil, fmt.Errorf("no port info for reconciling shared rule for service %s", service.Name)
		}
		ports = []v1.ServicePort{}
	}

	sg, err := az.getSecurityGroup(azcache.CacheReadTypeDefault)
	if err != nil {
		return nil, err
	}

	if wantLb && lbIPs == nil {
		return nil, fmt.Errorf("no load balancer IP for setting up security rules for service %s", service.Name)
	}

	destinationIPAddresses := map[bool][]string{}
	if lbIPs != nil {
		for _, ip := range *lbIPs {
			if net.ParseIP(ip).To4() != nil {
				destinationIPAddresses[false] = append(destinationIPAddresses[false], ip)
			} else {
				destinationIPAddresses[true] = append(destinationIPAddresses[true], ip)
			}
		}
	}

	if len(destinationIPAddresses[false]) == 0 {
		destinationIPAddresses[false] = []string{"*"}
	}
	if len(destinationIPAddresses[true]) == 0 {
		destinationIPAddresses[true] = []string{"*"}
	}

	disableFloatingIP := false
	if consts.IsK8sServiceDisableLoadBalancerFloatingIP(service) {
		disableFloatingIP = true
	}

	backendIPAddresses := map[bool][]string{}
	if wantLb && disableFloatingIP {
		lb, exist, err := az.getAzureLoadBalancer(pointer.StringDeref(lbName, ""), azcache.CacheReadTypeDefault)
		if err != nil {
			return nil, err
		}
		if !exist {
			return nil, fmt.Errorf("unable to get lb %s", pointer.StringDeref(lbName, ""))
		}
		backendIPAddresses[false], backendIPAddresses[true] = az.LoadBalancerBackendPool.GetBackendPrivateIPs(clusterName, service, lb)
	}

	additionalIPs, err := getServiceAdditionalPublicIPs(service)
	if err != nil {
		return nil, fmt.Errorf("unable to get additional public IPs, error=%w", err)
	}
	for _, ip := range additionalIPs {
		isIPv6 := net.ParseIP(ip).To4() == nil
		if len(destinationIPAddresses[isIPv6]) != 1 || destinationIPAddresses[isIPv6][0] != "*" {
			destinationIPAddresses[isIPv6] = append(destinationIPAddresses[isIPv6], ip)
		}
	}

	sourceRanges, err := servicehelpers.GetLoadBalancerSourceRanges(service)
	if err != nil {
		return nil, err
	}
	serviceTags := getServiceTags(service)
	if len(serviceTags) != 0 {
		delete(sourceRanges, consts.DefaultLoadBalancerSourceRanges)
	}

	sourceAddressPrefixes := map[bool][]string{}
	if (sourceRanges == nil || servicehelpers.IsAllowAll(sourceRanges)) && len(serviceTags) == 0 {
		if !requiresInternalLoadBalancer(service) || len(service.Spec.LoadBalancerSourceRanges) > 0 {
			sourceAddressPrefixes[false] = []string{"Internet"}
			sourceAddressPrefixes[true] = []string{"Internet"}
		}
	} else {
		for _, ip := range sourceRanges {
			if ip == nil {
				continue
			}
			isIPv6 := net.ParseIP(ip.IP.String()).To4() == nil
			sourceAddressPrefixes[isIPv6] = append(sourceAddressPrefixes[isIPv6], ip.String())
		}
		sourceAddressPrefixes[false] = append(sourceAddressPrefixes[false], serviceTags...)
		sourceAddressPrefixes[true] = append(sourceAddressPrefixes[true], serviceTags...)
	}

	expectedSecurityRules := []network.SecurityRule{}
	handleSecurityRules := func(isIPv6 bool) error {
		expectedSecurityRulesSingleStack, err := az.getExpectedSecurityRules(wantLb, ports, sourceAddressPrefixes[isIPv6], service, destinationIPAddresses[isIPv6], sourceRanges, backendIPAddresses[isIPv6], disableFloatingIP, isIPv6)
		expectedSecurityRules = append(expectedSecurityRules, expectedSecurityRulesSingleStack...)
		return err
	}
	v4Enabled, v6Enabled := getIPFamiliesEnabled(service)
	if v4Enabled {
		if err := handleSecurityRules(false); err != nil {
			return nil, err
		}
	}
	if v6Enabled {
		if err := handleSecurityRules(true); err != nil {
			return nil, err
		}
	}

	// update security rules
	dirtySg, updatedRules, err := az.reconcileSecurityRules(sg, service, serviceName, wantLb, expectedSecurityRules, ports, sourceAddressPrefixes, destinationIPAddresses)
	if err != nil {
		return nil, err
	}

	changed := az.ensureSecurityGroupTagged(&sg)
	if changed {
		dirtySg = true
	}

	if dirtySg {
		sg.SecurityRules = &updatedRules
		klog.V(2).Infof("reconcileSecurityGroup for service(%s): sg(%s) - updating", serviceName, *sg.Name)
		klog.V(10).Infof("CreateOrUpdateSecurityGroup(%q): start", *sg.Name)
		err := az.CreateOrUpdateSecurityGroup(sg)
		if err != nil {
			klog.V(2).Infof("ensure(%s) abort backoff: sg(%s) - updating", serviceName, *sg.Name)
			return nil, err
		}
		klog.V(10).Infof("CreateOrUpdateSecurityGroup(%q): end", *sg.Name)
		_ = az.nsgCache.Delete(pointer.StringDeref(sg.Name, ""))
	}
	return &sg, nil
}

func (az *Cloud) reconcileSecurityRules(sg network.SecurityGroup,
	service *v1.Service,
	serviceName string,
	wantLb bool,
	expectedSecurityRules []network.SecurityRule,
	ports []v1.ServicePort,
	sourceAddressPrefixes, destinationIPAddresses map[bool][]string,
) (bool, []network.SecurityRule, error) {
	dirtySg := false
	var updatedRules []network.SecurityRule
	if sg.SecurityGroupPropertiesFormat != nil && sg.SecurityGroupPropertiesFormat.SecurityRules != nil {
		updatedRules = *sg.SecurityGroupPropertiesFormat.SecurityRules
	}

	for _, r := range updatedRules {
		klog.V(10).Infof("Existing security rule while processing %s: %s:%s -> %s:%s", service.Name, logSafe(r.SourceAddressPrefix), logSafe(r.SourcePortRange), logSafeCollection(r.DestinationAddressPrefix, r.DestinationAddressPrefixes), logSafe(r.DestinationPortRange))
	}

	// update security rules: remove unwanted rules that belong privately
	// to this service
	for i := len(updatedRules) - 1; i >= 0; i-- {
		existingRule := updatedRules[i]
		if az.serviceOwnsRule(service, *existingRule.Name) {
			klog.V(10).Infof("reconcile(%s)(%t): sg rule(%s) - considering evicting", serviceName, wantLb, *existingRule.Name)
			keepRule := false
			if findSecurityRule(expectedSecurityRules, existingRule) {
				klog.V(10).Infof("reconcile(%s)(%t): sg rule(%s) - keeping", serviceName, wantLb, *existingRule.Name)
				keepRule = true
			}
			if !keepRule {
				klog.V(10).Infof("reconcile(%s)(%t): sg rule(%s) - dropping", serviceName, wantLb, *existingRule.Name)
				updatedRules = append(updatedRules[:i], updatedRules[i+1:]...)
				dirtySg = true
			}
		}
	}

	// update security rules: if the service uses a shared rule and is being deleted,
	// then remove it from the shared rule
	handleRule := func(isIPv6 bool) {
		if useSharedSecurityRule(service) {
			for _, port := range ports {
				for _, sourceAddressPrefix := range sourceAddressPrefixes[isIPv6] {
					sharedRuleName := az.getSecurityRuleName(service, port, sourceAddressPrefix, isIPv6)
					sharedIndex, sharedRule, sharedRuleFound := findSecurityRuleByName(updatedRules, sharedRuleName)
					if !sharedRuleFound {
						klog.V(4).Infof("Didn't find shared rule %s for service %s", sharedRuleName, service.Name)
						continue
					}
					shouldDeleteNSGRule := false
					if sharedRule.SecurityRulePropertiesFormat == nil ||
						sharedRule.SecurityRulePropertiesFormat.DestinationAddressPrefixes == nil ||
						len(*sharedRule.SecurityRulePropertiesFormat.DestinationAddressPrefixes) == 0 {
						shouldDeleteNSGRule = true
					} else {
						existingPrefixes := *sharedRule.DestinationAddressPrefixes
						for _, destinationIPAddress := range destinationIPAddresses[isIPv6] {
							addressIndex, found := findIndex(existingPrefixes, destinationIPAddress)
							if !found {
								klog.Warningf("Didn't find destination address %v in shared rule %s for service %s", destinationIPAddress, sharedRuleName, service.Name)
								continue
							}
							if len(existingPrefixes) == 1 {
								shouldDeleteNSGRule = true
								break //shared nsg rule has only one entry and entry owned by deleted svc has been found. skip the rest of the entries
							} else {
								newDestinations := append(existingPrefixes[:addressIndex], existingPrefixes[addressIndex+1:]...)
								sharedRule.DestinationAddressPrefixes = &newDestinations
								updatedRules[sharedIndex] = sharedRule
							}
							dirtySg = true
						}
					}

					if shouldDeleteNSGRule {
						klog.V(4).Infof("shared rule will be deleted because last service %s which refers this rule is deleted.", service.Name)
						updatedRules = append(updatedRules[:sharedIndex], updatedRules[sharedIndex+1:]...)
						dirtySg = true
						continue
					}
				}
			}
		}
	}
	v4Enabled, v6Enabled := getIPFamiliesEnabled(service)
	if v4Enabled {
		handleRule(consts.IPVersionIPv4)
	}
	if v6Enabled {
		handleRule(consts.IPVersionIPv6)
	}

	// update security rules: prepare rules for consolidation
	for index, rule := range updatedRules {
		if allowsConsolidation(rule) {
			updatedRules[index] = makeConsolidatable(rule)
		}
	}
	for index, rule := range expectedSecurityRules {
		if allowsConsolidation(rule) {
			expectedSecurityRules[index] = makeConsolidatable(rule)
		}
	}
	// update security rules: add needed
	for _, expectedRule := range expectedSecurityRules {
		foundRule := false
		if findSecurityRule(updatedRules, expectedRule) {
			klog.V(10).Infof("reconcile(%s)(%t): sg rule(%s) - already exists", serviceName, wantLb, *expectedRule.Name)
			foundRule = true
		}
		if foundRule && allowsConsolidation(expectedRule) {
			index, _ := findConsolidationCandidate(updatedRules, expectedRule)
			if updatedRules[index].DestinationAddressPrefixes != nil {
				updatedRules[index] = consolidate(updatedRules[index], expectedRule)
			} else {
				updatedRules = append(updatedRules[:index], updatedRules[index+1:]...)
			}
			dirtySg = true
		}
		if !foundRule && wantLb {
			klog.V(10).Infof("reconcile(%s)(%t): sg rule(%s) - adding", serviceName, wantLb, *expectedRule.Name)

			nextAvailablePriority, err := getNextAvailablePriority(updatedRules)
			if err != nil {
				return false, nil, err
			}

			expectedRule.Priority = pointer.Int32(nextAvailablePriority)
			updatedRules = append(updatedRules, expectedRule)
			dirtySg = true
		}
	}

	updatedRules = removeDuplicatedSecurityRules(updatedRules)

	for _, r := range updatedRules {
		klog.V(10).Infof("Updated security rule while processing %s: %s:%s -> %s:%s", service.Name, logSafe(r.SourceAddressPrefix), logSafe(r.SourcePortRange), logSafeCollection(r.DestinationAddressPrefix, r.DestinationAddressPrefixes), logSafe(r.DestinationPortRange))
	}

	return dirtySg, updatedRules, nil
}

func (az *Cloud) getExpectedSecurityRules(wantLb bool, ports []v1.ServicePort, sourceAddressPrefixes []string, service *v1.Service, destinationIPAddresses []string, sourceRanges utilnet.IPNetSet, backendIPAddresses []string, disableFloatingIP, isIPv6 bool) ([]network.SecurityRule, error) {
	expectedSecurityRules := []network.SecurityRule{}

	if wantLb {
		expectedSecurityRules = make([]network.SecurityRule, len(ports)*len(sourceAddressPrefixes))

		for i, port := range ports {
			_, securityProto, _, err := getProtocolsFromKubernetesProtocol(port.Protocol)
			if err != nil {
				return nil, err
			}
			dstPort := port.Port
			if disableFloatingIP {
				dstPort = port.NodePort
			}
			for j := range sourceAddressPrefixes {
				ix := i*len(sourceAddressPrefixes) + j
				securityRuleName := az.getSecurityRuleName(service, port, sourceAddressPrefixes[j], isIPv6)
				nsgRule := network.SecurityRule{
					Name: pointer.String(securityRuleName),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:             *securityProto,
						SourcePortRange:      pointer.String("*"),
						DestinationPortRange: pointer.String(strconv.Itoa(int(dstPort))),
						SourceAddressPrefix:  pointer.String(sourceAddressPrefixes[j]),
						Access:               network.SecurityRuleAccessAllow,
						Direction:            network.SecurityRuleDirectionInbound,
					},
				}

				if len(destinationIPAddresses) == 1 && disableFloatingIP {
					nsgRule.DestinationAddressPrefixes = &(backendIPAddresses)
				} else if len(destinationIPAddresses) == 1 && !disableFloatingIP {
					// continue to use DestinationAddressPrefix to avoid NSG updates for existing rules.
					nsgRule.DestinationAddressPrefix = pointer.String(destinationIPAddresses[0])
				} else {
					nsgRule.DestinationAddressPrefixes = &(destinationIPAddresses)
				}
				expectedSecurityRules[ix] = nsgRule
			}
		}

		shouldAddDenyRule := false
		if len(sourceRanges) > 0 && !servicehelpers.IsAllowAll(sourceRanges) {
			if v, ok := service.Annotations[consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges]; ok && strings.EqualFold(v, consts.TrueAnnotationValue) {
				shouldAddDenyRule = true
			}
		}
		if shouldAddDenyRule {
			for _, port := range ports {
				_, securityProto, _, err := getProtocolsFromKubernetesProtocol(port.Protocol)
				if err != nil {
					return nil, err
				}
				securityRuleName := az.getSecurityRuleName(service, port, "deny_all", isIPv6)
				nsgRule := network.SecurityRule{
					Name: pointer.String(securityRuleName),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:             *securityProto,
						SourcePortRange:      pointer.String("*"),
						DestinationPortRange: pointer.String(strconv.Itoa(int(port.Port))),
						SourceAddressPrefix:  pointer.String("*"),
						Access:               network.SecurityRuleAccessDeny,
						Direction:            network.SecurityRuleDirectionInbound,
					},
				}
				if len(destinationIPAddresses) == 1 {
					// continue to use DestinationAddressPrefix to avoid NSG updates for existing rules.
					nsgRule.DestinationAddressPrefix = pointer.String(destinationIPAddresses[0])
				} else {
					nsgRule.DestinationAddressPrefixes = &(destinationIPAddresses)
				}
				expectedSecurityRules = append(expectedSecurityRules, nsgRule)
			}
		}
	}

	for _, r := range expectedSecurityRules {
		klog.V(10).Infof("Expecting security rule for %s: %s:%s -> %v %v :%s", service.Name, pointer.StringDeref(r.SourceAddressPrefix, ""), pointer.StringDeref(r.SourcePortRange, ""), pointer.StringDeref(r.DestinationAddressPrefix, ""), stringSlice(r.DestinationAddressPrefixes), pointer.StringDeref(r.DestinationPortRange, ""))
	}
	return expectedSecurityRules, nil
}
