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

.DELETE_ON_ERROR:

SHELL=/bin/bash -o pipefail
BIN_DIR=bin
PKG_CONFIG=.pkg_config
PKG_CONFIG_CONTENT=$(shell cat $(PKG_CONFIG))

# TODO: fix code and enable more options
# -E deadcode -E gocyclo -E vetshadow -E gas -E ineffassign
GOMETALINTER_OPTION=--tests --disable-all -E gofmt -E vet -E golint -e "don't use underscores in Go names"

AKSENGINE_VERSION ?= master

TEST_RESULTS_DIR=testResults
# manifest name under tests/e2e/k8s-azure/manifest
TEST_MANIFEST ?= linux
# build hyperkube image when specified
K8S_BRANCH ?=
# Only run conformance tests by default (non-serial and non-slow)
# Note autoscaling tests would be skiped as well.
CCM_E2E_ARGS ?= -ginkgo.skip=\\[Serial\\]\\[Slow\\]
#The test args for Kubernetes e2e tests
TEST_E2E_ARGS ?= '--ginkgo.focus=Port\sforwarding'

IMAGE_REGISTRY ?= local
STAGING_REGISTRY := gcr.io/k8s-staging-provider-azure
K8S_VERSION ?= v1.18.0-rc.1
HYPERKUBE_IMAGE ?= gcrio.azureedge.net/google_containers/hyperkube-amd64:$(K8S_VERSION)

ifndef TAG
	IMAGE_TAG ?= $(shell git rev-parse --short=7 HEAD)
else
	IMAGE_TAG ?= $(TAG)
endif

