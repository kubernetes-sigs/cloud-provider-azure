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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/backendaddresspoolclient/mock_backendaddresspoolclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachinescalesetclient/mock_virtualmachinescalesetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/privatelinkservice"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/subnet"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/zone"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// LBInUseRawError is the LoadBalancerInUseByVirtualMachineScaleSet raw error
const LBInUseRawError = `{
	"error": {
    	"code": "LoadBalancerInUseByVirtualMachineScaleSet",
    	"message": "Cannot delete load balancer /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb since its child resources lb are in use by virtual machine scale set /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss.",
    	"details": []
  	}
}`

const (
	expectedPIPID = "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1"
	rgprefix      = "/subscriptions/subscription/resourceGroups/rg"
	ipv6Suffix    = "-IPv6"
	svcPrefix     = "aservice1-"
)

func TestExistsPip(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testcases := []struct {
		desc               string
		service            v1.Service
		expectedClientList func(client *mock_publicipaddressclient.MockInterface)
		expectedExist      bool
	}{
		{
			"IPv4 exists",
			getTestService("service", v1.ProtocolTCP, nil, false, 80),
			func(client *mock_publicipaddressclient.MockInterface) {
				pips := []*armnetwork.PublicIPAddress{
					{
						Name: ptr.To("testCluster-aservice"),
						Properties: &armnetwork.PublicIPAddressPropertiesFormat{
							IPAddress: ptr.To("1.2.3.4"),
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
			func(client *mock_publicipaddressclient.MockInterface) {
				client.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.PublicIPAddress{}, nil).MaxTimes(2)
			},
			false,
		},
		{
			"IPv6 exists",
			getTestService("service", v1.ProtocolTCP, nil, true, 80),
			func(client *mock_publicipaddressclient.MockInterface) {
				pips := []*armnetwork.PublicIPAddress{
					{
						Name: ptr.To("testCluster-aservice"),
						Properties: &armnetwork.PublicIPAddressPropertiesFormat{
							IPAddress: ptr.To("fe::1"),
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
			func(client *mock_publicipaddressclient.MockInterface) {
				pips := []*armnetwork.PublicIPAddress{
					{
						Name: ptr.To("testCluster-aservice-IPv6"),
						Properties: &armnetwork.PublicIPAddressPropertiesFormat{
							IPAddress: ptr.To("fe::1"),
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
			func(client *mock_publicipaddressclient.MockInterface) {
				pips := []*armnetwork.PublicIPAddress{
					{
						Name: ptr.To("testCluster-aservice"),
						Properties: &armnetwork.PublicIPAddressPropertiesFormat{
							IPAddress: ptr.To("1.2.3.4"),
						},
					},
					{
						Name: ptr.To("testCluster-aservice-IPv6"),
						Properties: &armnetwork.PublicIPAddressPropertiesFormat{
							IPAddress: ptr.To("fe::1"),
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
			func(client *mock_publicipaddressclient.MockInterface) {
				pips := []*armnetwork.PublicIPAddress{
					{
						Name: ptr.To("testCluster-aservice-IPv6"),
						Properties: &armnetwork.PublicIPAddressPropertiesFormat{
							IPAddress: ptr.To("fe::1"),
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
			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			tc.expectedClientList(mockPIPsClient)
			exist := az.existsPip(context.TODO(), "testCluster", &service)
			assert.Equal(t, tc.expectedExist, exist)
		})
	}
}

// TODO: Dualstack
func TestGetLoadBalancer(t *testing.T) {
	lb1 := &armnetwork.LoadBalancer{
		Name:       ptr.To("testCluster"),
		Properties: &armnetwork.LoadBalancerPropertiesFormat{},
	}
	lb2 := &armnetwork.LoadBalancer{
		Name: ptr.To("testCluster"),
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
				{
					Name: ptr.To("aservice"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("testCluster-aservice")},
					},
				},
			},
		},
	}
	lb3 := &armnetwork.LoadBalancer{
		Name: ptr.To("testCluster-internal"),
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
				{
					Name: ptr.To("aservice"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PrivateIPAddress: ptr.To("10.0.0.6"),
					},
				},
			},
		},
	}
	tests := []struct {
		desc           string
		service        v1.Service
		existingLBs    []*armnetwork.LoadBalancer
		pipExists      bool
		expectedGotLB  bool
		expectedStatus *v1.LoadBalancerStatus
	}{
		{
			desc:           "GetLoadBalancer should return true when only public IP exists",
			service:        getTestService("service", v1.ProtocolTCP, nil, false, 80),
			existingLBs:    []*armnetwork.LoadBalancer{lb1},
			pipExists:      true,
			expectedGotLB:  true,
			expectedStatus: nil,
		},
		{
			desc:           "GetLoadBalancer should return false when neither public IP nor LB exists",
			service:        getTestService("service", v1.ProtocolTCP, nil, false, 80),
			existingLBs:    []*armnetwork.LoadBalancer{lb1},
			pipExists:      false,
			expectedGotLB:  false,
			expectedStatus: nil,
		},
		{
			desc:          "GetLoadBalancer should return true when external service finds external LB",
			service:       getTestService("service", v1.ProtocolTCP, nil, false, 80),
			existingLBs:   []*armnetwork.LoadBalancer{lb2},
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
			existingLBs:   []*armnetwork.LoadBalancer{lb3},
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
			existingLBs:   []*armnetwork.LoadBalancer{lb3},
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
			existingLBs:   []*armnetwork.LoadBalancer{lb2},
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
			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			if c.pipExists {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.PublicIPAddress{
					{
						Name: ptr.To("testCluster-aservice"),
						Properties: &armnetwork.PublicIPAddressPropertiesFormat{
							IPAddress: ptr.To("1.2.3.4"),
						},
					},
				}, nil)
			} else {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.PublicIPAddress{}, nil).MaxTimes(2)
			}
			mockLBsClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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
		existingRule []*armnetwork.LoadBalancingRule
		curRule      *armnetwork.LoadBalancingRule
		expected     bool
	}{
		{
			msg:      "empty existing rules should return false",
			expected: false,
		},
		{
			msg: "rule names don't match should return false",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("httpProbe1"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						FrontendPort: ptr.To(int32(1)),
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("httpProbe2"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					FrontendPort: ptr.To(int32(1)),
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while protocols don't should return false",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("httpRule"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						Protocol: to.Ptr(armnetwork.TransportProtocolTCP),
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("httpRule"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					Protocol: to.Ptr(armnetwork.TransportProtocolUDP),
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while EnableTCPResets don't should return false",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("httpRule"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						Protocol:       to.Ptr(armnetwork.TransportProtocolTCP),
						EnableTCPReset: ptr.To(true),
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("httpRule"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					Protocol:       to.Ptr(armnetwork.TransportProtocolTCP),
					EnableTCPReset: ptr.To(false),
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while frontend ports don't should return false",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("httpProbe"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						FrontendPort: ptr.To(int32(1)),
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("httpProbe"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					FrontendPort: ptr.To(int32(2)),
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while backend ports don't should return false",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("httpProbe"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						BackendPort: ptr.To(int32(1)),
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("httpProbe"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					BackendPort: ptr.To(int32(2)),
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while idletimeout don't should return false",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("httpRule"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						IdleTimeoutInMinutes: ptr.To(int32(1)),
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("httpRule"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					IdleTimeoutInMinutes: ptr.To(int32(2)),
				},
			},
			expected: false,
		},
		{
			msg: "rule names match while idletimeout nil should return true",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name:       ptr.To("httpRule"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("httpRule"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					IdleTimeoutInMinutes: ptr.To(int32(2)),
				},
			},
			expected: true,
		},
		{
			msg: "rule names match while LoadDistribution don't should return false",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("httpRule"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						LoadDistribution: to.Ptr(armnetwork.LoadDistributionSourceIP),
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("httpRule"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					LoadDistribution: to.Ptr(armnetwork.LoadDistributionDefault),
				},
			},
			expected: false,
		},
		{
			msg: "rule and probe names match should return true",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("probe1"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						Probe: &armnetwork.SubResource{ID: ptr.To("probe")},
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("probe1"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					Probe: &armnetwork.SubResource{ID: ptr.To("probe")},
				},
			},
			expected: true,
		},
		{
			msg: "rule names match while probe don't should return false",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("probe1"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						Probe: nil,
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("probe1"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					Probe: &armnetwork.SubResource{ID: ptr.To("probe")},
				},
			},
			expected: false,
		},
		{
			msg: "both rule names and LoadBalancingRulePropertiesFormats match should return true",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("matchName"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						BackendPort:      ptr.To(int32(2)),
						FrontendPort:     ptr.To(int32(2)),
						LoadDistribution: to.Ptr(armnetwork.LoadDistributionSourceIP),
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("matchName"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					BackendPort:      ptr.To(int32(2)),
					FrontendPort:     ptr.To(int32(2)),
					LoadDistribution: to.Ptr(armnetwork.LoadDistributionSourceIP),
				},
			},
			expected: true,
		},
		{
			msg: "rule and FrontendIPConfiguration names match should return true",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("matchName"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("FrontendIPConfiguration")},
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("matchName"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("frontendipconfiguration")},
				},
			},
			expected: true,
		},
		{
			msg: "rule names match while FrontendIPConfiguration don't should return false",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("matchName"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("FrontendIPConfiguration")},
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("matchName"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("frontendipconifguration")},
				},
			},
			expected: false,
		},
		{
			msg: "rule and BackendAddressPool names match should return true",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("matchName"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						BackendAddressPool: &armnetwork.SubResource{ID: ptr.To("BackendAddressPool")},
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("matchName"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					BackendAddressPool: &armnetwork.SubResource{ID: ptr.To("backendaddresspool")},
				},
			},
			expected: true,
		},
		{
			msg: "rule and Probe names match should return true",
			existingRule: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To("matchName"),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						Probe: &armnetwork.SubResource{ID: ptr.To("Probe")},
					},
				},
			},
			curRule: &armnetwork.LoadBalancingRule{
				Name: ptr.To("matchName"),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					Probe: &armnetwork.SubResource{ID: ptr.To("probe")},
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
			expected: ptr.To("subnet"),
		},
	} {
		realValue := getInternalSubnet(c.service)
		assert.Equal(t, c.expected, realValue, fmt.Sprintf("TestCase[%d]: %s", i, c.desc))
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

	for i, c := range tests {
		i := i
		c := c
		t.Run(c.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			az := GetTestCloud(ctrl)
			mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
			mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
				return false, false, lb, nil
			}).AnyTimes()
			mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
			mockLBBackendPool.EXPECT().GetBackendPrivateIPs(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

			clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, vmCount, availabilitySetCount)
			setMockEnv(az, expectedInterfaces, expectedVirtualMachines, 5)

			service := c.service
			if c.service.Annotations[consts.ServiceAnnotationLoadBalancerInternal] == "true" {
				validateTestSubnet(t, az, &service)
			}

			expectedLBs := make([]*armnetwork.LoadBalancer, 0)
			setMockLBs(az, &expectedLBs, "service", 1, i+1, c.isInternalSvc)

			mockPLSRepo := privatelinkservice.NewMockRepository(ctrl)
			mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()
			az.plsRepo = mockPLSRepo

			// create the service first.
			lbStatus, err := az.EnsureLoadBalancer(context.TODO(), testClusterName, &service, clusterResources.nodes)
			if c.expectCreateError {
				assert.NotNil(t, err, "TestCase[%d]: %s", i, c.desc)
			} else {
				assert.Nil(t, err, "TestCase[%d]: %s", i, c.desc)
				assert.NotNil(t, lbStatus, "TestCase[%d]: %s", i, c.desc)
				result, rerr := az.NetworkClientFactory.GetLoadBalancerClient().List(context.TODO(), az.Config.ResourceGroup)
				assert.Nil(t, rerr, "TestCase[%d]: %s", i, c.desc)
				assert.Equal(t, 1, len(result), "TestCase[%d]: %s", i, c.desc)
				assert.Equal(t, 1, len(result[0].Properties.LoadBalancingRules), "TestCase[%d]: %s", i, c.desc)
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
			expectedLBs = make([]*armnetwork.LoadBalancer, 0)
			setMockLBs(az, &expectedLBs, "service", 1, i+1, c.isInternalSvc)

			err = az.EnsureLoadBalancerDeleted(context.TODO(), testClusterName, &service)
			assert.Nil(t, err, "TestCase[%d]: %s", i, c.desc)
		})
	}
}

func TestEnsureLoadBalancerLock(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	kubeClient := fake.NewSimpleClientset()
	kubeClient.PrependReactor(
		"get", "leases",
		func(_ k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, nil, errors.New("get lease failed")
		})
	az.KubeClient = kubeClient
	az.azureResourceLocker = NewAzureResourceLocker(
		az, "holder", "aks-managed-resource-locker", "kube-system", 900,
	)

	svc := getTestService("service", v1.ProtocolTCP, nil, false, 80)
	_, err := az.EnsureLoadBalancer(context.Background(), testClusterName, &svc, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "get lease failed")
	assert.Contains(t, err.Error(), "EnsureLoadBalancer failed due to fail to lock azure resources")

	kubeClient = fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "aks-managed-resource-locker",
			Namespace: "kube-system",
		},
		Spec: coordinationv1.LeaseSpec{
			LeaseDurationSeconds: ptr.To[int32](900),
		},
	})
	// update lease failed for the second time
	count := 0
	kubeClient.PrependReactor(
		"update", "leases",
		func(_ k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			count++
			if count == 2 {
				return true, nil, errors.New("update lease failed")
			}
			return false, nil, nil
		})
	az.KubeClient = kubeClient
	az.azureResourceLocker = NewAzureResourceLocker(
		az, "holder", "aks-managed-resource-locker", "kube-system", 900,
	)
	mockLBClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
	mockLBClient.EXPECT().List(gomock.Any(), gomock.Any()).
		Return(nil, &azcore.ResponseError{ErrorCode: "list lb failed"})

	_, err = az.EnsureLoadBalancer(context.Background(), testClusterName, &svc, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "update lease failed")
	assert.Contains(t, err.Error(), "list lb failed")
}

func TestEnsureLoadBalancerDeletedLock(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	kubeClient := fake.NewSimpleClientset()
	kubeClient.PrependReactor(
		"get", "leases",
		func(_ k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			return true, nil, errors.New("get lease failed")
		})
	az.KubeClient = kubeClient
	az.azureResourceLocker = NewAzureResourceLocker(
		az, "holder", "aks-managed-resource-locker", "kube-system", 900,
	)

	svc := getTestService("service", v1.ProtocolTCP, nil, false, 80)
	err := az.EnsureLoadBalancerDeleted(context.Background(), testClusterName, &svc)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "get lease failed")
	assert.Contains(t, err.Error(), "EnsureLoadBalancerDeleted failed due to fail to lock azure resources")

	kubeClient = fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "aks-managed-resource-locker",
			Namespace: "kube-system",
		},
		Spec: coordinationv1.LeaseSpec{
			LeaseDurationSeconds: ptr.To[int32](900),
		},
	})
	// update lease failed for the second time
	count := 0
	kubeClient.PrependReactor(
		"update", "leases",
		func(_ k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			count++
			if count == 2 {
				return true, nil, errors.New("update lease failed")
			}
			return false, nil, nil
		})
	az.KubeClient = kubeClient
	az.azureResourceLocker = NewAzureResourceLocker(
		az, "holder", "aks-managed-resource-locker", "kube-system", 900,
	)
	mockLBClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
	mockLBClient.EXPECT().List(gomock.Any(), gomock.Any()).
		Return(nil, &azcore.ResponseError{ErrorCode: "list lb failed"})

	err = az.EnsureLoadBalancerDeleted(context.Background(), testClusterName, &svc)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "update lease failed")
	assert.Contains(t, err.Error(), "list lb failed")
}

func TestServiceOwnsPublicIP(t *testing.T) {
	tests := []struct {
		desc                    string
		pip                     *armnetwork.PublicIPAddress
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
			pip: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Tags: map[string]*string{
					consts.ServiceTagKey: ptr.To("default/nginx"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			serviceName:  "web",
			expectedOwns: false,
		},
		{
			desc: "true should be returned when service name tag matches and cluster name tag is not set",
			pip: &armnetwork.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey: ptr.To("default/nginx"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx",
			expectedOwns: true,
		},
		{
			desc: "false should be returned when cluster name doesn't match",
			pip: &armnetwork.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey:  ptr.To("default/nginx"),
					consts.ClusterNameKey: ptr.To("kubernetes"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			clusterName:  "k8s",
			serviceName:  "nginx",
			expectedOwns: false,
		},
		{
			desc: "false should be returned when cluster name matches while service name doesn't match",
			pip: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Tags: map[string]*string{
					consts.ServiceTagKey:  ptr.To("default/web"),
					consts.ClusterNameKey: ptr.To("kubernetes"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx",
			expectedOwns: false,
		},
		{
			desc: "true should be returned when both service name tag and cluster name match",
			pip: &armnetwork.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey:  ptr.To("default/nginx"),
					consts.ClusterNameKey: ptr.To("kubernetes"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx",
			expectedOwns: true,
		},
		{
			desc: "false should be returned when the tag is empty and load balancer IP does not match",
			pip: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Tags: map[string]*string{
					consts.ServiceTagKey:  ptr.To(""),
					consts.ClusterNameKey: ptr.To("kubernetes"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			clusterName:             "kubernetes",
			serviceName:             "nginx",
			expectedOwns:            false,
			expectedUserAssignedPIP: true,
		},
		{
			desc: "true should be returned if there is a match among a multi-service tag",
			pip: &armnetwork.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey:  ptr.To("default/nginx1,default/nginx2"),
					consts.ClusterNameKey: ptr.To("kubernetes"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx1",
			expectedOwns: true,
		},
		{
			desc: "false should be returned if there is not a match among a multi-service tag",
			pip: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Tags: map[string]*string{
					consts.ServiceTagKey:  ptr.To("default/nginx1,default/nginx2"),
					consts.ClusterNameKey: ptr.To("kubernetes"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx3",
			expectedOwns: false,
		},
		{
			desc: "true should be returned if the load balancer IP is matched even if the svc name is not included in the tag",
			pip: &armnetwork.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey:  ptr.To(""),
					consts.ClusterNameKey: ptr.To("kubernetes"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
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
			pip: &armnetwork.PublicIPAddress{
				Tags: map[string]*string{
					consts.ServiceTagKey:  ptr.To("default/nginx1,default/nginx2"),
					consts.ClusterNameKey: ptr.To("kubernetes"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			clusterName:  "kubernetes",
			serviceName:  "nginx1",
			serviceLBIP:  "1.1.1.1",
			expectedOwns: true,
		},
		{
			desc: "should be user-assigned pip if it has no tags",
			pip: &armnetwork.PublicIPAddress{
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			serviceLBIP:             "1.2.3.4",
			expectedOwns:            true,
			expectedUserAssignedPIP: true,
		},
		{
			desc: "should be true if the pip name matches",
			pip: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			serviceLBName:           "pip1",
			expectedOwns:            true,
			expectedUserAssignedPIP: true,
		},
		{
			desc: "should be true if the pip with tag matches the pip name",
			pip: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Tags: map[string]*string{},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
				},
			},
			serviceLBName:           "pip1",
			expectedOwns:            true,
			expectedUserAssignedPIP: true,
		},
		{
			desc: "should be true if the pip with service tag matches the pip name",
			pip: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Tags: map[string]*string{
					consts.ServiceTagKey: ptr.To("default/web"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress: ptr.To("1.2.3.4"),
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
			realValue := az.getPublicIPAddressResourceGroup(s)
			assert.Equal(t, c.expected, realValue, "TestCase[%d]: %s", i, c.desc)
		})
	}
}

func TestShouldReleaseExistingOwnedPublicIP(t *testing.T) {
	existingPipWithTag := armnetwork.PublicIPAddress{
		ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
		Name: ptr.To("testPIP"),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
			IPTags: []*armnetwork.IPTag{
				{
					IPTagType: ptr.To("tag1"),
					Tag:       ptr.To("tag1value"),
				},
			},
		},
	}
	existingPipWithTagIPv6Suffix := existingPipWithTag
	existingPipWithTagIPv6Suffix.Name = to.Ptr("testPIP-IPv6")

	existingPipWithNoPublicIPAddressFormatProperties := armnetwork.PublicIPAddress{
		ID:         ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
		Name:       ptr.To("testPIP"),
		Tags:       map[string]*string{consts.ServiceTagKey: ptr.To("default/test2")},
		Properties: nil,
	}

	tests := []struct {
		desc                  string
		desiredPipName        string
		existingPip           armnetwork.PublicIPAddress
		ipTagRequest          serviceIPTagRequest
		lbShouldExist         bool
		lbIsInternal          bool
		isUserAssignedPIP     bool
		serviceReferences     []string
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
				IPTags:                      existingPipWithTag.Properties.IPTags,
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
				IPTags:                      existingPipWithTag.Properties.IPTags,
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
				IPTags:                      existingPipWithTag.Properties.IPTags,
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
				IPTags:                      existingPipWithTag.Properties.IPTags,
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
				IPTags:                      existingPipWithTag.Properties.IPTags,
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
				IPTags: []*armnetwork.IPTag{
					{
						IPTagType: ptr.To("tag2"),
						Tag:       ptr.To("tag2value"),
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
				IPTags: []*armnetwork.IPTag{
					{
						IPTagType: ptr.To("tag1"),
						Tag:       ptr.To("tag1value"),
					},
				},
			},
			expectedShouldRelease: true,
		},
		{
			desc:              "should delete orphaned managed public IP",
			existingPip:       existingPipWithTag,
			lbShouldExist:     false,
			lbIsInternal:      false,
			desiredPipName:    *existingPipWithTag.Name,
			serviceReferences: []string{},
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      existingPipWithTag.Properties.IPTags,
			},
			expectedShouldRelease: true,
		},
		{
			desc:              "should not delete managed public IP which has references",
			existingPip:       existingPipWithTag,
			lbShouldExist:     false,
			lbIsInternal:      false,
			desiredPipName:    *existingPipWithTag.Name,
			serviceReferences: []string{"svc1"},
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      existingPipWithTag.Properties.IPTags,
			},
		},
		{
			desc:              "should not delete orphaned unmanaged public IP",
			existingPip:       existingPipWithTag,
			lbShouldExist:     false,
			lbIsInternal:      false,
			desiredPipName:    *existingPipWithTag.Name,
			serviceReferences: []string{},
			ipTagRequest: serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      existingPipWithTag.Properties.IPTags,
			},
			isUserAssignedPIP: true,
		},
	}

	for _, c := range tests {
		t.Run(c.desc, func(t *testing.T) {
			existingPip := c.existingPip
			actualShouldRelease := shouldReleaseExistingOwnedPublicIP(&existingPip, c.serviceReferences, c.lbShouldExist, c.lbIsInternal, c.isUserAssignedPIP, c.desiredPipName, c.ipTagRequest)
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
		expected []*armnetwork.IPTag
	}{
		{
			desc:     "nil slice should be returned when the map is nil",
			input:    nil,
			expected: nil,
		},
		{
			desc:     "empty slice should be returned when the map is empty",
			input:    map[string]string{},
			expected: []*armnetwork.IPTag{},
		},
		{
			desc: "one tag should be returned when the map has one tag",
			input: map[string]string{
				"tag1": "tag1value",
			},
			expected: []*armnetwork.IPTag{
				{
					IPTagType: ptr.To("tag1"),
					Tag:       ptr.To("tag1value"),
				},
			},
		},
		{
			desc: "two tags should be returned when the map has two tags",
			input: map[string]string{
				"tag1": "tag1value",
				"tag2": "tag2value",
			},
			expected: []*armnetwork.IPTag{
				{
					IPTagType: ptr.To("tag1"),
					Tag:       ptr.To("tag1value"),
				},
				{
					IPTagType: ptr.To("tag2"),
					Tag:       ptr.To("tag2value"),
				},
			},
		},
	}

	for i, c := range tests {
		actual := convertIPTagMapToSlice(c.input)

		// Sort output to provide stability of return from map for test comparison
		// The order doesn't matter at runtime.
		if actual != nil {
			sort.Slice(actual, func(i, j int) bool {
				ipTagSlice := actual
				return ptr.Deref(ipTagSlice[i].IPTagType, "") < ptr.Deref(ipTagSlice[j].IPTagType, "")
			})
		}
		if c.expected != nil {
			sort.Slice(c.expected, func(i, j int) bool {
				ipTagSlice := c.expected
				return ptr.Deref(ipTagSlice[i].IPTagType, "") < ptr.Deref(ipTagSlice[j].IPTagType, "")
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
				IPTags:                      []*armnetwork.IPTag{},
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
				IPTags: []*armnetwork.IPTag{
					{
						IPTagType: ptr.To("tag1"),
						Tag:       ptr.To("tag1value"),
					},
					{
						IPTagType: ptr.To("tag2"),
						Tag:       ptr.To("tag2value"),
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
			sort.Slice(actual.IPTags, func(i, j int) bool {
				ipTagSlice := actual.IPTags
				return ptr.Deref(ipTagSlice[i].IPTagType, "") < ptr.Deref(ipTagSlice[j].IPTagType, "")
			})
		}
		if c.expected.IPTags != nil {
			sort.Slice(c.expected.IPTags, func(i, j int) bool {
				ipTagSlice := c.expected.IPTags
				return ptr.Deref(ipTagSlice[i].IPTagType, "") < ptr.Deref(ipTagSlice[j].IPTagType, "")
			})
		}

		assert.Equal(t, actual, c.expected, "TestCase[%d]: %s", i, c.desc)
	}
}

func TestAreIpTagsEquivalent(t *testing.T) {
	tests := []struct {
		desc     string
		input1   []*armnetwork.IPTag
		input2   []*armnetwork.IPTag
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
			input2:   []*armnetwork.IPTag{},
			expected: true,
		},
		{
			desc:     "nils should be considered to empty arrays (case 1)",
			input1:   []*armnetwork.IPTag{},
			input2:   nil,
			expected: true,
		},
		{
			desc: "nil should not be considered equal to anything (case 1)",
			input1: []*armnetwork.IPTag{
				{
					IPTagType: ptr.To("tag1"),
					Tag:       ptr.To("tag1value"),
				},
				{
					IPTagType: ptr.To("tag2"),
					Tag:       ptr.To("tag2value"),
				},
			},
			input2:   nil,
			expected: false,
		},
		{
			desc: "nil should not be considered equal to anything (case 2)",
			input2: []*armnetwork.IPTag{
				{
					IPTagType: ptr.To("tag1"),
					Tag:       ptr.To("tag1value"),
				},
				{
					IPTagType: ptr.To("tag2"),
					Tag:       ptr.To("tag2value"),
				},
			},
			input1:   nil,
			expected: false,
		},
		{
			desc: "exactly equal should be treated as equal",
			input1: []*armnetwork.IPTag{
				{
					IPTagType: ptr.To("tag1"),
					Tag:       ptr.To("tag1value"),
				},
				{
					IPTagType: ptr.To("tag2"),
					Tag:       ptr.To("tag2value"),
				},
			},
			input2: []*armnetwork.IPTag{
				{
					IPTagType: ptr.To("tag1"),
					Tag:       ptr.To("tag1value"),
				},
				{
					IPTagType: ptr.To("tag2"),
					Tag:       ptr.To("tag2value"),
				},
			},
			expected: true,
		},
		{
			desc: "equal but out of order should be treated as equal",
			input1: []*armnetwork.IPTag{
				{
					IPTagType: ptr.To("tag1"),
					Tag:       ptr.To("tag1value"),
				},
				{
					IPTagType: ptr.To("tag2"),
					Tag:       ptr.To("tag2value"),
				},
			},
			input2: []*armnetwork.IPTag{
				{
					IPTagType: ptr.To("tag2"),
					Tag:       ptr.To("tag2value"),
				},
				{
					IPTagType: ptr.To("tag1"),
					Tag:       ptr.To("tag1value"),
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

func TestGetServiceLoadBalancerMultiSLB(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description     string
		existingLBs     []*armnetwork.LoadBalancer
		refreshedLBs    []*armnetwork.LoadBalancer
		existingPIPs    []*armnetwork.PublicIPAddress
		service         v1.Service
		local           bool
		multiSLBConfigs []config.MultipleStandardLoadBalancerConfiguration
		expectedLB      *armnetwork.LoadBalancer
		expectedLBs     []*armnetwork.LoadBalancer
		expectedError   error
	}{
		{
			description: "should return the existing lb if the service is moved to the lb",
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb1-internal"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
							{
								Name: ptr.To("atest1"),
								ID:   ptr.To("atest1"),
								Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
									PrivateIPAddress: ptr.To("1.2.3.4"),
								},
							},
						},
					},
				},
				{
					Name: ptr.To("lb2-internal"),
				},
			},
			service: getInternalTestService("test1"),
			multiSLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
				},
				{
					Name: "lb2",
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveServices: utilsets.NewString("default/test1"),
					},
				},
			},
			expectedLB: &armnetwork.LoadBalancer{
				Name: ptr.To("lb2-internal"),
			},
			expectedLBs: []*armnetwork.LoadBalancer{
				{Name: ptr.To("lb2-internal")},
			},
		},
		{
			description: "remove backend pool when a local service changes its load balancer",
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb1"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
							{
								Name: ptr.To("atest1"),
								ID:   ptr.To("atest1"),
								Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
									PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("pip")},
								},
							},
							{
								Name: ptr.To("atest2"),
							},
						},
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
							},
							{
								Name: ptr.To("default-test1"),
							},
						},
					},
				},
			},
			refreshedLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb1"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
							{
								Name: ptr.To("atest1"),
								ID:   ptr.To("atest1"),
								Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
									PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("pip")},
								},
							},
							{
								Name: ptr.To("atest2"),
							},
						},
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
							},
						},
					},
				},
			},
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("1.2.3.4"),
					},
				},
			},
			service: getTestService("test1", v1.ProtocolTCP, nil, false, 80),
			local:   true,
			multiSLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
				},
				{
					Name: "lb2",
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveServices: utilsets.NewString("default/test1"),
					},
				},
			},
			expectedLB: &armnetwork.LoadBalancer{
				Name:     ptr.To("lb2"),
				Location: ptr.To("westus"),
				SKU: &armnetwork.LoadBalancerSKU{
					Name: to.Ptr(armnetwork.LoadBalancerSKUNameStandard),
				},
				Properties: &armnetwork.LoadBalancerPropertiesFormat{},
			},
			expectedLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb1"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
							{
								Name: ptr.To("atest1"),
								ID:   ptr.To("atest1"),
								Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
									PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("pip")},
								},
							},
							{
								Name: ptr.To("atest2"),
							},
						},
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
							},
						},
					},
				},
			},
		},
	} {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.LoadBalancerSKU = "Standard"
			cloud.MultipleStandardLoadBalancerConfigurations = tc.multiSLBConfigs
			lbClient := cloud.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
			lbClient.EXPECT().Delete(gomock.Any(), gomock.Any(), "lb1-internal").MaxTimes(1)
			bpClient := cloud.NetworkClientFactory.GetBackendAddressPoolClient().(*mock_backendaddresspoolclient.MockInterface)
			bpClient.EXPECT().Delete(gomock.Any(), gomock.Any(), "lb1", "default-test1").Return(nil).MaxTimes(1)
			lbClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(tc.refreshedLBs, nil).MaxTimes(1)
			lbClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).MaxTimes(1)

			mockPLSRepo := cloud.plsRepo.(*privatelinkservice.MockRepository)
			mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil)

			mockPIPClient := cloud.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			mockPIPClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]*armnetwork.PublicIPAddress{}, nil).MaxTimes(2)

			if tc.local {
				tc.service.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
			}

			lb, lbs, _, _, _, _, err := cloud.getServiceLoadBalancer(context.TODO(), &tc.service, testClusterName,
				[]*v1.Node{}, true, tc.existingLBs)
			assert.Equal(t, tc.expectedError, err)
			assert.Equal(t, tc.expectedLB, lb)
			assert.Equal(t, tc.expectedLBs, lbs)
		})
	}
}

