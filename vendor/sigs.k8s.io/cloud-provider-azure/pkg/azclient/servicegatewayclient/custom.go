/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package servicegatewayclient

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/metrics"
)

const UpdateTagsOperationName = "ServiceGatewaysClient.UpdateTags"

// UpdateTags updates the tags of a ServiceGateway.
func (client *Client) UpdateTags(ctx context.Context, resourceGroupName string, serviceGatewayName string, parameters armnetwork.TagsObject) (result *armnetwork.ServiceGateway, err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "ServiceGateway", "update_tags")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, UpdateTagsOperationName, client.tracer, nil)
	defer endSpan(err)
	resp, err := client.ServiceGatewaysClient.UpdateTags(ctx, resourceGroupName, serviceGatewayName, parameters, nil)
	if err != nil {
		return nil, err
	}
	return &resp.ServiceGateway, nil
}

const GetAddressLocationsOperationName = "ServiceGatewaysClient.GetAddressLocations"

// GetAddressLocations gets the address locations of a ServiceGateway.
func (client *Client) GetAddressLocations(ctx context.Context, resourceGroupName string, serviceGatewayName string) (result []*armnetwork.ServiceGatewayAddressLocationResponse, err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "ServiceGateway", "get_address_locations")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, GetAddressLocationsOperationName, client.tracer, nil)
	defer endSpan(err)
	pager := client.NewGetAddressLocationsPager(resourceGroupName, serviceGatewayName, nil)
	for pager.More() {
		nextResult, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, nextResult.Value...)
	}
	return result, nil
}

const GetServicesOperationName = "ServiceGatewaysClient.GetServices"

// GetServices gets the services of a ServiceGateway.
func (client *Client) GetServices(ctx context.Context, resourceGroupName string, serviceGatewayName string) (result []*armnetwork.ServiceGatewayService, err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "ServiceGateway", "get_services")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, GetServicesOperationName, client.tracer, nil)
	defer endSpan(err)
	pager := client.NewGetServicesPager(resourceGroupName, serviceGatewayName, nil)
	for pager.More() {
		nextResult, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, nextResult.Value...)
	}
	return result, nil
}

const UpdateAddressLocationsOperationName = "ServiceGatewaysClient.UpdateAddressLocations"

// UpdateAddressLocations updates the address locations of a ServiceGateway.
// NRP completes the update synchronously and answers 200 OK; there is no long-running operation to poll.
func (client *Client) UpdateAddressLocations(ctx context.Context, resourceGroupName string, serviceGatewayName string, parameters armnetwork.ServiceGatewayUpdateAddressLocationsRequest) (err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "ServiceGateway", "update_address_locations")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, UpdateAddressLocationsOperationName, client.tracer, nil)
	defer endSpan(err)
	var raw *http.Response
	if _, err = client.ServiceGatewaysClient.UpdateAddressLocations(runtime.WithCaptureResponse(ctx, &raw), resourceGroupName, serviceGatewayName, parameters, nil); err != nil {
		return err
	}
	return errorCodeInSuccess(raw)
}

const UpdateServicesOperationName = "ServiceGatewaysClient.UpdateServices"

// UpdateServices updates the services of a ServiceGateway.
// NRP completes the update synchronously and answers 200 OK; there is no long-running operation to poll.
func (client *Client) UpdateServices(ctx context.Context, resourceGroupName string, serviceGatewayName string, parameters armnetwork.ServiceGatewayUpdateServicesRequest) (err error) {
	metricsCtx := metrics.BeginARMRequest(client.subscriptionID, resourceGroupName, "ServiceGateway", "update_services")
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, UpdateServicesOperationName, client.tracer, nil)
	defer endSpan(err)
	var raw *http.Response
	if _, err = client.ServiceGatewaysClient.UpdateServices(runtime.WithCaptureResponse(ctx, &raw), resourceGroupName, serviceGatewayName, parameters, nil); err != nil {
		return err
	}
	return errorCodeInSuccess(raw)
}

// errorCodeInSuccess returns a 200 response as an error when it names an error code in the
// x-ms-error-code header or the body. A successful update must not carry one; accepting it would
// report a change NRP did not make. The error is only built when a code is present, because
// building it also writes an SDK response-error log event.
func errorCodeInSuccess(resp *http.Response) error {
	if resp == nil {
		return nil
	}
	code := resp.Header.Get("x-ms-error-code")
	if code == "" {
		code = errorCodeInBody(resp)
	}
	if code == "" {
		return nil
	}
	return runtime.NewResponseErrorWithErrorCode(resp, code)
}

// errorCodeInBody returns the code of an ARM error body, {"error":{"code":...}} or {"code":...}.
func errorCodeInBody(resp *http.Response) string {
	body, err := runtime.Payload(resp)
	if err != nil || len(body) == 0 {
		return ""
	}
	var payload struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if payload.Error.Code != "" {
		return payload.Error.Code
	}
	return payload.Code
}
