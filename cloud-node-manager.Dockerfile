# syntax=docker/dockerfile:1

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

FROM --platform=linux/amd64 mcr.microsoft.com/oss/go/microsoft/golang:1.24-bookworm@sha256:1bdcf4a46716b0a27fc6e9ec57cdfe585ccb0201aed9965cdf4e7ecfce5fea85 AS builder

ARG ENABLE_GIT_COMMAND=true
ARG ARCH=amd64

RUN if [ "$ARCH" = "arm64" ] ; then \
    apt-get update && apt-get install -y gcc-aarch64-linux-gnu ; \
    elif [ "$ARCH" = "arm" ] ; then \
    apt-get update && apt-get install -y gcc-arm-linux-gnueabihf ; \
    fi

WORKDIR /go/src/sigs.k8s.io/cloud-provider-azure
COPY . .

# Build the Go app
RUN make bin/azure-cloud-node-manager ENABLE_GIT_COMMAND=${ENABLE_GIT_COMMAND} ARCH=${ARCH}

# Use distroless base image for a lean production container.
# Start a new build stage.
FROM gcr.io/distroless/base:latest@sha256:27769871031f67460f1545a52dfacead6d18a9f197db77110cfc649ca2a91f44

# Create a group and user
USER 65532:65532

# Copy the pre-built binary file from the previous stage.
COPY --from=builder /go/src/sigs.k8s.io/cloud-provider-azure/bin/azure-cloud-node-manager /usr/local/bin/cloud-node-manager

# Run the web service on container startup.
ENTRYPOINT [ "/usr/local/bin/cloud-node-manager" ]
