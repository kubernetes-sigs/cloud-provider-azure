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

package configloader_test

import (
	"context"
	"os"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/configloader"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/configloader/mock_configloader"
)

var _ = Describe("LoaderFile", func() {
	var mockCtrl *gomock.Controller
	var underlayLoader *mock_configloader.MockFactoryConfigLoader
	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		underlayLoader = mock_configloader.NewMockFactoryConfigLoader(mockCtrl)
	})
	AfterEach(func() {
		mockCtrl.Finish()
	})
	Context("configfile is not valid", func() {
		BeforeEach(func() {
			underlayLoader.EXPECT().Load(gomock.Any()).Return(&azclient.ClientFactoryConfig{}, nil).AnyTimes()
		})
		It("should return empty", func() {
			loader := configloader.NewFileLoader[azclient.ClientFactoryConfig]("notexistfile", underlayLoader, configloader.NewYamlByteLoader[azclient.ClientFactoryConfig])
			Expect(loader).NotTo(BeNil())
			config, err := loader.Load(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(config).To(BeNil())
		})
	})
	Context("configfile is not nil", func() {
		var configFile *os.File
		BeforeEach(func() {
			underlayLoader.EXPECT().Load(gomock.Any()).Return(&azclient.ClientFactoryConfig{SubscriptionID: "123"}, nil)
			configFile, _ = os.CreateTemp("", "configfile")
		})
		AfterEach(func() {
			os.Remove(configFile.Name())
		})

		It("should keep old config", func() {
			loader := configloader.NewFileLoader[azclient.ClientFactoryConfig](configFile.Name(), underlayLoader, configloader.NewYamlByteLoader[azclient.ClientFactoryConfig])
			Expect(loader).NotTo(BeNil())
			config, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(config).NotTo(BeNil())
			Expect(config.SubscriptionID).To(Equal("123"))
		})

		It("should load config from file", func() {
			os.WriteFile(configFile.Name(), []byte("subscriptionId: 123"), 0644)
			loader := configloader.NewFileLoader[azclient.ClientFactoryConfig](configFile.Name(), underlayLoader, configloader.NewYamlByteLoader[azclient.ClientFactoryConfig])
			Expect(loader).NotTo(BeNil())
			config, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(config).NotTo(BeNil())
			Expect(config.SubscriptionID).To(Equal("123"))
		})
	})
})
