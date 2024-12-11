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
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/utils/ptr"

	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
)

func (az *Cloud) findMatchedPIP(ctx context.Context, loadBalancerIP, pipName, pipResourceGroup string) (pip *armnetwork.PublicIPAddress, err error) {
	pips, err := az.pipRepo.ListByResourceGroup(ctx, pipResourceGroup, azcache.CacheReadTypeDefault)
	if err != nil {
		return nil, fmt.Errorf("findMatchedPIPByLoadBalancerIP: failed to listPIP: %w", err)
	}

	if loadBalancerIP != "" {
		pip, err = az.findMatchedPIPByLoadBalancerIP(ctx, pips, loadBalancerIP, pipResourceGroup)
		if err != nil {
			return nil, err
		}
		return pip, nil
	}

	if pipResourceGroup != "" {
		pip, err = az.findMatchedPIPByName(ctx, pips, pipName, pipResourceGroup)
		if err != nil {
			return nil, err
		}
	}
	return pip, nil
}

func (az *Cloud) findMatchedPIPByName(ctx context.Context, pips []*armnetwork.PublicIPAddress, pipName, pipResourceGroup string) (*armnetwork.PublicIPAddress, error) {
	for _, pip := range pips {
		if strings.EqualFold(ptr.Deref(pip.Name, ""), pipName) {
			return pip, nil
		}
	}

	pipList, err := az.pipRepo.ListByResourceGroup(ctx, pipResourceGroup, azcache.CacheReadTypeForceRefresh)
	if err != nil {
		return nil, fmt.Errorf("findMatchedPIPByName: failed to listPIP force refresh: %w", err)
	}
	for _, pip := range pipList {
		if strings.EqualFold(ptr.Deref(pip.Name, ""), pipName) {
			return pip, nil
		}
	}

	return nil, fmt.Errorf("findMatchedPIPByName: failed to find PIP %s in resource group %s", pipName, pipResourceGroup)
}

func (az *Cloud) findMatchedPIPByLoadBalancerIP(ctx context.Context, pips []*armnetwork.PublicIPAddress, loadBalancerIP, pipResourceGroup string) (*armnetwork.PublicIPAddress, error) {
	pip, err := getExpectedPIPFromListByIPAddress(pips, loadBalancerIP)
	if err != nil {
		pipList, err := az.pipRepo.ListByResourceGroup(ctx, pipResourceGroup, azcache.CacheReadTypeForceRefresh)
		if err != nil {
			return nil, fmt.Errorf("findMatchedPIPByLoadBalancerIP: failed to listPIP force refresh: %w", err)
		}

		pip, err = getExpectedPIPFromListByIPAddress(pipList, loadBalancerIP)
		if err != nil {
			return nil, fmt.Errorf("findMatchedPIPByLoadBalancerIP: cannot find public IP with IP address %s in resource group %s", loadBalancerIP, pipResourceGroup)
		}
	}

	return pip, nil
}

func getExpectedPIPFromListByIPAddress(pips []*armnetwork.PublicIPAddress, ip string) (*armnetwork.PublicIPAddress, error) {
	for _, pip := range pips {
		if pip.Properties.IPAddress != nil &&
			*pip.Properties.IPAddress == ip {
			return pip, nil
		}
	}

	return nil, fmt.Errorf("getExpectedPIPFromListByIPAddress: cannot find public IP with IP address %s", ip)
}
