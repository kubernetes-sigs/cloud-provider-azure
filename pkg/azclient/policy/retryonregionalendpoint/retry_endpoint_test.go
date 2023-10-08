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

package retryonregionalendpoint_test

import (
	"context"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/retryonregionalendpoint"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

var _ = Describe("RetryEndpoint", func() {
	Describe("RetryEndpoint", func() {
		It("should return on regional endpoint", func() {
			regionalEndpoint := "eastus.management.microsoft.com"
			pipeline := runtime.NewPipeline("testmodule", "v0.1.0", runtime.PipelineOptions{}, &policy.ClientOptions{
				PerCallPolicies: []policy.Policy{
					retryonregionalendpoint.NewPolicy(regionalEndpoint),
					utils.FuncPolicyWrapper(
						func(req *policy.Request) (*http.Response, error) {
							if req.Raw().URL.Host != regionalEndpoint {
								return &http.Response{
									StatusCode: http.StatusOK,
									Body:       http.NoBody,
								}, nil
							}
							return &http.Response{
								Request:    req.Raw(),
								StatusCode: http.StatusOK,
								Body:       streaming.NopCloser(strings.NewReader(`{"content":"content"}`)),
							}, nil
						},
					),
				},
			})
			req, err := runtime.NewRequest(context.Background(), http.MethodGet, "https://management.microsoft.com")
			Expect(err).NotTo(HaveOccurred())
			response, err := pipeline.Do(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(response.StatusCode).To(Equal(http.StatusOK))
			Expect(response.Request.URL.Host).To(Equal(regionalEndpoint))
		})
		It("should return on non-regional endpoint", func() {
			regionalEndpoint := "eastus.management.microsoft.com"
			pipeline := runtime.NewPipeline("testmodule", "v0.1.0", runtime.PipelineOptions{}, &policy.ClientOptions{
				PerCallPolicies: []policy.Policy{
					retryonregionalendpoint.NewPolicy(regionalEndpoint),
					utils.FuncPolicyWrapper(
						func(req *policy.Request) (*http.Response, error) {
							if req.Raw().URL.Host != regionalEndpoint {
								return &http.Response{
									ContentLength: 20,
									Body:          streaming.NopCloser(strings.NewReader(`{"error":{"code":"ResourceGroupNotFound"}}`)),
								}, nil
							}
							return &http.Response{
								Request:    req.Raw(),
								StatusCode: http.StatusOK,
								Body:       streaming.NopCloser(strings.NewReader(`{"content":"content"}`)),
							}, nil
						},
					),
				},
			})
			req, err := runtime.NewRequest(context.Background(), http.MethodGet, "https://management.microsoft.com")
			Expect(err).NotTo(HaveOccurred())
			response, err := pipeline.Do(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(response.StatusCode).To(Equal(http.StatusOK))
			Expect(response.Request.URL.Host).To(Equal(regionalEndpoint))
		})
	})
})
