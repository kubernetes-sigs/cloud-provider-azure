package difftracker

import (
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// buildInboundServiceResources constructs the PIP, LoadBalancer, and ServicesDTO for an inbound service
// Returns the resources ready to be created via createOrUpdatePIP/createOrUpdateLB/updateNRPSGWServices
func buildInboundServiceResources(serviceUID string, config *InboundConfig, dtConfig Config) (
	pip armnetwork.PublicIPAddress,
	lb armnetwork.LoadBalancer,
	servicesDTO ServicesDataDTO,
) {
	pipName := fmt.Sprintf("%s-pip", serviceUID)

	// Build Public IP
	pip = armnetwork.PublicIPAddress{
		Name: to.Ptr(pipName),
		ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
			dtConfig.SubscriptionID, dtConfig.ResourceGroup, pipName)),
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandardV2),
		},
		Location: to.Ptr(dtConfig.Location),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
	}

	// Build LoadBalancer with backend pool and rules
	backendPoolName := serviceUID
	frontendIPConfigID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/%s/frontendIPConfigurations/frontend",
		dtConfig.SubscriptionID, dtConfig.ResourceGroup, serviceUID)
	backendPoolID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/%s/backendAddressPools/%s",
		dtConfig.SubscriptionID, dtConfig.ResourceGroup, serviceUID, backendPoolName)

	// Build backend pool
	backendPools := []*armnetwork.BackendAddressPool{
		{
			Name:       to.Ptr(backendPoolName),
			Properties: &armnetwork.BackendAddressPoolPropertiesFormat{
				// Backend pool will be populated by ServiceGateway with pod IPs
			},
		},
	}

	// Build LB rules and probes from config
	var lbRules []*armnetwork.LoadBalancingRule
	var probes []*armnetwork.Probe

	if config != nil && len(config.FrontendPorts) > 0 {
		// For CLB with PodIP backend pool, we disable floating IP and don't create health probes
		// Traffic goes directly to pod IPs on the backend port
		for i, frontendPort := range config.FrontendPorts {
			backendPort := frontendPort.Port
			if i < len(config.BackendPorts) {
				backendPort = config.BackendPorts[i].Port
			}

			protocol := armnetwork.TransportProtocolTCP
			if frontendPort.Protocol == "UDP" {
				protocol = armnetwork.TransportProtocolUDP
			}

			ruleName := fmt.Sprintf("rule-%s-%d", strings.ToLower(frontendPort.Protocol), frontendPort.Port)

			lbRules = append(lbRules, &armnetwork.LoadBalancingRule{
				Name: to.Ptr(ruleName),
				Properties: &armnetwork.LoadBalancingRulePropertiesFormat{
					Protocol:             to.Ptr(protocol),
					FrontendPort:         to.Ptr(frontendPort.Port),
					BackendPort:          to.Ptr(backendPort),
					EnableFloatingIP:     to.Ptr(false), // Disabled for PodIP backend
					IdleTimeoutInMinutes: to.Ptr(int32(4)),
					EnableTCPReset:       to.Ptr(true),
					FrontendIPConfiguration: &armnetwork.SubResource{
						ID: to.Ptr(frontendIPConfigID),
					},
					BackendAddressPool: &armnetwork.SubResource{
						ID: to.Ptr(backendPoolID),
					},
					// No probe for PodIP backend pools
				},
			})

			klog.V(4).Infof("buildInboundServiceResources: created LB rule %s: frontend=%d backend=%d protocol=%s for service %s",
				ruleName, frontendPort.Port, backendPort, frontendPort.Protocol, serviceUID)
		}
	} else {
		klog.V(2).Infof("buildInboundServiceResources: no port configuration provided for service %s, creating LB without rules", serviceUID)
	}

	lb = armnetwork.LoadBalancer{
		Name:     to.Ptr(serviceUID),
		ID:       to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/%s", dtConfig.SubscriptionID, dtConfig.ResourceGroup, serviceUID)),
		Location: to.Ptr(dtConfig.Location),
		SKU: &armnetwork.LoadBalancerSKU{
			Name: to.Ptr(armnetwork.LoadBalancerSKUNameService),
		},
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			FrontendIPConfigurations: []*armnetwork.FrontendIPConfiguration{
				{
					Name: to.Ptr("frontend"),
					Properties: &armnetwork.FrontendIPConfigurationPropertiesFormat{
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
								dtConfig.SubscriptionID, dtConfig.ResourceGroup, pipName)),
						},
					},
				},
			},
			BackendAddressPools: backendPools,
			LoadBalancingRules:  lbRules,
			Probes:              probes,
		},
	}

	// Build ServicesDTO for ServiceGateway registration
	servicesDTO = MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
		SyncServicesReturnType{
			Additions: newIgnoreCaseSetFromSlice([]string{serviceUID}),
			Removals:  nil,
		},
		SyncServicesReturnType{
			Additions: nil,
			Removals:  nil,
		},
		dtConfig.SubscriptionID,
		dtConfig.ResourceGroup,
	)

	return pip, lb, servicesDTO
}

