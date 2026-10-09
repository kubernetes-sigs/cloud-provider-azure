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

// TestLoadBalancerEnsureEchoesExistingStatus pins the echo. The service controller only patches
// when the returned status differs from what it captured before the call, so echoing is what keeps
// it from clearing an ingress IP that updateServiceLoadBalancerStatus already wrote.
func TestLoadBalancerEnsureEchoesExistingStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	svc := newInboundService("service-uid")
	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: "1.2.3.4"}}
	tracker := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(svc))

	lb := NewLoadBalancer(nil)
	assert.NoError(t, lb.SetTracker(tracker))

	status, err := lb.EnsureLoadBalancer(context.Background(), "cluster", svc, nil)
	assert.NoError(t, err)
	if !assert.Equal(t, &svc.Status.LoadBalancer, status) {
		t.FailNow()
	}

	// A copy, not an alias: the controller must not be able to mutate the Service through it.
	status.Ingress[0].IP = "9.9.9.9"
	assert.Equal(t, "1.2.3.4", svc.Status.LoadBalancer.Ingress[0].IP)

	// A Service with no IP yet echoes empty, which is what leaves the LB pending until the
	// engine finishes provisioning.
	pending := newInboundService("pending-uid")
	pendingStatus, err := lb.EnsureLoadBalancer(context.Background(), "cluster", pending, nil)
	assert.NoError(t, err)
	assert.Empty(t, pendingStatus.Ingress)
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
		{
			name: "source ranges",
			mutate: func(svc *v1.Service) {
				svc.Spec.LoadBalancerSourceRanges = []string{"203.0.113.0/24"}
			},
			reason: "UnsupportedAccessRestriction",
		},
		{
			name: "private link service",
			mutate: func(svc *v1.Service) {
				svc.Annotations = map[string]string{consts.ServiceAnnotationPLSCreation: consts.TrueAnnotationValue}
			},
			reason: "UnsupportedPrivateLinkService",
		},
		{
			name: "invalid load balancer IP",
			mutate: func(svc *v1.Service) {
				svc.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerIPDualStack[false]: "not-an-ip"}
			},
			reason: "InvalidLoadBalancerIP",
		},
		{
			name: "annotation without effect",
			mutate: func(svc *v1.Service) {
				svc.Annotations = map[string]string{consts.ServiceAnnotationDisableLoadBalancerFloatingIP: "true"}
			},
			reason: "UnsupportedAnnotations",
		},
		{
			name: "health probe annotation",
			mutate: func(svc *v1.Service) {
				svc.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath: "/healthz"}
			},
			reason: "UnsupportedHealthProbe",
		},
		{
			name: "reserved Public IP tag key",
			mutate: func(svc *v1.Service) {
				svc.Annotations = map[string]string{consts.ServiceAnnotationAzurePIPTags: "k8s-azure-cluster-name=spoof,team=a"}
			},
			reason: "InvalidPIPTags",
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

func TestLoadBalancerRejectsAnnotationsWithoutEffect(t *testing.T) {
	ctrl := gomock.NewController(t)
	svc := newInboundService("service-uid")
	svc.Annotations = map[string]string{
		consts.ServiceAnnotationLoadBalancerResourceGroup:                            "rg",
		consts.ServiceAnnotationLoadBalancerIdleTimeout:                              "10",
		consts.ServiceAnnotationDisableLoadBalancerFloatingIP:                        "false",
		consts.BuildAnnotationKeyForPort(80, consts.PortAnnotationNoHealthProbeRule): "true",
	}
	tracker := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(svc))
	recorder := record.NewFakeRecorder(10)
	tracker.SetEventRecorder(recorder)
	lb := NewLoadBalancer(nil)
	assert.NoError(t, lb.SetTracker(tracker))

	_, err := lb.EnsureLoadBalancer(context.Background(), "cluster", svc, nil)
	assert.Error(t, err)
	assert.False(t, tracker.IsServiceTracked(ServiceUID(svc)), "an annotation without effect must block provisioning")

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, v1.EventTypeWarning)
		assert.Contains(t, event, "UnsupportedAnnotations")
		assert.Contains(t, event, consts.ServiceAnnotationLoadBalancerResourceGroup)
		assert.Contains(t, event, consts.ServiceAnnotationDisableLoadBalancerFloatingIP)
		assert.Contains(t, event, string(consts.PortAnnotationNoHealthProbeRule))
		assert.NotContains(t, event, consts.ServiceAnnotationLoadBalancerIdleTimeout)
	default:
		t.Fatal("expected a warning event listing the unsupported annotations")
	}

	svc.Annotations = map[string]string{consts.ServiceAnnotationLoadBalancerIdleTimeout: "10"}
	_, err = lb.EnsureLoadBalancer(context.Background(), "cluster", svc, nil)
	assert.NoError(t, err)
	assert.True(t, tracker.IsServiceTracked(ServiceUID(svc)), "removing them admits the Service")
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected event for supported annotations only: %s", event)
	default:
	}

	svc.Annotations[consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath] = "/healthz"
	_, err = lb.EnsureLoadBalancer(context.Background(), "cluster", svc, nil)
	assert.Error(t, err)
	assert.True(t, tracker.IsServiceTracked(ServiceUID(svc)), "a provisioned Service that gains one keeps its load balancer")
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, v1.EventTypeWarning)
		assert.Contains(t, event, "UnsupportedHealthProbe")
		assert.Contains(t, event, consts.ServiceAnnotationLoadBalancerHealthProbeRequestPath)
		assert.Contains(t, event, "readinessProbe")
	default:
		t.Fatal("expected a warning event about the health-probe annotation")
	}
	select {
	case event := <-recorder.Events:
		t.Fatalf("health-probe annotations must not also be reported as unsupported: %s", event)
	default:
	}
}

