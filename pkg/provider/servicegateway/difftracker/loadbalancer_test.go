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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
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

// TestLoadBalancerEmitsWarningEventForRejectedService pins the user-visible half of a rejection:
// the Service controller only reports a generic SyncLoadBalancerFailed, so without this Event the
// specific reason (which part of the spec is unsupported) never reaches the user.
func TestLoadBalancerEmitsWarningEventForRejectedService(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*v1.Service)
		reason  string
		message string
	}{
		{
			name: "named targetPort",
			mutate: func(svc *v1.Service) {
				svc.Spec.Ports[0].TargetPort = intstr.FromString("http")
			},
			reason: "UnsupportedNamedTargetPort",
		},
		{
			name: "internal load balancer",
			mutate: func(svc *v1.Service) {
				svc.Annotations = map[string]string{
					consts.ServiceAnnotationLoadBalancerInternal: consts.TrueAnnotationValue,
				}
			},
			reason: "UnsupportedInternalLoadBalancer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			svc := newInboundService("service-uid")
			tt.mutate(svc)

			tracker := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(svc))
			recorder := record.NewFakeRecorder(10)
			tracker.SetEventRecorder(recorder)

			lb := NewLoadBalancer(nil)
			assert.NoError(t, lb.SetTracker(tracker))

			_, err := lb.EnsureLoadBalancer(context.Background(), "cluster", svc, nil)
			assert.Error(t, err)
			assert.False(t, tracker.IsServiceTracked(ServiceUID(svc)))

			select {
			case event := <-recorder.Events:
				assert.Contains(t, event, v1.EventTypeWarning)
				assert.Contains(t, event, tt.reason)
			default:
				t.Fatalf("expected a %s warning event on the Service", tt.reason)
			}
		})
	}
}

// TestLoadBalancerDoesNotEmitEventWithoutReason keeps the Event path from turning every failure
// into a Service Event: only errors carrying a reason are surfaced.
func TestLoadBalancerDoesNotEmitEventWithoutReason(t *testing.T) {
	ctrl := gomock.NewController(t)
	svc := newInboundService("service-uid")
	tracker := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(svc))
	recorder := record.NewFakeRecorder(10)
	tracker.SetEventRecorder(recorder)

	recordWarningEvent(tracker, svc, errors.New("some transient failure"))

	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected event recorded: %s", event)
	default:
	}
}

// TestRecordWarningEventWithoutRecorder covers the window before the runtime publishes a recorder.
func TestRecordWarningEventWithoutRecorder(t *testing.T) {
	ctrl := gomock.NewController(t)
	svc := newInboundService("service-uid")
	tracker := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(svc))

	assert.NotPanics(t, func() {
		recordWarningEvent(tracker, svc, &InboundConfigValidationError{Reason: "R", Message: "m"})
	})
}
