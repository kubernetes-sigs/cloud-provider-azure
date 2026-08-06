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
	"k8s.io/utils/ptr"
)

// TestInboundConfigEquals_ComparesEveryField pins that config drift detection sees every field it
// carries. Equals decides whether a user's spec change is dispatched to Azure at all, so a field it
// ignores is a field the user can edit with no effect.
func TestInboundConfigEquals_ComparesEveryField(t *testing.T) {
	base := func() *InboundConfig {
		return &InboundConfig{
			FrontendPorts:      []PortMapping{{Port: 80, Protocol: "TCP"}},
			BackendPorts:       []PortMapping{{Port: 8080, Protocol: "TCP"}},
			IdleTimeoutMinutes: ptr.To[int32](10),
			IPFamilies:         []string{"IPv4"},
			NamedTargetPorts:   []string{"http"},
		}
	}

	cases := []struct {
		field  string
		mutate func(*InboundConfig)
	}{
		{"IdleTimeoutMinutes", func(c *InboundConfig) { c.IdleTimeoutMinutes = ptr.To[int32](20) }},
		{"IdleTimeoutMinutes cleared", func(c *InboundConfig) { c.IdleTimeoutMinutes = nil }},
		{"IPFamilies", func(c *InboundConfig) { c.IPFamilies = []string{"IPv6"} }},
		{"NamedTargetPorts", func(c *InboundConfig) { c.NamedTargetPorts = []string{"metrics"} }},
		{"FrontendPorts", func(c *InboundConfig) { c.FrontendPorts = []PortMapping{{Port: 443, Protocol: "TCP"}} }},
		{"BackendPorts", func(c *InboundConfig) { c.BackendPorts = []PortMapping{{Port: 9090, Protocol: "TCP"}} }},
	}

	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			changed := base()
			tc.mutate(changed)
			assert.False(t, base().Equals(changed), "a change to %s must be detected as drift", tc.field)
		})
	}

	assert.True(t, base().Equals(base()), "control: identical configs must compare equal")
}
