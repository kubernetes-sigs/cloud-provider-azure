/*
Copyright 2019 The Kubernetes Authors.

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

package utils

import (
	"context"
	"fmt"

	aznetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	"github.com/Azure/go-autorest/autorest/to"

	providerazure "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

// ListRouteTables returns the list of all route tables in the resource group
func ListRouteTables(tc *AzureTestClient) (*[]aznetwork.RouteTable, error) {
	routeTableClient := tc.createRouteTableClient()

	list, err := routeTableClient.List(context.Background(), tc.GetResourceGroup())
	if err != nil {
		return nil, err
	}

	res := list.Values()
	if len(res) == 0 {
		return nil, fmt.Errorf("no route table found")
	}

	return &res, nil
}

// GetNodesInRouteTable returns all the nodes in the route table
func GetNodesInRouteTable(routeTable aznetwork.RouteTable) (map[string]interface{}, error) {
	if routeTable.Routes == nil || len(*routeTable.Routes) == 0 {
		return nil, fmt.Errorf("cannot obtained routes in route table %s", *routeTable.Name)
	}

	routeSet := make(map[string]interface{})
	for _, route := range *routeTable.Routes {
		routeSet[string(providerazure.MapRouteNameToNodeName(true, to.String(route.Name)))] = true
	}

	return routeSet, nil
}
