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

BIN_DIR=bin

# The current context of image building
# The architecture of the image
ARCH ?= amd64
# OS Version for the Windows images: 1809, 2004, 20H2, ltsc2022
WINDOWS_OSVERSION ?= 1809
# The output type for `docker buildx build` could either be docker (local), or registry.
OUTPUT_TYPE ?= docker

IMAGE_REGISTRY ?= local
IMAGE_TAG ?= $(shell git rev-parse --short=7 HEAD)
HEALTH_PROBE_PROXY_IMAGE=$(IMAGE_REGISTRY)/health-probe-proxy:$(IMAGE_TAG)
HEALTH_PROBE_PROXY_WIN_IMAGE=$(IMAGE_REGISTRY)/health-probe-proxy-windows:$(IMAGE_TAG)

DOCKER_BUILDX ?= docker buildx

$(BIN_DIR)/health-probe-proxy: ## Build binary for health-probe-proxy.
	CGO_ENABLED=0 GOOS=linux GOARCH=${ARCH} go build -a -o $(BIN_DIR)/health-probe-proxy .

$(BIN_DIR)/health-probe-proxy.exe: ## Build binary for health-probe-proxy.
	CGO_ENABLED=0 GOOS=windows GOARCH=${ARCH} go build -a -o $(BIN_DIR)/health-probe-proxy.exe .

buildx-setup:
	$(DOCKER_BUILDX) inspect img-builder > /dev/null 2>&1 || $(DOCKER_BUILDX) create --name img-builder --use
	# enable qemu for arm64 build
	# https://github.com/docker/buildx/issues/464#issuecomment-741507760
	docker run --privileged --rm tonistiigi/binfmt --uninstall qemu-aarch64
	docker run --rm --privileged tonistiigi/binfmt --install all

.PHONY: build-health-probe-proxy-image
build-health-probe-proxy-image: buildx-setup  $(BIN_DIR)/health-probe-proxy ## Build health-probe-proxy image for Linux.
	$(DOCKER_BUILDX) build --pull \
		--output=type=$(OUTPUT_TYPE) \
		--platform linux/$(ARCH) \
		-t $(HEALTH_PROBE_PROXY_IMAGE) \
		--build-arg OS=linux \
		--build-arg ARCH=$(ARCH) \
		-f Dockerfile . \
		--provenance=false \
		--sbom=false

.PHONY: build-health-probe-proxy-image-windows
build-health-probe-proxy-image-windows: buildx-setup $(BIN_DIR)/health-probe-proxy.exe ## Build health-probe-proxy image for Windows.
	$(DOCKER_BUILDX) build --pull \
		--output=type=$(OUTPUT_TYPE) \
		--platform windows/$(ARCH) \
		-t $(HEALTH_PROBE_PROXY_WIN_IMAGE) \
		--build-arg OSVERSION=$(WINDOWS_OSVERSION) \
		--build-arg ARCH=$(ARCH) \
		-f windows.Dockerfile . \
		--provenance=false \
		--sbom=false