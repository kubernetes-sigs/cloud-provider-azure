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
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/interfaceclient/mockinterfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/publicipclient/mockpublicipclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssvmclient/mockvmssvmclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/virtualmachine"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

const (
	fakePrivateIP          = "10.240.0.10"
	fakePublicIP           = "10.10.10.10"
	testVMSSName           = "vmss"
	testVMPowerState       = "PowerState/Running"
	testLBBackendpoolID0   = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0"
	testLBBackendpoolID0v6 = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-0" + "-" + consts.IPVersionIPv6String
	testLBBackendpoolID1   = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-1"
	testLBBackendpoolID1v6 = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-1" + "-" + consts.IPVersionIPv6String
	testLBBackendpoolID2   = "/subscriptions/sub/resourceGroups/rg1/providers/Microsoft.Network/loadBalancers/lb/backendAddressPools/backendpool-2"
	errMsgSuffix           = ", but an error occurs"
)

// helper enum for setting the OS variant
// of the VMSS image ref.
type osVersion int

const (
	unspecified osVersion = iota
	windows2019
	windows2022
	ubuntu
)

func buildTestOSSpecificVMSSWithLB(name, namePrefix string, lbBackendpoolIDs []string, os osVersion, ipv6 bool) compute.VirtualMachineScaleSet {
	vmss := buildTestVMSSWithLB(name, namePrefix, lbBackendpoolIDs, ipv6)
	switch os {
	case windows2019:
		vmss.VirtualMachineScaleSetProperties.VirtualMachineProfile.StorageProfile = &compute.VirtualMachineScaleSetStorageProfile{
			OsDisk: &compute.VirtualMachineScaleSetOSDisk{
				OsType: compute.OperatingSystemTypesWindows,
			},
			ImageReference: &compute.ImageReference{
				ID: ptr.To("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/AKS-Windows/providers/Microsoft.Compute/galleries/AKSWindows/images/windows-2019-containerd/versions/17763.5820.240516"),
			},
		}
	case windows2022:
		vmss.VirtualMachineScaleSetProperties.VirtualMachineProfile.StorageProfile = &compute.VirtualMachineScaleSetStorageProfile{
			OsDisk: &compute.VirtualMachineScaleSetOSDisk{
				OsType: compute.OperatingSystemTypesWindows,
			},
			ImageReference: &compute.ImageReference{
				ID: ptr.To("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/AKS-Windows/providers/Microsoft.Compute/galleries/AKSWindows/images/windows-2022-containerd/versions/20348.5820.240516"),
			},
		}
	}
	return vmss
}

func buildTestVMSSWithLB(name, namePrefix string, lbBackendpoolIDs []string, ipv6 bool) compute.VirtualMachineScaleSet {
	lbBackendpoolsV4, lbBackendpoolsV6 := make([]compute.SubResource, 0), make([]compute.SubResource, 0)
	for _, id := range lbBackendpoolIDs {
		lbBackendpoolsV4 = append(lbBackendpoolsV4, compute.SubResource{ID: ptr.To(id)})
		lbBackendpoolsV6 = append(lbBackendpoolsV6, compute.SubResource{ID: ptr.To(id + "-" + consts.IPVersionIPv6String)})
	}
	ipConfig := []compute.VirtualMachineScaleSetIPConfiguration{
		{
			VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
				LoadBalancerBackendAddressPools: &lbBackendpoolsV4,
			},
		},
	}
	if ipv6 {
		ipConfig = append(ipConfig, compute.VirtualMachineScaleSetIPConfiguration{
			VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
				LoadBalancerBackendAddressPools: &lbBackendpoolsV6,
				PrivateIPAddressVersion:         compute.IPv6,
			},
		})
	}

	expectedVMSS := compute.VirtualMachineScaleSet{
		Name: &name,
		VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
			OrchestrationMode: compute.Uniform,
			ProvisioningState: ptr.To("Running"),
			VirtualMachineProfile: &compute.VirtualMachineScaleSetVMProfile{
				OsProfile: &compute.VirtualMachineScaleSetOSProfile{
					ComputerNamePrefix: &namePrefix,
				},
				NetworkProfile: &compute.VirtualMachineScaleSetNetworkProfile{
					NetworkInterfaceConfigurations: &[]compute.VirtualMachineScaleSetNetworkConfiguration{
						{
							VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
								Primary:          ptr.To(true),
								IPConfigurations: &ipConfig,
							},
						},
					},
				},
			},
		},
	}

	return expectedVMSS
}

func buildTestVMSS(name, computerNamePrefix string) compute.VirtualMachineScaleSet {
	return compute.VirtualMachineScaleSet{
		Name: &name,
		VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
			OrchestrationMode: compute.Uniform,
			VirtualMachineProfile: &compute.VirtualMachineScaleSetVMProfile{
				OsProfile: &compute.VirtualMachineScaleSetOSProfile{
					ComputerNamePrefix: &computerNamePrefix,
				},
			},
		},
	}
}

func buildTestVirtualMachineEnv(ss *Cloud, scaleSetName, zone string, faultDomain int32, vmList []string, state string, isIPv6 bool) ([]compute.VirtualMachineScaleSetVM, network.Interface, network.PublicIPAddress) {
	expectedVMSSVMs := make([]compute.VirtualMachineScaleSetVM, 0)
	expectedInterface := network.Interface{}
	expectedPIP := network.PublicIPAddress{}

	for i := range vmList {
		nodeName := vmList[i]
		ID := fmt.Sprintf("/subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/%s/virtualMachines/%d", scaleSetName, i)
		interfaceID := fmt.Sprintf("/subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/%s/virtualMachines/%d/networkInterfaces/%s", scaleSetName, i, nodeName)
		instanceID := fmt.Sprintf("%d", i)
		vmName := fmt.Sprintf("%s_%s", scaleSetName, instanceID)
		publicAddressID := fmt.Sprintf("/subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/%s/virtualMachines/%d/networkInterfaces/%s/ipConfigurations/ipconfig1/publicIPAddresses/%s", scaleSetName, i, nodeName, nodeName)

		// set vmss virtual machine.
		networkInterfaces := []compute.NetworkInterfaceReference{
			{
				ID: &interfaceID,
				NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
					Primary: ptr.To(true),
				},
			},
		}
		ipConfigurations := []compute.VirtualMachineScaleSetIPConfiguration{
			{
				Name: ptr.To("ipconfig1"),
				VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
					Primary:                         ptr.To(true),
					LoadBalancerBackendAddressPools: &[]compute.SubResource{{ID: ptr.To(testLBBackendpoolID0)}},
					PrivateIPAddressVersion:         compute.IPv4,
				},
			},
		}
		if isIPv6 {
			ipConfigurations = append(ipConfigurations, compute.VirtualMachineScaleSetIPConfiguration{
				Name: ptr.To("ipconfigv6"),
				VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
					Primary:                         ptr.To(false),
					LoadBalancerBackendAddressPools: &[]compute.SubResource{{ID: ptr.To(testLBBackendpoolID0v6)}},
					PrivateIPAddressVersion:         compute.IPv6,
				},
			})
		}
		networkConfigurations := []compute.VirtualMachineScaleSetNetworkConfiguration{
			{
				Name: ptr.To("vmss-nic"),
				ID:   ptr.To("fakeNetworkConfiguration"),
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &ipConfigurations,
					Primary:          ptr.To(true),
				},
			},
		}

		vmssVM := compute.VirtualMachineScaleSetVM{
			VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
				ProvisioningState: ptr.To(state),
				OsProfile: &compute.OSProfile{
					ComputerName: &nodeName,
				},
				NetworkProfile: &compute.NetworkProfile{
					NetworkInterfaces: &networkInterfaces,
				},
				NetworkProfileConfiguration: &compute.VirtualMachineScaleSetVMNetworkProfileConfiguration{
					NetworkInterfaceConfigurations: &networkConfigurations,
				},
				InstanceView: &compute.VirtualMachineScaleSetVMInstanceView{
					PlatformFaultDomain: &faultDomain,
					Statuses: &[]compute.InstanceViewStatus{
						{Code: ptr.To(testVMPowerState)},
					},
				},
			},
			ID:         &ID,
			InstanceID: &instanceID,
			Name:       &vmName,
			Location:   &ss.Location,
			Sku:        &compute.Sku{Name: ptr.To("sku")},
		}
		if zone != "" {
			zones := []string{zone}
			vmssVM.Zones = &zones
		}

		// set interfaces.
		expectedInterface = network.Interface{
			Name: ptr.To("nic"),
			ID:   &interfaceID,
			InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
				IPConfigurations: &[]network.InterfaceIPConfiguration{
					{
						InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
							Primary:          ptr.To(true),
							PrivateIPAddress: ptr.To(fakePrivateIP),
							PublicIPAddress: &network.PublicIPAddress{
								ID: ptr.To(publicAddressID),
							},
						},
					},
				},
			},
		}

		// set public IPs.
		expectedPIP = network.PublicIPAddress{
			ID: ptr.To(publicAddressID),
			PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
				IPAddress: ptr.To(fakePublicIP),
			},
		}

		expectedVMSSVMs = append(expectedVMSSVMs, vmssVM)
	}

	return expectedVMSSVMs, expectedInterface, expectedPIP
}

func TestGetScaleSetVMInstanceID(t *testing.T) {
	tests := []struct {
		msg                string
		machineName        string
		expectError        bool
		expectedInstanceID string
	}{{
		msg:         "invalid vmss instance name",
		machineName: "vmvm",
		expectError: true,
	},
		{
			msg:                "valid vmss instance name",
			machineName:        "vm00000Z",
			expectError:        false,
			expectedInstanceID: "35",
		},
	}

	for i, test := range tests {
		instanceID, err := getScaleSetVMInstanceID(test.machineName)
		if test.expectError {
			assert.Error(t, err, fmt.Sprintf("TestCase[%d]: %s", i, test.msg))
		} else {
			assert.Equal(t, test.expectedInstanceID, instanceID, fmt.Sprintf("TestCase[%d]: %s", i, test.msg))
		}
	}
}

func TestGetNodeIdentityByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description  string
		vmList       []string
		nodeName     string
		expected     *nodeIdentity
		scaleSet     string
		computerName string
		expectError  bool
	}{
		{
			description:  "ScaleSet should get node identity by node name",
			vmList:       []string{"vmssee6c2000000", "vmssee6c2000001"},
			nodeName:     "vmssee6c2000001",
			scaleSet:     "vmssee6c2",
			computerName: "vmssee6c2",
			expected:     &nodeIdentity{"rg", "vmssee6c2", "vmssee6c2000001"},
		},
		{
			description:  "ScaleSet should get node identity when computerNamePrefix differs from vmss name",
			vmList:       []string{"vmssee6c2000000", "vmssee6c2000001"},
			nodeName:     "vmssee6c2000001",
			scaleSet:     "ss",
			computerName: "vmssee6c2",
			expected:     &nodeIdentity{"rg", "ss", "vmssee6c2000001"},
		},
		{
			description:  "ScaleSet should get node identity by node name with upper cases hostname",
			vmList:       []string{"VMSSEE6C2000000", "VMSSEE6C2000001"},
			nodeName:     "vmssee6c2000001",
			scaleSet:     "ss",
			computerName: "vmssee6c2",
			expected:     &nodeIdentity{"rg", "ss", "vmssee6c2000001"},
		},
		{
			description:  "ScaleSet should not get node identity for non-existing nodes",
			vmList:       []string{"vmssee6c2000000", "vmssee6c2000001"},
			nodeName:     "agente6c2000005",
			scaleSet:     "ss",
			computerName: "vmssee6c2",
			expectError:  true,
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.description)

		mockVMSSClient := mockvmssclient.NewMockInterface(ctrl)
		mockVMSSVMClient := mockvmssvmclient.NewMockInterface(ctrl)
		ss.VirtualMachineScaleSetsClient = mockVMSSClient
		ss.VirtualMachineScaleSetVMsClient = mockVMSSVMClient

		expectedScaleSet := buildTestVMSS(test.scaleSet, test.computerName)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expectedScaleSet}, nil).AnyTimes()

		expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, test.scaleSet, "", 0, test.vmList, "", false)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()

		mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		nodeID, err := ss.getNodeIdentityByNodeName(context.TODO(), test.nodeName, azcache.CacheReadTypeDefault)
		if test.expectError {
			assert.Error(t, err, test.description)
			continue
		}

		assert.NoError(t, err, test.description)
		assert.Equal(t, test.expected, nodeID, test.description)
	}
}

func TestGetInstanceIDByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description string
		scaleSet    string
		vmList      []string
		nodeName    string
		expected    string
		expectError bool
	}{
		{
			description: "ScaleSet should get instance by node name",
			scaleSet:    "ss",
			vmList:      []string{"vmssee6c2000000", "vmssee6c2000001"},
			nodeName:    "vmssee6c2000001",
			expected:    "/subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/ss/virtualMachines/1",
		},
		{
			description: "ScaleSet should get instance by node name with upper cases hostname",
			scaleSet:    "ss",
			vmList:      []string{"VMSSEE6C2000000", "VMSSEE6C2000001"},
			nodeName:    "vmssee6c2000000",
			expected:    "/subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/ss/virtualMachines/0",
		},
		{
			description: "ScaleSet should not get instance for non-exist nodes",
			scaleSet:    "ss",
			vmList:      []string{"vmssee6c2000000", "vmssee6c2000001"},
			nodeName:    "agente6c2000005",
			expectError: true,
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.description)

		mockVMSSClient := mockvmssclient.NewMockInterface(ctrl)
		mockVMSSVMClient := mockvmssvmclient.NewMockInterface(ctrl)
		ss.VirtualMachineScaleSetsClient = mockVMSSClient
		ss.VirtualMachineScaleSetVMsClient = mockVMSSVMClient

		expectedScaleSet := buildTestVMSS(test.scaleSet, "vmssee6c2")
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expectedScaleSet}, nil).AnyTimes()

		expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, test.scaleSet, "", 0, test.vmList, "", false)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()

		mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		realValue, err := ss.GetInstanceIDByNodeName(context.Background(), test.nodeName)
		if test.expectError {
			assert.Error(t, err, test.description)
			continue
		}

		assert.NoError(t, err, test.description)
		assert.Equal(t, test.expected, realValue, test.description)
	}
}

func TestGetZoneByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description string
		scaleSet    string
		nodeName    string
		location    string
		zone        string
		expected    string
		vmList      []string
		faultDomain int32
		expectError bool
	}{
		{
			description: "ScaleSet should get faultDomain for non-zoned nodes",
			scaleSet:    "ss",
			vmList:      []string{"vmssee6c2000000", "vmssee6c2000001"},
			nodeName:    "vmssee6c2000000",
			faultDomain: 3,
			expected:    "3",
		},
		{
			description: "ScaleSet should get availability zone for zoned nodes",
			scaleSet:    "ss",
			vmList:      []string{"vmssee6c2000000", "vmssee6c2000001"},
			nodeName:    "vmssee6c2000000",
			zone:        "2",
			faultDomain: 3,
			expected:    "westus-2",
		},
		{
			description: "ScaleSet should get availability zone in lower cases",
			scaleSet:    "ss",
			vmList:      []string{"vmssee6c2000000", "vmssee6c2000001"},
			nodeName:    "vmssee6c2000000",
			location:    "WestUS",
			zone:        "2",
			faultDomain: 3,
			expected:    "westus-2",
		},
		{
			description: "ScaleSet should return error for non-exist nodes",
			scaleSet:    "ss",
			faultDomain: 3,
			vmList:      []string{"vmssee6c2000000", "vmssee6c2000001"},
			nodeName:    "agente6c2000005",
			expectError: true,
		},
	}

	for _, test := range testCases {
		cloud := GetTestCloud(ctrl)
		if test.location != "" {
			cloud.Location = test.location
		}
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.description)

		mockVMSSClient := mockvmssclient.NewMockInterface(ctrl)
		mockVMSSVMClient := mockvmssvmclient.NewMockInterface(ctrl)
		ss.VirtualMachineScaleSetsClient = mockVMSSClient
		ss.VirtualMachineScaleSetVMsClient = mockVMSSVMClient

		expectedScaleSet := buildTestVMSS(test.scaleSet, "vmssee6c2")
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expectedScaleSet}, nil).AnyTimes()

		expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, test.scaleSet, test.zone, test.faultDomain, test.vmList, "", false)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()

		mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		realValue, err := ss.GetZoneByNodeName(context.TODO(), test.nodeName)
		if test.expectError {
			assert.Error(t, err, test.description)
			continue
		}

		assert.NoError(t, err, test.description)
		assert.Equal(t, test.expected, realValue.FailureDomain, test.description)
		assert.Equal(t, strings.ToLower(cloud.Location), realValue.Region, test.description)
	}
}

func TestGetIPByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description string
		scaleSet    string
		vmList      []string
		nodeName    string
		expected    []string
		expectError bool
	}{
		{
			description: "GetIPByNodeName should get node's privateIP and publicIP",
			scaleSet:    "ss",
			vmList:      []string{"vmssee6c2000000", "vmssee6c2000001"},
			nodeName:    "vmssee6c2000000",
			expected:    []string{fakePrivateIP, fakePublicIP},
		},
		{
			description: "GetIPByNodeName should return error for non-exist nodes",
			scaleSet:    "ss",
			vmList:      []string{"vmssee6c2000000", "vmssee6c2000001"},
			nodeName:    "agente6c2000005",
			expectError: true,
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.description)

		mockVMSSClient := mockvmssclient.NewMockInterface(ctrl)
		mockVMSSVMClient := mockvmssvmclient.NewMockInterface(ctrl)
		mockInterfaceClient := mockinterfaceclient.NewMockInterface(ctrl)
		mockPIPClient := mockpublicipclient.NewMockInterface(ctrl)
		ss.VirtualMachineScaleSetsClient = mockVMSSClient
		ss.VirtualMachineScaleSetVMsClient = mockVMSSVMClient
		ss.InterfacesClient = mockInterfaceClient
		ss.PublicIPAddressesClient = mockPIPClient

		expectedScaleSet := buildTestVMSS(test.scaleSet, "vmssee6c2")
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expectedScaleSet}, nil).AnyTimes()

		expectedVMs, expectedInterface, expectedPIP := buildTestVirtualMachineEnv(ss.Cloud, test.scaleSet, "", 0, test.vmList, "", false)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()
		mockInterfaceClient.EXPECT().GetVirtualMachineScaleSetNetworkInterface(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedInterface, nil).AnyTimes()
		mockPIPClient.EXPECT().GetVirtualMachineScaleSetPublicIPAddress(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedPIP, nil).AnyTimes()

		mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		privateIP, publicIP, err := ss.GetIPByNodeName(context.Background(), test.nodeName)
		if test.expectError {
			assert.Error(t, err, test.description)
			continue
		}

		assert.NoError(t, err, test.description)
		assert.Equal(t, test.expected, []string{privateIP, publicIP}, test.description)
	}
}

func TestGetNodeNameByIPConfigurationID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ipConfigurationIDTemplate := "/subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/%s/virtualMachines/%s/networkInterfaces/%s/ipConfigurations/ipconfig1"

	testCases := []struct {
		description          string
		scaleSet             string
		vmList               []string
		ipConfigurationID    string
		expectedNodeName     string
		expectedScaleSetName string
		expectError          bool
	}{
		{
			description:          "GetNodeNameByIPConfigurationID should get node's Name when the node is existing",
			scaleSet:             "scaleset1",
			ipConfigurationID:    fmt.Sprintf(ipConfigurationIDTemplate, "scaleset1", "0", "scaleset1"),
			vmList:               []string{"vmssee6c2000000", "vmssee6c2000001"},
			expectedNodeName:     "vmssee6c2000000",
			expectedScaleSetName: "scaleset1",
		},
		{
			description:       "GetNodeNameByIPConfigurationID should return error for non-exist nodes",
			scaleSet:          "scaleset2",
			ipConfigurationID: fmt.Sprintf(ipConfigurationIDTemplate, "scaleset2", "3", "scaleset1"),
			vmList:            []string{"vmssee6c2000002", "vmssee6c2000003"},
			expectError:       true,
		},
		{
			description:       "GetNodeNameByIPConfigurationID should return error for wrong ipConfigurationID",
			scaleSet:          "scaleset3",
			ipConfigurationID: "invalid-configuration-id",
			vmList:            []string{"vmssee6c2000004", "vmssee6c2000005"},
			expectError:       true,
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.description)

		mockVMSSClient := mockvmssclient.NewMockInterface(ctrl)
		mockVMSSVMClient := mockvmssvmclient.NewMockInterface(ctrl)
		ss.VirtualMachineScaleSetsClient = mockVMSSClient
		ss.VirtualMachineScaleSetVMsClient = mockVMSSVMClient

		expectedScaleSet := buildTestVMSS(test.scaleSet, "vmssee6c2")
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expectedScaleSet}, nil).AnyTimes()

		expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, test.scaleSet, "", 0, test.vmList, "", false)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, nil).AnyTimes()

		mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		nodeName, scalesetName, err := ss.GetNodeNameByIPConfigurationID(context.TODO(), test.ipConfigurationID)
		if test.expectError {
			assert.Error(t, err, test.description)
			continue
		}

		assert.NoError(t, err, test.description)
		assert.Equal(t, test.expectedNodeName, nodeName, test.description)
		assert.Equal(t, test.expectedScaleSetName, scalesetName, test.description)
	}
}

func TestExtractResourceGroupByVMSSNicID(t *testing.T) {
	vmssNicIDTemplate := "/subscriptions/script/resourceGroups/%s/providers/Microsoft.Compute/virtualMachineScaleSets/%s/virtualMachines/%s/networkInterfaces/nic-0"

	testCases := []struct {
		description string
		vmssNicID   string
		expected    string
		expectError bool
	}{
		{
			description: "extractResourceGroupByVMSSNicID should get resource group name for vmss nic ID",
			vmssNicID:   fmt.Sprintf(vmssNicIDTemplate, "rg1", "vmss1", "0"),
			expected:    "rg1",
		},
		{
			description: "extractResourceGroupByVMSSNicID should return error for VM nic ID",
			vmssNicID:   "/subscriptions/script/resourceGroups/rg2/providers/Microsoft.Network/networkInterfaces/nic-0",
			expectError: true,
		},
		{
			description: "extractResourceGroupByVMSSNicID should return error for wrong vmss nic ID",
			vmssNicID:   "wrong-nic-id",
			expectError: true,
		},
	}

	for _, test := range testCases {
		resourceGroup, err := extractResourceGroupByVMSSNicID(test.vmssNicID)
		if test.expectError {
			assert.Error(t, err, test.description)
			continue
		}

		assert.NoError(t, err, test.description)
		assert.Equal(t, test.expected, resourceGroup, test.description)
	}
}

func TestGetVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description     string
		existedVMSSName string
		vmssName        string
		vmssListError   *retry.Error
		expectedErr     error
	}{
		{
			description:     "getVMSS should return the correct VMSS",
			existedVMSSName: "vmss-1",
			vmssName:        "vmss-1",
		},
		{
			description:     "getVMSS should return cloudprovider.InstanceNotFound if there's no matching VMSS",
			existedVMSSName: "vmss-1",
			vmssName:        "vmss-2",
			expectedErr:     cloudprovider.InstanceNotFound,
		},
		{
			description:     "getVMSS should report an error if there's something wrong during an api call",
			existedVMSSName: "vmss-1",
			vmssName:        "vmss-1",
			vmssListError:   &retry.Error{RawError: fmt.Errorf("error during vmss list")},
			expectedErr:     fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error during vmss list"),
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.description)

		mockVMSSClient := mockvmssclient.NewMockInterface(ctrl)
		ss.VirtualMachineScaleSetsClient = mockVMSSClient

		expected := compute.VirtualMachineScaleSet{
			Name: ptr.To(test.existedVMSSName),
			VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
				VirtualMachineProfile: &compute.VirtualMachineScaleSetVMProfile{},
			},
		}
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expected}, test.vmssListError).AnyTimes()

		actual, err := ss.getVMSS(context.TODO(), test.vmssName, azcache.CacheReadTypeDefault)
		if test.expectedErr != nil {
			assert.EqualError(t, test.expectedErr, err.Error(), test.description)
		}
		if actual != nil {
			assert.Equal(t, expected, *actual, test.description)
		}
	}
}

func TestGetVmssVM(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description      string
		nodeName         string
		existedNodeNames []string
		existedVMSSName  string
		expectedError    error
	}{
		{
			description:      "getVmssVM should return the correct name of vmss, the instance id of the node, and the corresponding vmss instance",
			nodeName:         "vmss-vm-000000",
			existedNodeNames: []string{"vmss-vm-000000"},
			existedVMSSName:  testVMSSName,
		},
		{
			description:      "getVmssVM should report an error of instance not found if there's no matches",
			nodeName:         "vmss-vm-000001",
			existedNodeNames: []string{"vmss-vm-000000"},
			existedVMSSName:  testVMSSName,
			expectedError:    cloudprovider.InstanceNotFound,
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.description)

		expectedVMSS := buildTestVMSS(test.existedVMSSName, "vmss-vm-")
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, test.existedVMSSName, "", 0, test.existedNodeNames, "", false)
		var expectedVMSSVM compute.VirtualMachineScaleSetVM
		for _, expected := range expectedVMSSVMs {
			if strings.EqualFold(*expected.OsProfile.ComputerName, test.nodeName) {
				expectedVMSSVM = expected
			}
		}

		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, test.existedVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

		vmssVM, err := ss.getVmssVM(context.TODO(), test.nodeName, azcache.CacheReadTypeDefault)
		if vmssVM != nil {
			assert.Equal(t, expectedVMSSVM, *vmssVM.AsVirtualMachineScaleSetVM(), test.description)
		}
		assert.Equal(t, test.expectedError, err, test.description)
	}
}

func TestGetPowerStatusByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description        string
		vmList             []string
		nilStatus          bool
		expectedPowerState string
		expectedErr        error
	}{
		{
			description:        "GetPowerStatusByNodeName should return the correct power state",
			vmList:             []string{"vmss-vm-000001"},
			expectedPowerState: "Running",
		},
		{
			description:        "GetPowerStatusByNodeName should return vmPowerStateUnknown when the vm.InstanceView.Statuses is nil",
			vmList:             []string{"vmss-vm-000001"},
			nilStatus:          true,
			expectedPowerState: consts.VMPowerStateUnknown,
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test VMSS")

		expectedVMSS := buildTestVMSS(testVMSSName, "vmss-vm-")
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, test.vmList, "", false)
		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		if test.nilStatus {
			expectedVMSSVMs[0].InstanceView.Statuses = nil
		}
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

		mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		powerState, err := ss.GetPowerStatusByNodeName(context.TODO(), "vmss-vm-000001")
		assert.Equal(t, test.expectedErr, err, test.description+errMsgSuffix)
		assert.Equal(t, test.expectedPowerState, powerState, test.description)
	}
}

func TestGetProvisioningStateByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description               string
		vmList                    []string
		provisioningState         string
		expectedProvisioningState string
		expectedErr               error
	}{
		{
			description:               "GetProvisioningStateByNodeName should return empty value when the vm.ProvisioningState is nil",
			provisioningState:         "",
			vmList:                    []string{"vmss-vm-000001"},
			expectedProvisioningState: "",
		},
		{
			description:               "GetProvisioningStateByNodeName should return Succeeded when the vm is running",
			provisioningState:         "Succeeded",
			vmList:                    []string{"vmss-vm-000001"},
			expectedProvisioningState: "Succeeded",
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test VMSS")

		expectedVMSS := buildTestVMSS(testVMSSName, "vmss-vm-")
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, test.vmList, "", false)
		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		if test.provisioningState != "" {
			expectedVMSSVMs[0].ProvisioningState = ptr.To(test.provisioningState)
		} else {
			expectedVMSSVMs[0].ProvisioningState = nil
		}
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

		mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		provisioningState, err := ss.GetProvisioningStateByNodeName(context.TODO(), "vmss-vm-000001")
		assert.Equal(t, test.expectedErr, err, test.description+errMsgSuffix)
		assert.Equal(t, test.expectedProvisioningState, provisioningState, test.description)
	}
}

func TestGetVmssVMByInstanceID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description string
		instanceID  string
		vmList      []string
		expectedErr error
	}{
		{
			description: "GetVmssVMByInstanceID should return the correct VMSS VM",
			instanceID:  "0",
			vmList:      []string{"vmss-vm-000000"},
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test VMSS")

		expectedVMSS := compute.VirtualMachineScaleSet{
			Name: ptr.To(testVMSSName),
			VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
				VirtualMachineProfile: &compute.VirtualMachineScaleSetVMProfile{},
			},
		}
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, test.vmList, "", false)
		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

		vm, err := ss.getVmssVMByInstanceID(context.TODO(), ss.ResourceGroup, testVMSSName, test.instanceID, azcache.CacheReadTypeDefault)
		assert.Equal(t, test.expectedErr, err, test.description+errMsgSuffix)
		assert.Equal(t, expectedVMSSVMs[0], *vm, test.description)
	}
}

func TestGetVmssVMByNodeIdentity(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description       string
		instanceID        string
		vmList            []string
		goneVMList        []string
		expectedErr       error
		goneVMExpectedErr error
	}{
		{
			description: "getVmssVMByNodeIdentity should return the correct VMSS VM",
			vmList:      []string{"vmss-vm-000000"},
			goneVMList:  []string{},
		},
		{
			description:       "getVmssVMByNodeIdentity should not panic with a gone VMSS VM but cache for the VMSS VM entry still exists",
			vmList:            []string{"vmss-vm-000000"},
			goneVMList:        []string{"vmss-vm-000001"},
			goneVMExpectedErr: cloudprovider.InstanceNotFound,
		},
	}

	for _, test := range testCases {
		t.Run(test.description, func(t *testing.T) {
			ss, err := NewTestScaleSet(ctrl)
			assert.NoError(t, err, "unexpected error when creating test VMSS")

			expectedVMSS := compute.VirtualMachineScaleSet{
				Name: ptr.To(testVMSSName),
				VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
					VirtualMachineProfile: &compute.VirtualMachineScaleSetVMProfile{},
				},
			}
			mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

			expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, test.vmList, "", false)
			mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
			mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

			cacheKey := getVMSSVMCacheKey(ss.ResourceGroup, testVMSSName)
			virtualMachines, err := ss.getVMSSVMsFromCache(context.TODO(), ss.ResourceGroup, testVMSSName, azcache.CacheReadTypeDefault)
			assert.Nil(t, err)
			for _, vm := range test.goneVMList {
				entry := VMSSVirtualMachineEntry{
					ResourceGroup: ss.ResourceGroup,
					VMSSName:      testVMSSName,
				}
				virtualMachines.Store(vm, &entry)
			}
			ss.vmssVMCache.Update(cacheKey, virtualMachines)

			for i := 0; i < len(test.vmList); i++ {
				node := nodeIdentity{ss.ResourceGroup, testVMSSName, test.vmList[i]}
				vm, err := ss.getVmssVMByNodeIdentity(context.TODO(), &node, azcache.CacheReadTypeDefault)
				assert.Equal(t, test.expectedErr, err)
				assert.Equal(t, *virtualmachine.FromVirtualMachineScaleSetVM(&expectedVMSSVMs[i], virtualmachine.ByVMSS(testVMSSName)), *vm)
			}
			for i := 0; i < len(test.goneVMList); i++ {
				node := nodeIdentity{ss.ResourceGroup, testVMSSName, test.goneVMList[i]}
				_, err := ss.getVmssVMByNodeIdentity(context.TODO(), &node, azcache.CacheReadTypeDefault)
				assert.Equal(t, test.goneVMExpectedErr, err)
			}

			virtualMachines, err = ss.getVMSSVMsFromCache(context.TODO(), ss.ResourceGroup, testVMSSName, azcache.CacheReadTypeDefault)
			assert.Nil(t, err)

			for _, vm := range test.goneVMList {
				_, ok := virtualMachines.Load(vm)
				assert.True(t, ok)
			}
		})
	}
}

func TestGetInstanceTypeByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description  string
		vmList       []string
		vmClientErr  *retry.Error
		expectedType string
		expectedErr  error
	}{
		{
			description:  "GetInstanceTypeByNodeName should return the correct instance type",
			vmList:       []string{"vmss-vm-000000"},
			expectedType: "sku",
		},
		{
			description:  "GetInstanceTypeByNodeName should report the error that occurs",
			vmList:       []string{"vmss-vm-000000"},
			vmClientErr:  &retry.Error{RawError: fmt.Errorf("error")},
			expectedType: "",
			expectedErr:  fmt.Errorf("getter function of nonVmssUniformNodesCache: failed to list vms in the resource group rg: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error"),
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test VMSS")

		expectedVMSS := buildTestVMSS(testVMSSName, "vmss-vm-")
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, test.vmList, "", false)
		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

		mockVMClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, test.vmClientErr).AnyTimes()

		sku, err := ss.GetInstanceTypeByNodeName(context.Background(), "vmss-vm-000000")
		if test.expectedErr != nil {
			assert.EqualError(t, err, test.expectedErr.Error(), test.description)
		}
		assert.Equal(t, test.expectedType, sku, test.description)
	}
}

func TestGetPrimaryInterfaceID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description       string
		existedInterfaces []compute.NetworkInterfaceReference
		expectedID        string
		expectedErr       error
	}{
		{
			description: "GetPrimaryInterfaceID should return the ID of the primary NIC on the VMSS VM",
			existedInterfaces: []compute.NetworkInterfaceReference{
				{
					ID: ptr.To("1"),
					NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
						Primary: ptr.To(true),
					},
				},
				{ID: ptr.To("2")},
			},
			expectedID: "1",
		},
		{
			description: "GetPrimaryInterfaceID should report an error if there's no primary NIC on the VMSS VM",
			existedInterfaces: []compute.NetworkInterfaceReference{
				{
					ID: ptr.To("1"),
					NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
						Primary: ptr.To(false),
					},
				},
				{
					ID: ptr.To("2"),
					NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
						Primary: ptr.To(false),
					},
				},
			},
			expectedErr: fmt.Errorf("failed to find a primary nic for the vm. vmname=\"vm\""),
		},
		{
			description:       "GetPrimaryInterfaceID should report an error if there's no network interface on the VMSS VM",
			existedInterfaces: []compute.NetworkInterfaceReference{},
			expectedErr:       fmt.Errorf("failed to find the network interfaces for vm vm"),
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test VMSS")

		existedInterfaces := test.existedInterfaces
		vm := compute.VirtualMachineScaleSetVM{
			Name: ptr.To("vm"),
			VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
				NetworkProfile: &compute.NetworkProfile{
					NetworkInterfaces: &existedInterfaces,
				},
			},
		}
		if len(test.existedInterfaces) == 0 {
			vm.VirtualMachineScaleSetVMProperties.NetworkProfile = nil
		}

		id, err := ss.getPrimaryInterfaceID(virtualmachine.FromVirtualMachineScaleSetVM(&vm, virtualmachine.ByVMSS("vmss")))
		assert.Equal(t, test.expectedErr, err, test.description+errMsgSuffix)
		assert.Equal(t, test.expectedID, id, test.description)
	}
}

func TestGetPrimaryInterface(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description         string
		nodeName            string
		vmList              []string
		vmClientErr         *retry.Error
		vmssClientErr       *retry.Error
		nicClientErr        *retry.Error
		hasPrimaryInterface bool
		isInvalidNICID      bool
		expectedErr         error
	}{
		{
			description:         "GetPrimaryInterface should return the correct network interface",
			nodeName:            "vmss-vm-000000",
			vmList:              []string{"vmss-vm-000000"},
			hasPrimaryInterface: true,
		},
		{
			description:         "GetPrimaryInterface should report the error if vm client returns retry error",
			nodeName:            "vmss-vm-000000",
			vmList:              []string{"vmss-vm-000000"},
			hasPrimaryInterface: true,
			vmClientErr:         &retry.Error{RawError: fmt.Errorf("error")},
			expectedErr:         fmt.Errorf("getter function of nonVmssUniformNodesCache: failed to list vms in the resource group rg: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error"),
		},
		{
			description:         "GetPrimaryInterface should report the error if vmss client returns retry error",
			nodeName:            "vmss-vm-000000",
			vmList:              []string{"vmss-vm-000000"},
			hasPrimaryInterface: true,
			vmssClientErr:       &retry.Error{RawError: fmt.Errorf("error")},
			expectedErr:         fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error"),
		},
		{
			description:         "GetPrimaryInterface should report the error if there is no primary interface",
			nodeName:            "vmss-vm-000000",
			vmList:              []string{"vmss-vm-000000"},
			hasPrimaryInterface: false,
			expectedErr:         fmt.Errorf("failed to find a primary nic for the vm. vmname=\"vmss_0\""),
		},
		{
			description:         "GetPrimaryInterface should report the error if the id of the primary nic is not valid",
			nodeName:            "vmss-vm-000000",
			vmList:              []string{"vmss-vm-000000"},
			isInvalidNICID:      true,
			hasPrimaryInterface: true,
			expectedErr:         fmt.Errorf("resource name was missing from identifier"),
		},
		{
			description:         "GetPrimaryInterface should report the error if nic client returns retry error",
			nodeName:            "vmss-vm-000000",
			vmList:              []string{"vmss-vm-000000"},
			hasPrimaryInterface: true,
			nicClientErr:        &retry.Error{RawError: fmt.Errorf("error")},
			expectedErr:         fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error"),
		},
		{
			description:         "GetPrimaryInterface should report the error if the NIC instance is not found",
			nodeName:            "vmss-vm-000000",
			vmList:              []string{"vmss-vm-000000"},
			hasPrimaryInterface: true,
			nicClientErr:        &retry.Error{HTTPStatusCode: 404, RawError: fmt.Errorf("not found")},
			expectedErr:         cloudprovider.InstanceNotFound,
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test VMSS")

		expectedVMSS := buildTestVMSS(testVMSSName, "vmss-vm-")
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, test.vmssClientErr).AnyTimes()

		expectedVMSSVMs, expectedInterface, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, test.vmList, "", false)
		if !test.hasPrimaryInterface {
			networkInterfaces := *expectedVMSSVMs[0].NetworkProfile.NetworkInterfaces
			networkInterfaces[0].Primary = ptr.To(false)
			networkInterfaces = append(networkInterfaces, compute.NetworkInterfaceReference{
				NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{Primary: ptr.To(false)},
			})
			expectedVMSSVMs[0].NetworkProfile.NetworkInterfaces = &networkInterfaces
		}
		if test.isInvalidNICID {
			networkInterfaces := *expectedVMSSVMs[0].NetworkProfile.NetworkInterfaces
			networkInterfaces[0].ID = ptr.To("invalid/id/")
			expectedVMSSVMs[0].NetworkProfile.NetworkInterfaces = &networkInterfaces
		}
		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

		mockVMClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, test.vmClientErr).AnyTimes()

		mockInterfaceClient := ss.InterfacesClient.(*mockinterfaceclient.MockInterface)
		mockInterfaceClient.EXPECT().GetVirtualMachineScaleSetNetworkInterface(gomock.Any(), ss.ResourceGroup, testVMSSName, "0", test.nodeName, gomock.Any()).Return(expectedInterface, test.nicClientErr).AnyTimes()
		expectedInterface.Location = &ss.Location

		if test.vmClientErr != nil || test.vmssClientErr != nil || test.nicClientErr != nil || !test.hasPrimaryInterface || test.isInvalidNICID {
			expectedInterface = network.Interface{}
		}

		nic, err := ss.GetPrimaryInterface(context.Background(), test.nodeName)
		if test.expectedErr != nil {
			assert.EqualError(t, err, test.expectedErr.Error(), test.description)
		}
		assert.Equal(t, expectedInterface, nic, test.description)
	}
}

