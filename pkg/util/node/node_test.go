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

package node

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
)

func TestPreserveExternalIPs(t *testing.T) {
	hostname := v1.NodeAddress{Type: v1.NodeHostName, Address: "vm1"}
	internalV4 := v1.NodeAddress{Type: v1.NodeInternalIP, Address: "10.0.0.4"}
	internalV4New := v1.NodeAddress{Type: v1.NodeInternalIP, Address: "10.0.0.5"}
	internalV6 := v1.NodeAddress{Type: v1.NodeInternalIP, Address: "fd00::4"}
	externalV4 := v1.NodeAddress{Type: v1.NodeExternalIP, Address: "20.0.0.1"}
	externalV4Other := v1.NodeAddress{Type: v1.NodeExternalIP, Address: "20.0.0.2"}
	externalV6 := v1.NodeAddress{Type: v1.NodeExternalIP, Address: "2001:db8::1"}
	internalDNS := v1.NodeAddress{Type: v1.NodeInternalDNS, Address: "vm1.internal"}
	externalDNS := v1.NodeAddress{Type: v1.NodeExternalDNS, Address: "vm1.example.com"}

	testCases := []struct {
		desc     string
		recorded []v1.NodeAddress
		fresh    []v1.NodeAddress
		expected []v1.NodeAddress
	}{
		{
			desc:     "nil inputs should return an empty slice",
			expected: []v1.NodeAddress{},
		},
		{
			desc:     "empty recorded should return fresh unchanged",
			recorded: []v1.NodeAddress{},
			fresh:    []v1.NodeAddress{hostname, internalV4},
			expected: []v1.NodeAddress{hostname, internalV4},
		},
		{
			desc:     "empty fresh should return only recorded ExternalIPs",
			recorded: []v1.NodeAddress{hostname, internalV4, externalV4},
			fresh:    []v1.NodeAddress{},
			expected: []v1.NodeAddress{externalV4},
		},
		{
			desc:     "recorded ExternalIP should be appended after fresh addresses while fresh private IP changes",
			recorded: []v1.NodeAddress{hostname, internalV4, externalV4},
			fresh:    []v1.NodeAddress{hostname, internalV4New},
			expected: []v1.NodeAddress{hostname, internalV4New, externalV4},
		},
		{
			desc:     "duplicate fresh entries should be preserved unchanged",
			recorded: []v1.NodeAddress{externalV4},
			fresh:    []v1.NodeAddress{hostname, internalV4, internalV4, hostname},
			expected: []v1.NodeAddress{hostname, internalV4, internalV4, hostname, externalV4},
		},
		{
			desc:     "duplicate fresh ExternalIPs should be preserved unchanged",
			recorded: []v1.NodeAddress{externalV4},
			fresh:    []v1.NodeAddress{hostname, externalV4, externalV4},
			expected: []v1.NodeAddress{hostname, externalV4, externalV4},
		},
		{
			desc:     "recorded ExternalIP already in fresh should not be duplicated",
			recorded: []v1.NodeAddress{externalV4},
			fresh:    []v1.NodeAddress{hostname, internalV4, externalV4},
			expected: []v1.NodeAddress{hostname, internalV4, externalV4},
		},
		{
			desc:     "duplicate recorded ExternalIPs should be deduplicated against the growing result",
			recorded: []v1.NodeAddress{externalV4, externalV4Other, externalV4},
			fresh:    []v1.NodeAddress{hostname, internalV4},
			expected: []v1.NodeAddress{hostname, internalV4, externalV4, externalV4Other},
		},
		{
			desc:     "recorded ExternalIPs of both families should be preserved in recorded order",
			recorded: []v1.NodeAddress{hostname, internalV4, externalV4, internalV6, externalV6},
			fresh:    []v1.NodeAddress{hostname, internalV4, internalV6},
			expected: []v1.NodeAddress{hostname, internalV4, internalV6, externalV4, externalV6},
		},
		{
			desc:     "stale private family should not be rescued while its ExternalIP is kept",
			recorded: []v1.NodeAddress{hostname, internalV4, externalV4, internalV6, externalV6},
			fresh:    []v1.NodeAddress{hostname, internalV4},
			expected: []v1.NodeAddress{hostname, internalV4, externalV4, externalV6},
		},
		{
			desc:     "all non-ExternalIP recorded addresses should be ignored",
			recorded: []v1.NodeAddress{hostname, internalV4, internalV6, internalDNS, externalDNS},
			fresh:    []v1.NodeAddress{internalV4New},
			expected: []v1.NodeAddress{internalV4New},
		},
		{
			desc:     "recorded address with the same value but different type should not be treated as present",
			recorded: []v1.NodeAddress{{Type: v1.NodeExternalIP, Address: "10.0.0.4"}},
			fresh:    []v1.NodeAddress{internalV4},
			expected: []v1.NodeAddress{internalV4, {Type: v1.NodeExternalIP, Address: "10.0.0.4"}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			recordedBefore := slices.Clone(tc.recorded)
			freshBefore := slices.Clone(tc.fresh)

			result := PreserveExternalIPs(tc.recorded, tc.fresh)

			assert.Equal(t, tc.expected, result)
			assert.Equal(t, recordedBefore, tc.recorded)
			assert.Equal(t, freshBefore, tc.fresh)
		})
	}
}

func TestPreserveExternalIPsReturnsNewSlice(t *testing.T) {
	fresh := make([]v1.NodeAddress, 2, 8)
	fresh[0] = v1.NodeAddress{Type: v1.NodeHostName, Address: "vm1"}
	fresh[1] = v1.NodeAddress{Type: v1.NodeInternalIP, Address: "10.0.0.4"}
	recorded := []v1.NodeAddress{{Type: v1.NodeExternalIP, Address: "20.0.0.1"}}

	result := PreserveExternalIPs(recorded, fresh)
	assert.Equal(t, []v1.NodeAddress{fresh[0], fresh[1], recorded[0]}, result)

	// The no-op path without recorded ExternalIPs must still return a new slice.
	noop := PreserveExternalIPs(nil, fresh)
	assert.Equal(t, fresh, noop)
	noop[0].Address = "changed"
	assert.Equal(t, "vm1", fresh[0].Address)

	// Appending must not write into the spare capacity of fresh.
	spare := fresh[len(fresh):cap(fresh)]
	assert.Equal(t, make([]v1.NodeAddress, len(spare)), spare)

	// Mutating the result must not affect either input.
	result[0].Address = "changed"
	result[2].Address = "changed"
	assert.Equal(t, "vm1", fresh[0].Address)
	assert.Equal(t, "20.0.0.1", recorded[0].Address)
}

func TestErrLoadBalancerMetadataUnavailable(t *testing.T) {
	cause := errors.New("connection reset")
	err := fmt.Errorf("failed to get loadbalancer metadata: %w: %w", ErrLoadBalancerMetadataUnavailable, cause)

	assert.Equal(t, "load balancer metadata unavailable", ErrLoadBalancerMetadataUnavailable.Error())
	assert.Equal(t, "failed to get loadbalancer metadata: load balancer metadata unavailable: connection reset", err.Error())
	assert.True(t, errors.Is(err, ErrLoadBalancerMetadataUnavailable))
	assert.True(t, errors.Is(err, cause))
	assert.False(t, errors.Is(cause, ErrLoadBalancerMetadataUnavailable))
}
