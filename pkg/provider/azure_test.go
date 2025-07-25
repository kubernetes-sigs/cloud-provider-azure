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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	cloudprovider "k8s.io/cloud-provider"
	cloudproviderapi "k8s.io/cloud-provider/api"
	servicehelpers "k8s.io/cloud-provider/service/helpers"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/configloader"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/interfaceclient/mock_interfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient/mock_loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/securitygroupclient/mock_securitygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/virtualmachineclient/mock_virtualmachineclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/privatelinkservice"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/subnet"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/zone"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/taints"
)

const (
	testClusterName = "testCluster"
)

// Test flipServiceInternalAnnotation
func TestFlipServiceInternalAnnotation(t *testing.T) {
	svc := getTestService("servicea", v1.ProtocolTCP, nil, false, 80)
	svcUpdated := flipServiceInternalAnnotation(&svc)
	if !requiresInternalLoadBalancer(svcUpdated) {
		t.Errorf("Expected svc to be an internal service")
	}
	svcUpdated = flipServiceInternalAnnotation(svcUpdated)
	if requiresInternalLoadBalancer(svcUpdated) {
		t.Errorf("Expected svc to be an external service")
	}

	svc2 := getInternalTestService("serviceb", 8081)
	svc2Updated := flipServiceInternalAnnotation(&svc2)
	if requiresInternalLoadBalancer(svc2Updated) {
		t.Errorf("Expected svc to be an external service")
	}

	svc2Updated = flipServiceInternalAnnotation(svc2Updated)
	if !requiresInternalLoadBalancer(svc2Updated) {
		t.Errorf("Expected svc to be an internal service")
	}
}

// Test additional of a new service/port.
func TestAddPort(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestService("service1", v1.ProtocolTCP, nil, false, 80)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, expectedInterfaces, expectedVirtualMachines, 1)

	svc.Spec.Ports = append(svc.Spec.Ports, v1.ServicePort{
		Name:     fmt.Sprintf("port-udp-%d", 1234),
		Protocol: v1.ProtocolUDP,
		Port:     1234,
		NodePort: getBackendPort(1234),
	})

	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBs(az, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	mockPLSRepo := az.plsRepo.(*privatelinkservice.MockRepository)
	mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()
	lb, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	assert.Nil(t, err)

	// ensure we got a frontend ip configuration
	if len(lb.Properties.FrontendIPConfigurations) != 1 {
		t.Error("Expected the loadbalancer to have a frontend ip configuration")
	}

	validateLoadBalancer(t, az, lb, svc)
}

func setMockEnvDualStack(az *Cloud, expectedInterfaces []*armnetwork.Interface, expectedVirtualMachines []*armcompute.VirtualMachine, serviceCount int, services ...v1.Service) {
	mockInterfacesClient := az.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
	for i := range expectedInterfaces {
		mockInterfacesClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, fmt.Sprintf("vm-%d", i), gomock.Any()).Return(expectedInterfaces[i], nil).AnyTimes()
		mockInterfacesClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, fmt.Sprintf("vm-%d", i), gomock.Any()).Return(nil, nil).AnyTimes()
	}

	vmClient := az.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
	vmClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(expectedVirtualMachines, nil).AnyTimes()
	for i := range expectedVirtualMachines {
		vmClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, fmt.Sprintf("vm-%d", i), gomock.Any()).Return(expectedVirtualMachines[i], nil).AnyTimes()
	}

	setMockPublicIPs(az, serviceCount, true, true)

	sg := getTestSecurityGroupDualStack(az, services...)
	setMockSecurityGroup(az, sg)
}

func setMockEnv(az *Cloud, expectedInterfaces []*armnetwork.Interface, expectedVirtualMachines []*armcompute.VirtualMachine, serviceCount int, services ...v1.Service) {
	mockInterfacesClient := az.ComputeClientFactory.GetInterfaceClient().(*mock_interfaceclient.MockInterface)
	for i := range expectedInterfaces {
		mockInterfacesClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, fmt.Sprintf("vm-%d", i), gomock.Any()).Return(expectedInterfaces[i], nil).AnyTimes()
		mockInterfacesClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, fmt.Sprintf("vm-%d", i), gomock.Any()).Return(nil, nil).AnyTimes()
	}

	vmClient := az.ComputeClientFactory.GetVirtualMachineClient().(*mock_virtualmachineclient.MockInterface)
	vmClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(expectedVirtualMachines, nil).AnyTimes()
	for i := range expectedVirtualMachines {
		vmClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, fmt.Sprintf("vm-%d", i), gomock.Any()).Return(expectedVirtualMachines[i], nil).AnyTimes()
	}

	setMockPublicIPs(az, serviceCount, true, false)

	sg := getTestSecurityGroup(az, services...)
	setMockSecurityGroup(az, sg)
}

func setMockPublicIPs(az *Cloud, serviceCount int, v4Enabled, v6Enabled bool) {
	mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)

	expectedPIPsTotal := []*armnetwork.PublicIPAddress{}
	if v4Enabled {
		expectedPIPs := setMockPublicIP(az, mockPIPsClient, serviceCount, false)
		expectedPIPsTotal = append(expectedPIPsTotal, expectedPIPs...)
	}
	if v6Enabled {
		expectedPIPs := setMockPublicIP(az, mockPIPsClient, serviceCount, true)
		expectedPIPsTotal = append(expectedPIPsTotal, expectedPIPs...)
	}
	mockPIPsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(expectedPIPsTotal, nil).AnyTimes()

	mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mockPIPsClient.EXPECT().List(gomock.Any(), gomock.Not(az.ResourceGroup)).Return(nil, nil).AnyTimes()
	mockPIPsClient.EXPECT().Get(gomock.Any(), gomock.Not(az.ResourceGroup), gomock.Any(), gomock.Any()).Return(&armnetwork.PublicIPAddress{}, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()
}

func setMockPublicIP(az *Cloud, mockPIPsClient *mock_publicipaddressclient.MockInterface, serviceCount int, isIPv6 bool) []*armnetwork.PublicIPAddress {
	suffix := ""
	ipVer := to.Ptr(armnetwork.IPVersionIPv4)
	ipAddr1 := "1.2.3.4"
	ipAddra := "1.2.3.5"
	if isIPv6 {
		suffix = "-" + consts.IPVersionIPv6String
		ipVer = to.Ptr(armnetwork.IPVersionIPv6)
		ipAddr1 = "fd00::eef0"
		ipAddra = "fd00::eef1"
	}

	a := 'a'
	var expectedPIPs []*armnetwork.PublicIPAddress
	for i := 1; i <= serviceCount; i++ {
		expectedPIP := &armnetwork.PublicIPAddress{
			Name:     ptr.To("testCluster-aservicea"),
			Location: &az.Location,
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{
				PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
				PublicIPAddressVersion:   ipVer,
				IPAddress:                ptr.To(ipAddr1),
			},
			Tags: map[string]*string{
				consts.ServiceTagKey:  ptr.To("default/servicea"),
				consts.ClusterNameKey: ptr.To(testClusterName),
			},
			SKU: &armnetwork.PublicIPAddressSKU{
				Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
			},
			ID: ptr.To(fmt.Sprintf("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice%d%s", i, suffix)),
		}
		expectedPIP.Name = ptr.To(fmt.Sprintf("testCluster-aservice%d%s", i, suffix))
		expectedPIP.Properties = &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
			PublicIPAddressVersion:   ipVer,
			IPAddress:                ptr.To(ipAddr1),
		}
		expectedPIP.Tags[consts.ServiceTagKey] = ptr.To(fmt.Sprintf("default/service%d", i))
		mockPIPsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, fmt.Sprintf("testCluster-aservice%d%s", i, suffix), gomock.Any()).Return(expectedPIP, nil).AnyTimes()
		mockPIPsClient.EXPECT().Delete(gomock.Any(), az.ResourceGroup, fmt.Sprintf("testCluster-aservice%d%s", i, suffix)).Return(nil).AnyTimes()
		expectedPIPs = append(expectedPIPs, expectedPIP)
		expectedPIP = &armnetwork.PublicIPAddress{
			Name:     ptr.To(fmt.Sprintf("testCluster-aservice%c%s", a, suffix)),
			Location: &az.Location,
			Properties: &armnetwork.PublicIPAddressPropertiesFormat{
				PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
				PublicIPAddressVersion:   ipVer,
				IPAddress:                ptr.To(ipAddra),
			},
			Tags: map[string]*string{
				consts.ServiceTagKey:  ptr.To("default/servicea"),
				consts.ClusterNameKey: ptr.To(testClusterName),
			},
			SKU: &armnetwork.PublicIPAddressSKU{
				Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
			},
			ID: ptr.To(fmt.Sprintf("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/testCluster-aservice%d%s", i, suffix)),
		}
		expectedPIP.Tags[consts.ServiceTagKey] = ptr.To(fmt.Sprintf("default/service%c", a))
		mockPIPsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, fmt.Sprintf("testCluster-aservice%c%s", a, suffix), gomock.Any()).Return(expectedPIP, nil).AnyTimes()
		mockPIPsClient.EXPECT().Delete(gomock.Any(), az.ResourceGroup, fmt.Sprintf("testCluster-aservice%c%s", a, suffix)).Return(nil).AnyTimes()
		expectedPIPs = append(expectedPIPs, expectedPIP)
		a++
	}
	return expectedPIPs
}

func setMockSecurityGroup(az *Cloud, sgs ...*armnetwork.SecurityGroup) {
	mockSGsClient := az.NetworkClientFactory.GetSecurityGroupClient().(*mock_securitygroupclient.MockInterface)
	for _, sg := range sgs {
		mockSGsClient.EXPECT().Get(gomock.Any(), az.SecurityGroupResourceGroup, az.SecurityGroupName).Return(sg, nil).AnyTimes()
	}
	mockSGsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.SecurityGroupResourceGroup, az.SecurityGroupName, gomock.Any()).Return(nil, nil).AnyTimes()
}

