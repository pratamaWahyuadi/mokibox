-- =====================================================
-- queries.sql
-- Type-safe data access for the TikTok clone backend.
-- Consumed by sqlc. One file = one source of truth for
-- every SELECT/INSERT/UPDATE/DELETE issued by the app.
-- No raw SQL is allowed in handlers (LLD_PLAN.md).
-- =====================================================

-- =====================================================
-- users
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

-- =====================================================
-- videos
-- =====================================================

-- name: InsertVideo :one
-- Creates a new PENDING_UPLOAD row. r2_key MUST be unique.
INSERT INTO videos (user_id, r2_key, title, description)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetPendingVideoByUser :one
-- Used by upload-intent to reuse / replace an existing
-- PENDING_UPLOAD record.
SELECT *
FROM videos
WHERE user_id = $1 AND status = 'PENDING_UPLOAD'
LIMIT 1;

-- name: UpdatePendingVideoR2Key :one
-- When upload-intent is called again, rotate r2_key on
-- the existing PENDING_UPLOAD row. The partial unique
-- index makes the r2_key UNIQUE constraint safe.
UPDATE videos
SET r2_key = $2
WHERE id = $1 AND status = 'PENDING_UPLOAD'
RETURNING *;

-- name: GetVideoByID :one
SELECT *
FROM videos
WHERE id = $1
LIMIT 1;

-- name: GetVideoByIDForUpdate :one
-- Caller MUST be inside a transaction.
SELECT *
FROM videos
WHERE id = $1
FOR UPDATE;

-- name: ConfirmVideoProcessing :one
-- Atomic transition PENDING_UPLOAD -> PROCESSING. Returns
-- the updated row only when the ownership + r2_key + status
-- all match. Used by POST /api/videos/confirm.
UPDATE videos
SET status = 'PROCESSING', retry_count = 0
WHERE id = $1
  AND user_id = $2
  AND status = 'PENDING_UPLOAD'
  AND r2_key = $3
RETURNING *;

-- name: GetVideoDetail :one
-- Joins the owner user row and adds liked_by_me for the
-- requesting viewer (NULL viewer = not authenticated).
SELECT
    v.*,
    u.id            AS user_id_2,
    u.zitadel_id    AS user_zitadel_id,
    u.username      AS user_username,
    u.display_name  AS user_display_name,
    u.avatar_url    AS user_avatar_url,
    u.is_private    AS user_is_private,
    EXISTS (
        SELECT 1 FROM likes l
        WHERE l.video_id = v.id AND l.user_id = sqlc.narg('viewer_id')
    ) AS liked_by_me
FROM videos v
JOIN users u ON u.id = v.user_id
WHERE v.id = $1
LIMIT 1;

-- name: GetVideoStatusByID :one
SELECT id, user_id, status, retry_count, created_at, deleted_at
FROM videos
WHERE id = $1
LIMIT 1;

