# Image URL to use all building/pushing image targets
IMG ?= controller:latest
IMAGE_REGISTRY_NAME ?= quay.io/open-cluster-management
IMAGE_NAME = cluster-proxy
IMAGE_TAG ?= latest
E2E_TEST_CLUSTER_NAME ?= e2e
CONTAINER_ENGINE ?= docker
# Produce CRDs that work back to Kubernetes 1.11 (no version conversion)
CRD_OPTIONS ?= "crd:crdVersions={v1},allowDangerousTypes=true,generateEmbeddedObjectMeta=true"

# Label filter for e2e tests (Ginkgo v2 label filter expression)
# Examples: "install", "connectivity", "certificate && !rotation", etc.
LABEL_FILTER ?=

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

verify: fmt vet

test: manifests generate fmt vet ## Run tests.
	go test ./pkg/... -coverprofile cover.out

##@ Build

build: generate fmt vet
	go build -o bin/addon-manager cmd/addon-manager/main.go
	go build -o bin/addon-agent cmd/addon-agent/main.go
	go build -o bin/cluster-proxy cmd/cluster-proxy/main.go

docker-build: test ## Build docker image with the manager.
	$(CONTAINER_ENGINE) build -t ${IMG} .

docker-push: ## Push docker image with the manager.
	$(CONTAINER_ENGINE) push ${IMG}

##@ Deployment

CONTROLLER_GEN = $(shell pwd)/bin/controller-gen
controller-gen: ## Download controller-gen locally if necessary.
	$(call go-get-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.0)

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
	go install k8s.io/code-generator/cmd/client-gen@v0.29.2
	go install sigs.k8s.io/apiserver-runtime/tools/apiserver-runtime-gen@v1.1.1
	apiserver-runtime-gen \
 	--module open-cluster-management.io/cluster-proxy \
 	-g client-gen \
 	-g informer-gen \
 	-g lister-gen \
 	--versions=open-cluster-management.io/cluster-proxy/pkg/apis/proxy/v1alpha1

images:
	$(CONTAINER_ENGINE) build \
		-f cmd/Dockerfile \
		--build-arg ADDON_AGENT_IMAGE_NAME=$(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_TAG) \
		-t $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_TAG) .

images-amd64:
	$(CONTAINER_ENGINE) buildx build \
		--platform linux/amd64 \
		--load \
		-f cmd/Dockerfile \
		--build-arg ADDON_AGENT_IMAGE_NAME=$(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_TAG) \
		-t $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_TAG) .

## Integration Testing

ENVTEST = $(shell pwd)/bin/setup-envtest
ENVTEST_K8S_VERSION = 1.31.0
setup-envtest: ## Download setup-envtest locally if necessary.
	$(call go-get-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest@latest)

test-integration: manifests generate fmt vet setup-envtest
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(shell pwd)/testbin -p path)" \
		go test ./test/integration/... -coverprofile cover.out

## E2E Testing

# Note: here we use internal service ns as the entrypointAddress. The test cluster should be registered to itself as a managed cluster.
setup-env-for-e2e:
	@echo "Setting up environment for e2e tests..."
	./test/e2e/env/init.sh
.PHONY: setup-env-for-e2e

# load cluster-proxy image into kind cluster
load-cluster-proxy-image-kind:
	@echo "Loading cluster-proxy image into kind cluster..."
	kind load docker-image $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_TAG) --name $(E2E_TEST_CLUSTER_NAME)
.PHONY: load-cluster-proxy-image-kind

# delete cluster-proxy image from kind cluster nodes
delete-cluster-proxy-image-from-kind:
	@echo "Deleting cluster-proxy image from kind cluster nodes..."
	@for node in $$(kind get nodes --name $(E2E_TEST_CLUSTER_NAME) 2>/dev/null || echo ""); do \
		if [ -n "$$node" ]; then \
			docker exec $$node crictl rmi $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_TAG) 2>/dev/null || true; \
		fi; \
	done
.PHONY: delete-cluster-proxy-image-from-kind

deploy-cluster-proxy-e2e: delete-cluster-proxy-image-from-kind load-cluster-proxy-image-kind
	@echo "Deploying cluster-proxy..."
	helm install \
	-n open-cluster-management-addon --create-namespace \
	cluster-proxy charts/cluster-proxy \
	--set registry=$(IMAGE_REGISTRY_NAME) \
	--set image=$(IMAGE_NAME) \
	--set tag=$(IMAGE_TAG) \
	--set proxyServerImage=$(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME) \
	--set proxyAgentImage=$(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME) \
	--set proxyServer.entrypointAddress="proxy-entrypoint.open-cluster-management-addon.svc" \
	--set proxyServer.port=8091 \
	--set enableServiceProxy=true \
	--set userServer.enabled=true
	@echo "Cluster-proxy deployed successfully!"
.PHONY: deploy-cluster-proxy-e2e

