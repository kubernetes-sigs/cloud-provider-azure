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
	"fmt"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

const (
	publicIPNameSuffix    = "-pip"
	publicIPNameMaxLength = 80
)

// egressIdentityRegex matches a valid (lowercased) Azure NAT Gateway resource name and reserves
// enough room for publicIPNameSuffix within Azure's Public IP resource-name limit.
var egressIdentityRegex = regexp.MustCompile(fmt.Sprintf(
	`^[a-z0-9]([a-z0-9._-]{0,%d}[a-z0-9_])?$`,
	publicIPNameMaxLength-len(publicIPNameSuffix)-2,
))

// ServiceUID returns the canonical ServiceGateway identity for a Kubernetes Service.
func ServiceUID(service *v1.Service) string {
	if service == nil {
		return ""
	}
	return strings.ToLower(string(service.UID))
}

// PublicIPName returns the Public IP resource name associated with a ServiceGateway identity.
func PublicIPName(identity string) string {
	return identity + publicIPNameSuffix
}

// IsValidEgressIdentity reports whether name is a safe egress identity. The identity is derived from
// a user-controllable pod label and is used verbatim as the NAT Gateway name and as the stem of the
// Public IP name (identity+"-pip"), and is interpolated into their ARM resource IDs, so it must be
// validated to avoid malformed ARM requests (for example a path-traversal value like "../foo") or a
// PIP name that overflows Azure's 80-char limit, either of which causes endless create retries.
func IsValidEgressIdentity(name string) bool {
	return egressIdentityRegex.MatchString(name)
}

