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
	"testing"

	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
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

	provider := NewAcrProvider(&config.AzureAuthConfig{
		AzureAuthConfig: azclient.AzureAuthConfig{
			AADClientID:     "foo",
			AADClientSecret: "bar",
		},
	}, nil, nil)

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
		_, err = configFile.WriteString(test.configStr)
		if err != nil {
			t.Fatalf("Unexpected error when writing to temp file: %v", err)
		}
		err = configFile.Close()
		if err != nil {
			t.Fatalf("Unexpected error when closing temp file: %v", err)
		}
		provider, err := NewAcrProviderFromConfig(configFile.Name())
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
		err = os.Remove(configFile.Name())
		if err != nil {
			t.Fatalf("Unexpected error when writing to temp file: %v", err)
		}

		credResponse, err := provider.GetCredentials(context.Background(), test.image, nil)
		if err != nil {
			t.Fatalf("Unexpected error when fetching acr credentials: %v", err)
		}

		assert.NotNil(t, credResponse)
		assert.Equal(t, test.expectedCredsLength, len(credResponse.Auth), "TestCase[%d]: %s", i, test.desc)
	}
}

func TestParseACRLoginServerFromImage(t *testing.T) {

	providerInterface := NewAcrProvider(&config.AzureAuthConfig{
		AzureAuthConfig: azclient.AzureAuthConfig{
			AADClientID:     "foo",
			AADClientSecret: "bar",
		},
	}, nil, nil)

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
	}
	for _, test := range tests {
		if loginServer := provider.parseACRLoginServerFromImage(test.image); loginServer != test.expected {
			t.Errorf("function parseACRLoginServerFromImage returns \"%s\" for image %s, expected \"%s\"", loginServer, test.image, test.expected)
		}
	}
}
