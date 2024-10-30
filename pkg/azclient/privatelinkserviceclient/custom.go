package privatelinkserviceclient

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

const DeletePEConnectionOperationName = "PrivateLinkServicesClient.DeletePrivateEndpointConnection"

func (client *Client) DeletePrivateEndpointConnection(ctx context.Context, resourceGroupName string, serviceName string, peConnectionName string) (err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "PrivateLinkService", "deletePrivateEndpointConnection")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, DeletePEConnectionOperationName, client.tracer, nil)
	defer endSpan(err)

	_, err = utils.NewPollerWrapper(
		client.BeginDeletePrivateEndpointConnection(ctx, resourceGroupName, serviceName, peConnectionName, nil),
	).WaitforPollerResp(ctx)

	return err
}
