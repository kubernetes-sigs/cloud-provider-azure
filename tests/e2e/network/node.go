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

package network

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

const (
	vmssVMZoneLabelKey = "failure-domain.beta.kubernetes.io/zone"
	vmssScaleUpCelling = 10
)

var (
	vmProviderIDRE     = regexp.MustCompile(`azure:///subscriptions/(.+)/resourceGroups/(.+)/providers/Microsoft.Compute/virtualMachines/(.+)`)
	vmssVMProviderIDRE = regexp.MustCompile(`azure:///subscriptions/(.+)/resourceGroups/(.+)/providers/Microsoft.Compute/virtualMachineScaleSets/(.+)/virtualMachines/(\d+)`)
	vmNameRE           = regexp.MustCompile(`(k8s-.+-\d+)-.+`)
)

var _ = Describe("Azure node resources", Label(utils.TestSuiteLabelNode), func() {
	basename := "node-resources"

	var cs clientset.Interface
	var ns *v1.Namespace
	var tc *utils.AzureTestClient

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		tc, err = utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		err := utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
	})

	It("should set node provider id correctly", func() {
		utils.Logf("getting meta info of test environment")
		authConfig := tc.GetAuthConfig()
		subscriptionID := authConfig.SubscriptionID
		rgName := tc.GetResourceGroup()

		utils.Logf("getting VMs")
		vms, err := utils.ListVMs(tc)
		Expect(err).NotTo(HaveOccurred())

		if vms != nil && len(*vms) != 0 {
			for _, vm := range *vms {
				nodeName, err := utils.GetVMComputerName(vm)
				Expect(err).NotTo(HaveOccurred())

				node, err := utils.GetNode(cs, strings.ToLower(nodeName))
				Expect(err).NotTo(HaveOccurred())

				providerID := node.Spec.ProviderID
				providerIDMatches := vmProviderIDRE.FindStringSubmatch(providerID)
				Expect(len(providerIDMatches)).To(Equal(4))
				Expect(strings.EqualFold(providerIDMatches[1], subscriptionID)).To(BeTrue())
				Expect(strings.EqualFold(providerIDMatches[2], rgName)).To(BeTrue())
				Expect(strings.EqualFold(providerIDMatches[3], *vm.Name)).To(BeTrue())
			}
		}

		utils.Logf("getting VMSS VMs")
		vmsses, err := utils.ListVMSSes(tc)
		Expect(err).NotTo(HaveOccurred())

		if len(vmsses) != 0 {
			for _, vmss := range vmsses {
				vmssVMs, err := utils.ListVMSSVMs(tc, *vmss.Name)
				Expect(err).NotTo(HaveOccurred())

				for _, vmssVM := range vmssVMs {
					nodeName, err := utils.GetVMSSVMComputerName(vmssVM)
					Expect(err).NotTo(HaveOccurred())

					node, err := utils.GetNode(cs, strings.ToLower(nodeName))
					Expect(err).NotTo(HaveOccurred())

					providerID := node.Spec.ProviderID
					providerIDMatches := vmssVMProviderIDRE.FindStringSubmatch(providerID)
					Expect(len(providerIDMatches)).To(Equal(5))
					Expect(strings.EqualFold(providerIDMatches[1], subscriptionID)).To(BeTrue())
					Expect(strings.EqualFold(providerIDMatches[2], rgName)).To(BeTrue())
					Expect(strings.EqualFold(providerIDMatches[3], *vmss.Name)).To(BeTrue())
					Expect(strings.EqualFold(providerIDMatches[4], *vmssVM.InstanceID)).To(BeTrue())
				}
			}
		}
	})

	It("should set correct private IP address for every node", func() {
		utils.Logf("getting all NICs of availabilitySet VMs")
		vmasNICs, err := utils.ListNICs(tc, tc.GetResourceGroup())
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("getting all VMs managed by availabilitySet")
		vmasVMs, err := utils.ListVMs(tc)
		Expect(err).NotTo(HaveOccurred())

		for _, vmasVM := range *vmasVMs {
			nodeName, err := utils.GetVMComputerName(vmasVM)
			Expect(err).NotTo(HaveOccurred())

			node, err := utils.GetNode(cs, strings.ToLower(nodeName))
			Expect(err).NotTo(HaveOccurred())

			var privateIP string
			for _, address := range node.Status.Addresses {
				if address.Type == v1.NodeInternalIP {
					privateIP = address.Address
					break
				}
			}

			nicIDs, err := utils.GetNicIDsFromVM(vmasVM)
			Expect(err).NotTo(HaveOccurred())

			found := false

		Loop:
			for nicID := range nicIDs {
				nic, err := utils.GetNICByID(nicID, vmasNICs)
				Expect(err).NotTo(HaveOccurred())

				for _, ipConfig := range *nic.IPConfigurations {
					if strings.EqualFold(*ipConfig.PrivateIPAddress, privateIP) {
						found = true
						break Loop
					}
				}
			}
			Expect(found).To(BeTrue())
		}

		utils.Logf("getting all scale sets")
		vmsses, err := utils.ListVMSSes(tc)
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("getting all NICs of VMSSes")
		var vmssAllNics []network.Interface
		vmssVMs := make([]compute.VirtualMachineScaleSetVM, 0)
		for _, vmss := range vmsses {
			vmssVMList, err := utils.ListVMSSVMs(tc, *vmss.Name)
			Expect(err).NotTo(HaveOccurred())
			vmssVMs = append(vmssVMs, vmssVMList...)

			vmssNics, err := utils.ListVMSSNICs(tc, *vmss.Name)
			Expect(err).NotTo(HaveOccurred())

			vmssAllNics = append(vmssAllNics, *vmssNics...)
		}

		utils.Logf("getting all NICs of VMSS VMs")
		for _, vmssVM := range vmssVMs {
			utils.Logf("Checking %d VMSS VM %q", vmssVM.Name)
			nodeName, err := utils.GetVMSSVMComputerName(vmssVM)
			Expect(err).NotTo(HaveOccurred())

			node, err := utils.GetNode(cs, strings.ToLower(nodeName))
			Expect(err).NotTo(HaveOccurred())

			var privateIP string
			for _, address := range node.Status.Addresses {
				if address.Type == v1.NodeInternalIP {
					privateIP = address.Address
					break
				}
			}

			nicIDs, err := utils.GetNicIDsFromVMSSVM(vmssVM)
			Expect(err).NotTo(HaveOccurred())

			found := false

		VMSSLoop:
			for nicID := range nicIDs {
				nic, err := utils.GetNICByID(nicID, &vmssAllNics)
				Expect(err).NotTo(HaveOccurred())

				for _, ipConfig := range *nic.IPConfigurations {
					if strings.EqualFold(*ipConfig.PrivateIPAddress, privateIP) {
						found = true
						break VMSSLoop
					}
				}
			}
			Expect(found).To(BeTrue())
		}
	})

	It("should set route table correctly when the cluster is enabled by kubenet", Label(utils.TestSuiteLabelKubenet), func() {
		utils.Logf("getting route table")
		routeTables, err := utils.ListRouteTables(tc)
		Expect(err).NotTo(HaveOccurred())
		if len(*routeTables) == 0 {
			Skip("Skip because there's no routeTables, only test cluster with Kubenet network")
		}

		nodes, err := utils.GetAllNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		nodeSet := make(map[string]interface{})
		for _, node := range nodes {
			nodeSet[node.Name] = true
		}
		utils.Logf("nodeSet: %v", nodeSet)

		var succeeded bool
		for _, routeTable := range *routeTables {
			utils.Logf("getting all routes in route table %s", *routeTable.Name)
			routeSet, err := utils.GetNodesInRouteTable(routeTable)
			Expect(err).NotTo(HaveOccurred())

			utils.Logf("routeSet: %v", routeSet)

			if reflect.DeepEqual(nodeSet, routeSet) {
				succeeded = true
				break
			}
		}

		Expect(succeeded).To(BeTrue())
	})
})

