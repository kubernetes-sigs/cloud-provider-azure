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
	"errors"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func TestServiceGatewayResourceNaming(t *testing.T) {
	service := &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: "SERVICE-UID"}}

	assert.Equal(t, "service-uid", ServiceUID(service))
	assert.Empty(t, ServiceUID(nil))
	assert.Equal(t, "service-uid-pip", PublicIPName(ServiceUID(service)))
}

func TestExtractInboundConfigFromService_NilService(t *testing.T) {
	config := ExtractInboundConfigFromService(nil)
	assert.Nil(t, config)
}

func TestValidateInboundConfig(t *testing.T) {
	tests := []struct {
		name       string
		config     *InboundConfig
		wantReason string
	}{
		{
			name:   "nil config is valid",
			config: nil,
		},
		{
			name: "single-stack TCP and UDP is valid",
			config: &InboundConfig{
				FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}, {Port: 53, Protocol: "UDP"}},
				BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}, {Port: 5353, Protocol: "UDP"}},
				IPFamilies:    []string{"IPv4"},
			},
		},
		{
			name: "named target port rejected",
			config: &InboundConfig{
				FrontendPorts:    []PortMapping{{Port: 80, Protocol: "TCP"}},
				BackendPorts:     []PortMapping{{Port: 80, Protocol: "TCP"}},
				NamedTargetPorts: []string{"http"},
			},
			wantReason: "UnsupportedNamedTargetPort",
		},
		{
			name: "non-TCP/UDP protocol rejected",
			config: &InboundConfig{
				FrontendPorts: []PortMapping{{Port: 132, Protocol: "SCTP"}},
				BackendPorts:  []PortMapping{{Port: 132, Protocol: "SCTP"}},
			},
			wantReason: "UnsupportedProtocol",
		},
		{
			name: "two service ports colliding on one backend port rejected",
			config: &InboundConfig{
				FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}, {Port: 81, Protocol: "TCP"}},
				BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}, {Port: 8080, Protocol: "TCP"}},
			},
			wantReason: "UnsupportedBackendPortCollision",
		},
		{
			name: "same backend port different protocol allowed",
			config: &InboundConfig{
				FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}, {Port: 80, Protocol: "UDP"}},
				BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}, {Port: 8080, Protocol: "UDP"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateInboundConfig(tt.config)
			if tt.wantReason == "" {
				assert.NoError(t, err)
				return
			}
			var ve *InboundConfigValidationError
			if assert.ErrorAs(t, err, &ve) {
				assert.Equal(t, tt.wantReason, ve.Reason)
				assert.NotEmpty(t, ve.Message)
			}
		})
	}
}

func TestExtractInboundConfigFromService_EmptyPorts(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: "default",
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{},
		},
	}
	config := ExtractInboundConfigFromService(service)
	assert.Nil(t, config)
}

func TestExtractInboundConfigFromService_SingleTCPPort(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: "default",
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       "http",
					Protocol:   v1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
				},
			},
		},
	}

	config := ExtractInboundConfigFromService(service)
	assert.NotNil(t, config)
	assert.Len(t, config.FrontendPorts, 1)
	assert.Len(t, config.BackendPorts, 1)

	// Check frontend port
	assert.Equal(t, int32(80), config.FrontendPorts[0].Port)
	assert.Equal(t, "TCP", config.FrontendPorts[0].Protocol)

	// Check backend port (should be TargetPort)
	assert.Equal(t, int32(8080), config.BackendPorts[0].Port)
	assert.Equal(t, "TCP", config.BackendPorts[0].Protocol)
}

func TestExtractInboundConfigFromService_MultiplePortsWithUDP(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: "default",
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       "http",
					Protocol:   v1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
				},
				{
					Name:       "dns",
					Protocol:   v1.ProtocolUDP,
					Port:       53,
					TargetPort: intstr.FromInt(5353),
				},
				{
					Name:       "https",
					Protocol:   v1.ProtocolTCP,
					Port:       443,
					TargetPort: intstr.FromInt(8443),
				},
			},
		},
	}

	config := ExtractInboundConfigFromService(service)
	assert.NotNil(t, config)
	assert.Len(t, config.FrontendPorts, 3)
	assert.Len(t, config.BackendPorts, 3)

	// Verify HTTP
	assert.Equal(t, int32(80), config.FrontendPorts[0].Port)
	assert.Equal(t, "TCP", config.FrontendPorts[0].Protocol)
	assert.Equal(t, int32(8080), config.BackendPorts[0].Port)

	// Verify DNS (UDP)
	assert.Equal(t, int32(53), config.FrontendPorts[1].Port)
	assert.Equal(t, "UDP", config.FrontendPorts[1].Protocol)
	assert.Equal(t, int32(5353), config.BackendPorts[1].Port)

	// Verify HTTPS
	assert.Equal(t, int32(443), config.FrontendPorts[2].Port)
	assert.Equal(t, "TCP", config.FrontendPorts[2].Protocol)
	assert.Equal(t, int32(8443), config.BackendPorts[2].Port)
}

func TestExtractInboundConfigFromService_NoTargetPort(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: "default",
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:     "http",
					Protocol: v1.ProtocolTCP,
					Port:     80,
					// TargetPort not specified
				},
			},
		},
	}

	config := ExtractInboundConfigFromService(service)
	assert.NotNil(t, config)

	// When TargetPort is not specified, backend port should equal frontend port
	assert.Equal(t, int32(80), config.FrontendPorts[0].Port)
	assert.Equal(t, int32(80), config.BackendPorts[0].Port)
}

func TestExtractInboundConfigFromService_NamedTargetPort(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: "default",
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       "http",
					Protocol:   v1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromString("http-port"), // Named port
				},
			},
		},
	}

	config := ExtractInboundConfigFromService(service)
	assert.NotNil(t, config)
	assert.Len(t, config.FrontendPorts, 1)
	assert.Len(t, config.BackendPorts, 1)

	assert.Equal(t, int32(80), config.FrontendPorts[0].Port)
	assert.Equal(t, config.FrontendPorts[0].Port, config.BackendPorts[0].Port)
}

func TestExtractInboundConfigFromService_EmptyProtocol(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-service",
			Namespace: "default",
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name: "http",
					Port: 80,
					// Protocol not specified
				},
			},
		},
	}

	config := ExtractInboundConfigFromService(service)
	assert.NotNil(t, config)

	// Default protocol should be TCP
	assert.Equal(t, "TCP", config.FrontendPorts[0].Protocol)
	assert.Equal(t, "TCP", config.BackendPorts[0].Protocol)
}

func TestBuildInboundServiceResources_WithConfig(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{
			{Port: 80, Protocol: "TCP"},
			{Port: 443, Protocol: "TCP"},
		},
		BackendPorts: []PortMapping{
			{Port: 8080, Protocol: "TCP"},
			{Port: 8443, Protocol: "TCP"},
		},
	}

	dtConfig := Config{
		SubscriptionID:                "test-sub",
		NetworkResourceSubscriptionID: "network-sub",
		ResourceGroup:                 "test-rg",
		Location:                      "eastus",
		ServiceGatewayResourceName:    "test-sgw",
	}

	pip, lb, servicesDTO, err := buildInboundServiceResources("service-uid-123", config, dtConfig)
	assert.NoError(t, err)

	// Verify PIP
	assert.NotNil(t, pip.Name)
	assert.Equal(t, "service-uid-123-pip", *pip.Name)
	assert.Contains(t, *pip.ID, "/subscriptions/network-sub/")
	assert.Equal(t, armnetwork.PublicIPAddressSKUNameStandardV2, *pip.SKU.Name)
	assert.Equal(t, "eastus", *pip.Location)

	// Verify LoadBalancer
	assert.NotNil(t, lb.Name)
	assert.Equal(t, "service-uid-123", *lb.Name)
	assert.Contains(t, *lb.ID, "/subscriptions/network-sub/")
	assert.Equal(t, "Service", string(*lb.SKU.Name))
	assert.Equal(t, "eastus", *lb.Location)

	// Verify backend pool
	assert.Len(t, lb.Properties.BackendAddressPools, 1)
	assert.Equal(t, "service-uid-123", *lb.Properties.BackendAddressPools[0].Name)

	// Verify LB rules
	assert.Len(t, lb.Properties.LoadBalancingRules, 2)

	// Rule 1: port 80 -> 8080
	rule1 := lb.Properties.LoadBalancingRules[0]
	assert.Equal(t, "rule-tcp-80", *rule1.Name)
	assert.Equal(t, armnetwork.TransportProtocolTCP, *rule1.Properties.Protocol)
	assert.Equal(t, int32(80), *rule1.Properties.FrontendPort)
	assert.Equal(t, int32(8080), *rule1.Properties.BackendPort)
	assert.False(t, *rule1.Properties.EnableFloatingIP)

	// Rule 2: port 443 -> 8443
	rule2 := lb.Properties.LoadBalancingRules[1]
	assert.Equal(t, "rule-tcp-443", *rule2.Name)
	assert.Equal(t, int32(443), *rule2.Properties.FrontendPort)
	assert.Equal(t, int32(8443), *rule2.Properties.BackendPort)

	// Verify ServicesDTO
	assert.Len(t, servicesDTO.Services, 1)
	assert.Contains(t, servicesDTO.Services[0].LoadBalancerBackendPools[0].ID, "/subscriptions/network-sub/")
	assert.Contains(t, servicesDTO.Services[0].Service, "service-uid-123")
	assert.Equal(t, Inbound, servicesDTO.Services[0].ServiceType)
}

const testPrefixID = "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/publicIPPrefixes/prefix"

func TestPublicIPPrefixID_FollowsTheServiceFamily(t *testing.T) {
	svc := func(annotations map[string]string, families ...v1.IPFamily) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
			Spec:       v1.ServiceSpec{IPFamilies: families},
		}
	}
	both := map[string]string{
		consts.ServiceAnnotationPIPPrefixIDDualStack[false]: "v4-prefix",
		consts.ServiceAnnotationPIPPrefixIDDualStack[true]:  "v6-prefix",
	}
	v4Only := map[string]string{consts.ServiceAnnotationPIPPrefixIDDualStack[false]: "v4-prefix"}
	v6Only := map[string]string{consts.ServiceAnnotationPIPPrefixIDDualStack[true]: "v6-prefix"}

	assert.Equal(t, "v4-prefix", publicIPPrefixID(svc(both, v1.IPv4Protocol), v1.IPv4Protocol))
	assert.Equal(t, "v6-prefix", publicIPPrefixID(svc(both, v1.IPv6Protocol), v1.IPv6Protocol))
	assert.Equal(t, "v4-prefix", publicIPPrefixID(svc(v4Only, v1.IPv6Protocol), v1.IPv6Protocol), "an IPv6 Service falls back to the plain annotation")
	assert.Empty(t, publicIPPrefixID(svc(v6Only, v1.IPv4Protocol), v1.IPv4Protocol))

	dualStack := svc(both, v1.IPv4Protocol, v1.IPv6Protocol)
	assert.Equal(t, "v4-prefix", publicIPPrefixID(dualStack, v1.IPv4Protocol))
	assert.Equal(t, "v6-prefix", publicIPPrefixID(dualStack, v1.IPv6Protocol))
	assert.Empty(t, publicIPPrefixID(svc(v4Only, v1.IPv6Protocol, v1.IPv4Protocol), v1.IPv6Protocol),
		"each family of a dual-stack Service reads only its own annotation")
}

