# backend/Makefile — thin wrapper.
# Invoked from repo root via `make -C backend <target>` (root Makefile forwards here),
# or directly inside backend/. Runs go tooling in this module directory.
#
# The canonical targets live in the root Makefile; this file mirrors them so
# `make -C backend ...` works without bouncing back to the parent.

PKG := github.com/zhangguijunshi/backend
GO   := go

.PHONY: build test lint vet tidy fmt migrate-up migrate-down lint-tenant help

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' || \
		echo "targets: build test lint vet tidy fmt migrate-up migrate-down lint-tenant"

build: ## Build all packages
	$(GO) build ./...

test: ## Run all tests with the race detector
	$(GO) test -race -count=1 ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

vet: ## Run go vet
	$(GO) vet ./...

tidy: ## go mod tidy (run by orchestrator only)
	$(GO) mod tidy

fmt: ## Format Go sources
	$(GO) fmt ./...

migrate-up: ## Apply all pending up migrations
	$(GO) run ./cmd/migrate up

migrate-down: ## Roll back the most recent migration
	$(GO) run ./cmd/migrate down 1

lint-tenant: ## Tenant-isolation gate (go vet pass)
	$(GO) vet ./...
