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
	"strconv"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/interfaceclient/mockinterfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmasclient/mockvmasclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

const (
	networkResourceTenantID       = "networkResourceTenantID"
	networkResourceSubscriptionID = "networkResourceSubscriptionID"
	asID                          = "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/availabilitySets/myAvailabilitySet"
	primary                       = "primary"
)

func TestIsControlPlaneNode(t *testing.T) {
	if isControlPlaneNode(&v1.Node{}) {
		t.Errorf("Empty node should not be master!")
	}
	if isControlPlaneNode(&v1.Node{
		ObjectMeta: meta.ObjectMeta{
			Labels: map[string]string{
				consts.NodeLabelRole: "worker",
			},
		},
	}) {
		t.Errorf("Node labelled 'worker' should not be control plane!")
	}
	if !isControlPlaneNode(&v1.Node{
		ObjectMeta: meta.ObjectMeta{
			Labels: map[string]string{
				consts.NodeLabelRole: "master",
			},
		},
	}) {
		t.Errorf("Node with kubernetes.io/role: \"master\" label should be control plane!")
	}
	if !isControlPlaneNode(&v1.Node{
		ObjectMeta: meta.ObjectMeta{
			Labels: map[string]string{
				consts.MasterNodeRoleLabel: "",
			},
		},
	}) {
		t.Errorf("Node with node-role.kubernetes.io/master: \"\" label should be control plane!")
	}
	if !isControlPlaneNode(&v1.Node{
		ObjectMeta: meta.ObjectMeta{
			Labels: map[string]string{
				consts.ControlPlaneNodeRoleLabel: "",
			},
		},
	}) {
		t.Errorf("Node with node-role.kubernetes.io/control-plane: \"\" label should be control plane!")
	}
}

func TestGetLastSegment(t *testing.T) {
	tests := []struct {
		ID        string
		separator string
		expected  string
		expectErr bool
	}{
		{
			ID:        "",
			separator: "/",
			expected:  "",
			expectErr: true,
		},
		{
			ID:        "foo/",
			separator: "/",
			expected:  "",
			expectErr: true,
		},
		{
			ID:        "foo/bar",
			separator: "/",
			expected:  "bar",
			expectErr: false,
		},
		{
			ID:        "foo/bar/baz",
			separator: "/",
			expected:  "baz",
			expectErr: false,
		},
		{
			ID:        "k8s-agentpool-36841236-vmss_1",
			separator: "_",
			expected:  "1",
			expectErr: false,
		},
	}

	for _, test := range tests {
		s, e := getLastSegment(test.ID, test.separator)
		if test.expectErr && e == nil {
			t.Errorf("Expected err, but it was nil")
			continue
		}
		if !test.expectErr && e != nil {
			t.Errorf("Unexpected error: %v", e)
			continue
		}
		if s != test.expected {
			t.Errorf("expected: %s, got %s", test.expected, s)
		}
	}
}

func TestGenerateStorageAccountName(t *testing.T) {
	tests := []struct {
		prefix string
	}{
		{
			prefix: "",
		},
		{
			prefix: "pvc",
		},
		{
			prefix: "1234512345123451234512345",
		},
	}

	for _, test := range tests {
		accountName := generateStorageAccountName(test.prefix)
		if len(accountName) > consts.StorageAccountNameMaxLength || len(accountName) < 3 {
			t.Errorf("input prefix: %s, output account name: %s, length not in [3,%d]", test.prefix, accountName, consts.StorageAccountNameMaxLength)
		}

		for _, char := range accountName {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
				t.Errorf("input prefix: %s, output account name: %s, there is non-digit or non-letter(%q)", test.prefix, accountName, char)
				break
			}
		}
	}
}

func TestMapLoadBalancerNameToVMSet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	az.PrimaryAvailabilitySetName = primary

	cases := []struct {
		description   string
		lbName        string
		useStandardLB bool
		clusterName   string
		expectedVMSet string
	}{
		{
			description:   "default external LB should map to primary vmset",
			lbName:        "azure",
			clusterName:   "azure",
			expectedVMSet: primary,
		},
		{
			description:   "default internal LB should map to primary vmset",
			lbName:        "azure-internal",
			clusterName:   "azure",
			expectedVMSet: primary,
		},
		{
			description:   "non-default external LB should map to its own vmset",
			lbName:        "azuretest-internal",
			clusterName:   "azure",
			expectedVMSet: "azuretest",
		},
		{
			description:   "non-default internal LB should map to its own vmset",
			lbName:        "azuretest-internal",
			clusterName:   "azure",
			expectedVMSet: "azuretest",
		},
	}

	for _, c := range cases {
		if c.useStandardLB {
			az.Config.LoadBalancerSku = consts.LoadBalancerSkuStandard
		} else {
			az.Config.LoadBalancerSku = consts.LoadBalancerSkuBasic
		}
		vmset := az.mapLoadBalancerNameToVMSet(c.lbName, c.clusterName)
		assert.Equal(t, c.expectedVMSet, vmset, c.description)
	}
}

func TestGetLoadBalancingRuleName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	az.PrimaryAvailabilitySetName = primary

	svc := &v1.Service{
		ObjectMeta: meta.ObjectMeta{
			Annotations: map[string]string{},
			UID:         "257b9655-5137-4ad2-b091-ef3f07043ad3",
		},
		Spec: v1.ServiceSpec{
			IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
		},
	}

	cases := []struct {
		description   string
		subnetName    string
		expected      string
		protocol      v1.Protocol
		isInternal    bool
		isIPv6        bool
		useStandardLB bool
		port          int32
	}{
		{
			description:   "internal lb should have subnet name on the rule name",
			subnetName:    "shortsubnet",
			isInternal:    true,
			useStandardLB: true,
			protocol:      v1.ProtocolTCP,
			port:          9000,
			expected:      "a257b965551374ad2b091ef3f07043ad-shortsubnet-TCP-9000",
		},
		{
			description:   "internal lb should have subnet name on the rule name IPv6",
			subnetName:    "shortsubnet",
			isInternal:    true,
			isIPv6:        true,
			useStandardLB: true,
			protocol:      v1.ProtocolTCP,
			port:          9000,
			expected:      "a257b965551374ad2b091ef3f07043ad-shortsubnet-TCP-9000-IPv6",
		},
		{
			description:   "internal standard lb should have subnet name on the rule name but truncated to 80 (-5) characters",
			subnetName:    "averylonnnngggnnnnnnnnnnnnnnnnnnnnnngggggggggggggggggggggggggggggggggggggsubet",
			isInternal:    true,
			useStandardLB: true,
			protocol:      v1.ProtocolTCP,
			port:          9000,
			expected:      "a257b965551374ad2b091ef3f07043ad-averylonnnngggnnnnnnnnnnnnnnnnnnn-TCP-9000",
		},
		{
			description:   "internal standard lb should have subnet name on the rule name but truncated to 80 (-5) characters IPv6",
			subnetName:    "averylonnnngggnnnnnnnnnnnnnnnnnnnnnngggggggggggggggggggggggggggggggggggggsubet",
			isInternal:    true,
			isIPv6:        true,
			useStandardLB: true,
			protocol:      v1.ProtocolTCP,
			port:          9000,
			expected:      "a257b965551374ad2b091ef3f07043ad-averylonnnngggnnnnnnnnnnnnnnnnnnn-TCP-9000-IPv6",
		},
		{
			description:   "internal basic lb should have subnet name on the rule name but truncated to 80 (-5) characters",
			subnetName:    "averylonnnngggnnnnnnnnnnnnnnnnnnnnnngggggggggggggggggggggggggggggggggggggsubet",
			isInternal:    true,
			useStandardLB: false,
			protocol:      v1.ProtocolTCP,
			port:          9000,
			expected:      "a257b965551374ad2b091ef3f07043ad-averylonnnngggnnnnnnnnnnnnnnnnnnn-TCP-9000",
		},
		{
			description:   "internal basic lb should have subnet name on the rule name but truncated to 80 (-5) characters IPv6",
			subnetName:    "averylonnnngggnnnnnnnnnnnnnnnnnnnnnngggggggggggggggggggggggggggggggggggggsubet",
			isInternal:    true,
			isIPv6:        true,
			useStandardLB: false,
			protocol:      v1.ProtocolTCP,
			port:          9000,
			expected:      "a257b965551374ad2b091ef3f07043ad-averylonnnngggnnnnnnnnnnnnnnnnnnn-TCP-9000-IPv6",
		},
		{
			description:   "external standard lb should not have subnet name on the rule name",
			subnetName:    "shortsubnet",
			isInternal:    false,
			useStandardLB: true,
			protocol:      v1.ProtocolTCP,
			port:          9000,
			expected:      "a257b965551374ad2b091ef3f07043ad-TCP-9000",
		},
		{
			description:   "external standard lb should not have subnet name on the rule name IPv6",
			subnetName:    "shortsubnet",
			isInternal:    false,
			isIPv6:        true,
			useStandardLB: true,
			protocol:      v1.ProtocolTCP,
			port:          9000,
			expected:      "a257b965551374ad2b091ef3f07043ad-TCP-9000-IPv6",
		},
		{
			description:   "external basic lb should not have subnet name on the rule name",
			subnetName:    "shortsubnet",
			isInternal:    false,
			useStandardLB: false,
			protocol:      v1.ProtocolTCP,
			port:          9000,
			expected:      "a257b965551374ad2b091ef3f07043ad-TCP-9000",
		},
		{
			description:   "external basic lb should not have subnet name on the rule name IPv6",
			subnetName:    "shortsubnet",
			isInternal:    false,
			isIPv6:        true,
			useStandardLB: false,
			protocol:      v1.ProtocolTCP,
			port:          9000,
			expected:      "a257b965551374ad2b091ef3f07043ad-TCP-9000-IPv6",
		},
	}

	for _, c := range cases {
		t.Run(c.description, func(t *testing.T) {
			if c.useStandardLB {
				az.Config.LoadBalancerSku = consts.LoadBalancerSkuStandard
			} else {
				az.Config.LoadBalancerSku = consts.LoadBalancerSkuBasic
			}
			svc.Annotations[consts.ServiceAnnotationLoadBalancerInternalSubnet] = c.subnetName
			svc.Annotations[consts.ServiceAnnotationLoadBalancerInternal] = strconv.FormatBool(c.isInternal)

			loadbalancerRuleName := az.getLoadBalancerRuleName(svc, c.protocol, c.port, c.isIPv6)
			assert.Equal(t, c.expected, loadbalancerRuleName)
		})
	}
}

