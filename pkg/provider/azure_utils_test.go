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
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-08-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/stretchr/testify/assert"

	"go.uber.org/mock/gomock"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

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

func (lm *LockMap) lockAndCallback(_ *testing.T, entry string, callbackChan chan<- interface{}) {
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
				"a": ptr.To("b"),
			},
			newTags: map[string]*string{
				"a": ptr.To("c"),
				"b": ptr.To("d"),
			},
			expectedTags: map[string]*string{
				"a": ptr.To("c"),
				"b": ptr.To("d"),
			},
			expectedChanged: true,
		},
		{
			description: "reconcileTags should remove the tags that are not included in systemTags",
			currentTagsOnResource: map[string]*string{
				"a": ptr.To("b"),
				"c": ptr.To("d"),
			},
			newTags: map[string]*string{
				"a": ptr.To("c"),
			},
			systemTags: "a, b",
			expectedTags: map[string]*string{
				"a": ptr.To("c"),
			},
			expectedChanged: true,
		},
		{
			description: "reconcileTags should ignore the case of keys when comparing",
			currentTagsOnResource: map[string]*string{
				"A": ptr.To("b"),
				"c": ptr.To("d"),
			},
			newTags: map[string]*string{
				"a": ptr.To("b"),
				"C": ptr.To("d"),
			},
			expectedTags: map[string]*string{
				"A": ptr.To("b"),
				"c": ptr.To("d"),
			},
		},
		{
			description: "reconcileTags should ignore the case of values when comparing",
			currentTagsOnResource: map[string]*string{
				"A": ptr.To("b"),
				"c": ptr.To("d"),
			},
			newTags: map[string]*string{
				"a": ptr.To("B"),
				"C": ptr.To("D"),
			},
			expectedTags: map[string]*string{
				"A": ptr.To("b"),
				"c": ptr.To("d"),
			},
		},
		{
			description: "reconcileTags should ignore the case of keys when checking systemTags",
			currentTagsOnResource: map[string]*string{
				"a": ptr.To("b"),
				"c": ptr.To("d"),
			},
			newTags: map[string]*string{
				"a": ptr.To("c"),
			},
			systemTags: "A, b",
			expectedTags: map[string]*string{
				"a": ptr.To("c"),
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

func TestGetNodePrivateIPAddress(t *testing.T) {
	testcases := []struct {
		desc       string
		node       *v1.Node
		isIPv6     bool
		expectedIP string
	}{
		{
			"IPv4",
			&v1.Node{
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{
						{
							Type:    v1.NodeExternalIP,
							Address: "10.244.0.1",
						},
						{
							Type:    v1.NodeInternalIP,
							Address: "10.0.0.1",
						},
						{
							Type:    v1.NodeInternalIP,
							Address: "2001::1",
						},
					},
				},
			},
			false,
			"10.0.0.1",
		},
		{
			"IPv6",
			&v1.Node{
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{
						{
							Type:    v1.NodeExternalIP,
							Address: "2f00::1",
						},
						{
							Type:    v1.NodeInternalIP,
							Address: "10.0.0.1",
						},
						{
							Type:    v1.NodeInternalIP,
							Address: "2001::1",
						},
					},
				},
			},
			true,
			"2001::1",
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			ip := getNodePrivateIPAddress(tc.node, tc.isIPv6)
			assert.Equal(t, tc.expectedIP, ip)
		})
	}
}

