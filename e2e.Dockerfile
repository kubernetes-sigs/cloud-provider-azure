FROM golang:1.18.1-buster

WORKDIR /go/src/sigs.k8s.io/cloud-provider-azure

COPY . .

RUN go get github.com/onsi/ginkgo/ginkgo \
  && go get github.com/onsi/gomega/... \
  && go mod tidy
