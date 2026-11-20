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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestPodInformerSameRawIdentityHasIndependentDrain(t *testing.T) {
	const uid, hostIP, podIP = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "10.0.0.1", "10.244.0.5"
	inboundName, outboundName := inboundServiceName(uid), outboundServiceName(uid)
	for _, registered := range []bool{false, true} {
		pod := newTestPod("ns", "shared", uid, hostIP, podIP, v1.PodRunning, nil)
		pod.UID = "pod-uid"
		kube := fake.NewSimpleClientset(pod)
		dt := newTestDiffTracker()
		dt.kubeClient = kube
		dt.K8sResources.Services.Insert(inboundName)
		dt.NRPResources.LoadBalancers.Insert(inboundName)
		dt.NRPResources.Locations[hostIP] = NRPLocation{
			Addresses: map[string]NRPAddress{podIP: {Services: utilsets.NewString(inboundName)}},
		}
		dt.podInformerAddPod(pod)
		live, err := kube.CoreV1().Pods("ns").Get(context.Background(), pod.Name, metav1.GetOptions{})
		if !assert.NoError(t, err) {
			return
		}
		assert.True(t, hasPodFinalizer(live))
		assert.Contains(t, dt.pendingServiceOps, outboundName)
		assert.NotContains(t, dt.pendingServiceOps, inboundName)
		if registered {
			dt.NRPResources.NATGateways.Insert(outboundName)
			dt.pendingServiceOps[outboundName].State = StateCreationInProgress
			dt.OnServiceCreationComplete(outboundName, true, nil)
			dt.NRPResources.Locations[hostIP].Addresses[podIP].Services.Insert(outboundName)
		}
		now := metav1.Now()
		live.DeletionTimestamp = &now
		live, err = kube.CoreV1().Pods("ns").Update(context.Background(), live, metav1.UpdateOptions{})
		if !assert.NoError(t, err) {
			return
		}
		dt.podInformerRemovePod(live)
		live, err = kube.CoreV1().Pods("ns").Get(context.Background(), pod.Name, metav1.GetOptions{})
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, registered, hasPodFinalizer(live), "only outbound state may hold the egress finalizer")
		assert.True(t, dt.NRPResources.LoadBalancers.Has(inboundName))
		assert.True(t, dt.NRPResources.Locations[hostIP].Addresses[podIP].Services.Has(inboundName))
		assert.NotContains(t, dt.pendingServiceDeletions, inboundName)
		if registered {
			dt.NRPResources.Locations[hostIP].Addresses[podIP].Services.Delete(outboundName)
			dt.NRPResources.NATGateways.Delete(outboundName)
			assert.NoError(t, dt.RemoveLastPodFinalizers(context.Background(), outboundName))
			live, err = kube.CoreV1().Pods("ns").Get(context.Background(), pod.Name, metav1.GetOptions{})
			assert.NoError(t, err)
			assert.False(t, hasPodFinalizer(live), "remaining inbound references must not block outbound cleanup")
		}
	}
}

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
			buffered := dt.pendingPods[outboundServiceName(tt.expectedEgress)]
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
		K8sState{Services: utilsets.NewString(), Egresses: utilsets.NewString(outboundServiceName(egress)), Nodes: map[string]Node{}},
		NRPState{LoadBalancers: utilsets.NewString(), NATGateways: utilsets.NewString(outboundServiceName(egress)), Locations: map[string]NRPLocation{}})

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
	assert.True(t, dt.IsServiceTracked(outboundServiceName("egress-svc")),
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
	for _, tc := range []struct {
		label    string
		reserved bool
	}{
		{label: "../hijacked-nat"},
		{label: "trailing-dot."},
		{label: "trailing-hyphen-"},
		{label: strings.Repeat("a", maxEgressIdentityLength+1)},
		{label: "trailing-dot.", reserved: true},
	} {
		t.Run(fmt.Sprintf("%s/reserved=%t", tc.label, tc.reserved), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "evil-egress",
					Namespace: "default",
					Labels:    map[string]string{consts.PodLabelServiceEgressGateway: tc.label},
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, HostIP: "10.0.0.1", PodIP: "10.244.0.1"},
			}
			kube := fake.NewSimpleClientset(pod)

			dt := newProviderDiffTracker(t, ctrl, kube)
			if tc.reserved {
				dt.NRPResources.ReservedOutboundServices = utilsets.NewString(outboundServiceName(tc.label))
			}
			rec := record.NewFakeRecorder(10)
			dt.eventRecorder = rec
			beforeActions := len(kube.Actions())

			dt.podInformerAddPod(pod)

			assert.False(t, dt.IsServiceTracked(outboundServiceName(tc.label)),
				"a pod with an invalid egress label must not be registered with the engine")
			assert.Len(t, kube.Actions(), beforeActions, "invalid labels must be rejected before finalizer writes")

			select {
			case ev := <-rec.Events:
				assert.Contains(t, ev, "ServiceGatewayInvalidEgressLabel",
					"a warning Event must be emitted for an invalid egress gateway label")
			default:
				t.Fatal("expected a ServiceGatewayInvalidEgressLabel warning Event")
			}
			assert.Empty(t, rec.Events)
		})
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

	assert.False(t, dt.IsServiceTracked(outboundServiceName(egress)),
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

	assert.False(t, dt.IsServiceTracked(outboundServiceName("egress-svc")),
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
					tt.pod.Status.PodIP: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: outboundServiceName(egress)},
				}}
				nrp.NATGateways.Insert(outboundServiceName(egress))
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
			Egresses: utilsets.NewString(outboundServiceName(egress)),
			Nodes: map[string]Node{
				hostIP: {Pods: map[string]Pod{
					addrA: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: outboundServiceName(egress)},
					addrB: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: outboundServiceName(egress)},
				}},
			},
		}
	}
	// addrA is not registered in NRP (already drained), so a non-last pending record would strip at once.
	drainedNRP := func() NRPState {
		return NRPState{
			LoadBalancers: utilsets.NewString(),
			NATGateways:   utilsets.NewString(outboundServiceName(egress)),
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

	t.Run("ordinary deletion releases after the drain", func(t *testing.T) {
		deleting := podA(addrA)
		deleting.DeletionTimestamp = ptr.To(metav1.Now())
		kube := fake.NewSimpleClientset(deleting)
		dt := seededDT(t, ctrl, kube, seedState(), drainedNRP())

		dt.podInformerRemovePod(deleting)
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
		Egresses: utilsets.NewString(outboundServiceName(egress)),
		Nodes: map[string]Node{
			hostIP: {Pods: map[string]Pod{
				v4: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: outboundServiceName(egress)},
			}},
		},
	}
	nrp := NRPState{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(outboundServiceName(egress)),
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
	res := dt.DeletePod(outboundServiceName(egress), hostIP, []string{v4}, "default", "pod-a", "uid-a")
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
		Egresses: utilsets.NewString(outboundServiceName(egress)),
		Nodes: map[string]Node{
			hostIP: {Pods: map[string]Pod{
				v4: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: outboundServiceName(egress)},
			}},
		},
	}
	nrp := NRPState{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(outboundServiceName(egress)),
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
		rawEgress   = "egress-svc"
		oldAddress  = "10.244.0.1"
		liveAddress = "10.244.0.2"
		location    = "10.0.0.1"
	)
	egress := outboundServiceName(rawEgress)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "deleted", Namespace: "default", UID: types.UID("deleted-uid"),
			Labels:     map[string]string{consts.PodLabelServiceEgressGateway: rawEgress},
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
		Status: v1.PodStatus{Phase: v1.PodFailed},
	}
	kube := fake.NewSimpleClientset(pod)
	dt := newProviderDiffTracker(t, ctrl, kube)
	dt.pendingServiceOps[egress] = &ServiceOperationState{
		ServiceName: egress,
		Config:      NewOutboundServiceConfig(egress, nil),
		State:       StateCreated,
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

	const rawEgress = "egress-svc"
	egress := outboundServiceName(rawEgress)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "deleted", Namespace: "default", UID: types.UID("deleted-uid"),
			Labels:     map[string]string{consts.PodLabelServiceEgressGateway: rawEgress},
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		},
		Status: v1.PodStatus{Phase: v1.PodFailed},
	}
	kube := fake.NewSimpleClientset(pod)
	dt := newProviderDiffTracker(t, ctrl, kube)
	dt.pendingServiceOps[egress] = &ServiceOperationState{
		ServiceName: egress,
		Config:      NewOutboundServiceConfig(egress, nil),
		State:       StateCreated,
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
			Labels:            map[string]string{consts.PodLabelServiceEgressGateway: egress},
			Finalizers:        []string{ServiceGatewayPodCleanupFinalizer},
			DeletionTimestamp: ptr.To(metav1.Now()),
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
	dt.NRPResources.NATGateways.Insert(outboundServiceName(egress))
	dt.NRPResources.Locations[hostIP] = NRPLocation{
		Addresses: map[string]NRPAddress{
			podIP: {Services: utilsets.NewString(outboundServiceName(egress))},
		},
	}
	dt.pendingServiceOps[outboundServiceName(egress)] = &ServiceOperationState{
		ServiceName: outboundServiceName(egress),
		Config:      NewOutboundServiceConfig(outboundServiceName(egress), nil),
		State:       StateCreated,
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
	assert.NoError(t, dt.RemoveLastPodFinalizers(context.Background(), outboundServiceName(egress)))
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
		Egresses: utilsets.NewString(outboundServiceName(egress)),
		Nodes: map[string]Node{
			hostIP: {Pods: map[string]Pod{
				v4:    {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: outboundServiceName(egress)},
				other: {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: outboundServiceName(egress)},
			}},
		},
	}
	nrp := NRPState{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(outboundServiceName(egress)),
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
	assert.True(t, dt.IsServiceTracked(outboundServiceName(egress)), "the egress service must remain tracked")

	// Both IP families must be registered under their SAME-FAMILY node location: the dual-stack gain
	// added v6 under the node's IPv6 IP and kept the shared v4 under the IPv4 node IP.
	v4Pods := dt.K8sResources.Nodes[hostIP].Pods
	v6Pods := dt.K8sResources.Nodes[v6Node].Pods
	if assert.Contains(t, v4Pods, v4, "the primary family must remain registered under the IPv4 node location") {
		assert.Equal(t, outboundServiceName(egress), v4Pods[v4].PublicOutboundIdentity)
	}
	if assert.Contains(t, v6Pods, v6, "the gained secondary family must be registered under the IPv6 node location") {
		assert.Equal(t, outboundServiceName(egress), v6Pods[v6].PublicOutboundIdentity)
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
		Egresses: utilsets.NewString(outboundServiceName(egress)),
		Nodes: map[string]Node{
			hostIP: {Pods: map[string]Pod{
				oldIP: {
					InboundIdentities:      utilsets.NewString(),
					PublicOutboundIdentity: outboundServiceName(egress),
					OutboundPodKey:         podKey,
					OutboundPodUID:         podUID,
				},
			}},
		},
	}
	nrp := NRPState{
		LoadBalancers: utilsets.NewString(),
		NATGateways:   utilsets.NewString(outboundServiceName(egress)),
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

	assert.NotContains(t, dt.pendingServiceDeletions, outboundServiceName(egress),
		"a same-service IP replacement must not schedule NAT Gateway deletion in the drain/add gap")
	if op, ok := dt.pendingServiceOps[outboundServiceName(egress)]; ok {
		assert.NotEqual(t, StateDeletionPending, op.State)
		assert.NotEqual(t, StateDeletionInProgress, op.State)
	}
	assert.NotContains(t, dt.K8sResources.Nodes[hostIP].Pods, oldIP)
	replacement := dt.K8sResources.Nodes[hostIP].Pods[newIP]
	assert.Equal(t, outboundServiceName(egress), replacement.PublicOutboundIdentity)
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
func TestEgressPodUpdateNoIPInvalidationAfterDrain(t *testing.T) {
	const service = "egress-a-natgw"
	for _, tt := range []struct {
		name        string
		drainBefore bool
		phase       v1.PodPhase
		deleting    bool
		owned       bool
		wantHeld    bool
		failRead    bool
	}{
		{name: "deleting after drain", drainBefore: true, deleting: true, owned: true},
		{name: "succeeded after drain", drainBefore: true, phase: v1.PodSucceeded, owned: true},
		{name: "failed after drain", drainBefore: true, phase: v1.PodFailed, owned: true},
		{name: "unknown after drain", drainBefore: true, phase: v1.PodUnknown, owned: true},
		{name: "deletion before drain", deleting: true, owned: true},
		{name: "no owned finalizer", drainBefore: true, deleting: true},
		{name: "live resync", drainBefore: true, owned: true, wantHeld: true},
		{name: "lookup retry before deletion", deleting: true, owned: true, failRead: true},
		{name: "lookup retry before completed drain", drainBefore: true, deleting: true, owned: true, failRead: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			label := "egress-a"
			original := unlabelledFinalizerPod(&label, "uid-p", "10.244.0.7")
			if !tt.owned {
				original.Finalizers = []string{"example.com/keep"}
			}
			kube := fake.NewSimpleClientset(original)
			var unavailable atomic.Bool
			kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				if unavailable.Load() {
					return true, nil, apierrors.NewServiceUnavailable("read unavailable")
				}
				return false, nil, nil
			})
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			dt.NRPResources.NATGateways.Insert(service)
			dt.K8sResources.Egresses.Insert(service)
			dt.K8sResources.Nodes["10.0.0.1"] = Node{Pods: map[string]Pod{
				"10.244.0.7": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: service, OutboundPodKey: "ns/p", OutboundPodUID: "uid-p"},
				"10.244.0.9": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: service, OutboundPodKey: "ns/other", OutboundPodUID: "uid-other"},
			}}
			dt.outboundIdentityPodRefCount.Store(service, 2)
			dt.pendingServiceOps[service] = &ServiceOperationState{
				ServiceName: service, Config: NewOutboundServiceConfig(service, nil), State: StateCreated,
			}
			dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
				"10.244.0.7": {Services: utilsets.NewString(service)},
				"10.244.0.9": {Services: utilsets.NewString(service)},
			}}
			noIPs := original.DeepCopy()
			noIPs.Status.HostIP, noIPs.Status.PodIP, noIPs.Status.PodIPs = "", "", nil
			_, err := kube.CoreV1().Pods("ns").Update(ctx, noIPs, metav1.UpdateOptions{})
			if !assert.NoError(t, err) {
				return
			}
			unavailable.Store(tt.failRead)
			dt.reconcileEgressPodUpdate(original, noIPs)
			unavailable.Store(false)
			if tt.failRead {
				dt.CheckPendingPodDeletions(ctx)
			}
			assert.NotEqual(t, service, dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.7"].PublicOutboundIdentity,
				"remembering an address is not a substitute for starting its drain")
			if tt.drainBefore {
				delete(dt.NRPResources.Locations["10.0.0.1"].Addresses, "10.244.0.7")
				dt.CheckPendingPodDeletions(ctx)
				assert.NotContains(t, dt.pendingPodDeletions, "ns/p")
				got, getErr := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
				assert.NoError(t, getErr)
				assert.Equal(t, tt.owned, hasPodFinalizer(got), "live intent retains protection after the old drain retires")
			}
			current := noIPs.DeepCopy()
			if tt.phase != "" {
				current.Status.Phase = tt.phase
			}
			if tt.deleting {
				current.DeletionTimestamp = ptr.To(metav1.Now())
			}
			_, err = kube.CoreV1().Pods("ns").Update(ctx, current, metav1.UpdateOptions{})
			if !assert.NoError(t, err) {
				return
			}
			dt.reconcileEgressPodUpdate(noIPs, current)
			dt.CheckPendingPodDeletions(ctx)
			if !tt.drainBefore {
				got, getErr := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
				assert.NoError(t, getErr)
				assert.True(t, hasPodFinalizer(got), "invalidation cannot bypass an outstanding address drain")
				delete(dt.NRPResources.Locations["10.0.0.1"].Addresses, "10.244.0.7")
				dt.CheckPendingPodDeletions(ctx)
			}
			got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Contains(t, got.Finalizers, "example.com/keep")
			assert.Equal(t, tt.wantHeld, hasPodFinalizer(got))
			assert.NotContains(t, dt.pendingPodDeletions, "ns/p")
			assert.Equal(t, StateCreated, dt.pendingServiceOps[service].State, "the other Pod still needs this service")
			assert.Empty(t, dt.podFinalizerGuards)
		})
	}
}

