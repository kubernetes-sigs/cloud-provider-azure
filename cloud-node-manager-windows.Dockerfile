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

# NOTE(claudiub): Instead of pulling the servercore image, which is ~2GB in side, we
# can instead pull the windows-servercore-cache image, which is only a few MBs in size.
# The image contains the netapi32.dll we need.
FROM --platform=linux/amd64 gcr.io/k8s-staging-e2e-test-images/windows-servercore-cache:1.0-linux-${ARCH}-$OSVERSION as servercore-helper

FROM mcr.microsoft.com/windows/nanoserver:$OSVERSION

ARG OSVERSION
ARG ARCH

COPY --from=servercore-helper /Windows/System32/netapi32.dll /Windows/System32/netapi32.dll
COPY bin/azure-cloud-node-manager-${ARCH}.exe /cloud-node-manager.exe
USER ContainerUser
ENTRYPOINT ["/cloud-node-manager.exe"]
