-- =====================================================
-- queries/videos.sql
-- Video-related queries. See sqlc/queries/users.sql header
-- for context on the per-table split.
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

-- name: UpdatePendingVideoMetadata :one
-- Refresh title/description on an existing
-- PENDING_UPLOAD row when upload-intent is called
-- again. The WHERE guard matches UpdatePendingVideoR2Key
-- so a concurrent state transition (e.g. the user
-- confirmed in another tab) makes the UPDATE a no-op
-- (returns sql.ErrNoRows) instead of overwriting
-- metadata on a row that has already moved to
-- PROCESSING.
UPDATE videos
SET title = $2,
    description = $3
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
--
-- Phase 6 widened the row to include the owner user
-- fields (for the nested "user" summary in VideoObject)
-- and an EXISTS (likes) per-row "liked_by_me" flag, so
-- the feed handler can build the full VideoObject
-- without an N+1 round trip. viewer_id can be NULL
-- when the feed is fetched without a session (e.g. a
-- cron job pulling the public timeline) - in that case
-- liked_by_me is FALSE for every row.
SELECT
    v.id, v.user_id, v.title, v.description, v.r2_key,
    v.hls_prefix, v.thumbnail_key, v.duration_seconds,
    v.status, v.retry_count, v.likes_count, v.views_count,
    v.comments_count, v.created_at, v.deleted_at,
    u.id            AS user_id_2,
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

-- name: DecrementCommentsCountBy :exec
-- Phase 7.B: DeleteComment removes a whole subtree
-- (comment + replies via ON DELETE CASCADE), so the
-- counter must drop by the subtree size in one atomic
-- statement. Guarded by GREATEST so concurrent drift
-- never drives the counter negative (chk_videos_counters).
UPDATE videos
SET comments_count = GREATEST(comments_count - $2, 0)
WHERE id = $1;

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
