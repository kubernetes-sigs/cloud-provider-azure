# Cloud provider for Azure

[![Go Report Card](https://goreportcard.com/badge/sigs.k8s.io/cloud-provider-azure)](https://goreportcard.com/report/sigs.k8s.io/cloud-provider-azure)
[![Coverage Status](https://coveralls.io/repos/github/kubernetes-sigs/cloud-provider-azure/badge.svg?branch=master)](https://coveralls.io/github/kubernetes-sigs/cloud-provider-azure?branch=master)
[![GitHub stars](https://img.shields.io/github/stars/kubernetes-sigs/cloud-provider-azure.svg)](https://github.com/kubernetes-sigs/cloud-provider-azure/stargazers)
[![GitHub stars](https://img.shields.io/badge/contributions-welcome-orange.svg)](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/CONTRIBUTING.md)

## Introduction

This repository provides tools and scripts for building and testing `Kubernetes cloud-controller-manager` for Azure. The project is under development.

The Azure cloud provider code locates at [Kubernetes repository directory](https://github.com/kubernetes/kubernetes/tree/master/staging/src/k8s.io/legacy-cloud-providers/azure). If you want to create issues or pull requests for cloud provider, please go to [Kubernetes repository](https://github.com/kubernetes/kubernetes).

There is an ongoing work for refactoring cloud providers out of the upstream repository. For more details, please check [this issue](https://github.com/kubernetes/enhancements/issues/667).

## Current status

cloud-provider-azure is still under **alpha** stage and its releases are maintained on Microsoft Container Registry (MCR).

The latest version of azure-cloud-controller-manager and azure-cloud-node-manager could be found at

* `mcr.microsoft.com/oss/kubernetes/azure-cloud-controller-manager:v0.6.0`
* `mcr.microsoft.com/oss/kubernetes/azure-cloud-node-manager:v0.6.0`

Version matrix:

|Kubernetes version|cloud-provider version|cloud-provider branch|
|------------------|----------------------|---------------------|
| master           | N/A                  | master              |
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

Please checkout more details at [docs/cloud-controller-manager.md](docs/cloud-controller-manager.md).

## E2E tests

Please check the following documents for e2e tests:

- [Upstream Kubernetes e2e tests](docs/e2e-tests.md)
- [Azure e2e tests](docs/e2e-tests-azure.md)

## Documentation

- [Dependency management](docs/dependency-management.md)
- [Cloud provider config](docs/cloud-provider-config.md)
- [Azure load balancer and annotations](docs/services/README.md)
- [Azure permissions](docs/azure-permissions.md)
- [Azure availability zones](docs/using-availability-zones.md)
- [Cross resource group nodes](docs/using-cross-resource-group-nodes.md)
- [AzureDisk known issues](docs/persistentvolumes/azuredisk/issues.md)
- [AzureFile known issues](docs/persistentvolumes/azurefile/issues.md)

See [docs](docs/) for more documentations.

## Contributing

Please see [CONTRIBUTING.md](CONTRIBUTING.md) for instructions on how to contribute.

## Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

## License

[Apache License 2.0](LICENSE).
