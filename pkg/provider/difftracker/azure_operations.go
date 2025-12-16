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
// Error Handling:
// Transient errors (throttling, timeouts) should be retried by callers.
// Permanent errors (authentication, resource not found) should not be retried.
package difftracker

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

// createOrUpdatePIP creates or updates a public IP address
func (dt *DiffTracker) createOrUpdatePIP(ctx context.Context, pipResourceGroup string, pip *armnetwork.PublicIPAddress) error {
	pipName := ptr.Deref(pip.Name, "")
	if pipName == "" {
		return fmt.Errorf("createOrUpdatePIP: pip name is empty")
	}
	klog.Infof("createOrUpdatePIP(%s): start", pipName)

	_, err := dt.networkClientFactory.GetPublicIPAddressClient().CreateOrUpdate(ctx, pipResourceGroup, pipName, *pip)
	klog.V(10).Infof("PublicIPAddressClient.CreateOrUpdate(%s, %s): end", pipResourceGroup, pipName)
	if err != nil {
		klog.Warningf("PublicIPAddressClient.CreateOrUpdate(%s, %s) failed: %s", pipResourceGroup, pipName, err.Error())
		return err
	}

	return nil
}

// deletePublicIP deletes a public IP address
func (dt *DiffTracker) deletePublicIP(ctx context.Context, pipResourceGroup string, pipName string) error {
	if pipName == "" {
		return fmt.Errorf("deletePublicIP: pipName is empty")
	}
	klog.Infof("deletePublicIP(%s): start", pipName)

	err := dt.networkClientFactory.GetPublicIPAddressClient().Delete(ctx, pipResourceGroup, pipName)
	klog.Infof("deletePublicIP(%s): end, error: %v", pipName, err)

	if err != nil {
		klog.Warningf("deletePublicIP(%s) failed: %s", pipName, err.Error())
		return err
	}

	return nil
}

// createOrUpdateLB creates or updates a load balancer
func (dt *DiffTracker) createOrUpdateLB(ctx context.Context, lb armnetwork.LoadBalancer) error {
	lbName := ptr.Deref(lb.Name, "")
	if lbName == "" {
		return fmt.Errorf("createOrUpdateLB: load balancer name is empty")
	}
	klog.Infof("createOrUpdateLB(%s): start", lbName)

	_, err := dt.networkClientFactory.GetLoadBalancerClient().CreateOrUpdate(ctx, dt.config.ResourceGroup, lbName, lb)
	klog.V(10).Infof("LoadBalancerClient.CreateOrUpdate(%s): end", lbName)
	if err != nil {
		klog.Warningf("LoadBalancerClient.CreateOrUpdate(%s) failed: %v", lbName, err)
		return err
	}

	return nil
}

// deleteLB deletes a load balancer by service UID
func (dt *DiffTracker) deleteLB(ctx context.Context, uid string) error {
	// Normalize
	uid = strings.ToLower(uid)

	// Try to retrieve the live Service
	svc, err := dt.getServiceByUID(ctx, uid)
	if err != nil {
		// Service not found - try direct cleanup
		klog.V(3).Infof("deleteLB: service uid %s not found, attempting direct LoadBalancer deletion", uid)

		// Delete the load balancer directly by name (uid is the LB name)
		if err := dt.networkClientFactory.GetLoadBalancerClient().Delete(ctx, dt.config.ResourceGroup, uid); err != nil {
			klog.Warningf("deleteLB: failed to delete LoadBalancer %s directly: %v", uid, err)
			return err
		}

		klog.V(3).Infof("deleteLB: successfully deleted LoadBalancer %s directly", uid)
		return nil
	}

	// Service exists - use standard deletion
	if err := dt.networkClientFactory.GetLoadBalancerClient().Delete(ctx, dt.config.ResourceGroup, uid); err != nil {
		return fmt.Errorf("deleteLB: failed to delete LoadBalancer for service %s/%s (uid=%s): %w",
			svc.Namespace, svc.Name, uid, err)
	}

	return nil
}

