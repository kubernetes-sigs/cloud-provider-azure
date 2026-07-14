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
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	cloudprovider "k8s.io/cloud-provider"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	azureconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
)

type trackingControllerClientBuilder struct {
	cloudprovider.ControllerClientBuilder
	client kubernetes.Interface
	calls  int
}

func (b *trackingControllerClientBuilder) ClientOrDie(string) kubernetes.Interface {
	b.calls++
	return b.client
}

func serviceGatewayConfig() *azureconfig.Config {
	return &azureconfig.Config{
		ServiceGatewayEnabled:                    true,
		LoadBalancerSKU:                          consts.LoadBalancerSKUService,
		LoadBalancerBackendPoolConfigurationType: consts.LoadBalancerBackendPoolConfigurationTypePodIP,
	}
}

func TestNewCloudPreinitializesKubeClientOnlyForServiceGatewayCCM(t *testing.T) {
	t.Run("ServiceGateway CCM", func(t *testing.T) {
		builder := &trackingControllerClientBuilder{client: fake.NewSimpleClientset()}
		config := serviceGatewayConfig()
		config.MultipleStandardLoadBalancerConfigurations = []azureconfig.MultipleStandardLoadBalancerConfiguration{{}}

		_, err := NewCloud(context.Background(), builder, config, true)

		assert.ErrorContains(t, err, "mutually exclusive")
		assert.Equal(t, 1, builder.calls)
	})

	t.Run("non-ServiceGateway CCM", func(t *testing.T) {
		builder := &trackingControllerClientBuilder{client: fake.NewSimpleClientset()}
		config := &azureconfig.Config{LoadBalancerBackendPoolConfigurationType: "invalid"}

		_, err := NewCloud(context.Background(), builder, config, true)

		assert.ErrorContains(t, err, "is not supported")
		assert.Zero(t, builder.calls)
	})

	t.Run("ServiceGateway non-CCM", func(t *testing.T) {
		builder := &trackingControllerClientBuilder{client: fake.NewSimpleClientset()}
		config := serviceGatewayConfig()
		config.MultipleStandardLoadBalancerConfigurations = []azureconfig.MultipleStandardLoadBalancerConfiguration{{}}

		_, err := NewCloud(context.Background(), builder, config, false)

		assert.ErrorContains(t, err, "mutually exclusive")
		assert.Zero(t, builder.calls)
	})

	t.Run("ServiceGateway CCM requires builder", func(t *testing.T) {
		_, err := NewCloud(context.Background(), nil, serviceGatewayConfig(), true)

		assert.EqualError(t, err, "NewCloud: ServiceGateway requires a clientBuilder")
	})
}

func TestInitializePublishesServiceGatewayDependencies(t *testing.T) {
	ctrl := gomock.NewController(t)
	az := GetTestCloudWithContainerLoadBalancer(ctrl)
	kubeClient := az.KubeClient
	az.diffTracker.SetEventRecorder(nil)

	az.Initialize(nil, nil)
	t.Cleanup(az.eventBroadcaster.Shutdown)

	assert.Same(t, kubeClient, az.KubeClient, "Initialize must preserve the client retained by the difftracker")

	recorder := reflect.ValueOf(az.diffTracker).Elem().FieldByName("eventRecorder")
	assert.True(t, recorder.IsValid(), "difftracker must retain its event recorder dependency")
	assert.False(t, recorder.IsNil(), "Initialize must publish the event recorder to the difftracker")
}
