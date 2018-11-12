FROM golang:1.11.2-stretch AS build
WORKDIR /go/src/k8s.io/cloud-provider-azure
COPY . .
RUN make

FROM buildpack-deps:stretch-curl
COPY --from=build /go/src/k8s.io/cloud-provider-azure/bin/azure-cloud-controller-manager /usr/local/bin
RUN ln -s /usr/local/bin/azure-cloud-controller-manager /usr/local/bin/cloud-controller-manager
