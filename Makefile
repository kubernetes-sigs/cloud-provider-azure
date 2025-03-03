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
#The test args for Kubernetes e2e tests
TEST_E2E_ARGS ?= '--ginkgo.focus=Port\sforwarding'

WINDOWS_USE_HOST_PROCESS_CONTAINERS ?= false

# Generate all combination of all OS, ARCH, and OSVERSIONS for iteration

ALL_OS ?= linux windows
ALL_ARCH.linux ?= amd64 arm arm64
ALL_OS_ARCH.linux=$(foreach arch, $(ALL_ARCH.linux), linux-$(arch))
# as windows server core does not support arm64 windows image, trakced by the following link,
# and only 1809 has arm64 nanoserver support, we support here only amd64 windows image
# https://github.com/microsoft/Windows-Containers/issues/195
ALL_ARCH.windows ?= amd64
ALL_OSVERSIONS.windows ?= 1809 ltsc2022
ifeq ($(WINDOWS_USE_HOST_PROCESS_CONTAINERS), true)
ALL_OS_ARCH.windows = $(foreach arch, $(ALL_ARCH.windows), windows-hpc-${arch})
else
ALL_OS_ARCH.windows = $(foreach arch, $(ALL_ARCH.windows), $(foreach osversion, ${ALL_OSVERSIONS.windows}, windows-${osversion}-${arch}))
endif
ALL_OS_ARCH = $(foreach os, $(ALL_OS), $(ALL_OS_ARCH.$(os)))

# The current context of image building
# The architecture of the image
ARCH ?= amd64
# OS Version for the Windows images: 1809, 2004, 20H2, ltsc2022
WINDOWS_OSVERSION ?= 1809
# The output type for `docker buildx build` could either be docker (local), or registry.
OUTPUT_TYPE ?= docker

BASE.windows := mcr.microsoft.com/windows/nanoserver

IMAGE_REGISTRY ?= local
K8S_VERSION ?= v1.18.0-rc.1
HYPERKUBE_IMAGE ?= gcrio.azureedge.net/google_containers/hyperkube-amd64:$(K8S_VERSION)

ifndef TAG
	IMAGE_TAG ?= $(shell git rev-parse --short=7 HEAD)
else
	IMAGE_TAG ?= $(TAG)
endif

# `docker buildx` and `docker manifest` requires enabling DOCKER_CLI_EXPERIMENTAL for docker version < 1.20
export DOCKER_CLI_EXPERIMENTAL=enabled
DOCKER_CLI_EXPERIMENTAL := enabled
DOCKER_BUILDX ?= docker buildx

# Cross-platform build with CGO_ENABLED=1
ifeq ($(ARCH), amd64)
CGO_OPTION ?= CGO_ENABLED=1
else ifeq ($(ARCH), arm64)
CGO_OPTION ?= CGO_ENABLED=1 CC=aarch64-linux-gnu-gcc
else ifeq ($(ARCH), arm)
CGO_OPTION ?= CGO_ENABLED=1 CC=arm-linux-gnueabihf-gcc
endif

# image name and variable pattern for the cloud-controller-manager and cloud-node-manager:
# - XX_IMAGE only carries the registry and XX component image name without image tag
# - The Linux image tag of specific ARCH will be in the format $(IMAGE_TAG)-linux-$(ARCH)
# - The Windows image tag of specific ARCH and OS will be in the format $(IMAGE_TAG)-windows-$(WINDOWS_OSVERSION)-$(ARCH)
# - The Windows hpc image tag of specific ARCH will be in the format $(IMAGE_TAG)-windows-hpc-$(ARCH)

# cloud controller manager image
CONTROLLER_MANAGER_IMAGE_NAME=azure-cloud-controller-manager
CONTROLLER_MANAGER_IMAGE=$(IMAGE_REGISTRY)/$(CONTROLLER_MANAGER_IMAGE_NAME)

# cloud node manager image
NODE_MANAGER_IMAGE_NAME=azure-cloud-node-manager
NODE_MANAGER_IMAGE=$(IMAGE_REGISTRY)/$(NODE_MANAGER_IMAGE_NAME)

