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

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/loadbalancerclient/mockloadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func TestEnsureHostsInPoolNodeIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.LoadBalancerSku = consts.LoadBalancerSkuStandard
	bi := newBackendPoolTypeNodeIP(az)

	nodes := []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "master",
				Labels: map[string]string{consts.ControlPlaneNodeRoleLabel: "true"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "vmss-0",
			},
			Status: v1.NodeStatus{
				Addresses: []v1.NodeAddress{
					{
						Type:    v1.NodeInternalIP,
						Address: "10.0.0.2",
					},
					{
						Type:    v1.NodeInternalIP,
						Address: "2001::2",
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "vmss-1",
			},
			Status: v1.NodeStatus{
				Addresses: []v1.NodeAddress{
					{
						Type:    v1.NodeInternalIP,
						Address: "10.0.0.1",
					},
					{
						Type:    v1.NodeInternalIP,
						Address: "2001::1",
					},
				},
			},
		},
	}

	testcases := []struct {
		desc                string
		backendPool         network.BackendAddressPool
		expectedBackendPool network.BackendAddressPool
	}{
		{
			desc: "IPv4",
			backendPool: network.BackendAddressPool{
				Name: pointer.String("kubernetes"),
				BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
					LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{
						{
							LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: pointer.String("10.0.0.1"),
							},
						},
						{
							LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: pointer.String("10.0.0.3"),
							},
						},
					},
				},
			},
			expectedBackendPool: network.BackendAddressPool{
				Name: pointer.String("kubernetes"),
				BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
					VirtualNetwork: &network.SubResource{ID: pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet")},
					LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{
						{
							LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: pointer.String("10.0.0.1"),
							},
						},
						{
							Name: pointer.String("vmss-0"),
							LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: pointer.String("10.0.0.2"),
							},
						},
					},
				},
			},
		},
		{
			desc: "IPv6",
			backendPool: network.BackendAddressPool{
				Name: pointer.String("kubernetes-IPv6"),
				BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
					LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{
						{
							LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: pointer.String("2001::1"),
							},
						},
					},
				},
			},
			expectedBackendPool: network.BackendAddressPool{
				Name: pointer.String("kubernetes-IPv6"),
				BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
					VirtualNetwork: &network.SubResource{ID: pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet")},
					LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{
						{
							LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: pointer.String("2001::1"),
							},
						},
						{
							Name: pointer.String("vmss-0-IPv6"),
							LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
								IPAddress: pointer.String("2001::2"),
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			lbClient := mockloadbalancerclient.NewMockInterface(ctrl)
			lbClient.EXPECT().CreateOrUpdateBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
			az.LoadBalancerClient = lbClient

			service := getTestServiceDualStack("svc-1", v1.ProtocolTCP, nil, 80)
			err := bi.EnsureHostsInPool(&service, nodes, "", "", "kubernetes", "kubernetes", tc.backendPool)
			assert.NoError(t, err)
			assert.Equal(t, tc.expectedBackendPool, tc.backendPool)
		})
	}
}

func TestIsLBBackendPoolsExisting(t *testing.T) {
	testcases := []struct {
		desc               string
		lbBackendPoolNames map[bool]string
		bpName             *string
		expectedFound      bool
		expectedIsIPv6     bool
	}{
		{
			desc: "IPv4 backendpool exists",
			lbBackendPoolNames: map[bool]string{
				false: "bp",
				true:  "bp-IPv6",
			},
			bpName:         pointer.String("bp"),
			expectedFound:  true,
			expectedIsIPv6: false,
		},
		{
			desc: "IPv6 backendpool exists",
			lbBackendPoolNames: map[bool]string{
				false: "bp",
				true:  "bp-IPv6",
			},
			bpName:         pointer.String("bp-IPv6"),
			expectedFound:  true,
			expectedIsIPv6: true,
		},
		{
			desc: "backendpool not exists",
			lbBackendPoolNames: map[bool]string{
				false: "bp",
				true:  "bp-IPv6",
			},
			bpName:         pointer.String("bp0"),
			expectedFound:  false,
			expectedIsIPv6: false,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			found, isIPv6 := isLBBackendPoolsExisting(tc.lbBackendPoolNames, tc.bpName)
			assert.Equal(t, tc.expectedFound, found)
			assert.Equal(t, tc.expectedIsIPv6, isIPv6)
		})
	}
}