func TestBuildInboundServiceResources_PublicIPSettings(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
		ServiceName:   "ns/svc",
		ClusterName:   "cluster",
		PIPTags:       map[string]string{"team": "a"},
		IPTags:        map[string]string{"RoutingPreference": "Internet"},
		DNSLabel:      ptr.To("app"),
	}

	pip, _, _, err := buildInboundServiceResources("svc-uid", config, testConfig())
	assert.NoError(t, err)
	assert.Equal(t, map[string]*string{
		"team":                ptr.To("a"),
		consts.ServiceTagKey:  ptr.To("ns/svc"),
		consts.ClusterNameKey: ptr.To("cluster"),
	}, pip.Tags)
	assert.Equal(t, []*armnetwork.IPTag{{IPTagType: ptr.To("RoutingPreference"), Tag: ptr.To("Internet")}}, pip.Properties.IPTags)
	assert.Equal(t, "app", *pip.Properties.DNSSettings.DomainNameLabel)
	assert.Nil(t, pip.Properties.PublicIPPrefix)

	config.IPTags, config.DNSLabel, config.ClusterName, config.PIPPrefixID = nil, ptr.To(""), "", testPrefixID
	pip, _, _, err = buildInboundServiceResources("svc-uid", config, testConfig())
	assert.NoError(t, err)
	assert.Nil(t, pip.Properties.IPTags)
	assert.Nil(t, pip.Properties.DNSSettings)
	assert.NotContains(t, pip.Tags, consts.ClusterNameKey)
	assert.Equal(t, testPrefixID, *pip.Properties.PublicIPPrefix.ID)
}

func TestExtractInboundConfigFromService_PublicIPSettings(t *testing.T) {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", Annotations: map[string]string{
			consts.ServiceAnnotationAzurePIPTags:      "team = a, K8S-Azure-Service=x, kubernetes-cluster-name=y, broken, =v",
			consts.ServiceAnnotationIPTagsForPublicIP: "RoutingPreference=Internet",
			consts.ServiceAnnotationDNSLabelName:      " app ",
		}},
		Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP}}},
	}

	config := ExtractInboundConfigFromService(svc)
	assert.Equal(t, "ns/svc", config.ServiceName)
	assert.Equal(t, map[string]string{"team": "a"}, config.PIPTags)
	assert.Equal(t, map[string]string{"RoutingPreference": "Internet"}, config.IPTags)
	assert.Equal(t, ptr.To("app"), config.DNSLabel)
	assert.Equal(t, []string{"K8S-Azure-Service", "kubernetes-cluster-name"}, ReservedPIPTagKeysInAnnotation(svc))

	svc.Annotations = map[string]string{consts.ServiceAnnotationIPTagsForPublicIP: ""}
	config = ExtractInboundConfigFromService(svc)
	assert.NotNil(t, config.IPTags, "a present empty annotation asks to clear IP tags")
	assert.Empty(t, config.IPTags)
	assert.Nil(t, config.DNSLabel)
	assert.Nil(t, config.PIPTags)

	svc.Annotations = nil
	config = ExtractInboundConfigFromService(svc)
	assert.Nil(t, config.IPTags, "an absent annotation leaves IP tags unchanged")
}

func TestBuildInboundServiceResources_NilConfig(t *testing.T) {
	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "test-sgw",
	}

	pip, lb, servicesDTO, err := buildInboundServiceResources("service-uid-123", nil, dtConfig)
	assert.NoError(t, err)

	// Should still create LB, just without rules
	assert.NotNil(t, lb.Name)
	assert.Equal(t, "service-uid-123", *lb.Name)

	// Should have backend pool but no rules
	assert.Len(t, lb.Properties.BackendAddressPools, 1)
	assert.Empty(t, lb.Properties.LoadBalancingRules)

	// PIP should still be created
	assert.NotNil(t, pip.Name)

	// ServicesDTO should still be valid
	assert.Len(t, servicesDTO.Services, 1)
	assert.Equal(t, Inbound, servicesDTO.Services[0].ServiceType)
}

func TestBuildInboundServiceResources_UDPProtocol(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{
			{Port: 53, Protocol: "UDP"},
		},
		BackendPorts: []PortMapping{
			{Port: 5353, Protocol: "UDP"},
		},
	}

	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "westus",
		ServiceGatewayResourceName: "test-sgw",
	}

	_, lb, _, err := buildInboundServiceResources("service-uid-udp", config, dtConfig)
	assert.NoError(t, err)

	// Verify UDP rule
	assert.Len(t, lb.Properties.LoadBalancingRules, 1)
	rule := lb.Properties.LoadBalancingRules[0]
	assert.Equal(t, "rule-udp-53", *rule.Name)
	assert.Equal(t, armnetwork.TransportProtocolUDP, *rule.Properties.Protocol)
	assert.Equal(t, int32(53), *rule.Properties.FrontendPort)
	assert.Nil(t, rule.Properties.EnableTCPReset)
	assert.Equal(t, int32(5353), *rule.Properties.BackendPort)
}

func TestBuildOutboundServiceResources_Basic(t *testing.T) {
	dtConfig := Config{
		SubscriptionID:                "test-sub",
		NetworkResourceSubscriptionID: "network-sub",
		ResourceGroup:                 "test-rg",
		Location:                      "centralus",
		ServiceGatewayResourceName:    "test-sgw",
	}

	pips, natGw, servicesDTO := buildOutboundServiceResources("egress-uid-456", nil, dtConfig)
	pip := pips[0]

	// Verify PIP
	assert.NotNil(t, pip.Name)
	assert.Equal(t, "egress-uid-456-pip", *pip.Name)
	assert.Contains(t, *pip.ID, "/subscriptions/network-sub/")
	assert.Equal(t, armnetwork.PublicIPAddressSKUNameStandardV2, *pip.SKU.Name)
	assert.Equal(t, "centralus", *pip.Location)

	// Verify NAT Gateway
	assert.NotNil(t, natGw.Name)
	assert.Equal(t, "egress-uid-456", *natGw.Name)
	assert.Contains(t, *natGw.ID, "/subscriptions/network-sub/")
	assert.Equal(t, armnetwork.NatGatewaySKUNameStandardV2, *natGw.SKU.Name)
	assert.Equal(t, "centralus", *natGw.Location)
	assert.Equal(t, egressIdentityTags("egress-uid-456"), pip.Tags)
	assert.Equal(t, egressIdentityTags("egress-uid-456"), natGw.Tags)

	// Verify NAT Gateway has ServiceGateway reference
	assert.NotNil(t, natGw.Properties.ServiceGateway)
	assert.Equal(t, dtConfig.ServiceGatewayResourceID(), *natGw.Properties.ServiceGateway.ID)

	// Verify NAT Gateway has PIP reference
	assert.Len(t, natGw.Properties.PublicIPAddresses, 1)
	assert.Contains(t, *natGw.Properties.PublicIPAddresses[0].ID, "egress-uid-456-pip")

	// Verify ServicesDTO
	assert.Len(t, servicesDTO.Services, 1)
	assert.Contains(t, servicesDTO.Services[0].PublicNatGateway.ID, "/subscriptions/network-sub/")
	assert.Contains(t, servicesDTO.Services[0].Service, "egress-uid-456")
	assert.Equal(t, Outbound, servicesDTO.Services[0].ServiceType)
}

func TestBuildInboundServiceResources_BackendPoolNaming(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}

	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "test-sgw",
	}

	_, lb, _, err := buildInboundServiceResources("my-service-uid", config, dtConfig)
	assert.NoError(t, err)

	// Backend pool name must match serviceUID for SLB mode
	assert.Len(t, lb.Properties.BackendAddressPools, 1)
	backendPool := lb.Properties.BackendAddressPools[0]
	assert.Equal(t, "my-service-uid", *backendPool.Name)

	// LB rule should reference the correct backend pool
	rule := lb.Properties.LoadBalancingRules[0]
	assert.Contains(t, *rule.Properties.BackendAddressPool.ID, "my-service-uid")
}

func TestBuildInboundServiceResources_NoProbesForPodIPBackend(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}

	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "test-sgw",
	}

	_, lb, _, err := buildInboundServiceResources("service-uid", config, dtConfig)
	assert.NoError(t, err)

	// For PodIP backend pools, no health probes should be created
	assert.Empty(t, lb.Properties.Probes)

	// LB rules should have no probe reference
	rule := lb.Properties.LoadBalancingRules[0]
	assert.Nil(t, rule.Properties.Probe)
}

func TestBuildInboundServiceResources_ResourceIDs(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}

	dtConfig := Config{
		SubscriptionID:             "sub-123",
		ResourceGroup:              "rg-456",
		Location:                   "eastus",
		ServiceGatewayResourceName: "sgw-789",
	}

	pip, lb, _, err := buildInboundServiceResources("svc-abc", config, dtConfig)
	assert.NoError(t, err)

	// Verify PIP ID format
	expectedPIPID := "/subscriptions/sub-123/resourceGroups/rg-456/providers/Microsoft.Network/publicIPAddresses/svc-abc-pip"
	assert.Equal(t, expectedPIPID, *pip.ID)

	// Verify LB references PIP correctly
	frontendConfig := lb.Properties.FrontendIPConfigurations[0]
	assert.Equal(t, expectedPIPID, *frontendConfig.Properties.PublicIPAddress.ID)

	// Verify backend pool ID reference in rule
	rule := lb.Properties.LoadBalancingRules[0]
	expectedBackendPoolID := "/subscriptions/sub-123/resourceGroups/rg-456/providers/Microsoft.Network/loadBalancers/svc-abc/backendAddressPools/svc-abc"
	assert.Equal(t, expectedBackendPoolID, *rule.Properties.BackendAddressPool.ID)
}

func TestBuildInboundServiceResources_LowercaseUDP(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 53, Protocol: "udp"}},
		BackendPorts:  []PortMapping{{Port: 5353, Protocol: "udp"}},
	}
	dtConfig := Config{SubscriptionID: "sub", ResourceGroup: "rg", Location: "westus"}

	_, lb, _, err := buildInboundServiceResources("svc", config, dtConfig)
	assert.NoError(t, err)
	assert.Len(t, lb.Properties.LoadBalancingRules, 1)
	assert.Equal(t, armnetwork.TransportProtocolUDP, *lb.Properties.LoadBalancingRules[0].Properties.Protocol)
}

func TestBuildInboundServiceResources_UnsupportedProtocolErrors(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 53, Protocol: "SCTP"}},
	}
	dtConfig := Config{SubscriptionID: "sub", ResourceGroup: "rg", Location: "westus"}

	_, _, _, err := buildInboundServiceResources("svc", config, dtConfig)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported protocol")
}

func TestBuildInboundServiceResources_PortOutOfRangeErrors(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 65535, Protocol: "TCP"}},
	}
	dtConfig := Config{SubscriptionID: "sub", ResourceGroup: "rg", Location: "westus"}

	_, _, _, err := buildInboundServiceResources("svc", config, dtConfig)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "out of range")
}

