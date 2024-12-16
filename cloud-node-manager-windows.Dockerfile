# syntax=docker/dockerfile:1

# Copyright 2020 The Kubernetes Authors.
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
ARG OSVERSION=1809
ARG ARCH=amd64
ARG LOCAL_BUILD=false

# NOTE(mainred): BuildKit-based builder will skip the used stage depending on the value LOCAL_BUILD.

# Build windows cloud node manager binary from golang build stage
FROM --platform=linux/amd64 mcr.microsoft.com/oss/go/microsoft/golang:1.23@sha256:f4fc81062796c14e704559cad3748c5db70bf961ef24d5fac798afa18dff300e AS local-build-false
ARG ENABLE_GIT_COMMAND=true
ARG ARCH
WORKDIR /go/src/sigs.k8s.io/cloud-provider-azure
COPY . .
# Build the Go app
RUN make bin/azure-cloud-node-manager.exe ENABLE_GIT_COMMAND=${ENABLE_GIT_COMMAND} ARCH=${ARCH}

# COPY the binary built locally to the builder container to normalize the following COPY behavior
FROM --platform=linux/amd64 mcr.microsoft.com/oss/go/microsoft/golang:1.23@sha256:f4fc81062796c14e704559cad3748c5db70bf961ef24d5fac798afa18dff300e AS local-build-true
WORKDIR /go/src/sigs.k8s.io/cloud-provider-azure
COPY . .
COPY bin/azure-cloud-node-manager-*.exe bin/

# mutli-stage dependency are determined before the build starts, so it's invalid to use command like
# `COPY --from=local-build-{LOCAL_BUILD}` to copy the binary from a dynamic source, so we create a
# normalized builder base as a workaround.
# Example of the error:
# ERROR: failed to solve: failed to parse stage name "local-build-{LOCAL_BUILD}": invalid reference format: repository name (library/local-build-{LOCAL_BUILD}) must be lowercase
FROM local-build-${LOCAL_BUILD} AS builder


# NOTE(claudiub): Instead of pulling the servercore image, which is ~2GB in side, we
# can instead pull the windows-servercore-cache image, which is only a few MBs in size.
# The image contains the netapi32.dll we need.
FROM --platform=linux/amd64 gcr.io/k8s-staging-e2e-test-images/windows-servercore-cache:1.0-linux-${ARCH}-$OSVERSION AS servercore-helper

FROM mcr.microsoft.com/windows/nanoserver:$OSVERSION

ARG ARCH

COPY --from=servercore-helper /Windows/System32/netapi32.dll /Windows/System32/netapi32.dll
COPY --from=builder /go/src/sigs.k8s.io/cloud-provider-azure/bin/azure-cloud-node-manager-${ARCH}.exe /cloud-node-manager.exe
USER ContainerUser
ENTRYPOINT ["/cloud-node-manager.exe"]