# ccm e2e test image
CCM_E2E_TEST_IMAGE_NAME=cloud-provider-azure-e2e
CCM_E2E_TEST_IMAGE=$(IMAGE_REGISTRY)/$(CCM_E2E_TEST_IMAGE_NAME):$(IMAGE_TAG)
CCM_E2E_TEST_RELEASE_IMAGE=docker.pkg.github.com/kubernetes-sigs/cloud-provider-azure/cloud-provider-azure-e2e:$(IMAGE_TAG)

# cloud build variables
CLOUD_BUILD_IMAGE ?= ccm

##@ General

help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[.a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

## --------------------------------------
##@ Binaries
## --------------------------------------

.PHONY: all
all: $(BIN_DIR)/azure-cloud-controller-manager $(BIN_DIR)/azure-cloud-node-manager $(BIN_DIR)/azure-cloud-node-manager.exe ## Build binaries for the project.

$(BIN_DIR)/azure-cloud-node-manager: $(PKG_CONFIG) $(wildcard cmd/cloud-node-manager/*) $(wildcard cmd/cloud-node-manager/**/*) $(wildcard pkg/**/*) ## Build node-manager binary for Linux.
	$(CGO_OPTION) GOOS=linux GOARCH=${ARCH} go build -a -o $(BIN_DIR)/azure-cloud-node-manager $(shell cat $(PKG_CONFIG)) ./cmd/cloud-node-manager

$(BIN_DIR)/azure-cloud-node-manager.exe: $(PKG_CONFIG) $(wildcard cmd/cloud-node-manager/*) $(wildcard cmd/cloud-node-manager/**/*) $(wildcard pkg/**/*) ## Build node-manager binary for Windows.
	$(CGO_OPTION) GOOS=windows GOARCH=${ARCH} go build -a -o $(BIN_DIR)/azure-cloud-node-manager-${ARCH}.exe $(shell cat $(PKG_CONFIG)) ./cmd/cloud-node-manager

$(BIN_DIR)/azure-cloud-controller-manager: $(PKG_CONFIG) $(wildcard cmd/cloud-controller-manager/*) $(wildcard cmd/cloud-controller-manager/**/*) $(wildcard pkg/**/*) ## Build binary for controller-manager.
	$(CGO_OPTION) GOOS=linux GOARCH=${ARCH} go build -a -o $(BIN_DIR)/azure-cloud-controller-manager $(shell cat $(PKG_CONFIG)) ./cmd/cloud-controller-manager

$(BIN_DIR)/azure-acr-credential-provider: $(PKG_CONFIG) $(wildcard cmd/acr-credential-provider/*) $(wildcard cmd/acr-credential-provider/**/*) $(wildcard pkg/**/*) ## Build binary for acr-credential-provider.
	$(CGO_OPTION) GOOS=linux GOARCH=${ARCH} go build -a -o $(BIN_DIR)/azure-acr-credential-provider $(shell cat $(PKG_CONFIG)) ./cmd/acr-credential-provider

$(BIN_DIR)/azure-acr-credential-provider.exe: $(PKG_CONFIG) $(wildcard cmd/acr-credential-provider/*) $(wildcard cmd/acr-credential-provider/**/*) $(wildcard pkg/**/*) ## Build binary for acr-credential-provider.
	$(CGO_OPTION) GOOS=windows GOARCH=${ARCH} go build -a -o $(BIN_DIR)/azure-acr-credential-provider.exe $(shell cat $(PKG_CONFIG)) ./cmd/acr-credential-provider

## --------------------------------------
##@ Images
## --------------------------------------

buildx-setup:
	$(DOCKER_BUILDX) inspect img-builder > /dev/null 2>&1 || $(DOCKER_BUILDX) create --name img-builder --use
	# enable qemu for arm64 build
	# https://github.com/docker/buildx/issues/464#issuecomment-741507760
	docker run --privileged --rm tonistiigi/binfmt --uninstall qemu-aarch64
	docker run --rm --privileged tonistiigi/binfmt --install all

.PHONY: build-ccm-image
build-ccm-image: buildx-setup ## Build controller-manager image.
	$(DOCKER_BUILDX) build \
		--pull \
		--output=type=$(OUTPUT_TYPE) \
		--platform linux/$(ARCH) \
		--build-arg ENABLE_GIT_COMMAND="$(ENABLE_GIT_COMMAND)" \
		--build-arg ARCH="$(ARCH)" \
		--build-arg VERSION="$(VERSION)" \
		--file Dockerfile \
		--tag $(CONTROLLER_MANAGER_IMAGE):$(IMAGE_TAG)-linux-$(ARCH) . \
		--provenance=false \
		--sbom=false

.PHONY: build-node-image-linux
build-node-image-linux: buildx-setup ## Build node-manager Linux image with specific ARCH.
	$(DOCKER_BUILDX) build \
		--pull \
		--output=type=$(OUTPUT_TYPE) \
		--platform linux/$(ARCH) \
		--build-arg ENABLE_GIT_COMMAND="$(ENABLE_GIT_COMMAND)" \
		--build-arg ARCH="$(ARCH)" \
		--build-arg VERSION="$(VERSION)" \
		--file cloud-node-manager.Dockerfile \
		--tag $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)-linux-$(ARCH) . \
		--provenance=false \
		--sbom=false

