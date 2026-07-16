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

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// TestGuardV9WireValues_InboundResources pins the exact ARM wire values that the armnetwork v6->v9
// migration must preserve for the ServiceGateway inbound LoadBalancer path. The v9 SDK dropped the
// "Service" LoadBalancerSKUName enum (it now only ships Basic/Gateway/Standard), so the value is sent
// as a case-sensitive string cast; a silent change to that cast — or to the StandardV2 PIP SKU or the
// Public frontend scope — would be accepted by the compiler but rejected (or mis-handled) by NRP. This
// guard exercises the real builder and fails if any wire value drifts.
func TestGuardV9WireValues_InboundResources(t *testing.T) {
	pip, lb, _, err := buildInboundServiceResources("svc-wire-guard", makeInboundConfig(80), testConfig())
	assert.NoError(t, err)

	// Public IP SKU must be StandardV2 (case-sensitive wire value).
	if assert.NotNil(t, pip.SKU) && assert.NotNil(t, pip.SKU.Name) {
		assert.Equal(t, armnetwork.PublicIPAddressSKUNameStandardV2, *pip.SKU.Name)
		assert.Equal(t, "StandardV2", string(*pip.SKU.Name))
	}
	if assert.NotNil(t, pip.Properties) && assert.NotNil(t, pip.Properties.PublicIPAllocationMethod) {
		assert.Equal(t, armnetwork.IPAllocationMethodStatic, *pip.Properties.PublicIPAllocationMethod)
	}

	// LoadBalancer SKU must be the case-sensitive "Service" string (no v9 enum exists for it).
	if assert.NotNil(t, lb.SKU) && assert.NotNil(t, lb.SKU.Name) {
		assert.Equal(t, "Service", string(*lb.SKU.Name),
			"inbound LB SKU must be sent as case-sensitive %q", consts.LoadBalancerARMSKUService)
		assert.Equal(t, consts.LoadBalancerARMSKUService, string(*lb.SKU.Name))
	}

	// Frontend must reference a Public IP — the SGW inbound path always provisions a public frontend.
	if assert.NotNil(t, lb.Properties) && assert.NotEmpty(t, lb.Properties.FrontendIPConfigurations) {
		fe := lb.Properties.FrontendIPConfigurations[0]
		if assert.NotNil(t, fe.Properties) {
			assert.NotNil(t, fe.Properties.PublicIPAddress, "inbound frontend must reference a public IP")
		}
	}
}

// TestGuardV9WireValues_OutboundResources pins the StandardV2 wire values for the egress NAT Gateway
// path (PIP SKU + NAT Gateway SKU), which the v6->v9 migration must preserve.
func TestGuardV9WireValues_OutboundResources(t *testing.T) {
	pip, natGateway, _ := buildOutboundServiceResources("egress-wire-guard", &OutboundConfig{}, testConfig())

	if assert.NotNil(t, pip.SKU) && assert.NotNil(t, pip.SKU.Name) {
		assert.Equal(t, armnetwork.PublicIPAddressSKUNameStandardV2, *pip.SKU.Name)
		assert.Equal(t, "StandardV2", string(*pip.SKU.Name))
	}

	if assert.NotNil(t, natGateway.SKU) && assert.NotNil(t, natGateway.SKU.Name) {
		assert.Equal(t, armnetwork.NatGatewaySKUNameStandardV2, *natGateway.SKU.Name)
		assert.Equal(t, "StandardV2", string(*natGateway.SKU.Name))
	}
}

// TestGuardV9WireValues_EnumStrings pins the vendored v9 enum strings the feature relies on,
// independent of the builder wiring, so a vendor re-sync that changes a wire value is caught.
func TestGuardV9WireValues_EnumStrings(t *testing.T) {
	assert.Equal(t, "Public", string(armnetwork.LoadBalancerScopePublic))
	assert.Equal(t, "StandardV2", string(armnetwork.PublicIPAddressSKUNameStandardV2))
	assert.Equal(t, "StandardV2", string(armnetwork.NatGatewaySKUNameStandardV2))
	assert.Equal(t, "Service", consts.LoadBalancerARMSKUService)
}
