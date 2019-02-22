# Cloud provider for Azure

## Introduction

This repository provides tools and scripts for building and testing `Kubernetes cloud-controller-manager` for Azure. The project is under development.

The Azure cloud provider code locates at [Kubernetes repository directory](https://github.com/kubernetes/kubernetes/tree/master/pkg/cloudprovider/providers/azure). If you want to create issues or pull requests for cloud provider, please go to [Kubernetes repository](https://github.com/kubernetes/kubernetes).

There is an ongoing work for refactoring cloud providers out of the upstream repository. For more details, please check [this issue](https://github.com/kubernetes/features/issues/88).

## Build

Build azure-cloud-controller-manager:

```sh
make
```

Build docker image for azure-cloud-controller-manager:

```sh
IMAGE_REGISTRY=<registry> make image
```

## Run

Run azure-cloud-controller-manager:

```sh
azure-cloud-controller-manager --cloud-provider=azure \
    --cloud-config=/etc/kubernetes/azure.json \
    --kubeconfig=/etc/kubernetes/kubeconfig \
    --allocate-node-cidrs=true \
    --configure-cloud-routes=true \
    --cluster-cidr=10.240.0.0/12 \
    --leader-elect=true \
    --v=2
```

Please checkout more details in [docs/cloud-controller-manager.md](docs/cloud-controller-manager.md).

## E2E tests

Please check the following documents for e2e tests:

- [Upstream Kubernetes e2e tests](docs/e2e-tests.md)
- [Azure e2e tests](docs/e2e-tests-azure.md)

## Documentation

- [Component versioning](docs/component-versioning.md)
- [Dependency management](docs/dependency-management.md)
- [Cloud provider config](docs/cloud-provider-config.md)
- [Load balancer Annotations](docs/azure-loadbalancer.md)
- [Using Azure availability zones](docs/using-availability-zones.md)
- [Using cross resource group nodes](docs/using-cross-resource-group-nodes.md)
- [AzureDisk known issues](docs/azuredisk-issues.md)
- [AzureFile known issues](docs/azurefile-issues.md)

See [docs](docs/) for more documentations.

## Contributing

Please see [CONTRIBUTING.md](CONTRIBUTING.md) for instructions on how to contribute.

## NOTE

Currently this repository is used for building and testing cloud-controller-manager for Azure, it references Azure cloud provider implementation code as [vendor dir](vendor/k8s.io/kubernetes/pkg/cloudprovider/providers/azure). After handoff, the Azure cloud provider implementation will be moved here.