.PHONY: build-node-image-windows
build-node-image-windows: buildx-setup $(BIN_DIR)/azure-cloud-node-manager.exe ##  Build node-manager Windows image with specific ARCH and OS.
	$(DOCKER_BUILDX) build --pull \
		--output=type=$(OUTPUT_TYPE) \
		--platform windows/$(ARCH) \
		-t $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)-windows-$(WINDOWS_OSVERSION)-$(ARCH) \
		--build-arg OSVERSION=$(WINDOWS_OSVERSION) \
		--build-arg ARCH=$(ARCH) \
		-f cloud-node-manager-windows.Dockerfile . \
		--provenance=false \
		--sbom=false

.PHONY: build-node-image-windows-hpc
build-node-image-windows-hpc: buildx-setup $(BIN_DIR)/azure-cloud-node-manager.exe ## Build node-manager Windows image for a specific ARCH.
	$(DOCKER_BUILDX) build --pull \
		--output=type=$(OUTPUT_TYPE) \
		--platform windows/$(ARCH) \
		-t $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)-windows-hpc-$(ARCH) \
		--build-arg ARCH=$(ARCH) \
		-f cloud-node-manager-windows-hpc.Dockerfile . \
		--provenance=false \
		--sbom=false

.PHONY: build-ccm-e2e-test-image
build-ccm-e2e-test-image: ## Build e2e test image.
	docker build -t $(CCM_E2E_TEST_IMAGE) -f ./e2e.Dockerfile .

.PHONY: push-ccm-image
push-ccm-image: ## Push controller-manager Linux image of a specific ARCH.
	$(MAKE) ARCH=$(ARCH) OUTPUT_TYPE=registry build-ccm-image

.PHONY: push-node-image-linux
push-node-image-linux: ## Push node-manager Linux image for a specific ARCH.
	$(MAKE) ARCH=$(ARCH) OUTPUT_TYPE=registry build-node-image-linux

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

.PHONY: manifest
manifest: manifest-controller-manager manifest-node-manager ## Push cloud-controller-manager and cloud-node-manager manifests.

.PHONY: manifest-controller-manager
manifest-controller-manager: push-all-ccm-images ## Push controller-manager manifest by creating and pushing a manifest list containing all the Linux ccm images.
	docker manifest create --amend $(CONTROLLER_MANAGER_IMAGE):$(IMAGE_TAG) $(shell echo $(ALL_OS_ARCH.linux) | sed -e "s~[^ ]*~$(CONTROLLER_MANAGER_IMAGE):$(IMAGE_TAG)\-&~g")
	for arch in $(ALL_ARCH.linux); do \
		docker manifest annotate --os linux --arch $${arch} $(CONTROLLER_MANAGER_IMAGE):$(IMAGE_TAG) $(CONTROLLER_MANAGER_IMAGE):$(IMAGE_TAG)-linux-$${arch}; \
	done
	docker manifest push --purge $(CONTROLLER_MANAGER_IMAGE):$(IMAGE_TAG)

.PHONY: manifest-node-manager
manifest-node-manager: push-all-node-images ## Push node-manager manifest by creating and pushing a manifest list containing all the Windows and Linux images.
	docker manifest create --amend $(NODE_MANAGER_IMAGE):$(IMAGE_TAG) $(shell echo $(ALL_OS_ARCH) | sed -e "s~[^ ]*~$(NODE_MANAGER_IMAGE):$(IMAGE_TAG)\-&~g")
	for arch in $(ALL_ARCH.linux); do \
		docker manifest annotate --os linux --arch $${arch} $(NODE_MANAGER_IMAGE):$(IMAGE_TAG) $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)-linux-$${arch}; \
	done
