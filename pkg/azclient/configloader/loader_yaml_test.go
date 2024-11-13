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
)

var _ = Describe("LoaderYaml", func() {
	When("invalid config content is provided", func() {
		It("should return error", func() {
			loader := NewYamlByteLoader[TestConfig]([]byte("error"), nil)
			config, err := loader.Load(context.Background())
			Expect(err).NotTo(BeNil())
			Expect(config).To(BeNil())
		})
	})
	When("valid config content is provided", func() {
		It("should return error", func() {
			loader := NewYamlByteLoader[TestConfig]([]byte(`{"value":"123"}`), nil)
			config, err := loader.Load(context.Background())
			Expect(err).To(BeNil())
			Expect(config).NotTo(BeNil())
			Expect(*config.Value).To(Equal("123"))
		})
	})
	When("valid config content is provided with default config", func() {
		It("should return error", func() {
			loader := NewYamlByteLoader[TestConfig]([]byte(`{"value":"123"}`), newFileLoader[TestConfig]("wrong", nil, NewYamlByteLoader[TestConfig]))
			config, err := loader.Load(context.Background())
			Expect(err).NotTo(BeNil())
			Expect(config).To(BeNil())
		})
	})
})
