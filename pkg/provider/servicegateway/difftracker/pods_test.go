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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// TestPodInformerAddPod tests the podInformerAddPod function
func TestPodInformerAddPod(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name           string
		pod            *v1.Pod
		expectAddPod   bool
		expectedCalls  int
		expectedPodKey string
		expectedEgress string
		expectedHostIP string
		expectedPodIP  string
	}{
		{
			name:           "Valid pod with egress label and IPs should trigger AddPod",
			pod:            newTestPod("default", "test-pod", "egress-gateway-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			expectAddPod:   true,
			expectedCalls:  1,
			expectedPodKey: "default/test-pod",
			expectedEgress: "egress-gateway-a",
			expectedHostIP: "10.0.0.1",
			expectedPodIP:  "10.0.1.1",
		},
		{
			name:           "Pod in Pending phase with IPs should trigger AddPod",
			pod:            newTestPod("default", "pending-pod", "egress-b", "10.0.0.2", "10.0.1.2", v1.PodPending, nil),
			expectAddPod:   true,
			expectedCalls:  1,
			expectedPodKey: "default/pending-pod",
			expectedEgress: "egress-b",
		},
		{
			name:          "Pod without egress label should be skipped",
			pod:           newTestPod("default", "no-label", "", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			expectAddPod:  false,
			expectedCalls: 0,
		},
		{
			name:          "Pod with DeletionTimestamp should be skipped",
			pod:           newTestPod("default", "deleting", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, &now),
			expectAddPod:  false,
			expectedCalls: 0,
		},
		{
			name:          "Pod in Failed phase should be skipped",
			pod:           newTestPod("default", "failed-pod", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodFailed, nil),
			expectAddPod:  false,
			expectedCalls: 0,
		},
		{
			name:          "Pod in Succeeded phase should be skipped",
			pod:           newTestPod("default", "succeeded-pod", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodSucceeded, nil),
			expectAddPod:  false,
			expectedCalls: 0,
		},
		{
			name:          "Pod in Unknown phase should be skipped",
			pod:           newTestPod("default", "unknown-pod", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodUnknown, nil),
			expectAddPod:  false,
			expectedCalls: 0,
		},
		{
			name:          "Pod without HostIP should be skipped",
			pod:           newTestPod("default", "no-hostip", "egress-a", "", "10.0.1.1", v1.PodRunning, nil),
			expectAddPod:  false,
			expectedCalls: 0,
		},
		{
			name:          "Pod without PodIP should be skipped",
			pod:           newTestPod("default", "no-podip", "egress-a", "10.0.0.1", "", v1.PodRunning, nil),
			expectAddPod:  false,
			expectedCalls: 0,
		},
		{
			name:          "Pod without any IPs should be skipped",
			pod:           newTestPod("default", "no-ips", "egress-a", "", "", v1.PodPending, nil),
			expectAddPod:  false,
			expectedCalls: 0,
		},
		{
			name:           "Egress label should be case-insensitive (converted to lowercase)",
			pod:            newTestPod("default", "case-test", "Egress-Gateway-UPPER", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			expectAddPod:   true,
			expectedCalls:  1,
			expectedEgress: "egress-gateway-upper",
		},
		{
			name:          "Pod with path-traversal egress label should be skipped",
			pod:           newTestPod("default", "evil-pod", "../hijacked-nat", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			expectAddPod:  false,
			expectedCalls: 0,
		},
		{
			name:          "Pod with slash in egress label should be skipped",
			pod:           newTestPod("default", "slash-pod", "egress/gw", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			expectAddPod:  false,
			expectedCalls: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			dt := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(tt.pod))

			dt.podInformerAddPod(tt.pod)

			// On an empty engine a registered egress pod is buffered under pendingPods, keyed by its
			// (lowercased) egress identity and carrying the node location (HostIP) and address (PodIP).
			if !tt.expectAddPod {
				assert.Empty(t, dt.pendingPods, "a skipped pod must not be registered with the engine")
				return
			}
			assert.Len(t, dt.pendingPods, tt.expectedCalls)
			buffered := dt.pendingPods[tt.expectedEgress]
			if !assert.Len(t, buffered, 1, "expected the egress pod buffered under %q", tt.expectedEgress) {
				return
			}
			if tt.expectedPodKey != "" {
				assert.Equal(t, tt.expectedPodKey, buffered[0].PodKey)
			}
			if tt.expectedHostIP != "" {
				assert.Equal(t, tt.expectedHostIP, buffered[0].Location)
			}
			if tt.expectedPodIP != "" {
				assert.Equal(t, tt.expectedPodIP, buffered[0].Address)
			}
		})
	}
}

// TestPodInformerDualStackRegistersEveryFamily verifies the egress informer registers each IP family
// of a dual-stack pod under its SAME-FAMILY node location - the IPv6 PodIP under the node's IPv6 IP
// (Status.HostIPs), NOT under the (IPv4) HostIP. NRP rejects a location that mixes families
// (IPv4LocationCannotContainIPv6Addresses), so filing the IPv6 address under the IPv4 node location
// makes the whole registration fail. It runs against a real engine (NAT gateway pre-seeded so the pod
// registers live rather than buffering) and fails if the add path drops a family or misfiles it.
func TestPodInformerDualStackRegistersEveryFamily(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		egress = "egress-a"
		v4Node = "10.0.0.1"
		v6Node = "fd00::a"
		v4Pod  = "10.244.0.1"
		v6Pod  = "fd00:244::1"
	)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ds", Namespace: "default", UID: types.UID("uid-ds"),
			Labels: map[string]string{consts.PodLabelServiceEgressGateway: egress},
		},
		Status: v1.PodStatus{
			HostIP:  v4Node,
			HostIPs: []v1.HostIP{{IP: v4Node}, {IP: v6Node}},
			PodIP:   v4Pod,
			PodIPs:  []v1.PodIP{{IP: v4Pod}, {IP: v6Pod}},
			Phase:   v1.PodRunning,
		},
	}

	kube := fake.NewSimpleClientset(pod)
	dt, _ := newSeededDiffTracker(t, ctrl, kube,
		K8sState{Services: utilsets.NewString(), Egresses: utilsets.NewString(egress), Nodes: map[string]Node{}},
		NRPState{LoadBalancers: utilsets.NewString(), NATGateways: utilsets.NewString(egress), Locations: map[string]NRPLocation{}})

	dt.podInformerAddPod(pod)
	v4Pods := dt.K8sResources.Nodes[v4Node].Pods
	v6Pods := dt.K8sResources.Nodes[v6Node].Pods
	assert.Contains(t, v4Pods, v4Pod, "the IPv4 PodIP must register under the IPv4 node location")
	assert.Contains(t, v6Pods, v6Pod, "the IPv6 PodIP must register under the IPv6 node location")
	assert.NotContains(t, v4Pods, v6Pod, "the IPv6 PodIP must NOT be filed under the IPv4 node location (NRP rejects mixed-family locations)")

	dt.podInformerRemovePod(pod)
	assert.NotContains(t, dt.K8sResources.Nodes[v4Node].Pods, v4Pod, "the IPv4 family must drain on remove")
	assert.NotContains(t, dt.K8sResources.Nodes[v6Node].Pods, v6Pod, "the IPv6 family must drain on remove")
}

