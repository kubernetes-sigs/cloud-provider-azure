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
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"golang.org/x/crypto/pkcs12"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

// AzureAuthConfig holds auth related part of cloud config
type AzureAuthConfig struct {
	// The AAD Tenant ID for the Subscription that the cluster is deployed in
	TenantID string `json:"tenantId,omitempty" yaml:"tenantId,omitempty"`
	// The ClientID for an AAD application with RBAC access to talk to Azure RM APIs
	AADClientID string `json:"aadClientId,omitempty" yaml:"aadClientId,omitempty"`
	// The ClientSecret for an AAD application with RBAC access to talk to Azure RM APIs
	AADClientSecret string `json:"aadClientSecret,omitempty" yaml:"aadClientSecret,omitempty" datapolicy:"token"`
	// The path of a client certificate for an AAD application with RBAC access to talk to Azure RM APIs
	AADClientCertPath string `json:"aadClientCertPath,omitempty" yaml:"aadClientCertPath,omitempty"`
	// The password of the client certificate for an AAD application with RBAC access to talk to Azure RM APIs
	AADClientCertPassword string `json:"aadClientCertPassword,omitempty" yaml:"aadClientCertPassword,omitempty" datapolicy:"password"`
	// Use managed service identity for the virtual machine to access Azure ARM APIs
	UseManagedIdentityExtension bool `json:"useManagedIdentityExtension,omitempty" yaml:"useManagedIdentityExtension,omitempty"`
	// UserAssignedIdentityID contains the Client ID of the user assigned MSI which is assigned to the underlying VMs. If empty the user assigned identity is not used.
	// More details of the user assigned identity can be found at: https://docs.microsoft.com/en-us/azure/active-directory/managed-service-identity/overview
	// For the user assigned identity specified here to be used, the UseManagedIdentityExtension has to be set to true.
	UserAssignedIdentityID string `json:"userAssignedIdentityID,omitempty" yaml:"userAssignedIdentityID,omitempty"`
	// The AAD Tenant ID for the Subscription that the network resources are deployed in
	NetworkResourceTenantID string `json:"networkResourceTenantID,omitempty" yaml:"networkResourceTenantID,omitempty"`
	// The AAD federated token file
	AADFederatedTokenFile string `json:"aadFederatedTokenFile,omitempty" yaml:"aadFederatedTokenFile,omitempty"`
	// Use workload identity federation for the virtual machine to access Azure ARM APIs
	UseFederatedWorkloadIdentityExtension bool `json:"useFederatedWorkloadIdentityExtension,omitempty" yaml:"useFederatedWorkloadIdentityExtension,omitempty"`
}

var (
	// ErrorNoAuth indicates that no credentials are provided.
	ErrorNoAuth = fmt.Errorf("no credentials provided for Azure cloud provider")
)

type AuthProvider struct {
	AzureAuthConfig
	ARMClientConfig
}

const (
	azureClientID           = "AZURE_CLIENT_ID"
	azureFederatedTokenFile = "AZURE_FEDERATED_TOKEN_FILE"
	azureTenantID           = "AZURE_TENANT_ID"
)

func NewAuthProvider(config AzureAuthConfig, armConfig ARMClientConfig) (*AuthProvider, error) {
	// these environment variables are injected by workload identity webhook
	if tenantID := os.Getenv(azureTenantID); tenantID != "" {
		config.TenantID = tenantID
	}
	if clientID := os.Getenv(azureClientID); clientID != "" {
		config.AADClientID = clientID
	}
	if federatedTokenFile := os.Getenv(azureFederatedTokenFile); federatedTokenFile != "" {
		config.AADFederatedTokenFile = federatedTokenFile
		config.UseFederatedWorkloadIdentityExtension = true
	}
	return &AuthProvider{
		AzureAuthConfig: config,
		ARMClientConfig: armConfig,
	}, nil
}

func (factory *AuthProvider) GetAzIdentity() (azcore.TokenCredential, error) {
	if factory.UseFederatedWorkloadIdentityExtension {
		clientOptions, err := factory.GetDefaultClientOption()
		if err != nil {
			return nil, err
		}
		return azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientOptions: clientOptions.ClientOptions,
			ClientID:      factory.AADClientID,
			TenantID:      factory.TenantID,
			TokenFilePath: factory.AADFederatedTokenFile,
		})
	}

	if factory.UseManagedIdentityExtension {
		clientOptions, err := factory.GetDefaultClientOption()
		if err != nil {
			return nil, err
		}
		credOptions := &azidentity.ManagedIdentityCredentialOptions{
			ClientOptions: clientOptions.ClientOptions,
		}
		if len(factory.UserAssignedIdentityID) > 0 {
			if strings.Contains(strings.ToUpper(factory.UserAssignedIdentityID), "/SUBSCRIPTIONS/") {
				credOptions.ID = azidentity.ResourceID(factory.UserAssignedIdentityID)
			} else {
				credOptions.ID = azidentity.ClientID(factory.UserAssignedIdentityID)
			}
		}
		return azidentity.NewManagedIdentityCredential(credOptions)
	}

	if len(factory.AADClientSecret) > 0 {
		clientOptions, err := factory.GetDefaultClientOption()
		if err != nil {
			return nil, err
		}
		credOptions := &azidentity.ClientSecretCredentialOptions{
			ClientOptions: clientOptions.ClientOptions,
		}
		return azidentity.NewClientSecretCredential(factory.TenantID, factory.AADClientID, factory.AADClientSecret, credOptions)
	}

	if len(factory.AADClientCertPath) > 0 && len(factory.AADClientCertPassword) > 0 {
		clientOptions, err := factory.GetDefaultClientOption()
		if err != nil {
			return nil, err
		}
		credOptions := &azidentity.ClientCertificateCredentialOptions{
			ClientOptions: clientOptions.ClientOptions,
		}
		certData, err := os.ReadFile(factory.AADClientCertPath)
		if err != nil {
			return nil, fmt.Errorf("reading the client certificate from file %s: %w", factory.AADClientCertPath, err)
		}
		certificate, privateKey, err := decodePkcs12(certData, factory.AADClientCertPassword)
		if err != nil {
			return nil, fmt.Errorf("decoding the client certificate: %w", err)
		}
		return azidentity.NewClientCertificateCredential(factory.TenantID, factory.AADClientID, []*x509.Certificate{certificate}, privateKey, credOptions)
	}
	return nil, ErrorNoAuth

}