func TestGetServiceLoadBalancerCommon(t *testing.T) {
	testCases := []struct {
		desc           string
		SKU            string
		existingLBs    []*armnetwork.LoadBalancer
		service        v1.Service
		annotations    map[string]string
		expectedLB     *armnetwork.LoadBalancer
		expectedStatus *v1.LoadBalancerStatus
		wantLB         bool
		expectedExists bool
		expectedError  bool
	}{
		{
			desc: "getServiceLoadBalancer shall return corresponding lb, status, exists if there are existed lbs",
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("testCluster"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
							{
								Name: ptr.To("aservice1"),
								Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
									PublicIPAddress: &armnetwork.PublicIPAddress{
										ID: ptr.To("testCluster-aservice1"),
									},
								},
							},
						},
					},
				},
			},
			service: getTestService("service1", v1.ProtocolTCP, nil, false, 80),
			wantLB:  false,
			expectedLB: &armnetwork.LoadBalancer{
				Name: ptr.To("testCluster"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
						{
							Name: ptr.To("aservice1"),
							Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
								PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("testCluster-aservice1")},
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
			desc: "getServiceLoadBalancer shall select the lb with minimum lb rules if wantLb is true, the SKU is " +
				"not standard and there are existing lbs already",
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("testCluster"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{
							{Name: ptr.To("rule1")},
						},
					},
				},
				{
					Name: ptr.To("as-1"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{
							{Name: ptr.To("rule1")},
							{Name: ptr.To("rule2")},
						},
					},
				},
				{
					Name: ptr.To("as-2"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{
							{Name: ptr.To("rule1")},
							{Name: ptr.To("rule2")},
							{Name: ptr.To("rule3")},
						},
					},
				},
			},
			service:     getTestService("service1", v1.ProtocolTCP, nil, false, 80),
			annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: "__auto__"},
			wantLB:      true,
			expectedLB: &armnetwork.LoadBalancer{
				Name: ptr.To("testCluster"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					LoadBalancingRules: []*armnetwork.LoadBalancingRule{
						{Name: ptr.To("rule1")},
					},
				},
			},
			expectedExists: false,
			expectedError:  false,
		},
		{
			desc:    "getServiceLoadBalancer shall create a new lb otherwise",
			service: getTestService("service1", v1.ProtocolTCP, nil, false, 80),
			expectedLB: &armnetwork.LoadBalancer{
				Name:       ptr.To("testCluster"),
				Location:   ptr.To("westus"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{},
			},
			expectedExists: false,
			expectedError:  false,
		},
		{
			desc: "getServiceLoadBalancer should not change LB when names are the same but with different case with standard LB",
			SKU:  consts.LoadBalancerSKUStandard,
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("TestCluster"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
							{
								Name: ptr.To("aservice1"),
								Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
									PublicIPAddress: &armnetwork.PublicIPAddress{
										ID: ptr.To("TestCluster-aservice1"),
									},
								},
							},
						},
					},
				},
			},
			service: getTestService("service1", v1.ProtocolTCP, nil, false, 80),
			wantLB:  true,
			expectedLB: &armnetwork.LoadBalancer{
				Name: ptr.To("TestCluster"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
						{
							Name: ptr.To("aservice1"),
							Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
								PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("TestCluster-aservice1")},
							},
						},
					},
				},
			},
			expectedStatus: &v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: "1.2.3.4", Hostname: ""}}},
			expectedExists: true,
			expectedError:  false,
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			az := GetTestCloud(ctrl)
			clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 3, 3)
			setMockEnv(az, expectedInterfaces, expectedVirtualMachines, 1)

			mockLBsClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
			mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(nil, nil).Times(len(test.existingLBs))
			mockLBsClient.EXPECT().List(gomock.Any(), "rg").Return(test.existingLBs, nil)
			mockLBsClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

			for _, existingLB := range test.existingLBs {
				_, err := az.NetworkClientFactory.GetLoadBalancerClient().CreateOrUpdate(context.TODO(), "rg", *existingLB.Name, *existingLB)
				assert.NoError(t, err)
			}
			if test.annotations != nil {
				test.service.Annotations = test.annotations
			}
			az.LoadBalancerSKU = test.SKU
			service := test.service
			lb, _, status, _, exists, _, err := az.getServiceLoadBalancer(context.TODO(), &service, testClusterName,
				clusterResources.nodes, test.wantLB, []*armnetwork.LoadBalancer{})
			assert.Equal(t, test.expectedLB, lb)
			if test.expectedStatus != nil {
				assert.Equal(t, *test.expectedStatus, *status)
			} else {
				assert.Nil(t, status)
			}
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
	setMockEnv(az, expectedInterfaces, expectedVirtualMachines, 1)

	// Test with wantLB=false
	expectedLB := &armnetwork.LoadBalancer{
		Name:     ptr.To("testCluster"),
		Location: ptr.To("westus"),
		ExtendedLocation: &armnetwork.ExtendedLocation{
			Name: ptr.To("microsoftlosangeles1"),
			Type: to.Ptr(armnetwork.ExtendedLocationTypesEdgeZone),
		},
		Properties: &armnetwork.LoadBalancerPropertiesFormat{},
	}
	mockLBsClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
	mockLBsClient.EXPECT().List(gomock.Any(), "rg").Return(nil, nil)

	lb, _, status, _, exists, _, err := az.getServiceLoadBalancer(context.TODO(), &service, testClusterName,
		clusterResources.nodes, false, []*armnetwork.LoadBalancer{})
	assert.Equal(t, expectedLB, lb, "GetServiceLoadBalancer shall return a default LB with expected location.")
	assert.Nil(t, status, "GetServiceLoadBalancer: Status should be nil for default LB.")
	assert.Equal(t, false, exists, "GetServiceLoadBalancer: Default LB should not exist.")
	assert.NoError(t, err, "GetServiceLoadBalancer: No error should be thrown when returning default LB.")

	// Test with wantLB=true
	expectedLB = &armnetwork.LoadBalancer{
		Name:     ptr.To("testCluster"),
		Location: ptr.To("westus"),
		ExtendedLocation: &armnetwork.ExtendedLocation{
			Name: ptr.To("microsoftlosangeles1"),
			Type: to.Ptr(armnetwork.ExtendedLocationTypesEdgeZone),
		},
		Properties: &armnetwork.LoadBalancerPropertiesFormat{},
		SKU: &armnetwork.LoadBalancerSKU{
			Name: to.Ptr(armnetwork.LoadBalancerSKUNameBasic),
		},
	}
	mockLBsClient = az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
	mockLBsClient.EXPECT().List(gomock.Any(), "rg").Return(nil, nil)

	lb, _, status, _, exists, _, err = az.getServiceLoadBalancer(context.TODO(), &service, testClusterName,
		clusterResources.nodes, true, []*armnetwork.LoadBalancer{})
	assert.Equal(t, *expectedLB, *lb, "GetServiceLoadBalancer shall return a new LB with expected location.")
	assert.Nil(t, status, "GetServiceLoadBalancer: Status should be nil for new LB.")
	assert.Equal(t, false, exists, "GetServiceLoadBalancer: LB should not exist before hand.")
	assert.NoError(t, err, "GetServiceLoadBalancer: No error should be thrown when returning new LB.")
}

func TestIsFrontendIPChanged(t *testing.T) {

	testCases := []struct {
		desc                   string
		config                 *armnetwork.FrontendIPConfiguration
		service                v1.Service
		lbFrontendIPConfigName string
		annotations            string
		loadBalancerIP         string
		existingSubnet         *armnetwork.Subnet
		existingPIPs           []*armnetwork.PublicIPAddress
		expectedFlag           bool
		expectedError          bool
	}{
		{
			desc: "isFrontendIPChanged shall return true if config.Name has a prefix of lb's name and " +
				"config.Name != lbFrontendIPConfigName",
			config:                 &armnetwork.FrontendIPConfiguration{Name: ptr.To("atest1-name")},
			service:                getInternalTestService("test1", 80),
			lbFrontendIPConfigName: "configName",
			expectedFlag:           true,
			expectedError:          false,
		},
		{
			desc: "isFrontendIPChanged shall return false if config.Name doesn't have a prefix of lb's name " +
				"and config.Name != lbFrontendIPConfigName",
			config:                 &armnetwork.FrontendIPConfiguration{Name: ptr.To("btest1-name")},
			service:                getInternalTestService("test1", 80),
			lbFrontendIPConfigName: "configName",
			expectedFlag:           false,
			expectedError:          false,
		},
		{
			desc: "isFrontendIPChanged shall return false if the service is internal, no loadBalancerIP is given, " +
				"subnetName == nil and config.PrivateIPAllocationMethod == network.Static",
			config: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("btest1-name"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
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
			config: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("btest1-name"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
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
			config: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("btest1-name"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					Subnet: &armnetwork.Subnet{ID: ptr.To("testSubnet")},
				},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getInternalTestService("test1", 80),
			annotations:            "testSubnet",
			existingSubnet:         &armnetwork.Subnet{ID: ptr.To("testSubnet1")},
			expectedFlag:           true,
			expectedError:          false,
		},
		{
			desc: "isFrontendIPChanged shall return false if the service is internal, subnet == nil, " +
				"loadBalancerIP == config.Properties.PrivateIPAddress and config.PrivateIPAllocationMethod != 'static'",
			config: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("btest1-name"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PrivateIPAddress:          ptr.To("1.1.1.1"),
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
				"loadBalancerIP == config.Properties.PrivateIPAddress and config.PrivateIPAllocationMethod == 'static'",
			config: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("btest1-name"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
					PrivateIPAddress:          ptr.To("1.1.1.1"),
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
				"loadBalancerIP != config.Properties.PrivateIPAddress",
			config: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("btest1-name"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
					PrivateIPAddress:          ptr.To("1.1.1.2"),
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
			config: &armnetwork.FrontendIPConfiguration{
				Name:       ptr.To("btest1-name"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getTestService("test1", v1.ProtocolTCP, nil, false, 80),
			loadBalancerIP:         "1.1.1.1",
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pipName"),
					ID:   ptr.To("pip"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("1.1.1.1"),
					},
				},
			},
			expectedFlag:  false,
			expectedError: false,
		},
		{
			desc: "isFrontendIPChanged shall return false if pip.ID == config.PublicIPAddress.ID",
			config: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("btest1-name"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription" +
						"/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pipName")},
				},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getTestService("test1", v1.ProtocolTCP, nil, false, 80),
			loadBalancerIP:         "1.1.1.1",
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pipName"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("1.1.1.1"),
					},
					ID: ptr.To("/subscriptions/subscription" +
						"/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pipName"),
				},
			},
			expectedFlag:  false,
			expectedError: false,
		},
		{
			desc: "isFrontendIPChanged shall return true if pip.ID != config.PublicIPAddress.ID",
			config: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("btest1-name"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{
						ID: ptr.To("/subscriptions/subscription" +
							"/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pipName1"),
					},
				},
			},
			lbFrontendIPConfigName: "btest1-name",
			service:                getTestService("test1", v1.ProtocolTCP, nil, false, 80),
			loadBalancerIP:         "1.1.1.1",
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pipName"),
					ID: ptr.To("/subscriptions/subscription" +
						"/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pipName2"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("1.1.1.1"),
					},
				},
			},
			expectedFlag:  true,
			expectedError: false,
		},
	}

	for _, test := range testCases {
		test := test
		t.Run(test.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			az := GetTestCloud(ctrl)
			mockSubnetsClient := az.subnetRepo.(*subnet.MockRepository)
			mockSubnetsClient.EXPECT().Get(gomock.Any(), "rg", "vnet", "testSubnet").Return(test.existingSubnet, nil).AnyTimes()
			mockSubnetsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "vnet", "testSubnet", test.existingSubnet).Return(nil).AnyTimes()

			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
			for _, existingPIP := range test.existingPIPs {
				_, err := az.NetworkClientFactory.GetPublicIPAddressClient().CreateOrUpdate(context.TODO(), "rg", *existingPIP.Name, *existingPIP)
				if err != nil {
					t.Fatal(err)
				}
			}
			mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(test.existingPIPs, nil).MaxTimes(2)
			service := test.service
			setServiceLoadBalancerIP(&service, test.loadBalancerIP)
			test.service.Annotations[consts.ServiceAnnotationLoadBalancerInternalSubnet] = test.annotations
			var subnet armnetwork.Subnet
			flag, rerr := az.isFrontendIPChanged(context.TODO(), "testCluster", test.config,
				&service, test.lbFrontendIPConfigName, &subnet)
			if rerr != nil {
				fmt.Println(rerr.Error())
			}
			assert.Equal(t, test.expectedFlag, flag)
			assert.Equal(t, test.expectedError, rerr != nil)
		})
	}
}

