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
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	aznetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	testBaseName       = "service-lb"
	testServiceName    = "service-lb-test"
	testDeploymentName = "deployment-lb-test"
)

var (
	serviceAnnotationLoadBalancerInternalFalse = map[string]string{
		consts.ServiceAnnotationLoadBalancerInternal: "false",
	}
	serviceAnnotationLoadBalancerInternalTrue = map[string]string{
		consts.ServiceAnnotationLoadBalancerInternal: "true",
	}
	serviceAnnotationDisableLoadBalancerFloatingIP = map[string]string{
		consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true",
	}
)

var _ = Describe("Ensure LoadBalancer", Label(utils.TestSuiteLabelLB), func() {
	basename := testBaseName

	var cs clientset.Interface
	var ns *v1.Namespace
	var tc *utils.AzureTestClient
	var deployment *appsv1.Deployment

	labels := map[string]string{
		"app": testServiceName,
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

		utils.Logf("Creating deployment %s", testDeploymentName)
		deployment = createServerDeploymentManifest(testDeploymentName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), testDeploymentName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())
		}

		cs = nil
		ns = nil
		tc = nil
	})

	It("should support mixed protocol services", func() {
		utils.Logf("Updating deployment %s", testDeploymentName)
		tcpPort := int32(serverPort)
		udpPort := int32(testingPort)
		deployment := createDeploymentManifest(testDeploymentName, labels, &tcpPort, &udpPort)
		_, err := cs.AppsV1().Deployments(ns.Name).Update(context.TODO(), deployment, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("creating a mixed protocol service")
		mixedProtocolPorts := []v1.ServicePort{
			{
				Name:       "tcp",
				Port:       serverPort,
				TargetPort: intstr.FromInt(serverPort),
				Protocol:   v1.ProtocolTCP,
			},
			{
				Name:       "udp",
				Port:       testingPort,
				TargetPort: intstr.FromInt(testingPort),
				Protocol:   v1.ProtocolUDP,
			},
		}
		service := utils.CreateLoadBalancerServiceManifest(testServiceName, nil, labels, ns.Name, mixedProtocolPorts)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, []*string{})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(ips)).NotTo(BeZero())
		ip := ips[0]

		By("checking load balancing rules")
		foundTCP, foundUDP := false, false
		lb := getAzureLoadBalancerFromPIP(tc, ip, tc.GetResourceGroup(), "")
		for _, rule := range lb.Properties.LoadBalancingRules {
			switch {
			case strings.EqualFold(string(*rule.Properties.Protocol), string(v1.ProtocolTCP)):
				if pointer.Int32Deref(rule.Properties.FrontendPort, 0) == serverPort {
					foundTCP = true
				}
			case strings.EqualFold(string(*rule.Properties.Protocol), string(v1.ProtocolUDP)):
				if pointer.Int32Deref(rule.Properties.FrontendPort, 0) == testingPort {
					foundUDP = true
				}
			}
		}
		Expect(foundTCP).To(BeTrue())
		Expect(foundUDP).To(BeTrue())
	})

	It("should support BYO public IP", func() {
		By("creating a public IP with tags")
		ipNameBase := basename + "-public-IP" + string(uuid.NewUUID())[0:4]
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		expectedTags := map[string]*string{
			"foo": pointer.String("bar"),
		}
		pips := []*aznetwork.PublicIPAddress{}
		targetIPs := []*string{}
		ipNames := []string{}
		deleteFuncs := []func(){}

		createBYOPIP := func(isIPv6 bool) func() {
			ipName := utils.GetNameWithSuffix(ipNameBase, utils.Suffixes[isIPv6])
			ipNames = append(ipNames, ipName)
			pip := defaultPublicIPAddress(ipName, isIPv6)
			pip.Tags = expectedTags
			pips = append(pips, pip)
			pip, err := utils.WaitCreatePIP(tc, ipName, tc.GetResourceGroup(), pip)
			Expect(err).NotTo(HaveOccurred())
			deleteFunc := func() {
				err := utils.DeletePIPWithRetry(tc, ipName, tc.GetResourceGroup())
				Expect(err).To(BeNil())
			}
			Expect(pip.Tags).To(Equal(expectedTags))
			targetIPs = append(targetIPs, pip.Properties.IPAddress)
			utils.Logf("created pip with address %s", *pip.Properties.IPAddress)
			return deleteFunc
		}
		if v4Enabled {
			deleteFuncs = append(deleteFuncs, createBYOPIP(false))
		}
		if v6Enabled {
			deleteFuncs = append(deleteFuncs, createBYOPIP(true))
		}
		defer func() {
			for _, deleteFunc := range deleteFuncs {
				deleteFunc()
			}
		}()

		By("creating a service referencing the public IP")
		service := utils.CreateLoadBalancerServiceManifest(testServiceName, nil, labels, ns.Name, ports)
		service = updateServiceLBIPs(service, false, targetIPs)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, targetIPs)
		Expect(err).NotTo(HaveOccurred())

		By("deleting the service")
		err = utils.DeleteService(cs, ns.Name, testServiceName)
		Expect(err).NotTo(HaveOccurred())

		for _, ipName := range ipNames {
			By("test if the pip still exists")
			pip, err := utils.WaitGetPIP(tc, ipName)
			Expect(err).NotTo(HaveOccurred())

			By("test if the tags are changed")
			Expect(pip.Tags).To(Equal(expectedTags))
		}
	})

	// Public w/o IP -> Public w/ IP
	It("should support assigning to specific IP when updating public service", func() {
		ipNameBase := basename + "-public-none-IP" + string(uuid.NewUUID())[0:4]

		service := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationLoadBalancerInternalFalse, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.PrintCreateSVCSuccessfully(testServiceName, ns.Name)

		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		targetIPs := []*string{}
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
			By("Cleaning up Service")
			err = utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
			for _, deleteFunc := range deleteFuncs {
				deleteFunc()
			}
		}()

		By("Waiting for exposure of the original service without assigned lb IP")
		ips1, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, []*string{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.CompareStrings(ips1, targetIPs)).To(BeFalse())

		By("Updating service to bound to specific public IP")
		utils.Logf("will update IPs to %v", utils.StrPtrSliceToStrSlice(targetIPs))
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		service = updateServiceLBIPs(service, false, targetIPs)

		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, targetIPs)
		Expect(err).NotTo(HaveOccurred())
	})

	// Internal w/ IP -> Internal w/ IP
	It("should support updating internal IP when updating internal service", func() {
		ips1, err := utils.SelectAvailablePrivateIPs(tc)
		Expect(err).NotTo(HaveOccurred())

		service := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, ports)
		service = updateServiceLBIPs(service, true, ips1)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.PrintCreateSVCSuccessfully(testServiceName, ns.Name)

		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		By(fmt.Sprintf("Waiting for exposure of internal service with specific IPs %v", utils.StrPtrSliceToStrSlice(ips1)))
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, ips1)
		Expect(err).NotTo(HaveOccurred())
		list, errList := cs.CoreV1().Events(ns.Name).List(context.TODO(), metav1.ListOptions{})
		Expect(errList).NotTo(HaveOccurred())
		utils.Logf("Events list:")
		for i, event := range list.Items {
			utils.Logf("%d. %v", i, event)
		}

		ips2, err := utils.SelectAvailablePrivateIPs(tc)
		Expect(err).NotTo(HaveOccurred())

		By("Updating internal service private IP")
		utils.Logf("will update IPs to %v", utils.StrPtrSliceToStrSlice(ips2))
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		service = updateServiceLBIPs(service, true, ips2)
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, ips2)
		Expect(err).NotTo(HaveOccurred())
	})

	// internal w/o IP -> public w/ IP
	It("should support updating an internal service to a public service with assigned IP", func() {
		ipNameBase := basename + "-internal-none-public-IP" + string(uuid.NewUUID())[0:4]

		service := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.PrintCreateSVCSuccessfully(testServiceName, ns.Name)

		targetIPs := []*string{}
		deleteFuncs := []func(){}
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
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
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
			for _, deleteFunc := range deleteFuncs {
				deleteFunc()
			}
		}()

		By("Waiting for exposure of the original service without assigned lb private IP")
		ips1, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, []*string{})
		Expect(err).NotTo(HaveOccurred())
		Expect(utils.CompareStrings(ips1, targetIPs)).To(BeFalse())
		list, errList := cs.CoreV1().Events(ns.Name).List(context.TODO(), metav1.ListOptions{})
		Expect(errList).NotTo(HaveOccurred())
		utils.Logf("Events list:")
		for i, event := range list.Items {
			utils.Logf("%d. %v", i, event)
		}

		By("Updating service to bound to specific public IP")
		utils.Logf("will update IPs to %v", utils.StrPtrSliceToStrSlice(targetIPs))
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		service = updateServiceLBIPs(service, false, targetIPs)

		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, targetIPs)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should have no operation since no change in service when update", Label(utils.TestSuiteLabelSlow), func() {
		suffixBase := string(uuid.NewUUID())[0:4]
		ipNameBase := basename + "-public-remain" + suffixBase

		targetIPs := []*string{}
		deleteFuncs := []func(){}
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
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

		service := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationLoadBalancerInternalFalse, labels, ns.Name, ports)
		service = updateServiceLBIPs(service, false, targetIPs)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.PrintCreateSVCSuccessfully(testServiceName, ns.Name)

		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
			for _, deleteFunc := range deleteFuncs {
				deleteFunc()
			}
		}()

		By("Waiting for exposure of the original service with assigned lb private IP")
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, targetIPs)
		Expect(err).NotTo(HaveOccurred())

		By("Update without changing the service and wait for a while")
		utils.Logf("External IPs are now %q", targetIPs)
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		service.Annotations[consts.ServiceAnnotationDNSLabelName] = "testlabel" + suffixBase
		utils.Logf(service.Annotations[consts.ServiceAnnotationDNSLabelName])
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Wait for 5 minutes, there should return timeout err, since external ip should not change
		utils.Logf("External IPs should be %q", targetIPs)
		err = wait.PollImmediate(10*time.Second, 5*time.Minute, func() (bool, error) {
			service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
			if err != nil {
				if utils.IsRetryableAPIError(err) {
					return false, nil
				}
				return false, err
			}

			ingressList := service.Status.LoadBalancer.Ingress
			if len(ingressList) == 0 {
				err = fmt.Errorf("cannot find Ingress in limited time")
				utils.Logf("Fail to get ingress, retry it in %s seconds", 10)
				return false, nil
			}
			ingressIPs := []*string{}
			for _, ingress := range ingressList {
				ingress := ingress
				ingressIPs = append(ingressIPs, &ingress.IP)
			}
			if utils.CompareStrings(targetIPs, ingressIPs) {
				return false, nil
			}
			utils.Logf("External IPs changed unexpectedly to: %q", ingressIPs)
			return true, nil
		})
		Expect(err).To(Equal(wait.ErrWaitTimeout))
	})

	It("should support multiple external services sharing preset public IP addresses", func() {
		ipNameBase := fmt.Sprintf("%s-public-remain-%s", basename, string(uuid.NewUUID())[0:4])
		targetIPs := []*string{}
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		if v4Enabled {
			targetIP, deleteFunc := createPIP(tc, ipNameBase, false)
			targetIPs = append(targetIPs, &targetIP)
			defer deleteFunc()
		}
		if v6Enabled {
			targetIP, deleteFunc := createPIP(tc, ipNameBase, true)
			targetIPs = append(targetIPs, &targetIP)
			defer deleteFunc()
		}

		serviceNames := []string{}
		for i := 0; i < 3; i++ {
			serviceLabels := labels
			deploymentName := testDeploymentName
			tcpPort := int32(80 + i)
			if i != 0 {
				deploymentName = fmt.Sprintf("%s-%d", testDeploymentName, i)
				utils.Logf("Creating deployment %q", deploymentName)
				serviceLabels = map[string]string{
					"app": deploymentName,
				}
				deployment := createDeploymentManifest(deploymentName, serviceLabels, &tcpPort, nil)
				_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
				defer func() {
					err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), deploymentName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}()
				Expect(err).NotTo(HaveOccurred())
			}

			var service *v1.Service
			serviceName := fmt.Sprintf("%s-%d", testServiceName, i)
			utils.Logf("Creating Service %q", serviceName)
			serviceNames = append(serviceNames, serviceName)
			servicePort := []v1.ServicePort{{
				Port:       tcpPort,
				TargetPort: intstr.FromInt(int(tcpPort)),
			}}
			service = utils.CreateLoadBalancerServiceManifest(serviceName, nil, serviceLabels, ns.Name, servicePort)
			if i < 2 {
				service = updateServiceLBIPs(service, false, targetIPs)
			} else {
				var pipNames []string
				if v4Enabled {
					pipNames = append(pipNames, utils.GetNameWithSuffix(ipNameBase, utils.Suffixes[consts.IPVersionIPv4]))
				}
				if v6Enabled {
					pipNames = append(pipNames, utils.GetNameWithSuffix(ipNameBase, utils.Suffixes[consts.IPVersionIPv6]))
				}
				utils.Logf("update pip names %s", strings.Join(pipNames, ","))
				service = updateServicePIPNames(tc.IPFamily, service, pipNames)
			}
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			defer func() {
				err = utils.DeleteService(cs, ns.Name, serviceName)
				Expect(err).NotTo(HaveOccurred())
			}()
			Expect(err).NotTo(HaveOccurred())
		}

		for _, serviceName := range serviceNames {
			_, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, targetIPs)
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("Successfully created LoadBalancer Service %q in namespace %s with IPs: %v", serviceName, ns.Name, utils.StrPtrSliceToStrSlice(targetIPs))
		}
	})

	It("should support multiple external services sharing one newly created public IP addresses", func() {
		serviceCount := 3
		sharedIPs := []*string{}
		serviceNames := []string{}
		var serviceLabels map[string]string
		for i := 0; i < serviceCount; i++ {
			tcpPort := int32(80 + i)
			serviceLabels = labels
			deploymentName := testDeploymentName
			if i != 0 {
				deploymentName = fmt.Sprintf("%s-%d", testDeploymentName, i)
				utils.Logf("Creating deployment %s", deploymentName)
				serviceLabels = map[string]string{
					"app": deploymentName,
				}
				deployment := createDeploymentManifest(deploymentName, serviceLabels, &tcpPort, nil)
				_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
				defer func() {
					err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), deploymentName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}()
				Expect(err).NotTo(HaveOccurred())
			}

			serviceName := fmt.Sprintf("%s-%d", testServiceName, i)
			serviceNames = append(serviceNames, serviceName)
			servicePort := []v1.ServicePort{{
				Port:       tcpPort,
				TargetPort: intstr.FromInt(int(tcpPort)),
			}}
			service := utils.CreateLoadBalancerServiceManifest(serviceName, nil, serviceLabels, ns.Name, servicePort)
			if i < 2 {
				if len(sharedIPs) != 0 {
					service = updateServiceLBIPs(service, false, sharedIPs)
				}
			} else {
				var pipNames []string
				for _, sharedIP := range sharedIPs {
					pip, err := tc.GetPublicIPFromAddress(tc.GetResourceGroup(), sharedIP)
					Expect(err).NotTo(HaveOccurred())
					pipNames = append(pipNames, pointer.StringDeref(pip.Name, ""))
				}
				utils.Logf("update pip names %s", strings.Join(pipNames, ","))
				service = updateServicePIPNames(tc.IPFamily, service, pipNames)
			}
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			// No need to defer delete Services here
			ips, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
			if len(sharedIPs) == 0 {
				sharedIPs = ips
			}
		}

		for _, serviceName := range serviceNames {
			_, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, sharedIPs)
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("Successfully created LoadBalancer Service %q in namespace %q with IPs %v", serviceName, ns.Name, utils.StrPtrSliceToStrSlice(sharedIPs))
		}

		By("Deleting one Service and check if the other service works well")
		if len(serviceNames) < 2 {
			Skip("At least 2 Services are needed in this scenario")
		}
		err := utils.DeleteService(cs, ns.Name, serviceNames[0])
		Expect(err).NotTo(HaveOccurred())

		for i := 1; i < serviceCount; i++ {
			serviceName := serviceNames[i]
			_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, sharedIPs)
			Expect(err).NotTo(HaveOccurred())
		}

		By("Deleting all remaining services")
		for i := 1; i < serviceCount; i++ {
			serviceName := serviceNames[i]
			err = utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}

		By("Checking if the public IP has been deleted")
		sharedIPsMap := map[string]bool{}
		for _, sharedIP := range sharedIPs {
			Expect(len(*sharedIP)).NotTo(BeZero())
			sharedIPsMap[*sharedIP] = true
		}
		err = wait.PollImmediate(5*time.Second, 5*time.Minute, func() (bool, error) {
			pips, err := tc.ListPublicIPs(tc.GetResourceGroup())
			if err != nil {
				return false, err
			}

			for _, pip := range pips {
				if _, ok := sharedIPsMap[pointer.StringDeref(pip.Properties.IPAddress, "")]; ok {
					utils.Logf("the public IP with address %s still exists", pointer.StringDeref(pip.Properties.IPAddress, ""))
					return false, nil
				}
			}

			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("should support multiple internal services sharing IP addresses", func() {
		sharedIPs := []*string{}
		serviceNames := []string{}
		for i := 0; i < 2; i++ {
			serviceLabels := labels
			deploymentName := testDeploymentName
			tcpPort := int32(80 + i)
			if i != 0 {
				deploymentName = fmt.Sprintf("%s-%d", testDeploymentName, i)
				utils.Logf("Creating deployment %s", deploymentName)
				serviceLabels = map[string]string{
					"app": deploymentName,
				}
				deployment := createDeploymentManifest(deploymentName, serviceLabels, &tcpPort, nil)
				_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
				defer func() {
					err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), deploymentName, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}()
				Expect(err).NotTo(HaveOccurred())
			}

			serviceName := fmt.Sprintf("%s-%d", testServiceName, i)
			serviceNames = append(serviceNames, serviceName)
			servicePort := []v1.ServicePort{{
				Port:       tcpPort,
				TargetPort: intstr.FromInt(int(tcpPort)),
			}}
			service := utils.CreateLoadBalancerServiceManifest(serviceName, serviceAnnotationLoadBalancerInternalTrue, serviceLabels, ns.Name, servicePort)
			if len(sharedIPs) != 0 {
				service = updateServiceLBIPs(service, true, sharedIPs)
			}
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			defer func() {
				err = utils.DeleteService(cs, ns.Name, serviceName)
				Expect(err).NotTo(HaveOccurred())
			}()
			Expect(err).NotTo(HaveOccurred())
			ips, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
			if len(sharedIPs) == 0 {
				sharedIPs = ips
			}
		}

		for _, serviceName := range serviceNames {
			_, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, sharedIPs)
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("Successfully created LoadBalancer Service %q in namespace %q with IPs %v", serviceName, ns.Name, utils.StrPtrSliceToStrSlice(sharedIPs))
		}
	})

	It("should support node label `node.kubernetes.io/exclude-from-external-load-balancers`", Label(utils.TestSuiteLabelNonMultiSLB), func() {
		label := "node.kubernetes.io/exclude-from-external-load-balancers"
		By("Checking the number of the node pools")
		nodes, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		if os.Getenv(utils.AKSTestCCM) != "" {
			// AKS
			initNodepoolNodeMap := utils.GetNodepoolNodeMap(&nodes)
			if len(initNodepoolNodeMap) != 1 {
				Skip("single node pool is needed in this scenario")
			}
		}

		By("Creating a service to trigger the LB reconcile")
		service := utils.CreateLoadBalancerServiceManifest(testServiceName, nil, labels, ns.Name, ports)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		publicIPs, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, []*string{})
		defer func() {
			deleteSvcErr := utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(deleteSvcErr).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())
		Expect(len(publicIPs)).NotTo(BeZero())
		publicIP := publicIPs[0]

		By("Checking the initial node number in the LB backend pool")
		lb := getAzureLoadBalancerFromPIP(tc, publicIP, tc.GetResourceGroup(), "")
		if lb.SKU != nil && *lb.SKU.Name == aznetwork.LoadBalancerSKUNameBasic {
			// For a basic lb, not autoscaling pipeline
			idxes := getLBBackendPoolIndex(lb)
			Expect(idxes).NotTo(BeZero())
			ipConfigs := (lb.Properties.BackendAddressPools)[idxes[0]].Properties.BackendIPConfigurations
			Expect(ipConfigs).NotTo(BeNil())
			lbBackendPoolIPConfigCount := len(ipConfigs)
			Expect(lbBackendPoolIPConfigCount).To(Equal(len(nodes)))
			utils.Logf("Initial node number in the LB backend pool is %d", lbBackendPoolIPConfigCount)
		} else {
			// SLB: Here we use BackendPool IP instead of IP config because this works for both NIC based LB and IP based LB.
			lbBackendPoolIPCount := 0
			idxes := getLBBackendPoolIndex(lb)
			Expect(idxes).NotTo(BeZero())
			lbBackendPoolIPs := (lb.Properties.BackendAddressPools)[idxes[0]].Properties.LoadBalancerBackendAddresses
			Expect(lbBackendPoolIPs).NotTo(BeNil())
			if utils.IsAutoscalingAKSCluster() {
				for _, ip := range lbBackendPoolIPs {
					Expect(ip.Properties).NotTo(BeNil())
					Expect(ip.Properties.NetworkInterfaceIPConfiguration).NotTo(BeNil())
					ipConfigID := pointer.StringDeref(ip.Properties.NetworkInterfaceIPConfiguration.ID, "")
					if !strings.Contains(ipConfigID, utils.SystemPool) {
						lbBackendPoolIPCount++
					}
				}
			} else {
				lbBackendPoolIPCount = len(lbBackendPoolIPs)
			}
			Expect(lbBackendPoolIPCount).To(Equal(len(nodes)))
			utils.Logf("Initial node number in the LB backend pool is %d", lbBackendPoolIPCount)
		}
		nodeToLabel := nodes[0]

		By(fmt.Sprintf("Labeling node %q", nodeToLabel.Name))
		node, err := utils.LabelNode(cs, &nodeToLabel, label, false)
		defer func() {
			node, err = utils.GetNode(cs, node.Name)
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.LabelNode(cs, node, label, true)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())
		addDummyAnnotationWithServiceName(cs, ns.Name, testServiceName)
		var expectedCount int
		if len(nodes) == 1 {
			expectedCount = 1
		} else {
			expectedCount = len(nodes) - 1
		}

		err = waitForNodesInLBBackendPool(tc, publicIP, expectedCount)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Unlabeling node %q", nodeToLabel.Name))
		node, err = utils.GetNode(cs, node.Name)
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.LabelNode(cs, node, label, true)
		Expect(err).NotTo(HaveOccurred())
		addDummyAnnotationWithServiceName(cs, ns.Name, testServiceName)
		err = waitForNodesInLBBackendPool(tc, publicIP, len(nodes))
		Expect(err).NotTo(HaveOccurred())
	})

	It("should support disabling floating IP in load balancer rule with kubernetes service annotations", func() {
		ipNameBase := basename + "-public-IP" + string(uuid.NewUUID())[0:4]
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		targetIPs := []*string{}
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
			By("Clean up PIPs")
			for _, deleteFunc := range deleteFuncs {
				deleteFunc()
			}
		}()

		By("creating a service referencing the public IP")
		service := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationDisableLoadBalancerFloatingIP, labels, ns.Name, ports)
		service = updateServiceLBIPs(service, false, targetIPs)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, targetIPs)
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			By("Clean up Service")
			err := utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, targetIPs)
		Expect(err).NotTo(HaveOccurred())

		By("testing if floating IP disabled in load balancer rule")
		Expect(len(targetIPs)).NotTo(Equal(0))
		pipFrontendConfigID := getPIPFrontendConfigurationID(tc, *targetIPs[0], tc.GetResourceGroup(), false)
		pipFrontendConfigIDSplit := strings.Split(pipFrontendConfigID, "/")
		Expect(len(pipFrontendConfigIDSplit)).NotTo(Equal(0))
		configID := pipFrontendConfigIDSplit[len(pipFrontendConfigIDSplit)-1]
		utils.Logf("PIP frontend config ID %q", configID)

		lb := getAzureLoadBalancerFromPIP(tc, targetIPs[0], tc.GetResourceGroup(), "")
		found := false
		for _, lbRule := range lb.Properties.LoadBalancingRules {
			utils.Logf("Checking LB rule %q", *lbRule.Name)
			lbRuleSplit := strings.Split(*lbRule.Name, "-")
			Expect(len(lbRuleSplit)).NotTo(Equal(0))
			// id is like xxx or xxx-IPv6 and lbRuleSplit[0] is like xxx.
			if !strings.Contains(configID, lbRuleSplit[0]) {
				continue
			}
			utils.Logf("%q is the corresponding rule of the Service", *lbRule.Name)
			Expect(pointer.BoolDeref(lbRule.Properties.EnableFloatingIP, false)).To(BeFalse())
			found = true
			break
		}
		Expect(found).To(Equal(true))
	})
})

