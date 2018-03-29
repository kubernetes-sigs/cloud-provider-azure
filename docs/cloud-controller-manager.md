# Cloud controller manager for Azure

`azure-cloud-controller-manager` is a Kubernetes component that provides interoperability with Azure API, and will be used by Kubernetes clusters running on Azure. It runs together with other components to provide the Kubernetes cluster’s control plane.

Using [cloud-controller-manager](https://kubernetes.io/docs/concepts/overview/components/#cloud-controller-manager) is a new alpha feature for Kubernetes since v1.6. `cloud-controller-manager` runs cloud provider related controller loops, which used to be run by `controller-manager`.

`azure-cloud-controller-manager` is a specialization of `cloud-controller-manager`. It depends on [cloud-controller-manager app](https://github.com/kubernetes/kubernetes/tree/master/cmd/cloud-controller-manager/app) and [azure cloud provider](https://github.com/kubernetes/kubernetes/tree/master/pkg/cloudprovider/providers/azure).

## Usage
You can use [acs-engine](https://github.com/Azure/acs-engine) to deploy a Kubernetes cluster running with cloud-controller-manager. It supports deploying `Kubernetes azure-cloud-controller-manager` for Kubernetes v1.8+.

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