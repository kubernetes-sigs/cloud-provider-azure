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
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/loadbalancerclient/mockloadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestEnsureHostsInPoolNodeIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.LoadBalancerSku = consts.LoadBalancerSkuStandard
	az.EnableMultipleStandardLoadBalancers = true
	bi := newBackendPoolTypeNodeIP(az)

	backendPool := network.BackendAddressPool{
		Name: to.StringPtr("kubernetes"),
		BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
			LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{
				{
					LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
						IPAddress: to.StringPtr("10.0.0.1"),
					},
				},
			},
		},
	}
	expectedBackendPool := network.BackendAddressPool{
		Name: to.StringPtr("kubernetes"),
		BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
			LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{
				{
					LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
						IPAddress: to.StringPtr("10.0.0.1"),
					},
				},
				{
					Name: to.StringPtr("vmss-0"),
					LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
						IPAddress:      to.StringPtr("10.0.0.2"),
						VirtualNetwork: &network.SubResource{ID: to.StringPtr("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet")},
					},
				},
			},
		},
	}

	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().GetNodeVMSetName(gomock.Any()).Return("vmss-0", nil)
	mockVMSet.EXPECT().GetPrimaryVMSetName().Return("vmss-0")
	az.VMSet = mockVMSet

	lbClient := mockloadbalancerclient.NewMockInterface(ctrl)
	lbClient.EXPECT().CreateOrUpdateBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	az.LoadBalancerClient = lbClient

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
				},
			},
		},
	}

	service := getTestService("svc-1", v1.ProtocolTCP, nil, false, 80)
	err := bi.EnsureHostsInPool(&service, nodes, "", "", "kubernetes", "kubernetes", backendPool)
	assert.NoError(t, err)
	assert.Equal(t, expectedBackendPool, backendPool)
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
		Name: to.StringPtr("testCluster"),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
			BackendAddressPools: &[]network.BackendAddressPool{
				{
					Name: to.StringPtr("testCluster"),
					BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
						BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
							{
								ID: to.StringPtr("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1"),
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

	shouldRemoveVMSetFromSLB := func(vmSetName string) bool {
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
		Name: to.StringPtr("testCluster"),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
			BackendAddressPools: &[]network.BackendAddressPool{
				{
					Name: to.StringPtr("testCluster"),
					BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
						BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
							{
								ID: to.StringPtr("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1"),
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
	az.excludeLoadBalancerNodes = sets.NewString("k8s-agentpool1-00000000")

	bc := newBackendPoolTypeNodeIPConfig(az)
	svc := getTestService("test", v1.ProtocolTCP, nil, false, 80)
	_, _, err := bc.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.NoError(t, err)

	lb = network.LoadBalancer{
		Name:                         to.StringPtr(testClusterName),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{},
	}
	az = GetTestCloud(ctrl)
	az.PreConfiguredBackendPoolLoadBalancerTypes = consts.PreConfiguredBackendPoolLoadBalancerTypesAll
	bc = newBackendPoolTypeNodeIPConfig(az)
	preConfigured, changed, err := bc.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.NoError(t, err)
	assert.False(t, preConfigured)
	assert.True(t, changed)
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
	preConfigured, changed, err := bc.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.True(t, preConfigured)
	assert.False(t, changed)
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
	_, _, err := bc.ReconcileBackendPools(testClusterName, &svc, lb)
	assert.Contains(t, err.Error(), "create or update LB backend pool error")

	lb = buildLBWithVMIPs(testClusterName, []string{"10.0.0.1", "10.0.0.2"})
	mockLBClient.EXPECT().CreateOrUpdateBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	_, _, err = bc.ReconcileBackendPools(testClusterName, &svc, lb)
	assert.NoError(t, err)
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

	az := GetTestCloud(ctrl)
	az.LoadBalancerBackendPoolConfigurationType = consts.LoadBalancerBackendPoolConfigurationTypeNodeIP
	az.KubeClient = fake.NewSimpleClientset(nodes[0], nodes[1])
	az.excludeLoadBalancerNodes = sets.NewString("vmss-0")
	az.nodePrivateIPs["vmss-0"] = sets.NewString("10.0.0.1")

	lbClient := mockloadbalancerclient.NewMockInterface(ctrl)
	lbClient.EXPECT().CreateOrUpdateBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)
	lbClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(network.LoadBalancer{}, nil)
	az.LoadBalancerClient = lbClient

	bi := newBackendPoolTypeNodeIP(az)

	service := getTestService("test", v1.ProtocolTCP, nil, false, 80)

	_, _, err := bi.ReconcileBackendPools("kubernetes", &service, lb)
	assert.NoError(t, err)

	lb = &network.LoadBalancer{
		Name:                         to.StringPtr(testClusterName),
		LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{},
	}
	az = GetTestCloud(ctrl)
	az.PreConfiguredBackendPoolLoadBalancerTypes = consts.PreConfiguredBackendPoolLoadBalancerTypesAll
	bi = newBackendPoolTypeNodeIP(az)
	preConfigured, changed, err := bi.ReconcileBackendPools(testClusterName, &service, lb)
	assert.NoError(t, err)
	assert.False(t, preConfigured)
	assert.True(t, changed)
}

func TestReconcileBackendPoolsNodeIPEmptyPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildLBWithVMIPs("kubernetes", []string{})

	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(true, nil)
	mockVMSet.EXPECT().GetPrimaryVMSetName().Return("k8s-agentpool1-00000000")

	lbClient := mockloadbalancerclient.NewMockInterface(ctrl)
	lbClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(network.LoadBalancer{}, nil)

	az := GetTestCloud(ctrl)
	az.LoadBalancerBackendPoolConfigurationType = consts.LoadBalancerBackendPoolConfigurationTypeNodeIP
	az.VMSet = mockVMSet
	az.LoadBalancerClient = lbClient
	bi := newBackendPoolTypeNodeIP(az)

	service := getTestService("test", v1.ProtocolTCP, nil, false, 80)

	_, _, err := bi.ReconcileBackendPools("kubernetes", &service, lb)
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
	preConfigured, changed, err := bi.ReconcileBackendPools("kubernetes", &service, lb)
	assert.True(t, preConfigured)
	assert.False(t, changed)
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
	_, _, err := bi.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.Contains(t, err.Error(), "delete LB backend pool error")

	lb = buildDefaultTestLB(testClusterName, []string{
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1/ipConfigurations/ipconfig1",
		"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool2-00000000-nic-1/ipConfigurations/ipconfig1",
	})
	mockVMSet.EXPECT().EnsureBackendPoolDeleted(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(false, nil)
	_, _, err = bi.ReconcileBackendPools(testClusterName, &svc, &lb)
	assert.NoError(t, err)
	assert.Empty(t, (*lb.BackendAddressPools)[0].LoadBalancerBackendAddresses)
}

func TestRemoveNodeIPAddressFromBackendPool(t *testing.T) {
	nodeIPAddresses := []string{"1.2.3.4", "4.3.2.1"}
	backendPool := network.BackendAddressPool{
		Name: to.StringPtr("kubernetes"),
		BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
			LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{
				{
					LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
						IPAddress: to.StringPtr("1.2.3.4"),
					},
				},
				{
					LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
						IPAddress: to.StringPtr("5.6.7.8"),
					},
				},
				{
					LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
						IPAddress: to.StringPtr("4.3.2.1"),
					},
				},
				{
					LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{},
				},
			},
		},
	}
	expectedBackendPool := network.BackendAddressPool{
		Name: to.StringPtr("kubernetes"),
		BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
			LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{
				{
					LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
						IPAddress: to.StringPtr("5.6.7.8"),
					},
				},
				{
					LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{},
				},
			},
		},
	}

	removeNodeIPAddressesFromBackendPool(backendPool, nodeIPAddresses, false)
	assert.Equal(t, expectedBackendPool, backendPool)
}

func TestGetBackendPrivateIPsNodeIPConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	lb := buildDefaultTestLB(testClusterName, []string{"ipconfig1", "ipconfig2"})
	mockVMSet := NewMockVMSet(ctrl)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("ipconfig1").Return("node1", "", nil)
	mockVMSet.EXPECT().GetNodeNameByIPConfigurationID("ipconfig2").Return("node2", "", nil)

	az := GetTestCloud(ctrl)
	az.nodePrivateIPs = map[string]sets.String{
		"node1": sets.NewString("1.2.3.4", "fe80::1"),
	}
	az.VMSet = mockVMSet
	bc := newBackendPoolTypeNodeIPConfig(az)
	svc := getTestService("svc1", "TCP", nil, false)
	ipv4, ipv6 := bc.GetBackendPrivateIPs(testClusterName, &svc, &lb)
	assert.Equal(t, []string{"1.2.3.4"}, ipv4)
	assert.Equal(t, []string{"fe80::1"}, ipv6)
}
