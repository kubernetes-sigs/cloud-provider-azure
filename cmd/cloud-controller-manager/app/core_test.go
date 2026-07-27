/*
Copyright 2021 The Kubernetes Authors.

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

package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	controllermetrics "k8s.io/component-base/metrics/prometheus/controllers"
	genericcontrollermanager "k8s.io/controller-manager/app"

	cloudcontrollerconfig "sigs.k8s.io/cloud-provider-azure/cmd/cloud-controller-manager/app/config"
	nodeipamconfig "sigs.k8s.io/cloud-provider-azure/pkg/nodeipam/config"
	providerconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/servicegateway"
)

type fakeServiceGatewayRuntimeProvider struct {
	cloudprovider.Interface
	runtime *servicegateway.Runtime
}

func (f *fakeServiceGatewayRuntimeProvider) ServiceGatewayRuntime() *servicegateway.Runtime {
	return f.runtime
}

func (f *fakeServiceGatewayRuntimeProvider) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	if f.runtime == nil {
		return nil, false
	}
	return f.runtime.LoadBalancer()
}

type staticControllerClientBuilder struct {
	cloudprovider.ControllerClientBuilder
	client kubernetes.Interface
}

func (b staticControllerClientBuilder) ClientOrDie(string) kubernetes.Interface {
	return b.client
}

func TestSetNodeCIDRMaskSizesDualStack(t *testing.T) {
	for _, testCase := range []struct {
		description                        string
		mask, ipv4Mask, ipv6Mask           int32
		expectedIPV4Mask, expectedIPV6Mask int
	}{
		{
			description:      "setNodeCIDRMaskSizesDualStack should ignore the node cidr mask size",
			mask:             17,
			ipv6Mask:         65,
			expectedIPV4Mask: 24,
			expectedIPV6Mask: 65,
		},
		{
			description:      "setNodeCIDRMaskSizesDualStack should set the ipv4 and ipv6 mask sizes as configured",
			mask:             17,
			ipv4Mask:         18,
			ipv6Mask:         65,
			expectedIPV4Mask: 18,
			expectedIPV6Mask: 65,
		},
		{
			description:      "setNodeCIDRMaskSizesDualStack should set the default ipv4 and ipv6 mask sizes",
			mask:             17,
			expectedIPV4Mask: 24,
			expectedIPV6Mask: 64,
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			cfg := nodeipamconfig.NodeIPAMControllerConfiguration{
				NodeCIDRMaskSize:     testCase.mask,
				NodeCIDRMaskSizeIPv4: testCase.ipv4Mask,
				NodeCIDRMaskSizeIPv6: testCase.ipv6Mask,
			}

			ipv4Mask, ipv6Mask, err := setNodeCIDRMaskSizesDualStack(cfg)
			assert.NoError(t, err)
			assert.Equal(t, testCase.expectedIPV4Mask, ipv4Mask)
			assert.Equal(t, testCase.expectedIPV6Mask, ipv6Mask)
		})
	}
}

func TestValidateServiceGatewayControllerConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		cloud       any
		controllers []string
		wantErr     string
	}{
		{
			name:        "non ServiceGateway provider",
			cloud:       struct{}{},
			controllers: []string{"*"},
		},
		{
			name:        "ServiceGateway disabled",
			cloud:       &fakeServiceGatewayRuntimeProvider{runtime: servicegateway.NewRuntime(providerconfig.Config{}, nil, nil)},
			controllers: []string{"-service-lb-controller"},
		},
		{
			name:        "service controller enabled",
			cloud:       &fakeServiceGatewayRuntimeProvider{runtime: servicegateway.NewRuntime(providerconfig.Config{ServiceGatewayEnabled: true}, nil, nil)},
			controllers: []string{"*"},
		},
		{
			name:        "service controller disabled",
			cloud:       &fakeServiceGatewayRuntimeProvider{runtime: servicegateway.NewRuntime(providerconfig.Config{ServiceGatewayEnabled: true}, nil, nil)},
			controllers: []string{"-service-lb-controller"},
			wantErr:     `ServiceGateway requires "service-lb-controller" to be enabled`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateServiceGatewayControllerConfiguration(test.cloud, test.controllers)
			if test.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.EqualError(t, err, test.wantErr)
		})
	}
}

func TestStartServiceControllerBootstrapsServiceGatewayBeforeLoadBalancerCapture(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	config := (&cloudcontrollerconfig.Config{
		LoopbackClientConfig: &rest.Config{},
		ClientBuilder:        staticControllerClientBuilder{client: kubeClient},
		SharedInformers:      informers.NewSharedInformerFactory(kubeClient, 0),
	}).Complete()
	runtime := servicegateway.NewRuntime(providerconfig.Config{ServiceGatewayEnabled: true}, nil, kubeClient)
	runtime.SetEventRecorder(record.NewFakeRecorder(1))
	cloud := &fakeServiceGatewayRuntimeProvider{runtime: runtime}
	loadBalancer, supported := runtime.LoadBalancer()
	assert.True(t, supported)
	service := &v1.Service{}
	_, err := loadBalancer.EnsureLoadBalancer(context.Background(), "cluster", service, nil)
	assert.EqualError(t, err, "ServiceGateway LoadBalancer is not initialized")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, started, err := startServiceController(
		ctx,
		genericcontrollermanager.ControllerContext{
			ControllerManagerMetrics: controllermetrics.NewControllerManagerMetrics("test"),
		},
		config,
		cloud,
	)

	assert.NoError(t, err)
	assert.True(t, started)
	_, err = loadBalancer.EnsureLoadBalancer(context.Background(), "cluster", service, nil)
	assert.NoError(t, err)
}
