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
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Cloud Provider Azure", func() {
	basename := "service-lb"
	serviceName := "servicelb-test"

	var cs clientset.Interface
	var ns *v1.Namespace
	//var tc *utils.AzureTestClient

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

		//tc, err = utils.CreateAzureTestClient()
		//Expect(err).NotTo(HaveOccurred())

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
		//tc = nil
	})

	It("should support nodes crossing resource groups [multi-group] [availabilitySet]", func() {
		master, err := utils.GetMaster(cs)
		Expect(err).NotTo(HaveOccurred())

		var rgMaster string
		rgMaster, err = utils.GetNodeResourceGroup(master)
		utils.Logf("found master resource group %s", rgMaster)
		Expect(err).NotTo(HaveOccurred())

		nodes, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		var nodeNotInRGMaster v1.Node
		var nodeNotInRGMAsterCount int
		for _, node := range nodes {
			if rg, err := utils.GetNodeResourceGroup(&node); err == nil && rg != rgMaster {
				utils.Logf("rg of node %s is %s", node.Name, rg)
				nodeNotInRGMaster = node
				nodeNotInRGMAsterCount++
			} else if err != nil {
				Fail("cannot obtain the node's resource group")
			}
		}
		utils.Logf("found node %s in another resource group", nodeNotInRGMaster.Name)

		if nodeNotInRGMAsterCount == 0 {
			Skip("cannot find a second resource group, skipping")
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

		publicIP := createServiceWithAnnotation(cs, serviceName, ns.Name, labels, map[string]string{}, ports)
		lb := getAzureLoadBalancerFromPIP(publicIP, rgMaster, rgMaster)

		for _, backendPool := range *lb.BackendAddressPools {
			if backendPool.BackendIPConfigurations != nil {
				if *backendPool.BackendIPConfigurations != nil {
					for _, ipConfig := range *backendPool.BackendIPConfigurations {
						matches := backendASIPConfigurationRE.FindStringSubmatch(*ipConfig.ID)
						Expect(len(matches)).To(Equal(2))
						Expect(matches[1]).NotTo(Equal(nodeNotInRGMaster.Name))
					}
				}
			}
		}
	})
})
