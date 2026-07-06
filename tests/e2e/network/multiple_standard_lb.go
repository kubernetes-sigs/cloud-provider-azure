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

	DescribeTable("should place all services sharing an IP on the same load balancer",
		func(mode string) {
			isInternal := mode == "internal"
			useUserPIP := mode == "external-user-pip"

			var svcAnnotations map[string]string
			if isInternal {
				svcAnnotations = serviceAnnotationLoadBalancerInternalTrue
			}

			var sharedIP string
			if useUserPIP {
				By("Creating a user-assigned public IP")
				ipName := fmt.Sprintf("%s-shared-pip-%s", basename, ns.Name[:8])
				pip, err := utils.WaitCreatePIP(tc, ipName, tc.GetResourceGroup(), defaultPublicIPAddress(ipName, false))
				Expect(err).NotTo(HaveOccurred())
				sharedIP = ptr.Deref(pip.Properties.IPAddress, "")
				utils.Logf("Created user-assigned PIP with address %s", sharedIP)
				defer func() {
					By("Cleaning up PIP")
					err = utils.DeletePIPWithRetry(tc, ipName, "")
					Expect(err).NotTo(HaveOccurred())
				}()
			}

			var firstFIPID string
			serviceCount := 2
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
				servicePort := []v1.ServicePort{{
					Port:       tcpPort,
					TargetPort: intstr.FromInt(int(tcpPort)),
				}}
				service := utils.CreateLoadBalancerServiceManifest(serviceName, svcAnnotations, serviceLabels, ns.Name, servicePort)
				if sharedIP != "" {
					service = updateServiceLBIPs(service, isInternal, []*string{&sharedIP})
					utils.Logf("Creating Service %q with shared IP %s", serviceName, sharedIP)
				} else {
					utils.Logf("Creating Service %q (will get new IP)", serviceName)
				}
				_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func(name string) {
					err = utils.DeleteService(cs, ns.Name, name)
					Expect(err).NotTo(HaveOccurred())
				}(serviceName)

				var targetIPs []*string
				if sharedIP != "" {
					targetIPs = []*string{&sharedIP}
				}
				ips, err := utils.WaitServiceExposureAndValidateConnectivity(cs, tc.IPFamily, ns.Name, serviceName, targetIPs)
				Expect(err).NotTo(HaveOccurred())
				Expect(len(ips)).NotTo(BeZero())
				if sharedIP == "" {
					sharedIP = *ips[0]
				}

				fipID, lbName := getFIPIDAndLBName(tc, sharedIP, isInternal)
				utils.Logf("After service %d: IP %s on LB %s, FIP: %s", i, sharedIP, lbName, fipID)
				if i == 0 {
					firstFIPID = fipID
				}
			}

			By("Verifying first FIP has all services' rules")
			err := utils.VerifyFIPHasRulesForPorts(tc, firstFIPID, servicePorts, "TCP")
			Expect(err).NotTo(HaveOccurred(), "First FIP should have rules for all services sharing the IP")
		},
		Entry("external with user-assigned PIP", "external-user-pip"),
		Entry("external with managed PIP", "external-managed"),
		Entry("internal", "internal"),
	)

	Describe("LB Placement Conflicts", func() {
		const (
			svcNamePrimary = "svc-primary"
			svcNameLBIP    = "svc-lbip"
			svcNamePIPName = "svc-pipname"
			svcNameIPv4    = "svc-ipv4"

			msgConflictingLBConfig = "conflicting load balancer configuration"
		)

		DescribeTable("should block services with conflicting IP and LB config",
			func(mode string) {
				isInternal := mode == "internal"
				useUserPIP := mode == "external-user-pip"

				var svcAnnotations map[string]string
				if isInternal {
					svcAnnotations = serviceAnnotationLoadBalancerInternalTrue
				}

				var sharedIP, pipName string
				if useUserPIP {
					By("Creating user-assigned PIP")
					pipName = "conflict-test-pip"
					pip, err := utils.WaitCreatePIP(tc, pipName, tc.GetResourceGroup(), defaultPublicIPAddress(pipName, false))
					Expect(err).NotTo(HaveOccurred())
					sharedIP = ptr.Deref(pip.Properties.IPAddress, "")
					Expect(sharedIP).NotTo(BeEmpty())
					defer func() {
						utils.Logf("Cleaning up PIP %s", pipName)
						err := utils.DeletePIPWithRetry(tc, pipName, "")
						Expect(err).NotTo(HaveOccurred())
					}()
				}

				By("Creating primary service")
				primaryPort := int32(serverPort)
				primaryService := utils.CreateLoadBalancerServiceManifest(svcNamePrimary, svcAnnotations, labels, ns.Name, []v1.ServicePort{{
					Port:       primaryPort,
					TargetPort: intstr.FromInt(int(primaryPort)),
				}})
				if sharedIP != "" {
					primaryService = updateServiceLBIPs(primaryService, false, []*string{&sharedIP})
				}
				_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), primaryService, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					err := utils.DeleteService(cs, ns.Name, svcNamePrimary)
					Expect(err).NotTo(HaveOccurred())
				}()

				if sharedIP == "" {
					ips, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, svcNamePrimary)
					Expect(err).NotTo(HaveOccurred())
					Expect(len(ips)).NotTo(BeZero())
					sharedIP = *ips[0]
				} else {
					_, err = utils.WaitServiceExposure(cs, ns.Name, svcNamePrimary, []*string{&sharedIP})
					Expect(err).NotTo(HaveOccurred())
				}

				primaryFIPID, primaryLBName := getFIPIDAndLBName(tc, sharedIP, isInternal)
				utils.Logf("Primary service exposed with IP %s on LB %s, FIP: %s", sharedIP, primaryLBName, primaryFIPID)

				type conflictSvc struct {
					name string
					port int32
					svc  *v1.Service
				}
				nextPort := int32(serverPort + 1)

				svcLBIPPort := nextPort
				nextPort++
				svcLBIP := utils.CreateLoadBalancerServiceManifest(svcNameLBIP, svcAnnotations, labels, ns.Name, []v1.ServicePort{{
					Port:       svcLBIPPort,
					TargetPort: intstr.FromInt(int(svcLBIPPort)),
				}})
				svcLBIP.Spec.LoadBalancerIP = sharedIP
				svcLBIP.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"

				svcIPv4Port := nextPort
				nextPort++
				svcIPv4 := utils.CreateLoadBalancerServiceManifest(svcNameIPv4, svcAnnotations, labels, ns.Name, []v1.ServicePort{{
					Port:       svcIPv4Port,
					TargetPort: intstr.FromInt(int(svcIPv4Port)),
				}})
				svcIPv4 = updateServiceLBIPs(svcIPv4, isInternal, []*string{&sharedIP})
				svcIPv4.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"

				conflicts := []conflictSvc{
					{name: svcNameLBIP, port: svcLBIPPort, svc: svcLBIP},
					{name: svcNameIPv4, port: svcIPv4Port, svc: svcIPv4},
				}

				if useUserPIP {
					svcPIPPort := nextPort
					nextPort++
					svcPIP := utils.CreateLoadBalancerServiceManifest(svcNamePIPName, nil, labels, ns.Name, []v1.ServicePort{{
						Port:       svcPIPPort,
						TargetPort: intstr.FromInt(int(svcPIPPort)),
					}})
					svcPIP = updateServicePIPNames(tc.IPFamily, svcPIP, []string{pipName})
					svcPIP.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"
					conflicts = append(conflicts, conflictSvc{name: svcNamePIPName, port: svcPIPPort, svc: svcPIP})
				}

				for _, c := range conflicts {
					By(fmt.Sprintf("Creating %s with conflicting config, expecting blocked", c.name))
					_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), c.svc, metav1.CreateOptions{})
					Expect(err).NotTo(HaveOccurred())
					defer func(name string) {
						err := utils.DeleteService(cs, ns.Name, name)
						Expect(err).NotTo(HaveOccurred())
					}(c.name)

					err = utils.WaitForServiceWarningEventAfter(cs, ns.Name, c.name, "SyncLoadBalancerFailed", msgConflictingLBConfig, time.Time{})
					Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+c.name)
				}

				By("Verifying primary service is still working")
				err = utils.VerifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Primary service should still have its rule")

				allPorts := sets.New(primaryPort)
				for _, c := range conflicts {
					By(fmt.Sprintf("Resolving %s by removing LB config annotation", c.name))
					svc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), c.name, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					delete(svc.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
					_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())
					_, err = utils.WaitServiceExposure(cs, ns.Name, c.name, []*string{&sharedIP})
					Expect(err).NotTo(HaveOccurred(), c.name+" should be exposed after removing LB config")
					allPorts.Insert(c.port)
				}

				By("Verifying primary FIP has rules for all services")
				err = utils.VerifyFIPHasRulesForPorts(tc, primaryFIPID, allPorts, "TCP")
				Expect(err).NotTo(HaveOccurred(), "Primary FIP should have rules for all services sharing the IP")

				By("Adding LB annotation to " + svcNameIPv4 + " while it still shares IP, expecting blocked")
				targetLBAnnotation := "lb-1"
				if primaryLBName == "lb-1" || primaryLBName == "lb-1-internal" {
					targetLBAnnotation = os.Getenv("CLUSTER_NAME")
				}
				beforeUpdate := time.Now()
				svcIPv4, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				svcIPv4.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = targetLBAnnotation
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svcIPv4, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				err = utils.WaitForServiceWarningEventAfter(cs, ns.Name, svcNameIPv4, "SyncLoadBalancerFailed", msgConflictingLBConfig, beforeUpdate)
				Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+svcNameIPv4)

				By("Removing IP annotation from " + svcNameIPv4 + " to resolve conflict")
				beforeIPRemove := time.Now()
				svcIPv4, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				delete(svcIPv4.Annotations, consts.ServiceAnnotationLoadBalancerIPDualStack[false])
				delete(svcIPv4.Annotations, consts.ServiceAnnotationLoadBalancerIPDualStack[true])
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svcIPv4, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				if isInternal {
					By("Expecting PrivateIPAddressIsAllocated failure for internal service")
					err = utils.WaitForServiceWarningEventAfter(cs, ns.Name, svcNameIPv4, "SyncLoadBalancerFailed", "PrivateIPAddressIsAllocated", beforeIPRemove)
					Expect(err).NotTo(HaveOccurred(), "Internal service should fail to move because the private IP is still allocated on the old LB")

					By("Re-adding IP annotation and removing LB annotation to resolve")
					beforeResolve := time.Now()
					svcIPv4, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
					Expect(err).NotTo(HaveOccurred())
					svcIPv4.Annotations[consts.ServiceAnnotationLoadBalancerIPDualStack[false]] = sharedIP
					delete(svcIPv4.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
					_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svcIPv4, metav1.UpdateOptions{})
					Expect(err).NotTo(HaveOccurred())

					err = utils.WaitForServiceNormalEventAfter(cs, ns.Name, svcNameIPv4, "EnsuredLoadBalancer", "", beforeResolve)
					Expect(err).NotTo(HaveOccurred(), svcNameIPv4+" should be reconciled after re-adding IP and removing LB annotation")

					By("Verifying primary FIP still has rules for all services")
					err = utils.VerifyFIPHasRulesForPorts(tc, primaryFIPID, allPorts, "TCP")
					Expect(err).NotTo(HaveOccurred(), "Primary FIP should still have rules for all services sharing the IP")
				} else {
					By("Waiting for " + svcNameIPv4 + " to get a new IP on " + targetLBAnnotation)
					newIP, err := utils.WaitForServiceIPChange(cs, ns.Name, svcNameIPv4, sharedIP)
					Expect(err).NotTo(HaveOccurred())
					newFIPID := getPIPFrontendConfigurationID(tc, newIP, tc.GetResourceGroup(), true)
					utils.Logf("%s moved to new IP %s on FIP: %s", svcNameIPv4, newIP, newFIPID)

					By("Verifying primary FIP no longer has " + svcNameIPv4 + "'s rules")
					remainingPorts := allPorts.Difference(sets.New(svcIPv4Port))
					err = utils.VerifyFIPHasRulesForPorts(tc, primaryFIPID, remainingPorts, "TCP")
					Expect(err).NotTo(HaveOccurred(), "Primary FIP should only have remaining services' rules")

					By("Verifying " + svcNameIPv4 + " has rules on the new FIP")
					err = utils.VerifyFIPHasRulesForPorts(tc, newFIPID, sets.New(svcIPv4Port), "TCP")
					Expect(err).NotTo(HaveOccurred(), svcNameIPv4+" should have its rule on the new FIP")
				}
			},
			Entry("external with user-assigned PIP", "external-user-pip"),
			Entry("external with managed PIP", "external-managed"),
			Entry("internal", "internal"),
		)

		DescribeTable("should block primary service from moving LB when there is IP sharing",
			func(isInternal bool) {
				clusterName := os.Getenv("CLUSTER_NAME")

				var svcAnnotations map[string]string
				if isInternal {
					svcAnnotations = serviceAnnotationLoadBalancerInternalTrue
				}

				By("Creating primary service on cluster LB")
				primaryPort := int32(serverPort)
				primaryService := utils.CreateLoadBalancerServiceManifest(svcNamePrimary, svcAnnotations, labels, ns.Name, []v1.ServicePort{{
					Port:       primaryPort,
					TargetPort: intstr.FromInt(int(primaryPort)),
				}})
				primaryService.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = clusterName
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

				primaryFIPID, primaryLBName := getFIPIDAndLBName(tc, sharedIP, isInternal)
				utils.Logf("Primary service exposed with IP %s on LB %s, FIP: %s", sharedIP, primaryLBName, primaryFIPID)

				By("Creating secondary service sharing the IP")
				secondaryPort := int32(serverPort + 1)
				secondaryService := utils.CreateLoadBalancerServiceManifest(svcNameIPv4, svcAnnotations, labels, ns.Name, []v1.ServicePort{{
					Port:       secondaryPort,
					TargetPort: intstr.FromInt(int(secondaryPort)),
				}})
				secondaryService = updateServiceLBIPs(secondaryService, isInternal, []*string{&sharedIP})
				_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), secondaryService, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					err := utils.DeleteService(cs, ns.Name, svcNameIPv4)
					Expect(err).NotTo(HaveOccurred())
				}()

				_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameIPv4, []*string{&sharedIP})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying both services share the same FIP")
				err = utils.VerifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort, secondaryPort), "TCP")
				Expect(err).NotTo(HaveOccurred())

				By("Primary service changes LB annotation to lb-1, expecting blocked")
				beforeUpdate := time.Now()
				primaryService, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNamePrimary, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				primaryService.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), primaryService, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				err = utils.WaitForServiceWarningEventAfter(cs, ns.Name, svcNamePrimary, "SyncLoadBalancerFailed", "cannot migrate", beforeUpdate)
				Expect(err).NotTo(HaveOccurred(), "Expected warning event for primary service trying to move")

				By("Verifying both services still share IP on original LB")
				err = utils.VerifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort, secondaryPort), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Services should still share IP on original LB")

				By("Primary service removes LB annotation")
				primaryService, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNamePrimary, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				delete(primaryService.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), primaryService, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying primary FIP still has rules for all services after removing LB annotation")
				err = utils.VerifyFIPHasRulesForPorts(tc, primaryFIPID, sets.New(primaryPort, secondaryPort), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Services should still share IP after removing LB annotation")
			},
			Entry("external", false),
			Entry("internal", true),
		)

		DescribeTable("should allow service with no prior IP sharing to share IP",
			func(mode string) {
				isInternal := mode == "internal"
				useUserPIP := mode == "external-user-pip"
				clusterName := os.Getenv("CLUSTER_NAME")

				var svcAnnotations map[string]string
				if isInternal {
					svcAnnotations = serviceAnnotationLoadBalancerInternalTrue
				}

				var sharedIP string
				var pipName string
				if useUserPIP {
					By("Creating user-assigned PIP")
					pipName = "cross-lb-test-pip"
					pip, err := utils.WaitCreatePIP(tc, pipName, tc.GetResourceGroup(), defaultPublicIPAddress(pipName, false))
					Expect(err).NotTo(HaveOccurred())
					sharedIP = ptr.Deref(pip.Properties.IPAddress, "")
					Expect(sharedIP).NotTo(BeEmpty())
					defer func() {
						utils.Logf("Cleaning up PIP %s", pipName)
						err := utils.DeletePIPWithRetry(tc, pipName, "")
						Expect(err).NotTo(HaveOccurred())
					}()
				}

				By("Creating Service 1")
				svc1Port := int32(serverPort)
				svc1 := utils.CreateLoadBalancerServiceManifest(svcNamePrimary, svcAnnotations, labels, ns.Name, []v1.ServicePort{{
					Port:       svc1Port,
					TargetPort: intstr.FromInt(int(svc1Port)),
				}})
				if useUserPIP {
					svc1 = updateServicePIPNames(tc.IPFamily, svc1, []string{pipName})
				} else {
					svc1.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = clusterName
				}
				_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc1, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					err := utils.DeleteService(cs, ns.Name, svcNamePrimary)
					Expect(err).NotTo(HaveOccurred())
				}()

				if useUserPIP {
					_, err = utils.WaitServiceExposure(cs, ns.Name, svcNamePrimary, []*string{&sharedIP})
					Expect(err).NotTo(HaveOccurred())
				} else {
					ips1, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, svcNamePrimary)
					Expect(err).NotTo(HaveOccurred())
					Expect(len(ips1)).NotTo(BeZero())
					sharedIP = *ips1[0]
				}

				svc1FIPID, svc1LBName := getFIPIDAndLBName(tc, sharedIP, isInternal)
				utils.Logf("Service 1 exposed with IP %s on LB %s, FIP: %s", sharedIP, svc1LBName, svc1FIPID)

				svc2LBAnnotation := "lb-1"
				if svc1LBName == "lb-1" || svc1LBName == "lb-1-internal" {
					svc2LBAnnotation = clusterName
				}

				By(fmt.Sprintf("Creating Service 2 with LB annotation pointing to %s", svc2LBAnnotation))
				svc2Port := int32(serverPort + 1)
				svc2 := utils.CreateLoadBalancerServiceManifest(svcNameIPv4, svcAnnotations, labels, ns.Name, []v1.ServicePort{{
					Port:       svc2Port,
					TargetPort: intstr.FromInt(int(svc2Port)),
				}})
				svc2.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = svc2LBAnnotation
				_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc2, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					err := utils.DeleteService(cs, ns.Name, svcNameIPv4)
					Expect(err).NotTo(HaveOccurred())
				}()

				ips2, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, svcNameIPv4)
				Expect(err).NotTo(HaveOccurred())
				Expect(len(ips2)).NotTo(BeZero())
				svc2IP := *ips2[0]
				Expect(svc2IP).NotTo(Equal(sharedIP), "Service 2 should have a different IP on "+svc2LBAnnotation)

				svc2OldFIPID, svc2OldLBName := getFIPIDAndLBName(tc, svc2IP, isInternal)
				utils.Logf("Service 2 exposed with IP %s on LB %s, FIP: %s", svc2IP, svc2OldLBName, svc2OldFIPID)

				By("Service 2 adds IP annotation pointing to Service 1's IP, expecting blocked")
				svc2, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				svc2 = updateServiceLBIPs(svc2, isInternal, []*string{&sharedIP})
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc2, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				err = utils.WaitForServiceWarningEventAfter(cs, ns.Name, svcNameIPv4, "SyncLoadBalancerFailed", msgConflictingLBConfig, time.Time{})
				Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+svcNameIPv4)

				By("Verifying Service 1 is still working")
				err = utils.VerifyFIPHasRulesForPorts(tc, svc1FIPID, sets.New(svc1Port), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Service 1 should still have its rule")

				By("Service 2 removes IP annotation instead, keeping LB annotation")
				beforeIPRemove := time.Now()
				svc2, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				delete(svc2.Annotations, consts.ServiceAnnotationLoadBalancerIPDualStack[false])
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc2, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				By("Waiting for successful reconcile after removing IP annotation")
				err = utils.WaitForServiceNormalEventAfter(cs, ns.Name, svcNameIPv4, "EnsuredLoadBalancer", "", beforeIPRemove)
				Expect(err).NotTo(HaveOccurred(), "Expected EnsuredLoadBalancer event after removing IP annotation")

				By("Verifying Service 2 stays on its original LB with its original IP")
				_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameIPv4, []*string{&svc2IP})
				Expect(err).NotTo(HaveOccurred(), svcNameIPv4+" should stay on its original LB after removing IP annotation")
				err = utils.VerifyFIPHasRulesForPorts(tc, svc2OldFIPID, sets.New(svc2Port), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Service 2 should still have its rule on its original FIP")

				By("Verifying Service 1 is unaffected")
				err = utils.VerifyFIPHasRulesForPorts(tc, svc1FIPID, sets.New(svc1Port), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Service 1 should still have its rule")

				By("Service 2 adds back IP annotation, expecting conflict again")
				beforeIPReAdd := time.Now()
				svc2, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				svc2 = updateServiceLBIPs(svc2, isInternal, []*string{&sharedIP})
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc2, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				err = utils.WaitForServiceWarningEventAfter(cs, ns.Name, svcNameIPv4, "SyncLoadBalancerFailed", msgConflictingLBConfig, beforeIPReAdd)
				Expect(err).NotTo(HaveOccurred(), "Expected conflict warning after re-adding IP annotation")

				By("Service 2 removes LB annotation to resolve conflict")
				svc2, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				delete(svc2.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc2, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())
				_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameIPv4, []*string{&sharedIP})
				Expect(err).NotTo(HaveOccurred(), svcNameIPv4+" should be exposed after removing LB config")

				By("Verifying primary FIP has rules for all services")
				err = utils.VerifyFIPHasRulesForPorts(tc, svc1FIPID, sets.New(svc1Port, svc2Port), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Both services should share IP on the primary LB")

				By("Verifying Service 2's old FIP has no rules for its port")
				err = utils.VerifyFIPHasNoRulesForPorts(tc, svc2OldFIPID, sets.New(svc2Port), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Service 2's old FIP should have no rules after moving")
			},
			Entry("external with user-assigned PIP", "external-user-pip"),
			Entry("external with managed PIP", "external-managed"),
			Entry("internal", "internal"),
		)

		DescribeTable("should clean up stale rules when last secondary moves after primary deleted",
			func(mode string) {
				isInternal := mode == "internal"
				useUserPIP := mode == "external-user-pip"
				clusterName := os.Getenv("CLUSTER_NAME")

				var svcAnnotations map[string]string
				if isInternal {
					svcAnnotations = serviceAnnotationLoadBalancerInternalTrue
				}

				var sharedIP, pipName string
				if useUserPIP {
					By("Creating user-assigned PIP")
					pipName = "orphan-cleanup-test-pip"
					pip, err := utils.WaitCreatePIP(tc, pipName, tc.GetResourceGroup(), defaultPublicIPAddress(pipName, false))
					Expect(err).NotTo(HaveOccurred())
					sharedIP = ptr.Deref(pip.Properties.IPAddress, "")
					Expect(sharedIP).NotTo(BeEmpty())
					defer func() {
						utils.Logf("Cleaning up PIP %s", pipName)
						err := utils.DeletePIPWithRetry(tc, pipName, "")
						Expect(err).NotTo(HaveOccurred())
					}()
				}

				By("Creating Service 1 (primary)")
				svc1Port := int32(serverPort)
				svc1 := utils.CreateLoadBalancerServiceManifest(svcNamePrimary, svcAnnotations, labels, ns.Name, []v1.ServicePort{{
					Port:       svc1Port,
					TargetPort: intstr.FromInt(int(svc1Port)),
				}})
				if useUserPIP {
					svc1 = updateServicePIPNames(tc.IPFamily, svc1, []string{pipName})
				} else {
					svc1.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"
				}
				_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc1, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					err := utils.DeleteService(cs, ns.Name, svcNamePrimary)
					Expect(err).NotTo(HaveOccurred())
				}()

				if sharedIP == "" {
					ips1, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, svcNamePrimary)
					Expect(err).NotTo(HaveOccurred())
					Expect(len(ips1)).NotTo(BeZero())
					sharedIP = *ips1[0]
				} else {
					_, err = utils.WaitServiceExposure(cs, ns.Name, svcNamePrimary, []*string{&sharedIP})
					Expect(err).NotTo(HaveOccurred())
				}

				svc1FIPID, svc1LBName := getFIPIDAndLBName(tc, sharedIP, isInternal)
				utils.Logf("Service 1 exposed with IP %s on %s, FIP: %s", sharedIP, svc1LBName, svc1FIPID)

				By("Creating Service 2 (secondary) sharing Service 1's IP via IP annotation")
				svc2Port := int32(serverPort + 1)
				svc2 := utils.CreateLoadBalancerServiceManifest(svcNameIPv4, svcAnnotations, labels, ns.Name, []v1.ServicePort{{
					Port:       svc2Port,
					TargetPort: intstr.FromInt(int(svc2Port)),
				}})
				svc2 = updateServiceLBIPs(svc2, isInternal, []*string{&sharedIP})
				_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc2, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					err := utils.DeleteService(cs, ns.Name, svcNameIPv4)
					Expect(err).NotTo(HaveOccurred())
				}()

				_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameIPv4, []*string{&sharedIP})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying both services have rules on the shared FIP")
				err = utils.VerifyFIPHasRulesForPorts(tc, svc1FIPID, sets.New(svc1Port, svc2Port), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Both services should share the FIP")

				By("Deleting Service 1")
				err = utils.DeleteService(cs, ns.Name, svcNamePrimary)
				Expect(err).NotTo(HaveOccurred())

				By("Verifying FIP still has Service 2's rules but not Service 1's")
				err = utils.VerifyFIPHasRulesForPorts(tc, svc1FIPID, sets.New(svc2Port), "TCP")
				Expect(err).NotTo(HaveOccurred(), "FIP should retain Service 2's rules after Service 1 deletion")

				targetLBAnnotation := "lb-1"
				if svc1LBName == "lb-1" || svc1LBName == "lb-1-internal" {
					targetLBAnnotation = clusterName
				}
				By(fmt.Sprintf("Service 2 adds LB annotation %q, expecting blocked", targetLBAnnotation))
				svc2, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				svc2.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = targetLBAnnotation
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc2, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				err = utils.WaitForServiceWarningEventAfter(cs, ns.Name, svcNameIPv4, "SyncLoadBalancerFailed", msgConflictingLBConfig, time.Time{})
				Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+svcNameIPv4)

				By("Service 2 removes IP annotation to resolve conflict")
				beforeIPRemove := time.Now()
				svc2, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				delete(svc2.Annotations, consts.ServiceAnnotationLoadBalancerIPDualStack[false])
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc2, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				if isInternal {
					err = utils.WaitForServiceNormalEventAfter(cs, ns.Name, svcNameIPv4, "EnsuredLoadBalancer", "", beforeIPRemove)
					Expect(err).NotTo(HaveOccurred(), "Service 2 should be reconciled on "+targetLBAnnotation)
				} else {
					svc2NewIP, err := utils.WaitForServiceIPChange(cs, ns.Name, svcNameIPv4, sharedIP)
					Expect(err).NotTo(HaveOccurred(), "Service 2 should get a new IP on "+targetLBAnnotation)
					utils.Logf("Service 2 now on %s with IP %s", targetLBAnnotation, svc2NewIP)
				}

				By("Verifying old FIP is removed")
				err = utils.VerifyFIPRemoved(tc, svc1FIPID)
				Expect(err).NotTo(HaveOccurred(), "FIP should be removed after last service moved away")

				if useUserPIP {
					By("Verifying user-assigned PIP is not removed")
					userPIP, err := tc.GetPublicIPFromAddress(tc.GetResourceGroup(), &sharedIP)
					Expect(err).NotTo(HaveOccurred())
					Expect(userPIP).NotTo(BeNil(), "User-assigned PIP should still exist")
				} else {
					By("Verifying LB is deleted since it has no remaining FIPs")
					_, lbErr := tc.GetLoadBalancer(tc.GetResourceGroup(), svc1LBName)
					Expect(lbErr).To(HaveOccurred(), svc1LBName+" should be deleted after all FIPs are removed")
					Expect(strings.Contains(lbErr.Error(), "NotFound")).To(BeTrue(), "Expected NotFound error for "+svc1LBName)

					if !isInternal {
						By("Verifying managed PIP is removed")
						managedPIP, err := tc.GetPublicIPFromAddress(tc.GetResourceGroup(), &sharedIP)
						Expect(err).NotTo(HaveOccurred())
						Expect(managedPIP).To(BeNil(), "Managed PIP should be deleted after last service moved away")
					}
				}
			},
			Entry("with user-assigned PIP", "external-user-pip"),
			Entry("with managed PIP", "external-managed"),
			Entry("internal", "internal"),
		)

		DescribeTable("should clean up rules and preserve LB placement when a service sharing IP switches between internal and external",
			func(startInternal, moveToDifferentLB, switchPrimary bool) {
				clusterName := os.Getenv("CLUSTER_NAME")

				By("Creating Service 1 (primary)")
				svc1Port := int32(serverPort)
				svc1Annotations := map[string]string{}
				if startInternal {
					svc1Annotations[consts.ServiceAnnotationLoadBalancerInternal] = "true"
				}
				if moveToDifferentLB {
					svc1Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = clusterName
				}
				svc1 := utils.CreateLoadBalancerServiceManifest(svcNamePrimary, svc1Annotations, labels, ns.Name, []v1.ServicePort{{
					Port:       svc1Port,
					TargetPort: intstr.FromInt(int(svc1Port)),
				}})
				_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc1, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					err := utils.DeleteService(cs, ns.Name, svcNamePrimary)
					Expect(err).NotTo(HaveOccurred())
				}()

				ips1, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, svcNamePrimary)
				Expect(err).NotTo(HaveOccurred())
				Expect(len(ips1)).NotTo(BeZero())
				sharedIP := *ips1[0]

				svc1FIPID, svc1LBName := getFIPIDAndLBName(tc, sharedIP, startInternal)
				utils.Logf("Service 1 exposed with IP %s on LB %s, FIP: %s", sharedIP, svc1LBName, svc1FIPID)

				By("Creating Service 2 (secondary) sharing Service 1's IP via IP annotation")
				svc2Port := int32(serverPort + 1)
				var svc2Annotations map[string]string
				if startInternal {
					svc2Annotations = serviceAnnotationLoadBalancerInternalTrue
				}
				svc2 := utils.CreateLoadBalancerServiceManifest(svcNameIPv4, svc2Annotations, labels, ns.Name, []v1.ServicePort{{
					Port:       svc2Port,
					TargetPort: intstr.FromInt(int(svc2Port)),
				}})
				svc2 = updateServiceLBIPs(svc2, startInternal, []*string{&sharedIP})
				_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc2, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
				defer func() {
					err := utils.DeleteService(cs, ns.Name, svcNameIPv4)
					Expect(err).NotTo(HaveOccurred())
				}()

				_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameIPv4, []*string{&sharedIP})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying both services have rules on the shared FIP")
				err = utils.VerifyFIPHasRulesForPorts(tc, svc1FIPID, sets.New(svc1Port, svc2Port), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Both services should share the FIP")

				switchTo := "internal"
				if startInternal {
					switchTo = "external"
				}
				switchSvcRole := "secondary"
				switchSvcName := svcNameIPv4
				if switchPrimary {
					switchSvcRole = "primary"
					switchSvcName = svcNamePrimary
				}
				switchSvcPort := svc2Port
				if switchPrimary {
					switchSvcPort = svc1Port
				}
				remainingPort := svc1Port
				if switchPrimary {
					remainingPort = svc2Port
				}

				targetLB := ""
				if moveToDifferentLB {
					targetLB = " and moves to lb-1"
				}
				By(fmt.Sprintf("%s service switches to %s%s", switchSvcRole, switchTo, targetLB))
				switchSvc, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), switchSvcName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				if switchSvc.Annotations == nil {
					switchSvc.Annotations = map[string]string{}
				}
				if startInternal {
					delete(switchSvc.Annotations, consts.ServiceAnnotationLoadBalancerInternal)
				} else {
					switchSvc.Annotations[consts.ServiceAnnotationLoadBalancerInternal] = "true"
				}
				if moveToDifferentLB {
					switchSvc.Annotations[consts.ServiceAnnotationLoadBalancerConfigurations] = "lb-1"
				}
				delete(switchSvc.Annotations, consts.ServiceAnnotationLoadBalancerIPDualStack[false])
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), switchSvc, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				newIP, err := utils.WaitForServiceIPChange(cs, ns.Name, switchSvcName, sharedIP)
				Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("%s service should get a new %s IP", switchSvcRole, switchTo))

				By(fmt.Sprintf("Verifying %s service is on the correct %s LB", switchSvcRole, switchTo))
				switchedToInternal := switchTo == "internal"
				newFIPID, newLBName := getFIPIDAndLBName(tc, newIP, switchedToInternal)
				utils.Logf("%s service now on LB %s with IP %s, FIP: %s", switchSvcRole, newLBName, newIP, newFIPID)
				Expect(newFIPID).NotTo(BeEmpty(), switchSvcRole+" service should have a FIP on the new LB")

				var expectedLBAfterSwitch string
				if moveToDifferentLB {
					expectedLBAfterSwitch = "lb-1"
					if switchedToInternal {
						expectedLBAfterSwitch += consts.InternalLoadBalancerNameSuffix
					}
				} else {
					if switchedToInternal {
						expectedLBAfterSwitch = svc1LBName + consts.InternalLoadBalancerNameSuffix
					} else {
						expectedLBAfterSwitch = strings.TrimSuffix(svc1LBName, consts.InternalLoadBalancerNameSuffix)
					}
				}
				Expect(newLBName).To(Equal(expectedLBAfterSwitch), fmt.Sprintf("%s service should be on LB %s", switchSvcRole, expectedLBAfterSwitch))

				err = utils.VerifyFIPHasRulesForPorts(tc, newFIPID, sets.New(switchSvcPort), "TCP")
				Expect(err).NotTo(HaveOccurred(), switchSvcRole+" service should have rules on its new FIP")

				By("Verifying old FIP only has the remaining service's rules")
				err = utils.VerifyFIPHasRulesForPorts(tc, svc1FIPID, sets.New(remainingPort), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Switched service's rules should be cleaned from the old FIP")

				By(fmt.Sprintf("Triggering reconcile and verifying %s service stays on LB %s", switchSvcRole, newLBName))
				beforeReconcile := time.Now().Add(-1 * time.Second)
				addDummyAnnotationWithServiceName(cs, ns.Name, switchSvcName)
				err = utils.WaitForServiceNormalEventAfter(cs, ns.Name, switchSvcName, "EnsuredLoadBalancer", "", beforeReconcile)
				Expect(err).NotTo(HaveOccurred(), "Reconcile should succeed after dummy annotation")
				reconFIPID, reconLBName := getFIPIDAndLBName(tc, newIP, switchedToInternal)
				Expect(reconLBName).To(Equal(newLBName), switchSvcRole+" service should stay on the same LB after reconcile")
				Expect(reconFIPID).To(Equal(newFIPID), switchSvcRole+" service should keep the same FIP after reconcile")

				switchBack := "external"
				if startInternal {
					switchBack = "internal"
				}
				By(fmt.Sprintf("%s service switches back to %s", switchSvcRole, switchBack))
				switchSvc, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), switchSvcName, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				if switchSvc.Annotations == nil {
					switchSvc.Annotations = map[string]string{}
				}
				if startInternal {
					switchSvc.Annotations[consts.ServiceAnnotationLoadBalancerInternal] = "true"
				} else {
					delete(switchSvc.Annotations, consts.ServiceAnnotationLoadBalancerInternal)
				}
				if moveToDifferentLB {
					delete(switchSvc.Annotations, consts.ServiceAnnotationLoadBalancerConfigurations)
				}
				if !switchPrimary {
					switchSvc = updateServiceLBIPs(switchSvc, startInternal, []*string{&sharedIP})
				}
				_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), switchSvc, metav1.UpdateOptions{})
				Expect(err).NotTo(HaveOccurred())

				_, err = utils.WaitServiceExposure(cs, ns.Name, switchSvcName, []*string{&sharedIP})
				Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("%s service should get back shared IP %s", switchSvcRole, sharedIP))

				By("Verifying service is back on the original LB")
				switchBackFIPID, switchBackLBName := getFIPIDAndLBName(tc, sharedIP, startInternal)
				utils.Logf("%s service back on LB %s with IP %s, FIP: %s", switchSvcRole, switchBackLBName, sharedIP, switchBackFIPID)
				Expect(switchBackLBName).To(Equal(svc1LBName), switchSvcRole+" service should be back on the original LB")
				Expect(switchBackFIPID).To(Equal(svc1FIPID), "Should be back on the original FIP")

				By("Verifying both services' rules are back on the shared FIP")
				err = utils.VerifyFIPHasRulesForPorts(tc, svc1FIPID, sets.New(svc1Port, svc2Port), "TCP")
				Expect(err).NotTo(HaveOccurred(), "Both services should share the FIP again after switch back")

				By(fmt.Sprintf("Verifying rules are cleaned from LB %s after switch back", expectedLBAfterSwitch))
				err = utils.VerifyFIPHasNoRulesForPorts(tc, newFIPID, sets.New(switchSvcPort), "TCP")
				Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("Switched service's rules should be cleaned from LB %s after switch back", expectedLBAfterSwitch))

				By(fmt.Sprintf("Triggering reconcile and verifying %s service stays on LB %s after switch back", switchSvcRole, svc1LBName))
				beforeReconcile2 := time.Now().Add(-1 * time.Second)
				addDummyAnnotationWithServiceName(cs, ns.Name, switchSvcName)
				err = utils.WaitForServiceNormalEventAfter(cs, ns.Name, switchSvcName, "EnsuredLoadBalancer", "", beforeReconcile2)
				Expect(err).NotTo(HaveOccurred(), "Reconcile should succeed after switch back")
				finalFIPID, finalLBName := getFIPIDAndLBName(tc, sharedIP, startInternal)
				Expect(finalLBName).To(Equal(svc1LBName), switchSvcRole+" service should stay on original LB after reconcile")
				Expect(finalFIPID).To(Equal(svc1FIPID), switchSvcRole+" service should keep original FIP after reconcile")
			},
			Entry("secondary: external to internal same LB", false, false, false),
			Entry("secondary: external to internal different LB", false, true, false),
			Entry("secondary: internal to external same LB", true, false, false),
			Entry("secondary: internal to external different LB", true, true, false),
			Entry("primary: external to internal same LB", false, false, true),
			Entry("primary: external to internal different LB", false, true, true),
			Entry("primary: internal to external same LB", true, false, true),
			Entry("primary: internal to external different LB", true, true, true),
		)

		It("should block service sharing IP on LB not in eligible set", func() {
			By("Creating Service 1 with label matching lb-2's ServiceLabelSelector")
			svc1Port := int32(serverPort)
			svc1Labels := map[string]string{
				"app": testServiceName,
				"a":   "b",
			}
			svc1 := utils.CreateLoadBalancerServiceManifest(svcNamePrimary, nil, svc1Labels, ns.Name, []v1.ServicePort{{
				Port:       svc1Port,
				TargetPort: intstr.FromInt(int(svc1Port)),
			}})
			_, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc1, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNamePrimary)
				Expect(err).NotTo(HaveOccurred())
			}()

			ips1, err := utils.WaitServiceExposureAndGetIPs(cs, ns.Name, svcNamePrimary)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ips1)).NotTo(BeZero())
			svc1IP := *ips1[0]
			svc1LB := getAzureLoadBalancerFromPIP(tc, &svc1IP, tc.GetResourceGroup(), tc.GetResourceGroup())
			Expect(ptr.Deref(svc1LB.Name, "")).To(Equal("lb-2"), "Service 1 with label a=b should land on lb-2")
			svc1FIPID := getPIPFrontendConfigurationID(tc, svc1IP, tc.GetResourceGroup(), true)
			utils.Logf("Service 1 exposed with IP %s on lb-2, FIP: %s", svc1IP, svc1FIPID)

			By("Creating Service 2 without label a=b, pointing to Service 1's IP")
			svc2Port := int32(serverPort + 1)
			svc2 := utils.CreateLoadBalancerServiceManifest(svcNameIPv4, map[string]string{
				consts.ServiceAnnotationLoadBalancerIPDualStack[false]: svc1IP,
			}, labels, ns.Name, []v1.ServicePort{{
				Port:       svc2Port,
				TargetPort: intstr.FromInt(int(svc2Port)),
			}})
			_, err = cs.CoreV1().Services(ns.Name).Create(context.TODO(), svc2, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				err := utils.DeleteService(cs, ns.Name, svcNameIPv4)
				Expect(err).NotTo(HaveOccurred())
			}()

			By("Verifying Service 2 is blocked with not-in-eligible-set error")
			err = utils.WaitForServiceWarningEventAfter(cs, ns.Name, svcNameIPv4, "SyncLoadBalancerFailed", "not in the eligible set", time.Time{})
			Expect(err).NotTo(HaveOccurred(), "Expected SyncLoadBalancerFailed event for "+svcNameIPv4)

			By("Resolving by adding label a=b to Service 2")
			svc2, err = cs.CoreV1().Services(ns.Name).Get(context.TODO(), svcNameIPv4, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			svc2.Labels["a"] = "b"
			_, err = cs.CoreV1().Services(ns.Name).Update(context.TODO(), svc2, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Service 2 is now exposed with Service 1's IP")
			_, err = utils.WaitServiceExposure(cs, ns.Name, svcNameIPv4, []*string{&svc1IP})
			Expect(err).NotTo(HaveOccurred(), svcNameIPv4+" should be exposed after adding label")

			By("Verifying FIP has rules for both services")
			err = utils.VerifyFIPHasRulesForPorts(tc, svc1FIPID, sets.New(svc1Port, svc2Port), "TCP")
			Expect(err).NotTo(HaveOccurred(), "Both services should share the FIP on lb-2")
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

// getFIPIDAndLBName returns the frontend IP configuration ID and load balancer name for the given IP.
func getFIPIDAndLBName(tc *utils.AzureTestClient, ip string, isInternal bool) (fipID, lbName string) {
	if isInternal {
		lb := getAzureInternalLoadBalancerFromPrivateIP(tc, &ip, tc.GetResourceGroup())
		return utils.GetFIPIDForPrivateIP(lb, ip), ptr.Deref(lb.Name, "")
	}
	fipID = getPIPFrontendConfigurationID(tc, ip, tc.GetResourceGroup(), true)
	lb := getAzureLoadBalancerFromPIP(tc, &ip, tc.GetResourceGroup(), tc.GetResourceGroup())
	return fipID, ptr.Deref(lb.Name, "")
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
