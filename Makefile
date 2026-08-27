# =====================================================
# TikTok Clone Backend MVP - Makefile
# Single source of truth for local dev + manual deploy.
# =====================================================

# Force bash so $(...) and pipefail behave the same everywhere.
SHELL := /bin/bash

# Load .env into the Makefile environment so targets can
# reference vars like POSTGRES_DB and DATABASE_URL.
ifneq (,$(wildcard ./.env))
    include .env
    export
endif

COMPOSE       := docker compose
SERVICE_DB    := postgres
SERVICE_REDIS := redis
DB_NAME       ?= tiktok
MIGRATIONS    := migrations
SQLC_DIR      := sqlc

GO            ?= go
BIN_DIR       := bin
API_BIN       := $(BIN_DIR)/api-gateway
WORKER_BIN    := $(BIN_DIR)/transcoder-worker

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# -----------------------------------------------------------
# Database & Redis
# -----------------------------------------------------------

.PHONY: db-up
db-up: ## Start only postgres + redis.
	$(COMPOSE) up -d $(SERVICE_DB) $(SERVICE_REDIS)

.PHONY: db-down
db-down: ## Stop postgres + redis (keeps volumes).
	$(COMPOSE) stop $(SERVICE_DB) $(SERVICE_REDIS)

.PHONY: db-bootstrap
db-bootstrap: ## Create / update tiktok_api and tiktok_worker roles.
	./scripts/bootstrap_db.sh

.PHONY: db-migrate
db-migrate: ## Apply migrations/001_init.sql (forward-only, no rollback).
	@if [ ! -f $(MIGRATIONS)/001_init.sql ]; then \
		echo "ERROR: $(MIGRATIONS)/001_init.sql not found. Phase 1 has not landed yet."; \
		exit 1; \
	fi
	@cat $(MIGRATIONS)/001_init.sql | $(COMPOSE) exec -T $(SERVICE_DB) \
		psql -U $(POSTGRES_USER) -d $(DB_NAME) -v ON_ERROR_STOP=1

.PHONY: db-psql
db-psql: ## Open a psql shell as POSTGRES_USER on $(DB_NAME).
	$(COMPOSE) exec $(SERVICE_DB) psql -U $(POSTGRES_USER) -d $(DB_NAME)

# -----------------------------------------------------------
# sqlc
# -----------------------------------------------------------

.PHONY: sqlc-gen
sqlc-gen: ## Run sqlc generate into shared/db.
	@command -v sqlc >/dev/null 2>&1 || { \
		echo "ERROR: sqlc not installed. Install: go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest"; \
		exit 1; \
	}
	cd $(SQLC_DIR) && sqlc generate

# -----------------------------------------------------------
# Build
# -----------------------------------------------------------

.PHONY: build
build: ## Build api-gateway and transcoder-worker binaries into ./bin.
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(API_BIN)    ./api-gateway
	$(GO) build -o $(WORKER_BIN) ./transcoder-worker

.PHONY: build-api
build-api:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(API_BIN) ./api-gateway

.PHONY: build-worker
build-worker:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(WORKER_BIN) ./transcoder-worker

.PHONY: tidy
tidy: ## Run go mod tidy.
	$(GO) mod tidy

# -----------------------------------------------------------
# Compose
# -----------------------------------------------------------

.PHONY: up
up: ## docker compose up -d --build (all services).
	$(COMPOSE) up -d --build

.PHONY: down
down: ## docker compose down (keep volumes).
	$(COMPOSE) down

.PHONY: logs
logs: ## Tail logs of all services.
	$(COMPOSE) logs -f

# -----------------------------------------------------------
# Tests
# -----------------------------------------------------------

.PHONY: test
test: ## Run go test ./...
	$(GO) test ./...

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

.PHONY: clean
clean: ## Remove ./bin and Go test cache.
	rm -rf $(BIN_DIR)
	$(GO) clean -testcache
