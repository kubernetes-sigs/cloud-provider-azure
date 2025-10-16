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

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/armauth"
)

func TestNewAuthProviderWithWorkloadIdentity(t *testing.T) {
	t.Parallel()

	var (
		testAADClientID   = faker.UUIDHyphenated()
		testTenantID      = faker.UUIDHyphenated()
		testTokenFileName = faker.Word()
		testARMConfig     = &ARMClientConfig{
			TenantID: testTenantID,
		}
		testAzureAuthConfig = &AzureAuthConfig{
			AADClientID: testAADClientID,
		}
		testCloudConfig                = cloud.AzurePublic
		testClientOption               = &policy.ClientOptions{Cloud: testCloudConfig}
		testFakeComputeTokenCredential = newFakeTokenCredential()
		testErr                        = errors.New("test error")
	)

	tests := []struct {
		Name                  string
		AADFederatedTokenFile string
		ARMConfig             *ARMClientConfig
		AuthConfig            *AzureAuthConfig
		ClientOption          *policy.ClientOptions
		Opts                  *authProviderOptions
		Assertions            []AuthProviderAssertions
		ExpectErr             error
	}{
		{
			Name:                  "error when creating workload identity credential",
			AADFederatedTokenFile: testTokenFileName,
			ARMConfig:             testARMConfig,
			AuthConfig:            testAzureAuthConfig,
			ClientOption:          testClientOption,
			Opts: &authProviderOptions{
				NewWorkloadIdentityCredentialFn: func(_ *azidentity.WorkloadIdentityCredentialOptions) (azcore.TokenCredential, error) {
					return nil, testErr
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:                  "success",
			AADFederatedTokenFile: testTokenFileName,
			ARMConfig:             testARMConfig,
			AuthConfig:            testAzureAuthConfig,
			ClientOption:          testClientOption,
			Opts: &authProviderOptions{
				NewWorkloadIdentityCredentialFn: func(options *azidentity.WorkloadIdentityCredentialOptions) (azcore.TokenCredential, error) {
					assert.Equal(t, *testClientOption, options.ClientOptions)
					assert.Equal(t, testAADClientID, options.ClientID)
					assert.Equal(t, testTenantID, options.TenantID)
					assert.Equal(t, testTokenFileName, options.TokenFilePath)

					return testFakeComputeTokenCredential, nil
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredential),
				AssertNilNetworkTokenCredential(),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			authProvider, err := newAuthProviderWithWorkloadIdentity(
				tt.AADFederatedTokenFile,
				tt.ARMConfig,
				tt.AuthConfig,
				tt.ClientOption,
				tt.Opts,
			)

			if tt.ExpectErr != nil {
				assert.Error(t, err)
				assert.ErrorIs(t, err, tt.ExpectErr)
			} else {
				assert.NoError(t, err)
				ApplyAssertions(t, authProvider, tt.Assertions)
			}
		})
	}
}

func TestNewAuthProviderWithManagedIdentity(t *testing.T) {
	t.Parallel()

	var (
		testTenantID               = faker.UUIDHyphenated()
		testNetworkTenantID        = faker.UUIDHyphenated()
		testUserAssignedIdentityID = faker.UUIDHyphenated()
		testARMConfig              = &ARMClientConfig{
			TenantID: testTenantID,
		}
		testARMConfigMultiTenant = &ARMClientConfig{
			TenantID:                testTenantID,
			NetworkResourceTenantID: testNetworkTenantID,
		}
		testAzureAuthConfig = &AzureAuthConfig{
			UserAssignedIdentityID: testUserAssignedIdentityID,
		}
		testAzureAuthConfigWithAuxiliaryProvider = &AzureAuthConfig{
			UserAssignedIdentityID: testUserAssignedIdentityID,
			AuxiliaryTokenProvider: &AzureAuthAuxiliaryTokenProvider{
				SubscriptionID: faker.UUIDHyphenated(),
				ResourceGroup:  faker.Word(),
				VaultName:      faker.Word(),
				SecretName:     faker.Word(),
			},
		}
		testCloudConfig                = cloud.AzurePublic
		testClientOption               = &policy.ClientOptions{Cloud: testCloudConfig}
		testFakeComputeTokenCredential = newFakeTokenCredential()
		testFakeNetworkTokenCredential = newFakeTokenCredential()
		testErr                        = errors.New("test error")
	)

	tests := []struct {
		Name         string
		ARMConfig    *ARMClientConfig
		AuthConfig   *AzureAuthConfig
		ClientOption *policy.ClientOptions
		Opts         *authProviderOptions
		Assertions   []AuthProviderAssertions
		ExpectErr    error
	}{
		{
			Name:         "error when creating managed identity credential",
			ARMConfig:    testARMConfig,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewManagedIdentityCredentialFn: func(_ *azidentity.ManagedIdentityCredentialOptions) (azcore.TokenCredential, error) {
					return nil, testErr
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:         "success with single tenant",
			ARMConfig:    testARMConfig,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewManagedIdentityCredentialFn: func(options *azidentity.ManagedIdentityCredentialOptions) (azcore.TokenCredential, error) {
					assert.Equal(t, *testClientOption, options.ClientOptions)
					assert.Equal(t, azidentity.ClientID(testUserAssignedIdentityID), options.ID)
					return testFakeComputeTokenCredential, nil
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredential),
				AssertNilNetworkTokenCredential(),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
		{
			Name:         "error with multi-tenant and no auxiliary token provider",
			ARMConfig:    testARMConfigMultiTenant,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewManagedIdentityCredentialFn: func(_ *azidentity.ManagedIdentityCredentialOptions) (azcore.TokenCredential, error) {
					return testFakeComputeTokenCredential, nil
				},
			},
			ExpectErr: ErrAuxiliaryTokenProviderNotSet,
		},
		{
			Name:         "error with multi-tenant when creating KeyVault credential",
			ARMConfig:    testARMConfigMultiTenant,
			AuthConfig:   testAzureAuthConfigWithAuxiliaryProvider,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewManagedIdentityCredentialFn: func(_ *azidentity.ManagedIdentityCredentialOptions) (azcore.TokenCredential, error) {
					return testFakeComputeTokenCredential, nil
				},
				NewKeyVaultCredentialFn: func(_ azcore.TokenCredential, _ armauth.SecretResourceID) (azcore.TokenCredential, error) {
					return nil, testErr
				},
			},
			ExpectErr: ErrNewKeyVaultCredentialFailed,
		},
		{
			Name:         "success with multi-tenant",
			ARMConfig:    testARMConfigMultiTenant,
			AuthConfig:   testAzureAuthConfigWithAuxiliaryProvider,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewManagedIdentityCredentialFn: func(options *azidentity.ManagedIdentityCredentialOptions) (azcore.TokenCredential, error) {
					assert.Equal(t, *testClientOption, options.ClientOptions)
					assert.Equal(t, azidentity.ClientID(testUserAssignedIdentityID), options.ID)
					return testFakeComputeTokenCredential, nil
				},
				NewKeyVaultCredentialFn: func(credential azcore.TokenCredential, secretResourceID armauth.SecretResourceID) (azcore.TokenCredential, error) {
					assert.Equal(t, testFakeComputeTokenCredential, credential)
					assert.Equal(t, testAzureAuthConfigWithAuxiliaryProvider.AuxiliaryTokenProvider.SecretResourceID(), secretResourceID)
					return testFakeNetworkTokenCredential, nil
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredential),
				AssertNetworkTokenCredential(testFakeNetworkTokenCredential),
				AssertCloudConfig(testCloudConfig),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			authProvider, err := newAuthProviderWithManagedIdentity(
				tt.ARMConfig,
				tt.AuthConfig,
				tt.ClientOption,
				tt.Opts,
			)

			if tt.ExpectErr != nil {
				assert.Error(t, err)
				if errors.Is(err, tt.ExpectErr) {
					assert.ErrorIs(t, err, tt.ExpectErr)
				} else {
					assert.ErrorContains(t, err, tt.ExpectErr.Error())
				}
			} else {
				assert.NoError(t, err)
				ApplyAssertions(t, authProvider, tt.Assertions)
			}
		})
	}
}

func TestNewAuthProviderWithServicePrincipalClientSecret(t *testing.T) {
	t.Parallel()

	var (
		testAADClientID     = faker.UUIDHyphenated()
		testAADClientSecret = faker.Password()
		testTenantID        = faker.UUIDHyphenated()
		testNetworkTenantID = faker.UUIDHyphenated()
		testARMConfig       = &ARMClientConfig{
			TenantID: testTenantID,
		}
		testARMConfigMultiTenant = &ARMClientConfig{
			TenantID:                testTenantID,
			NetworkResourceTenantID: testNetworkTenantID,
		}
		testAzureAuthConfig = &AzureAuthConfig{
			AADClientID:     testAADClientID,
			AADClientSecret: testAADClientSecret,
		}
		testCloudConfig                = cloud.AzurePublic
		testClientOption               = &policy.ClientOptions{Cloud: testCloudConfig}
		testFakeComputeTokenCredential = newFakeTokenCredential()
		testFakeNetworkTokenCredential = newFakeTokenCredential()
		testErr                        = errors.New("test error")
	)

	tests := []struct {
		Name         string
		ARMConfig    *ARMClientConfig
		AuthConfig   *AzureAuthConfig
		ClientOption *policy.ClientOptions
		Opts         *authProviderOptions
		Assertions   []AuthProviderAssertions
		ExpectErr    error
	}{
		{
			Name:         "error when creating client secret credential in single tenant",
			ARMConfig:    testARMConfig,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewClientSecretCredentialFn: func(_ string, _ string, _ string, _ *azidentity.ClientSecretCredentialOptions) (azcore.TokenCredential, error) {
					return nil, testErr
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:         "success with single tenant",
			ARMConfig:    testARMConfig,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewClientSecretCredentialFn: func(tenantID, clientID, clientSecret string, options *azidentity.ClientSecretCredentialOptions) (azcore.TokenCredential, error) {
					assert.Equal(t, testTenantID, tenantID)
					assert.Equal(t, testAADClientID, clientID)
					assert.Equal(t, testAADClientSecret, clientSecret)
					assert.Equal(t, *testClientOption, options.ClientOptions)
					assert.Empty(t, options.AdditionallyAllowedTenants)
					return testFakeComputeTokenCredential, nil
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredential),
				AssertNilNetworkTokenCredential(),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
		{
			Name:         "error when creating network credential in multi-tenant",
			ARMConfig:    testARMConfigMultiTenant,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewClientSecretCredentialFn: func(tenantID, clientID, clientSecret string, options *azidentity.ClientSecretCredentialOptions) (azcore.TokenCredential, error) {
					if tenantID == testNetworkTenantID {
						return nil, testErr
					}
					assert.Equal(t, testTenantID, tenantID)
					assert.Equal(t, testAADClientID, clientID)
					assert.Equal(t, testAADClientSecret, clientSecret)
					assert.Equal(t, *testClientOption, options.ClientOptions)
					assert.Contains(t, options.AdditionallyAllowedTenants, testNetworkTenantID)
					return testFakeComputeTokenCredential, nil
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:         "error when creating compute credential in multi-tenant",
			ARMConfig:    testARMConfigMultiTenant,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewClientSecretCredentialFn: func(tenantID, clientID, clientSecret string, options *azidentity.ClientSecretCredentialOptions) (azcore.TokenCredential, error) {
					if tenantID == testTenantID {
						return nil, testErr
					}
					assert.Equal(t, testNetworkTenantID, tenantID)
					assert.Equal(t, testAADClientID, clientID)
					assert.Equal(t, testAADClientSecret, clientSecret)
					assert.Equal(t, *testClientOption, options.ClientOptions)
					assert.Empty(t, options.AdditionallyAllowedTenants)
					return testFakeNetworkTokenCredential, nil
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:         "success with multi-tenant",
			ARMConfig:    testARMConfigMultiTenant,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewClientSecretCredentialFn: func(tenantID, clientID, clientSecret string, options *azidentity.ClientSecretCredentialOptions) (azcore.TokenCredential, error) {
					assert.Equal(t, testAADClientID, clientID)
					assert.Equal(t, testAADClientSecret, clientSecret)
					assert.Equal(t, *testClientOption, options.ClientOptions)

					switch tenantID {
					case testNetworkTenantID:
						assert.Empty(t, options.AdditionallyAllowedTenants)
						return testFakeNetworkTokenCredential, nil
					case testTenantID:
						assert.Contains(t, options.AdditionallyAllowedTenants, testNetworkTenantID)
						return testFakeComputeTokenCredential, nil
					default:
						t.Fatalf("unexpected tenant ID: %s", tenantID)
						return nil, nil
					}
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredential),
				AssertNetworkTokenCredential(testFakeNetworkTokenCredential),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			authProvider, err := newAuthProviderWithServicePrincipalClientSecret(
				tt.ARMConfig,
				tt.AuthConfig,
				tt.ClientOption,
				tt.Opts,
			)

			if tt.ExpectErr != nil {
				assert.Error(t, err)
				assert.ErrorIs(t, err, tt.ExpectErr)
			} else {
				assert.NoError(t, err)
				ApplyAssertions(t, authProvider, tt.Assertions)
			}
		})
	}
}

func TestNewAuthProviderWithServicePrincipalClientCertificate(t *testing.T) {
	t.Parallel()

	var (
		testAADClientID           = faker.UUIDHyphenated()
		testAADClientCertPath     = faker.Word()
		testAADClientCertPassword = faker.Password()
		testTenantID              = faker.UUIDHyphenated()
		testNetworkTenantID       = faker.UUIDHyphenated()
		testARMConfig             = &ARMClientConfig{
			TenantID: testTenantID,
		}
		testARMConfigMultiTenant = &ARMClientConfig{
			TenantID:                testTenantID,
			NetworkResourceTenantID: testNetworkTenantID,
		}
		testAzureAuthConfig = &AzureAuthConfig{
			AADClientID:           testAADClientID,
			AADClientCertPath:     testAADClientCertPath,
			AADClientCertPassword: testAADClientCertPassword,
		}
		testCloudConfig                = cloud.AzurePublic
		testClientOption               = &policy.ClientOptions{Cloud: testCloudConfig}
		testFakeComputeTokenCredential = newFakeTokenCredential()
		testFakeNetworkTokenCredential = newFakeTokenCredential()
		testErr                        = errors.New("test error")
		testCertData                   = []byte(faker.Word())
		testCerts                      = []*x509.Certificate{{}}
		testPrivateKey                 = struct{ crypto.PrivateKey }{}
	)

	tests := []struct {
		Name         string
		ARMConfig    *ARMClientConfig
		AuthConfig   *AzureAuthConfig
		ClientOption *policy.ClientOptions
		Opts         *authProviderOptions
		Assertions   []AuthProviderAssertions
		ExpectErr    error
	}{
		{
			Name:         "error reading certificate file",
			ARMConfig:    testARMConfig,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				ReadFileFn: func(name string) ([]byte, error) {
					assert.Equal(t, testAADClientCertPath, name)
					return nil, testErr
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:         "error parsing certificate",
			ARMConfig:    testARMConfig,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				ReadFileFn: func(name string) ([]byte, error) {
					assert.Equal(t, testAADClientCertPath, name)
					return testCertData, nil
				},
				ParseCertificatesFn: func(certData []byte, password []byte) ([]*x509.Certificate, crypto.PrivateKey, error) {
					assert.Equal(t, testCertData, certData)
					assert.Equal(t, []byte(testAADClientCertPassword), password)
					return nil, nil, testErr
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:         "error creating client certificate credential in single tenant",
			ARMConfig:    testARMConfig,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				ReadFileFn: func(name string) ([]byte, error) {
					assert.Equal(t, testAADClientCertPath, name)
					return testCertData, nil
				},
				ParseCertificatesFn: func(certData []byte, password []byte) ([]*x509.Certificate, crypto.PrivateKey, error) {
					assert.Equal(t, testCertData, certData)
					assert.Equal(t, []byte(testAADClientCertPassword), password)
					return testCerts, testPrivateKey, nil
				},
				NewClientCertificateCredentialFn: func(tenantID string, clientID string, certs []*x509.Certificate, key crypto.PrivateKey, options *azidentity.ClientCertificateCredentialOptions) (azcore.TokenCredential, error) {
					assert.Equal(t, testTenantID, tenantID)
					assert.Equal(t, testAADClientID, clientID)
					assert.Equal(t, testCerts, certs)
					assert.Equal(t, testPrivateKey, key)
					assert.Equal(t, *testClientOption, options.ClientOptions)
					assert.True(t, options.SendCertificateChain)
					return nil, testErr
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:         "success with single tenant",
			ARMConfig:    testARMConfig,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				ReadFileFn: func(name string) ([]byte, error) {
					assert.Equal(t, testAADClientCertPath, name)
					return testCertData, nil
				},
				ParseCertificatesFn: func(certData []byte, password []byte) ([]*x509.Certificate, crypto.PrivateKey, error) {
					assert.Equal(t, testCertData, certData)
					assert.Equal(t, []byte(testAADClientCertPassword), password)
					return testCerts, testPrivateKey, nil
				},
				NewClientCertificateCredentialFn: func(tenantID string, clientID string, certs []*x509.Certificate, key crypto.PrivateKey, options *azidentity.ClientCertificateCredentialOptions) (azcore.TokenCredential, error) {
					assert.Equal(t, testTenantID, tenantID)
					assert.Equal(t, testAADClientID, clientID)
					assert.Equal(t, testCerts, certs)
					assert.Equal(t, testPrivateKey, key)
					assert.Equal(t, *testClientOption, options.ClientOptions)
					assert.True(t, options.SendCertificateChain)
					assert.Empty(t, options.AdditionallyAllowedTenants)
					return testFakeComputeTokenCredential, nil
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredential),
				AssertNilNetworkTokenCredential(),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
		{
			Name:         "error when creating network credential in multi-tenant",
			ARMConfig:    testARMConfigMultiTenant,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				ReadFileFn: func(name string) ([]byte, error) {
					assert.Equal(t, testAADClientCertPath, name)
					return testCertData, nil
				},
				ParseCertificatesFn: func(certData []byte, password []byte) ([]*x509.Certificate, crypto.PrivateKey, error) {
					assert.Equal(t, testCertData, certData)
					assert.Equal(t, []byte(testAADClientCertPassword), password)
					return testCerts, testPrivateKey, nil
				},
				NewClientCertificateCredentialFn: func(tenantID string, clientID string, certs []*x509.Certificate, key crypto.PrivateKey, options *azidentity.ClientCertificateCredentialOptions) (azcore.TokenCredential, error) {
					if tenantID == testNetworkTenantID {
						assert.Equal(t, testAADClientID, clientID)
						assert.Equal(t, testCerts, certs)
						assert.Equal(t, testPrivateKey, key)
						assert.Equal(t, *testClientOption, options.ClientOptions)
						assert.True(t, options.SendCertificateChain)
						return nil, testErr
					}
					assert.Equal(t, testTenantID, tenantID)
					assert.Equal(t, testAADClientID, clientID)
					assert.Equal(t, testCerts, certs)
					assert.Equal(t, testPrivateKey, key)
					assert.Equal(t, *testClientOption, options.ClientOptions)
					assert.True(t, options.SendCertificateChain)
					return testFakeComputeTokenCredential, nil
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:         "error when creating compute credential in multi-tenant",
			ARMConfig:    testARMConfigMultiTenant,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				ReadFileFn: func(name string) ([]byte, error) {
					assert.Equal(t, testAADClientCertPath, name)
					return testCertData, nil
				},
				ParseCertificatesFn: func(certData []byte, password []byte) ([]*x509.Certificate, crypto.PrivateKey, error) {
					assert.Equal(t, testCertData, certData)
					assert.Equal(t, []byte(testAADClientCertPassword), password)
					return testCerts, testPrivateKey, nil
				},
				NewClientCertificateCredentialFn: func(tenantID string, clientID string, certs []*x509.Certificate, key crypto.PrivateKey, options *azidentity.ClientCertificateCredentialOptions) (azcore.TokenCredential, error) {
					if tenantID == testTenantID {
						return nil, testErr
					}
					assert.Equal(t, testNetworkTenantID, tenantID)
					assert.Equal(t, testAADClientID, clientID)
					assert.Equal(t, testCerts, certs)
					assert.Equal(t, testPrivateKey, key)
					assert.Equal(t, *testClientOption, options.ClientOptions)
					assert.True(t, options.SendCertificateChain)
					return testFakeNetworkTokenCredential, nil
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:         "success with multi-tenant",
			ARMConfig:    testARMConfigMultiTenant,
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				ReadFileFn: func(name string) ([]byte, error) {
					assert.Equal(t, testAADClientCertPath, name)
					return testCertData, nil
				},
				ParseCertificatesFn: func(certData []byte, password []byte) ([]*x509.Certificate, crypto.PrivateKey, error) {
					assert.Equal(t, testCertData, certData)
					assert.Equal(t, []byte(testAADClientCertPassword), password)
					return testCerts, testPrivateKey, nil
				},
				NewClientCertificateCredentialFn: func(tenantID string, clientID string, certs []*x509.Certificate, key crypto.PrivateKey, options *azidentity.ClientCertificateCredentialOptions) (azcore.TokenCredential, error) {
					assert.Equal(t, testAADClientID, clientID)
					assert.Equal(t, testCerts, certs)
					assert.Equal(t, testPrivateKey, key)
					assert.Equal(t, *testClientOption, options.ClientOptions)
					assert.True(t, options.SendCertificateChain)

					switch tenantID {
					case testNetworkTenantID:
						assert.Empty(t, options.AdditionallyAllowedTenants)
						return testFakeNetworkTokenCredential, nil
					case testTenantID:
						assert.Contains(t, options.AdditionallyAllowedTenants, testNetworkTenantID)
						return testFakeComputeTokenCredential, nil
					default:
						t.Fatalf("unexpected tenant ID: %s", tenantID)
						return nil, nil
					}
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredential),
				AssertNetworkTokenCredential(testFakeNetworkTokenCredential),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			authProvider, err := newAuthProviderWithServicePrincipalClientCertificate(
				tt.ARMConfig,
				tt.AuthConfig,
				tt.ClientOption,
				tt.Opts,
			)

			if tt.ExpectErr != nil {
				assert.Error(t, err)
				assert.ErrorContains(t, err, tt.ExpectErr.Error())
			} else {
				assert.NoError(t, err)
				ApplyAssertions(t, authProvider, tt.Assertions)
			}
		})
	}
}

func TestNewAuthProviderWithUserAssignedIdentity(t *testing.T) {
	t.Parallel()

	var (
		testIdentityPath    = "/var/run/identity.json"
		testAzureAuthConfig = &AzureAuthConfig{
			AADMSIDataPlaneIdentityPath: testIdentityPath,
		}
		testCloudConfig                = cloud.AzurePublic
		testClientOption               = &policy.ClientOptions{Cloud: testCloudConfig}
		testFakeComputeTokenCredential = newFakeTokenCredential()
		testErr                        = errors.New("test error")
	)

	tests := []struct {
		Name         string
		AuthConfig   *AzureAuthConfig
		ClientOption *policy.ClientOptions
		Opts         *authProviderOptions
		Assertions   []AuthProviderAssertions
		ExpectErr    error
	}{
		{
			Name:         "error when creating user assigned identity credential",
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewUserAssignedIdentityCredentialFn: func(_ context.Context, _ string, _ ...dataplane.Option) (azcore.TokenCredential, error) {
					return nil, testErr
				},
			},
			ExpectErr: testErr,
		},
		{
			Name:         "success",
			AuthConfig:   testAzureAuthConfig,
			ClientOption: testClientOption,
			Opts: &authProviderOptions{
				NewUserAssignedIdentityCredentialFn: func(ctx context.Context, credentialPath string, opts ...dataplane.Option) (azcore.TokenCredential, error) {
					assert.Equal(t, testIdentityPath, credentialPath)
					// Check that the context is not nil
					assert.NotNil(t, ctx)
					// Verify we have at least one option passed (the cloud configuration)
					assert.NotEmpty(t, opts)
					return testFakeComputeTokenCredential, nil
				},
			},
			Assertions: []AuthProviderAssertions{
				AssertComputeTokenCredential(testFakeComputeTokenCredential),
				AssertNilNetworkTokenCredential(),
				AssertEmptyAdditionalComputeClientOptions(),
				AssertCloudConfig(testCloudConfig),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Parallel()

			authProvider, err := newAuthProviderWithUserAssignedIdentity(
				tt.AuthConfig,
				tt.ClientOption,
				tt.Opts,
			)

			if tt.ExpectErr != nil {
				assert.Error(t, err)
				assert.ErrorIs(t, err, tt.ExpectErr)
			} else {
				assert.NoError(t, err)
				ApplyAssertions(t, authProvider, tt.Assertions)
			}
		})
	}
}
