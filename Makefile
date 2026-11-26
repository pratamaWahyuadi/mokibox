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
# ZITADEL_COMPOSE_DIR is the directory that holds the
# Zitadel identity-provider stack (Traefik + Zitadel
# v4 multi-service + its own Postgres). It is
# intentionally kept OUT of the MokiBox source tree so
# that Zitadel can be versioned / patched independently
# of the app.
#
# The repo itself does NOT pin a location. Common layouts:
#   - ../zitadel-compose/   (Zitadel as sibling of MokiBox,
#                            recommended for production - separate
#                            git repo, separate lifecycle)
#   - ./zitadel-compose/    (Zitadel as subdirectory of MokiBox -
#                            gitignored, kept around for convenience
#                            in this dev environment)
# Override per-invocation with:
#   make up-zitadel ZITADEL_COMPOSE_DIR=/path/to/zitadel-compose
ZITADEL_COMPOSE_DIR ?= ./zitadel-compose
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

.PHONY: db-migrate-avatar
db-migrate-avatar: ## Apply migrations/008_user_avatar_upload.sql before deploying avatar-aware gateway.
	@if [ ! -f $(MIGRATIONS)/008_user_avatar_upload.sql ]; then \
		echo "ERROR: $(MIGRATIONS)/008_user_avatar_upload.sql not found."; \
		exit 1; \
	fi
	@cat $(MIGRATIONS)/008_user_avatar_upload.sql | $(COMPOSE) exec -T $(SERVICE_DB) \
		psql -U $(POSTGRES_USER) -d $(DB_NAME) -v ON_ERROR_STOP=1

.PHONY: db-migrate-dm
db-migrate-dm: ## Apply E2EE DM migrations (009-011, forward-only).
	@for migration in 009_dm_e2ee.sql 010_dm_inbox_sender_fk.sql 011_dm_fallback_keys.sql; do \
		if [ ! -f $(MIGRATIONS)/$$migration ]; then \
			echo "ERROR: $(MIGRATIONS)/$$migration not found."; \
			exit 1; \
		fi; \
	done
	@for migration in 009_dm_e2ee.sql 010_dm_inbox_sender_fk.sql 011_dm_fallback_keys.sql; do \
		cat $(MIGRATIONS)/$$migration | $(COMPOSE) exec -T $(SERVICE_DB) \
			psql -U $(POSTGRES_USER) -d $(DB_NAME) -v ON_ERROR_STOP=1 || exit 1; \
	done

.PHONY: db-migrate-dm-existing
db-migrate-dm-existing: ## Apply DM 010-011 to a database that already has 009.
	@for migration in 010_dm_inbox_sender_fk.sql 011_dm_fallback_keys.sql; do \
		cat $(MIGRATIONS)/$$migration | $(COMPOSE) exec -T $(SERVICE_DB) \
			psql -U $(POSTGRES_USER) -d $(DB_NAME) -v ON_ERROR_STOP=1 || exit 1; \
	done

.PHONY: db-migrate-marketplace
db-migrate-marketplace: ## Apply Marketplace migrations 012-014 (forward-only).
	@for migration in 012_marketplace.sql 013_marketplace_cleanup_outbox.sql 014_marketplace_location.sql; do \
		if [ ! -f $(MIGRATIONS)/$$migration ]; then \
			echo "ERROR: $(MIGRATIONS)/$$migration not found."; \
			exit 1; \
		fi; \
		cat $(MIGRATIONS)/$$migration | $(COMPOSE) exec -T $(SERVICE_DB) \
			psql -U $(POSTGRES_USER) -d $(DB_NAME) -v ON_ERROR_STOP=1 || exit 1; \
	done

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
#
# Bring-up order on a fresh VPS:
#   1. make up-zitadel     # Zitadel identity provider
#   2. make up             # MokiBox app services
# `make up-all` runs both in order.
#
# The Zitadel stack lives in $(ZITADEL_COMPOSE_DIR)
# (default ../zitadel-compose). It is NOT a service in
# this Makefile's docker-compose.yml - keeping the two
# stacks separate is intentional, see planning/LLD_PLAN.md
# asumsi A11.
# -----------------------------------------------------------

