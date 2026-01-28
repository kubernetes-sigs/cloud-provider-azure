package difftracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestCompleteServiceCreationWorkflow tests the entire workflow from Service -> Config -> Resources
func TestCompleteServiceCreationWorkflow(t *testing.T) {
	// Step 1: Create a realistic Kubernetes Service
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-web-service",
			Namespace: "production",
			UID:       "abc123-service-uid",
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{
				{
					Name:       "http",
					Protocol:   v1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt(8080),
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

	// Step 2: Extract InboundConfig from Service
	inboundConfig := ExtractInboundConfigFromService(service)
	assert.NotNil(t, inboundConfig, "Should extract config from service")
	assert.Len(t, inboundConfig.FrontendPorts, 2, "Should have 2 frontend ports")
	assert.Len(t, inboundConfig.BackendPorts, 2, "Should have 2 backend ports")

	// Validate port mappings
	assert.Equal(t, int32(80), inboundConfig.FrontendPorts[0].Port)
	assert.Equal(t, int32(8080), inboundConfig.BackendPorts[0].Port)
	assert.Equal(t, "TCP", inboundConfig.FrontendPorts[0].Protocol)

	assert.Equal(t, int32(443), inboundConfig.FrontendPorts[1].Port)
	assert.Equal(t, int32(8443), inboundConfig.BackendPorts[1].Port)

	// Step 3: Build Azure resources using the config
	dtConfig := Config{
		SubscriptionID:             "test-subscription-123",
		ResourceGroup:              "production-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "my-service-gateway",
		ServiceGatewayID:           "/subscriptions/test-subscription-123/resourceGroups/production-rg/providers/Microsoft.Network/serviceGateways/my-service-gateway",
	}

	serviceUID := string(service.UID)
	pip, lb, servicesDTO := buildInboundServiceResources(serviceUID, inboundConfig, dtConfig)

	// Step 4: Validate Public IP resource
	assert.NotNil(t, pip.Name)
	assert.Equal(t, serviceUID+"-pip", *pip.Name)
	assert.Equal(t, "eastus", *pip.Location)
	assert.Contains(t, *pip.ID, "test-subscription-123")
	assert.Contains(t, *pip.ID, "production-rg")
	assert.Contains(t, *pip.ID, serviceUID+"-pip")

	// Step 5: Validate LoadBalancer resource
	assert.NotNil(t, lb.Name)
	assert.Equal(t, serviceUID, *lb.Name)
	assert.Equal(t, "eastus", *lb.Location)

	// Validate backend pool
	assert.Len(t, lb.Properties.BackendAddressPools, 1)
	assert.Equal(t, serviceUID, *lb.Properties.BackendAddressPools[0].Name)

	// Validate LB rules match service ports
	assert.Len(t, lb.Properties.LoadBalancingRules, 2)

	// HTTP rule
	httpRule := lb.Properties.LoadBalancingRules[0]
	assert.Equal(t, "rule-tcp-80", *httpRule.Name)
	assert.Equal(t, int32(80), *httpRule.Properties.FrontendPort)
	assert.Equal(t, int32(8080), *httpRule.Properties.BackendPort)
	assert.False(t, *httpRule.Properties.EnableFloatingIP, "Floating IP should be disabled for PodIP backend")
	assert.Nil(t, httpRule.Properties.Probe, "No health probe for PodIP backend")

	// HTTPS rule
	httpsRule := lb.Properties.LoadBalancingRules[1]
	assert.Equal(t, "rule-tcp-443", *httpsRule.Name)
	assert.Equal(t, int32(443), *httpsRule.Properties.FrontendPort)
	assert.Equal(t, int32(8443), *httpsRule.Properties.BackendPort)

	// Step 6: Validate ServicesDTO for ServiceGateway registration
	assert.Len(t, servicesDTO.Services, 1)
	assert.Equal(t, Inbound, servicesDTO.Services[0].ServiceType)
	assert.Contains(t, servicesDTO.Services[0].Service, serviceUID)
}

// TestWorkflowWithUDPService tests the workflow with a UDP service
func TestWorkflowWithUDPService(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dns-service",
			Namespace: "kube-system",
			UID:       "dns-service-uid-456",
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{
				{
					Name:       "dns",
					Protocol:   v1.ProtocolUDP,
					Port:       53,
					TargetPort: intstr.FromInt(5353),
				},
				{
					Name:       "dns-tcp",
					Protocol:   v1.ProtocolTCP,
					Port:       53,
					TargetPort: intstr.FromInt(5353),
				},
			},
		},
	}

	// Extract config
	config := ExtractInboundConfigFromService(service)
	assert.NotNil(t, config)
	assert.Len(t, config.FrontendPorts, 2)

	// Verify protocols
	assert.Equal(t, "UDP", config.FrontendPorts[0].Protocol)
	assert.Equal(t, "TCP", config.FrontendPorts[1].Protocol)

	// Build resources
	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "westus",
		ServiceGatewayResourceName: "test-sgw",
		ServiceGatewayID:           "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}

	_, lb, _ := buildInboundServiceResources(string(service.UID), config, dtConfig)

	// Verify rules have correct protocols
	assert.Len(t, lb.Properties.LoadBalancingRules, 2)
	assert.Equal(t, "rule-udp-53", *lb.Properties.LoadBalancingRules[0].Name)
	assert.Equal(t, "rule-tcp-53", *lb.Properties.LoadBalancingRules[1].Name)
}

