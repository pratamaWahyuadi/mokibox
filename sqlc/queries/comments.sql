-- =====================================================
-- queries/comments.sql
-- Comments + recursive subtree count. See users.sql header.
-- =====================================================

-- name: InsertComment :one
INSERT INTO comments (video_id, user_id, parent_id, content)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetCommentByID :one
SELECT c.*, u.username, u.display_name, u.avatar_url, u.is_private
FROM comments c
JOIN users u ON u.id = c.user_id
WHERE c.id = $1
LIMIT 1;

-- name: ListCommentsByVideo :many
-- Flat list of comments for a video, cursor pagination.
-- Joins user for author info.
SELECT
    c.id,
    c.video_id,
    c.user_id,
    c.parent_id,
    c.content,
    c.created_at,
    u.username,
    u.display_name,
    u.avatar_url,
    u.is_private
FROM comments c
JOIN users u ON u.id = c.user_id
WHERE c.video_id = $1
  AND (sqlc.narg('cursor_created')::timestamptz IS NULL
       OR (c.created_at, c.id) < (sqlc.narg('cursor_created')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY c.created_at DESC, c.id DESC
LIMIT sqlc.arg('page_limit');

-- name: CountCommentSubtree :one
-- Recursive CTE counting this comment + all descendants.
-- Used before deletion to know how much to decrement
-- the video's comments_count.
WITH RECURSIVE subtree AS (
    SELECT c.id FROM comments c WHERE c.id = $1
    UNION ALL
    SELECT c.id FROM comments c
    JOIN subtree s ON c.parent_id = s.id
)
SELECT COUNT(*)::int FROM subtree;

-- name: DeleteCommentByID :exec
-- ON DELETE CASCADE handles replies automatically.
DELETE FROM comments WHERE id = $1;

-- name: DeleteCommentsByUser :exec
DELETE FROM comments WHERE user_id = $1;