// TestPodInformerAddPod_FinalizerAddFailureRegistersAndAlerts verifies that when AddPodFinalizer fails
// after its retries (sustained apiserver outage), podInformerAddPod must STILL register the pod
// (returning would silently kill its egress) and make the rare unprotected-pod state observable via
// a warning Event (and the pod_finalizer_add_failed_total metric).
func TestPodInformerAddPod_FinalizerAddFailureRegistersAndAlerts(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "egress-p",
			Namespace: "default",
			Labels:    map[string]string{consts.PodLabelServiceEgressGateway: "egress-svc"},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning, HostIP: "10.0.0.1", PodIP: "10.244.0.1"},
	}
	kube := fake.NewSimpleClientset(pod)
	// Persistent non-NotFound error on the finalizer Update -> AddPodFinalizer exhausts its retries.
	kube.PrependReactor("update", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(fmt.Errorf("apiserver down"))
	})

	dt := newProviderDiffTracker(t, ctrl, kube)
	rec := record.NewFakeRecorder(10)
	dt.eventRecorder = rec

	dt.podInformerAddPod(pod)

	// The pod must still be registered with the engine despite the finalizer add failure.
	assert.True(t, dt.IsServiceTracked("egress-svc"),
		"pod must still be registered (AddPod called) even when the finalizer could not be added")

	// And the rare unprotected-pod state must be surfaced as a warning Event.
	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "ServiceGatewayFinalizerAddFailed",
			"a warning Event must be emitted when an egress pod is registered without its cleanup finalizer")
	default:
		t.Fatal("expected a ServiceGatewayFinalizerAddFailed warning Event on finalizer add failure")
	}
}

// TestPodInformerAddPod_RejectsInvalidEgressLabel verifies that an egress pod whose label is not a
// valid Azure resource name (e.g. a path-traversal value) is NOT registered with the engine and a
// warning Event is emitted, so the label can never be interpolated raw into a NAT Gateway ARM ID.
func TestPodInformerAddPod_RejectsInvalidEgressLabel(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "evil-egress",
			Namespace: "default",
			Labels:    map[string]string{consts.PodLabelServiceEgressGateway: "../hijacked-nat"},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning, HostIP: "10.0.0.1", PodIP: "10.244.0.1"},
	}
	kube := fake.NewSimpleClientset(pod)

	dt := newProviderDiffTracker(t, ctrl, kube)
	rec := record.NewFakeRecorder(10)
	dt.eventRecorder = rec

	dt.podInformerAddPod(pod)

	assert.False(t, dt.IsServiceTracked("../hijacked-nat"),
		"a pod with an invalid egress label must not be registered with the engine")

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "ServiceGatewayInvalidEgressLabel",
			"a warning Event must be emitted for an invalid egress gateway label")
	default:
		t.Fatal("expected a ServiceGatewayInvalidEgressLabel warning Event")
	}
}

// TestPodInformerAddPod_SkipsStaleReplacedPod verifies that a stale Add event for a pod that has
// since been replaced by a same-name pod with a different UID does NOT register the stale event
// pod's address. AddPodFinalizer declines to add the finalizer to the replacement (it is UID-guarded)
// and signals ErrPodGoneOrReplaced; the handler must abort rather than register an unprotected
// mapping (no finalizer to drain it) for an IP that may already be reclaimed.
func TestPodInformerAddPod_SkipsStaleReplacedPod(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const egress = "egress-svc"
	// The live pod at default/egress-p is the replacement (new UID).
	livePod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "egress-p",
			Namespace: "default",
			UID:       "uid-new",
			Labels:    map[string]string{consts.PodLabelServiceEgressGateway: egress},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning, HostIP: "10.0.0.1", PodIP: "10.244.0.2"},
	}
	// The stale Add event carries the old pod (old UID, different IP).
	stalePod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "egress-p",
			Namespace: "default",
			UID:       "uid-old",
			Labels:    map[string]string{consts.PodLabelServiceEgressGateway: egress},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning, HostIP: "10.0.0.1", PodIP: "10.244.0.1"},
	}
	kube := fake.NewSimpleClientset(livePod)

	dt := newProviderDiffTracker(t, ctrl, kube)

	dt.podInformerAddPod(stalePod)

	assert.False(t, dt.IsServiceTracked(egress),
		"a stale event for a UID-replaced pod must not register the stale address")
	if node, ok := dt.K8sResources.Nodes["10.0.0.1"]; ok {
		assert.NotContains(t, node.Pods, "10.244.0.1",
			"the stale pod's IP must not be registered under the node location")
	}
}

func TestPodInformerAddPod_RejectsMalformedPodIP(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "malformed-ip-egress",
			Namespace: "default",
			Labels:    map[string]string{consts.PodLabelServiceEgressGateway: "egress-svc"},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning, HostIP: "10.0.0.1", PodIP: "not-an-ip"},
	}
	kube := fake.NewSimpleClientset(pod)

	dt := newProviderDiffTracker(t, ctrl, kube)
	rec := record.NewFakeRecorder(10)
	dt.eventRecorder = rec

	dt.podInformerAddPod(pod)

	assert.False(t, dt.IsServiceTracked("egress-svc"),
		"a pod with a malformed PodIP must not be registered with the engine")

	select {
	case ev := <-rec.Events:
		assert.Contains(t, ev, "ServiceGatewayInvalidPodIP",
			"a warning Event must be emitted for a malformed pod IP")
	default:
		t.Fatal("expected a ServiceGatewayInvalidPodIP warning Event")
	}
}

// TestPodInformerRemovePod tests the podInformerRemovePod function
func TestPodInformerRemovePod(t *testing.T) {
	tests := []struct {
		name            string
		pod             *v1.Pod
		expectDeletePod bool
	}{
		{
			name:            "Valid pod with egress label and IPs should trigger DeletePod",
			pod:             newTestPod("default", "test-pod", "egress-gateway-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			expectDeletePod: true,
		},
		{
			name:            "Pod without egress label should be skipped",
			pod:             newTestPod("default", "no-label", "", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			expectDeletePod: false,
		},
		{
			name:            "Pod without HostIP should be skipped with warning",
			pod:             newTestPod("default", "no-hostip", "egress-a", "", "10.0.1.1", v1.PodRunning, nil),
			expectDeletePod: false,
		},
		{
			name:            "Pod without PodIP should be skipped with warning",
			pod:             newTestPod("default", "no-podip", "egress-a", "10.0.0.1", "", v1.PodRunning, nil),
			expectDeletePod: false,
		},
		{
			name:            "Pod in any phase with IPs should trigger DeletePod (phase doesn't matter for removal)",
			pod:             newTestPod("default", "failed-pod", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodFailed, nil),
			expectDeletePod: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			// Seed the engine so a valid pod's address is registered: routing it to DeletePod then
			// drain-gates the address (observable via HasPendingPodDeletion), while the no-label and
			// no-IP paths never reach DeletePod.
			egress := tt.pod.Labels[consts.PodLabelServiceEgressGateway]
			k8s := K8sState{Services: utilsets.NewString(), Egresses: utilsets.NewString(), Nodes: map[string]Node{}}
			nrp := NRPState{LoadBalancers: utilsets.NewString(), NATGateways: utilsets.NewString(), Locations: map[string]NRPLocation{}}
			if egress != "" && tt.pod.Status.HostIP != "" && tt.pod.Status.PodIP != "" {
				k8s.Egresses.Insert(egress)
				k8s.Nodes[tt.pod.Status.HostIP] = Node{Pods: map[string]Pod{
					tt.pod.Status.PodIP: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: egress},
				}}
				nrp.NATGateways.Insert(egress)
			}
			pod := tt.pod.DeepCopy()
			pod.Finalizers = []string{ServiceGatewayPodCleanupFinalizer}
			dt := seededDT(t, ctrl, fake.NewSimpleClientset(pod), k8s, nrp)

			dt.podInformerRemovePod(pod)

			if tt.expectDeletePod {
				assert.True(t, dt.HasPendingPodDeletion(pod.Namespace, pod.Name, string(pod.UID)),
					"a valid egress pod must be routed to DeletePod, which drain-gates the registered address")
			} else {
				assert.False(t, dt.HasPendingPodDeletion(pod.Namespace, pod.Name, string(pod.UID)),
					"a skipped pod must not be drain-gated")
			}
		})
	}
}

