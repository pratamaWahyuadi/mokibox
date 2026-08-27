#!/usr/bin/env bash
# =====================================================
# bootstrap_db.sh
#
# Production-style role creation for the tiktok database.
# Reads TIKTOK_API_DB_PASSWORD and TIKTOK_WORKER_DB_PASSWORD
# from .env, then creates / updates the tiktok_api and
# tiktok_worker roles on the running PostgreSQL instance.
#
# This is the path used AFTER the first docker compose up
# (i.e. once the data volume exists and 01_roles.sql no
# longer runs automatically). The compose-managed init
# script is only for the very first boot.
#
# Usage:
#   make db-bootstrap
# or directly:
#   ./scripts/bootstrap_db.sh
# =====================================================

set -euo pipefail

# Resolve repo root regardless of where the script is invoked from
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${ROOT_DIR}"

# Load .env if present, without exporting comments / blanks
if [[ ! -f .env ]]; then
    echo "ERROR: .env not found at ${ROOT_DIR}/.env" >&2
    echo "       Copy .env.example to .env and fill in real values first." >&2
    exit 1
fi

set -a
# shellcheck disable=SC1091
source .env
set +a

: "${POSTGRES_USER:?POSTGRES_USER is required}"
: "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}"
: "${POSTGRES_DB:?POSTGRES_DB is required}"
: "${TIKTOK_API_DB_PASSWORD:?TIKTOK_API_DB_PASSWORD is required}"
: "${TIKTOK_WORKER_DB_PASSWORD:?TIKTOK_WORKER_DB_PASSWORD is required}"

# Connect to the postgres service inside the compose network.
# Requires `docker compose up postgres` to be running already.
COMPOSE_CMD="docker compose"

echo "==> Creating roles on postgres (db: ${POSTGRES_DB})"

# Use a single psql session with two \set variables, then CREATE ROLE.
${COMPOSE_CMD} exec -T postgres psql \
    -v ON_ERROR_STOP=1 \
    -U "${POSTGRES_USER}" \
    -d "${POSTGRES_DB}" \
    <<SQL
DO \$\$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tiktok_api') THEN
        CREATE ROLE tiktok_api LOGIN PASSWORD '${TIKTOK_API_DB_PASSWORD}';
    ELSE
        ALTER ROLE tiktok_api WITH LOGIN PASSWORD '${TIKTOK_API_DB_PASSWORD}';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tiktok_worker') THEN
        CREATE ROLE tiktok_worker LOGIN PASSWORD '${TIKTOK_WORKER_DB_PASSWORD}';
    ELSE
        ALTER ROLE tiktok_worker WITH LOGIN PASSWORD '${TIKTOK_WORKER_DB_PASSWORD}';
    END IF;
END
\$\$;

SELECT rolname, rolcanlogin FROM pg_roles
WHERE rolname IN ('tiktok_api', 'tiktok_worker')
ORDER BY rolname;
SQL

echo "==> Roles ready. Next: make db-migrate"
