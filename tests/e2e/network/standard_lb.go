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
	"strings"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("[StandardLoadBalancer] Standard load balancer", func() {
	basename := "service-lb"
	serviceName := "servicelb-test"

	var cs clientset.Interface
	var ns *v1.Namespace
	var tc *utils.AzureTestClient

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
		_, err = cs.AppsV1().Deployments(ns.Name).Create(deployment)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		err := cs.AppsV1().Deployments(ns.Name).Delete(serviceName, nil)
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
	})

	It("should add all nodes in different agent pools to backends [MultipleAgentPools]", func() {
		rgName := tc.GetResourceGroup()
		publicIP := createServiceWithAnnotation(cs, serviceName, ns.Name, labels, map[string]string{}, ports)
		lb := getAzureLoadBalancerFromPIP(publicIP, rgName, rgName)

		if !strings.EqualFold(string(lb.Sku.Name), "standard") {
			Skip("only support standard load balancer")
		}

		nodeList, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		if len(nodeList) < 2 {
			Skip("only support cluster with multiple agent pools")
		}

		lbBackendAddressPoolsIDMap := make(map[string]bool)
		for _, backendAddressPool := range *lb.BackendAddressPools {
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
				for _, backendAddressPool := range *ipConfig.LoadBalancerBackendAddressPools {
					if lbBackendAddressPoolsIDMap[*backendAddressPool.ID] {
						found = true
					}
				}
			}
			Expect(found).To(BeTrue())
		}
	})
})