func TestDeterminePublicIPName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc            string
		loadBalancerIP  string
		existingPIPs    []*armnetwork.PublicIPAddress
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
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pipName"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("1.2.3.4"),
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

			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(test.existingPIPs, nil).MaxTimes(2)
			mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
			for _, existingPIP := range test.existingPIPs {
				mockPIPsClient.EXPECT().Get(gomock.Any(), "rg", *existingPIP.Name, gomock.Any()).Return(existingPIP, nil).AnyTimes()
				_, err := az.NetworkClientFactory.GetPublicIPAddressClient().CreateOrUpdate(context.TODO(), "rg", *existingPIP.Name, *existingPIP)
				assert.NoError(t, err)
			}
			pipName, _, err := az.determinePublicIPName(context.TODO(), "testCluster", &service, test.isIPv6)
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
		loadBalancerSKU string
		probeProtocol   string
		probePath       string
		expectedProbes  map[bool][]*armnetwork.Probe
		expectedRules   map[bool][]*armnetwork.LoadBalancingRule
		expectedErr     bool
	}{
		{
			desc:            "getExpectedLBRules shall return corresponding probe and lbRule(blb)",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{}, 80),
			loadBalancerSKU: "basic",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules:   getDefaultTestRules(false),
		},
		{
			desc:            "getExpectedLBRules shall return tcp probe on non supported protocols when basic lb SKU is used",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{}, 80),
			loadBalancerSKU: "basic",
			probeProtocol:   "Mongodb",
			expectedRules:   getDefaultTestRules(false),
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
		},
		{
			desc:            "getExpectedLBRules shall return tcp probe on https protocols when basic lb SKU is used",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{}, 80),
			loadBalancerSKU: "basic",
			probeProtocol:   "Https",
			expectedRules:   getDefaultTestRules(false),
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
		},
		{
			desc:            "getExpectedLBRules shall return error (slb with external mode and SCTP)",
			service:         getTestServiceDualStack("test1", v1.ProtocolSCTP, map[string]string{}, 80),
			loadBalancerSKU: "standard",
			expectedErr:     true,
		},
		{
			desc:            "getExpectedLBRules shall return corresponding probe and lbRule(slb with tcp reset)",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			loadBalancerSKU: "standard",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc:            "getExpectedLBRules shall respect the probe protocol and path configuration in the config file",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			loadBalancerSKU: "standard",
			probeProtocol:   "Http",
			probePath:       "/healthy",
			expectedProbes:  getDefaultTestProbes("Http", "/healthy"),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc:            "getExpectedLBRules shall respect the probe protocol and path configuration in the config file",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			loadBalancerSKU: "standard",
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
			loadBalancerSKU: "standard",
			expectedProbes: map[bool][]*armnetwork.Probe{
				// Use false as IPv6 param but it is a IPv6 probe.
				true: {getTestProbe("Tcp", "", ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)), false)},
			},
			expectedRules: getDefaultInternalIPv6Rules(true),
		},
		{
			desc: "getExpectedLBRules shall return corresponding probe and lbRule (slb with HA enabled)",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "true",
				consts.ServiceAnnotationLoadBalancerInternal:                    "true",
			}, 80),
			loadBalancerSKU: "standard",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules: map[bool][]*armnetwork.LoadBalancingRule{
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
			loadBalancerSKU: "standard",
			expectedRules: map[bool][]*armnetwork.LoadBalancingRule{
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
			loadBalancerSKU: "standard",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules: map[bool][]*armnetwork.LoadBalancingRule{
				consts.IPVersionIPv4: getHATestRules(true, true, v1.ProtocolTCP, consts.IPVersionIPv4, true),
				consts.IPVersionIPv6: getHATestRules(true, true, v1.ProtocolTCP, consts.IPVersionIPv6, true),
			},
		},
		{
			desc:            "getExpectedLBRules should leave probe path empty when using TCP probe",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80),
			loadBalancerSKU: "standard",
			probeProtocol:   "Tcp",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return tcp probe when invalid protocol is defined",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy1",
			}, 80),
			loadBalancerSKU: "standard",
			probeProtocol:   "TCP1",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return tcp probe when invalid protocol is defined",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy1",
			}, 80),
			loadBalancerSKU: "basic",
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
			loadBalancerSKU: "standard",
			expectedProbes:  getDefaultTestProbes("Https", "/healthy1"),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return correct rule when deprecated annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy1",
				consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                              "http",
			}, 80),
			loadBalancerSKU: "standard",
			expectedProbes:  getDefaultTestProbes("Http", "/healthy1"),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return correct rule when deprecated annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy1",
				consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                              "tcp",
			}, 80),
			loadBalancerSKU: "standard",
			expectedProbes:  getDefaultTestProbes("Tcp", ""),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return correct rule when deprecated annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsRequestPath): "/healthy1",
			}, 80),
			loadBalancerSKU: "standard",
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
			loadBalancerSKU: "standard",
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
			loadBalancerSKU: "standard",
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
			loadBalancerSKU: "standard",
			probeProtocol:   "Tcp",
			expectedErr:     true,
		},
		{
			desc: "getExpectedLBRules should return correct rule when health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "20",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "5",
			}, 80),
			loadBalancerSKU: "standard",
			probeProtocol:   "Https",
			probePath:       "/healthy1",
			expectedProbes:  getTestProbes("Https", "/healthy1", ptr.To(int32(20)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(5))),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return correct rule when health probe annotations are added,default path should be /",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "20",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "5",
			}, 80),
			loadBalancerSKU: "standard",
			probeProtocol:   "Http",
			expectedProbes:  getTestProbes("Http", "/", ptr.To(int32(20)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(5))),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return correct rule when tcp health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "20",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "5",
			}, 80),
			loadBalancerSKU: "standard",
			probeProtocol:   "Tcp",
			expectedProbes:  getTestProbes("Tcp", "", ptr.To(int32(20)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(5))),
			expectedRules:   getDefaultTestRules(true),
		},
		{
			desc: "getExpectedLBRules should return error when invalid tcp health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "20",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "5a",
			}, 80),
			loadBalancerSKU: "standard",
			probeProtocol:   "Tcp",
			expectedErr:     true,
		},
		{
			desc: "getExpectedLBRules should return error when invalid tcp health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "1",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "5",
			}, 80),
			loadBalancerSKU: "standard",
			probeProtocol:   "Tcp",
			expectedErr:     true,
		},
		{
			desc: "getExpectedLBRules should return error when invalid tcp health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "10",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "1",
			}, 80),
			loadBalancerSKU: "standard",
			probeProtocol:   "Tcp",
			expectedErr:     true,
		},
		{
			desc: "getExpectedLBRules should return error when invalid tcp health probe annotations are added",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "10",
				consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "20",
			}, 80),
			loadBalancerSKU: "standard",
			probeProtocol:   "Tcp",
			expectedErr:     true,
		},
		{
			desc:            "getExpectedLBRules should return correct rule when floating ip annotations are added",
			service:         getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true"}, 80),
			loadBalancerSKU: "basic",
			expectedRules: map[bool][]*armnetwork.LoadBalancingRule{
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
			loadBalancerSKU: "standard",
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
			loadBalancerSKU: "standard",
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
			expectedRules: map[bool][]*armnetwork.LoadBalancingRule{
				consts.IPVersionIPv4: {getTestRule(false, 80, consts.IPVersionIPv4), getTestRule(false, 8000, consts.IPVersionIPv4)},
				consts.IPVersionIPv6: {getTestRule(false, 80, consts.IPVersionIPv6), getTestRule(false, 8000, consts.IPVersionIPv6)},
			},
			expectedProbes: map[bool][]*armnetwork.Probe{
				consts.IPVersionIPv4: {
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv4),
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(8000)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv6),
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(8000)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv6),
				},
			},
		},
		{
			desc: "getExpectedLBRules should support customize health probe port in multi-port service",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_8000_health-probe_port": "80",
			}, 80, 8000),
			expectedRules: map[bool][]*armnetwork.LoadBalancingRule{
				consts.IPVersionIPv4: {
					getTestRule(false, 80, consts.IPVersionIPv4),
					getTestRule(false, 8000, consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestRule(false, 80, consts.IPVersionIPv6),
					getTestRule(false, 8000, consts.IPVersionIPv6),
				},
			},
			expectedProbes: map[bool][]*armnetwork.Probe{
				consts.IPVersionIPv4: {
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv4),
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(8000)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv6),
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(8000)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv6),
				},
			},
		},
		{
			desc: "getExpectedLBRules should not generate probe rule when no health probe rule is specified.",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_8000_no_probe_rule": "true",
			}, 80, 8000),
			expectedRules: map[bool][]*armnetwork.LoadBalancingRule{
				consts.IPVersionIPv4: {
					getTestRule(false, 80, consts.IPVersionIPv4),
					func() *armnetwork.LoadBalancingRule {
						rule := getTestRule(false, 8000, consts.IPVersionIPv4)
						rule.Properties.Probe = nil
						return rule
					}(),
				},
				consts.IPVersionIPv6: {
					getTestRule(false, 80, consts.IPVersionIPv6),
					func() *armnetwork.LoadBalancingRule {
						rule := getTestRule(false, 8000, consts.IPVersionIPv6)
						rule.Properties.Probe = nil
						return rule
					}(),
				},
			},
			expectedProbes: map[bool][]*armnetwork.Probe{
				consts.IPVersionIPv4: {
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv6),
				},
			},
		},
		{
			desc: "getExpectedLBRules should not generate lb rule and health probe rule when no lb rule is specified.",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_8000_no_lb_rule": "true",
			}, 80, 8000),
			expectedRules: map[bool][]*armnetwork.LoadBalancingRule{
				consts.IPVersionIPv4: {
					getTestRule(false, 80, consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestRule(false, 80, consts.IPVersionIPv6),
				},
			},
			expectedProbes: map[bool][]*armnetwork.Probe{
				consts.IPVersionIPv4: {
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv6),
				},
			},
		},
		{
			desc: "getExpectedLBRules should support customize health probe port ",
			service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
				"service.beta.kubernetes.io/port_8000_health-probe_port": "5080",
			}, 80, 8000),
			expectedRules: map[bool][]*armnetwork.LoadBalancingRule{
				consts.IPVersionIPv4: {
					getTestRule(false, 80, consts.IPVersionIPv4),
					getTestRule(false, 8000, consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestRule(false, 80, consts.IPVersionIPv6),
					getTestRule(false, 8000, consts.IPVersionIPv6),
				},
			},
			expectedProbes: map[bool][]*armnetwork.Probe{
				consts.IPVersionIPv4: {
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv4),
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(8000)), ptr.To(int32(5080)), ptr.To(int32(2)), consts.IPVersionIPv4),
				},
				consts.IPVersionIPv6: {
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(2)), consts.IPVersionIPv6),
					getTestProbe("Tcp", "/", ptr.To(int32(5)), ptr.To(int32(8000)), ptr.To(int32(5080)), ptr.To(int32(2)), consts.IPVersionIPv6),
				},
			},
		},
	}
	rulesDualStack := getDefaultTestRules(true)
	for _, rules := range rulesDualStack {
		for _, rule := range rules {
			rule.Properties.IdleTimeoutInMinutes = to.Ptr(int32(5))
		}
	}
	testCases = append(testCases, struct {
		desc            string
		service         v1.Service
		loadBalancerSKU string
		probeProtocol   string
		probePath       string
		expectedProbes  map[bool][]*armnetwork.Probe
		expectedRules   map[bool][]*armnetwork.LoadBalancingRule
		expectedErr     bool
	}{
		desc: "getExpectedLBRules should expected rules when timeout are added",
		service: getTestServiceDualStack("test1", v1.ProtocolTCP, map[string]string{
			consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProbeInterval): "10",
			consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsNumOfProbe):    "10",
			consts.ServiceAnnotationLoadBalancerIdleTimeout:                                        "5",
		}, 80),
		loadBalancerSKU: "standard",
		probeProtocol:   "Tcp",
		expectedProbes:  getTestProbes("Tcp", "", ptr.To(int32(10)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(10))),
		expectedRules:   rulesDualStack,
	})
	rules1DualStack := map[bool][]*armnetwork.LoadBalancingRule{
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
		rule.Properties.Probe.ID = to.Ptr("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lbname/probes/atest1-TCP-34567")
	}
	for _, rule := range rules1DualStack[consts.IPVersionIPv6] {
		rule.Properties.Probe.ID = to.Ptr("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lbname/probes/atest1-TCP-34567-IPv6")
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
	probes := getTestProbes("Http", "/healthz", ptr.To(int32(5)), ptr.To(int32(34567)), ptr.To(int32(34567)), ptr.To(int32(2)))
	testCases = append(testCases, struct {
		desc            string
		service         v1.Service
		loadBalancerSKU string
		probeProtocol   string
		probePath       string
		expectedProbes  map[bool][]*armnetwork.Probe
		expectedRules   map[bool][]*armnetwork.LoadBalancingRule
		expectedErr     bool
	}{
		desc:            "getExpectedLBRules should expected rules when externalTrafficPolicy is local",
		service:         svc,
		loadBalancerSKU: "standard",
		probeProtocol:   "Http",
		expectedProbes:  probes,
		expectedRules:   rules1DualStack,
	})
	rules1DualStack = map[bool][]*armnetwork.LoadBalancingRule{
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
	probes = getTestProbes("Https", "/broken/local/path", ptr.To(int32(7)), ptr.To(int32(80)), ptr.To(int32(10080)), ptr.To(int32(15)))
	testCases = append(testCases, struct {
		desc            string
		service         v1.Service
		loadBalancerSKU string
		probeProtocol   string
		probePath       string
		expectedProbes  map[bool][]*armnetwork.Probe
		expectedRules   map[bool][]*armnetwork.LoadBalancingRule
		expectedErr     bool
	}{
		desc:            "getExpectedLBRules should return expected rules when externalTrafficPolicy is local and service.beta.kubernetes.io/azure-pls-proxy-protocol is enabled",
		service:         svc,
		loadBalancerSKU: "standard",
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
	probes = getTestProbes("Https", "/broken/local/path", ptr.To(int32(7)), ptr.To(int32(80)), ptr.To(int32(421)), ptr.To(int32(15)))
	testCases = append(testCases, struct {
		desc            string
		service         v1.Service
		loadBalancerSKU string
		probeProtocol   string
		probePath       string
		expectedProbes  map[bool][]*armnetwork.Probe
		expectedRules   map[bool][]*armnetwork.LoadBalancingRule
		expectedErr     bool
	}{
		desc:            "getExpectedLBRules should return expected rules when externalTrafficPolicy is local and service.beta.kubernetes.io/azure-pls-proxy-protocol is enabled",
		service:         svc,
		loadBalancerSKU: "standard",
		probeProtocol:   "https",
		expectedProbes:  probes,
		expectedRules:   rules1DualStack,
	})
	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			az.Config.LoadBalancerSKU = test.loadBalancerSKU
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

func TestGetExpectedLBRulesSharedProbe(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		desc string
	}{
		{
			desc: "getExpectedLBRules should return a shared rule for a cluster service when shared probe is enabled",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			az.ClusterServiceLoadBalancerHealthProbeMode = consts.ClusterServiceLoadBalancerHealthProbeModeShared
			svc := getTestService("test1", v1.ProtocolTCP, nil, false, 80, 81)

			probe, lbrule, err := az.getExpectedLBRules(&svc, "frontendIPConfigID", "backendPoolID", "lbname", consts.IPVersionIPv4)
			assert.NoError(t, err)
			assert.Equal(t, 1, len(probe))
			assert.Equal(t, *az.buildClusterServiceSharedProbe(), *probe[0])
			assert.Equal(t, 2, len(lbrule))
			assert.Equal(t, "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lbname/probes/cluster-service-shared-health-probe", *lbrule[0].Properties.Probe.ID)
			assert.Equal(t, "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lbname/probes/cluster-service-shared-health-probe", *lbrule[1].Properties.Probe.ID)
		})
	}
}

// getDefaultTestRules returns dualstack rules.
func getDefaultTestRules(enableTCPReset bool) map[bool][]*armnetwork.LoadBalancingRule {
	return map[bool][]*armnetwork.LoadBalancingRule{
		consts.IPVersionIPv4: {getTestRule(enableTCPReset, 80, consts.IPVersionIPv4)},
		consts.IPVersionIPv6: {getTestRule(enableTCPReset, 80, consts.IPVersionIPv6)},
	}
}

// getDefaultInternalIPv6Rules returns a rule for IPv6 single stack.
func getDefaultInternalIPv6Rules(enableTCPReset bool) map[bool][]*armnetwork.LoadBalancingRule {
	rule := getTestRule(enableTCPReset, 80, false)
	rule.Properties.EnableFloatingIP = to.Ptr(false)
	rule.Properties.BackendPort = to.Ptr(getBackendPort(*rule.Properties.FrontendPort))
	rule.Properties.BackendAddressPool.ID = to.Ptr("backendPoolID-IPv6")
	return map[bool][]*armnetwork.LoadBalancingRule{
		true: {rule},
	}
}

// getTCPResetTestRules returns rules with TCPReset always set.
func getTCPResetTestRules(enableTCPReset bool) map[bool][]*armnetwork.LoadBalancingRule {
	IPv4Rule := getTestRule(enableTCPReset, 80, consts.IPVersionIPv4)
	IPv6Rule := getTestRule(enableTCPReset, 80, consts.IPVersionIPv6)
	IPv4Rule.Properties.EnableTCPReset = to.Ptr(enableTCPReset)
	IPv6Rule.Properties.EnableTCPReset = to.Ptr(enableTCPReset)
	return map[bool][]*armnetwork.LoadBalancingRule{
		consts.IPVersionIPv4: {IPv4Rule},
		consts.IPVersionIPv6: {IPv6Rule},
	}
}

// getTestRule returns a rule for dualStack.
func getTestRule(enableTCPReset bool, port int32, isIPv6 bool) *armnetwork.LoadBalancingRule {
	suffix := ""
	if isIPv6 {
		suffix = "-" + consts.IPVersionIPv6String
	}
	expectedRules := &armnetwork.LoadBalancingRule{
		Name: ptr.To(fmt.Sprintf("atest1-TCP-%d", port) + suffix),
		Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
			Protocol: ptr.To(armnetwork.TransportProtocolTCP),
			FrontendIPConfiguration: &armnetwork.SubResource{
				ID: ptr.To("frontendIPConfigID" + suffix),
			},
			BackendAddressPool: &armnetwork.SubResource{
				ID: ptr.To("backendPoolID" + suffix),
			},
			LoadDistribution:     to.Ptr(armnetwork.LoadDistributionDefault),
			FrontendPort:         ptr.To(port),
			BackendPort:          ptr.To(port),
			EnableFloatingIP:     ptr.To(true),
			DisableOutboundSnat:  ptr.To(false),
			IdleTimeoutInMinutes: ptr.To(int32(4)),
			Probe: &armnetwork.SubResource{
				ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/" +
					fmt.Sprintf("Microsoft.Network/loadBalancers/lbname/probes/atest1-TCP-%d%s", port, suffix)),
			},
		},
	}
	if enableTCPReset {
		expectedRules.Properties.EnableTCPReset = to.Ptr(true)
	}
	return expectedRules
}

func getHATestRules(_, hasProbe bool, protocol v1.Protocol, isIPv6, isInternal bool) []*armnetwork.LoadBalancingRule {
	suffix := ""
	enableFloatingIP := true
	if isIPv6 {
		suffix = "-" + consts.IPVersionIPv6String
		if isInternal {
			enableFloatingIP = false
		}
	}

	expectedRules := []*armnetwork.LoadBalancingRule{
		{
			Name: ptr.To(fmt.Sprintf("atest1-%s-80%s", string(protocol), suffix)),
			Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
				Protocol: to.Ptr(armnetwork.TransportProtocolAll),
				FrontendIPConfiguration: &armnetwork.SubResource{
					ID: ptr.To("frontendIPConfigID" + suffix),
				},
				BackendAddressPool: &armnetwork.SubResource{
					ID: ptr.To("backendPoolID" + suffix),
				},
				LoadDistribution:     to.Ptr(armnetwork.LoadDistributionDefault),
				FrontendPort:         ptr.To(int32(0)),
				BackendPort:          ptr.To(int32(0)),
				EnableFloatingIP:     ptr.To(enableFloatingIP),
				DisableOutboundSnat:  ptr.To(false),
				IdleTimeoutInMinutes: ptr.To(int32(4)),
				EnableTCPReset:       ptr.To(true),
			},
		},
	}
	if hasProbe {
		expectedRules[0].Properties.Probe = &armnetwork.SubResource{
			ID: ptr.To(fmt.Sprintf("/subscriptions/subscription/resourceGroups/rg/providers/"+
				"Microsoft.Network/loadBalancers/lbname/probes/atest1-%s-80%s", string(protocol), suffix)),
		}
	}
	return expectedRules
}

func getFloatingIPTestRule(enableTCPReset, enableFloatingIP bool, port int32, isIPv6 bool) *armnetwork.LoadBalancingRule {
	suffix := ""
	if isIPv6 {
		suffix = "-" + consts.IPVersionIPv6String
	}
	expectedRules := &armnetwork.LoadBalancingRule{
		Name: ptr.To(fmt.Sprintf("atest1-TCP-%d%s", port, suffix)),
		Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
			Protocol: ptr.To(armnetwork.TransportProtocolTCP),
			FrontendIPConfiguration: &armnetwork.SubResource{
				ID: ptr.To("frontendIPConfigID" + suffix),
			},
			BackendAddressPool: &armnetwork.SubResource{
				ID: ptr.To("backendPoolID" + suffix),
			},
			LoadDistribution:     to.Ptr(armnetwork.LoadDistributionDefault),
			FrontendPort:         ptr.To(port),
			BackendPort:          ptr.To(getBackendPort(port)),
			EnableFloatingIP:     ptr.To(enableFloatingIP),
			DisableOutboundSnat:  ptr.To(false),
			IdleTimeoutInMinutes: ptr.To(int32(4)),
			Probe: &armnetwork.SubResource{
				ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/" +
					fmt.Sprintf("Microsoft.Network/loadBalancers/lbname/probes/atest1-TCP-%d%s", port, suffix)),
			},
		},
	}
	if enableTCPReset {
		expectedRules.Properties.EnableTCPReset = to.Ptr(true)
	}
	return expectedRules
}

func getTestLoadBalancer(name, rgName, clusterName, identifier *string, service v1.Service, lbSKU string) *armnetwork.LoadBalancer {
	caser := cases.Title(language.English)
	lb := &armnetwork.LoadBalancer{
		Name: name,
		SKU: &armnetwork.LoadBalancerSKU{
			Name: to.Ptr(armnetwork.LoadBalancerSKUName(lbSKU)),
		},
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
				{
					Name: identifier,
					ID: ptr.To("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
						"Microsoft.Network/loadBalancers/" + *name + "/frontendIPConfigurations/" + *identifier),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1")},
					},
				},
			},
			BackendAddressPools: []*armnetwork.BackendAddressPool{
				{Name: clusterName},
			},
			Probes: []*armnetwork.Probe{
				{
					Name: ptr.To(*identifier + "-" + string(service.Spec.Ports[0].Protocol) +
						"-" + strconv.Itoa(int(service.Spec.Ports[0].Port))),
					Properties: &armnetwork.ProbePropertiesFormat{
						Port:              ptr.To(int32(10080)),
						Protocol:          to.Ptr(armnetwork.ProbeProtocolTCP),
						IntervalInSeconds: ptr.To(int32(5)),
						ProbeThreshold:    ptr.To(int32(2)),
					},
				},
			},
			LoadBalancingRules: []*armnetwork.LoadBalancingRule{
				{
					Name: ptr.To(*identifier + "-" + string(service.Spec.Ports[0].Protocol) +
						"-" + strconv.Itoa(int(service.Spec.Ports[0].Port))),
					Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
						Protocol: to.Ptr(armnetwork.TransportProtocol(caser.String((strings.ToLower(string(service.Spec.Ports[0].Protocol)))))),
						FrontendIPConfiguration: &armnetwork.SubResource{
							ID: ptr.To("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
								"Microsoft.Network/loadBalancers/" + *name + "/frontendIPConfigurations/aservice1"),
						},
						BackendAddressPool: &armnetwork.SubResource{
							ID: ptr.To("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
								"Microsoft.Network/loadBalancers/" + *name + "/backendAddressPools/" + *clusterName),
						},
						LoadDistribution:     to.Ptr(armnetwork.LoadDistributionDefault),
						FrontendPort:         ptr.To(service.Spec.Ports[0].Port),
						BackendPort:          ptr.To(service.Spec.Ports[0].Port),
						EnableFloatingIP:     ptr.To(true),
						EnableTCPReset:       ptr.To(strings.EqualFold(lbSKU, "standard")),
						DisableOutboundSnat:  ptr.To(false),
						IdleTimeoutInMinutes: ptr.To(int32(4)),
						Probe: &armnetwork.SubResource{
							ID: ptr.To("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/Microsoft.Network/loadBalancers/testCluster/probes/aservice1-TCP-80"),
						},
					},
				},
			},
		},
	}
	return lb
}

func getTestLoadBalancerDualStack(name, rgName, clusterName, identifier *string, service v1.Service, lbSKU string) *armnetwork.LoadBalancer {
	caser := cases.Title(language.English)
	lb := getTestLoadBalancer(name, rgName, clusterName, identifier, service, lbSKU)
	lb.Properties.FrontendIPConfigurations = append(lb.Properties.FrontendIPConfigurations, &armnetwork.FrontendIPConfiguration{
		Name: ptr.To(*identifier + ipv6Suffix),
		ID: ptr.To("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
			"Microsoft.Network/loadBalancers/" + *name + "/frontendIPConfigurations/" + *identifier + ipv6Suffix),
		Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
			PublicIPAddress: &armnetwork.PublicIPAddress{
				ID: ptr.To("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1-IPv6"),
			},
		},
	})
	lb.Properties.BackendAddressPools = append(lb.Properties.BackendAddressPools, &armnetwork.BackendAddressPool{
		Name: ptr.To(*clusterName + ipv6Suffix),
	})
	lb.Properties.Probes = append(lb.Properties.Probes, &armnetwork.Probe{
		Name: ptr.To(*identifier + "-" + string(service.Spec.Ports[0].Protocol) +
			"-" + strconv.Itoa(int(service.Spec.Ports[0].Port)) + ipv6Suffix),
		Properties: &armnetwork.ProbePropertiesFormat{
			Port:              ptr.To(int32(10080)),
			Protocol:          to.Ptr(armnetwork.ProbeProtocolTCP),
			IntervalInSeconds: ptr.To(int32(5)),
			ProbeThreshold:    ptr.To(int32(2)),
		},
	})
	lb.Properties.LoadBalancingRules = append(lb.Properties.LoadBalancingRules, &armnetwork.LoadBalancingRule{
		Name: ptr.To(*identifier + "-" + string(service.Spec.Ports[0].Protocol) +
			"-" + strconv.Itoa(int(service.Spec.Ports[0].Port)) + ipv6Suffix),
		Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
			Protocol: to.Ptr(armnetwork.TransportProtocol(caser.String((strings.ToLower(string(service.Spec.Ports[0].Protocol)))))),
			FrontendIPConfiguration: &armnetwork.SubResource{
				ID: ptr.To("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
					"Microsoft.Network/loadBalancers/" + *name + "/frontendIPConfigurations/aservice1-IPv6"),
			},
			BackendAddressPool: &armnetwork.SubResource{
				ID: ptr.To("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/" +
					"Microsoft.Network/loadBalancers/" + *name + "/backendAddressPools/" + *clusterName + ipv6Suffix),
			},
			LoadDistribution:     to.Ptr(armnetwork.LoadDistributionDefault),
			FrontendPort:         ptr.To(service.Spec.Ports[0].Port),
			BackendPort:          ptr.To(service.Spec.Ports[0].Port),
			EnableFloatingIP:     ptr.To(true),
			EnableTCPReset:       ptr.To(strings.EqualFold(lbSKU, "standard")),
			DisableOutboundSnat:  ptr.To(false),
			IdleTimeoutInMinutes: ptr.To(int32(4)),
			Probe: &armnetwork.SubResource{
				ID: ptr.To("/subscriptions/subscription/resourceGroups/" + *rgName + "/providers/Microsoft.Network/loadBalancers/testCluster/probes/aservice1-TCP-80-IPv6"),
			},
		},
	})
	return lb
}

