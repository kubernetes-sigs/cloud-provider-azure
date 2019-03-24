/*
Copyright 2018 The Kubernetes Authors.

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
	"fmt"
	"net/http"
	"strings"
	"time"

	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2017-09-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/cloud-provider-azure/tests/e2e/utils"
	"k8s.io/kubernetes/pkg/cloudprovider/providers/azure"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Network security group", func() {
	basename := "nsg"
	serviceName := "nsg-test"

	var cs clientset.Interface
	var ns *v1.Namespace
	var azureTestClient *utils.AzureTestClient

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

		azureTestClient, err = utils.CreateAzureTestClient()
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
		azureTestClient = nil
	})

	It("should add the rule when expose a service", func() {
		By("Creating a service and expose it")
		ip, err := createAndWaitServiceExposure(cs, ns.Name, serviceName, map[string]string{}, labels, ports)
		defer func() {
			By("Cleaning up")
			err = utils.DeleteServiceIfExists(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		By("Validating ip exists in Security Group")
		port := fmt.Sprintf("%v", nginxPort)
		nsg, err := azureTestClient.GetClusterSecurityGroup()
		Expect(err).NotTo(HaveOccurred())
		Expect(validateUnsharedSecurityRuleExists(nsg, ip, port)).To(BeTrue(), "Security rule for service %s not exists", serviceName)

		By("Validating network security group working")
		var code int
		url := fmt.Sprintf("http://%s:%v", ip, ports[0].Port)
		for i := 1; i <= 30; i++ {
			resp, err := http.Get(url)
			if err == nil {
				defer func() {
					if resp != nil {
						resp.Body.Close()
					}
				}()
				code = resp.StatusCode
				if resp.StatusCode == nginxStatusCode {
					break
				}
			}
			time.Sleep(20 * time.Second)
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(nginxStatusCode), "Fail to get response from the domain name")

		By("Validate automatically delete the rule, when service is deleted")
		Expect(utils.DeleteService(cs, ns.Name, serviceName)).NotTo(HaveOccurred())
		isDeleted := false
		for i := 1; i <= 30; i++ {
			nsg, err := azureTestClient.GetClusterSecurityGroup()
			Expect(err).NotTo(HaveOccurred())
			if !validateUnsharedSecurityRuleExists(nsg, ip, port) {
				utils.Logf("Target rule successfully deleted")
				isDeleted = true
				break
			}
			time.Sleep(20 * time.Second)
		}
		Expect(isDeleted).To(BeTrue(), "Fail to automatically delete the rule")
	})

	It("can share a rule for multiple services", func() {
		By("Exposing two services with shared security rule")
		annotation := map[string]string{
			azure.ServiceAnnotationSharedSecurityRule: "true",
		}
		ip1, err := createAndWaitServiceExposure(cs, ns.Name, serviceName, annotation, labels, ports)

		defer func() {
			err = utils.DeleteServiceIfExists(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		serviceName2 := serviceName + "-share"
		ip2, err := createAndWaitServiceExposure(cs, ns.Name, serviceName2, annotation, labels, ports)
		defer func() {
			By("Cleaning up")
			err = utils.DeleteServiceIfExists(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		By("Validate shared security rule exists")
		port := fmt.Sprintf("%v", nginxPort)
		nsg, err := azureTestClient.GetClusterSecurityGroup()
		Expect(err).NotTo(HaveOccurred())

		ipList := []string{ip1, ip2}
		Expect(validateSharedSecurityRuleExists(nsg, ipList, port)).To(BeTrue(), "Security rule for service %s not exists", serviceName)
	})
})

func validateUnsharedSecurityRuleExists(nsg *aznetwork.SecurityGroup, ip string, port string) bool {
	if nsg == nil || nsg.SecurityRules == nil {
		return false
	}
	for _, securityRule := range *nsg.SecurityRules {
		if strings.EqualFold(to.String(securityRule.DestinationAddressPrefix), ip) && strings.EqualFold(to.String(securityRule.DestinationPortRange), port) {
			utils.Logf("Find target security rule")
			return true
		}
	}
	return false
}

func validateSharedSecurityRuleExists(nsg *aznetwork.SecurityGroup, ips []string, port string) bool {
	if nsg == nil || nsg.SecurityRules == nil {
		return false
	}
	for _, securityRule := range *nsg.SecurityRules {
		if strings.EqualFold(to.String(securityRule.DestinationPortRange), port) {
			isFind := true
			for _, ip := range ips {
				if !stringInListPtr(securityRule.DestinationAddressPrefixes, ip) {
					utils.Logf("Find target security rule")
					isFind = false
					break
				}
			}
			if isFind {
				return true
			}
		}
	}
	return false
}

func createAndWaitServiceExposure(cs clientset.Interface, ns string, serviceName string, annotation map[string]string, labels map[string]string, ports []v1.ServicePort) (string, error) {
	ip := ""
	service := createLoadBalancerServiceManifest(cs, serviceName, annotation, labels, ns, ports)
	if _, err := cs.CoreV1().Services(ns).Create(service); err != nil {
		return ip, err
	}
	utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns)
	utils.Logf("waiting for service %s to be exposured", serviceName)
	ip, err := utils.WaitServiceExposure(cs, ns, serviceName)
	return ip, err
}

// stringInListPtr check if string in a list
func stringInListPtr(list *[]string, s string) bool {
	if list == nil {
		return false
	}
	for _, item := range *list {
		if strings.EqualFold(item, s) {
			return true
		}
	}
	return false
}
