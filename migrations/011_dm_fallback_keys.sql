-- =====================================================
-- 011_dm_fallback_keys.sql - Olm fallback key relay.
-- One key per active device, retained when one-time
-- keys are exhausted so a new chat can still start.
-- Forward-only; apply after 010_dm_inbox_sender_fk.sql.
-- =====================================================

CREATE TABLE dm_fallback_keys (
    device_id UUID PRIMARY KEY REFERENCES dm_devices(device_id) ON DELETE CASCADE,
    key_id TEXT NOT NULL,
    public_key TEXT NOT NULL,
    signature TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_dm_fallback_key_id_length CHECK (length(key_id) BETWEEN 1 AND 128),
    CONSTRAINT chk_dm_fallback_key_public_length CHECK (length(public_key) BETWEEN 1 AND 256),
    CONSTRAINT chk_dm_fallback_key_signature_length CHECK (length(signature) BETWEEN 1 AND 512),
    CONSTRAINT chk_dm_fallback_key_timestamps CHECK (updated_at >= created_at)
);
