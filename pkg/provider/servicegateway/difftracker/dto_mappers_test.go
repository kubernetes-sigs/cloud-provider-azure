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

	"sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

func makeInboundConfig(frontendPorts ...int32) *InboundConfig {
	cfg := &InboundConfig{}
	for _, p := range frontendPorts {
		cfg.FrontendPorts = append(cfg.FrontendPorts, PortMapping{Port: p, Protocol: "TCP"})
		cfg.BackendPorts = append(cfg.BackendPorts, PortMapping{Port: p, Protocol: "TCP"})
	}
	return cfg
}

func TestInboundConfig_Equals(t *testing.T) {
	a := makeInboundConfig(80, 443)
	b := makeInboundConfig(80, 443)
	assert.True(t, a.Equals(b), "identical configs should be equal")

	c := makeInboundConfig(80, 8080)
	assert.False(t, a.Equals(c), "different ports should be unequal")

	d := makeInboundConfig(443, 80)
	assert.False(t, a.Equals(d), "ordered comparison: reversed ports must be unequal")

	assert.True(t, (*InboundConfig)(nil).Equals(nil), "nil-nil equal")
	assert.False(t, a.Equals(nil), "non-nil vs nil unequal")
}

func TestMapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(t *testing.T) {
	tests := []struct {
		name        string
		lbUpdates   SyncServicesReturnType
		natUpdates  SyncServicesReturnType
		expectedLen int
	}{
		{
			name: "only inbound additions",
			lbUpdates: SyncServicesReturnType{
				Additions: sets.NewString("svc1", "svc2"),
			},
			natUpdates:  SyncServicesReturnType{},
			expectedLen: 2,
		},
		{
			name:      "only outbound additions",
			lbUpdates: SyncServicesReturnType{},
			natUpdates: SyncServicesReturnType{
				Additions: sets.NewString("egress1"),
			},
			expectedLen: 1,
		},
		{
			name: "mixed additions",
			lbUpdates: SyncServicesReturnType{
				Additions: sets.NewString("svc1"),
			},
			natUpdates: SyncServicesReturnType{
				Additions: sets.NewString("egress1"),
			},
			expectedLen: 2,
		},
		{
			name: "removals only",
			lbUpdates: SyncServicesReturnType{
				Removals: sets.NewString("svc1"),
			},
			natUpdates: SyncServicesReturnType{
				Removals: sets.NewString("egress1"),
			},
			expectedLen: 2,
		},
		{
			name:        "empty updates",
			lbUpdates:   SyncServicesReturnType{},
			natUpdates:  SyncServicesReturnType{},
			expectedLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(tt.lbUpdates, tt.natUpdates, "sub1", "rg1")
			assert.Equal(t, tt.expectedLen, len(result.Services))
		})
	}
}