func TestGetloadbalancerHAmodeRuleName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	az.PrimaryAvailabilitySetName = primary

	svc := &v1.Service{
		ObjectMeta: meta.ObjectMeta{
			UID: "257b9655-5137-4ad2-b091-ef3f07043ad3",
			Annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerInternal:       "true",
				consts.ServiceAnnotationLoadBalancerInternalSubnet: "shortsubnet",
			},
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Port:     9000,
					Protocol: v1.ProtocolTCP,
				},
			},
			IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
		},
	}

	cases := []struct {
		description string
		expected    string
		isIPv6      bool
	}{
		{
			description: "IPv4",
			expected:    "a257b965551374ad2b091ef3f07043ad-shortsubnet-TCP-9000",
		},
		{
			description: "IPv6",
			isIPv6:      true,
			expected:    "a257b965551374ad2b091ef3f07043ad-shortsubnet-TCP-9000-IPv6",
		},
	}

	for _, c := range cases {
		t.Run(c.description, func(t *testing.T) {
			lbHAModeRuleName := az.getloadbalancerHAmodeRuleName(svc, c.isIPv6)
			assert.Equal(t, c.expected, lbHAModeRuleName)
		})
	}
}

func TestGetFrontendIPConfigID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	testGetLoadBalancerSubResourceID(t, az, az.getFrontendIPConfigID, az.getFrontendIPConfigIDWithRG, consts.FrontendIPConfigIDTemplate)
}

func TestGetBackendPoolID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	testGetLoadBalancerSubResourceID(t, az, az.getBackendPoolID, az.getBackendPoolIDWithRG, consts.BackendPoolIDTemplate)
}

func TestGetLoadBalancerProbeID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	testGetLoadBalancerSubResourceID(t, az, az.getLoadBalancerProbeID, az.getLoadBalancerProbeIDWithRG, consts.LoadBalancerProbeIDTemplate)
}

func testGetLoadBalancerSubResourceID(
	t *testing.T,
	az *Cloud,
	getLoadBalancerSubResourceID func(string, string) string,
	getLoadBalancerSubResourceIDWithRG func(string, string, string) string,
	expectedResourceIDTemplate string) {
	cases := []struct {
		description                         string
		useNetworkResourceInDifferentTenant bool
		useNetworkResourceInDifferentSub    bool
	}{
		{
			description:                         "resource id should contain NetworkResourceSubscriptionID when using network resources in different tenant and subscription",
			useNetworkResourceInDifferentTenant: true,
			useNetworkResourceInDifferentSub:    true,
		},
		{
			description:                         "resource id should contain NetworkResourceSubscriptionID when using network resources in different subscription",
			useNetworkResourceInDifferentTenant: false,
			useNetworkResourceInDifferentSub:    true,
		},
		{
			description:                         "resource id should contain SubscriptionID when not using network resources in different subscription",
			useNetworkResourceInDifferentTenant: false,
			useNetworkResourceInDifferentSub:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.description, func(t *testing.T) {
			subscriptionID := az.SubscriptionID
			rgName := "rgName"
			lbName := "lbName"
			subResourceName := "subResourceName"
			az.ResourceGroup = rgName
			if c.useNetworkResourceInDifferentTenant {
				az.NetworkResourceTenantID = networkResourceTenantID
			} else {
				az.NetworkResourceTenantID = ""
			}
			if c.useNetworkResourceInDifferentSub {
				az.NetworkResourceSubscriptionID = networkResourceSubscriptionID
				subscriptionID = networkResourceSubscriptionID
			} else {
				az.NetworkResourceSubscriptionID = ""
			}
			expected := fmt.Sprintf(
				expectedResourceIDTemplate,
				subscriptionID,
				rgName,
				lbName,
				subResourceName)
			subResourceID := getLoadBalancerSubResourceID(lbName, subResourceName)
			assert.Equal(t, expected, subResourceID)
			subResourceIDWithRG := getLoadBalancerSubResourceIDWithRG(lbName, rgName, subResourceName)
			assert.Equal(t, expected, subResourceIDWithRG)
		})
	}
}

func TestGetBackendPoolIDs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	testGetLoadBalancerSubResourceIDs(t, az, az.getBackendPoolIDs, consts.BackendPoolIDTemplate)
}

func testGetLoadBalancerSubResourceIDs(
	t *testing.T,
	az *Cloud,
	getLoadBalancerSubResourceIDs func(string, string) map[bool]string,
	expectedResourceIDTemplate string) {
	clusterName := "azure"
	rgName := "rgName"

	cases := []struct {
		description                         string
		loadBalancerName                    string
		resourceGroupName                   string
		useNetworkResourceInDifferentTenant bool
		useNetworkResourceInDifferentSub    bool
	}{
		{
			description:                         "resource id should contain NetworkResourceSubscriptionID when using network resources in different tenant and subscription",
			loadBalancerName:                    "lbName",
			useNetworkResourceInDifferentTenant: true,
			useNetworkResourceInDifferentSub:    true,
		},
		{
			description:                         "resource id should contain NetworkResourceSubscriptionID when using network resources in different subscription",
			loadBalancerName:                    "lbName",
			useNetworkResourceInDifferentTenant: false,
			useNetworkResourceInDifferentSub:    true,
		},
		{
			description:                         "resource id should contain SubscriptionID when not using network resources in different subscription",
			loadBalancerName:                    "lbName",
			useNetworkResourceInDifferentTenant: false,
			useNetworkResourceInDifferentSub:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.description, func(t *testing.T) {
			az.ResourceGroup = rgName
			subscriptionID := az.SubscriptionID
			if c.useNetworkResourceInDifferentTenant {
				az.NetworkResourceTenantID = networkResourceTenantID
			} else {
				az.NetworkResourceTenantID = ""
			}
			if c.useNetworkResourceInDifferentSub {
				az.NetworkResourceSubscriptionID = networkResourceSubscriptionID
				subscriptionID = networkResourceSubscriptionID
			} else {
				az.NetworkResourceSubscriptionID = ""
			}
			expectedV4 := fmt.Sprintf(
				expectedResourceIDTemplate,
				subscriptionID,
				rgName,
				c.loadBalancerName,
				clusterName)
			expectedV6 := fmt.Sprintf(
				expectedResourceIDTemplate,
				subscriptionID,
				rgName,
				c.loadBalancerName,
				clusterName) + "-" + consts.IPVersionIPv6String
			subResourceIDs := getLoadBalancerSubResourceIDs(clusterName, c.loadBalancerName)
			assert.Equal(t, expectedV4, subResourceIDs[false])
			assert.Equal(t, expectedV6, subResourceIDs[true])
		})
	}
}

