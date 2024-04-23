# kubetest2-aks repo to provision and delete aks clusters

## Commands

Make kubetest2-aks binary under kubetest2-aks folder.

```shell
make install-deployer
```

Build CCM images with target path or tags. Note: users should login the registry.

```shell
export IMAGE_REGISTRY=<user-image-registry>

kubetest2 aks --build --target ccm --targetPath ../cloud-provider-azure
kubetest2 aks --build --target ccm --targetPath --targetTag v1.24.4
```

Provision an aks cluster in a resource group.

```shell
export AZURE_SUBSCRIPTION_ID=<subscription-id>
export AZURE_TENANT_ID=<tenant-id>
export IMAGE_REGISTRY=<user-image-registry>
# Leaving `AZURE_CLIENT_SECRET` and `AZURE_CLIENT_ID` unset will use MSI by default.
export AZURE_CLIENT_ID=<client-id>
export AZURE_CLIENT_SECRET=<client-secret>
kubetest2 aks --up --rgName aks-resource-group --location eastus --config cluster-templates/basic-lb.json --customConfig cluster-templates/customconfiguration.json  --clusterName aks-cluster --ccmImageTag abcdefg --k8sVersion 1.24.0
```

Delete the resource group.

```shell
kubetest2 aks --down --rgName aks-resource-group --clusterName aks-cluster
```
