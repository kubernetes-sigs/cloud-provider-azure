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
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/tests/e2e/utils"
)

// Environment variable names for SLB configuration
const (
	slbTestLabel = "SLB"

	// Environment variable names
	envServiceGatewayName       = "AZURE_SERVICE_GATEWAY_NAME"
	envServiceGatewayAPIVersion = "AZURE_SERVICE_GATEWAY_API_VERSION"

	// Default values.
	// NOTE: as of commit 33dcfb27 the SGW resource name is hardcoded in the cloud
	// provider to consts.DefaultServiceGatewayResourceName ("servicegateway").
	// Override via AZURE_SERVICE_GATEWAY_NAME only if your cluster predates that change.
	defaultServiceGatewayName = "servicegateway"
	defaultAPIVersion         = "2025-01-01"
)

// Package-level variables populated from environment or AzureTestClient
var (
	subscriptionID     string
	resourceGroupName  string
	serviceGatewayName string
	apiVersion         string
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
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Location string            `json:"location"`
	Tags     map[string]string `json:"tags"`
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

// Helper functions for SLB tests

func inboundServiceName(serviceUID string) string {
	return strings.ToLower(serviceUID) + "-lb"
}

func outboundServiceName(egressName string) string {
	return strings.ToLower(egressName) + "-natgw"
}

func TestServiceGatewayCanonicalNames(t *testing.T) {
	for _, tc := range []struct {
		raw, inbound, outbound string
	}{
		{"same", "same-lb", "same-natgw"},
		{"Mixed", "mixed-lb", "mixed-natgw"},
		{"name-lb", "name-lb-lb", "name-lb-natgw"},
		{"name-natgw", "name-natgw-lb", "name-natgw-natgw"},
		{"default", "default-lb", "default-natgw"},
		{"default-natgw", "default-natgw-lb", "default-natgw-natgw"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := inboundServiceName(tc.raw); got != tc.inbound {
				t.Errorf("inboundServiceName(%q) = %q, want %q", tc.raw, got, tc.inbound)
			}
			if got := outboundServiceName(tc.raw); got != tc.outbound {
				t.Errorf("outboundServiceName(%q) = %q, want %q", tc.raw, got, tc.outbound)
			}
			if got := PublicIPNameForEgress(tc.raw); got != tc.outbound+"-pip" {
				t.Errorf("PublicIPNameForEgress(%q) = %q, want %q", tc.raw, got, tc.outbound+"-pip")
			}
		})
	}
}

// ensureSLBConfigInitialized ensures the SLB config is initialized.
// This should be called before using any SLB config variables.
func ensureSLBConfigInitialized() {
	// Try to get subscription and resource group from environment first
	if subscriptionID == "" {
		subscriptionID = os.Getenv("AZURE_SUBSCRIPTION_ID")
	}
	if resourceGroupName == "" {
		resourceGroupName = os.Getenv("AZURE_RESOURCE_GROUP")
	}

	// Initialize subscription and resource group from AzureTestClient if not set
	if subscriptionID == "" || resourceGroupName == "" {
		tc, err := utils.CreateAzureTestClient()
		if err == nil {
			if subscriptionID == "" {
				subscriptionID = tc.GetSubscriptionID()
			}
			if resourceGroupName == "" {
				resourceGroupName = tc.GetResourceGroup()
			}
		} else {
			utils.Logf("Warning: Could not create AzureTestClient: %v", err)
		}
	}

	// Initialize Service Gateway config from environment with defaults
	if serviceGatewayName == "" {
		serviceGatewayName = os.Getenv(envServiceGatewayName)
		if serviceGatewayName == "" {
			serviceGatewayName = defaultServiceGatewayName
		}
	}
	if apiVersion == "" {
		apiVersion = os.Getenv(envServiceGatewayAPIVersion)
		if apiVersion == "" {
			apiVersion = defaultAPIVersion
		}
	}

	utils.Logf("SLB Config: SubscriptionID=%s, ResourceGroup=%s, ServiceGateway=%s, APIVersion=%s",
		subscriptionID, resourceGroupName, serviceGatewayName, apiVersion)
}

// buildServiceGatewayURL constructs the Service Gateway API URL for a given path
func buildServiceGatewayURL(path string) string {
	ensureSLBConfigInitialized()
	return fmt.Sprintf(
		"https://management.azure.com/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/serviceGateways/%s/%s?api-version=%s",
		subscriptionID, resourceGroupName, serviceGatewayName, path, apiVersion,
	)
}

// azTransientRetries is how many times runAz re-issues a command that failed for a reason the
// Azure control plane is expected to recover from on its own.
const azTransientRetries = 4

// azRetryBackoff is the pause between those attempts.
const azRetryBackoff = 10 * time.Second

// isTransientAzureFailure reports whether an `az` invocation failed for a reason that is worth
// retrying rather than failing the spec.
//
// A 503 from ARM arrives as an HTML error page rather than JSON, and throttling (429) and gateway
// timeouts behave the same way. These say nothing about the cloud provider under test, but because
// most callers issue a single un-polled query, one of them is otherwise enough to fail a spec that
// has already run for minutes — and, in a suite this long, to cost a whole run.
func isTransientAzureFailure(output string) bool {
	for _, marker := range []string{
		"Service Unavailable",
		"ServiceUnavailable",
		"(503)",
		"Gateway Timeout",
		"(504)",
		"TooManyRequests",
		"(429)",
		"temporarily unavailable",
		"connection reset by peer",
		"Temporary failure in name resolution",
		"Please run 'az login'",
		"AADSTS700082", // expired refresh token; the next attempt re-authenticates
	} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

func TestIsTransientAzureFailure(t *testing.T) {
	for _, tc := range []struct {
		output    string
		transient bool
	}{
		{"ERROR: HTTPSConnectionPool(host='management.azure.com', port=443): Max retries exceeded with url: /subscriptions/x " +
			"(Caused by NameResolutionError(\"<urllib3.connection.HTTPSConnection object at 0x7cf50cc26510>: Failed to resolve " +
			"'management.azure.com' ([Errno -3] Temporary failure in name resolution)\"))", true},
		{"ERROR: (ServiceUnavailable) The service is temporarily unavailable.", true},
		{"ERROR: (ResourceNotFound) The Resource 'Microsoft.Network/natGateways/x' under resource group 'rg' was not found.", false},
		{"ERROR: (AuthorizationFailed) The client does not have authorization to perform action.", false},
	} {
		if got := isTransientAzureFailure(tc.output); got != tc.transient {
			t.Errorf("isTransientAzureFailure(%q) = %v, want %v", tc.output, got, tc.transient)
		}
	}
}

// runAz runs an `az` command and returns its stdout, retrying transient control-plane failures.
//
// It deliberately uses Output() rather than CombinedOutput(): `az` writes warnings (extension
// notices, deprecation and MSAL messages) to stderr, and folding those into stdout corrupts the
// buffer that callers hand to json.Unmarshal, turning a perfectly healthy query into a parse
// error. Stderr is still reported, but only as part of the error message.
func runAz(args ...string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= azTransientRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(azRetryBackoff)
		}

		cmd := exec.Command("az", args...)
		stdout, err := cmd.Output()
		if err == nil {
			return stdout, nil
		}

		stderr := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		lastErr = fmt.Errorf("az %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr))

		if !isTransientAzureFailure(stderr) && !isTransientAzureFailure(string(stdout)) {
			// Hand back the diagnostic text on the failure path. Several callers deliberately
			// expect the command to fail — "the resource is gone" is asserted by running `az
			// ... show` and matching ResourceNotFound — and `az` writes that to stderr. Returning
			// only stdout here would give them an empty buffer and turn a correct deletion into
			// an "unexpected error" with no message.
			return append(stdout, stderr...), lastErr
		}
		utils.Logf("Transient Azure failure (attempt %d/%d), retrying: %v", attempt+1, azTransientRetries+1, lastErr)
	}
	return nil, fmt.Errorf("az command still failing after %d attempts: %w", azTransientRetries+1, lastErr)
}

