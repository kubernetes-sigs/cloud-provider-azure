---
title: "Deploy with Out-of-tree Cloud Provider Azure"
linkTitle: "Out-of-tree"
type: docs
weight: 2
description: >
    Deploy a cluster with Out-of-tree Cloud Provider Azure.
---

The AKS-engine supports deploying clusters with customized `Cloud Controller Manager` (CCM) and `Cloud Node Manager` (CNM) images. The API model is defined [here](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/aks-engine.json). Follow [this guide](../../development/custom-images) to build your CCM and CNM images and fill them into the AKS-engine API model.

To manually deploy an out-of-tree cluster, we need to deploy the following manifests. Note that there are some restrictions when setting config flags. To get more infomation, checkout [this doc](../../install/azure-ccm).

[`cloud-controller-manager`](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/out-of-tree/cloud-controller-manager.yaml)

[`cloud-node-manager`](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/out-of-tree/cloud-node-manager.yaml)

[`kube-apiserver`](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/out-of-tree/kube-apiserver.yaml)

[`kube-controller-manager`](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/out-of-tree/kube-controller-manager.yaml)

[`kube-shedular`](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/out-of-tree/kube-scheduler.yaml)

[`kubelet` command](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/out-of-tree/kubelet.manifest)
