# Azure E2E Test

## Overview

Here provides some E2E tests only specific to Azure provider.

## Prerequisite

### Deploy a Kubernetes cluster with Azure CCM

Refer step 1-3 in [e2e-tests.md](e2e-tests.md) for deploying the Kubernetes cluster.

### Setup Azure credentials

```sh
export K8S_AZURE_TENANTID=<tenant-id>               # the tenant ID
export K8S_AZURE_SUBSID=<subscription-id>           # the subscription ID
export K8S_AZURE_SPID=<service-principal-id>        # the service principal ID
export K8S_AZURE_SPSEC=<service-principal-secret>   # the service principal secret
export K8S_AZURE_ENVIRONMENT=<AzurePublicCloud>     # the cloud environment (optional, default is AzurePublicCloud)
export K8S_AZURE_LOCATION=<location>                # the location
export K8S_AZURE_LOADBALANCE_SKU=<loadbalancer-sku> # the sku of load balancer (optional, default is basic)
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

After a long time test, a junit report will be generated in a directory named by the cluster name
