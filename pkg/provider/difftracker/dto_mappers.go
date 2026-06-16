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

package difftracker

import (
	"fmt"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(loadBalancerUpdates SyncServicesReturnType, natGatewayUpdates SyncServicesReturnType, subscriptionID string, resourceGroup string) ServicesDataDTO {
	var ServicesDataDTO ServicesDataDTO
	ServicesDataDTO.Action = PartialUpdate
	ServicesDataDTO.Services = []ServiceDTO{}
	for _, service := range loadBalancerUpdates.Additions.UnsortedList() {
		serviceDTO := ServiceDTO{
			Service:     service,
			ServiceType: Inbound,
			LoadBalancerBackendPools: []LoadBalancerBackendPoolDTO{
				{
					Id: fmt.Sprintf(
						consts.BackendPoolIDTemplate,
						subscriptionID,
						resourceGroup,
						service,
						service,
						// fmt.Sprintf("%s-backendpool", service),
					),
				},
			},
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
	for _, service := range loadBalancerUpdates.Removals.UnsortedList() {
		serviceDTO := ServiceDTO{
			Service:     service,
			IsDelete:    true,
			ServiceType: Inbound,
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
	for _, service := range natGatewayUpdates.Additions.UnsortedList() {
		serviceDTO := ServiceDTO{
			Service:     service,
			ServiceType: Outbound,
			PublicNatGateway: NatGatewayDTO{
				Id: fmt.Sprintf(
					consts.NatGatewayIDTemplate,
					subscriptionID,
					resourceGroup,
					service,
				),
			},
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
	for _, service := range natGatewayUpdates.Removals.UnsortedList() {
		serviceDTO := ServiceDTO{
			Service:     service,
			IsDelete:    true,
			ServiceType: Outbound,
		}
		ServicesDataDTO.Services = append(ServicesDataDTO.Services, serviceDTO)
	}
	return ServicesDataDTO
}