func TestGetVMSSPublicIPAddress(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description  string
		pipClientErr *retry.Error
		pipName      string
		found        bool
		expectedErr  error
	}{
		{
			description: "GetVMSSPublicIPAddress should return the correct public IP address",
			pipName:     "pip",
			found:       true,
		},
		{
			description:  "GetVMSSPublicIPAddress should report the error if the pip client returns retry.Error",
			pipName:      "pip",
			found:        false,
			pipClientErr: &retry.Error{RawError: fmt.Errorf("error")},
			expectedErr:  fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error"),
		},
		{
			description: "GetVMSSPublicIPAddress should not report errors if the pip cannot be found",
			pipName:     "pip-1",
			found:       false,
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test VMSS")

		mockPIPClient := ss.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
		mockPIPClient.EXPECT().GetVirtualMachineScaleSetPublicIPAddress(gomock.Any(), ss.ResourceGroup, testVMSSName, "0", "nic", "ip", "pip", "").Return(network.PublicIPAddress{}, test.pipClientErr).AnyTimes()
		mockPIPClient.EXPECT().GetVirtualMachineScaleSetPublicIPAddress(gomock.Any(), ss.ResourceGroup, testVMSSName, "0", "nic", "ip", gomock.Not("pip"), "").Return(network.PublicIPAddress{}, &retry.Error{HTTPStatusCode: 404, RawError: fmt.Errorf("not found")}).AnyTimes()

		_, found, err := ss.getVMSSPublicIPAddress(ss.ResourceGroup, testVMSSName, "0", "nic", "ip", test.pipName)
		if test.expectedErr != nil {
			assert.EqualError(t, test.expectedErr, err.Error(), test.description+errMsgSuffix)
		}
		assert.Equal(t, test.found, found, test.description)
	}
}

func TestGetPrivateIPsByNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description        string
		nodeName           string
		vmList             []string
		isNilIPConfigs     bool
		vmClientErr        *retry.Error
		expectedPrivateIPs []string
		expectedErr        error
	}{
		{
			description:        "GetPrivateIPsByNodeName should return the correct private IPs",
			nodeName:           "vmss-vm-000000",
			vmList:             []string{"vmss-vm-000000"},
			expectedPrivateIPs: []string{fakePrivateIP},
		},
		{
			description:        "GetPrivateIPsByNodeName should report the error if the ipconfig of the nic is nil",
			nodeName:           "vmss-vm-000000",
			vmList:             []string{"vmss-vm-000000"},
			isNilIPConfigs:     true,
			expectedPrivateIPs: []string{},
			expectedErr:        fmt.Errorf("nic.IPConfigurations for nic (nicname=\"nic\") is nil"),
		},
		{
			description:        "GetPrivateIPsByNodeName should report the error if error happens during GetPrimaryInterface",
			nodeName:           "vmss-vm-000000",
			vmList:             []string{"vmss-vm-000000"},
			vmClientErr:        &retry.Error{RawError: fmt.Errorf("error")},
			expectedPrivateIPs: []string{},
			expectedErr:        fmt.Errorf("getter function of nonVmssUniformNodesCache: failed to list vms in the resource group rg: Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error"),
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test VMSS")

		expectedVMSS := buildTestVMSS(testVMSSName, "vmss-vm-")
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

		expectedVMSSVMs, expectedInterface, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, test.vmList, "", false)

		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

		mockVMClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, test.vmClientErr).AnyTimes()

		if test.isNilIPConfigs {
			expectedInterface.IPConfigurations = nil
		}
		mockInterfaceClient := ss.InterfacesClient.(*mockinterfaceclient.MockInterface)
		mockInterfaceClient.EXPECT().GetVirtualMachineScaleSetNetworkInterface(gomock.Any(), ss.ResourceGroup, testVMSSName, "0", test.nodeName, gomock.Any()).Return(expectedInterface, nil).AnyTimes()

		privateIPs, err := ss.GetPrivateIPsByNodeName(context.Background(), test.nodeName)
		if test.expectedErr != nil {
			assert.EqualError(t, err, test.expectedErr.Error(), test.description)
		}
		assert.Equal(t, test.expectedPrivateIPs, privateIPs, test.description)
	}
}

func TestExtractScaleSetNameByProviderID(t *testing.T) {
	providerID := "/subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/vmss-vm-000000"
	vmssName, err := extractScaleSetNameByProviderID(providerID)
	assert.Nil(t, err, fmt.Errorf("unexpected error %w happened", err))
	assert.Equal(t, "vmss", vmssName, "extractScaleSetNameByProviderID should return the correct vmss name")

	providerID = "/invalid/id"
	vmssName, err = extractScaleSetNameByProviderID(providerID)
	assert.Equal(t, ErrorNotVmssInstance, err, "extractScaleSetNameByProviderID should return the error of ErrorNotVmssInstance if the providerID is not a valid vmss ID")
	assert.Equal(t, "", vmssName, "extractScaleSetNameByProviderID should return an empty string")
}

func TestExtractResourceGroupByProviderID(t *testing.T) {
	providerID := "/subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/vmss-vm-000000"
	vmssName, err := extractResourceGroupByProviderID(providerID)
	assert.Nil(t, err, fmt.Errorf("unexpected error %w happened", err))
	assert.Equal(t, "rg", vmssName, "extractScaleSetNameByProviderID should return the correct vmss name")

	providerID = "/invalid/id"
	vmssName, err = extractResourceGroupByProviderID(providerID)
	assert.Equal(t, ErrorNotVmssInstance, err, "extractScaleSetNameByProviderID should return the error of ErrorNotVmssInstance if the providerID is not a valid vmss ID")
	assert.Equal(t, "", vmssName, "extractScaleSetNameByProviderID should return an empty string")
}

func TestListScaleSetVMs(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description     string
		existedVMSSVMs  []compute.VirtualMachineScaleSetVM
		vmssVMClientErr *retry.Error
		expectedErr     error
	}{
		{
			description: "listScaleSetVMs should return the correct vmss vms",
			existedVMSSVMs: []compute.VirtualMachineScaleSetVM{
				{Name: ptr.To("vmss-vm-000000")},
				{Name: ptr.To("vmss-vm-000001")},
			},
		},
		{
			description:     "listScaleSetVMs should report the error that the vmss vm client hits",
			vmssVMClientErr: &retry.Error{RawError: fmt.Errorf("error")},
			expectedErr:     fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error"),
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test VMSS")

		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(test.existedVMSSVMs, test.vmssVMClientErr).AnyTimes()

		expectedVMSSVMs := test.existedVMSSVMs

		vmssVMs, err := ss.listScaleSetVMs(testVMSSName, ss.ResourceGroup)
		if test.expectedErr != nil {
			assert.EqualError(t, test.expectedErr, err.Error(), test.description+errMsgSuffix)
		}
		assert.Equal(t, expectedVMSSVMs, vmssVMs, test.description)
	}
}

func TestGetAgentPoolScaleSets(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description       string
		excludeLBNodes    []string
		nodes             []*v1.Node
		expectedVMSSNames *[]string
		expectedErr       error
	}{
		{
			description:    "getAgentPoolScaleSets should return the correct vmss names",
			excludeLBNodes: []string{"vmss-vm-000000", "vmss-vm-000001"},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "vmss-vm-000000",
						Labels: map[string]string{consts.NodeLabelRole: "master"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "vmss-vm-000001",
						Labels: map[string]string{consts.ManagedByAzureLabel: "false"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmss-vm-000002",
					},
				},
			},
			expectedVMSSNames: &[]string{"vmss"},
		},
		{
			description:    "getAgentPoolScaleSets should return the correct vmss names",
			excludeLBNodes: []string{"vmss-vm-000001"},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "vmss-vm-000001",
						Labels: map[string]string{v1.LabelNodeExcludeBalancers: "true"},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmss-vm-000002",
					},
				},
			},
			expectedVMSSNames: &[]string{"vmss"},
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test VMSS")
		ss.excludeLoadBalancerNodes = utilsets.NewString(test.excludeLBNodes...)

		expectedVMSS := buildTestVMSS(testVMSSName, "vmss-vm-")
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

		expectedVMSSVMs := []compute.VirtualMachineScaleSetVM{
			{
				VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
					OsProfile: &compute.OSProfile{ComputerName: ptr.To("vmss-vm-000000")},
					NetworkProfile: &compute.NetworkProfile{
						NetworkInterfaces: &[]compute.NetworkInterfaceReference{},
					},
				},
			},
			{
				VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
					OsProfile: &compute.OSProfile{ComputerName: ptr.To("vmss-vm-000001")},
					NetworkProfile: &compute.NetworkProfile{
						NetworkInterfaces: &[]compute.NetworkInterfaceReference{},
					},
				},
			},
			{
				VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
					OsProfile: &compute.OSProfile{ComputerName: ptr.To("vmss-vm-000002")},
					NetworkProfile: &compute.NetworkProfile{
						NetworkInterfaces: &[]compute.NetworkInterfaceReference{},
					},
				},
			},
		}
		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

		mockVMClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

		vmssNames, err := ss.getAgentPoolScaleSets(context.TODO(), test.nodes)
		assert.Equal(t, test.expectedErr, err, test.description+errMsgSuffix)
		assert.Equal(t, test.expectedVMSSNames, vmssNames)
	}
}