# Build e2e test container image
build-e2e-image:
	@echo "Building e2e test container image..."
	$(CONTAINER_ENGINE) build \
		-f test/e2e/Dockerfile \
		-t $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME)-e2e:$(IMAGE_TAG) .
.PHONY: build-e2e-image

# Load e2e image into kind cluster (for local testing)
load-e2e-image-kind:
	@echo "Loading e2e image into kind cluster..."
	kind load docker-image $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME)-e2e:$(IMAGE_TAG) --name $(E2E_TEST_CLUSTER_NAME)
.PHONY: load-e2e-image-kind

# Delete e2e image from kind cluster nodes (for rapid iteration)
delete-e2e-image-from-kind:
	@echo "Deleting e2e image from kind cluster nodes..."
	@for node in $$(kind get nodes --name $(E2E_TEST_CLUSTER_NAME) 2>/dev/null || echo ""); do \
		if [ -n "$$node" ]; then \
			docker exec $$node crictl rmi $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME)-e2e:$(IMAGE_TAG) 2>/dev/null || true; \
		fi; \
	done
.PHONY: delete-e2e-image-from-kind

# Run e2e tests in cluster using container image (Kubernetes-native approach)
# Use LABEL_FILTER to run specific tests, e.g.: make test-e2e LABEL_FILTER="install"
test-e2e: delete-e2e-image-from-kind build-e2e-image load-e2e-image-kind
	@echo "Deleting existing e2e test job if present..."
	@kubectl delete job cluster-proxy-e2e -n open-cluster-management-addon --ignore-not-found
	@echo "Deploying e2e test job..."
	@if [ -n "$(LABEL_FILTER)" ]; then \
		echo "Running tests with label filter: $(LABEL_FILTER)"; \
	fi
	@sed -e '/name: LABEL_FILTER/{n;s|value: ""|value: "$(LABEL_FILTER)"|;}' \
	     -e 's|image: quay.io/open-cluster-management/cluster-proxy-e2e:latest|image: $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME)-e2e:$(IMAGE_TAG)|g' \
	     test/e2e/env/job.yaml | kubectl apply -f -
	@./test/e2e/env/wait-for-job.sh cluster-proxy-e2e open-cluster-management-addon 1200
.PHONY: test-e2e

# Rapid iteration workflow for e2e tests (cleans up everything first)
# Use LABEL_FILTER to run specific tests, e.g.: make retest-e2e LABEL_FILTER="connectivity"
retest-e2e: clean-e2e delete-e2e-image-from-kind build-e2e-image load-e2e-image-kind
	@echo "Deleting existing e2e test job if present..."
	@kubectl delete job cluster-proxy-e2e -n open-cluster-management-addon --ignore-not-found
	@echo "Deploying e2e test job..."
	@if [ -n "$(LABEL_FILTER)" ]; then \
		echo "Running tests with label filter: $(LABEL_FILTER)"; \
	fi
	@sed -e '/name: LABEL_FILTER/{n;s|value: ""|value: "$(LABEL_FILTER)"|;}' \
	     -e 's|image: quay.io/open-cluster-management/cluster-proxy-e2e:latest|image: $(IMAGE_REGISTRY_NAME)/$(IMAGE_NAME)-e2e:$(IMAGE_TAG)|g' \
	     test/e2e/env/job.yaml | kubectl apply -f -
	@./test/e2e/env/wait-for-job.sh cluster-proxy-e2e open-cluster-management-addon 1200
.PHONY: retest-e2e

# Clean up e2e test job and related resources
clean-e2e:
	@echo "Cleaning up e2e test resources..."
	kubectl delete job/cluster-proxy-e2e -n open-cluster-management-addon --ignore-not-found=true
	kubectl delete serviceaccount/cluster-proxy-e2e -n open-cluster-management-addon --ignore-not-found=true
	kubectl delete clusterrolebinding/cluster-proxy-e2e --ignore-not-found=true
	kubectl delete clusterrole/cluster-proxy-e2e --ignore-not-found=true
.PHONY: clean-e2e

# Quick verify of user-server
# Example result:
# {
#   "kind": "APIVersions",
#   "versions": [
#     "v1"
#   ],
#   "serverAddressByClientCIDRs": [
#     {
#       "clientCIDR": "0.0.0.0/0",
#       "serverAddress": "172.17.0.2:6443"
#     }
#   ]
# }
verify-user-server:
	@echo "Verifying user-server..."
	TOKEN=$$(kubectl create token default -n default) && POD=$$(kubectl get pods -n open-cluster-management-addon -l component=cluster-proxy-addon-user --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}') && kubectl debug -it $$POD -n open-cluster-management-addon --image=praqma/network-multitool -- sh -c "curl -k -H 'Authorization: Bearer $$TOKEN' https://cluster-proxy-addon-user.open-cluster-management-addon.svc.cluster.local:9092/loopback/api"
.PHONY: verify-user-server
