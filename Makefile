SHELL := /bin/bash
CLUSTER_NAME := gpu-sim
KIND_CONFIG := hack/kind-cluster.yaml
TOPOLOGY ?= topologies/two-racks-h100.yaml

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

.PHONY: scenarios
scenarios: ## Run the scenario suite against the running cluster
	go run ./cmd/gpu-sim run scenarios/

.PHONY: build
build: ## Build the binaries into bin/
	go build -o bin/ ./cmd/...

.PHONY: topology
topology: ## Apply TOPOLOGY (default: topologies/two-racks-h100.yaml) to the running cluster
	go run ./cmd/gpu-sim topology apply -f $(TOPOLOGY)

.PHONY: render
render: ## Print the objects TOPOLOGY would create, without touching the cluster
	go run ./cmd/gpu-sim topology render -f $(TOPOLOGY)

.PHONY: test
test: ## Run the unit tests
	go test ./...

.PHONY: fmt
fmt: ## Format the Go sources
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...