func setMockLBsDualStack(az *Cloud, expectedLBs *[]*armnetwork.LoadBalancer, svcName string, lbCount, serviceIndex int, isInternal bool) string {
	lbIndex := (serviceIndex - 1) % lbCount
	expectedLBName := ""
	if lbIndex == 0 {
		expectedLBName = testClusterName
	} else {
		expectedLBName = fmt.Sprintf("as-%d", lbIndex)
	}
	if isInternal {
		expectedLBName += "-internal"
	}

	fullServiceName := strings.Replace(svcName, "-", "", -1)

	if lbIndex >= len(*expectedLBs) {
		lb := &armnetwork.LoadBalancer{
			Location: &az.Location,
			Properties: &armnetwork.LoadBalancerPropertiesFormat{
				BackendAddressPools: []*armnetwork.BackendAddressPool{
					{
						Name: ptr.To("testCluster"),
					},
					{
						Name: ptr.To("testCluster-IPv6"),
					},
				},
			},
		}
		lb.Name = &expectedLBName
		lb.Properties.LoadBalancingRules = []*armnetwork.LoadBalancingRule{
			{
				Name: ptr.To(fmt.Sprintf("a%s%d-TCP-8081", fullServiceName, serviceIndex)),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					FrontendPort: ptr.To(int32(8081)),
					BackendPort:  ptr.To(int32(8081)),
				},
			},
			{
				Name: ptr.To(fmt.Sprintf("a%s%d-TCP-8081-IPv6", fullServiceName, serviceIndex)),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					FrontendPort: ptr.To(int32(8081)),
					BackendPort:  ptr.To(int32(8081)),
				},
			},
		}
		lb.Properties.Probes = []*armnetwork.Probe{
			{
				Name: ptr.To(fmt.Sprintf("a%s%d-TCP-8081", fullServiceName, serviceIndex)),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port: ptr.To(int32(18081)),
				},
			},
			{
				Name: ptr.To(fmt.Sprintf("a%s%d-TCP-8081-IPv6", fullServiceName, serviceIndex)),
				Properties: &armnetwork.ProbePropertiesFormat{
					Port: ptr.To(int32(18081)),
				},
			},
		}
		fips := []*armnetwork.FrontendIPConfiguration{
			{
				Name: ptr.To(fmt.Sprintf("a%s%d", fullServiceName, serviceIndex)),
				ID:   ptr.To("fip"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					PublicIPAddress:           &armnetwork.PublicIPAddress{ID: ptr.To(fmt.Sprintf("testCluster-a%s%d", fullServiceName, serviceIndex))},
				},
			},
			{
				Name: ptr.To(fmt.Sprintf("a%s%d-IPv6", fullServiceName, serviceIndex)),
				ID:   ptr.To("fip-IPv6"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
					PublicIPAddress:           &armnetwork.PublicIPAddress{ID: ptr.To(fmt.Sprintf("testCluster-a%s%d-IPv6", fullServiceName, serviceIndex))},
				},
			},
		}
		if isInternal {
			fips[0].Properties.Subnet = &armnetwork.Subnet{Name: ptr.To("subnet")}
			fips[1].Properties.Subnet = &armnetwork.Subnet{Name: ptr.To("subnet")}
			lb.Properties.LoadBalancingRules[1].Properties.BackendPort = ptr.To(int32(18081))
		}
		lb.Properties.FrontendIPConfigurations = fips

		*expectedLBs = append(*expectedLBs, lb)
	} else {
		lbRules := []*armnetwork.LoadBalancingRule{
			{
				Name: ptr.To(fmt.Sprintf("a%s%d-TCP-8081", fullServiceName, serviceIndex)),
			},
			{
				Name: ptr.To(fmt.Sprintf("a%s%d-TCP-8081-IPv6", fullServiceName, serviceIndex)),
			},
		}
		(*expectedLBs)[lbIndex].Properties.LoadBalancingRules = append((*expectedLBs)[lbIndex].Properties.LoadBalancingRules, lbRules...)
		fips := []*armnetwork.FrontendIPConfiguration{
			{
				Name: ptr.To(fmt.Sprintf("a%s%d", fullServiceName, serviceIndex)),
				ID:   ptr.To("fip"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
					PublicIPAddress:           &armnetwork.PublicIPAddress{ID: ptr.To(fmt.Sprintf("testCluster-a%s%d", fullServiceName, serviceIndex))},
				},
			},
			{
				Name: ptr.To(fmt.Sprintf("a%s%d-IPv6", fullServiceName, serviceIndex)),
				ID:   ptr.To("fip-IPv6"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv6),
					PublicIPAddress:           &armnetwork.PublicIPAddress{ID: ptr.To(fmt.Sprintf("testCluster-a%s%d-IPv6", fullServiceName, serviceIndex))},
				},
			},
		}
		if isInternal {
			for _, fip := range fips {
				fip.Properties.Subnet = &armnetwork.Subnet{Name: ptr.To("subnet")}
			}
		}
		(*expectedLBs)[lbIndex].Properties.FrontendIPConfigurations = append((*expectedLBs)[lbIndex].Properties.FrontendIPConfigurations, fips...)
	}

	mockLBsClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
	mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	for _, lb := range *expectedLBs {
		mockLBsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, *lb.Name, gomock.Any()).Return((*expectedLBs)[lbIndex], nil).MaxTimes(2)
	}
	mockLBsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return((*expectedLBs), nil).MaxTimes(4)
	mockLBsClient.EXPECT().List(gomock.Any(), gomock.Not(az.ResourceGroup)).Return([]*armnetwork.LoadBalancer{}, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()
	mockLBsClient.EXPECT().Get(gomock.Any(), gomock.Not(az.ResourceGroup), gomock.Any(), gomock.Any()).Return(&armnetwork.LoadBalancer{}, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()
	mockLBsClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).MaxTimes(1)

	return expectedLBName
}

func setMockLBs(az *Cloud, expectedLBs *[]*armnetwork.LoadBalancer, svcName string, lbCount, serviceIndex int, isInternal bool) string {
	lbIndex := (serviceIndex - 1) % lbCount
	expectedLBName := ""
	if lbIndex == 0 {
		expectedLBName = testClusterName
	} else {
		expectedLBName = fmt.Sprintf("as-%d", lbIndex)
	}
	if isInternal {
		expectedLBName += "-internal"
	}

	fullServiceName := strings.Replace(svcName, "-", "", -1)

	if lbIndex >= len((*expectedLBs)) {
		lb := &armnetwork.LoadBalancer{
			Location: &az.Location,
			Properties: &armnetwork.LoadBalancerPropertiesFormat{
				BackendAddressPools: []*armnetwork.BackendAddressPool{
					{
						Name: ptr.To("testCluster"),
					},
				},
			},
		}
		lb.Name = &expectedLBName
		lb.Properties.LoadBalancingRules = []*armnetwork.LoadBalancingRule{
			{
				Name: ptr.To(fmt.Sprintf("a%s%d-TCP-8081", fullServiceName, serviceIndex)),
			},
		}
		fips := []*armnetwork.FrontendIPConfiguration{
			{
				Name: ptr.To(fmt.Sprintf("a%s%d", fullServiceName, serviceIndex)),
				ID:   ptr.To("fip"),
				Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					PublicIPAddress:           &armnetwork.PublicIPAddress{ID: ptr.To(fmt.Sprintf("testCluster-a%s%d", fullServiceName, serviceIndex))},
					PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
				},
			},
		}
		if isInternal {
			fips[0].Properties.Subnet = &armnetwork.Subnet{Name: ptr.To("subnet")}
		}
		lb.Properties.FrontendIPConfigurations = fips

		(*expectedLBs) = append((*expectedLBs), lb)
	} else {
		(*expectedLBs)[lbIndex].Properties.LoadBalancingRules = append((*expectedLBs)[lbIndex].Properties.LoadBalancingRules, &armnetwork.LoadBalancingRule{
			Name: ptr.To(fmt.Sprintf("a%s%d-TCP-8081", fullServiceName, serviceIndex)),
		})
		fip := &armnetwork.FrontendIPConfiguration{
			Name: ptr.To(fmt.Sprintf("a%s%d", fullServiceName, serviceIndex)),
			ID:   ptr.To("fip"),
			Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
				PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
				PublicIPAddress:           &armnetwork.PublicIPAddress{ID: ptr.To(fmt.Sprintf("testCluster-a%s%d", fullServiceName, serviceIndex))},
				PrivateIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
			},
		}
		if isInternal {
			fip.Properties.Subnet = &armnetwork.Subnet{Name: ptr.To("subnet")}
		}
		(*expectedLBs)[lbIndex].Properties.FrontendIPConfigurations = append((*expectedLBs)[lbIndex].Properties.FrontendIPConfigurations, fip)
	}

	mockLBsClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
	mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	for _, lb := range *expectedLBs {
		mockLBsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, *lb.Name, gomock.Any()).Return((*expectedLBs)[lbIndex], nil).MaxTimes(2)
	}
	mockLBsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return((*expectedLBs), nil).MaxTimes(4)
	mockLBsClient.EXPECT().List(gomock.Any(), gomock.Not(az.ResourceGroup)).Return([]*armnetwork.LoadBalancer{}, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()
	mockLBsClient.EXPECT().Get(gomock.Any(), gomock.Not(az.ResourceGroup), gomock.Any(), gomock.Any()).Return(&armnetwork.LoadBalancer{}, &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: cloudprovider.InstanceNotFound.Error()}).AnyTimes()
	mockLBsClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).MaxTimes(1)

	return expectedLBName
}

// Test addition of a new service on an internal LB with a subnet.
func TestReconcileLoadBalancerAddServiceOnInternalSubnet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnvDualStack(az, expectedInterfaces, expectedVirtualMachines, 1)

	svc := getInternalTestServiceDualStack("service1", 80)
	validateTestSubnet(t, az, &svc)

	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBsDualStack(az, &expectedLBs, "service", 1, 1, true)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	assert.Nil(t, err)

	// ensure we got 2 frontend ip configurations
	assert.Equal(t, 2, len(lb.Properties.FrontendIPConfigurations))
	validateLoadBalancer(t, az, lb, svc)
}

// Test addition of services on an internal LB using both default and explicit subnets.
func TestReconcileLoadBalancerAddServicesOnMultipleSubnets(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnvDualStack(az, expectedInterfaces, expectedVirtualMachines, 1)

	svc1 := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 8081)
	svc2 := getInternalTestServiceDualStack("service2", 8081)

	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBsDualStack(az, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	mockPLSRepo := az.plsRepo.(*privatelinkservice.MockRepository)
	mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil)

	// svc1 is using LB without "-internal" suffix
	lb, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc1, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling svc1: %q", err)
	}

	// ensure we got a frontend ip configuration for each service
	assert.Equal(t, 2, len(lb.Properties.FrontendIPConfigurations))

	validateLoadBalancer(t, az, lb, svc1)

	// Internal and External service cannot reside on the same LB resource
	validateTestSubnet(t, az, &svc2)

	expectedLBs = make([]*armnetwork.LoadBalancer, 0)
	setMockLBsDualStack(az, &expectedLBs, "service", 1, 2, true)

	// svc2 is using LB with "-internal" suffix
	lb, _, err = az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc2, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling svc2: %q", err)
	}

	// ensure we got a frontend ip configuration for each service
	assert.Equal(t, 2, len(lb.Properties.FrontendIPConfigurations))

	validateLoadBalancer(t, az, lb, svc2)
}

// Test moving a service exposure from one subnet to another.
func TestReconcileLoadBalancerEditServiceSubnet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnvDualStack(az, expectedInterfaces, expectedVirtualMachines, 1)

	svc := getInternalTestServiceDualStack("service1", 8081)
	validateTestSubnet(t, az, &svc)

	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBsDualStack(az, &expectedLBs, "service", 1, 1, true)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	mockPLSRepo := privatelinkservice.NewMockRepository(ctrl)
	mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()
	az.plsRepo = mockPLSRepo

	lb, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling initial svc: %q", err)
	}

	validateLoadBalancer(t, az, lb, svc)

	svc.Annotations[consts.ServiceAnnotationLoadBalancerInternalSubnet] = "subnet"
	validateTestSubnet(t, az, &svc)

	expectedLBs = make([]*armnetwork.LoadBalancer, 0)
	setMockLBsDualStack(az, &expectedLBs, "service", 1, 1, true)

	lb, _, err = az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling edits to svc: %q", err)
	}

	// ensure we got a frontend ip configuration for the service
	assert.Equal(t, 2, len(lb.Properties.FrontendIPConfigurations))

	validateLoadBalancer(t, az, lb, svc)
}

func TestReconcileLoadBalancerNodeHealth(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnvDualStack(az, expectedInterfaces, expectedVirtualMachines, 1)

	svc := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)
	svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	svc.Spec.HealthCheckNodePort = int32(32456)

	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBsDualStack(az, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	mockPLSRepo := az.plsRepo.(*privatelinkservice.MockRepository)
	mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()

	lb, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	assert.Nil(t, err)

	// ensure we got a frontend ip configuration
	assert.Equal(t, 2, len(lb.Properties.FrontendIPConfigurations))

	validateLoadBalancer(t, az, lb, svc)
}

