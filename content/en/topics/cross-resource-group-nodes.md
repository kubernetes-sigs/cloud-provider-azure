---
title: "Deploy Cross Resource Group Nodes"
linkTitle: "Cross Resource Group Nodes"
weight: 5
type: docs
description: >
  Deploy cross resource group nodes.
---

**Feature status:** GA since v1.21.

Kubernetes v1.21 adds support for cross resource group (RG) nodes and unmanaged (such as on-prem) nodes in Azure cloud provider. A few assumptions are made for such nodes:

- Cross-RG nodes are in same region and set with required labels (as clarified in the following part)
- Nodes will not be part of the load balancer managed by cloud provider
- Both node and container networking should be configured properly by provisioning tools
- AzureDisk is supported for Azure cross-RG nodes, but not for on-prem nodes

## Pre-requirements

Because cross-RG nodes and unmanaged nodes won't be added to Azure load balancer backends, feature gate `ServiceNodeExclusion` should be enabled for master components (`ServiceNodeExclusion` has been GA and enabled by default since v1.21).

## Cross-RG nodes

Cross-RG nodes should register themselves with required labels together with cloud provider:

- `node.kubernetes.io/exclude-from-external-load-balancers`, which is used to exclude the node from load balancer.
  - `alpha.service-controller.kubernetes.io/exclude-balancer=true` should be used if the cluster version is below v1.16.0.
- `kubernetes.azure.com/resource-group=<rg-name>`, which provides external RG and is used to get node information.
- cloud provider config
  - `--cloud-provider=azure` when using kube-controller-manager
  - `--cloud-provider=external` when using cloud-controller-manager

For example,

```shell script
kubelet ... \
  --cloud-provider=azure \
  --cloud-config=/etc/kubernetes/cloud-config/azure.json \
  --node-labels=node.kubernetes.io/exclude-from-external-load-balancers=true,kubernetes.azure.com/resource-group=<rg-name>
```

## Unmanaged nodes

On-prem nodes are different from Azure nodes, all Azure coupled features (such as load balancers and Azure managed disks) are not supported for them. To prevent the node being deleted, Azure cloud provider will always assumes the node existing.

On-prem nodes should register themselves with labels `node.kubernetes.io/exclude-from-external-load-balancers=true` and `kubernetes.azure.com/managed=false`:

- `node.kubernetes.io/exclude-from-external-load-balancers=true`, which is used to exclude the node from load balancer.
- `kubernetes.azure.com/managed=false`, which indicates the node is on-prem or on other clouds.

For example,

```shell script
kubelet ...\
  --cloud-provider= \
  --node-labels=node.kubernetes.io/exclude-from-external-load-balancers=true,kubernetes.azure.com/managed=false
```

## Limitations

Cross resource group nodes and unmanaged nodes are unsupported when joined to an AKS cluster. Using these labels on AKS-managed nodes is not supported.

## Reference

See design docs for cross resource group nodes in [KEP 20180809-cross-resource-group-nodes](https://github.com/kubernetes/enhancements/tree/master/keps/sig-cloud-provider/azure/604-cross-resource-group-nodes).
