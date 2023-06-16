---
title: "Deploy with In-tree Cloud Provider Azure"
linkTitle: "In-tree"
type: docs
weight: 1
description: >
    Deploy a cluster with In-tree Cloud Provider Azure.
---

[cluster-api-provider-azure](https://github.com/kubernetes-sigs/cluster-api-provider-azure) can be used to
provision a Kubernetes cluster with in-tree cloud-provider-azure.

```sh
export AZURE_SUBSCRIPTION_ID=<subscription-id>
export AZURE_TENANT_ID=<tenant-id>
export AZURE_CLIENT_ID=<client-id>
export AZURE_CLIENT_SECRET=<client-secret>
export CLUSTER_NAME=<cluster-name>
export AZURE_RESOURCE_GROUP=<resource-group>
export USE_IN_TREE_CLOUD_PROVIDER=true

make deploy-cluster
```