# cloud controller manager image
IMAGE_NAME=azure-cloud-controller-manager
IMAGE=$(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
# cloud node manager image
NODE_MANAGER_IMAGE_NAME=azure-cloud-node-manager
NODE_MANAGER_IMAGE=$(IMAGE_REGISTRY)/$(NODE_MANAGER_IMAGE_NAME):$(IMAGE_TAG)
NODE_MANAGER_WINDOWS_IMAGE_NAME=azure-cloud-node-manager-windows
NODE_MANAGER_WINDOWS_IMAGE=$(IMAGE_REGISTRY)/$(NODE_MANAGER_WINDOWS_IMAGE_NAME):$(IMAGE_TAG)
# ccm e2e test image
CCM_E2E_TEST_IMAGE_NAME=cloud-provider-azure-e2e
CCM_E2E_TEST_IMAGE=$(IMAGE_REGISTRY)/$(CCM_E2E_TEST_IMAGE_NAME):$(IMAGE_TAG)
CCM_E2E_TEST_RELEASE_IMAGE=docker.pkg.github.com/kubernetes-sigs/cloud-provider-azure/cloud-provider-azure-e2e:$(IMAGE_TAG)

# Bazel variables
BAZEL_VERSION := $(shell command -v bazel 2> /dev/null)
BAZEL_ARGS ?=

## --------------------------------------
## Binaries
## --------------------------------------

.PHONY: all
all: $(BIN_DIR)/azure-cloud-controller-manager $(BIN_DIR)/azure-cloud-node-manager $(BIN_DIR)/azure-cloud-node-manager.exe

$(BIN_DIR)/azure-cloud-node-manager: $(PKG_CONFIG) $(wildcard cmd/cloud-node-manager/*) $(wildcard cmd/cloud-node-manager/**/*) $(wildcard pkg/**/*)
	CGO_ENABLED=0 GOOS=linux go build -a -o $@ $(PKG_CONFIG_CONTENT) ./cmd/cloud-node-manager

$(BIN_DIR)/azure-cloud-node-manager.exe: $(PKG_CONFIG) $(wildcard cmd/cloud-node-manager/*) $(wildcard cmd/cloud-node-manager/**/*) $(wildcard pkg/**/*)
	CGO_ENABLED=0 GOOS=windows go build -a -o $@ $(PKG_CONFIG_CONTENT) ./cmd/cloud-node-manager

$(BIN_DIR)/azure-cloud-controller-manager: $(PKG_CONFIG) $(wildcard cmd/cloud-controller-manager/*) $(wildcard cmd/cloud-controller-manager/**/*) $(wildcard pkg/**/*)
	CGO_ENABLED=0 GOOS=linux go build -a -o $@ $(PKG_CONFIG_CONTENT) ./cmd/cloud-controller-manager

## --------------------------------------
## Images
## --------------------------------------

.PHONY: build-ccm-image
build-ccm-image:
	docker build -t $(IMAGE) --build-arg ENABLE_GIT_COMMAND=$(ENABLE_GIT_COMMAND) .

.PHONY: build-node-image
build-node-image:
	docker build -t $(NODE_MANAGER_IMAGE) -f cloud-node-manager.Dockerfile --build-arg ENABLE_GIT_COMMAND=$(ENABLE_GIT_COMMAND) .

.PHONY: build-node-image-windows
build-node-image-windows:
	go build -a -o $(BIN_DIR)/azure-cloud-node-manager.exe ./cmd/cloud-node-manager
	docker build --platform windows/amd64 -t $(NODE_MANAGER_WINDOWS_IMAGE) -f cloud-node-manager-windows.Dockerfile .

.PHONY: build-ccm-e2e-test-image
build-ccm-e2e-test-image:
	docker build -t $(CCM_E2E_TEST_IMAGE) -f ./e2e.Dockerfile .

.PHONY: build-images
build-images: build-ccm-image build-node-image

.PHONY: image
image: build-ccm-image build-node-image

.PHONY: push-ccm-image
push-ccm-image:
	docker push $(IMAGE)

.PHONY: push-node-image
push-node-image:
	docker push $(NODE_MANAGER_IMAGE)

.PHONY: push-node-image-windows
push-node-image-windows:
	docker push $(NODE_MANAGER_WINDOWS_IMAGE)

.PHONY: push
push: push-ccm-image push-node-image

.PHONY: push-images
push-images: push-ccm-image push-node-image

.PHONY: release-ccm-e2e-test-image
release-ccm-e2e-test-image:
	docker build -t $(CCM_E2E_TEST_RELEASE_IMAGE) -f ./e2e.Dockerfile .
	docker push $(CCM_E2E_TEST_RELEASE_IMAGE)

hyperkube:
ifneq ($(K8S_BRANCH), )
	$(eval K8S_VERSION=$(shell REGISTRY=$(IMAGE_REGISTRY) BRANCH=$(K8S_BRANCH) hack/build-hyperkube.sh))
	$(eval HYPERKUBE_IMAGE=$(IMAGE_REGISTRY)/hyperkube-amd64:$(K8S_VERSION))
endif

## --------------------------------------
## Tests
## --------------------------------------

.PHONY: test-unit
test-unit: $(PKG_CONFIG)
	mkdir -p $(TEST_RESULTS_DIR)
	hack/test-unit.sh | tee -a $(TEST_RESULTS_DIR)/unittest.txt
ifdef JUNIT
	hack/convert-test-report.pl $(TEST_RESULTS_DIR)/unittest.txt > $(TEST_RESULTS_DIR)/unittest.xml
endif

.PHONY: test-check
test-check: test-lint test-boilerplate test-spelling test-gofmt test-govet

.PHONY: test-gofmt
test-gofmt:
	hack/verify-gofmt.sh

.PHONY: test-govet
test-govet:
	hack/verify-govet.sh

.PHONY: test-lint
test-lint:
	hack/verify-golint.sh

.PHONY: test-boilerplate
test-boilerplate:
	hack/verify-boilerplate.sh

.PHONY: test-spelling
test-spelling:
	hack/verify-spelling.sh

.PHONY: test-bazel
test-bazel:
	hack/verify-bazel.sh

.PHONY: update-dependencies
update-dependencies:
	hack/update-dependencies.sh

.PHONY: update-bazel
update-bazel:
	hack/update-bazel.sh

.PHONY: update-gofmt
update-gofmt:
	hack/update-gofmt.sh

.PHONY: update
update: update-dependencies update-bazel update-gofmt

test-e2e:
	hack/test_k8s_e2e.sh $(TEST_E2E_ARGS)

test-ccm-e2e:
	go test ./tests/e2e/ -timeout 0 -v -ginkgo.v $(CCM_E2E_ARGS)

.PHONY: bazel-build
bazel-build:
# check if bazel exists
ifndef BAZEL_VERSION
	$(error "Bazel is not available. Installation instructions can be found at https://docs.bazel.build/versions/master/install.html")
endif
	bazel build //cmd/cloud-controller-manager $(BAZEL_ARGS)

.PHONY: bazel-clean
bazel-clean:
ifndef BAZEL_VERSION
	$(error "Bazel is not available. Installation instructions can be found at https://docs.bazel.build/versions/master/install.html")
endif
	bazel clean

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) $(PKG_CONFIG) $(TEST_RESULTS_DIR)
	$(MAKE) bazel-clean

$(PKG_CONFIG):
	ENABLE_GIT_COMMANDS=$(ENABLE_GIT_COMMAND) hack/pkg-config.sh > $@

## --------------------------------------
## Release
## --------------------------------------

.PHONY: deploy
deploy: image push
	IMAGE=$(IMAGE) HYPERKUBE_IMAGE=$(HYPERKUBE_IMAGE) hack/deploy-cluster.sh

.PHONY: release-staging
release-staging:
	ENABLE_GIT_COMMANDS=false IMAGE_REGISTRY=$(STAGING_REGISTRY) $(MAKE) build-images push-images
