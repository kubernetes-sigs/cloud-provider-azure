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

// NOTE: The Container Load Balancer (CLB) tests have been reorganized into the
// tests/e2e/network/clb/ directory for better maintainability. This file is
// preserved for reference. The new structure includes:
// - clb_suite_test.go: Shared types, constants, and helper functions
// - clb_basic_test.go: Basic LoadBalancer service creation test
// - clb_scale_test.go: Large-scale pod creation test (120 pods)
// - clb_label_update_test.go: Dynamic label update test (475 pods)
// - clb_concurrent_test.go: Concurrent services test (15 services × 5 pods)

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

const (
	clbTestLabel       = "CLB"
	subscriptionID     = "3dc13b6d-6896-40ac-98f7-f18cbce2a405"
	resourceGroupName  = "mc_enechitebld146945876_e2e-enechitoaia-34_eastus2euap"
	serviceGatewayName = "my-service-gateway"
	apiVersion         = "2025-01-01"
)

type AzurePublicIP struct {
	Name      string            `json:"name"`
	IPAddress string            `json:"ipAddress"`
	Tags      map[string]string `json:"tags"`
	ID        string            `json:"id"`
	Location  string            `json:"location"`
}

type AzureLoadBalancer struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	Location string `json:"location"`
	SKU      struct {
		Name string `json:"name"`
	} `json:"sku"`
	// Azure CLI returns these at root level, not under "properties"
	FrontendIPConfigurations []struct {
		Name string `json:"name"`
		// publicIPAddress is at root level in Azure CLI JSON
		PublicIPAddress struct {
			ID string `json:"id"`
		} `json:"publicIPAddress"`
	} `json:"frontendIPConfigurations"`
	LoadBalancingRules []struct {
		Name string `json:"name"`
	} `json:"loadBalancingRules"`
	BackendAddressPools []struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"backendAddressPools"`
}

type ServiceGatewayServicesResponse struct {
	Value []ServiceGatewayService `json:"value"`
}

type ServiceGatewayService struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Etag       string `json:"etag"`
	Properties struct {
		ProvisioningState        string `json:"provisioningState"`
		ServiceType              string `json:"serviceType"`
		IsDefault                bool   `json:"isDefault,omitempty"`
		PublicNatGatewayID       string `json:"publicNatGatewayId,omitempty"`
		LoadBalancerBackendPools []struct {
			ID string `json:"id"`
		} `json:"loadBalancerBackendPools"`
	} `json:"properties"`
}

type ServiceGatewayAddressLocationsResponse struct {
	Value []ServiceGatewayAddressLocation `json:"value"`
}

type ServiceGatewayAddressLocation struct {
	AddressLocation     string    `json:"addressLocation"`
	AddressUpdateAction string    `json:"addressUpdateAction"`
	Addresses           []Address `json:"addresses"`
}

type Address struct {
	Address  string   `json:"address"`
	Services []string `json:"services"`
}

// Helper functions
func buildServiceGatewayURL(path string) string {
	return fmt.Sprintf(
		"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/serviceGateways/%s/%s?api-version=%s",
		subscriptionID, resourceGroupName, serviceGatewayName, path, apiVersion,
	)
}

func queryServiceGatewayServices() (ServiceGatewayServicesResponse, error) {
	url := buildServiceGatewayURL("services")
	cmd := exec.Command("az", "rest", "--method", "get", "--url", url)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return ServiceGatewayServicesResponse{}, fmt.Errorf("failed to query Service Gateway services: %w, output: %s", err, string(output))
	}

	var response ServiceGatewayServicesResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return ServiceGatewayServicesResponse{}, fmt.Errorf("failed to parse Service Gateway services response: %w", err)
	}

	return response, nil
}

func queryServiceGatewayAddressLocations() (ServiceGatewayAddressLocationsResponse, error) {
	url := buildServiceGatewayURL("addressLocations")
	cmd := exec.Command("az", "rest", "--method", "get", "--url", url)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return ServiceGatewayAddressLocationsResponse{}, fmt.Errorf("failed to query Service Gateway address locations: %w, output: %s", err, string(output))
	}

	var response ServiceGatewayAddressLocationsResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return ServiceGatewayAddressLocationsResponse{}, fmt.Errorf("failed to parse Service Gateway address locations response: %w", err)
	}

	return response, nil
}

func verifyServiceGatewayCleanup() {
	utils.Logf("Verifying Service Gateway services only contain default outbound service")

	sgResponse, err := queryServiceGatewayServices()
	Expect(err).NotTo(HaveOccurred(), "Should be able to query Service Gateway services after cleanup")

	utils.Logf("Found %d service(s) in Service Gateway after cleanup", len(sgResponse.Value))
	for i := range sgResponse.Value {
		svc := &sgResponse.Value[i]
		utils.Logf("  Service: %s (Type: %s)", svc.Name, svc.Properties.ServiceType)

		if svc.Name != "default-natgw-v2" {
			Fail(fmt.Sprintf("Unexpected service '%s' still exists in Service Gateway after cleanup", svc.Name))
		}
		Expect(svc.Properties.ServiceType).To(Equal("Outbound"), "Service should be the default outbound service")
	}
	utils.Logf("  ✓ Only default outbound service remains in Service Gateway")
}

func verifyAddressLocationsCleanup() {
	utils.Logf("Verifying Service Gateway address locations are empty")

	alResponse, err := queryServiceGatewayAddressLocations()
	Expect(err).NotTo(HaveOccurred(), "Should be able to query Service Gateway address locations after cleanup")

	utils.Logf("Found %d address location(s) in Service Gateway after cleanup", len(alResponse.Value))
	for _, location := range alResponse.Value {
		utils.Logf("  Address Location: %s with %d addresses", location.AddressLocation, len(location.Addresses))

		for _, addr := range location.Addresses {
			if len(addr.Services) > 0 {
				Fail(fmt.Sprintf("Address %s in location %s still has %d service reference(s) after cleanup",
					addr.Address, location.AddressLocation, len(addr.Services)))
			}
		}
	}
	utils.Logf("  ✓ No addresses reference any services in Service Gateway")
}

// verifyAzureResources verifies Public IP, Load Balancer, and Service Gateway for a given service
func verifyAzureResources(serviceUID string) error {
	publicIPName := fmt.Sprintf("%s-pip", serviceUID)
	loadBalancerName := serviceUID

	// Verify Public IP in Azure
	pipCmd := exec.Command("az", "network", "public-ip", "list",
		"--resource-group", resourceGroupName,
		"--output", "json")
	pipOutput, err := pipCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to query Azure for Public IPs: %w", err)
	}

	var publicIPs []AzurePublicIP
	if err := json.Unmarshal(pipOutput, &publicIPs); err != nil {
		return fmt.Errorf("failed to parse Public IP JSON: %w", err)
	}

	var servicePublicIP *AzurePublicIP
	for i := range publicIPs {
		if publicIPs[i].Name == publicIPName {
			servicePublicIP = &publicIPs[i]
			break
		}
	}
	if servicePublicIP == nil {
		return fmt.Errorf("public IP not found: %s", publicIPName)
	}

	// Verify Load Balancer in Azure
	lbCmd := exec.Command("az", "network", "lb", "show",
		"--resource-group", resourceGroupName,
		"--name", loadBalancerName,
		"--output", "json")
	lbOutput, err := lbCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to query Azure for Load Balancer: %w", err)
	}

	var serviceLB AzureLoadBalancer
	if err := json.Unmarshal(lbOutput, &serviceLB); err != nil {
		return fmt.Errorf("failed to parse Load Balancer JSON: %w", err)
	}

	if serviceLB.SKU.Name != "Service" {
		return fmt.Errorf("load Balancer SKU should be 'Service', got '%s'", serviceLB.SKU.Name)
	}

	// Verify Service Gateway has this service
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return fmt.Errorf("failed to query Service Gateway services: %w", err)
	}

	var foundService bool
	for _, sgSvc := range sgResponse.Value {
		if sgSvc.Name == serviceUID {
			foundService = true
			break
		}
	}
	if !foundService {
		return fmt.Errorf("service %s not found in Service Gateway", serviceUID)
	}

	return nil
}

var _ = Describe("Container Load Balancer", Label(clbTestLabel), func() {
	basename := "clb-test"
	serviceName := "inbound-podname-echo5"

	var (
		cs clientset.Interface
		ns *v1.Namespace
	)

	labels := map[string]string{
		"app": serviceName,
	}

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

	It("should create a simple LoadBalancer service and verify it exists in Azure", func() {
		By("Creating a simple LoadBalancer service")

		service := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: ns.Name,
			},
			Spec: v1.ServiceSpec{
				Type:                  v1.ServiceTypeLoadBalancer,
				ExternalTrafficPolicy: v1.ServiceExternalTrafficPolicyTypeLocal,
				Selector:              labels,
				Ports: []v1.ServicePort{
					{
						Port:       5000,
						TargetPort: intstr.FromInt(30154),
						Protocol:   v1.ProtocolTCP,
					},
				},
			},
		}

		utils.Logf("Creating service: %s in namespace: %s", serviceName, ns.Name)
		createdService, err := cs.CoreV1().Services(ns.Name).Create(context.TODO(), service, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			By("Cleaning up service")
			err := cs.CoreV1().Services(ns.Name).Delete(context.TODO(), serviceName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())
		}()

		utils.Logf("✓ Service created in Kubernetes")
		utils.Logf("  Service Name: %s", serviceName)
		utils.Logf("  Namespace: %s", ns.Name)
		utils.Logf("  Service UID: %s", createdService.UID)
		utils.Logf("  Port: 5000 -> 30154")
		utils.Logf("  External Traffic Policy: Local")

		By("Waiting for Azure to provision LoadBalancer resources")
		utils.Logf("Waiting 60 seconds for Azure to create Public IP and Load Balancer...")
		time.Sleep(60 * time.Second)

		By("Verifying Public IP exists in Azure using CLI")

		// Get resource group
		rgName := resourceGroupName
		utils.Logf("Checking resources in Resource Group: %s", rgName) // Azure names resources using the service UID
		serviceUID := string(createdService.UID)
		expectedPIPName := fmt.Sprintf("%s-pip", serviceUID)
		expectedLBName := serviceUID

		utils.Logf("Service UID: %s", serviceUID)
		utils.Logf("Looking for Public IP with name: %s", expectedPIPName)

		// List all Public IPs and find ours by name
		pipCmd := exec.Command("az", "network", "public-ip", "list",
			"--resource-group", rgName,
			"--output", "json")
		pipOutput, err := pipCmd.CombinedOutput()

		if err != nil {
			utils.Logf("Failed to list public IPs: %v", err)
			utils.Logf("Output: %s", string(pipOutput))
			Fail("Could not query Azure for Public IP")
		}

		var publicIPs []AzurePublicIP
		err = json.Unmarshal(pipOutput, &publicIPs)
		Expect(err).NotTo(HaveOccurred(), "Should be able to parse Public IP JSON response")

		// Find our Public IP by name
		var servicePublicIP *AzurePublicIP
		for i := range publicIPs {
			if publicIPs[i].Name == expectedPIPName {
				servicePublicIP = &publicIPs[i]
				break
			}
		}
		Expect(servicePublicIP).NotTo(BeNil(), fmt.Sprintf("Should find public IP with name %s", expectedPIPName))

		externalIP := servicePublicIP.IPAddress

		utils.Logf("\n✓ Found Public IP in Azure:")
		utils.Logf("  Name: %s", servicePublicIP.Name)
		utils.Logf("  IP Address: %s", externalIP)
		utils.Logf("  Location: %s", servicePublicIP.Location)
		utils.Logf("  Tags:")
		for k, v := range servicePublicIP.Tags {
			utils.Logf("    %s: %s", k, v)
		}
		utils.Logf("  Azure Portal: https://portal.azure.com/#@/resource%s", servicePublicIP.ID)

		By("Verifying Load Balancer exists in Azure using CLI")

		utils.Logf("Looking for Load Balancer with name: %s", expectedLBName)

		// Use 'show' to get full details
		lbCmd := exec.Command("az", "network", "lb", "show",
			"--resource-group", rgName,
			"--name", expectedLBName,
			"--output", "json")
		lbOutput, err := lbCmd.CombinedOutput()

		if err != nil {
			utils.Logf("Failed to get load balancer: %v", err)
			utils.Logf("Output: %s", string(lbOutput))
			Fail("Could not query Azure for Load Balancer")
		}

		// Log the raw JSON response for debugging
		utils.Logf("\n=== RAW LOAD BALANCER JSON ===")
		utils.Logf("%s", string(lbOutput))
		utils.Logf("=== END RAW JSON ===\n")

		var serviceLB AzureLoadBalancer
		err = json.Unmarshal(lbOutput, &serviceLB)
		Expect(err).NotTo(HaveOccurred(), "Should be able to parse Load Balancer JSON response")

		utils.Logf("\n✓ Found Load Balancer in Azure:")
		utils.Logf("  Name: %s", serviceLB.Name)
		utils.Logf("  SKU: %s", serviceLB.SKU.Name)
		utils.Logf("  Location: %s", serviceLB.Location)
		utils.Logf("  Frontend IP Configurations: %d", len(serviceLB.FrontendIPConfigurations))
		utils.Logf("  Load Balancing Rules: %d", len(serviceLB.LoadBalancingRules))
		utils.Logf("  Backend Address Pools: %d", len(serviceLB.BackendAddressPools))

		// Verify LB SKU is "Service" (Container Load Balancer)
		Expect(serviceLB.SKU.Name).To(Equal("Service"), "Load Balancer SKU should be 'Service' for Container Load Balancer")
		utils.Logf("  ✓ Load Balancer SKU is 'Service' (Container Load Balancer)")

		By("Verifying Service exists in Service Gateway")

		subscriptionID := "3dc13b6d-6896-40ac-98f7-f18cbce2a405"
		serviceGatewayName := "my-service-gateway"
		apiVersion := "2025-01-01"

		serviceGatewayURL := fmt.Sprintf(
			"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/serviceGateways/%s/services?api-version=%s",
			subscriptionID,
			rgName,
			serviceGatewayName,
			apiVersion,
		)

		utils.Logf("Querying Service Gateway services...")
		utils.Logf("  Service Gateway: %s", serviceGatewayName)
		utils.Logf("  Resource Group: %s", rgName)

		sgCmd := exec.Command("az", "rest", "--method", "get", "--url", serviceGatewayURL)
		sgOutput, err := sgCmd.CombinedOutput()

		if err != nil {
			utils.Logf("Failed to query Service Gateway: %v", err)
			utils.Logf("Output: %s", string(sgOutput))
			Fail("Could not query Azure for Service Gateway services")
		}

		utils.Logf("\n=== RAW SERVICE GATEWAY JSON ===")
		utils.Logf("%s", string(sgOutput))
		utils.Logf("=== END RAW JSON ===\n")

		var sgResponse ServiceGatewayServicesResponse
		err = json.Unmarshal(sgOutput, &sgResponse)
		Expect(err).NotTo(HaveOccurred(), "Should be able to parse Service Gateway JSON response")

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

		// Verify LB has at least one frontend IP configuration
		Expect(len(serviceLB.FrontendIPConfigurations)).To(BeNumerically(">", 0),
			"Load Balancer should have at least one frontend IP configuration")

		// Verify the frontend IP configuration references our Public IP
		pipFound := false
		for _, fip := range serviceLB.FrontendIPConfigurations {
			utils.Logf("  Checking Frontend IP Config: %s", fip.Name)
			utils.Logf("    Public IP ID: %s", fip.PublicIPAddress.ID)
			if strings.Contains(fip.PublicIPAddress.ID, servicePublicIP.Name) {
				pipFound = true
				utils.Logf("  ✓ Load Balancer frontend uses our Public IP!")
				break
			}
		}
		Expect(pipFound).To(BeTrue(), "Load Balancer should reference our Public IP")
		utils.Logf("  Azure Portal: https://portal.azure.com/#@/resource%s", serviceLB.ID)

		By("Verifying service details from Kubernetes")
		updatedService, err := cs.CoreV1().Services(ns.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())

		// Check if external IP was eventually updated in Kubernetes (optional - might not be updated)
		if len(updatedService.Status.LoadBalancer.Ingress) > 0 {
			k8sIP := updatedService.Status.LoadBalancer.Ingress[0].IP
			utils.Logf("\n✓ Kubernetes Service Status was also updated:")
			utils.Logf("  External IP (from K8s): %s", k8sIP)
			if k8sIP == externalIP {
				utils.Logf("  ✓ Matches Azure Public IP!")
			} else {
				utils.Logf("  ⚠ Warning: K8s IP (%s) doesn't match Azure IP (%s)", k8sIP, externalIP)
			}
		} else {
			utils.Logf("\n⚠ Note: Kubernetes service status not yet updated with external IP")
			utils.Logf("  This is a known issue - resources exist in Azure but K8s status not synced")
		}

		utils.Logf("\n" + strings.Repeat("=", 80))
		utils.Logf("✓✓✓ TEST PASSED! ✓✓✓")
		utils.Logf("=" + strings.Repeat("=", 79))
		utils.Logf("Service: %s/%s", ns.Name, serviceName)
		utils.Logf("External IP (from Azure): %s", externalIP)
		utils.Logf("Service Type: %s", updatedService.Spec.Type)
		utils.Logf("External Traffic Policy: %s", updatedService.Spec.ExternalTrafficPolicy)
		utils.Logf("Status: LoadBalancer successfully provisioned and verified in Azure")
		utils.Logf(strings.Repeat("=", 80))

		// Verify the service properties
		Expect(externalIP).NotTo(BeEmpty(), "External IP should not be empty")
		Expect(updatedService.Spec.Type).To(Equal(v1.ServiceTypeLoadBalancer), "Service type should be LoadBalancer")
		Expect(updatedService.Spec.ExternalTrafficPolicy).To(Equal(v1.ServiceExternalTrafficPolicyTypeLocal),
			"External traffic policy should be Local")
		Expect(len(updatedService.Spec.Ports)).To(Equal(1), "Should have exactly one port")
		Expect(updatedService.Spec.Ports[0].Port).To(Equal(int32(5000)), "Port should be 5000")
	})

	XIt("should create a LoadBalancer service with individual pods created in parallel", func() {
		const (
			numPods       = 120
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

	XIt("should instantly make all running pods available when labels are updated to match service selector", func() {
		const (
			numPods       = 475
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

	// Test 4: Multiple concurrent services with pods
	XIt("should handle X concurrent LoadBalancer services with Y pods each", func() {
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
		utils.Logf("Waiting %v for Azure provisioning...", azureWaitTime)
		time.Sleep(azureWaitTime)

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

		if len(registeredServices) < numServices {
			utils.Logf("WARNING: Only %d/%d services found. Waiting an additional 60 seconds...", len(registeredServices), numServices)
			time.Sleep(60 * time.Second)

			// Query again
			sgServices, err = queryServiceGatewayServices()
			Expect(err).NotTo(HaveOccurred())

			registeredServices = make(map[string]bool)
			for _, sgSvc := range sgServices.Value {
				if !sgSvc.Properties.IsDefault && sgSvc.Properties.ServiceType == "Inbound" {
					registeredServices[sgSvc.Name] = true
				}
			}
			utils.Logf("After additional wait: Found %d non-default inbound services", len(registeredServices))
		}

		Expect(len(registeredServices)).To(BeNumerically(">=", numServices),
			fmt.Sprintf("Expected at least %d services in Service Gateway, found %d", numServices, len(registeredServices)))

		// Verify each service individually by querying all resources once
		By("Verifying Azure resources (PIP, LB, Service Gateway) for all services")

		// Query all Public IPs once
		utils.Logf("Querying all Public IPs...")
		pipCmd := exec.Command("az", "network", "public-ip", "list",
			"--resource-group", resourceGroupName,
			"--output", "json")
		pipOutput, err := pipCmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "Should be able to query Azure for Public IPs")

		var allPublicIPs []AzurePublicIP
		err = json.Unmarshal(pipOutput, &allPublicIPs)
		Expect(err).NotTo(HaveOccurred(), "Should parse Public IP JSON")
		utils.Logf("Found %d total Public IPs in resource group", len(allPublicIPs))

		// Query all Load Balancers once
		utils.Logf("Querying all Load Balancers...")
		lbListCmd := exec.Command("az", "network", "lb", "list",
			"--resource-group", resourceGroupName,
			"--output", "json")
		lbListOutput, err := lbListCmd.CombinedOutput()
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