func TestReconcileLoadBalancerCommon(t *testing.T) {

	service1 := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)
	basicLb1 := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("aservice1"), service1, "Basic")

	service2 := getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80)
	basicLb2 := getTestLoadBalancerDualStack(ptr.To("lb1"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("bservice1"), service2, "Basic")
	basicLb2.Name = to.Ptr("testCluster")
	basicLb2.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("bservice1"),
			ID:   ptr.To("bservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1")},
			},
		},
		{
			Name: ptr.To("bservice1-IPv6"),
			ID:   ptr.To("bservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("testCluster-bservice1-IPv6")},
			},
		},
	}

	service3 := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)
	modifiedLbs := make([]*armnetwork.LoadBalancer, 2)
	for i := range modifiedLbs {
		modifiedLbs[i] = getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("aservice1"), service3, "Basic")
		modifiedLbs[i].Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
			{
				Name: ptr.To("aservice1"),
				ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1")},
				},
			},
			{
				Name: ptr.To("bservice1"),
				ID:   ptr.To("bservice1"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1")},
				},
			},
			{
				Name: ptr.To("aservice1-IPv6"),
				ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1-IPv6")},
				},
			},
			{
				Name: ptr.To("bservice1-IPv6"),
				ID:   ptr.To("bservice1-IPv6"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1-IPv6")},
				},
			},
		}
		modifiedLbs[i].Properties.Probes = []*armnetwork.Probe{
			{
				Name: ptr.To(svcPrefix + string(service3.Spec.Ports[0].Protocol) +
					"-" + strconv.Itoa(int(service3.Spec.Ports[0].Port))),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port: ptr.To(int32(10080)),
				},
			},
			{
				Name: ptr.To(svcPrefix + string(service3.Spec.Ports[0].Protocol) +
					"-" + strconv.Itoa(int(service3.Spec.Ports[0].Port))),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port: ptr.To(int32(10081)),
				},
			},
			{
				Name: ptr.To(svcPrefix + string(service3.Spec.Ports[0].Protocol) +
					"-" + strconv.Itoa(int(service3.Spec.Ports[0].Port)) + ipv6Suffix),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port: ptr.To(int32(10080)),
				},
			},
			{
				Name: ptr.To(svcPrefix + string(service3.Spec.Ports[0].Protocol) +
					"-" + strconv.Itoa(int(service3.Spec.Ports[0].Port)) + ipv6Suffix),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port: ptr.To(int32(10081)),
				},
			},
		}
	}
	expectedLb1 := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("aservice1"), service3, "Basic")
	expectedLb1.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1")},
			},
		},
		{
			Name: ptr.To("bservice1"),
			ID:   ptr.To("bservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1")},
			},
		},
		{
			Name: ptr.To("aservice1-IPv6"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1-IPv6")},
			},
		},
		{
			Name: ptr.To("bservice1-IPv6"),
			ID:   ptr.To("bservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1-IPv6")},
			},
		},
	}

	service4 := getTestServiceDualStack("service1", v1.ProtocolTCP, map[string]string{}, 80)
	existingSLB := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("aservice1"), service4, "Standard")
	existingSLB.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1")},
			},
		},
		{
			Name: ptr.To("bservice1"),
			ID:   ptr.To("bservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1")},
			},
		},
		{
			Name: ptr.To("aservice1-IPv6"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1-IPv6")},
			},
		},
		{
			Name: ptr.To("bservice1-IPv6"),
			ID:   ptr.To("bservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1-IPv6")},
			},
		},
	}
	existingSLB.Properties.Probes = []*armnetwork.Probe{
		{
			Name: ptr.To(svcPrefix + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port))),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port: ptr.To(int32(10080)),
			},
		},
		{
			Name: ptr.To(svcPrefix + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port))),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port: ptr.To(int32(10081)),
			},
		},
		{
			Name: ptr.To(svcPrefix + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port)) + ipv6Suffix),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port: ptr.To(int32(10080)),
			},
		},
		{
			Name: ptr.To(svcPrefix + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port)) + ipv6Suffix),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port: ptr.To(int32(10081)),
			},
		},
	}

	expectedSLb := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("aservice1"), service4, "Standard")
	(expectedSLb.Properties.LoadBalancingRules)[0].Properties.DisableOutboundSnat = to.Ptr(true)
	(expectedSLb.Properties.LoadBalancingRules)[0].Properties.EnableTCPReset = to.Ptr(true)
	(expectedSLb.Properties.LoadBalancingRules)[0].Properties.IdleTimeoutInMinutes = to.Ptr(int32(4))
	(expectedSLb.Properties.LoadBalancingRules)[1].Properties.DisableOutboundSnat = to.Ptr(true)
	(expectedSLb.Properties.LoadBalancingRules)[1].Properties.EnableTCPReset = to.Ptr(true)
	(expectedSLb.Properties.LoadBalancingRules)[1].Properties.IdleTimeoutInMinutes = to.Ptr(int32(4))
	expectedSLb.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1")},
			},
		},
		{
			Name: ptr.To("bservice1"),
			ID:   ptr.To("bservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1")},
			},
		},
		{
			Name: ptr.To("aservice1-IPv6"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1-IPv6")},
			},
		},
		{
			Name: ptr.To("bservice1-IPv6"),
			ID:   ptr.To("bservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1-IPv6")},
			},
		},
	}

	service5 := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)
	slb5 := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("aservice1"), service5, "Standard")
	slb5.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1")},
			},
		},
		{
			Name: ptr.To("bservice1"),
			ID:   ptr.To("bservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1")},
			},
		},
		{
			Name: ptr.To("aservice1-IPv6"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1-IPv6")},
			},
		},
		{
			Name: ptr.To("bservice1-IPv6"),
			ID:   ptr.To("bservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1-IPv6")},
			},
		},
	}
	slb5.Properties.Probes = []*armnetwork.Probe{
		{
			Name: ptr.To(svcPrefix + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port))),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port: ptr.To(int32(10080)),
			},
		},
		{
			Name: ptr.To(svcPrefix + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port))),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port: ptr.To(int32(10081)),
			},
		},
		{
			Name: ptr.To(svcPrefix + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port)) + ipv6Suffix),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port: ptr.To(int32(10080)),
			},
		},
		{
			Name: ptr.To(svcPrefix + string(service4.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service4.Spec.Ports[0].Port)) + ipv6Suffix),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port: ptr.To(int32(10081)),
			},
		},
	}

	// change to false to test that reconciliation will fix it (despite the fact that disable-tcp-reset was removed in 1.20)
	(slb5.Properties.LoadBalancingRules)[0].Properties.EnableTCPReset = to.Ptr(false)
	(slb5.Properties.LoadBalancingRules)[1].Properties.EnableTCPReset = to.Ptr(false)

	expectedSLb5 := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("aservice1"), service5, "Standard")
	(expectedSLb5.Properties.LoadBalancingRules)[0].Properties.DisableOutboundSnat = ptr.To(true)
	(expectedSLb5.Properties.LoadBalancingRules)[0].Properties.IdleTimeoutInMinutes = ptr.To(int32(4))
	(expectedSLb5.Properties.LoadBalancingRules)[1].Properties.DisableOutboundSnat = ptr.To(true)
	(expectedSLb5.Properties.LoadBalancingRules)[1].Properties.IdleTimeoutInMinutes = ptr.To(int32(4))
	expectedSLb5.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1")},
			},
		},
		{
			Name: ptr.To("bservice1"),
			ID:   ptr.To("bservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1")},
			},
		},
		{
			Name: ptr.To("aservice1-IPv6"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1-IPv6")},
			},
		},
		{
			Name: ptr.To("bservice1-IPv6"),
			ID:   ptr.To("bservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-bservice1-IPv6")},
			},
		},
	}

	service6 := getTestServiceDualStack("service1", v1.ProtocolUDP, nil, 80)
	lb6 := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("aservice1"), service6, "basic")
	lb6.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{}
	lb6.Properties.Probes = []*armnetwork.Probe{}
	expectedLB6 := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("aservice1"), service6, "basic")
	expectedLB6.Properties.Probes = []*armnetwork.Probe{}
	(expectedLB6.Properties.LoadBalancingRules)[0].Properties.Probe = nil
	(expectedLB6.Properties.LoadBalancingRules)[0].Properties.EnableTCPReset = nil
	(expectedLB6.Properties.LoadBalancingRules)[1].Properties.Probe = nil
	(expectedLB6.Properties.LoadBalancingRules)[1].Properties.EnableTCPReset = nil
	expectedLB6.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1")},
			},
		},
		{
			Name: ptr.To("aservice1-IPv6"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1-IPv6")},
			},
		},
	}

	service7 := getTestServiceDualStack("service1", v1.ProtocolUDP, nil, 80)
	service7.Spec.HealthCheckNodePort = 10081
	service7.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	lb7 := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("aservice1"), service7, "basic")
	lb7.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{}
	lb7.Properties.Probes = []*armnetwork.Probe{}
	expectedLB7 := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("aservice1"), service7, "basic")
	(expectedLB7.Properties.LoadBalancingRules)[0].Properties.Probe = &armnetwork.SubResource{
		ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/probes/aservice1-TCP-10081"),
	}
	(expectedLB7.Properties.LoadBalancingRules)[1].Properties.Probe = &armnetwork.SubResource{
		ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/probes/aservice1-TCP-10081-IPv6"),
	}
	(expectedLB7.Properties.LoadBalancingRules)[0].Properties.EnableTCPReset = nil
	(lb7.Properties.LoadBalancingRules)[0].Properties.DisableOutboundSnat = ptr.To(true)
	(expectedLB7.Properties.LoadBalancingRules)[1].Properties.EnableTCPReset = nil
	(lb7.Properties.LoadBalancingRules)[1].Properties.DisableOutboundSnat = ptr.To(true)
	expectedLB7.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1")},
			},
		},
		{
			Name: ptr.To("aservice1-IPv6"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1-IPv6")},
			},
		},
	}
	expectedLB7.Properties.Probes = []*armnetwork.Probe{
		{
			Name: ptr.To(svcPrefix + string(v1.ProtocolTCP) +
				"-" + strconv.Itoa(int(service7.Spec.HealthCheckNodePort))),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port:              ptr.To(int32(10081)),
				RequestPath:       ptr.To("/healthz"),
				Protocol:          to.Ptr(armnetwork.ProbeProtocolHTTP),
				IntervalInSeconds: ptr.To(int32(5)),
				ProbeThreshold:    ptr.To(int32(2)),
			},
		},
		{
			Name: ptr.To(svcPrefix + string(v1.ProtocolTCP) +
				"-" + strconv.Itoa(int(service7.Spec.HealthCheckNodePort)) + ipv6Suffix),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port:              ptr.To(int32(10081)),
				RequestPath:       ptr.To("/healthz"),
				Protocol:          to.Ptr(armnetwork.ProbeProtocolHTTP),
				IntervalInSeconds: ptr.To(int32(5)),
				ProbeThreshold:    ptr.To(int32(2)),
			},
		},
	}

	service8 := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)
	lb8 := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("anotherRG"), ptr.To("testCluster"), ptr.To("aservice1"), service8, "Standard")
	lb8.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{}
	lb8.Properties.Probes = []*armnetwork.Probe{}
	expectedLB8 := getTestLoadBalancerDualStack(ptr.To("testCluster"), ptr.To("anotherRG"), ptr.To("testCluster"), ptr.To("aservice1"), service8, "Standard")
	(expectedLB8.Properties.LoadBalancingRules)[0].Properties.DisableOutboundSnat = ptr.To(false)
	(expectedLB8.Properties.LoadBalancingRules)[1].Properties.DisableOutboundSnat = ptr.To(false)
	expectedLB8.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1")},
			},
		},
		{
			Name: ptr.To("aservice1-IPv6"),
			ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/testCluster/frontendIPConfigurations/aservice1-IPv6"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice1-IPv6")},
			},
		},
	}
	expectedLB8.Properties.Probes = []*armnetwork.Probe{
		{
			Name: ptr.To(svcPrefix + string(service8.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service7.Spec.Ports[0].Port))),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port:              ptr.To(int32(10080)),
				Protocol:          to.Ptr(armnetwork.ProbeProtocolTCP),
				IntervalInSeconds: ptr.To(int32(5)),
				ProbeThreshold:    ptr.To(int32(2)),
			},
		},
		{
			Name: ptr.To(svcPrefix + string(service8.Spec.Ports[0].Protocol) +
				"-" + strconv.Itoa(int(service7.Spec.Ports[0].Port)) + ipv6Suffix),
			Properties: &armnetwork.ProbePropertiesFormat{
				Port:              ptr.To(int32(10080)),
				Protocol:          to.Ptr(armnetwork.ProbeProtocolTCP),
				IntervalInSeconds: ptr.To(int32(5)),
				ProbeThreshold:    ptr.To(int32(2)),
			},
		},
	}

	testCases := []struct {
		desc                                      string
		service                                   v1.Service
		loadBalancerSKU                           string
		preConfigLBType                           string
		loadBalancerResourceGroup                 string
		disableOutboundSnat                       *bool
		wantLb                                    bool
		shouldRefreshLBAfterReconcileBackendPools bool
		existingLB                                *armnetwork.LoadBalancer
		expectedLB                                *armnetwork.LoadBalancer
		expectLBUpdate                            bool
		expectedGetLBError                        error
		expectedError                             error
	}{
		{
			desc: "reconcileLoadBalancer shall return the lb deeply equal to the existingLB if there's no " +
				"modification needed when wantLb == true",
			loadBalancerSKU: "basic",
			service:         service1,
			existingLB:      basicLb1,
			wantLb:          true,
			expectedLB:      basicLb1,
			expectedError:   nil,
		},
		{
			desc: "reconcileLoadBalancer shall return the lb deeply equal to the existingLB if there's no " +
				"modification needed when wantLb == false",
			loadBalancerSKU: "basic",
			service:         service2,
			existingLB:      basicLb2,
			wantLb:          false,
			expectedLB:      basicLb2,
			expectedError:   nil,
		},
		{
			desc:            "reconcileLoadBalancer shall remove and reconstruct the corresponding field of lb",
			loadBalancerSKU: "basic",
			service:         service3,
			existingLB:      modifiedLbs[0],
			wantLb:          true,
			expectedLB:      expectedLb1,
			expectLBUpdate:  true,
			expectedError:   nil,
		},
		{
			desc:            "reconcileLoadBalancer shall not raise an error",
			loadBalancerSKU: "basic",
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
			loadBalancerSKU:     "standard",
			service:             service4,
			disableOutboundSnat: ptr.To(true),
			existingLB:          existingSLB,
			wantLb:              true,
			expectedLB:          expectedSLb,
			expectLBUpdate:      true,
			expectedError:       nil,
		},
		{
			desc:                "reconcileLoadBalancer shall remove and reconstruct the corresponding field of lb and set enableTcpReset (false => true) in lbRule",
			loadBalancerSKU:     "standard",
			service:             service5,
			disableOutboundSnat: ptr.To(true),
			existingLB:          slb5,
			wantLb:              true,
			expectedLB:          expectedSLb5,
			expectLBUpdate:      true,
			expectedError:       nil,
		},
		{
			desc:            "reconcileLoadBalancer shall reconcile UDP services",
			loadBalancerSKU: "basic",
			service:         service6,
			existingLB:      lb6,
			wantLb:          true,
			expectedLB:      expectedLB6,
			expectLBUpdate:  true,
			expectedError:   nil,
		},
		{
			desc:            "reconcileLoadBalancer shall reconcile probes for local traffic policy UDP services",
			loadBalancerSKU: "basic",
			service:         service7,
			existingLB:      lb7,
			wantLb:          true,
			expectedLB:      expectedLB7,
			expectLBUpdate:  true,
			expectedError:   nil,
		},
		{
			desc:                      "reconcileLoadBalancer in other resource group",
			loadBalancerSKU:           "standard",
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
			loadBalancerSKU: "basic",
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
			expectedGetLBError:                        &azcore.ResponseError{ErrorCode: "error"},
			expectedError:                             fmt.Errorf("reconcileLoadBalancer for service (default/service1): failed to get load balancer testCluster: %w", errors.New("error")),
		},
	}

	for _, test := range testCases {
		test := test
		t.Run(test.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			az := GetTestCloud(ctrl)
			az.Config.LoadBalancerSKU = test.loadBalancerSKU
			az.Config.DisableOutboundSNAT = test.disableOutboundSnat
			if test.preConfigLBType != "" {
				az.Config.PreConfiguredBackendPoolLoadBalancerTypes = test.preConfigLBType
			}
			az.LoadBalancerResourceGroup = test.loadBalancerResourceGroup

			clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 3, 3)
			setMockEnvDualStack(az, expectedInterfaces, expectedVirtualMachines, 1)

			service := test.service
			setServiceLoadBalancerIP(&service, "1.2.3.4")
			setServiceLoadBalancerIP(&service, "fd00::eef0")

			_, err := az.NetworkClientFactory.GetPublicIPAddressClient().CreateOrUpdate(context.TODO(), "rg", "pipName", armnetwork.PublicIPAddress{
				Name: ptr.To("pipName"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress:              ptr.To("1.2.3.4"),
					PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
				},
			})
			assert.NoError(t, err)
			_, err = az.NetworkClientFactory.GetPublicIPAddressClient().CreateOrUpdate(context.TODO(), "rg", "pipName-IPv6", armnetwork.PublicIPAddress{
				Name: ptr.To("pipName-IPv6"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPAddress:              ptr.To("fd00::eef0"),
					PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv6),
				},
			})
			assert.NoError(t, err)

			mockLBsClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
			mockLBsClient.EXPECT().List(gomock.Any(), az.getLoadBalancerResourceGroup()).Return([]*armnetwork.LoadBalancer{test.existingLB}, nil)
			mockLBsClient.EXPECT().Get(gomock.Any(), az.getLoadBalancerResourceGroup(), *test.existingLB.Name, gomock.Any()).Return(test.existingLB, test.expectedGetLBError).AnyTimes()
			expectLBUpdateCount := 1
			if test.expectLBUpdate {
				expectLBUpdateCount++
			}
			mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.getLoadBalancerResourceGroup(), gomock.Any(), gomock.Any()).Return(nil, nil).Times(expectLBUpdateCount)

			_, err = az.NetworkClientFactory.GetLoadBalancerClient().CreateOrUpdate(context.TODO(), az.getLoadBalancerResourceGroup(), "lb1", *test.existingLB)
			assert.NoError(t, err)

			mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
			if test.shouldRefreshLBAfterReconcileBackendPools {
				mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, test.expectedLB, test.expectedError)
			}
			mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
				return false, false, lb, nil
			}).AnyTimes()
			mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

			mockPLSRepo := az.plsRepo.(*privatelinkservice.MockRepository)
			mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()

			lb, _, rerr := az.reconcileLoadBalancer(context.TODO(), "testCluster", &service, clusterResources.nodes, test.wantLb)
			if test.expectedError != nil {
				assert.EqualError(t, rerr, test.expectedError.Error())
			} else {
				assert.Equal(t, *test.expectedLB, *lb)
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

	setMockPublicIPs(az, 1, v4Enabled, v6Enabled)

	lb1 := getTestLoadBalancer(ptr.To("lb1"), ptr.To("rg"), ptr.To("testCluster"),
		ptr.To("aservice1"), internalService, "Basic")
	lb1.Properties.FrontendIPConfigurations = nil
	lb2 := getTestLoadBalancer(ptr.To("lb2"), ptr.To("rg"), ptr.To("testCluster"),
		ptr.To("aservice1"), internalService, "Basic")
	lb2.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress:  &armnetwork.PublicIPAddress{ID: ptr.To("testCluster-aservice1")},
				PrivateIPAddress: ptr.To("private"),
			},
		},
	}
	lb3 := getTestLoadBalancer(ptr.To("lb3"), ptr.To("rg"), ptr.To("testCluster"),
		ptr.To("test1"), internalService, "Basic")
	lb3.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("bservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress:  &armnetwork.PublicIPAddress{ID: ptr.To("testCluster-bservice1")},
				PrivateIPAddress: ptr.To("private"),
			},
		},
	}
	lb4 := getTestLoadBalancer(ptr.To("lb4"), ptr.To("rg"), ptr.To("testCluster"),
		ptr.To("aservice1"), service, "Basic")
	lb4.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress:  &armnetwork.PublicIPAddress{ID: nil},
				PrivateIPAddress: ptr.To("private"),
			},
		},
	}
	lb5 := getTestLoadBalancer(ptr.To("lb5"), ptr.To("rg"), ptr.To("testCluster"),
		ptr.To("aservice1"), service, "Basic")
	lb5.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress:  nil,
				PrivateIPAddress: ptr.To("private"),
			},
		},
	}
	lb6 := getTestLoadBalancer(ptr.To("lb6"), ptr.To("rg"), ptr.To("testCluster"),
		ptr.To("aservice1"), service, "Basic")
	lb6.Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{
		{
			Name: ptr.To("aservice1"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress:  &armnetwork.PublicIPAddress{ID: ptr.To("illegal/id/")},
				PrivateIPAddress: ptr.To("private"),
			},
		},
	}

	testCases := []struct {
		desc           string
		service        *v1.Service
		lb             *armnetwork.LoadBalancer
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
			lb:      lb1,
		},
		{
			desc:           "getServiceLoadBalancerStatus shall return private ip if service is internal",
			service:        &internalService,
			lb:             lb2,
			expectedStatus: &v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: "private"}}},
		},
		{
			desc: "getServiceLoadBalancerStatus shall return nil if lb.Properties.FrontendIPConfigurations.name != " +
				"az.getDefaultFrontendIPConfigName(service)",
			service: &internalService,
			lb:      lb3,
		},
		{
			desc: "getServiceLoadBalancerStatus shall report error if the id of lb's " +
				"public ip address cannot be read",
			service:       &service,
			lb:            lb4,
			expectedError: true,
		},
		{
			desc:          "getServiceLoadBalancerStatus shall report error if lb's public ip address cannot be read",
			service:       &service,
			lb:            lb5,
			expectedError: true,
		},
		{
			desc:          "getServiceLoadBalancerStatus shall report error if id of lb's public ip address is illegal",
			service:       &service,
			lb:            lb6,
			expectedError: true,
		},
		{
			desc: "getServiceLoadBalancerStatus shall return the corresponding " +
				"lb status if everything is good",
			service:        &service,
			lb:             lb2,
			expectedStatus: &v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: "1.2.3.4"}}},
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			status, _, _, err := az.getServiceLoadBalancerStatus(context.TODO(), test.service, test.lb)
			assert.Equal(t, test.expectedStatus, status)
			assert.Equal(t, test.expectedError, err != nil)
		})
	}
}

func TestSafeDeletePublicIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc          string
		pip           *armnetwork.PublicIPAddress
		lb            *armnetwork.LoadBalancer
		listError     error
		expectedError error
	}{
		{
			desc: "safeDeletePublicIP shall delete corresponding ip configurations and lb rules",
			pip: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPConfiguration: &armnetwork.IPConfiguration{
						ID: ptr.To("id1"),
					},
				},
			},
			lb: &armnetwork.LoadBalancer{
				Name: ptr.To("lb1"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
						{
							ID: ptr.To("id1"),
							Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
								LoadBalancingRules: []*armnetwork.SubResource{{ID: ptr.To("rules1")}},
							},
						},
					},
					LoadBalancingRules: []*armnetwork.LoadBalancingRule{{ID: ptr.To("rules1")}},
				},
			},
		},
		{
			desc: "safeDeletePublicIP should return error if failed to list pip",
			pip: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPConfiguration: &armnetwork.IPConfiguration{
						ID: ptr.To("id1"),
					},
				},
			},
			listError:     &azcore.ResponseError{ErrorCode: "error"},
			expectedError: &azcore.ResponseError{ErrorCode: "error"},
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			if test.pip != nil &&
				test.pip.Properties != nil &&
				test.pip.Properties.IPConfiguration != nil {
				mockPIPsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]*armnetwork.PublicIPAddress{test.pip}, test.listError)
			}
			mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "pip1", gomock.Any()).Return(nil, nil).AnyTimes()
			mockPIPsClient.EXPECT().Delete(gomock.Any(), "rg", "pip1").Return(nil).AnyTimes()
			_, err := az.NetworkClientFactory.GetPublicIPAddressClient().CreateOrUpdate(context.TODO(), "rg", "pip1", armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					IPConfiguration: &armnetwork.IPConfiguration{
						ID: ptr.To("id1"),
					},
				},
			})
			assert.NoError(t, err)
			service := getTestService("test1", v1.ProtocolTCP, nil, false, 80)
			if test.listError == nil {
				mockLBsClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
				mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
			}
			rerr := az.safeDeletePublicIP(context.TODO(), &service, "rg", test.pip, test.lb)
			if test.expectedError == nil {
				assert.Equal(t, 0, len(test.lb.Properties.FrontendIPConfigurations))
				assert.Equal(t, 0, len(test.lb.Properties.LoadBalancingRules))
				assert.NoError(t, rerr)
			} else {
				assert.Equal(t, rerr.Error(), test.listError.Error())
			}
		})
	}
}

