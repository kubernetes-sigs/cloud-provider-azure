# Copyright 2022 The Kubernetes Authors.
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

# get the repo root and output path
REPO_ROOT:=$(shell pwd)
export REPO_ROOT
OUT_DIR=$(REPO_ROOT)/bin
# record the source commit in the binary, overridable
COMMIT?=$(shell git describe --tags --always --dirty 2>/dev/null)
INSTALL?=install
# make install will place binaries here
INSTALL_DIR?=$(GOPATH)/bin
# the output binary name, overridden when cross compiling
BINARY_NAME?=kubetest2-aks
BINARY_PATH?=.
# the container cli to use e.g. docker,podman
DOCKER?=$(shell which docker || which podman || echo "docker")
export DOCKER
# ========================= Setup Go With Gimme ================================
# go version to use for build etc.
# setup correct go version with gimme
# PATH:=$(shell . hack/build/setup-go.sh && echo "$${PATH}")
# go1.9+ can autodetect GOROOT, but if some other tool sets it ...
GOROOT:=
# enable modules
GO111MODULE=on
# disable CGO by default for static binaries
CGO_ENABLED=0
export PATH GOROOT GO111MODULE CGO_ENABLED
# work around broken PATH export
SPACE:=$(subst ,, )
SHELL:=env PATH=$(subst $(SPACE),\$(SPACE),$(PATH)) $(SHELL)
# ==============================================================================
# flags for reproducible go builds
BUILD_FLAGS?=-trimpath -ldflags="-buildid="

.PHONY: deployer
deployer:
	BINARY_PATH=.
	BINARY_NAME=kubetest2-aks
	BUILD_FLAGS='-trimpath -ldflags="-buildid= -X=sigs.k8s.io/cloud-provider-azure/kubetest2-aks/deployer.GitTag=$(COMMIT)"'
	go build -v $(BUILD_FLAGS) -o $(OUT_DIR)/$(BINARY_NAME) $(BINARY_PATH)

.PHONY: install
install:
	BINARY_NAME=kubetest2-aks
	$(INSTALL) -d $(INSTALL_DIR)
	$(INSTALL) $(OUT_DIR)/$(BINARY_NAME) $(INSTALL_DIR)/$(BINARY_NAME)

.PHONY: install-deployer
install-deployer: deployer install