// queryServiceGatewayServices queries all services in the Service Gateway
func queryServiceGatewayServices() (ServiceGatewayServicesResponse, error) {
	url := buildServiceGatewayURL("services")
	output, err := runAz("rest", "--method", "get", "--url", url)
	if err != nil {
		return ServiceGatewayServicesResponse{}, fmt.Errorf("failed to query Service Gateway services: %w", err)
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
	output, err := runAz("rest", "--method", "get", "--url", url)
	if err != nil {
		return ServiceGatewayAddressLocationsResponse{}, fmt.Errorf("failed to query Service Gateway address locations: %w", err)
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
	Expect(serviceGatewayCleanupErr()).To(Succeed())
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
			if svc.Properties.ServiceType == "Outbound" && svc.Name == outboundServiceName(egressName) {
				found = true
				Fail(fmt.Sprintf("Outbound service '%s' still exists in Service Gateway after cleanup", egressName))
			}
		}
		if !found {
			utils.Logf("  ✓ Outbound service '%s' cleaned up", egressName)
		}
	}
}

// serviceGatewayCleanupErr returns nil only when the Service Gateway contains just the
// default outbound service (i.e. all test services have been cleaned up). It is the
// error-returning core used to poll for cleanup via Eventually, so a spec waits exactly
// as long as Azure actually needs instead of a fixed, conservative sleep.
func serviceGatewayCleanupErr() error {
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return fmt.Errorf("query Service Gateway services: %w", err)
	}
	return serviceGatewayServicesCleanupErr(sgResponse.Value)
}

func isDefaultOutboundService(service ServiceGatewayService) bool {
	return service.Properties.ServiceType == "Outbound" &&
		(service.Properties.IsDefault || strings.EqualFold(service.Name, "default-natgw"))
}

func serviceGatewayServicesCleanupErr(services []ServiceGatewayService) error {
	foundDefault := false
	for _, svc := range services {
		if !isDefaultOutboundService(svc) || foundDefault {
			return fmt.Errorf("unexpected service %q (type %s) still exists in Service Gateway after cleanup", svc.Name, svc.Properties.ServiceType)
		}
		foundDefault = true
	}
	// Without this an empty Service Gateway satisfies the poll immediately: the loop finds no
	// unexpected service because it finds no service at all. Cleanup must remove the test's
	// services and leave the cluster-wide default outbound service untouched.
	if !foundDefault {
		return fmt.Errorf("the default outbound service is missing from the Service Gateway; cleanup must leave it in place")
	}
	return nil
}

func TestServiceGatewayServicesCleanupErr(t *testing.T) {
	service := func(name, kind string, isDefault bool) ServiceGatewayService {
		svc := ServiceGatewayService{Name: name}
		svc.Properties.ServiceType, svc.Properties.IsDefault = kind, isDefault
		return svc
	}
	named := service("default-natgw", "Outbound", false)
	flagged := service("custom-default", "Outbound", true)
	for _, tc := range []struct {
		name      string
		services  []ServiceGatewayService
		wantError bool
	}{
		{name: "named default", services: []ServiceGatewayService{named}},
		{name: "case-insensitive name", services: []ServiceGatewayService{service("DEFAULT-NATGW", "Outbound", false)}},
		{name: "RP default flag", services: []ServiceGatewayService{flagged}},
		{name: "missing default", wantError: true},
		{name: "ordinary outbound is not default", services: []ServiceGatewayService{service("custom-default", "Outbound", false)}, wantError: true},
		{name: "encoded default-natgw label is not default", services: []ServiceGatewayService{named, service(outboundServiceName("default-natgw"), "Outbound", false)}, wantError: true},
		{name: "inbound with reserved name", services: []ServiceGatewayService{service("default-natgw", "Inbound", false)}, wantError: true},
		{name: "inbound with default flag", services: []ServiceGatewayService{service("custom-default", "Inbound", true)}, wantError: true},
		{name: "extra service", services: []ServiceGatewayService{flagged, service("extra", "Outbound", false)}, wantError: true},
		{name: "extra flagged service", services: []ServiceGatewayService{named, flagged}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := serviceGatewayServicesCleanupErr(tc.services)
			if (err != nil) != tc.wantError {
				t.Fatalf("cleanup error = %v, wantError = %t", err, tc.wantError)
			}
		})
	}
}

// addressLocationsCleanupErr returns nil only when no address in the Service Gateway still
// references a service. Error-returning core for polling address-location cleanup.
func addressLocationsCleanupErr() error {
	alResponse, err := queryServiceGatewayAddressLocations()
	if err != nil {
		return fmt.Errorf("query Service Gateway address locations: %w", err)
	}
	for _, location := range alResponse.Value {
		for _, addr := range location.Addresses {
			if len(addr.Services) > 0 {
				return fmt.Errorf("address %s in location %s still has %d service reference(s) after cleanup",
					addr.Address, location.AddressLocation, len(addr.Services))
			}
		}
	}
	return nil
}

