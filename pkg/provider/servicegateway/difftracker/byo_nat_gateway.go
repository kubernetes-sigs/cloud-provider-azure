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

// Bring-your-own (BYO) NAT Gateways for egress identities.
//
// A pod selects an egress identity with the egress label; the label value names the identity. The
// gateway behind an identity is decided once, when the identity is created, from the annotation
// consts.PodAnnotationServiceEgressGatewayNATGatewayID of the first pod that is still a member of the
// identity (it exists, is not being deleted and carries the identity's label): the pod that triggered
// the creation, then the pods buffered for the identity, then its pods found at start-up. The
// identity's events are recorded on that pod.
//
//  1. Without the annotation: a NAT Gateway named after the identity in the cluster resource group,
//     managed by this controller. One this controller did not create (such as the default outbound
//     service's) is never taken over.
//  2. With the annotation: the NAT Gateway it names, in any resource group of the cluster's network
//     subscription other than the cluster resource group, with any name: a BYO NAT Gateway, owned by
//     the user. One that cannot be used rejects the identity (egressRejectedError): the creation is
//     retried with backoff and does not hold the controller's initial sync open; it never falls back
//     to a managed NAT Gateway. Until then the pods keep the default outbound egress.
//
// The annotation is read at every attempt until the identity's gateway may carry this controller's
// link (it was linked, or read as linked to this Service Gateway); from then on the identity keeps that
// gateway until the identity is deleted.
//
// A BYO NAT Gateway and its Public IPs are never created or deleted; the only property changed is the
// gateway's link to the Service Gateway (properties.serviceGateway), set when the identity is created
// and cleared when it is deleted. Which identities use a BYO gateway is rebuilt on
// start-up from the Service Gateway's services whose NAT Gateway the annotation could name.
// Link writes require the ETag of the read resource. Per-gateway claims serialize this controller's
// identities through creation and deletion; Azure's services list also protects existing users.
//
// Known gaps: if the controller stops between linking a BYO NAT Gateway and registering the identity,
// or between clearing the identity's registration and unlinking the gateway, and all the identity's
// pods are deleted before it restarts, the link is left behind (nothing records it). The gateway is
// picked up again if the identity is used again; otherwise its owner clears properties.serviceGateway.
// Scanning for such links at start-up is avoided because it would race the AKS RP, which links a BYO
// default NAT Gateway before registering it, and because an owner may link a gateway before its pods
// use it.
// A partial managed creation switched to BYO can leave managed resources until the identity is gone
// and startup orphan cleanup runs. An existing registration whose gateway was deleted by its owner
// is not repaired automatically, including at restart when its NAT Gateway reference is empty.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

// Event reasons emitted on the member pod whose annotation decided an egress identity's NAT Gateway.
const (
	egressNATGatewayLinkedReason   = "ServiceGatewayEgressNATGatewayLinked"
	egressNATGatewayRejectedReason = "ServiceGatewayEgressNATGatewayRejected"
)

// natGatewayResourceType is the ARM resource type of a NAT Gateway.
const natGatewayResourceType = "Microsoft.Network/natGateways"

// egressRejectedError marks an egress admission rejection: the resources cannot currently be used by this
// identity. It is retried with backoff like any failure, but
// does not hold the controller's initial sync open (see checkInitializationCompleteLocked).
type egressRejectedError struct{ error }

var errNoEgressMember = errors.New("no active member pod")

func (e egressRejectedError) Unwrap() error { return e.error }

// isEgressRejected reports whether err is (or wraps) an egressRejectedError.
func isEgressRejected(err error) bool {
	var rejected egressRejectedError
	return errors.As(err, &rejected)
}

