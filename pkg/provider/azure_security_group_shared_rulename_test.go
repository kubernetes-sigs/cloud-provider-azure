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

package provider

import "testing"

func TestParse(t *testing.T) {

	testcases := []struct {
		description string
		input       string
		wantError   bool
		expected    map[string]map[string]map[string]struct{}
	}{
		{
			description: "invalid input text",
			input:       "@#$%%^&&",
			wantError:   true,
		},
		{
			description: "empty",
			input:       "",
			wantError:   false,
			expected:    nil,
		},
		{
			description: "space should be ignored",
			input:       "192.168.1.1/24:default/svc1&svc2; ",
			wantError:   false,
			expected: map[string]map[string]map[string]struct{}{
				"192.168.1.1/24": {
					"default": {
						"svc1": struct{}{},
						"svc2": struct{}{},
					},
				},
			},
		},
		{
			description: "empty destination prefix should be valid",
			input:       ";;; ",
			wantError:   false,
		},
		{
			description: "empty destination prefix should be valid",
			input:       "192.168.1.1/24:default/svc1&svc2;; ",
			wantError:   false,
			expected: map[string]map[string]map[string]struct{}{
				"192.168.1.1/24": {
					"default": {
						"svc1": struct{}{},
						"svc2": struct{}{},
					},
				},
			},
		},
		{
			description: "empty destination prefix should be valid",
			input:       "192.168.1.1/24:;; ",
			wantError:   true,
		},
		{
			description: "namespace is default when not specified",
			input:       "192.168.1.1/24:svc&svc2&svc3;; ", //192.168.1.1/24:default/svc1&svc2
			wantError:   false,
			expected: map[string]map[string]map[string]struct{}{
				"192.168.1.1/24": {
					"default": {
						"svc1": struct{}{},
						"svc2": struct{}{},
						"svc3": struct{}{},
					},
				},
			},
		},
		{
			description: "namespace is default when not specified",
			input:       "192.168.1.1/24:svc&svc2&svc3,svc2&svc5;; ", //192.168.1.1/24:default/svc1&svc2,default/svc2&svc5
			wantError:   false,
			expected: map[string]map[string]map[string]struct{}{
				"192.168.1.1/24": {
					"default": {
						"svc1": struct{}{},
						"svc2": struct{}{},
						"svc3": struct{}{},
						"svc5": struct{}{},
					},
				},
			},
		},
		{
			description: "namespace is default when not specified",
			input:       "192.168.1.1/24:svc&svc2&svc3,svc2&svc5;; ", //192.168.1.1/24:default/svc1&svc2,default/svc2&svc5
			expected: map[string]map[string]map[string]struct{}{
				"192.168.1.1/24": {
					"default": {
						"svc1": struct{}{},
						"svc2": struct{}{},
						"svc3": struct{}{},
						"svc5": struct{}{},
					},
				},
			},
			wantError: false,
		},
		{
			description: "multiple destination should be valid",
			input:       "192.168.1.1/24:svc&svc2&svc3,svc2&svc5; 192.168.1.1/24:kube-system/ test;", //192.168.1.1/24:default/svc1&svc2,default/svc2&svc5
			wantError:   false,
			expected: map[string]map[string]map[string]struct{}{
				"192.168.1.1/24": {
					"default": {
						"svc1": struct{}{},
						"svc2": struct{}{},
						"svc3": struct{}{},
						"svc5": struct{}{},
					},
					"kube-system": {
						"test": struct{}{},
					},
				},
			},
		},
	}
	for _, testcase := range testcases {
		lexer := &SecurityGroupSharedRuleNameLexerImpl{line: []byte(testcase.input)}
		NSGSharedRuleNewParser().Parse(lexer)

		if !testcase.wantError && len(lexer.Errs) > 0 {
			t.Error(lexer.Errs)
		} else if testcase.wantError && len(lexer.Errs) == 0 {
			t.Errorf("expected error but got none")
		}
		if testcase.wantError {
			return
		}
		if len(testcase.expected) != len(lexer.DestinationPrefixes) {
			t.Errorf("case %s: expected %d results but got %+v", testcase.description, len(testcase.expected), lexer.DestinationPrefixes)
		}
		t.Logf("case %s: %+v", testcase.description, lexer.DestinationPrefixes)
		for expectedPrefix, expectedNSList := range testcase.expected {
			if foundList, ok := lexer.DestinationPrefixes[expectedPrefix]; !ok {
				t.Errorf("case %s: expected prefix %s but not found", testcase.description, expectedPrefix)
			} else if len(foundList) != len(expectedNSList) {
				t.Errorf("case %s: expected %d namespaces but got %+v", testcase.description, len(expectedNSList), foundList)
			}
			for expectedNS, expectedSvcList := range expectedNSList {
				if foundSvcList, ok := lexer.DestinationPrefixes[expectedPrefix][expectedNS]; !ok {
					t.Errorf("case %s: expected namespace %s but not found", testcase.description, expectedNS)
				} else if len(foundSvcList) != len(expectedSvcList) {
					t.Errorf("case %s: expected %d services but got %+v", testcase.description, len(expectedSvcList), foundSvcList)
				}
				for expectedSvc := range expectedSvcList {
					if _, ok := lexer.DestinationPrefixes[expectedPrefix][expectedNS][expectedSvc]; !ok {
						t.Errorf("case %s: expected service %s but not found", testcase.description, expectedSvc)
					}
				}
			}
		}
	}
}
