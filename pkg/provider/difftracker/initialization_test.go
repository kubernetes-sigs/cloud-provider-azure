package difftracker

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestLogSyncStringIntMap_EmptyMap(t *testing.T) {
	m := &sync.Map{}
	// Should not panic with empty map
	LogSyncStringIntMap("test", m)
}

func TestLogSyncStringIntMap_WithData(t *testing.T) {
	m := &sync.Map{}
	m.Store("service1", 10)
	m.Store("service2", 20)
	m.Store("service3", -34)

	// Should not panic with data
	LogSyncStringIntMap("test", m)
}

func TestDumpStringIntSyncMap_VariousTypes(t *testing.T) {
	m := &sync.Map{}
	m.Store("int", 42)
	m.Store("int32", int32(32))
	m.Store("int64", int64(64))
	intVal := 100
	m.Store("intptr", &intVal)

	result := dumpStringIntSyncMap(m)

	assert.Equal(t, 42, result["int"])
	assert.Equal(t, 32, result["int32"])
	assert.Equal(t, 64, result["int64"])
	assert.Equal(t, 100, result["intptr"])
}

func TestDumpStringIntSyncMap_NilPointer(t *testing.T) {
	m := &sync.Map{}
	var nilInt *int
	m.Store("nilptr", nilInt)

	result := dumpStringIntSyncMap(m)

	// Nil pointer should not appear in result
	_, exists := result["nilptr"]
	assert.False(t, exists)
}

func TestSyncMapFromMap_Empty(t *testing.T) {
	input := make(map[string]int)
	result := syncMapFromMap(input)

	assert.NotNil(t, result)
	// Should be empty
	count := 0
	result.Range(func(k, v any) bool {
		count++
		return true
	})
	assert.Equal(t, 0, count)
}

func TestSyncMapFromMap_WithData(t *testing.T) {
	input := map[string]int{
		"service1": 10,
		"service2": 20,
		"service3": -34,
	}

	result := syncMapFromMap(input)

	assert.NotNil(t, result)

	// Verify all entries transferred
	for key, expectedVal := range input {
		val, ok := result.Load(key)
		assert.True(t, ok, "Key %s should exist", key)
		assert.Equal(t, expectedVal, val, "Value for key %s should match", key)
	}
}

func TestNewIgnoreCaseSetFromSlice_Duplicates(t *testing.T) {
	items := []string{"service1", "SERVICE1", "service1", "service2"}
	set := newIgnoreCaseSetFromSlice(items)

	// Should deduplicate case-insensitively
	assert.Equal(t, 2, set.Len())
	assert.True(t, set.Has("service1"))
	assert.True(t, set.Has("service2"))
}

func TestExtractInboundConfigFromService_MixedTargetPorts(t *testing.T) {
	service := createTestService("mixed-service", []servicePort{
		{name: "http", port: 80, targetPort: intstr.FromInt(8080), protocol: "TCP"},
		{name: "https", port: 443, targetPort: intstr.IntOrString{}, protocol: "TCP"},       // Unset
		{name: "dns", port: 53, targetPort: intstr.FromString("dns-port"), protocol: "UDP"}, // Named
	})

	config := ExtractInboundConfigFromService(service)

	assert.NotNil(t, config)
	assert.Len(t, config.FrontendPorts, 3)
	assert.Len(t, config.BackendPorts, 3)

	// HTTP: should use TargetPort
	assert.Equal(t, int32(80), config.FrontendPorts[0].Port)
	assert.Equal(t, int32(8080), config.BackendPorts[0].Port)

	// HTTPS: should fall back to Port
	assert.Equal(t, int32(443), config.FrontendPorts[1].Port)
	assert.Equal(t, int32(443), config.BackendPorts[1].Port)

	// DNS: named port should fall back to Port
	assert.Equal(t, int32(53), config.FrontendPorts[2].Port)
	assert.Equal(t, int32(53), config.BackendPorts[2].Port)
	assert.Equal(t, "UDP", config.FrontendPorts[2].Protocol)
}

