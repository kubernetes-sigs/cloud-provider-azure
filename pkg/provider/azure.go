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
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	azureconfig "sigs.k8s.io/cloud-provider-azure/pkg/provider/config"

	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	azclients "sigs.k8s.io/cloud-provider-azure/pkg/azureclients"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/blobclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/containerserviceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/deploymentclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/diskclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/fileclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/interfaceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/loadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privatednsclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privatednszonegroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privateendpointclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/privatelinkserviceclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/publicipclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/routeclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/routetableclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/securitygroupclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/snapshotclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/storageaccountclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/subnetclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/virtualnetworklinksclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmasclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmsizeclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/vmssvmclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/zoneclient"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	nodemanager "sigs.k8s.io/cloud-provider-azure/pkg/nodemanager"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"

	"sigs.k8s.io/yaml"
)

var (
	_ cloudprovider.Interface    = (*Cloud)(nil)
	_ cloudprovider.Instances    = (*Cloud)(nil)
	_ cloudprovider.LoadBalancer = (*Cloud)(nil)
	_ cloudprovider.Routes       = (*Cloud)(nil)
	_ cloudprovider.Zones        = (*Cloud)(nil)
	_ cloudprovider.PVLabeler    = (*Cloud)(nil)
)

// Cloud holds the config and clients
type Cloud struct {
	azureconfig.Config
	azureconfig.InitSecretConfig
	Environment azure.Environment

	RoutesClient                    routeclient.Interface
	SubnetsClient                   subnetclient.Interface
	InterfacesClient                interfaceclient.Interface
	RouteTablesClient               routetableclient.Interface
	LoadBalancerClient              loadbalancerclient.Interface
	PublicIPAddressesClient         publicipclient.Interface
	SecurityGroupsClient            securitygroupclient.Interface
	VirtualMachinesClient           vmclient.Interface
	StorageAccountClient            storageaccountclient.Interface
	DisksClient                     diskclient.Interface
	SnapshotsClient                 snapshotclient.Interface
	FileClient                      fileclient.Interface
	BlobClient                      blobclient.Interface
	VirtualMachineScaleSetsClient   vmssclient.Interface
	VirtualMachineScaleSetVMsClient vmssvmclient.Interface
	VirtualMachineSizesClient       vmsizeclient.Interface
	AvailabilitySetsClient          vmasclient.Interface
	ZoneClient                      zoneclient.Interface
	privateendpointclient           privateendpointclient.Interface
	privatednsclient                privatednsclient.Interface
	privatednszonegroupclient       privatednszonegroupclient.Interface
	virtualNetworkLinksClient       virtualnetworklinksclient.Interface
	PrivateLinkServiceClient        privatelinkserviceclient.Interface
	containerServiceClient          containerserviceclient.Interface
	deploymentClient                deploymentclient.Interface

	ResourceRequestBackoff  wait.Backoff
	Metadata                *InstanceMetadataService
	VMSet                   VMSet
	LoadBalancerBackendPool BackendPool

	// ipv6DualStack allows overriding for unit testing.  It's normally initialized from featuregates
	ipv6DualStackEnabled bool
	// Lock for access to node caches, includes nodeZones, nodeResourceGroups, and unmanagedNodes.
	nodeCachesLock sync.RWMutex
	// nodeNames holds current nodes for tracking added nodes in VM caches.
	nodeNames sets.Set[string]
	// nodeZones is a mapping from Zone to a sets.Set[string] of Node's names in the Zone
	// it is updated by the nodeInformer
	nodeZones map[string]sets.Set[string]
	// nodeResourceGroups holds nodes external resource groups
	nodeResourceGroups map[string]string
	// unmanagedNodes holds a list of nodes not managed by Azure cloud provider.
	unmanagedNodes sets.Set[string]
	// excludeLoadBalancerNodes holds a list of nodes that should be excluded from LoadBalancer.
	excludeLoadBalancerNodes sets.Set[string]
	nodePrivateIPs           map[string]sets.Set[string]
	// nodeInformerSynced is for determining if the informer has synced.
	nodeInformerSynced cache.InformerSynced

	// routeCIDRsLock holds lock for routeCIDRs cache.
	routeCIDRsLock sync.Mutex
	// routeCIDRs holds cache for route CIDRs.
	routeCIDRs map[string]string

	// regionZonesMap stores all available zones for the subscription by region
	regionZonesMap   map[string][]string
	refreshZonesLock sync.RWMutex

	KubeClient       clientset.Interface
	eventBroadcaster record.EventBroadcaster
	eventRecorder    record.EventRecorder
	routeUpdater     *delayedRouteUpdater

	vmCache  azcache.Resource
	lbCache  azcache.Resource
	nsgCache azcache.Resource
	rtCache  azcache.Resource
	// public ip cache
	// key: [resourceGroupName]
	// Value: sync.Map of [pipName]*PublicIPAddress
	pipCache azcache.Resource
	// use LB frontEndIpConfiguration ID as the key and search for PLS attached to the frontEnd
	plsCache azcache.Resource

	// Add service lister to always get latest service
	serviceLister corelisters.ServiceLister
	// node-sync-loop routine and service-reconcile routine should not update LoadBalancer at the same time
	serviceReconcileLock sync.Mutex

	*ManagedDiskController
	*controllerCommon

	multipleStandardLoadBalancerConfigurationsSynced bool
}

// NewCloud returns a Cloud with initialized clients
func NewCloud(ctx context.Context, configReader io.Reader, callFromCCM bool) (cloudprovider.Interface, error) {
	az, err := NewCloudWithoutFeatureGates(ctx, configReader, callFromCCM)
	if err != nil {
		return nil, err
	}
	az.ipv6DualStackEnabled = true

	return az, nil
}

