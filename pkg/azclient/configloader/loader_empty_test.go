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
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("LoaderEmpty", func() {
	When("default config is not provided", func() {
		It("should return nil config", func() {
			loader := newEmptyLoader[TestConfig](nil)
			config, err := loader.Load(nil)
			Expect(err).To(BeNil())
			Expect(config).NotTo(BeNil())
			Expect(config.Value).To(BeNil())
		})
	})

	When("default config is provided", func() {
		It("should return default config", func() {
			var value string = "default"
			loader := newEmptyLoader[TestConfig](&TestConfig{Value: to.Ptr(value)})
			config, err := loader.Load(nil)
			Expect(err).To(BeNil())
			Expect(config).NotTo(BeNil())
			Expect(*config.Value).To(Equal(value))
		})
	})
})