func TestGetProtocolsFromKubernetesProtocol(t *testing.T) {
	testcases := []struct {
		Name                       string
		protocol                   v1.Protocol
		expectedTransportProto     network.TransportProtocol
		expectedSecurityGroupProto armnetwork.SecurityRuleProtocol
		expectedProbeProto         network.ProbeProtocol
		nilProbeProto              bool
		expectedErrMsg             error
	}{
		{
			Name:                       "getProtocolsFromKubernetesProtocol should get TCP protocol",
			protocol:                   v1.ProtocolTCP,
			expectedTransportProto:     network.TransportProtocolTCP,
			expectedSecurityGroupProto: armnetwork.SecurityRuleProtocolTCP,
			expectedProbeProto:         network.ProbeProtocolTCP,
		},
		{
			Name:                       "getProtocolsFromKubernetesProtocol should get UDP protocol",
			protocol:                   v1.ProtocolUDP,
			expectedTransportProto:     network.TransportProtocolUDP,
			expectedSecurityGroupProto: armnetwork.SecurityRuleProtocolUDP,
			nilProbeProto:              true,
		},
		{
			Name:                       "getProtocolsFromKubernetesProtocol should get SCTP protocol",
			protocol:                   v1.ProtocolSCTP,
			expectedTransportProto:     network.TransportProtocolAll,
			expectedSecurityGroupProto: armnetwork.SecurityRuleProtocolAsterisk,
			nilProbeProto:              true,
		},
		{
			Name:           "getProtocolsFromKubernetesProtocol should report error",
			protocol:       v1.Protocol("ICMP"),
			expectedErrMsg: fmt.Errorf("only TCP, UDP and SCTP are supported for Azure LoadBalancers"),
		},
	}

	for _, test := range testcases {
		transportProto, securityGroupProto, probeProto, err := getProtocolsFromKubernetesProtocol(test.protocol)
		assert.Equal(t, test.expectedTransportProto, *transportProto, test.Name)
		assert.Equal(t, test.expectedSecurityGroupProto, securityGroupProto, test.Name)
		if test.nilProbeProto {
			assert.Nil(t, probeProto, test.Name)
		} else {
			assert.Equal(t, test.expectedProbeProto, *probeProto, test.Name)
		}
		assert.Equal(t, test.expectedErrMsg, err, test.Name)
	}
}

func TestGetStandardVMPrimaryInterfaceID(t *testing.T) {
	testcases := []struct {
		name           string
		vm             compute.VirtualMachine
		expectedNicID  string
		expectedErrMsg error
	}{
		{
			name:          "GetPrimaryInterfaceID should get the only NIC ID",
			vm:            buildDefaultTestVirtualMachine("", []string{"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic"}),
			expectedNicID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic",
		},
		{
			name: "GetPrimaryInterfaceID should get primary NIC ID",
			vm: compute.VirtualMachine{
				Name: ptr.To("vm2"),
				VirtualMachineProperties: &compute.VirtualMachineProperties{
					NetworkProfile: &compute.NetworkProfile{
						NetworkInterfaces: &[]compute.NetworkInterfaceReference{
							{
								NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
									Primary: ptr.To(true),
								},
								ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic1"),
							},
							{
								NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
									Primary: ptr.To(false),
								},
								ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic2"),
							},
						},
					},
				},
			},
			expectedNicID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic1",
		},
		{
			name: "GetPrimaryInterfaceID should report error if node don't have primary NIC",
			vm: compute.VirtualMachine{
				Name: ptr.To("vm3"),
				VirtualMachineProperties: &compute.VirtualMachineProperties{
					NetworkProfile: &compute.NetworkProfile{
						NetworkInterfaces: &[]compute.NetworkInterfaceReference{
							{
								NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
									Primary: ptr.To(false),
								},
								ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic1"),
							},
							{
								NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
									Primary: ptr.To(false),
								},
								ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic2"),
							},
						},
					},
				},
			},
			expectedErrMsg: fmt.Errorf("failed to find a primary nic for the vm. vmname=%q", "vm3"),
		},
	}

	for _, test := range testcases {
		primaryNicID, err := getPrimaryInterfaceID(test.vm)
		assert.Equal(t, test.expectedNicID, primaryNicID, test.name)
		assert.Equal(t, test.expectedErrMsg, err, test.name)
	}
}

func TestGetPrimaryIPConfig(t *testing.T) {
	testcases := []struct {
		name             string
		nic              network.Interface
		expectedIPConfig *network.InterfaceIPConfiguration
		expectedErrMsg   error
	}{

		{
			name: "GetPrimaryIPConfig should get the only IP configuration",
			nic: network.Interface{
				Name: ptr.To("nic"),
				InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
					IPConfigurations: &[]network.InterfaceIPConfiguration{
						{
							Name: ptr.To("ipconfig1"),
						},
					},
				},
			},
			expectedIPConfig: &network.InterfaceIPConfiguration{
				Name: ptr.To("ipconfig1"),
			},
		},
		{
			name: "GetPrimaryIPConfig should get the primary IP configuration",
			nic: network.Interface{
				Name: ptr.To("nic"),
				InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
					IPConfigurations: &[]network.InterfaceIPConfiguration{
						{
							Name: ptr.To("ipconfig1"),
							InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
								Primary: ptr.To(true),
							},
						},
						{
							Name: ptr.To("ipconfig2"),
							InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
								Primary: ptr.To(false),
							},
						},
					},
				},
			},
			expectedIPConfig: &network.InterfaceIPConfiguration{
				Name: ptr.To("ipconfig1"),
				InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
					Primary: ptr.To(true),
				},
			},
		},
		{
			name: "GetPrimaryIPConfig should report error if nic don't have IP configuration",
			nic: network.Interface{
				Name:                      ptr.To("nic"),
				InterfacePropertiesFormat: &network.InterfacePropertiesFormat{},
			},
			expectedErrMsg: fmt.Errorf("nic.IPConfigurations for nic (nicname=%q) is nil", "nic"),
		},
		{
			name: "GetPrimaryIPConfig should report error if node has more than one IP configuration and don't have primary IP configuration",
			nic: network.Interface{
				Name: ptr.To("nic"),
				InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
					IPConfigurations: &[]network.InterfaceIPConfiguration{
						{
							Name: ptr.To("ipconfig1"),
							InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
								Primary: ptr.To(false),
							},
						},
						{
							Name: ptr.To("ipconfig2"),
							InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
								Primary: ptr.To(false),
							},
						},
					},
				},
			},
			expectedErrMsg: fmt.Errorf("failed to determine the primary ipconfig. nicname=%q", "nic"),
		},
	}

	for _, test := range testcases {
		primaryIPConfig, err := getPrimaryIPConfig(test.nic)
		assert.Equal(t, test.expectedIPConfig, primaryIPConfig, test.name)
		assert.Equal(t, test.expectedErrMsg, err, test.name)
	}
}

func TestGetIPConfigByIPFamily(t *testing.T) {
	ipv4IPconfig := network.InterfaceIPConfiguration{
		Name: ptr.To("ipconfig1"),
		InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
			PrivateIPAddressVersion: network.IPv4,
			PrivateIPAddress:        ptr.To("10.10.0.12"),
		},
	}
	ipv6IPconfig := network.InterfaceIPConfiguration{
		Name: ptr.To("ipconfig2"),
		InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
			PrivateIPAddressVersion: network.IPv6,
			PrivateIPAddress:        ptr.To("1111:11111:00:00:1111:1111:000:111"),
		},
	}
	testNic := network.Interface{
		Name: ptr.To("nic"),
		InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
			IPConfigurations: &[]network.InterfaceIPConfiguration{ipv4IPconfig, ipv6IPconfig},
		},
	}
	testcases := []struct {
		name             string
		nic              network.Interface
		expectedIPConfig *network.InterfaceIPConfiguration
		IPv6             bool
		expectedErrMsg   error
	}{
		{
			name:             "GetIPConfigByIPFamily should get the IPv6 IP configuration if IPv6 is false",
			nic:              testNic,
			expectedIPConfig: &ipv4IPconfig,
		},
		{
			name:             "GetIPConfigByIPFamily should get the IPv4 IP configuration if IPv6 is true",
			nic:              testNic,
			IPv6:             true,
			expectedIPConfig: &ipv6IPconfig,
		},
		{
			name: "GetIPConfigByIPFamily should report error if nic don't have IP configuration",
			nic: network.Interface{
				Name:                      ptr.To("nic"),
				InterfacePropertiesFormat: &network.InterfacePropertiesFormat{},
			},
			expectedErrMsg: fmt.Errorf("nic.IPConfigurations for nic (nicname=%q) is nil", "nic"),
		},
		{
			name: "GetIPConfigByIPFamily should report error if nic don't have IPv6 configuration when IPv6 is true",
			nic: network.Interface{
				Name: ptr.To("nic"),
				InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
					IPConfigurations: &[]network.InterfaceIPConfiguration{ipv4IPconfig},
				},
			},
			IPv6:           true,
			expectedErrMsg: fmt.Errorf("failed to determine the ipconfig(IPv6=%v). nicname=%q", true, "nic"),
		},
		{
			name: "GetIPConfigByIPFamily should report error if nic don't have PrivateIPAddress",
			nic: network.Interface{
				Name: ptr.To("nic"),
				InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
					IPConfigurations: &[]network.InterfaceIPConfiguration{
						{
							Name: ptr.To("ipconfig1"),
							InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
								PrivateIPAddressVersion: network.IPv4,
							},
						},
					},
				},
			},
			expectedErrMsg: fmt.Errorf("failed to determine the ipconfig(IPv6=%v). nicname=%q", false, "nic"),
		},
	}

	for _, test := range testcases {
		ipConfig, err := getIPConfigByIPFamily(test.nic, test.IPv6)
		assert.Equal(t, test.expectedIPConfig, ipConfig, test.name)
		assert.Equal(t, test.expectedErrMsg, err, test.name)
	}
}

