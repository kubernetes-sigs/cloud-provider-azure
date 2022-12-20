/*
Copyright 2022 The Kubernetes Authors.

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
package publicipaddressclient

import (
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/v2/utils"
)

// +azure:client:verbs=get;createorupdate;delete;list,resource=PublicIPAddress,packageName=github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v2,packageAlias=armnetwork,clientName=PublicIPAddressesClient,apiVersion="2021-08-01",expand=true
type Interface interface {
	utils.GetWithExpandFunc[armnetwork.PublicIPAddress]

	utils.CreateOrUpdateFunc[armnetwork.PublicIPAddress]

	utils.DeleteFunc[armnetwork.PublicIPAddress]

	utils.ListFunc[armnetwork.PublicIPAddress]
}