func NewCloudFromConfigFile(ctx context.Context, configFilePath string, calFromCCM bool) (cloudprovider.Interface, error) {
	var (
		cloud cloudprovider.Interface
		err   error
	)

	if configFilePath != "" {
		var config *os.File
		config, err = os.Open(configFilePath)
		if err != nil {
			klog.Fatalf("Couldn't open cloud provider configuration %s: %#v",
				configFilePath, err)
		}

		defer config.Close()
		cloud, err = NewCloud(ctx, config, calFromCCM)
	} else {
		// Pass explicit nil so plugins can actually check for nil. See
		// "Why is my nil error value not equal to nil?" in golang.org/doc/faq.
		cloud, err = NewCloud(ctx, nil, false)
	}

	if err != nil {
		return nil, fmt.Errorf("could not init cloud provider azure: %w", err)
	}
	if cloud == nil {
		return nil, fmt.Errorf("nil cloud")
	}

	return cloud, nil
}

func (az *Cloud) configSecretMetadata(secretName, secretNamespace, cloudConfigKey string) {
	if secretName == "" {
		secretName = consts.DefaultCloudProviderConfigSecName
	}
	if secretNamespace == "" {
		secretNamespace = consts.DefaultCloudProviderConfigSecNamespace
	}
	if cloudConfigKey == "" {
		cloudConfigKey = consts.DefaultCloudProviderConfigSecKey
	}

	az.InitSecretConfig = azureconfig.InitSecretConfig{
		SecretName:      secretName,
		SecretNamespace: secretNamespace,
		CloudConfigKey:  cloudConfigKey,
	}
}

func NewCloudFromSecret(ctx context.Context, clientBuilder cloudprovider.ControllerClientBuilder, secretName, secretNamespace, cloudConfigKey string) (cloudprovider.Interface, error) {
	az := &Cloud{
		nodeNames:                sets.New[string](),
		nodeZones:                map[string]sets.Set[string]{},
		nodeResourceGroups:       map[string]string{},
		unmanagedNodes:           sets.New[string](),
		routeCIDRs:               map[string]string{},
		excludeLoadBalancerNodes: sets.New[string](),
		nodePrivateIPs:           map[string]sets.Set[string]{},
	}

	az.configSecretMetadata(secretName, secretNamespace, cloudConfigKey)

	az.Initialize(clientBuilder, wait.NeverStop)

	err := az.InitializeCloudFromSecret(ctx)
	if err != nil {
		return nil, fmt.Errorf("NewCloudFromSecret: failed to initialize cloud from secret %s/%s: %w", az.SecretNamespace, az.SecretName, err)
	}

	az.ipv6DualStackEnabled = true

	return az, nil
}

// NewCloudWithoutFeatureGates returns a Cloud without trying to wire the feature gates.  This is used by the unit tests
// that don't load the actual features being used in the cluster.
func NewCloudWithoutFeatureGates(ctx context.Context, configReader io.Reader, callFromCCM bool) (*Cloud, error) {
	config, err := ParseConfig(configReader)
	if err != nil {
		return nil, err
	}

	az := &Cloud{
		nodeNames:                sets.New[string](),
		nodeZones:                map[string]sets.Set[string]{},
		nodeResourceGroups:       map[string]string{},
		unmanagedNodes:           sets.New[string](),
		routeCIDRs:               map[string]string{},
		excludeLoadBalancerNodes: sets.New[string](),
		nodePrivateIPs:           map[string]sets.Set[string]{},
	}

	err = az.InitializeCloudFromConfig(ctx, config, false, callFromCCM)
	if err != nil {
		return nil, err
	}

	return az, nil
}

