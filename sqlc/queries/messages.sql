-- =====================================================
-- Direct messages. A conversation is a canonical user pair.
-- =====================================================

-- name: GetOrCreateDMConversation :one
INSERT INTO dm_conversations (
    participant_low_id,
    participant_high_id,
    request_recipient_id,
    request_status
)
SELECT
    LEAST(sender.id, recipient.id),
    GREATEST(sender.id, recipient.id),
    CASE WHEN EXISTS (
        SELECT 1
        FROM follows
        WHERE follower_id = sqlc.arg(recipient_id)::uuid
          AND followee_id = sqlc.arg(sender_id)::uuid
    ) THEN NULL ELSE sqlc.arg(recipient_id)::uuid END,
    CASE WHEN EXISTS (
        SELECT 1
        FROM follows
        WHERE follower_id = sqlc.arg(recipient_id)::uuid
          AND followee_id = sqlc.arg(sender_id)::uuid
    ) THEN 'accepted' ELSE 'pending' END
FROM users AS sender
JOIN users AS recipient
  ON recipient.id = sqlc.arg(recipient_id)::uuid
 AND recipient.is_active = TRUE
WHERE sender.id = sqlc.arg(sender_id)::uuid
  AND sender.is_active = TRUE
ON CONFLICT (participant_low_id, participant_high_id) DO UPDATE
SET request_status = CASE
    WHEN dm_conversations.request_status = 'declined'
     AND dm_conversations.request_recipient_id = sqlc.arg(sender_id)::uuid
    THEN 'accepted'
    ELSE dm_conversations.request_status
END
RETURNING id, participant_low_id, participant_high_id, request_recipient_id,
          request_status, created_at, updated_at;

-- name: SendDMMessage :one
WITH eligible AS (
    SELECT id
    FROM dm_conversations
    WHERE id = sqlc.arg(conversation_id)::uuid
      AND (participant_low_id = sqlc.arg(sender_id)::uuid
           OR participant_high_id = sqlc.arg(sender_id)::uuid)
      AND request_status <> 'declined'
      AND NOT (
          request_status = 'pending'
          AND request_recipient_id = sqlc.arg(sender_id)::uuid
      )
      AND EXISTS (
          SELECT 1
          FROM users AS sender
          WHERE sender.id = sqlc.arg(sender_id)::uuid
            AND sender.is_active = TRUE
      )
      AND EXISTS (
          SELECT 1
          FROM users AS recipient
          WHERE recipient.id = CASE
              WHEN participant_low_id = sqlc.arg(sender_id)::uuid
              THEN participant_high_id
              ELSE participant_low_id
          END
            AND recipient.is_active = TRUE
      )
),
inserted AS (
    INSERT INTO dm_messages (conversation_id, sender_id, content)
    SELECT id, sqlc.arg(sender_id)::uuid, sqlc.arg(content)::text
    FROM eligible
    RETURNING *
),
touched AS (
    UPDATE dm_conversations AS c
    SET updated_at = GREATEST(c.updated_at, inserted.created_at)
    FROM inserted
    WHERE c.id = inserted.conversation_id
    RETURNING c.id, c.request_status
)
SELECT inserted.*, touched.request_status
FROM inserted
JOIN touched ON touched.id = inserted.conversation_id;

-- name: GetDMConversationForUser :one
SELECT c.id, c.participant_low_id, c.participant_high_id,
       c.request_recipient_id, c.request_status, c.created_at, c.updated_at
FROM dm_conversations AS c
WHERE c.id = sqlc.arg(conversation_id)::uuid
  AND (c.participant_low_id = sqlc.arg(user_id)::uuid
       OR c.participant_high_id = sqlc.arg(user_id)::uuid)
  AND EXISTS (
      SELECT 1
      FROM users AS other
      WHERE other.id = CASE
          WHEN c.participant_low_id = sqlc.arg(user_id)::uuid
          THEN c.participant_high_id
          ELSE c.participant_low_id
      END
        AND other.is_active = TRUE
  )
  AND NOT (
      c.request_status = 'declined'
      AND c.request_recipient_id = sqlc.arg(user_id)::uuid
  );

-- name: ListDMConversations :many
SELECT
    c.id AS conversation_id,
    other.id AS other_user_id,
    other.username AS other_username,
    other.display_name AS other_display_name,
    other.avatar_url AS other_avatar_url,
    c.request_status,
    latest.content AS last_message,
    latest.sender_id AS last_sender_id,
    latest.created_at AS last_message_at,
    c.updated_at AS conversation_updated_at,
    unread.count AS unread_count
FROM dm_conversations AS c
JOIN users AS other
  ON other.id = CASE
      WHEN c.participant_low_id = sqlc.arg(user_id)::uuid
      THEN c.participant_high_id
      ELSE c.participant_low_id
  END
 AND other.is_active = TRUE
