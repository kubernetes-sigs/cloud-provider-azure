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
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	network "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var (
	scalesetRE               = regexp.MustCompile(`.*/subscriptions/(?:.*)/resourceGroups/(.+)/providers/Microsoft.Compute/virtualMachineScaleSets/(.+)/virtualMachines(?:.*)`)
	lbNameRE                 = regexp.MustCompile(`^/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Network/loadBalancers/(.+)/frontendIPConfigurations(?:.*)`)
	backendIPConfigurationRE = regexp.MustCompile(`^/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Compute/virtualMachineScaleSets/(.+)/virtualMachines(?:.*)`)
)

const (
	serverPort             = 80
	alterNativeServicePort = 8080
	testingPort            = 81
	IPV6Prefix             = "IPv6"
)

var _ = Describe("Service with annotation", Label(utils.TestSuiteLabelServiceAnnotation), func() {
	basename := "service"
	serviceName := "annotation-test"
	initSuccess := false

	var (
		cs clientset.Interface
		tc *utils.AzureTestClient
		ns *v1.Namespace
	)

	labels := map[string]string{
		"app": serviceName,
	}
	var ports []v1.ServicePort

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating Azure clients")
		tc, err = utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating deployment " + serviceName)
		deployment := createServerDeploymentManifest(serviceName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Waiting for backend pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		ports = []v1.ServicePort{{
			Name:       "http",
			Port:       serverPort,
			TargetPort: intstr.FromInt(serverPort),
		}}
	})

	AfterEach(func() {
		if !initSuccess {
			// Get non-running Pods' describe info
			pods := []v1.Pod{}
			testPods, err := utils.GetPodList(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())
			pods = append(pods, testPods.Items...)
			ksPods, err := utils.GetPodList(cs, "kube-system")
			Expect(err).NotTo(HaveOccurred())
			pods = append(pods, ksPods.Items...)
			for _, pod := range pods {
				if pod.Status.Phase != v1.PodRunning {
					output, err := utils.RunKubectl(ns.Name, "describe", "pod", pod.Name)
					Expect(err).NotTo(HaveOccurred())
					utils.Logf("Describe info of Pod %q:\n%s", pod.Name, output)
				}
			}
		}

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

	It("should support service annotation 'service.beta.kubernetes.io/azure-dns-label-name'", func() {
		// This test creates/deletes/updates some Services:
		// 1. Create a Service with managed PIP and check connectivity with DNS
		// 2. Delete the Service
		// 3. Create a Service with user assigned PIP
		// 4. Delete the Service and check tags
		// 5. Create a Service with different name
		// 6. Update the Service with new tag
		By("Create a Service with managed PIP")
		serviceDomainNamePrefix := fmt.Sprintf("%s-%s", serviceName, uuid.NewUUID())
		annotation := map[string]string{
			consts.ServiceAnnotationDNSLabelName: serviceDomainNamePrefix,
		}
		// create service with given annotation and wait it to expose
		_ = createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		// Use a hostNetwork Pod to validate Service connectivity via the cluster Node's
		// network because the current VM running go test may not support IPv6.
		agnhostPod := fmt.Sprintf("%s-%s", utils.ExecAgnhostPod, "azure-dns-label-name")
		result, err := utils.CreateHostExecPod(cs, ns.Name, agnhostPod)
		defer func() {
			err = utils.DeletePod(cs, ns.Name, agnhostPod)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(BeTrue())

		serviceDomainName := utils.GetServiceDomainName(serviceDomainNamePrefix)
		By(fmt.Sprintf("Validating External domain name %q", serviceDomainName))
		err = utils.ValidateServiceConnectivity(ns.Name, agnhostPod, serviceDomainName, int(ports[0].Port), v1.ProtocolTCP)
		Expect(err).NotTo(HaveOccurred(), "Fail to get response from the domain name")

		By("Delete the Service")
		err = utils.DeleteService(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())

		By("Create PIPs")
		ipNameBase := fmt.Sprintf("%s-public-IP-%s", basename, uuid.NewUUID()[0:4])
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		pipNames, targetIPs := []*string{}, []*string{}
		deleteFuncs := []func(){}
		if v4Enabled {
			targetIP, deleteFunc := createPIP(tc, ipNameBase, false)
			targetIPs = append(targetIPs, &targetIP)
			deleteFuncs = append(deleteFuncs, deleteFunc)
		}
		if v6Enabled {
			targetIP, deleteFunc := createPIP(tc, ipNameBase, true)
			targetIPs = append(targetIPs, &targetIP)
			deleteFuncs = append(deleteFuncs, deleteFunc)
		}
		defer func() {
			for _, deleteFunc := range deleteFuncs {
				deleteFunc()
			}
		}()

		By("Create a Service which will be deleted with the PIP")
		nsName := ns.Name
		oldServiceName := fmt.Sprintf("%s-old", serviceName)
		service := utils.CreateLoadBalancerServiceManifest(oldServiceName, annotation, labels, nsName, ports)
		service = updateServiceLBIPs(service, false, targetIPs)

		// create service with given annotation and wait it to expose
		_, err = cs.CoreV1().Services(nsName).Create(context.TODO(), service, metav1.CreateOptions{})
		defer func() {
			utils.Logf("Delete test Service %q", oldServiceName)
			err := utils.DeleteService(cs, ns.Name, oldServiceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, nsName, oldServiceName, targetIPs)
		Expect(err).NotTo(HaveOccurred())

		By("Delete the old Service")
		err = utils.DeleteService(cs, ns.Name, oldServiceName)
		Expect(err).NotTo(HaveOccurred())

		By("Check if PIP DNS label is not tagged onto the user-assigned pip")
		for _, pipName := range pipNames {
			deleted, err := ifPIPDNSLabelDeleted(tc, *pipName)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())
		}

		By("Create a different Service with the same azure-dns-label-name tag")
		service.Name = serviceName
		_, err = cs.CoreV1().Services(nsName).Create(context.TODO(), service, metav1.CreateOptions{})
		defer func() {
			utils.Logf("Delete test Service %q", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		serviceDomainName = utils.GetServiceDomainName(serviceDomainNamePrefix)
		By(fmt.Sprintf("Validating External domain name %q", serviceDomainName))
		err = utils.ValidateServiceConnectivity(ns.Name, agnhostPod, serviceDomainName, int(ports[0].Port), v1.ProtocolTCP)
		Expect(err).NotTo(HaveOccurred(), "Fail to get response from the domain name")

		By("Update service")
		service.Annotations[consts.ServiceAnnotationDNSLabelName] = fmt.Sprintf("%s-new", serviceDomainNamePrefix)
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		serviceDomainName = utils.GetServiceDomainName(serviceDomainNamePrefix)
		By(fmt.Sprintf("Validating External domain name %q", serviceDomainName))
		err = utils.ValidateServiceConnectivity(ns.Name, agnhostPod, serviceDomainName, int(ports[0].Port), v1.ProtocolTCP)
		Expect(err).NotTo(HaveOccurred(), "Fail to get response from the domain name")
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-load-balancer-internal'", func() {
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal: "true",
		}

		// create service with given annotation and wait it to expose
		_ = createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-load-balancer-internal-subnet'", func() {
		By("creating environment")
		// This subnetName verifies a bug fix in an issue: https://github.com/kubernetes-sigs/cloud-provider-azure/issues/1443
		subnetName := "a--------------------------------------------------z"

		vNet, err := tc.GetClusterVirtualNetwork()
		Expect(err).NotTo(HaveOccurred())

		var newSubnetCIDRs []*net.IPNet
		for _, existingSubnet := range vNet.Properties.Subnets {
			if *existingSubnet.Name != subnetName {
				continue
			}
			utils.Logf("Test subnet have existed, skip creating")
			if existingSubnet.Properties.AddressPrefix != nil {
				// IPv4 picks the only AddressPrefix.
				_, newSubnetCIDR, err := net.ParseCIDR(*existingSubnet.Properties.AddressPrefix)
				Expect(err).NotTo(HaveOccurred())
				newSubnetCIDRs = append(newSubnetCIDRs, newSubnetCIDR)
			} else {
				Expect(existingSubnet.Properties.AddressPrefixes).NotTo(BeNil(),
					"subnet AddressPrefix and AddressPrefixes shouldn't be both nil")

				expectedAddrPrefixPickedCount := 1
				if tc.IPFamily == utils.DualStack {
					expectedAddrPrefixPickedCount = 2
				}
				for _, addrPrefix := range existingSubnet.Properties.AddressPrefixes {
					_, newSubnetCIDR, err := net.ParseCIDR(*addrPrefix)
					Expect(err).NotTo(HaveOccurred(), "failed to parse CIDR %q", addrPrefix)
					newSubnetCIDRs = append(newSubnetCIDRs, newSubnetCIDR)
				}
				Expect(len(newSubnetCIDRs)).To(Equal(expectedAddrPrefixPickedCount), "incorrect new subnet CIDRs %v", newSubnetCIDRs)
			}
			break
		}

		if len(newSubnetCIDRs) == 0 {
			By("Test subnet doesn't exist. Creating a new one...")
			newSubnetCIDRs, err = utils.GetNextSubnetCIDRs(vNet, tc.IPFamily)
			Expect(err).NotTo(HaveOccurred())
			newSubnetCIDRStrs := []string{}
			for _, newSubnetCIDR := range newSubnetCIDRs {
				newSubnetCIDRStrs = append(newSubnetCIDRStrs, newSubnetCIDR.String())
			}
			_, err = tc.CreateSubnet(vNet, &subnetName, to.SliceOfPtrs(newSubnetCIDRStrs...), false)
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				utils.Logf("cleaning up test subnet %s", subnetName)
				err = tc.DeleteSubnet(*vNet.Name, subnetName)
				Expect(err).NotTo(HaveOccurred())
			}()
		}

		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal:       "true",
			consts.ServiceAnnotationLoadBalancerInternalSubnet: subnetName,
		}

		// create service with given annotation and wait it to expose
		ips := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Get External IPs: %v", utils.StrPtrSliceToStrSlice(ips))

		By("Validating external ip in target subnet")
		Expect(len(ips)).NotTo(BeZero())
		for _, ip := range ips {
			contains := false
			for _, newSubnetCIDR := range newSubnetCIDRs {
				if newSubnetCIDR.Contains(net.ParseIP(*ip)) {
					contains = true
					break
				}
			}
			Expect(contains).To(BeTrue(), "external IP %q is not in the target subnets %q", ip, newSubnetCIDRs)
		}
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-load-balancer-enable-high-availability-ports'", func() {
		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(network.LoadBalancerSKUNameStandard)) {
			Skip("azure-load-balancer-enable-high-availability-ports only work with Standard Load Balancer")
		}

		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "true",
			consts.ServiceAnnotationLoadBalancerInternal:                    "true",
		}
		// create service with given annotation and wait it to expose
		ips := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(len(ips)).NotTo(BeZero())
		ip := ips[0]

		lb := getAzureInternalLoadBalancerFromPrivateIP(tc, ip, "")

		Expect(len(lb.Properties.LoadBalancingRules)).To(Equal(len(ips)))
		rule := (lb.Properties.LoadBalancingRules)[0]
		Expect(*rule.Properties.FrontendPort).To(Equal(int32(0)))
		Expect(*rule.Properties.BackendPort).To(Equal(int32(0)))
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-load-balancer-tcp-idle-timeout'", func() {
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerIdleTimeout: "5",
		}

		// create service with given annotation and wait it to expose
		publicIPs := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			By("Cleaning up service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(len(publicIPs)).NotTo(BeZero())
		publicIP := publicIPs[0]

		// get lb from azure client
		lb := getAzureLoadBalancerFromPIP(tc, publicIP, tc.GetResourceGroup(), "")

		var idleTimeout *int32
		for _, rule := range lb.Properties.LoadBalancingRules {
			if rule.Properties.IdleTimeoutInMinutes != nil {
				idleTimeout = rule.Properties.IdleTimeoutInMinutes
			}
		}
		Expect(*idleTimeout).To(Equal(int32(5)))
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-load-balancer-resource-group'", func() {
		By("creating a test resource group")
		rg, cleanup := utils.CreateTestResourceGroup(tc)
		defer cleanup(pointer.StringDeref(rg.Name, ""))

		By("creating test PIP in the test resource group")
		pips := []*string{}
		testPIPNames := []string{}
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		createPIPInRG := func(isIPv6 bool) {
			testPIPName := "testPIP-" + utils.GetNameWithSuffix(string(uuid.NewUUID())[0:4], utils.Suffixes[isIPv6])
			pip, err := utils.WaitCreatePIP(tc, testPIPName, *rg.Name, defaultPublicIPAddress(testPIPName, isIPv6))
			Expect(err).NotTo(HaveOccurred())
			testPIPNames = append(testPIPNames, testPIPName)
			Expect(pip.Properties).NotTo(BeNil())
			pips = append(pips, pip.Properties.IPAddress)
		}
		if v4Enabled {
			createPIPInRG(false)
		}
		if v6Enabled {
			createPIPInRG(true)
		}
		defer func() {
			utils.Logf("Cleaning up service and public IP")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
			for _, testPIPName := range testPIPNames {
				err = utils.DeletePIPWithRetry(tc, testPIPName, *rg.Name)
				Expect(err).NotTo(HaveOccurred())
			}
		}()

		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerResourceGroup: pointer.StringDeref(rg.Name, ""),
		}
		By("Creating service " + serviceName + " in namespace " + ns.Name)
		service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
		service = updateServiceLBIPs(service, false, pips)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.PrintCreateSVCSuccessfully(serviceName, ns.Name)

		//wait and get service's public IP Address
		By("Waiting service to expose...")
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, pips)
		Expect(err).NotTo(HaveOccurred())

		Expect(len(pips)).NotTo(BeZero())
		lb := getAzureLoadBalancerFromPIP(tc, pips[0], *rg.Name, "")
		Expect(lb).NotTo(BeNil())
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-pip-tags`", func() {
		if os.Getenv(utils.AKSTestCCM) != "" {
			Skip("Skip this test case for AKS test")
		}

		expectedTags := map[string]*string{
			"a": pointer.String("c"),
			"c": pointer.String("d"),
			"e": pointer.String(""),
			"x": pointer.String("y"),
		}

		testPIPTagAnnotationWithTags(cs, tc, ns, serviceName, labels, ports, expectedTags)
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-pip-tags` on aks clusters with systemTags set", func() {
		if os.Getenv(utils.AKSTestCCM) == "" {
			Skip("Skip this test case for non-AKS test")
		}

		expectedTags := map[string]*string{
			"a": pointer.String("c"),
			"x": pointer.String("y"),
		}

		testPIPTagAnnotationWithTags(cs, tc, ns, serviceName, labels, ports, expectedTags)
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-pip-name`", func() {
		By("Creating two test pips or more if DualStack")
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		pipNames1, pipNames2 := map[bool]string{}, map[bool]string{}
		pipNamesSlice1, pipNamesSlice2 := []string{}, []string{}
		pipNameBase1, pipNameBase2 := "pip1", "pip2"
		targetIPs1, targetIPs2 := []string{}, []string{}
		deleteFuncs := []func(){}
		doPIP := func(isIPv6 bool) {
			pipName1 := utils.GetNameWithSuffix(pipNameBase1, utils.Suffixes[isIPv6])
			pipNames1[isIPv6] = pipName1
			pipNamesSlice1 = append(pipNamesSlice1, pipName1)
			targetIP1, deleteFunc1 := createPIP(tc, pipNameBase1, isIPv6)
			targetIPs1 = append(targetIPs1, targetIP1)
			deleteFuncs = append(deleteFuncs, deleteFunc1)

			pipName2 := utils.GetNameWithSuffix(pipNameBase2, utils.Suffixes[isIPv6])
			pipNames2[isIPv6] = pipName2
			pipNamesSlice2 = append(pipNamesSlice2, pipName2)
			targetIP2, deleteFunc2 := createPIP(tc, pipNameBase2, isIPv6)
			targetIPs2 = append(targetIPs2, targetIP2)
			deleteFuncs = append(deleteFuncs, deleteFunc2)
		}
		if v4Enabled {
			doPIP(false)
		}
		if v6Enabled {
			doPIP(true)
		}
		defer func() {
			for _, deleteFunc := range deleteFuncs {
				deleteFunc()
			}
		}()

		By("Creating a service referring to the first pip")
		annotation := map[string]string{}
		if tc.IPFamily == utils.DualStack {
			annotation[consts.ServiceAnnotationPIPNameDualStack[false]] = pipNames1[false]
			annotation[consts.ServiceAnnotationPIPNameDualStack[true]] = pipNames1[true]
		} else {
			annotation[consts.ServiceAnnotationPIPNameDualStack[false]] = pipNames1[tc.IPFamily == utils.IPv6]
		}

		service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		defer func() {
			By("Cleaning up test service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the service to expose")
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, to.SliceOfPtrs(targetIPs1...))
		Expect(err).NotTo(HaveOccurred())

		By("Updating the service to refer to the second service")
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		if tc.IPFamily == utils.DualStack {
			service.Annotations[consts.ServiceAnnotationPIPNameDualStack[false]] = pipNames2[false]
			service.Annotations[consts.ServiceAnnotationPIPNameDualStack[true]] = pipNames2[true]
		} else {
			service.Annotations[consts.ServiceAnnotationPIPNameDualStack[false]] = pipNames2[tc.IPFamily == utils.IPv6]
		}

		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for service IP to be updated")
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, to.SliceOfPtrs(targetIPs2...))
		Expect(err).NotTo(HaveOccurred())
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-pip-prefix-id`", func() {
		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(network.LoadBalancerSKUNameStandard)) {
			Skip("pip-prefix-id only work with Standard Load Balancer")
		}

		By("Creating two test PIPPrefixes or more if DualStack")
		const (
			prefix1NameBase = "prefix1"
			prefix2NameBase = "prefix2"
		)
		prefixIDs1, prefixIDs2 := map[bool]string{}, map[bool]string{}
		prefixNames1, prefixNames2 := map[bool]string{}, map[bool]string{}
		deleteFuncs := []func(){}

		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		createPIPPrefix := func(isIPv6 bool) {
			prefixName := utils.GetNameWithSuffix(prefix1NameBase, utils.Suffixes[isIPv6])
			prefixNames1[isIPv6] = prefixName
			prefix, err := utils.WaitCreatePIPPrefix(tc, prefixName, tc.GetResourceGroup(), defaultPublicIPPrefix(prefixName, isIPv6))
			deleteFuncs = append(deleteFuncs, func() {
				Expect(utils.DeletePIPPrefixWithRetry(tc, prefixName)).NotTo(HaveOccurred())
			})
			Expect(err).NotTo(HaveOccurred())
			prefixIDs1[isIPv6] = pointer.StringDeref(prefix.ID, "")

			prefixName = utils.GetNameWithSuffix(prefix2NameBase, utils.Suffixes[isIPv6])
			prefixNames2[isIPv6] = prefixName
			prefix, err = utils.WaitCreatePIPPrefix(tc, prefixName, tc.GetResourceGroup(), defaultPublicIPPrefix(prefixName, isIPv6))
			deleteFuncs = append(deleteFuncs, func() {
				Expect(utils.DeletePIPPrefixWithRetry(tc, prefixName)).NotTo(HaveOccurred())
			})
			Expect(err).NotTo(HaveOccurred())
			prefixIDs2[isIPv6] = pointer.StringDeref(prefix.ID, "")
		}
		if v4Enabled {
			createPIPPrefix(false)
		}
		if v6Enabled {
			createPIPPrefix(true)
		}
		defer func() {
			for _, deleteFunc := range deleteFuncs {
				deleteFunc()
			}
		}()

		By("Creating a service referring to the prefix")
		{
			annotation := map[string]string{}
			for isIPv6, id := range prefixIDs1 {
				if tc.IPFamily == utils.DualStack {
					annotation[consts.ServiceAnnotationPIPPrefixIDDualStack[isIPv6]] = id
				} else {
					annotation[consts.ServiceAnnotationPIPPrefixIDDualStack[false]] = id
				}
			}
			service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		defer func() {
			By("Cleaning up test service")
			Expect(utils.DeleteService(cs, ns.Name, serviceName)).NotTo(HaveOccurred())
		}()

		By("Waiting for the service to expose")
		{
			ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, []*string{})
			Expect(err).NotTo(HaveOccurred())
			for _, ip := range ips {
				isIPv6 := net.ParseIP(*ip).To4() == nil
				pip, err := utils.WaitGetPIPByPrefix(tc, prefixNames1[isIPv6], true)
				Expect(err).NotTo(HaveOccurred())

				Expect(pip.Properties.IPAddress).NotTo(BeNil())
				Expect(pointer.StringDeref(pip.Properties.PublicIPPrefix.ID, "")).To(Equal(prefixIDs1[isIPv6]))
				Expect(*ip).To(Equal(pointer.StringDeref(pip.Properties.IPAddress, "")))
			}
		}

		By("Updating the service to refer to the second prefix")
		{
			service, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			for isIPv6, id := range prefixIDs2 {
				// TODO: Update after dual-stack implementation finishes
				if tc.IPFamily == utils.DualStack {
					service.Annotations[consts.ServiceAnnotationPIPPrefixIDDualStack[isIPv6]] = id
				} else {
					service.Annotations[consts.ServiceAnnotationPIPPrefixIDDualStack[false]] = id
				}
			}
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for service IP to be updated")
		{
			// wait until ip created by prefix
			pipAddrs := []string{}
			doPIPByPrefix := func(isIPv6 bool) {
				pip, err := utils.WaitGetPIPByPrefix(tc, prefixNames2[isIPv6], true)
				Expect(err).NotTo(HaveOccurred())

				Expect(pip.Properties.IPAddress).NotTo(BeNil())
				Expect(pointer.StringDeref(pip.Properties.PublicIPPrefix.ID, "")).To(Equal(prefixIDs2[isIPv6]))
				pipAddrs = append(pipAddrs, pointer.StringDeref(pip.Properties.IPAddress, ""))
			}
			if v4Enabled {
				doPIPByPrefix(false)
			}
			if v6Enabled {
				doPIPByPrefix(true)
			}

			_, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, to.SliceOfPtrs(pipAddrs...))
			Expect(err).NotTo(HaveOccurred())
		}
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-load-balancer-health-probe-port' and port specific configs", func() {
		By("Creating a service with health probe annotations")
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerHealthProbeNumOfProbe:                                      "5",
			consts.BuildHealthProbeAnnotationKeyForPort(serverPort, consts.HealthProbeParamsNumOfProbe):    "3",
			consts.ServiceAnnotationLoadBalancerHealthProbeInterval:                                        "15",
			consts.BuildHealthProbeAnnotationKeyForPort(serverPort, consts.HealthProbeParamsProbeInterval): "10",
			consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                                        "Http",
			consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath:                                     "/",
			consts.BuildHealthProbeAnnotationKeyForPort(serverPort, consts.HealthProbeParamsPort):          "10249",
		}

		// create service with given annotation and wait it to expose
		publicIPs := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, annotation, ports, func(s *v1.Service) error {
			s.Spec.HealthCheckNodePort = 32252
			s.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
			return nil
		})
		defer func() {
			By("Cleaning up service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(len(publicIPs)).NotTo(BeZero())
		ids := []string{}
		for _, publicIP := range publicIPs {
			pipFrontendConfigID := getPIPFrontendConfigurationID(tc, *publicIP, tc.GetResourceGroup(), false)
			pipFrontendConfigIDSplit := strings.Split(pipFrontendConfigID, "/")
			Expect(len(pipFrontendConfigIDSplit)).NotTo(Equal(0))
			ids = append(ids, pipFrontendConfigIDSplit[len(pipFrontendConfigIDSplit)-1])
		}
		utils.Logf("PIP frontend config IDs %q", ids)

		var lb *network.LoadBalancer
		var targetProbes []*network.Probe
		expectedTargetProbesCount := 1
		if tc.IPFamily == utils.DualStack {
			expectedTargetProbesCount = 2
		}
		//wait for backend update
		err := wait.PollImmediate(5*time.Second, 60*time.Second, func() (bool, error) {
			lb = getAzureLoadBalancerFromPIP(tc, publicIPs[0], tc.GetResourceGroup(), "")
			targetProbes = []*network.Probe{}
			for i := range lb.Properties.Probes {
				probe := (lb.Properties.Probes)[i]
				utils.Logf("One probe of LB is %q", *probe.Name)
				probeSplit := strings.Split(*probe.Name, "-")
				Expect(len(probeSplit)).NotTo(Equal(0))
				probeSplitID := probeSplit[0]
				if probeSplit[len(probeSplit)-1] == IPV6Prefix {
					probeSplitID += "-" + probeSplit[len(probeSplit)-1]
				}
				for _, id := range ids {
					if id == probeSplitID {
						targetProbes = append(targetProbes, probe)
					}
				}
			}

			utils.Logf("targetProbes count %d, expectedTargetProbes count %d", len(targetProbes), expectedTargetProbesCount)
			return len(targetProbes) == expectedTargetProbesCount, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Validating health probe configs")
		for _, probe := range targetProbes {
			if probe.Properties.ProbeThreshold != nil {
				utils.Logf("Validating health probe config numberOfProbes")
				Expect(*probe.Properties.ProbeThreshold).To(Equal(int32(3)))
			}
			if probe.Properties.IntervalInSeconds != nil {
				utils.Logf("Validating health probe config intervalInSeconds")
				Expect(*probe.Properties.IntervalInSeconds).To(Equal(int32(10)))
			}
			utils.Logf("Validating health probe config ProbeProtocolHTTP")
			Expect(*probe.Properties.Protocol).To(Equal(network.ProbeProtocolHTTP))
		}
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-load-balancer-health-probe-num-of-probe', 'service.beta.kubernetes.io/azure-load-balancer-health-probe-interval', 'service.beta.kubernetes.io/azure-load-balancer-health-probe-protocol' and port specific configs", func() {
		By("Creating a service with health probe annotations")
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerHealthProbeNumOfProbe:                                      "5",
			consts.BuildHealthProbeAnnotationKeyForPort(serverPort, consts.HealthProbeParamsNumOfProbe):    "3",
			consts.ServiceAnnotationLoadBalancerHealthProbeInterval:                                        "15",
			consts.BuildHealthProbeAnnotationKeyForPort(serverPort, consts.HealthProbeParamsProbeInterval): "10",
			consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                                        "Http",
			consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath:                                     "/",
		}

		// create service with given annotation and wait it to expose
		publicIPs := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			By("Cleaning up service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(len(publicIPs)).NotTo(BeZero())
		ids := []string{}
		for _, publicIP := range publicIPs {
			pipFrontendConfigID := getPIPFrontendConfigurationID(tc, *publicIP, tc.GetResourceGroup(), false)
			pipFrontendConfigIDSplit := strings.Split(pipFrontendConfigID, "/")
			Expect(len(pipFrontendConfigIDSplit)).NotTo(Equal(0))
			ids = append(ids, pipFrontendConfigIDSplit[len(pipFrontendConfigIDSplit)-1])
		}
		utils.Logf("PIP frontend config IDs %q", ids)

		var lb *network.LoadBalancer
		var targetProbes []*network.Probe
		expectedTargetProbesCount := 1
		if tc.IPFamily == utils.DualStack {
			expectedTargetProbesCount = 2
		}
		//wait for backend update
		err := wait.PollImmediate(5*time.Second, 60*time.Second, func() (bool, error) {
			lb = getAzureLoadBalancerFromPIP(tc, publicIPs[0], tc.GetResourceGroup(), "")
			targetProbes = []*network.Probe{}
			for i := range lb.Properties.Probes {
				probe := (lb.Properties.Probes)[i]
				utils.Logf("One probe of LB is %q", *probe.Name)
				probeSplit := strings.Split(*probe.Name, "-")
				Expect(len(probeSplit)).NotTo(Equal(0))
				probeSplitID := probeSplit[0]
				if probeSplit[len(probeSplit)-1] == IPV6Prefix {
					probeSplitID += "-" + probeSplit[len(probeSplit)-1]
				}
				for _, id := range ids {
					if id == probeSplitID {
						targetProbes = append(targetProbes, probe)
					}
				}
			}

			utils.Logf("targetProbes count %d, expectedTargetProbes count %d", len(targetProbes), expectedTargetProbesCount)
			return len(targetProbes) == expectedTargetProbesCount, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Validating health probe configs")
		for _, probe := range targetProbes {
			if probe.Properties.ProbeThreshold != nil {
				utils.Logf("Validating health probe config numberOfProbes")
				Expect(*probe.Properties.ProbeThreshold).To(Equal(int32(3)))
			}
			if probe.Properties.IntervalInSeconds != nil {
				utils.Logf("Validating health probe config intervalInSeconds")
				Expect(*probe.Properties.IntervalInSeconds).To(Equal(int32(10)))
			}
			utils.Logf("Validating health probe config ProbeProtocolHTTP")
			Expect(*probe.Properties.Protocol).To(Equal(network.ProbeProtocolHTTP))
		}

		By("Changing ExternalTrafficPolicy of the service to Local")
		expectedTargetProbesLocalCount := 1
		if tc.IPFamily == utils.DualStack {
			expectedTargetProbesLocalCount = 2
		}
		var service *v1.Service
		utils.Logf("Updating service "+serviceName, ns.Name)
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			service.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
			return err
		})
		Expect(retryErr).NotTo(HaveOccurred())
		utils.Logf("Successfully updated LoadBalancer service "+serviceName, ns.Name)

		By("Getting updated service object from server")
		retryErr = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if service.Spec.HealthCheckNodePort == 0 {
				return fmt.Errorf("service HealthCheckNodePort is not updated")
			}
			return nil
		})
		Expect(retryErr).NotTo(HaveOccurred())

		err = wait.PollImmediate(5*time.Second, 300*time.Second, func() (bool, error) {
			lb = getAzureLoadBalancerFromPIP(tc, publicIPs[0], tc.GetResourceGroup(), "")
			targetProbes = []*network.Probe{}
			for i := range lb.Properties.Probes {
				probe := (lb.Properties.Probes)[i]
				utils.Logf("One probe of LB is %q", *probe.Name)
				probeSplit := strings.Split(*probe.Name, "-")
				Expect(len(probeSplit)).NotTo(Equal(0))
				probeSplitID := probeSplit[0]
				if probeSplit[len(probeSplit)-1] == IPV6Prefix {
					probeSplitID += "-" + probeSplit[len(probeSplit)-1]
				}
				for _, id := range ids {
					if id == probeSplitID {
						targetProbes = append(targetProbes, probe)
					}
				}
			}

			utils.Logf("targetProbes count %d, expectedTargetProbesLocal count %d", len(targetProbes), expectedTargetProbesLocalCount)
			if len(targetProbes) != expectedTargetProbesLocalCount {
				return false, nil
			}
			By("Validating health probe configs")
			for _, probe := range targetProbes {
				utils.Logf("Validating health probe config numberOfProbes")
				if probe.Properties.ProbeThreshold == nil || *probe.Properties.ProbeThreshold != int32(5) {
					return false, nil
				}
				utils.Logf("Validating health probe config intervalInSeconds")
				if probe.Properties.IntervalInSeconds == nil || *probe.Properties.IntervalInSeconds != int32(15) {
					return false, nil
				}
				utils.Logf("Validating health probe config ProbeProtocolHTTP")
				if !strings.EqualFold(string(*probe.Properties.Protocol), string(network.ProbeProtocolHTTP)) {
					return false, nil
				}
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("should generate health probe configs in multi-port scenario", func() {
		By("Creating a service with health probe annotations")
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerHealthProbeNumOfProbe:                                      "5",
			consts.BuildHealthProbeAnnotationKeyForPort(serverPort, consts.HealthProbeParamsNumOfProbe):    "3",
			consts.ServiceAnnotationLoadBalancerHealthProbeInterval:                                        "15",
			consts.BuildHealthProbeAnnotationKeyForPort(serverPort, consts.HealthProbeParamsProbeInterval): "10",
			consts.BuildHealthProbeAnnotationKeyForPort(serverPort, consts.HealthProbeParamsPort):          strconv.Itoa(alterNativeServicePort),
			consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:                                        "Http",
			consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath:                                     "/",
			consts.BuildAnnotationKeyForPort(alterNativeServicePort, consts.PortAnnotationNoLBRule):        "true",
		}
		ports = append(ports, v1.ServicePort{
			Name:       "port2",
			Port:       alterNativeServicePort,
			TargetPort: intstr.FromInt(serverPort),
		})

		// create service with given annotation and wait it to expose
		publicIPs := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			By("Cleaning up service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(len(publicIPs)).NotTo(BeZero())
		for _, publicIP := range publicIPs {
			pipFrontendConfigID := getPIPFrontendConfigurationID(tc, *publicIP, tc.GetResourceGroup(), false)
			pipFrontendConfigIDSplit := strings.Split(pipFrontendConfigID, "/")
			Expect(len(pipFrontendConfigIDSplit)).NotTo(Equal(0))
		}

		var lb *network.LoadBalancer
		var targetProbes []*network.Probe
		// There should be no other Services besides the one in this test or the check below will fail.
		expectedTargetProbesCount := 1
		if tc.IPFamily == utils.DualStack {
			expectedTargetProbesCount = 2
		}
		//wait for backend update
		err := wait.PollImmediate(5*time.Second, 60*time.Second, func() (bool, error) {
			lb = getAzureLoadBalancerFromPIP(tc, publicIPs[0], tc.GetResourceGroup(), "")
			targetProbes = []*network.Probe{}
			for i := range lb.Properties.Probes {
				probe := (lb.Properties.Probes)[i]
				utils.Logf("One probe of LB is %q", *probe.Name)
				targetProbes = append(targetProbes, probe)
			}
			return len(targetProbes) == expectedTargetProbesCount, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Validating health probe configs")
		var numberOfProbes *int32
		var intervalInSeconds *int32
		for _, probe := range targetProbes {
			if probe.Properties.ProbeThreshold != nil {
				numberOfProbes = probe.Properties.ProbeThreshold
			}
			if probe.Properties.IntervalInSeconds != nil {
				intervalInSeconds = probe.Properties.IntervalInSeconds
			}
		}
		utils.Logf("Validating health probe config numberOfProbes")
		Expect(*numberOfProbes).To(Equal(int32(3)))
		utils.Logf("Validating health probe config intervalInSeconds")
		Expect(*intervalInSeconds).To(Equal(int32(10)))
		utils.Logf("Validating health probe config protocol")
		Expect((len(targetProbes))).To(Equal(expectedTargetProbesCount))
		for _, targetProbe := range targetProbes {
			Expect(*targetProbe.Properties.Protocol).To(Equal(network.ProbeProtocolHTTP))
		}
	})

	It("should return error with invalid health probe config", func() {
		By("Creating a service with health probe annotations")
		invalidPort := 65536
		annotation := map[string]string{
			consts.BuildHealthProbeAnnotationKeyForPort(serverPort, consts.HealthProbeParamsPort): strconv.Itoa(invalidPort),
		}

		service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		defer func() {
			By("Cleaning up Service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		By("Check if correct error message are presented")
		err = wait.PollImmediate(time.Second, 2*time.Minute, func() (bool, error) {
			output, err := utils.RunKubectl(ns.Name, "describe", "service", serviceName)
			if err != nil {
				return false, err
			}
			errMsg := fmt.Sprintf("port_%d_health-probe_port: port %d is out of range", serverPort, invalidPort)
			return strings.Contains(output, errMsg), nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// Check if the following annotations are correctly set with Service LB IP
	// service.beta.kubernetes.io/azure-load-balancer-ipv4 or service.beta.kubernetes.io/azure-load-balancer-ipv6
	It("should support service annotation 'service.beta.kubernetes.io/azure-load-balancer-ip'", func() {
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		rg := tc.GetResourceGroup()

		annotation := map[string]string{}

		createPIPWithIPFamily := func(isIPv6 bool) (string, func()) {
			pipName := fmt.Sprintf("%s-public-IP%s", basename, string(uuid.NewUUID())[0:4])
			By(fmt.Sprintf("Creating a public IP %q, isIPv6: %v", pipName, isIPv6))
			pip := defaultPublicIPAddress(pipName, isIPv6)
			pip, err := utils.WaitCreatePIP(tc, pipName, rg, pip)
			Expect(err).NotTo(HaveOccurred())
			cleanup := func() {
				utils.Logf("Cleaning up public IP %q", pipName)
				err := utils.DeletePIPWithRetry(tc, pipName, rg)
				Expect(err).NotTo(HaveOccurred())
			}

			pipAddr := pointer.StringDeref(pip.Properties.IPAddress, "")
			utils.Logf("Created pip with address %s", pipAddr)
			annotation[consts.ServiceAnnotationLoadBalancerIPDualStack[isIPv6]] = pipAddr
			return pipAddr, cleanup
		}

		var pipAddrV4, pipAddrV6 string
		var cleanPIPV4, cleanPIPV6 func()
		if v4Enabled {
			pipAddrV4, cleanPIPV4 = createPIPWithIPFamily(false)
			defer cleanPIPV4()
		}
		if v6Enabled {
			pipAddrV6, cleanPIPV6 = createPIPWithIPFamily(true)
			defer cleanPIPV6()
		}

		By("Creating Services")
		ips := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			By("Cleaning up service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Check if the Service has the correct addresses")
		if v4Enabled {
			found := false
			for _, ip := range ips {
				if *ip == pipAddrV4 {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())
		}
		if v6Enabled {
			found := false
			for _, ip := range ips {
				if *ip == pipAddrV6 {
					found = true
					break
				}
			}
			Expect(found).To(BeTrue())
		}
	})
})

var _ = Describe("Multiple VMSS", Label(utils.TestSuiteLabelMultiNodePools, utils.TestSuiteLabelVMSS), func() {
	basename := "vmssservice"
	serviceName := "vmss-test"

	var (
		cs clientset.Interface
		tc *utils.AzureTestClient
		ns *v1.Namespace
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

		utils.Logf("Creating deployment " + serviceName)
		deployment := createServerDeploymentManifest(serviceName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating Azure clients")
		tc, err = utils.CreateAzureTestClient()
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

	It("should support service annotation `service.beta.kubernetes.io/azure-load-balancer-mode`", func() {
		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(network.LoadBalancerSKUNameStandard)) {
			Skip("service.beta.kubernetes.io/azure-load-balancer-mode only works for basic load balancer")
		}

		//get nodelist and providerID specific to an agentnodes
		By("Getting agent nodes list")
		nodes, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())

		// Get all the vmss names from all node's providerIDs
		By("Get vmss names from node providerIDs")
		vmssNames := sets.NewString()
		var resourceGroupName string
		for i, node := range nodes {
			if utils.IsControlPlaneNode(&nodes[i]) {
				continue
			}
			providerID := node.Spec.ProviderID
			matches := scalesetRE.FindStringSubmatch(providerID)
			if len(matches) != 3 {
				Skip("azure-load-balancer-mode tests only works for vmss cluster")
			}
			resourceGroupName = matches[1]
			vmssNames.Insert(matches[2])
		}
		Expect(resourceGroupName).NotTo(Equal(""))
		utils.Logf("Got vmss names %v", vmssNames.List())

		//Skip if there're less than two vmss
		if len(vmssNames) < 2 {
			Skip("azure-load-balancer-mode tests only works for cluster with multiple vmss agent pools")
		}

		vmssList := vmssNames.List()[:2]
		for _, vmss := range vmssList {
			validateLoadBalancerBackendPools(tc, vmss, cs, serviceName, labels, ns.Name, ports, resourceGroupName)
		}
	})
})

var _ = Describe("Multi-ports service", Label(utils.TestSuiteLabelMultiPorts), func() {
	basename := "mpservice"
	serviceName := "multiport-test"
	initSuccess := false

	var (
		cs clientset.Interface
		tc *utils.AzureTestClient
		ns *v1.Namespace
	)

	labels := map[string]string{
		"app": serviceName,
	}
	ports := []v1.ServicePort{{
		AppProtocol: pointer.String("Tcp"),
		Port:        serverPort,
		Name:        "port1",
		TargetPort:  intstr.FromInt(serverPort),
	}, {
		Port:        serverPort + 1,
		Name:        "port2",
		TargetPort:  intstr.FromInt(serverPort),
		AppProtocol: pointer.String("Tcp"),
	},
	}

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating Azure clients")
		tc, err = utils.CreateAzureTestClient()
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Creating deployment " + serviceName)
		deployment := createServerDeploymentManifest(serviceName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Waiting for backend pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		initSuccess = true
	})

	AfterEach(func() {
		if !initSuccess {
			// Get non-running Pods' describe info
			pods := []v1.Pod{}
			testPods, err := utils.GetPodList(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())
			pods = append(pods, testPods.Items...)
			ksPods, err := utils.GetPodList(cs, "kube-system")
			Expect(err).NotTo(HaveOccurred())
			pods = append(pods, ksPods.Items...)
			for _, pod := range pods {
				if pod.Status.Phase != v1.PodRunning {
					output, err := utils.RunKubectl(ns.Name, "describe", "pod", pod.Name)
					Expect(err).NotTo(HaveOccurred())
					utils.Logf("Describe info of Pod %q:\n%s", pod.Name, output)
				}
			}
		}

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
	Context("When ExternalTrafficPolicy is updated", func() {
		It("Should not have error occurred", func() {
			By("Getting the service")
			annotation := map[string]string{}
			utils.Logf("Creating service "+serviceName, ns.Name)
			service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
			service, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			utils.PrintCreateSVCSuccessfully(serviceName, ns.Name)

			//wait and get service's public IP Address
			utils.Logf("Waiting service to expose...")
			publicIPs, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, []*string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(publicIPs)).NotTo(BeZero())
			// create service with given annotation and wait it to expose

			defer func() {
				By("Cleaning up service")
				err := utils.DeleteService(cs, ns.Name, serviceName)
				Expect(err).NotTo(HaveOccurred())
				for _, ip := range publicIPs {
					By(fmt.Sprintf("Cleaning up public IP %q", *ip))
					pip, err := tc.GetPublicIPFromAddress(tc.GetResourceGroup(), ip)
					Expect(err).NotTo(HaveOccurred())
					if pip != nil {
						err = utils.DeletePIPWithRetry(tc, pointer.StringDeref(pip.Name, ""), tc.GetResourceGroup())
						Expect(err).NotTo(HaveOccurred())
					}
				}
			}()

			By("Changing ExternalTrafficPolicy of the service to Local")

			utils.Logf("Updating service "+serviceName, ns.Name)
			retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				service.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
				return err
			})
			Expect(retryErr).NotTo(HaveOccurred())
			utils.Logf("Successfully updated LoadBalancer service "+serviceName, ns.Name)

			By("Getting updated service object from server")
			retryErr = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if service.Spec.HealthCheckNodePort == 0 {
					return fmt.Errorf("service HealthCheckNodePort is not updated")
				}
				return nil
			})
			Expect(retryErr).NotTo(HaveOccurred())

			ids := []string{}
			for _, publicIP := range publicIPs {
				pipFrontendConfigID := getPIPFrontendConfigurationID(tc, *publicIP, tc.GetResourceGroup(), false)
				pipFrontendConfigIDSplit := strings.Split(pipFrontendConfigID, "/")
				Expect(len(pipFrontendConfigIDSplit)).NotTo(Equal(0))
				ids = append(ids, pipFrontendConfigIDSplit[len(pipFrontendConfigIDSplit)-1])
			}

			var lb *network.LoadBalancer
			var targetProbes []*network.Probe
			expectedTargetProbesCount := 1
			if tc.IPFamily == utils.DualStack {
				expectedTargetProbesCount = 2
			}
			//wait for backend update
			checkPort := func(port int32, targetProbes []*network.Probe) bool {
				utils.Logf("Checking port %d", port)
				match := true
				for _, targetProbe := range targetProbes {
					if *(targetProbe.Properties.Port) != port {
						match = false
						break
					}
				}
				utils.Logf("match %v", match)
				return match
			}
			err = wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
				lb = getAzureLoadBalancerFromPIP(tc, publicIPs[0], tc.GetResourceGroup(), "")
				targetProbes = []*network.Probe{}
				for i := range lb.Properties.Probes {
					probe := (lb.Properties.Probes)[i]
					utils.Logf("One probe of LB is %q", *probe.Name)
					probeSplit := strings.Split(*probe.Name, "-")
					Expect(len(probeSplit)).NotTo(Equal(0))
					for _, id := range ids {
						utils.Logf("ID %q", id)
						if id == probeSplit[0] {
							targetProbes = append(targetProbes, probe)
						}
					}
				}
				return len(targetProbes) == expectedTargetProbesCount && checkPort(service.Spec.HealthCheckNodePort, targetProbes), nil
			})
			Expect(err).NotTo(HaveOccurred())

			var nodeHealthCheckPort = service.Spec.HealthCheckNodePort
			By("Changing ExternalTrafficPolicy of the service to Cluster")
			utils.Logf("Updating service "+serviceName, ns.Name)
			retryErr = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
				if err != nil {
					return err
				}
				service.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
				return err
			})
			Expect(retryErr).NotTo(HaveOccurred())
			utils.Logf("Successfully updated LoadBalancer service "+serviceName, ns.Name)

			//wait for backend update
			expectedTargetProbesCount = 2
			if tc.IPFamily == utils.DualStack {
				expectedTargetProbesCount = 4
			}
			err = wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
				lb := getAzureLoadBalancerFromPIP(tc, publicIPs[0], tc.GetResourceGroup(), "")
				targetProbes = []*network.Probe{}
				for i := range lb.Properties.Probes {
					probe := (lb.Properties.Probes)[i]
					utils.Logf("One probe of LB is %q", *probe.Name)
					probeSplit := strings.Split(*probe.Name, "-")
					Expect(len(probeSplit)).NotTo(Equal(0))
					for _, id := range ids {
						utils.Logf("ID %q", id)
						if id == probeSplit[0] {
							targetProbes = append(targetProbes, probe)
						}
					}
				}
				for _, targetProbe := range targetProbes {
					if checkPort(nodeHealthCheckPort, []*network.Probe{targetProbe}) {
						return false, nil
					}
				}
				return len(targetProbes) == expectedTargetProbesCount, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

func waitComparePIPTags(tc *utils.AzureTestClient, expectedTags map[string]*string, pipName string) error {
	err := wait.PollImmediate(10*time.Second, 10*time.Minute, func() (done bool, err error) {
		pip, err := utils.WaitGetPIP(tc, pipName)
		if err != nil {
			return false, err
		}
		tags := pip.Tags
		delete(tags, consts.ClusterNameKey)
		delete(tags, consts.LegacyClusterNameKey)
		delete(tags, consts.ServiceTagKey)
		delete(tags, consts.LegacyServiceTagKey)
		for _, k := range utils.UnwantedTagKeys {
			delete(tags, k)
		}

		printTags := func(name string, ts map[string]*string) {
			msg := ""
			for t := range ts {
				msg += fmt.Sprintf("%s:%s ", t, *ts[t])
			}
			utils.Logf("%s: [%s]", name, msg)
		}
		printTags("tags", tags)
		printTags("expectedTags", expectedTags)
		return reflect.DeepEqual(tags, expectedTags), nil
	})
	return err
}

func ifPIPDNSLabelDeleted(tc *utils.AzureTestClient, pipName string) (bool, error) {
	if err := wait.PollImmediate(10*time.Second, 2*time.Minute, func() (done bool, err error) {
		pip, err := utils.WaitGetPIP(tc, pipName)
		if err != nil {
			return false, err
		}
		keyDeleted, legacyKeyDeleted := false, false
		if name, ok := pip.Tags[consts.ServiceUsingDNSKey]; !ok || name == nil {
			keyDeleted = true
		}
		if name, ok := pip.Tags[consts.LegacyServiceUsingDNSKey]; !ok || name == nil {
			legacyKeyDeleted = true
		}
		return keyDeleted && legacyKeyDeleted, nil
	}); err != nil {
		return false, err
	}
	return true, nil
}

func getPIPFrontendConfigurationID(tc *utils.AzureTestClient, pip, pipResourceGroup string, shouldWait bool) string {
	pipFrontendConfigurationID := getFrontendConfigurationIDFromPIP(tc, pip, pipResourceGroup)

	if shouldWait {
		err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 5*time.Minute, false, func(context context.Context) (done bool, err error) {
			pipFrontendConfigurationID = getFrontendConfigurationIDFromPIP(tc, pip, pipResourceGroup)
			return pipFrontendConfigurationID != "", nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	return pipFrontendConfigurationID
}

func getFrontendConfigurationIDFromPIP(tc *utils.AzureTestClient, pip, pipResourceGroup string) string {
	utils.Logf("Getting public IPs in the resourceGroup " + pipResourceGroup)
	pipList, err := tc.ListPublicIPs(pipResourceGroup)
	Expect(err).NotTo(HaveOccurred())

	utils.Logf("Getting public IP frontend configuration ID")
	var pipFrontendConfigurationID string
	for _, ip := range pipList {
		if ip.Properties != nil &&
			ip.Properties.IPAddress != nil &&
			ip.Properties.IPConfiguration != nil &&
			ip.Properties.IPConfiguration.ID != nil &&
			*ip.Properties.IPAddress == pip {
			ipConfig := pointer.StringDeref(ip.Properties.IPConfiguration.ID, "")
			utils.Logf("Found pip %q with ipConfig %q", pip, ipConfig)
			pipFrontendConfigurationID = ipConfig
			break
		}
	}
	utils.Logf("Successfully obtained PIP front config id: %v", pipFrontendConfigurationID)
	return pipFrontendConfigurationID
}

func getAzureLoadBalancerFromPIP(tc *utils.AzureTestClient, pip *string, pipResourceGroup, lbResourceGroup string) *network.LoadBalancer {
	pipFrontendConfigurationID := getPIPFrontendConfigurationID(tc, *pip, pipResourceGroup, true)
	Expect(pipFrontendConfigurationID).NotTo(Equal(""))

	utils.Logf("Getting loadBalancer name from pipFrontendConfigurationID")
	match := lbNameRE.FindStringSubmatch(pipFrontendConfigurationID)
	Expect(len(match)).To(Equal(2))
	loadBalancerName := match[1]
	Expect(loadBalancerName).NotTo(Equal(""))
	utils.Logf("Got loadBalancerName %q", loadBalancerName)

	utils.Logf("Getting loadBalancer")
	if lbResourceGroup == "" {
		lbResourceGroup = tc.GetResourceGroup()
	}
	lb, err := tc.GetLoadBalancer(lbResourceGroup, loadBalancerName)
	Expect(err).NotTo(HaveOccurred())
	Expect(lb.Properties.LoadBalancingRules).NotTo(BeNil())

	return lb
}

func createAndExposeDefaultServiceWithAnnotation(cs clientset.Interface, ipFamily utils.IPFamily, serviceName, nsName string, labels, annotation map[string]string, ports []v1.ServicePort, customizeFuncs ...func(*v1.Service) error) []*string {
	utils.Logf("Creating service "+serviceName, nsName)
	service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, nsName, ports)
	for _, customizeFunc := range customizeFuncs {
		err := customizeFunc(service)
		Expect(err).NotTo(HaveOccurred())
	}
	_, err := cs.CoreV1().Services(nsName).Create(context.TODO(), service, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	utils.PrintCreateSVCSuccessfully(serviceName, nsName)

	//wait and get service's IP Address
	utils.Logf("Waiting service to expose...")
	publicIPs, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ipFamily, nsName, serviceName, []*string{})
	Expect(err).NotTo(HaveOccurred())

	return publicIPs
}

// createNginxDeploymentManifest returns a default deployment
// running nginx image which exposes port 80
func createServerDeploymentManifest(name string, labels map[string]string) *appsv1.Deployment {
	tcpPort := int32(serverPort)
	return createDeploymentManifest(name, labels, &tcpPort, nil)
}

func createDeploymentManifest(name string, labels map[string]string, tcpPort, udpPort *int32) *appsv1.Deployment {
	var replicas int32 = 5
	args := []string{"netexec"}
	ports := []v1.ContainerPort{}
	if tcpPort != nil {
		args = append(args, fmt.Sprintf("--http-port=%d", *tcpPort))
		ports = append(ports, v1.ContainerPort{
			Protocol:      v1.ProtocolTCP,
			ContainerPort: *tcpPort,
		})
	}
	if udpPort != nil {
		args = append(args, fmt.Sprintf("--udp-port=%d", *udpPort))
		ports = append(ports, v1.ContainerPort{
			Protocol:      v1.ProtocolUDP,
			ContainerPort: *udpPort,
		})
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: v1.PodSpec{
					Hostname: name,
					Containers: []v1.Container{
						{
							Name:            "test-app",
							Image:           utils.AgnhostImage,
							ImagePullPolicy: "Always",
							Args:            args,
							Ports:           ports,
						},
					},
				},
			},
		},
	}
}

func validateLoadBalancerBackendPools(tc *utils.AzureTestClient, vmssName string, cs clientset.Interface, serviceName string, labels map[string]string, ns string, ports []v1.ServicePort, resourceGroupName string) {
	serviceName = fmt.Sprintf("%s-%s", serviceName, vmssName)

	// create annotation for LoadBalancer service
	By("Creating service " + serviceName + " in namespace " + ns)
	annotation := map[string]string{
		consts.ServiceAnnotationLoadBalancerMode: vmssName,
	}
	service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns, ports)
	_, err := cs.CoreV1().Services(ns).Create(context.TODO(), service, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	utils.PrintCreateSVCSuccessfully(serviceName, ns)

	//wait and get service's public IP Address
	By("Waiting for service exposure")
	publicIPs, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns, serviceName, []*string{})
	Expect(err).NotTo(HaveOccurred())
	Expect(len(publicIPs)).NotTo(BeZero())
	publicIP := publicIPs[0]

	// Invoking azure network client to get list of public IP Addresses
	By("Getting public IPs in the resourceGroup " + resourceGroupName)
	pipList, err := tc.ListPublicIPs(resourceGroupName)
	Expect(err).NotTo(HaveOccurred())

	By("Getting public IP frontend configuration ID")
	var pipFrontendConfigurationID string
	for _, ip := range pipList {
		if ip.Properties != nil &&
			ip.Properties.IPAddress != nil &&
			ip.Properties.IPConfiguration != nil &&
			ip.Properties.IPConfiguration.ID != nil &&
			*ip.Properties.IPAddress == *publicIP {
			pipFrontendConfigurationID = *ip.Properties.IPConfiguration.ID
			break
		}
	}
	Expect(pipFrontendConfigurationID).NotTo(Equal(""))

	//Get Azure loadBalancer Name
	By("Getting loadBalancer name from pipFrontendConfigurationID")
	match := lbNameRE.FindStringSubmatch(pipFrontendConfigurationID)
	Expect(len(match)).To(Equal(2))
	loadBalancerName := match[1]
	Expect(loadBalancerName).NotTo(Equal(""))
	utils.Logf("Got loadBalancerName %q", loadBalancerName)

	//Get backendpools list
	By("Getting loadBalancer")
	lb, err := tc.GetLoadBalancer(resourceGroupName, loadBalancerName)
	Expect(err).NotTo(HaveOccurred())
	Expect(lb.Properties.BackendAddressPools).NotTo(BeNil())
	Expect(lb.Properties.LoadBalancingRules).NotTo(BeNil())

	if lb.SKU != nil && *lb.SKU.Name == network.LoadBalancerSKUNameStandard {
		Skip("azure-load-balancer-mode is not working for standard load balancer")
	}

	By("Getting loadBalancer backendPoolID")
	backendPoolID := ""
	for _, rule := range lb.Properties.LoadBalancingRules {
		if rule.Properties.FrontendIPConfiguration != nil &&
			rule.Properties.FrontendIPConfiguration.ID != nil &&
			strings.EqualFold(*rule.Properties.FrontendIPConfiguration.ID, pipFrontendConfigurationID) {
			Expect(rule.Properties.BackendAddressPool).NotTo(BeNil())
			Expect(rule.Properties.BackendAddressPool.ID).NotTo(BeNil())
			backendPoolID = *rule.Properties.BackendAddressPool.ID
		}
	}
	Expect(backendPoolID).NotTo(Equal(""))

	By("Validating loadBalancer backendPool")
	for _, pool := range lb.Properties.BackendAddressPools {
		if pool.ID == nil || pool.Properties.BackendIPConfigurations == nil || !strings.EqualFold(*pool.ID, backendPoolID) {
			continue
		}

		for _, ipConfig := range pool.Properties.BackendIPConfigurations {
			if ipConfig.ID == nil {
				continue
			}

			matches := backendIPConfigurationRE.FindStringSubmatch(*ipConfig.ID)
			Expect(len(matches)).To(Equal(2))
			Expect(matches[1]).To(Equal(vmssName))
		}
	}
}

func testPIPTagAnnotationWithTags(
	cs clientset.Interface,
	tc *utils.AzureTestClient,
	ns *v1.Namespace,
	serviceName string,
	labels map[string]string,
	ports []v1.ServicePort,
	expectedTagsAfterUpdate map[string]*string,
) {
	By("Creating a service with custom tags")
	annotation := map[string]string{
		consts.ServiceAnnotationAzurePIPTags: "a=b,c= d,e =, =f",
	}
	service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
	service.GetAnnotations()
	_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())

	By("Waiting service to expose...")
	ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, []*string{})
	Expect(err).NotTo(HaveOccurred())

	defer func() {
		By("Cleaning up test service")
		err := utils.DeleteService(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())
	}()

	By("Checking tags on the corresponding public IP")
	expectedTags := map[string]*string{
		"a": pointer.String("b"),
		"c": pointer.String("d"),
		"e": pointer.String(""),
	}
	pips, err := tc.ListPublicIPs(tc.GetResourceGroup())
	Expect(err).NotTo(HaveOccurred())
	var targetPIPs []network.PublicIPAddress
	for _, pip := range pips {
		for _, ip := range ips {
			if strings.EqualFold(pointer.StringDeref(pip.Properties.IPAddress, ""), *ip) {
				targetPIPs = append(targetPIPs, *pip)
				err := waitComparePIPTags(tc, expectedTags, pointer.StringDeref(pip.Name, ""))
				Expect(err).NotTo(HaveOccurred())
				break
			}
		}
	}

	By("Updating annotation and check tags again")
	service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	service.Annotations = map[string]string{
		consts.ServiceAnnotationAzurePIPTags: "a=c,x=y",
	}
	_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
	for _, targetPIP := range targetPIPs {
		err = waitComparePIPTags(tc, expectedTagsAfterUpdate, pointer.StringDeref(targetPIP.Name, ""))
		Expect(err).NotTo(HaveOccurred())
	}
}
