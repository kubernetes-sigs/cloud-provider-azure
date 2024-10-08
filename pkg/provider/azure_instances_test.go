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
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/interfaceclient/mockinterfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/publicipclient/mockpublicipclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient/mockvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient/mockvmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssvmclient/mockvmssvmclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// setTestVirtualMachines sets test virtual machine with powerstate.
func setTestVirtualMachines(c *Cloud, vmList map[string]string, isDataDisksFull bool) []compute.VirtualMachine {
	expectedVMs := make([]compute.VirtualMachine, 0)

	for nodeName, powerState := range vmList {
		nodeName := nodeName
		instanceID := fmt.Sprintf("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/%s", nodeName)
		vm := compute.VirtualMachine{
			Name:     &nodeName,
			ID:       &instanceID,
			Location: &c.Location,
		}
		status := []compute.InstanceViewStatus{
			{
				Code: ptr.To(powerState),
			},
			{
				Code: ptr.To("ProvisioningState/succeeded"),
			},
		}
		vm.VirtualMachineProperties = &compute.VirtualMachineProperties{
			ProvisioningState: ptr.To(string(consts.ProvisioningStateSucceeded)),
			HardwareProfile: &compute.HardwareProfile{
				VMSize: compute.StandardA0,
			},
			InstanceView: &compute.VirtualMachineInstanceView{
				Statuses: &status,
			},
			StorageProfile: &compute.StorageProfile{
				DataDisks: &[]compute.DataDisk{},
			},
		}
		if !isDataDisksFull {
			vm.StorageProfile.DataDisks = &[]compute.DataDisk{
				{
					Lun:  ptr.To(int32(0)),
					Name: ptr.To("disk1"),
				},
				{
					Lun:  ptr.To(int32(1)),
					Name: ptr.To("disk2"),
				},
				{
					Lun:  ptr.To(int32(2)),
					Name: ptr.To("disk3"),
				},
			}
		} else {
			dataDisks := make([]compute.DataDisk, maxLUN)
			for i := 0; i < maxLUN; i++ {
				dataDisks[i] = compute.DataDisk{Lun: ptr.To(int32(i))}
			}
			vm.StorageProfile.DataDisks = &dataDisks
		}

		expectedVMs = append(expectedVMs, vm)
	}

	return expectedVMs
}

