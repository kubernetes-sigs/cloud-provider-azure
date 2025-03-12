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
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/msi-dataplane/pkg/dataplane"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/armauth"
)

type AuthProvider struct {
	ComputeCredential              azcore.TokenCredential
	AdditionalComputeClientOptions []func(option *arm.ClientOptions)
	NetworkCredential              azcore.TokenCredential
	CloudConfig                    cloud.Configuration
}

func NewAuthProvider(
	armConfig *ARMClientConfig,
	config *AzureAuthConfig,
	clientOptionsMutFn ...func(option *policy.ClientOptions),
) (*AuthProvider, error) {
	clientOption, _, err := GetAzCoreClientOption(armConfig)
	if err != nil {
		return nil, err
	}
	for _, fn := range clientOptionsMutFn {
		fn(clientOption)
	}
	var (
		computeCredential              azcore.TokenCredential
		networkCredential              azcore.TokenCredential
		additionalComputeClientOptions []func(option *arm.ClientOptions)
	)

	// federatedIdentityCredential is used for workload identity federation
	if aadFederatedTokenFile, enabled := config.GetAzureFederatedTokenFile(); enabled {
		computeCredential, err = azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientOptions: *clientOption,
			ClientID:      config.GetAADClientID(),
			TenantID:      armConfig.GetTenantID(),
			TokenFilePath: aadFederatedTokenFile,
		})
		if err != nil {
			return nil, err
		}
	}

	// managedIdentityCredential is used for managed identity extension
	if computeCredential == nil && config.UseManagedIdentityExtension {
		credOptions := &azidentity.ManagedIdentityCredentialOptions{
			ClientOptions: *clientOption,
		}
		if len(config.UserAssignedIdentityID) > 0 {
			if strings.Contains(strings.ToUpper(config.UserAssignedIdentityID), "/SUBSCRIPTIONS/") {
				credOptions.ID = azidentity.ResourceID(config.UserAssignedIdentityID)
			} else {
				credOptions.ID = azidentity.ClientID(config.UserAssignedIdentityID)
			}
		}
		computeCredential, err = azidentity.NewManagedIdentityCredential(credOptions)
		if err != nil {
			return nil, err
		}
		if config.AuxiliaryTokenProvider != nil && IsMultiTenant(armConfig) {
			// Use AuxiliaryTokenProvider as the network credential
			networkCredential, err = armauth.NewKeyVaultCredential(
				computeCredential,
				config.AuxiliaryTokenProvider.SecretResourceID(),
			)
			if err != nil {
				return nil, fmt.Errorf("create KeyVaultCredential for auxiliary token provider: %w", err)
			}

			// Additionally, we need to add the auxiliary token to the HTTP header when making requests to the compute resources
			additionalComputeClientOptions = append(additionalComputeClientOptions, func(option *arm.ClientOptions) {
				option.PerRetryPolicies = append(option.PerRetryPolicies, armauth.NewAuxiliaryAuthPolicy(
					[]azcore.TokenCredential{networkCredential},
					DefaultTokenScopeFor(clientOption.Cloud),
				))
			})
		}
	}

	// Client secret authentication
	if computeCredential == nil && len(config.GetAADClientSecret()) > 0 {
		if IsMultiTenant(armConfig) {

			// Network credential for network resource access
			{
				credOptions := &azidentity.ClientSecretCredentialOptions{
					ClientOptions: *clientOption,
				}
				networkCredential, err = azidentity.NewClientSecretCredential(
					armConfig.NetworkResourceTenantID,
					config.GetAADClientID(),
					config.GetAADClientSecret(),
					credOptions,
				)
				if err != nil {
					return nil, err
				}
			}

			// Compute credential with additional allowed tenants for cross-tenant access
			{
				credOptions := &azidentity.ClientSecretCredentialOptions{
					ClientOptions:              *clientOption,
					AdditionallyAllowedTenants: []string{armConfig.NetworkResourceTenantID},
				}
				computeCredential, err = azidentity.NewClientSecretCredential(
					armConfig.GetTenantID(),
					config.GetAADClientID(),
					config.GetAADClientSecret(),
					credOptions,
				)
				if err != nil {
					return nil, err
				}
			}
		} else {
			// Single tenant
			credOptions := &azidentity.ClientSecretCredentialOptions{
				ClientOptions: *clientOption,
			}
			computeCredential, err = azidentity.NewClientSecretCredential(
				armConfig.GetTenantID(),
				config.GetAADClientID(),
				config.GetAADClientSecret(),
				credOptions,
			)
			if err != nil {
				return nil, err
			}
		}
	}

	// ClientCertificateCredential is used for client certificate
	if computeCredential == nil && len(config.AADClientCertPath) > 0 {
		certData, err := os.ReadFile(config.AADClientCertPath)
		if err != nil {
			return nil, fmt.Errorf("reading the client certificate from file %s: %w", config.AADClientCertPath, err)
		}
		certificate, privateKey, err := azidentity.ParseCertificates(certData, []byte(config.AADClientCertPassword))
		if err != nil {
			return nil, fmt.Errorf("decoding the client certificate: %w", err)
		}

		if IsMultiTenant(armConfig) {

			// Network credential for network resource access
			{
				credOptions := &azidentity.ClientCertificateCredentialOptions{
					ClientOptions:        *clientOption,
					SendCertificateChain: true,
				}
				networkCredential, err = azidentity.NewClientCertificateCredential(
					armConfig.NetworkResourceTenantID,
					config.GetAADClientID(),
					certificate,
					privateKey,
					credOptions,
				)
				if err != nil {
					return nil, err
				}
			}

			// Compute credential with additional allowed tenants for cross-tenant access
			{
				credOptions := &azidentity.ClientCertificateCredentialOptions{
					ClientOptions:              *clientOption,
					AdditionallyAllowedTenants: []string{armConfig.NetworkResourceTenantID},
					SendCertificateChain:       true,
				}
				computeCredential, err = azidentity.NewClientCertificateCredential(
					armConfig.GetTenantID(),
					config.GetAADClientID(),
					certificate,
					privateKey,
					credOptions,
				)
				if err != nil {
					return nil, err
				}
			}
		} else {
			// Single tenant
			credOptions := &azidentity.ClientCertificateCredentialOptions{
				ClientOptions:        *clientOption,
				SendCertificateChain: true,
			}
			computeCredential, err = azidentity.NewClientCertificateCredential(
				armConfig.GetTenantID(),
				config.GetAADClientID(),
				certificate,
				privateKey,
				credOptions,
			)
			if err != nil {
				return nil, err
			}
		}
	}

	// UserAssignedIdentityCredentials authentication
	if computeCredential == nil && len(config.AADMSIDataPlaneIdentityPath) > 0 {
		computeCredential, err = dataplane.NewUserAssignedIdentityCredential(
			context.Background(),
			config.AADMSIDataPlaneIdentityPath,
			dataplane.WithClientOpts(azcore.ClientOptions{Cloud: clientOption.Cloud}),
		)
		if err != nil {
			return nil, err
		}
	}

	return &AuthProvider{
		ComputeCredential:              computeCredential,
		AdditionalComputeClientOptions: additionalComputeClientOptions,
		NetworkCredential:              networkCredential,
		CloudConfig:                    clientOption.Cloud,
	}, nil
}

func (factory *AuthProvider) GetAzIdentity() azcore.TokenCredential {
	return factory.ComputeCredential
}

func (factory *AuthProvider) GetNetworkAzIdentity() azcore.TokenCredential {
	if factory.NetworkCredential != nil {
		return factory.NetworkCredential
	}
	return factory.ComputeCredential
}

func (factory *AuthProvider) DefaultTokenScope() string {
	return DefaultTokenScopeFor(factory.CloudConfig)
}

func DefaultTokenScopeFor(cloudCfg cloud.Configuration) string {
	audience := cloudCfg.Services[cloud.ResourceManager].Audience
	return fmt.Sprintf("%s/.default", strings.TrimRight(audience, "/"))
}