// resolveBYONATGateway returns the BYO NAT Gateway the pods of an egress identity name, or nil when
// the identity uses a NAT Gateway managed by this controller, and the member pod the identity's events
// are recorded on (nil when there is no Kubernetes client). A named NAT Gateway that cannot be used
// rejects the identity with a Warning event and an egressRejectedError, so the creation is retried with
// backoff (reading the annotation again unless a recorded gateway may carry this controller's link); it
// is never replaced by a managed NAT Gateway.
func (s *ServiceUpdater) resolveBYONATGateway(ctx context.Context, identity, podNS, podName string) (*armnetwork.NatGateway, *v1.Pod, error) {
	dt := s.diffTracker
	pod, err := dt.egressMemberPod(ctx, identity, podNS, podName)
	if err != nil {
		return nil, nil, err
	}
	reject := func(err error) (*armnetwork.NatGateway, *v1.Pod, error) {
		dt.recordEgressEvent(pod, v1.EventTypeWarning, egressNATGatewayRejectedReason,
			fmt.Sprintf("Egress identity %q cannot be used: %v.", identity, err))
		return nil, pod, egressRejectedError{fmt.Errorf("egress identity %q: %w", identity, err)}
	}
	// readFailed rejects a NAT Gateway the cluster identity may not read; any other read error is retried.
	readFailed := func(natGatewayID string, err error) (*armnetwork.NatGateway, *v1.Pod, error) {
		if httpStatus, _ := extractAzureErrorInfo(err); httpStatus == http.StatusForbidden {
			return reject(fmt.Errorf("access denied when reading NAT Gateway %q; grant the cluster identity read and write access to it", natGatewayID))
		}
		return nil, pod, err
	}
	if dt.isReservedOutboundService(identity) {
		return reject(errNATGatewayNotOwned)
	}
	if linked := dt.byoNATGatewayID(identity); linked != "" {
		// Recorded while the gateway may carry this controller's link, so it is used until the identity
		// is deleted (and then unlinked) rather than replaced by what the annotation names now.
		natGateway, exists, err := dt.getNatGatewayByID(ctx, linked)
		if err != nil {
			return readFailed(linked, err)
		}
		if exists {
			return natGateway, pod, nil
		}
		// Its owner deleted it; resolve afresh.
		dt.setBYONATGatewayID(identity, "")
	}

	natGatewayID := ""
	if pod != nil {
		natGatewayID = strings.TrimSpace(pod.Annotations[consts.PodAnnotationServiceEgressGatewayNATGatewayID])
	}
	if natGatewayID == "" {
		if _, err := dt.clusterNATGatewayExists(ctx, identity); err != nil {
			if errors.Is(err, errNATGatewayNotOwned) {
				return reject(err)
			}
			return nil, pod, err
		}
		return nil, pod, nil
	}
	if problem := byoNATGatewayIDProblem(natGatewayID, dt.config); problem != "" {
		return reject(errors.New(problem))
	}
	natGateway, exists, err := dt.getNatGatewayByID(ctx, natGatewayID)
	if err != nil {
		return readFailed(natGatewayID, err)
	}
	if !exists {
		return reject(fmt.Errorf("NAT Gateway %q does not exist", natGatewayID))
	}
	return natGateway, pod, nil
}

// egressMemberPod returns the first pod that is still a member of the egress identity: the pod that
// triggered the creation, then the pods buffered for the identity, then its pods in the Kubernetes
// state (those found at start-up), sorted. Pods are read from the API server, so an annotation fixed in
// place is used on the next attempt, also before the pod informer starts. Returns nil when there is no
// Kubernetes client, and an error when no member exists or a pod cannot be read.
func (dt *DiffTracker) egressMemberPod(ctx context.Context, identity, podNS, podName string) (*v1.Pod, error) {
	if dt.kubeClient == nil {
		return nil, nil
	}
	trigger := podNS + "/" + podName
	if podName != "" {
		// Most attempts end here, without scanning the identity's other pods under dt.mu.
		if pod, err := dt.readEgressMember(ctx, identity, podNS, podName); pod != nil || err != nil {
			return pod, err
		}
	}
	for _, key := range dt.egressPodKeysOf(identity) {
		namespace, name, _ := strings.Cut(key, "/")
		if name == "" || key == trigger {
			continue
		}
		if pod, err := dt.readEgressMember(ctx, identity, namespace, name); pod != nil || err != nil {
			return pod, err
		}
	}
	return nil, fmt.Errorf("%w: %q", errNoEgressMember, identity)
}

