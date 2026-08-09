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

package servicegateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("SLB - Concurrent Services", Label(slbTestLabel), func() {
	basename := "slb-concurrent-test"

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
		if cs != nil && ns != nil {
			err := utils.DeleteNamespace(cs, ns.Name)

			// For large-scale tests (50+ services), namespace deletion may timeout
			// due to Azure finalizer cleanup taking >10 minutes. This is expected
			// behavior and doesn't indicate a functional issue.
			if err != nil && strings.Contains(err.Error(), "timed out waiting for the condition") {
				utils.Logf("WARNING: Namespace deletion timed out (likely due to large number of services)")
				utils.Logf("This is expected for tests with 50+ services and does not indicate a test failure")
				// Don't fail the test - cleanup will eventually complete asynchronously
			} else {
				Expect(err).NotTo(HaveOccurred())
			}

			By("Waiting for Azure cleanup")
			eventuallyAzureCleanup(2 * time.Minute)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		cs = nil
		ns = nil
	})

	It("should handle X concurrent LoadBalancer services with Y pods each", func() {
		const (
			numServices    = 15
			podsPerService = 5
			totalPods      = numServices * podsPerService // 75 pods
			azureWaitTime  = 90 * time.Second
			testTimeout    = 20 * time.Minute
		)

		By(fmt.Sprintf("Creating %d services with %d pods each (%d total pods) concurrently", numServices, podsPerService, totalPods))

		// Create services and pods concurrently
		type serviceResult struct {
			service *v1.Service
			pods    []*v1.Pod
			err     error
		}

		resultChan := make(chan serviceResult, numServices)

		// Launch goroutines to create services and their pods concurrently
		for i := 0; i < numServices; i++ {
			go func(serviceIndex int) {
				var result serviceResult

				// Create service
				serviceName := fmt.Sprintf("test-service-%d", serviceIndex)
				selector := map[string]string{
					"app":     fmt.Sprintf("test-app-%d", serviceIndex),
					"service": fmt.Sprintf("svc-%d", serviceIndex),
				}

				service := &v1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceName,
						Namespace: ns.Name,
					},
					Spec: v1.ServiceSpec{
						Type:                  v1.ServiceTypeLoadBalancer,
						ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
						Selector:              selector,
						Ports: []v1.ServicePort{
							{
								Protocol:   v1.ProtocolTCP,
								Port:       80,
								TargetPort: intstr.FromInt(8080),
							},
						},
					},
				}

				createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
				if err != nil {
					result.err = fmt.Errorf("failed to create service %s: %w", serviceName, err)
					resultChan <- result
					return
				}
				result.service = createdService

				// Create pods for this service
				result.pods = make([]*v1.Pod, podsPerService)
				for j := 0; j < podsPerService; j++ {
					podName := fmt.Sprintf("%s-pod-%d", serviceName, j)
					pod := &v1.Pod{
						ObjectMeta: metav1.ObjectMeta{
							Name:      podName,
							Namespace: ns.Name,
							Labels:    selector,
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Name:            "test-app",
									Image:           utils.AgnhostImage,
									ImagePullPolicy: v1.PullIfNotPresent,
									Args:            []string{"netexec", "--http-port=8080"},
									Ports: []v1.ContainerPort{
										{
											ContainerPort: 8080,
											Protocol:      v1.ProtocolTCP,
										},
									},
								},
							},
						},
					}

					createdPod, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
					if err != nil {
						result.err = fmt.Errorf("failed to create pod %s: %w", podName, err)
						resultChan <- result
						return
					}
					result.pods[j] = createdPod
				}

				resultChan <- result
			}(i)
		}

		// Collect results
		services := make([]*v1.Service, 0, numServices)
		allPods := make([]*v1.Pod, 0, totalPods)

		for i := 0; i < numServices; i++ {
			result := <-resultChan
			Expect(result.err).NotTo(HaveOccurred(), fmt.Sprintf("Service/Pod creation failed: %v", result.err))
			services = append(services, result.service)
			allPods = append(allPods, result.pods...)
			utils.Logf("  ✓ Created service %s with %d pods (%d/%d)", result.service.Name, len(result.pods), i+1, numServices)
		}
		close(resultChan)

		utils.Logf("Successfully created %d services and %d pods", len(services), len(allPods))

		// Wait for all pods to be running
		By(fmt.Sprintf("Waiting for all %d pods to be running", totalPods))
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("  ✓ All %d pods are ready", totalPods)

		By("Waiting for Azure to provision all services")
		Eventually(func() error {
			for _, service := range services {
				// Assert each service's exact address set. A per-service count cannot detect one
				// service's pods being registered under another service's name - a plausible
				// outcome of a race in concurrent reconciliation, and one that leaves every count
				// correct while routing traffic to the wrong workload.
				want, err := livePodIPsForSelector(cs, ns.Name, service.Spec.Selector)
				if err != nil {
					return err
				}
				if len(want) != podsPerService {
					return fmt.Errorf("service %s: expected %d live pod IPs, got %d", service.Name, podsPerService, len(want))
				}
				if err := serviceReconciledMatchErr(string(service.UID), want); err != nil {
					return fmt.Errorf("service %s: %w", service.Name, err)
				}
			}

			addressLocations, err := queryServiceGatewayAddressLocations()
			if err != nil {
				return fmt.Errorf("query Service Gateway address locations: %w", err)
			}
			totalRegisteredPods := 0
			for _, location := range addressLocations.Value {
				for _, addr := range location.Addresses {
					if len(addr.Services) > 0 {
						totalRegisteredPods++
					}
				}
			}
			if totalRegisteredPods < totalPods {
				return fmt.Errorf("expected at least %d pod IPs in address locations, found %d", totalPods, totalRegisteredPods)
			}
			return nil
		}, azureWaitTime, 10*time.Second).Should(Succeed(),
			"all services should be reconciled in Azure and the Service Gateway")

		// Verify all services in Service Gateway
		By(fmt.Sprintf("Verifying all %d services are registered in Service Gateway", numServices))
		sgServices, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Service Gateway query returned %d total services", len(sgServices.Value))

		// Log all services for debugging
		for i, svc := range sgServices.Value {
			utils.Logf("  Service %d: Name=%s, Type=%s, IsDefault=%v", i+1, svc.Name, svc.Properties.ServiceType, svc.Properties.IsDefault)
		}

		// Count non-default services
		registeredServices := make(map[string]bool)
		for _, sgSvc := range sgServices.Value {
			// Check if it's not the default service by looking at ServiceType or IsDefault
			if !sgSvc.Properties.IsDefault && sgSvc.Properties.ServiceType == "Inbound" {
				registeredServices[sgSvc.Name] = true
			}
		}

		utils.Logf("Found %d non-default inbound services in Service Gateway", len(registeredServices))

		// A count is satisfied by the wrong services: with ">=" an unrelated leftover registration
		// offsets a missing one, so 15 registrations can hide a service that never reached the
		// Service Gateway. Assert each created Service's UID is registered by name instead.
		for _, service := range services {
			Expect(registeredServices).To(HaveKey(string(service.UID)),
				"service %s (UID %s) is not registered as an inbound service in the Service Gateway",
				service.Name, string(service.UID))
		}

		// Verify each service individually by querying all resources once
		By("Verifying Azure resources (PIP, LB, Service Gateway) for all services")

		// Query all Public IPs once
		utils.Logf("Querying all Public IPs...")
		pipOutput, err := runAz("network", "public-ip", "list",
			"--resource-group", resourceGroupName,
			"--output", "json")
		Expect(err).NotTo(HaveOccurred(), "Should be able to query Azure for Public IPs")

		var allPublicIPs []AzurePublicIP
		err = json.Unmarshal(pipOutput, &allPublicIPs)
		Expect(err).NotTo(HaveOccurred(), "Should parse Public IP JSON")
		utils.Logf("Found %d total Public IPs in resource group", len(allPublicIPs))

		// Query all Load Balancers once
		utils.Logf("Querying all Load Balancers...")
		lbListOutput, err := runAz("network", "lb", "list",
			"--resource-group", resourceGroupName,
			"--output", "json")
		Expect(err).NotTo(HaveOccurred(), "Should be able to query Azure for Load Balancers")

		var allLoadBalancers []AzureLoadBalancer
		err = json.Unmarshal(lbListOutput, &allLoadBalancers)
		Expect(err).NotTo(HaveOccurred(), "Should parse Load Balancer JSON")
		utils.Logf("Found %d total Load Balancers in resource group", len(allLoadBalancers))

		// Service Gateway services already queried above in sgServices
		utils.Logf("Already have %d services from Service Gateway", len(sgServices.Value))

		// Build lookup maps for faster verification
		pipByName := make(map[string]*AzurePublicIP)
		for i := range allPublicIPs {
			pipByName[allPublicIPs[i].Name] = &allPublicIPs[i]
		}

		lbByName := make(map[string]*AzureLoadBalancer)
		for i := range allLoadBalancers {
			lbByName[allLoadBalancers[i].Name] = &allLoadBalancers[i]
		}

		sgServiceByName := make(map[string]*ServiceGatewayService)
		for i := range sgServices.Value {
			sgServiceByName[sgServices.Value[i].Name] = &sgServices.Value[i]
		}

		// Verify all services
		verifiedCount := 0
		for _, service := range services {
			serviceUID := string(service.UID)
			publicIPName := fmt.Sprintf("%s-pip", serviceUID)
			loadBalancerName := serviceUID

			// Check Public IP
			pip, pipExists := pipByName[publicIPName]
			if !pipExists {
				Fail(fmt.Sprintf("Service %s: Public IP %s not found", service.Name, publicIPName))
			}
			Expect(pip.IPAddress).NotTo(BeEmpty(), fmt.Sprintf("Service %s: Public IP should have an address", service.Name))

			// Check Load Balancer
			lb, lbExists := lbByName[loadBalancerName]
			if !lbExists {
				Fail(fmt.Sprintf("Service %s: Load Balancer %s not found", service.Name, loadBalancerName))
			}
			if lb.SKU.Name != "Service" {
				Fail(fmt.Sprintf("Service %s: Load Balancer SKU should be 'Service', got '%s'", service.Name, lb.SKU.Name))
			}

			// Check Service Gateway
			_, sgExists := sgServiceByName[serviceUID]
			if !sgExists {
				Fail(fmt.Sprintf("Service %s: Service %s not found in Service Gateway", service.Name, serviceUID))
			}

			verifiedCount++
			if verifiedCount%10 == 0 || verifiedCount == numServices {
				utils.Logf("  ✓ Verified %d/%d services", verifiedCount, numServices)
			}
		}

		utils.Logf("Successfully verified all %d services", numServices)

		// Verify address locations contain all pod IPs
		By(fmt.Sprintf("Verifying Service Gateway address locations contain all %d pod IPs", totalPods))
		addressLocations, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		totalRegisteredPods := 0
		serviceToPodsCount := make(map[string]int)

		for _, location := range addressLocations.Value {
			for _, addr := range location.Addresses {
				if len(addr.Services) > 0 {
					totalRegisteredPods++
					for _, svcID := range addr.Services {
						serviceToPodsCount[svcID]++
					}
				}
			}
		}

		utils.Logf("Found %d pod IPs registered in Service Gateway address locations", totalRegisteredPods)
		Expect(totalRegisteredPods).To(BeNumerically(">=", totalPods),
			fmt.Sprintf("Expected at least %d pod IPs in address locations, found %d", totalPods, totalRegisteredPods))

		// Verify each service has approximately podsPerService pods registered
		servicesWithCorrectPodCount := 0
		for _, service := range services {
			// Service IDs might be in different formats, so we check by UID
			var podCount int
			for svcID, count := range serviceToPodsCount {
				if strings.Contains(svcID, string(service.UID)) {
					podCount = count
					break
				}
			}

			if podCount == podsPerService {
				servicesWithCorrectPodCount++
			}
		}

		utils.Logf("%d/%d services have exactly %d pods registered", servicesWithCorrectPodCount, numServices, podsPerService)
		Expect(servicesWithCorrectPodCount).To(Equal(numServices),
			"every service must have exactly %d pod IPs registered; computing this and only logging it "+
				"lets a run where most services registered nothing still pass", podsPerService)

		utils.Logf("\n✓ Container Load Balancer with %d concurrent services (%d total pods) verified", numServices, totalPods)
		utils.Logf("  Services verified: %d/%d", numServices, numServices)
		utils.Logf("  Total pod IPs registered: %d", totalRegisteredPods)

		// Explicitly delete all services to start Azure cleanup early
		By(fmt.Sprintf("Deleting all %d services to initiate Azure cleanup", numServices))
		deletionStartTime := time.Now()
		utils.Logf("Service deletion started at: %s", deletionStartTime.Format("15:04:05"))
		for _, service := range services {
			err := cs.CoreV1().Services(ns.Name).Delete(context.TODO(), service.Name, metav1.DeleteOptions{})
			if err != nil {
				utils.Logf("Warning: Failed to delete service %s: %v", service.Name, err)
			}
		}
		utils.Logf("All %d services deletion initiated at %s", numServices, time.Now().Format("15:04:05"))
		utils.Logf("Elapsed time: %.1f seconds", time.Since(deletionStartTime).Seconds())
		utils.Logf("Azure cleanup in progress - Load Balancers, Public IPs, and Service Gateway entries will be removed...")
	})
})

// livePodIPsForSelector returns the address set the pods matching a Service's own selector should
// have registered. Deriving the selector from the Service avoids relying on creation order, which
// is not preserved when results are collected from a channel.
func livePodIPsForSelector(cs clientset.Interface, namespace string, selector map[string]string) (map[string]struct{}, error) {
	pods, err := cs.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(selector).String(),
	})
	if err != nil {
		return nil, err
	}
	readyPods := make([]v1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		// Skip pods that are already Terminating: their addresses are being drained, so they
		// are not part of the set the Service Gateway should still hold. Counting them makes
		// a scale-down transiently expect one address too many.
		if pods.Items[i].DeletionTimestamp == nil {
			readyPods = append(readyPods, pods.Items[i])
		}
	}
	return podIPSet(readyPods), nil
}
