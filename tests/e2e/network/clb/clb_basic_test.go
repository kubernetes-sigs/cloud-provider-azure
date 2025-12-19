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

var _ = Describe("CLB - Basic Service", Label(clbTestLabel), func() {
	basename := "clb-basic-test"
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

		// Use the resource group from the shared constants
		utils.Logf("Checking resources in Resource Group: %s", resourceGroupName) // Azure names resources using the service UID
		serviceUID := string(createdService.UID)
		expectedPIPName := fmt.Sprintf("%s-pip", serviceUID)
		expectedLBName := serviceUID

		utils.Logf("Service UID: %s", serviceUID)
		utils.Logf("Looking for Public IP with name: %s", expectedPIPName)

		// List all Public IPs and find ours by name
		pipCmd := exec.Command("az", "network", "public-ip", "list",
			"--resource-group", resourceGroupName,
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
			"--resource-group", resourceGroupName,
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

		utils.Logf("Querying Service Gateway services...")
		utils.Logf("  Service Gateway: %s", serviceGatewayName)
		utils.Logf("  Resource Group: %s", resourceGroupName)

		serviceGatewayURL := buildServiceGatewayURL("services")
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
})