func TestReconcilePublicIPsCommon(t *testing.T) {
	deleteUnwantedPIPsAndCreateANewOneclientGet := func(client *mock_publicipaddressclient.MockInterface) {
		client.EXPECT().Get(gomock.Any(), "rg", "testCluster-atest1", gomock.Any()).Return(&armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-atest1")}, nil).Times(1)
		client.EXPECT().Get(gomock.Any(), "rg", "testCluster-atest1-IPv6", gomock.Any()).Return(&armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-atest1-IPv6")}, nil).Times(1)
	}
	getPIPAddMissingOne := func(client *mock_publicipaddressclient.MockInterface) {
		client.EXPECT().Get(gomock.Any(), "rg", "testCluster-atest1-IPv6", gomock.Any()).Return(&armnetwork.PublicIPAddress{ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-atest1-IPv6")}, nil).Times(1)
	}

	testCases := []struct {
		desc                        string
		annotations                 map[string]string
		existingPIPs                []*armnetwork.PublicIPAddress
		wantLb                      bool
		expectedIDs                 []string
		expectedPIPs                []*armnetwork.PublicIPAddress // len(expectedPIPs) <= 2
		expectedError               bool
		expectedCreateOrUpdateCount int
		expectedDeleteCount         int
		expectedClientGet           *func(client *mock_publicipaddressclient.MockInterface)
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
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1"),
				},
			},
			expectedCreateOrUpdateCount: 0,
			expectedDeleteCount:         0,
		},
		{
			desc:   "shall delete unwanted pips and create new ones",
			wantLb: true,
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("pip1-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("fd00::eef0"),
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
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
				},
				{
					Name: ptr.To("pip2"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip2"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
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
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("pip2"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip2"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("testPIP"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("testPIP-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						IPAddress:                ptr.To("fd00::eef0"),
					},
				},
			},
			expectedPIPs: []*armnetwork.PublicIPAddress{
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Name: ptr.To("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Name: ptr.To("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						IPAddress:                ptr.To("fd00::eef0"),
					},
				},
			},
			expectedCreateOrUpdateCount: 0,
			expectedDeleteCount:         2,
		},
		{
			desc:   "shall not delete unwanted PIP when there are other service references",
			wantLb: true,
			annotations: map[string]string{
				consts.ServiceAnnotationPIPNameDualStack[false]: "testPIP",
				consts.ServiceAnnotationPIPNameDualStack[true]:  "testPIP-IPv6",
			},
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("pip2"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip2"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1,default/test2")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("pip1-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv6),
						IPAddress:              ptr.To("fd00::eef0"),
					},
				},
				{
					Name: ptr.To("pip2-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip2-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1,default/test2")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv6),
						IPAddress:              ptr.To("fd00::eef0"),
					},
				},
				{
					Name: ptr.To("testPIP"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("testPIP-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						IPAddress:                ptr.To("fd00::eef0"),
					},
				},
			},
			expectedPIPs: []*armnetwork.PublicIPAddress{
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Name: ptr.To("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Name: ptr.To("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						IPAddress:                ptr.To("fd00::eef0"),
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
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1"),
					Tags: map[string]*string{
						consts.ServiceTagKey:       ptr.To("default/test1"),
						consts.LegacyServiceTagKey: ptr.To("foo"), // It should be ignored when ServiceTagKey is present.
					},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("pip2"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip2"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("testPIP"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("testPIP-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						IPAddress:                ptr.To("fd00::eef0"),
					},
				},
			},
			expectedPIPs: []*armnetwork.PublicIPAddress{
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Name: ptr.To("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						IPTags: []*armnetwork.IPTag{
							{
								IPTagType: ptr.To("tag1"),
								Tag:       ptr.To("tag1value"),
							},
						},
					},
				},
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Name: ptr.To("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						IPTags: []*armnetwork.IPTag{
							{
								IPTagType: ptr.To("tag1"),
								Tag:       ptr.To("tag1value"),
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
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("testPIP"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						IPTags: []*armnetwork.IPTag{
							{
								IPTagType: ptr.To("tag1"),
								Tag:       ptr.To("tag1value"),
							},
						},
						IPAddress: ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("testPIP-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						IPTags: []*armnetwork.IPTag{
							{
								IPTagType: ptr.To("tag1"),
								Tag:       ptr.To("tag1value"),
							},
						},
						IPAddress: ptr.To("fd00::eef0"),
					},
				},
			},
			expectedPIPs: []*armnetwork.PublicIPAddress{
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Name: ptr.To("testPIP"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						IPTags: []*armnetwork.IPTag{
							{
								IPTagType: ptr.To("tag1"),
								Tag:       ptr.To("tag1value"),
							},
						},
						IPAddress: ptr.To("1.2.3.4"),
					},
				},
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Name: ptr.To("testPIP-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						IPTags: []*armnetwork.IPTag{
							{
								IPTagType: ptr.To("tag1"),
								Tag:       ptr.To("tag1value"),
							},
						},
						IPAddress: ptr.To("fd00::eef0"),
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
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("pip2"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip2"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("testPIP"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("pip2-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip2-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						IPAddress:                ptr.To("fd00::eef0"),
					},
				},
				{
					Name: ptr.To("testPIP-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						IPAddress:                ptr.To("fd00::eef0"),
					},
				},
			},
			expectedPIPs: []*armnetwork.PublicIPAddress{
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP"),
					Name: ptr.To("testPIP"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testPIP-IPv6"),
					Name: ptr.To("testPIP-IPv6"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						IPAddress:                ptr.To("fd00::eef0"),
					},
				},
			},
			expectedCreateOrUpdateCount: 0,
			expectedDeleteCount:         2,
		},
		{
			desc:   "shall delete the unwanted PIP name from service tag and shall not delete it if there is other reference",
			wantLb: false,
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1,default/test2")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress:              ptr.To("1.2.3.4"),
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
				{
					Name: ptr.To("pip1-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1-IPv6"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1,default/test2")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress:                ptr.To("fd00::eef0"),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
					},
				},
			},
			expectedCreateOrUpdateCount: 2,
		},
		{
			desc:   "shall create the missing one",
			wantLb: true,
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("testCluster-atest1"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-atest1"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
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
			savedPips := make(map[string]*armnetwork.PublicIPAddress)
			createOrUpdateCount := 0
			var m sync.Mutex
			az := GetTestCloud(ctrl)
			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			creator := mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any()).AnyTimes()
			creator.DoAndReturn(func(_ context.Context, _ string, publicIPAddressName string, parameters armnetwork.PublicIPAddress) (result *armnetwork.PublicIPAddress, rerr error) {
				m.Lock()
				deletedPips[publicIPAddressName] = false
				savedPips[publicIPAddressName] = &parameters
				createOrUpdateCount++
				m.Unlock()
				return nil, nil
			})

			if test.expectedClientGet != nil {
				(*test.expectedClientGet)(mockPIPsClient)
			}
			service := getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80)
			service.Annotations = test.annotations
			for _, pip := range test.existingPIPs {
				savedPips[*pip.Name] = pip
				getter := mockPIPsClient.EXPECT().Get(gomock.Any(), "rg", *pip.Name, gomock.Any()).AnyTimes()
				getter.DoAndReturn(func(_ context.Context, _ string, publicIPAddressName string, _ *string) (result *armnetwork.PublicIPAddress, rerr error) {
					m.Lock()
					deletedValue, deletedContains := deletedPips[publicIPAddressName]
					savedPipValue, savedPipContains := savedPips[publicIPAddressName]
					m.Unlock()

					if (!deletedContains || !deletedValue) && savedPipContains {
						return savedPipValue, nil
					}

					return nil, &azcore.ResponseError{StatusCode: http.StatusNotFound}
				})
				deleter := mockPIPsClient.EXPECT().Delete(gomock.Any(), "rg", *pip.Name).Return(nil).AnyTimes()
				deleter.Do(func(_ context.Context, _ string, publicIPAddressName string) error {
					m.Lock()
					deletedPips[publicIPAddressName] = true
					m.Unlock()
					return nil
				})

				_, err := az.NetworkClientFactory.GetPublicIPAddressClient().CreateOrUpdate(context.TODO(), "rg", ptr.Deref(pip.Name, ""), *pip)
				assert.NoError(t, err)

				// Clear create or update count to prepare for main execution
				createOrUpdateCount = 0
			}
			lister := mockPIPsClient.EXPECT().List(gomock.Any(), "rg").AnyTimes()
			lister.DoAndReturn(func(_ context.Context, _ string) (result []*armnetwork.PublicIPAddress, rerr error) {
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

			pips, err := az.reconcilePublicIPs(context.TODO(), "testCluster", &service, "", test.wantLb)
			if !test.expectedError {
				assert.NoError(t, err)
			}
			// Check IDs
			if len(test.expectedIDs) != 0 {
				ids := []string{}
				for _, pip := range pips {
					ids = append(ids, *pip.ID)
				}
				assert.Truef(t, compareStrings(test.expectedIDs, ids),
					"expectedIDs %q, IDs %q", test.expectedIDs, ids)
			}
			// Check PIPs
			if len(test.expectedPIPs) != 0 {
				pipsNames := []*string{}
				for _, pip := range pips {
					pipsNames = append(pipsNames, pip.Name)
				}
				assert.Equal(t, len(test.expectedPIPs), len(pips), pipsNames)
				pipsOrdered := []*armnetwork.PublicIPAddress{}
				if len(test.expectedPIPs) == 1 {
					pipsOrdered = append(pipsOrdered, pips[0])
				} else {
					// len(test.expectedPIPs) == 2
					if ptr.Deref(test.expectedPIPs[0].Name, "") == ptr.Deref(pips[0].Name, "") {
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

					if test.expectedPIPs[i].Properties != nil {
						sortIPTags(&test.expectedPIPs[i].Properties.IPTags)
					}

					if pip.Properties != nil {
						sortIPTags(&pip.Properties.IPTags)
					}

					assert.Equal(t, test.expectedPIPs[i].Properties,
						pip.Properties, "pip name %q", *pip.Name)
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

func TestReconcilePublicIPsDeleteUnwantedFromOtherRG(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc                string
		annotations         map[string]string
		existingPIPs        []*armnetwork.PublicIPAddress
		wantLb              bool
		listFailed          bool
		expectedError       bool
		expectedDeleteCount int
	}{
		{
			desc:   "shall delete managed pip when switching to user provided pip in another resource group",
			wantLb: true,
			annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerResourceGroup: "rg-b",
				consts.ServiceAnnotationPIPNameDualStack[false]:   "pip1",
			},
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg-a/providers/Microsoft.Network/publicIPAddresses/pip1"),
					Name: ptr.To("pip1"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress:              ptr.To("1.2.3.4"),
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
				{
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip2"),
					Name: ptr.To("pip2"),
					Tags: map[string]*string{consts.ServiceTagKey: ptr.To("default/test1")},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress:              ptr.To("2.3.4.5"),
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedDeleteCount: 1,
		},
		{
			desc:   "shall return an error if failed to list pips in rg",
			wantLb: true,
			annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerResourceGroup: "rg-b",
				consts.ServiceAnnotationPIPNameDualStack[false]:   "pip1",
			},
			existingPIPs:  []*armnetwork.PublicIPAddress{{}},
			listFailed:    true,
			expectedError: true,
		},
	}

	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			service := getTestService("test1", v1.ProtocolTCP, nil, false, 80)
			service.Annotations = test.annotations

			// Set up mock clients
			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)

			// Set up mock List call to return the managed pip in rg-a
			mockPIPsClient.EXPECT().List(gomock.Any(), "rg-b").Return(test.existingPIPs[:1], nil)
			if test.listFailed {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(nil, errors.New("failed to list pips"))
			} else {
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(test.existingPIPs[1:], nil)
				mockPIPsClient.EXPECT().Get(gomock.Any(), "rg-b", "pip1", gomock.Any()).Return(test.existingPIPs[0], nil)
			}

			// Set up mock Delete call for the managed pip
			if test.expectedDeleteCount > 0 {
				mockPIPsClient.EXPECT().Delete(gomock.Any(), "rg", "pip2").Return(nil).Times(test.expectedDeleteCount)
			}

			// No need to mock Get or CreateOrUpdate since we're using an existing pip

			_, err := az.reconcilePublicIPs(context.TODO(), "testCluster", &service, "", test.wantLb)
			if test.expectedError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
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
		existingPIPs            []*armnetwork.PublicIPAddress
		expectedPIP             *armnetwork.PublicIPAddress
		foundDNSLabelAnnotation bool
		isIPv6                  bool
		useSLB                  bool
		shouldPutPIP            bool
		expectedError           bool
	}{
		{
			desc:         "shall return existed IPv4 PIP if there is any",
			pipName:      "pip1",
			existingPIPs: []*armnetwork.PublicIPAddress{{Name: ptr.To("pip1")}},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				ID:   ptr.To(expectedPIPID),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
				},
				Tags: map[string]*string{},
			},
			shouldPutPIP: true,
		},
		{
			desc:         "shall return existed IPv6 PIP if there is any",
			pipName:      "pip1-IPv6",
			existingPIPs: []*armnetwork.PublicIPAddress{{Name: ptr.To("pip1-IPv6")}},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1-IPv6"),
				ID: ptr.To(rgprefix +
					"/providers/Microsoft.Network/publicIPAddresses/pip1-IPv6"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
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
			existingPIPs: []*armnetwork.PublicIPAddress{{
				Name:       ptr.To("pip1"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{},
			}},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				ID:   ptr.To(expectedPIPID),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
						DomainNameLabel: ptr.To("newdns"),
					},
					PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
				},
				Tags: map[string]*string{consts.ServiceUsingDNSKey: ptr.To("default/test1")},
			},
			shouldPutPIP: true,
		},
		{
			desc:                    "shall delete DNS from PIP if DNS label is set empty",
			pipName:                 "pip1",
			foundDNSLabelAnnotation: true,
			existingPIPs: []*armnetwork.PublicIPAddress{{
				Name: ptr.To("pip1"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
						DomainNameLabel: ptr.To("previousdns"),
					},
				},
			}},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				ID:   ptr.To(expectedPIPID),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings:            nil,
					PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
				},
				Tags: map[string]*string{},
			},
			shouldPutPIP: true,
		},
		{
			desc:                    "shall not delete DNS from PIP if DNS label annotation is not set",
			pipName:                 "pip1",
			foundDNSLabelAnnotation: false,
			existingPIPs: []*armnetwork.PublicIPAddress{{
				Name: ptr.To("pip1"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
						DomainNameLabel: ptr.To("previousdns"),
					},
				},
			}},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				ID:   ptr.To(expectedPIPID),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
						DomainNameLabel: ptr.To("previousdns"),
					},
					PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
				},
			},
		},
		{
			desc:                    "shall update existed PIP's dns label for IPv6",
			pipName:                 "pip1",
			inputDNSLabel:           "newdns",
			foundDNSLabelAnnotation: true,
			isIPv6:                  true,
			existingPIPs: []*armnetwork.PublicIPAddress{{
				Name:       ptr.To("pip1"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{},
			}},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				ID:   ptr.To(expectedPIPID),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
						DomainNameLabel: ptr.To("newdns"),
					},
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
				},
				Tags: map[string]*string{consts.ServiceUsingDNSKey: ptr.To("default/test1")},
			},
			shouldPutPIP: true,
		},
		{
			desc:                    "shall update existed PIP's dns label for IPv6",
			pipName:                 "pip1",
			inputDNSLabel:           "newdns",
			foundDNSLabelAnnotation: true,
			isIPv6:                  true,
			existingPIPs: []*armnetwork.PublicIPAddress{{
				Name: ptr.To("pip1"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
						DomainNameLabel: ptr.To("previousdns"),
					},
				},
			}},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				ID:   ptr.To(expectedPIPID),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
						DomainNameLabel: ptr.To("newdns"),
					},
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
				},
				Tags: map[string]*string{
					"k8s-azure-dns-label-service": ptr.To("default/test1"),
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
			existingPIPs: []*armnetwork.PublicIPAddress{{
				Name: ptr.To("pip1"),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
						DomainNameLabel: ptr.To("previousdns"),
					},
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
				},
			}},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				ID:   ptr.To(expectedPIPID),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
						DomainNameLabel: ptr.To("newdns"),
					},
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
				},
				Tags: map[string]*string{
					"k8s-azure-dns-label-service": ptr.To("default/test1"),
				},
			},
			shouldPutPIP: true,
		},
		{
			desc:                    "shall report an conflict error if the DNS label is conflicted",
			pipName:                 "pip1",
			inputDNSLabel:           "test",
			foundDNSLabelAnnotation: true,
			existingPIPs: []*armnetwork.PublicIPAddress{{
				Name: ptr.To("pip1"),
				Tags: map[string]*string{consts.ServiceUsingDNSKey: ptr.To("test1")},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
						DomainNameLabel: ptr.To("previousdns"),
					},
				},
			}},
			expectedError: true,
		},
		{
			desc:          "shall return the pip without calling PUT API if the tags are good",
			pipName:       "pip1",
			inputDNSLabel: "test",
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					ID:   ptr.To(expectedPIPID),
					Tags: map[string]*string{
						consts.ServiceUsingDNSKey: ptr.To("default/test1"),
						consts.ServiceTagKey:      ptr.To("default/test1"),
					},
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
							DomainNameLabel: ptr.To("test"),
						},
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				ID:   ptr.To(expectedPIPID),
				Tags: map[string]*string{
					consts.ServiceUsingDNSKey: ptr.To("default/test1"),
					consts.ServiceTagKey:      ptr.To("default/test1"),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					DNSSettings: &armnetwork.PublicIPAddressDNSSettings{
						DomainNameLabel: ptr.To("test"),
					},
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
				},
			},
		},
		{
			desc:    "shall tag the service name to the pip correctly",
			pipName: "pip1",
			existingPIPs: []*armnetwork.PublicIPAddress{
				{Name: ptr.To("pip1")},
			},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				ID:   ptr.To(expectedPIPID),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
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
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
					},
				},
			},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				ID:   ptr.To(expectedPIPID),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
				},
				Tags: map[string]*string{},
				SKU: &armnetwork.PublicIPAddressSKU{
					Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
				},
			},
			shouldPutPIP: true,
		},
		{
			desc:         "shall update pip tags if there is any change",
			pipName:      "pip1",
			existingPIPs: []*armnetwork.PublicIPAddress{{Name: ptr.To("pip1"), Tags: map[string]*string{"a": ptr.To("b")}}},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Tags: map[string]*string{"a": ptr.To("c")},
				ID:   ptr.To(expectedPIPID),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
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
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
					Tags: map[string]*string{"a": ptr.To("b")},
				},
			},
			expectedPIP: &armnetwork.PublicIPAddress{
				Name: ptr.To("pip1"),
				Tags: map[string]*string{"a": ptr.To("b")},
				ID:   ptr.To(expectedPIPID),
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
					IPAddress:              ptr.To("1.2.3.4"),
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
				az.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			}

			service := getTestService("test1", v1.ProtocolTCP, nil, test.isIPv6, 80)
			service.ObjectMeta.Annotations = test.additionalAnnotations
			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			if test.shouldPutPIP {
				mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ string, parameters armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
					if len(test.existingPIPs) != 0 {
						test.existingPIPs[0] = &parameters
					} else {
						test.existingPIPs = append(test.existingPIPs, &parameters)
					}
					return nil, nil
				}).AnyTimes()
			}
			mockPIPsClient.EXPECT().Get(gomock.Any(), "rg", test.pipName, gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ string, _ *string) (*armnetwork.PublicIPAddress, error) {
				return test.existingPIPs[0], nil
			}).MaxTimes(1)
			mockPIPsClient.EXPECT().List(gomock.Any(), "rg").DoAndReturn(func(_ context.Context, _ string) ([]*armnetwork.PublicIPAddress, error) {
				var basicPIP *armnetwork.PublicIPAddress
				if len(test.existingPIPs) == 0 {
					basicPIP = &armnetwork.PublicIPAddress{
						Name: ptr.To(test.pipName),
					}
				} else {
					basicPIP = test.existingPIPs[0]
				}

				basicPIP.ID = ptr.To(rgprefix +
					"/providers/Microsoft.Network/publicIPAddresses/" + test.pipName)

				if basicPIP.Properties == nil {
					return []*armnetwork.PublicIPAddress{basicPIP}, nil
				}

				if test.isIPv6 {
					basicPIP.Properties.PublicIPAddressVersion = to.Ptr(armnetwork.IPVersionIPv6)
					basicPIP.Properties.PublicIPAllocationMethod = to.Ptr(armnetwork.IPAllocationMethodDynamic)
				} else {
					basicPIP.Properties.PublicIPAddressVersion = to.Ptr(armnetwork.IPVersionIPv4)
				}

				return []*armnetwork.PublicIPAddress{basicPIP}, nil
			}).AnyTimes()

			pip, err := az.ensurePublicIPExists(context.TODO(), &service, test.pipName, test.inputDNSLabel, "", false, test.foundDNSLabelAnnotation, test.isIPv6)
			assert.Equal(t, test.expectedError, err != nil, "unexpectedly encountered (or not) error: %v", err)
			if test.expectedID != "" {
				assert.Equal(t, test.expectedID, ptr.Deref(pip.ID, ""))
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
	az.LoadBalancerSKU = consts.LoadBalancerSKUStandard
	service := getTestServiceDualStack("test1", v1.ProtocolTCP, nil, 80)

	exLocName := "microsoftlosangeles1"

	testcases := []struct {
		desc        string
		pipName     string
		expectedPIP *armnetwork.PublicIPAddress
		isIPv6      bool
	}{
		{
			desc:    "should create a pip with extended location",
			pipName: "pip1",
			expectedPIP: &armnetwork.PublicIPAddress{
				Name:     ptr.To("pip1"),
				Location: &az.Location,
				ExtendedLocation: &armnetwork.ExtendedLocation{
					Name: ptr.To("microsoftlosangeles1"),
					Type: to.Ptr(armnetwork.ExtendedLocationTypesEdgeZone),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					ProvisioningState:        nil,
				},
				Tags: map[string]*string{
					consts.ServiceTagKey:  ptr.To("default/test1"),
					consts.ClusterNameKey: ptr.To(""),
				},
			},
			isIPv6: false,
		},
		{
			desc:    "should create a pip with extended location for IPv6",
			pipName: "pip1-IPv6",
			expectedPIP: &armnetwork.PublicIPAddress{
				Name:     ptr.To("pip1-IPv6"),
				Location: &az.Location,
				ExtendedLocation: &armnetwork.ExtendedLocation{
					Name: ptr.To("microsoftlosangeles1"),
					Type: to.Ptr(armnetwork.ExtendedLocationTypesEdgeZone),
				},
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
					ProvisioningState:        nil,
				},
				Tags: map[string]*string{
					consts.ServiceTagKey:  ptr.To("default/test1"),
					consts.ClusterNameKey: ptr.To(""),
				},
			},
			isIPv6: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			first := mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.PublicIPAddress{}, nil).Times(2)
			mockPIPsClient.EXPECT().Get(gomock.Any(), "rg", tc.pipName, gomock.Any()).Return(tc.expectedPIP, nil).After(first)

			mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", tc.pipName, gomock.Any()).
				DoAndReturn(func(_ context.Context, _ string, _ string, publicIPAddressParameters armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
					assert.NotNil(t, publicIPAddressParameters)
					assert.NotNil(t, publicIPAddressParameters.ExtendedLocation)
					assert.Equal(t, *publicIPAddressParameters.ExtendedLocation.Name, exLocName)
					assert.Equal(t, *publicIPAddressParameters.ExtendedLocation.Type, armnetwork.ExtendedLocationTypesEdgeZone)
					// Edge zones don't support availability zones.
					assert.Nil(t, publicIPAddressParameters.Zones)
					return nil, nil
				}).Times(1)
			pip, err := az.ensurePublicIPExists(context.TODO(), &service, tc.pipName, "", "", false, false, tc.isIPv6)
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
			az.LoadBalancerSKU = consts.LoadBalancerSKUBasic
			service := getTestService("test1", v1.ProtocolTCP, nil, false, 80)
			v4Enabled, v6Enabled := getIPFamiliesEnabled(&service)
			service.Spec.Type = test.serviceType
			setMockPublicIPs(az, 1, v4Enabled, v6Enabled)
			mockLBsClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
			if test.existsLb {
				mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil)
			}
			if test.lbHasDeletionTimestamp {
				service.ObjectMeta.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			}
			if test.existsLb {
				lb := &armnetwork.LoadBalancer{
					Name: ptr.To("vmas"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
							{
								Name: ptr.To("atest1"),
								Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
									PublicIPAddress: &armnetwork.PublicIPAddress{ID: ptr.To("testCluster-aservice1")},
								},
							},
						},
					},
				}
				_, err := az.NetworkClientFactory.GetLoadBalancerClient().CreateOrUpdate(context.TODO(), "rg", *lb.Name, *lb)
				assert.NoError(t, err)
				mockLBsClient.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.LoadBalancer{lb}, nil)
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
			mockVMSet.EXPECT().GetAgentPoolVMSetNames(gomock.Any(), gomock.Any()).Return(to.SliceOfPtrs("vmas"), nil).MaxTimes(1)
			mockVMSet.EXPECT().GetPrimaryVMSetName().Return(az.Config.PrimaryAvailabilitySetName).MaxTimes(3)
			az.VMSet = mockVMSet

			shouldUpdateLoadBalancer, err := az.shouldUpdateLoadBalancer(context.TODO(), testClusterName, &service, existingNodes)
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
		ptr.To("ns1/svc1,ns2/svc2"),
		ptr.To(" ns1/svc1, ns2/svc2 "),
		ptr.To("ns1/svc1,"),
		ptr.To(""),
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
	pips := []*armnetwork.PublicIPAddress{
		{Tags: nil},
		{Tags: map[string]*string{}},
		{Tags: map[string]*string{consts.ServiceTagKey: ptr.To("ns1/svc1")}},
		{Tags: map[string]*string{consts.ServiceTagKey: ptr.To("ns1/svc1,ns2/svc2")}},
		{Tags: map[string]*string{consts.ServiceTagKey: ptr.To("ns2/svc2,ns3/svc3")}},
	}
	serviceNames := []string{"ns2/svc2", "ns3/svc3"}
	expectedTags := []map[string]*string{
		{consts.ServiceTagKey: ptr.To("ns2/svc2,ns3/svc3")},
		{consts.ServiceTagKey: ptr.To("ns2/svc2,ns3/svc3")},
		{consts.ServiceTagKey: ptr.To("ns1/svc1,ns2/svc2,ns3/svc3")},
		{consts.ServiceTagKey: ptr.To("ns1/svc1,ns2/svc2,ns3/svc3")},
		{consts.ServiceTagKey: ptr.To("ns2/svc2,ns3/svc3")},
	}

	flags := []bool{true, true, true, true, false}

	for i, pip := range pips {
		addedNew, _ := bindServicesToPIP(pip, serviceNames, false)
		assert.Equal(t, expectedTags[i], pip.Tags)
		assert.Equal(t, flags[i], addedNew)
	}
}

func TestUnbindServiceFromPIP(t *testing.T) {
	tests := []struct {
		Name                      string
		InputTags                 map[string]*string
		InputIsUserAssigned       bool
		ExpectedTags              map[string]*string
		ExpectedServiceReferences []string
		ExpectedErr               bool
	}{
		{
			Name:        "Nil",
			ExpectedErr: true,
		},
		{
			Name:                "Empty tags",
			InputTags:           map[string]*string{},
			InputIsUserAssigned: true,
			ExpectedTags:        map[string]*string{},
		},
		{
			Name: "Single service",
			InputTags: map[string]*string{
				consts.ServiceTagKey: ptr.To("ns1/svc1"),
			},
			ExpectedTags: map[string]*string{
				consts.ServiceTagKey: ptr.To("ns1/svc1"),
			},
			ExpectedServiceReferences: []string{"ns1/svc1"},
		},
		{
			Name: "Multiple services #1",
			InputTags: map[string]*string{
				consts.ServiceTagKey: ptr.To("ns1/svc1,ns2/svc2"),
			},
			ExpectedTags: map[string]*string{
				consts.ServiceTagKey: ptr.To("ns1/svc1"),
			},
			ExpectedServiceReferences: []string{"ns1/svc1"},
		},
		{
			Name: "Multiple services #2",
			InputTags: map[string]*string{
				consts.ServiceTagKey: ptr.To("ns1/svc1,ns2/svc2,ns3/svc3"),
			},
			ExpectedTags: map[string]*string{
				consts.ServiceTagKey: ptr.To("ns1/svc1,ns3/svc3"),
			},
			ExpectedServiceReferences: []string{"ns1/svc1", "ns3/svc3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			svcName := "ns2/svc2"
			svc := getTestService(svcName, v1.ProtocolTCP, nil, false, 80)
			setServiceLoadBalancerIP(&svc, "1.2.3.4")

			pip := &armnetwork.PublicIPAddress{
				Tags: tt.InputTags,
			}
			serviceReferences, err := unbindServiceFromPIP(pip, svcName, tt.InputIsUserAssigned)
			if tt.ExpectedErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.ExpectedServiceReferences, serviceReferences)
				assert.Equal(t, tt.ExpectedTags, pip.Tags)
			}
		})
	}
}

