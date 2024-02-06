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
	"net"
	"strings"
	"time"

	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"
	"k8s.io/utils/strings/slices"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Network security group", Label(utils.TestSuiteLabelNSG), func() {
	basename := "nsg"
	serviceName := "nsg-test"

	var cs clientset.Interface
	var ns *v1.Namespace
	var tc *utils.AzureTestClient

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

	It("should add the rule when expose a service", func() {
		By("Creating a service and expose it")
		ips := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, map[string]string{}, ports)
		defer func() {
			By("Cleaning up")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(len(ips)).NotTo(BeZero())
		ip := ips[0]

		By("Validating ip exists in Security Group")
		port := fmt.Sprintf("%d", serverPort)
		nsgs, err := tc.GetClusterSecurityGroups()
		Expect(err).NotTo(HaveOccurred())
		Expect(validateUnsharedSecurityRuleExists(nsgs, ip, port)).To(BeTrue(), "Security rule for service %s not exists", serviceName)

		By("Validating network security group working")
		// Use a hostNetwork Pod to validate Service connectivity via the cluster Node's
		// network because the current VM running go test may not support IPv6.
		agnhostPod := fmt.Sprintf("%s-%s", utils.ExecAgnhostPod, "nsg")
		result, err := utils.CreateHostExecPod(cs, ns.Name, agnhostPod)
		defer func() {
			err = utils.DeletePod(cs, ns.Name, agnhostPod)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(result).To(BeTrue())
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Validating External domain name %q", ip))
		err = utils.ValidateServiceConnectivity(ns.Name, agnhostPod, ip, int(ports[0].Port), v1.ProtocolTCP)
		Expect(err).NotTo(HaveOccurred(), "Fail to get response from the domain name")

		By("Validate automatically delete the rule, when service is deleted")
		Expect(utils.DeleteService(cs, ns.Name, serviceName)).NotTo(HaveOccurred())
		err = wait.PollImmediate(20*time.Second, 10*time.Minute, func() (done bool, err error) {
			nsgs, err := tc.GetClusterSecurityGroups()
			if err != nil {
				return false, err
			}
			if !validateUnsharedSecurityRuleExists(nsgs, ip, port) {
				utils.Logf("Target rule successfully deleted")
				return true, nil
			}
			return false, nil
		})
		Expect(err).To(BeNil(), "Fail to automatically delete the rule")
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-shared-securityrule`", func(ctx SpecContext) {
		By("Exposing two services with shared security rule")
		annotation := map[string]string{
			consts.ServiceAnnotationSharedSecurityRule: "true",
		}
		ips1 := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, annotation, ports)

		defer func() {
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		serviceName2 := serviceName + "-share"
		ips2 := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName2, ns.Name, labels, annotation, ports)
		defer func() {
			By("Cleaning up")
			err := utils.DeleteService(cs, ns.Name, serviceName2)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Validate shared security rule exists")
		port := fmt.Sprintf("%d", serverPort)
		nsgs, err := tc.GetClusterSecurityGroups()
		Expect(err).NotTo(HaveOccurred())

		ipList := append(ips1, ips2...)
		Expect(validateSharedSecurityRuleExists(nsgs, ipList, port)).To(BeTrue(), "Security rule for service %s not exists", serviceName)

		By("Validate automatically adjust or delete the rule, when service is deleted")
		Expect(utils.DeleteService(cs, ns.Name, serviceName)).NotTo(HaveOccurred())
		ipList = ips2
		Expect(validateSharedSecurityRuleExists(nsgs, ipList, port)).To(BeTrue(), "Security rule should be modified to only contain service %s", serviceName2)

		Expect(utils.DeleteService(cs, ns.Name, serviceName2)).NotTo(HaveOccurred())
		Eventually(func() (bool, error) {
			nsgs, err := tc.GetClusterSecurityGroups()
			if err != nil {
				return false, err
			}
			return validateSharedSecurityRuleExists(nsgs, ipList, port), nil
		}).WithContext(ctx).Should(BeFalse(), "Fail to automatically delete the shared security rule")
	})

	It("can set source IP prefixes automatically according to corresponding service tag", func() {
		By("Creating service and wait it to expose")
		annotation := map[string]string{
			consts.ServiceAnnotationAllowedServiceTags: "AzureCloud",
		}
		utils.Logf("Creating service " + serviceName + " in namespace " + ns.Name)
		service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns.Name)

		By("Waiting for the service to be exposed")
		_, err = utils.WaitServiceExposure(cs, ns.Name, serviceName, []string{})
		Expect(err).NotTo(HaveOccurred())

		By("Validating if the corresponding IP prefix existing in nsg")
		nsgs, err := tc.GetClusterSecurityGroups()
		Expect(err).NotTo(HaveOccurred())

		var found bool
		for _, nsg := range nsgs {
			rules := nsg.SecurityRules
			if rules == nil {
				continue
			}
			for _, rule := range *rules {
				if rule.SourceAddressPrefix != nil && strings.Contains(*rule.SourceAddressPrefix, "AzureCloud") {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		Expect(found).To(BeTrue())
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-deny-all-except-load-balancer-source-ranges`", func() {
		By("Creating a test service with the deny rule annotation but without `service.Spec.LoadBalancerSourceRanges`")
		annotation := map[string]string{
			consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges: "true",
			consts.ServiceAnnotationLoadBalancerInternal:                  "true",
		}
		service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the service to expose")
		internalIPs, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, []string{})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(internalIPs)).NotTo(BeZero())
		internalIP := internalIPs[0]

		By("Checking if there is a deny_all rule")
		nsgs, err := tc.GetClusterSecurityGroups()
		Expect(err).NotTo(HaveOccurred())
		found := validateDenyAllSecurityRuleExists(nsgs, internalIP)
		Expect(found).To(BeFalse())

		By("Deleting the service")
		err = utils.DeleteService(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())

		By("Creating a test service with the deny rule annotation and `service.Spec.LoadBalancerSourceRanges`")
		// Create a host exec Pod which lives a bit longer
		agnhostPod := fmt.Sprintf("%s-%s", utils.ExecAgnhostPod, "deny-all-except-lb-range")
		result, err := utils.CreateHostExecPod(cs, ns.Name, agnhostPod)
		defer func() {
			err = utils.DeletePod(cs, ns.Name, agnhostPod)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(result).To(BeTrue())
		Expect(err).NotTo(HaveOccurred())
		hostExecPod, err := utils.GetPod(cs, ns.Name, agnhostPod)
		Expect(err).NotTo(HaveOccurred())
		hostExecPodIP := hostExecPod.Status.PodIP
		mask := 32
		// For IPv6
		if tc.IPFamily == utils.IPv6 {
			for _, ip := range hostExecPod.Status.PodIPs {
				if net.ParseIP(ip.IP).To4() == nil {
					hostExecPodIP = ip.IP
					break
				}
			}
			mask = 128
		}

		allowCIDR := fmt.Sprintf("%s/%d", hostExecPodIP, mask)
		service.Spec.LoadBalancerSourceRanges = []string{allowCIDR}
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Check Service connectivity with the deny-all-except-lb-range ExecAgnhostPod
		By("Waiting for the service to expose")
		internalIPs, err = utils.WaitServiceExposureAndGetIPs(cs, ns.Name, serviceName)
		Expect(len(internalIPs)).NotTo(BeZero())
		internalIP = internalIPs[0]
		for _, port := range service.Spec.Ports {
			utils.Logf("checking the connectivity of addr %s:%d with protocol %v", internalIP, int(port.Port), port.Protocol)
			err := utils.ValidateServiceConnectivity(ns.Name, agnhostPod, internalIP, int(port.Port), port.Protocol)
			Expect(err).NotTo(HaveOccurred())
		}

		By("Checking if there is a LoadBalancerSourceRanges rule")
		nsgs, err = tc.GetClusterSecurityGroups()
		Expect(err).NotTo(HaveOccurred())
		ipRangesSuffix := strings.Replace(fmt.Sprintf("%s_%d", hostExecPodIP, mask), ":", ".", -1) // Handled in pkg/provider/getSecurityRuleName()
		found = validateLoadBalancerSourceRangesRuleExists(nsgs, internalIP, allowCIDR, ipRangesSuffix)
		Expect(found).To(BeTrue())

		By("Checking if there is a deny_all rule")
		found = validateDenyAllSecurityRuleExists(nsgs, internalIP)
		Expect(found).To(BeTrue())
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip`", func() {
		By("Creating a public IP with tags")
		ipName := basename + "-public-IP-disable-floating-ip"
		pip := defaultPublicIPAddress(ipName, tc.IPFamily == utils.IPv6)
		pip, err := utils.WaitCreatePIP(tc, ipName, tc.GetResourceGroup(), pip)
		Expect(err).NotTo(HaveOccurred())
		targetIP := pointer.StringDeref(pip.IPAddress, "")
		utils.Logf("created pip with address %s", targetIP)

		By("Creating a test load balancer service with floating IP disabled")
		annotation := map[string]string{
			consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true",
		}
		service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
		service = updateServiceLBIPs(service, false, []string{targetIP})
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, []string{targetIP})
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			By("cleaning up")
			err = utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeletePIPWithRetry(tc, ipName, "")
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Checking if the LoadBalancer's public IP is included in the network security rule's DestinationAddressPrefixes")
		nsgs, err := tc.GetClusterSecurityGroups()
		Expect(err).NotTo(HaveOccurred())
		for _, nsg := range nsgs {
			if nsg.SecurityRules == nil {
				continue
			}

			for _, securityRules := range *nsg.SecurityRules {
				contains := slices.Contains(*securityRules.DestinationAddressPrefixes, targetIP)
				Expect(contains).To(BeFalse())
			}
		}
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-allowed-ip-ranges`", func() {

		// isSuperSet returns true if set1 is a superset of set2.
		isSuperSet := func(set1, set2 []string) bool {
			s1 := map[string]bool{}
			for _, s := range set1 {
				s1[s] = true
			}
			for _, s := range set2 {
				if !s1[s] {
					return false
				}
			}
			return true
		}

		var (
			allowedIPRanges []string
			ipFamilies      []v1.IPFamily
		)
		_, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		if v6Enabled {
			allowedIPRanges = []string{
				"2c0f:fe40:8000::/48",
				"2c0f:feb0::/43",
			}
			ipFamilies = []v1.IPFamily{v1.IPv6Protocol}
		} else {
			allowedIPRanges = []string{
				"10.20.0.0/16",
				"192.168.0.1/32",
			}
			ipFamilies = []v1.IPFamily{v1.IPv4Protocol}
		}

		ipFamilyPolicy := v1.IPFamilyPolicySingleStack
		svc := v1.Service{
			Spec: v1.ServiceSpec{
				Ports:          ports,
				Type:           v1.ServiceTypeLoadBalancer,
				Selector:       labels,
				IPFamilies:     ipFamilies,
				IPFamilyPolicy: &ipFamilyPolicy,
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
				Annotations: map[string]string{
					consts.ServiceAnnotationAllowedIPRanges: strings.Join(allowedIPRanges, ","),
				},
				Labels: labels,
			},
		}

		By("Creating load balancer service with allowed IP ranges")
		_, err := cs.CoreV1().Services(ns.Name).Create(context.Background(), &svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the service to be exposed")
		_, err = utils.WaitServiceExposure(cs, ns.Name, serviceName, nil)
		Expect(err).NotTo(HaveOccurred())

		By("Validating if the corresponding IP prefix existing in nsg")
		nsgs, err := tc.GetClusterSecurityGroups()
		Expect(err).NotTo(HaveOccurred())

		var sources []string

		for _, nsg := range nsgs {

			rules := nsg.SecurityRules
			if rules == nil {
				continue
			}
			for _, rule := range *rules {
				if rule.SourceAddressPrefix != nil {
					sources = append(sources, *rule.SourceAddressPrefix)
				}
			}
		}
		Expect(isSuperSet(sources, allowedIPRanges)).To(BeTrue(), "Expected %v to be a superset of %v", sources, allowedIPRanges)
	})
})

