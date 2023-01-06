/*
Copyright 2019 The Kubernetes Authors.

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

package utils

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2018-05-01/resources"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/utils/pointer"
)

// CreateTestResourceGroup create a test rg
func CreateTestResourceGroup(tc *AzureTestClient) (*resources.Group, func(string)) {
	gc := tc.createResourceGroupClient()
	rgName := pointer.String("e2e-" + string(uuid.NewUUID())[0:4])
	rg, err := gc.CreateOrUpdate(context.Background(), *rgName, createTestTemplate(tc, rgName))
	Expect(err).NotTo(HaveOccurred())
	By(fmt.Sprintf("resource group %s created", *rgName))

	return &rg, func(rgName string) {
		Logf("cleaning up test resource group %s", rgName)
		future, err := gc.Delete(context.Background(), rgName)
		Expect(err).NotTo(HaveOccurred())
		err = WaitForDeleteResourceGroupCompletion(gc, future, rgName)
		Expect(err).NotTo(HaveOccurred())
	}
}

// WaitForDeleteResourceGroupCompletion waits for delete group operations to finish
func WaitForDeleteResourceGroupCompletion(gc *resources.GroupsClient, future resources.GroupsDeleteFuture, rgName string) error {
	err := future.WaitForCompletionRef(context.Background(), gc.Client)
	if err != nil {
		return err
	}

	Logf("finished deleting group '%s'", rgName)
	return nil
}

func createTestTemplate(tc *AzureTestClient, name *string) resources.Group {
	return resources.Group{
		Name:     name,
		Location: pointer.String(tc.location),
	}
}
