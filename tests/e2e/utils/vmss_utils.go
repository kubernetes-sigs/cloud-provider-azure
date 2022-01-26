/*
Copyright 2019 The Kubernetes Authors.

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

package utils

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2020-12-01/compute"
	azcompute "github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2020-12-01/compute"
	"github.com/Azure/go-autorest/autorest/to"

	"k8s.io/apimachinery/pkg/util/wait"
)

var (
	vmssVMRE        = regexp.MustCompile(`/subscriptions/(?:.*)/resourceGroups/(?:.+)/providers/Microsoft.Compute/virtualMachineScaleSets/(.+)/virtualMachines/(?:\d+)`)
	errVMSSNotFound = fmt.Errorf("cannot find any VMSS")
)

// FindTestVMSS returns the first VMSS in the resource group,
// assume the VMSS is in the cluster
func FindTestVMSS(tc *AzureTestClient, rgName string) (*azcompute.VirtualMachineScaleSet, error) {
	Logf("FindTestVMSS: start")

	vmssClient := tc.createVMSSClient()

	list, err := vmssClient.List(context.Background(), rgName)
	if err != nil {
		return nil, err
	}

	vmssList := list.Values()
	if len(vmssList) == 0 {
		return nil, nil
	}

	return &vmssList[0], nil
}

// ScaleVMSS scales the given VMSS
func ScaleVMSS(tc *AzureTestClient, vmssName, rgName string, instanceCount int64) (err error) {
	Logf("ScaleVMSS: start")

	vmssClient := tc.createVMSSClient()

	vmss, err := vmssClient.Get(context.Background(), rgName, vmssName)
	if err != nil {
		return err
	}
	parameters := azcompute.VirtualMachineScaleSet{
		Location: to.StringPtr(tc.GetLocation()),
		Sku: &azcompute.Sku{
			Name:     vmss.Sku.Name,
			Capacity: to.Int64Ptr(instanceCount),
		},
	}

	Logf("ScaleVMSS: scaling VMSS %s", vmssName)
	_, err = vmssClient.CreateOrUpdate(context.Background(), rgName, vmssName, parameters)
	if err != nil {
		return err
	}

	Logf("ScaleVMSS: wait the scaling process to be over")
	err = waitVMSSVMCountToEqual(tc, int(instanceCount), vmssName)
	return err
}

// IsNodeInVMSS defines whether the node is the instance of the VMSS
func IsNodeInVMSS(tc *AzureTestClient, nodeName, vmssName string) (bool, error) {
	vms, err := ListVMSSVMs(tc, vmssName)
	if err != nil {
		return false, err
	}
	if vms == nil || len(*vms) == 0 {
		return false, fmt.Errorf("failed to find any VM in VMSS %s", vmssName)
	}

	var vmsInVMSS []azcompute.VirtualMachineScaleSetVM
	for _, vm := range *vms {
		vmssNameMatches := vmssVMRE.FindStringSubmatch(*vm.ID)
		if len(vmssNameMatches) != 2 {
			return false, fmt.Errorf("cannot obtain the name of VMSS from vmssVM.ID")
		}

		if vmssName == vmssNameMatches[1] {
			vmsInVMSS = append(vmsInVMSS, vm)
		}
	}

	for _, vmInVMSS := range vmsInVMSS {
		if vmInVMSS.OsProfile != nil && vmInVMSS.OsProfile.ComputerName != nil &&
			strings.EqualFold(nodeName, *vmInVMSS.OsProfile.ComputerName) {
			return true, nil
		}
	}

	return false, nil
}

func waitVMSSVMCountToEqual(tc *AzureTestClient, expected int, vmssName string) error {
	cs, err := CreateKubeClientSet()
	if err != nil {
		return err
	}

	err = wait.PollImmediate(vmssOperationInterval, vmssOperationTimeout, func() (bool, error) {
		nodes, err := GetAgentNodes(cs)
		if err != nil {
			return false, err
		}

		count := 0
		for _, node := range nodes {
			flag, err := IsNodeInVMSS(tc, node.Name, vmssName)
			if err != nil {
				return false, err
			}

			if !flag {
				continue
			}

			count++
		}

		Logf("current = %d, expected = %d", count, expected)
		return count == expected, nil
	})

	return err
}

// ValidateVMSSNodeLabels gets the label of VMs in VMSS with retry
func ValidateVMSSNodeLabels(tc *AzureTestClient, vmss *azcompute.VirtualMachineScaleSet, key string) error {
	cs, err := CreateKubeClientSet()
	if err != nil {
		return err
	}

	err = wait.PollImmediate(vmssOperationInterval, vmssOperationTimeout, func() (bool, error) {
		nodes, err := GetAgentNodes(cs)
		if err != nil {
			return false, err
		}

		for _, node := range nodes {
			flag, err := IsNodeInVMSS(tc, node.Name, *vmss.Name)
			if err != nil {
				return false, err
			}
			if !flag {
				continue
			}
			labels := node.Labels
			if labels == nil {
				return false, fmt.Errorf("cannot find labels on node %s", node.Name)
			}
			if _, ok := labels[key]; !ok {
				return false, nil
			}
		}
		return true, nil
	})

	return err
}

// ListVMSSVMs returns the VM list of the given VMSS
func ListVMSSVMs(tc *AzureTestClient, vmssName string) (*[]azcompute.VirtualMachineScaleSetVM, error) {
	vmssVMClient := tc.createVMSSVMClient()

	list, err := vmssVMClient.List(context.Background(), tc.GetResourceGroup(), vmssName, "", "", "")
	if err != nil {
		return nil, err
	}

	res := list.Values()
	if len(res) == 0 {
		return nil, fmt.Errorf("cannot find any VMSS VM in VMSS %s of resource group %s", vmssName, tc.GetResourceGroup())
	}

	return &res, nil
}

// ListVMSSes returns the list of scale sets
func ListVMSSes(tc *AzureTestClient) (*[]azcompute.VirtualMachineScaleSet, error) {
	vmssClient := tc.createVMSSClient()

	list, err := vmssClient.List(context.Background(), tc.GetResourceGroup())
	if err != nil {
		return nil, err
	}

	res := list.Values()
	if len(res) == 0 {
		return nil, errVMSSNotFound
	}

	return &res, nil
}

// GetVMSSVMComputerName returns the corresponding node name of the VMSS VM
func GetVMSSVMComputerName(vm azcompute.VirtualMachineScaleSetVM) (string, error) {
	if vm.OsProfile == nil || vm.OsProfile.ComputerName == nil {
		return "", fmt.Errorf("cannot find computer name from vmss vm %s", *vm.Name)
	}

	return *vm.OsProfile.ComputerName, nil
}

// IsSpotVMSS checks whether the vmss support azure spot vm instance
func IsSpotVMSS(vmss azcompute.VirtualMachineScaleSet) bool {
	return vmss.VirtualMachineProfile.Priority == compute.VirtualMachinePriorityTypesSpot
}
