-- Direct messages are text-only, one-to-one conversations.
-- Migrations 003-007 are reserved by the parity roadmap and 008
-- is used by the separate profile-avatar feature.
CREATE TABLE dm_conversations (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    participant_low_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    participant_high_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_recipient_id  UUID REFERENCES users(id) ON DELETE CASCADE,
    request_status        TEXT NOT NULL DEFAULT 'accepted',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_dm_conversations_participants
        UNIQUE (participant_low_id, participant_high_id),
    CONSTRAINT chk_dm_conversations_order
        CHECK (participant_low_id < participant_high_id),
    CONSTRAINT chk_dm_conversations_request_status
        CHECK (request_status IN ('pending', 'accepted', 'declined')),
    CONSTRAINT chk_dm_conversations_request_recipient
        CHECK (request_status = 'accepted' OR request_recipient_id IS NOT NULL),
    CONSTRAINT chk_dm_conversations_request_participant
        CHECK (
            request_recipient_id IS NULL OR
            request_recipient_id IN (participant_low_id, participant_high_id)
        )
);

CREATE INDEX idx_dm_conversations_low_updated
    ON dm_conversations(participant_low_id, updated_at DESC, id DESC);

CREATE INDEX idx_dm_conversations_high_updated
    ON dm_conversations(participant_high_id, updated_at DESC, id DESC);

CREATE INDEX idx_dm_conversations_pending_recipient
    ON dm_conversations(request_recipient_id, updated_at DESC, id DESC)
    WHERE request_status = 'pending';

CREATE TABLE dm_messages (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES dm_conversations(id) ON DELETE CASCADE,
    sender_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    content         TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    read_at         TIMESTAMPTZ,
    CONSTRAINT chk_dm_messages_content
        CHECK (char_length(btrim(content)) BETWEEN 1 AND 2000)
);

CREATE INDEX idx_dm_messages_conversation_created
    ON dm_messages(conversation_id, created_at DESC, id DESC);

CREATE INDEX idx_dm_messages_unread
    ON dm_messages(conversation_id, sender_id, created_at DESC)
    WHERE read_at IS NULL;
