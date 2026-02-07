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
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/kubelet/pkg/apis/credentialprovider/v1"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
)

const (
	// msiEndpointEnv is the environment variable used to store the endpoint in go-autorest/adal.
	msiEndpointEnv = "MSI_ENDPOINT"
	// msiSecretEnv is the environment variable used to store the request secret in go-autorest/adal.
	msiSecretEnv = "MSI_SECRET"
)

func TestGetCredentials(t *testing.T) {
	result := []string{
		"*.azurecr.io",
		"*.azurecr.cn",
		"*.azurecr.de",
		"*.azurecr.us",
	}
	configFile, err := os.CreateTemp(".", "config.json")
	if err != nil {
		t.Fatalf("Unexpected error when creating temp file: %v", err)
	}
	defer os.Remove(configFile.Name())

	_, err = configFile.WriteString(`
    {
        "aadClientId": "foo",
        "aadClientSecret": "bar"
    }`)
	if err != nil {
		t.Fatalf("Unexpected error when writing to temp file: %v", err)
	}

	provider, err := NewAcrProvider(
		&v1.CredentialProviderRequest{
			Image: "foo.azurecr.io/nginx:v1",
		},
		"",
		configFile.Name(),
		IdentityBindingsConfig{},
	)

	if err != nil {
		t.Fatalf("Unexpected error when creating acr provider: %v", err)
	}

	credResponse, err := provider.GetCredentials(context.TODO(), "foo.azurecr.io/nginx:v1", nil)
	if err != nil {
		t.Fatalf("Unexpected error when fetching acr credentials: %v", err)
	}

	if credResponse == nil || len(credResponse.Auth) != len(result)+1 {
		t.Errorf("Unexpected credential response: %v, expected length %d", credResponse, len(result)+1)
	}
	for _, cred := range credResponse.Auth {
		if cred.Username != "" && cred.Username != "foo" {
			t.Errorf("expected 'foo' for username, saw: %v", cred.Username)
		}
		if cred.Password != "" && cred.Password != "bar" {
			t.Errorf("expected 'bar' for password, saw: %v", cred.Username)
		}
	}
	for _, registryName := range result {
		if _, found := credResponse.Auth[registryName]; !found {
			t.Errorf("Missing expected registry: %s", registryName)
		}
	}
}
func TestGetCredentialsConfig(t *testing.T) {
	// msiEndpointEnv and msiSecretEnv are required because autorest/adal requires IMDS endpoint to be available.
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

	testCases := []struct {
		desc                string
		image               string
		configStr           string
		expectError         bool
		expectedCredsLength int
	}{
		{
			desc:        "Error should be returned when the config is incorrect",
			configStr:   "random-config",
			expectError: true,
		},
		{
			desc:  "Multiple credentials should be returned when using Service Principal",
			image: "foo.azurecr.io/bar/image:v1",
			configStr: `
    {
        "aadClientId": "foo",
        "aadClientSecret": "bar"
    }`,
			expectedCredsLength: 5,
		},
		{
			desc:  "0 credential should be returned for non-ACR image using Managed Identity",
			image: "busybox",
			configStr: `
    {
		"useManagedIdentityExtension": true
    }`,
			expectedCredsLength: 0,
		},
	}

	for i, test := range testCases {
		configFile, err := os.CreateTemp(".", "config.json")
		if err != nil {
			t.Fatalf("Unexpected error when creating temp file: %v", err)
		}
		defer os.Remove(configFile.Name())

		_, err = configFile.WriteString(test.configStr)
		if err != nil {
			t.Fatalf("Unexpected error when writing to temp file: %v", err)
		}
		err = configFile.Close()
		if err != nil {
			t.Fatalf("Unexpected error when closing temp file: %v", err)
		}

		provider, err := NewAcrProvider(
			&v1.CredentialProviderRequest{
				Image: "foo.azurecr.io/nginx:v1",
			},
			"",
			configFile.Name(),
			IdentityBindingsConfig{},
		)
		if err != nil && !test.expectError {
			t.Fatalf("Unexpected error when creating new acr provider: %v", err)
		}
		if err != nil && test.expectError {
			err = os.Remove(configFile.Name())
			if err != nil {
				t.Fatalf("Unexpected error when writing to temp file: %v", err)
			}
			continue
		}

		credResponse, err := provider.GetCredentials(context.Background(), test.image, nil)
		if err != nil {
			t.Fatalf("Unexpected error when fetching acr credentials: %v", err)
		}

		assert.NotNil(t, credResponse)
		assert.Equal(t, test.expectedCredsLength, len(credResponse.Auth), "TestCase[%d]: %s", i, test.desc)
	}
}

