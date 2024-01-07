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

package provider

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"

	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func TestSimpleLockEntry(t *testing.T) {
	testLockMap := newLockMap()

	callbackChan1 := make(chan interface{})
	go testLockMap.lockAndCallback(t, "entry1", callbackChan1)
	ensureCallbackHappens(t, callbackChan1)
}

func TestSimpleLockUnlockEntry(t *testing.T) {
	testLockMap := newLockMap()

	callbackChan1 := make(chan interface{})
	go testLockMap.lockAndCallback(t, "entry1", callbackChan1)
	ensureCallbackHappens(t, callbackChan1)
	testLockMap.UnlockEntry("entry1")
}

func TestConcurrentLockEntry(t *testing.T) {
	testLockMap := newLockMap()

	callbackChan1 := make(chan interface{})
	callbackChan2 := make(chan interface{})

	go testLockMap.lockAndCallback(t, "entry1", callbackChan1)
	ensureCallbackHappens(t, callbackChan1)

	go testLockMap.lockAndCallback(t, "entry1", callbackChan2)
	ensureNoCallback(t, callbackChan2)

	testLockMap.UnlockEntry("entry1")
	ensureCallbackHappens(t, callbackChan2)
	testLockMap.UnlockEntry("entry1")
}

func (lm *lockMap) lockAndCallback(t *testing.T, entry string, callbackChan chan<- interface{}) {
	lm.LockEntry(entry)
	callbackChan <- true
}

var callbackTimeout = 2 * time.Second

func ensureCallbackHappens(t *testing.T, callbackChan <-chan interface{}) bool {
	select {
	case <-callbackChan:
		return true
	case <-time.After(callbackTimeout):
		t.Fatalf("timed out waiting for callback")
		return false
	}
}

func ensureNoCallback(t *testing.T, callbackChan <-chan interface{}) bool {
	select {
	case <-callbackChan:
		t.Fatalf("unexpected callback")
		return false
	case <-time.After(callbackTimeout):
		return true
	}
}

func TestReconcileTags(t *testing.T) {
	for _, testCase := range []struct {
		description, systemTags                      string
		currentTagsOnResource, newTags, expectedTags map[string]*string
		expectedChanged                              bool
	}{
		{
			description: "reconcileTags should add missing tags and update existing tags",
			currentTagsOnResource: map[string]*string{
				"a": pointer.String("b"),
			},
			newTags: map[string]*string{
				"a": pointer.String("c"),
				"b": pointer.String("d"),
			},
			expectedTags: map[string]*string{
				"a": pointer.String("c"),
				"b": pointer.String("d"),
			},
			expectedChanged: true,
		},
		{
			description: "reconcileTags should remove the tags that are not included in systemTags",
			currentTagsOnResource: map[string]*string{
				"a": pointer.String("b"),
				"c": pointer.String("d"),
			},
			newTags: map[string]*string{
				"a": pointer.String("c"),
			},
			systemTags: "a, b",
			expectedTags: map[string]*string{
				"a": pointer.String("c"),
			},
			expectedChanged: true,
		},
		{
			description: "reconcileTags should ignore the case of keys when comparing",
			currentTagsOnResource: map[string]*string{
				"A": pointer.String("b"),
				"c": pointer.String("d"),
			},
			newTags: map[string]*string{
				"a": pointer.String("b"),
				"C": pointer.String("d"),
			},
			expectedTags: map[string]*string{
				"A": pointer.String("b"),
				"c": pointer.String("d"),
			},
		},
		{
			description: "reconcileTags should ignore the case of values when comparing",
			currentTagsOnResource: map[string]*string{
				"A": pointer.String("b"),
				"c": pointer.String("d"),
			},
			newTags: map[string]*string{
				"a": pointer.String("B"),
				"C": pointer.String("D"),
			},
			expectedTags: map[string]*string{
				"A": pointer.String("b"),
				"c": pointer.String("d"),
			},
		},
		{
			description: "reconcileTags should ignore the case of keys when checking systemTags",
			currentTagsOnResource: map[string]*string{
				"a": pointer.String("b"),
				"c": pointer.String("d"),
			},
			newTags: map[string]*string{
				"a": pointer.String("c"),
			},
			systemTags: "A, b",
			expectedTags: map[string]*string{
				"a": pointer.String("c"),
			},
			expectedChanged: true,
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			cloud := &Cloud{}
			if testCase.systemTags != "" {
				cloud.SystemTags = testCase.systemTags
			}

			tags, changed := cloud.reconcileTags(testCase.currentTagsOnResource, testCase.newTags)
			assert.Equal(t, testCase.expectedChanged, changed)
			assert.Equal(t, testCase.expectedTags, tags)
		})
	}
}

