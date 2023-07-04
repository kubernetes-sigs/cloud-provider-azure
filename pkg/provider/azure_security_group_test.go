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
	"net/http"
	"strconv"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/loadbalancerclient/mockloadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/securitygroupclient/mocksecuritygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestReconcileSecurityGroupCommon(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc          string
		lbIPs         *[]string
		lbName        *string
		service       v1.Service
		existingSgs   map[string]network.SecurityGroup
		expectedSg    *network.SecurityGroup
		wantLb        bool
		expectedError bool
	}{
		{
			desc: "reconcileSecurityGroup shall report error if the sg is shared and no ports in service",
			service: v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationSharedSecurityRule: "true",
					},
				},
			},
			expectedError: true,
		},
		{
			desc:          "reconcileSecurityGroup shall report error if no such sg can be found",
			service:       getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			expectedError: true,
		},
		{
			desc:          "reconcileSecurityGroup shall report error if wantLb is true and lbIPs is nil",
			service:       getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			wantLb:        true,
			existingSgs:   map[string]network.SecurityGroup{"nsg": {}},
			expectedError: true,
		},
		{
			desc:        "reconcileSecurityGroup shall remain the existingSgs intact if nothing needs to be modified",
			service:     getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {}},
			expectedSg:  &network.SecurityGroup{},
		},
		{
			desc:    "reconcileSecurityGroup shall delete unwanted sg if wantLb is false and lbIPs is nil",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("atest1-toBeDeleted"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								SourceAddressPrefix:      pointer.String("prefix"),
								SourcePortRange:          pointer.String("range"),
								DestinationAddressPrefix: pointer.String("desPrefix"),
								DestinationPortRange:     pointer.String("desRange"),
							},
						},
					},
				},
			}},
			wantLb: false,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{},
				},
			},
		},
		{
			desc:    "reconcileSecurityGroup shall delete unwanted sgs and create needed ones",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("atest1-toBeDeleted"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								SourceAddressPrefix:      pointer.String("prefix"),
								SourcePortRange:          pointer.String("range"),
								DestinationAddressPrefix: pointer.String("desPrefix"),
								DestinationPortRange:     pointer.String("desRange"),
							},
						},
					},
				},
			}},
			lbIPs:  &[]string{"1.1.1.1", "fd00::eef0"},
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("atest1-TCP-80-Internet"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                 network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:          pointer.String("*"),
								DestinationPortRange:     pointer.String("80"),
								SourceAddressPrefix:      pointer.String("Internet"),
								DestinationAddressPrefix: pointer.String("1.1.1.1"),
								Access:                   network.SecurityRuleAccess("Allow"),
								Priority:                 pointer.Int32(500),
								Direction:                network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("atest1-TCP-80-Internet-IPv6"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                 network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:          pointer.String("*"),
								DestinationPortRange:     pointer.String("80"),
								SourceAddressPrefix:      pointer.String("Internet"),
								DestinationAddressPrefix: pointer.String("fd00::eef0"),
								Access:                   network.SecurityRuleAccess("Allow"),
								Priority:                 pointer.Int32(501),
								Direction:                network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc:    "reconcileSecurityGroup shall create sgs with correct destinationPrefix for IPv6 only",
			service: getTestService("test1", v1.ProtocolTCP, nil, true, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name:                          pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
			}},
			lbIPs:  &[]string{"fd00::eef0"},
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("atest1-TCP-80-Internet"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                 network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:          pointer.String("*"),
								DestinationPortRange:     pointer.String("80"),
								SourceAddressPrefix:      pointer.String("Internet"),
								DestinationAddressPrefix: pointer.String("fd00::eef0"),
								Access:                   network.SecurityRuleAccess("Allow"),
								Priority:                 pointer.Int32(500),
								Direction:                network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc:    "reconcileSecurityGroup shall create sgs with correct destinationPrefix for Dual-stack",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name:                          pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
			}},
			lbIPs:  &[]string{"1.1.1.1", "fd00::eef0"},
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("atest1-TCP-80-Internet"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                 network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:          pointer.String("*"),
								DestinationPortRange:     pointer.String("80"),
								SourceAddressPrefix:      pointer.String("Internet"),
								DestinationAddressPrefix: pointer.String("1.1.1.1"),
								Access:                   network.SecurityRuleAccess("Allow"),
								Priority:                 pointer.Int32(500),
								Direction:                network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("atest1-TCP-80-Internet-IPv6"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                 network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:          pointer.String("*"),
								DestinationPortRange:     pointer.String("80"),
								SourceAddressPrefix:      pointer.String("Internet"),
								DestinationAddressPrefix: pointer.String("fd00::eef0"),
								Access:                   network.SecurityRuleAccess("Allow"),
								Priority:                 pointer.Int32(501),
								Direction:                network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc:    "reconcileSecurityGroup shall create sgs with correct destinationPrefix with additional public IPs",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationAdditionalPublicIPs: "2.3.4.5,fd00::eef1"}, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name:                          pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
			}},
			lbIPs:  &[]string{"1.2.3.4", "fd00::eef0"},
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("atest1-TCP-80-Internet"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String("80"),
								SourceAddressPrefix:        pointer.String("Internet"),
								DestinationAddressPrefixes: &([]string{"1.2.3.4", "2.3.4.5"}),
								Access:                     network.SecurityRuleAccess("Allow"),
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("atest1-TCP-80-Internet-IPv6"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String("80"),
								SourceAddressPrefix:        pointer.String("Internet"),
								DestinationAddressPrefixes: &([]string{"fd00::eef0", "fd00::eef1"}),
								Access:                     network.SecurityRuleAccess("Allow"),
								Priority:                   pointer.Int32(501),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc:    "reconcileSecurityGroup shall not create unwanted security rules if there is service tags",
			service: getTestService("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationAllowedServiceTag: "tag"}, false, 80),
			wantLb:  true,
			lbIPs:   &[]string{"1.1.1.1"},
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("atest1-toBeDeleted"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								SourceAddressPrefix:      pointer.String("prefix"),
								SourcePortRange:          pointer.String("range"),
								DestinationAddressPrefix: pointer.String("destPrefix"),
								DestinationPortRange:     pointer.String("desRange"),
							},
						},
					},
				},
			}},
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("atest1-TCP-80-tag"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                 network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:          pointer.String("*"),
								DestinationPortRange:     pointer.String("80"),
								SourceAddressPrefix:      pointer.String("tag"),
								DestinationAddressPrefix: pointer.String("1.1.1.1"),
								Access:                   network.SecurityRuleAccess("Allow"),
								Priority:                 pointer.Int32(500),
								Direction:                network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc:    "reconcileSecurityGroup shall create shared sgs for service with azure-shared-securityrule annotations",
			service: getTestService("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationSharedSecurityRule: "true"}, false, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name:                          pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
			}},
			lbIPs:  &[]string{"1.2.3.4"},
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("shared-TCP-80-Internet"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String("80"),
								SourceAddressPrefix:        pointer.String("Internet"),
								DestinationAddressPrefixes: &([]string{"1.2.3.4"}),
								Access:                     network.SecurityRuleAccess("Allow"),
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc:    "reconcileSecurityGroup shall delete shared sgs for service with azure-shared-securityrule annotations",
			service: getTestService("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationSharedSecurityRule: "true"}, false, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("shared-TCP-80-Internet"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String("80"),
								SourceAddressPrefix:        pointer.String("Internet"),
								DestinationAddressPrefixes: &([]string{"1.2.3.4"}),
								Access:                     network.SecurityRuleAccess("Allow"),
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			}},
			lbIPs:  &[]string{"1.2.3.4"},
			wantLb: false,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{},
				},
			},
		},
		{
			desc:    "reconcileSecurityGroup shall delete shared sgs destination for service with azure-shared-securityrule annotations",
			service: getTestService("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationSharedSecurityRule: "true"}, false, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("shared-TCP-80-Internet"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String("80"),
								SourceAddressPrefix:        pointer.String("Internet"),
								DestinationAddressPrefixes: &([]string{"1.2.3.4", "5.6.7.8"}),
								Access:                     network.SecurityRuleAccess("Allow"),
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			}},
			lbIPs:  &[]string{"1.2.3.4"},
			wantLb: false,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("shared-TCP-80-Internet"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String("80"),
								SourceAddressPrefix:        pointer.String("Internet"),
								DestinationAddressPrefixes: &([]string{"5.6.7.8"}),
								Access:                     network.SecurityRuleAccess("Allow"),
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc:    "reconcileSecurityGroup shall create sgs with floating IP disabled",
			service: getTestService("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true"}, false, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name:                          pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
			}},
			lbIPs:  &[]string{"1.2.3.4"},
			lbName: pointer.String("lb"),
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("atest1-TCP-80-Internet"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(int(getBackendPort(80)))),
								SourceAddressPrefix:        pointer.String("Internet"),
								DestinationAddressPrefixes: &([]string{"1.2.3.4", "5.6.7.8"}),
								Access:                     network.SecurityRuleAccess("Allow"),
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc:    "reconcileSecurityGroup shall create sgs with only IPv6 destination addresses for IPv6 services with floating IP disabled",
			service: getTestService("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true"}, true, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name:                          pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
			}},
			lbIPs:  &[]string{"1234::5"},
			lbName: pointer.String("lb"),
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("atest1-TCP-80-Internet"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(int(getBackendPort(80)))),
								SourceAddressPrefix:        pointer.String("Internet"),
								DestinationAddressPrefixes: &([]string{"fc00::1", "fc00::2"}),
								Access:                     network.SecurityRuleAccess("Allow"),
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			mockSGsClient := az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
			mockSGsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			if len(test.existingSgs) == 0 {
				mockSGsClient.EXPECT().Get(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(network.SecurityGroup{}, &retry.Error{HTTPStatusCode: http.StatusNotFound}).AnyTimes()
			}
			for name, sg := range test.existingSgs {
				mockSGsClient.EXPECT().Get(gomock.Any(), "rg", name, gomock.Any()).Return(sg, nil).AnyTimes()
				err := az.SecurityGroupsClient.CreateOrUpdate(context.TODO(), "rg", name, sg, "")
				assert.NoError(t, err.Error())
			}
			mockLBClient := az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
			mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
			if test.lbName != nil {
				mockLBBackendPool.EXPECT().GetBackendPrivateIPs(gomock.Any(), gomock.Any(), gomock.Any()).Return([]string{"1.2.3.4", "5.6.7.8"}, []string{"fc00::1", "fc00::2"}).AnyTimes()
				mockLBClient.EXPECT().Get(gomock.Any(), "rg", *test.lbName, gomock.Any()).Return(network.LoadBalancer{}, nil)
			}
			service := test.service
			sg, err := az.reconcileSecurityGroup("testCluster", &service, test.lbIPs, test.lbName, test.wantLb)
			assert.Equal(t, test.expectedSg, sg)
			assert.Equal(t, test.expectedError, err != nil)
		})
	}
}

