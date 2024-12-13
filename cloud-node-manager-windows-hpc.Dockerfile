# syntax=docker/dockerfile:1

# Copyright 2024 The Kubernetes Authors.
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
ARG ARCH=amd64

# build windows cloud noder manager binary
FROM --platform=linux/amd64 mcr.microsoft.com/oss/go/microsoft/golang:1.23@sha256:f4fc81062796c14e704559cad3748c5db70bf961ef24d5fac798afa18dff300e AS builder
ARG ENABLE_GIT_COMMAND=true
ARG ARCH
WORKDIR /go/src/sigs.k8s.io/cloud-provider-azure
COPY . .
RUN make bin/azure-cloud-node-manager.exe ENABLE_GIT_COMMAND=${ENABLE_GIT_COMMAND} ARCH=${ARCH}

FROM mcr.microsoft.com/oss/kubernetes/windows-host-process-containers-base-image:v1.0.0
ARG ARCH
COPY --from=builder /go/src/sigs.k8s.io/cloud-provider-azure/bin/azure-cloud-node-manager-${ARCH}.exe /cloud-node-manager.exe
ENTRYPOINT ["/cloud-node-manager.exe"]