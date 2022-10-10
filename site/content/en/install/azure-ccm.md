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

## Deployment

There is a [helm chart available](https://github.com/kubernetes-sigs/cloud-provider-azure/tree/master/helm/cloud-provider-azure) which can be used to deploy the Azure cloud controller manager.

To deploy Azure cloud controller manager, the following components need to be configured.

### kubelet

|Flag|Value|Remark|
|----|-----|------|
|`--cloud-provider`|external|cloud-provider should be set external|
|`--azure-container-registry-config`|/etc/kubernetes/cloud-config/azure.json|Used for Azure credential provider|

### kube-controller-manager

|Flag|Value|Remark|
|---|---|---|
|`--cloud-provider`|external|cloud-provider should be set external|
|`--external-cloud-volume-plugin`|azure|Optional*|

`*`: Since cloud controller manager does not support volume controllers, it will not provide volume capabilities compared to using previous built-in cloud provider case. You can add this flag to turn on volume controller for in-tree cloud providers. This option is likely to be [removed with in-tree cloud providers](https://github.com/kubernetes/kubernetes/blob/v1.11.0-alpha.2/cmd/kube-controller-manager/app/options/options.go#L93) in future.

### kube-apiserver

Do not set flag `--cloud-provider`.

### azure-cloud-controller-manager

azure-cloud-controller-manager should be run as Deployment with multiple replicas or Kubelet static Pods on each master Node.

|Flag|Value|Remark|
|---|---|---|
|`--cloud-provider`|azure|cloud-provider should be set azure|
|`--cloud-config`|/etc/kubernetes/cloud-config/azure.json|Path for [cloud provider config](../configs.md)|
|`--controllers`|*,-cloud-node | cloud node controller should be disabled|
|`--configure-cloud-routes`| "false" for Azure CNI and "true" for other network plugins| Used for non-AzureCNI clusters |

For other flags such as `--allocate-node-cidrs`, `--cluster-cidr` and `--cluster-name`, they are moved from kube-controller-manager. If you are migrating from kube-controller-manager, they should be set to same value.

For details of those flags, please refer to this [doc](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-controller-manager/).

### azure-cloud-node-manager

azure-cloud-node-manager should be run as daemonsets on both Windows and Linux nodes, and the following configurations should be set:

|Flag|Value|Remark|
|---|---|---|
|`--node-name`|The node name for the Pod|Kubernetes Downward API could be used to get Pod's name|
|`--wait-routes`| only set to true when `--configure-cloud-routes=true` in cloud-controller-manager | Used for non-AzureCNI clusters |

Please refer examples [here](../example/out-of-tree.md) for sample deployment manifests for above components.

Alternatively, you can use [cluster-api-provider-azure](https://github.com/kubernetes-sigs/cluster-api-provider-azure) to deploy a Kubernetes cluster running with cloud-controller-manager.

## AzureDisk and AzureFile

AzureDisk and AzureFile volume plugins are not supported with in-tree cloud provider (See [kubernetes/kubernetes#71018](https://github.com/kubernetes/kubernetes/issues/71018) for explanations).

Hence, [azuredisk-csi-driver](https://github.com/kubernetes-sigs/azuredisk-csi-driver) and [azurefile-csi-driver](https://github.com/kubernetes-sigs/azurefile-csi-driver) should be used for persistent volumes. Please refer the installation guides [here](https://github.com/kubernetes-sigs/azuredisk-csi-driver/tree/master/charts) and [here](https://github.com/kubernetes-sigs/azurefile-csi-driver/tree/master/charts) for their deployments.

### Change default storage class

Follow the steps below if you want change the current default storage class to AzureDisk CSI driver.

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
