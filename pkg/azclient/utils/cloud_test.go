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

package utils

import (
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("Cloud", func() {

	ginkgo.Context("AzureCloudFromName", func() {
		ginkgo.When("cloud name is empty", func() {
			ginkgo.It("should return the default cloud", func() {
				cloudConfig := AzureCloudConfigFromName("")
				gomega.Expect(cloudConfig).ToNot(gomega.BeNil())
				gomega.Expect(cloudConfig.ActiveDirectoryAuthorityHost).To(gomega.Equal("https://login.microsoftonline.com/"))
				gomega.Expect(cloudConfig.Services).NotTo(gomega.BeEmpty())
				gomega.Expect(cloudConfig.Services[cloud.ResourceManager].Audience).To(gomega.Equal("https://management.core.windows.net/"))
			})
		})
		ginkgo.When("cloud name is wrong", func() {
			ginkgo.It("should return the default cloud", func() {
				cloudConfig := AzureCloudConfigFromName("wrong")
				gomega.Expect(*cloudConfig).To(gomega.Equal(cloud.AzurePublic))
			})
		})
		ginkgo.When("cloud name is AzureChinaCloud", func() {
			ginkgo.It("should return the AzureChinaCloud", func() {
				cloudConfig := AzureCloudConfigFromName("AzureChinaCloud")
				gomega.Expect(cloudConfig).ToNot(gomega.BeNil())
				gomega.Expect(cloudConfig.ActiveDirectoryAuthorityHost).To(gomega.Equal("https://login.chinacloudapi.cn/"))
				gomega.Expect(cloudConfig.Services).NotTo(gomega.BeEmpty())
				gomega.Expect(cloudConfig.Services[cloud.ResourceManager].Audience).To(gomega.Equal("https://management.core.chinacloudapi.cn"))
			})
		})
	})
})