func TestInstanceID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)

	testcases := []struct {
		name                string
		vmList              []string
		nodeName            string
		vmssName            string
		metadataName        string
		resourceID          string
		metadataTemplate    string
		vmType              string
		expectedID          string
		useInstanceMetadata bool
		useCustomImsCache   bool
		nilVMSet            bool
		expectedErrMsg      error
	}{
		{
			name:                "InstanceID should get instanceID if node's name are equal to metadataName",
			vmList:              []string{"vm1"},
			nodeName:            "vm1",
			metadataName:        "vm1",
			resourceID:          "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
			vmType:              consts.VMTypeStandard,
			useInstanceMetadata: true,
			expectedID:          "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
		},
		{
			name:                "InstanceID should get vmss instanceID from local if node's name are equal to metadataName and metadata.Compute.VMScaleSetName is not null",
			vmList:              []string{"vmss1_0"},
			vmssName:            "vmss1",
			nodeName:            "vmss1_0",
			metadataName:        "vmss1_0",
			resourceID:          "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss1/virtualMachines/0",
			vmType:              consts.VMTypeStandard,
			useInstanceMetadata: true,
			expectedID:          "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmss1/virtualMachines/0",
		},
		{
			name:                "InstanceID should get standard instanceID from local if node's name are equal to metadataName and format of nodeName is not compliance with vmss instance",
			vmList:              []string{"vmss1-0"},
			vmssName:            "vmss1",
			nodeName:            "vmss1-0",
			metadataName:        "vmss1-0",
			resourceID:          "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vmss1-0",
			vmType:              consts.VMTypeStandard,
			useInstanceMetadata: true,
			expectedID:          "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vmss1-0",
		},
		{
			name:                "InstanceID should get instanceID from Azure API if node is not local instance",
			vmList:              []string{"vm2"},
			nodeName:            "vm2",
			metadataName:        "vm1",
			vmType:              consts.VMTypeStandard,
			useInstanceMetadata: true,
			expectedID:          "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm2",
		},
		{
			name:         "InstanceID should get instanceID from Azure API if cloud.UseInstanceMetadata is false",
			vmList:       []string{"vm2"},
			nodeName:     "vm2",
			metadataName: "vm2",
			vmType:       consts.VMTypeStandard,
			expectedID:   "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm2",
		},
		{
			name:                "InstanceID should report error if node doesn't exist",
			vmList:              []string{"vm1"},
			nodeName:            "vm3",
			vmType:              consts.VMTypeStandard,
			useInstanceMetadata: true,
			expectedErrMsg:      fmt.Errorf("instance not found"),
		},
		{
			name:                "InstanceID should report error if metadata.Compute is nil",
			nodeName:            "vm1",
			metadataName:        "vm1",
			vmType:              consts.VMTypeStandard,
			metadataTemplate:    `{"network":{"interface":[]}}`,
			useInstanceMetadata: true,
			expectedErrMsg:      fmt.Errorf("failure of getting instance metadata"),
		},
		{
			name:                "NodeAddresses should report error if cloud.VMSet is nil",
			nodeName:            "vm1",
			vmType:              consts.VMTypeStandard,
			useInstanceMetadata: true,
			nilVMSet:            true,
			expectedErrMsg:      fmt.Errorf("no credentials provided for Azure cloud provider"),
		},
		{
			name:                "NodeAddresses should report error if invoking GetMetadata returns error",
			nodeName:            "vm1",
			metadataName:        "vm1",
			vmType:              consts.VMTypeStandard,
			useCustomImsCache:   true,
			useInstanceMetadata: true,
			expectedErrMsg:      fmt.Errorf("getError"),
		},
	}

	for _, test := range testcases {
		if test.nilVMSet {
			cloud.VMSet = nil
		} else {
			cloud.VMSet, _ = newAvailabilitySet(cloud)
		}
		cloud.Config.VMType = test.vmType
		cloud.Config.UseInstanceMetadata = test.useInstanceMetadata
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Errorf("Test [%s] unexpected error: %v", test.name, err)
		}

		mux := http.NewServeMux()
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if test.metadataTemplate != "" {
				fmt.Fprint(w, test.metadataTemplate)
			} else {
				fmt.Fprintf(w, "{\"compute\":{\"name\":\"%s\",\"VMScaleSetName\":\"%s\",\"subscriptionId\":\"subscription\",\"resourceGroupName\":\"rg\", \"resourceId\":\"%s\"}}", test.metadataName, test.vmssName, test.resourceID)
			}
		}))
		go func() {
			_ = http.Serve(listener, mux)
		}()
		defer listener.Close()

		cloud.Metadata, err = NewInstanceMetadataService("http://" + listener.Addr().String() + "/")
		if err != nil {
			t.Errorf("Test [%s] unexpected error: %v", test.name, err)
		}
		if test.useCustomImsCache {
			cloud.Metadata.imsCache, err = azcache.NewTimedCache(consts.MetadataCacheTTL, func(_ string) (interface{}, error) {
				return nil, fmt.Errorf("getError")
			}, false)
			if err != nil {
				t.Errorf("Test [%s] unexpected error: %v", test.name, err)
			}
		}
		vmListWithPowerState := make(map[string]string)
		for _, vm := range test.vmList {
			vmListWithPowerState[vm] = ""
		}
		expectedVMs := setTestVirtualMachines(cloud, vmListWithPowerState, false)
		mockVMsClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "vm3", gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		mockVMsClient.EXPECT().Update(gomock.Any(), cloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

		instanceID, err := cloud.InstanceID(context.Background(), types.NodeName(test.nodeName))
		assert.Equal(t, test.expectedErrMsg, err, test.name)
		assert.Equal(t, test.expectedID, instanceID, test.name)
	}
}

