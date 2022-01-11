---
title: "Image building"
linkTitle: "Image building"
type: docs
weight: 5
description: >
    Image building.
---

## multi-arch image

Currently, only Linux multi-arch cloud-node-manager image is supported as a result of customer requests and windows limitations.
Supported Linux archs are defined by `ALL_ARCH.linux` in Makefile, and Windows os versions are by `ALL_OSVERSIONS.windows`.

### Windows multi-arch image limitation

Images [nanoserver](https://hub.docker.com/_/microsoft-windows-nanoserver) and [servercore](https://hub.docker.com/_/microsoft-windows-servercore) are referenced to build a Windows image, but as current officially published servercore images does not support non-amd64 image, and only Windows server 1809 has the support of non-amd64 for nanoserver, amd64 is the only supported arch for a range of Windows OS version so far. 
This issue is tracked [here](https://github.com/microsoft/Windows-Containers/issues/195)

## hand-on examples

To build and publish the multi-arch image for node manager

```sh
IMAGE_REGISTRY=<registry> make build-all-node-images
IMAGE_REGISTRY=<registry> make push-multi-arch-node-manager-image
```

To build a specific Linux arch image for node manager

```sh
IMAGE_REGISTRY=<registry> ARCH=amd64 make build-node-image-linux
```

To build specific Windows OS and arch image for node manager

```sh
IMAGE_REGISTRY=<registry> OUTPUT_TYPE=registry ARCH=amd64 WINDOWS_OSVERSION=1809 build-node-image-windows
```
The `OUTPUT_TYPE` registry here means the built image will be published to the registry, this is necessary to build a Windows image from a Linux working environment. An alternative is to export the image tarball to a local destination, like `OUTPUT_TYPE=docker,dest=dstdir/azure-cloud-node-manager.tar`. For more info about `docker buildx` output type, please check out [here](https://docs.docker.com/engine/reference/commandline/buildx_build/#output)