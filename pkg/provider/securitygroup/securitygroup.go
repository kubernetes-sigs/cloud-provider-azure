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

package securitygroup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/go-logr/logr"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	fnutil "sigs.k8s.io/cloud-provider-azure/pkg/util/collectionutil"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/iputil"
)

const (
	SecurityRuleNamePrefix = "k8s-azure-lb"
	SecurityRuleNameSep    = "_"
)

// Refer: https://learn.microsoft.com/en-us/azure/azure-resource-manager/management/azure-subscription-service-limits?toc=%2Fazure%2Fvirtual-network%2Ftoc.json#azure-resource-manager-virtual-networking-limits
const (
	MaxSecurityRulesPerGroup              = 1_000
	MaxSecurityRuleSourceIPsPerGroup      = 4_000
	MaxSecurityRuleDestinationIPsPerGroup = 4_000
)

const (
	ServiceTagInternet = "Internet"
)

var (
	ErrInvalidSecurityGroup                                = fmt.Errorf("invalid SecurityGroup object")
	ErrSecurityRulePriorityExhausted                       = fmt.Errorf("security rule priority exhausted")
	ErrSecurityRuleSourceAddressesNotFromSameIPFamily      = fmt.Errorf("security rule source addresses must be from the same IP family")
	ErrSecurityRuleDestinationAddressesNotFromSameIPFamily = fmt.Errorf("security rule destination addresses must be from the same IP family")
	ErrSecurityRuleSourceAndDestinationNotFromSameIPFamily = fmt.Errorf("security rule source addresses and destination addresses must be from the same IP family")
)

type ruleSourceKey struct {
	protocol             armnetwork.SecurityRuleProtocol
	sourcePrefixes       string
	ignoreSourcePrefixes bool
}

// RetainedRulePorts maps (protocol, source prefixes, ignoreSourcePrefixes) to
// destination IPs, then ports. A true value means the IP and port should be kept.
type RetainedRulePorts map[ruleSourceKey]map[netip.Addr]map[int32]bool

// Add records ports to keep for the given protocol, source prefixes, and destination IPs.
func (mapping RetainedRulePorts) Add(protocol armnetwork.SecurityRuleProtocol, sources []string, destinations []netip.Addr, ports []int32) {
	key := ruleSourceKey{protocol: protocol, sourcePrefixes: strings.Join(NormalizeSecurityRuleAddressPrefixes(sources), ",")}
	mapping.add(key, destinations, ports)
}

// AddIgnoringSourcePrefixes records ports to keep for the protocol and destination IPs,
// regardless of the source prefixes on existing rules.
// Used to retain existing rules when another Service has conflicting source range settings.
func (mapping RetainedRulePorts) AddIgnoringSourcePrefixes(protocol armnetwork.SecurityRuleProtocol, destinations []netip.Addr, ports []int32) {
	key := ruleSourceKey{protocol: protocol, ignoreSourcePrefixes: true}
	mapping.add(key, destinations, ports)
}

func (mapping RetainedRulePorts) add(key ruleSourceKey, destinations []netip.Addr, ports []int32) {
	if mapping[key] == nil {
		mapping[key] = make(map[netip.Addr]map[int32]bool)
	}
	for _, destination := range destinations {
		if mapping[key][destination] == nil {
			mapping[key][destination] = make(map[int32]bool)
		}
		for _, port := range ports {
			mapping[key][destination][port] = true
		}
	}
}

// Retains reports whether to keep the destination IP and port for the given protocol
// and source prefixes.
func (mapping RetainedRulePorts) Retains(protocol armnetwork.SecurityRuleProtocol, sources []string, destination netip.Addr, port int32) bool {
	key := ruleSourceKey{protocol: protocol, sourcePrefixes: strings.Join(NormalizeSecurityRuleAddressPrefixes(sources), ",")}
	if mapping[key][destination][port] {
		return true
	}
	return mapping[ruleSourceKey{protocol: protocol, ignoreSourcePrefixes: true}][destination][port]
}

