-- Marketplace cleanup outbox. Forward-only schema.
--
-- R2 cleanup must survive a Redis/enqueue failure: the marketplace handler
-- records the keys in this table inside the same transaction that removes
-- the database row. The api-gateway janitor dispatches pending rows and
-- retries with backoff. cleanup:objects is idempotent, so a replay after
-- a crash between enqueue and status update is safe.

CREATE TABLE marketplace_cleanup_outbox (
    id              BIGSERIAL PRIMARY KEY,
    r2_keys         TEXT[] NOT NULL CHECK (cardinality(r2_keys) > 0),
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'failed', 'done')),
    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_marketplace_cleanup_outbox_due
    ON marketplace_cleanup_outbox (id)
    WHERE status = 'pending';

GRANT SELECT, INSERT, UPDATE, DELETE ON marketplace_cleanup_outbox TO tiktok_api;
