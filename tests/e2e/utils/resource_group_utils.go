package utils

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/services/resources/mgmt/2018-05-01/resources"
	"github.com/Azure/go-autorest/autorest/to"

	"k8s.io/apimachinery/pkg/util/uuid"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// CreateTestResourceGroup create a test rg
func CreateTestResourceGroup(tc *AzureTestClient) (*resources.Group, func(string)) {
	gc := tc.createResourceGroupClient()
	rgName := to.StringPtr("e2e-" + string(uuid.NewUUID())[0:4])
	rg, err := gc.CreateOrUpdate(context.Background(), *rgName, createTestTemplate(tc, rgName))
	Expect(err).NotTo(HaveOccurred())
	By(fmt.Sprintf("resource group %s created", *rgName))

	return &rg, func(rgName string) {
		Logf("cleaning up test resource group %s", rgName)
		_, err := gc.Delete(context.Background(), rgName)
		Expect(err).NotTo(HaveOccurred())
		return
	}
}

func createTestTemplate(tc *AzureTestClient, name *string) resources.Group {
	return resources.Group{
		Name:     name,
		Location: to.StringPtr(tc.location),
	}
}