// InitializeCloudFromConfig initializes the Cloud from config.
func (az *Cloud) InitializeCloudFromConfig(ctx context.Context, config *azureconfig.Config, fromSecret, callFromCCM bool) error {
	if config == nil {
		// should not reach here
		return fmt.Errorf("InitializeCloudFromConfig: cannot initialize from nil config")
	}

	if config.RouteTableResourceGroup == "" {
		config.RouteTableResourceGroup = config.ResourceGroup
	}

	if config.SecurityGroupResourceGroup == "" {
		config.SecurityGroupResourceGroup = config.ResourceGroup
	}

	if config.PrivateLinkServiceResourceGroup == "" {
		config.PrivateLinkServiceResourceGroup = config.ResourceGroup
	}

	if config.VMType == "" {
		// default to vmss vmType if not set.
		config.VMType = consts.VMTypeVMSS
	}

	if config.RouteUpdateWaitingInSeconds <= 0 {
		config.RouteUpdateWaitingInSeconds = azureconfig.DefaultRouteUpdateWaitingInSeconds
	}

	if config.DisableAvailabilitySetNodes && config.VMType != consts.VMTypeVMSS {
		return fmt.Errorf("disableAvailabilitySetNodes %v is only supported when vmType is 'vmss'", config.DisableAvailabilitySetNodes)
	}

	if config.CloudConfigType == "" {
		// The default cloud config type is cloudConfigTypeMerge.
		config.CloudConfigType = azureconfig.CloudConfigTypeMerge
	} else {
		supportedCloudConfigTypes := sets.New(
			string(azureconfig.CloudConfigTypeMerge),
			string(azureconfig.CloudConfigTypeFile),
			string(azureconfig.CloudConfigTypeSecret))
		if !supportedCloudConfigTypes.Has(string(config.CloudConfigType)) {
			return fmt.Errorf("cloudConfigType %v is not supported, supported values are %v", config.CloudConfigType, supportedCloudConfigTypes.UnsortedList())
		}
	}

	if config.LoadBalancerBackendPoolConfigurationType == "" ||
		// TODO(nilo19): support pod IP mode in the future
		strings.EqualFold(config.LoadBalancerBackendPoolConfigurationType, consts.LoadBalancerBackendPoolConfigurationTypePODIP) {
		config.LoadBalancerBackendPoolConfigurationType = consts.LoadBalancerBackendPoolConfigurationTypeNodeIPConfiguration
	} else {
		supportedLoadBalancerBackendPoolConfigurationTypes := sets.New(
			strings.ToLower(consts.LoadBalancerBackendPoolConfigurationTypeNodeIPConfiguration),
			strings.ToLower(consts.LoadBalancerBackendPoolConfigurationTypeNodeIP),
			strings.ToLower(consts.LoadBalancerBackendPoolConfigurationTypePODIP))
		if !supportedLoadBalancerBackendPoolConfigurationTypes.Has(strings.ToLower(config.LoadBalancerBackendPoolConfigurationType)) {
			return fmt.Errorf("loadBalancerBackendPoolConfigurationType %s is not supported, supported values are %v", config.LoadBalancerBackendPoolConfigurationType, supportedLoadBalancerBackendPoolConfigurationTypes.UnsortedList())
		}
	}

	if az.useMultipleStandardLoadBalancers() {
		if err := az.checkEnableMultipleStandardLoadBalancers(); err != nil {
			return err
		}
	}

	env, err := azureconfig.ParseAzureEnvironment(config.Cloud, config.ResourceManagerEndpoint, config.IdentitySystem)
	if err != nil {
		return err
	}

	servicePrincipalToken, err := azureconfig.GetServicePrincipalToken(&config.AzureAuthConfig, env, env.ServiceManagementEndpoint)
	if errors.Is(err, azureconfig.ErrorNoAuth) {
		// Only controller-manager would lazy-initialize from secret, and credentials are required for such case.
		if fromSecret {
			err := fmt.Errorf("no credentials provided for Azure cloud provider")
			klog.Fatal(err)
			return err
		}

		// No credentials provided, useInstanceMetadata should be enabled for Kubelet.
		// TODO(feiskyer): print different error message for Kubelet and controller-manager, as they're
		// requiring different credential settings.
		if !config.UseInstanceMetadata && config.CloudConfigType == azureconfig.CloudConfigTypeFile {
			return fmt.Errorf("useInstanceMetadata must be enabled without Azure credentials")
		}

		klog.V(2).Infof("Azure cloud provider is starting without credentials")
	} else if err != nil {
		return err
	}

	// Initialize rate limiting config options.
	azureconfig.InitializeCloudProviderRateLimitConfig(&config.CloudProviderRateLimitConfig)

	resourceRequestBackoff := az.setCloudProviderBackoffDefaults(config)

	err = az.setLBDefaults(config)
	if err != nil {
		return err
	}

	az.Config = *config
	az.Environment = *env
	az.ResourceRequestBackoff = resourceRequestBackoff
	az.Metadata, err = NewInstanceMetadataService(consts.ImdsServer)
	if err != nil {
		return err
	}

	// No credentials provided, InstanceMetadataService would be used for getting Azure resources.
	// Note that this only applies to Kubelet, controller-manager should configure credentials for managing Azure resources.
	if servicePrincipalToken == nil {
		return nil
	}

	// If uses network resources in different AAD Tenant, then prepare corresponding Service Principal Token for VM/VMSS client and network resources client
	err = az.configureMultiTenantClients(servicePrincipalToken)
	if err != nil {
		return err
	}

	if az.MaximumLoadBalancerRuleCount == 0 {
		az.MaximumLoadBalancerRuleCount = consts.MaximumLoadBalancerRuleCount
	}

	if strings.EqualFold(consts.VMTypeVMSS, az.Config.VMType) {
		az.VMSet, err = newScaleSet(ctx, az)
		if err != nil {
			return err
		}
	} else if strings.EqualFold(consts.VMTypeVmssFlex, az.Config.VMType) {
		az.VMSet, err = newFlexScaleSet(ctx, az)
		if err != nil {
			return err
		}
	} else {
		az.VMSet, err = newAvailabilitySet(az)
		if err != nil {
			return err
		}
	}

	if az.isLBBackendPoolTypeNodeIPConfig() {
		az.LoadBalancerBackendPool = newBackendPoolTypeNodeIPConfig(az)
	} else if az.isLBBackendPoolTypeNodeIP() {
		az.LoadBalancerBackendPool = newBackendPoolTypeNodeIP(az)
	}

	err = az.initCaches()
	if err != nil {
		return err
	}

	if err := initDiskControllers(az); err != nil {
		return err
	}

	// updating routes and syncing zones only in CCM
	if callFromCCM {
		// start delayed route updater.
		az.routeUpdater = newDelayedRouteUpdater(az, routeUpdateInterval)
		go az.routeUpdater.run()

		// Azure Stack does not support zone at the moment
		// https://docs.microsoft.com/en-us/azure-stack/user/azure-stack-network-differences?view=azs-2102
		if !az.isStackCloud() {
			// wait for the success first time of syncing zones
			err = az.syncRegionZonesMap()
			if err != nil {
				klog.Errorf("InitializeCloudFromConfig: failed to sync regional zones map for the first time: %s", err.Error())
				return err
			}

			go az.refreshZones(az.syncRegionZonesMap)
		}
	}

	return nil
}

func (az *Cloud) useMultipleStandardLoadBalancers() bool {
	return az.useStandardLoadBalancer() && len(az.MultipleStandardLoadBalancerConfigurations) > 0
}

func (az *Cloud) useSingleStandardLoadBalancer() bool {
	return az.useStandardLoadBalancer() && len(az.MultipleStandardLoadBalancerConfigurations) == 0
}

// Multiple standard load balancer mode only supports IP-based load balancers.
func (az *Cloud) checkEnableMultipleStandardLoadBalancers() error {
	if az.isLBBackendPoolTypeNodeIPConfig() {
		return fmt.Errorf("multiple standard load balancers cannot be used with backend pool type %s", consts.LoadBalancerBackendPoolConfigurationTypeNodeIPConfiguration)
	}
	return nil
}

func (az *Cloud) isLBBackendPoolTypeNodeIPConfig() bool {
	return strings.EqualFold(az.LoadBalancerBackendPoolConfigurationType, consts.LoadBalancerBackendPoolConfigurationTypeNodeIPConfiguration)
}

func (az *Cloud) isLBBackendPoolTypeNodeIP() bool {
	return strings.EqualFold(az.LoadBalancerBackendPoolConfigurationType, consts.LoadBalancerBackendPoolConfigurationTypeNodeIP)
}

