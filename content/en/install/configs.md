---
title: "Configure Cloud Provider"
linkTitle: "Configurations"
type: docs
weight: 1
description: >
    The configurations for Cloud Provider Azure.
---

This doc describes cloud provider config file, which is to be used via the `--cloud-config` flag of azure-cloud-controller-manager.

Here is a config file sample:

```json
{
    "cloud":"AzurePublicCloud",
    "tenantId": "0000000-0000-0000-0000-000000000000",
    "aadClientId": "0000000-0000-0000-0000-000000000000",
    "aadClientSecret": "0000000-0000-0000-0000-000000000000",
    "subscriptionId": "0000000-0000-0000-0000-000000000000",
    "resourceGroup": "<name>",
    "location": "eastus",
    "subnetName": "<name>",
    "securityGroupName": "<name>",
    "securityGroupResourceGroup": "<name>",
    "vnetName": "<name>",
    "vnetResourceGroup": "<name>",
    "routeTableName": "<name>",
    "primaryAvailabilitySetName": "<name>",
    "routeTableResourceGroup": "<name>",
    "cloudProviderBackoff": false,
    "useManagedIdentityExtension": false,
    "useInstanceMetadata": true
}
```

Note: All values are of type `string` if not explicitly called out.

## Auth configs

|Name|Description|Remark|
|---|---|---|
|cloud|The cloud environment identifier|Valid values could be found  [here](https://github.com/Azure/go-autorest/blob/v9.9.0/autorest/azure/environments.go#L29). Default to `AzurePublicCloud`.|
|tenantID|The AAD Tenant ID for the Subscription that the cluster is deployed in|**Required**.|
|aadClientID|The ClientID for an AAD application with RBAC access to talk to Azure RM APIs|Used for service principal authn.|
|aadClientSecret|The ClientSecret for an AAD application with RBAC access to talk to Azure RM APIs|Used for service principal  authn.|
|aadClientCertPath|The path of a client certificate for an AAD application with RBAC access to talk to Azure RM APIs|Used for client cert authn.|
|aadClientCertPassword|The password of the client certificate for an AAD application with RBAC access to talk to Azure RM APIs|Used for client cert authn.|
|useManagedIdentityExtension|Use managed service identity for the virtual machine to access Azure ARM APIs|Boolean type, default to false.|
|userAssignedIdentityID|The Client ID of the user assigned MSI which is assigned to the underlying VMs|Required for user-assigned managed identity.|
|subscriptionId|The ID of the Azure Subscription that the cluster is deployed in|**Required**.|
|identitySystem|The identity system for AzureStack. Supported values are: ADFS|Only used for AzureStack|
|networkResourceTenantID|The AAD Tenant ID for the Subscription that the network resources are deployed in|Optional. Supported since v1.18.0. Only used for hosting network resources in different AAD Tenant and Subscription than those for the cluster.|
|networkResourceSubscriptionID|The ID of the Azure Subscription that the network resources are deployed in|Optional. Supported since v1.18.0. Only used for hosting network resources in different AAD Tenant and Subscription than those for the cluster.|

Note: Cloud provider currently supports three authentication methods, you can choose one combination of them:

- [Managed Identity](https://docs.microsoft.com/en-us/azure/active-directory/managed-service-identity/overview):
  - For system-assigned managed identity: set `useManagedIdentityExtension` to true
  - For user-assigned managed identity: set `useManagedIdentityExtension` to true and also set `userAssignedIdentityID`
- [Service Principal](https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/main/docs/book/src/topics/getting-started.md#setting-up-your-azure-environment): set `aadClientID` and `aadClientSecret`
- [Client Certificate](https://docs.microsoft.com/en-us/azure/active-directory/develop/active-directory-protocols-oauth-service-to-service): set `aadClientCertPath` and `aadClientCertPassword`

If more than one value is set, the order is `Managed Identity` > `Service Principal` > `Client Certificate`.

## Cluster config

| Name                                                       | Description                                                                                                                                                                                                       | Remark                                                                                                                                |
|------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------|
| resourceGroup                                              | The name of the resource group that the cluster is deployed in                                                                                                                                                    |                                                                                                                                       |
| location                                                   | The location of the resource group that the cluster is deployed in                                                                                                                                                |                                                                                                                                       |
| vnetName                                                   | The name of the VNet that the cluster is deployed in                                                                                                                                                              |                                                                                                                                       |
| vnetResourceGroup                                          | The name of the resource group that the Vnet is deployed in                                                                                                                                                       |                                                                                                                                       |
| subnetName                                                 | The name of the subnet that the cluster is deployed in                                                                                                                                                            |                                                                                                                                       |
| securityGroupName                                          | The name of the security group attached to the cluster's subnet                                                                                                                                                   |                                                                                                                                       |
| securityGroupResourceGroup                                 | The name of the resource group that the security group is deployed in                                                                                                                                             |                                                                                                                                       |
| routeTableName                                             | The name of the route table attached to the subnet that the cluster is deployed in                                                                                                                                | Optional in 1.6                                                                                                                       |
| primaryAvailabilitySetName[*](#primaryavailabilitysetname) | The name of the availability set that should be used as the load balancer backend                                                                                                                                 | Optional                                                                                                                              |
| vmType                                                     | The type of azure nodes. Candidate values are: `vmss`, `vmssflex` and `standard`                                                                                                                                  | Optional, default to `standard` for v1.27 and below versions and `vmss` for v1.28+ versions                                           |
| primaryScaleSetName[*](#primaryscalesetname)               | The name of the scale set that should be used as the load balancer backend                                                                                                                                        | Optional                                                                                                                              |
| cloudProviderBackoff                                       | Enable exponential backoff to manage resource request retries                                                                                                                                                     | Boolean value, default to false                                                                                                       |
| cloudProviderBackoffRetries                                | Backoff retry limit                                                                                                                                                                                               | Integer value, valid if `cloudProviderBackoff` is true                                                                                |
| cloudProviderBackoffExponent                               | Backoff exponent                                                                                                                                                                                                  | Float value, valid if `cloudProviderBackoff` is true                                                                                  |
| cloudProviderBackoffDuration                               | Backoff duration                                                                                                                                                                                                  | Integer value, valid if `cloudProviderBackoff` is true                                                                                |
| cloudProviderBackoffJitter                                 | Backoff jitter                                                                                                                                                                                                    | Float value, valid if `cloudProviderBackoff` is true                                                                                  |
| cloudProviderBackoffMode                                   | Backoff mode, supported values are "v2" and "default". Note that "v2" has been deprecated since v1.18.0.                                                                                                          | Default to "default"                                                                                                                  |
| cloudProviderRateLimit                                     | Enable rate limiting                                                                                                                                                                                              | Boolean value, default to false                                                                                                       |
| cloudProviderRateLimitQPS                                  | Rate limit QPS (Read)                                                                                                                                                                                             | Float value, valid if `cloudProviderRateLimit` is true                                                                                |
| cloudProviderRateLimitBucket                               | Rate limit Bucket Size                                                                                                                                                                                            | Integar value, valid if `cloudProviderRateLimit` is true                                                                              |
| cloudProviderRateLimitQPSWrite                             | Rate limit QPS (Write)                                                                                                                                                                                            | Float value, valid if `cloudProviderRateLimit` is true                                                                                |
| cloudProviderRateLimitBucketWrite                          | Rate limit Bucket Size                                                                                                                                                                                            | Integer value, valid if `cloudProviderRateLimit` is true                                                                              |
| useInstanceMetadata                                        | Use instance metadata service where possible                                                                                                                                                                      | Boolean value, default to false                                                                                                       |
| loadBalancerSku                                            | Sku of Load Balancer and Public IP. Candidate values are: `basic` and `standard`.                                                                                                                                 | Default to `basic`.                                                                                                                   |
| excludeMasterFromStandardLB                                | ExcludeMasterFromStandardLB excludes master nodes from standard load balancer.                                                                                                                                    | Boolean value, default to true.                                                                                                       |
| disableOutboundSNAT                                        | Disable outbound SNAT for SLB                                                                                                                                                                                     | Default to false and available since v1.11.9, v1.12.7, v1.13.5 and v1.14.0                                                            |
| maximumLoadBalancerRuleCount                               | Maximum allowed LoadBalancer Rule Count is the limit enforced by Azure Load balancer                                                                                                                              | Integer value, default to [148](https://github.com/kubernetes/kubernetes/blob/v1.10.0/pkg/cloudprovider/providers/azure/azure.go#L48) |
| routeTableResourceGroup                                    | The resource group name for routeTable                                                                                                                                                                            | Default same as resourceGroup and available since v1.15.0                                                                             |
| loadBalancerName                                           | Working together with loadBalancerResourceGroup to determine the LB name in a different resource group                                                                                                            | Since v1.18.0, default is cluster name setting on kube-controller-manager                                                             |
| loadBalancerResourceGroup                                  | The load balancer resource group name, which is different from node resource group                                                                                                                                | Since v1.18.0, default is same as resourceGroup                                                                                       |
| disableAvailabilitySetNodes                                | Disable supporting for AvailabilitySet virtual machines in vmss cluster. It should only be used when vmType is "vmss" and all the nodes (including master) are VMSS Uniform virtual machines                      | Since v1.18.0, default is false                                                                                                       |
| enableVmssFlexNodes                                        | Enable supporting for VMSS Flex virtual machines in vmss cluster. It should only be used when vmType is "vmss"                                                                                                    | Since v1.26.0, default is false                                                                                                       |
| availabilitySetsCacheTTLInSeconds                      | Cache TTL in seconds for availabilitySet Nodes                                                                                                                                                                    | Since v1.18.0, default is 600                                                                                                         |
| vmssCacheTTLInSeconds                                      | Cache TTL in seconds for VMSS                                                                                                                                                                                     | Since v1.18.0, default is 600                                                                                                         |
| vmssVirtualMachinesCacheTTLInSeconds                       | Cache TTL in seconds for VMSS virtual machines                                                                                                                                                                    | Since v1.18.0, default is 600                                                                                                         |
| vmCacheTTLInSeconds                                        | Cache TTL in seconds for virtual machines                                                                                                                                                                         | Since v1.18.0, default is 60                                                                                                          |
| loadBalancerCacheTTLInSeconds                              | Cache TTL in seconds for load balancers                                                                                                                                                                           | Since v1.18.0, default is 120                                                                                                         |
| nsgCacheTTLInSeconds                                       | Cache TTL in seconds for network security group                                                                                                                                                                   | Since v1.18.0, default is 120                                                                                                         |
| routeTableCacheTTLInSeconds                                | Cache TTL in seconds for route table                                                                                                                                                                              | Since v1.18.0, default is 120                                                                                                         |
| disableAzureStackCloud                                     | DisableAzureStackCloud disables AzureStackCloud support. Set to true if you are using AZURESTACKCLOUD environment variable to configure custom endpoints but not actually running on AzureStack. Default is false. |
| tags                                                       | Tags that would be tagged onto the cloud provider managed resources, including lb, public IP, network security group and route table.                                                                             | Optional. Supported since v1.20.0.                                                                                                    |
| tagsMap                                                    | JSON-style tags, will be merged with `tags`                                                                                                                                                                       | Optional. Supported since v1.23.0.                                                                                                    |
| systemTags                                                 | Tag keys that should not be deleted when being updated.                                                                                                                                                           | Optional. Supported since v1.21.0.                                                                                                    |
| loadBalancerBackendPoolConfigurationType                   | The type of the Load Balancer backend pool. Supported values are `nodeIPConfiguration` (default) and `nodeIP`                                                                                                     | Optional. Supported since v1.23.0                                                                                                     |
| putVMSSVMBatchSize                                         | The number of requests the client sends concurrently in a batch when putting the VMSS VMs. Anything smaller than or equal to 0 means to update VMSS VMs one by one in sequence.                                   | Optional. Supported since v1.24.0.                                                                                                    |
| enableMigrateToIPBasedBackendPoolAPI                       | Use the migration API to migrate from NIC-based to IP-based Load Balancer backend pools without downtime.                                                                                                         | Optional. Supported since v1.24.0.                                                                                                    |
| RouteUpdateIntervalInSeconds                               | Update interval for route updater, default to 30.                                                                                                                                                                 | Optional. Supported since v1.28.0                                                                                                     |
| LoadBalancerBackendPoolUpdateIntervalInSeconds             | Update interval for local service backend pool updater in multi-slb mode, default to 30.                                                                                                                          | Optional. Supported since v1.28.0                                                                                                     |
| multipleStandardLoadBalancerConfigurations                 | Configurations related to multiple standard load balancers. See [multi-slb documentation](../../topics/multislb) for details.                                                                                     | Optional. Supported since v1.28.0                                                                                                     |

### primaryAvailabilitySetName

If this is set, the Azure cloudprovider will only add nodes from that availability set to the load
balancer backend pool. If this is not set, and multiple agent pools (availability sets) are used, then
the cloudprovider will try to add all nodes to a single backend pool which is forbidden.
In other words, if you use multiple agent pools (availability sets), you MUST set this field.

### primaryScaleSetName

If this is set, the Azure cloudprovider will only add nodes from that scale set to the load
balancer backend pool. If this is not set, and multiple agent pools (scale sets) are used, then
the cloudprovider will try to add all nodes to a single backend pool which is forbidden when using Load Balancer Basic SKU.
In other words, if you use multiple agent pools (scale sets), and `loadBalancerSku` is set to `basic` you MUST set this field.

### excludeMasterFromStandardLB

Master nodes are not added to the backends of Azure Load Balancer (ALB) if `excludeMasterFromStandardLB` is set.

By default, if nodes are labeled with `node-role.kubernetes.io/master`, they would also be excluded from ALB. If you want to add the master nodes to ALB, `excludeMasterFromStandardLB` should be set to false and label `node-role.kubernetes.io/master` should be removed if it has already been applied.

### Dynamically reloading cloud controller manager

Since v1.21.0, Azure cloud provider supports reading the cloud config from Kubernetes secrets. The secret is a serialized version of `azure.json` file. When the secret is changed, the cloud controller manager will re-constructing itself without restarting the pod.

To enable this feature, set `--enable-dynamic-reloading=true` and configure the secret name, namespace and data key by `--cloud-config-secret-name`, `--cloud-config-secret-namespace` and `--cloud-config-key`. When initializing from secret, the `--cloud-config` should not be set.

> Note that the `--enable-dynamic-reloading` cannot be `false` if `--cloud-config` is empty. To build the cloud provider from classic config file, please explicitly specify the `--cloud-config` and do not set `--enable-dynamic-reloading=true`. In this manner, the cloud controller manager will not be updated when the config file is changed. You need to restart the pod to manually trigger the re-initialization.

Since Azure cloud provider would read Kubernetes secrets, the following RBAC should also be configured:

```yaml
---
apiVersion: rbac.authorization.k8s.io/v1beta1
kind: ClusterRole
metadata:
  labels:
    kubernetes.io/cluster-service: "true"
  name: system:azure-cloud-provider-secret-getter
rules:
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["azure-cloud-provider"]
  verbs:
  - get
---
apiVersion: rbac.authorization.k8s.io/v1beta1
kind: ClusterRoleBinding
metadata:
  labels:
    kubernetes.io/cluster-service: "true"
  name: system:azure-cloud-provider-secret-getter
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:azure-cloud-provider-secret-getter
subjects:
- kind: ServiceAccount
  name: azure-cloud-provider
  namespace: kube-system
```

It is also supported to build the cloud controller manager from the cloud config file and reload dynamically. To use this way, turn on `--enable-dynamic-reloading` and set `--cloud-config` to an non-empty value.

### per client rate limiting

Since v1.18.0, the original global rate limiting has been switched to per-client. A set of new rate limit configure options are introduced for each client, which includes:


- routeRateLimit
- SubnetsRateLimit
- InterfaceRateLimit
- RouteTableRateLimit
- LoadBalancerRateLimit
- PublicIPAddressRateLimit
- SecurityGroupRateLimit
- VirtualMachineRateLimit
- StorageAccountRateLimit
- DiskRateLimit
- SnapshotRateLimit
- VirtualMachineScaleSetRateLimit
- VirtualMachineSizeRateLimit
- AvailabilitySetRateLimit
- AttachDetachDiskRateLimit
- ContainerServiceRateLimit
- DeploymentRateLimit
- PrivateDNSRateLimit
- PrivateDNSZoneGroupRateLimit
- PrivateEndpointRateLimit
- PrivateLinkServiceRateLimit
- VirtualNetworkRateLimit

The original rate limiting options ("cloudProviderRateLimitBucket", "cloudProviderRateLimitBucketWrite", "cloudProviderRateLimitQPS", "cloudProviderRateLimitQPSWrite") are still supported, and they would be the default values if per-client rate limiting is not configured.

Here is an example of per-client config:

```json
{
  // default rate limit (enabled).
  "cloudProviderRatelimit": true,
  "cloudProviderRateLimitBucket": 1,
  "cloudProviderRateLimitBucketWrite": 1,
  "cloudProviderRateLimitQPS": 1,
  "cloudProviderRateLimitQPSWrite": 1,
  "virtualMachineScaleSetRateLimit": {  // VMSS specific (enabled).
    "cloudProviderRatelimit": true,
    "cloudProviderRateLimitBucket": 2,
    "CloudProviderRateLimitBucketWrite": 2,
    "cloudProviderRateLimitQPS": 0,
    "CloudProviderRateLimitQPSWrite": 0
  },
  "loadBalancerRateLimit": {  // LB specific (disabled)
    "cloudProviderRatelimit": false
  },
  ... // other cloud provider configs
}
```

## Run Kubelet without Azure identity

When running Kubelet with kube-controller-manager, it also supports running without Azure identity since v1.15.0.

Both kube-controller-manager and kubelet should configure `--cloud-provider=azure --cloud-config=/etc/kubernetes/cloud-config/azure.json`, but the contents for `azure.json` are different:

(1) For kube-controller-manager, refer the above part for setting `azure.json`.

(2) For kubelet, `useInstanceMetadata` is required to be `true` and Azure identities are not required. A sample for Kubelet's azure.json is

```json
{
  "useInstanceMetadata": true,
  "vmType": "vmss"
}
```

## Azure Stack Configuration

Azure Stack has different API endpoints, depending on the Azure Stack deployment. These need to be provided to the Azure SDK and currently this is done by adding an extra `json` file with the arguments, as well as an environment variable pointing to this file.

There are several available presets, namely:

- `AzureChinaCloud`
- `AzureGermanCloud`
- `AzurePublicCloud`
- `AzureUSGovernmentCloud`

These are determined using `cloud: <PRESET>` described above in the description of `azure.json`.

When `cloud: AzureStackCloud`, the extra environment variable used by the Azure SDK to find the Azure Stack configuration file is:

  - [`AZURE_ENVIRONMENT_FILEPATH`](https://github.com/Azure/go-autorest/blob/562d376/autorest/azure/environments.go#L28)

The configuration parameters of this file:

```json
{
  "name": "AzureStackCloud",
  "managementPortalURL": "...",
  "publishSettingsURL": "...",
  "serviceManagementEndpoint": "...",
  "resourceManagerEndpoint": "...",
  "activeDirectoryEndpoint": "...",
  "galleryEndpoint": "...",
  "keyVaultEndpoint": "...",
  "graphEndpoint": "...",
  "serviceBusEndpoint": "...",
  "batchManagementEndpoint": "...",
  "storageEndpointSuffix": "...",
  "sqlDatabaseDNSSuffix": "...",
  "trafficManagerDNSSuffix": "...",
  "keyVaultDNSSuffix": "...",
  "serviceBusEndpointSuffix": "...",
  "serviceManagementVMDNSSuffix": "...",
  "resourceManagerVMDNSSuffix": "...",
  "containerRegistryDNSSuffix": "...",
  "cosmosDBDNSSuffix": "...",
  "tokenAudience": "...",
  "resourceIdentifiers": {
    "graph": "...",
    "keyVault": "...",
    "datalake": "...",
    "batch": "...",
    "operationalInsights": "..."
  }
}
```

The full list of existing settings for the `AzureChinaCloud`, `AzureGermanCloud`, `AzurePublicCloud` and `AzureUSGovernmentCloud` is available in the source code at https://github.com/Azure/go-autorest/blob/master/autorest/azure/environments.go#L51.

## Host Network Resources in different AAD Tenant and Subscription

Since v1.18.0, Azure cloud provider supports hosting network resources (Virtual Network, Network Security Group, Route Table, Load Balancer and Public IP) in different AAD Tenant and Subscription than those for the cluster. To enable this feature, set `networkResourceTenantID` and `networkResourceSubscriptionID` in auth config. Note that the value of them need to be different than value of `tenantID` and `subscriptionID`.

With this feature enabled, network resources of the cluster will be created in `networkResourceSubscriptionID` in `networkResourceTenantID`, and rest resources of the cluster still remain in `subscriptionID` in `tenantID`. Properties which specify the resource groups of network resources are compatible with this feature. For example, Virtual Network will be created in `vnetResourceGroup` in `networkResourceSubscriptionID` in `networkResourceTenantID`.

For authentication methods, only Service Principal supports this feature, and `aadClientID` and `aadClientSecret` are used to authenticate with those two AAD Tenants and Subscriptions. Managed Identity and Client Certificate doesn't support this feature. Azure Stack doesn't support this feature.

## Current default rate-limiting values

The following are the default rate limiting values configured in [AKS](https://azure.microsoft.com/en-us/services/kubernetes-service/) and [cluster-api-provider-azure](https://github.com/kubernetes-sigs/cluster-api-provider-azure) clusters prior to Kubernetes version v1.18.0.

```json
    "cloudProviderBackoff": true,
    "cloudProviderBackoffRetries": 6,
    "cloudProviderBackoffDuration": 5,
    "cloudProviderRatelimit": true,
    "cloudProviderRateLimitQPS": 10,
    "cloudProviderRateLimitBucket": 100,
    "cloudProviderRatelimitQPSWrite": 10,
    "cloudProviderRatelimitBucketWrite": 100,
```

For v1.18.0+ refer to [per client rate limit config](#per-client-rate-limiting)
