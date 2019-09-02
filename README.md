# Cloud provider for Azure

[![Go Report Card](https://goreportcard.com/badge/k8s.io/cloud-provider-azure)](https://goreportcard.com/report/k8s.io/cloud-provider-azure)
[![GitHub stars](https://img.shields.io/github/stars/kubernetes/cloud-provider-azure.svg)](https://github.com/kubernetes/cloud-provider-azure/stargazers)
[![GitHub stars](https://img.shields.io/badge/contributions-welcome-orange.svg)](https://github.com/kubernetes/cloud-provider-azure/blob/master/CONTRIBUTING.md)

## Introduction

This repository provides tools and scripts for building and testing `Kubernetes cloud-controller-manager` for Azure. The project is under development.

The Azure cloud provider code locates at [Kubernetes repository directory](https://github.com/kubernetes/kubernetes/tree/master/pkg/cloudprovider/providers/azure). If you want to create issues or pull requests for cloud provider, please go to [Kubernetes repository](https://github.com/kubernetes/kubernetes).

There is an ongoing work for refactoring cloud providers out of the upstream repository. For more details, please check [this issue](https://github.com/kubernetes/features/issues/88).

## Current status

cloud-provider-azure is still under **alpha** stage and its releases are maintained on Microsoft Container Registry (MCR).

The latest version of azure-cloud-controller-manager could be found at `mcr.microsoft.com/k8s/core/azure-cloud-controller-manager:v0.1.0`.

Version matrix:

|Kubernetes version|cloud-provider version|cloud-provider branch|
|------------------|----------------------|---------------------|
| v1.16.x          |                      | master              |
| v1.15.x          | v0.2.0               | N/A                 |
| v1.14.x          | v0.1.0               | N/A                 |

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

- [Component versioning](docs/component-versioning.md)
- [Dependency management](docs/dependency-management.md)
- [Cloud provider config](docs/cloud-provider-config.md)
- [Azure Load balancer and annotations](docs/services/README.md)
- [Using Azure availability zones](docs/using-availability-zones.md)
- [Using cross resource group nodes](docs/using-cross-resource-group-nodes.md)
- [AzureDisk known issues](docs/persistentvolumes/azuredisk/issues.md)
- [AzureFile known issues](docs/persistentvolumes/azurefile/issues.md)

See [docs](docs/) for more documentations.

## Contributing

Please see [CONTRIBUTING.md](CONTRIBUTING.md) for instructions on how to contribute.

## Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

## License

[Apache License 2.0](LICENSE).