func (az *Cloud) getPutVMSSVMBatchSize() int {
	return az.PutVMSSVMBatchSize
}

func (az *Cloud) initCaches() (err error) {
	if az.Config.DisableAPICallCache {
		klog.Infof("API call cache is disabled, ignore logs about cache operations")
	}

	az.vmCache, err = az.newVMCache()
	if err != nil {
		return err
	}

	az.lbCache, err = az.newLBCache()
	if err != nil {
		return err
	}

	az.nsgCache, err = az.newNSGCache()
	if err != nil {
		return err
	}

	az.rtCache, err = az.newRouteTableCache()
	if err != nil {
		return err
	}

	az.pipCache, err = az.newPIPCache()
	if err != nil {
		return err
	}

	az.plsCache, err = az.newPLSCache()
	if err != nil {
		return err
	}

	return nil
}

func (az *Cloud) setLBDefaults(config *azureconfig.Config) error {
	if config.LoadBalancerSku == "" {
		config.LoadBalancerSku = consts.LoadBalancerSkuStandard
	}

	if strings.EqualFold(config.LoadBalancerSku, consts.LoadBalancerSkuStandard) {
		// Do not add master nodes to standard LB by default.
		if config.ExcludeMasterFromStandardLB == nil {
			config.ExcludeMasterFromStandardLB = &azureconfig.DefaultExcludeMasterFromStandardLB
		}

		// Enable outbound SNAT by default.
		if config.DisableOutboundSNAT == nil {
			config.DisableOutboundSNAT = &azureconfig.DefaultDisableOutboundSNAT
		}
	} else {
		if config.DisableOutboundSNAT != nil && *config.DisableOutboundSNAT {
			return fmt.Errorf("disableOutboundSNAT should only set when loadBalancerSku is standard")
		}
	}
	return nil
}

func (az *Cloud) configureMultiTenantClients(servicePrincipalToken *adal.ServicePrincipalToken) error {
	var err error
	var multiTenantServicePrincipalToken *adal.MultiTenantServicePrincipalToken
	var networkResourceServicePrincipalToken *adal.ServicePrincipalToken
	if az.Config.UsesNetworkResourceInDifferentTenant() {
		multiTenantServicePrincipalToken, err = azureconfig.GetMultiTenantServicePrincipalToken(&az.Config.AzureAuthConfig, &az.Environment)
		if err != nil {
			return err
		}
		networkResourceServicePrincipalToken, err = azureconfig.GetNetworkResourceServicePrincipalToken(&az.Config.AzureAuthConfig, &az.Environment)
		if err != nil {
			return err
		}
	}

	az.configAzureClients(servicePrincipalToken, multiTenantServicePrincipalToken, networkResourceServicePrincipalToken)
	return nil
}

func (az *Cloud) setCloudProviderBackoffDefaults(config *azureconfig.Config) wait.Backoff {
	// Conditionally configure resource request backoff
	resourceRequestBackoff := wait.Backoff{
		Steps: 1,
	}
	if config.CloudProviderBackoff {
		// Assign backoff defaults if no configuration was passed in
		if config.CloudProviderBackoffRetries == 0 {
			config.CloudProviderBackoffRetries = consts.BackoffRetriesDefault
		}
		if config.CloudProviderBackoffDuration == 0 {
			config.CloudProviderBackoffDuration = consts.BackoffDurationDefault
		}
		if config.CloudProviderBackoffExponent == 0 {
			config.CloudProviderBackoffExponent = consts.BackoffExponentDefault
		}

		if config.CloudProviderBackoffJitter == 0 {
			config.CloudProviderBackoffJitter = consts.BackoffJitterDefault
		}

		resourceRequestBackoff = wait.Backoff{
			Steps:    config.CloudProviderBackoffRetries,
			Factor:   config.CloudProviderBackoffExponent,
			Duration: time.Duration(config.CloudProviderBackoffDuration) * time.Second,
			Jitter:   config.CloudProviderBackoffJitter,
		}
		klog.V(2).Infof("Azure cloudprovider using try backoff: retries=%d, exponent=%f, duration=%d, jitter=%f",
			config.CloudProviderBackoffRetries,
			config.CloudProviderBackoffExponent,
			config.CloudProviderBackoffDuration,
			config.CloudProviderBackoffJitter)
	} else {
		// CloudProviderBackoffRetries will be set to 1 by default as the requirements of Azure SDK.
		config.CloudProviderBackoffRetries = 1
		config.CloudProviderBackoffDuration = consts.BackoffDurationDefault
	}
	return resourceRequestBackoff
}

