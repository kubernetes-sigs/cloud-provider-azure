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
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

var (
	CrossTenantNetworkResourceNegativeConfig = []*AzureAuthConfig{
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

const (
	// envMSIEndpoint is the environment variable used to store the endpoint in go-autorest/adal library.
	envMSIEndpoint = "MSI_ENDPOINT"
	// envMSISecret is the environment variable used to store the request secret in go-autorest/adal library.
	envMSISecret = "MSI_SECRET"
)

func TestGetServicePrincipalToken(t *testing.T) {
	env := &azure.PublicCloud
	setupLocalMSIServer := func(t *testing.T) (*httptest.Server, func()) {
		t.Helper()
		var (
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodGet, r.Method)
				w.WriteHeader(http.StatusOK)
				_, err := w.Write([]byte("{}"))
				assert.NoError(t, err)
			}))
			originalEnv    = os.Getenv(envMSIEndpoint)
			originalSecret = os.Getenv(envMSISecret)

			cleanUp = func() {
				server.Close()
				_ = os.Setenv(envMSIEndpoint, originalEnv)
				_ = os.Setenv(envMSISecret, originalSecret)
			}
		)

		_ = os.Setenv(envMSIEndpoint, server.URL)
		_ = os.Setenv(envMSISecret, "secret")

		return server, cleanUp
	}

	t.Run("setup with MSI (user assigned managed identity)", func(t *testing.T) {
		type IDType int
		const (
			ResourceID IDType = iota
			ClientID
		)

		tests := []struct {
			Name   string
			Type   IDType
			Config *AzureAuthConfig
		}{
			{
				Name: "client id",
				Type: ClientID,
				Config: &AzureAuthConfig{
					AzureAuthConfig: azclient.AzureAuthConfig{
						UseManagedIdentityExtension: true,
						UserAssignedIdentityID:      "00000000-0000-0000-0000-000000000000",
					},
				},
			},
			{
				Name: "client id with SP (SP config should be ignored)",
				Type: ClientID,
				Config: &AzureAuthConfig{
					ARMClientConfig: azclient.ARMClientConfig{
						TenantID: "TenantID",
					},
					AzureAuthConfig: azclient.AzureAuthConfig{
						UseManagedIdentityExtension: true,
						UserAssignedIdentityID:      "00000000-0000-0000-0000-000000000000",
						AADClientID:                 "AADClientID",
						AADClientSecret:             "AADClientSecret",
					},
				},
			},
			{
				Name: "resource id",
				Type: ResourceID,
				Config: &AzureAuthConfig{
					AzureAuthConfig: azclient.AzureAuthConfig{
						UseManagedIdentityExtension: true,
						UserAssignedIdentityID:      "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/ua",
					},
				},
			},
			{
				Name: "resource id with SP (SP config should be ignored)",
				Type: ResourceID,
				Config: &AzureAuthConfig{
					ARMClientConfig: azclient.ARMClientConfig{
						TenantID: "TenantID",
					},
					AzureAuthConfig: azclient.AzureAuthConfig{
						UseManagedIdentityExtension: true,
						UserAssignedIdentityID:      "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/ua",
						AADClientID:                 "AADClientID",
						AADClientSecret:             "AADClientSecret",
					},
				},
			},
		}

		for _, tt := range tests {
			t.Run(tt.Name, func(t *testing.T) {
				_, cleanUp := setupLocalMSIServer(t)
				defer cleanUp()

				token, err := GetServicePrincipalToken(tt.Config, env, "")
				assert.NoError(t, err)

				msiEndpoint, err := adal.GetMSIVMEndpoint()
				assert.NoError(t, err)

				switch tt.Type {
				case ClientID:
					spt, err := adal.NewServicePrincipalTokenFromMSIWithUserAssignedID(msiEndpoint,
						env.ServiceManagementEndpoint, tt.Config.UserAssignedIdentityID)
					assert.NoError(t, err)
					assert.Equal(t, token, spt)
				case ResourceID:
					spt, err := adal.NewServicePrincipalTokenFromMSIWithIdentityResourceID(msiEndpoint,
						env.ServiceManagementEndpoint, tt.Config.UserAssignedIdentityID)
					assert.NoError(t, err)
					assert.Equal(t, token, spt)
				}
			})
		}
	})

	t.Run("setup with MSI (system managed identity)", func(t *testing.T) {
		tests := []struct {
			Name   string
			Config *AzureAuthConfig
		}{
			{
				Name: "default",
				Config: &AzureAuthConfig{
					AzureAuthConfig: azclient.AzureAuthConfig{
						UseManagedIdentityExtension: true,
					},
				},
			},
			{
				Name: "with SP (SP config should be ignored",
				Config: &AzureAuthConfig{
					ARMClientConfig: azclient.ARMClientConfig{
						TenantID: "TenantID",
					},
					AzureAuthConfig: azclient.AzureAuthConfig{
						UseManagedIdentityExtension: true,
						AADClientID:                 "AADClientID",
						AADClientSecret:             "AADClientSecret",
					},
				},
			},
		}

		for _, tt := range tests {
			t.Run(tt.Name, func(t *testing.T) {
				_, cleanUp := setupLocalMSIServer(t)
				defer cleanUp()

				token, err := GetServicePrincipalToken(tt.Config, env, "")
				assert.NoError(t, err)

				msiEndpoint, err := adal.GetMSIVMEndpoint()
				assert.NoError(t, err)

				spt, err := adal.NewServicePrincipalTokenFromMSI(msiEndpoint, env.ServiceManagementEndpoint)
				assert.NoError(t, err)
				assert.Equal(t, token, spt)

			})
		}
	})

	t.Run("setup with workload identity", func(t *testing.T) {
		config := &AzureAuthConfig{
			ARMClientConfig: azclient.ARMClientConfig{
				TenantID: "TenantID",
			},
			AzureAuthConfig: azclient.AzureAuthConfig{
				AADClientID:                           "AADClientID",
				AADFederatedTokenFile:                 "/tmp/federated-token",
				UseFederatedWorkloadIdentityExtension: true,
			},
		}
		env := &azure.PublicCloud

		token, err := GetServicePrincipalToken(config, env, "")
		assert.NoError(t, err)
		marshalToken, _ := token.MarshalJSON()

		oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(env.ActiveDirectoryEndpoint, config.TenantID, nil)
		assert.NoError(t, err)

		jwtCallback := func() (string, error) {
			jwt, err := os.ReadFile(config.AADFederatedTokenFile)
			if err != nil {
				return "", fmt.Errorf("failed to read a file with a federated token: %w", err)
			}
			return string(jwt), nil
		}

		spt, err := adal.NewServicePrincipalTokenFromFederatedTokenCallback(*oauthConfig, config.AADClientID, jwtCallback, env.ResourceManagerEndpoint)
		assert.NoError(t, err)

		marshalSpt, _ := spt.MarshalJSON()

		assert.Equal(t, marshalToken, marshalSpt)
	})

	t.Run("setup with SP with password", func(t *testing.T) {
		config := &AzureAuthConfig{
			ARMClientConfig: azclient.ARMClientConfig{
				TenantID: "TenantID",
			},
			AzureAuthConfig: azclient.AzureAuthConfig{
				AADClientID:     "AADClientID",
				AADClientSecret: "AADClientSecret",
			},
		}
		env := &azure.PublicCloud

		token, err := GetServicePrincipalToken(config, env, "")
		assert.NoError(t, err)

		oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(env.ActiveDirectoryEndpoint, config.TenantID, nil)
		assert.NoError(t, err)

		spt, err := adal.NewServicePrincipalToken(*oauthConfig, config.AADClientID, config.AADClientSecret, env.ServiceManagementEndpoint)
		assert.NoError(t, err)

		assert.Equal(t, token, spt)
	})

	t.Run("setup with SP with certificate", func(t *testing.T) {
		config := &AzureAuthConfig{
			ARMClientConfig: azclient.ARMClientConfig{
				TenantID: "TenantID",
			},
			AzureAuthConfig: azclient.AzureAuthConfig{
				AADClientID:           "AADClientID",
				AADClientCertPath:     "./testdata/test.pfx",
				AADClientCertPassword: "id",
			},
		}
		env := &azure.PublicCloud
		token, err := GetServicePrincipalToken(config, env, "")
		assert.NoError(t, err)

		oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(env.ActiveDirectoryEndpoint, config.TenantID, nil)
		assert.NoError(t, err)
		pfxContent, err := os.ReadFile("./testdata/test.pfx")
		assert.NoError(t, err)
		certificates, privateKey, err := azidentity.ParseCertificates(pfxContent, []byte("id"))
		assert.NoError(t, err)
		spt, err := adal.NewServicePrincipalTokenFromCertificate(*oauthConfig, config.AADClientID, certificates[0], privateKey.(*rsa.PrivateKey), env.ServiceManagementEndpoint)
		assert.NoError(t, err)
		assert.Equal(t, token, spt)
	})

	t.Run("setup with SP with certificate has no password", func(t *testing.T) {
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
		certificates, privateKey, err := azidentity.ParseCertificates(pfxContent, nil)
		assert.NoError(t, err)
		spt, err := adal.NewServicePrincipalTokenFromCertificate(*oauthConfig, config.AADClientID, certificates[0], privateKey.(*rsa.PrivateKey), env.ServiceManagementEndpoint)
		assert.NoError(t, err)
		assert.Equal(t, token, spt)
	})

	t.Run("setup with SP with certificate has multi public key", func(t *testing.T) {
		config := &AzureAuthConfig{
			ARMClientConfig: azclient.ARMClientConfig{
				TenantID: "TenantID",
			},
			AzureAuthConfig: azclient.AzureAuthConfig{
				AADClientID:       "AADClientID",
				AADClientCertPath: "./testdata/testmultipublickey.pem",
			},
		}
		env := &azure.PublicCloud
		token, err := GetServicePrincipalToken(config, env, "")
		assert.NoError(t, err)

		oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(env.ActiveDirectoryEndpoint, config.TenantID, nil)
		assert.NoError(t, err)
		pfxContent, err := os.ReadFile("./testdata/testmultipublickey.pem")
		assert.NoError(t, err)
		certificates, privateKey, err := azidentity.ParseCertificates(pfxContent, nil)
		assert.NoError(t, err)
		// expected public key is in second bag
		certificate := certificates[1]
		spt, err := adal.NewServicePrincipalTokenFromCertificate(*oauthConfig, config.AADClientID, certificate, privateKey.(*rsa.PrivateKey), env.ServiceManagementEndpoint)
		assert.NoError(t, err)
		assert.Equal(t, token, spt)
	})

	t.Run("setup with SP with certificate has no public key", func(t *testing.T) {
		config := &AzureAuthConfig{
			ARMClientConfig: azclient.ARMClientConfig{
				TenantID: "TenantID",
			},
			AzureAuthConfig: azclient.AzureAuthConfig{
				AADClientID:       "AADClientID",
				AADClientCertPath: "./testdata/testnopublickey.pem",
			},
		}
		env := &azure.PublicCloud
		_, err := GetServicePrincipalToken(config, env, "")
		assert.Error(t, err)
	})
}

