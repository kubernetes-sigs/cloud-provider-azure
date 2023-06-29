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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/configloader"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/configloader/mock_configloader"
)

var _ = Describe("LoaderSecret", func() {
	var mockCtrl *gomock.Controller
	var underlayLoader *mock_configloader.MockFactoryConfigLoader
	var k8sConfig *configloader.K8sSecretConfig = &configloader.K8sSecretConfig{}
	var kubeClient *fake.Clientset
	BeforeEach(func() {
		mockCtrl = gomock.NewController(GinkgoT())
		underlayLoader = mock_configloader.NewMockFactoryConfigLoader(mockCtrl)
		kubeClient = fake.NewSimpleClientset()
	})
	AfterEach(func() {
		mockCtrl.Finish()
	})
	Context("configfile is not valid", func() {
		BeforeEach(func() {
			underlayLoader.EXPECT().Load(gomock.Any()).Return(&azclient.ClientFactoryConfig{}, nil).AnyTimes()
		})
		It("should return error", func() {
			loader := configloader.NewK8sSecretLoader[azclient.ClientFactoryConfig](k8sConfig, kubeClient, underlayLoader, configloader.NewYamlByteLoader[azclient.ClientFactoryConfig])
			Expect(loader).NotTo(BeNil())
			config, err := loader.Load(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(config).To(BeNil())
		})
		It("should return error", func() {
			kubeClient.CoreV1().Secrets(configloader.DefaultCloudProviderConfigSecNamespace).Create(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configloader.DefaultCloudProviderConfigSecName,
					Namespace: configloader.DefaultCloudProviderConfigSecNamespace,
				},
				Data: nil,
			}, metav1.CreateOptions{})
			loader := configloader.NewK8sSecretLoader[azclient.ClientFactoryConfig](k8sConfig, kubeClient, underlayLoader, configloader.NewYamlByteLoader[azclient.ClientFactoryConfig])
			Expect(loader).NotTo(BeNil())
			config, err := loader.Load(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(config).To(BeNil())
		})
	})
	Context("configfile is valid", func() {
		BeforeEach(func() {
			underlayLoader.EXPECT().Load(gomock.Any()).Return(&azclient.ClientFactoryConfig{SubscriptionID: "123"}, nil)
		})
		It("should not return error", func() {
			kubeClient.CoreV1().Secrets(configloader.DefaultCloudProviderConfigSecNamespace).Create(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configloader.DefaultCloudProviderConfigSecName,
					Namespace: configloader.DefaultCloudProviderConfigSecNamespace,
				},
				Data: map[string][]byte{
					configloader.DefaultCloudProviderConfigSecKey: {},
				},
			}, metav1.CreateOptions{})
			loader := configloader.NewK8sSecretLoader[azclient.ClientFactoryConfig](k8sConfig, kubeClient, underlayLoader, configloader.NewYamlByteLoader[azclient.ClientFactoryConfig])
			Expect(loader).NotTo(BeNil())
			config, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(config).NotTo(BeNil())
			Expect(config.SubscriptionID).To(Equal("123"))
		})
		It("should load config from file", func() {
			var content = []byte(`{"subscriptionId": "123"}`)
			kubeClient.CoreV1().Secrets(configloader.DefaultCloudProviderConfigSecNamespace).Create(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      configloader.DefaultCloudProviderConfigSecName,
					Namespace: configloader.DefaultCloudProviderConfigSecNamespace,
				},
				Data: map[string][]byte{
					configloader.DefaultCloudProviderConfigSecKey: content,
				},
			}, metav1.CreateOptions{})
			loader := configloader.NewK8sSecretLoader[azclient.ClientFactoryConfig](k8sConfig, kubeClient, underlayLoader, configloader.NewYamlByteLoader[azclient.ClientFactoryConfig])
			Expect(loader).NotTo(BeNil())
			config, err := loader.Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(config).NotTo(BeNil())
			Expect(config.SubscriptionID).To(Equal("123"))
		})
	})
})