// readEgressMember reads a pod and returns it while it is a member of the egress identity: it exists,
// is not being deleted and still carries the identity's label. A pod relabelled to another identity
// keeps no say in this one's NAT Gateway.
func (dt *DiffTracker) readEgressMember(ctx context.Context, identity, namespace, name string) (*v1.Pod, error) {
	pod, err := dt.kubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading pod %s/%s of egress identity %q: %w", namespace, name, identity, err)
	}
	if pod.DeletionTimestamp != nil || !strings.EqualFold(pod.Labels[consts.PodLabelServiceEgressGateway], identity) {
		return nil, nil
	}
	return pod, nil
}

// egressPodKeysOf returns the namespace/name keys of the pods buffered for an egress identity, in
// arrival order, then of its pods in the Kubernetes state, sorted, without repeats. Only the copy is
// made under dt.mu.
func (dt *DiffTracker) egressPodKeysOf(identity string) []string {
	var pending, known []string
	dt.mu.Lock()
	for _, p := range dt.pendingPods[identity] {
		pending = append(pending, p.PodKey)
	}
	for _, node := range dt.K8sResources.Nodes {
		for _, pod := range node.Pods {
			if strings.EqualFold(pod.PublicOutboundIdentity, identity) {
				known = append(known, pod.OutboundPodKey)
			}
		}
	}
	dt.mu.Unlock()
	slices.Sort(known)
	seen := make(map[string]bool, len(pending)+len(known))
	keys := make([]string, 0, len(pending)+len(known))
	for _, key := range append(pending, known...) {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}

// recordEgressEvent records an event on the member pod of an egress identity, if one is known.
func (dt *DiffTracker) recordEgressEvent(pod *v1.Pod, eventType, reason, message string) {
	if pod != nil {
		dt.recordEvent(pod, eventType, reason, message)
	}
}

// byoNATGatewayIDProblem returns why id cannot name the BYO NAT Gateway of an egress identity, or "".
// A BYO NAT Gateway is in the cluster's network subscription and outside the cluster resource group,
// which belongs to AKS and holds the NAT Gateways this controller manages.
func byoNATGatewayIDProblem(id string, config Config) string {
	parsed, err := arm.ParseResourceID(id)
	switch {
	case err != nil:
		return fmt.Sprintf("%s %q is not an Azure resource ID", consts.PodAnnotationServiceEgressGatewayNATGatewayID, id)
	case !strings.EqualFold(parsed.ResourceType.String(), natGatewayResourceType) || parsed.Parent == nil ||
		!strings.EqualFold(parsed.Parent.ResourceType.String(), arm.ResourceGroupResourceType.String()):
		return fmt.Sprintf("%s %q is not the ID of a NAT Gateway in a resource group (%s)", consts.PodAnnotationServiceEgressGatewayNATGatewayID, id, natGatewayResourceType)
	case !strings.EqualFold(parsed.SubscriptionID, config.networkResourceSubscriptionID()):
		return fmt.Sprintf("NAT Gateway %q is not in the cluster's network subscription %q", id, config.networkResourceSubscriptionID())
	case strings.EqualFold(parsed.ResourceGroupName, config.ResourceGroup):
		return fmt.Sprintf("NAT Gateway %q is in the cluster resource group %q, which only holds NAT Gateways this controller manages", id, config.ResourceGroup)
	}
	return ""
}

// createBYOOutboundService validates a BYO NAT Gateway, links it to the Service Gateway
// and registers the egress identity with it. Events are recorded on pod, the member pod whose
// annotation chose the gateway. Failures are retried with backoff, so a gateway the owner fixes (or
// grants access to) is picked up without recreating the pods.
func (s *ServiceUpdater) createBYOOutboundService(ctx context.Context, identity string, natGateway *armnetwork.NatGateway,
	correlationID string, pod *v1.Pod) {
	dt := s.diffTracker
	natGatewayID := derefString(natGateway.ID)
	serviceGatewayID := dt.config.ServiceGatewayResourceID()
	parsed, err := arm.ParseResourceID(natGatewayID)
	if err != nil {
		s.onComplete(identity, false, fmt.Errorf("parsing NAT Gateway ID %q: %w", natGatewayID, err))
		return
	}
	freshClaim, claimant := dt.claimBYONATGateway(identity, natGatewayID)
	if claimant != "" {
		problem := fmt.Sprintf("it is already used by Service Gateway service %q", claimant)
		dt.recordEgressEvent(pod, v1.EventTypeWarning, egressNATGatewayRejectedReason,
			fmt.Sprintf("Egress identity %q cannot use NAT Gateway %q: %s.", identity, natGatewayID, problem))
		s.onComplete(identity, false, egressRejectedError{fmt.Errorf("BYO NAT Gateway %q: %s", natGatewayID, problem)})
		return
	}
	// Release a fresh claim on pre-write failure unless the gateway already carried this SGW's link.
	linkedHere := strings.EqualFold(natGatewayServiceGatewayID(natGateway), serviceGatewayID)
	retainClaim := !freshClaim || linkedHere
	defer func() {
		if !retainClaim {
			dt.setBYONATGatewayID(identity, "")
		}
	}()

	reject := func(problem string) {
		if natGateway.Properties != nil && !linkedHere {
			// Missing properties cannot prove an existing link is gone.
			dt.setBYONATGatewayID(identity, "")
		}
		dt.recordEgressEvent(pod, v1.EventTypeWarning, egressNATGatewayRejectedReason,
			fmt.Sprintf("Egress identity %q cannot use NAT Gateway %q: %s.", identity, natGatewayID, problem))
		s.onComplete(identity, false, egressRejectedError{fmt.Errorf("BYO NAT Gateway %q: %s", natGatewayID, problem)})
	}

	if problem := byoNATGatewayProblem(natGateway, serviceGatewayID); problem != "" {
		reject(problem)
		return
	}
	user, err := dt.otherServiceUsing(ctx, identity, natGatewayID)
	if err != nil {
		s.onComplete(identity, false, err)
		return
	}
	if user != "" {
		reject(fmt.Sprintf("it is already used by Service Gateway service %q", user))
		return
	}
	// A cleanup reservation is not proof of a successful link; every creation attempt reasserts it.
	linkCtx, err := withNATGatewayETag(ctx, natGateway.Etag)
	if err != nil {
		s.onComplete(identity, false, err)
		return
	}

	// Keep the claim once a write may have applied, including an ambiguous failure.
	retainClaim = true
	natGateway.Properties.ServiceGateway = &armnetwork.SubResource{ID: to.Ptr(serviceGatewayID)}
	if err := dt.createOrUpdateNatGateway(linkCtx, parsed.ResourceGroupName, *natGateway); err != nil {
		s.logger.V(4).Info("Could not link BYO NAT Gateway to Service Gateway", "serviceUID", identity, "correlationID", correlationID, "natGateway", natGatewayID, "err", err)
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && isInitialNATPutResponse(respErr) &&
			respErr.StatusCode == http.StatusPreconditionFailed && freshClaim && !linkedHere {
			retainClaim = false
		}
		if errors.As(err, &respErr) && isLinkRefusal(respErr) {
			problem := fmt.Sprintf("Azure refused to link it to the Service Gateway (HTTP %d %s)", respErr.StatusCode, respErr.ErrorCode)
			if respErr.StatusCode == http.StatusForbidden {
				problem = "access denied when linking it to the Service Gateway; grant the cluster identity write access to the NAT Gateway"
			}
			reject(problem)
			return
		}
		// The write may still have been applied, so the record is kept for the deletion to unlink it.
		s.onComplete(identity, false, fmt.Errorf("failed to link NAT Gateway: %w", err))
		return
	}

	if err := dt.updateNRPSGWServices(ctx, dt.config.ServiceGatewayResourceName, byoOutboundServicesDTO(identity, natGatewayID, dt.config)); err != nil {
		s.logger.V(4).Info("Could not register outbound service with ServiceGateway", "serviceUID", identity, "correlationID", correlationID, "err", err)
		s.onComplete(identity, false, fmt.Errorf("failed to register with ServiceGateway: %w", err))
		return
	}

	dt.UpdateNRPNATGateways(SyncServicesReturnType{Additions: newIgnoreCaseSetFromSlice([]string{identity})})
	dt.recordEgressEvent(pod, v1.EventTypeNormal, egressNATGatewayLinkedReason,
		fmt.Sprintf("Egress identity %q uses BYO NAT Gateway %q.", identity, natGatewayID))
	s.onComplete(identity, true, nil)
	s.logger.V(2).Info("Created outbound service with BYO NAT Gateway", "serviceUID", identity, "natGateway", natGatewayID, "correlationID", correlationID)
}

// deleteBYOOutboundService unlinks a BYO NAT Gateway and unregisters the egress identity.
// The gateway and its Public IPs are left in place. The identity is unregistered once the gateway is
// unlinked, so a failed unlink is retried; access denied or a resource lock is logged and counted
// instead, and the link is left for the gateway's owner to clear.
func (s *ServiceUpdater) deleteBYOOutboundService(ctx context.Context, identity, natGatewayID, correlationID string) {
	dt := s.diffTracker
	if parsed, err := arm.ParseResourceID(natGatewayID); err == nil {
		if err := dt.unlinkNatGateway(ctx, dt.config.ServiceGatewayResourceName, identity, parsed.ResourceGroupName, parsed.Name); err != nil {
			// Access denied or a resource lock will not clear by retrying, and retrying would hold
			// the last egress pod in Terminating. The leftover link only affects the BYO gateway.
			if !isAccessDeniedOrLocked(err) {
				s.logger.V(4).Info("Could not unlink BYO NAT Gateway", "serviceUID", identity, "correlationID", correlationID, "natGateway", natGatewayID, "err", err)
				s.onComplete(identity, false, fmt.Errorf("failed to unlink NAT Gateway: %w", err))
				return
			}
			recordDeleteSubstepFailure(deleteStepDisassociateNAT)
			s.logger.Error(err, "Left the BYO NAT Gateway linked to the Service Gateway; remove the link manually", "serviceUID", identity, "natGateway", natGatewayID)
		}
	}

	if err := dt.updateNRPSGWServices(ctx, dt.config.ServiceGatewayResourceName, buildServiceGatewayRemovalDTO(identity, false, dt.config)); err != nil {
		var respErr *azcore.ResponseError
		if !errors.As(err, &respErr) || respErr.StatusCode != http.StatusNotFound {
			s.logger.V(4).Info("Could not unregister outbound service from ServiceGateway", "serviceUID", identity, "correlationID", correlationID, "err", err)
			s.onComplete(identity, false, fmt.Errorf("failed to unregister from ServiceGateway: %w", err))
			return
		}
	}

	s.finishOutboundDeletion(ctx, identity, correlationID, nil)
}

// otherServiceUsing returns the Service Gateway service other than identity that uses natGatewayID,
// or "" (also when the Service Gateway is gone).
func (dt *DiffTracker) otherServiceUsing(ctx context.Context, identity, natGatewayID string) (string, error) {
	services, err := dt.networkClientFactory.GetServiceGatewayClient().GetServices(ctx, dt.config.ResourceGroup, dt.config.ServiceGatewayResourceName)
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("getting Service Gateway services: %w", err)
	}
	return otherServiceUsingNATGateway(services, identity, natGatewayID), nil
}