// buildInboundServiceResources constructs the PIP, LoadBalancer, and ServicesDTO for an inbound service
// Returns the resources ready to be created via createOrUpdatePIP/createOrUpdateLB/updateNRPSGWServices
func buildInboundServiceResources(serviceUID string, config *InboundConfig, dtConfig Config) (
	pip armnetwork.PublicIPAddress,
	lb armnetwork.LoadBalancer,
	servicesDTO ServicesDataDTO,
	err error,
) {
	pipName := PublicIPName(serviceUID)

	// Resolve the Public IP version from the Service's IP families. Dual-stack is not
	// supported for PodIP backend pools, so reject it here; the create path classifies this
	// build error as terminal (the Service is parked until its spec changes) rather than
	// silently provisioning a single-family LB. Default to IPv4 when unspecified.
	pipVersion := armnetwork.IPVersionIPv4
	if config != nil {
		if len(config.NamedTargetPorts) > 0 {
			return pip, lb, servicesDTO, fmt.Errorf("buildInboundServiceResources: named targetPort(s) %v are not supported for PodIP backend pools (service %s); use a numeric targetPort", config.NamedTargetPorts, serviceUID)
		}
		if len(config.IPFamilies) > 1 {
			return pip, lb, servicesDTO, fmt.Errorf("buildInboundServiceResources: dual-stack services are not supported for PodIP backend pools (service %s, ipFamilies=%v)", serviceUID, config.IPFamilies)
		}
		if len(config.IPFamilies) == 1 {
			switch {
			case strings.EqualFold(config.IPFamilies[0], string(armnetwork.IPVersionIPv6)):
				pipVersion = armnetwork.IPVersionIPv6
			case strings.EqualFold(config.IPFamilies[0], string(armnetwork.IPVersionIPv4)):
				pipVersion = armnetwork.IPVersionIPv4
			default:
				return pip, lb, servicesDTO, fmt.Errorf("buildInboundServiceResources: unknown IP family %q for service %s", config.IPFamilies[0], serviceUID)
			}
		}
	}

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
			PublicIPAddressVersion:   to.Ptr(pipVersion),
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

	// Build LB rules from config. No health probes are created: PodIP backend pools
	// don't support them, so the LB's Probes field is left nil.
	var lbRules []*armnetwork.LoadBalancingRule

	if config != nil && len(config.FrontendPorts) > 0 {
		idleTimeout := int32(4)
		if config.IdleTimeoutMinutes != nil {
			idleTimeout = *config.IdleTimeoutMinutes
			if idleTimeout < 4 || idleTimeout > 30 {
				return pip, lb, servicesDTO, fmt.Errorf("buildInboundServiceResources: idle timeout %d out of range (4-30) for service %s", idleTimeout, serviceUID)
			}
		}
		// Azure rejects two load-balancing rules that share the same protocol and backend
		// port on the same backend pool when floating IP is disabled (always the case for
		// PodIP backends): RulesUseSameBackendPortProtocolAndPool. Detect that collision here
		// and fail the build terminally (as with named/dual-stack above) so the engine parks
		// the service instead of retrying an Azure PUT that can never succeed.
		seenBackend := make(map[string]int, len(config.FrontendPorts))
		for i, frontendPort := range config.FrontendPorts {
			backendPort := frontendPort.Port
			if i < len(config.BackendPorts) {
				backendPort = config.BackendPorts[i].Port
			}

			if frontendPort.Port < 1 || frontendPort.Port > 65534 {
				return pip, lb, servicesDTO, fmt.Errorf("buildInboundServiceResources: frontend port %d out of range (1-65534) for service %s", frontendPort.Port, serviceUID)
			}
			if backendPort < 1 || backendPort > 65535 {
				return pip, lb, servicesDTO, fmt.Errorf("buildInboundServiceResources: backend port %d out of range (1-65535) for service %s", backendPort, serviceUID)
			}

			var protocol armnetwork.TransportProtocol
			switch {
			case strings.EqualFold(frontendPort.Protocol, "TCP"):
				protocol = armnetwork.TransportProtocolTCP
			case strings.EqualFold(frontendPort.Protocol, "UDP"):
				protocol = armnetwork.TransportProtocolUDP
			default:
				return pip, lb, servicesDTO, fmt.Errorf("buildInboundServiceResources: unsupported protocol %q for service %s", frontendPort.Protocol, serviceUID)
			}

			backendKey := fmt.Sprintf("%s/%d", protocol, backendPort)
			if prev, ok := seenBackend[backendKey]; ok {
				return pip, lb, servicesDTO, fmt.Errorf("buildInboundServiceResources: service %s frontend ports %d and %d both map to backend port %d over %s; Azure rejects load-balancing rules that share a backend port and protocol on the same pool with floating IP disabled (RulesUseSameBackendPortProtocolAndPool), so distinct service ports must use distinct targetPorts", serviceUID, config.FrontendPorts[prev].Port, frontendPort.Port, backendPort, protocol)
			}
			seenBackend[backendKey] = i

			ruleName := fmt.Sprintf("rule-%s-%d", strings.ToLower(frontendPort.Protocol), frontendPort.Port)

			ruleProps := &armnetwork.LoadBalancingRulePropertiesFormat{
				Protocol:             to.Ptr(protocol),
				FrontendPort:         to.Ptr(frontendPort.Port),
				BackendPort:          to.Ptr(backendPort),
				EnableFloatingIP:     to.Ptr(false), // Disabled for PodIP backend
				IdleTimeoutInMinutes: to.Ptr(idleTimeout),
				FrontendIPConfiguration: &armnetwork.SubResource{
					ID: to.Ptr(frontendIPConfigID),
				},
				BackendAddressPool: &armnetwork.SubResource{
					ID: to.Ptr(backendPoolID),
				},
				// No probe for PodIP backend pools
			}
			if protocol == armnetwork.TransportProtocolTCP {
				ruleProps.EnableTCPReset = to.Ptr(true)
			}

			lbRules = append(lbRules, &armnetwork.LoadBalancingRule{
				Name:       to.Ptr(ruleName),
				Properties: ruleProps,
			})

			log.Background().WithName("difftracker").V(5).Info("Created LB rule",
				"rule", ruleName, "frontendPort", frontendPort.Port, "backendPort", backendPort, "protocol", frontendPort.Protocol, "service", serviceUID)
		}
	} else {
		log.Background().WithName("difftracker").V(5).Info("No port configuration provided, creating LB without rules", "service", serviceUID)
	}

	lb = armnetwork.LoadBalancer{
		Name:     to.Ptr(serviceUID),
		ID:       to.Ptr(fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/%s", dtConfig.SubscriptionID, dtConfig.ResourceGroup, serviceUID)),
		Location: to.Ptr(dtConfig.Location),
		SKU: &armnetwork.LoadBalancerSKU{
			Name: to.Ptr(armnetwork.LoadBalancerSKUName(consts.LoadBalancerARMSKUService)),
		},
		Properties: &armnetwork.LoadBalancerPropertiesFormat{
			Scope: to.Ptr(armnetwork.LoadBalancerScopePublic),
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

	return pip, lb, servicesDTO, nil
}

// buildOutboundServiceResources constructs the PIP, NAT Gateway, and ServicesDTO for an outbound service
// Returns the resources ready to be created via createOrUpdatePIP/createOrUpdateNatGateway/updateNRPSGWServices
func buildOutboundServiceResources(serviceUID string, config *OutboundConfig, dtConfig Config) (
	pip armnetwork.PublicIPAddress,
	natGateway armnetwork.NatGateway,
	servicesDTO ServicesDataDTO,
) {
	pipName := PublicIPName(serviceUID)

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
			ServiceGateway: &armnetwork.SubResource{
				ID: to.Ptr(dtConfig.ServiceGatewayResourceID()),
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
		// For SLB with PodIP backend, we use TargetPort
		// If TargetPort is not specified, default to Port
		backendPort := port.Port
		switch port.TargetPort.Type {
		case intstr.Int:
			if port.TargetPort.IntVal > 0 {
				backendPort = port.TargetPort.IntVal
			}
		case intstr.String:
			// A named targetPort cannot be resolved to a concrete PodIP backend port here.
			// Record it so buildInboundServiceResources rejects the service with a terminal
			// error instead of silently sending traffic to the wrong backend port. The
			// backendPort value below is a placeholder; it is never programmed because the
			// build fails first.
			log.Background().WithName("difftracker").V(4).Info("Named targetPort is not supported, service will be rejected until it uses a numeric targetPort",
				"targetPort", port.TargetPort.StrVal, "namespace", service.Namespace, "service", service.Name)
			config.NamedTargetPorts = append(config.NamedTargetPorts, port.TargetPort.StrVal)
			backendPort = port.Port
		}

		config.BackendPorts = append(config.BackendPorts, PortMapping{
			Port:     backendPort,
			Protocol: protocol,
		})
	}

	// Capture the Service's IP families so the LB Public IP is created with the correct
	// version (single-stack) and dual-stack can be rejected at build time. Stored as the
	// raw v1.IPFamily strings ("IPv4"/"IPv6").
	if len(service.Spec.IPFamilies) > 0 {
		config.IPFamilies = make([]string, 0, len(service.Spec.IPFamilies))
		for _, fam := range service.Spec.IPFamilies {
			config.IPFamilies = append(config.IPFamilies, string(fam))
		}
	}

	return config
}

// InboundConfigValidationError reports an inbound Service spec the PodIP backend pool cannot
// support. Reason is the Kubernetes Event reason the caller surfaces on the Service.
type InboundConfigValidationError struct {
	Reason  string
	Message string
}

func (e *InboundConfigValidationError) Error() string { return e.Message }

// WarningEventError describes an error that the provider should surface as a Kubernetes warning Event.
type WarningEventError interface {
	error
	WarningEvent() (reason, message string)
}

// WarningEvent returns the Kubernetes Event metadata for this validation error.
func (e *InboundConfigValidationError) WarningEvent() (reason, message string) {
	return e.Reason, e.Message
}

// ValidateInboundConfig rejects inbound Service specs the PodIP backend pool cannot support,
// returning an *InboundConfigValidationError whose Reason is the Event reason to surface. Without
// this fast check the engine would terminal-park the create asynchronously with no visible cause.
// A nil config (a port-less Service) is treated as valid; the caller handles that case separately.
func ValidateInboundConfig(config *InboundConfig) error {
	if config == nil {
		return nil
	}

	// ipFamilies has at most two entries (two == dual-stack); PodIP backends are single-stack.
	if len(config.IPFamilies) > 1 {
		return &InboundConfigValidationError{
			Reason:  "UnsupportedDualStack",
			Message: fmt.Sprintf("dual-stack Services are not supported when ServiceGateway is enabled (ipFamilies=%v); use a single-stack Service", config.IPFamilies),
		}
	}

	// A named targetPort cannot be resolved to a concrete PodIP backend port.
	if len(config.NamedTargetPorts) > 0 {
		return &InboundConfigValidationError{
			Reason:  "UnsupportedNamedTargetPort",
			Message: fmt.Sprintf("named targetPort(s) %v are not supported when ServiceGateway is enabled; use a numeric targetPort", config.NamedTargetPorts),
		}
	}

	// The PodIP backend only programs TCP and UDP load-balancing rules.
	for _, fp := range config.FrontendPorts {
		if !strings.EqualFold(fp.Protocol, "TCP") && !strings.EqualFold(fp.Protocol, "UDP") {
			return &InboundConfigValidationError{
				Reason:  "UnsupportedProtocol",
				Message: fmt.Sprintf("protocol %q is not supported when ServiceGateway is enabled (service port %d); use TCP or UDP", fp.Protocol, fp.Port),
			}
		}
	}

	// Two service ports mapping to the same (protocol, backend port) collide on the PodIP backend
	// pool (Azure's RulesUseSameBackendPortProtocolAndPool with floating IP disabled).
	seenBackend := make(map[string]int32, len(config.FrontendPorts))
	for i, fp := range config.FrontendPorts {
		backendPort := fp.Port
		if i < len(config.BackendPorts) {
			backendPort = config.BackendPorts[i].Port
		}
		key := fmt.Sprintf("%s/%d", strings.ToUpper(fp.Protocol), backendPort)
		if prevPort, ok := seenBackend[key]; ok {
			return &InboundConfigValidationError{
				Reason:  "UnsupportedBackendPortCollision",
				Message: fmt.Sprintf("service ports %d and %d both map to backend port %d over %s, which is not supported when ServiceGateway is enabled; distinct service ports must use distinct targetPorts", prevPort, fp.Port, backendPort, strings.ToUpper(fp.Protocol)),
			}
		}
		seenBackend[key] = fp.Port
	}

	return nil
}

// buildInboundResourceNames returns the resource names for an inbound service
func buildInboundResourceNames(serviceUID string) (lbName string, pipName string, backendPoolName string) {
	return serviceUID, PublicIPName(serviceUID), serviceUID
}

// buildOutboundResourceNames returns the resource names for an outbound service
func buildOutboundResourceNames(serviceUID string) (natGatewayName string, pipName string) {
	return serviceUID, PublicIPName(serviceUID)
}

// buildServiceGatewayRemovalDTO creates a ServicesDTO for removing a service from ServiceGateway
func buildServiceGatewayRemovalDTO(serviceUID string, isInbound bool, dtConfig Config) ServicesDataDTO {
	if isInbound {
		return MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
			SyncServicesReturnType{
				Additions: nil,
				Removals:  newIgnoreCaseSetFromSlice([]string{serviceUID}),
			},
			SyncServicesReturnType{
				Additions: nil,
				Removals:  nil,
			},
			dtConfig.SubscriptionID,
			dtConfig.ResourceGroup,
		)
	}
	return MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
		SyncServicesReturnType{
			Additions: nil,
			Removals:  nil,
		},
		SyncServicesReturnType{
			Additions: nil,
			Removals:  newIgnoreCaseSetFromSlice([]string{serviceUID}),
		},
		dtConfig.SubscriptionID,
		dtConfig.ResourceGroup,
	)
}