// Test removing all services results in removing the frontend ip configuration
func TestReconcileLoadBalancerRemoveService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnvDualStack(az, expectedInterfaces, expectedVirtualMachines, 1)

	svc := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80, 443)

	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBsDualStack(az, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockPLSRepo := az.plsRepo.(*privatelinkservice.MockRepository)
	mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()

	_, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	assert.Nil(t, err)

	expectedLBs[0].Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{}
	mockLBsClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
	mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mockLBsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, expectedLBs[0].Name, gomock.Any()).Return(expectedLBs[0], nil).MaxTimes(2)
	mockLBsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(expectedLBs, nil).MaxTimes(3)

	lb, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, false /* wantLb */)
	assert.Nil(t, err)

	// ensure we abandoned the frontend ip configuration
	assert.Zero(t, len(lb.Properties.FrontendIPConfigurations))

	validateLoadBalancer(t, az, lb)
}

// Test removing all service ports results in removing the frontend ip configuration
func TestReconcileLoadBalancerRemoveAllPortsRemovesFrontendConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnvDualStack(az, expectedInterfaces, expectedVirtualMachines, 1)

	svc := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)

	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBsDualStack(az, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	mockPLSRepo := az.plsRepo.(*privatelinkservice.MockRepository)
	mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()

	lb, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	assert.Nil(t, err)
	validateLoadBalancer(t, az, lb, svc)

	svcUpdated := getTestServiceDualStack("service1", v1.ProtocolTCP, nil)

	expectedLBs[0].Properties.FrontendIPConfigurations = []*armnetwork.FrontendIPConfiguration{}
	mockLBsClient := az.NetworkClientFactory.GetLoadBalancerClient().(*mock_loadbalancerclient.MockInterface)
	mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mockLBsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, expectedLBs[0].Name, gomock.Any()).Return(expectedLBs[0], nil).MaxTimes(2)
	mockLBsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(expectedLBs, nil).MaxTimes(3)

	lb, _, err = az.reconcileLoadBalancer(context.TODO(), testClusterName, &svcUpdated, clusterResources.nodes, false /* wantLb*/)
	assert.Nil(t, err)

	// ensure we abandoned the frontend ip configuration
	assert.Zero(t, len(lb.Properties.FrontendIPConfigurations))

	validateLoadBalancer(t, az, lb, svcUpdated)
}

// Test removal of a port from an existing service.
func TestReconcileLoadBalancerRemovesPort(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnvDualStack(az, expectedInterfaces, expectedVirtualMachines, 1)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockPLSRepo := az.plsRepo.(*privatelinkservice.MockRepository)
	mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()
	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBsDualStack(az, &expectedLBs, "service", 1, 1, false)
	svc := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80, 443)
	_, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	assert.Nil(t, err)

	expectedLBs = make([]*armnetwork.LoadBalancer, 0)
	setMockLBsDualStack(az, &expectedLBs, "service", 1, 1, false)
	svcUpdated := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80)
	lb, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svcUpdated, clusterResources.nodes, true /* wantLb */)
	assert.Nil(t, err)

	validateLoadBalancer(t, az, lb, svcUpdated)
}

// Test reconciliation of multiple services on same port
func TestReconcileLoadBalancerMultipleServices(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnvDualStack(az, expectedInterfaces, expectedVirtualMachines, 2)

	svc1 := getTestServiceDualStack("service1", v1.ProtocolTCP, nil, 80, 443)
	svc2 := getTestServiceDualStack("service2", v1.ProtocolTCP, nil, 81)

	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBsDualStack(az, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	mockPLSRepo := az.plsRepo.(*privatelinkservice.MockRepository)
	mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()

	_, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc1, clusterResources.nodes, true /* wantLb */)
	assert.Nil(t, err)

	setMockLBsDualStack(az, &expectedLBs, "service", 1, 2, false)

	updatedLoadBalancer, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc2, clusterResources.nodes, true /* wantLb */)
	assert.Nil(t, err)

	validateLoadBalancer(t, az, updatedLoadBalancer, svc1, svc2)
}

func findLBRuleForPort(lbRules []*armnetwork.LoadBalancingRule, port int32) (*armnetwork.LoadBalancingRule, error) {
	for _, lbRule := range lbRules {
		if *lbRule.Properties.FrontendPort == port {
			return lbRule, nil
		}
	}
	return &armnetwork.LoadBalancingRule{}, fmt.Errorf("expected LB rule with port %d but none found", port)
}

func TestServiceDefaultsToNoSessionPersistence(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestService("service-sa-omitted1", v1.ProtocolTCP, nil, false, 8081)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, expectedInterfaces, expectedVirtualMachines, 1)

	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBs(az, &expectedLBs, "service-sa-omitted", 1, 1, false)

	expectedPIP := &armnetwork.PublicIPAddress{
		Name:     ptr.To("testCluster-aservicesaomitted1"),
		Location: &az.Location,
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
			PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
		},
		Tags: map[string]*string{
			consts.ServiceTagKey:  ptr.To("aservicesaomitted1"),
			consts.ClusterNameKey: ptr.To(testClusterName),
		},
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
		},
		ID: ptr.To("testCluster-aservicesaomitted1"),
	}

	mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
	mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mockPIPsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, *expectedPIP.Name, nil).Return(expectedPIP, nil).AnyTimes()

	mockPlsClient := az.plsRepo.(*privatelinkservice.MockRepository)
	mockPlsClient.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling svc1: %q", err)
	}
	validateLoadBalancer(t, az, lb, svc)
	lbRule, err := findLBRuleForPort(lb.Properties.LoadBalancingRules, 8081)
	if err != nil {
		t.Error(err)
	}

	if *lbRule.Properties.LoadDistribution != armnetwork.LoadDistributionDefault {
		t.Errorf("Expected LB rule to have default load distribution but was %s", *lbRule.Properties.LoadDistribution)
	}
}

func TestServiceRespectsNoSessionAffinity(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestService("service-sa-none", v1.ProtocolTCP, nil, false, 8081)
	svc.Spec.SessionAffinity = v1.ServiceAffinityNone
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, expectedInterfaces, expectedVirtualMachines, 1)

	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBs(az, &expectedLBs, "service-sa-none", 1, 1, false)

	expectedPIP := &armnetwork.PublicIPAddress{
		Name:     ptr.To("testCluster-aservicesanone"),
		Location: &az.Location,
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
			PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
		},
		Tags: map[string]*string{
			consts.ServiceTagKey:  ptr.To("aservicesanone"),
			consts.ClusterNameKey: ptr.To(testClusterName),
		},
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
		},
		ID: ptr.To("testCluster-aservicesanone"),
	}

	mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
	mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mockPIPsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]*armnetwork.PublicIPAddress{expectedPIP}, nil).AnyTimes()
	mockPIPsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "testCluster-aservicesanone", gomock.Any()).Return(expectedPIP, nil).AnyTimes()

	mockPLSRepo := privatelinkservice.NewMockRepository(ctrl)
	mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()
	az.plsRepo = mockPLSRepo

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling svc1: %q", err)
	}

	validateLoadBalancer(t, az, lb, svc)

	lbRule, err := findLBRuleForPort(lb.Properties.LoadBalancingRules, 8081)
	if err != nil {
		t.Error(err)
	}

	if *lbRule.Properties.LoadDistribution != armnetwork.LoadDistributionDefault {
		t.Errorf("Expected LB rule to have default load distribution but was %s", *lbRule.Properties.LoadDistribution)
	}
}

func TestServiceRespectsClientIPSessionAffinity(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestService("service-sa-clientip", v1.ProtocolTCP, nil, false, 8081)
	svc.Spec.SessionAffinity = v1.ServiceAffinityClientIP
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, expectedInterfaces, expectedVirtualMachines, 1)

	expectedLBs := make([]*armnetwork.LoadBalancer, 0)
	setMockLBs(az, &expectedLBs, "service-sa-clientip", 1, 1, false)

	expectedPIP := &armnetwork.PublicIPAddress{
		Name:     ptr.To("testCluster-aservicesaclientip"),
		Location: &az.Location,
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
			PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
		},
		Tags: map[string]*string{
			consts.ServiceTagKey:  ptr.To("aservicesaclientip"),
			consts.ClusterNameKey: ptr.To(testClusterName),
		},
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
		},
		ID: ptr.To("testCluster-aservicesaclientip"),
	}

	mockPIPsClient := az.NetworkClientFactory.GetPublicIPAddressClient().(*mock_publicipaddressclient.MockInterface)
	mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	mockPIPsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "testCluster-aservicesaclientip", gomock.Any()).Return(expectedPIP, nil).AnyTimes()
	mockPIPsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]*armnetwork.PublicIPAddress{expectedPIP}, nil).AnyTimes()

	mockPLSRepo := az.plsRepo.(*privatelinkservice.MockRepository)
	mockPLSRepo.EXPECT().Get(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(&armnetwork.PrivateLinkService{ID: to.Ptr(consts.PrivateLinkServiceNotExistID)}, nil).AnyTimes()

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, _ *v1.Service, lb *armnetwork.LoadBalancer) (bool, bool, *armnetwork.LoadBalancer, error) {
		return false, false, lb, nil
	}).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, _, err := az.reconcileLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling svc1: %q", err)
	}

	validateLoadBalancer(t, az, lb, svc)

	lbRule, err := findLBRuleForPort(lb.Properties.LoadBalancingRules, 8081)
	if err != nil {
		t.Error(err)
	}

	if *lbRule.Properties.LoadDistribution != armnetwork.LoadDistributionSourceIP {
		t.Errorf("Expected LB rule to have SourceIP load distribution but was %s", *lbRule.Properties.LoadDistribution)
	}
}

func TestReconcilePublicIPsWithNewService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestServiceDualStack("servicea", v1.ProtocolTCP, nil, 80, 443)
	v4Enabled, v6Enabled := getIPFamiliesEnabled(&svc)

	setMockPublicIPs(az, 1, v4Enabled, v6Enabled)

	pips, err := az.reconcilePublicIPs(context.TODO(), testClusterName, &svc, "", true /* wantLb*/)
	assert.Nil(t, err)
	validatePublicIPs(t, pips, &svc, true)

	pips2, err := az.reconcilePublicIPs(context.TODO(), testClusterName, &svc, "", true /* wantLb */)
	assert.Nil(t, err)
	validatePublicIPs(t, pips2, &svc, true)

	pipsNames1, pipsNames2 := []string{}, []string{}
	pipsAddrs1, pipsAddrs2 := []string{}, []string{}
	for _, pip := range pips {
		pipsNames1 = append(pipsNames1, ptr.Deref(pip.Name, ""))
		pipsAddrs1 = append(pipsAddrs1, ptr.Deref(pip.Properties.IPAddress, ""))
	}
	for _, pip := range pips2 {
		pipsNames2 = append(pipsNames2, ptr.Deref(pip.Name, ""))
		pipsAddrs2 = append(pipsAddrs2, ptr.Deref(pip.Properties.IPAddress, ""))
	}
	assert.Truef(t, compareStrings(pipsNames1, pipsNames2) && compareStrings(pipsAddrs1, pipsAddrs2),
		"We should get the exact same public ip resource after a second reconcile")
}

func TestReconcilePublicIPsRemoveService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestServiceDualStack("servicea", v1.ProtocolTCP, nil, 80, 443)
	v4Enabled, v6Enabled := getIPFamiliesEnabled(&svc)

	setMockPublicIPs(az, 1, v4Enabled, v6Enabled)

	pips, err := az.reconcilePublicIPs(context.TODO(), testClusterName, &svc, "", true /* wantLb*/)
	assert.Nil(t, err)

	validatePublicIPs(t, pips, &svc, true)

	// Remove the service
	pips, err = az.reconcilePublicIPs(context.TODO(), testClusterName, &svc, "", false /* wantLb */)
	assert.Nil(t, err)
	validatePublicIPs(t, pips, &svc, false)
}

