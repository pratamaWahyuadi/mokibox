-- =====================================================
-- queries/notifications.sql
-- In-app notifications. See sqlc/queries/users.sql header.
-- =====================================================

-- name: InsertNotification :one
INSERT INTO notifications (user_id, actor_id, type, payload)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListNotifications :many
-- All notifications for a user, newest first, cursor pagination.
SELECT *
FROM notifications
WHERE user_id = $1
  AND (sqlc.narg('cursor_created')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('cursor_created')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_limit');

-- name: MarkAllNotificationsRead :execrows
UPDATE notifications
SET is_read = TRUE
WHERE user_id = $1 AND is_read = FALSE;

-- name: DeleteNotificationsForUser :exec
DELETE FROM notifications WHERE user_id = $1;

-- name: DeleteNotificationsByActor :exec
DELETE FROM notifications WHERE actor_id = $1;