// TestWorkflowWithMinimalService tests edge case with minimal service definition
func TestWorkflowWithMinimalService(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "simple-service",
			Namespace: "default",
			UID:       "simple-uid",
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{
				{
					Port: 8080, // Minimal: no name, no protocol, no targetPort
				},
			},
		},
	}

	config := ExtractInboundConfigFromService(service)
	assert.NotNil(t, config)

	// Should default protocol to TCP
	assert.Equal(t, "TCP", config.FrontendPorts[0].Protocol)

	// Should use Port for both frontend and backend when TargetPort not set
	assert.Equal(t, int32(8080), config.FrontendPorts[0].Port)
	assert.Equal(t, int32(8080), config.BackendPorts[0].Port)
}

// TestWorkflowWithComplexPortMapping tests complex port mapping scenarios
func TestWorkflowWithComplexPortMapping(t *testing.T) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "complex-service",
			Namespace: "default",
			UID:       "complex-uid",
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					TargetPort: intstr.FromInt(8080), // Int targetPort
				},
				{
					Name:       "admin",
					Port:       9090,
					TargetPort: intstr.IntOrString{}, // Unset targetPort
				},
				{
					Name:       "metrics",
					Port:       9091,
					TargetPort: intstr.FromString("metrics-port"), // Named targetPort
				},
			},
		},
	}

	config := ExtractInboundConfigFromService(service)
	assert.NotNil(t, config)
	assert.Len(t, config.FrontendPorts, 3)
	assert.Len(t, config.BackendPorts, 3)

	// Port 1: Should use TargetPort
	assert.Equal(t, int32(80), config.FrontendPorts[0].Port)
	assert.Equal(t, int32(8080), config.BackendPorts[0].Port)

	// Port 2: Should fall back to Port when TargetPort unset
	assert.Equal(t, int32(9090), config.FrontendPorts[1].Port)
	assert.Equal(t, int32(9090), config.BackendPorts[1].Port)

	// Port 3: Should fall back to Port for named targetPort
	assert.Equal(t, int32(9091), config.FrontendPorts[2].Port)
	assert.Equal(t, int32(9091), config.BackendPorts[2].Port)
}

// TestResourceIDsAreConsistent validates resource IDs are properly formatted
func TestResourceIDsAreConsistent(t *testing.T) {
	serviceUID := "test-service-123"
	dtConfig := Config{
		SubscriptionID:             "sub-abc",
		ResourceGroup:              "rg-xyz",
		Location:                   "centralus",
		ServiceGatewayResourceName: "sgw-test",
		ServiceGatewayID:           "/subscriptions/sub-abc/resourceGroups/rg-xyz/providers/Microsoft.Network/serviceGateways/sgw-test",
	}

	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}

	pip, lb, _ := buildInboundServiceResources(serviceUID, config, dtConfig)

	// Validate PIP ID format
	expectedPIPID := "/subscriptions/sub-abc/resourceGroups/rg-xyz/providers/Microsoft.Network/publicIPAddresses/test-service-123-pip"
	assert.Equal(t, expectedPIPID, *pip.ID)

	// Validate LB references PIP by ID
	frontendIP := lb.Properties.FrontendIPConfigurations[0]
	assert.Equal(t, expectedPIPID, *frontendIP.Properties.PublicIPAddress.ID)

	// Validate backend pool ID in rule
	expectedBackendPoolID := "/subscriptions/sub-abc/resourceGroups/rg-xyz/providers/Microsoft.Network/loadBalancers/test-service-123/backendAddressPools/test-service-123"
	assert.Equal(t, expectedBackendPoolID, *lb.Properties.LoadBalancingRules[0].Properties.BackendAddressPool.ID)

	// Validate frontend IP config ID in rule
	expectedFrontendIPID := "/subscriptions/sub-abc/resourceGroups/rg-xyz/providers/Microsoft.Network/loadBalancers/test-service-123/frontendIPConfigurations/frontend"
	assert.Equal(t, expectedFrontendIPID, *lb.Properties.LoadBalancingRules[0].Properties.FrontendIPConfiguration.ID)
}

