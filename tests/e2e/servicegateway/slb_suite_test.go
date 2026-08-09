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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"

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

// Helper functions for SLB tests

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
		"Please run 'az login'",
		"AADSTS700082", // expired refresh token; the next attempt re-authenticates
	} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
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

	sgResponse, err := queryServiceGatewayServices()
	Expect(err).NotTo(HaveOccurred(), "Should be able to query Service Gateway services after cleanup")

	utils.Logf("Found %d service(s) in Service Gateway after cleanup", len(sgResponse.Value))
	foundDefault := false
	for i := range sgResponse.Value {
		svc := &sgResponse.Value[i]
		utils.Logf("  Service: %s (Type: %s)", svc.Name, svc.Properties.ServiceType)

		if svc.Name != "default-natgw" {
			Fail(fmt.Sprintf("Unexpected service '%s' still exists in Service Gateway after cleanup", svc.Name))
		}
		foundDefault = true
		Expect(svc.Properties.ServiceType).To(Equal("Outbound"), "Service should be the default outbound service")
	}
	// The loop above only rejects *extra* services, so it passes vacuously on an empty response -
	// which is not cleanup succeeding but the cluster's default egress path having been destroyed.
	// The default NAT gateway is not created by these tests and must outlive every one of them.
	Expect(foundDefault).To(BeTrue(),
		"the default outbound service 'default-natgw' is missing from the Service Gateway; cleanup must leave it in place")
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

