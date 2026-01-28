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
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Container Load Balancer Outbound Performance Test", Label(slbTestLabel, "outbound-performance"), func() {
	basename := "slb-outbound-perf"

	var (
		cs clientset.Interface
		ns *v1.Namespace
	)

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		cs = nil
		ns = nil
	})

	// Test 1: Scale test for multiple egress services (NAT Gateways)
	// Creates N egress services (1 pod each with unique egress label)
	// Each egress service creates a separate NAT Gateway in Azure
	It("should create and delete 50 egress services (NAT Gateways) at scale", NodeTimeout(2*time.Hour), Label("egress-scale"), func(ctx SpecContext) {
		const (
			numEgressServices = 50
			targetPort        = 8080
		)

		utils.Logf("\n" + strings.Repeat("=", 80))
		utils.Logf("OUTBOUND PERFORMANCE TEST: %d EGRESS SERVICES (NAT GATEWAYS)", numEgressServices)
		utils.Logf(strings.Repeat("=", 80))
		utils.Logf("Test started at: %s", time.Now().Format(time.RFC3339))

		// Track egress services
		egressNames := make([]string, numEgressServices)
		for i := 0; i < numEgressServices; i++ {
			egressNames[i] = fmt.Sprintf("egress-%d", i)
		}

		// ============================================
		// PHASE 1: CREATE ALL PODS WITH EGRESS LABELS IN PARALLEL
		// ============================================
		utils.Logf("\n--- PHASE 1: Creating %d pods with unique egress labels ---", numEgressServices)
		createStart := time.Now()

		var wg sync.WaitGroup
		var createErrors int32
		var createMutex sync.Mutex

		for i := 0; i < numEgressServices; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()

				egressName := egressNames[index]
				podName := fmt.Sprintf("egress-pod-%d", index)

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

				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				if err != nil {
					createMutex.Lock()
					createErrors++
					createMutex.Unlock()
					utils.Logf("ERROR creating pod-%d: %v", index, err)
				}
			}(i)
		}
		wg.Wait()

		k8sCreateDuration := time.Since(createStart)
		utils.Logf("✓ K8s API calls completed in %v (%d pods created, %d errors)",
			k8sCreateDuration, numEgressServices-int(createErrors), createErrors)

		// Wait for pods to be ready
		utils.Logf("  Waiting for pods to be ready...")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("✓ All pods are ready")

		// ============================================
		// PHASE 2: WAIT FOR ALL NAT GATEWAYS TO BE PROVISIONED
		// ============================================
		utils.Logf("\n--- PHASE 2: Waiting for %d NAT Gateways in Azure ---", numEgressServices)
		provisionStart := time.Now()

		provisionedEgress := make(map[string]bool)
		var provMutex sync.Mutex

		for {
			if time.Since(provisionStart) > 30*time.Minute {
				utils.Logf("WARNING: Timeout waiting for Azure NAT Gateway provisioning")
				break
			}

			sgResponse, err := queryServiceGatewayServices()
			if err != nil {
				time.Sleep(5 * time.Second)
				continue
			}

			provMutex.Lock()
			for _, sgSvc := range sgResponse.Value {
				if sgSvc.Properties.ServiceType == "Outbound" {
					// Check if this is one of our egress services
					for _, name := range egressNames {
						if sgSvc.Name == name {
							provisionedEgress[name] = true
							break
						}
					}
				}
			}
			count := len(provisionedEgress)
			provMutex.Unlock()

			utils.Logf("  NAT Gateways provisioned: %d/%d", count, numEgressServices)

			if count >= numEgressServices {
				break
			}
			time.Sleep(5 * time.Second)
		}

		totalCreateDuration := time.Since(createStart)
		azureProvisionDuration := time.Since(provisionStart)
		utils.Logf("✓ ALL %d NAT GATEWAYS PROVISIONED", len(provisionedEgress))
		utils.Logf("  Total creation time: %v", totalCreateDuration)
		utils.Logf("  Azure provisioning time: %v", azureProvisionDuration)

		// ============================================
		// PHASE 3: VERIFY ALL PODS HAVE FINALIZERS
		// ============================================
		utils.Logf("\n--- PHASE 3: Verifying pod finalizers ---")

		podsWithFinalizer := 0
		for i := 0; i < numEgressServices; i++ {
			pod, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), fmt.Sprintf("egress-pod-%d", i), metav1.GetOptions{})
			if err != nil {
				continue
			}
			for _, f := range pod.Finalizers {
				if f == serviceGatewayPodFinalizer {
					podsWithFinalizer++
					break
				}
			}
		}
		utils.Logf("✓ %d/%d pods have ServiceGateway finalizer", podsWithFinalizer, numEgressServices)

		// ============================================
		// PHASE 4: DELETE ALL PODS IN PARALLEL
		// ============================================
		utils.Logf("\n--- PHASE 4: Deleting %d pods ---", numEgressServices)
		deleteStart := time.Now()

		var deleteWg sync.WaitGroup
		for i := 0; i < numEgressServices; i++ {
			deleteWg.Add(1)
			go func(index int) {
				defer deleteWg.Done()
				cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), fmt.Sprintf("egress-pod-%d", index), metav1.DeleteOptions{})
			}(i)
		}
		deleteWg.Wait()

		k8sDeleteDuration := time.Since(deleteStart)
		utils.Logf("✓ K8s delete calls completed in %v", k8sDeleteDuration)

		// ============================================
		// PHASE 5: WAIT FOR ALL PODS TO BE GONE
		// ============================================
		utils.Logf("\n--- PHASE 5: Waiting for pods to disappear from K8s ---")

		for {
			if time.Since(deleteStart) > 20*time.Minute {
				utils.Logf("WARNING: Timeout waiting for pod deletion")
				break
			}

			remaining := 0
			for i := 0; i < numEgressServices; i++ {
				_, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), fmt.Sprintf("egress-pod-%d", i), metav1.GetOptions{})
				if !errors.IsNotFound(err) {
					remaining++
				}
			}

			if remaining > 0 && remaining <= 10 {
				utils.Logf("  Remaining pods: %d (last pods waiting for NAT Gateway cleanup)", remaining)
			} else {
				utils.Logf("  Remaining pods: %d", remaining)
			}

			if remaining == 0 {
				break
			}
			time.Sleep(2 * time.Second)
		}

		utils.Logf("✓ ALL %d PODS DELETED", numEgressServices)

		// ============================================
		// PHASE 6: VERIFY NAT GATEWAYS ARE CLEANED UP
		// ============================================
		utils.Logf("\n--- PHASE 6: Verifying NAT Gateway cleanup ---")

		for {
			if time.Since(deleteStart) > 25*time.Minute {
				utils.Logf("WARNING: Timeout waiting for NAT Gateway cleanup")
				break
			}

			sgResponse, err := queryServiceGatewayServices()
			if err != nil {
				time.Sleep(5 * time.Second)
				continue
			}

			remaining := 0
			for _, sgSvc := range sgResponse.Value {
				if sgSvc.Properties.ServiceType == "Outbound" && sgSvc.Name != "default-natgw-v2" {
					remaining++
				}
			}

			utils.Logf("  NAT Gateways remaining (excluding default): %d", remaining)

			if remaining == 0 {
				break
			}
			time.Sleep(5 * time.Second)
		}

		azureCleanupDuration := time.Since(deleteStart)
		utils.Logf("✓ All NAT Gateways cleaned up")

		// ============================================
		// FINAL SUMMARY
		// ============================================
		utils.Logf("\n" + strings.Repeat("=", 80))
		utils.Logf("OUTBOUND PERFORMANCE TEST RESULTS - %d EGRESS SERVICES", numEgressServices)
		utils.Logf(strings.Repeat("=", 80))
		utils.Logf("")
		utils.Logf("CREATION:")
		utils.Logf("  Total (K8s create → NAT Gateway ready):  %v", totalCreateDuration)
		utils.Logf("  Azure provisioning only:                  %v", azureProvisionDuration)
		utils.Logf("  Creation rate: %.2f NAT Gateways/second", float64(numEgressServices)/totalCreateDuration.Seconds())
		utils.Logf("")
		utils.Logf("DELETION:")
		utils.Logf("  Total (K8s delete → NAT Gateway gone):   %v", azureCleanupDuration)
		utils.Logf("  Deletion rate: %.2f NAT Gateways/second", float64(numEgressServices)/azureCleanupDuration.Seconds())
		utils.Logf(strings.Repeat("=", 80))

		// Cleanup namespace
		utils.DeleteNamespace(cs, ns.Name)

		Expect(totalCreateDuration).To(BeNumerically("<", 30*time.Minute), "NAT Gateway creation should complete within 30 minutes")
		Expect(azureCleanupDuration).To(BeNumerically("<", 25*time.Minute), "NAT Gateway deletion should complete within 25 minutes")
	})

	// Test 2: Pod scale test within a single egress service
	// Creates 1 egress service with N pods to test address location registration
	It("should handle 100 pods in a single egress service", NodeTimeout(1*time.Hour), Label("pod-scale"), func(ctx SpecContext) {
		const (
			numPods    = 100
			targetPort = 8080
		)

		// Use namespace name as egress name to avoid conflicts
		egressName := ns.Name

		utils.Logf("\n" + strings.Repeat("=", 80))
		utils.Logf("OUTBOUND POD SCALE TEST: %d PODS IN SINGLE EGRESS", numPods)
		utils.Logf(strings.Repeat("=", 80))
		utils.Logf("Test started at: %s", time.Now().Format(time.RFC3339))
		utils.Logf("Egress name: %s", egressName)

		// ============================================
		// PHASE 1: CREATE ALL PODS IN PARALLEL
		// ============================================
		utils.Logf("\n--- PHASE 1: Creating %d pods with egress label '%s' ---", numPods, egressName)
		createStart := time.Now()

		var wg sync.WaitGroup
		var createErrors int32
		var createMutex sync.Mutex

		for i := 0; i < numPods; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()

				podName := fmt.Sprintf("scale-pod-%d", index)

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

				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				if err != nil {
					createMutex.Lock()
					createErrors++
					createMutex.Unlock()
					utils.Logf("ERROR creating pod-%d: %v", index, err)
				}
			}(i)
		}
		wg.Wait()

		k8sCreateDuration := time.Since(createStart)
		utils.Logf("✓ K8s API calls completed in %v (%d pods created, %d errors)",
			k8sCreateDuration, numPods-int(createErrors), createErrors)

		// Wait for pods to be ready
		utils.Logf("  Waiting for pods to be ready...")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		podsReadyDuration := time.Since(createStart)
		utils.Logf("✓ All pods are ready in %v", podsReadyDuration)

		// ============================================
		// PHASE 2: WAIT FOR NAT GATEWAY AND ADDRESS LOCATIONS
		// ============================================
		utils.Logf("\n--- PHASE 2: Waiting for NAT Gateway and address registration ---")
		provisionStart := time.Now()

		var natGatewayReady bool
		var addressCount int

		for {
			if time.Since(provisionStart) > 10*time.Minute {
				utils.Logf("WARNING: Timeout waiting for NAT Gateway provisioning")
				break
			}

			// Check Service Gateway for our egress service
			sgResponse, err := queryServiceGatewayServices()
			if err != nil {
				time.Sleep(2 * time.Second)
				continue
			}

			for _, sgSvc := range sgResponse.Value {
				if sgSvc.Name == egressName && sgSvc.Properties.ServiceType == "Outbound" {
					natGatewayReady = true
					break
				}
			}

			// Check address locations
			addrResponse, err := queryServiceGatewayAddressLocations()
			if err != nil {
				time.Sleep(2 * time.Second)
				continue
			}

			totalAddresses := 0
			for _, loc := range addrResponse.Value {
				totalAddresses += len(loc.Addresses)
			}
			addressCount = totalAddresses

			utils.Logf("  NAT Gateway: %v, Addresses registered: %d", natGatewayReady, addressCount)

			if natGatewayReady && addressCount >= numPods {
				break
			}
			time.Sleep(2 * time.Second)
		}

		totalCreateDuration := time.Since(createStart)
		azureProvisionDuration := time.Since(provisionStart)
		utils.Logf("✓ NAT Gateway ready, %d addresses registered", addressCount)
		utils.Logf("  Total setup time: %v", totalCreateDuration)
		utils.Logf("  Azure registration time: %v", azureProvisionDuration)

		// ============================================
		// PHASE 3: VERIFY ALL PODS HAVE FINALIZERS
		// ============================================
		utils.Logf("\n--- PHASE 3: Verifying pod finalizers ---")

		podsWithFinalizer := 0
		for i := 0; i < numPods; i++ {
			pod, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), fmt.Sprintf("scale-pod-%d", i), metav1.GetOptions{})
			if err != nil {
				continue
			}
			for _, f := range pod.Finalizers {
				if f == serviceGatewayPodFinalizer {
					podsWithFinalizer++
					break
				}
			}
		}
		utils.Logf("✓ %d/%d pods have ServiceGateway finalizer", podsWithFinalizer, numPods)

		// ============================================
		// PHASE 4: DELETE ALL PODS IN PARALLEL
		// ============================================
		utils.Logf("\n--- PHASE 4: Deleting %d pods ---", numPods)
		deleteStart := time.Now()

		var deleteWg sync.WaitGroup
		for i := 0; i < numPods; i++ {
			deleteWg.Add(1)
			go func(index int) {
				defer deleteWg.Done()
				cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), fmt.Sprintf("scale-pod-%d", index), metav1.DeleteOptions{})
			}(i)
		}
		deleteWg.Wait()

		k8sDeleteDuration := time.Since(deleteStart)
		utils.Logf("✓ K8s delete calls completed in %v", k8sDeleteDuration)

		// ============================================
		// PHASE 5: WAIT FOR ALL PODS TO BE GONE
		// ============================================
		utils.Logf("\n--- PHASE 5: Waiting for pods to disappear ---")

		for {
			if time.Since(deleteStart) > 15*time.Minute {
				utils.Logf("WARNING: Timeout waiting for pod deletion")
				break
			}

			remaining := 0
			for i := 0; i < numPods; i++ {
				_, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), fmt.Sprintf("scale-pod-%d", i), metav1.GetOptions{})
				if !errors.IsNotFound(err) {
					remaining++
				}
			}

			if remaining > 0 && remaining <= 5 {
				utils.Logf("  Remaining pods: %d (last pods waiting for NAT Gateway cleanup)", remaining)
			} else if remaining > 0 {
				utils.Logf("  Remaining pods: %d", remaining)
			}

			if remaining == 0 {
				break
			}
			time.Sleep(2 * time.Second)
		}

		totalDeleteDuration := time.Since(deleteStart)
		utils.Logf("✓ ALL %d PODS DELETED IN: %v", numPods, totalDeleteDuration)

		// ============================================
		// PHASE 6: VERIFY NAT GATEWAY IS CLEANED UP
		// ============================================
		utils.Logf("\n--- PHASE 6: Verifying NAT Gateway cleanup ---")

		for {
			if time.Since(deleteStart) > 20*time.Minute {
				break
			}

			sgResponse, err := queryServiceGatewayServices()
			if err != nil {
				time.Sleep(2 * time.Second)
				continue
			}

			found := false
			for _, sgSvc := range sgResponse.Value {
				if sgSvc.Name == egressName {
					found = true
					break
				}
			}

			if !found {
				utils.Logf("✓ NAT Gateway '%s' cleaned up", egressName)
				break
			}

			utils.Logf("  NAT Gateway '%s' still exists, waiting...", egressName)
			time.Sleep(2 * time.Second)
		}

		azureCleanupDuration := time.Since(deleteStart)

		// ============================================
		// FINAL SUMMARY
		// ============================================
		utils.Logf("\n" + strings.Repeat("=", 80))
		utils.Logf("OUTBOUND POD SCALE TEST RESULTS - %d PODS", numPods)
		utils.Logf(strings.Repeat("=", 80))
		utils.Logf("")
		utils.Logf("CREATION:")
		utils.Logf("  Total (K8s create → All registered):     %v", totalCreateDuration)
		utils.Logf("  Pods ready time:                          %v", podsReadyDuration)
		utils.Logf("  Address registration rate: %.2f pods/second", float64(numPods)/totalCreateDuration.Seconds())
		utils.Logf("")
		utils.Logf("DELETION:")
		utils.Logf("  Total (K8s delete → NAT Gateway gone):   %v", azureCleanupDuration)
		utils.Logf("  Pod deletion rate: %.2f pods/second", float64(numPods)/totalDeleteDuration.Seconds())
		utils.Logf(strings.Repeat("=", 80))

		// Cleanup namespace
		utils.DeleteNamespace(cs, ns.Name)

		Expect(totalCreateDuration).To(BeNumerically("<", 15*time.Minute), "Pod registration should complete within 15 minutes")
		Expect(azureCleanupDuration).To(BeNumerically("<", 20*time.Minute), "Cleanup should complete within 20 minutes")
	})
})