func TestGetBackendPoolName(t *testing.T) {
	testcases := []struct {
		name             string
		service          v1.Service
		clusterName      string
		expectedPoolName string
		isIPv6           bool
	}{
		{
			name:             "GetBackendPoolName should return <clusterName>-IPv6",
			service:          getTestService("test1", v1.ProtocolTCP, nil, true, 80),
			clusterName:      "azure",
			expectedPoolName: "azure-IPv6",
			isIPv6:           true,
		},
		{
			name:             "GetBackendPoolName should return <clusterName>",
			service:          getTestService("test1", v1.ProtocolTCP, nil, false, 80),
			clusterName:      "azure",
			expectedPoolName: "azure",
			isIPv6:           false,
		},
	}
	for _, test := range testcases {
		backPoolName := getBackendPoolName(test.clusterName, test.isIPv6)
		assert.Equal(t, test.expectedPoolName, backPoolName, test.name)
	}
}

func TestGetBackendPoolNames(t *testing.T) {
	testcases := []struct {
		name              string
		service           v1.Service
		clusterName       string
		expectedPoolNames map[bool]string
	}{
		{
			name:              "GetBackendPoolNames should return 2 backend pool names",
			service:           getTestService("test1", v1.ProtocolTCP, nil, true, 80),
			clusterName:       "azure",
			expectedPoolNames: map[bool]string{consts.IPVersionIPv4: "azure", consts.IPVersionIPv6: "azure-IPv6"},
		},
	}
	for _, test := range testcases {
		backPoolNames := getBackendPoolNames(test.clusterName)
		assert.Equal(t, test.expectedPoolNames, backPoolNames)
	}
}

func TestIsBackendPoolIPv6(t *testing.T) {
	testcases := []struct {
		name           string
		expectedIsIPv6 bool
	}{
		{"bp-IPv6", true},
		{"bp-IPv4", false},
		{"bp", false},
		{"bp-ipv6", true},
	}
	for _, test := range testcases {
		isIPv6 := isBackendPoolIPv6(test.name)
		assert.Equal(t, test.expectedIsIPv6, isIPv6)

		isIPv6ManagedResource := managedResourceHasIPv6Suffix(test.name)
		assert.Equal(t, test.expectedIsIPv6, isIPv6ManagedResource)
	}
}

func TestGetStandardInstanceIDByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)

	expectedVM := compute.VirtualMachine{
		Name: ptr.To("vm1"),
		ID:   ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"),
	}
	invalidResouceID := "/subscriptions/subscription/resourceGroups/rg/Microsoft.Compute/virtualMachines/vm4"
	testcases := []struct {
		name           string
		nodeName       string
		expectedID     string
		expectedErrMsg error
	}{
		{
			name:       "GetInstanceIDByNodeName should get instanceID as expected",
			nodeName:   "vm1",
			expectedID: "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
		},
		{
			name:           "GetInstanceIDByNodeName should report error if node don't exist",
			nodeName:       "vm2",
			expectedErrMsg: fmt.Errorf("instance not found"),
		},
		{
			name:           "GetInstanceIDByNodeName should report error if Error encountered when invoke mockVMClient.Get",
			nodeName:       "vm3",
			expectedErrMsg: fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 500, RawError: VMGet error"),
		},
		{
			name:           "GetInstanceIDByNodeName should report error if ResourceID is invalid",
			nodeName:       "vm4",
			expectedErrMsg: fmt.Errorf("%q isn't in Azure resource ID format %q", invalidResouceID, azureResourceGroupNameRE.String()),
		},
	}
	for _, test := range testcases {
		mockVMClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "vm1", gomock.Any()).Return(expectedVM, nil).AnyTimes()
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "vm2", gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "vm3", gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{
			HTTPStatusCode: http.StatusInternalServerError,
			RawError:       fmt.Errorf("VMGet error"),
		}).AnyTimes()
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "vm4", gomock.Any()).Return(compute.VirtualMachine{
			Name: ptr.To("vm4"),
			ID:   ptr.To(invalidResouceID),
		}, nil).AnyTimes()

		instanceID, err := cloud.VMSet.GetInstanceIDByNodeName(context.Background(), test.nodeName)
		if test.expectedErrMsg != nil {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), test.name)
		}
		assert.Equal(t, test.expectedID, instanceID, test.name)
	}
}

func TestGetStandardVMPowerStatusByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)

	testcases := []struct {
		name           string
		nodeName       string
		vm             compute.VirtualMachine
		expectedStatus string
		getErr         *retry.Error
		expectedErrMsg error
	}{
		{
			name:     "GetPowerStatusByNodeName should report error if node don't exist",
			nodeName: "vm1",
			vm:       compute.VirtualMachine{},
			getErr: &retry.Error{
				HTTPStatusCode: http.StatusNotFound,
				RawError:       cloudprovider.InstanceNotFound,
			},
			expectedErrMsg: fmt.Errorf("instance not found"),
		},
		{
			name:     "GetPowerStatusByNodeName should get power status as expected",
			nodeName: "vm2",
			vm: compute.VirtualMachine{
				Name: ptr.To("vm2"),
				VirtualMachineProperties: &compute.VirtualMachineProperties{
					ProvisioningState: ptr.To("Succeeded"),
					InstanceView: &compute.VirtualMachineInstanceView{
						Statuses: &[]compute.InstanceViewStatus{
							{
								Code: ptr.To("PowerState/Running"),
							},
						},
					},
				},
			},
			expectedStatus: "Running",
		},
		{
			name:     "GetPowerStatusByNodeName should get vmPowerStateUnknown if vm.InstanceView is nil",
			nodeName: "vm3",
			vm: compute.VirtualMachine{
				Name: ptr.To("vm3"),
				VirtualMachineProperties: &compute.VirtualMachineProperties{
					ProvisioningState: ptr.To("Succeeded"),
				},
			},
			expectedStatus: consts.VMPowerStateUnknown,
		},
		{
			name:     "GetPowerStatusByNodeName should get vmPowerStateUnknown if vm.InstanceView.statuses is nil",
			nodeName: "vm4",
			vm: compute.VirtualMachine{
				Name: ptr.To("vm4"),
				VirtualMachineProperties: &compute.VirtualMachineProperties{
					ProvisioningState: ptr.To("Succeeded"),
					InstanceView:      &compute.VirtualMachineInstanceView{},
				},
			},
			expectedStatus: consts.VMPowerStateUnknown,
		},
	}
	for _, test := range testcases {
		mockVMClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, test.nodeName, gomock.Any()).Return(test.vm, test.getErr).AnyTimes()

		powerState, err := cloud.VMSet.GetPowerStatusByNodeName(context.TODO(), test.nodeName)
		assert.Equal(t, test.expectedErrMsg, err, test.name)
		assert.Equal(t, test.expectedStatus, powerState, test.name)
	}
}

func TestGetStandardVMProvisioningStateByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)

	testcases := []struct {
		name                      string
		nodeName                  string
		vm                        compute.VirtualMachine
		expectedProvisioningState string
		getErr                    *retry.Error
		expectedErrMsg            error
	}{
		{
			name:     "GetProvisioningStateByNodeName should report error if node don't exist",
			nodeName: "vm1",
			vm:       compute.VirtualMachine{},
			getErr: &retry.Error{
				HTTPStatusCode: http.StatusNotFound,
				RawError:       cloudprovider.InstanceNotFound,
			},
			expectedErrMsg: fmt.Errorf("instance not found"),
		},
		{
			name:     "GetProvisioningStateByNodeName should return Succeeded for running VM",
			nodeName: "vm2",
			vm: compute.VirtualMachine{
				Name: ptr.To("vm2"),
				VirtualMachineProperties: &compute.VirtualMachineProperties{
					ProvisioningState: ptr.To("Succeeded"),
					InstanceView: &compute.VirtualMachineInstanceView{
						Statuses: &[]compute.InstanceViewStatus{
							{
								Code: ptr.To("PowerState/Running"),
							},
						},
					},
				},
			},
			expectedProvisioningState: "Succeeded",
		},
		{
			name:     "GetProvisioningStateByNodeName should return empty string when vm.ProvisioningState is nil",
			nodeName: "vm3",
			vm: compute.VirtualMachine{
				Name: ptr.To("vm3"),
				VirtualMachineProperties: &compute.VirtualMachineProperties{
					ProvisioningState: nil,
				},
			},
			expectedProvisioningState: "",
		},
	}
	for _, test := range testcases {
		mockVMClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, test.nodeName, gomock.Any()).Return(test.vm, test.getErr).AnyTimes()

		provisioningState, err := cloud.VMSet.GetProvisioningStateByNodeName(context.TODO(), test.nodeName)
		assert.Equal(t, test.expectedErrMsg, err, test.name)
		assert.Equal(t, test.expectedProvisioningState, provisioningState, test.name)
	}
}

func TestGetStandardVMZoneByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)

	var faultDomain int32 = 3
	testcases := []struct {
		name           string
		nodeName       string
		vm             compute.VirtualMachine
		expectedZone   cloudprovider.Zone
		getErr         *retry.Error
		expectedErrMsg error
	}{
		{
			name:     "GetZoneByNodeName should report error if node don't exist",
			nodeName: "vm1",
			vm:       compute.VirtualMachine{},
			getErr: &retry.Error{
				HTTPStatusCode: http.StatusNotFound,
				RawError:       cloudprovider.InstanceNotFound,
			},
			expectedErrMsg: fmt.Errorf("instance not found"),
		},
		{
			name:     "GetZoneByNodeName should get zone as expected",
			nodeName: "vm2",
			vm: compute.VirtualMachine{
				Name:     ptr.To("vm2"),
				Location: ptr.To("EASTUS"),
				Zones:    &[]string{"2"},
				VirtualMachineProperties: &compute.VirtualMachineProperties{
					InstanceView: &compute.VirtualMachineInstanceView{
						PlatformFaultDomain: &faultDomain,
					},
				},
			},
			expectedZone: cloudprovider.Zone{
				FailureDomain: "eastus-2",
				Region:        "eastus",
			},
		},
		{
			name:     "GetZoneByNodeName should get FailureDomain as zone if zone is not used for node",
			nodeName: "vm3",
			vm: compute.VirtualMachine{
				Name:     ptr.To("vm3"),
				Location: ptr.To("EASTUS"),
				VirtualMachineProperties: &compute.VirtualMachineProperties{
					InstanceView: &compute.VirtualMachineInstanceView{
						PlatformFaultDomain: &faultDomain,
					},
				},
			},
			expectedZone: cloudprovider.Zone{
				FailureDomain: "3",
				Region:        "eastus",
			},
		},
		{
			name:     "GetZoneByNodeName should report error if zones is invalid",
			nodeName: "vm4",
			vm: compute.VirtualMachine{
				Name:     ptr.To("vm4"),
				Location: ptr.To("EASTUS"),
				Zones:    &[]string{"a"},
				VirtualMachineProperties: &compute.VirtualMachineProperties{
					InstanceView: &compute.VirtualMachineInstanceView{
						PlatformFaultDomain: &faultDomain,
					},
				},
			},
			expectedErrMsg: fmt.Errorf("failed to parse zone %q: strconv.Atoi: parsing %q: invalid syntax", []string{"a"}, "a"),
		},
	}
	for _, test := range testcases {
		mockVMClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, test.nodeName, gomock.Any()).Return(test.vm, test.getErr).AnyTimes()

		zone, err := cloud.VMSet.GetZoneByNodeName(context.TODO(), test.nodeName)
		if test.expectedErrMsg != nil {
			assert.EqualError(t, test.expectedErrMsg, err.Error(), test.name)
		}
		assert.Equal(t, test.expectedZone, zone, test.name)
	}
}

func TestGetStandardVMSetNames(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testVM := compute.VirtualMachine{
		Name: ptr.To("vm1"),
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			AvailabilitySet: &compute.SubResource{ID: ptr.To(asID)},
		},
	}
	testVMWithoutAS := compute.VirtualMachine{
		Name:                     ptr.To("vm2"),
		VirtualMachineProperties: &compute.VirtualMachineProperties{},
	}
	testCases := []struct {
		name               string
		vm                 []compute.VirtualMachine
		service            *v1.Service
		nodes              []*v1.Node
		usingSingleSLBS    bool
		expectedVMSetNames *[]string
		expectedErrMsg     error
	}{
		{
			name:               "GetVMSetNames should return the primary vm set name if the service has no mode annotation",
			vm:                 []compute.VirtualMachine{testVM},
			service:            &v1.Service{},
			expectedVMSetNames: &[]string{"as"},
		},
		{
			name: "GetVMSetNames should return the primary vm set name when using the single SLB",
			vm:   []compute.VirtualMachine{testVM},
			service: &v1.Service{
				ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: consts.ServiceAnnotationLoadBalancerAutoModeValue}},
			},
			usingSingleSLBS:    true,
			expectedVMSetNames: &[]string{"as"},
		},
		{
			name: "GetVMSetNames should return the correct as names if the service has auto mode annotation",
			vm:   []compute.VirtualMachine{testVM},
			service: &v1.Service{
				ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: consts.ServiceAnnotationLoadBalancerAutoModeValue}},
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: meta.ObjectMeta{
						Name: "vm1",
					},
				},
			},
			expectedVMSetNames: &[]string{"myavailabilityset"},
		},
		{
			name: "GetVMSetNames should return the correct as names if node don't have availability set",
			vm:   []compute.VirtualMachine{testVMWithoutAS},
			service: &v1.Service{
				ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: consts.ServiceAnnotationLoadBalancerAutoModeValue}},
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: meta.ObjectMeta{
						Name: "vm2",
					},
				},
			},
			expectedErrMsg: errors.New("no availability sets found for nodes, node count(1)"),
		},
		{
			name: "GetVMSetNames should report the error if there's no such availability set",
			vm:   []compute.VirtualMachine{testVM},
			service: &v1.Service{
				ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: "vm2"}},
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: meta.ObjectMeta{
						Name: "vm1",
					},
				},
			},
			expectedErrMsg: fmt.Errorf("availability set (vm2) - not found"),
		},
		{
			name: "GetVMSetNames should return the correct node name",
			vm:   []compute.VirtualMachine{testVM},
			service: &v1.Service{
				ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: "myAvailabilitySet"}},
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: meta.ObjectMeta{
						Name: "vm1",
					},
				},
			},
			expectedVMSetNames: &[]string{"myAvailabilitySet"},
		},
	}

	for _, test := range testCases {
		cloud := GetTestCloud(ctrl)
		if test.usingSingleSLBS {
			cloud.LoadBalancerSku = consts.LoadBalancerSkuStandard
		}
		mockVMClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), cloud.ResourceGroup).Return(test.vm, nil).AnyTimes()

		vmSetNames, err := cloud.VMSet.GetVMSetNames(context.TODO(), test.service, test.nodes)
		assert.Equal(t, test.expectedErrMsg, err, test.name)
		assert.Equal(t, test.expectedVMSetNames, vmSetNames, test.name)
	}
}

