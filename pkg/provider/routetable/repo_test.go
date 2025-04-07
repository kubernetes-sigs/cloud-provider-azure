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
	kcache "k8s.io/client-go/tools/cache"
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

	t.Run("type assertion failure", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_routetableclient.NewMockInterface(ctrl)

		// Create a mock cache that will return a wrong type
		mockCache := &mockCacheWithWrongType{}

		// Create repo with the mock cache
		r := &repo{
			resourceGroup: ResourceGroup,
			client:        cli,
			cache:         mockCache,
		}

		ctx := context.Background()
		const RouteTableName = "route-table-name"

		// Should get type assertion error
		_, err := r.Get(ctx, RouteTableName, cache.CacheReadTypeDefault)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unexpected type for RouteTable")
		assert.Contains(t, err.Error(), "string")
	})

	t.Run("empty interface value", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_routetableclient.NewMockInterface(ctrl)

		// Create a mock cache that will return an empty interface{}
		mockCache := &mockCacheWithEmptyInterface{}

		// Create repo with the mock cache
		r := &repo{
			resourceGroup: ResourceGroup,
			client:        cli,
			cache:         mockCache,
		}

		ctx := context.Background()
		const RouteTableName = "route-table-name"

		// Should get type assertion error
		_, err := r.Get(ctx, RouteTableName, cache.CacheReadTypeDefault)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unexpected type for RouteTable")
		assert.Contains(t, err.Error(), "struct {}") // empty struct
	})

	t.Run("nil value from cache", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_routetableclient.NewMockInterface(ctrl)

		// Create a mock cache that will return nil
		mockCache := &mockCacheWithNilValue{}

		// Create repo with the mock cache
		r := &repo{
			resourceGroup: ResourceGroup,
			client:        cli,
			cache:         mockCache,
		}

		ctx := context.Background()
		const RouteTableName = "route-table-name"

		// Should return nil, nil
		rt, err := r.Get(ctx, RouteTableName, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Nil(t, rt)
	})
}

// mockCacheWithWrongType is a mock implementation of cache.Resource that returns a wrong type
type mockCacheWithWrongType struct{}

func (m *mockCacheWithWrongType) Get(_ context.Context, _ string, _ cache.AzureCacheReadType) (interface{}, error) {
	// Return a string instead of *armnetwork.RouteTable to trigger type assertion failure
	return "wrong type", nil
}

func (m *mockCacheWithWrongType) GetWithDeepCopy(_ context.Context, _ string, _ cache.AzureCacheReadType) (interface{}, error) {
	// Return a string instead of *armnetwork.RouteTable to trigger type assertion failure
	return "wrong type", nil
}

func (m *mockCacheWithWrongType) GetStore() kcache.Store {
	return nil
}

func (m *mockCacheWithWrongType) Lock() {
	// No-op for testing
}

func (m *mockCacheWithWrongType) Unlock() {
	// No-op for testing
}

func (m *mockCacheWithWrongType) Delete(_ string) error {
	return nil
}

func (m *mockCacheWithWrongType) Set(_ string, _ interface{}) {
	// No-op for testing
}

func (m *mockCacheWithWrongType) Update(_ string, _ interface{}) {
	// No-op for testing
}

// mockCacheWithEmptyInterface is a mock implementation of cache.Resource that returns an empty interface value
type mockCacheWithEmptyInterface struct{}

func (m *mockCacheWithEmptyInterface) Get(_ context.Context, _ string, _ cache.AzureCacheReadType) (interface{}, error) {
	// Return an empty struct as interface{} to trigger type assertion failure
	return struct{}{}, nil
}

func (m *mockCacheWithEmptyInterface) GetWithDeepCopy(_ context.Context, _ string, _ cache.AzureCacheReadType) (interface{}, error) {
	// Return an empty struct as interface{} to trigger type assertion failure
	return struct{}{}, nil
}

func (m *mockCacheWithEmptyInterface) GetStore() kcache.Store {
	return nil
}

func (m *mockCacheWithEmptyInterface) Lock() {
	// No-op for testing
}

func (m *mockCacheWithEmptyInterface) Unlock() {
	// No-op for testing
}

func (m *mockCacheWithEmptyInterface) Delete(_ string) error {
	return nil
}

func (m *mockCacheWithEmptyInterface) Set(_ string, _ interface{}) {
	// No-op for testing
}

func (m *mockCacheWithEmptyInterface) Update(_ string, _ interface{}) {
	// No-op for testing
}

// mockCacheWithNilValue is a mock implementation of cache.Resource that returns nil values
type mockCacheWithNilValue struct{}

func (m *mockCacheWithNilValue) Get(_ context.Context, _ string, _ cache.AzureCacheReadType) (interface{}, error) {
	// Return nil to test nil handling
	return nil, nil
}

func (m *mockCacheWithNilValue) GetWithDeepCopy(_ context.Context, _ string, _ cache.AzureCacheReadType) (interface{}, error) {
	// Return nil to test nil handling
	return nil, nil
}

func (m *mockCacheWithNilValue) GetStore() kcache.Store {
	return nil
}

func (m *mockCacheWithNilValue) Lock() {
	// No-op for testing
}

func (m *mockCacheWithNilValue) Unlock() {
	// No-op for testing
}

func (m *mockCacheWithNilValue) Delete(_ string) error {
	return nil
}

func (m *mockCacheWithNilValue) Set(_ string, _ interface{}) {
	// No-op for testing
}

func (m *mockCacheWithNilValue) Update(_ string, _ interface{}) {
	// No-op for testing
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
