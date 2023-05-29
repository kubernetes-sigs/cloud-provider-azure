/*
Copyright 2023 The Kubernetes Authors.

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

package azclient

import (
	"time"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/ratelimit"
)

type Config struct {
}

type RestClientConfig struct {
	PollingDelay  *time.Duration
	RetryAttempts *int
	RetryDuration *time.Duration
}

// ClientConfig contains all essential information to create an Azure client.
type ClientConfig struct {
	RateLimitConfig  *ratelimit.RateLimitConfig
	RestClientConfig RestClientConfig
	UserAgent        string
}

// CloudProviderRateLimitConfig indicates the rate limit config for each clients.
type CloudProviderRateLimitConfig struct {
	// The default rate limit config options.
	ratelimit.RateLimitConfig

	// Rate limit config for each clients. Values would override default settings above.
	RouteRateLimit                  *ratelimit.RateLimitConfig `json:"routeRateLimit,omitempty" yaml:"routeRateLimit,omitempty"`
	SubnetsRateLimit                *ratelimit.RateLimitConfig `json:"subnetsRateLimit,omitempty" yaml:"subnetsRateLimit,omitempty"`
	InterfaceRateLimit              *ratelimit.RateLimitConfig `json:"interfaceRateLimit,omitempty" yaml:"interfaceRateLimit,omitempty"`
	RouteTableRateLimit             *ratelimit.RateLimitConfig `json:"routeTableRateLimit,omitempty" yaml:"routeTableRateLimit,omitempty"`
	LoadBalancerRateLimit           *ratelimit.RateLimitConfig `json:"loadBalancerRateLimit,omitempty" yaml:"loadBalancerRateLimit,omitempty"`
	PublicIPAddressRateLimit        *ratelimit.RateLimitConfig `json:"publicIPAddressRateLimit,omitempty" yaml:"publicIPAddressRateLimit,omitempty"`
	SecurityGroupRateLimit          *ratelimit.RateLimitConfig `json:"securityGroupRateLimit,omitempty" yaml:"securityGroupRateLimit,omitempty"`
	VirtualMachineRateLimit         *ratelimit.RateLimitConfig `json:"virtualMachineRateLimit,omitempty" yaml:"virtualMachineRateLimit,omitempty"`
	StorageAccountRateLimit         *ratelimit.RateLimitConfig `json:"storageAccountRateLimit,omitempty" yaml:"storageAccountRateLimit,omitempty"`
	DiskRateLimit                   *ratelimit.RateLimitConfig `json:"diskRateLimit,omitempty" yaml:"diskRateLimit,omitempty"`
	SnapshotRateLimit               *ratelimit.RateLimitConfig `json:"snapshotRateLimit,omitempty" yaml:"snapshotRateLimit,omitempty"`
	VirtualMachineScaleSetRateLimit *ratelimit.RateLimitConfig `json:"virtualMachineScaleSetRateLimit,omitempty" yaml:"virtualMachineScaleSetRateLimit,omitempty"`
	VirtualMachineSizeRateLimit     *ratelimit.RateLimitConfig `json:"virtualMachineSizesRateLimit,omitempty" yaml:"virtualMachineSizesRateLimit,omitempty"`
	AvailabilitySetRateLimit        *ratelimit.RateLimitConfig `json:"availabilitySetRateLimit,omitempty" yaml:"availabilitySetRateLimit,omitempty"`
	AttachDetachDiskRateLimit       *ratelimit.RateLimitConfig `json:"attachDetachDiskRateLimit,omitempty" yaml:"attachDetachDiskRateLimit,omitempty"`
	ContainerServiceRateLimit       *ratelimit.RateLimitConfig `json:"containerServiceRateLimit,omitempty" yaml:"containerServiceRateLimit,omitempty"`
	DeploymentRateLimit             *ratelimit.RateLimitConfig `json:"deploymentRateLimit,omitempty" yaml:"deploymentRateLimit,omitempty"`
	PrivateDNSRateLimit             *ratelimit.RateLimitConfig `json:"privateDNSRateLimit,omitempty" yaml:"privateDNSRateLimit,omitempty"`
	PrivateDNSZoneGroupRateLimit    *ratelimit.RateLimitConfig `json:"privateDNSZoneGroupRateLimit,omitempty" yaml:"privateDNSZoneGroupRateLimit,omitempty"`
	PrivateEndpointRateLimit        *ratelimit.RateLimitConfig `json:"privateEndpointRateLimit,omitempty" yaml:"privateEndpointRateLimit,omitempty"`
	PrivateLinkServiceRateLimit     *ratelimit.RateLimitConfig `json:"privateLinkServiceRateLimit,omitempty" yaml:"privateLinkServiceRateLimit,omitempty"`
	VirtualNetworkRateLimit         *ratelimit.RateLimitConfig `json:"virtualNetworkRateLimit,omitempty" yaml:"virtualNetworkRateLimit,omitempty"`
}

type ClientFactoryConfig struct {
	CloudProviderRateLimitConfig
	// Backoff retry limit
	CloudProviderBackoffRetries int `json:"cloudProviderBackoffRetries,omitempty" yaml:"cloudProviderBackoffRetries,omitempty"`
	// Backoff duration
	CloudProviderBackoffDuration int `json:"cloudProviderBackoffDuration,omitempty" yaml:"cloudProviderBackoffDuration,omitempty"`
}
