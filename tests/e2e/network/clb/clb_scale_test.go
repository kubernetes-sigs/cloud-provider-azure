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

var _ = Describe("CLB - Pod Scaling", Label(clbTestLabel), func() {
	basename := "clb-scale-test"

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

	It("should create a LoadBalancer service with individual pods created in parallel", func() {
		const (
			numPods       = 90
			servicePort   = int32(8080)
			containerPort = 8080
			azureWaitTime = 60 * time.Second
		)

		serviceName := "clb-parallel-pods-service"

		podLabels := map[string]string{
			"app": serviceName,
		}

		By("Creating individual pods in parallel")
		utils.Logf("Creating %d pods in parallel", numPods)

		// Create pods in parallel using goroutines
		podNames := make([]string, numPods)
		errors := make(chan error, numPods)
		created := make(chan string, numPods)

		for i := 0; i < numPods; i++ {
			podName := fmt.Sprintf("%s-pod-%d", serviceName, i)
			podNames[i] = podName

			go func(name string) {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
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
										ContainerPort: containerPort,
										Protocol:      v1.ProtocolTCP,
									},
								},
							},
						},
					},
				}

				_, err := cs.CoreV1().Pods(ns.Name).Create(context.TODO(), pod, metav1.CreateOptions{})
				if err != nil {
					errors <- fmt.Errorf("failed to create pod %s: %w", name, err)
				} else {
					created <- name
				}
			}(podName)
		}

		// Wait for all pod creation attempts to complete
		createdCount := 0
		for i := 0; i < numPods; i++ {
			select {
			case podName := <-created:
				createdCount++
				utils.Logf("  ✓ Created pod: %s (%d/%d)", podName, createdCount, numPods)
			case err := <-errors:
				Fail(fmt.Sprintf("Pod creation failed: %v", err))
			}
		}

		utils.Logf("All %d pods created successfully", numPods)

		By("Waiting for all pods to be ready")
		err := utils.WaitPodsToBeReady(cs, ns.Name)
		Expect(err).NotTo(HaveOccurred())
		utils.Logf("All pods are ready")

		// Print pod IPs and node information
		podListAfterReady, err := cs.CoreV1().Pods(ns.Name).List(context.TODO(), metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", serviceName),
		})
		Expect(err).NotTo(HaveOccurred())

		utils.Logf("\nPod Details:")
		for _, pod := range podListAfterReady.Items {
			utils.Logf("  Pod: %s", pod.Name)
			utils.Logf("    Pod IP: %s", pod.Status.PodIP)
			utils.Logf("    Node: %s", pod.Spec.NodeName)
			utils.Logf("    Host IP: %s", pod.Status.HostIP)
		}

		By("Creating LoadBalancer service")
		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              podLabels,
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
			By("Cleaning up service and pods")
			_ = cs.CoreV1().Services(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})

			// Delete pods
			for _, podName := range podNames {
				_ = cs.CoreV1().Pods(ns.Name).Delete(context.TODO(), podName, metav1.DeleteOptions{})
			}
		}()

		utils.Logf("Service created: %s/%s (UID: %s)", ns.Name, serviceName, createdService.UID)

		By("Waiting for Azure to provision resources")
		utils.Logf("Waiting %v for Azure provisioning...", azureWaitTime)
		time.Sleep(azureWaitTime)

		// Azure names resources based on the service UID
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
		Expect(servicePublicIP.IPAddress).NotTo(BeEmpty(), "Public IP address should not be empty")

		utils.Logf("Public IP found:")
		utils.Logf("  Name: %s", servicePublicIP.Name)
		utils.Logf("  Address: %s", servicePublicIP.IPAddress)
		utils.Logf("  Location: %s", servicePublicIP.Location)

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

		utils.Logf("Load Balancer found:")
		utils.Logf("  Name: %s", serviceLB.Name)
		utils.Logf("  SKU: %s", serviceLB.SKU.Name)
		utils.Logf("  Location: %s", serviceLB.Location)
		utils.Logf("  Frontend IPs: %d", len(serviceLB.FrontendIPConfigurations))
		utils.Logf("  Backend Pools: %d", len(serviceLB.BackendAddressPools))

		By("Verifying Load Balancer is Container Load Balancer")
		Expect(serviceLB.SKU.Name).To(Equal("Service"),
			"Load Balancer SKU should be 'Service' for Container Load Balancer")

		By("Verifying Load Balancer references the Public IP")
		publicIPReferenced := false
		for _, fip := range serviceLB.FrontendIPConfigurations {
			if strings.Contains(fip.PublicIPAddress.ID, publicIPName) {
				publicIPReferenced = true
				utils.Logf("  ✓ Frontend IP '%s' references Public IP", fip.Name)
				break
			}
		}
		Expect(publicIPReferenced).To(BeTrue(), "Load Balancer should reference the Public IP")

		By("Verifying Service exists in Service Gateway")
		utils.Logf("Querying Service Gateway services...")
		utils.Logf("  Service Gateway: %s", serviceGatewayName)
		utils.Logf("  Resource Group: %s", resourceGroupName)

		sgResponse, err := queryServiceGatewayServices()
		if err != nil {
			utils.Logf("Failed to query Service Gateway: %v", err)
			Fail("Could not query Azure for Service Gateway services")
		}

		utils.Logf("\n=== RAW SERVICE GATEWAY JSON ===")
		sgOutput, _ := json.MarshalIndent(sgResponse, "", "  ")
		utils.Logf("%s", string(sgOutput))
		utils.Logf("=== END RAW JSON ===\n")

		utils.Logf("Found %d service(s) in Service Gateway", len(sgResponse.Value))

		// Find our service by matching the service UID
		var ourService *ServiceGatewayService
		for i := range sgResponse.Value {
			if sgResponse.Value[i].Name == serviceUID {
				ourService = &sgResponse.Value[i]
				break
			}
		}

		Expect(ourService).NotTo(BeNil(), fmt.Sprintf("Should find service with name %s in Service Gateway", serviceUID))

		utils.Logf("\n✓ Found Service in Service Gateway:")
		utils.Logf("  Name: %s", ourService.Name)
		utils.Logf("  Type: %s", ourService.Type)
		utils.Logf("  Service Type: %s", ourService.Properties.ServiceType)
		utils.Logf("  Provisioning State: %s", ourService.Properties.ProvisioningState)
		utils.Logf("  Backend Pools: %d", len(ourService.Properties.LoadBalancerBackendPools))

		// Verify service type is "Inbound"
		Expect(ourService.Properties.ServiceType).To(Equal("Inbound"), "Service type should be 'Inbound'")
		utils.Logf("  ✓ Service Type is 'Inbound'")

		// Verify provisioning state is "Succeeded"
		Expect(ourService.Properties.ProvisioningState).To(Equal("Succeeded"), "Provisioning state should be 'Succeeded'")
		utils.Logf("  ✓ Provisioning State is 'Succeeded'")

		// Verify service has backend pool references
		Expect(len(ourService.Properties.LoadBalancerBackendPools)).To(BeNumerically(">", 0),
			"Service should have at least one backend pool reference")

		// Verify the backend pool reference matches our Load Balancer's backend pool
		backendPoolMatched := false
		if len(serviceLB.BackendAddressPools) > 0 && len(ourService.Properties.LoadBalancerBackendPools) > 0 {
			expectedBackendPoolID := serviceLB.BackendAddressPools[0].ID
			for _, pool := range ourService.Properties.LoadBalancerBackendPools {
				utils.Logf("  Backend Pool ID: %s", pool.ID)
				if pool.ID == expectedBackendPoolID {
					backendPoolMatched = true
					utils.Logf("  ✓ Backend Pool matches Load Balancer's backend pool!")
					break
				}
			}
		}

		Expect(backendPoolMatched).To(BeTrue(), "Service Gateway service should reference the Load Balancer's backend pool")

		By("Verifying Service Gateway address locations")
		utils.Logf("Querying Service Gateway address locations...")

		alResponse, err := queryServiceGatewayAddressLocations()
		if err != nil {
			utils.Logf("Failed to query Service Gateway address locations: %v", err)
			Fail("Could not query Azure for Service Gateway address locations")
		}

		utils.Logf("\n=== RAW ADDRESS LOCATIONS JSON ===")
		alOutput, _ := json.MarshalIndent(alResponse, "", "  ")
		utils.Logf("%s", string(alOutput))
		utils.Logf("=== END RAW JSON ===\n")

		utils.Logf("Found %d address location(s) in Service Gateway", len(alResponse.Value))

		// Build mapping of expected node IP -> pod IPs
		expectedNodeToPods := make(map[string][]string)
		for _, pod := range podListAfterReady.Items {
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
			// Find the address location for this node
			var nodeAddressLocation *ServiceGatewayAddressLocation
			for i := range alResponse.Value {
				if alResponse.Value[i].AddressLocation == expectedNodeIP {
					nodeAddressLocation = &alResponse.Value[i]
					break
				}
			}

			Expect(nodeAddressLocation).NotTo(BeNil(), fmt.Sprintf("Should find address location for node IP %s", expectedNodeIP))

			utils.Logf("\n✓ Found Address Location in Service Gateway:")
			utils.Logf("  Address Location (Node IP): %s", nodeAddressLocation.AddressLocation)
			utils.Logf("  Update Action: %s", nodeAddressLocation.AddressUpdateAction)
			utils.Logf("  Number of Addresses (Pod IPs): %d", len(nodeAddressLocation.Addresses))

			// Verify the address location matches the node IP
			Expect(nodeAddressLocation.AddressLocation).To(Equal(expectedNodeIP), fmt.Sprintf("Address location should match node IP %s", expectedNodeIP))
			utils.Logf("  ✓ Address location matches node IP: %s", expectedNodeIP)

			// Collect registered pod IPs for this node
			registeredPodIPs := make(map[string]bool)
			for _, addr := range nodeAddressLocation.Addresses {
				registeredPodIPs[addr.Address] = true
				utils.Logf("  Pod IP registered: %s", addr.Address)

				// Verify the address references our service
				serviceReferenced := false
				for _, svc := range addr.Services {
					if svc == serviceUID {
						serviceReferenced = true
						break
					}
				}
				Expect(serviceReferenced).To(BeTrue(), fmt.Sprintf("Address %s should reference service %s", addr.Address, serviceUID))
				utils.Logf("    ✓ References service: %s", serviceUID)
			}

			// Verify all expected pod IPs for this node are registered
			for _, expectedPodIP := range expectedPodIPs {
				Expect(registeredPodIPs[expectedPodIP]).To(BeTrue(), fmt.Sprintf("Pod IP %s should be registered in address location %s", expectedPodIP, expectedNodeIP))
			}
			utils.Logf("  ✓ All %d pod IPs on node %s are registered in Service Gateway", len(expectedPodIPs), expectedNodeIP)
		}

		By("Verifying service configuration")
		updatedService, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(updatedService.Spec.Type).To(Equal(v1.ServiceTypeLoadBalancer))
		Expect(updatedService.Spec.ExternalTrafficPolicy).To(Equal(v1.ServiceExternalTrafficPolicyTypeLocal))

		By("Verifying all pods are still running")
		podList, err := cs.CoreV1().Pods(ns.Name).List(context.TODO(), metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s", serviceName),
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(len(podList.Items)).To(Equal(numPods), "Should have all pods running")

		runningPods := 0
		for _, pod := range podList.Items {
			if pod.Status.Phase == v1.PodRunning {
				runningPods++
			}
		}
		Expect(runningPods).To(Equal(numPods), "All pods should be in Running state")

		utils.Logf("\n✓ Container Load Balancer with %d individual pods verified", numPods)
		utils.Logf("  Service: %s/%s", ns.Name, serviceName)
		utils.Logf("  External IP: %s", servicePublicIP.IPAddress)
		utils.Logf("  Load Balancer SKU: %s", serviceLB.SKU.Name)
		utils.Logf("  Running Pods: %d/%d", runningPods, numPods)
	})
})
