# Variables
APP_NAME := prometheus-multi-tenant-proxy
VERSION := $(shell git describe --tags --always --dirty)
COMMIT := $(shell git rev-parse --short HEAD)
BUILD_TIME := $(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)

# Docker
DOCKER_REGISTRY := bnkarthik6
DOCKER_IMAGE := $(DOCKER_REGISTRY)/$(APP_NAME)
DOCKER_TAG := $(VERSION)

# Kubernetes
NAMESPACE := monitoring

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: deps
deps: ## Download dependencies
	go mod download
	go mod tidy

.PHONY: generate
generate: ## Generate code (CRDs, etc.)
	go generate ./...

.PHONY: fmt
fmt: ## Format code
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: test
test: ## Run tests
	go test -v ./...

.PHONY: test-coverage
test-coverage: ## Run tests with coverage
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

##@ Build

.PHONY: build
build: ## Build the binary
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(APP_NAME) ./cmd/proxy

.PHONY: build-linux
build-linux: ## Build the binary for Linux
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o bin/$(APP_NAME)-linux ./cmd/proxy

.PHONY: clean
clean: ## Clean build artifacts
	rm -rf bin/
	rm -f coverage.out coverage.html

##@ Docker

.PHONY: docker-build
docker-build: ## Build Docker image
	docker build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .
	docker tag $(DOCKER_IMAGE):$(DOCKER_TAG) $(DOCKER_IMAGE):latest

.PHONY: docker-push
docker-push: ## Push Docker image
	docker push $(DOCKER_IMAGE):$(DOCKER_TAG)
	docker push $(DOCKER_IMAGE):latest

.PHONY: docker-run
docker-run: ## Run Docker container locally
	docker run --rm -p 8080:8080 $(DOCKER_IMAGE):latest

##@ Kubernetes

.PHONY: k8s-namespace
k8s-namespace: ## Create Kubernetes namespace
	kubectl create namespace $(NAMESPACE) --dry-run=client -o yaml | kubectl apply -f -

.PHONY: k8s-crd
k8s-crd: ## Apply Custom Resource Definitions
	kubectl apply -f deploy/kubernetes/crd.yaml

.PHONY: k8s-config
k8s-config: ## Apply ConfigMap
	kubectl apply -f deploy/kubernetes/configmap.yaml

.PHONY: k8s-deploy
k8s-deploy: k8s-namespace k8s-crd k8s-config ## Deploy to Kubernetes
	kubectl apply -f deploy/kubernetes/deployment.yaml

.PHONY: k8s-delete
k8s-delete: ## Delete from Kubernetes
	kubectl delete -f deploy/kubernetes/deployment.yaml --ignore-not-found=true
	kubectl delete -f deploy/kubernetes/configmap.yaml --ignore-not-found=true

.PHONY: k8s-logs
k8s-logs: ## Show logs
	kubectl logs -f deployment/$(APP_NAME) -n $(NAMESPACE)

.PHONY: k8s-status
k8s-status: ## Show deployment status
	kubectl get all -n $(NAMESPACE) -l app=$(APP_NAME)

##@ Examples

.PHONY: examples-apply
examples-apply: ## Apply example MetricAccess resources
	kubectl apply -f examples/tenant-basic.yaml
	kubectl apply -f examples/tenant-with-labels.yaml
	kubectl apply -f examples/tenant-with-remote-write.yaml

.PHONY: examples-delete
examples-delete: ## Delete example MetricAccess resources
	kubectl delete -f examples/tenant-basic.yaml --ignore-not-found=true
	kubectl delete -f examples/tenant-with-labels.yaml --ignore-not-found=true
	kubectl delete -f examples/tenant-with-remote-write.yaml --ignore-not-found=true

##@ Development Tools

.PHONY: dev-setup
dev-setup: ## Setup development environment
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

.PHONY: dev-run
dev-run: ## Run locally for development
	go run ./cmd/proxy --config=examples/config.yaml --log-level=debug

.PHONY: dev-test-request
dev-test-request: ## Test a request to the proxy
	curl -H "X-Tenant-Namespace: myusernamespace" http://localhost:8080/api/v1/query?query=up

##@ All-in-one

.PHONY: all
all: deps fmt vet test build ## Run all checks and build

.PHONY: ci
ci: deps fmt vet lint test build ## Run CI pipeline

.PHONY: release
release: ci docker-build docker-push ## Build and release 