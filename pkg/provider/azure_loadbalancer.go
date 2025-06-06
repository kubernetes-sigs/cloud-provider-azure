/*
Copyright 2020 The Kubernetes Authors.

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

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	cloudprovider "k8s.io/cloud-provider"
	servicehelpers "k8s.io/cloud-provider/service/helpers"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/log"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/config"
	"sigs.k8s.io/cloud-provider-azure/pkg/provider/loadbalancer"
	"sigs.k8s.io/cloud-provider-azure/pkg/trace"
	"sigs.k8s.io/cloud-provider-azure/pkg/trace/attributes"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/errutils"
	"sigs.k8s.io/cloud-provider-azure/pkg/util/iputil"
	utilsets "sigs.k8s.io/cloud-provider-azure/pkg/util/sets"
)

var _ cloudprovider.LoadBalancer = (*Cloud)(nil)

// Since public IP is not a part of the load balancer on Azure,
// there is a chance that we could orphan public IP resources while we delete the load balancer (kubernetes/kubernetes#80571).
// We need to make sure the existence of the load balancer depends on the load balancer resource and public IP resource on Azure.
func (az *Cloud) existsPip(ctx context.Context, clusterName string, service *v1.Service) bool {
	v4Enabled, v6Enabled := getIPFamiliesEnabled(service)
	existsPipSingleStack := func(isIPv6 bool) bool {
		pipName, _, err := az.determinePublicIPName(ctx, clusterName, service, isIPv6)
		if err != nil {
			return false
		}
		pipResourceGroup := az.getPublicIPAddressResourceGroup(service)
		_, existingPip, err := az.getPublicIPAddress(ctx, pipResourceGroup, pipName, azcache.CacheReadTypeDefault)
		if err != nil {
			return false
		}
		return existingPip
	}

	if v4Enabled && !existsPipSingleStack(consts.IPVersionIPv4) {
		return false
	}
	if v6Enabled && !existsPipSingleStack(consts.IPVersionIPv6) {
		return false
	}
	return true
}

// GetLoadBalancer returns whether the specified load balancer exists, and
// if so, what its status is.
// Implementations must treat the *v1.Service parameter as read-only and not modify it.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager.
// TODO: Break this up into different interfaces (LB, etc) when we have more than one type of service
func (az *Cloud) GetLoadBalancer(ctx context.Context, clusterName string, service *v1.Service) (status *v1.LoadBalancerStatus, exists bool, err error) {
	const Operation = "GetLoadBalancer"

	ctx, span := trace.BeginReconcile(ctx, trace.DefaultTracer(), Operation)
	defer func() { span.Observe(ctx, err) }()

	logger := log.FromContextOrBackground(ctx).WithName(Operation).WithValues("service", service.Name)
	ctx = log.NewContext(ctx, logger)

	existingLBs, err := az.ListLB(ctx, service)
	if err != nil {
		return nil, az.existsPip(ctx, clusterName, service), err
	}

	_, _, status, _, existsLb, _, err := az.getServiceLoadBalancer(ctx, service, clusterName, nil, false, existingLBs)
	if err != nil || existsLb {
		return status, existsLb || az.existsPip(ctx, clusterName, service), err
	}

	flippedService := flipServiceInternalAnnotation(service)
	_, _, status, _, existsLb, _, err = az.getServiceLoadBalancer(ctx, flippedService, clusterName, nil, false, existingLBs)
	if err != nil || existsLb {
		return status, existsLb || az.existsPip(ctx, clusterName, service), err
	}

	// Return exists = false only if the load balancer and the public IP are not found on Azure
	if !az.existsPip(ctx, clusterName, service) {
		logger.V(5).Info("LoadBalancer and PublicIP not found")
		return nil, false, nil
	}

	// Return exists = true if only the public IP exists
	return nil, true, nil
}

func getPublicIPDomainNameLabel(service *v1.Service) (string, bool) {
	if labelName, found := service.Annotations[consts.ServiceAnnotationDNSLabelName]; found {
		return labelName, found
	}
	return "", false
}

// reconcileService reconcile the LoadBalancer service. It returns LoadBalancerStatus on success.
func (az *Cloud) reconcileService(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (*v1.LoadBalancerStatus, error) {
	logger := log.FromContextOrBackground(ctx)

	logger.V(2).Info("Start reconciling Service", "lb", az.GetLoadBalancerName(ctx, clusterName, service))

	lb, needRetry, err := az.reconcileLoadBalancer(ctx, clusterName, service, nodes, true /* wantLb */)
	if err != nil {
		logger.Error(err, "Failed to reconcile LoadBalancer")
		return nil, err
	}
	if needRetry {
		logger.V(2).Info("Reconciling Service again after deleting PLS, as the LB ETag has changed.")
		lb, _, err = az.reconcileLoadBalancer(ctx, clusterName, service, nodes, true /* wantLb */)
		if err != nil {
			logger.Error(err, "Failed to reconcile LoadBalancer")
			return nil, err
		}
	}

	lbStatus, lbIPsPrimaryPIPs, fipConfigs, err := az.getServiceLoadBalancerStatus(ctx, service, lb)
	if err != nil {
		logger.Error(err, "Failed to get LoadBalancer status")
		if !errors.Is(err, ErrorNotVmssInstance) {
			return nil, err
		}
	}

	if _, err := az.reconcileSecurityGroup(ctx, clusterName, service, ptr.Deref(lb.Name, ""), lbIPsPrimaryPIPs, true /* wantLb */); err != nil {
		logger.Error(err, "Failed to reconcile SecurityGroup")
		return nil, err
	}

	for _, fipConfig := range fipConfigs {
		if _, err := az.reconcilePrivateLinkService(ctx, clusterName, service, fipConfig, true /* wantPLS */); err != nil {
			logger.Error(err, "Failed to reconcile PrivateLinkService")
			return nil, err
		}
	}

	updateService := updateServiceLoadBalancerIPs(service, lbIPsPrimaryPIPs)
	flippedService := flipServiceInternalAnnotation(updateService)
	if _, _, err := az.reconcileLoadBalancer(ctx, clusterName, flippedService, nil, false /* wantLb */); err != nil {
		logger.Error(err, "Failed to reconcile flipped LoadBalancer")
		return nil, err
	}

	// lb is not reused here because the ETAG may be changed in above operations, hence reconcilePublicIP() would get lb again from cache.
	logger.V(2).Info("Reconciling PublicIPs")
	if _, err := az.reconcilePublicIPs(ctx, clusterName, updateService, ptr.Deref(lb.Name, ""), true /* wantLb */); err != nil {
		logger.Error(err, "Failed to reconcile PublicIPs")
		return nil, err
	}

	lbName := strings.ToLower(ptr.Deref(lb.Name, ""))
	key := strings.ToLower(getServiceName(service))
	if az.UseMultipleStandardLoadBalancers() && isLocalService(service) {
		az.localServiceNameToServiceInfoMap.Store(key, newServiceInfo(getServiceIPFamily(service), lbName))
		// There are chances that the endpointslice changes after EnsureHostsInPool, so
		// need to check endpointslice for a second time.
		if err := az.checkAndApplyLocalServiceBackendPoolUpdates(*lb, service); err != nil {
			logger.Error(err, "Failed to checkAndApplyLocalServiceBackendPoolUpdates")
			return nil, err
		}
	} else {
		az.localServiceNameToServiceInfoMap.Delete(key)
	}

	return lbStatus, nil
}

// EnsureLoadBalancer creates a new load balancer 'name', or updates the existing one. Returns the status of the balancer
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager.
//
// Implementations may return a (possibly wrapped) api.RetryError to enforce
// backing off at a fixed duration. This can be used for cases like when the
// load balancer is not ready yet (e.g., it is still being provisioned) and
// polling at a fixed rate is preferred over backing off exponentially in
// order to minimize latency.
func (az *Cloud) EnsureLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (lbStatus *v1.LoadBalancerStatus, err error) {
	// When a client updates the internal load balancer annotation,
	// the service may be switched from an internal LB to a public one, or vice versa.
	// Here we'll firstly ensure service do not lie in the opposite LB.
	const Operation = "EnsureLoadBalancer"

	ctx, span := trace.BeginReconcile(ctx, trace.DefaultTracer(), Operation, attributes.FeatureOfService(service)...)
	defer func() { span.Observe(ctx, err) }()

	// Serialize service reconcile process
	az.serviceReconcileLock.Lock()
	defer az.serviceReconcileLock.Unlock()

	var (
		svcName              = getServiceName(service)
		logger               = log.FromContextOrBackground(ctx).WithName(Operation).WithValues("cluster", clusterName, "service", svcName)
		mc                   = metrics.NewMetricContext("services", "ensure_loadbalancer", az.ResourceGroup, az.getNetworkResourceSubscriptionID(), svcName)
		isOperationSucceeded = false
	)

	if az.azureResourceLocker != nil {
		err = az.azureResourceLocker.Lock(ctx)
		if err != nil {
			logger.Error(err, "failed to lock azure resources")
			return nil, fmt.Errorf(
				consts.AzureResourceLockFailedToLockErrorTemplate,
				"EnsureLoadBalancer",
				err,
			)
		}

		defer func() {
			unlockErr := az.azureResourceLocker.Unlock(ctx)
			if unlockErr != nil {
				unlockErr = fmt.Errorf(
					consts.AzureResourceLockFailedToUnlockErrorTemplate,
					"EnsureLoadBalancer",
					consts.AzureResourceLockLeaseNamespace,
					consts.AzureResourceLockLeaseName,
					unlockErr,
				)
			}
			if err == nil {
				err = unlockErr
			} else if unlockErr != nil {
				err = fmt.Errorf(
					consts.AzureResourceLockFailedToReconcileWithUnlockErrorTemplate,
					"EnsureLoadBalancer",
					err,
					unlockErr,
				)
			}
		}()
	}

	logger.V(5).Info("Starting", "service-spec", log.ValueAsMap(service))

	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded)
		if err != nil {
			logger.V(5).Error(err, "Finished with error", "service-spec", log.ValueAsMap(service))
		} else {
			logger.V(5).Info("Finished", "service-spec", log.ValueAsMap(service))
		}
	}()

	lbStatus, err = az.reconcileService(ctx, clusterName, service, nodes)
	if err != nil {
		return nil, err
	}

	isOperationSucceeded = true
	return lbStatus, nil
}

func (az *Cloud) getLatestService(serviceName string, deepcopy bool) (*v1.Service, bool, error) {
	parts := strings.Split(serviceName, "/")
	ns, n := parts[0], parts[1]
	latestService, err := az.serviceLister.Services(ns).Get(n)
	switch {
	case apierrors.IsNotFound(err):
		// service absence in store means the service deletion is caught by watcher
		return nil, false, nil
	case err != nil:
		return nil, false, err
	default:
		if deepcopy {
			return latestService.DeepCopy(), true, nil
		}
		return latestService, true, nil
	}
}

// UpdateLoadBalancer updates hosts under the specified load balancer.
// Implementations must treat the *v1.Service and *v1.Node
// parameters as read-only and not modify them.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (az *Cloud) UpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) error {
	const Operation = "UpdateLoadBalancer"

	var err error
	ctx, span := trace.BeginReconcile(ctx, trace.DefaultTracer(), Operation, attributes.FeatureOfService(service)...)
	defer func() { span.Observe(ctx, err) }()

	// Serialize service reconcile process
	az.serviceReconcileLock.Lock()
	defer az.serviceReconcileLock.Unlock()

	var (
		svcName              = getServiceName(service)
		logger               = log.FromContextOrBackground(ctx).WithName(Operation).WithValues("cluster", clusterName, "service", svcName)
		mc                   = metrics.NewMetricContext("services", "update_loadbalancer", az.ResourceGroup, az.getNetworkResourceSubscriptionID(), svcName)
		isOperationSucceeded = false
	)

	logger.V(5).Info("Starting", "service-spec", log.ValueAsMap(service))
	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded)
		if err != nil {
			logger.V(5).Error(err, "Finished with error", "service-spec", log.ValueAsMap(service))
		} else {
			logger.V(5).Info("Finished", "service-spec", log.ValueAsMap(service))
		}
	}()

	// In case UpdateLoadBalancer gets stale service spec, retrieve the latest from lister
	service, serviceExists, err := az.getLatestService(svcName, true)
	if err != nil {
		return fmt.Errorf("UpdateLoadBalancer: failed to get latest service %s: %w", service.Name, err)
	}
	if !serviceExists {
		isOperationSucceeded = true
		logger.V(2).Info("Skipping because service is going to be deleted")
		return nil
	}

	shouldUpdateLB, err := az.shouldUpdateLoadBalancer(ctx, clusterName, service, nodes)
	if err != nil {
		return err
	}

	if !shouldUpdateLB {
		isOperationSucceeded = true
		logger.V(2).Info("Skipping because it is either being deleted or does not exist anymore")
		return nil
	}

	if az.azureResourceLocker != nil {
		err = az.azureResourceLocker.Lock(ctx)
		if err != nil {
			logger.Error(err, "failed to lock azure resources")
			return fmt.Errorf(
				consts.AzureResourceLockFailedToLockErrorTemplate,
				"UpdateLoadBalancer",
				err,
			)
		}

		defer func() {
			unlockErr := az.azureResourceLocker.Unlock(ctx)
			if unlockErr != nil {
				unlockErr = fmt.Errorf(
					consts.AzureResourceLockFailedToUnlockErrorTemplate,
					"UpdateLoadBalancer",
					consts.AzureResourceLockLeaseNamespace,
					consts.AzureResourceLockLeaseName,
					unlockErr,
				)
			}
			if err == nil {
				err = unlockErr
			} else if unlockErr != nil {
				err = fmt.Errorf(
					consts.AzureResourceLockFailedToReconcileWithUnlockErrorTemplate,
					"UpdateLoadBalancer",
					err,
					unlockErr,
				)
			}
		}()
	}

	_, err = az.reconcileService(ctx, clusterName, service, nodes)
	if err != nil {
		return err
	}

	isOperationSucceeded = true
	return nil
}

// EnsureLoadBalancerDeleted deletes the specified load balancer if it
// exists, returning nil if the load balancer specified either didn't exist or
// was successfully deleted.
// This construction is useful because many cloud providers' load balancers
// have multiple underlying components, meaning a Get could say that the LB
// doesn't exist even if some part of it is still laying around.
// Implementations must treat the *v1.Service parameter as read-only and not modify it.
// Parameter 'clusterName' is the name of the cluster as presented to kube-controller-manager
func (az *Cloud) EnsureLoadBalancerDeleted(ctx context.Context, clusterName string, service *v1.Service) (err error) {
	const Operation = "EnsureLoadBalancerDeleted"

	ctx, span := trace.BeginReconcile(ctx, trace.DefaultTracer(), Operation, attributes.FeatureOfService(service)...)
	defer func() { span.Observe(ctx, err) }()

	// Serialize service reconcile process
	az.serviceReconcileLock.Lock()
	defer az.serviceReconcileLock.Unlock()

	var (
		svcName              = getServiceName(service)
		logger               = log.FromContextOrBackground(ctx).WithName(Operation).WithValues("cluster", clusterName, "service", svcName)
		mc                   = metrics.NewMetricContext("services", "ensure_loadbalancer_deleted", az.ResourceGroup, az.getNetworkResourceSubscriptionID(), svcName)
		isOperationSucceeded = false
	)
	ctx = log.NewContext(ctx, logger)
	if az.azureResourceLocker != nil {
		err = az.azureResourceLocker.Lock(ctx)
		if err != nil {
			logger.Error(err, "failed to lock azure resources")
			return fmt.Errorf(
				consts.AzureResourceLockFailedToLockErrorTemplate,
				"EnsureLoadBalancerDeleted",
				err,
			)
		}

		defer func() {
			unlockErr := az.azureResourceLocker.Unlock(ctx)
			if unlockErr != nil {
				unlockErr = fmt.Errorf(
					consts.AzureResourceLockFailedToUnlockErrorTemplate,
					"EnsureLoadBalancerDeleted",
					consts.AzureResourceLockLeaseNamespace,
					consts.AzureResourceLockLeaseName,
					unlockErr,
				)
			}
			if err == nil {
				err = unlockErr
			} else if unlockErr != nil {
				err = fmt.Errorf(
					consts.AzureResourceLockFailedToReconcileWithUnlockErrorTemplate,
					"EnsureLoadBalancerDeleted",
					err,
					unlockErr,
				)
			}
		}()
	}

	logger.V(5).Info("Starting", "service-spec", log.ValueAsMap(service))
	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded)
		if err != nil {
			logger.Error(err, "Finished with error", "service-spec", log.ValueAsMap(service))
		} else {
			logger.V(5).Info("Finished", "service-spec", log.ValueAsMap(service))
		}
	}()

	lb, _, _, lbIPsPrimaryPIPs, _, _, err := az.getServiceLoadBalancer(ctx, service, clusterName, nil, false, []*armnetwork.LoadBalancer{})
	if err != nil && !errutils.HasStatusForbiddenOrIgnoredError(err) {
		return err
	}

	_, err = az.reconcileSecurityGroup(ctx, clusterName, service, ptr.Deref(lb.Name, ""), lbIPsPrimaryPIPs, false /* wantLb */)
	if err != nil {
		return err
	}

	_, needRetry, err := az.reconcileLoadBalancer(ctx, clusterName, service, nil, false /* wantLb */)
	if err != nil && !errutils.HasStatusForbiddenOrIgnoredError(err) {
		return err
	}
	if needRetry {
		_, _, err := az.reconcileLoadBalancer(ctx, clusterName, service, nil, false /* wantLb */)
		if err != nil && !errutils.HasStatusForbiddenOrIgnoredError(err) {
			return err
		}
	}

	// check flipped service also
	flippedService := flipServiceInternalAnnotation(service)
	if _, _, err := az.reconcileLoadBalancer(ctx, clusterName, flippedService, nil, false /* wantLb */); err != nil {
		return err
	}

	if _, err = az.reconcilePublicIPs(ctx, clusterName, service, "", false /* wantLb */); err != nil {
		return err
	}

	if az.UseMultipleStandardLoadBalancers() && isLocalService(service) {
		key := strings.ToLower(svcName)
		az.localServiceNameToServiceInfoMap.Delete(key)
	}

	isOperationSucceeded = true

	return nil
}

// GetLoadBalancerName returns the name of the load balancer. Implementations must treat the
// *v1.Service parameter as read-only and not modify it.
func (az *Cloud) GetLoadBalancerName(_ context.Context, _ string, service *v1.Service) string {
	return cloudprovider.DefaultLoadBalancerName(service)
}

func (az *Cloud) getLoadBalancerResourceGroup() string {
	if az.LoadBalancerResourceGroup != "" {
		return az.LoadBalancerResourceGroup
	}

	return az.ResourceGroup
}

// shouldChangeLoadBalancer determines if the load balancer of the service should be switched to another one
// according to the mode annotation on the service. This could be happened when the LB selection mode of an
// existing service is changed to another VMSS/VMAS.
func (az *Cloud) shouldChangeLoadBalancer(service *v1.Service, currLBName, clusterName, expectedLBName string) bool {
	// The load balancer can be changed in two cases:
	// 1. Using multiple standard load balancers.
	// 2. Migrate from multiple standard load balancers to single standard load balancer.
	if az.UseStandardLoadBalancer() {
		if !strings.EqualFold(currLBName, expectedLBName) {
			klog.V(2).Infof("shouldChangeLoadBalancer(%s, %s, %s): change the LB to another one %s", service.Name, currLBName, clusterName, expectedLBName)
			return true
		}
		return false
	}

	// basic LB
	hasMode, isAuto, vmSetName := az.getServiceLoadBalancerMode(service)

	// if no mode is given or the mode is `__auto__`, the current LB should be kept
	if !hasMode || isAuto {
		return false
	}

	lbName := trimSuffixIgnoreCase(currLBName, consts.InternalLoadBalancerNameSuffix)
	// change the LB from vmSet dedicated to primary if the vmSet becomes the primary one
	if strings.EqualFold(lbName, vmSetName) {
		if !strings.EqualFold(lbName, clusterName) &&
			strings.EqualFold(az.VMSet.GetPrimaryVMSetName(), vmSetName) {
			klog.V(2).Infof("shouldChangeLoadBalancer(%s, %s, %s): change the LB to another one", service.Name, currLBName, clusterName)
			return true
		}
		return false
	}
	if strings.EqualFold(vmSetName, az.VMSet.GetPrimaryVMSetName()) && strings.EqualFold(clusterName, lbName) {
		return false
	}

	// if the VMSS/VMAS of the current LB is different from the mode, change the LB
	// to another one
	klog.V(2).Infof("shouldChangeLoadBalancer(%s, %s, %s): change the LB to another one", service.Name, currLBName, clusterName)
	return true
}

// removeFrontendIPConfigurationFromLoadBalancer removes the given ip configs from the load balancer
// and delete the load balancer if there is no ip config on it. It returns the name of the deleted load balancer
// and it will be used in reconcileLoadBalancer to remove the load balancer from the list.
func (az *Cloud) removeFrontendIPConfigurationFromLoadBalancer(ctx context.Context, lb *armnetwork.LoadBalancer, existingLBs []*armnetwork.LoadBalancer, fips []*armnetwork.FrontendIPConfiguration, clusterName string, service *v1.Service) (string, bool /* deleted PLS */, error) {
	if lb == nil || lb.Properties == nil || lb.Properties.FrontendIPConfigurations == nil {
		return "", false, nil
	}
	fipConfigs := lb.Properties.FrontendIPConfigurations
	for i, fipConfig := range fipConfigs {
		for _, fip := range fips {
			if strings.EqualFold(ptr.Deref(fipConfig.Name, ""), ptr.Deref(fip.Name, "")) {
				fipConfigs = append(fipConfigs[:i], fipConfigs[i+1:]...)
				break
			}
		}
	}
	lb.Properties.FrontendIPConfigurations = fipConfigs

	// also remove the corresponding rules/probes
	if lb.Properties.LoadBalancingRules != nil {
		lbRules := lb.Properties.LoadBalancingRules
		for i := len(lbRules) - 1; i >= 0; i-- {
			for _, fip := range fips {
				if strings.Contains(ptr.Deref(lbRules[i].Name, ""), ptr.Deref(fip.Name, "")) {
					lbRules = append(lbRules[:i], lbRules[i+1:]...)
				}
			}
		}
		lb.Properties.LoadBalancingRules = lbRules
	}
	if lb.Properties.Probes != nil {
		lbProbes := lb.Properties.Probes
		for i := len(lbProbes) - 1; i >= 0; i-- {
			for _, fip := range fips {
				if strings.Contains(ptr.Deref(lbProbes[i].Name, ""), ptr.Deref(fip.Name, "")) {
					lbProbes = append(lbProbes[:i], lbProbes[i+1:]...)
				}
			}
		}
		lb.Properties.Probes = lbProbes
	}

	// PLS does not support IPv6 so there will not be additional API calls.
	var deletedPLS bool
	for _, fip := range fips {
		// clean up any private link service associated with the frontEndIPConfig
		var (
			deleted bool
			err     error
		)
		if deleted, err = az.reconcilePrivateLinkService(ctx, clusterName, service, fip, false /* wantPLS */); err != nil {
			klog.Errorf("removeFrontendIPConfigurationFromLoadBalancer(%s, %s, %s, %s): failed to clean up PLS: %v", ptr.Deref(lb.Name, ""), ptr.Deref(fip.Name, ""), clusterName, service.Name, err)
			return "", false, err
		}
		if deleted {
			deletedPLS = true
		}
	}
	if deletedPLS {
		return "", true, nil
	}

	var deletedLBName string
	fipNames := []string{}
	for _, fip := range fips {
		fipNames = append(fipNames, ptr.Deref(fip.Name, ""))
	}
	logPrefix := fmt.Sprintf("removeFrontendIPConfigurationFromLoadBalancer(%s, %q, %s, %s)", ptr.Deref(lb.Name, ""), fipNames, clusterName, service.Name)
	if len(fipConfigs) == 0 {
		klog.V(2).Infof("%s: deleting load balancer because there is no remaining frontend IP configurations", logPrefix)
		err := az.cleanOrphanedLoadBalancer(ctx, lb, existingLBs, service, clusterName)
		if err != nil {
			klog.Errorf("%s: failed to cleanupOrphanedLoadBalancer: %v", logPrefix, err)
			return "", false, err
		}
		deletedLBName = ptr.Deref(lb.Name, "")
	} else {
		klog.V(2).Infof("%s: updating the load balancer", logPrefix)
		err := az.CreateOrUpdateLB(ctx, service, *lb)
		if err != nil {
			klog.Errorf("%s: failed to CreateOrUpdateLB: %v", logPrefix, err)
			return "", false, err
		}
		_ = az.lbCache.Delete(ptr.Deref(lb.Name, ""))
	}
	return deletedLBName, false, nil
}

