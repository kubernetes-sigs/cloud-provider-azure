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
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// ListManagedLBs invokes az.LoadBalancerClient.List and filter out
// those that are not managed by cloud provider azure or not associated to a managed VMSet.
func (az *Cloud) ListManagedLBs(ctx context.Context, service *v1.Service, nodes []*v1.Node, clusterName string) ([]*armnetwork.LoadBalancer, error) {
	allLBs, err := az.lbRepo.ListByResourceGroup(ctx, az.getLoadBalancerResourceGroup())
	if err != nil {
		return nil, err
	}

	if allLBs == nil {
		klog.Warningf("ListManagedLBs: no LBs found")
		return nil, nil
	}

	managedLBNames := utilsets.NewString(clusterName)
	managedLBs := make([]*armnetwork.LoadBalancer, 0)
	if strings.EqualFold(az.LoadBalancerSku, consts.LoadBalancerSkuBasic) {
		// return early if wantLb=false
		if nodes == nil {
			klog.V(4).Infof("ListManagedLBs: return all LBs in the resource group %s, including unmanaged LBs", az.getLoadBalancerResourceGroup())
			return allLBs, nil
		}

		agentPoolVMSetNamesMap := make(map[string]bool)
		agentPoolVMSetNames, err := az.VMSet.GetAgentPoolVMSetNames(ctx, nodes)
		if err != nil {
			return nil, fmt.Errorf("ListManagedLBs: failed to get agent pool vmSet names: %w", err)
		}

		if agentPoolVMSetNames != nil && len(*agentPoolVMSetNames) > 0 {
			for _, vmSetName := range *agentPoolVMSetNames {
				klog.V(6).Infof("ListManagedLBs: found agent pool vmSet name %s", vmSetName)
				agentPoolVMSetNamesMap[strings.ToLower(vmSetName)] = true
			}
		}

		for agentPoolVMSetName := range agentPoolVMSetNamesMap {
			managedLBNames.Insert(az.mapVMSetNameToLoadBalancerName(agentPoolVMSetName, clusterName))
		}
	}

	if az.UseMultipleStandardLoadBalancers() {
		for _, multiSLBConfig := range az.MultipleStandardLoadBalancerConfigurations {
			managedLBNames.Insert(multiSLBConfig.Name, fmt.Sprintf("%s%s", multiSLBConfig.Name, consts.InternalLoadBalancerNameSuffix))
		}
	}

	for _, lb := range allLBs {
		if managedLBNames.Has(trimSuffixIgnoreCase(ptr.Deref(lb.Name, ""), consts.InternalLoadBalancerNameSuffix)) {
			managedLBs = append(managedLBs, lb)
			klog.V(4).Infof("ListManagedLBs: found managed LB %s", ptr.Deref(lb.Name, ""))
		}
	}

	return managedLBs, nil
}

// MigrateToIPBasedBackendPoolAndWaitForCompletion use the migration API to migrate from
// NIC-based to IP-based LB backend pools. It also makes sure the number of IP addresses
// in the backend pools is expected.
func (az *Cloud) MigrateToIPBasedBackendPoolAndWaitForCompletion(
	ctx context.Context,
	lbName string, backendPoolNames []string, nicsCountMap map[string]int,
) error {
	if _, err := az.lbRepo.MigrateToIPBased(ctx, az.ResourceGroup, lbName, to.SliceOfPtrs(backendPoolNames...)); err != nil {
		backendPoolNamesStr := strings.Join(backendPoolNames, ",")
		klog.Errorf("MigrateToIPBasedBackendPoolAndWaitForCompletion: Failed to migrate to IP based backend pool for lb %s, backend pool %s: %s", lbName, backendPoolNamesStr, err)
		return err
	}

	succeeded := make(map[string]bool)
	for bpName := range nicsCountMap {
		succeeded[bpName] = false
	}

	err := wait.PollImmediateWithContext(ctx, 5*time.Second, 10*time.Minute, func(ctx context.Context) (done bool, err error) {
		for bpName, nicsCount := range nicsCountMap {
			if succeeded[bpName] {
				continue
			}

			bp, err := az.lbRepo.GetBackendPool(ctx, az.ResourceGroup, lbName, bpName)
			if err != nil {
				klog.Errorf("MigrateToIPBasedBackendPoolAndWaitForCompletion: Failed to get backend pool %s for lb %s: %s", bpName, lbName, err)
				return false, err
			}

			if countIPsOnBackendPool(bp) != nicsCount {
				klog.V(4).Infof("MigrateToIPBasedBackendPoolAndWaitForCompletion: Expected IPs %d, current IPs %d, will retry in 5s", nicsCount, countIPsOnBackendPool(bp))
				return false, nil
			}
			succeeded[bpName] = true
		}
		return true, nil
	})
	if err != nil {
		if errors.Is(err, wait.ErrWaitTimeout) {
			klog.Warningf("MigrateToIPBasedBackendPoolAndWaitForCompletion: Timeout waiting for migration to IP based backend pool for lb %s, backend pool %s", lbName, strings.Join(backendPoolNames, ","))
			return nil
		}

		klog.Errorf("MigrateToIPBasedBackendPoolAndWaitForCompletion: Failed to wait for migration to IP based backend pool for lb %s, backend pool %s: %s", lbName, strings.Join(backendPoolNames, ","), err.Error())
		return err
	}

	return nil
}

// isBackendPoolOnSameLB checks whether newBackendPoolID is on the same load balancer as existingBackendPools.
// Since both public and internal LBs are supported, lbName and lbName-internal are treated as same.
// If not same, the lbName for existingBackendPools would also be returned.
func isBackendPoolOnSameLB(newBackendPoolID string, existingBackendPools []string) (bool, string, error) {
	matches := backendPoolIDRE.FindStringSubmatch(newBackendPoolID)
	if len(matches) != 3 {
		return false, "", fmt.Errorf("new backendPoolID %q is in wrong format", newBackendPoolID)
	}

	newLBName := matches[1]
	newLBNameTrimmed := trimSuffixIgnoreCase(newLBName, consts.InternalLoadBalancerNameSuffix)
	for _, backendPool := range existingBackendPools {
		matches := backendPoolIDRE.FindStringSubmatch(backendPool)
		if len(matches) != 3 {
			return false, "", fmt.Errorf("existing backendPoolID %q is in wrong format", backendPool)
		}

		lbName := matches[1]
		if !strings.EqualFold(trimSuffixIgnoreCase(lbName, consts.InternalLoadBalancerNameSuffix), newLBNameTrimmed) {
			return false, lbName, nil
		}
	}

	return true, "", nil
}

func (az *Cloud) serviceOwnsRule(service *v1.Service, rule string) bool {
	if !strings.EqualFold(string(service.Spec.ExternalTrafficPolicy), string(v1.ServiceExternalTrafficPolicyTypeLocal)) &&
		rule == consts.SharedProbeName {
		return true
	}
	prefix := az.getRulePrefix(service)
	return strings.HasPrefix(strings.ToUpper(rule), strings.ToUpper(prefix))
}

func isNICPool(bp *armnetwork.BackendAddressPool) bool {
	logger := klog.Background().WithName("isNICPool").WithValues("backendPoolName", ptr.Deref(bp.Name, ""))
	if bp.Properties != nil {
		for _, addr := range bp.Properties.LoadBalancerBackendAddresses {
			if ptr.Deref(addr.Properties.IPAddress, "") == "" {
				logger.V(4).Info("The load balancer backend address has empty ip address, assuming it is a NIC pool",
					"loadBalancerBackendAddress", ptr.Deref(addr.Name, ""))
				return true
			}
		}
	}
	return false
}
