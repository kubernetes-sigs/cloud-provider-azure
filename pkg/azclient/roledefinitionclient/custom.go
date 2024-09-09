/*
Copyright 2024 The Kubernetes Authors.

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

// +azure:enableclientgen:=true
package roledefinitionclient

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	armauthorization "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2"
	"go.opentelemetry.io/otel/attribute"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/metrics"
)

const ListOperationName = "RoleDefinitionsClient.List"

// List gets a list of RoleDefinition in the resource group.
func (client *Client) List(ctx context.Context, scopeName string, option *armauthorization.RoleDefinitionsClientListOptions) (result []*armauthorization.RoleDefinition, err error) {
	metricsCtx := metrics.BeginARMRequestWithAttributes(attribute.String("resource", "RoleDefinition"), attribute.String("method", "list"))
	defer func() { metricsCtx.Observe(ctx, err) }()
	ctx, endSpan := runtime.StartSpan(ctx, ListOperationName, client.tracer, nil)
	defer endSpan(err)
	pager := client.RoleDefinitionsClient.NewListPager(scopeName, option)
	for pager.More() {
		nextResult, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		result = append(result, nextResult.Value...)
	}
	return result, nil
}
