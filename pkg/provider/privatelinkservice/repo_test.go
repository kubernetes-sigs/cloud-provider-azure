package privatelinkservice

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/privatelinkserviceclient/mock_privatelinkserviceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func TestRepo_Get(t *testing.T) {
	t.Run("refresh cache", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_privatelinkserviceclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			rg                 = "resource-group"
			frontendIPConfigID = "frontend-ip-config-id"
			plsID              = "pls-id"
		)

		cli.EXPECT().List(gomock.Any(), rg).Return([]*armnetwork.PrivateLinkService{
			{
				ID: to.Ptr(plsID),
				Properties: &armnetwork.PrivateLinkServiceProperties{
					LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
						{ID: to.Ptr(frontendIPConfigID)},
					},
				},
			},
		}, nil).Times(1)

		v, err := repo.Get(ctx, rg, frontendIPConfigID, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, plsID, *v.ID)

		// cache hit
		v, err = repo.Get(ctx, rg, frontendIPConfigID, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, plsID, *v.ID)
	})

	t.Run("refresh cache with not found", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_privatelinkserviceclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			rg                 = "resource-group"
			frontendIPConfigID = "frontend-ip-config-id"
		)

		cli.EXPECT().List(gomock.Any(), rg).Return([]*armnetwork.PrivateLinkService{}, nil).Times(1)

		v, err := repo.Get(ctx, rg, frontendIPConfigID, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, consts.PrivateLinkServiceNotExistID, *v.ID)
	})

	t.Run("API error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_privatelinkserviceclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			rg                 = "resource-group"
			frontendIPConfigID = "frontend-ip-config-id"
		)

		expectedErr := fmt.Errorf("API error")

		cli.EXPECT().List(gomock.Any(), rg).Return(nil, expectedErr).Times(1)

		_, err = repo.Get(ctx, rg, frontendIPConfigID, cache.CacheReadTypeDefault)
		assert.Error(t, err)
		assert.ErrorIs(t, err, expectedErr)
	})
}

func TestRepo_CreateOrUpdate(t *testing.T) {
	t.Run("create one", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_privatelinkserviceclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			rg                 = "resource-group"
			plsName            = "pls-name"
			frontendIPConfigID = "frontend-ip-config-id"
		)

		cli.EXPECT().CreateOrUpdate(gomock.Any(), rg, plsName, gomock.Any()).Return(&armnetwork.PrivateLinkService{
			Name: to.Ptr(plsName),
			Properties: &armnetwork.PrivateLinkServiceProperties{
				LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
					{ID: to.Ptr(frontendIPConfigID)},
				},
			},
		}, nil).Times(1)

		v, err := repo.CreateOrUpdate(ctx, rg, armnetwork.PrivateLinkService{
			Name: to.Ptr(plsName),
			Properties: &armnetwork.PrivateLinkServiceProperties{
				LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
					{ID: to.Ptr(frontendIPConfigID)},
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, plsName, *v.Name)
	})

	t.Run("create one with missing PLS name", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_privatelinkserviceclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			rg = "resource-group"
		)

		_, err = repo.CreateOrUpdate(ctx, rg, armnetwork.PrivateLinkService{})
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrMissingPLSName)
	})

	t.Run("create one with missing LoadBalancerFrontendIPConfigurations", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_privatelinkserviceclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			rg      = "resource-group"
			plsName = "pls-name"
		)

		_, err = repo.CreateOrUpdate(ctx, rg, armnetwork.PrivateLinkService{
			Name: to.Ptr(plsName),
		})
		assert.Error(t, err)
		assert.ErrorIs(t, err, ErrLoadBalancerFrontendIPConfigurationsNotFound)
	})

	t.Run("should clear cache", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_privatelinkserviceclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			rg                 = "resource-group"
			plsName            = "pls-name"
			frontendIPConfigID = "frontend-ip-config-id"
		)

		cli.EXPECT().List(gomock.Any(), rg).Return([]*armnetwork.PrivateLinkService{
			{
				Name: to.Ptr(plsName),
				Properties: &armnetwork.PrivateLinkServiceProperties{
					LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
						{ID: to.Ptr(frontendIPConfigID)},
					},
				},
			},
		}, nil).Times(2)

		cli.EXPECT().CreateOrUpdate(gomock.Any(), rg, plsName, gomock.Any()).Return(&armnetwork.PrivateLinkService{
			Name: to.Ptr(plsName),
			Properties: &armnetwork.PrivateLinkServiceProperties{
				LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
					{ID: to.Ptr(frontendIPConfigID)},
				},
			},
		}, nil).Times(1)

		v, err := repo.Get(ctx, rg, frontendIPConfigID, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, plsName, *v.Name)

		v, err = repo.CreateOrUpdate(ctx, rg, armnetwork.PrivateLinkService{
			Name: to.Ptr(plsName),
			Properties: &armnetwork.PrivateLinkServiceProperties{
				LoadBalancerFrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
					{ID: to.Ptr(frontendIPConfigID)},
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, plsName, *v.Name)

		// cache miss
		v, err = repo.Get(ctx, rg, frontendIPConfigID, cache.CacheReadTypeDefault)
		assert.NoError(t, err)
		assert.Equal(t, plsName, *v.Name)

	})
}

func TestRepo_Delete(t *testing.T) {
	t.Run("delete one", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_privatelinkserviceclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			rg                 = "resource-group"
			plsName            = "pls-name"
			frontendIPConfigID = "frontend-ip-config-id"
		)

		cli.EXPECT().Delete(gomock.Any(), rg, plsName).Return(nil).Times(1)

		err = repo.Delete(ctx, rg, plsName, frontendIPConfigID)
		assert.NoError(t, err)
	})

	t.Run("API error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_privatelinkserviceclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			rg      = "resource-group"
			plsName = "pls-name"
		)

		expectedErr := fmt.Errorf("API error")

		cli.EXPECT().Delete(gomock.Any(), rg, plsName).Return(expectedErr).Times(1)

		err = repo.Delete(ctx, rg, plsName, "")
		assert.Error(t, err)
		assert.ErrorIs(t, err, expectedErr)
	})
}

func TestRepo_DeletePEConnection(t *testing.T) {
	t.Run("delete one", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_privatelinkserviceclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			rg         = "resource-group"
			plsName    = "pls-name"
			peConnName = "pe-conn-name"
		)

		cli.EXPECT().DeletePrivateEndpointConnection(gomock.Any(), rg, plsName, peConnName).Return(nil).Times(1)

		err = repo.DeletePEConnection(ctx, rg, plsName, peConnName)
		assert.NoError(t, err)
	})

	t.Run("API error", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		cli := mock_privatelinkserviceclient.NewMockInterface(ctrl)
		repo, err := NewRepo(cli, 60*time.Second, false)
		assert.NoError(t, err)
		ctx := context.Background()

		const (
			rg         = "resource-group"
			plsName    = "pls-name"
			peConnName = "pe-conn-name"
		)

		expectedErr := fmt.Errorf("API error")

		cli.EXPECT().DeletePrivateEndpointConnection(gomock.Any(), rg, plsName, peConnName).Return(expectedErr).Times(1)

		err = repo.DeletePEConnection(ctx, rg, plsName, peConnName)
		assert.Error(t, err)
		assert.ErrorIs(t, err, expectedErr)
	})
}
