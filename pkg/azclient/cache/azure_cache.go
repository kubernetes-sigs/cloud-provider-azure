/*
Copyright 2020 The Kubernetes Authors.

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

package cache

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/client-go/tools/cache"
)

// AzureCacheReadType defines the read type for cache data
type AzureCacheReadType int

const (
	// CacheReadTypeDefault returns data from cache if cache entry not expired
	// if cache entry expired, then it will refetch the data using getter
	// save the entry in cache and then return
	CacheReadTypeDefault AzureCacheReadType = iota
	// CacheReadTypeUnsafe returns data from cache even if the cache entry is
	// active/expired. If entry doesn't exist in cache, then data is fetched
	// using getter, saved in cache and returned
	CacheReadTypeUnsafe
	// CacheReadTypeForceRefresh force refreshes the cache even if the cache entry
	// is not expired
	CacheReadTypeForceRefresh
)

// GetFunc defines a getter function for timedCache.
type GetFunc[Type interface{}] func(ctx context.Context, key string) (*Type, error)

// AzureCacheEntry is the internal structure stores inside TTLStore.
type AzureCacheEntry[Type interface{}] struct {
	Key  string
	Data *Type

	// The lock to ensure not updating same entry simultaneously.
	Lock sync.Mutex
	// time when entry was fetched and created
	CreatedOn time.Time
}

// cacheKeyFunc defines the key function required in TTLStore.
func cacheKeyFunc[Type interface{}](obj interface{}) (string, error) {
	return obj.(*AzureCacheEntry[Type]).Key, nil
}

// Resource operations
type Resource[Type interface{}] interface {
	Get(ctx context.Context, key string, crt AzureCacheReadType) (*Type, error)
	GetWithDeepCopy(ctx context.Context, key string, crt AzureCacheReadType) (*Type, error)
	Delete(key string) error
	Set(key string, data *Type)
	Update(key string, data *Type)

	GetStore() cache.Store
	Lock()
	Unlock()
}

// TimedCache is a cache with TTL.
type TimedCache[Type interface{}] struct {
	Store     cache.Store
	MutexLock sync.RWMutex
	TTL       time.Duration

	resourceProvider Resource[Type]
}

type ResourceProvider[Type interface{}] struct {
	Getter GetFunc[Type]
}

// NewTimedCache creates a new azcache.Resource.
func NewTimedCache[Type interface{}](ttl time.Duration, getter GetFunc[Type], disabled bool) (Resource[Type], error) {
	if getter == nil {
		return nil, fmt.Errorf("getter is not provided")
	}

	provider := &ResourceProvider[Type]{
		Getter: getter,
	}

	if disabled {
		return provider, nil
	}

	timedCache := &TimedCache[Type]{
		// switch to using NewStore instead of NewTTLStore so that we can
		// reuse entries for calls that are fine with reading expired/stalled data.
		// with NewTTLStore, entries are not returned if they have already expired.
		Store:            cache.NewStore(cacheKeyFunc[Type]),
		MutexLock:        sync.RWMutex{},
		TTL:              ttl,
		resourceProvider: provider,
	}
	return timedCache, nil
}

// getInternal returns AzureCacheEntry by key. If the key is not cached yet,
// it returns a AzureCacheEntry with nil data.
func (t *TimedCache[Type]) getInternal(key string) (*AzureCacheEntry[Type], error) {
	entry, exists, err := t.Store.GetByKey(key)
	if err != nil {
		return nil, err
	}
	// if entry exists, return the entry
	if exists {
		return entry.(*AzureCacheEntry[Type]), nil
	}

	// lock here to ensure if entry doesn't exist, we add a new entry
	// avoiding overwrites
	t.Lock()
	defer t.Unlock()

	// Another goroutine might have written the same key.
	entry, exists, err = t.Store.GetByKey(key)
	if err != nil {
		return nil, err
	}
	if exists {
		return entry.(*AzureCacheEntry[Type]), nil
	}

	// Still not found, add new entry with nil data.
	// Note the data will be filled later by getter.
	newEntry := &AzureCacheEntry[Type]{
		Key:  key,
		Data: nil,
	}
	_ = t.Store.Add(newEntry)
	return newEntry, nil
}

// Get returns the requested item by key.
func (t *TimedCache[Type]) Get(ctx context.Context, key string, crt AzureCacheReadType) (*Type, error) {
	return t.get(ctx, key, crt)
}

func (c *ResourceProvider[Type]) Get(ctx context.Context, key string, _ AzureCacheReadType) (*Type, error) {
	return c.Getter(ctx, key)
}

// Get returns the requested item by key with deep copy.
func (t *TimedCache[Type]) GetWithDeepCopy(ctx context.Context, key string, crt AzureCacheReadType) (*Type, error) {
	data, err := t.get(ctx, key, crt)
	copied := Copy(data)
	return copied.(*Type), err
}

func (c *ResourceProvider[Type]) GetWithDeepCopy(ctx context.Context, key string, _ AzureCacheReadType) (*Type, error) {
	return c.Getter(ctx, key)
}

func (t *TimedCache[Type]) get(ctx context.Context, key string, crt AzureCacheReadType) (*Type, error) {
	entry, err := t.getInternal(key)
	if err != nil {
		return nil, err
	}

	entry.Lock.Lock()
	defer entry.Lock.Unlock()

	// entry exists and if cache is not force refreshed
	if entry.Data != nil && crt != CacheReadTypeForceRefresh {
		// allow unsafe read, so return data even if expired
		if crt == CacheReadTypeUnsafe {
			return entry.Data, nil
		}
		// if cached data is not expired, return cached data
		if crt == CacheReadTypeDefault && time.Since(entry.CreatedOn) < t.TTL {
			return entry.Data, nil
		}
	}
	// Data is not cached yet, cache data is expired or requested force refresh
	// cache it by getter. entry is locked before getting to ensure concurrent
	// gets don't result in multiple ARM calls.
	data, err := t.resourceProvider.Get(ctx, key, CacheReadTypeDefault /* not matter */)
	if err != nil {
		return nil, err
	}

	// set the data in cache and also set the last update time
	// to now as the data was recently fetched
	entry.Data = data
	entry.CreatedOn = time.Now().UTC()

	return entry.Data, nil
}

