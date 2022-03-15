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

# Generate all combination of all OS, ARCH, and OSVERSIONS for iteration

ALL_ARCH.linux = amd64 arm arm64
# as windows server core does not support arm64 windows image, trakced by the following link,
# and only 1809 has arm64 nanoserver support, we support here only amd64 windows image
# https://github.com/microsoft/Windows-Containers/issues/195
ALL_ARCH.windows = amd64
ALL_OSVERSIONS.windows := 1809 2004 20H2 ltsc2022
ALL_OS_ARCH.windows = $(foreach arch, $(ALL_ARCH.windows), $(foreach osversion, ${ALL_OSVERSIONS.windows}, ${osversion}-${arch}))

# The current context of image building
# The architecture of the image
ARCH ?= amd64
# OS Version for the Windows images: 1809, 2004, 20H2, ltsc2022
WINDOWS_OSVERSION ?= 1809
# The output type for `docker buildx build` could either be docker (local), or registry.
OUTPUT_TYPE ?= registry

BASE.windows := mcr.microsoft.com/windows/nanoserver

IMAGE_REGISTRY ?= local
K8S_VERSION ?= v1.18.0-rc.1
HYPERKUBE_IMAGE ?= gcrio.azureedge.net/google_containers/hyperkube-amd64:$(K8S_VERSION)

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
NODE_MANAGER_FULL_IMAGE_NAME=$(IMAGE_REGISTRY)/$(NODE_MANAGER_IMAGE_NAME)
NODE_MANAGER_IMAGE=$(NODE_MANAGER_FULL_IMAGE_NAME):$(IMAGE_TAG)
NODE_MANAGER_LINUX_FULL_IMAGE_PREFIX=$(NODE_MANAGER_FULL_IMAGE_NAME):$(IMAGE_TAG)-linux
NODE_MANAGER_WINDOWS_FULL_IMAGE_PREFIX=$(NODE_MANAGER_FULL_IMAGE_NAME):$(IMAGE_TAG)-windows
ALL_NODE_MANAGER_IMAGES = $(foreach arch, ${ALL_ARCH.linux}, $(NODE_MANAGER_LINUX_FULL_IMAGE_PREFIX)-${arch}) $(foreach osversion-arch, ${ALL_OS_ARCH.windows}, $(NODE_MANAGER_WINDOWS_FULL_IMAGE_PREFIX)-${osversion-arch})

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
	CGO_ENABLED=0 GOOS=windows GOARCH=${ARCH} go build -a -o $(BIN_DIR)/azure-cloud-node-manager-${ARCH}.exe $(shell cat $(PKG_CONFIG)) ./cmd/cloud-node-manager

$(BIN_DIR)/azure-cloud-controller-manager: $(PKG_CONFIG) $(wildcard cmd/cloud-controller-manager/*) $(wildcard cmd/cloud-controller-manager/**/*) $(wildcard pkg/**/*) ## Build binary for controller-manager.
	CGO_ENABLED=0 GOOS=linux GOARCH=${ARCH} go build -a -o $(BIN_DIR)/azure-cloud-controller-manager $(shell cat $(PKG_CONFIG)) ./cmd/cloud-controller-manager

$(BIN_DIR)/azure-acr-credential-provider: $(PKG_CONFIG) $(wildcard cmd/acr-credential-provider/*) $(wildcard cmd/acr-credential-provider/**/*) $(wildcard pkg/**/*) ## Build binary for acr-credential-provider.
	CGO_ENABLED=0 GOOS=linux GOARCH=${ARCH} go build -a -o $(BIN_DIR)/azure-acr-credential-provider $(shell cat $(PKG_CONFIG)) ./cmd/acr-credential-provider

$(BIN_DIR)/azure-acr-credential-provider.exe: $(PKG_CONFIG) $(wildcard cmd/acr-credential-provider/*) $(wildcard cmd/acr-credential-provider/**/*) $(wildcard pkg/**/*) ## Build binary for acr-credential-provider.
	CGO_ENABLED=0 GOOS=windows GOARCH=${ARCH} go build -a -o $(BIN_DIR)/azure-acr-credential-provider.exe $(shell cat $(PKG_CONFIG)) ./cmd/acr-credential-provider

## --------------------------------------
##@ Images
## --------------------------------------

buildx-setup:
	docker buildx inspect img-builder > /dev/null || docker buildx create --name img-builder --use
	# enable qemu for arm64 build
	# https://github.com/docker/buildx/issues/464#issuecomment-741507760
	docker run --privileged --rm tonistiigi/binfmt --uninstall qemu-aarch64
	docker run --rm --privileged tonistiigi/binfmt --install all

