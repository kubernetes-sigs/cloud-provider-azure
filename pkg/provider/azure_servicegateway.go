package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// TODO(enechitoaia): remove after added aks-rp support
func (az *Cloud) existsServiceGateway(ctx context.Context, serviceGatewayName string) (bool, error) {
	sgw, err := az.GetServiceGateway(ctx, serviceGatewayName)
	if err != nil {
		if strings.Contains(err.Error(), consts.ResourceNotFoundMessageCode) {
			return false, nil
		}
		klog.Infof("ExistsServiceGateway: error checking existence of Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return false, err
	}

	if sgw == nil || sgw.Properties == nil || sgw.Properties.RouteTargetAddress == nil ||
		sgw.Properties.RouteTargetAddress.PrivateIPAllocationMethod == nil ||
		*sgw.Properties.RouteTargetAddress.PrivateIPAllocationMethod != armnetwork.IPAllocationMethodDynamic ||
		sgw.Properties.RouteTargetAddress.Subnet == nil {
		klog.Infof("ExistsServiceGateway: Service Gateway %s in resource group %s is not properly configured", serviceGatewayName, az.ResourceGroup)
		return false, nil
	}

	return true, nil
}

// TODO(enechitoaia): remove after added aks-rp support
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
			RouteTargetAddress: &armnetwork.RouteTargetAddressPropertiesFormat{
				PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
				Subnet: &armnetwork.Subnet{
					ID: to.Ptr(fmt.Sprintf(
						"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/virtualNetworks/%s/subnets/%s",
						az.SubscriptionID,
						az.ResourceGroup,
						az.VnetName,
						az.SubnetName,
					)),
				},
			},
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