func TestGetServiceAdditionalPublicIPs(t *testing.T) {
	for _, testCase := range []struct {
		description   string
		service       *v1.Service
		expectedIPs   []string
		expectedError error
	}{
		{
			description: "nil service should return empty IP list",
		},
		{
			description: "service without annotation should return empty IP list",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			expectedIPs: []string{},
		},
		{
			description: "service without annotation should return empty IP list",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationAdditionalPublicIPs: "",
					},
				},
			},
			expectedIPs: []string{},
		},
		{
			description: "service with one IP in annotation should return expected IPs",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationAdditionalPublicIPs: "1.2.3.4 ",
					},
				},
			},
			expectedIPs: []string{"1.2.3.4"},
		},
		{
			description: "service with multiple IPs in annotation should return expected IPs",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationAdditionalPublicIPs: "1.2.3.4, 2.3.4.5 ",
					},
				},
			},
			expectedIPs: []string{"1.2.3.4", "2.3.4.5"},
		},
		{
			description: "service with wrong IP in annotation should report an error",
			service: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationAdditionalPublicIPs: "invalid",
					},
				},
			},
			expectedError: fmt.Errorf("invalid is not a valid IP address"),
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			ips, err := getServiceAdditionalPublicIPs(testCase.service)
			assert.Equal(t, testCase.expectedIPs, ips)
			assert.Equal(t, testCase.expectedError, err)
		})
	}
}

func TestRemoveDuplicatedSecurityRules(t *testing.T) {
	for _, testCase := range []struct {
		description string
		rules       []network.SecurityRule
		expected    []network.SecurityRule
	}{
		{
			description: "no duplicated rules",
			rules: []network.SecurityRule{
				{
					Name: pointer.String("rule1"),
				},
				{
					Name: pointer.String("rule2"),
				},
			},
			expected: []network.SecurityRule{
				{
					Name: pointer.String("rule1"),
				},
				{
					Name: pointer.String("rule2"),
				},
			},
		},
		{
			description: "duplicated rules",
			rules: []network.SecurityRule{
				{
					Name: pointer.String("rule1"),
				},
				{
					Name: pointer.String("rule2"),
				},
				{
					Name: pointer.String("rule1"),
				},
			},
			expected: []network.SecurityRule{
				{
					Name: pointer.String("rule2"),
				},
				{
					Name: pointer.String("rule1"),
				},
			},
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			rules := testCase.rules
			rules = removeDuplicatedSecurityRules(rules)
			assert.Equal(t, testCase.expected, rules)
		})
	}
}

func TestGetVMSSVMCacheKey(t *testing.T) {
	tests := []struct {
		description       string
		resourceGroupName string
		vmssName          string
		cacheKey          string
	}{
		{
			description:       "Resource group and Vmss Name are in lower case",
			resourceGroupName: "resgrp",
			vmssName:          "vmss",
			cacheKey:          "resgrp/vmss",
		},
		{
			description:       "Resource group has upper case and Vmss Name is in lower case",
			resourceGroupName: "Resgrp",
			vmssName:          "vmss",
			cacheKey:          "resgrp/vmss",
		},
		{
			description:       "Resource group is in lower case and Vmss Name has upper case",
			resourceGroupName: "resgrp",
			vmssName:          "Vmss",
			cacheKey:          "resgrp/vmss",
		},
		{
			description:       "Resource group and Vmss Name are both in upper case",
			resourceGroupName: "Resgrp",
			vmssName:          "Vmss",
			cacheKey:          "resgrp/vmss",
		},
	}

	for _, test := range tests {
		result := getVMSSVMCacheKey(test.resourceGroupName, test.vmssName)
		assert.Equal(t, result, test.cacheKey, test.description)
	}
}

func TestIsNodeInVMSSVMCache(t *testing.T) {

	getter := func(key string) (interface{}, error) {
		return nil, nil
	}
	emptyCacheEntryTimedCache, _ := azcache.NewTimedcache(fakeCacheTTL, getter)
	emptyCacheEntryTimedCache.Set("key", nil)

	cacheEntryTimedCache, _ := azcache.NewTimedcache(fakeCacheTTL, getter)
	syncMap := &sync.Map{}
	syncMap.Store("node", nil)
	cacheEntryTimedCache.Set("key", syncMap)

	tests := []struct {
		description    string
		nodeName       string
		vmssVMCache    *azcache.TimedCache
		expectedResult bool
	}{
		{
			description:    "nil cache",
			vmssVMCache:    nil,
			expectedResult: false,
		},
		{
			description:    "empty CacheEntry timed cache",
			vmssVMCache:    emptyCacheEntryTimedCache,
			expectedResult: false,
		},
		{
			description:    "node name in the cache",
			nodeName:       "node",
			vmssVMCache:    cacheEntryTimedCache,
			expectedResult: true,
		},
		{
			description:    "node name not in the cache",
			nodeName:       "node2",
			vmssVMCache:    cacheEntryTimedCache,
			expectedResult: false,
		},
	}

	for _, test := range tests {
		result := isNodeInVMSSVMCache(test.nodeName, test.vmssVMCache)
		assert.Equal(t, test.expectedResult, result, test.description)
	}
}

func TestIsInternalLoadBalancer(t *testing.T) {
	tests := []struct {
		name     string
		lb       network.LoadBalancer
		expected bool
	}{
		{
			name: "internal load balancer",
			lb: network.LoadBalancer{
				Name: pointer.String("test-internal"),
			},
			expected: true,
		},
		{
			name: "internal load balancer",
			lb: network.LoadBalancer{
				Name: pointer.String("TEST-INTERNAL"),
			},
			expected: true,
		},
		{
			name: "not internal load balancer",
			lb: network.LoadBalancer{
				Name: pointer.String("test"),
			},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lb := test.lb
			result := isInternalLoadBalancer(&lb)
			assert.Equal(t, test.expected, result)
		})
	}
}
