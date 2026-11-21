-- =====================================================
-- Direct-message device directory and opaque inbox relay.
-- =====================================================

-- name: GetDMDeviceByUser :one
SELECT id, user_id, protocol, protocol_version, key_bundle, fallback_key_id,
       fallback_key_material, next_sequence, registered_at
FROM dm_devices
WHERE user_id = $1;

-- name: GetDMDeviceForUpdate :one
SELECT id, user_id, protocol, protocol_version, key_bundle, fallback_key_id,
       fallback_key_material, next_sequence, registered_at
FROM dm_devices
WHERE user_id = $1
FOR UPDATE;

-- name: GetDMDeviceByUserForKeyShare :one
SELECT id, user_id, protocol, protocol_version, key_bundle, fallback_key_id,
       fallback_key_material, next_sequence, registered_at
FROM dm_devices
WHERE user_id = $1
FOR KEY SHARE;

-- name: CreateDMDevice :one
INSERT INTO dm_devices
    (id, user_id, protocol, protocol_version, key_bundle, fallback_key_id,
     fallback_key_material)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, user_id, protocol, protocol_version, key_bundle, fallback_key_id,
          fallback_key_material, next_sequence, registered_at;

-- name: InsertDMOneTimeKey :execrows
INSERT INTO dm_one_time_keys (device_id, key_id, key_material)
VALUES ($1, $2, $3)
ON CONFLICT (device_id, key_id) DO NOTHING;

-- name: CountPendingDMInboxForRecipient :one
SELECT COUNT(*)
FROM dm_inbox
WHERE recipient_device_id = $1
  AND expires_at > NOW();

-- name: CountDMOneTimeKeys :one
SELECT COUNT(*)
FROM dm_one_time_keys
WHERE device_id = $1;

-- name: GetEligibleDMDeviceForBundle :one
SELECT d.id, d.user_id, d.protocol, d.protocol_version, d.key_bundle,
       d.fallback_key_id, d.fallback_key_material, d.next_sequence, d.registered_at
FROM dm_devices d
JOIN users recipient ON recipient.id = d.user_id AND recipient.is_active
JOIN follows f_recipient_to_sender
  ON f_recipient_to_sender.follower_id = d.user_id
 AND f_recipient_to_sender.followee_id = $1
JOIN follows f_sender_to_recipient
  ON f_sender_to_recipient.follower_id = $1
 AND f_sender_to_recipient.followee_id = d.user_id
WHERE d.user_id = $2
FOR KEY SHARE OF d, f_recipient_to_sender, f_sender_to_recipient;

-- name: ClaimDMOneTimeKey :one
DELETE FROM dm_one_time_keys AS key_row
WHERE (key_row.device_id, key_row.key_id) = (
    SELECT candidate.device_id, candidate.key_id
    FROM dm_one_time_keys AS candidate
    WHERE candidate.device_id = $1
    ORDER BY candidate.created_at, candidate.key_id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING device_id, key_id, key_material;

-- name: GetEligibleDMRecipientDeviceForSend :one
SELECT recipient_device.id, recipient_device.protocol,
       recipient_device.protocol_version
FROM dm_devices sender_device
JOIN dm_devices recipient_device ON recipient_device.user_id = $2
JOIN users sender ON sender.id = sender_device.user_id AND sender.is_active
JOIN users recipient ON recipient.id = recipient_device.user_id AND recipient.is_active
JOIN follows sender_follows_recipient
  ON sender_follows_recipient.follower_id = sender.id
 AND sender_follows_recipient.followee_id = recipient.id
JOIN follows recipient_follows_sender
  ON recipient_follows_sender.follower_id = recipient.id
 AND recipient_follows_sender.followee_id = sender.id
WHERE sender_device.id = $1
FOR KEY SHARE OF sender_device, recipient_device,
                 sender_follows_recipient, recipient_follows_sender;

-- name: IncrementDMDeviceSequence :one
UPDATE dm_devices
SET next_sequence = next_sequence + 1
WHERE id = $1
RETURNING next_sequence;

-- name: GetDMSendIdempotencyByClientID :one
SELECT recipient_user_id, recipient_device_id, protocol, version,
       ciphertext_digest, message_id, sequence_no, created_at, expires_at
FROM dm_send_idempotency
WHERE sender_device_id = $1 AND client_message_id = $2;

-- name: InsertDMSendIdempotency :one
INSERT INTO dm_send_idempotency
    (sender_device_id, sender_user_id, client_message_id, recipient_user_id,
     recipient_device_id, protocol, version, ciphertext_digest, message_id,
     sequence_no, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
        NOW() + (sqlc.arg(expires_in_seconds)::double precision * INTERVAL '1 second'))
ON CONFLICT (sender_device_id, client_message_id) DO NOTHING
RETURNING message_id, sequence_no, created_at, expires_at;

-- name: InsertDMInboxMessage :one
INSERT INTO dm_inbox
    (id, recipient_device_id, recipient_user_id, sequence_no, sender_user_id,
     sender_device_id, protocol, version, ciphertext, client_message_id, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (sender_device_id, client_message_id) DO NOTHING
RETURNING id, sequence_no, client_message_id, created_at, expires_at;

-- name: ListDMInboxAfter :many
SELECT id, sequence_no, sender_user_id, sender_device_id, protocol, version,
       ciphertext, client_message_id, created_at, expires_at
FROM dm_inbox
WHERE recipient_device_id = $1
  AND sequence_no > $2
  AND expires_at > NOW()
ORDER BY sequence_no ASC
LIMIT $3;

-- name: AckDMInboxMessage :execrows
DELETE FROM dm_inbox
WHERE id = $1 AND recipient_device_id = $2;

-- name: DeleteDMInboxByUser :exec
DELETE FROM dm_inbox
WHERE sender_user_id = $1
   OR recipient_device_id IN (
       SELECT id FROM dm_devices WHERE user_id = $1
   );

-- name: DeleteDMDevicesByUser :exec
DELETE FROM dm_devices
WHERE user_id = $1;

-- name: DeleteDMSendIdempotencyByUser :exec
DELETE FROM dm_send_idempotency
WHERE sender_user_id = $1 OR recipient_user_id = $1;

-- name: DeleteExpiredDMInbox :execrows
WITH expired AS (
    SELECT id
    FROM dm_inbox
    WHERE expires_at <= NOW()
    ORDER BY expires_at
    LIMIT 1000
    FOR UPDATE SKIP LOCKED
)
DELETE FROM dm_inbox inbox
USING expired
WHERE inbox.id = expired.id;

-- name: DeleteExpiredDMSendIdempotency :execrows
WITH expired AS (
    SELECT sender_device_id, client_message_id
    FROM dm_send_idempotency
    WHERE expires_at <= NOW()
    ORDER BY expires_at
    LIMIT 1000
    FOR UPDATE SKIP LOCKED
)
DELETE FROM dm_send_idempotency entry
USING expired
WHERE entry.sender_device_id = expired.sender_device_id
  AND entry.client_message_id = expired.client_message_id;
