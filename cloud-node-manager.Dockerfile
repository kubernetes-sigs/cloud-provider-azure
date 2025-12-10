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

FROM --platform=linux/amd64 mcr.microsoft.com/oss/go/microsoft/golang:1.24.11-bookworm@sha256:911d1e831f87d39ac8e2c1a536ca02284e7cce1d97f33f6e0311be453e193d58 AS builder

ARG ENABLE_GIT_COMMAND=true
ARG ARCH=amd64

WORKDIR /go/src/sigs.k8s.io/cloud-provider-azure
COPY . .

# Build the Go app
RUN make bin/azure-cloud-node-manager ENABLE_GIT_COMMAND=${ENABLE_GIT_COMMAND}

# Use distroless static image for a lean production container.
# Start a new build stage.
FROM gcr.io/distroless/static:latest@sha256:87bce11be0af225e4ca761c40babb06d6d559f5767fbf7dc3c47f0f1a466b92c

# Create a group and user
USER 65532:65532

# Copy the pre-built binary file from the previous stage.
COPY --from=builder /go/src/sigs.k8s.io/cloud-provider-azure/bin/azure-cloud-node-manager /usr/local/bin/cloud-node-manager

# Run the web service on container startup.
ENTRYPOINT [ "/usr/local/bin/cloud-node-manager" ]
