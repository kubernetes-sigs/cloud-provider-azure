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
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/mock_azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/publicipaddressclient/mock_publicipaddressclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azclient/servicegatewayclient/mock_servicegatewayclient"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

// The RP-owned default NAT Gateway is absent from both NRP state and K8s egresses by construction,
// so the reserved-identity check is the only thing keeping orphan cleanup from deleting it on every
// start. Unlike load balancers there is no UUID backstop, because egress names are user-chosen.
func TestGuardDefaultNATGatewayIsNeverScheduledForOrphanDeletion(t *testing.T) {
	dt := newTestDiffTracker()

	// Exactly the startup shape: the default gateway is live in Azure, is not in NRP state
	// (excluded at parse time) and is not a K8s egress identity (rejected as a pod label).
	azureNATs := utilsets.NewString(DefaultOutboundNATGatewayName, "real-orphan-egress")

	scheduleOrphanedResourceDeletions(dt, utilsets.NewString(), azureNATs, utilsets.NewString())

	dt.mu.Lock()
	defer dt.mu.Unlock()

	_, defaultScheduled := dt.pendingServiceOps[DefaultOutboundNATGatewayName]
	assert.False(t, defaultScheduled,
		"BUG CASE: the RP-owned %q was scheduled for deletion; deleting it is a cluster-wide egress outage",
		DefaultOutboundNATGatewayName)

	_, orphanScheduled := dt.pendingServiceOps["real-orphan-egress"]
	assert.True(t, orphanScheduled,
		"CONTROL: a genuine unreferenced NAT Gateway must still be collected, otherwise this probe proves nothing")
}

// An orphaned NAT Gateway must be scheduled on the outbound path. The inbound path deletes a
// LoadBalancer that does not exist and the "<name>-pip" Public IP, but never the gateway itself, so
// the gateway leaks while its address is destroyed.
func TestGuardOrphanedNATGatewayUsesOutboundDeletionPath(t *testing.T) {
	dt := newTestDiffTracker()

	scheduleOrphanedResourceDeletions(dt,
		utilsets.NewString(),
		utilsets.NewString("orphan-egress"),
		utilsets.NewString())

	dt.mu.Lock()
	defer dt.mu.Unlock()

	opState, ok := dt.pendingServiceOps["orphan-egress"]
	if !assert.True(t, ok, "precondition: the orphaned NAT Gateway must be scheduled at all") {
		return
	}
	assert.False(t, opState.Config.IsInbound,
		"BUG CASE: orphaned NAT Gateway scheduled on the INBOUND path; deleteInboundService never deletes a NAT Gateway")

	// CONTROL: an orphaned LoadBalancer in the same call must be inbound.
	dt2 := newTestDiffTracker()
	const orphanLB = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	scheduleOrphanedResourceDeletions(dt2,
		utilsets.NewString(orphanLB),
		utilsets.NewString(),
		utilsets.NewString())
	dt2.mu.Lock()
	defer dt2.mu.Unlock()
	lbOp, ok := dt2.pendingServiceOps[orphanLB]
	if assert.True(t, ok, "control: an orphaned LoadBalancer must be scheduled") {
		assert.True(t, lbOp.Config.IsInbound, "control: an orphaned LoadBalancer must use the inbound path")
	}
}

// A NAT Gateway with live egress pods must never be torn down. handleEmptyOutboundServiceLocked
// guards on both buffered pods and the live pod ref-count; this pins the ref-count guard.
func TestGuardOutboundTeardownRespectsLivePodRefCount(t *testing.T) {
	dt := newTestDiffTracker()
	const svcUID = "egress-identity"

	dt.pendingServiceOps[svcUID] = &ServiceOperationState{
		ServiceUID: svcUID,
		Config:     NewOutboundServiceConfig(svcUID, nil),
		State:      StateNotStarted,
		RetryCount: 3, // a create that already reached Azure
	}
	// No buffered pods, but three LIVE pods are still registered against this identity.
	dt.outboundIdentityPodRefCount.Store(svcUID, 3)

	dt.mu.Lock()
	retained := dt.handleEmptyOutboundServiceLocked(svcUID)
	_, scheduled := dt.pendingServiceDeletions[svcUID]
	state := dt.pendingServiceOps[svcUID].State
	dt.mu.Unlock()

	assert.False(t, scheduled,
		"BUG CASE: NAT Gateway scheduled for deletion while 3 live pods still use it for egress")
	assert.Equal(t, StateNotStarted, state, "BUG CASE: operation flipped to deletion with live pods present")
	assert.False(t, retained, "with live pods the helper must report no teardown in flight")

	// CONTROL: with the ref-count at zero the same call MUST schedule the teardown, so the probe
	// is not passing because handleEmptyOutboundServiceLocked never does anything.
	dt2 := newTestDiffTracker()
	dt2.pendingServiceOps[svcUID] = &ServiceOperationState{
		ServiceUID: svcUID,
		Config:     NewOutboundServiceConfig(svcUID, nil),
		State:      StateNotStarted,
		RetryCount: 3,
	}
	dt2.outboundIdentityPodRefCount.Store(svcUID, 0)
	dt2.mu.Lock()
	retained2 := dt2.handleEmptyOutboundServiceLocked(svcUID)
	_, scheduled2 := dt2.pendingServiceDeletions[svcUID]
	dt2.mu.Unlock()
	assert.True(t, scheduled2, "CONTROL: with no live pods the failed create must be torn down")
	assert.True(t, retained2, "CONTROL: teardown in flight must retain the pod finalizer")
}

