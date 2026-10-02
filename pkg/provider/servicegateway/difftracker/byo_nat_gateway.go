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
// A pod selects an egress identity with the egress label; the label value is a NAT Gateway name.
// The gateway behind an identity is resolved once, when the identity is created:
//
//  1. A NAT Gateway of that name in the cluster resource group: managed by this controller. One this
//     controller did not create (such as the default outbound service's) is never taken over.
//  2. Otherwise one in the VNet resource group (only when it differs from the cluster resource
//     group): a BYO NAT Gateway, owned by the user.
//  3. Otherwise this controller creates a managed one in the cluster resource group.
//
// Once linked, an identity keeps its BYO NAT Gateway until the identity is deleted.
//
// A BYO NAT Gateway, and its Public IPs, are never created, modified or deleted. The only
// write is its link to the Service Gateway (properties.serviceGateway), set when the identity is
// created and cleared when it is deleted. Which identities use a BYO gateway is rebuilt on
// start-up from the Service Gateway's services, whose NAT Gateway IDs lie outside the cluster
// resource group.
//
// Known gap: if the controller stops between linking a BYO NAT Gateway and registering the identity,
// and all the identity's pods are deleted before it restarts, the link is left behind (nothing
// records it). The gateway is picked up again if the identity is used again; otherwise its owner
// clears properties.serviceGateway. Scanning for such links at start-up is avoided because it would
// race the AKS RP, which links a BYO default NAT Gateway before registering it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9"
	v1 "k8s.io/api/core/v1"

	"sigs.k8s.io/cloud-provider-azure/pkg/azclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
)

// Event reasons emitted on the pod that triggered an egress identity's creation.
const (
	egressNATGatewayLinkedReason     = "ServiceGatewayEgressNATGatewayLinked"
	egressNATGatewayRejectedReason   = "ServiceGatewayEgressNATGatewayRejected"
	egressNATGatewayUnreadableReason = "ServiceGatewayEgressNATGatewayUnreadable"
)

