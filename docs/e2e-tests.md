# E2E tests

## Prerequisite
- An azure service principal

    Please follow this [guide](https://github.com/Azure/aks-engine/blob/master/docs/topics/service-principals.md) for creating an azure service principal
    The service principal should either have:
    - Contributor permission of a subscription
    - Contributor permission of a resource group. In this case, please create the resource group first

## How to run E2E test locally

### 1. Without container

1. Prepare dependency project
- [aks-engine](https://github.com/Azure/aks-engine)

    It is recommended to use the same version as defined in test [Dockerfile](/tests/k8s-azure/Dockerfile). For example:
    ```
    ARG AKSENGINE_VERSION=v0.31.1
    ```

    Build aks-engine, and make aks-engine binary in PATH environment variable.

    ```
    go get -d github.com/Azure/aks-engine
    pushd $GOPATH/src/github.com/Azure/aks-engine
    # git checkout <version>
    make
    popd
    export PATH=$PATH:$GOPATH/src/github.com/Azure/aks-engine/bin
    ```

- [Kubernetes](https://github.com/kubernetes/kubernetes)

    This serves as E2E tests case source, it should be located at `$GOPATH/src/k8s.io/kubernetes`.

    ```
    go get -d k8s.io/kubernetes
    ```
   

2. Build a custom image and push it to a testing repository.
    ```
    IMAGE_REGISTRY=<username> make image
    docker push <username>/azure-cloud-controller-manager:<image_version>
    ```

3. Deploy a cluster and run smoke test

    To deploy a cluster:
    ```
    #Enter all of the details as in TestProfile
    export RESOURCE_GROUP_NAME=<resource group name>
    export LOCATION=<location>
    export SUBSCRIPTION_ID=<subscription ID>
    export CLIENT_ID=<client id>
    export CLIENT_SECRET=<client secret>
    export TENANT_ID=<tenant id>
    export USE_CSI_DEFAULT_STORAGECLASS=<true/false>
    make deploy
    ```

    To connect the cluster:
    ```
    cd tests/k8s-azure/manifest
    export KUBECONFIG=_output/kubeconfig/kubeconfig.<LOCATION>.json
    kubectl cluster-info
    ```
    To check out more of the deployed cluster , replace ``` kubectl cluster-info ``` with other `kubectl` commands.
    To further debug and diagnose cluster problems, use ```kubectl cluster-info dump```

4. Run E2E tests
    Please first ensure the kubernetes project locates at `$GOPATH/src/k8s.io/kubernetes`, the e2e tests will be built from that location.
    - Run test suite: default, serial, slow, smoke (smoke suite just tests the cluster is up and do not run any case)
        ```
        tests/k8s-azure/k8s-azure e2e -cname=$CLUSTER_NAME -ctype=<SuiteName> -cskipdeploy=1 -cbuild_e2e_test=1
        ```

        Option '-cbuild_e2e_test=1' tells it to build E2E tests, if the tests have been built, that option can be omitted.

    - Run custom test:
        ```
        CASE_NAME='<some name>'
        tests/k8s-azure/k8s-azure e2e -cname=$CLUSTER_NAME -ctype=custom -ccustom_tests=$CASE_NAME -cskipdeploy=1
        ```

## 2. With container

1. Prepare the environment variable file `<TestProfileWithContainer>`.
    ```
    # Azure tenant id
    K8S_AZURE_TENANTID=
    # Azure subscription id
    K8S_AZURE_SUBSID=
    # Azure service principal id
    K8S_AZURE_SPID=
    # Azure service principal secret
    K8S_AZURE_SPSEC=
    # SSH public key to be deployed to cluster
    K8S_AZURE_SSHPUB=
    # Azure location for the testing cluster
    K8S_AZURE_LOCATION=
    ```

2. Build the container image

    ```
    cd tests/k8s-azure
    docker build . -t k8s-azure:local
    docker run --env-file <TestProfileWithContainer> k8s-azure:local k8s-azure e2e
    ```