func TestLoadBalancerRecordsClusterName(t *testing.T) {
	ctrl := gomock.NewController(t)
	svc := newInboundService("service-uid")
	svc.Annotations = map[string]string{consts.ServiceAnnotationAzurePIPTags: "team=a"}
	tracker := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(svc))
	lb := NewLoadBalancer(nil)
	assert.NoError(t, lb.SetTracker(tracker))

	_, err := lb.EnsureLoadBalancer(context.Background(), "my-cluster", svc, nil)
	assert.NoError(t, err)

	tracker.mu.Lock()
	config := tracker.pendingServiceOps[ServiceUID(svc)].Config.InboundConfig
	tracker.mu.Unlock()
	assert.Equal(t, "my-cluster", config.ClusterName)
	assert.Equal(t, map[string]string{"team": "a"}, config.PIPTags)
}

// TestLoadBalancerRecordsClusterNameFromEveryCall pins that any load balancer call teaches the tracker the
// cluster name: after a restart it is needed to decide ownership of Public IPs, and a cluster may have no
// Service to ensure for a long time.
func TestLoadBalancerRecordsClusterNameFromEveryCall(t *testing.T) {
	for name, call := range map[string]func(lb *LoadBalancer, svc *v1.Service) error{
		"GetLoadBalancer": func(lb *LoadBalancer, svc *v1.Service) error {
			_, _, err := lb.GetLoadBalancer(context.Background(), "my-cluster", svc)
			return err
		},
		"UpdateLoadBalancer": func(lb *LoadBalancer, svc *v1.Service) error {
			return lb.UpdateLoadBalancer(context.Background(), "my-cluster", svc, nil)
		},
		"EnsureLoadBalancerDeleted": func(lb *LoadBalancer, svc *v1.Service) error {
			return lb.EnsureLoadBalancerDeleted(context.Background(), "my-cluster", svc)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			svc := newInboundService("service-uid")
			tracker := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(svc))
			lb := NewLoadBalancer(nil)
			assert.NoError(t, lb.SetTracker(tracker))
			assert.NoError(t, call(lb, svc))
			assert.Equal(t, "my-cluster", tracker.getClusterName())
		})
	}

	tracker := newTestDiffTracker()
	tracker.SetClusterName("first")
	tracker.SetClusterName("")
	assert.Equal(t, "first", tracker.getClusterName(), "an empty name never replaces a known one")
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

// TestLoadBalancerEnsureDeletedSchedulesTeardown pins the cloud-provider deletion entry point.
//
// The Service controller removes its own load-balancer finalizer as soon as this returns nil, so a
// nil return that did not actually schedule teardown loses the deletion: the Azure resources are
// left behind with no Kubernetes object driving their removal.
func TestLoadBalancerEnsureDeletedSchedulesTeardown(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	svc := newInboundService("service-uid")
	tracker := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(svc))

	lb := NewLoadBalancer(nil)
	assert.NoError(t, lb.SetTracker(tracker))

	_, err := lb.EnsureLoadBalancer(context.Background(), "cluster", svc, nil)
	assert.NoError(t, err)
	assert.True(t, tracker.IsServiceTracked(ServiceUID(svc)))

	assert.NoError(t, lb.EnsureLoadBalancerDeleted(context.Background(), "cluster", svc))

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	opState, tracked := tracker.pendingServiceOps[ServiceUID(svc)]
	if assert.True(t, tracked, "deletion must leave the engine tracking the teardown") {
		assert.Contains(t,
			[]ResourceState{StateDeletionPending, StateDeletionInProgress},
			opState.State,
			"the Service must be moved into a deleting state, not left as-is")
	}
}

// TestLoadBalancerEnsureDeletedRequiresTracker pins that deletion fails loudly when the runtime has
// not published a tracker. Returning nil here would let the Service controller drop its finalizer
// and consider the load balancer deleted while nothing ever tore it down.
func TestLoadBalancerEnsureDeletedRequiresTracker(t *testing.T) {
	lb := NewLoadBalancer(nil)
	err := lb.EnsureLoadBalancerDeleted(context.Background(), "cluster", newInboundService("service-uid"))
	assert.Error(t, err, "an unbound tracker must not report a successful deletion")
}

// TestLoadBalancerEnsureDeletedRejectsUnusableService pins that a Service the engine cannot identify
// is reported as an error rather than silently reported deleted.
func TestLoadBalancerEnsureDeletedRejectsUnusableService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	tracker := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset())
	lb := NewLoadBalancer(nil)
	assert.NoError(t, lb.SetTracker(tracker))

	assert.Error(t, lb.EnsureLoadBalancerDeleted(context.Background(), "cluster", nil),
		"a nil Service must be an error, not a silent success")

	noUID := newInboundService("")
	noUID.UID = ""
	assert.Error(t, lb.EnsureLoadBalancerDeleted(context.Background(), "cluster", noUID),
		"a Service without a UID cannot be identified and must not report success")
}