// createOrUpdateNatGateway creates or updates a NAT gateway
func (dt *DiffTracker) createOrUpdateNatGateway(ctx context.Context, natGatewayResourceGroup string, natGateway armnetwork.NatGateway) error {
	natGatewayName := ptr.Deref(natGateway.Name, "")
	if natGatewayName == "" {
		return fmt.Errorf("createOrUpdateNatGateway: NAT gateway name is empty")
	}
	klog.Infof("createOrUpdateNatGateway(%s): start", natGatewayName)

	_, err := dt.networkClientFactory.GetNatGatewayClient().CreateOrUpdate(ctx, natGatewayResourceGroup, natGatewayName, natGateway)
	if err != nil {
		klog.Warningf("NatGatewayClient.CreateOrUpdate(%s) failed: %v", natGatewayName, err)
		return err
	}

	klog.V(10).Infof("NatGatewayClient.CreateOrUpdate(%s): success", natGatewayName)
	klog.Infof("createOrUpdateNatGateway(%s): end, error: nil", natGatewayName)
	return nil
}

// deleteNatGateway deletes a NAT gateway
func (dt *DiffTracker) deleteNatGateway(ctx context.Context, natGatewayResourceGroup string, natGatewayName string) error {
	if natGatewayName == "" {
		return fmt.Errorf("deleteNatGateway: NAT gateway name is empty")
	}
	klog.Infof("deleteNatGateway(%s) in resource group %s: start", natGatewayName, natGatewayResourceGroup)

	err := dt.networkClientFactory.GetNatGatewayClient().Delete(ctx, natGatewayResourceGroup, natGatewayName)
	if err != nil {
		klog.Errorf("NatGatewayClient.Delete(%s) in resource group %s failed: %v", natGatewayName, natGatewayResourceGroup, err)
		return err
	}

	klog.V(10).Infof("NatGatewayClient.Delete(%s) in resource group %s: success", natGatewayName, natGatewayResourceGroup)
	klog.Infof("deleteNatGateway(%s) in resource group %s: end, error: nil", natGatewayName, natGatewayResourceGroup)
	return nil
}

// disassociateNatGatewayFromServiceGateway removes the NAT gateway association from the Service Gateway
// This should be called before deleting the NAT gateway to properly clean up the references
func (dt *DiffTracker) disassociateNatGatewayFromServiceGateway(ctx context.Context, serviceGatewayName string, natGatewayName string) error {
	klog.Infof("disassociateNatGatewayFromServiceGateway: Disassociating NAT Gateway %s from Service Gateway %s in resource group %s", natGatewayName, serviceGatewayName, dt.config.ResourceGroup)

	// Step 1: Get the service and remove the NAT gateway reference
	services, err := dt.networkClientFactory.GetServiceGatewayClient().GetServices(ctx, dt.config.ResourceGroup, serviceGatewayName)
	if err != nil {
		klog.Errorf("disassociateNatGatewayFromServiceGateway: Failed to get Service Gateway %s: %v", serviceGatewayName, err)
		return fmt.Errorf("failed to get Service Gateway services: %w", err)
	}

	var serviceToBeUpdated *armnetwork.ServiceGatewayService
	for _, service := range services {
		if service.Name != nil && *service.Name == natGatewayName {
			serviceToBeUpdated = service
			break
		}
	}

	if serviceToBeUpdated == nil {
		klog.Infof("disassociateNatGatewayFromServiceGateway: NAT Gateway %s is not associated with Service Gateway %s", natGatewayName, serviceGatewayName)
		return nil
	}

	// Remove the NAT gateway reference from the service
	if serviceToBeUpdated.Properties != nil {
		serviceToBeUpdated.Properties.PublicNatGatewayID = nil
	}

	updateServicesRequest := armnetwork.ServiceGatewayUpdateServicesRequest{
		Action: ptr.To(armnetwork.ServiceGatewayUpdateServicesRequestActionPartialUpdate),
		ServiceRequests: []*armnetwork.ServiceGatewayServiceRequest{
			{
				IsDelete: ptr.To(false),
				Service:  serviceToBeUpdated,
			},
		},
	}

	err = dt.networkClientFactory.GetServiceGatewayClient().UpdateServices(ctx, dt.config.ResourceGroup, serviceGatewayName, updateServicesRequest)
	if err != nil {
		klog.Errorf("disassociateNatGatewayFromServiceGateway: Failed to update Service Gateway %s to disassociate NAT Gateway %s: %v", serviceGatewayName, natGatewayName, err)
		return fmt.Errorf("failed to update Service Gateway: %w", err)
	}
	klog.Infof("disassociateNatGatewayFromServiceGateway: Successfully removed NAT Gateway %s reference from Service Gateway %s", natGatewayName, serviceGatewayName)

	// Step 2: Get the NAT gateway and remove the service gateway reference
	natGateway, err := dt.networkClientFactory.GetNatGatewayClient().Get(ctx, dt.config.ResourceGroup, natGatewayName, nil)
	if err != nil {
		klog.Errorf("disassociateNatGatewayFromServiceGateway: Failed to get NAT Gateway %s: %v", natGatewayName, err)
		return fmt.Errorf("failed to get NAT Gateway: %w", err)
	}

	if natGateway.Properties != nil {
		natGateway.Properties.ServiceGateway = nil
	}

	_, err = dt.networkClientFactory.GetNatGatewayClient().CreateOrUpdate(ctx, dt.config.ResourceGroup, natGatewayName, *natGateway)
	if err != nil {
		klog.Errorf("disassociateNatGatewayFromServiceGateway: Failed to update NAT Gateway %s to remove Service Gateway reference: %v", natGatewayName, err)
		return fmt.Errorf("failed to update NAT Gateway: %w", err)
	}

	klog.Infof("disassociateNatGatewayFromServiceGateway: Successfully disassociated NAT Gateway %s from Service Gateway %s in resource group %s", natGatewayName, serviceGatewayName, dt.config.ResourceGroup)
	return nil
}

