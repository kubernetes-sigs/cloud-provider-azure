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
	"strings"
	"sync"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/diskclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/ratelimit"
)

type ClientFactory struct {
	*ClientFactoryConfig
	diskClientRegistry sync.Map
	*AuthProvider
}

func NewClientFactory(config *ClientFactoryConfig, authProvider *AuthProvider) *ClientFactory {
	return &ClientFactory{
		ClientFactoryConfig: config,
		AuthProvider:        authProvider,
	}
}

func (factory *ClientFactory) GetDiskClient(subscription string) (diskclient.Interface, error) {
	subID := strings.ToLower(subscription)

	options, err := factory.GetDefaultResourceClientOption()
	if err != nil {
		return nil, err
	}
	//add ratelimit policy
	ratelimitOption := factory.ClientFactoryConfig.GetRateLimitConfig("diskRateLimit")
	rateLimitPolicy := ratelimit.NewRateLimitPolicy(ratelimitOption)
	options.ClientOptions.PerCallPolicies = append(options.ClientOptions.PerCallPolicies, rateLimitPolicy)

	cred, err := factory.AuthProvider.GetAzIdentity()
	if err != nil {
		return nil, err
	}
	defaultClient, err := diskclient.New(subscription, cred, options)
	if err != nil {
		return nil, err
	}
	client, _ := factory.diskClientRegistry.LoadOrStore(subID, &defaultClient)

	return *client.(*diskclient.Interface), nil
}