func TestBuildInboundServiceResources_TCPHasResetEnabled(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}
	dtConfig := Config{SubscriptionID: "sub", ResourceGroup: "rg", Location: "westus"}

	_, lb, _, err := buildInboundServiceResources("svc", config, dtConfig)
	assert.NoError(t, err)
	assert.Len(t, lb.Properties.LoadBalancingRules, 1)
	if assert.NotNil(t, lb.Properties.LoadBalancingRules[0].Properties.EnableTCPReset) {
		assert.True(t, *lb.Properties.LoadBalancingRules[0].Properties.EnableTCPReset)
	}
}

func TestBuildInboundServiceResources_BackendPortMaxIsValid(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 65535, Protocol: "TCP"}},
	}
	dtConfig := Config{SubscriptionID: "sub", ResourceGroup: "rg", Location: "westus"}

	_, lb, _, err := buildInboundServiceResources("svc", config, dtConfig)
	assert.NoError(t, err)
	if assert.Len(t, lb.Properties.LoadBalancingRules, 1) {
		assert.Equal(t, int32(65535), *lb.Properties.LoadBalancingRules[0].Properties.BackendPort)
	}
}

func TestBuildInboundServiceResources_BackendPortOutOfRangeErrors(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 65536, Protocol: "TCP"}},
	}
	dtConfig := Config{SubscriptionID: "sub", ResourceGroup: "rg", Location: "westus"}

	_, _, _, err := buildInboundServiceResources("svc", config, dtConfig)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "backend port")
}

func TestBuildInboundServiceResources_AppliesIdleTimeout(t *testing.T) {
	idle := int32(30)
	config := &InboundConfig{
		FrontendPorts:      []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:       []PortMapping{{Port: 8080, Protocol: "TCP"}},
		IdleTimeoutMinutes: &idle,
	}
	dtConfig := Config{SubscriptionID: "sub", ResourceGroup: "rg", Location: "westus"}

	_, lb, _, err := buildInboundServiceResources("svc", config, dtConfig)
	assert.NoError(t, err)
	assert.Len(t, lb.Properties.LoadBalancingRules, 1)
	assert.Equal(t, int32(30), *lb.Properties.LoadBalancingRules[0].Properties.IdleTimeoutInMinutes)
}

func TestBuildInboundServiceResources_IdleTimeoutOutOfRangeErrors(t *testing.T) {
	idle := int32(99)
	config := &InboundConfig{
		FrontendPorts:      []PortMapping{{Port: 80, Protocol: "TCP"}},
		IdleTimeoutMinutes: &idle,
	}
	dtConfig := Config{SubscriptionID: "sub", ResourceGroup: "rg", Location: "westus"}

	_, _, _, err := buildInboundServiceResources("svc", config, dtConfig)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "idle timeout")
}

func TestBuildInboundResourceNames(t *testing.T) {
	lbName, pipName, backendPoolName := buildInboundResourceNames("uid")
	assert.Equal(t, "uid", lbName)
	assert.Equal(t, "uid-pip", pipName)
	assert.Equal(t, "uid", backendPoolName)
}

func TestBuildOutboundResourceNames(t *testing.T) {
	natGatewayName, pipName := buildOutboundResourceNames("uid")
	assert.Equal(t, "uid", natGatewayName)
	assert.Equal(t, "uid-pip", pipName)
}

func TestBuildServiceGatewayRemovalDTO(t *testing.T) {
	dtConfig := Config{SubscriptionID: "sub", ResourceGroup: "rg"}

	t.Run("inbound removal", func(t *testing.T) {
		dto := buildServiceGatewayRemovalDTO("uid", true, dtConfig)
		assert.Equal(t, PartialUpdate, dto.Action)
		if assert.Len(t, dto.Services, 1) {
			assert.Equal(t, "uid", dto.Services[0].Service)
			assert.Equal(t, Inbound, dto.Services[0].ServiceType)
			assert.True(t, dto.Services[0].IsDelete)
		}
	})

	t.Run("outbound removal", func(t *testing.T) {
		dto := buildServiceGatewayRemovalDTO("uid", false, dtConfig)
		assert.Equal(t, PartialUpdate, dto.Action)
		if assert.Len(t, dto.Services, 1) {
			assert.Equal(t, "uid", dto.Services[0].Service)
			assert.Equal(t, Outbound, dto.Services[0].ServiceType)
			assert.True(t, dto.Services[0].IsDelete)
		}
	})
}

// newIgnoreCaseSetFromSlice is used by the service updater and resource builders, so its
// coverage stays here.
func TestNewIgnoreCaseSetFromSlice_Empty(t *testing.T) {
	set := newIgnoreCaseSetFromSlice([]string{})
	assert.NotNil(t, set)
	assert.Equal(t, 0, set.Len())
}

func TestNewIgnoreCaseSetFromSlice_WithItems(t *testing.T) {
	items := []string{"service1", "service2", "SERVICE3"}
	set := newIgnoreCaseSetFromSlice(items)

	assert.Equal(t, 3, set.Len())
	assert.True(t, set.Has("service1"))
	assert.True(t, set.Has("service2"))
	assert.True(t, set.Has("service3")) // Case insensitive
	assert.True(t, set.Has("SERVICE3"))
}

// TestBuildInboundServiceResources_IPFamilies verifies that the Public IP version follows the IP
// family of the unit: a dual-stack Service builds one Public IP per family.
func TestBuildInboundServiceResources_IPFamilies(t *testing.T) {
	service := func(fams ...v1.IPFamily) *v1.Service {
		return &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: "11111111-1111-1111-1111-111111111111"}, Spec: v1.ServiceSpec{
			IPFamilies: fams,
			Ports:      []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt(80)}},
		}}
	}
	build := func(fams ...v1.IPFamily) (armnetwork.PublicIPAddress, error) {
		pip, _, _, err := buildInboundServiceResources("svc", ExtractInboundConfigFromService(service(fams...)), testConfig())
		return pip, err
	}

	pip4, err := build(v1.IPv4Protocol)
	assert.NoError(t, err)
	assert.Equal(t, armnetwork.IPVersionIPv4, *pip4.Properties.PublicIPAddressVersion)

	pip6, err := build(v1.IPv6Protocol)
	assert.NoError(t, err)
	assert.Equal(t, armnetwork.IPVersionIPv6, *pip6.Properties.PublicIPAddressVersion)

	dualStack := service(v1.IPv6Protocol, v1.IPv4Protocol)
	units := InboundUnits(dualStack)
	if !assert.Len(t, units, 2) {
		return
	}
	for _, unit := range units {
		pip, lb, services, err := buildInboundServiceResources(unit.Name, extractInboundUnitConfig(dualStack, unit), testConfig())
		if !assert.NoError(t, err) {
			continue
		}
		assert.Equal(t, armnetwork.IPVersion(unit.Family), *pip.Properties.PublicIPAddressVersion, unit.Name)
		assert.Equal(t, unit.Name+"-pip", *pip.Name)
		assert.Equal(t, unit.Name, *lb.Name)
		assert.Equal(t, unit.Name, services.Services[0].Service)
	}
	assert.Equal(t, "11111111-1111-1111-1111-111111111111-v4", units[1].Name)
}

// TestExtractInboundConfigFromService_NamedTargetPortRecorded verifies that a named (string)
// targetPort is recorded so it can be rejected rather than silently mapped to the Service port.
func TestExtractInboundConfigFromService_NamedTargetPortRecorded(t *testing.T) {
	cfg := ExtractInboundConfigFromService(&v1.Service{Spec: v1.ServiceSpec{
		Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromString("http")}},
	}})
	assert.Equal(t, []string{"http"}, cfg.NamedTargetPorts)
}

// TestBuildInboundServiceResources_NamedTargetPortRejected verifies that a named targetPort,
// which cannot be resolved to a PodIP backend port, is rejected at build time.
func TestBuildInboundServiceResources_NamedTargetPortRejected(t *testing.T) {
	cfg := ExtractInboundConfigFromService(&v1.Service{Spec: v1.ServiceSpec{
		Ports: []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromString("http")}},
	}})
	_, _, _, err := buildInboundServiceResources("svc", cfg, testConfig())
	assert.Error(t, err, "a named targetPort must be rejected for PodIP backend pools")
}

// Two service ports that resolve to the same protocol + backend port collide on the shared
// PodIP backend pool (floating IP is always disabled), which Azure rejects with
// RulesUseSameBackendPortProtocolAndPool. The build must fail terminally rather than emit an
// LB the Azure PUT can never accept.
func TestBuildInboundServiceResources_DuplicateBackendPortRejected(t *testing.T) {
	cfg := ExtractInboundConfigFromService(&v1.Service{Spec: v1.ServiceSpec{
		Ports: []v1.ServicePort{
			{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt(8080)},
			{Port: 443, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt(8080)},
		},
	}})
	_, _, _, err := buildInboundServiceResources("svc", cfg, testConfig())
	assert.Error(t, err, "two ports sharing a backend port and protocol must be rejected")
	assert.Contains(t, err.Error(), "RulesUseSameBackendPortProtocolAndPool")
}

// Distinct backend ports (or differing protocol) are a valid multi-port LB and must build two
// rules without error. This is the shape the add/remove e2e exercises.
func TestBuildInboundServiceResources_DistinctBackendPortsAllowed(t *testing.T) {
	cfg := ExtractInboundConfigFromService(&v1.Service{Spec: v1.ServiceSpec{
		Ports: []v1.ServicePort{
			{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt(8080)},
			{Port: 443, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt(8443)},
		},
	}})
	_, lb, _, err := buildInboundServiceResources("svc", cfg, testConfig())
	assert.NoError(t, err, "distinct backend ports must build a valid multi-rule LB")
	assert.Len(t, lb.Properties.LoadBalancingRules, 2)
}

// Same backend port but different protocol (TCP vs UDP) does not collide: Azure scopes the
// constraint per protocol, so this must build two rules.
func TestBuildInboundServiceResources_SameBackendPortDifferentProtocolAllowed(t *testing.T) {
	cfg := &InboundConfig{
		FrontendPorts: []PortMapping{
			{Port: 80, Protocol: "TCP"},
			{Port: 80, Protocol: "UDP"},
		},
		BackendPorts: []PortMapping{
			{Port: 8080, Protocol: "TCP"},
			{Port: 8080, Protocol: "UDP"},
		},
	}
	_, lb, _, err := buildInboundServiceResources("svc", cfg, testConfig())
	assert.NoError(t, err, "same backend port over different protocols must be allowed")
	assert.Len(t, lb.Properties.LoadBalancingRules, 2)
}