// decodePkcs12 decodes a PKCS#12 client certificate by extracting the public certificate and
// the private RSA key
func decodePkcs12(pkcs []byte, password string) (*x509.Certificate, *rsa.PrivateKey, error) {
	privateKey, certificate, err := pkcs12.Decode(pkcs, password)
	if err != nil {
		return nil, nil, fmt.Errorf("decoding the PKCS#12 client certificate: %w", err)
	}
	rsaPrivateKey, isRsaKey := privateKey.(*rsa.PrivateKey)
	if !isRsaKey {
		return nil, nil, fmt.Errorf("PKCS#12 certificate must contain a RSA private key")
	}

	return certificate, rsaPrivateKey, nil
}

func (factory *AuthProvider) GetNetworkAzIdentity() (azcore.TokenCredential, error) {
	err := factory.checkConfigWhenNetworkResourceInDifferentTenant()
	if err != nil {
		return nil, fmt.Errorf("got error(%w) in getting network resources service principal token", err)
	}
	if len(factory.AADClientSecret) > 0 {
		clientOptions, err := factory.GetDefaultClientOption()
		if err != nil {
			return nil, err
		}
		credOptions := &azidentity.ClientSecretCredentialOptions{
			ClientOptions: clientOptions.ClientOptions,
		}
		return azidentity.NewClientSecretCredential(factory.NetworkResourceTenantID, factory.AADClientID, factory.AADClientSecret, credOptions)
	}
	if len(factory.AADClientCertPath) > 0 && len(factory.AADClientCertPassword) > 0 {
		return nil, fmt.Errorf("AAD Application client certificate authentication is not supported in getting network resources service principal token")
	}
	return nil, ErrorNoAuth
}

// UsesNetworkResourceInDifferentTenant determines whether the AzureAuthConfig indicates to use network resources in
// different AAD Tenant than those for the cluster. Return true when NetworkResourceTenantID is specified  and not equal
// to one defined in global configs
func (factory *AuthProvider) UsesNetworkResourceInDifferentTenant() bool {
	return len(factory.NetworkResourceTenantID) > 0 && !strings.EqualFold(factory.NetworkResourceTenantID, factory.TenantID)
}

// checkConfigWhenNetworkResourceInDifferentTenant checks configuration for the scenario of using network resource in different tenant
func (factory *AuthProvider) checkConfigWhenNetworkResourceInDifferentTenant() error {
	if !factory.UsesNetworkResourceInDifferentTenant() {
		return fmt.Errorf("NetworkResourceTenantID must be configured")
	}

	if factory.UseManagedIdentityExtension {
		return fmt.Errorf("managed identity is not supported")
	}

	return nil
}

func (factory *AuthProvider) GetMultiTenantIdentity() (azcore.TokenCredential, error) {
	err := factory.checkConfigWhenNetworkResourceInDifferentTenant()
	if err != nil {
		return nil, fmt.Errorf("got error(%w) in getting network resources service principal token", err)
	}

	if len(factory.AADClientSecret) > 0 {
		clientOptions, err := factory.GetDefaultClientOption()
		if err != nil {
			return nil, err
		}
		credOptions := &azidentity.ClientSecretCredentialOptions{
			ClientOptions:              clientOptions.ClientOptions,
			AdditionallyAllowedTenants: []string{factory.NetworkResourceTenantID},
		}
		return azidentity.NewClientSecretCredential(factory.TenantID, factory.AADClientID, factory.AADClientSecret, credOptions)
	}
	if len(factory.AADClientCertPath) > 0 && len(factory.AADClientCertPassword) > 0 {
		return nil, fmt.Errorf("AAD Application client certificate authentication is not supported in getting multi-tenant service principal token")
	}

	return nil, ErrorNoAuth
}

func (factory *AuthProvider) GetDefaultClientOption() (*policy.ClientOptions, error) {

	//Get default settings
	options := utils.GetDefaultOption()

	//update user agent header
	options.ClientOptions.Telemetry.ApplicationID = factory.UserAgent
	//todo: add backoff retry policy

	//set cloud
	var err error
	options.ClientOptions.Cloud, err = AzureCloudConfigFromName(factory.Cloud, factory.ARMClientConfig.ResourceManagerEndpoint)
	return options, err

}
