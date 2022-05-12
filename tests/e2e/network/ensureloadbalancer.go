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
	"fmt"
	"os"
	"strings"
	"time"

	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-02-01/network"
	"github.com/Azure/go-autorest/autorest/to"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	testServiceName = "servicelb-test"
)

var (
	serviceAnnotationLoadBalancerInternalFalse = map[string]string{
		consts.ServiceAnnotationLoadBalancerInternal: "false",
	}
	serviceAnnotationLoadBalancerInternalTrue = map[string]string{
		consts.ServiceAnnotationLoadBalancerInternal: "true",
	}
)

var _ = Describe("Ensure LoadBalancer", func() {
	basename := "service-lb"

	var cs clientset.Interface
	var ns *v1.Namespace
	var tc *utils.AzureTestClient

	labels := map[string]string{
		"app": testServiceName,
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

		utils.Logf("Creating deployment " + testServiceName)
		deployment := createNginxDeploymentManifest(testServiceName, labels)
		_, err = cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deployment, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), testServiceName, metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
		tc = nil
	})

	It("should support mixed protocol services", func() {
		By("creating a mixed protocol service")
		mixedProtocolPorts := []v1.ServicePort{
			{
				Name:       "tcp",
				Port:       nginxPort,
				TargetPort: intstr.FromInt(nginxPort),
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
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, "")
		Expect(err).NotTo(HaveOccurred())

		By("checking load balancing rules")
		foundTCP, foundUDP := false, false
		lb := getAzureLoadBalancerFromPIP(tc, ip, tc.GetResourceGroup(), "")
		for _, rule := range *lb.LoadBalancingRules {
			switch {
			case strings.EqualFold(string(rule.Protocol), string(v1.ProtocolTCP)):
				if to.Int32(rule.FrontendPort) == nginxPort {
					foundTCP = true
				}
			case strings.EqualFold(string(rule.Protocol), string(v1.ProtocolUDP)):
				if to.Int32(rule.FrontendPort) == testingPort {
					foundUDP = true
				}
			}
		}
		Expect(foundTCP).To(BeTrue())
		Expect(foundUDP).To(BeTrue())
	})

	It("should support BYO public IP", func() {
		By("creating a public IP with tags")
		ipName := basename + "-public-IP" + string(uuid.NewUUID())[0:4]
		pip := defaultPublicIPAddress(ipName)
		expectedTags := map[string]*string{
			"foo": to.StringPtr("bar"),
		}
		pip.Tags = expectedTags
		pip, err := utils.WaitCreatePIP(tc, ipName, tc.GetResourceGroup(), pip)
		Expect(err).NotTo(HaveOccurred())
		Expect(pip.Tags).To(Equal(expectedTags))
		targetIP := to.String(pip.IPAddress)
		utils.Logf("created pip with address %s", targetIP)

		By("creating a service referencing the public IP")
		service := utils.CreateLoadBalancerServiceManifest(testServiceName, nil, labels, ns.Name, ports)
		service = updateServiceBalanceIP(service, false, targetIP)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(ip).To(Equal(targetIP))

		By("deleting the service")
		err = utils.DeleteService(cs, ns.Name, testServiceName)
		Expect(err).NotTo(HaveOccurred())

		By("test if the pip still exists")
		pip, err = utils.WaitGetPIP(tc, ipName)
		Expect(err).NotTo(HaveOccurred())

		By("test if the tags are changed")
		Expect(pip.Tags).To(Equal(expectedTags))

		By("cleaning up")
		err = utils.DeletePIPWithRetry(tc, ipName, "")
		Expect(err).NotTo(HaveOccurred())
	})

	// Public w/o IP -> Public w/ IP
	It("should support assigning to specific IP when updating public service", func() {
		ipName := basename + "-public-none-IP" + string(uuid.NewUUID())[0:4]

		service := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationLoadBalancerInternalFalse, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + testServiceName + " in namespace " + ns.Name)

		pip, err := utils.WaitCreatePIP(tc, ipName, tc.GetResourceGroup(), defaultPublicIPAddress(ipName))
		Expect(err).NotTo(HaveOccurred())
		targetIP := to.String(pip.IPAddress)
		utils.Logf("PIP to %s", targetIP)

		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeletePIPWithRetry(tc, ipName, "")
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Waiting for exposure of the original service without assigned lb IP")
		ip1, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, "")
		Expect(err).NotTo(HaveOccurred())

		Expect(ip1).NotTo(Equal(targetIP))

		By("Updating service to bound to specific public IP")
		utils.Logf("will update IP to %s", targetIP)
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		service = updateServiceBalanceIP(service, false, targetIP)

		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, targetIP)
		Expect(err).NotTo(HaveOccurred())
		Expect(ip).To(Equal(targetIP))
	})

	// Internal w/ IP -> Internal w/ IP
	It("should support updating internal IP when updating internal service", func() {
		ip1, err := utils.SelectAvailablePrivateIP(tc)
		Expect(err).NotTo(HaveOccurred())

		service := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, ports)
		service = updateServiceBalanceIP(service, true, ip1)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + testServiceName + " in namespace " + ns.Name)

		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		// Create host exec Pod
		result, err := utils.CreateHostExecPod(cs, ns.Name, utils.ExecAgnhostPod)
		Expect(result).To(BeTrue())
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			err = utils.DeletePod(cs, ns.Name, utils.ExecAgnhostPod)
			Expect(err).NotTo(HaveOccurred())
		}()
		By("Waiting for exposure of internal service with specific IP")
		ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, ip1)
		Expect(err).NotTo(HaveOccurred())
		Expect(ip).To(Equal(ip1))
		list, errList := cs.CoreV1().Events(ns.Name).List(context.TODO(), metav1.ListOptions{})
		Expect(errList).NotTo(HaveOccurred())
		utils.Logf("Events list:")
		for i, event := range list.Items {
			utils.Logf("%d. %v", i, event)
		}

		ip2, err := utils.SelectAvailablePrivateIP(tc)
		Expect(err).NotTo(HaveOccurred())

		By("Updating internal service private IP")
		utils.Logf("will update IP to %s", ip2)
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		service = updateServiceBalanceIP(service, true, ip2)
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		ip, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, ip2)
		Expect(err).NotTo(HaveOccurred())
		Expect(ip).To(Equal(ip2))
	})

	// internal w/o IP -> public w/ IP
	It("should support updating an internal service to a public service with assigned IP", func() {
		ipName := basename + "-internal-none-public-IP" + string(uuid.NewUUID())[0:4]

		service := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + testServiceName + " in namespace " + ns.Name)

		pip, err := utils.WaitCreatePIP(tc, ipName, tc.GetResourceGroup(), defaultPublicIPAddress(ipName))
		Expect(err).NotTo(HaveOccurred())
		targetIP := to.String(pip.IPAddress)

		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeletePIPWithRetry(tc, ipName, "")
			Expect(err).NotTo(HaveOccurred())
		}()

		// Create host exec Pod
		result, err := utils.CreateHostExecPod(cs, ns.Name, utils.ExecAgnhostPod)
		Expect(result).To(BeTrue())
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			err = utils.DeletePod(cs, ns.Name, utils.ExecAgnhostPod)
			Expect(err).NotTo(HaveOccurred())
		}()
		By("Waiting for exposure of the original service without assigned lb private IP")
		ip1, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(ip1).NotTo(Equal(targetIP))
		list, errList := cs.CoreV1().Events(ns.Name).List(context.TODO(), metav1.ListOptions{})
		Expect(errList).NotTo(HaveOccurred())
		utils.Logf("Events list:")
		for i, event := range list.Items {
			utils.Logf("%d. %v", i, event)
		}

		By("Updating service to bound to specific public IP")
		utils.Logf("will update IP to %s, %v", targetIP, len(targetIP))
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		service = updateServiceBalanceIP(service, false, targetIP)

		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, targetIP)
		Expect(err).NotTo(HaveOccurred())
		Expect(ip).To(Equal(targetIP))
	})

	// internal w/o IP -> public w/o IP
	// This test is to replace an upstream k/k one because there's a bug:
	// https://github.com/kubernetes/kubernetes/blob/373c08e0c7873a76cecde1d6d714cc2ff7af0c9a/test/e2e/network/loadbalancer.go#L574
	// https://github.com/kubernetes/kubernetes/pull/109413
	It("should support updating an internal Service to a public one", func() {
		By("Creating an internal Service")
		service := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service %s in namespace %s", testServiceName, ns.Name)
		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		// Create host exec Pod
		result, err := utils.CreateHostExecPod(cs, ns.Name, utils.ExecAgnhostPod)
		Expect(result).To(BeTrue())
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			err = utils.DeletePod(cs, ns.Name, utils.ExecAgnhostPod)
			Expect(err).NotTo(HaveOccurred())
		}()
		By("Waiting for exposure of the internal Service")
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, "")
		Expect(err).NotTo(HaveOccurred())
		list, errList := cs.CoreV1().Events(ns.Name).List(context.TODO(), metav1.ListOptions{})
		Expect(errList).NotTo(HaveOccurred())
		utils.Logf("Events list:")
		for i, event := range list.Items {
			utils.Logf("%d. %v", i, event)
		}

		By("Updating the Service to public")
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		service = updateServiceBalanceIP(service, false, "")

		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Expect the Service IP to be a public one")
		var targetIP string
		err = wait.PollImmediate(10*time.Second, 5*time.Minute, func() (bool, error) {
			svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			targetIP = svc.Status.LoadBalancer.Ingress[0].IP
			if utils.IsInternalEndpoint(targetIP) {
				utils.Logf("expected IP is public, current IP is internal, retry in 10 seconds")
				return false, nil
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, targetIP)
		Expect(err).NotTo(HaveOccurred())
	})

	// public w/o IP -> internal w/ IP
	// This test is to replace an upstream k/k one because there's a bug:
	// https://github.com/kubernetes/kubernetes/blob/373c08e0c7873a76cecde1d6d714cc2ff7af0c9a/test/e2e/network/loadbalancer.go#L574
	// https://github.com/kubernetes/kubernetes/pull/109413
	It("should support updating a public Service to an internal one with specific IP", func() {
		service := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationLoadBalancerInternalFalse, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service %s in namespace %s", testServiceName, ns.Name)
		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
		}()

		// Create host exec Pod
		result, err := utils.CreateHostExecPod(cs, ns.Name, utils.ExecAgnhostPod)
		Expect(result).To(BeTrue())
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			err = utils.DeletePod(cs, ns.Name, utils.ExecAgnhostPod)
			Expect(err).NotTo(HaveOccurred())
		}()
		By("Waiting for exposure of a public Service")
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, "")
		Expect(err).NotTo(HaveOccurred())
		list, errList := cs.CoreV1().Events(ns.Name).List(context.TODO(), metav1.ListOptions{})
		Expect(errList).NotTo(HaveOccurred())
		utils.Logf("Events list:")
		for i, event := range list.Items {
			utils.Logf("%d. %v", i, event)
		}

		internalIP, err := utils.SelectAvailablePrivateIP(tc)
		Expect(err).NotTo(HaveOccurred())

		By("Updating the public service to an internal one with an IP")
		utils.Logf("will update IP to %s", internalIP)
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		service = updateServiceBalanceIP(service, true, internalIP)
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, internalIP)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should have no operation since no change in service when update [Slow]", func() {
		suffix := string(uuid.NewUUID())[0:4]
		ipName := basename + "-public-remain" + suffix
		pip, err := utils.WaitCreatePIP(tc, ipName, tc.GetResourceGroup(), defaultPublicIPAddress(ipName))
		Expect(err).NotTo(HaveOccurred())
		targetIP := to.String(pip.IPAddress)

		service := utils.CreateLoadBalancerServiceManifest(testServiceName, serviceAnnotationLoadBalancerInternalFalse, labels, ns.Name, ports)
		service = updateServiceBalanceIP(service, false, targetIP)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service %s in namespace %s", testServiceName, ns.Name)

		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, testServiceName)
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeletePIPWithRetry(tc, ipName, "")
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Waiting for exposure of the original service with assigned lb private IP")
		targetIP, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, targetIP)
		Expect(err).NotTo(HaveOccurred())

		By("Update without changing the service and wait for a while")
		utils.Logf("External IP is now %s", targetIP)
		service, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), testServiceName, metav1.GetOptions{})
		service.Annotations[consts.ServiceAnnotationDNSLabelName] = "testlabel" + suffix
		utils.Logf(service.Annotations[consts.ServiceAnnotationDNSLabelName])
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Wait for 5 minutes, there should return timeout err, since external ip should not change
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
				err = fmt.Errorf("Cannot find Ingress in limited time")
				utils.Logf("Fail to get ingress, retry it in %s seconds", 10)
				return false, nil
			}
			if targetIP == ingressList[0].IP {
				utils.Logf("External IP is still %s", targetIP)
				return false, nil
			}
			utils.Logf("succeeded")
			return true, nil
		})
		Expect(err).To(Equal(wait.ErrWaitTimeout))
	})

	It("should support multiple external services sharing one preset public IP address", func() {
		ipName := basename + "-public-remain" + string(uuid.NewUUID())[0:4]
		pip, err := utils.WaitCreatePIP(tc, ipName, tc.GetResourceGroup(), defaultPublicIPAddress(ipName))
		Expect(err).NotTo(HaveOccurred())
		targetIP := to.String(pip.IPAddress)

		service1 := utils.CreateLoadBalancerServiceManifest("service1", nil, labels, ns.Name, ports)
		service1 = updateServiceBalanceIP(service1, false, targetIP)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service1, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, "service1", targetIP)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service1 in namespace %s with IP %s", ns.Name, ip)

		ports2 := []v1.ServicePort{{
			Port:       testingPort,
			TargetPort: intstr.FromInt(testingPort),
		}}
		service2 := utils.CreateLoadBalancerServiceManifest("service2", nil, labels, ns.Name, ports2)
		service2 = updateServiceBalanceIP(service2, false, targetIP)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ip, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, "service2", targetIP)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service2 in namespace %s with IP %s", ns.Name, ip)

		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, "service1")
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeleteService(cs, ns.Name, "service2")
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeletePIPWithRetry(tc, ipName, "")
			Expect(err).NotTo(HaveOccurred())
		}()
	})

	It("should support multiple external services sharing one newly created public IP address", func() {
		service1 := utils.CreateLoadBalancerServiceManifest("service1", nil, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service1, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, "service1", "")
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service1 in namespace %s with IP %s", ns.Name, ip)

		ports2 := []v1.ServicePort{{
			Port:       testingPort,
			TargetPort: intstr.FromInt(testingPort),
		}}
		service2 := utils.CreateLoadBalancerServiceManifest("service2", nil, labels, ns.Name, ports2)
		service2 = updateServiceBalanceIP(service2, false, ip)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, "service2", ip)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service2 in namespace %s with IP %s", ns.Name, ip)

		By("Deleting one service and check if the other service works well")
		err = utils.DeleteService(cs, ns.Name, "service1")
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, "service2", ip)
		Expect(err).NotTo(HaveOccurred())

		By("Deleting all services")
		err = utils.DeleteService(cs, ns.Name, "service2")
		Expect(err).NotTo(HaveOccurred())

		By("Checking if the public IP has been deleted")
		err = wait.PollImmediate(5*time.Second, 5*time.Minute, func() (bool, error) {
			pips, err := tc.ListPublicIPs(tc.GetResourceGroup())
			if err != nil {
				return false, err
			}

			for _, pip := range pips {
				if strings.EqualFold(*pip.IPAddress, ip) {
					utils.Logf("the public IP with address %s still exists", ip)
					return false, nil
				}
			}

			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("should support multiple internal services sharing one IP address", func() {
		service1 := utils.CreateLoadBalancerServiceManifest("service1", serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service1, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ip, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, "service1", "")
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service1 in namespace %s with IP %s", ns.Name, ip)

		ports2 := []v1.ServicePort{{
			Port:       testingPort,
			TargetPort: intstr.FromInt(testingPort),
		}}
		service2 := utils.CreateLoadBalancerServiceManifest("service2", serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, ports2)
		service2.Spec.LoadBalancerIP = ip
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, "service2", ip)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service2 in namespace %s with IP %s", ns.Name, ip)

		defer func() {
			By("Cleaning up")
			err = utils.DeleteService(cs, ns.Name, "service1")
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeleteService(cs, ns.Name, "service2")
			Expect(err).NotTo(HaveOccurred())
		}()
	})

	It("should support node label `node.kubernetes.io/exclude-from-external-load-balancers`", func() {
		label := "node.kubernetes.io/exclude-from-external-load-balancers"
		By("Checking the number of the node pools")
		nodes, err := utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		initNodepoolNodeMap := utils.GetNodepoolNodeMap(&nodes)
		if len(initNodepoolNodeMap) != 1 {
			Skip("single node pool is needed in this scenario")
		}

		By("Creating a service to trigger the LB reconcile")
		service := utils.CreateLoadBalancerServiceManifest(testServiceName, nil, labels, ns.Name, ports)
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		publicIP, err := utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, testServiceName, "")
		Expect(err).NotTo(HaveOccurred())

		By("Checking the initial node number in the LB backend pool")
		lb := getAzureLoadBalancerFromPIP(tc, publicIP, tc.GetResourceGroup(), "")
		lbBackendPoolIPConfigs := (*lb.BackendAddressPools)[0].BackendIPConfigurations
		nodes, err = utils.GetAgentNodes(cs)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(*lbBackendPoolIPConfigs)).To(Equal(len(nodes)))

		By("Labeling node")
		node, err := utils.LabelNode(cs, &nodes[0], label, false)
		Expect(err).NotTo(HaveOccurred())
		err = waitForNodesInLBBackendPool(tc, publicIP, len(nodes)-1)
		Expect(err).NotTo(HaveOccurred())

		By("Unlabeling node")
		node, err = utils.GetNode(cs, node.Name)
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.LabelNode(cs, node, label, true)
		Expect(err).NotTo(HaveOccurred())
		err = waitForNodesInLBBackendPool(tc, publicIP, len(nodes))
		Expect(err).NotTo(HaveOccurred())
	})
})