func TestReconcileSecurityGroupLoadBalancerSourceRanges(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	service := getTestService("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges: "true"}, false, 80)
	service.Spec.LoadBalancerSourceRanges = []string{"1.2.3.4/32"}
	existingSg := network.SecurityGroup{
		Name: pointer.String("nsg"),
		SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
			SecurityRules: &[]network.SecurityRule{},
		},
	}
	lbIPs := &[]string{"1.1.1.1"}
	expectedSg := network.SecurityGroup{
		Name: pointer.String("nsg"),
		SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
			SecurityRules: &[]network.SecurityRule{
				{
					Name: pointer.String("atest1-TCP-80-1.2.3.4_32"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocol("Tcp"),
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccess("Allow"),
						Priority:                 pointer.Int32(500),
						Direction:                network.SecurityRuleDirection("Inbound"),
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-deny_all"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocol("Tcp"),
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("*"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccess("Deny"),
						Priority:                 pointer.Int32(501),
						Direction:                network.SecurityRuleDirection("Inbound"),
					},
				},
			},
		},
	}
	mockSGClient := az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
	mockSGClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any()).Return(existingSg, nil)
	mockSGClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	sg, err := az.reconcileSecurityGroup("testCluster", &service, lbIPs, nil, true)
	assert.NoError(t, err)
	assert.Equal(t, expectedSg, *sg)
}