func (az *Cloud) cleanOrphanedLoadBalancer(ctx context.Context, lb *armnetwork.LoadBalancer, existingLBs []*armnetwork.LoadBalancer, service *v1.Service, clusterName string) error {
	lbName := ptr.Deref(lb.Name, "")
	serviceName := getServiceName(service)
	isBackendPoolPreConfigured := az.isBackendPoolPreConfigured(service)
	v4Enabled, v6Enabled := getIPFamiliesEnabled(service)
	lbBackendPoolIDs := az.getBackendPoolIDs(clusterName, lbName)
	lbBackendPoolIDsToDelete := []string{}
	if v4Enabled {
		lbBackendPoolIDsToDelete = append(lbBackendPoolIDsToDelete, lbBackendPoolIDs[consts.IPVersionIPv4])
	}
	if v6Enabled {
		lbBackendPoolIDsToDelete = append(lbBackendPoolIDsToDelete, lbBackendPoolIDs[consts.IPVersionIPv6])
	}
	if isBackendPoolPreConfigured {
		klog.V(2).Infof("cleanOrphanedLoadBalancer(%s, %s, %s): ignore cleanup of dirty lb because the lb is pre-configured", lbName, serviceName, clusterName)
	} else {
		foundLB := false
		for _, existingLB := range existingLBs {
			if strings.EqualFold(ptr.Deref(lb.Name, ""), ptr.Deref(existingLB.Name, "")) {
				foundLB = true
				break
			}
		}
		if !foundLB {
			klog.V(2).Infof("cleanOrphanedLoadBalancer: the LB %s doesn't exist, will not delete it", ptr.Deref(lb.Name, ""))
			return nil
		}

		// When FrontendIPConfigurations is empty, we need to delete the Azure load balancer resource itself,
		// because an Azure load balancer cannot have an empty FrontendIPConfigurations collection
		klog.V(2).Infof("cleanOrphanedLoadBalancer(%s, %s, %s): deleting the LB since there are no remaining frontendIPConfigurations", lbName, serviceName, clusterName)

		// Remove backend pools from vmSets. This is required for virtual machine scale sets before removing the LB.
		if _, ok := az.VMSet.(*availabilitySet); ok {
			// do nothing for availability set
			lb.Properties.BackendAddressPools = nil
		}

		if deleteErr := az.safeDeleteLoadBalancer(ctx, *lb, clusterName, service); deleteErr != nil {
			klog.Warningf("cleanOrphanedLoadBalancer(%s, %s, %s): failed to DeleteLB: %v", lbName, serviceName, clusterName, deleteErr)

			rgName, vmssName, parseErr := errutils.GetVMSSMetadataByRawError(deleteErr)
			if parseErr != nil {
				klog.Warningf("cleanOrphanedLoadBalancer(%s, %s, %s): failed to parse error: %v", lbName, serviceName, clusterName, parseErr)
				return deleteErr
			}
			if rgName == "" || vmssName == "" {
				klog.Warningf("cleanOrphanedLoadBalancer(%s, %s, %s): empty rgName or vmssName", lbName, serviceName, clusterName)
				return deleteErr
			}

			// if we reach here, it means the VM couldn't be deleted because it is being referenced by a VMSS
			if _, ok := az.VMSet.(*ScaleSet); !ok {
				klog.Warningf("cleanOrphanedLoadBalancer(%s, %s, %s): unexpected VMSet type, expected VMSS", lbName, serviceName, clusterName)
				return deleteErr
			}

			if !strings.EqualFold(rgName, az.ResourceGroup) {
				return fmt.Errorf("cleanOrphanedLoadBalancer(%s, %s, %s): the VMSS %s is in the resource group %s, but is referencing the LB in %s", lbName, serviceName, clusterName, vmssName, rgName, az.ResourceGroup)
			}

			vmssNamesMap := map[string]bool{vmssName: true}
			if err := az.VMSet.EnsureBackendPoolDeletedFromVMSets(ctx, vmssNamesMap, lbBackendPoolIDsToDelete); err != nil {
				klog.Errorf("cleanOrphanedLoadBalancer(%s, %s, %s): failed to EnsureBackendPoolDeletedFromVMSets: %v", lbName, serviceName, clusterName, err)
				return err
			}

			if deleteErr := az.DeleteLB(ctx, service, lbName); deleteErr != nil {
				klog.Errorf("cleanOrphanedLoadBalancer(%s, %s, %s): failed delete lb for the second time, stop retrying: %v", lbName, serviceName, clusterName, deleteErr)
				return deleteErr
			}
		}
		klog.V(10).Infof("cleanOrphanedLoadBalancer(%s, %s, %s): az.DeleteLB finished", lbName, serviceName, clusterName)
	}
	return nil
}

// safeDeleteLoadBalancer deletes the load balancer after decoupling it from the vmSet
func (az *Cloud) safeDeleteLoadBalancer(ctx context.Context, lb armnetwork.LoadBalancer, clusterName string, service *v1.Service) error {
	vmSetName := az.mapLoadBalancerNameToVMSet(ptr.Deref(lb.Name, ""), clusterName)
	lbBackendPoolIDsToDelete := []string{}
	if lb.Properties != nil && lb.Properties.BackendAddressPools != nil {
		for _, bp := range lb.Properties.BackendAddressPools {
			lbBackendPoolIDsToDelete = append(lbBackendPoolIDsToDelete, ptr.Deref(bp.ID, ""))
		}
	}
	if _, err := az.VMSet.EnsureBackendPoolDeleted(ctx, service, lbBackendPoolIDsToDelete, vmSetName, lb.Properties.BackendAddressPools, true); err != nil {
		return fmt.Errorf("safeDeleteLoadBalancer: failed to EnsureBackendPoolDeleted: %w", err)
	}

	klog.V(2).Infof("safeDeleteLoadBalancer: deleting LB %s", ptr.Deref(lb.Name, ""))
	if rerr := az.DeleteLB(ctx, service, ptr.Deref(lb.Name, "")); rerr != nil {
		return rerr
	}
	_ = az.lbCache.Delete(ptr.Deref(lb.Name, ""))

	// Remove corresponding nodes in ActiveNodes and nodesWithCorrectLoadBalancerByPrimaryVMSet.
	for i := range az.MultipleStandardLoadBalancerConfigurations {
		if strings.EqualFold(
			trimSuffixIgnoreCase(ptr.Deref(lb.Name, ""), consts.InternalLoadBalancerNameSuffix),
			az.MultipleStandardLoadBalancerConfigurations[i].Name,
		) {
			for _, nodeName := range az.MultipleStandardLoadBalancerConfigurations[i].ActiveNodes.UnsortedList() {
				az.nodesWithCorrectLoadBalancerByPrimaryVMSet.Delete(nodeName)
			}
			az.MultipleStandardLoadBalancerConfigurations[i].ActiveNodes = utilsets.NewString()
			break
		}
	}

	return nil
}

// getServiceLoadBalancer gets the loadbalancer for the service if it already exists.
// If wantLb is TRUE then -it selects a new load balancer.
// In case the selected load balancer does not exist it returns network.LoadBalancer struct
// with added metadata (such as name, location) and existsLB set to FALSE.
// By default - cluster default LB is returned.
func (az *Cloud) getServiceLoadBalancer(
	ctx context.Context,
	service *v1.Service,
	clusterName string,
	nodes []*v1.Node,
	wantLb bool,
	existingLBs []*armnetwork.LoadBalancer,
) (lb *armnetwork.LoadBalancer, refreshedLBs []*armnetwork.LoadBalancer, status *v1.LoadBalancerStatus, lbIPsPrimaryPIPs []string, exists, deletedPLS bool, err error) {
	logger := log.FromContextOrBackground(ctx)

	isInternal := requiresInternalLoadBalancer(service)
	var defaultLB *armnetwork.LoadBalancer
	primaryVMSetName := az.VMSet.GetPrimaryVMSetName()
	defaultLBName, err := az.getAzureLoadBalancerName(ctx, service, existingLBs, clusterName, primaryVMSetName, isInternal)
	if err != nil {
		return nil, nil, nil, nil, false, false, err
	}

	// reuse the lb list from reconcileSharedLoadBalancer to reduce the api call
	if len(existingLBs) == 0 {
		lbs, err := az.ListLB(ctx, service)
		if err != nil {
			return nil, nil, nil, nil, false, false, err
		}
		existingLBs = lbs
	}

	// check if the service already has a load balancer
	var shouldChangeLB bool
	for i := range existingLBs {
		existingLB := (existingLBs)[i]

		if strings.EqualFold(*existingLB.Name, defaultLBName) {
			defaultLB = existingLB
		}
		if isInternalLoadBalancer(existingLB) != isInternal {
			continue
		}

		var fipConfigs []*armnetwork.FrontendIPConfiguration
		status, lbIPsPrimaryPIPs, fipConfigs, err = az.getServiceLoadBalancerStatus(ctx, service, existingLB)
		if err != nil {
			return nil, nil, nil, nil, false, false, err
		}
		if status == nil {
			// service is not on this load balancer
			continue
		}
		logger.V(4).Info(fmt.Sprintf("getServiceLoadBalancer(%s, %s, %v): current lb IPs: %q", service.Name, clusterName, wantLb, lbIPsPrimaryPIPs))

		// select another load balancer instead of returning
		// the current one if the change is needed
		var (
			deletedLBName string
			err           error
		)
		if wantLb && az.shouldChangeLoadBalancer(service, ptr.Deref(existingLB.Name, ""), clusterName, defaultLBName) {
			shouldChangeLB = true
			fipConfigNames := []string{}
			for _, fipConfig := range fipConfigs {
				fipConfigNames = append(fipConfigNames, ptr.Deref(fipConfig.Name, ""))
			}
			deletedLBName, deletedPLS, err = az.removeFrontendIPConfigurationFromLoadBalancer(ctx, existingLB, existingLBs, fipConfigs, clusterName, service)
			if err != nil {
				logger.Error(err, fmt.Sprintf("getServiceLoadBalancer(%s, %s, %v): failed to remove frontend IP configurations %q from load balancer", service.Name, clusterName, wantLb, fipConfigNames))
				return nil, nil, nil, nil, false, false, err
			}
			if deletedPLS {
				return nil, nil, nil, nil, false, true, nil
			}
			if deletedLBName != "" {
				removeLBFromList(&existingLBs, deletedLBName)
			}
			az.reconcileMultipleStandardLoadBalancerConfigurationStatus(
				false,
				getServiceName(service),
				ptr.Deref(existingLB.Name, ""),
			)

			if isLocalService(service) && az.UseMultipleStandardLoadBalancers() {
				// No need for the endpoint slice informer to update the backend pool
				// for the service because the main loop will delete the old backend pool
				// and create a new one in the new load balancer.
				svcName := getServiceName(service)
				if az.backendPoolUpdater != nil {
					az.backendPoolUpdater.removeOperation(svcName)
				}

				// Remove backend pools on the previous load balancer for the local service
				if deletedLBName == "" {
					newLBs, err := az.cleanupLocalServiceBackendPool(ctx, service, nodes, existingLBs, clusterName)
					if err != nil {
						logger.Error(err, fmt.Sprintf("getServiceLoadBalancer(%s, %s, %v): failed to cleanup backend pool for local service", service.Name, clusterName, wantLb))
						return nil, nil, nil, nil, false, false, err
					}
					existingLBs = newLBs
				}
			}
			break
		}

		return existingLB, existingLBs, status, lbIPsPrimaryPIPs, true, false, nil
	}

	// Service does not have a load balancer, select one.
	// Single standard load balancer doesn't need this because
	// all backends nodes should be added to same LB.
	if wantLb && !az.UseStandardLoadBalancer() {
		// select new load balancer for service
		selectedLB, exists, err := az.selectLoadBalancer(ctx, clusterName, service, existingLBs, nodes)
		if err != nil {
			return nil, existingLBs, nil, nil, false, false, err
		}

		return selectedLB, existingLBs, status, lbIPsPrimaryPIPs, exists, false, err
	}

	// If the service moves to a different load balancer, return the one
	// instead of creating a new load balancer if it exists.
	if shouldChangeLB {
		for _, existingLB := range existingLBs {
			if strings.EqualFold(ptr.Deref(existingLB.Name, ""), defaultLBName) {
				return existingLB, existingLBs, status, lbIPsPrimaryPIPs, true, false, nil
			}
		}
	}

	// create a default LB with meta data if not present
	if defaultLB == nil {
		defaultLB = &armnetwork.LoadBalancer{
			Name:       &defaultLBName,
			Location:   &az.Location,
			Properties: &armnetwork.LoadBalancerPropertiesFormat{},
		}
		if az.UseStandardLoadBalancer() {
			defaultLB.SKU = &armnetwork.LoadBalancerSKU{
				Name: to.Ptr(armnetwork.LoadBalancerSKUNameStandard),
			}
		}
		if az.HasExtendedLocation() {
			var typ *armnetwork.ExtendedLocationTypes
			if getExtendedLocationTypeFromString(az.ExtendedLocationType) == armnetwork.ExtendedLocationTypesEdgeZone {
				typ = to.Ptr(armnetwork.ExtendedLocationTypesEdgeZone)
			}
			defaultLB.ExtendedLocation = &armnetwork.ExtendedLocation{
				Name: &az.ExtendedLocationName,
				Type: typ,
			}
		}
	}

	return defaultLB, existingLBs, nil, nil, false, false, nil
}

// selectLoadBalancer selects load balancer for the service in the cluster.
// The selection algorithm selects the load balancer which currently has
// the minimum lb rules. If there are multiple LBs with same number of rules,
// then selects the first one (sorted based on name).
// Note: this function is only useful for basic LB clusters.
func (az *Cloud) selectLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, existingLBs []*armnetwork.LoadBalancer, nodes []*v1.Node) (selectedLB *armnetwork.LoadBalancer, existsLb bool, err error) {
	isInternal := requiresInternalLoadBalancer(service)
	serviceName := getServiceName(service)
	klog.V(2).Infof("selectLoadBalancer for service (%s): isInternal(%v) - start", serviceName, isInternal)
	vmSetNames, err := az.VMSet.GetVMSetNames(ctx, service, nodes)
	if err != nil {
		klog.Errorf("az.selectLoadBalancer: cluster(%s) service(%s) isInternal(%t) - az.GetVMSetNames failed, err=(%v)", clusterName, serviceName, isInternal, err)
		return nil, false, err
	}
	klog.V(2).Infof("selectLoadBalancer: cluster(%s) service(%s) isInternal(%t) - vmSetNames %v", clusterName, serviceName, isInternal, vmSetNames)

	mapExistingLBs := map[string]*armnetwork.LoadBalancer{}
	for _, lb := range existingLBs {
		mapExistingLBs[*lb.Name] = lb
	}
	selectedLBRuleCount := math.MaxInt32
	for _, currVMSetName := range vmSetNames {
		currLBName, _ := az.getAzureLoadBalancerName(ctx, service, existingLBs, clusterName, *currVMSetName, isInternal)
		lb, exists := mapExistingLBs[currLBName]
		if !exists {
			// select this LB as this is a new LB and will have minimum rules
			// create tmp lb struct to hold metadata for the new load-balancer
			var loadBalancerSKU *armnetwork.LoadBalancerSKUName
			if az.UseStandardLoadBalancer() {
				loadBalancerSKU = to.Ptr(armnetwork.LoadBalancerSKUNameStandard)
			} else {
				loadBalancerSKU = to.Ptr(armnetwork.LoadBalancerSKUNameBasic)
			}
			selectedLB = &armnetwork.LoadBalancer{
				Name:       &currLBName,
				Location:   &az.Location,
				SKU:        &armnetwork.LoadBalancerSKU{Name: loadBalancerSKU},
				Properties: &armnetwork.LoadBalancerPropertiesFormat{},
			}
			if az.HasExtendedLocation() {
				var typ *armnetwork.ExtendedLocationTypes
				if getExtendedLocationTypeFromString(az.ExtendedLocationType) == armnetwork.ExtendedLocationTypesEdgeZone {
					typ = to.Ptr(armnetwork.ExtendedLocationTypesEdgeZone)
				}
				selectedLB.ExtendedLocation = &armnetwork.ExtendedLocation{
					Name: &az.ExtendedLocationName,
					Type: typ,
				}
			}

			return selectedLB, false, nil
		}

		lbRules := lb.Properties.LoadBalancingRules
		currLBRuleCount := 0
		if lbRules != nil {
			currLBRuleCount = len(lbRules)
		}
		if currLBRuleCount < selectedLBRuleCount {
			selectedLBRuleCount = currLBRuleCount
			selectedLB = lb
		}
	}

	if selectedLB == nil {
		err = fmt.Errorf("selectLoadBalancer: cluster(%s) service(%s) isInternal(%t) - unable to find load balancer for selected VM sets %v", clusterName, serviceName, isInternal, vmSetNames)
		klog.Error(err)
		return nil, false, err
	}
	// validate if the selected LB has not exceeded the MaximumLoadBalancerRuleCount
	if az.Config.MaximumLoadBalancerRuleCount != 0 && selectedLBRuleCount >= az.Config.MaximumLoadBalancerRuleCount {
		err = fmt.Errorf("selectLoadBalancer: cluster(%s) service(%s) isInternal(%t) -  all available load balancers have exceeded maximum rule limit %d, vmSetNames (%v)", clusterName, serviceName, isInternal, selectedLBRuleCount, vmSetNames)
		klog.Error(err)
		return selectedLB, existsLb, err
	}

	return selectedLB, existsLb, nil
}

// getServiceLoadBalancerStatus returns LB status for the Service.
// Before DualStack support, old logic takes the first ingress IP as non-additional one
// and the second one as additional one. With DualStack support, the second IP may be
// the IP of another IP family so the new logic returns two variables.
func (az *Cloud) getServiceLoadBalancerStatus(ctx context.Context, service *v1.Service, lb *armnetwork.LoadBalancer) (status *v1.LoadBalancerStatus, lbIPsPrimaryPIPs []string, fipConfigs []*armnetwork.FrontendIPConfiguration, err error) {
	if lb == nil {
		klog.V(10).Info("getServiceLoadBalancerStatus: lb is nil")
		return nil, nil, nil, nil
	}
	if lb.Properties == nil || len(lb.Properties.FrontendIPConfigurations) == 0 {
		klog.V(10).Info("getServiceLoadBalancerStatus: lb.Properties.FrontendIPConfigurations is nil")
		return nil, nil, nil, nil
	}

	isInternal := requiresInternalLoadBalancer(service)
	serviceName := getServiceName(service)
	lbIngresses := []v1.LoadBalancerIngress{}
	for i := range lb.Properties.FrontendIPConfigurations {
		ipConfiguration := lb.Properties.FrontendIPConfigurations[i]
		owns, isPrimaryService, _ := az.serviceOwnsFrontendIP(ctx, ipConfiguration, service)
		if owns {
			klog.V(2).Infof("get(%s): lb(%s) - found frontend IP config, primary service: %v", serviceName, ptr.Deref(lb.Name, ""), isPrimaryService)

			var lbIP *string
			if isInternal {
				lbIP = ipConfiguration.Properties.PrivateIPAddress
			} else {
				if ipConfiguration.Properties.PublicIPAddress == nil {
					return nil, nil, nil, fmt.Errorf("get(%s): lb(%s) - failed to get LB PublicIPAddress is Nil", serviceName, *lb.Name)
				}
				pipID := ipConfiguration.Properties.PublicIPAddress.ID
				if pipID == nil {
					return nil, nil, nil, fmt.Errorf("get(%s): lb(%s) - failed to get LB PublicIPAddress ID is Nil", serviceName, *lb.Name)
				}
				pipName, err := getLastSegment(*pipID, "/")
				if err != nil {
					return nil, nil, nil, fmt.Errorf("get(%s): lb(%s) - failed to get LB PublicIPAddress Name from ID(%s)", serviceName, *lb.Name, *pipID)
				}
				pip, existsPip, err := az.getPublicIPAddress(ctx, az.getPublicIPAddressResourceGroup(service), pipName, azcache.CacheReadTypeDefault)
				if err != nil {
					return nil, nil, nil, err
				}
				if existsPip {
					lbIP = pip.Properties.IPAddress
				}
			}

			klog.V(2).Infof("getServiceLoadBalancerStatus gets ingress IP %q from frontendIPConfiguration %q for service %q", ptr.Deref(lbIP, ""), ptr.Deref(ipConfiguration.Name, ""), serviceName)

			lbIngresses = append(lbIngresses, v1.LoadBalancerIngress{IP: ptr.Deref(lbIP, "")})
			lbIPsPrimaryPIPs = append(lbIPsPrimaryPIPs, ptr.Deref(lbIP, ""))
			fipConfigs = append(fipConfigs, ipConfiguration)
		}
	}
	if len(lbIngresses) == 0 {
		return nil, nil, nil, nil
	}

	// set additional public IPs to LoadBalancerStatus, so that kube-proxy would create their iptables rules.
	additionalIPs, err := loadbalancer.AdditionalPublicIPs(service)
	if err != nil {
		return &v1.LoadBalancerStatus{Ingress: lbIngresses}, lbIPsPrimaryPIPs, fipConfigs, err
	}
	if len(additionalIPs) > 0 {
		for _, pip := range additionalIPs {
			lbIngresses = append(lbIngresses, v1.LoadBalancerIngress{
				IP: pip.String(),
			})
		}
	}
	return &v1.LoadBalancerStatus{Ingress: lbIngresses}, lbIPsPrimaryPIPs, fipConfigs, nil
}

func (az *Cloud) determinePublicIPName(ctx context.Context, clusterName string, service *v1.Service, isIPv6 bool) (string, bool, error) {
	if name := getServicePIPName(service, isIPv6); name != "" {
		return name, true, nil
	}

	pipResourceGroup := az.getPublicIPAddressResourceGroup(service)
	if id := getServicePIPPrefixID(service, isIPv6); id != "" {
		pipName, err := az.getPublicIPName(clusterName, service, isIPv6)
		return pipName, false, err
	}

	loadBalancerIP := getServiceLoadBalancerIP(service, isIPv6)

	// Assume that the service without loadBalancerIP set is a primary service.
	// If a secondary service doesn't set the loadBalancerIP, it is not allowed to share the IP.
	if len(loadBalancerIP) == 0 {
		pipName, err := az.getPublicIPName(clusterName, service, isIPv6)
		return pipName, false, err
	}

	// For the services with loadBalancerIP set, an existing public IP is required, primary
	// or secondary, or a public IP not found error would be reported.
	pip, err := az.findMatchedPIP(ctx, loadBalancerIP, "", pipResourceGroup)
	if err != nil {
		return "", false, err
	}

	if pip != nil && pip.Name != nil {
		return *pip.Name, false, nil
	}

	return "", false, fmt.Errorf("user supplied IP Address %s was not found in resource group %s", loadBalancerIP, pipResourceGroup)
}

func flipServiceInternalAnnotation(service *v1.Service) *v1.Service {
	copyService := service.DeepCopy()
	if copyService.Annotations == nil {
		copyService.Annotations = map[string]string{}
	}
	if v, ok := copyService.Annotations[consts.ServiceAnnotationLoadBalancerInternal]; ok && v == consts.TrueAnnotationValue {
		// If it is internal now, we make it external by remove the annotation
		delete(copyService.Annotations, consts.ServiceAnnotationLoadBalancerInternal)
	} else {
		// If it is external now, we make it internal
		copyService.Annotations[consts.ServiceAnnotationLoadBalancerInternal] = consts.TrueAnnotationValue
	}
	return copyService
}

