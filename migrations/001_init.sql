-- =====================================================
-- 001_init.sql
-- Forward-only initial schema for the TikTok clone backend.
-- Source of truth: planning/03_schema.md
--
-- This file is the canonical DDL for the entire app. After
-- this migration is merged, ALL schema changes must be
-- applied as a NEW migration (002_*.sql). Do not edit this
-- file in place.
-- =====================================================

-- Required for gen_random_uuid().
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- -------------------------------------------------
-- users
-- -------------------------------------------------
CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    zitadel_id    TEXT NOT NULL UNIQUE,
    username      TEXT NOT NULL UNIQUE,
    display_name  TEXT,
    bio           TEXT,
    avatar_url    TEXT,
    is_private    BOOLEAN NOT NULL DEFAULT FALSE,
    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    deleted_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_users_deleted_at
        CHECK (deleted_at IS NULL OR is_active = FALSE)
);

-- -------------------------------------------------
-- videos
-- -------------------------------------------------
CREATE TABLE videos (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title            TEXT,
    description      TEXT,
    r2_key           TEXT NOT NULL UNIQUE,
    hls_prefix       TEXT,
    thumbnail_key    TEXT,
    duration_seconds INTEGER,
    status           TEXT NOT NULL DEFAULT 'PENDING_UPLOAD',
    retry_count      INTEGER NOT NULL DEFAULT 0,
    likes_count      INTEGER NOT NULL DEFAULT 0,
    views_count      INTEGER NOT NULL DEFAULT 0,
    comments_count   INTEGER NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at       TIMESTAMPTZ,
    CONSTRAINT chk_videos_status
        CHECK (status IN ('PENDING_UPLOAD','PROCESSING','READY','FAILED','DELETED')),
    CONSTRAINT chk_videos_retry
        CHECK (retry_count BETWEEN 0 AND 3),
    CONSTRAINT chk_videos_counters
        CHECK (likes_count >= 0 AND views_count >= 0 AND comments_count >= 0),
    CONSTRAINT chk_videos_duration
        CHECK (duration_seconds IS NULL OR (duration_seconds BETWEEN 1 AND 180)),
    CONSTRAINT chk_videos_deleted_at
        CHECK (
            (status = 'DELETED' AND deleted_at IS NOT NULL) OR
            (status <> 'DELETED' AND deleted_at IS NULL)
        )
);

CREATE INDEX idx_videos_user_id
    ON videos(user_id);

CREATE INDEX idx_videos_feed
    ON videos(status, created_at DESC)
    WHERE status = 'READY';

CREATE INDEX idx_videos_deleted_cleanup
    ON videos(status, deleted_at)
    WHERE status = 'DELETED';

CREATE UNIQUE INDEX uq_videos_one_pending_upload_per_user
    ON videos(user_id)
    WHERE status = 'PENDING_UPLOAD';

-- -------------------------------------------------
-- follows
-- -------------------------------------------------
CREATE TABLE follows (
    follower_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    followee_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (follower_id, followee_id),
    CONSTRAINT chk_follows_no_self
        CHECK (follower_id <> followee_id)
);

CREATE INDEX idx_follows_follower_created
    ON follows(follower_id, created_at DESC);

CREATE INDEX idx_follows_followee_created
    ON follows(followee_id, created_at DESC);

-- -------------------------------------------------
-- likes
-- -------------------------------------------------
CREATE TABLE likes (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    video_id   UUID NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, video_id)
);

CREATE INDEX idx_likes_video_created
    ON likes(video_id, created_at DESC);

-- -------------------------------------------------
-- comments
-- -------------------------------------------------
CREATE TABLE comments (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    video_id   UUID NOT NULL REFERENCES videos(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id  UUID,
    content    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_comments_content
        CHECK (char_length(content) BETWEEN 1 AND 1000),
    CONSTRAINT uq_comments_id_video
        UNIQUE (id, video_id),
    CONSTRAINT fk_comments_parent
        FOREIGN KEY (parent_id, video_id)
        REFERENCES comments(id, video_id)
        ON DELETE CASCADE
);

CREATE INDEX idx_comments_video_created
    ON comments(video_id, created_at DESC);

CREATE INDEX idx_comments_parent_created
    ON comments(parent_id, created_at DESC)
    WHERE parent_id IS NOT NULL;

CREATE INDEX idx_comments_user_id
    ON comments(user_id, created_at DESC);

-- -------------------------------------------------
-- notifications
-- -------------------------------------------------
CREATE TABLE notifications (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    actor_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type       TEXT NOT NULL,
    payload    JSONB NOT NULL DEFAULT '{}'::jsonb,
    is_read    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_notifications_type
        CHECK (type IN ('follow','like','comment'))
);

CREATE INDEX idx_notifications_user_read_created
    ON notifications(user_id, is_read, created_at DESC);

CREATE INDEX idx_notifications_actor
    ON notifications(actor_id);

-- =====================================================
-- Privileges (SEC-02). Roles tiktok_api and tiktok_worker
-- are created either by deploy/postgres/init/01_roles.sql
-- (first boot) or by scripts/bootstrap_db.sh (subsequent
-- boots). Default privileges on FUTURE tables are also
-- granted so subsequent migrations (002_*.sql) pick up
-- sane perms without a manual step.
-- =====================================================

-- API gateway: full CRUD on every app table.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO tiktok_api;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO tiktok_api;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO tiktok_api;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO tiktok_api;

-- Transcoder worker: read + status transitions on videos
-- only. No INSERT, no DELETE on videos (cleanup happens
-- via a separate API-gateway call); no access to any other
-- table. UPDATE is required for status / retry_count /
-- duration / hls_prefix / thumbnail_key.
GRANT SELECT, UPDATE, DELETE ON videos TO tiktok_worker;
