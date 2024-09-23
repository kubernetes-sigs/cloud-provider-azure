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
	"os"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	azcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/pointer"
)

var (
	vmssVMRE = regexp.MustCompile(`/subscriptions/(?:.*)/resourceGroups/(?:.+)/providers/Microsoft.Compute/virtualMachineScaleSets/(.+)/virtualMachines/(?:\d+)`)
)

// FindTestVMSS returns the first VMSS in the resource group,
// assume the VMSS is in the cluster
func FindTestVMSS(tc *AzureTestClient, rgName string) (*azcompute.VirtualMachineScaleSet, error) {
	Logf("FindTestVMSS: start")

	vmssClient := tc.createVMSSClient()

	vmssList, err := vmssClient.List(context.Background(), rgName)
	if err != nil {
		return nil, err
	}

	for i := range vmssList {
		vmss := vmssList[i]
		if IsAutoscalingAKSCluster() && vmss.Name != nil && strings.Contains(*vmss.Name, SystemPool) {
			continue
		}
		return vmss, nil
	}
	return nil, nil
}

func Scale(tc *AzureTestClient, vmssName string, instanceCount int64) error {
	if strings.EqualFold(os.Getenv(CAPZTestCCM), "true") {
		return ScaleMachinePool(vmssName, instanceCount)
	}
	return ScaleVMSS(tc, vmssName, instanceCount)
}

