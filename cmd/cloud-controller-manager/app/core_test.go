/*
Copyright 2021 The Kubernetes Authors.

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

package app

import (
	"testing"

	"github.com/stretchr/testify/assert"

	nodeipamconfig "sigs.k8s.io/cloud-provider-azure/pkg/nodeipam/config"
)

func TestSetNodeCIDRMaskSizesDualStack(t *testing.T) {
	for _, testCase := range []struct {
		description                        string
		mask, ipv4Mask, ipv6Mask           int32
		expectedIPV4Mask, expectedIPV6Mask int
	}{
		{
			description:      "setNodeCIDRMaskSizesDualStack should ignore the node cidr mask size",
			mask:             17,
			ipv6Mask:         65,
			expectedIPV4Mask: 24,
			expectedIPV6Mask: 65,
		},
		{
			description:      "setNodeCIDRMaskSizesDualStack should set the ipv4 and ipv6 mask sizes as configured",
			mask:             17,
			ipv4Mask:         18,
			ipv6Mask:         65,
			expectedIPV4Mask: 18,
			expectedIPV6Mask: 65,
		},
		{
			description:      "setNodeCIDRMaskSizesDualStack should set the default ipv4 and ipv6 mask sizes",
			mask:             17,
			expectedIPV4Mask: 24,
			expectedIPV6Mask: 64,
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			cfg := nodeipamconfig.NodeIPAMControllerConfiguration{
				NodeCIDRMaskSize:     testCase.mask,
				NodeCIDRMaskSizeIPv4: testCase.ipv4Mask,
				NodeCIDRMaskSizeIPv6: testCase.ipv6Mask,
			}

			ipv4Mask, ipv6Mask, err := setNodeCIDRMaskSizesDualStack(cfg)
			assert.NoError(t, err)
			assert.Equal(t, testCase.expectedIPV4Mask, ipv4Mask)
			assert.Equal(t, testCase.expectedIPV6Mask, ipv6Mask)
		})
	}
}