// RuleHelper manages security rules within a security group.
type RuleHelper struct {
	logger   logr.Logger
	sg       *armnetwork.SecurityGroup
	snapshot []byte

	// helper map to store the security rules
	// name -> security rule
	rules      map[string]*armnetwork.SecurityRule
	priorities map[int32]string
}

func NewSecurityGroupHelper(logger logr.Logger, sg *armnetwork.SecurityGroup) (*RuleHelper, error) {
	if sg == nil ||
		sg.Name == nil ||
		sg.Properties == nil ||
		sg.Properties.SecurityRules == nil {
		// In the real world, this should never happen.
		// Anyway, defensively check it.
		return nil, ErrInvalidSecurityGroup
	}
	var (
		rules      = make(map[string]*armnetwork.SecurityRule, len(sg.Properties.SecurityRules))
		priorities = make(map[int32]string, len(sg.Properties.SecurityRules))
	)
	for i := range sg.Properties.SecurityRules {
		rule := (sg.Properties.SecurityRules)[i]
		rules[*rule.Name] = rule
		priorities[*rule.Properties.Priority] = *rule.Name
	}

	snapshot := makeSecurityGroupSnapshot(sg)

	return &RuleHelper{
		logger: logger.WithName("RuleHelper"),
		sg:     sg,

		rules:      rules,
		priorities: priorities,
		snapshot:   snapshot,
	}, nil
}

type rulePriorityPrefer string

const (
	rulePriorityPreferFromStart rulePriorityPrefer = "from_start"
	rulePriorityPreferFromEnd   rulePriorityPrefer = "from_end"
)

// nextRulePriority returns the next available priority for a new rule.
// It takes a preference for whether to start from the beginning or end of the priority range.
func (helper *RuleHelper) nextRulePriority(prefer rulePriorityPrefer) (int32, error) {
	var (
		init, end = consts.LoadBalancerMinimumPriority, consts.LoadBalancerMaximumPriority
		delta     = 1
	)
	if prefer == rulePriorityPreferFromEnd {
		init, end, delta = end-1, init-1, -1
	}

	for init != end {
		p := int32(init)
		if _, found := helper.priorities[p]; found {
			init += delta
			continue
		}
		return p, nil
	}

	return 0, ErrSecurityRulePriorityExhausted
}

// getOrCreateRule returns an existing rule or create a new one if it doesn't exist.
func (helper *RuleHelper) getOrCreateRule(name string, priorityPrefer rulePriorityPrefer) (*armnetwork.SecurityRule, error) {
	logger := helper.logger.WithName("getOrCreateRule").WithValues("rule-name", name)

	if rule, found := helper.rules[name]; found {
		logger.V(4).Info("Found an existing rule", "priority", *rule.Properties.Priority)
		return rule, nil
	}

	priority, err := helper.nextRulePriority(priorityPrefer)
	if err != nil {
		// NOTE: right now it won't happen because the number of rules is limited.
		//       maxPriority[4096] - minPriority[500] > maxSecurityRulesPerGroup[1000]
		helper.logger.Error(err, "Failed to get an available rule priority")
		return nil, err
	}
	rule := &armnetwork.SecurityRule{
		Name: ptr.To(name),
		Properties: &armnetwork.SecurityRulePropertiesFormat{
			Priority: ptr.To(priority),
		},
	}

	helper.rules[name] = rule
	helper.priorities[priority] = name

	logger.V(4).Info("Adding a new rule", "rule-name", name, "priority", priority)

	return rule, nil
}

