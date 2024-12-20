/*
Copyright 2019 The Kubernetes Authors.

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
	"testing"

	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

var (
	CrossTenantNetworkResourceNegativeConfig = []*AzureClientConfig{
		{
			// missing NetworkResourceTenantID
			ARMClientConfig: azclient.ARMClientConfig{
				TenantID: "TenantID",
			},
			AzureAuthConfig: azclient.AzureAuthConfig{
				AADClientID:     "AADClientID",
				AADClientSecret: "AADClientSecret",
			},
		},
		{
			ARMClientConfig: azclient.ARMClientConfig{
				TenantID:                "TenantID",
				NetworkResourceTenantID: "NetworkResourceTenantID",
			},
			AzureAuthConfig: azclient.AzureAuthConfig{
				AADClientID:     "AADClientID",
				AADClientSecret: "AADClientSecret",
			},
			NetworkResourceSubscriptionID: "NetworkResourceSubscriptionID",
			IdentitySystem:                consts.ADFSIdentitySystem, // multi-tenant not supported with ADFS
		},
	}
)

func TestParseAzureEnvironment(t *testing.T) {
	cases := []struct {
		cloudName               string
		resourceManagerEndpoint string
		identitySystem          string
		expected                *azure.Environment
	}{
		{
			cloudName:               "",
			resourceManagerEndpoint: "",
			identitySystem:          "",
			expected:                &azure.PublicCloud,
		},
		{
			cloudName:               "AZURECHINACLOUD",
			resourceManagerEndpoint: "",
			identitySystem:          "",
			expected:                &azure.ChinaCloud,
		},
	}

	for _, c := range cases {
		env, err := ParseAzureEnvironment(c.cloudName, c.resourceManagerEndpoint, c.identitySystem)
		assert.NoError(t, err)
		assert.Equal(t, env, c.expected)
	}
}

func TestParseAzureEnvironmentForAzureStack(t *testing.T) {
	c := struct {
		cloudName               string
		resourceManagerEndpoint string
		identitySystem          string
	}{
		cloudName:               "AZURESTACKCCLOUD",
		resourceManagerEndpoint: "https://management.azure.com/",
		identitySystem:          "",
	}

	nameOverride := azure.OverrideProperty{Key: azure.EnvironmentName, Value: c.cloudName}
	expected, err := azure.EnvironmentFromURL(c.resourceManagerEndpoint, nameOverride)
	assert.NoError(t, err)
	azureStackOverrides(&expected, c.resourceManagerEndpoint, c.identitySystem)

	env, err := ParseAzureEnvironment(c.cloudName, c.resourceManagerEndpoint, c.identitySystem)
	assert.NoError(t, err)
	assert.Equal(t, env, &expected)

}

func TestAzureStackOverrides(t *testing.T) {
	env := &azure.PublicCloud
	resourceManagerEndpoint := "https://management.test.com/"

	azureStackOverrides(env, resourceManagerEndpoint, "")
	assert.Equal(t, env.ManagementPortalURL, "https://portal.test.com/")
	assert.Equal(t, env.ServiceManagementEndpoint, env.TokenAudience)
	assert.Equal(t, env.ResourceManagerVMDNSSuffix, "cloudapp.test.com")
	assert.Equal(t, env.ActiveDirectoryEndpoint, "https://login.microsoftonline.com/")

	azureStackOverrides(env, resourceManagerEndpoint, "adfs")
	assert.Equal(t, env.ManagementPortalURL, "https://portal.test.com/")
	assert.Equal(t, env.ServiceManagementEndpoint, env.TokenAudience)
	assert.Equal(t, env.ResourceManagerVMDNSSuffix, "cloudapp.test.com")
	assert.Equal(t, env.ActiveDirectoryEndpoint, "https://login.microsoftonline.com")
}

func TestUsesNetworkResourceInDifferentTenant(t *testing.T) {
	config := &AzureClientConfig{
		ARMClientConfig: azclient.ARMClientConfig{
			TenantID:                "TenantID",
			NetworkResourceTenantID: "NetworkResourceTenantID",
		},
		AzureAuthConfig: azclient.AzureAuthConfig{
			AADClientID:     "AADClientID",
			AADClientSecret: "AADClientSecret",
		},
		NetworkResourceSubscriptionID: "NetworkResourceSubscriptionID",
	}

	assert.Equal(t, config.UsesNetworkResourceInDifferentTenant(), true)
	assert.Equal(t, config.UsesNetworkResourceInDifferentSubscription(), true)

	config.NetworkResourceTenantID = ""
	config.NetworkResourceSubscriptionID = ""
	assert.Equal(t, config.UsesNetworkResourceInDifferentTenant(), false)
	assert.Equal(t, config.UsesNetworkResourceInDifferentSubscription(), false)
}
