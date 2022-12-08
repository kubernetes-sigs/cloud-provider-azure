# syntax=docker/dockerfile:1.1-experimental

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

FROM --platform=linux/amd64 golang:1.19.4-buster AS builder

ARG ENABLE_GIT_COMMAND=true
ARG ARCH=amd64

WORKDIR /go/src/sigs.k8s.io/cloud-provider-azure
COPY . .

RUN make bin/azure-cloud-node-manager ENABLE_GIT_COMMAND=${ENABLE_GIT_COMMAND}

FROM gcr.io/distroless/static
COPY --from=builder /go/src/sigs.k8s.io/cloud-provider-azure/bin/azure-cloud-node-manager /usr/local/bin/cloud-node-manager
ENTRYPOINT [ "/usr/local/bin/cloud-node-manager" ]
