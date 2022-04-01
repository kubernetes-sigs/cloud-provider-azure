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
	"fmt"
	"runtime"
	"sync"
	"time"
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
type GetFunc func(key string) (interface{}, error)

// AzureCacheEntry is the internal structure stores inside TTLStore.
type AzureCacheEntry struct {
	Key  string
	Data interface{}

	// time when entry was fetched and created
	CreatedOn time.Time
}

// TimedCache is a cache with TTL.
type TimedCache struct {
	Store  sync.Map
	Getter GetFunc
	TTL    time.Duration
}

// NewTimedcache creates a new TimedCache.
func NewTimedcache(ttl time.Duration, getter GetFunc) (*TimedCache, error) {
	if getter == nil {
		return nil, fmt.Errorf("getter is not provided")
	}

	return &TimedCache{
		Getter: getter,
		TTL:    ttl,
	}, nil
}

// Get returns the requested item by key.
func (t *TimedCache) Get(key string, crt AzureCacheReadType) (interface{}, error) {
	rawEntry, loaded := t.Store.LoadOrStore(key, &AzureCacheEntry{
		Key:  key,
		Data: nil,
	})
	entry := rawEntry.(*AzureCacheEntry)

	// spin lock is introduced to make sure one ARM call only for one resource
	for loaded {
		if entry.Data != nil {
			switch crt {
			case CacheReadTypeUnsafe:
				return entry.Data, nil
			case CacheReadTypeDefault:
				if time.Since(entry.CreatedOn) < t.TTL {
					return entry.Data, nil
				}
				fallthrough
			case CacheReadTypeForceRefresh:
				t.Store.Delete(key)
			}
		}
		runtime.Gosched()
		rawEntry, loaded = t.Store.LoadOrStore(key, &AzureCacheEntry{
			Key:  key,
			Data: nil,
		})
		entry = rawEntry.(*AzureCacheEntry)
	}

	return func() (data interface{}, reterr error) {
		defer func() {
			if err := recover(); err != nil {
				reterr = err.(error)
			}
			if reterr != nil {
				t.Store.Delete(key)
			}
		}()
		data, reterr = t.Getter(key)
		if reterr != nil {
			return nil, reterr
		}
		// set the data in cache and also set the last update time
		// to now as the data was recently fetched
		t.Set(key, data)
		return data, nil
	}()
}

// Delete removes an item from the cache.
func (t *TimedCache) Delete(key string) error {
	t.Store.Delete(key)
	return nil
}

// Set sets the data cache for the key.
// It is only used for testing.
func (t *TimedCache) Set(key string, data interface{}) {
	t.Store.Store(key, &AzureCacheEntry{
		Key:       key,
		Data:      data,
		CreatedOn: time.Now().UTC(),
	})
}
