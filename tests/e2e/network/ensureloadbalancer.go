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
	"os"
	"strings"

	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-07-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/cloud-provider-azure/tests/e2e/utils"
	"k8s.io/legacy-cloud-providers/azure"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = FDescribe("Ensure LoadBalancer", func() {
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
		tc = nil
	})

	// Public w/o IP -> Public w/ IP
	It("should support assigning to specific IP when updating public service", func() {
		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerInternal: "false",
		}
		ipName := basename + "-public-none-IP" + string(uuid.NewUUID())[0:4]

		service := createLoadBalancerServiceManifest(cs, serviceName, annotation, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(service)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns.Name)

		pip, err := utils.WaitCreatePIP(tc, ipName, defaultPublicIPAddress(ipName))
		Expect(err).NotTo(HaveOccurred())
		targetIP := to.String(pip.IPAddress)
		utils.Logf("PIP to %s", targetIP)

		defer func() {
			By("Cleaning up")
			err = cs.CoreV1().Services(ns.Name).Delete(serviceName, nil)
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeletePIPWithRetry(tc, ipName)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Waiting for exposure of the original service without assigned lb IP")
		ip1, err := utils.WaitServiceExposure(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())

		Expect(ip1).NotTo(Equal(targetIP))

		By("Updating service to bound to specific public IP")
		utils.Logf("will update IP to %s", targetIP)
		service, err = cs.CoreV1().Services(ns.Name).Get(serviceName, metav1.GetOptions{})
		service = updateServiceBalanceIP(service, false, targetIP)

		_, err = cs.CoreV1().Services(ns.Name).Update(service)
		Expect(err).NotTo(HaveOccurred())

		err = utils.WaitUpdateServiceExposure(cs, ns.Name, serviceName, targetIP, true)
		Expect(err).NotTo(HaveOccurred())
	})

	// Internal w/ IP -> Internal w/ IP
	It("should support updating internal IP when updating internal service", func() {
		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerInternal: "true",
		}
		ip1, err := utils.SelectAvailablePrivateIP(tc)
		Expect(err).NotTo(HaveOccurred())

		service := createLoadBalancerServiceManifest(cs, serviceName, annotation, labels, ns.Name, ports)
		service = updateServiceBalanceIP(service, true, ip1)
		_, err = cs.CoreV1().Services(ns.Name).Create(service)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns.Name)

		defer func() {
			By("Cleaning up")
			err = cs.CoreV1().Services(ns.Name).Delete(serviceName, nil)
			Expect(err).NotTo(HaveOccurred())
		}()
		By("Waiting for exposure of internal service with specific IP")
		err = utils.WaitUpdateServiceExposure(cs, ns.Name, serviceName, ip1, true)
		Expect(err).NotTo(HaveOccurred())

		ip2, err := utils.SelectAvailablePrivateIP(tc)
		Expect(err).NotTo(HaveOccurred())

		By("Updating internal service private IP")
		utils.Logf("will update IP to %s", ip2)
		service, err = cs.CoreV1().Services(ns.Name).Get(serviceName, metav1.GetOptions{})
		service = updateServiceBalanceIP(service, true, ip2)
		_, err = cs.CoreV1().Services(ns.Name).Update(service)
		Expect(err).NotTo(HaveOccurred())

		err = utils.WaitUpdateServiceExposure(cs, ns.Name, serviceName, ip2, true)
		Expect(err).NotTo(HaveOccurred())
	})

	// internal w/o IP -> public w/ IP
	It("should support updating an internal service to a public service with assigned IP", func() {
		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerInternal: "true",
		}
		ipName := basename + "-internal-none-public-IP" + string(uuid.NewUUID())[0:4]

		service := createLoadBalancerServiceManifest(cs, serviceName, annotation, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(service)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns.Name)

		pip, err := utils.WaitCreatePIP(tc, ipName, defaultPublicIPAddress(ipName))
		Expect(err).NotTo(HaveOccurred())
		targetIP := to.String(pip.IPAddress)

		defer func() {
			By("Cleaning up")
			err = cs.CoreV1().Services(ns.Name).Delete(serviceName, nil)
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeletePIPWithRetry(tc, ipName)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Waiting for exposure of the original service without assigned lb private IP")
		ip1, err := utils.WaitServiceExposure(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())
		Expect(ip1).NotTo(Equal(targetIP))

		By("Updating service to bound to specific public IP")
		utils.Logf("will update IP to %s, %v", targetIP, len(targetIP))
		service, err = cs.CoreV1().Services(ns.Name).Get(serviceName, metav1.GetOptions{})
		service = updateServiceBalanceIP(service, false, targetIP)

		_, err = cs.CoreV1().Services(ns.Name).Update(service)
		Expect(err).NotTo(HaveOccurred())

		err = utils.WaitUpdateServiceExposure(cs, ns.Name, serviceName, targetIP, true)
		Expect(err).NotTo(HaveOccurred())
	})

	It("should have no operation since no change in service when update", func() {
		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerInternal: "false",
		}
		ipName := basename + "-public-remain" + string(uuid.NewUUID())[0:4]
		pip, err := utils.WaitCreatePIP(tc, ipName, defaultPublicIPAddress(ipName))
		Expect(err).NotTo(HaveOccurred())
		targetIP := to.String(pip.IPAddress)

		service := createLoadBalancerServiceManifest(cs, serviceName, annotation, labels, ns.Name, ports)
		service = updateServiceBalanceIP(service, false, targetIP)
		_, err = cs.CoreV1().Services(ns.Name).Create(service)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns.Name)

		defer func() {
			By("Cleaning up")
			err = cs.CoreV1().Services(ns.Name).Delete(serviceName, nil)
			Expect(err).NotTo(HaveOccurred())
			err = utils.DeletePIPWithRetry(tc, ipName)
			Expect(err).NotTo(HaveOccurred())
		}()

		By("Waiting for exposure of the original service with assigned lb private IP")
		targetIP, err = utils.WaitServiceExposure(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())

		By("Update without changing the service and wait for a while")
		utils.Logf("Exteral IP is now %s", targetIP)
		service, err = cs.CoreV1().Services(ns.Name).Get(serviceName, metav1.GetOptions{})
		service.Annotations[azure.ServiceAnnotationDNSLabelName] = "testlabel"
		utils.Logf(service.Annotations[azure.ServiceAnnotationDNSLabelName])
		_, err = cs.CoreV1().Services(ns.Name).Update(service)
		Expect(err).NotTo(HaveOccurred())

		//Wait for 10 minutes, there should return timeout err, since external ip should not change
		err = utils.WaitUpdateServiceExposure(cs, ns.Name, serviceName, targetIP, false /*expectSame*/)
		Expect(err).To(Equal(wait.ErrWaitTimeout))
	})

	It("should support the annotation of ServiceAnnotationLoadBalancerIdleTimeout", func() {
		annotation := map[string]string{
			azure.ServiceAnnotationLoadBalancerIdleTimeout: "5",
		}
		// ipName := basename + "-public-none-IP" + string(uuid.NewUUID())[0:4]
		service := createLoadBalancerServiceManifest(cs, serviceName, annotation, labels, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(service)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created LoadBalancer service " + serviceName + " in namespace " + ns.Name)

		_, err = utils.WaitServiceExposure(cs, ns.Name, serviceName)
		Expect(err).NotTo(HaveOccurred())

		service, err = cs.CoreV1().Services(ns.Name).Get(serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		idleAnnotation, ok := service.ObjectMeta.Annotations[azure.ServiceAnnotationLoadBalancerIdleTimeout]
		Expect(ok).To(Equal(true))
		Expect(idleAnnotation).To(Equal("5"))
	})
})

func judgeInternal(service v1.Service) bool {
	return service.Annotations[azure.ServiceAnnotationLoadBalancerInternal] == "true"
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
		result.Annotations[azure.ServiceAnnotationLoadBalancerInternal] = "true"
	} else {
		delete(result.Annotations, azure.ServiceAnnotationLoadBalancerInternal)
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
			PublicIPAllocationMethod: aznetwork.Static,
		},
	}
}
