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

package providererrors_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	providererrors "sigs.k8s.io/cloud-provider-azure/pkg/provider/errors"
)

func TestLoadBalancerIPErrors(t *testing.T) {
	for _, tc := range []struct {
		description string
		err         error
		target      error
		expected    string
	}{
		{
			description: "invalid LoadBalancer IP",
			err:         providererrors.NewInvalidLoadBalancerIPError("not-an-ip"),
			target:      providererrors.ErrInvalidLoadBalancerIP,
			expected:    `invalid LoadBalancer IP "not-an-ip"`,
		},
		{
			description: "non-public LoadBalancer IP",
			err:         providererrors.NewNonPublicLoadBalancerIPError("10.0.0.5"),
			target:      providererrors.ErrNonPublicLoadBalancerIP,
			expected:    `non-public LoadBalancer IP "10.0.0.5"`,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			assert.ErrorIs(t, tc.err, tc.target)
			assert.EqualError(t, tc.err, tc.expected)
			assert.True(t, providererrors.IsLoadBalancerIPValidationError(tc.err))
		})
	}
}

func TestNewExternalServiceLoadBalancerIPError(t *testing.T) {
	unknownErr := errors.New("unexpected")
	for _, tc := range []struct {
		description    string
		loadBalancerIP string
		err            error
		target         error
		expected       string
		expectedSame   bool
	}{
		{
			description:    "invalid LoadBalancer IP",
			loadBalancerIP: "not-an-ip",
			err:            providererrors.NewInvalidLoadBalancerIPError("not-an-ip"),
			target:         providererrors.ErrInvalidLoadBalancerIP,
			expected:       `external Service "default/test" has invalid LoadBalancer IP "not-an-ip"`,
		},
		{
			description:    "non-public LoadBalancer IP",
			loadBalancerIP: "10.0.0.5",
			err:            providererrors.NewNonPublicLoadBalancerIPError("10.0.0.5"),
			target:         providererrors.ErrNonPublicLoadBalancerIP,
			expected:       `external Service "default/test" cannot use non-public LoadBalancer IP "10.0.0.5"; use an internal LoadBalancer annotation or a valid Azure Public IP address`,
		},
		{
			description:    "unknown error",
			loadBalancerIP: "1.2.3.4",
			err:            unknownErr,
			expected:       "unexpected",
			expectedSame:   true,
		},
	} {
		t.Run(tc.description, func(t *testing.T) {
			err := providererrors.NewExternalServiceLoadBalancerIPError("default/test", tc.loadBalancerIP, tc.err)

			if tc.target != nil {
				assert.ErrorIs(t, err, tc.target)
			}
			if tc.expectedSame {
				assert.Same(t, unknownErr, err)
			}
			assert.EqualError(t, err, tc.expected)
		})
	}
}