JOIN LATERAL (
    SELECT m.content, m.sender_id, m.created_at
    FROM dm_messages AS m
    WHERE m.conversation_id = c.id
    ORDER BY m.created_at DESC, m.id DESC
    LIMIT 1
) AS latest ON TRUE
LEFT JOIN LATERAL (
    SELECT COUNT(*)::bigint AS count
    FROM dm_messages AS m
    WHERE m.conversation_id = c.id
      AND m.sender_id <> sqlc.arg(user_id)::uuid
      AND m.read_at IS NULL
) AS unread ON TRUE
WHERE (c.participant_low_id = sqlc.arg(user_id)::uuid
       OR c.participant_high_id = sqlc.arg(user_id)::uuid)
  AND (
      (sqlc.arg(folder)::text = 'inbox' AND c.request_status = 'accepted')
      OR (sqlc.arg(folder)::text = 'requests'
          AND c.request_status = 'pending'
          AND c.request_recipient_id = sqlc.arg(user_id)::uuid)
      OR (sqlc.arg(folder)::text = 'sent'
          AND c.request_status = 'pending'
          AND c.request_recipient_id <> sqlc.arg(user_id)::uuid)
  )
  AND (sqlc.narg(cursor_created)::timestamptz IS NULL
       OR (c.updated_at, c.id) <
          (sqlc.narg(cursor_created)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY c.updated_at DESC, c.id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListDMMessages :many
SELECT m.*
FROM dm_messages AS m
JOIN dm_conversations AS c ON c.id = m.conversation_id
WHERE c.id = sqlc.arg(conversation_id)::uuid
  AND (c.participant_low_id = sqlc.arg(user_id)::uuid
       OR c.participant_high_id = sqlc.arg(user_id)::uuid)
  AND NOT (
      c.request_status = 'declined'
      AND c.request_recipient_id = sqlc.arg(user_id)::uuid
  )
  AND (sqlc.narg(cursor_created)::timestamptz IS NULL
       OR (m.created_at, m.id) <
          (sqlc.narg(cursor_created)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY m.created_at DESC, m.id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListDMMessagesAfter :many
SELECT m.*
FROM dm_messages AS m
JOIN dm_conversations AS c ON c.id = m.conversation_id
WHERE c.id = sqlc.arg(conversation_id)::uuid
  AND (c.participant_low_id = sqlc.arg(user_id)::uuid
       OR c.participant_high_id = sqlc.arg(user_id)::uuid)
  AND NOT (
      c.request_status = 'declined'
      AND c.request_recipient_id = sqlc.arg(user_id)::uuid
  )
  AND (m.created_at, m.id) >
      (sqlc.arg(after_created)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY m.created_at ASC, m.id ASC
LIMIT sqlc.arg(page_limit);

-- name: MarkDMMessagesRead :execrows
UPDATE dm_messages AS m
SET read_at = NOW()
FROM dm_conversations AS c
WHERE c.id = m.conversation_id
  AND c.id = sqlc.arg(conversation_id)::uuid
  AND (c.participant_low_id = sqlc.arg(user_id)::uuid
       OR c.participant_high_id = sqlc.arg(user_id)::uuid)
  AND NOT (
      c.request_status = 'declined'
      AND c.request_recipient_id = sqlc.arg(user_id)::uuid
  )
  AND c.request_status = 'accepted'
  AND m.sender_id <> sqlc.arg(user_id)::uuid
  AND m.read_at IS NULL;

-- name: AcceptDMRequest :one
WITH changed AS (
    UPDATE dm_conversations AS c
    SET request_status = 'accepted',
        updated_at = NOW()
    WHERE c.id = sqlc.arg(conversation_id)::uuid
      AND c.request_recipient_id = sqlc.arg(user_id)::uuid
      AND c.request_status = 'pending'
      AND EXISTS (
          SELECT 1
          FROM users AS other
          WHERE other.id = CASE
              WHEN c.participant_low_id = sqlc.arg(user_id)::uuid
              THEN c.participant_high_id
              ELSE c.participant_low_id
          END
            AND other.is_active = TRUE
      )
    RETURNING id, request_status
)
SELECT id, request_status FROM changed
UNION ALL
SELECT id, request_status
FROM dm_conversations AS c
WHERE c.id = sqlc.arg(conversation_id)::uuid
  AND c.request_recipient_id = sqlc.arg(user_id)::uuid
  AND c.request_status = 'accepted'
  AND EXISTS (
      SELECT 1
      FROM users AS other
      WHERE other.id = CASE
          WHEN c.participant_low_id = sqlc.arg(user_id)::uuid
          THEN c.participant_high_id
          ELSE c.participant_low_id
      END
        AND other.is_active = TRUE
  )
LIMIT 1;

-- name: DeclineDMRequest :one
WITH changed AS (
    UPDATE dm_conversations AS c
    SET request_status = 'declined',
        updated_at = NOW()
    WHERE c.id = sqlc.arg(conversation_id)::uuid
      AND c.request_recipient_id = sqlc.arg(user_id)::uuid
      AND c.request_status = 'pending'
      AND EXISTS (
          SELECT 1
          FROM users AS other
          WHERE other.id = CASE
              WHEN c.participant_low_id = sqlc.arg(user_id)::uuid
              THEN c.participant_high_id
              ELSE c.participant_low_id
          END
            AND other.is_active = TRUE
      )
    RETURNING id, request_status
)
SELECT id, request_status FROM changed
UNION ALL
SELECT id, request_status
FROM dm_conversations AS c
WHERE c.id = sqlc.arg(conversation_id)::uuid
  AND c.request_recipient_id = sqlc.arg(user_id)::uuid
  AND c.request_status = 'declined'
  AND EXISTS (
      SELECT 1
      FROM users AS other
      WHERE other.id = CASE
          WHEN c.participant_low_id = sqlc.arg(user_id)::uuid
          THEN c.participant_high_id
          ELSE c.participant_low_id
      END
        AND other.is_active = TRUE
  )
LIMIT 1;

-- name: DeleteDMConversationsForUser :exec
DELETE FROM dm_conversations
WHERE participant_low_id = $1 OR participant_high_id = $1;