// Delete removes an item from the cache.
func (t *TimedCache[Type]) Delete(key string) error {
	return t.Store.Delete(&AzureCacheEntry[Type]{
		Key: key,
	})
}

func (c *ResourceProvider[Type]) Delete(_ string) error {
	return nil
}

// Set sets the data cache for the key.
// It is only used for testing.
func (t *TimedCache[Type]) Set(key string, data *Type) {
	_ = t.Store.Add(&AzureCacheEntry[Type]{
		Key:       key,
		Data:      data,
		CreatedOn: time.Now().UTC(),
	})
}

func (c *ResourceProvider[Type]) Set(_ string, _ *Type) {}

// Update updates the data cache for the key.
func (t *TimedCache[Type]) Update(key string, data *Type) {
	if entry, err := t.getInternal(key); err == nil {
		entry.Lock.Lock()
		defer entry.Lock.Unlock()
		entry.Data = data
		entry.CreatedOn = time.Now().UTC()
	} else {
		_ = t.Store.Update(&AzureCacheEntry[Type]{
			Key:       key,
			Data:      data,
			CreatedOn: time.Now().UTC(),
		})
	}
}

func (c *ResourceProvider[Type]) Update(_ string, _ *Type) {}

func (t *TimedCache[Type]) GetStore() cache.Store {
	return t.Store
}

func (c *ResourceProvider[Type]) GetStore() cache.Store {
	return nil
}

func (t *TimedCache[Type]) Lock() {
	t.MutexLock.Lock()
}

func (t *TimedCache[Type]) Unlock() {
	t.MutexLock.Unlock()
}

func (c *ResourceProvider[Type]) Lock() {}

func (c *ResourceProvider[Type]) Unlock() {}
