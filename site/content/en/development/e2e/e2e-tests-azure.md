---
title: "Azure E2E tests"
linkTitle: "Azure E2E tests"
type: docs
description: >
    Azure E2E tests guidance.
---

## Overview

Here provides some E2E tests only specific to Azure provider.

## Prerequisite

### Deploy a Kubernetes cluster with Azure CCM

Refer step 1-3 in [e2e-tests](../e2e-tests) for deploying the Kubernetes cluster.

### Setup Azure credentials

```sh
export AZURE_TENANT_ID=<tenant-id>               # the tenant ID
export AZURE_SUBSCRIPTION_ID=<subscription-id>           # the subscription ID
export AZURE_CLIENT_ID=<service-principal-id>        # the service principal ID
export AZURE_CLIENT_SECRET=<service-principal-secret>   # the service principal secret
export AZURE_ENVIRONMENT=<AzurePublicCloud>     # the cloud environment (optional, default is AzurePublicCloud)
export AZURE_LOCATION=<location>                # the location
export AZURE_LOADBALANCER_SKU=<loadbalancer-sku> # the sku of load balancer (optional, default is basic)
```

### Setup KUBECONFIG

- Locate your kubeconfig and set it as env variable
    ```export KUBECONFIG=<kubeconfig>```
    or
    ```cp <kubeconfig> ~/.kube/config```

- Test it via  ```kubectl version```

## Run Test

### Have installed ginkgo
- Run ```ginkgo ./tests/e2e/ ```

    For more usage of ginkgo, please follow [ginkgo](https://github.com/onsi/ginkgo/blob/master/README.md)

### Without ginkgo
- Run ```go test ./tests/e2e/ -timeout 0```

After a long time test, a JUnit report will be generated in a directory named by the cluster name
