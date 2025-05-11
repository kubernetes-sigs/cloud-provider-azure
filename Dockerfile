# Copyright 2019 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# syntax=docker/dockerfile:1

FROM --platform=linux/amd64 mcr.microsoft.com/oss/go/microsoft/golang:1.24.3-bookworm@sha256:d9e8acf8d7fdb518c144fff40ce9cd7edd1c0fa7b0c7bdcb076d388a1ce6bad1 AS builder

ARG ENABLE_GIT_COMMAND=true
ARG ARCH=amd64

RUN if [ "$ARCH" = "arm64" ] ; then \
    apt-get update && apt-get install -y gcc-aarch64-linux-gnu ; \
    elif [ "$ARCH" = "arm" ] ; then \
    apt-get update && apt-get install -y gcc-arm-linux-gnueabihf ; \
    fi

WORKDIR /go/src/sigs.k8s.io/cloud-provider-azure
COPY . .

# Cache the go build into the the Go's compiler cache folder so we take benefits of compiler caching across docker build calls
RUN --mount=type=cache,target=/root/.cache/go-build \
    make bin/azure-cloud-controller-manager ENABLE_GIT_COMMAND=${ENABLE_GIT_COMMAND} ARCH=${ARCH}

FROM gcr.io/distroless/base:latest@sha256:27769871031f67460f1545a52dfacead6d18a9f197db77110cfc649ca2a91f44
COPY --from=builder /go/src/sigs.k8s.io/cloud-provider-azure/bin/azure-cloud-controller-manager /usr/local/bin/cloud-controller-manager
ENTRYPOINT [ "/usr/local/bin/cloud-controller-manager" ]