func updateServiceLoadBalancerIPs(service *v1.Service, serviceIPs []string) *v1.Service {
	copyService := service.DeepCopy()
	if copyService != nil {
		for _, serviceIP := range serviceIPs {
			setServiceLoadBalancerIP(copyService, serviceIP)
		}
	}
	return copyService
}

func (az *Cloud) ensurePublicIPExists(ctx context.Context, service *v1.Service, pipName string, domainNameLabel, clusterName string, shouldPIPExisted, foundDNSLabelAnnotation, isIPv6 bool) (*armnetwork.PublicIPAddress, error) {
	pipResourceGroup := az.getPublicIPAddressResourceGroup(service)
	pip, existsPip, err := az.getPublicIPAddress(ctx, pipResourceGroup, pipName, azcache.CacheReadTypeDefault)
	if err != nil {
		return nil, err
	}
	serviceName := getServiceName(service)
	ipVersion := to.Ptr(armnetwork.IPVersionIPv4)
	if isIPv6 {
		ipVersion = to.Ptr(armnetwork.IPVersionIPv6)
	}

	var changed, owns, isUserAssignedPIP bool
	if existsPip {
		// ensure that the service tag is good for managed pips
		owns, isUserAssignedPIP = serviceOwnsPublicIP(service, pip, clusterName)
		if owns && !isUserAssignedPIP {
			changed, err = bindServicesToPIP(pip, []string{serviceName}, false)
			if err != nil {
				return nil, err
			}
		}

		if pip.Tags == nil {
			pip.Tags = make(map[string]*string)
		}

		if az.UseStandardLoadBalancer() {
			if pip.SKU == nil {
				pip.SKU = &armnetwork.PublicIPAddressSKU{
					Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
				}
				changed = true
			} else if !strings.EqualFold(string(*pip.SKU.Name), string(armnetwork.PublicIPAddressSKUNameStandard)) {
				pip.SKU.Name = ptr.To(armnetwork.PublicIPAddressSKUNameStandard)
				changed = true
			}
		}

		// return if pip exist and dns label is the same
		if strings.EqualFold(getDomainNameLabel(pip), domainNameLabel) {
			if existingServiceName := getServiceFromPIPDNSTags(pip.Tags); existingServiceName != "" && strings.EqualFold(existingServiceName, serviceName) {
				klog.V(6).Infof("ensurePublicIPExists for service(%s): pip(%s) - "+
					"the service is using the DNS label on the public IP", serviceName, pipName)

				var err error
				if changed {
					klog.V(2).Infof("ensurePublicIPExists: updating the PIP %s for the incoming service %s", pipName, serviceName)
					err = az.CreateOrUpdatePIP(service, pipResourceGroup, pip)
					if err != nil {
						return nil, err
					}
					pip, err = az.NetworkClientFactory.GetPublicIPAddressClient().Get(ctx, pipResourceGroup, *pip.Name, nil)
					if err != nil {
						return nil, err
					}
				}

				return pip, nil
			}
		}

		klog.V(2).Infof("ensurePublicIPExists for service(%s): pip(%s) - updating", serviceName, ptr.Deref(pip.Name, ""))
		if pip.Properties == nil {
			pip.Properties = &armnetwork.PublicIPAddressPropertiesFormat{
				PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
				PublicIPAddressVersion:   ipVersion,
			}
			changed = true
		}
	} else {
		if shouldPIPExisted {
			return nil, fmt.Errorf("PublicIP from annotation azure-pip-name(-IPv6)=%s for service %s doesn't exist", pipName, serviceName)
		}

		changed = true
		pip = &armnetwork.PublicIPAddress{
			Name:     ptr.To(pipName),
			Location: ptr.To(az.Location),
		}
		if az.HasExtendedLocation() {
			klog.V(2).Infof("Using extended location with name %s, and type %s for PIP", az.ExtendedLocationName, az.ExtendedLocationType)
			var typ *armnetwork.ExtendedLocationTypes
			if getExtendedLocationTypeFromString(az.ExtendedLocationType) == armnetwork.ExtendedLocationTypesEdgeZone {
				typ = to.Ptr(armnetwork.ExtendedLocationTypesEdgeZone)
			}
			pip.ExtendedLocation = &armnetwork.ExtendedLocation{
				Name: &az.ExtendedLocationName,
				Type: typ,
			}
		}
		pip.Properties = &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
			PublicIPAddressVersion:   ipVersion,
			IPTags:                   getServiceIPTagRequestForPublicIP(service).IPTags,
		}
		pip.Tags = map[string]*string{
			consts.ServiceTagKey:  ptr.To(""),
			consts.ClusterNameKey: &clusterName,
		}
		if _, err = bindServicesToPIP(pip, []string{serviceName}, false); err != nil {
			return nil, err
		}

		if az.UseStandardLoadBalancer() {
			pip.SKU = &armnetwork.PublicIPAddressSKU{
				Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
			}

			if id := getServicePIPPrefixID(service, isIPv6); id != "" {
				pip.Properties.PublicIPPrefix = &armnetwork.SubResource{ID: ptr.To(id)}
			}

			// skip adding zone info since edge zones doesn't support multiple availability zones.
			if !az.HasExtendedLocation() {
				// only add zone information for the new standard pips
				zones, err := az.getRegionZonesBackoff(ctx, ptr.Deref(pip.Location, ""))
				if err != nil {
					return nil, err
				}
				if len(zones) > 0 {
					pip.Zones = zones
				}
			}
		}
		klog.V(2).Infof("ensurePublicIPExists for service(%s): pip(%s) - creating", serviceName, *pip.Name)
	}
	if !isUserAssignedPIP && az.ensurePIPTagged(service, pip) {
		changed = true
	}

	if foundDNSLabelAnnotation {
		updatedDNSSettings, err := reconcileDNSSettings(pip, domainNameLabel, serviceName, pipName, isUserAssignedPIP)
		if err != nil {
			return nil, fmt.Errorf("ensurePublicIPExists for service(%s): failed to reconcileDNSSettings: %w", serviceName, err)
		}

		if updatedDNSSettings {
			changed = true
		}
	}

	// use the same family as the clusterIP as we support IPv6 single stack as well
	// as dual-stack clusters
	updatedIPSettings := az.reconcileIPSettings(pip, service, isIPv6)
	if updatedIPSettings {
		changed = true
	}

	if changed {
		klog.V(2).Infof("CreateOrUpdatePIP(%s, %q): start", pipResourceGroup, *pip.Name)
		err = az.CreateOrUpdatePIP(service, pipResourceGroup, pip)
		if err != nil {
			klog.V(2).Infof("ensure(%s) abort backoff: pip(%s)", serviceName, *pip.Name)
			return nil, err
		}

		klog.V(10).Infof("CreateOrUpdatePIP(%s, %q): end", pipResourceGroup, *pip.Name)
	}

	pip, rerr := az.NetworkClientFactory.GetPublicIPAddressClient().Get(ctx, pipResourceGroup, *pip.Name, nil)
	if rerr != nil {
		return nil, rerr
	}
	return pip, nil
}

func (az *Cloud) reconcileIPSettings(pip *armnetwork.PublicIPAddress, service *v1.Service, isIPv6 bool) bool {
	var changed bool

	serviceName := getServiceName(service)
	if isIPv6 {
		if !strings.EqualFold(string(*pip.Properties.PublicIPAddressVersion), string(armnetwork.IPVersionIPv6)) {
			pip.Properties.PublicIPAddressVersion = to.Ptr(armnetwork.IPVersionIPv6)
			klog.V(2).Infof("service(%s): pip(%s) - should be created as IPv6", serviceName, *pip.Name)
			changed = true
		}

		if az.UseStandardLoadBalancer() {
			// standard SKU must have static allocation method for ipv6
			if !strings.EqualFold(string(*pip.Properties.PublicIPAllocationMethod), string(armnetwork.IPAllocationMethodStatic)) {
				pip.Properties.PublicIPAllocationMethod = to.Ptr(armnetwork.IPAllocationMethodStatic)
				changed = true
			}
		} else if !strings.EqualFold(string(*pip.Properties.PublicIPAllocationMethod), string(armnetwork.IPAllocationMethodDynamic)) {
			pip.Properties.PublicIPAllocationMethod = to.Ptr(armnetwork.IPAllocationMethodDynamic)
			changed = true
		}
	} else {
		if !strings.EqualFold(string(*pip.Properties.PublicIPAddressVersion), string(armnetwork.IPVersionIPv4)) {
			pip.Properties.PublicIPAddressVersion = to.Ptr(armnetwork.IPVersionIPv4)
			klog.V(2).Infof("service(%s): pip(%s) - should be created as IPv4", serviceName, *pip.Name)
			changed = true
		}
	}

	return changed
}

func reconcileDNSSettings(
	pip *armnetwork.PublicIPAddress,
	domainNameLabel, serviceName, pipName string,
	isUserAssignedPIP bool,
) (bool, error) {
	var changed bool

	if existingServiceName := getServiceFromPIPDNSTags(pip.Tags); existingServiceName != "" && !strings.EqualFold(existingServiceName, serviceName) {
		return false, fmt.Errorf("ensurePublicIPExists for service(%s): pip(%s) - there is an existing service %s consuming the DNS label on the public IP, so the service cannot set the DNS label annotation with this value", serviceName, pipName, existingServiceName)
	}

	if len(domainNameLabel) == 0 {
		if pip.Properties.DNSSettings != nil {
			pip.Properties.DNSSettings = nil
			changed = true
		}
	} else {
		if pip.Properties.DNSSettings == nil ||
			pip.Properties.DNSSettings.DomainNameLabel == nil {
			klog.V(6).Infof("ensurePublicIPExists for service(%s): pip(%s) - no existing DNS label on the public IP, create one", serviceName, pipName)
			pip.Properties.DNSSettings = &armnetwork.PublicIPAddressDNSSettings{
				DomainNameLabel: &domainNameLabel,
			}
			changed = true
		} else {
			existingDNSLabel := pip.Properties.DNSSettings.DomainNameLabel
			if !strings.EqualFold(ptr.Deref(existingDNSLabel, ""), domainNameLabel) {
				pip.Properties.DNSSettings.DomainNameLabel = &domainNameLabel
				changed = true
			}
		}

		if svc := getServiceFromPIPDNSTags(pip.Tags); svc == "" || !strings.EqualFold(svc, serviceName) {
			if !isUserAssignedPIP {
				pip.Tags[consts.ServiceUsingDNSKey] = &serviceName
				changed = true
			}
		}
	}

	return changed, nil
}

func getServiceFromPIPDNSTags(tags map[string]*string) string {
	v, ok := tags[consts.ServiceUsingDNSKey]
	if ok && v != nil {
		return *v
	}

	v, ok = tags[consts.LegacyServiceUsingDNSKey]
	if ok && v != nil {
		return *v
	}

	return ""
}

func deleteServicePIPDNSTags(tags *map[string]*string) {
	delete(*tags, consts.ServiceUsingDNSKey)
	delete(*tags, consts.LegacyServiceUsingDNSKey)
}

func getServiceFromPIPServiceTags(tags map[string]*string) string {
	v, ok := tags[consts.ServiceTagKey]
	if ok && v != nil {
		return *v
	}

	v, ok = tags[consts.LegacyServiceTagKey]
	if ok && v != nil {
		return *v
	}

	return ""
}

func getClusterFromPIPClusterTags(tags map[string]*string) string {
	v, ok := tags[consts.ClusterNameKey]
	if ok && v != nil {
		return *v
	}

	v, ok = tags[consts.LegacyClusterNameKey]
	if ok && v != nil {
		return *v
	}

	return ""
}

type serviceIPTagRequest struct {
	IPTagsRequestedByAnnotation bool
	IPTags                      []*armnetwork.IPTag
}

// Get the ip tag Request for the public ip from service annotations.
func getServiceIPTagRequestForPublicIP(service *v1.Service) serviceIPTagRequest {
	if service != nil {
		if ipTagString, found := service.Annotations[consts.ServiceAnnotationIPTagsForPublicIP]; found {
			return serviceIPTagRequest{
				IPTagsRequestedByAnnotation: true,
				IPTags:                      convertIPTagMapToSlice(getIPTagMap(ipTagString)),
			}
		}
	}

	return serviceIPTagRequest{
		IPTagsRequestedByAnnotation: false,
		IPTags:                      nil,
	}
}

func getIPTagMap(ipTagString string) map[string]string {
	outputMap := make(map[string]string)
	commaDelimitedPairs := strings.Split(strings.TrimSpace(ipTagString), ",")
	for _, commaDelimitedPair := range commaDelimitedPairs {
		splitKeyValue := strings.Split(commaDelimitedPair, "=")

		// Include only valid pairs in the return value
		// Last Write wins.
		if len(splitKeyValue) == 2 {
			tagKey := strings.TrimSpace(splitKeyValue[0])
			tagValue := strings.TrimSpace(splitKeyValue[1])

			outputMap[tagKey] = tagValue
		}
	}

	return outputMap
}

func sortIPTags(ipTags *[]*armnetwork.IPTag) {
	if ipTags != nil {
		sort.Slice(*ipTags, func(i, j int) bool {
			return ptr.Deref((*ipTags)[i].IPTagType, "") < ptr.Deref((*ipTags)[j].IPTagType, "") ||
				ptr.Deref((*ipTags)[i].Tag, "") < ptr.Deref((*ipTags)[j].Tag, "")
		})
	}
}

func areIPTagsEquivalent(ipTags1 []*armnetwork.IPTag, ipTags2 []*armnetwork.IPTag) bool {
	sortIPTags(&ipTags1)
	sortIPTags(&ipTags2)

	if ipTags1 == nil {
		ipTags1 = []*armnetwork.IPTag{}
	}

	if ipTags2 == nil {
		ipTags2 = []*armnetwork.IPTag{}
	}

	return reflect.DeepEqual(ipTags1, ipTags2)
}

func convertIPTagMapToSlice(ipTagMap map[string]string) []*armnetwork.IPTag {
	if ipTagMap == nil {
		return nil
	}

	if len(ipTagMap) == 0 {
		return []*armnetwork.IPTag{}
	}

	outputTags := []*armnetwork.IPTag{}
	for k, v := range ipTagMap {
		ipTag := &armnetwork.IPTag{
			IPTagType: ptr.To(k),
			Tag:       ptr.To(v),
		}
		outputTags = append(outputTags, ipTag)
	}

	return outputTags
}

func getDomainNameLabel(pip *armnetwork.PublicIPAddress) string {
	if pip == nil || pip.Properties == nil || pip.Properties.DNSSettings == nil {
		return ""
	}
	return ptr.Deref(pip.Properties.DNSSettings.DomainNameLabel, "")
}

// subnet is reused to reduce API calls when dualstack.
func (az *Cloud) isFrontendIPChanged(
	ctx context.Context,
	clusterName string,
	config *armnetwork.FrontendIPConfiguration,
	service *v1.Service,
	lbFrontendIPConfigName string,
	subnet *armnetwork.Subnet,
) (bool, error) {
	isServiceOwnsFrontendIP, isPrimaryService, fipIPVersion := az.serviceOwnsFrontendIP(ctx, config, service)
	if isServiceOwnsFrontendIP && isPrimaryService && !strings.EqualFold(ptr.Deref(config.Name, ""), lbFrontendIPConfigName) {
		return true, nil
	}
	if !strings.EqualFold(ptr.Deref(config.Name, ""), lbFrontendIPConfigName) {
		return false, nil
	}
	pipRG := az.getPublicIPAddressResourceGroup(service)
	var isIPv6 bool
	var err error
	if fipIPVersion != nil {
		isIPv6 = *fipIPVersion == armnetwork.IPVersionIPv6
	} else {
		if isIPv6, err = az.isFIPIPv6(service, config); err != nil {
			return false, err
		}
	}
	loadBalancerIP := getServiceLoadBalancerIP(service, isIPv6)
	isInternal := requiresInternalLoadBalancer(service)
	if isInternal {
		// Judge subnet
		subnetName := getInternalSubnet(service)
		if subnetName != nil {
			if subnet == nil {
				return false, fmt.Errorf("isFrontendIPChanged: Unexpected nil subnet %q", ptr.Deref(subnetName, ""))
			}
			if config.Properties.Subnet != nil && !strings.EqualFold(ptr.Deref(config.Properties.Subnet.ID, ""), ptr.Deref(subnet.ID, "")) {
				return true, nil
			}
		}
		return loadBalancerIP != "" && !strings.EqualFold(loadBalancerIP, ptr.Deref(config.Properties.PrivateIPAddress, "")), nil
	}
	pipName, _, err := az.determinePublicIPName(ctx, clusterName, service, isIPv6)
	if err != nil {
		return false, err
	}
	pip, existsPip, err := az.getPublicIPAddress(ctx, pipRG, pipName, azcache.CacheReadTypeDefault)
	if err != nil {
		return false, err
	}
	if !existsPip {
		return true, nil
	}
	return config.Properties.PublicIPAddress != nil && !strings.EqualFold(ptr.Deref(pip.ID, ""), ptr.Deref(config.Properties.PublicIPAddress.ID, "")), nil
}

// isFrontendIPConfigUnsafeToDelete checks if a frontend IP config is safe to be deleted.
// It is safe to be deleted if and only if there is no reference from other
// loadBalancing resources, including loadBalancing rules, outbound rules, inbound NAT rules
// and inbound NAT pools.
func (az *Cloud) isFrontendIPConfigUnsafeToDelete(
	lb *armnetwork.LoadBalancer,
	service *v1.Service,
	fipConfigID *string,
) (bool, error) {
	if lb == nil || fipConfigID == nil || *fipConfigID == "" {
		return false, fmt.Errorf("isFrontendIPConfigUnsafeToDelete: incorrect parameters")
	}

	var (
		lbRules         []*armnetwork.LoadBalancingRule
		outboundRules   []*armnetwork.OutboundRule
		inboundNatRules []*armnetwork.InboundNatRule
		inboundNatPools []*armnetwork.InboundNatPool
		unsafe          bool
	)

	if lb.Properties != nil {
		if lb.Properties.LoadBalancingRules != nil {
			lbRules = lb.Properties.LoadBalancingRules
		}
		if lb.Properties.OutboundRules != nil {
			outboundRules = lb.Properties.OutboundRules
		}
		if lb.Properties.InboundNatRules != nil {
			inboundNatRules = lb.Properties.InboundNatRules
		}
		if lb.Properties.InboundNatPools != nil {
			inboundNatPools = lb.Properties.InboundNatPools
		}
	}

	// check if there are load balancing rules from other services
	// referencing this frontend IP configuration
	for _, lbRule := range lbRules {
		if lbRule.Properties != nil &&
			lbRule.Properties.FrontendIPConfiguration != nil &&
			lbRule.Properties.FrontendIPConfiguration.ID != nil &&
			strings.EqualFold(*lbRule.Properties.FrontendIPConfiguration.ID, *fipConfigID) {
			if !az.serviceOwnsRule(service, *lbRule.Name) {
				warningMsg := fmt.Sprintf("isFrontendIPConfigUnsafeToDelete: frontend IP configuration with ID %s on LB %s cannot be deleted because it is being referenced by load balancing rules of other services", *fipConfigID, *lb.Name)
				klog.Warning(warningMsg)
				az.Event(service, v1.EventTypeWarning, "DeletingFrontendIPConfiguration", warningMsg)
				unsafe = true
				break
			}
		}
	}

	// check if there are outbound rules
	// referencing this frontend IP configuration
	for _, outboundRule := range outboundRules {
		if outboundRule.Properties != nil && outboundRule.Properties.FrontendIPConfigurations != nil {
			outboundRuleFIPConfigs := outboundRule.Properties.FrontendIPConfigurations
			if found := findMatchedOutboundRuleFIPConfig(fipConfigID, outboundRuleFIPConfigs); found {
				warningMsg := fmt.Sprintf("isFrontendIPConfigUnsafeToDelete: frontend IP configuration with ID %s on LB %s cannot be deleted because it is being referenced by the outbound rule %s", *fipConfigID, *lb.Name, *outboundRule.Name)
				klog.Warning(warningMsg)
				az.Event(service, v1.EventTypeWarning, "DeletingFrontendIPConfiguration", warningMsg)
				unsafe = true
				break
			}
		}
	}

	// check if there are inbound NAT rules
	// referencing this frontend IP configuration
	for _, inboundNatRule := range inboundNatRules {
		if inboundNatRule.Properties != nil &&
			inboundNatRule.Properties.FrontendIPConfiguration != nil &&
			inboundNatRule.Properties.FrontendIPConfiguration.ID != nil &&
			strings.EqualFold(*inboundNatRule.Properties.FrontendIPConfiguration.ID, *fipConfigID) {
			warningMsg := fmt.Sprintf("isFrontendIPConfigUnsafeToDelete: frontend IP configuration with ID %s on LB %s cannot be deleted because it is being referenced by the inbound NAT rule %s", *fipConfigID, *lb.Name, *inboundNatRule.Name)
			klog.Warning(warningMsg)
			az.Event(service, v1.EventTypeWarning, "DeletingFrontendIPConfiguration", warningMsg)
			unsafe = true
			break
		}
	}

	// check if there are inbound NAT pools
	// referencing this frontend IP configuration
	for _, inboundNatPool := range inboundNatPools {
		if inboundNatPool.Properties != nil &&
			inboundNatPool.Properties.FrontendIPConfiguration != nil &&
			inboundNatPool.Properties.FrontendIPConfiguration.ID != nil &&
			strings.EqualFold(*inboundNatPool.Properties.FrontendIPConfiguration.ID, *fipConfigID) {
			warningMsg := fmt.Sprintf("isFrontendIPConfigUnsafeToDelete: frontend IP configuration with ID %s on LB %s cannot be deleted because it is being referenced by the inbound NAT pool %s", *fipConfigID, *lb.Name, *inboundNatPool.Name)
			klog.Warning(warningMsg)
			az.Event(service, v1.EventTypeWarning, "DeletingFrontendIPConfiguration", warningMsg)
			unsafe = true
			break
		}
	}

	return unsafe, nil
}

func findMatchedOutboundRuleFIPConfig(fipConfigID *string, outboundRuleFIPConfigs []*armnetwork.SubResource) bool {
	var found bool
	for _, config := range outboundRuleFIPConfigs {
		if config.ID != nil && strings.EqualFold(*config.ID, *fipConfigID) {
			found = true
		}
	}
	return found
}

func (az *Cloud) findFrontendIPConfigsOfService(
	ctx context.Context,
	fipConfigs []*armnetwork.FrontendIPConfiguration,
	service *v1.Service,
) (map[bool]*armnetwork.FrontendIPConfiguration, error) {
	fipsOfServiceMap := map[bool]*armnetwork.FrontendIPConfiguration{}
	for _, config := range fipConfigs {
		config := config
		owns, _, fipIPVersion := az.serviceOwnsFrontendIP(ctx, config, service)
		if owns {
			var fipIsIPv6 bool
			var err error
			if fipIPVersion != nil {
				fipIsIPv6 = fipIPVersion == to.Ptr(armnetwork.IPVersionIPv6)
			} else {
				if fipIsIPv6, err = az.isFIPIPv6(service, config); err != nil {
					return nil, err
				}
			}

			fipsOfServiceMap[fipIsIPv6] = config
		}
	}

	return fipsOfServiceMap, nil
}

