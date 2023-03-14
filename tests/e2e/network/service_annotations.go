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

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"

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

		err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
		tc = nil
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-dns-label-name'", func() {
		// This test creates/deletes/updates some Services:
		// 1. Create a Service with managed PIP and check connectivity with DNS
		// 2. Delete the Service
		// 3. Create a Service with user assigned PIP
		// 4. Delete the Servcie and check tags
		// 5. Create a Service with different name
		// 6. Update the Service with new tag
		By("Create a Service with managed PIP")
		serviceDomainNamePrefix := fmt.Sprintf("%s-%s", serviceName, uuid.NewUUID())
		annotation := map[string]string{
			consts.ServiceAnnotationDNSLabelName: serviceDomainNamePrefix,
		}
		// create service with given annotation and wait it to expose
		_ = createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
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

		By("Create a PIP")
		ipName := fmt.Sprintf("%s-public-IP-%s", basename, uuid.NewUUID()[0:4])
		nsName := ns.Name
		rgName := tc.GetResourceGroup()
		pip, err := utils.WaitCreatePIP(tc, ipName, rgName, defaultPublicIPAddress(ipName, tc.IPFamily == utils.IPv6))
		defer func() {
			err := utils.DeletePIPWithRetry(tc, ipName, rgName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())
		targetIP := pointer.StringDeref(pip.IPAddress, "")
		pipName := pointer.StringDeref(pip.Name, "")
		utils.Logf("PIP %q to %q", pipName, targetIP)

		By("Create a Service which will be deleted with the PIP")
		oldServiceName := fmt.Sprintf("%s-old", serviceName)
		service := utils.CreateLoadBalancerServiceManifest(oldServiceName, annotation, labels, nsName, ports)
		service = updateServiceLBIP(service, false, targetIP)

		// create service with given annotation and wait it to expose
		_, err = cs.CoreV1().Services(nsName).Create(context.TODO(), service, metav1.CreateOptions{})
		defer func() {
			utils.Logf("Delete test Service %q", oldServiceName)
			err := utils.DeleteService(cs, ns.Name, oldServiceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, nsName, oldServiceName, targetIP)
		Expect(err).NotTo(HaveOccurred())

		By("Delete the old Service")
		err = utils.DeleteService(cs, ns.Name, oldServiceName)
		Expect(err).NotTo(HaveOccurred())

		By("Check if PIP DNS label is deleted")
		deleted, err := ifPIPDNSLabelDeleted(tc, pipName)
		Expect(err).NotTo(HaveOccurred())
		Expect(deleted).To(BeTrue())

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
		_ = createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
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

		var newSubnetCIDR *net.IPNet
		for _, existingSubnet := range *vNet.Subnets {
			if *existingSubnet.Name != subnetName {
				continue
			}
			utils.Logf("Test subnet have existed, skip creating")
			if existingSubnet.AddressPrefix != nil {
				// IPv4 picks the only AddressPrefix.
				_, newSubnetCIDR, err = net.ParseCIDR(*existingSubnet.AddressPrefix)
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(existingSubnet.AddressPrefixes).NotTo(BeNil(),
					"subnet AddressPrefix and AddressPrefixes shouldn't be both nil")

				addrPrefixPicked := false
				for _, addrPrefix := range *existingSubnet.AddressPrefixes {
					var parsedIP net.IP
					parsedIP, newSubnetCIDR, err = net.ParseCIDR(addrPrefix)
					Expect(err).NotTo(HaveOccurred(), "failed to parse CIDR %q", addrPrefix)
					if tc.IPFamily == utils.DualStack && parsedIP.To4() != nil {
						// Dual-stack picks IPv4 prefix.
						addrPrefixPicked = true
						break
					}
					if tc.IPFamily == utils.IPv6 && parsedIP.To4() == nil {
						// IPv6 picks IPv6 prefix.
						addrPrefixPicked = true
						break
					}
				}
				Expect(addrPrefixPicked).To(BeTrue(), "there's no matching AddressPrefixes")
			}
			break
		}

		if newSubnetCIDR == nil {
			By("Test subnet doesn't exist. Creating a new one...")
			newSubnetCIDR, err = utils.GetNextSubnetCIDR(vNet, tc.IPFamily)
			Expect(err).NotTo(HaveOccurred())
			newSubnetCIDRStr := newSubnetCIDR.String()
			By(fmt.Sprintf("Creating a subnet %q", newSubnetCIDRStr))
			_, err = tc.CreateSubnet(vNet, &subnetName, &newSubnetCIDRStr, false)
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
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Get External IP: %s", ip)

		By("Validating external ip in target subnet")
		contains := newSubnetCIDR.Contains(net.ParseIP(ip))
		Expect(contains).To(BeTrue(), "external ip %s is not in the target subnet %s", ip, newSubnetCIDR)
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-load-balancer-enable-high-availability-ports'", func() {
		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(network.PublicIPAddressSkuNameStandard)) {
			Skip("azure-load-balancer-enable-high-availability-ports only work with Standard Load Balancer")
		}

		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "true",
			consts.ServiceAnnotationLoadBalancerInternal:                    "true",
		}
		// create service with given annotation and wait it to expose
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		lb := getAzureInternalLoadBalancerFromPrivateIP(tc, ip, "")

		Expect(len(*lb.LoadBalancingRules)).To(Equal(1))
		rule := (*lb.LoadBalancingRules)[0]
		Expect(*rule.FrontendPort).To(Equal(int32(0)))
		Expect(*rule.BackendPort).To(Equal(int32(0)))
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-load-balancer-tcp-idle-timeout'", func() {
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerIdleTimeout: "5",
		}

		// create service with given annotation and wait it to expose
		publicIP := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			By("Cleaning up service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		// get lb from azure client
		lb := getAzureLoadBalancerFromPIP(tc, publicIP, tc.GetResourceGroup(), "")

		var idleTimeout *int32
		for _, rule := range *lb.LoadBalancingRules {
			if rule.IdleTimeoutInMinutes != nil {
				idleTimeout = rule.IdleTimeoutInMinutes
			}
		}
		Expect(*idleTimeout).To(Equal(int32(5)))
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-load-balancer-resource-group'", func() {
		By("creating a test resource group")
		rg, cleanup := utils.CreateTestResourceGroup(tc)
		defer cleanup(pointer.StringDeref(rg.Name, ""))

		By("creating test PIP in the test resource group")
		testPIPName := "testPIP-" + string(uuid.NewUUID())[0:4]
		pip, err := utils.WaitCreatePIP(tc, testPIPName, *rg.Name, defaultPublicIPAddress(testPIPName, tc.IPFamily == utils.IPv6))
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			utils.Logf("Cleaning up service and public IP")
			err = utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeletePIPWithRetry(tc, testPIPName, *rg.Name)
			Expect(err).NotTo(HaveOccurred())
		}()

		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerResourceGroup: pointer.StringDeref(rg.Name, ""),
		}
		By("Creating service " + serviceName + " in namespace " + ns.Name)
		service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
		service = updateServiceLBIP(service, false, *pip.IPAddress)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns.Name)

		//wait and get service's public IP Address
		By("Waiting service to expose...")
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, serviceName, pointer.StringDeref(pip.IPAddress, ""))
		Expect(err).NotTo(HaveOccurred())

		lb := getAzureLoadBalancerFromPIP(tc, *pip.IPAddress, *rg.Name, "")
		Expect(lb).NotTo(BeNil())
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-additional-public-ips`", func() {
		By("creating a public IP")
		ipName := basename + "-public-IP" + string(uuid.NewUUID())[0:4]
		pip := defaultPublicIPAddress(ipName, tc.IPFamily == utils.IPv6)
		pip, err := utils.WaitCreatePIP(tc, ipName, tc.GetResourceGroup(), pip)
		Expect(err).NotTo(HaveOccurred())
		additionalPIP := pointer.StringDeref(pip.IPAddress, "")
		utils.Logf("created pip with address %s", additionalPIP)

		By("Exposing service with additional pip")
		annotation := map[string]string{
			consts.ServiceAnnotationAdditionalPublicIPs: additionalPIP,
		}
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			By("cleaning up")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeletePIPWithRetry(tc, ipName, "")
			Expect(err).NotTo(HaveOccurred())
		}()

		err = wait.PollImmediate(10*time.Second, 2*time.Minute, func() (bool, error) {
			service, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			if err != nil {
				if utils.IsRetryableAPIError(err) {
					return false, nil
				}
				return false, err
			}

			utils.Logf("Checking additionalPIP in ingress")
			foundInIngress := false
			ingressList := service.Status.LoadBalancer.Ingress
			Expect(ingressList).NotTo(BeNil())
			for _, ingress := range ingressList {
				if additionalPIP == ingress.IP {
					foundInIngress = true
					break
				}
			}

			utils.Logf("Checking additionalPIP in nsg")
			ipList := []string{ip, additionalPIP}
			port := fmt.Sprintf("%d", serverPort)
			nsgs, err := tc.GetClusterSecurityGroups()
			if err != nil {
				return false, err
			}
			foundInNsg := validateSharedSecurityRuleExists(nsgs, ipList, port)

			return foundInIngress && foundInNsg, nil
		})
		Expect(err).NotTo(HaveOccurred())
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
		By("Creating two test pips")
		pipName1 := "pip1"
		pip1, err := utils.WaitCreatePIP(tc, pipName1, tc.GetResourceGroup(), defaultPublicIPAddress(pipName1, tc.IPFamily == utils.IPv6))
		defer func() {
			By("Cleaning up test PIP")
			err := utils.DeletePIPWithRetry(tc, pipName1, tc.GetResourceGroup())
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())
		pipName2 := "pip2"
		pip2, err := utils.WaitCreatePIP(tc, pipName2, tc.GetResourceGroup(), defaultPublicIPAddress(pipName2, tc.IPFamily == utils.IPv6))
		defer func() {
			By("Cleaning up test PIP")
			err := utils.DeletePIPWithRetry(tc, pipName2, tc.GetResourceGroup())
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		By("Creating a service referring to the first pip")
		annotation := map[string]string{
			consts.ServiceAnnotationPIPName: pipName1,
		}
		service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		defer func() {
			By("Cleaning up test service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the service to expose")
		ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, serviceName, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(ip).To(Equal(pointer.StringDeref(pip1.IPAddress, "")))

		By("Updating the service to refer to the second service")
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		service.Annotations[consts.ServiceAnnotationPIPName] = pipName2
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for service IP to be updated")
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, serviceName, pointer.StringDeref(pip2.IPAddress, ""))
		Expect(err).NotTo(HaveOccurred())
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-pip-prefix-id`", func() {
		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(network.PublicIPAddressSkuNameStandard)) {
			Skip("pip-prefix-id only work with Standard Load Balancer")
		}
		const (
			prefix1Name = "prefix1"
			prefix2Name = "prefix2"
		)

		By("Creating two test PIPPrefix")
		prefix1, err := utils.WaitCreatePIPPrefix(tc, prefix1Name, tc.GetResourceGroup(), defaultPublicIPPrefix(prefix1Name, tc.IPFamily == utils.IPv6))
		defer func() {
			By(fmt.Sprintf("Cleaning up pip-prefix: %s", prefix1Name))
			Expect(utils.DeletePIPPrefixWithRetry(tc, prefix1Name)).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		prefix2, err := utils.WaitCreatePIPPrefix(tc, prefix2Name, tc.GetResourceGroup(), defaultPublicIPPrefix(prefix2Name, tc.IPFamily == utils.IPv6))
		defer func() {
			By(fmt.Sprintf("Cleaning up pip-prefix: %s", prefix2Name))
			Expect(utils.DeletePIPPrefixWithRetry(tc, prefix2Name)).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		By("Creating a service referring to the prefix")
		{
			annotation := map[string]string{
				consts.ServiceAnnotationPIPPrefixID: pointer.StringDeref(prefix1.ID, ""),
			}
			service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
			_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		defer func() {
			By("Cleaning up test service")
			Expect(utils.DeleteService(cs, ns.Name, serviceName)).NotTo(HaveOccurred())
		}()

		By("Waiting for the service to expose")
		{
			ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, serviceName, "")
			Expect(err).NotTo(HaveOccurred())

			pip, err := utils.WaitGetPIPByPrefix(tc, prefix1Name, true)
			Expect(err).NotTo(HaveOccurred())

			Expect(pip.IPAddress).NotTo(BeNil())
			Expect(pip.PublicIPPrefix.ID).To(Equal(prefix1.ID))
			Expect(ip).To(Equal(pointer.StringDeref(pip.IPAddress, "")))
		}

		By("Updating the service to refer to the second prefix")
		{
			service, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			service.Annotations[consts.ServiceAnnotationPIPPrefixID] = pointer.StringDeref(prefix2.ID, "")
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		By("Waiting for service IP to be updated")
		{
			var pip network.PublicIPAddress

			// wait until ip created by prefix
			pip, err := utils.WaitGetPIPByPrefix(tc, prefix2Name, true)
			Expect(err).NotTo(HaveOccurred())

			Expect(pip.IPAddress).NotTo(BeNil())
			Expect(pip.PublicIPPrefix.ID).To(Equal(prefix2.ID))

			_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, serviceName, pointer.StringDeref(pip.IPAddress, ""))
			Expect(err).NotTo(HaveOccurred())
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
		publicIP := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			By("Cleaning up service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		pipFrontendConfigID := getPIPFrontendConfigurationID(tc, publicIP, tc.GetResourceGroup(), "")
		pipFrontendConfigIDSplit := strings.Split(pipFrontendConfigID, "/")
		Expect(len(pipFrontendConfigIDSplit)).NotTo(Equal(0))

		var lb *network.LoadBalancer
		var targetProbes []*network.Probe
		//wait for backend update
		err := wait.PollImmediate(5*time.Second, 60*time.Second, func() (bool, error) {
			lb = getAzureLoadBalancerFromPIP(tc, publicIP, tc.GetResourceGroup(), "")
			targetProbes = []*network.Probe{}
			for i := range *lb.LoadBalancerPropertiesFormat.Probes {
				probe := (*lb.LoadBalancerPropertiesFormat.Probes)[i]
				utils.Logf("One probe of LB is %q", *probe.Name)
				probeSplit := strings.Split(*probe.Name, "-")
				Expect(len(probeSplit)).NotTo(Equal(0))
				if pipFrontendConfigIDSplit[len(pipFrontendConfigIDSplit)-1] == probeSplit[0] {
					targetProbes = append(targetProbes, &probe)
				}
			}

			return len(targetProbes) == 1, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Validating health probe configs")
		var numberOfProbes *int32
		var intervalInSeconds *int32
		for _, probe := range targetProbes {
			if probe.NumberOfProbes != nil {
				numberOfProbes = probe.NumberOfProbes
			}
			if probe.IntervalInSeconds != nil {
				intervalInSeconds = probe.IntervalInSeconds
			}
		}
		utils.Logf("Validating health probe config numberOfProbes")
		Expect(*numberOfProbes).To(Equal(int32(3)))
		utils.Logf("Validating health probe config intervalInSeconds")
		Expect(*intervalInSeconds).To(Equal(int32(10)))
		utils.Logf("Validating health probe config protocol")
		Expect((len(targetProbes))).To(Equal(1))
		Expect(targetProbes[0].Protocol).To(Equal(network.ProbeProtocolHTTP))
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
		publicIP := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			By("Cleaning up service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		pipFrontendConfigID := getPIPFrontendConfigurationID(tc, publicIP, tc.GetResourceGroup(), "")
		pipFrontendConfigIDSplit := strings.Split(pipFrontendConfigID, "/")
		Expect(len(pipFrontendConfigIDSplit)).NotTo(Equal(0))

		var lb *network.LoadBalancer
		var targetProbes []*network.Probe
		//wait for backend update
		err := wait.PollImmediate(5*time.Second, 60*time.Second, func() (bool, error) {
			lb = getAzureLoadBalancerFromPIP(tc, publicIP, tc.GetResourceGroup(), "")
			targetProbes = []*network.Probe{}
			for i := range *lb.LoadBalancerPropertiesFormat.Probes {
				probe := (*lb.LoadBalancerPropertiesFormat.Probes)[i]
				utils.Logf("One probe of LB is %q", *probe.Name)
				targetProbes = append(targetProbes, &probe)
			}
			return len(targetProbes) == 1, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Validating health probe configs")
		var numberOfProbes *int32
		var intervalInSeconds *int32
		for _, probe := range targetProbes {
			if probe.NumberOfProbes != nil {
				numberOfProbes = probe.NumberOfProbes
			}
			if probe.IntervalInSeconds != nil {
				intervalInSeconds = probe.IntervalInSeconds
			}
		}
		utils.Logf("Validating health probe config numberOfProbes")
		Expect(*numberOfProbes).To(Equal(int32(3)))
		utils.Logf("Validating health probe config intervalInSeconds")
		Expect(*intervalInSeconds).To(Equal(int32(10)))
		utils.Logf("Validating health probe config protocol")
		Expect((len(targetProbes))).To(Equal(1))
		Expect(targetProbes[0].Protocol).To(Equal(network.ProbeProtocolHTTP))
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
		pipName := fmt.Sprintf("%s-public-IP%s", basename, string(uuid.NewUUID())[0:4])
		By(fmt.Sprintf("Creating a public IP %q", pipName))
		var pip network.PublicIPAddress
		// TODO: dual-stack support
		if tc.IPFamily != utils.DualStack {
			pip = defaultPublicIPAddress(pipName, tc.IPFamily == utils.IPv6)
		}
		rg := tc.GetResourceGroup()
		pip, err := utils.WaitCreatePIP(tc, pipName, rg, pip)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			utils.Logf("Cleaning up public IP")
			err = utils.DeletePIPWithRetry(tc, pipName, rg)
			Expect(err).NotTo(HaveOccurred())
		}()
		pipAddr := pointer.StringDeref(pip.IPAddress, "")
		utils.Logf("Created pip with address %s", pipAddr)

		annotation := map[string]string{}
		// TODO: dual-stack support
		if tc.IPFamily == utils.IPv4 {
			annotation[consts.ServiceAnnotationLoadBalancerIPDualStack[false]] = pipAddr
		} else if tc.IPFamily == utils.IPv6 {
			annotation[consts.ServiceAnnotationLoadBalancerIPDualStack[true]] = pipAddr
		}

		By("Creating a Service")
		publicIP := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			By("Cleaning up service")
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Check if the Service has the correct address")
		Expect(publicIP).To(Equal(pipAddr))
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
		err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
		tc = nil
	})

	It("should support service annotation `service.beta.kubernetes.io/azure-load-balancer-mode`", func() {
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

		err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
		tc = nil
	})
	Context("When ExternalTrafficPolicy is updated", func() {
		It("Should not have error occurred", func() {
			By("Getting the service")
			annotation := map[string]string{}
			utils.Logf("Creating service " + serviceName + " in namespace " + ns.Name)
			service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, ns.Name, ports)
			service, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns.Name)

			//wait and get service's public IP Address
			utils.Logf("Waiting service to expose...")
			publicIP, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, serviceName, "")
			Expect(err).NotTo(HaveOccurred())
			// create service with given annotation and wait it to expose

			defer func() {
				By("Cleaning up service and public IP")
				err := utils.DeleteService(cs, ns.Name, serviceName)
				Expect(err).NotTo(HaveOccurred())
				err = utils.DeletePIPWithRetry(tc, publicIP, tc.GetResourceGroup())
				Expect(err).NotTo(HaveOccurred())
			}()

			By("Changing ExternalTrafficPolicy of the service to Local")

			utils.Logf("Updating service " + serviceName + " in namespace " + ns.Name)
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
			utils.Logf("Successfully updated LoadBalancer service " + serviceName + " in namespace " + ns.Name)

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

			pipFrontendConfigID := getPIPFrontendConfigurationID(tc, publicIP, tc.GetResourceGroup(), "")
			pipFrontendConfigIDSplit := strings.Split(pipFrontendConfigID, "/")
			Expect(len(pipFrontendConfigIDSplit)).NotTo(Equal(0))

			var lb *network.LoadBalancer
			var targetProbes []*network.Probe
			//wait for backend update
			err = wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
				lb = getAzureLoadBalancerFromPIP(tc, publicIP, tc.GetResourceGroup(), "")
				targetProbes = []*network.Probe{}
				for i := range *lb.LoadBalancerPropertiesFormat.Probes {
					probe := (*lb.LoadBalancerPropertiesFormat.Probes)[i]
					utils.Logf("One probe of LB is %q", *probe.Name)
					probeSplit := strings.Split(*probe.Name, "-")
					Expect(len(probeSplit)).NotTo(Equal(0))
					if pipFrontendConfigIDSplit[len(pipFrontendConfigIDSplit)-1] == probeSplit[0] {
						targetProbes = append(targetProbes, &probe)
					}
				}
				return len(targetProbes) == 1 &&
					*(targetProbes)[0].Port == service.Spec.HealthCheckNodePort, nil
			})
			Expect(err).NotTo(HaveOccurred())

			var nodeHealthCheckPort = service.Spec.HealthCheckNodePort
			By("Changing ExternalTrafficPolicy of the service to Cluster")
			utils.Logf("Updating service " + serviceName + " in namespace " + ns.Name)
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
			utils.Logf("Successfully updated LoadBalancer service " + serviceName + " in namespace " + ns.Name)

			//wait for backend update
			err = wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
				lb := getAzureLoadBalancerFromPIP(tc, publicIP, tc.GetResourceGroup(), "")
				targetProbes = []*network.Probe{}
				for i := range *lb.LoadBalancerPropertiesFormat.Probes {
					probe := (*lb.LoadBalancerPropertiesFormat.Probes)[i]
					utils.Logf("One probe of LB is %q", *probe.Name)
					probeSplit := strings.Split(*probe.Name, "-")
					Expect(len(probeSplit)).NotTo(Equal(0))
					if pipFrontendConfigIDSplit[len(pipFrontendConfigIDSplit)-1] == probeSplit[0] {
						targetProbes = append(targetProbes, &probe)
					}
				}
				return len(targetProbes) == 2 &&
					*(targetProbes)[0].Port != nodeHealthCheckPort &&
					*(targetProbes)[1].Port != nodeHealthCheckPort, nil
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

func getPIPFrontendConfigurationID(tc *utils.AzureTestClient, pip, pipResourceGroup, lbResourceGroup string) string {
	utils.Logf("Getting public IPs in the resourceGroup " + pipResourceGroup)
	pipList, err := tc.ListPublicIPs(pipResourceGroup)
	Expect(err).NotTo(HaveOccurred())

	utils.Logf("Getting public IP frontend configuration ID")
	var pipFrontendConfigurationID string
	for _, ip := range pipList {
		if ip.PublicIPAddressPropertiesFormat != nil &&
			ip.PublicIPAddressPropertiesFormat.IPAddress != nil &&
			ip.PublicIPAddressPropertiesFormat.IPConfiguration != nil &&
			ip.PublicIPAddressPropertiesFormat.IPConfiguration.ID != nil &&
			*ip.PublicIPAddressPropertiesFormat.IPAddress == pip {
			pipFrontendConfigurationID = *ip.PublicIPAddressPropertiesFormat.IPConfiguration.ID
			break
		}
	}
	utils.Logf("Successfully obtained PIP front config id: %v", pipFrontendConfigurationID)
	return pipFrontendConfigurationID
}

func getAzureLoadBalancerFromPIP(tc *utils.AzureTestClient, pip, pipResourceGroup, lbResourceGroup string) *network.LoadBalancer {
	pipFrontendConfigurationID := getPIPFrontendConfigurationID(tc, pip, pipResourceGroup, lbResourceGroup)
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
	Expect(lb.LoadBalancingRules).NotTo(BeNil())

	return &lb
}

func createAndExposeDefaultServiceWithAnnotation(cs clientset.Interface, serviceName, nsName string, labels, annotation map[string]string, ports []v1.ServicePort) string {
	utils.Logf("Creating service " + serviceName + " in namespace " + nsName)
	service := utils.CreateLoadBalancerServiceManifest(serviceName, annotation, labels, nsName, ports)
	_, err := cs.CoreV1().Services(nsName).Create(context.TODO(), service, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + nsName)

	//wait and get service's IP Address
	utils.Logf("Waiting service to expose...")
	publicIP, err := utils.WaitServiceExposureAndValidateConnectivity(cs, nsName, serviceName, "")
	Expect(err).NotTo(HaveOccurred())

	return publicIP
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
							Image:           "registry.k8s.io/e2e-test-images/agnhost:2.36",
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
	utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns)

	//wait and get service's public IP Address
	By("Waiting for service exposure")
	publicIP, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns, serviceName, "")
	Expect(err).NotTo(HaveOccurred())

	// Invoking azure network client to get list of public IP Addresses
	By("Getting public IPs in the resourceGroup " + resourceGroupName)
	pipList, err := tc.ListPublicIPs(resourceGroupName)
	Expect(err).NotTo(HaveOccurred())

	By("Getting public IP frontend configuration ID")
	var pipFrontendConfigurationID string
	for _, ip := range pipList {
		if ip.PublicIPAddressPropertiesFormat != nil &&
			ip.PublicIPAddressPropertiesFormat.IPAddress != nil &&
			ip.PublicIPAddressPropertiesFormat.IPConfiguration != nil &&
			ip.PublicIPAddressPropertiesFormat.IPConfiguration.ID != nil &&
			*ip.PublicIPAddressPropertiesFormat.IPAddress == publicIP {
			pipFrontendConfigurationID = *ip.PublicIPAddressPropertiesFormat.IPConfiguration.ID
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
	Expect(lb.BackendAddressPools).NotTo(BeNil())
	Expect(lb.LoadBalancingRules).NotTo(BeNil())

	if lb.Sku != nil && lb.Sku.Name == network.LoadBalancerSkuNameStandard {
		Skip("azure-load-balancer-mode is not working for standard load balancer")
	}

	By("Getting loadBalancer backendPoolID")
	backendPoolID := ""
	for _, rule := range *lb.LoadBalancingRules {
		if rule.FrontendIPConfiguration != nil &&
			rule.FrontendIPConfiguration.ID != nil &&
			strings.EqualFold(*rule.FrontendIPConfiguration.ID, pipFrontendConfigurationID) {
			Expect(rule.BackendAddressPool).NotTo(BeNil())
			Expect(rule.BackendAddressPool.ID).NotTo(BeNil())
			backendPoolID = *rule.BackendAddressPool.ID
		}
	}
	Expect(backendPoolID).NotTo(Equal(""))

	By("Validating loadBalancer backendPool")
	for _, pool := range *lb.BackendAddressPools {
		if pool.ID == nil || pool.BackendIPConfigurations == nil || !strings.EqualFold(*pool.ID, backendPoolID) {
			continue
		}

		for _, ipConfig := range *pool.BackendIPConfigurations {
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
	ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, serviceName, "")
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
	var targetPIP network.PublicIPAddress
	for _, pip := range pips {
		if strings.EqualFold(pointer.StringDeref(pip.IPAddress, ""), ip) {
			targetPIP = pip
			err := waitComparePIPTags(tc, expectedTags, pointer.StringDeref(pip.Name, ""))
			Expect(err).NotTo(HaveOccurred())
			break
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
	err = waitComparePIPTags(tc, expectedTagsAfterUpdate, pointer.StringDeref(targetPIP.Name, ""))
	Expect(err).NotTo(HaveOccurred())
}
