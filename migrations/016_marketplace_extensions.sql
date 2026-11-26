-- Marketplace v1 extensions: listing expiry, image metadata/cover,
-- and report moderation audit fields. Forward-only; apply after 015.

ALTER TABLE marketplace_listings
    ADD COLUMN expires_at TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '30 days';

CREATE INDEX idx_marketplace_listings_expiry
    ON marketplace_listings (expires_at)
    WHERE status = 'active';

ALTER TABLE marketplace_listing_images
    ADD COLUMN size_bytes BIGINT,
    ADD COLUMN is_cover BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE marketplace_listing_images
    ADD CONSTRAINT chk_marketplace_image_size
    CHECK (size_bytes IS NULL OR (size_bytes > 0 AND size_bytes <= 10485760));

CREATE UNIQUE INDEX idx_marketplace_image_cover
    ON marketplace_listing_images (listing_id)
    WHERE is_cover;

ALTER TABLE marketplace_reports
    ADD COLUMN reviewed_at TIMESTAMPTZ,
    ADD COLUMN reviewer_user_id UUID REFERENCES users(id),
    ADD COLUMN review_note TEXT NOT NULL DEFAULT '',
    ADD COLUMN resolution TEXT;

ALTER TABLE marketplace_reports
    ADD CONSTRAINT chk_marketplace_report_review
    CHECK ((status = 'open' AND reviewed_at IS NULL AND reviewer_user_id IS NULL AND resolution IS NULL)
        OR (status <> 'open' AND reviewed_at IS NOT NULL AND reviewer_user_id IS NOT NULL
            AND resolution IN ('dismissed', 'resolved')));

ALTER TABLE marketplace_reports
    ADD CONSTRAINT chk_marketplace_report_note CHECK (char_length(review_note) <= 1000);