func TestIsFrontendIPConfigIsUnsafeToDelete(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	service := getTestService("service1", v1.ProtocolTCP, nil, false, 80)
	az := GetTestCloud(ctrl)
	fipID := ptr.To("fip")

	testCases := []struct {
		desc       string
		existingLB *armnetwork.LoadBalancer
		unsafe     bool
	}{
		{
			desc: "isFrontendIPConfigUnsafeToDelete should return true if there is a " +
				"loadBalancing rule from other service referencing the frontend IP config",
			existingLB: &armnetwork.LoadBalancer{
				Name: ptr.To("lb"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					LoadBalancingRules: []*armnetwork.LoadBalancingRule{
						{
							Name: ptr.To("aservice2-rule"),
							Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip")},
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
			existingLB: &armnetwork.LoadBalancer{
				Name: ptr.To("lb"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					OutboundRules: []*armnetwork.OutboundRule{
						{
							Name: ptr.To("aservice1-rule"),
							Properties: &armnetwork.OutboundRulePropertiesFormat{
								FrontendIPConfigurations: []*armnetwork.SubResource{
									{ID: ptr.To("fip")},
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
			existingLB: &armnetwork.LoadBalancer{
				Name: ptr.To("lb"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					LoadBalancingRules: []*armnetwork.LoadBalancingRule{
						{
							Name: ptr.To("aservice1-rule"),
							Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip")},
							},
						},
					},
				},
			},
		},
		{
			desc: "isFrontendIPConfigUnsafeToDelete should return true if there is a " +
				"inbound NAT rule referencing the frontend IP config",
			existingLB: &armnetwork.LoadBalancer{
				Name: ptr.To("lb"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					InboundNatRules: []*armnetwork.InboundNatRule{
						{
							Name: ptr.To("aservice2-rule"),
							Properties: &armnetwork.InboundNatRulePropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip")},
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
			existingLB: &armnetwork.LoadBalancer{
				Name: ptr.To("lb"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					InboundNatPools: []*armnetwork.InboundNatPool{
						{
							Name: ptr.To("aservice2-rule"),
							Properties: &armnetwork.InboundNatPoolPropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip")},
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
		existingLB  *armnetwork.LoadBalancer
		expectedErr bool
	}{
		{
			desc: "checkLoadBalancerResourcesConflicts should report the conflict error if " +
				"there is a conflicted loadBalancing rule - IPv4",
			fipID: "fip",
			existingLB: &armnetwork.LoadBalancer{
				Name: ptr.To("lb"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					LoadBalancingRules: []*armnetwork.LoadBalancingRule{
						{
							Name: ptr.To("aservice2-rule"),
							Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip")},
								FrontendPort:            ptr.To(int32(80)),
								Protocol:                to.Ptr(armnetwork.TransportProtocolTCP),
							},
						},
						{
							Name: ptr.To("aservice2-rule-IPv6"),
							Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip-IPv6")},
								FrontendPort:            ptr.To(int32(80)),
								Protocol:                to.Ptr(armnetwork.TransportProtocolTCP),
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
			existingLB: &armnetwork.LoadBalancer{
				Name: ptr.To("lb"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					LoadBalancingRules: []*armnetwork.LoadBalancingRule{
						{
							Name: ptr.To("aservice2-rule"),
							Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip")},
								FrontendPort:            ptr.To(int32(80)),
								Protocol:                to.Ptr(armnetwork.TransportProtocolTCP),
							},
						},
						{
							Name: ptr.To("aservice2-rule-IPv6"),
							Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip-IPv6")},
								FrontendPort:            ptr.To(int32(80)),
								Protocol:                to.Ptr(armnetwork.TransportProtocolTCP),
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
			existingLB: &armnetwork.LoadBalancer{
				Name: ptr.To("lb"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					InboundNatRules: []*armnetwork.InboundNatRule{
						{
							Name: ptr.To("aservice1-rule"),
							Properties: &armnetwork.InboundNatRulePropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip")},
								FrontendPort:            ptr.To(int32(80)),
								Protocol:                to.Ptr(armnetwork.TransportProtocolTCP),
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
			existingLB: &armnetwork.LoadBalancer{
				Name: ptr.To("lb"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					InboundNatPools: []*armnetwork.InboundNatPool{
						{
							Name: ptr.To("aservice1-rule"),
							Properties: &armnetwork.InboundNatPoolPropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip")},
								FrontendPortRangeStart:  ptr.To(int32(80)),
								FrontendPortRangeEnd:    ptr.To(int32(90)),
								Protocol:                to.Ptr(armnetwork.TransportProtocolTCP),
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
			existingLB: &armnetwork.LoadBalancer{
				Name: ptr.To("lb"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					LoadBalancingRules: []*armnetwork.LoadBalancingRule{
						{
							Name: ptr.To("aservice2-rule"),
							Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip")},
								FrontendPort:            ptr.To(int32(90)),
								Protocol:                to.Ptr(armnetwork.TransportProtocolTCP),
							},
						},
					},
					InboundNatRules: []*armnetwork.InboundNatRule{
						{
							Name: ptr.To("aservice1-rule"),
							Properties: &armnetwork.InboundNatRulePropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip")},
								FrontendPort:            ptr.To(int32(90)),
								Protocol:                to.Ptr(armnetwork.TransportProtocolTCP),
							},
						},
					},
					InboundNatPools: []*armnetwork.InboundNatPool{
						{
							Name: ptr.To("aservice1-rule"),
							Properties: &armnetwork.InboundNatPoolPropertiesFormat{
								FrontendIPConfiguration: &armnetwork.SubResource{ID: ptr.To("fip")},
								FrontendPortRangeStart:  ptr.To(int32(800)),
								FrontendPortRangeEnd:    ptr.To(int32(900)),
								Protocol:                to.Ptr(armnetwork.TransportProtocolTCP),
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

func buildLBWithVMIPs(clusterName string, vmIPs []string) *armnetwork.LoadBalancer {
	lb := armnetwork.LoadBalancer{
		Name: ptr.To(clusterName),
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			BackendAddressPools: []*armnetwork.BackendAddressPool{
				{
					Name: ptr.To(clusterName),
					Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
						LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{},
					},
				},
			},
		},
	}

	for _, vmIP := range vmIPs {
		vmIP := vmIP
		(lb.Properties.BackendAddressPools)[0].Properties.LoadBalancerBackendAddresses = append(lb.Properties.BackendAddressPools[0].Properties.LoadBalancerBackendAddresses, &armnetwork.LoadBalancerBackendAddress{
			Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
				IPAddress: &vmIP,
				VirtualNetwork: &armnetwork.SubResource{
					ID: ptr.To("vnet"),
				},
			},
		})
	}

	return &lb
}

func buildDefaultTestLB(name string, backendIPConfigs []string) armnetwork.LoadBalancer {
	expectedLB := armnetwork.LoadBalancer{
		Name: ptr.To(name),
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			BackendAddressPools: []*armnetwork.BackendAddressPool{
				{
					Name: ptr.To(name),
					Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
						BackendIPConfigurations: []*armnetwork.InterfaceIPConfiguration{},
					},
				},
			},
		},
	}
	backendIPConfigurations := make([]*armnetwork.InterfaceIPConfiguration, 0)
	for _, ipConfig := range backendIPConfigs {
		backendIPConfigurations = append(backendIPConfigurations, &armnetwork.InterfaceIPConfiguration{ID: ptr.To(ipConfig)})
	}
	(expectedLB.Properties.BackendAddressPools)[0].Properties.BackendIPConfigurations = backendIPConfigurations
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
	pip := armnetwork.PublicIPAddress{
		Tags: map[string]*string{
			consts.ClusterNameKey:     ptr.To("testCluster"),
			consts.ServiceTagKey:      ptr.To("default/svc1,default/svc2"),
			consts.ServiceUsingDNSKey: ptr.To("default/svc1"),
			"foo":                     ptr.To("bar"),
			"a":                       ptr.To("j"),
			"m":                       ptr.To("n"),
		},
	}

	t.Run("ensurePIPTagged should ensure the pip is tagged as configured", func(t *testing.T) {
		expectedPIP := armnetwork.PublicIPAddress{
			Tags: map[string]*string{
				consts.ClusterNameKey:     ptr.To("testCluster"),
				consts.ServiceTagKey:      ptr.To("default/svc1,default/svc2"),
				consts.ServiceUsingDNSKey: ptr.To("default/svc1"),
				"foo":                     ptr.To("bar"),
				"a":                       ptr.To("b"),
				"c":                       ptr.To("d"),
				"y":                       ptr.To("z"),
				"m":                       ptr.To("n"),
				"e":                       ptr.To(""),
			},
		}
		changed := cloud.ensurePIPTagged(&service, &pip)
		assert.True(t, changed)
		assert.Equal(t, expectedPIP, pip)
	})

	t.Run("ensurePIPTagged should delete the old tags if the SystemTags is set", func(t *testing.T) {
		cloud.SystemTags = "a,foo"
		expectedPIP := armnetwork.PublicIPAddress{
			Tags: map[string]*string{
				consts.ClusterNameKey:     ptr.To("testCluster"),
				consts.ServiceTagKey:      ptr.To("default/svc1,default/svc2"),
				consts.ServiceUsingDNSKey: ptr.To("default/svc1"),
				"foo":                     ptr.To("bar"),
				"a":                       ptr.To("b"),
				"c":                       ptr.To("d"),
				"y":                       ptr.To("z"),
				"e":                       ptr.To(""),
			},
		}
		changed := cloud.ensurePIPTagged(&service, &pip)
		assert.True(t, changed)
		assert.Equal(t, expectedPIP, pip)
	})

	t.Run("ensurePIPTagged should support TagsMap", func(t *testing.T) {
		cloud.SystemTags = "a,foo"
		cloud.TagsMap = map[string]string{"a": "c", "a=b": "c=d", "Y": "zz"}
		expectedPIP := armnetwork.PublicIPAddress{
			Tags: map[string]*string{
				consts.ClusterNameKey:     ptr.To("testCluster"),
				consts.ServiceTagKey:      ptr.To("default/svc1,default/svc2"),
				consts.ServiceUsingDNSKey: ptr.To("default/svc1"),
				"foo":                     ptr.To("bar"),
				"a":                       ptr.To("b"),
				"c":                       ptr.To("d"),
				"a=b":                     ptr.To("c=d"),
				"e":                       ptr.To(""),
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
			existedTags:     map[string]*string{"a": ptr.To("b")},
			newTags:         "c=d",
			expectedTags:    map[string]*string{"a": ptr.To("b"), "c": ptr.To("d")},
			expectedChanged: true,
		},
		{
			description:     "ensureLoadBalancerTagged should delete the old tags if SystemTags is specified",
			existedTags:     map[string]*string{"a": ptr.To("b"), "c": ptr.To("d"), "h": ptr.To("i")},
			newTags:         "c=e,f=g",
			systemTags:      "a,x,y,z",
			expectedTags:    map[string]*string{"a": ptr.To("b"), "c": ptr.To("e"), "f": ptr.To("g")},
			expectedChanged: true,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.Tags = tc.newTags
			cloud.SystemTags = tc.systemTags
			lb := &armnetwork.LoadBalancer{Tags: tc.existedTags}

			changed := cloud.ensureLoadBalancerTagged(lb)
			assert.Equal(t, tc.expectedChanged, changed)
			assert.Equal(t, tc.expectedTags, lb.Tags)
		})
	}
}

func TestRemoveFrontendIPConfigurationFromLoadBalancerDelete(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	t.Run("removeFrontendIPConfigurationFromLoadBalancer should remove the unwanted frontend IP configuration and delete the orphaned LB", func(t *testing.T) {
		fip := &armnetwork.FrontendIPConfiguration{
			Name: ptr.To("testCluster"),
			ID:   ptr.To("testCluster-fip"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{
					ID: ptr.To("pipID"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
				PrivateIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
			},
		}
		service := getTestService("svc1", v1.ProtocolTCP, nil, false, 80)
		lb := getTestLoadBalancer(ptr.To("lb"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("testCluster"), service, "standard")
		bid := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-0/ipConfigurations/ipconfig1"
		lb.Properties.BackendAddressPools = []*armnetwork.BackendAddressPool{
			{
				Name: ptr.To("testCluster"),
				Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
					BackendIPConfigurations: []*armnetwork.InterfaceIPConfiguration{
						{ID: ptr.To(bid)},
					},
				},
			},
		}
		cloud := GetTestCloud(ctrl)
		mockLBClient := cloud.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
		mockLBClient.EXPECT().Delete(gomock.Any(), "rg", "lb").Return(nil)
		mockPLSRepo := cloud.plsRepo.(*privatelinkservice.MockRepository)
		mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil)
		existingLBs := []*armnetwork.LoadBalancer{{Name: ptr.To("lb")}}
		_, _, err := cloud.removeFrontendIPConfigurationFromLoadBalancer(context.TODO(), lb, existingLBs, []*armnetwork.FrontendIPConfiguration{fip}, "testCluster", &service)
		assert.NoError(t, err)
	})
}

func TestRemoveFrontendIPConfigurationFromLoadBalancerUpdate(t *testing.T) {

	t.Run("removeFrontendIPConfigurationFromLoadBalancer should remove the unwanted frontend IP configuration and update the LB if there are remaining frontend IP configurations", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cloud := GetTestCloud(ctrl)
		fip := &armnetwork.FrontendIPConfiguration{
			Name: ptr.To("testCluster"),
			ID:   ptr.To("testCluster-fip"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PublicIPAddress: &armnetwork.PublicIPAddress{
					ID: ptr.To("pipID"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
				PrivateIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
			},
		}
		service := getTestService("svc1", v1.ProtocolTCP, nil, false, 80)
		lb := getTestLoadBalancer(ptr.To("lb"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("testCluster"), service, "standard")
		lb.Properties.FrontendIPConfigurations = append(lb.Properties.FrontendIPConfigurations, &armnetwork.FrontendIPConfiguration{Name: ptr.To("fip1")})
		mockLBClient := cloud.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
		mockLBClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", "lb", gomock.Any()).Return(nil, nil)
		mockPLSRepo := cloud.plsRepo.(*privatelinkservice.MockRepository)
		mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil)
		_, _, err := cloud.removeFrontendIPConfigurationFromLoadBalancer(context.TODO(), lb, []*armnetwork.LoadBalancer{}, []*armnetwork.FrontendIPConfiguration{fip}, "testCluster", &service)
		assert.NoError(t, err)
	})
}

func TestCleanOrphanedLoadBalancerLBInUseByVMSS(t *testing.T) {

	t.Run("cleanOrphanedLoadBalancer should retry deleting lb when meeting LoadBalancerInUseByVirtualMachineScaleSet", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cloud := GetTestCloud(ctrl)
		vmss, err := newScaleSet(cloud)
		assert.NoError(t, err)
		cloud.VMSet = vmss
		cloud.LoadBalancerSKU = consts.LoadBalancerSKUStandard

		mockLBClient := cloud.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
		mockLBClient.EXPECT().Delete(gomock.Any(), "rg", "test").Return(&azcore.ResponseError{ErrorCode: LBInUseRawError})
		mockLBClient.EXPECT().Delete(gomock.Any(), "rg", "test").Return(nil)

		expectedVMSS := buildTestVMSSWithLB(testVMSSName, "vmss-vm-", []string{testLBBackendpoolID0}, false)
		mockVMSSClient := cloud.ComputeClientFactory.GetVirtualMachineScaleSetClient().(*mock_virtualmachinescalesetclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), "rg").Return([]*armcompute.VirtualMachineScaleSet{expectedVMSS}, nil).MaxTimes(2)
		mockVMSSClient.EXPECT().Get(gomock.Any(), "rg", testVMSSName, gomock.Any()).Return(expectedVMSS, nil)
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", testVMSSName, gomock.Any()).Return(nil, nil)

		service := getTestService("test", v1.ProtocolTCP, nil, false, 80)
		lb := getTestLoadBalancer(ptr.To("test"), ptr.To("rg"), ptr.To("test"), ptr.To("test"), service, consts.LoadBalancerSKUStandard)
		(lb.Properties.BackendAddressPools)[0].ID = ptr.To(testLBBackendpoolID0)

		existingLBs := []*armnetwork.LoadBalancer{{Name: ptr.To("test")}}

		err = cloud.cleanOrphanedLoadBalancer(context.TODO(), lb, existingLBs, &service, "test")
		assert.NoError(t, err)
	})

	t.Run("cleanupOrphanedLoadBalancer should not call delete api if the lb does not exist", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cloud := GetTestCloud(ctrl)
		vmss, err := newScaleSet(cloud)
		assert.NoError(t, err)
		cloud.VMSet = vmss
		cloud.LoadBalancerSKU = consts.LoadBalancerSKUStandard

		service := getTestService("test", v1.ProtocolTCP, nil, false, 80)
		lb := getTestLoadBalancer(ptr.To("test"), ptr.To("rg"), ptr.To("test"), ptr.To("test"), service, consts.LoadBalancerSKUStandard)
		(lb.Properties.BackendAddressPools)[0].ID = ptr.To(testLBBackendpoolID0)

		existingLBs := []*armnetwork.LoadBalancer{}

		err = cloud.cleanOrphanedLoadBalancer(context.TODO(), lb, existingLBs, &service, "test")
		assert.NoError(t, err)
	})
}

func TestReconcileZonesForFrontendIPConfigs(t *testing.T) {

	for _, tc := range []struct {
		description               string
		service                   v1.Service
		existingFrontendIPConfigs []*armnetwork.FrontendIPConfiguration
		existingPIPV4             *armnetwork.PublicIPAddress
		existingPIPV6             *armnetwork.PublicIPAddress
		status                    *v1.LoadBalancerStatus
		getZoneError              error
		regionZonesMap            map[string][]string
		expectedZones             []*string
		expectedDirty             bool
		expectedIPv4              *string
		expectedIPv6              *string
		expectedErr               error
	}{
		{
			description:               "reconcileFrontendIPConfigs should reconcile the zones for the new fip config",
			service:                   getTestServiceDualStack("test", v1.ProtocolTCP, nil, 80),
			existingFrontendIPConfigs: []*armnetwork.FrontendIPConfiguration{},
			existingPIPV4:             &armnetwork.PublicIPAddress{Name: ptr.To("testCluster-atest"), Location: ptr.To("eastus")},
			existingPIPV6:             &armnetwork.PublicIPAddress{Name: ptr.To("testCluster-atest-IPv6"), Location: ptr.To("eastus")},
			regionZonesMap:            map[string][]string{"westus": {"1", "2", "3"}, "eastus": {"1", "2"}},
			expectedDirty:             true,
		},
		{
			description:               "reconcileFrontendIPConfigs should reconcile the zones for the new internal fip config",
			service:                   getInternalTestServiceDualStack("test", 80),
			existingFrontendIPConfigs: []*armnetwork.FrontendIPConfiguration{},
			existingPIPV4:             &armnetwork.PublicIPAddress{Name: ptr.To("testCluster-atest"), Location: ptr.To("eastus")},
			existingPIPV6:             &armnetwork.PublicIPAddress{Name: ptr.To("testCluster-atest-IPv6"), Location: ptr.To("eastus")},
			regionZonesMap:            map[string][]string{"westus": {"1", "2", "3"}, "eastus": {"1", "2"}},
			expectedZones:             to.SliceOfPtrs("1", "2", "3"),
			expectedDirty:             true,
		},
		{
			description:  "reconcileFrontendIPConfigs should report an error if failed to get zones",
			service:      getInternalTestServiceDualStack("test", 80),
			getZoneError: errors.New("get zone failed"),
			expectedErr:  errors.New("get zone failed"),
		},
		{
			description: "reconcileFrontendIPConfigs should use the nil zones of the existing frontend",
			service: getTestServiceWithAnnotation("test", map[string]string{
				consts.ServiceAnnotationLoadBalancerInternalSubnet: "subnet",
				consts.ServiceAnnotationLoadBalancerInternal:       consts.TrueAnnotationValue,
			}, true, 80),
			existingFrontendIPConfigs: []*armnetwork.FrontendIPConfiguration{
				{
					Name: ptr.To("atest1"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						Subnet: &armnetwork.Subnet{
							Name: ptr.To("subnet-1"),
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
				consts.ServiceAnnotationLoadBalancerInternal:       consts.TrueAnnotationValue,
			}, true, 80),
			existingFrontendIPConfigs: []*armnetwork.FrontendIPConfiguration{
				{
					Name: ptr.To("not-this-one"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						Subnet: &armnetwork.Subnet{
							Name: ptr.To("subnet-1"),
						},
					},
					Zones: to.SliceOfPtrs("2"),
				},
				{
					Name: ptr.To("atest1"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						Subnet: &armnetwork.Subnet{
							Name: ptr.To("subnet-1"),
						},
					},
					Zones: to.SliceOfPtrs("1"),
				},
			},
			expectedZones: to.SliceOfPtrs("1"),
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
			expectedIPv4:  ptr.To("1.2.3.4"),
			expectedIPv6:  ptr.To("2001::1"),
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
			expectedIPv4:  ptr.To(""),
			expectedIPv6:  ptr.To(""),
			expectedDirty: true,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			cloud := GetTestCloud(ctrl)
			cloud.regionZonesMap = tc.regionZonesMap
			cloud.LoadBalancerSKU = string(armnetwork.LoadBalancerSKUNameStandard)

			lb := getTestLoadBalancer(ptr.To("lb"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("testCluster"), tc.service, "standard")
			existingFrontendIPConfigs := tc.existingFrontendIPConfigs
			lb.Properties.FrontendIPConfigurations = existingFrontendIPConfigs

			mockPIPClient := cloud.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
			firstV4 := mockPIPClient.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.PublicIPAddress{}, nil).MaxTimes(2)
			firstV6 := mockPIPClient.EXPECT().List(gomock.Any(), "rg").Return([]*armnetwork.PublicIPAddress{}, nil).MaxTimes(2)
			mockPIPClient.EXPECT().Get(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(tc.existingPIPV4, nil).MaxTimes(1).After(firstV4)
			mockPIPClient.EXPECT().Get(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(tc.existingPIPV6, nil).MaxTimes(1).After(firstV6)
			mockPIPClient.EXPECT().CreateOrUpdate(gomock.Any(), "rg", gomock.Any(), gomock.Any()).Return(nil, nil).MaxTimes(2)

			subnetClient := cloud.subnetRepo.(*subnet.MockRepository)
			subnetClient.EXPECT().Get(gomock.Any(), "rg", "vnet", "subnet").Return(
				&armnetwork.Subnet{ID: ptr.To("subnet0"), Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefixes: to.SliceOfPtrs("1.2.3.4/31", "2001::1/127")}}, nil).MaxTimes(1)

			zoneMock := cloud.zoneRepo.(*zone.MockRepository)
			zoneMock.EXPECT().ListZones(gomock.Any()).Return(map[string][]string{}, tc.getZoneError).MaxTimes(2)

			service := tc.service
			isDualStack := isServiceDualStack(&service)
			defaultLBFrontendIPConfigName := cloud.getDefaultFrontendIPConfigName(&service)
			lbFrontendIPConfigNames := map[bool]string{
				consts.IPVersionIPv4: getResourceByIPFamily(defaultLBFrontendIPConfigName, isDualStack, consts.IPVersionIPv4),
				consts.IPVersionIPv6: getResourceByIPFamily(defaultLBFrontendIPConfigName, isDualStack, consts.IPVersionIPv6),
			}
			_, _, dirty, err := cloud.reconcileFrontendIPConfigs(context.TODO(), "testCluster", &service, lb, tc.status, true, lbFrontendIPConfigNames)
			if tc.expectedErr == nil {
				assert.NoError(t, err)
			} else {
				assert.Contains(t, err.Error(), tc.expectedErr.Error())
			}
			assert.Equal(t, tc.expectedDirty, dirty)

			for _, fip := range lb.Properties.FrontendIPConfigurations {
				if strings.EqualFold(ptr.Deref(fip.Name, ""), defaultLBFrontendIPConfigName) {
					assert.Equal(t, tc.expectedZones, fip.Zones)
				}
			}

			checkExpectedIP := func(isIPv6 bool, expectedIP *string) {
				if expectedIP != nil {
					for _, fip := range lb.Properties.FrontendIPConfigurations {
						if strings.EqualFold(ptr.Deref(fip.Name, ""), lbFrontendIPConfigNames[isIPv6]) {
							assert.Equal(t, *expectedIP, ptr.Deref(fip.Properties.PrivateIPAddress, ""))
							if *expectedIP != "" {
								assert.Equal(t, to.Ptr(armnetwork.IPAllocationMethodStatic), (lb.Properties.FrontendIPConfigurations)[0].Properties.PrivateIPAllocationMethod)
							} else {
								assert.Equal(t, to.Ptr(armnetwork.IPAllocationMethodDynamic), (lb.Properties.FrontendIPConfigurations)[0].Properties.PrivateIPAllocationMethod)
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
		existingFIPs  []*armnetwork.FrontendIPConfiguration
		existingPIPs  []*armnetwork.PublicIPAddress
		status        *v1.LoadBalancerStatus
		wantLB        bool
		expectedDirty bool
		expectedFIPs  []*armnetwork.FrontendIPConfiguration
		expectedErr   error
	}{
		{
			desc:    "DualStack Service reconciles existing FIPs and does not touch others, not dirty",
			service: getTestServiceDualStack("test", v1.ProtocolTCP, nil, 80),
			existingFIPs: []*armnetwork.FrontendIPConfiguration{
				{
					Name: ptr.To("fipV4"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							Name: ptr.To("pipV4"),
							Properties: &armnetwork.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
								IPAddress:              ptr.To("1.2.3.4"),
							},
						},
					},
				},
				{
					Name: ptr.To("fipV6"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							Name: ptr.To("pipV6"),
							Properties: &armnetwork.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv6),
								IPAddress:              ptr.To("fe::1"),
							},
						},
					},
				},
				{
					Name: ptr.To("atest"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: ptr.To("testCluster-atest-id"),
						},
					},
				},
				{
					Name: ptr.To("atest-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest-IPv6"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: ptr.To("testCluster-atest-id-IPv6"),
						},
					},
				},
			},
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("testCluster-atest"),
					ID:   ptr.To("testCluster-atest-id"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						IPAddress:                ptr.To("1.2.3.5"),
					},
				},
				{
					Name: ptr.To("testCluster-atest-IPv6"),
					ID:   ptr.To("testCluster-atest-id-IPv6"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						IPAddress:                ptr.To("fe::2"),
					},
				},
				{
					Name: ptr.To("pipV4"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
				},
				{
					Name: ptr.To("pipV6"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv6),
						IPAddress:              ptr.To("fe::1"),
					},
				},
			},
			status:        nil,
			wantLB:        true,
			expectedDirty: false,
			expectedFIPs: []*armnetwork.FrontendIPConfiguration{
				{
					Name: ptr.To("fipV4"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							Name: ptr.To("pipV4"),
							Properties: &armnetwork.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
								IPAddress:              ptr.To("1.2.3.4"),
							},
						},
					},
				},
				{
					Name: ptr.To("fipV6"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							Name: ptr.To("pipV6"),
							Properties: &armnetwork.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv6),
								IPAddress:              ptr.To("fe::1"),
							},
						},
					},
				},
				{
					Name: ptr.To("atest"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: ptr.To("testCluster-atest-id"),
						},
					},
				},
				{
					Name: ptr.To("atest-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest-IPv6"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: ptr.To("testCluster-atest-id-IPv6"),
						},
					},
				},
			},
		},
		{
			desc:    "DualStack Service reconciles existing FIPs, wantLB == false, but an FIP ID is empty, should return error",
			service: getTestServiceDualStack("test", v1.ProtocolTCP, nil, 80),
			existingFIPs: []*armnetwork.FrontendIPConfiguration{
				{
					Name: ptr.To("atest"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: ptr.To("testCluster-atest-id"),
						},
					},
				},
				{
					Name: ptr.To("atest-IPv6"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest-IPv6"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: ptr.To("testCluster-atest-id-IPv6"),
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
			existingFIPs: []*armnetwork.FrontendIPConfiguration{
				{
					Name: ptr.To("fipV4"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							Name: ptr.To("pipV4"),
							Properties: &armnetwork.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
								IPAddress:              ptr.To("1.2.3.4"),
							},
						},
					},
				},
			},
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("testCluster-atest"),
					ID:   ptr.To("testCluster-atest-id"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
						PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
						IPAddress:                ptr.To("fe::1"),
					},
					SKU: &armnetwork.PublicIPAddressSKU{
						Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
					},
				},
				{
					Name: ptr.To("pipV4"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("1.2.3.4"),
					},
					SKU: &armnetwork.PublicIPAddressSKU{
						Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
					},
				},
			},
			status:        nil,
			wantLB:        true,
			expectedDirty: true,
			expectedFIPs: []*armnetwork.FrontendIPConfiguration{
				{
					Name: ptr.To("fipV4"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							Name: ptr.To("pipV4"),
							Properties: &armnetwork.PublicIPAddressPropertiesFormat{
								PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
								IPAddress:              ptr.To("1.2.3.4"),
							},
						},
					},
				},
				{
					Name: ptr.To("atest"),
					ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/frontendIPConfigurations/atest"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: ptr.To("testCluster-atest-id"),
						},
					},
				},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.LoadBalancerSKU = string(armnetwork.LoadBalancerSKUNameStandard)

			lb := getTestLoadBalancer(ptr.To("lb"), ptr.To("rg"), ptr.To("testCluster"), ptr.To("testCluster"), tc.service, "standard")
			existingFIPs := tc.existingFIPs
			lb.Properties.FrontendIPConfigurations = existingFIPs

			mockPIPClient := cloud.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
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
			_, _, dirty, err := cloud.reconcileFrontendIPConfigs(context.TODO(), "testCluster", &service, lb, tc.status, tc.wantLB, lbFrontendIPConfigNames)
			if tc.expectedErr != nil {
				assert.Equal(t, tc.expectedErr, err)
			} else {
				assert.Nil(t, err)
				assert.Equal(t, tc.expectedDirty, dirty)
				assert.Equal(t, tc.expectedFIPs, lb.Properties.FrontendIPConfigurations)
			}
		})
	}
}

func TestReconcileIPSettings(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testcases := []struct {
		desc                     string
		SKU                      string
		pip                      *armnetwork.PublicIPAddress
		service                  v1.Service
		isIPv6                   bool
		expectedChanged          bool
		expectedIPVersion        armnetwork.IPVersion
		expectedAllocationMethod armnetwork.IPAllocationMethod
	}{
		{
			desc: "correct IPv4 PIP",
			SKU:  consts.LoadBalancerSKUStandard,
			pip: &armnetwork.PublicIPAddress{
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
				},
			},
			service:                  getTestService("test", v1.ProtocolTCP, nil, false, 80),
			isIPv6:                   false,
			expectedChanged:          false,
			expectedIPVersion:        armnetwork.IPVersionIPv4,
			expectedAllocationMethod: armnetwork.IPAllocationMethodStatic,
		},
		{
			desc: "IPv4 PIP but IP version is IPv6",
			SKU:  consts.LoadBalancerSKUStandard,
			pip: &armnetwork.PublicIPAddress{
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
				},
			},
			service:                  getTestService("test", v1.ProtocolTCP, nil, false, 80),
			isIPv6:                   false,
			expectedChanged:          true,
			expectedIPVersion:        armnetwork.IPVersionIPv4,
			expectedAllocationMethod: armnetwork.IPAllocationMethodStatic,
		},
		{
			desc: "IPv6 PIP but allocation method is dynamic with standard SKU",
			SKU:  consts.LoadBalancerSKUStandard,
			pip: &armnetwork.PublicIPAddress{
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
				},
			},
			service:                  getTestService("test", v1.ProtocolTCP, nil, true, 80),
			isIPv6:                   true,
			expectedChanged:          true,
			expectedIPVersion:        armnetwork.IPVersionIPv6,
			expectedAllocationMethod: armnetwork.IPAllocationMethodStatic,
		},
		{
			desc: "IPv6 PIP but allocation method is static with basic SKU",
			SKU:  consts.LoadBalancerSKUBasic,
			pip: &armnetwork.PublicIPAddress{
				Properties: &armnetwork.PublicIPAddressPropertiesFormat{
					PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
					PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
				},
			},
			service:                  getTestService("test", v1.ProtocolTCP, nil, true, 80),
			isIPv6:                   true,
			expectedChanged:          true,
			expectedIPVersion:        armnetwork.IPVersionIPv6,
			expectedAllocationMethod: armnetwork.IPAllocationMethodDynamic,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			az.LoadBalancerSKU = tc.SKU
			pip := tc.pip
			pip.Name = ptr.To("pip")
			service := tc.service
			changed := az.reconcileIPSettings(pip, &service, tc.isIPv6)
			assert.Equal(t, tc.expectedChanged, changed)
			assert.NotNil(t, pip.Properties)
			assert.Equal(t, *pip.Properties.PublicIPAddressVersion, tc.expectedIPVersion)
			assert.Equal(t, *pip.Properties.PublicIPAllocationMethod, tc.expectedAllocationMethod)
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
			tags:     map[string]*string{consts.ServiceUsingDNSKey: ptr.To("test-service")},
			expected: "test-service",
		},
		{
			desc:     "Expected service should be returned when tags contain legacy dns label tag",
			tags:     map[string]*string{consts.LegacyServiceUsingDNSKey: ptr.To("test-service")},
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
			tags:     map[string]*string{consts.ServiceTagKey: ptr.To("test-service")},
			expected: "test-service",
		},
		{
			desc:     "Expected service should be returned when tags contain legacy service tag",
			tags:     map[string]*string{consts.LegacyServiceTagKey: ptr.To("test-service")},
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
			tags:     map[string]*string{consts.ClusterNameKey: ptr.To("test-cluster")},
			expected: "test-cluster",
		},
		{
			desc:     "Expected service should be returned when tags contain legacy cluster name tag",
			tags:     map[string]*string{consts.LegacyClusterNameKey: ptr.To("test-cluster")},
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
		desc                          string
		expectedDeleteCall            bool
		expectedDecoupleErr           error
		multiSLBConfigs               []config.MultipleStandardLoadBalancerConfiguration
		nodesWithCorrectVMSet         *utilsets.IgnoreCaseSet
		expectedMultiSLBConfigs       []config.MultipleStandardLoadBalancerConfiguration
		expectedNodesWithCorrectVMSet *utilsets.IgnoreCaseSet
		expectedErr                   error
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
			expectedErr:         fmt.Errorf("safeDeleteLoadBalancer: failed to EnsureBackendPoolDeleted: error"),
		},
		{
			desc:               "should cleanup active nodes when using multi-slb",
			expectedDeleteCall: true,
			multiSLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "test",
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString("node1", "node2"),
					},
				},
				{
					Name: "test2",
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString("node3"),
					},
				},
			},
			nodesWithCorrectVMSet: utilsets.NewString("node1", "node2", "node3"),
			expectedMultiSLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "test",
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString(),
					},
				},
				{
					Name: "test2",
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString("node3"),
					},
				},
			},
			expectedNodesWithCorrectVMSet: utilsets.NewString("node3"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			mockLBClient := cloud.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
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
				gomock.Any(),
			).Return(false, tc.expectedDecoupleErr)
			cloud.VMSet = mockVMSet

			if len(tc.multiSLBConfigs) > 0 {
				cloud.MultipleStandardLoadBalancerConfigurations = tc.multiSLBConfigs
				for _, nodeName := range tc.nodesWithCorrectVMSet.UnsortedList() {
					cloud.nodesWithCorrectLoadBalancerByPrimaryVMSet.Store(nodeName, sets.Empty{})
				}
			}
			svc := getTestService("svc", v1.ProtocolTCP, nil, false, 80)
			lb := armnetwork.LoadBalancer{
				Name: ptr.To("test"),
				Properties: &armnetwork.LoadBalancerPropertiesFormat{
					BackendAddressPools: []*armnetwork.BackendAddressPool{},
				},
			}
			err := cloud.safeDeleteLoadBalancer(context.TODO(), lb, "vmss", &svc)
			if tc.expectedErr != nil {
				assert.Contains(t, err.Error(), tc.expectedErr.Error())
			}
			if len(tc.multiSLBConfigs) > 0 {
				assert.Equal(t, tc.expectedMultiSLBConfigs, cloud.MultipleStandardLoadBalancerConfigurations)
				actualNodesWithCorrectLoadBalancerByPrimaryVMSet := utilsets.NewString()
				cloud.nodesWithCorrectLoadBalancerByPrimaryVMSet.Range(func(key, _ interface{}) bool {
					actualNodesWithCorrectLoadBalancerByPrimaryVMSet.Insert(key.(string))
					return true
				})
				assert.Equal(t, tc.expectedNodesWithCorrectVMSet, actualNodesWithCorrectLoadBalancerByPrimaryVMSet)
			}
		})
	}
}

func TestEqualSubResource(t *testing.T) {
	testcases := []struct {
		desc         string
		subResource1 *armnetwork.SubResource
		subResource2 *armnetwork.SubResource
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
			subResource1: &armnetwork.SubResource{},
			subResource2: nil,
			expected:     false,
		},
		{
			desc:         "equal",
			subResource1: &armnetwork.SubResource{ID: ptr.To("id")},
			subResource2: &armnetwork.SubResource{ID: ptr.To("id")},
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

func TestGetEligibleLoadBalancers(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description string
		lbConfigs   []config.MultipleStandardLoadBalancerConfiguration
		svc         v1.Service
		namespace   *v1.Namespace
		labels      map[string]string
		expectedLBs []string
		expectedErr error
	}{
		{
			description: "should respect service annotation",
			svc:         getTestService("test", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationLoadBalancerConfigurations: "A ,b"}, false),
			lbConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "a",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{},
				},
				{
					Name: "c",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{},
				},
			},
			expectedLBs: []string{"a"},
		},
		{
			description: "should report an error if a service selects a load balancer that is not defined in the configuration",
			svc:         getTestService("test", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationLoadBalancerConfigurations: "b"}, false),
			lbConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "a",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{},
				},
				{
					Name: "c",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{},
				},
			},
			expectedErr: errors.New(`service "test" selects 1 load balancers by annotation, but none of them is defined in cloud provider configuration`),
		},
		{
			description: "should respect namespace selector",
			svc:         getTestService("test", v1.ProtocolTCP, nil, false),
			lbConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "a",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceNamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k1": "v1"},
						},
					},
				},
				{
					Name: "b",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceNamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k2": "v2"},
						},
					},
				},
				{
					Name: "c",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{},
				},
			},
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ns1",
					Labels: map[string]string{"k1": "v1"},
				},
			},
			expectedLBs: []string{"a"},
		},
		{
			description: "should respect label selector",
			svc:         getTestService("test", v1.ProtocolTCP, nil, false),
			labels:      map[string]string{"k2": "v2", "k3": "v3"},
			lbConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "a",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceLabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k1": "v1"},
						},
					},
				},
				{
					Name: "b",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceLabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k2": "v2"},
						},
					},
				},
				{
					Name: "c",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{},
				},
			},
			expectedLBs: []string{"b"},
		},
		{
			description: "should put the service to the load balancers that does not have label/namespace selector if there is no other choice",
			svc:         getTestService("test", v1.ProtocolTCP, nil, false),
			labels:      map[string]string{"k2": "v2", "k3": "v3"},
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ns1",
					Labels: map[string]string{"k1": "v1"},
				},
			},
			lbConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "fails to match service label",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceLabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k1": "v1"},
						},
					},
				},
				{
					Name: "fails to match service namespace",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceNamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k2": "v2"},
						},
					},
				},
				{
					Name: "empty",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{},
				},
			},
			expectedLBs: []string{"empty"},
		},
		{
			description: "should return the intersection of annotation, namespace and label selector",
			svc:         getTestService("test", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationLoadBalancerConfigurations: "a,b"}, false),
			labels:      map[string]string{"k2": "v2"},
			lbConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "a",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceLabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k1": "v1"},
						},
					},
				},
				{
					Name: "b",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceLabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "k2",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"v1", "v2", "v3"},
								},
							},
						},
						ServiceNamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "k2",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"v1", "v2", "v3"},
								},
							},
						},
					},
				},
				{
					Name: "c",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceNamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k2": "v2"},
						},
					},
				},
			},
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ns1",
					Labels: map[string]string{"k1": "v1", "k2": "v2"},
				},
			},
			expectedLBs: []string{"b"},
		},
		{
			description: "should return an error if there is no matching lb config",
			svc:         getTestService("test", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationLoadBalancerConfigurations: "a,b,c"}, false),
			lbConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "a",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceLabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k1": "v1"},
						},
					},
				},
				{
					Name: "b",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceNamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "k2",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"v1", "v3"},
								},
							},
						},
					},
				},
				{
					Name: "c",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceNamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k2": "v3"},
						},
					},
				},
				{
					Name: "d",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						AllowServicePlacement: ptr.To(false),
						ServiceNamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k2": "v2"},
						},
					},
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveServices: utilsets.NewString("default/test"),
					},
				},
			},
			namespace: &v1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "ns1",
					Labels: map[string]string{"k1": "v1", "k2": "v2"},
				},
			},
			expectedLBs: []string{},
			expectedErr: errors.New(`service "ns1/test" selects 3 load balancers (a, b, c), but 0 of them () have AllowServicePlacement set to false and the service is not using any of them, 1 of them (a) do not match the service label selector, and 2 of them (c, b) do not match the service namespace selector`),
		},
		{
			description: "should report an error if failed to convert label selector as a selector",
			svc:         getTestService("test", v1.ProtocolTCP, nil, false),
			lbConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "a",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceLabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "k2",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{},
								},
							},
						},
					},
				},
			},
			expectedLBs: []string{},
			expectedErr: errors.New("values: Invalid value: []string(nil): for 'in', 'notin' operators, values set can't be empty"),
		},
		{
			description: "should report an error if failed to convert namespace selector as a selector",
			svc:         getTestService("test", v1.ProtocolTCP, nil, false),
			lbConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "a",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceNamespaceSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "k2",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{},
								},
							},
						},
					},
				},
			},
			expectedLBs: []string{},
			expectedErr: errors.New("values: Invalid value: []string(nil): for 'in', 'notin' operators, values set can't be empty"),
		},
		{
			description: "should respect allowServicePlacement flag",
			svc:         getTestService("test", v1.ProtocolTCP, map[string]string{consts.ServiceAnnotationLoadBalancerConfigurations: "a,c"}, false),
			lbConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "a",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						AllowServicePlacement: ptr.To(false),
					},
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveServices: utilsets.NewString("default/test"),
					},
				},
				{
					Name: "b",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{},
				},
				{
					Name: "c",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{},
				},
				{
					Name: "d",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{},
				},
			},
			expectedLBs: []string{"a", "c"},
		},
	} {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			az.MultipleStandardLoadBalancerConfigurations = tc.lbConfigs
			if tc.namespace != nil {
				az.KubeClient = fake.NewSimpleClientset(tc.namespace)
			}
			if tc.namespace != nil {
				tc.svc.Namespace = tc.namespace.Name
			}
			if tc.labels != nil {
				tc.svc.Labels = tc.labels
			}

			lbs, err := az.getEligibleLoadBalancersForService(context.TODO(), &tc.svc)
			assert.Equal(t, tc.expectedLBs, lbs)
			if tc.expectedErr != nil {
				assert.Equal(t, tc.expectedErr.Error(), err.Error())
			}
		})
	}
}

