package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/difftracker"
)

func (az *Cloud) existsServiceGateway(ctx context.Context, serviceGatewayName string) (bool, error) {
	_, err := az.GetServiceGateway(ctx, serviceGatewayName)
	if err != nil {
		if strings.Contains(err.Error(), consts.ResourceNotFoundMessageCode) {
			return false, nil
		}
		klog.Infof("ExistsServiceGateway: error checking existence of Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return false, err
	}
	return true, nil
}

func (az *Cloud) createServiceGateway(ctx context.Context, serviceGatewayName string) error {
	// Create the service gateway if it does not exist.
	serviceGateway := armnetwork.ServiceGateway{
		Location: to.Ptr(az.Location),
		SKU: &armnetwork.ServiceGatewaySKU{
			Name: to.Ptr(armnetwork.ServiceGatewaySKUNameStandard),
			Tier: to.Ptr(armnetwork.ServiceGatewaySKUTierRegional),
		},
		Properties: &armnetwork.ServiceGatewayPropertiesFormat{
			VirtualNetwork: &armnetwork.VirtualNetwork{ID: to.Ptr(fmt.Sprintf(
				"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s",
				az.SubscriptionID,
				az.ResourceGroup,
				az.VnetName,
			))},
		},
	}
	// logObject(serviceGateway)
	err := az.CreateOrUpdateServiceGateway(ctx, serviceGatewayName, serviceGateway)
	if err != nil {
		klog.Infof("createServiceGateway: error creating Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return fmt.Errorf("InitializeCloudFromConfig: failed to create Service Gateway %s: %w", serviceGatewayName, err)
	}
	klog.Infof("createServiceGateway: successfully created Service Gateway %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return nil
}

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

func (az *Cloud) UpdateNRPSGWServices(ctx context.Context, serviceGatewayName string, updateServicesRequestDTO difftracker.ServicesDataDTO) error {
	if len(updateServicesRequestDTO.Services) == 0 {
		klog.Infof("UpdateNRPSGWServices: no services to update for NRP service gateway %s in resource group %s", serviceGatewayName, az.ResourceGroup)
		return nil
	}
	klog.V(2).Infof("Updating NRP service gateway services for %s in resource group %s", serviceGatewayName, az.ResourceGroup)

	var action armnetwork.ServiceGatewayUpdateServicesRequestAction
	switch updateServicesRequestDTO.Action {
	case difftracker.FullUpdate:
		action = armnetwork.ServiceGatewayUpdateServicesRequestActionFullUpdate
	case difftracker.PartialUpdate:
		action = armnetwork.ServiceGatewayUpdateServicesRequestActionPartialUpdate
	}

	req := armnetwork.ServiceGatewayUpdateServicesRequest{
		Action:          &action,
		ServiceRequests: []*armnetwork.ServiceGatewayServiceRequest{},
	}

	for _, service := range updateServicesRequestDTO.Services {
		loadBalancerBackendPools := make([]*armnetwork.BackendAddressPool, len(service.LoadBalancerBackendPools))
		for i := range service.LoadBalancerBackendPools {
			vnetID := az.getVnetResourceID()
			backendPoolResourceID := service.LoadBalancerBackendPools[i].Id
			backendPoolName := extractResourceChildName(backendPoolResourceID, "backendAddressPools")
			loadBalancerBackendPools[i] = &armnetwork.BackendAddressPool{
				ID:   &backendPoolResourceID,
				Name: &backendPoolName,
				Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
					// Location: &az.Location,
					VirtualNetwork: &armnetwork.SubResource{
						ID: &vnetID,
					},
				},
			}
		}

		var serviceType armnetwork.ServiceGatewayServicePropertiesFormatServiceType
		var publicNatGatewayID *string
		switch service.ServiceType {
		case difftracker.Inbound:
			serviceType = armnetwork.ServiceGatewayServicePropertiesFormatServiceTypeInbound
			publicNatGatewayID = nil
		case difftracker.Outbound:
			serviceType = armnetwork.ServiceGatewayServicePropertiesFormatServiceTypeOutbound
			publicNatGatewayID = &service.PublicNatGateway.Id
		}

		req.ServiceRequests = append(req.ServiceRequests, &armnetwork.ServiceGatewayServiceRequest{
			IsDelete: &service.IsDelete,
			Service: &armnetwork.ServiceGatewayService{
				Name: &service.Service,
				Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
					LoadBalancerBackendPools: loadBalancerBackendPools,
					PublicNatGatewayID:       publicNatGatewayID,
					ServiceType:              &serviceType,
				},
			},
		})
	}
	// klog.Infof("CLB-ENECHITOAIA: UpdateNRPSGWServices request: %+v", req)
	// logObject(req)

	err := az.UpdateServices(ctx, serviceGatewayName, req)
	if err != nil {
		klog.Infof("UpdateNRPSGWServices: error updating NRP service gateway services for %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return err
	}

	klog.Infof("UpdateNRPSGWServices: successfully updated NRP service gateway services for %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return nil
}

func (az *Cloud) UpdateNRPSGWAddressLocations(ctx context.Context, serviceGatewayName string, updateAddressLocationsRequestDTO difftracker.LocationsDataDTO) error {
	klog.V(2).Infof("UpdateNRPSGWAddressLocations: Updating NRP service gateway address locations for %s in resource group %s", serviceGatewayName, az.ResourceGroup)

	var action armnetwork.ServiceGatewayUpdateAddressLocationsRequestAction
	switch updateAddressLocationsRequestDTO.Action {
	case difftracker.FullUpdate:
		action = armnetwork.ServiceGatewayUpdateAddressLocationsRequestActionFullUpdate
	case difftracker.PartialUpdate:
		action = armnetwork.ServiceGatewayUpdateAddressLocationsRequestActionPartialUpdate
	}

	req := armnetwork.ServiceGatewayUpdateAddressLocationsRequest{
		Action:           &action,
		AddressLocations: []*armnetwork.ServiceGatewayAddressLocation{},
	}
	for _, location := range updateAddressLocationsRequestDTO.Locations {
		var addressUpdateAction armnetwork.ServiceGatewayAddressLocationAddressUpdateAction
		switch location.AddressUpdateAction {
		case difftracker.FullUpdate:
			addressUpdateAction = armnetwork.ServiceGatewayAddressLocationAddressUpdateActionFullUpdate
		case difftracker.PartialUpdate:
			addressUpdateAction = armnetwork.ServiceGatewayAddressLocationAddressUpdateActionPartialUpdate
		}

		addresses := make([]*armnetwork.ServiceGatewayAddress, len(location.Addresses))
		for i := range location.Addresses {
			serviceNames := location.Addresses[i].ServiceNames.UnsortedList()
			services := make([]*string, len(serviceNames))
			for j := range serviceNames {
				services[j] = &serviceNames[j]
			}
			addresses[i] = &armnetwork.ServiceGatewayAddress{
				Address:  &location.Addresses[i].Address,
				Services: services,
			}
		}

		req.AddressLocations = append(req.AddressLocations, &armnetwork.ServiceGatewayAddressLocation{
			AddressLocation:     &location.Location,
			AddressUpdateAction: &addressUpdateAction,
			Addresses:           addresses,
		})
	}
	// klog.Infof("UpdateNRPSGWAddressLocations: request: %+v", req)
	// logObject(req)

	err := az.UpdateAddressLocations(ctx, serviceGatewayName, req)
	if err != nil {
		klog.Infof("UpdateNRPSGWAddressLocations: error updating NRP service gateway address locations for %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return err
	}

	klog.Infof("UpdateNRPSGWAddressLocations: successfully updated NRP service gateway address locations for %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return nil
}

func (az *Cloud) DisassociateNatGatewayFromServiceGateway(ctx context.Context, serviceGatewayName string, natGatewayName string) error {
	klog.Infof("DisassociateNatGatewayFromServiceGateway: Disassociating NAT Gateway %s from Service Gateway %s in resource group %s", natGatewayName, serviceGatewayName, az.ResourceGroup)

	// First get the service and remove the nat gateway reference
	services, err := az.GetServices(ctx, serviceGatewayName)
	if err != nil {
		klog.Errorf("DisassociateNatGatewayFromServiceGateway: Failed to get Service Gateway %s: %v", serviceGatewayName, err)
		return err
	}

	var serviceToBeUpdated *armnetwork.ServiceGatewayService
	for _, service := range services {
		if service.Name != nil && *service.Name == natGatewayName {
			serviceToBeUpdated = service
			break
		}
	}

	if serviceToBeUpdated == nil {
		klog.Infof("DisassociateNatGatewayFromServiceGateway: NAT Gateway %s is not associated with Service Gateway %s", natGatewayName, serviceGatewayName)
		return nil
	}

	if serviceToBeUpdated.Properties != nil {
		serviceToBeUpdated.Properties.PublicNatGatewayID = nil
	}

	updateServicesRequest := armnetwork.ServiceGatewayUpdateServicesRequest{
		Action:          to.Ptr(armnetwork.ServiceGatewayUpdateServicesRequestActionPartialUpdate),
		ServiceRequests: []*armnetwork.ServiceGatewayServiceRequest{},
	}

	updateServicesRequest.ServiceRequests = append(updateServicesRequest.ServiceRequests, &armnetwork.ServiceGatewayServiceRequest{
		IsDelete: to.Ptr(false),
		Service:  serviceToBeUpdated,
	})

	err = az.UpdateServices(ctx, serviceGatewayName, updateServicesRequest)
	if err != nil {
		klog.Errorf("DisassociateNatGatewayFromServiceGateway: Failed to update Service Gateway %s to disassociate NAT Gateway %s: %v", serviceGatewayName, natGatewayName, err)
		return err
	}
	klog.Infof("DisassociateNatGatewayFromServiceGateway: Successfully removed NAT Gateway %s reference from Service Gateway %s", natGatewayName, serviceGatewayName)

	// Now get the NAT gateway and remove the service gateway reference

	natGateway, err := az.getNatGateway(ctx, az.ResourceGroup, natGatewayName)
	if err != nil {
		klog.Errorf("DisassociateNatGatewayFromServiceGateway: Failed to get NAT Gateway %s: %v", natGatewayName, err)
		return err
	}
	natGateway.Properties.ServiceGateway = nil
	err = az.createOrUpdateNatGateway(ctx, az.ResourceGroup, *natGateway)
	if err != nil {
		klog.Errorf("DisassociateNatGatewayFromServiceGateway: Failed to disassociate NAT Gateway %s from Service Gateway %s: %v", natGatewayName, serviceGatewayName, err)
		return err
	}
	klog.Infof("DisassociateNatGatewayFromServiceGateway: Successfully disassociated NAT Gateway %s from Service Gateway %s in resource group %s", natGatewayName, serviceGatewayName, az.ResourceGroup)
	return nil
}
