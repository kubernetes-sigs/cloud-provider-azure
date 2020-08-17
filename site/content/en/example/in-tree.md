---
title: "Deploy with In-tree Cloud Provider Azure"
linkTitle: "In-tree"
type: docs
weight: 1
description: >
    Deploy a cluster with In-tree Cloud Provider Azure.
---

To deploy an In-tree Cloud Provider Azure, all you need to do is deploy a cluster using AKS-Engine with the API model defined [here](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/az.json). The AKS-Engine will automatically deploy the Kubernetes components needed and you don't have to deploy them manually. However, customization is possible by modifying the manifests in `/etc/kubernetes` on master node. Here are the examples:

[`kube-apiserver`](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/in-tree/kube-apiserver.yaml)

[`kube-controller-manager`](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/in-tree/kube-controller-manager.yaml)

[`kube-scheduler`](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/in-tree/kube-scheduler.yaml)

To customize `kubelet`, you need to modify the starting command like [here](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/examples/in-tree/kubelet.manifest).