func TestIsValidEgressIdentity(t *testing.T) {
	valid := []string{
		"egress-gateway-a",
		"tenant-a-egress",
		"my_egress.gw-1",
		"a",
		"e0",
		strings.Repeat("a", 73), // max length (reserves 7 chars for the "-pip-v6" suffix)
	}
	for _, n := range valid {
		assert.True(t, IsValidEgressIdentity(n), "expected %q to be a valid egress identity", n)
	}

	invalid := []string{
		"",                      // empty
		"../hijacked-nat",       // path traversal
		"egress/gateway",        // slash
		"-leading-hyphen",       // must start with an alphanumeric
		".leading-dot",          // must start with an alphanumeric
		"trailing-dot.",         // must end with an alphanumeric or underscore
		"trailing-hyphen-",      // must end with an alphanumeric or underscore
		"has space",             // whitespace
		"UPPER",                 // callers lowercase first; raw uppercase is rejected
		strings.Repeat("a", 74), // exceeds 73: the IPv6 PIP name would overflow Azure's 80-char limit
		strings.Repeat("a", 81), // too long
	}
	for _, n := range invalid {
		assert.False(t, IsValidEgressIdentity(n), "expected %q to be an invalid egress identity", n)
	}

	// A max-length egress identity must yield BOTH Public IP names within Azure's 80-char limit.
	for _, pipName := range OutboundPublicIPNames(strings.Repeat("a", 73)) {
		assert.LessOrEqual(t, len(pipName), 80,
			"PIP name %q derived from a max-length egress identity must fit Azure's 80-char publicIPAddresses limit", pipName)
	}
}

func TestExtractInboundConfigFromService_ExtractsIdleTimeout(t *testing.T) {
	newService := func(annotations map[string]string) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "test-service", Namespace: "default", Annotations: annotations},
			Spec: v1.ServiceSpec{
				Ports: []v1.ServicePort{
					{Name: "http", Protocol: v1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt(8080)},
				},
			},
		}
	}

	// buildInboundServiceResources programs IdleTimeoutInMinutes from this field, so leaving it
	// unset makes the annotation inert and the Service silently keeps the Azure default.
	t.Run("annotation reaches the config", func(t *testing.T) {
		config := ExtractInboundConfigFromService(newService(
			map[string]string{consts.ServiceAnnotationLoadBalancerIdleTimeout: "30"}))
		if assert.NotNil(t, config) {
			assert.Equal(t, int32(30), ptr.Deref(config.IdleTimeoutMinutes, 0))
			assert.Nil(t, config.InvalidIdleTimeout)
			assert.NoError(t, ValidateInboundConfig(config))
		}
	})

	t.Run("absent annotation leaves the builder default in place", func(t *testing.T) {
		config := ExtractInboundConfigFromService(newService(nil))
		if assert.NotNil(t, config) {
			assert.Nil(t, config.IdleTimeoutMinutes)
			assert.NoError(t, ValidateInboundConfig(config))
		}
	})

	// An unusable value must be rejected with a reason rather than silently falling back to the
	// default, which would report a timeout the Service is not running with.
	for _, value := range []string{"0", "3", "101", "not-a-number"} {
		t.Run("invalid value "+value+" is rejected", func(t *testing.T) {
			config := ExtractInboundConfigFromService(newService(
				map[string]string{consts.ServiceAnnotationLoadBalancerIdleTimeout: value}))
			if !assert.NotNil(t, config) {
				return
			}
			assert.Nil(t, config.IdleTimeoutMinutes)
			err := ValidateInboundConfig(config)
			var validationErr *InboundConfigValidationError
			if assert.ErrorAs(t, err, &validationErr) {
				assert.Equal(t, "InvalidIdleTimeout", validationErr.Reason)
			}
		})
	}
}

