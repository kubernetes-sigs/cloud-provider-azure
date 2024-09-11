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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/configloader"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	v1 "k8s.io/kubelet/pkg/apis/credentialprovider/v1"
)

// Refer: https://github.com/kubernetes/kubernetes/blob/master/pkg/credentialprovider/azure/azure_credentials.go

const (
	maxReadLength   = 10 * 1 << 20 // 10MB
	defaultCacheTTL = 5 * time.Minute
)

var (
	containerRegistryUrls = []string{"*.azurecr.io", "*.azurecr.cn", "*.azurecr.de", "*.azurecr.us"}
	acrRE                 = regexp.MustCompile(`^.+?\.(azurecr\.io|azurecr\.cn|azurecr\.de|azurecr\.us)`)
)

// CredentialProvider is an interface implemented by the kubelet credential provider plugin to fetch
// the username/password based on the provided image name.
type CredentialProvider interface {
	GetCredentials(ctx context.Context, image string, args []string) (response *v1.CredentialProviderResponse, err error)
}

// acrProvider implements the credential provider interface for Azure Container Registry.
type acrProvider struct {
	config      *providerconfig.AzureAuthConfig
	environment *azclient.Environment
	credential  azcore.TokenCredential
}

func NewAcrProvider(config *providerconfig.AzureAuthConfig, environment *azclient.Environment, credential azcore.TokenCredential) CredentialProvider {
	return &acrProvider{
		config:      config,
		credential:  credential,
		environment: environment,
	}
}

// NewAcrProvider creates a new instance of the ACR provider.
func NewAcrProviderFromConfig(configFile string) (CredentialProvider, error) {
	if len(configFile) == 0 {
		return nil, errors.New("no azure credential file is provided")
	}
	config, err := configloader.Load[providerconfig.AzureAuthConfig](context.Background(), nil, &configloader.FileLoaderConfig{FilePath: configFile})
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	var envConfig azclient.Environment
	envFilePath, ok := os.LookupEnv(azclient.EnvironmentFilepathName)
	if ok {
		content, err := os.ReadFile(envFilePath)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(content, &envConfig); err != nil {
			return nil, err
		}
	}

	var managedIdentityCredential azcore.TokenCredential

	clientOption, err := azclient.GetAzCoreClientOption(&config.ARMClientConfig)
	if err != nil {
		return nil, err
	}
	if config.UseManagedIdentityExtension {
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
		managedIdentityCredential, err = azidentity.NewManagedIdentityCredential(credOptions)
		if err != nil {
			return nil, err
		}
	}

	return &acrProvider{
		config:      config,
		credential:  managedIdentityCredential,
		environment: &envConfig,
	}, nil
}

func (a *acrProvider) GetCredentials(ctx context.Context, image string, _ []string) (*v1.CredentialProviderResponse, error) {
	loginServer := a.parseACRLoginServerFromImage(image)
	if loginServer == "" {
		klog.V(2).Infof("image(%s) is not from ACR, return empty authentication", image)
		return &v1.CredentialProviderResponse{
			CacheKeyType:  v1.RegistryPluginCacheKeyType,
			CacheDuration: &metav1.Duration{Duration: 0},
			Auth:          map[string]v1.AuthConfig{},
		}, nil
	}

	response := &v1.CredentialProviderResponse{
		CacheKeyType:  v1.RegistryPluginCacheKeyType,
		CacheDuration: &metav1.Duration{Duration: defaultCacheTTL},
		Auth: map[string]v1.AuthConfig{
			// empty username and password for anonymous ACR access
			"*.azurecr.*": {
				Username: "",
				Password: "",
			},
		},
	}

	if a.config.UseManagedIdentityExtension {
		username, password, err := a.getFromACR(ctx, loginServer)
		if err != nil {
			klog.Errorf("error getting credentials from ACR for %s: %s", loginServer, err)
			return nil, err
		}

		response.Auth[loginServer] = v1.AuthConfig{
			Username: username,
			Password: password,
		}
	} else {
		// Add our entry for each of the supported container registry URLs
		for _, url := range containerRegistryUrls {
			cred := v1.AuthConfig{
				Username: a.config.AADClientID,
				Password: a.config.AADClientSecret,
			}
			response.Auth[url] = cred
		}

		// Handle the custom cloud case
		// In clouds where ACR is not yet deployed, the string will be empty
		if a.environment != nil && strings.Contains(a.environment.ContainerRegistryDNSSuffix, ".azurecr.") {
			customAcrSuffix := "*" + a.environment.ContainerRegistryDNSSuffix
			hasBeenAdded := false
			for _, url := range containerRegistryUrls {
				if strings.EqualFold(url, customAcrSuffix) {
					hasBeenAdded = true
					break
				}
			}

			if !hasBeenAdded {
				cred := v1.AuthConfig{
					Username: a.config.AADClientID,
					Password: a.config.AADClientSecret,
				}
				response.Auth[customAcrSuffix] = cred
			}
		}
	}

	return response, nil
}

// getFromACR gets credentials from ACR.
func (a *acrProvider) getFromACR(ctx context.Context, loginServer string) (string, string, error) {
	config, err := azclient.GetAzureCloudConfig(&a.config.ARMClientConfig)
	if err != nil {
		return "", "", err
	}
	var armAccessToken azcore.AccessToken
	if armAccessToken, err = a.credential.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{
			strings.TrimRight(config.Services[azcontainerregistry.ServiceName].Audience, "/") + "/.default",
		},
	}); err != nil {
		klog.Errorf("Failed to ensure fresh service principal token: %v", err)
		return "", "", err
	}

	klog.V(4).Infof("discovering auth redirects for: %s", loginServer)
	directive, err := receiveChallengeFromLoginServer(loginServer, "https")
	if err != nil {
		klog.Errorf("failed to receive challenge: %s", err)
		return "", "", err
	}

	klog.V(4).Infof("exchanging an acr refresh_token")
	registryRefreshToken, err := performTokenExchange(
		loginServer, directive, a.config.TenantID, armAccessToken.Token)
	if err != nil {
		klog.Errorf("failed to perform token exchange: %s", err)
		return "", "", err
	}

	return dockerTokenLoginUsernameGUID, registryRefreshToken, nil
}

// parseACRLoginServerFromImage takes image as parameter and returns login server of it.
// Parameter `image` is expected in following format: foo.azurecr.io/bar/imageName:version
// If the provided image is not an acr image, this function will return an empty string.
func (a *acrProvider) parseACRLoginServerFromImage(image string) string {
	match := acrRE.FindAllString(image, -1)
	if len(match) == 1 {
		return match[0]
	}

	// handle the custom cloud case
	if a != nil && a.environment != nil {
		cloudAcrSuffix := a.environment.ContainerRegistryDNSSuffix
		cloudAcrSuffixLength := len(cloudAcrSuffix)
		if cloudAcrSuffixLength > 0 {
			customAcrSuffixIndex := strings.Index(image, cloudAcrSuffix)
			if customAcrSuffixIndex != -1 {
				endIndex := customAcrSuffixIndex + cloudAcrSuffixLength
				return image[0:endIndex]
			}
		}
	}

	return ""
}