func TestExtractResourceGroupByNicID(t *testing.T) {
	testCases := []struct {
		name           string
		nicID          string
		expectedRG     string
		expectedErrMsg error
	}{
		{
			name:       "ExtractResourceGroupByNicID should return correct resource group",
			nicID:      "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic",
			expectedRG: "rg",
		},
		{
			name:           "ExtractResourceGroupByNicID should report error if nicID is invalid",
			nicID:          "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/networkInterfaces/nic",
			expectedErrMsg: fmt.Errorf("error of extracting resourceGroup from nicID %q", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/networkInterfaces/nic"),
		},
	}

	for _, test := range testCases {
		rgName, err := extractResourceGroupByNicID(test.nicID)
		assert.Equal(t, test.expectedErrMsg, err, test.name)
		assert.Equal(t, test.expectedRG, rgName, test.name)
	}
}

func TestStandardEnsureHostInPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)

	availabilitySetID := asID
	backendAddressPoolID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1-internal/backendAddressPools/backendpool-1"

	testCases := []struct {
		name              string
		service           *v1.Service
		nodeName          types.NodeName
		backendPoolID     string
		nicName           string
		nicID             string
		vmSetName         string
		nicProvisionState network.ProvisioningState
		isStandardLB      bool
		expectedErrMsg    error
	}{
		{
			name:      "EnsureHostInPool should return nil if node is not in VMSet",
			service:   &v1.Service{},
			nodeName:  "vm1",
			nicName:   "nic1",
			nicID:     "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic1",
			vmSetName: "availabilityset-1",
		},
		{
			name:           "EnsureHostInPool should report error if last segment of nicID is nil",
			service:        &v1.Service{},
			nodeName:       "vm2",
			nicName:        "nic2",
			nicID:          "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/",
			vmSetName:      "availabilityset-1",
			expectedErrMsg: fmt.Errorf("resource name was missing from identifier"),
		},
		{
			name:              "EnsureHostInPool should return nil if node's provisioning state is Failed",
			service:           &v1.Service{},
			nodeName:          "vm3",
			nicName:           "nic3",
			nicID:             "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic3",
			nicProvisionState: consts.NicFailedState,
			vmSetName:         "myAvailabilitySet",
		},
		{
			name:          "EnsureHostInPool should return nil if there is matched backend pool",
			service:       &v1.Service{},
			backendPoolID: backendAddressPoolID,
			nodeName:      "vm5",
			nicName:       "nic5",
			nicID:         "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic5",
			vmSetName:     "myAvailabilitySet",
		},
		{
			name:          "EnsureHostInPool should return nil if there isn't matched backend pool",
			service:       &v1.Service{},
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1-internal/backendAddressPools/backendpool-2",
			nodeName:      "vm6",
			nicName:       "nic6",
			nicID:         "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic6",
			vmSetName:     "myAvailabilitySet",
		},
		{
			name:          "EnsureHostInPool should return nil if BackendPool is not on same LB",
			service:       &v1.Service{},
			isStandardLB:  true,
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb2-internal/backendAddressPools/backendpool-3",
			nodeName:      "vm7",
			nicName:       "nic7",
			nicID:         "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic7",
			vmSetName:     "myAvailabilitySet",
		},
		{
			name:           "EnsureHostInPool should report error if the format of backendPoolID is invalid",
			service:        &v1.Service{},
			isStandardLB:   true,
			backendPoolID:  "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb2-internal/backendAddressPool/backendpool-3",
			nodeName:       "vm8",
			nicName:        "nic8",
			nicID:          "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic7",
			vmSetName:      "myAvailabilitySet",
			expectedErrMsg: fmt.Errorf("new backendPoolID %q is in wrong format", "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb2-internal/backendAddressPool/backendpool-3"),
		},
	}

	for _, test := range testCases {
		if test.isStandardLB {
			cloud.Config.LoadBalancerSku = consts.LoadBalancerSkuStandard
		}

		testVM := buildDefaultTestVirtualMachine(availabilitySetID, []string{test.nicID})
		testVM.Name = ptr.To(string(test.nodeName))
		testNIC := buildDefaultTestInterface(false, []string{backendAddressPoolID})
		testNIC.Name = ptr.To(test.nicName)
		testNIC.ID = ptr.To(test.nicID)
		testNIC.ProvisioningState = test.nicProvisionState

		mockVMClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, string(test.nodeName), gomock.Any()).Return(testVM, nil).AnyTimes()

		mockInterfaceClient := cloud.InterfacesClient.(*mockinterfaceclient.MockInterface)
		mockInterfaceClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, test.nicName, gomock.Any()).Return(testNIC, nil).AnyTimes()
		mockInterfaceClient.EXPECT().CreateOrUpdate(gomock.Any(), cloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

		_, _, _, vm, err := cloud.VMSet.EnsureHostInPool(context.Background(), test.service, test.nodeName, test.backendPoolID, test.vmSetName)
		assert.Equal(t, test.expectedErrMsg, err, test.name)
		assert.Nil(t, vm, test.name)
	}
}

func TestStandardEnsureHostsInPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)

	availabilitySetID := asID
	backendAddressPoolID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1-internal/backendAddressPools/backendpool-1"

	testCases := []struct {
		name           string
		service        *v1.Service
		nodes          []*v1.Node
		excludeLBNodes []string
		nodeName       string
		backendPoolID  string
		nicName        string
		nicID          string
		vmSetName      string
		expectedErr    bool
		expectedErrMsg error
	}{
		{
			name:     "EnsureHostsInPool should return nil if there's no error when invoke EnsureHostInPool",
			service:  &v1.Service{},
			nodeName: "vm1",
			nodes: []*v1.Node{
				{
					ObjectMeta: meta.ObjectMeta{
						Name: "vm1",
					},
				},
			},
			nicName:       "nic1",
			nicID:         "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic1",
			backendPoolID: backendAddressPoolID,
			vmSetName:     "availabilityset-1",
		},
		{
			name:           "EnsureHostsInPool should skip if node is master node",
			service:        &v1.Service{},
			nodeName:       "vm2",
			excludeLBNodes: []string{"vm2"},
			nodes: []*v1.Node{
				{
					ObjectMeta: meta.ObjectMeta{
						Name:   "vm2",
						Labels: map[string]string{consts.NodeLabelRole: "master"},
					},
				},
			},
			nicName:   "nic2",
			nicID:     "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/",
			vmSetName: "availabilityset-1",
		},
		{
			name:           "EnsureHostsInPool should skip if node is in external resource group",
			service:        &v1.Service{},
			nodeName:       "vm3",
			excludeLBNodes: []string{"vm3"},
			nodes: []*v1.Node{
				{
					ObjectMeta: meta.ObjectMeta{
						Name:   "vm3",
						Labels: map[string]string{consts.ExternalResourceGroupLabel: "rg-external"},
					},
				},
			},
			nicName:   "nic3",
			nicID:     "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic3",
			vmSetName: "availabilityset-1",
		},
		{
			name:           "EnsureHostsInPool should skip if node is unmanaged",
			service:        &v1.Service{},
			nodeName:       "vm4",
			excludeLBNodes: []string{"vm4"},
			nodes: []*v1.Node{
				{
					ObjectMeta: meta.ObjectMeta{
						Name:   "vm4",
						Labels: map[string]string{consts.ManagedByAzureLabel: "false"},
					},
				},
			},

			nicName:   "nic4",
			nicID:     "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic4",
			vmSetName: "availabilityset-1",
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			cloud.Config.LoadBalancerSku = consts.LoadBalancerSkuStandard
			cloud.Config.ExcludeMasterFromStandardLB = ptr.To(true)
			cloud.excludeLoadBalancerNodes = utilsets.NewString(test.excludeLBNodes...)

			testVM := buildDefaultTestVirtualMachine(availabilitySetID, []string{test.nicID})
			testNIC := buildDefaultTestInterface(false, []string{backendAddressPoolID})
			testNIC.Name = ptr.To(test.nicName)
			testNIC.ID = ptr.To(test.nicID)

			mockVMClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
			mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, test.nodeName, gomock.Any()).Return(testVM, nil).AnyTimes()

			mockInterfaceClient := cloud.InterfacesClient.(*mockinterfaceclient.MockInterface)
			mockInterfaceClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, test.nicName, gomock.Any()).Return(testNIC, nil).AnyTimes()
			mockInterfaceClient.EXPECT().CreateOrUpdate(gomock.Any(), cloud.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

			err := cloud.VMSet.EnsureHostsInPool(context.Background(), test.service, test.nodes, test.backendPoolID, test.vmSetName)
			if test.expectedErr {
				assert.EqualError(t, test.expectedErrMsg, err.Error(), test.name)
			} else {
				assert.Nil(t, err, test.name)
			}
		})
	}
}