// TestAdmitInboundService_RejectsUnimplementedSpecFields pins that Service spec fields the PodIP
// data path does not implement are rejected rather than silently ignored. Accepting them makes the
// Service report a configuration it is not running with: ClientIP affinity that is not honoured,
// or externalTrafficPolicy Local that behaves as Cluster.
func TestAdmitInboundService_RejectsUnimplementedSpecFields(t *testing.T) {
	base := func() *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "abc"},
			Spec: v1.ServiceSpec{
				Type:  v1.ServiceTypeLoadBalancer,
				Ports: []v1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: v1.ProtocolTCP}},
			},
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(*v1.Service)
		reason string
	}{
		{"sessionAffinity ClientIP", func(s *v1.Service) { s.Spec.SessionAffinity = v1.ServiceAffinityClientIP }, "UnsupportedSessionAffinity"},
		{"Private Link Service", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationPLSCreation: "True"}
		}, "UnsupportedPrivateLinkService"},
		{"loadBalancerIP that is not an address", func(s *v1.Service) { s.Spec.LoadBalancerIP = "not-an-ip" }, "InvalidLoadBalancerIP"},
		{"IPv6 loadBalancerIP on an IPv4 Service", func(s *v1.Service) { s.Spec.LoadBalancerIP = "2001:db8::1" }, "InvalidLoadBalancerIP"},
		{"IPv6 loadBalancerIP on a single-stack IPv4 Service", func(s *v1.Service) {
			s.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol}
			s.Spec.LoadBalancerIP = "2001:db8::1"
		}, "InvalidLoadBalancerIP"},
		{"IPv4 loadBalancerIP on a single-stack IPv6 Service", func(s *v1.Service) {
			s.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
			s.Spec.LoadBalancerIP = "203.0.113.7"
		}, "InvalidLoadBalancerIP"},
		{"loadBalancerSourceRanges", func(s *v1.Service) { s.Spec.LoadBalancerSourceRanges = []string{"203.0.113.0/24"} }, "UnsupportedAccessRestriction"},
		{"source ranges mixing allow-all with a restriction", func(s *v1.Service) {
			s.Spec.LoadBalancerSourceRanges = []string{"::/0", "203.0.113.0/24"}
		}, "UnsupportedAccessRestriction"},
		{"IPv6 allow-all on an IPv4 Service", func(s *v1.Service) { s.Spec.LoadBalancerSourceRanges = []string{"::/0"} }, "UnsupportedAccessRestriction"},
		{"unmasked /0", func(s *v1.Service) { s.Spec.LoadBalancerSourceRanges = []string{"10.0.0.0/0"} }, "UnsupportedAccessRestriction"},
		{"source ranges annotation", func(s *v1.Service) {
			s.Annotations = map[string]string{v1.AnnotationLoadBalancerSourceRangesKey: "203.0.113.0/24"}
		}, "UnsupportedAccessRestriction"},
		{"restrictive annotation next to an allow-all spec", func(s *v1.Service) {
			s.Spec.LoadBalancerSourceRanges = []string{"0.0.0.0/0"}
			s.Annotations = map[string]string{v1.AnnotationLoadBalancerSourceRangesKey: "203.0.113.0/24"}
		}, "UnsupportedAccessRestriction"},
		{"empty source ranges annotation", func(s *v1.Service) {
			s.Annotations = map[string]string{v1.AnnotationLoadBalancerSourceRangesKey: ""}
		}, "UnsupportedAccessRestriction"},
		{"unparsable source range", func(s *v1.Service) {
			s.Annotations = map[string]string{v1.AnnotationLoadBalancerSourceRangesKey: "not-a-range"}
		}, "UnsupportedAccessRestriction"},
		{"allowed IP ranges", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationAllowedIPRanges: "10.0.0.0/8"}
		}, "UnsupportedAccessRestriction"},
		{"allowed service tags", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationAllowedServiceTags: "AzureCloud"}
		}, "UnsupportedAccessRestriction"},
		{"port without a load-balancing rule", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.BuildAnnotationKeyForPort(80, consts.PortAnnotationNoLBRule): "true"}
		}, "UnsupportedAccessRestriction"},
		{"IPv4 address annotation that is not an address", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "203.0.113"}
		}, "InvalidLoadBalancerIP"},
		{"IPv4 address on an IPv6 Service", func(s *v1.Service) {
			s.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
			s.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[true]: "203.0.113.10"}
		}, "InvalidLoadBalancerIP"},
		{"PIP name and address", func(s *v1.Service) {
			s.Spec.LoadBalancerIP = "203.0.113.10"
			s.Annotations = map[string]string{consts.ServiceAnnotationPIPNameDualStack[false]: "my-pip"}
		}, "ConflictingPublicIPSettings"},
		{"address and PIP prefix", func(s *v1.Service) {
			s.Annotations = map[string]string{
				consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "203.0.113.10",
				consts.ServiceAnnotationPIPPrefixIDDualStack[false]:    testPrefixID,
			}
		}, "ConflictingPublicIPSettings"},
		{"malformed PIP prefix", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationPIPPrefixIDDualStack[false]: "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/p"}
		}, "InvalidPublicIPPrefix"},
		{"PIP prefix with IP tags", func(s *v1.Service) {
			s.Annotations = map[string]string{
				consts.ServiceAnnotationPIPPrefixIDDualStack[false]: testPrefixID,
				consts.ServiceAnnotationIPTagsForPublicIP:           "RoutingPreference=Internet",
			}
		}, "ConflictingPublicIPSettings"},
		{"PIP prefix with empty IP tags annotation", func(s *v1.Service) {
			s.Annotations = map[string]string{
				consts.ServiceAnnotationPIPPrefixIDDualStack[false]: testPrefixID,
				consts.ServiceAnnotationIPTagsForPublicIP:           "",
			}
		}, "ConflictingPublicIPSettings"},
		{"deny all except the source ranges", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges: "True"}
		}, "UnsupportedAccessRestriction"},
		{"deny all with allow-all source ranges", func(s *v1.Service) {
			s.Spec.LoadBalancerSourceRanges = []string{"0.0.0.0/0", "::/0"}
			s.Annotations = map[string]string{consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges: "true"}
		}, "UnsupportedAccessRestriction"},
		{"Private Link Service creation with a value that does not disable it", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationPLSCreation: "no"}
		}, "UnsupportedAnnotations"},
		{"internal load balancer with a value that has no effect", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerInternal: "yes"}
		}, "UnsupportedAnnotations"},
		{"deny all with an empty value", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges: ""}
		}, "UnsupportedAnnotations"},
		{"Private Link Service settings without creation", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationPLSCreation: "false", consts.ServiceAnnotationPLSName: "pls"}
		}, "UnsupportedAnnotations"},
		{"floating IP disabled", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true"}
		}, "UnsupportedAnnotations"},
		{"floating IP enabled", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "false"}
		}, "UnsupportedAnnotations"},
		{"port without a health probe", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.BuildAnnotationKeyForPort(80, consts.PortAnnotationNoHealthProbeRule): "true"}
		}, "UnsupportedAnnotations"},
		{"no load-balancing rule for a non-service port", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.BuildAnnotationKeyForPort(8080, consts.PortAnnotationNoLBRule): "true"}
		}, "UnsupportedAnnotations"},
		{"no load-balancing rule with a value that does not disable it", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.BuildAnnotationKeyForPort(80, consts.PortAnnotationNoLBRule): "1"}
		}, "UnsupportedAnnotations"},
		{"no load-balancing rule with a padded value", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.BuildAnnotationKeyForPort(80, consts.PortAnnotationNoLBRule): " true"}
		}, "UnsupportedAnnotations"},
		{"no load-balancing rule for a zero-padded port", func(s *v1.Service) {
			s.Annotations = map[string]string{"service.beta.kubernetes.io/port_080_no_lb_rule": "true"}
		}, "UnsupportedAnnotations"},
		{"no load-balancing rule for a named port", func(s *v1.Service) {
			s.Annotations = map[string]string{"service.beta.kubernetes.io/port_http_no_lb_rule": "true"}
		}, "UnsupportedAnnotations"},
		{"no load-balancing rule kept for a non-service port", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.BuildAnnotationKeyForPort(8080, consts.PortAnnotationNoLBRule): "false"}
		}, "UnsupportedAnnotations"},
		{"no load-balancing rule kept for a zero-padded port", func(s *v1.Service) {
			s.Annotations = map[string]string{"service.beta.kubernetes.io/port_080_no_lb_rule": "false"}
		}, "UnsupportedAnnotations"},
		{"no load-balancing rule kept for a named port", func(s *v1.Service) {
			s.Annotations = map[string]string{"service.beta.kubernetes.io/port_http_no_lb_rule": "false"}
		}, "UnsupportedAnnotations"},
		{"additional Public IPs", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationAdditionalPublicIPs: "203.0.113.20"}
		}, "UnsupportedAnnotations"},
		{"TCP reset disabled", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationDisableTCPReset: "true"}
		}, "UnsupportedAnnotations"},
		{"load balancer mode", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerMode: "auto"}
		}, "UnsupportedAnnotations"},
		{"resource group without a chosen Public IP", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerResourceGroup: "rg"}
		}, "UnsupportedAnnotations"},
		{"IPv6 Public IP name on an IPv4 Service", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationPIPNameDualStack[true]: "my-pip-v6"}
		}, "UnsupportedAnnotations"},
		{"health probe request path", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath: "/healthz"}
		}, "UnsupportedHealthProbe"},
		{"per-port health probe protocol", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProtocol): "http"}
		}, "UnsupportedHealthProbe"},
		{"reserved Public IP tag key", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationAzurePIPTags: "team=a,K8S-Azure-Service=spoof"}
		}, "InvalidPIPTags"},
		{"malformed Public IP tag", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationAzurePIPTags: "team:a"}
		}, "InvalidPIPTags"},
		{"malformed Public IP tag with empty key", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationAzurePIPTags: "=value"}
		}, "InvalidPIPTags"},
		{"malformed Public IP IP tag", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationIPTagsForPublicIP: "FirstPartyUsage:/Unprivileged"}
		}, "InvalidIPTags"},
		{"malformed Public IP IP tag with empty key", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationIPTagsForPublicIP: "=/Unprivileged"}
		}, "InvalidIPTags"},
		{"malformed Public IP IP tag with extra separator", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationIPTagsForPublicIP: "FirstPartyUsage=/Unprivileged=extra"}
		}, "InvalidIPTags"},
		{"repeated Public IP tag", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationAzurePIPTags: "team=a,Team=b"}
		}, "InvalidPIPTags"},
		{"repeated Public IP IP tag type", func(s *v1.Service) {
			s.Annotations = map[string]string{consts.ServiceAnnotationIPTagsForPublicIP: "FirstPartyUsage=/a,FirstPartyUsage=/b"}
		}, "InvalidIPTags"},
		{"load balancer IP annotation with an invalid spec.loadBalancerIP", func(s *v1.Service) {
			s.Spec.LoadBalancerIP = "garbage"
			s.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "203.0.113.20"}
		}, "ConflictingPublicIPSettings"},
		{"conflicting load balancer IP annotation and spec", func(s *v1.Service) {
			s.Spec.LoadBalancerIP = "203.0.113.10"
			s.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "203.0.113.20"}
		}, "ConflictingPublicIPSettings"},
		{"conflicting IPv6 load balancer IP annotation and spec", func(s *v1.Service) {
			s.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
			s.Spec.LoadBalancerIP = "2001:db8::10"
			s.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[true]: "2001:db8::20"}
		}, "ConflictingPublicIPSettings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := base()
			tc.mutate(svc)
			config, err := AdmitInboundService(svc)
			assert.Nil(t, config)
			var validationErr *InboundConfigValidationError
			if assert.ErrorAs(t, err, &validationErr) {
				assert.Equal(t, tc.reason, validationErr.Reason)
			}
		})
	}

	t.Run("IPv6-specific Public IP name makes the plain name unsupported", func(t *testing.T) {
		svc := base()
		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
		svc.Annotations = map[string]string{
			consts.ServiceAnnotationPIPNameDualStack[false]: "plain-pip",
			consts.ServiceAnnotationPIPNameDualStack[true]:  "ipv6-pip",
		}
		config, err := AdmitInboundService(svc)
		assert.Nil(t, config)
		var validationErr *InboundConfigValidationError
		if assert.ErrorAs(t, err, &validationErr) {
			assert.Equal(t, "UnsupportedAnnotations", validationErr.Reason)
			assert.Contains(t, validationErr.Message, consts.ServiceAnnotationPIPNameDualStack[false])
		}
	})

	t.Run("IPv6-specific Public IP prefix makes the plain prefix unsupported", func(t *testing.T) {
		svc := base()
		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
		svc.Annotations = map[string]string{
			consts.ServiceAnnotationPIPPrefixIDDualStack[false]: testPrefixID,
			consts.ServiceAnnotationPIPPrefixIDDualStack[true]:  strings.Replace(testPrefixID, "prefix", "prefix-v6", 1),
		}
		config, err := AdmitInboundService(svc)
		assert.Nil(t, config)
		var validationErr *InboundConfigValidationError
		if assert.ErrorAs(t, err, &validationErr) {
			assert.Equal(t, "UnsupportedAnnotations", validationErr.Reason)
			assert.Contains(t, validationErr.Message, consts.ServiceAnnotationPIPPrefixIDDualStack[false])
		}

		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol}
		assert.Equal(t, []string{consts.ServiceAnnotationPIPPrefixIDDualStack[true]}, UnsupportedServiceAnnotations(svc),
			"an IPv4 Service uses the plain prefix; only the IPv6 one has no effect")
	})

	t.Run("IPv6 Services can fall back to the plain Public IP name", func(t *testing.T) {
		svc := base()
		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
		svc.Annotations = map[string]string{consts.ServiceAnnotationPIPNameDualStack[false]: "plain-pip"}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		if assert.NotNil(t, config) {
			assert.Equal(t, "plain-pip", config.PIPName)
		}
	})

	t.Run("IPv6 Services can fall back to the plain Public IP prefix", func(t *testing.T) {
		svc := base()
		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
		svc.Annotations = map[string]string{consts.ServiceAnnotationPIPPrefixIDDualStack[false]: testPrefixID}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		if assert.NotNil(t, config) {
			assert.Equal(t, testPrefixID, config.PIPPrefixID)
		}
	})

	t.Run("defaults are still admitted", func(t *testing.T) {
		svc := base()
		svc.Spec.SessionAffinity = v1.ServiceAffinityNone
		svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeCluster
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		assert.NotNil(t, config)
	})

	t.Run("settings that restrict nothing are admitted", func(t *testing.T) {
		svc := base()
		svc.Spec.LoadBalancerSourceRanges = []string{"0.0.0.0/0", "::/0"}
		svc.Annotations = map[string]string{
			consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges:       "false",
			consts.ServiceAnnotationPLSCreation:                                 "False",
			consts.ServiceAnnotationLoadBalancerInternal:                        "FALSE",
			consts.ServiceAnnotationPIPNameDualStack[false]:                     "",
			consts.BuildAnnotationKeyForPort(80, consts.PortAnnotationNoLBRule): "False",
		}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		assert.NotNil(t, config)
	})

	t.Run("blank IP tags annotation is admitted as remove", func(t *testing.T) {
		svc := base()
		svc.Annotations = map[string]string{consts.ServiceAnnotationIPTagsForPublicIP: "  "}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		if assert.NotNil(t, config) {
			assert.NotNil(t, config.IPTags)
			assert.Empty(t, config.IPTags)
		}
	})

	t.Run("valid IP tags annotation is admitted", func(t *testing.T) {
		svc := base()
		svc.Annotations = map[string]string{consts.ServiceAnnotationIPTagsForPublicIP: "FirstPartyUsage=/Unprivileged"}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		if assert.NotNil(t, config) {
			assert.Equal(t, map[string]string{"FirstPartyUsage": "/Unprivileged"}, config.IPTags)
		}
	})

	t.Run("trailing blank tag pairs are admitted", func(t *testing.T) {
		svc := base()
		svc.Annotations = map[string]string{
			consts.ServiceAnnotationAzurePIPTags:      "team=a,",
			consts.ServiceAnnotationIPTagsForPublicIP: "FirstPartyUsage=/Unprivileged,",
		}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		if assert.NotNil(t, config) {
			assert.Equal(t, map[string]string{"team": "a"}, config.PIPTags)
			assert.Equal(t, map[string]string{"FirstPartyUsage": "/Unprivileged"}, config.IPTags)
		}
	})

	t.Run("equal load balancer IP annotation and spec are admitted", func(t *testing.T) {
		svc := base()
		svc.Spec.LoadBalancerIP = "203.0.113.10"
		svc.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "203.0.113.10"}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		if assert.NotNil(t, config) {
			assert.Equal(t, "203.0.113.10", config.LoadBalancerIP)
		}
	})

	t.Run("equal IPv6 load balancer IP annotation and spec are admitted", func(t *testing.T) {
		svc := base()
		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
		svc.Spec.LoadBalancerIP = "2001:DB8::10"
		svc.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[true]: "2001:db8:0::10"}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		if assert.NotNil(t, config) {
			assert.Equal(t, "2001:db8::10", config.LoadBalancerIP)
		}
	})

	t.Run("allow-all annotations are admitted", func(t *testing.T) {
		svc := base()
		svc.Annotations = map[string]string{
			v1.AnnotationLoadBalancerSourceRangesKey: " 0.0.0.0/0 ",
			consts.ServiceAnnotationAllowedIPRanges:  "0.0.0.0/0, ::/0",
		}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		assert.NotNil(t, config)
	})

	t.Run("the two range annotations are combined as one list", func(t *testing.T) {
		svc := base()
		svc.Annotations = map[string]string{
			v1.AnnotationLoadBalancerSourceRangesKey: "10.0.0.0/8",
			consts.ServiceAnnotationAllowedIPRanges:  "0.0.0.0/0",
		}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		assert.NotNil(t, config)
	})

	t.Run("a PIP prefix is admitted", func(t *testing.T) {
		svc := base()
		svc.Annotations = map[string]string{consts.ServiceAnnotationPIPPrefixIDDualStack[false]: testPrefixID}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		if assert.NotNil(t, config) {
			assert.Equal(t, testPrefixID, config.PIPPrefixID)
		}
	})

	t.Run("Public IP selection is read for the Service's family", func(t *testing.T) {
		for _, tc := range []struct {
			name                     string
			ipv6                     bool
			loadBalancerIP           string
			annotations              map[string]string
			wantName, wantIP, wantRG string
		}{
			{name: "name with resource group", annotations: map[string]string{
				consts.ServiceAnnotationPIPNameDualStack[false]:   " my-pip ",
				consts.ServiceAnnotationLoadBalancerResourceGroup: "other-rg",
			}, wantName: "my-pip", wantRG: "other-rg"},
			{name: "IPv6 name annotation", ipv6: true, annotations: map[string]string{
				consts.ServiceAnnotationPIPNameDualStack[true]: "my-pip-v6",
			}, wantName: "my-pip-v6"},
			{name: "IPv6 falls back to the plain name", ipv6: true, annotations: map[string]string{
				consts.ServiceAnnotationPIPNameDualStack[false]: "my-pip",
			}, wantName: "my-pip"},
			{name: "IPv4 address annotation", annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "203.0.113.10",
			}, wantIP: "203.0.113.10"},
			{name: "IPv6 address annotation", ipv6: true, annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerIPDualStack[true]: "2001:db8::1",
			}, wantIP: "2001:db8::1"},
			{name: "spec.loadBalancerIP", loadBalancerIP: "203.0.113.10", wantIP: "203.0.113.10"},
			{name: "address with resource group", annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "203.0.113.10",
				consts.ServiceAnnotationLoadBalancerResourceGroup:      "other-rg",
			}, wantIP: "203.0.113.10", wantRG: "other-rg"},
			{name: "the address annotation can match spec.loadBalancerIP", loadBalancerIP: "203.0.113.10", annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "203.0.113.10",
			}, wantIP: "203.0.113.10"},
			{name: "IPv6 address in another form", ipv6: true, annotations: map[string]string{
				consts.ServiceAnnotationLoadBalancerIPDualStack[true]: " 2001:DB8:0:0::10 ",
			}, wantIP: "2001:db8::10"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				svc := base()
				if tc.ipv6 {
					svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
				}
				svc.Spec.LoadBalancerIP = tc.loadBalancerIP
				svc.Annotations = tc.annotations
				config, err := AdmitInboundService(svc)
				assert.NoError(t, err)
				if assert.NotNil(t, config) {
					assert.Equal(t, tc.wantName, config.PIPName)
					assert.Equal(t, tc.wantIP, config.LoadBalancerIP)
					assert.Equal(t, tc.wantRG, config.PIPResourceGroup)
				}
			})
		}
	})

	t.Run("a PIP prefix without IP tags is admitted", func(t *testing.T) {
		svc := base()
		svc.Annotations = map[string]string{consts.ServiceAnnotationPIPPrefixIDDualStack[false]: testPrefixID}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		if assert.NotNil(t, config) {
			assert.Nil(t, config.IPTags)
			assert.Equal(t, testPrefixID, config.PIPPrefixID)
		}
	})

	t.Run("a PIP prefix with an empty IP tags annotation is rejected", func(t *testing.T) {
		svc := base()
		svc.Annotations = map[string]string{
			consts.ServiceAnnotationPIPPrefixIDDualStack[false]: testPrefixID,
			consts.ServiceAnnotationIPTagsForPublicIP:           "",
		}
		_, err := AdmitInboundService(svc)
		var validation *InboundConfigValidationError
		if assert.ErrorAs(t, err, &validation) {
			assert.Equal(t, "ConflictingPublicIPSettings", validation.Reason)
		}
	})

	t.Run("IPv6 allow-all on an IPv6 Service is admitted", func(t *testing.T) {
		svc := base()
		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
		svc.Spec.LoadBalancerSourceRanges = []string{"::/0"}
		config, err := AdmitInboundService(svc)
		assert.NoError(t, err)
		assert.NotNil(t, config)
	})
}

