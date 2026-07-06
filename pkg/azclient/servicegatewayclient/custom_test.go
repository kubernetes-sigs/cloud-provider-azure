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

package servicegatewayclient

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	testVnetName   = "testVnet"
	testSubnetName = "testSubnet"
)

var (
	testVnetClient *armnetwork.VirtualNetworksClient
	testVnet       *armnetwork.VirtualNetwork
)

func init() {
	additionalTestCases = func() {
		When("UpdateTags is called on a non-existent resource", func() {
			It("should return an error", func(ctx context.Context) {
				_, err := realClient.UpdateTags(ctx, resourceGroupName, resourceName, armnetwork.TagsObject{
					Tags: map[string]*string{
						"key": to.Ptr("value"),
					},
				})
				Expect(err).To(HaveOccurred())
			})
		})

		When("GetAddressLocations is called on a non-existent resource", func() {
			It("should return an error", func(ctx context.Context) {
				result, err := realClient.GetAddressLocations(ctx, resourceGroupName, resourceName)
				Expect(err).To(HaveOccurred())
				Expect(result).To(BeNil())
			})
		})

		When("GetServices is called on a non-existent resource", func() {
			It("should return an error", func(ctx context.Context) {
				result, err := realClient.GetServices(ctx, resourceGroupName, resourceName)
				Expect(err).To(HaveOccurred())
				Expect(result).To(BeNil())
			})
		})

		When("UpdateAddressLocations is called on a non-existent resource", func() {
			It("should return an error", func(ctx context.Context) {
				err := realClient.UpdateAddressLocations(ctx, resourceGroupName, resourceName, armnetwork.ServiceGatewayUpdateAddressLocationsRequest{
					AddressLocations: []*armnetwork.ServiceGatewayAddressLocation{},
				})
				Expect(err).To(HaveOccurred())
			})
		})

		When("UpdateServices is called on a non-existent resource", func() {
			It("should return an error", func(ctx context.Context) {
				err := realClient.UpdateServices(ctx, resourceGroupName, resourceName, armnetwork.ServiceGatewayUpdateServicesRequest{
					ServiceRequests: []*armnetwork.ServiceGatewayServiceRequest{},
				})
				Expect(err).To(HaveOccurred())
			})
		})
	}

	beforeAllFunc = func(ctx context.Context) {
		networkClientOption := clientOption
		networkClientOption.Telemetry.ApplicationID = "ccm-network-client"
		testVnetClient, err = armnetwork.NewVirtualNetworksClient(recorder.SubscriptionID(), recorder.TokenCredential(), &arm.ClientOptions{
			ClientOptions: networkClientOption,
		})
		Expect(err).NotTo(HaveOccurred())

		vnet := armnetwork.VirtualNetwork{
			Location: to.Ptr(location),
			Properties: &armnetwork.VirtualNetworkPropertiesFormat{
				AddressSpace: &armnetwork.AddressSpace{
					AddressPrefixes: []*string{
						to.Ptr("10.0.0.0/16"),
					},
				},
				Subnets: []*armnetwork.Subnet{
					{
						Name: to.Ptr(testSubnetName),
						Properties: &armnetwork.SubnetPropertiesFormat{
							AddressPrefix: to.Ptr("10.0.0.0/24"),
						},
					},
				},
			},
		}
		poller, err := testVnetClient.BeginCreateOrUpdate(ctx, resourceGroupName, testVnetName, vnet, nil)
		Expect(err).NotTo(HaveOccurred())
		resp, err := poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{
			Frequency: 1 * time.Second,
		})
		Expect(err).NotTo(HaveOccurred())
		testVnet = &resp.VirtualNetwork

		newResource = &armnetwork.ServiceGateway{
			Location: to.Ptr(location),
			SKU: &armnetwork.ServiceGatewaySKU{
				Name: to.Ptr(armnetwork.ServiceGatewaySKUNameStandard),
				Tier: to.Ptr(armnetwork.ServiceGatewaySKUTierRegional),
			},
			Properties: &armnetwork.ServiceGatewayPropertiesFormat{
				VirtualNetwork: &armnetwork.VirtualNetwork{
					ID: testVnet.ID,
				},
				RouteTargetAddress: &armnetwork.RouteTargetAddressPropertiesFormat{
					PrivateIPAddress:          to.Ptr("10.0.0.4"),
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
					Subnet: &armnetwork.Subnet{
						ID: testVnet.Properties.Subnets[0].ID,
					},
				},
			},
		}
	}

	afterAllFunc = func(ctx context.Context) {
		poller, err := testVnetClient.BeginDelete(ctx, resourceGroupName, testVnetName, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = poller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{
			Frequency: 1 * time.Second,
		})
		Expect(err).NotTo(HaveOccurred())
	}
}
