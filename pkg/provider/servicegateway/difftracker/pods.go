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
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/netip"
	"slices"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// ResyncPeriod returns a function that generates a randomized resync duration
// to prevent controllers from syncing in lock-step and overloading the API server.
func ResyncPeriod(base time.Duration) func() time.Duration {
	return func() time.Duration {
		n, _ := rand.Int(rand.Reader, big.NewInt(1000))
		factor := float64(n.Int64())/1000.0 + 1.0
		return time.Duration(float64(base.Nanoseconds()) * factor)
	}
}

// setUpPodInformerForEgress creates an informer for Pods with egress labels.
// It uses label selectors to filter pods efficiently at the API server level,
// reducing memory and CPU overhead by only watching relevant pods.
// podFromDeleteObj decodes the object an informer DeleteFunc receives into a Pod.
//
// A delete event carries either the Pod itself or, when the watch missed the deletion, a
// DeletedFinalStateUnknown tombstone wrapping it. Dropping the tombstone case would silently
// ignore the deletion of an egress pod and strand its cleanup finalizer forever, so the decode is
// kept as a named function that can be exercised directly.
func podFromDeleteObj(obj interface{}) (*v1.Pod, bool) {
	switch v := obj.(type) {
	case *v1.Pod:
		return v, true
	case cache.DeletedFinalStateUnknown:
		pod, ok := v.Obj.(*v1.Pod)
		if !ok {
			klog.Errorf("Cannot convert to *v1.Pod: %T", v.Obj)
			return nil, false
		}
		klog.V(2).Infof("DeleteFunc: processing DeletedFinalStateUnknown for pod %s/%s",
			pod.Namespace, pod.Name)
		return pod, true
	default:
		klog.Errorf("Cannot convert to *v1.Pod: %T", v)
		return nil, false
	}
}

func (dt *DiffTracker) SetUpPodInformer(stopCh <-chan struct{}) error {
	klog.V(2).Infof("setUpPodInformerForEgress: Setting up pod informer with label selector: %s", consts.PodLabelServiceEgressGateway)

	// Create a separate informer factory with label selector to filter pods at the API server
	//
	// Resync is a safety net, not a correctness dependency: the watch should deliver every pod
	// event, and a resync only re-delivers current state as synthetic updates. It is kept short
	// deliberately so that recovery path is exercised routinely rather than a handful of times a
	// day, which is when a latent bug in it would otherwise surface. ResyncPeriod randomizes
	// x1.0-2.0 to avoid lock-step across controllers, so this is an effective 1-2h.
	podInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		dt.kubeClient,
		ResyncPeriod(time.Hour)(),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			// Only watch pods with the egress gateway label
			options.LabelSelector = consts.PodLabelServiceEgressGateway
		}),
	)

	podInformer := podInformerFactory.Core().V1().Pods().Informer()
	_, err := podInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				dt.podInformerAddPod(pod)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				oldPod := oldObj.(*v1.Pod)
				newPod := newObj.(*v1.Pod)
				dt.reconcileEgressPodUpdate(oldPod, newPod)
			},
			DeleteFunc: func(obj interface{}) {
				// Deletion is normally handled via UpdateFunc (2-phase deletion sets DeletionTimestamp
				// before the finalizer is removed). This is a defensive backup for
				// DeletedFinalStateUnknown and pods created during CCM downtime.
				pod, ok := podFromDeleteObj(obj)
				if !ok {
					return
				}
				// Idempotent - safe even if already processed in UpdateFunc.
				dt.podInformerRemovePod(pod)
			},
		})
	if err != nil {
		return fmt.Errorf("setUpPodInformerForEgress: add event handlers: %w", err)
	}

	// Start the informer factory
	klog.V(2).Infof("setUpPodInformerForEgress: Starting pod informer factory")
	podInformerFactory.Start(stopCh)
	if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced) {
		return fmt.Errorf("setUpPodInformerForEgress: cache sync stopped before completion")
	}
	klog.V(2).Infof("setUpPodInformerForEgress: Pod informer successfully initialized and synced")
	return nil
}

