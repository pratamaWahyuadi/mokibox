-- =====================================================
-- 01_app_roles.sql
-- Creates the MokiBox app's two application database
-- roles. The Zitadel identity provider is NOT bootstrapped
-- here - it lives in its own Postgres instance in the
-- sibling zitadel-compose project (see planning/LLD_PLAN.md
-- asumsi A11 for the rationale).
--
-- IMPORTANT:
--   This file is mounted into the MokiBox postgres
--   container at /docker-entrypoint-initdb.d:ro and
--   runs ONLY on the FIRST initialization of the data
--   directory. The passwords below are placeholders
--   for local development only.
--
--   For real deployments, run scripts/bootstrap_db.sh
--   (which reads TIKTOK_API_DB_PASSWORD and
--   TIKTOK_WORKER_DB_PASSWORD from .env) and rotate the
--   placeholder passwords.
-- =====================================================

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tiktok_api') THEN
        CREATE ROLE tiktok_api LOGIN PASSWORD 'change-me-tiktok-api';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tiktok_worker') THEN
        CREATE ROLE tiktok_worker LOGIN PASSWORD 'change-me-tiktok-worker';
    END IF;
END
$$;

-- Table-level privileges are granted in migrations/001_init.sql
-- (Phase 1) after the schema is created. This file only
-- creates the roles.
