/*
Copyright 2024 The Kubernetes Authors.

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

package backendaddresspoolclient

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/onsi/gomega"
)

var (
	networkClientFactory *armnetwork.ClientFactory
	loadbalancerClient   *armnetwork.LoadBalancersClient
	loadBalancer         *armnetwork.LoadBalancer
	publicIPClient       *armnetwork.PublicIPAddressesClient
	publicip             *armnetwork.PublicIPAddress
	publicIPName         = "publicip"
)

func init() {
	additionalTestCases = func() {
	}

	beforeAllFunc = func(ctx context.Context) {
		networkClientOption := clientOption
		networkClientOption.Telemetry.ApplicationID = "ccm-network-client"
		networkClientFactory, err = armnetwork.NewClientFactory(subscriptionID, recorder.TokenCredential(), &arm.ClientOptions{
			ClientOptions: networkClientOption,
		})
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		publicIPClient = networkClientFactory.NewPublicIPAddressesClient()
		publicip, err = createPublicIP(ctx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		loadbalancerClient = networkClientFactory.NewLoadBalancersClient()
		loadBalancer, err = createLoadbalancer(ctx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())

		newResource = &armnetwork.BackendAddressPool{
			Name:       to.Ptr(resourceName),
			Properties: &armnetwork.BackendAddressPoolPropertiesFormat{},
		}

	}
	afterAllFunc = func(ctx context.Context) {
		pollerResp, err := loadbalancerClient.BeginDelete(ctx, resourceGroupName, loadbalancerName, nil)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = pollerResp.PollUntilDone(ctx, nil)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		pippollerResp, err := publicIPClient.BeginDelete(ctx, resourceGroupName, publicIPName, nil)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		_, err = pippollerResp.PollUntilDone(ctx, nil)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}
}

func createLoadbalancer(ctx context.Context) (*armnetwork.LoadBalancer, error) {
	pollerResp, err := loadbalancerClient.BeginCreateOrUpdate(
		ctx,
		resourceGroupName,
		loadbalancerName,
		armnetwork.LoadBalancer{
			Location: to.Ptr(location),
			SKU: &armnetwork.LoadBalancerSKU{
				Name: to.Ptr(armnetwork.LoadBalancerSKUNameStandard),
				Tier: to.Ptr(armnetwork.LoadBalancerSKUTierRegional),
			},
			Properties: &armnetwork.LoadBalancerPropertiesFormat{
				FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
					{
						Name: to.Ptr("frontendIPConfiguration"),
						Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
							PublicIPAddress: &armnetwork.PublicIPAddress{
								ID: to.Ptr("/subscriptions/" + subscriptionID + "/resourceGroups/" + resourceGroupName + "/providers/Microsoft.Network/publicIPAddresses/" + publicIPName),
							},
						},
					},
				},
			},
		},
		nil)
	if err != nil {
		return nil, err
	}

	resp, err := pollerResp.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &resp.LoadBalancer, nil
}

func createPublicIP(ctx context.Context) (*armnetwork.PublicIPAddress, error) {
	pollerResp, err := publicIPClient.BeginCreateOrUpdate(
		ctx,
		resourceGroupName,
		publicIPName,
		armnetwork.PublicIPAddress{
			Location: to.Ptr(location),
			SKU: &armnetwork.PublicIPAddressSKU{
				Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
				Tier: to.Ptr(armnetwork.PublicIPAddressSKUTierRegional),
			},
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{
				PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
				PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
			},
		},
		nil)

	if err != nil {
		return nil, err
	}

	resp, err := pollerResp.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &resp.PublicIPAddress, nil
}
