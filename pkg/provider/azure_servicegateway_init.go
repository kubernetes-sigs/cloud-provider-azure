package provider

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// TODO(enechitoaia): remove after added aks-rp support
func (az *Cloud) attachServiceGatewayToSubnet(ctx context.Context) error {
	klog.Infof("Attaching Service Gateway %s to subnet in VNet %s", consts.DefaultServiceGatewayResourceName, az.VnetName)
	subnetName := "aks-subnet"

	subnet, err := az.NetworkClientFactory.GetSubnetClient().Get(ctx, az.ResourceGroup, az.VnetName, subnetName, nil)
	if err != nil {
		return fmt.Errorf("failed to get subnet: %w", err)
	}

	// Ensure subnet.Properties and ServiceGateway are initialized
	if subnet.Properties == nil {
		subnet.Properties = &armnetwork.SubnetPropertiesFormat{}
	}
	if subnet.Properties.ServiceGateway == nil {
		subnet.Properties.ServiceGateway = &armnetwork.ServiceGatewaySubnetPropertiesFormat{}
	}

	subnet.Properties.ServiceGateway.ID = to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/serviceGateways/%s",
		az.SubscriptionID, az.ResourceGroup, consts.DefaultServiceGatewayResourceName))

	_, err = az.NetworkClientFactory.GetSubnetClient().CreateOrUpdate(ctx, az.ResourceGroup, az.VnetName, subnetName, *subnet)
	if err != nil {
		return fmt.Errorf("failed to attach Service Gateway to subnet: %w", err)
	}

	klog.Infof("Successfully attached Service Gateway %s to subnet in VNet %s", consts.DefaultServiceGatewayResourceName, az.VnetName)
	return nil
}

// TODO(enechitoaia): remove after added aks-rp support
func (az *Cloud) ensureDefaultOutboundServiceExists(ctx context.Context) error {
	klog.Infof("ensureDefaultOutboundServiceExists: Ensuring default outbound service exists in Service Gateway %s", consts.DefaultServiceGatewayResourceName)

	// createOrUpdate pip
	pipResourceName := "default-natgw-pip"
	pipResource := armnetwork.PublicIPAddress{
		Name: to.Ptr(pipResourceName),
		ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
			az.SubscriptionID, az.ResourceGroup, pipResourceName)),
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandardV2),
		},
		Location: to.Ptr(az.Location),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{ // TODO (enechitoaia): What properties should we use for the Public IP
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
	}
	az.CreateOrUpdatePIPOutbound(ctx, az.ResourceGroup, &pipResource)

	// createOrUpdate nat gateway
	defaultNatGatewayName := "default-natgw"
	natGatewayResource := armnetwork.NatGateway{
		Name: to.Ptr(defaultNatGatewayName),
		ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/natGateways/%s",
			az.SubscriptionID, az.ResourceGroup, defaultNatGatewayName)),
		SKU: &armnetwork.NatGatewaySKU{
			Name: to.Ptr(armnetwork.NatGatewaySKUNameStandardV2)},
		Location: to.Ptr(az.Location),
		Properties: &armnetwork.NatGatewayPropertiesFormat{
			ServiceGateway: &armnetwork.ServiceGateway{
				ID: to.Ptr(az.GetServiceGatewayID()),
			},
			PublicIPAddresses: []*armnetwork.SubResource{
				{
					ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
						az.SubscriptionID, az.ResourceGroup, pipResourceName)),
				},
			},
		},
	}
	az.createOrUpdateNatGateway(ctx, az.ResourceGroup, natGatewayResource)

	servicesDTO, err := az.GetServices(ctx, consts.DefaultServiceGatewayResourceName)
	if err != nil {
		return fmt.Errorf("ensureDefaultOutboundServiceExists: failed to get services from ServiceGateway API: %w", err)
	}
	serviceExists := false
	for _, service := range servicesDTO {
		if *service.Name == "default-natgw" && service.Properties != nil && service.Properties.IsDefault != nil && *service.Properties.IsDefault {
			klog.Infof("ensureDefaultOutboundServiceExists: Default outbound service already exists in Service Gateway %s", consts.DefaultServiceGatewayResourceName)
			serviceExists = true
			break
		}
	}

	if !serviceExists {
		klog.Infof("ensureDefaultOutboundServiceExists: Creating default outbound service in Service Gateway %s", consts.DefaultServiceGatewayResourceName)

		req := armnetwork.ServiceGatewayUpdateServicesRequest{
			Action:          to.Ptr(armnetwork.ServiceGatewayUpdateServicesRequestActionPartialUpdate),
			ServiceRequests: []*armnetwork.ServiceGatewayServiceRequest{},
		}
		req.ServiceRequests = append(req.ServiceRequests, &armnetwork.ServiceGatewayServiceRequest{
			IsDelete: to.Ptr(false),
			Service: &armnetwork.ServiceGatewayService{
				Name: &defaultNatGatewayName,
				Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{
					ServiceType: to.Ptr(armnetwork.ServiceGatewayServicePropertiesFormatServiceTypeOutbound),
					IsDefault:   to.Ptr(true),
					PublicNatGatewayID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/natGateways/%s",
						az.SubscriptionID, az.ResourceGroup, defaultNatGatewayName)),
				},
			},
		})
		err := az.UpdateServices(ctx, consts.DefaultServiceGatewayResourceName, req)
		if err != nil {
			return fmt.Errorf("ensureDefaultOutboundServiceExists: failed to create default outbound service in ServiceGateway API: %w", err)
		}
		klog.Infof("ensureDefaultOutboundServiceExists: Successfully created default outbound service in Service Gateway %s", consts.DefaultServiceGatewayResourceName)
	}

	return nil
}