// TestPodInformerRemovePod_UntrackedPodFinalizerRemovedDirectly verifies that when the engine is
// not tracking the pod (a stale/duplicate delete, or a pod no longer in live state after a CCM
// restart), podInformerRemovePod removes the ServiceGateway finalizer directly. There is nothing
// to drain from NRP, so the pod must not be stranded in Terminating. Regression test for the
// drain-gating gap where DeletePod's stale-pod early return skipped the pendingPodDeletions enqueue.
func TestPodInformerRemovePod_UntrackedPodFinalizerRemovedDirectly(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "egress-stale",
			Namespace:  "default",
			Labels:     map[string]string{consts.PodLabelServiceEgressGateway: "egress-svc"},
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
		Status: v1.PodStatus{HostIP: "10.0.0.1", PodIP: "10.244.0.1"},
	}
	kubeClient := fake.NewSimpleClientset(pod)

	dt := newProviderDiffTracker(t, ctrl, kubeClient)

	dt.podInformerRemovePod(pod)

	got, err := kubeClient.CoreV1().Pods("default").Get(context.Background(), "egress-stale", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"an untracked egress pod's finalizer must be removed directly so it is not stranded in Terminating")
}

// TestPodInformerRemovePod_NoIPPodFinalizerRemovedDirectly verifies the same direct removal when a
// deleted egress pod has no IPs (so its NRP address cannot be identified): there is nothing to
// drain, so the finalizer must be removed rather than stranding the pod.
func TestPodInformerRemovePod_NoIPPodFinalizerRemovedDirectly(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "egress-noip",
			Namespace:  "default",
			Labels:     map[string]string{consts.PodLabelServiceEgressGateway: "egress-svc"},
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
		Status: v1.PodStatus{}, // no HostIP/PodIP
	}
	kubeClient := fake.NewSimpleClientset(pod)

	dt := newProviderDiffTracker(t, ctrl, kubeClient)

	dt.podInformerRemovePod(pod)

	got, err := kubeClient.CoreV1().Pods("default").Get(context.Background(), "egress-noip", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"a no-IP egress pod's finalizer must be removed directly so it is not stranded")
}

// TestPodInformerDrainForReplace_LiveReRegistrationDoesNotStripFinalizer proves the live
// re-registration drain enqueues no strippable finalizer record. Two live pods share the service (so
// removing one is a non-last delete) and the address is absent from NRP, so a pending record would be
// strippable at once; the contrast sub-test uses podInformerRemovePod to show the record path would
// strip the finalizer.
func TestPodInformerDrainForReplace_LiveReRegistrationDoesNotStripFinalizer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		egress = "egress-svc"
		hostIP = "10.0.0.1"
		addrA  = "10.244.0.1"
		addrB  = "10.244.0.2"
	)

	// Two live pods on the same node/egress service so removing pod A is a non-last delete. New()
	// seeds the outbound ref-counter to 2 from these pods.
	seedState := func() K8sState {
		return K8sState{
			Services: utilsets.NewString(),
			Egresses: utilsets.NewString(egress),
			Nodes: map[string]Node{
				hostIP: {Pods: map[string]Pod{
					addrA: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: egress},
					addrB: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: egress},
				}},
			},
		}
	}
	// addrA is not registered in NRP (already drained), so a non-last pending record would strip at once.
	drainedNRP := func() NRPState {
		return NRPState{
			LoadBalancers: utilsets.NewString(),
			NATGateways:   utilsets.NewString(egress),
			Locations:     make(map[string]NRPLocation),
		}
	}
	podA := func(ip string) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "pod-a",
				Namespace:  "default",
				UID:        types.UID("uid-a"),
				Labels:     map[string]string{consts.PodLabelServiceEgressGateway: egress},
				Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
			},
			Status: v1.PodStatus{HostIP: hostIP, PodIP: ip, PodIPs: []v1.PodIP{{IP: ip}}, Phase: v1.PodRunning},
		}
	}

	t.Run("drainForReplace leaves the live pod's finalizer intact", func(t *testing.T) {
		kube := fake.NewSimpleClientset(podA(addrA))
		dt := seededDT(t, ctrl, kube, seedState(), drainedNRP())

		// A same-service address change (addrA -> addrC) drains addrA with empty namespace/name, so
		// no finalizer-deletion record is enqueued for the still-live pod.
		dt.podInformerDrainForReplace(podA(addrA), podA("10.244.0.3"))
		dt.CheckPendingPodDeletions(context.Background())

		got, err := kube.CoreV1().Pods("default").Get(context.Background(), "pod-a", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
			"a live re-registration drain must not enqueue a strippable record, so the finalizer must survive")
	})

	t.Run("contrast: the record-enqueuing removal path would strip it", func(t *testing.T) {
		kube := fake.NewSimpleClientset(podA(addrA))
		dt := seededDT(t, ctrl, kube, seedState(), drainedNRP())

		dt.podInformerRemovePod(podA(addrA)) // enqueues a non-last drain-gated record
		dt.CheckPendingPodDeletions(context.Background())

		got, err := kube.CoreV1().Pods("default").Get(context.Background(), "pod-a", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
			"the record-enqueuing removal drains the address then strips the finalizer once it has left NRP")
	})
}

// TestPodInformerDrainForReplace_SoleDualStackGainKeepsSharedAddress guards the delta-drain: a sole
// egress pod gaining a secondary family ([v4] -> [v4,v6]) must NOT drain the shared v4. Draining the
// full old set would drop the sole pod's service ref-count to zero, transiently marking the NAT
// Gateway for deletion - which a concurrent ServiceUpdater could act on and tear down under the
// still-live pod (egress outage). Only (old - new) is drained, which is empty here.
func TestPodInformerDrainForReplace_SoleDualStackGainKeepsSharedAddress(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		egress = "egress-svc"
		hostIP = "10.0.0.1"
		v4     = "10.244.0.1"
		v6     = "fd00::1"
	)

	// A single live pod carrying only v4: New() seeds the ref-count to 1.
	k8s := K8sState{
		Services: utilsets.NewString(),
		Egresses: utilsets.NewString(egress),
		Nodes: map[string]Node{
			hostIP: {Pods: map[string]Pod{
				v4: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: egress},
			}},
		},
	}
	nrp := NRPState{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(egress),
		Locations:     make(map[string]NRPLocation),
	}
	pod := func(ips ...string) *v1.Pod {
		podIPs := make([]v1.PodIP, 0, len(ips))
		for _, ip := range ips {
			podIPs = append(podIPs, v1.PodIP{IP: ip})
		}
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-a", Namespace: "default", UID: types.UID("uid-a"),
				Labels:     map[string]string{consts.PodLabelServiceEgressGateway: egress},
				Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
			},
			Status: v1.PodStatus{HostIP: hostIP, PodIP: ips[0], PodIPs: podIPs, Phase: v1.PodRunning},
		}
	}

	kube := fake.NewSimpleClientset(pod(v4, v6))
	dt, _ := newSeededDiffTracker(t, ctrl, kube, k8s, nrp)

	dt.podInformerDrainForReplace(pod(v4), pod(v4, v6))

	// v4 must still be the sole live address: draining it now is the last-pod case. Had the full old
	// set been drained, v4 would already be gone and this would be a no-op (IsLastPod=false).
	res := dt.DeletePod(egress, hostIP, []string{v4}, "default", "pod-a", "uid-a")
	assert.True(t, res.IsLastPod,
		"a dual-stack gain must keep the shared v4 registered, so the sole pod's service is never emptied/torn down mid-replace")
	assert.True(t, res.Enqueued)
}

