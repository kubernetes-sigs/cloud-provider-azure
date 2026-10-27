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
	"net/http"
	"regexp"
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

const (
	egressNATGatewayLinkedReason   = "ServiceGatewayEgressNATGatewayLinked"
	egressNATGatewayRejectedReason = "ServiceGatewayEgressNATGatewayRejected"
)

const natGatewayResourceType = "Microsoft.Network/natGateways"

var byoResourceGroupNameRE = regexp.MustCompile(consts.ResourceGroupNamePattern)

// Admission rejection retries without holding initial synchronization open between attempts.
type egressRejectedError struct{ error }

var errNoEgressMember = errors.New("no active member pod")

func (e egressRejectedError) Unwrap() error { return e.error }

func isEgressRejected(err error) bool {
	var rejected egressRejectedError
	return errors.As(err, &rejected)
}

// On success, a nil gateway means managed egress; the pod identifies the event recipient.
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
		// Keep a possibly linked gateway selected until cleanup, regardless of annotation edits.
		natGateway, exists, err := dt.getNatGatewayByID(ctx, linked)
		if err != nil {
			return readFailed(linked, err)
		}
		if exists {
			return natGateway, pod, nil
		}
		dt.setBYONATGatewayID(identity, "")
	}

	resourceGroup, explicitBYO := "", false
	if pod != nil {
		resourceGroup, explicitBYO = pod.Annotations[consts.PodAnnotationServiceEgressGatewayResourceGroup]
		resourceGroup = strings.TrimSpace(resourceGroup)
	}
	if !explicitBYO {
		if _, err := dt.clusterNATGatewayExists(ctx, identity); err != nil {
			if errors.Is(err, errNATGatewayNotOwned) {
				return reject(err)
			}
			return nil, pod, err
		}
		return nil, pod, nil
	}
	if problem := byoNATGatewayResourceGroupProblem(resourceGroup, dt.config); problem != "" {
		return reject(errors.New(problem))
	}
	gatewayName, kind, ok := serviceIdentity(identity)
	if !ok || kind != Outbound {
		return reject(fmt.Errorf("invalid outbound ServiceGateway name %q", identity))
	}
	natGatewayID := fmt.Sprintf(consts.NatGatewayIDTemplate, dt.config.networkResourceSubscriptionID(), resourceGroup, gatewayName)
	natGateway, exists, err := dt.getNatGatewayByID(ctx, natGatewayID)
	if err != nil {
		return readFailed(natGatewayID, err)
	}
	if !exists {
		return reject(fmt.Errorf("NAT Gateway %q not found in resource group %q", gatewayName, resourceGroup))
	}
	return natGateway, pod, nil
}

// Read pods from the API because selection also runs before informer startup.
// Prefer the trigger, then buffered arrival order, then sorted startup members.
func (dt *DiffTracker) egressMemberPod(ctx context.Context, identity, podNS, podName string) (*v1.Pod, error) {
	if dt.kubeClient == nil {
		return nil, nil
	}
	trigger := podNS + "/" + podName
	if podName != "" {
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

func (dt *DiffTracker) readEgressMember(ctx context.Context, identity, namespace, name string) (*v1.Pod, error) {
	pod, err := dt.kubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading pod %s/%s of egress identity %q: %w", namespace, name, identity, err)
	}
	if pod.DeletionTimestamp != nil ||
		(pod.Status.Phase != v1.PodRunning && pod.Status.Phase != v1.PodPending) ||
		!strings.EqualFold(outboundServiceName(pod.Labels[consts.PodLabelServiceEgressGateway]), identity) {
		return nil, nil
	}
	return pod, nil
}

// Snapshot membership under dt.mu; callers read pods after releasing it.
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

func (dt *DiffTracker) recordEgressEvent(pod *v1.Pod, eventType, reason, message string) {
	if pod != nil {
		dt.recordEvent(pod, eventType, reason, message)
	}
}

func byoNATGatewayResourceGroupProblem(resourceGroup string, config Config) string {
	if !byoResourceGroupNameRE.MatchString(resourceGroup) {
		return fmt.Sprintf("%s %q must be a valid Azure resource group name",
			consts.PodAnnotationServiceEgressGatewayResourceGroup, resourceGroup)
	}
	if strings.EqualFold(resourceGroup, config.ResourceGroup) {
		return fmt.Sprintf("resource group %q is the cluster resource group; BYO NAT Gateways must be outside it", resourceGroup)
	}
	return ""
}

// Recovered ARM IDs must still match the network subscription and BYO scope.
func byoNATGatewayIDProblem(id string, config Config) string {
	parsed, err := arm.ParseResourceID(id)
	switch {
	case err != nil:
		return fmt.Sprintf("NAT Gateway ID %q is not an Azure resource ID", id)
	case !strings.EqualFold(parsed.ResourceType.String(), natGatewayResourceType) || parsed.Parent == nil ||
		!strings.EqualFold(parsed.Parent.ResourceType.String(), arm.ResourceGroupResourceType.String()):
		return fmt.Sprintf("%q is not the ID of a NAT Gateway in a resource group (%s)", id, natGatewayResourceType)
	case !strings.EqualFold(parsed.SubscriptionID, config.networkResourceSubscriptionID()):
		return fmt.Sprintf("NAT Gateway %q is not in the cluster's network subscription %q", id, config.networkResourceSubscriptionID())
	case strings.EqualFold(parsed.ResourceGroupName, config.ResourceGroup):
		return fmt.Sprintf("NAT Gateway %q is in the cluster resource group %q, which only holds NAT Gateways this controller manages", id, config.ResourceGroup)
	}
	return ""
}

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
	freshRecord := dt.setBYONATGatewayID(identity, natGatewayID)
	linkedHere := strings.EqualFold(natGatewayServiceGatewayID(natGateway), serviceGatewayID)
	retainRecord := !freshRecord || linkedHere
	defer func() {
		if !retainRecord {
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
	// Recorded IDs include ambiguous writes, so every attempt must reassert the link.
	linkCtx, err := withNATGatewayETag(ctx, natGateway.Etag)
	if err != nil {
		s.onComplete(identity, false, err)
		return
	}
	if err := s.cleanupManagedOutboundResources(ctx, identity, correlationID); err != nil {
		s.onComplete(identity, false, fmt.Errorf("cleaning managed resources before BYO selection: %w", err))
		return
	}

	// Keep the record once a write may have applied, including an ambiguous failure.
	retainRecord = true
	natGateway.Properties.ServiceGateway = &armnetwork.SubResource{ID: to.Ptr(serviceGatewayID)}
	if err := dt.createOrUpdateNatGateway(linkCtx, parsed.ResourceGroupName, *natGateway); err != nil {
		s.logger.V(4).Info("Could not link BYO NAT Gateway to Service Gateway", "serviceUID", identity, "correlationID", correlationID, "natGateway", natGatewayID, "err", err)
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && isResponseForMethod(respErr, http.MethodPut) &&
			respErr.StatusCode == http.StatusPreconditionFailed && freshRecord && !linkedHere {
			retainRecord = false
		}
		if errors.As(err, &respErr) && isLinkRefusal(respErr) {
			problem := fmt.Sprintf("Azure refused to link it to the Service Gateway (HTTP %d %s)", respErr.StatusCode, respErr.ErrorCode)
			if respErr.StatusCode == http.StatusForbidden {
				problem = "access denied when linking it to the Service Gateway; grant the cluster identity write access to the NAT Gateway"
			}
			reject(problem)
			return
		}
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

// Remove owned resources left by an interrupted managed creation.
func (s *ServiceUpdater) cleanupManagedOutboundResources(ctx context.Context, identity, correlationID string) error {
	dt := s.diffTracker
	exists, err := dt.clusterNATGatewayExists(ctx, identity)
	if errors.Is(err, errNATGatewayNotOwned) {
		s.logger.V(2).Info("Kept NAT Gateway and Public IPs not owned by this identity", "serviceUID", identity)
		return nil
	}
	if err != nil {
		return err
	}
	if exists {
		if err := dt.disassociateNatGatewayFromServiceGateway(ctx, dt.config.ServiceGatewayResourceName, identity); err != nil {
			return err
		}
		return s.deleteManagedOutboundResources(ctx, identity, correlationID)
	}
	return s.deleteOwnedOutboundPublicIPs(ctx, identity, correlationID)
}

func (s *ServiceUpdater) deleteBYOOutboundService(ctx context.Context, identity, natGatewayID, correlationID string) {
	dt := s.diffTracker
	if parsed, err := arm.ParseResourceID(natGatewayID); err == nil {
		if err := dt.unlinkNatGateway(ctx, dt.config.ServiceGatewayResourceName, identity, parsed.ResourceGroupName, parsed.Name); err != nil {
			// Permission or lock refusals do not block finalizer release.
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

	s.finishOutboundDeletion(ctx, identity, correlationID, s.cleanupManagedOutboundResources(ctx, identity, correlationID))
}

// A missing Service Gateway has no remaining users.
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

const scopeLockedErrorCode = "ScopeLocked"

// Only a proven initial PUT refusal can clear a record; polling/final GET errors cannot.
// Retryable 408/409/412/429 responses are excluded, except the ScopeLocked 409.
func isLinkRefusal(respErr *azcore.ResponseError) bool {
	if !isResponseForMethod(respErr, http.MethodPut) {
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

func isAccessDeniedOrLocked(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && (respErr.StatusCode == http.StatusForbidden || strings.EqualFold(respErr.ErrorCode, scopeLockedErrorCode))
}

// Public addresses and IP-family configuration remain the owner's responsibility.
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

func isClusterResourceGroup(id *arm.ResourceID, config Config) bool {
	return strings.EqualFold(id.ResourceGroupName, config.ResourceGroup) &&
		strings.EqualFold(id.SubscriptionID, config.networkResourceSubscriptionID())
}

func natGatewayServiceGatewayID(natGateway *armnetwork.NatGateway) string {
	if natGateway == nil || natGateway.Properties == nil || natGateway.Properties.ServiceGateway == nil {
		return ""
	}
	return derefString(natGateway.Properties.ServiceGateway.ID)
}

func (dt *DiffTracker) byoNATGatewayID(identity string) string {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.byoNATGateways[strings.ToLower(identity)]
}

// setBYONATGatewayID returns whether it added the identity's first cleanup record.
func (dt *DiffTracker) setBYONATGatewayID(identity, natGatewayID string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	identity = strings.ToLower(identity)
	if natGatewayID == "" {
		delete(dt.byoNATGateways, identity)
		return false
	}
	fresh := dt.byoNATGateways[identity] == ""
	if dt.byoNATGateways == nil {
		dt.byoNATGateways = make(map[string]string)
	}
	dt.byoNATGateways[identity] = natGatewayID
	return fresh
}

var errNATGatewayNotOwned = errors.New("a NAT Gateway or Service Gateway service of that name in the cluster resource group is not managed by this controller")

// An existing gateway needs matching ownership and no other user in this Service Gateway.
func (dt *DiffTracker) clusterNATGatewayExists(ctx context.Context, identity string) (bool, error) {
	natGateway, exists, err := dt.getNatGateway(ctx, dt.config.ResourceGroup, identity)
	if err != nil || !exists {
		return false, err
	}
	if !ownsNATGateway(natGateway) {
		return true, errNATGatewayNotOwned
	}
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

// Absence also permits cleanup of any remaining owned Public IPs.
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

// Reserve service identities, not the names of their referenced NAT Gateways.
func (dt *DiffTracker) isReservedOutboundService(identity string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.isReservedServiceLocked(identity)
}

func (dt *DiffTracker) isReservedServiceLocked(identity string) bool {
	return IsReservedEgressIdentity(identity) ||
		(dt.NRPResources.ReservedOutboundServices != nil && dt.NRPResources.ReservedOutboundServices.Has(identity))
}
