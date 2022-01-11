---
title: "Deploy with Customized Images"
linkTitle: "Custom Images"
type: docs
weight: 1
description: >
    Deploy a cluster with customized CCM or CNM images.
---

Switch to the project root directory and run the following command to build both CCM and CNM images:

```sh
make image
```

If you want to build only one of them, try `make build-ccm-image` or `ARCH=amd64 make build-node-image-linux`.

To push the images to your own image registry, you can specify the registry and image tag while building:

```sh
IMAGE_REGISTRY=<image registry name> IMAGE_TAG=<tag name> make image
```

After building, you can push them to your image registry by `make push`.

Please follow [here](http://kubernetes-sigs.github.io/cloud-provider-azure/development/image-building/) to build multi-arch image