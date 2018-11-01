# Component Versioning

When syncing to a new Kubernetes release, please update corresponding lines in following files.
- [glide.yaml](/glide.yaml) for glide update
- [linux.json](/tests/k8s-azure/manifest/linux.json) for local test deployment
- [Dockerfile](/tests/k8s-azure/Dockerfile) for local test image

Please find details as following

## Components
### 1. Main package
- Main dockerfile: [Dockerfile](/Dockerfile)
    
    Update golang version in `FROM golang:*`

    Update `FROM buildpack-deps:*` if base image version changes.

- Test deployment image: [linux.json](/tests/k8s-azure/manifest/linux.json)

  Update `customCcmImage` to latest stable released image, this is used for local deployment.

### 2. Kubernetes in E2E test
Following Kubernetes versions should stick to Kubernetes package version specified in [glide.yaml](/glide.yaml), please see [Dependency management](dependency-management.md) for details about package versions.

   - Test cluster hyperkube Image: [linux.json](/tests/k8s-azure/manifest/linux.json)
     
     Update `"customHyperkubeImage": "*"` for Kubernetes version.

   - E2E tests: [Dockerfile](/tests/k8s-azure/Dockerfile)
 
     Update `ARG K8S_VERSION=` for Kubernetes version.
 
     Update `FROM golang:* AS build_kubernetes`. This should stick to the Go version used by [Kubernetes](https://github.com/kubernetes/kubernetes/blob/master/build/build-image/cross/Dockerfile)

### 3. acs-engine in E2E test
   Edit file [Dockerfile](/tests/k8s-azure/Dockerfile)

   Update `ARG ACSENGINE_VERSION=` for acs-engine version.

   Update `FROM golang:* AS build_acs-engine`.
   This should stick to the Go version used by [acs-engine](https://github.com/Azure/acs-engine/blob/master/Dockerfile).