// reconcileMultipleStandardLoadBalancerConfigurations runs only once every time the
// cloud controller manager restarts or reloads itself. It checks all existing
// load balancer typed services and add service names to the ActiveServices queue
// of the corresponding load balancer configuration. It also checks if there is a configuration
// named <clustername>. If not, an error will be reported.
func (az *Cloud) reconcileMultipleStandardLoadBalancerConfigurations(
	ctx context.Context,
	lbs []*armnetwork.LoadBalancer,
	service *v1.Service,
	clusterName string,
	existingLBs []*armnetwork.LoadBalancer,
	nodes []*v1.Node,
) (err error) {
	if !az.UseMultipleStandardLoadBalancers() {
		return nil
	}

	if az.multipleStandardLoadBalancerConfigurationsSynced {
		return nil
	}
	defer func() {
		if err == nil {
			az.multipleStandardLoadBalancerConfigurationsSynced = true
		}
	}()

	var found bool
	for _, multiSLBConfig := range az.MultipleStandardLoadBalancerConfigurations {
		if strings.EqualFold(multiSLBConfig.Name, clusterName) {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("multiple standard load balancers are enabled but no configuration named %q is found", clusterName)
	}

	svcs, err := az.KubeClient.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("reconcileMultipleStandardLoadBalancerConfigurations: failed to list all load balancer services: %V", err)
		return fmt.Errorf("failed to list all load balancer services: %w", err)
	}
	rulePrefixToSVCNameMap := make(map[string]string)
	for _, svc := range svcs.Items {
		svc := svc
		if strings.EqualFold(string(svc.Spec.Type), string(v1.ServiceTypeLoadBalancer)) {
			prefix := az.GetLoadBalancerName(ctx, "", &svc)
			svcName := getServiceName(&svc)
			rulePrefixToSVCNameMap[strings.ToLower(prefix)] = svcName
			klog.V(2).Infof("reconcileMultipleStandardLoadBalancerConfigurations: found service %q with prefix %q", svcName, prefix)
		}
	}

	for _, existingLB := range existingLBs {
		lbName := ptr.Deref(existingLB.Name, "")
		if existingLB.Properties != nil &&
			existingLB.Properties.LoadBalancingRules != nil {
			for _, rule := range existingLB.Properties.LoadBalancingRules {
				ruleName := ptr.Deref(rule.Name, "")
				rulePrefix := strings.Split(ruleName, "-")[0]
				if rulePrefix == "" {
					klog.Warningf("reconcileMultipleStandardLoadBalancerConfigurations: the load balancing rule name %s is not in the correct format", ruleName)
				}
				svcName, ok := rulePrefixToSVCNameMap[strings.ToLower(rulePrefix)]
				if ok {
					klog.V(2).Infof(
						"reconcileMultipleStandardLoadBalancerConfigurations: found load balancer %q with rule %q of service %q",
						lbName, ruleName, svcName,
					)
					for i := range az.MultipleStandardLoadBalancerConfigurations {
						if strings.EqualFold(trimSuffixIgnoreCase(lbName, consts.InternalLoadBalancerNameSuffix), az.MultipleStandardLoadBalancerConfigurations[i].Name) {
							az.multipleStandardLoadBalancersActiveServicesLock.Lock()
							az.MultipleStandardLoadBalancerConfigurations[i].ActiveServices = utilsets.SafeInsert(az.MultipleStandardLoadBalancerConfigurations[i].ActiveServices, svcName)
							az.multipleStandardLoadBalancersActiveServicesLock.Unlock()
							klog.V(2).Infof("reconcileMultipleStandardLoadBalancerConfigurations: service(%s) is active on lb(%s)", svcName, lbName)
						}
					}
				}
			}
		}
	}

	return az.reconcileMultipleStandardLoadBalancerBackendNodes(ctx, clusterName, "", lbs, service, nodes, true)
}

// reconcileLoadBalancer ensures load balancer exists and the frontend ip config is setup.
// This also reconciles the Service's Ports with the LoadBalancer config.
// This entails adding rules/probes for expected Ports and removing stale rules/ports.
// nodes only used if wantLb is true
func (az *Cloud) reconcileLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node, wantLb bool) (*armnetwork.LoadBalancer, bool /*needRetry*/, error) {
	isBackendPoolPreConfigured := az.isBackendPoolPreConfigured(service)
	serviceName := getServiceName(service)
	klog.V(2).Infof("reconcileLoadBalancer for service(%s) - wantLb(%t): started", serviceName, wantLb)

	existingLBs, err := az.ListManagedLBs(ctx, service, nodes, clusterName)
	if err != nil {
		return nil, false, fmt.Errorf("reconcileLoadBalancer: failed to list managed LB: %w", err)
	}

	if existingLBs, err = az.cleanupBasicLoadBalancer(ctx, clusterName, service, existingLBs); err != nil {
		klog.ErrorS(err, "reconcileLoadBalancer: failed to check and remove outdated basic load balancers", "service", serviceName)
		return nil, false, err
	}

	// Delete backend pools for local service if:
	// 1. the cluster is migrating from multi-slb to single-slb,
	// 2. the service is changed from local to cluster.
	if !az.UseMultipleStandardLoadBalancers() || !isLocalService(service) {
		existingLBs, err = az.cleanupLocalServiceBackendPool(ctx, service, nodes, existingLBs, clusterName)
		if err != nil {
			klog.Errorf("reconcileLoadBalancer: failed to cleanup local service backend pool for service %q, error: %s", serviceName, err.Error())
			return nil, false, err
		}
	}

	if err := az.reconcileMultipleStandardLoadBalancerConfigurations(ctx, existingLBs, service, clusterName, existingLBs, nodes); err != nil {
		klog.Errorf("reconcileLoadBalancer: failed to reconcile multiple standard load balancer configurations: %s", err.Error())
		return nil, false, err
	}

	lb, newLBs, lbStatus, _, _, deletedPLS, err := az.getServiceLoadBalancer(ctx, service, clusterName, nodes, wantLb, existingLBs)
	if err != nil {
		klog.Errorf("reconcileLoadBalancer: failed to get load balancer for service %q, error: %v", serviceName, err)
		return nil, false, err
	}
	if deletedPLS {
		klog.V(2).InfoS("reconcileLoadBalancer: PLS is deleted and the LB ETag has changed, need to retry", "service", serviceName)
		return lb, true, nil
	}
	existingLBs = newLBs

	lbName := *lb.Name
	lbResourceGroup := az.getLoadBalancerResourceGroup()
	lbBackendPoolIDs := az.getBackendPoolIDsForService(service, clusterName, lbName)
	klog.V(2).Infof("reconcileLoadBalancer for service(%s): lb(%s/%s) wantLb(%t) resolved load balancer name",
		serviceName, lbResourceGroup, lbName, wantLb)
	lbFrontendIPConfigNames := az.getFrontendIPConfigNames(service)
	lbFrontendIPConfigIDs := map[bool]string{
		consts.IPVersionIPv4: az.getFrontendIPConfigID(lbName, lbFrontendIPConfigNames[consts.IPVersionIPv4]),
		consts.IPVersionIPv6: az.getFrontendIPConfigID(lbName, lbFrontendIPConfigNames[consts.IPVersionIPv6]),
	}
	dirtyLb := false

	// reconcile the load balancer's backend pool configuration.
	if wantLb {
		var (
			preConfig, backendPoolsUpdated bool
			err                            error
		)
		preConfig, backendPoolsUpdated, lb, err = az.LoadBalancerBackendPool.ReconcileBackendPools(ctx, clusterName, service, lb)
		if err != nil {
			return lb, false, err
		}
		if backendPoolsUpdated {
			dirtyLb = true
		}
		isBackendPoolPreConfigured = preConfig

		// If the LB is changed, refresh it to avoid etag mismatch error
		// later when create or update the LB.
		addOrUpdateLBInList(&existingLBs, lb)
	}

	// reconcile the load balancer's frontend IP configurations.
	ownedFIPConfigs, toDeleteConfigs, fipChanged, err := az.reconcileFrontendIPConfigs(ctx, clusterName, service, lb, lbStatus, wantLb, lbFrontendIPConfigNames)
	if err != nil {
		return lb, false, err
	}
	if fipChanged {
		dirtyLb = true
	}

	// update probes/rules
	for _, ownedFIPConfig := range ownedFIPConfigs {
		if ownedFIPConfig == nil {
			continue
		}
		if ownedFIPConfig.ID == nil {
			return nil, false, fmt.Errorf("reconcileLoadBalancer for service (%s)(%t): nil ID for frontend IP config", serviceName, wantLb)
		}

		var isIPv6 bool
		var err error
		_, _, fipIPVersion := az.serviceOwnsFrontendIP(ctx, ownedFIPConfig, service)
		if fipIPVersion != nil {
			isIPv6 = fipIPVersion == to.Ptr(armnetwork.IPVersionIPv6)
		} else {
			if isIPv6, err = az.isFIPIPv6(service, ownedFIPConfig); err != nil {
				return nil, false, err
			}
		}
		lbFrontendIPConfigIDs[isIPv6] = *ownedFIPConfig.ID
	}

	var expectedProbes []*armnetwork.Probe
	var expectedRules []*armnetwork.LoadBalancingRule
	getExpectedLBRule := func(isIPv6 bool) error {
		expectedProbesSingleStack, expectedRulesSingleStack, err := az.getExpectedLBRules(service, lbFrontendIPConfigIDs[isIPv6], lbBackendPoolIDs[isIPv6], lbName, isIPv6)
		if err != nil {
			return err
		}
		expectedProbes = append(expectedProbes, expectedProbesSingleStack...)
		expectedRules = append(expectedRules, expectedRulesSingleStack...)
		return nil
	}
	v4Enabled, v6Enabled := getIPFamiliesEnabled(service)
	if wantLb && v4Enabled {
		if err = az.checkLoadBalancerResourcesConflicts(lb, lbFrontendIPConfigIDs[false], service); err != nil {
			return nil, false, err
		}
		if err := getExpectedLBRule(consts.IPVersionIPv4); err != nil {
			return nil, false, err
		}
	}
	if wantLb && v6Enabled {
		if err = az.checkLoadBalancerResourcesConflicts(lb, lbFrontendIPConfigIDs[true], service); err != nil {
			return nil, false, err
		}
		if err := getExpectedLBRule(consts.IPVersionIPv6); err != nil {
			return nil, false, err
		}
	}

	if changed := az.reconcileLBProbes(lb, service, serviceName, wantLb, expectedProbes); changed {
		dirtyLb = true
	}

	if changed := az.reconcileLBRules(lb, service, serviceName, wantLb, expectedRules); changed {
		dirtyLb = true
	}
	if changed := az.ensureLoadBalancerTagged(lb); changed {
		dirtyLb = true
	}

	// We don't care if the LB exists or not
	// We only care about if there is any change in the LB, which means dirtyLB
	// If it is not exist, and no change to that, we don't CreateOrUpdate LB
	if dirtyLb {
		if len(toDeleteConfigs) > 0 {
			var needRetry bool
			for i := range toDeleteConfigs {
				fipConfigToDel := toDeleteConfigs[i]
				deletedPLS, err = az.reconcilePrivateLinkService(ctx, clusterName, service, fipConfigToDel, false /* wantPLS */)
				if err != nil {
					klog.Errorf(
						"reconcileLoadBalancer for service(%s): lb(%s) - failed to clean up PrivateLinkService for frontEnd(%s): %v",
						serviceName,
						lbName,
						ptr.Deref(fipConfigToDel.Name, ""),
						err,
					)
				}
				if deletedPLS {
					needRetry = true
				}
			}
			if needRetry {
				klog.V(2).InfoS("reconcileLoadBalancer: PLS is deleted and the LB ETag has changed, need to retry", "service", serviceName)
				return lb, true, nil
			}
		}

		if lb.Properties == nil || len(lb.Properties.FrontendIPConfigurations) == 0 {
			err := az.cleanOrphanedLoadBalancer(ctx, lb, existingLBs, service, clusterName)
			if err != nil {
				klog.Errorf("reconcileLoadBalancer for service(%s): lb(%s) - failed to cleanOrphanedLoadBalancer: %v", serviceName, lbName, err)
				return nil, false, err
			}
		} else {
			klog.V(2).Infof("reconcileLoadBalancer: reconcileLoadBalancer for service(%s): lb(%s) - updating", serviceName, lbName)
			err := az.CreateOrUpdateLB(ctx, service, *lb)
			if err != nil {
				klog.Errorf("reconcileLoadBalancer for service(%s) abort backoff: lb(%s) - updating: %s", serviceName, lbName, err.Error())
				return nil, false, err
			}

			// Refresh updated lb which will be used later in other places.
			newLB, exist, err := az.getAzureLoadBalancer(ctx, lbName, azcache.CacheReadTypeForceRefresh)
			if err != nil {
				klog.Errorf("reconcileLoadBalancer for service(%s): getAzureLoadBalancer(%s) failed: %v", serviceName, lbName, err)
				return nil, false, err
			}
			if !exist {
				return nil, false, fmt.Errorf("load balancer %q not found", lbName)
			}
			lb = newLB

			addOrUpdateLBInList(&existingLBs, newLB)
		}
	}

	if wantLb && nodes != nil && !isBackendPoolPreConfigured {
		// Add the machines to the backend pool if they're not already
		vmSetName := az.mapLoadBalancerNameToVMSet(lbName, clusterName)
		// Etag would be changed when updating backend pools, so invalidate lbCache after it.
		defer func() {
			_ = az.lbCache.Delete(lbName)
		}()

		if az.UseMultipleStandardLoadBalancers() {
			err := az.reconcileMultipleStandardLoadBalancerBackendNodes(ctx, clusterName, lbName, existingLBs, service, nodes, false)
			if err != nil {
				return nil, false, err
			}
		}

		// Need to reconcile every managed backend pools of all managed load balancers in
		// the cluster when using multiple standard load balancers.
		// This is because there are chances for backend pools from more than one load balancers
		// change in one reconciliation loop.
		var lbToReconcile []*armnetwork.LoadBalancer
		lbToReconcile = append(lbToReconcile, lb)
		if az.UseMultipleStandardLoadBalancers() {
			lbToReconcile = existingLBs
		}
		lb, err = az.reconcileBackendPoolHosts(ctx, lb, lbToReconcile, service, nodes, clusterName, vmSetName, lbBackendPoolIDs)
		if err != nil {
			return nil, false, err
		}
	}

	if fipChanged {
		az.reconcileMultipleStandardLoadBalancerConfigurationStatus(wantLb, serviceName, lbName)
	}

	klog.V(2).Infof("reconcileLoadBalancer for service(%s): lb(%s) finished", serviceName, lbName)
	return lb, false, nil
}

func (az *Cloud) reconcileBackendPoolHosts(
	ctx context.Context,
	currentLB *armnetwork.LoadBalancer,
	lbs []*armnetwork.LoadBalancer,
	service *v1.Service,
	nodes []*v1.Node,
	clusterName, vmSetName string,
	lbBackendPoolIDs map[bool]string,
) (*armnetwork.LoadBalancer, error) {
	var res *armnetwork.LoadBalancer
	res = currentLB
	for _, lb := range lbs {
		lb := lb
		lbName := ptr.Deref(lb.Name, "")
		if lb.Properties != nil && lb.Properties.BackendAddressPools != nil {
			for i, backendPool := range lb.Properties.BackendAddressPools {
				isIPv6 := isBackendPoolIPv6(ptr.Deref(backendPool.Name, ""))
				if strings.EqualFold(ptr.Deref(backendPool.Name, ""), az.getBackendPoolNameForService(service, clusterName, isIPv6)) {
					if err := az.LoadBalancerBackendPool.EnsureHostsInPool(
						ctx,
						service,
						nodes,
						lbBackendPoolIDs[isIPv6],
						vmSetName,
						clusterName,
						lbName,
						(lb.Properties.BackendAddressPools)[i],
					); err != nil {
						return nil, err
					}
				}
			}
		}
		if strings.EqualFold(lbName, *currentLB.Name) {
			res = lb
		}
	}
	return res, nil
}

// addOrUpdateLBInList adds or updates the given lb in the list
func addOrUpdateLBInList(lbs *[]*armnetwork.LoadBalancer, targetLB *armnetwork.LoadBalancer) {
	if lbs != nil {
		for i, lb := range *lbs {
			if strings.EqualFold(ptr.Deref(lb.Name, ""), ptr.Deref(targetLB.Name, "")) {
				(*lbs)[i] = targetLB
				return
			}
		}
		*lbs = append(*lbs, targetLB)
	}
}

// removeLBFromList removes the given lb from the list
func removeLBFromList(lbs *[]*armnetwork.LoadBalancer, lbName string) {
	if lbs != nil {
		for i := len(*lbs) - 1; i >= 0; i-- {
			if strings.EqualFold(ptr.Deref((*lbs)[i].Name, ""), lbName) {
				*lbs = append((*lbs)[:i], (*lbs)[i+1:]...)
				break
			}
		}
	}
}

// removeNodeFromLBConfig searches for the occurrence of the given node in the lb configs and removes it
func (az *Cloud) removeNodeFromLBConfig(nodeNameToLBConfigIDXMap map[string]int, nodeName string) {
	if idx, ok := nodeNameToLBConfigIDXMap[nodeName]; ok {
		currentLBConfigName := az.MultipleStandardLoadBalancerConfigurations[idx].Name
		klog.V(4).Infof("reconcileMultipleStandardLoadBalancerBackendNodes: remove node(%s) on lb(%s)", nodeName, currentLBConfigName)
		az.multipleStandardLoadBalancersActiveNodesLock.Lock()
		az.MultipleStandardLoadBalancerConfigurations[idx].ActiveNodes.Delete(strings.ToLower(nodeName))
		az.multipleStandardLoadBalancersActiveNodesLock.Unlock()
	}
}

// removeDeletedNodesFromLoadBalancerConfigurations removes the deleted nodes
// that do not exist in nodes list from the load balancer configurations
func (az *Cloud) removeDeletedNodesFromLoadBalancerConfigurations(nodes []*v1.Node) map[string]int {
	logger := klog.Background().WithName("removeDeletedNodesFromLoadBalancerConfigurations")
	nodeNamesSet := utilsets.NewString()
	for _, node := range nodes {
		nodeNamesSet.Insert(node.Name)
	}

	az.multipleStandardLoadBalancersActiveNodesLock.Lock()
	defer az.multipleStandardLoadBalancersActiveNodesLock.Unlock()

	// Remove the nodes from the load balancer configurations if they are not in the node list.
	nodeNameToLBConfigIDXMap := make(map[string]int)
	for i, multiSLBConfig := range az.MultipleStandardLoadBalancerConfigurations {
		logger.V(4).Info("checking load balancer configuration", "lb", multiSLBConfig.Name)
		if multiSLBConfig.ActiveNodes != nil {
			for _, nodeName := range multiSLBConfig.ActiveNodes.UnsortedList() {
				if nodeNamesSet.Has(nodeName) {
					logger.V(4).Info("found node in load balancer configuration", "node", nodeName, "lb", multiSLBConfig.Name)
					nodeNameToLBConfigIDXMap[nodeName] = i
				} else {
					logger.V(4).Info("removing node which is not found in input node list from load balancer configuration", "node", nodeName, "lb", multiSLBConfig.Name)
					az.MultipleStandardLoadBalancerConfigurations[i].ActiveNodes.Delete(nodeName)
				}
			}
		}
	}

	return nodeNameToLBConfigIDXMap
}

// accommodateNodesByPrimaryVMSet decides which load balancer configuration the node should be added to by primary vmSet
func (az *Cloud) accommodateNodesByPrimaryVMSet(
	ctx context.Context,
	lbName string,
	lbs []*armnetwork.LoadBalancer,
	nodes []*v1.Node,
	nodeNameToLBConfigIDXMap map[string]int,
) error {
	for _, node := range nodes {
		if _, ok := az.nodesWithCorrectLoadBalancerByPrimaryVMSet.Load(strings.ToLower(node.Name)); ok {
			continue
		}

		// TODO(niqi): reduce the API calls for VMAS and standalone VMs
		vmSetName, err := az.VMSet.GetNodeVMSetName(ctx, node)
		if err != nil {
			klog.Errorf("accommodateNodesByPrimaryVMSet: failed to get vmSetName for node(%s): %s", node.Name, err.Error())
			return err
		}
		for i := range az.MultipleStandardLoadBalancerConfigurations {
			multiSLBConfig := az.MultipleStandardLoadBalancerConfigurations[i]
			if strings.EqualFold(multiSLBConfig.PrimaryVMSet, vmSetName) {
				foundPrimaryLB := isLBInList(lbs, multiSLBConfig.Name)
				if !foundPrimaryLB && !strings.EqualFold(trimSuffixIgnoreCase(lbName, consts.InternalLoadBalancerNameSuffix), multiSLBConfig.Name) {
					klog.V(4).Infof("accommodateNodesByPrimaryVMSet: node(%s) should be on lb(%s) because of primary vmSet (%s), but the lb is not found and will not be created this time, will ignore the primaryVMSet", node.Name, multiSLBConfig.Name, vmSetName)
					continue
				}

				az.nodesWithCorrectLoadBalancerByPrimaryVMSet.Store(strings.ToLower(node.Name), struct{}{})
				if !multiSLBConfig.ActiveNodes.Has(node.Name) {
					klog.V(4).Infof("accommodateNodesByPrimaryVMSet: node(%s) should be on lb(%s) because of primary vmSet (%s)", node.Name, multiSLBConfig.Name, vmSetName)

					az.removeNodeFromLBConfig(nodeNameToLBConfigIDXMap, node.Name)

					az.multipleStandardLoadBalancersActiveNodesLock.Lock()
					az.MultipleStandardLoadBalancerConfigurations[i].ActiveNodes = utilsets.SafeInsert(az.MultipleStandardLoadBalancerConfigurations[i].ActiveNodes, node.Name)
					az.multipleStandardLoadBalancersActiveNodesLock.Unlock()
				}
				break
			}
		}
	}

	return nil
}