func TestGetAzureLoadBalancerName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	az.PrimaryAvailabilitySetName = primary

	cases := []struct {
		description       string
		vmSet             string
		isInternal        bool
		useStandardLB     bool
		clusterName       string
		lbName            string
		multiSLBConfigs   []config.MultipleStandardLoadBalancerConfiguration
		serviceAnnotation map[string]string
		serviceLabel      map[string]string
		expected          string
		expectedErr       error
	}{
		{
			description: "prefix of loadBalancerName should be az.LoadBalancerName if az.LoadBalancerName is not nil",
			vmSet:       primary,
			clusterName: "azure",
			lbName:      "azurelb",
			expected:    "azurelb",
		},
		{
			description: "default external LB should get primary vmset",
			vmSet:       primary,
			clusterName: "azure",
			expected:    "azure",
		},
		{
			description: "default internal LB should get primary vmset",
			vmSet:       primary,
			clusterName: "azure",
			isInternal:  true,
			expected:    "azure-internal",
		},
		{
			description: "non-default external LB should get its own vmset",
			vmSet:       "as",
			clusterName: "azure",
			expected:    "as",
		},
		{
			description: "non-default internal LB should get its own vmset",
			vmSet:       "as",
			clusterName: "azure",
			isInternal:  true,
			expected:    "as-internal",
		},
		{
			description:   "default standard external LB should get cluster name",
			vmSet:         primary,
			useStandardLB: true,
			clusterName:   "azure",
			expected:      "azure",
		},
		{
			description:   "default standard internal LB should get cluster name",
			vmSet:         primary,
			useStandardLB: true,
			isInternal:    true,
			clusterName:   "azure",
			expected:      "azure-internal",
		},
		{
			description:   "non-default standard external LB should get cluster-name",
			vmSet:         "as",
			useStandardLB: true,
			clusterName:   "azure",
			expected:      "azure",
		},
		{
			description:   "non-default standard internal LB should get cluster-name",
			vmSet:         "as",
			useStandardLB: true,
			isInternal:    true,
			clusterName:   "azure",
			expected:      "azure-internal",
		},
		{
			description:   "should select the most eligible load balancer when using multi-slb",
			vmSet:         primary,
			useStandardLB: true,
			multiSLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "a",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceLabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k1": "v1"},
						},
					},
				},
				{
					Name: "b",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceLabelSelector: &metav1.LabelSelector{
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      "k2",
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{"v1", "v2", "v3"},
								},
							},
						},
					},
				},
				{
					Name: "c",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceNamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k2": "v2"},
						},
					},
				},
			},
			serviceAnnotation: map[string]string{consts.ServiceAnnotationLoadBalancerConfigurations: "a,b"},
			serviceLabel:      map[string]string{"k2": "v2"},
			isInternal:        true,
			expected:          "b-internal",
		},
		{
			description:   "should report an error if failed to select eligible load balancers",
			vmSet:         primary,
			useStandardLB: true,
			multiSLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "a",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						ServiceLabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k1": "v1"},
						},
					},
				},
			},
			expectedErr: errors.New(`service "default/test" selects 1 load balancers (a), but 0 of them () have AllowServicePlacement set to false and the service is not using any of them, 1 of them (a) do not match the service label selector, and 0 of them () do not match the service namespace selector`),
		},
	}

	for _, c := range cases {
		t.Run(c.description, func(t *testing.T) {
			if c.useStandardLB {
				az.Config.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			} else {
				az.Config.LoadBalancerSKU = consts.LoadBalancerSKUBasic
			}

			if len(c.multiSLBConfigs) > 0 {
				az.MultipleStandardLoadBalancerConfigurations = c.multiSLBConfigs
			}

			az.Config.LoadBalancerName = c.lbName
			svc := getTestService("test", v1.ProtocolTCP, c.serviceAnnotation, false)
			if c.serviceLabel != nil {
				svc.Labels = c.serviceLabel
			}
			loadbalancerName, err := az.getAzureLoadBalancerName(context.TODO(), &svc, []*armnetwork.LoadBalancer{}, c.clusterName, c.vmSet, c.isInternal)
			assert.Equal(t, c.expected, loadbalancerName)
			if c.expectedErr != nil {
				assert.EqualError(t, err, c.expectedErr.Error())
			}
		})
	}
}

func TestGetMostEligibleLBName(t *testing.T) {
	for _, tc := range []struct {
		description    string
		currentLBName  string
		eligibleLBs    []string
		existingLBs    []*armnetwork.LoadBalancer
		isInternal     bool
		expectedLBName string
	}{
		{
			description:    "should return current LB name if it is eligible",
			currentLBName:  "lb1",
			eligibleLBs:    []string{"lb1", "lb2"},
			expectedLBName: "lb1",
		},
		{
			description:   "should return eligible LBs with fewest rules",
			currentLBName: "lb1",
			eligibleLBs:   []string{"lb2", "lb3"},
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb2"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{
							{},
							{},
							{},
						},
					},
				},
				{
					Name: ptr.To("lb3"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{
							{},
							{},
						},
					},
				},
				{
					Name: ptr.To("lb4"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{
							{},
						},
					},
				},
			},
			expectedLBName: "lb3",
		},
		{
			description:    "should return the first eligible LB if there is no existing eligible LBs",
			eligibleLBs:    []string{"lb1", "lb2"},
			expectedLBName: "lb1",
		},
		{
			description: "should return the first eligible LB that does not exist",
			eligibleLBs: []string{"lb1", "lb2", "lb3"},
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb3"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{},
					},
				},
			},
			expectedLBName: "lb1",
		},
		{
			description: "should respect internal load balancers",
			eligibleLBs: []string{"lb1", "lb2", "lb3"},
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb1-internal"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{
							{},
						},
					},
				},
				{
					Name: ptr.To("lb2-internal"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{},
					},
				},
				{
					Name: ptr.To("lb3-internal"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{},
					},
				},
			},
			expectedLBName: "lb2",
			isInternal:     true,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			lbName := getMostEligibleLBForService(tc.currentLBName, tc.eligibleLBs, tc.existingLBs, tc.isInternal)
			assert.Equal(t, tc.expectedLBName, lbName)
		})
	}
}

func TestReconcileMultipleStandardLoadBalancerConfigurations(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description            string
		useMultipleLB          bool
		noPrimaryConfig        bool
		nodes                  []*v1.Node
		expectedActiveServices map[string]*utilsets.IgnoreCaseSet
		expectedErr            error
	}{
		{
			description:   "should set active services correctly",
			useMultipleLB: true,
			expectedActiveServices: map[string]*utilsets.IgnoreCaseSet{
				"kubernetes": utilsets.NewString("default/lbsvconkubernetes"),
				"lb1":        utilsets.NewString("ns1/lbsvconlb1"),
			},
		},
		{
			description: "should do nothing when using single standard LB",
		},
		{
			description:     "should report an error if there is no primary LB config",
			useMultipleLB:   true,
			noPrimaryConfig: true,
			expectedErr:     errors.New(`multiple standard load balancers are enabled but no configuration named "kubernetes" is found`),
		},
	} {
		az := GetTestCloud(ctrl)
		az.LoadBalancerSKU = consts.LoadBalancerSKUStandard

		t.Run(tc.description, func(t *testing.T) {
			existingSvcs := []v1.Service{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "newLBSvc",
						Namespace: "default",
					},
					Spec: v1.ServiceSpec{
						Type: v1.ServiceTypeLoadBalancer,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "LBSvcOnKubernetes",
						Namespace: "default",
						UID:       types.UID("lbSvcOnKubernetesUID"),
					},
					Spec: v1.ServiceSpec{
						Type: v1.ServiceTypeLoadBalancer,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "LBSvcOnLB1",
						Namespace: "ns1",
						UID:       types.UID("lbSvcOnLB1UID"),
					},
					Spec: v1.ServiceSpec{
						Type: v1.ServiceTypeLoadBalancer,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "nonLBSvc",
						UID:  types.UID("nonLBSvcUID"),
					},
				},
			}
			az.KubeClient = fake.NewSimpleClientset(&existingSvcs[0], &existingSvcs[1], &existingSvcs[2], &existingSvcs[3])

			lbSvcOnKubernetesRuleName := az.getLoadBalancerRuleName(&existingSvcs[1], v1.ProtocolTCP, 80, false)
			lbSvcOnLB1RuleName := az.getLoadBalancerRuleName(&existingSvcs[2], v1.ProtocolTCP, 80, false)
			existingLBs := []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("kubernetes-internal"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{
							{Name: &lbSvcOnKubernetesRuleName},
						},
					},
				},
				{
					Name: ptr.To("lb1"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{
							{Name: &lbSvcOnLB1RuleName},
						},
					},
				},
				{
					Name: ptr.To("lb2"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						LoadBalancingRules: []*armnetwork.LoadBalancingRule{},
					},
				},
			}

			if tc.useMultipleLB {
				az.LoadBalancerBackendPool = newBackendPoolTypeNodeIP(az)
				az.MultipleStandardLoadBalancerConfigurations = []config.MultipleStandardLoadBalancerConfiguration{
					{Name: "lb1"},
					{Name: "lb2"},
				}
				if !tc.noPrimaryConfig {
					az.MultipleStandardLoadBalancerConfigurations = append(az.MultipleStandardLoadBalancerConfigurations, config.MultipleStandardLoadBalancerConfiguration{Name: "kubernetes"})
				}
			}

			svc := getTestService("test", v1.ProtocolTCP, nil, false)
			err := az.reconcileMultipleStandardLoadBalancerConfigurations(context.TODO(), existingLBs, &svc, "kubernetes", existingLBs, tc.nodes)
			assert.Equal(t, err, tc.expectedErr)

			activeServices := make(map[string]*utilsets.IgnoreCaseSet)
			for _, multiSLBConfig := range az.MultipleStandardLoadBalancerConfigurations {
				activeServices[multiSLBConfig.Name] = multiSLBConfig.ActiveServices
			}
			for lbConfigName, svcNames := range tc.expectedActiveServices {
				assert.Equal(t, svcNames, activeServices[lbConfigName])
			}
		})
	}
}