// TestOutboundWorkflow tests NAT Gateway creation workflow
func TestOutboundWorkflow(t *testing.T) {
	egressUID := "egress-gateway-789"
	dtConfig := Config{
		SubscriptionID:             "sub-123",
		ResourceGroup:              "rg-456",
		Location:                   "westus2",
		ServiceGatewayResourceName: "sgw-789",
		ServiceGatewayID:           "/subscriptions/sub-123/resourceGroups/rg-456/providers/Microsoft.Network/serviceGateways/sgw-789",
	}

	pip, natGw, servicesDTO := buildOutboundServiceResources(egressUID, nil, dtConfig)

	// Validate PIP
	assert.Equal(t, egressUID+"-pip", *pip.Name)
	assert.Equal(t, "westus2", *pip.Location)

	// Validate NAT Gateway
	assert.Equal(t, egressUID, *natGw.Name)
	assert.Equal(t, "westus2", *natGw.Location)

	// Verify NAT Gateway references ServiceGateway
	assert.NotNil(t, natGw.Properties.ServiceGateway)
	assert.Equal(t, dtConfig.ServiceGatewayID, *natGw.Properties.ServiceGateway.ID)

	// Verify NAT Gateway references PIP
	assert.Len(t, natGw.Properties.PublicIPAddresses, 1)
	expectedPIPID := "/subscriptions/sub-123/resourceGroups/rg-456/providers/Microsoft.Network/publicIPAddresses/egress-gateway-789-pip"
	assert.Equal(t, expectedPIPID, *natGw.Properties.PublicIPAddresses[0].ID)

	// Validate ServicesDTO
	assert.Len(t, servicesDTO.Services, 1)
	assert.Equal(t, Outbound, servicesDTO.Services[0].ServiceType)
}

// TestMultipleServicesIndependence validates that multiple services don't interfere
func TestMultipleServicesIndependence(t *testing.T) {
	dtConfig := Config{
		SubscriptionID:             "sub",
		ResourceGroup:              "rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "sgw",
		ServiceGatewayID:           "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/serviceGateways/sgw",
	}

	// Create resources for service 1
	config1 := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}
	pip1, lb1, dto1 := buildInboundServiceResources("service-1", config1, dtConfig)

	// Create resources for service 2
	config2 := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 443, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8443, Protocol: "TCP"}},
	}
	pip2, lb2, dto2 := buildInboundServiceResources("service-2", config2, dtConfig)

	// Verify complete independence
	assert.NotEqual(t, *pip1.Name, *pip2.Name)
	assert.NotEqual(t, *pip1.ID, *pip2.ID)
	assert.NotEqual(t, *lb1.Name, *lb2.Name)
	assert.NotEqual(t, *lb1.ID, *lb2.ID)

	// Verify each has correct ports
	assert.Equal(t, int32(80), *lb1.Properties.LoadBalancingRules[0].Properties.FrontendPort)
	assert.Equal(t, int32(443), *lb2.Properties.LoadBalancingRules[0].Properties.FrontendPort)

	// Verify DTOs are independent
	assert.NotEqual(t, dto1.Services[0].Service, dto2.Services[0].Service)
}