// accommodateNodesByNodeSelector decides which load balancer configuration the node should be added to by node selector
func (az *Cloud) accommodateNodesByNodeSelector(
	ctx context.Context,
	lbName string,
	lbs []*armnetwork.LoadBalancer,
	service *v1.Service,
	nodes []*v1.Node,
	nodeNameToLBConfigIDXMap map[string]int,
) error {
	logger := klog.FromContext(ctx).WithName("accommodateNodesByNodeSelector")

	for _, node := range nodes {
		// Skip nodes that have been matched with a load balancer
		// by primary vmSet.
		if _, ok := az.nodesWithCorrectLoadBalancerByPrimaryVMSet.Load(strings.ToLower(node.Name)); ok {
			continue
		}

		logger.V(4).Info("checking node", "node", node.Name)

		// If the vmSet of the node does not match any load balancer,
		// pick all load balancers whose node selector matches the node.
		var eligibleLBsIDX []int
		for i, multiSLBConfig := range az.MultipleStandardLoadBalancerConfigurations {
			if !isEmptyLabelSelector(multiSLBConfig.NodeSelector) {
				nodeSelector, err := metav1.LabelSelectorAsSelector(multiSLBConfig.NodeSelector)
				if err != nil {
					logger.Error(err, "failed to parse nodeSelector", "lb", multiSLBConfig.Name)
					return err
				}
				if nodeSelector.Matches(labels.Set(node.Labels)) {
					logger.V(4).Info("node matches nodeSelector", "node", node.Name, "lb", multiSLBConfig.Name)
					found := isLBInList(lbs, multiSLBConfig.Name)
					if !found && !strings.EqualFold(trimSuffixIgnoreCase(lbName, consts.InternalLoadBalancerNameSuffix), multiSLBConfig.Name) {
						logger.V(4).Info("but the lb is not found and will not be created this time, will ignore this load balancer", "lb", multiSLBConfig.Name)
						continue
					}
					eligibleLBsIDX = append(eligibleLBsIDX, i)
				}
			}
		}
		// If no load balancer is matched, all load balancers without node selector are eligible.
		if len(eligibleLBsIDX) == 0 {
			for i, multiSLBConfig := range az.MultipleStandardLoadBalancerConfigurations {
				logger.V(4).Info("checking the node selector of the lb", "lb", multiSLBConfig.Name, "nodeSelector", multiSLBConfig.NodeSelector)
				if isEmptyLabelSelector(multiSLBConfig.NodeSelector) {
					eligibleLBsIDX = append(eligibleLBsIDX, i)
				}
			}
		}
		// Check if the valid load balancer exists or will exist
		// after the reconciliation.
		for i := len(eligibleLBsIDX) - 1; i >= 0; i-- {
			multiSLBConfig := az.MultipleStandardLoadBalancerConfigurations[eligibleLBsIDX[i]]
			found := isLBInList(lbs, multiSLBConfig.Name)
			if !found && !strings.EqualFold(trimSuffixIgnoreCase(lbName, consts.InternalLoadBalancerNameSuffix), multiSLBConfig.Name) {
				logger.V(4).Info(
					"the load balancer is a valid placement target for node, but the lb is not found and will not be created this time, ignore this load balancer",
					"lb", multiSLBConfig.Name, "node", node.Name,
				)
				eligibleLBsIDX = append(eligibleLBsIDX[:i], eligibleLBsIDX[i+1:]...)
			}
		}
		if idx, ok := nodeNameToLBConfigIDXMap[node.Name]; ok {
			if IntInSlice(idx, eligibleLBsIDX) {
				logger.V(4).Info("node is already on the eligible lb", "node", node.Name, "lb", az.MultipleStandardLoadBalancerConfigurations[idx].Name)
				continue
			}
		}

		logger.V(4).Info("showing eligible load balancer indices for the node", "node", node.Name, "lbs", eligibleLBsIDX)

		// Pick one with the fewest nodes among all eligible load balancers.
		minNodesIDX := -1
		minNodes := math.MaxInt32
		az.multipleStandardLoadBalancersActiveNodesLock.Lock()
		for _, idx := range eligibleLBsIDX {
			multiSLBConfig := az.MultipleStandardLoadBalancerConfigurations[idx]
			if multiSLBConfig.ActiveNodes.Len() < minNodes {
				logger.V(4).Info("found an lb with fewer nodes", "lb", multiSLBConfig.Name, "nodes", multiSLBConfig.ActiveNodes.Len())
				minNodes = multiSLBConfig.ActiveNodes.Len()
				minNodesIDX = idx
			}
		}
		logger.V(4).Info("showing the lb with the fewest nodes", "lb index", minNodesIDX, "node count", minNodes)
		az.multipleStandardLoadBalancersActiveNodesLock.Unlock()

		if idx, ok := nodeNameToLBConfigIDXMap[node.Name]; ok && idx != minNodesIDX {
			az.removeNodeFromLBConfig(nodeNameToLBConfigIDXMap, node.Name)
		}

		// Emit a warning for the orphaned node.
		if minNodesIDX == -1 {
			warningMsg := fmt.Sprintf("failed to find a lb for node %s", node.Name)
			az.Event(service, v1.EventTypeWarning, "FailedToFindLoadBalancerForNode", warningMsg)
			continue
		}

		klog.V(4).Infof("accommodateNodesByNodeSelector: node(%s) should be on lb(%s) it is the eligible LB with fewest number of nodes", node.Name, az.MultipleStandardLoadBalancerConfigurations[minNodesIDX].Name)
		az.multipleStandardLoadBalancersActiveNodesLock.Lock()
		az.MultipleStandardLoadBalancerConfigurations[minNodesIDX].ActiveNodes = utilsets.SafeInsert(az.MultipleStandardLoadBalancerConfigurations[minNodesIDX].ActiveNodes, node.Name)
		az.multipleStandardLoadBalancersActiveNodesLock.Unlock()
	}

	return nil
}

// isLBInList checks if the lb is in the list by multipleStandardLoadBalancerConfig name
func isLBInList(lbs []*armnetwork.LoadBalancer, lbConfigName string) bool {
	for _, lb := range lbs {
		if strings.EqualFold(trimSuffixIgnoreCase(ptr.Deref(lb.Name, ""), consts.InternalLoadBalancerNameSuffix), lbConfigName) {
			return true
		}
	}

	return false
}

// reconcileMultipleStandardLoadBalancerBackendNodes makes sure the arrangement of nodes
// across load balancer configurations is expected. This is used in two places:
// 1. Every time the cloud provide restarts.
// 2. Every time we ensure hosts in pool.
// It consists of two parts. First we put corresponding nodes to the load balancers
// whose primary vmSet matches the node. Then we put the rest of the nodes to the
// most eligible load balancers according to the node selector and the number of
// nodes currently in the load balancer.
// For availability set (no cache) amd vmss flex (with cache) clusters,
// a list call will be introduced every time we
// try to get the vmSet of a node. This is acceptable because of two reasons:
// 1. In AKS, we don't support multiple availability sets in a cluster so the
// cluster scale is small. For self-managed clusters, it is not recommended to
// use multiple standard load balancers with availability sets.
// 2. We only check nodes that are not matched by primary vmSet before we ensure
// hosts in pool. So the number API calls is under control.
func (az *Cloud) reconcileMultipleStandardLoadBalancerBackendNodes(
	ctx context.Context,
	clusterName string,
	lbName string,
	lbs []*armnetwork.LoadBalancer,
	service *v1.Service,
	nodes []*v1.Node,
	init bool,
) error {
	logger := klog.FromContext(ctx).
		WithName("reconcileMultipleStandardLoadBalancerBackendNodes").
		WithValues(
			"clusterName", clusterName,
			"lbName", lbName,
			"service", service.Name,
			"init", init,
		)
	if init {
		if err := az.recordExistingNodesOnLoadBalancers(clusterName, lbs); err != nil {
			logger.Error(err, "failed to record existing nodes on load balancers")
			return err
		}
	}

	// Remove the nodes from the load balancer configurations if they are not in the node list.
	nodeNameToLBConfigIDXMap := az.removeDeletedNodesFromLoadBalancerConfigurations(nodes)

	err := az.accommodateNodesByPrimaryVMSet(ctx, lbName, lbs, nodes, nodeNameToLBConfigIDXMap)
	if err != nil {
		return err
	}

	err = az.accommodateNodesByNodeSelector(ctx, lbName, lbs, service, nodes, nodeNameToLBConfigIDXMap)
	if err != nil {
		return err
	}

	return nil
}

// recordExistingNodesOnLoadBalancers restores the node distribution
// across multiple load balancers each time the cloud provider restarts.
func (az *Cloud) recordExistingNodesOnLoadBalancers(clusterName string, lbs []*armnetwork.LoadBalancer) error {
	bi, ok := az.LoadBalancerBackendPool.(*backendPoolTypeNodeIP)
	if !ok {
		return errors.New("must use backend pool type nodeIP")
	}
	bpNames := getBackendPoolNames(clusterName)
	for _, lb := range lbs {
		if lb.Properties == nil ||
			lb.Properties.BackendAddressPools == nil {
			continue
		}
		lbName := ptr.Deref(lb.Name, "")
		for _, backendPool := range lb.Properties.BackendAddressPools {
			backendPool := backendPool
			if found, _ := isLBBackendPoolsExisting(bpNames, backendPool.Name); found {
				nodeNames := bi.getBackendPoolNodeNames(backendPool)
				for i, multiSLBConfig := range az.MultipleStandardLoadBalancerConfigurations {
					if strings.EqualFold(trimSuffixIgnoreCase(
						lbName, consts.InternalLoadBalancerNameSuffix,
					), multiSLBConfig.Name) {
						az.MultipleStandardLoadBalancerConfigurations[i].ActiveNodes = utilsets.SafeInsert(multiSLBConfig.ActiveNodes, nodeNames...)
					}
				}
			}
		}
	}
	return nil
}

func (az *Cloud) reconcileMultipleStandardLoadBalancerConfigurationStatus(wantLb bool, svcName, lbName string) {
	lbName = trimSuffixIgnoreCase(lbName, consts.InternalLoadBalancerNameSuffix)
	for i := range az.MultipleStandardLoadBalancerConfigurations {
		if strings.EqualFold(lbName, az.MultipleStandardLoadBalancerConfigurations[i].Name) {
			az.multipleStandardLoadBalancersActiveServicesLock.Lock()

			if wantLb {
				klog.V(4).Infof("reconcileMultipleStandardLoadBalancerConfigurationStatus: service(%s) is active on lb(%s)", svcName, lbName)
				az.MultipleStandardLoadBalancerConfigurations[i].ActiveServices = utilsets.SafeInsert(az.MultipleStandardLoadBalancerConfigurations[i].ActiveServices, svcName)
			} else {
				klog.V(4).Infof("reconcileMultipleStandardLoadBalancerConfigurationStatus: service(%s) is not active on lb(%s) any more", svcName, lbName)
				az.MultipleStandardLoadBalancerConfigurations[i].ActiveServices.Delete(svcName)
			}
			az.multipleStandardLoadBalancersActiveServicesLock.Unlock()
			break
		}
	}
}

func (az *Cloud) reconcileLBProbes(lb *armnetwork.LoadBalancer, service *v1.Service, serviceName string, wantLb bool, expectedProbes []*armnetwork.Probe) bool {
	expectedProbes, _ = az.keepSharedProbe(service, *lb, expectedProbes, wantLb)

	// remove unwanted probes
	dirtyProbes := false
	var updatedProbes []*armnetwork.Probe
	if lb.Properties.Probes != nil {
		updatedProbes = lb.Properties.Probes
	}
	for i := len(updatedProbes) - 1; i >= 0; i-- {
		existingProbe := updatedProbes[i]
		if az.serviceOwnsRule(service, *existingProbe.Name) {
			klog.V(10).Infof("reconcileLoadBalancer for service (%s)(%t): lb probe(%s) - considering evicting", serviceName, wantLb, *existingProbe.Name)
			keepProbe := false
			if findProbe(expectedProbes, existingProbe) {
				klog.V(10).Infof("reconcileLoadBalancer for service (%s)(%t): lb probe(%s) - keeping", serviceName, wantLb, *existingProbe.Name)
				keepProbe = true
			}
			if !keepProbe {
				updatedProbes = append(updatedProbes[:i], updatedProbes[i+1:]...)
				klog.V(2).Infof("reconcileLoadBalancer for service (%s)(%t): lb probe(%s) - dropping", serviceName, wantLb, *existingProbe.Name)
				dirtyProbes = true
			}
		}
	}
	// add missing, wanted probes
	for _, expectedProbe := range expectedProbes {
		foundProbe := false
		if findProbe(updatedProbes, expectedProbe) {
			klog.V(10).Infof("reconcileLoadBalancer for service (%s)(%t): lb probe(%s) - already exists", serviceName, wantLb, *expectedProbe.Name)
			foundProbe = true
		}
		if !foundProbe {
			klog.V(10).Infof("reconcileLoadBalancer for service (%s)(%t): lb probe(%s) - adding", serviceName, wantLb, *expectedProbe.Name)
			updatedProbes = append(updatedProbes, expectedProbe)
			dirtyProbes = true
		}
	}
	if dirtyProbes {
		probesJSON, _ := json.Marshal(expectedProbes)
		klog.V(2).Infof("reconcileLoadBalancer for service (%s)(%t): lb probes updated: %s", serviceName, wantLb, string(probesJSON))
		lb.Properties.Probes = updatedProbes
	}
	return dirtyProbes
}

func (az *Cloud) reconcileLBRules(lb *armnetwork.LoadBalancer, service *v1.Service, serviceName string, wantLb bool, expectedRules []*armnetwork.LoadBalancingRule) bool {
	// update rules
	dirtyRules := false
	var updatedRules []*armnetwork.LoadBalancingRule
	if lb.Properties.LoadBalancingRules != nil {
		updatedRules = lb.Properties.LoadBalancingRules
	}

	// update rules: remove unwanted
	for i := len(updatedRules) - 1; i >= 0; i-- {
		existingRule := updatedRules[i]
		if az.serviceOwnsRule(service, *existingRule.Name) {
			keepRule := false
			klog.V(10).Infof("reconcileLoadBalancer for service (%s)(%t): lb rule(%s) - considering evicting", serviceName, wantLb, *existingRule.Name)
			if findRule(expectedRules, existingRule, wantLb) {
				klog.V(10).Infof("reconcileLoadBalancer for service (%s)(%t): lb rule(%s) - keeping", serviceName, wantLb, *existingRule.Name)
				keepRule = true
			}
			if !keepRule {
				klog.V(2).Infof("reconcileLoadBalancer for service (%s)(%t): lb rule(%s) - dropping", serviceName, wantLb, *existingRule.Name)
				updatedRules = append(updatedRules[:i], updatedRules[i+1:]...)
				dirtyRules = true
			}
		}
	}
	// update rules: add needed
	for _, expectedRule := range expectedRules {
		foundRule := false
		if findRule(updatedRules, expectedRule, wantLb) {
			klog.V(10).Infof("reconcileLoadBalancer for service (%s)(%t): lb rule(%s) - already exists", serviceName, wantLb, *expectedRule.Name)
			foundRule = true
		}
		if !foundRule {
			klog.V(10).Infof("reconcileLoadBalancer for service (%s)(%t): lb rule(%s) adding", serviceName, wantLb, *expectedRule.Name)
			updatedRules = append(updatedRules, expectedRule)
			dirtyRules = true
		}
	}
	if dirtyRules {
		ruleJSON, _ := json.Marshal(expectedRules)
		klog.V(2).Infof("reconcileLoadBalancer for service (%s)(%t): lb rules updated: %s", serviceName, wantLb, string(ruleJSON))
		lb.Properties.LoadBalancingRules = updatedRules
	}
	return dirtyRules
}

func (az *Cloud) reconcileFrontendIPConfigs(
	ctx context.Context,
	clusterName string,
	service *v1.Service,
	lb *armnetwork.LoadBalancer,
	status *v1.LoadBalancerStatus,
	wantLb bool,
	lbFrontendIPConfigNames map[bool]string,
) ([]*armnetwork.FrontendIPConfiguration, []*armnetwork.FrontendIPConfiguration, bool, error) {
	var err error
	lbName := *lb.Name
	serviceName := getServiceName(service)
	isInternal := requiresInternalLoadBalancer(service)
	dirtyConfigs := false
	var newConfigs []*armnetwork.FrontendIPConfiguration
	var toDeleteConfigs []*armnetwork.FrontendIPConfiguration
	if lb.Properties.FrontendIPConfigurations != nil {
		newConfigs = lb.Properties.FrontendIPConfigurations
	}

	var ownedFIPConfigs []*armnetwork.FrontendIPConfiguration
	if !wantLb {
		for i := len(newConfigs) - 1; i >= 0; i-- {
			config := newConfigs[i]
			isServiceOwnsFrontendIP, _, _ := az.serviceOwnsFrontendIP(ctx, config, service)
			if isServiceOwnsFrontendIP {
				unsafe, err := az.isFrontendIPConfigUnsafeToDelete(lb, service, config.ID)
				if err != nil {
					return nil, toDeleteConfigs, false, err
				}

				// If the frontend IP configuration is not being referenced by:
				// 1. loadBalancing rules of other services with different ports;
				// 2. outbound rules;
				// 3. inbound NAT rules;
				// 4. inbound NAT pools,
				// do the deletion, or skip it.
				if !unsafe {
					var configNameToBeDeleted string
					if newConfigs[i].Name != nil {
						configNameToBeDeleted = *newConfigs[i].Name
						klog.V(2).Infof("reconcileLoadBalancer for service (%s)(%t): lb frontendconfig(%s) - dropping", serviceName, wantLb, configNameToBeDeleted)
					} else {
						klog.V(2).Infof("reconcileLoadBalancer for service (%s)(%t): nil name of lb frontendconfig", serviceName, wantLb)
					}

					toDeleteConfigs = append(toDeleteConfigs, newConfigs[i])
					newConfigs = append(newConfigs[:i], newConfigs[i+1:]...)
					dirtyConfigs = true
				}
			}
		}
	} else {
		var (
			previousZone []*string
			isFipChanged bool
			subnet       *armnetwork.Subnet
			existsSubnet bool
		)

		if isInternal {
			subnetName := getInternalSubnet(service)
			if subnetName == nil {
				subnetName = &az.SubnetName
			}

			vnetResourceGroup := ""
			if len(az.VnetResourceGroup) > 0 {
				vnetResourceGroup = az.VnetResourceGroup
			} else {
				vnetResourceGroup = az.ResourceGroup
			}

			subnet, err = az.subnetRepo.Get(ctx, vnetResourceGroup, az.VnetName, *subnetName)
			if existsSubnet, err = errutils.CheckResourceExistsFromAzcoreError(err); !existsSubnet && err != nil {
				return nil, toDeleteConfigs, false, err
			} else if !existsSubnet {
				return nil, toDeleteConfigs, false, fmt.Errorf("ensure(%s): lb(%s) - failed to get subnet: %s/%s", serviceName, lbName, az.VnetName, *subnetName)
			}
		}

		for i := len(newConfigs) - 1; i >= 0; i-- {
			config := newConfigs[i]
			isServiceOwnsFrontendIP, _, fipIPVersion := az.serviceOwnsFrontendIP(ctx, config, service)
			if !isServiceOwnsFrontendIP {
				klog.V(4).Infof("reconcileFrontendIPConfigs for service (%s): the frontend IP configuration %s does not belong to the service", serviceName, ptr.Deref(config.Name, ""))
				continue
			}
			klog.V(4).Infof("reconcileFrontendIPConfigs for service (%s): checking owned frontend IP configuration %s", serviceName, ptr.Deref(config.Name, ""))
			var isIPv6 bool
			var err error
			if fipIPVersion != nil {
				isIPv6 = fipIPVersion == to.Ptr(armnetwork.IPVersionIPv6)
			} else {
				if isIPv6, err = az.isFIPIPv6(service, config); err != nil {
					return nil, toDeleteConfigs, false, err
				}
			}

			isFipChanged, err = az.isFrontendIPChanged(ctx, clusterName, config, service, lbFrontendIPConfigNames[isIPv6], subnet)
			if err != nil {
				return nil, toDeleteConfigs, false, err
			}
			if isFipChanged {
				klog.V(2).Infof("reconcileLoadBalancer for service (%s)(%t): lb frontendconfig(%s) - dropping", serviceName, wantLb, *config.Name)
				toDeleteConfigs = append(toDeleteConfigs, newConfigs[i])
				newConfigs = append(newConfigs[:i], newConfigs[i+1:]...)
				dirtyConfigs = true
				previousZone = config.Zones
			}
		}

		ownedFIPConfigMap, err := az.findFrontendIPConfigsOfService(ctx, newConfigs, service)
		if err != nil {
			return nil, toDeleteConfigs, false, err
		}
		for _, config := range ownedFIPConfigMap {
			ownedFIPConfigs = append(ownedFIPConfigs, config)
		}

		addNewFIPOfService := func(isIPv6 bool) error {
			klog.V(4).Infof("ensure(%s): lb(%s) - creating a new frontend IP config %q (isIPv6=%t)",
				serviceName, lbName, lbFrontendIPConfigNames[isIPv6], isIPv6)

			// construct FrontendIPConfigurationPropertiesFormat
			var fipConfigurationProperties *armnetwork.FrontendIPConfigurationPropertiesFormat
			if isInternal {
				configProperties := &armnetwork.FrontendIPConfigurationPropertiesFormat{
					Subnet: subnet,
				}

				if isIPv6 {
					configProperties.PrivateIPAddressVersion = to.Ptr(armnetwork.IPVersionIPv6)
				}

				loadBalancerIP := getServiceLoadBalancerIP(service, isIPv6)
				privateIP := ""
				ingressIPInSubnet := func(ingresses []v1.LoadBalancerIngress) bool {
					for _, ingress := range ingresses {
						ingressIP := ingress.IP
						if (net.ParseIP(ingressIP).To4() == nil) == isIPv6 && ipInSubnet(ingressIP, subnet) {
							privateIP = ingressIP
							break
						}
					}
					return privateIP != ""
				}
				if loadBalancerIP != "" {
					klog.V(4).Infof("reconcileFrontendIPConfigs for service (%s): use loadBalancerIP %q from Service spec", serviceName, loadBalancerIP)
					configProperties.PrivateIPAllocationMethod = to.Ptr(armnetwork.IPAllocationMethodStatic)
					configProperties.PrivateIPAddress = &loadBalancerIP
				} else if status != nil && len(status.Ingress) > 0 && ingressIPInSubnet(status.Ingress) {
					klog.V(4).Infof("reconcileFrontendIPConfigs for service (%s): keep the original private IP %s", serviceName, privateIP)
					configProperties.PrivateIPAllocationMethod = to.Ptr(armnetwork.IPAllocationMethodStatic)
					configProperties.PrivateIPAddress = ptr.To(privateIP)
				} else if len(service.Status.LoadBalancer.Ingress) > 0 && ingressIPInSubnet(service.Status.LoadBalancer.Ingress) {
					klog.V(4).Infof("reconcileFrontendIPConfigs for service (%s): keep the original private IP %s from service.status.loadbalacner.ingress", serviceName, privateIP)
					configProperties.PrivateIPAllocationMethod = to.Ptr(armnetwork.IPAllocationMethodStatic)
					configProperties.PrivateIPAddress = ptr.To(privateIP)
				} else {
					// We'll need to call GetLoadBalancer later to retrieve allocated IP.
					klog.V(4).Infof("reconcileFrontendIPConfigs for service (%s): dynamically allocate the private IP", serviceName)
					configProperties.PrivateIPAllocationMethod = to.Ptr(armnetwork.IPAllocationMethodDynamic)
				}

				fipConfigurationProperties = configProperties
			} else {
				pipName, shouldPIPExisted, err := az.determinePublicIPName(ctx, clusterName, service, isIPv6)
				if err != nil {
					return err
				}
				domainNameLabel, found := getPublicIPDomainNameLabel(service)
				pip, err := az.ensurePublicIPExists(ctx, service, pipName, domainNameLabel, clusterName, shouldPIPExisted, found, isIPv6)
				if err != nil {
					return err
				}
				fipConfigurationProperties = &armnetwork.FrontendIPConfigurationPropertiesFormat{
					PublicIPAddress: &armnetwork.PublicIPAddress{ID: pip.ID},
				}
			}

			newConfig := &armnetwork.FrontendIPConfiguration{
				Name:       ptr.To(lbFrontendIPConfigNames[isIPv6]),
				ID:         ptr.To(fmt.Sprintf(consts.FrontendIPConfigIDTemplate, az.getNetworkResourceSubscriptionID(), az.ResourceGroup, ptr.Deref(lb.Name, ""), lbFrontendIPConfigNames[isIPv6])),
				Properties: fipConfigurationProperties,
			}

			if isInternal {
				if err := az.getFrontendZones(ctx, newConfig, previousZone, isFipChanged, serviceName, lbFrontendIPConfigNames[isIPv6]); err != nil {
					klog.Errorf("reconcileLoadBalancer for service (%s)(%t): failed to getFrontendZones: %s", serviceName, wantLb, err.Error())
					return err
				}
			}
			newConfigs = append(newConfigs, newConfig)
			klog.V(2).Infof("reconcileLoadBalancer for service (%s)(%t): lb frontendconfig(%s) - adding", serviceName, wantLb, lbFrontendIPConfigNames[isIPv6])
			dirtyConfigs = true
			return nil
		}

		v4Enabled, v6Enabled := getIPFamiliesEnabled(service)
		if v4Enabled && ownedFIPConfigMap[false] == nil {
			if err := addNewFIPOfService(false); err != nil {
				return nil, toDeleteConfigs, false, err
			}
		}
		if v6Enabled && ownedFIPConfigMap[true] == nil {
			if err := addNewFIPOfService(true); err != nil {
				return nil, toDeleteConfigs, false, err
			}
		}
	}

	if dirtyConfigs {
		lb.Properties.FrontendIPConfigurations = newConfigs
	}

	return ownedFIPConfigs, toDeleteConfigs, dirtyConfigs, err
}