func validateUnsharedSecurityRuleExists(nsgs []aznetwork.SecurityGroup, ip string, port string) bool {
	for _, nsg := range nsgs {
		if nsg.SecurityRules == nil {
			continue
		}
		for _, securityRule := range *nsg.SecurityRules {
			utils.Logf("Checking security rule %q", pointer.StringDeref(securityRule.Name, ""))
			if strings.EqualFold(pointer.StringDeref(securityRule.DestinationAddressPrefix, ""), ip) && strings.EqualFold(pointer.StringDeref(securityRule.DestinationPortRange, ""), port) {
				utils.Logf("Found target security rule")
				return true
			}
		}
	}
	return false
}

func validateSharedSecurityRuleExists(nsgs []aznetwork.SecurityGroup, ips []string, port string) bool {
	for _, nsg := range nsgs {
		if nsg.SecurityRules == nil {
			continue
		}
		for _, securityRule := range *nsg.SecurityRules {
			utils.Logf("Checking security rule %q", pointer.StringDeref(securityRule.Name, ""))
			if strings.EqualFold(pointer.StringDeref(securityRule.DestinationPortRange, ""), port) {
				found := true
				for _, ip := range ips {
					if !utils.StringInSlice(ip, *securityRule.DestinationAddressPrefixes) {
						found = false
						break
					}
				}
				if found {
					utils.Logf("Found target security rule")
					return true
				}
			}
		}
	}
	return false
}

