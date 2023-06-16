---
title: FAQ
linkTitle: FAQ
type: docs
menu:
  main:
    weight: 40
---

## What is Cloud Provider Azure?

A Kubernetes `Cloud Provider` consists of two parts: a provider-specified `cloud-controller-manager` (or `kube-controller-manager` for in-tree version) and a provider-specified implementation of Kubernetes [cloud provider interface](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/cloud-provider/cloud.go). Currently, the Azure `cloud-controller-manager` is outside of [Kubernetes repo](https://github.com/kubernetes/kubernetes) and the cloud provider interface implementation is in `pkg/provider`. 

The `cloud-controller-manager` is a Kubernetes [control plane](https://kubernetes.io/docs/reference/glossary/?all=true#term-control-plane) component which embeds cloud-specific control logic. It lets you link your cluster into your cloud provider's API, and separates out the components that interact with that cloud platform from components that just interact with your cluster.

By decoupling the interoperability logic between Kubernetes and the underlying cloud infrastructure, the `cloud-controller-manager` component enables cloud providers to release features at a different pace compared to the main Kubernetes project.

## What is the difference between in-tree and out-of-tree cloud provider? 

In-tree cloud providers are the providers we develop & release in the [main Kubernetes repository](https://github.com/kubernetes/kubernetes/tree/master/pkg/cloudprovider/providers). This results in embedding the knowledge and context of each cloud provider into most of the Kubernetes components. This enables more native integrations such as the kubelet requesting information about itself via a metadata service from the cloud provider.

Out-of-tree cloud providers are providers that can be developed, built, and released independent of Kubernetes core. This requires deploying a new component called the cloud-controller-manager which is responsible for running all the cloud specific controllers that were previously run in the kube-controller-manager.

## Which one is recommended? 

We recommend using the in-tree cloud provider at this time because it's out-of-tree counterpart is not 100% ready. However, out-of-tree cloud provider will become the No.1 pick in the near future.