func TestInstanceShutdownByProviderID(t *testing.T) {
	testcases := []struct {
		name              string
		vmList            map[string]string
		nodeName          string
		providerID        string
		provisioningState string
		expected          bool
		expectedErrMsg    error
	}{
		{
			name:       "InstanceShutdownByProviderID should return false if the vm is in PowerState/Running status",
			vmList:     map[string]string{"vm1": "PowerState/Running"},
			nodeName:   "vm1",
			providerID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
			expected:   false,
		},
		{
			name:       "InstanceShutdownByProviderID should return true if the vm is in PowerState/Deallocated status",
			vmList:     map[string]string{"vm2": "PowerState/Deallocated"},
			nodeName:   "vm2",
			providerID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm2",
			expected:   true,
		},
		{
			name:       "InstanceShutdownByProviderID should return false if the vm is in PowerState/Deallocating status",
			vmList:     map[string]string{"vm3": "PowerState/Deallocating"},
			nodeName:   "vm3",
			providerID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm3",
			expected:   true,
		},
		{
			name:       "InstanceShutdownByProviderID should return false if the vm is in PowerState/Starting status",
			vmList:     map[string]string{"vm4": "PowerState/Starting"},
			nodeName:   "vm4",
			providerID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm4",
			expected:   false,
		},
		{
			name:       "InstanceShutdownByProviderID should return true if the vm is in PowerState/Stopped status",
			vmList:     map[string]string{"vm5": "PowerState/Stopped"},
			nodeName:   "vm5",
			providerID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm5",
			expected:   true,
		},
		{
			name:              "InstanceShutdownByProviderID should return false if the vm is in PowerState/Stopped state with Creating provisioning state",
			vmList:            map[string]string{"vm5": "PowerState/Stopped"},
			nodeName:          "vm5",
			provisioningState: "Creating",
			providerID:        "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm5",
			expected:          false,
		},
		{
			name:       "InstanceShutdownByProviderID should return false if the vm is in PowerState/Stopping status",
			vmList:     map[string]string{"vm6": "PowerState/Stopping"},
			nodeName:   "vm6",
			providerID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm6",
			expected:   false,
		},
		{
			name:       "InstanceShutdownByProviderID should return false if the vm is in PowerState/Unknown status",
			vmList:     map[string]string{"vm7": "PowerState/Unknown"},
			nodeName:   "vm7",
			providerID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm7",
			expected:   false,
		},
		{
			name:       "InstanceShutdownByProviderID should return false if node doesn't exist",
			vmList:     map[string]string{"vm1": "PowerState/running"},
			nodeName:   "vm8",
			providerID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm8",
			expected:   false,
		},
		{
			name:     "InstanceShutdownByProviderID should report error if providerID is null",
			nodeName: "vmm",
			expected: false,
		},
		{
			name:           "InstanceShutdownByProviderID should report error if providerID is invalid",
			nodeName:       "vm9",
			providerID:     "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/VM/vm9",
			expected:       false,
			expectedErrMsg: fmt.Errorf("error splitting providerID"),
		},
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	for _, test := range testcases {
		cloud := GetTestCloud(ctrl)
		expectedVMs := setTestVirtualMachines(cloud, test.vmList, false)
		if test.provisioningState != "" {
			expectedVMs[0].ProvisioningState = ptr.To(test.provisioningState)
		}
		mockVMsClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, test.nodeName, gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()

		hasShutdown, err := cloud.InstanceShutdownByProviderID(context.Background(), test.providerID)
		assert.Equal(t, test.expectedErrMsg, err, test.name)
		assert.Equal(t, test.expected, hasShutdown, test.name)

		hasShutdown, err = cloud.InstanceShutdown(context.Background(), &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: test.nodeName},
			Spec: v1.NodeSpec{
				ProviderID: test.providerID,
			},
		})
		assert.Equal(t, test.expectedErrMsg, err, test.name)
		assert.Equal(t, test.expected, hasShutdown, test.name)
	}
}

