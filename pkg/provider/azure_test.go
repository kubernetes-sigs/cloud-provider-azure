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
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-03-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	cloudprovider "k8s.io/cloud-provider"
	cloudproviderapi "k8s.io/cloud-provider/api"
	servicehelpers "k8s.io/cloud-provider/service/helpers"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/interfaceclient/mockinterfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/loadbalancerclient/mockloadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privatelinkserviceclient/mockprivatelinkserviceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/publicipclient/mockpublicipclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/securitygroupclient/mocksecuritygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/subnetclient/mocksubnetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/zoneclient/mockzoneclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

const (
	testClusterName = "testCluster"
	testIP1         = "192.168.77.88"
	testIP2         = "192.168.33.44"
	testIP3         = "192.168.99.11"
	testRuleName    = "shared-TCP-80-Internet"
	testRuleName2   = "shared-TCP-4444-Internet"
	testRuleName3   = "shared-TCP-8888-Internet"
	testRuleName4   = "TCP-80-Internet"
)

// clusterIPFamiliesEnabled checks cluster IP families.
func (az *Cloud) clusterIPFamiliesEnabled() (v4Enabled bool, v6Enabled bool) {
	if az.IPFamily == DualStack || az.IPFamily == IPv4 {
		v4Enabled = true
	}
	if az.IPFamily == DualStack || az.IPFamily == IPv6 {
		v6Enabled = true
	}
	return
}

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
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	svc.Spec.Ports = append(svc.Spec.Ports, v1.ServicePort{
		Name:     fmt.Sprintf("port-udp-%d", 1234),
		Protocol: v1.ProtocolUDP,
		Port:     1234,
		NodePort: getBackendPort(1234),
	})

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, err := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	// ensure we got a frontend ip configuration
	if len(*lb.FrontendIPConfigurations) != 1 {
		t.Error("Expected the loadbalancer to have a frontend ip configuration")
	}

	validateLoadBalancer(t, lb, svc)
}

func TestLoadBalancerInternalServiceModeSelection(t *testing.T) {
	testLoadBalancerServiceDefaultModeSelection(t, true)
	testLoadBalancerServiceAutoModeSelection(t, true)
	testLoadBalancerServicesSpecifiedSelection(t, true)
	testLoadBalancerMaxRulesServices(t, true)
	testLoadBalancerServiceAutoModeDeleteSelection(t, true)
}

func TestLoadBalancerExternalServiceModeSelection(t *testing.T) {
	testLoadBalancerServiceDefaultModeSelection(t, false)
	testLoadBalancerServiceAutoModeSelection(t, false)
	testLoadBalancerServicesSpecifiedSelection(t, false)
	testLoadBalancerMaxRulesServices(t, false)
	testLoadBalancerServiceAutoModeDeleteSelection(t, false)
}

func setMockEnv(az *Cloud, ctrl *gomock.Controller, expectedInterfaces []network.Interface, expectedVirtualMachines []compute.VirtualMachine, serviceCount int, services ...v1.Service) {
	mockInterfacesClient := mockinterfaceclient.NewMockInterface(ctrl)
	az.InterfacesClient = mockInterfacesClient
	for i := range expectedInterfaces {
		mockInterfacesClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, fmt.Sprintf("vm-%d", i), gomock.Any()).Return(expectedInterfaces[i], nil).AnyTimes()
		mockInterfacesClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, fmt.Sprintf("vm-%d", i), gomock.Any()).Return(nil).AnyTimes()
	}

	mockVirtualMachinesClient := mockvmclient.NewMockInterface(ctrl)
	az.VirtualMachinesClient = mockVirtualMachinesClient
	mockVirtualMachinesClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(expectedVirtualMachines, nil).AnyTimes()
	for i := range expectedVirtualMachines {
		mockVirtualMachinesClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, fmt.Sprintf("vm-%d", i), gomock.Any()).Return(expectedVirtualMachines[i], nil).AnyTimes()
	}

	setMockPublicIPs(az, ctrl, serviceCount)

	sg := getTestSecurityGroup(az, services...)
	setMockSecurityGroup(az, ctrl, sg)
}

func setMockPublicIPs(az *Cloud, ctrl *gomock.Controller, serviceCount int) {
	v4Enabled, v6Enabled := az.clusterIPFamiliesEnabled()

	mockPIPsClient := mockpublicipclient.NewMockInterface(ctrl)

	expectedPIPs := []network.PublicIPAddress{}
	if v4Enabled {
		expectedPIP := setMockPublicIP(az, mockPIPsClient, serviceCount, false)
		expectedPIPs = append(expectedPIPs, expectedPIP)
	}
	if v6Enabled {
		expectedPIP := setMockPublicIP(az, mockPIPsClient, serviceCount, true)
		expectedPIPs = append(expectedPIPs, expectedPIP)
	}

	az.PublicIPAddressesClient = mockPIPsClient
	mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockPIPsClient.EXPECT().List(gomock.Any(), gomock.Not(az.ResourceGroup)).Return(nil, nil).AnyTimes()
	mockPIPsClient.EXPECT().Get(gomock.Any(), gomock.Not(az.ResourceGroup), gomock.Any(), gomock.Any()).Return(network.PublicIPAddress{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
}

func setMockPublicIP(az *Cloud, mockPIPsClient *mockpublicipclient.MockInterface, serviceCount int, isIPv6 bool) network.PublicIPAddress {
	suffix := v4Suffix
	ipVer := network.IPv4
	ipAddr := "1.2.3.4"
	if isIPv6 {
		suffix = v6Suffix
		ipVer = network.IPv6
		ipAddr = "fd00::eef0"
	}

	expectedPIP := network.PublicIPAddress{
		Name:     pointer.String("testCluster-aservicea"),
		Location: &az.Location,
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: network.Static,
			PublicIPAddressVersion:   ipVer,
			IPAddress:                pointer.String(ipAddr),
		},
		Tags: map[string]*string{
			consts.ServiceTagKey:  pointer.String("default/servicea"),
			consts.ClusterNameKey: pointer.String(testClusterName),
		},
		Sku: &network.PublicIPAddressSku{
			Name: network.PublicIPAddressSkuNameStandard,
		},
		ID: pointer.String("testCluster-aservice1"),
	}

	a := 'a'
	var expectedPIPs []network.PublicIPAddress
	for i := 1; i <= serviceCount; i++ {
		expectedPIP.Name = pointer.String(fmt.Sprintf("testCluster-aservice%d-%s", i, suffix))
		expectedPIP.Tags[consts.ServiceTagKey] = pointer.String(fmt.Sprintf("default/service%d", i))
		mockPIPsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, fmt.Sprintf("testCluster-aservice%d-%s", i, suffix), gomock.Any()).Return(expectedPIP, nil).AnyTimes()
		mockPIPsClient.EXPECT().Delete(gomock.Any(), az.ResourceGroup, fmt.Sprintf("testCluster-aservice%d-%s", i, suffix)).Return(nil).AnyTimes()
		expectedPIPs = append(expectedPIPs, expectedPIP)
		expectedPIP.Name = pointer.String(fmt.Sprintf("testCluster-aservice%c-%s", a, suffix))
		expectedPIP.Tags[consts.ServiceTagKey] = pointer.String(fmt.Sprintf("default/service%c", a))
		mockPIPsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, fmt.Sprintf("testCluster-aservice%c-%s", a, suffix), gomock.Any()).Return(expectedPIP, nil).AnyTimes()
		mockPIPsClient.EXPECT().Delete(gomock.Any(), az.ResourceGroup, fmt.Sprintf("testCluster-aservice%c-%s", a, suffix)).Return(nil).AnyTimes()
		expectedPIPs = append(expectedPIPs, expectedPIP)
		a++
	}
	mockPIPsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(expectedPIPs, nil).AnyTimes()

	return expectedPIP
}

func setMockSecurityGroup(az *Cloud, ctrl *gomock.Controller, sgs ...*network.SecurityGroup) {
	mockSGsClient := mocksecuritygroupclient.NewMockInterface(ctrl)
	az.SecurityGroupsClient = mockSGsClient
	for _, sg := range sgs {
		mockSGsClient.EXPECT().Get(gomock.Any(), az.SecurityGroupResourceGroup, az.SecurityGroupName, gomock.Any()).Return(*sg, nil).AnyTimes()
	}
	mockSGsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.SecurityGroupResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
}

func setMockLBs(az *Cloud, ctrl *gomock.Controller, expectedLBs *[]network.LoadBalancer, svcName string, lbCount, serviceIndex int, isInternal bool) string {
	v4Enabled, v6Enabled := az.clusterIPFamiliesEnabled()

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
		lb := network.LoadBalancer{
			Location: &az.Location,
			LoadBalancerPropertiesFormat: &network.LoadBalancerPropertiesFormat{
				BackendAddressPools: &[]network.BackendAddressPool{
					{
						Name: pointer.String("testCluster"),
					},
				},
			},
		}
		lb.Name = &expectedLBName
		rules := []network.LoadBalancingRule{}
		fips := []network.FrontendIPConfiguration{}
		addRuleAndFIP := func(isIPv6 bool) {
			suffix := v4Suffix
			if isIPv6 {
				suffix = v6Suffix
			}
			rules = append(rules, network.LoadBalancingRule{
				Name: pointer.String(fmt.Sprintf("a%s%d-TCP-8081-%s", fullServiceName, serviceIndex, suffix)),
			})
			fips = append(fips, network.FrontendIPConfiguration{
				Name: pointer.String(fmt.Sprintf("a%s%d-%s", fullServiceName, serviceIndex, suffix)),
				ID:   pointer.String("fip"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: network.Dynamic,
					PublicIPAddress:           &network.PublicIPAddress{ID: pointer.String(fmt.Sprintf("testCluster-a%s%d-%s", fullServiceName, serviceIndex, suffix))},
				}})
		}
		if v4Enabled {
			addRuleAndFIP(false)
		}
		if v6Enabled {
			addRuleAndFIP(true)
		}

		if isInternal {
			fips[0].Subnet = &network.Subnet{Name: pointer.String("subnet")}
		}
		lb.LoadBalancingRules = &rules
		lb.FrontendIPConfigurations = &fips

		*expectedLBs = append(*expectedLBs, lb)
	} else {
		addRuleAndFIP := func(isIPv6 bool) {
			suffix := v4Suffix
			if isIPv6 {
				suffix = v6Suffix
			}
			*(*expectedLBs)[lbIndex].LoadBalancingRules = append(*(*expectedLBs)[lbIndex].LoadBalancingRules, network.LoadBalancingRule{
				Name: pointer.String(fmt.Sprintf("a%s%d-TCP-8081-%s", fullServiceName, serviceIndex, suffix)),
			})

			fip := network.FrontendIPConfiguration{
				Name: pointer.String(fmt.Sprintf("a%s%d-%s", fullServiceName, serviceIndex, suffix)),
				ID:   pointer.String("fip"),
				FrontendIPConfigurationPropertiesFormat: &network.FrontendIPConfigurationPropertiesFormat{
					PrivateIPAllocationMethod: network.Dynamic,
					PublicIPAddress:           &network.PublicIPAddress{ID: pointer.String(fmt.Sprintf("testCluster-a%s%d-%s", fullServiceName, serviceIndex, suffix))},
				},
			}
			if isInternal {
				fip.Subnet = &network.Subnet{Name: pointer.String("subnet")}
			}
			*(*expectedLBs)[lbIndex].FrontendIPConfigurations = append(*(*expectedLBs)[lbIndex].FrontendIPConfigurations, fip)
		}
		if v4Enabled {
			addRuleAndFIP(false)
		}
		if v6Enabled {
			addRuleAndFIP(true)
		}
	}

	mockLBsClient := mockloadbalancerclient.NewMockInterface(ctrl)
	az.LoadBalancerClient = mockLBsClient
	mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	for _, lb := range *expectedLBs {
		mockLBsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, *lb.Name, gomock.Any()).Return((*expectedLBs)[lbIndex], nil).MaxTimes(2)
	}
	mockLBsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(*expectedLBs, nil).MaxTimes(4)
	mockLBsClient.EXPECT().List(gomock.Any(), gomock.Not(az.ResourceGroup)).Return([]network.LoadBalancer{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
	mockLBsClient.EXPECT().Get(gomock.Any(), gomock.Not(az.ResourceGroup), gomock.Any(), gomock.Any()).Return(network.LoadBalancer{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
	mockLBsClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).MaxTimes(1)

	return expectedLBName
}

func testLoadBalancerServiceDefaultModeSelection(t *testing.T, isInternal bool) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	const vmCount = 8
	const availabilitySetCount = 4
	const serviceCount = 9

	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, vmCount, availabilitySetCount)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, serviceCount)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLBBackendPool.EXPECT().GetBackendPrivateIPs(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	expectedLBs := make([]network.LoadBalancer, 0)

	for index := 1; index <= serviceCount; index++ {
		svcName := fmt.Sprintf("service-%d", index)
		var svc v1.Service
		if isInternal {
			svc = getInternalTestService(svcName, int32(index))
			validateTestSubnet(t, az, &svc)
		} else {
			svc = getTestService(svcName, v1.ProtocolTCP, nil, false, int32(index))
		}

		expectedLBName := setMockLBs(az, ctrl, &expectedLBs, "service", 1, index, isInternal)

		expectedPLS := make([]network.PrivateLinkService, 0)
		mockPLSClient := mockprivatelinkserviceclient.NewMockInterface(ctrl)
		mockPLSClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(expectedPLS, nil).MinTimes(1).MaxTimes(1)
		az.PrivateLinkServiceClient = mockPLSClient

		lbStatus, err := az.EnsureLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes)
		if err != nil {
			t.Errorf("Unexpected error: %q", err)
		}
		if lbStatus == nil {
			t.Errorf("Unexpected error: %s", svcName)
		}

		ctx, cancel := getContextWithCancel()
		defer cancel()
		result, _ := az.LoadBalancerClient.List(ctx, az.Config.ResourceGroup)
		lb := result[0]
		lbCount := len(result)
		expectedNumOfLB := 1
		if lbCount != expectedNumOfLB {
			t.Errorf("Unexpected number of LB's: Expected (%d) Found (%d)", expectedNumOfLB, lbCount)
		}

		if !strings.EqualFold(*lb.Name, expectedLBName) {
			t.Errorf("lb name should be the default LB name Extected (%s) Found (%s)", expectedLBName, *lb.Name)
		}

		ruleCount := len(*lb.LoadBalancingRules)
		if ruleCount != index {
			t.Errorf("lb rule count should be equal to number of services deployed, expected (%d) Found (%d)", index, ruleCount)
		}
	}
}

