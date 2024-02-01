# Cloud provider for Azure

[![Go Report Card](https://goreportcard.com/badge/sigs.k8s.io/cloud-provider-azure)](https://goreportcard.com/report/sigs.k8s.io/cloud-provider-azure)
[![Coverage Status](https://coveralls.io/repos/github/kubernetes-sigs/cloud-provider-azure/badge.svg?branch=master)](https://coveralls.io/github/kubernetes-sigs/cloud-provider-azure?branch=master)
[![GitHub stars](https://img.shields.io/github/stars/kubernetes-sigs/cloud-provider-azure.svg)](https://github.com/kubernetes-sigs/cloud-provider-azure/stargazers)
[![GitHub stars](https://img.shields.io/badge/contributions-welcome-orange.svg)](https://github.com/kubernetes-sigs/cloud-provider-azure/blob/master/CONTRIBUTING.md)

## Introduction

This repository provides the Azure implementation of the Kubernetes cloud provider [interface](https://github.com/kubernetes/cloud-provider).

This is the "external" or "out-of-tree" cloud provider for Azure. The "in-tree" cloud provider has been deprecated since v1.20 and only bug fixes are allowed in its [Kubernetes repository directory](https://github.com/kubernetes/kubernetes/tree/master/staging/src/k8s.io/legacy-cloud-providers/azure).

## Current status

`cloud-provider-azure` has been **GA** since v1.0.0. Releases are available from the Microsoft Container Registry (MCR).

The latest release of azure-cloud-controller-manager and azure-cloud-node-manager can be found at

* `mcr.microsoft.com/oss/kubernetes/azure-cloud-controller-manager:v1.29.0`
* `mcr.microsoft.com/oss/kubernetes/azure-cloud-node-manager:v1.29.0`

### Version matrix

(Minor release versions match Kubernetes minor release versions since v1.23.0.)

| Kubernetes version | cloud-provider version | cloud-provider branch |
|--------------------|------------------------|-----------------------|
| master             | N/A                    | master                |
| v1.y.x             | v1.y.z                 | release-1.y           |
| v1.29.x            | v1.29.z                | release-1.29          |
| v1.28.x            | v1.28.z                | release-1.28          |
| v1.27.x            | v1.27.z                | release-1.27          |
| v1.26.x            | v1.26.z                | release-1.26          |
| v1.25.x            | v1.25.z                | release-1.25          |
| v1.24.x            | v1.24.z                | release-1.24          |
| v1.23.x            | v1.23.z                | release-1.23          |
| v1.22.x            | v1.1.z                 | release-1.1           |

### AKS version matrix

Below table shows the cloud-controller-manager and cloud-node-manager versions supported in Azure Kubernetes Service(AKS).

| AKS version                 | cloud-controller-manager version | cloud-node-manager version |
|-----------------------------|----------------------------------|----------------------------|
| v1.29.x                     | v1.29.0                          | v1.29.0                    |
| v1.28.x                     | v1.28.5                          | v1.28.5                    |
| v1.27.x                     | v1.27.13                         | v1.27.13                   |
| v1.26.x                     | v1.26.19                         | v1.26.19                   |
| v1.25.x                     | v1.25.24                         | v1.25.24                   |
| v1.24.x                     | v1.24.21                         | v1.24.20                   |
| v1.23.x                     | v1.23.30                         | v1.23.24                   |
| v1.22.x                     | v1.1.26                          | v1.1.14                    |
| v1.21.x                     | v1.0.23                          | v1.0.18                    |

## Build

To build the binary for azure-cloud-controller-manager:

```sh
make all
```

To build the Docker image for azure-cloud-controller-manager:

```sh
IMAGE_REGISTRY=<registry> make image
```

For detailed directions on image building, please read [here](http://kubernetes-sigs.github.io/cloud-provider-azure/development/image-building/).

## Run

To run azure-cloud-controller-manager locally:

```sh
azure-cloud-controller-manager \
    --cloud-provider=azure \
    --cluster-name=kubernetes \
    --controllers=*,-cloud-node \
    --cloud-config=/etc/kubernetes/cloud-config/azure.json \
    --kubeconfig=/etc/kubernetes/kubeconfig \
    --allocate-node-cidrs=true \
    --configure-cloud-routes=true \
    --cluster-cidr=10.240.0.0/16 \
    --route-reconciliation-period=10s \
    --leader-elect=true \
    --secure-port=10267 \
    --v=2
```

To run azure-cloud-node-manager locally:

```sh
azure-cloud-node-manager \
    --node-name=$(hostname) \
    --wait-routes=true
```

It is recommended to run azure-cloud-controller-manager as a Deployment with multiple replicas, or directly with kubelet as static Pods on each control plane Node. See [here](examples/out-of-tree/cloud-controller-manager.yaml) for an example.

Get more detail at [Deploy Cloud Controller Manager](http://kubernetes-sigs.github.io/cloud-provider-azure/install/azure-ccm/).

## E2E tests

Please read the following documents for e2e test information:

- [Upstream Kubernetes e2e tests](http://kubernetes-sigs.github.io/cloud-provider-azure/development/e2e/e2e-tests/)
- [Azure e2e tests](http://kubernetes-sigs.github.io/cloud-provider-azure/development/e2e/e2e-tests-azure/)

## Documentation

Refer <https://cloud-provider-azure.sigs.k8s.io/> for the documentation of Cloud Provider Azure (documents are hosted in [documentation](https://github.com/kubernetes-sigs/cloud-provider-azure/tree/documentation) branch).

## Contributing

Please see [CONTRIBUTING.md](CONTRIBUTING.md) for instructions on how to contribute.

## Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

## License

[Apache License 2.0](LICENSE).
