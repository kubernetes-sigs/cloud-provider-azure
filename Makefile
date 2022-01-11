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

ARCH ?= amd64
# golang/strecth supports only amd64, arm32v7, arm64v8 and i386, before we have an
# explicit requirement for other arch support, let's keep golang/stretch
# https://github.com/docker-library/official-images/blob/master/library/golang
LINUX_ARCHS = amd64 arm arm64

# The output type for `docker buildx build` could either be docker (local), or registry.
OUTPUT_TYPE ?= docker

AKSENGINE_VERSION ?= master
ENABLE_GIT_COMMAND ?= true
TEST_RESULTS_DIR=testResults
# manifest name under tests/e2e/k8s-azure/manifest
TEST_MANIFEST ?= linux
# build hyperkube image when specified
K8S_BRANCH ?=
# Only run conformance tests by default (non-serial and non-slow)
# Note autoscaling tests would be skipped as well.
CCM_E2E_ARGS ?= -ginkgo.skip=\\[Serial\\]\\[Slow\\]
#The test args for Kubernetes e2e tests
TEST_E2E_ARGS ?= '--ginkgo.focus=Port\sforwarding'

IMAGE_REGISTRY ?= local
STAGING_REGISTRY := gcr.io/k8s-staging-provider-azure
K8S_VERSION ?= v1.18.0-rc.1
HYPERKUBE_IMAGE ?= gcrio.azureedge.net/google_containers/hyperkube-amd64:$(K8S_VERSION)

# The OS Version for the Windows images: 1809, 2004, 20H2, ltsc2022
WINDOWS_OSVERSION ?= 1809
ALL_WINDOWS_OSVERSIONS = 1809 2004 20H2 ltsc2022
BASE.windows := mcr.microsoft.com/windows/nanoserver

# `docker buildx` and `docker manifest` requires enabling DOCKER_CLI_EXPERIMENTAL for docker version < 1.20
export DOCKER_CLI_EXPERIMENTAL=enabled

ifndef TAG
	IMAGE_TAG ?= $(shell git rev-parse --short=7 HEAD)
else
	IMAGE_TAG ?= $(TAG)
endif

DOCKER_CLI_EXPERIMENTAL := enabled