func TestNodeAddresses(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)

	expectedVM := compute.VirtualMachine{
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			NetworkProfile: &compute.NetworkProfile{
				NetworkInterfaces: &[]compute.NetworkInterfaceReference{
					{
						NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{
							Primary: ptr.To(true),
						},
						ID: ptr.To("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic"),
					},
				},
			},
		},
	}

	expectedPIP := network.PublicIPAddress{
		Name: ptr.To("pip1"),
		ID:   ptr.To("/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip1"),
		PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
			IPAddress: ptr.To("192.168.1.12"),
		},
	}

	expectedInterface := network.Interface{
		InterfacePropertiesFormat: &network.InterfacePropertiesFormat{
			IPConfigurations: &[]network.InterfaceIPConfiguration{
				{
					InterfaceIPConfigurationPropertiesFormat: &network.InterfaceIPConfigurationPropertiesFormat{
						PrivateIPAddress: ptr.To("172.1.0.3"),
						PublicIPAddress:  &expectedPIP,
					},
				},
			},
		},
	}

	expectedNodeAddress := []v1.NodeAddress{
		{
			Type:    v1.NodeInternalIP,
			Address: "172.1.0.3",
		},
		{
			Type:    v1.NodeHostName,
			Address: "vm1",
		},
		{
			Type:    v1.NodeExternalIP,
			Address: "192.168.1.12",
		},
	}
	metadataTemplate := `{"compute":{"name":"%s"},"network":{"interface":[{"ipv4":{"ipAddress":[{"privateIpAddress":"%s","publicIpAddress":"%s"}]},"ipv6":{"ipAddress":[{"privateIpAddress":"%s","publicIpAddress":"%s"}]}}]}}`
	loadbalancerTemplate := `{"loadbalancer": {"publicIpAddresses": [{"frontendIpAddress": "%s","privateIpAddress": "%s"},{"frontendIpAddress": "%s","privateIpAddress": "%s"}]}}`

	testcases := []struct {
		name                string
		nodeName            string
		metadataName        string
		metadataTemplate    string
		vmType              string
		ipV4                string
		ipV6                string
		ipV4Public          string
		ipV6Public          string
		loadBalancerSku     string
		expectedAddress     []v1.NodeAddress
		useInstanceMetadata bool
		useCustomImsCache   bool
		nilVMSet            bool
		expectedErrMsg      error
	}{
		{
			name:                "NodeAddresses should report error if metadata.Network is nil",
			metadataTemplate:    `{"compute":{"name":"vm1"}}`,
			useInstanceMetadata: true,
			expectedErrMsg:      fmt.Errorf("failure of getting instance metadata"),
		},
		{
			name:                "NodeAddresses should report error if metadata.Compute is nil",
			metadataTemplate:    `{"network":{"interface":[]}}`,
			useInstanceMetadata: true,
			expectedErrMsg:      fmt.Errorf("failure of getting instance metadata"),
		},
		{
			name:                "NodeAddresses should report error if metadata.Network.Interface is nil",
			nodeName:            "vm1",
			metadataName:        "vm1",
			vmType:              consts.VMTypeStandard,
			metadataTemplate:    `{"compute":{"name":"vm1"},"network":{}}`,
			useInstanceMetadata: true,
			expectedErrMsg:      fmt.Errorf("no interface is found for the instance"),
		},
		{
			name:                "NodeAddresses should report error when invoke GetMetadata",
			nodeName:            "vm1",
			metadataName:        "vm1",
			vmType:              consts.VMTypeStandard,
			useCustomImsCache:   true,
			useInstanceMetadata: true,
			expectedErrMsg:      fmt.Errorf("getError"),
		},
		{
			name:                "NodeAddresses should report error if cloud.VMSet is nil",
			nodeName:            "vm1",
			vmType:              consts.VMTypeStandard,
			useInstanceMetadata: true,
			nilVMSet:            true,
			expectedErrMsg:      fmt.Errorf("no credentials provided for Azure cloud provider"),
		},
		{
			name:                "NodeAddresses should report error when IPs are empty",
			nodeName:            "vm1",
			metadataName:        "vm1",
			vmType:              consts.VMTypeStandard,
			useInstanceMetadata: true,
			expectedErrMsg:      fmt.Errorf("get empty IP addresses from instance metadata service"),
		},
		{
			name:                "NodeAddresses should report error if node don't exist",
			nodeName:            "vm2",
			metadataName:        "vm1",
			vmType:              consts.VMTypeStandard,
			useInstanceMetadata: true,
			expectedErrMsg:      wait.ErrorInterrupted(fmt.Errorf("timed out waiting for the condition")),
		},
		{
			name:                "NodeAddresses should get IP addresses from Azure API if node's name isn't equal to metadataName",
			nodeName:            "vm1",
			vmType:              consts.VMTypeStandard,
			useInstanceMetadata: true,
			expectedAddress:     expectedNodeAddress,
		},
		{
			name:            "NodeAddresses should get IP addresses from Azure API if useInstanceMetadata is false",
			nodeName:        "vm1",
			vmType:          consts.VMTypeStandard,
			expectedAddress: expectedNodeAddress,
		},
		{
			name:                "NodeAddresses should get IP addresses from local IMDS if node's name is equal to metadataName",
			nodeName:            "vm1",
			metadataName:        "vm1",
			vmType:              consts.VMTypeStandard,
			ipV4:                "10.240.0.1",
			ipV4Public:          "192.168.1.12",
			ipV6:                "1111:11111:00:00:1111:1111:000:111",
			ipV6Public:          "2222:22221:00:00:2222:2222:000:111",
			loadBalancerSku:     "basic",
			useInstanceMetadata: true,
			expectedAddress: []v1.NodeAddress{
				{
					Type:    v1.NodeHostName,
					Address: "vm1",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "10.240.0.1",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "192.168.1.12",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "1111:11111:00:00:1111:1111:000:111",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "2222:22221:00:00:2222:2222:000:111",
				},
			},
		},
		{
			name:                "NodeAddresses should get IP addresses from local IMDS for standard LoadBalancer if node's name is equal to metadataName",
			nodeName:            "vm1",
			metadataName:        "vm1",
			vmType:              consts.VMTypeStandard,
			ipV4:                "10.240.0.1",
			ipV4Public:          "192.168.1.12",
			ipV6:                "1111:11111:00:00:1111:1111:000:111",
			ipV6Public:          "2222:22221:00:00:2222:2222:000:111",
			loadBalancerSku:     "standard",
			useInstanceMetadata: true,
			expectedAddress: []v1.NodeAddress{
				{
					Type:    v1.NodeHostName,
					Address: "vm1",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "10.240.0.1",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "192.168.1.12",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "1111:11111:00:00:1111:1111:000:111",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "2222:22221:00:00:2222:2222:000:111",
				},
			},
		},
	}

	for _, test := range testcases {
		if test.nilVMSet {
			cloud.VMSet = nil
		} else {
			cloud.VMSet, _ = newAvailabilitySet(cloud)
		}
		cloud.Config.VMType = test.vmType
		cloud.Config.UseInstanceMetadata = test.useInstanceMetadata
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Errorf("Test [%s] unexpected error: %v", test.name, err)
		}

		mux := http.NewServeMux()
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.RequestURI, consts.ImdsLoadBalancerURI) {
				fmt.Fprintf(w, loadbalancerTemplate, test.ipV4Public, test.ipV4, test.ipV6Public, test.ipV6)
				return
			}

			if test.metadataTemplate != "" {
				fmt.Fprint(w, test.metadataTemplate)
			} else {
				if test.loadBalancerSku == "standard" {
					fmt.Fprintf(w, metadataTemplate, test.metadataName, test.ipV4, "", test.ipV6, "")
				} else {
					fmt.Fprintf(w, metadataTemplate, test.metadataName, test.ipV4, test.ipV4Public, test.ipV6, test.ipV6Public)
				}
			}
		}))
		go func() {
			_ = http.Serve(listener, mux)
		}()
		defer listener.Close()

		cloud.Metadata, err = NewInstanceMetadataService("http://" + listener.Addr().String() + "/")
		if err != nil {
			t.Errorf("Test [%s] unexpected error: %v", test.name, err)
		}

		if test.useCustomImsCache {
			cloud.Metadata.imsCache, err = azcache.NewTimedCache(consts.MetadataCacheTTL, func(_ string) (interface{}, error) {
				return nil, fmt.Errorf("getError")
			}, false)
			if err != nil {
				t.Errorf("Test [%s] unexpected error: %v", test.name, err)
			}
		}
		mockVMClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "vm1", gomock.Any()).Return(expectedVM, nil).AnyTimes()
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "vm2", gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()

		mockPublicIPAddressesClient := cloud.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
		mockPublicIPAddressesClient.EXPECT().List(gomock.Any(), cloud.ResourceGroup).Return([]network.PublicIPAddress{expectedPIP}, nil).AnyTimes()

		mockInterfaceClient := cloud.InterfacesClient.(*mockinterfaceclient.MockInterface)
		mockInterfaceClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "nic", gomock.Any()).Return(expectedInterface, nil).AnyTimes()

		ipAddresses, err := cloud.NodeAddresses(context.Background(), types.NodeName(test.nodeName))
		assert.Equal(t, test.expectedErrMsg, err, test.name)
		assert.Equal(t, test.expectedAddress, ipAddresses, test.name)
	}
}