func (az *Cloud) getFrontendZones(
	ctx context.Context,
	fipConfig *armnetwork.FrontendIPConfiguration,
	previousZone []*string,
	isFipChanged bool,
	serviceName, lbFrontendIPConfigName string,
) error {
	if !isFipChanged { // fetch zone information from API for new frontends
		// only add zone information for new internal frontend IP configurations for standard load balancer not deployed to an edge zone.
		location := az.Location
		zones, err := az.getRegionZonesBackoff(ctx, location)
		if err != nil {
			return err
		}
		if az.UseStandardLoadBalancer() && len(zones) > 0 && !az.HasExtendedLocation() {
			fipConfig.Zones = zones
		}
	} else {
		if previousZone == nil { // keep the existing zone information for existing frontends
			klog.V(2).Infof("getFrontendZones for service (%s): lb frontendconfig(%s): setting zone to nil", serviceName, lbFrontendIPConfigName)
		} else {
			zoneStr := strings.Join(lo.FromSlicePtr(previousZone), ",")
			klog.V(2).Infof("getFrontendZones for service (%s): lb frontendconfig(%s): setting zone to %s", serviceName, lbFrontendIPConfigName, zoneStr)
		}
		fipConfig.Zones = previousZone
	}
	return nil
}

// checkLoadBalancerResourcesConflicts checks if the service is consuming
// ports which conflict with the existing loadBalancer resources,
// including inbound NAT rule, inbound NAT pools and loadBalancing rules
func (az *Cloud) checkLoadBalancerResourcesConflicts(
	lb *armnetwork.LoadBalancer,
	frontendIPConfigID string,
	service *v1.Service,
) error {
	if service.Spec.Ports == nil {
		return nil
	}
	ports := service.Spec.Ports

	for _, port := range ports {
		if lb.Properties.LoadBalancingRules != nil {
			for _, rule := range lb.Properties.LoadBalancingRules {
				if lbRuleConflictsWithPort(rule, frontendIPConfigID, port) {
					// ignore self-owned rules for unit test
					if rule.Name != nil && az.serviceOwnsRule(service, *rule.Name) {
						continue
					}
					return fmt.Errorf("checkLoadBalancerResourcesConflicts: service port %s is trying to "+
						"consume the port %d which is being referenced by an existing loadBalancing rule %s with "+
						"the same protocol %s and frontend IP config with ID %s",
						port.Name,
						*rule.Properties.FrontendPort,
						*rule.Name,
						*rule.Properties.Protocol,
						*rule.Properties.FrontendIPConfiguration.ID)
				}
			}
		}

		if lb.Properties.InboundNatRules != nil {
			for _, inboundNatRule := range lb.Properties.InboundNatRules {
				if inboundNatRuleConflictsWithPort(inboundNatRule, frontendIPConfigID, port) {
					return fmt.Errorf("checkLoadBalancerResourcesConflicts: service port %s is trying to "+
						"consume the port %d which is being referenced by an existing inbound NAT rule %s with "+
						"the same protocol %s and frontend IP config with ID %s",
						port.Name,
						*inboundNatRule.Properties.FrontendPort,
						*inboundNatRule.Name,
						*inboundNatRule.Properties.Protocol,
						*inboundNatRule.Properties.FrontendIPConfiguration.ID)
				}
			}
		}

		if lb.Properties.InboundNatPools != nil {
			for _, pool := range lb.Properties.InboundNatPools {
				if inboundNatPoolConflictsWithPort(pool, frontendIPConfigID, port) {
					return fmt.Errorf("checkLoadBalancerResourcesConflicts: service port %s is trying to "+
						"consume the port %d which is being in the range (%d-%d) of an existing "+
						"inbound NAT pool %s with the same protocol %s and frontend IP config with ID %s",
						port.Name,
						port.Port,
						*pool.Properties.FrontendPortRangeStart,
						*pool.Properties.FrontendPortRangeEnd,
						*pool.Name,
						*pool.Properties.Protocol,
						*pool.Properties.FrontendIPConfiguration.ID)
				}
			}
		}
	}

	return nil
}

func inboundNatPoolConflictsWithPort(pool *armnetwork.InboundNatPool, frontendIPConfigID string, port v1.ServicePort) bool {
	return pool.Properties != nil &&
		pool.Properties.FrontendIPConfiguration != nil &&
		pool.Properties.FrontendIPConfiguration.ID != nil &&
		strings.EqualFold(*pool.Properties.FrontendIPConfiguration.ID, frontendIPConfigID) &&
		strings.EqualFold(string(*pool.Properties.Protocol), string(port.Protocol)) &&
		pool.Properties.FrontendPortRangeStart != nil &&
		pool.Properties.FrontendPortRangeEnd != nil &&
		*pool.Properties.FrontendPortRangeStart <= port.Port &&
		*pool.Properties.FrontendPortRangeEnd >= port.Port
}

func inboundNatRuleConflictsWithPort(inboundNatRule *armnetwork.InboundNatRule, frontendIPConfigID string, port v1.ServicePort) bool {
	return inboundNatRule.Properties != nil &&
		inboundNatRule.Properties.FrontendIPConfiguration != nil &&
		inboundNatRule.Properties.FrontendIPConfiguration.ID != nil &&
		strings.EqualFold(*inboundNatRule.Properties.FrontendIPConfiguration.ID, frontendIPConfigID) &&
		strings.EqualFold(string(*inboundNatRule.Properties.Protocol), string(port.Protocol)) &&
		inboundNatRule.Properties.FrontendPort != nil &&
		*inboundNatRule.Properties.FrontendPort == port.Port
}

func lbRuleConflictsWithPort(rule *armnetwork.LoadBalancingRule, frontendIPConfigID string, port v1.ServicePort) bool {
	return rule.Properties != nil &&
		rule.Properties.FrontendIPConfiguration != nil &&
		rule.Properties.FrontendIPConfiguration.ID != nil &&
		strings.EqualFold(*rule.Properties.FrontendIPConfiguration.ID, frontendIPConfigID) &&
		strings.EqualFold(string(*rule.Properties.Protocol), string(port.Protocol)) &&
		rule.Properties.FrontendPort != nil &&
		*rule.Properties.FrontendPort == port.Port
}

// buildLBRules
// for following SKU: basic loadbalancer vs standard load balancer
// for following scenario: internal vs external
func (az *Cloud) getExpectedLBRules(
	service *v1.Service,
	lbFrontendIPConfigID string,
	lbBackendPoolID string,
	lbName string,
	isIPv6 bool,
) ([]*armnetwork.Probe, []*armnetwork.LoadBalancingRule, error) {
	var expectedRules []*armnetwork.LoadBalancingRule
	var expectedProbes []*armnetwork.Probe

	// support podPresence health check when External Traffic Policy is local
	// take precedence over user defined probe configuration
	// healthcheck proxy server serves http requests
	// https://github.com/kubernetes/kubernetes/blob/7c013c3f64db33cf19f38bb2fc8d9182e42b0b7b/pkg/proxy/healthcheck/service_health.go#L236
	var nodeEndpointHealthprobe *armnetwork.Probe
	var nodeEndpointHealthprobeAdded bool
	if servicehelpers.NeedsHealthCheck(service) && !(consts.IsPLSEnabled(service.Annotations) && consts.IsPLSProxyProtocolEnabled(service.Annotations)) {
		podPresencePath, podPresencePort := servicehelpers.GetServiceHealthCheckPathPort(service)
		lbRuleName := az.getLoadBalancerRuleName(service, v1.ProtocolTCP, podPresencePort, isIPv6)
		probeInterval, numberOfProbes, err := az.getHealthProbeConfigProbeIntervalAndNumOfProbe(service, podPresencePort)
		if err != nil {
			return nil, nil, err
		}
		nodeEndpointHealthprobe = &armnetwork.Probe{
			Name: &lbRuleName,
			Properties: &armnetwork.ProbePropertiesFormat{
				RequestPath:       ptr.To(podPresencePath),
				Protocol:          to.Ptr(armnetwork.ProbeProtocolHTTP),
				Port:              ptr.To(podPresencePort),
				IntervalInSeconds: probeInterval,
				ProbeThreshold:    numberOfProbes,
			},
		}
	}

	var useSharedProbe bool
	if az.useSharedLoadBalancerHealthProbeMode() &&
		!strings.EqualFold(string(service.Spec.ExternalTrafficPolicy), string(v1.ServiceExternalTrafficPolicyLocal)) {
		nodeEndpointHealthprobe = az.buildClusterServiceSharedProbe()
		useSharedProbe = true
	}

	// In HA mode, lb forward traffic of all port to backend
	// HA mode is only supported on standard loadbalancer SKU in internal mode
	if consts.IsK8sServiceUsingInternalLoadBalancer(service) &&
		az.UseStandardLoadBalancer() &&
		consts.IsK8sServiceHasHAModeEnabled(service) {

		lbRuleName := az.getloadbalancerHAmodeRuleName(service, isIPv6)
		klog.V(2).Infof("getExpectedLBRules lb name (%s) rule name (%s)", lbName, lbRuleName)

		props, err := az.getExpectedHAModeLoadBalancingRuleProperties(service, lbFrontendIPConfigID, lbBackendPoolID)
		if err != nil {
			return nil, nil, fmt.Errorf("error generate lb rule for ha mod loadbalancer. err: %w", err)
		}
		// Here we need to find one health probe rule for the HA lb rule.
		if nodeEndpointHealthprobe == nil {
			// use user customized health probe rule if any
			for _, port := range service.Spec.Ports {
				portprobe, err := az.buildHealthProbeRulesForPort(service, port, lbRuleName, nil, false)
				if err != nil {
					klog.V(2).ErrorS(err, "error occurred when buildHealthProbeRulesForPort", "service", service.Name, "namespace", service.Namespace,
						"rule-name", lbRuleName, "port", port.Port)
					// ignore error because we only need one correct rule
				}
				if portprobe != nil {
					props.Probe = &armnetwork.SubResource{
						ID: ptr.To(az.getLoadBalancerProbeID(lbName, *portprobe.Name)),
					}
					expectedProbes = append(expectedProbes, portprobe)
					break
				}
			}
		} else {
			props.Probe = &armnetwork.SubResource{
				ID: ptr.To(az.getLoadBalancerProbeID(lbName, *nodeEndpointHealthprobe.Name)),
			}
			expectedProbes = append(expectedProbes, nodeEndpointHealthprobe)
		}

		expectedRules = append(expectedRules, &armnetwork.LoadBalancingRule{
			Name:       &lbRuleName,
			Properties: props,
		})
		// end of HA mode handling
	} else {
		// generate lb rule for each port defined in svc object

		for _, port := range service.Spec.Ports {
			lbRuleName := az.getLoadBalancerRuleName(service, port.Protocol, port.Port, isIPv6)
			klog.V(2).Infof("getExpectedLBRules lb name (%s) rule name (%s)", lbName, lbRuleName)
			isNoLBRuleRequired, err := consts.IsLBRuleOnK8sServicePortDisabled(service.Annotations, port.Port)
			if err != nil {
				err := fmt.Errorf("failed to parse annotation %s: %w", consts.BuildAnnotationKeyForPort(port.Port, consts.PortAnnotationNoLBRule), err)
				klog.V(2).ErrorS(err, "error occurred when getExpectedLoadBalancingRulePropertiesForPort", "service", service.Name, "namespace", service.Namespace,
					"rule-name", lbRuleName, "port", port.Port)
			}
			if isNoLBRuleRequired {
				klog.V(2).Infof("getExpectedLBRules lb name (%s) rule name (%s) no lb rule required", lbName, lbRuleName)
				continue
			}
			if port.Protocol == v1.ProtocolSCTP && !(az.UseStandardLoadBalancer() && consts.IsK8sServiceUsingInternalLoadBalancer(service)) {
				return expectedProbes, expectedRules, fmt.Errorf("SCTP is only supported on standard loadbalancer in internal mode")
			}

			transportProto, _, _, err := getProtocolsFromKubernetesProtocol(port.Protocol)
			if err != nil {
				return expectedProbes, expectedRules, fmt.Errorf("failed to parse transport protocol: %w", err)
			}
			props, err := az.getExpectedLoadBalancingRulePropertiesForPort(service, lbFrontendIPConfigID, lbBackendPoolID, port, transportProto)
			if err != nil {
				return expectedProbes, expectedRules, fmt.Errorf("error generate lb rule for ha mod loadbalancer. err: %w", err)
			}

			isNoHealthProbeRule, err := consts.IsHealthProbeRuleOnK8sServicePortDisabled(service.Annotations, port.Port)
			if err != nil {
				err := fmt.Errorf("failed to parse annotation %s: %w", consts.BuildAnnotationKeyForPort(port.Port, consts.PortAnnotationNoHealthProbeRule), err)
				klog.V(2).ErrorS(err, "error occurred when buildHealthProbeRulesForPort", "service", service.Name, "namespace", service.Namespace,
					"rule-name", lbRuleName, "port", port.Port)
			}
			if !isNoHealthProbeRule {
				portprobe, err := az.buildHealthProbeRulesForPort(service, port, lbRuleName, nodeEndpointHealthprobe, useSharedProbe)
				if err != nil {
					klog.V(2).ErrorS(err, "error occurred when buildHealthProbeRulesForPort", "service", service.Name, "namespace", service.Namespace,
						"rule-name", lbRuleName, "port", port.Port)
					return expectedProbes, expectedRules, err
				}
				if portprobe != nil {
					props.Probe = &armnetwork.SubResource{
						ID: ptr.To(az.getLoadBalancerProbeID(lbName, *portprobe.Name)),
					}
					expectedProbes = append(expectedProbes, portprobe)
				} else if nodeEndpointHealthprobe != nil {
					props.Probe = &armnetwork.SubResource{
						ID: ptr.To(az.getLoadBalancerProbeID(lbName, *nodeEndpointHealthprobe.Name)),
					}
					if !nodeEndpointHealthprobeAdded {
						expectedProbes = append(expectedProbes, nodeEndpointHealthprobe)
						nodeEndpointHealthprobeAdded = true
					}
				}
			}
			if consts.IsK8sServiceDisableLoadBalancerFloatingIP(service) {
				props.BackendPort = ptr.To(port.NodePort)
				props.EnableFloatingIP = ptr.To(false)
			}
			expectedRules = append(expectedRules, &armnetwork.LoadBalancingRule{
				Name:       &lbRuleName,
				Properties: props,
			})
		}
	}

	return expectedProbes, expectedRules, nil
}

