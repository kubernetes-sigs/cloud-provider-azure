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

package diskclient

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v4"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Diskclient", func() {

	When("requests are raised", func() {
		It("should not return error", func(ctx context.Context) {

			diskResource, err := realClient.CreateOrUpdate(ctx, resourceGroupName, resourceName, armcompute.Disk{
				Location: to.Ptr(location),
				Properties: &armcompute.DiskProperties{
					CreationData: &armcompute.CreationData{
						CreateOption: to.Ptr(armcompute.DiskCreateOptionEmpty),
					},
					DiskSizeGB: to.Ptr[int32](200),
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(diskResource).NotTo(BeNil())
			Expect(*diskResource.Name).To(Equal(resourceName))

			diskResource, err = realClient.Get(ctx, resourceGroupName, resourceName)
			Expect(err).NotTo(HaveOccurred())
			Expect(diskResource).NotTo(BeNil())

			diskList, err := realClient.List(ctx, resourceGroupName)
			Expect(err).NotTo(HaveOccurred())
			Expect(diskList).NotTo(BeNil())
			Expect(len(diskList)).To(Equal(1))
			Expect(*diskList[0].Name).To(Equal(resourceName))

			err = realClient.Delete(ctx, resourceGroupName, resourceName)
			Expect(err).NotTo(HaveOccurred())

		})
	})
	When("raise requests against non-existing resources", func() {
		It("should return error", func(ctx context.Context) {
			diskResource, err := realClient.CreateOrUpdate(ctx, resourceGroupName, resourceName, armcompute.Disk{
				Location: to.Ptr(location),
			})
			Expect(err).To(HaveOccurred())
			Expect(diskResource).To(BeNil())

			diskResource, err = realClient.Get(ctx, resourceGroupName, resourceName+"notfound")
			Expect(err).To(HaveOccurred())
			Expect(diskResource).To(BeNil())

			diskList, err := realClient.List(ctx, resourceGroupName+"notfound")
			Expect(err).To(HaveOccurred())
			Expect(diskList).To(BeNil())
		})
	})
})