func TestInstanceExistsByProviderID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)

	testcases := []struct {
		name           string
		vmList         []string
		nodeName       string
		providerID     string
		expected       bool
		expectedErrMsg error
	}{
		{
			name:       "InstanceExistsByProviderID should return true if node exists",
			vmList:     []string{"vm2"},
			nodeName:   "vm2",
			providerID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm2",
			expected:   true,
		},
		{
			name:       "InstanceExistsByProviderID should return true if node is unmanaged",
			providerID: "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
			expected:   true,
		},
		{
			name:       "InstanceExistsByProviderID should return false if node doesn't exist",
			vmList:     []string{"vm1"},
			nodeName:   "vm3",
			providerID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm3",
			expected:   false,
		},
		{
			name:           "InstanceExistsByProviderID should report error if providerID is invalid",
			providerID:     "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachine/vm3",
			expected:       false,
			expectedErrMsg: fmt.Errorf("error splitting providerID"),
		},
		{
			name:           "InstanceExistsByProviderID should report error if providerID is null",
			expected:       false,
			expectedErrMsg: fmt.Errorf("providerID is empty, the node is not initialized yet"),
		},
	}

	for _, test := range testcases {
		vmListWithPowerState := make(map[string]string)
		for _, vm := range test.vmList {
			vmListWithPowerState[vm] = ""
		}
		expectedVMs := setTestVirtualMachines(cloud, vmListWithPowerState, false)
		mockVMsClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		for _, vm := range expectedVMs {
			mockVMsClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, *vm.Name, gomock.Any()).Return(vm, nil).AnyTimes()
		}
		mockVMsClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "vm3", gomock.Any()).Return(compute.VirtualMachine{}, &retry.Error{HTTPStatusCode: http.StatusNotFound, RawError: cloudprovider.InstanceNotFound}).AnyTimes()
		mockVMsClient.EXPECT().Update(gomock.Any(), cloud.ResourceGroup, gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()

		exist, err := cloud.InstanceExistsByProviderID(context.Background(), test.providerID)
		assert.Equal(t, test.expectedErrMsg, err, test.name)
		assert.Equal(t, test.expected, exist, test.name)
	}

	vmssTestCases := []struct {
		name       string
		providerID string
		scaleSet   string
		vmList     []string
		expected   bool
		rerr       *retry.Error
	}{
		{
			name:       "InstanceExistsByProviderID should return true if VMSS and VM exist",
			providerID: "azure:///subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssee6c2/virtualMachines/0",
			scaleSet:   "vmssee6c2",
			vmList:     []string{"vmssee6c2000000"},
			expected:   true,
		},
		{
			name:       "InstanceExistsByProviderID should return false if VMSS exist but VM doesn't",
			providerID: "azure:///subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/vmssee6c2/virtualMachines/0",
			scaleSet:   "vmssee6c2",
			expected:   false,
		},
		{
			name:       "InstanceExistsByProviderID should return false if VMSS doesn't exist",
			providerID: "azure:///subscriptions/script/resourceGroups/rg/providers/Microsoft.Compute/virtualMachineScaleSets/missing-vmss/virtualMachines/0",
			rerr:       &retry.Error{HTTPStatusCode: 404},
			expected:   false,
		},
	}

	for _, test := range vmssTestCases {
		ss, err := NewTestScaleSet(ctrl)
		assert.NoError(t, err, test.name)
		cloud.VMSet = ss

		mockVMSSClient := mockvmssclient.NewMockInterface(ctrl)
		mockVMSSVMClient := mockvmssvmclient.NewMockInterface(ctrl)
		ss.VirtualMachineScaleSetsClient = mockVMSSClient
		ss.VirtualMachineScaleSetVMsClient = mockVMSSVMClient

		expectedScaleSet := buildTestVMSS(test.scaleSet, test.scaleSet)
		mockVMSSClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachineScaleSet{expectedScaleSet}, test.rerr).AnyTimes()

		expectedVMs, _, _ := buildTestVirtualMachineEnv(ss.Cloud, test.scaleSet, "", 0, test.vmList, "succeeded", false)
		mockVMSSVMClient.EXPECT().List(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(expectedVMs, test.rerr).AnyTimes()

		mockVMsClient := ss.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMsClient.EXPECT().List(gomock.Any(), gomock.Any()).Return([]compute.VirtualMachine{}, nil).AnyTimes()

		exist, _ := cloud.InstanceExistsByProviderID(context.Background(), test.providerID)
		assert.Equal(t, test.expected, exist, test.name)
	}
}

