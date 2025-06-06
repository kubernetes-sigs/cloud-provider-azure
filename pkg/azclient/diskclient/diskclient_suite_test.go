// /*
// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// */

// Code generated by client-gen. DO NOT EDIT.
package diskclient

import (
	"context"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/recording"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/utils"
)

func TestClient(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "Client Suite")
}

var resourceGroupName = "aks-cit-Disk"
var resourceName = "testResource"

var subscriptionID string
var location string
var resourceGroupClient *armresources.ResourceGroupsClient
var err error
var recorder *recording.Recorder
var cloudConfig *cloud.Configuration
var realClient Interface
var clientOption azcore.ClientOptions

var _ = ginkgo.BeforeSuite(func(ctx context.Context) {
	recorder, cloudConfig, location, err = recording.NewRecorder()
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	subscriptionID = recorder.SubscriptionID()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	cred := recorder.TokenCredential()
	clientOption = azcore.ClientOptions{
		Transport:       recorder.HTTPClient(),
		TracingProvider: utils.TracingProvider,
		Telemetry: policy.TelemetryOptions{
			ApplicationID: "ccm-resource-group-client",
		},
		Cloud: *cloudConfig,
	}
	rgClientOption := clientOption
	rgClientOption.Telemetry.ApplicationID = "ccm-resource-group-client"
	resourceGroupClient, err = armresources.NewResourceGroupsClient(subscriptionID, cred, &arm.ClientOptions{
		ClientOptions: rgClientOption,
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	realClientOption := clientOption
	realClientOption.Telemetry.ApplicationID = "ccm-Disk-client"
	realClient, err = New(subscriptionID, recorder.TokenCredential(), &arm.ClientOptions{
		ClientOptions: realClientOption,
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = resourceGroupClient.CreateOrUpdate(
		ctx,
		resourceGroupName,
		armresources.ResourceGroup{
			Location: to.Ptr(location),
		},
		nil)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
})

var _ = ginkgo.AfterSuite(func(ctx context.Context) {
	poller, err := resourceGroupClient.BeginDelete(ctx, resourceGroupName, nil)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = poller.PollUntilDone(ctx, nil)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	err = recorder.Stop()
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
})
