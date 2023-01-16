/*
Copyright 2021 The Kubernetes Authors.

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

//go:generate sh -c "mockgen -destination=$GOPATH/src/sigs.k8s.io/cloud-provider-azure/pkg/provider/azure_mock_loadbalancer_backendpool.go -source=$GOPATH/src/sigs.k8s.io/cloud-provider-azure/pkg/provider/azure_loadbalancer_backendpool.go -package=provider BackendPool"

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
)

type BackendPool interface {
	// EnsureHostsInPool ensures the nodes join the backend pool of the load balancer
	EnsureHostsInPool(service *v1.Service, nodes []*v1.Node, backendPoolID, vmSetName, clusterName, lbName string, backendPool network.BackendAddressPool) error

	// CleanupVMSetFromBackendPoolByCondition removes nodes of the unwanted vmSet from the lb backend pool.
	// This is needed in two scenarios:
	// 1. When migrating from single SLB to multiple SLBs, the existing
	// SLB's backend pool contains nodes from different agent pools, while we only want the
	// nodes from the primary agent pool to join the backend pool.
	// 2. When migrating from dedicated SLB to shared SLB (or vice versa), we should move the vmSet from
	// one SLB to another one.
	CleanupVMSetFromBackendPoolByCondition(slb *network.LoadBalancer, service *v1.Service, nodes []*v1.Node, clusterName string, shouldRemoveVMSetFromSLB func(string) bool) (*network.LoadBalancer, error)

	// ReconcileBackendPools creates the inbound backend pool if it is not existed, and removes nodes that are supposed to be
	// excluded from the load balancers.
	ReconcileBackendPools(clusterName string, service *v1.Service, lb *network.LoadBalancer) (bool, bool, error)

	// GetBackendPrivateIPs returns the private IPs of LoadBalancer's backend pool
	GetBackendPrivateIPs(clusterName string, service *v1.Service, lb *network.LoadBalancer) ([]string, []string)
}

type backendPoolTypeNodeIPConfig struct {
	*Cloud
}

func newBackendPoolTypeNodeIPConfig(c *Cloud) BackendPool {
	return &backendPoolTypeNodeIPConfig{c}
}

func (bc *backendPoolTypeNodeIPConfig) EnsureHostsInPool(service *v1.Service, nodes []*v1.Node, backendPoolID, vmSetName, clusterName, lbName string, backendPool network.BackendAddressPool) error {
	return bc.VMSet.EnsureHostsInPool(service, nodes, backendPoolID, vmSetName)
}

func (bc *backendPoolTypeNodeIPConfig) CleanupVMSetFromBackendPoolByCondition(slb *network.LoadBalancer, service *v1.Service, nodes []*v1.Node, clusterName string, shouldRemoveVMSetFromSLB func(string) bool) (*network.LoadBalancer, error) {
	lbBackendPoolName := getBackendPoolName(clusterName, service)
	lbResourceGroup := bc.getLoadBalancerResourceGroup()
	lbBackendPoolID := bc.getBackendPoolID(pointer.StringDeref(slb.Name, ""), lbResourceGroup, lbBackendPoolName)
	newBackendPools := make([]network.BackendAddressPool, 0)
	if slb.LoadBalancerPropertiesFormat != nil && slb.BackendAddressPools != nil {
		newBackendPools = *slb.BackendAddressPools
	}
	vmSetNameToBackendIPConfigurationsToBeDeleted := make(map[string][]network.InterfaceIPConfiguration)

	for j, bp := range newBackendPools {
		if strings.EqualFold(pointer.StringDeref(bp.Name, ""), lbBackendPoolName) {
			klog.V(2).Infof("bc.CleanupVMSetFromBackendPoolByCondition: checking the backend pool %s from standard load balancer %s", pointer.StringDeref(bp.Name, ""), pointer.StringDeref(slb.Name, ""))
			if bp.BackendAddressPoolPropertiesFormat != nil && bp.BackendIPConfigurations != nil {
				for i := len(*bp.BackendIPConfigurations) - 1; i >= 0; i-- {
					ipConf := (*bp.BackendIPConfigurations)[i]
					ipConfigID := pointer.StringDeref(ipConf.ID, "")
					_, vmSetName, err := bc.VMSet.GetNodeNameByIPConfigurationID(ipConfigID)
					if err != nil && !errors.Is(err, cloudprovider.InstanceNotFound) {
						return nil, err
					}

					if shouldRemoveVMSetFromSLB(vmSetName) {
						klog.V(2).Infof("bc.CleanupVMSetFromBackendPoolByCondition: found unwanted vmSet %s, decouple it from the LB", vmSetName)
						// construct a backendPool that only contains the IP config of the node to be deleted
						interfaceIPConfigToBeDeleted := network.InterfaceIPConfiguration{
							ID: pointer.String(ipConfigID),
						}
						vmSetNameToBackendIPConfigurationsToBeDeleted[vmSetName] = append(vmSetNameToBackendIPConfigurationsToBeDeleted[vmSetName], interfaceIPConfigToBeDeleted)
						*bp.BackendIPConfigurations = append((*bp.BackendIPConfigurations)[:i], (*bp.BackendIPConfigurations)[i+1:]...)
					}
				}
			}

			newBackendPools[j] = bp
			break
		}
	}

	for vmSetName := range vmSetNameToBackendIPConfigurationsToBeDeleted {
		backendIPConfigurationsToBeDeleted := vmSetNameToBackendIPConfigurationsToBeDeleted[vmSetName]
		backendpoolToBeDeleted := &[]network.BackendAddressPool{
			{
				ID: pointer.String(lbBackendPoolID),
				BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
					BackendIPConfigurations: &backendIPConfigurationsToBeDeleted,
				},
			},
		}
		// decouple the backendPool from the node
		shouldRefreshLB, err := bc.VMSet.EnsureBackendPoolDeleted(service, lbBackendPoolID, vmSetName, backendpoolToBeDeleted, true)
		if err != nil {
			return nil, err
		}
		if shouldRefreshLB {
			slb, _, err = bc.getAzureLoadBalancer(pointer.StringDeref(slb.Name, ""), cache.CacheReadTypeForceRefresh)
			if err != nil {
				return nil, fmt.Errorf("bc.CleanupVMSetFromBackendPoolByCondition: failed to get load balancer %s, err: %w", pointer.StringDeref(slb.Name, ""), err)
			}
		}
	}

	return slb, nil
}

func (bc *backendPoolTypeNodeIPConfig) ReconcileBackendPools(clusterName string, service *v1.Service, lb *network.LoadBalancer) (bool, bool, error) {
	var newBackendPools []network.BackendAddressPool
	var err error
	if lb.BackendAddressPools != nil {
		newBackendPools = *lb.BackendAddressPools
	}

	var foundBackendPool, changed, shouldRefreshLB, isOperationSucceeded, isMigration bool
	lbName := *lb.Name

	serviceName := getServiceName(service)
	lbBackendPoolName := getBackendPoolName(clusterName, service)
	lbBackendPoolID := bc.getBackendPoolID(lbName, bc.getLoadBalancerResourceGroup(), lbBackendPoolName)
	vmSetName := bc.mapLoadBalancerNameToVMSet(lbName, clusterName)
	isBackendPoolPreConfigured := bc.isBackendPoolPreConfigured(service)

	mc := metrics.NewMetricContext("services", "migrate_to_ip_based_backend_pool", bc.ResourceGroup, bc.getNetworkResourceSubscriptionID(), serviceName)

	for i := len(newBackendPools) - 1; i >= 0; i-- {
		bp := newBackendPools[i]
		if strings.EqualFold(*bp.Name, lbBackendPoolName) {
			klog.V(10).Infof("bc.ReconcileBackendPools for service (%s): lb backendpool - found wanted backendpool. not adding anything", serviceName)
			foundBackendPool = true

			// Don't bother to remove unused nodeIPConfiguration if backend pool is pre configured
			if isBackendPoolPreConfigured {
				break
			}

			// If the LB backend pool type is configured from nodeIP or podIP
			// to nodeIPConfiguration, we need to decouple the VM NICs from the LB
			// before attaching nodeIPs/podIPs to the LB backend pool.
			if bp.BackendAddressPoolPropertiesFormat != nil &&
				bp.LoadBalancerBackendAddresses != nil &&
				len(*bp.LoadBalancerBackendAddresses) > 0 {
				isMigration = true

				if removeNodeIPAddressesFromBackendPool(bp, []string{}, true) {
					if err := bc.CreateOrUpdateLBBackendPool(lbName, bp); err != nil {
						klog.Errorf("bc.ReconcileBackendPools for service (%s): failed to cleanup IP based backend pool %s: %s", serviceName, lbBackendPoolName, err.Error())
						return false, false, fmt.Errorf("bc.ReconcileBackendPools for service (%s): failed to cleanup IP based backend pool %s: %w", serviceName, lbBackendPoolName, err)
					}
					newBackendPools[i] = bp
					lb.BackendAddressPools = &newBackendPools
					shouldRefreshLB = true
				}
			}

			var backendIPConfigurationsToBeDeleted []network.InterfaceIPConfiguration
			if bp.BackendAddressPoolPropertiesFormat != nil && bp.BackendIPConfigurations != nil {
				for _, ipConf := range *bp.BackendIPConfigurations {
					ipConfID := pointer.StringDeref(ipConf.ID, "")
					nodeName, _, err := bc.VMSet.GetNodeNameByIPConfigurationID(ipConfID)
					if err != nil {
						if errors.Is(err, cloudprovider.InstanceNotFound) {
							klog.V(2).Infof("bc.ReconcileBackendPools for service (%s): vm not found for ipConfID %s", serviceName, ipConfID)
							backendIPConfigurationsToBeDeleted = append(backendIPConfigurationsToBeDeleted, ipConf)
						} else {
							return false, false, err
						}
					}

					// If a node is not supposed to be included in the LB, it
					// would not be in the `nodes` slice. We need to check the nodes that
					// have been added to the LB's backendpool, find the unwanted ones and
					// delete them from the pool.
					shouldExcludeLoadBalancer, err := bc.ShouldNodeExcludedFromLoadBalancer(nodeName)
					if err != nil {
						klog.Errorf("bc.ReconcileBackendPools: ShouldNodeExcludedFromLoadBalancer(%s) failed with error: %v", nodeName, err)
						return false, false, err
					}
					if shouldExcludeLoadBalancer {
						klog.V(2).Infof("bc.ReconcileBackendPools for service (%s): lb backendpool - found unwanted node %s, decouple it from the LB %s", serviceName, nodeName, lbName)
						// construct a backendPool that only contains the IP config of the node to be deleted
						backendIPConfigurationsToBeDeleted = append(backendIPConfigurationsToBeDeleted, network.InterfaceIPConfiguration{ID: pointer.String(ipConfID)})
					}
				}
			}
			if len(backendIPConfigurationsToBeDeleted) > 0 {
				backendpoolToBeDeleted := &[]network.BackendAddressPool{
					{
						ID: pointer.String(lbBackendPoolID),
						BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
							BackendIPConfigurations: &backendIPConfigurationsToBeDeleted,
						},
					},
				}
				// decouple the backendPool from the node
				updated, err := bc.VMSet.EnsureBackendPoolDeleted(service, lbBackendPoolID, vmSetName, backendpoolToBeDeleted, false)
				if err != nil {
					return false, false, err
				}
				if updated {
					shouldRefreshLB = true
				}
			}
			break
		} else {
			klog.V(10).Infof("bc.ReconcileBackendPools for service (%s): lb backendpool - found unmanaged backendpool %s", serviceName, *bp.Name)
		}
	}

	if shouldRefreshLB {
		lb, _, err = bc.getAzureLoadBalancer(lbName, cache.CacheReadTypeForceRefresh)
		if err != nil {
			return false, false, fmt.Errorf("bc.ReconcileBackendPools for service (%s): failed to get loadbalancer %s: %w", serviceName, lbName, err)
		}
	}

	if !foundBackendPool {
		isBackendPoolPreConfigured = newBackendPool(lb, isBackendPoolPreConfigured, bc.PreConfiguredBackendPoolLoadBalancerTypes, getServiceName(service), getBackendPoolName(clusterName, service))
		changed = true
	}

	if isMigration {
		defer func() {
			mc.ObserveOperationWithResult(isOperationSucceeded)
		}()
	}

	isOperationSucceeded = true
	return isBackendPoolPreConfigured, changed, err
}

func (bc *backendPoolTypeNodeIPConfig) GetBackendPrivateIPs(clusterName string, service *v1.Service, lb *network.LoadBalancer) ([]string, []string) {
	serviceName := getServiceName(service)
	lbBackendPoolName := getBackendPoolName(clusterName, service)
	if lb.LoadBalancerPropertiesFormat == nil || lb.LoadBalancerPropertiesFormat.BackendAddressPools == nil {
		return nil, nil
	}

	backendPrivateIPv4s, backendPrivateIPv6s := sets.NewString(), sets.NewString()
	for _, bp := range *lb.BackendAddressPools {
		if strings.EqualFold(pointer.StringDeref(bp.Name, ""), lbBackendPoolName) {
			klog.V(10).Infof("bc.GetBackendPrivateIPs for service (%s): found wanted backendpool %s", serviceName, pointer.StringDeref(bp.Name, ""))
			if bp.BackendAddressPoolPropertiesFormat != nil && bp.BackendIPConfigurations != nil {
				for _, backendIPConfig := range *bp.BackendIPConfigurations {
					ipConfigID := pointer.StringDeref(backendIPConfig.ID, "")
					nodeName, _, err := bc.VMSet.GetNodeNameByIPConfigurationID(ipConfigID)
					if err != nil {
						klog.Errorf("bc.GetBackendPrivateIPs for service (%s): GetNodeNameByIPConfigurationID failed with error: %v", serviceName, err)
						continue
					}
					privateIPsSet, ok := bc.nodePrivateIPs[nodeName]
					if !ok {
						klog.Warningf("bc.GetBackendPrivateIPs for service (%s): failed to get private IPs of node %s", serviceName, nodeName)
						continue
					}
					privateIPs := privateIPsSet.List()
					for _, ip := range privateIPs {
						klog.V(2).Infof("bc.GetBackendPrivateIPs for service (%s): lb backendpool - found private IPs %s of node %s", serviceName, ip, nodeName)
						if utilnet.IsIPv4String(ip) {
							backendPrivateIPv4s.Insert(ip)
						} else {
							backendPrivateIPv6s.Insert(ip)
						}
					}
				}
			}
		} else {
			klog.V(10).Infof("bc.GetBackendPrivateIPs for service (%s): found unmanaged backendpool %s", serviceName, pointer.StringDeref(bp.Name, ""))
		}
	}
	return backendPrivateIPv4s.List(), backendPrivateIPv6s.List()
}

type backendPoolTypeNodeIP struct {
	*Cloud
}

func newBackendPoolTypeNodeIP(c *Cloud) BackendPool {
	return &backendPoolTypeNodeIP{c}
}

func (bi *backendPoolTypeNodeIP) EnsureHostsInPool(service *v1.Service, nodes []*v1.Node, backendPoolID, vmSetName, clusterName, lbName string, backendPool network.BackendAddressPool) error {
	vnetResourceGroup := bi.ResourceGroup
	if len(bi.VnetResourceGroup) > 0 {
		vnetResourceGroup = bi.VnetResourceGroup
	}
	vnetID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s", bi.SubscriptionID, vnetResourceGroup, bi.VnetName)

	changed := false
	numOfAdd := 0
	lbBackendPoolName := getBackendPoolName(clusterName, service)
	if strings.EqualFold(pointer.StringDeref(backendPool.Name, ""), lbBackendPoolName) &&
		backendPool.BackendAddressPoolPropertiesFormat != nil {
		if backendPool.LoadBalancerBackendAddresses == nil {
			lbBackendPoolAddresses := make([]network.LoadBalancerBackendAddress, 0)
			backendPool.LoadBalancerBackendAddresses = &lbBackendPoolAddresses
		}

		existingIPs := sets.NewString()
		for _, loadBalancerBackendAddress := range *backendPool.LoadBalancerBackendAddresses {
			if loadBalancerBackendAddress.LoadBalancerBackendAddressPropertiesFormat != nil &&
				loadBalancerBackendAddress.IPAddress != nil {
				klog.V(4).Infof("bi.EnsureHostsInPool: found existing IP %s in the backend pool %s", pointer.StringDeref(loadBalancerBackendAddress.IPAddress, ""), lbBackendPoolName)
				existingIPs.Insert(pointer.StringDeref(loadBalancerBackendAddress.IPAddress, ""))
			}
		}

		for _, node := range nodes {
			if isControlPlaneNode(node) {
				klog.V(4).Infof("bi.EnsureHostsInPool: skipping control plane node %s", node.Name)
				continue
			}

			var err error
			shouldSkip := false
			useSingleSLB := strings.EqualFold(bi.LoadBalancerSku, consts.LoadBalancerSkuStandard) && !bi.EnableMultipleStandardLoadBalancers
			if !useSingleSLB {
				vmSetName, err = bi.VMSet.GetNodeVMSetName(node)
				if err != nil {
					klog.Errorf("bi.EnsureHostsInPool: failed to get vmSet name by node name: %s", err.Error())
					return err
				}

				if !strings.EqualFold(vmSetName, bi.mapLoadBalancerNameToVMSet(lbName, clusterName)) {
					shouldSkip = true

					lbNamePrefix := strings.TrimSuffix(lbName, consts.InternalLoadBalancerNameSuffix)
					if strings.EqualFold(lbNamePrefix, clusterName) &&
						strings.EqualFold(bi.LoadBalancerSku, consts.LoadBalancerSkuStandard) &&
						bi.getVMSetNamesSharingPrimarySLB().Has(vmSetName) {
						klog.V(4).Infof("bi.EnsureHostsInPool: the node %s in VMSet %s is supposed to share the primary SLB", node.Name, vmSetName)
						shouldSkip = false
					}
				}
			}
			if shouldSkip {
				klog.V(4).Infof("bi.EnsureHostsInPool: skipping attaching node %s to lb %s, because the vmSet of the node is %s", node.Name, lbName, vmSetName)
				continue
			}

			privateIP := getNodePrivateIPAddress(service, node)
			if !existingIPs.Has(privateIP) {
				name := node.Name
				if utilnet.IsIPv6String(privateIP) {
					name = fmt.Sprintf("%s-ipv6", name)
				}

				klog.V(6).Infof("bi.EnsureHostsInPool: adding %s with ip address %s", name, privateIP)
				*backendPool.LoadBalancerBackendAddresses = append(*backendPool.LoadBalancerBackendAddresses, network.LoadBalancerBackendAddress{
					Name: pointer.String(name),
					LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
						IPAddress:      pointer.String(privateIP),
						VirtualNetwork: &network.SubResource{ID: pointer.String(vnetID)},
					},
				})
				numOfAdd++
				changed = true
			}
		}
	}
	if changed {
		klog.V(2).Infof("bi.EnsureHostsInPool: updating backend pool %s of load balancer %s to add %d nodes", lbBackendPoolName, lbName, numOfAdd)
		if err := bi.CreateOrUpdateLBBackendPool(lbName, backendPool); err != nil {
			return fmt.Errorf("bi.EnsureHostsInPool: failed to update backend pool %s: %w", lbBackendPoolName, err)
		}
	}

	return nil
}

func (bi *backendPoolTypeNodeIP) CleanupVMSetFromBackendPoolByCondition(slb *network.LoadBalancer, service *v1.Service, nodes []*v1.Node, clusterName string, shouldRemoveVMSetFromSLB func(string) bool) (*network.LoadBalancer, error) {
	lbBackendPoolName := getBackendPoolName(clusterName, service)
	newBackendPools := make([]network.BackendAddressPool, 0)
	if slb.LoadBalancerPropertiesFormat != nil && slb.BackendAddressPools != nil {
		newBackendPools = *slb.BackendAddressPools
	}

	var updatedPrivateIPs bool
	for j, bp := range newBackendPools {
		if strings.EqualFold(pointer.StringDeref(bp.Name, ""), lbBackendPoolName) {
			klog.V(2).Infof("bi.CleanupVMSetFromBackendPoolByCondition: checking the backend pool %s from standard load balancer %s", pointer.StringDeref(bp.Name, ""), pointer.StringDeref(slb.Name, ""))
			vmIPsToBeDeleted := sets.NewString()
			for _, node := range nodes {
				vmSetName, err := bi.VMSet.GetNodeVMSetName(node)
				if err != nil {
					return nil, err
				}

				if shouldRemoveVMSetFromSLB(vmSetName) {
					privateIP := getNodePrivateIPAddress(service, node)
					klog.V(4).Infof("bi.CleanupVMSetFromBackendPoolByCondition: removing ip %s from the backend pool %s", privateIP, lbBackendPoolName)
					vmIPsToBeDeleted.Insert(privateIP)
				}
			}

			if bp.BackendAddressPoolPropertiesFormat != nil && bp.LoadBalancerBackendAddresses != nil {
				for i := len(*bp.LoadBalancerBackendAddresses) - 1; i >= 0; i-- {
					if (*bp.LoadBalancerBackendAddresses)[i].LoadBalancerBackendAddressPropertiesFormat != nil &&
						vmIPsToBeDeleted.Has(pointer.StringDeref((*bp.LoadBalancerBackendAddresses)[i].IPAddress, "")) {
						*bp.LoadBalancerBackendAddresses = append((*bp.LoadBalancerBackendAddresses)[:i], (*bp.LoadBalancerBackendAddresses)[i+1:]...)
						updatedPrivateIPs = true
					}
				}
			}

			newBackendPools[j] = bp
			break
		}
	}
	if updatedPrivateIPs {
		klog.V(2).Infof("bi.CleanupVMSetFromBackendPoolByCondition: updating lb %s since there are private IP updates", pointer.StringDeref(slb.Name, ""))
		slb.BackendAddressPools = &newBackendPools

		for _, backendAddressPool := range *slb.BackendAddressPools {
			if strings.EqualFold(lbBackendPoolName, pointer.StringDeref(backendAddressPool.Name, "")) {
				if err := bi.CreateOrUpdateLBBackendPool(pointer.StringDeref(slb.Name, ""), backendAddressPool); err != nil {
					return nil, fmt.Errorf("bi.CleanupVMSetFromBackendPoolByCondition: failed to create or update backend pool %s: %w", lbBackendPoolName, err)
				}
			}
		}
	}

	return slb, nil
}

func (bi *backendPoolTypeNodeIP) ReconcileBackendPools(clusterName string, service *v1.Service, lb *network.LoadBalancer) (bool, bool, error) {
	var newBackendPools []network.BackendAddressPool
	if lb.BackendAddressPools != nil {
		newBackendPools = *lb.BackendAddressPools
	}

	var foundBackendPool, changed, shouldRefreshLB, isOperationSucceeded, isMigration bool
	lbName := *lb.Name
	serviceName := getServiceName(service)
	lbBackendPoolName := getBackendPoolName(clusterName, service)
	vmSetName := bi.mapLoadBalancerNameToVMSet(lbName, clusterName)
	lbBackendPoolID := bi.getBackendPoolID(pointer.StringDeref(lb.Name, ""), bi.getLoadBalancerResourceGroup(), getBackendPoolName(clusterName, service))
	isBackendPoolPreConfigured := bi.isBackendPoolPreConfigured(service)

	mc := metrics.NewMetricContext("services", "migrate_to_nic_based_backend_pool", bi.ResourceGroup, bi.getNetworkResourceSubscriptionID(), serviceName)

	var err error
	for i := len(newBackendPools) - 1; i >= 0; i-- {
		bp := newBackendPools[i]
		if strings.EqualFold(*bp.Name, lbBackendPoolName) {
			klog.V(10).Infof("bi.ReconcileBackendPools for service (%s): found wanted backendpool. not adding anything", serviceName)
			foundBackendPool = true

			// Don't bother to remove unused nodeIP if backend pool is pre configured
			if isBackendPoolPreConfigured {
				break
			}

			// If the LB backend pool type is configured from nodeIPConfiguration
			// to nodeIP, we need to decouple the VM NICs from the LB
			// before attaching nodeIPs/podIPs to the LB backend pool.
			klog.V(2).Infof("bi.ReconcileBackendPools for service (%s) and vmSet (%s): ensuring the LB is decoupled from the VMSet", serviceName, vmSetName)
			shouldRefreshLB, err = bi.VMSet.EnsureBackendPoolDeleted(service, lbBackendPoolID, vmSetName, lb.BackendAddressPools, true)
			if err != nil {
				klog.Errorf("bi.ReconcileBackendPools for service (%s): failed to EnsureBackendPoolDeleted: %s", serviceName, err.Error())
				return false, false, err
			}

			var nodeIPAddressesToBeDeleted []string
			for nodeName := range bi.excludeLoadBalancerNodes {
				for ip := range bi.nodePrivateIPs[nodeName] {
					klog.V(2).Infof("bi.ReconcileBackendPools for service (%s): found unwanted node private IP %s, decoupling it from the LB %s", serviceName, ip, lbName)
					nodeIPAddressesToBeDeleted = append(nodeIPAddressesToBeDeleted, ip)
				}
			}
			if len(nodeIPAddressesToBeDeleted) > 0 {
				isMigration = true

				updated := removeNodeIPAddressesFromBackendPool(bp, nodeIPAddressesToBeDeleted, false)
				if updated {
					(*lb.BackendAddressPools)[i] = bp
					if err := bi.CreateOrUpdateLBBackendPool(lbName, bp); err != nil {
						return false, false, fmt.Errorf("bi.ReconcileBackendPools for service (%s): lb backendpool - failed to update backend pool %s for load balancer %s: %w", serviceName, lbBackendPoolName, lbName, err)
					}
					shouldRefreshLB = true
				}
			}
			break
		} else {
			klog.V(10).Infof("bi.ReconcileBackendPools for service (%s): found unmanaged backendpool %s", serviceName, *bp.Name)
		}
	}

	if shouldRefreshLB {
		lb, _, err = bi.getAzureLoadBalancer(lbName, cache.CacheReadTypeForceRefresh)
		if err != nil {
			return false, false, fmt.Errorf("bi.ReconcileBackendPools for service (%s): failed to get load balancer %s: %w", serviceName, lbName, err)
		}
	}

	if !foundBackendPool {
		isBackendPoolPreConfigured = newBackendPool(lb, isBackendPoolPreConfigured, bi.PreConfiguredBackendPoolLoadBalancerTypes, getServiceName(service), getBackendPoolName(clusterName, service))
		changed = true
	}

	if isMigration {
		defer func() {
			mc.ObserveOperationWithResult(isOperationSucceeded)
		}()
	}

	isOperationSucceeded = true
	return isBackendPoolPreConfigured, changed, nil
}

func (bi *backendPoolTypeNodeIP) GetBackendPrivateIPs(clusterName string, service *v1.Service, lb *network.LoadBalancer) ([]string, []string) {
	serviceName := getServiceName(service)
	lbBackendPoolName := getBackendPoolName(clusterName, service)
	if lb.LoadBalancerPropertiesFormat == nil || lb.LoadBalancerPropertiesFormat.BackendAddressPools == nil {
		return nil, nil
	}

	backendPrivateIPv4s, backendPrivateIPv6s := sets.NewString(), sets.NewString()
	for _, bp := range *lb.BackendAddressPools {
		if strings.EqualFold(pointer.StringDeref(bp.Name, ""), lbBackendPoolName) {
			klog.V(10).Infof("bi.GetBackendPrivateIPs for service (%s): found wanted backendpool %s", serviceName, pointer.StringDeref(bp.Name, ""))
			if bp.BackendAddressPoolPropertiesFormat != nil && bp.LoadBalancerBackendAddresses != nil {
				for _, backendAddress := range *bp.LoadBalancerBackendAddresses {
					ipAddress := backendAddress.IPAddress
					if ipAddress != nil {
						klog.V(2).Infof("bi.GetBackendPrivateIPs for service (%s): lb backendpool - found private IP %q", serviceName, *ipAddress)
						if utilnet.IsIPv4String(*ipAddress) {
							backendPrivateIPv4s.Insert(*ipAddress)
						} else {
							backendPrivateIPv6s.Insert(*ipAddress)
						}
					} else {
						klog.V(4).Infof("bi.GetBackendPrivateIPs for service (%s): lb backendpool - found null private IP")
					}
				}
			}
		} else {
			klog.V(10).Infof("bi.GetBackendPrivateIPs for service (%s): found unmanaged backendpool %s", serviceName, pointer.StringDeref(bp.Name, ""))
		}
	}
	return backendPrivateIPv4s.List(), backendPrivateIPv6s.List()
}

func newBackendPool(lb *network.LoadBalancer, isBackendPoolPreConfigured bool, preConfiguredBackendPoolLoadBalancerTypes, serviceName, lbBackendPoolName string) bool {
	if isBackendPoolPreConfigured {
		klog.V(2).Infof("newBackendPool for service (%s)(true): lb backendpool - PreConfiguredBackendPoolLoadBalancerTypes %s has been set but can not find corresponding backend pool, ignoring it",
			serviceName,
			preConfiguredBackendPoolLoadBalancerTypes)
		isBackendPoolPreConfigured = false
	}

	if lb.BackendAddressPools == nil {
		lb.BackendAddressPools = &[]network.BackendAddressPool{}
	}
	*lb.BackendAddressPools = append(*lb.BackendAddressPools, network.BackendAddressPool{
		Name:                               pointer.String(lbBackendPoolName),
		BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{},
	})

	return isBackendPoolPreConfigured
}

func removeNodeIPAddressesFromBackendPool(backendPool network.BackendAddressPool, nodeIPAddresses []string, removeAll bool) bool {
	changed := false
	nodeIPsSet := sets.NewString(nodeIPAddresses...)
	if backendPool.BackendAddressPoolPropertiesFormat != nil &&
		backendPool.LoadBalancerBackendAddresses != nil {
		for i := len(*backendPool.LoadBalancerBackendAddresses) - 1; i >= 0; i-- {
			if (*backendPool.LoadBalancerBackendAddresses)[i].LoadBalancerBackendAddressPropertiesFormat != nil {
				ipAddress := pointer.StringDeref((*backendPool.LoadBalancerBackendAddresses)[i].IPAddress, "")
				if ipAddress == "" {
					klog.V(4).Infof("removeNodeIPAddressFromBackendPool: LoadBalancerBackendAddress %s is not IP-based, skipping", pointer.StringDeref((*backendPool.LoadBalancerBackendAddresses)[i].Name, ""))
					continue
				}
				if removeAll || nodeIPsSet.Has(ipAddress) {
					klog.V(4).Infof("removeNodeIPAddressFromBackendPool: removing %s from the backend pool %s", ipAddress, pointer.StringDeref(backendPool.Name, ""))
					*backendPool.LoadBalancerBackendAddresses = append((*backendPool.LoadBalancerBackendAddresses)[:i], (*backendPool.LoadBalancerBackendAddresses)[i+1:]...)
					changed = true
				}
			}
		}
	}

	return changed
}
