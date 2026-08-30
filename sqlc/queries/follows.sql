-- =====================================================
-- queries/follows.sql
-- Follow relations. See sqlc/queries/users.sql header.
-- =====================================================

-- name: FollowUser :exec
-- Idempotent insert. ON CONFLICT DO NOTHING makes it safe
-- to call even if the follow already exists.
INSERT INTO follows (follower_id, followee_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: GetFollow :one
-- Returns the row (including created_at) for a given
-- (follower, followee) pair, or sql.ErrNoRows when the
-- follow does not exist. Used by the follow handler to
-- return the wire `created_at` after an idempotent
-- INSERT (which is ON CONFLICT DO NOTHING and therefore
-- cannot RETURNING the original row).
SELECT follower_id, followee_id, created_at
FROM follows
WHERE follower_id = $1 AND followee_id = $2;

-- name: DeleteFollow :exec
DELETE FROM follows
WHERE follower_id = $1 AND followee_id = $2;

-- name: IsFollowing :one
SELECT EXISTS (
    SELECT 1 FROM follows
    WHERE follower_id = $1 AND followee_id = $2
);

-- name: ListFollowers :many
-- Returns followers of followee_id with user profile fields.
-- Cursor pagination: (f.created_at, f.follower_id) < ($3, $4).
SELECT
    f.follower_id AS id,
    u.username,
    u.display_name,
    u.avatar_url,
    u.is_private,
    f.created_at,
    EXISTS (
        SELECT 1 FROM follows f2
        WHERE f2.follower_id = $1 AND f2.followee_id = f.follower_id
    ) AS is_following_back
FROM follows f
JOIN users u ON u.id = f.follower_id
WHERE f.followee_id = $2
  AND (sqlc.narg('cursor_created')::timestamptz IS NULL
       OR (f.created_at, f.follower_id) < (sqlc.narg('cursor_created')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY f.created_at DESC, f.follower_id DESC
LIMIT sqlc.arg('page_limit');

-- name: ListFollowing :many
-- Returns users that follower_id is following.
SELECT
    f.followee_id AS id,
    u.username,
    u.display_name,
    u.avatar_url,
    u.is_private,
    f.created_at
FROM follows f
JOIN users u ON u.id = f.followee_id
WHERE f.follower_id = $1
  AND (sqlc.narg('cursor_created')::timestamptz IS NULL
       OR (f.created_at, f.followee_id) < (sqlc.narg('cursor_created')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY f.created_at DESC, f.followee_id DESC
LIMIT sqlc.arg('page_limit');

-- name: CountFollowers :one
SELECT COUNT(*) FROM follows WHERE followee_id = $1;

-- name: CountFollowing :one
SELECT COUNT(*) FROM follows WHERE follower_id = $1;

-- name: DeleteFollowsByFollower :exec
DELETE FROM follows WHERE follower_id = $1;

-- name: DeleteFollowsByFollowee :exec
DELETE FROM follows WHERE followee_id = $1;