// TestPodInformerRemovePod_NoIPDeleteKeepsFinalizerWhileDrainPending checks that a terminating egress
// pod whose IPs the kubelet already cleared keeps its cleanup finalizer while an earlier drain is
// pending: stripping it inline would reclaim the pod (and its IP) while the NAT Gateway still maps the
// address, misrouting egress. CheckPendingPodDeletions strips it once NRP confirms the drain.
func TestPodInformerRemovePod_NoIPDeleteKeepsFinalizerWhileDrainPending(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		egress = "egress-svc"
		hostIP = "10.0.0.1"
		v4     = "10.244.0.1"
	)

	k8s := K8sState{
		Services: utilsets.NewString(),
		Egresses: utilsets.NewString(egress),
		Nodes: map[string]Node{
			hostIP: {Pods: map[string]Pod{
				v4: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: egress},
			}},
		},
	}
	nrp := NRPState{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(egress),
		Locations:     make(map[string]NRPLocation),
	}

	newPod := func(withIPs bool) *v1.Pod {
		status := v1.PodStatus{Phase: v1.PodRunning}
		if withIPs {
			status.HostIP = hostIP
			status.PodIP = v4
			status.PodIPs = []v1.PodIP{{IP: v4}}
		}
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-a", Namespace: "default", UID: types.UID("uid-a"),
				Labels:     map[string]string{consts.PodLabelServiceEgressGateway: egress},
				Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
			},
			Status: status,
		}
	}

	kube := fake.NewSimpleClientset(newPod(true))
	dt, _ := newSeededDiffTracker(t, ctrl, kube, k8s, nrp)

	// First termination event still carries the IPs: it drain-gates the address (Enqueued) and keeps
	// the finalizer for the drain-gate.
	dt.podInformerRemovePod(newPod(true))
	assert.True(t, dt.HasPendingPodDeletion("default", "pod-a", "uid-a"),
		"the with-IP delete must record a pending drain")

	// Second event arrives after the kubelet cleared the IPs. The finalizer must NOT be stripped.
	dt.podInformerRemovePod(newPod(false))

	got, err := kube.CoreV1().Pods("default").Get(context.Background(), "pod-a", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"a no-IP delete must not strip the finalizer while a drain is still pending")
	assert.True(t, dt.HasPendingPodDeletion("default", "pod-a", "uid-a"),
		"the pending drain must remain until NRP confirms the address is gone")
}

// TestPodInformerRemovePod_NoIPDeleteStripsUntrackedPod checks the complementary case: a no-IP egress
// pod the engine never registered (no pending drain) still has its finalizer removed directly.
func TestPodInformerRemovePod_NoIPDeleteStripsUntrackedPod(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-b", Namespace: "default", UID: types.UID("uid-b"),
			Labels:     map[string]string{consts.PodLabelServiceEgressGateway: "egress-svc"},
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
		Status: v1.PodStatus{Phase: v1.PodFailed},
	}
	kube := fake.NewSimpleClientset(pod)
	dt := newProviderDiffTracker(t, ctrl, kube)

	dt.podInformerRemovePod(pod)

	got, err := kube.CoreV1().Pods("default").Get(context.Background(), "pod-b", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"an untracked no-IP pod with no pending drain must have its finalizer removed to avoid stranding")
}

func TestPodInformerRemovePod_NoIPDeleteWaitsForUnbackedNRPAddress(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		egress      = "egress-svc"
		oldAddress  = "10.244.0.1"
		liveAddress = "10.244.0.2"
		location    = "10.0.0.1"
	)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "deleted", Namespace: "default", UID: types.UID("deleted-uid"),
			Labels:     map[string]string{consts.PodLabelServiceEgressGateway: egress},
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
		Status: v1.PodStatus{Phase: v1.PodFailed},
	}
	kube := fake.NewSimpleClientset(pod)
	dt := newProviderDiffTracker(t, ctrl, kube)
	dt.pendingServiceOps[egress] = &ServiceOperationState{
		ServiceUID: egress,
		Config:     NewOutboundServiceConfig(egress, nil),
		State:      StateCreated,
	}
	dt.K8sResources.Nodes[location] = Node{
		Pods: map[string]Pod{
			liveAddress: {
				InboundIdentities:      utilsets.NewString(),
				PublicOutboundIdentity: egress,
			},
		},
	}
	dt.NRPResources.NATGateways.Insert(egress)
	dt.NRPResources.Locations[location] = NRPLocation{
		Addresses: map[string]NRPAddress{
			oldAddress:  {Services: utilsets.NewString(egress)},
			liveAddress: {Services: utilsets.NewString(egress)},
		},
	}

	dt.podInformerRemovePod(pod)

	got, err := kube.CoreV1().Pods("default").Get(context.Background(), "deleted", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer)
	entry := dt.pendingPodDeletions["default/deleted"]
	if assert.NotNil(t, entry) {
		assert.True(t, entry.VerifyServiceDrain)
		assert.False(t, entry.IsLastPod)
	}

	dt.CheckPendingPodDeletions(context.Background())
	got, err = kube.CoreV1().Pods("default").Get(context.Background(), "deleted", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"the no-IP finalizer must remain while an unbacked NRP address can belong to the pod")

	delete(dt.NRPResources.Locations[location].Addresses, oldAddress)
	dt.CheckPendingPodDeletions(context.Background())

	got, err = kube.CoreV1().Pods("default").Get(context.Background(), "deleted", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"once every remaining NRP address is backed by a live pod, the no-IP pod can terminate")
}

func TestPodInformerRemovePod_NoIPLastPodWaitsForServiceDeletion(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const egress = "egress-svc"
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "deleted", Namespace: "default", UID: types.UID("deleted-uid"),
			Labels:     map[string]string{consts.PodLabelServiceEgressGateway: egress},
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
		Status: v1.PodStatus{Phase: v1.PodFailed},
	}
	kube := fake.NewSimpleClientset(pod)
	dt := newProviderDiffTracker(t, ctrl, kube)
	dt.pendingServiceOps[egress] = &ServiceOperationState{
		ServiceUID: egress,
		Config:     NewOutboundServiceConfig(egress, nil),
		State:      StateCreated,
	}
	dt.K8sResources.Nodes["10.0.0.1"] = Node{
		Pods: map[string]Pod{
			"10.244.0.1": {
				InboundIdentities:      utilsets.NewString(),
				PublicOutboundIdentity: egress,
				OutboundPodKey:         "default/deleted",
			},
		},
	}
	dt.outboundIdentityPodRefCount.Store(egress, 1)
	dt.NRPResources.NATGateways.Insert(egress)
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{
		Addresses: map[string]NRPAddress{
			"10.244.0.1": {Services: utilsets.NewString(egress)},
		},
	}

	dt.podInformerRemovePod(pod)

	got, err := kube.CoreV1().Pods("default").Get(context.Background(), "deleted", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer)
	entry := dt.pendingPodDeletions["default/deleted"]
	if assert.NotNil(t, entry) {
		assert.False(t, entry.VerifyServiceDrain,
			"the pod identity should recover its stale live address even though status PodIPs are empty")
		assert.Equal(t, []string{"10.244.0.1"}, entry.Addresses)
		assert.True(t, entry.IsLastPod)
	}
	assert.NotContains(t, dt.K8sResources.Nodes, "10.0.0.1",
		"the deleted pod's stale desired address must be removed before NRP verification")
	assert.Equal(t, StateDeletionPending, dt.pendingServiceOps[egress].State)

	assert.NoError(t, dt.RemoveLastPodFinalizers(context.Background(), egress))
	got, err = kube.CoreV1().Pods("default").Get(context.Background(), "deleted", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer)
}