-- name: ListVideosByUser :many
-- Owner sees all rows except DELETED. Non-owner sees only
-- READY. The caller decides which branch by branching on
-- (is_owner).
SELECT *
FROM videos
WHERE user_id = $1
  AND (sqlc.arg('is_owner')::boolean = TRUE  AND status <> 'DELETED'
       OR sqlc.arg('is_owner')::boolean = FALSE AND status = 'READY')
  AND (sqlc.narg('cursor_created')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('cursor_created')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('page_limit');

-- name: ListFeedVideos :many
-- FR-FEED-01: status=READY, owner active, exclude self,
-- include public accounts + accounts the viewer follows.
-- Cursor pagination uses (created_at, id) < ($3, $4).
SELECT v.*
FROM videos v
JOIN users u ON u.id = v.user_id
WHERE v.status = 'READY'
  AND v.user_id <> $1
  AND u.is_active = TRUE
  AND (u.is_private = FALSE
       OR EXISTS (
           SELECT 1 FROM follows f
           WHERE f.follower_id = $1 AND f.followee_id = u.id
       ))
  AND (sqlc.narg('cursor_created')::timestamptz IS NULL
       OR (v.created_at, v.id) < (sqlc.narg('cursor_created')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY v.created_at DESC, v.id DESC
LIMIT sqlc.arg('page_limit');

-- name: MarkVideoDeleted :one
-- Soft delete: status -> DELETED, deleted_at -> NOW().
-- chk_videos_deleted_at forces deleted_at to be set.
UPDATE videos
SET status = 'DELETED', deleted_at = NOW()
WHERE id = $1 AND user_id = $2
RETURNING *;

-- name: MarkVideoFailed :one
UPDATE videos
SET status = 'FAILED'
WHERE id = $1
RETURNING *;

-- name: MarkVideoReady :one
-- Called by the worker after a successful transcode. The
-- WHERE clause guards against overwriting a row that has
-- been deleted while the worker was busy (FR-VIDEO-08).
UPDATE videos
SET status = 'READY',
    hls_prefix = $2,
    thumbnail_key = $3,
    duration_seconds = $4
WHERE id = $1 AND status = 'PROCESSING'
RETURNING *;

-- name: IncrementVideoRetry :one
UPDATE videos
SET retry_count = retry_count + 1
WHERE id = $1 AND status IN ('PROCESSING','PENDING_UPLOAD')
RETURNING *;

-- name: UpdateVideoToProcessing :one
-- Used by the retry path: set status back to PROCESSING
-- and reset retry_count.
UPDATE videos
SET status = 'PROCESSING', retry_count = 0
WHERE id = $1
RETURNING *;

-- name: IncrementViews :exec
-- FR-FEED-05: no per-user deduplication, just +1.
UPDATE videos
SET views_count = views_count + 1
WHERE id = $1;

-- name: IncrementLikesCount :exec
-- Called inside the same tx as INSERT INTO likes.
UPDATE videos
SET likes_count = likes_count + 1
WHERE id = $1;

-- name: DecrementLikesCount :exec
-- Called inside the same tx as DELETE FROM likes. Guarded
-- by >= 1 to keep the chk_videos_counters check happy.
UPDATE videos
SET likes_count = likes_count - 1
WHERE id = $1 AND likes_count > 0;

-- name: IncrementCommentsCount :exec
UPDATE videos
SET comments_count = comments_count + 1
WHERE id = $1;

-- name: DecrementCommentsCount :exec
UPDATE videos
SET comments_count = comments_count - 1
WHERE id = $1 AND comments_count > 0;

-- name: DecrementLikesForUser :exec
-- Called by DeleteUserData to keep the denormalized
-- counter accurate after a user is tombstoned.
UPDATE videos
SET likes_count = GREATEST(likes_count - 1, 0)
WHERE id IN (SELECT l.video_id FROM likes l WHERE l.user_id = $1);

-- name: DecrementCommentsForUser :exec
UPDATE videos
SET comments_count = GREATEST(comments_count - 1, 0)
WHERE id IN (
    SELECT c.video_id FROM comments c WHERE c.user_id = $1
);

-- name: ListVideoKeysByUser :many
-- Returns the r2 object keys that must be deleted from R2
-- when the user is tombstoned. hls_prefix is a folder, so
-- the worker uses it to list + delete every object under
-- that prefix.
SELECT id, r2_key, hls_prefix, thumbnail_key
FROM videos
WHERE user_id = $1;

-- name: DeleteVideosByUser :exec
-- Called from DeleteUserData AFTER the r2 keys are
-- enqueued for cleanup. videos.user_id has ON DELETE
-- CASCADE so this is mostly defensive.
DELETE FROM videos WHERE user_id = $1;

-- name: ListVideosEligibleForCleanup :many
-- NFR-13: 24-hour grace period after DELETED.
SELECT id, user_id, r2_key, hls_prefix, thumbnail_key
FROM videos
WHERE status = 'DELETED'
  AND deleted_at < NOW() - INTERVAL '24 hours'
ORDER BY deleted_at ASC
LIMIT $1;

-- name: DeleteVideoRow :exec
DELETE FROM videos WHERE id = $1;

-- =====================================================
-- follows
-- =====================================================

-- name: FollowUser :exec
-- Idempotent insert. ON CONFLICT DO NOTHING makes it safe
-- to call even if the follow already exists.
INSERT INTO follows (follower_id, followee_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

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

-- =====================================================
-- likes
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

-- =====================================================
-- comments
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

-- =====================================================
-- notifications
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
