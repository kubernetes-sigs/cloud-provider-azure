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
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// CCM Crash Recovery test label
const slbCrashTestLabel = "SLB-Crash"

var _ = Describe("Container Load Balancer Crash Recovery", Label(slbTestLabel, slbCrashTestLabel), func() {
	basename := "slb-crash-test"
	serviceName := "crash-service"

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
		if cs != nil && ns != nil {
			err := utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			utils.Logf("Waiting 120 seconds for Azure cleanup...")
			time.Sleep(120 * time.Second)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		// Ensure CCM is recovered before next test
		if ccmClient != nil {
			ctx := context.Background()
			err := ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
			if err != nil {
				utils.Logf("Warning: CCM may not be fully recovered after test: %v", err)
			}
		}

		cs = nil
		ccmClient = nil
		ns = nil
	})

	It("should recover service after CCM crash during stable state", func() {
		const (
			numPods     = 5
			servicePort = int32(8080)
			targetPort  = 8080
			waitTime    = 60 * time.Second
		)

		ctx := context.Background()
		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating pods")
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
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

		By("Waiting for pods to be ready")
		time.Sleep(30 * time.Second)

		By("Creating LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
				Annotations: map[string]string{
					"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "slb",
				},
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: serviceLabels,
				Ports: []v1.ServicePort{
					{
						Name:       "http",
						Protocol:   v1.ProtocolTCP,
						Port:       servicePort,
						TargetPort: intstr.FromInt(targetPort),
					},
				},
			},
		}
		_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for service to be established")
		time.Sleep(waitTime)

		By("Verifying Azure resources before crash (PIP, LB, Service Gateway)")
		svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(svc.UID)
		utils.Logf("Service UID: %s", serviceUID)

		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred(), "Azure resources should exist before CCM crash")

		By("Crashing CCM and waiting for recovery")
		err = ccmClient.CrashCCMAndWaitForRecovery(ctx, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to reconcile service")
		time.Sleep(30 * time.Second)

		By("Verifying Azure resources after CCM recovery (PIP, LB, Service Gateway)")
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred(), "Azure resources should still exist after CCM recovery")

		By("Verifying endpoints are correct after CCM recovery")
		endpoints, err := cs.CoreV1().Endpoints(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		totalAddresses := 0
		for _, subset := range endpoints.Subsets {
			totalAddresses += len(subset.Addresses)
		}
		utils.Logf("Endpoint count after CCM recovery: %d (expected: %d)", totalAddresses, numPods)
		Expect(totalAddresses).To(Equal(numPods), "All pods should be in endpoints after CCM recovery")
	})

	It("should handle pod creation during CCM downtime", func() {
		const (
			initialPods    = 3
			additionalPods = 2
			servicePort    = int32(8080)
			targetPort     = 8080
			waitTime       = 60 * time.Second
		)

		ctx := context.Background()
		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating initial pods")
		for i := 0; i < initialPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
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

		By("Waiting for initial pods to be ready")
		time.Sleep(30 * time.Second)

		By("Creating LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
				Annotations: map[string]string{
					"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "slb",
				},
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: serviceLabels,
				Ports: []v1.ServicePort{
					{
						Name:       "http",
						Protocol:   v1.ProtocolTCP,
						Port:       servicePort,
						TargetPort: intstr.FromInt(targetPort),
					},
				},
			},
		}
		_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for service to be established")
		time.Sleep(waitTime)

		By("Verifying Azure resources before crash")
		svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(svc.UID)
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred(), "Azure resources should exist before CCM crash")

		By("Crashing CCM")
		err = ccmClient.DeleteAllCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())

		By("Creating additional pods while CCM is down")
		for i := initialPods; i < initialPods+additionalPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
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
		utils.Logf("Created %d additional pods while CCM was down", additionalPods)

		By("Waiting for CCM to recover")
		err = ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to reconcile endpoints")
		time.Sleep(60 * time.Second)

		By("Verifying Azure resources after CCM recovery")
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred(), "Azure resources should still exist after CCM recovery")

		By("Verifying all pods are reflected in service endpoints")
		endpoints, err := cs.CoreV1().Endpoints(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		totalAddresses := 0
		for _, subset := range endpoints.Subsets {
			totalAddresses += len(subset.Addresses)
		}
		expectedPods := initialPods + additionalPods
		utils.Logf("Expected %d endpoints, found %d", expectedPods, totalAddresses)
		Expect(totalAddresses).To(Equal(expectedPods), "All pods should be in endpoints after CCM recovery")
	})

	It("should maintain consistency after multiple CCM crashes", func() {
		const (
			numPods     = 5
			servicePort = int32(8080)
			targetPort  = 8080
			numCrashes  = 3
			waitTime    = 60 * time.Second
		)

		ctx := context.Background()
		serviceLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating pods")
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-pod-%d", serviceName, i),
					Namespace: ns.Name,
					Labels:    serviceLabels,
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

		By("Waiting for pods to be ready")
		time.Sleep(30 * time.Second)

		By("Creating LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
				Annotations: map[string]string{
					"service.beta.kubernetes.io/azure-load-balancer-backend-pool-type": "slb",
				},
			},
			Spec: v1.ServiceSpec{
				Type:     v1.ServiceTypeLoadBalancer,
				Selector: serviceLabels,
				Ports: []v1.ServicePort{
					{
						Name:       "http",
						Protocol:   v1.ProtocolTCP,
						Port:       servicePort,
						TargetPort: intstr.FromInt(targetPort),
					},
				},
			},
		}
		_, err := cs.CoreV1().Services(ns.Name).Create(ctx, service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for service to be established")
		time.Sleep(waitTime)

		By("Verifying Azure resources before crashes")
		svc, err := cs.CoreV1().Services(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		serviceUID := string(svc.UID)
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred(), "Azure resources should exist before CCM crashes")

		By(fmt.Sprintf("Performing %d CCM crashes and recoveries", numCrashes))
		for i := 1; i <= numCrashes; i++ {
			utils.Logf("=== CCM Crash iteration %d/%d ===", i, numCrashes)

			err = ccmClient.CrashCCMAndWaitForRecovery(ctx, utils.CCMRecoveryTimeout)
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("Waiting for CCM to reconcile after crash %d", i))
			time.Sleep(30 * time.Second)

			By(fmt.Sprintf("Verifying Azure resources after crash %d", i))
			err = verifyAzureResources(serviceUID)
			Expect(err).NotTo(HaveOccurred(), "Azure resources should persist after crash %d", i)
		}

		By("Verifying Azure resources after all crashes")
		err = verifyAzureResources(serviceUID)
		Expect(err).NotTo(HaveOccurred(), "Azure resources should still exist after multiple CCM crashes")

		By("Verifying endpoints are correct")
		endpoints, err := cs.CoreV1().Endpoints(ns.Name).Get(ctx, serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		totalAddresses := 0
		for _, subset := range endpoints.Subsets {
			totalAddresses += len(subset.Addresses)
		}
		utils.Logf("Final endpoint count: %d (expected: %d)", totalAddresses, numPods)
		Expect(totalAddresses).To(Equal(numPods), "All pods should be in endpoints after multiple crashes")
	})
})