func (az *Cloud) configAzureClients(
	servicePrincipalToken *adal.ServicePrincipalToken,
	multiTenantServicePrincipalToken *adal.MultiTenantServicePrincipalToken,
	networkResourceServicePrincipalToken *adal.ServicePrincipalToken) {
	azClientConfig := az.getAzureClientConfig(servicePrincipalToken)

	// Prepare AzureClientConfig for all azure clients
	interfaceClientConfig := azClientConfig.WithRateLimiter(az.Config.InterfaceRateLimit)
	vmSizeClientConfig := azClientConfig.WithRateLimiter(az.Config.VirtualMachineSizeRateLimit)
	snapshotClientConfig := azClientConfig.WithRateLimiter(az.Config.SnapshotRateLimit)
	storageAccountClientConfig := azClientConfig.WithRateLimiter(az.Config.StorageAccountRateLimit)
	diskClientConfig := azClientConfig.WithRateLimiter(az.Config.DiskRateLimit)
	vmClientConfig := azClientConfig.WithRateLimiter(az.Config.VirtualMachineRateLimit)
	vmssClientConfig := azClientConfig.WithRateLimiter(az.Config.VirtualMachineScaleSetRateLimit)
	// Error "not an active Virtual Machine Scale Set VM" is not retriable for VMSS VM.
	// But http.StatusNotFound is retriable because of ARM replication latency.
	vmssVMClientConfig := azClientConfig.WithRateLimiter(az.Config.VirtualMachineScaleSetRateLimit)
	vmssVMClientConfig.Backoff = vmssVMClientConfig.Backoff.WithNonRetriableErrors([]string{consts.VmssVMNotActiveErrorMessage}).WithRetriableHTTPStatusCodes([]int{http.StatusNotFound})
	routeClientConfig := azClientConfig.WithRateLimiter(az.Config.RouteRateLimit)
	subnetClientConfig := azClientConfig.WithRateLimiter(az.Config.SubnetsRateLimit)
	routeTableClientConfig := azClientConfig.WithRateLimiter(az.Config.RouteTableRateLimit)
	loadBalancerClientConfig := azClientConfig.WithRateLimiter(az.Config.LoadBalancerRateLimit)
	securityGroupClientConfig := azClientConfig.WithRateLimiter(az.Config.SecurityGroupRateLimit)
	publicIPClientConfig := azClientConfig.WithRateLimiter(az.Config.PublicIPAddressRateLimit)
	containerServiceConfig := azClientConfig.WithRateLimiter(az.Config.ContainerServiceRateLimit)
	deploymentConfig := azClientConfig.WithRateLimiter(az.Config.DeploymentRateLimit)
	privateDNSConfig := azClientConfig.WithRateLimiter(az.Config.PrivateDNSRateLimit)
	privateDNSZoenGroupConfig := azClientConfig.WithRateLimiter(az.Config.PrivateDNSZoneGroupRateLimit)
	privateEndpointConfig := azClientConfig.WithRateLimiter(az.Config.PrivateEndpointRateLimit)
	privateLinkServiceConfig := azClientConfig.WithRateLimiter(az.Config.PrivateLinkServiceRateLimit)
	virtualNetworkConfig := azClientConfig.WithRateLimiter(az.Config.VirtualNetworkRateLimit)
	// TODO(ZeroMagic): add azurefileRateLimit
	fileClientConfig := azClientConfig.WithRateLimiter(nil)
	blobClientConfig := azClientConfig.WithRateLimiter(nil)
	vmasClientConfig := azClientConfig.WithRateLimiter(az.Config.AvailabilitySetRateLimit)
	zoneClientConfig := azClientConfig.WithRateLimiter(nil)

	// If uses network resources in different AAD Tenant, update Authorizer for VM/VMSS/VMAS client config
	if multiTenantServicePrincipalToken != nil {
		multiTenantServicePrincipalTokenAuthorizer := autorest.NewMultiTenantServicePrincipalTokenAuthorizer(multiTenantServicePrincipalToken)
		vmClientConfig.Authorizer = multiTenantServicePrincipalTokenAuthorizer
		vmssClientConfig.Authorizer = multiTenantServicePrincipalTokenAuthorizer
		vmssVMClientConfig.Authorizer = multiTenantServicePrincipalTokenAuthorizer
		vmasClientConfig.Authorizer = multiTenantServicePrincipalTokenAuthorizer
	}

	// If uses network resources in different AAD Tenant, update SubscriptionID and Authorizer for network resources client config
	if networkResourceServicePrincipalToken != nil {
		networkResourceServicePrincipalTokenAuthorizer := autorest.NewBearerAuthorizer(networkResourceServicePrincipalToken)
		routeClientConfig.Authorizer = networkResourceServicePrincipalTokenAuthorizer
		subnetClientConfig.Authorizer = networkResourceServicePrincipalTokenAuthorizer
		routeTableClientConfig.Authorizer = networkResourceServicePrincipalTokenAuthorizer
		loadBalancerClientConfig.Authorizer = networkResourceServicePrincipalTokenAuthorizer
		securityGroupClientConfig.Authorizer = networkResourceServicePrincipalTokenAuthorizer
		publicIPClientConfig.Authorizer = networkResourceServicePrincipalTokenAuthorizer
	}

	if az.UsesNetworkResourceInDifferentSubscription() {
		routeClientConfig.SubscriptionID = az.Config.NetworkResourceSubscriptionID
		subnetClientConfig.SubscriptionID = az.Config.NetworkResourceSubscriptionID
		routeTableClientConfig.SubscriptionID = az.Config.NetworkResourceSubscriptionID
		loadBalancerClientConfig.SubscriptionID = az.Config.NetworkResourceSubscriptionID
		securityGroupClientConfig.SubscriptionID = az.Config.NetworkResourceSubscriptionID
		publicIPClientConfig.SubscriptionID = az.Config.NetworkResourceSubscriptionID
	}

	// Initialize all azure clients based on client config
	az.InterfacesClient = interfaceclient.New(interfaceClientConfig)
	az.VirtualMachineSizesClient = vmsizeclient.New(vmSizeClientConfig)
	az.SnapshotsClient = snapshotclient.New(snapshotClientConfig)
	az.StorageAccountClient = storageaccountclient.New(storageAccountClientConfig)
	az.DisksClient = diskclient.New(diskClientConfig)
	az.VirtualMachinesClient = vmclient.New(vmClientConfig)
	az.VirtualMachineScaleSetsClient = vmssclient.New(vmssClientConfig)
	az.VirtualMachineScaleSetVMsClient = vmssvmclient.New(vmssVMClientConfig)
	az.RoutesClient = routeclient.New(routeClientConfig)
	az.SubnetsClient = subnetclient.New(subnetClientConfig)
	az.RouteTablesClient = routetableclient.New(routeTableClientConfig)
	az.LoadBalancerClient = loadbalancerclient.New(loadBalancerClientConfig)
	az.SecurityGroupsClient = securitygroupclient.New(securityGroupClientConfig)
	az.PublicIPAddressesClient = publicipclient.New(publicIPClientConfig)
	az.FileClient = fileclient.New(fileClientConfig)
	az.BlobClient = blobclient.New(blobClientConfig)
	az.AvailabilitySetsClient = vmasclient.New(vmasClientConfig)
	az.privateendpointclient = privateendpointclient.New(privateEndpointConfig)
	az.privatednsclient = privatednsclient.New(privateDNSConfig)
	az.privatednszonegroupclient = privatednszonegroupclient.New(privateDNSZoenGroupConfig)
	az.virtualNetworkLinksClient = virtualnetworklinksclient.New(virtualNetworkConfig)
	az.PrivateLinkServiceClient = privatelinkserviceclient.New(privateLinkServiceConfig)
	az.containerServiceClient = containerserviceclient.New(containerServiceConfig)
	az.deploymentClient = deploymentclient.New(deploymentConfig)

	if az.ZoneClient == nil {
		az.ZoneClient = zoneclient.New(zoneClientConfig)
	}
}

