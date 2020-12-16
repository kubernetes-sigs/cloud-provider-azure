---
title: "Deploy Cloud Controller Manager"
linkTitle: "Azure Cloud Controller Manager"
type: docs
weight: 2
description: >
    The configurations for using Azure Cloud Controller Manager.
---

`azure-cloud-controller-manager` is a Kubernetes component which provides interoperability with Azure API, and will be used by Kubernetes clusters running on Azure. It runs together with other components to provide the Kubernetes cluster’s control plane.

Using [cloud-controller-manager](https://kubernetes.io/docs/concepts/overview/components/#cloud-controller-manager) is a new alpha feature for Kubernetes since v1.14. `cloud-controller-manager` runs cloud provider related controller loops, which used to be run by `controller-manager`.

`azure-cloud-controller-manager` is a specialization of `cloud-controller-manager`. It depends on [cloud-controller-manager app](https://github.com/kubernetes/kubernetes/tree/master/cmd/cloud-controller-manager/app) and [azure cloud provider](https://github.com/kubernetes-sigs/cloud-provider-azure/tree/master/pkg/provider).

## Usage

To use cloud controller manager, the following components need to be configured:

1. kubelet

    |Flag|Value|Remark|
    |----|-----|------|
    |--cloud-provider|external|cloud-provider should be set external|
    |--azure-container-registry-config|/etc/kubernetes/azure.json|Used for Azure credential provider|

1. kube-controller-manager

    |Flag|Value|Remark|
    |---|---|---|
    |--cloud-provider|external|cloud-provider should be set external|
    |--external-cloud-volume-plugin|azure|Optional*|

    `*` Since cloud controller manager does not support volume controllers, it will not provide volume capabilities compared to using previous built-in cloud provider case. You can add this flag to turn on volume controller for in-tree cloud providers. This option is likely to be [removed with in-tree cloud providers](https://github.com/kubernetes/kubernetes/blob/v1.11.0-alpha.2/cmd/kube-controller-manager/app/options/options.go#L93) in future.

1. kube-apiserver

    Do not set flag `--cloud-provider`

1. azure-cloud-controller-manager

    Set following flags:

    |Flag|Value|Remark|
    |---|---|---|
    |--cloud-provider|azure|cloud-provider should be set azure|
    |--cloud-config|/etc/kubernetes/azure.json|Path for [cloud provider config](../configs)|

    For other flags such as `--allocate-node-cidrs`, `--configure-cloud-routes`, `--cluster-cidr`, they are moved from kube-controller-manager. If you are migrating from kube-controller-manager, they should be set to same value.

    For details of those flags, please refer to this [doc](https://kubernetes.io/docs/reference/command-line-tools-reference/cloud-controller-manager/).

Alternatively, you can use [aks-engine](https://github.com/Azure/aks-engine) to deploy a Kubernetes cluster running with cloud-controller-manager. It supports deploying `Kubernetes azure-cloud-controller-manager` for Kubernetes v1.16+.

## AzureDisk and AzureFile

AzureDisk and AzureFile volume plugins are not supported with external coud provider (See [kubernetes/kubernetes#71018](https://github.com/kubernetes/kubernetes/issues/71018) for explanations).

Hence, [azuredisk-csi-driver](https://github.com/kubernetes-sigs/azuredisk-csi-driver) and [azurefile-csi-driver](https://github.com/kubernetes-sigs/azurefile-csi-driver) should be used for persistent volumes.

### Deploy AzureDisk CSI plugin

Run following commands:

```sh
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/deploy/csi-azuredisk-driver.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/deploy/rbac-csi-azuredisk-controller.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/deploy/rbac-csi-azuredisk-node.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/deploy/csi-azuredisk-controller.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/deploy/csi-azuredisk-node.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/deploy/csi-azuredisk-node-windows.yaml

# skip below yaml configurations if snapshot feature(only available from v1.17.0) will not be used
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/deploy/crd-csi-snapshot.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/deploy/rbac-csi-snapshot-controller.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azuredisk-csi-driver/master/deploy/csi-snapshot-controller.yaml
```

See [azuredisk-csi-driver](https://github.com/kubernetes-sigs/azuredisk-csi-driver) for more details.

### Deploy AzureFile CSI plugin

Run following commands:

```sh
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azurefile-csi-driver/master/deploy/rbac-csi-azurefile-controller.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azurefile-csi-driver/master/deploy/rbac-csi-azurefile-node.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azurefile-csi-driver/master/deploy/csi-azurefile-controller.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azurefile-csi-driver/master/deploy/csi-azurefile-driver.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azurefile-csi-driver/master/deploy/csi-azurefile-node.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-sigs/azurefile-csi-driver/master/deploy/csi-azurefile-node-windows.yaml
```

See [azurefile-csi-driver](https://github.com/kubernetes-sigs/azurefile-csi-driver) for more details.

### Change default storage class

Follow the steps bellow if you want change the current default storage class to AzureDisk CSI driver.

First, delete the default storage class:

```sh
kubectl delete storageclass default
```

Then create a new storage class named `default`:

```sh
cat <<EOF | kubectl apply -f-
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  annotations:
    storageclass.beta.kubernetes.io/is-default-class: "true"
  name: default
provisioner: disk.csi.azure.com
parameters:
  skuname: Standard_LRS # available values: Standard_LRS, Premium_LRS, StandardSSD_LRS and UltraSSD_LRS
  kind: managed         # value "dedicated", "shared" are deprecated since it's using unmanaged disk
  cachingMode: ReadOnly
reclaimPolicy: Delete
volumeBindingMode: Immediate
EOF
```

## Development

Build project:

```sh
make
```

Build image:

```sh
IMAGE_REGISTRY=<registry> make image
```

Run unit tests:

```sh
make test-unit
```

Updating dependency: (please check [Dependency management](../../development/dependencies) for additional information)

```sh
make update
```

## Limitations

Because [CSI](https://kubernetes-csi.github.io/docs/) is not ready on Windows, AzureDisk/AzureFile CSI drivers don't support Windows either. If you have Windows nodes in the cluster, please use kube-controller-manager instead of cloud-controller-manager.

