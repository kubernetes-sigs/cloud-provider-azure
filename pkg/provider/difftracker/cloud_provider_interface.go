package difftracker

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	v1 "k8s.io/api/core/v1"
)

// CloudProvider interface defines methods needed from the Cloud provider
type CloudProvider interface {
	// Resource creation/deletion methods
	CreateOrUpdatePIPOutbound(ctx context.Context, pipResourceGroup string, pip *armnetwork.PublicIPAddress) error
	DeletePublicIPOutbound(ctx context.Context, pipResourceGroup string, pipName string) error
	CreateOrUpdateLB(ctx context.Context, service *v1.Service, lb armnetwork.LoadBalancer) error
	CreateOrUpdateNatGateway(ctx context.Context, natGatewayResourceGroup string, natGateway armnetwork.NatGateway) error
	DeleteNatGateway(ctx context.Context, natGatewayResourceGroup string, natGatewayName string) error
	EnsureServiceLoadBalancerDeletedByUID(ctx context.Context, uid string, clusterName string) error

	// ServiceGateway API
	UpdateNRPSGWServices(ctx context.Context, serviceGatewayName string, updateServicesRequestDTO ServicesDataDTO) error
	UpdateNRPSGWAddressLocations(ctx context.Context, serviceGatewayName string, locationsDTO LocationsDataDTO) error

	// Helper methods
	GetServiceGatewayID() string
	GetServiceByUID(ctx context.Context, uid string) (*v1.Service, error)

	// Config accessors
	GetSubscriptionID() string
	GetResourceGroup() string
	GetLocation() string
	GetClusterName() string
	GetServiceGatewayResourceName() string
}