func (az *Cloud) getAzureClientConfig(servicePrincipalToken *adal.ServicePrincipalToken) *azclients.ClientConfig {
	azClientConfig := &azclients.ClientConfig{
		CloudName:               az.Config.Cloud,
		Location:                az.Config.Location,
		SubscriptionID:          az.Config.SubscriptionID,
		ResourceManagerEndpoint: az.Environment.ResourceManagerEndpoint,
		Authorizer:              autorest.NewBearerAuthorizer(servicePrincipalToken),
		Backoff:                 &retry.Backoff{Steps: 1},
		DisableAzureStackCloud:  az.Config.DisableAzureStackCloud,
		UserAgent:               az.Config.UserAgent,
	}

	if az.Config.CloudProviderBackoff {
		azClientConfig.Backoff = &retry.Backoff{
			Steps:    az.Config.CloudProviderBackoffRetries,
			Factor:   az.Config.CloudProviderBackoffExponent,
			Duration: time.Duration(az.Config.CloudProviderBackoffDuration) * time.Second,
			Jitter:   az.Config.CloudProviderBackoffJitter,
		}
	}

	if az.Config.HasExtendedLocation() {
		azClientConfig.ExtendedLocation = &azclients.ExtendedLocation{
			Name: az.Config.ExtendedLocationName,
			Type: az.Config.ExtendedLocationType,
		}
	}

	return azClientConfig
}

// ParseConfig returns a parsed configuration for an Azure cloudprovider config file
func ParseConfig(configReader io.Reader) (*azureconfig.Config, error) {
	var config azureconfig.Config
	if configReader == nil {
		return nil, nil
	}

	configContents, err := io.ReadAll(configReader)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(configContents, &config)
	if err != nil {
		return nil, err
	}

	// The resource group name may be in different cases from different Azure APIs, hence it is converted to lower here.
	// See more context at https://github.com/kubernetes/kubernetes/issues/71994.
	config.ResourceGroup = strings.ToLower(config.ResourceGroup)

	// these environment variables are injected by workload identity webhook
	if tenantID := os.Getenv("AZURE_TENANT_ID"); tenantID != "" {
		config.TenantID = tenantID
	}
	if clientID := os.Getenv("AZURE_CLIENT_ID"); clientID != "" {
		config.AADClientID = clientID
	}
	if federatedTokenFile := os.Getenv("AZURE_FEDERATED_TOKEN_FILE"); federatedTokenFile != "" {
		config.AADFederatedTokenFile = federatedTokenFile
		config.UseFederatedWorkloadIdentityExtension = true
	}
	return &config, nil
}

func (az *Cloud) isStackCloud() bool {
	return strings.EqualFold(az.Config.Cloud, consts.AzureStackCloudName) && !az.Config.DisableAzureStackCloud
}

// Initialize passes a Kubernetes clientBuilder interface to the cloud provider
func (az *Cloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	az.KubeClient = clientBuilder.ClientOrDie("azure-cloud-provider")
	az.eventBroadcaster = record.NewBroadcaster()
	az.eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: az.KubeClient.CoreV1().Events("")})
	az.eventRecorder = az.eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "azure-cloud-provider"})
}

// LoadBalancer returns a balancer interface. Also returns true if the interface is supported, false otherwise.
func (az *Cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return az, true
}

// Instances returns an instances interface. Also returns true if the interface is supported, false otherwise.
func (az *Cloud) Instances() (cloudprovider.Instances, bool) {
	return az, true
}

// InstancesV2 returns an instancesV2 interface. Also returns true if the interface is supported, false otherwise.
func (az *Cloud) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return az, true
}

// Zones returns a zones interface. Also returns true if the interface is supported, false otherwise.
func (az *Cloud) Zones() (cloudprovider.Zones, bool) {
	if az.isStackCloud() {
		// Azure stack does not support zones at this point
		// https://docs.microsoft.com/en-us/azure-stack/user/azure-stack-network-differences?view=azs-2102
		return nil, false
	}
	return az, true
}

// Clusters returns a clusters interface.  Also returns true if the interface is supported, false otherwise.
func (az *Cloud) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// Routes returns a routes interface along with whether the interface is supported.
func (az *Cloud) Routes() (cloudprovider.Routes, bool) {
	return az, true
}

// HasClusterID returns true if the cluster has a clusterID
func (az *Cloud) HasClusterID() bool {
	return true
}

// ProviderName returns the cloud provider ID.
func (az *Cloud) ProviderName() string {
	return consts.CloudProviderName
}

func initDiskControllers(az *Cloud) error {
	// Common controller contains the function
	// needed by both blob disk and managed disk controllers

	qps := float32(azureconfig.DefaultAtachDetachDiskQPS)
	bucket := azureconfig.DefaultAtachDetachDiskBucket
	if az.Config.AttachDetachDiskRateLimit != nil {
		qps = az.Config.AttachDetachDiskRateLimit.CloudProviderRateLimitQPSWrite
		bucket = az.Config.AttachDetachDiskRateLimit.CloudProviderRateLimitBucketWrite
	}
	klog.V(2).Infof("attach/detach disk operation rate limit QPS: %f, Bucket: %d", qps, bucket)

	common := &controllerCommon{
		cloud:                        az,
		lockMap:                      newLockMap(),
		diskOpRateLimiter:            flowcontrol.NewTokenBucketRateLimiter(qps, bucket),
		AttachDetachInitialDelayInMs: defaultAttachDetachInitialDelayInMs,
	}

	az.ManagedDiskController = &ManagedDiskController{common: common}
	az.controllerCommon = common

	return nil
}

