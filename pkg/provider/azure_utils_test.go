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
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-08-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
				"a": to.StringPtr("b"),
			},
			newTags: map[string]*string{
				"a": to.StringPtr("c"),
				"b": to.StringPtr("d"),
			},
			expectedTags: map[string]*string{
				"a": to.StringPtr("c"),
				"b": to.StringPtr("d"),
			},
			expectedChanged: true,
		},
		{
			description: "reconcileTags should remove the tags that are not included in systemTags",
			currentTagsOnResource: map[string]*string{
				"a": to.StringPtr("b"),
				"c": to.StringPtr("d"),
			},
			newTags: map[string]*string{
				"a": to.StringPtr("c"),
			},
			systemTags: "a, b",
			expectedTags: map[string]*string{
				"a": to.StringPtr("c"),
			},
			expectedChanged: true,
		},
		{
			description: "reconcileTags should ignore the case of keys when comparing",
			currentTagsOnResource: map[string]*string{
				"A": to.StringPtr("b"),
				"c": to.StringPtr("d"),
			},
			newTags: map[string]*string{
				"a": to.StringPtr("b"),
				"C": to.StringPtr("d"),
			},
			expectedTags: map[string]*string{
				"A": to.StringPtr("b"),
				"c": to.StringPtr("d"),
			},
		},
		{
			description: "reconcileTags should ignore the case of values when comparing",
			currentTagsOnResource: map[string]*string{
				"A": to.StringPtr("b"),
				"c": to.StringPtr("d"),
			},
			newTags: map[string]*string{
				"a": to.StringPtr("B"),
				"C": to.StringPtr("D"),
			},
			expectedTags: map[string]*string{
				"A": to.StringPtr("b"),
				"c": to.StringPtr("d"),
			},
		},
		{
			description: "reconcileTags should ignore the case of keys when checking systemTags",
			currentTagsOnResource: map[string]*string{
				"a": to.StringPtr("b"),
				"c": to.StringPtr("d"),
			},
			newTags: map[string]*string{
				"a": to.StringPtr("c"),
			},
			systemTags: "A, b",
			expectedTags: map[string]*string{
				"a": to.StringPtr("c"),
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
					Name: to.StringPtr("rule1"),
				},
				{
					Name: to.StringPtr("rule2"),
				},
			},
			expected: []network.SecurityRule{
				{
					Name: to.StringPtr("rule1"),
				},
				{
					Name: to.StringPtr("rule2"),
				},
			},
		},
		{
			description: "duplicated rules",
			rules: []network.SecurityRule{
				{
					Name: to.StringPtr("rule1"),
				},
				{
					Name: to.StringPtr("rule2"),
				},
				{
					Name: to.StringPtr("rule1"),
				},
			},
			expected: []network.SecurityRule{
				{
					Name: to.StringPtr("rule2"),
				},
				{
					Name: to.StringPtr("rule1"),
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
