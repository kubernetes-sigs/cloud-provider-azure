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

package difftracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVNetResourceGroupOrDefault(t *testing.T) {
	tests := []struct {
		name              string
		resourceGroup     string
		vnetResourceGroup string
		expected          string
	}{
		{
			name:              "returns the VNet resource group when set",
			resourceGroup:     "cluster-rg",
			vnetResourceGroup: "vnet-rg",
			expected:          "vnet-rg",
		},
		{
			name:          "falls back to the cluster resource group when unset",
			resourceGroup: "cluster-rg",
			expected:      "cluster-rg",
		},
		{
			name:              "prefers the VNet resource group even when both match",
			resourceGroup:     "cluster-rg",
			vnetResourceGroup: "cluster-rg",
			expected:          "cluster-rg",
		},
		{
			name:     "returns empty when neither is set",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := Config{
				ResourceGroup:     tt.resourceGroup,
				VNetResourceGroup: tt.vnetResourceGroup,
			}
			assert.Equal(t, tt.expected, config.VNetResourceGroupOrDefault())
		})
	}
}