func TestStandardEnsureBackendPoolDeleted(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)
	service := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	backendPoolID := "backendPoolID"
	vmSetName := "AS"

	tests := []struct {
		desc                string
		backendAddressPools *[]network.BackendAddressPool
		loadBalancerSKU     string
		existingVM          compute.VirtualMachine
		existingNIC         network.Interface
	}{
		{
			desc: "EnsureBackendPoolDeleted should decouple the nic and the load balancer properly",
			backendAddressPools: &[]network.BackendAddressPool{
				{
					ID: ptr.To(backendPoolID),
					BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
						BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
							{
								ID: ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1"),
							},
						},
					},
				},
			},
			existingVM: buildDefaultTestVirtualMachine("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/availabilitySets/as", []string{
				"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1",
			}),
			existingNIC: buildDefaultTestInterface(true, []string{"/subscriptions/sub/resourceGroups/gh/providers/Microsoft.Network/loadBalancers/testCluster/backendAddressPools/testCluster"}),
		},
	}

	for _, test := range tests {
		cloud.LoadBalancerSku = test.loadBalancerSKU
		mockVMClient := mockvmclient.NewMockInterface(ctrl)
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "k8s-agentpool1-00000000-1", gomock.Any()).Return(test.existingVM, nil)
		cloud.VirtualMachinesClient = mockVMClient
		mockNICClient := mockinterfaceclient.NewMockInterface(ctrl)
		test.existingNIC.VirtualMachine = &network.SubResource{
			ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/k8s-agentpool1-00000000-1"),
		}
		mockNICClient.EXPECT().Get(gomock.Any(), "rg", "k8s-agentpool1-00000000-nic-1", gomock.Any()).Return(test.existingNIC, nil).Times(2)
		mockNICClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
		cloud.InterfacesClient = mockNICClient

		nicUpdated, err := cloud.VMSet.EnsureBackendPoolDeleted(context.TODO(), &service, []string{backendPoolID}, vmSetName, test.backendAddressPools, true)
		assert.NoError(t, err, test.desc)
		assert.True(t, nicUpdated)
	}
}

func buildDefaultTestInterface(isPrimary bool, lbBackendpoolIDs []string) network.Interface {
	expectedNIC := network.Interface{
		InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
			ProvisioningState: network.ProvisioningStateSucceeded,
			IPConfigurations: &[]network.InterfaceIPConfiguration{
				{
					InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
						Primary: ptr.To(isPrimary),
					},
				},
			},
		},
	}
	backendAddressPool := make([]network.BackendAddressPool, 0)
	for _, id := range lbBackendpoolIDs {
		backendAddressPool = append(backendAddressPool, network.BackendAddressPool{
			ID: ptr.To(id),
		})
	}
	(*expectedNIC.IPConfigurations)[0].LoadBalancerBackendAddressPools = &backendAddressPool
	return expectedNIC
}

func buildDefaultTestVirtualMachine(asID string, nicIDs []string) compute.VirtualMachine {
	expectedVM := compute.VirtualMachine{
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			AvailabilitySet: &compute.SubResource{
				ID: ptr.To(asID),
			},
			NetworkProfile: &compute.NetworkProfile{},
		},
	}
	networkInterfaces := make([]compute.NetworkInterfaceReference, 0)
	for _, nicID := range nicIDs {
		networkInterfaces = append(networkInterfaces, compute.NetworkInterfaceReference{
			ID: ptr.To(nicID),
		})
	}
	expectedVM.VirtualMachineProperties.NetworkProfile.NetworkInterfaces = &networkInterfaces
	return expectedVM
}

func TestStandardGetNodeNameByIPConfigurationID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)
	expectedVM := buildDefaultTestVirtualMachine("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/availabilitySets/AGENTPOOL1-AVAILABILITYSET-00000000", []string{})
	expectedVM.Name = ptr.To("name")
	mockVMClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
	mockVMClient.EXPECT().Get(gomock.Any(), "rg", "k8s-agentpool1-00000000-0", gomock.Any()).Return(expectedVM, nil)
	expectedNIC := buildDefaultTestInterface(true, []string{})
	expectedNIC.VirtualMachine = &network.SubResource{
		ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/k8s-agentpool1-00000000-0"),
	}
	mockNICClient := cloud.InterfacesClient.(*mockinterfaceclient.MockInterface)
	mockNICClient.EXPECT().Get(gomock.Any(), "rg", "k8s-agentpool1-00000000-nic-0", gomock.Any()).Return(expectedNIC, nil)
	ipConfigurationID := `/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-0/ipConfigurations/ipconfig1`
	nodeName, asName, err := cloud.VMSet.GetNodeNameByIPConfigurationID(context.TODO(), ipConfigurationID)
	assert.NoError(t, err)
	assert.Equal(t, "k8s-agentpool1-00000000-0", nodeName)
	assert.Equal(t, "agentpool1-availabilityset-00000000", asName)
}

func TestGetAvailabilitySetByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description   string
		nodeName      string
		vmasVMIDs     []string
		vmasListError *retry.Error
		expectedErr   error
	}{
		{
			description: "getAvailabilitySetByNodeName should return the correct VMAS",
			nodeName:    "vm-1",
			vmasVMIDs:   []string{"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-1"},
		},
		{
			description: "getAvailabilitySetByNodeName should return cloudprovider.InstanceNotFound if there's no matching VMAS",
			nodeName:    "vm-2",
			vmasVMIDs:   []string{"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-1"},
			expectedErr: cloudprovider.InstanceNotFound,
		},
		{
			description:   "getAvailabilitySetByNodeName should report an error if there's something wrong during an api call",
			nodeName:      "vm-1",
			vmasVMIDs:     []string{"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-1"},
			vmasListError: &retry.Error{RawError: fmt.Errorf("error during vmas list")},
			expectedErr:   fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error during vmas list"),
		},
		{
			description: "getAvailabilitySetByNodeName should report an error if the vmID on the vmas is invalid",
			nodeName:    "vm-1",
			vmasVMIDs:   []string{"invalid"},
			expectedErr: fmt.Errorf("invalid vm ID invalid"),
		},
	}

	for _, test := range testCases {
		cloud := GetTestCloud(ctrl)
		vmSet, err := newAvailabilitySet(cloud)
		assert.NoError(t, err)
		as := vmSet.(*availabilitySet)

		mockVMASClient := mockvmasclient.NewMockInterface(ctrl)
		cloud.AvailabilitySetsClient = mockVMASClient

		subResources := make([]compute.SubResource, 0)
		for _, vmID := range test.vmasVMIDs {
			subResources = append(subResources, compute.SubResource{
				ID: ptr.To(vmID),
			})
		}
		expected := compute.AvailabilitySet{
			Name: ptr.To("vmas-1"),
			AvailabilitySetProperties: &compute.AvailabilitySetProperties{
				VirtualMachines: &subResources,
			},
		}
		mockVMASClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.AvailabilitySet{expected}, test.vmasListError).AnyTimes()

		actual, err := as.getAvailabilitySetByNodeName(context.TODO(), test.nodeName, azcache.CacheReadTypeDefault)
		if test.expectedErr != nil {
			assert.EqualError(t, test.expectedErr, err.Error(), test.description)
		}
		if actual != nil {
			assert.Equal(t, expected, *actual, test.description)
		}
	}
}

func TestGetNodeCIDRMasksByProviderIDAvailabilitySet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description, providerID                    string
		tags                                       map[string]*string
		expectedIPV4MaskSize, expectedIPV6MaskSize int
		expectedErr                                error
	}{
		{
			description: "GetNodeCIDRMasksByProviderID should report an error if the providerID is not valid",
			providerID:  "invalid",
			expectedErr: errors.New("error splitting providerID"),
		},
		{
			description: "GetNodeCIDRMaksByProviderID should return the correct mask sizes",
			providerID:  "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-0",
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("24"),
				consts.VMSetCIDRIPV6TagKey: ptr.To("64"),
			},
			expectedIPV4MaskSize: 24,
			expectedIPV6MaskSize: 64,
		},
		{
			description:          "GetNodeCIDRMaksByProviderID should report cloudprovider.InstanceNotFound if there is no matching vmas",
			providerID:           "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-1",
			expectedIPV4MaskSize: 24,
			expectedIPV6MaskSize: 64,
		},
		{
			description: "GetNodeCIDRMaksByProviderID should return the correct mask sizes even if some of the tags are not specified",
			providerID:  "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-0",
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("24"),
			},
			expectedIPV4MaskSize: 24,
		},
		{
			description: "GetNodeCIDRMaksByProviderID should not fail even if some of the tag is invalid",
			providerID:  "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-0",
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("abc"),
				consts.VMSetCIDRIPV6TagKey: ptr.To("64"),
			},
			expectedIPV6MaskSize: 64,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			vmSet, err := newAvailabilitySet(cloud)
			assert.NoError(t, err)
			as := vmSet.(*availabilitySet)

			mockVMASClient := mockvmasclient.NewMockInterface(ctrl)
			cloud.AvailabilitySetsClient = mockVMASClient

			expected := compute.AvailabilitySet{
				Name: ptr.To("vmas-1"),
				AvailabilitySetProperties: &compute.AvailabilitySetProperties{
					VirtualMachines: &[]compute.SubResource{
						{ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-0")},
					},
				},
				Tags: tc.tags,
			}
			mockVMASClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.AvailabilitySet{expected}, nil).AnyTimes()

			ipv4MaskSize, ipv6MaskSize, err := as.GetNodeCIDRMasksByProviderID(context.TODO(), tc.providerID)
			assert.Equal(t, tc.expectedErr, err)
			assert.Equal(t, tc.expectedIPV4MaskSize, ipv4MaskSize)
			assert.Equal(t, tc.expectedIPV6MaskSize, ipv6MaskSize)
		})
	}
}