.PHONY: up-zitadel
up-zitadel: ## Bring up the Zitadel identity provider (sibling compose).
	@if [ ! -d "$(ZITADEL_COMPOSE_DIR)" ]; then \
		echo "ERROR: ZITADEL_COMPOSE_DIR=$(ZITADEL_COMPOSE_DIR) does not exist."; \
		echo "Clone / create the Zitadel compose project there, or override with:"; \
		echo "  make up-zitadel ZITADEL_COMPOSE_DIR=/path/to/zitadel-compose"; \
		exit 1; \
	fi
	$(COMPOSE) -f $(ZITADEL_COMPOSE_DIR)/docker-compose.yml up -d
	@echo "Zitadel should be reachable at $$ZITADEL_ISSUER_URL (check Traefik in $(ZITADEL_COMPOSE_DIR))."

.PHONY: down-zitadel
down-zitadel: ## Bring down the Zitadel identity provider.
	$(COMPOSE) -f $(ZITADEL_COMPOSE_DIR)/docker-compose.yml down

.PHONY: up
up: ## docker compose up -d --build (MokiBox app services only).
	$(COMPOSE) up -d --build

.PHONY: up-all
up-all: up-zitadel up ## Bring up Zitadel then MokiBox app in one shot.

.PHONY: down
down: ## docker compose down MokiBox app (keep volumes). Does NOT touch Zitadel.
	$(COMPOSE) down

.PHONY: down-all
down-all: down down-zitadel ## Bring down both MokiBox app and Zitadel.

.PHONY: logs
logs: ## Tail logs of MokiBox app services.
	$(COMPOSE) logs -f

.PHONY: logs-zitadel
logs-zitadel: ## Tail logs of the Zitadel identity provider.
	$(COMPOSE) -f $(ZITADEL_COMPOSE_DIR)/docker-compose.yml logs -f

# -----------------------------------------------------------
# Tests
# -----------------------------------------------------------

.PHONY: test
test: ## Run go test ./...
	$(GO) test ./...

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

.PHONY: smoke-marketplace-local
smoke-marketplace-local: ## Run local Marketplace HTTP+DB smoke (requires DATABASE_URL).
	@test -n "$(DATABASE_URL)" || { echo "ERROR: DATABASE_URL is required"; exit 1; }
	@DATABASE_URL="$(DATABASE_URL)" $(GO) test -tags=integration ./api-gateway ./shared/db -run 'TestMarketplace' -count=1 -v

.PHONY: smoke-marketplace-stub-server
smoke-marketplace-stub-server: ## Start local stub-auth gateway for simulated live smoke.
	@test -n "$(DATABASE_URL)" || { echo "ERROR: DATABASE_URL is required"; exit 1; }
	@DATABASE_URL="$(DATABASE_URL)" MARKETPLACE_STUB_SERVER_ENABLED=1 $(GO) test -tags=integration ./api-gateway -run TestStubMarketplaceServer -count=1 -v

.PHONY: smoke-marketplace-live
smoke-marketplace-live: ## Run live/staging Marketplace smoke from .env.live.local.
	@set -a; . ./.env.live.local; set +a; \
	if [ "$$MARKETPLACE_SMOKE_MODE" = "local-simulation" ]; then echo "WARNING: .env.live.local berisi simulasi lokal, bukan staging asli"; fi; \
	for v in MARKETPLACE_API_URL DATABASE_URL MARKETPLACE_SMOKE_SELLER_USER MARKETPLACE_SMOKE_SELLER_TOKEN MARKETPLACE_SMOKE_BUYER_USER MARKETPLACE_SMOKE_BUYER_TOKEN; do \
		case "$${!v}" in ""|replace-*) echo "ERROR: $$v belum diisi di .env.live.local"; exit 1;; esac; \
	done; \
	$(GO) run ./scripts/smoketest/marketplace

.PHONY: clean
clean: ## Remove ./bin and Go test cache.
	rm -rf $(BIN_DIR)
	$(GO) clean -testcache

# -----------------------------------------------------------
# Reconciliation (issue #44)
# -----------------------------------------------------------

.PHONY: reconcile-once
reconcile-once: ## Enqueue one R2 orphan reconciliation sweep now (batch 100, not dry-run).
	$(GO) run ./scripts/smoketest/reconcile_enqueue -batch 100 -dry-run=false

.PHONY: reconcile-once-dry
reconcile-once-dry: ## Enqueue one reconciliation sweep in DRY-RUN mode (log only, no cleanup).
	$(GO) run ./scripts/smoketest/reconcile_enqueue -batch 100 -dry-run=true
