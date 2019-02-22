# Cloud controller manager for Azure

`azure-cloud-controller-manager` is a Kubernetes component that provides interoperability with Azure API, and will be used by Kubernetes clusters running on Azure. It runs together with other components to provide the Kubernetes cluster’s control plane.

Using [cloud-controller-manager](https://kubernetes.io/docs/concepts/overview/components/#cloud-controller-manager) is a new alpha feature for Kubernetes since v1.6. `cloud-controller-manager` runs cloud provider related controller loops, which used to be run by `controller-manager`.

`azure-cloud-controller-manager` is a specialization of `cloud-controller-manager`. It depends on [cloud-controller-manager app](https://github.com/kubernetes/kubernetes/tree/master/cmd/cloud-controller-manager/app) and [azure cloud provider](https://github.com/kubernetes/kubernetes/tree/master/pkg/cloudprovider/providers/azure).

## Usage
To use cloud controller manager, the following components need to be configured:

1. kubelet

    Set flag `--cloud-provider=external`

1. kube-controller-manager
    Set following flags:

    |Flag|Value|Remark|
    |---|---|---|
    |--cloud-provider|external||
    |--external-cloud-volume-plugin|azure|Optional*|

    `*` Since cloud controller manager does not support volume controllers, it will not provide volume capabilities compared to using previous built-in cloud provider case. You can add this flag to turn on volume controller for in-tree cloud providers. This option is likely to be [removed with in-tree cloud providers](https://github.com/kubernetes/kubernetes/blob/v1.11.0-alpha.2/cmd/kube-controller-manager/app/options/options.go#L93) in future.

1. kube-apiserver

    Do not set flag `--cloud-provider`

1. azure-cloud-controller-manager

    Set following flags:

    |Flag|Value|Remark|
    |---|---|---|
    |--cloud-provider|azure||
    |--cloud-config||Path for [cloud provider config](cloud-provider-config.md)|
    |--kubeconfig||Path for cluster kubeconfig|

    For other flags such as `--allocate-node-cidrs`, `--configure-cloud-routes`, `--cluster-cidr`, they are moved from kube-controller-manager. If you are migrating from kube-controller-manager, they should be set to same value.

    For details of those flags, please refer to this [doc](https://kubernetes.io/docs/reference/command-line-tools-reference/cloud-controller-manager/).

Alternatively, you can use [aks-engine](https://github.com/Azure/aks-engine) to deploy a Kubernetes cluster running with cloud-controller-manager. It supports deploying `Kubernetes azure-cloud-controller-manager` for Kubernetes v1.8+.

## Development
Build project:
```
make
```

Build image:
```
make image
```

Run unit tests:
```
make test-unit
```

Updating dependency: (please check [Dependency management](dependency-management.md) for additional information)
```
make update
```