func TestGetMultiTenantServicePrincipalToken(t *testing.T) {
	t.Run("setup with SP with password", func(t *testing.T) {
		config := &AzureAuthConfig{
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
		env := &azure.PublicCloud

		multiTenantToken, err := GetMultiTenantServicePrincipalToken(config, env, nil)
		assert.NoError(t, err)

		multiTenantOAuthConfig, err := adal.NewMultiTenantOAuthConfig(env.ActiveDirectoryEndpoint, config.TenantID, []string{config.NetworkResourceTenantID}, adal.OAuthOptions{})
		assert.NoError(t, err)

		spt, err := adal.NewMultiTenantServicePrincipalToken(multiTenantOAuthConfig, config.AADClientID, config.AADClientSecret, env.ServiceManagementEndpoint)
		assert.NoError(t, err)

		assert.Equal(t, multiTenantToken, spt)
	})

	t.Run("setup with SP with certificate", func(t *testing.T) {
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

		multiTenantToken, err := GetMultiTenantServicePrincipalToken(config, env, nil)
		assert.NoError(t, err)

		multiTenantOAuthConfig, err := adal.NewMultiTenantOAuthConfig(env.ActiveDirectoryEndpoint, config.TenantID, []string{config.NetworkResourceTenantID}, adal.OAuthOptions{})
		assert.NoError(t, err)

		pfxContent, err := os.ReadFile("./testdata/testnopassword.pfx")
		assert.NoError(t, err)
		certificates, privateKey, err := azidentity.ParseCertificates(pfxContent, nil)
		assert.NoError(t, err)
		spt, err := adal.NewMultiTenantServicePrincipalTokenFromCertificate(multiTenantOAuthConfig, config.AADClientID, certificates[0], privateKey.(*rsa.PrivateKey), env.ServiceManagementEndpoint)
		assert.NoError(t, err)

		assert.Equal(t, multiTenantToken, spt)
	})

	t.Run("setup with SP with certificate has no public key", func(t *testing.T) {
		config := &AzureAuthConfig{
			ARMClientConfig: azclient.ARMClientConfig{
				TenantID:                "TenantID",
				NetworkResourceTenantID: "NetworkResourceTenantID",
			},
			AzureAuthConfig: azclient.AzureAuthConfig{
				AADClientID:       "AADClientID",
				AADClientCertPath: "./testdata/testnopublickey.pem",
			},
			NetworkResourceSubscriptionID: "NetworkResourceSubscriptionID",
		}
		env := &azure.PublicCloud
		_, err := GetMultiTenantServicePrincipalToken(config, env, nil)
		assert.Error(t, err)
	})

	t.Run("setup with MSI and auxiliary token provider", func(t *testing.T) {
		const (
			managedIdentityToken = "managed-identity-token"
			networkToken         = "network-token"
		)
		var (
			cfg = &AzureAuthConfig{
				AzureAuthConfig: azclient.AzureAuthConfig{
					UseManagedIdentityExtension: true,
				},
				ARMClientConfig: azclient.ARMClientConfig{
					TenantID:                "TenantID",
					NetworkResourceTenantID: "NetworkResourceTenantID",
				},
			}
			authProvider = &azclient.AuthProvider{
				ManagedIdentityCredential: NewDummyTokenCredential(managedIdentityToken),
				NetworkTokenCredential:    NewDummyTokenCredential(networkToken),
				ClientOptions: &policy.ClientOptions{
					Cloud: cloud.AzurePublic,
				},
			}
		)

		token, err := GetMultiTenantServicePrincipalToken(cfg, &azure.PublicCloud, authProvider)
		assert.NoError(t, err)

		assert.Equal(t, managedIdentityToken, token.PrimaryOAuthToken())
		auxTokens := token.AuxiliaryOAuthTokens()
		assert.Len(t, auxTokens, 1)
		assert.Equal(t, networkToken, auxTokens[0])
	})

	t.Run("invalid config", func(t *testing.T) {
		env := &azure.PublicCloud
		for _, config := range CrossTenantNetworkResourceNegativeConfig {
			_, err := GetMultiTenantServicePrincipalToken(config, env, nil)
			assert.Error(t, err)
		}
	})
}

func TestGetNetworkResourceServicePrincipalToken(t *testing.T) {

	t.Run("setup with SP with password", func(t *testing.T) {
		config := &AzureAuthConfig{
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
		env := &azure.PublicCloud

		token, err := GetNetworkResourceServicePrincipalToken(config, env, nil)
		assert.NoError(t, err)

		oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(env.ActiveDirectoryEndpoint, config.NetworkResourceTenantID, nil)
		assert.NoError(t, err)

		spt, err := adal.NewServicePrincipalToken(*oauthConfig, config.AADClientID, config.AADClientSecret, env.ServiceManagementEndpoint)
		assert.NoError(t, err)

		assert.Equal(t, token, spt)
	})

	t.Run("setup with SP with certificate", func(t *testing.T) {
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

		token, err := GetNetworkResourceServicePrincipalToken(config, env, nil)
		assert.NoError(t, err)

		oauthConfig, err := adal.NewOAuthConfigWithAPIVersion(env.ActiveDirectoryEndpoint, config.NetworkResourceTenantID, nil)
		assert.NoError(t, err)

		pfxContent, err := os.ReadFile("./testdata/testnopassword.pfx")
		assert.NoError(t, err)
		certificates, privateKey, err := azidentity.ParseCertificates(pfxContent, nil)
		assert.NoError(t, err)
		spt, err := adal.NewServicePrincipalTokenFromCertificate(*oauthConfig, config.AADClientID, certificates[0], privateKey.(*rsa.PrivateKey), env.ServiceManagementEndpoint)
		assert.NoError(t, err)

		assert.Equal(t, token, spt)
	})

	t.Run("setup with SP with certificate has no public key", func(t *testing.T) {
		config := &AzureAuthConfig{
			ARMClientConfig: azclient.ARMClientConfig{
				TenantID:                "TenantID",
				NetworkResourceTenantID: "NetworkResourceTenantID",
			},
			AzureAuthConfig: azclient.AzureAuthConfig{
				AADClientID:       "AADClientID",
				AADClientCertPath: "./testdata/testnopublickey.pem",
			},
			NetworkResourceSubscriptionID: "NetworkResourceSubscriptionID",
		}
		env := &azure.PublicCloud

		_, err := GetNetworkResourceServicePrincipalToken(config, env, nil)
		assert.Error(t, err)
	})

	t.Run("setup with MSI and auxiliary token provider", func(t *testing.T) {
		const (
			managedIdentityToken = "managed-identity-token"
			networkToken         = "network-token"
		)
		var (
			cfg = &AzureAuthConfig{
				AzureAuthConfig: azclient.AzureAuthConfig{
					UseManagedIdentityExtension: true,
				},
				ARMClientConfig: azclient.ARMClientConfig{
					TenantID:                "TenantID",
					NetworkResourceTenantID: "NetworkResourceTenantID",
				},
			}
			authProvider = &azclient.AuthProvider{
				ManagedIdentityCredential: NewDummyTokenCredential(managedIdentityToken),
				NetworkTokenCredential:    NewDummyTokenCredential(networkToken),
				ClientOptions: &policy.ClientOptions{
					Cloud: cloud.AzurePublic,
				},
			}
		)

		token, err := GetNetworkResourceServicePrincipalToken(cfg, &azure.PublicCloud, authProvider)
		assert.NoError(t, err)
		assert.Equal(t, networkToken, token.OAuthToken())
	})

	t.Run("invalid config", func(t *testing.T) {
		env := &azure.PublicCloud
		for _, config := range CrossTenantNetworkResourceNegativeConfig {
			_, err := GetNetworkResourceServicePrincipalToken(config, env, nil)
			assert.Error(t, err)
		}
	})
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
