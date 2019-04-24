# Cloud provider config

This doc describes cloud provider config file, which is to be used via `--cloud-config` flag of azure-cloud-controller-manager.

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
    "vnetName": "<name>",
    "vnetResourceGroup": "",
    "routeTableName": "<name>",
    "primaryAvailabilitySetName": "<name>",
    "cloudProviderBackoff": false,
    "useManagedIdentityExtension": false,
    "useInstanceMetadata": true
}
```

Note: All values are of string type if not explicitly called out.

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

Note: Cloud provider currently supports three authentication methods, you can choose one combination of them:

- [Managed Identity](https://docs.microsoft.com/en-us/azure/active-directory/managed-service-identity/overview):
  - For system-assigned managed identity: set `useManagedIdentityExtension` to true
  - For user-assigned managed identity: set `useManagedIdentityExtension` to true and also set `userAssignedIdentityID`
- [Service Principal](https://github.com/Azure/aks-engine/blob/master/docs/topics/service-principals.md): set `aadClientID` and `aadClientSecret`
- [Client Certificate](https://docs.microsoft.com/en-us/azure/active-directory/develop/active-directory-protocols-oauth-service-to-service): set `aadClientCertPath` and `aadClientCertPassword`

If more than one value is set, the order is `Managed Identity` > `Service Principal` > `Client Certificate`.

## Cluster config

|Name|Description|Remark|
|---|---|---|
|resourceGroup|The name of the resource group that the cluster is deployed in||
|location|The location of the resource group that the cluster is deployed in||
|vnetName|The name of the VNet that the cluster is deployed in||
|vnetResourceGroup|The name of the resource group that the Vnet is deployed in||
|subnetName|The name of the subnet that the cluster is deployed in||
|securityGroupName|The name of the security group attached to the cluster's subnet||
|routeTableName|The name of the route table attached to the subnet that the cluster is deployed in|Optional in 1.6|
|primaryAvailabilitySetName[*](#primaryavailabilitysetname)|The name of the availability set that should be used as the load balancer backend|Optional|
|vmType|The type of azure nodes. Candidate values are: `vmss` and `standard`|Optional, default to `standard`|
|primaryScaleSetName[*](#primaryscalesetname)|The name of the scale set that should be used as the load balancer backend|Optional|
|cloudProviderBackoff|Enable exponential backoff to manage resource request retries|Boolean value, default to false|
|cloudProviderBackoffRetries|Backoff retry limit|Integer value, valid if `cloudProviderBackoff` is true|
|cloudProviderBackoffExponent|Backoff exponent|Float value, valid if `cloudProviderBackoff` is true|
|cloudProviderBackoffDuration|Backoff duration|Integer value, valid if `cloudProviderBackoff` is true|
|cloudProviderBackoffJitter|Backoff jitter|Float value, valid if `cloudProviderBackoff` is true|
|cloudProviderBackoffMode|Backoff mode, supported values are "v2" and "default"|Default to "default"|
|cloudProviderRateLimit|Enable rate limiting|Boolean value, default to false|
|cloudProviderRateLimitQPS|Rate limit QPS (Read)|Float value, valid if `cloudProviderRateLimit` is true|
|cloudProviderRateLimitBucket|Rate limit Bucket Size|Integar value, valid if `cloudProviderRateLimit` is true|
|cloudProviderRateLimitQPSWrite|Rate limit QPS (Write)|Float value, valid if `cloudProviderRateLimit` is true|
|cloudProviderRateLimitBucketWrite|Rate limit Bucket Size|Integer value, valid if `cloudProviderRateLimit` is true|
|useInstanceMetadata|Use instance metadata service where possible|Boolean value, default to false|
|loadBalancerSku|Sku of Load Balancer and Public IP. Candidate values are: `basic` and `standard`.|Default to `basic`.|
|excludeMasterFromStandardLB|ExcludeMasterFromStandardLB excludes master nodes from standard load balancer.|Boolean value, default to true.|
|maximumLoadBalancerRuleCount|Maximum allowed LoadBalancer Rule Count is the limit enforced by Azure Load balancer|Integer value, default to [148](https://github.com/kubernetes/kubernetes/blob/v1.10.0/pkg/cloudprovider/providers/azure/azure.go#L48)|

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

Master nodes would not add to the backends of Azure loadbalancer (ALB) if `excludeMasterFromStandardLB` is set.

By default, if nodes are labeled with `node-role.kubernetes.io/master`, they would also be excluded from ALB. If you want adding the master nodes to ALB, `excludeMasterFromStandardLB` should be set to false and label `node-role.kubernetes.io/master` should be removed if it has already been applied.

# Azure Stack Configuration

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

```
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

The full list of existing settings for the `AzureChinaCloud`, `AzureGermanCloud`, `AzurePublicCloud` and `AzureUSGovernmentCloud` is available in the source code at https://github.com/Azure/go-autorest/blob/master/autorest/azure/environments.go#L51
