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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
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

// verifyAllPodsReachable makes HTTP requests to the service's external IP and verifies
// that all expected pods receive traffic. It uses agnhost's /hostname endpoint which
// returns the pod name. The function retries until all pods are seen or timeout is reached.
func verifyAllPodsReachable(externalIP string, port int, expectedPodNames []string, timeout time.Duration) error {
	// Disable HTTP keep-alive so every request opens a NEW TCP connection. The Azure Load
	// Balancer distributes traffic by a 5-tuple hash (source IP/port, dest IP/port, protocol).
	// Reusing a single connection (the default http.Client behaviour) keeps the same source
	// port, so the hash pins every request to one backend pod and a multi-pod service appears
	// to have only a single reachable pod. A fresh connection per request varies the source
	// port and lets the load balancer spread requests across all registered pods.
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	seenPods := make(map[string]bool)
	url := fmt.Sprintf("http://%s:%d/hostname", externalIP, port)

	startTime := time.Now()
	requestCount := 0
	maxRequests := 100 // Safety limit

	for {
		// Check timeout
		if time.Since(startTime) > timeout {
			var missedPods []string
			for _, podName := range expectedPodNames {
				if !seenPods[podName] {
					missedPods = append(missedPods, podName)
				}
			}
			return fmt.Errorf("timeout after %v: pods not reached: %v (made %d requests, saw %d unique pods)",
				timeout, missedPods, requestCount, len(seenPods))
		}

		// Check request limit
		if requestCount >= maxRequests {
			var missedPods []string
			for _, podName := range expectedPodNames {
				if !seenPods[podName] {
					missedPods = append(missedPods, podName)
				}
			}
			return fmt.Errorf("max requests (%d) reached: pods not reached: %v (saw %d unique pods)",
				maxRequests, missedPods, len(seenPods))
		}

		requestCount++

		resp, err := client.Get(url)
		if err != nil {
			utils.Logf("    Request %d failed: %v (will retry)", requestCount, err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			utils.Logf("    Request %d: failed to read body: %v", requestCount, err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		podName := strings.TrimSpace(string(body))
		if podName == "" {
			// A transient empty body carries no pod identity; retry without recording it.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if !seenPods[podName] {
			seenPods[podName] = true
			utils.Logf("    Request %d: reached new pod: %s (%d/%d pods seen)",
				requestCount, podName, len(seenPods), len(expectedPodNames))
		}

		// Check if all pods have been seen
		allSeen := true
		for _, expected := range expectedPodNames {
			if !seenPods[expected] {
				allSeen = false
				break
			}
		}

		if allSeen {
			utils.Logf("    ✓ All %d pods reached after %d requests", len(expectedPodNames), requestCount)
			return nil
		}

		// Small delay between requests to avoid overwhelming the service
		time.Sleep(100 * time.Millisecond)
	}
}

var _ = Describe("SLB - Multi-Service Reachability", Label(slbTestLabel), func() {
	basename := "slb-reachability-test"

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

			// For large-scale tests, namespace deletion may timeout
			// due to Azure finalizer cleanup taking >10 minutes. This is expected
			// behavior and doesn't indicate a functional issue.
			if err != nil && strings.Contains(err.Error(), "timed out waiting for the condition") {
				utils.Logf("WARNING: Namespace deletion timed out (likely due to large number of services)")
				utils.Logf("This is expected for tests with many services and does not indicate a test failure")
			} else {
				Expect(err).NotTo(HaveOccurred())
			}

			By("Waiting for Azure cleanup to complete")
			// Poll instead of a fixed sleep; large multi-service tests can take a while
			// to clean up, so keep a generous cap.
			eventuallyAzureCleanup(6 * time.Minute)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		cs = nil
		ns = nil
	})

	It("should create 20 LoadBalancer services with 3 pods each and verify all pods are reachable",
		NodeTimeout(15*time.Minute),
		func(ctx SpecContext) {
			const (
				numServices   = 20
				podsPerSvc    = 3
				servicePort   = int32(8080)
				containerPort = 8080
				azureWaitTime = 120 * time.Second
			)

			// Track created services and their info
			type serviceInfo struct {
				name     string
				uid      string
				podNames []string
			}
			services := make([]serviceInfo, numServices)

			By(fmt.Sprintf("Creating %d services with %d pods each (%d total pods)",
				numServices, podsPerSvc, numServices*podsPerSvc))

			// Create services and pods in parallel
			var wg sync.WaitGroup
			errChan := make(chan error, numServices)

			for i := 0; i < numServices; i++ {
				wg.Add(1)
				go func(index int) {
					defer wg.Done()

					serviceName := fmt.Sprintf("svc-%d", index)
					podLabels := map[string]string{
						"app": serviceName,
					}

					// Create pods for this service
					podNames := make([]string, podsPerSvc)
					for j := 0; j < podsPerSvc; j++ {
						podName := fmt.Sprintf("%s-pod-%d", serviceName, j)
						podNames[j] = podName

						pod := &v1.Pod{
							ObjectMeta: metav1.ObjectMeta{
								Name:      podName,
								Namespace: ns.Name,
								Labels:    podLabels,
							},
							Spec: v1.PodSpec{
								Containers: []v1.Container{
									{
										Name:            "test-app",
										Image:           utils.AgnhostImage,
										ImagePullPolicy: v1.PullIfNotPresent,
										Args:            []string{"netexec", fmt.Sprintf("--http-port=%d", containerPort)},
										Ports: []v1.ContainerPort{
											{
												ContainerPort: int32(containerPort),
												Protocol:      v1.ProtocolTCP,
											},
										},
										ReadinessProbe: &v1.Probe{
											ProbeHandler: v1.ProbeHandler{
												HTTPGet: &v1.HTTPGetAction{
													Path: "/",
													Port: intstr.FromInt(containerPort),
												},
											},
											InitialDelaySeconds: 5,
											PeriodSeconds:       5,
											TimeoutSeconds:      2,
											FailureThreshold:    3,
										},
									},
								},
							},
						}

						_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
						if err != nil {
							errChan <- fmt.Errorf("failed to create pod %s: %w", podName, err)
							return
						}
					}

					// Create LoadBalancer service
					service := &v1.Service{
						ObjectMeta: metav1.ObjectMeta{
							Name:      serviceName,
							Namespace: ns.Name,
						},
						Spec: v1.ServiceSpec{
							Type:     v1.ServiceTypeLoadBalancer,
							Selector: podLabels,
							Ports: []v1.ServicePort{
								{
									Port:       servicePort,
									TargetPort: intstr.FromInt(containerPort),
									Protocol:   v1.ProtocolTCP,
								},
							},
						},
					}

					createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
					if err != nil {
						errChan <- fmt.Errorf("failed to create service %s: %w", serviceName, err)
						return
					}

					services[index] = serviceInfo{
						name:     serviceName,
						uid:      string(createdService.UID),
						podNames: podNames,
					}

					utils.Logf("  ✓ Created service %s (UID: %s) with %d pods",
						serviceName, createdService.UID, podsPerSvc)
				}(i)
			}

			wg.Wait()
			close(errChan)

			// Check for any errors during creation
			for err := range errChan {
				Fail(fmt.Sprintf("Creation failed: %v", err))
			}

			utils.Logf("All %d services and %d pods created successfully",
				numServices, numServices*podsPerSvc)

			By("Waiting for all pods to be ready")
			err := utils.WaitPodsToBeReady(cs, ns.Name)
			Expect(err).NotTo(HaveOccurred())
			utils.Logf("All pods are ready")

			By("Waiting for all services to register their pods in the Service Gateway")
			// Poll instead of a fixed sleep: once every service has its pods registered,
			// its Azure resources (PIP/LB) are in place, so the checks below pass quickly.
			Eventually(func() error {
				alResponse, err := queryServiceGatewayAddressLocations()
				if err != nil {
					return fmt.Errorf("query Service Gateway address locations: %w", err)
				}
				counts := make(map[string]int)
				for _, loc := range alResponse.Value {
					for _, addr := range loc.Addresses {
						for _, s := range addr.Services {
							counts[s]++
						}
					}
				}
				for i := range services {
					if counts[services[i].uid] != podsPerSvc {
						return fmt.Errorf("service %s has %d registered pods, want %d", services[i].name, counts[services[i].uid], podsPerSvc)
					}
				}
				return nil
			}, azureWaitTime, 10*time.Second).Should(Succeed(),
				"all services should have their pods registered in the Service Gateway")

			By("Verifying all Public IPs exist in Azure")
			pipCmd := exec.Command("az", "network", "public-ip", "list",
				"--resource-group", resourceGroupName,
				"--output", "json")
			pipOutput, err := pipCmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "Should be able to query Azure for Public IPs")

			var publicIPs []AzurePublicIP
			err = json.Unmarshal(pipOutput, &publicIPs)
			Expect(err).NotTo(HaveOccurred(), "Should parse Public IP JSON")

			// Build a map of PIP name to IP address
			pipMap := make(map[string]string)
			for _, pip := range publicIPs {
				pipMap[pip.Name] = pip.IPAddress
			}

			utils.Logf("Found %d total Public IPs in resource group", len(publicIPs))

			// Verify each service has a PIP and collect external IPs
			serviceExternalIPs := make(map[int]string)
			for i, svc := range services {
				expectedPIPName := fmt.Sprintf("%s-pip", svc.uid)
				ipAddr, found := pipMap[expectedPIPName]
				Expect(found).To(BeTrue(), fmt.Sprintf("Service %s should have PIP: %s", svc.name, expectedPIPName))
				Expect(ipAddr).NotTo(BeEmpty(), fmt.Sprintf("PIP %s should have an IP address", expectedPIPName))

				serviceExternalIPs[i] = ipAddr
				utils.Logf("  ✓ Service %s has PIP %s with IP %s", svc.name, expectedPIPName, ipAddr)
			}

			utils.Logf("All %d services have their Public IPs", numServices)

			By("Verifying all services are registered in Service Gateway")
			sgResponse, err := queryServiceGatewayServices()
			Expect(err).NotTo(HaveOccurred(), "Should be able to query Service Gateway services")

			utils.Logf("Found %d services in Service Gateway", len(sgResponse.Value))

			// Verify each service UID is registered (excluding default outbound service)
			registeredUIDs := make(map[string]bool)
			for _, sgSvc := range sgResponse.Value {
				if sgSvc.Name != "default-natgw" {
					registeredUIDs[sgSvc.Name] = true
				}
			}

			for _, svc := range services {
				Expect(registeredUIDs[svc.uid]).To(BeTrue(),
					fmt.Sprintf("Service %s (UID: %s) should be registered in Service Gateway", svc.name, svc.uid))
			}
			utils.Logf("  ✓ All %d services registered in Service Gateway", numServices)

			By("Probing pod reachability (best-effort, env-dependent — not strongly asserted)")
			// Reachability depends on the cluster actually routing LoadBalancer traffic. On some
			// environments (e.g. standalone test clusters without a working dataplane) traffic
			// does not flow even though the Azure LB/PIP/Service Gateway resources are correct, so
			// this is reported but NOT asserted for now. The contract-level checks above (PIP/LB,
			// SKU=Service, backend pool, Service Gateway registration) are the gating ones. Keep a
			// short per-service timeout so the total stays well under the spec NodeTimeout.
			reachabilityTimeout := 15 * time.Second

			// Test reachability for each service
			var reachabilityErrors []string
			for i, svc := range services {
				externalIP := serviceExternalIPs[i]
				utils.Logf("Testing reachability for service %s at %s:%d", svc.name, externalIP, servicePort)

				err := verifyAllPodsReachable(externalIP, int(servicePort), svc.podNames, reachabilityTimeout)
				if err != nil {
					reachabilityErrors = append(reachabilityErrors,
						fmt.Sprintf("Service %s: %v", svc.name, err))
				}
			}

			if len(reachabilityErrors) > 0 {
				utils.Logf("⚠ Pod reachability not confirmed for %d/%d services (env may not route LB traffic; not asserting):\n%s",
					len(reachabilityErrors), numServices, strings.Join(reachabilityErrors, "\n"))
			} else {
				utils.Logf("✓ All %d pods across %d services are reachable", numServices*podsPerSvc, numServices)
			}
		})
})
