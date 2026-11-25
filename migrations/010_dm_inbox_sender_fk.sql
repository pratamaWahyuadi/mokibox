-- =====================================================
-- 010_dm_inbox_sender_fk.sql - preserve sent DM history.
-- A sender re-registering its device must not delete
-- messages it already delivered. Reference the sender
-- user instead of the short-lived sender device.
-- Forward-only; apply after 009_dm_e2ee.sql.
-- =====================================================

ALTER TABLE dm_inbox
    DROP CONSTRAINT IF EXISTS fk_dm_inbox_sender_device;

ALTER TABLE dm_inbox
    ADD CONSTRAINT fk_dm_inbox_sender_user
        FOREIGN KEY (sender_user_id)
        REFERENCES users(id) ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS idx_dm_inbox_sender_user_id
    ON dm_inbox(sender_user_id);