func TestReconcilePublicIPsWithInternalService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getInternalTestServiceDualStack("servicea", 80, 443)
	v4Enabled, v6Enabled := getIPFamiliesEnabled(&svc)

	setMockPublicIPs(az, 1, v4Enabled, v6Enabled)

	pips, err := az.reconcilePublicIPs(context.TODO(), testClusterName, &svc, "", true /* wantLb*/)
	assert.Nil(t, err)

	validatePublicIPs(t, pips, &svc, true)
}

func TestReconcilePublicIPsWithExternalAndInternalSwitch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getInternalTestServiceDualStack("servicea", 80, 443)
	v4Enabled, v6Enabled := getIPFamiliesEnabled(&svc)

	setMockPublicIPs(az, 1, v4Enabled, v6Enabled)

	pips, err := az.reconcilePublicIPs(context.TODO(), testClusterName, &svc, "", true /* wantLb*/)
	assert.Nil(t, err)
	validatePublicIPs(t, pips, &svc, true)

	// Update to external service
	svcUpdated := getTestService("servicea", v1.ProtocolTCP, nil, false, 80)
	pips, err = az.reconcilePublicIPs(context.TODO(), testClusterName, &svcUpdated, "", true /* wantLb*/)
	assert.Nil(t, err)
	validatePublicIPs(t, pips, &svcUpdated, true)

	// Update to internal service again
	pips, err = az.reconcilePublicIPs(context.TODO(), testClusterName, &svc, "", true /* wantLb*/)
	assert.Nil(t, err)
	validatePublicIPs(t, pips, &svc, true)
}

const networkInterfacesIDTemplate = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/networkInterfaces/%s"
const primaryIPConfigIDTemplate = "%s/ipConfigurations/ipconfig"

// returns the full identifier of Network Interface.
func getNetworkInterfaceID(subscriptionID string, resourceGroupName, nicName string) string {
	return fmt.Sprintf(
		networkInterfacesIDTemplate,
		subscriptionID,
		resourceGroupName,
		nicName)
}

// returns the full identifier of a private ipconfig of the nic
func getPrimaryIPConfigID(nicID string) string {
	return fmt.Sprintf(
		primaryIPConfigIDTemplate,
		nicID)
}

const TestResourceNameFormat = "%s-%d"
const TestVMResourceBaseName = "vm"
const TestASResourceBaseName = "as"

func getTestResourceName(resourceBaseName string, index int) string {
	return fmt.Sprintf(TestResourceNameFormat, resourceBaseName, index)
}

func getVMName(vmIndex int) string {
	return getTestResourceName(TestVMResourceBaseName, vmIndex)
}

func getAvailabilitySetName(az *Cloud, vmIndex int, numAS int) string {
	asIndex := vmIndex % numAS
	if asIndex == 0 {
		return az.Config.PrimaryAvailabilitySetName
	}

	return getTestResourceName(TestASResourceBaseName, asIndex)
}

// test supporting on 1 nic per vm
// we really dont care about the name of the nic
// just using the vm name for testing purposes
func getNICName(vmIndex int) string {
	return getVMName(vmIndex)
}

type ClusterResources struct {
	nodes                []*v1.Node
	availabilitySetNames []string
}

func getClusterResources(az *Cloud, vmCount int, availabilitySetCount int) (clusterResources *ClusterResources, expectedInterfaces []*armnetwork.Interface, expectedVirtualMachines []*armcompute.VirtualMachine) {
	if vmCount < availabilitySetCount {
		return nil, expectedInterfaces, expectedVirtualMachines
	}
	clusterResources = &ClusterResources{}
	clusterResources.nodes = []*v1.Node{}
	clusterResources.availabilitySetNames = []string{}
	for vmIndex := 0; vmIndex < vmCount; vmIndex++ {
		vmName := getVMName(vmIndex)
		asName := getAvailabilitySetName(az, vmIndex, availabilitySetCount)
		clusterResources.availabilitySetNames = append(clusterResources.availabilitySetNames, asName)

		nicName := getNICName(vmIndex)
		nicID := getNetworkInterfaceID(az.Config.SubscriptionID, az.Config.ResourceGroup, nicName)
		primaryIPConfigID := getPrimaryIPConfigID(nicID)
		isPrimary := true
		newNIC := &armnetwork.Interface{
			ID:   &nicID,
			Name: &nicName,
			Properties: &armnetwork.InterfacePropertiesFormat{
				IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
					{
						ID: &primaryIPConfigID,
						Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
							PrivateIPAddress: &nicName,
							Primary:          &isPrimary,
						},
					},
				},
			},
		}
		expectedInterfaces = append(expectedInterfaces, newNIC)

		// create vm
		asID := az.getAvailabilitySetID(az.Config.ResourceGroup, asName)
		newVM := &armcompute.VirtualMachine{
			Name:     &vmName,
			Location: &az.Config.Location,
			Properties: &armcompute.VirtualMachineProperties{
				AvailabilitySet: &armcompute.SubResource{
					ID: &asID,
				},
				NetworkProfile: &armcompute.NetworkProfile{
					NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
						{
							ID: &nicID,
						},
					},
				},
			},
		}
		expectedVirtualMachines = append(expectedVirtualMachines, newVM)

		// add to kubernetes
		newNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: vmName,
				Labels: map[string]string{
					v1.LabelHostname: vmName,
				},
			},
		}
		clusterResources.nodes = append(clusterResources.nodes, newNode)
	}

	return clusterResources, expectedInterfaces, expectedVirtualMachines
}

func getBackendPort(port int32) int32 {
	return port + 10000
}

// TODO: This function should be merged into getTestService()
func getTestServiceDualStack(identifier string, proto v1.Protocol, annotations map[string]string, requestedPorts ...int32) v1.Service {
	svc := getTestServiceCommon(identifier, proto, annotations, requestedPorts...)
	svc.Spec.ClusterIPs = []string{"10.0.0.2", "fd00::1907"}
	svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}

	return svc
}

func getTestService(identifier string, proto v1.Protocol, annotations map[string]string, isIPv6 bool, requestedPorts ...int32) v1.Service {
	svc := getTestServiceCommon(identifier, proto, annotations, requestedPorts...)
	svc.Spec.ClusterIP = "10.0.0.2"
	svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol}
	if isIPv6 {
		svc.Spec.ClusterIP = "fd00::1907"
		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
	}

	return svc
}

func getTestServiceCommon(identifier string, proto v1.Protocol, annotations map[string]string, requestedPorts ...int32) v1.Service {
	ports := []v1.ServicePort{}
	for _, port := range requestedPorts {
		ports = append(ports, v1.ServicePort{
			Name:     fmt.Sprintf("port-tcp-%d", port),
			Protocol: proto,
			Port:     port,
			NodePort: getBackendPort(port),
		})
	}

	svc := v1.Service{
		Spec: v1.ServiceSpec{
			Type:  v1.ServiceTypeLoadBalancer,
			Ports: ports,
		},
	}
	svc.Name = identifier
	svc.Namespace = "default"
	svc.UID = types.UID(identifier)
	if annotations == nil {
		svc.Annotations = make(map[string]string)
	} else {
		svc.Annotations = annotations
	}

	return svc
}

func getInternalTestService(identifier string, requestedPorts ...int32) v1.Service {
	return getTestServiceWithAnnotation(identifier, map[string]string{consts.ServiceAnnotationLoadBalancerInternal: consts.TrueAnnotationValue}, false, requestedPorts...)
}

// getInternalTestServiceDualStack() should be merged into getInternalTestService()
func getInternalTestServiceDualStack(identifier string, requestedPorts ...int32) v1.Service {
	return getTestServiceWithAnnotation(identifier, map[string]string{consts.ServiceAnnotationLoadBalancerInternal: consts.TrueAnnotationValue}, true, requestedPorts...)
}

func getTestServiceWithAnnotation(identifier string, annotations map[string]string, isDualStack bool, requestedPorts ...int32) v1.Service {
	var svc v1.Service
	if isDualStack {
		svc = getTestServiceDualStack(identifier, v1.ProtocolTCP, nil, requestedPorts...)
	} else {
		svc = getTestService(identifier, v1.ProtocolTCP, nil, false, requestedPorts...)
	}
	for k, v := range annotations {
		svc.Annotations[k] = v
	}
	return svc
}

func getResourceGroupTestService(identifier, resourceGroup, loadBalancerIP string, requestedPorts ...int32) v1.Service {
	svc := getTestService(identifier, v1.ProtocolTCP, nil, false, requestedPorts...)
	setServiceLoadBalancerIP(&svc, loadBalancerIP)
	svc.Annotations[consts.ServiceAnnotationLoadBalancerResourceGroup] = resourceGroup
	return svc
}

func getServiceSourceRanges(service *v1.Service) []string {
	if len(service.Spec.LoadBalancerSourceRanges) == 0 {
		if !requiresInternalLoadBalancer(service) {
			return []string{"Internet"}
		}
	}

	return service.Spec.LoadBalancerSourceRanges
}

func getTestSecurityGroupCommon(az *Cloud, v4Enabled, v6Enabled bool, services ...v1.Service) *armnetwork.SecurityGroup {
	rules := []*armnetwork.SecurityRule{}
	for i, service := range services {
		for _, port := range service.Spec.Ports {
			getRule := func(svc *v1.Service, port v1.ServicePort, src string, isIPv6 bool) *armnetwork.SecurityRule {
				ruleName := az.getSecurityRuleName(svc, port, src, isIPv6)
				return &armnetwork.SecurityRule{
					Name: ptr.To(ruleName),
					Properties: &armnetwork.SecurityRulePropertiesFormat{
						SourceAddressPrefix:  ptr.To(src),
						DestinationPortRange: ptr.To(fmt.Sprintf("%d", port.Port)),
					},
				}
			}

			sources := getServiceSourceRanges(&services[i])
			for _, src := range sources {
				if v4Enabled {
					rule := getRule(&services[i], port, src, false)
					rules = append(rules, rule)
				}
				if v6Enabled {
					rule := getRule(&services[i], port, src, true)
					rules = append(rules, rule)
				}
			}
		}
	}

	sg := armnetwork.SecurityGroup{
		Name: &az.SecurityGroupName,
		Etag: ptr.To("0000000-0000-0000-0000-000000000000"),
		Properties: &armnetwork.SecurityGroupPropertiesFormat{
			SecurityRules: rules,
		},
	}

	return &sg
}

func getTestSecurityGroupDualStack(az *Cloud, services ...v1.Service) *armnetwork.SecurityGroup {
	return getTestSecurityGroupCommon(az, true, true, services...)
}

func getTestSecurityGroup(az *Cloud, services ...v1.Service) *armnetwork.SecurityGroup {
	return getTestSecurityGroupCommon(az, true, false, services...)
}

