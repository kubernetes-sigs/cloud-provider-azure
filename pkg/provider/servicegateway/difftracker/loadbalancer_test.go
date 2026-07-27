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

package difftracker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func newInboundService(uid string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "servicegateway-external",
			UID:       types.UID(uid),
		},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeLoadBalancer,
			Ports: []v1.ServicePort{
				{
					Name:     "http",
					Protocol: v1.ProtocolTCP,
					Port:     80,
					NodePort: 50080,
				},
			},
		},
	}
}

// TestLoadBalancerEnsureTracksExternalService verifies that EnsureLoadBalancer registers an
// external LoadBalancer Service with the engine, so the Service becomes tracked and is reported
// as existing by GetLoadBalancer.
func TestLoadBalancerEnsureTracksExternalService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	svc := newInboundService("service-uid")
	tracker := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(svc))

	lb := NewLoadBalancer(nil)
	assert.NoError(t, lb.SetTracker(tracker))
	assert.False(t, tracker.IsServiceTracked(ServiceUID(svc)))

	status, err := lb.EnsureLoadBalancer(context.Background(), "cluster", svc, nil)
	assert.NoError(t, err)
	assert.NotNil(t, status)
	assert.True(t, tracker.IsServiceTracked(ServiceUID(svc)))

	_, exists, err := lb.GetLoadBalancer(context.Background(), "cluster", svc)
	assert.NoError(t, err)
	assert.True(t, exists)
}
