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

package fixture

import (
	"fmt"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"

	"k8s.io/utils/ptr"
)

func (f *AzureFixture) PublicIPAddress(name string) *AzurePublicIPAddressFixture {
	const (
		SubscriptionID = "00000000-0000-0000-0000-000000000000"
		ResourceGroup  = "rg"
	)

	return &AzurePublicIPAddressFixture{
		pip: &network.PublicIPAddress{
			ID:                              ptr.To(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s", SubscriptionID, ResourceGroup, name)),
			Name:                            ptr.To(name),
			Tags:                            make(map[string]*string),
			PublicIPAddressPropertiesFormat: &network.PublicIPAddressPropertiesFormat{},
		},
	}
}

type AzurePublicIPAddressFixture struct {
	pip *network.PublicIPAddress
}

func (f *AzurePublicIPAddressFixture) Build() network.PublicIPAddress {
	return *f.pip
}

func (f *AzurePublicIPAddressFixture) WithTag(key, value string) *AzurePublicIPAddressFixture {
	f.pip.Tags[key] = ptr.To(value)
	return f
}

func (f *AzurePublicIPAddressFixture) WithAddress(address string) *AzurePublicIPAddressFixture {
	f.pip.PublicIPAddressPropertiesFormat.IPAddress = ptr.To(address)
	return f
}