func TestProcessImageWithMirrorMapping(t *testing.T) {
	configStr := `
	{
	    "aadClientId": "foo",
	    "aadClientSecret": "bar"
	}`

	configFile, err := os.CreateTemp(".", "config.json")
	assert.Nilf(t, err, "Unexpected error when creating temp file")
	defer os.Remove(configFile.Name())
	_, err = configFile.WriteString(configStr)
	assert.Nilf(t, err, "Unexpected error when writing to temp file")
	assert.Nilf(t, configFile.Close(), "Unexpected error when closing temp file")

	provider, err := NewAcrProvider(
		&v1.CredentialProviderRequest{
			Image: "foo.azurecr.io/nginx:v1",
		},
		"mcr.microsoft.com:abc.azurecr.io",
		configFile.Name(),
		IdentityBindingsConfig{},
	)

	assert.Nilf(t, err, "Unexpected error when creating new acr provider")
	acrProvider := provider.(*acrProvider)

	testcases := []struct {
		description               string
		image                     string
		expectedLoginServer       string
		expectedLoginServerMirror string
	}{
		{
			description:               "image in registry mirror map",
			image:                     "mcr.microsoft.com/bar/image:version",
			expectedLoginServer:       "abc.azurecr.io",
			expectedLoginServerMirror: "mcr.microsoft.com",
		},
		{
			description:               "image not in registry mirror map",
			image:                     "foo.azurecr.io/bar/image:version",
			expectedLoginServer:       "foo.azurecr.io",
			expectedLoginServerMirror: "",
		},
	}

	for _, test := range testcases {
		t.Run(test.description, func(t *testing.T) {
			targetloginServer, sourceloginServer := acrProvider.parseACRLoginServerFromImage(test.image)
			assert.Equal(t, test.expectedLoginServer, targetloginServer)
			assert.Equal(t, test.expectedLoginServerMirror, sourceloginServer)
		})
	}
}

func TestParseACRLoginServerFromImage(t *testing.T) {
	configFile, err := os.CreateTemp(".", "config.json")
	if err != nil {
		t.Fatalf("Unexpected error when creating temp file: %v", err)
	}
	defer os.Remove(configFile.Name())

	_, err = configFile.WriteString(`
    {
        "aadClientId": "foo",
        "aadClientSecret": "bar"
    }`)
	if err != nil {
		t.Fatalf("Unexpected error when writing to temp file: %v", err)
	}

	providerInterface, err := NewAcrProvider(
		&v1.CredentialProviderRequest{
			Image: "foo.azurecr.io/nginx:v1",
		},
		"mcr.microsoft.com:abc.azurecr.io",
		configFile.Name(),
		IdentityBindingsConfig{},
	)
	if err != nil {
		t.Fatalf("Unexpected error when creating new acr provider: %v", err)
	}

	provider := providerInterface.(*acrProvider)

	provider.environment = &azclient.Environment{
		ContainerRegistryDNSSuffix: ".azurecr.my.cloud",
	}
	tests := []struct {
		image    string
		expected string
	}{
		{
			image:    "invalidImage",
			expected: "",
		},
		{
			image:    "docker.io/library/busybox:latest",
			expected: "",
		},
		{
			image:    "foo.azurecr.io/bar/image:version",
			expected: "foo.azurecr.io",
		},
		{
			image:    "foo.azurecr.cn/bar/image:version",
			expected: "foo.azurecr.cn",
		},
		{
			image:    "foo.azurecr.de/bar/image:version",
			expected: "foo.azurecr.de",
		},
		{
			image:    "foo.azurecr.us/bar/image:version",
			expected: "foo.azurecr.us",
		},
		{
			image:    "foo.azurecr.my.cloud/bar/image:version",
			expected: "foo.azurecr.my.cloud",
		},
		{
			image:    "foo.azurecr.us/foo.azurecr.io/bar/image:version",
			expected: "foo.azurecr.us",
		},
		{
			image:    "foo.azurecr.io.example/bar/image:version",
			expected: "",
		},
		{
			image:    "docker/foo.azurecr.io/bar/image:version",
			expected: "",
		},
		{
			image:    "foo.azurecr.io",
			expected: "foo.azurecr.io",
		},
		{
			image:    "foo.azurecr.io.azurecr.cn",
			expected: "",
		},
		{
			image:    "foo-azurecr-io.azurecr.cn",
			expected: "",
		},
	}
	for _, test := range tests {
		t.Run(test.image, func(t *testing.T) {
			targetloginServer, _ := provider.parseACRLoginServerFromImage(test.image)
			assert.Equal(t, test.expected, targetloginServer)
		})
	}
}