func TestPodLookupRetryKeepsCurrentAddresses(t *testing.T) {
	const service = "egress-a-natgw"
	for _, tt := range []struct {
		name     string
		initial  []string
		restored []string
		v6Node   string
	}{
		{name: "same address", initial: []string{"10.244.0.7"}, restored: []string{"10.244.0.7"}},
		{name: "changed address", initial: []string{"10.244.0.7"}, restored: []string{"10.244.0.8"}},
		{name: "secondary family changed", initial: []string{"10.244.0.7", "fd00::7"}, restored: []string{"10.244.0.7", "fd00::8"}},
		{name: "secondary location changed", initial: []string{"10.244.0.7", "fd00::7"}, restored: []string{"10.244.0.7", "fd00::7"}, v6Node: "fd00::2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			label := "egress-a"
			pod := unlabelledFinalizerPod(&label, "uid-p", tt.initial...)
			pod.Status.HostIPs = []v1.HostIP{{IP: "10.0.0.1"}, {IP: "fd00::1"}}
			kube := fake.NewSimpleClientset(pod)
			var unavailable atomic.Bool
			kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				if unavailable.Load() {
					return true, nil, apierrors.NewServiceUnavailable("read unavailable")
				}
				return false, nil, nil
			})
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			dt.NRPResources.NATGateways.Insert(service)
			dt.K8sResources.Egresses.Insert(service)
			dt.pendingServiceOps[service] = &ServiceOperationState{
				ServiceName: service, Config: NewOutboundServiceConfig(service, nil), State: StateCreated,
			}
			dt.podInformerAddPod(pod)
			dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
			noIPs := pod.DeepCopy()
			noIPs.Status.HostIP, noIPs.Status.PodIP, noIPs.Status.PodIPs = "", "", nil
			_, err := kube.CoreV1().Pods("ns").Update(ctx, noIPs, metav1.UpdateOptions{})
			if !assert.NoError(t, err) {
				return
			}
			unavailable.Store(true)
			dt.reconcileEgressPodUpdate(pod, noIPs)
			unavailable.Store(false)
			restored := pod.DeepCopy()
			if tt.v6Node != "" {
				restored.Status.HostIPs[1].IP = tt.v6Node
			}
			restored.Status.PodIP = tt.restored[0]
			restored.Status.PodIPs = nil
			for _, address := range tt.restored {
				restored.Status.PodIPs = append(restored.Status.PodIPs, v1.PodIP{IP: address})
			}
			_, err = kube.CoreV1().Pods("ns").Update(ctx, restored, metav1.UpdateOptions{})
			if !assert.NoError(t, err) {
				return
			}
			gets, prematureCleanup := 0, false
			kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				gets++
				if gets == 2 {
					dt.mu.Lock()
					prematureCleanup = dt.pendingServiceOps[service].State != StateCreated
					dt.mu.Unlock()
				}
				return false, nil, nil
			})
			dt.CheckPendingPodDeletions(ctx)
			assert.GreaterOrEqual(t, gets, 2)
			assert.False(t, prematureCleanup, "same-service replacement must not schedule deletion before re-registration")
			locations := PodNodeLocationsByFamily(restored)
			for _, address := range tt.restored {
				location, found := NodeLocationForAddress(locations, address)
				assert.True(t, found)
				state := dt.K8sResources.Nodes[location].Pods[address]
				assert.Equal(t, "uid-p", state.OutboundPodUID)
				assert.Equal(t, service, state.PublicOutboundIdentity)
			}
			dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
			previousLocations := PodNodeLocationsByFamily(pod)
			for _, address := range tt.initial {
				kept := false
				for _, current := range tt.restored {
					kept = kept || current == address
				}
				if !kept {
					assert.False(t, dt.outboundAddressInAnyNRPLocationLocked(service, address, ""))
				} else if oldLocation, _ := NodeLocationForAddress(previousLocations, address); oldLocation != "" {
					newLocation, _ := NodeLocationForAddress(locations, address)
					if oldLocation != newLocation {
						assert.False(t, dt.NRPResources.Locations[oldLocation].Addresses[address].Services.Has(service))
					}
				}
			}
			assert.Equal(t, StateCreated, dt.pendingServiceOps[service].State, "live same-service replacement must not delete its gateway")
			assert.NotContains(t, dt.pendingServiceDeletions, service)
			got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.True(t, hasPodFinalizer(got))
			assert.Empty(t, dt.podFinalizerGuards)
		})
	}
}

func TestPodFinalizerCompletionSerializesLocationMove(t *testing.T) {
	const a, b = "egress-a-natgw", "egress-b-natgw"
	for _, mode := range []string{"interleaved move", "interleaved move with GET failure", "move after completion", "unchanged"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			label := "egress-a"
			podA := unlabelledFinalizerPod(&label, "uid-p", "10.244.0.7")
			kube := fake.NewSimpleClientset(podA)
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			dt.NRPResources.NATGateways.Insert(a, b)
			dt.K8sResources.Egresses.Insert(a, b)
			for _, service := range []string{a, b} {
				dt.pendingServiceOps[service] = &ServiceOperationState{
					ServiceName: service, Config: NewOutboundServiceConfig(service, nil), State: StateCreated,
				}
			}
			dt.podInformerAddPod(podA)
			dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
			podB := podA.DeepCopy()
			podB.Labels[consts.PodLabelServiceEgressGateway] = "egress-b"
			if _, err := kube.CoreV1().Pods("ns").Update(ctx, podB, metav1.UpdateOptions{}); !assert.NoError(t, err) {
				return
			}
			dt.reconcileEgressPodUpdate(podA, podB)
			dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
			dt.CheckPendingServiceDeletions()
			moved := podB.DeepCopy()
			target := "10.0.0.1"
			if mode != "unchanged" {
				target = "10.0.0.2"
				moved.Status.HostIP = target
			}
			if strings.HasPrefix(mode, "interleaved") {
				entered, proceed := make(chan struct{}), make(chan struct{})
				var once sync.Once
				resume := func() { once.Do(func() { close(proceed) }) }
				defer resume()
				var captured atomic.Bool
				kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					if captured.CompareAndSwap(false, true) {
						snapshot := podB.DeepCopy()
						close(entered)
						<-proceed
						return true, snapshot, nil
					}
					if mode == "interleaved move with GET failure" {
						return true, nil, apierrors.NewServiceUnavailable("read unavailable")
					}
					return false, nil, nil
				})
				dt.NRPResources.NATGateways.Delete(a)
				done := make(chan struct{})
				var completionErr error
				go func() {
					defer close(done)
					completionErr = dt.RemoveLastPodFinalizers(ctx, a)
				}()
				var movedDone chan struct{}
				defer func() {
					resume()
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Error("completion did not stop")
					}
					if movedDone != nil {
						select {
						case <-movedDone:
						case <-time.After(3 * time.Second):
							t.Error("move did not stop")
						}
					}
				}()
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("completion never captured its snapshot")
				}
				// The tracker bypasses the fake client's mutex held by the blocked GET.
				if err := kube.Tracker().Update(v1.SchemeGroupVersion.WithResource("pods"), moved, "ns"); !assert.NoError(t, err) {
					return
				}
				movedDone = make(chan struct{})
				go func() {
					defer close(movedDone)
					dt.reconcileEgressPodUpdate(podB, moved)
				}()
				if !assert.Eventually(t, func() bool {
					dt.mu.Lock()
					defer dt.mu.Unlock()
					guard := dt.podFinalizerGuards["ns/p"]
					return guard != nil && guard.users >= 2
				}, 3*time.Second, time.Millisecond, "replacement must queue on the Pod guard") {
					return
				}
				dt.mu.Lock()
				retained := dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.7"].PublicOutboundIdentity == b
				dt.mu.Unlock()
				assert.True(t, retained, "replacement drain must wait for the same guard as registration")
				resume()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("completion remained blocked")
				}
				select {
				case <-movedDone:
				case <-time.After(3 * time.Second):
					t.Fatal("registration remained blocked")
				}
				assert.NoError(t, completionErr)
			} else {
				completeEgressCleanup(t, dt, a)
				if _, err := kube.CoreV1().Pods("ns").Update(ctx, moved, metav1.UpdateOptions{}); !assert.NoError(t, err) {
					return
				}
				dt.reconcileEgressPodUpdate(podB, moved)
			}
			assert.Equal(t, b, dt.K8sResources.Nodes[target].Pods["10.244.0.7"].PublicOutboundIdentity)
			dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
			assert.True(t, dt.NRPResources.Locations[target].Addresses["10.244.0.7"].Services.Has(b))
			if target != "10.0.0.1" {
				assert.NotEqual(t, b, dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.7"].PublicOutboundIdentity)
				assert.False(t, dt.NRPResources.Locations["10.0.0.1"].Addresses["10.244.0.7"].Services.Has(b))
			}
			assert.Equal(t, StateCreated, dt.pendingServiceOps[b].State)
			assert.Empty(t, dt.podFinalizerGuards)
		})
	}
}

