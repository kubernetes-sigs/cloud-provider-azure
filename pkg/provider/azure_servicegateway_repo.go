package provider

import (
	"context"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func (az *Cloud) CreateOrUpdateServiceGateway(ctx context.Context, serviceGatewayName string, parameters armnetwork.ServiceGateway) error {
	_, err := az.NetworkClientFactory.GetServiceGatewayClient().CreateOrUpdate(ctx, az.ResourceGroup, serviceGatewayName, parameters)
	if err != nil {
		klog.Infof("CLB-ENECHITOAIA: error creating or updating Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return err
	}
	klog.Infof("CLB-ENECHITOAIA: successfully created or updated Service Gateway %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return nil
}

func (az *Cloud) GetServiceGateway(ctx context.Context, serviceGatewayName string) (*armnetwork.ServiceGateway, error) {
	result, err := az.NetworkClientFactory.GetServiceGatewayClient().Get(ctx, az.ResourceGroup, serviceGatewayName, nil)
	if err != nil {
		klog.Infof("CLB-ENECHITOAIA: error getting Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return nil, err
	}
	klog.Infof("CLB-ENECHITOAIA: successfully got Service Gateway %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return result, nil
}

func (az *Cloud) ExistsServiceGateway(ctx context.Context, serviceGatewayName string) (bool, error) {
	_, err := az.GetServiceGateway(ctx, serviceGatewayName)
	if err != nil {
		if strings.Contains(err.Error(), consts.ResourceNotFoundMessageCode) {
			return false, nil
		}
		klog.Infof("CLB-ENECHITOAIA: error checking existence of Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return false, err
	}
	return true, nil
}

func (az *Cloud) UpdateAddressLocations(ctx context.Context, serviceGatewayName string, req armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
	err := az.NetworkClientFactory.GetServiceGatewayClient().UpdateAddressLocations(ctx, az.ResourceGroup, serviceGatewayName, req)
	if err != nil {
		klog.Infof("CLB-ENECHITOAIA: error updating address locations for Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return err
	}
	klog.Infof("CLB-ENECHITOAIA: successfully updated address locations for Service Gateway %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return nil
}

func (az *Cloud) UpdateServices(ctx context.Context, serviceGatewayName string, req armnetwork.ServiceGatewayUpdateServicesRequest) error {
	err := az.NetworkClientFactory.GetServiceGatewayClient().UpdateServices(ctx, az.ResourceGroup, serviceGatewayName, req)
	if err != nil {
		klog.Infof("CLB-ENECHITOAIA: error updating services for Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return err
	}
	klog.Infof("CLB-ENECHITOAIA: successfully updated services for Service Gateway %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return nil
}

func (az *Cloud) GetServices(ctx context.Context, serviceGatewayName string) ([]*armnetwork.ServiceGatewayService, error) {
	result, err := az.NetworkClientFactory.GetServiceGatewayClient().GetServices(ctx, az.ResourceGroup, serviceGatewayName)
	if err != nil {
		klog.Infof("CLB-ENECHITOAIA: error getting services for Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return nil, err
	}
	klog.Infof("CLB-ENECHITOAIA: successfully got services for Service Gateway %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return result, nil
}

// GetAddressLocations retrieves the address locations associated with the specified Service Gateway.
func (az *Cloud) GetAddressLocations(ctx context.Context, serviceGatewayName string) ([]*armnetwork.ServiceGatewayAddressLocationResponse, error) {
	result, err := az.NetworkClientFactory.GetServiceGatewayClient().GetAddressLocations(ctx, az.ResourceGroup, serviceGatewayName)
	if err != nil {
		klog.Infof("CLB-ENECHITOAIA: error getting address locations for Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return nil, err
	}
	klog.Infof("CLB-ENECHITOAIA: successfully got address locations for Service Gateway %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return result, nil
}
