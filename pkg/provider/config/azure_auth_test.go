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
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

var (
	CrossTenantNetworkResourceNegativeConfig = []*AzureAuthConfig{
		{
			TenantID:        "TenantID",
			AADClientID:     "AADClientID",
			AADClientSecret: "AADClientSecret",
		},
		{
			TenantID:                      "TenantID",
			AADClientID:                   "AADClientID",
			AADClientSecret:               "AADClientSecret",
			NetworkResourceTenantID:       "NetworkResourceTenantID",
			NetworkResourceSubscriptionID: "NetworkResourceSubscriptionID",
			IdentitySystem:                consts.ADFSIdentitySystem,
		},
		{
			TenantID:                      "TenantID",
			AADClientID:                   "AADClientID",
			AADClientSecret:               "AADClientSecret",
			NetworkResourceTenantID:       "NetworkResourceTenantID",
			NetworkResourceSubscriptionID: "NetworkResourceSubscriptionID",
			UseManagedIdentityExtension:   true,
		},
	}

	// msiEndpointEnv is the environment variable used to store the endpoint in go-autorest/adal library.
	msiEndpointEnv = "MSI_ENDPOINT"
	// msiSecretEnv is the environment variable used to store the request secret in go-autorest/adal library.
	msiSecretEnv = "MSI_SECRET"
)

func TestGetServicePrincipalTokenFromMSIWithUserAssignedID(t *testing.T) {
	configs := []*AzureAuthConfig{
		{
			UseManagedIdentityExtension: true,
			UserAssignedIdentityID:      "00000000-0000-0000-0000-000000000000",
		},
		// The Azure service principal is ignored when
		// UseManagedIdentityExtension is set to true
		{
			UseManagedIdentityExtension: true,
			UserAssignedIdentityID:      "00000000-0000-0000-0000-000000000000",
			TenantID:                    "TenantID",
			AADClientID:                 "AADClientID",
			AADClientSecret:             "AADClientSecret",
		},
	}
	env := &azure.PublicCloud

	// msiEndpointEnv and msiSecretEnv are required because autorest/adal library requires IMDS endpoint to be available.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("{}"))
		assert.NoError(t, err)
	}))
	originalEnv := os.Getenv(msiEndpointEnv)
	originalSecret := os.Getenv(msiSecretEnv)
	os.Setenv(msiEndpointEnv, server.URL)
	os.Setenv(msiSecretEnv, "secret")
	defer func() {
		server.Close()
		os.Setenv(msiEndpointEnv, originalEnv)
		os.Setenv(msiSecretEnv, originalSecret)
	}()

	for _, config := range configs {
		token, err := GetServicePrincipalToken(config, env, "")
		assert.NoError(t, err)

		msiEndpoint, err := adal.GetMSIVMEndpoint()
		assert.NoError(t, err)

		spt, err := adal.NewServicePrincipalTokenFromMSIWithUserAssignedID(msiEndpoint,
			env.ServiceManagementEndpoint, config.UserAssignedIdentityID)
		assert.NoError(t, err)
		assert.Equal(t, token, spt)
	}
}

func TestGetServicePrincipalTokenFromMSIWithIdentityResourceID(t *testing.T) {
	configs := []*AzureAuthConfig{
		{
			UseManagedIdentityExtension: true,
			UserAssignedIdentityID:      "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/ua",
		},
		// The Azure service principal is ignored when
		// UseManagedIdentityExtension is set to true
		{
			UseManagedIdentityExtension: true,
			UserAssignedIdentityID:      "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/ua",
			TenantID:                    "TenantID",
			AADClientID:                 "AADClientID",
			AADClientSecret:             "AADClientSecret",
		},
	}
	env := &azure.PublicCloud

	// msiEndpointEnv and msiSecretEnv are required because autorest/adal library requires IMDS endpoint to be available.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("{}"))
		assert.NoError(t, err)
	}))
	originalEnv := os.Getenv(msiEndpointEnv)
	originalSecret := os.Getenv(msiSecretEnv)
	os.Setenv(msiEndpointEnv, server.URL)
	os.Setenv(msiSecretEnv, "secret")
	defer func() {
		server.Close()
		os.Setenv(msiEndpointEnv, originalEnv)
		os.Setenv(msiSecretEnv, originalSecret)
	}()

	for _, config := range configs {
		token, err := GetServicePrincipalToken(config, env, "")
		assert.NoError(t, err)

		msiEndpoint, err := adal.GetMSIVMEndpoint()
		assert.NoError(t, err)

		spt, err := adal.NewServicePrincipalTokenFromMSIWithIdentityResourceID(msiEndpoint,
			env.ServiceManagementEndpoint, config.UserAssignedIdentityID)
		assert.NoError(t, err)
		assert.Equal(t, token, spt)
	}
}