// addAllowRule adds a rule that allows certain traffic.
func (helper *RuleHelper) addAllowRule(
	protocol armnetwork.SecurityRuleProtocol,
	ipFamily iputil.Family,
	srcPrefixes []string,
	dstPrefixes []string,
	dstPorts []int32,
) error {
	name := GenerateAllowSecurityRuleName(protocol, ipFamily, srcPrefixes, dstPorts)
	rule, err := helper.getOrCreateRule(name, rulePriorityPreferFromStart)
	if err != nil {
		return err
	}
	dstPortRanges := fnutil.Map(func(p int32) string { return strconv.FormatInt(int64(p), 10) }, dstPorts)
	sort.Strings(dstPortRanges)

	rule.Properties.Protocol = to.Ptr(protocol)
	rule.Properties.Access = to.Ptr(armnetwork.SecurityRuleAccessAllow)
	rule.Properties.Direction = to.Ptr(armnetwork.SecurityRuleDirectionInbound)
	{
		// Source
		if len(srcPrefixes) == 1 {
			rule.Properties.SourceAddressPrefix = to.Ptr(srcPrefixes[0])
		} else {
			rule.Properties.SourceAddressPrefixes = to.SliceOfPtrs(srcPrefixes...)
		}
		rule.Properties.SourcePortRange = ptr.To("*")
	}
	{
		// Destination
		addresses := append(ListDestinationPrefixes(rule), dstPrefixes...)
		SetDestinationPrefixes(rule, addresses)
		SetDestinationPortRanges(rule, dstPorts)
	}

	helper.logger.V(4).Info("Patched a rule for allow", "rule-name", name)

	return nil
}

// AddRuleForAllowedServiceTag adds a rule for traffic from a certain service tag.
func (helper *RuleHelper) AddRuleForAllowedServiceTag(
	serviceTag string,
	protocol armnetwork.SecurityRuleProtocol,
	dstAddresses []netip.Addr,
	dstPorts []int32,
) error {
	if !iputil.AreAddressesFromSameFamily(dstAddresses) {
		return ErrSecurityRuleDestinationAddressesNotFromSameIPFamily
	}

	var (
		ipFamily    = iputil.FamilyOfAddr(dstAddresses[0])
		srcPrefixes = []string{serviceTag}
		dstPrefixes = fnutil.Map(func(ip netip.Addr) string { return ip.String() }, dstAddresses)
	)

	helper.logger.V(4).Info("Patching a rule for allowed service tag", "ip-family", ipFamily)

	return helper.addAllowRule(protocol, ipFamily, srcPrefixes, dstPrefixes, dstPorts)
}

// AddRuleForAllowedIPRanges adds a rule for traffic from certain IP ranges.
func (helper *RuleHelper) AddRuleForAllowedIPRanges(
	ipRanges []netip.Prefix,
	protocol armnetwork.SecurityRuleProtocol,
	dstAddresses []netip.Addr,
	dstPorts []int32,
) error {
	if !iputil.ArePrefixesFromSameFamily(ipRanges) {
		return ErrSecurityRuleSourceAddressesNotFromSameIPFamily
	}
	if !iputil.AreAddressesFromSameFamily(dstAddresses) {
		return ErrSecurityRuleDestinationAddressesNotFromSameIPFamily
	}
	if ipRanges[0].Addr().Is4() != dstAddresses[0].Is4() {
		return ErrSecurityRuleSourceAndDestinationNotFromSameIPFamily
	}

	var (
		ipFamily    = iputil.FamilyOfAddr(ipRanges[0].Addr())
		srcPrefixes = fnutil.Map(func(ip netip.Prefix) string { return ip.String() }, ipRanges)
		dstPrefixes = fnutil.Map(func(ip netip.Addr) string { return ip.String() }, dstAddresses)
	)

	helper.logger.V(4).Info("Patching a rule for allowed IP ranges", "ip-family", ipFamily)

	return helper.addAllowRule(protocol, ipFamily, srcPrefixes, dstPrefixes, dstPorts)
}