func TestProcessMirrorMapping(t *testing.T) {
	testcases := []struct {
		description      string
		mirrorMappingStr string
		expected         map[string]string
	}{
		{
			"empty registry mirrors",
			"",
			map[string]string{},
		},
		{
			"multiple registry mirrors",
			"aaa:bbb,ccc:ddd",
			map[string]string{
				"aaa": "bbb",
				"ccc": "ddd",
			},
		},
		{
			"multiple registry mirrors joined with comma and extra spaces",
			"aaa: bbb, ccc:ddd",
			map[string]string{
				"aaa": "bbb",
				"ccc": "ddd",
			},
		},
		{
			"single registry mirror",
			"aaa:bbb",
			map[string]string{
				"aaa": "bbb",
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			result := parseRegistryMirror(tc.mirrorMappingStr)
			assert.True(t, reflect.DeepEqual(result, tc.expected))
		})
	}
}

func TestNewAcrProvider_WithEmptyServiceAccountToken(t *testing.T) {
	// Test case when ServiceAccountToken is empty - should use managed identity credential
	configFile, err := os.CreateTemp(".", "config.json")
	assert.NoError(t, err)
	defer os.Remove(configFile.Name())

	configStr := `{
		"useManagedIdentityExtension": true,
		"userAssignedIdentityID": "test-client-id"
	}`
	_, err = configFile.WriteString(configStr)
	assert.NoError(t, err)
	assert.NoError(t, configFile.Close())

	req := &v1.CredentialProviderRequest{
		Image:               "test.azurecr.io/test:latest",
		ServiceAccountToken: "", // Empty token
	}

	provider, err := NewAcrProvider(req, "", configFile.Name(), IdentityBindingsConfig{})
	assert.NoError(t, err)
	assert.NotNil(t, provider)

	acrProv := provider.(*acrProvider)
	assert.NotNil(t, acrProv.credential)
	assert.Equal(t, true, acrProv.config.UseManagedIdentityExtension)
}

func TestNewAcrProvider_WithServiceAccountToken(t *testing.T) {
	// Test case when ServiceAccountToken is provided - should use service account credential
	configFile, err := os.CreateTemp(".", "config.json")
	assert.NoError(t, err)
	defer os.Remove(configFile.Name())

	configStr := `{
		"tenantID": "test-tenant-id"
	}`
	_, err = configFile.WriteString(configStr)
	assert.NoError(t, err)
	assert.NoError(t, configFile.Close())

	req := &v1.CredentialProviderRequest{
		Image:               "test.azurecr.io/test:latest",
		ServiceAccountToken: "test-service-account-token",
		ServiceAccountAnnotations: map[string]string{
			"kubernetes.azure.com/acr-client-id": "test-client-id",
			"kubernetes.azure.com/acr-tenant-id": "test-tenant-id",
		},
	}

	provider, err := NewAcrProvider(req, "", configFile.Name(), IdentityBindingsConfig{})
	assert.NoError(t, err)
	assert.NotNil(t, provider)

	acrProv := provider.(*acrProvider)
	assert.NotNil(t, acrProv.credential)
}

func TestNewAcrProvider_WithServiceAccountToken_MissingClientIDAnnotation(t *testing.T) {
	// Test case when ServiceAccountToken is provided but client ID annotation is missing
	configFile, err := os.CreateTemp(".", "config.json")
	assert.NoError(t, err)
	defer os.Remove(configFile.Name())

	configStr := `{
		"tenantID": "test-tenant-id"
	}`
	_, err = configFile.WriteString(configStr)
	assert.NoError(t, err)
	assert.NoError(t, configFile.Close())

	req := &v1.CredentialProviderRequest{
		Image:               "test.azurecr.io/test:latest",
		ServiceAccountToken: "test-service-account-token",
		ServiceAccountAnnotations: map[string]string{
			"kubernetes.azure.com/acr-tenant-id": "test-tenant-id",
			// kubernetes.azure.com/acr-client-id is missing
		},
	}

	provider, err := NewAcrProvider(req, "", configFile.Name(), IdentityBindingsConfig{})
	assert.Error(t, err)
	assert.Nil(t, provider)
	assert.Contains(t, err.Error(), "client id annotation")
}

