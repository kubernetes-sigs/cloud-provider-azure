package difftracker

import (
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

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
			name: "dual-stack rejected",
			config: &InboundConfig{
				FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
				BackendPorts:  []PortMapping{{Port: 80, Protocol: "TCP"}},
				IPFamilies:    []string{"IPv4", "IPv6"},
			},
			wantReason: "UnsupportedDualStack",
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
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "test-sgw",
	}

	pip, lb, servicesDTO, err := buildInboundServiceResources("service-uid-123", config, dtConfig)
	assert.NoError(t, err)

	// Verify PIP
	assert.NotNil(t, pip.Name)
	assert.Equal(t, "service-uid-123-pip", *pip.Name)
	assert.Equal(t, armnetwork.PublicIPAddressSKUNameStandardV2, *pip.SKU.Name)
	assert.Equal(t, "eastus", *pip.Location)

	// Verify LoadBalancer
	assert.NotNil(t, lb.Name)
	assert.Equal(t, "service-uid-123", *lb.Name)
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
	assert.Contains(t, servicesDTO.Services[0].Service, "service-uid-123")
	assert.Equal(t, Inbound, servicesDTO.Services[0].ServiceType)
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
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "centralus",
		ServiceGatewayResourceName: "test-sgw",
	}

	pip, natGw, servicesDTO := buildOutboundServiceResources("egress-uid-456", nil, dtConfig)

	// Verify PIP
	assert.NotNil(t, pip.Name)
	assert.Equal(t, "egress-uid-456-pip", *pip.Name)
	assert.Equal(t, armnetwork.PublicIPAddressSKUNameStandardV2, *pip.SKU.Name)
	assert.Equal(t, "centralus", *pip.Location)

	// Verify NAT Gateway
	assert.NotNil(t, natGw.Name)
	assert.Equal(t, "egress-uid-456", *natGw.Name)
	assert.Equal(t, armnetwork.NatGatewaySKUNameStandardV2, *natGw.SKU.Name)
	assert.Equal(t, "centralus", *natGw.Location)

	// Verify NAT Gateway has ServiceGateway reference
	assert.NotNil(t, natGw.Properties.ServiceGateway)
	assert.Equal(t, dtConfig.ServiceGatewayResourceID(), *natGw.Properties.ServiceGateway.ID)

	// Verify NAT Gateway has PIP reference
	assert.Len(t, natGw.Properties.PublicIPAddresses, 1)
	assert.Contains(t, *natGw.Properties.PublicIPAddresses[0].ID, "egress-uid-456-pip")

	// Verify ServicesDTO
	assert.Len(t, servicesDTO.Services, 1)
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

// TestBuildInboundServiceResources_IPFamilies verifies that the Public IP version follows the
// Service IP family, and that dual-stack (unsupported for PodIP backend pools) is rejected.
func TestBuildInboundServiceResources_IPFamilies(t *testing.T) {
	build := func(fams ...v1.IPFamily) (armnetwork.PublicIPAddress, error) {
		cfg := ExtractInboundConfigFromService(&v1.Service{Spec: v1.ServiceSpec{
			IPFamilies: fams,
			Ports:      []v1.ServicePort{{Port: 80, Protocol: v1.ProtocolTCP, TargetPort: intstr.FromInt(80)}},
		}})
		pip, _, _, err := buildInboundServiceResources("svc", cfg, testConfig())
		return pip, err
	}

	pip4, err := build(v1.IPv4Protocol)
	assert.NoError(t, err)
	assert.Equal(t, armnetwork.IPVersionIPv4, *pip4.Properties.PublicIPAddressVersion)

	pip6, err := build(v1.IPv6Protocol)
	assert.NoError(t, err)
	assert.Equal(t, armnetwork.IPVersionIPv6, *pip6.Properties.PublicIPAddressVersion)

	_, err = build(v1.IPv4Protocol, v1.IPv6Protocol)
	assert.Error(t, err, "dual-stack services must be rejected for PodIP backend pools")
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
		strings.Repeat("a", 76), // max length (reserves 4 chars for the "-pip" suffix)
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
		strings.Repeat("a", 77), // exceeds 76: derived PIP name would overflow Azure's 80-char limit
		strings.Repeat("a", 81), // too long
	}
	for _, n := range invalid {
		assert.False(t, IsValidEgressIdentity(n), "expected %q to be an invalid egress identity", n)
	}

	// A max-length egress identity must yield a Public IP name within Azure's 80-char limit.
	_, pipName := buildOutboundResourceNames(strings.Repeat("a", 76))
	assert.LessOrEqual(t, len(pipName), 80,
		"PIP name derived from a max-length egress identity must fit Azure's 80-char publicIPAddresses limit")
}

func TestExtractInboundConfigFromService_DropsAffinityAndIdleTimeout(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-service",
			Namespace:   "default",
			Annotations: map[string]string{consts.ServiceAnnotationLoadBalancerIdleTimeout: "30"},
		},
		Spec: v1.ServiceSpec{
			SessionAffinity: v1.ServiceAffinityClientIP,
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
	if !assert.NotNil(t, config) {
		t.FailNow()
	}

	// The extractor maps neither SessionAffinity nor the idle-timeout annotation, so both stay nil.
	assert.Nil(t, config.SessionPersistence)
	assert.Nil(t, config.IdleTimeoutMinutes)
}