// reconcileEgressPodUpdate applies an egress pod UPDATE. A live re-registration (needsRemove AND
// needsAdd) drains via podInformerDrainForReplace (no finalizer record, so a concurrent stripper
// can't touch the still-live pod); a genuine removal (needsRemove only) uses podInformerRemovePod,
// which holds the finalizer until every address drains.
func (dt *DiffTracker) reconcileEgressPodUpdate(oldPod, newPod *v1.Pod) {
	needsRemove, needsAdd, reason := egressPodUpdateActions(oldPod, newPod)
	if !needsRemove && !needsAdd {
		klog.V(4).Infof("setUpPodInformerForEgress: Pod %s/%s update has no relevant changes, skipping",
			newPod.Namespace, newPod.Name)
		return
	}

	klog.V(2).Infof("setUpPodInformerForEgress: Pod %s/%s update: %s", newPod.Namespace, newPod.Name, reason)
	if needsRemove {
		if needsAdd {
			dt.podInformerDrainForReplace(oldPod, newPod)
		} else {
			dt.podInformerRemovePod(oldPod)
		}
	}
	if needsAdd {
		dt.podInformerAddPod(newPod)
	}
}

// egressPodUpdateActions decides whether an updated egress pod must be removed from its old gateway
// and/or (re-)added to its current one. Change-detection uses the full PodIPs set and the per-family
// node locations, so a dual-stack address or secondary-family location change is reconciled.
// DeletionTimestamp being set, or the Phase leaving Running/Pending, makes the pod invalid (removal).
func egressPodUpdateActions(oldPod, newPod *v1.Pod) (needsRemove, needsAdd bool, reason string) {
	var prevEgressGatewayName, currEgressGatewayName string
	if oldPod.Labels != nil {
		prevEgressGatewayName = strings.ToLower(oldPod.Labels[consts.PodLabelServiceEgressGateway])
	}
	if newPod.Labels != nil {
		currEgressGatewayName = strings.ToLower(newPod.Labels[consts.PodLabelServiceEgressGateway])
	}

	oldIPs := PodEgressAddresses(oldPod)
	newIPs := PodEgressAddresses(newPod)
	labelChanged := prevEgressGatewayName != currEgressGatewayName
	oldHadIPs := oldPod.Status.HostIP != "" && len(oldIPs) > 0
	newHasIPs := newPod.Status.HostIP != "" && len(newIPs) > 0
	// Re-register when the addresses OR the per-family node locations change: a secondary-family host
	// IP change (Status.HostIPs) while the primary HostIP is unchanged still moves that family's
	// address to a new location and must be re-synced, so compare the family->location map, not just
	// the primary HostIP.
	ipsChanged := !slices.Equal(oldIPs, newIPs) ||
		!maps.Equal(PodNodeLocationsByFamily(oldPod), PodNodeLocationsByFamily(newPod))

	oldWasValid := oldPod.DeletionTimestamp == nil &&
		(oldPod.Status.Phase == v1.PodRunning || oldPod.Status.Phase == v1.PodPending)
	newIsValid := newPod.DeletionTimestamp == nil &&
		(newPod.Status.Phase == v1.PodRunning || newPod.Status.Phase == v1.PodPending)

	switch {
	case labelChanged:
		// Remove from the old gateway (if it had IPs), add to the new one (if valid and has IPs).
		needsRemove = prevEgressGatewayName != "" && oldHadIPs
		needsAdd = currEgressGatewayName != "" && newHasIPs && newIsValid
		reason = fmt.Sprintf("label changed from %s to %s", prevEgressGatewayName, currEgressGatewayName)
	case currEgressGatewayName == "":
		// Not an egress pod and the label did not change; nothing to do.
	case oldWasValid && !newIsValid && oldHadIPs:
		needsRemove = true
		reason = fmt.Sprintf("pod became invalid (Phase: %s, DeletionTimestamp: %v)",
			newPod.Status.Phase, newPod.DeletionTimestamp != nil)
	case !oldHadIPs && newHasIPs && newIsValid:
		needsAdd = true
		reason = "completed IP initialization"
	case oldHadIPs && !newHasIPs:
		needsRemove = true
		reason = "lost IPs"
	case !oldWasValid && newIsValid && oldHadIPs && newHasIPs && !ipsChanged:
		// Pod regained validity (e.g. recovered from a transient node-NotReady Unknown phase) with the
		// same addresses; it was removed when it went invalid, so re-add the current set.
		needsAdd = true
		reason = "pod regained validity"
	case oldHadIPs && newHasIPs && ipsChanged && newIsValid:
		// Pod moved, or gained/lost/swapped an IP family - re-register the full address set.
		needsRemove = true
		needsAdd = true
		reason = fmt.Sprintf("IPs changed (HostIP: %s→%s, PodIPs: %v→%v)",
			oldPod.Status.HostIP, newPod.Status.HostIP, oldIPs, newIPs)
	case oldHadIPs && newHasIPs && !newIsValid:
		needsRemove = true
		reason = fmt.Sprintf("pod became invalid while having IPs (Phase: %s, DeletionTimestamp: %v)",
			newPod.Status.Phase, newPod.DeletionTimestamp != nil)
	}
	return needsRemove, needsAdd, reason
}

