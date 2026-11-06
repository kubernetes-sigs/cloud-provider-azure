/*
Copyright 2026 The Kubernetes Authors.

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

package network

import (
	"errors"
	"testing"

	"github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
)

func TestRunCleanupActions(t *testing.T) {
	testcases := []struct {
		name        string
		failures    map[string]bool
		bodyFailure bool
	}{
		{name: "success"},
		{name: "IPv6 failure", failures: map[string]bool{"ipv6": true}},
		{name: "multiple failures", failures: map[string]bool{"ipv6": true, "ipv4": true}},
		{name: "service failure", failures: map[string]bool{"service": true}},
		{name: "body assertion failure", bodyFailure: true},
		{name: "body and cleanup failures", bodyFailure: true, failures: map[string]bool{"ipv6": true}},
	}
	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			var attempted, reported []string
			var actions []func()
			for _, resource := range []string{"service", "ipv6", "ipv4", "resource group"} {
				actions = append(actions, func() {
					attempted = append(attempted, resource)
					if testcase.failures[resource] {
						expect := gomega.NewGomega(func(message string, _ ...int) {
							reported = append(reported, resource)
							panic(message)
						})
						expect.Expect(errors.New(resource + " deletion failed")).NotTo(gomega.HaveOccurred())
					}
				})
			}
			bodyErr := errors.New("connectivity timeout")
			var panicValue any
			func() {
				defer func() { panicValue = recover() }()
				defer func() {
					defer runCleanupActions(actions[1:]...)
					actions[0]()
				}()
				if testcase.bodyFailure {
					panic(bodyErr)
				}
			}()
			assert.Equal(t, []string{"service", "ipv6", "ipv4", "resource group"}, attempted)
			assert.Len(t, reported, len(testcase.failures))
			switch {
			case len(testcase.failures) > 0:
				// The first failure encountered surfaces; a later cleanup action
				// must never mask an earlier failure.
				firstFailure := ""
				for _, resource := range []string{"service", "ipv6", "ipv4", "resource group"} {
					if testcase.failures[resource] {
						firstFailure = resource
						break
					}
				}
				msg, ok := panicValue.(string)
				assert.True(t, ok)
				assert.Contains(t, msg, firstFailure)
			case testcase.bodyFailure:
				// With no cleanup failure the in-flight failure propagates unchanged.
				assert.Same(t, bodyErr, panicValue)
			default:
				assert.Nil(t, panicValue)
			}
		})
	}
}
