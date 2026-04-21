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

package utils

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
)

var lbNameRE = regexp.MustCompile(`^/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Network/loadBalancers/(.+)/frontendIPConfigurations(?:.*)`)

// GetFIPIDForPrivateIP finds the frontend IP configuration ID for a given private IP.
func GetFIPIDForPrivateIP(lb *armnetwork.LoadBalancer, privateIP string) string {
	if lb.Properties == nil || lb.Properties.FrontendIPConfigurations == nil {
		return ""
	}
	for _, fip := range lb.Properties.FrontendIPConfigurations {
		if fip.Properties != nil && fip.Properties.PrivateIPAddress != nil {
			if *fip.Properties.PrivateIPAddress == privateIP {
				return ptr.Deref(fip.ID, "")
			}
		}
	}
	return ""
}

// VerifyFIPRemoved checks that the specified frontend IP configuration no longer exists on the LB.
// Returns nil if the FIP is gone (or the entire LB is gone). Returns an error if the FIP still exists.
func VerifyFIPRemoved(tc *AzureTestClient, fipID string) error {
	if fipID == "" {
		return fmt.Errorf("empty FIP ID")
	}

	match := lbNameRE.FindStringSubmatch(fipID)
	if len(match) != 2 {
		return fmt.Errorf("could not parse LB name from FIP ID: %s", fipID)
	}
	lbName := match[1]

	lb, err := tc.GetLoadBalancer(tc.GetResourceGroup(), lbName)
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") {
			Logf("LB %s not found, FIP %s is removed", lbName, fipID)
			return nil
		}
		return fmt.Errorf("failed to get load balancer %s: %w", lbName, err)
	}

	if lb.Properties != nil && lb.Properties.FrontendIPConfigurations != nil {
		for _, fip := range lb.Properties.FrontendIPConfigurations {
			if strings.EqualFold(ptr.Deref(fip.ID, ""), fipID) {
				var ruleIDs []string
				if fip.Properties != nil {
					for _, r := range fip.Properties.LoadBalancingRules {
						ruleIDs = append(ruleIDs, ptr.Deref(r.ID, "<unknown>"))
					}
				}
				return fmt.Errorf("FIP %s still exists on LB %s with rules %v", fipID, lbName, ruleIDs)
			}
		}
	}

	Logf("FIP %s has been removed from LB %s", fipID, lbName)
	return nil
}

// GetFIPRulePorts returns all ports that have load balancing rules for the given FIP and protocol.
// Returns (ports, lbExists, error). If lbExists is false, the LB doesn't exist (ports will be empty).
func GetFIPRulePorts(tc *AzureTestClient, fipID string, protocol string) (sets.Set[int32], bool, error) {
	if fipID == "" {
		return nil, false, fmt.Errorf("empty FIP ID")
	}

	match := lbNameRE.FindStringSubmatch(fipID)
	if len(match) != 2 {
		return nil, false, fmt.Errorf("could not parse LB name from FIP ID: %s", fipID)
	}
	lbName := match[1]

	lb, err := tc.GetLoadBalancer(tc.GetResourceGroup(), lbName)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "NotFound") {
			return sets.New[int32](), false, nil
		}
		return nil, false, fmt.Errorf("failed to get load balancer %s: %w", lbName, err)
	}

	// Find all rules that reference the target FIP ID with matching protocol.
	ports := sets.New[int32]()
	if lb.Properties != nil && lb.Properties.LoadBalancingRules != nil {
		for _, rule := range lb.Properties.LoadBalancingRules {
			if rule.Properties == nil || rule.Properties.FrontendIPConfiguration == nil {
				continue
			}
			ruleFIPID := ptr.Deref(rule.Properties.FrontendIPConfiguration.ID, "")
			if !strings.EqualFold(ruleFIPID, fipID) {
				continue
			}

			rulePort := ptr.Deref(rule.Properties.FrontendPort, 0)
			ruleProtocol := string(ptr.Deref(rule.Properties.Protocol, ""))

			if strings.EqualFold(ruleProtocol, protocol) {
				Logf("Found rule %q for port %d/%s on FIP", ptr.Deref(rule.Name, ""), rulePort, ruleProtocol)
				ports.Insert(rulePort)
			}
		}
	}

	return ports, true, nil
}

// VerifyFIPHasRulesForPorts checks that the specified frontend IP config has rules for exactly all expected ports.
func VerifyFIPHasRulesForPorts(tc *AzureTestClient, fipID string, expectedPorts sets.Set[int32], protocol string) error {
	Logf("Verifying FIP ID %q has rules for ports %v", fipID, expectedPorts.UnsortedList())

	actualPorts, lbExists, err := GetFIPRulePorts(tc, fipID, protocol)
	if err != nil {
		return err
	}
	if !lbExists {
		return fmt.Errorf("load balancer for FIP %q does not exist", fipID)
	}

	missingPorts := expectedPorts.Difference(actualPorts)
	extraPorts := actualPorts.Difference(expectedPorts)

	if missingPorts.Len() > 0 || extraPorts.Len() > 0 {
		return fmt.Errorf("FIP %q port mismatch: missing=%v, extra=%v, expected=%v, actual=%v",
			fipID, missingPorts.UnsortedList(), extraPorts.UnsortedList(),
			expectedPorts.UnsortedList(), actualPorts.UnsortedList())
	}

	Logf("FIP %q has exactly the expected rules for ports %v", fipID, expectedPorts.UnsortedList())
	return nil
}

// VerifyFIPHasNoRulesForPorts checks that the specified frontend IP config has no rules for the specified ports.
// If the LB does not exist, that counts as success (no rules).
func VerifyFIPHasNoRulesForPorts(tc *AzureTestClient, fipID string, absentPorts sets.Set[int32], protocol string) error {
	Logf("Verifying FIP ID %q has NO rules for ports %v", fipID, absentPorts.UnsortedList())

	actualPorts, lbExists, err := GetFIPRulePorts(tc, fipID, protocol)
	if err != nil {
		return err
	}
	if !lbExists {
		Logf("LB for FIP %q not found, treating as no rules", fipID)
		return nil
	}

	foundPorts := absentPorts.Intersection(actualPorts)
	if foundPorts.Len() > 0 {
		return fmt.Errorf("FIP %q still has rules for ports %v", fipID, foundPorts.UnsortedList())
	}

	Logf("FIP %q has no rules for ports %v (as expected)", fipID, absentPorts.UnsortedList())
	return nil
}