func validateLoadBalancerSourceRangesRuleExists(nsgs []aznetwork.SecurityGroup, ip, sourceAddressPrefix, ipRangesSuffix string) bool {
	for _, nsg := range nsgs {
		if nsg.SecurityRules == nil {
			continue
		}
		for _, securityRule := range *nsg.SecurityRules {
			utils.Logf("Checking security rule %q", pointer.StringDeref(securityRule.Name, ""))
			if securityRule.Access == aznetwork.SecurityRuleAccessAllow &&
				strings.EqualFold(pointer.StringDeref(securityRule.DestinationAddressPrefix, ""), ip) &&
				strings.HasSuffix(pointer.StringDeref(securityRule.Name, ""), ipRangesSuffix) &&
				strings.EqualFold(pointer.StringDeref(securityRule.SourceAddressPrefix, ""), sourceAddressPrefix) {
				return true
			}
		}
	}

	return false
}

func validateDenyAllSecurityRuleExists(nsgs []aznetwork.SecurityGroup, ip string) bool {
	for _, nsg := range nsgs {
		if nsg.SecurityRules == nil {
			continue
		}
		for _, securityRule := range *nsg.SecurityRules {
			utils.Logf("Checking security rule %q", pointer.StringDeref(securityRule.Name, ""))
			if securityRule.Access == aznetwork.SecurityRuleAccessDeny &&
				strings.EqualFold(pointer.StringDeref(securityRule.DestinationAddressPrefix, ""), ip) &&
				strings.HasSuffix(pointer.StringDeref(securityRule.Name, ""), "deny_all") &&
				strings.EqualFold(pointer.StringDeref(securityRule.SourceAddressPrefix, ""), "*") {
				return true
			}
		}
	}

	return false
}
