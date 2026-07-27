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

FROM mcr.microsoft.com/oss/go/microsoft/golang:1.26.5-bookworm@sha256:6617edd08296ac9f66719ae5ee239ca1018951b9a25af3efa6f084ba0186ca30

WORKDIR /go/src/sigs.k8s.io/cloud-provider-azure

COPY . .

RUN go get github.com/onsi/ginkgo/ginkgo \
  && go get github.com/onsi/gomega/... \
  && go mod tidy