func TestCleanupVMSetFromBackendPoolByConditionNodeIPConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)
	cloud.LoadBalancerSku = consts.LoadBalancerSkuStandard
	cloud.EnableMultipleStandardLoadBalancers = true
	service := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	lb := buildDefaultTestLB("testCluster", []string{
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1",
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1",
	})

	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1").Return("", "agentpool1-availabilitySet-00000000", nil)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1").Return("", "agentpool2-availabilitySet-00000000", nil)
	mockVMSet.EXPECT().GetPrimaryVMSetName().Return("agentpool1-availabilitySet-00000000").AnyTimes()
	cloud.VMSet = mockVMSet

	expectedLB := network.LoadBalancer{
		Name: pointer.String("testCluster"),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
			BackendAddressPools: &[]network.BackendAddressPool{
				{
					Name: pointer.String("testCluster"),
					BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
						BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
							{
								ID: pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1"),
							},
						},
					},
				},
			},
		},
	}

	mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
	mockLBClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedLB, nil)
	cloud.LoadBalancerClient = mockLBClient

	bc := newBackendPoolTypeNodeIPConfig(cloud)

	shouldRemoveVMSetFromSLB := func(vmSetName string) bool {
		return !strings.EqualFold(vmSetName, cloud.VMSet.GetPrimaryVMSetName()) && vmSetName != ""
	}
	cleanedLB, err := bc.CleanupVMSetFromBackendPoolByCondition(&lb, &service, nil, testClusterName, shouldRemoveVMSetFromSLB)
	assert.NoError(t, err)
	assert.Equal(t, expectedLB, *cleanedLB)
}

func TestCleanupVMSetFromBackendPoolByConditionNodeIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)
	cloud.LoadBalancerSku = consts.LoadBalancerSkuStandard
	cloud.LoadBalancerBackendPoolConfigurationType = consts.LoadBalancerBackendPoolConfigurationTypeNodeIP
	service := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	clusterName := "testCluster"

	lb := buildLBWithVMIPs("testCluster", []string{"10.0.0.1", "10.0.0.2"})
	expectedLB := buildLBWithVMIPs("testCluster", []string{"10.0.0.2"})

	nodes := []*v1.Node{
		{
			Status: v1.NodeStatus{
				Addresses: []v1.NodeAddress{
					{
						Type:    v1.NodeInternalIP,
						Address: "10.0.0.1",
					},
				},
			},
		},
	}

	lbClient := mockloadbalancerclient.NewMockInterface(ctrl)
	lbClient.EXPECT().CreateOrUpdateBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	cloud.LoadBalancerClient = lbClient

	bi := newBackendPoolTypeNodeIP(cloud)

	shouldRemoveVMSetFromSLB := func(_ string) bool {
		return true
	}

	cleanedLB, err := bi.CleanupVMSetFromBackendPoolByCondition(lb, &service, nodes, clusterName, shouldRemoveVMSetFromSLB)
	assert.NoError(t, err)
	assert.Equal(t, expectedLB, cleanedLB)
}

