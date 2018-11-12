.PHONY: all clean update-prepare update test-check test-update test-unit test-lint test-lint-prepare image
.DELETE_ON_ERROR:

SHELL=/bin/bash -o pipefail
BIN_DIR=bin
PKG_CONFIG=.pkg_config
PKG_CONFIG_CONTENT=$(shell cat $(PKG_CONFIG))
TEST_RESULTS_DIR=testResults
# TODO: fix code and enable more options
# -E deadcode -E gocyclo -E vetshadow -E gas -E ineffassign
GOMETALINTER_OPTION=--tests --disable-all -E gofmt -E vet -E golint

IMAGE_REGISTRY ?= local
K8S_VERSION ?= v1.13.0-alpha.3
ACSENGINE_VERSION ?= v0.25.0
HYPERKUBE_IMAGE ?= "gcrio.azureedge.net/google_containers/hyperkube-amd64:$(K8S_VERSION)"

IMAGE_NAME=azure-cloud-controller-manager
IMAGE_TAG=$(shell git rev-parse --short=7 HEAD)
IMAGE=$(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

TEST_IMAGE_NAME=azure-cloud-controller-manager-test
TEST_IMAGE=$(TEST_IMAGE_NAME):$(IMAGE_TAG)

WORKSPACE ?= $(shell pwd)

all: $(BIN_DIR)/azure-cloud-controller-manager

clean:
	rm -rf $(BIN_DIR) $(PKG_CONFIG) $(TEST_RESULTS_DIR)

$(BIN_DIR)/azure-cloud-controller-manager: $(PKG_CONFIG) $(wildcard cloud-controller-manager/*) $(wildcard cloud-controller-manager/**/*)
	 go build -o $@ $(PKG_CONFIG_CONTENT) ./cloud-controller-manager

image:
	docker build -t $(IMAGE) .

$(PKG_CONFIG):
	scripts/pkg-config.sh > $@

test-unit: $(PKG_CONFIG)
	mkdir -p $(TEST_RESULTS_DIR)
	cd cloud-controller-manager && go test $(PKG_CONFIG_CONTENT) -v ./... | tee ../$(TEST_RESULTS_DIR)/unittest.txt
ifdef JUNIT
	scripts/convert-test-report.pl $(TEST_RESULTS_DIR)/unittest.txt > $(TEST_RESULTS_DIR)/unittest.xml
endif

# collection of check tests
test-check: test-lint-prepare test-lint

test-lint-prepare:
	go get -u gopkg.in/alecthomas/gometalinter.v1
	gometalinter.v1 -i
test-lint:
	gometalinter.v1 $(GOMETALINTER_OPTION) ./ cloud-controller-manager/...
	gometalinter.v1 $(GOMETALINTER_OPTION) -e "should not use dot imports" tests/e2e/...
update-prepare:
	go get -u github.com/sgotti/glide-vc
	go get -u github.com/Masterminds/glide
update:
	scripts/update-dependencies.sh
test-update: update-prepare update
	git checkout glide.lock
	git add -A .
	git diff --staged --name-status --exit-code || { \
		echo "You have committed changes after running 'make update', please check"; \
		exit 1; \
	} \

test-e2e: image
	docker push $(IMAGE)
	docker build -t $(TEST_IMAGE) \
		--build-arg K8S_VERSION=$(K8S_VERSION) \
		--build-arg ACSENGINE_VERSION=$(ACSENGINE_VERSION) \
		tests/k8s-azure
	docker run --env-file $(K8S_AZURE_ACCOUNT_CONFIG) \
		-e K8S_AZURE_TEST_ARTIFACTS_DIR=$(WORKSPACE)/_artifacts \
		-v $(WORKSPACE):$(WORKSPACE) \
		$(TEST_IMAGE) e2e -v -caccm_image=$(IMAGE) \
		-ctype=$(SUITE) \
		-csubject=$(SUBJECT) \
		-chyperkube_image=$(HYPERKUBE_IMAGE)
