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

package loadbalancer

// Generate mocks for the repository interface
//go:generate mockgen -destination=./mock_repo.go -package=loadbalancer -copyright_file ../../../hack/boilerplate/boilerplate.generatego.txt -source=repo.go Repository

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/backendaddresspoolclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/loadbalancerclient"

	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/errutils"
)

type Repository interface {
	Get(
		ctx context.Context,
		resourceGroup string,
		resourceName string,
		crt cache.AzureCacheReadType,
	) (*armnetwork.LoadBalancer, error)

	ListByResourceGroup(
		ctx context.Context,
		resourceGroup string,
	) ([]*armnetwork.LoadBalancer, error)

	CreateOrUpdate(
		ctx context.Context,
		resourceGroup string,
		resource *armnetwork.LoadBalancer,
	) (*armnetwork.LoadBalancer, error)

	MigrateToIPBased(
		ctx context.Context,
		resourceGroup string,
		resourceName string,
		pools []*string,
	) (*armnetwork.MigratedPools, error)

	Delete(
		ctx context.Context,
		resourceGroup string,
		resourceName string,
	) error

	GetBackendPool(
		ctx context.Context,
		resourceGroup string,
		resourceName string,
		backendPoolName string,
	) (*armnetwork.BackendAddressPool, error)

	CreateOrUpdateBackendPool(
		ctx context.Context,
		resourceGroup string,
		resourceName string,
		backendPool *armnetwork.BackendAddressPool,
	) (*armnetwork.BackendAddressPool, error)

	DeleteBackendPool(
		ctx context.Context,
		resourceGroup string,
		resourceName string,
		backendPoolName string,
	) error
}

type repo struct {
	loadBalancerClient       loadbalancerclient.Interface
	backendAddressPoolClient backendaddresspoolclient.Interface
	cache                    cache.Resource
}

func NewRepo(
	loadBalancerClient loadbalancerclient.Interface,
	backendAddressPoolClient backendaddresspoolclient.Interface,
	cacheTTL time.Duration,
	disableAPICallCache bool,
) (Repository, error) {
	c, err := NewCache(loadBalancerClient, cacheTTL, disableAPICallCache)
	if err != nil {
		return nil, fmt.Errorf("new LoadBalancer cache: %w", err)
	}

	return &repo{
		loadBalancerClient:       loadBalancerClient,
		backendAddressPoolClient: backendAddressPoolClient,
		cache:                    c,
	}, nil
}

func (r *repo) Get(
	ctx context.Context,
	resourceGroup string,
	resourceName string,
	crt cache.AzureCacheReadType,
) (*armnetwork.LoadBalancer, error) {
	cacheKey := getCacheKey(resourceGroup, resourceName)
	rv, err := r.cache.GetWithDeepCopy(ctx, cacheKey, crt)
	if err != nil {
		return nil, err
	}
	return rv.(*armnetwork.LoadBalancer), nil
}

func (r *repo) ListByResourceGroup(ctx context.Context, resourceGroup string) ([]*armnetwork.LoadBalancer, error) {
	rv, err := r.loadBalancerClient.List(ctx, resourceGroup)
	if err != nil {
		return nil, err
	}
	return rv, nil
}

func (r *repo) CreateOrUpdate(
	ctx context.Context, resourceGroup string, resource *armnetwork.LoadBalancer,
) (*armnetwork.LoadBalancer, error) {
	rv, err := r.loadBalancerClient.CreateOrUpdate(ctx, resourceGroup, *resource.Name, *resource)
	if err != nil {
		return nil, err
	}
	_ = r.cache.Delete(*resource.Name)
	return rv, nil
}

func (r *repo) MigrateToIPBased(
	ctx context.Context,
	resourceGroup string,
	resourceName string,
	pools []*string,
) (*armnetwork.MigratedPools, error) {
	opts := &armnetwork.LoadBalancersClientMigrateToIPBasedOptions {
		Parameters: &armnetwork.MigrateLoadBalancerToIPBasedRequest{
			Pools: pools,
		},
	}
	resp, err := r.loadBalancerClient.MigrateToIPBased(ctx, resourceGroup, resourceName, opts)
	if err != nil {
		return nil, err
	}
	_ = r.cache.Delete(resourceName)
	return &resp.MigratedPools, nil
}

func (r *repo) GetBackendPool(
	ctx context.Context,
	resourceGroup string,
	resourceName string,
	backendPoolName string,
) (*armnetwork.BackendAddressPool, error) {
	rv, err := r.backendAddressPoolClient.Get(ctx, resourceGroup, resourceName, backendPoolName)
	found, err := errutils.CheckResourceExistsFromAzcoreError(err)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrBackendPoolNotFound
	}

	return rv, nil
}

func (r *repo) CreateOrUpdateBackendPool(
	ctx context.Context,
	resourceGroup string,
	resourceName string,
	backendPool *armnetwork.BackendAddressPool,
) (*armnetwork.BackendAddressPool, error) {
	rv, err := r.backendAddressPoolClient.CreateOrUpdate(
		ctx, resourceGroup, resourceName, *backendPool.Name, *backendPool,
	)
	if err != nil {
		return nil, err
	}
	_ = r.cache.Delete(resourceName)
	return rv, nil
}

func (r *repo) Delete(ctx context.Context, resourceGroup string, resourceName string) error {
	err := r.loadBalancerClient.Delete(ctx, resourceGroup, resourceName)
	if err == nil {
		_ = r.cache.Delete(resourceName)
	}
	return err
}

func (r *repo) DeleteBackendPool(
	ctx context.Context, resourceGroup string, resourceName string, backendPoolName string,
) error {
	err := r.backendAddressPoolClient.Delete(ctx, resourceGroup, resourceName, backendPoolName)
	if err == nil {
		_ = r.cache.Delete(resourceName)
	}
	return err
}
