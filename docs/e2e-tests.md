# E2E tests

## Prerequisite
- An azure service principal

    Please follow this [guide](https://github.com/Azure/acs-engine/blob/v0.14.0/docs/serviceprincipal.md) for creating an azure service principal
    The service principal should either have:
    - Contributor permsision of a subscription
    - Contributor permission of a resource group. In this case, please create the resource group first

## How to run E2E test locally

### 1. Without container

1. Prepare dependency project
- [acs-engine](https://github.com/Azure/acs-engine)

    Build acs-engine, and make acs-engine binary in PATH environment variable.

    ```
    go get -d github.com/Azure/acs-engine
    pushd $GOPATH/src/github.com/Azure/acs-engine
    make
    popd
    export PATH=$PATH:$GOPATH/src/github.com/Azure/acs-engine/bin
    ```

- [Kubernetes](https://github.com/kubernetes/kubernetes)

    This serves as E2E tests case source, it should be located at `$GOPATH/src/k8s.io/kubernetes`.

    ```
    go get -d k8s.io/kubernetes
    ```

2. Fill in following profile file and save it somewhere. This file will be referred as `<TestProfile>` in following steps.

    ```
    # Azure tenant id
    export K8S_AZURE_TENANTID=
    # Azure subscription id
    export K8S_AZURE_SUBSID=
    # Azure service principal id
    export K8S_AZURE_SPID=
    # Azure service principal secret
    export K8S_AZURE_SPSEC=
    # SSH public key to be deployed to cluster
    export K8S_AZURE_SSHPUB=
    # Azure location for the testing cluster
    export K8S_AZURE_LOCATION=
    ```

3. Build custom image
    Build a custom image and push it to a testing repository.
    ```
    K8S_AZURE_IMAGE_REPOSITORY=<username> make image
    docker push <username>/azure-cloud-controller-manager:<image_version>
    ```

4. Deploy a cluster and run smoke test
    ```
    source <TestProfile>
    CLUSTER_NAME=<ClusterName>
    tests/k8s-azure/k8s-azure e2e -cname=$CLUSTER_NAME -cbuild_e2e_test=1 -caccm_image=<username>/azure-cloud-controller-manager:<image_version>
    ```

    To connect the cluster:
    ```
    source $CLUSTER_NAME/cluster.profile
    kubectl version
    ```

5. Run E2E tests
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
