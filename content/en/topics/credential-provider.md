---
title: "Kubelet Credential Provider"
linkTitle: "Kubelet Credential Provider"
weight: 10
type: docs
description: >
    Detailed steps to setup out-of-tree Kubelet Credential Provider.
---

As part of [Out-of-Tree Credential Providers](https://github.com/kubernetes/enhancements/tree/master/keps/sig-cloud-provider/2133-out-of-tree-credential-provider), the kubelet builtin image pulling from ACR (which could originally be enabled by setting `kubelet --azure-container-registry-config=<config-file>`) would be moved out-of-tree credential plugin `acr-credential-provider`. Please refer the original [KEP](https://github.com/kubernetes/enhancements/tree/master/keps/sig-cloud-provider/2133-out-of-tree-credential-provider) and the [credential provider KEP](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/2133-kubelet-credential-providers) for details.

In order to switch the kubelet credential provider to out-of-tree, you'll have to

* Remove  `--azure-container-registry-config` from kubelet configuration options.
* Create directory `/var/lib/kubelet/credential-provider`, download 'acr-credential-provider' binary to this directory and add `--image-credential-provider-bin-dir=/var/lib/kubelet/credential-provider` to kubelet configuration options.
* Create the following credential-provider-config.yaml file and add `--image-credential-provider-config=/var/lib/kubelet/credential-provider-config.yaml` to kubelet configuration options.

```yaml
# cat /var/lib/kubelet/credential-provider-config.yaml
kind: CredentialProviderConfig
apiVersion: kubelet.config.k8s.io/v1
providers:
- name: acr-credential-provider
  apiVersion: credentialprovider.kubelet.k8s.io/v1
  defaultCacheDuration: 10m
  matchImages:
  - "*.azurecr.io"
  - "*.azurecr.cn"
  - "*.azurecr.de"
  - "*.azurecr.us"
  args:
  - /etc/kubernetes/azure.json
```

## Registry Mirror

Kubelet credential provider for Azure allows the user to mirror a registry to another one, and the latter registry will be used for authentication to Azure Registry when the image matches the first registry. Kubelet will request pulling image to the container runtime with the image from mirror source and credential from mirror target.

This feature is beneficial for cases like when the user leverages [Containerd registry host namespace](https://github.com/containerd/containerd/blob/main/docs/hosts.md#registry-host-namespace) to proxy the workload image authentication to another registry server to achieve production-test environment agnostic or private registry.

In order to turn on registry mirror configuration, you'll have to

* add the mirror source image(s) in matchImages list
* add the registry mirror(s) in `arg` field `--registry-mirror`
* multiple registry mirrors are supported, for example, `--registry-mirror=mcrx.microsoft.com:xxx.azurecr.io,mcry.microsoft.com:yyy.azurecr.io`

```yaml
kind: CredentialProviderConfig
apiVersion: kubelet.config.k8s.io/v1
providers:
- name: acr-credential-provider
  apiVersion: credentialprovider.kubelet.k8s.io/v1
  defaultCacheDuration: 10m
  matchImages:
  - "*.azurecr.io"
  - "*.azurecr.cn"
  - "*.azurecr.de"
  - "*.azurecr.us"
  - "mcr.microsoft.com"
  args:
  - /etc/kubernetes/azure.json
  - --registry-mirror=mcr.microsoft.com:xxx.azurecr.io
```