func TestNodeAddressesByProviderID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)
	cloud.Config.UseInstanceMetadata = true
	metadataTemplate := `{"compute":{"name":"%s"},"network":{"interface":[{"ipv4":{"ipAddress":[{"privateIpAddress":"%s","publicIpAddress":"%s"}]},"ipv6":{"ipAddress":[{"privateIpAddress":"%s","publicIpAddress":"%s"}]}}]}}`

	testcases := []struct {
		name            string
		nodeName        string
		ipV4            string
		ipV6            string
		ipV4Public      string
		ipV6Public      string
		providerID      string
		expectedAddress []v1.NodeAddress
		expectedErrMsg  error
	}{
		{
			name:       "NodeAddressesByProviderID should get both ipV4 and ipV6 private addresses",
			nodeName:   "vm1",
			providerID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
			ipV4:       "10.240.0.1",
			ipV6:       "1111:11111:00:00:1111:1111:000:111",
			expectedAddress: []v1.NodeAddress{
				{
					Type:    v1.NodeHostName,
					Address: "vm1",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "10.240.0.1",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "1111:11111:00:00:1111:1111:000:111",
				},
			},
		},
		{
			name:           "NodeAddressesByProviderID should report error when IPs are empty",
			nodeName:       "vm1",
			providerID:     "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
			expectedErrMsg: fmt.Errorf("get empty IP addresses from instance metadata service"),
		},
		{
			name:       "NodeAddressesByProviderID should return nil if node is unmanaged",
			providerID: "/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1",
		},
		{
			name:           "NodeAddressesByProviderID should report error if providerID is invalid",
			providerID:     "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/virtualMachine/vm3",
			expectedErrMsg: fmt.Errorf("error splitting providerID"),
		},
		{
			name:           "NodeAddressesByProviderID should report error if providerID is null",
			expectedErrMsg: fmt.Errorf("providerID is empty, the node is not initialized yet"),
		},
	}

	for _, test := range testcases {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Errorf("Test [%s] unexpected error: %v", test.name, err)
		}

		mux := http.NewServeMux()
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, metadataTemplate, test.nodeName, test.ipV4, test.ipV4Public, test.ipV6, test.ipV6Public)
		}))
		go func() {
			_ = http.Serve(listener, mux)
		}()
		defer listener.Close()

		cloud.Metadata, err = NewInstanceMetadataService("http://" + listener.Addr().String() + "/")
		if err != nil {
			t.Errorf("Test [%s] unexpected error: %v", test.name, err)
		}

		ipAddresses, err := cloud.NodeAddressesByProviderID(context.Background(), test.providerID)
		assert.Equal(t, test.expectedErrMsg, err, test.name)
		assert.Equal(t, test.expectedAddress, ipAddresses, test.name)
	}
}