func TestBuildInboundServiceResources_MismatchedPortCounts(t *testing.T) {
	// Frontend has more ports than backend (edge case)
	config := &InboundConfig{
		FrontendPorts: []PortMapping{
			{Port: 80, Protocol: "TCP"},
			{Port: 443, Protocol: "TCP"},
			{Port: 8080, Protocol: "TCP"},
		},
		BackendPorts: []PortMapping{
			{Port: 8000, Protocol: "TCP"},
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

	_, lb, _ := buildInboundServiceResources("test-service", config, dtConfig)

	// Should create 3 rules
	assert.Len(t, lb.Properties.LoadBalancingRules, 3)

	// First two should use backend ports
	assert.Equal(t, int32(80), *lb.Properties.LoadBalancingRules[0].Properties.FrontendPort)
	assert.Equal(t, int32(8000), *lb.Properties.LoadBalancingRules[0].Properties.BackendPort)

	assert.Equal(t, int32(443), *lb.Properties.LoadBalancingRules[1].Properties.FrontendPort)
	assert.Equal(t, int32(8443), *lb.Properties.LoadBalancingRules[1].Properties.BackendPort)

	// Third should fall back to frontend port
	assert.Equal(t, int32(8080), *lb.Properties.LoadBalancingRules[2].Properties.FrontendPort)
	assert.Equal(t, int32(8080), *lb.Properties.LoadBalancingRules[2].Properties.BackendPort)
}

func TestBuildInboundServiceResources_EmptyConfig(t *testing.T) {
	config := &InboundConfig{
		FrontendPorts: []PortMapping{},
		BackendPorts:  []PortMapping{},
	}

	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "test-sgw",
		ServiceGatewayID:           "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}

	_, lb, _ := buildInboundServiceResources("test-service", config, dtConfig)

	// Should create LB with no rules (empty config is valid)
	assert.Empty(t, lb.Properties.LoadBalancingRules)
	assert.Len(t, lb.Properties.BackendAddressPools, 1)
}

func TestBuildInboundServiceResources_LongServiceUID(t *testing.T) {
	// Test with very long service UID
	longUID := "very-long-service-uid-that-exceeds-normal-length-abcdef123456789012345678901234567890"

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

	pip, lb, _ := buildInboundServiceResources(longUID, config, dtConfig)

	// Should handle long UIDs without truncation
	assert.Equal(t, longUID, *lb.Name)
	assert.Equal(t, longUID+"-pip", *pip.Name)
	assert.Equal(t, longUID, *lb.Properties.BackendAddressPools[0].Name)
}

func TestBuildOutboundServiceResources_NilConfig(t *testing.T) {
	// OutboundConfig is currently not used but test nil handling
	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "westus",
		ServiceGatewayResourceName: "test-sgw",
		ServiceGatewayID:           "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}

	pip, natGw, servicesDTO := buildOutboundServiceResources("egress-123", nil, dtConfig)

	// Should create resources even with nil config
	assert.NotNil(t, pip)
	assert.NotNil(t, natGw)
	assert.NotNil(t, servicesDTO)
	assert.Equal(t, "egress-123-pip", *pip.Name)
	assert.Equal(t, "egress-123", *natGw.Name)
}

func TestBuildInboundServiceResources_MultipleConfigs(t *testing.T) {
	// Test that multiple invocations produce independent resources
	config1 := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 80, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8080, Protocol: "TCP"}},
	}

	config2 := &InboundConfig{
		FrontendPorts: []PortMapping{{Port: 443, Protocol: "TCP"}},
		BackendPorts:  []PortMapping{{Port: 8443, Protocol: "TCP"}},
	}

	dtConfig := Config{
		SubscriptionID:             "test-sub",
		ResourceGroup:              "test-rg",
		Location:                   "eastus",
		ServiceGatewayResourceName: "test-sgw",
		ServiceGatewayID:           "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.Network/serviceGateways/test-sgw",
	}

	pip1, lb1, _ := buildInboundServiceResources("service-1", config1, dtConfig)
	pip2, lb2, _ := buildInboundServiceResources("service-2", config2, dtConfig)

	// Should produce different resources
	assert.NotEqual(t, *pip1.Name, *pip2.Name)
	assert.NotEqual(t, *lb1.Name, *lb2.Name)

	// Each should have correct config
	assert.Equal(t, int32(80), *lb1.Properties.LoadBalancingRules[0].Properties.FrontendPort)
	assert.Equal(t, int32(443), *lb2.Properties.LoadBalancingRules[0].Properties.FrontendPort)
}

