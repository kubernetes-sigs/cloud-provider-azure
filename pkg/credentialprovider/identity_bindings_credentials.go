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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"k8s.io/klog/v2"
	v1 "k8s.io/kubelet/pkg/apis/credentialprovider/v1"
)

const (
	// Kubernetes certificate path
	KubernetesCACertPath = "/etc/kubernetes/certs/ca.crt"
)

// identityBindingsTokenCredential implements azcore.TokenCredential interface
// using identity bindings token exchange
type identityBindingsTokenCredential struct {
	token    string
	clientID string
	// tenantID is reserved for future SDK compatibility and may be used in token endpoint construction.
	tenantID  string
	config    *providerconfig.AzureClientConfig
	ibConfig  IdentityBindingsConfig
	endpoint  string
	transport *http.Transport
}

// tokenResponse represents the response from identity bindings token exchange
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// createTransport creates an HTTP transport with custom CA
// The transport uses a custom dialer that resolves the SNI name to the configured API server IP
func createTransport(sniName string, apiServerIP string, caPool *x509.CertPool) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// reset Proxy to avoid using environment proxy settings
	transport.Proxy = nil

	// Custom dialer that resolves the SNI hostname to the fixed API server IP
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Extract port from addr (format is "host:port")
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse address %s: %w", addr, err)
		}

		// Always connect to the configured API server IP
		fixedAddr := net.JoinHostPort(apiServerIP, port)
		klog.V(5).Infof("Identity bindings: resolving %s to %s", addr, fixedAddr)

		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		return dialer.DialContext(ctx, network, fixedAddr)
	}

	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12, // #nosec G402
		}
	}
	transport.TLSClientConfig.ServerName = sniName
	// Explicitly set minimum TLS version to TLS 1.2 for security
	transport.TLSClientConfig.MinVersion = tls.VersionTLS12

	// Set custom CA pool if provided
	if caPool != nil {
		transport.TLSClientConfig.RootCAs = caPool
	}

	return transport
}

// getTransport provides the transport to use for the request
func (c *identityBindingsTokenCredential) getTransport() (*http.Transport, error) {
	// Return existing transport if already created
	if c.transport != nil {
		return c.transport, nil
	}

	// Read CA file
	b, err := os.ReadFile(KubernetesCACertPath)
	if err != nil {
		return nil, fmt.Errorf("read CA file %q: %w", KubernetesCACertPath, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("CA file %q is empty", KubernetesCACertPath)
	}

	// Create CA pool
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("parse CA file %q: no valid certificates found", KubernetesCACertPath)
	}

	// Create and cache transport
	c.transport = createTransport(c.ibConfig.SNIName, c.ibConfig.APIServerIP, caPool)

	return c.transport, nil
}

// GetToken retrieves an access token using identity bindings token exchange
func (c *identityBindingsTokenCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	// The scope should be exactly one value in format "https://management.azure.com/.default"
	// or "https://containerregistry.azure.net/.default"
	if len(opts.Scopes) != 1 {
		return azcore.AccessToken{}, fmt.Errorf("expected exactly one scope, got %d", len(opts.Scopes))
	}

	scope := opts.Scopes[0]

	// Use stored client assertion token
	clientAssertion := c.token
	if clientAssertion == "" {
		return azcore.AccessToken{}, fmt.Errorf("service account token not found")
	}

	// Use stored client ID
	clientID := c.clientID
	if clientID == "" {
		return azcore.AccessToken{}, fmt.Errorf("client ID not configured")
	}

	// Prepare form data
	formData := url.Values{}
	formData.Set("grant_type", "client_credentials")
	formData.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	formData.Set("scope", scope)
	formData.Set("client_assertion", clientAssertion)
	formData.Set("client_id", clientID)

	// Create request
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, strings.NewReader(formData.Encode()))
	if err != nil {
		return azcore.AccessToken{}, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	klog.V(4).Infof("Requesting token from identity bindings endpoint: %s with scope: %s", c.endpoint, scope)

	// Get transport (handles CA rotation)
	transport, err := c.getTransport()
	if err != nil {
		return azcore.AccessToken{}, fmt.Errorf("failed to get transport: %w", err)
	}

	// Execute request
	httpClient := &http.Client{Transport: transport}
	resp, err := httpClient.Do(req)
	if err != nil {
		return azcore.AccessToken{}, fmt.Errorf("failed to execute token request: %w", err)
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return azcore.AccessToken{}, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return azcore.AccessToken{}, fmt.Errorf("token request failed with status %d", resp.StatusCode)
	}

	// Parse response
	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return azcore.AccessToken{}, fmt.Errorf("failed to parse token response: %w", err)
	}

	expiresOn := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)

	klog.V(4).Infof("Successfully obtained token from identity bindings, expires at: %s", expiresOn)

	return azcore.AccessToken{
		Token:     tokenResp.AccessToken,
		ExpiresOn: expiresOn,
	}, nil
}

func GetIdentityBindingsTokenCredential(req *v1.CredentialProviderRequest, config *providerconfig.AzureClientConfig, ibConfig IdentityBindingsConfig) (azcore.TokenCredential, error) {
	klog.V(2).Infof("Using identity bindings token credential for image %s", req.Image)

	// Get SNI name from config
	sniName := ibConfig.SNIName
	if sniName == "" {
		return nil, fmt.Errorf("SNI name not provided in identity bindings config")
	}

	// Get API server IP from config
	apiServerIP := ibConfig.APIServerIP
	if apiServerIP == "" {
		return nil, fmt.Errorf("API server IP not provided in identity bindings config")
	}

	// Get service account token
	token := req.ServiceAccountToken
	if token == "" {
		return nil, fmt.Errorf("service account token not found in request")
	}

	// Resolve client ID from annotation or use default
	var clientID string
	if id, ok := req.ServiceAccountAnnotations[clientIDAnnotation]; ok {
		clientID = id
	} else {
		clientID = ibConfig.DefaultClientID
	}
	if clientID == "" {
		return nil, fmt.Errorf("client ID not found in service account annotations (checked %s) and no default client ID configured",
			clientIDAnnotation)
	}

	// Resolve tenant ID from annotation or use default
	var tenantID string
	if id, ok := req.ServiceAccountAnnotations[tenantIDAnnotation]; ok {
		tenantID = id
	} else {
		tenantID = ibConfig.DefaultTenantID
	}

	// Build endpoint URL
	endpoint := "https://" + sniName

	return &identityBindingsTokenCredential{
		token:    token,
		clientID: clientID,
		tenantID: tenantID,
		config:   config,
		ibConfig: ibConfig,
		endpoint: endpoint,
	}, nil
}
