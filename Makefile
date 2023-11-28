
# Image URL to use all building/pushing image targets
IMG ?= controller:latest
IMAGE_REGISTRY_NAME ?= quay.io/open-cluster-management
IMAGE_NAME = cluster-proxy
IMAGE_TAG ?= latest
E2E_TEST_CLUSTER_NAME ?= loopback
# Produce CRDs that work back to Kubernetes 1.11 (no version conversion)
CRD_OPTIONS ?= "crd:trivialVersions=true,preserveUnknownFields=false"

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# This is a requirement for 'setup-envtest.sh' in the test target.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) $(CRD_OPTIONS) \
		paths="./pkg/apis/..." \
		rbac:roleName=manager-role \
		output:crd:artifacts:config=hack/crd/bases

generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./pkg/apis/..."

fmt: ## Run go fmt against code.
	go fmt ./...

vet: ## Run go vet against code.
	go vet ./...

golint:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.54.1
	golangci-lint run --timeout=3m ./...

verify: fmt vet golint

test: manifests generate fmt vet ## Run tests.
	go test ./pkg/... -coverprofile cover.out

##@ Build

build: generate fmt vet
	go build -o bin/addon-manager cmd/addon-manager/main.go
	go build -o bin/addon-agent cmd/addon-agent/main.go

docker-build: test ## Build docker image with the manager.
	docker build -t ${IMG} .

docker-push: ## Push docker image with the manager.
	docker push ${IMG}

##@ Deployment

install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl delete -f -

deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | kubectl apply -f -

undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/default | kubectl delete -f -


CONTROLLER_GEN = $(shell pwd)/bin/controller-gen
controller-gen: ## Download controller-gen locally if necessary.
	$(call go-get-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen@v0.4.1)

KUSTOMIZE = $(shell pwd)/bin/kustomize
kustomize: ## Download kustomize locally if necessary.
	$(call go-get-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v3@v3.8.7)

# go-get-tool will 'go get' any package $2 and install it to $1.
PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))
define go-get-tool
@[ -f $(1) ] || { \
set -e ;\
TMP_DIR=$$(mktemp -d) ;\
cd $$TMP_DIR ;\
go mod init tmp ;\
echo "Downloading $(2)" ;\
GOBIN=$(PROJECT_DIR)/bin go install $(2) ;\
rm -rf $$TMP_DIR ;\
}
endef

client-gen:
	go install k8s.io/code-generator/cmd/client-gen@v0.23.0
	go install sigs.k8s.io/apiserver-runtime/tools/apiserver-runtime-gen@v1.1.1
	apiserver-runtime-gen \
 	--module open-cluster-management.io/cluster-proxy \
 	-g client-gen \
 	-g informer-gen \
 	-g lister-gen \
 	--versions=open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1

images:
	docker build \
		-f cmd/Dockerfile \
		--build-arg ADDON_AGENT_IMAGE_NAME=$(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_TAG) \
		-t $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_TAG) .

pure-image:
	docker build \
		-f cmd/pure.Dockerfile \
		--build-arg ADDON_AGENT_IMAGE_NAME=$(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_TAG) \
		-t $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_TAG) .

ENVTEST_ASSETS_DIR=$(shell pwd)/testbin
test-integration: manifests generate fmt vet
	mkdir -p ${ENVTEST_ASSETS_DIR}
	test -f ${ENVTEST_ASSETS_DIR}/setup-envtest.sh || curl -sSLo ${ENVTEST_ASSETS_DIR}/setup-envtest.sh https://raw.githubusercontent.com/kubernetes-sigs/controller-runtime/v0.8.3/hack/setup-envtest.sh
	source ${ENVTEST_ASSETS_DIR}/setup-envtest.sh; \
		fetch_envtest_tools $(ENVTEST_ASSETS_DIR); \
		setup_envtest_env $(ENVTEST_ASSETS_DIR); \
		go test ./test/integration/... -coverprofile cover.out

e2e-job-image:
	docker build \
		-f test/e2e/job/Dockerfile \
		-t $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME)-e2e-job:$(IMAGE_TAG) .

build-e2e:
	go test -c -o bin/e2e ./test/e2e/

test-e2e: build-e2e
	./bin/e2e --test-cluster $(E2E_TEST_CLUSTER_NAME)