func validateLoadBalancer(t *testing.T, az *Cloud, loadBalancer *armnetwork.LoadBalancer, services ...v1.Service) {
	expectedRuleCount := 0
	expectedFrontendIPCount := 0
	expectedProbeCount := 0
	expectedFrontendIPs := []ExpectedFrontendIPInfo{}
	svcIPFamilyCount := 1
	if len(services) > 0 {
		svcIPFamilyCount = len(services[0].Spec.IPFamilies)
	}
	for i, svc := range services {
		isInternal := requiresInternalLoadBalancer(&services[i])
		if len(svc.Spec.Ports) > 0 {
			expectedFrontendIPCount += svcIPFamilyCount
			expectedSubnetName := ""
			if isInternal {
				expectedSubnetName = svc.Annotations[consts.ServiceAnnotationLoadBalancerInternalSubnet]
				if expectedSubnetName == "" {
					expectedSubnetName = az.SubnetName
				}
			}
			expectedFrontendIP := ExpectedFrontendIPInfo{
				Name:   az.getDefaultFrontendIPConfigName(&services[i]),
				Subnet: ptr.To(expectedSubnetName),
			}
			expectedFrontendIPs = append(expectedFrontendIPs, expectedFrontendIP)
			if svcIPFamilyCount == 2 {
				expectedFrontendIP := ExpectedFrontendIPInfo{
					Name:   az.getDefaultFrontendIPConfigName(&services[i]) + "-" + consts.IPVersionIPv6String,
					Subnet: ptr.To(expectedSubnetName),
				}
				expectedFrontendIPs = append(expectedFrontendIPs, expectedFrontendIP)
			}
		}
		for _, wantedRule := range svc.Spec.Ports {
			expectedRuleCount += svcIPFamilyCount
			wantedRuleNameMap := map[bool]string{}
			for _, ipFamily := range services[i].Spec.IPFamilies {
				isIPv6 := ipFamily == v1.IPv6Protocol
				wantedRuleName := az.getLoadBalancerRuleName(&services[i], wantedRule.Protocol, wantedRule.Port, isIPv6)
				wantedRuleNameMap[isIPv6] = wantedRuleName
				foundRule := false
				for _, actualRule := range loadBalancer.Properties.LoadBalancingRules {
					if actualRule != nil && actualRule.Name != nil &&
						strings.EqualFold(*actualRule.Name, wantedRuleName) &&
						actualRule.Properties != nil && actualRule.Properties.FrontendPort != nil &&
						*actualRule.Properties.FrontendPort == wantedRule.Port {
						if isInternal {
							if (!isIPv6 && *actualRule.Properties.BackendPort == wantedRule.Port) ||
								(isIPv6 && *actualRule.Properties.BackendPort == wantedRule.NodePort) {
								foundRule = true
								break
							}
						} else if *actualRule.Properties.BackendPort == wantedRule.Port {
							foundRule = true
							break
						}
					}
				}
				if !foundRule {
					t.Errorf("Expected load balancer rule but didn't find it: %q", wantedRuleName)
				}
			}

			// if UDP rule, there is no probe
			if wantedRule.Protocol == v1.ProtocolUDP {
				continue
			}

			expectedProbeCount += svcIPFamilyCount
			for _, ipFamily := range services[i].Spec.IPFamilies {
				isIPv6 := ipFamily == v1.IPv6Protocol
				foundProbe := false
				if servicehelpers.NeedsHealthCheck(&services[i]) {
					path, port := servicehelpers.GetServiceHealthCheckPathPort(&services[i])
					wantedRuleName := az.getLoadBalancerRuleName(&services[i], v1.ProtocolTCP, port, isIPv6)
					for _, actualProbe := range loadBalancer.Properties.Probes {
						if strings.EqualFold(*actualProbe.Name, wantedRuleName) &&
							*actualProbe.Properties.Port == port &&
							*actualProbe.Properties.RequestPath == path &&
							*actualProbe.Properties.Protocol == armnetwork.ProbeProtocolHTTP {
							foundProbe = true
							break
						}
					}
				} else {
					for _, actualProbe := range loadBalancer.Properties.Probes {
						if strings.EqualFold(*actualProbe.Name, wantedRuleNameMap[isIPv6]) &&
							*actualProbe.Properties.Port == wantedRule.NodePort {
							foundProbe = true
							break
						}
					}
				}
				if !foundProbe {
					for _, actualProbe := range loadBalancer.Properties.Probes {
						t.Logf("Probe: %s %d", *actualProbe.Name, *actualProbe.Properties.Port)
					}
					t.Errorf("Expected loadbalancer probe but didn't find it: %q", wantedRuleNameMap[isIPv6])
				}
			}
		}
	}

	frontendIPCount := len(loadBalancer.Properties.FrontendIPConfigurations)
	if frontendIPCount != expectedFrontendIPCount {
		t.Errorf("Expected the loadbalancer to have %d frontend IPs. Found %d.\n%v", expectedFrontendIPCount, frontendIPCount, loadBalancer.Properties.FrontendIPConfigurations)
	}

	frontendIPs := loadBalancer.Properties.FrontendIPConfigurations
	for _, expectedFrontendIP := range expectedFrontendIPs {
		if !expectedFrontendIP.existsIn(frontendIPs) {
			t.Errorf("Expected the loadbalancer to have frontend IP %s/%s. Found %s", expectedFrontendIP.Name, ptr.Deref(expectedFrontendIP.Subnet, ""), describeFIPs(frontendIPs))
		}
	}

	lenRules := len(loadBalancer.Properties.LoadBalancingRules)
	if lenRules != expectedRuleCount {
		t.Errorf("Expected the loadbalancer to have %d rules. Found %d.\n%v", expectedRuleCount, lenRules, loadBalancer.Properties.LoadBalancingRules)
	}

	lenProbes := len(loadBalancer.Properties.Probes)
	if lenProbes != expectedProbeCount {
		t.Errorf("Expected the loadbalancer to have %d probes. Found %d.", expectedRuleCount, lenProbes)
	}
}

type ExpectedFrontendIPInfo struct {
	Name   string
	Subnet *string
}

func (expected ExpectedFrontendIPInfo) matches(frontendIP *armnetwork.FrontendIPConfiguration) bool {
	return strings.EqualFold(expected.Name, ptr.Deref(frontendIP.Name, "")) && strings.EqualFold(ptr.Deref(expected.Subnet, ""), ptr.Deref(subnetName(frontendIP), ""))
}

func (expected ExpectedFrontendIPInfo) existsIn(frontendIPs []*armnetwork.FrontendIPConfiguration) bool {
	for _, fip := range frontendIPs {
		if expected.matches(fip) {
			return true
		}
	}
	return false
}

func subnetName(frontendIP *armnetwork.FrontendIPConfiguration) *string {
	if frontendIP.Properties.Subnet != nil {
		return frontendIP.Properties.Subnet.Name
	}
	return nil
}

func describeFIPs(frontendIPs []*armnetwork.FrontendIPConfiguration) string {
	description := ""
	for _, actualFIP := range frontendIPs {
		actualSubnetName := ""
		if actualFIP.Properties.Subnet != nil {
			actualSubnetName = ptr.Deref(actualFIP.Properties.Subnet.Name, "")
		}
		actualFIPText := fmt.Sprintf("%s/%s ", ptr.Deref(actualFIP.Name, ""), actualSubnetName)
		description = description + actualFIPText
	}
	return description
}

func validatePublicIPs(t *testing.T, pips []*armnetwork.PublicIPAddress, service *v1.Service, wantLb bool) {
	for _, pip := range pips {
		validatePublicIP(t, pip, service, wantLb)
	}
}

func validatePublicIP(t *testing.T, publicIP *armnetwork.PublicIPAddress, service *v1.Service, wantLb bool) {
	isInternal := requiresInternalLoadBalancer(service)
	if isInternal || !wantLb {
		if publicIP != nil {
			t.Errorf("Expected publicIP resource to be nil, when it is an internal service or doesn't want LB")
		}
		return
	}

	// For external service
	if publicIP == nil {
		t.Fatal("Expected publicIP resource exists, when it is not an internal service")
	}

	if _, ok := publicIP.Tags[consts.ServiceTagKey]; !ok {
		t.Fatalf("Expected publicIP resource does not have tags[%s]", consts.ServiceTagKey)
	}

	serviceName := getServiceName(service)
	assert.Equalf(t, serviceName, ptr.Deref(publicIP.Tags[consts.ServiceTagKey], ""),
		"Expected publicIP resource has matching tags[%s]", consts.ServiceTagKey)
	assert.NotNilf(t, publicIP.Tags[consts.ClusterNameKey], "Expected publicIP resource does not have tags[%s]", consts.ClusterNameKey)
	assert.Equalf(t, testClusterName, ptr.Deref(publicIP.Tags[consts.ClusterNameKey], ""),
		"Expected publicIP resource has matching tags[%s]", consts.ClusterNameKey)

	// We cannot use Service LoadBalancerIP to compare with
	// Public IP's IPAddress
	// Because service properties are updated outside of cloudprovider code
}

func TestGetNextAvailablePriority(t *testing.T) {
	rules50, rulesTooMany := []*armnetwork.SecurityRule{}, []*armnetwork.SecurityRule{}
	for i := int32(consts.LoadBalancerMinimumPriority); i < consts.LoadBalancerMinimumPriority+50; i++ {
		rules50 = append(rules50, &armnetwork.SecurityRule{
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Priority: ptr.To(i),
			},
		})
	}
	for i := int32(consts.LoadBalancerMinimumPriority); i < consts.LoadBalancerMaximumPriority; i++ {
		rulesTooMany = append(rulesTooMany, &armnetwork.SecurityRule{
			Properties: &armnetwork.SecurityRulePropertiesFormat{
				Priority: ptr.To(i),
			},
		})
	}

	testcases := []struct {
		desc             string
		rules            []*armnetwork.SecurityRule
		lastPriority     int32
		expectErr        bool
		expectedPriority int32
	}{
		{
			desc:             "50 rules",
			rules:            rules50,
			lastPriority:     consts.LoadBalancerMinimumPriority - 1,
			expectedPriority: consts.LoadBalancerMinimumPriority + 50,
		},
		{
			desc:         "too many rules",
			rules:        rulesTooMany,
			lastPriority: consts.LoadBalancerMinimumPriority - 1,
			expectErr:    true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			priority, err := getNextAvailablePriority(tc.rules)
			if tc.expectErr {
				assert.NotNil(t, err)
			} else {
				assert.Nil(t, err)
				assert.Equal(t, tc.expectedPriority, priority)
			}
		})
	}
}

func TestProtocolTranslationTCP(t *testing.T) {
	proto := v1.ProtocolTCP
	transportProto, securityGroupProto, probeProto, err := getProtocolsFromKubernetesProtocol(proto)
	if err != nil {
		t.Error(err)
	}

	if *transportProto != armnetwork.TransportProtocolTCP {
		t.Errorf("Expected TCP LoadBalancer Rule Protocol. Got %v", transportProto)
	}
	if *securityGroupProto != armnetwork.SecurityRuleProtocolTCP {
		t.Errorf("Expected TCP SecurityGroup Protocol. Got %v", transportProto)
	}
	if *probeProto != armnetwork.ProbeProtocolTCP {
		t.Errorf("Expected TCP LoadBalancer Probe Protocol. Got %v", transportProto)
	}
}

func TestProtocolTranslationUDP(t *testing.T) {
	proto := v1.ProtocolUDP
	transportProto, securityGroupProto, probeProto, _ := getProtocolsFromKubernetesProtocol(proto)
	if *transportProto != armnetwork.TransportProtocolUDP {
		t.Errorf("Expected UDP LoadBalancer Rule Protocol. Got %v", transportProto)
	}
	if *securityGroupProto != armnetwork.SecurityRuleProtocolUDP {
		t.Errorf("Expected UDP SecurityGroup Protocol. Got %v", transportProto)
	}
	if probeProto != nil {
		t.Errorf("Expected UDP LoadBalancer Probe Protocol. Got %v", transportProto)
	}
}

