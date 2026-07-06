# Root Makefile for zhanggui-junshi monorepo.
# Routes to the backend module. Run from repo root: `make <target>`.
# (This machine has no Docker; `make dev` only defines the target — it is not
#  executed during M0-S1 per CONTRACT.md.)

COMPOSE_FILE := infra/compose/docker-compose.yml
BACKEND_DIR  := backend

.PHONY: dev build test lint vet tidy fmt migrate-up migrate-down lint-tenant help

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

dev: ## Start local deps (postgres/redis/minio) via docker compose
	@echo ">> 提示: 启动本地依赖请执行 docker compose -f $(COMPOSE_FILE) up -d"
	@echo ">> (本机无 Docker 时跳过; compose 文件: $(COMPOSE_FILE))"
	docker compose -f $(COMPOSE_FILE) up -d

build: ## Build all backend packages
	cd $(BACKEND_DIR) && go build ./...

test: ## Run backend tests with the race detector
	cd $(BACKEND_DIR) && go test -race ./...

lint: ## Run golangci-lint over the backend
	cd $(BACKEND_DIR) && golangci-lint run ./...

vet: ## Run go vet over the backend
	cd $(BACKEND_DIR) && go vet ./...

tidy: ## Tidy backend go.mod (run by the orchestrator, never concurrently by agents)
	cd $(BACKEND_DIR) && go mod tidy

fmt: ## Format backend Go sources
	cd $(BACKEND_DIR) && go fmt ./...

migrate-up: ## Apply all pending up migrations
	cd $(BACKEND_DIR) && go run ./cmd/migrate up

migrate-down: ## Roll back the most recent migration
	cd $(BACKEND_DIR) && go run ./cmd/migrate down 1

lint-tenant: ## Tenant-isolation gate (go vet pass over internal/)
	cd $(BACKEND_DIR) && go vet ./...
