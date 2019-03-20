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

FROM golang:1.12.1-stretch AS build
WORKDIR /go/src/k8s.io/cloud-provider-azure
COPY . .
RUN make

FROM buildpack-deps:stretch-curl
COPY --from=build /go/src/k8s.io/cloud-provider-azure/bin/azure-cloud-controller-manager /usr/local/bin
RUN ln -s /usr/local/bin/azure-cloud-controller-manager /usr/local/bin/cloud-controller-manager
