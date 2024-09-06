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

package etag_test

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/policy/etag"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

var _ = ginkgo.Describe("Etag", func() {
	ginkgo.Describe("AppendEtag", func() {
		ginkgo.It("should append etag", func() {
			pipeline := runtime.NewPipeline("testmodule", "v0.1.0", runtime.PipelineOptions{}, &policy.ClientOptions{
				PerCallPolicies: []policy.Policy{
					utils.FuncPolicyWrapper(etag.AppendEtag),
					utils.FuncPolicyWrapper(
						func(req *policy.Request) (*http.Response, error) {
							gomega.Expect(req.Raw().Header.Get("If-Match")).To(gomega.Equal("etag"))
							body, err := io.ReadAll(req.Body())
							gomega.Expect(err).NotTo(gomega.HaveOccurred())
							gomega.Expect(string(body)).To(gomega.Equal(`{"etag":"etag"}`))
							return nil, nil
						},
					),
				},
			})
			req, err := runtime.NewRequest(context.Background(), http.MethodPut, "http://localhost:8080")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			err = req.SetBody(streaming.NopCloser(strings.NewReader(`{"etag":"etag"}`)), "application/json")
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
			_, err = pipeline.Do(req)
			gomega.Expect(err).NotTo(gomega.HaveOccurred())
		})
	})
})