// natGatewayCleanupErr returns nil only when none of the named egress (outbound) services
// remain in the Service Gateway. Error-returning core for polling NAT Gateway cleanup.
func natGatewayCleanupErr(egressNames []string) error {
	if len(egressNames) == 0 {
		return nil
	}
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return fmt.Errorf("query Service Gateway services: %w", err)
	}
	for _, egressName := range egressNames {
		for _, svc := range sgResponse.Value {
			if svc.Properties.ServiceType == "Outbound" && svc.Name == outboundServiceName(egressName) {
				return fmt.Errorf("outbound service %q still exists in Service Gateway after cleanup", egressName)
			}
		}
	}
	// Deregistering from the ServiceGateway says nothing about ARM. Without this a CCM that
	// unregisters but never deletes the resources passes every egress teardown while leaking a NAT
	// Gateway and, on a dual-stack cluster, both of its Public IPs.
	for _, egressName := range egressNames {
		if err := azureEgressResourcesAbsentErr(egressName); err != nil {
			return err
		}
	}
	return nil
}

// azurePublicIPAbsentErr returns nil once no Public IP exists for the given service UID. It is the
// positive form of "nothing was provisioned", usable on its own rather than as a side effect of
// verifyAzureResources failing for some unrelated reason.
func azurePublicIPAbsentErr(serviceUID string) error {
	return azurePublicIPNamedAbsentErr(inboundServiceName(serviceUID) + "-pip")
}

// azurePublicIPNamedAbsentErr returns nil once the exactly-named Public IP is gone from Azure.
func azurePublicIPNamedAbsentErr(publicIPName string) error {
	pipOutput, err := runAz("network", "public-ip", "list",
		"--resource-group", resourceGroupName,
		"--output", "json")
	if err != nil {
		return fmt.Errorf("failed to query Azure for Public IPs: %w", err)
	}

	var publicIPs []AzurePublicIP
	if err := json.Unmarshal(pipOutput, &publicIPs); err != nil {
		return fmt.Errorf("failed to parse Public IPs response: %w", err)
	}
	for i := range publicIPs {
		if publicIPs[i].Name == publicIPName {
			return fmt.Errorf("public IP %s exists for a service that should not have been provisioned", publicIPName)
		}
	}
	return nil
}

// azureLoadBalancerAbsentErr returns nil once the service's Azure Load Balancer is gone.
//
// Deletion specs previously asserted only that the Service Gateway registration disappeared, which
// says nothing about the underlying ARM resources: a CCM that deregisters the service but never
// issues the LB/PIP deletes passes every such assertion while leaking billable Azure resources on
// each service deletion. Absence is decided from the LIST output rather than a `show` exit code so
// a transient query failure is reported as an error instead of being mistaken for "deleted".
func azureLoadBalancerAbsentErr(serviceUID string) error {
	lbOutput, err := runAz("network", "lb", "list",
		"--resource-group", resourceGroupName,
		"--output", "json")
	if err != nil {
		return fmt.Errorf("failed to query Azure for Load Balancers: %w", err)
	}

	var loadBalancers []AzureLoadBalancer
	if err := json.Unmarshal(lbOutput, &loadBalancers); err != nil {
		return fmt.Errorf("failed to parse Load Balancers response: %w", err)
	}
	for i := range loadBalancers {
		if loadBalancers[i].Name == inboundServiceName(serviceUID) {
			return fmt.Errorf("load balancer %s still exists in Azure after deletion (leaked)", serviceUID)
		}
	}
	return nil
}

// azureInboundResourcesAbsentErr returns nil once BOTH the service's Load Balancer and its Public
// IP are gone from Azure. This is the resource-level counterpart to serviceDeletedErr, which only
// checks the ServiceGateway registration.
func azureInboundResourcesAbsentErr(serviceUID string) error {
	if err := azureLoadBalancerAbsentErr(serviceUID); err != nil {
		return err
	}
	return azurePublicIPAbsentErr(serviceUID)
}

// azureEgressResourcesAbsentErr returns nil once the egress identity's NAT Gateway and its Public
// IP are gone from Azure.
//
// The NAT Gateway uses the canonical outbound name. Its IPv4 Public IP follows the same
// <name>-pip convention as the inbound path, and a dual-stack cluster adds <name>-pip-v6.
// Without this check a CCM that removes the Outbound registration from the ServiceGateway but
// never deletes the ARM resources passes every egress teardown spec while leaking a NAT Gateway
// and a Public IP per egress identity.
func azureEgressResourcesAbsentErr(egressName string) error {
	natOutput, err := runAz("network", "nat", "gateway", "show",
		"--resource-group", resourceGroupName,
		"--name", outboundServiceName(egressName),
		"--output", "json")
	if err == nil {
		return fmt.Errorf("NAT Gateway %s still exists in Azure after teardown (leaked)", egressName)
	}
	text := string(natOutput)
	if !strings.Contains(text, "not found") && !strings.Contains(text, "NotFound") {
		return fmt.Errorf("could not determine whether NAT Gateway %s was deleted: %s", egressName, text)
	}
	if err := azurePublicIPNamedAbsentErr(PublicIPNameForEgress(egressName)); err != nil {
		return err
	}
	// A dual-stack cluster also gets an IPv6 address, named "<name>-pip-v6". It is provisioned by
	// the same create and removed by the same delete, so a teardown that misses it leaks a second
	// billable address per egress identity.
	return azurePublicIPNamedAbsentErr(PublicIPNameForEgress(egressName) + "-v6")
}