func TestCurrentNodeName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	cloud := GetTestCloud(ctrl)

	hostname := "testvm"
	nodeName, err := cloud.CurrentNodeName(context.Background(), hostname)
	assert.Equal(t, types.NodeName(hostname), nodeName)
	assert.NoError(t, err)
}

func TestInstanceMetadata(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	t.Run("instance not exists", func(t *testing.T) {
		cloud := GetTestCloud(ctrl)
		cloud.unmanagedNodes = utilsets.NewString("node0")

		meta, err := cloud.InstanceMetadata(context.Background(), &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node0",
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, cloudprovider.InstanceMetadata{}, *meta)
	})

	t.Run("instance exists", func(t *testing.T) {
		cloud := GetTestCloud(ctrl)
		expectedVM := buildDefaultTestVirtualMachine("as", []string{"/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/k8s-agentpool1-00000000-nic-1"})
		expectedVM.HardwareProfile = &compute.HardwareProfile{
			VMSize: compute.BasicA0,
		}
		expectedVM.Location = ptr.To("westus2")
		expectedVM.Zones = &[]string{"1"}
		expectedVM.ID = ptr.To("/subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/VirtualMachines/vm")
		mockVMClient := cloud.VirtualMachinesClient.(*mockvmclient.MockInterface)
		mockVMClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "vm", gomock.Any()).Return(expectedVM, nil)
		expectedNIC := buildDefaultTestInterface(true, []string{})
		(*expectedNIC.IPConfigurations)[0].PrivateIPAddress = ptr.To("1.2.3.4")
		(*expectedNIC.IPConfigurations)[0].PublicIPAddress = &network.PublicIPAddress{
			ID: ptr.To("pip"),
			PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
				IPAddress: ptr.To("5.6.7.8"),
			},
		}
		mockNICClient := cloud.InterfacesClient.(*mockinterfaceclient.MockInterface)
		mockNICClient.EXPECT().Get(gomock.Any(), cloud.ResourceGroup, "k8s-agentpool1-00000000-nic-1", gomock.Any()).Return(expectedNIC, nil)
		expectedPIP := network.PublicIPAddress{
			Name: ptr.To("pip"),
			PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{
				IPAddress: ptr.To("5.6.7.8"),
			},
		}
		mockPIPClient := cloud.PublicIPAddressesClient.(*mockpublicipclient.MockInterface)
		mockPIPClient.EXPECT().List(gomock.Any(), cloud.ResourceGroup).Return([]network.PublicIPAddress{expectedPIP}, nil)

		expectedMetadata := cloudprovider.InstanceMetadata{
			ProviderID:   "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/VirtualMachines/vm",
			InstanceType: string(compute.BasicA0),
			NodeAddresses: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "1.2.3.4",
				},
				{
					Type:    v1.NodeHostName,
					Address: "vm",
				},
				{
					Type:    v1.NodeExternalIP,
					Address: "5.6.7.8",
				},
			},
			Zone:   "westus2-1",
			Region: "westus2",
		}
		meta, err := cloud.InstanceMetadata(context.Background(), &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "vm",
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, expectedMetadata, *meta)
	})
}