var _ = Describe("Container Load Balancer Outbound Crash Recovery", Label(slbTestLabel, slbCrashTestLabel), func() {
	basename := "slb-outbound-crash"

	var (
		cs        clientset.Interface
		ccmClient *utils.CCMClusterClient
		ns        *v1.Namespace
	)

	BeforeEach(func() {
		var err error

		if !utils.IsCCMClusterConfigured() {
			Skip(fmt.Sprintf("Skipping CCM crash tests: %s environment variable not set", utils.CCMKubeconfigEnvVar))
		}

		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ccmClient, err = utils.NewCCMClusterClient()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())

		ctx := context.Background()
		pods, err := ccmClient.GetCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(len(pods)).To(BeNumerically(">", 0), "CCM should be running before test starts")
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			err := utils.DeleteNamespace(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())

			utils.Logf("Waiting 120 seconds for Azure cleanup...")
			time.Sleep(120 * time.Second)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		if ccmClient != nil {
			ctx := context.Background()
			err := ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
			if err != nil {
				utils.Logf("Warning: CCM may not be fully recovered after test: %v", err)
			}
		}

		cs = nil
		ccmClient = nil
		ns = nil
	})

	It("should recover NAT gateway after CCM crash", func() {
		const (
			numPods    = 5
			egressName = "test-egress-crash"
			targetPort = 8080
			waitTime   = 90 * time.Second
		)

		ctx := context.Background()

		By(fmt.Sprintf("Creating %d pods with egress label '%s=%s'", numPods, egressLabel, egressName))
		for i := 0; i < numPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("egress-pod-%d", i),
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

		By("Waiting for NAT Gateway provisioning")
		time.Sleep(waitTime)

		By("Verifying NAT Gateway and Service Gateway before crash")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		var natGatewayID string
		var foundOutbound bool
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
				foundOutbound = true
				natGatewayID = svc.Properties.PublicNatGatewayID
				utils.Logf("Found outbound service '%s' with NAT Gateway: %s", egressName, natGatewayID)
				break
			}
		}
		Expect(foundOutbound).To(BeTrue(), "Outbound service should exist before crash")
		Expect(natGatewayID).NotTo(BeEmpty(), "NAT Gateway ID should not be empty")

		By("Crashing CCM and waiting for recovery")
		err = ccmClient.CrashCCMAndWaitForRecovery(ctx, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to reconcile NAT Gateway")
		time.Sleep(30 * time.Second)

		By("Verifying NAT Gateway still exists after CCM recovery")
		sgResponse, err = queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		foundOutbound = false
		var recoveredNatGatewayID string
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
				foundOutbound = true
				recoveredNatGatewayID = svc.Properties.PublicNatGatewayID
				utils.Logf("Outbound service still exists after recovery with NAT Gateway: %s", recoveredNatGatewayID)
				break
			}
		}
		Expect(foundOutbound).To(BeTrue(), "Outbound service should still exist after CCM recovery")
		Expect(recoveredNatGatewayID).To(Equal(natGatewayID), "NAT Gateway ID should remain the same")

		By("Verifying pod registrations in Address Locations")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		registeredPods := 0
		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					if svcName == egressName {
						registeredPods++
					}
				}
			}
		}
		utils.Logf("Registered %d pod IPs for egress gateway after recovery", registeredPods)
		Expect(registeredPods).To(Equal(numPods), "All pods should remain registered after CCM recovery")
	})

	It("should handle pod creation during CCM downtime for NAT gateway", func() {
		const (
			initialPods    = 3
			additionalPods = 2
			egressName     = "test-egress-downtime"
			targetPort     = 8080
			waitTime       = 90 * time.Second
		)

		ctx := context.Background()

		By(fmt.Sprintf("Creating %d initial egress pods", initialPods))
		for i := 0; i < initialPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("egress-pod-%d", i),
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

		By("Waiting for NAT Gateway provisioning")
		time.Sleep(waitTime)

		By("Verifying NAT Gateway exists")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())
		var foundOutbound bool
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
				foundOutbound = true
				break
			}
		}
		Expect(foundOutbound).To(BeTrue(), "Outbound service should exist")

		By("Crashing CCM")
		err = ccmClient.DeleteAllCCMPods(ctx)
		Expect(err).NotTo(HaveOccurred())

		By(fmt.Sprintf("Creating %d additional pods while CCM is down", additionalPods))
		for i := initialPods; i < initialPods+additionalPods; i++ {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("egress-pod-%d", i),
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
		utils.Logf("Created %d additional pods while CCM was down", additionalPods)

		By("Waiting for CCM to recover")
		err = ccmClient.WaitForCCMReady(ctx, utils.CCMRecoveryTimeout)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for CCM to reconcile")
		time.Sleep(60 * time.Second)

		By("Verifying all pods registered in Address Locations")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		registeredPods := 0
		for _, location := range alResponse.Value {
			for _, addr := range location.Addresses {
				for _, svcName := range addr.Services {
					if svcName == egressName {
						registeredPods++
					}
				}
			}
		}
		expectedPods := initialPods + additionalPods
		utils.Logf("Expected %d pods, found %d registered", expectedPods, registeredPods)
		Expect(registeredPods).To(Equal(expectedPods), "All pods should be registered after CCM recovery")
	})

	It("should maintain multiple NAT gateways across CCM crashes", func() {
		const (
			podsPerGateway = 3
			numCrashes     = 2
			targetPort     = 8080
			waitTime       = 90 * time.Second
		)

		egressGateways := []string{"egress-alpha-crash", "egress-beta-crash"}
		ctx := context.Background()

		By(fmt.Sprintf("Creating %d egress gateways with %d pods each", len(egressGateways), podsPerGateway))
		for _, egressName := range egressGateways {
			for i := 0; i < podsPerGateway; i++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("%s-pod-%d", egressName, i),
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
		}

		By("Waiting for NAT Gateways provisioning")
		time.Sleep(waitTime)

		By("Verifying both NAT Gateways exist")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		natGatewayIDs := make(map[string]string)
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" {
				for _, egressName := range egressGateways {
					if svc.Name == egressName {
						natGatewayIDs[egressName] = svc.Properties.PublicNatGatewayID
						utils.Logf("Found egress gateway '%s' with NAT Gateway: %s", egressName, svc.Properties.PublicNatGatewayID)
					}
				}
			}
		}
		Expect(len(natGatewayIDs)).To(Equal(len(egressGateways)), "All egress gateways should exist before crashes")

		By(fmt.Sprintf("Performing %d CCM crashes", numCrashes))
		for i := 1; i <= numCrashes; i++ {
			utils.Logf("=== CCM Crash iteration %d/%d ===", i, numCrashes)

			err = ccmClient.CrashCCMAndWaitForRecovery(ctx, utils.CCMRecoveryTimeout)
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("Waiting for reconciliation after crash %d", i))
			time.Sleep(30 * time.Second)

			By(fmt.Sprintf("Verifying both NAT Gateways after crash %d", i))
			sgResponse, err = queryServiceGatewayServices()
			Expect(err).NotTo(HaveOccurred())

			foundGateways := 0
			for _, svc := range sgResponse.Value {
				if svc.Properties.ServiceType == "Outbound" {
					for egressName, expectedNatID := range natGatewayIDs {
						if svc.Name == egressName {
							foundGateways++
							Expect(svc.Properties.PublicNatGatewayID).To(Equal(expectedNatID),
								"NAT Gateway ID should remain the same for %s after crash %d", egressName, i)
						}
					}
				}
			}
			Expect(foundGateways).To(Equal(len(egressGateways)), "All egress gateways should persist after crash %d", i)
		}

		By("Verifying final pod registrations")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		for _, egressName := range egressGateways {
			registeredPods := 0
			for _, location := range alResponse.Value {
				for _, addr := range location.Addresses {
					for _, svcName := range addr.Services {
						if svcName == egressName {
							registeredPods++
						}
					}
				}
			}
			utils.Logf("Egress gateway '%s' has %d registered pods", egressName, registeredPods)
			Expect(registeredPods).To(Equal(podsPerGateway), "All pods should be registered for %s", egressName)
		}
	})
})
