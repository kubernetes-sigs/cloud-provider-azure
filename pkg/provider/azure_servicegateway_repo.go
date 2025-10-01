package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"k8s.io/klog/v2"
)

func (az *Cloud) GetServiceGateway(ctx context.Context, serviceGatewayName string) (*armnetwork.ServiceGateway, error) {
	result, err := az.NetworkClientFactory.GetServiceGatewayClient().Get(ctx, az.ResourceGroup, serviceGatewayName, nil)
	if err != nil {
		klog.Infof("GetServiceGateway: error getting Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return nil, err
	}
	klog.Infof("GetServiceGateway: successfully got Service Gateway %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return result, nil
}

func (az *Cloud) GetServices(ctx context.Context, serviceGatewayName string) ([]*armnetwork.ServiceGatewayService, error) {
	result, err := az.NetworkClientFactory.GetServiceGatewayClient().GetServices(ctx, az.ResourceGroup, serviceGatewayName)
	if err != nil {
		klog.Infof("GetServices: error getting services for Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return nil, err
	}
	klog.Infof("GetServices: successfully got services for Service Gateway %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return result, nil
}

// GetAddressLocations retrieves the address locations associated with the specified Service Gateway.
func (az *Cloud) GetAddressLocations(ctx context.Context, serviceGatewayName string) ([]*armnetwork.ServiceGatewayAddressLocationResponse, error) {
	result, err := az.NetworkClientFactory.GetServiceGatewayClient().GetAddressLocations(ctx, az.ResourceGroup, serviceGatewayName)
	if err != nil {
		klog.Infof("GetAddressLocations: error getting address locations for Service Gateway %s in resource group %s: %v", serviceGatewayName, az.ResourceGroup, err)
		return nil, err
	}
	klog.Infof("GetAddressLocations: successfully got address locations for Service Gateway %s in resource group %s", serviceGatewayName, az.ResourceGroup)
	return result, nil
}

func (az *Cloud) CreateOrUpdateServiceGateway(ctx context.Context, serviceGatewayName string, parameters armnetwork.ServiceGateway) error {
	klog.Infof("ServiceGatewayClient.CreateOrUpdate(%s): start", serviceGatewayName)

	for { // endless retry until success or context cancellation
		_, err := az.NetworkClientFactory.GetServiceGatewayClient().CreateOrUpdate(ctx, az.ResourceGroup, serviceGatewayName, parameters)
		if err == nil {
			klog.V(10).Infof("ServiceGatewayClient.CreateOrUpdate(%s): success", serviceGatewayName)
			klog.Infof("ServiceGatewayClient.CreateOrUpdate(%s): end, error: nil", serviceGatewayName)
			return nil
		}

		// Log the error with serialized parameters for easier debugging
		paramsJSON, _ := json.Marshal(parameters)
		klog.Warningf("ServiceGatewayClient.CreateOrUpdate(%s) failed: %v, request: %s", serviceGatewayName, err, string(paramsJSON))

		// Respect context cancellation
		select {
		case <-ctx.Done():
			klog.V(3).Infof("CreateOrUpdateServiceGateway: context canceled, stopping retry for %s", serviceGatewayName)
			return fmt.Errorf("context canceled while creating or updating service gateway %s: %w", serviceGatewayName, ctx.Err())
		default:
		}

		klog.V(3).Infof("CreateOrUpdateServiceGateway: retrying in 5 seconds for Service Gateway %s", serviceGatewayName)
		time.Sleep(5 * time.Second)
	}
}

func (az *Cloud) UpdateAddressLocations(ctx context.Context, serviceGatewayName string, req armnetwork.ServiceGatewayUpdateAddressLocationsRequest) error {
	klog.Infof("ServiceGatewayClient.UpdateAddressLocations(%s): start", serviceGatewayName)

	for { // endless retry until success or context cancellation
		err := az.NetworkClientFactory.GetServiceGatewayClient().UpdateAddressLocations(ctx, az.ResourceGroup, serviceGatewayName, req)
		if err == nil {
			klog.V(10).Infof("ServiceGatewayClient.UpdateAddressLocations(%s): success", serviceGatewayName)
			klog.Infof("ServiceGatewayClient.UpdateAddressLocations(%s): end, error: nil", serviceGatewayName)
			return nil
		}

		reqJSON, _ := json.Marshal(req)
		klog.Warningf("ServiceGatewayClient.UpdateAddressLocations(%s) failed: %v, request: %s", serviceGatewayName, err, string(reqJSON))

		select {
		case <-ctx.Done():
			klog.V(3).Infof("UpdateAddressLocations: context canceled, stopping retry for %s", serviceGatewayName)
			return fmt.Errorf("context canceled while updating address locations for service gateway %s: %w", serviceGatewayName, ctx.Err())
		default:
		}

		klog.V(3).Infof("UpdateAddressLocations: retrying in 5 seconds for Service Gateway %s", serviceGatewayName)
		time.Sleep(5 * time.Second)
	}
}

func (az *Cloud) UpdateServices(ctx context.Context, serviceGatewayName string, req armnetwork.ServiceGatewayUpdateServicesRequest) error {
	klog.Infof("ServiceGatewayClient.UpdateServices(%s): start", serviceGatewayName)

	for { // endless retry until success or context cancellation
		err := az.NetworkClientFactory.GetServiceGatewayClient().UpdateServices(ctx, az.ResourceGroup, serviceGatewayName, req)
		if err == nil {
			klog.V(10).Infof("ServiceGatewayClient.UpdateServices(%s): success", serviceGatewayName)
			klog.Infof("ServiceGatewayClient.UpdateServices(%s): end, error: nil", serviceGatewayName)
			return nil
		}

		reqJSON, _ := json.Marshal(req)
		klog.Warningf("ServiceGatewayClient.UpdateServices(%s) failed: %v, request: %s", serviceGatewayName, err, string(reqJSON))

		select {
		case <-ctx.Done():
			klog.V(3).Infof("UpdateServices: context canceled, stopping retry for %s", serviceGatewayName)
			return fmt.Errorf("context canceled while updating services for service gateway %s: %w", serviceGatewayName, ctx.Err())
		default:
		}

		klog.V(3).Infof("UpdateServices: retrying in 5 seconds for Service Gateway %s", serviceGatewayName)
		time.Sleep(5 * time.Second)
	}
}