func TestUnsupportedServiceAnnotations(t *testing.T) {
	svc := &v1.Service{Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 80}}}, ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		consts.ServiceAnnotationLoadBalancerResourceGroup:                                 "rg",
		consts.ServiceAnnotationDNSLabelName:                                              "app",
		consts.ServiceAnnotationAzurePIPTags:                                              "a=b",
		consts.ServiceAnnotationIPTagsForPublicIP:                                         "RoutingPreference=Internet",
		consts.ServiceAnnotationLoadBalancerInternal:                                      "false",
		consts.ServiceAnnotationLoadBalancerIdleTimeout:                                   "10",
		consts.ServiceAnnotationAllowedIPRanges:                                           "0.0.0.0/0",
		consts.ServiceAnnotationPIPNameDualStack[false]:                                   "",
		v1.AnnotationLoadBalancerSourceRangesKey:                                          "0.0.0.0/0",
		consts.ServiceAnnotationDenyAllExceptLoadBalancerSourceRanges:                     "false",
		consts.BuildAnnotationKeyForPort(80, consts.PortAnnotationNoLBRule):               "false",
		consts.BuildAnnotationKeyForPort(80, consts.PortAnnotationNoHealthProbeRule):      "true",
		consts.ServiceAnnotationDisableLoadBalancerFloatingIP:                             "true",
		consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath:                        "/healthz",
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProtocol): "http",
		"example.com/unrelated":                                                           "x",
	}}}

	assert.Equal(t, []string{
		consts.ServiceAnnotationDisableLoadBalancerFloatingIP,
		consts.ServiceAnnotationLoadBalancerResourceGroup,
		consts.BuildAnnotationKeyForPort(80, consts.PortAnnotationNoHealthProbeRule),
	}, UnsupportedServiceAnnotations(svc), "health-probe annotations and source restrictions are reported separately")
	assert.Equal(t, []string{
		consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath,
		consts.BuildHealthProbeAnnotationKeyForPort(80, consts.HealthProbeParamsProtocol),
	}, HealthProbeServiceAnnotations(svc))
	assert.Empty(t, HealthProbeServiceAnnotations(&v1.Service{}))
	assert.Empty(t, HealthProbeServiceAnnotations(nil))
	assert.Empty(t, UnsupportedServiceAnnotations(&v1.Service{}))
	assert.Empty(t, UnsupportedServiceAnnotations(nil))

	for _, key := range []string{
		consts.ServiceAnnotationLoadBalancerHealthProbeProtocol,
		consts.ServiceAnnotationLoadBalancerHealthProbeInterval,
		consts.ServiceAnnotationLoadBalancerHealthProbeNumOfProbe,
		consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath,
		consts.BuildHealthProbeAnnotationKeyForPort(443, consts.HealthProbeParamsPort),
		consts.BuildHealthProbeAnnotationKeyForPort(443, consts.HealthProbeParamsProbeInterval),
		consts.BuildHealthProbeAnnotationKeyForPort(443, consts.HealthProbeParamsNumOfProbe),
		consts.BuildHealthProbeAnnotationKeyForPort(443, consts.HealthProbeParamsRequestPath),
	} {
		assert.True(t, healthProbeAnnotation(key), key)
	}
	for _, key := range []string{
		consts.BuildAnnotationKeyForPort(443, consts.PortAnnotationNoHealthProbeRule),
		consts.ServiceAnnotationLoadBalancerIdleTimeout,
		"example.com/health-probe_protocol",
	} {
		assert.False(t, healthProbeAnnotation(key), key)
	}

	t.Run("Public IP annotations that do not apply to the Service", func(t *testing.T) {
		svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			consts.ServiceAnnotationPIPNameDualStack[false]:        "my-pip",
			consts.ServiceAnnotationPIPNameDualStack[true]:         "my-pip-v6",
			consts.ServiceAnnotationLoadBalancerIPDualStack[true]:  "2001:db8::1",
			consts.ServiceAnnotationPIPPrefixIDDualStack[true]:     testPrefixID,
			consts.ServiceAnnotationLoadBalancerResourceGroup:      "other-rg",
			consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "",
		}}}
		assert.Equal(t, []string{
			consts.ServiceAnnotationLoadBalancerIPDualStack[true],
			consts.ServiceAnnotationPIPNameDualStack[true],
			consts.ServiceAnnotationPIPPrefixIDDualStack[true],
		}, UnsupportedServiceAnnotations(svc), "IPv6-only annotations do not apply to an IPv4 Service")

		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv6Protocol}
		assert.Equal(t, []string{consts.ServiceAnnotationLoadBalancerIPDualStack[false], consts.ServiceAnnotationPIPNameDualStack[false]}, UnsupportedServiceAnnotations(svc),
			"the IPv4 address does not apply to an IPv6 Service")

		rgOnly := &v1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerResourceGroup: "other-rg"}}}
		assert.Equal(t, []string{consts.ServiceAnnotationLoadBalancerResourceGroup}, UnsupportedServiceAnnotations(rgOnly),
			"a resource group without a chosen Public IP has nothing to look up")
		rgOnly.Spec.LoadBalancerIP = "203.0.113.10"
		assert.Empty(t, UnsupportedServiceAnnotations(rgOnly), "the resource group applies to an address chosen in the spec")
	})
}

// TestAdmitInboundService_RejectsInternalLoadBalancerCaseInsensitively pins the shared admission
// gate used by both the runtime path (ReconcileInboundService) and the startup path
// (reconcileServices).
//
// An exact "true" comparison treats "True"/"TRUE" as absent and lets the request through, and the
// builder then hardcodes Scope="Public" - so the user asks for an internal load balancer and
// receives a public, internet-facing one. Every other Azure annotation in this provider is matched
// case-insensitively.
func TestAdmitInboundService_RejectsInternalLoadBalancerCaseInsensitively(t *testing.T) {
	for _, value := range []string{"true", "True", "TRUE", "TrUe"} {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "svc",
				Namespace:   "ns",
				UID:         "11111111-1111-1111-1111-111111111111",
				Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerInternal: value},
			},
			Spec: v1.ServiceSpec{
				Type:  v1.ServiceTypeLoadBalancer,
				Ports: []v1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: v1.ProtocolTCP}},
			},
		}

		config, err := AdmitInboundService(svc)
		assert.Nil(t, config, "an internal-LB request must not yield a provisionable config (value %q)", value)
		assert.Error(t, err, "internal load balancer annotation %q must be rejected", value)

		var validationErr *InboundConfigValidationError
		if assert.ErrorAs(t, err, &validationErr) {
			assert.Equal(t, "UnsupportedInternalLoadBalancer", validationErr.Reason)
		}
	}
}

// TestAdmitInboundService_RejectsNilService pins that a nil Service is an error, not a skip. A
// (nil, nil) return is how admission reports "nothing to provision", so callers would silently
// drop the Service instead of surfacing the programming error.
func TestAdmitInboundService_RejectsNilService(t *testing.T) {
	config, err := AdmitInboundService(nil)

	assert.EqualError(t, err, "cannot admit a nil Service")
	assert.Nil(t, config)
}

// TestAdmitInboundService_AdmitsSupportedService is the control: a plain LoadBalancer Service must
// still be admitted, so the guard above cannot pass by rejecting everything.
func TestAdmitInboundService_AdmitsSupportedService(t *testing.T) {
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "22222222-2222-2222-2222-222222222222"},
		Spec: v1.ServiceSpec{
			Type:  v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: v1.ProtocolTCP}},
		},
	}

	config, err := AdmitInboundService(svc)
	assert.NoError(t, err)
	assert.NotNil(t, config, "a supported LoadBalancer Service must still be admitted")
}

