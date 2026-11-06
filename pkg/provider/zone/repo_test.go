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

package zone

import (
	"context"
	"fmt"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/providerclient/mock_providerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func TestRepo_ListZones(t *testing.T) {
	for _, tc := range []struct {
		name         string
		resourceType string
		provider     *armresources.Provider
		expected     map[string][]string
	}{
		{
			name:         "public IP zones exclude unsupported VM zones",
			resourceType: consts.PublicIPAddressResourceType,
			provider:     networkProviderWithZones(),
			expected:     map[string][]string{"eastus2": {"1", "2", "3", "4"}},
		},
		{
			name:         "resource type matching is case insensitive",
			resourceType: "PUBLICIPADDRESSES",
			provider:     networkProviderWithZones(),
			expected:     map[string][]string{"eastus2": {"1", "2", "3", "4"}},
		},
		{
			name:         "missing resource type has no zones",
			resourceType: consts.PublicIPAddressResourceType,
			provider:     &armresources.Provider{},
			expected:     map[string][]string{},
		},
		{
			name:         "resource type without zone mappings has no zones",
			resourceType: consts.PublicIPAddressResourceType,
			provider: &armresources.Provider{ResourceTypes: []*armresources.ProviderResourceType{
				{ResourceType: to.Ptr(consts.PublicIPAddressResourceType)},
			}},
			expected: map[string][]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			client := mock_providerclient.NewMockInterface(ctrl)
			repo, err := NewRepo(client)
			assert.NoError(t, err)

			client.EXPECT().GetProvider(gomock.Any(), "Microsoft.Network").Return(tc.provider, nil)

			zones, err := repo.ListZones(context.Background(), tc.resourceType)
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, zones)
		})
	}

	for _, tc := range []struct {
		name         string
		resourceType string
		zones        map[string][]*string
		err          error
		expected     map[string][]string
	}{
		{
			name:         "virtual machine zones use existing compute discovery",
			resourceType: consts.VirtualMachineResourceType,
			zones:        map[string][]*string{"eastus2": to.SliceOfPtrs("1", "2", "3", "4", "5")},
			expected:     map[string][]string{"eastus2": {"1", "2", "3", "4", "5"}},
		},
		{
			name:         "virtual machine resource type matching is case insensitive",
			resourceType: "VIRTUALMACHINES",
			zones:        map[string][]*string{"eastus2": to.SliceOfPtrs("1", "2", "3")},
			expected:     map[string][]string{"eastus2": {"1", "2", "3"}},
		},
		{
			name:         "missing virtual machine mappings have no zones",
			resourceType: consts.VirtualMachineResourceType,
			expected:     map[string][]string{},
		},
		{
			name:         "virtual machine discovery returns errors",
			resourceType: consts.VirtualMachineResourceType,
			err:          fmt.Errorf("compute zone discovery error"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			client := mock_providerclient.NewMockInterface(ctrl)
			repo, err := NewRepo(client)
			assert.NoError(t, err)

			client.EXPECT().GetVirtualMachineSupportedZones(gomock.Any()).Return(tc.zones, tc.err)

			zones, err := repo.ListZones(context.Background(), tc.resourceType)
			assert.ErrorIs(t, err, tc.err)
			assert.Equal(t, tc.expected, zones)
		})
	}

	t.Run("return error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		client := mock_providerclient.NewMockInterface(ctrl)
		repo, err := NewRepo(client)
		assert.NoError(t, err)

		expectedErr := fmt.Errorf("error")
		client.EXPECT().GetProvider(gomock.Any(), "Microsoft.Network").Return(nil, expectedErr).Times(1)

		_, err = repo.ListZones(context.Background(), consts.PublicIPAddressResourceType)
		assert.ErrorIs(t, err, expectedErr)
	})
}

func networkProviderWithZones() *armresources.Provider {
	return &armresources.Provider{ResourceTypes: []*armresources.ProviderResourceType{
		{
			ResourceType: to.Ptr("virtualMachines"),
			ZoneMappings: []*armresources.ZoneMapping{
				{Location: to.Ptr("East US 2"), Zones: to.SliceOfPtrs("1", "2", "3", "4", "5")},
			},
		},
		{
			ResourceType: to.Ptr("publicIPAddresses"),
			ZoneMappings: []*armresources.ZoneMapping{
				{Location: to.Ptr("East US 2"), Zones: to.SliceOfPtrs("1", "2", "3", "4")},
			},
		},
		{ResourceType: to.Ptr("loadBalancers")},
	}}
}
