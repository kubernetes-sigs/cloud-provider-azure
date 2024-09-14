# client-gen

## typescaffold

```shell
typescaffold --package github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6 --package-alias network --resource PrivateLinkService --client-name PrivateLinkServicesClient 
```

### client-gen

```shell
client-gen clientgen:headerFile=../../../hack/boilerplate/boilerplate.gomock.txt paths=./...
```