// TestAdmitInboundService_AdmitsBothExternalTrafficPolicies pins that externalTrafficPolicy is not
// an admission input. Local only differs from Cluster for node-IP backend pools, where it avoids a
// second hop to a node without a local pod; the PodIP backend pool registers Ready pod IPs
// directly, so the load balancer already reaches the pod without that hop under either policy.
// Rejecting Local would also strand Services that are already running: AdmitInboundService gates
// the startup path too, so a CCM restart would tear their load balancers down.
func TestAdmitInboundService_AdmitsBothExternalTrafficPolicies(t *testing.T) {
	for _, policy := range []v1.ServiceExternalTrafficPolicyType{
		v1.ServiceExternalTrafficPolicyTypeCluster,
		v1.ServiceExternalTrafficPolicyTypeLocal,
	} {
		t.Run(string(policy), func(t *testing.T) {
			svc := &v1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "ns", UID: "33333333-3333-3333-3333-333333333333"},
				Spec: v1.ServiceSpec{
					Type:                  v1.ServiceTypeLoadBalancer,
					ExternalTrafficPolicy: policy,
					Ports:                 []v1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080), Protocol: v1.ProtocolTCP}},
				},
			}

			config, err := AdmitInboundService(svc)
			assert.NoError(t, err, "externalTrafficPolicy %s must be admitted", policy)
			assert.NotNil(t, config, "externalTrafficPolicy %s must be admitted", policy)
		})
	}
}

// TestIsValidEgressIdentity_RejectsReservedNames pins the reserved-name guard at the shared
// chokepoint used by both the pod informer and startup egress discovery.
func TestIsValidEgressIdentity_RejectsReservedNames(t *testing.T) {
	for _, name := range []string{"default-natgw", "Default-NatGW", "DEFAULT-NATGW"} {
		assert.True(t, IsReservedEgressIdentity(name), "%q must be recognised as reserved", name)
		assert.False(t, IsValidEgressIdentity(name),
			"%q names the RP-owned default gateway and must not be usable as an egress identity", name)
	}

	// Controls: shape-valid identities that merely resemble the reserved name stay usable.
	for _, name := range []string{"team-egress", "default-natgw2", "my-default-natgw", "default-natgateway"} {
		assert.False(t, IsReservedEgressIdentity(name), "%q must not be treated as reserved", name)
		assert.True(t, IsValidEgressIdentity(name), "%q must remain a usable egress identity", name)
	}
}

// TestIdleTimeout_AdmissionAndBuildAgree pins that every idle timeout admission accepts can also be
// built. Admission and buildInboundServiceResources enforce the same range from shared constants; if
// they diverge, a value passes admission, EnsureLoadBalancer reports success, and the build then
// fails terminally so the Service never provisions and nothing retries it.
func TestIdleTimeout_AdmissionAndBuildAgree(t *testing.T) {
	newService := func(minutes string) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: "svc", Namespace: "default", UID: "idle-uid",
				Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerIdleTimeout: minutes},
			},
			Spec: v1.ServiceSpec{
				Type:  v1.ServiceTypeLoadBalancer,
				Ports: []v1.ServicePort{{Name: "http", Protocol: v1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt(8080)}},
			},
		}
	}

	for _, minutes := range []string{"4", "5", "30", "31", "60", "100", "101"} {
		t.Run(minutes, func(t *testing.T) {
			config, admitErr := AdmitInboundService(newService(minutes))
			if admitErr != nil {
				return // Rejected up front, with a synchronous error the user sees.
			}
			if !assert.NotNil(t, config, "an admitted Service must carry a config") {
				return
			}
			_, _, _, buildErr := buildInboundServiceResources("idle-uid", config, testConfig())
			assert.NoError(t, buildErr,
				"idle timeout %s passed admission, so the build must accept it too or the Service parks terminally", minutes)
		})
	}
}

// TestBuildOutboundServiceResources_DualStack pins the exact wire shape of a dual-stack egress
// NAT Gateway: an IPv4 and an IPv6 Public IP, each with its version set, attached to the matching
// NAT Gateway list. An IPv6 pod address registered against a gateway with no V6 public path has no
// egress at all, and there is no outbound update path to correct it later.
func TestBuildOutboundServiceResources_DualStack(t *testing.T) {
	const uid = "team-egress"
	dtConfig := testConfig()

	versions := func(pips []armnetwork.PublicIPAddress) map[string]string {
		got := map[string]string{}
		for _, pip := range pips {
			version := ""
			if pip.Properties != nil && pip.Properties.PublicIPAddressVersion != nil {
				version = string(*pip.Properties.PublicIPAddressVersion)
			}
			got[ptr.Deref(pip.Name, "")] = version
		}
		return got
	}
	refNames := func(refs []*armnetwork.SubResource) []string {
		names := []string{}
		for _, ref := range refs {
			parts := strings.Split(ptr.Deref(ref.ID, ""), "/")
			names = append(names, parts[len(parts)-1])
		}
		return names
	}

	t.Run("dual-stack provisions both families", func(t *testing.T) {
		pips, natGw, _ := buildOutboundServiceResources(uid, &OutboundConfig{
			IPFamilies: []string{"IPv4", "IPv6"},
		}, dtConfig)

		assert.Equal(t, map[string]string{
			"team-egress-pip":    "IPv4",
			"team-egress-pip-v6": "IPv6",
		}, versions(pips))
		assert.Equal(t, []string{"team-egress-pip"}, refNames(natGw.Properties.PublicIPAddresses))
		assert.Equal(t, []string{"team-egress-pip-v6"}, refNames(natGw.Properties.PublicIPAddressesV6),
			"the IPv6 address must be attached to the V6 list; the V4 list is a different field")
	})

	t.Run("IPv4-only is unchanged", func(t *testing.T) {
		pips, natGw, _ := buildOutboundServiceResources(uid, &OutboundConfig{
			IPFamilies: []string{"IPv4"},
		}, dtConfig)

		assert.Equal(t, map[string]string{"team-egress-pip": "IPv4"}, versions(pips))
		assert.Equal(t, []string{"team-egress-pip"}, refNames(natGw.Properties.PublicIPAddresses))
		assert.Empty(t, natGw.Properties.PublicIPAddressesV6,
			"an IPv4-only cluster must not be charged for an unused IPv6 address")
	})

	t.Run("a nil config stays IPv4-only", func(t *testing.T) {
		pips, natGw, _ := buildOutboundServiceResources(uid, nil, dtConfig)

		assert.Equal(t, map[string]string{"team-egress-pip": "IPv4"}, versions(pips))
		assert.Empty(t, natGw.Properties.PublicIPAddressesV6)
	})
}

func TestOwnsPublicIPByTags(t *testing.T) {
	pip := func(tags map[string]string) *armnetwork.PublicIPAddress {
		p := &armnetwork.PublicIPAddress{Tags: map[string]*string{}}
		for key, value := range tags {
			p.Tags[key] = ptr.To(value)
		}
		return p
	}
	for _, tc := range []struct {
		name string
		tags map[string]string
		want bool
	}{
		{"service and cluster", map[string]string{consts.ServiceTagKey: "ns/svc", consts.ClusterNameKey: "cluster"}, true},
		{"one of several services", map[string]string{consts.ServiceTagKey: "ns/a, ns/svc", consts.ClusterNameKey: "cluster"}, true},
		{"keys and values in another case", map[string]string{"K8s-Azure-Service": "NS/SVC", "K8s-Azure-Cluster-Name": "CLUSTER"}, true},
		{"no cluster tag", map[string]string{consts.ServiceTagKey: "ns/svc"}, true},
		{"legacy keys", map[string]string{consts.LegacyServiceTagKey: "ns/svc", consts.LegacyClusterNameKey: "cluster"}, true},
		{"legacy keys of another cluster", map[string]string{consts.LegacyServiceTagKey: "ns/svc", consts.LegacyClusterNameKey: "other"}, false},
		{"another cluster", map[string]string{consts.ServiceTagKey: "ns/svc", consts.ClusterNameKey: "other"}, false},
		{"another Service", map[string]string{consts.ServiceTagKey: "ns/other", consts.ClusterNameKey: "cluster"}, false},
		{"a Service with the same prefix", map[string]string{consts.ServiceTagKey: "ns/svc2"}, false},
		{"no service tag", map[string]string{consts.ClusterNameKey: "cluster"}, false},
		{"no tags", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ownsPublicIPByTags(pip(tc.tags), "ns/svc", "cluster"))
		})
	}
	assert.True(t, ownedByClusterTags(pip(map[string]string{consts.LegacyServiceTagKey: "ns/gone", consts.LegacyClusterNameKey: "cluster"}), "cluster"))
	assert.False(t, ownedByClusterTags(pip(map[string]string{consts.LegacyServiceTagKey: "ns/gone", consts.LegacyClusterNameKey: "other"}), "cluster"))
	assert.Equal(t, "legacy", clusterOwnershipTag(pip(map[string]string{consts.LegacyClusterNameKey: " legacy "})))
	assert.Empty(t, clusterOwnershipTag(pip(map[string]string{consts.ClusterNameKey: " "})))
	assert.False(t, ownsPublicIPByTags(nil, "ns/svc", "cluster"))
	assert.False(t, ownsPublicIPByTags(pip(map[string]string{consts.ServiceTagKey: ""}), "", "cluster"))
}

func TestInboundUnitNames(t *testing.T) {
	const uid = "11111111-2222-3333-4444-555555555555"
	service := func(families ...v1.IPFamily) *v1.Service {
		return &v1.Service{ObjectMeta: metav1.ObjectMeta{UID: uid}, Spec: v1.ServiceSpec{IPFamilies: families}}
	}

	assert.Equal(t, []InboundUnit{{Name: uid, Family: v1.IPv4Protocol, Primary: true}}, InboundUnits(service()))
	assert.Equal(t, []InboundUnit{{Name: uid, Family: v1.IPv6Protocol, Primary: true}}, InboundUnits(service(v1.IPv6Protocol)))
	assert.Equal(t, []InboundUnit{
		{Name: uid, Family: v1.IPv4Protocol, Primary: true},
		{Name: uid + "-v6", Family: v1.IPv6Protocol},
	}, InboundUnits(service(v1.IPv4Protocol, v1.IPv6Protocol)))
	assert.Equal(t, []InboundUnit{
		{Name: uid, Family: v1.IPv6Protocol, Primary: true},
		{Name: uid + "-v4", Family: v1.IPv4Protocol},
	}, InboundUnits(service(v1.IPv6Protocol, v1.IPv4Protocol)))
	assert.Nil(t, InboundUnits(nil))

	for name, want := range map[string]struct {
		parent    string
		secondary bool
		unit      bool
	}{
		uid:                            {uid, false, true},
		uid + "-v6":                    {uid, true, true},
		uid + "-V4":                    {uid, true, true},
		uid + "-v5":                    {uid + "-v5", false, false},
		"egress-v6":                    {"egress-v6", false, false},
		"default-natgw":                {"default-natgw", false, false},
		"11111111-2222-3333-4444-5-v6": {"11111111-2222-3333-4444-5-v6", false, false},
	} {
		parent, secondary := ParentServiceUID(name)
		assert.Equal(t, want.parent, parent, name)
		assert.Equal(t, want.secondary, secondary, name)
		assert.Equal(t, want.unit, IsInboundUnitName(name), name)
	}

	// An egress identity's IPv6 Public IP keeps its own naming and is not taken for a unit.
	identity, ok := identityFromPublicIPName(PublicIPNameV6(uid))
	assert.True(t, ok)
	assert.Equal(t, uid, identity)
	identity, _ = identityFromPublicIPName(PublicIPName(uid + "-v6"))
	assert.Equal(t, uid+"-v6", identity)
}