func TestPodRegistrationUsesCurrentStatus(t *testing.T) {
	const service = "egress-a-natgw"
	for _, state := range []string{
		"changed status", "no IPs", "malformed host", "malformed address", "update failure",
		"updated status", "GET failure", "cancel during GET",
	} {
		t.Run(state, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			label := "egress-a"
			observed := unlabelledFinalizerPod(&label, "uid-p", "10.244.0.7")
			current := observed.DeepCopy()
			current.Status.HostIP = "10.0.0.2"
			current.Status.HostIPs = []v1.HostIP{{IP: "10.0.0.2"}, {IP: "fd00::2"}}
			current.Status.PodIP = "10.244.0.8"
			current.Status.PodIPs = []v1.PodIP{{IP: "10.244.0.8"}, {IP: "fd00::8"}}
			switch state {
			case "no IPs":
				current.Status.PodIP, current.Status.PodIPs = "", nil
			case "malformed host":
				current.Status.HostIP = "invalid"
			case "malformed address":
				current.Status.PodIP = "invalid"
				current.Status.PodIPs = []v1.PodIP{{IP: "invalid"}}
			case "update failure", "updated status":
				current.Finalizers = []string{"example.com/keep"}
			}
			kube := fake.NewSimpleClientset(current)
			switch state {
			case "update failure":
				kube.PrependReactor("update", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewServiceUnavailable("update unavailable")
				})
			case "updated status":
				kube.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
					updated := action.(k8stesting.UpdateAction).GetObject().(*v1.Pod).DeepCopy()
					updated.Status.HostIP = "10.0.0.3"
					updated.Status.HostIPs[0].IP = "10.0.0.3"
					if err := kube.Tracker().Update(v1.SchemeGroupVersion.WithResource("pods"), updated, "ns"); err != nil {
						return true, nil, err
					}
					current = updated
					return true, updated, nil
				})
			case "GET failure":
				kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewServiceUnavailable("read unavailable")
				})
				current = observed
			case "cancel during GET":
				kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					cancel()
					return false, nil, nil
				})
			}
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			dt.NRPResources.NATGateways.Insert(service)
			unlock, err := dt.lockPodFinalizer(ctx, "ns/p")
			if !assert.NoError(t, err) {
				return
			}
			dt.podInformerRegisterPodGuarded(ctx, observed)
			unlock()
			wantEmpty := state == "no IPs" || state == "malformed host" || state == "malformed address" || state == "cancel during GET"
			if wantEmpty {
				assert.Empty(t, dt.K8sResources.Nodes)
				assert.Empty(t, dt.pendingPods)
			} else {
				for _, address := range PodEgressAddresses(current) {
					location, ok := NodeLocationForAddress(PodNodeLocationsByFamily(current), address)
					assert.True(t, ok)
					assert.Equal(t, service, dt.K8sResources.Nodes[location].Pods[address].PublicOutboundIdentity)
					assert.Equal(t, "uid-p", dt.K8sResources.Nodes[location].Pods[address].OutboundPodUID)
				}
				if state != "GET failure" {
					assert.NotEqual(t, service, dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.7"].PublicOutboundIdentity)
				}
			}
			assert.Empty(t, dt.podFinalizerGuards)
		})
	}
}

func TestPodFinalizerCleansDepartedUnobservedStatus(t *testing.T) {
	const service = "egress-a-natgw"
	backoff := finalizerRetryBackoff
	finalizerRetryBackoff.Steps = 1
	t.Cleanup(func() { finalizerRetryBackoff = backoff })
	for _, family := range []struct {
		name  string
		nodes []string
		old   []string
		fresh []string
		next  []string
	}{
		{name: "ipv4", nodes: []string{"10.0.0.1"}, old: []string{"10.244.0.7"}, fresh: []string{"10.244.0.8"}, next: []string{"10.244.0.9"}},
		{name: "ipv6", nodes: []string{"fd00::1"}, old: []string{"fd00::7"}, fresh: []string{"fd00::8"}, next: []string{"fd00::9"}},
		{name: "dual", nodes: []string{"10.0.0.1", "fd00::1"}, old: []string{"10.244.0.7", "fd00::7"}, fresh: []string{"10.244.0.8", "fd00::8"}, next: []string{"10.244.0.9", "fd00::9"}},
	} {
		t.Run(family.name, func(t *testing.T) {
			for _, ready := range []bool{false, true} {
				for _, replacement := range []bool{false, true} {
					t.Run(fmt.Sprintf("ready=%t/replacement=%t", ready, replacement), func(t *testing.T) {
						ctx := context.Background()
						label := "egress-a"
						makePod := func(uid string, addresses []string) *v1.Pod {
							pod := unlabelledFinalizerPod(&label, uid, addresses...)
							pod.Finalizers = nil
							pod.Status.HostIP = family.nodes[0]
							for _, node := range family.nodes {
								pod.Status.HostIPs = append(pod.Status.HostIPs, v1.HostIP{IP: node})
							}
							return pod
						}
						observed, current := makePod("uid-p", family.old), makePod("uid-p", family.fresh)
						kube := fake.NewSimpleClientset(current)
						kube.PrependReactor("update", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
							return true, nil, apierrors.NewServiceUnavailable("finalizer update unavailable")
						})
						dt := newTestDiffTracker()
						dt.kubeClient = kube
						dt.podInformerAddPod(observed)
						op := dt.pendingServiceOps[service]
						if !assert.NotNil(t, op) {
							return
						}
						op.State = StateCreationInProgress
						inFlight := op.Config
						op.InFlightConfig = &inFlight
						if ready {
							dt.NRPResources.NATGateways.Insert(service)
							dt.OnServiceCreationComplete(service, true, nil)
							dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
						}
						assert.NoError(t, kube.CoreV1().Pods("ns").Delete(ctx, "p", metav1.DeleteOptions{}))
						if replacement {
							next := makePod("uid-next", family.next)
							_, err := kube.CoreV1().Pods("ns").Create(ctx, next, metav1.CreateOptions{})
							assert.NoError(t, err)
							dt.podInformerAddPod(next)
						}
						dt.podInformerRemovePod(observed)
						for _, pending := range dt.pendingPods[service] {
							assert.NotEqual(t, "uid-p", pending.PodUID)
						}
						for i, address := range family.fresh {
							assert.NotEqual(t, "uid-p", dt.K8sResources.Nodes[family.nodes[i]].Pods[address].OutboundPodUID)
						}
						if !replacement {
							assert.True(t, op.State == StateDeletionPending || op.State == StateDeletionInProgress)
						}
						if !ready {
							dt.NRPResources.NATGateways.Insert(service)
							dt.OnServiceCreationComplete(service, true, nil)
						}
						for i, address := range family.fresh {
							assert.NotEqual(t, "uid-p", dt.K8sResources.Nodes[family.nodes[i]].Pods[address].OutboundPodUID)
							if replacement {
								assert.Equal(t, "uid-next", dt.K8sResources.Nodes[family.nodes[i]].Pods[family.next[i]].OutboundPodUID)
							}
						}
						assert.Empty(t, dt.podFinalizerGuards)
					})
				}
			}
		})
	}
}

func TestPodRegistrationReconcilesUnobservedAddresses(t *testing.T) {
	const service = "egress-a-natgw"
	for _, family := range []struct {
		name  string
		nodes []string
		moved []string
		old   []string
		mid   []string
		last  []string
	}{
		{name: "ipv4", nodes: []string{"10.0.0.1"}, moved: []string{"10.0.0.2"}, old: []string{"10.244.0.7"}, mid: []string{"10.244.0.8"}, last: []string{"10.244.0.9"}},
		{name: "ipv6", nodes: []string{"fd00::1"}, moved: []string{"fd00::2"}, old: []string{"fd00::7"}, mid: []string{"fd00::8"}, last: []string{"fd00::9"}},
		{name: "dual", nodes: []string{"10.0.0.1", "fd00::1"}, moved: []string{"10.0.0.2", "fd00::2"}, old: []string{"10.244.0.7", "fd00::7"}, mid: []string{"10.244.0.8", "fd00::8"}, last: []string{"10.244.0.9", "fd00::9"}},
	} {
		t.Run(family.name, func(t *testing.T) {
			for _, mode := range []string{"buffered", "creating", "ready", "moved"} {
				t.Run(mode, func(t *testing.T) {
					ctx := context.Background()
					label := "egress-a"
					makePod := func(nodes, addresses []string) *v1.Pod {
						pod := unlabelledFinalizerPod(&label, "uid-p", addresses...)
						pod.Status.HostIP = nodes[0]
						for _, node := range nodes {
							pod.Status.HostIPs = append(pod.Status.HostIPs, v1.HostIP{IP: node})
						}
						return pod
					}
					old := makePod(family.nodes, family.old)
					mid := makePod(family.nodes, family.mid)
					nodes, addresses := family.nodes, family.last
					if mode == "moved" {
						nodes, addresses = family.moved, family.mid
					}
					last := makePod(nodes, addresses)
					kube := fake.NewSimpleClientset(mid)
					dt := newTestDiffTracker()
					dt.kubeClient = kube
					dt.podInformerAddPod(old)
					op := dt.pendingServiceOps[service]
					if !assert.NotNil(t, op) {
						return
					}
					complete := func() {
						if op.State == StateNotStarted {
							op.State = StateCreationInProgress
						}
						if !assert.Equal(t, StateCreationInProgress, op.State) {
							return
						}
						dt.NRPResources.NATGateways.Insert(service)
						dt.OnServiceCreationComplete(service, true, nil)
					}
					if mode == "creating" {
						op.State = StateCreationInProgress
						inFlight := op.Config
						op.InFlightConfig = &inFlight
					}
					if mode == "ready" || mode == "moved" {
						complete()
						dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
					}
					_, err := kube.CoreV1().Pods("ns").Update(ctx, last, metav1.UpdateOptions{})
					assert.NoError(t, err)
					dt.reconcileEgressPodUpdate(old, last)
					assert.Same(t, op, dt.pendingServiceOps[service], "same-service reconciliation must not abort and recreate the operation")
					if mode == "buffered" || mode == "creating" {
						complete()
					}
					dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
					for i, address := range addresses {
						assert.Equal(t, "uid-p", dt.K8sResources.Nodes[nodes[i]].Pods[address].OutboundPodUID)
						assert.NotEqual(t, service, dt.K8sResources.Nodes[family.nodes[i]].Pods[family.mid[i]].PublicOutboundIdentity)
						assert.False(t, dt.NRPResources.Locations[family.nodes[i]].Addresses[family.mid[i]].Services.Has(service))
					}
					count, found := dt.outboundIdentityPodRefCount.Load(service)
					assert.True(t, found)
					assert.Equal(t, len(addresses), count)
					assert.Equal(t, StateCreated, op.State)
					assert.Empty(t, dt.pendingPods[service])
					assert.Empty(t, dt.pendingServiceDeletions)
					dt.reconcileEgressPodUpdate(last, last)
					assert.Empty(t, dt.GetSyncLocationsAddresses().Locations)
					assert.Empty(t, dt.podFinalizerGuards)
				})
			}
		})
	}
}

func TestPodRegistrationReconcilesEgressBeforePublication(t *testing.T) {
	const a, b = "egress-a-natgw", "egress-b-natgw"
	ctx := context.Background()
	labelA, labelB := "egress-a", "egress-b"
	old := unlabelledFinalizerPod(&labelA, "uid-p", "10.244.0.7")
	mid := unlabelledFinalizerPod(&labelA, "uid-p", "10.244.0.8")
	other := unlabelledFinalizerPod(&labelB, "uid-other", "10.244.0.9")
	other.Name = "other"
	kube := fake.NewSimpleClientset(mid, other)
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	dt.podInformerAddPod(old)
	dt.podInformerAddPod(other)
	for _, service := range []string{a, b} {
		op := dt.pendingServiceOps[service]
		if !assert.NotNil(t, op) {
			return
		}
		op.State = StateCreationInProgress
		dt.NRPResources.NATGateways.Insert(service)
		dt.OnServiceCreationComplete(service, true, nil)
	}
	assert.Empty(t, dt.NRPResources.Locations)
	last := mid.DeepCopy()
	last.Labels[consts.PodLabelServiceEgressGateway] = labelB
	_, err := kube.CoreV1().Pods("ns").Update(ctx, last, metav1.UpdateOptions{})
	assert.NoError(t, err)
	dt.reconcileEgressPodUpdate(old, last)
	assert.Equal(t, b, dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.8"].PublicOutboundIdentity)
	assert.Equal(t, "uid-other", dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.9"].OutboundPodUID)
	pending := dt.pendingPodDeletions["ns/p"]
	if assert.NotNil(t, pending) {
		assert.Equal(t, "uid-p", pending.UID)
		assert.True(t, pending.waitsForService(a), "publishing B cannot erase A's previously unobserved last member")
	}
	dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
	dt.CheckPendingServiceDeletions()
	assert.Equal(t, StateDeletionInProgress, dt.pendingServiceOps[a].State)
	assert.Equal(t, StateCreated, dt.pendingServiceOps[b].State)
	assert.Empty(t, dt.podFinalizerGuards)
}

func TestPodRegistrationReconciliationRequiresKnownOwnership(t *testing.T) {
	const service = "egress-a-natgw"
	backoff := finalizerRetryBackoff
	finalizerRetryBackoff.Steps = 1
	t.Cleanup(func() { finalizerRetryBackoff = backoff })
	for _, buffered := range []bool{false, true} {
		for _, tc := range []struct {
			name        string
			uid         string
			key         string
			requestUID  string
			unavailable bool
			remove      bool
		}{
			{name: "same UID", uid: "uid-p", key: "ns/p", requestUID: "uid-p", remove: true},
			{name: "legacy key", uid: "uid-p", requestUID: "uid-p", remove: true},
			{name: "other UID", uid: "uid-other", key: "ns/p", requestUID: "uid-p"},
			{name: "unknown UID", key: "ns/p", requestUID: "uid-p"},
			{name: "other key", uid: "uid-p", key: "ns/other", requestUID: "uid-p"},
			{name: "unknown requesting UID", uid: "uid-p", key: "ns/p"},
			{name: "both UIDs unknown", key: "ns/p"},
			{name: "unavailable API", uid: "uid-p", key: "ns/p", requestUID: "uid-p", unavailable: true},
		} {
			t.Run(fmt.Sprintf("buffered=%t/%s", buffered, tc.name), func(t *testing.T) {
				label := "egress-a"
				observed := unlabelledFinalizerPod(&label, tc.requestUID, "10.244.0.7")
				current := unlabelledFinalizerPod(&label, tc.requestUID, "10.244.0.9")
				kube := fake.NewSimpleClientset(current)
				if tc.unavailable {
					kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewServiceUnavailable("read unavailable")
					})
				}
				dt := newTestDiffTracker()
				dt.kubeClient = kube
				if buffered {
					dt.pendingServiceOps[service] = &ServiceOperationState{
						ServiceName: service, Config: NewOutboundServiceConfig(service, nil), State: StateCreationInProgress,
					}
					dt.pendingPods[service] = []PendingPodUpdate{{PodKey: tc.key, PodUID: tc.uid, Location: "10.0.0.1", Address: "10.244.0.8"}}
				} else {
					dt.NRPResources.NATGateways.Insert(service)
					dt.K8sResources.Nodes["10.0.0.1"] = Node{Pods: map[string]Pod{
						"10.244.0.8": {
							InboundIdentities: utilsets.NewString("inbound-lb"), PublicOutboundIdentity: service,
							OutboundPodKey: tc.key, OutboundPodUID: tc.uid,
						},
					}}
					dt.outboundIdentityPodRefCount.Store(service, 1)
				}
				dt.podInformerAddPod(observed)
				if buffered {
					retained := false
					for _, entry := range dt.pendingPods[service] {
						retained = retained || entry.Address == "10.244.0.8"
					}
					assert.Equal(t, !tc.remove, retained)
					assert.Equal(t, StateCreationInProgress, dt.pendingServiceOps[service].State)
				} else {
					old := dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.8"]
					assert.Equal(t, !tc.remove, old.PublicOutboundIdentity == service)
					assert.True(t, old.InboundIdentities.Has("inbound-lb"))
				}
				assert.Empty(t, dt.podFinalizerGuards)
			})
		}
	}
}