.PHONY: build-node-image-windows
build-node-image-windows: buildx-setup $(BIN_DIR)/azure-cloud-node-manager.exe ## Build node-manager image for Windows.
	docker buildx build --pull \
		--output=type=$(OUTPUT_TYPE) \
		--platform windows/$(ARCH) \
		-t $(NODE_MANAGER_WINDOWS_FULL_IMAGE_PREFIX)-$(WINDOWS_OSVERSION)-$(ARCH) \
		--build-arg OSVERSION=$(WINDOWS_OSVERSION) \
		--build-arg ARCH=$(ARCH) \
		-f cloud-node-manager-windows.Dockerfile .

.PHONY: build-ccm-e2e-test-image
build-ccm-e2e-test-image: ## Build e2e test image.
	docker build -t $(CCM_E2E_TEST_IMAGE) -f ./e2e.Dockerfile .

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

# NOTE(mainred): build-images target is going to be deprecated
.PHONY: build-images
build-images: image

# NOTE(mainred): push-images target is going to be deprecated
.PHONY: push-images
push-images: push

.PHONY: image
image: build-all-images

.PHONY: push
push: build-all-images push-multi-arch-node-manager-image ## Push all images.

.PHONY: push-multi-arch-node-manager-image ## Push multi-arch node-manager image
push-multi-arch-node-manager-image: ## Create and push a manifest list containing all the Windows and Linux images.
	docker manifest create --amend $(NODE_MANAGER_IMAGE) $(ALL_NODE_MANAGER_IMAGES)
	for arch in $(ALL_ARCH.linux); do \
		docker manifest annotate --os linux --arch $${arch} $(NODE_MANAGER_IMAGE)  $(NODE_MANAGER_LINUX_FULL_IMAGE_PREFIX)-$${arch}; \
	done
	# For Windows images, we also need to include the "os.version" in the manifest list, so the Windows node can pull the proper image it needs.
	# we use awk to also trim the quotes around the OS version string.
	set -x; \
	for windowsarch in $(ALL_ARCH.windows); do \
		for osversion in $(ALL_OSVERSIONS.windows); do \
			full_version=`docker manifest inspect ${BASE.windows}:$${osversion} | jq -r '.manifests[0].platform["os.version"]'`; \
			docker manifest annotate --os windows --arch $${windowsarch} --os-version $${full_version} $(NODE_MANAGER_IMAGE) $(NODE_MANAGER_WINDOWS_FULL_IMAGE_PREFIX)-$${osversion}-$${windowsarch}; \
		done; \
	done
	docker manifest push --purge $(NODE_MANAGER_IMAGE)

.PHONY: build-all-images
build-all-images: buildx-setup
	ENABLE_GIT_COMMAND="$(ENABLE_GIT_COMMAND)" VERSION="$(VERSION)" OUTPUT="$(OUTPUT_TYPE)" IMAGE_TAG="$(IMAGE_TAG)" docker buildx bake --pull --progress plain -f buildx-bake.hcl 

build-ccm-image-%: buildx-setup
	ENABLE_GIT_COMMAND="$(ENABLE_GIT_COMMAND)" VERSION="$(VERSION)" OUTPUT="$(OUTPUT_TYPE)" IMAGE_TAG="$(IMAGE_TAG)" docker buildx bake --pull --progress plain -f buildx-bake.hcl ccm-$*

.PHONY: build-ccm-image
build-ccm-image: buildx-setup build-ccm-image-amd64 ## Build controller-manager image.

.PHONY: build-node-image-linux
build-node-image-linux: buildx-setup 
	ENABLE_GIT_COMMAND="$(ENABLE_GIT_COMMAND)" VERSION="$(VERSION)" OUTPUT="$(OUTPUT_TYPE)" IMAGE_TAG="$(IMAGE_TAG)" docker buildx bake --pull --progress plain -f buildx-bake.hcl cnm-linux-amd64
	
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

test-e2e-capz: ## Run k8s e2e tests with capz
	hack/test_k8s_e2e_capz.sh $(TEST_E2E_ARGS)

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
	ENABLE_GIT_COMMAND=$(ENABLE_GIT_COMMAND) $(MAKE) image push

## --------------------------------------
##@ Deploy clusters
## --------------------------------------

.PHONY: deploy-cluster
deploy-cluster:
	hack/deploy-cluster-capz.sh
