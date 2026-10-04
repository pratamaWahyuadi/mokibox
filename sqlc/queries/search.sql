-- =====================================================
-- queries/search.sql
-- Search queries for users and videos.
-- See planning/ROADMAP_TIKTOK_PARITY.md §3 and Issue #57.
-- =====================================================

-- name: SearchUsers :many
-- Searches active users by username or display_name (case-insensitive).
-- Excludes tombstoned users and the requesting viewer itself.
SELECT
    u.id,
    u.username,
    u.display_name,
    u.avatar_url,
    u.is_private
FROM users u
WHERE u.is_active = TRUE
  AND u.id <> sqlc.arg('viewer_id')
  AND (u.username ILIKE '%' || sqlc.arg('query')::text || '%' OR u.display_name ILIKE '%' || sqlc.arg('query')::text || '%')
ORDER BY
    (LOWER(u.username) = LOWER(sqlc.arg('query')::text)) DESC,
    length(u.username) ASC,
    COALESCE((SELECT count(*) FROM follows f WHERE f.followee_id = u.id), 0) DESC,
    u.username ASC
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

-- name: SearchVideos :many
-- Searches READY videos by title or description (case-insensitive).
-- Enforces active owner user, deleted_at NULL, and private user visibility rule (public, owner self, or followed).
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
        WHERE l.video_id = v.id AND l.user_id = sqlc.arg('viewer_id')
    ) AS liked_by_me
FROM videos v
JOIN users u ON u.id = v.user_id
WHERE v.status = 'READY'
  AND v.deleted_at IS NULL
  AND u.is_active = TRUE
  AND (v.title ILIKE '%' || sqlc.arg('query')::text || '%' OR v.description ILIKE '%' || sqlc.arg('query')::text || '%')
  AND (u.is_private = FALSE
       OR u.id = sqlc.arg('viewer_id')
       OR EXISTS (
           SELECT 1 FROM follows f
           WHERE f.follower_id = sqlc.arg('viewer_id') AND f.followee_id = u.id
       ))
ORDER BY v.views_count DESC, v.created_at DESC, v.id DESC
LIMIT sqlc.arg('page_limit') OFFSET sqlc.arg('page_offset');

