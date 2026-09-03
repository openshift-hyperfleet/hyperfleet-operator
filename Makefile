# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
VERSION ?= 0.0.1

# Set the Operator SDK version to use. By default, what is installed on the system is used.
# This is useful for CI or a project to utilize a specific version of the operator-sdk toolkit.
OPERATOR_SDK_VERSION ?= v1.42.3
YQ_VERSION ?= v4.44.1

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

# Root directory
ROOT_DIR := $(dir $(realpath $(firstword $(MAKEFILE_LIST))))

# Tool versioning
TOOL_MOD := $(ROOT_DIR)tools/go.mod
gotool = go tool -modfile="$(TOOL_MOD)" $(1)

# Tool shortcuts
CONTROLLER_GEN := $(call gotool,controller-gen)
GOLANGCI_LINT := $(call gotool,golangci-lint)
KUSTOMIZE := $(call gotool,kustomize)
SETUP_ENVTEST := $(call gotool,setup-envtest)

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

##@ Testing
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
KIND_CLUSTER ?= hyperfleet-operator-test-e2e

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true

.PHONY: setup-envtest
setup-envtest: $(LOCALBIN) ## Download the envtest binaries (etcd, kube-apiserver) into the local bin directory.
	$(SETUP_ENVTEST) use '$(ENVTEST_K8S_VERSION)' --bin-dir $(LOCALBIN) -p path

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use '$(ENVTEST_K8S_VERSION)' --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND_CLUSTER=$(KIND_CLUSTER) go test ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

##@ Lint

.PHONY: verify-related-images
verify-related-images: ## Verify immutable deployable images match CSV relatedImages.
	go run ./hack/verify-related-images

.PHONY: lint
lint: verify-related-images ## Run image verification and golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

OC_MIRROR_IMAGE ?= hyperfleet-oc-mirror:local

.PHONY: build-oc-mirror-image
build-oc-mirror-image: ## Build the containerized oc-mirror runner.
	$(CONTAINER_TOOL) build -f hack/oc-mirror.Dockerfile -t $(OC_MIRROR_IMAGE) .

.PHONY: test-disconnected-mirror
test-disconnected-mirror: ## Exercise the disk-to-mirror archive transfer.
	CONTAINER_TOOL=$(CONTAINER_TOOL) OC_MIRROR_IMAGE=$(OC_MIRROR_IMAGE) \
	BUNDLE_IMAGE="$(BUNDLE_IMAGE)" OPERATOR_IMAGE="$(OPERATOR_IMAGE)" API_IMAGE="$(API_IMAGE)" \
	./hack/test-disconnected-mirror.sh

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

# OPERATOR_NAMESPACE is the namespace the operator creates operands in. In-cluster
# it comes from the downward API; for `make run` it defaults here so local dev works.
# Override with e.g. `make run OPERATOR_NAMESPACE=my-ns`.
OPERATOR_NAMESPACE ?= hyperfleet-system

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	OPERATOR_NAMESPACE=$(OPERATOR_NAMESPACE) go run ./cmd/main.go


##@ Container Images

# Image configuration
PLATFORM ?= linux/amd64
QUAY_REPO ?= openshift-hyperfleet
IMG_REGISTRY ?= quay.io/$(QUAY_REPO)
IMG_NAME ?= hyperfleet-operator
IMG_TAG ?= $(APP_VERSION)
IMG ?= $(IMG_REGISTRY)/$(IMG_NAME):$(IMG_TAG)
# Base image for production builds - matches Dockerfile default
# Override with DEV_BASE_IMAGE for dev builds (see image-dev target)
BASE_IMAGE ?= registry.access.redhat.com/ubi9-micro:latest

APP_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
GIT_SHA ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_DIRTY ?= $(shell [ -z "$$(git status --porcelain 2>/dev/null)" ] || echo "-modified")

# For container builds, use linux by default; override PLATFORM to build for other platforms (e.g. linux/arm64)

# Go build flags (FIPS compliant)
CGO_ENABLED ?= 1
GOEXPERIMENT ?= boringcrypto 
GOFLAGS ?= -trimpath
# LDFLAGS := -s -w \
#            -X github.com/openshift-hyperfleet/hyperfleet-operator/pkg/version.Version=$(APP_VERSION) \
#            -X github.com/openshift-hyperfleet/hyperfleet-operator/pkg/version.Commit=$(GIT_SHA) \
#            -X 'github.com/openshift-hyperfleet/hyperfleet-operator/pkg/version.BuildTime=$(BUILD_DATE)'

