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

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/configloader"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/configloader/mock_configloader"
)

var _ = Describe("LoaderYaml", func() {
	var mockCtrl *gomock.Controller
	var underlayLoader *mock_configloader.MockFactoryConfigLoader
	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		underlayLoader = mock_configloader.NewMockFactoryConfigLoader(mockCtrl)
	})
	AfterEach(func() {
		mockCtrl.Finish()
	})

	Context("newYamlByteLoader", func() {
		It("should return nil if no config is provided", func() {
			Expect(configloader.NewYamlByteLoader[azclient.ClientFactoryConfig]([]byte{}, nil)).NotTo(BeNil())
		})
		It("should return the config if provided", func() {
			Expect(configloader.NewYamlByteLoader[azclient.ClientFactoryConfig]([]byte{}, underlayLoader)).NotTo(BeNil())
		})
	})
	Context("yaml is invalid", func() {
		When("yaml is empty", func() {
			It("should return empty", func() {
				underlayLoader.EXPECT().Load(gomock.Any()).Return(&azclient.ClientFactoryConfig{}, nil)
				loader := configloader.NewYamlByteLoader[azclient.ClientFactoryConfig]([]byte{}, underlayLoader)
				config, err := loader.Load(context.Background())
				Expect(err).NotTo(HaveOccurred())
				Expect(config).NotTo(BeNil())
			})
		})
	})
	Context("yaml is valid", func() {
		When("config is nil", func() {
			When("yaml is not empty", func() {
				It("should return config", func() {
					underlayLoader.EXPECT().Load(gomock.Any()).Return(&azclient.ClientFactoryConfig{}, nil)
					loader := configloader.NewYamlByteLoader[azclient.ClientFactoryConfig]([]byte(`subscriptionId: 123`), underlayLoader)
					config, err := loader.Load(context.Background())
					Expect(err).NotTo(HaveOccurred())
					Expect(config).NotTo(BeNil())
					Expect(config.SubscriptionID).To(Equal("123"))
				})
			})
		})
		When("config is not nil", func() {
			It("should keep old config", func() {
				underlayLoader.EXPECT().Load(gomock.Any()).Return(&azclient.ClientFactoryConfig{CloudProviderBackoff: true}, nil)
				loader := configloader.NewYamlByteLoader[azclient.ClientFactoryConfig]([]byte(`subscriptionId: 123`), underlayLoader)
				config, err := loader.Load(context.Background())
				Expect(err).NotTo(HaveOccurred())
				Expect(config).NotTo(BeNil())
				Expect(config.SubscriptionID).To(Equal("123"))
				Expect(config.CloudProviderBackoff).To(BeTrue())
			})
			It("should overwrite config", func() {
				underlayLoader.EXPECT().Load(gomock.Any()).Return(&azclient.ClientFactoryConfig{SubscriptionID: "456"}, nil)
				loader := configloader.NewYamlByteLoader[azclient.ClientFactoryConfig]([]byte(`subscriptionId: 123`), underlayLoader)
				config, err := loader.Load(context.Background())
				Expect(err).NotTo(HaveOccurred())
				Expect(config).NotTo(BeNil())
				Expect(config.SubscriptionID).To(Equal("123"))
			})
		})
	})
	Context("yaml is invalid", func() {
		When("yaml is actually a json", func() {
			It("should not return error", func() {
				underlayLoader.EXPECT().Load(gomock.Any()).Return(&azclient.ClientFactoryConfig{}, nil)
				loader := configloader.NewYamlByteLoader[azclient.ClientFactoryConfig]([]byte(`{"subscriptionId": "123"}`), underlayLoader)
				config, err := loader.Load(context.Background())
				Expect(err).NotTo(HaveOccurred())
				Expect(config).NotTo(BeNil())
				Expect(config.SubscriptionID).To(Equal("123"))
			})
		})
	})
})