// podInformerAddPod handles pod addition events for egress.
// It validates the pod has the required egress label and IPs, then calls Engine.AddPod().
// The Engine handles all states:
// - Service doesn't exist → Engine creates NAT Gateway and buffers pod
// - Service being created → Engine buffers pod
// - Service ready → Engine adds pod immediately
func (dt *DiffTracker) podInformerAddPod(pod *v1.Pod) {
	// Validate pod has egress label (should always be true due to label selector, but check anyway)
	if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
		klog.V(4).Infof("podInformerAddPod: Pod %s/%s has no egress label, skipping", pod.Namespace, pod.Name)
		return
	}

	// A pod already marked for deletion (informer sync, or a delete delivered as an ADD) routes to
	// the delete handler.
	if pod.DeletionTimestamp != nil {
		klog.V(2).Infof("podInformerAddPod: Pod %s/%s is being deleted (DeletionTimestamp set), routing to delete handler",
			pod.Namespace, pod.Name)
		dt.podInformerRemovePod(pod)
		return
	}

	// Only process pods in Running or Pending phase
	if pod.Status.Phase != v1.PodRunning && pod.Status.Phase != v1.PodPending {
		klog.V(4).Infof("podInformerAddPod: Pod %s/%s is in phase %s (not Running/Pending), skipping",
			pod.Namespace, pod.Name, pod.Status.Phase)
		return
	}

	if pod.Status.HostIP == "" || len(PodEgressAddresses(pod)) == 0 {
		klog.V(4).Infof("podInformerAddPod: Pod %s/%s has egress label but no HostIP or PodIP yet, skipping",
			pod.Namespace, pod.Name)
		return
	}

	// Reject a malformed HostIP: it would produce a rejected ARM request and endless create retries.
	if _, err := netip.ParseAddr(pod.Status.HostIP); err != nil {
		klog.Warningf("podInformerAddPod: pod %s/%s has a malformed HostIP %q; skipping egress registration",
			pod.Namespace, pod.Name, pod.Status.HostIP)
		dt.recordEvent(pod, v1.EventTypeWarning, "ServiceGatewayInvalidPodIP",
			fmt.Sprintf("Malformed HostIP %q on the pod status", pod.Status.HostIP))
		return
	}

	// A dual-stack pod exposes one address per IP family in Status.PodIPs; register each so the
	// secondary family egresses through the NAT Gateway too. Skip individually malformed addresses.
	var podIPs []string
	for _, podIP := range PodEgressAddresses(pod) {
		if _, err := netip.ParseAddr(podIP); err != nil {
			klog.Warningf("podInformerAddPod: pod %s/%s has a malformed PodIP %q; skipping that address",
				pod.Namespace, pod.Name, podIP)
			dt.recordEvent(pod, v1.EventTypeWarning, "ServiceGatewayInvalidPodIP",
				fmt.Sprintf("Malformed PodIP %q on the pod status", podIP))
			continue
		}
		podIPs = append(podIPs, podIP)
	}
	if len(podIPs) == 0 {
		klog.V(4).Infof("podInformerAddPod: Pod %s/%s has no valid PodIP, skipping egress registration",
			pod.Namespace, pod.Name)
		return
	}

	egressName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
	if !IsValidEgressIdentity(egressName) {
		// The label becomes the NAT Gateway name/ARM resource ID; an invalid value would produce
		// malformed ARM requests (endless retries), and a reserved value would make this
		// controller manage a resource it does not own.
		reason, message := "ServiceGatewayInvalidEgressLabel",
			fmt.Sprintf("Invalid egress gateway label %q: must be a valid Azure resource name (alphanumerics, '-', '_', '.'; start alphanumeric; 1-%d chars)", egressName, maxEgressIdentityLength)
		if IsReservedEgressIdentity(egressName) {
			reason, message = "ServiceGatewayReservedEgressLabel",
				fmt.Sprintf("Egress gateway label %q is reserved for the cluster's default outbound gateway and cannot be used; choose a different value", egressName)
		}
		klog.Warningf("podInformerAddPod: pod %s/%s has an unusable egress gateway label %q; skipping egress registration",
			pod.Namespace, pod.Name, egressName)
		dt.recordEvent(pod, v1.EventTypeWarning, reason, message)
		return
	}
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	klog.V(2).Infof("podInformerAddPod: Pod %s added with egress %s (HostIP=%s, PodIPs=%v)",
		podKey, egressName, pod.Status.HostIP, podIPs)

	// Add the finalizer before registering. On sustained apiserver failure we still register the pod
	// (returning would kill its egress) but without the cleanup finalizer, losing NRP-drain
	// protection on a later delete; surface that via a metric + Event.
	if err := dt.AddPodFinalizer(context.Background(), pod); err != nil {
		if errors.Is(err, ErrPodGoneOrReplaced) {
			// The named pod is gone or already replaced by a same-name pod with a different UID.
			// Registering this stale event pod's address would add an unprotected NRP mapping (no
			// finalizer to drain it); the live replacement registers itself via its own Add event.
			klog.V(4).Infof("podInformerAddPod: pod %s gone or replaced; skipping stale egress registration", podKey)
			return
		}
		klog.Warningf("podInformerAddPod: registering egress pod %s WITHOUT cleanup finalizer after retries: %v", podKey, err)
		RecordPodFinalizerAddFailed()
		dt.recordEvent(pod, v1.EventTypeWarning, "ServiceGatewayFinalizerAddFailed",
			fmt.Sprintf("Failed to add ServiceGateway cleanup finalizer after retries; egress pod registered without NRP-drain protection: %v", err))
	}

	// Register each pod IP under its same-family node location (see PodNodeLocationsByFamily).
	hostByFamily := PodNodeLocationsByFamily(pod)
	for _, podIP := range podIPs {
		location, ok := NodeLocationForAddress(hostByFamily, podIP)
		if !ok {
			klog.Warningf("podInformerAddPod: pod %s/%s has no same-family node IP for PodIP %q (HostIPs=%v); skipping that address",
				pod.Namespace, pod.Name, podIP, pod.Status.HostIPs)
			dt.recordEvent(pod, v1.EventTypeWarning, "ServiceGatewayNoNodeLocation",
				fmt.Sprintf("No same-family node IP for pod address %q; the node must expose that IP family in status.hostIPs", podIP))
			continue
		}
		dt.AddPodWithUID(egressName, podKey, string(pod.UID), location, podIP)
	}
}

