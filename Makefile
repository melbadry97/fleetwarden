# fleetwarden — see docs/GUIDE.md
SHELL := /bin/bash
BINDIR := bin
IMG ?= fleetwarden:dev
KIND_CLUSTER ?= fleetwarden
ENVTEST_K8S_VERSION ?= 1.29.0

.DEFAULT_GOAL := help

##@ General

help: ## Show this help
	@awk 'BEGIN{FS=":.*##"; printf "\nUsage: make <target>\n"} \
	/^[a-zA-Z_-]+:.*?##/ {printf "  \033[36m%-22s\033[0m %s\n",$$1,$$2} \
	/^##@/ {printf "\n\033[1m%s\033[0m\n",substr($$0,5)}' $(MAKEFILE_LIST)

##@ Development

deps: ## Download Go module dependencies
	go mod download
	go mod tidy

generate: controller-gen ## Generate deepcopy methods
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths="./api/..."

manifests: controller-gen ## Generate CRDs, RBAC and webhook manifests
	$(CONTROLLER_GEN) crd rbac:roleName=fleetwarden-manager webhook \
		paths="./api/...;./controllers/...;./webhooks/..." \
		output:crd:artifacts:config=config/crd \
		output:rbac:artifacts:config=config/rbac \
		output:webhook:artifacts:config=config/webhook

fmt: ## Format
	go fmt ./...

vet: ## Vet
	go vet ./...

##@ Testing

test-core: ## Unit test the dependency-free core (no cluster, no k8s modules)
	go test -race -cover ./internal/... ./cmd/fakeredfish/...

test: manifests generate fmt vet envtest ## Full test suite including envtest
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test -race -coverprofile=cover.out ./...

cover: test ## Open the coverage report
	go tool cover -html=cover.out

##@ Build

build: ## Build the manager binary
	go build -o $(BINDIR)/manager ./cmd/manager

build-sim: ## Build the Redfish simulator
	go build -o $(BINDIR)/fakeredfish ./cmd/fakeredfish

docker-build: ## Build the container image
	docker build -t $(IMG) .

##@ Demo

kind-up: ## Create a kind cluster with kwok installed
	kind create cluster --name $(KIND_CLUSTER) || true
	kubectl apply -f https://github.com/kubernetes-sigs/kwok/releases/latest/download/kwok.yaml
	kubectl apply -f https://github.com/kubernetes-sigs/kwok/releases/latest/download/stage-fast.yaml

kind-down: ## Delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

fake-nodes: ## Create 500 simulated GPU nodes via kwok
	./hack/fake-nodes.sh 500

install: manifests ## Install CRDs into the current cluster
	kubectl apply -f config/crd

deploy: install ## Deploy the manager
	kubectl apply -f config/rbac
	kubectl apply -f config/manager

samples: ## Apply the sample NodeClass and NodeHealthPolicy
	kubectl apply -f config/samples

demo: kind-up fake-nodes install samples ## Full local demo from nothing
	@echo ""
	@echo "Cluster is up with 500 simulated nodes."
	@echo "Run the manager locally:  make run"
	@echo "Then inject chaos:        ./hack/chaos.sh ecc 40"

run: manifests generate ## Run the manager against the current kubeconfig
	go run ./cmd/manager -enable-webhooks=false -leader-elect=false

##@ Tools

CONTROLLER_GEN := $(shell pwd)/$(BINDIR)/controller-gen
controller-gen: ## Install controller-gen
	@test -f $(CONTROLLER_GEN) || GOBIN=$(shell pwd)/$(BINDIR) \
		go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.14.0

ENVTEST := $(shell pwd)/$(BINDIR)/setup-envtest
envtest: ## Install setup-envtest
	@test -f $(ENVTEST) || GOBIN=$(shell pwd)/$(BINDIR) \
		go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

.PHONY: help deps generate manifests fmt vet test test-core cover build build-sim \
	docker-build kind-up kind-down fake-nodes install deploy samples demo run \
	controller-gen envtest