// verifyAzureResources verifies Public IP, Load Balancer, and Service Gateway for a given service
func verifyAzureResources(serviceUID string) error {
	loadBalancerName := inboundServiceName(serviceUID)
	publicIPName := loadBalancerName + "-pip"

	// Verify Public IP in Azure
	pipOutput, err := runAz("network", "public-ip", "list",
		"--resource-group", resourceGroupName,
		"--output", "json")
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
	if servicePublicIP.Tags[consts.ServiceGatewayManagedTagKey] != consts.ServiceGatewayManagedTagValue {
		return fmt.Errorf("public IP %s is missing the CCM managed marker", publicIPName)
	}

	// Verify Load Balancer in Azure
	lbOutput, err := runAz("network", "lb", "show",
		"--resource-group", resourceGroupName,
		"--name", loadBalancerName,
		"--output", "json")
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
	if _, tagged := serviceLB.Tags[consts.ServiceGatewayManagedTagKey]; tagged {
		return fmt.Errorf("load balancer %s must not carry the CCM managed marker", loadBalancerName)
	}

	// Verify Load Balancer has backend address pools
	if len(serviceLB.BackendAddressPools) == 0 {
		return fmt.Errorf("load Balancer %s has no backend address pools", loadBalancerName)
	}
	for _, pool := range serviceLB.BackendAddressPools {
		if pool.Name != loadBalancerName {
			return fmt.Errorf("load balancer %s has unexpected backend pool %s", loadBalancerName, pool.Name)
		}
	}

	// Verify Service Gateway has this service
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return fmt.Errorf("failed to query Service Gateway services: %w", err)
	}

	var foundService bool
	for _, sgSvc := range sgResponse.Value {
		if sgSvc.Name == loadBalancerName && sgSvc.Properties.ServiceType == "Inbound" {
			foundService = true
			break
		}
	}
	if !foundService {
		return fmt.Errorf("service %s not found in Service Gateway", serviceUID)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Shared polling helpers
//
// These wrap the common "wait until Azure and the Service Gateway converge"
// checks behind error-returning predicates (suitable for Eventually) and thin
// Eventually wrappers. They let specs poll for convergence instead of sleeping a
// fixed, conservative amount of time, which is both faster and far less flaky.
//
// Predicates (return nil on success) are composable inside a caller's own
// Eventually loop (e.g. when polling several services at once); the eventually*
// wrappers are convenience one-liners for the common single-resource case.
// ---------------------------------------------------------------------------

// defaultPollInterval is the polling cadence used by the eventually* wrappers.
const defaultPollInterval = 10 * time.Second

// countRegisteredEndpoints returns how many distinct addresses the Service Gateway has registered
// for the given canonical service/egress identifier. It works for both inbound services and outbound egress,
// since both are referenced by identifier in an address's Services list.
//
// Two properties matter for callers:
//
//   - It counts ADDRESSES, not pods. A single-stack pod contributes one address, so a pod count and
//     an address count coincide; a dual-stack pod registers one address per IP family and therefore
//     contributes two. Callers that want to express an expectation in pods on a dual-stack cluster
//     must derive the eligible address set separately from the pod count.
//   - Each address is counted once even if it appears under more than one node location (which
//     happens transiently while a node's IP changes) and locations that are not a live view of
//     state are skipped, so a tombstoned entry cannot inflate the total.
func countRegisteredEndpoints(serviceID string) (int, error) {
	alResponse, err := queryServiceGatewayAddressLocations()
	if err != nil {
		return 0, fmt.Errorf("query Service Gateway address locations: %w", err)
	}
	seen := make(map[string]struct{})
	for _, location := range alResponse.Value {
		// A delete-shaped location describes addresses being removed, not addresses in service.
		if strings.EqualFold(location.AddressUpdateAction, "Delete") {
			continue
		}
		for _, addr := range location.Addresses {
			for _, svc := range addr.Services {
				if svc == serviceID {
					seen[addr.Address] = struct{}{}
					break
				}
			}
		}
	}
	return len(seen), nil
}

// registeredAddressesFor returns the exact set of addresses the Service Gateway has registered for
// the given canonical service/egress identifier.
//
// Prefer it over countRegisteredEndpoints whenever a spec removes backends. A count cannot tell a
// correct removal apart from a stale address that leaked paired with a live one that was wrongly
// dropped — both leave the total unchanged — which is precisely the regression the removal paths
// are most likely to have.
func registeredAddressesFor(serviceID string) (map[string]struct{}, error) {
	alResponse, err := queryServiceGatewayAddressLocations()
	if err != nil {
		return nil, fmt.Errorf("query Service Gateway address locations: %w", err)
	}
	addrs := make(map[string]struct{})
	for _, location := range alResponse.Value {
		if strings.EqualFold(location.AddressUpdateAction, "Delete") {
			continue
		}
		for _, addr := range location.Addresses {
			for _, svc := range addr.Services {
				if svc == serviceID {
					addrs[addr.Address] = struct{}{}
					break
				}
			}
		}
	}
	return addrs, nil
}

// registeredAddressesMatchErr returns nil once the Service Gateway has registered exactly want for
// the service: no missing address and, just as importantly, no extra one left behind.
func registeredAddressesMatchErr(serviceID string, want map[string]struct{}) error {
	got, err := registeredAddressesFor(serviceID)
	if err != nil {
		return err
	}
	return registeredAddressSetsMatchErr(serviceID, got, want)
}

func registeredAddressSetsMatchErr(serviceID string, got, want map[string]struct{}) error {
	var missing, unexpected []string
	for addr := range want {
		if _, ok := got[addr]; !ok {
			missing = append(missing, addr)
		}
	}
	for addr := range got {
		if _, ok := want[addr]; !ok {
			unexpected = append(unexpected, addr)
		}
	}
	if len(missing) > 0 || len(unexpected) > 0 {
		sort.Strings(missing)
		sort.Strings(unexpected)
		return fmt.Errorf("registered addresses for %s do not match: missing %v, unexpectedly still registered %v",
			serviceID, missing, unexpected)
	}
	return nil
}

// podIPSet collects assigned pod addresses, not a pod count or a Service's served families.
func podIPSet(pods []v1.Pod) map[string]struct{} {
	set := make(map[string]struct{})
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		addresses := pod.Status.PodIPs
		hasAddress := false
		for _, address := range addresses {
			hasAddress = hasAddress || address.IP != ""
		}
		if !hasAddress {
			addresses = []v1.PodIP{{IP: pod.Status.PodIP}}
		}
		for _, address := range addresses {
			if ip := net.ParseIP(address.IP); ip != nil {
				set[ip.String()] = struct{}{}
			}
		}
	}
	return set
}

func ipFamily(address string) v1.IPFamily {
	if ip := net.ParseIP(address); ip != nil {
		if ip.To4() != nil {
			return v1.IPv4Protocol
		}
		return v1.IPv6Protocol
	}
	return ""
}

// inboundPodIPSet intersects assigned addresses with the Service and each node's InternalIP families.
func inboundPodIPSet(cs clientset.Interface, pods []v1.Pod, families []v1.IPFamily) (map[string]struct{}, error) {
	locations, err := inboundPodAddressLocations(cs, pods, families)
	if err != nil {
		return nil, err
	}
	want := make(map[string]struct{})
	for _, addresses := range locations {
		for _, address := range addresses {
			want[address] = struct{}{}
		}
	}
	return want, nil
}

