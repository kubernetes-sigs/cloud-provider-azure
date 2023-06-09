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
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm/policy"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/ratelimit"
)

type ClientFactoryConfig struct {
	ratelimit.CloudProviderRateLimitConfig
	*ARMClientConfig

	// Enable exponential backoff to manage resource request retries
	CloudProviderBackoff bool `json:"cloudProviderBackoff,omitempty" yaml:"cloudProviderBackoff,omitempty"`
}

func GetDefaultResourceClientOption(config *ClientFactoryConfig) (*policy.ClientOptions, error) {
	var armConfig *ARMClientConfig
	if config != nil {
		armConfig = config.ARMClientConfig
	}
	//Get default settings
	options, err := NewClientOptionFromARMClientConfig(armConfig)
	if err != nil {
		return nil, err
	}
	if config != nil {
		// add retry policy
		if !config.CloudProviderBackoff {
			options.ClientOptions.Retry.MaxRetries = 1
		}
	}
	return options, err
}
