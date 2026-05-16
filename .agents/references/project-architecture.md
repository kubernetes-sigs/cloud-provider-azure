# Project Architecture

This repository is the Kubernetes out-of-tree cloud provider for Azure,
implementing the cloud provider interface. It produces three binaries:

- **cloud-controller-manager**: manages Azure resources for a Kubernetes cluster
- **cloud-node-manager**: runs on each node and manages node-level cloud operations
- **acr-credential-provider**: provides Azure Container Registry credentials

## Layout

```text
cmd/
  cloud-controller-manager/     Entry point for CCM
  cloud-node-manager/           Entry point for CNM
  acr-credential-provider/      Entry point for ACR credential provider

pkg/
  provider/                     Core cloud provider implementation
    azure_loadbalancer*.go        Load balancer reconciliation
    azure_standard.go             Standard single-VM support
    azure_vmss*.go                VMSS scale set support
    azure_vmssflex*.go            VMSS Flex support
    azure_routes.go               Route table management
    azure_zones.go                Availability zone logic
    azure_instances_v1.go         Instance metadata v1
    azure_instances_v2.go         Instance metadata v2
    azure_controller_*.go         Disk attach/detach for Standard, VMSS, VMSSFlex
    azure_backoff.go              Backoff and retry logic
    azure_lock.go                 Concurrent access locking
    azure_*_repo.go               Data access and repository pattern
    azure_fakes.go                Manual test fakes
    azure_mock_*.go               Generated mocks
  azclient/                     Azure SDK client wrappers; do not call Azure SDK directly
  consts/                       Shared constants
  cache/                        Caching layer for Azure API responses
  node/                         Node management helpers
  nodemanager/                  Node lifecycle management
  nodeipam/                     Node IP address management
  credentialprovider/           ACR credential provider logic
  util/                         Shared utilities
  log/                          Logging helpers
  metrics/                      Prometheus metrics
  trace/                        Tracing helpers
  version/                      Version info
```
