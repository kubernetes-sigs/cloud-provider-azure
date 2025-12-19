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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("Container Load Balancer Performance Test", Label(clbTestLabel, "performance"), func() {
	basename := "clb-perf-test"

	var (
		cs            clientset.Interface
		ns            *v1.Namespace
		totalServices int // Track service count for dynamic cleanup wait
	)

	BeforeEach(func() {
		var err error
		cs, err = utils.CreateKubeClientSet()
		Expect(err).NotTo(HaveOccurred())

		ns, err = utils.CreateTestingNamespace(basename, cs)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if cs != nil && ns != nil {
			utils.Logf("\n=== CLEANUP PHASE ===")
			utils.Logf("Deleting namespace with test resources...")
			cleanupStart := time.Now()

			err := utils.DeleteNamespace(cs, ns.Name)
			if err != nil {
				utils.Logf("WARNING: Namespace deletion returned error (may timeout for large cleanup): %v", err)
			}

			cleanupDuration := time.Since(cleanupStart)
			utils.Logf("Namespace deletion call took: %v", cleanupDuration)

			// Progressive cleanup checks with early exit (like provisioning)
			azureCleanupStart := time.Now()
			baseCleanupInterval := 60 * time.Second // 1 minute base
			cleanupScaleFactor := time.Duration(totalServices / 40)
			if cleanupScaleFactor < 1 {
				cleanupScaleFactor = 1 // Minimum 1x
			}
			cleanupIntervals := []time.Duration{
				baseCleanupInterval * cleanupScaleFactor,
				baseCleanupInterval * 2 * cleanupScaleFactor,
				baseCleanupInterval * 3 * cleanupScaleFactor,
				baseCleanupInterval * 4 * cleanupScaleFactor,
			}

			utils.Logf("\n--- Waiting for Azure cleanup (checking after each interval) ---")
			cleanedUp := false
			for i, interval := range cleanupIntervals {
				utils.Logf("Waiting %v before checking cleanup status... (interval %d/%d)", interval, i+1, len(cleanupIntervals))
				time.Sleep(interval)

				elapsed := time.Since(azureCleanupStart)
				utils.Logf("Elapsed cleanup time: %v", elapsed)

				// Check Service Gateway cleanup status
				utils.Logf("Checking Service Gateway cleanup...")
				sgResponse, err := queryServiceGatewayServices()
				if err != nil {
					utils.Logf("  WARNING: Failed to query Service Gateway: %v", err)
					continue // Try next interval
				}

				remainingServices := len(sgResponse.Value) - 1 // Subtract 1 for default-natgw-v2
				utils.Logf("  Remaining services in Service Gateway: %d (expected: 0)", remainingServices)

				if remainingServices <= 0 {
					utils.Logf("✓ Azure cleanup complete! Breaking early after %v", elapsed)
					cleanedUp = true
					break
				}

				utils.Logf("  Cleanup not complete yet, continuing to next interval...")
			}

			if !cleanedUp {
				utils.Logf("WARNING: Cleanup intervals exhausted, proceeding with verification...")
			}

			By("Verifying Service Gateway cleanup")
			sgCleanupStart := time.Now()
			verifyServiceGatewayCleanup()
			utils.Logf("Service Gateway cleanup verification took: %v", time.Since(sgCleanupStart))

			By("Verifying Address Locations cleanup")
			alCleanupStart := time.Now()
			verifyAddressLocationsCleanup()
			utils.Logf("Address Locations cleanup verification took: %v", time.Since(alCleanupStart))
		}

		cs = nil
		ns = nil
	})

	It("should create LoadBalancer services with pods and measure performance", NodeTimeout(2*time.Hour), func(ctx SpecContext) {
		const (
			numServices       = 500
			podsPerService    = 1
			servicePort       = int32(8080)
			targetPort        = 8080
			batchSize         = 500 // Create all services in parallel
			verificationBatch = 10  // Verify in batches to avoid overwhelming API
		)

		totalServices = numServices // Set for dynamic cleanup wait calculation

		utils.Logf("\n=== PERFORMANCE TEST: %d SERVICES (%d POD%s EACH) ===", totalServices, podsPerService, map[bool]string{true: "", false: "S"}[podsPerService == 1])
		utils.Logf("Cluster: e2e-enechitoaia-67")
		utils.Logf("Test started at: %s", time.Now().Format(time.RFC3339))

		// Track service UIDs for verification
		serviceUIDs := make([]string, 0, totalServices)
		var serviceUIDsMutex sync.Mutex

		// ============================================
		// PHASE 1: SERVICE CREATION
		// ============================================
		utils.Logf("\n--- PHASE 1: Creating %d services ---", totalServices)
		creationStart := time.Now()

		for batch := 0; batch < totalServices; batch += batchSize {
			batchStart := time.Now()
			end := batch + batchSize
			if end > totalServices {
				end = totalServices
			}

			utils.Logf("Creating services %d-%d...", batch+1, end)

			// Create services in parallel within batch
			var wg sync.WaitGroup
			for i := batch; i < end; i++ {
				wg.Add(1)
				go func(index int) {
					defer wg.Done()

					serviceName := fmt.Sprintf("perf-svc-%d", index)
					serviceLabels := map[string]string{
						"app":   "perf-test",
						"index": fmt.Sprintf("%d", index),
					}

					service := &v1.Service{
						ObjectMeta: metav1.ObjectMeta{
							Name:      serviceName,
							Namespace: ns.Name,
							Labels: map[string]string{
								"test":  "performance",
								"batch": fmt.Sprintf("%d", index/batchSize),
							},
						},
						Spec: v1.ServiceSpec{
							Type:                  v1.ServiceTypeLoadBalancer,
							ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
							Selector:              serviceLabels,
							Ports: []v1.ServicePort{
								{
									Port:       servicePort,
									TargetPort: intstr.FromInt(targetPort),
									Protocol:   v1.ProtocolTCP,
								},
							},
						},
					}

					createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
					if err != nil {
						utils.Logf("ERROR: Failed to create service %s: %v", serviceName, err)
						return
					}

					serviceUIDsMutex.Lock()
					serviceUIDs = append(serviceUIDs, string(createdService.UID))
					serviceUIDsMutex.Unlock()
				}(i)
			}

			wg.Wait()
			batchDuration := time.Since(batchStart)
			utils.Logf("  Batch %d-%d created in %v (avg: %.2f services/sec)",
				batch+1, end, batchDuration, float64(batchSize)/batchDuration.Seconds())
		}

		creationDuration := time.Since(creationStart)
		creationRate := float64(totalServices) / creationDuration.Seconds()

		utils.Logf("\n✓ SERVICE CREATION COMPLETE:")
		utils.Logf("  Total services: %d", len(serviceUIDs))
		utils.Logf("  Creation time: %v", creationDuration)
		utils.Logf("  Creation rate: %.2f services/second", creationRate)
		utils.Logf("  Average time per service: %v", creationDuration/time.Duration(totalServices))

		Expect(len(serviceUIDs)).To(Equal(totalServices), "All services should be created")

		// ============================================
		// PHASE 2: POD CREATION
		// ============================================
		totalPods := totalServices * podsPerService
		utils.Logf("\n--- PHASE 2: Creating %d pods (%d per service) ---", totalPods, podsPerService)
		podCreationStart := time.Now()

		var podWg sync.WaitGroup
		for i := 0; i < totalServices; i++ {
			for j := 0; j < podsPerService; j++ {
				podWg.Add(1)
				go func(serviceIndex, podIndex int) {
					defer podWg.Done()

					podName := fmt.Sprintf("perf-pod-%d-%d", serviceIndex, podIndex)
					podLabels := map[string]string{
						"app":   "perf-test",
						"index": fmt.Sprintf("%d", serviceIndex),
					}

					pod := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      podName,
							Namespace: ns.Name,
							Labels:    podLabels,
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:  "nginx",
									Image: "nginx:alpine",
									Ports: []v1.ContainerPort{
										{
											ContainerPort: int32(targetPort),
											Protocol:      v1.ProtocolTCP,
										},
									},
								},
							},
						},
					}

					_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
					if err != nil {
						utils.Logf("ERROR: Failed to create pod %s: %v", podName, err)
					}
				}(i, j)
			}
		}

		podWg.Wait()
		podCreationDuration := time.Since(podCreationStart)
		podCreationRate := float64(totalPods) / podCreationDuration.Seconds()

		utils.Logf("\n✓ POD CREATION COMPLETE:")
		utils.Logf("  Total pods: %d", totalPods)
		utils.Logf("  Creation time: %v", podCreationDuration)
		utils.Logf("  Creation rate: %.2f pods/second", podCreationRate)

		// Wait for pods to be ready
		utils.Logf("\nWaiting for pods to become ready...")
		podReadyStart := time.Now()
		Eventually(func() int {
			readyCount := 0
			for i := 0; i < totalServices; i++ {
				for j := 0; j < podsPerService; j++ {
					pod, err := cs.CoreV1().Pods(ns.Name).Get(context.TODO(), fmt.Sprintf("perf-pod-%d-%d", i, j), metav1.GetOptions{})
					if err == nil && pod.Status.Phase == v1.PodRunning {
						readyCount++
					}
				}
			}
			utils.Logf("  Pods ready: %d/%d", readyCount, totalPods)
			return readyCount
		}, 5*time.Minute, 10*time.Second).Should(Equal(totalPods), "All pods should be running")

		podReadyDuration := time.Since(podReadyStart)
		utils.Logf("✓ All pods ready in %v", podReadyDuration)

		// ============================================
		// PHASE 3: AZURE PROVISIONING
		// ============================================
		utils.Logf("\n--- PHASE 3: Waiting for Azure provisioning ---")

		// Progressive wait with status checks
		provisioningStart := time.Now()

		// Scale wait times based on service count
		baseInterval := 30 * time.Second
		scaleFactor := time.Duration(totalServices / 40) // 1x for 20, 10x for 200
		if scaleFactor < 1 {
			scaleFactor = 1 // Minimum scale factor of 1
		}
		checkIntervals := []time.Duration{
			baseInterval * scaleFactor,
			baseInterval * 2 * scaleFactor,
			baseInterval * 3 * scaleFactor,
			baseInterval * 4 * scaleFactor,
		}

		for i, interval := range checkIntervals {
			utils.Logf("Waiting %v before checking Service Gateway... (interval %d/%d)", interval, i+1, len(checkIntervals))
			time.Sleep(interval)

			elapsed := time.Since(provisioningStart)
			utils.Logf("Elapsed provisioning time: %v", elapsed)

			// Check Service Gateway registration
			utils.Logf("Checking Service Gateway registration...")
			sgResponse, err := queryServiceGatewayServices()
			if err != nil {
				utils.Logf("  WARNING: Failed to query Service Gateway: %v", err)
				continue // Try next interval
			}

			registeredCount := len(sgResponse.Value)
			utils.Logf("  Service Gateway services: %d/%d (%.2f%%)", registeredCount-1, totalServices, float64(registeredCount-1)/float64(totalServices)*100)

			// Check if all services are registered (minus 1 for default-natgw-v2)
			if registeredCount-1 >= totalServices {
				utils.Logf("✓ All services registered in Service Gateway! Breaking early after %v", elapsed)
				break
			}

			utils.Logf("  Not all services registered yet, continuing to next interval...")
		}

		// ============================================
		// PHASE 4: SERVICE GATEWAY VERIFICATION
		// ============================================
		utils.Logf("\n--- PHASE 4: Verifying Service Gateway registration ---")
		sgVerificationStart := time.Now()

		utils.Logf("Querying Service Gateway for all services...")
		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Service Gateway returned %d total services", len(sgResponse.Value))

		// Count how many of our test services are registered
		registeredCount := 0
		serviceUIDMap := make(map[string]bool)
		for _, uid := range serviceUIDs {
			serviceUIDMap[uid] = true
		}

		for _, sgSvc := range sgResponse.Value {
			if serviceUIDMap[sgSvc.Name] {
				registeredCount++
			}
		}

		sgVerificationDuration := time.Since(sgVerificationStart)

		utils.Logf("\n✓ SERVICE GATEWAY VERIFICATION:")
		utils.Logf("  Services registered: %d/%d", registeredCount, totalServices)
		utils.Logf("  Registration rate: %.2f%%", float64(registeredCount)/float64(totalServices)*100)
		utils.Logf("  Verification time: %v", sgVerificationDuration)

		// ============================================
		// PHASE 4.5: ADDRESS LOCATIONS VERIFICATION
		// ============================================
		utils.Logf("\n--- PHASE 4.5: Verifying Address Locations registration ---")
		alVerificationStart := time.Now()

		utils.Logf("Querying Service Gateway for address locations...")
		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Service Gateway returned %d address location(s)", len(alResponse.Value))

		// Count total addresses and which services they map to
		totalAddresses := 0
		addressesWithServices := make(map[string][]string) // service UID -> list of pod IPs
		servicesInAddresses := make(map[string]bool)       // track which service UIDs appear

		for _, location := range alResponse.Value {
			utils.Logf("  Address Location: %s (%d addresses)", location.AddressLocation, len(location.Addresses))
			for _, addr := range location.Addresses {
				totalAddresses++
				for _, svcUID := range addr.Services {
					addressesWithServices[svcUID] = append(addressesWithServices[svcUID], addr.Address)
					servicesInAddresses[svcUID] = true
				}
			}
		}

		// Count how many of our test services have addresses registered
		servicesWithAddresses := 0
		for _, uid := range serviceUIDs {
			if servicesInAddresses[uid] {
				servicesWithAddresses++
			}
		}

		alVerificationDuration := time.Since(alVerificationStart)

		utils.Logf("\n✓ ADDRESS LOCATIONS VERIFICATION:")
		utils.Logf("  Total addresses registered: %d", totalAddresses)
		utils.Logf("  Services with addresses: %d/%d", servicesWithAddresses, totalServices)
		utils.Logf("  Address registration rate: %.2f%%", float64(servicesWithAddresses)/float64(totalServices)*100)
		utils.Logf("  Expected pods: %d, Registered addresses: %d", totalPods, totalAddresses)
		utils.Logf("  Verification time: %v", alVerificationDuration)

		// ============================================
		// PHASE 5: AZURE RESOURCES SPOT CHECK
		// ============================================
		// Sample up to 10 services, or all if less than 10
		sampleSize := 10
		if totalServices < sampleSize {
			sampleSize = totalServices
		}
		sampleServices := make([]int, 0, sampleSize)
		if totalServices <= 10 {
			// Sample all services
			for i := 0; i < totalServices; i++ {
				sampleServices = append(sampleServices, i)
			}
		} else {
			// Sample evenly distributed services
			step := float64(totalServices-1) / float64(sampleSize-1)
			for i := 0; i < sampleSize; i++ {
				idx := int(float64(i) * step)
				sampleServices = append(sampleServices, idx)
			}
		}

		utils.Logf("\n--- PHASE 5: Azure resources spot check (sample of %d services) ---", len(sampleServices))
		spotCheckStart := time.Now()

		verifiedSample := 0

		for _, idx := range sampleServices {
			serviceUID := serviceUIDs[idx]
			err := verifyAzureResources(serviceUID)
			if err == nil {
				verifiedSample++
			} else {
				utils.Logf("  Service %d (UID: %s): %v", idx, serviceUID, err)
			}
		}

		spotCheckDuration := time.Since(spotCheckStart)

		utils.Logf("\n✓ AZURE RESOURCES SPOT CHECK:")
		utils.Logf("  Verified: %d/%d sampled services", verifiedSample, len(sampleServices))
		utils.Logf("  Spot check time: %v", spotCheckDuration)

		// ============================================
		// FINAL SUMMARY
		// ============================================
		totalDuration := time.Since(creationStart)

		utils.Logf("\n" + strings.Repeat("=", 60))
		utils.Logf("PERFORMANCE TEST SUMMARY - %d SERVICES (%d POD%s EACH)", totalServices, podsPerService, map[bool]string{true: "", false: "S"}[podsPerService == 1])
		utils.Logf(strings.Repeat("=", 60))
		utils.Logf("Test completion time: %s", time.Now().Format(time.RFC3339))
		utils.Logf("")
		utils.Logf("TIMINGS:")
		utils.Logf("  1. Service creation:              %v (%.2f svc/sec)", creationDuration, creationRate)
		utils.Logf("  2. Pod creation:                  %v (%.2f pod/sec)", podCreationDuration, podCreationRate)
		utils.Logf("  3. Pod ready wait:                %v", podReadyDuration)
		utils.Logf("  4. Azure provisioning wait:       %v", time.Since(provisioningStart))
		utils.Logf("  5. Service Gateway verification:  %v", sgVerificationDuration)
		utils.Logf("  6. Address Locations verification: %v", alVerificationDuration)
		utils.Logf("  7. Azure resources spot check:    %v", spotCheckDuration)
		utils.Logf("  TOTAL END-TO-END TIME:            %v", totalDuration)
		utils.Logf("")
		utils.Logf("RESULTS:")
		utils.Logf("  Services created:               %d/%d", len(serviceUIDs), totalServices)
		utils.Logf("  Pods created:                   %d/%d", totalPods, totalPods)
		utils.Logf("  Service Gateway registered:     %d/%d (%.2f%%)", registeredCount, totalServices, float64(registeredCount)/float64(totalServices)*100)
		utils.Logf("  Address Locations registered:   %d/%d (%.2f%%)", servicesWithAddresses, totalServices, float64(servicesWithAddresses)/float64(totalServices)*100)
		utils.Logf("  Total pod addresses registered: %d/%d", totalAddresses, totalPods)
		utils.Logf("  Azure resources verified:       %d/%d samples", verifiedSample, len(sampleServices))
		utils.Logf("")
		utils.Logf("THROUGHPUT:")
		utils.Logf("  Overall rate:                   %.2f services/second", float64(totalServices)/totalDuration.Seconds())
		utils.Logf("  Avg time per service+pod:       %v", totalDuration/time.Duration(totalServices))
		utils.Logf(strings.Repeat("=", 60))

		// Assert minimum success criteria
		Expect(len(serviceUIDs)).To(Equal(totalServices), "All services should be created")
		Expect(registeredCount).To(BeNumerically(">=", totalServices*80/100), "At least 80% of services should be registered in Service Gateway")
		Expect(verifiedSample).To(BeNumerically(">=", len(sampleServices)*70/100), "At least 70% of sampled services should have Azure resources")
	})
})