func TestCleanupVMSetFromBackendPoolForInstanceNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)
	cloud.LoadBalancerSku = consts.LoadBalancerSkuStandard
	cloud.EnableMultipleStandardLoadBalancers = true
	cloud.PrimaryAvailabilitySetName = "agentpool1-availabilitySet-00000000"
	clusterName := "testCluster"
	service := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	lb := buildDefaultTestLB("testCluster", []string{
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1",
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1",
	})

	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(false, nil)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1").Return("", "agentpool1-availabilitySet-00000000", nil)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1").Return("", "agentpool2-availabilitySet-00000000", nil)
	mockVMSet.EXPECT().GetPrimaryVMSetName().Return("agentpool1-availabilitySet-00000000").AnyTimes()
	cloud.VMSet = mockVMSet

	expectedLB := network.LoadBalancer{
		Name: pointer.String("testCluster"),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
			BackendAddressPools: &[]network.BackendAddressPool{
				{
					Name: pointer.String("testCluster"),
					BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
						BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
							{
								ID: pointer.String("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1"),
							},
						},
					},
				},
			},
		},
	}

	bc := newBackendPoolTypeNodeIPConfig(cloud)

	shouldRemoveVMSetFromSLB := func(vmSetName string) bool {
		return !strings.EqualFold(vmSetName, cloud.VMSet.GetPrimaryVMSetName()) && vmSetName != ""
	}
	cleanedLB, err := bc.CleanupVMSetFromBackendPoolByCondition(&lb, &service, nil, clusterName, shouldRemoveVMSetFromSLB)
	assert.NoError(t, err)
	assert.Equal(t, expectedLB, *cleanedLB)
}

func TestReconcileBackendPoolsNodeIPConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildDefaultTestLB(testClusterName, []string{
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1",
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1",
	})

	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1").Return("k8s-agentpool1-00000000", "", nil)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1").Return("k8s-agentpool2-00000000", "", nil)
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil)
	mockVMSet.EXPECT().GetPrimaryVMSetName().Return("k8s-agentpool1-00000000")

	mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
	mockLBClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(network.LoadBalancer{}, nil)

	az := GetTestCloud(ctrl)
	az.VMSet = mockVMSet
	az.LoadBalancerClient = mockLBClient
	az.nodeInformerSynced = func() bool { return true }
	az.excludeLoadBalancerNodes = utilsets.NewString("k8s-agentpool1-00000000")

	bc := newBackendPoolTypeNodeIPConfig(az)
	svc := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	_, _, _, err := bc.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.NoError(t, err)

	lb = network.LoadBalancer{
		Name:                         pointer.String(testClusterName),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{},
	}
	az = GetTestCloud(ctrl)
	az.PreConfiguredBackendPoolLoadBalancerTypes = consts.PreConfiguredBackendPoolLoadBalancerTypesAll
	bc = newBackendPoolTypeNodeIPConfig(az)
	preConfigured, changed, updatedLB, err := bc.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.NoError(t, err)
	assert.False(t, preConfigured)
	assert.Equal(t, lb, *updatedLB)
	assert.True(t, changed)
}

func TestReconcileBackendPoolsNodeIPConfigRemoveIPConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildDefaultTestLB(testClusterName, []string{
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1",
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1",
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool3-00000000-nic-1/ipConfigurations/ipconfig1",
	})

	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1").Return("k8s-agentpool1-00000000", "", nil)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1").Return("k8s-agentpool2-00000000", "", nil)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool3-00000000-nic-1/ipConfigurations/ipconfig1").Return("", "", cloudprovider.InstanceNotFound)
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(false, nil)
	mockVMSet.EXPECT().GetPrimaryVMSetName().Return("k8s-agentpool1-00000000").Times(2)

	az := GetTestCloud(ctrl)
	az.VMSet = mockVMSet
	az.nodeInformerSynced = func() bool { return true }
	az.excludeLoadBalancerNodes = utilsets.NewString("k8s-agentpool1-00000000")

	bc := newBackendPoolTypeNodeIPConfig(az)
	svc := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	_, _, updatedLB, err := bc.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.NoError(t, err)
	assert.Equal(t, lb, *updatedLB)

	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1").Return("k8s-agentpool1-00000000", "", errors.New("error"))
	_, _, _, err = bc.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.Equal(t, "error", err.Error())
}