func TestEgressPodReplacementDrainFencesLocationAndUID(t *testing.T) {
	const service = "egress-a-natgw"
	for _, state := range []string{"replacement UID", "different location", "matching old location"} {
		t.Run(state, func(t *testing.T) {
			label := "egress-a"
			old := unlabelledFinalizerPod(&label, "uid-old", "10.244.0.7")
			next := old.DeepCopy()
			next.Status.PodIP = "10.244.0.8"
			next.Status.PodIPs = []v1.PodIP{{IP: "10.244.0.8"}}
			uid, location := "uid-old", "10.0.0.1"
			if state == "replacement UID" {
				uid = "uid-new"
			}
			if state == "different location" {
				location = "10.0.0.2"
			}
			dt := newTestDiffTracker()
			dt.NRPResources.NATGateways.Insert(service)
			dt.K8sResources.Egresses.Insert(service)
			dt.K8sResources.Nodes[location] = Node{Pods: map[string]Pod{
				"10.244.0.7": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: service, OutboundPodKey: "ns/p", OutboundPodUID: uid},
			}}
			dt.outboundIdentityPodRefCount.Store(service, 1)
			dt.podInformerDrainForReplace(old, next)
			retained := dt.K8sResources.Nodes[location].Pods["10.244.0.7"].PublicOutboundIdentity == service
			assert.Equal(t, state != "matching old location", retained)
			assert.Empty(t, dt.pendingPodDeletions)
			assert.Empty(t, dt.pendingServiceDeletions)
			assert.Empty(t, dt.podFinalizerGuards)
		})
	}
}

func TestPodFinalizerCompletionKeepsCurrentLocation(t *testing.T) {
	const a, b = "egress-a-natgw", "egress-b-natgw"
	for _, tt := range []struct {
		name        string
		move        bool
		deliverMove bool
		syncBefore  bool
		v6Only      bool
	}{
		{name: "before location sync", move: true, deliverMove: true},
		{name: "before informer move", move: true},
		{name: "unchanged location", deliverMove: true},
		{name: "location already synced", move: true, deliverMove: true, syncBefore: true},
		{name: "IPv6-only before location sync", move: true, deliverMove: true, v6Only: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			label := "egress-a"
			podA := unlabelledFinalizerPod(&label, "uid-p", "10.244.0.7", "fd00::7")
			if tt.v6Only {
				podA.Status.PodIP = "fd00::7"
				podA.Status.PodIPs = []v1.PodIP{{IP: "fd00::7"}}
			}
			podA.Status.HostIPs = []v1.HostIP{{IP: "10.0.0.1"}, {IP: "fd00::1"}}
			kube := fake.NewSimpleClientset(podA)
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			dt.NRPResources.NATGateways.Insert(a, b)
			dt.K8sResources.Egresses.Insert(a, b)
			for _, service := range []string{a, b} {
				dt.pendingServiceOps[service] = &ServiceOperationState{
					ServiceName: service, Config: NewOutboundServiceConfig(service, nil), State: StateCreated,
				}
			}
			dt.podInformerAddPod(podA)
			dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
			podB := podA.DeepCopy()
			podB.Labels[consts.PodLabelServiceEgressGateway] = "egress-b"
			_, err := kube.CoreV1().Pods("ns").Update(ctx, podB, metav1.UpdateOptions{})
			if !assert.NoError(t, err) {
				return
			}
			dt.reconcileEgressPodUpdate(podA, podB)
			dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
			dt.CheckPendingServiceDeletions()
			pending := dt.pendingPodDeletions["ns/p"]
			if !assert.NotNil(t, pending) {
				return
			}
			assert.False(t, pending.ResolutionPending)
			assert.True(t, pending.waitsForService(a))
			moved := podB.DeepCopy()
			target := "fd00::1"
			if tt.move {
				target = "fd00::2"
				moved.Status.HostIPs[1].IP = target
			}
			_, err = kube.CoreV1().Pods("ns").Update(ctx, moved, metav1.UpdateOptions{})
			if !assert.NoError(t, err) {
				return
			}
			if tt.deliverMove {
				dt.reconcileEgressPodUpdate(podB, moved)
				assert.Equal(t, b, dt.K8sResources.Nodes[target].Pods["fd00::7"].PublicOutboundIdentity)
			}
			if tt.syncBefore {
				dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
			}
			gets, lostCurrent := 0, false
			kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				gets++
				if gets == 2 && tt.deliverMove {
					dt.mu.Lock()
					lostCurrent = dt.K8sResources.Nodes[target].Pods["fd00::7"].PublicOutboundIdentity != b
					dt.mu.Unlock()
				}
				return false, nil, nil
			})
			completeEgressCleanup(t, dt, a)
			assert.False(t, lostCurrent, "a stale NRP location must not remove the currently desired location before re-registration")
			assert.Equal(t, b, dt.K8sResources.Nodes[target].Pods["fd00::7"].PublicOutboundIdentity)
			dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
			dt.CheckPendingPodDeletions(ctx)
			assert.True(t, dt.NRPResources.Locations[target].Addresses["fd00::7"].Services.Has(b))
			assert.Equal(t, StateCreated, dt.pendingServiceOps[b].State)
			assert.Empty(t, dt.pendingPodDeletions)
			got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.True(t, hasPodFinalizer(got))
			assert.Contains(t, got.Finalizers, "example.com/keep")
			assert.Empty(t, dt.podFinalizerGuards)
		})
	}
}

func TestEgressPodUpdateActions(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name        string
		oldPod      *v1.Pod
		newPod      *v1.Pod
		wantRemove  bool
		wantAdd     bool
		wantRelease bool
	}{
		{
			name:        "Label emptied on a pod with our finalizer releases through the drain",
			oldPod:      newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:      withPodFinalizer(newTestPod("default", "test", "", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil)),
			wantRelease: true,
		},
		{
			name:       "Label emptied on a pod without our finalizer removes it from the old egress",
			oldPod:     newTestPod("default", "test", "egress-a", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			newPod:     newTestPod("default", "test", "", "10.0.0.1", "10.0.1.1", v1.PodRunning, nil),
			wantRemove: true,
		},
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
			name:       "Inactive IP-less pod resync retains cleanup ownership",
			oldPod:     withPodFinalizer(newTestPod("default", "test", "egress-a", "", "", v1.PodSucceeded, nil)),
			newPod:     withPodFinalizer(newTestPod("default", "test", "egress-a", "", "", v1.PodSucceeded, nil)),
			wantRemove: true,
		},
		{
			name:       "IP-less pod gets deletion timestamp and changes label",
			oldPod:     withPodFinalizer(newTestPod("default", "test", "egress-a", "", "", v1.PodRunning, nil)),
			newPod:     withPodFinalizer(newTestPod("default", "test", "egress-b", "", "", v1.PodRunning, &now)),
			wantRemove: true,
		},
		{
			name:   "Inactive IP-less pod without our finalizer needs no cleanup",
			oldPod: newTestPod("default", "test", "egress-a", "", "", v1.PodRunning, nil),
			newPod: newTestPod("default", "test", "egress-a", "", "", v1.PodSucceeded, nil),
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
			needsRemove, needsAdd, release, _ := egressPodUpdateActions(tt.oldPod, tt.newPod)
			assert.Equal(t, tt.wantRemove, needsRemove, "needsRemove")
			assert.Equal(t, tt.wantAdd, needsAdd, "needsAdd")
			assert.Equal(t, tt.wantRelease, release, "releaseUnlabelledFinalizer")
		})
	}
}

func withPodFinalizer(pod *v1.Pod) *v1.Pod {
	pod.Finalizers = append(pod.Finalizers, ServiceGatewayPodCleanupFinalizer)
	return pod
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
		needsRemove, needsAdd, _, _ := egressPodUpdateActions(dsPod(v6NodeOld), dsPod(v6NodeNew))
		assert.True(t, needsRemove)
		assert.True(t, needsAdd)
	})
	t.Run("secondary-family node IP appearing re-registers", func(t *testing.T) {
		needsRemove, needsAdd, _, _ := egressPodUpdateActions(dsPod(""), dsPod(v6NodeNew))
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
			pod.DeletionTimestamp = ptr.To(metav1.Now())
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
	dt.AddPod(outboundServiceName("egress-svc"), "default/egress-buffered", "10.0.0.1", "10.244.0.7")

	dt.podInformerRemovePod(pod)

	got, err := kubeClient.CoreV1().Pods("default").Get(context.Background(), "egress-buffered", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"a pod deleted while buffered must have its finalizer removed by the caller, not stranded in Terminating")
}

func TestPodInformerRemovePod_ReservedFinalizerDecision(t *testing.T) {
	for _, tc := range []struct {
		name, label, pendingUID   string
		last, retry, unregistered bool
	}{
		{name: "no pending drain", label: "default"},
		{name: "case-insensitive reserved name", label: "DEFAULT"},
		{name: "RP default alias", label: "policy-default"},
		{name: "previous identity still draining", label: "default", pendingUID: "pod-uid"},
		{name: "previous gateway still deleting", label: "policy-default", pendingUID: "pod-uid", last: true},
		{name: "another UID does not hold this pod", label: "default", pendingUID: "older-uid", last: true},
		{name: "another UID does not lose a retry", label: "default", pendingUID: "older-uid", last: true, retry: true},
		{name: "failed finalizer removal is retried", label: "default", retry: true},
		{name: "rejected raw default label", label: "default-natgw", unregistered: true},
		{name: "rejected raw default label preserves prior drain", label: "default-natgw", pendingUID: "pod-uid", unregistered: true},
	} {
		for _, noIP := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/noIP=%t", tc.name, noIP), func(t *testing.T) {
				ctx := context.Background()
				deleting := metav1.Now()
				pod := newTestPod("ns", "reserved", tc.label, "10.0.0.1", "10.244.0.9", v1.PodRunning, &deleting)
				pod.UID = "pod-uid"
				pod.Finalizers = []string{ServiceGatewayPodCleanupFinalizer, "owner/finalizer"}
				if noIP {
					pod.Status.HostIP, pod.Status.PodIP = "", ""
					pod.Status.HostIPs, pod.Status.PodIPs = nil, nil
				}
				kube := fake.NewSimpleClientset(pod)
				dt := newTestDiffTracker()
				dt.kubeClient = kube
				serviceName := outboundServiceName(tc.label)
				dt.NRPResources.ReservedOutboundServices = utilsets.NewString()
				if !tc.unregistered {
					dt.NRPResources.ReservedOutboundServices.Insert(serviceName)
					dt.NRPResources.NATGateways.Insert(serviceName)
				}
				var pending *PendingPodDeletion
				if tc.pendingUID != "" {
					pending = &PendingPodDeletion{
						Namespace: "ns", Name: pod.Name, UID: tc.pendingUID,
						ServiceName: outboundServiceName("previous"),
						Addresses:   []string{"10.244.0.9"}, IsLastPod: tc.last,
					}
					dt.pendingPodDeletions["ns/"+pod.Name] = pending
				}
				attempts := 0
				if tc.retry {
					kube.PrependReactor("update", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
						attempts++
						if attempts == 1 {
							return true, nil, apierrors.NewInternalError(errors.New("temporary API failure"))
						}
						return false, nil, nil
					})
				}
				var result DeletePodResult
				if noIP {
					result = dt.DeletePodWithoutAddresses(serviceName, "ns", pod.Name, string(pod.UID))
				} else {
					result = dt.DeletePod(serviceName, pod.Status.HostIP, PodEgressAddresses(pod), "ns", pod.Name, string(pod.UID))
				}
				held := tc.pendingUID == string(pod.UID)
				assert.Equal(t, held, result.Enqueued)
				switch {
				case held && tc.last:
					assert.Equal(t, PodFinalizerDecisionHoldForServiceDeletion, result.FinalizerDecision)
				case held:
					assert.Equal(t, PodFinalizerDecisionHoldForDrain, result.FinalizerDecision)
				default:
					assert.Equal(t, PodFinalizerDecisionReleaseNoDrain, result.FinalizerDecision)
				}
				dt.podInformerRemovePod(pod)
				if tc.retry {
					assert.True(t, dt.HasPendingPodDeletion("ns", pod.Name, string(pod.UID)))
					assert.Len(t, dt.locationsUpdaterTrigger, 1)
					assert.False(t, dt.CheckPendingPodDeletions(ctx))
					assert.False(t, dt.HasPendingPodDeletion("ns", pod.Name, string(pod.UID)))
					assert.Equal(t, 2, attempts)
				}
				got, err := kube.CoreV1().Pods("ns").Get(ctx, pod.Name, metav1.GetOptions{})
				assert.NoError(t, err)
				assert.Equal(t, held, hasPodFinalizer(got))
				assert.Contains(t, got.Finalizers, "owner/finalizer")
				if pending != nil && !tc.retry {
					assert.Same(t, pending, dt.pendingPodDeletions["ns/"+pod.Name])
				}
				assert.Empty(t, dt.pendingServiceOps)
				assert.Empty(t, dt.pendingServiceDeletions)
				assert.Empty(t, dt.pendingPods)
				assert.Equal(t, !tc.unregistered, dt.NRPResources.ReservedOutboundServices.Has(serviceName))
				assert.Equal(t, !tc.unregistered, dt.NRPResources.NATGateways.Has(serviceName))
			})
		}
	}
	dt := newTestDiffTracker()
	assert.Equal(t, DeletePodResult{}, dt.DeletePod(DefaultOutboundNATGatewayName, "10.0.0.1", []string{"10.244.0.9"}, "", "", ""))
}

