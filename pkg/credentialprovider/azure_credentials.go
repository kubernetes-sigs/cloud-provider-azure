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
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/containers/azcontainerregistry"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	v1 "k8s.io/kubelet/pkg/apis/credentialprovider/v1"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/configloader"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
)

// Refer: https://github.com/kubernetes/kubernetes/blob/master/pkg/credentialprovider/azure/azure_credentials.go

const (
	maxReadLength   = 10 * 1 << 20 // 10MB
	defaultCacheTTL = 5 * time.Minute

	// ACR audience scopes the AAD token for registration authentication to ACR only
	acrAudience = "https://containerregistry.azure.net"

	// The null GUID tells the container registry that this is an ACR refresh token during the login flow
	acrRefreshDockerLoginUsernameGUID = "00000000-0000-0000-0000-000000000000"
)

var (
	containerRegistryUrls = []string{"*.azurecr.io", "*.azurecr.cn", "*.azurecr.de", "*.azurecr.us"}
	// a valid acr image starts with alphanumerics, followed by corresponding acr domain name.
	acrRE = regexp.MustCompile(`^[a-zA-Z0-9]+\.(azurecr\.io|azurecr\.cn|azurecr\.de|azurecr\.us)`)
)

// CredentialProvider is an interface implemented by the kubelet credential provider plugin to fetch
// the username/password based on the provided image name.
type CredentialProvider interface {
	GetCredentials(ctx context.Context, request *v1.CredentialProviderRequest, args []string) (response *v1.CredentialProviderResponse, err error)
}

// acrProvider implements the credential provider interface for Azure Container Registry.
type acrProvider struct {
	config         *providerconfig.AzureClientConfig
	environment    *azclient.Environment
	cloudConfig    cloud.Configuration
	credential     azcore.TokenCredential
	registryMirror map[string]string // Registry mirror relation: source registry -> target registry
}

func NewAcrProvider(config *providerconfig.AzureClientConfig, environment *azclient.Environment, credential azcore.TokenCredential) CredentialProvider {
	return &acrProvider{
		config:      config,
		credential:  credential,
		environment: environment,
	}
}

// NewAcrProvider creates a new instance of the ACR provider.
func NewAcrProviderFromConfig(configFile string, registryMirrorStr string) (CredentialProvider, error) {
	if len(configFile) == 0 {
		return nil, errors.New("no azure credential file is provided")
	}
	config, err := configloader.Load[providerconfig.AzureClientConfig](context.Background(), nil, &configloader.FileLoaderConfig{FilePath: configFile})
	if err != nil {
		return nil, fmt.Errorf("Failed to load config: %w", err)
	}

	var managedIdentityCredential azcore.TokenCredential

	clientOption, env, err := azclient.GetAzCoreClientOption(&config.ARMClientConfig)
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
	}

	return &acrProvider{
		config:         config,
		credential:     managedIdentityCredential,
		environment:    env,
		cloudConfig:    clientOption.Cloud,
		registryMirror: parseRegistryMirror(registryMirrorStr),
	}, nil
}

