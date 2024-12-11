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

package publicip

import (
	"context"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient"

	"sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/errutils"
)

const (
	DefaultCacheTTL = 120 * time.Second
)

func NewCache(
	client publicipaddressclient.Interface,
	cacheTTL time.Duration,
	disableAPICallCache bool,
) (cache.Resource, error) {
	getter := func(ctx context.Context, key string) (interface{}, error) {
		resourceGroup, pipName := parseCacheKey(key)

		rt, err := client.Get(ctx, resourceGroup, pipName, nil)
		found, err := errutils.CheckResourceExistsFromAzcoreError(err)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, ErrNotFound
		}

		return rt, nil
	}

	if cacheTTL == 0 {
		cacheTTL = DefaultCacheTTL
	}
	return cache.NewTimedCache(cacheTTL, getter, disableAPICallCache)
}

func getCacheKey(resourceGroup, pipName string) string {
	return fmt.Sprintf("%s/%s", resourceGroup, pipName)
}

func parseCacheKey(key string) (string, string) {
	s := strings.Split(key, "/")
	return s[0], s[1]
}