func inboundPodAddressLocations(cs clientset.Interface, pods []v1.Pod, families []v1.IPFamily) (map[string][]string, error) {
	want := make(map[string][]string)
	if len(pods) == 0 {
		return want, nil
	}
	nodes, err := cs.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	nodeFamilies := make(map[string]map[v1.IPFamily]string, len(nodes.Items))
	for _, node := range nodes.Items {
		nodeFamilies[node.Name] = make(map[v1.IPFamily]string)
		for _, address := range node.Status.Addresses {
			if family := ipFamily(address.Address); address.Type == v1.NodeInternalIP && family != "" {
				if nodeFamilies[node.Name][family] == "" {
					nodeFamilies[node.Name][family] = net.ParseIP(address.Address).String()
				}
			}
		}
	}
	if len(families) == 0 {
		families = []v1.IPFamily{v1.IPv4Protocol}
	}
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		available, ok := nodeFamilies[pod.Spec.NodeName]
		if !ok || len(available) == 0 {
			return nil, fmt.Errorf("pod %s/%s has no known node InternalIP families", pod.Namespace, pod.Name)
		}
		addresses := podIPSet([]v1.Pod{pod})
		if len(addresses) == 0 {
			return nil, fmt.Errorf("pod %s/%s has no assigned IP", pod.Namespace, pod.Name)
		}
		for address := range addresses {
			family := ipFamily(address)
			for _, served := range families {
				location := available[family]
				if served == family && location != "" && !slices.Contains(want[location], address) {
					want[location] = append(want[location], address)
				}
			}
		}
	}
	return want, nil
}

// egressPodIPSet includes only addresses with a same-family node location in pod status.
func egressPodIPSet(pods []v1.Pod) (map[string]struct{}, error) {
	locations, err := egressPodAddressLocations(pods)
	if err != nil {
		return nil, err
	}
	want := make(map[string]struct{})
	for _, addresses := range locations {
		for _, address := range addresses {
			want[address] = struct{}{}
		}
	}
	return want, nil
}

func egressPodAddressLocations(pods []v1.Pod) (map[string][]string, error) {
	want := make(map[string][]string)
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		if net.ParseIP(pod.Status.HostIP) == nil {
			return nil, fmt.Errorf("pod %s/%s has no valid primary node IP", pod.Namespace, pod.Name)
		}
		families := make(map[v1.IPFamily]string)
		for _, host := range pod.Status.HostIPs {
			if family := ipFamily(host.IP); family != "" {
				if families[family] == "" {
					families[family] = net.ParseIP(host.IP).String()
				}
			}
		}
		if len(families) == 0 {
			if family := ipFamily(pod.Status.HostIP); family != "" {
				families[family] = net.ParseIP(pod.Status.HostIP).String()
			}
		}
		if len(families) == 0 {
			return nil, fmt.Errorf("pod %s/%s has no node IP", pod.Namespace, pod.Name)
		}
		addresses := podIPSet([]v1.Pod{pod})
		if len(addresses) == 0 {
			return nil, fmt.Errorf("pod %s/%s has no assigned IP", pod.Namespace, pod.Name)
		}
		for address := range addresses {
			if location := families[ipFamily(address)]; location != "" && !slices.Contains(want[location], address) {
				want[location] = append(want[location], address)
			}
		}
	}
	return want, nil
}

// servicePodsReconciledErr counts Ready pods independently of their eligible backend addresses.
func servicePodsReconciledErr(cs clientset.Interface, namespace, serviceUID string, wantPods int) error {
	services, err := cs.CoreV1().Services(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, service := range services.Items {
		if string(service.UID) != serviceUID {
			continue
		}
		want, podCount, err := livePodIPsForSelector(cs, namespace, service.Spec.Selector, service.Spec.IPFamilies)
		if err != nil {
			return err
		}
		if podCount != wantPods {
			return fmt.Errorf("service %s has %d Ready pods, want %d", serviceUID, podCount, wantPods)
		}
		return serviceReconciledMatchErr(serviceUID, want)
	}
	return fmt.Errorf("Service UID %s not found in namespace %s", serviceUID, namespace)
}

func egressPodsRegisteredErr(cs clientset.Interface, namespace, egressName string, wantPods int) error {
	want, podCount, err := livePodIPsWithLabel(cs, namespace, egressLabel, egressName)
	if err != nil {
		return err
	}
	if podCount != wantPods {
		return fmt.Errorf("egress %s has %d active pods, want %d", egressName, podCount, wantPods)
	}
	return egressRegisteredMatchErr(egressName, want)
}

// serviceReconciledErr returns nil once the inbound service's Azure resources exist
// (PIP, LB with SKU=Service and a backend pool, and a Service Gateway entry) and,
// when wantEndpoints >= 0, exactly wantEndpoints pod IPs are registered for it. Pass
// a negative wantEndpoints to skip the endpoint-count assertion.
func serviceReconciledErr(serviceUID string, wantEndpoints int) error {
	if err := verifyAzureResources(serviceUID); err != nil {
		return err
	}
	if wantEndpoints < 0 {
		return nil
	}
	got, err := countRegisteredEndpoints(inboundServiceName(serviceUID))
	if err != nil {
		return err
	}
	if got != wantEndpoints {
		return fmt.Errorf("service %s has %d registered endpoints, want %d", serviceUID, got, wantEndpoints)
	}
	return nil
}

// serviceReconciledMatchErr is the set-based sibling of serviceReconciledErr: it verifies the
// inbound service's Azure resources exist AND that exactly wantAddrs are registered for it.
//
// Prefer this whenever the expected pod IPs are knowable. A count cannot distinguish the right N
// addresses from the wrong N: registering a node IP, a stale pod's IP, or another service's pod
// leaves the count correct while traffic goes to the wrong place, and on a scale-down it cannot
// tell a drained survivor from a drained victim.
func serviceReconciledMatchErr(serviceUID string, wantAddrs map[string]struct{}) error {
	if err := verifyAzureResources(serviceUID); err != nil {
		return err
	}
	return registeredAddressesMatchErr(inboundServiceName(serviceUID), wantAddrs)
}

// egressRegisteredMatchErr is the set-based sibling of egressRegisteredErr: the egress service must
// exist with a NAT Gateway, and exactly wantAddrs must be registered for it.
func egressRegisteredMatchErr(egressName string, wantAddrs map[string]struct{}) error {
	if err := egressRegisteredErr(egressName, -1); err != nil {
		return err
	}
	return registeredAddressesMatchErr(outboundServiceName(egressName), wantAddrs)
}

// serviceDeletedErr returns nil once the inbound service is gone from the Service Gateway, no
// address location still references it, AND its Azure Load Balancer and Public IP are deleted.
//
// The Azure-resource check is the important half: without it a CCM that unregisters the service
// but never issues the ARM deletes satisfies every deletion spec in the suite while leaking a
// billable Load Balancer and Public IP per deleted service. Every caller already wraps this in
// Eventually/Consistently, so the extra convergence time is absorbed there.
func serviceDeletedErr(serviceUID string) error {
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return fmt.Errorf("query Service Gateway services: %w", err)
	}
	for _, svc := range sgResponse.Value {
		if svc.Name == inboundServiceName(serviceUID) {
			return fmt.Errorf("service %s still registered in Service Gateway", serviceUID)
		}
	}
	got, err := countRegisteredEndpoints(inboundServiceName(serviceUID))
	if err != nil {
		return err
	}
	if got > 0 {
		return fmt.Errorf("service %s still has %d registered endpoint(s)", serviceUID, got)
	}
	return azureInboundResourcesAbsentErr(serviceUID)
}