// TestConfigValidationInWorkflow ensures Config validation catches errors
func TestConfigValidationInWorkflow(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		shouldError bool
	}{
		{
			name: "valid config",
			config: Config{
				SubscriptionID:             "sub",
				ResourceGroup:              "rg",
				Location:                   "eastus",
				VNetName:                   "test-vnet",
				ServiceGatewayResourceName: "sgw",
				ServiceGatewayID:           "/sub/rg/sgw",
			},
			shouldError: false,
		},
		{
			name: "missing subscription",
			config: Config{
				ResourceGroup:              "rg",
				Location:                   "eastus",
				ServiceGatewayResourceName: "sgw",
				ServiceGatewayID:           "/sub/rg/sgw",
			},
			shouldError: true,
		},
		{
			name: "empty location",
			config: Config{
				SubscriptionID:             "sub",
				ResourceGroup:              "rg",
				Location:                   "",
				ServiceGatewayResourceName: "sgw",
				ServiceGatewayID:           "/sub/rg/sgw",
			},
			shouldError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.shouldError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestLBRuleNaming validates rule naming convention
func TestLBRuleNaming(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{
			{Port: 80, Protocol: "TCP"},
			{Port: 443, Protocol: "TCP"},
			{Port: 53, Protocol: "UDP"},
			{Port: 8080, Protocol: "tcp"}, // lowercase
		},
		BackendPorts: []PortMapping{
			{Port: 8080, Protocol: "TCP"},
			{Port: 8443, Protocol: "TCP"},
			{Port: 5353, Protocol: "UDP"},
			{Port: 9090, Protocol: "tcp"},
		},
	}

	dtConfig := Config{
		SubscriptionID:             "sub",
		ResourceGroup:              "rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "sgw",
		ServiceGatewayID:           "/sub/rg/sgw",
	}

	_, lb, _ := buildInboundServiceResources("test-svc", config, dtConfig)

	assert.Len(t, lb.Properties.LoadBalancingRules, 4)

	// Verify naming convention: rule-{protocol}-{port}
	assert.Equal(t, "rule-tcp-80", *lb.Properties.LoadBalancingRules[0].Name)
	assert.Equal(t, "rule-tcp-443", *lb.Properties.LoadBalancingRules[1].Name)
	assert.Equal(t, "rule-udp-53", *lb.Properties.LoadBalancingRules[2].Name)
	assert.Equal(t, "rule-tcp-8080", *lb.Properties.LoadBalancingRules[3].Name)
}

// TestBackendPoolPopulation validates backend pool structure for ServiceGateway
func TestBackendPoolPopulation(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}

	dtConfig := Config{
		SubscriptionID:             "sub",
		ResourceGroup:              "rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "sgw",
		ServiceGatewayID:           "/sub/rg/sgw",
	}

	serviceUID := "my-service-uid"
	_, lb, _ := buildInboundServiceResources(serviceUID, config, dtConfig)

	// Backend pool should be named after serviceUID for SLB
	assert.Len(t, lb.Properties.BackendAddressPools, 1)
	backendPool := lb.Properties.BackendAddressPools[0]

	assert.Equal(t, serviceUID, *backendPool.Name)

	// Backend pool should be empty (populated by ServiceGateway)
	assert.NotNil(t, backendPool.Properties)
	// Properties exist but no addresses pre-populated
}

// TestRoundTripServiceToResourcesAndBack validates the complete cycle
func TestRoundTripServiceToResourcesAndBack(t *testing.T) {
	// Start with a real-world service
	originalService := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "production-api",
			Namespace: "api-namespace",
			UID:       "prod-api-uid-xyz",
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{
				{Name: "http", Protocol: v1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt(3000)},
				{Name: "https", Protocol: v1.ProtocolTCP, Port: 443, TargetPort: intstr.FromInt(3443)},
				{Name: "grpc", Protocol: v1.ProtocolTCP, Port: 50051, TargetPort: intstr.FromInt(50051)},
			},
		},
	}

	// Extract config
	config := ExtractInboundConfigFromService(originalService)
	assert.NotNil(t, config)

	// Verify all ports extracted
	assert.Len(t, config.FrontendPorts, 3)
	assert.Len(t, config.BackendPorts, 3)

	// Build Azure resources
	dtConfig := Config{
		SubscriptionID:             "prod-sub",
		ResourceGroup:              "prod-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "prod-sgw",
		ServiceGatewayID:           "/subscriptions/prod-sub/resourceGroups/prod-rg/providers/Microsoft.Network/serviceGateways/prod-sgw",
	}

	pip, lb, servicesDTO := buildInboundServiceResources(string(originalService.UID), config, dtConfig)

	// Verify we can reconstruct the port mapping from LB rules
	assert.Len(t, lb.Properties.LoadBalancingRules, 3)

	reconstructedMappings := make(map[int32]int32) // frontend -> backend
	for _, rule := range lb.Properties.LoadBalancingRules {
		reconstructedMappings[*rule.Properties.FrontendPort] = *rule.Properties.BackendPort
	}

	// Verify mappings match original service
	assert.Equal(t, int32(3000), reconstructedMappings[80])
	assert.Equal(t, int32(3443), reconstructedMappings[443])
	assert.Equal(t, int32(50051), reconstructedMappings[50051])

	// Verify resources reference each other correctly
	pipID := *pip.ID
	lbFrontendPIPRef := *lb.Properties.FrontendIPConfigurations[0].Properties.PublicIPAddress.ID
	assert.Equal(t, pipID, lbFrontendPIPRef, "LB should reference the PIP we created")

	// Verify ServicesDTO can be used for ServiceGateway API
	assert.Len(t, servicesDTO.Services, 1)
	assert.Equal(t, Inbound, servicesDTO.Services[0].ServiceType)
	assert.Contains(t, servicesDTO.Services[0].Service, string(originalService.UID))
}
