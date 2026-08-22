SHELL := /bin/bash
CLUSTER_NAME := gpu-sim
KIND_CONFIG := hack/kind-cluster.yaml

.PHONY: help
help: ## Show the available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: cluster-up
cluster-up: ## Create the kind cluster with KWOK, fake-gpu-operator and simulated GPU nodes
	hack/setup-cluster.sh

.PHONY: cluster-down
cluster-down: ## Delete the local kind cluster
	kind delete cluster --name $(CLUSTER_NAME)

.PHONY: install-kai
install-kai: ## Install KAI Scheduler on the running cluster
	hack/install-kai.sh

.PHONY: smoke
smoke: ## Run the Phase 0 smoke tests against the running cluster
	kubectl apply -f hack/smoke/kai-gang.yaml
	kubectl apply -f hack/smoke/kai-gang-overcapacity.yaml

.PHONY: build
build: ## Build the gpu-sim binary into bin/
	go build -o bin/gpu-sim ./cmd/gpu-sim

.PHONY: test
test: ## Run the unit tests
	go test ./...

.PHONY: fmt
fmt: ## Format the Go sources
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...