func TestReconcileBackendPoolsNodeIPConfigPreConfigured(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildDefaultTestLB(testClusterName, []string{
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1",
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1",
	})

	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID(gomock.Any()).Times(0)
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	mockVMSet.EXPECT().GetPrimaryVMSetName().Return("k8s-agentpool1-00000000")

	az := GetTestCloud(ctrl)
	az.VMSet = mockVMSet
	az.PreConfiguredBackendPoolLoadBalancerTypes = consts.PreConfiguredBackendPoolLoadBalancerTypesAll

	svc := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	bc := newBackendPoolTypeNodeIPConfig(az)
	preConfigured, changed, updatedLB, err := bc.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.True(t, preConfigured)
	assert.False(t, changed)
	assert.Equal(t, lb, *updatedLB)
	assert.NoError(t, err)
}

func TestReconcileBackendPoolsNodeIPToIPConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildLBWithVMIPs(testClusterName, []string{"10.0.0.1", "10.0.0.2"})
	mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
	mockLBClient.EXPECT().CreateOrUpdateBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(retry.NewError(false, fmt.Errorf("create or update LB backend pool error")))
	mockLBClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(network.LoadBalancer{}, nil)

	az := GetTestCloud(ctrl)
	az.LoadBalancerClient = mockLBClient

	bc := newBackendPoolTypeNodeIPConfig(az)
	svc := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	_, _, _, err := bc.ReconcileBackendPools(testClusterName, &svc, lb)
	assert.Contains(t, err.Error(), "create or update LB backend pool error")

	lb = buildLBWithVMIPs(testClusterName, []string{"10.0.0.1", "10.0.0.2"})
	mockLBClient.EXPECT().CreateOrUpdateBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	_, _, updatedLB, err := bc.ReconcileBackendPools(testClusterName, &svc, lb)
	assert.NoError(t, err)
	assert.Equal(t, network.LoadBalancer{}, *updatedLB)
	assert.Empty(t, (*lb.BackendAddressPools)[0].LoadBalancerBackendAddresses)
}

func TestReconcileBackendPoolsNodeIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildLBWithVMIPs("kubernetes", []string{"10.0.0.1", "10.0.0.2"})
	nodes := []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "vmss-0",
			},
			Status: v1.NodeStatus{
				Addresses: []v1.NodeAddress{
					{
						Type:    v1.NodeInternalIP,
						Address: "10.0.0.1",
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "vmss-1",
			},
			Status: v1.NodeStatus{
				Addresses: []v1.NodeAddress{
					{
						Type:    v1.NodeInternalIP,
						Address: "10.0.0.2",
					},
				},
			},
		},
	}

	bp := network.BackendAddressPool{
		Name: pointer.String("kubernetes"),
		BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
			VirtualNetwork: &network.SubResource{
				ID: pointer.String("vnet"),
			},
			LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{
				{
					LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
						IPAddress: pointer.String("10.0.0.2"),
					},
				},
			},
		},
	}

	az := GetTestCloud(ctrl)
	az.LoadBalancerBackendPoolConfigurationType = consts.LoadBalancerBackendPoolConfigurationTypeNodeIP
	az.KubeClient = fake.NewSimpleClientset(nodes[0], nodes[1])
	az.excludeLoadBalancerNodes = utilsets.NewString("vmss-0")
	az.nodePrivateIPs["vmss-0"] = utilsets.NewString("10.0.0.1")

	lbClient := mockloadbalancerclient.NewMockInterface(ctrl)
	lbClient.EXPECT().CreateOrUpdateBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), bp, gomock.Any()).Return(nil)
	lbClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(network.LoadBalancer{}, nil)
	az.LoadBalancerClient = lbClient

	bi := newBackendPoolTypeNodeIP(az)

	service := getTestService("test", v1.ProtocolTCP, nil, false, 80)

	_, _, updatedLB, err := bi.ReconcileBackendPools("kubernetes", &service, lb)
	assert.Equal(t, network.LoadBalancer{}, *updatedLB)
	assert.NoError(t, err)

	lb = &network.LoadBalancer{
		Name:                         pointer.String(testClusterName),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{},
	}
	az = GetTestCloud(ctrl)
	az.PreConfiguredBackendPoolLoadBalancerTypes = consts.PreConfiguredBackendPoolLoadBalancerTypesAll
	bi = newBackendPoolTypeNodeIP(az)
	preConfigured, changed, updatedLB, err := bi.ReconcileBackendPools(testClusterName, &service, lb)
	assert.NoError(t, err)
	assert.False(t, preConfigured)
	assert.Equal(t, lb, updatedLB)
	assert.True(t, changed)
}