// buildOutboundServiceResources constructs the PIP, NAT Gateway, and ServicesDTO for an outbound service
// Returns the resources ready to be created via createOrUpdatePIP/createOrUpdateNatGateway/updateNRPSGWServices
func buildOutboundServiceResources(serviceUID string, config *OutboundConfig, dtConfig Config) (
	pip armnetwork.PublicIPAddress,
	natGateway armnetwork.NatGateway,
	servicesDTO ServicesDataDTO,
) {
	pipName := fmt.Sprintf("%s-pip", serviceUID)

	// Build Public IP
	pip = armnetwork.PublicIPAddress{
		Name: to.Ptr(pipName),
		ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
			dtConfig.SubscriptionID, dtConfig.ResourceGroup, pipName)),
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandardV2),
		},
		Location: to.Ptr(dtConfig.Location),
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
	}

	// Build NAT Gateway
	natGateway = armnetwork.NatGateway{
		Name: to.Ptr(serviceUID),
		ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/natGateways/%s",
			dtConfig.SubscriptionID, dtConfig.ResourceGroup, serviceUID)),
		SKU: &armnetwork.NatGatewaySKU{
			Name: to.Ptr(armnetwork.NatGatewaySKUNameStandardV2),
		},
		Location: to.Ptr(dtConfig.Location),
		Properties: &armnetwork.NatGatewayPropertiesFormat{
			ServiceGateway: &armnetwork.ServiceGateway{
				ID: to.Ptr(dtConfig.ServiceGatewayID),
			},
			PublicIPAddresses: []*armnetwork.SubResource{
				{
					ID: to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/publicIPAddresses/%s",
						dtConfig.SubscriptionID, dtConfig.ResourceGroup, pipName)),
				},
			},
		},
	}

	// Build ServicesDTO for ServiceGateway registration
	servicesDTO = MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
		SyncServicesReturnType{
			Additions: nil,
			Removals:  nil,
		},
		SyncServicesReturnType{
			Additions: newIgnoreCaseSetFromSlice([]string{serviceUID}),
			Removals:  nil,
		},
		dtConfig.SubscriptionID,
		dtConfig.ResourceGroup,
	)

	return pip, natGateway, servicesDTO
}

// newIgnoreCaseSetFromSlice creates an IgnoreCaseSet from a slice of strings
func newIgnoreCaseSetFromSlice(items []string) *utilsets.IgnoreCaseSet {
	set := utilsets.NewString()
	for _, item := range items {
		set.Insert(item)
	}
	return set
}

// ExtractInboundConfigFromService creates InboundConfig from a Kubernetes Service
// This is shared between initialization and the provider layer
func ExtractInboundConfigFromService(service *v1.Service) *InboundConfig {
	if service == nil || len(service.Spec.Ports) == 0 {
		return nil
	}

	config := &InboundConfig{
		FrontendPorts: make([]PortMapping, 0, len(service.Spec.Ports)),
		BackendPorts:  make([]PortMapping, 0, len(service.Spec.Ports)),
	}

	// Extract port mappings from service
	for _, port := range service.Spec.Ports {
		protocol := string(port.Protocol)
		if protocol == "" {
			protocol = "TCP"
		}

		// Frontend port (service port)
		config.FrontendPorts = append(config.FrontendPorts, PortMapping{
			Port:     port.Port,
			Protocol: protocol,
		})

		// Backend port (target port)
		// For CLB with PodIP backend, we use TargetPort
		// If TargetPort is not specified, default to Port
		backendPort := port.Port
		if port.TargetPort.Type == 0 && port.TargetPort.IntVal > 0 { // intstr.Int
			backendPort = port.TargetPort.IntVal
		} else if port.TargetPort.Type == 1 { // intstr.String
			// Named ports not supported in CLB mode - use Port as fallback
			serviceName := fmt.Sprintf("%s/%s", service.Namespace, service.Name)
			klog.V(2).Infof("Named targetPort %s not supported in CLB mode for service %s, using Port %d",
				port.TargetPort.StrVal, serviceName, port.Port)
			backendPort = port.Port
		}

		config.BackendPorts = append(config.BackendPorts, PortMapping{
			Port:     backendPort,
			Protocol: protocol,
		})
	}

	return config
}