# cloud controller manager image
ifeq ($(ARCH), amd64)
IMAGE_NAME=azure-cloud-controller-manager
else
IMAGE_NAME=azure-cloud-controller-manager-$(ARCH)
endif
IMAGE=$(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
# cloud node manager image
NODE_MANAGER_IMAGE_NAME=azure-cloud-node-manager
NODE_MANAGER_LINUX_IMAGE_NAME=azure-cloud-node-manager-linux
NODE_MANAGER_WINDOWS_IMAGE_NAME=azure-cloud-node-manager-windows
NODE_MANAGER_IMAGE=$(IMAGE_REGISTRY)/$(NODE_MANAGER_IMAGE_NAME):$(IMAGE_TAG)
NODE_MANAGER_LINUX_FULL_IMAGE=$(IMAGE_REGISTRY)/$(NODE_MANAGER_LINUX_IMAGE_NAME)
NODE_MANAGER_WINDOWS_IMAGE=$(IMAGE_REGISTRY)/$(NODE_MANAGER_WINDOWS_IMAGE_NAME):$(IMAGE_TAG)

ALL_NODE_MANAGER_IMAGES = $(foreach arch, ${LINUX_ARCHS}, $(NODE_MANAGER_LINUX_FULL_IMAGE):$(IMAGE_TAG)-${arch}) $(foreach osversion, ${ALL_WINDOWS_OSVERSIONS}, $(NODE_MANAGER_WINDOWS_IMAGE)-${osversion})
ALL_LINUX_NODE_MANAGER_IMAGES = $(foreach arch, ${LINUX_ARCHS}, $(NODE_MANAGER_LINUX_FULL_IMAGE):$(IMAGE_TAG)-${arch})


# ccm e2e test image
CCM_E2E_TEST_IMAGE_NAME=cloud-provider-azure-e2e
CCM_E2E_TEST_IMAGE=$(IMAGE_REGISTRY)/$(CCM_E2E_TEST_IMAGE_NAME):$(IMAGE_TAG)
CCM_E2E_TEST_RELEASE_IMAGE=docker.pkg.github.com/kubernetes-sigs/cloud-provider-azure/cloud-provider-azure-e2e:$(IMAGE_TAG)


##@ General

help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[.a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

## --------------------------------------
##@ Binaries
## --------------------------------------

.PHONY: all
all: $(BIN_DIR)/azure-cloud-controller-manager $(BIN_DIR)/azure-cloud-node-manager $(BIN_DIR)/azure-cloud-node-manager.exe ## Build binaries for the project.

$(BIN_DIR)/azure-cloud-node-manager: $(PKG_CONFIG) $(wildcard cmd/cloud-node-manager/*) $(wildcard cmd/cloud-node-manager/**/*) $(wildcard pkg/**/*) ## Build node-manager binary for Linux.
	CGO_ENABLED=0 GOOS=linux GOARCH=${ARCH} go build -a -o $(BIN_DIR)/azure-cloud-node-manager $(shell cat $(PKG_CONFIG)) ./cmd/cloud-node-manager

$(BIN_DIR)/azure-cloud-node-manager.exe: $(PKG_CONFIG) $(wildcard cmd/cloud-node-manager/*) $(wildcard cmd/cloud-node-manager/**/*) $(wildcard pkg/**/*) ## Build node-manager binary for Windows.
	CGO_ENABLED=0 GOOS=windows go build -a -o $(BIN_DIR)/azure-cloud-node-manager.exe $(shell cat $(PKG_CONFIG)) ./cmd/cloud-node-manager

$(BIN_DIR)/azure-cloud-controller-manager: $(PKG_CONFIG) $(wildcard cmd/cloud-controller-manager/*) $(wildcard cmd/cloud-controller-manager/**/*) $(wildcard pkg/**/*) ## Build binary for controller-manager.
	CGO_ENABLED=0 GOOS=linux GOARCH=${ARCH} go build -a -o $(BIN_DIR)/azure-cloud-controller-manager $(shell cat $(PKG_CONFIG)) ./cmd/cloud-controller-manager

## --------------------------------------
##@ Images
## --------------------------------------

.PHONY: docker-pull-prerequisites
docker-pull-prerequisites: ## Pull prerequisite images.
	docker pull docker/dockerfile:1.3.1
	docker pull docker.io/library/golang:1.17-buster
	docker pull gcr.io/distroless/static:latest

buildx-setup:
	docker buildx inspect img-builder > /dev/null || docker buildx create --name img-builder --use
	# enable qemu for arm64 build
	# https://github.com/docker/buildx/issues/464#issuecomment-741507760
	docker run --privileged --rm tonistiigi/binfmt --uninstall qemu-aarch64
	docker run --rm --privileged tonistiigi/binfmt --install all

.PHONY: build-ccm-image
build-ccm-image: buildx-setup docker-pull-prerequisites ## Build controller-manager image.
	docker buildx build \
		--pull \
		--output=type=$(OUTPUT_TYPE) \
		--platform linux/$(ARCH) \
		--build-arg ENABLE_GIT_COMMAND="$(ENABLE_GIT_COMMAND)" \
		--build-arg ARCH="$(ARCH)" \
		--build-arg VERSION="$(VERSION)" \
		--file Dockerfile \
		--tag $(IMAGE) .

.PHONY: build-node-image
build-node-image: buildx-setup docker-pull-prerequisites ## Build node-manager image.
	docker buildx build \
		--pull \
		--output=type=$(OUTPUT_TYPE) \
		--platform linux/$(ARCH) \
		--build-arg ENABLE_GIT_COMMAND="$(ENABLE_GIT_COMMAND)" \
		--build-arg ARCH="$(ARCH)" \
		--build-arg VERSION="$(VERSION)" \
		--file cloud-node-manager.Dockerfile \
		--tag $(NODE_MANAGER_LINUX_FULL_IMAGE):$(IMAGE_TAG)-$(ARCH) .

.PHONY: build-and-push-node-image-windows
build-and-push-node-image-windows: buildx-setup ## Build node-manager image for Windows and push it to registry.
	go build -a -o $(BIN_DIR)/azure-cloud-node-manager.exe ./cmd/cloud-node-manager
	docker buildx build --pull --push --output=type=registry --platform windows/amd64 \
		-t $(NODE_MANAGER_WINDOWS_IMAGE)-$(WINDOWS_OSVERSION) --build-arg OSVERSION=$(WINDOWS_OSVERSION) \
		-f cloud-node-manager-windows.Dockerfile .

.PHONY: build-ccm-e2e-test-image
build-ccm-e2e-test-image: ## Build e2e test image.
	docker build -t $(CCM_E2E_TEST_IMAGE) -f ./e2e.Dockerfile .

.PHONY: push-ccm-image
push-ccm-image: build-ccm-image ## Push controller-manager image.
	docker push $(IMAGE)

.PHONY: push-node-image
push-node-image: ## Push node-manager image for Linux.
	docker push $(NODE_MANAGER_LINUX_FULL_IMAGE):$(IMAGE_TAG)-$(ARCH)

.PHONY: release-ccm-e2e-test-image
release-ccm-e2e-test-image: ## Build and release e2e test image.
	docker build -t $(CCM_E2E_TEST_RELEASE_IMAGE) -f ./e2e.Dockerfile .
	docker push $(CCM_E2E_TEST_RELEASE_IMAGE)

hyperkube:  ## Build hyperkube image.
ifneq ($(K8S_BRANCH), )
	$(eval K8S_VERSION=$(shell REGISTRY=$(IMAGE_REGISTRY) BRANCH=$(K8S_BRANCH) hack/build-hyperkube.sh))
	$(eval HYPERKUBE_IMAGE=$(IMAGE_REGISTRY)/hyperkube-amd64:$(K8S_VERSION))
endif

## --------------------------------------
##@ All Arch or OS Version
## --------------------------------------

.PHONY: build-images
build-images: build-all-ccm-images build-all-node-images ## Build all images.

.PHONY: image
image: build-all-ccm-images build-all-node-images ## Build all images.

.PHONY: push-images
push-images: push-all-ccm-images push-all-node-images ## Push all images.

.PHONY: push
push: push-all-ccm-images push-all-node-images ## Push all images.

.PHONY: push-node-manager-manifest
push-node-manager-manifest: push-all-node-images push-all-windows-node-images ## Create and push a manifest list containing all the Windows and Linux images.
	docker manifest create --amend $(NODE_MANAGER_IMAGE) $(ALL_NODE_MANAGER_IMAGES)
	for arch in $(LINUX_ARCHS); do \
		docker manifest annotate --os linux --arch $${arch} $(NODE_MANAGER_IMAGE)  $(NODE_MANAGER_LINUX_FULL_IMAGE):$(IMAGE_TAG)-$${arch}; \
	done
	# For Windows images, we also need to include the "os.version" in the manifest list, so the Windows node can pull the proper image it needs.
	# we use awk to also trim the quotes around the OS version string.
	set -x; \
	for osversion in $(ALL_WINDOWS_OSVERSIONS); do \
		full_version=`docker manifest inspect ${BASE.windows}:$${osversion} | grep "os.version" | head -n 1 | awk -F\" '{print $$4}'` || true; \
		docker manifest annotate --os windows --arch amd64 --os-version $${full_version} $(NODE_MANAGER_IMAGE) $(NODE_MANAGER_WINDOWS_IMAGE)-$${osversion}; \
	done
	docker manifest push --purge $(NODE_MANAGER_IMAGE)

# TODO(mainred): Currently we push only Linux multi-arch docker images for node image, after fully support Windows docker image building,
#                we need to replace push-all-node-images with push-all-node-images to push multi-arch and multi-os node image, 
#                which is tracked https://github.com/kubernetes-sigs/cloud-provider-azure/issues/829
.PHONY: push-all-node-images
push-all-node-images: $(addprefix push-node-image-,$(LINUX_ARCHS))
	docker manifest create --amend $(NODE_MANAGER_IMAGE) $(ALL_LINUX_NODE_MANAGER_IMAGES)
	for arch in $(LINUX_ARCHS); do \
		docker manifest annotate --os linux --arch $${arch} $(NODE_MANAGER_IMAGE)  $(NODE_MANAGER_LINUX_FULL_IMAGE):$(IMAGE_TAG)-$${arch}; \
	done
	docker manifest push --purge $(NODE_MANAGER_IMAGE)

.PHONY: push-all-windows-node-images
push-all-windows-node-images: $(addprefix push-node-image-windows-,$(ALL_WINDOWS_OSVERSIONS))

.PHONY: build-all-node-images
build-all-node-images: $(addprefix build-node-image-,$(LINUX_ARCHS))

build-node-image-%:
	$(MAKE) ARCH=$* build-node-image

push-node-image-windows-%: ## Push node-manager image for Windows.
	$(MAKE) WINDOWS_OSVERSION=$* build-and-push-node-image-windows

push-node-image-%:
	$(MAKE) ARCH=$* push-node-image

.PHONY: build-all-ccm-images
build-all-ccm-images: $(addprefix build-ccm-image-,$(LINUX_ARCHS))

build-ccm-image-%:
	$(MAKE) ARCH=$* build-ccm-image

.PHONY: push-all-ccm-images
push-all-ccm-images: $(addprefix push-ccm-image-,$(LINUX_ARCHS))

push-ccm-image-%:
	$(MAKE) ARCH=$* push-ccm-image


## --------------------------------------
##@ Tests
## --------------------------------------

.PHONY: test-unit
test-unit: $(PKG_CONFIG) ## Run unit tests.
	mkdir -p $(TEST_RESULTS_DIR)
	hack/test-unit.sh | tee -a $(TEST_RESULTS_DIR)/unittest.txt
ifdef JUNIT
	hack/convert-test-report.pl $(TEST_RESULTS_DIR)/unittest.txt > $(TEST_RESULTS_DIR)/unittest.xml
endif

.PHONY: test-check
test-check: test-lint test-boilerplate test-spelling test-gofmt test-govet ## Run all static checks.

.PHONY: test-gofmt
test-gofmt: ## Run gofmt test.
	hack/verify-gofmt.sh

.PHONY: test-govet
test-govet: ## Run govet test.
	hack/verify-govet.sh

.PHONY: test-lint
test-lint: ## Run golint test.
	hack/verify-golint.sh

.PHONY: test-boilerplate
test-boilerplate: ## Run boilerplate test.
	hack/verify-boilerplate.sh

.PHONY: test-spelling
test-spelling: ## Run spelling test.
	hack/verify-spelling.sh

.PHONY: update-dependencies
update-dependencies: ## Update dependencies and go modules.
	hack/update-dependencies.sh

.PHONY: update-gofmt
update-gofmt: ## Update go formats.
	hack/update-gofmt.sh

.PHONY: update-mocks
update-mocks: ## Create or update mock clients.
	@hack/update-mock-clients.sh

.PHONY: update
update: update-dependencies update-gofmt update-mocks ## Update go formats, mocks and dependencies.

test-e2e: ## Run k8s e2e tests.
	hack/test_k8s_e2e.sh $(TEST_E2E_ARGS)

test-ccm-e2e: ## Run cloud provider e2e tests.
	go test ./tests/e2e/ -timeout 0 -v -ginkgo.v $(CCM_E2E_ARGS)

.PHONY: clean
clean: ## Cleanup local builds.
	rm -rf $(BIN_DIR) $(PKG_CONFIG) $(TEST_RESULTS_DIR)

$(PKG_CONFIG):
	ENABLE_GIT_COMMAND=$(ENABLE_GIT_COMMAND) hack/pkg-config.sh > $@

## --------------------------------------
##@ Release
## --------------------------------------

.PHONY: deploy
deploy: image push ## Build, push and deploy an aks-engine cluster.
	IMAGE=$(IMAGE) HYPERKUBE_IMAGE=$(HYPERKUBE_IMAGE) hack/deploy-cluster.sh

.PHONY: release-staging
release-staging: ## Release the cloud provider images.
	ENABLE_GIT_COMMAND=$(ENABLE_GIT_COMMAND) IMAGE_REGISTRY=$(STAGING_REGISTRY) $(MAKE) build-images push-images