// Validate even distribution of external services across load balancers
// based on number of availability sets
func testLoadBalancerServiceAutoModeSelection(t *testing.T, isInternal bool) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	const vmCount = 8
	const availabilitySetCount = 4
	const serviceCount = 9

	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, vmCount, availabilitySetCount)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, serviceCount)

	expectedLBs := make([]network.LoadBalancer, 0)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLBBackendPool.EXPECT().GetBackendPrivateIPs(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	for index := 1; index <= serviceCount; index++ {
		svcName := fmt.Sprintf("service-%d", index)
		var svc v1.Service
		if isInternal {
			svc = getInternalTestService(svcName, int32(index))
			validateTestSubnet(t, az, &svc)
		} else {
			svc = getTestService(svcName, v1.ProtocolTCP, nil, false, int32(index))
		}
		setLoadBalancerAutoModeAnnotation(&svc)

		setMockLBs(az, ctrl, &expectedLBs, "service", availabilitySetCount, index, isInternal)

		expectedPLS := make([]network.PrivateLinkService, 0)
		mockPLSClient := mockprivatelinkserviceclient.NewMockInterface(ctrl)
		mockPLSClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(expectedPLS, nil).MinTimes(1).MaxTimes(1)
		az.PrivateLinkServiceClient = mockPLSClient

		lbStatus, err := az.EnsureLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes)
		if err != nil {
			t.Errorf("Unexpected error: %q", err)
		}
		if lbStatus == nil {
			t.Errorf("Unexpected error: %s", svcName)
		}

		// expected is MIN(index, availabilitySetCount)
		expectedNumOfLB := int(math.Min(float64(index), float64(availabilitySetCount)))
		ctx, cancel := getContextWithCancel()
		defer cancel()
		result, _ := az.LoadBalancerClient.List(ctx, az.Config.ResourceGroup)
		lbCount := len(result)
		if lbCount != expectedNumOfLB {
			t.Errorf("Unexpected number of LB's: Expected (%d) Found (%d)", expectedNumOfLB, lbCount)
		}

		maxRules := 0
		minRules := serviceCount
		for _, lb := range result {
			ruleCount := len(*lb.LoadBalancingRules)
			if ruleCount < minRules {
				minRules = ruleCount
			}
			if ruleCount > maxRules {
				maxRules = ruleCount
			}
		}

		delta := maxRules - minRules
		if delta > 1 {
			t.Errorf("Unexpected min or max rule in LB's in resource group: Service Index (%d) Min (%d) Max(%d)", index, minRules, maxRules)
		}
	}
}

// Validate availability set selection of services across load balancers
// based on provided availability sets through service annotation
// The scenario is that there are 4 availability sets in the agent pool but the
// services will be assigned load balancers that are part of the provided availability sets
// specified in service annotation
func testLoadBalancerServicesSpecifiedSelection(t *testing.T, isInternal bool) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	const vmCount = 8
	const availabilitySetCount = 4
	const serviceCount = 9

	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, vmCount, availabilitySetCount)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, serviceCount)

	selectedAvailabilitySetName1 := getAvailabilitySetName(az, 0, availabilitySetCount)

	expectedLBs := make([]network.LoadBalancer, 0)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLBBackendPool.EXPECT().GetBackendPrivateIPs(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	for index := 1; index <= serviceCount; index++ {
		svcName := fmt.Sprintf("service-%d", index)
		var svc v1.Service
		if isInternal {
			svc = getInternalTestService(svcName, int32(index))
			validateTestSubnet(t, az, &svc)
		} else {
			svc = getTestService(svcName, v1.ProtocolTCP, nil, false, int32(index))
		}
		lbMode := selectedAvailabilitySetName1
		setLoadBalancerModeAnnotation(&svc, lbMode)

		setMockLBs(az, ctrl, &expectedLBs, "service", 1, index, isInternal)

		expectedPLS := make([]network.PrivateLinkService, 0)
		mockPLSClient := mockprivatelinkserviceclient.NewMockInterface(ctrl)
		mockPLSClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(expectedPLS, nil).MinTimes(1).MaxTimes(1)
		az.PrivateLinkServiceClient = mockPLSClient

		lbStatus, err := az.EnsureLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes)
		if err != nil {
			t.Errorf("Unexpected error: %q", err)
		}
		if lbStatus == nil {
			t.Errorf("Unexpected error: %s", svcName)
		}

		// expected is MIN(index, 2)
		expectedNumOfLB := int(math.Min(float64(index), float64(1)))
		ctx, cancel := getContextWithCancel()
		defer cancel()
		result, _ := az.LoadBalancerClient.List(ctx, az.Config.ResourceGroup)
		lbCount := len(result)
		if lbCount != expectedNumOfLB {
			t.Errorf("Unexpected number of LB's: Expected (%d) Found (%d)", expectedNumOfLB, lbCount)
		}
	}
}

func testLoadBalancerMaxRulesServices(t *testing.T, isInternal bool) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	const vmCount = 1
	const availabilitySetCount = 1

	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, vmCount, availabilitySetCount)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	az.Config.MaximumLoadBalancerRuleCount = 1

	expectedLBs := make([]network.LoadBalancer, 0)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLBBackendPool.EXPECT().GetBackendPrivateIPs(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	for index := 1; index <= az.Config.MaximumLoadBalancerRuleCount; index++ {
		svcName := fmt.Sprintf("service-%d", index)
		var svc v1.Service
		if isInternal {
			svc = getInternalTestService(svcName, int32(index))
			validateTestSubnet(t, az, &svc)
		} else {
			svc = getTestService(svcName, v1.ProtocolTCP, nil, false, int32(index))
		}

		setMockLBs(az, ctrl, &expectedLBs, "service", az.Config.MaximumLoadBalancerRuleCount, index, isInternal)

		expectedPLS := make([]network.PrivateLinkService, 0)
		mockPLSClient := mockprivatelinkserviceclient.NewMockInterface(ctrl)
		mockPLSClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(expectedPLS, nil).MinTimes(1).MaxTimes(1)
		az.PrivateLinkServiceClient = mockPLSClient

		lbStatus, err := az.EnsureLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes)
		if err != nil {
			t.Errorf("Unexpected error: %q", err)
		}
		if lbStatus == nil {
			t.Errorf("Unexpected error: %s", svcName)
		}

		// expected is MIN(index, az.Config.MaximumLoadBalancerRuleCount)
		expectedNumOfLBRules := int(math.Min(float64(index), float64(az.Config.MaximumLoadBalancerRuleCount)))
		ctx, cancel := getContextWithCancel()
		defer cancel()
		result, _ := az.LoadBalancerClient.List(ctx, az.Config.ResourceGroup)
		lbCount := len(result)
		if lbCount != expectedNumOfLBRules {
			t.Errorf("Unexpected number of LB's: Expected (%d) Found (%d)", expectedNumOfLBRules, lbCount)
		}
	}

	// validate adding a new service fails since it will exceed the max limit on LB
	svcName := fmt.Sprintf("service-%d", az.Config.MaximumLoadBalancerRuleCount+1)
	var svc v1.Service
	if isInternal {
		svc = getInternalTestService(svcName, 8081)
		validateTestSubnet(t, az, &svc)
	} else {
		svc = getTestService(svcName, v1.ProtocolTCP, nil, false, 8081)
	}

	mockLBsClient := mockloadbalancerclient.NewMockInterface(ctrl)
	az.LoadBalancerClient = mockLBsClient
	mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	for _, lb := range expectedLBs {
		mockLBsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, *lb.Name, gomock.Any()).Return(expectedLBs[0], nil).MaxTimes(2)
	}
	mockLBsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(expectedLBs, nil).MaxTimes(2)

	_, err := az.EnsureLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes)
	if err == nil {
		t.Errorf("Expect any new service to fail as max limit in lb has reached")
	} else {
		expectedErrMessageSubString := "all available load balancers have exceeded maximum rule limit"
		if !strings.Contains(err.Error(), expectedErrMessageSubString) {
			t.Errorf("Error message returned is not expected, expected sub string=%s, actual error message=%v", expectedErrMessageSubString, err)
		}
	}
}

// Validate service deletion in lb auto selection mode
func testLoadBalancerServiceAutoModeDeleteSelection(t *testing.T, isInternal bool) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	const vmCount = 8
	const availabilitySetCount = 4
	const serviceCount = 9

	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, vmCount, availabilitySetCount)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, serviceCount)

	expectedLBs := make([]network.LoadBalancer, 0)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLBBackendPool.EXPECT().GetBackendPrivateIPs(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

	for index := 1; index <= serviceCount; index++ {
		svcName := fmt.Sprintf("service-%d", index)
		var svc v1.Service
		if isInternal {
			svc = getInternalTestService(svcName, int32(index))
			validateTestSubnet(t, az, &svc)
		} else {
			svc = getTestService(svcName, v1.ProtocolTCP, nil, false, int32(index))
		}
		setLoadBalancerAutoModeAnnotation(&svc)

		setMockLBs(az, ctrl, &expectedLBs, "service", availabilitySetCount, index, isInternal)

		expectedPLS := make([]network.PrivateLinkService, 0)
		mockPLSClient := mockprivatelinkserviceclient.NewMockInterface(ctrl)
		mockPLSClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(expectedPLS, nil).MinTimes(1).MaxTimes(1)
		az.PrivateLinkServiceClient = mockPLSClient

		lbStatus, err := az.EnsureLoadBalancer(context.TODO(), testClusterName, &svc, clusterResources.nodes)
		if err != nil {
			t.Errorf("Unexpected error: %q", err)
		}
		if lbStatus == nil {
			t.Errorf("Unexpected error: %s", svcName)
		}
	}

	for index := serviceCount; index >= 1; index-- {
		svcName := fmt.Sprintf("service-%d", index)
		var svc v1.Service
		if isInternal {
			svc = getInternalTestService(svcName, int32(index))
			validateTestSubnet(t, az, &svc)
		} else {
			svc = getTestService(svcName, v1.ProtocolTCP, nil, false, int32(index))
		}

		setLoadBalancerAutoModeAnnotation(&svc)

		mockLBsClient := mockloadbalancerclient.NewMockInterface(ctrl)
		az.LoadBalancerClient = mockLBsClient
		mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
		for _, lb := range expectedLBs {
			mockLBsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, *lb.Name, gomock.Any()).Return(expectedLBs[0], nil).MaxTimes(2)
		}
		mockLBsClient.EXPECT().Delete(gomock.Any(), az.ResourceGroup, gomock.Any()).Return(nil).AnyTimes()
		mockLBsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(expectedLBs, nil).MaxTimes(4)

		// expected is MIN(index, availabilitySetCount)
		expectedNumOfLB := int(math.Min(float64(index), float64(availabilitySetCount)))
		ctx, cancel := getContextWithCancel()
		defer cancel()
		result, _ := az.LoadBalancerClient.List(ctx, az.Config.ResourceGroup)
		lbCount := len(result)
		if lbCount != expectedNumOfLB {
			t.Errorf("Unexpected number of LB's: Expected (%d) Found (%d)", expectedNumOfLB, lbCount)
		}

		expectedPLS := make([]network.PrivateLinkService, 0)
		mockPLSClient := mockprivatelinkserviceclient.NewMockInterface(ctrl)
		mockPLSClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(expectedPLS, nil).MinTimes(1).MaxTimes(1)
		az.PrivateLinkServiceClient = mockPLSClient

		err := az.EnsureLoadBalancerDeleted(context.TODO(), testClusterName, &svc)
		if err != nil {
			t.Errorf("Unexpected error: %q", err)
		}

		if index <= availabilitySetCount {
			expectedLBs = expectedLBs[:len(expectedLBs)-1]
		}
	}
}

// Test addition of a new service on an internal LB with a subnet.
func TestReconcileLoadBalancerAddServiceOnInternalSubnet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	svc := getInternalTestService("service1", 80)
	validateTestSubnet(t, az, &svc)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, true)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, err := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	// ensure we got a frontend ip configuration
	if len(*lb.FrontendIPConfigurations) != 1 {
		t.Error("Expected the loadbalancer to have a frontend ip configuration")
	}

	validateLoadBalancer(t, lb, svc)
}