func TestGetNodePrivateIPAddresses(t *testing.T) {
	testcases := []struct {
		desc       string
		node       *v1.Node
		expetedIPs []string
	}{
		{
			"default",
			&v1.Node{
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{
						{
							Type:    v1.NodeExternalIP,
							Address: "2f00::1",
						},
						{
							Type:    v1.NodeInternalIP,
							Address: "10.0.0.1",
						},
						{
							Type:    v1.NodeInternalIP,
							Address: "2001::1",
						},
					},
				},
			},
			[]string{"10.0.0.1", "2001::1"},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			ips := getNodePrivateIPAddresses(tc.node)
			assert.Equal(t, tc.expetedIPs, ips)
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
					Name: ptr.To("rule1"),
				},
				{
					Name: ptr.To("rule2"),
				},
			},
			expected: []network.SecurityRule{
				{
					Name: ptr.To("rule1"),
				},
				{
					Name: ptr.To("rule2"),
				},
			},
		},
		{
			description: "duplicated rules",
			rules: []network.SecurityRule{
				{
					Name: ptr.To("rule1"),
				},
				{
					Name: ptr.To("rule2"),
				},
				{
					Name: ptr.To("rule1"),
				},
			},
			expected: []network.SecurityRule{
				{
					Name: ptr.To("rule2"),
				},
				{
					Name: ptr.To("rule1"),
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
	emptyCacheEntryTimedCache, _ := azcache.NewTimedCache(fakeCacheTTL, getter, false)
	emptyCacheEntryTimedCache.Set("key", nil)

	cacheEntryTimedCache, _ := azcache.NewTimedCache(fakeCacheTTL, getter, false)
	syncMap := &sync.Map{}
	syncMap.Store("node", nil)
	cacheEntryTimedCache.Set("key", syncMap)

	tests := []struct {
		description    string
		nodeName       string
		vmssVMCache    azcache.Resource
		expectedResult bool
	}{
		{
			description:    "nil cache",
			vmssVMCache:    nil,
			expectedResult: false,
		},
		{
			description:    "empty CacheEntry timed cache",
			vmssVMCache:    emptyCacheEntryTimedCache.(*azcache.TimedCache),
			expectedResult: false,
		},
		{
			description:    "node name in the cache",
			nodeName:       "node",
			vmssVMCache:    cacheEntryTimedCache.(*azcache.TimedCache),
			expectedResult: true,
		},
		{
			description:    "node name not in the cache",
			nodeName:       "node2",
			vmssVMCache:    cacheEntryTimedCache.(*azcache.TimedCache),
			expectedResult: false,
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			result := isNodeInVMSSVMCache(test.nodeName, test.vmssVMCache)
			assert.Equal(t, test.expectedResult, result)
		})
	}
}

func TestIsServiceDualStack(t *testing.T) {
	singleStackSvc := v1.Service{
		Spec: v1.ServiceSpec{IPFamilies: []v1.IPFamily{v1.IPv4Protocol}},
	}
	assert.False(t, isServiceDualStack(&singleStackSvc))

	dualStackSvc := v1.Service{
		Spec: v1.ServiceSpec{IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}},
	}
	assert.True(t, isServiceDualStack(&dualStackSvc))
}

func TestGetIPFamiliesEnabled(t *testing.T) {
	testcases := []struct {
		desc              string
		svc               *v1.Service
		ExpectedV4Enabled bool
		ExpectedV6Enabled bool
	}{
		{
			"IPv4",
			&v1.Service{
				Spec: v1.ServiceSpec{IPFamilies: []v1.IPFamily{v1.IPv4Protocol}},
			},
			true,
			false,
		},
		{
			"DualStack",
			&v1.Service{
				Spec: v1.ServiceSpec{IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol}},
			},
			true,
			true,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			v4Enabled, v6Enabled := getIPFamiliesEnabled(tc.svc)
			assert.Equal(t, tc.ExpectedV4Enabled, v4Enabled)
			assert.Equal(t, tc.ExpectedV6Enabled, v6Enabled)
		})
	}
}

func TestGetServiceLoadBalancerIP(t *testing.T) {
	testcases := []struct {
		desc       string
		svc        *v1.Service
		isIPv6     bool
		expectedIP string
	}{
		{
			"IPv6",
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "10.0.0.1",
						consts.ServiceAnnotationLoadBalancerIPDualStack[true]:  "2001::1",
					},
				},
				Spec: v1.ServiceSpec{
					LoadBalancerIP: "10.0.0.2",
				},
			},
			true,
			"2001::1",
		},
		{
			"IPv4 but from LoadBalancerIP",
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
				Spec: v1.ServiceSpec{
					LoadBalancerIP: "10.0.0.2",
				},
			},
			false,
			"10.0.0.2",
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			ip := getServiceLoadBalancerIP(tc.svc, tc.isIPv6)
			assert.Equal(t, tc.expectedIP, ip)
		})
	}
}

