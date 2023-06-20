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
	// The AAD Tenant ID for the Subscription that the network resources are deployed in.
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
	FederatedIdentityCredential   azcore.TokenCredential
	ManagedIdentityCredential     azcore.TokenCredential
	ClientSecretCredential        azcore.TokenCredential
	NetworkClientSecretCredential azcore.TokenCredential
	MultiTenantCredential         azcore.TokenCredential
	ClientCertificateCredential   azcore.TokenCredential
}

func GetDefaultAuthClientOption(armConfig *ARMClientConfig) (*policy.ClientOptions, error) {
	//Get default settings
	options, err := NewClientOptionFromARMClientConfig(armConfig)
	if err != nil {
		return nil, err
	}
	return options, nil
}

func NewAuthProvider(config AzureAuthConfig, clientOption *policy.ClientOptions) (*AuthProvider, error) {
	if clientOption == nil {
		clientOption = &policy.ClientOptions{}
	}
	// these environment variables are injected by workload identity webhook
	if tenantID := os.Getenv(utils.AzureTenantID); tenantID != "" {
		config.TenantID = tenantID
	}
	if clientID := os.Getenv(utils.AzureClientID); clientID != "" {
		config.AADClientID = clientID
	}
	var err error
	// federatedIdentityCredential is used for workload identity federation
	var federatedIdentityCredential azcore.TokenCredential
	if federatedTokenFile := os.Getenv(utils.AzureFederatedTokenFile); federatedTokenFile != "" {
		config.AADFederatedTokenFile = federatedTokenFile
		config.UseFederatedWorkloadIdentityExtension = true
	}
	if config.UseFederatedWorkloadIdentityExtension {
		federatedIdentityCredential, err = azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientOptions: clientOption.ClientOptions,
			ClientID:      config.AADClientID,
			TenantID:      config.TenantID,
			TokenFilePath: config.AADFederatedTokenFile,
		})
		if err != nil {
			return nil, err
		}
	}

	// managedIdentityCredential is used for managed identity extension
	var managedIdentityCredential azcore.TokenCredential
	if config.UseManagedIdentityExtension {
		credOptions := &azidentity.ManagedIdentityCredentialOptions{
			ClientOptions: clientOption.ClientOptions,
		}
		if len(config.UserAssignedIdentityID) > 0 {
			if strings.Contains(strings.ToUpper(config.UserAssignedIdentityID), "/SUBSCRIPTIONS/") {
				credOptions.ID = azidentity.ResourceID(config.UserAssignedIdentityID)
			} else {
				credOptions.ID = azidentity.ClientID(config.UserAssignedIdentityID)
			}
		}
		managedIdentityCredential, err = azidentity.NewManagedIdentityCredential(credOptions)
		if err != nil {
			return nil, err
		}
	}

	// ClientSecretCredential is used for client secret
	var clientSecretCredential azcore.TokenCredential
	var networkClientSecretCredential azcore.TokenCredential
	var multiTenantCredential azcore.TokenCredential
	if len(config.AADClientSecret) > 0 {
		credOptions := &azidentity.ClientSecretCredentialOptions{
			ClientOptions: clientOption.ClientOptions,
		}
		clientSecretCredential, err = azidentity.NewClientSecretCredential(config.TenantID, config.AADClientID, config.AADClientSecret, credOptions)
		if err != nil {
			return nil, err
		}
		if len(config.NetworkResourceTenantID) > 0 && !strings.EqualFold(config.NetworkResourceTenantID, config.TenantID) {
			credOptions := &azidentity.ClientSecretCredentialOptions{
				ClientOptions: clientOption.ClientOptions,
			}
			networkClientSecretCredential, err = azidentity.NewClientSecretCredential(config.NetworkResourceTenantID, config.AADClientID, config.AADClientSecret, credOptions)
			if err != nil {
				return nil, err
			}

			credOptions = &azidentity.ClientSecretCredentialOptions{
				ClientOptions:              clientOption.ClientOptions,
				AdditionallyAllowedTenants: []string{config.NetworkResourceTenantID},
			}
			multiTenantCredential, err = azidentity.NewClientSecretCredential(config.TenantID, config.AADClientID, config.AADClientSecret, credOptions)
			if err != nil {
				return nil, err
			}

		}
	}

	// ClientCertificateCredential is used for client certificate
	var clientCertificateCredential azcore.TokenCredential
	if len(config.AADClientCertPath) > 0 && len(config.AADClientCertPassword) > 0 {
		credOptions := &azidentity.ClientCertificateCredentialOptions{
			ClientOptions: clientOption.ClientOptions,
		}
		certData, err := os.ReadFile(config.AADClientCertPath)
		if err != nil {
			return nil, fmt.Errorf("reading the client certificate from file %s: %w", config.AADClientCertPath, err)
		}
		certificate, privateKey, err := decodePkcs12(certData, config.AADClientCertPassword)
		if err != nil {
			return nil, fmt.Errorf("decoding the client certificate: %w", err)
		}
		clientCertificateCredential, err = azidentity.NewClientCertificateCredential(config.TenantID, config.AADClientID, []*x509.Certificate{certificate}, privateKey, credOptions)
		if err != nil {
			return nil, err
		}
	}

	return &AuthProvider{
		FederatedIdentityCredential:   federatedIdentityCredential,
		ManagedIdentityCredential:     managedIdentityCredential,
		ClientSecretCredential:        clientSecretCredential,
		ClientCertificateCredential:   clientCertificateCredential,
		NetworkClientSecretCredential: networkClientSecretCredential,
		MultiTenantCredential:         multiTenantCredential,
	}, nil
}

func (factory *AuthProvider) GetAzIdentity() (azcore.TokenCredential, error) {
	switch true {
	case factory.FederatedIdentityCredential != nil:
		return factory.FederatedIdentityCredential, nil
	case factory.ManagedIdentityCredential != nil:
		return factory.ManagedIdentityCredential, nil
	case factory.ClientSecretCredential != nil:
		return factory.ClientSecretCredential, nil
	case factory.ClientCertificateCredential != nil:
		return factory.ClientCertificateCredential, nil
	default:
		return nil, ErrorNoAuth
	}
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
	if factory.NetworkClientSecretCredential != nil {
		return factory.NetworkClientSecretCredential, nil
	}
	return nil, ErrorNoAuth
}

func (factory *AuthProvider) GetMultiTenantIdentity() (azcore.TokenCredential, error) {
	if factory.MultiTenantCredential != nil {
		return factory.MultiTenantCredential, nil
	}
	return nil, ErrorNoAuth
}