func TestReconcileBackendPoolsNodeIPEmptyPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildLBWithVMIPs("kubernetes", []string{})

	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil)
	mockVMSet.EXPECT().GetPrimaryVMSetName().Return("k8s-agentpool1-00000000")

	mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
	mockLBClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(network.LoadBalancer{}, nil)

	az := GetTestCloud(ctrl)
	az.LoadBalancerBackendPoolConfigurationType = consts.LoadBalancerBackendPoolConfigurationTypeNodeIP
	az.VMSet = mockVMSet
	az.LoadBalancerClient = mockLBClient
	bi := newBackendPoolTypeNodeIP(az)

	service := getTestService("test", v1.ProtocolTCP, nil, false, 80)

	_, _, updatedLB, err := bi.ReconcileBackendPools("kubernetes", &service, lb)
	assert.Equal(t, network.LoadBalancer{}, *updatedLB)
	assert.NoError(t, err)
}

func TestReconcileBackendPoolsNodeIPPreConfigured(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildLBWithVMIPs("kubernetes", []string{"10.0.0.1", "10.0.0.2"})
	az := GetTestCloud(ctrl)
	az.PreConfiguredBackendPoolLoadBalancerTypes = consts.PreConfiguredBackendPoolLoadBalancerTypesAll

	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID(gomock.Any()).Times(0)
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	mockVMSet.EXPECT().GetPrimaryVMSetName().Return("k8s-agentpool1-00000000").AnyTimes()
	az.VMSet = mockVMSet

	service := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	bi := newBackendPoolTypeNodeIP(az)
	preConfigured, changed, updatedLB, err := bi.ReconcileBackendPools("kubernetes", &service, lb)
	assert.True(t, preConfigured)
	assert.False(t, changed)
	assert.Equal(t, lb, updatedLB)
	assert.NoError(t, err)
}

func TestReconcileBackendPoolsNodeIPConfigToIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildDefaultTestLB(testClusterName, []string{
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1",
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1",
	})
	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(false, fmt.Errorf("delete LB backend pool error"))
	mockVMSet.EXPECT().GetPrimaryVMSetName().Return("k8s-agentpool1-00000000").AnyTimes()

	az := GetTestCloud(ctrl)
	az.VMSet = mockVMSet
	bi := newBackendPoolTypeNodeIP(az)
	svc := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	_, _, _, err := bi.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.Contains(t, err.Error(), "delete LB backend pool error")

	lb = buildDefaultTestLB(testClusterName, []string{
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1",
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1",
	})
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(false, nil)
	mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
	mockLBClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(network.LoadBalancer{}, nil)
	az.LoadBalancerClient = mockLBClient
	_, _, updatedLB, err := bi.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.NoError(t, err)
	assert.Equal(t, network.LoadBalancer{}, *updatedLB)
	assert.Empty(t, (*lb.BackendAddressPools)[0].LoadBalancerBackendAddresses)
}