.PHONY: check-container-tool
check-container-tool:
ifndef CONTAINER_TOOL
	@echo "Error: No container tool found (docker or podman)"
	@exit 1
endif

.PHONY: image
image: check-container-tool manifests generate fmt vet ## Build container image with configurable registry/tag
	@echo "Building container image $(IMG)..."
	$(CONTAINER_TOOL) build \
		--platform $(PLATFORM) \
		--build-arg BASE_IMAGE=$(BASE_IMAGE) \
		--build-arg APP_VERSION=$(APP_VERSION) \
		-t $(IMG) .
	@echo "Image built: $(IMG)"
	@echo "$(IMG)"

.PHONY: image-push
image-push: check-container-tool ## Push container image to registry
	@echo "Pushing image $(IMG)..."
	$(CONTAINER_TOOL) push $(IMG)
	@echo "Image pushed: $(IMG)"

.PHONY: image-build-push
image-build-push: image image-push ## Build and push container image to registry

.PHONY: check-quay-user
check-quay-user:
ifeq ($(strip $(QUAY_USER)),)
	@echo "Error: QUAY_USER is not set"
	@echo ""
	@echo "Usage: QUAY_USER=myuser make image-dev"
	@exit 1
endif

# Usage: QUAY_USER=myuser make image-dev
# Dev image configuration - set QUAY_USER to push to personal registry
DEV_TAG ?= dev-$(GIT_SHA)
QUAY_USER ?=
DEV_BASE_IMAGE ?= registry.access.redhat.com/ubi9/ubi-minimal:latest

.PHONY: image-dev
image-dev: QUAY_REPO = $(QUAY_USER)
image-dev: IMG_TAG = $(DEV_TAG)
image-dev: BASE_IMAGE = $(DEV_BASE_IMAGE)
image-dev: check-quay-user image-build-push ## Build and push dev image to dev Quay registry (requires QUAY_USER)

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name hyperfleet-operator-builder
	$(CONTAINER_TOOL) buildx use hyperfleet-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm hyperfleet-operator-builder
	rm Dockerfile.cross


##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@$(KUSTOMIZE)  build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: build-deployer-override-img ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	@$(KUBECTL) apply -f dist/install.yaml

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@$(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f dist/install.yaml


##@ Bundles/Catalog


# Non-olm installs
# Generates dist/install.yaml
# Install resources
# kubectl apply -f dist/install.yaml
# Uninstall resources 
# kubectl delete -f dist/install.yaml
# For image overrides edit config/manager/kustomization.yaml
.PHONY: build-deployer
build-deployer: manifests generate ## Generate a consolidated YAML with CRDs and deployment.
	@mkdir -p dist
	@$(KUSTOMIZE) build config/default > dist/install.yaml

.PHONY: build-deployer-override-img
build-deployer-override-img: manifests generate ## Generate deployer with IMG override, then restore kustomization.yaml
	@mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	@$(KUSTOMIZE) build config/default > dist/install.yaml
	@echo "Deployer generated with IMG=$(IMG)"
	@echo "Note: config/manager/kustomization.yaml has been modified. Commit or reset as needed."

# For now `stable` channel is the default and only channel
# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "candidate,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=candidate,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="candidate,fast,stable")
CHANNELS ?= stable
BUNDLE_CHANNELS := --channels=$(CHANNELS)

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
DEFAULT_CHANNEL ?= stable
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)


# USE_IMAGE_DIGESTS defines if images are resolved via tags or digests
# You can enable this value if you would like to use SHA Based Digests
# To enable set flag to true
USE_IMAGE_DIGESTS ?= false
ifeq ($(USE_IMAGE_DIGESTS), true)
	BUNDLE_GEN_FLAGS += --use-image-digests
endif

# Defines the base of the registry we use for `make bundle-build catalog-build catalog-push bundle-push`
# Defaults to quay.io/openshift-hyperfleet/hyperfleet-operator
# For dev: If QUAY_REPO is set quay.io/<QUAY_REPO>/hyperfleet-operator
REG_REPO_BASE ?= $(IMG_REGISTRY)/$(IMG_NAME)

# Image tag for the bundle
BUNDLE_IMG ?= $(REG_REPO_BASE)-bundle:v$(VERSION)

