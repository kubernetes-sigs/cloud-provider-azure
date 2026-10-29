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

// egressRejectedError marks admission rejections that retry without blocking initial sync between attempts.
type egressRejectedError struct{ error }

var errNoEgressMember = errors.New("no active member pod")

func (e egressRejectedError) Unwrap() error { return e.error }

func isEgressRejected(err error) bool {
	var rejected egressRejectedError
	return errors.As(err, &rejected)
}

// resolveBYONATGateway selects a gateway; nil on success means managed egress.
// The returned pod identifies the event recipient.
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
		_, canManage, err := dt.clusterNATGatewayState(ctx, identity)
		if err != nil {
			return nil, pod, err
		}
		if !canManage {
			return reject(errNATGatewayNotOwned)
		}
		return nil, pod, nil
	}
	if problem := validateBYONATGatewayResourceGroup(resourceGroup, dt.config); problem != "" {
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

// egressMemberPod reads from the API because selection also runs before informer startup.
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

// egressPodKeysOf snapshots membership under dt.mu; callers read pods after releasing it.
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

// recoverUnregisteredBYOLinks recovers cleanup targets declared by departed pods.
// Registered identities retain their recorded target; live members retain their gateway.
func (dt *DiffTracker) recoverUnregisteredBYOLinks(ctx context.Context, pods *v1.PodList) ([]string, error) {
	if pods == nil {
		return nil, nil
	}
	active := make(map[string]bool)
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil && (pod.Status.Phase == v1.PodRunning || pod.Status.Phase == v1.PodPending) {
			active[outboundServiceName(pod.Labels[consts.PodLabelServiceEgressGateway])] = true
		}
	}

	var recovered []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		identity := outboundServiceName(pod.Labels[consts.PodLabelServiceEgressGateway])
		if identity == "" || active[identity] || !hasPodFinalizer(pod) ||
			dt.NRPResources.NATGateways.Has(identity) || dt.isReservedOutboundService(identity) {
			continue
		}
		resourceGroup, explicit := pod.Annotations[consts.PodAnnotationServiceEgressGatewayResourceGroup]
		resourceGroup = strings.TrimSpace(resourceGroup)
		if !explicit || validateBYONATGatewayResourceGroup(resourceGroup, dt.config) != "" {
			continue
		}
		name, kind, valid := serviceIdentity(identity)
		if !valid || kind != Outbound {
			continue
		}
		id := fmt.Sprintf(consts.NatGatewayIDTemplate, dt.config.networkResourceSubscriptionID(), resourceGroup, name)
		previous := dt.byoNATGatewayID(identity)
		if strings.EqualFold(previous, id) {
			continue
		}
		gateway, exists, err := dt.getNatGatewayByID(ctx, id)
		if err != nil {
			if isAccessDeniedOrLocked(err) {
				dt.logger.Error(err, "Could not recover BYO cleanup target; remove any remaining link manually", "service", identity, "natGateway", id)
				continue
			}
			return nil, fmt.Errorf("reading unregistered BYO cleanup target %q: %w", id, err)
		}
		if !exists {
			continue
		}
		if gateway.Properties == nil {
			return nil, fmt.Errorf("NAT Gateway %q was read without its properties; cleanup link state is unknown", id)
		}
		if !strings.EqualFold(natGatewayServiceGatewayID(gateway), dt.config.ServiceGatewayResourceID()) {
			continue
		}
		if previous != "" {
			return nil, fmt.Errorf("egress identity %q has conflicting unregistered BYO cleanup targets %q and %q", identity, previous, id)
		}
		dt.setBYONATGatewayID(identity, id)
		recovered = append(recovered, identity)
	}
	return recovered, nil
}

func (dt *DiffTracker) recordEgressEvent(pod *v1.Pod, eventType, reason, message string) {
	if pod != nil {
		dt.recordEvent(pod, eventType, reason, message)
	}
}

// validateBYONATGatewayResourceGroup checks the name syntax and rejects the cluster resource group.
// It returns a rejection reason, or an empty string when valid.
func validateBYONATGatewayResourceGroup(resourceGroup string, config Config) string {
	if !byoResourceGroupNameRE.MatchString(resourceGroup) {
		return fmt.Sprintf("%s %q must be a valid Azure resource group name",
			consts.PodAnnotationServiceEgressGatewayResourceGroup, resourceGroup)
	}
	if strings.EqualFold(resourceGroup, config.ResourceGroup) {
		return fmt.Sprintf("resource group %q is the cluster resource group; BYO NAT Gateways must be outside it", resourceGroup)
	}
	return ""
}

// validateBYONATGatewayID requires a NAT Gateway ARM ID in the network subscription,
// outside the cluster resource group. It returns a rejection reason, or an empty string when valid.
func validateBYONATGatewayID(id string, config Config) string {
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

// cleanupManagedOutboundResources removes owned resources left by an interrupted managed creation.
func (s *ServiceUpdater) cleanupManagedOutboundResources(ctx context.Context, identity, correlationID string) error {
	dt := s.diffTracker
	exists, canManage, err := dt.unlinkNatGateway(ctx, dt.config.ServiceGatewayResourceName, identity, dt.config.ResourceGroup, identity)
	if err != nil {
		return err
	}
	if !canManage {
		s.logger.V(2).Info("Kept NAT Gateway and Public IPs not owned by this identity", "serviceUID", identity)
		return nil
	}
	if exists {
		return s.deleteManagedOutboundResources(ctx, identity, correlationID)
	}
	return s.deleteOwnedOutboundPublicIPs(ctx, identity, correlationID)
}

// otherServiceUsing finds another service using the NAT Gateway.
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

// isLinkRefusal identifies a non-retryable rejection of the initial NAT Gateway link PUT.
// Polling/final GET errors may happen after the link was applied, so the saved
// BYO NAT Gateway ID must be retained for cleanup.
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

// validateBYONATGateway requires readable properties, StandardV2 SKU, and no link to another Service Gateway.
// It returns a rejection reason, or an empty string when valid.
// Public addresses and IP-family configuration remain the owner's responsibility.
func validateBYONATGateway(natGateway *armnetwork.NatGateway, serviceGatewayID string) string {
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

// clusterNATGatewayState reports existence and permission to manage the identity.
// Existing gateways must be CCM-owned and unused by other services.
// Absence permits creation and cleanup of owned leftover Public IPs.
func (dt *DiffTracker) clusterNATGatewayState(ctx context.Context, identity string) (exists, canManage bool, err error) {
	natGateway, exists, err := dt.getNatGateway(ctx, dt.config.ResourceGroup, identity)
	if err != nil {
		return false, false, err
	}
	if !exists {
		return false, true, nil
	}
	if !ownsNATGateway(natGateway) {
		return true, false, nil
	}
	natGatewayID := fmt.Sprintf(consts.NatGatewayIDTemplate, dt.config.networkResourceSubscriptionID(), dt.config.ResourceGroup, identity)
	user, err := dt.otherServiceUsing(ctx, identity, natGatewayID)
	if err != nil {
		return true, false, err
	}
	return true, user == "", nil
}

// isReservedOutboundService checks reserved service identities, not referenced NAT Gateway names.
func (dt *DiffTracker) isReservedOutboundService(identity string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.isReservedServiceLocked(identity)
}

func (dt *DiffTracker) isReservedServiceLocked(identity string) bool {
	return IsReservedEgressIdentity(identity) ||
		(dt.NRPResources.ReservedOutboundServices != nil && dt.NRPResources.ReservedOutboundServices.Has(identity))
}
