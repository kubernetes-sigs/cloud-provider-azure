/*
Copyright 2024 The Kubernetes Authors.

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

package storage

import (
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/accountclient"
	azureconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
)

// TestBuildSATokenClientOptions_CloudSelection is a regression test for
// https://github.com/kubernetes-sigs/blob-csi-driver/issues/2649: when
// GetStorageAccesskeyFromServiceAccountToken runs in a sovereign cloud
// (Azure China / Azure US Government / Azure Stack), the azidentity
// credential and the ARM storage-account client MUST NOT default to Azure
// Public. Otherwise the AAD token exchange fails with AADSTS500011 because
// the token audience does not match the sovereign-cloud ARM resource ID.
//
// This test derives the client options exactly the same way the fix does
// (via buildSATokenClientOptions -> azclient.GetAzCoreClientOption), and
// pins both the AAD authority host and the ARM ResourceManager endpoint per
// cloud so any future refactor that drops the cloud config on the floor
// fails loudly.
func TestBuildSATokenClientOptions_CloudSelection(t *testing.T) {
	tests := []struct {
		name                      string
		cloudName                 string
		wantAADAuthority          string
		wantARMEndpoint           string
		wantARMAudience           string
		wantAPIVersion            string
		wantDisableInstanceDiscov bool
	}{
		{
			name:                      "Azure Public (default)",
			cloudName:                 "AzurePublicCloud",
			wantAADAuthority:          cloud.AzurePublic.ActiveDirectoryAuthorityHost,
			wantARMEndpoint:           cloud.AzurePublic.Services[cloud.ResourceManager].Endpoint,
			wantARMAudience:           cloud.AzurePublic.Services[cloud.ResourceManager].Audience,
			wantAPIVersion:            "", // SDK default; no override.
			wantDisableInstanceDiscov: false,
		},
		{
			// The bug: before this fix, the credential/ARM client fell
			// back to AzurePublic here, and Azure China token exchange
			// failed with AADSTS500011. Assert BOTH the endpoint and the
			// audience so a mixed China-endpoint + Public-audience
			// configuration (the exact shape of issue #2649) cannot pass.
			name:                      "Azure China",
			cloudName:                 "AzureChinaCloud",
			wantAADAuthority:          cloud.AzureChina.ActiveDirectoryAuthorityHost,
			wantARMEndpoint:           cloud.AzureChina.Services[cloud.ResourceManager].Endpoint,
			wantARMAudience:           cloud.AzureChina.Services[cloud.ResourceManager].Audience,
			wantAPIVersion:            accountclient.MooncakeApiVersion,
			wantDisableInstanceDiscov: false,
		},
		{
			name:                      "Azure US Government",
			cloudName:                 "AzureUSGovernmentCloud",
			wantAADAuthority:          cloud.AzureGovernment.ActiveDirectoryAuthorityHost,
			wantARMEndpoint:           cloud.AzureGovernment.Services[cloud.ResourceManager].Endpoint,
			wantARMAudience:           cloud.AzureGovernment.Services[cloud.ResourceManager].Audience,
			wantAPIVersion:            accountclient.MooncakeApiVersion,
			wantDisableInstanceDiscov: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			az := &AccountRepo{
				Config: azureconfig.Config{
					AzureClientConfig: azureconfig.AzureClientConfig{
						ARMClientConfig: azclient.ARMClientConfig{
							Cloud: tc.cloudName,
						},
					},
				},
			}

			credOpts, armOpts, isAzureStack, err := buildSATokenClientOptions(&az.ARMClientConfig)
			assert.NoError(t, err)

			// azidentity uses ClientOptions.Cloud.ActiveDirectoryAuthorityHost
			// to build the token endpoint; this is exactly what determines
			// whether we hit login.microsoftonline.com vs
			// login.chinacloudapi.cn vs login.microsoftonline.us.
			assert.Equal(t, tc.wantAADAuthority, credOpts.Cloud.ActiveDirectoryAuthorityHost,
				"credential AAD authority must follow the configured cloud")

			// arm.ClientOptions.Cloud.Services[ResourceManager] is what the
			// storage-account ARM client uses for both the request base URL
			// and the token audience (scope).
			gotARM, ok := armOpts.Cloud.Services[cloud.ResourceManager]
			assert.True(t, ok, "arm client options must carry a ResourceManager endpoint")
			assert.Equal(t, tc.wantARMEndpoint, gotARM.Endpoint,
				"ARM ResourceManager endpoint must follow the configured cloud")
			// The ARM bearer policy builds its scope from Services[ResourceManager].Audience
			// independently of the endpoint, so a mixed China-endpoint +
			// Public-audience configuration (issue #2649's exact shape)
			// must not slip through.
			assert.Equal(t, tc.wantARMAudience, gotARM.Audience,
				"ARM ResourceManager audience must follow the configured cloud")
			// The account-client factory pins an older ListKeys API
			// version in sovereign clouds; the SA-token path must match
			// or ListKeys will fail with an unsupported-API-version error
			// in the same clouds even after the authentication fix.
			assert.Equal(t, tc.wantAPIVersion, armOpts.APIVersion,
				"ARM APIVersion override must match the normal account-client factory")
			assert.Equal(t, tc.wantDisableInstanceDiscov, isAzureStack,
				"DisableInstanceDiscovery must be true for Azure Stack")
		})
	}
}

// TestBuildSATokenClientOptions_NilConfig ensures the helper degrades to
// a valid default (public cloud) rather than panicking when no cloud is
// configured; this matches the historical behavior of the ARM client
// factory for a zero-valued ARMClientConfig.
func TestBuildSATokenClientOptions_NilConfig(t *testing.T) {
	az := &AccountRepo{}

	credOpts, armOpts, isAzureStack, err := buildSATokenClientOptions(&az.ARMClientConfig)
	assert.NoError(t, err)
	assert.False(t, isAzureStack)
	// Zero-valued config defaults to AzurePublic.
	assert.Equal(t, cloud.AzurePublic.ActiveDirectoryAuthorityHost, credOpts.Cloud.ActiveDirectoryAuthorityHost)
	gotARM, ok := armOpts.Cloud.Services[cloud.ResourceManager]
	assert.True(t, ok)
	assert.Equal(t, cloud.AzurePublic.Services[cloud.ResourceManager].Endpoint, gotARM.Endpoint)
}
