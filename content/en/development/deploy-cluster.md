---
title: "Deploy clusters"
linkTitle: "Deploy clusters"
type: docs
weight: 4
description: >
  Deploy Kubernetes clusters
---

## Cluster API Provider Azure (CAPZ)

A management cluster is needed to deploy workload clusters. Please follow the [instructions](https://capz.sigs.k8s.io/topics/workload-identity#set-up-a-management-cluster-with-kind).

After the management cluster is provisioned, please run the following command in the root directory of the repo.

```shell
make deploy-workload-cluster
```

Customizations are supported by environment variables:

| Environment variables                   | required | description                                                                        | default                                                                 |
|-----------------------------------------|----------|------------------------------------------------------------------------------------|-------------------------------------------------------------------------|
| AZURE_SUBSCRIPTION_ID                       | true     | subscription ID                                                                    |                                                                         |
| AZURE_TENANT_ID                             | true     | tenant ID                                                                          |                                                                         |
| AZURE_CLIENT_ID                             | true     | client ID with permission                                                          |                                                                         |
| CLUSTER_NAME                                | true     | name of the cluster                                                                |                                                                         |
| AZURE_RESOURCE_GROUP                        | true     | name of the resource group to be deployed (auto generated if not existed)          |                                                                         |
| MANAGEMENT_CLUSTER_NAME                     | true     | name of the management cluster                                                     |                                                                         |
| KIND                                        | false     | whether the management cluster is provisioned by kind                             | true                                                                    |
| WORKLOAD_CLUSTER_TEMPLATE                   | false    | path to the cluster-api template                                                   | tests/k8s-azure-manifest/cluster-api/vmss-multi-nodepool.yaml           |
| CUSTOMIZED_CLOUD_CONFIG_TEMPLATE            | false    | customized cloud provider configs                                                  |                                                                         |
| AZURE_CLUSTER_IDENTITY_SECRET_NAME          | false    | name of the cluster identity secret                                                | cluster-identity-secret                                                 |
| AZURE_CLUSTER_IDENTITY_SECRET_NAMESPACE     | false    | namespace of the cluster identity secret                                           | default                                                                 |
| CLUSTER_IDENTITY_NAME                       | false    | name of the AzureClusterIdentity CRD                                               | cluster-identity                                                        |
| CONTROL_PLANE_MACHINE_COUNT                 | false    | number of the control plane nodes                                                  | 1                                                                       |
| WORKER_MACHINE_COUNT                        | false    | number of the worker nodes                                                         | 2                                                                       |
| AZURE_CONTROL_PLANE_MACHINE_TYPE            | false    | VM SKU of the control plane nodes                                                  | Standard_D4s_v3                                                         |
| AZURE_NODE_MACHINE_TYPE                     | false    | VM SKU of the worker nodes                                                         | Standard_D2s_v3                                                         |
| AZURE_LOCATION                              | false    | region of the cluster resources                                                    | westus2                                                                 |
| AZURE_CLOUD_CONTROLLER_MANAGER_IMG_REGISTRY | false    | image registry of the cloud-controller-manager                                     | mcr.microsoft.com/oss/kubernetes                                        |
| AZURE_CLOUD_NODE_MANAGER_IMG_REGISTRY       | false    | image registry of the cloud-node-manager                                           | mcr.microsoft.com/oss/kubernetes                                        |
| AZURE_CLOUD_CONTROLLER_MANAGER_IMG_NAME     | false    | image name of the cloud-controller-manager                                         | azure-cloud-controller-manager                                          |
| AZURE_CLOUD_NODE_MANAGER_IMG_NAME           | false    | image name of the cloud-node-manager                                               | azure-cloud-node-manager                                                |
| AZURE_CLOUD_CONTROLLER_MANAGER_IMG_TAG      | false    | image tag of the cloud-controller-manager                                          | v1.28.4                                                                 |
| AZURE_CLOUD_NODE_MANAGER_IMG_TAG            | false    | image tag of the cloud-node-manager                                                | v1.28.4                                                                 |
| KUBERNETES_VERSION                          | false    | Kubernetes components version                                                          | v1.28.0                                                             |
| AZURE_LOADBALANCER_SKU                      | false    | LoadBalancer SKU, Standard or Basic                                                | Standard                                                                |
| LB_BACKEND_POOL_CONFIG_TYPE                 | false    | LoadBalancer backend pool configuration type, nodeIPConfiguration, nodeIP or podIP | nodeIPConfiguration                                                     |
| PUT_VMSS_VM_BATCH_SIZE                      | false    | Batch size when updating VMSS VM concurrently                                      | 0                                                                       |
| AZURE_SSH_PUBLIC_KEY                        | false    | SSH public key to connecet to the VMs                                              | ""                                                                      |

To completely remove the cluster, specify the "${CLUSTER_NAME}" and run:

```shell
make delete-workload-cluster
```