// RemoveLastPodFinalizers must only touch the service it was called for.
func TestGuardRemoveLastPodFinalizersIsScopedToItsService(t *testing.T) {
	pod := func(name, uid string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace:  "ns",
			Name:       name,
			UID:        types.UID(uid),
			Finalizers: []string{ServiceGatewayPodCleanupFinalizer},
		}}
	}
	kubeClient := fake.NewSimpleClientset(pod("pod-a", "uid-a"), pod("pod-b", "uid-b"))

	dt := newTestDiffTracker()
	dt.kubeClient = kubeClient
	dt.pendingPodDeletions["ns/pod-a"] = &PendingPodDeletion{
		Namespace: "ns", Name: "pod-a", UID: "uid-a", ServiceUID: "egress-a", IsLastPod: true,
	}
	dt.pendingPodDeletions["ns/pod-b"] = &PendingPodDeletion{
		Namespace: "ns", Name: "pod-b", UID: "uid-b", ServiceUID: "egress-b", IsLastPod: true,
	}

	// egress-b's NAT Gateway has NOT been deleted, so pod-b must keep its finalizer.
	assert.NoError(t, dt.RemoveLastPodFinalizers(context.Background(), "egress-a"))

	got := func(name string) []string {
		p, err := kubeClient.CoreV1().Pods("ns").Get(context.Background(), name, metav1.GetOptions{})
		assert.NoError(t, err)
		return p.Finalizers
	}
	assert.Empty(t, got("pod-a"), "CONTROL: the requested service's last pod must be released")
	assert.Contains(t, got("pod-b"), ServiceGatewayPodCleanupFinalizer,
		"BUG CASE: RemoveLastPodFinalizers(egress-a) released egress-b's last pod, whose NAT Gateway still exists")

	dt.mu.Lock()
	_, bStillPending := dt.pendingPodDeletions["ns/pod-b"]
	dt.mu.Unlock()
	assert.True(t, bStillPending, "BUG CASE: egress-b's pending last-pod entry was consumed by another service's teardown")
}

// The default gateway's Public IP must survive the orphan sweep, whichever guard rejects it.
func TestGuardDefaultNATGatewayPIPSurvivesOrphanSweep(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()

	// The sweep deletes in parallel through a worker pool, so the recorder must be locked.
	var deletedMu sync.Mutex
	var deleted []string
	mockPIP.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, name string) error {
			deletedMu.Lock()
			defer deletedMu.Unlock()
			deleted = append(deleted, name)
			return nil
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.networkClientFactory = mockFactory

	detached := func(name string) *armnetwork.PublicIPAddress {
		return &armnetwork.PublicIPAddress{Name: ptr.To(name), Properties: &armnetwork.PublicIPAddressPropertiesFormat{}}
	}
	const managedOrphan = "33333333-3333-3333-3333-333333333333-pip"
	assert.NoError(t, dt.cleanupOrphanedPublicIPs(context.Background(), []*armnetwork.PublicIPAddress{
		detached(PublicIPName(DefaultOutboundNATGatewayName)),
		detached(managedOrphan),
	}))

	assert.NotContains(t, deleted, PublicIPName(DefaultOutboundNATGatewayName),
		"BUG CASE: the cluster's default egress Public IP was deleted by the orphan sweeper")
	assert.Contains(t, deleted, managedOrphan,
		"CONTROL: a genuine managed orphan must still be swept, otherwise this probe proves nothing")
}

