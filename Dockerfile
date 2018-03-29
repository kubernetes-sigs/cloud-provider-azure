FROM golang:1.9.3-stretch AS build
WORKDIR /go/src/github.com/kubernetes/cloud-provider-azure
COPY . .
RUN make

FROM buildpack-deps:stretch-curl
COPY --from=build /go/src/github.com/kubernetes/cloud-provider-azure/bin/azure-cloud-controller-manager /usr/local/bin
RUN ln -s /usr/local/bin/azure-cloud-controller-manager /usr/local/bin/cloud-controller-manager