// getDefaultLoadBalancingRulePropertiesFormat returns the loadbalancing rule for one port
func (az *Cloud) getExpectedLoadBalancingRulePropertiesForPort(
	service *v1.Service,
	lbFrontendIPConfigID string,
	lbBackendPoolID string, servicePort v1.ServicePort, transportProto *armnetwork.TransportProtocol,
) (*armnetwork.LoadBalancingRulePropertiesFormat, error) {
	var err error

	loadDistribution := to.Ptr(armnetwork.LoadDistributionDefault)
	if service.Spec.SessionAffinity == v1.ServiceAffinityClientIP {
		loadDistribution = to.Ptr(armnetwork.LoadDistributionSourceIP)
	}

	var lbIdleTimeout *int32
	if lbIdleTimeout, err = consts.Getint32ValueFromK8sSvcAnnotation(service.Annotations, consts.ServiceAnnotationLoadBalancerIdleTimeout, func(val *int32) error {
		const (
			idleTimoutMin  = 4
			idleTimeoutMax = 100
		)
		if *val < idleTimoutMin || *val > idleTimeoutMax {
			return fmt.Errorf("idle timeout value must be a whole number representing minutes between %d and %d, actual value: %d", idleTimoutMin, idleTimeoutMax, *val)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("error parsing idle timeout key: %s, err: %w", consts.ServiceAnnotationLoadBalancerIdleTimeout, err)
	} else if lbIdleTimeout == nil {
		lbIdleTimeout = ptr.To(int32(4))
	}

	props := &armnetwork.LoadBalancingRulePropertiesFormat{
		Protocol:            transportProto,
		FrontendPort:        ptr.To(servicePort.Port),
		BackendPort:         ptr.To(servicePort.Port),
		DisableOutboundSnat: ptr.To(az.DisableLoadBalancerOutboundSNAT()),
		EnableFloatingIP:    ptr.To(true),
		LoadDistribution:    loadDistribution,
		FrontendIPConfiguration: &armnetwork.SubResource{
			ID: ptr.To(lbFrontendIPConfigID),
		},
		BackendAddressPool: &armnetwork.SubResource{
			ID: ptr.To(lbBackendPoolID),
		},
		IdleTimeoutInMinutes: lbIdleTimeout,
	}
	if strings.EqualFold(string(*transportProto), string(armnetwork.TransportProtocolTCP)) && az.UseStandardLoadBalancer() {
		props.EnableTCPReset = ptr.To(!consts.IsTCPResetDisabled(service.Annotations))
	}

	// Azure ILB does not support secondary IPs as floating IPs on the LB. Therefore, floating IP needs to be turned
	// off and the rule should point to the nodeIP:nodePort.
	if consts.IsK8sServiceUsingInternalLoadBalancer(service) && isBackendPoolIPv6(lbBackendPoolID) {
		props.BackendPort = ptr.To(servicePort.NodePort)
		props.EnableFloatingIP = ptr.To(false)
	}
	return props, nil
}

// getExpectedHAModeLoadBalancingRuleProperties build load balancing rule for lb in HA mode
func (az *Cloud) getExpectedHAModeLoadBalancingRuleProperties(
	service *v1.Service,
	lbFrontendIPConfigID string,
	lbBackendPoolID string,
) (*armnetwork.LoadBalancingRulePropertiesFormat, error) {
	props, err := az.getExpectedLoadBalancingRulePropertiesForPort(service, lbFrontendIPConfigID, lbBackendPoolID, v1.ServicePort{}, to.Ptr(armnetwork.TransportProtocolAll))
	if err != nil {
		return nil, fmt.Errorf("error generate lb rule for ha mod loadbalancer. err: %w", err)
	}
	props.EnableTCPReset = ptr.To(!consts.IsTCPResetDisabled(service.Annotations))

	return props, nil
}

// This reconciles the Network Security Group similar to how the LB is reconciled.
// This entails adding required, missing SecurityRules and removing stale rules.
func (az *Cloud) reconcileSecurityGroup(
	ctx context.Context,
	clusterName string, service *v1.Service,
	lbName string,
	lbIPs []string, wantLb bool,
) (*armnetwork.SecurityGroup, error) {
	logger := log.FromContextOrBackground(ctx).WithName("reconcileSecurityGroup").
		WithValues("load-balancer", lbName).
		WithValues("delete-lb", !wantLb)
	logger.V(2).Info("Starting")
	ctx = log.NewContext(ctx, logger)

	if wantLb && len(lbIPs) == 0 {
		return nil, fmt.Errorf("no load balancer IP for setting up security rules for service %s", service.Name)
	}

	additionalIPs, err := loadbalancer.AdditionalPublicIPs(service)
	if wantLb && err != nil {
		return nil, fmt.Errorf("unable to get additional public IPs: %w", err)
	}

	var accessControl *loadbalancer.AccessControl
	{
		sg, err := az.nsgRepo.GetSecurityGroup(ctx)
		if err != nil {
			return nil, err
		}

		var opts []loadbalancer.AccessControlOption
		if !wantLb {
			// When deleting LB, we don't need to validate the annotation
			opts = append(opts, loadbalancer.WithEventEmitter(az.Event))
		}
		accessControl, err = loadbalancer.NewAccessControl(logger, service, sg, opts...)
		if err != nil {
			logger.Error(err, "Failed to parse access control configuration for service")
			return nil, err
		}
	}

	var (
		disableFloatingIP                                = consts.IsK8sServiceDisableLoadBalancerFloatingIP(service)
		lbIPAddresses, _                                 = iputil.ParseAddresses(lbIPs)
		lbIPv4Addresses, lbIPv6Addresses                 = iputil.GroupAddressesByFamily(lbIPAddresses)
		additionalIPv4Addresses, additionalIPv6Addresses = iputil.GroupAddressesByFamily(additionalIPs)
		backendIPv4Addresses, backendIPv6Addresses       []netip.Addr
	)
	{
		// Get backend node IPs
		lb, lbFound, err := az.getAzureLoadBalancer(ctx, lbName, azcache.CacheReadTypeDefault)
		{
			if err != nil {
				return nil, err
			}
			if wantLb && !lbFound {
				logger.Error(err, "Failed to get load balancer")
				return nil, fmt.Errorf("unable to get lb %s", lbName)
			}
		}
		var backendIPv4List, backendIPv6List []string
		if lbFound {
			backendIPv4List, backendIPv6List = az.LoadBalancerBackendPool.GetBackendPrivateIPs(ctx, clusterName, service, lb)
		}
		backendIPv4Addresses, _ = iputil.ParseAddresses(backendIPv4List)
		backendIPv6Addresses, _ = iputil.ParseAddresses(backendIPv6List)
	}

	var (
		dstIPv4Addresses = additionalIPv4Addresses
		dstIPv6Addresses = additionalIPv6Addresses
	)

	if disableFloatingIP {
		// use the backend node IPs
		dstIPv4Addresses = append(dstIPv4Addresses, backendIPv4Addresses...)
		dstIPv6Addresses = append(dstIPv6Addresses, backendIPv6Addresses...)
	} else {
		// use the LoadBalancer IPs
		dstIPv4Addresses = append(dstIPv4Addresses, lbIPv4Addresses...)
		dstIPv6Addresses = append(dstIPv6Addresses, lbIPv6Addresses...)
	}

	{
		retainPortRanges, err := az.listSharedIPPortMapping(ctx, service, append(dstIPv4Addresses, dstIPv6Addresses...))
		if err != nil {
			logger.Error(err, "Failed to list retain port ranges")
			return nil, err
		}

		if err := accessControl.CleanSecurityGroup(dstIPv4Addresses, dstIPv6Addresses, retainPortRanges); err != nil {
			logger.Error(err, "Failed to clean security group")
			return nil, err
		}
	}

	if wantLb {
		err := accessControl.PatchSecurityGroup(dstIPv4Addresses, dstIPv6Addresses)
		if err != nil {
			logger.Error(err, "Failed to patch security group")
			return nil, err
		}
	}

	rv, updated, err := accessControl.SecurityGroup()
	if err != nil {
		err = fmt.Errorf("unable to apply access control configuration to security group: %w", err)
		logger.Error(err, "Failed to get security group after patching")
		return nil, err
	}
	if az.ensureSecurityGroupTagged(rv) {
		updated = true
	}

	if updated {
		logger.V(2).Info("Preparing to update security group")
		logger.V(5).Info("CreateOrUpdateSecurityGroup begin")
		err := az.nsgRepo.CreateOrUpdateSecurityGroup(ctx, rv)
		if err != nil {
			logger.Error(err, "Failed to update security group")
			return nil, err
		}
		logger.V(5).Info("CreateOrUpdateSecurityGroup end")
	}
	return rv, nil
}

func (az *Cloud) shouldUpdateLoadBalancer(ctx context.Context, clusterName string, service *v1.Service, nodes []*v1.Node) (bool, error) {
	existingManagedLBs, err := az.ListManagedLBs(ctx, service, nodes, clusterName)
	if err != nil {
		return false, fmt.Errorf("shouldUpdateLoadBalancer: failed to list managed load balancers: %w", err)
	}

	_, _, _, _, existsLb, _, _ := az.getServiceLoadBalancer(ctx, service, clusterName, nodes, false, existingManagedLBs)
	return existsLb && service.ObjectMeta.DeletionTimestamp == nil && service.Spec.Type == v1.ServiceTypeLoadBalancer, nil
}

// Determine if we should release existing owned public IPs
// FIXME: This function is a bit of a mess, and could use some refactoring.
func shouldReleaseExistingOwnedPublicIP(
	existingPip *armnetwork.PublicIPAddress,
	serviceReferences []string,
	lbShouldExist, lbIsInternal, isUserAssignedPIP bool,
	desiredPipName string,
	ipTagRequest serviceIPTagRequest,
) bool {
	// skip deleting user created pip
	if isUserAssignedPIP {
		return false
	}

	// Latch some variables for readability purposes.
	pipName := *existingPip.Name

	// Assume the current IP Tags are empty by default unless properties specify otherwise.
	currentIPTags := []*armnetwork.IPTag{}
	if existingPip.Properties != nil {
		currentIPTags = existingPip.Properties.IPTags
	}

	// Check whether the public IP is being referenced by other service.
	// The owned public IP can be released only when there is not other service using it.
	// case 1: there is at least one reference when deleting the PIP
	if !lbShouldExist && len(serviceReferences) > 0 {
		return false
	}

	// case 2: there is at least one reference from other service
	if lbShouldExist && len(serviceReferences) > 1 {
		return false
	}

	// Release the ip under the following criteria -
	// #1 - If we don't actually want a load balancer,
	return !lbShouldExist ||
		// #2 - If the load balancer is internal, and thus doesn't require public exposure
		lbIsInternal ||
		// #3 - If the name of this public ip does not match the desired name,
		// NOTICE: For IPv6 Service created with CCM v1.27.1, the created PIP has IPv6 suffix.
		// We need to recreate such PIP and current logic to delete needs no change.
		(pipName != desiredPipName) ||
		// #4 If the service annotations have specified the ip tags that the public ip must have, but they do not match the ip tags of the existing instance
		(ipTagRequest.IPTagsRequestedByAnnotation && !areIPTagsEquivalent(currentIPTags, ipTagRequest.IPTags))
}

// ensurePIPTagged ensures the public IP of the service is tagged as configured
func (az *Cloud) ensurePIPTagged(service *v1.Service, pip *armnetwork.PublicIPAddress) bool {
	configTags := parseTags(az.Tags, az.TagsMap)
	annotationTags := make(map[string]*string)
	if _, ok := service.Annotations[consts.ServiceAnnotationAzurePIPTags]; ok {
		annotationTags = parseTags(service.Annotations[consts.ServiceAnnotationAzurePIPTags], map[string]string{})
	}

	for k, v := range annotationTags {
		found, key := findKeyInMapCaseInsensitive(configTags, k)
		if !found {
			configTags[k] = v
		} else if !strings.EqualFold(ptr.Deref(v, ""), ptr.Deref(configTags[key], "")) {
			configTags[key] = v
		}
	}

	// include the cluster name and service names tags when comparing
	var clusterName, serviceNames, serviceNameUsingDNS *string
	if v := getClusterFromPIPClusterTags(pip.Tags); v != "" {
		clusterName = &v
	}
	if v := getServiceFromPIPServiceTags(pip.Tags); v != "" {
		serviceNames = &v
	}
	if v := getServiceFromPIPDNSTags(pip.Tags); v != "" {
		serviceNameUsingDNS = &v
	}
	if clusterName != nil {
		configTags[consts.ClusterNameKey] = clusterName
	}
	if serviceNames != nil {
		configTags[consts.ServiceTagKey] = serviceNames
	}
	if serviceNameUsingDNS != nil {
		configTags[consts.ServiceUsingDNSKey] = serviceNameUsingDNS
	}

	tags, changed := az.reconcileTags(pip.Tags, configTags)
	pip.Tags = tags

	return changed
}

// reconcilePublicIPs reconciles the PublicIP resources similar to how the LB is reconciled.
func (az *Cloud) reconcilePublicIPs(ctx context.Context, clusterName string, service *v1.Service, lbName string, wantLb bool) ([]*armnetwork.PublicIPAddress, error) {
	logger := klog.FromContext(ctx).WithName("reconcilePublicIPs").
		WithValues("loadBalancer", lbName)

	pipResourceGroup := az.getPublicIPAddressResourceGroup(service)

	reconciledPIPs := make([]*armnetwork.PublicIPAddress, 0)

	var (
		pips []*armnetwork.PublicIPAddress
		err  error
	)
	pips, err = az.listPIP(ctx, pipResourceGroup, azcache.CacheReadTypeDefault)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(az.ResourceGroup, pipResourceGroup) {
		pipsFromClusterRG, err := az.listPIP(ctx, az.ResourceGroup, azcache.CacheReadTypeDefault)
		if err != nil {
			logger.Error(err, "Failed to list public IPs from cluster resource group", "resourceGroup", az.ResourceGroup)
			return nil, err
		}
		pips = append(pips, pipsFromClusterRG...)
	}

	pipsV4, pipsV6 := []*armnetwork.PublicIPAddress{}, []*armnetwork.PublicIPAddress{}
	for _, pip := range pips {
		if pip.Properties == nil || pip.Properties.PublicIPAddressVersion == nil ||
			*pip.Properties.PublicIPAddressVersion == armnetwork.IPVersionIPv4 {
			pipsV4 = append(pipsV4, pip)
		} else {
			pipsV6 = append(pipsV6, pip)
		}
	}

	v4Enabled, v6Enabled := getIPFamiliesEnabled(service)
	if v4Enabled {
		reconciledPIP, err := az.reconcilePublicIP(ctx, pipsV4, clusterName, service, lbName, wantLb, false)
		if err != nil {
			return reconciledPIPs, err
		}
		if reconciledPIP != nil {
			reconciledPIPs = append(reconciledPIPs, reconciledPIP)
		}
	}
	if v6Enabled {
		reconciledPIP, err := az.reconcilePublicIP(ctx, pipsV6, clusterName, service, lbName, wantLb, true)
		if err != nil {
			return reconciledPIPs, err
		}
		if reconciledPIP != nil {
			reconciledPIPs = append(reconciledPIPs, reconciledPIP)
		}
	}
	return reconciledPIPs, nil
}

// reconcilePublicIP reconciles the PublicIP resources similar to how the LB is reconciled with the specified IP family.
func (az *Cloud) reconcilePublicIP(ctx context.Context, pips []*armnetwork.PublicIPAddress, clusterName string, service *v1.Service, lbName string, wantLb, isIPv6 bool) (*armnetwork.PublicIPAddress, error) {
	logger := klog.FromContext(ctx).WithName("reconcilePublicIP")
	isInternal := requiresInternalLoadBalancer(service)
	serviceName := getServiceName(service)
	serviceIPTagRequest := getServiceIPTagRequestForPublicIP(service)
	pipResourceGroup := az.getPublicIPAddressResourceGroup(service)

	var (
		lb               *armnetwork.LoadBalancer
		desiredPipName   string
		err              error
		shouldPIPExisted bool
	)

	if !isInternal && wantLb {
		desiredPipName, shouldPIPExisted, err = az.determinePublicIPName(ctx, clusterName, service, isIPv6)
		if err != nil {
			return nil, err
		}
	}

	if lbName != "" {
		lb, _, err = az.getAzureLoadBalancer(ctx, lbName, azcache.CacheReadTypeDefault)
		if err != nil {
			return nil, err
		}
	}

	discoveredDesiredPublicIP, pipsToBeDeleted, deletedDesiredPublicIP, pipsToBeUpdated, err := az.getPublicIPUpdates(
		clusterName, service, pips, wantLb, isInternal, desiredPipName, serviceName, serviceIPTagRequest, shouldPIPExisted, isIPv6)
	if err != nil {
		return nil, err
	}

	var deleteFuncs, updateFuncs []func() error
	for _, pip := range pipsToBeUpdated {
		pipCopy := *pip
		updateFuncs = append(updateFuncs, func() error {
			logger.V(2).Info("reconcilePublicIP for service", "service", serviceName, "pip", *pip.Name, "isIPv6", isIPv6, "action", "updating")
			return az.CreateOrUpdatePIP(service, pipResourceGroup, &pipCopy)
		})
	}
	errs := utilerrors.AggregateGoroutines(updateFuncs...)
	if errs != nil {
		return nil, utilerrors.Flatten(errs)
	}

	for _, pip := range pipsToBeDeleted {
		pipCopy := *pip
		deleteFuncs = append(deleteFuncs, func() error {
			pipID := strings.ToLower((ptr.Deref(pipCopy.ID, "")))
			rg, err := getPIPRGFromID(pipID)
			if err != nil {
				logger.Error(err, "Failed to get resource group from PIP ID", "pip-id", pipID)
				return err
			}
			logger.V(2).Info("reconcilePublicIP for service",
				"service", serviceName, "pip", *pip.Name, "rg", rg, "isIPv6", isIPv6, "action", "deleting",
			)
			return az.safeDeletePublicIP(ctx, service, rg, &pipCopy, lb)
		})
	}
	errs = utilerrors.AggregateGoroutines(deleteFuncs...)
	if errs != nil {
		return nil, utilerrors.Flatten(errs)
	}

	if !isInternal && wantLb {
		// Confirm desired public ip resource exists
		var pip *armnetwork.PublicIPAddress
		domainNameLabel, found := getPublicIPDomainNameLabel(service)
		errorIfPublicIPDoesNotExist := shouldPIPExisted && discoveredDesiredPublicIP && !deletedDesiredPublicIP
		if pip, err = az.ensurePublicIPExists(ctx, service, desiredPipName, domainNameLabel, clusterName, errorIfPublicIPDoesNotExist, found, isIPv6); err != nil {
			return nil, err
		}
		return pip, nil
	}
	return nil, nil
}

// getPublicIPUpdates handles one IP family only according to isIPv6 and PIP IP version.
func (az *Cloud) getPublicIPUpdates(
	clusterName string,
	service *v1.Service,
	pips []*armnetwork.PublicIPAddress,
	wantLb bool,
	isInternal bool,
	desiredPipName string,
	serviceName string,
	serviceIPTagRequest serviceIPTagRequest,
	serviceAnnotationRequestsNamedPublicIP,
	isIPv6 bool,
) (bool, []*armnetwork.PublicIPAddress, bool, []*armnetwork.PublicIPAddress, error) {
	var (
		err                       error
		discoveredDesiredPublicIP bool
		deletedDesiredPublicIP    bool
		pipsToBeDeleted           []*armnetwork.PublicIPAddress
		pipsToBeUpdated           []*armnetwork.PublicIPAddress
	)
	for i := range pips {
		pip := pips[i]
		if pip.Properties != nil && pip.Properties.PublicIPAddressVersion != nil {
			if (*pip.Properties.PublicIPAddressVersion == armnetwork.IPVersionIPv4 && isIPv6) ||
				(*pip.Properties.PublicIPAddressVersion == armnetwork.IPVersionIPv6 && !isIPv6) {
				continue
			}
		}

		if pip.Name == nil {
			return false, nil, false, nil, fmt.Errorf("PIP name is empty: %v", pip)
		}
		pipName := *pip.Name

		// If we've been told to use a specific public ip by the client, let's track whether or not it actually existed
		// when we inspect the set in Azure.
		discoveredDesiredPublicIP = discoveredDesiredPublicIP || wantLb && !isInternal && pipName == desiredPipName

		// Now, let's perform additional analysis to determine if we should release the public ips we have found.
		// We can only let them go if (a) they are owned by this service and (b) they meet the criteria for deletion.
		owns, isUserAssignedPIP := serviceOwnsPublicIP(service, pip, clusterName)
		if owns {
			var (
				serviceReferences     = parsePIPServiceTag(ptr.To(getServiceFromPIPServiceTags(pip.Tags)))
				dirtyPIP, toBeDeleted bool
			)
			if !wantLb && !isUserAssignedPIP {
				klog.V(2).Infof("reconcilePublicIP for service(%s): unbinding the service from pip %s", serviceName, *pip.Name)
				if serviceReferences, err = unbindServiceFromPIP(pip, serviceName, isUserAssignedPIP); err != nil {
					return false, nil, false, nil, err
				}
				dirtyPIP = true
			}
			if !isUserAssignedPIP {
				if az.ensurePIPTagged(service, pip) {
					dirtyPIP = true
				}
			}
			if shouldReleaseExistingOwnedPublicIP(pip, serviceReferences, wantLb, isInternal, isUserAssignedPIP, desiredPipName, serviceIPTagRequest) {
				// Then, release the public ip
				pipsToBeDeleted = append(pipsToBeDeleted, pip)

				// Flag if we deleted the desired public ip
				deletedDesiredPublicIP = deletedDesiredPublicIP || pipName == desiredPipName

				// An aside: It would be unusual, but possible, for us to delete a public ip referred to explicitly by name
				// in Service annotations (which is usually reserved for non-service-owned externals), if that IP is tagged as
				// having been owned by a particular Kubernetes cluster.

				// If the pip is going to be deleted, we do not need to update it
				toBeDeleted = true
			}

			// Update tags of PIP only instead of deleting it.
			if !toBeDeleted && dirtyPIP {
				pipsToBeUpdated = append(pipsToBeUpdated, pip)
			}
		}
	}

	if !isInternal && serviceAnnotationRequestsNamedPublicIP && !discoveredDesiredPublicIP && wantLb {
		return false, nil, false, nil, fmt.Errorf("reconcilePublicIP for service(%s): pip(%s) not found", serviceName, desiredPipName)
	}
	return discoveredDesiredPublicIP, pipsToBeDeleted, deletedDesiredPublicIP, pipsToBeUpdated, err
}

// safeDeletePublicIP deletes public IP by removing its reference first.
func (az *Cloud) safeDeletePublicIP(ctx context.Context, service *v1.Service, pipResourceGroup string, pip *armnetwork.PublicIPAddress, lb *armnetwork.LoadBalancer) error {
	// Remove references if pip.IPConfiguration is not nil.
	if pip.Properties != nil &&
		pip.Properties.IPConfiguration != nil {
		// Fetch latest pip to check if the pip in the cache is stale.
		// In some cases the public IP to be deleted is still referencing
		// the frontend IP config on the LB. This is because the pip is
		// stored in the cache and is not up-to-date.
		latestPIP, ok, err := az.getPublicIPAddress(ctx, pipResourceGroup, *pip.Name, azcache.CacheReadTypeForceRefresh)
		if err != nil {
			klog.Errorf("safeDeletePublicIP: failed to get latest public IP %s/%s: %s", pipResourceGroup, *pip.Name, err.Error())
			return err
		}
		if ok && latestPIP.Properties != nil &&
			latestPIP.Properties.IPConfiguration != nil &&
			lb != nil && lb.Properties != nil &&
			lb.Properties.FrontendIPConfigurations != nil {
			referencedLBRules := []*armnetwork.SubResource{}
			frontendIPConfigUpdated := false
			loadBalancerRuleUpdated := false

			// Check whether there are still frontend IP configurations referring to it.
			ipConfigurationID := ptr.Deref(pip.Properties.IPConfiguration.ID, "")
			if ipConfigurationID != "" {
				lbFrontendIPConfigs := lb.Properties.FrontendIPConfigurations
				for i := len(lbFrontendIPConfigs) - 1; i >= 0; i-- {
					config := lbFrontendIPConfigs[i]
					if strings.EqualFold(ipConfigurationID, ptr.Deref(config.ID, "")) {
						if config.Properties != nil &&
							config.Properties.LoadBalancingRules != nil {
							referencedLBRules = config.Properties.LoadBalancingRules
						}

						frontendIPConfigUpdated = true
						lbFrontendIPConfigs = append(lbFrontendIPConfigs[:i], lbFrontendIPConfigs[i+1:]...)
						break
					}
				}

				if frontendIPConfigUpdated {
					lb.Properties.FrontendIPConfigurations = lbFrontendIPConfigs
				}
			}

			// Check whether there are still load balancer rules referring to it.
			if len(referencedLBRules) > 0 {
				referencedLBRuleIDs := utilsets.NewString()
				for _, refer := range referencedLBRules {
					referencedLBRuleIDs.Insert(ptr.Deref(refer.ID, ""))
				}

				if lb.Properties.LoadBalancingRules != nil {
					lbRules := lb.Properties.LoadBalancingRules
					for i := len(lbRules) - 1; i >= 0; i-- {
						ruleID := ptr.Deref(lbRules[i].ID, "")
						if ruleID != "" && referencedLBRuleIDs.Has(ruleID) {
							loadBalancerRuleUpdated = true
							lbRules = append(lbRules[:i], lbRules[i+1:]...)
						}
					}

					if loadBalancerRuleUpdated {
						lb.Properties.LoadBalancingRules = lbRules
					}
				}
			}

			// Update load balancer when frontendIPConfigUpdated or loadBalancerRuleUpdated.
			if frontendIPConfigUpdated || loadBalancerRuleUpdated {
				err := az.CreateOrUpdateLB(ctx, service, *lb)
				if err != nil {
					klog.Errorf("safeDeletePublicIP for service(%s) failed with error: %v", getServiceName(service), err)
					return err
				}
			}
		}
	}

	pipName := ptr.Deref(pip.Name, "")
	klog.V(10).Infof("DeletePublicIP(%s, %q): start", pipResourceGroup, pipName)
	err := az.DeletePublicIP(service, pipResourceGroup, pipName)
	if err != nil {
		return err
	}
	klog.V(10).Infof("DeletePublicIP(%s, %q): end", pipResourceGroup, pipName)

	return nil
}

func findRule(rules []*armnetwork.LoadBalancingRule, rule *armnetwork.LoadBalancingRule, wantLB bool) bool {
	for _, existingRule := range rules {
		if strings.EqualFold(ptr.Deref(existingRule.Name, ""), ptr.Deref(rule.Name, "")) &&
			equalLoadBalancingRulePropertiesFormat(existingRule.Properties, rule.Properties, wantLB) {
			return true
		}
	}
	return false
}

// equalLoadBalancingRulePropertiesFormat checks whether the provided LoadBalancingRulePropertiesFormat are equal.
// Note: only fields used in reconcileLoadBalancer are considered.
// s: existing, t: target
func equalLoadBalancingRulePropertiesFormat(s *armnetwork.LoadBalancingRulePropertiesFormat, t *armnetwork.LoadBalancingRulePropertiesFormat, wantLB bool) bool {
	if s == nil || t == nil {
		return false
	}

	properties := reflect.DeepEqual(s.Protocol, t.Protocol)
	if !properties {
		return false
	}

	if reflect.DeepEqual(s.Protocol, to.Ptr(armnetwork.TransportProtocolTCP)) {
		properties = properties && reflect.DeepEqual(ptr.Deref(s.EnableTCPReset, false), ptr.Deref(t.EnableTCPReset, false))
	}

	properties = properties && equalSubResource(s.FrontendIPConfiguration, t.FrontendIPConfiguration) &&
		equalSubResource(s.BackendAddressPool, t.BackendAddressPool) &&
		reflect.DeepEqual(s.LoadDistribution, t.LoadDistribution) &&
		reflect.DeepEqual(s.FrontendPort, t.FrontendPort) &&
		reflect.DeepEqual(s.BackendPort, t.BackendPort) &&
		equalSubResource(s.Probe, t.Probe) &&
		reflect.DeepEqual(s.EnableFloatingIP, t.EnableFloatingIP) &&
		reflect.DeepEqual(ptr.Deref(s.DisableOutboundSnat, false), ptr.Deref(t.DisableOutboundSnat, false))

	if wantLB && s.IdleTimeoutInMinutes != nil && t.IdleTimeoutInMinutes != nil {
		return properties && reflect.DeepEqual(s.IdleTimeoutInMinutes, t.IdleTimeoutInMinutes)
	}
	return properties
}

func equalSubResource(s *armnetwork.SubResource, t *armnetwork.SubResource) bool {
	if s == nil && t == nil {
		return true
	}
	if s == nil || t == nil {
		return false
	}
	return strings.EqualFold(ptr.Deref(s.ID, ""), ptr.Deref(t.ID, ""))
}

func (az *Cloud) getPublicIPAddressResourceGroup(service *v1.Service) string {
	if resourceGroup, found := service.Annotations[consts.ServiceAnnotationLoadBalancerResourceGroup]; found {
		resourceGroupName := strings.TrimSpace(resourceGroup)
		if len(resourceGroupName) > 0 {
			return resourceGroupName
		}
	}

	return az.ResourceGroup
}

func (az *Cloud) isBackendPoolPreConfigured(service *v1.Service) bool {
	preConfigured := false
	isInternal := requiresInternalLoadBalancer(service)

	if az.PreConfiguredBackendPoolLoadBalancerTypes == consts.PreConfiguredBackendPoolLoadBalancerTypesAll {
		preConfigured = true
	}
	if (az.PreConfiguredBackendPoolLoadBalancerTypes == consts.PreConfiguredBackendPoolLoadBalancerTypesInternal) && isInternal {
		preConfigured = true
	}
	if (az.PreConfiguredBackendPoolLoadBalancerTypes == consts.PreConfiguredBackendPoolLoadBalancerTypesExternal) && !isInternal {
		preConfigured = true
	}

	return preConfigured
}

// Check if service requires an internal load balancer.
func requiresInternalLoadBalancer(service *v1.Service) bool {
	if l, found := service.Annotations[consts.ServiceAnnotationLoadBalancerInternal]; found {
		return l == consts.TrueAnnotationValue
	}

	return false
}

func getInternalSubnet(service *v1.Service) *string {
	if requiresInternalLoadBalancer(service) {
		if l, found := service.Annotations[consts.ServiceAnnotationLoadBalancerInternalSubnet]; found && strings.TrimSpace(l) != "" {
			return &l
		}
	}

	return nil
}

func ipInSubnet(ip string, subnet *armnetwork.Subnet) bool {
	if subnet == nil || subnet.Properties == nil {
		return false
	}
	netIP, err := netip.ParseAddr(ip)
	if err != nil {
		klog.Errorf("ipInSubnet: failed to parse ip %s: %v", netIP, err)
		return false
	}
	cidrs := make([]*string, 0)
	if subnet.Properties.AddressPrefix != nil {
		cidrs = append(cidrs, subnet.Properties.AddressPrefix)
	}
	if subnet.Properties.AddressPrefixes != nil {
		cidrs = append(cidrs, subnet.Properties.AddressPrefixes...)
	}
	for _, cidr := range cidrs {
		network, err := netip.ParsePrefix(*cidr)
		if err != nil {
			klog.Errorf("ipInSubnet: failed to parse ip cidr %s: %v", *cidr, err)
			continue
		}
		if network.Contains(netIP) {
			return true
		}
	}
	return false
}

// getServiceLoadBalancerMode parses the mode value.
// if the value is __auto__ it returns isAuto = TRUE.
// if anything else it returns the unique VM set names after trimming spaces.
func (az *Cloud) getServiceLoadBalancerMode(service *v1.Service) (bool, bool, string) {
	mode, hasMode := service.Annotations[consts.ServiceAnnotationLoadBalancerMode]
	if az.UseStandardLoadBalancer() && hasMode {
		klog.Warningf("single standard load balancer doesn't work with annotation %q, would ignore it", consts.ServiceAnnotationLoadBalancerMode)
	}
	mode = strings.TrimSpace(mode)
	isAuto := strings.EqualFold(mode, consts.ServiceAnnotationLoadBalancerAutoModeValue)

	return hasMode, isAuto, mode
}

func useSharedSecurityRule(service *v1.Service) bool {
	if l, ok := service.Annotations[consts.ServiceAnnotationSharedSecurityRule]; ok {
		return l == consts.TrueAnnotationValue
	}

	return false
}

// serviceOwnsPublicIP checks if the service owns the pip and if the pip is user-created.
// The pip is user-created if and only if there is no service tags.
// The service owns the pip if:
// 1. The serviceName is included in the service tags of a system-created pip.
// 2. The service LoadBalancerIP matches the IP address of a user-created pip.
func serviceOwnsPublicIP(service *v1.Service, pip *armnetwork.PublicIPAddress, clusterName string) (bool, bool) {
	if service == nil || pip == nil {
		klog.Warningf("serviceOwnsPublicIP: nil service or public IP")
		return false, false
	}

	if pip.Properties == nil || ptr.Deref(pip.Properties.IPAddress, "") == "" {
		klog.Warningf("serviceOwnsPublicIP: empty pip.Properties.IPAddress")
		return false, false
	}

	serviceName := getServiceName(service)

	isIPv6 := ptr.Deref(pip.Properties.PublicIPAddressVersion, "") == armnetwork.IPVersionIPv6
	if pip.Tags != nil {
		serviceTag := getServiceFromPIPServiceTags(pip.Tags)
		clusterTag := getClusterFromPIPClusterTags(pip.Tags)

		// if there is no service tag on the pip, it is user-created pip
		if serviceTag == "" {
			return isServiceSelectPIP(service, pip, isIPv6), true
		}

		// if there is service tag on the pip, it is system-created pip
		if isSVCNameInPIPTag(serviceTag, serviceName) {
			// Backward compatible for clusters upgraded from old releases.
			// In such case, only "service" tag is set.
			if clusterTag == "" {
				return true, false
			}

			// If cluster name tag is set, then return true if it matches.
			return strings.EqualFold(clusterTag, clusterName), false
		}

		// if the service is not included in the tags of the system-created pip, check the ip address
		// or pip name, this could happen for secondary services
		return isServiceSelectPIP(service, pip, isIPv6), false
	}

	// if the pip has no tags, it should be user-created
	return isServiceSelectPIP(service, pip, isIPv6), true
}

func isServiceLoadBalancerIPMatchesPIP(service *v1.Service, pip *armnetwork.PublicIPAddress, isIPV6 bool) bool {
	return strings.EqualFold(ptr.Deref(pip.Properties.IPAddress, ""), getServiceLoadBalancerIP(service, isIPV6))
}

func isServicePIPNameMatchesPIP(service *v1.Service, pip *armnetwork.PublicIPAddress, isIPV6 bool) bool {
	return strings.EqualFold(ptr.Deref(pip.Name, ""), getServicePIPName(service, isIPV6))
}

func isServiceSelectPIP(service *v1.Service, pip *armnetwork.PublicIPAddress, isIPV6 bool) bool {
	return isServiceLoadBalancerIPMatchesPIP(service, pip, isIPV6) || isServicePIPNameMatchesPIP(service, pip, isIPV6)
}

func isSVCNameInPIPTag(tag, svcName string) bool {
	svcNames := parsePIPServiceTag(&tag)

	for _, name := range svcNames {
		if strings.EqualFold(name, svcName) {
			return true
		}
	}

	return false
}

func parsePIPServiceTag(serviceTag *string) []string {
	if serviceTag == nil || len(*serviceTag) == 0 {
		return []string{}
	}

	serviceNames := strings.FieldsFunc(*serviceTag, func(r rune) bool {
		return r == ','
	})
	for i, name := range serviceNames {
		serviceNames[i] = strings.TrimSpace(name)
	}

	return serviceNames
}

// bindServicesToPIP add the incoming service name to the PIP's tag
// parameters: public IP address to be updated and incoming service names
// return values:
// 1. a bool flag to indicate if there is a new service added
// 2. an error when the pip is nil
// example:
// "ns1/svc1" + ["ns1/svc1", "ns2/svc2"] = "ns1/svc1,ns2/svc2"
func bindServicesToPIP(pip *armnetwork.PublicIPAddress, incomingServiceNames []string, replace bool) (bool, error) {
	if pip == nil {
		return false, fmt.Errorf("nil public IP")
	}

	if pip.Tags == nil {
		pip.Tags = map[string]*string{consts.ServiceTagKey: ptr.To("")}
	}

	serviceTagValue := ptr.To(getServiceFromPIPServiceTags(pip.Tags))
	serviceTagValueSet := make(map[string]struct{})
	existingServiceNames := parsePIPServiceTag(serviceTagValue)
	addedNew := false

	// replace is used when unbinding the service from PIP so addedNew remains false all the time
	if replace {
		serviceTagValue = ptr.To(strings.Join(incomingServiceNames, ","))
		pip.Tags[consts.ServiceTagKey] = serviceTagValue

		return false, nil
	}

	for _, name := range existingServiceNames {
		if _, ok := serviceTagValueSet[name]; !ok {
			serviceTagValueSet[name] = struct{}{}
		}
	}

	for _, serviceName := range incomingServiceNames {
		if serviceTagValue == nil || *serviceTagValue == "" {
			serviceTagValue = ptr.To(serviceName)
			addedNew = true
		} else {
			// detect duplicates
			if _, ok := serviceTagValueSet[serviceName]; !ok {
				*serviceTagValue += fmt.Sprintf(",%s", serviceName)
				addedNew = true
			} else {
				klog.V(10).Infof("service %s has been bound to the pip already", serviceName)
			}
		}
	}
	pip.Tags[consts.ServiceTagKey] = serviceTagValue

	return addedNew, nil
}

// unbindServiceFromPIP removes the service name from the PIP's tag.
// And returns the updated service names.
func unbindServiceFromPIP(
	pip *armnetwork.PublicIPAddress,
	serviceName string,
	isUserAssignedPIP bool,
) ([]string, error) {
	if pip == nil || pip.Tags == nil {
		return nil, fmt.Errorf("nil public IP or tags")
	}

	if existingServiceName := getServiceFromPIPDNSTags(pip.Tags); existingServiceName != "" && strings.EqualFold(existingServiceName, serviceName) {
		deleteServicePIPDNSTags(&pip.Tags)
	}
	if isUserAssignedPIP {
		return nil, nil
	}

	// skip removing tags for user assigned pips
	serviceTagValue := ptr.To(getServiceFromPIPServiceTags(pip.Tags))
	existingServiceNames := parsePIPServiceTag(serviceTagValue)
	var found bool
	for i := len(existingServiceNames) - 1; i >= 0; i-- {
		if strings.EqualFold(existingServiceNames[i], serviceName) {
			existingServiceNames = append(existingServiceNames[:i], existingServiceNames[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		klog.Warningf("cannot find the service %s in the corresponding PIP", serviceName)
	}

	_, err := bindServicesToPIP(pip, existingServiceNames, true)
	return existingServiceNames, err
}

// ensureLoadBalancerTagged ensures every load balancer in the resource group is tagged as configured
func (az *Cloud) ensureLoadBalancerTagged(lb *armnetwork.LoadBalancer) bool {
	if az.Tags == "" && len(az.TagsMap) == 0 {
		return false
	}
	tags := parseTags(az.Tags, az.TagsMap)
	if lb.Tags == nil {
		lb.Tags = make(map[string]*string)
	}

	tags, changed := az.reconcileTags(lb.Tags, tags)
	lb.Tags = tags

	return changed
}

// ensureSecurityGroupTagged ensures the security group is tagged as configured
func (az *Cloud) ensureSecurityGroupTagged(sg *armnetwork.SecurityGroup) bool {
	if az.Tags == "" && (len(az.TagsMap) == 0) {
		return false
	}
	tags := parseTags(az.Tags, az.TagsMap)
	if sg.Tags == nil {
		sg.Tags = make(map[string]*string)
	}

	tags, changed := az.reconcileTags(sg.Tags, tags)
	sg.Tags = tags

	return changed
}

// For a load balancer, all frontend ip should reference either a subnet or publicIpAddress.
// Thus Azure do not allow mixed type (public and internal) load balancer.
// So we'd have a separate name for internal load balancer.
// This would be the name for Azure LoadBalancer resource.
func (az *Cloud) getAzureLoadBalancerName(
	ctx context.Context,
	service *v1.Service,
	existingLBs []*armnetwork.LoadBalancer,
	clusterName, vmSetName string,
	isInternal bool,
) (string, error) {
	if az.LoadBalancerName != "" {
		clusterName = az.LoadBalancerName
	}
	lbNamePrefix := vmSetName
	// The LB name prefix is set to the name of the cluster when:
	// 1. the LB belongs to the primary agent pool.
	// 2. using the single SLB.
	if strings.EqualFold(vmSetName, az.VMSet.GetPrimaryVMSetName()) || az.UseSingleStandardLoadBalancer() {
		lbNamePrefix = clusterName
	}

	// For multiple standard load balancers scenario:
	// 1. Filter out the eligible load balancers.
	// 2. Choose the most eligible load balancer.
	if az.UseMultipleStandardLoadBalancers() {
		eligibleLBs, err := az.getEligibleLoadBalancersForService(ctx, service)
		if err != nil {
			return "", err
		}

		currentLBName := az.getServiceCurrentLoadBalancerName(service)
		lbNamePrefix = getMostEligibleLBForService(currentLBName, eligibleLBs, existingLBs, requiresInternalLoadBalancer(service))
	}

	if isInternal {
		return fmt.Sprintf("%s%s", lbNamePrefix, consts.InternalLoadBalancerNameSuffix), nil
	}
	return lbNamePrefix, nil
}

func getMostEligibleLBForService(
	currentLBName string,
	eligibleLBs []string,
	existingLBs []*armnetwork.LoadBalancer,
	isInternal bool,
) string {
	// 1. If the LB is eligible and being used, choose it.
	if StringInSlice(currentLBName, eligibleLBs) {
		klog.V(4).Infof("getMostEligibleLBForService: choose %s as it is eligible and being used", currentLBName)
		return currentLBName
	}

	// 2. If the LB is eligible and not created yet, choose it because it has the fewest rules.
	for _, eligibleLB := range eligibleLBs {
		var found bool
		for i := range existingLBs {
			existingLB := (existingLBs)[i]
			if strings.EqualFold(trimSuffixIgnoreCase(ptr.Deref(existingLB.Name, ""), consts.InternalLoadBalancerNameSuffix), eligibleLB) &&
				isInternalLoadBalancer(existingLB) == isInternal {
				found = true
				break
			}
		}

		if !found {
			klog.V(4).Infof("getMostEligibleLBForService: choose %s as it is eligible and not existing", eligibleLB)
			return eligibleLB
		}
	}

	// 3. If all eligible LBs are existing, choose the one with the fewest rules.
	var expectedLBName string
	ruleCount := 301
	for i := range existingLBs {
		existingLB := existingLBs[i]
		if StringInSlice(trimSuffixIgnoreCase(ptr.Deref(existingLB.Name, ""), consts.InternalLoadBalancerNameSuffix), eligibleLBs) &&
			isInternalLoadBalancer(existingLB) == isInternal {
			if existingLB.Properties != nil &&
				existingLB.Properties.LoadBalancingRules != nil {
				if len(existingLB.Properties.LoadBalancingRules) < ruleCount {
					ruleCount = len(existingLB.Properties.LoadBalancingRules)
					expectedLBName = ptr.Deref(existingLB.Name, "")
				}
			}
		}
	}

	if expectedLBName != "" {
		klog.V(4).Infof("getMostEligibleLBForService: choose %s with fewest %d rules", expectedLBName, ruleCount)
	}

	return trimSuffixIgnoreCase(expectedLBName, consts.InternalLoadBalancerNameSuffix)
}

func (az *Cloud) getServiceCurrentLoadBalancerName(service *v1.Service) string {
	for _, multiSLBConfig := range az.MultipleStandardLoadBalancerConfigurations {
		if az.isLoadBalancerInUseByService(service, multiSLBConfig) {
			return multiSLBConfig.Name
		}
	}
	return ""
}

// getEligibleLoadBalancersForService filter out the eligible load balancers for the service.
// It follows four kinds of constraints:
// 1. Service annotation `service.beta.kubernetes.io/azure-load-balancer-configurations: lb1,lb2`.
// 2. AllowServicePlacement flag. Default to true, if set to false, the new services will not be put onto the LB.
// But the existing services that is using the LB will not be affected.
// 3. ServiceLabelSelector. The service will be put onto the LB only if the service has the labels specified in the selector.
// 4. ServiceNamespaceSelector. The service will be put onto the LB only if the service is in the namespaces specified in the selector.
// 5. If there is no label/namespace selector on the LB, it can be a valid placement target if and only if the service has no other choice.
func (az *Cloud) getEligibleLoadBalancersForService(ctx context.Context, service *v1.Service) ([]string, error) {
	var (
		eligibleLBs               []config.MultipleStandardLoadBalancerConfiguration
		eligibleLBNames           []string
		lbSelectedByAnnotation    []string
		lbFailedLabelSelector     []string
		lbFailedNamespaceSelector []string
		lbFailedPlacementFlag     []string
	)

	logger := klog.Background().
		WithName("getEligibleLoadBalancersForService").
		WithValues("service", service.Name)

	// 1. Service selects LBs defined in the annotation.
	// If there is no annotation given, it selects all LBs.
	lbsFromAnnotation := consts.GetLoadBalancerConfigurationsNames(service)
	if len(lbsFromAnnotation) > 0 {
		lbNamesSet := utilsets.NewString(lbsFromAnnotation...)
		for i := range az.MultipleStandardLoadBalancerConfigurations {
			multiSLBConfig := az.MultipleStandardLoadBalancerConfigurations[i]
			if lbNamesSet.Has(multiSLBConfig.Name) {
				logger.V(4).Info("selects the load balancer by annotation",
					"load balancer configuration name", multiSLBConfig.Name)
				eligibleLBs = append(eligibleLBs, multiSLBConfig)
				lbSelectedByAnnotation = append(lbSelectedByAnnotation, multiSLBConfig.Name)
			}
		}
		if len(lbSelectedByAnnotation) == 0 {
			return nil, fmt.Errorf("service %q selects %d load balancers by annotation, but none of them is defined in cloud provider configuration", service.Name, len(lbsFromAnnotation))
		}
	} else {
		logger.V(4).Info("the service does not select any load balancer by annotation, all load balancers are eligible")
		eligibleLBs = append(eligibleLBs, az.MultipleStandardLoadBalancerConfigurations...)
		for _, eligibleLB := range eligibleLBs {
			lbSelectedByAnnotation = append(lbSelectedByAnnotation, eligibleLB.Name)
		}
	}

	var selectorMatched bool
	for i := len(eligibleLBs) - 1; i >= 0; i-- {
		eligibleLB := eligibleLBs[i]

		// 2. If the LB does not allow service placement, it is not eligible,
		// unless the service is already using the LB.
		if !ptr.Deref(eligibleLB.AllowServicePlacement, true) {
			if az.isLoadBalancerInUseByService(service, eligibleLB) {
				logger.V(4).Info("although the load balancer has AllowServicePlacement=false, service is allowed to be placed on load balancer because it is using the load balancer",
					"load balancer configuration name", eligibleLB.Name)
			} else {
				logger.V(4).Info("the load balancer has AllowServicePlacement=false, service is not allowed to be placed on load balancer",
					"load balancer configuration name", eligibleLB.Name)
				eligibleLBs = append(eligibleLBs[:i], eligibleLBs[i+1:]...)
				lbFailedPlacementFlag = append(lbFailedPlacementFlag, eligibleLB.Name)
				continue
			}
		}

		// 3. Check the service label selector. The service can be migrated from one LB to another LB
		// if the service does not match the selector of the LB that it is currently using.
		if eligibleLB.ServiceLabelSelector != nil {
			serviceLabelSelector, err := metav1.LabelSelectorAsSelector(eligibleLB.ServiceLabelSelector)
			if err != nil {
				logger.Error(err, "failed to parse label selector",
					"label selector", eligibleLB.ServiceLabelSelector.String(),
					"load balancer configuration name", eligibleLB.Name)
				return []string{}, err
			}
			if !serviceLabelSelector.Matches(labels.Set(service.Labels)) {
				logger.V(2).Info("service does not match the label selector",
					"label selector", eligibleLB.ServiceLabelSelector.String(),
					"load balancer configuration name", eligibleLB.Name)
				eligibleLBs = append(eligibleLBs[:i], eligibleLBs[i+1:]...)
				lbFailedLabelSelector = append(lbFailedLabelSelector, eligibleLB.Name)
				continue
			}
			logger.V(4).Info("service matches the label selector",
				"label selector", eligibleLB.ServiceLabelSelector.String(),
				"load balancer configuration name", eligibleLB.Name)
			selectorMatched = true
		}

		// 4. Check the service namespace selector. The service can be migrated from one LB to another LB
		// if the service does not match the selector of the LB that it is currently using.
		if eligibleLB.ServiceNamespaceSelector != nil {
			serviceNamespaceSelector, err := metav1.LabelSelectorAsSelector(eligibleLB.ServiceNamespaceSelector)
			if err != nil {
				logger.Error(err, "failed to parse namespace selector",
					"namespace selector", eligibleLB.ServiceNamespaceSelector.String(),
					"load balancer configuration name", eligibleLB.Name)
				return []string{}, err
			}
			ns, err := az.KubeClient.CoreV1().Namespaces().Get(ctx, service.Namespace, metav1.GetOptions{})
			if err != nil {
				logger.Error(err, "failed to get namespace",
					"namespace", service.Namespace,
					"load balancer configuration name", eligibleLB.Name)
				return []string{}, err
			}
			if !serviceNamespaceSelector.Matches(labels.Set(ns.Labels)) {
				logger.V(2).Info("namespace does not match the namespace selector",
					"namespace", service.Namespace,
					"namespace selector", eligibleLB.ServiceNamespaceSelector.String(),
					"load balancer configuration name", eligibleLB.Name)
				eligibleLBs = append(eligibleLBs[:i], eligibleLBs[i+1:]...)
				lbFailedNamespaceSelector = append(lbFailedNamespaceSelector, eligibleLB.Name)
				continue
			}
			logger.V(4).Info("namespace matches the namespace selector",
				"namespace", service.Namespace,
				"namespace selector", eligibleLB.ServiceNamespaceSelector.String(),
				"load balancer configuration name", eligibleLB.Name)
			selectorMatched = true
		}
	}

	serviceName := getServiceName(service)
	if len(eligibleLBs) == 0 {
		return []string{}, fmt.Errorf(
			"service %q selects %d load balancers (%s), but %d of them (%s) have AllowServicePlacement set to false and the service is not using any of them, %d of them (%s) do not match the service label selector, and %d of them (%s) do not match the service namespace selector",
			serviceName,
			len(lbSelectedByAnnotation),
			strings.Join(lbSelectedByAnnotation, ", "),
			len(lbFailedPlacementFlag),
			strings.Join(lbFailedPlacementFlag, ", "),
			len(lbFailedLabelSelector),
			strings.Join(lbFailedLabelSelector, ", "),
			len(lbFailedNamespaceSelector),
			strings.Join(lbFailedNamespaceSelector, ", "),
		)
	}

	if selectorMatched {
		for i := len(eligibleLBs) - 1; i >= 0; i-- {
			eligibleLB := eligibleLBs[i]
			if eligibleLB.ServiceLabelSelector == nil && eligibleLB.ServiceNamespaceSelector == nil {
				logger.V(6).Info("service matches at least one label/namespace selector of the load balancer, so it should not be placed on the load balancer that does not have any label/namespace selector",
					"load balancer configuration name", eligibleLB.Name)
				eligibleLBs = append(eligibleLBs[:i], eligibleLBs[i+1:]...)
			}
		}
	} else {
		logger.V(4).Info("no load balancer that has label/namespace selector matches the service, so the service can be placed on the load balancers that do not have label/namespace selector")
	}

	for i := range eligibleLBs {
		eligibleLB := eligibleLBs[i]
		eligibleLBNames = append(eligibleLBNames, eligibleLB.Name)
	}

	return eligibleLBNames, nil
}

func (az *Cloud) isLoadBalancerInUseByService(service *v1.Service, lbConfig config.MultipleStandardLoadBalancerConfiguration) bool {
	az.multipleStandardLoadBalancersActiveServicesLock.Lock()
	defer az.multipleStandardLoadBalancersActiveServicesLock.Unlock()

	serviceName := getServiceName(service)
	return lbConfig.ActiveServices.Has(serviceName)
}

// There are two cases when a service owns the frontend IP config:
// 1. The primary service, which means the frontend IP config is created after the creation of the service.
// This means the name of the config can be tracked by the service UID.
// 2. The secondary services must have their loadBalancer IP set if they want to share the same config as the primary
// service. Hence, it can be tracked by the loadBalancer IP.
// If the IP version is not empty, which means it is the secondary Service, it returns IP version of the Service FIP.
func (az *Cloud) serviceOwnsFrontendIP(ctx context.Context, fip *armnetwork.FrontendIPConfiguration, service *v1.Service) (bool, bool, *armnetwork.IPVersion) {
	var isPrimaryService bool
	baseName := az.GetLoadBalancerName(ctx, "", service)
	if fip != nil && strings.HasPrefix(ptr.Deref(fip.Name, ""), baseName) {
		klog.V(6).Infof("serviceOwnsFrontendIP: found primary service %s of the frontend IP config %s", service.Name, *fip.Name)
		isPrimaryService = true
		return true, isPrimaryService, nil
	}

	loadBalancerIPs := getServiceLoadBalancerIPs(service)
	pipResourceGroup := az.getPublicIPAddressResourceGroup(service)
	var pipNames []string
	if len(loadBalancerIPs) == 0 {
		if !requiresInternalLoadBalancer(service) {
			pipNames = getServicePIPNames(service)
			for _, pipName := range pipNames {
				if pipName != "" {
					pip, err := az.findMatchedPIP(ctx, "", pipName, pipResourceGroup)
					if err != nil {
						klog.Warningf("serviceOwnsFrontendIP: unexpected error when finding match public IP of the service %s with name %s: %v", service.Name, pipName, err)
						return false, isPrimaryService, nil
					}
					if publicIPOwnsFrontendIP(service, fip, pip) {
						return true, isPrimaryService, pip.Properties.PublicIPAddressVersion
					}
				}
			}
		}
		// it is a must that the secondary services set the loadBalancer IP or pip name
		return false, isPrimaryService, nil
	}

	// for external secondary service the public IP address should be checked
	if !requiresInternalLoadBalancer(service) {
		for _, loadBalancerIP := range loadBalancerIPs {
			pip, err := az.findMatchedPIP(ctx, loadBalancerIP, "", pipResourceGroup)
			if err != nil {
				klog.Warningf("serviceOwnsFrontendIP: unexpected error when finding match public IP of the service %s with loadBalancerIP %s: %v", service.Name, loadBalancerIP, err)
				return false, isPrimaryService, nil
			}

			if publicIPOwnsFrontendIP(service, fip, pip) {
				return true, isPrimaryService, pip.Properties.PublicIPAddressVersion
			}
			klog.V(6).Infof("serviceOwnsFrontendIP: the public IP with ID %s is being referenced by other service with public IP address %s "+
				"OR it is of incorrect IP version", *pip.ID, *pip.Properties.IPAddress)
		}

		return false, isPrimaryService, nil
	}

	// for internal secondary service the private IP address on the frontend IP config should be checked
	if fip.Properties.PrivateIPAddress == nil {
		return false, isPrimaryService, nil
	}
	privateIPAddrVersion := to.Ptr(armnetwork.IPVersionIPv4)
	if net.ParseIP(*fip.Properties.PrivateIPAddress).To4() == nil {
		privateIPAddrVersion = to.Ptr(armnetwork.IPVersionIPv6)
	}

	privateIPEquals := false
	for _, loadBalancerIP := range loadBalancerIPs {
		if strings.EqualFold(*fip.Properties.PrivateIPAddress, loadBalancerIP) {
			privateIPEquals = true
			break
		}
	}
	return privateIPEquals, isPrimaryService, privateIPAddrVersion
}

func (az *Cloud) getFrontendIPConfigNames(service *v1.Service) map[bool]string {
	isDualStack := isServiceDualStack(service)
	defaultLBFrontendIPConfigName := az.getDefaultFrontendIPConfigName(service)
	return map[bool]string{
		consts.IPVersionIPv4: getResourceByIPFamily(defaultLBFrontendIPConfigName, isDualStack, consts.IPVersionIPv4),
		consts.IPVersionIPv6: getResourceByIPFamily(defaultLBFrontendIPConfigName, isDualStack, consts.IPVersionIPv6),
	}
}

func (az *Cloud) getDefaultFrontendIPConfigName(service *v1.Service) string {
	baseName := az.GetLoadBalancerName(context.TODO(), "", service)
	subnetName := getInternalSubnet(service)
	if subnetName != nil {
		ipcName := fmt.Sprintf("%s-%s", baseName, *subnetName)

		// Azure lb front end configuration name must not exceed 80 characters
		maxLength := consts.FrontendIPConfigNameMaxLength - consts.IPFamilySuffixLength
		if len(ipcName) > maxLength {
			ipcName = ipcName[:maxLength]
			// Cutting the string may result in char like "-" as the string end.
			// If the last char is not a letter or '_', replace it with "_".
			if !unicode.IsLetter(rune(ipcName[len(ipcName)-1:][0])) && ipcName[len(ipcName)-1:] != "_" {
				ipcName = ipcName[:len(ipcName)-1] + "_"
			}
		}
		return ipcName
	}
	return baseName
}
