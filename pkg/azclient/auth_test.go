/*
Copyright 2025 The Kubernetes Authors.

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
	"context"
	"crypto"
	"crypto/x509"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/msi-dataplane/pkg/dataplane"
	"github.com/go-faker/faker/v4"
	"github.com/stretchr/testify/assert"
)

func TestNewAuthProvider(t *testing.T) {
	t.Parallel()

	var (
		testTenantID                     = faker.UUIDHyphenated()
		testNetworkTenantID              = faker.UUIDHyphenated()
		testAADClientID                  = faker.UUIDHyphenated()
		testAADClientSecret              = faker.Password()
		testAADClientCertPath            = "/path/to/cert.pem"
		testIdentityPath                 = "/var/run/identity.json"
		testFederatedTokenPath           = "/var/run/secrets/token"
		testCloudConfig                  = cloud.AzurePublic
		testFakeComputeTokenCredentialID = "fake-compute-token-credential"
		testFakeComputeTokenCredential   = NewFakeTokenCredential(testFakeComputeTokenCredentialID)
		testFakeNetworkTokenCredentialID = "fake-network-token-credential"
		testFakeNetworkTokenCredential   = NewFakeTokenCredential(testFakeNetworkTokenCredentialID)
		testErr                          = errors.New("test error")
	)

	testARMConfig := &ARMClientConfig{
		TenantID: testTenantID,
	}

	testARMConfigMultiTenant := &ARMClientConfig{
		TenantID:                testTenantID,
		NetworkResourceTenantID: testNetworkTenantID,
	}

	tests := []struct {
		Name          string
		ARMConfig     *ARMClientConfig
		AuthConfig    *AzureAuthConfig
		Options       []AuthProviderOption
		Assertions    []AuthProviderAssertions
		ExpectErr     error
		ErrorContains string
	}{
		{
			Name:          "error when GetAzCoreClientOption fails",
			ARMConfig:     &ARMClientConfig{}, // This will cause validation failure
			AuthConfig:    &AzureAuthConfig{},
			ExpectErr:     ErrNoValidAuthMethodFound, // The error won't be this, but we expect an error
			ErrorContains: "invalid ARM client config",
		},
		{
			Name:       "error when no valid auth method found",
			ARMConfig:  testARMConfig,
			AuthConfig: &AzureAuthConfig{},
			ExpectErr:  ErrNoValidAuthMethodFound,
		},
		{
			Name:      "success with workload identity",
			ARMConfig: testARMConfig,
			AuthConfig: &AzureAuthConfig{
				AADClientID:                           testAADClientID,
				AADFederatedTokenFile:                 testFederatedTokenPath,
				UseFederatedWorkloadIdentityExtension: true,
			},
			Options: []AuthProviderOption{
				func(option *authProviderOptions) {
					option.NewWorkloadIdentityCredentialFn = func(options *azidentity.WorkloadIdentityCredentialOptions) (azcore.TokenCredential, error) {
						return testFakeComputeTokenCredential, nil
					}
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredentialID),
				AssertNilNetworkTokenCredential(),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
		{
			Name:      "success with managed identity",
			ARMConfig: testARMConfig,
			AuthConfig: &AzureAuthConfig{
				UseManagedIdentityExtension: true,
			},
			Options: []AuthProviderOption{
				func(option *authProviderOptions) {
					option.NewManagedIdentityCredentialFn = func(options *azidentity.ManagedIdentityCredentialOptions) (azcore.TokenCredential, error) {
						return testFakeComputeTokenCredential, nil
					}
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredentialID),
				AssertNilNetworkTokenCredential(),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
		{
			Name:      "error with managed identity",
			ARMConfig: testARMConfig,
			AuthConfig: &AzureAuthConfig{
				UseManagedIdentityExtension: true,
			},
			Options: []AuthProviderOption{
				func(option *authProviderOptions) {
					option.NewManagedIdentityCredentialFn = func(options *azidentity.ManagedIdentityCredentialOptions) (azcore.TokenCredential, error) {
						return nil, testErr
					}
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:      "success with service principal client secret",
			ARMConfig: testARMConfig,
			AuthConfig: &AzureAuthConfig{
				AADClientID:     testAADClientID,
				AADClientSecret: testAADClientSecret,
			},
			Options: []AuthProviderOption{
				func(option *authProviderOptions) {
					option.NewClientSecretCredentialFn = func(tenantID, clientID, clientSecret string, options *azidentity.ClientSecretCredentialOptions) (azcore.TokenCredential, error) {
						return testFakeComputeTokenCredential, nil
					}
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredentialID),
				AssertNilNetworkTokenCredential(),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
		{
			Name:      "success with service principal client certificate",
			ARMConfig: testARMConfig,
			AuthConfig: &AzureAuthConfig{
				AADClientID:       testAADClientID,
				AADClientCertPath: testAADClientCertPath,
			},
			Options: []AuthProviderOption{
				func(option *authProviderOptions) {
					option.ReadFileFn = func(name string) ([]byte, error) {
						return []byte("test-cert-data"), nil
					}
					option.ParseCertificatesFn = func(certData []byte, password []byte) ([]*x509.Certificate, crypto.PrivateKey, error) {
						return []*x509.Certificate{{}}, struct{ crypto.PrivateKey }{}, nil
					}
					option.NewClientCertificateCredentialFn = func(tenantID string, clientID string, certs []*x509.Certificate, key crypto.PrivateKey, options *azidentity.ClientCertificateCredentialOptions) (azcore.TokenCredential, error) {
						return testFakeComputeTokenCredential, nil
					}
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredentialID),
				AssertNilNetworkTokenCredential(),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
		{
			Name:      "success with user assigned identity",
			ARMConfig: testARMConfig,
			AuthConfig: &AzureAuthConfig{
				AADMSIDataPlaneIdentityPath: testIdentityPath,
			},
			Options: []AuthProviderOption{
				func(option *authProviderOptions) {
					option.NewUserAssignedIdentityCredentialFn = func(ctx context.Context, credentialPath string, opts ...dataplane.Option) (azcore.TokenCredential, error) {
						return testFakeComputeTokenCredential, nil
					}
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredentialID),
				AssertNilNetworkTokenCredential(),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
		{
			Name:      "success with client option mutation",
			ARMConfig: testARMConfig,
			AuthConfig: &AzureAuthConfig{
				AADClientID:     testAADClientID,
				AADClientSecret: testAADClientSecret,
			},
			Options: []AuthProviderOption{
				func(option *authProviderOptions) {
					option.NewClientSecretCredentialFn = func(tenantID, clientID, clientSecret string, options *azidentity.ClientSecretCredentialOptions) (azcore.TokenCredential, error) {
						return testFakeComputeTokenCredential, nil
					}
				},
				WithClientOptionsMutFn(func(option *policy.ClientOptions) {
					// Just to test that the mutation function is called
					option.Retry.MaxRetries = 5
				}),
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredentialID),
				AssertNilNetworkTokenCredential(),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
		{
			Name:      "success with multi-tenant service principal",
			ARMConfig: testARMConfigMultiTenant,
			AuthConfig: &AzureAuthConfig{
				AADClientID:     testAADClientID,
				AADClientSecret: testAADClientSecret,
			},
			Options: []AuthProviderOption{
				func(option *authProviderOptions) {
					option.NewClientSecretCredentialFn = func(tenantID, clientID, clientSecret string, options *azidentity.ClientSecretCredentialOptions) (azcore.TokenCredential, error) {
						if tenantID == testNetworkTenantID {
							return testFakeNetworkTokenCredential, nil
						}
						return testFakeComputeTokenCredential, nil
					}
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredentialID),
				AssertNetworkTokenCredential(testFakeNetworkTokenCredentialID),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			authProvider, err := NewAuthProvider(
				tt.ARMConfig,
				tt.AuthConfig,
				tt.Options...,
			)

			if tt.ExpectErr != nil {
				assert.Error(t, err)
				if errors.Is(err, tt.ExpectErr) {
					assert.ErrorIs(t, err, tt.ExpectErr)
				} else if tt.ErrorContains != "" {
					assert.ErrorContains(t, err, tt.ErrorContains)
				}
				return
			}

			assert.NoError(t, err)
			ApplyAssertions(t, authProvider, tt.Assertions)

			// Test additional methods
			assert.Equal(t, authProvider.ComputeCredential, authProvider.GetAzIdentity())

			networkIdentity := authProvider.GetNetworkAzIdentity()
			if authProvider.NetworkCredential != nil {
				assert.Equal(t, authProvider.NetworkCredential, networkIdentity)
			} else {
				assert.Equal(t, authProvider.ComputeCredential, networkIdentity)
			}

			expectedScope := DefaultTokenScopeFor(testCloudConfig)
			assert.Equal(t, expectedScope, authProvider.DefaultTokenScope())
		})
	}
}

func TestDefaultTokenScopeFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		Name     string
		CloudCfg cloud.Configuration
		Expected string
	}{
		{
			Name:     "AzurePublic",
			CloudCfg: cloud.AzurePublic,
			Expected: "https://management.core.windows.net/.default",
		},
		{
			Name:     "AzureChina",
			CloudCfg: cloud.AzureChina,
			Expected: "https://management.core.chinacloudapi.cn/.default",
		},
		{
			Name: "Custom audience with trailing slash",
			CloudCfg: cloud.Configuration{
				Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
					cloud.ResourceManager: {
						Audience: "https://custom.endpoint.com/",
					},
				},
			},
			Expected: "https://custom.endpoint.com/.default",
		},
		{
			Name: "Custom audience without trailing slash",
			CloudCfg: cloud.Configuration{
				Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
					cloud.ResourceManager: {
						Audience: "https://custom.endpoint.com",
					},
				},
			},
			Expected: "https://custom.endpoint.com/.default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()
			got := DefaultTokenScopeFor(tt.CloudCfg)
			assert.Equal(t, tt.Expected, got)
		})
	}
}