// Orphan cleanup must never delete a resource Kubernetes still wants.
func TestGuardOrphanCleanupSkipsResourcesStillDesiredInKubernetes(t *testing.T) {
	const desiredLB = "11111111-1111-1111-1111-111111111111"
	const orphanLB = "22222222-2222-2222-2222-222222222222"

	dt := newTestDiffTracker()
	// Crash-mid-create shape: live in Azure and desired in K8s, but SGW registration never ran.
	dt.K8sResources.Services.Insert(desiredLB)
	dt.K8sResources.Egresses.Insert("desired-egress")

	scheduleOrphanedResourceDeletions(dt,
		utilsets.NewString(desiredLB, orphanLB),
		utilsets.NewString("desired-egress", "orphan-egress"),
		utilsets.NewString())

	dt.mu.Lock()
	defer dt.mu.Unlock()

	_, lbScheduled := dt.pendingServiceOps[desiredLB]
	assert.False(t, lbScheduled,
		"BUG CASE: a LoadBalancer Kubernetes still wants was scheduled for orphan deletion; isOrphan=true skips every existence check")
	_, natScheduled := dt.pendingServiceOps["desired-egress"]
	assert.False(t, natScheduled,
		"BUG CASE: a NAT Gateway Kubernetes still wants was scheduled for orphan deletion")

	_, orphanLBScheduled := dt.pendingServiceOps[orphanLB]
	assert.True(t, orphanLBScheduled, "CONTROL: a genuinely undesired LoadBalancer must still be collected")
	_, orphanNATScheduled := dt.pendingServiceOps["orphan-egress"]
	assert.True(t, orphanNATScheduled, "CONTROL: a genuinely undesired NAT Gateway must still be collected")
}

// The RP-owned default outbound NAT Gateway must stay out of the startup NRP snapshot.
func TestGuardDefaultNATGatewayNeverEntersNRPSnapshotOrRemovalDiff(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	svc := func(name string, t armnetwork.ServiceType) *armnetwork.ServiceGatewayService {
		return &armnetwork.ServiceGatewayService{
			Name:       ptr.To(name),
			Properties: &armnetwork.ServiceGatewayServicePropertiesFormat{ServiceType: ptr.To(t)},
		}
	}
	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockSGW := mock_servicegatewayclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetServiceGatewayClient().Return(mockSGW).AnyTimes()
	mockSGW.EXPECT().GetServices(gomock.Any(), gomock.Any(), gomock.Any()).Return(
		[]*armnetwork.ServiceGatewayService{
			svc(DefaultOutboundNATGatewayName, armnetwork.ServiceTypeOutbound),
			svc("team-egress", armnetwork.ServiceTypeOutbound),
		}, nil).AnyTimes()

	nrp := &NRPState{LoadBalancers: utilsets.NewString(), NATGateways: utilsets.NewString()}
	assert.NoError(t, fetchServiceGatewayServices(context.Background(), testConfig(), mockFactory, nrp))

	assert.False(t, nrp.NATGateways.Has(DefaultOutboundNATGatewayName),
		"BUG CASE: the RP-owned default outbound gateway entered the NRP snapshot")
	assert.True(t, nrp.NATGateways.Has("team-egress"),
		"CONTROL: a managed outbound service must still be tracked")

	// Second half: prove the consequence. Feed the snapshot into the diff with an empty K8s egress
	// set (the real startup shape, since no pod may carry the reserved label) and assert the
	// default gateway is not marked for removal.
	dt := newTestDiffTracker()
	dt.NRPResources.NATGateways = nrp.NATGateways
	removals := dt.GetSyncNRPNATGateways().Removals.UnsortedList()
	assert.NotContains(t, removals, DefaultOutboundNATGatewayName,
		"BUG CASE: the diff marked the RP-owned default NAT Gateway for removal")
	assert.Contains(t, removals, "team-egress",
		"CONTROL: an outbound service K8s no longer wants must still be marked for removal")
}