// podInformerRemovePod handles egress pod deletion. Engine.DeletePod drain-gates the finalizer: for
// a tracked pod it is removed only after every address drains from NRP (CheckPendingPodDeletions, or
// RemoveLastPodFinalizers after NAT Gateway deletion for the last pod). For an untracked pod or one
// with no IPs there is nothing to drain, so the finalizer is removed directly here.
func (dt *DiffTracker) podInformerRemovePod(pod *v1.Pod) {
	// Validate pod has egress label
	if pod.Labels == nil || pod.Labels[consts.PodLabelServiceEgressGateway] == "" {
		// An unlabelled pod carrying our cleanup finalizer is still ours to finish with. The informer
		// selects on the label key alone, so a pod whose value was emptied keeps matching and is
		// still delivered here, with nothing left to identify its egress service. Skipping it would
		// leave the finalizer attached with nothing able to remove it, blocking node drain and
		// namespace deletion.
		if hasFinalizer(pod.Finalizers, ServiceGatewayPodCleanupFinalizer) {
			klog.V(2).Infof("podInformerRemovePod: Pod %s/%s has no egress label but carries the cleanup finalizer; removing it directly",
				pod.Namespace, pod.Name)
			if err := dt.RemovePodFinalizerByPod(context.Background(), pod); err != nil {
				RecordPodFinalizerRemoveFailed()
				klog.Errorf("podInformerRemovePod: pod %s/%s could not have its cleanup finalizer removed, queued for retry: %v",
					pod.Namespace, pod.Name, err)
				dt.enqueuePodFinalizerRetry(pod, "")
			}
			return
		}
		klog.V(4).Infof("podInformerRemovePod: Pod %s/%s has no egress label, skipping", pod.Namespace, pod.Name)
		return
	}

	egressName := strings.ToLower(pod.Labels[consts.PodLabelServiceEgressGateway])
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	podIPs := PodEgressAddresses(pod)

	// No new IPs to drain. But if an earlier event already drain-gated this pod (the kubelet can
	// clear a terminating pod's PodIPs after we registered its address, so the delete event carries
	// none), the finalizer must stay until CheckPendingPodDeletions strips it once NRP confirms the
	// drain. Only strip inline when the engine has no pending drain - a genuinely untracked or
	// never-registered pod - to avoid stranding it.
	if pod.Status.HostIP == "" || len(podIPs) == 0 {
		result := dt.DeletePodWithoutAddresses(egressName, pod.Namespace, pod.Name, string(pod.UID))
		klog.V(2).Infof("podInformerRemovePod: Pod %s has no IPs; finalizer decision=%s (lastPod=%t, enqueued=%t)",
			podKey, result.FinalizerDecision, result.IsLastPod, result.Enqueued)
		if result.FinalizerDecision == PodFinalizerDecisionReleaseNoDrain {
			if err := dt.RemovePodFinalizerByPod(context.Background(), pod); err != nil {
				// The engine proved there is no drain, so queue a finalizer-only record: nothing
				// else re-drives this pod, because the informer only reacts to a state change and a
				// resync delivers an unchanged object.
				RecordPodFinalizerRemoveFailed()
				klog.Errorf("podInformerRemovePod: pod %s could not have its cleanup finalizer removed, queued for retry: %v", podKey, err)
				dt.enqueuePodFinalizerRetry(pod, egressName)
			}
		}
		return
	}

	klog.V(2).Infof("podInformerRemovePod: Pod %s removed from egress %s (HostIP=%s, PodIPs=%v)",
		podKey, egressName, pod.Status.HostIP, podIPs)

	// Atomically remove every address (a dual-stack pod has one per family); the single finalizer is
	// stripped only after all have drained from NRP, never inline.
	result := dt.DeletePod(egressName, pod.Status.HostIP, podIPs, pod.Namespace, pod.Name, string(pod.UID))

	klog.V(2).Infof("podInformerRemovePod: Pod %s finalizer decision=%s (lastPod=%t, enqueued=%t)",
		podKey, result.FinalizerDecision, result.IsLastPod, result.Enqueued)

	// Remove only after the engine explicitly proves that neither local nor NRP state needs a drain.
	// Do not infer release from !IsLastPod && !Enqueued: a local-state miss can still have an NRP
	// mapping, in which case DeletePod reconstructs pending drain tracking and holds the finalizer.
	if result.FinalizerDecision == PodFinalizerDecisionReleaseNoDrain {
		klog.V(2).Infof("podInformerRemovePod: Pod %s has no local or NRP drain; removing finalizer directly", podKey)
		if err := dt.RemovePodFinalizerByPod(context.Background(), pod); err != nil {
			RecordPodFinalizerRemoveFailed()
			klog.Errorf("podInformerRemovePod: pod %s could not have its cleanup finalizer removed, queued for retry: %v", podKey, err)
			dt.enqueuePodFinalizerRetry(pod, egressName)
		}
	}
}

