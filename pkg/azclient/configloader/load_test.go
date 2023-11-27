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

package configloader

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type TestConfig struct {
	ConfigMergeConfig
	Value               *string `json:"value,omitempty" yaml:"value,omitempty"`
	Cloud               *string `json:"cloud,omitempty" yaml:"cloud,omitempty"`                             // configured both in secret and file
	FromSecret          bool    `json:"fromSecret,omitempty" yaml:"fromSecret,omitempty"`                   // be true only in secret config
	UseInstanceMetadata bool    `json:"useInstanceMetadata,omitempty" yaml:"useInstanceMetadata,omitempty"` // be true only in file config
}

var _ = Describe("Load", func() {
	var fakeKubeClient *fake.Clientset
	BeforeEach(func() {
		fakeKubeClient = fake.NewSimpleClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "azure-cloud-provider",

				Namespace: "kube-system",
			},
			Data: map[string][]byte{
				"cloud-config": []byte(`{"cloud": "AzureCloud","fromSecret": true}`),
			},
		})
	})
	When("No config is provided", func() {
		It("should return default config", func() {
			loader := newEmptyLoader[TestConfig](nil)
			config, err := loader.Load(context.Background())
			Expect(err).To(BeNil())
			Expect(config).NotTo(BeNil())
			Expect(config.Value).To(BeNil())
		})
	})
	When("file config is provided", func() {
		var fileLoaderConfig *FileLoaderConfig

		BeforeEach(func() {
			fileLoaderConfig = &FileLoaderConfig{FilePath: "testdata/azure.json"}
		})
		When("config merge type is file", func() {
			It("should return file config", func() {
				config, err := Load[TestConfig](context.Background(), nil, fileLoaderConfig)
				Expect(err).To(BeNil())
				Expect(config).NotTo(BeNil())
				Expect(*config.Cloud).To(Equal("AzurePublicCloud"))
				Expect(config.FromSecret).To(BeFalse())
				Expect(config.UseInstanceMetadata).To(BeTrue())
			})
		})
		When("secret config is provided", func() {
			var secretLoaderConfig *K8sSecretLoaderConfig
			BeforeEach(func() {
				secretLoaderConfig = &K8sSecretLoaderConfig{
					K8sSecretConfig: K8sSecretConfig{
						SecretName:      "azure-cloud-provider",
						SecretNamespace: "kube-system",
						CloudConfigKey:  "cloud-config",
					},
					KubeClient: fakeKubeClient,
				}
			})
			When("config merge type is secret", func() {
				It("should return file config", func() {
					fileLoaderConfig := &FileLoaderConfig{FilePath: "testdata/azure_secret.json"}
					config, err := Load[TestConfig](context.Background(), secretLoaderConfig, fileLoaderConfig)
					Expect(err).To(BeNil())
					Expect(config).NotTo(BeNil())
					Expect(*config.Cloud).To(Equal("AzureCloud"))
					Expect(config.FromSecret).To(BeTrue())
					Expect(config.UseInstanceMetadata).To(BeFalse())
				})
			})
			When("config merge type is merge", func() {
				It("should return file config", func() {
					fileLoaderConfig := &FileLoaderConfig{FilePath: "testdata/azure_merge.json"}
					config, err := Load[TestConfig](context.Background(), secretLoaderConfig, fileLoaderConfig)
					Expect(err).To(BeNil())
					Expect(config).NotTo(BeNil())
					Expect(*config.Cloud).To(Equal("AzureCloud"))
					Expect(config.FromSecret).To(BeTrue())
					Expect(config.UseInstanceMetadata).To(BeTrue())
				})
			})
		})
	})
	When("only secret config is provided", func() {
		var secretLoaderConfig *K8sSecretLoaderConfig
		BeforeEach(func() {
			secretLoaderConfig = &K8sSecretLoaderConfig{
				K8sSecretConfig: K8sSecretConfig{
					SecretName:      "azure-cloud-provider",
					SecretNamespace: "kube-system",
					CloudConfigKey:  "cloud-config",
				},
				KubeClient: fakeKubeClient,
			}
		})
		It("should return secret config", func() {
			config, err := Load[TestConfig](context.Background(), secretLoaderConfig, nil)
			Expect(err).To(BeNil())
			Expect(config).NotTo(BeNil())
			Expect(*config.Cloud).To(Equal("AzureCloud"))
			Expect(config.FromSecret).To(BeTrue())
			Expect(config.UseInstanceMetadata).To(BeFalse())
		})
	})

})