// serviceGatewayCleanupErr returns nil only when the Service Gateway contains just the
// default outbound service (i.e. all test services have been cleaned up). It is the
// error-returning core used to poll for cleanup via Eventually, so a spec waits exactly
// as long as Azure actually needs instead of a fixed, conservative sleep.
func serviceGatewayCleanupErr() error {
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return fmt.Errorf("query Service Gateway services: %w", err)
	}
	foundDefault := false
	for i := range sgResponse.Value {
		svc := &sgResponse.Value[i]
		if svc.Name != "default-natgw" {
			return fmt.Errorf("unexpected service %q (type %s) still exists in Service Gateway after cleanup", svc.Name, svc.Properties.ServiceType)
		}
		foundDefault = true
	}
	// Without this an empty Service Gateway satisfies the poll immediately: the loop finds no
	// unexpected service because it finds no service at all. Cleanup must remove the test's
	// services and leave the cluster-wide default outbound service untouched.
	if !foundDefault {
		return fmt.Errorf("the default outbound service %q is missing from the Service Gateway; cleanup must leave it in place", "default-natgw")
	}
	return nil
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
			if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
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
	return azurePublicIPNamedAbsentErr(fmt.Sprintf("%s-pip", serviceUID))
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
		if loadBalancers[i].Name == serviceUID {
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
// The NAT Gateway is named after the egress identity. Its IPv4 Public IP follows the same
// <name>-pip convention as the inbound path, and a dual-stack cluster adds <name>-pip-v6.
// Without this check a CCM that removes the Outbound registration from the ServiceGateway but
// never deletes the ARM resources passes every egress teardown spec while leaking a NAT Gateway
// and a Public IP per egress identity.
func azureEgressResourcesAbsentErr(egressName string) error {
	natOutput, err := runAz("network", "nat", "gateway", "show",
		"--resource-group", resourceGroupName,
		"--name", egressName,
		"--output", "json")
	if err == nil {
		return fmt.Errorf("NAT Gateway %s still exists in Azure after teardown (leaked)", egressName)
	}
	text := string(natOutput)
	if !strings.Contains(text, "not found") && !strings.Contains(text, "NotFound") {
		return fmt.Errorf("could not determine whether NAT Gateway %s was deleted: %s", egressName, text)
	}
	if err := azurePublicIPAbsentErr(egressName); err != nil {
		return err
	}
	// A dual-stack cluster also gets an IPv6 address, named "<name>-pip-v6". It is provisioned by
	// the same create and removed by the same delete, so a teardown that misses it leaks a second
	// billable address per egress identity.
	return azurePublicIPNamedAbsentErr(fmt.Sprintf("%s-pip-v6", egressName))
}

// verifyAzureResources verifies Public IP, Load Balancer, and Service Gateway for a given service
func verifyAzureResources(serviceUID string) error {
	publicIPName := fmt.Sprintf("%s-pip", serviceUID)
	loadBalancerName := serviceUID

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
// for the given service/egress identifier. It works for both inbound services and outbound egress,
// since both are referenced by identifier in an address's Services list.
//
// Two properties matter for callers:
//
//   - It counts ADDRESSES, not pods. A single-stack pod contributes one address, so a pod count and
//     an address count coincide; a dual-stack pod registers one address per IP family and therefore
//     contributes two. Callers that want to express an expectation in pods on a dual-stack cluster
//     must use expectedAddressesForPods rather than the raw pod count.
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
// the given service/egress identifier.
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

// podIPSet collects every IP of the given pods, which is the address set they should register.
// podIPSet collects the pod IPs of the given pods, skipping any pod that is already terminating.
// A pod keeps its IP in a List response until it is fully gone, so counting it after a scale-down
// yields one address more than the workload actually has and makes an exact-set comparison against
// the Service Gateway fail against a set the data path has correctly stopped using.
func podIPSet(pods []v1.Pod) map[string]struct{} {
	set := make(map[string]struct{})
	for i := range pods {
		if pods[i].DeletionTimestamp != nil {
			continue
		}
		for _, ip := range pods[i].Status.PodIPs {
			if ip.IP != "" {
				set[ip.IP] = struct{}{}
			}
		}
	}
	return set
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
	got, err := countRegisteredEndpoints(serviceUID)
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
	return registeredAddressesMatchErr(serviceUID, wantAddrs)
}

// egressRegisteredMatchErr is the set-based sibling of egressRegisteredErr: the egress service must
// exist with a NAT Gateway, and exactly wantAddrs must be registered for it.
func egressRegisteredMatchErr(egressName string, wantAddrs map[string]struct{}) error {
	if err := egressRegisteredErr(egressName, -1); err != nil {
		return err
	}
	return registeredAddressesMatchErr(egressName, wantAddrs)
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
		if svc.Name == serviceUID {
			return fmt.Errorf("service %s still registered in Service Gateway", serviceUID)
		}
	}
	got, err := countRegisteredEndpoints(serviceUID)
	if err != nil {
		return err
	}
	if got > 0 {
		return fmt.Errorf("service %s still has %d registered endpoint(s)", serviceUID, got)
	}
	return azureInboundResourcesAbsentErr(serviceUID)
}

// egressRegisteredErr returns nil once the named egress (outbound) service exists in
// the Service Gateway with a NAT Gateway and, when wantPods >= 0, exactly wantPods pod
// IPs registered for it. Pass a negative wantPods to skip the pod-count assertion.
func egressRegisteredErr(egressName string, wantPods int) error {
	sgResponse, err := queryServiceGatewayServices()
	if err != nil {
		return fmt.Errorf("query Service Gateway services: %w", err)
	}
	found := false
	for _, svc := range sgResponse.Value {
		if svc.Properties.ServiceType == "Outbound" && svc.Name == egressName {
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
	if wantPods < 0 {
		return nil
	}
	got, err := countRegisteredEndpoints(egressName)
	if err != nil {
		return err
	}
	if got != wantPods {
		return fmt.Errorf("egress %s has %d registered pod(s), want %d", egressName, got, wantPods)
	}
	return nil
}

// eventuallyServiceReconciled polls until the inbound service is fully reconciled in
// Azure and the Service Gateway. Pass a negative wantEndpoints to skip the count check.
func eventuallyServiceReconciled(serviceUID string, wantEndpoints int, timeout time.Duration) {
	Eventually(func() error {
		return serviceReconciledErr(serviceUID, wantEndpoints)
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
// Gateway and registered pods. Pass a negative wantPods to skip the count check.
func eventuallyEgressRegistered(egressName string, wantPods int, timeout time.Duration) {
	Eventually(func() error {
		return egressRegisteredErr(egressName, wantPods)
	}, timeout, defaultPollInterval).Should(Succeed(),
		"egress %s should be registered with %d pod(s) in the Service Gateway", egressName, wantPods)
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
// The "-pip-v6" suffix is created only by this controller, for a dual-stack egress NAT Gateway, so
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
		if strings.HasSuffix(publicIPs[i].Name, "-pip-v6") {
			leaked = append(leaked, publicIPs[i].Name)
		}
	}
	if len(leaked) > 0 {
		return fmt.Errorf("IPv6 egress Public IPs still exist after cleanup (leaked): %v", leaked)
	}
	return nil
}
