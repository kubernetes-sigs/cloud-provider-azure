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

// updateNRPSGWServices updates services in the Service Gateway
func (dt *DiffTracker) updateNRPSGWServices(ctx context.Context, serviceGatewayName string, updateServicesRequestDTO ServicesDataDTO) error {
	klog.Infof("updateNRPSGWServices(%s): start", serviceGatewayName)

	// Convert DTO to ARM SDK request
	req := armnetwork.ServiceGatewayUpdateServicesRequest{
		Action:          convertServicesUpdateActionToARM(updateServicesRequestDTO.Action),
		ServiceRequests: convertServiceDTOsToServiceRequests(updateServicesRequestDTO.Services),
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

func convertServiceDTOsToServiceRequests(services []ServiceDTO) []*armnetwork.ServiceGatewayServiceRequest {
	serviceRequests := make([]*armnetwork.ServiceGatewayServiceRequest, 0, len(services))
	for _, svc := range services {
		// Build the ServiceGatewayService
		armSvc := &armnetwork.ServiceGatewayService{
			Name:       ptr.To(svc.Service),
			Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{},
		}

		// Set service type in properties
		switch svc.ServiceType {
		case Inbound:
			armSvc.Properties.ServiceType = ptr.To(armnetwork.ServiceGatewayServicePropertiesFormatServiceTypeInbound)
		case Outbound:
			armSvc.Properties.ServiceType = ptr.To(armnetwork.ServiceGatewayServicePropertiesFormatServiceTypeOutbound)
		}

		// Set LoadBalancer backend pools
		if len(svc.LoadBalancerBackendPools) > 0 {
			armSvc.Properties.LoadBalancerBackendPools = make([]*armnetwork.BackendAddressPool, 0, len(svc.LoadBalancerBackendPools))
			for _, pool := range svc.LoadBalancerBackendPools {
				armSvc.Properties.LoadBalancerBackendPools = append(armSvc.Properties.LoadBalancerBackendPools, &armnetwork.BackendAddressPool{
					ID: ptr.To(pool.Id),
				})
			}
		}

		// Set NAT Gateway
		if svc.PublicNatGateway.Id != "" {
			armSvc.Properties.PublicNatGatewayID = ptr.To(svc.PublicNatGateway.Id)
		}

		// Wrap in ServiceGatewayServiceRequest
		serviceRequest := &armnetwork.ServiceGatewayServiceRequest{
			Service: armSvc,
		}

		// Set IsDelete flag on the request
		if svc.IsDelete {
			serviceRequest.IsDelete = ptr.To(true)
		}

		serviceRequests = append(serviceRequests, serviceRequest)
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