// scopeLockedErrorCode is Azure's error code for a write blocked by a resource lock (HTTP 409).
const scopeLockedErrorCode = "ScopeLocked"

func isInitialNATPutResponse(respErr *azcore.ResponseError) bool {
	return respErr != nil && respErr.RawResponse != nil && respErr.RawResponse.Request != nil &&
		respErr.RawResponse.Request.Method == http.MethodPut
}

// isLinkRefusal reports whether Azure refused the initial PUT, not a polling or final GET.
// It is a 4xx answer other than a timeout (408),
// throttling (429), a stale precondition (412), or a conflict (409) such as another operation in
// progress, which may clear by themselves. A resource lock's 409 is a refusal.
func isLinkRefusal(respErr *azcore.ResponseError) bool {
	if !isInitialNATPutResponse(respErr) {
		return false
	}
	switch respErr.StatusCode {
	case http.StatusConflict:
		return strings.EqualFold(respErr.ErrorCode, scopeLockedErrorCode)
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusPreconditionFailed:
		return false
	}
	return respErr.StatusCode >= 400 && respErr.StatusCode < 500
}

// isAccessDeniedOrLocked reports whether an Azure call failed on missing permissions or a resource lock.
func isAccessDeniedOrLocked(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && (respErr.StatusCode == http.StatusForbidden || strings.EqualFold(respErr.ErrorCode, scopeLockedErrorCode))
}

