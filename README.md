# Cloud provider for Azure

## Cloud controller manager for Azure

This repository provides tools and scripts for building and testing `Kubernetes cloud-controller-manager` for Azure. The project is under development.

The azure cloud provider code locates at [Kubernetes repository directory](https://github.com/kubernetes/kubernetes/tree/master/pkg/cloudprovider/providers/azure). If you want to create issues or pull requests for cloud provider, please go to [Kubernetes repository](https://github.com/kubernetes/kubernetes).

There is an ongoing work for refactoring cloud providers out of the upstream repository. For more details, please check [this issue](https://github.com/kubernetes/features/issues/88).

Please checkout details in [Cloud controller manager doc](docs/cloud-controller-manager.md).

Please also check following docs:
- [Component versioning](docs/component-versioning.md)
- [Dependency management](docs/dependency-management.md)
- [E2E Tests](docs/e2e-tests.md)
- [Cloud provider config](docs/cloud-provider-config.md)

### NOTE
Currently this repository is used for building and testing cloud-controller-manager for Azure, it references Azure cloud provider implementation code as [vendor dir](vendor/k8s.io/kubernetes/pkg/cloudprovider/providers/azure). After handoff, the Azure cloud provider implementation will be moved here.
