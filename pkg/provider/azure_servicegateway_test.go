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

package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/servicegateway/difftracker"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// newProviderDiffTracker builds a real engine with empty state from a test Cloud's Azure
// configuration. Provider tests use it to inject a ready tracker instead of calling
// Runtime.Start, which performs live Azure discovery.
func newProviderDiffTracker(t *testing.T, az *Cloud, kubeClient kubernetes.Interface) *difftracker.DiffTracker {
	t.Helper()

	dt, err := difftracker.New(
		log.Noop(),
		difftracker.K8sState{
			Services: utilsets.NewString(),
			Egresses: utilsets.NewString(),
			Nodes:    make(map[string]difftracker.Node),
		},
		difftracker.NRPState{
			LoadBalancers: utilsets.NewString(),
			NATGateways:   utilsets.NewString(),
			Locations:     make(map[string]difftracker.NRPLocation),
		},
		difftracker.Config{
			SubscriptionID:                az.SubscriptionID,
			NetworkResourceSubscriptionID: az.getNetworkResourceSubscriptionID(),
			ResourceGroup:                 az.ResourceGroup,
			Location:                      az.Location,
			VNetName:                      az.VnetName,
			VNetResourceGroup:             az.VnetResourceGroup,
			ServiceGatewayResourceName:    consts.DefaultServiceGatewayResourceName,
		},
		az.NetworkClientFactory,
		kubeClient,
	)
	if !assert.NoError(t, err) {
		t.FailNow()
	}
	return dt
}