func TestReconcileBackendPoolsNodeIPConfigToIPWithMigrationAPI(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildDefaultTestLB(testClusterName, []string{
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1",
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1",
	})

	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil)
	mockVMSet.EXPECT().GetPrimaryVMSetName().Return("k8s-agentpool1-00000000").AnyTimes()

	mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
	mockLBClient.EXPECT().MigrateToIPBasedBackendPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(retry.NewError(false, errors.New("error")))

	az := GetTestCloud(ctrl)
	az.VMSet = mockVMSet
	az.LoadBalancerClient = mockLBClient
	az.EnableMigrateToIPBasedBackendPoolAPI = true

	bi := newBackendPoolTypeNodeIP(az)
	svc := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	_, _, _, err := bi.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "error")

	mockLBClient.EXPECT().MigrateToIPBasedBackendPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	bps := buildLBWithVMIPs(testClusterName, []string{"1.2.3.4", "2.3.4.5"}).BackendAddressPools
	mockLBClient.EXPECT().GetLBBackendPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return((*bps)[0], nil)
	mockLBClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(network.LoadBalancer{}, nil)
	_, _, updatedLB, err := bi.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.NoError(t, err)
	assert.Equal(t, network.LoadBalancer{}, *updatedLB)
}

func buildTestLoadBalancerBackendPoolWithIPs(name string, ips []string) network.BackendAddressPool {
	backendPool := network.BackendAddressPool{
		Name: &name,
		BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
			LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{},
		},
	}
	for _, ip := range ips {
		ip := ip
		*backendPool.LoadBalancerBackendAddresses = append(*backendPool.LoadBalancerBackendAddresses, network.LoadBalancerBackendAddress{
			LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
				IPAddress: &ip,
			},
		})
	}

	return backendPool
}

func TestRemoveNodeIPAddressFromBackendPool(t *testing.T) {
	for _, tc := range []struct {
		description                           string
		removeAll                             bool
		unwantedIPs, existingIPs, expectedIPs []string
	}{
		{
			description: "removeNodeIPAddressFromBackendPool should remove the unwanted IP addresses from the backend pool",
			unwantedIPs: []string{"1.2.3.4", "4.3.2.1"},
			existingIPs: []string{"1.2.3.4", "5.6.7.8", "4.3.2.1", ""},
			expectedIPs: []string{"5.6.7.8", ""},
		},
		{
			description: "removeNodeIPAddressFromBackendPool should not make the backend pool empty",
			unwantedIPs: []string{"1.2.3.4", "4.3.2.1"},
			existingIPs: []string{"1.2.3.4", "4.3.2.1"},
			expectedIPs: []string{"1.2.3.4", "4.3.2.1"},
		},
		{
			description: "removeNodeIPAddressFromBackendPool should remove all the IP addresses from the backend pool",
			removeAll:   true,
			unwantedIPs: []string{"1.2.3.4", "4.3.2.1"},
			existingIPs: []string{"1.2.3.4", "4.3.2.1", ""},
			expectedIPs: []string{""},
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			backendPool := buildTestLoadBalancerBackendPoolWithIPs("kubernetes", tc.existingIPs)
			expectedBackendPool := buildTestLoadBalancerBackendPoolWithIPs("kubernetes", tc.expectedIPs)

			removeNodeIPAddressesFromBackendPool(backendPool, tc.unwantedIPs, tc.removeAll)
			assert.Equal(t, expectedBackendPool, backendPool)
		})
	}
}

func TestGetBackendPrivateIPsNodeIPConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildDefaultTestLB(testClusterName, []string{"ipconfig1", "ipconfig2"})
	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("ipconfig1").Return("node1", "", nil)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("ipconfig2").Return("node2", "", nil)

	az := GetTestCloud(ctrl)
	az.nodePrivateIPs = map[string]*utilsets.IgnoreCaseSet{
		"node1": utilsets.NewString("1.2.3.4", "fe80::1"),
	}
	az.VMSet = mockVMSet
	bc := newBackendPoolTypeNodeIPConfig(az)
	svc := getTestService("svc1", "TCP", nil, false)
	ipv4, ipv6 := bc.GetBackendPrivateIPs(testClusterName, &svc, &lb)
	assert.Equal(t, []string{"1.2.3.4"}, ipv4)
	assert.Equal(t, []string{"fe80::1"}, ipv6)
}

func TestGetBackendPrivateIPsNodeIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	svc := getTestService("svc1", "TCP", nil, false) // isIPv6 doesn't matter.
	testcases := []struct {
		desc         string
		lb           *network.LoadBalancer
		expectedIPv4 []string
		expectedIPv6 []string
	}{
		{
			"normal",
			buildLBWithVMIPs(testClusterName, []string{"1.2.3.4", "fe80::1"}),
			[]string{"1.2.3.4"},
			[]string{"fe80::1"},
		},
		{
			"some invalid IPs",
			buildLBWithVMIPs(testClusterName, []string{"1.2.3.4.5", "fe80::1"}),
			[]string{},
			[]string{"fe80::1"},
		},
		{
			"no IPs",
			buildLBWithVMIPs(testClusterName, []string{}),
			[]string{},
			[]string{},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			az.VMSet = NewMockVMSet(ctrl)
			bi := newBackendPoolTypeNodeIP(az)
			ipv4, ipv6 := bi.GetBackendPrivateIPs(testClusterName, &svc, tc.lb)
			assert.Equal(t, tc.expectedIPv4, ipv4)
			assert.Equal(t, tc.expectedIPv6, ipv6)
		})
	}
}

func TestGetBackendIPConfigurationsToBeDeleted(t *testing.T) {
	for _, tc := range []struct {
		description                         string
		bipConfigNotFound, bipConfigExclude []network.InterfaceIPConfiguration
		expected                            map[string]bool
	}{
		{
			description: "should ignore excluded IP configurations if the backend pool will be empty after removing IP configurations of not found vms",
			bipConfigNotFound: []network.InterfaceIPConfiguration{
				{ID: pointer.String("ipconfig1")},
				{ID: pointer.String("ipconfig2")},
			},
			bipConfigExclude: []network.InterfaceIPConfiguration{
				{ID: pointer.String("ipconfig3")},
			},
			expected: map[string]bool{
				"ipconfig1": true,
				"ipconfig2": true,
			},
		},
		{
			description: "should remove both not found and excluded vms",
			bipConfigNotFound: []network.InterfaceIPConfiguration{
				{ID: pointer.String("ipconfig1")},
			},
			bipConfigExclude: []network.InterfaceIPConfiguration{
				{ID: pointer.String("ipconfig3")},
			},
			expected: map[string]bool{
				"ipconfig1": true,
				"ipconfig3": true,
			},
		},
		{
			description: "should remove all not found vms even if the backend pool will be empty",
			bipConfigNotFound: []network.InterfaceIPConfiguration{
				{ID: pointer.String("ipconfig1")},
				{ID: pointer.String("ipconfig2")},
				{ID: pointer.String("ipconfig3")},
			},
			bipConfigExclude: []network.InterfaceIPConfiguration{
				{ID: pointer.String("ipconfig4")},
			},
			expected: map[string]bool{
				"ipconfig1": true,
				"ipconfig2": true,
				"ipconfig3": true,
			},
		},
	} {
		bp := network.BackendAddressPool{
			BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
				BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
					{ID: pointer.String("ipconfig1")},
					{ID: pointer.String("ipconfig2")},
					{ID: pointer.String("ipconfig3")},
				},
			},
		}

		ipConfigs := getBackendIPConfigurationsToBeDeleted(bp, tc.bipConfigNotFound, tc.bipConfigExclude)
		actual := make(map[string]bool)
		for _, ipConfig := range ipConfigs {
			actual[*ipConfig.ID] = true
		}
		assert.Equal(t, tc.expected, actual)
	}
}