// updateNRPSGWServices updates services in the Service Gateway
func (dt *DiffTracker) updateNRPSGWServices(ctx context.Context, serviceGatewayName string, updateServicesRequestDTO ServicesDataDTO) error {
	// Early return if no services to update
	if len(updateServicesRequestDTO.Services) == 0 {
		klog.Infof("updateNRPSGWServices(%s): no services to update", serviceGatewayName)
		return nil
	}

	klog.Infof("updateNRPSGWServices(%s): start", serviceGatewayName)

	// Convert DTO to ARM SDK request
	req := armnetwork.ServiceGatewayUpdateServicesRequest{
		Action:          convertServicesUpdateActionToARM(updateServicesRequestDTO.Action),
		ServiceRequests: convertServiceDTOsToServiceRequests(updateServicesRequestDTO.Services, dt.config),
	}

	err := dt.networkClientFactory.GetServiceGatewayClient().UpdateServices(ctx, dt.config.ResourceGroup, serviceGatewayName, req)
	if err != nil {
		klog.Warningf("ServiceGatewayClient.UpdateServices(%s) failed: %v", serviceGatewayName, err)
		return err
	}

	klog.V(10).Infof("ServiceGatewayClient.UpdateServices(%s): success", serviceGatewayName)
	klog.Infof("updateNRPSGWServices(%s): end, error: nil", serviceGatewayName)
	return nil
}

// updateNRPSGWAddressLocations updates address locations in the Service Gateway
func (dt *DiffTracker) updateNRPSGWAddressLocations(ctx context.Context, serviceGatewayName string, locationsDTO LocationsDataDTO) error {
	klog.Infof("updateNRPSGWAddressLocations(%s): start", serviceGatewayName)

	// Convert DTO to ARM SDK request
	req := armnetwork.ServiceGatewayUpdateAddressLocationsRequest{
		Action:           convertLocationsUpdateActionToARM(locationsDTO.Action),
		AddressLocations: convertLocationDTOsToAddressLocations(locationsDTO.Locations),
	}

	err := dt.networkClientFactory.GetServiceGatewayClient().UpdateAddressLocations(ctx, dt.config.ResourceGroup, serviceGatewayName, req)
	if err != nil {
		klog.Warningf("ServiceGatewayClient.UpdateAddressLocations(%s) failed: %v", serviceGatewayName, err)
		return err
	}

	klog.V(10).Infof("ServiceGatewayClient.UpdateAddressLocations(%s): success", serviceGatewayName)
	klog.Infof("updateNRPSGWAddressLocations(%s): end, error: nil", serviceGatewayName)
	return nil
}

