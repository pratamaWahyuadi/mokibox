-- =====================================================
-- sqlc/queries/chat.sql
-- Queries for WhatsApp-style Chat (conversations, members, messages)
-- =====================================================

-- name: CreateConversation :one
INSERT INTO conversations (type, name)
VALUES ($1, $2)
RETURNING id, type, name, created_at, updated_at;

-- name: AddConversationMember :exec
INSERT INTO conversation_members (conversation_id, user_id, role)
VALUES ($1, $2, $3)
ON CONFLICT (conversation_id, user_id) DO NOTHING;

-- name: FindDirectConversation :one
SELECT c.id, c.type, c.name, c.created_at, c.updated_at
FROM conversations c
JOIN conversation_members cm1 ON cm1.conversation_id = c.id
JOIN conversation_members cm2 ON cm2.conversation_id = c.id
WHERE c.type = 'direct'
  AND cm1.user_id = $1
  AND cm2.user_id = $2
LIMIT 1;

-- name: GetConversation :one
SELECT id, type, name, created_at, updated_at
FROM conversations
WHERE id = $1;

-- name: IsConversationMember :one
SELECT EXISTS (
    SELECT 1 FROM conversation_members
    WHERE conversation_id = $1 AND user_id = $2
);

-- name: ListConversationMembers :many
SELECT cm.conversation_id, cm.user_id, cm.role, cm.joined_at, cm.last_read_at,
       u.username, u.display_name, u.avatar_url
FROM conversation_members cm
JOIN users u ON u.id = cm.user_id
WHERE cm.conversation_id = $1
ORDER BY cm.joined_at ASC;

-- name: ListUserConversations :many
SELECT c.id, c.type, c.name, c.created_at, c.updated_at,
       cm.last_read_at,
       COALESCE(m.id, '00000000-0000-0000-0000-000000000000'::uuid) AS last_message_id,
       COALESCE(m.sender_id, '00000000-0000-0000-0000-000000000000'::uuid) AS last_message_sender_id,
       COALESCE(m.message_type, '') AS last_message_type,
       COALESCE(m.content, '') AS last_message_content,
       COALESCE(m.created_at, '1970-01-01 00:00:00Z'::timestamptz) AS last_message_created_at
FROM conversations c
JOIN conversation_members cm ON cm.conversation_id = c.id
LEFT JOIN LATERAL (
    SELECT id, sender_id, message_type, content, created_at
    FROM messages
    WHERE conversation_id = c.id AND deleted_at IS NULL
    ORDER BY created_at DESC
    LIMIT 1
) m ON true
WHERE cm.user_id = $1
  AND (sqlc.narg('cursor_updated')::timestamptz IS NULL
       OR (c.updated_at, c.id) < (sqlc.narg('cursor_updated')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY c.updated_at DESC, c.id DESC
LIMIT sqlc.arg('page_limit');

-- name: InsertMessage :one
INSERT INTO messages (conversation_id, sender_id, message_type, content, media_url, media_metadata)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, conversation_id, sender_id, message_type, content, media_url, media_metadata, created_at, deleted_at;

-- name: ListMessages :many
SELECT m.id, m.conversation_id, m.sender_id, m.message_type, m.content, m.media_url, m.media_metadata, m.created_at,
       u.username AS sender_username, u.display_name AS sender_display_name, u.avatar_url AS sender_avatar_url
FROM messages m
JOIN users u ON u.id = m.sender_id
WHERE m.conversation_id = $1
  AND m.deleted_at IS NULL
  AND (sqlc.narg('cursor_created')::timestamptz IS NULL
       OR (m.created_at, m.id) < (sqlc.narg('cursor_created')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY m.created_at DESC, m.id DESC
LIMIT sqlc.arg('page_limit');

-- name: UpdateLastReadAt :exec
UPDATE conversation_members
SET last_read_at = NOW()
WHERE conversation_id = $1 AND user_id = $2;

-- name: TouchConversationUpdatedAt :exec
UPDATE conversations
SET updated_at = NOW()
WHERE id = $1;
