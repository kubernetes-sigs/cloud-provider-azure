# Cloud provider config

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
    "vnetName": "<name>",
    "vnetResourceGroup": "",
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
|loadBalancerResourceGroup|Resource group name of the load balancer user want to use, default value is the name of cluster's resource group|String value of rg's name, optional|
|loadBalancerName|The name of the load balancer user want to use. If not set, default naming pattern is used.|String value, optional|
|excludeMasterFromStandardLB|ExcludeMasterFromStandardLB excludes master nodes from standard load balancer.|Boolean value, default to true.|
|disableOutboundSNAT| Disable outbound SNAT for SLB | Default to false and available since v1.11.9, v1.12.7, v1.13.5 and v1.14.0|
|maximumLoadBalancerRuleCount|Maximum allowed LoadBalancer Rule Count is the limit enforced by Azure Load balancer|Integer value, default to [148](https://github.com/kubernetes/kubernetes/blob/v1.10.0/pkg/cloudprovider/providers/azure/azure.go#L48)|
|routeTableResourceGroup| The resource group name for routeTable | Default same as resourceGroup and available since v1.15.0 |
|cloudConfigType| The cloud configure type for Azure cloud provider. Supported values are file, secret and merge.| Default to `merge`.  and available since v1.15.0 |

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

### Setting Azure cloud provider from Kubernetes secrets

Since v1.15.0, Azure cloud provider supports reading the cloud config from Kubernetes secrets. The secret is a serialized version of `azure.json` file with key `cloud-config`. The secret should be put in `kube-system` namespace and its name should be `azure-cloud-provider`.

To enable this feature, set `cloudConfigType` to `secret` or `merge` (default is `merge`). All supported values for this option are:

- `file`: The cloud provider configuration is read from cloud-config file.
- `secret`: the cloud provider configuration must be overridden by the secret.
- `merge`: the cloud provider configuration can be optionally overridden by a secret when it is set explicitly in the secret, this is default value.

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

## Run Kubelet without Azure identity

When running Kubelet with kube-controller-manager, it also supports running without Azure identity since v1.15.0.

Both kube-controller-manager and kubelet should configure `--cloud-provider=azure --cloud-config=/etc/kubernetes/azure.json`, but the contents for `azure.json` are different:

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

The full list of existing settings for the `AzureChinaCloud`, `AzureGermanCloud`, `AzurePublicCloud` and `AzureUSGovernmentCloud` is available in the source code at https://github.com/Azure/go-autorest/blob/master/autorest/azure/environments.go#L51
