package config

import (
	"sigs.k8s.io/cloud-provider-azure/pkg/auth"
)

var (
	// Master nodes are not added to standard load balancer by default.
	defaultExcludeMasterFromStandardLB = true
	// Outbound SNAT is enabled by default.
	defaultDisableOutboundSNAT = false
	// RouteUpdateWaitingInSeconds is 30 seconds by default.
	defaultRouteUpdateWaitingInSeconds = 30
)

// Config holds the configuration parsed from the --cloud-config flag
// All fields are required unless otherwise specified
// NOTE: Cloud config files should follow the same Kubernetes deprecation policy as
// flags or CLIs. Config fields should not change behavior in incompatible ways and
// should be deprecated for at least 2 release prior to removing.
// See https://kubernetes.io/docs/reference/using-api/deprecation-policy/#deprecating-a-flag-or-cli
// for more details.
type Config struct {
	auth.AzureAuthConfig

	// The cloud configure type for Azure cloud provider. Supported values are file, secret and merge.
	CloudConfigType cloudConfigType `json:"cloudConfigType,omitempty" yaml:"cloudConfigType,omitempty"`

	// The name of the resource group that the cluster is deployed in
	ResourceGroup string `json:"resourceGroup,omitempty" yaml:"resourceGroup,omitempty"`
	// The location of the resource group that the cluster is deployed in
	Location string `json:"location,omitempty" yaml:"location,omitempty"`
	// The name of site where the cluster will be deployed to that is more granular than the region specified by the "location" field.
	// Currently only public ip, load balancer and managed disks support this.
	ExtendedLocationName string `json:"extendedLocationName,omitempty" yaml:"extendedLocationName,omitempty"`
	// The type of site that is being targeted.
	// Currently only public ip, load balancer and managed disks support this.
	ExtendedLocationType string `json:"extendedLocationType,omitempty" yaml:"extendedLocationType,omitempty"`
	// The name of the VNet that the cluster is deployed in
	VnetName string `json:"vnetName,omitempty" yaml:"vnetName,omitempty"`
	// The name of the resource group that the Vnet is deployed in
	VnetResourceGroup string `json:"vnetResourceGroup,omitempty" yaml:"vnetResourceGroup,omitempty"`
	// The name of the subnet that the cluster is deployed in
	SubnetName string `json:"subnetName,omitempty" yaml:"subnetName,omitempty"`
	// The name of the security group attached to the cluster's subnet
	SecurityGroupName string `json:"securityGroupName,omitempty" yaml:"securityGroupName,omitempty"`
	// The name of the resource group that the security group is deployed in
	SecurityGroupResourceGroup string `json:"securityGroupResourceGroup,omitempty" yaml:"securityGroupResourceGroup,omitempty"`
	// (Optional in 1.6) The name of the route table attached to the subnet that the cluster is deployed in
	RouteTableName string `json:"routeTableName,omitempty" yaml:"routeTableName,omitempty"`
	// The name of the resource group that the RouteTable is deployed in
	RouteTableResourceGroup string `json:"routeTableResourceGroup,omitempty" yaml:"routeTableResourceGroup,omitempty"`
	// (Optional) The name of the availability set that should be used as the load balancer backend
	// If this is set, the Azure cloudprovider will only add nodes from that availability set to the load
	// balancer backend pool. If this is not set, and multiple agent pools (availability sets) are used, then
	// the cloudprovider will try to add all nodes to a single backend pool which is forbidden.
	// In other words, if you use multiple agent pools (availability sets), you MUST set this field.
	PrimaryAvailabilitySetName string `json:"primaryAvailabilitySetName,omitempty" yaml:"primaryAvailabilitySetName,omitempty"`
	// The type of azure nodes. Candidate values are: vmss and standard.
	// If not set, it will be default to standard.
	VMType string `json:"vmType,omitempty" yaml:"vmType,omitempty"`
	// The name of the scale set that should be used as the load balancer backend.
	// If this is set, the Azure cloudprovider will only add nodes from that scale set to the load
	// balancer backend pool. If this is not set, and multiple agent pools (scale sets) are used, then
	// the cloudprovider will try to add all nodes to a single backend pool which is forbidden in the basic sku.
	// In other words, if you use multiple agent pools (scale sets), and loadBalancerSku is set to basic, you MUST set this field.
	PrimaryScaleSetName string `json:"primaryScaleSetName,omitempty" yaml:"primaryScaleSetName,omitempty"`
	// Tags determines what tags shall be applied to the shared resources managed by controller manager, which
	// includes load balancer, security group and route table. The supported format is `a=b,c=d,...`. After updated
	// this config, the old tags would be replaced by the new ones.
	// Because special characters are not supported in "tags" configuration, "tags" support would be removed in a future release,
	// please consider migrating the config to "tagsMap".
	Tags string `json:"tags,omitempty" yaml:"tags,omitempty"`
	// TagsMap is similar to Tags but holds tags with special characters such as `=` and `,`.
	TagsMap map[string]string `json:"tagsMap,omitempty" yaml:"tagsMap,omitempty"`
	// SystemTags determines the tag keys managed by cloud provider. If it is not set, no tags would be deleted if
	// the `Tags` is changed. However, the old tags would be deleted if they are neither included in `Tags` nor
	// in `SystemTags` after the update of `Tags`.
	SystemTags string `json:"systemTags,omitempty" yaml:"systemTags,omitempty"`
	// Sku of Load Balancer and Public IP. Candidate values are: basic and standard.
	// If not set, it will be default to basic.
	LoadBalancerSku string `json:"loadBalancerSku,omitempty" yaml:"loadBalancerSku,omitempty"`
	// LoadBalancerName determines the specific name of the load balancer user want to use, working with
	// LoadBalancerResourceGroup
	LoadBalancerName string `json:"loadBalancerName,omitempty" yaml:"loadBalancerName,omitempty"`
	// LoadBalancerResourceGroup determines the specific resource group of the load balancer user want to use, working
	// with LoadBalancerName
	LoadBalancerResourceGroup string `json:"loadBalancerResourceGroup,omitempty" yaml:"loadBalancerResourceGroup,omitempty"`
	// PreConfiguredBackendPoolLoadBalancerTypes determines whether the LoadBalancer BackendPool has been preconfigured.
	// Candidate values are:
	//   "": exactly with today (not pre-configured for any LBs)
	//   "internal": for internal LoadBalancer
	//   "external": for external LoadBalancer
	//   "all": for both internal and external LoadBalancer
	PreConfiguredBackendPoolLoadBalancerTypes string `json:"preConfiguredBackendPoolLoadBalancerTypes,omitempty" yaml:"preConfiguredBackendPoolLoadBalancerTypes,omitempty"`

	// DisableAvailabilitySetNodes disables VMAS nodes support when "VMType" is set to "vmss".
	DisableAvailabilitySetNodes bool `json:"disableAvailabilitySetNodes,omitempty" yaml:"disableAvailabilitySetNodes,omitempty"`
	// EnableVmssFlexNodes enables vmss flex nodes support when "VMType" is set to "vmss".
	EnableVmssFlexNodes bool `json:"enableVmssFlexNodes,omitempty" yaml:"enableVmssFlexNodes,omitempty"`
	// DisableAzureStackCloud disables AzureStackCloud support. It should be used
	// when setting AzureAuthConfig.Cloud with "AZURESTACKCLOUD" to customize ARM endpoints
	// while the cluster is not running on AzureStack.
	DisableAzureStackCloud bool `json:"disableAzureStackCloud,omitempty" yaml:"disableAzureStackCloud,omitempty"`
	// Enable exponential backoff to manage resource request retries
	CloudProviderBackoff bool `json:"cloudProviderBackoff,omitempty" yaml:"cloudProviderBackoff,omitempty"`
	// Use instance metadata service where possible
	UseInstanceMetadata bool `json:"useInstanceMetadata,omitempty" yaml:"useInstanceMetadata,omitempty"`

	// EnableMultipleStandardLoadBalancers determines the behavior of the standard load balancer. If set to true
	// there would be one standard load balancer per VMAS or VMSS, which is similar with the behavior of the basic
	// load balancer. Users could select the specific standard load balancer for their service by the service
	// annotation `service.beta.kubernetes.io/azure-load-balancer-mode`, If set to false, the same standard load balancer
	// would be shared by all services in the cluster. In this case, the mode selection annotation would be ignored.
	EnableMultipleStandardLoadBalancers bool `json:"enableMultipleStandardLoadBalancers,omitempty" yaml:"enableMultipleStandardLoadBalancers,omitempty"`
	// NodePoolsWithoutDedicatedSLB stores the VMAS/VMSS names that share the primary standard load balancer instead
	// of having a dedicated one. This is useful only when EnableMultipleStandardLoadBalancers is set to true.
	NodePoolsWithoutDedicatedSLB string `json:"nodePoolsWithoutDedicatedSLB,omitempty" yaml:"nodePoolsWithoutDedicatedSLB,omitempty"`

	// Backoff exponent
	CloudProviderBackoffExponent float64 `json:"cloudProviderBackoffExponent,omitempty" yaml:"cloudProviderBackoffExponent,omitempty"`
	// Backoff jitter
	CloudProviderBackoffJitter float64 `json:"cloudProviderBackoffJitter,omitempty" yaml:"cloudProviderBackoffJitter,omitempty"`

	// ExcludeMasterFromStandardLB excludes master nodes from standard load balancer.
	// If not set, it will be default to true.
	ExcludeMasterFromStandardLB *bool `json:"excludeMasterFromStandardLB,omitempty" yaml:"excludeMasterFromStandardLB,omitempty"`
	// DisableOutboundSNAT disables the outbound SNAT for public load balancer rules.
	// It should only be set when loadBalancerSku is standard. If not set, it will be default to false.
	DisableOutboundSNAT *bool `json:"disableOutboundSNAT,omitempty" yaml:"disableOutboundSNAT,omitempty"`

	// Maximum allowed LoadBalancer Rule Count is the limit enforced by Azure Load balancer
	MaximumLoadBalancerRuleCount int `json:"maximumLoadBalancerRuleCount,omitempty" yaml:"maximumLoadBalancerRuleCount,omitempty"`
	// Backoff retry limit
	CloudProviderBackoffRetries int `json:"cloudProviderBackoffRetries,omitempty" yaml:"cloudProviderBackoffRetries,omitempty"`
	// Backoff duration
	CloudProviderBackoffDuration int `json:"cloudProviderBackoffDuration,omitempty" yaml:"cloudProviderBackoffDuration,omitempty"`
	// NonVmssUniformNodesCacheTTLInSeconds sets the Cache TTL for NonVmssUniformNodesCacheTTLInSeconds
	// if not set, will use default value
	NonVmssUniformNodesCacheTTLInSeconds int `json:"nonVmssUniformNodesCacheTTLInSeconds,omitempty" yaml:"nonVmssUniformNodesCacheTTLInSeconds,omitempty"`
	// AvailabilitySetNodesCacheTTLInSeconds sets the Cache TTL for availabilitySetNodesCache
	// if not set, will use default value
	AvailabilitySetNodesCacheTTLInSeconds int `json:"availabilitySetNodesCacheTTLInSeconds,omitempty" yaml:"availabilitySetNodesCacheTTLInSeconds,omitempty"`
	// VmssCacheTTLInSeconds sets the cache TTL for VMSS
	VmssCacheTTLInSeconds int `json:"vmssCacheTTLInSeconds,omitempty" yaml:"vmssCacheTTLInSeconds,omitempty"`
	// VmssVirtualMachinesCacheTTLInSeconds sets the cache TTL for vmssVirtualMachines
	VmssVirtualMachinesCacheTTLInSeconds int `json:"vmssVirtualMachinesCacheTTLInSeconds,omitempty" yaml:"vmssVirtualMachinesCacheTTLInSeconds,omitempty"`

	// VmssFlexCacheTTLInSeconds sets the cache TTL for VMSS Flex
	VmssFlexCacheTTLInSeconds int `json:"vmssFlexCacheTTLInSeconds,omitempty" yaml:"vmssFlexCacheTTLInSeconds,omitempty"`
	// VmssFlexVMCacheTTLInSeconds sets the cache TTL for vmss flex vms
	VmssFlexVMCacheTTLInSeconds int `json:"vmssFlexVMCacheTTLInSeconds,omitempty" yaml:"vmssFlexVMCacheTTLInSeconds,omitempty"`

	// VmCacheTTLInSeconds sets the cache TTL for vm
	VMCacheTTLInSeconds int `json:"vmCacheTTLInSeconds,omitempty" yaml:"vmCacheTTLInSeconds,omitempty"`
	// LoadBalancerCacheTTLInSeconds sets the cache TTL for load balancer
	LoadBalancerCacheTTLInSeconds int `json:"loadBalancerCacheTTLInSeconds,omitempty" yaml:"loadBalancerCacheTTLInSeconds,omitempty"`
	// NsgCacheTTLInSeconds sets the cache TTL for network security group
	NsgCacheTTLInSeconds int `json:"nsgCacheTTLInSeconds,omitempty" yaml:"nsgCacheTTLInSeconds,omitempty"`
	// RouteTableCacheTTLInSeconds sets the cache TTL for route table
	RouteTableCacheTTLInSeconds int `json:"routeTableCacheTTLInSeconds,omitempty" yaml:"routeTableCacheTTLInSeconds,omitempty"`
	// PlsCacheTTLInSeconds sets the cache TTL for private link service resource
	PlsCacheTTLInSeconds int `json:"plsCacheTTLInSeconds,omitempty" yaml:"plsCacheTTLInSeconds,omitempty"`
	// AvailabilitySetsCacheTTLInSeconds sets the cache TTL for VMAS
	AvailabilitySetsCacheTTLInSeconds int `json:"availabilitySetsCacheTTLInSeconds,omitempty" yaml:"availabilitySetsCacheTTLInSeconds,omitempty"`
	// PublicIPCacheTTLInSeconds sets the cache TTL for public ip
	PublicIPCacheTTLInSeconds int `json:"publicIPCacheTTLInSeconds,omitempty" yaml:"publicIPCacheTTLInSeconds,omitempty"`
	// RouteUpdateWaitingInSeconds is the delay time for waiting route updates to take effect. This waiting delay is added
	// because the routes are not taken effect when the async route updating operation returns success. Default is 30 seconds.
	RouteUpdateWaitingInSeconds int `json:"routeUpdateWaitingInSeconds,omitempty" yaml:"routeUpdateWaitingInSeconds,omitempty"`
	// The user agent for Azure customer usage attribution
	UserAgent string `json:"userAgent,omitempty" yaml:"userAgent,omitempty"`
	// LoadBalancerBackendPoolConfigurationType defines how vms join the load balancer backend pools. Supported values
	// are `nodeIPConfiguration`, `nodeIP` and `podIP`.
	// `nodeIPConfiguration`: vm network interfaces will be attached to the inbound backend pool of the load balancer (default);
	// `nodeIP`: vm private IPs will be attached to the inbound backend pool of the load balancer;
	// `podIP`: pod IPs will be attached to the inbound backend pool of the load balancer (not supported yet).
	LoadBalancerBackendPoolConfigurationType string `json:"loadBalancerBackendPoolConfigurationType,omitempty" yaml:"loadBalancerBackendPoolConfigurationType,omitempty"`
	// PutVMSSVMBatchSize defines how many requests the client send concurrently when putting the VMSS VMs.
	// If it is smaller than or equal to zero, the request will be sent one by one in sequence (default).
	PutVMSSVMBatchSize int `json:"putVMSSVMBatchSize" yaml:"putVMSSVMBatchSize"`
	// PrivateLinkServiceResourceGroup determines the specific resource group of the private link services user want to use
	PrivateLinkServiceResourceGroup string `json:"privateLinkServiceResourceGroup,omitempty" yaml:"privateLinkServiceResourceGroup,omitempty"`
}

// HasExtendedLocation returns true if extendedlocation prop are specified.
func (config *Config) HasExtendedLocation() bool {
	return config.ExtendedLocationName != "" && config.ExtendedLocationType != ""
}