func TestGetFrontendIPConfigName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	az.PrimaryAvailabilitySetName = primary

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerInternalSubnet: "subnet",
				consts.ServiceAnnotationLoadBalancerInternal:       "true",
			},
			UID: "257b9655-5137-4ad2-b091-ef3f07043ad3",
		},
	}

	cases := []struct {
		description   string
		subnetName    string
		isInternal    bool
		useStandardLB bool
		expected      string
	}{
		{
			description:   "internal lb should have subnet name on the frontend ip configuration name",
			subnetName:    "shortsubnet",
			isInternal:    true,
			useStandardLB: true,
			expected:      "a257b965551374ad2b091ef3f07043ad-shortsubnet",
		},
		{
			description:   "internal lb should have subnet name on the frontend ip configuration name but truncated to 80 characters, also not end with char like '-'",
			subnetName:    "a--------------------------------------------------z",
			isInternal:    true,
			useStandardLB: true,
			expected:      "a257b965551374ad2b091ef3f07043ad-a----------------------------------------_",
		},
		{
			description:   "internal standard lb should have subnet name on the frontend ip configuration name but truncated to 80 characters",
			subnetName:    "averylonnnngggnnnnnnnnnnnnnnnnnnnnnngggggggggggggggggggggggggggggggggggggsubet",
			isInternal:    true,
			useStandardLB: true,
			expected:      "a257b965551374ad2b091ef3f07043ad-averylonnnngggnnnnnnnnnnnnnnnnnnnnnngggggg",
		},
		{
			description:   "internal basic lb should have subnet name on the frontend ip configuration name but truncated to 80 characters",
			subnetName:    "averylonnnngggnnnnnnnnnnnnnnnnnnnnnngggggggggggggggggggggggggggggggggggggsubet",
			isInternal:    true,
			useStandardLB: false,
			expected:      "a257b965551374ad2b091ef3f07043ad-averylonnnngggnnnnnnnnnnnnnnnnnnnnnngggggg",
		},
		{
			description:   "external standard lb should not have subnet name on the frontend ip configuration name",
			subnetName:    "shortsubnet",
			isInternal:    false,
			useStandardLB: true,
			expected:      "a257b965551374ad2b091ef3f07043ad",
		},
		{
			description:   "external basic lb should not have subnet name on the frontend ip configuration name",
			subnetName:    "shortsubnet",
			isInternal:    false,
			useStandardLB: false,
			expected:      "a257b965551374ad2b091ef3f07043ad",
		},
	}

	for _, c := range cases {
		t.Run(c.description, func(t *testing.T) {
			if c.useStandardLB {
				az.Config.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			} else {
				az.Config.LoadBalancerSKU = consts.LoadBalancerSKUBasic
			}
			svc.Annotations[consts.ServiceAnnotationLoadBalancerInternalSubnet] = c.subnetName
			svc.Annotations[consts.ServiceAnnotationLoadBalancerInternal] = strconv.FormatBool(c.isInternal)

			ipconfigName := az.getDefaultFrontendIPConfigName(svc)
			assert.Equal(t, c.expected, ipconfigName)
		})
	}
}

func TestGetFrontendIPConfigNames(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	az.PrimaryAvailabilitySetName = primary

	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerInternalSubnet: "subnet",
				consts.ServiceAnnotationLoadBalancerInternal:       "true",
			},
			UID: "257b9655-5137-4ad2-b091-ef3f07043ad3",
		},
		Spec: v1.ServiceSpec{
			IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
		},
	}

	cases := []struct {
		description   string
		subnetName    string
		isInternal    bool
		useStandardLB bool
		expectedV4    string
		expectedV6    string
	}{
		{
			description:   "internal lb should have subnet name on the frontend ip configuration name",
			subnetName:    "shortsubnet",
			isInternal:    true,
			useStandardLB: true,
			expectedV4:    "a257b965551374ad2b091ef3f07043ad-shortsubnet",
			expectedV6:    "a257b965551374ad2b091ef3f07043ad-shortsubnet-IPv6",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.description, func(t *testing.T) {
			if c.useStandardLB {
				az.Config.LoadBalancerSKU = consts.LoadBalancerSKUStandard
			} else {
				az.Config.LoadBalancerSKU = consts.LoadBalancerSKUBasic
			}
			svc.Annotations[consts.ServiceAnnotationLoadBalancerInternalSubnet] = c.subnetName
			svc.Annotations[consts.ServiceAnnotationLoadBalancerInternal] = strconv.FormatBool(c.isInternal)

			ipconfigNames := az.getFrontendIPConfigNames(svc)
			assert.Equal(t, c.expectedV4, ipconfigNames[false])
			assert.Equal(t, c.expectedV6, ipconfigNames[true])
		})
	}
}

func TestServiceOwnsFrontendIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		desc                 string
		existingPIPs         []*armnetwork.PublicIPAddress
		fip                  *armnetwork.FrontendIPConfiguration
		service              *v1.Service
		isOwned              bool
		isPrimary            bool
		expectedFIPIPVersion armnetwork.IPVersion
		listError            error
	}{
		{
			desc: "serviceOwnsFrontendIP should detect the primary service",
			fip: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("auid"),
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("uid"),
				},
			},
			isOwned:   true,
			isPrimary: true,
		},
		{
			desc: "serviceOwnsFrontendIP should return false if the secondary external service doesn't set it's loadBalancer IP",
			fip: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("auid"),
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("secondary"),
				},
			},
		},
		{
			desc: "serviceOwnsFrontendIP should report a not found error if there is no public IP " +
				"found according to the external service's loadBalancer IP but do not return the error",
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					ID: ptr.To("pip"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("4.3.2.1"),
					},
				},
			},
			fip: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("auid"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{
						ID: ptr.To("pip"),
					},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID:         types.UID("secondary"),
					Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "1.2.3.4"},
				},
			},
		},
		{
			desc: "serviceOwnsFrontendIP should return correct FIP IP version",
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					ID: ptr.To("pip"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress:              ptr.To("4.3.2.1"),
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
					},
				},
			},
			fip: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("auid"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{
						ID: ptr.To("pip"),
					},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID:         types.UID("secondary"),
					Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "4.3.2.1"},
				},
			},
			expectedFIPIPVersion: armnetwork.IPVersionIPv4,
			isOwned:              true,
		},
		{
			desc: "serviceOwnsFrontendIP should return false if there is a mismatch between the PIP's ID and " +
				"the counterpart on the frontend IP config",
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					ID: ptr.To("pip"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("4.3.2.1"),
					},
				},
			},
			fip: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("auid"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{
						ID: ptr.To("pip1"),
					},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID:         types.UID("secondary"),
					Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "4.3.2.1"},
				},
			},
		},
		{
			desc: "serviceOwnsFrontendIP should return false if there is no public IP address in the frontend IP config",
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					ID: ptr.To("pip"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("4.3.2.1"),
					},
				},
			},
			fip: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("auid"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPPrefix: &armnetwork.SubResource{
						ID: ptr.To("pip1"),
					},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("secondary"),
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "4.3.2.1",
					},
				},
			},
		},
		{
			desc: "serviceOwnsFrontendIP should detect the secondary external service",
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					ID: ptr.To("pip"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("4.3.2.1"),
					},
				},
			},
			fip: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("auid"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{
						ID: ptr.To("pip"),
					},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("secondary"),
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "4.3.2.1",
						consts.ServiceAnnotationLoadBalancerIPDualStack[true]:  "fd00::eef0",
					},
				},
			},
			isOwned: true,
		},
		{
			desc: "serviceOwnsFrontendIP should detect the secondary external service dual-stack",
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip"),
					ID:   ptr.To("pip"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv4),
						IPAddress:              ptr.To("4.3.2.1"),
					},
				},
				{
					Name: ptr.To("pip1"),
					ID:   ptr.To("pip1"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv6),
						IPAddress:              ptr.To("fd00::eef0"),
					},
				},
			},
			fip: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("auid"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{
						Properties: &armnetwork.PublicIPAddressPropertiesFormat{
							PublicIPAddressVersion: to.Ptr(armnetwork.IPVersionIPv6),
						},
						ID: ptr.To("pip1"),
					},
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("secondary"),
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "4.3.2.1",
						consts.ServiceAnnotationLoadBalancerIPDualStack[true]:  "fd00::eef0",
					},
				},
			},
			isOwned:   true,
			isPrimary: false,
		},
		{
			desc: "serviceOwnsFrontendIP should detect the secondary internal service",
			fip: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("auid"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAddress: ptr.To("4.3.2.1"),
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("secondary"),
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerInternal:           "true",
						consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "4.3.2.1",
					},
				},
			},
			isOwned: true,
		},
		{
			desc: "serviceOwnsFrontendIP should detect the secondary internal service - dualstack",
			fip: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("auid"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAddress: ptr.To("fd00::eef0"),
				},
			},
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					UID: types.UID("secondary"),
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerInternal:           "true",
						consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "4.3.2.1",
						consts.ServiceAnnotationLoadBalancerIPDualStack[true]:  "fd00::eef0",
					},
				},
			},
			isOwned: true,
		},
		{
			desc:      "serviceOwnsFrontendIP should return false if failed to find matched pip by name",
			service:   &v1.Service{},
			listError: &azcore.ResponseError{ErrorCode: "error"},
		},
		{
			desc: "serviceOwnsFrontnedIP should support search pip by name",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{consts.ServiceAnnotationPIPNameDualStack[false]: "pip1"},
				},
			},
			existingPIPs: []*armnetwork.PublicIPAddress{
				{
					Name: ptr.To("pip1"),
					ID:   ptr.To("pip1"),
					Properties: &armnetwork.PublicIPAddressPropertiesFormat{
						IPAddress: ptr.To("1.2.3.4"),
					},
				},
			},
			fip: &armnetwork.FrontendIPConfiguration{
				Name: ptr.To("test"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{
						ID: ptr.To("pip1"),
					},
				},
			},
			isOwned: true,
		},
	}

	for _, test := range testCases {
		test := test
		t.Run(test.desc, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			if test.existingPIPs != nil {
				mockPIPsClient := cloud.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
				mockPIPsClient.EXPECT().List(gomock.Any(), "rg").Return(test.existingPIPs, test.listError).MaxTimes(2)
			}
			isOwned, isPrimary, fipIPVersion := cloud.serviceOwnsFrontendIP(context.TODO(), test.fip, test.service)
			if test.expectedFIPIPVersion != "" {
				assert.Equal(t, test.expectedFIPIPVersion, *fipIPVersion)
			}
			assert.Equal(t, test.isOwned, isOwned)
			assert.Equal(t, test.isPrimary, isPrimary)
		})
	}
}

func TestReconcileMultipleStandardLoadBalancerNodes(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description          string
		lbName               string
		init                 bool
		existingLBConfigs    []config.MultipleStandardLoadBalancerConfiguration
		existingNodes        []*v1.Node
		existingLBs          []*armnetwork.LoadBalancer
		expectedPutLBTimes   int
		expectedLBToNodesMap map[string]*utilsets.IgnoreCaseSet
	}{
		{
			description: "should remove unwanted nodes and arrange existing nodes with primary vmSet as expected",
			existingLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-1",
					},
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString("node1", "node2"),
					},
				},
				{
					Name: "lb2",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-2",
					},
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString("node3", "node4"),
					},
				},
			},
			existingNodes: []*v1.Node{
				getTestNodeWithMetadata("node1", "vmss-1", nil, "10.1.0.1"),
				getTestNodeWithMetadata("node2", "vmss-2", nil, "10.1.0.2"),
				getTestNodeWithMetadata("node3", "vmss-2", nil, "10.1.0.3"),
			},
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb1-internal"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
								Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
									LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
										{
											Name: ptr.To("node1"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.1"),
											},
										},
										{
											Name: ptr.To("node2"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.2"),
											},
										},
									},
								},
							},
						},
					},
				},
				{
					Name: ptr.To("lb2"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
								Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
									LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
										{
											Name: ptr.To("node3"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.3"),
											},
										},
										{
											Name: ptr.To("node4"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.4"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedPutLBTimes: 1,
			expectedLBToNodesMap: map[string]*utilsets.IgnoreCaseSet{
				"lb1": utilsets.NewString("node1"),
				"lb2": utilsets.NewString("node2", "node3"),
			},
		},
		{
			description: "should respect node selector",
			lbName:      "lb2-internal",
			existingLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-1",
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k1": "v1"},
						},
					},
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString("node1", "node2"),
					},
				},
				{
					Name: "lb2",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-2",
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k2": "v2"},
						},
					},
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString("node3", "node4"),
					},
				},
				{
					Name: "lb3",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-2",
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k2": "v2"},
						},
					},
				},
				{
					Name: "lb4",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-2",
					},
				},
			},
			existingNodes: []*v1.Node{
				getTestNodeWithMetadata("node1", "vmss-1", map[string]string{"k2": "v2"}, "10.1.0.1"),
				getTestNodeWithMetadata("node2", "vmss-3", map[string]string{"k3": "v3"}, "10.1.0.2"),
				getTestNodeWithMetadata("node3", "vmss-3", map[string]string{"k2": "v2"}, "10.1.0.3"),
				getTestNodeWithMetadata("node5", "vmss-3", map[string]string{"k2": "v2"}, "10.1.0.5"),
				getTestNodeWithMetadata("node6", "vmss-3", map[string]string{"k3": "v3"}, "10.1.0.6"),
			},
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb1-internal"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
								Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
									LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
										{
											Name: ptr.To("node1"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.1"),
											},
										},
										{
											Name: ptr.To("node2"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.2"),
											},
										},
									},
								},
							},
						},
					},
				},
				{
					Name: ptr.To("lb3"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
							},
						},
					},
				},
				{
					Name: ptr.To("lb4"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
							},
						},
					},
				},
			},
			expectedLBToNodesMap: map[string]*utilsets.IgnoreCaseSet{
				"lb1": utilsets.NewString("node1"),
				"lb2": utilsets.NewString("node3"),
				"lb3": utilsets.NewString("node5"),
				"lb4": utilsets.NewString("node2", "node6"),
			},
		},
		{
			description: "should handle empty node selector",
			existingLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-1",
						NodeSelector: &metav1.LabelSelector{},
					},
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString("node1"),
					},
				},
				{
					Name: "lb2",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-2",
					},
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString("node2", "node3"),
					},
				},
			},
			existingNodes: []*v1.Node{
				getTestNodeWithMetadata("node1", "vmss-1", nil, "10.1.0.1"),
				getTestNodeWithMetadata("node2", "vmss-2", nil, "10.1.0.2"),
				getTestNodeWithMetadata("node3", "vmss-2", nil, "10.1.0.3"),
			},
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb1"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
								Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
									LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
										{
											Name: ptr.To("node1"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.1"),
											},
										},
									},
								},
							},
						},
					},
				},
				{
					Name: ptr.To("lb2"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
								Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
									LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
										{
											Name: ptr.To("node2"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.2"),
											},
										},
										{
											Name: ptr.To("node3"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.3"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedLBToNodesMap: map[string]*utilsets.IgnoreCaseSet{
				"lb1": utilsets.NewString("node1"),
				"lb2": utilsets.NewString("node2", "node3"),
			},
		},
		{
			description: "should remove the node on the lb if it is no longer eligible",
			existingLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-1",
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k1": "v1"},
						},
					},
					MultipleStandardLoadBalancerConfigurationStatus: config.MultipleStandardLoadBalancerConfigurationStatus{
						ActiveNodes: utilsets.NewString("node1", "node2"),
					},
				},
			},
			existingNodes: []*v1.Node{
				getTestNodeWithMetadata("node1", "vmss-2", map[string]string{"k2": "v2"}, "10.1.0.1"),
			},
			expectedLBToNodesMap: map[string]*utilsets.IgnoreCaseSet{
				"lb1": utilsets.NewString(),
			},
		},
		{
			description: "should skip lbs that do not exist or will not be created",
			existingLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-1",
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k1": "v1"},
						},
					},
				},
				{
					Name: "lb2",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-2",
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k2": "v2"},
						},
					},
				},
				{
					Name: "lb3",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-2",
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k3": "v3"},
						},
					},
				},
				{
					Name: "lb4",
				},
			},
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb2-internal"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{Name: ptr.To("kubernetes")},
						},
					},
				},
				{
					Name: ptr.To("lb4"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
							},
						},
					},
				},
			},
			existingNodes: []*v1.Node{
				getTestNodeWithMetadata("node1", "vmss-1", map[string]string{"k1": "v1"}, "10.1.0.1"),
				getTestNodeWithMetadata("node2", "vmss-3", map[string]string{"k3": "v3"}, "10.1.0.2"),
				getTestNodeWithMetadata("node3", "vmss-3", map[string]string{"k2": "v2"}, "10.1.0.3"),
				getTestNodeWithMetadata("node5", "vmss-3", map[string]string{"k2": "v2"}, "10.1.0.5"),
				getTestNodeWithMetadata("node6", "vmss-3", map[string]string{"k3": "v3"}, "10.1.0.6"),
			},
			expectedLBToNodesMap: map[string]*utilsets.IgnoreCaseSet{
				"lb1": nil,
				"lb2": utilsets.NewString("node3", "node5"),
				"lb3": nil,
				"lb4": utilsets.NewString("node1", "node2", "node6"),
			},
		},
		{
			description: "should skip lbs that do not exist or will not be created when no lb is selected by node selector",
			existingLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-1",
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k1": "v1"},
						},
					},
				},
				{
					Name: "lb2",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-2",
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k2": "v2"},
						},
					},
				},
				{
					Name: "lb3",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-2",
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k3": "v3"},
						},
					},
				},
				{
					Name: "lb4",
				},
			},
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb2-internal"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{Name: ptr.To("kubernetes")},
						},
					},
				},
			},
			existingNodes: []*v1.Node{
				getTestNodeWithMetadata("node1", "vmss-1", map[string]string{"k1": "v1"}, "10.1.0.1"),
				getTestNodeWithMetadata("node2", "vmss-3", map[string]string{"k3": "v3"}, "10.1.0.2"),
				getTestNodeWithMetadata("node3", "vmss-3", map[string]string{"k2": "v2"}, "10.1.0.3"),
				getTestNodeWithMetadata("node5", "vmss-3", map[string]string{"k2": "v2"}, "10.1.0.5"),
				getTestNodeWithMetadata("node6", "vmss-3", map[string]string{"k3": "v3"}, "10.1.0.6"),
			},
			expectedLBToNodesMap: map[string]*utilsets.IgnoreCaseSet{
				"lb1": nil,
				"lb2": utilsets.NewString("node3", "node5"),
				"lb3": nil,
				"lb4": nil,
			},
		},
		{
			description: "should record current node distributions after restarting the controller",
			init:        true,
			existingLBConfigs: []config.MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-1",
					},
				},
				{
					Name: "lb2",
					MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
						PrimaryVMSet: "vmss-2",
					},
				},
			},
			existingNodes: []*v1.Node{
				getTestNodeWithMetadata("node1", "vmss-1", map[string]string{"k1": "v1"}, "10.1.0.1"),
				getTestNodeWithMetadata("node2", "vmss-2", map[string]string{"k3": "v3"}, "10.1.0.2"),
				getTestNodeWithMetadata("node3", "vmss-3", map[string]string{"k2": "v2"}, "10.1.0.3"),
				getTestNodeWithMetadata("node5", "vmss-5", map[string]string{"k2": "v2"}, "10.1.0.5"),
				getTestNodeWithMetadata("node6", "vmss-6", map[string]string{"k3": "v3"}, "10.1.0.6"),
			},
			existingLBs: []*armnetwork.LoadBalancer{
				{
					Name: ptr.To("lb1-internal"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
								Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
									LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
										{
											Name: ptr.To("node2"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.2"),
											},
										},
									},
								},
							},
						},
					},
				},
				{
					Name: ptr.To("lb2"),
					Properties: &armnetwork.LoadBalancerPropertiesFormat{
						BackendAddressPools: []*armnetwork.BackendAddressPool{
							{
								Name: ptr.To("kubernetes"),
								Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
									LoadBalancerBackendAddresses: []*armnetwork.LoadBalancerBackendAddress{
										{
											Name: ptr.To("node3"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.3"),
											},
										},
										{
											Name: ptr.To("node4"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.4"),
											},
										},
										{
											Name: ptr.To("node5"),
											Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
												IPAddress: ptr.To("10.1.0.5"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
			expectedLBToNodesMap: map[string]*utilsets.IgnoreCaseSet{
				"lb1": utilsets.NewString("node1", "node6"),
				"lb2": utilsets.NewString("node2", "node3", "node5"),
			},
		},
	} {
		tc := tc
		t.Run(tc.description, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			ss, _ := NewTestScaleSet(ctrl)
			ss.DisableAvailabilitySetNodes = true
			az.VMSet = ss
			az.nodePrivateIPToNodeNameMap = map[string]string{
				"10.1.0.1": "node1",
				"10.1.0.2": "node2",
				"10.1.0.3": "node3",
				"10.1.0.4": "node4",
				"10.1.0.5": "node5",
				"10.1.0.6": "node6",
			}
			az.LoadBalancerBackendPool = newBackendPoolTypeNodeIP(az)
			az.MultipleStandardLoadBalancerConfigurations = tc.existingLBConfigs
			svc := getTestService("test", v1.ProtocolTCP, nil, false)
			_ = az.reconcileMultipleStandardLoadBalancerBackendNodes(context.TODO(), "kubernetes", tc.lbName, tc.existingLBs, &svc, tc.existingNodes, tc.init)

			expectedLBToNodesMap := make(map[string]*utilsets.IgnoreCaseSet)
			for _, multiSLBConfig := range az.MultipleStandardLoadBalancerConfigurations {
				expectedLBToNodesMap[multiSLBConfig.Name] = multiSLBConfig.ActiveNodes
			}
			assert.Equal(t, tc.expectedLBToNodesMap, expectedLBToNodesMap)
		})
	}
}

func getTestNodeWithMetadata(nodeName, vmssName string, labels map[string]string, ip string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nodeName,
			Labels: labels,
		},
		Spec: v1.NodeSpec{
			ProviderID: fmt.Sprintf("azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/%s/virtualMachines/0", vmssName),
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: ip,
				},
			},
		},
	}
}

func TestAddOrUpdateLBInList(t *testing.T) {
	existingLBs := []*armnetwork.LoadBalancer{
		{
			Name: ptr.To("lb1"),
			Properties: &armnetwork.LoadBalancerPropertiesFormat{
				BackendAddressPools: []*armnetwork.BackendAddressPool{
					{Name: ptr.To("kubernetes")},
				},
			},
		},
		{
			Name: ptr.To("lb2"),
			Properties: &armnetwork.LoadBalancerPropertiesFormat{
				BackendAddressPools: []*armnetwork.BackendAddressPool{
					{Name: ptr.To("kubernetes")},
				},
			},
		},
	}
	targetLB := &armnetwork.LoadBalancer{
		Name: ptr.To("lb1"),
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			BackendAddressPools: []*armnetwork.BackendAddressPool{
				{Name: ptr.To("lb1")},
			},
		},
	}
	expectedLBs := []*armnetwork.LoadBalancer{
		{
			Name: ptr.To("lb1"),
			Properties: &armnetwork.LoadBalancerPropertiesFormat{
				BackendAddressPools: []*armnetwork.BackendAddressPool{
					{Name: ptr.To("lb1")},
				},
			},
		},
		{
			Name: ptr.To("lb2"),
			Properties: &armnetwork.LoadBalancerPropertiesFormat{
				BackendAddressPools: []*armnetwork.BackendAddressPool{
					{Name: ptr.To("kubernetes")},
				},
			},
		},
	}

	addOrUpdateLBInList(&existingLBs, targetLB)
	assert.Equal(t, expectedLBs, existingLBs)

	targetLB = &armnetwork.LoadBalancer{
		Name: ptr.To("lb3"),
	}
	expectedLBs = []*armnetwork.LoadBalancer{
		{
			Name: ptr.To("lb1"),
			Properties: &armnetwork.LoadBalancerPropertiesFormat{
				BackendAddressPools: []*armnetwork.BackendAddressPool{
					{Name: ptr.To("lb1")},
				},
			},
		},
		{
			Name: ptr.To("lb2"),
			Properties: &armnetwork.LoadBalancerPropertiesFormat{
				BackendAddressPools: []*armnetwork.BackendAddressPool{
					{Name: ptr.To("kubernetes")},
				},
			},
		},
		{Name: ptr.To("lb3")},
	}

	addOrUpdateLBInList(&existingLBs, targetLB)
	assert.Equal(t, expectedLBs, existingLBs)
}

func TestReconcileBackendPoolHosts(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	svc := getTestService("test", v1.ProtocolTCP, nil, false)
	lbBackendPoolIDs := map[bool]string{false: "id"}
	clusterName := "kubernetes"
	ips := []string{"10.0.0.1"}
	bp1 := buildTestLoadBalancerBackendPoolWithIPs(clusterName, ips)
	bp2 := buildTestLoadBalancerBackendPoolWithIPs(clusterName, ips)
	ips = []string{"10.0.0.2", "10.0.0.3"}
	bp3 := buildTestLoadBalancerBackendPoolWithIPs(clusterName, ips)
	lb1 := &armnetwork.LoadBalancer{
		Name: ptr.To(clusterName),
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			BackendAddressPools: []*armnetwork.BackendAddressPool{bp1},
		},
	}
	lb2 := &armnetwork.LoadBalancer{
		Name: ptr.To("lb2"),
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			BackendAddressPools: []*armnetwork.BackendAddressPool{bp2},
		},
	}
	expectedLB := &armnetwork.LoadBalancer{
		Name: ptr.To(clusterName),
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			BackendAddressPools: []*armnetwork.BackendAddressPool{bp3},
		},
	}
	existingLBs := []*armnetwork.LoadBalancer{lb1, lb2}

	cloud := GetTestCloud(ctrl)
	mockLBBackendPool := NewMockBackendPool(ctrl)
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), bp1).DoAndReturn(fakeEnsureHostsInPool())
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), bp2).Return(nil)
	cloud.LoadBalancerBackendPool = mockLBBackendPool

	var err error
	lb1, err = cloud.reconcileBackendPoolHosts(context.Background(), lb1, existingLBs, &svc, []*v1.Node{}, clusterName, "vmss", lbBackendPoolIDs)
	assert.NoError(t, err)
	assert.Equal(t, expectedLB, lb1)

	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(errors.New("error"))
	_, err = cloud.reconcileBackendPoolHosts(context.Background(), lb1, existingLBs, &svc, []*v1.Node{}, clusterName, "vmss", lbBackendPoolIDs)
	assert.Equal(t, errors.New("error"), err)
}

func fakeEnsureHostsInPool() func(context.Context, *v1.Service, []*v1.Node, string, string, string, string, *armnetwork.BackendAddressPool) error {
	return func(_ context.Context, _ *v1.Service, _ []*v1.Node, _, _, _, _ string, backendPool *armnetwork.BackendAddressPool) error {
		backendPool.Properties.LoadBalancerBackendAddresses = []*armnetwork.LoadBalancerBackendAddress{
			{
				Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
					IPAddress: ptr.To("10.0.0.2"),
				},
			},
			{
				Properties: &armnetwork.LoadBalancerBackendAddressPropertiesFormat{
					IPAddress: ptr.To("10.0.0.3"),
				},
			},
		}
		return nil
	}
}