var _ = Describe("EnsureLoadBalancer should not update any resources when service config is not changed", Label(utils.TestSuiteLabelLB), func() {
	basename := testBaseName

	var cs clientset.Interface
	var ns *v1.Namespace
	var tc *utils.AzureTestClient
	var deployment *appsv1.Deployment

	labels := map[string]string{
		"app": testServiceName,
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

		utils.Logf("Creating deployment %s", testDeploymentName)
		deployment = createServerDeploymentManifest(testDeploymentName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), testDeploymentName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())
		}

		cs = nil
		ns = nil
		tc = nil
	})

	It("should respect service with various configurations", func() {
		By("Creating a service and expose it")
		serviceDomainNamePrefix := testServiceName + string(uuid.NewUUID())
		annotation := map[string]string{
			consts.ServiceAnnotationDNSLabelName:                       serviceDomainNamePrefix,
			consts.ServiceAnnotationLoadBalancerIdleTimeout:            "20",
			consts.ServiceAnnotationLoadBalancerHealthProbeProtocol:    "HTTP",
			consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath: "/healthtz",
			consts.ServiceAnnotationLoadBalancerHealthProbeInterval:    "10",
			consts.ServiceAnnotationLoadBalancerHealthProbeNumOfProbe:  "8",
		}

		if strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(aznetwork.LoadBalancerSKUNameStandard)) &&
			tc.IPFamily == utils.IPv4 {
			// Routing preference is only supported in standard public IPs
			annotation[consts.ServiceAnnotationIPTagsForPublicIP] = "RoutingPreference=Internet"
		}

		ips := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, testServiceName, ns.Name, labels, annotation, ports)
		Expect(len(ips)).NotTo(BeZero())
		ip := ips[0]
		service, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		defer func() {
			By("Cleaning up")
			err := utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		By("Update the service and without significant changes and compare etags")
		updateServiceAndCompareEtags(tc, cs, ns, service, ip, false)
	})

	It("should respect service with BYO public IP with various configurations", func() {
		By("Creating BYO public IPs")
		ipNameBase := basename + "-public-IP" + string(uuid.NewUUID())[0:4]
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		targetIPs := []*string{}
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
			By("Clean up PIPs")
			for _, deleteFunc := range deleteFuncs {
				deleteFunc()
			}
		}()

		customHealthProbeConfigPrefix := "service.beta.kubernetes.io/port_" + strconv.Itoa(int(ports[0].Port)) + "_health-probe_"
		By("Creating a service and expose it")
		annotation := map[string]string{
			consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges: "true",
			customHealthProbeConfigPrefix + "interval":                    "10",
			customHealthProbeConfigPrefix + "num-of-probe":                "6",
			customHealthProbeConfigPrefix + "request-path":                "/healthtz",
		}
		if tc.IPFamily == utils.DualStack {
			annotation[consts.ServiceAnnotationPIPNameDualStack[false]] = utils.GetNameWithSuffix(ipNameBase, utils.Suffixes[false])
			annotation[consts.ServiceAnnotationPIPNameDualStack[true]] = utils.GetNameWithSuffix(ipNameBase, utils.Suffixes[true])
		} else {
			annotation[consts.ServiceAnnotationPIPNameDualStack[false]] = utils.GetNameWithSuffix(ipNameBase, utils.Suffixes[tc.IPFamily == utils.IPv6])
		}

		service := utils.CreateLoadBalancerServiceManifest(testServiceName, annotation, labels, ns.Name, ports)
		service.Spec.LoadBalancerSourceRanges = []string{}
		if v4Enabled {
			service.Spec.LoadBalancerSourceRanges = append(service.Spec.LoadBalancerSourceRanges, "0.0.0.0/0")
		}
		if v6Enabled {
			service.Spec.LoadBalancerSourceRanges = append(service.Spec.LoadBalancerSourceRanges, "::/0")
		}
		service.Spec.SessionAffinity = "ClientIP"
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, targetIPs)
		Expect(err).NotTo(HaveOccurred())

		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		defer func() {
			By("Cleaning up")
			err := utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		By("Update the service and without significant changes and compare etags")
		Expect(len(targetIPs)).NotTo(BeZero())
		updateServiceAndCompareEtags(tc, cs, ns, service, targetIPs[0], false)
	})

	It("should respect service with BYO public IP prefix with various configurations", func() {
		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(aznetwork.LoadBalancerSKUNameStandard)) {
			Skip("pip-prefix-id only work with Standard Load Balancer")
		}

		annotation := map[string]string{
			consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true",
			consts.ServiceAnnotationSharedSecurityRule:            "true",
		}

		By("Creating BYO public IP prefixes")
		prefixNameBase := "prefix"
		v4Enabled, v6Enabled := utils.IfIPFamiliesEnabled(tc.IPFamily)
		createPIPPrefix := func(isIPv6 bool) func() {
			prefixName := utils.GetNameWithSuffix(prefixNameBase, utils.Suffixes[isIPv6])
			prefix, err := utils.WaitCreatePIPPrefix(tc, prefixName, tc.GetResourceGroup(), defaultPublicIPPrefix(prefixName, isIPv6))
			deleteFunc := func() {
				Expect(utils.DeletePIPPrefixWithRetry(tc, prefixName)).NotTo(HaveOccurred())
			}
			Expect(err).NotTo(HaveOccurred())

			if tc.IPFamily == utils.DualStack {
				annotation[consts.ServiceAnnotationPIPPrefixIDDualStack[isIPv6]] = pointer.StringDeref(prefix.ID, "")
			} else {
				annotation[consts.ServiceAnnotationPIPPrefixIDDualStack[false]] = pointer.StringDeref(prefix.ID, "")
			}
			return deleteFunc
		}
		deleteFuncs := []func(){}
		if v4Enabled {
			deleteFuncs = append(deleteFuncs, createPIPPrefix(false))
		}
		if v6Enabled {
			deleteFuncs = append(deleteFuncs, createPIPPrefix(true))
		}
		defer func() {
			for _, deleteFunc := range deleteFuncs {
				deleteFunc()
			}
		}()

		By("Creating a service and expose it")
		service := utils.CreateLoadBalancerServiceManifest(testServiceName, annotation, labels, ns.Name, ports)
		service.Spec.ExternalTrafficPolicy = "Local"
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, []*string{})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(ips)).NotTo(BeZero())
		ip := ips[0]

		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		defer func() {
			By("Cleaning up")
			err := utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		By("Update the service and without significant changes and compare etags")
		updateServiceAndCompareEtags(tc, cs, ns, service, ip, false)
	})

	It("should respect internal service with various configurations", func() {
		By("Creating a subnet for ilb frontend ip")
		subnetName := "testSubnet"
		subnet, isNew := createNewSubnet(tc, subnetName)
		Expect(pointer.StringDeref(subnet.Name, "")).To(Equal(subnetName))
		if isNew {
			defer func() {
				utils.Logf("cleaning up test subnet %s", subnetName)
				vNet, err := tc.GetClusterVirtualNetwork()
				Expect(err).NotTo(HaveOccurred())
				err = tc.DeleteSubnet(pointer.StringDeref(vNet.Name, ""), subnetName)
				Expect(err).NotTo(HaveOccurred())
			}()
		}

		By("Creating a service and expose it")
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal:                    "true",
			consts.ServiceAnnotationLoadBalancerInternalSubnet:              subnetName,
			consts.ServiceAnnotationLoadBalancerEnableHighAvailabilityPorts: "true",
		}
		ips := createAndExposeDefaultServiceWithAnnotation(cs, tc.IPFamily, testServiceName, ns.Name, labels, annotation, ports)
		service, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		defer func() {
			By("Cleaning up")
			err := utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())
		Expect(len(ips)).NotTo(BeZero())
		ip := ips[0]

		By("Update the service and without significant changes and compare etags")
		updateServiceAndCompareEtags(tc, cs, ns, service, ip, true)
	})
})

