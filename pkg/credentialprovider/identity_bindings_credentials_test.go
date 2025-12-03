/*
Copyright 2021 The Kubernetes Authors.

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

package credentialprovider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	v1 "k8s.io/kubelet/pkg/apis/credentialprovider/v1"

	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
)

func TestGetIdentityBindingsTokenCredential(t *testing.T) {
	tests := []struct {
		name        string
		ibConfig    IdentityBindingsConfig
		wantErr     bool
		errContains string
	}{
		{
			name: "valid config",
			ibConfig: IdentityBindingsConfig{
				SNIName:     "api.example.com",
				APIServerIP: "10.0.0.1",
			},
			wantErr: false,
		},
		{
			name: "missing SNI name",
			ibConfig: IdentityBindingsConfig{
				APIServerIP: "10.0.0.1",
			},
			wantErr:     true,
			errContains: "SNI name not provided",
		},
		{
			name: "missing API server IP",
			ibConfig: IdentityBindingsConfig{
				SNIName: "api.example.com",
			},
			wantErr:     true,
			errContains: "API server IP not provided",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &v1.CredentialProviderRequest{
				Image:               "test.azurecr.io/test:latest",
				ServiceAccountToken: "test-sa-token",
				ServiceAccountAnnotations: map[string]string{
					clientIDAnnotation: "test-client-123",
				},
			}
			config := &providerconfig.AzureClientConfig{}

			cred, err := GetIdentityBindingsTokenCredential(req, config, tt.ibConfig)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetIdentityBindingsTokenCredential() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errContains)
				} else if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}
			if cred == nil {
				t.Error("expected non-nil credential")
			}
		})
	}
}

func TestCreateTransport(t *testing.T) {
	// Set HTTPS_PROXY environment variable to verify it's ignored
	originalHTTPSProxy := os.Getenv("HTTPS_PROXY")
	defer func() {
		if originalHTTPSProxy != "" {
			os.Setenv("HTTPS_PROXY", originalHTTPSProxy)
		} else {
			os.Unsetenv("HTTPS_PROXY")
		}
	}()

	// Set proxy environment variables
	os.Setenv("HTTPS_PROXY", "http://proxy.example.com:8080")

	// Generate a self-signed certificate for testing
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "api.example.com",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"api.example.com"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}

	caPool := x509.NewCertPool()
	caPool.AddCert(cert)

	tests := []struct {
		name        string
		sniName     string
		apiServerIP string
		caPool      *x509.CertPool
	}{
		{
			name:        "with CA pool",
			sniName:     "api.example.com",
			apiServerIP: "10.0.0.1",
			caPool:      caPool,
		},
		{
			name:        "without CA pool",
			sniName:     "api.example.com",
			apiServerIP: "10.0.0.1",
			caPool:      nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := createTransport(tt.sniName, tt.apiServerIP, tt.caPool)
			if transport == nil {
				t.Error("expected non-nil transport")
				return
			}
			if transport.TLSClientConfig == nil {
				t.Error("expected non-nil TLSClientConfig")
				return
			}
			if transport.TLSClientConfig.ServerName != tt.sniName {
				t.Errorf("ServerName = %v, want %v", transport.TLSClientConfig.ServerName, tt.sniName)
			}
			if tt.caPool != nil {
				if transport.TLSClientConfig.RootCAs == nil {
					t.Error("expected non-nil RootCAs when caPool provided")
				}
			}
			if transport.DialContext == nil {
				t.Error("expected non-nil DialContext")
			}
			// Verify that Proxy is explicitly set to nil to avoid using environment proxy settings
			if transport.Proxy != nil {
				t.Error("expected Proxy to be nil to bypass environment proxy settings")
			}
		})
	}
}

func TestIdentityBindingsTokenCredential_GetToken(t *testing.T) {
	// Create a test server
	mux := http.NewServeMux()
	var formDataReceived map[string]string

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "failed to parse form", http.StatusBadRequest)
			return
		}
		formDataReceived = make(map[string]string)
		for key := range r.Form {
			formDataReceived[key] = r.Form.Get(key)
		}

		resp := tokenResponse{
			AccessToken: "test-token",
			ExpiresIn:   3600,
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			http.Error(w, "failed to encode response", http.StatusInternalServerError)
			return
		}
	})

	server := httptest.NewUnstartedServer(mux)

	// Generate certificate for TLS
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "api.example.com",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"api.example.com"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	cert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  priv,
	}

	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12, // #nosec G402
	}
	server.StartTLS()
	defer server.Close()

	// Parse server URL to get the port
	serverURL := server.URL
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(serverURL, "https://"))

	// Create CA pool from the server's certificate
	parsedCert, _ := x509.ParseCertificate(certDER)
	caPool := x509.NewCertPool()
	caPool.AddCert(parsedCert)

	tests := []struct {
		name              string
		req               *v1.CredentialProviderRequest
		ibConfig          IdentityBindingsConfig
		scopes            []string
		wantErr           bool
		errContains       string
		checkFormData     bool
		expectedGrantType string
	}{
		{
			name: "successful token retrieval with client ID from annotation",
			req: &v1.CredentialProviderRequest{
				Image:               "test.azurecr.io/test:latest",
				ServiceAccountToken: "test-sa-token",
				ServiceAccountAnnotations: map[string]string{
					clientIDAnnotation: "client-123",
				},
			},
			ibConfig: IdentityBindingsConfig{
				SNIName:     "api.example.com",
				APIServerIP: "127.0.0.1",
			},
			scopes:            []string{"https://containerregistry.azure.net/.default"},
			wantErr:           false,
			checkFormData:     true,
			expectedGrantType: "client_credentials",
		},
		{
			name: "successful token retrieval with default client ID",
			req: &v1.CredentialProviderRequest{
				Image:                     "test.azurecr.io/test:latest",
				ServiceAccountToken:       "test-sa-token",
				ServiceAccountAnnotations: map[string]string{},
			},
			ibConfig: IdentityBindingsConfig{
				SNIName:         "api.example.com",
				APIServerIP:     "127.0.0.1",
				DefaultClientID: "default-client-456",
			},
			scopes:            []string{"https://containerregistry.azure.net/.default"},
			wantErr:           false,
			checkFormData:     true,
			expectedGrantType: "client_credentials",
		},
		{
			name: "missing service account token",
			req: &v1.CredentialProviderRequest{
				Image:                     "test.azurecr.io/test:latest",
				ServiceAccountToken:       "",
				ServiceAccountAnnotations: map[string]string{},
			},
			ibConfig: IdentityBindingsConfig{
				SNIName:     "api.example.com",
				APIServerIP: "127.0.0.1",
			},
			scopes:      []string{"https://containerregistry.azure.net/.default"},
			wantErr:     true,
			errContains: "service account token not found",
		},
		{
			name: "missing client ID",
			req: &v1.CredentialProviderRequest{
				Image:                     "test.azurecr.io/test:latest",
				ServiceAccountToken:       "test-sa-token",
				ServiceAccountAnnotations: map[string]string{},
			},
			ibConfig: IdentityBindingsConfig{
				SNIName:     "api.example.com",
				APIServerIP: "127.0.0.1",
			},
			scopes:      []string{"https://containerregistry.azure.net/.default"},
			wantErr:     true,
			errContains: "client ID not configured",
		},
		{
			name: "invalid scope count",
			req: &v1.CredentialProviderRequest{
				Image:               "test.azurecr.io/test:latest",
				ServiceAccountToken: "test-sa-token",
				ServiceAccountAnnotations: map[string]string{
					clientIDAnnotation: "client-123",
				},
			},
			ibConfig: IdentityBindingsConfig{
				SNIName:     "api.example.com",
				APIServerIP: "127.0.0.1",
			},
			scopes:      []string{"scope1", "scope2"},
			wantErr:     true,
			errContains: "expected exactly one scope",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset captured data
			formDataReceived = nil

			endpoint := fmt.Sprintf("https://api.example.com:%s", port)
			transport := createTransport(tt.ibConfig.SNIName, tt.ibConfig.APIServerIP, caPool)

			// Determine client ID from annotation or default
			var clientID string
			if id, ok := tt.req.ServiceAccountAnnotations[clientIDAnnotation]; ok {
				clientID = id
			} else {
				clientID = tt.ibConfig.DefaultClientID
			}

			// Determine tenant ID from annotation or default
			var tenantID string
			if id, ok := tt.req.ServiceAccountAnnotations[tenantIDAnnotation]; ok {
				tenantID = id
			} else {
				tenantID = tt.ibConfig.DefaultTenantID
			}

			cred := &identityBindingsTokenCredential{
				token:     tt.req.ServiceAccountToken,
				clientID:  clientID,
				tenantID:  tenantID,
				config:    &providerconfig.AzureClientConfig{},
				ibConfig:  tt.ibConfig,
				endpoint:  endpoint,
				transport: transport,
			}

			ctx := context.Background()
			opts := policy.TokenRequestOptions{
				Scopes: tt.scopes,
			}

			token, err := cred.GetToken(ctx, opts)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetToken() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errContains)
				} else if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}

			if token.Token != "test-token" {
				t.Errorf("token = %v, want %v", token.Token, "test-token")
			}

			if tt.checkFormData && formDataReceived != nil {
				if formDataReceived["grant_type"] != tt.expectedGrantType {
					t.Errorf("grant_type = %v, want %v", formDataReceived["grant_type"], tt.expectedGrantType)
				}
				if formDataReceived["client_assertion"] != tt.req.ServiceAccountToken {
					t.Errorf("client_assertion = %v, want %v", formDataReceived["client_assertion"], tt.req.ServiceAccountToken)
				}
				if formDataReceived["scope"] != tt.scopes[0] {
					t.Errorf("scope = %v, want %v", formDataReceived["scope"], tt.scopes[0])
				}
			}
		})
	}
}