func TestGetVMSetNames(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description        string
		service            *v1.Service
		nodes              []*v1.Node
		useSingleSLB       bool
		expectedVMSetNames *[]string
		expectedErr        error
	}{
		{
			description:        "GetVMSetNames should return the primary vm set name if the service has no mode annotation",
			service:            &v1.Service{},
			expectedVMSetNames: &[]string{"vmss"},
		},
		{
			description: "GetVMSetNames should return the primary vm set name when using the single SLB",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: consts.ServiceAnnotationLoadBalancerAutoModeValue}},
			},
			useSingleSLB:       true,
			expectedVMSetNames: &[]string{"vmss"},
		},
		{
			description: "GetVMSetNames should return all scale sets if the service has auto mode annotation",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: consts.ServiceAnnotationLoadBalancerAutoModeValue}},
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmss-vm-000002",
					},
				},
			},
			expectedVMSetNames: &[]string{"vmss"},
		},
		{
			description: "GetVMSetNames should report the error if there's no such vmss",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: "vmss-1"}},
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmss-vm-000002",
					},
				},
			},
			expectedErr: ErrScaleSetNotFound,
		},
		{
			description: "GetVMSetNames should report an error if vm's network profile is nil",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: "vmss"}},
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmss-vm-000003",
					},
				},
			},
			expectedErr: cloudprovider.InstanceNotFound,
		},
		{
			description: "GetVMSetNames should return the correct vmss names",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerMode: "vmss"}},
			},
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmss-vm-000002",
					},
				},
			},
			expectedVMSetNames: &[]string{"vmss"},
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, "unexpected error when creating test VMSS")

		if test.useSingleSLB {
			ss.LoadBalancerSku = consts.LoadBalancerSkuStandard
		}

		expectedVMSS := buildTestVMSS(testVMSSName, "vmss-vm-")
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

		expectedVMSSVMs := []compute.VirtualMachineScaleSetVM{
			{
				VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
					OsProfile: &compute.OSProfile{ComputerName: ptr.To("vmss-vm-000000")},
					NetworkProfile: &compute.NetworkProfile{
						NetworkInterfaces: &[]compute.NetworkInterfaceReference{},
					},
				},
			},
			{
				VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
					OsProfile: &compute.OSProfile{ComputerName: ptr.To("vmss-vm-000001")},
					NetworkProfile: &compute.NetworkProfile{
						NetworkInterfaces: &[]compute.NetworkInterfaceReference{},
					},
				},
			},
			{
				VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
					OsProfile: &compute.OSProfile{ComputerName: ptr.To("vmss-vm-000002")},
					NetworkProfile: &compute.NetworkProfile{
						NetworkInterfaces: &[]compute.NetworkInterfaceReference{},
					},
				},
			},
			{
				VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
					OsProfile: &compute.OSProfile{ComputerName: ptr.To("vmss-vm-000003")},
				},
			},
		}
		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

		mockVMClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

		vmSetNames, err := ss.GetVMSetNames(context.TODO(), test.service, test.nodes)
		if test.expectedErr != nil {
			assert.True(t, errors.Is(err, test.expectedErr), "expected error %v, got %v", test.expectedErr, err)
		}
		assert.Equal(t, test.expectedVMSetNames, vmSetNames, test.description)
	}
}

func TestGetPrimaryNetworkInterfaceConfiguration(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	networkConfigs := []compute.VirtualMachineScaleSetNetworkConfiguration{
		{Name: ptr.To("config-0")},
	}
	config, err := getPrimaryNetworkInterfaceConfiguration(networkConfigs, testVMSSName)
	assert.Nil(t, err, "getPrimaryNetworkInterfaceConfiguration should return the correct network config")
	assert.Equal(t, &networkConfigs[0], config, "getPrimaryNetworkInterfaceConfiguration should return the correct network config")

	networkConfigs = []compute.VirtualMachineScaleSetNetworkConfiguration{
		{
			Name: ptr.To("config-0"),
			VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
				Primary: ptr.To(false),
			},
		},
		{
			Name: ptr.To("config-1"),
			VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
				Primary: ptr.To(true),
			},
		},
	}
	config, err = getPrimaryNetworkInterfaceConfiguration(networkConfigs, testVMSSName)
	assert.Nil(t, err, "getPrimaryNetworkInterfaceConfiguration should return the correct network config")
	assert.Equal(t, &networkConfigs[1], config, "getPrimaryNetworkInterfaceConfiguration should return the correct network config")

	networkConfigs = []compute.VirtualMachineScaleSetNetworkConfiguration{
		{
			Name: ptr.To("config-0"),
			VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
				Primary: ptr.To(false),
			},
		},
		{
			Name: ptr.To("config-1"),
			VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
				Primary: ptr.To(false),
			},
		},
	}
	config, err = getPrimaryNetworkInterfaceConfiguration(networkConfigs, testVMSSName)
	assert.Equal(t, fmt.Errorf("failed to find a primary network configuration for the VMSS VM or VMSS \"vmss\""), err, "getPrimaryNetworkInterfaceConfiguration should report an error if there is no primary nic")
	assert.Nil(t, config, "getPrimaryNetworkInterfaceConfiguration should report an error if there is no primary nic")
}

func TestGetPrimaryIPConfigFromVMSSNetworkConfig(t *testing.T) {
	testcases := []struct {
		desc             string
		netConfig        *compute.VirtualMachineScaleSetNetworkConfiguration
		backendPoolID    string
		expectedIPConfig *compute.VirtualMachineScaleSetIPConfiguration
		expectedErr      error
	}{
		{
			desc: "only one IPv4 without primary (should not exist)",
			netConfig: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
						},
					},
				},
			},
			backendPoolID: testLBBackendpoolID0,
			expectedIPConfig: &compute.VirtualMachineScaleSetIPConfiguration{
				Name: ptr.To("config-0"),
			},
		},
		{
			desc: "two IPv4 but one with primary",
			netConfig: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(false),
							},
						},
						{
							Name: ptr.To("config-1"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(true),
							},
						},
					},
				},
			},
			backendPoolID: testLBBackendpoolID0,
			expectedIPConfig: &compute.VirtualMachineScaleSetIPConfiguration{
				Name: ptr.To("config-1"),
				VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
					Primary: ptr.To(true),
				},
			},
		},
		{
			desc: "multiple IPv4 without primary",
			netConfig: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(false),
							},
						},
						{
							Name: ptr.To("config-1"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(false),
							},
						},
					},
				},
			},
			backendPoolID: testLBBackendpoolID0,
			expectedErr:   fmt.Errorf("failed to find a primary IP configuration (IPv6=false) for the VMSS VM or VMSS \"vmss-config-0\""),
		},
		{
			desc: "dualstack for IPv4",
			netConfig: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								PrivateIPAddressVersion: compute.IPv4,
								Primary:                 ptr.To(true),
							},
						},
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								PrivateIPAddressVersion: compute.IPv6,
							},
						},
					},
				},
			},
			backendPoolID: testLBBackendpoolID0,
			expectedIPConfig: &compute.VirtualMachineScaleSetIPConfiguration{
				Name: ptr.To("config-0"),
				VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
					PrivateIPAddressVersion: compute.IPv4,
					Primary:                 ptr.To(true),
				},
			},
		},
		{
			desc: "dualstack for IPv6",
			netConfig: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								PrivateIPAddressVersion: compute.IPv4,
								Primary:                 ptr.To(true),
							},
						},
						{
							Name: ptr.To("config-0-IPv6"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								PrivateIPAddressVersion: compute.IPv6,
							},
						},
					},
				},
			},
			backendPoolID: testLBBackendpoolID0v6,
			expectedIPConfig: &compute.VirtualMachineScaleSetIPConfiguration{
				Name: ptr.To("config-0-IPv6"),
				VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
					PrivateIPAddressVersion: compute.IPv6,
				},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			ipConfig, err := getPrimaryIPConfigFromVMSSNetworkConfig(tc.netConfig, tc.backendPoolID, "vmss-config-0")
			assert.Equal(t, tc.expectedErr, err)
			assert.Equal(t, tc.expectedIPConfig, ipConfig)
		})
	}
}

func TestDeleteBackendPoolFromIPConfig(t *testing.T) {
	testcases := []struct {
		desc               string
		backendPoolID      string
		primaryNIC         *compute.VirtualMachineScaleSetNetworkConfiguration
		expectedPrimaryNIC *compute.VirtualMachineScaleSetNetworkConfiguration
		expectedFound      bool
		expectedErr        error
	}{
		{
			desc:          "delete backend pool from ip config",
			backendPoolID: "backendpool-0",
			primaryNIC: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(true),
								LoadBalancerBackendAddressPools: &[]compute.SubResource{
									{
										ID: ptr.To("backendpool-0"),
									},
									{
										ID: ptr.To("backendpool-1"),
									},
								},
							},
						},
					},
				},
			},
			expectedPrimaryNIC: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(true),
								LoadBalancerBackendAddressPools: &[]compute.SubResource{
									{
										ID: ptr.To("backendpool-1"),
									},
								},
							},
						},
					},
				},
			},
			expectedFound: true,
		},
		{
			desc:          "backend pool not found",
			backendPoolID: "backendpool-0",
			primaryNIC: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(true),
								LoadBalancerBackendAddressPools: &[]compute.SubResource{
									{
										ID: ptr.To("backendpool-1"),
									},
								},
							},
						},
					},
				},
			},
			expectedPrimaryNIC: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(true),
								LoadBalancerBackendAddressPools: &[]compute.SubResource{
									{
										ID: ptr.To("backendpool-1"),
									},
								},
							},
						},
					},
				},
			},
			expectedFound: false,
		},
		{
			desc:          "delete backend pool from ip config IPv6",
			backendPoolID: "backendpool-0-IPv6",
			primaryNIC: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(true),
								LoadBalancerBackendAddressPools: &[]compute.SubResource{
									{
										ID: ptr.To("backendpool-1"),
									},
								},
							},
						},
						{
							Name: ptr.To("config-1"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(false),
								LoadBalancerBackendAddressPools: &[]compute.SubResource{
									{
										ID: ptr.To("backendpool-0-IPv6"),
									},
								},
								PrivateIPAddressVersion: compute.IPv6,
							},
						},
					},
				},
			},
			expectedPrimaryNIC: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(true),
								LoadBalancerBackendAddressPools: &[]compute.SubResource{
									{
										ID: ptr.To("backendpool-1"),
									},
								},
							},
						},
						{
							Name: ptr.To("config-1"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary:                         ptr.To(false),
								LoadBalancerBackendAddressPools: &[]compute.SubResource{},
								PrivateIPAddressVersion:         compute.IPv6,
							},
						},
					},
				},
			},
			expectedFound: true,
		},
		{
			desc:          "primary IP config not found IPv4",
			backendPoolID: "backendpool-0",
			primaryNIC: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(false),
							},
						},
						{
							Name: ptr.To("config-1"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(false),
							},
						},
					},
				},
			},
			expectedPrimaryNIC: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(false),
							},
						},
						{
							Name: ptr.To("config-1"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary: ptr.To(false),
							},
						},
					},
				},
			},
			expectedFound: false,
			expectedErr:   fmt.Errorf("failed to find a primary IP configuration (IPv6=false) for the VMSS VM or VMSS \"test-resource\""),
		},
		{
			desc:          "primary IP config not found IPv6",
			backendPoolID: "backendpool-0-IPv6",
			primaryNIC: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary:                 ptr.To(true),
								PrivateIPAddressVersion: compute.IPv4,
							},
						},
						{
							Name: ptr.To("config-1"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary:                 ptr.To(false),
								PrivateIPAddressVersion: compute.IPv4,
							},
						},
					},
				},
			},
			expectedPrimaryNIC: &compute.VirtualMachineScaleSetNetworkConfiguration{
				VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
					IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
						{
							Name: ptr.To("config-0"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary:                 ptr.To(true),
								PrivateIPAddressVersion: compute.IPv4,
							},
						},
						{
							Name: ptr.To("config-1"),
							VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
								Primary:                 ptr.To(false),
								PrivateIPAddressVersion: compute.IPv4,
							},
						},
					},
				},
			},
			expectedFound: false,
			expectedErr:   fmt.Errorf("failed to find a primary IP configuration (IPv6=true) for the VMSS VM or VMSS \"test-resource\""),
		},
	}

	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			found, err := deleteBackendPoolFromIPConfig("test", tc.backendPoolID, "test-resource", tc.primaryNIC)
			assert.Equal(t, tc.expectedFound, found)
			assert.Equal(t, tc.expectedErr, err)
			assert.Equal(t, tc.expectedPrimaryNIC, tc.primaryNIC)
		})
	}
}

func TestEnsureHostInPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description               string
		service                   *v1.Service
		nodeName                  types.NodeName
		backendPoolID             string
		vmSetName                 string
		isBasicLB                 bool
		isNilVMNetworkConfigs     bool
		isVMBeingDeleted          bool
		isVMNotActive             bool
		expectedNodeResourceGroup string
		expectedVMSSName          string
		expectedInstanceID        string
		expectedVMSSVM            *compute.VirtualMachineScaleSetVM
		expectedErr               error
		vmssVMListError           *retry.Error
	}{
		{
			description: "EnsureHostInPool should skip the current node if the vmSetName is not equal to the node's vmss name and the basic LB is used",
			nodeName:    "vmss-vm-000000",
			vmSetName:   "vmss-1",
			isBasicLB:   true,
		},
		{
			description:           "EnsureHostInPool should skip the current node if the network configs of the VMSS VM is nil",
			nodeName:              "vmss-vm-000000",
			vmSetName:             "vmss",
			isNilVMNetworkConfigs: true,
		},
		{
			description:   "EnsureHostInPool should skip the current node if the backend pool has existed",
			service:       &v1.Service{Spec: v1.ServiceSpec{ClusterIP: "clusterIP"}},
			nodeName:      "vmss-vm-000000",
			vmSetName:     "vmss",
			backendPoolID: testLBBackendpoolID0,
		},
		{
			description:   "EnsureHostInPool should skip the current node if it has already been added to another LB",
			service:       &v1.Service{Spec: v1.ServiceSpec{ClusterIP: "clusterIP"}},
			nodeName:      "vmss-vm-000000",
			vmSetName:     "vmss",
			backendPoolID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1-internal/backendAddressPools/backendpool-1",
			isBasicLB:     false,
		},
		{
			description:      "EnsureHostInPool should skip the current node if it is being deleted",
			nodeName:         "vmss-vm-000000",
			isVMBeingDeleted: true,
		},
		{
			description:               "EnsureHostInPool should add a new backend pool to the vm",
			service:                   &v1.Service{Spec: v1.ServiceSpec{ClusterIP: "clusterIP"}},
			nodeName:                  "vmss-vm-000000",
			vmSetName:                 "vmss",
			backendPoolID:             "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1",
			isBasicLB:                 false,
			expectedNodeResourceGroup: "rg",
			expectedVMSSName:          testVMSSName,
			expectedInstanceID:        "0",
			expectedVMSSVM: &compute.VirtualMachineScaleSetVM{
				Location: ptr.To("westus"),
				VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
					NetworkProfileConfiguration: &compute.VirtualMachineScaleSetVMNetworkProfileConfiguration{
						NetworkInterfaceConfigurations: &[]compute.VirtualMachineScaleSetNetworkConfiguration{
							{
								Name: ptr.To("vmss-nic"),
								ID:   ptr.To("fakeNetworkConfiguration"),
								VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
									IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
										{
											Name: ptr.To("ipconfig1"),
											VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
												Primary: ptr.To(true),
												LoadBalancerBackendAddressPools: &[]compute.SubResource{
													{
														ID: ptr.To(testLBBackendpoolID0),
													},
													{
														ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb-internal/backendAddressPools/backendpool-1"),
													},
												},
												PrivateIPAddressVersion: compute.IPv4,
											},
										},
									},
									Primary: ptr.To(true),
								},
							},
						},
					},
				},
			},
		},
		{
			description:   "EnsureHostInPool should skip if the current node is not active",
			nodeName:      "vmss-vm-000000",
			isVMNotActive: true,
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.description)

		if !test.isBasicLB {
			ss.LoadBalancerSku = consts.LoadBalancerSkuStandard
		}

		expectedVMSS := buildTestVMSS(testVMSSName, "vmss-vm-")
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

		provisionState := ""
		if test.isVMBeingDeleted {
			provisionState = "Deleting"
		}
		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(
			ss.Cloud,
			testVMSSName,
			"",
			0,
			[]string{string(test.nodeName)},
			provisionState,
			false,
		)
		if test.isNilVMNetworkConfigs {
			expectedVMSSVMs[0].NetworkProfileConfiguration.NetworkInterfaceConfigurations = nil
		}
		if test.isVMNotActive {
			(*expectedVMSSVMs[0].InstanceView.Statuses)[0] = compute.InstanceViewStatus{
				Code: ptr.To("PowerState/deallocated"),
			}
		}
		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(
			gomock.Any(),
			ss.ResourceGroup,
			testVMSSName,
			gomock.Any(),
		).Return(expectedVMSSVMs, test.vmssVMListError).AnyTimes()

		nodeResourceGroup, ssName, instanceID, vm, err := ss.EnsureHostInPool(context.Background(), test.service, test.nodeName, test.backendPoolID, test.vmSetName)
		assert.Equal(t, test.expectedErr, err, test.description+errMsgSuffix)
		assert.Equal(t, test.expectedNodeResourceGroup, nodeResourceGroup, test.description)
		assert.Equal(t, test.expectedVMSSName, ssName, test.description)
		assert.Equal(t, test.expectedInstanceID, instanceID, test.description)
		assert.Equal(t, test.expectedVMSSVM, vm, test.description)
	}
}

func TestGetVmssAndResourceGroupNameByVMProviderID(t *testing.T) {
	providerID := "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0"
	rgName, vmssName, err := getVmssAndResourceGroupNameByVMProviderID(providerID)
	assert.NoError(t, err)
	assert.Equal(t, "rg", rgName)
	assert.Equal(t, "vmss", vmssName)

	providerID = "invalid/id"
	rgName, vmssName, err = getVmssAndResourceGroupNameByVMProviderID(providerID)
	assert.Equal(t, err, ErrorNotVmssInstance)
	assert.Equal(t, "", rgName)
	assert.Equal(t, "", vmssName)
}

func TestEnsureVMSSInPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description           string
		backendPoolID         string
		vmSetName             string
		clusterIP             string
		nodes                 []*v1.Node
		os                    osVersion
		isBasicLB             bool
		isVMSSDeallocating    bool
		isVMSSNilNICConfig    bool
		expectedGetInstanceID string
		expectedPutVMSS       bool
		setIPv6Config         bool
		useMultipleSLBs       bool
		getInstanceIDErr      error
		expectedErr           error
	}{
		{
			description: "ensureVMSSInPool should skip the node if it isn't managed by VMSS",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "invalid/id",
					},
				},
			},
			isBasicLB:       false,
			expectedPutVMSS: false,
		},
		{
			description: "ensureVMSSInPool should skip the node if the corresponding VMSS is deallocating",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			isBasicLB:          false,
			isVMSSDeallocating: true,
			expectedPutVMSS:    false,
		},
		{
			description: "ensureVMSSInPool should skip the node if the NIC config of the corresponding VMSS is nil",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			isBasicLB:          false,
			isVMSSNilNICConfig: true,
			expectedPutVMSS:    false,
		},
		{
			description: "ensureVMSSInPool should skip the node if the backendpool ID has been added already",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			isBasicLB:       false,
			backendPoolID:   testLBBackendpoolID0,
			expectedPutVMSS: false,
		},
		{
			description: "ensureVMSSInPool should skip the node if the VMSS has been added to another LB's backendpool already",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			isBasicLB:       false,
			backendPoolID:   "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/lb1/backendAddressPools/backendpool-0",
			expectedPutVMSS: false,
		},
		{
			description: "ensureVMSSInPool should update the VMSS correctly",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			isBasicLB:       false,
			backendPoolID:   testLBBackendpoolID1,
			expectedPutVMSS: true,
		},
		{
			description: "ensureVMSSInPool should fail if no IPv6 network config",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			isBasicLB:       false,
			backendPoolID:   testLBBackendpoolID1 + "-" + consts.IPVersionIPv6String,
			clusterIP:       "fd00::e68b",
			expectedPutVMSS: false,
			expectedErr:     fmt.Errorf("failed to find a primary IP configuration (IPv6=true) for the VMSS VM or VMSS \"vmss\""),
		},
		{
			description: "ensureVMSSInPool should skip Windows 2019 VM for IPv6 backend pool",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			isBasicLB:       false,
			backendPoolID:   testLBBackendpoolID1 + "-" + consts.IPVersionIPv6String,
			clusterIP:       "fd00::e68b",
			expectedPutVMSS: false,
			setIPv6Config:   false,
			expectedErr:     nil,
			os:              windows2019,
		},
		{
			description: "ensureVMSSInPool should add Windows2019 VM to IPv4 backend pool even if service is IPv6",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			isBasicLB:       false,
			backendPoolID:   testLBBackendpoolID1,
			clusterIP:       "fd00::e68b",
			expectedPutVMSS: true,
			setIPv6Config:   false,
			expectedErr:     nil,
			os:              windows2019,
		},
		{
			description: "ensureVMSSInPool should add Windows 2022 VM to IPv6 backend pool",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			isBasicLB:       false,
			backendPoolID:   testLBBackendpoolID1 + "-" + consts.IPVersionIPv6String,
			clusterIP:       "fd00::e68b",
			expectedPutVMSS: true,
			setIPv6Config:   true,
			expectedErr:     nil,
			os:              windows2022,
		},
		{
			description: "ensureVMSSInPool should fail if no IPv6 network config - Windows 2022",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			isBasicLB:       false,
			backendPoolID:   testLBBackendpoolID1 + "-" + consts.IPVersionIPv6String,
			clusterIP:       "fd00::e68b",
			expectedPutVMSS: false,
			setIPv6Config:   false,
			expectedErr:     fmt.Errorf("failed to find a primary IP configuration (IPv6=true) for the VMSS VM or VMSS \"vmss\""),
			os:              windows2022,
		},
		{
			description: "ensureVMSSInPool should update the VMSS correctly for IPv6",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			isBasicLB:       false,
			backendPoolID:   testLBBackendpoolID1 + "-" + consts.IPVersionIPv6String,
			setIPv6Config:   true,
			clusterIP:       "fd00::e68b",
			expectedPutVMSS: true,
		},
		{
			description: "ensureVMSSInPool should work for the basic load balancer",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			vmSetName:       testVMSSName,
			isBasicLB:       true,
			backendPoolID:   testLBBackendpoolID1,
			expectedPutVMSS: true,
		},
		{
			description: "ensureVMSSInPool should work for multiple standard load balancers",
			nodes: []*v1.Node{
				{
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
			},
			vmSetName:       testVMSSName,
			backendPoolID:   testLBBackendpoolID1,
			useMultipleSLBs: true,
			expectedPutVMSS: true,
		},
		{
			description: "ensureVMSSInPool should get the vmss name from vm instance ID if the provider ID of the node is empty",
			nodes: []*v1.Node{
				{},
			},
			vmSetName:             testVMSSName,
			backendPoolID:         testLBBackendpoolID1,
			expectedGetInstanceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
			expectedPutVMSS:       true,
		},
		{
			description: "ensureVMSSInPool should skip updating vmss if the node is not a vmss vm",
			nodes: []*v1.Node{
				{},
			},
			vmSetName:             testVMSSName,
			backendPoolID:         testLBBackendpoolID1,
			expectedGetInstanceID: "invalid",
		},
		{
			description: "ensureVMSSInPool should report an error if failed to get instance ID",
			nodes: []*v1.Node{
				{},
			},
			vmSetName:             testVMSSName,
			backendPoolID:         testLBBackendpoolID1,
			expectedGetInstanceID: "invalid",
			getInstanceIDErr:      errors.New("error"),
			expectedErr:           errors.New("error"),
		},
	}

	for _, test := range testCases {
		t.Run(test.description, func(t *testing.T) {
			ss, err := NewTestScaleSet(ctrl)
			assert.NoError(t, err, test.description)

			if !test.isBasicLB {
				ss.LoadBalancerSku = consts.LoadBalancerSkuStandard
			}

			expectedVMSS := buildTestOSSpecificVMSSWithLB(testVMSSName, "vmss-vm-", []string{testLBBackendpoolID0}, test.os, test.setIPv6Config)
			if test.isVMSSDeallocating {
				expectedVMSS.ProvisioningState = ptr.To(consts.ProvisionStateDeleting)
			}
			if test.isVMSSNilNICConfig {
				expectedVMSS.VirtualMachineProfile.NetworkProfile.NetworkInterfaceConfigurations = nil
			}
			mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()
			vmssPutTimes := 0
			if test.expectedPutVMSS {
				vmssPutTimes = 1
				mockVMSSClient.EXPECT().Get(gomock.Any(), ss.ResourceGroup, testVMSSName).Return(expectedVMSS, nil)
			}
			mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(nil).Times(vmssPutTimes)

			expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, []string{"vmss-vm-000000"}, "", test.setIPv6Config)
			mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
			mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

			if test.expectedGetInstanceID != "" {
				mockVMSet := NewMockVMSet(ctrl)
				mockVMSet.EXPECT().GetInstanceIDByNodeName(gomock.Any(), gomock.Any()).Return(test.expectedGetInstanceID, test.getInstanceIDErr)
				ss.VMSet = mockVMSet
			}

			err = ss.ensureVMSSInPool(context.TODO(), &v1.Service{Spec: v1.ServiceSpec{ClusterIP: test.clusterIP}}, test.nodes, test.backendPoolID, test.vmSetName)
			assert.Equal(t, test.expectedErr, err, test.description+errMsgSuffix)
		})
	}
}

