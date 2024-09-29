/*
Copyright 2023 The Kubernetes Authors.

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

package testutil

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"
)

// ExpectHasSecurityRules asserts the security group whether it has the given rules.
func ExpectHasSecurityRules(t *testing.T, sg *armnetwork.SecurityGroup, expected []*armnetwork.SecurityRule, msgAndArgs ...any) {
	t.Helper()

	expectedRuleIndex := make(map[string]*armnetwork.SecurityRule)
	for _, rule := range expected {
		expectedRuleIndex[*rule.Name] = rule
	}

	for _, actual := range sg.Properties.SecurityRules {
		expected, found := expectedRuleIndex[*actual.Name]
		if !found {
			continue
		}
		ExpectEqualInJSON(t, expected, actual, msgAndArgs...)
		delete(expectedRuleIndex, *actual.Name)
	}

	// If empty, some expected rules are not found in the security group.
	assert.Empty(t, expectedRuleIndex, msgAndArgs...)
}

// ExpectExactSecurityRules asserts the security group whether it has the exact same rules.
func ExpectExactSecurityRules(t *testing.T, sg *armnetwork.SecurityGroup, expected []*armnetwork.SecurityRule, msgAndArgs ...any) {
	t.Helper()

	assert.NotNil(t, sg)
	assert.NotNil(t, sg.Properties)
	assert.NotNil(t, sg.Properties.SecurityRules)

	actual := sg.Properties.SecurityRules

	// order insensitive
	sort.Slice(actual, func(i, j int) bool {
		return *actual[i].Properties.Priority < *actual[j].Properties.Priority
	})
	sort.Slice(expected, func(i, j int) bool {
		return *expected[i].Properties.Priority < *expected[j].Properties.Priority
	})

	ExpectEqualInJSON(t, expected, actual, msgAndArgs...)
}

func ExpectEqualInJSON(t *testing.T, expected, actual any, msgAndArgs ...any) {
	t.Helper()

	actualJSON, err := json.Marshal(actual)
	assert.NoError(t, err)

	expectedJSON, err := json.Marshal(expected)
	assert.NoError(t, err)

	// convert to string for better readability
	assert.Equal(t, string(expectedJSON), string(actualJSON), msgAndArgs...)
}
