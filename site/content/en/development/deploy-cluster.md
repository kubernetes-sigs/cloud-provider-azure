---
title: "Deploy clusters"
linkTitle: "Deploy clusters"
type: docs
weight: 4
description: >
  Deploy Kubernetes clusters
---

## Cluster API Provider Azure (CAPZ)

Please run the following command in the root directory of the repo.

```shell
make deploy-cluster
```

Customizations are supported by environment variables:

| Environment variables                   | required | description                                                                        | default                                                                 |
|-----------------------------------------|----------|------------------------------------------------------------------------------------|-------------------------------------------------------------------------|
| AZURE_SUBSCRIPTION_ID                   | true     | subscription ID                                                                    |                                                                         |
| AZURE_TENANT_ID                         | true     | tenant ID                                                                          |                                                                         |
| AZURE_CLIENT_ID                         | true     | client ID with permission                                                          |                                                                         |
| AZURE_CLIENT_SECRET                     | true     | client secret                                                                      |                                                                         |
| CLUSTER_NAME                            | true     | name of the cluster                                                                |                                                                         |
| AZURE_RESOURCE_GROUP                    | true     | name of the resource group to be deployed (auto generated if not existed)          |                                                                         |
| MANAGEMENT_CLUSTER_NAME                 | false    | name of the kind management cluster                                                | capi                                                                    |
| WORKLOAD_CLUSTER_TEMPLATE               | false    | path to the cluster-api template                                                   | tests/k8s-azure-manifest/cluster-api/vmss-multi-nodepool.yaml           |
| CUSTOMIZED_CLOUD_CONFIG_TEMPLATE        | false    | customized cloud provider configs                                                  |                                                                         |
| AZURE_CLUSTER_IDENTITY_SECRET_NAME      | false    | name of the cluster identity secret                                                | cluster-identity-secret                                                 |
| AZURE_CLUSTER_IDENTITY_SECRET_NAMESPACE | false    | namespace of the cluster identity secret                                           | default                                                                 |
| CLUSTER_IDENTITY_NAME                   | false    | name of the AzureClusterIdentity CRD                                               | cluster-identity                                                        |
| CONTROL_PLANE_MACHINE_COUNT             | false    | number of the control plane nodes                                                  | 1                                                                       |
| WORKER_MACHINE_COUNT                    | false    | number of the worker nodes                                                         | 2                                                                       |
| AZURE_CONTROL_PLANE_MACHINE_TYPE        | false    | VM SKU of the control plane nodes                                                  | Standard_D4s_v3                                                         |
| AZURE_NODE_MACHINE_TYPE                 | false    | VM SKU of the worker nodes                                                         | Standard_D2s_v3                                                         |
| AZURE_LOCATION                          | false    | region of the cluster resources                                                    | westus2                                                                 |
| AZURE_CLOUD_CONTROLLER_MANAGER_IMG      | false    | image of the cloud-controller-manager                                              | mcr.microsoft.com/oss/kubernetes/azure-cloud-controller-manager:v1.23.1 |
| AZURE_CLOUD_NODE_MANAGER_IMG            | false    | image of the cloud-node-manager                                                    | mcr.microsoft.com/oss/kubernetes/azure-cloud-node-manager:v1.23.1       |
| KUBERNETES_VERSION                      | false    | Kubernetes components version                                                      | v1.25.0                                                                 |
| AZURE_LOADBALANCER_SKU                  | false    | LoadBalancer SKU, Standard or Basic                                                | Standard                                                                |
| ENABLE_MULTI_SLB                        | false    | Enable multiple standard LoadBalancers per cluster                                 | false                                                                   |
| LB_BACKEND_POOL_CONFIG_TYPE             | false    | LoadBalancer backend pool configuration type, nodeIPConfiguration, nodeIP or podIP | nodeIPConfiguration                                                     |
| PUT_VMSS_VM_BATCH_SIZE                  | false    | Batch size when updating VMSS VM concurrently                                      | 0                                                                       |
| AZURE_SSH_PUBLIC_KEY                    | false    | SSH public key to connecet to the VMs                                              | ""                                                                      |

