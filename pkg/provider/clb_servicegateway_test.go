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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"sigs.k8s.io/cloud-provider-azure/pkg/provider/servicegateway/difftracker"
)

// TestServiceGatewayEnsureLoadBalancerTracksExternalService verifies that EnsureLoadBalancer
// registers an external LoadBalancer service with the difftracker when ServiceGateway is enabled.
func TestServiceGatewayEnsureLoadBalancerTracksExternalService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	az := GetTestCloudWithContainerLoadBalancer(ctrl)
	svc := getTestService("servicegateway-external", v1.ProtocolTCP, nil, false, 80)
	kubeClient := fake.NewSimpleClientset(&svc)
	az.KubeClient = kubeClient
	az.diffTracker = newProviderDiffTracker(t, az, kubeClient)

	status, err := az.EnsureLoadBalancer(context.Background(), testClusterName, &svc, nil)
	assert.NoError(t, err)
	assert.NotNil(t, status)
	assert.True(t, az.diffTracker.IsServiceTracked(difftracker.ServiceUID(&svc)))
}