// TestPodInformerAddPod_IgnoresReservedEgressLabel pins that a pod cannot make this controller
// manage the default service or accept its reserved raw label.
func TestPodInformerAddPod_IgnoresReservedEgressLabel(t *testing.T) {
	for _, tc := range []struct {
		label      string
		nrpDefault bool
	}{
		{label: "default"},
		{label: "Default"},
		{label: "DEFAULT"},
		{label: "default-natgw"},
		{label: "DEFAULT-NATGW"},
		{label: "custom-egress", nrpDefault: true},
		{label: "CUSTOM-EGRESS", nrpDefault: true},
	} {
		t.Run(tc.label, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "evil-pod",
					Namespace: "attacker-ns",
					UID:       types.UID("pod-uid"),
					Labels:    map[string]string{consts.PodLabelServiceEgressGateway: tc.label},
				},
				Status: v1.PodStatus{
					Phase:  v1.PodRunning,
					HostIP: "10.0.0.1",
					PodIP:  "10.244.0.9",
					PodIPs: []v1.PodIP{{IP: "10.244.0.9"}},
				},
				Spec: v1.PodSpec{NodeName: "node-1"},
			}

			kube := fake.NewSimpleClientset(pod)
			dt := newProviderDiffTracker(t, ctrl, kube)
			serviceName := outboundServiceName(tc.label)
			if tc.nrpDefault {
				dt.NRPResources.ReservedOutboundServices = utilsets.NewString(strings.ToUpper(serviceName))
			}
			rec := record.NewFakeRecorder(10)
			dt.eventRecorder = rec
			beforeActions := len(kube.Actions())

			dt.podInformerAddPod(pod)

			assert.False(t, dt.IsServiceTracked(serviceName),
				"a pod must never make the controller manage a default outbound service")
			dt.mu.Lock()
			opCount := len(dt.pendingServiceOps)
			dt.mu.Unlock()
			assert.Zero(t, opCount, "no Azure operation may be scheduled for a reserved egress identity")
			assert.Len(t, kube.Actions(), beforeActions, "reserved labels must be rejected before finalizer writes")

			select {
			case ev := <-rec.Events:
				assert.Contains(t, ev, "ServiceGatewayReservedEgressLabel",
					"the user must be told the label is reserved, not merely malformed")
				assert.Contains(t, ev, fmt.Sprintf("Egress label %q is reserved for a default service", strings.ToLower(tc.label)))
			default:
				t.Fatal("expected a ServiceGatewayReservedEgressLabel warning Event")
			}
			assert.Empty(t, rec.Events)
		})
	}
}

// TestPodInformerAddPod_AcceptsOrdinaryEgressLabel is the control: the reserved-name guard must not
// reject ordinary egress identities.
func TestPodInformerAddPod_AcceptsOrdinaryEgressLabel(t *testing.T) {
	for _, label := range []string{"team-egress", "TEAM-EGRESS", "team-natgw", "TEAM-NATGW", "default-natgw2", strings.Repeat("a", maxEgressIdentityLength)} {
		t.Run(label, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "app-pod", Namespace: "team-ns", UID: types.UID("pod-uid"),
					Labels: map[string]string{consts.PodLabelServiceEgressGateway: label},
				},
				Status: v1.PodStatus{
					Phase: v1.PodRunning, HostIP: "10.0.0.1", PodIP: "10.244.0.9",
					PodIPs: []v1.PodIP{{IP: "10.244.0.9"}},
				},
				Spec: v1.PodSpec{NodeName: "node-1"},
			}

			dt := newProviderDiffTracker(t, ctrl, fake.NewSimpleClientset(pod))
			rec := record.NewFakeRecorder(10)
			dt.eventRecorder = rec

			dt.podInformerAddPod(pod)

			assert.True(t, dt.IsServiceTracked(outboundServiceName(label)),
				"an ordinary egress identity must still be registered")
			assert.Equal(t, label, pod.Labels[consts.PodLabelServiceEgressGateway])
			assert.Empty(t, rec.Events)
		})
	}
}

// A failed direct removal remains observable while its pending record owns the retry.
func TestPodInformerRemovePod_CountsForgottenFinalizerRemoval(t *testing.T) {
	RegisterMetrics()
	before, err := testutil.GetCounterMetricValue(podFinalizerRemoveFailedTotal)
	assert.NoError(t, err)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gone-pod", Namespace: "ns", UID: types.UID("pod-uid"),
			Labels:            map[string]string{consts.PodLabelServiceEgressGateway: "team-egress"},
			Finalizers:        []string{ServiceGatewayPodCleanupFinalizer},
			DeletionTimestamp: ptr.To(metav1.Now()),
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
	labelledDT.NRPResources.NATGateways.Insert(outboundServiceName("team-egress"))
	labelledDT.NRPResources.Locations["10.0.0.1"] = NRPLocation{
		Addresses: map[string]NRPAddress{"10.244.0.5": {Services: utilsets.NewString(outboundServiceName("team-egress"))}},
	}
	labelledDT.pendingServiceOps[outboundServiceName("team-egress")] = &ServiceOperationState{
		ServiceName: outboundServiceName("team-egress"),
		Config:      NewOutboundServiceConfig(outboundServiceName("team-egress"), nil),
		State:       StateCreated,
	}

	labelledDT.podInformerRemovePod(labelled)

	gotLabelled, err := labelledKube.CoreV1().Pods("default").Get(context.Background(), "egress-pod", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.True(t, hasFinalizer(gotLabelled.Finalizers, ServiceGatewayPodCleanupFinalizer),
		"control: a labelled pod keeps its finalizer until its addresses drain from NRP")
}

func TestSetUpPodInformer_ReleasesPodsUnlabelledDuringInitialization(t *testing.T) {
	ctx := context.Background()
	tracked := unlabelledFinalizerPod(nil, "uid-p", "10.244.0.7")
	untracked := unlabelledFinalizerPod(nil, "uid-q", "10.244.0.9")
	untracked.Name = "q"
	kube := fake.NewSimpleClientset(tracked, untracked)
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	dt.NRPResources.NATGateways.Insert("egress-a-natgw")
	dt.K8sResources.Egresses.Insert("egress-a-natgw")
	dt.K8sResources.Nodes["10.0.0.1"] = Node{Pods: map[string]Pod{
		"10.244.0.7": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: "egress-a-natgw", OutboundPodKey: "ns/p", OutboundPodUID: "uid-p"},
	}}
	dt.outboundIdentityPodRefCount.Store("egress-a-natgw", 1)
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
		"10.244.0.7": {Services: utilsets.NewString("egress-a-natgw")},
	}}
	stopCh := make(chan struct{})
	defer close(stopCh)

	assert.NoError(t, dt.SetUpPodInformer(stopCh))

	got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer, "a tracked pod drains before release")
	dt.mu.Lock()
	_, published := dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.7"]
	_, pending := dt.pendingPodDeletions["ns/p"]
	dt.mu.Unlock()
	assert.False(t, published, "the pod stops being published")
	assert.True(t, pending)
	got, err = kube.CoreV1().Pods("ns").Get(ctx, "q", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer)
	assert.Contains(t, got.Finalizers, "example.com/keep")
}

type podRegistrationBarrier struct {
	record.EventRecorder
	entered chan struct{}
	proceed <-chan struct{}
}

func (r *podRegistrationBarrier) Event(_ runtime.Object, _, reason, _ string) {
	if reason == "ServiceGatewayNoNodeLocation" {
		close(r.entered)
		<-r.proceed
	}
}

func TestPodRegistrationVerificationRetriesAreIdempotent(t *testing.T) {
	const a, b, unknown = "egress-a-natgw", "egress-b-natgw", "egress-unknown-natgw"
	for _, buffered := range []bool{false, true} {
		for _, mode := range []string{"service_wait", "unbacked_network", "nrp_mapping", "without_unknown_control", "service_completed_control"} {
			t.Run(fmt.Sprintf("buffered=%t/%s", buffered, mode), func(t *testing.T) {
				ctx := context.Background()
				pod := unlabelledFinalizerPod(nil, "uid-p")
				kube := fake.NewSimpleClientset(pod)
				dt := newTestDiffTracker()
				dt.kubeClient = kube
				dt.NRPResources.NATGateways.Insert(a)
				dt.K8sResources.Egresses.Insert(a)
				dt.pendingServiceOps[a] = &ServiceOperationState{
					ServiceName: a, Config: NewOutboundServiceConfig(a, nil), State: StateCreated,
				}
				dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
					"10.244.0.99": {Services: utilsets.NewString(a)},
				}}
				if mode == "nrp_mapping" {
					dt.NRPResources.Locations["10.0.0.1"].Addresses["10.244.0.7"] = NRPAddress{Services: utilsets.NewString(a)}
				}
				recoverUnlabelledPodFinalizers(ctx, dt)
				pending := dt.pendingPodDeletions["ns/p"]
				if !assert.NotNil(t, pending) {
					return
				}
				assert.True(t, pending.VerifyUnknownEgressDrain)
				podA := pod.DeepCopy()
				podA.Labels[consts.PodLabelServiceEgressGateway] = "egress-a"
				podA.Status.PodIP = "10.244.0.7"
				podA.Status.PodIPs = []v1.PodIP{{IP: "10.244.0.7"}}
				if _, err := kube.CoreV1().Pods("ns").Update(ctx, podA, metav1.UpdateOptions{}); !assert.NoError(t, err) {
					return
				}
				dt.podInformerAddPod(podA)
				podB := podA.DeepCopy()
				podB.Labels[consts.PodLabelServiceEgressGateway] = "egress-b"
				if _, err := kube.CoreV1().Pods("ns").Update(ctx, podB, metav1.UpdateOptions{}); !assert.NoError(t, err) {
					return
				}
				dt.reconcileEgressPodUpdate(podA, podB)
				op := dt.pendingServiceOps[b]
				if !assert.NotNil(t, op) {
					return
				}
				assert.Equal(t, StateNotStarted, op.State)
				assert.Len(t, dt.pendingPods[b], 1)
				op.State = StateCreationInProgress
				if !buffered {
					dt.NRPResources.NATGateways.Insert(b)
					dt.OnServiceCreationComplete(b, true, nil)
					assert.Equal(t, StateCreated, op.State)
					assert.Empty(t, dt.pendingPods[b])
				}
				if mode != "nrp_mapping" {
					dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
					dt.CheckPendingServiceDeletions()
				}
				pending = dt.pendingPodDeletions["ns/p"]
				if !assert.NotNil(t, pending) {
					return
				}
				assert.True(t, pending.VerifyUnknownEgressDrain)
				assert.True(t, pending.waitsForService(a))
				if mode == "nrp_mapping" {
					assert.Equal(t, StateDeletionPending, dt.pendingServiceOps[a].State)
					assert.NotEmpty(t, dt.GetSyncLocationsAddresses().Locations)
				} else {
					assert.Equal(t, StateDeletionInProgress, dt.pendingServiceOps[a].State)
				}
				if mode == "unbacked_network" {
					dt.NRPResources.NATGateways.Insert(unknown)
					location := dt.NRPResources.Locations["10.0.0.1"]
					if location.Addresses == nil {
						location.Addresses = make(map[string]NRPAddress)
					}
					location.Addresses["10.244.0.99"] = NRPAddress{Services: utilsets.NewString(unknown)}
					dt.NRPResources.Locations["10.0.0.1"] = location
				} else if mode != "nrp_mapping" {
					assert.Empty(t, dt.GetSyncLocationsAddresses().Locations)
				}
				if mode == "without_unknown_control" {
					updated := *pending
					updated.VerifyUnknownEgressDrain = false
					dt.pendingPodDeletions["ns/p"] = &updated
				}
				if mode == "service_completed_control" {
					completeEgressCleanup(t, dt, a)
				}
				select {
				case <-dt.locationsUpdaterTrigger:
				default:
				}
				before := len(dt.pendingPods[b])
				triggers := 0
				for range 32 {
					dt.CheckPendingPodDeletions(ctx)
					select {
					case <-dt.locationsUpdaterTrigger:
						triggers++
					default:
					}
				}
				assert.Zero(t, triggers, "no-op retries must not bypass LocationsUpdater backoff")
				assert.Len(t, dt.pendingPods[b], before, "unchanged intent must not accumulate duplicate buffers")
				if mode != "service_completed_control" {
					pending = dt.pendingPodDeletions["ns/p"]
					if assert.NotNil(t, pending) {
						assert.True(t, pending.waitsForService(a))
					}
				}
				got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
				assert.NoError(t, err)
				assert.True(t, hasPodFinalizer(got))
				if !buffered {
					assert.Equal(t, b, dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.7"].PublicOutboundIdentity)
				}
				assert.Empty(t, dt.podFinalizerGuards)
			})
		}
	}
}