// A local K8s-state miss is not proof that an egress pod is untracked: NRP can still map its
// address while the engine is between snapshots or recovering state. The terminating pod must keep
// its finalizer and reconstruct drain tracking instead of taking the inline "untracked" release.
func TestPodInformerRemovePod_LocalStateMissingButNRPAddressPresentKeepsFinalizer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		egress = "egress-svc"
		hostIP = "10.0.0.1"
		podIP  = "10.244.0.1"
	)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-a", Namespace: "default", UID: types.UID("uid-a"),
			Labels:     map[string]string{consts.PodLabelServiceEgressGateway: egress},
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
		Status: v1.PodStatus{
			HostIP: hostIP,
			PodIP:  podIP,
			PodIPs: []v1.PodIP{{IP: podIP}},
			Phase:  v1.PodRunning,
		},
	}
	kube := fake.NewSimpleClientset(pod)
	dt := newProviderDiffTracker(t, ctrl, kube)
	dt.NRPResources.NATGateways.Insert(egress)
	dt.NRPResources.Locations[hostIP] = NRPLocation{
		Addresses: map[string]NRPAddress{
			podIP: {Services: utilsets.NewString(egress)},
		},
	}
	dt.pendingServiceOps[egress] = &ServiceOperationState{
		ServiceUID: egress,
		Config:     NewOutboundServiceConfig(egress, nil),
		State:      StateCreated,
	}

	// Deliberately do not seed K8sResources.Nodes: this models the production failure where the
	// informer delete raced a transient local-state gap even though NRP still owned the address.
	dt.podInformerRemovePod(pod)
	dt.podInformerRemovePod(pod.DeepCopy()) // duplicate terminating update
	noIPs := pod.DeepCopy()
	noIPs.Status.HostIP = ""
	noIPs.Status.PodIP = ""
	noIPs.Status.PodIPs = nil
	dt.podInformerRemovePod(noIPs) // later terminating update after kubelet clears addresses

	got, err := kube.CoreV1().Pods("default").Get(context.Background(), "pod-a", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"NRP still maps the pod address, so a local state miss must not authorize finalizer removal")
	pending, ok := dt.pendingPodDeletions["default/pod-a"]
	if assert.True(t, ok, "the local-state miss must reconstruct pending drain tracking") {
		assert.Equal(t, "uid-a", pending.UID)
		assert.Equal(t, []string{podIP}, pending.Addresses)
		assert.True(t, pending.IsLastPod,
			"with no other live or buffered egress pods, cleanup must wait for NAT Gateway deletion")
	}

	// Simulate the outbound delete worker's post-NAT/PIP finalizer sweep.
	assert.NoError(t, dt.RemoveLastPodFinalizers(context.Background(), egress))
	got, err = kube.CoreV1().Pods("default").Get(context.Background(), "pod-a", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"the finalizer should release only when the outbound delete worker completes cleanup")
}

// TestReconcileEgressPodUpdate_LiveReRegistrationKeepsFinalizerAndReAdds drives the real update
// executor end-to-end on a live engine: a dual-stack pod that gains a secondary family
// ([v4] -> [v4,v6]) is a live re-registration. The executor must drain the old set without a
// finalizer record and re-add the full set, leaving the pod finalized and both addresses tracked.
func TestReconcileEgressPodUpdate_LiveReRegistrationKeepsFinalizerAndReAdds(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		egress = "egress-svc"
		hostIP = "10.0.0.1"
		v6Node = "fd00::a"
		v4     = "10.244.0.1"
		v6     = "fd00::1"
		other  = "10.244.0.9"
	)

	// One live pod (v4) plus a second pod so the service is not torn down mid-update.
	k8s := K8sState{
		Services: utilsets.NewString(),
		Egresses: utilsets.NewString(egress),
		Nodes: map[string]Node{
			hostIP: {Pods: map[string]Pod{
				v4:    {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: egress},
				other: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: egress},
			}},
		},
	}
	nrp := NRPState{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(egress),
		Locations:     make(map[string]NRPLocation),
	}

	pod := func(ips ...string) *v1.Pod {
		podIPs := make([]v1.PodIP, 0, len(ips))
		for _, ip := range ips {
			podIPs = append(podIPs, v1.PodIP{IP: ip})
		}
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "pod-a",
				Namespace:  "default",
				UID:        types.UID("uid-a"),
				Labels:     map[string]string{consts.PodLabelServiceEgressGateway: egress},
				Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
			},
			Status: v1.PodStatus{HostIP: hostIP, HostIPs: []v1.HostIP{{IP: hostIP}, {IP: v6Node}}, PodIP: ips[0], PodIPs: podIPs, Phase: v1.PodRunning},
		}
	}

	oldPod, newPod := pod(v4), pod(v4, v6)
	kube := fake.NewSimpleClientset(newPod)
	dt, _ := newSeededDiffTracker(t, ctrl, kube, k8s, nrp)

	dt.reconcileEgressPodUpdate(oldPod, newPod)
	dt.CheckPendingPodDeletions(context.Background())

	got, err := kube.CoreV1().Pods("default").Get(context.Background(), "pod-a", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"a live re-registration must leave the pod finalized")
	assert.True(t, dt.IsServiceTracked(egress), "the egress service must remain tracked")

	// Both IP families must be registered under their SAME-FAMILY node location: the dual-stack gain
	// added v6 under the node's IPv6 IP and kept the shared v4 under the IPv4 node IP.
	v4Pods := dt.K8sResources.Nodes[hostIP].Pods
	v6Pods := dt.K8sResources.Nodes[v6Node].Pods
	if assert.Contains(t, v4Pods, v4, "the primary family must remain registered under the IPv4 node location") {
		assert.Equal(t, egress, v4Pods[v4].PublicOutboundIdentity)
	}
	if assert.Contains(t, v6Pods, v6, "the gained secondary family must be registered under the IPv6 node location") {
		assert.Equal(t, egress, v6Pods[v6].PublicOutboundIdentity)
	}
	assert.NotContains(t, v4Pods, v6, "the IPv6 address must not be filed under the IPv4 node location")
}

