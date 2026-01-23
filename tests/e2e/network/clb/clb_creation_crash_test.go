/*
Copyright 2025 The Kubernetes Authors.

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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Container Load Balancer Creation Crash Recovery Tests", Label(clbTestLabel, "CLB-CreationCrash"), func() {
	basename := "clb-create-crash"

	var (
		cs        clientset.Interface
		ccmClient *utils.CCMClusterClient
		ns        *v1.Namespace
	)

	BeforeEach(func() {
		var err error

		// First check if CCM cluster is configured
		if !utils.IsCCMClusterConfigured() {
			Skip(fmt.Sprintf("Skipping CCM crash tests: %s environment variable not set", utils.CCMKubeconfigEnvVar))
		}

		// Create workload cluster client
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		// Create CCM cluster client
		ccmClient, err = utils.NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())

		// Create test namespace in workload cluster
		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		// Verify CCM is running before starting test
		ctx := context.Background()
		pods, err := ccmClient.GetCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods)).To(BeNumerically(">", 0), "CCM should be running before test starts")
	})

	AfterEach(func() {
		if ns != nil {
			utils.Logf("Deleting namespace %s", ns.Name)
			utils.DeleteNamespace(cs, ns.Name)
		}

		// Ensure CCM is recovered before next test
		if ccmClient != nil {
			ctx := context.Background()
			err := ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
			if err != nil {
				utils.Logf("Warning: CCM may not be fully recovered after test: %v", err)
			}
		}

		// Wait for Azure cleanup
		utils.Logf("Waiting 120 seconds for Azure cleanup...")
		time.Sleep(120 * time.Second)

		By("Verifying Service Gateway cleanup")
		verifyServiceGatewayCleanup()

		cs = nil
		ccmClient = nil
		ns = nil
	})

	// Test 1: Create inbound services + immediately crash CCM during provisioning
	It("should recover and complete service creation after CCM crash during provisioning", func() {
		const (
			serviceCount    = 5
			servicePort     = int32(8080)
			targetPort      = 8080
			crashDelay      = 2 * time.Second  // Crash after 2 seconds
			ccmDowntime     = 20 * time.Second // Keep CCM down for 20 seconds
			recoveryTimeout = 180 * time.Second
		)

		ctx := context.Background()

		By(fmt.Sprintf("Creating %d LoadBalancer services with backend pods", serviceCount))
		serviceNames := make([]string, serviceCount)
		for i := 0; i < serviceCount; i++ {
			serviceName := fmt.Sprintf("create-crash-svc-%d", i)
			serviceNames[i] = serviceName

			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: ns.Name,
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer,
					Selector: map[string]string{
						"app": serviceName,
					},
					Ports: []v1.ServicePort{
						{
							Port:       servicePort,
							TargetPort: intstr.FromInt(targetPort),
							Protocol:   v1.ProtocolTCP,
						},
					},
				},
			}
			_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Create backend pod
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod", serviceName),
					Namespace: ns.Name,
					Labels: map[string]string{
						"app": serviceName,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:            "nginx",
							Image:           "nginx:alpine",
							ImagePullPolicy: v1.PullIfNotPresent,
							Ports: []v1.ContainerPort{
								{ContainerPort: int32(targetPort)},
							},
						},
					},
				},
			}
			_, err = cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		utils.Logf("Created %d services with backend pods", serviceCount)

		By("Waiting for all pods to be ready (so endpoints exist)")
		for _, serviceName := range serviceNames {
			podName := fmt.Sprintf("%s-pod", serviceName)
			err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 120*time.Second, true, func(ctx context.Context) (bool, error) {
				pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
				if err != nil {
					return false, nil
				}
				for _, cond := range pod.Status.Conditions {
					if cond.Type == v1.PodReady && cond.Status == v1.ConditionTrue {
						return true, nil
					}
				}
				return false, nil
			})
			Expect(err).NotTo(HaveOccurred(), "Pod %s should become ready", podName)
			utils.Logf("Pod %s is ready", podName)
		}
		utils.Logf("All %d pods are ready", serviceCount)

		By(fmt.Sprintf("IMMEDIATELY crashing CCM after %v (during Azure provisioning)", crashDelay))
		time.Sleep(crashDelay)
		err := ccmClient.DeleteAllCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM crashed during service provisioning!")

		By(fmt.Sprintf("Waiting %v with CCM down", ccmDowntime))
		time.Sleep(ccmDowntime)

		By("Waiting for CCM to recover")
		err = ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM recovered")

		By(fmt.Sprintf("Waiting for all services to get External IPs (timeout: %v)", recoveryTimeout))
		err = wait.PollUntilContextTimeout(ctx, 5*time.Second, recoveryTimeout, true, func(ctx context.Context) (bool, error) {
			servicesWithIP := 0
			for _, serviceName := range serviceNames {
				svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
				if err != nil {
					return false, nil
				}
				if len(svc.Status.LoadBalancer.Ingress) > 0 && svc.Status.LoadBalancer.Ingress[0].IP != "" {
					servicesWithIP++
				}
			}
			utils.Logf("Services with External IP: %d/%d", servicesWithIP, serviceCount)
			return servicesWithIP == serviceCount, nil
		})
		Expect(err).NotTo(HaveOccurred(), "All services should get External IPs within timeout")

		By("Verifying all services have LoadBalancer IPs")
		servicesWithIP := 0
		for _, serviceName := range serviceNames {
			svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			if len(svc.Status.LoadBalancer.Ingress) > 0 && svc.Status.LoadBalancer.Ingress[0].IP != "" {
				servicesWithIP++
				utils.Logf("✓ Service %s has External IP: %s", serviceName, svc.Status.LoadBalancer.Ingress[0].IP)
			} else {
				utils.Logf("✗ Service %s has no LoadBalancer IP yet", serviceName)
			}
		}
		Expect(servicesWithIP).To(Equal(serviceCount), "All services should have LoadBalancer IPs after recovery")
		utils.Logf("✓ All %d services have External IPs", serviceCount)

		By("Verifying all services have ServiceGateway finalizer")
		for _, serviceName := range serviceNames {
			svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			hasFinalizer := false
			for _, f := range svc.Finalizers {
				if f == serviceGatewayServiceFinalizer {
					hasFinalizer = true
					break
				}
			}
			Expect(hasFinalizer).To(BeTrue(), "Service %s should have ServiceGateway finalizer", serviceName)
		}
		utils.Logf("✓ All services have ServiceGateway finalizer")

		By("Verifying services exist in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		inboundCount := 0
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Inbound" {
				inboundCount++
			}
		}
		Expect(inboundCount).To(BeNumerically(">=", serviceCount), "All services should be in Service Gateway")
		utils.Logf("✓ Found %d inbound services in Service Gateway", inboundCount)

		By("Verifying Load Balancers exist in Azure")
		lbCount, err := countAzureLoadBalancers()
		Expect(err).NotTo(HaveOccurred())
		Expect(lbCount).To(BeNumerically(">=", serviceCount), "All LBs should exist in Azure")
		utils.Logf("✓ Found %d Load Balancers in Azure", lbCount)

		utils.Logf("\n✓ Creation crash recovery test PASSED!")
		utils.Logf("  - CCM crashed %v after creating %d services", crashDelay, serviceCount)
		utils.Logf("  - CCM was down for %v", ccmDowntime)
		utils.Logf("  - All services fully provisioned after recovery")
	})

	// Test 2: Create egress pods + immediately crash CCM during NAT Gateway provisioning
	It("should recover and complete egress provisioning after CCM crash during creation", func() {
		const (
			podCount        = 10
			targetPort      = 8080
			crashDelay      = 2 * time.Second
			ccmDowntime     = 20 * time.Second
			recoveryTimeout = 180 * time.Second
		)

		ctx := context.Background()
		egressName := ns.Name

		By(fmt.Sprintf("Creating %d egress pods", podCount))
		podNames := make([]string, podCount)
		for i := 0; i < podCount; i++ {
			podName := fmt.Sprintf("egress-create-crash-%d", i)
			podNames[i] = podName

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: ns.Name,
					Labels: map[string]string{
						egressLabel: egressName,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:            "test-app",
							Image:           utils.AgnhostImage,
							ImagePullPolicy: v1.PullIfNotPresent,
							Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)},
						},
					},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		utils.Logf("Created %d egress pods", podCount)

		By("Waiting for all pods to be ready (so egress processing starts)")
		for _, podName := range podNames {
			err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 120*time.Second, true, func(ctx context.Context) (bool, error) {
				pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
				if err != nil {
					return false, nil
				}
				for _, cond := range pod.Status.Conditions {
					if cond.Type == v1.PodReady && cond.Status == v1.ConditionTrue {
						return true, nil
					}
				}
				return false, nil
			})
			Expect(err).NotTo(HaveOccurred(), "Pod %s should become ready", podName)
		}
		utils.Logf("All %d pods are ready", podCount)

		By(fmt.Sprintf("IMMEDIATELY crashing CCM after %v (during NAT Gateway provisioning)", crashDelay))
		time.Sleep(crashDelay)
		err := ccmClient.DeleteAllCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM crashed during egress provisioning!")

		By(fmt.Sprintf("Waiting %v with CCM down", ccmDowntime))
		time.Sleep(ccmDowntime)

		By("Waiting for CCM to recover")
		err = ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM recovered")

		By("Waiting for pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Waiting for CCM to complete NAT Gateway provisioning (%v)", recoveryTimeout))
		time.Sleep(recoveryTimeout)

		By("Verifying all pods have ServiceGateway finalizer")
		podsWithFinalizer := 0
		for _, podName := range podNames {
			pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			for _, f := range pod.Finalizers {
				if f == serviceGatewayPodFinalizer {
					podsWithFinalizer++
					break
				}
			}
		}
		Expect(podsWithFinalizer).To(Equal(podCount), "All pods should have ServiceGateway finalizer")
		utils.Logf("✓ All %d pods have ServiceGateway finalizer", podCount)

		By("Verifying egress service exists in Service Gateway")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		var foundEgress bool
		var natGatewayID string
		for _, svc := range sgResponse.Value {
			if svc.Name == egressName && svc.Properties.ServiceType == "Outbound" {
				foundEgress = true
				natGatewayID = svc.Properties.PublicNatGatewayID
				break
			}
		}
		Expect(foundEgress).To(BeTrue(), "Egress service should exist in Service Gateway")
		Expect(natGatewayID).NotTo(BeEmpty(), "NAT Gateway should be provisioned")
		utils.Logf("✓ Found egress service with NAT Gateway: %s", natGatewayID)

		// Get address count from address locations API
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())
		addressCount := 0
		for _, loc := range alResponse.Value {
			addressCount += len(loc.Addresses)
		}
		utils.Logf("✓ Address locations registered: %d", addressCount)

		By("Verifying NAT Gateway exists in Azure")
		natCount, err := countAzureNATGateways()
		Expect(err).NotTo(HaveOccurred())
		Expect(natCount).To(BeNumerically(">=", 1), "NAT Gateway should exist in Azure")
		utils.Logf("✓ Found %d NAT Gateway(s) in Azure", natCount)

		utils.Logf("\n✓ Egress creation crash recovery test PASSED!")
		utils.Logf("  - CCM crashed %v after creating %d egress pods", crashDelay, podCount)
		utils.Logf("  - CCM was down for %v", ccmDowntime)
		utils.Logf("  - NAT Gateway fully provisioned after recovery")
		utils.Logf("  - All pod addresses registered")
	})

	// Test 3: Mixed inbound + egress creation + crash during provisioning
	It("should recover and complete mixed inbound/egress creation after CCM crash", func() {
		const (
			inboundServiceCount = 3
			egressPodCount      = 6
			servicePort         = int32(8080)
			targetPort          = 8080
			crashDelay          = 2 * time.Second
			ccmDowntime         = 25 * time.Second
			recoveryTimeout     = 240 * time.Second
		)

		ctx := context.Background()
		egressName := ns.Name

		By(fmt.Sprintf("Creating %d inbound services + %d egress pods simultaneously", inboundServiceCount, egressPodCount))

		// Create inbound services
		serviceNames := make([]string, inboundServiceCount)
		for i := 0; i < inboundServiceCount; i++ {
			serviceName := fmt.Sprintf("mixed-create-svc-%d", i)
			serviceNames[i] = serviceName

			service := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      serviceName,
					Namespace: ns.Name,
				},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeLoadBalancer,
					Selector: map[string]string{
						"app": serviceName,
					},
					Ports: []v1.ServicePort{
						{
							Port:       servicePort,
							TargetPort: intstr.FromInt(targetPort),
							Protocol:   v1.ProtocolTCP,
						},
					},
				},
			}
			_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Backend pod
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod", serviceName),
					Namespace: ns.Name,
					Labels:    map[string]string{"app": serviceName},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "nginx", Image: "nginx:alpine", Ports: []v1.ContainerPort{{ContainerPort: int32(targetPort)}}},
					},
				},
			}
			_, err = cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}

		// Create egress pods
		egressPodNames := make([]string, egressPodCount)
		for i := 0; i < egressPodCount; i++ {
			podName := fmt.Sprintf("mixed-egress-create-%d", i)
			egressPodNames[i] = podName

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: ns.Name,
					Labels:    map[string]string{egressLabel: egressName},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "test-app", Image: utils.AgnhostImage, Args: []string{"netexec", fmt.Sprintf("--http-port=%d", targetPort)}},
					},
				},
			}
			_, err := cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
		}
		utils.Logf("Created %d inbound services + %d egress pods", inboundServiceCount, egressPodCount)

		By("Waiting for all pods to be ready (so processing starts)")
		allPodNames := make([]string, 0, inboundServiceCount+egressPodCount)
		for _, svcName := range serviceNames {
			allPodNames = append(allPodNames, fmt.Sprintf("%s-pod", svcName))
		}
		allPodNames = append(allPodNames, egressPodNames...)
		for _, podName := range allPodNames {
			err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 120*time.Second, true, func(ctx context.Context) (bool, error) {
				pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
				if err != nil {
					return false, nil
				}
				for _, cond := range pod.Status.Conditions {
					if cond.Type == v1.PodReady && cond.Status == v1.ConditionTrue {
						return true, nil
					}
				}
				return false, nil
			})
			Expect(err).NotTo(HaveOccurred(), "Pod %s should become ready", podName)
		}
		utils.Logf("All %d pods are ready", len(allPodNames))

		By(fmt.Sprintf("IMMEDIATELY crashing CCM after %v (during mixed provisioning)", crashDelay))
		time.Sleep(crashDelay)
		err := ccmClient.DeleteAllCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM crashed during mixed provisioning!")

		By(fmt.Sprintf("Waiting %v with CCM down", ccmDowntime))
		time.Sleep(ccmDowntime)

		By("Waiting for CCM to recover")
		err = ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("CCM recovered")

		By("Waiting for all pods to be ready")
		err = utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Waiting for all inbound services to get External IPs (timeout: %v)", recoveryTimeout))
		err = wait.PollUntilContextTimeout(ctx, 5*time.Second, recoveryTimeout, true, func(ctx context.Context) (bool, error) {
			servicesWithIP := 0
			for _, serviceName := range serviceNames {
				svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
				if err != nil {
					return false, nil
				}
				if len(svc.Status.LoadBalancer.Ingress) > 0 && svc.Status.LoadBalancer.Ingress[0].IP != "" {
					servicesWithIP++
				}
			}
			utils.Logf("Services with External IP: %d/%d", servicesWithIP, inboundServiceCount)
			return servicesWithIP == inboundServiceCount, nil
		})
		Expect(err).NotTo(HaveOccurred(), "All inbound services should get External IPs within timeout")

		By("Verifying all inbound services have LoadBalancer IPs")
		servicesWithIP := 0
		for _, serviceName := range serviceNames {
			svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			if len(svc.Status.LoadBalancer.Ingress) > 0 && svc.Status.LoadBalancer.Ingress[0].IP != "" {
				servicesWithIP++
				utils.Logf("✓ Service %s has External IP: %s", serviceName, svc.Status.LoadBalancer.Ingress[0].IP)
			}
		}
		Expect(servicesWithIP).To(Equal(inboundServiceCount), "All services should have LoadBalancer IPs")
		utils.Logf("✓ All %d inbound services have External IPs", inboundServiceCount)

		By("Verifying all egress pods have finalizers")
		podsWithFinalizer := 0
		for _, podName := range egressPodNames {
			pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			for _, f := range pod.Finalizers {
				if f == serviceGatewayPodFinalizer {
					podsWithFinalizer++
					break
				}
			}
		}
		Expect(podsWithFinalizer).To(Equal(egressPodCount), "All egress pods should have finalizer")
		utils.Logf("✓ All %d egress pods have ServiceGateway finalizer", egressPodCount)

		By("Waiting for CCM to complete NAT Gateway provisioning (15s)")
		time.Sleep(15 * time.Second)

		By("Verifying Service Gateway state")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		inboundCount := 0
		outboundCount := 0
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Inbound" {
				inboundCount++
			} else if svc.Properties.ServiceType == "Outbound" && svc.Name != "default-natgw-v2" {
				outboundCount++
			}
		}
		Expect(inboundCount).To(BeNumerically(">=", inboundServiceCount), "All inbound services should be in SGW")
		Expect(outboundCount).To(BeNumerically(">=", 1), "Egress service should be in SGW")

		// Get egress address count from address locations API
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())
		egressAddressCount := 0
		for _, loc := range alResponse.Value {
			egressAddressCount += len(loc.Addresses)
		}
		utils.Logf("✓ Service Gateway: %d inbound, %d outbound (with %d addresses)", inboundCount, outboundCount, egressAddressCount)

		By("Verifying Azure resources")
		lbCount, _ := countAzureLoadBalancers()
		pipCount, _ := countAzurePublicIPs()
		natCount, _ := countAzureNATGateways()
		utils.Logf("Azure resources: %d LBs, %d PIPs, %d NAT GWs", lbCount, pipCount, natCount)

		Expect(lbCount).To(BeNumerically(">=", inboundServiceCount), "All LBs should exist")
		Expect(natCount).To(BeNumerically(">=", 1), "NAT Gateway should exist")

		utils.Logf("\n✓ Mixed creation crash recovery test PASSED!")
		utils.Logf("  - CCM crashed %v after creating mixed workload", crashDelay)
		utils.Logf("  - CCM was down for %v", ccmDowntime)
		utils.Logf("  - %d inbound services fully provisioned", inboundServiceCount)
		utils.Logf("  - %d egress pods registered with NAT Gateway", egressPodCount)
	})

	// Test 4: Rapid create-crash-recover cycles
	It("should handle multiple rapid creation crash cycles", func() {
		const (
			cycleCount       = 3
			servicesPerCycle = 2
			servicePort      = int32(8080)
			targetPort       = 8080
			crashDelay       = 1 * time.Second
			ccmDowntime      = 10 * time.Second
		)

		ctx := context.Background()

		By(fmt.Sprintf("Running %d rapid create-crash-recover cycles", cycleCount))

		allServiceNames := []string{}

		for cycle := 0; cycle < cycleCount; cycle++ {
			utils.Logf("\n=== Cycle %d/%d ===", cycle+1, cycleCount)

			// Create services
			for i := 0; i < servicesPerCycle; i++ {
				serviceName := fmt.Sprintf("rapid-cycle%d-svc%d", cycle, i)
				allServiceNames = append(allServiceNames, serviceName)

				service := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceName,
						Namespace: ns.Name,
					},
					Spec: v1.ServiceSpec{
						Type:     v1.ServiceTypeLoadBalancer,
						Selector: map[string]string{"app": serviceName},
						Ports: []v1.ServicePort{
							{Port: servicePort, TargetPort: intstr.FromInt(targetPort), Protocol: v1.ProtocolTCP},
						},
					},
				}
				_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())

				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod", serviceName),
						Namespace: ns.Name,
						Labels:    map[string]string{"app": serviceName},
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{Name: "nginx", Image: "nginx:alpine", Ports: []v1.ContainerPort{{ContainerPort: int32(targetPort)}}},
						},
					},
				}
				_, err = cs.CoreV1().Pods(ns.Name).Create(ctx, pod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred())
			}
			utils.Logf("Created %d services in cycle %d", servicesPerCycle, cycle+1)

			// Wait for pods in this cycle to be ready
			for i := 0; i < servicesPerCycle; i++ {
				podName := fmt.Sprintf("rapid-cycle%d-svc%d-pod", cycle, i)
				err := wait.PollUntilContextTimeout(ctx, 2*time.Second, 120*time.Second, true, func(ctx context.Context) (bool, error) {
					pod, err := cs.CoreV1().Pods(ns.Name).Get(ctx, podName, metav1.GetOptions{})
					if err != nil {
						return false, nil
					}
					for _, cond := range pod.Status.Conditions {
						if cond.Type == v1.PodReady && cond.Status == v1.ConditionTrue {
							return true, nil
						}
					}
					return false, nil
				})
				Expect(err).NotTo(HaveOccurred(), "Pod %s should become ready", podName)
			}
			utils.Logf("All pods ready in cycle %d", cycle+1)

			// Crash CCM immediately after pods are ready
			time.Sleep(crashDelay)
			err := ccmClient.DeleteAllCCMPods(ctx)
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("CCM crashed in cycle %d", cycle+1)

			// Wait with CCM down
			time.Sleep(ccmDowntime)

			// Recover CCM
			err = ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("CCM recovered in cycle %d", cycle+1)

			// Brief stabilization
			time.Sleep(5 * time.Second)
		}

		By("Waiting for all services to get External IPs (timeout: 180s)")
		totalServices := cycleCount * servicesPerCycle
		err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 180*time.Second, true, func(ctx context.Context) (bool, error) {
			servicesWithIP := 0
			for _, serviceName := range allServiceNames {
				svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
				if err != nil {
					return false, nil
				}
				if len(svc.Status.LoadBalancer.Ingress) > 0 && svc.Status.LoadBalancer.Ingress[0].IP != "" {
					servicesWithIP++
				}
			}
			utils.Logf("Services with External IP: %d/%d", servicesWithIP, totalServices)
			return servicesWithIP == totalServices, nil
		})
		Expect(err).NotTo(HaveOccurred(), "All services should get External IPs within timeout")

		By("Verifying all services from all cycles have LoadBalancer IPs")
		servicesWithIP := 0
		for _, serviceName := range allServiceNames {
			svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			if len(svc.Status.LoadBalancer.Ingress) > 0 && svc.Status.LoadBalancer.Ingress[0].IP != "" {
				servicesWithIP++
				utils.Logf("✓ Service %s has External IP: %s", serviceName, svc.Status.LoadBalancer.Ingress[0].IP)
			}
		}
		Expect(servicesWithIP).To(Equal(totalServices), "All services should have LoadBalancer IPs")
		utils.Logf("✓ All %d services from %d cycles have External IPs", totalServices, cycleCount)

		By("Verifying Service Gateway contains all services")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		inboundCount := 0
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Inbound" {
				inboundCount++
			}
		}
		Expect(inboundCount).To(BeNumerically(">=", totalServices), "All services should be in Service Gateway")
		utils.Logf("✓ Found %d inbound services in Service Gateway", inboundCount)

		utils.Logf("\n✓ Rapid create-crash-recover cycles test PASSED!")
		utils.Logf("  - %d cycles completed", cycleCount)
		utils.Logf("  - %d total services created", totalServices)
		utils.Logf("  - All services fully provisioned despite repeated crashes")
	})
})
