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

	azcompute "github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-07-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"

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
		err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
		tc = nil
	})

	It("should add all nodes in different agent pools to backends", Label(utils.TestSuiteLabelMultiNodePools), func() {
		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(network.PublicIPAddressSkuNameStandard)) {
			Skip("only test standard load balancer")
		}

		rgName := tc.GetResourceGroup()
		publicIP := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, map[string]string{}, ports)
		lb := getAzureLoadBalancerFromPIP(tc, publicIP, rgName, rgName)

		nodeList, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		if len(nodeList) < 2 {
			Skip("only support cluster with multiple agent pools")
		}

		ipcIDs := []string{}
		for _, backendAddressPool := range *lb.BackendAddressPools {
			if os.Getenv(utils.AKSTestCCM) != "" && *backendAddressPool.Name == "aksOutboundBackendPool" {
				continue
			}
			for _, ipc := range *backendAddressPool.BackendIPConfigurations {
				if ipc.ID != nil {
					ipcIDs = append(ipcIDs, *ipc.ID)
				}
			}
		}
		utils.Logf("got BackendIPConfigurations IDs: %v", ipcIDs)

		// Check if it is a cluster with VMSS
		vmsses, err := utils.ListVMSSes(tc)
		Expect(err).NotTo(HaveOccurred())
		if len(vmsses) != 0 {
			allVMs := []azcompute.VirtualMachineScaleSetVM{}
			for _, vmss := range vmsses {
				if strings.Contains(*vmss.ID, "control-plane") || strings.Contains(*vmss.ID, "master") {
					continue
				}
				vms, err := utils.ListVMSSVMs(tc, *vmss.Name)
				Expect(err).NotTo(HaveOccurred())
				allVMs = append(allVMs, vms...)
			}
			Expect(len(allVMs)).To(Equal(len(ipcIDs)))
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
			vms, err := utils.ListVMs(tc)
			Expect(err).NotTo(HaveOccurred())
			for _, vm := range *vms {
				if strings.Contains(*vm.ID, "control-plane") || strings.Contains(*vm.ID, "master") {
					continue
				}
				vmID := *vm.ID
				vmName := vmID[strings.LastIndex(vmID, "/")+1:]
				utils.Logf("Checking VM %q", vmName)

				nic := (*vm.NetworkProfile.NetworkInterfaces)[0].ID
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
		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(network.PublicIPAddressSkuNameStandard)) {
			Skip("only test standard load balancer")
		}

		rgName := tc.GetResourceGroup()
		publicIP := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, map[string]string{}, ports)
		lb := getAzureLoadBalancerFromPIP(tc, publicIP, rgName, rgName)

		Expect(lb.OutboundRules).NotTo(BeNil())
		var fipConfigIDs []string
		for _, outboundRule := range *lb.OutboundRules {
			Expect(outboundRule.FrontendIPConfigurations).NotTo(BeNil())
			for _, fipConfig := range *outboundRule.FrontendIPConfigurations {
				fipConfigIDs = append(fipConfigIDs, *fipConfig.ID)
			}
		}

		pips, err := tc.ListPublicIPs(tc.GetResourceGroup())
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pips)).NotTo(Equal(0))

		outboundRuleIPs := make(map[string]bool)
		for _, fipConfigID := range fipConfigIDs {
			for _, pip := range pips {
				if strings.EqualFold(*pip.IPConfiguration.ID, fipConfigID) {
					outboundRuleIPs[*pip.IPAddress] = true
					break
				}
			}
		}
		if len(outboundRuleIPs) == 0 {
			Skip("skip validating outbound IPs since outbound rules are not configured on SLB")
		}

		podTemplate := createPodGetIP()
		err = utils.CreatePod(cs, ns.Name, podTemplate)
		Expect(err).NotTo(HaveOccurred())

		podOutboundIP, err := utils.GetPodOutboundIP(cs, podTemplate, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		_, found := outboundRuleIPs[podOutboundIP]
		Expect(found).To(BeTrue())
	})
})

func createPodGetIP() *v1.Pod {
	podName := "test-pod"
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Hostname: podName,
			Containers: []v1.Container{
				{
					Name:            "test-app",
					Image:           "k8s.gcr.io/e2e-test-images/agnhost:2.36",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command: []string{
						"/bin/sh", "-c", "curl -s -m 5 --retry-delay 5 --retry 10 ifconfig.me",
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
}