# BUNDLE_GEN_FLAGS are the flags passed to the operator-sdk generate bundle command
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)

# A comma-separated list of bundle images (e.g. make catalog-build BUNDLE_IMGS=example.com/operator-bundle:v0.1.0,example.com/operator-bundle:v0.2.0).
# These images MUST exist in a registry and be pull-able.
BUNDLE_IMGS ?= $(BUNDLE_IMG)

# The image tag given to the resulting catalog image (e.g. make catalog-build CATALOG_IMG=example.com/operator-catalog:v0.2.0).
CATALOG_IMG ?= $(REG_REPO_BASE)-catalog:v$(VERSION)

# Set CATALOG_BASE_IMG to an existing catalog image tag to add $BUNDLE_IMGS to that image.
ifneq ($(origin CATALOG_BASE_IMG), undefined)
FROM_INDEX_OPT := --from-index $(CATALOG_BASE_IMG)
endif

.PHONY: bundle
bundle: manifests operator-sdk yq ## Generate bundle manifests and metadata, then validate generated files.
	$(OPERATOR_SDK) generate kustomize manifests -q
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	HYPERFLEET_OPERATOR_IMAGE_PULLSPEC="$$($(YQ) eval '.spec.install.spec.deployments[].spec.template.spec.containers[] | select(.name == "manager") | .image' bundle/manifests/hyperfleet-operator.clusterserviceversion.yaml)" CSV_FILE=bundle/manifests/hyperfleet-operator.clusterserviceversion.yaml ./hack/bundle/add_operator_related_image.sh
	$(OPERATOR_SDK) bundle validate ./bundle

.PHONY: bundle-override-img
bundle-override-img: manifests operator-sdk yq ## Generate bundle with IMG override, then restore kustomization.yaml
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	HYPERFLEET_OPERATOR_IMAGE_PULLSPEC="$$($(YQ) eval '.spec.install.spec.deployments[].spec.template.spec.containers[] | select(.name == "manager") | .image' bundle/manifests/hyperfleet-operator.clusterserviceversion.yaml)" CSV_FILE=bundle/manifests/hyperfleet-operator.clusterserviceversion.yaml ./hack/bundle/add_operator_related_image.sh --allow-tag
	$(OPERATOR_SDK) bundle validate ./bundle
	@echo "Bundle generated with IMG=$(IMG)"
	@echo "Note: config/manager/kustomization.yaml has been modified. Commit or reset as needed."

.PHONY: bundle-build
bundle-build: ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(MAKE) docker-push IMG=$(BUNDLE_IMG)

# Build a catalog image by adding bundle images to an empty catalog using the operator package manager tool, 'opm'.
# This recipe invokes 'opm' in 'semver' bundle add mode. For more information on add modes, see:
# https://github.com/operator-framework/community-operators/blob/7f1438c/docs/packaging-operator.md#updating-your-existing-operator
.PHONY: catalog-build
catalog-build: opm ## Build a catalog image.
	$(OPM) index add --container-tool $(CONTAINER_TOOL) --mode semver --tag $(CATALOG_IMG) --bundles $(BUNDLE_IMGS) $(FROM_INDEX_OPT)

# Push the catalog image.
.PHONY: catalog-push
catalog-push: ## Push a catalog image.
	$(MAKE) docker-push IMG=$(CATALOG_IMG)


##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind

.PHONY: yq
YQ ?= $(LOCALBIN)/yq
yq: ## Download yq locally if necessary.
ifeq (,$(wildcard $(YQ)))
ifeq (, $(shell which yq 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(YQ)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(YQ) https://github.com/mikefarah/yq/releases/download/$(YQ_VERSION)/yq_$${OS}_$${ARCH} ;\
	chmod +x $(YQ) ;\
	}
else
YQ = $(shell which yq)
endif
endif

.PHONY: operator-sdk
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
operator-sdk: ## Download operator-sdk locally if necessary.
ifeq (,$(wildcard $(OPERATOR_SDK)))
ifeq (, $(shell which operator-sdk 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPERATOR_SDK)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPERATOR_SDK) https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$${OS}_$${ARCH} ;\
	chmod +x $(OPERATOR_SDK) ;\
	}
else
OPERATOR_SDK = $(shell which operator-sdk)
endif
endif


.PHONY: opm
OPM = $(LOCALBIN)/opm
opm: ## Download opm locally if necessary.
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/v1.55.0/$${OS}-$${ARCH}-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif
