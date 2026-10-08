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
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// staticTokenCredential is a TokenCredential that is never asked for a token; its value tells instances apart.
type staticTokenCredential string

func (staticTokenCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, nil
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestClientFactoryIdentity(t *testing.T) {
	var cred azcore.TokenCredential = staticTokenCredential("cred")

	subscriptionID, gotCred := clientFactoryIdentity("", nil)
	assert.Equal(t, consts.PlaceholderSubscriptionID, subscriptionID,
		"without credentials an empty subscription must not stop IMDS-only callers from starting")
	assert.Equal(t, noAzureCredentials{}, gotCred, "without credentials ARM calls must fail with an error, not panic")

	subscriptionID, gotCred = clientFactoryIdentity("sub", nil)
	assert.Equal(t, "sub", subscriptionID)
	assert.Equal(t, noAzureCredentials{}, gotCred)

	subscriptionID, gotCred = clientFactoryIdentity("", cred)
	assert.Equal(t, consts.PlaceholderSubscriptionID, subscriptionID,
		"with credentials but no subscription, IMDS-only callers must still start")
	assert.Equal(t, cred, gotCred, "clients for an explicit subscription must keep the credentials")

	subscriptionID, gotCred = clientFactoryIdentity("sub", cred)
	assert.Equal(t, "sub", subscriptionID)
	assert.Equal(t, cred, gotCred)
}

func TestNoAzureCredentialsFailsEveryTokenRequest(t *testing.T) {
	_, err := noAzureCredentials{}.GetToken(context.Background(), policy.TokenRequestOptions{})
	assert.ErrorIs(t, err, errNoAzureCredentials)
}

func TestBlockPlaceholderSubscription(t *testing.T) {
	options := &arm.ClientOptions{}
	blockPlaceholderSubscription(options)
	sent := 0
	pipeline := runtime.NewPipeline("test", "v0", runtime.PipelineOptions{}, &policy.ClientOptions{
		PerCallPolicies: options.PerCallPolicies,
		Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			sent++
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
	})

	req, err := runtime.NewRequest(context.Background(), http.MethodGet,
		"https://management.azure.com/subscriptions/"+consts.PlaceholderSubscriptionID+"/resourceGroups/rg")
	assert.NoError(t, err)
	_, err = pipeline.Do(req)
	assert.ErrorIs(t, err, errNoAzureCredentials)
	assert.Zero(t, sent, "no request may reach the placeholder subscription")

	req, err = runtime.NewRequest(context.Background(), http.MethodGet,
		"https://management.azure.com/Subscriptions/"+consts.PlaceholderSubscriptionID+"/resourceGroups/rg")
	assert.NoError(t, err)
	_, err = pipeline.Do(req)
	assert.ErrorIs(t, err, errNoAzureCredentials)
	assert.Zero(t, sent, "the path is matched case-insensitively")

	req, err = runtime.NewRequest(context.Background(), http.MethodGet,
		"https://management.azure.com/subscriptions/33333333-3333-3333-3333-333333333333/resourceGroups/rg")
	assert.NoError(t, err)
	_, err = pipeline.Do(req)
	assert.NoError(t, err)
	assert.Equal(t, 1, sent, "requests to an explicit subscription must be sent")
}

func TestNewCompatibleARMClientFactory(t *testing.T) {
	var (
		gotConfig    *azclient.ClientFactoryConfig
		gotARMConfig *azclient.ARMClientConfig
		gotCloud     cloud.Configuration
		gotCred      azcore.TokenCredential
		gotOptions   []func(option *arm.ClientOptions)
	)
	newARMClientFactory = func(
		config *azclient.ClientFactoryConfig,
		armConfig *azclient.ARMClientConfig,
		cloudConfig cloud.Configuration,
		cred azcore.TokenCredential,
		clientOptionsMutFn ...func(option *arm.ClientOptions),
	) (azclient.ClientFactory, error) {
		gotConfig, gotARMConfig, gotCloud, gotCred, gotOptions = config, armConfig, cloudConfig, cred, clientOptionsMutFn
		return nil, nil
	}
	defer func() { newARMClientFactory = azclient.NewClientFactory }()

	extraApplied := false
	extra := func(*arm.ClientOptions) { extraApplied = true }
	armConfig := &azclient.ARMClientConfig{Cloud: "AzureChinaCloud"}
	_, subscriptionID, err := newCompatibleARMClientFactory("", nil, armConfig, cloud.AzureChina, extra)
	assert.NoError(t, err)
	assert.Same(t, armConfig, gotARMConfig)
	assert.Equal(t, cloud.AzureChina.ActiveDirectoryAuthorityHost, gotCloud.ActiveDirectoryAuthorityHost)
	assert.Equal(t, consts.PlaceholderSubscriptionID, subscriptionID)
	assert.Equal(t, consts.PlaceholderSubscriptionID, gotConfig.SubscriptionID)
	assert.Equal(t, noAzureCredentials{}, gotCred)

	options := &arm.ClientOptions{}
	for _, fn := range gotOptions {
		fn(options)
	}
	assert.Contains(t, options.PerCallPolicies, policy.Policy(placeholderSubscriptionPolicy{}),
		"every factory must block requests to the placeholder subscription")
	assert.True(t, extraApplied, "caller options must still be applied")

	var cred azcore.TokenCredential = staticTokenCredential("cred")
	_, subscriptionID, err = newCompatibleARMClientFactory("sub", cred, &azclient.ARMClientConfig{}, cloud.AzurePublic)
	assert.NoError(t, err)
	assert.Equal(t, "sub", subscriptionID)
	assert.Equal(t, "sub", gotConfig.SubscriptionID)
	assert.Equal(t, cred, gotCred)
}

func TestNewCompatibleARMClientFactoryReturnsFactoryError(t *testing.T) {
	errFactory := errors.New("factory error")
	newARMClientFactory = func(
		*azclient.ClientFactoryConfig,
		*azclient.ARMClientConfig,
		cloud.Configuration,
		azcore.TokenCredential,
		...func(option *arm.ClientOptions),
	) (azclient.ClientFactory, error) {
		return nil, errFactory
	}
	defer func() { newARMClientFactory = azclient.NewClientFactory }()

	_, _, err := newCompatibleARMClientFactory("sub", nil, &azclient.ARMClientConfig{}, cloud.AzurePublic)
	assert.ErrorIs(t, err, errFactory)
}