// resolveBYONATGateway returns the BYO NAT Gateway an egress identity names, or nil when
// the identity uses a NAT Gateway managed by this controller.
func (s *ServiceUpdater) resolveBYONATGateway(ctx context.Context, identity, podNS, podName string) (*armnetwork.NatGateway, error) {
	dt := s.diffTracker
	if dt.isUnmanagedNATGateway(identity) {
		// Recorded at start-up: the default outbound service's name or gateway, or a gateway in the
		// cluster resource group that this controller did not create. Never taken over.
		dt.recordPodEvent(ctx, podNS, podName, v1.EventTypeWarning, egressNATGatewayRejectedReason,
			fmt.Sprintf("Egress identity %q cannot be used: %v.", identity, errNATGatewayNotOwned))
		return nil, fmt.Errorf("egress identity %q: %w", identity, errNATGatewayNotOwned)
	}
	if linked := dt.byoNATGatewayID(identity); linked != "" {
		natGateway, exists, err := dt.getNatGatewayByID(ctx, linked)
		if err != nil || exists {
			return natGateway, err
		}
		// Its owner deleted it; resolve afresh.
		dt.setBYONATGatewayID(identity, "")
	}

	if exists, err := dt.clusterNATGatewayExists(ctx, identity); err != nil || exists {
		if errors.Is(err, errNATGatewayNotOwned) {
			dt.recordPodEvent(ctx, podNS, podName, v1.EventTypeWarning, egressNATGatewayRejectedReason,
				fmt.Sprintf("Egress identity %q cannot be used: %v.", identity, err))
		}
		return nil, err
	}
	byoRG := dt.config.byoNATGatewayResourceGroup()
	if byoRG == "" {
		return nil, nil
	}
	natGateway, exists, err := dt.getNatGateway(ctx, byoRG, identity)
	if err != nil {
		// Without read access the owner's intent is unknown; egress must keep working, so fall
		// back to a managed gateway. This is the normal case when the cluster identity has no read
		// access on the VNet resource group, so it is a Normal event, not a Warning.
		if httpStatus, _ := extractAzureErrorInfo(err); httpStatus == http.StatusForbidden {
			dt.recordPodEvent(ctx, podNS, podName, v1.EventTypeNormal, egressNATGatewayUnreadableReason, fmt.Sprintf(
				"Cannot read NAT Gateway %q in resource group %q (access denied), so egress identity %q uses a NAT Gateway managed by the cluster. "+
					"To use your own NAT Gateway, grant the cluster identity read and write access to it, then delete and recreate all pods labelled %s=%s.",
				identity, byoRG, identity, consts.PodLabelServiceEgressGateway, identity))
			return nil, nil
		}
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return natGateway, nil
}

// createBYOOutboundService validates a BYO NAT Gateway, links it to the Service Gateway
// and registers the egress identity with it. Failures are retried with backoff, so a gateway the
// owner fixes (or grants access to) is picked up without recreating the pods.
func (s *ServiceUpdater) createBYOOutboundService(ctx context.Context, identity string, config *OutboundConfig, natGateway *armnetwork.NatGateway,
	correlationID, podNS, podName string) {
	dt := s.diffTracker
	natGatewayID := derefString(natGateway.ID)
	serviceGatewayID := dt.config.ServiceGatewayResourceID()

	reject := func(problem string) {
		dt.recordPodEvent(ctx, podNS, podName, v1.EventTypeWarning, egressNATGatewayRejectedReason,
			fmt.Sprintf("Egress identity %q cannot use NAT Gateway %q: %s.", identity, natGatewayID, problem))
		s.onComplete(identity, false, fmt.Errorf("BYO NAT Gateway %q: %s", natGatewayID, problem))
	}

	var families []string
	if config != nil {
		families = config.IPFamilies
	}
	if problem := byoNATGatewayProblem(natGateway, families, serviceGatewayID); problem != "" {
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
	parsed, err := arm.ParseResourceID(natGatewayID)
	if err != nil {
		s.onComplete(identity, false, fmt.Errorf("parsing NAT Gateway ID %q: %w", natGatewayID, err))
		return
	}

	// Recorded before the link is written, so a delete after a partial create unlinks it.
	dt.setBYONATGatewayID(identity, natGatewayID)

	// The link alone does not prove it was applied, so a gateway that is not Succeeded is written again.
	linked := strings.EqualFold(natGatewayServiceGatewayID(natGateway), serviceGatewayID) &&
		natGateway.Properties.ProvisioningState != nil && *natGateway.Properties.ProvisioningState == armnetwork.ProvisioningStateSucceeded
	if !linked {
		natGateway.Properties.ServiceGateway = &armnetwork.SubResource{ID: to.Ptr(serviceGatewayID)}
		if err := dt.createOrUpdateNatGateway(ctx, parsed.ResourceGroupName, *natGateway); err != nil {
			if httpStatus, _ := extractAzureErrorInfo(err); httpStatus == http.StatusForbidden {
				dt.recordPodEvent(ctx, podNS, podName, v1.EventTypeWarning, egressNATGatewayRejectedReason, fmt.Sprintf(
					"Egress identity %q cannot use NAT Gateway %q: access denied when linking it to the Service Gateway. "+
						"Grant the cluster identity write access to the NAT Gateway.", identity, natGatewayID))
			}
			s.logger.V(4).Info("Could not link BYO NAT Gateway to Service Gateway", "serviceUID", identity, "correlationID", correlationID, "natGateway", natGatewayID, "err", err)
			s.onComplete(identity, false, fmt.Errorf("failed to link NAT Gateway: %w", err))
			return
		}
	}

	if err := dt.updateNRPSGWServices(ctx, dt.config.ServiceGatewayResourceName, byoOutboundServicesDTO(identity, natGatewayID, dt.config)); err != nil {
		s.logger.V(4).Info("Could not register outbound service with ServiceGateway", "serviceUID", identity, "correlationID", correlationID, "err", err)
		s.onComplete(identity, false, fmt.Errorf("failed to register with ServiceGateway: %w", err))
		return
	}

	dt.UpdateNRPNATGateways(SyncServicesReturnType{Additions: newIgnoreCaseSetFromSlice([]string{identity})})
	dt.recordPodEvent(ctx, podNS, podName, v1.EventTypeNormal, egressNATGatewayLinkedReason,
		fmt.Sprintf("Egress identity %q uses BYO NAT Gateway %q.", identity, natGatewayID))
	s.onComplete(identity, true, nil)
	s.logger.V(2).Info("Created outbound service with BYO NAT Gateway", "serviceUID", identity, "natGateway", natGatewayID, "correlationID", correlationID)
}

// deleteBYOOutboundService unlinks a BYO NAT Gateway and unregisters the egress identity.
// The gateway and its Public IPs are left in place. The identity is only unregistered once the
// gateway is unlinked, so a failed unlink is retried rather than leaving the link behind.
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

// recordLinkedBYONATGateways covers a restart between the two steps of an unlink: the identity's
// Service Gateway service has already lost its NAT Gateway ID, but its BYO NAT Gateway (named
// after the identity) is still linked to this Service Gateway. Recording it lets the pending delete
// finish the unlink. Best-effort: a gateway that cannot be read stays linked.
func recordLinkedBYONATGateways(ctx context.Context, config Config, networkClientFactory azclient.ClientFactory, nrp *NRPState) {
	byoRG := config.byoNATGatewayResourceGroup()
	if byoRG == "" || nrp.NATGateways == nil {
		return
	}
	logger := log.FromContextOrBackground(ctx)
	for _, identity := range nrp.NATGateways.UnsortedList() {
		if nrp.OutboundNATGatewayIDs[strings.ToLower(identity)] != "" {
			continue
		}
		natGateway, err := networkClientFactory.GetNatGatewayClient().Get(ctx, byoRG, identity, nil)
		if err != nil {
			logger.V(4).Info("Could not check for a linked BYO NAT Gateway", "identity", identity, "err", err)
			continue
		}
		if natGateway == nil || !strings.EqualFold(natGatewayServiceGatewayID(natGateway), config.ServiceGatewayResourceID()) {
			continue
		}
		if nrp.OutboundNATGatewayIDs == nil {
			nrp.OutboundNATGatewayIDs = make(map[string]string)
		}
		nrp.OutboundNATGatewayIDs[strings.ToLower(identity)] = fmt.Sprintf(consts.NatGatewayIDTemplate, config.networkResourceSubscriptionID(), byoRG, identity)
		logger.V(2).Info("Recorded BYO NAT Gateway still linked to an identity being unlinked", "identity", identity)
	}
}

// isAccessDeniedOrLocked reports whether an Azure call failed on missing permissions or a resource lock.
func isAccessDeniedOrLocked(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && (respErr.StatusCode == http.StatusForbidden || strings.EqualFold(respErr.ErrorCode, "ScopeLocked"))
}

// byoNATGatewayProblem returns why a BYO NAT Gateway cannot serve an egress identity, or
// "" when it can. families are the address families the identity needs ("IPv4"/"IPv6").
func byoNATGatewayProblem(natGateway *armnetwork.NatGateway, families []string, serviceGatewayID string) string {
	name := derefString(natGateway.Name)
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
	var props armnetwork.NatGatewayPropertiesFormat
	if natGateway.Properties != nil {
		props = *natGateway.Properties
	}
	if len(families) == 0 {
		families = []string{string(armnetwork.IPVersionIPv4)}
	}
	for _, family := range families {
		if strings.EqualFold(family, string(armnetwork.IPVersionIPv6)) {
			if len(props.PublicIPAddressesV6) == 0 && len(props.PublicIPPrefixesV6) == 0 {
				return fmt.Sprintf("NAT Gateway %q has no IPv6 Public IP address or prefix, which this cluster's IPv6 egress needs", name)
			}
		} else if len(props.PublicIPAddresses) == 0 && len(props.PublicIPPrefixes) == 0 {
			return fmt.Sprintf("NAT Gateway %q has no IPv4 Public IP address or prefix", name)
		}
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

// isBYONATGatewayID reports whether a NAT Gateway ID lies outside the cluster resource group,
// i.e. the gateway was not created by this controller and must never be deleted by it.
func isBYONATGatewayID(natGatewayID string, dtConfig Config) bool {
	parsed, err := arm.ParseResourceID(natGatewayID)
	if err != nil {
		return false
	}
	return !strings.EqualFold(parsed.ResourceGroupName, dtConfig.ResourceGroup) ||
		!strings.EqualFold(parsed.SubscriptionID, dtConfig.networkResourceSubscriptionID())
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
	if !ownsNATGateway(natGateway, dt.config) {
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

// isUnmanagedNATGateway reports whether the cluster resource group holds a NAT Gateway named identity
// that this controller did not create (recorded at start-up), and identity is not registered with
// the Service Gateway (a registered one still has its own registration to remove).
func (dt *DiffTracker) isUnmanagedNATGateway(identity string) bool {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return dt.NRPResources.UnmanagedNATGateways != nil && dt.NRPResources.UnmanagedNATGateways.Has(identity) &&
		(dt.NRPResources.NATGateways == nil || !dt.NRPResources.NATGateways.Has(identity))
}