func TestPodRegistrationSerializesFinalizerRelease(t *testing.T) {
	const service = "egress-b-natgw"
	ctx := context.Background()
	label := "egress-b"
	pod := unlabelledFinalizerPod(&label, "uid-p", "fd00::7", "10.244.0.7")
	kube := fake.NewSimpleClientset(pod)
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	dt.NRPResources.NATGateways.Insert(service)
	dt.pendingPodDeletions["ns/p"] = &PendingPodDeletion{
		Namespace: "ns", Name: "p", UID: "uid-p",
		ServiceName: "egress-a-natgw", Addresses: []string{"10.244.0.7"},
	}
	entered, proceed := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	resume := func() { resumeOnce.Do(func() { close(proceed) }) }
	defer resume()
	dt.SetEventRecorder(&podRegistrationBarrier{entered: entered, proceed: proceed})
	addDone := make(chan struct{})
	go func() {
		defer close(addDone)
		dt.podInformerAddPod(pod)
	}()
	defer func() {
		resume()
		select {
		case <-addDone:
		case <-time.After(3 * time.Second):
			t.Error("registration did not stop")
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("registration never reached the per-family event after ensuring its finalizer")
	}
	current := pod.DeepCopy()
	current.Labels = nil
	_, err := kube.CoreV1().Pods("ns").Update(ctx, current, metav1.UpdateOptions{})
	if !assert.NoError(t, err) {
		return
	}
	checkDone := make(chan struct{})
	go func() {
		defer close(checkDone)
		dt.CheckPendingPodDeletions(ctx)
	}()
	defer func() {
		resume()
		select {
		case <-checkDone:
		case <-time.After(3 * time.Second):
			t.Error("finalizer check did not stop")
		}
	}()
	assert.Eventually(t, func() bool {
		dt.mu.Lock()
		defer dt.mu.Unlock()
		guard := dt.podFinalizerGuards["ns/p"]
		return guard != nil && guard.users == 2
	}, time.Second, time.Millisecond, "release must wait until registration publishes all its addresses")
	select {
	case <-checkDone:
		t.Error("release ran between finalizer protection and address publication")
	default:
	}
	resume()
	<-addDone
	<-checkDone
	dt.CheckPendingPodDeletions(ctx)
	got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.True(t, hasPodFinalizer(got))
	pending := dt.pendingPodDeletions["ns/p"]
	if assert.NotNil(t, pending) {
		assert.True(t, pending.waitsForService(service))
	}
	assert.Empty(t, dt.podFinalizerGuards)
}

func TestPodLookupRetryCleansSupersededEgress(t *testing.T) {
	const a, b = "egress-a-natgw", "egress-b-natgw"
	for _, tt := range []struct {
		name                string
		failRead            bool
		relabel             bool
		registerBeforeRetry bool
		emptyUpdate         bool
	}{
		{name: "failed read then B", failRead: true, relabel: true},
		{name: "unlabelled retry", failRead: true},
		{name: "healthy removal then B", relabel: true},
		{name: "B registered and A drained before retry", failRead: true, relabel: true, registerBeforeRetry: true},
		{name: "empty update then B registered before retry", failRead: true, relabel: true, registerBeforeRetry: true, emptyUpdate: true},
		{name: "healthy empty update then B", relabel: true, registerBeforeRetry: true, emptyUpdate: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			label := "egress-a"
			event := unlabelledFinalizerPod(&label, "uid-p", "10.244.0.7")
			current := unlabelledFinalizerPod(nil, "uid-p", "10.244.0.7")
			if tt.emptyUpdate {
				current.Labels = map[string]string{consts.PodLabelServiceEgressGateway: ""}
			}
			kube := fake.NewSimpleClientset(current)
			var unavailable atomic.Bool
			unavailable.Store(tt.failRead)
			kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				if unavailable.Load() {
					return true, nil, apierrors.NewServiceUnavailable("lookup unavailable")
				}
				return false, nil, nil
			})
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			dt.NRPResources.NATGateways.Insert(a, b)
			dt.K8sResources.Egresses.Insert(a, b)
			dt.K8sResources.Nodes["10.0.0.1"] = Node{Pods: map[string]Pod{
				"10.244.0.7": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: a, OutboundPodKey: "ns/p", OutboundPodUID: "uid-p"},
				"10.244.0.9": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: b, OutboundPodKey: "ns/other", OutboundPodUID: "uid-other"},
			}}
			dt.outboundIdentityPodRefCount.Store(a, 1)
			dt.outboundIdentityPodRefCount.Store(b, 1)
			for _, service := range []string{a, b} {
				dt.pendingServiceOps[service] = &ServiceOperationState{
					ServiceName: service, Config: NewOutboundServiceConfig(service, nil), State: StateCreated,
				}
			}
			dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
				"10.244.0.7": {Services: utilsets.NewString(a)},
				"10.244.0.9": {Services: utilsets.NewString(b)},
			}}
			if tt.emptyUpdate {
				dt.reconcileEgressPodUpdate(event, current)
			} else {
				dt.podInformerRemovePod(event)
			}
			if tt.failRead {
				pending := dt.pendingPodDeletions["ns/p"]
				if !assert.NotNil(t, pending) {
					return
				}
				assert.True(t, pending.ResolutionPending)
				assert.Equal(t, a, dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.7"].PublicOutboundIdentity)
				assert.Equal(t, StateCreated, dt.pendingServiceOps[a].State, "failed discovery must not drain before resolving fresh intent")
			}
			beforeRelabel := current.DeepCopy()
			if tt.relabel {
				current.Labels = map[string]string{consts.PodLabelServiceEgressGateway: "egress-b"}
				_, err := kube.CoreV1().Pods("ns").Update(ctx, current, metav1.UpdateOptions{})
				assert.NoError(t, err)
			}
			unavailable.Store(false)
			if tt.registerBeforeRetry {
				dt.reconcileEgressPodUpdate(beforeRelabel, current)
				dt.NRPResources.Locations["10.0.0.1"].Addresses["10.244.0.7"] = NRPAddress{Services: utilsets.NewString(b)}
			}
			if tt.failRead {
				dt.CheckPendingPodDeletions(ctx)
			} else if tt.relabel {
				dt.podInformerAddPod(current)
			}
			got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.True(t, hasPodFinalizer(got))
			op := dt.pendingServiceOps[a]
			if assert.NotNil(t, op) {
				assert.Equal(t, StateDeletionPending, op.State, "A still needs last-Pod cleanup")
			}
			pending := dt.pendingPodDeletions["ns/p"]
			if assert.NotNil(t, pending) {
				assert.True(t, pending.waitsForService(a))
			}
		})
	}
}

func TestPodLookupRetryRetainsUnfinalizedCleanup(t *testing.T) {
	const a, b = "egress-a-natgw", "egress-b-natgw"
	backoff := finalizerRetryBackoff
	finalizerRetryBackoff.Steps = 1
	t.Cleanup(func() { finalizerRetryBackoff = backoff })
	for _, tt := range []struct {
		name               string
		failRead           bool
		owned              bool
		gone               bool
		replacement        string
		sameIP             bool
		mappingReplaced    bool
		restored           bool
		releaseReplacement bool
	}{
		{name: "unfinalized removal", failRead: true},
		{name: "gone Pod", failRead: true, gone: true},
		{name: "replacement new IP", failRead: true, replacement: "egress-b"},
		{name: "replacement same IP", failRead: true, replacement: "egress-b", sameIP: true},
		{name: "replacement after sync", failRead: true, replacement: "egress-b", sameIP: true, mappingReplaced: true},
		{name: "replacement later unlabelled", failRead: true, replacement: "egress-b", releaseReplacement: true},
		{name: "same identity replacement", failRead: true, replacement: "egress-a", sameIP: true},
		{name: "healthy removal"},
		{name: "owned finalizer", failRead: true, owned: true},
		{name: "restored live intent", failRead: true, restored: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			label := "egress-a"
			original := unlabelledFinalizerPod(&label, "uid-old", "10.244.0.7")
			if !tt.owned {
				original.Finalizers = nil
			}
			kube := fake.NewSimpleClientset(original)
			var failWrites, failReads atomic.Bool
			failWrites.Store(!tt.owned)
			kube.PrependReactor("update", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				if failWrites.Load() {
					return true, nil, apierrors.NewServiceUnavailable("finalizer add unavailable")
				}
				return false, nil, nil
			})
			kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				if failReads.Load() {
					return true, nil, apierrors.NewServiceUnavailable("removal read unavailable")
				}
				return false, nil, nil
			})
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			dt.NRPResources.NATGateways.Insert(a, b)
			dt.K8sResources.Egresses.Insert(a, b)
			for _, service := range []string{a, b} {
				dt.pendingServiceOps[service] = &ServiceOperationState{
					ServiceName: service, Config: NewOutboundServiceConfig(service, nil), State: StateCreated,
				}
			}
			dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
				"10.244.0.7": {Services: utilsets.NewString(a)},
			}}
			dt.podInformerAddPod(original)
			assert.Equal(t, a, dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.7"].PublicOutboundIdentity)
			got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Equal(t, tt.owned, hasPodFinalizer(got), "exercise the permitted unfinalized registration fallback")
			failWrites.Store(false)
			empty := original.DeepCopy()
			empty.Labels = map[string]string{consts.PodLabelServiceEgressGateway: ""}
			_, err = kube.CoreV1().Pods("ns").Update(ctx, empty, metav1.UpdateOptions{})
			if !assert.NoError(t, err) {
				return
			}
			failReads.Store(tt.failRead)
			dt.reconcileEgressPodUpdate(original, empty)
			if tt.failRead {
				pending := dt.pendingPodDeletions["ns/p"]
				if !assert.NotNil(t, pending) {
					return
				}
				assert.True(t, pending.ResolutionPending)
			}
			failReads.Store(false)
			if tt.gone || tt.replacement != "" {
				if !assert.NoError(t, kube.CoreV1().Pods("ns").Delete(ctx, "p", metav1.DeleteOptions{})) {
					return
				}
			}
			var replacement *v1.Pod
			if tt.replacement != "" {
				address := "10.244.0.8"
				if tt.sameIP {
					address = "10.244.0.7"
				}
				replacement = unlabelledFinalizerPod(&tt.replacement, "uid-new", address)
				_, err := kube.CoreV1().Pods("ns").Create(ctx, replacement, metav1.CreateOptions{})
				if !assert.NoError(t, err) {
					return
				}
				dt.podInformerAddPod(replacement)
				if tt.mappingReplaced {
					dt.NRPResources.Locations["10.0.0.1"].Addresses["10.244.0.7"] = NRPAddress{Services: utilsets.NewString(b)}
				}
				if tt.releaseReplacement {
					departing := replacement.DeepCopy()
					departing.Labels = nil
					_, err := kube.CoreV1().Pods("ns").Update(ctx, departing, metav1.UpdateOptions{})
					if !assert.NoError(t, err) {
						return
					}
					dt.reconcileEgressPodUpdate(replacement, departing)
				}
			}
			if tt.restored {
				_, err := kube.CoreV1().Pods("ns").Update(ctx, original, metav1.UpdateOptions{})
				if !assert.NoError(t, err) {
					return
				}
			}
			dt.CheckPendingPodDeletions(ctx)
			assert.Empty(t, dt.podFinalizerGuards)
			if replacement != nil {
				got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
				assert.NoError(t, err)
				assert.Equal(t, replacement.UID, got.UID)
				assert.True(t, hasPodFinalizer(got))
				assert.Contains(t, got.Finalizers, "example.com/keep")
				if !tt.releaseReplacement {
					state := dt.K8sResources.Nodes["10.0.0.1"].Pods[replacement.Status.PodIP]
					assert.Equal(t, string(replacement.UID), state.OutboundPodUID)
					assert.Equal(t, outboundServiceName(tt.replacement), state.PublicOutboundIdentity)
				}
			}
			if tt.restored || tt.replacement == "egress-a" {
				assert.Equal(t, StateCreated, dt.pendingServiceOps[a].State)
				assert.Equal(t, a, dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.7"].PublicOutboundIdentity)
				return
			}
			for _, node := range dt.K8sResources.Nodes {
				for _, state := range node.Pods {
					assert.False(t, state.OutboundPodUID == "uid-old" && state.PublicOutboundIdentity == a,
						"a lost finalizer cannot abandon the departed Pod's desired registration")
				}
			}
			assert.Equal(t, StateDeletionPending, dt.pendingServiceOps[a].State)
		})
	}
}