// ScaleVMSS scales the given VMSS
func ScaleVMSS(tc *AzureTestClient, vmssName string, instanceCount int64) (err error) {
	Logf("ScaleVMSS: start")
	rgName := tc.GetResourceGroup()

	vmssClient := tc.createVMSSClient()

	vmss, err := vmssClient.Get(context.Background(), rgName, vmssName, to.Ptr(azcompute.ExpandTypesForGetVMScaleSetsUserData))
	if err != nil {
		return err
	}
	parameters := azcompute.VirtualMachineScaleSet{
		Location: vmss.Location,
		SKU: &azcompute.SKU{
			Name:     vmss.SKU.Name,
			Capacity: pointer.Int64(instanceCount),
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
	if len(vms) == 0 {
		return false, fmt.Errorf("failed to find any VM in VMSS %s", vmssName)
	}

	var vmsInVMSS []*azcompute.VirtualMachineScaleSetVM
	for _, vm := range vms {
		vmssNameMatches := vmssVMRE.FindStringSubmatch(*vm.ID)
		if len(vmssNameMatches) != 2 {
			return false, fmt.Errorf("cannot obtain the name of VMSS from vmssVM.ID")
		}

		if vmssName == vmssNameMatches[1] {
			vmsInVMSS = append(vmsInVMSS, vm)
		}
	}

	for _, vmInVMSS := range vmsInVMSS {
		if vmInVMSS.Properties != nil && vmInVMSS.Properties.OSProfile != nil && vmInVMSS.Properties.OSProfile.ComputerName != nil && strings.EqualFold(nodeName, *vmInVMSS.Properties.OSProfile.ComputerName) {
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
		vms, err := ListVMSSVMs(tc, vmssName)
		if err != nil {
			return false, err
		}
		if len(vms) > 0 {
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
		}

		Logf("Number of VMSS instance in %s: current = %d, expected = %d (will retry)", vmssName, count, expected)
		return count == expected, nil
	})

	return err
}

// WaitVMSSVMCountToEqualNodeCount waits until the number of VMSS VMs equals the number of Nodes.
func WaitVMSSVMCountToEqualNodeCount(tc *AzureTestClient) error {
	cs, err := CreateKubeClientSet()
	if err != nil {
		return err
	}

	vmssList, _ := ListUniformVMSSes(tc)
	vmssNames := []string{}
	for _, vmss := range vmssList {
		if vmss.Name == nil {
			return fmt.Errorf("VMSS name is nil, VMSS: %v", vmss)
		}
		vmssNames = append(vmssNames, *vmss.Name)
	}

	err = wait.PollImmediate(vmssOperationInterval, vmssOperationTimeout, func() (bool, error) {
		nodes, err := GetAgentNodes(cs)
		if err != nil {
			return false, err
		}

		vmssVMCount := 0
		for _, vmssName := range vmssNames {
			vms, err := ListVMSSVMs(tc, vmssName)
			if err != nil {
				return false, err
			}
			vmssVMCount += len(vms)
		}
		if vmssVMCount == len(nodes) {
			return true, nil
		}

		Logf("Number of VMSS instances %d of VMSSes (%q) doesn't equal number of Nodes %d (will retry)", vmssVMCount, vmssNames, len(nodes))
		return false, nil
	})
	return err
}

func ValidateClusterNodesMatchVMSSInstances(tc *AzureTestClient, expectedCap map[string]int64, originalNodes []v1.Node) error {
	k8sCli, err := CreateKubeClientSet()
	if err != nil {
		return err
	}

	originalNodeSet := sets.NewString()
	for _, originalNode := range originalNodes {
		originalNodeSet.Insert(strings.ToLower(originalNode.Name))
	}

	return wait.PollImmediate(vmssOperationInterval, vmssOperationTimeout, func() (bool, error) {
		var (
			err         error
			nodes       []v1.Node
			nodeSet     = sets.NewString()
			instanceSet = sets.NewString()
			actualCap   = map[string]int64{}
		)

		// log progress
		defer func() {
			if err != nil {
				Logf("Failed to validate VMSS instances: %s", err)
			}

			Logf("Matching cluster nodes[%s] with VMSS instances[%s]\nExpected capacity: %v, actual capacity: %v",
				strings.Join(nodeSet.List(), ","),
				strings.Join(instanceSet.List(), ","),
				expectedCap,
				actualCap)
		}()

		nodes, err = GetAgentNodes(k8sCli)
		if err != nil {
			return false, err
		}
		for _, node := range nodes {
			nodeSet.Insert(strings.ToLower(node.Name))
		}

		// ignore error; check intersection of sets instead.
		vmssList, _ := ListUniformVMSSes(tc)
		capMatch := true
		for _, vmss := range vmssList {
			vms, err := ListVMSSVMs(tc, *vmss.Name)
			if err != nil {
				return false, err
			}
			vmssInstanceSet := sets.NewString()
			for _, vm := range vms {
				var nodeName string
				nodeName, err = GetVMSSVMComputerName(vm)
				if err != nil {
					return false, err
				}
				vmssInstanceSet.Insert(strings.ToLower(nodeName))
				instanceSet.Insert(strings.ToLower(nodeName))
			}
			cap, ok := expectedCap[*vmss.Name]
			if !ok {
				continue
			}

			actualCap[*vmss.Name] = *vmss.SKU.Capacity
			if cap != *vmss.SKU.Capacity {
				if !strings.Contains(os.Getenv(AKSClusterType), "autoscaling") {
					Logf("It is not an autoscaling cluster and vmss SKU capacity does not equal expectedCap")
					capMatch = false
					break
				}
				if cap != int64(originalNodeSet.Intersection(vmssInstanceSet).Len()) {
					// For autoscaling cluster, simply comparing the capacity may not work since if the number of current nodes is lower than the "minCount", a new node may be created after scaling down.
					// In this situation, we compare the expected capacity with the length of intersection between original nodes and current nodes.
					Logf("VMSS %q sku capacity is expected to be %d, but actually %d", *vmss.Name, cap, *vmss.SKU.Capacity)
					capMatch = false
					break
				}
			}
		}

		return nodeSet.Equal(instanceSet) && capMatch, nil
	})
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
func ListVMSSVMs(tc *AzureTestClient, vmssName string) ([]*azcompute.VirtualMachineScaleSetVM, error) {
	vmssVMClient := tc.createVMSSVMClient()

	list, err := vmssVMClient.List(context.Background(), tc.GetResourceGroup(), vmssName)
	if err != nil {
		return nil, err
	}

	return list, nil
}

// GetVMSS gets VMSS object with vmssName.
func GetVMSS(tc *AzureTestClient, vmssName string) (*azcompute.VirtualMachineScaleSet, error) {
	vmssClient := tc.createVMSSClient()
	return vmssClient.Get(context.Background(), tc.GetResourceGroup(), vmssName, nil)
}

// ListUniformVMSSes returns the list of scale sets
func ListUniformVMSSes(tc *AzureTestClient) ([]*azcompute.VirtualMachineScaleSet, error) {
	vmssClient := tc.createVMSSClient()

	list, err := vmssClient.List(context.Background(), tc.GetResourceGroup())
	if err != nil {
		return nil, err
	}

	vmssUniforms := make([]*azcompute.VirtualMachineScaleSet, 0)
	for i := range list {
		vmssUniform := list[i]
		if IsAutoscalingAKSCluster() && vmssUniform.Name != nil && strings.Contains(*vmssUniform.Name, SystemPool) {
			continue
		}
		if vmssUniform.Properties.OrchestrationMode == nil || *vmssUniform.Properties.OrchestrationMode == "" || *vmssUniform.Properties.OrchestrationMode == azcompute.OrchestrationModeUniform {
			vmssUniforms = append(vmssUniforms, vmssUniform)
		}
	}
	return vmssUniforms, nil
}

// GetVMSSVMComputerName returns the corresponding node name of the VMSS VM
func GetVMSSVMComputerName(vm *azcompute.VirtualMachineScaleSetVM) (string, error) {
	if vm.Properties == nil || vm.Properties.OSProfile == nil || vm.Properties.OSProfile.ComputerName == nil {
		return "", fmt.Errorf("cannot find computer name from vmss vm %s", *vm.Name)
	}

	return *vm.Properties.OSProfile.ComputerName, nil
}

// IsSpotVMSS checks whether the vmss support azure spot vm instance
func IsSpotVMSS(vmss *azcompute.VirtualMachineScaleSet) bool {
	if vmss.Properties == nil || vmss.Properties.VirtualMachineProfile == nil || vmss.Properties.VirtualMachineProfile.Priority == nil {
		return false
	}
	return *vmss.Properties.VirtualMachineProfile.Priority == azcompute.VirtualMachinePriorityTypesSpot
}