ifeq ($(WINDOWS_USE_HOST_PROCESS_CONTAINERS),true)
	for windowsarch in $(ALL_ARCH.windows); do \
		docker manifest annotate --os windows --arch $${windowsarch} $(NODE_MANAGER_IMAGE):$(IMAGE_TAG) $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)-windows-hpc-$(ARCH); \
	done
else
	# For Windows images, we also need to include the "os.version" in the manifest list, so the Windows node can pull the proper image it needs.
	# we use awk to also trim the quotes around the OS version string.
	set -x; \
	for windowsarch in $(ALL_ARCH.windows); do \
		for osversion in $(ALL_OSVERSIONS.windows); do \
			full_version=`docker manifest inspect ${BASE.windows}:$${osversion} | jq -r '.manifests[0].platform["os.version"]'`; \
			docker manifest annotate --os windows --arch $${windowsarch} --os-version $${full_version} $(NODE_MANAGER_IMAGE):$(IMAGE_TAG) $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)-windows-$${osversion}-$${windowsarch}; \
		done; \
	done
endif

	docker manifest push --purge $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)

.PHONY: push-all-ccm-images
push-all-ccm-images: $(addprefix push-ccm-image-,$(ALL_ARCH.linux))

push-ccm-image-%:
	$(MAKE) ARCH=$* push-ccm-image
# TODO(mainred): capz uses push-ccm-image-amd64 to push the image, to not break the test and image release, we want to keep the same capability.
#                And remove this workaround after we onboard the new targets in capz.
ifeq ($(IS_RELEASE),)
	docker tag $(CONTROLLER_MANAGER_IMAGE):$(IMAGE_TAG)-linux-amd64 $(CONTROLLER_MANAGER_IMAGE):$(IMAGE_TAG)
	docker push $(CONTROLLER_MANAGER_IMAGE):$(IMAGE_TAG)
endif

.PHONY: push-all-node-Images
push-all-node-images: push-all-node-images-linux push-all-node-images-windows

.PHONY: push-all-node-images-Linux
push-all-node-images-linux: $(addprefix push-node-image-,$(ALL_OS_ARCH.linux))

.PHONY: push-all-node-images-windows
push-all-node-images-windows: $(addprefix push-node-image-,$(ALL_OS_ARCH.windows))

# split words on hyphen, access by 1-index
word-hyphen = $(word $2,$(subst -, ,$1))

push-node-image-linux-%:
	$(MAKE) ARCH=$* push-node-image-linux

push-node-image-windows-%:
	$(MAKE) WINDOWS_OSVERSION=$(call word-hyphen,$*,1) ARCH=$(call word-hyphen,$*,2) OUTPUT_TYPE=registry build-node-image-windows

push-node-image-windows-hpc-%:
	$(MAKE) ARCH=$(call word-hyphen,$*,1) OUTPUT_TYPE=registry build-node-image-windows-hpc

.PHONY: build-all-node-images-linux
build-all-node-images-linux: $(addprefix build-node-image-,$(ALL_OS_ARCH.linux))

.PHONY: build-all-node-images-windows
build-all-node-images-windows: $(addprefix build-node-image-,$(ALL_OS_ARCH.windows))

build-node-image-linux-%:
	$(MAKE) ARCH=$* build-node-image-linux

build-node-image-windows-%:
	$(MAKE) WINDOWS_OSVERSION=$(call word-hyphen,$*,1) ARCH=$(call word-hyphen,$*,2) build-node-image-windows

build-node-image-windows-hpc-%:
	$(MAKE) ARCH=$(call word-hyphen,$*,1) build-node-image-windows-hpc

.PHONY: build-all-ccm-images
build-all-ccm-images: $(addprefix build-ccm-image-,$(ALL_ARCH.linux))

build-ccm-image-%:
	$(MAKE) ARCH=$* build-ccm-image

# TODO(mainred): remove this after we onboard new patterns in capz

manifest-node-manager-image-windows-hpc-%:
	$(MAKE) ARCH=$(call word-hyphen,$*,1) manifest-node-manager-image-windows-hpc

