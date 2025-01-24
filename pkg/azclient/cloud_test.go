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

package azclient_test

import (
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
)

var _ = ginkgo.Describe("Cloud", func() {
	ginkgo.Context("AzureCloudFromUrl", func() {
		ginkgo.When("the url is valid", func() {
			ginkgo.It("should return the cloud", func() {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, err := w.Write([]byte(`
[
    {
        "portal":"https://portal.azure.com",
        "authentication":{
            "loginEndpoint":"https://login.microsoftonline.com/",
            "audiences":[
                "https://management.core.windows.net/",
                "https://management.azure.com/"
            ],
            "tenant":"common",
            "identityProvider":"AAD"
        }
,
        "media":"https://rest.media.azure.net",
        "graphAudience":"https://graph.windows.net/",
        "graph":"https://graph.windows.net/",
        "name":"AzureCloud",
        "suffixes":{
            "azureDataLakeStoreFileSystem":"azuredatalakestore.net",
            "acrLoginServer":"azurecr.io",
            "sqlServerHostname":"database.windows.net",
            "azureDataLakeAnalyticsCatalogAndJob":"azuredatalakeanalytics.net",
            "keyVaultDns":"vault.azure.net",
            "storage":"core.windows.net",
            "azureFrontDoorEndpointSuffix":"azurefd.net"
        }
,
        "batch":"https://batch.core.windows.net/",
        "resourceManager":"https://management.azure.com/",
        "vmImageAliasDoc":"https://raw.githubusercontent.com/Azure/azure-rest-api-specs/master/arm-compute/quickstart-templates/aliases.json",
        "activeDirectoryDataLake":"https://datalake.azure.net/",
        "sqlManagement":"https://management.core.windows.net:8443/",
        "gallery":"https://gallery.azure.com/"
    }

]
					`))
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
				}))
				defer server.Close()
				cloudConfig := cloud.AzurePublic
				env := &azclient.Environment{}
				err := azclient.OverrideAzureCloudConfigAndEnvConfigFromMetadataService(server.URL, "AzureCloud", &cloudConfig, env)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(cloudConfig).ToNot(gomega.BeNil())
				gomega.Expect(cloudConfig.ActiveDirectoryAuthorityHost).To(gomega.Equal("https://login.microsoftonline.com/"))
				gomega.Expect(cloudConfig.Services).NotTo(gomega.BeEmpty())
				gomega.Expect(cloudConfig.Services[cloud.ResourceManager].Audience).To(gomega.Equal("https://management.core.windows.net/"))
				gomega.Expect(env).ToNot(gomega.BeNil())
				gomega.Expect(env.ResourceManagerEndpoint).To(gomega.Equal("https://management.azure.com/"))
				gomega.Expect(env.ContainerRegistryDNSSuffix).To(gomega.Equal("azurecr.io"))
				gomega.Expect(env.StorageEndpointSuffix).To(gomega.Equal("core.windows.net"))
			})
		})

		ginkgo.When("the resourceManager is not returned from a valid url", func() {
			ginkgo.It("should substitute the url into the response", func() {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, err := w.Write([]byte(`
[
    {
        "portal":"https://portal.azure.com",
        "authentication":{
            "loginEndpoint":"https://login.microsoftonline.com/",
            "audiences":[
                "https://management.core.windows.net/",
                "https://management.azure.com/"
            ],
            "tenant":"common",
            "identityProvider":"AAD"
        }
,
        "media":"https://rest.media.azure.net",
        "graphAudience":"https://graph.windows.net/",
        "graph":"https://graph.windows.net/",
        "name":"AzureCloud",
        "suffixes":{
            "azureDataLakeStoreFileSystem":"azuredatalakestore.net",
            "acrLoginServer":"azurecr.io",
            "sqlServerHostname":"database.windows.net",
            "azureDataLakeAnalyticsCatalogAndJob":"azuredatalakeanalytics.net",
            "keyVaultDns":"vault.azure.net",
            "storage":"core.windows.net",
            "azureFrontDoorEndpointSuffix":"azurefd.net"
        }
,
        "batch":"https://batch.core.windows.net/",
        "vmImageAliasDoc":"https://raw.githubusercontent.com/Azure/azure-rest-api-specs/master/arm-compute/quickstart-templates/aliases.json",
        "activeDirectoryDataLake":"https://datalake.azure.net/",
        "sqlManagement":"https://management.core.windows.net:8443/",
        "gallery":"https://gallery.azure.com/"
    }

]
					`))
					gomega.Expect(err).ToNot(gomega.HaveOccurred())
				}))
				defer server.Close()
				cloudConfig := cloud.AzurePublic
				env := &azclient.Environment{}
				err := azclient.OverrideAzureCloudConfigAndEnvConfigFromMetadataService(server.URL, "AzureCloud", &cloudConfig, env)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(cloudConfig).ToNot(gomega.BeNil())
				gomega.Expect(cloudConfig.ActiveDirectoryAuthorityHost).To(gomega.Equal("https://login.microsoftonline.com/"))
				gomega.Expect(cloudConfig.Services).NotTo(gomega.BeEmpty())
				gomega.Expect(cloudConfig.Services[cloud.ResourceManager].Audience).To(gomega.Equal("https://management.core.windows.net/"))
				gomega.Expect(cloudConfig.Services[cloud.ResourceManager].Endpoint).To(gomega.Equal(server.URL))
				gomega.Expect(env).ToNot(gomega.BeNil())
				gomega.Expect(env.ResourceManagerEndpoint).To(gomega.Equal(server.URL))
			})
		})
	})
	ginkgo.Context("AzureCloudFromName", func() {
		ginkgo.When("cloud name is empty", func() {
			ginkgo.It("should return the default cloud", func() {
				cloudConfig := azclient.AzureCloudConfigFromName("")
				gomega.Expect(cloudConfig).ToNot(gomega.BeNil())
				gomega.Expect(cloudConfig.ActiveDirectoryAuthorityHost).To(gomega.Equal("https://login.microsoftonline.com/"))
				gomega.Expect(cloudConfig.Services).NotTo(gomega.BeEmpty())
				gomega.Expect(cloudConfig.Services[cloud.ResourceManager].Audience).To(gomega.Equal("https://management.core.windows.net/"))
			})
		})
		ginkgo.When("cloud name is wrong", func() {
			ginkgo.It("should return the default cloud", func() {
				cloudConfig := azclient.AzureCloudConfigFromName("wrong")
				gomega.Expect(*cloudConfig).To(gomega.Equal(cloud.AzurePublic))
			})
		})
		ginkgo.When("cloud name is AzureChinaCloud", func() {
			ginkgo.It("should return the AzureChinaCloud", func() {
				cloudConfig := azclient.AzureCloudConfigFromName("AzureChinaCloud")
				gomega.Expect(cloudConfig).ToNot(gomega.BeNil())
				gomega.Expect(cloudConfig.ActiveDirectoryAuthorityHost).To(gomega.Equal("https://login.chinacloudapi.cn/"))
				gomega.Expect(cloudConfig.Services).NotTo(gomega.BeEmpty())
				gomega.Expect(cloudConfig.Services[cloud.ResourceManager].Audience).To(gomega.Equal("https://management.core.chinacloudapi.cn"))
			})
		})
	})

	ginkgo.Context("AzureCloudFromEnvironment", func() {
		ginkgo.When("the environment is empty", func() {
			ginkgo.It("should return the default cloud", func() {
				env := &azclient.Environment{}
				cloudConfig := &cloud.AzurePublic
				err := azclient.OverrideAzureCloudConfigFromEnv(azclient.AzureStackCloudName, cloudConfig, env)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(cloudConfig).ToNot(gomega.BeNil())
				gomega.Expect(cloudConfig.ActiveDirectoryAuthorityHost).To(gomega.Equal("https://login.microsoftonline.com/"))
				gomega.Expect(cloudConfig.Services).NotTo(gomega.BeEmpty())
				gomega.Expect(cloudConfig.Services[cloud.ResourceManager].Audience).To(gomega.Equal("https://management.core.windows.net/"))
			})
		})

		ginkgo.When("the environment is set,file is not found", func() {
			ginkgo.It("should return error", func() {
				os.Setenv(azclient.EnvironmentFilepathName, "notfound")
				env := &azclient.Environment{}
				cloudConfig := &cloud.AzurePublic
				err := azclient.OverrideAzureCloudConfigFromEnv(azclient.AzureStackCloudName, cloudConfig, env)
				gomega.Expect(err).To(gomega.HaveOccurred())
				os.Unsetenv(azclient.EnvironmentFilepathName)
			})
		})
		ginkgo.When("the environment is set,file is empty", func() {
			ginkgo.It("should return error", func() {
				os.Setenv(azclient.EnvironmentFilepathName, "notfound")
				env := &azclient.Environment{}
				cloudConfig := &cloud.AzurePublic
				err := azclient.OverrideAzureCloudConfigFromEnv(azclient.AzureStackCloudName, cloudConfig, env)
				gomega.Expect(err).To(gomega.HaveOccurred())
				os.Unsetenv(azclient.EnvironmentFilepathName)
			})
		})
		ginkgo.When("the environment is set,file is correct", func() {
			ginkgo.It("should return error", func() {
				configFile, err := os.CreateTemp("", "azure.json")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				defer os.Remove(configFile.Name())

				err = os.WriteFile(configFile.Name(), []byte(`
				{
                   "resourceManagerEndpoint":"https://management.chinacloudapi.cn",
				   "activeDirectoryEndpoint":"https://login.chinacloudapi.cn",
				   "tokenAudience":"https://management.core.chinacloudapi.cn/"
				}`), 0600)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				os.Setenv(azclient.EnvironmentFilepathName, configFile.Name())
				env := &azclient.Environment{}
				cloudConfig := &cloud.AzureGovernment
				err = azclient.OverrideAzureCloudConfigFromEnv(azclient.AzureStackCloudName, cloudConfig, env)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(cloudConfig).ToNot(gomega.BeNil())
				gomega.Expect(cloudConfig.ActiveDirectoryAuthorityHost).To(gomega.Equal("https://login.chinacloudapi.cn"))
				gomega.Expect(cloudConfig.Services).NotTo(gomega.BeEmpty())
				gomega.Expect(cloudConfig.Services[cloud.ResourceManager].Audience).To(gomega.Equal("https://management.core.chinacloudapi.cn/"))
				gomega.Expect(env).ToNot(gomega.BeNil())
				gomega.Expect(env.ResourceManagerEndpoint).To(gomega.Equal("https://management.chinacloudapi.cn"))
				os.Unsetenv(azclient.EnvironmentFilepathName)
			})
		})
		ginkgo.When("the environment is set,file is correct", func() {
			ginkgo.It("should return error", func() {
				configFile, err := os.CreateTemp("", "azure.json")
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				defer os.Remove(configFile.Name())

				err = os.WriteFile(configFile.Name(), []byte(`
				{
                   "resourceManagerEndpoint":"https://management.chinacloudapi.cn",
				   "activeDirectoryEndpoint":"https://login.chinacloudapi.cn",
				   "tokenAudience":"https://management.core.chinacloudapi.cn/"
				}`), 0600)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				os.Setenv(azclient.EnvironmentFilepathName, configFile.Name())
				env := &azclient.Environment{}
				cloudConfig := &cloud.AzurePublic
				err = azclient.OverrideAzureCloudConfigFromEnv("AZUREPUBLICCLOUD", cloudConfig, env)
				gomega.Expect(err).ToNot(gomega.HaveOccurred())
				gomega.Expect(cloudConfig).ToNot(gomega.BeNil())
				gomega.Expect(cloudConfig.ActiveDirectoryAuthorityHost).NotTo(gomega.Equal("https://login.chinacloudapi.cn"))
				gomega.Expect(cloudConfig.Services).NotTo(gomega.BeEmpty())
				gomega.Expect(cloudConfig.Services[cloud.ResourceManager].Audience).NotTo(gomega.Equal("https://management.core.chinacloudapi.cn/"))
				gomega.Expect(env).ToNot(gomega.BeNil())
				gomega.Expect(env.ResourceManagerEndpoint).NotTo(gomega.Equal("https://management.chinacloudapi.cn"))
				os.Unsetenv(azclient.EnvironmentFilepathName)
			})
		})
	})
})