func TestAdmitInboundServiceUnits_DualStack(t *testing.T) {
	const uid = "11111111-2222-3333-4444-555555555555"
	service := func(annotations map[string]string, families ...v1.IPFamily) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{UID: uid, Namespace: "ns", Name: "web", Annotations: annotations},
			Spec: v1.ServiceSpec{
				Type:       v1.ServiceTypeLoadBalancer,
				IPFamilies: families,
				Ports:      []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt(8080)}},
			},
		}
	}
	reason := func(err error) string {
		var ve *InboundConfigValidationError
		if errors.As(err, &ve) {
			return ve.Reason
		}
		return ""
	}

	t.Run("each family reads its own Public IP settings", func(t *testing.T) {
		units, err := AdmitInboundServiceUnits(service(map[string]string{
			consts.ServiceAnnotationPIPNameDualStack[false]:       "pip-v4",
			consts.ServiceAnnotationPIPNameDualStack[true]:        "pip-v6",
			consts.ServiceAnnotationPIPPrefixIDDualStack[true]:    "",
			consts.ServiceAnnotationAzurePIPTags:                  "team=a",
			consts.ServiceAnnotationLoadBalancerIdleTimeout:       "10",
			consts.ServiceAnnotationLoadBalancerResourceGroup:     "user-rg",
			consts.ServiceAnnotationLoadBalancerIPDualStack[true]: "",
		}, v1.IPv4Protocol, v1.IPv6Protocol))
		assert.NoError(t, err)
		if !assert.Len(t, units, 2) {
			return
		}
		for i, want := range []struct{ name, family, pip string }{{uid, "IPv4", "pip-v4"}, {uid + "-v6", "IPv6", "pip-v6"}} {
			assert.Equal(t, want.name, units[i].Unit.Name)
			assert.Equal(t, []string{want.family}, units[i].Config.IPFamilies)
			assert.Equal(t, want.pip, units[i].Config.PIPName)
			assert.Equal(t, "user-rg", units[i].Config.PIPResourceGroup)
			assert.Equal(t, map[string]string{"team": "a"}, units[i].Config.PIPTags)
			assert.Equal(t, int32(10), *units[i].Config.IdleTimeoutMinutes)
		}
	})

	t.Run("each family reads its own address and prefix", func(t *testing.T) {
		units, err := AdmitInboundServiceUnits(service(map[string]string{
			consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "20.1.2.3",
			consts.ServiceAnnotationPIPPrefixIDDualStack[true]:     testPrefixID,
		}, v1.IPv6Protocol, v1.IPv4Protocol))
		assert.NoError(t, err)
		if assert.Len(t, units, 2) {
			assert.Empty(t, units[0].Config.LoadBalancerIP, "the IPv6 primary does not take the IPv4 address")
			assert.Equal(t, testPrefixID, units[0].Config.PIPPrefixID)
			assert.Equal(t, "20.1.2.3", units[1].Config.LoadBalancerIP)
			assert.Empty(t, units[1].Config.PIPPrefixID, "the IPv4 unit does not take the IPv6 prefix")
		}
	})

	t.Run("each family can choose its own Public IP prefix", func(t *testing.T) {
		v4Prefix := strings.Replace(testPrefixID, "prefix", "prefix-v4", 1)
		v6Prefix := strings.Replace(testPrefixID, "prefix", "prefix-v6", 1)
		units, err := AdmitInboundServiceUnits(service(map[string]string{
			consts.ServiceAnnotationPIPPrefixIDDualStack[false]: v4Prefix,
			consts.ServiceAnnotationPIPPrefixIDDualStack[true]:  v6Prefix,
		}, v1.IPv4Protocol, v1.IPv6Protocol))
		assert.NoError(t, err)
		if assert.Len(t, units, 2) {
			assert.Equal(t, v4Prefix, units[0].Config.PIPPrefixID)
			assert.Equal(t, v6Prefix, units[1].Config.PIPPrefixID)
		}
	})

	t.Run("spec.loadBalancerIP applies to its own family", func(t *testing.T) {
		svc := service(nil, v1.IPv4Protocol, v1.IPv6Protocol)
		svc.Spec.LoadBalancerIP = "2603:1030::7"
		units, err := AdmitInboundServiceUnits(svc)
		assert.NoError(t, err)
		if assert.Len(t, units, 2) {
			assert.Empty(t, units[0].Config.LoadBalancerIP)
			assert.Equal(t, "2603:1030::7", units[1].Config.LoadBalancerIP)
		}

		svc.Spec.LoadBalancerIP = "not-an-ip"
		_, err = AdmitInboundServiceUnits(svc)
		assert.Equal(t, "InvalidLoadBalancerIP", reason(err))
	})

	t.Run("spec.loadBalancerIP conflicts only with the matching family annotation", func(t *testing.T) {
		svc := service(map[string]string{
			consts.ServiceAnnotationLoadBalancerIPDualStack[true]: "2603:1030::7",
		}, v1.IPv4Protocol, v1.IPv6Protocol)
		svc.Spec.LoadBalancerIP = "20.1.2.3"
		units, err := AdmitInboundServiceUnits(svc)
		assert.NoError(t, err)
		if assert.Len(t, units, 2) {
			assert.Equal(t, "20.1.2.3", units[0].Config.LoadBalancerIP)
			assert.Equal(t, "2603:1030::7", units[1].Config.LoadBalancerIP)
		}

		svc.Annotations[consts.ServiceAnnotationLoadBalancerIPDualStack[false]] = "20.1.2.4"
		_, err = AdmitInboundServiceUnits(svc)
		assert.Equal(t, "ConflictingPublicIPSettings", reason(err))

		svc = service(map[string]string{
			consts.ServiceAnnotationLoadBalancerIPDualStack[true]: "2603:1030::8",
		}, v1.IPv4Protocol, v1.IPv6Protocol)
		svc.Spec.LoadBalancerIP = "2603:1030::7"
		_, err = AdmitInboundServiceUnits(svc)
		assert.Equal(t, "ConflictingPublicIPSettings", reason(err))
	})

	t.Run("single-stack spec.loadBalancerIP conflicts with any served-family address annotation", func(t *testing.T) {
		svc := service(map[string]string{
			consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "20.1.2.3",
		}, v1.IPv4Protocol)
		svc.Spec.LoadBalancerIP = "2001:db8::1"
		_, err := AdmitInboundServiceUnits(svc)
		assert.Equal(t, "ConflictingPublicIPSettings", reason(err))

		svc = service(map[string]string{
			consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "20.1.2.3",
			consts.ServiceAnnotationLoadBalancerIPDualStack[true]:  "2603:1030::7",
		}, v1.IPv4Protocol, v1.IPv6Protocol)
		svc.Spec.LoadBalancerIP = "20.1.2.3"
		units, err := AdmitInboundServiceUnits(svc)
		assert.NoError(t, err)
		if assert.Len(t, units, 2) {
			assert.Equal(t, "20.1.2.3", units[0].Config.LoadBalancerIP)
			assert.Equal(t, "2603:1030::7", units[1].Config.LoadBalancerIP)
		}
	})

	t.Run("the same Public IP for both families is rejected", func(t *testing.T) {
		_, err := AdmitInboundServiceUnits(service(map[string]string{
			consts.ServiceAnnotationPIPNameDualStack[false]: "shared",
			consts.ServiceAnnotationPIPNameDualStack[true]:  "SHARED",
		}, v1.IPv4Protocol, v1.IPv6Protocol))
		assert.Equal(t, "ConflictingPublicIPSettings", reason(err))
	})

	t.Run("an invalid setting of one family rejects the whole Service", func(t *testing.T) {
		units, err := AdmitInboundServiceUnits(service(map[string]string{
			consts.ServiceAnnotationPIPPrefixIDDualStack[true]: "not-a-prefix-id",
		}, v1.IPv4Protocol, v1.IPv6Protocol))
		assert.Equal(t, "InvalidPublicIPPrefix", reason(err))
		assert.Nil(t, units)
	})

	t.Run("allowing every source needs the /0 of every family", func(t *testing.T) {
		for ranges, want := range map[string]string{
			"0.0.0.0/0":      "UnsupportedAccessRestriction",
			"::/0":           "UnsupportedAccessRestriction",
			"0.0.0.0/0,::/0": "",
		} {
			svc := service(nil, v1.IPv4Protocol, v1.IPv6Protocol)
			svc.Spec.LoadBalancerSourceRanges = strings.Split(ranges, ",")
			_, err := AdmitInboundServiceUnits(svc)
			assert.Equal(t, want, reason(err), ranges)

			annotated := service(map[string]string{v1.AnnotationLoadBalancerSourceRangesKey: ranges}, v1.IPv4Protocol, v1.IPv6Protocol)
			_, err = AdmitInboundServiceUnits(annotated)
			assert.Equal(t, want, reason(err), "annotation "+ranges)
		}
	})

	t.Run("both families Public IP annotations of a dual-stack Service are supported", func(t *testing.T) {
		svc := service(map[string]string{
			consts.ServiceAnnotationPIPNameDualStack[false]:       "pip-v4",
			consts.ServiceAnnotationPIPNameDualStack[true]:        "pip-v6",
			consts.ServiceAnnotationPIPPrefixIDDualStack[false]:   testPrefixID,
			consts.ServiceAnnotationLoadBalancerIPDualStack[true]: "2603:1030::7",
			consts.ServiceAnnotationLoadBalancerResourceGroup:     "user-rg",
		}, v1.IPv4Protocol, v1.IPv6Protocol)
		assert.Empty(t, UnsupportedServiceAnnotations(svc), "the resource group is used by a chosen Public IP")
		svc.Spec.IPFamilies = []v1.IPFamily{v1.IPv4Protocol}
		assert.Equal(t, []string{
			consts.ServiceAnnotationLoadBalancerIPDualStack[true],
			consts.ServiceAnnotationPIPNameDualStack[true],
		}, UnsupportedServiceAnnotations(svc), "an IPv4 Service uses the plain Public IP annotations; only the IPv6 ones have no effect")
	})

	t.Run("IPv6-only Public IP name makes resource group supported on a dual-stack Service", func(t *testing.T) {
		svc := service(map[string]string{
			consts.ServiceAnnotationPIPNameDualStack[true]:    "pip-v6",
			consts.ServiceAnnotationLoadBalancerResourceGroup: "user-rg",
		}, v1.IPv4Protocol, v1.IPv6Protocol)
		assert.Empty(t, UnsupportedServiceAnnotations(svc), "the resource group is used by the IPv6 Public IP name")

		units, err := AdmitInboundServiceUnits(svc)
		assert.NoError(t, err)
		if assert.Len(t, units, 2) {
			assert.Empty(t, units[0].Config.PIPName)
			assert.Equal(t, "pip-v6", units[1].Config.PIPName)
			assert.Equal(t, "user-rg", units[1].Config.PIPResourceGroup)
		}
	})
}
