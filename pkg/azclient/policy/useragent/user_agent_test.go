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

package useragent_test

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/useragent"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

var _ = ginkgo.Describe("useragent", func() {
	ginkgo.Describe("useragent", func() {
		ginkgo.It("should respect useragent", func() {
			once := sync.Once{}
			userAgentPolicy := &useragent.CustomUserAgentPolicy{}
			pipeline := runtime.NewPipeline("testmodule", "v0.1.0", runtime.PipelineOptions{}, &policy.ClientOptions{
				Telemetry: policy.TelemetryOptions{
					Disabled: true,
				},
				PerCallPolicies: []policy.Policy{
					userAgentPolicy,
					utils.FuncPolicyWrapper(
						func(*policy.Request) (*http.Response, error) {
							resp := &http.Response{
								StatusCode: http.StatusOK,
								Body:       http.NoBody,
							}
							once.Do(func() {
								resp = &http.Response{
									StatusCode: http.StatusTooManyRequests,
									Body:       http.NoBody,
									Header: http.Header{
										"Retry-After": []string{"10"},
									},
								}
							})
							return resp, nil
						},
					),
				},
			})
			userAgentPolicy.CustomUserAgent = "test"
			req, err := runtime.NewRequest(context.Background(), http.MethodPut, "http://localhost:8080")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = req.SetBody(streaming.NopCloser(strings.NewReader(`{"etag":"etag"}`)), "application/json")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = pipeline.Do(req)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(req.Raw().Header.Get(useragent.HeaderUserAgent)).To(gomega.Equal("test"))
			userAgentPolicy.CustomUserAgent = ""
			req, err = runtime.NewRequest(context.Background(), http.MethodPut, "http://localhost:8080")
			req.Raw().Header.Set(useragent.HeaderUserAgent, "test-override")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = req.SetBody(streaming.NopCloser(strings.NewReader(`{"etag":"etag"}`)), "application/json")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = pipeline.Do(req)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(req.Raw().Header.Get(useragent.HeaderUserAgent)).To(gomega.Equal("test-override"))
			userAgentPolicy.CustomUserAgent = ""
			req, err = runtime.NewRequest(context.Background(), http.MethodPut, "http://localhost:8080")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = req.SetBody(streaming.NopCloser(strings.NewReader(`{"etag":"etag"}`)), "application/json")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = pipeline.Do(req)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			gomega.Expect(req.Raw().Header.Get(useragent.HeaderUserAgent)).To(gomega.BeEmpty())
		})
	})
})
