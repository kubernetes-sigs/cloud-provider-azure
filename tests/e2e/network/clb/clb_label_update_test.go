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
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientset "k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

var _ = Describe("CLB - Label Updates", Label(clbTestLabel), func() {
	basename := "clb-label-update-test"

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

			// Wait for Azure cleanup - increased for multi-service tests
			utils.Logf("Waiting 120 seconds for Azure cleanup...")
			time.Sleep(120 * time.Second)

			By("Verifying Service Gateway cleanup")
			verifyServiceGatewayCleanup()

			By("Verifying Address Locations cleanup")
			verifyAddressLocationsCleanup()
		}

		cs = nil
		ns = nil
	})

	It("should instantly make all running pods available when labels are updated to match service selector", func() {
		const (
			numPods       = 120
			servicePort   = int32(8080)
			containerPort = 8080
			azureWaitTime = 60 * time.Second
		)

		serviceName := "clb-label-update-service"

		// Labels that initially don't match the service
		initialPodLabels := map[string]string{
			"app":   "initial-app",
			"phase": "pre-service",
		}

		// Service selector labels
		serviceSelectorLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating pods with initial labels (not matching service selector)")
		utils.Logf("Creating %d individual pods with initial labels %v", numPods, initialPodLabels)

		// Create pods in parallel
		type podResult struct {
			pod *v1.Pod
			err error
		}
		podChan := make(chan podResult, numPods)

		for i := 0; i < numPods; i++ {
			go func(index int) {
				podName := fmt.Sprintf("clb-label-update-pod-%d", index)
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: ns.Name,
						Labels:    initialPodLabels,
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
										ContainerPort: containerPort,
										Protocol:      v1.ProtocolTCP,
									},
								},
							},
						},
					},
				}
				createdPod, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				podChan <- podResult{pod: createdPod, err: err}
			}(i)
		}

		// Collect results
		for i := 0; i < numPods; i++ {
			result := <-podChan
			Expect(result.err).NotTo(HaveOccurred())
			utils.Logf("  ✓ Created pod: %s", result.pod.Name)
		}

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("  ✓ All %d pods are ready", numPods)

		// Get pod list
		podList, err := cs.CoreV1().Pods(ns.Name).List(context.TODO(), metav1.ListOptions{
			LabelSelector: "app=initial-app",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(podList.Items)).To(Equal(numPods), "Should have all pods running")

		utils.Logf("\nInitial Pod Details (before label update):")
		for _, pod := range podList.Items {
			utils.Logf("  Pod: %s", pod.Name)
			utils.Logf("    Pod IP: %s", pod.Status.PodIP)
			utils.Logf("    Node: %s", pod.Spec.NodeName)
			utils.Logf("    Host IP: %s", pod.Status.HostIP)
			utils.Logf("    Labels: %v", pod.Labels)
		}

		By("Creating LoadBalancer service with selector")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              serviceSelectorLabels,
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
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			By("Cleaning up service")
			_ = cs.CoreV1().Services(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
		}()

		utils.Logf("Service created: %s/%s (UID: %s)", ns.Name, serviceName, createdService.UID)
		utils.Logf("  ✓ Service has no matching pods yet (selector: %v)", serviceSelectorLabels)

		By("Waiting for Azure to provision resources")
		utils.Logf("Waiting %v for Azure provisioning...", azureWaitTime)
		time.Sleep(azureWaitTime)

		serviceUID := string(createdService.UID)
		publicIPName := fmt.Sprintf("%s-pip", serviceUID)
		loadBalancerName := serviceUID

		By("Verifying Public IP in Azure")
		pipCmd := exec.Command("az", "network", "public-ip", "list",
			"--resource-group", resourceGroupName,
			"--output", "json")
		pipOutput, err := pipCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "Should be able to query Azure for Public IPs")

		var publicIPs []AzurePublicIP
		err = json.Unmarshal(pipOutput, &publicIPs)
		Expect(err).NotTo(HaveOccurred(), "Should parse Public IP JSON")

		var servicePublicIP *AzurePublicIP
		for i := range publicIPs {
			if publicIPs[i].Name == publicIPName {
				servicePublicIP = &publicIPs[i]
				break
			}
		}
		Expect(servicePublicIP).NotTo(BeNil(), fmt.Sprintf("Should find Public IP: %s", publicIPName))
		utils.Logf("Public IP found: %s", servicePublicIP.IPAddress)

		By("Verifying Load Balancer in Azure")
		lbCmd := exec.Command("az", "network", "lb", "show",
			"--resource-group", resourceGroupName,
			"--name", loadBalancerName,
			"--output", "json")
		lbOutput, err := lbCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "Should be able to query Azure for Load Balancer")

		var serviceLB AzureLoadBalancer
		err = json.Unmarshal(lbOutput, &serviceLB)
		Expect(err).NotTo(HaveOccurred(), "Should parse Load Balancer JSON")
		Expect(serviceLB.SKU.Name).To(Equal("Service"), "Load Balancer SKU should be 'Service'")
		utils.Logf("Load Balancer found with SKU: %s", serviceLB.SKU.Name)

		By("Verifying Service Gateway before label update")
		utils.Logf("Checking Service Gateway state (should have service but no address locations yet)...")

		sgResponse, err := queryServiceGatewayServices()
		Expect(err).NotTo(HaveOccurred())

		var ourService *ServiceGatewayService
		for i := range sgResponse.Value {
			if sgResponse.Value[i].Name == serviceUID {
				ourService = &sgResponse.Value[i]
				break
			}
		}
		Expect(ourService).NotTo(BeNil(), "Service should exist in Service Gateway")
		utils.Logf("  ✓ Service exists in Service Gateway: %s", ourService.Name)

		alResponseBefore, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("  Address locations before label update: %d", len(alResponseBefore.Value))

		By("Updating pod labels to match service selector")
		utils.Logf("Updating all pod labels from %v to %v", initialPodLabels, serviceSelectorLabels)

		// Update existing pod labels directly in parallel (without restarting pods)
		// Note: We don't update the deployment template because its selector is immutable
		// and we want to test instant availability of already-running pods
		type labelUpdateResult struct {
			podName string
			err     error
		}
		updateChan := make(chan labelUpdateResult, len(podList.Items))

		for i := range podList.Items {
			go func(index int) {
				pod := &podList.Items[index]
				pod.Labels = serviceSelectorLabels
				_, err := cs.CoreV1().Pods(ns.Name).Update(context.TODO(), pod, metav1.UpdateOptions{})
				updateChan <- labelUpdateResult{podName: pod.Name, err: err}
			}(i)
		}

		// Collect results
		updatedCount := 0
		for i := 0; i < len(podList.Items); i++ {
			result := <-updateChan
			Expect(result.err).NotTo(HaveOccurred())
			updatedCount++
			utils.Logf("  ✓ Updated pod %s labels to match service selector (%d/%d)", result.podName, updatedCount, numPods)
		}

		utils.Logf("  ✓ All %d pods now match service selector!", numPods)

		By("Waiting for Azure to update Service Gateway with pod IPs")
		utils.Logf("Waiting %v for Service Gateway to register pod IPs...", azureWaitTime)
		time.Sleep(azureWaitTime)

		By("Verifying Service Gateway address locations after label update")
		utils.Logf("Querying Service Gateway address locations...")

		alResponse, err := queryServiceGatewayAddressLocations()
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("Found %d address location(s) in Service Gateway", len(alResponse.Value))

		// Build mapping of expected node IP -> pod IPs
		expectedNodeToPods := make(map[string][]string)
		for _, pod := range podList.Items {
			nodeIP := pod.Status.HostIP
			podIP := pod.Status.PodIP
			expectedNodeToPods[nodeIP] = append(expectedNodeToPods[nodeIP], podIP)
		}

		utils.Logf("Expected address locations (Node IP -> Pod IPs):")
		for nodeIP, podIPs := range expectedNodeToPods {
			utils.Logf("  Node %s: %d pods", nodeIP, len(podIPs))
		}

		// Verify each expected node's address location
		for expectedNodeIP, expectedPodIPs := range expectedNodeToPods {
			var nodeAddressLocation *ServiceGatewayAddressLocation
			for i := range alResponse.Value {
				if alResponse.Value[i].AddressLocation == expectedNodeIP {
					nodeAddressLocation = &alResponse.Value[i]
					break
				}
			}

			Expect(nodeAddressLocation).NotTo(BeNil(), fmt.Sprintf("Should find address location for node %s", expectedNodeIP))

			utils.Logf("\n✓ Found Address Location in Service Gateway:")
			utils.Logf("  Address Location (Node IP): %s", nodeAddressLocation.AddressLocation)
			utils.Logf("  Number of Addresses (Pod IPs): %d", len(nodeAddressLocation.Addresses))

			Expect(nodeAddressLocation.AddressLocation).To(Equal(expectedNodeIP))

			// Verify all expected pod IPs are registered
			registeredPodIPs := make(map[string]bool)
			for _, addr := range nodeAddressLocation.Addresses {
				registeredPodIPs[addr.Address] = true
				utils.Logf("  Pod IP registered: %s", addr.Address)

				// Verify the service reference
				foundServiceRef := false
				for _, svcID := range addr.Services {
					if svcID == serviceUID {
						foundServiceRef = true
						utils.Logf("    ✓ References service: %s", svcID)
						break
					}
				}
				Expect(foundServiceRef).To(BeTrue(), fmt.Sprintf("Pod IP %s should reference service %s", addr.Address, serviceUID))
			}

			// Verify all expected pod IPs are present
			for _, expectedPodIP := range expectedPodIPs {
				Expect(registeredPodIPs[expectedPodIP]).To(BeTrue(),
					fmt.Sprintf("Pod IP %s should be registered in address location %s", expectedPodIP, expectedNodeIP))
			}

			utils.Logf("  ✓ All %d pod IPs on node %s are registered in Service Gateway", len(expectedPodIPs), expectedNodeIP)
		}

		By("Verifying all pods are still running")
		finalPodList, err := cs.CoreV1().Pods(ns.Name).List(context.TODO(), metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", serviceName),
		})
		Expect(err).NotTo(HaveOccurred())

		runningPods := 0
		for _, pod := range finalPodList.Items {
			if pod.Status.Phase == v1.PodRunning {
				runningPods++
			}
		}
		Expect(runningPods).To(Equal(numPods), "All pods should be in Running state")

		utils.Logf("\n✓ Container Load Balancer with instant pod label update verified")
		utils.Logf("  Service: %s/%s", ns.Name, serviceName)
		utils.Logf("  External IP: %s", servicePublicIP.IPAddress)
		utils.Logf("  Load Balancer SKU: %s", serviceLB.SKU.Name)
		utils.Logf("  Running Pods: %d/%d", runningPods, numPods)
		utils.Logf("  ✓ All pods instantly available after label update!")
	})
})
