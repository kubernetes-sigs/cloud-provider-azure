# client-gen

## typescaffold

```shell
typescaffold --package github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v9 --package-alias network --resource PrivateLinkService --client-name PrivateLinkServicesClient 
```

### client-gen

```shell
client-gen clientgen:headerFile=../../../hack/boilerplate/boilerplate.gomock.txt paths=./...
```

Clients with handwritten ETag coverage can set `skipEtagTest=true` in the
`+azure:client` marker to omit the generic invalid-ETag test. This does not
disable the `etag=true` client policy. The Interface client uses this option
for its offline conditional-write regressions and live-only NRP checks.