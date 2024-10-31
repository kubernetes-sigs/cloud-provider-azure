/*
Copyright 2022 The Kubernetes Authors.

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
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/subnetclient/mocksubnetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/privatelinkservice"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestReconcilePrivateLinkService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc              string
		annotations       map[string]string
		wantPLS           bool
		expectedSubnetGet bool
		existingSubnet    *network.Subnet
		expectedPLSList   bool
		existingPLSList   []*armnetwork.PrivateLinkService
		expectedPLSCreate bool
		expectedPLS       *armnetwork.PrivateLinkService
		expectedPLSDelete bool
		expectedError     bool
	}{
		{
			desc:    "reconcilePrivateLinkService should do nothing if service does not create any PLS",
			wantPLS: true,
		},
		{
			desc: "reconcilePrivateLinkService should return error if service requires PLS but needs external LB and floating ip enabled",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation: "true",
			},
			wantPLS:       true,
			expectedError: true,
		},
		{
			desc: "reconcilePrivateLinkService should create a new PLS for external service with floating ip disabled",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation:                   "true",
				consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true",
			},
			wantPLS:           true,
			expectedSubnetGet: true,
			existingSubnet: &network.Subnet{
				Name: ptr.To("subnet"),
				ID:   ptr.To("subnetID"),
				SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
					PrivateLinkServiceNetworkPolicies: network.VirtualNetworkPrivateLinkServiceNetworkPoliciesDisabled,
				},
			},
			expectedPLSList:   true,
			existingPLSList:   []*armnetwork.PrivateLinkService{},
			expectedPLSCreate: true,
			expectedPLS:       &armnetwork.PrivateLinkService{Name: ptr.To("pls-fipConfig")},
		},
		{
			desc: "reconcilePrivateLinkService should create a new PLS if no existing PLS attached to the LB frontend",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation:          "true",
				consts.ServiceAnnotationLoadBalancerInternal: "true",
				consts.ServiceAnnotationPLSName:              "testpls",
			},
			wantPLS:           true,
			expectedSubnetGet: true,
			existingSubnet: &network.Subnet{
				Name: ptr.To("subnet"),
				ID:   ptr.To("subnetID"),
				SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
					PrivateLinkServiceNetworkPolicies: network.VirtualNetworkPrivateLinkServiceNetworkPoliciesDisabled,
				},
			},
			expectedPLSList:   true,
			existingPLSList:   []*armnetwork.PrivateLinkService{},
			expectedPLSCreate: true,
			expectedPLS:       &armnetwork.PrivateLinkService{Name: ptr.To("testpls")},
		},
		{
			desc: "reconcilePrivateLinkService should report error if existing PLS attached to LB frontEnd is unmanaged",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation:          "true",
				consts.ServiceAnnotationLoadBalancerInternal: "true",
				consts.ServiceAnnotationPLSName:              "testpls",
			},
			wantPLS:         true,
			expectedPLSList: true,
			existingPLSList: []*armnetwork.PrivateLinkService{
				{
					Name: ptr.To("testpls"),
					Properties: &armnetwork.PrivateLinkServiceProperties{
						LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{ID: ptr.To("fipConfigID")}},
						IPConfigurations: []*armnetwork.PrivateLinkServiceIPConfiguration{
							{
								Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
									PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
									Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
									Primary:                   ptr.To(true),
									PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
								},
							},
						},
					},
				},
			},
			expectedError: true,
		},
		{
			desc: "reconcilePrivateLinkService should report error if service tries to update pls without being an owner",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation:          "true",
				consts.ServiceAnnotationLoadBalancerInternal: "true",
				consts.ServiceAnnotationPLSName:              "testpls",
			},
			wantPLS:         true,
			expectedPLSList: true,
			existingPLSList: []*armnetwork.PrivateLinkService{
				{
					Name: ptr.To("testpls"),
					Properties: &armnetwork.PrivateLinkServiceProperties{
						LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{ID: ptr.To("fipConfigID")}},
						IPConfigurations: []*armnetwork.PrivateLinkServiceIPConfiguration{
							{
								Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
									PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
									Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
									Primary:                   ptr.To(true),
									PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
								},
							},
						},
					},
					Tags: map[string]*string{
						consts.ClusterNameTagKey:  ptr.To(testClusterName),
						consts.OwnerServiceTagKey: ptr.To("default/test1"),
					},
				},
			},
			expectedError: true,
		},
		{
			desc: "reconcilePrivateLinkService should share existing pls to a service using the same LB frontEnd without any changes",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation:          "true",
				consts.ServiceAnnotationLoadBalancerInternal: "true",
			},
			wantPLS:         true,
			expectedPLSList: true,
			existingPLSList: []*armnetwork.PrivateLinkService{
				{
					Name: ptr.To("testpls"),
					Properties: &armnetwork.PrivateLinkServiceProperties{
						LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{ID: ptr.To("fipConfigID")}},
						IPConfigurations: []*armnetwork.PrivateLinkServiceIPConfiguration{
							{
								Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
									PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
									PrivateIPAddress:          ptr.To("10.2.0.4"),
									Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
									Primary:                   ptr.To(true),
									PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
								},
							},
						},
					},
					Tags: map[string]*string{
						consts.ClusterNameTagKey:  ptr.To(testClusterName),
						consts.OwnerServiceTagKey: ptr.To("default/test1"),
					},
				},
			},
		},
		{
			desc: "reconcilePrivateLinkService should not update an existing PLS if everything is same",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation:          "true",
				consts.ServiceAnnotationLoadBalancerInternal: "true",
				consts.ServiceAnnotationPLSName:              "testpls",
			},
			wantPLS:           true,
			expectedSubnetGet: true,
			existingSubnet: &network.Subnet{
				Name: ptr.To("subnet"),
				ID:   ptr.To("subnetID"),
				SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
					PrivateLinkServiceNetworkPolicies: network.VirtualNetworkPrivateLinkServiceNetworkPoliciesDisabled,
				},
			},
			expectedPLSList: true,
			existingPLSList: []*armnetwork.PrivateLinkService{
				{
					Name: ptr.To("testpls"),
					Properties: &armnetwork.PrivateLinkServiceProperties{
						LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{ID: ptr.To("fipConfigID")}},
						IPConfigurations: []*armnetwork.PrivateLinkServiceIPConfiguration{
							{
								Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
									PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
									Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
									Primary:                   ptr.To(true),
									PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
								},
							},
						},
					},
					Tags: map[string]*string{
						consts.ClusterNameTagKey:  ptr.To(testClusterName),
						consts.OwnerServiceTagKey: ptr.To("default/test"),
					},
				},
			},
		},
		{
			desc: "reconcilePrivateLinkService should update an existing PLS if some configuration gets changed",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation:                      "true",
				consts.ServiceAnnotationLoadBalancerInternal:             "true",
				consts.ServiceAnnotationPLSName:                          "testpls",
				consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "2",
			},
			wantPLS:           true,
			expectedSubnetGet: true,
			existingSubnet: &network.Subnet{
				Name: ptr.To("subnet"),
				ID:   ptr.To("subnetID"),
				SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
					PrivateLinkServiceNetworkPolicies: network.VirtualNetworkPrivateLinkServiceNetworkPoliciesDisabled,
				},
			},
			expectedPLSList: true,
			existingPLSList: []*armnetwork.PrivateLinkService{
				{
					Name: ptr.To("testpls"),
					Properties: &armnetwork.PrivateLinkServiceProperties{
						LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{ID: ptr.To("fipConfigID")}},
						IPConfigurations: []*armnetwork.PrivateLinkServiceIPConfiguration{
							{
								Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
									PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
									Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
									Primary:                   ptr.To(true),
									PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
								},
							},
						},
					},
					Tags: map[string]*string{
						consts.ClusterNameTagKey:  ptr.To(testClusterName),
						consts.OwnerServiceTagKey: ptr.To("default/test"),
					},
				},
			},
			expectedPLSCreate: true,
			expectedPLS:       &armnetwork.PrivateLinkService{Name: ptr.To("testpls")},
		},
		{
			desc: "reconcilePrivateLinkService should not do anything if no existing PLS attached to the LB frontend when deleting",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation:          "true",
				consts.ServiceAnnotationLoadBalancerInternal: "true",
				consts.ServiceAnnotationPLSName:              "testpls",
			},
			expectedPLSList: true,
			existingPLSList: []*armnetwork.PrivateLinkService{
				{
					Name: ptr.To("testpls"),
					Properties: &armnetwork.PrivateLinkServiceProperties{
						LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{ID: ptr.To("fipConfigID2")}},
					},
				},
			},
		},
		{
			desc: "reconcilePrivateLinkService should delete pls when frontend is deleted",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation:          "true",
				consts.ServiceAnnotationLoadBalancerInternal: "true",
				consts.ServiceAnnotationPLSName:              "testpls",
			},
			expectedPLSList: true,
			existingPLSList: []*armnetwork.PrivateLinkService{
				{
					Name: ptr.To("testpls"),
					Properties: &armnetwork.PrivateLinkServiceProperties{
						LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{ID: ptr.To("fipConfigID")}},
					},
				},
			},
			expectedPLSDelete: true,
		},
	}
	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			service := getTestServiceWithAnnotation("test", test.annotations, false, 80)
			fipConfig := &network.FrontendIPConfiguration{
				Name: ptr.To("fipConfig"),
				ID:   ptr.To("fipConfigID"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &network.PublicIPAddress{
						ID: ptr.To("pipID"),
						PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
							PublicIPAddressVersion: network.IPv4,
						},
					},
					PrivateIPAddressVersion: network.IPv4,
				},
			}
			clusterName := testClusterName

			mockSubnetsClient := az.SubnetsClient.(*mocksubnetclient.MockInterface)
			mockPLSRepo := az.plsRepo.(*privatelinkservice.MockRepository)
			if test.expectedSubnetGet {
				mockSubnetsClient.EXPECT().Get(gomock.Any(), "rg", "vnet", "subnet", "").Return(*test.existingSubnet, nil).MaxTimes(2)
			}
			if test.expectedPLSList {
				mockPLSRepo.EXPECT().Get(gomock.Any(), "rg", *fipConfig.ID, gomock.Any()).DoAndReturn(func(_ context.Context, _, frontendIPConfigID string, _ cache.AzureCacheReadType) (*armnetwork.PrivateLinkService, error) {
					for _, pls := range test.existingPLSList {
						if strings.EqualFold(*pls.Properties.LoadBalancerFrontendIPConfigurations[0].ID, frontendIPConfigID) {
							return pls, nil
						}
					}
					return &armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil
				}).MaxTimes(1)
				mockPLSRepo.EXPECT().List(gomock.Any(), "rg").Return(test.existingPLSList, nil).MaxTimes(1)
			}
			if test.expectedPLSCreate {
				mockPLSRepo.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any()).Return(nil, nil).Times(1)
			}
			if test.expectedPLSDelete {
				mockPLSRepo.EXPECT().Delete(gomock.Any(), "rg", "testpls", *fipConfig.ID).Return(nil).Times(1)
			}
			err := az.reconcilePrivateLinkService(context.TODO(), clusterName, &service, fipConfig, test.wantPLS)
			assert.Equal(t, test.expectedError, err != nil, "error: %v", err)
		})
	}
}

func TestGetPLSResourceGroup(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc        string
		annotations map[string]string
		expectedRG  string
	}{
		{
			desc: "getPLSResourceGroup should return resource group from annotation",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSResourceGroup: "testRG",
			},
			expectedRG: "testRG",
		},
		{
			desc:       "getPLSResourceGroup should return resource group from azure config when annotation is not set",
			expectedRG: "rg",
		},
	}
	for i, test := range testCases {
		az := GetTestCloud(ctrl)
		service := getTestServiceWithAnnotation("test", test.annotations, false, 80)
		rg := az.getPLSResourceGroup(&service)
		assert.Equal(t, test.expectedRG, rg, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestDisablePLSNetworkPolicy(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc                 string
		subnet               network.Subnet
		expectedSubnetUpdate bool
		expectedError        bool
	}{
		{
			desc: "disablePLSNetworkPolicy shall not update subnet if pls-network-policy is disabled",
			subnet: network.Subnet{
				Name: ptr.To("plsSubnet"),
				SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
					PrivateLinkServiceNetworkPolicies: network.VirtualNetworkPrivateLinkServiceNetworkPoliciesDisabled,
				},
			},
			expectedSubnetUpdate: false,
		},
		{
			desc: "disablePLSNetworkPolicy shall update subnet if pls-network-policy is enabled",
			subnet: network.Subnet{
				Name: ptr.To("plsSubnet"),
				SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
					PrivateLinkServiceNetworkPolicies: network.VirtualNetworkPrivateLinkServiceNetworkPoliciesEnabled,
				},
			},
			expectedSubnetUpdate: true,
		},
	}

	for i, test := range testCases {
		az := GetTestCloud(ctrl)
		service := &v1.Service{}
		service.Annotations = map[string]string{
			consts.ServiceAnnotationPLSIpConfigurationSubnet: "plsSubnet",
		}
		mockSubnetsClient := az.SubnetsClient.(*mocksubnetclient.MockInterface)
		mockSubnetsClient.EXPECT().Get(gomock.Any(), "rg", "vnet", "plsSubnet", "").Return(test.subnet, nil).Times(1)
		if test.expectedSubnetUpdate {
			mockSubnetsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "vnet", "plsSubnet", network.Subnet{
				Name: ptr.To("plsSubnet"),
				SubnetPropertiesFormat: &network.SubnetPropertiesFormat{
					PrivateLinkServiceNetworkPolicies: network.VirtualNetworkPrivateLinkServiceNetworkPoliciesDisabled,
				},
			}).Return(nil).Times(1)
		}
		err := az.disablePLSNetworkPolicy(service)
		assert.Equal(t, test.expectedError, err != nil, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestSafeDeletePLS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc          string
		pls           *armnetwork.PrivateLinkService
		expectedError bool
	}{
		{
			desc: "safeDeletePLS shall delete all PE connections and pls itself",
			pls: &armnetwork.PrivateLinkService{
				Name: ptr.To("testpls"),
				Properties: &armnetwork.PrivateLinkServiceProperties{
					LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{{ID: ptr.To("FipConfigID")}},
					PrivateEndpointConnections: []*armnetwork.PrivateEndpointConnection{
						{Name: ptr.To("pe1")},
						{Name: ptr.To("pe2")},
					},
				},
			},
		},
	}

	for i, test := range testCases {
		az := GetTestCloud(ctrl)
		mockPLSRepo := az.plsRepo.(*privatelinkservice.MockRepository)
		mockPLSRepo.EXPECT().DeletePEConnection(gomock.Any(), "rg", "testpls", "pe1").Return(nil).Times(1)
		mockPLSRepo.EXPECT().DeletePEConnection(gomock.Any(), "rg", "testpls", "pe2").Return(nil).Times(1)
		mockPLSRepo.EXPECT().Delete(gomock.Any(), "rg", "testpls", gomock.Any()).Return(nil).Times(1)
		service := getTestService("test1", v1.ProtocolTCP, nil, false, 80)
		rerr := az.safeDeletePLS(context.Background(), test.pls, &service)
		assert.Equal(t, test.expectedError, rerr != nil, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestGetPrivateLinkServiceName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	tests := []struct {
		desc         string
		annotations  map[string]string
		pls          *armnetwork.PrivateLinkService
		fipConfig    *network.FrontendIPConfiguration
		expectedName string
		expectedErr  bool
	}{
		{
			desc: "If pls name does not set, sets it as what service configures",
			pls:  &armnetwork.PrivateLinkService{},
			annotations: map[string]string{
				consts.ServiceAnnotationPLSName: "testpls",
			},
			expectedName: "testpls",
		},
		{
			desc: "If pls name does not set, and service does not configure, sets it as default(pls-fipConfigName)",
			pls:  &armnetwork.PrivateLinkService{},
			fipConfig: &network.FrontendIPConfiguration{
				Name: ptr.To("fipname"),
			},
			expectedName: "pls-fipname",
		},
		{
			desc: "If pls name is not equal to service configuration, error should be reported",
			pls: &armnetwork.PrivateLinkService{
				Name: ptr.To("testpls"),
			},
			annotations: map[string]string{
				consts.ServiceAnnotationPLSName: "testpls1",
			},
			expectedErr: true,
		},
		{
			desc: "If pls name is same as service configuration, simply return it",
			pls: &armnetwork.PrivateLinkService{
				Name: ptr.To("testpls"),
			},
			annotations: map[string]string{
				consts.ServiceAnnotationPLSName: "testpls",
			},
			expectedName: "testpls",
		},
	}
	for i, test := range tests {
		s := &v1.Service{}
		s.Annotations = test.annotations
		actualName, err := az.getPrivateLinkServiceName(test.pls, s, test.fipConfig)
		if test.expectedErr {
			assert.Error(t, err)
		} else {
			assert.NoError(t, err)
			assert.Equal(t, test.expectedName, actualName, "TestCase[%d]: %s", i, test.desc)
		}
	}
}

func TestGetExpectedPrivateLinkService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	t.Run("getExpectedPrivateLinkService correctly sets all configurations", func(t *testing.T) {
		cloud := GetTestCloud(ctrl)
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "service",
				Annotations: map[string]string{
					consts.ServiceAnnotationPLSIpConfigurationSubnet:         "subnet",
					consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "3",
					consts.ServiceAnnotationPLSIpConfigurationIPAddress:      "10.2.0.4 10.2.0.5",
					consts.ServiceAnnotationPLSFqdns:                         "fqdns1 fqdns2 fqdns3",
					consts.ServiceAnnotationPLSProxyProtocol:                 "true",
					consts.ServiceAnnotationPLSVisibility:                    "*",
					consts.ServiceAnnotationPLSAutoApproval:                  "sub1 sub2 sub3",
				},
			},
		}
		plsName := "testPLS"
		clusterName := testClusterName
		fipConfig := &network.FrontendIPConfiguration{ID: ptr.To("fipConfigID")}
		pls := &armnetwork.PrivateLinkService{Properties: &armnetwork.PrivateLinkServiceProperties{}}
		subnetClient := cloud.SubnetsClient.(*mocksubnetclient.MockInterface)
		subnetClient.EXPECT().Get(gomock.Any(), "rg", "vnet", "subnet", gomock.Any()).Return(
			network.Subnet{
				ID:   ptr.To("subnetID"),
				Name: ptr.To("subnet"),
			}, nil).MaxTimes(1)

		dirtyPLS, err := cloud.getExpectedPrivateLinkService(pls, &plsName, &clusterName, service, fipConfig)
		assert.NoError(t, err)
		assert.True(t, dirtyPLS)

		assert.Equal(t, pls.Name, &plsName)

		expectedLBFrontendConfig := []*armnetwork.FrontendIPConfiguration{{ID: fipConfig.ID}}
		assert.Equal(t, pls.Properties.LoadBalancerFrontendIPConfigurations, expectedLBFrontendConfig)

		expectedConfigs := []*armnetwork.PrivateLinkServiceIPConfiguration{
			{
				Name: ptr.To("subnet-testPLS-static-10.2.0.4"),
				Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
					PrivateIPAddress:          ptr.To("10.2.0.4"),
					Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
					Primary:                   ptr.To(true),
					PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
				},
			},
			{
				Name: ptr.To("subnet-testPLS-static-10.2.0.5"),
				Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
					PrivateIPAddress:          ptr.To("10.2.0.5"),
					Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
					Primary:                   ptr.To(false),
					PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
				},
			},
			{
				Name: ptr.To("subnet-testPLS-dynamic-0"),
				Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
					Primary:                   ptr.To(false),
					PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
				},
			},
		}

		testSamePLSIpConfigs(t, pls.Properties.IPConfigurations, expectedConfigs)

		expectedFqdns := to.SliceOfPtrs("fqdns1", "fqdns2", "fqdns3")
		assert.Equal(t, expectedFqdns, pls.Properties.Fqdns)

		assert.True(t, *pls.Properties.EnableProxyProtocol)

		expectedVisibility := to.SliceOfPtrs("*")
		assert.Equal(t, expectedVisibility, pls.Properties.Visibility.Subscriptions)

		expectedAutoApproval := to.SliceOfPtrs("sub1", "sub2", "sub3")
		assert.Equal(t, expectedAutoApproval, pls.Properties.AutoApproval.Subscriptions)
	})
}

func TestReconcilePLSIpConfigs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for i, test := range []struct {
		desc              string
		annotations       map[string]string
		plsName           string
		existingIPConfigs []*armnetwork.PrivateLinkServiceIPConfiguration
		expectedIPConfigs []*armnetwork.PrivateLinkServiceIPConfiguration
		getSubnetError    *retry.Error
		expectedChanged   bool
		expectedErr       bool
	}{
		{
			desc:    "reconcilePLSIpConfigs should report error when subnet specified by service does not exist",
			plsName: "testpls",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationSubnet: "subnet",
			},
			getSubnetError: &retry.Error{HTTPStatusCode: http.StatusNotFound},
			expectedErr:    true,
		},
		{
			desc:    "reconcilePLSIpConfigs should report error when ip count specified is fewer than number of static IPs",
			plsName: "testpls",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationSubnet:         "subnet",
				consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "1",
				consts.ServiceAnnotationPLSIpConfigurationIPAddress:      "10.2.0.4 10.2.0.5",
			},
			expectedErr: true,
		},
		{
			desc:    "reconcilePLSIpConfigs should change existingPLS if its ipConfig is nil",
			plsName: "testpls",
			expectedIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedChanged: true,
		},
		{
			desc:    "reconcilePLSIpConfigs should change existingPLS if its ipConfig count is different",
			plsName: "testpls",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "2",
			},
			existingIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
				{
					Name: ptr.To("subnet-testpls-dynamic-1"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(false),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedChanged: true,
		},
		{
			desc:    "reconcilePLSIpConfigs should change existingPLS if its subnetID is different",
			plsName: "testpls",
			existingIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID1")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedChanged: true,
		},
		{
			desc:    "reconcilePLSIpConfigs should change existingPLS if ip allocation type is changed from dynamic to static",
			plsName: "testpls",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "2",
				consts.ServiceAnnotationPLSIpConfigurationIPAddress:      "10.2.0.4",
			},
			existingIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-static-10.2.0.4"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						PrivateIPAddress:          ptr.To("10.2.0.4"),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
				{
					Name: ptr.To("subnet-testpls-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(false),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedChanged: true,
		},
		{
			desc:    "reconcilePLSIpConfigs should change existingPLS if ip allocation type is changed from static to dynamic",
			plsName: "testpls",
			existingIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-static-10.2.0.4"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						PrivateIPAddress:          ptr.To("10.2.0.4"),
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedChanged: true,
		},
		{
			desc:    "reconcilePLSIpConfigs should change existingPLS if static ip is changed",
			plsName: "testpls",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddress: "10.2.0.5",
			},
			existingIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-static-10.2.0.4"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						PrivateIPAddress:          ptr.To("10.2.0.4"),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-static-10.2.0.5"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						PrivateIPAddress:          ptr.To("10.2.0.5"),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedChanged: true,
		},
		{
			desc:    "reconcilePLSIpConfigs should not change existingPLS if ip allocation type is dynamic only",
			plsName: "testpls",
			existingIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
		},
		{
			desc:    "reconcilePLSIpConfigs should not change existingPLS if static ip is exactly same",
			plsName: "testpls",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddress: "10.2.0.5",
			},
			existingIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-static-10.2.0.5"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						PrivateIPAddress:          ptr.To("10.2.0.5"),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-testpls-static-10.2.0.5"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						PrivateIPAddress:          ptr.To("10.2.0.5"),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
		},
		{
			desc:    "reconcilePLSIpConfigs should truncate frontendIPConfig name if it's too long",
			plsName: strings.Repeat("12345678", 10),
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddress:      "10.2.0.5",
				consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "2",
			},
			existingIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{},
			expectedIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-" + strings.Repeat("12345678", 7) + "1" + "-static-10.2.0.5"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						PrivateIPAddress:          ptr.To("10.2.0.5"),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
				{
					Name: ptr.To("subnet-" + strings.Repeat("12345678", 7) + "1234567" + "-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(false),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedChanged: true,
		},
		{
			desc:    "reconcilePLSIpConfigs should not modify existingPLS in name truncation case",
			plsName: strings.Repeat("12345678", 10),
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddress:      "10.2.0.5",
				consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "2",
			},
			existingIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-" + strings.Repeat("12345678", 7) + "1" + "-static-10.2.0.5"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						PrivateIPAddress:          ptr.To("10.2.0.5"),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
				{
					Name: ptr.To("subnet-" + strings.Repeat("12345678", 7) + "1234567" + "-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(false),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedIPConfigs: []*armnetwork.PrivateLinkServiceIPConfiguration{
				{
					Name: ptr.To("subnet-" + strings.Repeat("12345678", 7) + "1" + "-static-10.2.0.5"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						PrivateIPAddress:          ptr.To("10.2.0.5"),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(true),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
				{
					Name: ptr.To("subnet-" + strings.Repeat("12345678", 7) + "1234567" + "-dynamic-0"),
					Properties: &armnetwork.PrivateLinkServiceIPConfigurationProperties{
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						Subnet:                    &armnetwork.Subnet{ID: ptr.To("subnetID")},
						Primary:                   ptr.To(false),
						PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
		},
	} {
		cloud := GetTestCloud(ctrl)
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   "default",
				Name:        "service",
				Annotations: test.annotations,
			},
		}
		pls := &armnetwork.PrivateLinkService{
			Name: ptr.To(test.plsName),
			Properties: &armnetwork.PrivateLinkServiceProperties{
				IPConfigurations: test.existingIPConfigs,
			},
		}
		subnetClient := cloud.SubnetsClient.(*mocksubnetclient.MockInterface)
		subnetClient.EXPECT().Get(gomock.Any(), "rg", "vnet", "subnet", gomock.Any()).Return(
			network.Subnet{
				ID:   ptr.To("subnetID"),
				Name: ptr.To("subnet"),
			}, test.getSubnetError).MaxTimes(1)

		changed, err := cloud.reconcilePLSIpConfigs(pls, service)
		if test.expectedErr {
			assert.Error(t, err, "TestCase[%d]: %s", i, test.desc)
		} else {
			assert.Equal(t, test.expectedChanged, changed, "TestCase[%d]: %s", i, test.desc)
			testSamePLSIpConfigs(t, pls.Properties.IPConfigurations, test.expectedIPConfigs)
		}
	}
}

func testSamePLSIpConfigs(t *testing.T, actual []*armnetwork.PrivateLinkServiceIPConfiguration, expected []*armnetwork.PrivateLinkServiceIPConfiguration) {
	t.Helper()
	actualIPConfigs := make(map[string]armnetwork.PrivateLinkServiceIPConfiguration)
	expectedIPConfigs := make(map[string]armnetwork.PrivateLinkServiceIPConfiguration)
	for _, ipConfig := range actual {
		actualIPConfigs[ptr.Deref(ipConfig.Name, "")] = *ipConfig
	}
	for _, ipConfig := range expected {
		expectedIPConfigs[ptr.Deref(ipConfig.Name, "")] = *ipConfig
	}

	for name, expectedIPConfig := range expectedIPConfigs {
		actualIPConfig, ok := actualIPConfigs[name]
		assert.True(t, ok, "Expected IPConfig %s not found", name)
		assert.Equal(t, expectedIPConfig, actualIPConfig, "IPConfig %s not equal", name)
		delete(actualIPConfigs, name)
	}

	assert.Empty(t, actualIPConfigs)
}

func TestServiceRequiresPLS(t *testing.T) {
	tests := []struct {
		desc        string
		annotations map[string]string
		expected    bool
	}{
		{
			desc: "Service with nil annotations should return false",
		},
		{
			desc:        "Service with empty annotations should return false",
			annotations: map[string]string{},
		},
		{
			desc: "Service with false pls creation annotation should return false",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation: "False",
			},
		},
		{
			desc: "Service with true pls creation annotation should return true",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSCreation: "True",
			},
			expected: true,
		},
	}
	for i, test := range tests {
		s := &v1.Service{}
		s.Annotations = test.annotations
		actual := serviceRequiresPLS(s)
		assert.Equal(t, test.expected, actual, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestReconcilePLSEnableProxyProtocol(t *testing.T) {
	tests := []struct {
		desc            string
		annotations     map[string]string
		pls             *armnetwork.PrivateLinkService
		expectedChanged bool
		expectedEnabled *bool
	}{
		{
			desc:        "empty service enableProxyProto and empty pls enableProxyProto should not trigger any change",
			annotations: map[string]string{},
			pls: &armnetwork.PrivateLinkService{
				Properties: &armnetwork.PrivateLinkServiceProperties{},
			},
		},
		{
			desc: "false service enableProxyProto and empty pls enableProxyProto should not trigger any change",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSProxyProtocol: "False",
			},
			pls: &armnetwork.PrivateLinkService{
				Properties: &armnetwork.PrivateLinkServiceProperties{},
			},
		},
		{
			desc: "false service enableProxyProto and false pls enableProxyProto should not trigger any change",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSProxyProtocol: "False",
			},
			pls: &armnetwork.PrivateLinkService{
				Properties: &armnetwork.PrivateLinkServiceProperties{
					EnableProxyProtocol: ptr.To(false),
				},
			},
			expectedEnabled: ptr.To(false),
		},
		{
			desc: "true service enableProxyProto and true pls enableProxyProto should not trigger any change",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSProxyProtocol: "True",
			},
			pls: &armnetwork.PrivateLinkService{
				Properties: &armnetwork.PrivateLinkServiceProperties{
					EnableProxyProtocol: ptr.To(true),
				},
			},
			expectedEnabled: ptr.To(true),
		},
		{
			desc: "true service enableProxyProto and empty pls enableProxyProto should trigger update",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSProxyProtocol: "True",
			},
			pls: &armnetwork.PrivateLinkService{
				Properties: &armnetwork.PrivateLinkServiceProperties{},
			},
			expectedEnabled: ptr.To(true),
			expectedChanged: true,
		},
		{
			desc: "false service enableProxyProto and true pls enableProxyProto should trigger update",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSProxyProtocol: "False",
			},
			pls: &armnetwork.PrivateLinkService{
				Properties: &armnetwork.PrivateLinkServiceProperties{
					EnableProxyProtocol: ptr.To(true),
				},
			},
			expectedEnabled: ptr.To(false),
			expectedChanged: true,
		},
	}
	for i, test := range tests {
		s := &v1.Service{}
		s.Annotations = test.annotations
		changed := reconcilePLSEnableProxyProtocol(test.pls, s)
		assert.Equal(t, test.expectedChanged, changed, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedEnabled, test.pls.Properties.EnableProxyProtocol, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestReconcilePLSFqdn(t *testing.T) {
	tests := []struct {
		desc            string
		annotations     map[string]string
		pls             *armnetwork.PrivateLinkService
		expectedChanged bool
		expectedFQDNs   []*string
	}{
		{
			desc:        "Empty service fqdns + empty pls fqdns should not trigger any change",
			annotations: map[string]string{},
			pls: &armnetwork.PrivateLinkService{
				Properties: &armnetwork.PrivateLinkServiceProperties{},
			},
		},
		{
			desc: "Same service fqdns and pls fqdns should not trigger any change",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSFqdns: "fqdns1 fqdns2 fqdns3",
			},
			pls: &armnetwork.PrivateLinkService{
				Properties: &armnetwork.PrivateLinkServiceProperties{
					Fqdns: to.SliceOfPtrs("fqdns2", "fqdns3", "fqdns1"),
				},
			},
			expectedFQDNs: to.SliceOfPtrs("fqdns2", "fqdns3", "fqdns1"),
		},
		{
			desc: "fqdns should be changed according to service - 0",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSFqdns: "",
			},
			pls: &armnetwork.PrivateLinkService{
				Properties: &armnetwork.PrivateLinkServiceProperties{
					Fqdns: to.SliceOfPtrs("fqdns2", "fqdns3", "fqdns1"),
				},
			},
			expectedChanged: true,
			expectedFQDNs:   []*string{},
		},
		{
			desc: "fqdns should be changed according to service - 1",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSFqdns: "fqdns1 fqdns2",
			},
			pls: &armnetwork.PrivateLinkService{
				Properties: &armnetwork.PrivateLinkServiceProperties{},
			},
			expectedChanged: true,
			expectedFQDNs:   to.SliceOfPtrs("fqdns1", "fqdns2"),
		},
		{
			desc: "fqdns should be changed according to service - 2",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSFqdns: "fqdns1 fqdns2",
			},
			pls: &armnetwork.PrivateLinkService{
				Properties: &armnetwork.PrivateLinkServiceProperties{
					Fqdns: to.SliceOfPtrs("fqdns2", "fqdns3", "fqdns1"),
				},
			},
			expectedChanged: true,
			expectedFQDNs:   to.SliceOfPtrs("fqdns1", "fqdns2"),
		},
	}
	for i, test := range tests {
		s := &v1.Service{}
		s.Annotations = test.annotations
		changed := reconcilePLSFqdn(test.pls, s)
		assert.Equal(t, test.expectedChanged, changed, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedFQDNs, test.pls.Properties.Fqdns, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestReconcilePLSVisibility(t *testing.T) {
	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{},
		},
	}
	pls := armnetwork.PrivateLinkService{
		Properties: &armnetwork.PrivateLinkServiceProperties{},
	}

	t.Run("reconcilePLSVisibility should change nothing when both visibility and auto-approval are nil", func(t *testing.T) {
		changed, err := reconcilePLSVisibility(&pls, &service)
		assert.False(t, changed)
		assert.NoError(t, err)
	})

	t.Run("reconcilePLSVisibility should return not changed if both Visibility and autoApproval are same", func(t *testing.T) {
		annotations := map[string]string{
			consts.ServiceAnnotationPLSVisibility: "sub1 sub2",
		}
		service.Annotations = annotations
		pls.Properties.Visibility = &armnetwork.PrivateLinkServicePropertiesVisibility{
			Subscriptions: to.SliceOfPtrs("sub2", "sub1"),
		}
		changed, err := reconcilePLSVisibility(&pls, &service)
		assert.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("reconcilePLSVisibility should return not changed if both Visibility and autoApproval are same with *", func(t *testing.T) {
		annotations := map[string]string{
			consts.ServiceAnnotationPLSVisibility:   "*",
			consts.ServiceAnnotationPLSAutoApproval: "sub1 sub2",
		}
		service.Annotations = annotations
		pls.Properties.Visibility = &armnetwork.PrivateLinkServicePropertiesVisibility{
			Subscriptions: to.SliceOfPtrs("*"),
		}
		pls.Properties.AutoApproval = &armnetwork.PrivateLinkServicePropertiesAutoApproval{
			Subscriptions: to.SliceOfPtrs("sub1", "sub2"),
		}
		changed, err := reconcilePLSVisibility(&pls, &service)
		assert.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("reconcilePLSVisibility should return change pls according to service - 0", func(t *testing.T) {
		annotations := map[string]string{
			consts.ServiceAnnotationPLSVisibility:   "*",
			consts.ServiceAnnotationPLSAutoApproval: "sub1 sub2",
		}
		pls.Properties.Visibility = nil
		pls.Properties.AutoApproval = nil
		expectedPLS := armnetwork.PrivateLinkService{
			Properties: &armnetwork.PrivateLinkServiceProperties{
				Visibility: &armnetwork.PrivateLinkServicePropertiesVisibility{
					Subscriptions: to.SliceOfPtrs("*"),
				},
				AutoApproval: &armnetwork.PrivateLinkServicePropertiesAutoApproval{
					Subscriptions: to.SliceOfPtrs("sub1", "sub2"),
				},
			},
		}
		service.Annotations = annotations
		changed, err := reconcilePLSVisibility(&pls, &service)
		assert.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, pls, expectedPLS)
	})

	t.Run("reconcilePLSVisibility should return change pls according to service - 1", func(t *testing.T) {
		annotations := map[string]string{
			consts.ServiceAnnotationPLSVisibility: "sub1 sub2",
		}
		pls.Properties.Visibility = nil
		pls.Properties.AutoApproval = nil
		expectedPLS := armnetwork.PrivateLinkService{
			Properties: &armnetwork.PrivateLinkServiceProperties{
				Visibility: &armnetwork.PrivateLinkServicePropertiesVisibility{
					Subscriptions: to.SliceOfPtrs("sub1", "sub2"),
				},
				AutoApproval: &armnetwork.PrivateLinkServicePropertiesAutoApproval{
					Subscriptions: []*string{},
				},
			},
		}
		service.Annotations = annotations
		changed, err := reconcilePLSVisibility(&pls, &service)
		assert.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, pls, expectedPLS)
	})

	t.Run("reconcilePLSVisibility should return change pls according to service - 2", func(t *testing.T) {
		annotations := map[string]string{
			consts.ServiceAnnotationPLSVisibility: "sub1 sub2",
		}
		pls = armnetwork.PrivateLinkService{
			Properties: &armnetwork.PrivateLinkServiceProperties{
				Visibility: &armnetwork.PrivateLinkServicePropertiesVisibility{
					Subscriptions: to.SliceOfPtrs("*"),
				},
				AutoApproval: &armnetwork.PrivateLinkServicePropertiesAutoApproval{
					Subscriptions: to.SliceOfPtrs("sub1"),
				},
			},
		}
		expectedPLS := armnetwork.PrivateLinkService{
			Properties: &armnetwork.PrivateLinkServiceProperties{
				Visibility: &armnetwork.PrivateLinkServicePropertiesVisibility{
					Subscriptions: to.SliceOfPtrs("sub1", "sub2"),
				},
				AutoApproval: &armnetwork.PrivateLinkServicePropertiesAutoApproval{
					Subscriptions: []*string{},
				},
			},
		}
		service.Annotations = annotations
		changed, err := reconcilePLSVisibility(&pls, &service)
		assert.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, pls, expectedPLS)
	})

	t.Run("reconcilePLSVisibility should return change pls according to service - 3", func(t *testing.T) {
		annotations := map[string]string{
			consts.ServiceAnnotationPLSVisibility:   "*",
			consts.ServiceAnnotationPLSAutoApproval: "sub1 sub2",
		}
		pls = armnetwork.PrivateLinkService{
			Properties: &armnetwork.PrivateLinkServiceProperties{
				Visibility: &armnetwork.PrivateLinkServicePropertiesVisibility{
					Subscriptions: to.SliceOfPtrs("*"),
				},
				AutoApproval: &armnetwork.PrivateLinkServicePropertiesAutoApproval{
					Subscriptions: to.SliceOfPtrs("sub3"),
				},
			},
		}
		expectedPLS := armnetwork.PrivateLinkService{
			Properties: &armnetwork.PrivateLinkServiceProperties{
				Visibility: &armnetwork.PrivateLinkServicePropertiesVisibility{
					Subscriptions: to.SliceOfPtrs("*"),
				},
				AutoApproval: &armnetwork.PrivateLinkServicePropertiesAutoApproval{
					Subscriptions: to.SliceOfPtrs("sub1", "sub2"),
				},
			},
		}
		service.Annotations = annotations
		changed, err := reconcilePLSVisibility(&pls, &service)
		assert.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, pls, expectedPLS)
	})
}

func TestReconcilePLSTags(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.Tags = "a=x,y=z"

	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "test-svc",
		},
	}
	pls := armnetwork.PrivateLinkService{
		Tags: map[string]*string{
			"foo": ptr.To("bar"),
			"a":   ptr.To("j"),
			"m":   ptr.To("n"),
		},
	}
	clusterName := testClusterName

	t.Run("reconcilePLSTags should ensure the pls is tagged as configured", func(t *testing.T) {
		expectedPLS := armnetwork.PrivateLinkService{
			Tags: map[string]*string{
				consts.ClusterNameTagKey:  ptr.To(testClusterName),
				consts.OwnerServiceTagKey: ptr.To("test-ns/test-svc"),
				"foo":                     ptr.To("bar"),
				"a":                       ptr.To("x"),
				"y":                       ptr.To("z"),
				"m":                       ptr.To("n"),
			},
		}
		changed := cloud.reconcilePLSTags(&pls, &clusterName, &service)
		assert.True(t, changed)
		assert.Equal(t, expectedPLS, pls)
	})

	t.Run("reconcilePLSTags should delete the old tags if the SystemTags is set", func(t *testing.T) {
		cloud.SystemTags = "a,foo,b"
		expectedPLS := armnetwork.PrivateLinkService{
			Tags: map[string]*string{
				consts.ClusterNameTagKey:  ptr.To(testClusterName),
				consts.OwnerServiceTagKey: ptr.To("test-ns/test-svc"),
				"foo":                     ptr.To("bar"),
				"a":                       ptr.To("x"),
				"y":                       ptr.To("z"),
			},
		}
		changed := cloud.reconcilePLSTags(&pls, &clusterName, &service)
		assert.True(t, changed)
		assert.Equal(t, expectedPLS, pls)
	})

	t.Run("reconcilePLSTags should support TagsMap", func(t *testing.T) {
		cloud.SystemTags = "a,foo,b"
		cloud.TagsMap = map[string]string{"a": "c", "a=b": "c=d", "Y": "zz"}
		expectedPLS := armnetwork.PrivateLinkService{
			Tags: map[string]*string{
				consts.ClusterNameTagKey:  ptr.To(testClusterName),
				consts.OwnerServiceTagKey: ptr.To("test-ns/test-svc"),
				"foo":                     ptr.To("bar"),
				"a":                       ptr.To("c"),
				"a=b":                     ptr.To("c=d"),
			},
		}
		changed := cloud.reconcilePLSTags(&pls, &clusterName, &service)
		assert.True(t, changed)
		assert.Equal(t, expectedPLS, pls)
	})

	pls.Tags[consts.ClusterNameTagKey] = ptr.To("testCluster1")
	pls.Tags[consts.OwnerServiceTagKey] = ptr.To("default/svc")

	t.Run("reconcilePLSTags should respect cluster and owner service tag keys", func(t *testing.T) {
		expectedPLS := armnetwork.PrivateLinkService{
			Tags: map[string]*string{
				consts.ClusterNameTagKey:  ptr.To("testCluster1"),
				consts.OwnerServiceTagKey: ptr.To("default/svc"),
				"foo":                     ptr.To("bar"),
				"a":                       ptr.To("c"),
				"a=b":                     ptr.To("c=d"),
				"Y":                       ptr.To("zz"),
			},
		}
		changed := cloud.reconcilePLSTags(&pls, &clusterName, &service)
		assert.True(t, changed)
		assert.Equal(t, expectedPLS, pls)
	})
}

func TestGetPLSSubnetName(t *testing.T) {
	tests := []struct {
		desc           string
		annotations    map[string]string
		expectedSubnet *string
	}{
		{
			desc: "Service with nil annotations should return nil",
		},
		{
			desc: "Service with empty annotations should return nil",
		},
		{
			desc: "Service with private link subnet specified should return it",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationSubnet: "pls-subnet",
			},
			expectedSubnet: ptr.To("pls-subnet"),
		},
		{
			desc: "Service with empty private link subnet specified but LB subnet specified should return LB subnet",
			annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerInternal:       "true",
				consts.ServiceAnnotationPLSIpConfigurationSubnet:   "",
				consts.ServiceAnnotationLoadBalancerInternalSubnet: "lb-subnet",
			},
			expectedSubnet: ptr.To("lb-subnet"),
		},
		{
			desc: "Service with LB subnet specified should return it",
			annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerInternal:       "true",
				consts.ServiceAnnotationLoadBalancerInternalSubnet: "lb-subnet",
			},
			expectedSubnet: ptr.To("lb-subnet"),
		},
		{
			desc: "Service with both empty subnets specified should return nil",
			annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerInternal:       "true",
				consts.ServiceAnnotationPLSIpConfigurationSubnet:   "",
				consts.ServiceAnnotationLoadBalancerInternalSubnet: "",
			},
		},
	}
	for i, test := range tests {
		s := &v1.Service{}
		s.Annotations = test.annotations
		actualSubnet := getPLSSubnetName(s)
		assert.Equal(t, test.expectedSubnet, actualSubnet, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestGetPLSIPConfigCount(t *testing.T) {
	tests := []struct {
		desc            string
		annotations     map[string]string
		expectedIPCount int32
		expectedErr     bool
	}{
		{
			desc:            "Service with nil annotations should return default(1) without any error",
			expectedIPCount: 1,
		},
		{
			desc:            "Service with empty annotations should return default(1) without any error",
			annotations:     map[string]string{},
			expectedIPCount: 1,
		},
		{
			desc: "Service with valid ip count specified should return it",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "6",
			},
			expectedIPCount: 6,
		},
		{
			desc: "Service with < 1 ip count specified should return error",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "0",
			},
			expectedErr: true,
		},
		{
			desc: "Service with > 8 ip count specified should return error",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "9",
			},
			expectedErr: true,
		},
		{
			desc: "Redundant spaces should be removed",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "   4   ",
			},
			expectedIPCount: 4,
		},
		{
			desc: "Service with not valid digit string specified should return error",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "test2",
			},
			expectedErr: true,
		},
	}
	for i, test := range tests {
		s := &v1.Service{}
		s.Annotations = test.annotations
		actualCount, err := getPLSIPConfigCount(s)
		if test.expectedErr {
			assert.Error(t, err, "TestCase[%d]: %s", i, test.desc)
		} else {
			assert.Equal(t, test.expectedIPCount, actualCount, "TestCase[%d]: %s", i, test.desc)
			assert.NoError(t, err)
		}
	}
}

func TestGetPLSFqdns(t *testing.T) {
	tests := []struct {
		desc          string
		annotations   map[string]string
		expectedFqdns []string
	}{
		{
			desc:          "Service with nil annotations should return all empty result without any error",
			expectedFqdns: []string{},
		},
		{
			desc:          "Service with empty annotations should return all empty result without any error",
			annotations:   map[string]string{},
			expectedFqdns: []string{},
		},
		{
			desc: "Service with just 1 fqdns should include it in result",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSFqdns: "test-fqdns",
			},
			expectedFqdns: []string{"test-fqdns"},
		},
		{
			desc: "Service with multiple fqdns should include them in result",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSFqdns: "test-fqdns1 test-fqdns2 test-fqdns3",
			},
			expectedFqdns: []string{"test-fqdns1", "test-fqdns2", "test-fqdns3"},
		},
		{
			desc: "Redundant spaces should be removed",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSFqdns: "    test-fqdns1     test-fqdns2    test-fqdns3   ",
			},
			expectedFqdns: []string{"test-fqdns1", "test-fqdns2", "test-fqdns3"},
		},
	}
	for i, test := range tests {
		s := &v1.Service{}
		s.Annotations = test.annotations
		actual := getPLSFqdns(s)
		assert.Equal(t, test.expectedFqdns, actual, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestGetPLSVisibility(t *testing.T) {
	tests := []struct {
		desc             string
		annotations      map[string]string
		expectedVis      []string
		expectedAllowAll bool
	}{
		{
			desc:        "Service with nil annotations should return all empty result without any error",
			expectedVis: []string{},
		},
		{
			desc:        "Service with empty annotations should return all empty result without any error",
			annotations: map[string]string{},
			expectedVis: []string{},
		},
		{
			desc: "Service with just 1 visibility should include it in result",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSVisibility: "test-sub-id",
			},
			expectedVis: []string{"test-sub-id"},
		},
		{
			desc: "Service with multiple visibilities should include them in result",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSVisibility: "test-sub-id1 test-sub-id2 test-sub-id3",
			},
			expectedVis: []string{"test-sub-id1", "test-sub-id2", "test-sub-id3"},
		},
		{
			desc: "All redundant spaces should be removed",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSVisibility: "   test-sub-id1    test-sub-id2   test-sub-id3   ",
			},
			expectedVis: []string{"test-sub-id1", "test-sub-id2", "test-sub-id3"},
		},
		{
			desc: "Visibility with * should include it in result, and set allow-all as true",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSVisibility: "*",
			},
			expectedVis:      []string{"*"},
			expectedAllowAll: true,
		},
		{
			desc: "Visibility with * and other sub ids should only include * in result, and set allow-all as true",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSVisibility: "   test-sub-id1     *  test-sub-id2",
			},
			expectedVis:      []string{"*"},
			expectedAllowAll: true,
		},
	}
	for i, test := range tests {
		s := &v1.Service{}
		s.Annotations = test.annotations
		actualVis, actualAllowAll := getPLSVisibility(s)
		assert.Equal(t, test.expectedVis, actualVis, "TestCase[%d]: %s", i, test.desc)
		assert.Equal(t, test.expectedAllowAll, actualAllowAll, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestGetPLSAutoApproval(t *testing.T) {
	tests := []struct {
		desc              string
		annotations       map[string]string
		expectedApprovals []string
	}{
		{
			desc:              "Service with nil annotations should return all empty result without any error",
			expectedApprovals: []string{},
		},
		{
			desc:              "Service with empty annotations should return all empty result without any error",
			annotations:       map[string]string{},
			expectedApprovals: []string{},
		},
		{
			desc: "Service with just 1 auto approval should include it in result",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSAutoApproval: "test-sub-id",
			},
			expectedApprovals: []string{"test-sub-id"},
		},
		{
			desc: "Service with multiple auto approvals should include them in result",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSAutoApproval: "test-sub-id1 test-sub-id2 test-sub-id3",
			},
			expectedApprovals: []string{"test-sub-id1", "test-sub-id2", "test-sub-id3"},
		},
		{
			desc: "All redundant spaces should be removed",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSAutoApproval: "   test-sub-id1    test-sub-id2   test-sub-id3   ",
			},
			expectedApprovals: []string{"test-sub-id1", "test-sub-id2", "test-sub-id3"},
		},
	}
	for i, test := range tests {
		s := &v1.Service{}
		s.Annotations = test.annotations
		actual := getPLSAutoApproval(s)
		assert.Equal(t, test.expectedApprovals, actual, "TestCase[%d]: %s", i, test.desc)
	}
}

func TestGetPLSStaticIPs(t *testing.T) {
	tests := []struct {
		desc              string
		annotations       map[string]string
		expectedIPs       map[string]bool
		expectedPrimaryIP string
		expectedErr       bool
	}{
		{
			desc:        "Service with nil annotations should return all empty result without any error",
			expectedIPs: map[string]bool{},
		},
		{
			desc:        "Service with empty annotations should return all empty result without any error",
			annotations: map[string]string{},
			expectedIPs: map[string]bool{},
		},
		{
			desc: "Service with just 1 static ip should include it in map and set as primary ip",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddress: "10.2.0.4",
			},
			expectedIPs: map[string]bool{
				"10.2.0.4": true,
			},
			expectedPrimaryIP: "10.2.0.4",
		},
		{
			desc: "Service with multiple static ips should include them in map and set the first one as primary ip",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddress: "10.2.0.4 10.2.0.5 10.2.0.6",
			},
			expectedIPs: map[string]bool{
				"10.2.0.4": true,
				"10.2.0.5": true,
				"10.2.0.6": true,
			},
			expectedPrimaryIP: "10.2.0.4",
		},
		{
			desc: "Service with invalid ip should return error",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddress: "10.2.0.4 300.1.2.9 10.2.0.6",
			},
			expectedErr: true,
		},
		{
			desc: "Service with ipv6 address should return error",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddress: "fc00:f853:ccd:e793::1",
			},
			expectedErr: true,
		},
		{
			desc: "All redundant spaces should be removed",
			annotations: map[string]string{
				consts.ServiceAnnotationPLSIpConfigurationIPAddress: "   10.2.0.4    10.2.0.5     10.2.0.6 ",
			},
			expectedIPs: map[string]bool{
				"10.2.0.4": true,
				"10.2.0.5": true,
				"10.2.0.6": true,
			},
			expectedPrimaryIP: "10.2.0.4",
		},
	}
	for i, test := range tests {
		s := &v1.Service{}
		s.Annotations = test.annotations
		actualIPs, actualPrimaryIP, err := getPLSStaticIPs(s)
		if test.expectedErr {
			assert.Error(t, err, "TestCase[%d]: %s", i, test.desc)
		} else {
			assert.Equal(t, test.expectedIPs, actualIPs, "TestCase[%d]: %s", i, test.desc)
			assert.Equal(t, test.expectedPrimaryIP, actualPrimaryIP, "TestCase[%d]: %s", i, test.desc)
			assert.NoError(t, err)
		}
	}
}

func TestIsManagedPrivateLinkSerivce(t *testing.T) {
	tests := []struct {
		desc        string
		pls         *armnetwork.PrivateLinkService
		clusterName string
		expected    bool
	}{
		{
			desc: "Private link service with nil tag should return false",
			pls:  &armnetwork.PrivateLinkService{},
		},
		{
			desc: "Private link service with empty tag should return false",
			pls: &armnetwork.PrivateLinkService{
				Tags: map[string]*string{},
			},
		},
		{
			desc: "Private link service with different cluster name should return false",
			pls: &armnetwork.PrivateLinkService{
				Tags: map[string]*string{
					"k8s-azure-cluster-name": ptr.To("test-cluster1"),
				},
			},
			clusterName: "test-cluster",
		},
		{
			desc: "Private link service with same cluster name should return true",
			pls: &armnetwork.PrivateLinkService{
				Tags: map[string]*string{
					"k8s-azure-cluster-name": ptr.To("test-cluster"),
				},
			},
			clusterName: "test-cluster",
			expected:    true,
		},
	}
	for i, c := range tests {
		actual := isManagedPrivateLinkSerivce(c.pls, c.clusterName)
		assert.Equal(t, actual, c.expected, "TestCase[%d]: %s", i, c.desc)
	}
}

func TestGetPrivateLinkServiceOwner(t *testing.T) {
	tests := []struct {
		desc     string
		pls      *armnetwork.PrivateLinkService
		expected string
	}{
		{
			desc: "Private link service with nil tag should return empty string",
			pls:  &armnetwork.PrivateLinkService{},
		},
		{
			desc: "Private link service with empty tag should return empty string",
			pls: &armnetwork.PrivateLinkService{
				Tags: map[string]*string{},
			},
		},
		{
			desc: "Private link service with service owner tag should return service owner",
			pls: &armnetwork.PrivateLinkService{
				Tags: map[string]*string{"k8s-azure-owner-service": ptr.To("test-service")},
			},
			expected: "test-service",
		},
	}
	for i, c := range tests {
		actual := getPrivateLinkServiceOwner(c.pls)
		assert.Equal(t, actual, c.expected, "TestCase[%d]: %s", i, c.desc)
	}
}

func TestServiceHasAdditionalConfigs(t *testing.T) {
	tests := []struct {
		desc        string
		annotations map[string]string
		expected    bool
	}{
		{
			desc:     "Service without any annotations should return false",
			expected: false,
		},
		{
			desc:        "Service with only pls-create annotation should return false",
			annotations: map[string]string{consts.ServiceAnnotationPLSCreation: "true"},
			expected:    false,
		},
		{
			desc:        "Service with pls-name annotation should return true",
			annotations: map[string]string{consts.ServiceAnnotationPLSName: "test"},
			expected:    true,
		},
		{
			desc:        "Service with pls-subnet annotation should return true",
			annotations: map[string]string{consts.ServiceAnnotationPLSIpConfigurationSubnet: "test"},
			expected:    true,
		},
		{
			desc:        "Service with pls-ip-address-count annotation should return true",
			annotations: map[string]string{consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: "test"},
			expected:    true,
		},
		{
			desc:        "Service with pls-ip-address annotation should return true",
			annotations: map[string]string{consts.ServiceAnnotationPLSIpConfigurationIPAddress: "test"},
			expected:    true,
		},
		{
			desc:        "Service with pls-fqdns annotation should return true",
			annotations: map[string]string{consts.ServiceAnnotationPLSFqdns: "test"},
			expected:    true,
		},
		{
			desc:        "Service with pls-proxy-protocol annotation should return true",
			annotations: map[string]string{consts.ServiceAnnotationPLSProxyProtocol: "test"},
			expected:    true,
		},
		{
			desc:        "Service with pls-visibility annotation should return true",
			annotations: map[string]string{consts.ServiceAnnotationPLSVisibility: "test"},
			expected:    true,
		},
		{
			desc:        "Service with pls-auto-approval annotation should return true",
			annotations: map[string]string{consts.ServiceAnnotationPLSAutoApproval: "test"},
			expected:    true,
		},
	}
	for i, c := range tests {
		s := &v1.Service{}
		s.Annotations = c.annotations
		actual := serviceHasAdditionalConfigs(s)
		assert.Equal(t, actual, c.expected, "TestCase[%d]: %s", i, c.desc)
	}
}