func waitForNodesInLBBackendPool(tc *utils.AzureTestClient, ip string, expectedNum int) error {
	return wait.PollImmediate(10*time.Second, 10*time.Minute, func() (done bool, err error) {
		lb := getAzureLoadBalancerFromPIP(tc, ip, tc.GetResourceGroup(), "")
		lbBackendPoolIPConfigs := (*lb.BackendAddressPools)[0].BackendIPConfigurations
		if len(*lbBackendPoolIPConfigs) == expectedNum {
			return true, nil
		}
		utils.Logf("Number of IP configs: %d in the LB backend pool, will retry soon", len(*lbBackendPoolIPConfigs))
		return false, nil
	})
}

func judgeInternal(service v1.Service) bool {
	return service.Annotations[consts.ServiceAnnotationLoadBalancerInternal] == "true"
}

func updateServiceBalanceIP(service *v1.Service, isInternal bool, ip string) (result *v1.Service) {
	result = service
	if result == nil {
		return
	}
	result.Spec.LoadBalancerIP = ip
	if judgeInternal(*service) == isInternal {
		return
	}
	if isInternal {
		result.Annotations[consts.ServiceAnnotationLoadBalancerInternal] = "true"
	} else {
		delete(result.Annotations, consts.ServiceAnnotationLoadBalancerInternal)
	}
	return
}

func defaultPublicIPAddress(ipName string) aznetwork.PublicIPAddress {
	// The default sku for LoadBalancer and PublicIP is basic.
	skuName := aznetwork.PublicIPAddressSkuNameBasic
	if skuEnv := os.Getenv(utils.LoadBalancerSkuEnv); skuEnv != "" {
		if strings.EqualFold(skuEnv, string(aznetwork.PublicIPAddressSkuNameStandard)) {
			skuName = aznetwork.PublicIPAddressSkuNameStandard
		}
	}
	return aznetwork.PublicIPAddress{
		Name:     to.StringPtr(ipName),
		Location: to.StringPtr(os.Getenv(utils.ClusterLocationEnv)),
		Sku: &aznetwork.PublicIPAddressSku{
			Name: skuName,
		},
		PublicIPAddressPropertiesFormat: &aznetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: aznetwork.IPAllocationMethodStatic,
		},
	}
}