// byoNATGatewayProblem returns why a BYO NAT Gateway cannot serve an egress identity, or
// "" when it can. Its Public IPs and prefixes are its owner's: one without an address for a family
// the pods use is linked anyway, and their egress in that family fails until the owner adds one.
func byoNATGatewayProblem(natGateway *armnetwork.NatGateway, serviceGatewayID string) string {
	name := derefString(natGateway.Name)
	if natGateway.Properties == nil {
		return fmt.Sprintf("NAT Gateway %q was read without its properties", name)
	}
	sku := ""
	if natGateway.SKU != nil && natGateway.SKU.Name != nil {
		sku = string(*natGateway.SKU.Name)
	}
	if !strings.EqualFold(sku, string(armnetwork.NatGatewaySKUNameStandardV2)) {
		return fmt.Sprintf("NAT Gateway %q has SKU %q; Service Gateway egress requires %q", name, sku, armnetwork.NatGatewaySKUNameStandardV2)
	}
	if linked := natGatewayServiceGatewayID(natGateway); linked != "" && !strings.EqualFold(linked, serviceGatewayID) {
		return fmt.Sprintf("NAT Gateway %q is already linked to Service Gateway %q", name, linked)
	}
	return ""
}

// otherServiceUsingNATGateway returns the name of a Service Gateway service other than identity
// (for example the default outbound service) that uses natGatewayID, or "".
func otherServiceUsingNATGateway(services []*armnetwork.ServiceGatewayService, identity, natGatewayID string) string {
	if natGatewayID == "" {
		return ""
	}
	for _, service := range services {
		if service == nil || service.Properties == nil || strings.EqualFold(derefString(service.Name), identity) {
			continue
		}
		if strings.EqualFold(derefString(service.Properties.PublicNatGatewayID), natGatewayID) {
			return derefString(service.Name)
		}
	}
	return ""
}

// byoOutboundServicesDTO registers an egress identity with a BYO NAT Gateway.
func byoOutboundServicesDTO(identity, natGatewayID string, dtConfig Config) ServicesDataDTO {
	dto := MapLoadBalancerAndNATGatewayUpdatesToServicesDataDTO(
		SyncServicesReturnType{},
		SyncServicesReturnType{Additions: newIgnoreCaseSetFromSlice([]string{identity})},
		dtConfig.networkResourceSubscriptionID(),
		dtConfig.ResourceGroup,
	)
	for i := range dto.Services {
		dto.Services[i].PublicNatGateway.ID = natGatewayID
	}
	return dto
}

// isClusterResourceGroup reports whether a resource ID lies in the cluster resource group of the cluster's
// network subscription, where this controller creates its NAT Gateways.
func isClusterResourceGroup(id *arm.ResourceID, config Config) bool {
	return strings.EqualFold(id.ResourceGroupName, config.ResourceGroup) &&
		strings.EqualFold(id.SubscriptionID, config.networkResourceSubscriptionID())
}

// natGatewayServiceGatewayID returns the Service Gateway a NAT Gateway is linked to, or "".
func natGatewayServiceGatewayID(natGateway *armnetwork.NatGateway) string {
	if natGateway == nil || natGateway.Properties == nil || natGateway.Properties.ServiceGateway == nil {
		return ""
	}
	return derefString(natGateway.Properties.ServiceGateway.ID)
}

