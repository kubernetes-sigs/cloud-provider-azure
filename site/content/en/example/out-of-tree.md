---
title: "Deploy with Out-of-tree Cloud Provider Azure"
linkTitle: "Out-of-tree"
type: docs
weight: 2
description: >
    Deploy a cluster with Out-of-tree Cloud Provider Azure.
---

[cluster-api-provider-azure](https://github.com/kubernetes-sigs/cluster-api-provider-azure) can be used to
provision a Kubernetes cluster with out-of-tree cloud-provider-azure, including specific cloud-controller-manager
and cloud-node-manager images.

```sh
export AZURE_SUBSCRIPTION_ID=<subscription-id>
export AZURE_TENANT_ID=<tenant-id>
export AZURE_CLIENT_ID=<client-id>
export AZURE_CLIENT_SECRET=<client-secret>
export CLUSTER_NAME=<cluster-name>
export AZURE_RESOURCE_GROUP=<resource-group>
export AZURE_CLOUD_CONTROLLER_MANAGER_IMG=<cloud-controller-manager-image>
export AZURE_CLOUD_NODE_MANAGER_IMG=<cloud-node-manager-image>

make deploy-cluster
```