func TestReconcileSecurityGroupFromAnyDestinationAddressPrefixToLoadBalancerIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.IPFamily = DualStack
	svc1 := getTestService("serviceea", v1.ProtocolTCP, nil, false, 80)
	makeTestServiceDualStack(&svc1)
	setServiceLoadBalancerIP(&svc1, "192.168.0.0")
	setServiceLoadBalancerIP(&svc1, "fdf8:f535:82e4::53")

	sg := getTestSecurityGroup(az)
	setMockSecurityGroup(az, ctrl, sg)

	// Simulate a pre-Kubernetes 1.8 NSG, where we do not specify the destination address prefix
	_, err := az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{""}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}
	lbIPs := []string{getServiceLoadBalancerIP(&svc1, false), getServiceLoadBalancerIP(&svc1, true)}
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc1, &lbIPs, nil, true)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}
	validateSecurityGroup(t, az, sg, svc1)
}

func TestReconcileSecurityGroupDynamicLoadBalancerIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.IPFamily = DualStack
	svc1 := getTestService("servicea", v1.ProtocolTCP, nil, false, 80)
	setServiceLoadBalancerIP(&svc1, "")

	sg := getTestSecurityGroup(az)
	setMockSecurityGroup(az, ctrl, sg)

	dynamicallyAssignedIPs := []string{"192.168.0.0", "fdf8:f535:82e4::53"}
	sg, err := az.reconcileSecurityGroup(testClusterName, &svc1, &dynamicallyAssignedIPs, nil, true)
	if err != nil {
		t.Errorf("unexpected error: %q", err)
	}
	validateSecurityGroup(t, az, sg, svc1)
}

// Test addition of services on an internal LB using both default and explicit subnets.
func TestReconcileLoadBalancerAddServicesOnMultipleSubnets(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	svc1 := getTestService("service1", v1.ProtocolTCP, nil, false, 8081)
	svc2 := getInternalTestService("service2", 8081)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	// svc1 is using LB without "-internal" suffix
	lb, err := az.reconcileLoadBalancer(testClusterName, &svc1, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling svc1: %q", err)
	}

	// ensure we got a frontend ip configuration for each service
	if len(*lb.FrontendIPConfigurations) != 1 {
		t.Error("Expected the loadbalancer to have 1 frontend ip configurations")
	}

	validateLoadBalancer(t, lb, svc1)

	// Internal and External service cannot reside on the same LB resource
	validateTestSubnet(t, az, &svc2)

	expectedLBs = make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 2, true)

	// svc2 is using LB with "-internal" suffix
	lb, err = az.reconcileLoadBalancer(testClusterName, &svc2, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling svc2: %q", err)
	}

	// ensure we got a frontend ip configuration for each service
	if len(*lb.FrontendIPConfigurations) != 1 {
		t.Error("Expected the loadbalancer to have 1 frontend ip configurations")
	}

	validateLoadBalancer(t, lb, svc2)
}

// Test moving a service exposure from one subnet to another.
func TestReconcileLoadBalancerEditServiceSubnet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	svc := getInternalTestService("service1", 8081)
	validateTestSubnet(t, az, &svc)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, true)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	expectedPLS := make([]network.PrivateLinkService, 0)
	mockPLSClient := az.PrivateLinkServiceClient.(*mockprivatelinkserviceclient.MockInterface)
	mockPLSClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(expectedPLS, nil).MinTimes(1).MaxTimes(1)

	lb, err := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling initial svc: %q", err)
	}

	validateLoadBalancer(t, lb, svc)

	svc.Annotations[consts.ServiceAnnotationLoadBalancerInternalSubnet] = "subnet"
	validateTestSubnet(t, az, &svc)

	expectedLBs = make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, true)

	lb, err = az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling edits to svc: %q", err)
	}

	// ensure we got a frontend ip configuration for the service
	if len(*lb.FrontendIPConfigurations) != 1 {
		t.Error("Expected the loadbalancer to have 1 frontend ip configuration")
	}

	validateLoadBalancer(t, lb, svc)
}

func TestReconcileLoadBalancerNodeHealth(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	svc := getTestService("service1", v1.ProtocolTCP, nil, false, 80)
	svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	svc.Spec.HealthCheckNodePort = int32(32456)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, err := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	// ensure we got a frontend ip configuration
	if len(*lb.FrontendIPConfigurations) != 1 {
		t.Error("Expected the loadbalancer to have a frontend ip configuration")
	}

	validateLoadBalancer(t, lb, svc)
}

// Test removing all services results in removing the frontend ip configuration
func TestReconcileLoadBalancerRemoveService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	svc := getTestService("service1", v1.ProtocolTCP, nil, false, 80, 443)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	_, err := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	expectedLBs[0].FrontendIPConfigurations = &[]network.FrontendIPConfiguration{}
	mockLBsClient := mockloadbalancerclient.NewMockInterface(ctrl)
	az.LoadBalancerClient = mockLBsClient
	mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLBsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, *expectedLBs[0].Name, gomock.Any()).Return(expectedLBs[0], nil).MaxTimes(2)
	mockLBsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(expectedLBs, nil).MaxTimes(3)
	mockLBsClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

	lb, err := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, false /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	// ensure we abandoned the frontend ip configuration
	if len(*lb.FrontendIPConfigurations) != 0 {
		t.Error("Expected the loadbalancer to have no frontend ip configuration")
	}

	validateLoadBalancer(t, lb)
}

// Test removing all service ports results in removing the frontend ip configuration
func TestReconcileLoadBalancerRemoveAllPortsRemovesFrontendConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	svc := getTestService("service1", v1.ProtocolTCP, nil, false, 80)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, err := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}
	validateLoadBalancer(t, lb, svc)

	svcUpdated := getTestService("service1", v1.ProtocolTCP, nil, false)

	expectedLBs[0].FrontendIPConfigurations = &[]network.FrontendIPConfiguration{}
	mockLBsClient := mockloadbalancerclient.NewMockInterface(ctrl)
	az.LoadBalancerClient = mockLBsClient
	mockLBsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockLBsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, *expectedLBs[0].Name, gomock.Any()).Return(expectedLBs[0], nil).MaxTimes(2)
	mockLBsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return(expectedLBs, nil).MaxTimes(3)
	mockLBsClient.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

	lb, err = az.reconcileLoadBalancer(testClusterName, &svcUpdated, clusterResources.nodes, false /* wantLb*/)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	// ensure we abandoned the frontend ip configuration
	if len(*lb.FrontendIPConfigurations) != 0 {
		t.Error("Expected the loadbalancer to have no frontend ip configuration")
	}

	validateLoadBalancer(t, lb, svcUpdated)
}

// Test removal of a port from an existing service.
func TestReconcileLoadBalancerRemovesPort(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, false)
	svc := getTestService("service1", v1.ProtocolTCP, nil, false, 80, 443)
	_, err := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	expectedLBs = make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, false)
	svcUpdated := getTestService("service1", v1.ProtocolTCP, nil, false, 80)
	lb, err := az.reconcileLoadBalancer(testClusterName, &svcUpdated, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	validateLoadBalancer(t, lb, svcUpdated)
}

// Test reconciliation of multiple services on same port
func TestReconcileLoadBalancerMultipleServices(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 2)

	svc1 := getTestService("service1", v1.ProtocolTCP, nil, false, 80, 443)
	svc2 := getTestService("service2", v1.ProtocolTCP, nil, false, 81)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, false)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	expectedPLS := make([]network.PrivateLinkService, 0)
	mockPLSClient := az.PrivateLinkServiceClient.(*mockprivatelinkserviceclient.MockInterface)
	mockPLSClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(expectedPLS, nil).MinTimes(1).MaxTimes(1)

	_, err := az.reconcileLoadBalancer(testClusterName, &svc1, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 2, false)

	updatedLoadBalancer, err := az.reconcileLoadBalancer(testClusterName, &svc2, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	validateLoadBalancer(t, updatedLoadBalancer, svc1, svc2)
}

func findLBRuleForPort(lbRules []network.LoadBalancingRule, port int32) (network.LoadBalancingRule, error) {
	for _, lbRule := range lbRules {
		if *lbRule.FrontendPort == port {
			return lbRule, nil
		}
	}
	return network.LoadBalancingRule{}, fmt.Errorf("expected LB rule with port %d but none found", port)
}

func TestServiceDefaultsToNoSessionPersistence(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestService("service-sa-omitted1", v1.ProtocolTCP, nil, false, 8081)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service-sa-omitted", 1, 1, false)

	expectedPIP := network.PublicIPAddress{
		Name:     pointer.String("testCluster-aservicesaomitted1"),
		Location: &az.Location,
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: network.Static,
			PublicIPAddressVersion:   network.IPv4,
		},
		Tags: map[string]*string{
			consts.ServiceTagKey:  pointer.String("aservicesaomitted1"),
			consts.ClusterNameKey: pointer.String(testClusterName),
		},
		Sku: &network.PublicIPAddressSku{
			Name: network.PublicIPAddressSkuNameStandard,
		},
		ID: pointer.String("testCluster-aservicesaomitted1"),
	}

	mockPIPsClient := mockpublicipclient.NewMockInterface(ctrl)
	az.PublicIPAddressesClient = mockPIPsClient
	mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockPIPsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]network.PublicIPAddress{expectedPIP}, nil).AnyTimes()

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, err := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling svc1: %q", err)
	}
	validateLoadBalancer(t, lb, svc)
	lbRule, err := findLBRuleForPort(*lb.LoadBalancingRules, 8081)
	if err != nil {
		t.Error(err)
	}

	if lbRule.LoadDistribution != network.LoadDistributionDefault {
		t.Errorf("Expected LB rule to have default load distribution but was %s", lbRule.LoadDistribution)
	}
}

func TestServiceRespectsNoSessionAffinity(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestService("service-sa-none", v1.ProtocolTCP, nil, false, 8081)
	svc.Spec.SessionAffinity = v1.ServiceAffinityNone
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service-sa-none", 1, 1, false)

	expectedPIP := network.PublicIPAddress{
		Name:     pointer.String("testCluster-aservicesanone"),
		Location: &az.Location,
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: network.Static,
			PublicIPAddressVersion:   network.IPv4,
		},
		Tags: map[string]*string{
			consts.ServiceTagKey:  pointer.String("aservicesanone"),
			consts.ClusterNameKey: pointer.String(testClusterName),
		},
		Sku: &network.PublicIPAddressSku{
			Name: network.PublicIPAddressSkuNameStandard,
		},
		ID: pointer.String("testCluster-aservicesanone"),
	}

	mockPIPsClient := mockpublicipclient.NewMockInterface(ctrl)
	az.PublicIPAddressesClient = mockPIPsClient
	mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockPIPsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]network.PublicIPAddress{expectedPIP}, nil).AnyTimes()
	mockPIPsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "testCluster-aservicesanone", gomock.Any()).Return(expectedPIP, nil).AnyTimes()

	expectedPLS := make([]network.PrivateLinkService, 0)
	mockPLSClient := az.PrivateLinkServiceClient.(*mockprivatelinkserviceclient.MockInterface)
	mockPLSClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(expectedPLS, nil).MinTimes(1).MaxTimes(1)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, err := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling svc1: %q", err)
	}

	validateLoadBalancer(t, lb, svc)

	lbRule, err := findLBRuleForPort(*lb.LoadBalancingRules, 8081)
	if err != nil {
		t.Error(err)
	}

	if lbRule.LoadDistribution != network.LoadDistributionDefault {
		t.Errorf("Expected LB rule to have default load distribution but was %s", lbRule.LoadDistribution)
	}
}

func TestServiceRespectsClientIPSessionAffinity(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestService("service-sa-clientip", v1.ProtocolTCP, nil, false, 8081)
	svc.Spec.SessionAffinity = v1.ServiceAffinityClientIP
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service-sa-clientip", 1, 1, false)

	expectedPIP := network.PublicIPAddress{
		Name:     pointer.String("testCluster-aservicesaclientip"),
		Location: &az.Location,
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: network.Static,
			PublicIPAddressVersion:   network.IPv4,
		},
		Tags: map[string]*string{
			consts.ServiceTagKey:  pointer.String("aservicesaclientip"),
			consts.ClusterNameKey: pointer.String(testClusterName),
		},
		Sku: &network.PublicIPAddressSku{
			Name: network.PublicIPAddressSkuNameStandard,
		},
		ID: pointer.String("testCluster-aservicesaclientip"),
	}

	mockPIPsClient := mockpublicipclient.NewMockInterface(ctrl)
	az.PublicIPAddressesClient = mockPIPsClient
	mockPIPsClient.EXPECT().CreateOrUpdate(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockPIPsClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "testCluster-aservicesaclientip", gomock.Any()).Return(expectedPIP, nil).AnyTimes()
	mockPIPsClient.EXPECT().List(gomock.Any(), az.ResourceGroup).Return([]network.PublicIPAddress{expectedPIP}, nil).AnyTimes()

	expectedPLS := make([]network.PrivateLinkService, 0)
	mockPLSClient := az.PrivateLinkServiceClient.(*mockprivatelinkserviceclient.MockInterface)
	mockPLSClient.EXPECT().List(gomock.Any(), az.Config.ResourceGroup).Return(expectedPLS, nil).MinTimes(1).MaxTimes(1)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, err := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error reconciling svc1: %q", err)
	}

	validateLoadBalancer(t, lb, svc)

	lbRule, err := findLBRuleForPort(*lb.LoadBalancingRules, 8081)
	if err != nil {
		t.Error(err)
	}

	if lbRule.LoadDistribution != network.LoadDistributionSourceIP {
		t.Errorf("Expected LB rule to have SourceIP load distribution but was %s", lbRule.LoadDistribution)
	}
}

