---
title: "Deploy with Customized Images"
linkTitle: "Custom Images"
type: docs
weight: 1
description: >
    Deploy a cluster with customized CCM or CNM images.
---

Switch to the project root directory and `make image`. This will build both CCM and CNM images. If you want to build only one of them, try `make build-ccm-image` or `make build-node-image`. To push the images to your own image registry, you can specify the registry and image tag while building: `IMAGE_REGISTRY=<image registry name> IMAGE_TAG=<tag name> make image`. After building, you can push it by `make push`.