func TestReconcileEgressPodUpdate_SolePodIPReplacementKeepsNATGateway(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	const (
		egress = "egress-svc"
		hostIP = "10.0.0.1"
		oldIP  = "10.244.0.1"
		newIP  = "10.244.0.2"
		podKey = "default/pod-a"
		podUID = "uid-a"
	)
	k8s := K8sState{
		Services: utilsets.NewString(),
		Egresses: utilsets.NewString(egress),
		Nodes: map[string]Node{
			hostIP: {Pods: map[string]Pod{
				oldIP: {
					InboundIdentities:      utilsets.NewString(),
					PublicOutboundIdentity: egress,
					OutboundPodKey:         podKey,
					OutboundPodUID:         podUID,
				},
			}},
		},
	}
	nrp := NRPState{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(egress),
		Locations:     make(map[string]NRPLocation),
	}
	makePod := func(ip string) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "pod-a",
				Namespace:  "default",
				UID:        types.UID(podUID),
				Labels:     map[string]string{consts.PodLabelServiceEgressGateway: egress},
				Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
			},
			Status: v1.PodStatus{
				HostIP: hostIP,
				PodIP:  ip,
				PodIPs: []v1.PodIP{{IP: ip}},
				Phase:  v1.PodRunning,
			},
		}
	}

	oldPod, newPod := makePod(oldIP), makePod(newIP)
	kube := fake.NewSimpleClientset(newPod)
	dt, _ := newSeededDiffTracker(t, ctrl, kube, k8s, nrp)

	dt.reconcileEgressPodUpdate(oldPod, newPod)

	assert.NotContains(t, dt.pendingServiceDeletions, egress,
		"a same-service IP replacement must not schedule NAT Gateway deletion in the drain/add gap")
	if op, ok := dt.pendingServiceOps[egress]; ok {
		assert.NotEqual(t, StateDeletionPending, op.State)
		assert.NotEqual(t, StateDeletionInProgress, op.State)
	}
	assert.NotContains(t, dt.K8sResources.Nodes[hostIP].Pods, oldIP)
	replacement := dt.K8sResources.Nodes[hostIP].Pods[newIP]
	assert.Equal(t, egress, replacement.PublicOutboundIdentity)
	assert.Equal(t, podKey, replacement.OutboundPodKey)
	assert.Equal(t, podUID, replacement.OutboundPodUID)

	got, err := kube.CoreV1().Pods("default").Get(context.Background(), "pod-a", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer)
}

// TestEgressPodUpdateActions verifies the pure UPDATE decision function directly: for each pod
// transition it must report whether the pod has to be removed from its old gateway and/or (re-)added
// to its current one. Driving egressPodUpdateActions itself (rather than a mock informer) keeps this
// a genuine guard - the routing that consumes these decisions is covered by the real-Cloud tests
// (TestPodInformerDrainForReplace_*, TestReconcileEgressPodUpdate_*).
func TestEgressPodUpdateActions(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name       string
		oldPod     *v1.Pod
		newPod     *v1.Pod
		wantRemove bool
		wantAdd    bool
	}{
		{
			name:       "Label change from A to B with IPs",
			oldPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:     newTestPod("default", "test", "egress-b", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			wantRemove: true,
			wantAdd:    true,
		},
		{
			name:    "Pod gets IPs AND label changes (never had IPs in A)",
			oldPod:  newTestPod("default", "test", "egress-a", "", "", v1.PodPending, nil),
			newPod:  newTestPod("default", "test", "egress-b", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			wantAdd: true,
		},
		{
			name:    "Pod just gets IPs (no label change)",
			oldPod:  newTestPod("default", "test", "egress-a", "", "", v1.PodPending, nil),
			newPod:  newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			wantAdd: true,
		},
		{
			name:       "IP change (pod moved to different node)",
			oldPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:     newTestPod("default", "test", "egress-a", "10.0.0.2", "10.0.1.2", v1.PodRunning, nil),
			wantRemove: true,
			wantAdd:    true,
		},
		{
			name:       "Dual-stack secondary IP changes while primary PodIP is unchanged",
			oldPod:     withPodIPs(newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil), "10.0.1.1", "fd00::old"),
			newPod:     withPodIPs(newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil), "10.0.1.1", "fd00::new"),
			wantRemove: true,
			wantAdd:    true,
		},
		{
			name:       "Dual-stack secondary IP added after the primary",
			oldPod:     withPodIPs(newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil), "10.0.1.1"),
			newPod:     withPodIPs(newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil), "10.0.1.1", "fd00::1"),
			wantRemove: true,
			wantAdd:    true,
		},
		{
			name:       "Dual-stack pod downgraded to single stack (loses its secondary family)",
			oldPod:     withPodIPs(newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil), "10.0.1.1", "fd00::1"),
			newPod:     withPodIPs(newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil), "10.0.1.1"),
			wantRemove: true,
			wantAdd:    true,
		},
		{
			name:       "Pod loses IPs",
			oldPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:     newTestPod("default", "test", "egress-a", "", "", v1.PodRunning, nil),
			wantRemove: true,
		},
		{
			name:       "Label removed (pod no longer egress)",
			oldPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:     newTestPod("default", "test", "", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			wantRemove: true,
		},
		{
			name:       "Pod transitions to Failed phase",
			oldPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodFailed, nil),
			wantRemove: true,
		},
		{
			name:       "Pod transitions to Succeeded phase",
			oldPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodSucceeded, nil),
			wantRemove: true,
		},
		{
			name:       "Pod gets DeletionTimestamp",
			oldPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, &now),
			wantRemove: true,
		},
		{
			name:       "Pod gets DeletionTimestamp AND label changes",
			oldPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:     newTestPod("default", "test", "egress-b", "10.0.0.1", "10.0.1.1", v1.PodRunning, &now),
			wantRemove: true,
		},
		{
			name:   "No relevant changes (same state)",
			oldPod: newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod: newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
		},
		{
			name:   "Pod in Pending without IPs stays in Pending without IPs",
			oldPod: newTestPod("default", "test", "egress-a", "", "", v1.PodPending, nil),
			newPod: newTestPod("default", "test", "egress-a", "", "", v1.PodPending, nil),
		},
		{
			name:       "IP change on terminating pod",
			oldPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:     newTestPod("default", "test", "egress-a", "10.0.0.2", "10.0.1.2", v1.PodRunning, &now),
			wantRemove: true,
		},
		{
			name:    "Pod recovers to Running from an invalid phase with unchanged IPs",
			oldPod:  newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodFailed, nil),
			newPod:  newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			wantAdd: true,
		},
		{
			name:       "Egress pod goes Unknown while keeping its IPs",
			oldPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodUnknown, nil),
			wantRemove: true,
		},
		{
			name:    "Egress pod recovers from Unknown to Running with the same IPs",
			oldPod:  newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodUnknown, nil),
			newPod:  newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			wantAdd: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			needsRemove, needsAdd, _ := egressPodUpdateActions(tt.oldPod, tt.newPod)
			assert.Equal(t, tt.wantRemove, needsRemove, "needsRemove")
			assert.Equal(t, tt.wantAdd, needsAdd, "needsAdd")
		})
	}
}

