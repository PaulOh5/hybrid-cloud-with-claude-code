.PHONY: help dev build test lint proto migrate-up migrate-down migrate-reset e2e fmt clean tidy dev-db dev-db-down

GO_SERVICES := ./services/compute-agent/... ./services/main-api/... ./services/ssh-proxy/... ./shared/...
FRONTEND_DIR := web/frontend
GOBIN := $(shell go env GOPATH)/bin
DATABASE_URL ?= postgres://hybrid:hybrid@localhost:5432/hybrid?sslmode=disable
MIGRATIONS_DIR := services/main-api/db/migrations

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# --- dev ---
dev-db: ## Start local postgres in docker
	docker compose -f infra/docker-compose.dev.yml up -d postgres

dev-db-down: ## Stop local postgres
	docker compose -f infra/docker-compose.dev.yml down

dev: dev-db ## Start local stack
	@echo "postgres up — start services manually via 'go run ./services/<name>/cmd/<name>'"

# --- build/test/lint ---
build: ## Build all services into bin/
	@mkdir -p bin
	go build -o bin/compute-agent ./services/compute-agent/cmd/compute-agent
	go build -o bin/main-api ./services/main-api/cmd/main-api
	go build -o bin/ssh-proxy ./services/ssh-proxy/cmd/ssh-proxy
	cd $(FRONTEND_DIR) && pnpm build

test: ## Run unit tests
	go test -race -count=1 $(GO_SERVICES)
	cd $(FRONTEND_DIR) && pnpm test

test-integration: ## Run integration tests (requires docker)
	go test -race -tags=integration -count=1 $(GO_SERVICES)

lint: ## Lint all code
	$(GOBIN)/golangci-lint run $(GO_SERVICES)
	cd $(FRONTEND_DIR) && pnpm lint

fmt: ## Format all code
	gofmt -s -w services
	cd $(FRONTEND_DIR) && pnpm format

tidy: ## Tidy go modules
	cd services/compute-agent && go mod tidy
	cd services/main-api && go mod tidy
	cd services/ssh-proxy && go mod tidy

clean: ## Clean build artifacts
	rm -rf bin/ dist/ coverage.out

# --- proto ---
proto: ## Generate proto stubs
	@./scripts/gen-proto.sh

# --- db ---
migrate-up: ## Apply migrations
	$(GOBIN)/goose -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

migrate-down: ## Rollback one migration
	$(GOBIN)/goose -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" down

migrate-reset: ## Drop all migrations and reapply
	$(GOBIN)/goose -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" reset
	$(GOBIN)/goose -dir $(MIGRATIONS_DIR) postgres "$(DATABASE_URL)" up

sqlc: ## Generate sqlc queries
	cd services/main-api && $(GOBIN)/sqlc generate

# --- e2e ---
e2e: ## Run e2e tests
	cd $(FRONTEND_DIR) && pnpm test:e2e
