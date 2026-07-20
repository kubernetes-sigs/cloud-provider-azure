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
	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/cloud-provider-azure/pkg/provider/servicegateway"
)

func TestCloudSelectsStableServiceGatewayLoadBalancer(t *testing.T) {
	az := &Cloud{}
	legacy, supported := az.LoadBalancer()
	assert.True(t, supported)
	assert.Same(t, az, legacy)

	az.ServiceGatewayEnabled = true
	az.serviceGatewayRuntime = servicegateway.NewRuntime(az.Config, nil, nil)
	first, supported := az.LoadBalancer()
	assert.True(t, supported)
	assert.NotSame(t, az, first)
	second, supported := az.LoadBalancer()
	assert.True(t, supported)
	assert.Same(t, first, second)
}

func TestServiceGatewayLoadBalancerRequiresRuntimeInitialization(t *testing.T) {
	az := &Cloud{Config: *serviceGatewayConfig()}
	az.serviceGatewayRuntime = servicegateway.NewRuntime(az.Config, nil, nil)
	loadBalancer, _ := az.LoadBalancer()
	service := getTestService("servicegateway-not-started", v1.ProtocolTCP, nil, false, 80)

	status, err := loadBalancer.EnsureLoadBalancer(context.Background(), testClusterName, &service, nil)
	assert.EqualError(t, err, "ServiceGateway LoadBalancer is not initialized")
	assert.Nil(t, status)
	assert.EqualError(t, loadBalancer.UpdateLoadBalancer(context.Background(), testClusterName, &service, nil), "ServiceGateway LoadBalancer is not initialized")
	assert.EqualError(t, loadBalancer.EnsureLoadBalancerDeleted(context.Background(), testClusterName, &service), "ServiceGateway LoadBalancer is not initialized")
	_, _, err = loadBalancer.GetLoadBalancer(context.Background(), testClusterName, &service)
	assert.EqualError(t, err, "ServiceGateway LoadBalancer is not initialized")
}
