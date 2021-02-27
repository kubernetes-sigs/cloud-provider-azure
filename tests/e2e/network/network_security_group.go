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
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2020-07-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	azureprovider "sigs.k8s.io/cloud-provider-azure/pkg/provider"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Network security group", func() {
	basename := "nsg"
	serviceName := "nsg-test"

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

	It("should add the rule when expose a service", func() {
		By("Creating a service and expose it")
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, map[string]string{}, ports)
		defer func() {
			By("Cleaning up")
			err := utils.DeleteServiceIfExists(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Validating ip exists in Security Group")
		port := fmt.Sprintf("%v", nginxPort)
		nsg, err := tc.GetClusterSecurityGroup()
		Expect(err).NotTo(HaveOccurred())
		Expect(validateUnsharedSecurityRuleExists(nsg, ip, port)).To(BeTrue(), "Security rule for service %s not exists", serviceName)

		By("Validating network security group working")
		var code int
		url := fmt.Sprintf("http://%s:%v", ip, ports[0].Port)
		for i := 1; i <= 30; i++ {
			utils.Logf("round %d, GET %s", i, url)
			/* #nosec G107: Potential HTTP request made with variable url */
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
			nsg, err := tc.GetClusterSecurityGroup()
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

	It("can set source IP prefixes automatically according to corresponding service tag", func() {
		By("Creating service and wait it to expose")
		annotation := map[string]string{
			azureprovider.ServiceAnnotationAllowedServiceTag: "AzureCloud",
		}
		_ = createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)

		By("Validating if the corresponding IP prefix existing in nsg")
		nsg, err := tc.GetClusterSecurityGroup()
		Expect(err).NotTo(HaveOccurred())

		rules := nsg.SecurityRules
		Expect(len(*rules)).NotTo(Equal(0))
		var found bool
		for _, rule := range *rules {
			if rule.SourceAddressPrefix != nil && strings.Contains(*rule.SourceAddressPrefix, "AzureCloud") {
				found = true
				break
			}
		}
		Expect(found).To(BeTrue())
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges`", func() {
		By("Creating a test service with the deny rule annotation but without `service.Spec.LoadBalancerSourceRanges`")
		annotation := map[string]string{
			azureprovider.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges: "true",
			azureprovider.ServiceAnnotationLoadBalancerInternal:                  "true",
		}
		service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the service to expose")
		internalIP, err := utils.WaitServiceExposure(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())

		By("Checking if there is a deny_all rule")
		nsg, err := tc.GetClusterSecurityGroup()
		Expect(err).NotTo(HaveOccurred())
		found := validateDenyAllSecurityRuleExists(nsg, internalIP)
		Expect(found).ToNot(BeTrue())

		By("Deleting the service")
		err = utils.DeleteService(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())

		By("Creating a test service with the deny rule annotation and `service.Spec.LoadBalancerSourceRanges`")
		service.Spec.LoadBalancerSourceRanges = []string{"1.2.3.4/32"}
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the service to expose")
		internalIP, err = utils.WaitServiceExposure(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())

		By("Checking if there is a LoadBalancerSourceRanges rule")
		nsg, err = tc.GetClusterSecurityGroup()
		Expect(err).NotTo(HaveOccurred())
		found = validateLoadBalancerSourceRangesRuleExists(nsg, internalIP, "1.2.3.4/32", "1.2.3.4_32")
		Expect(found).To(BeTrue())

		By("Checking if there is a deny_all rule")
		found = validateDenyAllSecurityRuleExists(nsg, internalIP)
		Expect(found).To(BeTrue())
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
				if !utils.StringInSlice(ip, *securityRule.DestinationAddressPrefixes) {
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

func validateLoadBalancerSourceRangesRuleExists(nsg *aznetwork.SecurityGroup, ip, sourceAddressPrefix, ipRangesSuffix string) bool {
	if nsg == nil || nsg.SecurityRules == nil {
		return false
	}
	for _, securityRule := range *nsg.SecurityRules {
		if securityRule.Access == aznetwork.SecurityRuleAccessAllow &&
			strings.EqualFold(to.String(securityRule.DestinationAddressPrefix), ip) &&
			strings.HasSuffix(to.String(securityRule.Name), ipRangesSuffix) &&
			strings.EqualFold(to.String(securityRule.SourceAddressPrefix), sourceAddressPrefix) {
			return true
		}
	}

	return false
}

func validateDenyAllSecurityRuleExists(nsg *aznetwork.SecurityGroup, ip string) bool {
	if nsg == nil || nsg.SecurityRules == nil {
		return false
	}
	for _, securityRule := range *nsg.SecurityRules {
		if securityRule.Access == aznetwork.SecurityRuleAccessDeny &&
			strings.EqualFold(to.String(securityRule.DestinationAddressPrefix), ip) &&
			strings.HasSuffix(to.String(securityRule.Name), "deny_all") &&
			strings.EqualFold(to.String(securityRule.SourceAddressPrefix), "*") {
			return true
		}
	}

	return false
}