func TestCloud_InstanceExists(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	t.Run("should not return error when instance not found by node name", func(t *testing.T) {
		cloud := GetTestCloud(ctrl)
		cloud.VMSet = NewMockVMSet(ctrl) // FIXME(lodrem): implement MockCloud and init in MockCloud constructor

		ctx := context.Background()
		node := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		}

		cloud.VMSet.(*MockVMSet).EXPECT().GetInstanceIDByNodeName(gomock.Any(), "foo").Return("", cloudprovider.InstanceNotFound)

		exist, err := cloud.InstanceExists(ctx, node)
		assert.False(t, exist)
		assert.NoError(t, err)
	})

	t.Run("should not return error when instance not found by provider id", func(t *testing.T) {
		cloud := GetTestCloud(ctrl)
		cloud.VMSet = NewMockVMSet(ctrl) // FIXME(lodrem): implement MockCloud and init in MockCloud constructor

		ctx := context.Background()
		node := &v1.Node{
			Spec: v1.NodeSpec{ProviderID: "azure:///subscriptions/subscription/resourceGroups/rg/providers/Microsoft.Compute/VirtualMachines/vm"},
		}

		cloud.VMSet.(*MockVMSet).EXPECT().GetNodeNameByProviderID(node.Spec.ProviderID).Return(types.NodeName(""), cloudprovider.InstanceNotFound)

		exist, err := cloud.InstanceExists(ctx, node)
		assert.NoError(t, err)
		assert.False(t, exist)
	})
	t.Run("should return true when instance is not managed by azure", func(t *testing.T) {
		ctx := context.Background()
		cloud := GetTestCloud(ctrl)
		cloud.unmanagedNodes = utilsets.NewString("foo")
		node := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
				Labels: map[string]string{
					"kubernetes.azure.com/managed": "false",
				},
			},
		}

		exist, err := cloud.InstanceExists(ctx, node)
		assert.NoError(t, err)
		assert.True(t, exist)
	})
}