func TestNewAcrProvider_WithServiceAccountToken_MissingTenantIDAnnotation(t *testing.T) {
	// Test case when ServiceAccountToken is provided but tenant ID annotation is missing
	configFile, err := os.CreateTemp(".", "config.json")
	assert.NoError(t, err)
	defer os.Remove(configFile.Name())

	configStr := `{
		"tenantID": "test-tenant-id"
	}`
	_, err = configFile.WriteString(configStr)
	assert.NoError(t, err)
	assert.NoError(t, configFile.Close())

	req := &v1.CredentialProviderRequest{
		Image:               "test.azurecr.io/test:latest",
		ServiceAccountToken: "test-service-account-token",
		ServiceAccountAnnotations: map[string]string{
			"kubernetes.azure.com/acr-client-id": "test-client-id",
			// kubernetes.azure.com/acr-tenant-id is missing
		},
	}

	provider, err := NewAcrProvider(req, "", configFile.Name(), IdentityBindingsConfig{})
	assert.Error(t, err)
	assert.Nil(t, provider)
	assert.Contains(t, err.Error(), "tenant id annotation")
}

func TestNewAcrProvider_WithServiceAccountToken_EmptyClientID(t *testing.T) {
	// Test case when ServiceAccountToken is provided but client ID annotation is empty
	configFile, err := os.CreateTemp(".", "config.json")
	assert.NoError(t, err)
	defer os.Remove(configFile.Name())

	configStr := `{
		"tenantID": "test-tenant-id"
	}`
	_, err = configFile.WriteString(configStr)
	assert.NoError(t, err)
	assert.NoError(t, configFile.Close())

	req := &v1.CredentialProviderRequest{
		Image:               "test.azurecr.io/test:latest",
		ServiceAccountToken: "test-service-account-token",
		ServiceAccountAnnotations: map[string]string{
			"kubernetes.azure.com/acr-client-id": "", // Empty client ID
			"kubernetes.azure.com/acr-tenant-id": "test-tenant-id",
		},
	}

	provider, err := NewAcrProvider(req, "", configFile.Name(), IdentityBindingsConfig{})
	assert.Error(t, err)
	assert.Nil(t, provider)
	assert.Contains(t, err.Error(), "client id annotation")
}

func TestNewAcrProvider_WithServiceAccountToken_EmptyTenantID(t *testing.T) {
	// Test case when ServiceAccountToken is provided but tenant ID annotation is empty
	configFile, err := os.CreateTemp(".", "config.json")
	assert.NoError(t, err)
	defer os.Remove(configFile.Name())

	configStr := `{
		"tenantID": "test-tenant-id"
	}`
	_, err = configFile.WriteString(configStr)
	assert.NoError(t, err)
	assert.NoError(t, configFile.Close())

	req := &v1.CredentialProviderRequest{
		Image:               "test.azurecr.io/test:latest",
		ServiceAccountToken: "test-service-account-token",
		ServiceAccountAnnotations: map[string]string{
			"kubernetes.azure.com/acr-client-id": "test-client-id",
			"kubernetes.azure.com/acr-tenant-id": "", // Empty tenant ID
		},
	}

	provider, err := NewAcrProvider(req, "", configFile.Name(), IdentityBindingsConfig{})
	assert.Error(t, err)
	assert.Nil(t, provider)
	assert.Contains(t, err.Error(), "tenant id annotation")
}

func TestNewAcrProvider_InvalidConfig(t *testing.T) {
	// Test case when config file is invalid
	configFile, err := os.CreateTemp(".", "config.json")
	assert.NoError(t, err)
	defer os.Remove(configFile.Name())

	// Invalid JSON
	invalidConfigStr := `{invalid json`
	_, err = configFile.WriteString(invalidConfigStr)
	assert.NoError(t, err)
	assert.NoError(t, configFile.Close())

	req := &v1.CredentialProviderRequest{
		Image:               "test.azurecr.io/test:latest",
		ServiceAccountToken: "",
	}

	provider, err := NewAcrProvider(req, "", configFile.Name(), IdentityBindingsConfig{})
	assert.Error(t, err)
	assert.Nil(t, provider)
	assert.Contains(t, err.Error(), "failed to load config")
}
