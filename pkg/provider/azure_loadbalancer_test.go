/*
Copyright 2020 The Kubernetes Authors.

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
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-03-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/loadbalancerclient/mockloadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privatelinkserviceclient/mockprivatelinkserviceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/publicipclient/mockpublicipclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/securitygroupclient/mocksecuritygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/subnetclient/mocksubnetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/zoneclient/mockzoneclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

// LBInUseRawError is the LoadBalancerInUseByVirtualMachineScaleSet raw error
const LBInUseRawError = `{
	"error": {
    	"code": "LoadBalancerInUseByVirtualMachineScaleSet",
    	"message": "Cannot delete load balancer /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb since its child resources lb are in use by virtual machine scale set /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss.",
    	"details": []
  	}
}`

func TestExistsPip(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testcases := []struct {
		desc               string
		service            v1.Service
		expectedClientList func(client *mockpublicipclient.MockInterface)
		expectedExist      bool
	}{
		{
			"IPv4 exists",
			getTestService("service", v1.ProtocolTCP, nil, false, 80),
			func(client *mockpublicipclient.MockInterface) {
				pips := []network.PublicIPAddress{
					{
						Name: pointer.String("testCluster-aservice"),
						PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
							IPAddress: pointer.String("1.2.3.4"),
						},
					},
				}
				client.EXPECT().List(gomock.Any(), "rg").Return(pips, nil)
			},
			true,
		},
		{
			"IPv4 not exists",
			getTestService("service", v1.ProtocolTCP, nil, false, 80),
			func(client *mockpublicipclient.MockInterface) {
				client.EXPECT().List(gomock.Any(), "rg").Return([]network.PublicIPAddress{}, nil).MaxTimes(2)
			},
			false,
		},
		{
			"IPv6 exists",
			getTestService("service", v1.ProtocolTCP, nil, true, 80),
			func(client *mockpublicipclient.MockInterface) {
				pips := []network.PublicIPAddress{
					{
						Name: pointer.String("testCluster-aservice"),
						PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
							IPAddress: pointer.String("fe::1"),
						},
					},
				}
				client.EXPECT().List(gomock.Any(), "rg").Return(pips, nil)
			},
			true,
		},
		{
			"IPv6 not exists - should not have suffix",
			getTestService("service", v1.ProtocolTCP, nil, true, 80),
			func(client *mockpublicipclient.MockInterface) {
				pips := []network.PublicIPAddress{
					{
						Name: pointer.String("testCluster-aservice-IPv6"),
						PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
							IPAddress: pointer.String("fe::1"),
						},
					},
				}
				client.EXPECT().List(gomock.Any(), "rg").Return(pips, nil).MaxTimes(2)
			},
			false,
		},
		{
			"DualStack exists",
			getTestServiceDualStack("service", v1.ProtocolTCP, nil, 80),
			func(client *mockpublicipclient.MockInterface) {
				pips := []network.PublicIPAddress{
					{
						Name: pointer.String("testCluster-aservice"),
						PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
							IPAddress: pointer.String("1.2.3.4"),
						},
					},
					{
						Name: pointer.String("testCluster-aservice-IPv6"),
						PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
							IPAddress: pointer.String("fe::1"),
						},
					},
				}
				client.EXPECT().List(gomock.Any(), "rg").Return(pips, nil)
			},
			true,
		},
		{
			"DualStack, IPv4 not exists",
			getTestServiceDualStack("service", v1.ProtocolTCP, nil, 80),
			func(client *mockpublicipclient.MockInterface) {
				pips := []network.PublicIPAddress{
					{
						Name: pointer.String("testCluster-aservice-IPv6"),
						PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
							IPAddress: pointer.String("fe::1"),
						},
					},
				}
				client.EXPECT().List(gomock.Any(), "rg").Return(pips, nil).MaxTimes(2)
			},
			false,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			service := tc.service
			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			tc.expectedClientList(mockPIPsClient)
			exist := az.existsPip("testCluster", &service)
			assert.Equal(t, tc.expectedExist, exist)
		})
	}
}

// TODO: Dualstack
func TestGetLoadBalancer(t *testing.T) {
	lb1 := network.LoadBalancer{
		Name:                         pointer.String("testCluster"),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{},
	}
	lb2 := network.LoadBalancer{
		Name: pointer.String("testCluster"),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: &[]network.FrontendIPConfiguration{
				{
					Name: pointer.String("aservice"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice")},
					},
				},
			},
		},
	}
	lb3 := network.LoadBalancer{
		Name: pointer.String("testCluster-internal"),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: &[]network.FrontendIPConfiguration{
				{
					Name: pointer.String("aservice"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PrivateIPAddress: pointer.String("10.0.0.6"),
					},
				},
			},
		},
	}
	tests := []struct {
		desc           string
		service        v1.Service
		existingLBs    []network.LoadBalancer
		pipExists      bool
		expectedGotLB  bool
		expectedStatus *v1.LoadBalancerStatus
	}{
		{
			desc:           "GetLoadBalancer should return true when only public IP exists",
			service:        getTestService("service", v1.ProtocolTCP, nil, false, 80),
			existingLBs:    []network.LoadBalancer{lb1},
			pipExists:      true,
			expectedGotLB:  true,
			expectedStatus: nil,
		},
		{
			desc:           "GetLoadBalancer should return false when neither public IP nor LB exists",
			service:        getTestService("service", v1.ProtocolTCP, nil, false, 80),
			existingLBs:    []network.LoadBalancer{lb1},
			pipExists:      false,
			expectedGotLB:  false,
			expectedStatus: nil,
		},
		{
			desc:          "GetLoadBalancer should return true when external service finds external LB",
			service:       getTestService("service", v1.ProtocolTCP, nil, false, 80),
			existingLBs:   []network.LoadBalancer{lb2},
			pipExists:     true,
			expectedGotLB: true,
			expectedStatus: &v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "1.2.3.4"},
				},
			},
		},
		{
			desc:          "GetLoadBalancer should return true when internal service finds internal LB",
			service:       getInternalTestService("service", 80),
			existingLBs:   []network.LoadBalancer{lb3},
			expectedGotLB: true,
			expectedStatus: &v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "10.0.0.6"},
				},
			},
		},
		{
			desc:          "GetLoadBalancer should return true when external service finds previous internal LB",
			service:       getTestService("service", v1.ProtocolTCP, nil, false, 80),
			existingLBs:   []network.LoadBalancer{lb3},
			expectedGotLB: true,
			expectedStatus: &v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "10.0.0.6"},
				},
			},
		},
		{
			desc:          "GetLoadBalancer should return true when external service finds external LB",
			service:       getInternalTestService("service", 80),
			existingLBs:   []network.LoadBalancer{lb2},
			pipExists:     true,
			expectedGotLB: true,
			expectedStatus: &v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "1.2.3.4"},
				},
			},
		},
	}

	for _, c := range tests {
		t.Run(c.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			az := GetTestCloud(ctrl)
			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			if c.pipExists {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return([]network.PublicIPAddress{
					{
						Name: pointer.String("testCluster-aservice"),
						PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
							IPAddress: pointer.String("1.2.3.4"),
						},
					},
				}, nil)
			} else {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return([]network.PublicIPAddress{}, nil).MaxTimes(2)
			}
			mockLBsClient := az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
			mockLBsClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(c.existingLBs, nil)

			service := c.service
			status, existsLB, err := az.GetLoadBalancer(context.TODO(), testClusterName, &service)
			assert.Nil(t, err)
			assert.Equal(t, c.expectedGotLB, existsLB)
			assert.Equal(t, c.expectedStatus, status)
		})
	}
}

func TestFindRule(t *testing.T) {
	tests := []struct {
		msg          string
		existingRule []network.LoadBalancingRule
		curRule      network.LoadBalancingRule
		expected     bool
	}{
		{
			msg:      "empty existing rules should return false",
			expected: false,
		},
		{
			msg: "rule names don't match should return false",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("httpProbe1"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						FrontendPort: pointer.Int32(1),
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("httpProbe2"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					FrontendPort: pointer.Int32(1),
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while protocols don't should return false",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("httpRule"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						Protocol: network.TransportProtocolTCP,
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("httpRule"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					Protocol: network.TransportProtocolUDP,
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while EnableTCPResets don't should return false",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("httpRule"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						Protocol:       network.TransportProtocolTCP,
						EnableTCPReset: pointer.Bool(true),
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("httpRule"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					Protocol:       network.TransportProtocolTCP,
					EnableTCPReset: pointer.Bool(false),
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while frontend ports don't should return false",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("httpProbe"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						FrontendPort: pointer.Int32(1),
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("httpProbe"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					FrontendPort: pointer.Int32(2),
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while backend ports don't should return false",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("httpProbe"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						BackendPort: pointer.Int32(1),
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("httpProbe"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					BackendPort: pointer.Int32(2),
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while idletimeout don't should return false",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("httpRule"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						IdleTimeoutInMinutes: pointer.Int32(1),
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("httpRule"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					IdleTimeoutInMinutes: pointer.Int32(2),
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while idletimeout nil should return true",
			existingRule: []network.LoadBalancingRule{
				{
					Name:                              pointer.String("httpRule"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("httpRule"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					IdleTimeoutInMinutes: pointer.Int32(2),
				},
			},
			expected: true,
		},
		{
			msg: "rule names match while LoadDistribution don't should return false",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("httpRule"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						LoadDistribution: network.LoadDistributionSourceIP,
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("httpRule"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					LoadDistribution: network.LoadDistributionDefault,
				},
			},
			expected: false,
		},
		{
			msg: "rule and probe names match should return true",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("probe1"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						Probe: &network.SubResource{ID: pointer.String("probe")},
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("probe1"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					Probe: &network.SubResource{ID: pointer.String("probe")},
				},
			},
			expected: true,
		},
		{
			msg: "rule names match while probe don't should return false",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("probe1"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						Probe: nil,
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("probe1"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					Probe: &network.SubResource{ID: pointer.String("probe")},
				},
			},
			expected: false,
		},
		{
			msg: "both rule names and LoadBalancingRulePropertiesFormats match should return true",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("matchName"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						BackendPort:      pointer.Int32(2),
						FrontendPort:     pointer.Int32(2),
						LoadDistribution: network.LoadDistributionSourceIP,
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("matchName"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					BackendPort:      pointer.Int32(2),
					FrontendPort:     pointer.Int32(2),
					LoadDistribution: network.LoadDistributionSourceIP,
				},
			},
			expected: true,
		},
		{
			msg: "rule and FrontendIPConfiguration names match should return true",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("matchName"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						FrontendIPConfiguration: &network.SubResource{ID: pointer.String("FrontendIPConfiguration")},
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("matchName"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					FrontendIPConfiguration: &network.SubResource{ID: pointer.String("frontendipconfiguration")},
				},
			},
			expected: true,
		},
		{
			msg: "rule names match while FrontendIPConfiguration don't should return false",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("matchName"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						FrontendIPConfiguration: &network.SubResource{ID: pointer.String("FrontendIPConfiguration")},
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("matchName"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					FrontendIPConfiguration: &network.SubResource{ID: pointer.String("frontendipconifguration")},
				},
			},
			expected: false,
		},
		{
			msg: "rule and BackendAddressPool names match should return true",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("matchName"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						BackendAddressPool: &network.SubResource{ID: pointer.String("BackendAddressPool")},
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("matchName"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					BackendAddressPool: &network.SubResource{ID: pointer.String("backendaddresspool")},
				},
			},
			expected: true,
		},
		{
			msg: "rule and Probe names match should return true",
			existingRule: []network.LoadBalancingRule{
				{
					Name: pointer.String("matchName"),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						Probe: &network.SubResource{ID: pointer.String("Probe")},
					},
				},
			},
			curRule: network.LoadBalancingRule{
				Name: pointer.String("matchName"),
				LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
					Probe: &network.SubResource{ID: pointer.String("probe")},
				},
			},
			expected: true,
		},
	}

	for _, test := range tests {
		t.Run(test.msg, func(t *testing.T) {
			findResult := findRule(test.existingRule, test.curRule, true)
			assert.Equal(t, test.expected, findResult)
		})
	}
}

func TestSubnet(t *testing.T) {
	for i, c := range []struct {
		desc     string
		service  *v1.Service
		expected *string
	}{
		{
			desc:     "No annotation should return nil",
			service:  &v1.Service{},
			expected: nil,
		},
		{
			desc: "annotation with subnet but no ILB should return nil",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerInternalSubnet: "subnet",
					},
				},
			},
			expected: nil,
		},
		{
			desc: "annotation with subnet but ILB=false should return nil",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerInternalSubnet: "subnet",
						consts.ServiceAnnotationLoadBalancerInternal:       "false",
					},
				},
			},
			expected: nil,
		},
		{
			desc: "annotation with empty subnet should return nil",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerInternalSubnet: "",
						consts.ServiceAnnotationLoadBalancerInternal:       "true",
					},
				},
			},
			expected: nil,
		},
		{
			desc: "annotation with subnet and ILB should return subnet",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerInternalSubnet: "subnet",
						consts.ServiceAnnotationLoadBalancerInternal:       "true",
					},
				},
			},
			expected: pointer.String("subnet"),
		},
	} {
		real := getInternalSubnet(c.service)
		assert.Equal(t, c.expected, real, fmt.Sprintf("TestCase[%d]: %s", i, c.desc))
	}
}

func TestEnsureLoadBalancerDeleted(t *testing.T) {
	const vmCount = 8
	const availabilitySetCount = 4

	tests := []struct {
		desc              string
		service           v1.Service
		isInternalSvc     bool
		expectCreateError bool
		wrongRGAtDelete   bool
		flipService       bool
	}{
		{
			desc:        "external service then flipped to internal should be created and deleted successfully",
			service:     getTestService("service1", v1.ProtocolTCP, nil, false, 80),
			flipService: true,
		},
		{
			desc:          "internal service then flipped to external should be created and deleted successfully",
			service:       getInternalTestService("service2", 80),
			isInternalSvc: true,
			flipService:   true,
		},
		{
			desc:    "external service should be created and deleted successfully",
			service: getTestService("service3", v1.ProtocolTCP, nil, false, 80),
		},
		{
			desc:          "internal service should be created and deleted successfully",
			service:       getInternalTestService("service4", 80),
			isInternalSvc: true,
		},
		{
			desc:    "annotated service with same resourceGroup should be created and deleted successfully",
			service: getResourceGroupTestService("service5", "rg", "", 80),
		},
		{
			desc:              "annotated service with different resourceGroup shouldn't be created but should be deleted successfully",
			service:           getResourceGroupTestService("service6", "random-rg", "1.2.3.4", 80),
			expectCreateError: true,
		},
		{
			desc:              "annotated service with different resourceGroup shouldn't be created but should be deleted successfully",
			service:           getResourceGroupTestService("service7", "random-rg", "", 80),
			expectCreateError: true,
			wrongRGAtDelete:   true,
		},
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(clusterName string, service *v1.Service, lb *network.LoadBalancer) (bool, bool, *network.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLBBackendPool.EXPECT().GetBackendPrivateIPs(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, vmCount, availabilitySetCount)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 5)

	for i, c := range tests {
		service := c.service
		if c.service.Annotations[consts.ServiceAnnotationLoadBalancerInternal] == "true" {
			validateTestSubnet(t, az, &service)
		}

		expectedLBs := make([]network.LoadBalancer, 0)
		setMockLBs(az, ctrl, &expectedLBs, "service", 1, i+1, c.isInternalSvc)

		mockPLSClient := mockprivatelinkserviceclient.NewMockInterface(ctrl)
		mockPLSClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return([]network.PrivateLinkService{}, nil).MaxTimes(2)
		az.PrivateLinkServiceClient = mockPLSClient

		// create the service first.
		lbStatus, err := az.EnsureLoadBalancer(context.TODO(), testClusterName, &service, clusterResources.nodes)
		if c.expectCreateError {
			assert.NotNil(t, err, "TestCase[%d]: %s", i, c.desc)
		} else {
			assert.Nil(t, err, "TestCase[%d]: %s", i, c.desc)
			assert.NotNil(t, lbStatus, "TestCase[%d]: %s", i, c.desc)
			result, rerr := az.LoadBalancerClient.List(context.TODO(), az.Config.ResourceGroup)
			assert.Nil(t, rerr, "TestCase[%d]: %s", i, c.desc)
			assert.Equal(t, 1, len(result), "TestCase[%d]: %s", i, c.desc)
			assert.Equal(t, 1, len(*result[0].LoadBalancingRules), "TestCase[%d]: %s", i, c.desc)
		}

		// finally, delete it.
		if c.wrongRGAtDelete {
			az.LoadBalancerResourceGroup = "nil"
		}
		if c.flipService {
			flippedService := flipServiceInternalAnnotation(&service)
			c.service = *flippedService
			c.isInternalSvc = !c.isInternalSvc
		}
		expectedLBs = make([]network.LoadBalancer, 0)
		setMockLBs(az, ctrl, &expectedLBs, "service", 1, i+1, c.isInternalSvc)

		err = az.EnsureLoadBalancerDeleted(context.TODO(), testClusterName, &service)
		expectedLBs = make([]network.LoadBalancer, 0)
		mockLBsClient := mockloadbalancerclient.NewMockInterface(ctrl)
		mockLBsClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(expectedLBs, nil).MaxTimes(2)
		az.LoadBalancerClient = mockLBsClient
		assert.Nil(t, err, "TestCase[%d]: %s", i, c.desc)
		result, rerr := az.LoadBalancerClient.List(context.Background(), az.Config.ResourceGroup)
		assert.Nil(t, rerr, "TestCase[%d]: %s", i, c.desc)
		assert.Equal(t, 0, len(result), "TestCase[%d]: %s", i, c.desc)
	}
}

func TestServiceOwnsPublicIP(t *testing.T) {
	tests := []struct {
		desc                    string
		pip                     *network.PublicIPAddress
		clusterName             string
		serviceName             string
		serviceLBIP             string
		serviceLBName           string
		expectedOwns            bool
		expectedUserAssignedPIP bool
	}{
		{
			desc:         "false should be returned when pip is nil",
			clusterName:  "kubernetes",
			serviceName:  "nginx",
			expectedOwns: false,
		},
		{
			desc: "false should be returned when service name tag doesn't match",
			pip: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				Tags: map[string]*string{
					consts.ServiceTagKey: pointer.String("default/nginx"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			serviceName:  "web",
			expectedOwns: false,
		},
		{
			desc: "true should be returned when service name tag matches and cluster name tag is not set",
			pip: &network.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey: pointer.String("default/nginx"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx",
			expectedOwns: true,
		},
		{
			desc: "false should be returned when cluster name doesn't match",
			pip: &network.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey:  pointer.String("default/nginx"),
					consts.ClusterNameKey: pointer.String("kubernetes"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			clusterName:  "k8s",
			serviceName:  "nginx",
			expectedOwns: false,
		},
		{
			desc: "false should be returned when cluster name matches while service name doesn't match",
			pip: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				Tags: map[string]*string{
					consts.ServiceTagKey:  pointer.String("default/web"),
					consts.ClusterNameKey: pointer.String("kubernetes"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx",
			expectedOwns: false,
		},
		{
			desc: "true should be returned when both service name tag and cluster name match",
			pip: &network.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey:  pointer.String("default/nginx"),
					consts.ClusterNameKey: pointer.String("kubernetes"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx",
			expectedOwns: true,
		},
		{
			desc: "false should be returned when the tag is empty and load balancer IP does not match",
			pip: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				Tags: map[string]*string{
					consts.ServiceTagKey:  pointer.String(""),
					consts.ClusterNameKey: pointer.String("kubernetes"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			clusterName:             "kubernetes",
			serviceName:             "nginx",
			expectedOwns:            false,
			expectedUserAssignedPIP: true,
		},
		{
			desc: "true should be returned if there is a match among a multi-service tag",
			pip: &network.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey:  pointer.String("default/nginx1,default/nginx2"),
					consts.ClusterNameKey: pointer.String("kubernetes"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx1",
			expectedOwns: true,
		},
		{
			desc: "false should be returned if there is not a match among a multi-service tag",
			pip: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				Tags: map[string]*string{
					consts.ServiceTagKey:  pointer.String("default/nginx1,default/nginx2"),
					consts.ClusterNameKey: pointer.String("kubernetes"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx3",
			expectedOwns: false,
		},
		{
			desc: "true should be returned if the load balancer IP is matched even if the svc name is not included in the tag",
			pip: &network.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey:  pointer.String(""),
					consts.ClusterNameKey: pointer.String("kubernetes"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			clusterName:             "kubernetes",
			serviceName:             "nginx3",
			serviceLBIP:             "1.2.3.4",
			expectedOwns:            true,
			expectedUserAssignedPIP: true,
		},
		{
			desc: "true should be returned if the load balancer IP is not matched but the svc name is included in the tag",
			pip: &network.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey:  pointer.String("default/nginx1,default/nginx2"),
					consts.ClusterNameKey: pointer.String("kubernetes"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx1",
			serviceLBIP:  "1.1.1.1",
			expectedOwns: true,
		},
		{
			desc: "should be user-assigned pip if it has no tags",
			pip: &network.PublicIPAddress{
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			serviceLBIP:             "1.2.3.4",
			expectedOwns:            true,
			expectedUserAssignedPIP: true,
		},
		{
			desc: "should be true if the pip name matches",
			pip: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			serviceLBName:           "pip1",
			expectedOwns:            true,
			expectedUserAssignedPIP: true,
		},
		{
			desc: "should be true if the pip with tag matches the pip name",
			pip: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				Tags: map[string]*string{},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			serviceLBName:           "pip1",
			expectedOwns:            true,
			expectedUserAssignedPIP: true,
		},
		{
			desc: "should be true if the pip with service tag matches the pip name",
			pip: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				Tags: map[string]*string{
					consts.ServiceTagKey: pointer.String("default/web"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress: pointer.String("1.2.3.4"),
				},
			},
			serviceLBName: "pip1",
			expectedOwns:  true,
		},
	}

	for i, c := range tests {
		t.Run(c.desc, func(t *testing.T) {
			service := getTestService(c.serviceName, v1.ProtocolTCP, nil, false, 80)
			if c.serviceLBIP != "" {
				setServiceLoadBalancerIP(&service, c.serviceLBIP)
			}
			if c.serviceLBName != "" {
				if service.ObjectMeta.Annotations == nil {
					service.ObjectMeta.Annotations = map[string]string{consts.ServiceAnnotationPIPNameDualStack[false]: "pip1"}
				} else {
					service.ObjectMeta.Annotations[consts.ServiceAnnotationPIPNameDualStack[false]] = "pip1"
				}
			}
			owns, isUserAssignedPIP := serviceOwnsPublicIP(&service, c.pip, c.clusterName)
			assert.Equal(t, c.expectedOwns, owns, "TestCase[%d]: %s", i, c.desc)
			assert.Equal(t, c.expectedUserAssignedPIP, isUserAssignedPIP, "TestCase[%d]: %s", i, c.desc)
		})
	}
}

func TestGetPublicIPAddressResourceGroup(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	for i, c := range []struct {
		desc        string
		annotations map[string]string
		expected    string
	}{
		{
			desc:     "no annotation",
			expected: "rg",
		},
		{
			desc:        "annotation with empty string resource group",
			annotations: map[string]string{consts.ServiceAnnotationLoadBalancerResourceGroup: ""},
			expected:    "rg",
		},
		{
			desc:        "annotation with non-empty resource group ",
			annotations: map[string]string{consts.ServiceAnnotationLoadBalancerResourceGroup: "rg2"},
			expected:    "rg2",
		},
	} {
		t.Run(c.desc, func(t *testing.T) {
			s := &v1.Service{}
			s.Annotations = c.annotations
			real := az.getPublicIPAddressResourceGroup(s)
			assert.Equal(t, c.expected, real, "TestCase[%d]: %s", i, c.desc)
		})
	}
}

func TestShouldReleaseExistingOwnedPublicIP(t *testing.T) {
	existingPipWithTag := network.PublicIPAddress{
		ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
		Name: pointer.String("testPIP"),
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			PublicIPAddressVersion:   network.IPv4,
			PublicIPAllocationMethod: network.Static,
			IPTags: &[]network.IPTag{
				{
					IPTagType: pointer.String("tag1"),
					Tag:       pointer.String("tag1value"),
				},
			},
		},
	}
	existingPipWithTagIPv6Suffix := existingPipWithTag
	existingPipWithTagIPv6Suffix.Name = pointer.String("testPIP-IPv6")

	existingPipWithNoPublicIPAddressFormatProperties := network.PublicIPAddress{
		ID:                              pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
		Name:                            pointer.String("testPIP"),
		Tags:                            map[string]*string{consts.ServiceTagKey: pointer.String("default/test2")},
		PublicIPAddressPropertiesFormat: nil,
	}

	tests := []struct {
		desc                  string
		desiredPipName        string
		existingPip           network.PublicIPAddress
		ipTagRequest          serviceIPTagRequest
		tags                  map[string]*string
		lbShouldExist         bool
		lbIsInternal          bool
		isUserAssignedPIP     bool
		expectedShouldRelease bool
	}{
		{
			desc:           "Everything matches, no release",
			existingPip:    existingPipWithTag,
			lbShouldExist:  true,
			lbIsInternal:   false,
			desiredPipName: *existingPipWithTag.Name,
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      existingPipWithTag.PublicIPAddressPropertiesFormat.IPTags,
			},
			expectedShouldRelease: false,
		},
		{
			desc:           "nil tags (none-specified by annotation, some are present on object), no release",
			existingPip:    existingPipWithTag,
			lbShouldExist:  true,
			lbIsInternal:   false,
			desiredPipName: *existingPipWithTag.Name,
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: false,
				IPTags:                      nil,
			},
			expectedShouldRelease: false,
		},
		{
			desc:           "existing public ip with no format properties (unit test only?), tags required by annotation, expect release",
			existingPip:    existingPipWithNoPublicIPAddressFormatProperties,
			lbShouldExist:  true,
			lbIsInternal:   false,
			desiredPipName: *existingPipWithTag.Name,
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      existingPipWithTag.PublicIPAddressPropertiesFormat.IPTags,
			},
			expectedShouldRelease: true,
		},
		{
			desc:           "LB no longer desired, expect release",
			existingPip:    existingPipWithTag,
			lbShouldExist:  false,
			lbIsInternal:   false,
			desiredPipName: *existingPipWithTag.Name,
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      existingPipWithTag.PublicIPAddressPropertiesFormat.IPTags,
			},
			expectedShouldRelease: true,
		},
		{
			desc:           "LB now internal, expect release",
			existingPip:    existingPipWithTag,
			lbShouldExist:  true,
			lbIsInternal:   true,
			desiredPipName: *existingPipWithTag.Name,
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      existingPipWithTag.PublicIPAddressPropertiesFormat.IPTags,
			},
			expectedShouldRelease: true,
		},
		{
			desc:           "Alternate desired name, expect release",
			existingPip:    existingPipWithTag,
			lbShouldExist:  true,
			lbIsInternal:   false,
			desiredPipName: "otherName",
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      existingPipWithTag.PublicIPAddressPropertiesFormat.IPTags,
			},
			expectedShouldRelease: true,
		},
		{
			desc:           "mismatching, expect release",
			existingPip:    existingPipWithTag,
			lbShouldExist:  true,
			lbIsInternal:   false,
			desiredPipName: *existingPipWithTag.Name,
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags: &[]network.IPTag{
					{
						IPTagType: pointer.String("tag2"),
						Tag:       pointer.String("tag2value"),
					},
				},
			},
			expectedShouldRelease: true,
		},
		{
			// This test is for IPv6 PIP created with CCM v1.27.1 and CCM gets upgraded.
			// Such PIP should be recreated.
			desc:           "matching except PIP name (with IPv6 suffix), expect release",
			existingPip:    existingPipWithTagIPv6Suffix,
			lbShouldExist:  true,
			lbIsInternal:   false,
			desiredPipName: *existingPipWithTag.Name,
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags: &[]network.IPTag{
					{
						IPTagType: pointer.String("tag1"),
						Tag:       pointer.String("tag1value"),
					},
				},
			},
			expectedShouldRelease: true,
		},
		{
			desc:           "should delete orphaned managed public IP",
			existingPip:    existingPipWithTag,
			lbShouldExist:  false,
			lbIsInternal:   false,
			desiredPipName: *existingPipWithTag.Name,
			tags:           map[string]*string{consts.ServiceTagKey: pointer.String("")},
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      existingPipWithTag.PublicIPAddressPropertiesFormat.IPTags,
			},
			expectedShouldRelease: true,
		},
		{
			desc:           "should not delete managed public IP which has references",
			existingPip:    existingPipWithTag,
			lbShouldExist:  false,
			lbIsInternal:   false,
			desiredPipName: *existingPipWithTag.Name,
			tags:           map[string]*string{consts.ServiceTagKey: pointer.String("svc1")},
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      existingPipWithTag.PublicIPAddressPropertiesFormat.IPTags,
			},
		},
		{
			desc:           "should not delete orphaned unmanaged public IP",
			existingPip:    existingPipWithTag,
			lbShouldExist:  false,
			lbIsInternal:   false,
			desiredPipName: *existingPipWithTag.Name,
			tags:           map[string]*string{consts.ServiceTagKey: pointer.String("")},
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      existingPipWithTag.PublicIPAddressPropertiesFormat.IPTags,
			},
			isUserAssignedPIP: true,
		},
	}

	for _, c := range tests {
		t.Run(c.desc, func(t *testing.T) {
			if c.tags != nil {
				c.existingPip.Tags = c.tags
			}
			existingPip := c.existingPip
			actualShouldRelease := shouldReleaseExistingOwnedPublicIP(&existingPip, c.lbShouldExist, c.lbIsInternal, c.isUserAssignedPIP, c.desiredPipName, c.ipTagRequest)
			assert.Equal(t, c.expectedShouldRelease, actualShouldRelease)
		})
	}
}

func TestGetIPTagMap(t *testing.T) {
	tests := []struct {
		desc     string
		input    string
		expected map[string]string
	}{
		{
			desc:     "empty map should be returned when service has blank annotations",
			input:    "",
			expected: map[string]string{},
		},
		{
			desc:  "a single tag should be returned when service has set one tag pair in the annotation",
			input: "tag1=tagvalue1",
			expected: map[string]string{
				"tag1": "tagvalue1",
			},
		},
		{
			desc:  "a single tag should be returned when service has set one tag pair in the annotation (and spaces are trimmed)",
			input: " tag1 = tagvalue1 ",
			expected: map[string]string{
				"tag1": "tagvalue1",
			},
		},
		{
			desc:  "a single tag should be returned when service has set two tag pairs in the annotation with the same key (last write wins - according to appearance order in the string)",
			input: "tag1=tagvalue1,tag1=tagvalue1new",
			expected: map[string]string{
				"tag1": "tagvalue1new",
			},
		},
		{
			desc:  "two tags should be returned when service has set two tag pairs in the annotation",
			input: "tag1=tagvalue1,tag2=tagvalue2",
			expected: map[string]string{
				"tag1": "tagvalue1",
				"tag2": "tagvalue2",
			},
		},
		{
			desc:  "two tags should be returned when service has set two tag pairs (and one malformation) in the annotation",
			input: "tag1=tagvalue1,tag2=tagvalue2,tag3malformed",
			expected: map[string]string{
				"tag1": "tagvalue1",
				"tag2": "tagvalue2",
			},
		},
		{
			// We may later decide not to support blank values.  The Azure contract is not entirely clear here.
			desc:  "two tags should be returned when service has set two tag pairs (and one has a blank value) in the annotation",
			input: "tag1=tagvalue1,tag2=",
			expected: map[string]string{
				"tag1": "tagvalue1",
				"tag2": "",
			},
		},
		{
			// We may later decide not to support blank keys.  The Azure contract is not entirely clear here.
			desc:  "two tags should be returned when service has set two tag pairs (and one has a blank key) in the annotation",
			input: "tag1=tagvalue1,=tag2value",
			expected: map[string]string{
				"tag1": "tagvalue1",
				"":     "tag2value",
			},
		},
	}

	for i, c := range tests {
		actual := getIPTagMap(c.input)
		assert.Equal(t, c.expected, actual, "TestCase[%d]: %s", i, c.desc)
	}
}

func TestConvertIPTagMapToSlice(t *testing.T) {
	tests := []struct {
		desc     string
		input    map[string]string
		expected *[]network.IPTag
	}{
		{
			desc:     "nil slice should be returned when the map is nil",
			input:    nil,
			expected: nil,
		},
		{
			desc:     "empty slice should be returned when the map is empty",
			input:    map[string]string{},
			expected: &[]network.IPTag{},
		},
		{
			desc: "one tag should be returned when the map has one tag",
			input: map[string]string{
				"tag1": "tag1value",
			},
			expected: &[]network.IPTag{
				{
					IPTagType: pointer.String("tag1"),
					Tag:       pointer.String("tag1value"),
				},
			},
		},
		{
			desc: "two tags should be returned when the map has two tags",
			input: map[string]string{
				"tag1": "tag1value",
				"tag2": "tag2value",
			},
			expected: &[]network.IPTag{
				{
					IPTagType: pointer.String("tag1"),
					Tag:       pointer.String("tag1value"),
				},
				{
					IPTagType: pointer.String("tag2"),
					Tag:       pointer.String("tag2value"),
				},
			},
		},
	}

	for i, c := range tests {
		actual := convertIPTagMapToSlice(c.input)

		// Sort output to provide stability of return from map for test comparison
		// The order doesn't matter at runtime.
		if actual != nil {
			sort.Slice(*actual, func(i, j int) bool {
				ipTagSlice := *actual
				return pointer.StringDeref(ipTagSlice[i].IPTagType, "") < pointer.StringDeref(ipTagSlice[j].IPTagType, "")
			})
		}
		if c.expected != nil {
			sort.Slice(*c.expected, func(i, j int) bool {
				ipTagSlice := *c.expected
				return pointer.StringDeref(ipTagSlice[i].IPTagType, "") < pointer.StringDeref(ipTagSlice[j].IPTagType, "")
			})

		}

		assert.Equal(t, c.expected, actual, "TestCase[%d]: %s", i, c.desc)
	}
}

func TestGetserviceIPTagRequestForPublicIP(t *testing.T) {
	tests := []struct {
		desc     string
		input    *v1.Service
		expected serviceIPTagRequest
	}{
		{
			desc:  "Annotation should be false when service is absent",
			input: nil,
			expected: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: false,
				IPTags:                      nil,
			},
		},
		{
			desc: "Annotation should be false when service is present, without annotation",
			input: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			expected: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: false,
				IPTags:                      nil,
			},
		},
		{
			desc: "Annotation should be true, tags slice empty, when annotation blank",
			input: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationIPTagsForPublicIP: "",
					},
				},
			},
			expected: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      &[]network.IPTag{},
			},
		},
		{
			desc: "two tags should be returned when service has set two tag pairs (and one malformation) in the annotation",
			input: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationIPTagsForPublicIP: "tag1=tag1value,tag2=tag2value,tag3malformed",
					},
				},
			},
			expected: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags: &[]network.IPTag{
					{
						IPTagType: pointer.String("tag1"),
						Tag:       pointer.String("tag1value"),
					},
					{
						IPTagType: pointer.String("tag2"),
						Tag:       pointer.String("tag2value"),
					},
				},
			},
		},
	}
	for i, c := range tests {
		actual := getServiceIPTagRequestForPublicIP(c.input)

		// Sort output to provide stability of return from map for test comparison
		// The order doesn't matter at runtime.
		if actual.IPTags != nil {
			sort.Slice(*actual.IPTags, func(i, j int) bool {
				ipTagSlice := *actual.IPTags
				return pointer.StringDeref(ipTagSlice[i].IPTagType, "") < pointer.StringDeref(ipTagSlice[j].IPTagType, "")
			})
		}
		if c.expected.IPTags != nil {
			sort.Slice(*c.expected.IPTags, func(i, j int) bool {
				ipTagSlice := *c.expected.IPTags
				return pointer.StringDeref(ipTagSlice[i].IPTagType, "") < pointer.StringDeref(ipTagSlice[j].IPTagType, "")
			})

		}

		assert.Equal(t, actual, c.expected, "TestCase[%d]: %s", i, c.desc)
	}
}

func TestAreIpTagsEquivalent(t *testing.T) {
	tests := []struct {
		desc     string
		input1   *[]network.IPTag
		input2   *[]network.IPTag
		expected bool
	}{
		{
			desc:     "nils should be considered equal",
			input1:   nil,
			input2:   nil,
			expected: true,
		},
		{
			desc:     "nils should be considered to empty arrays (case 1)",
			input1:   nil,
			input2:   &[]network.IPTag{},
			expected: true,
		},
		{
			desc:     "nils should be considered to empty arrays (case 1)",
			input1:   &[]network.IPTag{},
			input2:   nil,
			expected: true,
		},
		{
			desc: "nil should not be considered equal to anything (case 1)",
			input1: &[]network.IPTag{
				{
					IPTagType: pointer.String("tag1"),
					Tag:       pointer.String("tag1value"),
				},
				{
					IPTagType: pointer.String("tag2"),
					Tag:       pointer.String("tag2value"),
				},
			},
			input2:   nil,
			expected: false,
		},
		{
			desc: "nil should not be considered equal to anything (case 2)",
			input2: &[]network.IPTag{
				{
					IPTagType: pointer.String("tag1"),
					Tag:       pointer.String("tag1value"),
				},
				{
					IPTagType: pointer.String("tag2"),
					Tag:       pointer.String("tag2value"),
				},
			},
			input1:   nil,
			expected: false,
		},
		{
			desc: "exactly equal should be treated as equal",
			input1: &[]network.IPTag{
				{
					IPTagType: pointer.String("tag1"),
					Tag:       pointer.String("tag1value"),
				},
				{
					IPTagType: pointer.String("tag2"),
					Tag:       pointer.String("tag2value"),
				},
			},
			input2: &[]network.IPTag{
				{
					IPTagType: pointer.String("tag1"),
					Tag:       pointer.String("tag1value"),
				},
				{
					IPTagType: pointer.String("tag2"),
					Tag:       pointer.String("tag2value"),
				},
			},
			expected: true,
		},
		{
			desc: "equal but out of order should be treated as equal",
			input1: &[]network.IPTag{
				{
					IPTagType: pointer.String("tag1"),
					Tag:       pointer.String("tag1value"),
				},
				{
					IPTagType: pointer.String("tag2"),
					Tag:       pointer.String("tag2value"),
				},
			},
			input2: &[]network.IPTag{
				{
					IPTagType: pointer.String("tag2"),
					Tag:       pointer.String("tag2value"),
				},
				{
					IPTagType: pointer.String("tag1"),
					Tag:       pointer.String("tag1value"),
				},
			},
			expected: true,
		},
	}
	for i, c := range tests {
		actual := areIPTagsEquivalent(c.input1, c.input2)
		assert.Equal(t, actual, c.expected, "TestCase[%d]: %s", i, c.desc)
	}
}

func TestGetServiceTags(t *testing.T) {
	tests := []struct {
		desc     string
		service  *v1.Service
		expected []string
	}{
		{
			desc:     "nil should be returned when service is nil",
			service:  nil,
			expected: nil,
		},
		{
			desc:     "nil should be returned when service has no annotations",
			service:  &v1.Service{},
			expected: nil,
		},
		{
			desc: "single tag should be returned when service has set one annotations",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationAllowedServiceTags: "tag1",
					},
				},
			},
			expected: []string{"tag1"},
		},
		{
			desc: "multiple tags should be returned when service has set multi-annotations",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationAllowedServiceTags: "tag1, tag2",
					},
				},
			},
			expected: []string{"tag1", "tag2"},
		},
		{
			desc: "correct tags should be returned when comma or spaces are included in the annotations",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationAllowedServiceTags: ", tag1, ",
					},
				},
			},
			expected: []string{"tag1"},
		},
	}

	for i, c := range tests {
		tags := getServiceTags(c.service)
		assert.Equal(t, tags, c.expected, "TestCase[%d]: %s", i, c.desc)
	}
}

func TestGetServiceLoadBalancerCommon(t *testing.T) {
	testCases := []struct {
		desc           string
		sku            string
		existingLBs    []network.LoadBalancer
		service        v1.Service
		annotations    map[string]string
		expectedLB     *network.LoadBalancer
		expectedStatus *v1.LoadBalancerStatus
		wantLB         bool
		expectedExists bool
		expectedError  bool
	}{
		{
			desc: "getServiceLoadBalancer shall return corresponding lb, status, exists if there are existed lbs",
			existingLBs: []network.LoadBalancer{
				{
					Name: pointer.String("testCluster"),
					LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
						FrontendIPConfigurations: &[]network.FrontendIPConfiguration{
							{
								Name: pointer.String("aservice1"),
								FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
									PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
								},
							},
						},
					},
				},
			},
			service: getTestService("service1", v1.ProtocolTCP, nil, false, 80),
			wantLB:  false,
			expectedLB: &network.LoadBalancer{
				Name: pointer.String("testCluster"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					FrontendIPConfigurations: &[]network.FrontendIPConfiguration{
						{
							Name: pointer.String("aservice1"),
							FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
								PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
							},
						},
					},
				},
			},
			expectedStatus: &v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: "1.2.3.4", Hostname: ""}}},
			expectedExists: true,
			expectedError:  false,
		},
		{
			desc: "getServiceLoadBalancer shall select the lb with minimum lb rules if wantLb is true, the sku is " +
				"not standard and there are existing lbs already",
			existingLBs: []network.LoadBalancer{
				{
					Name: pointer.String("testCluster"),
					LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
						LoadBalancingRules: &[]network.LoadBalancingRule{
							{Name: pointer.String("rule1")},
						},
					},
				},
				{
					Name: pointer.String("as-1"),
					LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
						LoadBalancingRules: &[]network.LoadBalancingRule{
							{Name: pointer.String("rule1")},
							{Name: pointer.String("rule2")},
						},
					},
				},
				{
					Name: pointer.String("as-2"),
					LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
						LoadBalancingRules: &[]network.LoadBalancingRule{
							{Name: pointer.String("rule1")},
							{Name: pointer.String("rule2")},
							{Name: pointer.String("rule3")},
						},
					},
				},
			},
			service:     getTestService("service1", v1.ProtocolTCP, nil, false, 80),
			annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: "__auto__"},
			wantLB:      true,
			expectedLB: &network.LoadBalancer{
				Name: pointer.String("testCluster"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					LoadBalancingRules: &[]network.LoadBalancingRule{
						{Name: pointer.String("rule1")},
					},
				},
			},
			expectedExists: false,
			expectedError:  false,
		},
		{
			desc:    "getServiceLoadBalancer shall create a new lb otherwise",
			service: getTestService("service1", v1.ProtocolTCP, nil, false, 80),
			expectedLB: &network.LoadBalancer{
				Name:                         pointer.String("testCluster"),
				Location:                     pointer.String("westus"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{},
			},
			expectedExists: false,
			expectedError:  false,
		},
		{
			desc:    "getServiceLoadBalancer should create a new lb and return the status of the previous lb",
			sku:     "Basic",
			wantLB:  true,
			service: getTestService("service1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationLoadBalancerMode: "as", consts.ServiceAnnotationLoadBalancerInternal: consts.TrueAnnotationValue}, false, 80),
			existingLBs: []network.LoadBalancer{
				{
					Name:     pointer.String("as-internal"),
					Location: pointer.String("westus"),
					LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
						FrontendIPConfigurations: &[]network.FrontendIPConfiguration{
							{
								Name: pointer.String("aservice1"),
								ID:   pointer.String("as-internal-aservice1"),
								FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
									PrivateIPAddress: pointer.String("1.2.3.4"),
								},
							},
						},
					},
				},
			},
			expectedLB: &network.LoadBalancer{
				Name:     pointer.String("testCluster-internal"),
				Location: pointer.String("westus"),
				Sku: &network.LoadBalancerSku{
					Name: network.LoadBalancerSkuNameBasic,
				},
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{},
			},
			expectedStatus: &v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: "1.2.3.4", Hostname: ""}}},
		},
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 3, 3)
			setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

			mockLBsClient := mockloadbalancerclient.NewMockInterface(ctrl)
			mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(len(test.existingLBs))
			mockLBsClient.EXPECT().List(gomock.Any(), "rg").Return(test.existingLBs, nil)
			mockLBsClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			az.LoadBalancerClient = mockLBsClient

			expectedPLS := make([]network.PrivateLinkService, 0)
			mockPLSClient := mockprivatelinkserviceclient.NewMockInterface(ctrl)
			mockPLSClient.EXPECT().List(gomock.Any(), "rg").Return(expectedPLS, nil).MaxTimes(1)
			az.PrivateLinkServiceClient = mockPLSClient

			for _, existingLB := range test.existingLBs {
				err := az.LoadBalancerClient.CreateOrUpdate(context.TODO(), "rg", *existingLB.Name, existingLB, "")
				assert.NoError(t, err.Error())
			}
			if test.annotations != nil {
				test.service.Annotations = test.annotations
			}
			az.LoadBalancerSku = test.sku
			service := test.service
			lb, status, _, exists, err := az.getServiceLoadBalancer(&service, testClusterName,
				clusterResources.nodes, test.wantLB, []network.LoadBalancer{})
			assert.Equal(t, test.expectedLB, lb)
			assert.Equal(t, test.expectedStatus, status)
			assert.Equal(t, test.expectedExists, exists)
			assert.Equal(t, test.expectedError, err != nil)
		})
	}
}

func TestGetServiceLoadBalancerWithExtendedLocation(t *testing.T) {
	service := getTestService("service1", v1.ProtocolTCP, nil, false, 80)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloudWithExtendedLocation(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 3, 3)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	// Test with wantLB=false
	expectedLB := &network.LoadBalancer{
		Name:     pointer.String("testCluster"),
		Location: pointer.String("westus"),
		ExtendedLocation: &network.ExtendedLocation{
			Name: pointer.String("microsoftlosangeles1"),
			Type: network.EdgeZone,
		},
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{},
	}
	mockLBsClient := mockloadbalancerclient.NewMockInterface(ctrl)
	mockLBsClient.EXPECT().List(gomock.Any(), "rg").Return(nil, nil)
	az.LoadBalancerClient = mockLBsClient

	lb, status, _, exists, err := az.getServiceLoadBalancer(&service, testClusterName,
		clusterResources.nodes, false, []network.LoadBalancer{})
	assert.Equal(t, expectedLB, lb, "GetServiceLoadBalancer shall return a default LB with expected location.")
	assert.Nil(t, status, "GetServiceLoadBalancer: Status should be nil for default LB.")
	assert.Equal(t, false, exists, "GetServiceLoadBalancer: Default LB should not exist.")
	assert.NoError(t, err, "GetServiceLoadBalancer: No error should be thrown when returning default LB.")

	// Test with wantLB=true
	expectedLB = &network.LoadBalancer{
		Name:     pointer.String("testCluster"),
		Location: pointer.String("westus"),
		ExtendedLocation: &network.ExtendedLocation{
			Name: pointer.String("microsoftlosangeles1"),
			Type: network.EdgeZone,
		},
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{},
		Sku: &network.LoadBalancerSku{
			Name: network.LoadBalancerSkuName("Basic"),
			Tier: network.LoadBalancerSkuTier(""),
		},
	}
	mockLBsClient = mockloadbalancerclient.NewMockInterface(ctrl)
	mockLBsClient.EXPECT().List(gomock.Any(), "rg").Return(nil, nil)
	az.LoadBalancerClient = mockLBsClient

	lb, status, _, exists, err = az.getServiceLoadBalancer(&service, testClusterName,
		clusterResources.nodes, true, []network.LoadBalancer{})
	assert.Equal(t, expectedLB, lb, "GetServiceLoadBalancer shall return a new LB with expected location.")
	assert.Nil(t, status, "GetServiceLoadBalancer: Status should be nil for new LB.")
	assert.Equal(t, false, exists, "GetServiceLoadBalancer: LB should not exist before hand.")
	assert.NoError(t, err, "GetServiceLoadBalancer: No error should be thrown when returning new LB.")
}

func TestIsFrontendIPChanged(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc                   string
		config                 network.FrontendIPConfiguration
		service                v1.Service
		lbFrontendIPConfigName string
		annotations            string
		loadBalancerIP         string
		existingSubnet         network.Subnet
		existingPIPs           []network.PublicIPAddress
		expectedFlag           bool
		expectedError          bool
	}{
		{
			desc: "isFrontendIPChanged shall return true if config.Name has a prefix of lb's name and " +
				"config.Name != lbFrontendIPConfigName",
			config:                 network.FrontendIPConfiguration{Name: pointer.String("atest1-name")},
			service:                getInternalTestService("test1", 80),
			lbFrontendIPConfigName: "configName",
			expectedFlag:           true,
			expectedError:          false,
		},
		{
			desc: "isFrontendIPChanged shall return false if config.Name doesn't have a prefix of lb's name " +
				"and config.Name != lbFrontendIPConfigName",
			config:                 network.FrontendIPConfiguration{Name: pointer.String("btest1-name")},
			service:                getInternalTestService("test1", 80),
			lbFrontendIPConfigName: "configName",
			expectedFlag:           false,
			expectedError:          false,
		},
		{
			desc: "isFrontendIPChanged shall return false if the service is internal, no loadBalancerIP is given, " +
				"subnetName == nil and config.PrivateIPAllocationMethod == network.Static",
			config: network.FrontendIPConfiguration{
				Name: pointer.String("btest1-name"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: network.Static,
				},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getInternalTestService("test1", 80),
			expectedFlag:           false,
			expectedError:          false,
		},
		{
			desc: "isFrontendIPChanged shall return false if the service is internal, no loadBalancerIP is given, " +
				"subnetName == nil and config.PrivateIPAllocationMethod != network.Static",
			config: network.FrontendIPConfiguration{
				Name: pointer.String("btest1-name"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: network.Dynamic,
				},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getInternalTestService("test1", 80),
			expectedFlag:           false,
			expectedError:          false,
		},
		{
			desc: "isFrontendIPChanged shall return true if the service is internal and " +
				"config.Subnet.ID != subnet.ID",
			config: network.FrontendIPConfiguration{
				Name: pointer.String("btest1-name"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					Subnet: &network.Subnet{ID: pointer.String("testSubnet")},
				},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getInternalTestService("test1", 80),
			annotations:            "testSubnet",
			existingSubnet:         network.Subnet{ID: pointer.String("testSubnet1")},
			expectedFlag:           true,
			expectedError:          false,
		},
		{
			desc: "isFrontendIPChanged shall return false if the service is internal, subnet == nil, " +
				"loadBalancerIP == config.PrivateIPAddress and config.PrivateIPAllocationMethod != 'static'",
			config: network.FrontendIPConfiguration{
				Name: pointer.String("btest1-name"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: network.Dynamic,
					PrivateIPAddress:          pointer.String("1.1.1.1"),
				},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getInternalTestService("test1", 80),
			loadBalancerIP:         "1.1.1.1",
			expectedFlag:           false,
			expectedError:          false,
		},
		{
			desc: "isFrontendIPChanged shall return false if the service is internal, subnet == nil, " +
				"loadBalancerIP == config.PrivateIPAddress and config.PrivateIPAllocationMethod == 'static'",
			config: network.FrontendIPConfiguration{
				Name: pointer.String("btest1-name"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: network.Static,
					PrivateIPAddress:          pointer.String("1.1.1.1"),
				},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getInternalTestService("test1", 80),
			loadBalancerIP:         "1.1.1.1",
			expectedFlag:           false,
			expectedError:          false,
		},
		{
			desc: "isFrontendIPChanged shall return true if the service is internal, subnet == nil and " +
				"loadBalancerIP != config.PrivateIPAddress",
			config: network.FrontendIPConfiguration{
				Name: pointer.String("btest1-name"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: network.Static,
					PrivateIPAddress:          pointer.String("1.1.1.2"),
				},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getInternalTestService("test1", 80),
			loadBalancerIP:         "1.1.1.1",
			expectedFlag:           true,
			expectedError:          false,
		},
		{
			desc: "isFrontendIPChanged shall return false if config.PublicIPAddress == nil",
			config: network.FrontendIPConfiguration{
				Name:                                    pointer.String("btest1-name"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getTestService("test1", v1.ProtocolTCP, nil, false, 80),
			loadBalancerIP:         "1.1.1.1",
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pipName"),
					ID:   pointer.String("pip"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						IPAddress: pointer.String("1.1.1.1"),
					},
				},
			},
			expectedFlag:  false,
			expectedError: false,
		},
		{
			desc: "isFrontendIPChanged shall return false if pip.ID == config.PublicIPAddress.ID",
			config: network.FrontendIPConfiguration{
				Name: pointer.String("btest1-name"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("/subscriptions/subscription" +
						"/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pipName")},
				},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getTestService("test1", v1.ProtocolTCP, nil, false, 80),
			loadBalancerIP:         "1.1.1.1",
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pipName"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						IPAddress: pointer.String("1.1.1.1"),
					},
					ID: pointer.String("/subscriptions/subscription" +
						"/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pipName"),
				},
			},
			expectedFlag:  false,
			expectedError: false,
		},
		{
			desc: "isFrontendIPChanged shall return true if pip.ID != config.PublicIPAddress.ID",
			config: network.FrontendIPConfiguration{
				Name: pointer.String("btest1-name"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &network.PublicIPAddress{
						ID: pointer.String("/subscriptions/subscription" +
							"/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pipName1"),
					},
				},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getTestService("test1", v1.ProtocolTCP, nil, false, 80),
			loadBalancerIP:         "1.1.1.1",
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pipName"),
					ID: pointer.String("/subscriptions/subscription" +
						"/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pipName2"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						IPAddress: pointer.String("1.1.1.1"),
					},
				},
			},
			expectedFlag:  true,
			expectedError: false,
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			mockSubnetsClient := az.SubnetsClient.(*mocksubnetclient.MockInterface)
			mockSubnetsClient.EXPECT().Get(gomock.Any(), "rg", "vnet", "testSubnet", "").Return(test.existingSubnet, nil).AnyTimes()
			mockSubnetsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "vnet", "testSubnet", test.existingSubnet).Return(nil)
			err := az.SubnetsClient.CreateOrUpdate(context.TODO(), "rg", "vnet", "testSubnet", test.existingSubnet)
			if err != nil {
				t.Fatal(err)
			}

			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			for _, existingPIP := range test.existingPIPs {
				err := az.PublicIPAddressesClient.CreateOrUpdate(context.TODO(), "rg", *existingPIP.Name, existingPIP)
				if err != nil {
					t.Fatal(err)
				}
			}
			mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(test.existingPIPs, nil).MaxTimes(2)
			service := test.service
			setServiceLoadBalancerIP(&service, test.loadBalancerIP)
			test.service.Annotations[consts.ServiceAnnotationLoadBalancerInternalSubnet] = test.annotations
			var subnet network.Subnet
			flag, rerr := az.isFrontendIPChanged("testCluster", test.config,
				&service, test.lbFrontendIPConfigName, &subnet)
			if rerr != nil {
				fmt.Println(rerr.Error())
			}
			assert.Equal(t, test.expectedFlag, flag)
			assert.Equal(t, test.expectedError, rerr != nil)
		})
	}
}

func TestFindMatchedPIPByLoadBalancerIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testPIP := network.PublicIPAddress{
		Name: pointer.String("pipName"),
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			IPAddress: pointer.String("1.2.3.4"),
		},
	}
	testCases := []struct {
		desc               string
		pips               []network.PublicIPAddress
		pipsSecondTime     []network.PublicIPAddress
		shouldRefreshCache bool
		expectedPIP        *network.PublicIPAddress
		expectedError      bool
	}{
		{
			desc:        "findMatchedPIPByLoadBalancerIP shall return the matched ip",
			pips:        []network.PublicIPAddress{testPIP},
			expectedPIP: &testPIP,
		},
		{
			desc:               "findMatchedPIPByLoadBalancerIP shall return error if ip is not found",
			pips:               []network.PublicIPAddress{},
			shouldRefreshCache: true,
			expectedError:      true,
		},
		{
			desc:               "findMatchedPIPByLoadBalancerIP should refresh cache if no matched ip is found",
			pipsSecondTime:     []network.PublicIPAddress{testPIP},
			shouldRefreshCache: true,
			expectedPIP:        &testPIP,
		},
	}
	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)

			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			if test.shouldRefreshCache {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(test.pipsSecondTime, nil)
			}
			pip, err := az.findMatchedPIPByLoadBalancerIP(&test.pips, "1.2.3.4", "rg")
			assert.Equal(t, test.expectedPIP, pip)
			assert.Equal(t, test.expectedError, err != nil)
		})
	}
}

func TestFindMatchedPIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testPIP := network.PublicIPAddress{
		Name: pointer.String("pipName"),
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			IPAddress: pointer.String("1.2.3.4"),
		},
	}

	for _, tc := range []struct {
		description         string
		pips                []network.PublicIPAddress
		pipsSecondTime      []network.PublicIPAddress
		pipName             string
		loadBalancerIP      string
		shouldRefreshCache  bool
		listError           *retry.Error
		listErrorSecondTime *retry.Error
		expectedPIP         *network.PublicIPAddress
		expectedError       error
	}{
		{
			description:        "should ignore pipName if loadBalancerIP is specified",
			pips:               []network.PublicIPAddress{testPIP},
			pipsSecondTime:     []network.PublicIPAddress{testPIP},
			shouldRefreshCache: true,
			loadBalancerIP:     "2.3.4.5",
			pipName:            "pipName",
			expectedError:      errors.New("findMatchedPIPByLoadBalancerIP: cannot find public IP with IP address 2.3.4.5 in resource group rg"),
		},
		{
			description:   "should report an error if failed to list pip",
			listError:     retry.NewError(false, errors.New("list error")),
			expectedError: errors.New("findMatchedPIPByLoadBalancerIP: failed to listPIP: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: list error"),
		},
		{
			description:        "should refresh the cache if failed to search by name",
			pips:               []network.PublicIPAddress{},
			pipsSecondTime:     []network.PublicIPAddress{testPIP},
			shouldRefreshCache: true,
			pipName:            "pipName",
			expectedPIP:        &testPIP,
		},
		{
			description: "should return the expected pip by name",
			pips:        []network.PublicIPAddress{testPIP},
			pipName:     "pipName",
			expectedPIP: &testPIP,
		},
		{
			description:         "should report an error if failed to list pip second time",
			pips:                []network.PublicIPAddress{},
			listErrorSecondTime: retry.NewError(false, errors.New("list error")),
			shouldRefreshCache:  true,
			expectedError:       errors.New("findMatchedPIPByName: failed to listPIP force refresh: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: list error"),
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(tc.pips, tc.listError)
			if tc.shouldRefreshCache {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(tc.pipsSecondTime, tc.listErrorSecondTime)
			}

			pip, err := az.findMatchedPIP(tc.loadBalancerIP, tc.pipName, "rg")
			assert.Equal(t, tc.expectedPIP, pip)
			if tc.expectedError != nil {
				assert.Equal(t, tc.expectedError.Error(), err.Error())
			}
		})
	}
}

func TestDeterminePublicIPName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc            string
		loadBalancerIP  string
		existingPIPs    []network.PublicIPAddress
		expectedPIPName string
		expectedError   bool
		isIPv6          bool
	}{
		{
			desc: "determinePublicIpName shall get public IP from az.getPublicIPName if no specific " +
				"loadBalancerIP is given",
			expectedPIPName: "testCluster-atest1",
			expectedError:   false,
		},
		{
			desc:            "determinePublicIpName shall report error if loadBalancerIP is not in the resource group",
			loadBalancerIP:  "1.2.3.4",
			expectedPIPName: "",
			expectedError:   true,
		},
		{
			desc: "determinePublicIpName shall return loadBalancerIP in service.Spec if it's in the " +
				"resource group",
			loadBalancerIP: "1.2.3.4",
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pipName"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						IPAddress: pointer.String("1.2.3.4"),
					},
				},
			},
			expectedPIPName: "pipName",
			expectedError:   false,
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			service := getTestService("test1", v1.ProtocolTCP, nil, false, 80)
			setServiceLoadBalancerIP(&service, test.loadBalancerIP)

			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(test.existingPIPs, nil).MaxTimes(2)
			mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			for _, existingPIP := range test.existingPIPs {
				mockPIPsClient.EXPECT().Get(gomock.Any(), "rg", *existingPIP.Name, gomock.Any()).Return(existingPIP, nil).AnyTimes()
				err := az.PublicIPAddressesClient.CreateOrUpdate(context.TODO(), "rg", *existingPIP.Name, existingPIP)
				assert.NoError(t, err.Error())
			}
			pipName, _, err := az.determinePublicIPName("testCluster", &service, test.isIPv6)
			assert.Equal(t, test.expectedPIPName, pipName)
			assert.Equal(t, test.expectedError, err != nil)
		})
	}
}

func TestReconcileLoadBalancerRuleCommon(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc            string
		service         v1.Service
		loadBalancerSku string
		probeProtocol   string
		probePath       string
		expectedProbes  map[bool][]network.Probe
		expectedRules   map[bool][]network.LoadBalancingRule
		expectedErr     bool
	}{
		{
			desc:            "getExpectedLBRules shall return corresponding probe and lbRule(blb)",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{}, 80),
			loadBalancerSku: "basic",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules:   getDefaultTestRules(false),
		},
		{
			desc:            "getExpectedLBRules shall return tcp probe on non supported protocols when basic lb sku is used",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{}, 80),
			loadBalancerSku: "basic",
			probeProtocol:   "Mongodb",
			expectedRules:   getDefaultTestRules(false),
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
		},
		{
			desc:            "getExpectedLBRules shall return tcp probe on https protocols when basic lb sku is used",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{}, 80),
			loadBalancerSku: "basic",
			probeProtocol:   "Https",
			expectedRules:   getDefaultTestRules(false),
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
		},
		{
			desc:            "getExpectedLBRules shall return error (slb with external mode and SCTP)",
			service:         getTestServiceDualStack("test1", v1.ProtocolSCTP, map[string]string{}, 80),
			loadBalancerSku: "standard",
			expectedErr:     true,
		},
		{
			desc:            "getExpectedLBRules shall return corresponding probe and lbRule(slb with tcp reset)",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			loadBalancerSku: "standard",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc:            "getExpectedLBRules shall respect the probe protocol and path configuration in the config file",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Http",
			probePath:       "/healthy",
			expectedProbes:  getDefaultTestProbes("Http", "/healthy"),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc:            "getExpectedLBRules shall respect the probe protocol and path configuration in the config file",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Https",
			probePath:       "/healthy1",
			expectedProbes:  getDefaultTestProbes("Https", "/healthy1"),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules shall return corresponding probe and lbRule (slb with IPv6)",
			service: getTestService("test1", v1.ProtocolTCP, map[string]string{
				consts.ServiceAnnotationLoadBalancerInternal: "true",
			}, true, 80),
			loadBalancerSku: "standard",
			expectedProbes: map[bool][]network.Probe{
				// Use false as IPv6 param but it is a IPv6 probe.
				true: {getTestProbe("Tcp", "", pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2), false)},
			},
			expectedRules: getDefaultInternalIPv6Rules(true),
		},
		{
			desc: "getExpectedLBRules shall return corresponding probe and lbRule (slb with HA enabled)",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "true",
				consts.ServiceAnnotationLoadBalancerInternal:                    "true",
			}, 80),
			loadBalancerSku: "standard",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules: map[bool][]network.LoadBalancingRule{
				consts.IPVersionIPv4: getHATestRules(true, true, v1.ProtocolTCP, consts.IPVersionIPv4, true),
				consts.IPVersionIPv6: getHATestRules(true, true, v1.ProtocolTCP, consts.IPVersionIPv6, true),
			},
		},
		{
			desc: "getExpectedLBRules shall return corresponding probe and lbRule (slb with HA mode and SCTP)",
			service: getTestServiceDualStack("test1", v1.ProtocolSCTP, map[string]string{
				consts.ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "true",
				consts.ServiceAnnotationLoadBalancerInternal:                    "true",
			}, 80),
			loadBalancerSku: "standard",
			expectedRules: map[bool][]network.LoadBalancingRule{
				consts.IPVersionIPv4: getHATestRules(true, false, v1.ProtocolSCTP, consts.IPVersionIPv4, true),
				consts.IPVersionIPv6: getHATestRules(true, false, v1.ProtocolSCTP, consts.IPVersionIPv6, true),
			},
		},
		{
			desc: "getExpectedLBRules shall return corresponding probe and lbRule (slb with HA enabled multi-ports services)",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "true",
				consts.ServiceAnnotationLoadBalancerInternal:                    "true",
			}, 80, 8080),
			loadBalancerSku: "standard",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules: map[bool][]network.LoadBalancingRule{
				consts.IPVersionIPv4: getHATestRules(true, true, v1.ProtocolTCP, consts.IPVersionIPv4, true),
				consts.IPVersionIPv6: getHATestRules(true, true, v1.ProtocolTCP, consts.IPVersionIPv6, true),
			},
		},
		{
			desc:            "getExpectedLBRules should leave probe path empty when using TCP probe",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Tcp",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return tcp probe when invalid protocol is defined",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy1",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "TCP1",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return tcp probe when invalid protocol is defined",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy1",
			}, 80),
			loadBalancerSku: "basic",
			probeProtocol:   "TCP1",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules:   getDefaultTestRules(false),
		},
		{
			desc: "getExpectedLBRules should return correct rule when deprecated annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy1",
				consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                              "https",
			}, 80),
			loadBalancerSku: "standard",
			expectedProbes:  getDefaultTestProbes("Https", "/healthy1"),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return correct rule when deprecated annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy1",
				consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                              "http",
			}, 80),
			loadBalancerSku: "standard",
			expectedProbes:  getDefaultTestProbes("Http", "/healthy1"),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return correct rule when deprecated annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy1",
				consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                              "tcp",
			}, 80),
			loadBalancerSku: "standard",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return correct rule when deprecated annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy1",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Https",
			expectedProbes:  getDefaultTestProbes("Https", "/healthy1"),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should overwrite value defined in deprecated annotation when deprecated annotations and probe path are defined",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath:                           "/healthy1",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy2",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Https",
			expectedProbes:  getDefaultTestProbes("Https", "/healthy2"),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return error when probe interval * num > 120",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "10",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "20",
				consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                                "https",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Tcp",
			expectedErr:     true,
		},
		{
			desc: "getExpectedLBRules should return error when probe interval * num ==  120",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "10",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "20",
				consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                                "tcp",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Tcp",
			expectedErr:     true,
		},
		{
			desc: "getExpectedLBRules should return correct rule when health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "20",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "5",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Https",
			probePath:       "/healthy1",
			expectedProbes:  getTestProbes("Https", "/healthy1", pointer.Int32(20), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(5)),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return correct rule when health probe annotations are added,default path should be /",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "20",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "5",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Http",
			expectedProbes:  getTestProbes("Http", "/", pointer.Int32(20), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(5)),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return correct rule when tcp health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "20",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "5",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Tcp",
			expectedProbes:  getTestProbes("Tcp", "", pointer.Int32(20), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(5)),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return error when invalid tcp health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "20",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "5a",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Tcp",
			expectedErr:     true,
		},
		{
			desc: "getExpectedLBRules should return error when invalid tcp health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "1",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "5",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Tcp",
			expectedErr:     true,
		},
		{
			desc: "getExpectedLBRules should return error when invalid tcp health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "10",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "1",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Tcp",
			expectedErr:     true,
		},
		{
			desc: "getExpectedLBRules should return error when invalid tcp health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "10",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "20",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Tcp",
			expectedErr:     true,
		},
		{
			desc:            "getExpectedLBRules should return correct rule when floating ip annotations are added",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true"}, 80),
			loadBalancerSku: "basic",
			expectedRules: map[bool][]network.LoadBalancingRule{
				consts.IPVersionIPv4: {getFloatingIPTestRule(false, false, 80, consts.IPVersionIPv4)},
				consts.IPVersionIPv6: {getFloatingIPTestRule(false, false, 80, consts.IPVersionIPv6)},
			},
			expectedProbes: getDefaultTestProbes("Tcp", ""),
		},
		{
			desc: "getExpectedLBRules should prioritize port specific probe protocol over defaults",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_80_health-probe_protocol": "HtTp",
			}, 80),
			expectedRules:  getDefaultTestRules(false),
			expectedProbes: getDefaultTestProbes("Http", "/"),
		},
		{
			desc: "getExpectedLBRules should disable tcp reset when annotation is set",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/azure-load-balancer-disable-tcp-reset": "true",
			}, 80),
			loadBalancerSku: "standard",
			expectedRules:   getTCPResetTestRules(false),
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
		},
		{
			desc: "getExpectedLBRules should prioritize port specific probe protocol over appProtocol",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_80_health-probe_protocol": "HtTp",
			}, 80),
			probeProtocol:  "Mongodb",
			expectedRules:  getDefaultTestRules(false),
			expectedProbes: getDefaultTestProbes("Http", "/"),
		},
		{
			desc: "getExpectedLBRules should prioritize port specific probe protocol over deprecated annotation",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_80_health-probe_protocol":             "HtTpS",
				"service.beta.kubernetes.io/azure-load-balancer-health-probe-protocol": "TcP",
			}, 80),
			loadBalancerSku: "standard",
			probeProtocol:   "Https",
			expectedRules:   getDefaultTestRules(true),
			expectedProbes:  getDefaultTestProbes("Https", "/"),
		},
		{
			desc: "getExpectedLBRules should default to Tcp on invalid port specific probe protocol",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_80_health-probe_protocol": "FooBar",
			}, 80),
			probeProtocol:  "Http",
			expectedRules:  getDefaultTestRules(false),
			expectedProbes: getDefaultTestProbes("Tcp", ""),
		},
		{
			desc: "getExpectedLBRules should support customize health probe port in multi-port service",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_8000_health-probe_port": "port-tcp-80",
			}, 80, 8000),
			expectedRules: map[bool][]network.LoadBalancingRule{
				consts.IPVersionIPv4: {getTestRule(false, 80, consts.IPVersionIPv4), getTestRule(false, 8000, consts.IPVersionIPv4)},
				consts.IPVersionIPv6: {getTestRule(false, 80, consts.IPVersionIPv6), getTestRule(false, 8000, consts.IPVersionIPv6)},
			},
			expectedProbes: map[bool][]network.Probe{
				consts.IPVersionIPv4: {
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv4),
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(8000), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv6),
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(8000), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv6),
				},
			},
		},
		{
			desc: "getExpectedLBRules should support customize health probe port in multi-port service",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_8000_health-probe_port": "80",
			}, 80, 8000),
			expectedRules: map[bool][]network.LoadBalancingRule{
				consts.IPVersionIPv4: {
					getTestRule(false, 80, consts.IPVersionIPv4),
					getTestRule(false, 8000, consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestRule(false, 80, consts.IPVersionIPv6),
					getTestRule(false, 8000, consts.IPVersionIPv6),
				},
			},
			expectedProbes: map[bool][]network.Probe{
				consts.IPVersionIPv4: {
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv4),
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(8000), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv6),
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(8000), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv6),
				},
			},
		},
		{
			desc: "getExpectedLBRules should not generate probe rule when no health probe rule is specified.",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_8000_no_probe_rule": "true",
			}, 80, 8000),
			expectedRules: map[bool][]network.LoadBalancingRule{
				consts.IPVersionIPv4: {
					getTestRule(false, 80, consts.IPVersionIPv4),
					func() network.LoadBalancingRule {
						rule := getTestRule(false, 8000, consts.IPVersionIPv4)
						rule.Probe = nil
						return rule
					}(),
				},
				consts.IPVersionIPv6: {
					getTestRule(false, 80, consts.IPVersionIPv6),
					func() network.LoadBalancingRule {
						rule := getTestRule(false, 8000, consts.IPVersionIPv6)
						rule.Probe = nil
						return rule
					}(),
				},
			},
			expectedProbes: map[bool][]network.Probe{
				consts.IPVersionIPv4: {
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv6),
				},
			},
		},
		{
			desc: "getExpectedLBRules should not generate lb rule and health probe rule when no lb rule is specified.",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_8000_no_lb_rule": "true",
			}, 80, 8000),
			expectedRules: map[bool][]network.LoadBalancingRule{
				consts.IPVersionIPv4: {
					getTestRule(false, 80, consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestRule(false, 80, consts.IPVersionIPv6),
				},
			},
			expectedProbes: map[bool][]network.Probe{
				consts.IPVersionIPv4: {
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv6),
				},
			},
		},
		{
			desc: "getExpectedLBRules should support customize health probe port ",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_8000_health-probe_port": "5080",
			}, 80, 8000),
			expectedRules: map[bool][]network.LoadBalancingRule{
				consts.IPVersionIPv4: {
					getTestRule(false, 80, consts.IPVersionIPv4),
					getTestRule(false, 8000, consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestRule(false, 80, consts.IPVersionIPv6),
					getTestRule(false, 8000, consts.IPVersionIPv6),
				},
			},
			expectedProbes: map[bool][]network.Probe{
				consts.IPVersionIPv4: {
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv4),
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(8000), pointer.Int32(5080), pointer.Int32(2), consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(2), consts.IPVersionIPv6),
					getTestProbe("Tcp", "/", pointer.Int32(5), pointer.Int32(8000), pointer.Int32(5080), pointer.Int32(2), consts.IPVersionIPv6),
				},
			},
		},
	}
	rulesDualStack := getDefaultTestRules(true)
	for _, rules := range rulesDualStack {
		for _, rule := range rules {
			rule.IdleTimeoutInMinutes = pointer.Int32(5)
		}
	}
	testCases = append(testCases, struct {
		desc            string
		service         v1.Service
		loadBalancerSku string
		probeProtocol   string
		probePath       string
		expectedProbes  map[bool][]network.Probe
		expectedRules   map[bool][]network.LoadBalancingRule
		expectedErr     bool
	}{
		desc: "getExpectedLBRules should expected rules when timeout are added",
		service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
			consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "10",
			consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "10",
			consts.ServiceAnnotationLoadBalancerIdleTimeout:                                        "5",
		}, 80),
		loadBalancerSku: "standard",
		probeProtocol:   "Tcp",
		expectedProbes:  getTestProbes("Tcp", "", pointer.Int32(10), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(10)),
		expectedRules:   rulesDualStack,
	})
	rules1DualStack := map[bool][]network.LoadBalancingRule{
		consts.IPVersionIPv4: {
			getTestRule(true, 80, consts.IPVersionIPv4),
			getTestRule(true, 443, consts.IPVersionIPv4),
			getTestRule(true, 421, consts.IPVersionIPv4),
		},
		consts.IPVersionIPv6: {
			getTestRule(true, 80, consts.IPVersionIPv6),
			getTestRule(true, 443, consts.IPVersionIPv6),
			getTestRule(true, 421, consts.IPVersionIPv6),
		},
	}
	for _, rule := range rules1DualStack[consts.IPVersionIPv4] {
		rule.Probe.ID = pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lbname/probes/atest1-TCP-34567")
	}
	for _, rule := range rules1DualStack[consts.IPVersionIPv6] {
		rule.Probe.ID = pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lbname/probes/atest1-TCP-34567-IPv6")
	}

	// When the service spec externalTrafficPolicy is Local all of these annotations should be ignored
	svc := getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
		consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                                "tcp",
		consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath:                             "/broken/global/path",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "10",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProtocol):      "https",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath):   "/broken/local/path",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "10",
	}, 80, 443, 421)
	svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	svc.Spec.HealthCheckNodePort = 34567
	probes := getTestProbes("Http", "/healthz", pointer.Int32(5), pointer.Int32(34567), pointer.Int32(34567), pointer.Int32(2))
	testCases = append(testCases, struct {
		desc            string
		service         v1.Service
		loadBalancerSku string
		probeProtocol   string
		probePath       string
		expectedProbes  map[bool][]network.Probe
		expectedRules   map[bool][]network.LoadBalancingRule
		expectedErr     bool
	}{
		desc:            "getExpectedLBRules should expected rules when externalTrafficPolicy is local",
		service:         svc,
		loadBalancerSku: "standard",
		probeProtocol:   "Http",
		expectedProbes:  probes,
		expectedRules:   rules1DualStack,
	})
	rules1DualStack = map[bool][]network.LoadBalancingRule{
		consts.IPVersionIPv4: {
			getTestRule(true, 80, consts.IPVersionIPv4),
		},
		consts.IPVersionIPv6: {
			getTestRule(true, 80, consts.IPVersionIPv6),
		},
	}
	// When the service spec externalTrafficPolicy is Local and azure-disable-service-health-port-probe is set, should return default
	svc = getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
		consts.ServiceAnnotationPLSCreation:                                                    "true",
		consts.ServiceAnnotationPLSProxyProtocol:                                               "true",
		consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                                "tcp",
		consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath:                             "/broken/global/path",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "7",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProtocol):      "https",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath):   "/broken/local/path",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "15",
	}, 80)
	svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	svc.Spec.HealthCheckNodePort = 34567
	probes = getTestProbes("Https", "/broken/local/path", pointer.Int32(7), pointer.Int32(80), pointer.Int32(10080), pointer.Int32(15))
	testCases = append(testCases, struct {
		desc            string
		service         v1.Service
		loadBalancerSku string
		probeProtocol   string
		probePath       string
		expectedProbes  map[bool][]network.Probe
		expectedRules   map[bool][]network.LoadBalancingRule
		expectedErr     bool
	}{
		desc:            "getExpectedLBRules should return expected rules when externalTrafficPolicy is local and service.beta.kubernetes.io/azure-pls-proxy-protocol is enabled",
		service:         svc,
		loadBalancerSku: "standard",
		probeProtocol:   "https",
		expectedProbes:  probes,
		expectedRules:   rules1DualStack,
	})
	// ETP is local and port is specified in annotation
	svc = getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
		consts.ServiceAnnotationPLSCreation:                                                    "true",
		consts.ServiceAnnotationPLSProxyProtocol:                                               "true",
		consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                                "tcp",
		consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath:                             "/broken/global/path",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "7",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProtocol):      "https",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath):   "/broken/local/path",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "15",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsPort):          "421",
	}, 80)
	svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	svc.Spec.HealthCheckNodePort = 34567
	probes = getTestProbes("Https", "/broken/local/path", pointer.Int32(7), pointer.Int32(80), pointer.Int32(421), pointer.Int32(15))
	testCases = append(testCases, struct {
		desc            string
		service         v1.Service
		loadBalancerSku string
		probeProtocol   string
		probePath       string
		expectedProbes  map[bool][]network.Probe
		expectedRules   map[bool][]network.LoadBalancingRule
		expectedErr     bool
	}{
		desc:            "getExpectedLBRules should return expected rules when externalTrafficPolicy is local and service.beta.kubernetes.io/azure-pls-proxy-protocol is enabled",
		service:         svc,
		loadBalancerSku: "standard",
		probeProtocol:   "https",
		expectedProbes:  probes,
		expectedRules:   rules1DualStack,
	})
	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			az.Config.LoadBalancerSku = test.loadBalancerSku
			service := test.service
			firstPort := service.Spec.Ports[0]
			probeProtocol := test.probeProtocol
			if test.probeProtocol != "" {
				service.Spec.Ports[0].AppProtocol = &probeProtocol
			}
			if test.probePath != "" {
				service.Annotations[consts.BuildHealthProbeAnnotationKeyForPort(firstPort.Port, consts.HealthProbeParamsRequestPath)] = test.probePath
			}
			v4Enabled, v6Enabled := getIPFamiliesEnabled(&service)
			if v4Enabled {
				probe, lbrule, err := az.getExpectedLBRules(&service,
					"frontendIPConfigID", "backendPoolID", "lbname", consts.IPVersionIPv4)
				if test.expectedErr {
					assert.Error(t, err)
				} else {
					assert.Equal(t, test.expectedProbes[consts.IPVersionIPv4], probe)
					assert.Equal(t, test.expectedRules[consts.IPVersionIPv4], lbrule)
					assert.NoError(t, err)
				}
			}
			isDualStack := v4Enabled && v6Enabled
			if v6Enabled {
				lbFrontendIPConfigID, lbBackendPoolID := "frontendIPConfigID", "backendPoolID-IPv6"
				if isDualStack {
					lbFrontendIPConfigID = "frontendIPConfigID-IPv6"
				}
				probe, lbrule, err := az.getExpectedLBRules(&service,
					lbFrontendIPConfigID, lbBackendPoolID, "lbname", consts.IPVersionIPv6)
				if test.expectedErr {
					assert.Error(t, err)
				} else {
					assert.Equal(t, test.expectedProbes[consts.IPVersionIPv6], probe)
					assert.Equal(t, test.expectedRules[consts.IPVersionIPv6], lbrule)
					assert.NoError(t, err)
				}
			}
		})
	}
}

// getDefaultTestRules returns dualstack rules.
func getDefaultTestRules(enableTCPReset bool) map[bool][]network.LoadBalancingRule {
	return map[bool][]network.LoadBalancingRule{
		consts.IPVersionIPv4: {getTestRule(enableTCPReset, 80, consts.IPVersionIPv4)},
		consts.IPVersionIPv6: {getTestRule(enableTCPReset, 80, consts.IPVersionIPv6)},
	}
}

// getDefaultInternalIPv6Rules returns a rule for IPv6 single stack.
func getDefaultInternalIPv6Rules(enableTCPReset bool) map[bool][]network.LoadBalancingRule {
	rule := getTestRule(enableTCPReset, 80, false)
	rule.EnableFloatingIP = pointer.Bool(false)
	rule.BackendPort = pointer.Int32(getBackendPort(*rule.FrontendPort))
	rule.BackendAddressPool.ID = pointer.String("backendPoolID-IPv6")
	return map[bool][]network.LoadBalancingRule{
		true: {rule},
	}
}

// getTCPResetTestRules returns rules with TCPReset always set.
func getTCPResetTestRules(enableTCPReset bool) map[bool][]network.LoadBalancingRule {
	IPv4Rule := getTestRule(enableTCPReset, 80, consts.IPVersionIPv4)
	IPv6Rule := getTestRule(enableTCPReset, 80, consts.IPVersionIPv6)
	IPv4Rule.EnableTCPReset = pointer.Bool(enableTCPReset)
	IPv6Rule.EnableTCPReset = pointer.Bool(enableTCPReset)
	return map[bool][]network.LoadBalancingRule{
		consts.IPVersionIPv4: {IPv4Rule},
		consts.IPVersionIPv6: {IPv6Rule},
	}
}

// getTestRule returns a rule for dualStack.
func getTestRule(enableTCPReset bool, port int32, isIPv6 bool) network.LoadBalancingRule {
	suffix := ""
	if isIPv6 {
		suffix = "-" + v6Suffix
	}
	expectedRules := network.LoadBalancingRule{
		Name: pointer.String(fmt.Sprintf("atest1-TCP-%d", port) + suffix),
		LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
			Protocol: network.TransportProtocol("Tcp"),
			FrontendIPConfiguration: &network.SubResource{
				ID: pointer.String("frontendIPConfigID" + suffix),
			},
			BackendAddressPool: &network.SubResource{
				ID: pointer.String("backendPoolID" + suffix),
			},
			LoadDistribution:     "Default",
			FrontendPort:         pointer.Int32(port),
			BackendPort:          pointer.Int32(port),
			EnableFloatingIP:     pointer.Bool(true),
			DisableOutboundSnat:  pointer.Bool(false),
			IdleTimeoutInMinutes: pointer.Int32(4),
			Probe: &network.SubResource{
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/" +
					fmt.Sprintf("Microsoft.Network/loadBalancers/lbname/probes/atest1-TCP-%d%s", port, suffix)),
			},
		},
	}
	if enableTCPReset {
		expectedRules.EnableTCPReset = pointer.Bool(true)
	}
	return expectedRules
}

func getHATestRules(enableTCPReset, hasProbe bool, protocol v1.Protocol, isIPv6, isInternal bool) []network.LoadBalancingRule {
	suffix := ""
	enableFloatingIP := true
	if isIPv6 {
		suffix = "-" + v6Suffix
		if isInternal {
			enableFloatingIP = false
		}
	}

	expectedRules := []network.LoadBalancingRule{
		{
			Name: pointer.String(fmt.Sprintf("atest1-%s-80%s", string(protocol), suffix)),
			LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
				Protocol: network.TransportProtocol("All"),
				FrontendIPConfiguration: &network.SubResource{
					ID: pointer.String("frontendIPConfigID" + suffix),
				},
				BackendAddressPool: &network.SubResource{
					ID: pointer.String("backendPoolID" + suffix),
				},
				LoadDistribution:     "Default",
				FrontendPort:         pointer.Int32(0),
				BackendPort:          pointer.Int32(0),
				EnableFloatingIP:     pointer.Bool(enableFloatingIP),
				DisableOutboundSnat:  pointer.Bool(false),
				IdleTimeoutInMinutes: pointer.Int32(4),
				EnableTCPReset:       pointer.Bool(true),
			},
		},
	}
	if hasProbe {
		expectedRules[0].Probe = &network.SubResource{
			ID: pointer.String(fmt.Sprintf("/subscriptions/subscription/resourceGroups/rg/providers/"+
				"Microsoft.Network/loadBalancers/lbname/probes/atest1-%s-80%s", string(protocol), suffix)),
		}
	}
	return expectedRules
}

func getFloatingIPTestRule(enableTCPReset, enableFloatingIP bool, port int32, isIPv6 bool) network.LoadBalancingRule {
	suffix := ""
	if isIPv6 {
		suffix = "-" + v6Suffix
	}
	expectedRules := network.LoadBalancingRule{
		Name: pointer.String(fmt.Sprintf("atest1-TCP-%d%s", port, suffix)),
		LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
			Protocol: network.TransportProtocol("Tcp"),
			FrontendIPConfiguration: &network.SubResource{
				ID: pointer.String("frontendIPConfigID" + suffix),
			},
			BackendAddressPool: &network.SubResource{
				ID: pointer.String("backendPoolID" + suffix),
			},
			LoadDistribution:     "Default",
			FrontendPort:         pointer.Int32(port),
			BackendPort:          pointer.Int32(getBackendPort(port)),
			EnableFloatingIP:     pointer.Bool(enableFloatingIP),
			DisableOutboundSnat:  pointer.Bool(false),
			IdleTimeoutInMinutes: pointer.Int32(4),
			Probe: &network.SubResource{
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/" +
					fmt.Sprintf("Microsoft.Network/loadBalancers/lbname/probes/atest1-TCP-%d%s", port, suffix)),
			},
		},
	}
	if enableTCPReset {
		expectedRules.EnableTCPReset = pointer.Bool(true)
	}
	return expectedRules
}

func getTestLoadBalancer(name, rgName, clusterName, identifier *string, service v1.Service, lbSku string) network.LoadBalancer {
	caser := cases.Title(language.English)
	lb := network.LoadBalancer{
		Name: name,
		Sku: &network.LoadBalancerSku{
			Name: network.LoadBalancerSkuName(lbSku),
		},
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: &[]network.FrontendIPConfiguration{
				{
					Name: identifier,
					ID: pointer.String("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
						"Microsoft.Network/loadBalancers/" + *name + "/frontendIPConfigurations/" + *identifier),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
					},
				},
			},
			BackendAddressPools: &[]network.BackendAddressPool{
				{Name: clusterName},
			},
			Probes: &[]network.Probe{
				{
					Name: pointer.String(*identifier + "-" + string(service.Spec.Ports[0].Protocol) +
						"-" + strconv.Itoa(int(service.Spec.Ports[0].Port))),
					ProbePropertiesFormat: &network.ProbePropertiesFormat{
						Port:              pointer.Int32(10080),
						Protocol:          network.ProbeProtocolTCP,
						IntervalInSeconds: pointer.Int32(5),
						ProbeThreshold:    pointer.Int32(2),
					},
				},
			},
			LoadBalancingRules: &[]network.LoadBalancingRule{
				{
					Name: pointer.String(*identifier + "-" + string(service.Spec.Ports[0].Protocol) +
						"-" + strconv.Itoa(int(service.Spec.Ports[0].Port))),
					LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
						Protocol: network.TransportProtocol(caser.String((strings.ToLower(string(service.Spec.Ports[0].Protocol))))),
						FrontendIPConfiguration: &network.SubResource{
							ID: pointer.String("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
								"Microsoft.Network/loadBalancers/" + *name + "/frontendIPConfigurations/aservice1"),
						},
						BackendAddressPool: &network.SubResource{
							ID: pointer.String("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
								"Microsoft.Network/loadBalancers/" + *name + "/backendAddressPools/" + *clusterName),
						},
						LoadDistribution:     network.LoadDistribution("Default"),
						FrontendPort:         pointer.Int32(service.Spec.Ports[0].Port),
						BackendPort:          pointer.Int32(service.Spec.Ports[0].Port),
						EnableFloatingIP:     pointer.Bool(true),
						EnableTCPReset:       pointer.Bool(strings.EqualFold(lbSku, "standard")),
						DisableOutboundSnat:  pointer.Bool(false),
						IdleTimeoutInMinutes: pointer.Int32(4),
						Probe: &network.SubResource{
							ID: pointer.String("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/Microsoft.Network/loadBalancers/testCluster/probes/aservice1-TCP-80"),
						},
					},
				},
			},
		},
	}
	return lb
}

func getTestLoadBalancerDualStack(name, rgName, clusterName, identifier *string, service v1.Service, lbSku string) network.LoadBalancer {
	caser := cases.Title(language.English)
	lb := getTestLoadBalancer(name, rgName, clusterName, identifier, service, lbSku)
	*lb.LoadBalancerPropertiesFormat.FrontendIPConfigurations = append(*lb.LoadBalancerPropertiesFormat.FrontendIPConfigurations, network.FrontendIPConfiguration{
		Name: pointer.String(*identifier + "-IPv6"),
		ID: pointer.String("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
			"Microsoft.Network/loadBalancers/" + *name + "/frontendIPConfigurations/" + *identifier + "-IPv6"),
		FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
			PublicIPAddress: &network.PublicIPAddress{
				ID: pointer.String("testCluster-aservice1-IPv6"),
			},
		},
	})
	*lb.LoadBalancerPropertiesFormat.BackendAddressPools = append(*lb.LoadBalancerPropertiesFormat.BackendAddressPools, network.BackendAddressPool{
		Name: pointer.String(*clusterName + "-IPv6"),
	})
	*lb.LoadBalancerPropertiesFormat.Probes = append(*lb.LoadBalancerPropertiesFormat.Probes, network.Probe{
		Name: pointer.String(*identifier + "-" + string(service.Spec.Ports[0].Protocol) +
			"-" + strconv.Itoa(int(service.Spec.Ports[0].Port)) + "-IPv6"),
		ProbePropertiesFormat: &network.ProbePropertiesFormat{
			Port:              pointer.Int32(10080),
			Protocol:          network.ProbeProtocolTCP,
			IntervalInSeconds: pointer.Int32(5),
			ProbeThreshold:    pointer.Int32(2),
		},
	})
	*lb.LoadBalancerPropertiesFormat.LoadBalancingRules = append(*lb.LoadBalancerPropertiesFormat.LoadBalancingRules, network.LoadBalancingRule{
		Name: pointer.String(*identifier + "-" + string(service.Spec.Ports[0].Protocol) +
			"-" + strconv.Itoa(int(service.Spec.Ports[0].Port)) + "-IPv6"),
		LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
			Protocol: network.TransportProtocol(caser.String((strings.ToLower(string(service.Spec.Ports[0].Protocol))))),
			FrontendIPConfiguration: &network.SubResource{
				ID: pointer.String("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
					"Microsoft.Network/loadBalancers/" + *name + "/frontendIPConfigurations/aservice1-IPv6"),
			},
			BackendAddressPool: &network.SubResource{
				ID: pointer.String("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
					"Microsoft.Network/loadBalancers/" + *name + "/backendAddressPools/" + *clusterName + "-IPv6"),
			},
			LoadDistribution:     network.LoadDistribution("Default"),
			FrontendPort:         pointer.Int32(service.Spec.Ports[0].Port),
			BackendPort:          pointer.Int32(service.Spec.Ports[0].Port),
			EnableFloatingIP:     pointer.Bool(true),
			EnableTCPReset:       pointer.Bool(strings.EqualFold(lbSku, "standard")),
			DisableOutboundSnat:  pointer.Bool(false),
			IdleTimeoutInMinutes: pointer.Int32(4),
			Probe: &network.SubResource{
				ID: pointer.String("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/Microsoft.Network/loadBalancers/testCluster/probes/aservice1-TCP-80-IPv6"),
			},
		},
	})
	return lb
}

func TestReconcileLoadBalancerCommon(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	service1 := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)
	basicLb1 := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("aservice1"), service1, "Basic")

	service2 := getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80)
	basicLb2 := getTestLoadBalancerDualStack(pointer.String("lb1"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("bservice1"), service2, "Basic")
	basicLb2.Name = pointer.String("testCluster")
	basicLb2.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("bservice1"),
			ID:   pointer.String("bservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1")},
			},
		},
		{
			Name: pointer.String("bservice1-IPv6"),
			ID:   pointer.String("bservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1-IPv6")},
			},
		},
	}

	service3 := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)
	modifiedLbs := make([]network.LoadBalancer, 2)
	for i := range modifiedLbs {
		modifiedLbs[i] = getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("aservice1"), service3, "Basic")
		modifiedLbs[i].FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
			{
				Name: pointer.String("aservice1"),
				ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
				},
			},
			{
				Name: pointer.String("bservice1"),
				ID:   pointer.String("bservice1"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1")},
				},
			},
			{
				Name: pointer.String("aservice1-IPv6"),
				ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1-IPv6")},
				},
			},
			{
				Name: pointer.String("bservice1-IPv6"),
				ID:   pointer.String("bservice1-IPv6"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1-IPv6")},
				},
			},
		}
		modifiedLbs[i].Probes = &[]network.Probe{
			{
				Name: pointer.String("aservice1-" + string(service3.Spec.Ports[0].Protocol) +
					"-" + strconv.Itoa(int(service3.Spec.Ports[0].Port))),
				ProbePropertiesFormat: &network.ProbePropertiesFormat{
					Port: pointer.Int32(10080),
				},
			},
			{
				Name: pointer.String("aservice1-" + string(service3.Spec.Ports[0].Protocol) +
					"-" + strconv.Itoa(int(service3.Spec.Ports[0].Port))),
				ProbePropertiesFormat: &network.ProbePropertiesFormat{
					Port: pointer.Int32(10081),
				},
			},
			{
				Name: pointer.String("aservice1-" + string(service3.Spec.Ports[0].Protocol) +
					"-" + strconv.Itoa(int(service3.Spec.Ports[0].Port)) + "-IPv6"),
				ProbePropertiesFormat: &network.ProbePropertiesFormat{
					Port: pointer.Int32(10080),
				},
			},
			{
				Name: pointer.String("aservice1-" + string(service3.Spec.Ports[0].Protocol) +
					"-" + strconv.Itoa(int(service3.Spec.Ports[0].Port)) + "-IPv6"),
				ProbePropertiesFormat: &network.ProbePropertiesFormat{
					Port: pointer.Int32(10081),
				},
			},
		}
	}
	expectedLb1 := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("aservice1"), service3, "Basic")
	expectedLb1.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
			},
		},
		{
			Name: pointer.String("bservice1"),
			ID:   pointer.String("bservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1")},
			},
		},
		{
			Name: pointer.String("aservice1-IPv6"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1-IPv6")},
			},
		},
		{
			Name: pointer.String("bservice1-IPv6"),
			ID:   pointer.String("bservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1-IPv6")},
			},
		},
	}

	service4 := getTestServiceDualStack("service1", v1.ProtocolTCP, map[string]string{}, 80)
	existingSLB := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("aservice1"), service4, "Standard")
	existingSLB.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
			},
		},
		{
			Name: pointer.String("bservice1"),
			ID:   pointer.String("bservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1")},
			},
		},
		{
			Name: pointer.String("aservice1-IPv6"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1-IPv6")},
			},
		},
		{
			Name: pointer.String("bservice1-IPv6"),
			ID:   pointer.String("bservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1-IPv6")},
			},
		},
	}
	existingSLB.Probes = &[]network.Probe{
		{
			Name: pointer.String("aservice1-" + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port))),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port: pointer.Int32(10080),
			},
		},
		{
			Name: pointer.String("aservice1-" + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port))),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port: pointer.Int32(10081),
			},
		},
		{
			Name: pointer.String("aservice1-" + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port)) + "-IPv6"),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port: pointer.Int32(10080),
			},
		},
		{
			Name: pointer.String("aservice1-" + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port)) + "-IPv6"),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port: pointer.Int32(10081),
			},
		},
	}

	expectedSLb := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("aservice1"), service4, "Standard")
	(*expectedSLb.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].DisableOutboundSnat = pointer.Bool(true)
	(*expectedSLb.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].EnableTCPReset = pointer.Bool(true)
	(*expectedSLb.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].IdleTimeoutInMinutes = pointer.Int32(4)
	(*expectedSLb.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].DisableOutboundSnat = pointer.Bool(true)
	(*expectedSLb.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].EnableTCPReset = pointer.Bool(true)
	(*expectedSLb.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].IdleTimeoutInMinutes = pointer.Int32(4)
	expectedSLb.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
			},
		},
		{
			Name: pointer.String("bservice1"),
			ID:   pointer.String("bservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1")},
			},
		},
		{
			Name: pointer.String("aservice1-IPv6"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1-IPv6")},
			},
		},
		{
			Name: pointer.String("bservice1-IPv6"),
			ID:   pointer.String("bservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1-IPv6")},
			},
		},
	}

	service5 := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)
	slb5 := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("aservice1"), service5, "Standard")
	slb5.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
			},
		},
		{
			Name: pointer.String("bservice1"),
			ID:   pointer.String("bservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1")},
			},
		},
		{
			Name: pointer.String("aservice1-IPv6"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1-IPv6")},
			},
		},
		{
			Name: pointer.String("bservice1-IPv6"),
			ID:   pointer.String("bservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1-IPv6")},
			},
		},
	}
	slb5.Probes = &[]network.Probe{
		{
			Name: pointer.String("aservice1-" + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port))),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port: pointer.Int32(10080),
			},
		},
		{
			Name: pointer.String("aservice1-" + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port))),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port: pointer.Int32(10081),
			},
		},
		{
			Name: pointer.String("aservice1-" + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port)) + "-IPv6"),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port: pointer.Int32(10080),
			},
		},
		{
			Name: pointer.String("aservice1-" + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port)) + "-IPv6"),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port: pointer.Int32(10081),
			},
		},
	}

	//change to false to test that reconciliation will fix it (despite the fact that disable-tcp-reset was removed in 1.20)
	(*slb5.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].EnableTCPReset = pointer.Bool(false)
	(*slb5.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].EnableTCPReset = pointer.Bool(false)

	expectedSLb5 := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("aservice1"), service5, "Standard")
	(*expectedSLb5.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].DisableOutboundSnat = pointer.Bool(true)
	(*expectedSLb5.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].IdleTimeoutInMinutes = pointer.Int32(4)
	(*expectedSLb5.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].DisableOutboundSnat = pointer.Bool(true)
	(*expectedSLb5.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].IdleTimeoutInMinutes = pointer.Int32(4)
	expectedSLb5.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
			},
		},
		{
			Name: pointer.String("bservice1"),
			ID:   pointer.String("bservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1")},
			},
		},
		{
			Name: pointer.String("aservice1-IPv6"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1-IPv6")},
			},
		},
		{
			Name: pointer.String("bservice1-IPv6"),
			ID:   pointer.String("bservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1-IPv6")},
			},
		},
	}

	service6 := getTestServiceDualStack("service1", v1.ProtocolUDP, nil, 80)
	lb6 := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("aservice1"), service6, "basic")
	lb6.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{}
	lb6.Probes = &[]network.Probe{}
	expectedLB6 := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("aservice1"), service6, "basic")
	expectedLB6.Probes = &[]network.Probe{}
	(*expectedLB6.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].Probe = nil
	(*expectedLB6.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].EnableTCPReset = nil
	(*expectedLB6.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].Probe = nil
	(*expectedLB6.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].EnableTCPReset = nil
	expectedLB6.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
			},
		},
		{
			Name: pointer.String("aservice1-IPv6"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1-IPv6")},
			},
		},
	}

	service7 := getTestServiceDualStack("service1", v1.ProtocolUDP, nil, 80)
	service7.Spec.HealthCheckNodePort = 10081
	service7.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	lb7 := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("aservice1"), service7, "basic")
	lb7.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{}
	lb7.Probes = &[]network.Probe{}
	expectedLB7 := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("aservice1"), service7, "basic")
	(*expectedLB7.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].Probe = &network.SubResource{
		ID: pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/probes/aservice1-TCP-10081"),
	}
	(*expectedLB7.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].Probe = &network.SubResource{
		ID: pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/probes/aservice1-TCP-10081-IPv6"),
	}
	(*expectedLB7.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].EnableTCPReset = nil
	(*lb7.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].DisableOutboundSnat = pointer.Bool(true)
	(*expectedLB7.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].EnableTCPReset = nil
	(*lb7.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].DisableOutboundSnat = pointer.Bool(true)
	expectedLB7.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
			},
		},
		{
			Name: pointer.String("aservice1-IPv6"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1-IPv6")},
			},
		},
	}
	expectedLB7.Probes = &[]network.Probe{
		{
			Name: pointer.String("aservice1-" + string(v1.ProtocolTCP) +
				"-" + strconv.Itoa(int(service7.Spec.HealthCheckNodePort))),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port:              pointer.Int32(10081),
				RequestPath:       pointer.String("/healthz"),
				Protocol:          network.ProbeProtocolHTTP,
				IntervalInSeconds: pointer.Int32(5),
				ProbeThreshold:    pointer.Int32(2),
			},
		},
		{
			Name: pointer.String("aservice1-" + string(v1.ProtocolTCP) +
				"-" + strconv.Itoa(int(service7.Spec.HealthCheckNodePort)) + "-IPv6"),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port:              pointer.Int32(10081),
				RequestPath:       pointer.String("/healthz"),
				Protocol:          network.ProbeProtocolHTTP,
				IntervalInSeconds: pointer.Int32(5),
				ProbeThreshold:    pointer.Int32(2),
			},
		},
	}

	service8 := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)
	lb8 := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("anotherRG"), pointer.String("testCluster"), pointer.String("aservice1"), service8, "Standard")
	lb8.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{}
	lb8.Probes = &[]network.Probe{}
	expectedLB8 := getTestLoadBalancerDualStack(pointer.String("testCluster"), pointer.String("anotherRG"), pointer.String("testCluster"), pointer.String("aservice1"), service8, "Standard")
	(*expectedLB8.LoadBalancerPropertiesFormat.LoadBalancingRules)[0].DisableOutboundSnat = pointer.Bool(false)
	(*expectedLB8.LoadBalancerPropertiesFormat.LoadBalancingRules)[1].DisableOutboundSnat = pointer.Bool(false)
	expectedLB8.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
			},
		},
		{
			Name: pointer.String("aservice1-IPv6"),
			ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1-IPv6")},
			},
		},
	}
	expectedLB8.Probes = &[]network.Probe{
		{
			Name: pointer.String("aservice1-" + string(service8.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service7.Spec.Ports[0].Port))),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port:              pointer.Int32(10080),
				Protocol:          network.ProbeProtocolTCP,
				IntervalInSeconds: pointer.Int32(5),
				ProbeThreshold:    pointer.Int32(2),
			},
		},
		{
			Name: pointer.String("aservice1-" + string(service8.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service7.Spec.Ports[0].Port)) + "-IPv6"),
			ProbePropertiesFormat: &network.ProbePropertiesFormat{
				Port:              pointer.Int32(10080),
				Protocol:          network.ProbeProtocolTCP,
				IntervalInSeconds: pointer.Int32(5),
				ProbeThreshold:    pointer.Int32(2),
			},
		},
	}

	testCases := []struct {
		desc                                      string
		service                                   v1.Service
		loadBalancerSku                           string
		preConfigLBType                           string
		loadBalancerResourceGroup                 string
		disableOutboundSnat                       *bool
		wantLb                                    bool
		shouldRefreshLBAfterReconcileBackendPools bool
		existingLB                                network.LoadBalancer
		expectedLB                                network.LoadBalancer
		expectLBUpdate                            bool
		expectedGetLBError                        *retry.Error
		expectedError                             error
	}{
		{
			desc: "reconcileLoadBalancer shall return the lb deeply equal to the existingLB if there's no " +
				"modification needed when wantLb == true",
			loadBalancerSku: "basic",
			service:         service1,
			existingLB:      basicLb1,
			wantLb:          true,
			expectedLB:      basicLb1,
			expectedError:   nil,
		},
		{
			desc: "reconcileLoadBalancer shall return the lb deeply equal to the existingLB if there's no " +
				"modification needed when wantLb == false",
			loadBalancerSku: "basic",
			service:         service2,
			existingLB:      basicLb2,
			wantLb:          false,
			expectedLB:      basicLb2,
			expectedError:   nil,
		},
		{
			desc:            "reconcileLoadBalancer shall remove and reconstruct the corresponding field of lb",
			loadBalancerSku: "basic",
			service:         service3,
			existingLB:      modifiedLbs[0],
			wantLb:          true,
			expectedLB:      expectedLb1,
			expectLBUpdate:  true,
			expectedError:   nil,
		},
		{
			desc:            "reconcileLoadBalancer shall not raise an error",
			loadBalancerSku: "basic",
			service:         service3,
			existingLB:      modifiedLbs[1],
			preConfigLBType: "external",
			wantLb:          true,
			expectedLB:      expectedLb1,
			expectLBUpdate:  true,
			expectedError:   nil,
		},
		{
			desc:                "reconcileLoadBalancer shall remove and reconstruct the corresponding field of lb and set enableTcpReset to true in lbRule",
			loadBalancerSku:     "standard",
			service:             service4,
			disableOutboundSnat: pointer.Bool(true),
			existingLB:          existingSLB,
			wantLb:              true,
			expectedLB:          expectedSLb,
			expectLBUpdate:      true,
			expectedError:       nil,
		},
		{
			desc:                "reconcileLoadBalancer shall remove and reconstruct the corresponding field of lb and set enableTcpReset (false => true) in lbRule",
			loadBalancerSku:     "standard",
			service:             service5,
			disableOutboundSnat: pointer.Bool(true),
			existingLB:          slb5,
			wantLb:              true,
			expectedLB:          expectedSLb5,
			expectLBUpdate:      true,
			expectedError:       nil,
		},
		{
			desc:            "reconcileLoadBalancer shall reconcile UDP services",
			loadBalancerSku: "basic",
			service:         service6,
			existingLB:      lb6,
			wantLb:          true,
			expectedLB:      expectedLB6,
			expectLBUpdate:  true,
			expectedError:   nil,
		},
		{
			desc:            "reconcileLoadBalancer shall reconcile probes for local traffic policy UDP services",
			loadBalancerSku: "basic",
			service:         service7,
			existingLB:      lb7,
			wantLb:          true,
			expectedLB:      expectedLB7,
			expectLBUpdate:  true,
			expectedError:   nil,
		},
		{
			desc:                      "reconcileLoadBalancer in other resource group",
			loadBalancerSku:           "standard",
			loadBalancerResourceGroup: "anotherRG",
			service:                   service8,
			existingLB:                lb8,
			wantLb:                    true,
			expectedLB:                expectedLB8,
			expectLBUpdate:            true,
			expectedError:             nil,
		},
		{
			desc:            "reconcileLoadBalancer should refresh the LB after reconciling backend pools if needed",
			loadBalancerSku: "basic",
			service:         service1,
			existingLB:      basicLb1,
			wantLb:          true,
			shouldRefreshLBAfterReconcileBackendPools: true,
			expectedLB:    basicLb1,
			expectedError: nil,
		},
		{
			desc:       "reconcileLoadBalancer should return the error if failed to get the LB after reconcileBackendPools",
			service:    service1,
			existingLB: basicLb1,
			wantLb:     true,
			shouldRefreshLBAfterReconcileBackendPools: true,
			expectedGetLBError:                        retry.NewError(false, errors.New("error")),
			expectedError:                             fmt.Errorf("reconcileLoadBalancer for service (default/service1): failed to get load balancer testCluster: %w", retry.NewError(false, errors.New("error")).Error()),
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			az.Config.LoadBalancerSku = test.loadBalancerSku
			az.DisableOutboundSNAT = test.disableOutboundSnat
			if test.preConfigLBType != "" {
				az.Config.PreConfiguredBackendPoolLoadBalancerTypes = test.preConfigLBType
			}
			az.LoadBalancerResourceGroup = test.loadBalancerResourceGroup

			clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 3, 3)
			setMockEnvDualStack(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

			service := test.service
			setServiceLoadBalancerIP(&service, "1.2.3.4")
			setServiceLoadBalancerIP(&service, "fd00::eef0")

			err := az.PublicIPAddressesClient.CreateOrUpdate(context.TODO(), "rg", "pipName", network.PublicIPAddress{
				Name: pointer.String("pipName"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress:              pointer.String("1.2.3.4"),
					PublicIPAddressVersion: network.IPv4,
				},
			})
			assert.NoError(t, err.Error())
			err = az.PublicIPAddressesClient.CreateOrUpdate(context.TODO(), "rg", "pipName-IPv6", network.PublicIPAddress{
				Name: pointer.String("pipName-IPv6"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPAddress:              pointer.String("fd00::eef0"),
					PublicIPAddressVersion: network.IPv6,
				},
			})
			assert.NoError(t, err.Error())

			mockLBsClient := mockloadbalancerclient.NewMockInterface(ctrl)
			mockLBsClient.EXPECT().List(gomock.Any(), az.getLoadBalancerResourceGroup()).Return([]network.LoadBalancer{test.existingLB}, nil)
			mockLBsClient.EXPECT().Get(gomock.Any(), az.getLoadBalancerResourceGroup(), *test.existingLB.Name, gomock.Any()).Return(test.existingLB, test.expectedGetLBError).AnyTimes()
			expectLBUpdateCount := 1
			if test.expectLBUpdate {
				expectLBUpdateCount++
			}
			mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.getLoadBalancerResourceGroup(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(expectLBUpdateCount)
			az.LoadBalancerClient = mockLBsClient

			err = az.LoadBalancerClient.CreateOrUpdate(context.TODO(), az.getLoadBalancerResourceGroup(), "lb1", test.existingLB, "")
			assert.NoError(t, err.Error())

			mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
			if test.shouldRefreshLBAfterReconcileBackendPools {
				mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, &test.expectedLB, test.expectedError)
			}
			mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(clusterName string, service *v1.Service, lb *network.LoadBalancer) (bool, bool, *network.LoadBalancer, error) {
				return false, false, lb, nil
			}).AnyTimes()
			mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

			lb, rerr := az.reconcileLoadBalancer("testCluster", &service, clusterResources.nodes, test.wantLb)
			assert.Equal(t, test.expectedError, rerr)

			if test.expectedError == nil {
				assert.Equal(t, test.expectedLB, *lb)
			}
		})
	}
}

func TestGetServiceLoadBalancerStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	// service has the same IP family as internalService.
	service := getTestService("service1", v1.ProtocolTCP, nil, false, 80)
	internalService := getInternalTestService("service1", 80)
	v4Enabled, v6Enabled := getIPFamiliesEnabled(&service)

	setMockPublicIPs(az, ctrl, 1, v4Enabled, v6Enabled)

	lb1 := getTestLoadBalancer(pointer.String("lb1"), pointer.String("rg"), pointer.String("testCluster"),
		pointer.String("aservice1"), internalService, "Basic")
	lb1.FrontendIPConfigurations = nil
	lb2 := getTestLoadBalancer(pointer.String("lb2"), pointer.String("rg"), pointer.String("testCluster"),
		pointer.String("aservice1"), internalService, "Basic")
	lb2.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress:  &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
				PrivateIPAddress: pointer.String("private"),
			},
		},
	}
	lb3 := getTestLoadBalancer(pointer.String("lb3"), pointer.String("rg"), pointer.String("testCluster"),
		pointer.String("test1"), internalService, "Basic")
	lb3.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("bservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress:  &network.PublicIPAddress{ID: pointer.String("testCluster-bservice1")},
				PrivateIPAddress: pointer.String("private"),
			},
		},
	}
	lb4 := getTestLoadBalancer(pointer.String("lb4"), pointer.String("rg"), pointer.String("testCluster"),
		pointer.String("aservice1"), service, "Basic")
	lb4.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress:  &network.PublicIPAddress{ID: nil},
				PrivateIPAddress: pointer.String("private"),
			},
		},
	}
	lb5 := getTestLoadBalancer(pointer.String("lb5"), pointer.String("rg"), pointer.String("testCluster"),
		pointer.String("aservice1"), service, "Basic")
	lb5.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress:  nil,
				PrivateIPAddress: pointer.String("private"),
			},
		},
	}
	lb6 := getTestLoadBalancer(pointer.String("lb6"), pointer.String("rg"), pointer.String("testCluster"),
		pointer.String("aservice1"), service, "Basic")
	lb6.FrontendIPConfigurations = &[]network.FrontendIPConfiguration{
		{
			Name: pointer.String("aservice1"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress:  &network.PublicIPAddress{ID: pointer.String("illegal/id/")},
				PrivateIPAddress: pointer.String("private"),
			},
		},
	}

	testCases := []struct {
		desc           string
		service        *v1.Service
		lb             *network.LoadBalancer
		expectedStatus *v1.LoadBalancerStatus
		expectedError  bool
	}{
		{
			desc:    "getServiceLoadBalancer shall return nil if no lb is given",
			service: &service,
			lb:      nil,
		},
		{
			desc:    "getServiceLoadBalancerStatus shall return nil if given lb has no front ip config",
			service: &service,
			lb:      &lb1,
		},
		{
			desc:           "getServiceLoadBalancerStatus shall return private ip if service is internal",
			service:        &internalService,
			lb:             &lb2,
			expectedStatus: &v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: "private"}}},
		},
		{
			desc: "getServiceLoadBalancerStatus shall return nil if lb.FrontendIPConfigurations.name != " +
				"az.getDefaultFrontendIPConfigName(service)",
			service: &internalService,
			lb:      &lb3,
		},
		{
			desc: "getServiceLoadBalancerStatus shall report error if the id of lb's " +
				"public ip address cannot be read",
			service:       &service,
			lb:            &lb4,
			expectedError: true,
		},
		{
			desc:          "getServiceLoadBalancerStatus shall report error if lb's public ip address cannot be read",
			service:       &service,
			lb:            &lb5,
			expectedError: true,
		},
		{
			desc:          "getServiceLoadBalancerStatus shall report error if id of lb's public ip address is illegal",
			service:       &service,
			lb:            &lb6,
			expectedError: true,
		},
		{
			desc: "getServiceLoadBalancerStatus shall return the corresponding " +
				"lb status if everything is good",
			service:        &service,
			lb:             &lb2,
			expectedStatus: &v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: "1.2.3.4"}}},
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			status, _, _, err := az.getServiceLoadBalancerStatus(test.service, test.lb)
			assert.Equal(t, test.expectedStatus, status)
			assert.Equal(t, test.expectedError, err != nil)
		})
	}
}

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
			service: getTestService("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationAllowedServiceTags: "tag"}, false, 80),
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
		{
			desc: "reconcileSecurityGroup shall create sgs while allowedIPRanges annotation is set for IPv4",
			service: getTestService("svc", v1.ProtocolTCP, map[string]string{
				consts.ServiceAnnotationAllowedIPRanges: "10.10.10.0/24,192.168.0.1/32",
			}, false, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name:                          pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
			}},
			lbIPs:  &[]string{"10.0.0.1", "10.0.0.2"},
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("asvc-TCP-80-10.10.10.0_24"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("10.10.10.0/24"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("asvc-TCP-80-192.168.0.1_32"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("192.168.0.1/32"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(501),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc: "reconcileSecurityGroup shall create sgs while allowedIPRanges annotation is set for IPv6",
			service: getTestService("svc", v1.ProtocolTCP, map[string]string{
				consts.ServiceAnnotationAllowedIPRanges: "2001:0db8:85a3::/64,2607:f0d0:1002:0051::/64",
			}, true, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name:                          pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
			}},
			lbIPs:  &[]string{"10.0.0.1", "10.0.0.2", "fd12:3456:789a:1::1"},
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("asvc-TCP-80-2001.db8.85a3.._64"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                 network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:          pointer.String("*"),
								DestinationPortRange:     pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:      pointer.String("2001:db8:85a3::/64"),
								DestinationAddressPrefix: pointer.String("fd12:3456:789a:1::1"),
								Access:                   network.SecurityRuleAccessAllow,
								Priority:                 pointer.Int32(500),
								Direction:                network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("asvc-TCP-80-2607.f0d0.1002.51.._64"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                 network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:          pointer.String("*"),
								DestinationPortRange:     pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:      pointer.String("2607:f0d0:1002:51::/64"),
								DestinationAddressPrefix: pointer.String("fd12:3456:789a:1::1"),
								Access:                   network.SecurityRuleAccessAllow,
								Priority:                 pointer.Int32(501),
								Direction:                network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc: "reconcileSecurityGroup shall create sgs while allowedIPRanges annotation is set for both IPv4 and IPv6",
			service: getTestServiceDualStack("svc", v1.ProtocolTCP, map[string]string{
				consts.ServiceAnnotationAllowedIPRanges: "10.10.10.0/24,2607:f0d0:1002:0051::/64",
			}, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name:                          pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
			}},
			lbIPs:  &[]string{"10.0.0.1", "10.0.0.2", "fd12:3456:789a:1::1"},
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("asvc-TCP-80-10.10.10.0_24"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("10.10.10.0/24"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("asvc-TCP-80-2607.f0d0.1002.51.._64-IPv6"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                 network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:          pointer.String("*"),
								DestinationPortRange:     pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:      pointer.String("2607:f0d0:1002:51::/64"),
								DestinationAddressPrefix: pointer.String("fd12:3456:789a:1::1"),
								Access:                   network.SecurityRuleAccessAllow,
								Priority:                 pointer.Int32(501),
								Direction:                network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc: "reconcileSecurityGroup shall create sgs with deny_all rule and not allow internet access when provided allowedIPRanges is invalid",
			service: getTestService("svc", v1.ProtocolTCP, map[string]string{
				consts.ServiceAnnotationAllowedIPRanges: "10.10.10.1/24",
			}, false, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name:                          pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
			}},
			lbIPs:  &[]string{"10.0.0.1", "10.0.0.2"},
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("asvc-TCP-80-deny_all"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								SourceAddressPrefix:        pointer.String("*"),
								DestinationPortRange:       pointer.String("80"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccess("Deny"),
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc: "reconcileSecurityGroup shall create sgs while allowedIPRanges and serviceTags annotation is set",
			service: getTestService("svc", v1.ProtocolTCP, map[string]string{
				consts.ServiceAnnotationAllowedIPRanges:    "10.10.10.0/24,192.168.0.1/32",
				consts.ServiceAnnotationAllowedServiceTags: "foo,bar",
			}, false, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name:                          pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{},
			}},
			lbIPs:  &[]string{"10.0.0.1", "10.0.0.2"},
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("asvc-TCP-80-10.10.10.0_24"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("10.10.10.0/24"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("asvc-TCP-80-192.168.0.1_32"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("192.168.0.1/32"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(501),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("asvc-TCP-80-bar"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("bar"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(502),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("asvc-TCP-80-foo"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("foo"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(503),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			},
		},
		{
			desc: "reconcileSecurityGroup shall create/update/delete sgs while allowedIPRanges and serviceTags annotation is set",
			service: getTestService("svc", v1.ProtocolTCP, map[string]string{
				consts.ServiceAnnotationAllowedIPRanges:    "10.10.10.0/24,192.168.0.1/32",
				consts.ServiceAnnotationAllowedServiceTags: "foo,bar",
			}, false, 80),
			existingSgs: map[string]network.SecurityGroup{"nsg": {
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							// will be kept
							Name: pointer.String("asvc-TCP-80-192.168.0.1_32"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("192.168.0.1/32"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							// will be removed: no longer in allowedIPRanges
							Name: pointer.String("asvc-TCP-80-192.168.0.5_32"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("192.168.0.5/32"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(501),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
					},
				},
			}},
			lbIPs:  &[]string{"10.0.0.1", "10.0.0.2"},
			wantLb: true,
			expectedSg: &network.SecurityGroup{
				Name: pointer.String("nsg"),
				SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
					SecurityRules: &[]network.SecurityRule{
						{
							Name: pointer.String("asvc-TCP-80-10.10.10.0_24"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("10.10.10.0/24"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(500),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("asvc-TCP-80-192.168.0.1_32"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("192.168.0.1/32"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(501),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("asvc-TCP-80-bar"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("bar"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(502),
								Direction:                  network.SecurityRuleDirection("Inbound"),
							},
						},
						{
							Name: pointer.String("asvc-TCP-80-foo"),
							SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
								Protocol:                   network.SecurityRuleProtocol("Tcp"),
								SourcePortRange:            pointer.String("*"),
								DestinationPortRange:       pointer.String(strconv.Itoa(80)),
								SourceAddressPrefix:        pointer.String("foo"),
								DestinationAddressPrefixes: &([]string{"10.0.0.1", "10.0.0.2"}),
								Access:                     network.SecurityRuleAccessAllow,
								Priority:                   pointer.Int32(503),
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

	tests := []struct {
		name          string
		sourceRanges  []string
		originalRules []network.SecurityRule
		expectedRules []network.SecurityRule
	}{
		{
			name:          "should add deny-all rule if not exists #1",
			sourceRanges:  []string{"1.2.3.4/32"},
			originalRules: nil,
			expectedRules: []network.SecurityRule{
				{
					Name: pointer.String("atest1-TCP-80-1.2.3.4_32"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(500),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-deny_all"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("*"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessDeny,
						Priority:                 pointer.Int32(4096),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
			},
		},

		{
			name:         "should add deny-all rule if not exists #2",
			sourceRanges: []string{"1.2.3.4/32"},
			originalRules: []network.SecurityRule{
				{
					Name: pointer.String("atest1-TCP-80-1.2.3.4_32"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(505),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
			},
			expectedRules: []network.SecurityRule{
				{
					Name: pointer.String("atest1-TCP-80-1.2.3.4_32"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(500), // would be reset
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-deny_all"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("*"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessDeny,
						Priority:                 pointer.Int32(4096),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
			},
		},
		{
			name:         "should move deny-all rule after allow rule: reorder priority",
			sourceRanges: []string{"1.2.3.4/32"},
			originalRules: []network.SecurityRule{
				{
					Name: pointer.String("atest1-TCP-80-1.2.3.4_32"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(505),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-deny_all"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("*"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessDeny,
						Priority:                 pointer.Int32(503),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
			},
			expectedRules: []network.SecurityRule{
				{
					Name: pointer.String("atest1-TCP-80-1.2.3.4_32"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(500), // would be reset
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-deny_all"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("*"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessDeny,
						Priority:                 pointer.Int32(4096),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
			},
		},
		{
			name:         "should move deny-all rule after allow rule: add new source range",
			sourceRanges: []string{"1.2.3.4/32", "10.0.0.0/16"},
			originalRules: []network.SecurityRule{
				{
					Name: pointer.String("atest1-TCP-80-1.2.3.4_32"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(505),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-deny_all"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("*"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessDeny,
						Priority:                 pointer.Int32(503),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
			},
			expectedRules: []network.SecurityRule{
				{
					Name: pointer.String("atest1-TCP-80-1.2.3.4_32"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(500), // would be reset
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-10.0.0.0_16"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("10.0.0.0/16"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(501),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-deny_all"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("*"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessDeny,
						Priority:                 pointer.Int32(4096),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
			},
		},
		{
			name:         "should move deny-all rule after allow rule: keep unmanaged rules unchanged",
			sourceRanges: []string{"1.2.3.4/32"},
			originalRules: []network.SecurityRule{
				{
					Name: pointer.String("unmanaged_rule_1"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(499),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("unmanaged_rule_2"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/16"),
						DestinationPortRange:     pointer.String("443"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(5000),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("unmanaged_rule_3"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/16"),
						DestinationPortRange:     pointer.String("443"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessDeny,
						Priority:                 pointer.Int32(400),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-1.2.3.4_32"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(505),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-deny_all"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("*"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessDeny,
						Priority:                 pointer.Int32(503),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
			},
			expectedRules: []network.SecurityRule{
				{
					Name: pointer.String("unmanaged_rule_3"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/16"),
						DestinationPortRange:     pointer.String("443"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessDeny,
						Priority:                 pointer.Int32(400),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("unmanaged_rule_1"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(499),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-1.2.3.4_32"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/32"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(500), // would be reset
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("atest1-TCP-80-deny_all"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("*"),
						DestinationPortRange:     pointer.String("80"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessDeny,
						Priority:                 pointer.Int32(4096),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
				{
					Name: pointer.String("unmanaged_rule_2"),
					SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
						Protocol:                 network.SecurityRuleProtocolTCP,
						SourcePortRange:          pointer.String("*"),
						SourceAddressPrefix:      pointer.String("1.2.3.4/16"),
						DestinationPortRange:     pointer.String("443"),
						DestinationAddressPrefix: pointer.String("1.1.1.1"),
						Access:                   network.SecurityRuleAccessAllow,
						Priority:                 pointer.Int32(5000),
						Direction:                network.SecurityRuleDirectionInbound,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		originalSg := network.SecurityGroup{
			Name: pointer.String("nsg"),
			SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
				SecurityRules: &tt.originalRules,
			},
		}
		expectedSg := network.SecurityGroup{
			Name: pointer.String("nsg"),
			SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
				SecurityRules: &tt.expectedRules,
			},
		}

		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			az := GetTestCloud(ctrl)
			service := getTestService("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges: "true"}, false, 80)
			service.Spec.LoadBalancerSourceRanges = tt.sourceRanges
			lbIPs := &[]string{"1.1.1.1"}

			mockSGClient := az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
			mockSGClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any()).Return(originalSg, nil)
			mockSGClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
			sg, err := az.reconcileSecurityGroup("testCluster", &service, lbIPs, nil, true)
			assert.NoError(t, err)
			assert.Equal(t, expectedSg, *sg)
		})
	}
}

func TestReconcileSecurityGroup(t *testing.T) {
	var (
		clusterName = "testCluster"
		lbIPs       = []string{"10.0.0.1", "10.0.0.2"}
		lbName      = "lb-name"
	)

	t.Run("with spec.loadBalancerSourceRanges and IPRanges annotation specified", func(t *testing.T) {
		var (
			ctrl = gomock.NewController(t)
			az   = GetTestCloud(ctrl)
			svc  = v1.Service{
				Spec: v1.ServiceSpec{
					Type:                     v1.ServiceTypeLoadBalancer,
					LoadBalancerSourceRanges: []string{"10.10.10.0/24"},
					IPFamilies: []v1.IPFamily{
						v1.IPv4Protocol,
						v1.IPv6Protocol,
					},
					Ports: []v1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: v1.ProtocolTCP,
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-svc",
					Annotations: map[string]string{
						consts.ServiceAnnotationAllowedIPRanges: "192.168.0.1/32",
					},
				},
			}
		)
		defer ctrl.Finish()

		mockSGClient := az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
		mockSGClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any()).Return(network.SecurityGroup{
			Name: pointer.String("nsg"),
			SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
				SecurityRules: &[]network.SecurityRule{},
			},
		}, nil)
		_, err := az.reconcileSecurityGroup(clusterName, &svc, &lbIPs, &lbName, true)
		assert.Error(t, err)
		assert.EqualError(t, err, fmt.Sprintf(
			"both of spec.loadBalancerSourceRanges and annotation %s are specified for service %s, which is not allowed",
			consts.ServiceAnnotationAllowedIPRanges, svc.Name,
		))
	})

	t.Run("with spec.loadBalancerSourceRanges and ServiceTags annotation specified", func(t *testing.T) {
		var (
			ctrl = gomock.NewController(t)
			az   = GetTestCloud(ctrl)
			svc  = v1.Service{
				Spec: v1.ServiceSpec{
					Type:                     v1.ServiceTypeLoadBalancer,
					LoadBalancerSourceRanges: []string{"10.10.10.0/24"},
					IPFamilies: []v1.IPFamily{
						v1.IPv4Protocol,
						v1.IPv6Protocol,
					},
					Ports: []v1.ServicePort{
						{
							Name:     "http",
							Port:     80,
							Protocol: v1.ProtocolTCP,
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-svc",
					Annotations: map[string]string{
						consts.ServiceAnnotationAllowedServiceTags: "foo,bar",
					},
				},
			}
		)
		defer ctrl.Finish()

		mockSGClient := az.SecurityGroupsClient.(*mocksecuritygroupclient.MockInterface)
		mockSGClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any()).Return(network.SecurityGroup{
			Name: pointer.String("nsg"),
			SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
				SecurityRules: &[]network.SecurityRule{},
			},
		}, nil)
		mockSGClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

		_, err := az.reconcileSecurityGroup(clusterName, &svc, &lbIPs, &lbName, true)
		assert.NoError(t, err)
	})
}

func TestSafeDeletePublicIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc          string
		pip           *network.PublicIPAddress
		lb            *network.LoadBalancer
		listError     *retry.Error
		expectedError error
	}{
		{
			desc: "safeDeletePublicIP shall delete corresponding ip configurations and lb rules",
			pip: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPConfiguration: &network.IPConfiguration{
						ID: pointer.String("id1"),
					},
				},
			},
			lb: &network.LoadBalancer{
				Name: pointer.String("lb1"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					FrontendIPConfigurations: &[]network.FrontendIPConfiguration{
						{
							ID: pointer.String("id1"),
							FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
								LoadBalancingRules: &[]network.SubResource{{ID: pointer.String("rules1")}},
							},
						},
					},
					LoadBalancingRules: &[]network.LoadBalancingRule{{ID: pointer.String("rules1")}},
				},
			},
		},
		{
			desc: "safeDeletePublicIP should return error if failed to list pip",
			pip: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPConfiguration: &network.IPConfiguration{
						ID: pointer.String("id1"),
					},
				},
			},
			listError:     retry.NewError(false, errors.New("error")),
			expectedError: retry.NewError(false, errors.New("error")).Error(),
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			if test.pip != nil &&
				test.pip.PublicIPAddressPropertiesFormat != nil &&
				test.pip.IPConfiguration != nil {
				mockPIPsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]network.PublicIPAddress{*test.pip}, test.listError)
			}
			mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "pip1", gomock.Any()).Return(nil).AnyTimes()
			mockPIPsClient.EXPECT().Delete(gomock.Any(), "rg", "pip1").Return(nil).AnyTimes()
			err := az.PublicIPAddressesClient.CreateOrUpdate(context.TODO(), "rg", "pip1", network.PublicIPAddress{
				Name: pointer.String("pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					IPConfiguration: &network.IPConfiguration{
						ID: pointer.String("id1"),
					},
				},
			})
			assert.NoError(t, err.Error())
			service := getTestService("test1", v1.ProtocolTCP, nil, false, 80)
			if test.listError == nil {
				mockLBsClient := mockloadbalancerclient.NewMockInterface(ctrl)
				mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
				az.LoadBalancerClient = mockLBsClient
			}
			rerr := az.safeDeletePublicIP(&service, "rg", test.pip, test.lb)
			if test.expectedError == nil {
				assert.Equal(t, 0, len(*test.lb.FrontendIPConfigurations))
				assert.Equal(t, 0, len(*test.lb.LoadBalancingRules))
				assert.NoError(t, rerr)
			} else {
				assert.Equal(t, rerr.Error(), test.listError.Error().Error())
			}
		})
	}
}

func TestReconcilePublicIPsCommon(t *testing.T) {
	deleteUnwantedPIPsAndCreateANewOneclientGet := func(client *mockpublicipclient.MockInterface) {
		client.EXPECT().Get(gomock.Any(), "rg", "testCluster-atest1", gomock.Any()).Return(network.PublicIPAddress{ID: pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-atest1")}, nil).Times(1)
		client.EXPECT().Get(gomock.Any(), "rg", "testCluster-atest1-IPv6", gomock.Any()).Return(network.PublicIPAddress{ID: pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-atest1-IPv6")}, nil).Times(1)
	}
	getPIPAddMissingOne := func(client *mockpublicipclient.MockInterface) {
		client.EXPECT().Get(gomock.Any(), "rg", "testCluster-atest1-IPv6", gomock.Any()).Return(network.PublicIPAddress{ID: pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-atest1-IPv6")}, nil).Times(1)
	}

	testCases := []struct {
		desc                        string
		annotations                 map[string]string
		existingPIPs                []network.PublicIPAddress
		wantLb                      bool
		expectedIDs                 []string
		expectedPIPs                []*network.PublicIPAddress // len(expectedPIPs) <= 2
		expectedError               bool
		expectedCreateOrUpdateCount int
		expectedDeleteCount         int
		expectedClientGet           *func(client *mockpublicipclient.MockInterface)
	}{
		{
			desc:                        "shall return nil if there's no pip in service",
			wantLb:                      false,
			expectedCreateOrUpdateCount: 0,
			expectedDeleteCount:         0,
		},
		{
			desc:   "shall return nil if no pip is owned by service",
			wantLb: false,
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pip1"),
				},
			},
			expectedCreateOrUpdateCount: 0,
			expectedDeleteCount:         0,
		},
		{
			desc:   "shall delete unwanted pips and create new ones",
			wantLb: true,
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						IPAddress: pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("pip1-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						IPAddress: pointer.String("fd00::eef0"),
					},
				},
			},
			expectedIDs: []string{
				"/subscriptions/subscription/resourceGroups/rg/providers/" +
					"Microsoft.Network/publicIPAddresses/testCluster-atest1",
				"/subscriptions/subscription/resourceGroups/rg/providers/" +
					"Microsoft.Network/publicIPAddresses/testCluster-atest1-IPv6",
			},
			expectedCreateOrUpdateCount: 2,
			expectedDeleteCount:         2,
			expectedClientGet:           &deleteUnwantedPIPsAndCreateANewOneclientGet,
		},
		{
			desc:        "shall report error if the given PIP name doesn't exist in the resource group",
			wantLb:      true,
			annotations: map[string]string{consts.ServiceAnnotationPIPNameDualStack[false]: "testPIP"},
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
				},
				{
					Name: pointer.String("pip2"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
				},
			},
			expectedError:               true,
			expectedCreateOrUpdateCount: 0,
			expectedDeleteCount:         0,
		},
		{
			desc:   "shall delete unwanted PIP when given the name of desired PIP",
			wantLb: true,
			annotations: map[string]string{
				consts.ServiceAnnotationPIPNameDualStack[false]: "testPIP",
				consts.ServiceAnnotationPIPNameDualStack[true]:  "testPIP-IPv6",
			},
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("pip2"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Dynamic,
						IPAddress:                pointer.String("fd00::eef0"),
					},
				},
			},
			expectedPIPs: []*network.PublicIPAddress{
				{
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Name: pointer.String("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Name: pointer.String("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Dynamic,
						IPAddress:                pointer.String("fd00::eef0"),
					},
				},
			},
			expectedCreateOrUpdateCount: 0, // Before the fix, this was 1 because IPv4's IP Version was not set. Same for other tests.
			expectedDeleteCount:         2,
		},
		{
			desc:   "shall not delete unwanted PIP when there are other service references",
			wantLb: true,
			annotations: map[string]string{
				consts.ServiceAnnotationPIPNameDualStack[false]: "testPIP",
				consts.ServiceAnnotationPIPNameDualStack[true]:  "testPIP-IPv6",
			},
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("pip2"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1,default/test2")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("pip1-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv6,
						IPAddress:              pointer.String("fd00::eef0"),
					},
				},
				{
					Name: pointer.String("pip2-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1,default/test2")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv6,
						IPAddress:              pointer.String("fd00::eef0"),
					},
				},
				{
					Name: pointer.String("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Dynamic,
						IPAddress:                pointer.String("fd00::eef0"),
					},
				},
			},
			expectedPIPs: []*network.PublicIPAddress{
				{
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Name: pointer.String("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Name: pointer.String("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Dynamic,
						IPAddress:                pointer.String("fd00::eef0"),
					},
				},
			},
			expectedCreateOrUpdateCount: 0,
			expectedDeleteCount:         2,
		},
		{
			desc:   "shall delete unwanted pips and existing pips, when the existing pips IP do not match",
			wantLb: true,
			annotations: map[string]string{
				consts.ServiceAnnotationPIPNameDualStack[false]: "testPIP",
				consts.ServiceAnnotationPIPNameDualStack[true]:  "testPIP-IPv6",
				consts.ServiceAnnotationIPTagsForPublicIP:       "tag1=tag1value",
			},
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("pip2"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Dynamic,
						IPAddress:                pointer.String("fd00::eef0"),
					},
				},
			},
			expectedPIPs: []*network.PublicIPAddress{
				{
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Name: pointer.String("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv4,
						PublicIPAllocationMethod: network.Static,
						IPTags: &[]network.IPTag{
							{
								IPTagType: pointer.String("tag1"),
								Tag:       pointer.String("tag1value"),
							},
						},
					},
				},
				{
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Name: pointer.String("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Dynamic,
						IPTags: &[]network.IPTag{
							{
								IPTagType: pointer.String("tag1"),
								Tag:       pointer.String("tag1value"),
							},
						},
					},
				},
			},
			expectedCreateOrUpdateCount: 2,
			expectedDeleteCount:         2,
		},
		{
			desc:   "shall preserve existing pips, when the existing pips IP tags do match",
			wantLb: true,
			annotations: map[string]string{
				consts.ServiceAnnotationPIPNameDualStack[false]: "testPIP",
				consts.ServiceAnnotationPIPNameDualStack[true]:  "testPIP-IPv6",
				consts.ServiceAnnotationIPTagsForPublicIP:       "tag1=tag1value",
			},
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv4,
						PublicIPAllocationMethod: network.Static,
						IPTags: &[]network.IPTag{
							{
								IPTagType: pointer.String("tag1"),
								Tag:       pointer.String("tag1value"),
							},
						},
						IPAddress: pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Dynamic,
						IPTags: &[]network.IPTag{
							{
								IPTagType: pointer.String("tag1"),
								Tag:       pointer.String("tag1value"),
							},
						},
						IPAddress: pointer.String("fd00::eef0"),
					},
				},
			},
			expectedPIPs: []*network.PublicIPAddress{
				{
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Name: pointer.String("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv4,
						PublicIPAllocationMethod: network.Static,
						IPTags: &[]network.IPTag{
							{
								IPTagType: pointer.String("tag1"),
								Tag:       pointer.String("tag1value"),
							},
						},
						IPAddress: pointer.String("1.2.3.4"),
					},
				},
				{
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Name: pointer.String("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Dynamic,
						IPTags: &[]network.IPTag{
							{
								IPTagType: pointer.String("tag1"),
								Tag:       pointer.String("tag1value"),
							},
						},
						IPAddress: pointer.String("fd00::eef0"),
					},
				},
			},
			expectedCreateOrUpdateCount: 0,
			expectedDeleteCount:         0,
		},
		{
			desc:   "shall find the PIP by given name and shall not delete the PIP which is not owned by service",
			wantLb: true,
			annotations: map[string]string{
				consts.ServiceAnnotationPIPNameDualStack[false]: "testPIP",
				consts.ServiceAnnotationPIPNameDualStack[true]:  "testPIP-IPv6",
			},
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pip1"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("pip2"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("testPIP"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("pip2-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Dynamic,
						IPAddress:                pointer.String("fd00::eef0"),
					},
				},
				{
					Name: pointer.String("testPIP-IPv6"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Dynamic,
						IPAddress:                pointer.String("fd00::eef0"),
					},
				},
			},
			expectedPIPs: []*network.PublicIPAddress{
				{
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Name: pointer.String("testPIP"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Name: pointer.String("testPIP-IPv6"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Dynamic,
						IPAddress:                pointer.String("fd00::eef0"),
					},
				},
			},
			expectedCreateOrUpdateCount: 0,
			expectedDeleteCount:         2,
		},
		{
			desc:   "shall delete the unwanted PIP name from service tag and shall not delete it if there is other reference",
			wantLb: false,
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1,default/test2")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						IPAddress:              pointer.String("1.2.3.4"),
						PublicIPAddressVersion: network.IPv4,
					},
				},
				{
					Name: pointer.String("pip1-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1,default/test2")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						IPAddress:                pointer.String("fd00::eef0"),
						PublicIPAllocationMethod: network.Dynamic,
						PublicIPAddressVersion:   network.IPv6,
					},
				},
			},
			expectedCreateOrUpdateCount: 2,
		},
		{
			desc:   "shall create the missing one",
			wantLb: true,
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("testCluster-atest1"),
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-atest1"),
					Tags: map[string]*string{consts.ServiceTagKey: pointer.String("default/test1")},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
			},
			expectedIDs: []string{
				"/subscriptions/subscription/resourceGroups/rg/providers/" +
					"Microsoft.Network/publicIPAddresses/testCluster-atest1",
				"/subscriptions/subscription/resourceGroups/rg/providers/" +
					"Microsoft.Network/publicIPAddresses/testCluster-atest1-IPv6",
			},
			expectedCreateOrUpdateCount: 1,
			expectedDeleteCount:         0,
			expectedClientGet:           &getPIPAddMissingOne,
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			deletedPips := make(map[string]bool)
			savedPips := make(map[string]network.PublicIPAddress)
			createOrUpdateCount := 0
			var m sync.Mutex
			az := GetTestCloud(ctrl)
			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			creator := mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any()).AnyTimes()
			creator.DoAndReturn(func(ctx context.Context, resourceGroupName string, publicIPAddressName string, parameters network.PublicIPAddress) *retry.Error {
				m.Lock()
				deletedPips[publicIPAddressName] = false
				savedPips[publicIPAddressName] = parameters
				createOrUpdateCount++
				m.Unlock()
				return nil
			})

			if test.expectedClientGet != nil {
				(*test.expectedClientGet)(mockPIPsClient)
			}
			service := getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80)
			service.Annotations = test.annotations
			for _, pip := range test.existingPIPs {
				savedPips[*pip.Name] = pip
				getter := mockPIPsClient.EXPECT().Get(gomock.Any(), "rg", *pip.Name, gomock.Any()).AnyTimes()
				getter.DoAndReturn(func(ctx context.Context, resourceGroupName string, publicIPAddressName string, expand string) (result network.PublicIPAddress, rerr *retry.Error) {
					m.Lock()
					deletedValue, deletedContains := deletedPips[publicIPAddressName]
					savedPipValue, savedPipContains := savedPips[publicIPAddressName]
					m.Unlock()

					if (!deletedContains || !deletedValue) && savedPipContains {
						return savedPipValue, nil
					}

					return network.PublicIPAddress{}, &retry.Error{HTTPStatusCode: http.StatusNotFound}

				})
				deleter := mockPIPsClient.EXPECT().Delete(gomock.Any(), "rg", *pip.Name).Return(nil).AnyTimes()
				deleter.Do(func(ctx context.Context, resourceGroupName string, publicIPAddressName string) *retry.Error {
					m.Lock()
					deletedPips[publicIPAddressName] = true
					m.Unlock()
					return nil
				})

				err := az.PublicIPAddressesClient.CreateOrUpdate(context.TODO(), "rg", pointer.StringDeref(pip.Name, ""), pip)
				assert.NoError(t, err.Error())

				// Clear create or update count to prepare for main execution
				createOrUpdateCount = 0
			}
			lister := mockPIPsClient.EXPECT().List(gomock.Any(), "rg").AnyTimes()
			lister.DoAndReturn(func(ctx context.Context, resourceGroupName string) (result []network.PublicIPAddress, rerr *retry.Error) {
				m.Lock()
				for pipName, pip := range savedPips {
					deleted, deletedContains := deletedPips[pipName]
					if !deletedContains || !deleted {
						result = append(result, pip)
					}
				}
				m.Unlock()
				return
			})

			pips, err := az.reconcilePublicIPs("testCluster", &service, "", test.wantLb)
			if !test.expectedError {
				assert.NoError(t, err)
			}
			// Check IDs
			if len(test.expectedIDs) != 0 {
				ids := []string{}
				for _, pip := range pips {
					ids = append(ids, pointer.StringDeref(pip.ID, ""))
				}
				assert.Truef(t, compareStrings(test.expectedIDs, ids),
					"expectedIDs %q, IDs %q", test.expectedIDs, ids)
			}
			// Check PIPs
			if len(test.expectedPIPs) != 0 {
				pipsNames := []string{}
				for _, pip := range pips {
					pipsNames = append(pipsNames, pointer.StringDeref(pip.Name, ""))
				}
				assert.Equal(t, len(test.expectedPIPs), len(pips), pipsNames)
				pipsOrdered := []*network.PublicIPAddress{}
				if len(test.expectedPIPs) == 1 {
					pipsOrdered = append(pipsOrdered, pips[0])
				} else {
					// len(test.expectedPIPs) == 2
					if pointer.StringDeref(test.expectedPIPs[0].Name, "") == pointer.StringDeref(pips[0].Name, "") {
						pipsOrdered = append(pipsOrdered, pips...)
					} else {
						pipsOrdered = append(pipsOrdered, pips[1], pips[0])
					}
				}
				for i := range pipsOrdered {
					pip := pipsOrdered[i]
					assert.NotNil(t, test.expectedPIPs[i].Name)
					assert.NotNil(t, pip.Name)
					assert.Equal(t, *test.expectedPIPs[i].Name, *pip.Name, "pip name %q", *pip.Name)

					if test.expectedPIPs[i].PublicIPAddressPropertiesFormat != nil {
						sortIPTags(test.expectedPIPs[i].PublicIPAddressPropertiesFormat.IPTags)
					}

					if pip.PublicIPAddressPropertiesFormat != nil {
						sortIPTags(pip.PublicIPAddressPropertiesFormat.IPTags)
					}

					assert.Equal(t, test.expectedPIPs[i].PublicIPAddressPropertiesFormat,
						pip.PublicIPAddressPropertiesFormat, "pip name %q", *pip.Name)
				}
			}
			assert.Equal(t, test.expectedCreateOrUpdateCount, createOrUpdateCount)
			assert.Equal(t, test.expectedError, err != nil)

			deletedCount := 0
			for _, deleted := range deletedPips {
				if deleted {
					deletedCount++
				}
			}
			assert.Equal(t, test.expectedDeleteCount, deletedCount)
		})
	}
}

func compareStrings(s0, s1 []string) bool {
	ss0 := sets.NewString(s0...)
	ss1 := sets.NewString(s1...)
	return ss0.Equal(ss1)
}

func TestEnsurePublicIPExistsCommon(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc                    string
		pipName                 string
		inputDNSLabel           string
		expectedID              string
		additionalAnnotations   map[string]string
		existingPIPs            []network.PublicIPAddress
		expectedPIP             *network.PublicIPAddress
		foundDNSLabelAnnotation bool
		isIPv6                  bool
		useSLB                  bool
		shouldPutPIP            bool
		expectedError           bool
	}{
		{
			desc:         "shall return existed IPv4 PIP if there is any",
			pipName:      "pip1",
			existingPIPs: []network.PublicIPAddress{{Name: pointer.String("pip1")}},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   network.IPv4,
					PublicIPAllocationMethod: network.Static,
				},
				Tags: map[string]*string{},
			},
			shouldPutPIP: true,
		},
		{
			desc:         "shall return existed IPv6 PIP if there is any",
			pipName:      "pip1-IPv6",
			existingPIPs: []network.PublicIPAddress{{Name: pointer.String("pip1-IPv6")}},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1-IPv6"),
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1-IPv6"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   network.IPv6,
					PublicIPAllocationMethod: network.Dynamic,
				},
				Tags: map[string]*string{},
			},
			isIPv6:       true,
			shouldPutPIP: true,
		},
		{
			desc:    "shall create a new pip if there is no existed pip",
			pipName: "pip1",
			expectedID: "/subscriptions/subscription/resourceGroups/rg/providers/" +
				"Microsoft.Network/publicIPAddresses/pip1",
			shouldPutPIP: true,
		},
		{
			desc:                    "shall update existed PIP's dns label",
			pipName:                 "pip1",
			inputDNSLabel:           "newdns",
			foundDNSLabelAnnotation: true,
			existingPIPs: []network.PublicIPAddress{{
				Name:                            pointer.String("pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{},
			}},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings: &network.PublicIPAddressDNSSettings{
						DomainNameLabel: pointer.String("newdns"),
					},
					PublicIPAddressVersion: network.IPv4,
				},
				Tags: map[string]*string{consts.ServiceUsingDNSKey: pointer.String("default/test1")},
			},
			shouldPutPIP: true,
		},
		{
			desc:                    "shall delete DNS from PIP if DNS label is set empty",
			pipName:                 "pip1",
			foundDNSLabelAnnotation: true,
			existingPIPs: []network.PublicIPAddress{{
				Name: pointer.String("pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings: &network.PublicIPAddressDNSSettings{
						DomainNameLabel: pointer.String("previousdns"),
					},
				},
			}},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings:            nil,
					PublicIPAddressVersion: network.IPv4,
				},
				Tags: map[string]*string{},
			},
			shouldPutPIP: true,
		},
		{
			desc:                    "shall not delete DNS from PIP if DNS label annotation is not set",
			pipName:                 "pip1",
			foundDNSLabelAnnotation: false,
			existingPIPs: []network.PublicIPAddress{{
				Name: pointer.String("pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings: &network.PublicIPAddressDNSSettings{
						DomainNameLabel: pointer.String("previousdns"),
					},
				},
			}},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings: &network.PublicIPAddressDNSSettings{
						DomainNameLabel: pointer.String("previousdns"),
					},
					PublicIPAddressVersion: network.IPv4,
				},
			},
		},
		{
			desc:                    "shall update existed PIP's dns label for IPv6",
			pipName:                 "pip1",
			inputDNSLabel:           "newdns",
			foundDNSLabelAnnotation: true,
			isIPv6:                  true,
			existingPIPs: []network.PublicIPAddress{{
				Name:                            pointer.String("pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{},
			}},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings: &network.PublicIPAddressDNSSettings{
						DomainNameLabel: pointer.String("newdns"),
					},
					PublicIPAllocationMethod: network.Dynamic,
					PublicIPAddressVersion:   network.IPv6,
				},
				Tags: map[string]*string{consts.ServiceUsingDNSKey: pointer.String("default/test1")},
			},
			shouldPutPIP: true,
		},
		{
			desc:                    "shall update existed PIP's dns label for IPv6",
			pipName:                 "pip1",
			inputDNSLabel:           "newdns",
			foundDNSLabelAnnotation: true,
			isIPv6:                  true,
			existingPIPs: []network.PublicIPAddress{{
				Name: pointer.String("pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings: &network.PublicIPAddressDNSSettings{
						DomainNameLabel: pointer.String("previousdns"),
					},
				},
			}},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings: &network.PublicIPAddressDNSSettings{
						DomainNameLabel: pointer.String("newdns"),
					},
					PublicIPAllocationMethod: network.Dynamic,
					PublicIPAddressVersion:   network.IPv6,
				},
				Tags: map[string]*string{
					"k8s-azure-dns-label-service": pointer.String("default/test1"),
				},
			},
			shouldPutPIP: true,
		},
		{
			desc:                    "shall update existed PIP's dns label for IPv4",
			pipName:                 "pip1",
			inputDNSLabel:           "newdns",
			foundDNSLabelAnnotation: true,
			isIPv6:                  false,
			existingPIPs: []network.PublicIPAddress{{

				Name: pointer.String("pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings: &network.PublicIPAddressDNSSettings{
						DomainNameLabel: pointer.String("previousdns"),
					},
					PublicIPAllocationMethod: network.Dynamic,
					PublicIPAddressVersion:   network.IPv4,
				},
			}},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings: &network.PublicIPAddressDNSSettings{
						DomainNameLabel: pointer.String("newdns"),
					},
					PublicIPAllocationMethod: network.Dynamic,
					PublicIPAddressVersion:   network.IPv4,
				},
				Tags: map[string]*string{
					"k8s-azure-dns-label-service": pointer.String("default/test1"),
				},
			},
			shouldPutPIP: true,
		},
		{
			desc:                    "shall report an conflict error if the DNS label is conflicted",
			pipName:                 "pip1",
			inputDNSLabel:           "test",
			foundDNSLabelAnnotation: true,
			existingPIPs: []network.PublicIPAddress{{
				Name: pointer.String("pip1"),
				Tags: map[string]*string{consts.ServiceUsingDNSKey: pointer.String("test1")},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings: &network.PublicIPAddressDNSSettings{
						DomainNameLabel: pointer.String("previousdns"),
					},
				},
			}},
			expectedError: true,
		},
		{
			desc:          "shall return the pip without calling PUT API if the tags are good",
			pipName:       "pip1",
			inputDNSLabel: "test",
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pip1"),
					ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
						"/providers/Microsoft.Network/publicIPAddresses/pip1"),
					Tags: map[string]*string{
						consts.ServiceUsingDNSKey: pointer.String("default/test1"),
						consts.ServiceTagKey:      pointer.String("default/test1"),
					},
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						DNSSettings: &network.PublicIPAddressDNSSettings{
							DomainNameLabel: pointer.String("test"),
						},
						PublicIPAllocationMethod: network.Static,
						PublicIPAddressVersion:   network.IPv4,
					},
				},
			},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				Tags: map[string]*string{
					consts.ServiceUsingDNSKey: pointer.String("default/test1"),
					consts.ServiceTagKey:      pointer.String("default/test1"),
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					DNSSettings: &network.PublicIPAddressDNSSettings{
						DomainNameLabel: pointer.String("test"),
					},
					PublicIPAllocationMethod: network.Static,
					PublicIPAddressVersion:   network.IPv4,
				},
			},
		},
		{
			desc:    "shall tag the service name to the pip correctly",
			pipName: "pip1",
			existingPIPs: []network.PublicIPAddress{
				{Name: pointer.String("pip1")},
			},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   network.IPv4,
					PublicIPAllocationMethod: network.Static,
				},
				Tags: map[string]*string{},
			},
			shouldPutPIP: true,
		},
		{
			desc:    "shall not call the PUT API for IPV6 pip if it is not necessary",
			pipName: "pip1",
			isIPv6:  true,
			useSLB:  true,
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pip1"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Static,
					},
				},
			},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   network.IPv6,
					PublicIPAllocationMethod: network.Static,
				},
				Tags: map[string]*string{},
			},
			shouldPutPIP: true,
		},
		{
			desc:         "shall update pip tags if there is any change",
			pipName:      "pip1",
			existingPIPs: []network.PublicIPAddress{{Name: pointer.String("pip1"), Tags: map[string]*string{"a": pointer.String("b")}}},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				Tags: map[string]*string{"a": pointer.String("c")},
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   network.IPv4,
					PublicIPAllocationMethod: network.Static,
				},
			},
			additionalAnnotations: map[string]string{
				consts.ServiceAnnotationAzurePIPTags: "a=c",
			},
			shouldPutPIP: true,
		},
		{
			desc:    "should not tag the user-assigned pip",
			pipName: "pip1",
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("pip1"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
					Tags: map[string]*string{"a": pointer.String("b")},
				},
			},
			expectedPIP: &network.PublicIPAddress{
				Name: pointer.String("pip1"),
				Tags: map[string]*string{"a": pointer.String("b")},
				ID: pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/pip1"),
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion: network.IPv4,
					IPAddress:              pointer.String("1.2.3.4"),
				},
			},
			additionalAnnotations: map[string]string{
				consts.ServiceAnnotationAzurePIPTags: "a=c",
			},
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			if test.useSLB {
				az.LoadBalancerSku = consts.LoadBalancerSkuStandard
			}

			service := getTestService("test1", v1.ProtocolTCP, nil, test.isIPv6, 80)
			service.ObjectMeta.Annotations = test.additionalAnnotations
			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			if test.shouldPutPIP {
				mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any()).DoAndReturn(func(ctx context.Context, resourceGroupName string, publicIPAddressName string, parameters network.PublicIPAddress) *retry.Error {
					if len(test.existingPIPs) != 0 {
						test.existingPIPs[0] = parameters
					} else {
						test.existingPIPs = append(test.existingPIPs, parameters)
					}
					return nil
				}).AnyTimes()
			}
			mockPIPsClient.EXPECT().Get(gomock.Any(), "rg", test.pipName, gomock.Any()).DoAndReturn(func(ctx context.Context, resourceGroupName string, publicIPAddressName string, expand string) (network.PublicIPAddress, *retry.Error) {
				return test.existingPIPs[0], nil
			}).MaxTimes(1)
			mockPIPsClient.EXPECT().List(gomock.Any(), "rg").DoAndReturn(func(ctx context.Context, resourceGroupName string) ([]network.PublicIPAddress, *retry.Error) {
				var basicPIP *network.PublicIPAddress
				if len(test.existingPIPs) == 0 {
					basicPIP = &network.PublicIPAddress{
						Name: pointer.String(test.pipName),
					}
				} else {
					basicPIP = &test.existingPIPs[0]
				}

				basicPIP.ID = pointer.String("/subscriptions/subscription/resourceGroups/rg" +
					"/providers/Microsoft.Network/publicIPAddresses/" + test.pipName)

				if basicPIP.PublicIPAddressPropertiesFormat == nil {
					return []network.PublicIPAddress{*basicPIP}, nil
				}

				if test.isIPv6 {
					basicPIP.PublicIPAddressPropertiesFormat.PublicIPAddressVersion = network.IPv6
					basicPIP.PublicIPAllocationMethod = network.Dynamic
				} else {
					basicPIP.PublicIPAddressPropertiesFormat.PublicIPAddressVersion = network.IPv4
				}

				return []network.PublicIPAddress{*basicPIP}, nil
			}).AnyTimes()

			pip, err := az.ensurePublicIPExists(&service, test.pipName, test.inputDNSLabel, "", false, test.foundDNSLabelAnnotation, test.isIPv6)
			assert.Equal(t, test.expectedError, err != nil, "unexpectedly encountered (or not) error: %v", err)
			if test.expectedID != "" {
				assert.Equal(t, test.expectedID, pointer.StringDeref(pip.ID, ""))
			} else {
				assert.Equal(t, test.expectedPIP, pip)
			}
		})
	}
}
func TestEnsurePublicIPExistsWithExtendedLocation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloudWithExtendedLocation(ctrl)
	az.LoadBalancerSku = consts.LoadBalancerSkuStandard
	service := getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80)

	exLocName := "microsoftlosangeles1"

	testcases := []struct {
		desc        string
		pipName     string
		expectedPIP *network.PublicIPAddress
		isIPv6      bool
	}{
		{
			desc:    "should create a pip with extended location",
			pipName: "pip1",
			expectedPIP: &network.PublicIPAddress{
				Name:     pointer.String("pip1"),
				Location: &az.Location,
				ExtendedLocation: &network.ExtendedLocation{
					Name: pointer.String("microsoftlosangeles1"),
					Type: network.EdgeZone,
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAllocationMethod: network.Static,
					PublicIPAddressVersion:   network.IPv4,
					ProvisioningState:        "",
				},
				Tags: map[string]*string{
					consts.ServiceTagKey:  pointer.String("default/test1"),
					consts.ClusterNameKey: pointer.String(""),
				},
			},
			isIPv6: false,
		},
		{
			desc:    "should create a pip with extended location for IPv6",
			pipName: "pip1-IPv6",
			expectedPIP: &network.PublicIPAddress{
				Name:     pointer.String("pip1-IPv6"),
				Location: &az.Location,
				ExtendedLocation: &network.ExtendedLocation{
					Name: pointer.String("microsoftlosangeles1"),
					Type: network.EdgeZone,
				},
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAllocationMethod: network.Dynamic,
					PublicIPAddressVersion:   network.IPv6,
					ProvisioningState:        "",
				},
				Tags: map[string]*string{
					consts.ServiceTagKey:  pointer.String("default/test1"),
					consts.ClusterNameKey: pointer.String(""),
				},
			},
			isIPv6: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			mockPIPsClient := az.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			first := mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return([]network.PublicIPAddress{}, nil).Times(2)
			mockPIPsClient.EXPECT().Get(gomock.Any(), "rg", tc.pipName, gomock.Any()).Return(*tc.expectedPIP, nil).After(first)

			mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", tc.pipName, gomock.Any()).
				DoAndReturn(func(ctx context.Context, resourceGroupName string, publicIPAddressName string, publicIPAddressParameters network.PublicIPAddress) *retry.Error {
					assert.NotNil(t, publicIPAddressParameters)
					assert.NotNil(t, publicIPAddressParameters.ExtendedLocation)
					assert.Equal(t, *publicIPAddressParameters.ExtendedLocation.Name, exLocName)
					assert.Equal(t, publicIPAddressParameters.ExtendedLocation.Type, network.EdgeZone)
					// Edge zones don't support availability zones.
					assert.Nil(t, publicIPAddressParameters.Zones)
					return nil
				}).Times(1)
			pip, err := az.ensurePublicIPExists(&service, tc.pipName, "", "", false, false, tc.isIPv6)
			assert.NotNil(t, pip, "ensurePublicIPExists shall create a new pip"+
				"with extendedLocation if there is no existing pip")
			assert.Nil(t, err, "ensurePublicIPExists should create a new pip without errors.")
		})
	}
}

func TestShouldUpdateLoadBalancer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc                   string
		lbHasDeletionTimestamp bool
		serviceType            v1.ServiceType
		existsLb               bool
		expectedOutput         bool
	}{
		{
			desc:                   "should update a load balancer that does not have a deletion timestamp and exists in Azure",
			lbHasDeletionTimestamp: false,
			serviceType:            v1.ServiceTypeLoadBalancer,
			existsLb:               true,
			expectedOutput:         true,
		},
		{
			desc:                   "should not update a load balancer that is being deleted / already deleted in K8s",
			lbHasDeletionTimestamp: true,
			serviceType:            v1.ServiceTypeLoadBalancer,
			existsLb:               true,
			expectedOutput:         false,
		},
		{
			desc:                   "should not update a load balancer that is no longer LoadBalancer type in K8s",
			lbHasDeletionTimestamp: false,
			serviceType:            v1.ServiceTypeClusterIP,
			existsLb:               true,
			expectedOutput:         false,
		},
		{
			desc:                   "should not update a load balancer that does not exist in Azure",
			lbHasDeletionTimestamp: false,
			serviceType:            v1.ServiceTypeLoadBalancer,
			existsLb:               false,
			expectedOutput:         false,
		},
		{
			desc:                   "should not update a load balancer that has a deletion timestamp and does not exist in Azure",
			lbHasDeletionTimestamp: true,
			serviceType:            v1.ServiceTypeLoadBalancer,
			existsLb:               false,
			expectedOutput:         false,
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			service := getTestService("test1", v1.ProtocolTCP, nil, false, 80)
			v4Enabled, v6Enabled := getIPFamiliesEnabled(&service)
			service.Spec.Type = test.serviceType
			setMockPublicIPs(az, ctrl, 1, v4Enabled, v6Enabled)
			mockLBsClient := mockloadbalancerclient.NewMockInterface(ctrl)
			az.LoadBalancerClient = mockLBsClient
			if test.existsLb {
				mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
			}
			if test.lbHasDeletionTimestamp {
				service.ObjectMeta.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			}
			if test.existsLb {
				lb := network.LoadBalancer{
					Name: pointer.String("vmas"),
					LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
						FrontendIPConfigurations: &[]network.FrontendIPConfiguration{
							{
								Name: pointer.String("atest1"),
								FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
									PublicIPAddress: &network.PublicIPAddress{ID: pointer.String("testCluster-aservice1")},
								},
							},
						},
					},
				}
				err := az.LoadBalancerClient.CreateOrUpdate(context.TODO(), "rg", *lb.Name, lb, "")
				assert.NoError(t, err.Error())
				mockLBsClient.EXPECT().List(gomock.Any(), "rg").Return([]network.LoadBalancer{lb}, nil)
			} else {
				mockLBsClient.EXPECT().List(gomock.Any(), "rg").Return(nil, nil).Times(2)
			}

			existingNodes := []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "vmas-1"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "vmas-2",
						Labels: map[string]string{consts.NodeLabelRole: "master"},
					},
				},
			}

			mockVMSet := NewMockVMSet(ctrl)
			mockVMSet.EXPECT().GetAgentPoolVMSetNames(gomock.Any()).Return(&[]string{"vmas"}, nil).MaxTimes(1)
			mockVMSet.EXPECT().GetPrimaryVMSetName().Return(az.Config.PrimaryAvailabilitySetName).Times(2)
			az.VMSet = mockVMSet

			shouldUpdateLoadBalancer, err := az.shouldUpdateLoadBalancer(testClusterName, &service, existingNodes)
			assert.NoError(t, err)
			assert.Equal(t, test.expectedOutput, shouldUpdateLoadBalancer)
		})
	}
}

func TestIsBackendPoolPreConfigured(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc                                      string
		preConfiguredBackendPoolLoadBalancerTypes string
		isInternalService                         bool
		expectedOutput                            bool
	}{
		{
			desc: "should return true when preConfiguredBackendPoolLoadBalancerTypes is both for any case",
			preConfiguredBackendPoolLoadBalancerTypes: "all",
			isInternalService:                         true,
			expectedOutput:                            true,
		},
		{
			desc: "should return true when preConfiguredBackendPoolLoadBalancerTypes is both for any case",
			preConfiguredBackendPoolLoadBalancerTypes: "all",
			isInternalService:                         false,
			expectedOutput:                            true,
		},
		{
			desc: "should return true when preConfiguredBackendPoolLoadBalancerTypes is external when creating external lb",
			preConfiguredBackendPoolLoadBalancerTypes: "external",
			isInternalService:                         false,
			expectedOutput:                            true,
		},
		{
			desc: "should return false when preConfiguredBackendPoolLoadBalancerTypes is external when creating internal lb",
			preConfiguredBackendPoolLoadBalancerTypes: "external",
			isInternalService:                         true,
			expectedOutput:                            false,
		},
		{
			desc: "should return false when preConfiguredBackendPoolLoadBalancerTypes is internal when creating external lb",
			preConfiguredBackendPoolLoadBalancerTypes: "internal",
			isInternalService:                         false,
			expectedOutput:                            false,
		},
		{
			desc: "should return true when preConfiguredBackendPoolLoadBalancerTypes is internal when creating internal lb",
			preConfiguredBackendPoolLoadBalancerTypes: "internal",
			isInternalService:                         true,
			expectedOutput:                            true,
		},
		{
			desc: "should return false when preConfiguredBackendPoolLoadBalancerTypes is empty for any case",
			preConfiguredBackendPoolLoadBalancerTypes: "",
			isInternalService:                         true,
			expectedOutput:                            false,
		},
		{
			desc: "should return false when preConfiguredBackendPoolLoadBalancerTypes is empty for any case",
			preConfiguredBackendPoolLoadBalancerTypes: "",
			isInternalService:                         false,
			expectedOutput:                            false,
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			az.Config.PreConfiguredBackendPoolLoadBalancerTypes = test.preConfiguredBackendPoolLoadBalancerTypes
			var service v1.Service
			if test.isInternalService {
				service = getInternalTestService("test", 80)
			} else {
				service = getTestService("test", v1.ProtocolTCP, nil, false, 80)
			}

			isPreConfigured := az.isBackendPoolPreConfigured(&service)
			assert.Equal(t, test.expectedOutput, isPreConfigured)
		})
	}
}

func TestParsePIPServiceTag(t *testing.T) {
	tags := []*string{
		pointer.String("ns1/svc1,ns2/svc2"),
		pointer.String(" ns1/svc1, ns2/svc2 "),
		pointer.String("ns1/svc1,"),
		pointer.String(""),
		nil,
	}
	expectedNames := [][]string{
		{"ns1/svc1", "ns2/svc2"},
		{"ns1/svc1", "ns2/svc2"},
		{"ns1/svc1"},
		{},
		{},
	}

	for i, tag := range tags {
		names := parsePIPServiceTag(tag)
		assert.Equal(t, expectedNames[i], names)
	}
}

func TestBindServicesToPIP(t *testing.T) {
	pips := []*network.PublicIPAddress{
		{Tags: nil},
		{Tags: map[string]*string{}},
		{Tags: map[string]*string{consts.ServiceTagKey: pointer.String("ns1/svc1")}},
		{Tags: map[string]*string{consts.ServiceTagKey: pointer.String("ns1/svc1,ns2/svc2")}},
		{Tags: map[string]*string{consts.ServiceTagKey: pointer.String("ns2/svc2,ns3/svc3")}},
	}
	serviceNames := []string{"ns2/svc2", "ns3/svc3"}
	expectedTags := []map[string]*string{
		{consts.ServiceTagKey: pointer.String("ns2/svc2,ns3/svc3")},
		{consts.ServiceTagKey: pointer.String("ns2/svc2,ns3/svc3")},
		{consts.ServiceTagKey: pointer.String("ns1/svc1,ns2/svc2,ns3/svc3")},
		{consts.ServiceTagKey: pointer.String("ns1/svc1,ns2/svc2,ns3/svc3")},
		{consts.ServiceTagKey: pointer.String("ns2/svc2,ns3/svc3")},
	}

	flags := []bool{true, true, true, true, false}

	for i, pip := range pips {
		addedNew, _ := bindServicesToPIP(pip, serviceNames, false)
		assert.Equal(t, expectedTags[i], pip.Tags)
		assert.Equal(t, flags[i], addedNew)
	}
}

func TestUnbindServiceFromPIP(t *testing.T) {
	pips := []*network.PublicIPAddress{
		{Tags: nil},
		{Tags: map[string]*string{consts.ServiceTagKey: pointer.String("")}},
		{Tags: map[string]*string{consts.ServiceTagKey: pointer.String("ns1/svc1")}},
		{Tags: map[string]*string{consts.ServiceTagKey: pointer.String("ns1/svc1,ns2/svc2")}},
	}
	serviceName := "ns2/svc2"
	service := getTestService(serviceName, v1.ProtocolTCP, nil, false, 80)
	setServiceLoadBalancerIP(&service, "1.2.3.4")
	expectedTags := []map[string]*string{
		nil,
		{consts.ServiceTagKey: pointer.String("")},
		{consts.ServiceTagKey: pointer.String("ns1/svc1")},
		{consts.ServiceTagKey: pointer.String("ns1/svc1")},
	}

	for i, pip := range pips {
		_ = unbindServiceFromPIP(pip, &service, serviceName, "", false)
		assert.Equal(t, expectedTags[i], pip.Tags)
	}
}

func TestIsFrontendIPConfigIsUnsafeToDelete(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	service := getTestService("service1", v1.ProtocolTCP, nil, false, 80)
	az := GetTestCloud(ctrl)
	fipID := pointer.String("fip")

	testCases := []struct {
		desc       string
		existingLB *network.LoadBalancer
		unsafe     bool
	}{
		{
			desc: "isFrontendIPConfigUnsafeToDelete should return true if there is a " +
				"loadBalancing rule from other service referencing the frontend IP config",
			existingLB: &network.LoadBalancer{
				Name: pointer.String("lb"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					LoadBalancingRules: &[]network.LoadBalancingRule{
						{
							Name: pointer.String("aservice2-rule"),
							LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip")},
							},
						},
					},
				},
			},
			unsafe: true,
		},
		{
			desc: "isFrontendIPConfigUnsafeToDelete should return true if there is a " +
				"outbound rule referencing the frontend IP config",
			existingLB: &network.LoadBalancer{
				Name: pointer.String("lb"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					OutboundRules: &[]network.OutboundRule{
						{
							Name: pointer.String("aservice1-rule"),
							OutboundRulePropertiesFormat: &network.OutboundRulePropertiesFormat{
								FrontendIPConfigurations: &[]network.SubResource{
									{ID: pointer.String("fip")},
								},
							},
						},
					},
				},
			},
			unsafe: true,
		},
		{
			desc: "isFrontendIPConfigUnsafeToDelete should return false if there is a " +
				"loadBalancing rule from this service referencing the frontend IP config",
			existingLB: &network.LoadBalancer{
				Name: pointer.String("lb"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					LoadBalancingRules: &[]network.LoadBalancingRule{
						{
							Name: pointer.String("aservice1-rule"),
							LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip")},
							},
						},
					},
				},
			},
		},
		{
			desc: "isFrontendIPConfigUnsafeToDelete should return true if there is a " +
				"inbound NAT rule referencing the frontend IP config",
			existingLB: &network.LoadBalancer{
				Name: pointer.String("lb"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					InboundNatRules: &[]network.InboundNatRule{
						{
							Name: pointer.String("aservice2-rule"),
							InboundNatRulePropertiesFormat: &network.InboundNatRulePropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip")},
							},
						},
					},
				},
			},
			unsafe: true,
		},
		{
			desc: "isFrontendIPConfigUnsafeToDelete should return true if there is a " +
				"inbound NAT pool referencing the frontend IP config",
			existingLB: &network.LoadBalancer{
				Name: pointer.String("lb"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					InboundNatPools: &[]network.InboundNatPool{
						{
							Name: pointer.String("aservice2-rule"),
							InboundNatPoolPropertiesFormat: &network.InboundNatPoolPropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip")},
							},
						},
					},
				},
			},
			unsafe: true,
		},
	}

	for _, testCase := range testCases {
		unsafe, _ := az.isFrontendIPConfigUnsafeToDelete(testCase.existingLB, &service, fipID)
		assert.Equal(t, testCase.unsafe, unsafe, testCase.desc)
	}
}

func TestCheckLoadBalancerResourcesConflicted(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	service := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)
	az := GetTestCloud(ctrl)

	testCases := []struct {
		desc        string
		fipID       string
		existingLB  *network.LoadBalancer
		expectedErr bool
	}{
		{
			desc: "checkLoadBalancerResourcesConflicts should report the conflict error if " +
				"there is a conflicted loadBalancing rule - IPv4",
			fipID: "fip",
			existingLB: &network.LoadBalancer{
				Name: pointer.String("lb"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					LoadBalancingRules: &[]network.LoadBalancingRule{
						{
							Name: pointer.String("aservice2-rule"),
							LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip")},
								FrontendPort:            pointer.Int32(80),
								Protocol:                network.TransportProtocol(v1.ProtocolTCP),
							},
						},
						{
							Name: pointer.String("aservice2-rule-IPv6"),
							LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip-IPv6")},
								FrontendPort:            pointer.Int32(80),
								Protocol:                network.TransportProtocol(v1.ProtocolTCP),
							},
						},
					},
				},
			},
			expectedErr: true,
		},
		{
			desc: "checkLoadBalancerResourcesConflicts should report the conflict error if " +
				"there is a conflicted loadBalancing rule - IPv6",
			fipID: "fip-IPv6",
			existingLB: &network.LoadBalancer{
				Name: pointer.String("lb"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					LoadBalancingRules: &[]network.LoadBalancingRule{
						{
							Name: pointer.String("aservice2-rule"),
							LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip")},
								FrontendPort:            pointer.Int32(80),
								Protocol:                network.TransportProtocol(v1.ProtocolTCP),
							},
						},
						{
							Name: pointer.String("aservice2-rule-IPv6"),
							LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip-IPv6")},
								FrontendPort:            pointer.Int32(80),
								Protocol:                network.TransportProtocol(v1.ProtocolTCP),
							},
						},
					},
				},
			},
			expectedErr: true,
		},
		{
			desc: "checkLoadBalancerResourcesConflicts should report the conflict error if " +
				"there is a conflicted inbound NAT rule",
			fipID: "fip",
			existingLB: &network.LoadBalancer{
				Name: pointer.String("lb"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					InboundNatRules: &[]network.InboundNatRule{
						{
							Name: pointer.String("aservice1-rule"),
							InboundNatRulePropertiesFormat: &network.InboundNatRulePropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip")},
								FrontendPort:            pointer.Int32(80),
								Protocol:                network.TransportProtocol(v1.ProtocolTCP),
							},
						},
					},
				},
			},
			expectedErr: true,
		},
		{
			desc: "checkLoadBalancerResourcesConflicts should report the conflict error if " +
				"there is a conflicted inbound NAT pool",
			fipID: "fip",
			existingLB: &network.LoadBalancer{
				Name: pointer.String("lb"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					InboundNatPools: &[]network.InboundNatPool{
						{
							Name: pointer.String("aservice1-rule"),
							InboundNatPoolPropertiesFormat: &network.InboundNatPoolPropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip")},
								FrontendPortRangeStart:  pointer.Int32(80),
								FrontendPortRangeEnd:    pointer.Int32(90),
								Protocol:                network.TransportProtocol(v1.ProtocolTCP),
							},
						},
					},
				},
			},
			expectedErr: true,
		},
		{
			desc: "checkLoadBalancerResourcesConflicts should not report the conflict error if there " +
				"is no conflicted loadBalancer resources",
			fipID: "fip",
			existingLB: &network.LoadBalancer{
				Name: pointer.String("lb"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					LoadBalancingRules: &[]network.LoadBalancingRule{
						{
							Name: pointer.String("aservice2-rule"),
							LoadBalancingRulePropertiesFormat: &network.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip")},
								FrontendPort:            pointer.Int32(90),
								Protocol:                network.TransportProtocol(v1.ProtocolTCP),
							},
						},
					},
					InboundNatRules: &[]network.InboundNatRule{
						{
							Name: pointer.String("aservice1-rule"),
							InboundNatRulePropertiesFormat: &network.InboundNatRulePropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip")},
								FrontendPort:            pointer.Int32(90),
								Protocol:                network.TransportProtocol(v1.ProtocolTCP),
							},
						},
					},
					InboundNatPools: &[]network.InboundNatPool{
						{
							Name: pointer.String("aservice1-rule"),
							InboundNatPoolPropertiesFormat: &network.InboundNatPoolPropertiesFormat{
								FrontendIPConfiguration: &network.SubResource{ID: pointer.String("fip")},
								FrontendPortRangeStart:  pointer.Int32(800),
								FrontendPortRangeEnd:    pointer.Int32(900),
								Protocol:                network.TransportProtocol(v1.ProtocolTCP),
							},
						},
					},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.desc, func(t *testing.T) {
			err := az.checkLoadBalancerResourcesConflicts(testCase.existingLB, testCase.fipID, &service)
			assert.Equal(t, testCase.expectedErr, err != nil)
		})
	}
}

func buildLBWithVMIPs(clusterName string, vmIPs []string) *network.LoadBalancer {
	lb := network.LoadBalancer{
		Name: pointer.String(clusterName),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
			BackendAddressPools: &[]network.BackendAddressPool{
				{
					Name: pointer.String(clusterName),
					BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
						LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{},
					},
				},
			},
		},
	}

	for _, vmIP := range vmIPs {
		vmIP := vmIP
		*(*lb.BackendAddressPools)[0].LoadBalancerBackendAddresses = append(*(*lb.BackendAddressPools)[0].LoadBalancerBackendAddresses, network.LoadBalancerBackendAddress{
			LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
				IPAddress: &vmIP,
				VirtualNetwork: &network.SubResource{
					ID: pointer.String("vnet"),
				},
			},
		})
	}

	return &lb
}

func buildDefaultTestLB(name string, backendIPConfigs []string) network.LoadBalancer {
	expectedLB := network.LoadBalancer{
		Name: pointer.String(name),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
			BackendAddressPools: &[]network.BackendAddressPool{
				{
					Name: pointer.String(name),
					BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
						BackendIPConfigurations: &[]network.InterfaceIPConfiguration{},
					},
				},
			},
		},
	}
	backendIPConfigurations := make([]network.InterfaceIPConfiguration, 0)
	for _, ipConfig := range backendIPConfigs {
		backendIPConfigurations = append(backendIPConfigurations, network.InterfaceIPConfiguration{ID: pointer.String(ipConfig)})
	}
	(*expectedLB.BackendAddressPools)[0].BackendIPConfigurations = &backendIPConfigurations
	return expectedLB
}

func TestEnsurePIPTagged(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.Tags = "a=x,y=z"

	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				consts.ServiceAnnotationAzurePIPTags: "A=b,c=d,e=,=f,ghi",
			},
		},
	}
	pip := network.PublicIPAddress{
		Tags: map[string]*string{
			consts.ClusterNameKey:     pointer.String("testCluster"),
			consts.ServiceTagKey:      pointer.String("default/svc1,default/svc2"),
			consts.ServiceUsingDNSKey: pointer.String("default/svc1"),
			"foo":                     pointer.String("bar"),
			"a":                       pointer.String("j"),
			"m":                       pointer.String("n"),
		},
	}

	t.Run("ensurePIPTagged should ensure the pip is tagged as configured", func(t *testing.T) {
		expectedPIP := network.PublicIPAddress{
			Tags: map[string]*string{
				consts.ClusterNameKey:     pointer.String("testCluster"),
				consts.ServiceTagKey:      pointer.String("default/svc1,default/svc2"),
				consts.ServiceUsingDNSKey: pointer.String("default/svc1"),
				"foo":                     pointer.String("bar"),
				"a":                       pointer.String("b"),
				"c":                       pointer.String("d"),
				"y":                       pointer.String("z"),
				"m":                       pointer.String("n"),
				"e":                       pointer.String(""),
			},
		}
		changed := cloud.ensurePIPTagged(&service, &pip)
		assert.True(t, changed)
		assert.Equal(t, expectedPIP, pip)
	})

	t.Run("ensurePIPTagged should delete the old tags if the SystemTags is set", func(t *testing.T) {
		cloud.SystemTags = "a,foo"
		expectedPIP := network.PublicIPAddress{
			Tags: map[string]*string{
				consts.ClusterNameKey:     pointer.String("testCluster"),
				consts.ServiceTagKey:      pointer.String("default/svc1,default/svc2"),
				consts.ServiceUsingDNSKey: pointer.String("default/svc1"),
				"foo":                     pointer.String("bar"),
				"a":                       pointer.String("b"),
				"c":                       pointer.String("d"),
				"y":                       pointer.String("z"),
				"e":                       pointer.String(""),
			},
		}
		changed := cloud.ensurePIPTagged(&service, &pip)
		assert.True(t, changed)
		assert.Equal(t, expectedPIP, pip)
	})

	t.Run("ensurePIPTagged should support TagsMap", func(t *testing.T) {
		cloud.SystemTags = "a,foo"
		cloud.TagsMap = map[string]string{"a": "c", "a=b": "c=d", "Y": "zz"}
		expectedPIP := network.PublicIPAddress{
			Tags: map[string]*string{
				consts.ClusterNameKey:     pointer.String("testCluster"),
				consts.ServiceTagKey:      pointer.String("default/svc1,default/svc2"),
				consts.ServiceUsingDNSKey: pointer.String("default/svc1"),
				"foo":                     pointer.String("bar"),
				"a":                       pointer.String("b"),
				"c":                       pointer.String("d"),
				"a=b":                     pointer.String("c=d"),
				"e":                       pointer.String(""),
			},
		}
		changed := cloud.ensurePIPTagged(&service, &pip)
		assert.True(t, changed)
		assert.Equal(t, expectedPIP, pip)
	})
}

func TestEnsureLoadBalancerTagged(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description               string
		existedTags, expectedTags map[string]*string
		newTags, systemTags       string
		expectedChanged           bool
	}{
		{
			description:     "ensureLoadBalancerTagged should not delete the old tags if SystemTags is not specified",
			existedTags:     map[string]*string{"a": pointer.String("b")},
			newTags:         "c=d",
			expectedTags:    map[string]*string{"a": pointer.String("b"), "c": pointer.String("d")},
			expectedChanged: true,
		},
		{
			description:     "ensureLoadBalancerTagged should delete the old tags if SystemTags is specified",
			existedTags:     map[string]*string{"a": pointer.String("b"), "c": pointer.String("d"), "h": pointer.String("i")},
			newTags:         "c=e,f=g",
			systemTags:      "a,x,y,z",
			expectedTags:    map[string]*string{"a": pointer.String("b"), "c": pointer.String("e"), "f": pointer.String("g")},
			expectedChanged: true,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.Tags = tc.newTags
			cloud.SystemTags = tc.systemTags
			lb := &network.LoadBalancer{Tags: tc.existedTags}

			changed := cloud.ensureLoadBalancerTagged(lb)
			assert.Equal(t, tc.expectedChanged, changed)
			assert.Equal(t, tc.expectedTags, lb.Tags)
		})
	}
}

func TestShouldChangeLoadBalancer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)
	cloud.LoadBalancerSku = consts.LoadBalancerSkuBasic

	t.Run("shouldChangeLoadBalancer should return true if the mode is different from the current vm set", func(t *testing.T) {
		annotations := map[string]string{
			consts.ServiceAnnotationLoadBalancerMode: "as2",
		}
		service := getTestService("service1", v1.ProtocolTCP, annotations, false, 80)
		res := cloud.shouldChangeLoadBalancer(&service, "as1", "testCluster")
		assert.True(t, res)
	})

	t.Run("shouldChangeLoadBalancer should return false if the current lb is the primary slb and the vmSet selected by annotation is sharing the primary slb", func(t *testing.T) {
		cloud.LoadBalancerSku = consts.LoadBalancerSkuStandard
		cloud.EnableMultipleStandardLoadBalancers = true
		cloud.NodePoolsWithoutDedicatedSLB = "vmss-1,vmss2"

		annotations := map[string]string{
			consts.ServiceAnnotationLoadBalancerMode: "vmss-1",
		}
		service := getTestService("service1", v1.ProtocolTCP, annotations, false, 80)
		res := cloud.shouldChangeLoadBalancer(&service, "testCluster-internal", "testCluster")
		assert.False(t, res)
	})

	t.Run("shouldChangeLoadBalancer should return true if the mode is the same as the current LB but the vmSet is the primary one", func(t *testing.T) {
		cloud.LoadBalancerSku = consts.LoadBalancerSkuStandard
		cloud.EnableMultipleStandardLoadBalancers = true
		cloud.PrimaryAvailabilitySetName = "vmss-1"
		annotations := map[string]string{
			consts.ServiceAnnotationLoadBalancerMode: "vmss-1",
		}
		service := getTestService("service1", v1.ProtocolTCP, annotations, false, 80)
		res := cloud.shouldChangeLoadBalancer(&service, "vmss-1", "testCluster")
		assert.True(t, res)
	})
}

func TestRemoveFrontendIPConfigurationFromLoadBalancerDelete(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	t.Run("removeFrontendIPConfigurationFromLoadBalancer should remove the unwanted frontend IP configuration and delete the orphaned LB", func(t *testing.T) {
		fip := &network.FrontendIPConfiguration{
			Name: pointer.String("testCluster"),
			ID:   pointer.String("testCluster-fip"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{
					ID: pointer.String("pipID"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
					},
				},
				PrivateIPAddressVersion: network.IPv4,
			},
		}
		service := getTestService("svc1", v1.ProtocolTCP, nil, false, 80)
		lb := getTestLoadBalancer(pointer.String("lb"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("testCluster"), service, "standard")
		bid := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-0/ipConfigurations/ipconfig1"
		lb.BackendAddressPools = &[]network.BackendAddressPool{
			{
				Name: pointer.String("testCluster"),
				BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
					BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
						{ID: pointer.String(bid)},
					},
				},
			},
		}
		cloud := GetTestCloud(ctrl)
		mockLBClient := cloud.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
		mockLBClient.EXPECT().Delete(gomock.Any(), "rg", "lb").Return(nil)
		expectedPLS := make([]network.PrivateLinkService, 0)
		mockPLSClient := cloud.PrivateLinkServiceClient.(*mockprivatelinkserviceclient.MockInterface)
		mockPLSClient.EXPECT().List(gomock.Any(), "rg").Return(expectedPLS, nil).MaxTimes(1)
		existingLBs := []network.LoadBalancer{{Name: pointer.String("lb")}}
		err := cloud.removeFrontendIPConfigurationFromLoadBalancer(&lb, existingLBs, []*network.FrontendIPConfiguration{fip}, "testCluster", &service)
		assert.NoError(t, err)
	})
}

func TestRemoveFrontendIPConfigurationFromLoadBalancerUpdate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	t.Run("removeFrontendIPConfigurationFromLoadBalancer should remove the unwanted frontend IP configuration and update the LB if there are remaining frontend IP configurations", func(t *testing.T) {
		fip := &network.FrontendIPConfiguration{
			Name: pointer.String("testCluster"),
			ID:   pointer.String("testCluster-fip"),
			FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &network.PublicIPAddress{
					ID: pointer.String("pipID"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
					},
				},
				PrivateIPAddressVersion: network.IPv4,
			},
		}
		service := getTestService("svc1", v1.ProtocolTCP, nil, false, 80)
		lb := getTestLoadBalancer(pointer.String("lb"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("testCluster"), service, "standard")
		*lb.FrontendIPConfigurations = append(*lb.FrontendIPConfigurations, network.FrontendIPConfiguration{Name: pointer.String("fip1")})
		cloud := GetTestCloud(ctrl)
		mockLBClient := cloud.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
		mockLBClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "lb", gomock.Any(), gomock.Any()).Return(nil)
		expectedPLS := make([]network.PrivateLinkService, 0)
		mockPLSClient := cloud.PrivateLinkServiceClient.(*mockprivatelinkserviceclient.MockInterface)
		mockPLSClient.EXPECT().List(gomock.Any(), "rg").Return(expectedPLS, nil).MaxTimes(1)
		err := cloud.removeFrontendIPConfigurationFromLoadBalancer(&lb, []network.LoadBalancer{}, []*network.FrontendIPConfiguration{fip}, "testCluster", &service)
		assert.NoError(t, err)
	})
}

func TestCleanOrphanedLoadBalancerLBInUseByVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	t.Run("cleanOrphanedLoadBalancer should retry deleting lb when meeting LoadBalancerInUseByVirtualMachineScaleSet", func(t *testing.T) {
		cloud := GetTestCloud(ctrl)
		vmss, err := newScaleSet(context.Background(), cloud)
		assert.NoError(t, err)
		cloud.VMSet = vmss
		cloud.LoadBalancerSku = consts.LoadBalancerSkuStandard

		mockLBClient := cloud.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
		mockLBClient.EXPECT().Delete(gomock.Any(), "rg", "test").Return(&retry.Error{RawError: errors.New(LBInUseRawError)})
		mockLBClient.EXPECT().Delete(gomock.Any(), "rg", "test").Return(nil)

		expectedVMSS := buildTestVMSSWithLB(testVMSSName, "vmss-vm-", []string{testLBBackendpoolID0}, false)
		mockVMSSClient := cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), "rg").Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil)

		service := getTestService("test", v1.ProtocolTCP, nil, false, 80)
		lb := getTestLoadBalancer(pointer.String("test"), pointer.String("rg"), pointer.String("test"), pointer.String("test"), service, consts.LoadBalancerSkuStandard)
		(*lb.BackendAddressPools)[0].ID = pointer.String(testLBBackendpoolID0)

		existingLBs := []network.LoadBalancer{{Name: pointer.String("test")}}

		err = cloud.cleanOrphanedLoadBalancer(&lb, existingLBs, &service, "test")
		assert.NoError(t, err)
	})

	t.Run("cleanupOrphanedLoadBalancer should not call delete api if the lb does not exist", func(t *testing.T) {
		cloud := GetTestCloud(ctrl)
		vmss, err := newScaleSet(context.Background(), cloud)
		assert.NoError(t, err)
		cloud.VMSet = vmss
		cloud.LoadBalancerSku = consts.LoadBalancerSkuStandard

		service := getTestService("test", v1.ProtocolTCP, nil, false, 80)
		lb := getTestLoadBalancer(pointer.String("test"), pointer.String("rg"), pointer.String("test"), pointer.String("test"), service, consts.LoadBalancerSkuStandard)
		(*lb.BackendAddressPools)[0].ID = pointer.String(testLBBackendpoolID0)

		existingLBs := []network.LoadBalancer{}

		err = cloud.cleanOrphanedLoadBalancer(&lb, existingLBs, &service, "test")
		assert.NoError(t, err)
	})
}

func TestReconcileZonesForFrontendIPConfigs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description               string
		service                   v1.Service
		existingFrontendIPConfigs []network.FrontendIPConfiguration
		existingPIPV4             network.PublicIPAddress
		existingPIPV6             network.PublicIPAddress
		status                    *v1.LoadBalancerStatus
		getZoneError              *retry.Error
		regionZonesMap            map[string][]string
		expectedZones             *[]string
		expectedDirty             bool
		expectedIPv4              *string
		expectedIPv6              *string
		expectedErr               error
	}{
		{
			description:               "reconcileFrontendIPConfigs should reconcile the zones for the new fip config",
			service:                   getTestServiceDualStack("test", v1.ProtocolTCP, nil, 80),
			existingFrontendIPConfigs: []network.FrontendIPConfiguration{},
			existingPIPV4:             network.PublicIPAddress{Name: pointer.String("testCluster-atest"), Location: pointer.String("eastus")},
			existingPIPV6:             network.PublicIPAddress{Name: pointer.String("testCluster-atest-IPv6"), Location: pointer.String("eastus")},
			regionZonesMap:            map[string][]string{"westus": {"1", "2", "3"}, "eastus": {"1", "2"}},
			expectedDirty:             true,
		},
		{
			description:               "reconcileFrontendIPConfigs should reconcile the zones for the new internal fip config",
			service:                   getInternalTestServiceDualStack("test", 80),
			existingFrontendIPConfigs: []network.FrontendIPConfiguration{},
			existingPIPV4:             network.PublicIPAddress{Name: pointer.String("testCluster-atest"), Location: pointer.String("eastus")},
			existingPIPV6:             network.PublicIPAddress{Name: pointer.String("testCluster-atest-IPv6"), Location: pointer.String("eastus")},
			regionZonesMap:            map[string][]string{"westus": {"1", "2", "3"}, "eastus": {"1", "2"}},
			expectedZones:             &[]string{"1", "2", "3"},
			expectedDirty:             true,
		},
		{
			description:  "reconcileFrontendIPConfigs should report an error if failed to get zones",
			service:      getInternalTestServiceDualStack("test", 80),
			getZoneError: retry.NewError(false, errors.New("get zone failed")),
			expectedErr:  errors.New("get zone failed"),
		},
		{
			description: "reconcileFrontendIPConfigs should use the nil zones of the existing frontend",
			service: getTestServiceWithAnnotation("test", map[string]string{
				consts.ServiceAnnotationLoadBalancerInternalSubnet: "subnet",
				consts.ServiceAnnotationLoadBalancerInternal:       consts.TrueAnnotationValue}, true, 80),
			existingFrontendIPConfigs: []network.FrontendIPConfiguration{
				{
					Name: pointer.String("atest1"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						Subnet: &network.Subnet{
							Name: pointer.String("subnet-1"),
						},
					},
				},
			},
			expectedDirty: true,
		},
		{
			description: "reconcileFrontendIPConfigs should use the non-nil zones of the existing frontend",
			service: getTestServiceWithAnnotation("test", map[string]string{
				consts.ServiceAnnotationLoadBalancerInternalSubnet: "subnet",
				consts.ServiceAnnotationLoadBalancerInternal:       consts.TrueAnnotationValue}, true, 80),
			existingFrontendIPConfigs: []network.FrontendIPConfiguration{
				{
					Name: pointer.String("not-this-one"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						Subnet: &network.Subnet{
							Name: pointer.String("subnet-1"),
						},
					},
					Zones: &[]string{"2"},
				},
				{
					Name: pointer.String("atest1"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						Subnet: &network.Subnet{
							Name: pointer.String("subnet-1"),
						},
					},
					Zones: &[]string{"1"},
				},
			},
			expectedZones: &[]string{"1"},
			expectedDirty: true,
		},
		{
			description: "reconcileFrontendIPConfigs should reuse the existing private IP for internal services when subnet does not change",
			service:     getInternalTestServiceDualStack("test", 80),
			status: &v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "1.2.3.4"},
					{IP: "2001::1"},
				},
			},
			expectedIPv4:  pointer.String("1.2.3.4"),
			expectedIPv6:  pointer.String("2001::1"),
			expectedDirty: true,
		},
		{
			description: "reconcileFrontendIPConfigs should not reuse the existing private IP for internal services when subnet changes",
			service:     getInternalTestServiceDualStack("test", 80),
			status: &v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{
					{IP: "1.2.3.6"},
					{IP: "2001::3"},
				},
			},
			expectedIPv4:  pointer.String(""),
			expectedIPv6:  pointer.String(""),
			expectedDirty: true,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.regionZonesMap = tc.regionZonesMap
			cloud.LoadBalancerSku = string(network.LoadBalancerSkuNameStandard)

			lb := getTestLoadBalancer(pointer.String("lb"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("testCluster"), tc.service, "standard")
			existingFrontendIPConfigs := tc.existingFrontendIPConfigs
			lb.FrontendIPConfigurations = &existingFrontendIPConfigs

			mockPIPClient := cloud.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			firstV4 := mockPIPClient.EXPECT().List(gomock.Any(), "rg").Return([]network.PublicIPAddress{}, nil).MaxTimes(2)
			firstV6 := mockPIPClient.EXPECT().List(gomock.Any(), "rg").Return([]network.PublicIPAddress{}, nil).MaxTimes(2)
			mockPIPClient.EXPECT().Get(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(tc.existingPIPV4, nil).MaxTimes(1).After(firstV4)
			mockPIPClient.EXPECT().Get(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(tc.existingPIPV6, nil).MaxTimes(1).After(firstV6)
			mockPIPClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(nil).MaxTimes(2)

			subnetClient := cloud.SubnetsClient.(*mocksubnetclient.MockInterface)
			subnetClient.EXPECT().Get(gomock.Any(), "rg", "vnet", "subnet", gomock.Any()).Return(
				network.Subnet{ID: pointer.String("subnet0"), SubnetPropertiesFormat: &network.SubnetPropertiesFormat{AddressPrefixes: &[]string{"1.2.3.4/31", "2001::1/127"}}}, nil).MaxTimes(1)

			zoneClient := mockzoneclient.NewMockInterface(ctrl)
			zoneClient.EXPECT().GetZones(gomock.Any(), gomock.Any()).Return(map[string][]string{}, tc.getZoneError).MaxTimes(2)
			cloud.ZoneClient = zoneClient

			service := tc.service
			isDualStack := isServiceDualStack(&service)
			defaultLBFrontendIPConfigName := cloud.getDefaultFrontendIPConfigName(&service)
			lbFrontendIPConfigNames := map[bool]string{
				consts.IPVersionIPv4: getResourceByIPFamily(defaultLBFrontendIPConfigName, isDualStack, consts.IPVersionIPv4),
				consts.IPVersionIPv6: getResourceByIPFamily(defaultLBFrontendIPConfigName, isDualStack, consts.IPVersionIPv6),
			}
			_, _, dirty, err := cloud.reconcileFrontendIPConfigs("testCluster", &service, &lb, tc.status, true, lbFrontendIPConfigNames)
			if tc.expectedErr == nil {
				assert.NoError(t, err)
			} else {
				assert.Contains(t, err.Error(), tc.expectedErr.Error())
			}
			assert.Equal(t, tc.expectedDirty, dirty)

			for _, fip := range *lb.FrontendIPConfigurations {
				if strings.EqualFold(pointer.StringDeref(fip.Name, ""), defaultLBFrontendIPConfigName) {
					assert.Equal(t, tc.expectedZones, fip.Zones)
				}
			}

			checkExpectedIP := func(isIPv6 bool, expectedIP *string) {
				if expectedIP != nil {
					for _, fip := range *lb.FrontendIPConfigurations {
						if strings.EqualFold(pointer.StringDeref(fip.Name, ""), lbFrontendIPConfigNames[isIPv6]) {
							assert.Equal(t, *expectedIP, pointer.StringDeref(fip.PrivateIPAddress, ""))
							if *expectedIP != "" {
								assert.Equal(t, network.Static, (*lb.FrontendIPConfigurations)[0].PrivateIPAllocationMethod)
							} else {
								assert.Equal(t, network.Dynamic, (*lb.FrontendIPConfigurations)[0].PrivateIPAllocationMethod)
							}
						}
					}
				}
			}
			checkExpectedIP(false, tc.expectedIPv4)
			checkExpectedIP(true, tc.expectedIPv6)
		})
	}
}

func TestReconcileFrontendIPConfigs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testcases := []struct {
		desc          string
		service       v1.Service
		existingFIPs  []network.FrontendIPConfiguration
		existingPIPs  []network.PublicIPAddress
		status        *v1.LoadBalancerStatus
		wantLB        bool
		expectedDirty bool
		expectedFIPs  []network.FrontendIPConfiguration
		expectedErr   error
	}{
		{
			desc:    "DualStack Service reconciles existing FIPs and does not touch others, not dirty",
			service: getTestServiceDualStack("test", v1.ProtocolTCP, nil, 80),
			existingFIPs: []network.FrontendIPConfiguration{
				{
					Name: pointer.String("fipV4"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							Name: pointer.String("pipV4"),
							PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: network.IPv4,
								IPAddress:              pointer.String("1.2.3.4"),
							},
						},
					},
				},
				{
					Name: pointer.String("fipV6"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							Name: pointer.String("pipV6"),
							PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: network.IPv6,
								IPAddress:              pointer.String("fe::1"),
							},
						},
					},
				},
				{
					Name: pointer.String("atest"),
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							ID: pointer.String("testCluster-atest-id"),
						},
					},
				},
				{
					Name: pointer.String("atest-IPv6"),
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest-IPv6"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							ID: pointer.String("testCluster-atest-id-IPv6"),
						},
					},
				},
			},
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("testCluster-atest"),
					ID:   pointer.String("testCluster-atest-id"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv4,
						PublicIPAllocationMethod: network.Static,
						IPAddress:                pointer.String("1.2.3.5"),
					},
				},
				{
					Name: pointer.String("testCluster-atest-IPv6"),
					ID:   pointer.String("testCluster-atest-id-IPv6"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Static,
						IPAddress:                pointer.String("fe::2"),
					},
				},
				{
					Name: pointer.String("pipV4"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
				{
					Name: pointer.String("pipV6"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv6,
						IPAddress:              pointer.String("fe::1"),
					},
				},
			},
			status:        nil,
			wantLB:        true,
			expectedDirty: false,
			expectedFIPs: []network.FrontendIPConfiguration{
				{
					Name: pointer.String("fipV4"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							Name: pointer.String("pipV4"),
							PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: network.IPv4,
								IPAddress:              pointer.String("1.2.3.4"),
							},
						},
					},
				},
				{
					Name: pointer.String("fipV6"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							Name: pointer.String("pipV6"),
							PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: network.IPv6,
								IPAddress:              pointer.String("fe::1"),
							},
						},
					},
				},
				{
					Name: pointer.String("atest"),
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							ID: pointer.String("testCluster-atest-id"),
						},
					},
				},
				{
					Name: pointer.String("atest-IPv6"),
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest-IPv6"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							ID: pointer.String("testCluster-atest-id-IPv6"),
						},
					},
				},
			},
		},
		{
			desc:    "DualStack Service reconciles existing FIPs, wantLB == false, but an FIP ID is empty, should return error",
			service: getTestServiceDualStack("test", v1.ProtocolTCP, nil, 80),
			existingFIPs: []network.FrontendIPConfiguration{
				{
					Name: pointer.String("atest"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							ID: pointer.String("testCluster-atest-id"),
						},
					},
				},
				{
					Name: pointer.String("atest-IPv6"),
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest-IPv6"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							ID: pointer.String("testCluster-atest-id-IPv6"),
						},
					},
				},
			},
			status:      nil,
			wantLB:      false,
			expectedErr: fmt.Errorf("isFrontendIPConfigUnsafeToDelete: incorrect parameters"),
		},
		{
			desc:    "IPv6 Service with existing IPv4 FIP",
			service: getTestService("test", v1.ProtocolTCP, nil, true, 80),
			existingFIPs: []network.FrontendIPConfiguration{
				{
					Name: pointer.String("fipV4"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							Name: pointer.String("pipV4"),
							PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: network.IPv4,
								IPAddress:              pointer.String("1.2.3.4"),
							},
						},
					},
				},
			},
			existingPIPs: []network.PublicIPAddress{
				{
					Name: pointer.String("testCluster-atest"),
					ID:   pointer.String("testCluster-atest-id"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   network.IPv6,
						PublicIPAllocationMethod: network.Static,
						IPAddress:                pointer.String("fe::1"),
					},
				},
				{
					Name: pointer.String("pipV4"),
					PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: network.IPv4,
						IPAddress:              pointer.String("1.2.3.4"),
					},
				},
			},
			status:        nil,
			wantLB:        true,
			expectedDirty: true,
			expectedFIPs: []network.FrontendIPConfiguration{
				{
					Name: pointer.String("fipV4"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							Name: pointer.String("pipV4"),
							PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: network.IPv4,
								IPAddress:              pointer.String("1.2.3.4"),
							},
						},
					},
				},
				{
					Name: pointer.String("atest"),
					ID:   pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest"),
					FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &network.PublicIPAddress{
							ID: pointer.String("testCluster-atest-id"),
						},
					},
				},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.LoadBalancerSku = string(network.LoadBalancerSkuNameStandard)

			lb := getTestLoadBalancer(pointer.String("lb"), pointer.String("rg"), pointer.String("testCluster"), pointer.String("testCluster"), tc.service, "standard")
			existingFIPs := tc.existingFIPs
			lb.FrontendIPConfigurations = &existingFIPs

			mockPIPClient := cloud.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
			mockPIPClient.EXPECT().List(gomock.Any(), "rg").Return(tc.existingPIPs, nil).MaxTimes(2)
			for _, pip := range tc.existingPIPs {
				mockPIPClient.EXPECT().Get(gomock.Any(), "rg", *pip.Name, gomock.Any()).Return(pip, nil).MaxTimes(1)
			}

			service := tc.service
			isDualStack := isServiceDualStack(&service)
			defaultLBFrontendIPConfigName := cloud.getDefaultFrontendIPConfigName(&service)
			lbFrontendIPConfigNames := map[bool]string{
				false: getResourceByIPFamily(defaultLBFrontendIPConfigName, isDualStack, false),
				true:  getResourceByIPFamily(defaultLBFrontendIPConfigName, isDualStack, true),
			}
			_, _, dirty, err := cloud.reconcileFrontendIPConfigs("testCluster", &service, &lb, tc.status, tc.wantLB, lbFrontendIPConfigNames)
			if tc.expectedErr != nil {
				assert.Equal(t, tc.expectedErr, err)
			} else {
				assert.Nil(t, err)
				assert.Equal(t, tc.expectedDirty, dirty)
				assert.Equal(t, tc.expectedFIPs, *lb.FrontendIPConfigurations)
			}
		})
	}
}

func TestReconcileIPSettings(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testcases := []struct {
		desc                     string
		sku                      string
		pip                      *network.PublicIPAddress
		service                  v1.Service
		isIPv6                   bool
		expectedChanged          bool
		expectedIPVersion        network.IPVersion
		expectedAllocationMethod network.IPAllocationMethod
	}{
		{
			desc: "correct IPv4 PIP",
			sku:  consts.LoadBalancerSkuStandard,
			pip: &network.PublicIPAddress{
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   network.IPv4,
					PublicIPAllocationMethod: network.Static,
				},
			},
			service:                  getTestService("test", v1.ProtocolTCP, nil, false, 80),
			isIPv6:                   false,
			expectedChanged:          false,
			expectedIPVersion:        network.IPv4,
			expectedAllocationMethod: network.Static,
		},
		{
			desc: "IPv4 PIP but IP version is IPv6",
			sku:  consts.LoadBalancerSkuStandard,
			pip: &network.PublicIPAddress{
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   network.IPv6,
					PublicIPAllocationMethod: network.Static,
				},
			},
			service:                  getTestService("test", v1.ProtocolTCP, nil, false, 80),
			isIPv6:                   false,
			expectedChanged:          true,
			expectedIPVersion:        network.IPv4,
			expectedAllocationMethod: network.Static,
		},
		{
			desc: "IPv6 PIP but allocation method is dynamic with standard sku",
			sku:  consts.LoadBalancerSkuStandard,
			pip: &network.PublicIPAddress{
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   network.IPv6,
					PublicIPAllocationMethod: network.Dynamic,
				},
			},
			service:                  getTestService("test", v1.ProtocolTCP, nil, true, 80),
			isIPv6:                   true,
			expectedChanged:          true,
			expectedIPVersion:        network.IPv6,
			expectedAllocationMethod: network.Static,
		},
		{
			desc: "IPv6 PIP but allocation method is static with basic sku",
			sku:  consts.LoadBalancerSkuBasic,
			pip: &network.PublicIPAddress{
				PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   network.IPv6,
					PublicIPAllocationMethod: network.Static,
				},
			},
			service:                  getTestService("test", v1.ProtocolTCP, nil, true, 80),
			isIPv6:                   true,
			expectedChanged:          true,
			expectedIPVersion:        network.IPv6,
			expectedAllocationMethod: network.Dynamic,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			az.LoadBalancerSku = tc.sku
			pip := tc.pip
			pip.Name = pointer.String("pip")
			service := tc.service
			changed := az.reconcileIPSettings(pip, &service, tc.isIPv6)
			assert.Equal(t, tc.expectedChanged, changed)
			assert.NotNil(t, pip.PublicIPAddressPropertiesFormat)
			assert.Equal(t, pip.PublicIPAddressPropertiesFormat.PublicIPAddressVersion, tc.expectedIPVersion)
			assert.Equal(t, pip.PublicIPAddressPropertiesFormat.PublicIPAllocationMethod, tc.expectedAllocationMethod)
		})
	}
}

func TestGetServiceFromPIPDNSTags(t *testing.T) {
	tests := []struct {
		desc     string
		tags     map[string]*string
		expected string
	}{
		{
			desc: "Empty string should be returned when tags are empty",
		},
		{
			desc: "Empty string should be returned when tags don't contain dns label tag",
		},
		{
			desc:     "Expected service should be returned when tags contain dns label tag",
			tags:     map[string]*string{consts.ServiceUsingDNSKey: pointer.String("test-service")},
			expected: "test-service",
		},
		{
			desc:     "Expected service should be returned when tags contain legacy dns label tag",
			tags:     map[string]*string{consts.LegacyServiceUsingDNSKey: pointer.String("test-service")},
			expected: "test-service",
		},
	}
	for i, c := range tests {
		actual := getServiceFromPIPDNSTags(c.tags)
		assert.Equal(t, actual, c.expected, "TestCase[%d]: %s", i, c.desc)
	}
}

func TestGetServiceFromPIPServiceTags(t *testing.T) {
	tests := []struct {
		desc     string
		tags     map[string]*string
		expected string
	}{
		{
			desc: "Empty string should be returned when tags are empty",
		},
		{
			desc: "Empty string should be returned when tags don't contain service tag",
		},
		{
			desc:     "Expected service should be returned when tags contain service tag",
			tags:     map[string]*string{consts.ServiceTagKey: pointer.String("test-service")},
			expected: "test-service",
		},
		{
			desc:     "Expected service should be returned when tags contain legacy service tag",
			tags:     map[string]*string{consts.LegacyServiceTagKey: pointer.String("test-service")},
			expected: "test-service",
		},
	}
	for i, c := range tests {
		actual := getServiceFromPIPServiceTags(c.tags)
		assert.Equal(t, actual, c.expected, "TestCase[%d]: %s", i, c.desc)
	}
}

func TestGetClusterFromPIPClusterTags(t *testing.T) {
	tests := []struct {
		desc     string
		tags     map[string]*string
		expected string
	}{
		{
			desc: "Empty string should be returned when tags are empty",
		},
		{
			desc: "Empty string should be returned when tags don't contain cluster name tag",
		},
		{
			desc:     "Expected service should be returned when tags contain cluster name tag",
			tags:     map[string]*string{consts.ClusterNameKey: pointer.String("test-cluster")},
			expected: "test-cluster",
		},
		{
			desc:     "Expected service should be returned when tags contain legacy cluster name tag",
			tags:     map[string]*string{consts.LegacyClusterNameKey: pointer.String("test-cluster")},
			expected: "test-cluster",
		},
	}
	for i, c := range tests {
		actual := getClusterFromPIPClusterTags(c.tags)
		assert.Equal(t, actual, c.expected, "TestCase[%d]: %s", i, c.desc)
	}
}

func TestSafeDeleteLoadBalancer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)

	testCases := []struct {
		desc                string
		expectedDeleteCall  bool
		expectedDecoupleErr error
		expectedErr         *retry.Error
	}{
		{
			desc:               "Standard SKU: should delete the load balancer",
			expectedDeleteCall: true,
			expectedErr:        nil,
		},
		{
			desc:                "Standard SKU: should not delete the load balancer if failed to ensure backend pool deleted",
			expectedDeleteCall:  false,
			expectedDecoupleErr: errors.New("error"),
			expectedErr: retry.NewError(
				false,
				fmt.Errorf("safeDeleteLoadBalancer: failed to EnsureBackendPoolDeleted: %w", errors.New("error")),
			),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
			if tc.expectedDeleteCall {
				mockLBClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(tc.expectedErr).Times(1)
			}
			mockVMSet := NewMockVMSet(ctrl)
			mockVMSet.EXPECT().EnsureBackendPoolDeleted(
				gomock.Any(),
				gomock.Any(),
				gomock.Any(),
				gomock.Any(),
				gomock.Any(),
			).Return(false, tc.expectedDecoupleErr)
			cloud.VMSet = mockVMSet
			cloud.LoadBalancerClient = mockLBClient
			svc := getTestService("svc", v1.ProtocolTCP, nil, false, 80)
			lb := network.LoadBalancer{
				Name: pointer.String("test"),
				LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
					BackendAddressPools: &[]network.BackendAddressPool{},
				},
			}
			err := cloud.safeDeleteLoadBalancer(lb, "cluster", "vmss", &svc)
			assert.Equal(t, tc.expectedErr, err)
		})
	}
}

func TestEqualSubResource(t *testing.T) {
	testcases := []struct {
		desc         string
		subResource1 *network.SubResource
		subResource2 *network.SubResource
		expected     bool
	}{
		{
			desc:         "both nil",
			subResource1: nil,
			subResource2: nil,
			expected:     true,
		},
		{
			desc:         "one nil",
			subResource1: &network.SubResource{},
			subResource2: nil,
			expected:     false,
		},
		{
			desc:         "equal",
			subResource1: &network.SubResource{ID: pointer.String("id")},
			subResource2: &network.SubResource{ID: pointer.String("id")},
			expected:     true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			equal := equalSubResource(tc.subResource1, tc.subResource2)
			assert.Equal(t, tc.expected, equal)
		})
	}
}

func newAllowRule(name string, priority int32) network.SecurityRule {
	return network.SecurityRule{
		Name: &name,
		SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
			Priority: &priority,
			Access:   network.SecurityRuleAccessAllow,
		},
	}
}
func newDenyRule(name string, priority int32) network.SecurityRule {
	return network.SecurityRule{
		Name: &name,
		SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
			Priority: &priority,
			Access:   network.SecurityRuleAccessDeny,
		},
	}
}

func Test_shouldTidySecurityRules(t *testing.T) {
	type args struct {
		rules []network.SecurityRule
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "should return false if there are no rules #1",
			args: args{
				rules: nil,
			},
			want: false,
		},
		{
			name: "should return false if there are no rules #2",
			args: args{
				rules: []network.SecurityRule{},
			},
			want: false,
		},
		{
			name: "should return false if there is 1 rule",
			args: args{
				rules: []network.SecurityRule{
					newAllowRule("rule1", 500),
				},
			},
			want: false,
		},
		{
			name: "should return false if no allow rule after deny rule",
			args: args{
				rules: []network.SecurityRule{
					newAllowRule("rule1", 500),
					newDenyRule("rule2", 505),
				},
			},
			want: false,
		},
		{
			name: "should return false if no allow rule after deny rule: ignore unmanaged rules",
			args: args{
				rules: []network.SecurityRule{
					newAllowRule("unmanaged_rule1", 300),
					newAllowRule("unmanaged_rule2", 400),
					newDenyRule("unmanaged_rule3", 450),

					newAllowRule("rule1", 500),
					newDenyRule("rule2", 505),

					newDenyRule("unmanaged_rule4", 5000),
					newAllowRule("unmanaged_rule5", 6000),
				},
			},
			want: false,
		},
		{
			name: "should return true if allow rule after deny rule",
			args: args{
				rules: []network.SecurityRule{
					newAllowRule("rule1", 500),
					newDenyRule("rule4", 501),
					newAllowRule("rule2", 502),
					newDenyRule("rule5", 503),
					newAllowRule("rule3", 504),
				},
			},
			want: true,
		},
		{
			name: "should return true if allow rule after deny rule: with unmanaged rules",
			args: args{
				rules: []network.SecurityRule{

					newAllowRule("unmanaged_rule1", 300),
					newAllowRule("unmanaged_rule2", 400),
					newDenyRule("unmanaged_rule3", 450),

					newAllowRule("rule1", 500),
					newDenyRule("rule4", 501),
					newAllowRule("rule2", 502),
					newDenyRule("rule5", 503),
					newAllowRule("rule3", 504),

					newDenyRule("unmanaged_rule4", 5000),
					newAllowRule("unmanaged_rule5", 6000),
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equalf(t, tt.want, shouldTidySecurityRules(tt.args.rules), "shouldTidySecurityRules(%v)", tt.args.rules)
		})
	}
}

func Test_tidySecurityRules(t *testing.T) {

	type args struct {
		rules []network.SecurityRule
	}
	tests := []struct {
		name string
		args args
		want []network.SecurityRule
	}{
		{
			name: "should return the same rules if there are no rules #1",
			args: args{
				rules: nil,
			},
			want: nil,
		},
		{
			name: "should return the same rules if there are no rules #2",
			args: args{
				rules: []network.SecurityRule{},
			},
			want: []network.SecurityRule{},
		},
		{
			name: "should return the same rules if there is 1 rule",
			args: args{
				rules: []network.SecurityRule{
					newAllowRule("rule1", 500),
				},
			},
			want: []network.SecurityRule{
				newAllowRule("rule1", 500),
			},
		},
		{
			name: "should return the same rules if there are only unmanaged rules",
			args: args{
				rules: []network.SecurityRule{
					newAllowRule("unmanaged_rule1", 300),
					newAllowRule("unmanaged_rule2", 400),
					newDenyRule("unmanaged_rule3", 450),

					newAllowRule("rule1", 500),

					newDenyRule("unmanaged_rule4", 5000),
					newAllowRule("unmanaged_rule5", 6000),
				},
			},
			want: []network.SecurityRule{
				newAllowRule("unmanaged_rule1", 300),
				newAllowRule("unmanaged_rule2", 400),
				newDenyRule("unmanaged_rule3", 450),

				newAllowRule("rule1", 500),

				newDenyRule("unmanaged_rule4", 5000),
				newAllowRule("unmanaged_rule5", 6000),
			},
		},
		{
			name: "should reorder managed rules #1",
			args: args{
				rules: []network.SecurityRule{
					newAllowRule("rule1", 500),
					newAllowRule("rule2", 541),
					newAllowRule("rule3", 652),
					newDenyRule("rule4", 700),
					newDenyRule("rule5", 1000),
				},
			},
			want: []network.SecurityRule{
				newAllowRule("rule1", 500),
				newAllowRule("rule2", 501),
				newAllowRule("rule3", 502),
				newDenyRule("rule5", 4095),
				newDenyRule("rule4", 4096),
			},
		},
		{
			name: "should reorder managed rules #2",
			args: args{
				rules: []network.SecurityRule{
					newAllowRule("rule1", 500),
					newDenyRule("rule4", 501),
					newAllowRule("rule2", 502),
					newDenyRule("rule5", 503),
					newAllowRule("rule3", 504),
				},
			},
			want: []network.SecurityRule{
				newAllowRule("rule1", 500),
				newAllowRule("rule2", 501),
				newAllowRule("rule3", 502),
				newDenyRule("rule5", 4095),
				newDenyRule("rule4", 4096),
			},
		},
		{
			name: "should reorder managed rules #3: ignore unmanaged rules",
			args: args{
				rules: []network.SecurityRule{
					newAllowRule("unmanaged_rule1", 300),
					newAllowRule("unmanaged_rule2", 400),
					newDenyRule("unmanaged_rule3", 450),

					newAllowRule("rule1", 500),
					newDenyRule("rule4", 501),
					newAllowRule("rule2", 502),
					newDenyRule("rule5", 503),
					newAllowRule("rule3", 504),

					newDenyRule("unmanaged_rule4", 5000),
					newAllowRule("unmanaged_rule5", 6000),
				},
			},
			want: []network.SecurityRule{
				newAllowRule("unmanaged_rule1", 300),
				newAllowRule("unmanaged_rule2", 400),
				newDenyRule("unmanaged_rule3", 450),

				newAllowRule("rule1", 500),
				newAllowRule("rule2", 501),
				newAllowRule("rule3", 502),
				newDenyRule("rule5", 4095),
				newDenyRule("rule4", 4096),

				newDenyRule("unmanaged_rule4", 5000),
				newAllowRule("unmanaged_rule5", 6000),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equalf(t, tt.want, tidySecurityRules(tt.args.rules), "tidySecurityRules(%v)", tt.args.rules)
		})
	}
}