// TestEgressPodUpdateActions_NodeLocationChange verifies re-registration is triggered when a
// secondary-family node IP (Status.HostIPs) changes or appears, even though PodIPs and the primary
// HostIP are unchanged - otherwise the stale IPv6 location leaks or the IPv6 address never registers.
func TestEgressPodUpdateActions_NodeLocationChange(t *testing.T) {
	const v4, v6, v4Node, v6NodeOld, v6NodeNew = "10.244.0.1", "fd00::1", "10.0.0.1", "fd00::a", "fd00::b"
	dsPod := func(hostV6 string) *v1.Pod {
		hostIPs := []v1.HostIP{{IP: v4Node}}
		if hostV6 != "" {
			hostIPs = append(hostIPs, v1.HostIP{IP: hostV6})
		}
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p", Namespace: "default",
				Labels: map[string]string{consts.PodLabelServiceEgressGateway: "egress-a"},
			},
			Status: v1.PodStatus{HostIP: v4Node, HostIPs: hostIPs, PodIP: v4, PodIPs: []v1.PodIP{{IP: v4}, {IP: v6}}, Phase: v1.PodRunning},
		}
	}

	t.Run("secondary-family node IP change re-registers", func(t *testing.T) {
		needsRemove, needsAdd, _ := egressPodUpdateActions(dsPod(v6NodeOld), dsPod(v6NodeNew))
		assert.True(t, needsRemove)
		assert.True(t, needsAdd)
	})
	t.Run("secondary-family node IP appearing re-registers", func(t *testing.T) {
		needsRemove, needsAdd, _ := egressPodUpdateActions(dsPod(""), dsPod(v6NodeNew))
		assert.True(t, needsRemove)
		assert.True(t, needsAdd)
	})
}
func TestPodInformerDeleteFunc(t *testing.T) {
	tests := []struct {
		name            string
		obj             interface{}
		expectDeletePod bool
		shouldError     bool
	}{
		{
			name:            "Direct pod object",
			obj:             newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			expectDeletePod: true,
		},
		{
			name: "Tombstone with valid pod",
			obj: cache.DeletedFinalStateUnknown{
				Key: "default/test",
				Obj: newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			},
			expectDeletePod: true,
		},
		{
			name: "Tombstone with invalid object type",
			obj: cache.DeletedFinalStateUnknown{
				Key: "default/test",
				Obj: "not-a-pod",
			},
			expectDeletePod: false,
			shouldError:     true,
		},
		{
			name:            "Invalid object type (not pod or tombstone)",
			obj:             "invalid-type",
			expectDeletePod: false,
			shouldError:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Drive the production decode the informer's DeleteFunc uses. Re-implementing the
			// type switch here would leave the real one untested: dropping its
			// DeletedFinalStateUnknown arm silently ignores the deletion of every egress pod
			// whose delete event the watch missed, stranding their cleanup finalizers.
			pod, ok := podFromDeleteObj(tt.obj)
			assert.Equal(t, !tt.shouldError, ok, "podFromDeleteObj decode result mismatch")
			if !ok {
				assert.Nil(t, pod, "a rejected object must not yield a pod")
				return
			}
			assert.NotNil(t, pod)

			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			// A decoded egress pod is routed to podInformerRemovePod; on an untracked (empty) engine
			// that strips its cleanup finalizer directly (nothing to drain).
			pod = pod.DeepCopy()
			pod.Finalizers = []string{ServiceGatewayPodCleanupFinalizer}
			kube := fake.NewSimpleClientset(pod)
			dt := newProviderDiffTracker(t, ctrl, kube)

			dt.podInformerRemovePod(pod)

			if tt.expectDeletePod {
				got, err := kube.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
				assert.NoError(t, err)
				assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
					"a decoded egress pod must reach podInformerRemovePod, which strips the finalizer on an untracked pod")
			}
		})
	}
}

// TestPodInformerRemovePod_BufferedPodFinalizerRemovedByCaller verifies that a pod deleted while it
// is still buffered for an in-flight (never-created) egress service does not strand its cleanup
// finalizer. DeletePod returns early with Enqueued=false (nothing to drain from NRP), so
// podInformerRemovePod's !result.IsLastPod && !result.Enqueued branch removes the finalizer directly.
func TestPodInformerRemovePod_BufferedPodFinalizerRemovedByCaller(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "egress-buffered",
			Namespace:  "default",
			Labels:     map[string]string{consts.PodLabelServiceEgressGateway: "egress-svc"},
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
		Status: v1.PodStatus{HostIP: "10.0.0.1", PodIP: "10.244.0.7"},
	}
	kubeClient := fake.NewSimpleClientset(pod)

	dt := newProviderDiffTracker(t, ctrl, kubeClient)

	// Buffer the pod for an in-flight egress service (the harness runs no async workers, so the
	// service stays in StateNotStarted with the pod buffered in pendingPods).
	dt.AddPod("egress-svc", "default/egress-buffered", "10.0.0.1", "10.244.0.7")

	dt.podInformerRemovePod(pod)

	got, err := kubeClient.CoreV1().Pods("default").Get(context.Background(), "egress-buffered", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"a pod deleted while buffered must have its finalizer removed by the caller, not stranded in Terminating")
}

// TestPodInformerAddPod_IgnoresReservedEgressLabel pins that a pod cannot make this controller
// manage the cluster's default outbound NAT Gateway.
//
// The egress label is user-controlled and becomes the Azure resource name verbatim: the NAT Gateway
// is named after it and its Public IP is "<label>-pip". Labelling a pod with the RP-owned default
// gateway's name therefore aims the whole egress lifecycle at it - registering the first pod PUTs
// that gateway and its Public IP, and removing the last pod DELETEs them, taking out the cluster's
// default egress and reallocating its address. Nothing else prevents this: the default gateway is
// deliberately excluded from NRP state at startup, so the pod-add path always concludes it must be
// created, and the pod informer watches every namespace filtered only by this label.
func TestPodInformerAddPod_IgnoresReservedEgressLabel(t *testing.T) {
	for _, label := range []string{"default-natgw", "Default-NatGW", "DEFAULT-NATGW"} {
		t.Run(label, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "evil-pod",
					Namespace: "attacker-ns",
					UID:       types.UID("pod-uid"),
					Labels:    map[string]string{consts.PodLabelServiceEgressGateway: label},
				},
				Status: v1.PodStatus{
					Phase:  v1.PodRunning,
					HostIP: "10.0.0.1",
					PodIP:  "10.244.0.9",
					PodIPs: []v1.PodIP{{IP: "10.244.0.9"}},
				},
				Spec: v1.PodSpec{NodeName: "node-1"},
			}

			dt := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(pod))
			rec := record.NewFakeRecorder(10)
			dt.eventRecorder = rec

			dt.podInformerAddPod(pod)

			assert.False(t, dt.IsServiceTracked(DefaultOutboundNATGatewayName),
				"a pod must never make the controller manage the default outbound NAT Gateway")
			dt.mu.Lock()
			opCount := len(dt.pendingServiceOps)
			dt.mu.Unlock()
			assert.Zero(t, opCount, "no Azure operation may be scheduled for a reserved egress identity")

			select {
			case ev := <-rec.Events:
				assert.Contains(t, ev, "ServiceGatewayReservedEgressLabel",
					"the user must be told the label is reserved, not merely malformed")
			default:
				t.Fatal("expected a ServiceGatewayReservedEgressLabel warning Event")
			}
		})
	}
}

// TestPodInformerAddPod_AcceptsOrdinaryEgressLabel is the control: the reserved-name guard must not
// reject ordinary egress identities.
func TestPodInformerAddPod_AcceptsOrdinaryEgressLabel(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-pod", Namespace: "team-ns", UID: types.UID("pod-uid"),
			Labels: map[string]string{consts.PodLabelServiceEgressGateway: "team-egress"},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning, HostIP: "10.0.0.1", PodIP: "10.244.0.9",
			PodIPs: []v1.PodIP{{IP: "10.244.0.9"}},
		},
		Spec: v1.PodSpec{NodeName: "node-1"},
	}

	dt := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(pod))
	dt.eventRecorder = record.NewFakeRecorder(10)

	dt.podInformerAddPod(pod)

	assert.True(t, dt.IsServiceTracked("team-egress"),
		"an ordinary egress identity must still be registered")
}

