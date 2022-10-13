---
title: "Kubernetes E2E tests"
linkTitle: "Kubernetes E2E tests"
type: docs
description: >
    Kubernetes E2E tests guidance.
---

## Prerequisite

- An azure service principal

    Please follow this [guide](https://github.com/kubernetes-sigs/cluster-api-provider-azure/blob/main/docs/book/src/topics/getting-started.md#setting-up-your-azure-environment) for creating an azure service principal
    The service principal should either have:
    - Contributor permission of a subscription
    - Contributor permission of a resource group. In this case, please create the resource group first

- Docker daemon enabled

## How to run Kubernetes e2e tests locally

1. Prepare dependency project

- [kubectl](https://kubectl.docs.kubernetes.io/)

  Kubectl allows you to run command against Kubernetes cluster, which is also used for deploying CSI plugins. You can follow [here](https://kubernetes.io/docs/tasks/tools/install-kubectl/#install-kubectl-binary-with-curl) to install kubectl. e.g. on Linux

  ```sh
  curl -LO https://storage.googleapis.com/kubernetes-release/release/$(curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt)/bin/linux/amd64/kubectl
  chmod +x kubectl
  sudo mv kubectl /usr/local/bin/
  ```

2. Build docker images `azure-cloud-controller-manager`, `azure-cloud-node-manager` and push them to your image repository.

    ```sh
    git clone https://github.com/kubernetes-sigs/cloud-provider-azure $GOPATH/src/sigs.k8s.io/cloud-provider-azure
    cd $GOPATH/src/sigs.k8s.io/cloud-provider-azure
    export IMAGE_REGISTRY=<your-registry>
    export IMAGE_TAG=<tag>
    make image # build all images of different ARCHs and OSes
    make push # push all images of different ARCHs and OSes to your registry. Or manually `docker push`
    ```

3. Deploy a Kubernetes cluster with the above `azure-cloud-controller-manager` and `azure-cloud-node-manager` images.

   To deploy a cluster, export all the required environmental variables first and then invoke `make deploy-cluster`.
   Please notice that [cluster-api-provider-azure](https://github.com/kubernetes-sigs/cluster-api-provider-azure) is
   used to provision the management and workload clusters. To learn more about this provisioner, you can refer to
   its [quick-start](https://cluster-api.sigs.k8s.io/user/quick-start.html) doc.

    ```sh
    export AZURE_SUBSCRIPTION_ID=<subscription-id>
    export AZURE_TENANT_ID=<tenant-id>
    export AZURE_CLIENT_ID=<client-id>
    export AZURE_CLIENT_SECRET=<client-secret>
    export CLUSTER_NAME=<cluster-name>
    export AZURE_RESOURCE_GROUP=<resource-group>
    export AZURE_CLOUD_CONTROLLER_MANAGER_IMG=<cloud-controller-manager-image>
    export AZURE_CLOUD_NODE_MANAGER_IMG=<cloud-node-manager-image>

    make deploy-cluster
    ```

   To connect the cluster:

    ```sh
    export KUBECONFIG=$GOPATH/src/sigs.k8s.io/cloud-provider-azure/$CLUSTER_NAME-kubeconfig
    kubectl cluster-info
    ```

   To check out more of the deployed cluster , replace `kubectl cluster-info` with other `kubectl` commands. To further debug and diagnose cluster problems, use `kubectl cluster-info dump`

4. Run Kubernetes E2E tests

   ```sh
   make test-e2e-capz
   ```
