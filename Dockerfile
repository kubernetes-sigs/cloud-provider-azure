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

FROM --platform=linux/amd64 mcr.microsoft.com/oss/go/microsoft/golang:1.24.11-bookworm@sha256:911d1e831f87d39ac8e2c1a536ca02284e7cce1d97f33f6e0311be453e193d58 AS builder

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

FROM gcr.io/distroless/base:latest@sha256:d605e138bb398428779e5ab490a6bbeeabfd2551bd919578b1044718e5c30798
COPY --from=builder /go/src/sigs.k8s.io/cloud-provider-azure/bin/azure-cloud-controller-manager /usr/local/bin/cloud-controller-manager
ENTRYPOINT [ "/usr/local/bin/cloud-controller-manager" ]