var _ = Describe("Azure nodes", func() {
	basename := "azure-nodes"
	serviceName := "servicelb-test"

	var (
		cs clientset.Interface
		ns *v1.Namespace
		tc *utils.AzureTestClient
	)

	labels := map[string]string{
		"app": serviceName,
	}
	ports := []v1.ServicePort{{
		Port:       serverPort,
		TargetPort: intstr.FromInt(serverPort),
	}}

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		tc, err = utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating deployment " + serviceName)
		deployment := createServerDeploymentManifest(serviceName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
		tc = nil
	})

	It("should expose zones correctly after created", Label(utils.TestSuiteLabelVMSS, utils.TestSuiteLabelSerial, utils.TestSuiteLabelSlow), func() {
		utils.Logf("getting test VMSS")
		vmss, err := utils.FindTestVMSS(tc, tc.GetResourceGroup())
		Expect(err).NotTo(HaveOccurred())
		if vmss == nil {
			Skip("only test cluster with VMSS")
		}

		utils.Logf("scaling VMSS")
		count := *vmss.Sku.Capacity
		err = utils.ScaleVMSS(tc, *vmss.Name, tc.GetResourceGroup(), int64(vmssScaleUpCelling))

		defer func() {
			utils.Logf("restoring VMSS")
			err = utils.ScaleVMSS(tc, *vmss.Name, tc.GetResourceGroup(), count)
			Expect(err).NotTo(HaveOccurred())
		}()

		utils.Logf("validating zone label")
		err = utils.ValidateVMSSNodeLabels(tc, vmss, vmssVMZoneLabelKey)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should support crossing resource groups", Label(utils.TestSuiteLabelMultiGroup, utils.TestSuiteLabelAvailabilitySet), func() {
		if os.Getenv(utils.AKSTestCCM) != "" {
			Skip("aks cluster cannot obtain master node, skip the case")
		}
		master, err := utils.GetMaster(cs)
		Expect(err).NotTo(HaveOccurred())

		var rgMaster, rgNotMaster string
		rgMaster, err = utils.GetNodeResourceGroup(master)
		utils.Logf("found master resource group %s", rgMaster)
		Expect(err).NotTo(HaveOccurred())

		nodes, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		var nodeNotInRGMaster v1.Node
		var nodeNotInRGMAsterCount int
		for i, node := range nodes {
			if rg, err := utils.GetNodeResourceGroup(&nodes[i]); err == nil && rg != rgMaster {
				utils.Logf("rg of node %s is %s", node.Name, rg)
				nodeNotInRGMaster = node
				nodeNotInRGMAsterCount++
				rgNotMaster = rg
			} else if err != nil {
				Fail("cannot obtain the node's resource group")
			}
		}
		utils.Logf("found node %s in another resource group", nodeNotInRGMaster.Name)

		if nodeNotInRGMAsterCount == 0 {
			Skip("cannot find a second resource group, skip the case")
		}
		labels := nodeNotInRGMaster.Labels
		Expect(labels).NotTo(BeNil())
		excludeLB, ok := labels[`alpha.service-controller.kubernetes.io/exclude-balancer`]
		Expect(ok).To(BeTrue())
		Expect(excludeLB).To(Equal("true"))

		clusterRG, ok := labels[`kubernetes.azure.com/cluster`]
		Expect(ok).To(BeTrue())
		Expect(clusterRG).To(Equal(rgMaster))

		nodeRG, ok := labels[`kubernetes.azure.com/resource-group`]
		Expect(ok).To(BeTrue())
		Expect(nodeRG).NotTo(Equal(rgMaster))

		publicIP := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, map[string]string{}, ports)
		lb := getAzureLoadBalancerFromPIP(tc, publicIP, rgMaster, rgMaster)

		utils.Logf("finding NIC of the node %s, assuming it's in the same rg as master", nodeNotInRGMaster.Name)
		nodeNamePrefix, err := getNodeNamePrefix(nodeNotInRGMaster.Name)
		Expect(err).NotTo(HaveOccurred())
		NICList, err := utils.ListNICs(tc, rgMaster)
		Expect(err).NotTo(HaveOccurred())
		targetNIC, err := utils.GetTargetNICFromList(NICList, nodeNamePrefix)
		Expect(err).NotTo(HaveOccurred())
		if targetNIC == nil {
			utils.Logf("finding NIC of the node %s in another resource group", nodeNotInRGMaster.Name)
			NICList, err = utils.ListNICs(tc, rgNotMaster)
			Expect(err).NotTo(HaveOccurred())
			targetNIC, err = utils.GetTargetNICFromList(NICList, nodeNamePrefix)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(targetNIC).NotTo(BeNil())
		utils.Logf("found NIC %s of node %s", *targetNIC.Name, nodeNotInRGMaster.Name)

		for _, nicIPConfigs := range *targetNIC.IPConfigurations {
			if nicIPConfigs.LoadBalancerBackendAddressPools != nil {
				for _, nicBackendPool := range *nicIPConfigs.LoadBalancerBackendAddressPools {
					for _, lbBackendPool := range *lb.BackendAddressPools {
						Expect(*lbBackendPool.ID).NotTo(Equal(*nicBackendPool.ID))
					}
				}
			}
		}
	})
})

func getNodeNamePrefix(nodeName string) (string, error) {
	nodeNameMatches := vmNameRE.FindStringSubmatch(nodeName)
	if len(nodeNameMatches) != 2 {
		return "", fmt.Errorf("cannot obtain the prefix from given node name %s", nodeName)
	}
	return nodeNameMatches[1], nil
}
