/*
Copyright 2026 The Kubernetes Authors.

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

package azclient

import (
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// armnetwork v12 client constructors reject an empty subscription ID, so a factory that cannot
// address any subscription fails when it is created instead of on its first request.
var _ = ginkgo.Describe("Factory without a subscription", func() {
	ginkgo.It("should return an error when config is nil", func() {
		factory, err := NewClientFactory(nil, nil, cloud.AzurePublic, nil)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("subscriptionID")))
		gomega.Expect(factory).To(gomega.BeNil())
	})

	ginkgo.It("should return an error when the subscription is empty", func() {
		factory, err := NewClientFactory(&ClientFactoryConfig{}, nil, cloud.AzurePublic, nil)
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("subscriptionID")))
		gomega.Expect(factory).To(gomega.BeNil())
	})
})