// Test Configuration deserialization (json)
func TestNewCloudFromJSON(t *testing.T) {
	// Fake values for testing.
	config := `{
		"tenantId": "--tenant-id--",
		"subscriptionId": "--subscription-id--",
		"aadClientId": "--aad-client-id--",
		"aadClientSecret": "--aad-client-secret--",
		"aadClientCertPath": "--aad-client-cert-path--",
		"aadClientCertPassword": "--aad-client-cert-password--",
		"resourceGroup": "--resource-group--",
		"routeTableResourceGroup": "--route-table-resource-group--",
		"securityGroupResourceGroup": "--security-group-resource-group--",
		"privateLinkServiceResourceGroup": "--private-link-service-resource-group--",
		"location": "--location--",
		"subnetName": "--subnet-name--",
		"securityGroupName": "--security-group-name--",
		"vnetName": "--vnet-name--",
		"routeTableName": "--route-table-name--",
		"primaryAvailabilitySetName": "--primary-availability-set-name--",
		"cloudProviderBackoff": true,
		"cloudProviderRatelimit": true,
		"cloudProviderRateLimitQPS": 0.5,
		"cloudProviderRateLimitBucket": 5,
		"availabilitySetsCacheTTLInSeconds": 100,
		"nonVmssUniformNodesCacheTTLInSeconds": 100,
		"vmssCacheTTLInSeconds": 100,
		"vmssVirtualMachinesCacheTTLInSeconds": 100,
		"vmCacheTTLInSeconds": 100,
		"loadBalancerCacheTTLInSeconds": 100,
		"nsgCacheTTLInSeconds": 100,
		"routeTableCacheTTLInSeconds": 100,
		"publicIPCacheTTLInSeconds": 100,
		"plsCacheTTLInSeconds": 100,
		"vmType": "vmss",
		"disableAvailabilitySetNodes": true
	}`
	os.Setenv("AZURE_FEDERATED_TOKEN_FILE", "--aad-federated-token-file--")
	defer func() {
		os.Unsetenv("AZURE_FEDERATED_TOKEN_FILE")
	}()
	validateConfig(t, config)
}

// Test Backoff and Rate Limit defaults (json)
func TestCloudDefaultConfigFromJSON(t *testing.T) {
	config := `{
								"tenantId": "--tenant-id--",
                "aadClientId": "--aad-client-id--",
                "aadClientSecret": "--aad-client-secret--"
        }`

	validateEmptyConfig(t, config)
}

// Test Backoff and Rate Limit defaults (yaml)
func TestCloudDefaultConfigFromYAML(t *testing.T) {
	config := `
tenantId: --tenant-id--
aadClientId: --aad-client-id--
aadClientSecret: --aad-client-secret--
`
	validateEmptyConfig(t, config)
}

// Test Configuration deserialization (yaml) without
// specific resource group for the route table
func TestNewCloudFromYAML(t *testing.T) {
	config := `
tenantId: --tenant-id--
subscriptionId: --subscription-id--
aadClientId: --aad-client-id--
aadClientSecret: --aad-client-secret--
aadClientCertPath: --aad-client-cert-path--
aadClientCertPassword: --aad-client-cert-password--
resourceGroup: --resource-group--
routeTableResourceGroup: --route-table-resource-group--
securityGroupResourceGroup: --security-group-resource-group--
privateLinkServiceResourceGroup: --private-link-service-resource-group--
location: --location--
subnetName: --subnet-name--
securityGroupName: --security-group-name--
vnetName: --vnet-name--
routeTableName: --route-table-name--
primaryAvailabilitySetName: --primary-availability-set-name--
cloudProviderBackoff: true
cloudProviderBackoffRetries: 6
cloudProviderBackoffExponent: 1.5
cloudProviderBackoffDuration: 5
cloudProviderBackoffJitter: 1.0
cloudProviderRatelimit: true
cloudProviderRateLimitQPS: 0.5
cloudProviderRateLimitBucket: 5
availabilitySetsCacheTTLInSeconds: 100
nonVmssUniformNodesCacheTTLInSeconds: 100
vmssCacheTTLInSeconds: 100
vmssVirtualMachinesCacheTTLInSeconds: 100
vmCacheTTLInSeconds: 100
loadBalancerCacheTTLInSeconds: 100
nsgCacheTTLInSeconds: 100
routeTableCacheTTLInSeconds: 100
publicIPCacheTTLInSeconds: 100
plsCacheTTLInSeconds: 100
vmType: vmss
disableAvailabilitySetNodes: true
`
	os.Setenv("AZURE_FEDERATED_TOKEN_FILE", "--aad-federated-token-file--")
	defer func() {
		os.Unsetenv("AZURE_FEDERATED_TOKEN_FILE")
	}()
	validateConfig(t, config)
}

func validateConfig(t *testing.T, config string) { //nolint
	azureCloud := getCloudFromConfig(t, config)

	if azureCloud.TenantID != "--tenant-id--" {
		t.Errorf("got incorrect value for TenantID")
	}
	if azureCloud.SubscriptionID != "--subscription-id--" {
		t.Errorf("got incorrect value for SubscriptionID")
	}
	if azureCloud.AADClientID != "--aad-client-id--" {
		t.Errorf("got incorrect value for AADClientID")
	}
	if azureCloud.AADClientSecret != "--aad-client-secret--" {
		t.Errorf("got incorrect value for AADClientSecret")
	}
	if azureCloud.AADClientCertPath != "--aad-client-cert-path--" {
		t.Errorf("got incorrect value for AADClientCertPath")
	}
	if azureCloud.AADClientCertPassword != "--aad-client-cert-password--" {
		t.Errorf("got incorrect value for AADClientCertPassword")
	}
	if azureCloud.ResourceGroup != "--resource-group--" {
		t.Errorf("got incorrect value for ResourceGroup")
	}
	if azureCloud.RouteTableResourceGroup != "--route-table-resource-group--" {
		t.Errorf("got incorrect value for RouteTableResourceGroup")
	}
	if azureCloud.SecurityGroupResourceGroup != "--security-group-resource-group--" {
		t.Errorf("got incorrect value for SecurityGroupResourceGroup")
	}
	if azureCloud.PrivateLinkServiceResourceGroup != "--private-link-service-resource-group--" {
		t.Errorf("got incorrect value for PrivateLinkResourceGroup")
	}
	if azureCloud.Location != "--location--" {
		t.Errorf("got incorrect value for Location")
	}
	if azureCloud.SubnetName != "--subnet-name--" {
		t.Errorf("got incorrect value for SubnetName")
	}
	if azureCloud.SecurityGroupName != "--security-group-name--" {
		t.Errorf("got incorrect value for SecurityGroupName")
	}
	if azureCloud.VnetName != "--vnet-name--" {
		t.Errorf("got incorrect value for VnetName")
	}
	if azureCloud.RouteTableName != "--route-table-name--" {
		t.Errorf("got incorrect value for RouteTableName")
	}
	if azureCloud.PrimaryAvailabilitySetName != "--primary-availability-set-name--" {
		t.Errorf("got incorrect value for PrimaryAvailabilitySetName")
	}
	if azureCloud.CloudProviderBackoff != true {
		t.Errorf("got incorrect value for CloudProviderBackoff")
	}
	if azureCloud.CloudProviderBackoffRetries != 6 {
		t.Errorf("got incorrect value for CloudProviderBackoffRetries")
	}
	if azureCloud.CloudProviderBackoffExponent != 1.5 {
		t.Errorf("got incorrect value for CloudProviderBackoffExponent")
	}
	if azureCloud.CloudProviderBackoffDuration != 5 {
		t.Errorf("got incorrect value for CloudProviderBackoffDuration")
	}
	if azureCloud.CloudProviderBackoffJitter != 1.0 {
		t.Errorf("got incorrect value for CloudProviderBackoffJitter")
	}
	if azureCloud.CloudProviderRateLimit != true {
		t.Errorf("got incorrect value for CloudProviderRateLimit")
	}
	if azureCloud.CloudProviderRateLimitQPS != 0.5 {
		t.Errorf("got incorrect value for CloudProviderRateLimitQPS")
	}
	if azureCloud.CloudProviderRateLimitBucket != 5 {
		t.Errorf("got incorrect value for CloudProviderRateLimitBucket")
	}
	if azureCloud.AvailabilitySetsCacheTTLInSeconds != 100 {
		t.Errorf("got incorrect value for AvailabilitySetsCacheTTLInSeconds")
	}
	if azureCloud.NonVmssUniformNodesCacheTTLInSeconds != 100 {
		t.Errorf("got incorrect value for nonVmssUniformNodesCacheTTLInSeconds")
	}
	if azureCloud.VmssCacheTTLInSeconds != 100 {
		t.Errorf("got incorrect value for vmssCacheTTLInSeconds")
	}
	if azureCloud.VmssVirtualMachinesCacheTTLInSeconds != 100 {
		t.Errorf("got incorrect value for vmssVirtualMachinesCacheTTLInSeconds")
	}
	if azureCloud.VMCacheTTLInSeconds != 100 {
		t.Errorf("got incorrect value for vmCacheTTLInSeconds")
	}
	if azureCloud.LoadBalancerCacheTTLInSeconds != 100 {
		t.Errorf("got incorrect value for loadBalancerCacheTTLInSeconds")
	}
	if azureCloud.NsgCacheTTLInSeconds != 100 {
		t.Errorf("got incorrect value for nsgCacheTTLInSeconds")
	}
	if azureCloud.RouteTableCacheTTLInSeconds != 100 {
		t.Errorf("got incorrect value for routeTableCacheTTLInSeconds")
	}
	if azureCloud.PublicIPCacheTTLInSeconds != 100 {
		t.Errorf("got incorrect value for publicIPCacheTTLInSeconds")
	}
	if azureCloud.PlsCacheTTLInSeconds != 100 {
		t.Errorf("got incorrect value for plsCacheTTLInSeconds")
	}
	if azureCloud.VMType != consts.VMTypeVMSS {
		t.Errorf("got incorrect value for vmType")
	}
	if !azureCloud.DisableAvailabilitySetNodes {
		t.Errorf("got incorrect value for disableAvailabilitySetNodes")
	}
	if azureCloud.AADFederatedTokenFile != "--aad-federated-token-file--" {
		t.Errorf("got incorrect value for AADFederatedTokenFile")
	}
	if !azureCloud.UseFederatedWorkloadIdentityExtension {
		t.Errorf("got incorrect value for UseFederatedWorkloadIdentityExtension")
	}
}

func getCloudFromConfig(t *testing.T, configContent string) *Cloud {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	configReader := strings.NewReader(configContent)
	c, err := config.ParseConfig(configReader)
	assert.NoError(t, err)

	az := GetTestCloud(ctrl)
	zoneMock := az.zoneRepo.(*zone.MockRepository)
	zoneMock.EXPECT().ListZones(gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil).AnyTimes()

	// Skip AAD client cert path validation since it will read the file from the path
	aadCertPath := c.AADClientCertPath
	c.AADClientCertPath = ""

	err = az.InitializeCloudFromConfig(context.Background(), c, false, true)
	assert.NoError(t, err)

	az.AADClientCertPath = aadCertPath

	return az
}

// TODO include checks for other appropriate default config parameters
func validateEmptyConfig(t *testing.T, config string) {
	azureCloud := getCloudFromConfig(t, config)

	// backoff should be disabled by default if not explicitly enabled in config
	if azureCloud.CloudProviderBackoff != false {
		t.Errorf("got incorrect value for CloudProviderBackoff")
	}
	// rate limits should be disabled by default if not explicitly enabled in config
	if azureCloud.CloudProviderRateLimit != false {
		t.Errorf("got incorrect value for CloudProviderRateLimit")
	}
}

