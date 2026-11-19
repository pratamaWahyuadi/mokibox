ALTER TABLE users
    ADD COLUMN avatar_key TEXT,
    ADD COLUMN pending_avatar_key TEXT,
    ADD COLUMN pending_avatar_expires_at TIMESTAMPTZ,
    ADD CONSTRAINT chk_users_pending_avatar_upload
        CHECK ((pending_avatar_key IS NULL) = (pending_avatar_expires_at IS NULL));
