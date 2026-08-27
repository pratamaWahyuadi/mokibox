-- =====================================================
-- queries/users.sql
-- User-related queries. Split from queries.sql (single
-- file) on phase-1.5 to keep each table's query surface
-- in its own file for easier review and maintenance.
-- See sqlc.yaml: queries: "queries" picks up every
-- *.sql file under sqlc/queries/ in alphabetical order.
-- =====================================================

-- name: GetUserByZitadelID :one
-- Used by the auth middleware get-or-create flow.
SELECT *
FROM users
WHERE zitadel_id = $1
LIMIT 1;

-- name: CreateUser :one
-- get-or-create: on conflict do nothing. Caller re-selects
-- the row when INSERT returns nothing.
INSERT INTO users (zitadel_id, username, display_name)
VALUES ($1, $2, $3)
ON CONFLICT (zitadel_id) DO NOTHING
RETURNING *;

-- name: GetUserByID :one
SELECT *
FROM users
WHERE id = $1
LIMIT 1;

-- name: UpdateUserProfile :one
-- All fields are optional; only non-NULL params are written.
UPDATE users
SET
    display_name = COALESCE(sqlc.narg('display_name'), display_name),
    bio          = COALESCE(sqlc.narg('bio'),          bio),
    avatar_url   = COALESCE(sqlc.narg('avatar_url'),   avatar_url),
    is_private   = COALESCE(sqlc.narg('is_private'),   is_private)
WHERE id = $1
RETURNING *;

-- name: DeactivateUser :one
-- Triggered by webhook event user.deactivated.
UPDATE users
SET is_active  = FALSE,
    deleted_at = COALESCE(deleted_at, NOW())
WHERE id = $1
RETURNING *;

-- name: TombstoneUser :one
-- Triggered by user.removed. Username is replaced with a
-- deterministic placeholder so the UNIQUE constraint on
-- username keeps holding; PII is nulled out.
UPDATE users
SET is_active     = FALSE,
    deleted_at    = NOW(),
    username      = 'deleted_' || id::text,
    display_name  = NULL,
    bio           = NULL,
    avatar_url    = NULL
WHERE id = $1
RETURNING *;

-- name: GetUserProfileWithStats :one
-- Profile + follower/following counts + is_following for
-- the requesting viewer. Returns NULL for
-- is_following when the viewer is the same as the
-- profile owner.
SELECT
    u.*,
    (SELECT COUNT(*) FROM follows f WHERE f.followee_id = u.id) AS follower_count,
    (SELECT COUNT(*) FROM follows f WHERE f.follower_id = u.id) AS following_count,
    EXISTS (
        SELECT 1
        FROM follows f
        WHERE f.follower_id = $2 AND f.followee_id = u.id
    ) AS is_following
FROM users u
WHERE u.id = $1
LIMIT 1;