// byoNATGatewayID returns the BYO NAT Gateway ID an egress identity uses, or "".
func (dt *DiffTracker) byoNATGatewayID(identity string) string {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.byoNATGateways[strings.ToLower(identity)]
}

// claimBYONATGateway reserves a gateway before link/register calls; its record survives through deletion.
// It returns whether the claim is new, or the other identity that already holds it.
func (dt *DiffTracker) claimBYONATGateway(identity, natGatewayID string) (bool, string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	identity = strings.ToLower(identity)
	for other, id := range dt.byoNATGateways {
		if other != identity && strings.EqualFold(id, natGatewayID) {
			return false, other
		}
	}
	fresh := dt.byoNATGateways[identity] == ""
	if dt.byoNATGateways == nil {
		dt.byoNATGateways = make(map[string]string)
	}
	dt.byoNATGateways[identity] = natGatewayID
	return fresh, ""
}

// setBYONATGatewayID records the BYO NAT Gateway an egress identity uses; "" forgets it.
func (dt *DiffTracker) setBYONATGatewayID(identity, natGatewayID string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if natGatewayID == "" {
		delete(dt.byoNATGateways, strings.ToLower(identity))
		return
	}
	if dt.byoNATGateways == nil {
		dt.byoNATGateways = make(map[string]string)
	}
	dt.byoNATGateways[strings.ToLower(identity)] = natGatewayID
}

// errNATGatewayNotOwned rejects an egress identity whose name belongs to a NAT Gateway (or service)
// this controller must not manage.
var errNATGatewayNotOwned = errors.New("a NAT Gateway or Service Gateway service of that name in the cluster resource group is not managed by this controller")

// clusterNATGatewayExists reports whether the cluster resource group holds a NAT Gateway named
// identity, and returns errNATGatewayNotOwned when it does but this controller did not create it.
func (dt *DiffTracker) clusterNATGatewayExists(ctx context.Context, identity string) (bool, error) {
	natGateway, exists, err := dt.getNatGateway(ctx, dt.config.ResourceGroup, identity)
	if err != nil || !exists {
		return false, err
	}
	if !ownsNATGateway(natGateway) {
		return true, errNATGatewayNotOwned
	}
	// A gateway another service (the default outbound service) uses is never this identity's.
	natGatewayID := fmt.Sprintf(consts.NatGatewayIDTemplate, dt.config.networkResourceSubscriptionID(), dt.config.ResourceGroup, identity)
	user, err := dt.otherServiceUsing(ctx, identity, natGatewayID)
	if err != nil {
		return true, err
	}
	if user != "" {
		return true, errNATGatewayNotOwned
	}
	return true, nil
}

// ownsClusterNATGateway reports whether this controller may delete the cluster-resource-group NAT
// Gateway named identity: it does not exist, or this controller created it and no other service uses it.
func (dt *DiffTracker) ownsClusterNATGateway(ctx context.Context, identity string) (bool, error) {
	_, err := dt.clusterNATGatewayExists(ctx, identity)
	switch {
	case errors.Is(err, errNATGatewayNotOwned):
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

// isReservedOutboundService protects RP-owned and inbound identities, not unrelated gateway names.
func (dt *DiffTracker) isReservedOutboundService(identity string) bool {
	if IsReservedEgressIdentity(identity) {
		return true
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.outboundIdentityConflictsWithInboundLocked(identity) ||
		(dt.NRPResources.ReservedOutboundServices != nil && dt.NRPResources.ReservedOutboundServices.Has(identity))
}
