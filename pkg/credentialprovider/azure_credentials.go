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
	"fmt"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/configloader"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	v1 "k8s.io/kubelet/pkg/apis/credentialprovider/v1"
)

// Refer: https://github.com/kubernetes/kubernetes/blob/master/pkg/credentialprovider/azure/azure_credentials.go

const (
	maxReadLength   = 10 * 1 << 20 // 10MB
	defaultCacheTTL = 5 * time.Minute

	AcrAudience = "https://containerregistry.azure.net"
)

var (
	containerRegistryUrls = []string{"*.azurecr.io", "*.azurecr.cn", "*.azurecr.de", "*.azurecr.us"}
	// a valid acr image starts with alphanumerics, followed by corresponding acr domain name.
	acrRE = regexp.MustCompile(`^[a-zA-Z0-9]+\.(azurecr\.io|azurecr\.cn|azurecr\.de|azurecr\.us)`)
)

// CredentialProvider is an interface implemented by the kubelet credential provider plugin to fetch
// the username/password based on the provided image name.
type CredentialProvider interface {
	GetCredentials(ctx context.Context, image string, args []string) (response *v1.CredentialProviderResponse, err error)
}

// acrProvider implements the credential provider interface for Azure Container Registry.
type acrProvider struct {
	config         *providerconfig.AzureClientConfig
	environment    *azclient.Environment
	cloudConfig    cloud.Configuration
	credential     azcore.TokenCredential
	registryMirror map[string]string // Registry mirror relation: source registry -> target registry
}

type getTokenCredentialFunc func(req *v1.CredentialProviderRequest, config *providerconfig.AzureClientConfig) (azcore.TokenCredential, error)

func NewAcrProvider(req *v1.CredentialProviderRequest, registryMirrorStr string, configFile string) (CredentialProvider, error) {
	config, err := configloader.Load[providerconfig.AzureClientConfig](context.Background(), nil, &configloader.FileLoaderConfig{FilePath: configFile})
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	_, env, err := azclient.GetAzCoreClientOption(&config.ARMClientConfig)
	if err != nil {
		return nil, err
	}
	clientOption, _, err := azclient.GetAzCoreClientOption(&config.ARMClientConfig)
	if err != nil {
		return nil, err
	}

	var getTokenCredential getTokenCredentialFunc

	// kubelet is responsible for checking the service account token emptiness when service account token is enabled, and only when service account token provide is enabled,
	// service account token is set in the request, so we can safely check the service account token emptiness to decide which credential to use.
	if len(req.ServiceAccountToken) != 0 {
		klog.V(2).Infof("Using service account token to authenticate ACR for image %s", req.Image)
		getTokenCredential = getServiceAccountTokenCredential
	} else {
		klog.V(2).Infof("Using managed identity to authenticate ACR for image %s", req.Image)
		getTokenCredential = getManagedIdentityCredential
	}
	credential, err := getTokenCredential(req, config)
	if err != nil {
		return nil, fmt.Errorf("failed to get token credential for image %s: %w", req.Image, err)
	}

	return &acrProvider{
		config:         config,
		credential:     credential,
		environment:    env,
		cloudConfig:    clientOption.Cloud,
		registryMirror: parseRegistryMirror(registryMirrorStr),
	}, nil
}

