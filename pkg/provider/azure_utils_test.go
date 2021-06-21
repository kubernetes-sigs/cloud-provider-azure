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
	"testing"
	"time"

	"github.com/Azure/go-autorest/autorest/to"
	"github.com/stretchr/testify/assert"
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