// TestPodInformerRemovePod_CountsForgottenFinalizerRemoval pins that a failed direct finalizer
// removal is observable.
//
// This removal path is not retried: the engine has already proven there is nothing to drain, so no
// pending record exists to retry from, and the informer only re-drives a pod whose state changes -
// an informer resync delivers an unchanged object and is skipped by egressPodUpdateActions. The pod
// therefore stays Terminating until the CCM restarts, blocking node drain and namespace deletion,
// so the condition must at least be countable.
func TestPodInformerRemovePod_CountsForgottenFinalizerRemoval(t *testing.T) {
	RegisterMetrics()
	before, err := testutil.GetCounterMetricValue(podFinalizerRemoveFailedTotal)
	assert.NoError(t, err)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gone-pod", Namespace: "ns", UID: types.UID("pod-uid"),
			Labels:     map[string]string{consts.PodLabelServiceEgressGateway: "team-egress"},
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
		// No HostIP/PodIP: takes the direct, non-drain-gated removal path.
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}

	kube := fake.NewSimpleClientset(pod)
	kube.PrependReactor("update", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(fmt.Errorf("transient apiserver failure"))
	})

	dt := newProviderDiffTracker(t, ctrl, kube)
	dt.eventRecorder = record.NewFakeRecorder(10)

	dt.podInformerRemovePod(pod)

	after, err := testutil.GetCounterMetricValue(podFinalizerRemoveFailedTotal)
	assert.NoError(t, err)
	assert.Equal(t, float64(1), after-before,
		"a forgotten finalizer removal must be counted so the resulting Terminating pod is detectable")
}

// TestPodInformerRemovePod_ReleasesFinalizerWhenLabelValueEmptied pins that an unlabelled pod
// carrying our cleanup finalizer is still finished off. The informer selects on the label key alone,
// so a pod whose value was emptied keeps matching and is still delivered here. Returning early on
// the missing label leaves the finalizer attached with nothing able to remove it, blocking node
// drain and namespace deletion.
func TestPodInformerRemovePod_ReleasesFinalizerWhenLabelValueEmptied(t *testing.T) {
	newEgressPod := func(labelValue string) *v1.Pod {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "egress-pod", Namespace: "default", UID: types.UID("pod-uid-1"),
				Labels:     map[string]string{consts.PodLabelServiceEgressGateway: labelValue},
				Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
			},
			Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.244.0.5", HostIP: "10.0.0.1"},
		}
		return pod
	}

	// The label value was emptied, so this is all that is left.
	emptied := newEgressPod("")
	kube := fake.NewSimpleClientset(emptied)
	dt := newTestDiffTracker()
	dt.kubeClient = kube

	dt.podInformerRemovePod(emptied)

	got, err := kube.CoreV1().Pods("default").Get(context.Background(), "egress-pod", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.False(t, hasFinalizer(got.Finalizers, ServiceGatewayPodCleanupFinalizer),
		"a pod carrying our cleanup finalizer must be released even once its egress label value is gone")

	// Control 1: an unlabelled pod that is NOT ours must be left completely alone.
	foreign := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foreign-pod", Namespace: "default", UID: types.UID("pod-uid-2"),
			Labels:     map[string]string{consts.PodLabelServiceEgressGateway: ""},
			Finalizers: []string{"example.com/other-controller"},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.244.0.6", HostIP: "10.0.0.1"},
	}
	foreignKube := fake.NewSimpleClientset(foreign)
	foreignDT := newTestDiffTracker()
	foreignDT.kubeClient = foreignKube

	foreignDT.podInformerRemovePod(foreign)

	gotForeign, err := foreignKube.CoreV1().Pods("default").Get(context.Background(), "foreign-pod", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, []string{"example.com/other-controller"}, gotForeign.Finalizers,
		"control: a pod we do not own must not be touched")

	// Control 2: a labelled pod whose address is still in NRP keeps its finalizer until it drains.
	labelled := newEgressPod("team-egress")
	labelledKube := fake.NewSimpleClientset(labelled)
	labelledDT := newTestDiffTracker()
	labelledDT.kubeClient = labelledKube
	labelledDT.NRPResources.NATGateways.Insert("team-egress")
	labelledDT.NRPResources.Locations["10.0.0.1"] = NRPLocation{
		Addresses: map[string]NRPAddress{"10.244.0.5": {Services: utilsets.NewString("team-egress")}},
	}
	labelledDT.pendingServiceOps["team-egress"] = &ServiceOperationState{
		ServiceUID: "team-egress",
		Config:     NewOutboundServiceConfig("team-egress", nil),
		State:      StateCreated,
	}

	labelledDT.podInformerRemovePod(labelled)

	gotLabelled, err := labelledKube.CoreV1().Pods("default").Get(context.Background(), "egress-pod", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.True(t, hasFinalizer(gotLabelled.Finalizers, ServiceGatewayPodCleanupFinalizer),
		"control: a labelled pod keeps its finalizer until its addresses drain from NRP")
}

// TestPodInformerRemovePod_QueuesRetryWhenFinalizerRemovalFails pins that a transient apiserver
// error on a direct finalizer removal is retried rather than lost.
//
// These paths run only once the engine has proved there is nothing to drain, so no pending record
// exists and the informer will not re-drive the pod: a resync delivers an unchanged object and is
// skipped. Without a queued record the pod stays Terminating until the CCM restarts, blocking node
// drain and namespace deletion. The successful removal is the control: it must queue nothing.
func TestPodInformerRemovePod_QueuesRetryWhenFinalizerRemovalFails(t *testing.T) {
	newPodObj := func() *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "egress-pod", Namespace: "default", UID: types.UID("pod-uid"),
				Labels:            map[string]string{consts.PodLabelServiceEgressGateway: "team-egress"},
				Finalizers:        []string{ServiceGatewayPodCleanupFinalizer},
				DeletionTimestamp: ptr.To(metav1.Now()),
			},
			// No IPs, so the engine takes the "nothing to drain" path.
			Status: v1.PodStatus{Phase: v1.PodRunning},
		}
	}

	run := func(t *testing.T, removalFails bool) *DiffTracker {
		t.Helper()
		pod := newPodObj()
		kube := fake.NewSimpleClientset(pod)
		if removalFails {
			kube.PrependReactor("update", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("apiserver unavailable")
			})
		}
		dt := newTestDiffTracker()
		dt.kubeClient = kube
		dt.podInformerRemovePod(pod)
		return dt
	}

	failed := run(t, true)
	failed.mu.Lock()
	pending, queued := failed.pendingPodDeletions["default/egress-pod"]
	failed.mu.Unlock()
	if assert.True(t, queued, "a failed finalizer removal must be queued for retry, not dropped") {
		assert.Equal(t, "pod-uid", pending.UID, "the record must pin the pod UID so a replacement is not stripped")
		assert.False(t, pending.IsLastPod, "a finalizer-only retry must not wait on a NAT Gateway deletion")
	}

	ok := run(t, false)
	ok.mu.Lock()
	_, queuedOnSuccess := ok.pendingPodDeletions["default/egress-pod"]
	ok.mu.Unlock()
	assert.False(t, queuedOnSuccess, "control: a successful removal queues nothing")
}
