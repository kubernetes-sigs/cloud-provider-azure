/*
Copyright 2026 The Kubernetes Authors.

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

package node

import (
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	cloudproviderapi "k8s.io/cloud-provider/api"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Standalone VM check-in", Label(utils.TestSuiteLabelNode, utils.TestSuiteLabelStandaloneVM), func() {
	It("should initialize standalone VM nodes with the correct cloud metadata", SpecTimeout(5*time.Minute), func(ctx SpecContext) {
		if !strings.EqualFold(os.Getenv(utils.CAPZTestCCM), utils.TrueValue) {
			Skip("requires the CAPZ check-in fixture (TEST_CCM=true)")
		}
		clusterName := os.Getenv("CLUSTER_NAME")
		resourceGroup := os.Getenv("AZURE_RESOURCE_GROUP")
		Expect(clusterName).NotTo(BeEmpty(), "CAPZ must export CLUSTER_NAME")
		Expect(resourceGroup).NotTo(BeEmpty(), "CAPZ must export AZURE_RESOURCE_GROUP")
		cs, err := utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())
		tc, err := utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())
		// The CAPZ check-in templates put both control-plane and worker VMs
		// and their NICs in AZURE_RESOURCE_GROUP. Reject a different layout
		// instead of silently inspecting only an arbitrary worker's group.
		Expect(strings.ToLower(tc.GetResourceGroup())).To(Equal(strings.ToLower(resourceGroup)),
			"requires the CAPZ single-resource-group fixture")

		By("discovering standalone VMs in the cluster resource group")
		// CAPZ's ci-entrypoint.sh waits for provisioning and Node readiness
		// before invoking this suite. VM/NIC metadata should already exist;
		// fetch it once rather than issuing ARM calls on every Node poll.
		vms, err := utils.ListVMs(tc)
		Expect(err).NotTo(HaveOccurred())
		clusterTag := "sigs.k8s.io_cluster-api-provider-azure_cluster_" + clusterName
		clusterVMCount := 0
		var standaloneVMs []*armcompute.VirtualMachine
		for _, vm := range vms {
			Expect(vm).NotTo(BeNil())
			if ptr.Deref(vm.Tags[clusterTag], "") != "owned" {
				continue
			}
			clusterVMCount++
			Expect(vm.Properties).NotTo(BeNil(), "VM %s has no properties", ptr.Deref(vm.Name, ""))
			// VMSS Flex instances also use the virtualMachines API. Neither those
			// nor availability-set members exercise standalone VM support.
			if vm.Properties.AvailabilitySet == nil && vm.Properties.VirtualMachineScaleSet == nil {
				standaloneVMs = append(standaloneVMs, vm)
			}
		}
		Expect(clusterVMCount).To(BeNumerically(">", 0),
			"CAPZ fixture %s must have owned control-plane VMs in resource group %s", clusterName, resourceGroup)
		if len(standaloneVMs) == 0 {
			Skip("requires standalone VM nodes (without an availability set or a scale set)")
		}

		nics, err := utils.ListNICs(tc, resourceGroup)
		Expect(err).NotTo(HaveOccurred())
		// Discover from Azure, not registered Nodes, so a VM that never
		// checks in fails the test. All waits share the spec's deadline.
		for _, vm := range standaloneVMs {
			Expect(ptr.Deref(vm.ID, "")).NotTo(BeEmpty())
			Expect(vm.Properties.OSProfile).NotTo(BeNil())
			computerName := ptr.Deref(vm.Properties.OSProfile.ComputerName, "")
			Expect(computerName).NotTo(BeEmpty())
			// CAPZ's kubeadm templates register ds.meta_data["local_hostname"].
			nodeName := strings.ToLower(computerName)
			By(fmt.Sprintf("checking registration and cloud initialization of standalone VM %s (node %s)", *vm.ID, nodeName))

			Expect(vm.Properties.HardwareProfile).NotTo(BeNil())
			instanceType := string(ptr.Deref(vm.Properties.HardwareProfile.VMSize, ""))
			Expect(instanceType).NotTo(BeEmpty())
			region := strings.ToLower(ptr.Deref(vm.Location, ""))
			Expect(region).NotTo(BeEmpty())
			zone := ""
			if len(vm.Zones) > 0 {
				Expect(ptr.Deref(vm.Zones[0], "")).NotTo(BeEmpty())
				zone = region + "-" + *vm.Zones[0]
			}

			Expect(vm.Properties.NetworkProfile).NotTo(BeNil())
			interfaces := vm.Properties.NetworkProfile.NetworkInterfaces
			Expect(interfaces).NotTo(BeEmpty())
			privateIPs := make(map[netip.Addr]bool)
			primaryNICCount := 0
			for _, ref := range interfaces {
				Expect(ref).NotTo(BeNil())
				if len(interfaces) > 1 && (ref.Properties == nil || !ptr.Deref(ref.Properties.Primary, false)) {
					continue
				}
				primaryNICCount++
				Expect(ptr.Deref(ref.ID, "")).NotTo(BeEmpty())
				nic, err := utils.GetNICByID(*ref.ID, nics)
				Expect(err).NotTo(HaveOccurred())
				Expect(nic.Properties).NotTo(BeNil())
				for _, ipConfig := range nic.Properties.IPConfigurations {
					Expect(ipConfig).NotTo(BeNil())
					Expect(ipConfig.Properties).NotTo(BeNil())
					ip, err := netip.ParseAddr(ptr.Deref(ipConfig.Properties.PrivateIPAddress, ""))
					Expect(err).NotTo(HaveOccurred())
					privateIPs[ip.Unmap()] = true
				}
			}
			Expect(primaryNICCount).To(Equal(1), "VM %s must identify exactly one primary NIC", *vm.ID)
			Expect(privateIPs).NotTo(BeEmpty(), "VM %s must have private addresses on its primary NIC", *vm.ID)

			Eventually(func(g Gomega) {
				node, err := cs.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "getting Node %s for VM %s", nodeName, *vm.ID)
				g.Expect(node.DeletionTimestamp).To(BeNil())
				g.Expect(strings.ToLower(node.Spec.ProviderID)).To(Equal(strings.ToLower("azure://" + *vm.ID)))
				g.Expect(node.Labels).To(HaveKeyWithValue(v1.LabelInstanceTypeStable, instanceType))
				g.Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyRegion, region))
				if zone != "" {
					g.Expect(node.Labels).To(HaveKeyWithValue(v1.LabelTopologyZone, zone))
				} else {
					// Regional VMs use the provider's fault-domain fallback.
					g.Expect(node.Labels[v1.LabelTopologyZone]).NotTo(BeEmpty())
				}
				for _, taint := range node.Spec.Taints {
					g.Expect(taint.Key).NotTo(Equal(cloudproviderapi.TaintExternalCloudProvider))
				}
				ready := false
				for _, condition := range node.Status.Conditions {
					if condition.Type == v1.NodeReady {
						ready = condition.Status == v1.ConditionTrue
					}
				}
				g.Expect(ready).To(BeTrue(), "node conditions: %+v", node.Status.Conditions)
				g.Expect(node.Status.Addresses).To(ContainElement(v1.NodeAddress{Type: v1.NodeHostName, Address: nodeName}))
				hasInternalIP := false
				for _, address := range node.Status.Addresses {
					if address.Type != v1.NodeInternalIP {
						continue
					}
					ip, err := netip.ParseAddr(address.Address)
					g.Expect(err).NotTo(HaveOccurred())
					ip = ip.Unmap()
					g.Expect(privateIPs).To(HaveKey(ip), "node internal IP must belong to VM %s's primary NIC", *vm.ID)
					hasInternalIP = true
				}
				g.Expect(hasInternalIP).To(BeTrue(), "node must report an internal IP")
			}).WithContext(ctx).WithPolling(5*time.Second).Should(Succeed(),
				"standalone VM %s (Node %s) failed to check in", *vm.ID, nodeName)
		}
	})
})