// SetInformers sets informers for Azure cloud provider.
func (az *Cloud) SetInformers(informerFactory informers.SharedInformerFactory) {
	klog.Infof("Setting up informers for Azure cloud provider")
	nodeInformer := informerFactory.Core().V1().Nodes().Informer()
	_, _ = nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node := obj.(*v1.Node)
			az.updateNodeCaches(nil, node)
		},
		UpdateFunc: func(prev, obj interface{}) {
			prevNode := prev.(*v1.Node)
			newNode := obj.(*v1.Node)
			az.updateNodeCaches(prevNode, newNode)
		},
		DeleteFunc: func(obj interface{}) {
			node, isNode := obj.(*v1.Node)
			// We can get DeletedFinalStateUnknown instead of *v1.Node here
			// and we need to handle that correctly.
			if !isNode {
				deletedState, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					klog.Errorf("Received unexpected object: %v", obj)
					return
				}
				node, ok = deletedState.Obj.(*v1.Node)
				if !ok {
					klog.Errorf("DeletedFinalStateUnknown contained non-Node object: %v", deletedState.Obj)
					return
				}
			}
			az.updateNodeCaches(node, nil)

			klog.V(4).Infof("Removing node %s from VMSet cache.", node.Name)
			_ = az.VMSet.DeleteCacheForNode(node.Name)
		},
	})
	az.nodeInformerSynced = nodeInformer.HasSynced

	az.serviceLister = informerFactory.Core().V1().Services().Lister()
}

// updateNodeCaches updates local cache for node's zones and external resource groups.
func (az *Cloud) updateNodeCaches(prevNode, newNode *v1.Node) {
	az.nodeCachesLock.Lock()
	defer az.nodeCachesLock.Unlock()

	if prevNode != nil {
		// Remove from nodeNames cache.
		az.nodeNames.Delete(prevNode.ObjectMeta.Name)

		// Remove from nodeZones cache.
		prevZone, ok := prevNode.ObjectMeta.Labels[v1.LabelTopologyZone]
		if ok && az.isAvailabilityZone(prevZone) {
			az.nodeZones[prevZone].Delete(prevNode.ObjectMeta.Name)
			if az.nodeZones[prevZone].Len() == 0 {
				az.nodeZones[prevZone] = nil
			}
		}

		// Remove from nodeResourceGroups cache.
		_, ok = prevNode.ObjectMeta.Labels[consts.ExternalResourceGroupLabel]
		if ok {
			delete(az.nodeResourceGroups, prevNode.ObjectMeta.Name)
		}

		managed, ok := prevNode.ObjectMeta.Labels[consts.ManagedByAzureLabel]
		isNodeManagedByCloudProvider := !ok || !strings.EqualFold(managed, consts.NotManagedByAzureLabelValue)

		klog.Infof("managed=%v, ok=%v, isNodeManagedByCloudProvider=%v",
			managed, ok, isNodeManagedByCloudProvider)

		// Remove from unmanagedNodes cache
		if !isNodeManagedByCloudProvider {
			az.unmanagedNodes.Delete(prevNode.ObjectMeta.Name)
		}

		// if the node is being deleted from the cluster, exclude it from load balancers
		if newNode == nil {
			az.excludeLoadBalancerNodes.Insert(prevNode.ObjectMeta.Name)
		}

		// Remove from nodePrivateIPs cache.
		for _, address := range getNodePrivateIPAddresses(prevNode) {
			klog.V(4).Infof("removing IP address %s of the node %s", address, prevNode.Name)
			az.nodePrivateIPs[prevNode.Name].Delete(address)
		}
	}

	if newNode != nil {
		// Add to nodeNames cache.
		az.nodeNames.Insert(newNode.ObjectMeta.Name)

		// Add to nodeZones cache.
		newZone, ok := newNode.ObjectMeta.Labels[v1.LabelTopologyZone]
		if ok && az.isAvailabilityZone(newZone) {
			if az.nodeZones[newZone] == nil {
				az.nodeZones[newZone] = sets.New[string]()
			}
			az.nodeZones[newZone].Insert(newNode.ObjectMeta.Name)
		}

		// Add to nodeResourceGroups cache.
		newRG, ok := newNode.ObjectMeta.Labels[consts.ExternalResourceGroupLabel]
		if ok && len(newRG) > 0 {
			az.nodeResourceGroups[newNode.ObjectMeta.Name] = strings.ToLower(newRG)
		}

		_, hasExcludeBalancerLabel := newNode.ObjectMeta.Labels[v1.LabelNodeExcludeBalancers]
		managed, ok := newNode.ObjectMeta.Labels[consts.ManagedByAzureLabel]
		isNodeManagedByCloudProvider := !ok || !strings.EqualFold(managed, consts.NotManagedByAzureLabelValue)

		// Update unmanagedNodes cache
		if !isNodeManagedByCloudProvider {
			az.unmanagedNodes.Insert(newNode.ObjectMeta.Name)
		}

		// Update excludeLoadBalancerNodes cache
		switch {
		case !isNodeManagedByCloudProvider:
			az.excludeLoadBalancerNodes.Insert(newNode.ObjectMeta.Name)
			klog.V(6).Infof("excluding Node %q from LoadBalancer because it is not managed by cloud provider", newNode.ObjectMeta.Name)

		case hasExcludeBalancerLabel:
			az.excludeLoadBalancerNodes.Insert(newNode.ObjectMeta.Name)
			klog.V(6).Infof("excluding Node %q from LoadBalancer because it has exclude-from-external-load-balancers label", newNode.ObjectMeta.Name)

		case !isNodeReady(newNode) && nodemanager.GetCloudTaint(newNode.Spec.Taints) == nil:
			// If not in ready state and not a newly created node, add to excludeLoadBalancerNodes cache.
			// New nodes (tainted with "node.cloudprovider.kubernetes.io/uninitialized") should not be
			// excluded from load balancers regardless of their state, so as to reduce the number of
			// VMSS API calls and not provoke VMScaleSetActiveModelsCountLimitReached.
			// (https://github.com/kubernetes-sigs/cloud-provider-azure/issues/851)
			az.excludeLoadBalancerNodes.Insert(newNode.ObjectMeta.Name)
			klog.V(6).Infof("excluding Node %q from LoadBalancer because it is not in ready state or a newly created one", newNode.ObjectMeta.Name)

		default:
			// Nodes not falling into the three cases above are valid backends and
			// should not appear in excludeLoadBalancerNodes cache.
			az.excludeLoadBalancerNodes.Delete(newNode.ObjectMeta.Name)
		}

		// Add to nodePrivateIPs cache
		for _, address := range getNodePrivateIPAddresses(newNode) {
			if az.nodePrivateIPs[newNode.Name] == nil {
				az.nodePrivateIPs[newNode.Name] = sets.New[string]()
			}

			klog.V(6).Infof("adding IP address %s of the node %s", address, newNode.Name)
			az.nodePrivateIPs[newNode.Name].Insert(address)
		}
	}
}

