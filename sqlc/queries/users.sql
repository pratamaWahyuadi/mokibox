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
WHERE users.id = $1
LIMIT 1;

-- name: UpdateUserProfile :one
-- All fields are optional; only non-NULL params are written.
UPDATE users
SET
    display_name              = COALESCE(sqlc.narg('display_name'),              users.display_name),
    bio                       = COALESCE(sqlc.narg('bio'),                       users.bio),
    avatar_url                = COALESCE(sqlc.narg('avatar_url'),                users.avatar_url),
    avatar_key                = CASE WHEN sqlc.narg('avatar_url') IS NOT NULL THEN NULL ELSE users.avatar_key END,
    pending_avatar_key        = CASE WHEN sqlc.narg('avatar_url') IS NOT NULL THEN NULL ELSE users.pending_avatar_key END,
    pending_avatar_expires_at = CASE WHEN sqlc.narg('avatar_url') IS NOT NULL THEN NULL ELSE users.pending_avatar_expires_at END,
    is_private                = COALESCE(sqlc.narg('is_private'),                users.is_private)
FROM (
    SELECT u.id, u.avatar_key, u.pending_avatar_key
    FROM users AS u
    WHERE u.id = $1
    FOR UPDATE
) AS previous
WHERE users.id = previous.id
RETURNING users.id,
          users.zitadel_id,
          users.username,
          users.username_changed_at,
          users.display_name,
          users.bio,
          users.avatar_url,
          users.is_private,
          users.is_active,
          users.deleted_at,
          users.created_at,
          users.avatar_key,
          users.pending_avatar_key,
          users.pending_avatar_expires_at,
          previous.avatar_key AS previous_avatar_key,
          previous.pending_avatar_key AS previous_pending_avatar_key;

-- name: SetPendingAvatarUpload :one
-- Replaces any earlier, unconfirmed avatar upload for the user.
UPDATE users
SET pending_avatar_key = $2,
    pending_avatar_expires_at = $3
FROM (
    SELECT u.id, u.pending_avatar_key
    FROM users AS u
    WHERE u.id = $1 AND u.is_active = TRUE
    FOR UPDATE
) AS previous
WHERE users.id = previous.id
RETURNING users.pending_avatar_key,
          users.pending_avatar_expires_at,
          previous.pending_avatar_key AS previous_pending_avatar_key;

-- name: ConfirmAvatarUpload :one
-- Promotes only the currently pending, unexpired key. The old key is
-- returned so the caller can enqueue storage cleanup after the update.
WITH previous AS (
    SELECT users.avatar_key
    FROM users
    WHERE users.id = $1
      AND users.is_active = TRUE
      AND users.pending_avatar_key = $2
      AND users.pending_avatar_expires_at > NOW()
    FOR UPDATE
),
updated AS (
    UPDATE users
    SET avatar_url = NULL,
        avatar_key = $2,
        pending_avatar_key = NULL,
        pending_avatar_expires_at = NULL
    WHERE users.id = $1
      AND EXISTS (SELECT 1 FROM previous)
    RETURNING id, avatar_key
)
SELECT updated.id,
       updated.avatar_key,
       previous.avatar_key AS previous_avatar_key
FROM updated
CROSS JOIN previous;

-- name: ClearPendingAvatarUpload :execrows
UPDATE users
SET pending_avatar_key = NULL,
    pending_avatar_expires_at = NULL
WHERE id = $1 AND pending_avatar_key = $2;

-- name: GetUserAvatarKeys :one
SELECT avatar_key, pending_avatar_key
FROM users
WHERE id = $1
FOR UPDATE;

-- name: DeactivateUser :one
-- Triggered by webhook event user.deactivated.
UPDATE users
SET is_active  = FALSE,
    deleted_at = COALESCE(deleted_at, NOW())
WHERE id = $1
RETURNING *;

-- name: UpdateUsername :one
-- Fitur B: penggantian username sendiri. SATU statement
-- atomik tahan race: kondisi cooldown dievaluasi oleh
-- Postgres dengan now() database (BUKAN jam aplikasi),
-- jadi tidak ada jendela TOCTOU. RowsAffected == 0
-- (sql.ErrNoRows dengan :one) berarti user tidak ada /
-- tidak aktif / masih cooldown — handler membedakannya
-- via GetUsernameChangedAt SETELAH update nihil
-- (read-only, keputusan tulis tetap di UPDATE atomik ini).
-- Pelanggaran unique lower(username) (SQLSTATE 23505)
-- dipetakan handler menjadi 409 USERNAME_TAKEN.
UPDATE users
SET username = sqlc.arg('new_username'), username_changed_at = now()
WHERE id = sqlc.arg('user_id')
  AND is_active = TRUE
  AND (username_changed_at IS NULL OR username_changed_at <= now() - interval '7 days')
RETURNING id, username, username_changed_at;

-- name: GetUsernameChangedAt :one
-- Dipakai SETELAH UpdateUsername nihil (0 rows) untuk
-- membedakan "user tidak ada/tidak aktif" (404) dari
-- "masih cooldown" (429). next_change_at dan
-- retry_after_seconds dihitung dari now() DATABASE, bukan
-- jam aplikasi — dipakai untuk details next_change_at dan
-- header Retry-After (K6).
SELECT username_changed_at,
       (username_changed_at + interval '7 days')::timestamptz AS next_change_at,
       GREATEST(EXTRACT(EPOCH FROM (username_changed_at + interval '7 days' - now()))::int, 0)::bigint AS retry_after_seconds
FROM users
WHERE id = $1;

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
    avatar_url    = NULL,
    avatar_key    = NULL,
    pending_avatar_key = NULL,
    pending_avatar_expires_at = NULL
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

-- name: ListUsersEligibleForReconcile :many
-- Issue #44 (phase-10): candidates for the R2 orphan
-- reconciliation sweeper. A user is eligible when:
--   - tombstoned (is_active = FALSE) AND deleted_at set
--     (chk_users_deleted_at guarantees the pair), AND
--   - the 24h delete-account grace has elapsed, so the
--     post-commit cleanup:objects enqueue had every
--     chance to run; anything left in R2 after it is an
--     orphan from a lost enqueue / worker outage.
-- Active users are never returned - the sweeper refuses
-- to operate on them by construction, and corrupt rows
-- (is_active=FALSE but deleted_at NULL, or vice versa)
-- are excluded here and left to admin review (per issue
-- #44 Out of Scope).
SELECT id
FROM users
WHERE is_active = FALSE
  AND deleted_at IS NOT NULL
  AND deleted_at < NOW() - INTERVAL '24 hours'
ORDER BY deleted_at ASC
LIMIT $1;
