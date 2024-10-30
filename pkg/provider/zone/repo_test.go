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
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/providerclient/mock_providerclient"
)

func TestRepo_ListZones(t *testing.T) {
	t.Run("return zones", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		client := mock_providerclient.NewMockInterface(ctrl)
		repo, err := NewRepo(client)
		assert.NoError(t, err)

		expectedZones := map[string][]*string{
			"k1": {to.Ptr("zone1"), to.Ptr("zone2")},
			"k2": {to.Ptr("zone3"), to.Ptr("zone4")},
		}
		client.EXPECT().GetVirtualMachineSupportedZones(gomock.Any()).Return(expectedZones, nil).Times(1)

		zones, err := repo.ListZones(context.Background())
		assert.NoError(t, err)
		assert.Equal(t, map[string][]string{
			"k1": {"zone1", "zone2"},
			"k2": {"zone3", "zone4"},
		}, zones)
	})

	t.Run("return error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		client := mock_providerclient.NewMockInterface(ctrl)
		repo, err := NewRepo(client)
		assert.NoError(t, err)

		expectedErr := fmt.Errorf("error")
		client.EXPECT().GetVirtualMachineSupportedZones(gomock.Any()).Return(nil, expectedErr).Times(1)

		_, err = repo.ListZones(context.Background())
		assert.ErrorIs(t, err, expectedErr)
	})
}