manifest-node-manager-image-windows-hpc:
	set -x
	docker manifest create --amend $(NODE_MANAGER_IMAGE):$(IMAGE_TAG) $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)-linux-$(ARCH) $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)-windows-hpc-$(ARCH)
	docker manifest annotate --os linux --arch $(ARCH) $(NODE_MANAGER_IMAGE):$(IMAGE_TAG) $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)-linux-$(ARCH)
	docker manifest annotate --os windows --arch $(ARCH) $(NODE_MANAGER_IMAGE):$(IMAGE_TAG) $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)-windows-hpc-$(ARCH)
	docker manifest push --purge $(NODE_MANAGER_IMAGE):$(IMAGE_TAG)

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
test-check: test-boilerplate test-helm ## Run all static checks.

.PHONY: lint
lint: golangci-lint ## Run golangci-lint against code.
	$(LINTER) run -v -E exportloopref

.PHONY: test-boilerplate
test-boilerplate: ## Run boilerplate test.
	hack/verify-boilerplate.sh

.PHONY: test-helm
test-helm: ## Validate helm charts。
	hack/verify-helm-repo.sh

.PHONY: verify-vendor-licenses
verify-vendor-licenses: ## Verify vendor licenses
	hack/verify-azure-vendor-licenses.sh

.PHONY: update-helm
update-helm: ## Update helm charts.
	hack/update-helm-repo.sh

.PHONY: update-dependencies
update-dependencies: ## Update dependencies and go modules.
	hack/update-dependencies.sh

.PHONY: update-gofmt
update-gofmt: ## Update go formats.
	hack/update-gofmt.sh

.PHONY: update-mocks
update-mocks: ## Create or update mock clients.
	@hack/update-mock-clients.sh

.PHONY: update-vendor-licenses
update-vendor-licenses: ## Update vendor licenses。
	hack/update-azure-vendor-licenses.sh

.PHONY: update
update: update-dependencies update-gofmt update-mocks update-vendor-licenses ## Update go formats, mocks and dependencies.

test-e2e: ## Run k8s e2e tests.
	hack/test_k8s_e2e.sh $(TEST_E2E_ARGS)

test-e2e-capz: ## Run k8s e2e tests with capz.
	hack/test_k8s_e2e_capz.sh $(TEST_E2E_ARGS)

test-ccm-e2e: ## Run cloud provider e2e tests.
	hack/test-ccm-e2e.sh

.PHONY: clean
clean: ## Cleanup local builds.
	rm -rf $(BIN_DIR) $(PKG_CONFIG) $(TEST_RESULTS_DIR)

$(PKG_CONFIG):
ifeq ($(OVERRIDE_PKG_CONFIG), true)
	@echo "Overriding pkg-config script. Use an alternate method to generate .pkg_config"
else
	ENABLE_GIT_COMMAND=$(ENABLE_GIT_COMMAND) hack/pkg-config.sh > $@
endif

## --------------------------------------
##@ Release
## --------------------------------------

.PHONY: deploy
deploy: image push ## Build, push and deploy an aks-engine cluster.
	CCM_IMAGE=$(CONTROLLER_MANAGER_IMAGE) CNM_IMAGE=$(NODE_MANAGER_IMAGE) HYPERKUBE_IMAGE=$(HYPERKUBE_IMAGE) hack/deploy-cluster.sh

.PHONY: cloud-build-prerequisites
cloud-build-prerequisites:
	apk add --no-cache jq

.PHONY: release-staging
release-staging: ## Release the cloud provider images.
ifeq ($(CLOUD_BUILD_IMAGE),ccm)
	ENABLE_GIT_COMMAND=$(ENABLE_GIT_COMMAND) $(MAKE) IS_RELEASE=true manifest-controller-manager
else
	ENABLE_GIT_COMMAND=$(ENABLE_GIT_COMMAND) $(MAKE) cloud-build-prerequisites manifest-node-manager
endif

## --------------------------------------
##@ Deploy clusters with capz
## --------------------------------------

.PHONY: deploy-workload-cluster
deploy-workload-cluster:
	hack/deploy-workload-cluster.sh

.PHONY: delete-workload-cluster
delete-workload-cluster:
	hack/delete-workload-cluster.sh

##@ Tools

LINTER = $(shell pwd)/bin/golangci-lint
LINTER_VERSION = v1.60.1
.PHONY: golangci-lint
golangci-lint:  ## Download golangci-lint locally if necessary.
	@echo "Installing golangci-lint"
	@test -s $(LINTER) || curl -sfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(shell pwd)/bin $(LINTER_VERSION)