// getManagedIdentityCredential creates a new instance of the ACR provider.
func getManagedIdentityCredential(_ *v1.CredentialProviderRequest, config *providerconfig.AzureClientConfig) (azcore.TokenCredential, error) {

	var managedIdentityCredential azcore.TokenCredential

	clientOption, _, err := azclient.GetAzCoreClientOption(&config.ARMClientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get Azure client options: %w", err)
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

	return managedIdentityCredential, nil
}

func getServiceAccountTokenCredential(req *v1.CredentialProviderRequest, config *providerconfig.AzureClientConfig) (azcore.TokenCredential, error) {
	if len(req.ServiceAccountToken) == 0 {
		return nil, fmt.Errorf("kubernetes Service account token is not provided for image %s", req.Image)
	}
	klog.V(2).Infof("Kubernetes Service account token is provided for image %s", req.Image)

	clientOption, _, err := azclient.GetAzCoreClientOption(&config.ARMClientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to get Azure client options for image %s: %w", req.Image, err)
	}

	// check required annotations
	clientID, ok := req.ServiceAccountAnnotations[clientIDAnnotation]
	if !ok || len(clientID) == 0 {
		return nil, fmt.Errorf("client id annotation %s is not found or the value is empty", clientIDAnnotation)
	}

	// check required annotations
	tenantID, ok := req.ServiceAccountAnnotations[tenantIDAnnotation]
	if !ok || len(tenantID) == 0 {
		return nil, fmt.Errorf("tenant id annotation %s is not found or the value is empty", tenantIDAnnotation)
	}

	// Create getAssertion callback that returns the service account token
	getAssertion := func(_ context.Context) (string, error) {
		return req.ServiceAccountToken, nil
	}

	clientAssertCredential, err := azidentity.NewClientAssertionCredential(tenantID, clientID, getAssertion, &azidentity.ClientAssertionCredentialOptions{
		ClientOptions: *clientOption,
	})

	if err != nil {
		return nil, fmt.Errorf("failed to initialize client assertion credential: %w", err)
	}
	return clientAssertCredential, nil
}

func (a *acrProvider) GetCredentials(ctx context.Context, image string, _ []string) (*v1.CredentialProviderResponse, error) {
	targetloginServer, sourceloginServer := a.parseACRLoginServerFromImage(image)
	if targetloginServer == "" {
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
		username, password, err := a.getFromACR(ctx, targetloginServer)
		if err != nil {
			klog.Errorf("error getting credentials from ACR for %s: %s", targetloginServer, err)
			return nil, err
		}

		authConfig := v1.AuthConfig{
			Username: username,
			Password: password,
		}
		response.Auth[targetloginServer] = authConfig
		if sourceloginServer != "" {
			response.Auth[sourceloginServer] = authConfig
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
	var armAccessToken azcore.AccessToken
	var err error
	if armAccessToken, err = a.credential.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{
			fmt.Sprintf("%s/%s", AcrAudience, ".default"),
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
	registryRefreshToken, err := performTokenExchange(directive, a.config.TenantID, armAccessToken.Token)
	if err != nil {
		klog.Errorf("failed to perform token exchange: %s", err)
		return "", "", err
	}

	return dockerTokenLoginUsernameGUID, registryRefreshToken, nil
}

// parseACRLoginServerFromImage inputs an image URL and outputs login servers of target registry and source registry if --registry-mirror is set.
// Input is expected in following format: foo.azurecr.io/bar/imageName:version
// If the provided image is not an acr image, this function will return an empty string.
func (a *acrProvider) parseACRLoginServerFromImage(image string) (string, string) {
	targetImage, sourceRegistry := a.processImageWithRegistryMirror(image)
	targetRegistry := acrRE.FindString(targetImage)
	imageWithoutRegistry := strings.TrimPrefix(targetImage, targetRegistry)
	// for non customer cloud case, return registry only when:
	//   - the acr pattern match
	//   - the left string is empty or // credential provider authenticates the image pull request, but not validates the existence of the image
	//   - the left string starts with a image repository, lead by "/"
	// foo.azurecr.io/bar/image:version -> foo.azurecr.io
	// foo.azurecr.io -> not match
	// foo.azurecr.io.example -> not match
	if len(targetRegistry) != 0 && (len(imageWithoutRegistry) == 0 || (len(imageWithoutRegistry) != 0 && strings.HasPrefix(imageWithoutRegistry, "/"))) {
		return targetRegistry, sourceRegistry
	}

	// handle the custom cloud case
	if a != nil && a.environment != nil {
		cloudAcrSuffix := a.environment.ContainerRegistryDNSSuffix
		cloudAcrSuffixLength := len(cloudAcrSuffix)
		if cloudAcrSuffixLength > 0 {
			customAcrSuffixIndex := strings.Index(targetImage, cloudAcrSuffix)
			if customAcrSuffixIndex != -1 {
				endIndex := customAcrSuffixIndex + cloudAcrSuffixLength
				return targetImage[0:endIndex], sourceRegistry
			}
		}
	}

	return "", ""
}

// With acrProvider registry mirror, e.g. {"mcr.microsoft.com": "abc.azurecr.io"}
// processImageWithRegistryMirror input format: "mcr.microsoft.com/bar/image:version"
// output format: "abc.azurecr.io/bar/image:version", "mcr.microsoft.com"
func (a *acrProvider) processImageWithRegistryMirror(image string) (string, string) {
	for sourceRegistry, targetRegistry := range a.registryMirror {
		if strings.HasPrefix(image, sourceRegistry) {
			return strings.Replace(image, sourceRegistry, targetRegistry, 1), sourceRegistry
		}
	}
	return image, ""
}

// parseRegistryMirror input format: "--registry-mirror=aaa:bbb,ccc:ddd"
// output format: map[string]string{"aaa": "bbb", "ccc": "ddd"}
func parseRegistryMirror(registryMirrorStr string) map[string]string {
	registryMirror := map[string]string{}

	registryMirrorStr = strings.TrimSpace(registryMirrorStr)
	if len(registryMirrorStr) == 0 {
		return registryMirror
	}

	registryMirrorStr = strings.ReplaceAll(registryMirrorStr, " ", "")
	for _, mapping := range strings.Split(registryMirrorStr, ",") {
		parts := strings.Split(mapping, ":")
		if len(parts) != 2 {
			klog.Errorf("Invalid registry mirror format: %s", mapping)
			continue
		}
		registryMirror[parts[0]] = parts[1]
	}
	return registryMirror
}
