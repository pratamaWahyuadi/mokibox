-- =====================================================
-- 015_dm_authorization_hardening.sql - two-way DM blocks.
-- A block denies DM eligibility in both directions and is
-- reused by Marketplace seller revocations to stop re-contact
-- through another listing. Forward-only; apply after 011.
-- =====================================================

CREATE TABLE dm_blocks (
    blocker_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    blocked_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT pk_dm_blocks PRIMARY KEY (blocker_id, blocked_id),
    CONSTRAINT chk_dm_blocks_not_self CHECK (blocker_id <> blocked_id)
);

CREATE INDEX idx_dm_blocks_blocked ON dm_blocks(blocked_id, blocker_id);
