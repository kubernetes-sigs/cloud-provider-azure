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

		By("Validating ip exists in Security Group")
		port := fmt.Sprintf("%d", serverPort)
		nsgs, err := tc.GetClusterSecurityGroups()
		Expect(err).NotTo(HaveOccurred())
		Expect(validateUnsharedSecurityRuleExists(nsgs, ips, port)).To(BeTrue(), "Security rule for service %s not exists", serviceName)

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

		By(fmt.Sprintf("Validating External domain name %q", ips))
		Expect(len(ports)).NotTo(BeZero())
		for _, ip := range ips {
			err = utils.ValidateServiceConnectivity(ns.Name, agnhostPod, ip, int(ports[0].Port), v1.ProtocolTCP)
			Expect(err).NotTo(HaveOccurred(), "Fail to get response from the domain name %q", ip)
		}

		By("Validate automatically delete the rule, when service is deleted")
		Expect(utils.DeleteService(cs, ns.Name, serviceName)).NotTo(HaveOccurred())
		err = wait.PollImmediate(20*time.Second, 10*time.Minute, func() (done bool, err error) {
			nsgs, err := tc.GetClusterSecurityGroups()
			if err != nil {
				return false, err
			}
			if !validateUnsharedSecurityRuleExists(nsgs, ips, port) {
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
		exist := validateSharedSecurityRuleExists(nsgs, ipList, port)
		Expect(exist).To(BeTrue(), "Security rule for service %s not exists, ipList %q, port %q", serviceName, ipList, port)

		By("Validate automatically adjust or delete the rule, when service is deleted")
		Expect(utils.DeleteService(cs, ns.Name, serviceName)).NotTo(HaveOccurred())
		ipList = ips2

		Eventually(func() (bool, error) {
			nsgs, err = tc.GetClusterSecurityGroups()
			if err != nil {
				return false, err
			}
			return validateSharedSecurityRuleExists(nsgs, ips2, port) && !validateSharedSecurityRuleExists(nsgs, ips1, port), nil
		}).WithContext(ctx).Should(BeTrue(), "Security rule should be modified to only contain service %s", serviceName2)

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

		By("Checking if there is a deny_all rule")
		nsgs, err := tc.GetClusterSecurityGroups()
		Expect(err).NotTo(HaveOccurred())
		found := validateDenyAllSecurityRuleExists(nsgs, internalIPs)
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
		hostExecPodIPv4, hostExecPodIPv6 := hostExecPod.Status.PodIP, hostExecPod.Status.PodIP
		for _, ip := range hostExecPod.Status.PodIPs {
			if net.ParseIP(ip.IP).To4() != nil {
				hostExecPodIPv4 = ip.IP
			} else {
				hostExecPodIPv6 = ip.IP
			}
		}

		maskV4, maskV6 := 32, 128
		allowCIDRs, ipRangesSuffixes := []string{}, []string{}
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		if v4Enabled {
			allowCIDRs = append(allowCIDRs, fmt.Sprintf("%s/%d", hostExecPodIPv4, maskV4))
			ipRangesSuffix := strings.Replace(fmt.Sprintf("%s_%d", hostExecPodIPv4, maskV4), ":", ".", -1) // Handled in pkg/provider/getSecurityRuleName()
			ipRangesSuffixes = append(ipRangesSuffixes, ipRangesSuffix)
		}
		if v6Enabled {
			allowCIDRs = append(allowCIDRs, fmt.Sprintf("%s/%d", hostExecPodIPv6, maskV6))
			ipRangesSuffix := strings.Replace(fmt.Sprintf("%s_%d", hostExecPodIPv6, maskV6), ":", ".", -1) // Handled in pkg/provider/getSecurityRuleName()
			ipRangesSuffixes = append(ipRangesSuffixes, ipRangesSuffix)
		}
		service.Spec.LoadBalancerSourceRanges = allowCIDRs
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Check Service connectivity with the deny-all-except-lb-range ExecAgnhostPod
		By("Waiting for the service to expose")
		internalIPs, err = utils.WaitServiceExposureAndGetIPs(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())
		for _, port := range service.Spec.Ports {
			for _, internalIP := range internalIPs {
				utils.Logf("checking the connectivity of addr %s:%d with protocol %v", internalIP, int(port.Port), port.Protocol)
				err := utils.ValidateServiceConnectivity(ns.Name, agnhostPod, internalIP, int(port.Port), port.Protocol)
				Expect(err).NotTo(HaveOccurred())
			}
		}

		nsgs, err = tc.GetClusterSecurityGroups()
		Expect(err).NotTo(HaveOccurred())
		By("Checking if there are LoadBalancerSourceRanges rules")
		found = validateLoadBalancerSourceRangesRuleExists(nsgs, internalIPs, allowCIDRs, ipRangesSuffixes)
		Expect(found).To(BeTrue(), "internalIPs %q allowCIDRs %q ipRangesSuffixes %q", internalIPs, allowCIDRs, ipRangesSuffixes)

		By("Checking if there is a deny_all rule")
		found = validateDenyAllSecurityRuleExists(nsgs, internalIPs)
		Expect(found).To(BeTrue(), "internalIPs %q", internalIPs)
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-disable-load-balancer-floating-ip`", func() {
		By("Creating public IPs with tags")
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		ipNameBase := basename + "-public-IP-disable-floating-ip"
		targetIPs := []string{}
		deleteFuncs := []func(){}
		if v4Enabled {
			targetIP, deleteFunc := createPIP(tc, ipNameBase, false)
			targetIPs = append(targetIPs, targetIP)
			deleteFuncs = append(deleteFuncs, deleteFunc)
		}
		if v6Enabled {
			targetIP, deleteFunc := createPIP(tc, ipNameBase, true)
			targetIPs = append(targetIPs, targetIP)
			deleteFuncs = append(deleteFuncs, deleteFunc)
		}
		defer func() {
			for _, deleteFunc := range deleteFuncs {
				deleteFunc()
			}
		}()

		By("Creating a test load balancer service with floating IP disabled")
		annotation := map[string]string{
			consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true",
		}
		service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
		service = updateServiceLBIPs(service, false, targetIPs)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, targetIPs)
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			By("cleaning up")
			err = utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Checking if the LoadBalancer's public IP is included in the network security rule's DestinationAddressPrefixes")
		nsgs, err := tc.GetClusterSecurityGroups()
		Expect(err).NotTo(HaveOccurred())
		for _, nsg := range nsgs {
			if nsg.SecurityRules == nil {
				continue
			}

			for _, targetIP := range targetIPs {
				contains := false
				for _, securityRules := range *nsg.SecurityRules {
					if slices.Contains(*securityRules.DestinationAddressPrefixes, targetIP) {
						contains = true
						break
					}
				}
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

		allowedIPRanges := []string{
			"10.20.0.0/16",
			"192.168.0.1/32",
		}

		ipFamilyPolicy := v1.IPFamilyPolicyPreferDualStack
		svc := v1.Service{
			Spec: v1.ServiceSpec{
				Ports:          ports,
				Type:           v1.ServiceTypeLoadBalancer,
				Selector:       labels,
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
		_, err = utils.WaitServiceExposure(cs, ns.Name, serviceName, []string{})
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

func validateUnsharedSecurityRuleExists(nsgs []aznetwork.SecurityGroup, ips []string, port string) bool {
	for _, nsg := range nsgs {
		if nsg.SecurityRules == nil {
			continue
		}
		utils.Logf("Checking nsg %q", pointer.StringDeref(nsg.Name, ""))
		count := 0
		for _, ip := range ips {
			utils.Logf("Checking IP %q", ip)
			for _, securityRule := range *nsg.SecurityRules {
				utils.Logf("Checking security rule %q", pointer.StringDeref(securityRule.Name, ""))
				if strings.EqualFold(pointer.StringDeref(securityRule.DestinationAddressPrefix, ""), ip) &&
					strings.EqualFold(pointer.StringDeref(securityRule.DestinationPortRange, ""), port) {
					utils.Logf("Found")
					count++
					break
				}
			}
		}
		if count == len(ips) {
			return true
		}
	}
	return false
}

func validateSharedSecurityRuleExists(nsgs []aznetwork.SecurityGroup, ips []string, port string) bool {
	ipsV4, ipsV6 := []string{}, []string{}
	for _, ip := range ips {
		if net.ParseIP(ip).To4() != nil {
			ipsV4 = append(ipsV4, ip)
		} else {
			ipsV6 = append(ipsV6, ip)
		}
	}
	v4Exist := validateSharedSecurityRuleExistsSingleStack(nsgs, ipsV4, port, false)
	v6Exist := validateSharedSecurityRuleExistsSingleStack(nsgs, ipsV6, port, true)
	return v4Exist && v6Exist
}

func validateSharedSecurityRuleExistsSingleStack(nsgs []aznetwork.SecurityGroup, ips []string, port string, isIPv6 bool) bool {
	if len(ips) == 0 {
		return true
	}
	utils.Logf("validateSharedSecurityRuleExists for isIPv6 %t", isIPv6)
	for _, nsg := range nsgs {
		if nsg.SecurityRules == nil {
			continue
		}
		utils.Logf("Checking nsg %q", pointer.StringDeref(nsg.Name, ""))
		for _, securityRule := range *nsg.SecurityRules {
			utils.Logf("Checking security rule %q DestinationPortRange %q DestinationAddressPrefixes %q",
				pointer.StringDeref(securityRule.Name, ""), pointer.StringDeref(securityRule.DestinationPortRange, ""), *securityRule.DestinationAddressPrefixes)
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

func validateLoadBalancerSourceRangesRuleExists(nsgs []aznetwork.SecurityGroup, ips, sourceAddressPrefixes, ipRangesSuffixes []string) bool {
	ipsV4, ipsV6 := []string{}, []string{}
	for _, ip := range ips {
		if net.ParseIP(ip).To4() != nil {
			ipsV4 = append(ipsV4, ip)
		} else {
			ipsV6 = append(ipsV6, ip)
		}
	}
	isDualStack := len(ipsV4) != 0 && len(ipsV6) != 0
	v4Exist := validateLoadBalancerSourceRangesRuleExistsSingleStack(nsgs, ipsV4, sourceAddressPrefixes, ipRangesSuffixes, isDualStack, false)
	v6Exist := validateLoadBalancerSourceRangesRuleExistsSingleStack(nsgs, ipsV6, sourceAddressPrefixes, ipRangesSuffixes, isDualStack, true)
	return v4Exist && v6Exist
}

func validateLoadBalancerSourceRangesRuleExistsSingleStack(nsgs []aznetwork.SecurityGroup, ips, sourceAddressPrefixes, ipRangesSuffixes []string, isDualStack, isIPv6 bool) bool {
	if len(ips) == 0 {
		return true
	}
	utils.Logf("validateLoadBalancerSourceRangesRuleExists for isIPv6 %t", isIPv6)
	if len(sourceAddressPrefixes) != len(ipRangesSuffixes) {
		return false
	}
	for _, nsg := range nsgs {
		if nsg.SecurityRules == nil {
			continue
		}
		utils.Logf("Checking nsg %q", pointer.StringDeref(nsg.Name, ""))
		count := 0
		for _, ip := range ips {
			utils.Logf("Checking IP %q", ip)
			for _, securityRule := range *nsg.SecurityRules {
				utils.Logf("Checking security rule %q access %q DestinationAddressPrefix %q SourceAddressPrefix %q",
					pointer.StringDeref(securityRule.Name, ""), securityRule.Access,
					pointer.StringDeref(securityRule.DestinationAddressPrefix, ""), pointer.StringDeref(securityRule.SourceAddressPrefix, ""))
				found := false
				if securityRule.Access == aznetwork.SecurityRuleAccessAllow &&
					strings.EqualFold(pointer.StringDeref(securityRule.DestinationAddressPrefix, ""), ip) {
					for i := range sourceAddressPrefixes {
						sourceAddressPrefix := sourceAddressPrefixes[i]
						ipRangesSuffix := ipRangesSuffixes[i]
						if isDualStack {
							ipRangesSuffix = ipRangesSuffixes[i] + utils.Suffixes[isIPv6]
						}
						if strings.HasSuffix(pointer.StringDeref(securityRule.Name, ""), ipRangesSuffix) &&
							strings.EqualFold(pointer.StringDeref(securityRule.SourceAddressPrefix, ""), sourceAddressPrefix) {
							utils.Logf("Found")
							count++
							found = true
							break
						}
					}
				}
				if found {
					break
				}
			}
		}
		if count == len(ips) {
			return true
		}
	}

	return false
}

func validateDenyAllSecurityRuleExists(nsgs []aznetwork.SecurityGroup, ips []string) bool {
	ipsV4, ipsV6 := []string{}, []string{}
	for _, ip := range ips {
		if net.ParseIP(ip).To4() != nil {
			ipsV4 = append(ipsV4, ip)
		} else {
			ipsV6 = append(ipsV6, ip)
		}
	}
	isDualStack := len(ipsV4) != 0 && len(ipsV6) != 0
	v4Exist := validateDenyAllSecurityRuleExistsSingleStack(nsgs, ipsV4, isDualStack, false)
	v6Exist := validateDenyAllSecurityRuleExistsSingleStack(nsgs, ipsV6, isDualStack, true)
	return v4Exist && v6Exist
}

func validateDenyAllSecurityRuleExistsSingleStack(nsgs []aznetwork.SecurityGroup, ips []string, isDualStack, isIPv6 bool) bool {
	if len(ips) == 0 {
		return true
	}
	utils.Logf("validateDenyAllSecurityRuleExists for isIPv6 %t", isIPv6)
	for _, nsg := range nsgs {
		if nsg.SecurityRules == nil {
			continue
		}
		utils.Logf("Checking nsg %q", pointer.StringDeref(nsg.Name, ""))
		count := 0
		for _, ip := range ips {
			utils.Logf("Checking IP %q", ip)
			for _, securityRule := range *nsg.SecurityRules {
				utils.Logf("Checking security rule %q DestinationAddressPrefix %q SourceAddressPrefix %q",
					pointer.StringDeref(securityRule.Name, ""), pointer.StringDeref(securityRule.DestinationAddressPrefix, ""), pointer.StringDeref(securityRule.SourceAddressPrefix, ""))
				denyAllSuffix := "deny_all"
				if isDualStack {
					denyAllSuffix = "deny_all" + utils.Suffixes[isIPv6]
				}
				if securityRule.Access == aznetwork.SecurityRuleAccessDeny &&
					strings.EqualFold(pointer.StringDeref(securityRule.DestinationAddressPrefix, ""), ip) &&
					strings.HasSuffix(pointer.StringDeref(securityRule.Name, ""), denyAllSuffix) &&
					strings.EqualFold(pointer.StringDeref(securityRule.SourceAddressPrefix, ""), "*") {
					utils.Logf("Found")
					count++
					break
				}
			}

		}
		if count == len(ips) {
			return true
		}
	}

	return false
}
