package servicegatewayclient

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

const GetAddressLocationsOperationName = "ServiceGatewaysClient.GetAddressLocations"

func (client *Client) GetAddressLocations(ctx context.Context, resourceGroupName string, serviceGatewayName string) (result []*armnetwork.ServiceGatewayAddressLocationResponse, err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "ServiceGateway", "getAddressLocations")
	defer func() { metricsCtx.Observe(ctx, err) }()

	ctx, endSpan := runtime.StartSpan(ctx, GetAddressLocationsOperationName, client.tracer, nil)
	defer endSpan(err)

	resp, err := utils.NewPollerWrapper(
		client.ServiceGatewaysClient.BeginGetAddressLocations(ctx, resourceGroupName, serviceGatewayName, nil),
	).WaitforPollerResp(ctx)
	if err != nil {
		return nil, err
	}
	// The response embeds GetServiceGatewayAddressLocationsResult, so Value is directly accessible.
	return (*resp).Value, nil
}

// Optional extras below. Uncomment if needed.

const GetServicesOperationName = "ServiceGatewaysClient.GetServices"

func (client *Client) GetServices(ctx context.Context, resourceGroupName string, serviceGatewayName string) (result []*armnetwork.ServiceGatewayService, err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "ServiceGateway", "getServices")
	defer func() { metricsCtx.Observe(ctx, err) }()

	ctx, endSpan := runtime.StartSpan(ctx, GetServicesOperationName, client.tracer, nil)
	defer endSpan(err)

	resp, err := utils.NewPollerWrapper(
		client.ServiceGatewaysClient.BeginGetServices(ctx, resourceGroupName, serviceGatewayName, nil),
	).WaitforPollerResp(ctx)
	if err != nil {
		return nil, err
	}
	return (*resp).Value, nil
}

const UpdateAddressLocationsOperationName = "ServiceGatewaysClient.UpdateAddressLocations"

func (client *Client) UpdateAddressLocations(ctx context.Context, resourceGroupName string, serviceGatewayName string, req armnetwork.ServiceGatewayUpdateAddressLocationsRequest) (err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "ServiceGateway", "updateAddressLocations")
	defer func() { metricsCtx.Observe(ctx, err) }()

	ctx, endSpan := runtime.StartSpan(ctx, UpdateAddressLocationsOperationName, client.tracer, nil)
	defer endSpan(err)

	_, err = client.ServiceGatewaysClient.UpdateAddressLocations(ctx, resourceGroupName, serviceGatewayName, req, nil)
	return err
}

const UpdateServicesOperationName = "ServiceGatewaysClient.UpdateServices"

func (client *Client) UpdateServices(ctx context.Context, resourceGroupName string, serviceGatewayName string, req armnetwork.ServiceGatewayUpdateServicesRequest) (err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "ServiceGateway", "updateServices")
	defer func() { metricsCtx.Observe(ctx, err) }()

	ctx, endSpan := runtime.StartSpan(ctx, UpdateServicesOperationName, client.tracer, nil)
	defer endSpan(err)

	_, err = utils.NewPollerWrapper(
		client.ServiceGatewaysClient.BeginUpdateServices(ctx, resourceGroupName, serviceGatewayName, req, nil),
	).WaitforPollerResp(ctx)
	return err
}