func TestEnsureHostsInPool(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description            string
		nodes                  []*v1.Node
		backendpoolID          string
		vmSetName              string
		expectedVMSSVMPutTimes int
		expectedErr            bool
	}{
		{
			description: "EnsureHostsInPool should skip the invalid node and update the VMSS VM correctly",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "vmss-vm-000000",
						Labels: map[string]string{consts.NodeLabelRole: "master"},
					},
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "vmss-vm-000001",
						Labels: map[string]string{consts.ManagedByAzureLabel: "false"},
					},
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/1",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmss-vm-000002",
					},
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/2",
					},
				},
			},
			backendpoolID:          testLBBackendpoolID1,
			vmSetName:              testVMSSName,
			expectedVMSSVMPutTimes: 1,
		},
		{
			description: "EnsureHostsInPool should skip not found nodes",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "vmss-vm-000003",
					},
					Spec: v1.NodeSpec{
						ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/3",
					},
				},
			},
			backendpoolID:          testLBBackendpoolID1,
			vmSetName:              testVMSSName,
			expectedVMSSVMPutTimes: 0,
			expectedErr:            false,
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.description)

		ss.LoadBalancerSku = consts.LoadBalancerSkuStandard
		ss.ExcludeMasterFromStandardLB = ptr.To(true)

		expectedVMSS := buildTestVMSSWithLB(testVMSSName, "vmss-vm-", []string{testLBBackendpoolID0}, false)
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()
		mockVMSSClient.EXPECT().Get(gomock.Any(), ss.ResourceGroup, testVMSSName).Return(expectedVMSS, nil).MaxTimes(1)
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(nil).MaxTimes(1)

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, []string{"vmss-vm-000000", "vmss-vm-000001", "vmss-vm-000002"}, "", false)
		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()
		mockVMSSVMClient.EXPECT().UpdateVMs(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(test.expectedVMSSVMPutTimes)

		mockVMClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

		err = ss.EnsureHostsInPool(context.Background(), &v1.Service{}, test.nodes, test.backendpoolID, test.vmSetName)
		assert.Equal(t, test.expectedErr, err != nil, test.description+errMsgSuffix)
	}
}

func TestEnsureBackendPoolDeletedFromNodeCommon(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description               string
		nodeName                  string
		backendpoolIDs            []string
		isNilVMNetworkConfigs     bool
		isVMNotActive             bool
		expectedNodeResourceGroup string
		expectedVMSSName          string
		expectedInstanceID        string
		expectedVMSSVM            *compute.VirtualMachineScaleSetVM
		expectedErr               error
	}{
		{
			description: "ensureBackendPoolDeletedFromNode should skip not found nodes",
			nodeName:    "vmss-vm-000001",
		},
		{
			description:           "ensureBackendPoolDeletedFromNode skip the node if the VM's NIC config is nil",
			nodeName:              "vmss-vm-000000",
			isNilVMNetworkConfigs: true,
		},
		{
			description:    "ensureBackendPoolDeletedFromNode should skip the node if there's no wanted lb backendpool IDs on that VM",
			nodeName:       "vmss-vm-000000",
			backendpoolIDs: []string{testLBBackendpoolID1, testLBBackendpoolID1v6},
		},
		{
			description:               "ensureBackendPoolDeletedFromNode should delete the given backendpool IDs",
			nodeName:                  "vmss-vm-000000",
			backendpoolIDs:            []string{testLBBackendpoolID0, testLBBackendpoolID0v6},
			expectedNodeResourceGroup: "rg",
			expectedVMSSName:          testVMSSName,
			expectedInstanceID:        "0",
			expectedVMSSVM: &compute.VirtualMachineScaleSetVM{
				Location: ptr.To("westus"),
				VirtualMachineScaleSetVMProperties: &compute.VirtualMachineScaleSetVMProperties{
					NetworkProfileConfiguration: &compute.VirtualMachineScaleSetVMNetworkProfileConfiguration{
						NetworkInterfaceConfigurations: &[]compute.VirtualMachineScaleSetNetworkConfiguration{
							{
								Name: ptr.To("vmss-nic"),
								ID:   ptr.To("fakeNetworkConfiguration"),
								VirtualMachineScaleSetNetworkConfigurationProperties: &compute.VirtualMachineScaleSetNetworkConfigurationProperties{
									IPConfigurations: &[]compute.VirtualMachineScaleSetIPConfiguration{
										{
											Name: ptr.To("ipconfig1"),
											VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
												Primary:                         ptr.To(true),
												LoadBalancerBackendAddressPools: &[]compute.SubResource{},
												PrivateIPAddressVersion:         compute.IPv4,
											},
										},
										{
											Name: ptr.To("ipconfigv6"),
											VirtualMachineScaleSetIPConfigurationProperties: &compute.VirtualMachineScaleSetIPConfigurationProperties{
												Primary:                         ptr.To(false),
												LoadBalancerBackendAddressPools: &[]compute.SubResource{},
												PrivateIPAddressVersion:         compute.IPv6,
											},
										},
									},
									Primary: ptr.To(true),
								},
							},
						},
					},
				},
			},
		},
		{
			description:   "ensureBackendPoolDeletedFromNode should skip if the node is not active",
			nodeName:      "vmss-vm-000000",
			isVMNotActive: true,
		},
	}

	for _, test := range testCases {
		t.Run(test.description, func(t *testing.T) {
			ss, err := NewTestScaleSet(ctrl)
			assert.NoError(t, err, test.description)

			expectedVMSS := buildTestVMSS(testVMSSName, "vmss-vm-")
			mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()

			// isIPv6 true means it is a DualStack or IPv6 only cluster.
			expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, []string{"vmss-vm-000000"}, "", true)
			if test.isNilVMNetworkConfigs {
				expectedVMSSVMs[0].NetworkProfileConfiguration.NetworkInterfaceConfigurations = nil
			}
			if test.isVMNotActive {
				(*expectedVMSSVMs[0].InstanceView.Statuses)[0] = compute.InstanceViewStatus{
					Code: ptr.To("PowerState/deallocated"),
				}
			}
			mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
			mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

			nodeResourceGroup, ssName, instanceID, vm, err := ss.ensureBackendPoolDeletedFromNode(context.TODO(), test.nodeName, test.backendpoolIDs)
			assert.Equal(t, test.expectedErr, err)
			assert.Equal(t, test.expectedNodeResourceGroup, nodeResourceGroup)
			assert.Equal(t, test.expectedVMSSName, ssName)
			assert.Equal(t, test.expectedInstanceID, instanceID)
			assert.Equal(t, test.expectedVMSSVM, vm)
		})
	}
}

func TestGetScaleSetAndResourceGroupNameByIPConfigurationID(t *testing.T) {
	ipConfigID := "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/vmss-vm-000000/networkInterfaces/nic"
	scaleSetName, resourceGroup, err := getScaleSetAndResourceGroupNameByIPConfigurationID(ipConfigID)
	assert.Equal(t, "vmss", scaleSetName)
	assert.Equal(t, "rg", resourceGroup)
	assert.NoError(t, err)

	ipConfigID = "invalid/id"
	scaleSetName, resourceGroup, err = getScaleSetAndResourceGroupNameByIPConfigurationID(ipConfigID)
	assert.Equal(t, ErrorNotVmssInstance, err)
	assert.Equal(t, "", scaleSetName)
	assert.Equal(t, "", resourceGroup)
}

func TestEnsureBackendPoolDeletedFromVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description                    string
		backendPoolID                  string
		ipConfigurationIDs             []string
		isVMSSDeallocating             bool
		isVMSSNilNICConfig             bool
		isVMSSNilVirtualMachineProfile bool
		expectedPutVMSS                bool
		vmssClientErr                  *retry.Error
		expectedErr                    error
	}{
		{
			description:        "ensureBackendPoolDeletedFromVMSS should skip the VMSS if it's being deleting",
			isVMSSDeallocating: true,
		},
		{
			description:        "ensureBackendPoolDeletedFromVMSS should skip the VMSS if it's NIC config is nil",
			isVMSSNilNICConfig: true,
		},
		{
			description:     "ensureBackendPoolDeletedFromVMSS should delete the corresponding LB backendpool ID",
			backendPoolID:   testLBBackendpoolID0,
			expectedPutVMSS: true,
		},
		{
			description:   "ensureBackendPoolDeletedFromVMSS should skip the VMSS if there's no wanted LB backendpool ID",
			backendPoolID: testLBBackendpoolID1,
		},
		{
			description:                    "ensureBackendPoolDeletedFromVMSS should skip the VMSS if there's no vm profile",
			isVMSSNilVirtualMachineProfile: true,
		},
		{
			ipConfigurationIDs: []string{"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/vmss-vm-000000/networkInterfaces/nic"},
			backendPoolID:      testLBBackendpoolID0,
			expectedPutVMSS:    true,
			vmssClientErr:      &retry.Error{RawError: fmt.Errorf("error")},
			expectedErr:        utilerrors.NewAggregate([]error{fmt.Errorf("Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error")}),
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.description)

		ss.LoadBalancerSku = consts.LoadBalancerSkuStandard

		expectedVMSS := buildTestVMSSWithLB(testVMSSName, "vmss-vm-", []string{testLBBackendpoolID0}, false)
		if test.isVMSSDeallocating {
			expectedVMSS.ProvisioningState = ptr.To(consts.ProvisionStateDeleting)
		}
		if test.isVMSSNilNICConfig {
			expectedVMSS.VirtualMachineProfile.NetworkProfile.NetworkInterfaceConfigurations = nil
		}
		if test.isVMSSNilVirtualMachineProfile {
			expectedVMSS.VirtualMachineProfile = nil
		}
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes().MinTimes(1)
		vmssPutTimes := 0
		if test.expectedPutVMSS {
			vmssPutTimes = 1
			mockVMSSClient.EXPECT().Get(gomock.Any(), ss.ResourceGroup, testVMSSName).Return(expectedVMSS, nil)
		}
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(test.vmssClientErr).Times(vmssPutTimes)

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, []string{"vmss-vm-000000"}, "", false)
		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()

		err = ss.ensureBackendPoolDeletedFromVMSS(context.TODO(), []string{test.backendPoolID}, testVMSSName)
		if test.expectedErr != nil {
			assert.EqualError(t, test.expectedErr, err.Error(), test.description+errMsgSuffix)
		}
	}
}

func TestEnsureBackendPoolDeleted(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testCases := []struct {
		description            string
		backendpoolID          string
		backendAddressPools    *[]network.BackendAddressPool
		expectedVMSSVMPutTimes int
		vmClientErr            *retry.Error
		expectedErr            bool
	}{
		{
			description:   "EnsureBackendPoolDeleted should skip the unwanted backend address pools and update the VMSS VM correctly",
			backendpoolID: testLBBackendpoolID0,
			backendAddressPools: &[]network.BackendAddressPool{
				{
					ID: ptr.To(testLBBackendpoolID0),
					BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
						BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
							{
								Name: ptr.To("ip-1"),
								ID:   ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0/networkInterfaces/nic"),
							},
							{
								Name: ptr.To("ip-2"),
							},
							{
								Name: ptr.To("ip-3"),
								ID:   ptr.To("/invalid/id"),
							},
						},
					},
				},
				{
					ID: ptr.To(testLBBackendpoolID1),
				},
			},
			expectedVMSSVMPutTimes: 1,
		},
		{
			description:   "EnsureBackendPoolDeleted should report the error that occurs during the call of VMSS VM client",
			backendpoolID: testLBBackendpoolID0,
			backendAddressPools: &[]network.BackendAddressPool{
				{
					ID: ptr.To(testLBBackendpoolID0),
					BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
						BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
							{
								Name: ptr.To("ip-1"),
								ID:   ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0/networkInterfaces/nic"),
							},
						},
					},
				},
				{
					ID: ptr.To(testLBBackendpoolID1),
				},
			},
			expectedVMSSVMPutTimes: 1,
			expectedErr:            true,
			vmClientErr:            &retry.Error{RawError: fmt.Errorf("error")},
		},
		{
			description:   "EnsureBackendPoolDeleted should skip the node that doesn't exist",
			backendpoolID: testLBBackendpoolID0,
			backendAddressPools: &[]network.BackendAddressPool{
				{
					ID: ptr.To(testLBBackendpoolID0),
					BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
						BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
							{
								Name: ptr.To("ip-1"),
								ID:   ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/6/networkInterfaces/nic"),
							},
						},
					},
				},
				{
					ID: ptr.To(testLBBackendpoolID1),
				},
			},
		},
	}

	for _, test := range testCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.description)

		expectedVMSS := buildTestVMSSWithLB(testVMSSName, "vmss-vm-", []string{testLBBackendpoolID0}, false)
		mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).AnyTimes()
		mockVMSSClient.EXPECT().Get(gomock.Any(), ss.ResourceGroup, testVMSSName).Return(expectedVMSS, nil).MaxTimes(1)
		mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(nil).Times(1)

		expectedVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, []string{"vmss-vm-000000", "vmss-vm-000001", "vmss-vm-000002"}, "", false)
		mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any()).Return(expectedVMSSVMs, nil).AnyTimes()
		mockVMSSVMClient.EXPECT().UpdateVMs(gomock.Any(), ss.ResourceGroup, testVMSSName, gomock.Any(), gomock.Any(), gomock.Any()).Return(test.vmClientErr).Times(test.expectedVMSSVMPutTimes)

		mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		updated, err := ss.EnsureBackendPoolDeleted(context.TODO(), &v1.Service{}, []string{test.backendpoolID}, testVMSSName, test.backendAddressPools, true)
		assert.Equal(t, test.expectedErr, err != nil, test.description+errMsgSuffix)
		if !test.expectedErr && test.expectedVMSSVMPutTimes > 0 {
			assert.True(t, updated, test.description)
		}
	}
}

