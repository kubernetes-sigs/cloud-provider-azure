# Cloud provider for Azure

[![Go Report Card](https://goreportcard.com/badge/sigs.k8s.io/cloud-provider-azure)](https://goreportcard.com/report/sigs.k8s.io/cloud-provider-azure)
[![GitHub stars](https://img.shields.io/github/stars/kubernetes-sigs/cloud-provider-azure.svg)](https://github.com/kubernetes-sigs/cloud-provider-azure/stargazers)
[![GitHub stars](https://img.shields.io/badge/contributions-welcome-orange.svg)](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/CONTRIBUTING.md)

## Introduction

This repository provides Azure implementation of Kubernetes cloud provider [interface](https://github.com/kubernetes/cloud-provider). The in-tree cloud provider has been deprecated since v1.20 and only the bug fixes were allowed in the [Kubernetes repository directory](https://github.com/kubernetes/kubernetes/tree/master/staging/src/k8s.io/legacy-cloud-providers/azure).

## Current status

cloud-provider-azure is under **Beta** stage and its releases are maintained on Microsoft Container Registry (MCR).

The latest version of azure-cloud-controller-manager and azure-cloud-node-manager could be found at

* `mcr.microsoft.com/oss/kubernetes/azure-cloud-controller-manager:v0.7.0`
* `mcr.microsoft.com/oss/kubernetes/azure-cloud-node-manager:v0.7.0`

Version matrix:

|Kubernetes version|cloud-provider version|cloud-provider branch|
|------------------|----------------------|---------------------|
| master           | N/A                  | master              |
| v1.20.x          | v0.7.0               | release-0.7         |
| v1.19.x          | v0.6.0               | release-0.6         |
| v1.18.x          | v0.5.1               | release-0.5         |
| v1.17.x          | v0.4.1               | N/A                 |
| v1.16.x          | v0.3.0               | N/A                 |
| v1.15.x          | v0.2.0               | N/A                 |

## Build

Build azure-cloud-controller-manager with pure make:

```sh
make
```

or with bazel:

```sh
make bazel-build
```

Build docker image for azure-cloud-controller-manager:

```sh
IMAGE_REGISTRY=<registry> make image
```

## Run

Run azure-cloud-controller-manager locally:

```sh
azure-cloud-controller-manager --cloud-provider=azure \
    --cluster-name=kubernetes \
    --cloud-config=/etc/kubernetes/azure.json \
    --kubeconfig=/etc/kubernetes/kubeconfig \
    --allocate-node-cidrs=true \
    --configure-cloud-routes=true \
    --cluster-cidr=10.240.0.0/16 \
    --route-reconciliation-period=10s \
    --leader-elect=true \
    --v=2
```

It is recommended to run azure-cloud-controller-manager as Pods on master nodes. See [here](examples/out-of-tree/cloud-controller-manager.yaml) for the example.

Please checkout more details at [Deploy Cloud Controller Manager](http://kubernetes-sigs.github.io/cloud-provider-azure/install/azure-ccm/).

## E2E tests

Please check the following documents for e2e tests:

- [Upstream Kubernetes e2e tests](http://kubernetes-sigs.github.io/cloud-provider-azure/development/e2e/e2e-tests/)
- [Azure e2e tests](http://kubernetes-sigs.github.io/cloud-provider-azure/development/e2e/e2e-tests-azure/)

## Documentation

- [Dependency management]([docs/dependency-management.md](http://kubernetes-sigs.github.io/cloud-provider-azure/development/dependencies/))
- [Cloud provider config](http://kubernetes-sigs.github.io/cloud-provider-azure/install/configs/)
- [Azure load balancer and annotations](http://kubernetes-sigs.github.io/cloud-provider-azure/topics/loadbalancer/)
- [Azure permissions](http://kubernetes-sigs.github.io/cloud-provider-azure/topics/azure-permissions/)
- [Azure availability zones](http://kubernetes-sigs.github.io/cloud-provider-azure/topics/availability-zones/)
- [Cross resource group nodes]([docs/using-cross-resource-group-nodes.md](http://kubernetes-sigs.github.io/cloud-provider-azure/topics/cross-resource-group-nodes/))
- [AzureDisk known issues]([docs/persistentvolumes/azuredisk/issues.md](http://kubernetes-sigs.github.io/cloud-provider-azure/faq/known-issues/azuredisk/))
- [AzureFile known issues]([docs/persistentvolumes/azurefile/issues.md](http://kubernetes-sigs.github.io/cloud-provider-azure/faq/known-issues/azurefile/))

See [kubernetes-sigs.github.io/cloud-provider-azure](https://kubernetes-sigs.github.io/cloud-provider-azure/) for more documentations.

## Contributing

Please see [CONTRIBUTING.md](CONTRIBUTING.md) for instructions on how to contribute.

## Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

## License

[Apache License 2.0](LICENSE).