func TestConfigValidation_AllFieldsRequired(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		expectError bool
	}{
		{
			name: "valid config",
			config: Config{
				SubscriptionID:             "sub-123",
				ResourceGroup:              "rg-456",
				Location:                   "eastus",
				VNetName:                   "test-vnet",
				ServiceGatewayResourceName: "sgw-789",
				ServiceGatewayID:           "/subscriptions/sub-123/resourceGroups/rg-456/providers/Microsoft.Network/serviceGateways/sgw-789",
			},
			expectError: false,
		},
		{
			name: "missing subscription",
			config: Config{
				ResourceGroup:              "rg-456",
				Location:                   "eastus",
				ServiceGatewayResourceName: "sgw-789",
				ServiceGatewayID:           "/subscriptions/sub-123/resourceGroups/rg-456/providers/Microsoft.Network/serviceGateways/sgw-789",
			},
			expectError: true,
		},
		{
			name: "missing resource group",
			config: Config{
				SubscriptionID:             "sub-123",
				Location:                   "eastus",
				ServiceGatewayResourceName: "sgw-789",
				ServiceGatewayID:           "/subscriptions/sub-123/resourceGroups/rg-456/providers/Microsoft.Network/serviceGateways/sgw-789",
			},
			expectError: true,
		},
		{
			name: "missing location",
			config: Config{
				SubscriptionID:             "sub-123",
				ResourceGroup:              "rg-456",
				ServiceGatewayResourceName: "sgw-789",
				ServiceGatewayID:           "/subscriptions/sub-123/resourceGroups/rg-456/providers/Microsoft.Network/serviceGateways/sgw-789",
			},
			expectError: true,
		},
		{
			name: "missing ServiceGatewayResourceName",
			config: Config{
				SubscriptionID:   "sub-123",
				ResourceGroup:    "rg-456",
				Location:         "eastus",
				ServiceGatewayID: "/subscriptions/sub-123/resourceGroups/rg-456/providers/Microsoft.Network/serviceGateways/sgw-789",
			},
			expectError: true,
		},
		{
			name: "missing ServiceGatewayID",
			config: Config{
				SubscriptionID:             "sub-123",
				ResourceGroup:              "rg-456",
				Location:                   "eastus",
				ServiceGatewayResourceName: "sgw-789",
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestNewIgnoreCaseSetFromSlice_PreservesOrder(t *testing.T) {
	// Order shouldn't matter for set membership
	items1 := []string{"a", "b", "c"}
	items2 := []string{"c", "b", "a"}

	set1 := newIgnoreCaseSetFromSlice(items1)
	set2 := newIgnoreCaseSetFromSlice(items2)

	// Both should contain same elements
	assert.Equal(t, set1.Len(), set2.Len())
	for _, item := range items1 {
		assert.True(t, set1.Has(item))
		assert.True(t, set2.Has(item))
	}
}

// Helper types and functions for tests

type servicePort struct {
	name       string
	port       int32
	targetPort intstr.IntOrString
	protocol   string
}

func createTestService(name string, ports []servicePort) *v1.Service {
	v1Ports := make([]v1.ServicePort, len(ports))
	for i, p := range ports {
		protocol := v1.ProtocolTCP
		if p.protocol == "UDP" {
			protocol = v1.ProtocolUDP
		}
		v1Ports[i] = v1.ServicePort{
			Name:       p.name,
			Port:       p.port,
			TargetPort: p.targetPort,
			Protocol:   protocol,
		}
	}

	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: v1.ServiceSpec{
			Ports: v1Ports,
		},
	}
}
