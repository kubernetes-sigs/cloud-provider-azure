/*
Copyright 2026 The Kubernetes Authors.

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

// Package difftracker provides state tracking and synchronization between Kubernetes
// resources and Azure Network Resource Provider (NRP) resources.
//
// Architecture:
//   - DiffTracker: Maintains the desired state (K8s) and actual state (NRP)
//   - ServiceUpdater: Handles service creation/deletion (LoadBalancers, NAT Gateways)
//   - LocationsUpdater: Handles address/location synchronization
//
// Azure Operations:
// All Azure SDK operations in azure_operations.go attempt their action once and return
// errors immediately. Retry logic is the responsibility of the callers (ServiceUpdater,
// LocationsUpdater) which implement appropriate retry strategies based on their use cases.
//
// Concurrency invariant:
// ARM primitives in this file never acquire the DiffTracker mutex (dt.mu); the lock is
// released before any I/O. Lock-guarded state lives in the in-memory files and its helpers
// carry the "Locked" suffix, while the public snapshot methods (GetSync*) take the lock,
// compute the diff, and return it by value so callers act on it lock-free. Enforced by
// TestARMPrimitivesDoNotHoldStateLock.
//
// Error Handling:
// Transient errors (throttling, timeouts) should be retried by callers.
// Permanent errors (authentication, resource not found) should not be retried.
package difftracker

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/util/retry"
	servicehelper "k8s.io/cloud-provider/service/helpers"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/errutils"
)

// createOrUpdatePIP creates or updates a public IP address
func (dt *DiffTracker) createOrUpdatePIP(ctx context.Context, pipResourceGroup string, pip *armnetwork.PublicIPAddress) error {
	_, err := dt.createOrUpdatePIPWithResponse(ctx, pipResourceGroup, pip)
	return err
}

// createOrUpdatePIPWithResponse creates or updates a public IP address and returns the response
// containing the allocated IP address. Use this when you need the IP address after PIP creation.
func (dt *DiffTracker) createOrUpdatePIPWithResponse(ctx context.Context, pipResourceGroup string, pip *armnetwork.PublicIPAddress) (*armnetwork.PublicIPAddress, error) {
	if pip == nil {
		return nil, fmt.Errorf("createOrUpdatePIPWithResponse: pip is nil")
	}
	pipName := ptr.Deref(pip.Name, "")
	if pipName == "" {
		return nil, fmt.Errorf("createOrUpdatePIPWithResponse: pip name is empty")
	}
	dt.logger.V(2).Info("Creating or updating public IP", "pip", pipName, "resourceGroup", pipResourceGroup)

	response, err := dt.networkClientFactory.GetPublicIPAddressClient().CreateOrUpdate(ctx, pipResourceGroup, pipName, *pip)
	if err != nil {
		return nil, fmt.Errorf("creating or updating public IP %q: %w", pipName, err)
	}

	return response, nil
}

// deletePublicIP deletes a public IP address
func (dt *DiffTracker) deletePublicIP(ctx context.Context, pipResourceGroup string, pipName string) error {
	if pipName == "" {
		return fmt.Errorf("deletePublicIP: pipName is empty")
	}
	dt.logger.V(2).Info("Deleting public IP", "pip", pipName, "resourceGroup", pipResourceGroup)

	err := dt.networkClientFactory.GetPublicIPAddressClient().Delete(ctx, pipResourceGroup, pipName)
	if _, err := errutils.CheckResourceExistsFromAzcoreError(err); err != nil {
		return fmt.Errorf("deleting public IP %q: %w", pipName, err)
	}

	return nil
}

// createOrUpdateLB creates or updates a load balancer
func (dt *DiffTracker) createOrUpdateLB(ctx context.Context, lb armnetwork.LoadBalancer) error {
	lbName := ptr.Deref(lb.Name, "")
	if lbName == "" {
		return fmt.Errorf("createOrUpdateLB: load balancer name is empty")
	}
	dt.logger.V(2).Info("Creating or updating LoadBalancer", "loadBalancer", lbName)

	_, err := dt.networkClientFactory.GetLoadBalancerClient().CreateOrUpdate(ctx, dt.config.ResourceGroup, lbName, lb)
	if err != nil {
		return fmt.Errorf("creating or updating LoadBalancer %q: %w", lbName, err)
	}

	return nil
}

// deleteLB deletes a load balancer by service UID
func (dt *DiffTracker) deleteLB(ctx context.Context, uid string) error {
	uid = strings.ToLower(uid)
	dt.logger.V(2).Info("Deleting LoadBalancer", "loadBalancer", uid)

	err := dt.networkClientFactory.GetLoadBalancerClient().Delete(ctx, dt.config.ResourceGroup, uid)
	if _, err := errutils.CheckResourceExistsFromAzcoreError(err); err != nil {
		return fmt.Errorf("deleteLB: failed to delete LoadBalancer (uid=%s): %w", uid, err)
	}

	return nil
}

// createOrUpdateNatGateway creates or updates a NAT gateway
func (dt *DiffTracker) createOrUpdateNatGateway(ctx context.Context, natGatewayResourceGroup string, natGateway armnetwork.NatGateway) error {
	natGatewayName := ptr.Deref(natGateway.Name, "")
	if natGatewayName == "" {
		return fmt.Errorf("createOrUpdateNatGateway: NAT gateway name is empty")
	}
	dt.logger.V(2).Info("Creating or updating NAT Gateway", "natGateway", natGatewayName, "resourceGroup", natGatewayResourceGroup)

	_, err := dt.networkClientFactory.GetNatGatewayClient().CreateOrUpdate(ctx, natGatewayResourceGroup, natGatewayName, natGateway)
	if err != nil {
		return fmt.Errorf("creating or updating NAT Gateway %q: %w", natGatewayName, err)
	}

	return nil
}

// deleteNatGateway deletes a NAT gateway
func (dt *DiffTracker) deleteNatGateway(ctx context.Context, natGatewayResourceGroup string, natGatewayName string) error {
	if natGatewayName == "" {
		return fmt.Errorf("deleteNatGateway: NAT gateway name is empty")
	}
	dt.logger.V(2).Info("Deleting NAT Gateway", "natGateway", natGatewayName, "resourceGroup", natGatewayResourceGroup)

	err := dt.networkClientFactory.GetNatGatewayClient().Delete(ctx, natGatewayResourceGroup, natGatewayName)
	if _, err := errutils.CheckResourceExistsFromAzcoreError(err); err != nil {
		return fmt.Errorf("deleting NAT Gateway %q in resource group %q: %w", natGatewayName, natGatewayResourceGroup, err)
	}

	return nil
}

// disassociateNatGatewayFromServiceGateway removes the NAT gateway association from the Service Gateway
// This should be called before deleting the NAT gateway to properly clean up the references
func (dt *DiffTracker) disassociateNatGatewayFromServiceGateway(ctx context.Context, serviceGatewayName string, natGatewayName string) error {
	dt.logger.V(2).Info("Disassociating NAT Gateway from Service Gateway", "natGateway", natGatewayName, "serviceGateway", serviceGatewayName, "resourceGroup", dt.config.ResourceGroup)

	// Step 1: clear the ServiceGateway-side reference if it still has one.
	services, err := dt.networkClientFactory.GetServiceGatewayClient().GetServices(ctx, dt.config.ResourceGroup, serviceGatewayName)
	if err != nil {
		return fmt.Errorf("getting Service Gateway services: %w", err)
	}

	var serviceToBeUpdated *armnetwork.ServiceGatewayService
	for _, service := range services {
		if service.Name != nil && strings.EqualFold(*service.Name, natGatewayName) {
			serviceToBeUpdated = service
			break
		}
	}

	if serviceToBeUpdated != nil && serviceToBeUpdated.Properties != nil && serviceToBeUpdated.Properties.PublicNatGatewayID != nil {
		serviceToBeUpdated.Properties.PublicNatGatewayID = nil
		updateServicesRequest := armnetwork.ServiceGatewayUpdateServicesRequest{
			Action: ptr.To(armnetwork.ServiceUpdateActionPartialUpdate),
			ServiceRequests: []*armnetwork.ServiceGatewayServiceRequest{
				{
					IsDelete: ptr.To(false),
					Service:  serviceToBeUpdated,
				},
			},
		}
		if err := dt.networkClientFactory.GetServiceGatewayClient().UpdateServices(ctx, dt.config.ResourceGroup, serviceGatewayName, updateServicesRequest); err != nil {
			return fmt.Errorf("updating Service Gateway to disassociate NAT Gateway %q: %w", natGatewayName, err)
		}
	}

	// Step 2: clear the NAT-gateway-side reference if it still points back at the
	// Service Gateway. This runs independently of Step 1 so a retry after a
	// partial failure still reconciles the NAT gateway.
	natGateway, err := dt.networkClientFactory.GetNatGatewayClient().Get(ctx, dt.config.ResourceGroup, natGatewayName, nil)
	if err != nil {
		if exists, cerr := errutils.CheckResourceExistsFromAzcoreError(err); cerr == nil && !exists {
			return nil
		}
		return fmt.Errorf("getting NAT Gateway %q: %w", natGatewayName, err)
	}

	if natGateway.Properties != nil && natGateway.Properties.ServiceGateway != nil {
		natGateway.Properties.ServiceGateway = nil
		if _, err := dt.networkClientFactory.GetNatGatewayClient().CreateOrUpdate(ctx, dt.config.ResourceGroup, natGatewayName, *natGateway); err != nil {
			return fmt.Errorf("updating NAT Gateway %q to remove Service Gateway reference: %w", natGatewayName, err)
		}
	}

	dt.logger.V(5).Info("Disassociated NAT Gateway from Service Gateway", "natGateway", natGatewayName, "serviceGateway", serviceGatewayName, "resourceGroup", dt.config.ResourceGroup)
	return nil
}

// updateNRPSGWServices updates services in the Service Gateway
func (dt *DiffTracker) updateNRPSGWServices(ctx context.Context, serviceGatewayName string, updateServicesRequestDTO ServicesDataDTO) error {
	if len(updateServicesRequestDTO.Services) == 0 && updateServicesRequestDTO.Action != FullUpdate {
		dt.logger.V(5).Info("No services to update", "serviceGateway", serviceGatewayName)
		return nil
	}

	dt.logger.V(2).Info("Updating Service Gateway services", "serviceGateway", serviceGatewayName)

	// Convert DTO to ARM SDK request
	serviceRequests, err := convertServiceDTOsToServiceRequests(updateServicesRequestDTO.Services, dt.config)
	if err != nil {
		return fmt.Errorf("updateNRPSGWServices(%s): %w", serviceGatewayName, err)
	}
	req := armnetwork.ServiceGatewayUpdateServicesRequest{
		Action:          convertServicesUpdateActionToARM(updateServicesRequestDTO.Action),
		ServiceRequests: serviceRequests,
	}

	err = dt.networkClientFactory.GetServiceGatewayClient().UpdateServices(ctx, dt.config.ResourceGroup, serviceGatewayName, req)
	if err != nil {
		return fmt.Errorf("updating Service Gateway services for %q: %w", serviceGatewayName, err)
	}

	return nil
}

// updateNRPSGWAddressLocations updates address locations in the Service Gateway
func (dt *DiffTracker) updateNRPSGWAddressLocations(ctx context.Context, serviceGatewayName string, locationsDTO LocationsDataDTO) error {
	dt.logger.V(2).Info("Updating Service Gateway address locations", "serviceGateway", serviceGatewayName)

	// Convert DTO to ARM SDK request
	req := armnetwork.ServiceGatewayUpdateAddressLocationsRequest{
		Action:           convertLocationsUpdateActionToARM(locationsDTO.Action),
		AddressLocations: convertLocationDTOsToAddressLocations(locationsDTO.Locations),
	}

	err := dt.networkClientFactory.GetServiceGatewayClient().UpdateAddressLocations(ctx, dt.config.ResourceGroup, serviceGatewayName, req)
	if err != nil {
		return fmt.Errorf("updating Service Gateway address locations for %q: %w", serviceGatewayName, err)
	}

	return nil
}

// getServiceByUID returns the Service whose UID matches the given uid
func (dt *DiffTracker) getServiceByUID(ctx context.Context, uid string) (*v1.Service, error) {
	// This lists Services and scans for a UID match, which is expensive. It is
	// acceptable for now since it runs once per service operation (plus conflict retries),
	// not in a hot path. The spec.type field selector narrows the server-side result to
	// LoadBalancer Services only, which is the sole type the difftracker manages.
	// TODO: carry the Service namespace/name into ServiceConfig (EnsureLoadBalancer already
	// holds the *v1.Service) and resolve it through the provider's existing serviceLister
	// (az.serviceLister, already backed by a SharedInformer), i.e. serviceLister.Services(ns).
	// Get(name) with the UID kept only as a consistency check. That makes this an O(1),
	// zero-apiserver, zero-extra-memory cached read instead of a list. Wiring the lister and
	// ServiceConfig into the engine lands in the PR that connects this path.
	svcList, err := dt.kubeClient.CoreV1().Services(v1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.type", string(v1.ServiceTypeLoadBalancer)).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("getServiceByUID: list failed: %w", err)
	}
	for _, svc := range svcList.Items {
		if string(svc.UID) == uid {
			return svc.DeepCopy(), nil
		}
	}
	return nil, fmt.Errorf("service with uid %s not found", uid)
}

// updateServiceLoadBalancerStatus updates the K8s Service status with the LoadBalancer IP address.
// EnsureLoadBalancer returns an empty LoadBalancer status immediately while the difftracker
// engine provisions the PIP, LB and ServiceGateway registration asynchronously in the
// background. This function backfills Service.Status.LoadBalancer.Ingress once the PIP is
// created, since it would otherwise stay empty.
func (dt *DiffTracker) updateServiceLoadBalancerStatus(ctx context.Context, serviceUID string, ip string) error {
	if ip == "" {
		return fmt.Errorf("updateServiceLoadBalancerStatus: ip is empty")
	}
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return fmt.Errorf("updateServiceLoadBalancerStatus: invalid ip %q", ip)
	}
	newIsIPv4 := parsedIP.To4() != nil

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		svc, err := dt.getServiceByUID(ctx, serviceUID)
		if err != nil {
			return fmt.Errorf("updateServiceLoadBalancerStatus: failed to get service: %w", err)
		}

		desired := make([]v1.LoadBalancerIngress, 0, len(svc.Status.LoadBalancer.Ingress)+1)
		newPresent := false
		// Rebuild the ingress list, keeping it dual-stack safe: preserve non-IP (hostname-only)
		// entries and entries of the other IP family untouched, while replacing any stale
		// same-family IP with the new one. If the new IP is already present we keep it and
		// avoid appending a duplicate below.
		for _, ingress := range svc.Status.LoadBalancer.Ingress {
			if ingress.IP == "" {
				desired = append(desired, ingress)
				continue
			}
			ingressIP := net.ParseIP(ingress.IP)
			if ingressIP != nil && (ingressIP.To4() != nil) == newIsIPv4 {
				if ingress.IP == ip {
					newPresent = true
					desired = append(desired, ingress)
				}
				continue
			}
			desired = append(desired, ingress)
		}
		if !newPresent {
			desired = append(desired, v1.LoadBalancerIngress{IP: ip})
		}

		if apiequality.Semantic.DeepEqual(svc.Status.LoadBalancer.Ingress, desired) {
			dt.logger.V(5).Info("Service already has LoadBalancer IP", "namespace", svc.Namespace, "service", svc.Name, "ip", ip)
			return nil
		}

		updated := svc.DeepCopy()
		updated.Status.LoadBalancer.Ingress = desired
		if _, err := servicehelper.PatchService(dt.kubeClient.CoreV1(), svc, updated); err != nil {
			return fmt.Errorf("updateServiceLoadBalancerStatus: failed to patch service: %w", err)
		}

		dt.logger.V(2).Info("Updated Service with LoadBalancer IP", "namespace", svc.Namespace, "service", svc.Name, "ip", ip)
		return nil
	})
}

// Helper functions to convert DTOs to ARM SDK types

func convertServicesUpdateActionToARM(action UpdateAction) *armnetwork.ServiceUpdateAction {
	switch action {
	case PartialUpdate:
		return ptr.To(armnetwork.ServiceUpdateActionPartialUpdate)
	case FullUpdate:
		return ptr.To(armnetwork.ServiceUpdateActionFullUpdate)
	default:
		log.Background().WithName("difftracker").V(4).Info("Unknown service UpdateAction, defaulting to PartialUpdate", "action", action)
		return ptr.To(armnetwork.ServiceUpdateActionPartialUpdate)
	}
}

func convertLocationsUpdateActionToARM(action UpdateAction) *armnetwork.UpdateAction {
	switch action {
	case PartialUpdate:
		return ptr.To(armnetwork.UpdateActionPartialUpdate)
	case FullUpdate:
		return ptr.To(armnetwork.UpdateActionFullUpdate)
	default:
		log.Background().WithName("difftracker").V(4).Info("Unknown locations UpdateAction, defaulting to PartialUpdate", "action", action)
		return ptr.To(armnetwork.UpdateActionPartialUpdate)
	}
}

// extractResourceChildName extracts a child resource name from an Azure resource ID.
// Returns "" if the requested segment is not present.
func extractResourceChildName(id, segment string) string {
	if id == "" {
		return ""
	}
	parts := strings.Split(id, "/")
	for i := 0; i < len(parts)-1; i++ {
		if strings.EqualFold(parts[i], segment) && parts[i+1] != "" {
			return parts[i+1]
		}
	}
	return ""
}

func convertServiceDTOsToServiceRequests(services []ServiceDTO, config Config) ([]*armnetwork.ServiceGatewayServiceRequest, error) {
	serviceRequests := make([]*armnetwork.ServiceGatewayServiceRequest, 0, len(services))

	// Construct VNet resource ID once for all backend pools
	vnetID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s",
		config.SubscriptionID, config.ResourceGroup, config.VNetName)

	for _, svc := range services {
		// Build backend pools with full details
		loadBalancerBackendPools := make([]*armnetwork.BackendAddressPool, len(svc.LoadBalancerBackendPools))
		for i := range svc.LoadBalancerBackendPools {
			backendPoolResourceID := svc.LoadBalancerBackendPools[i].Id
			backendPoolName := extractResourceChildName(backendPoolResourceID, "backendAddressPools")
			loadBalancerBackendPools[i] = &armnetwork.BackendAddressPool{
				ID:   &backendPoolResourceID,
				Name: &backendPoolName,
				Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
					VirtualNetwork: &armnetwork.SubResource{
						ID: &vnetID,
					},
				},
			}
		}

		// Set service type and NAT gateway based on service type
		var serviceType armnetwork.ServiceType
		var publicNatGatewayID *string
		switch svc.ServiceType {
		case Inbound:
			serviceType = armnetwork.ServiceTypeInbound
			publicNatGatewayID = nil
		case Outbound:
			serviceType = armnetwork.ServiceTypeOutbound
			if !svc.IsDelete && svc.PublicNatGateway.Id != "" {
				natID := svc.PublicNatGateway.Id
				publicNatGatewayID = &natID
			}
		default:
			return nil, fmt.Errorf("convertServiceDTOsToServiceRequests: unknown ServiceType %q for service %q", svc.ServiceType, svc.Service)
		}

		// Build and append the service request
		serviceRequests = append(serviceRequests, &armnetwork.ServiceGatewayServiceRequest{
			IsDelete: &svc.IsDelete,
			Service: &armnetwork.ServiceGatewayService{
				Name: &svc.Service,
				Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
					LoadBalancerBackendPools: loadBalancerBackendPools,
					PublicNatGatewayID:       publicNatGatewayID,
					ServiceType:              &serviceType,
				},
			},
		})
	}
	return serviceRequests, nil
}

func convertLocationDTOsToAddressLocations(locations []LocationDTO) []*armnetwork.ServiceGatewayAddressLocation {
	armLocations := make([]*armnetwork.ServiceGatewayAddressLocation, 0, len(locations))
	for _, loc := range locations {
		armLoc := &armnetwork.ServiceGatewayAddressLocation{
			AddressLocation: ptr.To(loc.Location),
		}

		// Set address update action. Mirror the service/location action converters by
		// defaulting an unknown (e.g. unset/zero) value to PartialUpdate instead of
		// leaving it nil, so NRP always receives an explicit action.
		switch loc.AddressUpdateAction {
		case PartialUpdate:
			armLoc.AddressUpdateAction = ptr.To(armnetwork.AddressUpdateActionPartialUpdate)
		case FullUpdate:
			armLoc.AddressUpdateAction = ptr.To(armnetwork.AddressUpdateActionFullUpdate)
		default:
			log.Background().WithName("difftracker").V(4).Info("Unknown AddressUpdateAction, defaulting to PartialUpdate", "action", loc.AddressUpdateAction, "location", loc.Location)
			armLoc.AddressUpdateAction = ptr.To(armnetwork.AddressUpdateActionPartialUpdate)
		}

		// Convert addresses - always initialize the slice to avoid null in JSON
		armLoc.Addresses = make([]*armnetwork.ServiceGatewayAddress, 0, len(loc.Addresses))
		for _, addr := range loc.Addresses {
			armAddr := &armnetwork.ServiceGatewayAddress{
				Address: ptr.To(addr.Address),
			}

			// Convert service names - always initialize the slice to avoid null in JSON
			armAddr.Services = make([]*string, 0, addr.ServiceNames.Len())
			for _, svcName := range addr.ServiceNames.UnsortedList() {
				armAddr.Services = append(armAddr.Services, ptr.To(svcName))
			}

			armLoc.Addresses = append(armLoc.Addresses, armAddr)
		}

		armLocations = append(armLocations, armLoc)
	}
	return armLocations
}
