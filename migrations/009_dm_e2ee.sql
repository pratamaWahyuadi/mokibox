-- =====================================================
-- 009_dm_e2ee.sql - E2EE direct-message relay.
-- Private keys and plaintext never enter this database.
-- Forward-only; apply after 008_user_avatar_upload.sql.
-- =====================================================

CREATE TABLE dm_devices (
    device_id UUID PRIMARY KEY,
    user_id UUID NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    identity_curve25519 TEXT NOT NULL,
    identity_ed25519 TEXT NOT NULL,
    next_inbox_sequence BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_dm_devices_curve_key_length
        CHECK (length(identity_curve25519) BETWEEN 1 AND 256),
    CONSTRAINT chk_dm_devices_ed_key_length
        CHECK (length(identity_ed25519) BETWEEN 1 AND 256),
    CONSTRAINT chk_dm_devices_next_sequence
        CHECK (next_inbox_sequence > 0),
    CONSTRAINT uq_dm_devices_device_user UNIQUE (device_id, user_id)
);

CREATE TABLE dm_one_time_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id UUID NOT NULL REFERENCES dm_devices(device_id) ON DELETE CASCADE,
    key_id TEXT NOT NULL,
    public_key TEXT NOT NULL,
    signature TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    claimed_at TIMESTAMPTZ,
    CONSTRAINT uq_dm_one_time_keys_device_key UNIQUE (device_id, key_id),
    CONSTRAINT chk_dm_one_time_key_id_length CHECK (length(key_id) BETWEEN 1 AND 128),
    CONSTRAINT chk_dm_one_time_key_public_length CHECK (length(public_key) BETWEEN 1 AND 256),
    CONSTRAINT chk_dm_one_time_key_signature_length CHECK (length(signature) BETWEEN 1 AND 512)
);

CREATE INDEX idx_dm_one_time_keys_available
    ON dm_one_time_keys(device_id, created_at, id)
    WHERE claimed_at IS NULL;

CREATE TABLE dm_inbox (
    message_id UUID PRIMARY KEY,
    sender_user_id UUID NOT NULL,
    sender_device_id UUID NOT NULL,
    recipient_user_id UUID NOT NULL,
    recipient_device_id UUID NOT NULL,
    sequence BIGINT NOT NULL,
    client_message_id UUID NOT NULL,
    protocol TEXT NOT NULL DEFAULT 'olm',
    protocol_version SMALLINT NOT NULL DEFAULT 1,
    message_type SMALLINT NOT NULL,
    ciphertext BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    acknowledged_at TIMESTAMPTZ,
    CONSTRAINT fk_dm_inbox_sender_device
        FOREIGN KEY (sender_device_id, sender_user_id)
        REFERENCES dm_devices(device_id, user_id) ON DELETE CASCADE,
    CONSTRAINT fk_dm_inbox_recipient_device
        FOREIGN KEY (recipient_device_id, recipient_user_id)
        REFERENCES dm_devices(device_id, user_id) ON DELETE CASCADE,
    CONSTRAINT uq_dm_inbox_recipient_sequence
        UNIQUE (recipient_device_id, sequence),
    CONSTRAINT chk_dm_inbox_sequence CHECK (sequence > 0),
    CONSTRAINT chk_dm_inbox_protocol CHECK (protocol = 'olm'),
    CONSTRAINT chk_dm_inbox_protocol_version CHECK (protocol_version = 1),
    CONSTRAINT chk_dm_inbox_message_type CHECK (message_type IN (0, 1)),
    CONSTRAINT chk_dm_inbox_ciphertext_size
        CHECK (ciphertext IS NULL OR octet_length(ciphertext) BETWEEN 1 AND 16384),
    CONSTRAINT chk_dm_inbox_expiry CHECK (expires_at > created_at),
    CONSTRAINT chk_dm_inbox_ack_ciphertext
        CHECK ((acknowledged_at IS NULL) = (ciphertext IS NOT NULL))
);

CREATE INDEX idx_dm_inbox_pending_poll
    ON dm_inbox(recipient_device_id, sequence)
    WHERE acknowledged_at IS NULL;

CREATE INDEX idx_dm_inbox_expiry ON dm_inbox(expires_at);
CREATE INDEX idx_dm_inbox_sender_user ON dm_inbox(sender_user_id);
CREATE INDEX idx_dm_inbox_recipient_user ON dm_inbox(recipient_user_id);

CREATE TABLE dm_idempotency (
    sender_device_id UUID NOT NULL REFERENCES dm_devices(device_id) ON DELETE CASCADE,
    client_message_id UUID NOT NULL,
    request_digest BYTEA NOT NULL,
    message_id UUID NOT NULL,
    response_sequence BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (sender_device_id, client_message_id),
    CONSTRAINT chk_dm_idempotency_digest_length CHECK (octet_length(request_digest) = 32),
    CONSTRAINT chk_dm_idempotency_expiry CHECK (expires_at > created_at),
    CONSTRAINT chk_dm_idempotency_sequence
        CHECK (response_sequence IS NULL OR response_sequence > 0)
);

CREATE INDEX idx_dm_idempotency_expiry ON dm_idempotency(expires_at);
