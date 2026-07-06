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

package retryrepectthrottled_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/retryrepectthrottled"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

var _ = ginkgo.Describe("Throttle", func() {
	ginkgo.Describe("Throttle", func() {
		ginkgo.It("should respect retry-after", func() {
			once := sync.Once{}
			throttlePolicy := &retryrepectthrottled.ThrottlingPolicy{}
			pipeline := runtime.NewPipeline("testmodule", "v0.1.0", runtime.PipelineOptions{}, &policy.ClientOptions{
				PerCallPolicies: []policy.Policy{
					throttlePolicy,
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
			req, err := runtime.NewRequest(context.Background(), http.MethodPut, "http://localhost:8080")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = req.SetBody(streaming.NopCloser(strings.NewReader(`{"etag":"etag"}`)), "application/json")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = pipeline.Do(req)
			gomega.Expect(err).To(gomega.HaveOccurred())
			req, err = runtime.NewRequest(context.Background(), http.MethodPut, "http://localhost:8080")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = pipeline.Do(req)
			gomega.Expect(err).To(gomega.HaveOccurred())
			throttlePolicy.RetryAfterWriter = time.Now().Add(-time.Second * 10)
			req, err = runtime.NewRequest(context.Background(), http.MethodPut, "http://localhost:8080")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = req.SetBody(streaming.NopCloser(strings.NewReader(`{"etag":"etag"}`)), "application/json")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = pipeline.Do(req)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})

	ginkgo.Describe("ThrottleError", func() {
		before := time.Now()
		rfc1123Future := time.Now().Add(120 * time.Second).UTC()
		pastTimer := time.Now().Add(-1 * time.Hour)
		futureTimer := time.Now().Add(30 * time.Second)

		ginkgo.DescribeTable("should populate RetryAfter correctly",
			func(initialTimer *time.Time, responseStatus int, retryAfterValue string, check func(time.Time)) {
				throttlePolicy := &retryrepectthrottled.ThrottlingPolicy{}
				if initialTimer != nil {
					throttlePolicy.RetryAfterWriter = *initialTimer
				}
				pipeline := runtime.NewPipeline("testmodule", "v0.1.0", runtime.PipelineOptions{}, &policy.ClientOptions{
					PerCallPolicies: []policy.Policy{
						throttlePolicy,
						utils.FuncPolicyWrapper(
							func(*policy.Request) (*http.Response, error) {
								header := http.Header{}
								if retryAfterValue != "" {
									header.Set("Retry-After", retryAfterValue)
								}
								return &http.Response{
									StatusCode: responseStatus,
									Body:       http.NoBody,
									Header:     header,
								}, nil
							},
						),
					},
				})
				req, err := runtime.NewRequest(context.Background(), http.MethodPut, "http://localhost:8080")
				gomega.Expect(err).NotTo(gomega.HaveOccurred())
				_, err = pipeline.Do(req)
				gomega.Expect(err).To(gomega.HaveOccurred())
				var throttleErr *retryrepectthrottled.ThrottleError
				gomega.Expect(errors.As(err, &throttleErr)).To(gomega.BeTrue())
				check(throttleErr.RetryAfter)
			},
			ginkgo.Entry("ARM 429 with integer seconds Retry-After",
				nil, http.StatusTooManyRequests, "60",
				func(retryAfter time.Time) {
					gomega.Expect(retryAfter).To(gomega.BeTemporally(">=", before.Add(60*time.Second), time.Second))
				}),
			ginkgo.Entry("ARM 429 with RFC1123 Retry-After",
				nil, http.StatusTooManyRequests, rfc1123Future.Format(time.RFC1123),
				func(retryAfter time.Time) {
					gomega.Expect(retryAfter).To(gomega.BeTemporally("~", rfc1123Future, time.Second))
				}),
			ginkgo.Entry("ARM 429 without Retry-After header",
				nil, http.StatusTooManyRequests, "",
				func(retryAfter time.Time) {
					gomega.Expect(retryAfter).To(gomega.BeTemporally("<=", time.Now(), time.Second))
				}),
			ginkgo.Entry("ARM 429 with unparsable Retry-After",
				&pastTimer, http.StatusTooManyRequests, "not-a-number-or-date",
				func(retryAfter time.Time) {
					gomega.Expect(retryAfter).To(gomega.BeTemporally("~", pastTimer, time.Millisecond))
				}),
			ginkgo.Entry("local gate path with active timer",
				&futureTimer, http.StatusOK, "",
				func(retryAfter time.Time) {
					gomega.Expect(retryAfter).To(gomega.BeTemporally("~", futureTimer, time.Millisecond))
				}),
		)

		ginkgo.It("should satisfy errors.Is(err, ErrTooManyRequest) for ThrottleError", func() {
			err := &retryrepectthrottled.ThrottleError{RetryAfter: time.Now()}
			gomega.Expect(errors.Is(err, retryrepectthrottled.ErrTooManyRequest)).To(gomega.BeTrue())
		})

		ginkgo.It("should allow errors.As to extract ThrottleError with correct RetryAfter", func() {
			expected := time.Now().Add(42 * time.Second)
			var err error = &retryrepectthrottled.ThrottleError{RetryAfter: expected}
			var throttleErr *retryrepectthrottled.ThrottleError
			gomega.Expect(errors.As(err, &throttleErr)).To(gomega.BeTrue())
			gomega.Expect(throttleErr.RetryAfter).To(gomega.BeTemporally("~", expected, time.Millisecond))
		})

		ginkgo.It("should return the same error string as ErrTooManyRequest", func() {
			err := &retryrepectthrottled.ThrottleError{RetryAfter: time.Now()}
			gomega.Expect(err.Error()).To(gomega.Equal(retryrepectthrottled.ErrTooManyRequest.Error()))
		})
	})
})