func TestEnsureBackendPoolDeletedConcurrentlyLoop(t *testing.T) {
	// run TestEnsureBackendPoolDeletedConcurrently 20 times to detect race conditions
	for i := 0; i < 20; i++ {
		TestEnsureBackendPoolDeletedConcurrently(t)
	}
}

func TestEnsureBackendPoolDeletedConcurrently(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ss, err := NewTestScaleSet(ctrl)
	assert.NoError(t, err)

	backendAddressPools := &[]network.BackendAddressPool{
		{
			ID: ptr.To(testLBBackendpoolID0),
			BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
				BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
					{
						Name: ptr.To("ip-1"),
						ID:   ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss-0/virtualMachines/0/networkInterfaces/nic"),
					},
				},
			},
		},
		{
			ID: ptr.To(testLBBackendpoolID1),
			BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
				BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
					{
						Name: ptr.To("ip-1"),
						ID:   ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss-1/virtualMachines/0/networkInterfaces/nic"),
					},
				},
			},
		},
		{
			ID: ptr.To(testLBBackendpoolID2),
			BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
				BackendIPConfigurations: &[]network.InterfaceIPConfiguration{
					{
						Name: ptr.To("ip-1"),
						ID:   ptr.To("/subscriptions/sub/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachineScaleSets/vmss-0/virtualMachines/0/networkInterfaces/nic"),
					},
				},
			},
		},
	}

	vmss0 := buildTestVMSSWithLB("vmss-0", "vmss-0-vm-", []string{testLBBackendpoolID0, testLBBackendpoolID1}, false)
	vmss1 := buildTestVMSSWithLB("vmss-1", "vmss-1-vm-", []string{testLBBackendpoolID0, testLBBackendpoolID1}, false)

	expectedVMSSVMsOfVMSS0, _, _ := buildTestVirtualMachineEnv(ss.Cloud, "vmss-0", "", 0, []string{"vmss-0-vm-000000"}, "succeeded", false)
	expectedVMSSVMsOfVMSS1, _, _ := buildTestVirtualMachineEnv(ss.Cloud, "vmss-1", "", 0, []string{"vmss-1-vm-000001"}, "succeeded", false)
	for _, expectedVMSSVMs := range [][]compute.VirtualMachineScaleSetVM{expectedVMSSVMsOfVMSS0, expectedVMSSVMsOfVMSS1} {
		vmssVMNetworkConfigs := expectedVMSSVMs[0].NetworkProfileConfiguration
		vmssVMIPConfigs := (*vmssVMNetworkConfigs.NetworkInterfaceConfigurations)[0].VirtualMachineScaleSetNetworkConfigurationProperties.IPConfigurations
		lbBackendpools := (*vmssVMIPConfigs)[0].LoadBalancerBackendAddressPools
		*lbBackendpools = append(*lbBackendpools, compute.SubResource{ID: ptr.To(testLBBackendpoolID1)})
	}

	mockVMClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
	mockVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachine{}, nil).AnyTimes()

	mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
	mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{vmss0, vmss1}, nil).AnyTimes()
	mockVMSSClient.EXPECT().List(gomock.Any(), "rg1").Return(nil, nil).AnyTimes()
	mockVMSSClient.EXPECT().Get(gomock.Any(), ss.ResourceGroup, "vmss-0").Return(vmss0, nil).MaxTimes(2)
	mockVMSSClient.EXPECT().Get(gomock.Any(), ss.ResourceGroup, "vmss-1").Return(vmss1, nil).MaxTimes(2)
	mockVMSSClient.EXPECT().CreateOrUpdate(gomock.Any(), ss.ResourceGroup, gomock.Any(), gomock.Any()).Return(nil).Times(2)

	mockVMSSVMClient := ss.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
	mockVMSSVMClient.EXPECT().List(gomock.Any(), "rg1", "vmss-0", gomock.Any()).Return(nil, nil).AnyTimes()
	mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, "vmss-0", gomock.Any()).Return(expectedVMSSVMsOfVMSS0, nil).AnyTimes()
	mockVMSSVMClient.EXPECT().List(gomock.Any(), ss.ResourceGroup, "vmss-1", gomock.Any()).Return(expectedVMSSVMsOfVMSS1, nil).AnyTimes()
	mockVMSSVMClient.EXPECT().UpdateVMs(gomock.Any(), ss.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).Times(2)

	backendpoolAddressIDs := []string{testLBBackendpoolID0, testLBBackendpoolID1, testLBBackendpoolID2}
	testVMSSNames := []string{"vmss-0", "vmss-1", "vmss-2"}
	testFunc := make([]func() error, 0)
	for i, id := range backendpoolAddressIDs {
		i := i
		id := id
		testFunc = append(testFunc, func() error {
			_, err := ss.EnsureBackendPoolDeleted(context.TODO(), &v1.Service{}, []string{id}, testVMSSNames[i], backendAddressPools, true)
			return err
		})
	}
	errs := utilerrors.AggregateGoroutines(testFunc...)
	assert.Equal(t, 1, len(errs.Errors()))
	assert.Equal(t, "instance not found", errs.Error())
}

func TestGetNodeCIDRMasksByProviderID(t *testing.T) {
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
			expectedErr: fmt.Errorf("getVMManagementTypeByProviderID : failed to check the providerID invalid management type"),
		},
		{
			description: "GetNodeCIDRMaksByProviderID should return the correct mask sizes",
			providerID:  "azure:///subscriptions/sub/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("24"),
				consts.VMSetCIDRIPV6TagKey: ptr.To("64"),
			},
			expectedIPV4MaskSize: 24,
			expectedIPV6MaskSize: 64,
		},
		{
			description: "GetNodeCIDRMaksByProviderID should return the correct mask sizes even if some of the tags are not specified",
			providerID:  "azure:///subscriptions/sub/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("24"),
			},
			expectedIPV4MaskSize: 24,
		},
		{
			description: "GetNodeCIDRMaksByProviderID should not fail even if some of the tag is invalid",
			providerID:  "azure:///subscriptions/sub/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
			tags: map[string]*string{
				consts.VMSetCIDRIPV4TagKey: ptr.To("abc"),
				consts.VMSetCIDRIPV6TagKey: ptr.To("64"),
			},
			expectedIPV6MaskSize: 64,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			ss, err := NewTestScaleSet(ctrl)
			assert.NoError(t, err)

			expectedVMSS := compute.VirtualMachineScaleSet{
				Name: ptr.To("vmss"),
				Tags: tc.tags,
				VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
					OrchestrationMode: compute.Uniform,
				},
			}
			mockVMSSClient := ss.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
			mockVMSSClient.EXPECT().List(gomock.Any(), ss.ResourceGroup).Return([]compute.VirtualMachineScaleSet{expectedVMSS}, nil).MaxTimes(1)

			mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
			mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

			ipv4MaskSize, ipv6MaskSize, err := ss.GetNodeCIDRMasksByProviderID(context.TODO(), tc.providerID)
			assert.Equal(t, tc.expectedErr, err, tc.description)
			assert.Equal(t, tc.expectedIPV4MaskSize, ipv4MaskSize, tc.description)
			assert.Equal(t, tc.expectedIPV6MaskSize, ipv6MaskSize, tc.description)
		})
	}
}

func TestGetAgentPoolVMSetNamesMixedInstances(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ss, err := NewTestScaleSet(ctrl)
	assert.NoError(t, err)

	existingVMs := []compute.VirtualMachine{
		{
			Name: ptr.To("vm-0"),
			VirtualMachineProperties: &compute.VirtualMachineProperties{
				OsProfile: &compute.OSProfile{
					ComputerName: ptr.To("vm-0"),
				},
				AvailabilitySet: &compute.SubResource{
					ID: ptr.To("vmas-0"),
				},
			},
		},
		{
			Name: ptr.To("vm-1"),
			VirtualMachineProperties: &compute.VirtualMachineProperties{
				OsProfile: &compute.OSProfile{
					ComputerName: ptr.To("vm-1"),
				},
				AvailabilitySet: &compute.SubResource{
					ID: ptr.To("vmas-1"),
				},
			},
		},
	}
	expectedScaleSet := buildTestVMSS(testVMSSName, "vmssee6c2")
	vmList := []string{"vmssee6c2000000", "vmssee6c2000001", "vmssee6c2000002"}
	existingVMSSVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, testVMSSName, "", 0, vmList, "", false)

	mockVMClient := ss.Cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
	mockVMClient.EXPECT().List(gomock.Any(), gomock.Any()).Return(existingVMs, nil).AnyTimes()

	mockVMSSClient := ss.Cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
	mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expectedScaleSet}, nil)

	mockVMSSVMClient := ss.Cloud.VirtualMachineScaleSetVMsClient.(*mockvmssvmclient.MockInterface)
	mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), testVMSSName, gomock.Any()).Return(existingVMSSVMs, nil)

	nodes := []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "vmssee6c2000000",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "vm-0",
			},
		},
	}
	expectedVMSetNames := &[]string{testVMSSName, "vmas-0"}
	vmSetNames, err := ss.GetAgentPoolVMSetNames(context.TODO(), nodes)
	assert.NoError(t, err)
	assert.Equal(t, expectedVMSetNames, vmSetNames)
}

func TestGetNodeVMSetNameVMSS(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	node := &v1.Node{
		Spec: v1.NodeSpec{
			ProviderID: "invalid",
		},
	}

	ss, err := NewTestScaleSet(ctrl)
	assert.NoError(t, err)

	mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
	mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

	vmSetName, err := ss.GetNodeVMSetName(context.TODO(), node)
	assert.Equal(t, ErrorNotVmssInstance, err)
	assert.Equal(t, "", vmSetName)

	node = &v1.Node{
		Spec: v1.NodeSpec{
			ProviderID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss/virtualMachines/0",
		},
	}

	vmSetName, err = ss.GetNodeVMSetName(context.TODO(), node)
	assert.NoError(t, err)
	assert.Equal(t, "vmss", vmSetName)
}

func TestScaleSet_VMSSBatchSize(t *testing.T) {

	const (
		BatchSize = 500
	)

	t.Run("failed to get vmss", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err)

		var (
			vmssName   = "foo"
			getVMSSErr = &retry.Error{RawError: fmt.Errorf("list vmss error")}
		)
		mockVMSSClient := ss.Cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).
			Return(nil, getVMSSErr)

		_, err = ss.VMSSBatchSize(context.TODO(), vmssName)
		assert.Error(t, err)
	})

	t.Run("vmss contains batch operation tag", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err)
		ss.Cloud.PutVMSSVMBatchSize = BatchSize

		scaleSet := compute.VirtualMachineScaleSet{
			Name: ptr.To("foo"),
			Tags: map[string]*string{
				consts.VMSSTagForBatchOperation: ptr.To(""),
			},
			VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
				OrchestrationMode: compute.Uniform,
			},
		}
		mockVMSSClient := ss.Cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).
			Return([]compute.VirtualMachineScaleSet{scaleSet}, nil)

		batchSize, err := ss.VMSSBatchSize(context.TODO(), ptr.Deref(scaleSet.Name, ""))
		assert.NoError(t, err)
		assert.Equal(t, BatchSize, batchSize)
	})

	t.Run("vmss doesn't contain batch operation tag", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err)
		ss.Cloud.PutVMSSVMBatchSize = BatchSize

		scaleSet := compute.VirtualMachineScaleSet{
			Name: ptr.To("bar"),
			VirtualMachineScaleSetProperties: &compute.VirtualMachineScaleSetProperties{
				OrchestrationMode: compute.Uniform,
			},
		}
		mockVMSSClient := ss.Cloud.VirtualMachineScaleSetsClient.(*mockvmssclient.MockInterface)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).
			Return([]compute.VirtualMachineScaleSet{scaleSet}, nil)

		batchSize, err := ss.VMSSBatchSize(context.TODO(), ptr.Deref(scaleSet.Name, ""))
		assert.NoError(t, err)
		assert.Equal(t, 0, batchSize)
	})
}
