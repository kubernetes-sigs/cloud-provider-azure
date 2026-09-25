# client-gen

## typescaffold

```shell
typescaffold --package github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9 --package-alias network --resource PrivateLinkService --client-name PrivateLinkServicesClient 
```

### client-gen

```shell
client-gen clientgen:headerFile=../../../hack/boilerplate/boilerplate.gomock.txt paths=./...
```

Clients marked `etag=true` generate a conditional-write test that asserts the
outgoing `If-Match` header through the real SDK pipeline. It uses a fake
credential and intercepts the request before HTTP transport, so this assertion
does not depend on Azure credentials or recorded responses.