func addDummyAnnotationWithServiceName(cs clientset.Interface, namespace string, serviceName string) {
	service, err := cs.CoreV1().Services(namespace).Get(context.TODO(), serviceName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	addDummyAnnotationWithService(cs, namespace, service)
}

func addDummyAnnotationWithService(cs clientset.Interface, namespace string, service *v1.Service) {
	utils.Logf("Adding a dummy annotation to trigger Service reconciliation")
	Expect(service).NotTo(BeNil())
	annotation := service.GetAnnotations()
	if annotation == nil {
		annotation = make(map[string]string)
	}
	// e2e test should not have 100+ dummy annotations.
	for i := 0; i < 100; i++ {
		if _, ok := annotation["dummy-annotation"+strconv.Itoa(i)]; !ok {
			annotation["dummy-annotation"+strconv.Itoa(i)] = "dummy"
			break
		}
	}
	service = updateServiceAnnotation(service, annotation)
	utils.Logf("Service's annotations: %v", annotation)
	_, err := cs.CoreV1().Services(namespace).Update(context.TODO(), service, metav1.UpdateOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func updateServiceAndCompareEtags(tc *utils.AzureTestClient, cs clientset.Interface, ns *v1.Namespace, service *v1.Service, ip *string, isInternal bool) {
	utils.Logf("Retrieving etags from resources")
	lbEtag, nsgEtag, pipEtag := getResourceEtags(tc, ip, cloudprovider.DefaultLoadBalancerName(service), isInternal)

	addDummyAnnotationWithService(cs, ns.Name, service)
	ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, []*string{})
	Expect(err).NotTo(HaveOccurred())
	Expect(len(ips)).NotTo(BeZero())
	ip = ips[0]

	utils.Logf("Checking etags are not changed")
	newLbEtag, newNsgEtag, newPipEtag := getResourceEtags(tc, ip, cloudprovider.DefaultLoadBalancerName(service), isInternal)
	Expect(lbEtag).To(Equal(newLbEtag), "lb etag")
	Expect(nsgEtag).To(Equal(newNsgEtag), "nsg etag")
	Expect(pipEtag).To(Equal(newPipEtag), "pip etag")
}

func createNewSubnet(tc *utils.AzureTestClient, subnetName string) (*aznetwork.Subnet, bool) {
	vNet, err := tc.GetClusterVirtualNetwork()
	Expect(err).NotTo(HaveOccurred())

	var subnetToReturn *aznetwork.Subnet
	isNew := false
	for i := range vNet.Properties.Subnets {
		existingSubnet := (vNet.Properties.Subnets)[i]
		if *existingSubnet.Name == subnetName {
			By("Test subnet exists, skip creating")
			subnetToReturn = existingSubnet
			break
		}
	}

	if subnetToReturn == nil {
		By("Test subnet doesn't exist. Creating a new one...")
		isNew = true
		newSubnetCIDRs, err := utils.GetNextSubnetCIDRs(vNet, tc.IPFamily)
		Expect(err).NotTo(HaveOccurred())
		newSubnetCIDRStrs := []*string{}
		for _, newSubnetCIDR := range newSubnetCIDRs {
			newSubnetCIDRStrs = append(newSubnetCIDRStrs, to.Ptr(newSubnetCIDR.String()))
		}
		newSubnet, err := tc.CreateSubnet(vNet, &subnetName, newSubnetCIDRStrs, true)
		Expect(err).NotTo(HaveOccurred())
		subnetToReturn = newSubnet
	}

	return subnetToReturn, isNew
}

func getResourceEtags(tc *utils.AzureTestClient, ip *string, nsgRulePrefix string, internal bool) (lbEtag, nsgEtag, pipEtag string) {
	if internal {
		lbEtag = pointer.StringDeref(getAzureInternalLoadBalancerFromPrivateIP(tc, ip, "").Etag, "")
	} else {
		lbEtag = pointer.StringDeref(getAzureLoadBalancerFromPIP(tc, ip, tc.GetResourceGroup(), "").Etag, "")
	}

	nsgs, err := tc.GetClusterSecurityGroups()
	Expect(err).NotTo(HaveOccurred())
	for _, nsg := range nsgs {
		if nsg.Properties.SecurityRules == nil {
			continue
		}
		for _, securityRule := range nsg.Properties.SecurityRules {
			utils.Logf("Checking security rule %q", pointer.StringDeref(securityRule.Name, ""))
			if strings.HasPrefix(pointer.StringDeref(securityRule.Name, ""), nsgRulePrefix) {
				nsgEtag = pointer.StringDeref(nsg.Etag, "")
				break
			}
		}
	}

	if !internal {
		pip, err := tc.GetPublicIPFromAddress(tc.GetResourceGroup(), ip)
		Expect(err).NotTo(HaveOccurred())
		pipEtag = pointer.StringDeref(pip.Etag, "")
	}
	utils.Logf("Got resource etags: lbEtag: %s; nsgEtag: %s, pipEtag: %s", lbEtag, nsgEtag, pipEtag)
	return
}

func getAzureInternalLoadBalancerFromPrivateIP(tc *utils.AzureTestClient, ip *string, lbResourceGroup string) *aznetwork.LoadBalancer {
	if lbResourceGroup == "" {
		lbResourceGroup = tc.GetResourceGroup()
	}
	utils.Logf("Listing all LBs in the resourceGroup " + lbResourceGroup)
	lbList, err := tc.ListLoadBalancers(lbResourceGroup)
	Expect(err).NotTo(HaveOccurred())

	var ilb *aznetwork.LoadBalancer
	utils.Logf("Looking for internal load balancer frontend config ID with private ip as frontend")
	for i := range lbList {
		lb := lbList[i]
		for _, fipconfig := range lb.Properties.FrontendIPConfigurations {
			if fipconfig.Properties.PrivateIPAddress != nil &&
				*fipconfig.Properties.PrivateIPAddress == *ip {
				ilb = lb
				break
			}
		}
	}
	Expect(ilb).NotTo(BeNil())
	return ilb
}

func waitForNodesInLBBackendPool(tc *utils.AzureTestClient, ip *string, expectedNum int) error {
	return wait.PollImmediate(10*time.Second, 10*time.Minute, func() (done bool, err error) {
		lb := getAzureLoadBalancerFromPIP(tc, ip, tc.GetResourceGroup(), "")
		if lb.SKU != nil && *lb.SKU.Name == aznetwork.LoadBalancerSKUNameBasic {
			// basic lb
			idxes := getLBBackendPoolIndex(lb)
			if len(idxes) == 0 {
				return false, errors.New("no backend pool found")
			}
			failed := false
			for _, idx := range idxes {
				bp := (lb.Properties.BackendAddressPools)[idx]
				lbBackendPoolIPConfigs := bp.Properties.BackendIPConfigurations
				ipConfigNum := 0
				ipConfigIDs := []string{}
				if lbBackendPoolIPConfigs != nil {
					ipConfigNum = len(lbBackendPoolIPConfigs)
					for _, ipConfig := range lbBackendPoolIPConfigs {
						ipConfigIDs = append(ipConfigIDs, pointer.StringDeref(ipConfig.ID, ""))
					}
				}
				if expectedNum == ipConfigNum {
					utils.Logf("Number of IP configs in the LB backend pool %q matches expected number %d. Success", *bp.Name, expectedNum)
				} else {
					utils.Logf("Number of IP configs: %d in the LB backend pool %q, expected %d, IP config IDs %q, will retry soon", ipConfigNum, *bp.Name, expectedNum, ipConfigIDs)
					failed = true
				}
			}
			return !failed, nil
		}
		// SLB
		idxes := getLBBackendPoolIndex(lb)
		if len(idxes) == 0 {
			return false, errors.New("no backend pool found")
		}
		failed := false
		for _, idx := range idxes {
			bp := (lb.Properties.BackendAddressPools)[idx]
			lbBackendPoolIPs := bp.Properties.LoadBalancerBackendAddresses
			ipNum := 0
			ipConfigIDs := []string{}
			if lbBackendPoolIPs != nil {
				if utils.IsAutoscalingAKSCluster() {
					// Autoscaling tests don't include IP based LB.
					for _, ip := range lbBackendPoolIPs {
						if ip.Properties == nil ||
							ip.Properties.NetworkInterfaceIPConfiguration == nil {
							return false, fmt.Errorf("LB backendPool address's NIC IP config ID is nil")
						}
						ipConfigID := pointer.StringDeref(ip.Properties.NetworkInterfaceIPConfiguration.ID, "")
						if !strings.Contains(ipConfigID, utils.SystemPool) {
							ipNum++
							ipConfigIDs = append(ipConfigIDs, ipConfigID)
						}
					}
				} else {
					ipNum = len(lbBackendPoolIPs)
				}
			}
			if ipNum == expectedNum {
				utils.Logf("Number of IPs in the LB backend pool %q matches expected number %d. Success", *bp.Name, expectedNum)
			} else {
				utils.Logf("Number of IPs: %d in the LB backend pool %q, expected %d, IP config IDs %q, will retry soon", ipNum, *bp.Name, expectedNum, ipConfigIDs)
				failed = true
			}
		}
		return !failed, nil
	})
}

func judgeInternal(service v1.Service) bool {
	return service.Annotations[consts.ServiceAnnotationLoadBalancerInternal] == utils.TrueValue
}

func getLBBackendPoolIndex(lb *aznetwork.LoadBalancer) []int {
	idxes := []int{}
	for index, backendPool := range lb.Properties.BackendAddressPools {
		if !strings.Contains(strings.ToLower(*backendPool.Name), "outboundbackendpool") {
			idxes = append(idxes, index)
		}
	}
	return idxes
}

func updateServiceLBIPs(service *v1.Service, isInternal bool, ips []*string) (result *v1.Service) {
	result = service
	if result == nil {
		return
	}
	if result.Annotations == nil {
		result.Annotations = map[string]string{}
	}
	for _, ip := range ips {
		isIPv6 := net.ParseIP(*ip).To4() == nil
		result.Annotations[consts.ServiceAnnotationLoadBalancerIPDualStack[isIPv6]] = *ip
	}

	if judgeInternal(*service) == isInternal {
		return
	}
	if isInternal {
		result.Annotations[consts.ServiceAnnotationLoadBalancerInternal] = utils.TrueValue
	} else {
		delete(result.Annotations, consts.ServiceAnnotationLoadBalancerInternal)
	}
	return
}

func updateServicePIPNames(ipFamily utils.IPFamily, service *v1.Service, pipNames []string) *v1.Service {
	if service.Annotations == nil {
		service.Annotations = map[string]string{}
	}

	isDualStack := ipFamily == utils.DualStack
	for _, pipName := range pipNames {
		if !isDualStack || !strings.HasSuffix(pipName, "-IPv6") {
			service.Annotations[consts.ServiceAnnotationPIPNameDualStack[consts.IPVersionIPv4]] = pipName
		} else {
			service.Annotations[consts.ServiceAnnotationPIPNameDualStack[consts.IPVersionIPv6]] = pipName
		}
	}

	return service
}

func defaultPublicIPAddress(ipName string, isIPv6 bool) *aznetwork.PublicIPAddress {
	// The default sku for LoadBalancer and PublicIP is basic.
	skuName := aznetwork.PublicIPAddressSKUNameBasic
	if skuEnv := os.Getenv(utils.LoadBalancerSkuEnv); skuEnv != "" {
		if strings.EqualFold(skuEnv, string(aznetwork.PublicIPAddressSKUNameStandard)) {
			skuName = aznetwork.PublicIPAddressSKUNameStandard
		}
	}
	pip := &aznetwork.PublicIPAddress{
		Name:     pointer.String(ipName),
		Location: pointer.String(os.Getenv(utils.ClusterLocationEnv)),
		SKU: &aznetwork.PublicIPAddressSKU{
			Name: to.Ptr(skuName),
		},
		Properties: &aznetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(aznetwork.IPAllocationMethodStatic),
		},
	}
	if isIPv6 {
		pip.Properties.PublicIPAddressVersion = to.Ptr(aznetwork.IPVersionIPv6)
	}
	return pip
}

func defaultPublicIPPrefix(name string, isIPv6 bool) aznetwork.PublicIPPrefix {
	pipAddrVersion := aznetwork.IPVersionIPv4
	var prefixLen int32 = 28
	if isIPv6 {
		pipAddrVersion = aznetwork.IPVersionIPv6
		prefixLen = 124
	}
	return aznetwork.PublicIPPrefix{
		Name:     pointer.String(name),
		Location: pointer.String(os.Getenv(utils.ClusterLocationEnv)),
		SKU: &aznetwork.PublicIPPrefixSKU{
			Name: to.Ptr(aznetwork.PublicIPPrefixSKUNameStandard),
		},
		Properties: &aznetwork.PublicIPPrefixPropertiesFormat{
			PrefixLength:           pointer.Int32(prefixLen),
			PublicIPAddressVersion: to.Ptr(pipAddrVersion),
		},
	}
}

func createPIP(tc *utils.AzureTestClient, ipNameBase string, isIPv6 bool) (string, func()) {
	ipName := utils.GetNameWithSuffix(ipNameBase, utils.Suffixes[isIPv6])
	pip, err := utils.WaitCreatePIP(tc, ipName, tc.GetResourceGroup(), defaultPublicIPAddress(ipName, isIPv6))
	Expect(err).NotTo(HaveOccurred())
	targetIP := pointer.StringDeref(pip.Properties.IPAddress, "")
	utils.Logf("Created PIP to %s", targetIP)
	return targetIP, func() {
		By("Cleaning up PIP")
		err = utils.DeletePIPWithRetry(tc, ipName, "")
		Expect(err).NotTo(HaveOccurred())
	}
}