// AddRuleForDenyAll adds a rule to deny all traffic from the given destination addresses.
// NOTE:
// This rule is to limit the traffic inside the VNet.
// The traffic out of the VNet is already limited by rule `DenyAllInBound`.
func (helper *RuleHelper) AddRuleForDenyAll(dstAddresses []netip.Addr) error {
	if !iputil.AreAddressesFromSameFamily(dstAddresses) {
		return ErrSecurityRuleDestinationAddressesNotFromSameIPFamily
	}

	var (
		ipFamily = iputil.FamilyOfAddr(dstAddresses[0])
		ruleName = GenerateDenyAllSecurityRuleName(ipFamily)
	)

	helper.logger.V(4).Info("Patching a rule for deny all", "ip-family", ipFamily)

	rule, err := helper.getOrCreateRule(ruleName, rulePriorityPreferFromEnd)
	if err != nil {
		return err
	}
	rule.Properties.Protocol = to.Ptr(armnetwork.SecurityRuleProtocolAsterisk)
	rule.Properties.Access = to.Ptr(armnetwork.SecurityRuleAccessDeny)
	rule.Properties.Direction = to.Ptr(armnetwork.SecurityRuleDirectionInbound)
	{
		// Source
		rule.Properties.SourceAddressPrefix = ptr.To("*")
		rule.Properties.SourcePortRange = ptr.To("*")
	}
	{
		// Destination
		addresses := fnutil.Map(func(ip netip.Addr) string { return ip.String() }, dstAddresses)
		addresses = append(addresses, ListDestinationPrefixes(rule)...)
		SetDestinationPrefixes(rule, addresses)
		SetAsteriskDestinationPortRange(rule)
	}

	helper.logger.V(4).Info("Patched a rule for deny all", "rule-name", ptr.To(rule.Name))

	return nil
}

type splitAllowRule struct {
	ipFamily            iputil.Family
	sourcePrefixes      []string
	destinationPrefixes []string
	destinationPorts    []int32
}

// RemoveDestinationFromRules removes the given destination addresses from rules matching the given protocol.
// Destination ports matched by retainDstPorts are kept in allow rules.
// It may add a new rule if the original rule needs to be split.
func (helper *RuleHelper) RemoveDestinationFromRules(
	protocol armnetwork.SecurityRuleProtocol,
	dstPrefixes []string,
	retainDstPorts RetainedRulePorts,
) error {
	logger := helper.logger.WithName("RemoveDestinationFromRules").WithValues("protocol", protocol, "num-dst-prefixes", len(dstPrefixes))
	logger.V(10).Info("Cleaning destination from SecurityGroup")

	splitRulesByName := make(map[string]*splitAllowRule)
	for name, rule := range helper.rules {
		if !IsManagedSecurityRule(rule) {
			logger.V(4).Info("Skip rule with not-managed priority", "rule-name", name)
			continue
		}

		if *rule.Properties.Protocol != protocol {
			continue
		}
		splitRules, err := helper.removeDestinationFromRule(rule, dstPrefixes, retainDstPorts)
		if err != nil {
			logger.Error(err, "Failed to remove destination from rule", "rule-name", name)
			return err
		}
		for _, splitRule := range splitRules {
			name := GenerateAllowSecurityRuleName(protocol, splitRule.ipFamily, splitRule.sourcePrefixes, splitRule.destinationPorts)
			if splitRulesByName[name] == nil {
				splitRulesByName[name] = &splitAllowRule{
					ipFamily:         splitRule.ipFamily,
					sourcePrefixes:   splitRule.sourcePrefixes,
					destinationPorts: splitRule.destinationPorts,
				}
			}
			splitRulesByName[name].destinationPrefixes = append(splitRulesByName[name].destinationPrefixes, splitRule.destinationPrefixes...)
		}
	}

	splitRuleNames := make([]string, 0, len(splitRulesByName))
	for name := range splitRulesByName {
		splitRuleNames = append(splitRuleNames, name)
	}
	sort.Strings(splitRuleNames)
	// Add split rules after cleanup so priorities from deleted rules can be reused.
	for _, name := range splitRuleNames {
		splitRule := splitRulesByName[name]
		if err := helper.addAllowRule(protocol, splitRule.ipFamily, splitRule.sourcePrefixes, splitRule.destinationPrefixes, splitRule.destinationPorts); err != nil {
			logger.Error(err, "Failed to remove destination from rule", "rule-name", name)
			return err
		}
	}

	return nil
}

