# client-gen

## typescaffold

```shell
typescaffold --package github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v3 --package-alias network --resource PrivateLinkService --client-name PrivateLinkServicesClient --apiversion 2021-08-01
```

### client-gen

```shell
client-gen clientgen:headerFile=../../../hack/boilerplate/boilerplate.gomock.txt paths=./...
```