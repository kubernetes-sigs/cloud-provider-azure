/*
Copyright 2020 The Kubernetes Authors.

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

package config

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"

	"github.com/Azure/go-autorest/autorest/azure"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/ratelimit"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

var (
	// ErrorNoAuth indicates that no credentials are provided.
	ErrorNoAuth = fmt.Errorf("no credentials provided for Azure cloud provider")
)

const (
	maxReadLength = 10 * 1 << 20 // 10MB
)

// AzureClientConfig holds azure client related part of cloud config
type AzureClientConfig struct {
	azclient.ARMClientConfig               `json:",inline" yaml:",inline"`
	azclient.AzureAuthConfig               `json:",inline" yaml:",inline"`
	ratelimit.CloudProviderRateLimitConfig `json:",inline" yaml:",inline"`
	CloudProviderCacheConfig               `json:",inline" yaml:",inline"`
	// Backoff retry limit
	CloudProviderBackoffRetries int `json:"cloudProviderBackoffRetries,omitempty" yaml:"cloudProviderBackoffRetries,omitempty"`
	// Backoff duration
	CloudProviderBackoffDuration int `json:"cloudProviderBackoffDuration,omitempty" yaml:"cloudProviderBackoffDuration,omitempty"`

	// The ID of the Azure Subscription that the cluster is deployed in
	SubscriptionID string `json:"subscriptionId,omitempty" yaml:"subscriptionId,omitempty"`
	// IdentitySystem indicates the identity provider. Relevant only to hybrid clouds (Azure Stack).
	// Allowed values are 'azure_ad' (default), 'adfs'.
	IdentitySystem string `json:"identitySystem,omitempty" yaml:"identitySystem,omitempty"`

	// The ID of the Azure Subscription that the network resources are deployed in
	NetworkResourceSubscriptionID string `json:"networkResourceSubscriptionID,omitempty" yaml:"networkResourceSubscriptionID,omitempty"`
}

// ParseAzureEnvironment returns the azure environment.
// If 'resourceManagerEndpoint' is set, the environment is computed by querying the cloud's resource manager endpoint.
// Otherwise, a pre-defined Environment is looked up by name.
func ParseAzureEnvironment(cloudName, resourceManagerEndpoint, identitySystem string) (*azure.Environment, error) {
	var env azure.Environment
	var err error
	if resourceManagerEndpoint != "" {
		klog.V(4).Infof("Loading environment from resource manager endpoint: %s", resourceManagerEndpoint)
		nameOverride := azure.OverrideProperty{Key: azure.EnvironmentName, Value: cloudName}
		env, err = azure.EnvironmentFromURL(resourceManagerEndpoint, nameOverride)
		if err == nil {
			azureStackOverrides(&env, resourceManagerEndpoint, identitySystem)
		}
	} else if cloudName == "" {
		klog.V(4).Info("Using public cloud environment")
		env = azure.PublicCloud
	} else {
		klog.V(4).Infof("Using %s environment", cloudName)
		env, err = azure.EnvironmentFromName(cloudName)
	}
	return &env, err
}

// ParseAzureAuthConfig returns a parsed configuration for an Azure cloudprovider config file
func ParseAzureAuthConfig(configReader io.Reader) (*AzureClientConfig, *azure.Environment, error) {
	var config AzureClientConfig

	if configReader == nil {
		return nil, nil, errors.New("nil config is provided")
	}

	limitedReader := &io.LimitedReader{R: configReader, N: maxReadLength}
	configContents, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, nil, err
	}
	if limitedReader.N <= 0 {
		return nil, nil, errors.New("the read limit is reached")
	}
	err = yaml.Unmarshal(configContents, &config)
	if err != nil {
		return nil, nil, err
	}

	environment, err := ParseAzureEnvironment(config.Cloud, config.ResourceManagerEndpoint, config.IdentitySystem)
	if err != nil {
		return nil, nil, err
	}

	return &config, environment, nil
}

// UsesNetworkResourceInDifferentTenant determines whether the AzureAuthConfig indicates to use network resources in
// different AAD Tenant than those for the cluster. Return true when NetworkResourceTenantID is specified  and not equal
// to one defined in global configs
func (config *AzureClientConfig) UsesNetworkResourceInDifferentTenant() bool {
	return len(config.NetworkResourceTenantID) > 0 && !strings.EqualFold(config.NetworkResourceTenantID, config.TenantID)
}

// UsesNetworkResourceInDifferentSubscription determines whether the AzureAuthConfig indicates to use network resources
// in different Subscription than those for the cluster. Return true when NetworkResourceSubscriptionID is specified
// and not equal to one defined in global configs
func (config *AzureClientConfig) UsesNetworkResourceInDifferentSubscription() bool {
	return len(config.NetworkResourceSubscriptionID) > 0 && !strings.EqualFold(config.NetworkResourceSubscriptionID, config.SubscriptionID)
}

// azureStackOverrides ensures that the Environment matches what AKSe currently generates for Azure Stack
func azureStackOverrides(env *azure.Environment, resourceManagerEndpoint, identitySystem string) {
	env.ManagementPortalURL = strings.Replace(resourceManagerEndpoint, "https://management.", "https://portal.", -1)
	env.ServiceManagementEndpoint = env.TokenAudience
	env.ResourceManagerVMDNSSuffix = strings.Replace(resourceManagerEndpoint, "https://management.", "cloudapp.", -1)
	env.ResourceManagerVMDNSSuffix = strings.TrimSuffix(env.ResourceManagerVMDNSSuffix, "/")
	if strings.EqualFold(identitySystem, consts.ADFSIdentitySystem) {
		env.ActiveDirectoryEndpoint = strings.TrimSuffix(env.ActiveDirectoryEndpoint, "/")
		env.ActiveDirectoryEndpoint = strings.TrimSuffix(env.ActiveDirectoryEndpoint, "adfs")
	}
}

// ValidateForMultiTenant checks configuration for the scenario of using network resource in different tenant
func (config *AzureClientConfig) ValidateForMultiTenant() error {
	if !config.UsesNetworkResourceInDifferentTenant() {
		return fmt.Errorf("NetworkResourceTenantID must be configured")
	}

	if strings.EqualFold(config.IdentitySystem, consts.ADFSIdentitySystem) {
		return fmt.Errorf("ADFS identity system is not supported")
	}

	return nil
}