// getServiceByUID returns the Service whose UID matches the given uid
func (dt *DiffTracker) getServiceByUID(ctx context.Context, uid string) (*v1.Service, error) {
	// list via client (could be expensive; acceptable for initialization)
	svcList, err := dt.kubeClient.CoreV1().Services(v1.NamespaceAll).List(ctx, metav1.ListOptions{})
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

// Helper functions to convert DTOs to ARM SDK types

func convertServicesUpdateActionToARM(action UpdateAction) *armnetwork.ServiceGatewayUpdateServicesRequestAction {
	switch action {
	case PartialUpdate:
		return ptr.To(armnetwork.ServiceGatewayUpdateServicesRequestActionPartialUpdate)
	case FullUpdate:
		return ptr.To(armnetwork.ServiceGatewayUpdateServicesRequestActionFullUpdate)
	default:
		return ptr.To(armnetwork.ServiceGatewayUpdateServicesRequestActionPartialUpdate)
	}
}

func convertLocationsUpdateActionToARM(action UpdateAction) *armnetwork.ServiceGatewayUpdateAddressLocationsRequestAction {
	switch action {
	case PartialUpdate:
		return ptr.To(armnetwork.ServiceGatewayUpdateAddressLocationsRequestActionPartialUpdate)
	case FullUpdate:
		return ptr.To(armnetwork.ServiceGatewayUpdateAddressLocationsRequestActionFullUpdate)
	default:
		return ptr.To(armnetwork.ServiceGatewayUpdateAddressLocationsRequestActionPartialUpdate)
	}
}

// extractResourceChildName extracts a child resource name from an Azure resource ID
func extractResourceChildName(id, segment string) string {
	if id == "" {
		return ""
	}
	parts := strings.Split(id, "/")
	// Look for the explicit segment first
	for i := 0; i < len(parts)-1; i++ {
		if strings.EqualFold(parts[i], segment) && parts[i+1] != "" {
			return parts[i+1]
		}
	}
	// Fallback: last non-empty
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] != "" {
			return parts[i]
		}
	}
	return ""
}

func convertServiceDTOsToServiceRequests(services []ServiceDTO, config Config) []*armnetwork.ServiceGatewayServiceRequest {
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
		var serviceType armnetwork.ServiceGatewayServicePropertiesFormatServiceType
		var publicNatGatewayID *string
		switch svc.ServiceType {
		case Inbound:
			serviceType = armnetwork.ServiceGatewayServicePropertiesFormatServiceTypeInbound
			publicNatGatewayID = nil
		case Outbound:
			serviceType = armnetwork.ServiceGatewayServicePropertiesFormatServiceTypeOutbound
			publicNatGatewayID = &svc.PublicNatGateway.Id
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
	return serviceRequests
}

func convertLocationDTOsToAddressLocations(locations []LocationDTO) []*armnetwork.ServiceGatewayAddressLocation {
	armLocations := make([]*armnetwork.ServiceGatewayAddressLocation, 0, len(locations))
	for _, loc := range locations {
		armLoc := &armnetwork.ServiceGatewayAddressLocation{
			AddressLocation: ptr.To(loc.Location),
		}

		// Set address update action
		switch loc.AddressUpdateAction {
		case PartialUpdate:
			armLoc.AddressUpdateAction = ptr.To(armnetwork.ServiceGatewayAddressLocationAddressUpdateActionPartialUpdate)
		case FullUpdate:
			armLoc.AddressUpdateAction = ptr.To(armnetwork.ServiceGatewayAddressLocationAddressUpdateActionFullUpdate)
		}

		// Convert addresses
		if len(loc.Addresses) > 0 {
			armLoc.Addresses = make([]*armnetwork.ServiceGatewayAddress, 0, len(loc.Addresses))
			for _, addr := range loc.Addresses {
				armAddr := &armnetwork.ServiceGatewayAddress{
					Address: ptr.To(addr.Address),
				}

				// Convert service names
				if addr.ServiceNames != nil && addr.ServiceNames.Len() > 0 {
					armAddr.Services = make([]*string, 0, addr.ServiceNames.Len())
					for _, svcName := range addr.ServiceNames.UnsortedList() {
						armAddr.Services = append(armAddr.Services, ptr.To(svcName))
					}
				}

				armLoc.Addresses = append(armLoc.Addresses, armAddr)
			}
		}

		armLocations = append(armLocations, armLoc)
	}
	return armLocations
}