// egressRegisteredErr returns nil once the named egress (outbound) service exists in
// the Service Gateway with a NAT Gateway and, when wantAddresses >= 0, exactly that
// many addresses registered. Use egressPodsRegisteredErr for a pod-count expectation.
func egressRegisteredErr(egressName string, wantAddresses int) error {
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return fmt.Errorf("query Service Gateway services: %w", err)
	}
	found := false
	for _, svc := range sgResponse.Value {
		if svc.Properties.ServiceType == "Outbound" && svc.Name == outboundServiceName(egressName) {
			found = true
			if svc.Properties.PublicNatGatewayID == "" {
				return fmt.Errorf("egress %s has no NAT Gateway ID yet", egressName)
			}
			break
		}
	}
	if !found {
		return fmt.Errorf("egress %s not registered in Service Gateway yet", egressName)
	}
	if wantAddresses < 0 {
		return nil
	}
	got, err := countRegisteredEndpoints(outboundServiceName(egressName))
	if err != nil {
		return err
	}
	if got != wantAddresses {
		return fmt.Errorf("egress %s has %d registered addresses, want %d", egressName, got, wantAddresses)
	}
	return nil
}

// eventuallyServiceReconciled polls until the inbound service is fully reconciled in
// Azure and the Service Gateway, with a separate Ready-pod count and exact address set.
func eventuallyServiceReconciled(cs clientset.Interface, namespace, serviceUID string, wantPods int, timeout time.Duration) {
	Eventually(func() error {
		return servicePodsReconciledErr(cs, namespace, serviceUID, wantPods)
	}, timeout, defaultPollInterval).Should(Succeed(),
		"service %s should be reconciled in Azure and the Service Gateway", serviceUID)
}

// eventuallyServiceDeleted polls until the inbound service is fully removed from the
// Service Gateway and its address locations.
func eventuallyServiceDeleted(serviceUID string, timeout time.Duration) {
	Eventually(func() error {
		return serviceDeletedErr(serviceUID)
	}, timeout, defaultPollInterval).Should(Succeed(),
		"service %s should be removed from the Service Gateway", serviceUID)
}

// eventuallyEgressRegistered polls until the egress service is reconciled with its NAT
// Gateway and the requested address count.
func eventuallyEgressRegistered(egressName string, wantAddresses int, timeout time.Duration) {
	Eventually(func() error {
		return egressRegisteredErr(egressName, wantAddresses)
	}, timeout, defaultPollInterval).Should(Succeed(),
		"egress %s should be registered with %d addresses in the Service Gateway", egressName, wantAddresses)
}

func eventuallyEgressPodsRegistered(cs clientset.Interface, namespace, egressName string, wantPods int, timeout time.Duration) {
	Eventually(func() error {
		return egressPodsRegisteredErr(cs, namespace, egressName, wantPods)
	}, timeout, defaultPollInterval).Should(Succeed(),
		"egress %s should register the eligible addresses of %d pods", egressName, wantPods)
}

