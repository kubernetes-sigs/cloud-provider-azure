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

package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

var (
	// newARMClientFactory is a function that returns a new ARM client factory.
	// It is used to mock the ARM client factory for testing.
	// TODO: use fake options for testing
	newARMClientFactory = azclient.NewClientFactory
)

// errNoAzureCredentials is returned for every ARM request made without Azure credentials or a subscription ID.
var errNoAzureCredentials = errors.New("no Azure credentials or subscription ID are configured, so ARM requests cannot be made")

// noAzureCredentials is the TokenCredential given to ARM client factories when there are no Azure
// credentials. Every token request fails, so an ARM call returns errNoAzureCredentials before any
// request is sent. Without it the factory would fall back to a zero-value DefaultAzureCredential,
// which panics when asked for a token.
type noAzureCredentials struct{}

func (noAzureCredentials) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, errNoAzureCredentials
}

// newCompatibleARMClientFactory creates an ARM client factory that, like the SDK before it rejected an
// empty subscription ID, also starts without a subscription ID or credentials (cloud-node-manager's
// default IMDS mode). It returns the subscription ID the factory was created with.
func newCompatibleARMClientFactory(
	subscriptionID string,
	cred azcore.TokenCredential,
	armConfig *azclient.ARMClientConfig,
	cloudConfig cloud.Configuration,
	clientOptionsMutFn ...func(option *arm.ClientOptions),
) (azclient.ClientFactory, string, error) {
	subscriptionID, cred = clientFactoryIdentity(subscriptionID, cred)
	options := append([]func(*arm.ClientOptions){blockPlaceholderSubscription}, clientOptionsMutFn...)
	factory, err := newARMClientFactory(&azclient.ClientFactoryConfig{
		SubscriptionID: subscriptionID,
	}, armConfig, cloudConfig, cred, options...)
	return factory, subscriptionID, err
}

// clientFactoryIdentity returns the subscription ID and credential to build an ARM client factory with.
// IMDS-only callers must start without a subscription ID, even when credentials are found in the
// environment (for example workload identity variables), so an empty subscription ID becomes
// consts.PlaceholderSubscriptionID. Credentials are kept, because clients created for an explicit
// subscription (Get*ClientForSub) still use them; blockPlaceholderSubscription stops requests to the
// placeholder. Without credentials the credential is noAzureCredentials.
func clientFactoryIdentity(subscriptionID string, cred azcore.TokenCredential) (string, azcore.TokenCredential) {
	if subscriptionID == "" {
		subscriptionID = consts.PlaceholderSubscriptionID
	}
	if cred == nil {
		cred = noAzureCredentials{}
	}
	return subscriptionID, cred
}

// blockPlaceholderSubscription makes every ARM request to consts.PlaceholderSubscriptionID fail with
// errNoAzureCredentials. It runs per call, before a token is requested, so no request reaches the
// placeholder subscription.
func blockPlaceholderSubscription(option *arm.ClientOptions) {
	option.PerCallPolicies = append(option.PerCallPolicies, placeholderSubscriptionPolicy{})
}

type placeholderSubscriptionPolicy struct{}

func (placeholderSubscriptionPolicy) Do(req *policy.Request) (*http.Response, error) {
	if strings.Contains(strings.ToLower(req.Raw().URL.Path), "/subscriptions/"+consts.PlaceholderSubscriptionID+"/") {
		return nil, errNoAzureCredentials
	}
	return req.Next()
}
