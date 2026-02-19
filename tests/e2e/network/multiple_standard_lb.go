/*
Copyright 2023 The Kubernetes Authors.

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

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Ensure LoadBalancer", Label(utils.TestSuiteLabelMultiSLB), func() {
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

	It("should send nodes to correct load balancers by primary vmSet", func() {
		if os.Getenv(utils.AKSTestCCM) != "" {
			Skip("This test is not supported for AKS CCM.")
		}

		var svcIPs []*string
		svcCount := 2
		for i := 0; i < svcCount; i++ {
			svcName := fmt.Sprintf("%s-%d", testServiceName, i)
			svc1 := utils.CreateLoadBalancerServiceManifest(svcName, nil, labels, ns.Name, ports)
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, svcName, []*string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ips)).NotTo(BeZero())
			svcIPs = append(svcIPs, ips[0])
		}

		clusterName := os.Getenv("CLUSTER_NAME")
		lbNameToExpectedVMSetNameMap := map[string]string{
			clusterName: fmt.Sprintf("%s-mp-0", clusterName),
			"lb1":       fmt.Sprintf("%s-mp-1", clusterName),
			"lb2":       fmt.Sprintf("%s-mp-2", clusterName),
		}

		for _, svcIP := range svcIPs {
			lb := getAzureLoadBalancerFromPIP(tc, svcIP, tc.GetResourceGroup(), tc.GetResourceGroup())
			lbName := ptr.Deref(lb.Name, "")
			for _, bp := range lb.Properties.BackendAddressPools {
				for _, a := range bp.Properties.LoadBalancerBackendAddresses {
					nodeName := ptr.Deref(a.Name, "")
					expectedVMSSName := lbNameToExpectedVMSetNameMap[lbName]
					if nodeName == "" || !strings.HasPrefix(nodeName, expectedVMSSName) {
						Fail(fmt.Sprintf("Node %s is not in the expected VMSS %s on LB %s", nodeName, expectedVMSSName, lbName))
					}
				}
			}
		}
	})

	It("should arrange services across load balancers correctly", func() {
		By("Creating a service targeting to lb-2")
		var svcIPs []*string
		l := map[string]string{
			"app": testServiceName,
			"a":   "b",
		}
		svcWithLabel := utils.CreateLoadBalancerServiceManifest("svc-with-label", nil, l, ns.Name, ports)
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svcWithLabel, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		svsWithLabel, err := utils.WaitServiceExposure(cs, ns.Name, "svc-with-label", []*string{})
		Expect(err).NotTo(HaveOccurred())
		svcIP := svsWithLabel.Status.LoadBalancer.Ingress[0].IP
		svcIPs = append(svcIPs, &svcIP)
		lb := getAzureLoadBalancerFromPIP(tc, &svcIP, tc.GetResourceGroup(), tc.GetResourceGroup())
		Expect(ptr.Deref(lb.Name, "")).To(Equal("lb-2"))

		svcCount := 2
		var svc *v1.Service
		By(fmt.Sprintf("Creating %d services", svcCount))
		for i := 0; i < svcCount; i++ {
			svcName := fmt.Sprintf("%s-%d", testServiceName, i)
			svc = utils.CreateLoadBalancerServiceManifest(svcName, nil, labels, ns.Name, ports)
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, svcName, []*string{})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ips)).NotTo(BeZero())
			svcIPs = append(svcIPs, ips[0])
		}

		By("Checking the load balancer count to equal 3")
		interval := 5 * time.Second
		timeout := 5 * time.Minute
		_, err = waitLBCountEqualTo(tc, interval, timeout, 3, svcIPs)
		Expect(err).NotTo(HaveOccurred())

		clusterName := os.Getenv("CLUSTER_NAME")
		By(fmt.Sprintf("Updating service %s to move to another load balancer %s", svc.Name, clusterName))
		updateServiceAnnotation(svc, map[string]string{"service.beta.kubernetes.io/azure-load-balancer-configurations": clusterName})
		_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Checking the load balancer count to equal 2")
		lbNames, err := waitLBCountEqualTo(tc, interval, timeout, 2, svcIPs)
		Expect(err).NotTo(HaveOccurred())
		Expect(lbNames.Has("lb-1")).To(BeFalse())
		Expect(lbNames.Has(clusterName)).To(BeTrue())
	})

	It("should arrange local services", func() {
		By("Creating a local service")
		svc := utils.CreateLoadBalancerServiceManifest(testServiceName, nil, labels, ns.Name, ports)
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
		_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, testServiceName, []*string{})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(ips)).NotTo(BeZero())

		nodeNames, err := getDeploymentPodsNodeNames(cs, ns.Name, testDeploymentName)
		Expect(err).NotTo(HaveOccurred())
		By(fmt.Sprintf("Checking the node count in the local service backend pool to equal %d", len(nodeNames)))
		clusterName := os.Getenv("CLUSTER_NAME")
		Expect(err).NotTo(HaveOccurred())
		expectedBPName := fmt.Sprintf("%s-%s", svc.Namespace, svc.Name)
		err = checkNodeCountInBackendPoolByServiceIPs(tc, clusterName, expectedBPName, ips, len(nodeNames))
		Expect(err).NotTo(HaveOccurred())

		By("Scaling the deployment to 3 replicas and then to 1")
		deployment.Spec.Replicas = ptr.To(int32(3))
		_, err = cs.AppsV1().Deployments(ns.Name).Update(context.Background(), deployment, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		deployment.Spec.Replicas = ptr.To(int32(1))
		_, err = cs.AppsV1().Deployments(ns.Name).Update(context.Background(), deployment, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 5*time.Minute, false, func(ctx context.Context) (bool, error) {
			if err := checkNodeCountInBackendPoolByServiceIPs(tc, clusterName, expectedBPName, ips, 1); err != nil {
				if strings.Contains(err.Error(), "expected node count") {
					utils.Logf("Waiting for the node count in the backend pool to equal 1")
					return false, nil
				}
				return false, err
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())

		By("Scaling the deployment to 5")
		deployment.Spec.Replicas = ptr.To(int32(5))
		_, err = cs.AppsV1().Deployments(ns.Name).Update(context.Background(), deployment, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())
		nodeNames, err = getDeploymentPodsNodeNames(cs, ns.Name, testDeploymentName)
		Expect(err).NotTo(HaveOccurred())
		err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 5*time.Minute, false, func(ctx context.Context) (bool, error) {
			if err := checkNodeCountInBackendPoolByServiceIPs(tc, clusterName, expectedBPName, ips, len(nodeNames)); err != nil {
				if strings.Contains(err.Error(), "expected node count") {
					utils.Logf("Waiting for the node count in the backend pool to equal %d", len(nodeNames))
					return false, nil
				}
				return false, err
			}
			return true, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// TODO: Remove F prefix before merging
	FIt("should place all external services sharing a user-assigned public IP on the same load balancer", func() {
		logAllLoadBalancerStates(tc, "Before creating services (user-assigned PIP test)")

		By("Creating a user-assigned public IP")
		ipName := fmt.Sprintf("%s-shared-pip-%s", basename, ns.Name[:8])
		pip, err := utils.WaitCreatePIP(tc, ipName, tc.GetResourceGroup(), defaultPublicIPAddress(ipName, false))
		Expect(err).NotTo(HaveOccurred())
		targetIP := ptr.Deref(pip.Properties.IPAddress, "")
		utils.Logf("Created user-assigned PIP with address %s", targetIP)
		defer func() {
			By("Cleaning up PIP")
			err = utils.DeletePIPWithRetry(tc, ipName, "")
			Expect(err).NotTo(HaveOccurred())
		}()

		var firstFIPID string
		serviceCount := 2
		serviceNames := []string{}
		servicePorts := sets.New[int32]()

		for i := range serviceCount {
			serviceLabels := labels
			deploymentName := testDeploymentName
			tcpPort := int32(serverPort + i)
			servicePorts.Insert(tcpPort)

			if i != 0 {
				deploymentName = fmt.Sprintf("%s-%d", testDeploymentName, i)
				utils.Logf("Creating deployment %q", deploymentName)
				serviceLabels = map[string]string{
					"app": deploymentName,
				}
				tcpPortPtr := tcpPort
				extraDeployment := createDeploymentManifest(deploymentName, serviceLabels, &tcpPortPtr, nil)
				_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), extraDeployment, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func(name string) {
					err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), name, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}(deploymentName)
			}

			serviceName := fmt.Sprintf("%s-%d", testServiceName, i)
			serviceNames = append(serviceNames, serviceName)
			utils.Logf("Creating Service %q with shared IP %s", serviceName, targetIP)

			servicePort := []v1.ServicePort{{
				Port:       tcpPort,
				TargetPort: intstr.FromInt(int(tcpPort)),
			}}
			service := utils.CreateLoadBalancerServiceManifest(serviceName, nil, serviceLabels, ns.Name, servicePort)
			service = updateServiceLBIPs(service, false, []*string{&targetIP})
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func(name string) {
				err = utils.DeleteService(cs, ns.Name, name)
				Expect(err).NotTo(HaveOccurred())
			}(serviceName)

			_, err = utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, []*string{&targetIP})
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("Service %q is exposed with IP %s", serviceName, targetIP)

			// Log FIP state after each service.
			fipID := getPIPFrontendConfigurationID(tc, targetIP, tc.GetResourceGroup(), true)
			utils.Logf("After service %d: IP %s has FIP ID %q", i, targetIP, fipID)
			if i == 0 {
				firstFIPID = fipID
			}
		}

		logAllLoadBalancerStates(tc, "After creating all services (user-assigned PIP test)")

		By("Verifying first FIP has all services' rules")
		err = verifyFIPHasRulesForPorts(tc, firstFIPID, servicePorts, "TCP")
		Expect(err).NotTo(HaveOccurred(), "First FIP should have rules for all services sharing the IP")
	})

	// TODO: Remove F prefix before merging
	FIt("should place all external services sharing a managed public IP on the same load balancer", func() {
		logAllLoadBalancerStates(tc, "Before creating services (managed PIP test)")
		var firstFIPID string
		var sharedIP string
		serviceCount := 2
		serviceNames := []string{}
		servicePorts := sets.New[int32]()

		for i := range serviceCount {
			serviceLabels := labels
			deploymentName := testDeploymentName
			tcpPort := int32(serverPort + i)
			servicePorts.Insert(tcpPort)

			if i != 0 {
				deploymentName = fmt.Sprintf("%s-%d", testDeploymentName, i)
				utils.Logf("Creating deployment %q", deploymentName)
				serviceLabels = map[string]string{
					"app": deploymentName,
				}
				tcpPortPtr := tcpPort
				extraDeployment := createDeploymentManifest(deploymentName, serviceLabels, &tcpPortPtr, nil)
				_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), extraDeployment, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func(name string) {
					err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), name, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}(deploymentName)
			}

			serviceName := fmt.Sprintf("%s-%d", testServiceName, i)
			serviceNames = append(serviceNames, serviceName)

			servicePort := []v1.ServicePort{{
				Port:       tcpPort,
				TargetPort: intstr.FromInt(int(tcpPort)),
			}}
			service := utils.CreateLoadBalancerServiceManifest(serviceName, nil, serviceLabels, ns.Name, servicePort)
			if sharedIP != "" {
				service = updateServiceLBIPs(service, false, []*string{&sharedIP})
				utils.Logf("Creating Service %q with shared managed IP %s", serviceName, sharedIP)
			} else {
				utils.Logf("Creating Service %q (will get managed IP)", serviceName)
			}

			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func(name string) {
				err = utils.DeleteService(cs, ns.Name, name)
				Expect(err).NotTo(HaveOccurred())
			}(serviceName)

			ips, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ips)).NotTo(BeZero())

			// Record shared IP on first service.
			if i == 0 {
				sharedIP = *ips[0]
				utils.Logf("First service got managed IP: %s", sharedIP)
			}

			// Log FIP state after each service.
			fipID := getPIPFrontendConfigurationID(tc, sharedIP, tc.GetResourceGroup(), true)
			utils.Logf("After service %d: IP %s has FIP ID %q", i, sharedIP, fipID)
			if i == 0 {
				firstFIPID = fipID
			}
		}

		logAllLoadBalancerStates(tc, "After creating all services (managed PIP test)")

		By("Verifying first FIP has all services' rules")
		err := verifyFIPHasRulesForPorts(tc, firstFIPID, servicePorts, "TCP")
		Expect(err).NotTo(HaveOccurred(), "First FIP should have rules for all services sharing the IP")
	})

	// TODO: Remove F prefix before merging
	FIt("should place all internal services sharing a private IP on the same load balancer", func() {
		logAllLoadBalancerStates(tc, "Before creating services (internal IP test)")
		var firstFIPID string
		var sharedIP string
		serviceCount := 2
		serviceNames := []string{}
		servicePorts := sets.New[int32]()

		for i := range serviceCount {
			serviceLabels := labels
			deploymentName := testDeploymentName
			tcpPort := int32(serverPort + i)
			servicePorts.Insert(tcpPort)

			if i != 0 {
				deploymentName = fmt.Sprintf("%s-%d", testDeploymentName, i)
				utils.Logf("Creating deployment %q", deploymentName)
				serviceLabels = map[string]string{
					"app": deploymentName,
				}
				tcpPortPtr := tcpPort
				extraDeployment := createDeploymentManifest(deploymentName, serviceLabels, &tcpPortPtr, nil)
				_, err := cs.AppsV1().Deployments(ns.Name).Create(context.TODO(), extraDeployment, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func(name string) {
					err := cs.AppsV1().Deployments(ns.Name).Delete(context.TODO(), name, metav1.DeleteOptions{})
					Expect(err).NotTo(HaveOccurred())
				}(deploymentName)
			}

			serviceName := fmt.Sprintf("%s-%d", testServiceName, i)
			serviceNames = append(serviceNames, serviceName)

			servicePort := []v1.ServicePort{{
				Port:       tcpPort,
				TargetPort: intstr.FromInt(int(tcpPort)),
			}}
			service := utils.CreateLoadBalancerServiceManifest(serviceName, serviceAnnotationLoadBalancerInternalTrue, serviceLabels, ns.Name, servicePort)
			if sharedIP != "" {
				service = updateServiceLBIPs(service, true, []*string{&sharedIP})
				utils.Logf("Creating internal Service %q with shared IP %s", serviceName, sharedIP)
			} else {
				utils.Logf("Creating internal Service %q (will get new IP)", serviceName)
			}

			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func(name string) {
				err = utils.DeleteService(cs, ns.Name, name)
				Expect(err).NotTo(HaveOccurred())
			}(serviceName)

			ips, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, serviceName)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ips)).NotTo(BeZero())

			// Record shared IP on first service.
			if i == 0 {
				sharedIP = *ips[0]
				utils.Logf("First internal service got IP: %s", sharedIP)
			}

			// Log FIP state after each service.
			lb := getAzureInternalLoadBalancerFromPrivateIP(tc, &sharedIP, tc.GetResourceGroup())
			fipID := getFIPIDForPrivateIP(lb, sharedIP)
			utils.Logf("After service %d: IP %s has FIP ID %q", i, sharedIP, fipID)
			if i == 0 {
				firstFIPID = fipID
			}
		}

		logAllLoadBalancerStates(tc, "After creating all services (internal IP test)")

		By("Verifying first FIP has all internal services' rules")
		err := verifyFIPHasRulesForPorts(tc, firstFIPID, servicePorts, "TCP")
		Expect(err).NotTo(HaveOccurred(), "First FIP should have rules for all internal services sharing the IP")
	})

	// Conflict detection and resolution tests: verify services with IP and conflicting LB config
	// are blocked, then test both resolution paths (remove LB annotation vs remove IP pin)
	// TODO: Remove F prefix before merging
	FDescribe("Conflicting LB Configuration", func() {
		const (
			pollInterval      = 10 * time.Second
			serviceTimeout    = 5 * time.Minute
			eventTimeout      = 30 * time.Second
			notExposedTimeout = serviceTimeout / 2

			svcNamePrimary = "svc-primary"
			svcNameLBIP    = "svc-lbip"
			svcNamePIPName = "svc-pipname"
			svcNameIPv4    = "svc-ipv4"
		)

		// TODO: Remove F prefix before merging
		FIt("should block external services with conflicting IP and LB config for user-assigned PIP", func() {
			By("Creating user-assigned PIP")
			pipName := "conflict-test-pip"
			pip, err := utils.WaitCreatePIP(tc, pipName, tc.GetResourceGroup(), defaultPublicIPAddress(pipName, false))
			Expect(err).NotTo(HaveOccurred())
			sharedIP := ptr.Deref(pip.Properties.IPAddress, "")
			Expect(sharedIP).NotTo(BeEmpty())
			defer func() {
				utils.Logf("Cleaning up PIP %s", pipName)
				err := utils.DeletePIPWithRetry(tc, pipName, "")
				Expect(err).NotTo(HaveOccurred())
			}()

			By("Creating primary service with azure-load-balancer-ipv4 annotation")
			primaryPort := int32(serverPort)
			primaryService := utils.CreateLoadBalancerServiceManifest(svcNamePrimary, nil, labels, ns.Name, []v1.ServicePort{{
				Port:       primaryPort,
				TargetPort: intstr.FromInt(int(primaryPort)),
			}})
			primaryService = updateServiceLBIPs(primaryService, false, []*string{&sharedIP})
			_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), primaryService, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNamePrimary)
				Expect(err).NotTo(HaveOccurred())
			}()

			_, err = utils.WaitServiceExposure(cs, ns.Name, svcNamePrimary, []*string{&sharedIP})
			Expect(err).NotTo(HaveOccurred())
			primaryFIPID := getPIPFrontendConfigurationID(tc, sharedIP, tc.GetResourceGroup(), true)
			utils.Logf("Primary service exposed on FIP: %s", primaryFIPID)

			By("Creating service with spec.loadBalancerIP and LB config, expecting blocked")
			svc1Port := int32(serverPort + 1)
			svc1 := utils.CreateLoadBalancerServiceManifest(svcNameLBIP, nil, labels, ns.Name, []v1.ServicePort{{
				Port:       svc1Port,
				TargetPort: intstr.FromInt(int(svc1Port)),
			}})
			svc1.Spec.LoadBalancerIP = sharedIP
			if svc1.Annotations == nil {
				svc1.Annotations = map[string]string{}
			}
			svc1.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"
			_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNameLBIP)
				Expect(err).NotTo(HaveOccurred())
			}()

			err = verifyServiceNotExposed(cs, ns.Name, svcNameLBIP, notExposedTimeout)
			Expect(err).NotTo(HaveOccurred(), svcNameLBIP+" should stay pending")
			err = waitForServiceWarningEvent(cs, ns.Name, svcNameLBIP, "SyncLoadBalancerFailed", eventTimeout)
			Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+svcNameLBIP)

			By("Creating service with azure-pip-name and LB config, expecting blocked")
			svc2Port := int32(serverPort + 2)
			svc2 := utils.CreateLoadBalancerServiceManifest(svcNamePIPName, nil, labels, ns.Name, []v1.ServicePort{{
				Port:       svc2Port,
				TargetPort: intstr.FromInt(int(svc2Port)),
			}})
			svc2 = updateServicePIPNames(tc.IPFamily, svc2, []string{pipName})
			if svc2.Annotations == nil {
				svc2.Annotations = map[string]string{}
			}
			svc2.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"
			_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc2, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNamePIPName)
				Expect(err).NotTo(HaveOccurred())
			}()

			err = verifyServiceNotExposed(cs, ns.Name, svcNamePIPName, notExposedTimeout)
			Expect(err).NotTo(HaveOccurred(), svcNamePIPName+" should stay pending")
			err = waitForServiceWarningEvent(cs, ns.Name, svcNamePIPName, "SyncLoadBalancerFailed", eventTimeout)
			Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+svcNamePIPName)

			By("Creating service with azure-load-balancer-ipv4 and LB config, expecting blocked")
			svc3Port := int32(serverPort + 3)
			svc3 := utils.CreateLoadBalancerServiceManifest(svcNameIPv4, nil, labels, ns.Name, []v1.ServicePort{{
				Port:       svc3Port,
				TargetPort: intstr.FromInt(int(svc3Port)),
			}})
			svc3 = updateServiceLBIPs(svc3, false, []*string{&sharedIP})
			if svc3.Annotations == nil {
				svc3.Annotations = map[string]string{}
			}
			svc3.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"
			_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc3, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNameIPv4)
				Expect(err).NotTo(HaveOccurred())
			}()

			err = verifyServiceNotExposed(cs, ns.Name, svcNameIPv4, notExposedTimeout)
			Expect(err).NotTo(HaveOccurred(), svcNameIPv4+" should stay pending")
			err = waitForServiceWarningEvent(cs, ns.Name, svcNameIPv4, "SyncLoadBalancerFailed", eventTimeout)
			Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+svcNameIPv4)

			By("Verifying primary service is still working")
			err = verifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort), "TCP")
			Expect(err).NotTo(HaveOccurred(), "Primary service should still have its rule")

			By("Resolving " + svcNameIPv4 + " by removing LB config annotation")
			svc3, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			delete(svc3.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc3, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameIPv4, []*string{&sharedIP})
			Expect(err).NotTo(HaveOccurred(), svcNameIPv4+" should be exposed after removing LB config")

			By("Resolving " + svcNamePIPName + " by removing LB config annotation")
			svc2, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNamePIPName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			delete(svc2.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc2, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.WaitServiceExposure(cs, ns.Name, svcNamePIPName, []*string{&sharedIP})
			Expect(err).NotTo(HaveOccurred(), svcNamePIPName+" should be exposed after removing LB config")

			By("Resolving " + svcNameLBIP + " by removing LB config annotation")
			svc1, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameLBIP, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			delete(svc1.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc1, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameLBIP, []*string{&sharedIP})
			Expect(err).NotTo(HaveOccurred(), svcNameLBIP+" should be exposed after removing LB config")

			By("Verifying primary FIP has rules for all services")
			err = verifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort, svc1Port, svc2Port, svc3Port), "TCP")
			Expect(err).NotTo(HaveOccurred(), "Primary FIP should have rules for all services sharing the IP")

			By("Re-adding LB config annotation to " + svcNameLBIP + ", expecting blocked again")
			svc1, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameLBIP, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			if svc1.Annotations == nil {
				svc1.Annotations = map[string]string{}
			}
			// Pick a different LB than the primary so the service actually moves when IP pin is removed.
			primaryLBName := lbNameRE.FindStringSubmatch(primaryFIPID)[1]
			lbConfig := map[bool]string{true: "lb-2", false: "lb-1"}[strings.HasPrefix(primaryLBName, "lb-1")]
			svc1.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = lbConfig
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc1, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = waitForServiceWarningEvent(cs, ns.Name, svcNameLBIP, "SyncLoadBalancerFailed", eventTimeout)
			Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event on update")

			By("Resolving " + svcNameLBIP + " by removing IP pin, expecting service moves to configured LB")
			svc1, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameLBIP, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			svc1.Spec.LoadBalancerIP = ""
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc1, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			newIP, err := waitForServiceIPChange(cs, ns.Name, svcNameLBIP, sharedIP, serviceTimeout)
			Expect(err).NotTo(HaveOccurred(), svcNameLBIP+" should get a new IP after removing IP pin")
			Expect(newIP).NotTo(Equal(sharedIP), svcNameLBIP+" should get a new IP on the configured LB")

			newFIPID := getPIPFrontendConfigurationID(tc, newIP, tc.GetResourceGroup(), true)
			utils.Logf("%s moved to new FIP: %s with IP %s", svcNameLBIP, newFIPID, newIP)

			By("Verifying final state with primary on original FIP and " + svcNameLBIP + " on new FIP")
			err = verifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort, svc2Port, svc3Port), "TCP")
			Expect(err).NotTo(HaveOccurred(), "Primary FIP should have rules for primary, "+svcNamePIPName+", and "+svcNameIPv4)
			err = verifyFIPHasRulesForPorts(tc, newFIPID, sets.New(svc1Port), "TCP")
			Expect(err).NotTo(HaveOccurred(), "New FIP should have "+svcNameLBIP+"'s rule")
		})

		// TODO: Remove F prefix before merging
		FIt("should block external services with conflicting IP and LB config for managed PIP", func() {
			By("Creating primary service that gets a managed PIP")
			primaryPort := int32(serverPort)
			primaryService := utils.CreateLoadBalancerServiceManifest(svcNamePrimary, nil, labels, ns.Name, []v1.ServicePort{{
				Port:       primaryPort,
				TargetPort: intstr.FromInt(int(primaryPort)),
			}})
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), primaryService, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNamePrimary)
				Expect(err).NotTo(HaveOccurred())
			}()

			ips, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, svcNamePrimary)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ips)).NotTo(BeZero())
			sharedIP := *ips[0]
			primaryFIPID := getPIPFrontendConfigurationID(tc, sharedIP, tc.GetResourceGroup(), true)
			utils.Logf("Primary service exposed with managed IP %s on FIP: %s", sharedIP, primaryFIPID)

			By("Creating service with spec.loadBalancerIP and LB config, expecting blocked")
			svc1Port := int32(serverPort + 1)
			svc1 := utils.CreateLoadBalancerServiceManifest(svcNameLBIP, nil, labels, ns.Name, []v1.ServicePort{{
				Port:       svc1Port,
				TargetPort: intstr.FromInt(int(svc1Port)),
			}})
			svc1.Spec.LoadBalancerIP = sharedIP
			if svc1.Annotations == nil {
				svc1.Annotations = map[string]string{}
			}
			svc1.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"
			_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNameLBIP)
				Expect(err).NotTo(HaveOccurred())
			}()

			err = verifyServiceNotExposed(cs, ns.Name, svcNameLBIP, notExposedTimeout)
			Expect(err).NotTo(HaveOccurred(), svcNameLBIP+" should stay pending")
			err = waitForServiceWarningEvent(cs, ns.Name, svcNameLBIP, "SyncLoadBalancerFailed", eventTimeout)
			Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+svcNameLBIP)

			By("Creating service with azure-load-balancer-ipv4 and LB config, expecting blocked")
			svc2Port := int32(serverPort + 2)
			svc2 := utils.CreateLoadBalancerServiceManifest(svcNameIPv4, nil, labels, ns.Name, []v1.ServicePort{{
				Port:       svc2Port,
				TargetPort: intstr.FromInt(int(svc2Port)),
			}})
			svc2 = updateServiceLBIPs(svc2, false, []*string{&sharedIP})
			if svc2.Annotations == nil {
				svc2.Annotations = map[string]string{}
			}
			svc2.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"
			_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc2, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNameIPv4)
				Expect(err).NotTo(HaveOccurred())
			}()

			err = verifyServiceNotExposed(cs, ns.Name, svcNameIPv4, notExposedTimeout)
			Expect(err).NotTo(HaveOccurred(), svcNameIPv4+" should stay pending")
			err = waitForServiceWarningEvent(cs, ns.Name, svcNameIPv4, "SyncLoadBalancerFailed", eventTimeout)
			Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+svcNameIPv4)

			By("Verifying primary service is still working")
			err = verifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort), "TCP")
			Expect(err).NotTo(HaveOccurred(), "Primary service should still have its rule")

			By("Resolving " + svcNameIPv4 + " by removing LB config annotation")
			svc2, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			delete(svc2.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc2, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameIPv4, []*string{&sharedIP})
			Expect(err).NotTo(HaveOccurred(), svcNameIPv4+" should be exposed after removing LB config")

			By("Resolving " + svcNameLBIP + " by removing LB config annotation")
			svc1, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameLBIP, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			delete(svc1.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc1, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameLBIP, []*string{&sharedIP})
			Expect(err).NotTo(HaveOccurred(), svcNameLBIP+" should be exposed after removing LB config")

			By("Verifying primary FIP has rules for all services")
			err = verifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort, svc1Port, svc2Port), "TCP")
			Expect(err).NotTo(HaveOccurred(), "Primary FIP should have rules for all services sharing the IP")

			By("Re-adding LB config annotation to " + svcNameLBIP + ", expecting blocked again")
			svc1, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameLBIP, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			if svc1.Annotations == nil {
				svc1.Annotations = map[string]string{}
			}
			// Pick a different LB than the primary so the service actually moves when IP pin is removed
			primaryLBName := lbNameRE.FindStringSubmatch(primaryFIPID)[1]
			lbConfig := map[bool]string{true: "lb-2", false: "lb-1"}[strings.HasPrefix(primaryLBName, "lb-1")]
			svc1.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = lbConfig
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc1, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = waitForServiceWarningEvent(cs, ns.Name, svcNameLBIP, "SyncLoadBalancerFailed", eventTimeout)
			Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event on update")

			By("Resolving " + svcNameLBIP + " by removing IP pin, expecting service moves to configured LB")
			svc1, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameLBIP, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			svc1.Spec.LoadBalancerIP = ""
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc1, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			newIP, err := waitForServiceIPChange(cs, ns.Name, svcNameLBIP, sharedIP, serviceTimeout)
			Expect(err).NotTo(HaveOccurred(), svcNameLBIP+" should get a new IP after removing IP pin")
			Expect(newIP).NotTo(Equal(sharedIP), svcNameLBIP+" should get a new IP on the configured LB")

			newFIPID := getPIPFrontendConfigurationID(tc, newIP, tc.GetResourceGroup(), true)
			utils.Logf("%s moved to new FIP: %s with IP %s", svcNameLBIP, newFIPID, newIP)

			By("Verifying final state with primary on original FIP and " + svcNameLBIP + " on new FIP")
			err = verifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort, svc2Port), "TCP")
			Expect(err).NotTo(HaveOccurred(), "Primary FIP should have rules for primary and "+svcNameIPv4)
			err = verifyFIPHasRulesForPorts(tc, newFIPID, sets.New(svc1Port), "TCP")
			Expect(err).NotTo(HaveOccurred(), "New FIP should have "+svcNameLBIP+"'s rule")
		})

		// TODO: Remove F prefix before merging
		FIt("should block internal services with conflicting IP and LB config", func() {
			By("Creating primary internal service")
			primaryPort := int32(serverPort)
			primaryService := utils.CreateLoadBalancerServiceManifest(svcNamePrimary, serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, []v1.ServicePort{{
				Port:       primaryPort,
				TargetPort: intstr.FromInt(int(primaryPort)),
			}})
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), primaryService, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNamePrimary)
				Expect(err).NotTo(HaveOccurred())
			}()

			ips, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, svcNamePrimary)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ips)).NotTo(BeZero())
			sharedIP := *ips[0]
			lb := getAzureInternalLoadBalancerFromPrivateIP(tc, &sharedIP, tc.GetResourceGroup())
			primaryFIPID := getFIPIDForPrivateIP(lb, sharedIP)
			utils.Logf("Primary internal service exposed with IP %s on FIP: %s", sharedIP, primaryFIPID)

			By("Creating service with spec.loadBalancerIP and LB config, expecting blocked")
			svc1Port := int32(serverPort + 1)
			svc1 := utils.CreateLoadBalancerServiceManifest(svcNameLBIP, serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, []v1.ServicePort{{
				Port:       svc1Port,
				TargetPort: intstr.FromInt(int(svc1Port)),
			}})
			svc1.Spec.LoadBalancerIP = sharedIP
			if svc1.Annotations == nil {
				svc1.Annotations = map[string]string{}
			}
			svc1.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"
			_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNameLBIP)
				Expect(err).NotTo(HaveOccurred())
			}()

			err = verifyServiceNotExposed(cs, ns.Name, svcNameLBIP, notExposedTimeout)
			Expect(err).NotTo(HaveOccurred(), svcNameLBIP+" should stay pending")
			err = waitForServiceWarningEvent(cs, ns.Name, svcNameLBIP, "SyncLoadBalancerFailed", eventTimeout)
			Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+svcNameLBIP)

			By("Creating service with azure-load-balancer-ipv4 and LB config, expecting blocked")
			svc2Port := int32(serverPort + 2)
			svc2 := utils.CreateLoadBalancerServiceManifest(svcNameIPv4, serviceAnnotationLoadBalancerInternalTrue, labels, ns.Name, []v1.ServicePort{{
				Port:       svc2Port,
				TargetPort: intstr.FromInt(int(svc2Port)),
			}})
			svc2 = updateServiceLBIPs(svc2, true, []*string{&sharedIP})
			if svc2.Annotations == nil {
				svc2.Annotations = map[string]string{}
			}
			svc2.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"
			_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc2, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNameIPv4)
				Expect(err).NotTo(HaveOccurred())
			}()

			err = verifyServiceNotExposed(cs, ns.Name, svcNameIPv4, notExposedTimeout)
			Expect(err).NotTo(HaveOccurred(), svcNameIPv4+" should stay pending")
			err = waitForServiceWarningEvent(cs, ns.Name, svcNameIPv4, "SyncLoadBalancerFailed", eventTimeout)
			Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+svcNameIPv4)

			By("Verifying primary service is still working")
			err = verifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort), "TCP")
			Expect(err).NotTo(HaveOccurred(), "Primary service should still have its rule")

			By("Resolving " + svcNameIPv4 + " by removing LB config annotation")
			svc2, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			delete(svc2.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc2, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameIPv4, []*string{&sharedIP})
			Expect(err).NotTo(HaveOccurred(), svcNameIPv4+" should be exposed after removing LB config")

			By("Resolving " + svcNameLBIP + " by removing LB config annotation")
			svc1, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameLBIP, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			delete(svc1.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc1, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameLBIP, []*string{&sharedIP})
			Expect(err).NotTo(HaveOccurred(), svcNameLBIP+" should be exposed after removing LB config")

			By("Verifying primary FIP has rules for all services")
			err = verifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort, svc1Port, svc2Port), "TCP")
			Expect(err).NotTo(HaveOccurred(), "Primary FIP should have rules for all services sharing the IP")

			By("Re-adding LB config annotation to " + svcNameLBIP + ", expecting blocked again")
			svc1, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameLBIP, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			if svc1.Annotations == nil {
				svc1.Annotations = map[string]string{}
			}
			// Pick a different LB than the primary so the service actually moves when IP pin is removed.
			primaryLBName := ptr.Deref(lb.Name, "")
			lbConfig := map[bool]string{true: "lb-2", false: "lb-1"}[strings.HasPrefix(primaryLBName, "lb-1")]
			svc1.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = lbConfig
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc1, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			err = waitForServiceWarningEvent(cs, ns.Name, svcNameLBIP, "SyncLoadBalancerFailed", eventTimeout)
			Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event on update")

			By("Resolving " + svcNameLBIP + " by removing IP pin, expecting service moves to configured LB")
			svc1, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameLBIP, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			svc1.Spec.LoadBalancerIP = ""
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc1, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())
			newIP, err := waitForServiceIPChange(cs, ns.Name, svcNameLBIP, sharedIP, serviceTimeout)
			Expect(err).NotTo(HaveOccurred(), svcNameLBIP+" should get a new IP after removing IP pin")
			Expect(newIP).NotTo(Equal(sharedIP), svcNameLBIP+" should get a new IP on the configured LB")

			lb = getAzureInternalLoadBalancerFromPrivateIP(tc, &newIP, tc.GetResourceGroup())
			newFIPID := getFIPIDForPrivateIP(lb, newIP)
			utils.Logf("%s moved to new FIP: %s with IP %s", svcNameLBIP, newFIPID, newIP)

			By("Verifying final state with primary on original FIP and " + svcNameLBIP + " on new FIP")
			err = verifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort, svc2Port), "TCP")
			Expect(err).NotTo(HaveOccurred(), "Primary FIP should have rules for primary and "+svcNameIPv4)
			err = verifyFIPHasRulesForPorts(tc, newFIPID, sets.New(svc1Port), "TCP")
			Expect(err).NotTo(HaveOccurred(), "New FIP should have "+svcNameLBIP+"'s rule")
		})
	})
})

// getDeploymentPodsNodeNames returns the node names of the pods in the deployment created in BeforeEach.
func getDeploymentPodsNodeNames(kubeClient clientset.Interface, namespace, deploymentName string) (map[string]bool, error) {
	var (
		podList *v1.PodList
		res     = make(map[string]bool)
		err     error
	)
	err = wait.PollUntilContextTimeout(context.Background(), 5*time.Second, 5*time.Minute, false, func(ctx context.Context) (bool, error) {
		podList, err = kubeClient.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		for _, pod := range podList.Items {
			if strings.HasPrefix(pod.Name, deploymentName) {
				if pod.Spec.NodeName == "" {
					utils.Logf("Waiting for pod %s to be running", pod.Name)
					return false, nil
				}
				utils.Logf("Pod %s is running on node %s", pod.Name, pod.Spec.NodeName)
				res[pod.Spec.NodeName] = true
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}

func checkNodeCountInBackendPoolByServiceIPs(tc *utils.AzureTestClient, expectedLBName, bpName string, svcIPs []*string, expectedCount int) error {
	for _, svcIP := range svcIPs {
		lb := getAzureLoadBalancerFromPIP(tc, svcIP, tc.GetResourceGroup(), tc.GetResourceGroup())
		lbName := ptr.Deref(lb.Name, "")
		if !strings.EqualFold(lbName, expectedLBName) {
			return fmt.Errorf("expected load balancer name %s, actual %s", expectedLBName, lbName)
		}

		var found bool
		for _, bp := range lb.Properties.BackendAddressPools {
			if strings.HasPrefix(strings.ToLower(ptr.Deref(bp.Name, "")), strings.ToLower(bpName)) {
				found = true
			}
			if len(bp.Properties.LoadBalancerBackendAddresses) != expectedCount {
				return fmt.Errorf("expected node count %d, actual %d", expectedCount, len(bp.Properties.LoadBalancerBackendAddresses))
			}
		}
		if !found {
			return fmt.Errorf("cannot find backend pool %s in load balancer %s", bpName, lbName)
		}
	}
	return nil
}

func getLBsFromPublicIPs(tc *utils.AzureTestClient, pips []*string) sets.Set[string] {
	lbNames := sets.New[string]()
	for _, svcIP := range pips {
		lb := getAzureLoadBalancerFromPIP(tc, svcIP, tc.GetResourceGroup(), tc.GetResourceGroup())
		lbName := ptr.Deref(lb.Name, "")
		lbNames.Insert(lbName)
	}
	return lbNames
}

// verifyFIPHasRulesForPorts checks that the specified frontend IP config has rules for all expected ports.
func verifyFIPHasRulesForPorts(tc *utils.AzureTestClient, fipID string, expectedPorts sets.Set[int32], protocol string) error {
	if fipID == "" {
		return fmt.Errorf("empty FIP ID")
	}

	// Extract LB name from FIP ID.
	match := lbNameRE.FindStringSubmatch(fipID)
	if len(match) != 2 {
		return fmt.Errorf("could not parse LB name from FIP ID: %s", fipID)
	}
	lbName := match[1]

	lb, err := tc.GetLoadBalancer(tc.GetResourceGroup(), lbName)
	if err != nil {
		return fmt.Errorf("failed to get load balancer %q: %w", lbName, err)
	}

	utils.Logf("Verifying FIP ID %q has rules for ports %v", fipID, expectedPorts.UnsortedList())

	// Find rules that reference the target FIP ID directly.
	foundPorts := sets.New[int32]()
	if lb.Properties != nil && lb.Properties.LoadBalancingRules != nil {
		for _, rule := range lb.Properties.LoadBalancingRules {
			if rule.Properties == nil || rule.Properties.FrontendIPConfiguration == nil {
				continue
			}
			// Compare rule's FIP ID directly with our target.
			ruleFIPID := ptr.Deref(rule.Properties.FrontendIPConfiguration.ID, "")
			if !strings.EqualFold(ruleFIPID, fipID) {
				continue
			}

			rulePort := ptr.Deref(rule.Properties.FrontendPort, 0)
			ruleProtocol := string(ptr.Deref(rule.Properties.Protocol, ""))

			if expectedPorts.Has(rulePort) && strings.EqualFold(ruleProtocol, protocol) {
				utils.Logf("Found rule %q for port %d/%s on FIP", ptr.Deref(rule.Name, ""), rulePort, ruleProtocol)
				foundPorts.Insert(rulePort)
			}
		}
	}

	// Check for missing ports
	missingPorts := expectedPorts.Difference(foundPorts)
	if missingPorts.Len() > 0 {
		return fmt.Errorf("FIP %q is missing rules for ports: %v", fipID, missingPorts.UnsortedList())
	}

	utils.Logf("FIP %q has all expected rules for ports %v", fipID, expectedPorts.UnsortedList())
	return nil
}

// getFIPIDForPrivateIP finds the frontend IP configuration ID for a given private IP.
func getFIPIDForPrivateIP(lb *armnetwork.LoadBalancer, privateIP string) string {
	if lb.Properties == nil || lb.Properties.FrontendIPConfigurations == nil {
		return ""
	}
	for _, fip := range lb.Properties.FrontendIPConfigurations {
		if fip.Properties != nil && fip.Properties.PrivateIPAddress != nil {
			if *fip.Properties.PrivateIPAddress == privateIP {
				return ptr.Deref(fip.ID, "")
			}
		}
	}
	return ""
}

// logAllLoadBalancerStates logs the current state of all load balancers in the resource group.
// It prints LB name, frontend IP count, and load balancing rule count.
func logAllLoadBalancerStates(tc *utils.AzureTestClient, context string) {
	lbs, err := tc.ListLoadBalancers(tc.GetResourceGroup())
	if err != nil {
		utils.Logf("[%s] Failed to list load balancers: %v", context, err)
		return
	}
	utils.Logf("[%s] Load Balancer State (total: %d LBs):", context, len(lbs))
	for _, lb := range lbs {
		lbName := ptr.Deref(lb.Name, "<nil>")
		fipCount := 0
		ruleCount := 0
		if lb.Properties != nil {
			if lb.Properties.FrontendIPConfigurations != nil {
				fipCount = len(lb.Properties.FrontendIPConfigurations)
			}
			if lb.Properties.LoadBalancingRules != nil {
				ruleCount = len(lb.Properties.LoadBalancingRules)
			}
		}
		utils.Logf("  LB %q: %d frontend IPs, %d rules", lbName, fipCount, ruleCount)
		// Log frontend IP details with their associated rules
		if lb.Properties != nil && lb.Properties.FrontendIPConfigurations != nil {
			for _, fip := range lb.Properties.FrontendIPConfigurations {
				fipName := ptr.Deref(fip.Name, "<nil>")
				var ipAddr string
				if fip.Properties != nil {
					if fip.Properties.PrivateIPAddress != nil {
						ipAddr = *fip.Properties.PrivateIPAddress + " (private)"
					} else if fip.Properties.PublicIPAddress != nil && fip.Properties.PublicIPAddress.ID != nil {
						// Extract PIP name from ID
						parts := strings.Split(*fip.Properties.PublicIPAddress.ID, "/")
						ipAddr = "pip:" + parts[len(parts)-1]
					}
				}
				// Get rules associated with this FIP
				var ruleNames []string
				if fip.Properties != nil && fip.Properties.LoadBalancingRules != nil {
					for _, ruleRef := range fip.Properties.LoadBalancingRules {
						if ruleRef.ID != nil {
							// Extract rule name from ID
							parts := strings.Split(*ruleRef.ID, "/")
							ruleNames = append(ruleNames, parts[len(parts)-1])
						}
					}
				}
				if len(ruleNames) > 0 {
					utils.Logf("    FIP %q: %s, rules: [%s]", fipName, ipAddr, strings.Join(ruleNames, ", "))
				} else {
					utils.Logf("    FIP %q: %s, rules: []", fipName, ipAddr)
				}
			}
		}
	}
}

func waitLBCountEqualTo(tc *utils.AzureTestClient, interval, timeout time.Duration, expectedCount int, svcIPs []*string) (sets.Set[string], error) {
	var lbNames sets.Set[string]
	err := wait.PollImmediate(interval, timeout, func() (bool, error) {
		lbNames = getLBsFromPublicIPs(tc, svcIPs)
		if len(lbNames) != expectedCount {
			utils.Logf("Waiting for %d load balancers, found %d", expectedCount, len(lbNames))
			return false, nil
		}
		return true, nil
	})
	return lbNames, err
}

// waitForServiceWarningEvent waits for a Warning event on the service with the expected Reason.
func waitForServiceWarningEvent(
	cs clientset.Interface,
	ns, serviceName string,
	expectedReason string,
	timeout time.Duration,
) error {
	return wait.PollUntilContextTimeout(context.Background(), 10*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		events, err := cs.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Service,type=Warning", serviceName),
		})
		if err != nil {
			return false, err
		}
		for _, event := range events.Items {
			if event.Reason == expectedReason {
				utils.Logf("Found Warning event for service %s: Reason=%s, Message=%s",
					serviceName, event.Reason, event.Message)
				return true, nil
			}
		}
		return false, nil
	})
}

// verifyServiceNotExposed verifies the service does not have a LoadBalancer IP assigned
// within a timeout period. If it gets an IP, returns an error.
// If it stays pending (as expected for blocked services), returns nil after timeout.
func verifyServiceNotExposed(cs clientset.Interface, ns, serviceName string, timeout time.Duration) error {
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		service, err := cs.CoreV1().Services(ns).Get(ctx, serviceName, metav1.GetOptions{})
		if err != nil {
			utils.Logf("Error getting service %s: %v (will retry)", serviceName, err)
			return false, nil // Treat as transient, continue polling.
		}
		if len(service.Status.LoadBalancer.Ingress) > 0 {
			utils.Logf("Service %s unexpectedly got IP: %s", serviceName, service.Status.LoadBalancer.Ingress[0].IP)
			return false, fmt.Errorf("service %s unexpectedly got IP: %s",
				serviceName, service.Status.LoadBalancer.Ingress[0].IP)
		}
		return false, nil // Keep polling, expect it to stay empty.
	})
	// Poll timeout is expected (service stays pending), return nil.
	if wait.Interrupted(err) {
		utils.Logf("Service %s correctly stayed pending (no IP assigned)", serviceName)
		return nil
	}
	utils.Logf("verifyServiceNotExposed for %s returned unexpected error: %v", serviceName, err)
	return err
}

// waitForServiceIPChange waits for a service to get a different IP than the oldIP.
func waitForServiceIPChange(cs clientset.Interface, ns, serviceName, oldIP string, timeout time.Duration) (string, error) {
	var newIP string
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		service, err := cs.CoreV1().Services(ns).Get(ctx, serviceName, metav1.GetOptions{})
		if err != nil {
			utils.Logf("Error getting service %s: %v", serviceName, err)
			return false, nil
		}
		if len(service.Status.LoadBalancer.Ingress) == 0 {
			utils.Logf("Service %s has no IP yet, waiting...", serviceName)
			return false, nil
		}
		currentIP := service.Status.LoadBalancer.Ingress[0].IP
		if currentIP != oldIP {
			utils.Logf("Service %s IP changed from %s to %s", serviceName, oldIP, currentIP)
			newIP = currentIP
			return true, nil
		}
		utils.Logf("Service %s still has old IP %s, waiting for change...", serviceName, oldIP)
		return false, nil
	})
	if err != nil {
		return "", fmt.Errorf("timeout waiting for service %s IP to change from %s: %w", serviceName, oldIP, err)
	}
	return newIP, nil
}