func TestReconcileSecurityGroupNewServiceAddsPort(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.IPFamily = DualStack
	getTestSecurityGroup(az)
	svc1 := getTestService("service1", v1.ProtocolTCP, nil, false, 80)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)
	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, false)
	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	lb, _ := az.reconcileLoadBalancer(testClusterName, &svc1, clusterResources.nodes, true)
	lbStatus, _, _, _ := az.getServiceLoadBalancerStatus(&svc1, lb, nil)

	assert.Equal(t, 2, len(lbStatus.Ingress))
	lbIPs := []string{lbStatus.Ingress[0].IP, lbStatus.Ingress[1].IP}
	sg, err := az.reconcileSecurityGroup(testClusterName, &svc1, &lbIPs, nil, true /* wantLb */)
	assert.NoError(t, err)

	validateSecurityGroup(t, az, sg, svc1)
}

func TestReconcileSecurityGroupNewInternalServiceAddsPort(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	getTestSecurityGroup(az)
	svc1 := getInternalTestService("service1", 80)
	validateTestSubnet(t, az, &svc1)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, true)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, _ := az.reconcileLoadBalancer(testClusterName, &svc1, clusterResources.nodes, true)
	lbStatus, _, _, _ := az.getServiceLoadBalancerStatus(&svc1, lb, nil)
	sg, err := az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{lbStatus.Ingress[0].IP}, nil, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc1)
}

// TestReconcileSecurityGroupRemoveService keeps service1 but removes service2.
func TestReconcileSecurityGroupRemoveService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.IPFamily = DualStack
	service1 := getTestService("service1", v1.ProtocolTCP, nil, false, 81)
	makeTestServiceDualStack(&service1)
	service2 := getTestService("service2", v1.ProtocolTCP, nil, false, 82)
	makeTestServiceDualStack(&service2)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 2, service1, service2)

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, true)
	mockLBClient := az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
	mockLBClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "testCluster", gomock.Any()).Return(
		getTestLoadBalancerDualStack(pointer.String(testClusterName),
			pointer.String(az.ResourceGroup),
			pointer.String(testClusterName),
			pointer.String("aservice1"),
			service1,
			"Standard"), nil).AnyTimes()

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, _ := az.reconcileLoadBalancer(testClusterName, &service1, clusterResources.nodes, true)
	_, _ = az.reconcileLoadBalancer(testClusterName, &service2, clusterResources.nodes, true)

	lbStatus, _, _, err := az.getServiceLoadBalancerStatus(&service1, lb, nil)
	assert.NoError(t, err)
	assert.NotNil(t, lbStatus)

	sg := getTestSecurityGroup(az, service1, service2)
	validateSecurityGroup(t, az, sg, service1, service2)

	assert.Equal(t, 2, len(lbStatus.Ingress))
	sg, err = az.reconcileSecurityGroup(testClusterName, &service1, &[]string{lbStatus.Ingress[0].IP, lbStatus.Ingress[1].IP}, nil, false /* wantLb */)
	assert.NoError(t, err)

	validateSecurityGroup(t, az, sg, service2)
}

func TestReconcileSecurityGroupRemoveServiceRemovesPort(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestService("service1", v1.ProtocolTCP, nil, false, 80, 443)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	getTestSecurityGroup(az, svc)
	svcUpdated := getTestService("service1", v1.ProtocolTCP, nil, false, 80)
	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, true)
	mockLBClient := az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
	mockLBClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "testCluster", gomock.Any()).Return(
		getTestLoadBalancer(pointer.String(testClusterName),
			pointer.String(az.ResourceGroup),
			pointer.String(testClusterName),
			pointer.String("aservice1"),
			svc,
			"Standard"), nil).AnyTimes()
	lb, _ := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true)
	lbStatus, _, _, _ := az.getServiceLoadBalancerStatus(&svc, lb, nil)

	sg, err := az.reconcileSecurityGroup(testClusterName, &svcUpdated, &[]string{lbStatus.Ingress[0].IP}, nil, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	validateSecurityGroup(t, az, sg, svcUpdated)
}

func TestReconcileSecurityWithSourceRanges(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestService("service1", v1.ProtocolTCP, nil, false, 80, 443)
	svc.Spec.LoadBalancerSourceRanges = []string{
		"192.168.0.0/24",
		"10.0.0.0/32",
	}
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	getTestSecurityGroup(az, svc)
	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, false)
	lb, _ := az.reconcileLoadBalancer(testClusterName, &svc, clusterResources.nodes, true)
	lbStatus, _, _, _ := az.getServiceLoadBalancerStatus(&svc, lb, nil)

	sg, err := az.reconcileSecurityGroup(testClusterName, &svc, &[]string{lbStatus.Ingress[0].IP}, nil, true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc)
}

func TestReconcileSecurityGroupEtagMismatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	sg := getTestSecurityGroup(az)
	cachedSG := *sg
	cachedSG.Etag = pointer.String("1111111-0000-0000-0000-000000000000")
	az.nsgCache.Set(pointer.StringDeref(sg.Name, ""), &cachedSG)

	svc1 := getTestService("service1", v1.ProtocolTCP, nil, false, 80)
	clusterResources, expectedInterfaces, expectedVirtualMachines := getClusterResources(az, 1, 1)
	setMockEnv(az, ctrl, expectedInterfaces, expectedVirtualMachines, 1)
	mockSGsClient := mocksecuritygroupclient.NewMockInterface(ctrl)
	az.SecurityGroupsClient = mockSGsClient
	mockSGsClient.EXPECT().Get(gomock.Any(), az.SecurityGroupResourceGroup, az.SecurityGroupName, gomock.Any()).Return(cachedSG, nil).AnyTimes()
	expectedError := &retry.Error{
		HTTPStatusCode: http.StatusPreconditionFailed,
		RawError:       errPreconditionFailedEtagMismatch,
	}
	mockSGsClient.EXPECT().CreateOrUpdate(gomock.Any(), az.SecurityGroupResourceGroup, az.SecurityGroupName, gomock.Any(), gomock.Any()).Return(expectedError).AnyTimes()

	expectedLBs := make([]network.LoadBalancer, 0)
	setMockLBs(az, ctrl, &expectedLBs, "service", 1, 1, true)
	mockLBClient := az.LoadBalancerClient.(*mockloadbalancerclient.MockInterface)
	mockLBClient.EXPECT().Get(gomock.Any(), az.ResourceGroup, "testCluster", gomock.Any()).Return(
		getTestLoadBalancer(pointer.String(testClusterName),
			pointer.String(az.ResourceGroup),
			pointer.String(testClusterName),
			pointer.String("aservice1"),
			svc1,
			"Standard"), nil).AnyTimes()

	mockLBBackendPool := az.LoadBalancerBackendPool.(*MockBackendPool)
	mockLBBackendPool.EXPECT().ReconcileBackendPools(gomock.Any(), gomock.Any(), gomock.Any()).Return(false, false, nil).AnyTimes()
	mockLBBackendPool.EXPECT().EnsureHostsInPool(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	lb, _ := az.reconcileLoadBalancer(testClusterName, &svc1, clusterResources.nodes, true)
	lbStatus, _, _, _ := az.getServiceLoadBalancerStatus(&svc1, lb, nil)

	newSG, err := az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{lbStatus.Ingress[0].IP}, nil, true /* wantLb */)
	assert.Nil(t, newSG)
	assert.Error(t, err)

	assert.Equal(t, expectedError.Error(), err)
}

func TestReconcilePublicIPsWithNewService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.IPFamily = DualStack
	svc := getTestService("servicea", v1.ProtocolTCP, nil, false, 80, 443)
	makeTestServiceDualStack(&svc)

	setMockPublicIPs(az, ctrl, 1)

	pips, err := az.reconcilePublicIPs(testClusterName, &svc, "", true /* wantLb*/)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}
	validatePublicIPs(t, pips, &svc, true)

	pips2, err := az.reconcilePublicIPs(testClusterName, &svc, "", true /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}
	validatePublicIPs(t, pips2, &svc, true)

	pipsNames, pips2Names := []string{}, []string{}
	pipsAddrs, pips2Addrs := []string{}, []string{}
	for _, pip := range pips {
		pipsNames = append(pipsNames, *pip.Name)
		pipsAddrs = append(pipsAddrs, pointer.StringDeref(pip.PublicIPAddressPropertiesFormat.IPAddress, ""))
	}
	for _, pip := range pips2 {
		pips2Names = append(pips2Names, *pip.Name)
		pips2Addrs = append(pips2Addrs, pointer.StringDeref(pip.PublicIPAddressPropertiesFormat.IPAddress, ""))
	}
	assert.Truef(t, compareStrings(pipsNames, pips2Names) && compareStrings(pipsAddrs, pips2Addrs),
		"We should get the exact same public ip resource after a second reconcile")
}

func TestReconcilePublicIPsRemoveService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.IPFamily = DualStack
	svc := getTestService("servicea", v1.ProtocolTCP, nil, false, 80, 443)
	makeTestServiceDualStack(&svc)

	setMockPublicIPs(az, ctrl, 1)

	pips, err := az.reconcilePublicIPs(testClusterName, &svc, "", true /* wantLb*/)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	validatePublicIPs(t, pips, &svc, true)

	// Remove the service
	pips, err = az.reconcilePublicIPs(testClusterName, &svc, "", false /* wantLb */)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}
	validatePublicIPs(t, pips, &svc, false)
}

func TestReconcilePublicIPsWithInternalService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.IPFamily = DualStack
	svc := getInternalTestService("servicea", 80, 443)
	makeTestServiceDualStack(&svc)

	setMockPublicIPs(az, ctrl, 1)

	pips, err := az.reconcilePublicIPs(testClusterName, &svc, "", true /* wantLb*/)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	validatePublicIPs(t, pips, &svc, true)
}

func TestReconcilePublicIPsWithExternalAndInternalSwitch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	az.IPFamily = DualStack
	svc := getInternalTestService("servicea", 80, 443)
	makeTestServiceDualStack(&svc)

	setMockPublicIPs(az, ctrl, 1)

	pips, err := az.reconcilePublicIPs(testClusterName, &svc, "", true /* wantLb*/)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}
	validatePublicIPs(t, pips, &svc, true)

	// Update to external service
	svcUpdated := getTestService("servicea", v1.ProtocolTCP, nil, false, 80)
	pips, err = az.reconcilePublicIPs(testClusterName, &svcUpdated, "", true /* wantLb*/)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}
	validatePublicIPs(t, pips, &svcUpdated, true)

	// Update to internal service again
	pips, err = az.reconcilePublicIPs(testClusterName, &svc, "", true /* wantLb*/)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}
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