func TestGetAvailabilitySetNameByID(t *testing.T) {
	t.Run("getAvailabilitySetNameByID should return empty string if the given ID is empty", func(t *testing.T) {
		vmasName, err := getAvailabilitySetNameByID("")
		assert.Nil(t, err)
		assert.Empty(t, vmasName)
	})

	t.Run("getAvailabilitySetNameByID should report an error if the format of the given ID is wrong", func(t *testing.T) {
		asID := "illegal-id"
		vmasName, err := getAvailabilitySetNameByID(asID)
		assert.Equal(t, fmt.Errorf("getAvailabilitySetNameByID: failed to parse the VMAS ID illegal-id"), err)
		assert.Empty(t, vmasName)
	})

	t.Run("getAvailabilitySetNameByID should extract the VMAS name from the given ID", func(t *testing.T) {
		asID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/availabilitySets/as"
		vmasName, err := getAvailabilitySetNameByID(asID)
		assert.Nil(t, err)
		assert.Equal(t, "as", vmasName)
	})
}

func TestGetNodeVMSetName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		description       string
		node              *v1.Node
		listTimes         int
		expectedVMs       []compute.VirtualMachine
		listErr           *retry.Error
		expectedVMSetName string
		expectedErr       error
	}{
		{
			description: "GetNodeVMSetName should return early if there is no host name in the node",
			node:        &v1.Node{},
		},
		{
			description: "GetNodeVMSetName should report an error if failed to list vms",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{
						{
							Type:    v1.NodeHostName,
							Address: "vm",
						},
					},
				},
			},
			listTimes:   1,
			listErr:     retry.NewError(false, errors.New("error")),
			expectedErr: retry.NewError(false, errors.New("error")).Error(),
		},
		{
			description: "GetNodeVMSetName should report an error if the availability set ID of the vm is not legal",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{
						{
							Type:    v1.NodeHostName,
							Address: "vm",
						},
					},
				},
			},
			expectedVMs: []compute.VirtualMachine{
				{
					Name: ptr.To("vm"),
					VirtualMachineProperties: &compute.VirtualMachineProperties{
						AvailabilitySet: &compute.SubResource{
							ID: ptr.To("/"),
						},
					},
				},
			},
			listTimes:   1,
			expectedErr: errors.New("resource name was missing from identifier"),
		},
		{
			description: "GetNodeVMSetName should return the availability set name in the vm",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{
						{
							Type:    v1.NodeHostName,
							Address: "vm",
						},
					},
				},
			},
			expectedVMs: []compute.VirtualMachine{
				{
					Name: ptr.To("vm"),
					VirtualMachineProperties: &compute.VirtualMachineProperties{
						AvailabilitySet: &compute.SubResource{
							ID: ptr.To("as"),
						},
					},
				},
			},
			listTimes:         1,
			expectedVMSetName: "as",
		},
	} {
		az := GetTestCloud(ctrl)
		vmClient := mockvmclient.NewMockInterface(ctrl)
		vmClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(tc.expectedVMs, tc.listErr).Times(tc.listTimes)
		az.VirtualMachinesClient = vmClient

		vmSet, err := newAvailabilitySet(az)
		assert.NoError(t, err)
		as := vmSet.(*availabilitySet)

		vmSetName, err := as.GetNodeVMSetName(context.TODO(), tc.node)
		assert.Equal(t, tc.expectedErr, err, tc.description)
		assert.Equal(t, tc.expectedVMSetName, vmSetName, tc.description)
	}
}

func TestGetSecurityRuleName(t *testing.T) {
	testcases := []struct {
		desc             string
		svc              *v1.Service
		port             v1.ServicePort
		sourceAddrPrefix string
		isIPv6           bool
		expectedRuleName string
	}{
		{
			"IPv4",
			&v1.Service{
				ObjectMeta: meta.ObjectMeta{
					UID: "257b9655-5137-4ad2-b091-ef3f07043ad3",
				},
			},
			v1.ServicePort{
				Protocol: v1.ProtocolTCP,
				Port:     80,
			},
			"10.0.0.1/24",
			false,
			"a257b965551374ad2b091ef3f07043ad-TCP-80-10.0.0.1_24",
		},
		{
			"IPv4-shared",
			&v1.Service{
				ObjectMeta: meta.ObjectMeta{
					UID:         "257b9655-5137-4ad2-b091-ef3f07043ad3",
					Annotations: map[string]string{consts.ServiceAnnotationSharedSecurityRule: "true"},
				},
			},
			v1.ServicePort{
				Protocol: v1.ProtocolTCP,
				Port:     80,
			},
			"10.0.0.1/24",
			false,
			"shared-TCP-80-10.0.0.1_24",
		},
		{
			"IPv6",
			&v1.Service{
				ObjectMeta: meta.ObjectMeta{
					UID: "257b9655-5137-4ad2-b091-ef3f07043ad3",
				},
			},
			v1.ServicePort{
				Protocol: v1.ProtocolTCP,
				Port:     80,
			},
			"2001:0:0::1/64",
			true,
			"a257b965551374ad2b091ef3f07043ad-TCP-80-2001.0.0..1_64",
		},
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			ruleName := az.getSecurityRuleName(tc.svc, tc.port, tc.sourceAddrPrefix, tc.isIPv6)
			assert.Equal(t, tc.expectedRuleName, ruleName)
		})
	}
}

func TestGetPublicIPName(t *testing.T) {
	testcases := []struct {
		desc            string
		svc             *v1.Service
		pips            []network.PublicIPAddress
		isIPv6          bool
		expectedPIPName string
	}{
		{
			desc: "common",
			svc: &v1.Service{
				ObjectMeta: meta.ObjectMeta{UID: types.UID("uid")},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
				},
			},
			isIPv6:          false,
			expectedPIPName: "azure-auid",
		},
		{
			desc: "Service PIP prefix id",
			svc: &v1.Service{
				ObjectMeta: meta.ObjectMeta{
					UID: types.UID("uid"),
					Annotations: map[string]string{
						consts.ServiceAnnotationPIPPrefixIDDualStack[false]: "prefix-id",
					},
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
				},
			},
			isIPv6:          false,
			expectedPIPName: "azure-auid-prefix-id",
		},
		{
			desc: "Service PIP prefix id dualstack IPv4",
			svc: &v1.Service{
				ObjectMeta: meta.ObjectMeta{
					UID: types.UID("uid"),
					Annotations: map[string]string{
						consts.ServiceAnnotationPIPPrefixIDDualStack[false]: "prefix-id",
						consts.ServiceAnnotationPIPPrefixIDDualStack[true]:  "prefix-id-ipv6",
					},
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
			isIPv6:          false,
			expectedPIPName: "azure-auid-prefix-id",
		},
		{
			desc: "Service PIP prefix id dualstack lengthy IPv6",
			svc: &v1.Service{
				ObjectMeta: meta.ObjectMeta{
					UID: types.UID("uid"),
					Annotations: map[string]string{
						consts.ServiceAnnotationPIPPrefixIDDualStack[false]: "prefix-id",
						consts.ServiceAnnotationPIPPrefixIDDualStack[true]:  "prefix-id-ipv6-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					},
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
			isIPv6:          true,
			expectedPIPName: "azure-auid-prefix-id-ipv6-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-IPv6",
		},
		{
			desc: "Service PIP IPv6 only with existing PIP",
			svc: &v1.Service{
				ObjectMeta: meta.ObjectMeta{
					UID: types.UID("uid"),
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv6Protocol},
				},
			},
			isIPv6:          true,
			expectedPIPName: "azure-auid",
		},
		{
			desc: "Service PIP IPv6 only with existing PIP - dualstack",
			svc: &v1.Service{
				ObjectMeta: meta.ObjectMeta{
					UID: types.UID("uid"),
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
			isIPv6:          true,
			expectedPIPName: "azure-auid-IPv6",
		},
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			name, err := az.getPublicIPName("azure", tc.svc, tc.isIPv6)
			assert.Nil(t, err)
			assert.Equal(t, tc.expectedPIPName, name)
		})
	}
}