func TestGetServiceLoadBalancerIPs(t *testing.T) {
	testcases := []struct {
		desc        string
		svc         *v1.Service
		expectedIPs []string
	}{
		{
			"Get IPv4 and IPv6 IPs",
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "10.0.0.1",
						consts.ServiceAnnotationLoadBalancerIPDualStack[true]:  "2001::1",
					},
				},
			},
			[]string{"10.0.0.1", "2001::1"},
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			ips := getServiceLoadBalancerIPs(tc.svc)
			assert.Equal(t, tc.expectedIPs, ips)
		})
	}
}

func TestSetServiceLoadBalancerIP(t *testing.T) {
	testcases := []struct {
		desc        string
		ip          string
		svc         *v1.Service
		expectedSvc *v1.Service
	}{
		{
			"IPv4",
			"10.0.0.1",
			&v1.Service{},
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerIPDualStack[consts.IPVersionIPv4]: "10.0.0.1",
					},
				},
			},
		},
		{
			"IPv6",
			"2001::1",
			&v1.Service{},
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationLoadBalancerIPDualStack[consts.IPVersionIPv6]: "2001::1",
					},
				},
			},
		},
		{
			"empty IP",
			"",
			&v1.Service{},
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{},
			},
		},
		{
			"invalid IP",
			"invalid-ip",
			&v1.Service{},
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{},
			},
		},
		{
			"empty Service",
			"10.0.0.1",
			nil,
			nil,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			setServiceLoadBalancerIP(tc.svc, tc.ip)
			assert.Equal(t, tc.expectedSvc, tc.svc)
		})
	}
}

func TestGetServicePIPName(t *testing.T) {
	testcases := []struct {
		desc         string
		svc          *v1.Service
		isIPv6       bool
		expectedName string
	}{
		{
			"From ServiceAnnotationPIPName IPv4 single stack",
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationPIPNameDualStack[false]: "pip-name",
					},
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
				},
			},
			false,
			"pip-name",
		},
		{
			"From ServiceAnnotationPIPName IPv6 single stack",
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationPIPNameDualStack[false]: "pip-name-ipv6",
					},
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv6Protocol},
				},
			},
			true,
			"pip-name-ipv6",
		},
		{
			"From ServiceAnnotationPIPName IPv4",
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationPIPNameDualStack[false]: "pip-name",
						consts.ServiceAnnotationPIPNameDualStack[true]:  "pip-name-ipv6",
					},
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
			false,
			"pip-name",
		},
		{
			"From ServiceAnnotationPIPName IPv6",
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationPIPNameDualStack[false]: "pip-name",
						consts.ServiceAnnotationPIPNameDualStack[true]:  "pip-name-ipv6",
					},
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
			true,
			"pip-name-ipv6",
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			name := getServicePIPName(tc.svc, tc.isIPv6)
			assert.Equal(t, tc.expectedName, name)
		})
	}
}

func TestGetServicePIPPrefixID(t *testing.T) {
	testcases := []struct {
		desc       string
		svc        *v1.Service
		isIPv6     bool
		expectedID string
	}{
		{
			"From ServiceAnnotationPIPPrefixIDDualStack IPv4 single stack",
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationPIPPrefixIDDualStack[false]: "pip-prefix-id",
					},
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
				},
			},
			false,
			"pip-prefix-id",
		},
		{
			"From ServiceAnnotationPIPPrefixIDDualStack IPv6 single stack",
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationPIPPrefixIDDualStack[false]: "pip-prefix-id-ipv6",
					},
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv6Protocol},
				},
			},
			true,
			"pip-prefix-id-ipv6",
		},
		{
			"From ServiceAnnotationPIPPrefixIDDualStack IPv4",
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationPIPPrefixIDDualStack[false]: "pip-prefix-id",
						consts.ServiceAnnotationPIPPrefixIDDualStack[true]:  "pip-prefix-id-ipv6",
					},
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
			false,
			"pip-prefix-id",
		},
		{
			"From ServiceAnnotationPIPPrefixIDDualStack IPv6",
			&v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						consts.ServiceAnnotationPIPPrefixIDDualStack[false]: "pip-prefix-id",
						consts.ServiceAnnotationPIPPrefixIDDualStack[true]:  "pip-prefix-id-ipv6",
					},
				},
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
			true,
			"pip-prefix-id-ipv6",
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			id := getServicePIPPrefixID(tc.svc, tc.isIPv6)
			assert.Equal(t, tc.expectedID, id)
		})
	}
}

