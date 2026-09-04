# Copyright 2023 The Kubernetes Authors.
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

FROM --platform=linux/amd64 mcr.microsoft.com/oss/go/microsoft/golang:1.26.5-2-bookworm@sha256:ea712a1aaf80306c19ff842ba0bfcb9ad360afd8143e70044e0d0bd6d6899887 AS builder

ARG ARCH=amd64

WORKDIR /go/src/health-probe-proxy
COPY . .

# Cache the go build into the Go compiler cache folder so we benefit from compiler caching across docker build calls
RUN --mount=type=cache,target=/root/.cache/go-build \
    make bin/health-probe-proxy ARCH=${ARCH}

FROM gcr.io/distroless/base:latest@sha256:f4a335ca209e1d2ee873102c17c389ad0142e3d5b21aee2817e9cc9c01d87d20
COPY --from=builder /go/src/health-probe-proxy/bin/health-probe-proxy /usr/local/bin/health-probe-proxy
ENTRYPOINT [ "/usr/local/bin/health-probe-proxy" ]