func TestGetServicePrincipalTokenFromMSI(t *testing.T) {
	configs := []*AzureAuthConfig{
		{
			UseManagedIdentityExtension: true,
		},
		// The Azure service principal is ignored when
		// UseManagedIdentityExtension is set to true
		{
			UseManagedIdentityExtension: true,
			TenantID:                    "TenantID",
			AADClientID:                 "AADClientID",
			AADClientSecret:             "AADClientSecret",
		},
	}
	env := &azure.PublicCloud

	// msiEndpointEnv and msiSecretEnv are required because autorest/adal library requires IMDS endpoint to be available.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("{}"))
		assert.NoError(t, err)
	}))
	originalEnv := os.Getenv(msiEndpointEnv)
	originalSecret := os.Getenv(msiSecretEnv)
	os.Setenv(msiEndpointEnv, server.URL)
	os.Setenv(msiSecretEnv, "secret")
	defer func() {
		server.Close()
		os.Setenv(msiEndpointEnv, originalEnv)
		os.Setenv(msiSecretEnv, originalSecret)
	}()

	for _, config := range configs {
		token, err := GetServicePrincipalToken(config, env, "")
		assert.NoError(t, err)

		msiEndpoint, err := adal.GetMSIVMEndpoint()
		assert.NoError(t, err)

		spt, err := adal.NewServicePrincipalTokenFromMSI(msiEndpoint, env.ServiceManagementEndpoint)
		assert.NoError(t, err)
		assert.Equal(t, token, spt)
	}

}

func TestGetServicePrincipalToken(t *testing.T) {
	config := &AzureAuthConfig{
		TenantID:        "TenantID",
		AADClientID:     "AADClientID",
		AADClientSecret: "AADClientSecret",
	}
	env := &azure.PublicCloud

	token, err := GetServicePrincipalToken(config, env, "")
	assert.NoError(t, err)

	oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(env.ActiveDirectoryEndpoint, config.TenantID, nil)
	assert.NoError(t, err)

	spt, err := adal.NewServicePrincipalToken(*oauthConfig, config.AADClientID, config.AADClientSecret, env.ServiceManagementEndpoint)
	assert.NoError(t, err)

	assert.Equal(t, token, spt)
}

func TestGetMultiTenantServicePrincipalToken(t *testing.T) {
	config := &AzureAuthConfig{
		TenantID:                      "TenantID",
		AADClientID:                   "AADClientID",
		AADClientSecret:               "AADClientSecret",
		NetworkResourceTenantID:       "NetworkResourceTenantID",
		NetworkResourceSubscriptionID: "NetworkResourceSubscriptionID",
	}
	env := &azure.PublicCloud

	multiTenantToken, err := GetMultiTenantServicePrincipalToken(config, env)
	assert.NoError(t, err)

	multiTenantOAuthConfig, err := adal.NewMultiTenantOAuthConfig(env.ActiveDirectoryEndpoint, config.TenantID, []string{config.NetworkResourceTenantID}, adal.OAuthOptions{})
	assert.NoError(t, err)

	spt, err := adal.NewMultiTenantServicePrincipalToken(multiTenantOAuthConfig, config.AADClientID, config.AADClientSecret, env.ServiceManagementEndpoint)
	assert.NoError(t, err)

	assert.Equal(t, multiTenantToken, spt)
}

func TestGetServicePrincipalTokenFromCertificate(t *testing.T) {
	config := &AzureAuthConfig{
		TenantID:              "TenantID",
		AADClientID:           "AADClientID",
		AADClientCertPath:     "./testdata/test.pfx",
		AADClientCertPassword: "id",
	}
	env := &azure.PublicCloud
	token, err := GetServicePrincipalToken(config, env, "")
	assert.NoError(t, err)

	oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(env.ActiveDirectoryEndpoint, config.TenantID, nil)
	assert.NoError(t, err)
	pfxContent, err := os.ReadFile("./testdata/test.pfx")
	assert.NoError(t, err)
	certificate, privateKey, err := decodePkcs12(pfxContent, "id")
	assert.NoError(t, err)
	spt, err := adal.NewServicePrincipalTokenFromCertificate(*oauthConfig, config.AADClientID, certificate, privateKey, env.ServiceManagementEndpoint)
	assert.NoError(t, err)
	assert.Equal(t, token, spt)
}

func TestGetServicePrincipalTokenFromCertificateWithoutPassword(t *testing.T) {
	config := &AzureAuthConfig{
		ARMClientConfig: azclient.ARMClientConfig{
			TenantID: "TenantID",
		},
		AzureAuthConfig: azclient.AzureAuthConfig{
			AADClientID:       "AADClientID",
			AADClientCertPath: "./testdata/testnopassword.pfx",
		},
	}
	env := &azure.PublicCloud
	token, err := GetServicePrincipalToken(config, env, "")
	assert.NoError(t, err)

	oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(env.ActiveDirectoryEndpoint, config.TenantID, nil)
	assert.NoError(t, err)
	pfxContent, err := os.ReadFile("./testdata/testnopassword.pfx")
	assert.NoError(t, err)
	certificate, privateKey, err := decodePkcs12(pfxContent, "")
	assert.NoError(t, err)
	spt, err := adal.NewServicePrincipalTokenFromCertificate(*oauthConfig, config.AADClientID, certificate, privateKey, env.ServiceManagementEndpoint)
	assert.NoError(t, err)
	assert.Equal(t, token, spt)
}