func (a *acrProvider) GetCredentials(ctx context.Context, request *v1.CredentialProviderRequest, _ []string) (*v1.CredentialProviderResponse, error) {
	if len(request.Image) == 0 {
		return nil, errors.New("image in plugin request is empty")
	}
	targetLoginServer, sourceLoginServer := a.parseACRLoginServerFromImage(request.Image)
	if targetLoginServer == "" {
		klog.V(2).Infof("Image(%s) is not from ACR, return empty authentication", request.Image)
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

		// Validations for service account token
		if len(request.ServiceAccountToken) != 0 {
			clientAssertCredential, err := getServiceAccountTokenCredential(request, targetLoginServer)
			if err != nil {
				return nil, fmt.Errorf("failed to get client assertion credential, %s", err)
			}
			a.credential = clientAssertCredential
		}

		username, password, err := a.getRefreshTokenFromACR(ctx, targetLoginServer)
		if err != nil {
			klog.Errorf("Failed to get refresh token from ACR for %s: %s", targetLoginServer, err)
			return nil, err
		}

		authConfig := v1.AuthConfig{
			Username: username,
			Password: password,
		}
		response.Auth[targetLoginServer] = authConfig
		if sourceLoginServer != "" {
			response.Auth[sourceLoginServer] = authConfig
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

// Custom policy to add User-Agent
type userAgentPolicy struct {
	userAgent string
}

func (p *userAgentPolicy) Do(req *policy.Request) (*http.Response, error) {
	// Prepend your custom User-Agent before the default one
	ua := fmt.Sprintf("%s %s", p.userAgent, req.Raw().Header.Get("User-Agent"))
	req.Raw().Header.Set("User-Agent", ua)
	return req.Next()
}

// getRefreshTokenFromACR gets credentials from ACR.
func (a *acrProvider) getRefreshTokenFromACR(ctx context.Context, loginServer string) (string, string, error) {
	// 1. Get an AAD ACR audience token
	acrAudienceToken, err := a.credential.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{
			fmt.Sprintf("%s/%s", acrAudience, ".default"),
		},
	})
	if err != nil {
		klog.Errorf("Failed to get AAD ACR audience token: %v", err)
		return "", "", err
	}

	uaPolicy := &userAgentPolicy{userAgent: userAgent}
	acrAuthenticationClient, err := azcontainerregistry.NewAuthenticationClient(loginServer, &azcontainerregistry.AuthenticationClientOptions{
		ClientOptions: azcore.ClientOptions{
			PerCallPolicies: []policy.Policy{uaPolicy},
		},
	})
	if err != nil {
		log.Fatalf("Failed to create token client: %v", err)
		return "", "", err
	}

	// 2. Exchange AAD access token for ACR refresh token
	// TODO(mainred): log correlation id
	// TODO(mainred): can we replace the loginserver with directive.service?
	// Exchange AAD token for ACR refresh token
	exchangeResp, err := acrAuthenticationClient.ExchangeAADAccessTokenForACRRefreshToken(ctx,
		azcontainerregistry.PostContentSchemaGrantTypeAccessTokenRefreshToken,
		loginServer,
		&azcontainerregistry.AuthenticationClientExchangeAADAccessTokenForACRRefreshTokenOptions{
			Tenant:      to.Ptr(a.config.TenantID),
			AccessToken: to.Ptr(acrAudienceToken.Token),
		},
	)
	if err != nil {
		log.Fatalf("Failed to exchange AAD token for ACR refresh token: %v", err)
		return "", "", err
	}

	if exchangeResp.RefreshToken == nil {
		log.Fatalf("ACR refresh token is nil")
	}

	return acrRefreshDockerLoginUsernameGUID, *exchangeResp.RefreshToken, nil
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

func getServiceAccountTokenCredential(request *v1.CredentialProviderRequest, loginServer string) (azcore.TokenCredential, error) {
	if len(request.ServiceAccountToken) == 0 {
		return nil, nil
	}
	klog.V(2).Infof("Kubernetes service account token is set")

	// check required annotations
	clientIDAnnotation := "azure.workload.identity/client-id"
	clientID, ok := request.ServiceAccountAnnotations[clientIDAnnotation]
	if !ok || len(clientID) == 0 {
		return nil, fmt.Errorf("client id annotation %s is not found or the value is empty", clientIDAnnotation)
	}

	tenantIDAnnotation := "azure.workload.identity/tenant-id"
	tenantID, ok := request.ServiceAccountAnnotations[tenantIDAnnotation]
	if !ok || len(tenantID) == 0 {
		return nil, fmt.Errorf("client id annotation %s is not found or the value is empty", tenantIDAnnotation)
	}

	// discover token authority, which varies among different clouds
	klog.V(4).Infof("discovering auth redirects for: %s", loginServer)
	directive, err := receiveChallengeFromLoginServer(loginServer, "https")
	if err != nil {
		klog.Errorf("failed to receive challenge: %s", err)
		return nil, err
	}
	var realmURL *url.URL
	if realmURL, err = url.Parse(directive.realm); err != nil {
		return nil, fmt.Errorf("Www-Authenticate: invalid realm %s", directive.realm)
	}

	clientAssertCredential, err := NewClientAssertionCredential(tenantID, clientID, realmURL.Host, request.ServiceAccountToken, nil)

	if err != nil {
		klog.Fatal("Failed to initialize client assertion credential: %s", err.Error())
		return nil, err
	}
	return clientAssertCredential, nil
}