func TestPodAddressSetsByFamily(t *testing.T) {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node"},
		Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
			{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
			{Type: v1.NodeInternalIP, Address: "2001:db8::1"},
		}},
	}
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "scope"},
		Spec:       v1.PodSpec{NodeName: node.Name},
		Status: v1.PodStatus{
			PodIP: "10.244.0.1", PodIPs: []v1.PodIP{{IP: "10.244.0.1"}, {IP: "2001:DB8::0010"}},
			HostIP: "10.0.0.1", HostIPs: []v1.HostIP{{IP: "10.0.0.1"}, {IP: "2001:db8::1"}},
		},
	}
	for _, tc := range []struct {
		name     string
		families []v1.IPFamily
		nodeV4   bool
		want     map[string]struct{}
	}{
		{"default IPv4 Service", nil, false, map[string]struct{}{"10.244.0.1": {}}},
		{"IPv4 Service on DS node", []v1.IPFamily{v1.IPv4Protocol}, false, map[string]struct{}{"10.244.0.1": {}}},
		{"IPv6 Service on DS node", []v1.IPFamily{v1.IPv6Protocol}, false, map[string]struct{}{"2001:db8::10": {}}},
		{"both served families", []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}, false, map[string]struct{}{"10.244.0.1": {}, "2001:db8::10": {}}},
		{"node without IPv6 InternalIP", []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}, true, map[string]struct{}{"10.244.0.1": {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := node.DeepCopy()
			if tc.nodeV4 {
				n.Status.Addresses[1].Type = v1.NodeExternalIP
			}
			before := pod.DeepCopy()
			got, err := inboundPodIPSet(fake.NewSimpleClientset(n), []v1.Pod{pod}, tc.families)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("addresses=%v, err=%v, want=%v", got, err, tc.want)
			}
			if !reflect.DeepEqual(&pod, before) {
				t.Fatal("address expectation mutated pod metadata/status")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*v1.Pod)
		want   map[string][]string
		err    bool
	}{
		{"dual stack", func(*v1.Pod) {}, map[string][]string{"10.0.0.1": {"10.244.0.1"}, "2001:db8::1": {"2001:db8::10"}}, false},
		{"HostIP fallback", func(p *v1.Pod) { p.Status.HostIPs = nil }, map[string][]string{"10.0.0.1": {"10.244.0.1"}}, false},
		{"PodIP fallback", func(p *v1.Pod) { p.Status.PodIPs = nil }, map[string][]string{"10.0.0.1": {"10.244.0.1"}}, false},
		{"empty PodIPs fallback", func(p *v1.Pod) { p.Status.PodIPs = []v1.PodIP{{}} }, map[string][]string{"10.0.0.1": {"10.244.0.1"}}, false},
		{"IPv6-only pod", func(p *v1.Pod) { p.Status.PodIPs = p.Status.PodIPs[1:]; p.Status.PodIP = "2001:db8::10" }, map[string][]string{"2001:db8::1": {"2001:db8::10"}}, false},
		{"terminating", func(p *v1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now }, map[string][]string{}, false},
		{"no pod IP", func(p *v1.Pod) { p.Status.PodIPs = nil; p.Status.PodIP = "" }, nil, true},
		{"no primary host IP", func(p *v1.Pod) { p.Status.HostIP = "" }, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pod.DeepCopy()
			tc.change(p)
			got, err := egressPodAddressLocations([]v1.Pod{*p})
			if (err != nil) != tc.err || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("locations=%v err=%v, want=%v error=%t", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestLivePodAddressSets(t *testing.T) {
	makePod := func(name, namespace, nodeName, v4, v6 string, ready bool) *v1.Pod {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app": "backend", egressLabel: "Raw-lb"}},
			Spec:       v1.PodSpec{NodeName: nodeName},
			Status: v1.PodStatus{
				Phase: v1.PodRunning, PodIP: v4, PodIPs: []v1.PodIP{{IP: v4}}, HostIP: "10.0.0.1",
				HostIPs:    []v1.HostIP{{IP: "10.0.0.1"}},
				Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}},
			},
		}
		if v6 != "" {
			pod.Status.PodIPs = append(pod.Status.PodIPs, v1.PodIP{IP: v6})
		}
		if nodeName == "dual" {
			pod.Status.HostIPs = append(pod.Status.HostIPs, v1.HostIP{IP: "2001:db8::1"})
		} else {
			pod.Status.HostIP = "10.0.0.2"
			pod.Status.HostIPs = []v1.HostIP{{IP: "10.0.0.2"}}
		}
		if ready {
			pod.Status.Conditions[0].Status = v1.ConditionTrue
		}
		return pod
	}
	dual := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "dual"}, Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
		{Type: v1.NodeInternalIP, Address: "10.0.0.1"}, {Type: v1.NodeInternalIP, Address: "2001:db8::1"},
	}}}
	v4 := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "v4"}, Status: v1.NodeStatus{Addresses: []v1.NodeAddress{
		{Type: v1.NodeInternalIP, Address: "10.0.0.2"}, {Type: v1.NodeExternalIP, Address: "2001:db8::2"},
	}}}
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "scope", UID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"},
		Spec: v1.ServiceSpec{Selector: map[string]string{"app": "backend"}, IPFamilies: []v1.IPFamily{v1.IPv4Protocol}}}
	dsPod := makePod("ds", "scope", "dual", "10.244.0.1", "2001:db8::10", true)
	v4Pod := makePod("v4", "scope", "v4", "10.244.0.2", "2001:db8::20", true)
	unready := makePod("unready", "scope", "dual", "10.244.0.3", "2001:db8::30", false)
	failed := makePod("failed", "scope", "dual", "10.244.0.4", "2001:db8::40", false)
	failed.Status.Phase = v1.PodFailed
	deleting := makePod("deleting", "scope", "dual", "10.244.0.5", "2001:db8::50", true)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	otherNamespace := makePod("ds", "elsewhere", "dual", "10.244.0.6", "2001:db8::60", true)
	otherSelector := makePod("other", "scope", "dual", "10.244.0.7", "2001:db8::70", true)
	otherSelector.Labels = map[string]string{"app": "other", egressLabel: "other"}
	objects := []runtime.Object{dual, v4, service, dsPod, v4Pod, unready, failed, deleting, otherNamespace, otherSelector}
	cs := fake.NewSimpleClientset(objects...)
	wantV4 := map[string]struct{}{"10.244.0.1": {}, "10.244.0.2": {}}
	got, podCount, err := livePodIPsFor(cs, "scope", "web")
	if err != nil || podCount != 2 || !reflect.DeepEqual(got, wantV4) {
		t.Fatalf("IPv4 Service addresses=%v pods=%d err=%v", got, podCount, err)
	}
	got, podCount, err = livePodIPsForSelector(cs, "scope", service.Spec.Selector, []v1.IPFamily{v1.IPv6Protocol})
	if err != nil || podCount != 2 || !reflect.DeepEqual(got, map[string]struct{}{"2001:db8::10": {}}) {
		t.Fatalf("IPv6 Service addresses=%v pods=%d err=%v", got, podCount, err)
	}
	got, podCount, err = livePodIPsWithLabel(cs, "scope", egressLabel, "Raw-lb")
	wantEgress := map[string]struct{}{"10.244.0.1": {}, "2001:db8::10": {}, "10.244.0.2": {}, "10.244.0.3": {}, "2001:db8::30": {}}
	if err != nil || podCount != 3 || !reflect.DeepEqual(got, wantEgress) {
		t.Fatalf("egress addresses=%v active pods=%d err=%v", got, podCount, err)
	}
	got, podCount, err = livePodIPsForSelector(cs, "scope", nil, []v1.IPFamily{v1.IPv4Protocol})
	if err != nil || podCount != 0 || len(got) != 0 {
		t.Fatalf("empty selector must not select the namespace: addresses=%v pods=%d err=%v", got, podCount, err)
	}
	for _, tc := range []struct {
		name, verb, resource string
		call                 func(clientset.Interface) error
	}{
		{"pod list", "list", "pods", func(c clientset.Interface) error {
			_, _, err := livePodIPsWithLabel(c, "scope", egressLabel, "Raw-lb")
			return err
		}},
		{"node list", "list", "nodes", func(c clientset.Interface) error { _, _, err := livePodIPsFor(c, "scope", "web"); return err }},
		{"Service get", "get", "services", func(c clientset.Interface) error { _, _, err := livePodIPsFor(c, "scope", "web"); return err }},
	} {
		t.Run(tc.name+" errors propagate", func(t *testing.T) {
			c := fake.NewSimpleClientset(objects...)
			boom := errors.New("API read failed")
			c.PrependReactor(tc.verb, tc.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, boom
			})
			if err := tc.call(c); !errors.Is(err, boom) {
				t.Fatalf("error=%v, want %v", err, boom)
			}
		})
	}
	t.Run("two host-network pods are not two distinct addresses", func(t *testing.T) {
		a := makePod("a", "scope", "v4", "10.0.0.2", "", true)
		b := makePod("b", "scope", "v4", "10.0.0.2", "", true)
		c := fake.NewSimpleClientset(a, b)
		got, count, err := livePodIPsWithLabel(c, "scope", egressLabel, "Raw-lb")
		if err != nil || count != 2 || !reflect.DeepEqual(got, map[string]struct{}{"10.0.0.2": {}}) {
			t.Fatalf("addresses=%v pods=%d err=%v", got, count, err)
		}
	})
	t.Run("positive pod fixtures cannot pass with an empty eligible set", func(t *testing.T) {
		p := makePod("v4", "scope", "v4", "10.244.0.1", "", true)
		c := fake.NewSimpleClientset(v4, p)
		if _, _, err := livePodIPsForSelector(c, "scope", service.Spec.Selector, []v1.IPFamily{v1.IPv6Protocol}); err == nil {
			t.Fatal("an IPv6 Service with only IPv4 pod/node families must not pass vacuously")
		}
		p.Status.PodIP = "2001:db8::10"
		p.Status.PodIPs = []v1.PodIP{{IP: p.Status.PodIP}}
		c = fake.NewSimpleClientset(p)
		if _, _, err := livePodIPsWithLabel(c, "scope", egressLabel, "Raw-lb"); err == nil {
			t.Fatal("a pod without a matching node-location family must not pass vacuously")
		}
	})
	t.Run("Pending egress is not gated on Ready", func(t *testing.T) {
		p := makePod("pending", "scope", "dual", "10.244.0.1", "2001:db8::10", false)
		p.Status.Phase = v1.PodPending
		c := fake.NewSimpleClientset(p)
		got, count, err := livePodIPsWithLabel(c, "scope", egressLabel, "Raw-lb")
		want := map[string]struct{}{"10.244.0.1": {}, "2001:db8::10": {}}
		if err != nil || count != 1 || !reflect.DeepEqual(got, want) {
			t.Fatalf("addresses=%v pods=%d err=%v", got, count, err)
		}
	})
	for _, count := range []int{8, 10, 20} {
		for _, dualStack := range []bool{false, true} {
			t.Run(fmt.Sprintf("workload-%d/dual=%t", count, dualStack), func(t *testing.T) {
				objects := []runtime.Object{dual, v4, service}
				inbound, outbound := map[string]struct{}{}, map[string]struct{}{}
				for i := 1; i <= count; i++ {
					v4IP, v6IP, nodeName := fmt.Sprintf("10.244.0.%d", i), "", "v4"
					inbound[v4IP], outbound[v4IP] = struct{}{}, struct{}{}
					if dualStack {
						v6IP, nodeName = fmt.Sprintf("2001:db8::%x", i), "dual"
						outbound[v6IP] = struct{}{}
					}
					objects = append(objects, makePod(fmt.Sprintf("pod-%d", i), "scope", nodeName, v4IP, v6IP, true))
				}
				c := fake.NewSimpleClientset(objects...)
				got, pods, err := livePodIPsFor(c, "scope", "web")
				if err != nil || pods != count || !reflect.DeepEqual(got, inbound) {
					t.Fatalf("inbound=%v pods=%d err=%v, want=%v pods=%d", got, pods, err, inbound, count)
				}
				got, pods, err = livePodIPsWithLabel(c, "scope", egressLabel, "Raw-lb")
				if err != nil || pods != count || !reflect.DeepEqual(got, outbound) {
					t.Fatalf("outbound=%v pods=%d err=%v, want=%v pods=%d", got, pods, err, outbound, count)
				}
			})
		}
	}
}

