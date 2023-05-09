/*
Copyright 2023 The Kubernetes Authors.

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
package privatelinkserviceclient

import (
	network "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

// +azure:client:verbs=get;createorupdate;delete;list,resource=PrivateLinkService,packageName=github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v2,packageAlias=network,clientName=PrivateLinkServicesClient,apiVersion="2022-07-01",expand=true
type Interface interface {
	utils.GetWithExpandFunc[network.PrivateLinkService]

	utils.CreateOrUpdateFunc[network.PrivateLinkService]

	utils.DeleteFunc[network.PrivateLinkService]

	utils.ListFunc[network.PrivateLinkService]
}
