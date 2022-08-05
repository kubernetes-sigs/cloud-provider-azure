/*
Copyright 2022 The Kubernetes Authors.

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
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Private link service", Label(utils.TestSuiteLabelPrivateLinkService), func() {
	basename := "pls"
	serviceName := "pls-test"

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
		if !strings.EqualFold(os.Getenv(utils.LoadBalancerSkuEnv), string(network.PublicIPAddressSkuNameStandard)) {
			Skip("private link service only works with standard load balancer")
		}
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
	})

	AfterEach(func() {
		err := utils.DeletePod(cs, ns.Name, utils.ExecAgnhostPod)
		Expect(err).NotTo(HaveOccurred())

		err = utils.DeleteNamespace(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		cs = nil
		ns = nil
		tc = nil
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-pls-create'", func() {
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal: "true",
			consts.ServiceAnnotationPLSCreation:          "true",
		}

		// create service with given annotation and wait it to expose
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Get Internal IP: %s", ip)

		// get pls from azure client
		pls := getPrivateLinkServiceFromIP(tc, ip, "", "", "")
		Expect(pls.IPConfigurations).NotTo(BeNil())
		Expect(len(*pls.IPConfigurations)).To(Equal(1))
		Expect((*pls.IPConfigurations)[0].PrivateIPAllocationMethod).To(Equal(network.IPAllocationMethodDynamic))
		Expect(pls.Fqdns == nil || len(*pls.Fqdns) == 0).To(BeTrue())
		Expect(pls.EnableProxyProtocol == nil || !*pls.EnableProxyProtocol).To(BeTrue())
		Expect(pls.Visibility == nil || len(*pls.Visibility.Subscriptions) == 0).To(BeTrue())
		Expect(pls.AutoApproval == nil || len(*pls.AutoApproval.Subscriptions) == 0).To(BeTrue())
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-pls-name'", func() {
		plsName := "testpls"
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal: "true",
			consts.ServiceAnnotationPLSCreation:          "true",
			consts.ServiceAnnotationPLSName:              plsName,
		}

		// create service with given annotation and wait it to expose
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Get Internal IP: %s", ip)

		// get pls from azure client
		pls := getPrivateLinkServiceFromIP(tc, ip, "", "", plsName)
		Expect(*pls.Name).To(Equal(plsName))
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-pls-ip-configuration-subnet'", func() {
		subnetName := "pls-subnet"

		vNet, err := tc.GetClusterVirtualNetwork()
		Expect(err).NotTo(HaveOccurred())

		var newSubnetID string
		for _, existingSubnet := range *vNet.Subnets {
			if *existingSubnet.Name == subnetName {
				By("Test subnet have existed, skip creating")
				newSubnetID = *existingSubnet.ID
				break
			}
		}

		if newSubnetID == "" {
			By("Test subnet doesn't exist. Creating a new one...")
			newSubnetCIDR, err := utils.GetNextSubnetCIDR(vNet)
			Expect(err).NotTo(HaveOccurred())
			newSubnet, err := tc.CreateSubnet(vNet, &subnetName, &newSubnetCIDR, true)
			newSubnetID = *newSubnet.ID
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				utils.Logf("cleaning up test subnet %s", subnetName)
				err = tc.DeleteSubnet(*vNet.Name, subnetName)
				Expect(err).NotTo(HaveOccurred())
			}()
		}

		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal:     "true",
			consts.ServiceAnnotationPLSCreation:              "true",
			consts.ServiceAnnotationPLSIpConfigurationSubnet: subnetName,
		}

		// create service with given annotation and wait it to expose
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Get Internal IP: %s", ip)

		// get pls from azure client
		pls := getPrivateLinkServiceFromIP(tc, ip, "", "", "")
		Expect(pls.IPConfigurations).NotTo(BeNil())
		Expect(len(*pls.IPConfigurations)).To(Equal(1))
		Expect(strings.EqualFold(*(*pls.IPConfigurations)[0].Subnet.ID, newSubnetID)).To(BeTrue())
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-pls-ip-configuration-ip-address-count'", func() {
		ipConfigCount := 3
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal:             "true",
			consts.ServiceAnnotationPLSCreation:                      "true",
			consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: strconv.Itoa(ipConfigCount),
		}

		// create service with given annotation and wait it to expose
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Get Internal IP: %s", ip)

		// get pls from azure client
		pls := getPrivateLinkServiceFromIP(tc, ip, "", "", "")
		Expect(pls.IPConfigurations).NotTo(BeNil())
		Expect(len(*pls.IPConfigurations)).To(Equal(ipConfigCount))
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-pls-ip-configuration-ip-address'", func() {
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal: "true",
			consts.ServiceAnnotationPLSCreation:          "true",
		}

		// create service with given annotation and wait it to expose
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Get Internal IP: %s", ip)

		selectedip, err := utils.SelectAvailablePrivateIP(tc)
		Expect(err).NotTo(HaveOccurred())
		annotation[consts.ServiceAnnotationPLSIpConfigurationIPAddress] = selectedip
		utils.Logf("Now update private link service's static ip to %s", selectedip)

		service, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		service = updateServiceAnnotation(service, annotation)
		utils.Logf("service's annotations: %v", annotation)
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), service, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		ip, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, serviceName, "")
		Expect(err).NotTo(HaveOccurred())

		// wait and check pls is updated also
		err = wait.PollImmediate(10*time.Second, 5*time.Minute, func() (bool, error) {
			pls := getPrivateLinkServiceFromIP(tc, ip, "", "", "")
			return pls.IPConfigurations != nil &&
				len(*pls.IPConfigurations) == 1 &&
				(*pls.IPConfigurations)[0].PrivateIPAllocationMethod == network.IPAllocationMethodStatic &&
				*(*pls.IPConfigurations)[0].PrivateIPAddress == selectedip, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-pls-fqdns'", func() {
		fqdns := "fqdns1 fqdns2"
		expectedFqdns := map[string]bool{
			"fqdns1": true,
			"fqdns2": true,
		}
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal: "true",
			consts.ServiceAnnotationPLSCreation:          "true",
			consts.ServiceAnnotationPLSFqdns:             fqdns,
		}

		// create service with given annotation and wait it to expose
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Get Internal IP: %s", ip)

		// get pls from azure client
		pls := getPrivateLinkServiceFromIP(tc, ip, "", "", "")
		Expect(pls.Fqdns).NotTo(BeNil())
		actualFqdns := make(map[string]bool)
		for _, f := range *pls.Fqdns {
			actualFqdns[f] = true
		}
		Expect(actualFqdns).To(Equal(expectedFqdns))
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-pls-proxy-protocol'", func() {
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal: "true",
			consts.ServiceAnnotationPLSCreation:          "true",
			consts.ServiceAnnotationPLSProxyProtocol:     "true",
		}

		// create service with given annotation and wait it to expose
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Get Internal IP: %s", ip)

		// get pls from azure client
		pls := getPrivateLinkServiceFromIP(tc, ip, "", "", "")
		Expect(pls.EnableProxyProtocol).NotTo(BeNil())
		Expect(*pls.EnableProxyProtocol).To(BeTrue())
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-pls-visibility'", func() {
		vis := "00000000-0000-0000-0000-000000000000 00000000-0000-0000-0000-000000000001"
		expectedVis := map[string]bool{
			"00000000-0000-0000-0000-000000000000": true,
			"00000000-0000-0000-0000-000000000001": true,
		}
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal: "true",
			consts.ServiceAnnotationPLSCreation:          "true",
			consts.ServiceAnnotationPLSVisibility:        vis,
		}

		// create service with given annotation and wait it to expose
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Get Internal IP: %s", ip)

		// get pls from azure client
		pls := getPrivateLinkServiceFromIP(tc, ip, "", "", "")
		Expect(pls.Visibility).NotTo(BeNil())
		Expect(pls.Visibility.Subscriptions).NotTo(BeNil())
		actualVis := make(map[string]bool)
		for _, v := range *pls.Visibility.Subscriptions {
			actualVis[v] = true
		}
		Expect(actualVis).To(Equal(expectedVis))
	})

	It("should support service annotation 'service.beta.kubernetes.io/azure-pls-auto-approval'", func() {
		autoapp := "00000000-0000-0000-0000-000000000000 00000000-0000-0000-0000-000000000001"
		expectedAutoApp := map[string]bool{
			"00000000-0000-0000-0000-000000000000": true,
			"00000000-0000-0000-0000-000000000001": true,
		}
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal: "true",
			consts.ServiceAnnotationPLSCreation:          "true",
			consts.ServiceAnnotationPLSVisibility:        "*",
			consts.ServiceAnnotationPLSAutoApproval:      autoapp,
		}

		// create service with given annotation and wait it to expose
		ip := createAndExposeDefaultServiceWithAnnotation(cs, serviceName, ns.Name, labels, annotation, ports)
		defer func() {
			utils.Logf("cleaning up test service %s", serviceName)
			err := utils.DeleteService(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Get Internal IP: %s", ip)

		// get pls from azure client
		pls := getPrivateLinkServiceFromIP(tc, ip, "", "", "")
		Expect(pls.AutoApproval).NotTo(BeNil())
		Expect(pls.AutoApproval.Subscriptions).NotTo(BeNil())
		actualAutoApp := make(map[string]bool)
		for _, v := range *pls.AutoApproval.Subscriptions {
			actualAutoApp[v] = true
		}
		Expect(actualAutoApp).To(Equal(expectedAutoApp))
	})

	It("should support multiple internal services sharing one private link service", func() {
		ipAddrCount := 2
		annotation := map[string]string{
			consts.ServiceAnnotationLoadBalancerInternal:             "true",
			consts.ServiceAnnotationPLSCreation:                      "true",
			consts.ServiceAnnotationPLSIpConfigurationIPAddressCount: strconv.Itoa(ipAddrCount),
		}
		svc1 := "service1"
		ip := createAndExposeDefaultServiceWithAnnotation(cs, svc1, ns.Name, labels, annotation, ports)
		defer func() {
			err := utils.DeleteService(cs, ns.Name, svc1)
			Expect(err).NotTo(HaveOccurred())
		}()
		utils.Logf("Successfully created %s in namespace %s with IP %s", svc1, ns.Name, ip)

		deployName0 := "pls-deploy0"
		utils.Logf("Creating deployment %s", deployName0)
		label0 := map[string]string{
			"app": deployName0,
		}
		tcpTestingPort := int32(testingPort)
		deploy0 := createDeploymentManifest(deployName0, label0, &tcpTestingPort, nil)
		_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), deploy0, metav1.CreateOptions{})
		defer func() {
			err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), deployName0, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}()
		Expect(err).NotTo(HaveOccurred())

		ports2 := []v1.ServicePort{{
			Port:       testingPort,
			TargetPort: intstr.FromInt(testingPort),
		}}
		delete(annotation, consts.ServiceAnnotationPLSIpConfigurationIPAddressCount)
		svc2 := "service2"
		service2 := utils.CreateLoadBalancerServiceManifest(svc2, annotation, label0, ns.Name, ports2)
		defer func() {
			err = utils.DeleteService(cs, ns.Name, svc2)
			Expect(err).NotTo(HaveOccurred())
		}()
		service2.Spec.LoadBalancerIP = ip
		_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), service2, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, ns.Name, svc2, ip)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("Successfully created %s in namespace %s with IP %s", svc2, ns.Name, ip)

		// get pls from azure client
		pls := getPrivateLinkServiceFromIP(tc, ip, "", "", "")
		Expect(pls.IPConfigurations).NotTo(BeNil())
		// Verify it's still the configuration from service1
		Expect(len(*pls.IPConfigurations)).To(Equal(ipAddrCount))
	})
})

func updateServiceAnnotation(service *v1.Service, annotation map[string]string) (result *v1.Service) {
	result = service
	if result == nil {
		return
	}
	result.Annotations = annotation
	return
}

func getPrivateLinkServiceFromIP(tc *utils.AzureTestClient, ip, plsResourceGroup, lbResourceGroup, plsName string) *network.PrivateLinkService {
	if lbResourceGroup == "" {
		lbResourceGroup = tc.GetResourceGroup()
	}
	utils.Logf("Getting load balancers in the resourceGroup " + lbResourceGroup)
	lbList, err := tc.ListLoadBalancers(lbResourceGroup)
	Expect(err).NotTo(HaveOccurred())

	utils.Logf("Looking for internal load balancer frontend config ID with private ip as frontend")
	var lbFipConfigName string
	for _, lb := range lbList {
		for _, fipconfig := range *lb.FrontendIPConfigurations {
			if fipconfig.PrivateIPAddress != nil &&
				*fipconfig.PrivateIPAddress == ip {
				lbFipConfigName = *fipconfig.Name
				break
			}
		}
	}
	Expect(lbFipConfigName).NotTo(Equal(""))
	utils.Logf("Successfully obtained LB frontend config: %v", lbFipConfigName)

	if plsName == "" {
		plsName = fmt.Sprintf("%s-%s", "pls", lbFipConfigName)
	}
	if plsResourceGroup == "" {
		plsResourceGroup = tc.GetResourceGroup()
	}

	utils.Logf("Getting private link service(%s) from rg(%s)", plsName, plsResourceGroup)
	var pls network.PrivateLinkService
	err = wait.PollImmediate(10*time.Second, 10*time.Minute, func() (bool, error) {
		pls, err = tc.GetPrivateLinkService(plsResourceGroup, plsName)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return false, nil
			}
			return false, err
		}
		return *pls.Name == plsName, nil
	})
	Expect(err).NotTo(HaveOccurred())

	utils.Logf("Successfully obtained private link service")
	return &pls
}