func TestRegisteredAddressSetsMatchErr(t *testing.T) {
	want := map[string]struct{}{"10.244.0.1": {}, "2001:db8::1": {}}
	for _, tc := range []struct {
		name string
		got  map[string]struct{}
		fail bool
	}{
		{"both families", want, false},
		{"missing IPv6", map[string]struct{}{"10.244.0.1": {}}, true},
		{"same count but wrong survivor", map[string]struct{}{"10.244.0.2": {}, "2001:db8::1": {}}, true},
		{"extra stale address", map[string]struct{}{"10.244.0.1": {}, "2001:db8::1": {}, "10.244.0.2": {}}, true},
		{"empty registration", map[string]struct{}{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := registeredAddressSetsMatchErr("team-natgw", tc.got, want); (err != nil) != tc.fail {
				t.Fatalf("error=%v, want failure=%t", err, tc.fail)
			}
		})
	}
}

// eventuallyAzureCleanup polls until the Service Gateway and its address locations are
// free of all test services (only the default outbound service remains). Use it in
// AfterEach in place of a fixed post-delete sleep.
func eventuallyAzureCleanup(timeout time.Duration) {
	Eventually(func() error {
		if err := serviceGatewayCleanupErr(); err != nil {
			return err
		}
		if err := addressLocationsCleanupErr(); err != nil {
			return err
		}
		return egressIPv6PublicIPsCleanedUpErr()
	}, timeout, defaultPollInterval).Should(Succeed(),
		"Service Gateway, address locations and egress Public IPs should be free of test resources after cleanup")
}

// egressIPv6PublicIPsCleanedUpErr returns nil once no IPv6 egress Public IP remains in Azure.
//
// A marked "-natgw-pip-v6" belongs to a managed dual-stack egress NAT Gateway, so
// any survivor is a leak. This is a global check like serviceGatewayCleanupErr: the other cleanup
// assertions only read the ServiceGateway registration, which says nothing about ARM, so without
// this a CCM that deregisters an egress identity but never deletes its IPv6 address passes every
// egress spec while leaking a billable Public IP per identity.
func egressIPv6PublicIPsCleanedUpErr() error {
	pipOutput, err := runAz("network", "public-ip", "list",
		"--resource-group", resourceGroupName,
		"--output", "json")
	if err != nil {
		return fmt.Errorf("failed to query Azure for Public IPs: %w", err)
	}

	var publicIPs []AzurePublicIP
	if err := json.Unmarshal(pipOutput, &publicIPs); err != nil {
		return fmt.Errorf("failed to parse Public IPs response: %w", err)
	}
	leaked := []string{}
	for i := range publicIPs {
		if publicIPs[i].Tags[consts.ServiceGatewayManagedTagKey] == consts.ServiceGatewayManagedTagValue &&
			strings.HasSuffix(publicIPs[i].Name, "-natgw-pip-v6") {
			leaked = append(leaked, publicIPs[i].Name)
		}
	}
	if len(leaked) > 0 {
		return fmt.Errorf("IPv6 egress Public IPs still exist after cleanup (leaked): %v", leaked)
	}
	return nil
}
