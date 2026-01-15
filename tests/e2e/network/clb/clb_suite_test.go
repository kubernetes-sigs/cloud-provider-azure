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
	"encoding/json"
	"fmt"
	"os/exec"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// Constants for CLB tests
const (
	clbTestLabel       = "CLB"
	subscriptionID     = "3dc13b6d-6896-40ac-98f7-f18cbce2a405"
	resourceGroupName  = "MC_enechitebld149025306_e2e-enechitoaia-67_eastus2euap"
	serviceGatewayName = "my-service-gateway"
	apiVersion         = "2025-01-01"
)

// AzurePublicIP represents a Public IP resource in Azure
type AzurePublicIP struct {
	Name      string            `json:"name"`
	IPAddress string            `json:"ipAddress"`
	Tags      map[string]string `json:"tags"`
	ID        string            `json:"id"`
	Location  string            `json:"location"`
}

// AzureLoadBalancer represents a Load Balancer resource in Azure
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

// ServiceGatewayServicesResponse represents the response from Service Gateway services API
type ServiceGatewayServicesResponse struct {
	Value []ServiceGatewayService `json:"value"`
}

// ServiceGatewayService represents a service in the Service Gateway
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

// ServiceGatewayAddressLocationsResponse represents the response from Service Gateway address locations API
type ServiceGatewayAddressLocationsResponse struct {
	Value []ServiceGatewayAddressLocation `json:"value"`
}

// ServiceGatewayAddressLocation represents an address location in the Service Gateway
type ServiceGatewayAddressLocation struct {
	AddressLocation     string    `json:"addressLocation"`
	AddressUpdateAction string    `json:"addressUpdateAction"`
	Addresses           []Address `json:"addresses"`
}

// Address represents an IP address and its associated services
type Address struct {
	Address  string   `json:"address"`
	Services []string `json:"services"`
}

// Helper functions for CLB tests

// buildServiceGatewayURL constructs the Service Gateway API URL for a given path
func buildServiceGatewayURL(path string) string {
	return fmt.Sprintf(
		"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/serviceGateways/%s/%s?api-version=%s",
		subscriptionID, resourceGroupName, serviceGatewayName, path, apiVersion,
	)
}

// queryServiceGatewayServices queries all services in the Service Gateway
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

// queryServiceGatewayAddressLocations queries all address locations in the Service Gateway
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

// verifyServiceGatewayCleanup verifies that only the default outbound service remains in the Service Gateway
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

// verifyAddressLocationsCleanup verifies that no addresses reference any services in the Service Gateway
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

// verifyNATGatewayCleanup verifies that test-created NAT Gateways are cleaned up
func verifyNATGatewayCleanup(egressNames []string) {
	if len(egressNames) == 0 {
		return // No egress gateways to verify
	}

	utils.Logf("Verifying NAT Gateway cleanup for %d egress gateway(s)", len(egressNames))

	sgResponse, err := queryServiceGatewayServices()
	Expect(err).NotTo(HaveOccurred(), "Should be able to query Service Gateway services")

	for _, egressName := range egressNames {
		found := false
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
				found = true
				Fail(fmt.Sprintf("Outbound service '%s' still exists in Service Gateway after cleanup", egressName))
			}
		}
		if !found {
			utils.Logf("  ✓ Outbound service '%s' cleaned up", egressName)
		}
	}
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

	// Verify Load Balancer has backend address pools
	if len(serviceLB.BackendAddressPools) == 0 {
		return fmt.Errorf("load Balancer %s has no backend address pools", loadBalancerName)
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
