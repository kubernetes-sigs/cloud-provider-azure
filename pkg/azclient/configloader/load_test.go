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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/configloader"
)

var _ = Describe("Load", func() {
	When("authconfig is selected", func() {
		It("should return empty", func() {
			loader, err := configloader.Load[azclient.ClientFactoryConfig](context.Background(), nil, &configloader.FileLoaderConfig{
				FilePath: "testdata/azure.json",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(loader).NotTo(BeNil())
		})
	})
	When("cloud config is selected", func() {
		It("should return empty", func() {
			loader, err := configloader.Load[azclient.ARMClientConfig](context.Background(), nil, &configloader.FileLoaderConfig{
				FilePath: "testdata/azure.json",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(loader).NotTo(BeNil())
		})
	})

	When("client factory config is selected", func() {
		It("should return empty", func() {
			loader, err := configloader.Load[azclient.ClientFactoryConfig](context.Background(), nil, &configloader.FileLoaderConfig{
				FilePath: "testdata/azure.json",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(loader).NotTo(BeNil())
		})
	})
})
