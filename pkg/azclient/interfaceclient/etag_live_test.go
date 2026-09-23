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

package interfaceclient

import (
	"context"
	"errors"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/google/uuid"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

func addNICETagLiveTests() {
	ginkgo.It("enforces NIC ETags for stale updates and deleted interfaces", func(ctx context.Context) {
		if !recorder.IsNewCassette() {
			ginkgo.Skip("Live NRP ETag validation requires recording in a disposable Azure subscription")
		}

		name := "etag-" + uuid.NewString()
		created, createErr := realClient.CreateOrUpdate(ctx, resourceGroupName, name, *newResource)
		gomega.Expect(createErr).NotTo(gomega.HaveOccurred())
		gomega.Expect(created).NotTo(gomega.BeNil())
		ginkgo.DeferCleanup(func(cleanupContext context.Context) {
			gomega.Expect(realClient.Delete(cleanupContext, resourceGroupName, name)).To(gomega.Succeed())
		})

		nic, getErr := realClient.Get(ctx, resourceGroupName, name, nil)
		gomega.Expect(getErr).NotTo(gomega.HaveOccurred())
		gomega.Expect(nic).NotTo(gomega.BeNil())
		gomega.Expect(nic.Etag).NotTo(gomega.BeNil())
		gomega.Expect(*nic.Etag).NotTo(gomega.BeEmpty())
		nic.Tags = map[string]*string{"etag-test": to.Ptr("current")}
		updated, updateErr := realClient.CreateOrUpdate(ctx, resourceGroupName, name, *nic)
		gomega.Expect(updateErr).NotTo(gomega.HaveOccurred())
		gomega.Expect(updated).NotTo(gomega.BeNil())

		nic.Tags["etag-test"] = to.Ptr("stale")
		_, updateErr = realClient.CreateOrUpdate(ctx, resourceGroupName, name, *nic)
		expectNICLiveResponseError(updateErr, http.StatusPreconditionFailed, "PreconditionFailed")

		fresh, getErr := realClient.Get(ctx, resourceGroupName, name, nil)
		gomega.Expect(getErr).NotTo(gomega.HaveOccurred())
		gomega.Expect(fresh).NotTo(gomega.BeNil())
		gomega.Expect(fresh.Tags["etag-test"]).To(gomega.Equal(to.Ptr("current")))
		fresh.Tags["etag-test"] = to.Ptr("refreshed")
		updated, updateErr = realClient.CreateOrUpdate(ctx, resourceGroupName, name, *fresh)
		gomega.Expect(updateErr).NotTo(gomega.HaveOccurred())
		gomega.Expect(updated).NotTo(gomega.BeNil())
		gomega.Expect(updated.Etag).NotTo(gomega.BeNil())
		gomega.Expect(*updated.Etag).NotTo(gomega.BeEmpty())

		gomega.Expect(realClient.Delete(ctx, resourceGroupName, name)).To(gomega.Succeed())
		_, getErr = realClient.Get(ctx, resourceGroupName, name, nil)
		expectNICLiveResponseError(getErr, http.StatusNotFound, "")
		_, updateErr = realClient.CreateOrUpdate(ctx, resourceGroupName, name, *updated)
		expectNICLiveResponseError(updateErr, http.StatusPreconditionFailed, "PreconditionFailed")
		_, getErr = realClient.Get(ctx, resourceGroupName, name, nil)
		expectNICLiveResponseError(getErr, http.StatusNotFound, "")
	})
}

func expectNICLiveResponseError(err error, status int, code string) {
	ginkgo.GinkgoHelper()
	var responseError *azcore.ResponseError
	gomega.Expect(errors.As(err, &responseError)).To(gomega.BeTrue(), "expected an Azure response error, got %v", err)
	gomega.Expect(responseError.StatusCode).To(gomega.Equal(status))
	if code != "" {
		gomega.Expect(responseError.ErrorCode).To(gomega.Equal(code))
	}
}
