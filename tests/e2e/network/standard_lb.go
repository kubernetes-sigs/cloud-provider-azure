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
	"os"
	"strings"

	azcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v6"
	network "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("[StandardLoadBalancer] Standard load balancer", func() {
	basename := "service-lb"
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

		utils.Logf("Waiting for backend pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())
		}

		cs = nil
		ns = nil
		tc = nil
	})

	It("should add all nodes in different agent pools to backends", Label(utils.TestSuiteLabelMultiNodePools), Label(utils.TestSuiteLabelNonMultiSLB), func() {
		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(network.LoadBalancerSKUNameStandard)) {
			Skip("only test standard load balancer")
		}

		rgName := tc.GetResourceGroup()
		publicIPs := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, map[string]string{}, ports)
		Expect(len(publicIPs)).NotTo(BeZero())
		publicIP := publicIPs[0]
		lb := getAzureLoadBalancerFromPIP(tc, publicIP, rgName, rgName)

		nodeList, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		if len(nodeList) < 2 {
			Skip("only support cluster with multiple agent pools")
		}

		// Check if it is a cluster with VMSS
		vmsses, err := utils.ListUniformVMSSes(tc)
		Expect(err).NotTo(HaveOccurred())
		isVMSS := len(vmsses) != 0

		ipcIDs := []string{}
		for i := range lb.Properties.BackendAddressPools {
			backendAddressPool := (lb.Properties.BackendAddressPools)[i]
			if (os.Getenv(utils.AKSTestCCM) != "" && *backendAddressPool.Name == "aksOutboundBackendPool") ||
				strings.Contains(*backendAddressPool.Name, "outboundBackendPool") {
				continue
			}
			if backendAddressPool.Properties.BackendIPConfigurations == nil {
				utils.Logf("BackendIPConfigurations is nil for backendAddressPool %q", *backendAddressPool.Name)
				continue
			}
			for j := range backendAddressPool.Properties.BackendIPConfigurations {
				ipc := (backendAddressPool.Properties.BackendIPConfigurations)[j]
				if ipc.ID != nil {
					if utils.IsAutoscalingAKSCluster() && strings.Contains(*ipc.ID, utils.SystemPool) {
						continue
					}
					if isVMSS && tc.IPFamily == utils.IPv6 && strings.Contains(*ipc.ID, "ipConfigurations/ipConfig0") {
						// For IPv6 VMSS, there'll be IPv4 IP configurations as well
						// e.g.
						// virtualMachines/0/networkInterfaces/<vmss-0>-nic-0/ipConfigurations/ipConfig0
						// virtualMachines/0/networkInterfaces/<vmss-0>-nic-0/ipConfigurations/ipConfigv6
						continue
					}
					ipcIDs = append(ipcIDs, *ipc.ID)
				}
			}
		}
		utils.Logf("got BackendIPConfigurations IDs: %q", ipcIDs)

		if isVMSS {
			allVMs := []*azcompute.VirtualMachineScaleSetVM{}
			for _, vmss := range vmsses {
				if strings.Contains(*vmss.ID, "control-plane") || strings.Contains(*vmss.ID, "master") {
					continue
				}
				vms, err := utils.ListVMSSVMs(tc, *vmss.Name)
				Expect(err).NotTo(HaveOccurred())
				allVMs = append(allVMs, vms...)
			}
			if tc.IPFamily == utils.DualStack {
				Expect(len(allVMs) * 2).To(Equal(len(ipcIDs)))
			} else {
				Expect(len(allVMs)).To(Equal(len(ipcIDs)))
			}
			for _, vm := range allVMs {
				utils.Logf("Checking VM %q", *vm.ID)
				found := false
				for _, ipcID := range ipcIDs {
					if strings.Contains(strings.ToLower(ipcID), strings.ToLower(*vm.ID)) {
						found = true
						break
					}
				}
				Expect(found).To(Equal(true))
			}
			utils.Logf("Validation succeeded for a VMSS cluster")
		} else {
			// AvSet VMs, standalone VMs, VMSS Flex VMs
			vms, err := utils.ListVMs(tc)
			Expect(err).NotTo(HaveOccurred())
			for _, vm := range vms {
				if strings.Contains(*vm.ID, "control-plane") || strings.Contains(*vm.ID, "master") {
					continue
				}
				vmID := *vm.ID
				vmName := vmID[strings.LastIndex(vmID, "/")+1:]
				utils.Logf("Checking VM %q", vmName)

				nic := vm.Properties.NetworkProfile.NetworkInterfaces[0].ID
				found := false
				for _, ipcID := range ipcIDs {
					if strings.Contains(strings.ToLower(ipcID), strings.ToLower(*nic)) {
						found = true
						break
					}
				}
				Expect(found).To(Equal(true))
			}
			utils.Logf("Validation succeeded for a non-VMSS cluster")
		}
	})

	It("should make outbound IP of pod same as in SLB's outbound rules", Label(utils.TestSuiteLabelSLBOutbound), func() {
		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(network.LoadBalancerSKUNameStandard)) {
			Skip("only test standard load balancer")
		}

		rgName := tc.GetResourceGroup()
		publicIPs := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, map[string]string{}, ports)
		Expect(len(publicIPs)).NotTo(BeZero())
		publicIP := publicIPs[0]
		lb := getAzureLoadBalancerFromPIP(tc, publicIP, rgName, rgName)

		Expect(lb.Properties.OutboundRules).NotTo(BeNil())
		var fipConfigIDs []string
		for _, outboundRule := range lb.Properties.OutboundRules {
			Expect(outboundRule.Properties.FrontendIPConfigurations).NotTo(BeNil())
			for _, fipConfig := range outboundRule.Properties.FrontendIPConfigurations {
				fipConfigIDs = append(fipConfigIDs, *fipConfig.ID)
			}
		}

		pips, err := tc.ListPublicIPs(tc.GetResourceGroup())
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pips)).NotTo(Equal(0))

		outboundRuleIPs := make(map[string]bool)
		for _, fipConfigID := range fipConfigIDs {
			for _, pip := range pips {
				if strings.EqualFold(*pip.Properties.IPConfiguration.ID, fipConfigID) {
					outboundRuleIPs[*pip.Properties.IPAddress] = true
					break
				}
			}
		}
		if len(outboundRuleIPs) == 0 {
			Skip("skip validating outbound IPs since outbound rules are not configured on SLB")
		}

		podTemplate := utils.CreatePodGetIPManifest()
		err = utils.CreatePod(cs, ns.Name, podTemplate)
		Expect(err).NotTo(HaveOccurred())

		podOutboundIP, err := utils.GetPodOutboundIP(cs, podTemplate, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		_, found := outboundRuleIPs[podOutboundIP]
		Expect(found).To(BeTrue())
	})
})