func TestGetNodeNameByProviderID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	providers := []struct {
		providerID string
		name       types.NodeName

		fail bool
	}{
		{
			providerID: consts.CloudProviderName + ":///subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-0",
			name:       "k8s-agent-AAAAAAAA-0",
			fail:       false,
		},
		{
			providerID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-1",
			name:       "k8s-agent-AAAAAAAA-1",
			fail:       false,
		},
		{
			providerID: consts.CloudProviderName + ":/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-0",
			name:       "k8s-agent-AAAAAAAA-0",
			fail:       false,
		},
		{
			providerID: consts.CloudProviderName + "://",
			name:       "",
			fail:       true,
		},
		{
			providerID: ":///subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-0",
			name:       "k8s-agent-AAAAAAAA-0",
			fail:       false,
		},
		{
			providerID: "aws:///subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroupName/providers/Microsoft.Compute/virtualMachines/k8s-agent-AAAAAAAA-0",
			name:       "k8s-agent-AAAAAAAA-0",
			fail:       false,
		},
	}

	for _, test := range providers {
		name, err := az.VMSet.GetNodeNameByProviderID(context.TODO(), test.providerID)
		if (err != nil) != test.fail {
			t.Errorf("Expected to fail=%t, with pattern %v", test.fail, test)
		}

		if test.fail {
			continue
		}

		if name != test.name {
			t.Errorf("Expected %v, but got %v", test.name, name)
		}

	}
}

func validateTestSubnet(t *testing.T, az *Cloud, svc *v1.Service) {
	if svc.Annotations[consts.ServiceAnnotationLoadBalancerInternal] != consts.TrueAnnotationValue {
		t.Error("Subnet added to non-internal service")
	}

	subName := svc.Annotations[consts.ServiceAnnotationLoadBalancerInternalSubnet]
	if subName == "" {
		subName = az.SubnetName
	}
	subnetID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s/subnets/%s",
		az.SubscriptionID,
		az.VnetResourceGroup,
		az.VnetName,
		subName)
	mockSubnetsClient := az.subnetRepo.(*subnet.MockRepository)
	mockSubnetsClient.EXPECT().Get(gomock.Any(), az.VnetResourceGroup, az.VnetName, subName).Return(
		&armnetwork.Subnet{
			ID:   &subnetID,
			Name: &subName,
		}, nil).AnyTimes()
}

func TestGetResourceGroups(t *testing.T) {
	tests := []struct {
		name               string
		nodeResourceGroups map[string]string
		expected           *utilsets.IgnoreCaseSet
		informerSynced     bool
		expectError        bool
	}{
		{
			name:               "cloud provider configured RG should be returned by default",
			nodeResourceGroups: map[string]string{},
			informerSynced:     true,
			expected:           utilsets.NewString("rg"),
		},
		{
			name:               "cloud provider configured RG and node RGs should be returned",
			nodeResourceGroups: map[string]string{"node1": "rg1", "node2": "rg2"},
			informerSynced:     true,
			expected:           utilsets.NewString("rg", "rg1", "rg2"),
		},
		{
			name:               "error should be returned if informer hasn't synced yet",
			nodeResourceGroups: map[string]string{"node1": "rg1", "node2": "rg2"},
			informerSynced:     false,
			expectError:        true,
		},
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	for _, test := range tests {
		az.nodeResourceGroups = test.nodeResourceGroups
		if test.informerSynced {
			az.nodeInformerSynced = func() bool { return true }
		} else {
			az.nodeInformerSynced = func() bool { return false }
		}
		actual, err := az.GetResourceGroups()
		if test.expectError {
			assert.NotNil(t, err, test.name)
			continue
		}

		assert.Nil(t, err, test.name)
		assert.Equal(t, test.expected, actual, test.name)
	}
}

func TestGetNodeResourceGroup(t *testing.T) {
	tests := []struct {
		name               string
		nodeResourceGroups map[string]string
		node               string
		expected           string
		informerSynced     bool
		expectError        bool
	}{
		{
			name:               "cloud provider configured RG should be returned by default",
			nodeResourceGroups: map[string]string{},
			informerSynced:     true,
			node:               "node1",
			expected:           "rg",
		},
		{
			name:               "node RGs should be returned",
			nodeResourceGroups: map[string]string{"node1": "rg1", "node2": "rg2"},
			informerSynced:     true,
			node:               "node1",
			expected:           "rg1",
		},
		{
			name:               "error should be returned if informer hasn't synced yet",
			nodeResourceGroups: map[string]string{"node1": "rg1", "node2": "rg2"},
			informerSynced:     false,
			expectError:        true,
		},
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	for _, test := range tests {
		az.nodeResourceGroups = test.nodeResourceGroups
		if test.informerSynced {
			az.nodeInformerSynced = func() bool { return true }
		} else {
			az.nodeInformerSynced = func() bool { return false }
		}
		actual, err := az.GetNodeResourceGroup(test.node)
		if test.expectError {
			assert.NotNil(t, err, test.name)
			continue
		}

		assert.Nil(t, err, test.name)
		assert.Equal(t, test.expected, actual, test.name)
	}
}

func TestSetInformers(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	az.nodeInformerSynced = nil

	sharedInformers := informers.NewSharedInformerFactory(az.KubeClient, time.Minute)
	az.SetInformers(sharedInformers)
	assert.NotNil(t, az.nodeInformerSynced)
}

func TestUpdateNodeCaches(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	// delete node appearing in unmanagedNodes and excludeLoadBalancerNodes
	zone := fmt.Sprintf("%s-0", az.Location)
	nodesInZone := utilsets.NewString("prevNode")
	az.nodeZones = map[string]*utilsets.IgnoreCaseSet{zone: nodesInZone}
	az.nodeResourceGroups = map[string]string{"prevNode": "rg"}
	az.unmanagedNodes = utilsets.NewString("prevNode")
	az.excludeLoadBalancerNodes = utilsets.NewString("prevNode")
	az.nodeNames = utilsets.NewString("prevNode")

	prevNode := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.LabelTopologyZone:              zone,
				consts.ExternalResourceGroupLabel: consts.TrueAnnotationValue,
				consts.ManagedByAzureLabel:        "false",
			},
			Name: "prevNode",
		},
	}

	az.updateNodeCaches(&prevNode, nil)
	assert.Equal(t, 0, az.nodeZones[zone].Len())
	assert.Equal(t, 0, len(az.nodeResourceGroups))
	assert.Equal(t, 0, az.unmanagedNodes.Len())
	assert.Equal(t, 1, az.excludeLoadBalancerNodes.Len())
	assert.Equal(t, 0, az.nodeNames.Len())

	// add new (unmanaged and to-be-excluded) node
	newNode := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.LabelTopologyZone:              zone,
				consts.ExternalResourceGroupLabel: consts.TrueAnnotationValue,
				consts.ManagedByAzureLabel:        "false",
				v1.LabelNodeExcludeBalancers:      "true",
			},
			Name: "newNode",
		},
	}

	az.updateNodeCaches(nil, &newNode)
	assert.Equal(t, 1, az.nodeZones[zone].Len())
	assert.Equal(t, 1, len(az.nodeResourceGroups))
	assert.Equal(t, 1, az.unmanagedNodes.Len())
	assert.Equal(t, 2, az.excludeLoadBalancerNodes.Len())
	assert.Equal(t, 1, az.nodeNames.Len())
}

func TestUpdateNodeTaint(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	tests := []struct {
		desc     string
		node     *v1.Node
		hasTaint bool
	}{
		{
			desc: "ready node without taint should not have taint",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			hasTaint: false,
		},
		{
			desc: "ready node with taint should remove the taint",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{*nodeOutOfServiceTaint},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			hasTaint: false,
		},
		{
			desc: "not-ready node without shutdown taint should not have out-of-service taint",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			hasTaint: false,
		},
		{
			desc: "not-ready node with shutdown taint should have out-of-service taint",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{*nodeShutdownTaint},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			hasTaint: true,
		},
		{
			desc: "not-ready node with out-of-service taint should keep it",
			node: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node",
				},
				Spec: v1.NodeSpec{
					Taints: []v1.Taint{*nodeOutOfServiceTaint},
				},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			hasTaint: true,
		},
	}
	for _, test := range tests {
		cs := fake.NewSimpleClientset(test.node)
		az.KubeClient = cs
		az.updateNodeTaint(test.node)
		newNode, _ := cs.CoreV1().Nodes().Get(context.Background(), "node", metav1.GetOptions{})
		assert.Equal(t, test.hasTaint, taints.TaintExists(newNode.Spec.Taints, nodeOutOfServiceTaint), test.desc)
	}
}

func TestUpdateNodeCacheExcludeLoadBalancer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	zone := fmt.Sprintf("%s-0", az.Location)
	nodesInZone := utilsets.NewString("aNode")
	az.nodeZones = map[string]*utilsets.IgnoreCaseSet{zone: nodesInZone}
	az.nodeResourceGroups = map[string]string{"aNode": "rg"}

	// a non-ready node should be included
	az.unmanagedNodes = utilsets.NewString()
	az.excludeLoadBalancerNodes = utilsets.NewString()
	az.nodeNames = utilsets.NewString()
	nonReadyNode := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.LabelTopologyZone: zone,
			},
			Name: "aNode",
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{
					Type:   v1.NodeReady,
					Status: v1.ConditionFalse,
				},
			},
		},
	}
	az.updateNodeCaches(nil, &nonReadyNode)
	assert.Equal(t, 0, az.excludeLoadBalancerNodes.Len())

	// node becomes ready => no impact on az.excludeLoadBalancerNodes
	readyNode := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.LabelTopologyZone: zone,
			},
			Name: "aNode",
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{
					Type:   v1.NodeReady,
					Status: v1.ConditionTrue,
				},
			},
		},
	}
	az.updateNodeCaches(&nonReadyNode, &readyNode)
	assert.Equal(t, 0, az.excludeLoadBalancerNodes.Len())

	// new non-ready node with taint is added to the cluster: don't exclude it
	nonReadyTaintedNode := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				v1.LabelTopologyZone: zone,
			},
			Name: "anotherNode",
		},
		Spec: v1.NodeSpec{
			Taints: []v1.Taint{{
				Key:    cloudproviderapi.TaintExternalCloudProvider,
				Value:  "aValue",
				Effect: v1.TaintEffectNoSchedule},
			},
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{
				{
					Type:   v1.NodeReady,
					Status: v1.ConditionFalse,
				},
			},
		},
	}
	az.updateNodeCaches(nil, &nonReadyTaintedNode)
	assert.Equal(t, 0, az.excludeLoadBalancerNodes.Len())
}

func TestGetActiveZones(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	az.nodeInformerSynced = nil
	zones, err := az.GetActiveZones()
	expectedErr := fmt.Errorf("azure cloud provider doesn't have informers set")
	assert.Equal(t, expectedErr, err)
	assert.Nil(t, zones)

	az.nodeInformerSynced = func() bool { return false }
	zones, err = az.GetActiveZones()
	expectedErr = fmt.Errorf("node informer is not synced when trying to GetActiveZones")
	assert.Equal(t, expectedErr, err)
	assert.Nil(t, zones)

	az.nodeInformerSynced = func() bool { return true }
	zone := fmt.Sprintf("%s-0", az.Location)
	nodesInZone := utilsets.NewString("node1")
	az.nodeZones = map[string]*utilsets.IgnoreCaseSet{zone: nodesInZone}

	expectedZones := utilsets.NewString(zone)
	zones, err = az.GetActiveZones()
	assert.Equal(t, expectedZones, zones)
	assert.NoError(t, err)
}

