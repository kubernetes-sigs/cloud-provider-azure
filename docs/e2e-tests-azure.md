# Azure E2E Test

## Overview

Here provides some E2E tests only specific to Azure provider.

## Prerequisite
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
