CREATE TABLE dm_devices (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id               UUID NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    protocol              TEXT NOT NULL,
    protocol_version      INTEGER NOT NULL CHECK (protocol_version > 0),
    key_bundle            BYTEA NOT NULL,
    fallback_key_id       TEXT,
    fallback_key_material BYTEA,
    next_sequence         BIGINT NOT NULL DEFAULT 0 CHECK (next_sequence >= 0),
    registered_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_dm_device_protocol
        CHECK (char_length(protocol) BETWEEN 1 AND 64),
    CONSTRAINT chk_dm_device_key_bundle_size
        CHECK (octet_length(key_bundle) BETWEEN 1 AND 65536),
    CONSTRAINT chk_dm_device_fallback_pair
        CHECK ((fallback_key_id IS NULL) = (fallback_key_material IS NULL)),
    CONSTRAINT chk_dm_device_fallback_key_size
        CHECK (fallback_key_material IS NULL OR
               octet_length(fallback_key_material) BETWEEN 1 AND 4096),
    CONSTRAINT chk_dm_device_fallback_key_id
        CHECK (fallback_key_id IS NULL OR char_length(fallback_key_id) BETWEEN 1 AND 64)
);

CREATE TABLE dm_one_time_keys (
    device_id    UUID NOT NULL REFERENCES dm_devices(id) ON DELETE CASCADE,
    key_id       TEXT NOT NULL CHECK (char_length(key_id) BETWEEN 1 AND 64),
    key_material BYTEA NOT NULL CHECK (octet_length(key_material) BETWEEN 1 AND 4096),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (device_id, key_id)
);

CREATE TABLE dm_inbox (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    recipient_device_id  UUID NOT NULL REFERENCES dm_devices(id) ON DELETE CASCADE,
    recipient_user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    sequence_no          BIGINT NOT NULL CHECK (sequence_no > 0),
    sender_user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    sender_device_id     UUID NOT NULL,
    protocol             TEXT NOT NULL CHECK (char_length(protocol) BETWEEN 1 AND 64),
    version              INTEGER NOT NULL CHECK (version > 0),
    ciphertext           BYTEA NOT NULL CHECK (octet_length(ciphertext) BETWEEN 1 AND 65536),
    client_message_id    UUID NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at           TIMESTAMPTZ NOT NULL,
    CONSTRAINT uq_dm_inbox_recipient_sequence
        UNIQUE (recipient_device_id, sequence_no),
    CONSTRAINT uq_dm_inbox_sender_client_message
        UNIQUE (sender_device_id, client_message_id),
    CONSTRAINT chk_dm_inbox_expiry
        CHECK (expires_at > created_at)
);

CREATE INDEX idx_dm_inbox_recipient_sequence
    ON dm_inbox(recipient_device_id, sequence_no);

CREATE INDEX idx_dm_inbox_expiry
    ON dm_inbox(expires_at);
