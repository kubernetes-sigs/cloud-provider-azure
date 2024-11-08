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

package routetable

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/routetableclient/mock_routetableclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
)

func TestRepo_Get(t *testing.T) {
	t.Parallel()

	const ResourceGroup = "testing-rg"

	t.Run("refresh cache", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_routetableclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, ResourceGroup, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			RouteTableName = "route-table-name"
		)

		cli.EXPECT().Get(gomock.Any(), ResourceGroup, gomock.Any()).Return(&armnetwork.RouteTable{
			Name: ptr.To(RouteTableName),
		}, nil)

		v, err := repo.Get(ctx, RouteTableName, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, RouteTableName, *v.Name)

		// cache hit
		v, err = repo.Get(ctx, RouteTableName, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, RouteTableName, *v.Name)
	})

	t.Run("refresh cache with not found", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_routetableclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, ResourceGroup, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			RouteTableName = "route-table-name"
		)

		cli.EXPECT().Get(gomock.Any(), ResourceGroup, gomock.Any()).Return(nil, nil)

		v, err := repo.Get(ctx, RouteTableName, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Nil(t, v)
	})

	t.Run("API error", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_routetableclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, ResourceGroup, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			RouteTableName = "route-table-name"
		)

		expectedErr := fmt.Errorf("API error")
		cli.EXPECT().Get(gomock.Any(), ResourceGroup, gomock.Any()).Return(nil, expectedErr)

		_, err = repo.Get(ctx, RouteTableName, cache.CacheReadTypeDefault)
		assert.Error(t, err)
		assert.ErrorIs(t, err, expectedErr)
	})
}

func TestRepo_CreateOrUpdate(t *testing.T) {
	t.Parallel()

	const ResourceGroup = "testing-rg"

	t.Run("create one", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_routetableclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, ResourceGroup, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			RouteTableName = "route-table-name"
		)

		cli.EXPECT().CreateOrUpdate(gomock.Any(), ResourceGroup, RouteTableName, armnetwork.RouteTable{
			Name: to.Ptr(RouteTableName),
		}).Return(&armnetwork.RouteTable{
			Name: to.Ptr(RouteTableName),
		}, nil).Times(1)

		v, err := repo.CreateOrUpdate(ctx, armnetwork.RouteTable{
			Name: to.Ptr(RouteTableName),
		})
		assert.NoError(t, err)
		assert.Equal(t, RouteTableName, *v.Name)
	})

	t.Run("create one with missing RouteTable name", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_routetableclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, ResourceGroup, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		_, err = repo.CreateOrUpdate(ctx, armnetwork.RouteTable{})
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrMissingRouteTableName)
	})

	t.Run("should clear cache", func(t *testing.T) {

		t.Parallel()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_routetableclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, ResourceGroup, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			RouteTableName = "route-table-name"
		)

		cli.EXPECT().Get(gomock.Any(), ResourceGroup, RouteTableName).Return(&armnetwork.RouteTable{
			Name: to.Ptr(RouteTableName),
		}, nil).Times(2)

		cli.EXPECT().CreateOrUpdate(gomock.Any(), ResourceGroup, RouteTableName, armnetwork.RouteTable{
			Name: to.Ptr(RouteTableName),
		}).Return(&armnetwork.RouteTable{
			Name: to.Ptr(RouteTableName),
		}, nil).Times(1)

		v, err := repo.Get(ctx, RouteTableName, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, RouteTableName, *v.Name)

		v, err = repo.CreateOrUpdate(ctx, armnetwork.RouteTable{
			Name: to.Ptr(RouteTableName),
		})
		assert.NoError(t, err)
		assert.Equal(t, RouteTableName, *v.Name)

		v, err = repo.Get(ctx, RouteTableName, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, RouteTableName, *v.Name)

		v, err = repo.Get(ctx, RouteTableName, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, RouteTableName, *v.Name)
	})
}