func TestInitializeCloudFromConfig(t *testing.T) {

	t.Run("cannot initialize from nil config", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		az := GetTestCloud(ctrl)
		zoneMock := az.zoneRepo.(*zone.MockRepository)
		zoneMock.EXPECT().ListZones(gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil).AnyTimes()

		err := az.InitializeCloudFromConfig(context.Background(), nil, false, true)
		assert.Equal(t, fmt.Errorf("InitializeCloudFromConfig: cannot initialize from nil config"), err)

	})
	t.Run("disableAvailabilitySetNodes true is only supported when vmType is 'vmss'", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		az := GetTestCloud(ctrl)
		zoneMock := az.zoneRepo.(*zone.MockRepository)
		zoneMock.EXPECT().ListZones(gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil).AnyTimes()

		azureconfig := config.Config{
			DisableAvailabilitySetNodes: true,
			VMType:                      consts.VMTypeStandard,
		}
		err := az.InitializeCloudFromConfig(context.Background(), &azureconfig, false, true)
		expectedErr := fmt.Errorf("disableAvailabilitySetNodes true is only supported when vmType is 'vmss'")
		assert.Equal(t, expectedErr, err)
	})
	t.Run("useInstanceMetadata must be enabled without Azure credentials", func(t *testing.T) {

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		az := GetTestCloud(ctrl)
		zoneMock := az.zoneRepo.(*zone.MockRepository)
		zoneMock.EXPECT().ListZones(gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil).AnyTimes()

		azureconfig := config.Config{
			AzureClientConfig: providerconfig.AzureClientConfig{
				ARMClientConfig: azclient.ARMClientConfig{
					Cloud: "AZUREPUBLICCLOUD",
				},
			},
			CloudConfigType: configloader.CloudConfigTypeFile,
		}
		az.AuthProvider = &azclient.AuthProvider{}
		err := az.InitializeCloudFromConfig(context.Background(), &azureconfig, false, true)
		expectedErr := fmt.Errorf("useInstanceMetadata must be enabled without Azure credentials")
		assert.Equal(t, expectedErr, err)
	})
	t.Run("loadBalancerBackendPoolConfigurationType invalid is not supported, supported values are", func(t *testing.T) {

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		az := GetTestCloud(ctrl)
		zoneMock := az.zoneRepo.(*zone.MockRepository)
		zoneMock.EXPECT().ListZones(gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil).AnyTimes()

		azureconfig := config.Config{
			LoadBalancerBackendPoolConfigurationType: "invalid",
		}
		err := az.InitializeCloudFromConfig(context.Background(), &azureconfig, false, true)
		expectedErr := errors.New("loadBalancerBackendPoolConfigurationType invalid is not supported, supported values are")
		assert.Contains(t, err.Error(), expectedErr.Error())
	})
	t.Run("loadBalancerBackendPoolConfigurationType is set to NodeIPConfiguration", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		az := GetTestCloud(ctrl)
		zoneMock := az.zoneRepo.(*zone.MockRepository)
		zoneMock.EXPECT().ListZones(gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil).AnyTimes()

		azureconfig := config.Config{}
		err := az.InitializeCloudFromConfig(context.Background(), &azureconfig, false, true)
		assert.NoError(t, err)
		assert.Equal(t, az.Config.LoadBalancerBackendPoolConfigurationType, consts.LoadBalancerBackendPoolConfigurationTypeNodeIPConfiguration)
	})

	t.Run("should setup network client factory with network subscription ID - same network sub in same tenant", func(t *testing.T) {

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		az := GetTestCloud(ctrl)
		zoneMock := az.zoneRepo.(*zone.MockRepository)
		zoneMock.EXPECT().ListZones(gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil).AnyTimes()

		const (
			tenantID       = "tenant-id"
			subscriptionID = "subscription-id"
		)

		az.ComputeClientFactory = nil
		az.NetworkClientFactory = nil

		azureconfig := config.Config{}
		azureconfig.ARMClientConfig.TenantID = tenantID
		azureconfig.SubscriptionID = subscriptionID

		nCall := 0
		newARMClientFactory = func(
			config *azclient.ClientFactoryConfig,
			armConfig *azclient.ARMClientConfig,
			cloud cloud.Configuration,
			cred azcore.TokenCredential,
			clientOptionsMutFn ...func(option *arm.ClientOptions),
		) (azclient.ClientFactory, error) {
			switch nCall {
			case 0:
				// It should create network client factory
				assert.Equal(t, subscriptionID, config.SubscriptionID)
			case 1:
				// It should create compute client factory
				assert.Equal(t, subscriptionID, config.SubscriptionID)
			default:
				panic("unexpected call")
			}
			nCall++
			return azclient.NewClientFactory(config, armConfig, cloud, cred, clientOptionsMutFn...)
		}
		defer func() {
			newARMClientFactory = azclient.NewClientFactory
		}()

		err := az.InitializeCloudFromConfig(context.Background(), &azureconfig, false, true)
		assert.NoError(t, err)
	})

	t.Run("should setup network client factory with network subscription ID - different network sub in same tenant", func(t *testing.T) {

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		az := GetTestCloud(ctrl)
		zoneMock := az.zoneRepo.(*zone.MockRepository)
		zoneMock.EXPECT().ListZones(gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil).AnyTimes()

		const (
			tenantID              = "tenant-id"
			networkSubscriptionID = "network-subscription-id"
			computeSubscriptionID = "compute-subscription-id"
		)

		az.ComputeClientFactory = nil
		az.NetworkClientFactory = nil

		azureconfig := config.Config{}
		azureconfig.ARMClientConfig.TenantID = tenantID
		azureconfig.ARMClientConfig.NetworkResourceTenantID = tenantID
		azureconfig.NetworkResourceSubscriptionID = networkSubscriptionID
		azureconfig.SubscriptionID = computeSubscriptionID

		nCall := 0
		newARMClientFactory = func(
			config *azclient.ClientFactoryConfig,
			armConfig *azclient.ARMClientConfig,
			cloud cloud.Configuration,
			cred azcore.TokenCredential,
			clientOptionsMutFn ...func(option *arm.ClientOptions),
		) (azclient.ClientFactory, error) {
			switch nCall {
			case 0:
				// It should create network client factory
				assert.Equal(t, networkSubscriptionID, config.SubscriptionID)
			case 1:
				// It should create compute client factory
				assert.Equal(t, computeSubscriptionID, config.SubscriptionID)
			default:
				panic("unexpected call")
			}
			nCall++
			return azclient.NewClientFactory(config, armConfig, cloud, cred, clientOptionsMutFn...)
		}
		defer func() {
			newARMClientFactory = azclient.NewClientFactory
		}()

		err := az.InitializeCloudFromConfig(context.Background(), &azureconfig, false, true)
		assert.NoError(t, err)
	})

	t.Run("should setup network client factory with network subscription ID - different network sub in different tenant", func(t *testing.T) {

		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		az := GetTestCloud(ctrl)
		zoneMock := az.zoneRepo.(*zone.MockRepository)
		zoneMock.EXPECT().ListZones(gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil).AnyTimes()

		const (
			tenantID              = "tenant-id"
			networkTenantID       = "network-tenant-id"
			networkSubscriptionID = "network-subscription-id"
			computeSubscriptionID = "compute-subscription-id"
		)

		az.ComputeClientFactory = nil
		az.NetworkClientFactory = nil

		azureconfig := config.Config{}
		azureconfig.ARMClientConfig.TenantID = tenantID
		azureconfig.ARMClientConfig.NetworkResourceTenantID = networkTenantID
		azureconfig.NetworkResourceSubscriptionID = networkSubscriptionID
		azureconfig.SubscriptionID = computeSubscriptionID

		nCall := 0
		newARMClientFactory = func(
			config *azclient.ClientFactoryConfig,
			armConfig *azclient.ARMClientConfig,
			cloud cloud.Configuration,
			cred azcore.TokenCredential,
			clientOptionsMutFn ...func(option *arm.ClientOptions),
		) (azclient.ClientFactory, error) {
			switch nCall {
			case 0:
				// It should create network client factory
				assert.Equal(t, networkSubscriptionID, config.SubscriptionID)
			case 1:
				// It should create compute client factory
				assert.Equal(t, computeSubscriptionID, config.SubscriptionID)
			default:
				panic("unexpected call")
			}
			nCall++
			return azclient.NewClientFactory(config, armConfig, cloud, cred, clientOptionsMutFn...)
		}
		defer func() {
			newARMClientFactory = azclient.NewClientFactory
		}()

		err := az.InitializeCloudFromConfig(context.Background(), &azureconfig, false, true)
		assert.NoError(t, err)
	})
}

func TestSetLBDefaults(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	config := &config.Config{}
	_ = az.setLBDefaults(config)
	assert.Equal(t, config.LoadBalancerSKU, consts.LoadBalancerSKUStandard)
}

func TestCheckEnableMultipleStandardLoadBalancers(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	az.LoadBalancerBackendPoolConfigurationType = consts.LoadBalancerBackendPoolConfigurationTypeNodeIP

	az.MultipleStandardLoadBalancerConfigurations = []config.MultipleStandardLoadBalancerConfiguration{
		{
			Name: "kubernetes",
			MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
				PrimaryVMSet: "vmss-0",
			},
		},
		{
			Name: "lb1",
			MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
				PrimaryVMSet: "vmss-1",
			},
		},
		{
			Name: "kubernetes",
			MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
				PrimaryVMSet: "vmss-2",
			},
		},
	}

	err := az.checkEnableMultipleStandardLoadBalancers()
	assert.Equal(t, "duplicated multiple standard load balancer configuration name kubernetes", err.Error())

	az.MultipleStandardLoadBalancerConfigurations = []config.MultipleStandardLoadBalancerConfiguration{
		{
			Name: "kubernetes",
			MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
				PrimaryVMSet: "vmss-0",
			},
		},
		{
			Name: "lb1",
		},
	}

	err = az.checkEnableMultipleStandardLoadBalancers()
	assert.Equal(t, "multiple standard load balancer configuration lb1 must have primary VMSet", err.Error())

	az.MultipleStandardLoadBalancerConfigurations = []config.MultipleStandardLoadBalancerConfiguration{
		{
			Name: "kubernetes",
			MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
				PrimaryVMSet: "vmss-0",
			},
		},
		{
			Name: "lb1",
			MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
				PrimaryVMSet: "vmss-2",
			},
		},
		{
			Name: "lb2",
			MultipleStandardLoadBalancerConfigurationSpec: config.MultipleStandardLoadBalancerConfigurationSpec{
				PrimaryVMSet: "vmss-2",
			},
		},
	}

	err = az.checkEnableMultipleStandardLoadBalancers()
	assert.Equal(t, "duplicated primary VMSet vmss-2 in multiple standard load balancer configurations lb2", err.Error())
}

func TestIsNodeReady(t *testing.T) {
	tests := []struct {
		name     string
		node     *v1.Node
		expected bool
	}{
		{
			node:     nil,
			expected: false,
		},
		{
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			expected: true,
		},
		{
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeReady,
							Status: v1.ConditionFalse,
						},
					},
				},
			},
			expected: false,
		},
		{
			node: &v1.Node{
				Status: v1.NodeStatus{},
			},
			expected: false,
		},
		{
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{
							Type:   v1.NodeMemoryPressure,
							Status: v1.ConditionTrue,
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, test := range tests {
		if got := isNodeReady(test.node); got != test.expected {
			assert.Equal(t, test.expected, got)
		}
	}
}
