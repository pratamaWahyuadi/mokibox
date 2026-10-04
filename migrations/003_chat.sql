-- =====================================================
-- 003_chat.sql
-- Migration for WhatsApp-style Chat system (Direct & Group Chat)
-- =====================================================

CREATE TABLE IF NOT EXISTS conversations (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type       TEXT NOT NULL DEFAULT 'direct',
    name       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_conversations_type CHECK (type IN ('direct', 'group'))
);

CREATE TABLE IF NOT EXISTS conversation_members (
    conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role            TEXT NOT NULL DEFAULT 'member',
    joined_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_read_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (conversation_id, user_id),
    CONSTRAINT chk_conversation_members_role CHECK (role IN ('member', 'admin'))
);

CREATE INDEX IF NOT EXISTS idx_conversation_members_user_updated
    ON conversation_members(user_id, last_read_at DESC);

CREATE TABLE IF NOT EXISTS messages (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    sender_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    message_type    TEXT NOT NULL DEFAULT 'text',
    content         TEXT,
    media_url       TEXT,
    media_metadata  JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ,
    CONSTRAINT chk_messages_type CHECK (message_type IN ('text', 'photo', 'video', 'sticker', 'gif'))
);

CREATE INDEX IF NOT EXISTS idx_messages_conversation_created
    ON messages(conversation_id, created_at DESC);

-- Privileges for API role
GRANT SELECT, INSERT, UPDATE, DELETE ON conversations TO tiktok_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON conversation_members TO tiktok_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON messages TO tiktok_api;