// podInformerDrainForReplace drains the NRP addresses a still-live egress pod is LEAVING during an
// immediate re-registration (an IP change or egress-label move where podInformerAddPod re-adds it on
// the same event), without touching its cleanup finalizer.
//
// Empty namespace/name makes Engine.DeletePod drain but enqueue no finalizer record, so a concurrent
// stripper can't touch the still-live pod. Only addresses that won't stay at their current family
// location are drained; ones that stay put are kept so a sole pod's ref-count never dips to zero.
func (dt *DiffTracker) podInformerDrainForReplace(oldPod, newPod *v1.Pod) {
	if oldPod.Labels == nil || oldPod.Labels[consts.PodLabelServiceEgressGateway] == "" {
		return
	}

	oldEgress := strings.ToLower(oldPod.Labels[consts.PodLabelServiceEgressGateway])
	podKey := fmt.Sprintf("%s/%s", oldPod.Namespace, oldPod.Name)
	oldAddrs := PodEgressAddresses(oldPod)
	if oldPod.Status.HostIP == "" || len(oldAddrs) == 0 {
		return
	}

	var newEgress string
	if newPod.Labels != nil {
		newEgress = strings.ToLower(newPod.Labels[consts.PodLabelServiceEgressGateway])
	}

	// Drain each old address that will not stay at its current family node location (removed or
	// moved); keep the ones that stay put so a sole pod's ref-count does not dip to zero. A different
	// egress service drains the full old set.
	var toDrain []string
	if oldEgress != newEgress {
		toDrain = oldAddrs
	} else {
		oldLocs := PodNodeLocationsByFamily(oldPod)
		newLocs := PodNodeLocationsByFamily(newPod)
		newAddrs := PodEgressAddresses(newPod)
		for _, addr := range oldAddrs {
			oldLoc, _ := NodeLocationForAddress(oldLocs, addr)
			newLoc, _ := NodeLocationForAddress(newLocs, addr)
			if !slices.Contains(newAddrs, addr) || oldLoc != newLoc {
				toDrain = append(toDrain, addr)
			}
		}
	}
	if len(toDrain) == 0 {
		return
	}

	klog.V(2).Infof("podInformerDrainForReplace: draining egress %s addresses %v for live re-registration of pod %s (HostIP=%s)",
		oldEgress, toDrain, podKey, oldPod.Status.HostIP)

	// Empty namespace/name: drain from NRP but enqueue no finalizer record, so no stripper can act on
	// the still-live pod. The finalizer is re-ensured by the following AddPod.
	if oldEgress == newEgress {
		dt.DeletePodForReplacement(oldEgress, oldPod.Status.HostIP, toDrain, "", "", "")
		return
	}
	dt.DeletePod(oldEgress, oldPod.Status.HostIP, toDrain, "", "", "")
}
