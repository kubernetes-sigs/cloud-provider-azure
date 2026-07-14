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

package provider

import (
	"fmt"
	"strings"

	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func getServiceUID(service *v1.Service) string {
	return strings.ToLower(string(service.UID))
}

func (az *Cloud) nodePrivateIPsForNode(nodeName string) []string {
	az.nodeCachesLock.RLock()
	defer az.nodeCachesLock.RUnlock()
	if set := az.nodePrivateIPs[strings.ToLower(nodeName)]; set != nil {
		return set.UnsortedList()
	}
	return nil
}

func (az *Cloud) GetServiceGatewayID() string {
	return fmt.Sprintf(
		"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/serviceGateways/%s",
		az.SubscriptionID,
		az.ResourceGroup,
		consts.DefaultServiceGatewayResourceName,
	)
}
