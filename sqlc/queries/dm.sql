-- =====================================================
-- queries/dm.sql
-- Device-bound Olm ciphertext relay.
-- =====================================================

-- name: GetDMDeviceByUser :one
SELECT device_id, user_id, identity_curve25519, identity_ed25519,
       next_inbox_sequence, created_at
FROM dm_devices
WHERE user_id = $1;

-- name: GetDMDeviceByUserAndID :one
SELECT device_id, user_id, identity_curve25519, identity_ed25519,
       next_inbox_sequence, created_at
FROM dm_devices
WHERE user_id = $1 AND device_id = $2;

-- name: GetDMDeviceForUpdate :one
SELECT device_id, user_id, identity_curve25519, identity_ed25519,
       next_inbox_sequence, created_at
FROM dm_devices
WHERE user_id = $1 AND device_id = $2
FOR UPDATE;

-- name: LockActiveDMUser :one
SELECT id
FROM users
WHERE id = $1 AND is_active = TRUE
FOR UPDATE;

-- name: DeleteDMDevicesByUser :execrows
DELETE FROM dm_devices
WHERE user_id = $1;

-- name: CreateDMDevice :one
INSERT INTO dm_devices (
    device_id, user_id, identity_curve25519, identity_ed25519
)
VALUES ($1, $2, $3, $4)
RETURNING device_id, user_id, identity_curve25519, identity_ed25519,
          next_inbox_sequence, created_at;

-- name: IsDMMutualFollow :one
SELECT EXISTS (
    SELECT 1
    FROM users sender
    JOIN users recipient ON recipient.id = $2
    WHERE sender.id = $1
      AND sender.is_active = TRUE
      AND recipient.is_active = TRUE
      AND EXISTS (
          SELECT 1 FROM follows
          WHERE follower_id = sender.id AND followee_id = recipient.id
      )
      AND EXISTS (
          SELECT 1 FROM follows
          WHERE follower_id = recipient.id AND followee_id = sender.id
      )
);

-- name: GetDMTargetDevice :one
SELECT d.device_id, d.user_id, d.identity_curve25519, d.identity_ed25519,
       d.next_inbox_sequence, d.created_at
FROM dm_devices d
JOIN users u ON u.id = d.user_id
WHERE d.user_id = $1 AND u.is_active = TRUE;

-- name: InsertDMOneTimeKey :execrows
INSERT INTO dm_one_time_keys (device_id, key_id, public_key, signature)
VALUES ($1, $2, $3, $4)
ON CONFLICT (device_id, key_id) DO NOTHING;

-- name: ClaimDMOneTimeKey :one
WITH candidate AS (
    SELECT k.id
    FROM dm_one_time_keys k
    WHERE k.device_id = $1 AND k.claimed_at IS NULL
    ORDER BY k.created_at, k.id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE dm_one_time_keys k
SET claimed_at = NOW()
FROM candidate c
WHERE k.id = c.id
RETURNING k.key_id, k.public_key, k.signature;

-- name: UpsertDMFallbackKey :one
INSERT INTO dm_fallback_keys (device_id, key_id, public_key, signature)
VALUES ($1, $2, $3, $4)
ON CONFLICT (device_id) DO UPDATE
SET key_id = EXCLUDED.key_id,
    public_key = EXCLUDED.public_key,
    signature = EXCLUDED.signature,
    updated_at = NOW()
RETURNING device_id, key_id, public_key, signature, created_at, updated_at;

-- name: GetDMFallbackKey :one
SELECT device_id, key_id, public_key, signature, created_at, updated_at
FROM dm_fallback_keys
WHERE device_id = $1;

-- name: GetDMIdempotency :one
SELECT request_digest, message_id, response_sequence, expires_at
FROM dm_idempotency
WHERE sender_device_id = $1 AND client_message_id = $2;

-- name: CreateDMIdempotency :one
INSERT INTO dm_idempotency (
    sender_device_id, client_message_id, request_digest, message_id, expires_at
)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (sender_device_id, client_message_id) DO UPDATE
SET request_digest = EXCLUDED.request_digest,
    message_id = EXCLUDED.message_id,
    response_sequence = NULL,
    created_at = NOW(),
    expires_at = EXCLUDED.expires_at
WHERE dm_idempotency.expires_at <= NOW()
RETURNING sender_device_id, client_message_id, request_digest, message_id,
          response_sequence, expires_at;

-- name: SetDMIdempotencySequence :execrows
UPDATE dm_idempotency
SET response_sequence = $3
WHERE sender_device_id = $1 AND client_message_id = $2;

-- name: CountPendingDMInbox :one
SELECT COUNT(*)
FROM dm_inbox
WHERE recipient_device_id = $1
  AND acknowledged_at IS NULL
  AND expires_at > NOW();

-- name: AllocateDMSequence :one
UPDATE dm_devices
SET next_inbox_sequence = next_inbox_sequence + 1
WHERE device_id = $1
RETURNING (next_inbox_sequence - 1)::BIGINT AS sequence;

-- name: CreateDMInboxMessage :one
INSERT INTO dm_inbox (
    message_id, sender_user_id, sender_device_id, recipient_user_id,
    recipient_device_id, sequence, client_message_id, protocol,
    protocol_version, message_type, ciphertext, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING message_id, recipient_device_id, sequence, expires_at;

-- name: ListPendingDMInbox :many
SELECT message_id, sender_user_id, sender_device_id, recipient_user_id,
       recipient_device_id, sequence, protocol, protocol_version,
       message_type, ciphertext, created_at, expires_at
FROM dm_inbox
WHERE recipient_device_id = $1
  AND sequence > $2
  AND acknowledged_at IS NULL
  AND expires_at > NOW()
ORDER BY sequence
LIMIT $3;

-- name: AcknowledgeDMInboxMessage :execrows
UPDATE dm_inbox
SET ciphertext = NULL, acknowledged_at = NOW()
WHERE message_id = $1
  AND recipient_device_id = $2
  AND acknowledged_at IS NULL;

-- name: GetDMInboxMessageForDevice :one
SELECT message_id
FROM dm_inbox
WHERE message_id = $1 AND recipient_device_id = $2;

-- name: DeleteExpiredDMInbox :execrows
DELETE FROM dm_inbox
WHERE expires_at <= NOW();

-- name: DeleteExpiredDMIdempotency :execrows
DELETE FROM dm_idempotency
WHERE expires_at <= NOW();
