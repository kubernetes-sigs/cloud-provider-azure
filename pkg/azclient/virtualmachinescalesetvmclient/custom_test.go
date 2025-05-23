// /*
// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// */

// Code generated by client-gen. DO NOT EDIT.
package virtualmachinescalesetvmclient

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	networkClientFactory  *armnetwork.ClientFactory
	computeClientFactory  *armcompute.ClientFactory
	virtualNetworksClient *armnetwork.VirtualNetworksClient
	vmssClient            *armcompute.VirtualMachineScaleSetsClient
	vNet                  *armnetwork.VirtualNetwork
	vmss                  *armcompute.VirtualMachineScaleSet
)

func init() {
	additionalTestCases = func() {
		When("get requests are raised", func() {
			It("should return 404 error", func(ctx context.Context) {
				newResource, err := realClient.GetInstanceView(ctx, resourceGroupName, virtualmachinescalesetName, resourceName)
				Expect(err).NotTo(HaveOccurred())
				Expect(newResource).NotTo(BeNil())
			})
		})

		When("list requests are raised", func() {
			It("should not return error", func(ctx context.Context) {
				resourceList, err := realClient.List(ctx, resourceGroupName, virtualmachinescalesetName)
				Expect(err).NotTo(HaveOccurred())
				Expect(resourceList).NotTo(BeNil())
				Expect(len(resourceList)).To(Equal(1))
			})
		})
		When("invalid list requests are raised", func() {
			It("should return error", func(ctx context.Context) {
				resourceList, err := realClient.List(ctx, resourceGroupName+"notfound", virtualmachinescalesetName)
				Expect(err).To(HaveOccurred())
				Expect(resourceList).To(BeNil())
			})
		})

		When("update requests are raised", func() {
			It("should not return error", func(ctx context.Context) {
				newResource, err := realClient.Update(ctx, resourceGroupName, virtualmachinescalesetName, "0", armcompute.VirtualMachineScaleSetVM{
					Tags: map[string]*string{
						"key1": to.Ptr("value1"),
					},
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(newResource).NotTo(BeNil())
			})
		})
		When("update requests are raised with invalid etag", func() {
			It("should return error", func(ctx context.Context) {
				_, err := realClient.Update(ctx, resourceGroupName, virtualmachinescalesetName, "0", armcompute.VirtualMachineScaleSetVM{
					Tags: map[string]*string{
						"key1": to.Ptr("value1"),
					},
					Etag: to.Ptr("invalid-etag"),
				})
				Expect(err).To(HaveOccurred())
			})
		})
	}

	beforeAllFunc = func(ctx context.Context) {
		networkClientOption := clientOption
		networkClientOption.Telemetry.ApplicationID = "ccm-network-client"
		networkClientFactory, err := armnetwork.NewClientFactory(recorder.SubscriptionID(), recorder.TokenCredential(), &arm.ClientOptions{
			ClientOptions: networkClientOption,
		})
		Expect(err).NotTo(HaveOccurred())

		virtualNetworksClient = networkClientFactory.NewVirtualNetworksClient()
		vnetpoller, err := virtualNetworksClient.BeginCreateOrUpdate(ctx, resourceGroupName, "vnet1", armnetwork.VirtualNetwork{
			Location: to.Ptr(location),
			Properties: &armnetwork.VirtualNetworkPropertiesFormat{
				AddressSpace: &armnetwork.AddressSpace{
					AddressPrefixes: []*string{
						to.Ptr("10.1.0.0/16"),
					},
				},
				Subnets: []*armnetwork.Subnet{
					{
						Name: to.Ptr("subnet1"),
						Properties: &armnetwork.SubnetPropertiesFormat{
							AddressPrefix: to.Ptr("10.1.0.0/24"),
						},
					},
				},
			},
		}, nil)
		Expect(err).NotTo(HaveOccurred())

		vnetresp, err := vnetpoller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{
			Frequency: 1 * time.Second,
		})
		Expect(err).NotTo(HaveOccurred())
		vNet = &vnetresp.VirtualNetwork
		computeClientOption := clientOption
		computeClientOption.Telemetry.ApplicationID = "ccm-computeClientOption-client"
		computeClientFactory, err := armcompute.NewClientFactory(recorder.SubscriptionID(), recorder.TokenCredential(), &arm.ClientOptions{
			ClientOptions: computeClientOption,
		})
		Expect(err).NotTo(HaveOccurred())
		vmssClient = computeClientFactory.NewVirtualMachineScaleSetsClient()

		vmsspoller, err := vmssClient.BeginCreateOrUpdate(ctx, resourceGroupName, virtualmachinescalesetName, armcompute.VirtualMachineScaleSet{
			Location: to.Ptr(location),
			SKU: &armcompute.SKU{
				Name:     to.Ptr(string(armcompute.VirtualMachineSizeTypesStandardD2SV3)),
				Capacity: to.Ptr[int64](1),
			},
			Properties: &armcompute.VirtualMachineScaleSetProperties{
				Overprovision: to.Ptr(false),
				UpgradePolicy: &armcompute.UpgradePolicy{
					Mode: to.Ptr(armcompute.UpgradeModeManual),
					AutomaticOSUpgradePolicy: &armcompute.AutomaticOSUpgradePolicy{
						EnableAutomaticOSUpgrade: to.Ptr(false),
						DisableAutomaticRollback: to.Ptr(false),
					},
				},
				VirtualMachineProfile: &armcompute.VirtualMachineScaleSetVMProfile{
					OSProfile: &armcompute.VirtualMachineScaleSetOSProfile{
						ComputerNamePrefix: to.Ptr("vmss"),
						AdminUsername:      to.Ptr("sample-user"),
						AdminPassword:      to.Ptr("Password01!@#"),
					},
					StorageProfile: &armcompute.VirtualMachineScaleSetStorageProfile{
						ImageReference: &armcompute.ImageReference{
							Offer:     to.Ptr("WindowsServer"),
							Publisher: to.Ptr("MicrosoftWindowsServer"),
							SKU:       to.Ptr("2019-Datacenter"),
							Version:   to.Ptr("latest"),
						},
					},
					NetworkProfile: &armcompute.VirtualMachineScaleSetNetworkProfile{
						NetworkInterfaceConfigurations: []*armcompute.VirtualMachineScaleSetNetworkConfiguration{
							{
								Name: to.Ptr("vmss1"),
								Properties: &armcompute.VirtualMachineScaleSetNetworkConfigurationProperties{
									Primary:            to.Ptr(true),
									EnableIPForwarding: to.Ptr(true),
									IPConfigurations: []*armcompute.VirtualMachineScaleSetIPConfiguration{
										{
											Name: to.Ptr("vmss1"),
											Properties: &armcompute.VirtualMachineScaleSetIPConfigurationProperties{
												Subnet: &armcompute.APIEntityReference{
													ID: vNet.Properties.Subnets[0].ID,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		vmssResp, err := vmsspoller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{
			Frequency: 1 * time.Second,
		})
		Expect(err).NotTo(HaveOccurred())
		vmss = &vmssResp.VirtualMachineScaleSet
		resourceName = "0"
	}
	afterAllFunc = func(ctx context.Context) {
		vmsspoller, err := vmssClient.BeginDelete(ctx, resourceGroupName, virtualmachinescalesetName, nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = vmsspoller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{
			Frequency: 1 * time.Second,
		})
		Expect(err).NotTo(HaveOccurred())
		vnetPoller, err := virtualNetworksClient.BeginDelete(ctx, resourceGroupName, "vnet1", nil)
		Expect(err).NotTo(HaveOccurred())
		_, err = vnetPoller.PollUntilDone(ctx, &runtime.PollUntilDoneOptions{
			Frequency: 1 * time.Second,
		})
		Expect(err).NotTo(HaveOccurred())
	}
}