// RetainDestinationFromRules retains the given destination prefixes from the rules.
func (helper *RuleHelper) RetainDestinationFromRules(dstPrefixes []string) error {
	logger := helper.logger.WithName("RetainDestinationFromRules").WithValues("num-dst-prefixes", len(dstPrefixes))
	logger.V(10).Info("Start retaining")
	defer logger.V(10).Info("Completed retaining")

	for _, rule := range helper.rules {
		if !IsManagedSecurityRule(rule) {
			logger.V(4).Info("Skip rule with not-managed priority", "rule-name", *rule.Name)
			continue
		}

		var (
			currentPrefixes  = ListDestinationPrefixes(rule)
			expectedPrefixes = fnutil.Intersection(currentPrefixes, dstPrefixes) // The prefixes to keep.
		)

		if len(expectedPrefixes) == len(currentPrefixes) {
			continue
		}

		logger.Info("Removing destination prefixes from rule",
			"rule-name", *rule.Name,
			"current-prefixes", currentPrefixes,
			"retained-prefixes", dstPrefixes,
			"expected-prefixes", expectedPrefixes,
		)
		SetDestinationPrefixes(rule, expectedPrefixes)
	}

	return nil
}

// removeDestinationFromRule removes the given destination prefixes from a rule,
// keeping destination ports matched by retainDstPorts in allow rules.
// It returns split allow rules for destinations that retain only some original ports.
func (helper *RuleHelper) removeDestinationFromRule(rule *armnetwork.SecurityRule, prefixes []string, retainDstPorts RetainedRulePorts) ([]splitAllowRule, error) {
	logger := helper.logger.WithName("removeDestinationFromRule").WithValues("security-rule-name", rule.Name)

	var (
		prefixIndex     = fnutil.IndexSet(prefixes) // Used to check whether the prefix should be removed.
		currentPrefixes = ListDestinationPrefixes(rule)

		expectedPrefixes = prefixIndex.SubtractedBy(currentPrefixes) // The prefixes to keep.
		targetPrefixes   = prefixIndex.Intersection(currentPrefixes) // The prefixes to check for removal.
	)

	if len(targetPrefixes) == 0 {
		return nil, nil
	}

	// Clean DenyAll rule
	if *rule.Properties.Access == armnetwork.SecurityRuleAccessDeny {
		// Update the prefixes
		SetDestinationPrefixes(rule, expectedPrefixes)

		return nil, nil
	}

	// Clean Allow rule
	currentPorts, err := ListDestinationPortRanges(rule)
	if err != nil {
		// Skip the rule with invalid destination port ranges.
		// NOTE: cloud-provider would not create allow rules with `*` or `4000-5000` as destination port ranges.
		logger.Info("Skip because it contains `*` or port-ranges as destination port ranges.")
		return nil, nil
	}
	var (
		sourcePrefixes = ListSourcePrefixes(rule)
		splitRules     []splitAllowRule
	)

	for _, prefix := range targetPrefixes {
		addr, err := netip.ParseAddr(prefix)
		if err != nil {
			logger.Error(err, "Failed to parse dst IP address", "dst-ip", prefix)
			return nil, fmt.Errorf("parse prefix as IP address %q: %w", prefix, err)
		}
		var expectedPorts []int32 // The ports to keep for this destination.
		for _, port := range currentPorts {
			if retainDstPorts.Retains(*rule.Properties.Protocol, sourcePrefixes, addr, port) {
				expectedPorts = append(expectedPorts, port)
			}
		}
		if len(currentPorts) == len(expectedPorts) {
			// Keep the destination on the original rule to preserve its name and priority.
			expectedPrefixes = append(expectedPrefixes, prefix)
			continue
		}
		if len(expectedPorts) == 0 {
			// No ports are retained for this destination.
			continue
		}
		splitRules = append(splitRules, splitAllowRule{
			ipFamily:            iputil.FamilyOfAddr(addr),
			sourcePrefixes:      sourcePrefixes,
			destinationPrefixes: []string{prefix},
			destinationPorts:    expectedPorts,
		})
	}

	if len(expectedPrefixes) == len(currentPrefixes) {
		// Every destination remains on the original rule, so no update is needed.
		return nil, nil
	}

	// Update the prefixes
	SetDestinationPrefixes(rule, expectedPrefixes)
	if len(expectedPrefixes) == 0 && len(rule.Properties.DestinationApplicationSecurityGroups) == 0 {
		delete(helper.rules, *rule.Name)
		delete(helper.priorities, *rule.Properties.Priority)
	}
	return splitRules, nil
}

