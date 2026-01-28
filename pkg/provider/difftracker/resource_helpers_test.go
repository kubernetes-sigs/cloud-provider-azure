package difftracker

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestExtractInboundConfigFromService_NilService(t *testing.T) {
	config := ExtractInboundConfigFromService(nil)
	assert.Nil(t, config)
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

	// Named ports should fall back to Port
	assert.Equal(t, int32(80), config.FrontendPorts[0].Port)
	assert.Equal(t, int32(80), config.BackendPorts[0].Port)
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
		ServiceGatewayID:           "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}

	pip, lb, servicesDTO := buildInboundServiceResources("service-uid-123", config, dtConfig)

	// Verify PIP
	assert.NotNil(t, pip.Name)
	assert.Equal(t, "service-uid-123-pip", *pip.Name)
	assert.Equal(t, armnetwork.PublicIPAddressSKUNameStandardV2, *pip.SKU.Name)
	assert.Equal(t, "eastus", *pip.Location)

	// Verify LoadBalancer
	assert.NotNil(t, lb.Name)
	assert.Equal(t, "service-uid-123", *lb.Name)
	assert.Equal(t, armnetwork.LoadBalancerSKUNameService, *lb.SKU.Name)
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
		ServiceGatewayID:           "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}

	pip, lb, servicesDTO := buildInboundServiceResources("service-uid-123", nil, dtConfig)

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
		ServiceGatewayID:           "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}

	_, lb, _ := buildInboundServiceResources("service-uid-udp", config, dtConfig)

	// Verify UDP rule
	assert.Len(t, lb.Properties.LoadBalancingRules, 1)
	rule := lb.Properties.LoadBalancingRules[0]
	assert.Equal(t, "rule-udp-53", *rule.Name)
	assert.Equal(t, armnetwork.TransportProtocolUDP, *rule.Properties.Protocol)
	assert.Equal(t, int32(53), *rule.Properties.FrontendPort)
	assert.Equal(t, int32(5353), *rule.Properties.BackendPort)
}

func TestBuildOutboundServiceResources_Basic(t *testing.T) {
	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "centralus",
		ServiceGatewayResourceName: "test-sgw",
		ServiceGatewayID:           "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
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
	assert.Equal(t, dtConfig.ServiceGatewayID, *natGw.Properties.ServiceGateway.ID)

	// Verify NAT Gateway has PIP reference
	assert.Len(t, natGw.Properties.PublicIPAddresses, 1)
	assert.Contains(t, *natGw.Properties.PublicIPAddresses[0].ID, "egress-uid-456-pip")

	// Verify ServicesDTO
	assert.Len(t, servicesDTO.Services, 1)
	assert.Contains(t, servicesDTO.Services[0].Service, "egress-uid-456")
	assert.Equal(t, Outbound, servicesDTO.Services[0].ServiceType)
}

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
		ServiceGatewayID:           "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}

	_, lb, _ := buildInboundServiceResources("my-service-uid", config, dtConfig)

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
		ServiceGatewayID:           "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}

	_, lb, _ := buildInboundServiceResources("service-uid", config, dtConfig)

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
		ServiceGatewayID:           "/subscriptions/sub-123/resourceGroups/rg-456/providers/Microsoft.Network/serviceGateways/sgw-789",
	}

	pip, lb, _ := buildInboundServiceResources("svc-abc", config, dtConfig)

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