func TestPodLookupRetryAcrossReplacements(t *testing.T) {
	backoff := finalizerRetryBackoff
	finalizerRetryBackoff.Steps = 1
	t.Cleanup(func() { finalizerRetryBackoff = backoff })
	for _, tt := range []struct {
		name        string
		generations int
		deleting    bool
		failRead    bool
	}{
		{name: "two UIDs unlabelled", generations: 2, failRead: true},
		{name: "two UIDs ordinary deletion", generations: 2, deleting: true, failRead: true},
		{name: "three UIDs unlabelled", generations: 3, failRead: true},
		{name: "five UIDs ordinary deletion", generations: 5, deleting: true, failRead: true},
		{name: "healthy ordinary deletion", generations: 2, deleting: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			kube := fake.NewSimpleClientset()
			var failWrites, failReads atomic.Bool
			kube.PrependReactor("update", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				if failWrites.Load() {
					return true, nil, apierrors.NewServiceUnavailable("finalizer add unavailable")
				}
				return false, nil, nil
			})
			kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				if failReads.Load() {
					return true, nil, apierrors.NewServiceUnavailable("removal read unavailable")
				}
				return false, nil, nil
			})
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			location := NRPLocation{Addresses: make(map[string]NRPAddress)}
			for i := 0; i < tt.generations; i++ {
				service := outboundServiceName(fmt.Sprintf("egress-%d", i))
				dt.NRPResources.NATGateways.Insert(service)
				dt.K8sResources.Egresses.Insert(service)
				dt.pendingServiceOps[service] = &ServiceOperationState{
					ServiceName: service, Config: NewOutboundServiceConfig(service, nil), State: StateCreated,
				}
				location.Addresses[fmt.Sprintf("10.244.0.%d", i+7)] = NRPAddress{Services: utilsets.NewString(service)}
			}
			dt.NRPResources.Locations["10.0.0.1"] = location
			for i := 0; i < tt.generations; i++ {
				label := fmt.Sprintf("egress-%d", i)
				uid := fmt.Sprintf("uid-%d", i)
				pod := unlabelledFinalizerPod(&label, uid, fmt.Sprintf("10.244.0.%d", i+7))
				pod.Finalizers = nil
				_, err := kube.CoreV1().Pods("ns").Create(ctx, pod, metav1.CreateOptions{})
				if !assert.NoError(t, err) {
					return
				}
				last := i == tt.generations-1
				failWrites.Store(!last)
				dt.podInformerAddPod(pod)
				failWrites.Store(false)
				current, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
				if !assert.NoError(t, err) {
					return
				}
				assert.Equal(t, last, hasPodFinalizer(current))
				if last && tt.deleting {
					current.DeletionTimestamp = ptr.To(metav1.Now())
				} else {
					current.Labels = map[string]string{consts.PodLabelServiceEgressGateway: ""}
				}
				_, err = kube.CoreV1().Pods("ns").Update(ctx, current, metav1.UpdateOptions{})
				if !assert.NoError(t, err) {
					return
				}
				failReads.Store(!last && tt.failRead)
				dt.reconcileEgressPodUpdate(pod, current)
				failReads.Store(false)
				if !last {
					pending := dt.pendingPodDeletions["ns/p"].forUID(uid)
					if !assert.NotNil(t, pending) {
						return
					}
					assert.Equal(t, tt.failRead, pending.ResolutionPending)
					if !assert.NoError(t, kube.CoreV1().Pods("ns").Delete(ctx, "p", metav1.DeleteOptions{})) {
						return
					}
				}
			}
			dt.CheckPendingPodDeletions(ctx)
			for i := 0; i < tt.generations; i++ {
				service := outboundServiceName(fmt.Sprintf("egress-%d", i))
				state := dt.K8sResources.Nodes["10.0.0.1"].Pods[fmt.Sprintf("10.244.0.%d", i+7)]
				assert.NotEqual(t, service, state.PublicOutboundIdentity, "every departed UID must drain")
				assert.Equal(t, StateDeletionPending, dt.pendingServiceOps[service].State)
			}
			dt.UpdateLocationsAddresses(dt.GetSyncLocationsAddresses())
			dt.CheckPendingServiceDeletions()
			completeEgressCleanup(t, dt, outboundServiceName(fmt.Sprintf("egress-%d", tt.generations-1)))
			got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Equal(t, types.UID(fmt.Sprintf("uid-%d", tt.generations-1)), got.UID)
			assert.False(t, hasPodFinalizer(got), "departed UIDs cannot transfer their finalizer gates to the latest Pod")
			assert.Empty(t, dt.pendingPodDeletions)
			assert.Empty(t, dt.podFinalizerGuards)
		})
	}
}

func TestPodRelabelPreservesEarlierServiceCleanup(t *testing.T) {
	const a, b = "egress-a-natgw", "egress-b-natgw"
	for _, tt := range []struct {
		name         string
		oldMapping   bool
		oldCompleted bool
		wantHeld     bool
	}{
		{name: "A addresses drained cleanup pending", wantHeld: true},
		{name: "A mapping still present", oldMapping: true, wantHeld: true},
		{name: "A cleanup completed", oldCompleted: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			labelA, labelB := "egress-a", "egress-b"
			podA := unlabelledFinalizerPod(&labelA, "uid-p", "10.244.0.7")
			podB := unlabelledFinalizerPod(&labelB, "uid-p", "10.244.0.7")
			unlabelled := unlabelledFinalizerPod(nil, "uid-p", "10.244.0.7")
			kube := fake.NewSimpleClientset(podA)
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			dt.NRPResources.NATGateways.Insert(a, b)
			dt.K8sResources.Egresses.Insert(a, b)
			dt.K8sResources.Nodes["10.0.0.1"] = Node{Pods: map[string]Pod{
				"10.244.0.7": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: a, OutboundPodKey: "ns/p", OutboundPodUID: "uid-p"},
				"10.244.0.9": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: b, OutboundPodKey: "ns/other", OutboundPodUID: "uid-other"},
			}}
			dt.outboundIdentityPodRefCount.Store(a, 1)
			dt.outboundIdentityPodRefCount.Store(b, 1)
			for _, service := range []string{a, b} {
				dt.pendingServiceOps[service] = &ServiceOperationState{
					ServiceName: service, Config: NewOutboundServiceConfig(service, nil), State: StateCreated,
				}
			}
			dt.setBYONATGatewayID(a, byoNATGatewayARMID("external-a"))
			dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
				"10.244.0.7": {Services: utilsets.NewString(a)},
				"10.244.0.9": {Services: utilsets.NewString(b)},
			}}
			_, err := kube.CoreV1().Pods("ns").Update(ctx, podB, metav1.UpdateOptions{})
			assert.NoError(t, err)
			dt.reconcileEgressPodUpdate(podA, podB)
			op := dt.pendingServiceOps[a]
			if !assert.NotNil(t, op) {
				return
			}
			assert.Equal(t, StateDeletionPending, op.State)
			op.State = StateDeletionInProgress
			mapped := utilsets.NewString(b)
			if tt.oldMapping {
				mapped.Insert(a)
			}
			dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
				"10.244.0.7": {Services: mapped},
				"10.244.0.9": {Services: utilsets.NewString(b)},
			}}
			if tt.oldCompleted {
				completeEgressCleanup(t, dt, a)
				dt.OnServiceCreationComplete(a, true, nil)
			}
			_, err = kube.CoreV1().Pods("ns").Update(ctx, unlabelled, metav1.UpdateOptions{})
			assert.NoError(t, err)
			dt.reconcileEgressPodUpdate(podB, unlabelled)
			delete(dt.NRPResources.Locations["10.0.0.1"].Addresses, "10.244.0.7")
			dt.CheckPendingPodDeletions(ctx)
			got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Contains(t, got.Finalizers, "example.com/keep")
			assert.Equal(t, tt.wantHeld, hasPodFinalizer(got))
			if !tt.oldCompleted {
				assert.Equal(t, byoNATGatewayARMID("external-a"), dt.byoNATGatewayID(a))
				completeEgressCleanup(t, dt, a)
				dt.CheckPendingPodDeletions(ctx)
				got, err = kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
				assert.NoError(t, err)
				assert.False(t, hasPodFinalizer(got))
			}
		})
	}
}

func TestSetUpPodInformerRetainsFailedRecoveryLookup(t *testing.T) {
	ctx := context.Background()
	pod := unlabelledFinalizerPod(nil, "uid-p")
	kube := fake.NewSimpleClientset(pod)
	var unavailable atomic.Bool
	unavailable.Store(true)
	kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if unavailable.Load() {
			return true, nil, apierrors.NewServiceUnavailable("Pod reads unavailable")
		}
		return false, nil, nil
	})
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	stop := make(chan struct{})
	defer close(stop)
	assert.NoError(t, dt.SetUpPodInformer(stop))
	dt.mu.Lock()
	pending := dt.pendingPodDeletions["ns/p"]
	if assert.NotNil(t, pending) {
		assert.True(t, pending.ResolutionPending)
		assert.True(t, pending.RecoveredAtStartup)
	}
	dt.mu.Unlock()
	assert.Len(t, dt.locationsUpdaterTrigger, 1)
	unavailable.Store(false)
	assert.False(t, dt.CheckPendingPodDeletions(ctx))
	got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.False(t, hasPodFinalizer(got))
}

func TestSetUpPodInformerCancelsIncompleteRecovery(t *testing.T) {
	pod := unlabelledFinalizerPod(nil, "uid-p")
	kube := fake.NewSimpleClientset(pod)
	stop := make(chan struct{})
	var stopOnce sync.Once
	cancel := func() { stopOnce.Do(func() { close(stop) }) }
	defer cancel()
	kube.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.ListActionImpl).GetListOptions().LabelSelector == "" {
			cancel()
			return true, nil, apierrors.NewServiceUnavailable("list unavailable")
		}
		return false, nil, nil
	})
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	assert.ErrorIs(t, dt.SetUpPodInformer(stop), context.Canceled)
	assert.Empty(t, dt.pendingPodDeletions)
	assert.Empty(t, dt.podFinalizerGuards)
}

func TestSetUpPodInformer_SweepRechecksPodsRelabelledAfterItsList(t *testing.T) {
	ctx := context.Background()
	label := "egress-a"
	egress := outboundServiceName(label)
	labelled := unlabelledFinalizerPod(&label, "uid-p", "10.244.0.7")
	stale := unlabelledFinalizerPod(nil, "uid-p", "10.244.0.7")
	kube := fake.NewSimpleClientset(labelled)
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	registered := func() bool {
		dt.mu.Lock()
		defer dt.mu.Unlock()
		pod, ok := dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.7"]
		_, pending := dt.pendingPodDeletions["ns/p"]
		return ok && pod.PublicOutboundIdentity == egress && !pending
	}
	kube.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.ListActionImpl).GetListOptions().LabelSelector != "" {
			return false, nil, nil
		}
		for deadline := time.Now().Add(5 * time.Second); !registered() && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		return true, &v1.PodList{Items: []v1.Pod{*stale}}, nil
	})
	dt.NRPResources.NATGateways.Insert(egress)
	dt.K8sResources.Egresses.Insert(egress)
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
		"10.244.0.7": {Services: utilsets.NewString(egress)},
	}}
	stopCh := make(chan struct{})
	defer close(stopCh)

	assert.NoError(t, dt.SetUpPodInformer(stopCh))

	assert.True(t, registered(), "a stale unlabelled list entry must not release a pod the informer registered")
	got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer)
}