func TestGetMultiTenantServicePrincipalTokenFromCertificate(t *testing.T) {
	config := &AzureAuthConfig{
		ARMClientConfig: azclient.ARMClientConfig{
			TenantID:                "TenantID",
			NetworkResourceTenantID: "NetworkResourceTenantID",
		},
		AzureAuthConfig: azclient.AzureAuthConfig{
			AADClientID:       "AADClientID",
			AADClientCertPath: "./testdata/testnopassword.pfx",
		},
		NetworkResourceSubscriptionID: "NetworkResourceSubscriptionID",
	}
	env := &azure.PublicCloud

	multiTenantToken, err := GetMultiTenantServicePrincipalToken(config, env)
	assert.NoError(t, err)

	multiTenantOAuthConfig, err := adal.NewMultiTenantOAuthConfig(env.ActiveDirectoryEndpoint, config.TenantID, []string{config.NetworkResourceTenantID}, adal.OAuthOptions{})
	assert.NoError(t, err)

	pfxContent, err := os.ReadFile("./testdata/testnopassword.pfx")
	assert.NoError(t, err)
	certificate, privateKey, err := decodePkcs12(pfxContent, "")
	assert.NoError(t, err)
	spt, err := adal.NewMultiTenantServicePrincipalTokenFromCertificate(multiTenantOAuthConfig, config.AADClientID, certificate, privateKey, env.ServiceManagementEndpoint)
	assert.NoError(t, err)

	assert.Equal(t, multiTenantToken, spt)
}

func TestGetMultiTenantServicePrincipalTokenNegative(t *testing.T) {
	env := &azure.PublicCloud
	for _, config := range CrossTenantNetworkResourceNegativeConfig {
		_, err := GetMultiTenantServicePrincipalToken(config, env)
		assert.Error(t, err)
	}
}

func TestGetNetworkResourceServicePrincipalToken(t *testing.T) {
	config := &AzureAuthConfig{
		TenantID:                      "TenantID",
		AADClientID:                   "AADClientID",
		AADClientSecret:               "AADClientSecret",
		NetworkResourceTenantID:       "NetworkResourceTenantID",
		NetworkResourceSubscriptionID: "NetworkResourceSubscriptionID",
	}
	env := &azure.PublicCloud

	token, err := GetNetworkResourceServicePrincipalToken(config, env)
	assert.NoError(t, err)

	oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(env.ActiveDirectoryEndpoint, config.NetworkResourceTenantID, nil)
	assert.NoError(t, err)

	spt, err := adal.NewServicePrincipalToken(*oauthConfig, config.AADClientID, config.AADClientSecret, env.ServiceManagementEndpoint)
	assert.NoError(t, err)

	assert.Equal(t, token, spt)
}

func TestGetNetworkResourceServicePrincipalTokenFromCertificate(t *testing.T) {
	config := &AzureAuthConfig{
		ARMClientConfig: azclient.ARMClientConfig{
			TenantID:                "TenantID",
			NetworkResourceTenantID: "NetworkResourceTenantID",
		},
		AzureAuthConfig: azclient.AzureAuthConfig{
			AADClientID:       "AADClientID",
			AADClientCertPath: "./testdata/testnopassword.pfx",
		},
		NetworkResourceSubscriptionID: "NetworkResourceSubscriptionID",
	}
	env := &azure.PublicCloud

	token, err := GetNetworkResourceServicePrincipalToken(config, env)
	assert.NoError(t, err)

	oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(env.ActiveDirectoryEndpoint, config.NetworkResourceTenantID, nil)
	assert.NoError(t, err)

	pfxContent, err := os.ReadFile("./testdata/testnopassword.pfx")
	assert.NoError(t, err)
	certificate, privateKey, err := decodePkcs12(pfxContent, "")
	assert.NoError(t, err)
	spt, err := adal.NewServicePrincipalTokenFromCertificate(*oauthConfig, config.AADClientID, certificate, privateKey, env.ServiceManagementEndpoint)
	assert.NoError(t, err)

	assert.Equal(t, token, spt)
}

func TestGetNetworkResourceServicePrincipalTokenNegative(t *testing.T) {
	env := &azure.PublicCloud
	for _, config := range CrossTenantNetworkResourceNegativeConfig {
		_, err := GetNetworkResourceServicePrincipalToken(config, env)
		assert.Error(t, err)
	}
}

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
	config := &AzureAuthConfig{
		TenantID:                      "TenantID",
		AADClientID:                   "AADClientID",
		AADClientSecret:               "AADClientSecret",
		NetworkResourceTenantID:       "NetworkTenantID",
		NetworkResourceSubscriptionID: "NetworkResourceSubscriptionID",
	}

	assert.Equal(t, config.UsesNetworkResourceInDifferentTenant(), true)
	assert.Equal(t, config.UsesNetworkResourceInDifferentSubscription(), true)

	config.NetworkResourceTenantID = ""
	config.NetworkResourceSubscriptionID = ""
	assert.Equal(t, config.UsesNetworkResourceInDifferentTenant(), false)
	assert.Equal(t, config.UsesNetworkResourceInDifferentSubscription(), false)
}
