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

package fileservice

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/cache"
	azureconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
)

type Repository interface {
	Get(ctx context.Context, subsID, resourceGroup, account string) (*armstorage.FileServiceProperties, error)
	Set(ctx context.Context, subsID, resourceGroup, account string, properties *armstorage.FileServiceProperties) error
}

type fileServicePropertiesRepo struct {
	clientFactory              azclient.ClientFactory
	fileServicePropertiesCache cache.Resource[armstorage.FileServiceProperties]
}

func NewRepository(config azureconfig.Config, clientFactory azclient.ClientFactory) (Repository, error) {
	getter := func(_ context.Context, _ string) (*armstorage.FileServiceProperties, error) { return nil, nil }
	fileServicePropertiesCache, err := cache.NewTimedCache(5*time.Minute, getter, config.DisableAPICallCache)
	if err != nil {
		return nil, err
	}
	return &fileServicePropertiesRepo{
		clientFactory:              clientFactory,
		fileServicePropertiesCache: fileServicePropertiesCache,
	}, nil
}
func (az *fileServicePropertiesRepo) Get(ctx context.Context, subsID, resourceGroup, account string) (*armstorage.FileServiceProperties, error) {
	if az.clientFactory == nil {
		return nil, fmt.Errorf("clientFactory is nil")
	}
	if az.fileServicePropertiesCache == nil {
		return nil, fmt.Errorf("fileServicePropertiesCache is nil")
	}

	// search in cache first
	cache, err := az.fileServicePropertiesCache.Get(ctx, account, cache.CacheReadTypeDefault)
	if err != nil {
		return nil, err
	}
	if cache != nil {
		klog.V(2).Infof("Get service properties(%s) from cache", account)
		return cache, nil
	}

	fileserviceClient, err := az.clientFactory.GetFileServicePropertiesClientForSub(subsID)
	if err != nil {
		return nil, err
	}

	result, err := fileserviceClient.Get(ctx, resourceGroup, account)
	if err != nil {
		return nil, err
	}
	az.fileServicePropertiesCache.Set(account, result)
	return result, nil
}

func (az *fileServicePropertiesRepo) Set(ctx context.Context, subsID, resourceGroup, account string, properties *armstorage.FileServiceProperties) error {
	if az.fileServicePropertiesCache == nil {
		return fmt.Errorf("fileServicePropertiesCache is nil")
	}
	if properties == nil {
		properties = &armstorage.FileServiceProperties{}
	}
	fileserviceClient, err := az.clientFactory.GetFileServicePropertiesClientForSub(subsID)
	if err != nil {
		return err
	}
	result, err := fileserviceClient.Set(ctx, resourceGroup, account, *properties)
	if err != nil {
		cacheErr := az.fileServicePropertiesCache.Delete(account)
		return errors.Join(err, cacheErr)
	}
	az.fileServicePropertiesCache.Set(account, result)
	return nil
}