// A transient Service LOOKUP failure during deletion must not report success.
func TestGuardDeleteInboundServiceRetriesOnTransientServiceLookupFailure(t *testing.T) {
	run := func(t *testing.T, getErr error) (reported *bool, tracked bool) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		kube := fake.NewSimpleClientset(deletionTestService())
		if getErr != nil {
			// getServiceByUID resolves the Service by listing/getting; fail every read.
			fail := func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, getErr }
			kube.PrependReactor("get", "services", fail)
			kube.PrependReactor("list", "services", fail)
		}

		dt := deletionTestDiffTracker(kube, deletionTestFactory(ctrl))
		su := deletionTestUpdater(dt, func(uid string, ok bool, err error) {
			v := ok
			reported = &v
			dt.OnServiceCreationComplete(uid, ok, err)
		})
		su.deleteInboundService("uid-1", "corr-1")
		_, tracked = dt.pendingServiceOps["uid-1"]
		return reported, tracked
	}

	t.Run("BUG CASE: transient 500 on the Service lookup", func(t *testing.T) {
		reported, tracked := run(t, apierrors.NewInternalError(errors.New("etcdserver: request timed out")))
		if assert.NotNil(t, reported, "onComplete must be called") {
			assert.False(t, *reported,
				"BUG CASE: deletion reported SUCCESS after a transient Service lookup failure; the finalizer was never removed and nothing re-drives it")
		}
		assert.True(t, tracked,
			"BUG CASE: tracking was cleared, so a retried DeleteService is a no-op and the Service strands Terminating")
	})

	t.Run("CONTROL: the Service is genuinely gone", func(t *testing.T) {
		reported, tracked := run(t, apierrors.NewNotFound(corev1.Resource("services"), "svc"))
		if assert.NotNil(t, reported) {
			assert.True(t, *reported, "CONTROL: a NotFound Service means nothing is left to finalize; the delete succeeds")
		}
		assert.False(t, tracked, "CONTROL: tracking is cleared on a genuine success")
	})
}

// An egress identity's Public IPs must be swept once its NAT Gateway is gone. The identity comes
// from a user-chosen pod label rather than a UUID, and the sweeper used to delete only UUID-named
// addresses, so the egress address leaked forever. The IPv6 address is covered too:
// "<identity>-pip-v6" does not end in "-pip", so it was rejected as an unknown name.
func TestGuardEgressPublicIPsAreSweptOnceNATGatewayIsGone(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockFactory := mock_azclient.NewMockClientFactory(ctrl)
	mockPIP := mock_publicipaddressclient.NewMockInterface(ctrl)
	mockFactory.EXPECT().GetPublicIPAddressClient().Return(mockPIP).AnyTimes()

	// The sweep deletes in parallel through a worker pool, so the recorder must be locked.
	var deletedMu sync.Mutex
	var deleted []string
	mockPIP.EXPECT().Delete(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, name string) error {
			deletedMu.Lock()
			defer deletedMu.Unlock()
			deleted = append(deleted, name)
			return nil
		}).AnyTimes()

	dt := newTestDiffTracker()
	dt.config = testConfig()
	dt.networkClientFactory = mockFactory

	pip := func(name string) *armnetwork.PublicIPAddress {
		return &armnetwork.PublicIPAddress{Name: ptr.To(name), Properties: &armnetwork.PublicIPAddressPropertiesFormat{}}
	}

	const egress = "team-egress"
	const desired = "still-wanted-egress"
	dt.K8sResources.Egresses = newIgnoreCaseSetFromSlice([]string{desired})

	attached := pip(PublicIPName("still-in-use"))
	attached.Properties.NatGateway = &armnetwork.NatGateway{ID: ptr.To("/natGateways/still-in-use")}

	assert.NoError(t, dt.cleanupOrphanedPublicIPs(context.Background(), []*armnetwork.PublicIPAddress{
		pip(PublicIPName(egress)),
		pip(PublicIPNameV6(egress)),
		pip(PublicIPName(desired)),
		pip(PublicIPName(DefaultOutboundNATGatewayName)),
		attached,
	}))

	assert.Contains(t, deleted, PublicIPName(egress),
		"BUG CASE: the egress IPv4 Public IP leaked because its name is not a UUID")
	assert.Contains(t, deleted, PublicIPNameV6(egress),
		"BUG CASE: the egress IPv6 Public IP leaked because \"-pip-v6\" was treated as an unknown name")
	assert.NotContains(t, deleted, PublicIPName(desired),
		"BUG CASE: an address for an egress identity Kubernetes still wants was deleted")
	assert.NotContains(t, deleted, PublicIPName(DefaultOutboundNATGatewayName),
		"BUG CASE: the cluster's default egress Public IP was deleted")
	assert.NotContains(t, deleted, PublicIPName("still-in-use"),
		"BUG CASE: a Public IP still attached to a NAT Gateway was scheduled for deletion")
}
