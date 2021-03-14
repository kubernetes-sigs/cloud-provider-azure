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
	"strings"

	"github.com/Azure/go-autorest/autorest/to"

	. "github.com/onsi/ginkgo"
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
		Port:       nginxPort,
		TargetPort: intstr.FromInt(nginxPort),
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
		deployment := createNginxDeploymentManifest(serviceName, labels)
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

	It("should add all nodes in different agent pools to backends [MultipleAgentPools]", func() {
		rgName := tc.GetResourceGroup()
		publicIP := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, map[string]string{}, ports)
		lb := getAzureLoadBalancerFromPIP(tc, publicIP, rgName, rgName)

		if !strings.EqualFold(string(lb.Sku.Name), "standard") {
			utils.Logf("sku: %s", lb.Sku.Name)
			Skip("only support standard load balancer")
		}

		nodeList, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		if len(nodeList) < 2 {
			Skip("only support cluster with multiple agent pools")
		}

		lbBackendAddressPoolsIDMap := make(map[string]bool)
		for _, backendAddressPool := range *lb.BackendAddressPools {
			utils.Logf("found backend pool %s", to.String(backendAddressPool.ID))
			lbBackendAddressPoolsIDMap[*backendAddressPool.ID] = true
		}

		NICList, err := utils.ListNICs(tc, rgName)
		Expect(err).NotTo(HaveOccurred())
		var found bool
		for _, nic := range *NICList {
			if strings.Split(*nic.Name, "-")[1] == "master" {
				continue
			}
			for _, ipConfig := range *nic.IPConfigurations {
				if found {
					break
				}
				utils.Logf("found ip config %s", to.String(ipConfig.Name))
				for _, backendAddressPool := range *ipConfig.LoadBalancerBackendAddressPools {
					utils.Logf("found backend pool on nic %s", to.String(backendAddressPool.ID))
					if lbBackendAddressPoolsIDMap[*backendAddressPool.ID] {
						found = true
						break
					}
				}
			}
			Expect(found).To(BeTrue())
		}
	})

	It("should make outbound IP of pod same as in SLB's outbound rules", func() {
		rgName := tc.GetResourceGroup()
		publicIP := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, map[string]string{}, ports)
		lb := getAzureLoadBalancerFromPIP(tc, publicIP, rgName, rgName)

		if !strings.EqualFold(string(lb.Sku.Name), "standard") {
			utils.Logf("sku: %s", lb.Sku.Name)
			Skip("only support standard load balancer")
		}

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
		Expect(len(outboundRuleIPs)).NotTo(Equal(0))

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
					Image:           "appropriate/curl",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command: []string{
						"/bin/sh",
						"-c",
						`curl -s -m 5 --retry-delay 5 --retry 10 ifconfig.me`,
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
}