func TestGetResourceByIPFamily(t *testing.T) {
	testcases := []struct {
		desc             string
		resource         string
		isIPv6           bool
		isDualStack      bool
		expectedResource string
	}{
		{
			"DualStack - IPv4",
			"resource0",
			false,
			true,
			"resource0",
		},
		{
			"DualStack - IPv6",
			"resource0",
			true,
			true,
			"resource0-IPv6",
		},
		{
			"SingleStack - IPv4",
			"resource0",
			false,
			false,
			"resource0",
		},
		{
			"SingleStack - IPv6",
			"resource0",
			true,
			false,
			"resource0",
		},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			resource := getResourceByIPFamily(tc.resource, tc.isDualStack, tc.isIPv6)
			assert.Equal(t, tc.expectedResource, resource)
		})
	}
}

func TestIsFIPIPv6(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	testcases := []struct {
		desc           string
		svc            v1.Service
		fip            *network.FrontendIPConfiguration
		expectedIsIPv6 bool
	}{
		{
			desc: "IPv4",
			svc: v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
				},
			},
			fip:            nil,
			expectedIsIPv6: false,
		},
		{
			desc: "IPv6",
			svc: v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv6Protocol},
				},
			},
			fip:            nil,
			expectedIsIPv6: true,
		},
		{
			desc: "DualStack IPv4",
			svc: v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
			fip: &network.FrontendIPConfiguration{
				Name: ptr.To("fip"),
			},
			expectedIsIPv6: false,
		},
		{
			desc: "DualStack IPv6",
			svc: v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
			fip: &network.FrontendIPConfiguration{
				Name: ptr.To("fip-IPv6"),
			},
			expectedIsIPv6: true,
		},
	}
	for _, tc := range testcases {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			az := GetTestCloud(ctrl)
			isIPv6, err := az.isFIPIPv6(&tc.svc, "rg", tc.fip)
			assert.Nil(t, err)
			assert.Equal(t, tc.expectedIsIPv6, isIPv6)
		})
	}
}

func TestGetResourceIDPrefix(t *testing.T) {
	testcases := []struct {
		desc           string
		id             string
		expectedPrefix string
	}{
		{"normal", "a/b/c", "a/b"},
		{"no-slash", "ab", "ab"},
	}
	for _, tc := range testcases {
		t.Run(tc.desc, func(t *testing.T) {
			prefix := getResourceIDPrefix(tc.id)
			assert.Equal(t, tc.expectedPrefix, prefix)
		})
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
				Name: ptr.To("test-internal"),
			},
			expected: true,
		},
		{
			name: "internal load balancer",
			lb: network.LoadBalancer{
				Name: ptr.To("TEST-INTERNAL"),
			},
			expected: true,
		},
		{
			name: "not internal load balancer",
			lb: network.LoadBalancer{
				Name: ptr.To("test"),
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

func TestToArmcomputeDisk(t *testing.T) {
	type args struct {
		disks []compute.DataDisk
	}
	tests := []struct {
		name    string
		args    args
		want    []*armcompute.DataDisk
		wantErr bool
	}{
		{
			name: "normal",
			args: args{
				disks: []compute.DataDisk{
					{
						Name: ptr.To("disk1"),
					},
				},
			},
			want: []*armcompute.DataDisk{
				{
					Name: ptr.To("disk1"),
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ToArmcomputeDisk(tt.args.disks)
			if (err != nil) != tt.wantErr {
				t.Errorf("ToArmcomputeDisk() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ToArmcomputeDisk() = %v, want %v", got, tt.want)
			}
		})
	}
}