// GetActiveZones returns all the zones in which k8s nodes are currently running.
func (az *Cloud) GetActiveZones() (sets.Set[string], error) {
	if az.nodeInformerSynced == nil {
		return nil, fmt.Errorf("azure cloud provider doesn't have informers set")
	}

	az.nodeCachesLock.RLock()
	defer az.nodeCachesLock.RUnlock()
	if !az.nodeInformerSynced() {
		return nil, fmt.Errorf("node informer is not synced when trying to GetActiveZones")
	}

	zones := sets.New[string]()
	for zone, nodes := range az.nodeZones {
		if len(nodes) > 0 {
			zones.Insert(zone)
		}
	}
	return zones, nil
}

// GetLocation returns the location in which k8s cluster is currently running.
func (az *Cloud) GetLocation() string {
	return az.Location
}

// GetNodeResourceGroup gets resource group for given node.
func (az *Cloud) GetNodeResourceGroup(nodeName string) (string, error) {
	// Kubelet won't set az.nodeInformerSynced, always return configured resourceGroup.
	if az.nodeInformerSynced == nil {
		return az.ResourceGroup, nil
	}

	az.nodeCachesLock.RLock()
	defer az.nodeCachesLock.RUnlock()
	if !az.nodeInformerSynced() {
		return "", fmt.Errorf("node informer is not synced when trying to GetNodeResourceGroup")
	}

	// Return external resource group if it has been cached.
	if cachedRG, ok := az.nodeResourceGroups[nodeName]; ok {
		return cachedRG, nil
	}

	// Return resource group from cloud provider options.
	return az.ResourceGroup, nil
}

// GetNodeNames returns a set of all node names in the k8s cluster.
func (az *Cloud) GetNodeNames() (sets.Set[string], error) {
	// Kubelet won't set az.nodeInformerSynced, return nil.
	if az.nodeInformerSynced == nil {
		return nil, nil
	}

	az.nodeCachesLock.RLock()
	defer az.nodeCachesLock.RUnlock()
	if !az.nodeInformerSynced() {
		return nil, fmt.Errorf("node informer is not synced when trying to GetNodeNames")
	}

	return sets.New(az.nodeNames.UnsortedList()...), nil
}

// GetResourceGroups returns a set of resource groups that all nodes are running on.
func (az *Cloud) GetResourceGroups() (sets.Set[string], error) {
	// Kubelet won't set az.nodeInformerSynced, always return configured resourceGroup.
	if az.nodeInformerSynced == nil {
		return sets.New(az.ResourceGroup), nil
	}

	az.nodeCachesLock.RLock()
	defer az.nodeCachesLock.RUnlock()
	if !az.nodeInformerSynced() {
		return nil, fmt.Errorf("node informer is not synced when trying to GetResourceGroups")
	}

	resourceGroups := sets.New(az.ResourceGroup)
	for _, rg := range az.nodeResourceGroups {
		resourceGroups.Insert(rg)
	}

	return resourceGroups, nil
}

// GetUnmanagedNodes returns a list of nodes not managed by Azure cloud provider (e.g. on-prem nodes).
func (az *Cloud) GetUnmanagedNodes() (sets.Set[string], error) {
	// Kubelet won't set az.nodeInformerSynced, always return nil.
	if az.nodeInformerSynced == nil {
		return nil, nil
	}

	az.nodeCachesLock.RLock()
	defer az.nodeCachesLock.RUnlock()
	if !az.nodeInformerSynced() {
		return nil, fmt.Errorf("node informer is not synced when trying to GetUnmanagedNodes")
	}

	return sets.New(az.unmanagedNodes.UnsortedList()...), nil
}

// ShouldNodeExcludedFromLoadBalancer returns true if node is unmanaged, in external resource group or labeled with "node.kubernetes.io/exclude-from-external-load-balancers".
func (az *Cloud) ShouldNodeExcludedFromLoadBalancer(nodeName string) (bool, error) {
	// Kubelet won't set az.nodeInformerSynced, always return nil.
	if az.nodeInformerSynced == nil {
		return false, nil
	}

	az.nodeCachesLock.RLock()
	defer az.nodeCachesLock.RUnlock()
	if !az.nodeInformerSynced() {
		return false, fmt.Errorf("node informer is not synced when trying to fetch node caches")
	}

	// Return true if the node is in external resource group.
	if cachedRG, ok := az.nodeResourceGroups[nodeName]; ok && !strings.EqualFold(cachedRG, az.ResourceGroup) {
		return true, nil
	}

	return az.excludeLoadBalancerNodes.Has(nodeName), nil
}

func isNodeReady(node *v1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == v1.NodeReady && cond.Status == v1.ConditionTrue {
			return true
		}
	}
	return false
}