// SecurityGroup returns the underlying SecurityGroup object and a bool indicating whether any changes were made to the RuleHelper.
func (helper *RuleHelper) SecurityGroup() (*armnetwork.SecurityGroup, bool, error) {
	var (
		rv    = helper.sg
		rules = make([]*armnetwork.SecurityRule, 0, len(helper.rules))
	)
	for _, r := range helper.rules {
		var (
			dstAddresses = ListDestinationPrefixes(r)
			dstASGs      = r.Properties.DestinationApplicationSecurityGroups
		)

		if len(dstAddresses) == 0 && len(dstASGs) == 0 {
			// Skip the rule without destination prefixes.
			continue
		}
		rules = append(rules, r)
	}

	rv.Properties.SecurityRules = rules

	var (
		snapshot = makeSecurityGroupSnapshot(rv)
		updated  = !bytes.Equal(helper.snapshot, snapshot)
		nRules   int
		nSrcIPs  int
		nDstIPs  int
	)
	{
		// Check whether the SecurityGroup exceeds the limit.
		for _, rule := range rv.Properties.SecurityRules {
			nRules++
			if rule.Properties.SourceAddressPrefixes != nil {
				nSrcIPs += len(rule.Properties.SourceAddressPrefixes)
			}
			if rule.Properties.SourceAddressPrefix != nil {
				nSrcIPs++
			}
			if rule.Properties.DestinationAddressPrefixes != nil {
				nDstIPs += len(rule.Properties.DestinationAddressPrefixes)
			}
			if rule.Properties.DestinationAddressPrefix != nil {
				nDstIPs++
			}
		}
		helper.logger.V(10).Info("Checking the number of rules and IP addresses", "num-rules", nRules, "num-src-ips", nSrcIPs, "num-dst-ips", nDstIPs)
		if nRules > MaxSecurityRulesPerGroup {
			return nil, false, fmt.Errorf("exceeds the maximum number of rules (%d > %d)", nRules, MaxSecurityRulesPerGroup)
		}
		if nSrcIPs > MaxSecurityRuleSourceIPsPerGroup {
			return nil, false, fmt.Errorf("exceeds the maximum number of source IP addresses (%d > %d)", nSrcIPs, MaxSecurityRuleSourceIPsPerGroup)
		}
		if nDstIPs > MaxSecurityRuleDestinationIPsPerGroup {
			return nil, false, fmt.Errorf("exceeds the maximum number of destination IP addresses (%d > %d)", nDstIPs, MaxSecurityRuleDestinationIPsPerGroup)
		}
	}

	return rv, updated, nil
}

// makeSecurityGroupSnapshot returns a byte array as the snapshot of the given SecurityGroup.
// It's used to check if the SecurityGroup had been changed.
func makeSecurityGroupSnapshot(sg *armnetwork.SecurityGroup) []byte {
	sort.SliceStable(sg.Properties.SecurityRules, func(i, j int) bool {
		return *(sg.Properties.SecurityRules)[i].Properties.Priority < *(sg.Properties.SecurityRules)[j].Properties.Priority
	})
	snapshot, _ := json.Marshal(sg)
	return snapshot
}
