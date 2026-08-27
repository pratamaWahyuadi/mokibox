-- =====================================================
-- queries/likes.sql
-- Like / unlike. See sqlc/queries/users.sql header.
-- =====================================================

-- name: InsertLike :one
INSERT INTO likes (user_id, video_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: DeleteLike :one
DELETE FROM likes
WHERE user_id = $1 AND video_id = $2
RETURNING *;

-- name: IsLiked :one
SELECT EXISTS (
    SELECT 1 FROM likes
    WHERE user_id = $1 AND video_id = $2
);

-- name: DeleteLikesByUser :exec
DELETE FROM likes WHERE user_id = $1;