func TestPostSyncUnlabelledSweep_RechecksLivePod(t *testing.T) {
	ctx := context.Background()
	label := "egress-a"
	egress := outboundServiceName(label)
	setup := func(t *testing.T, staleGets int) (*DiffTracker, *fake.Clientset) {
		t.Helper()
		labelled := unlabelledFinalizerPod(&label, "uid-p", "10.244.0.7")
		stale := unlabelledFinalizerPod(nil, "uid-p", "10.244.0.7")
		kube := fake.NewSimpleClientset(labelled)
		kube.PrependReactor("list", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
			return true, &v1.PodList{Items: []v1.Pod{*stale}}, nil
		})
		gets := 0
		kube.PrependReactor("get", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
			gets++
			if gets <= staleGets {
				return true, stale.DeepCopy(), nil
			}
			return false, nil, nil
		})
		dt := newTestDiffTracker()
		dt.kubeClient = kube
		dt.NRPResources.NATGateways.Insert(egress)
		dt.K8sResources.Egresses.Insert(egress)
		dt.K8sResources.Nodes["10.0.0.1"] = Node{Pods: map[string]Pod{
			"10.244.0.7": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: egress, OutboundPodKey: "ns/p", OutboundPodUID: "uid-p"},
		}}
		dt.outboundIdentityPodRefCount.Store(egress, 1)
		dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
			"10.244.0.7": {Services: utilsets.NewString(egress)},
		}}
		return dt, kube
	}
	assertRegistered := func(t *testing.T, dt *DiffTracker, kube *fake.Clientset) {
		t.Helper()
		if pod, ok := dt.K8sResources.Nodes["10.0.0.1"].Pods["10.244.0.7"]; assert.True(t, ok, "the relabelled pod must stay registered") {
			assert.Equal(t, egress, pod.PublicOutboundIdentity)
		}
		assert.NotContains(t, dt.pendingPodDeletions, "ns/p")
		got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer)
	}

	t.Run("pod relabelled before the release is skipped", func(t *testing.T) {
		dt, kube := setup(t, 0)
		recovered, direct := recoverUnlabelledPodFinalizers(ctx, dt)
		assert.Zero(t, recovered+direct)
		assertRegistered(t, dt, kube)
	})

	t.Run("pod relabelled during the release is registered again", func(t *testing.T) {
		dt, kube := setup(t, 1)
		recovered, direct := recoverUnlabelledPodFinalizers(ctx, dt)
		assert.Zero(t, recovered+direct, "live re-registration retired the pending recovery without removing its finalizer")
		assertRegistered(t, dt, kube)
	})
}

func TestPostSyncUnlabelledSweep_DoesNotRecountScheduledDrain(t *testing.T) {
	ctx := context.Background()
	pod := unlabelledFinalizerPod(nil, "uid-p", "10.244.0.7")
	kube := fake.NewSimpleClientset(pod)
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	dt.NRPResources.NATGateways.Insert("egress-a-natgw")
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
		"10.244.0.7": {Services: utilsets.NewString("egress-a-natgw")},
	}}
	recoverStuckFinalizers(ctx, dt, nil, nil, nil, utilsets.NewString(), nil)
	RegisterMetrics()
	recovered0, err := testutil.GetCounterMetricValue(finalizersRecoveredTotal)
	assert.NoError(t, err)
	scheduled0, err := testutil.GetCounterMetricValue(finalizersRecoveryScheduledTotal)
	assert.NoError(t, err)

	recovered, direct := recoverUnlabelledPodFinalizers(ctx, dt)

	assert.Zero(t, recovered+direct)
	recovered1, err := testutil.GetCounterMetricValue(finalizersRecoveredTotal)
	assert.NoError(t, err)
	scheduled1, err := testutil.GetCounterMetricValue(finalizersRecoveryScheduledTotal)
	assert.NoError(t, err)
	assert.Zero(t, recovered1-recovered0)
	assert.Zero(t, scheduled1-scheduled0, "a drain scheduled by the first sweep is not counted again")
	if pending := dt.pendingPodDeletions["ns/p"]; assert.NotNil(t, pending) {
		assert.True(t, pending.RecoveredAtStartup)
	}
}

func TestPodInformerRemovePod_LiveRecheckFallsBackToLabelledRemoval(t *testing.T) {
	labelA := "egress-a"
	egressA := outboundServiceName(labelA)
	for _, tc := range []struct {
		name    string
		liveUID string
		liveFin bool
	}{
		{name: "live pod without our finalizer", liveUID: "uid-p"},
		{name: "same-name replacement", liveUID: "uid-q", liveFin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			event := unlabelledFinalizerPod(&labelA, "uid-p", "10.244.0.7")
			event.Finalizers = []string{"example.com/keep"}
			live := unlabelledFinalizerPod(nil, tc.liveUID, "10.244.0.7")
			if !tc.liveFin {
				live.Finalizers = []string{"example.com/keep"}
			}
			kube := fake.NewSimpleClientset(live)
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			dt.K8sResources.Egresses.Insert(egressA)
			dt.K8sResources.Nodes["10.0.0.1"] = Node{Pods: map[string]Pod{
				"10.244.0.7": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: egressA, OutboundPodKey: "ns/p", OutboundPodUID: "uid-p"},
			}}
			dt.outboundIdentityPodRefCount.Store(egressA, 1)

			dt.podInformerRemovePod(event)

			assert.NotContains(t, dt.K8sResources.Nodes["10.0.0.1"].Pods, "10.244.0.7",
				"the stale labelled registration must be removed through the labelled path")
			got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Equal(t, tc.liveFin, hasPodFinalizer(got), "the live pod's finalizer must be left alone")
		})
	}
}

func TestRuntimeUnlabelledPodFinalizerRelease_RelabelChainThenKeyRemovedDrainsFirstEgress(t *testing.T) {
	for _, secondInNRP := range []bool{false, true} {
		t.Run(fmt.Sprintf("second egress in NRP=%v", secondInNRP), func(t *testing.T) {
			ctx := context.Background()
			labelA, labelB := "egress-a", "egress-b"
			egressA, egressB := outboundServiceName(labelA), outboundServiceName(labelB)
			podA := unlabelledFinalizerPod(&labelA, "uid-p", "10.244.0.7")
			podB := unlabelledFinalizerPod(&labelB, "uid-p", "10.244.0.7")
			podNoKey := unlabelledFinalizerPod(nil, "uid-p", "10.244.0.7")
			kube := fake.NewSimpleClientset(podA.DeepCopy())
			dt := newTestDiffTracker()
			dt.kubeClient = kube
			dt.NRPResources.NATGateways.Insert(egressA)
			if secondInNRP {
				dt.NRPResources.NATGateways.Insert(egressB)
			}
			dt.K8sResources.Egresses.Insert(egressA)
			dt.K8sResources.Nodes["10.0.0.1"] = Node{Pods: map[string]Pod{
				"10.244.0.7": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: egressA, OutboundPodKey: "ns/p", OutboundPodUID: "uid-p"},
			}}
			dt.outboundIdentityPodRefCount.Store(egressA, 1)
			dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
				"10.244.0.7": {Services: utilsets.NewString(egressA)},
			}}

			_, err := kube.CoreV1().Pods("ns").Update(ctx, podB.DeepCopy(), metav1.UpdateOptions{})
			assert.NoError(t, err)
			dt.reconcileEgressPodUpdate(podA, podB)
			_, err = kube.CoreV1().Pods("ns").Update(ctx, podNoKey.DeepCopy(), metav1.UpdateOptions{})
			assert.NoError(t, err)
			dt.podInformerRemovePod(podB)
			dt.CheckPendingPodDeletions(ctx)

			got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
				"the finalizer must hold while NRP still maps the address to the first egress")
			if pending := dt.pendingPodDeletions["ns/p"]; assert.NotNil(t, pending) {
				_, found := pending.cleanupFor(egressA)
				assert.True(t, found)
			}

			dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{}}
			dt.CheckPendingPodDeletions(ctx)
			completeEgressCleanup(t, dt, egressA, egressB)
			got, err = kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer)
			assert.Contains(t, got.Finalizers, "example.com/keep")
		})
	}
}

func TestRuntimeUnlabelledPodFinalizerRelease_RelabelChainThenEmptyDrainsFirstEgress(t *testing.T) {
	ctx := context.Background()
	labelA, labelB, empty := "egress-a", "egress-b", ""
	egressA, egressB := outboundServiceName(labelA), outboundServiceName(labelB)
	podA := unlabelledFinalizerPod(&labelA, "uid-p", "10.244.0.7")
	podB := unlabelledFinalizerPod(&labelB, "uid-p", "10.244.0.7")
	podEmpty := unlabelledFinalizerPod(&empty, "uid-p", "10.244.0.7")
	kube := fake.NewSimpleClientset(podA.DeepCopy())
	dt := newTestDiffTracker()
	dt.kubeClient = kube
	dt.NRPResources.NATGateways.Insert(egressA)
	dt.K8sResources.Egresses.Insert(egressA)
	dt.K8sResources.Nodes["10.0.0.1"] = Node{Pods: map[string]Pod{
		"10.244.0.7": {InboundIdentities: utilsets.NewString(), PublicOutboundIdentity: egressA, OutboundPodKey: "ns/p", OutboundPodUID: "uid-p"},
	}}
	dt.outboundIdentityPodRefCount.Store(egressA, 1)
	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{
		"10.244.0.7": {Services: utilsets.NewString(egressA)},
	}}
	update := func(old, cur *v1.Pod) {
		_, err := kube.CoreV1().Pods("ns").Update(ctx, cur.DeepCopy(), metav1.UpdateOptions{})
		assert.NoError(t, err)
		dt.reconcileEgressPodUpdate(old, cur)
	}

	update(podA, podB)
	update(podB, podEmpty)
	dt.CheckPendingPodDeletions(ctx)

	got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer,
		"the finalizer must hold while NRP still maps the address to the first egress")

	dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{}}
	dt.CheckPendingPodDeletions(ctx)
	completeEgressCleanup(t, dt, egressA, egressB)
	got, err = kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer)
	assert.Contains(t, got.Finalizers, "example.com/keep")
}

func TestRuntimeUnlabelledPodFinalizerRelease_DrainsFromHandlers(t *testing.T) {
	ctx := context.Background()

	newDT := func(t *testing.T, pod *v1.Pod) (*DiffTracker, *fake.Clientset) {
		t.Helper()
		kube := fake.NewSimpleClientset(pod)
		dt := newTestDiffTracker()
		dt.kubeClient = kube
		dt.NRPResources.NATGateways.Insert("egress-a-natgw")
		dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{
			Addresses: map[string]NRPAddress{
				"10.244.0.7": {Services: utilsets.NewString("egress-a-natgw")},
			},
		}
		return dt, kube
	}
	assertDrainedThenReleased := func(t *testing.T, dt *DiffTracker, kube *fake.Clientset) {
		t.Helper()
		got, err := kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.Contains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer)
		assert.Contains(t, dt.pendingPodDeletions, "ns/p")

		dt.NRPResources.Locations["10.0.0.1"] = NRPLocation{Addresses: map[string]NRPAddress{}}
		dt.CheckPendingPodDeletions(ctx)
		completeEgressCleanup(t, dt, "egress-a-natgw")
		got, err = kube.CoreV1().Pods("ns").Get(ctx, "p", metav1.GetOptions{})
		assert.NoError(t, err)
		assert.NotContains(t, got.Finalizers, ServiceGatewayPodCleanupFinalizer)
	}

	t.Run("add with empty label and finalizer", func(t *testing.T) {
		empty := ""
		pod := unlabelledFinalizerPod(&empty, "uid-p", "10.244.0.7")
		dt, kube := newDT(t, pod)

		dt.podInformerAddPod(pod)

		assertDrainedThenReleased(t, dt, kube)
	})

	t.Run("unchanged empty update while deleting", func(t *testing.T) {
		empty := ""
		oldPod := unlabelledFinalizerPod(&empty, "uid-p", "10.244.0.7")
		newPod := oldPod.DeepCopy()
		now := metav1.Now()
		newPod.DeletionTimestamp = &now
		dt, kube := newDT(t, newPod)

		dt.reconcileEgressPodUpdate(oldPod, newPod)

		assertDrainedThenReleased(t, dt, kube)
	})

	t.Run("remove with empty label", func(t *testing.T) {
		empty := ""
		pod := unlabelledFinalizerPod(&empty, "uid-p", "10.244.0.7")
		dt, kube := newDT(t, pod)

		dt.podInformerRemovePod(pod)

		assertDrainedThenReleased(t, dt, kube)
	})

	t.Run("delete tombstone with empty label", func(t *testing.T) {
		empty := ""
		pod := unlabelledFinalizerPod(&empty, "uid-p", "10.244.0.7")
		dt, kube := newDT(t, pod)
		deleted, ok := podFromDeleteObj(cache.DeletedFinalStateUnknown{Obj: pod})
		assert.True(t, ok)

		dt.podInformerRemovePod(deleted)

		assertDrainedThenReleased(t, dt, kube)
	})
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
