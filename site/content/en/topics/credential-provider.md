---
title: "Kubelet Credential Provider"
linkTitle: "Kubelet Credential Provider"
weight: 10
type: docs
description: >
    Detailed steps to setup out-of-tree Kubelet Credential Provider.
---

> Note: The Kubelet credential provider feature is still in alpha and shouldn't be used in production environments. Please use `--azure-container-registry-config=/etc/kubernetes/cloud-config/azure.json` if you need pulling images from ACR in production.

As part of [Out-of-Tree Credential Providers](https://github.com/kubernetes/enhancements/tree/master/keps/sig-cloud-provider/2133-out-of-tree-credential-provider), the kubelet builtin image pulling from ACR (which could be enabled by setting `kubelet --azure-container-registry-config=<config-file>`) would be moved out-of-tree credential plugin `acr-credential-provider`. Please refer the original [KEP](https://github.com/kubernetes/enhancements/tree/master/keps/sig-cloud-provider/2133-out-of-tree-credential-provider) for details.

In order to switch the kubelet credential provider to out-of-tree, you'll have to

* Remove  `--azure-container-registry-config` from kubelet configuration options.
* Add `--feature-gates=KubeletCredentialProviders=true` to kubelet configuration options.
* Create directory `/var/lib/kubelet/credential-provider`, download 'acr-credential-provider' binary to this directory and add `--image-credential-provider-bin-dir=/var/lib/kubelet/credential-provider` to kubelet configuration options.
* Create the following credential-provider-config.yaml file and add `--image-credential-provider-config=/var/lib/kubelet/credential-provider-config.yaml` to kubelet configuration options.

```yaml
# cat /var/lib/kubelet/credential-provider-config.yaml
kind: CredentialProviderConfig
apiVersion: kubelet.config.k8s.io/v1alpha1
providers:
- name: acr-credential-provider
  apiVersion: credentialprovider.kubelet.k8s.io/v1alpha1
  defaultCacheDuration: 10m
  matchImages:
  - "*.azurecr.io"
  - "*.azurecr.cn"
  - "*.azurecr.de"
  - "*.azurecr.us"
  - "*.azurecr.*" # Only required for custom Azure cloud.
  args:
  - /etc/kubernetes/cloud-config/azure.json
```