func getClusterResources(az *Cloud, vmCount int, availabilitySetCount int) (clusterResources *ClusterResources, expectedInterfaces []network.Interface, expectedVirtualMachines []compute.VirtualMachine) {
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
		newNIC := network.Interface{
			ID:   &nicID,
			Name: &nicName,
			InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
				IPConfigurations: &[]network.InterfaceIPConfiguration{
					{
						ID: &primaryIPConfigID,
						InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
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
		newVM := compute.VirtualMachine{
			Name:     &vmName,
			Location: &az.Config.Location,
			VirtualMachineProperties: &compute.VirtualMachineProperties{
				AvailabilitySet: &compute.SubResource{
					ID: &asID,
				},
				NetworkProfile: &compute.NetworkProfile{
					NetworkInterfaces: &[]compute.NetworkInterfaceReference{
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

func getTestService(identifier string, proto v1.Protocol, annotations map[string]string, isIPv6 bool, requestedPorts ...int32) v1.Service {
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

	svc.Spec.ClusterIP = "10.0.0.2"
	if isIPv6 {
		svc.Spec.ClusterIP = "fd00::1907"
	}

	return svc
}

// TODO: This function should be merged into getTestService()
func makeTestServiceDualStack(svc *v1.Service) {
	svc.Spec.ClusterIPs = []string{"10.0.0.2", "fd00::1907"}
}

func getInternalTestService(identifier string, requestedPorts ...int32) v1.Service {
	return getTestServiceWithAnnotation(identifier, map[string]string{consts.ServiceAnnotationLoadBalancerInternal: consts.TrueAnnotationValue}, requestedPorts...)
}

func getTestServiceWithAnnotation(identifier string, annotations map[string]string, requestedPorts ...int32) v1.Service {
	svc := getTestService(identifier, v1.ProtocolTCP, nil, false, requestedPorts...)
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

func setLoadBalancerModeAnnotation(service *v1.Service, lbMode string) {
	service.Annotations[consts.ServiceAnnotationLoadBalancerMode] = lbMode
}

func setLoadBalancerAutoModeAnnotation(service *v1.Service) {
	setLoadBalancerModeAnnotation(service, consts.ServiceAnnotationLoadBalancerAutoModeValue)
}

func getServiceSourceRanges(service *v1.Service) []string {
	if len(service.Spec.LoadBalancerSourceRanges) == 0 {
		if !requiresInternalLoadBalancer(service) {
			return []string{"Internet"}
		}
	}

	return service.Spec.LoadBalancerSourceRanges
}

func getTestSecurityGroup(az *Cloud, services ...v1.Service) *network.SecurityGroup {
	v4Enabled, v6Enabled := az.clusterIPFamiliesEnabled()
	getRule := func(svc *v1.Service, port v1.ServicePort, src string, isIPv6 bool) network.SecurityRule {
		ruleName := az.getSecurityRuleName(svc, port, src, isIPv6)
		return network.SecurityRule{
			Name: pointer.String(ruleName),
			SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
				SourceAddressPrefix:  pointer.String(src),
				DestinationPortRange: pointer.String(fmt.Sprintf("%d", port.Port)),
			},
		}
	}

	rules := []network.SecurityRule{}
	for i, service := range services {
		for _, port := range service.Spec.Ports {
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

	sg := network.SecurityGroup{
		Name: &az.SecurityGroupName,
		Etag: pointer.String("0000000-0000-0000-0000-000000000000"),
		SecurityGroupPropertiesFormat: &network.SecurityGroupPropertiesFormat{
			SecurityRules: &rules,
		},
	}

	return &sg
}

func validateLoadBalancer(t *testing.T, loadBalancer *network.LoadBalancer, services ...v1.Service) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	expectedRuleCount := 0
	expectedFrontendIPCount := 0
	expectedProbeCount := 0
	expectedFrontendIPs := []ExpectedFrontendIPInfo{}
	for i, svc := range services {
		if len(svc.Spec.Ports) > 0 {
			expectedFrontendIPCount++
			expectedSubnetName := ""
			if requiresInternalLoadBalancer(&services[i]) {
				expectedSubnetName = svc.Annotations[consts.ServiceAnnotationLoadBalancerInternalSubnet]
				if expectedSubnetName == "" {
					expectedSubnetName = az.SubnetName
				}
			}
			expectedFrontendIP := ExpectedFrontendIPInfo{
				Name:   az.getDefaultFrontendIPConfigName(&services[i]),
				Subnet: pointer.String(expectedSubnetName),
			}
			expectedFrontendIPs = append(expectedFrontendIPs, expectedFrontendIP)
		}
		for _, wantedRule := range svc.Spec.Ports {
			expectedRuleCount++
			wantedRuleName := az.getLoadBalancerRuleName(&services[i], wantedRule.Protocol, wantedRule.Port, false)
			foundRule := false
			for _, actualRule := range *loadBalancer.LoadBalancingRules {
				if strings.EqualFold(*actualRule.Name, wantedRuleName) &&
					*actualRule.FrontendPort == wantedRule.Port &&
					*actualRule.BackendPort == wantedRule.Port {
					foundRule = true
					break
				}
			}
			if !foundRule {
				t.Errorf("Expected load balancer rule but didn't find it: %q", wantedRuleName)
			}

			// if UDP rule, there is no probe
			if wantedRule.Protocol == v1.ProtocolUDP {
				continue
			}

			expectedProbeCount++
			foundProbe := false
			if servicehelpers.NeedsHealthCheck(&services[i]) {
				path, port := servicehelpers.GetServiceHealthCheckPathPort(&services[i])
				wantedRuleName := az.getLoadBalancerRuleName(&services[i], v1.ProtocolTCP, port, false)
				for _, actualProbe := range *loadBalancer.Probes {
					if strings.EqualFold(*actualProbe.Name, wantedRuleName) &&
						*actualProbe.Port == port &&
						*actualProbe.RequestPath == path &&
						actualProbe.Protocol == network.ProbeProtocolHTTP {
						foundProbe = true
						break
					}
				}
			} else {
				for _, actualProbe := range *loadBalancer.Probes {
					if strings.EqualFold(*actualProbe.Name, wantedRuleName) &&
						*actualProbe.Port == wantedRule.NodePort {
						foundProbe = true
						break
					}
				}
			}
			if !foundProbe {
				for _, actualProbe := range *loadBalancer.Probes {
					t.Logf("Probe: %s %d", *actualProbe.Name, *actualProbe.Port)
				}
				t.Errorf("Expected loadbalancer probe but didn't find it: %q", wantedRuleName)
			}
		}
	}

	frontendIPCount := len(*loadBalancer.FrontendIPConfigurations)
	if frontendIPCount != expectedFrontendIPCount {
		t.Errorf("Expected the loadbalancer to have %d frontend IPs. Found %d.\n%v", expectedFrontendIPCount, frontendIPCount, loadBalancer.FrontendIPConfigurations)
	}

	frontendIPs := *loadBalancer.FrontendIPConfigurations
	for _, expectedFrontendIP := range expectedFrontendIPs {
		if !expectedFrontendIP.existsIn(frontendIPs) {
			t.Errorf("Expected the loadbalancer to have frontend IP %s/%s. Found %s", expectedFrontendIP.Name, pointer.StringDeref(expectedFrontendIP.Subnet, ""), describeFIPs(frontendIPs))
		}
	}

	lenRules := len(*loadBalancer.LoadBalancingRules)
	if lenRules != expectedRuleCount {
		t.Errorf("Expected the loadbalancer to have %d rules. Found %d.\n%v", expectedRuleCount, lenRules, loadBalancer.LoadBalancingRules)
	}

	lenProbes := len(*loadBalancer.Probes)
	if lenProbes != expectedProbeCount {
		t.Errorf("Expected the loadbalancer to have %d probes. Found %d.", expectedRuleCount, lenProbes)
	}
}

type ExpectedFrontendIPInfo struct {
	Name   string
	Subnet *string
}

func (expected ExpectedFrontendIPInfo) matches(frontendIP network.FrontendIPConfiguration) bool {
	return strings.EqualFold(expected.Name, pointer.StringDeref(frontendIP.Name, "")) && strings.EqualFold(pointer.StringDeref(expected.Subnet, ""), pointer.StringDeref(subnetName(frontendIP), ""))
}

func (expected ExpectedFrontendIPInfo) existsIn(frontendIPs []network.FrontendIPConfiguration) bool {
	for _, fip := range frontendIPs {
		if expected.matches(fip) {
			return true
		}
	}
	return false
}

func subnetName(frontendIP network.FrontendIPConfiguration) *string {
	if frontendIP.Subnet != nil {
		return frontendIP.Subnet.Name
	}
	return nil
}

func describeFIPs(frontendIPs []network.FrontendIPConfiguration) string {
	description := ""
	for _, actualFIP := range frontendIPs {
		actualSubnetName := ""
		if actualFIP.Subnet != nil {
			actualSubnetName = pointer.StringDeref(actualFIP.Subnet.Name, "")
		}
		actualFIPText := fmt.Sprintf("%s/%s ", pointer.StringDeref(actualFIP.Name, ""), actualSubnetName)
		description = description + actualFIPText
	}
	return description
}

func validatePublicIPs(t *testing.T, pips []*network.PublicIPAddress, service *v1.Service, wantLb bool) {
	for _, pip := range pips {
		validatePublicIP(t, pip, service, wantLb)
	}
}

func validatePublicIP(t *testing.T, publicIP *network.PublicIPAddress, service *v1.Service, wantLb bool) {
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

	if publicIP.Tags == nil || publicIP.Tags[consts.ServiceTagKey] == nil {
		t.Fatalf("Expected publicIP resource does not have tags[%s]", consts.ServiceTagKey)
	}

	serviceName := getServiceName(service)
	assert.Equalf(t, serviceName, pointer.StringDeref(publicIP.Tags[consts.ServiceTagKey], ""),
		"Expected publicIP resource has matching tags[%s]", consts.ServiceTagKey)
	assert.NotNilf(t, publicIP.Tags[consts.ClusterNameKey], "Expected publicIP resource does not have tags[%s]", consts.ClusterNameKey)
	assert.Equalf(t, testClusterName, pointer.StringDeref(publicIP.Tags[consts.ClusterNameKey], ""),
		"Expected publicIP resource has matching tags[%s]", consts.ClusterNameKey)

	// We cannot use Service LoadBalancerIP to compare with
	// Public IP's IPAddress
	// Because service properties are updated outside of cloudprovider code
}

func contains(ruleValues []string, targetValue string) bool {
	for _, ruleValue := range ruleValues {
		if strings.EqualFold(ruleValue, targetValue) {
			return true
		}
	}
	return false
}

func securityRuleMatches(serviceSourceRange string, servicePort v1.ServicePort, serviceIP string, securityRule network.SecurityRule) error {
	ruleSource := securityRule.SourceAddressPrefixes
	if ruleSource == nil || len(*ruleSource) == 0 {
		if securityRule.SourceAddressPrefix == nil {
			ruleSource = &[]string{}
		} else {
			ruleSource = &[]string{*securityRule.SourceAddressPrefix}
		}
	}

	rulePorts := securityRule.DestinationPortRanges
	if rulePorts == nil || len(*rulePorts) == 0 {
		if securityRule.DestinationPortRange == nil {
			rulePorts = &[]string{}
		} else {
			rulePorts = &[]string{*securityRule.DestinationPortRange}
		}
	}

	ruleDestination := securityRule.DestinationAddressPrefixes
	if ruleDestination == nil || len(*ruleDestination) == 0 {
		if securityRule.DestinationAddressPrefix == nil {
			ruleDestination = &[]string{}
		} else {
			ruleDestination = &[]string{*securityRule.DestinationAddressPrefix}
		}
	}

	if !contains(*ruleSource, serviceSourceRange) {
		return fmt.Errorf("Rule does not contain source %s", serviceSourceRange)
	}

	if !contains(*rulePorts, fmt.Sprintf("%d", servicePort.Port)) {
		return fmt.Errorf("Rule does not contain port %d", servicePort.Port)
	}

	if serviceIP != "" && !contains(*ruleDestination, serviceIP) {
		return fmt.Errorf("Rule does not contain destination %s", serviceIP)
	}

	return nil
}

func validateSecurityGroup(t *testing.T, az *Cloud, securityGroup *network.SecurityGroup, services ...v1.Service) {
	v4Enabled, v6Enabled := az.clusterIPFamiliesEnabled()

	seenRules := make(map[string]string)
	for i, svc := range services {
		svc := svc
		for _, wantedRule := range svc.Spec.Ports {
			sources := getServiceSourceRanges(&services[i])
			for _, source := range sources {
				checkRule := func(svc *v1.Service, wantedRule v1.ServicePort, source string, isIPv6 bool) {
					wantedRuleName := az.getSecurityRuleName(svc, wantedRule, source, isIPv6)
					seenRules[wantedRuleName] = wantedRuleName

					foundRule := false
					for _, actualRule := range *securityGroup.SecurityRules {
						if strings.EqualFold(*actualRule.Name, wantedRuleName) {
							err := securityRuleMatches(source, wantedRule, getServiceLoadBalancerIP(svc, isIPv6), actualRule)
							if err != nil {
								t.Errorf("Found matching security rule %q but properties were incorrect: %v", wantedRuleName, err)
							}
							foundRule = true
							break
						}
					}
					if !foundRule {
						t.Errorf("Expected security group rule but didn't find it: %q", wantedRuleName)
					}
				}

				if v4Enabled {
					checkRule(&services[i], wantedRule, source, false)
				}
				if v6Enabled {
					checkRule(&services[i], wantedRule, source, true)
				}
			}
		}
	}

	lenRules := len(*securityGroup.SecurityRules)
	expectedRuleCount := len(seenRules)
	assert.Equalf(t, expectedRuleCount, lenRules, "Expected rules: %v", seenRules)
}

func TestSecurityRulePriorityPicksNextAvailablePriority(t *testing.T) {
	rules := []network.SecurityRule{}

	var expectedPriority int32 = consts.LoadBalancerMinimumPriority + 50

	var i int32
	for i = consts.LoadBalancerMinimumPriority; i < expectedPriority; i++ {
		rules = append(rules, network.SecurityRule{
			SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
				Priority: pointer.Int32(i),
			},
		})
	}

	priority, err := getNextAvailablePriority(rules)
	if err != nil {
		t.Errorf("Unexpectected error: %q", err)
	}

	if priority != expectedPriority {
		t.Errorf("Expected priority %d. Got priority %d.", expectedPriority, priority)
	}
}

func TestSecurityRulePriorityFailsIfExhausted(t *testing.T) {
	rules := []network.SecurityRule{}

	var i int32
	for i = consts.LoadBalancerMinimumPriority; i < consts.LoadBalancerMaximumPriority; i++ {
		rules = append(rules, network.SecurityRule{
			SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
				Priority: pointer.Int32(i),
			},
		})
	}

	_, err := getNextAvailablePriority(rules)
	if err == nil {
		t.Error("Expect an error. There are no priority levels left.")
	}
}

func TestProtocolTranslationTCP(t *testing.T) {
	proto := v1.ProtocolTCP
	transportProto, securityGroupProto, probeProto, err := getProtocolsFromKubernetesProtocol(proto)
	if err != nil {
		t.Error(err)
	}

	if *transportProto != network.TransportProtocolTCP {
		t.Errorf("Expected TCP LoadBalancer Rule Protocol. Got %v", transportProto)
	}
	if *securityGroupProto != network.SecurityRuleProtocolTCP {
		t.Errorf("Expected TCP SecurityGroup Protocol. Got %v", transportProto)
	}
	if *probeProto != network.ProbeProtocolTCP {
		t.Errorf("Expected TCP LoadBalancer Probe Protocol. Got %v", transportProto)
	}
}

func TestProtocolTranslationUDP(t *testing.T) {
	proto := v1.ProtocolUDP
	transportProto, securityGroupProto, probeProto, _ := getProtocolsFromKubernetesProtocol(proto)
	if *transportProto != network.TransportProtocolUDP {
		t.Errorf("Expected UDP LoadBalancer Rule Protocol. Got %v", transportProto)
	}
	if *securityGroupProto != network.SecurityRuleProtocolUDP {
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
		"availabilitySetNodesCacheTTLInSeconds": 100,
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
	validateConfig(t, config)
}

// Test Backoff and Rate Limit defaults (json)
func TestCloudDefaultConfigFromJSON(t *testing.T) {
	config := `{
                "aadClientId": "--aad-client-id--",
                "aadClientSecret": "--aad-client-secret--"
        }`

	validateEmptyConfig(t, config)
}

// Test Backoff and Rate Limit defaults (yaml)
func TestCloudDefaultConfigFromYAML(t *testing.T) {
	config := `
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
availabilitySetNodesCacheTTLInSeconds: 100
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
	if azureCloud.AvailabilitySetNodesCacheTTLInSeconds != 100 {
		t.Errorf("got incorrect value for availabilitySetNodesCacheTTLInSeconds")
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
}

func getCloudFromConfig(t *testing.T, config string) *Cloud {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	configReader := strings.NewReader(config)
	c, err := ParseConfig(configReader)
	assert.NoError(t, err)

	az := &Cloud{
		nodeNames:                sets.NewString(),
		nodeZones:                map[string]sets.String{},
		nodeResourceGroups:       map[string]string{},
		unmanagedNodes:           sets.NewString(),
		excludeLoadBalancerNodes: sets.NewString(),
		routeCIDRs:               map[string]string{},
		ZoneClient:               mockzoneclient.NewMockInterface(ctrl),
	}
	mockZoneClient := az.ZoneClient.(*mockzoneclient.MockInterface)
	mockZoneClient.EXPECT().GetZones(gomock.Any(), gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil)

	err = az.InitializeCloudFromConfig(context.Background(), c, false, true)
	assert.NoError(t, err)

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
		name, err := az.VMSet.GetNodeNameByProviderID(test.providerID)
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
	mockSubnetsClient := az.SubnetsClient.(*mocksubnetclient.MockInterface)
	mockSubnetsClient.EXPECT().Get(gomock.Any(), az.VnetResourceGroup, az.VnetName, subName, "").Return(
		network.Subnet{
			ID:   &subnetID,
			Name: &subName,
		}, nil).AnyTimes()
}

func TestIfServiceSpecifiesSharedRuleAndRuleDoesNotExistItIsCreated(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestService("servicea", v1.ProtocolTCP, nil, false, 80)
	setServiceLoadBalancerIP(&svc, testIP1)
	svc.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	sg := getTestSecurityGroup(az)
	setMockSecurityGroup(az, ctrl, sg)

	sg, err := az.reconcileSecurityGroup(testClusterName, &svc, &[]string{getServiceLoadBalancerIP(&svc, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc)

	expectedRuleName := testRuleName
	_, securityRule, ruleFound := findSecurityRuleByName(*sg.SecurityRules, expectedRuleName)
	if !ruleFound {
		t.Fatalf("Expected security rule %q but it was not present", expectedRuleName)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 80}, testIP1, securityRule)
	if err != nil {
		t.Errorf("Shared rule was not updated with new service IP: %v", err)
	}

	if securityRule.Priority == nil {
		t.Errorf("Shared rule %s had no priority", expectedRuleName)
	}

	if securityRule.Access != network.SecurityRuleAccessAllow {
		t.Errorf("Shared rule %s did not have Allow access", expectedRuleName)
	}

	if securityRule.Direction != network.SecurityRuleDirectionInbound {
		t.Errorf("Shared rule %s did not have Inbound direction", expectedRuleName)
	}
}

func TestIfServiceSpecifiesSharedRuleAndRuleExistsThenTheServicesPortAndAddressAreAdded(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloud(ctrl)
	svc := getTestService("servicesr", v1.ProtocolTCP, nil, false, 80)
	setServiceLoadBalancerIP(&svc, testIP1)
	svc.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	expectedRuleName := testRuleName

	sg := getTestSecurityGroup(az)
	sg.SecurityRules = &[]network.SecurityRule{
		{
			Name: &expectedRuleName,
			SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
				Protocol:                 network.SecurityRuleProtocolTCP,
				SourcePortRange:          pointer.String("*"),
				SourceAddressPrefix:      pointer.String("Internet"),
				DestinationPortRange:     pointer.String("80"),
				DestinationAddressPrefix: pointer.String(testIP2),
				Access:                   network.SecurityRuleAccessAllow,
				Direction:                network.SecurityRuleDirectionInbound,
			},
		},
	}
	setMockSecurityGroup(az, ctrl, sg)

	sg, err := az.reconcileSecurityGroup(testClusterName, &svc, &[]string{getServiceLoadBalancerIP(&svc, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc)

	_, securityRule, ruleFound := findSecurityRuleByName(*sg.SecurityRules, expectedRuleName)
	if !ruleFound {
		t.Fatalf("Expected security rule %q but it was not present", expectedRuleName)
	}

	expectedDestinationIPCount := 2
	if len(*securityRule.DestinationAddressPrefixes) != expectedDestinationIPCount {
		t.Errorf("Shared rule should have had %d destination IP addresses but had %d", expectedDestinationIPCount, len(*securityRule.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 80}, testIP2, securityRule)
	if err != nil {
		t.Errorf("Shared rule no longer matched other service IP: %v", err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 80}, testIP1, securityRule)
	if err != nil {
		t.Errorf("Shared rule was not updated with new service IP: %v", err)
	}
}

func TestIfServicesSpecifySharedRuleButDifferentPortsThenSeparateRulesAreCreated(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	svc1 := getTestService("servicesr1", v1.ProtocolTCP, nil, false, 4444)
	setServiceLoadBalancerIP(&svc1, testIP1)
	svc1.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc2 := getTestService("servicesr2", v1.ProtocolTCP, nil, false, 8888)
	setServiceLoadBalancerIP(&svc2, testIP2)
	svc2.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	sg := getTestSecurityGroup(az)

	setMockSecurityGroup(az, ctrl, sg)
	sg, err := az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{getServiceLoadBalancerIP(&svc1, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc1: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc2, &[]string{getServiceLoadBalancerIP(&svc2, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc2: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc1, svc2)

	_, securityRule1, rule1Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName2)
	if !rule1Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName2)
	}

	_, securityRule2, rule2Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName3)
	if !rule2Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName3)
	}

	expectedDestinationIPCount1 := 1
	if len(*securityRule1.DestinationAddressPrefixes) != expectedDestinationIPCount1 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName2, expectedDestinationIPCount1, len(*securityRule1.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP1, securityRule1)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName2, err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 8888}, testIP2, securityRule1)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName2)
	}

	expectedDestinationIPCount2 := 1
	if len(*securityRule2.DestinationAddressPrefixes) != expectedDestinationIPCount2 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName3, expectedDestinationIPCount2, len(*securityRule2.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 8888}, testIP2, securityRule2)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName3, err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP1, securityRule2)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName3)
	}
}

func TestIfServicesSpecifySharedRuleButDifferentProtocolsThenSeparateRulesAreCreated(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	svc1 := getTestService("servicesr1", v1.ProtocolTCP, nil, false, 4444)

	setServiceLoadBalancerIP(&svc1, testIP1)
	svc1.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc2 := getTestService("servicesr2", v1.ProtocolUDP, nil, false, 4444)
	setServiceLoadBalancerIP(&svc2, testIP1)
	svc2.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	testRuleName3 := "shared-UDP-4444-Internet"

	sg := getTestSecurityGroup(az)

	setMockSecurityGroup(az, ctrl, sg)
	sg, err := az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{getServiceLoadBalancerIP(&svc1, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc1: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc2, &[]string{getServiceLoadBalancerIP(&svc2, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc2: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc1, svc2)

	_, securityRule1, rule1Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName2)
	if !rule1Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName2)
	}

	_, securityRule2, rule2Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName3)
	if !rule2Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName3)
	}

	expectedDestinationIPCount1 := 1
	if len(*securityRule1.DestinationAddressPrefixes) != expectedDestinationIPCount1 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName2, expectedDestinationIPCount1, len(*securityRule1.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP1, securityRule1)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName2, err)
	}

	if securityRule1.Protocol != network.SecurityRuleProtocolTCP {
		t.Errorf("Shared rule %s should have been %s but was %s", testRuleName2, network.SecurityRuleProtocolTCP, securityRule1.Protocol)
	}

	expectedDestinationIPCount2 := 1
	if len(*securityRule2.DestinationAddressPrefixes) != expectedDestinationIPCount2 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName3, expectedDestinationIPCount2, len(*securityRule2.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP1, securityRule2)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName3, err)
	}

	if securityRule2.Protocol != network.SecurityRuleProtocolUDP {
		t.Errorf("Shared rule %s should have been %s but was %s", testRuleName3, network.SecurityRuleProtocolUDP, securityRule2.Protocol)
	}
}

func TestIfServicesSpecifySharedRuleButDifferentSourceAddressesThenSeparateRulesAreCreated(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	svc1 := getTestService("servicesr1", v1.ProtocolTCP, nil, false, 80)
	setServiceLoadBalancerIP(&svc1, testIP1)
	svc1.Spec.LoadBalancerSourceRanges = []string{"192.168.12.0/24"}
	svc1.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc2 := getTestService("servicesr2", v1.ProtocolTCP, nil, false, 80)
	setServiceLoadBalancerIP(&svc2, testIP2)
	svc2.Spec.LoadBalancerSourceRanges = []string{"192.168.34.0/24"}
	svc2.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	testRuleName2 := "shared-TCP-80-192.168.12.0_24"
	testRuleName3 := "shared-TCP-80-192.168.34.0_24"

	sg := getTestSecurityGroup(az)

	setMockSecurityGroup(az, ctrl, sg)
	sg, err := az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{getServiceLoadBalancerIP(&svc1, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc1: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc2, &[]string{getServiceLoadBalancerIP(&svc2, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc2: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc1, svc2)

	_, securityRule1, rule1Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName2)
	if !rule1Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName2)
	}

	_, securityRule2, rule2Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName3)
	if !rule2Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName3)
	}

	expectedDestinationIPCount1 := 1
	if len(*securityRule1.DestinationAddressPrefixes) != expectedDestinationIPCount1 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName2, expectedDestinationIPCount1, len(*securityRule1.DestinationAddressPrefixes))
	}

	err = securityRuleMatches(svc1.Spec.LoadBalancerSourceRanges[0], v1.ServicePort{Port: 80}, testIP1, securityRule1)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName2, err)
	}

	err = securityRuleMatches(svc2.Spec.LoadBalancerSourceRanges[0], v1.ServicePort{Port: 80}, testIP2, securityRule1)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName2)
	}

	expectedDestinationIPCount2 := 1
	if len(*securityRule2.DestinationAddressPrefixes) != expectedDestinationIPCount2 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName3, expectedDestinationIPCount2, len(*securityRule2.DestinationAddressPrefixes))
	}

	err = securityRuleMatches(svc2.Spec.LoadBalancerSourceRanges[0], v1.ServicePort{Port: 80}, testIP2, securityRule2)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName3, err)
	}

	err = securityRuleMatches(svc1.Spec.LoadBalancerSourceRanges[0], v1.ServicePort{Port: 80}, testIP1, securityRule2)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName3)
	}
}

func TestIfServicesSpecifySharedRuleButSomeAreOnDifferentPortsThenRulesAreSeparatedOrConsoliatedByPort(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	svc1 := getTestService("servicesr1", v1.ProtocolTCP, nil, false, 4444)
	setServiceLoadBalancerIP(&svc1, testIP1)
	svc1.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc2 := getTestService("servicesr2", v1.ProtocolTCP, nil, false, 8888)
	setServiceLoadBalancerIP(&svc2, testIP2)
	svc2.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc3 := getTestService("servicesr3", v1.ProtocolTCP, nil, false, 4444)
	setServiceLoadBalancerIP(&svc3, testIP3)
	svc3.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	testRuleName23 := testRuleName2
	sg := getTestSecurityGroup(az)

	setMockSecurityGroup(az, ctrl, sg)
	sg, err := az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{getServiceLoadBalancerIP(&svc1, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc1: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc2, &[]string{getServiceLoadBalancerIP(&svc2, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc2: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc3, &[]string{getServiceLoadBalancerIP(&svc3, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc3: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc1, svc2, svc3)

	_, securityRule13, rule13Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName23)
	if !rule13Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName23)
	}

	_, securityRule2, rule2Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName3)
	if !rule2Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName3)
	}

	expectedDestinationIPCount13 := 2
	if len(*securityRule13.DestinationAddressPrefixes) != expectedDestinationIPCount13 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName23, expectedDestinationIPCount13, len(*securityRule13.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP1, securityRule13)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName23, err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP3, securityRule13)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName23, err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 8888}, testIP2, securityRule13)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName23)
	}

	if securityRule13.Priority == nil {
		t.Errorf("Shared rule %s had no priority", testRuleName23)
	}

	if securityRule13.Access != network.SecurityRuleAccessAllow {
		t.Errorf("Shared rule %s did not have Allow access", testRuleName23)
	}

	if securityRule13.Direction != network.SecurityRuleDirectionInbound {
		t.Errorf("Shared rule %s did not have Inbound direction", testRuleName23)
	}

	expectedDestinationIPCount2 := 1
	if len(*securityRule2.DestinationAddressPrefixes) != expectedDestinationIPCount2 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName3, expectedDestinationIPCount2, len(*securityRule2.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 8888}, testIP2, securityRule2)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName3, err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP1, securityRule2)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName3)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP3, securityRule2)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName3)
	}
}

func TestIfServiceSpecifiesSharedRuleAndServiceIsDeletedThenTheServicesPortAndAddressAreRemoved(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	svc1 := getTestService("servicesr1", v1.ProtocolTCP, nil, false, 80)
	setServiceLoadBalancerIP(&svc1, testIP1)
	svc1.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc2 := getTestService("servicesr2", v1.ProtocolTCP, nil, false, 80)
	setServiceLoadBalancerIP(&svc2, testIP2)
	svc2.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	expectedRuleName := testRuleName

	sg := getTestSecurityGroup(az)

	setMockSecurityGroup(az, ctrl, sg)
	sg, err := az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{getServiceLoadBalancerIP(&svc1, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc1: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc2, &[]string{getServiceLoadBalancerIP(&svc2, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc2: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc1, svc2)

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{getServiceLoadBalancerIP(&svc1, false)}, nil, false)
	if err != nil {
		t.Errorf("Unexpected error removing svc1: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc2)

	_, securityRule, ruleFound := findSecurityRuleByName(*sg.SecurityRules, expectedRuleName)
	if !ruleFound {
		t.Fatalf("Expected security rule %q but it was not present", expectedRuleName)
	}

	expectedDestinationIPCount := 1
	if len(*securityRule.DestinationAddressPrefixes) != expectedDestinationIPCount {
		t.Errorf("Shared rule should have had %d destination IP addresses but had %d", expectedDestinationIPCount, len(*securityRule.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 80}, testIP2, securityRule)
	if err != nil {
		t.Errorf("Shared rule no longer matched other service IP: %v", err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 80}, testIP1, securityRule)
	if err == nil {
		t.Error("Shared rule was not updated to remove deleted service IP")
	}
}

func TestIfSomeServicesShareARuleAndOneIsDeletedItIsRemovedFromTheRightRule(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	svc1 := getTestService("servicesr1", v1.ProtocolTCP, nil, false, 4444)
	setServiceLoadBalancerIP(&svc1, testIP1)
	svc1.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc2 := getTestService("servicesr2", v1.ProtocolTCP, nil, false, 8888)
	setServiceLoadBalancerIP(&svc2, testIP2)
	svc2.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc3 := getTestService("servicesr3", v1.ProtocolTCP, nil, false, 4444)
	setServiceLoadBalancerIP(&svc3, testIP3)
	svc3.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	testRuleName23 := testRuleName2
	sg := getTestSecurityGroup(az)

	setMockSecurityGroup(az, ctrl, sg)
	sg, err := az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{getServiceLoadBalancerIP(&svc1, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc1: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc2, &[]string{getServiceLoadBalancerIP(&svc2, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc2: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc3, &[]string{getServiceLoadBalancerIP(&svc3, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc3: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc1, svc2, svc3)

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{getServiceLoadBalancerIP(&svc1, false)}, nil, false)
	if err != nil {
		t.Errorf("Unexpected error removing svc1: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc2, svc3)

	_, securityRule13, rule13Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName23)
	if !rule13Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName23)
	}

	_, securityRule2, rule2Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName3)
	if !rule2Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName3)
	}

	expectedDestinationIPCount13 := 1
	if len(*securityRule13.DestinationAddressPrefixes) != expectedDestinationIPCount13 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName23, expectedDestinationIPCount13, len(*securityRule13.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP1, securityRule13)
	if err == nil {
		t.Errorf("Shared rule %s should have had svc1 removed but did not", testRuleName23)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP3, securityRule13)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName23, err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 8888}, testIP2, securityRule13)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName23)
	}

	if securityRule13.Priority == nil {
		t.Errorf("Shared rule %s had no priority", testRuleName23)
	}

	if securityRule13.Access != network.SecurityRuleAccessAllow {
		t.Errorf("Shared rule %s did not have Allow access", testRuleName23)
	}

	if securityRule13.Direction != network.SecurityRuleDirectionInbound {
		t.Errorf("Shared rule %s did not have Inbound direction", testRuleName23)
	}

	expectedDestinationIPCount2 := 1
	if len(*securityRule2.DestinationAddressPrefixes) != expectedDestinationIPCount2 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName3, expectedDestinationIPCount2, len(*securityRule2.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 8888}, testIP2, securityRule2)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName3, err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP1, securityRule2)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName3)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP3, securityRule2)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName3)
	}
}

func TestIfServiceSpecifiesSharedRuleAndLastServiceIsDeletedThenRuleIsDeleted(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	svc1 := getTestService("servicesr1", v1.ProtocolTCP, nil, false, 4444)
	setServiceLoadBalancerIP(&svc1, testIP1)
	svc1.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc2 := getTestService("servicesr2", v1.ProtocolTCP, nil, false, 8888)
	setServiceLoadBalancerIP(&svc2, testIP2)
	svc2.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc3 := getTestService("servicesr3", v1.ProtocolTCP, nil, false, 4444)
	setServiceLoadBalancerIP(&svc3, testIP3)
	svc3.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	testRuleName23 := testRuleName2
	sg := getTestSecurityGroup(az)

	setMockSecurityGroup(az, ctrl, sg)
	sg, err := az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{getServiceLoadBalancerIP(&svc1, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc1: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc2, &[]string{getServiceLoadBalancerIP(&svc2, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc2: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc3, &[]string{getServiceLoadBalancerIP(&svc3, false)}, nil, true)
	if err != nil {
		t.Errorf("Unexpected error adding svc3: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc1, svc2, svc3)

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{getServiceLoadBalancerIP(&svc1, false)}, nil, false)
	if err != nil {
		t.Errorf("Unexpected error removing svc1: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc3, &[]string{getServiceLoadBalancerIP(&svc3, false)}, nil, false)
	if err != nil {
		t.Errorf("Unexpected error removing svc3: %q", err)
	}

	validateSecurityGroup(t, az, sg, svc2)

	_, _, rule13Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName23)
	if rule13Found {
		t.Fatalf("Expected security rule %q to have been deleted but it was still present", testRuleName23)
	}

	_, securityRule2, rule2Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName3)
	if !rule2Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName3)
	}

	expectedDestinationIPCount2 := 1
	if len(*securityRule2.DestinationAddressPrefixes) != expectedDestinationIPCount2 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName3, expectedDestinationIPCount2, len(*securityRule2.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 8888}, testIP2, securityRule2)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName3, err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP1, securityRule2)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName3)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP3, securityRule2)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong service's port and IP", testRuleName3)
	}
}

func TestCanCombineSharedAndPrivateRulesInSameGroup(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)
	var err error

	svc1 := getTestService("servicesr1", v1.ProtocolTCP, nil, false, 4444)
	setServiceLoadBalancerIP(&svc1, testIP1)
	svc1.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc2 := getTestService("servicesr2", v1.ProtocolTCP, nil, false, 8888)
	setServiceLoadBalancerIP(&svc2, testIP2)
	svc2.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc3 := getTestService("servicesr3", v1.ProtocolTCP, nil, false, 4444)
	setServiceLoadBalancerIP(&svc3, testIP3)
	svc3.Annotations[consts.ServiceAnnotationSharedSecurityRule] = consts.TrueAnnotationValue

	svc4 := getTestService("servicesr4", v1.ProtocolTCP, nil, false, 4444)
	setServiceLoadBalancerIP(&svc4, "192.168.22.33")
	svc4.Annotations[consts.ServiceAnnotationSharedSecurityRule] = "false"

	svc5 := getTestService("servicesr5", v1.ProtocolTCP, nil, false, 8888)
	setServiceLoadBalancerIP(&svc5, "192.168.22.33")
	svc5.Annotations[consts.ServiceAnnotationSharedSecurityRule] = "false"

	testServices := []v1.Service{svc1, svc2, svc3, svc4, svc5}

	testRuleName23 := testRuleName2
	expectedRuleName4 := az.getSecurityRuleName(&svc4, v1.ServicePort{Port: 4444, Protocol: v1.ProtocolTCP}, "Internet", false)
	expectedRuleName5 := az.getSecurityRuleName(&svc5, v1.ServicePort{Port: 8888, Protocol: v1.ProtocolTCP}, "Internet", false)

	sg := getTestSecurityGroup(az)

	for i, svc := range testServices {
		svc := svc
		setMockSecurityGroup(az, ctrl, sg)
		sg, err = az.reconcileSecurityGroup(testClusterName, &testServices[i], &[]string{getServiceLoadBalancerIP(&svc, false)}, nil, true)
		if err != nil {
			t.Errorf("Unexpected error adding svc%d: %q", i+1, err)
		}
	}

	validateSecurityGroup(t, az, sg, svc1, svc2, svc3, svc4, svc5)

	expectedRuleCount := 4
	if len(*sg.SecurityRules) != expectedRuleCount {
		t.Errorf("Expected security group to have %d rules but it had %d", expectedRuleCount, len(*sg.SecurityRules))
	}

	_, securityRule13, rule13Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName23)
	if !rule13Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName23)
	}

	_, securityRule2, rule2Found := findSecurityRuleByName(*sg.SecurityRules, testRuleName3)
	if !rule2Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName3)
	}

	_, securityRule4, rule4Found := findSecurityRuleByName(*sg.SecurityRules, expectedRuleName4)
	if !rule4Found {
		t.Fatalf("Expected security rule %q but it was not present", expectedRuleName4)
	}

	_, securityRule5, rule5Found := findSecurityRuleByName(*sg.SecurityRules, expectedRuleName5)
	if !rule5Found {
		t.Fatalf("Expected security rule %q but it was not present", expectedRuleName5)
	}

	expectedDestinationIPCount13 := 2
	if len(*securityRule13.DestinationAddressPrefixes) != expectedDestinationIPCount13 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName23, expectedDestinationIPCount13, len(*securityRule13.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP1, securityRule13)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName23, err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, testIP3, securityRule13)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName23, err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 4444}, "192.168.22.33", securityRule13)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong (unshared) service's port and IP", testRuleName23)
	}

	expectedDestinationIPCount2 := 1
	if len(*securityRule2.DestinationAddressPrefixes) != expectedDestinationIPCount2 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName3, expectedDestinationIPCount2, len(*securityRule2.DestinationAddressPrefixes))
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 8888}, testIP2, securityRule2)
	if err != nil {
		t.Errorf("Shared rule %s did not match service IP: %v", testRuleName3, err)
	}

	err = securityRuleMatches("Internet", v1.ServicePort{Port: 8888}, "192.168.22.33", securityRule2)
	if err == nil {
		t.Errorf("Shared rule %s matched wrong (unshared) service's port and IP", testRuleName3)
	}

	if securityRule4.DestinationAddressPrefixes != nil {
		t.Errorf("Expected unshared rule %s to use single destination IP address but used collection", expectedRuleName4)
	}

	if securityRule4.DestinationAddressPrefix == nil {
		t.Errorf("Expected unshared rule %s to have a destination IP address", expectedRuleName4)
	} else {
		if !strings.EqualFold(*securityRule4.DestinationAddressPrefix, getServiceLoadBalancerIP(&svc4, false)) {
			t.Errorf("Expected unshared rule %s to have a destination %s but had %s", expectedRuleName4, getServiceLoadBalancerIP(&svc4, false), *securityRule4.DestinationAddressPrefix)
		}
	}

	if securityRule5.DestinationAddressPrefixes != nil {
		t.Errorf("Expected unshared rule %s to use single destination IP address but used collection", expectedRuleName5)
	}

	if securityRule5.DestinationAddressPrefix == nil {
		t.Errorf("Expected unshared rule %s to have a destination IP address", expectedRuleName5)
	} else {
		if !strings.EqualFold(*securityRule5.DestinationAddressPrefix, getServiceLoadBalancerIP(&svc5, false)) {
			t.Errorf("Expected unshared rule %s to have a destination %s but had %s", expectedRuleName5, getServiceLoadBalancerIP(&svc5, false), *securityRule5.DestinationAddressPrefix)
		}
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc1, &[]string{getServiceLoadBalancerIP(&svc1, false)}, nil, false)
	if err != nil {
		t.Errorf("Unexpected error removing svc1: %q", err)
	}

	setMockSecurityGroup(az, ctrl, sg)
	sg, err = az.reconcileSecurityGroup(testClusterName, &svc5, &[]string{getServiceLoadBalancerIP(&svc5, false)}, nil, false)
	if err != nil {
		t.Errorf("Unexpected error removing svc5: %q", err)
	}

	_, securityRule13, rule13Found = findSecurityRuleByName(*sg.SecurityRules, testRuleName23)
	if !rule13Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName23)
	}

	_, _, rule2Found = findSecurityRuleByName(*sg.SecurityRules, testRuleName3)
	if !rule2Found {
		t.Fatalf("Expected security rule %q but it was not present", testRuleName3)
	}

	_, _, rule4Found = findSecurityRuleByName(*sg.SecurityRules, expectedRuleName4)
	if !rule4Found {
		t.Fatalf("Expected security rule %q but it was not present", expectedRuleName4)
	}

	_, _, rule5Found = findSecurityRuleByName(*sg.SecurityRules, expectedRuleName5)
	if rule5Found {
		t.Fatalf("Expected security rule %q to have been removed but it was not present", expectedRuleName5)
	}

	expectedDestinationIPCount13 = 1
	if len(*securityRule13.DestinationAddressPrefixes) != expectedDestinationIPCount13 {
		t.Errorf("Shared rule %s should have had %d destination IP addresses but had %d", testRuleName23, expectedDestinationIPCount13, len(*securityRule13.DestinationAddressPrefixes))
	}
}

func TestGetInfoFromDiskURI(t *testing.T) {
	tests := []struct {
		diskURL        string
		expectedRG     string
		expectedSubsID string
		expectError    bool
	}{
		{
			diskURL:        "/subscriptions/4be8920b-2978-43d7-axyz-04d8549c1d05/resourceGroups/azure-k8s1102/providers/Microsoft.Compute/disks/andy-mghyb1102-dynamic-pvc-f7f014c9-49f4-11e8-ab5c-000d3af7b38e",
			expectedRG:     "azure-k8s1102",
			expectedSubsID: "4be8920b-2978-43d7-axyz-04d8549c1d05",
			expectError:    false,
		},
		{
			// case insensitive check
			diskURL:        "/subscriptions/4be8920b-2978-43d7-axyz-04d8549c1d05/resourcegroups/azure-k8s1102/providers/Microsoft.Compute/disks/andy-mghyb1102-dynamic-pvc-f7f014c9-49f4-11e8-ab5c-000d3af7b38e",
			expectedRG:     "azure-k8s1102",
			expectedSubsID: "4be8920b-2978-43d7-axyz-04d8549c1d05",
			expectError:    false,
		},
		{
			diskURL:     "/4be8920b-2978-43d7-axyz-04d8549c1d05/resourceGroups/azure-k8s1102/providers/Microsoft.Compute/disks/andy-mghyb1102-dynamic-pvc-f7f014c9-49f4-11e8-ab5c-000d3af7b38e",
			expectedRG:  "",
			expectError: true,
		},
		{
			diskURL:     "",
			expectedRG:  "",
			expectError: true,
		},
	}

	for _, test := range tests {
		rg, subsID, err := getInfoFromDiskURI(test.diskURL)
		assert.Equal(t, rg, test.expectedRG, "Expect result not equal with getInfoFromDiskURI(%s) return: %q, expected: %q",
			test.diskURL, rg, test.expectedRG)
		assert.Equal(t, subsID, test.expectedSubsID, "Expect result not equal with getInfoFromDiskURI(%s) return: %q, expected: %q",
			test.diskURL, subsID, test.expectedSubsID)

		if test.expectError {
			assert.NotNil(t, err, "Expect error during getInfoFromDiskURI(%s)", test.diskURL)
		} else {
			assert.Nil(t, err, "Expect error is nil during getInfoFromDiskURI(%s)", test.diskURL)
		}
	}
}

func TestGetResourceGroups(t *testing.T) {
	tests := []struct {
		name               string
		nodeResourceGroups map[string]string
		expected           sets.String
		informerSynced     bool
		expectError        bool
	}{
		{
			name:               "cloud provider configured RG should be returned by default",
			nodeResourceGroups: map[string]string{},
			informerSynced:     true,
			expected:           sets.NewString("rg"),
		},
		{
			name:               "cloud provider configured RG and node RGs should be returned",
			nodeResourceGroups: map[string]string{"node1": "rg1", "node2": "rg2"},
			informerSynced:     true,
			expected:           sets.NewString("rg", "rg1", "rg2"),
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
	nodesInZone := sets.NewString("prevNode")
	az.nodeZones = map[string]sets.String{zone: nodesInZone}
	az.nodeResourceGroups = map[string]string{"prevNode": "rg"}
	az.unmanagedNodes = sets.NewString("prevNode")
	az.excludeLoadBalancerNodes = sets.NewString("prevNode")
	az.nodeNames = sets.NewString("prevNode")

	prevNode := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				consts.LabelFailureDomainBetaZone: zone,
				consts.ExternalResourceGroupLabel: consts.TrueAnnotationValue,
				consts.ManagedByAzureLabel:        "false",
			},
			Name: "prevNode",
		},
	}

	az.updateNodeCaches(&prevNode, nil)
	assert.Equal(t, 0, len(az.nodeZones[zone]))
	assert.Equal(t, 0, len(az.nodeResourceGroups))
	assert.Equal(t, 0, len(az.unmanagedNodes))
	assert.Equal(t, 1, len(az.excludeLoadBalancerNodes))
	assert.Equal(t, 0, len(az.nodeNames))

	// add new (unmanaged and to-be-excluded) node
	newNode := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				consts.LabelFailureDomainBetaZone: zone,
				consts.ExternalResourceGroupLabel: consts.TrueAnnotationValue,
				consts.ManagedByAzureLabel:        "false",
				v1.LabelNodeExcludeBalancers:      "true",
			},
			Name: "newNode",
		},
	}

	az.updateNodeCaches(nil, &newNode)
	assert.Equal(t, 1, len(az.nodeZones[zone]))
	assert.Equal(t, 1, len(az.nodeResourceGroups))
	assert.Equal(t, 1, len(az.unmanagedNodes))
	assert.Equal(t, 2, len(az.excludeLoadBalancerNodes))
	assert.Equal(t, 1, len(az.nodeNames))
}

func TestUpdateNodeCacheExcludeLoadBalancer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	zone := fmt.Sprintf("%s-0", az.Location)
	nodesInZone := sets.NewString("aNode")
	az.nodeZones = map[string]sets.String{zone: nodesInZone}
	az.nodeResourceGroups = map[string]string{"aNode": "rg"}

	// a non-ready node should be excluded
	az.unmanagedNodes = sets.NewString()
	az.excludeLoadBalancerNodes = sets.NewString()
	az.nodeNames = sets.NewString()
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
	assert.Equal(t, 1, len(az.excludeLoadBalancerNodes))

	// node becomes ready, it should be removed from az.excludeLoadBalancerNodes
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
	assert.Equal(t, 0, len(az.excludeLoadBalancerNodes))

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
	assert.Equal(t, 0, len(az.excludeLoadBalancerNodes))
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
	nodesInZone := sets.NewString("node1")
	az.nodeZones = map[string]sets.String{zone: nodesInZone}

	expectedZones := sets.NewString(zone)
	zones, err = az.GetActiveZones()
	assert.Equal(t, expectedZones, zones)
	assert.NoError(t, err)
}

func TestInitializeCloudFromConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	az := GetTestCloud(ctrl)

	mockZoneClient := mockzoneclient.NewMockInterface(ctrl)
	mockZoneClient.EXPECT().GetZones(gomock.Any(), gomock.Any()).Return(map[string][]string{"eastus": {"1", "2", "3"}}, nil).AnyTimes()
	az.ZoneClient = mockZoneClient

	err := az.InitializeCloudFromConfig(context.Background(), nil, false, true)
	assert.Equal(t, fmt.Errorf("InitializeCloudFromConfig: cannot initialize from nil config"), err)

	config := Config{
		DisableAvailabilitySetNodes: true,
		VMType:                      consts.VMTypeStandard,
	}
	err = az.InitializeCloudFromConfig(context.Background(), &config, false, true)
	expectedErr := fmt.Errorf("disableAvailabilitySetNodes true is only supported when vmType is 'vmss'")
	assert.Equal(t, expectedErr, err)

	config = Config{
		AzureAuthConfig: providerconfig.AzureAuthConfig{
			Cloud: "AZUREPUBLICCLOUD",
		},
		CloudConfigType: cloudConfigTypeFile,
	}
	err = az.InitializeCloudFromConfig(context.Background(), &config, false, true)
	expectedErr = fmt.Errorf("useInstanceMetadata must be enabled without Azure credentials")
	assert.Equal(t, expectedErr, err)

	config = Config{
		LoadBalancerBackendPoolConfigurationType: "invalid",
	}
	err = az.InitializeCloudFromConfig(context.Background(), &config, false, true)
	expectedErr = errors.New("loadBalancerBackendPoolConfigurationType invalid is not supported, supported values are")
	assert.Contains(t, err.Error(), expectedErr.Error())

	config = Config{}
	err = az.InitializeCloudFromConfig(context.Background(), &config, false, true)
	assert.NoError(t, err)
	assert.Equal(t, az.Config.LoadBalancerBackendPoolConfigurationType, consts.LoadBalancerBackendPoolConfigurationTypeNodeIPConfiguration)
}

func TestFindSecurityRule(t *testing.T) {
	sg := network.SecurityRule{
		Name: pointer.String(testRuleName4),
		SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
			Protocol:                   network.SecurityRuleProtocolTCP,
			SourcePortRange:            pointer.String("*"),
			SourceAddressPrefix:        pointer.String("Internet"),
			DestinationPortRange:       pointer.String("80"),
			DestinationAddressPrefix:   pointer.String(testIP1),
			DestinationAddressPrefixes: &([]string{}),
			Access:                     network.SecurityRuleAccessAllow,
			Direction:                  network.SecurityRuleDirectionInbound,
		},
	}
	testCases := []struct {
		desc     string
		testRule network.SecurityRule
		expected bool
	}{
		{
			desc:     "false should be returned for an empty rule",
			testRule: network.SecurityRule{},
			expected: false,
		},
		{
			desc: "false should be returned when rule name doesn't match",
			testRule: network.SecurityRule{
				Name: pointer.String("not-the-right-name"),
			},
			expected: false,
		},
		{
			desc: "false should be returned when protocol doesn't match",
			testRule: network.SecurityRule{
				Name: pointer.String(testRuleName4),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol: network.SecurityRuleProtocolUDP,
				},
			},
			expected: false,
		},
		{
			desc: "false should be returned when SourcePortRange doesn't match",
			testRule: network.SecurityRule{
				Name: pointer.String(testRuleName4),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:        network.SecurityRuleProtocolUDP,
					SourcePortRange: pointer.String("1.2.3.4/32"),
				},
			},
			expected: false,
		},
		{
			desc: "false should be returned when SourceAddressPrefix doesn't match",
			testRule: network.SecurityRule{
				Name: pointer.String(testRuleName4),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:            network.SecurityRuleProtocolUDP,
					SourcePortRange:     pointer.String("*"),
					SourceAddressPrefix: pointer.String("2.3.4.0/24"),
				},
			},
			expected: false,
		},
		{
			desc: "false should be returned when DestinationPortRange doesn't match",
			testRule: network.SecurityRule{
				Name: pointer.String(testRuleName4),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:             network.SecurityRuleProtocolUDP,
					SourcePortRange:      pointer.String("*"),
					SourceAddressPrefix:  pointer.String("Internet"),
					DestinationPortRange: pointer.String("443"),
				},
			},
			expected: false,
		},
		{
			desc: "false should be returned when DestinationAddressPrefix doesn't match",
			testRule: network.SecurityRule{
				Name: pointer.String(testRuleName4),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                 network.SecurityRuleProtocolUDP,
					SourcePortRange:          pointer.String("*"),
					SourceAddressPrefix:      pointer.String("Internet"),
					DestinationPortRange:     pointer.String("80"),
					DestinationAddressPrefix: pointer.String(testIP2),
				},
			},
			expected: false,
		},
		{
			desc: "false should be returned when Access doesn't match",
			testRule: network.SecurityRule{
				Name: pointer.String(testRuleName4),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                 network.SecurityRuleProtocolUDP,
					SourcePortRange:          pointer.String("*"),
					SourceAddressPrefix:      pointer.String("Internet"),
					DestinationPortRange:     pointer.String("80"),
					DestinationAddressPrefix: pointer.String(testIP1),
					Access:                   network.SecurityRuleAccessDeny,
					// Direction:                network.SecurityRuleDirectionInbound,
				},
			},
			expected: false,
		},
		{
			desc: "false should be returned when Direction doesn't match",
			testRule: network.SecurityRule{
				Name: pointer.String(testRuleName4),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                 network.SecurityRuleProtocolUDP,
					SourcePortRange:          pointer.String("*"),
					SourceAddressPrefix:      pointer.String("Internet"),
					DestinationPortRange:     pointer.String("80"),
					DestinationAddressPrefix: pointer.String(testIP1),
					Access:                   network.SecurityRuleAccessAllow,
					Direction:                network.SecurityRuleDirectionOutbound,
				},
			},
			expected: false,
		},
		{
			desc: "true should be returned when everything matches but protocol is in different case",
			testRule: network.SecurityRule{
				Name: pointer.String(testRuleName4),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                 network.SecurityRuleProtocol("TCP"),
					SourcePortRange:          pointer.String("*"),
					SourceAddressPrefix:      pointer.String("Internet"),
					DestinationPortRange:     pointer.String("80"),
					DestinationAddressPrefix: pointer.String(testIP1),
					Access:                   network.SecurityRuleAccessAllow,
					Direction:                network.SecurityRuleDirectionInbound,
				},
			},
			expected: true,
		},
		{
			desc: "true should be returned when everything matches but DestinationAddressPrefixes is nil",
			testRule: network.SecurityRule{
				Name: pointer.String(testRuleName4),
				SecurityRulePropertiesFormat: &network.SecurityRulePropertiesFormat{
					Protocol:                   network.SecurityRuleProtocolTCP,
					SourcePortRange:            pointer.String("*"),
					SourceAddressPrefix:        pointer.String("Internet"),
					DestinationPortRange:       pointer.String("80"),
					DestinationAddressPrefix:   pointer.String(testIP1),
					DestinationAddressPrefixes: nil,
					Access:                     network.SecurityRuleAccessAllow,
					Direction:                  network.SecurityRuleDirectionInbound,
				},
			},
			expected: true,
		},
		{
			desc:     "true should be returned when everything matches",
			testRule: sg,
			expected: true,
		},
	}

	for i := range testCases {
		found := findSecurityRule([]network.SecurityRule{sg}, testCases[i].testRule)
		assert.Equal(t, testCases[i].expected, found, testCases[i].desc)
	}
}
