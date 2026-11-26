-- Marketplace location radius. Forward-only schema.
--
-- Coordinates are optional so existing city-only listings keep working. When
-- present, browse can filter listings by distance from the viewer instead of
-- only by an exact city string, like a Facebook Marketplace radius search.

ALTER TABLE marketplace_listings
    ADD COLUMN latitude DOUBLE PRECISION,
    ADD COLUMN longitude DOUBLE PRECISION;

ALTER TABLE marketplace_listings
    ADD CONSTRAINT chk_marketplace_listing_coordinates
    CHECK (
        (latitude IS NULL AND longitude IS NULL)
        OR (
            latitude IS NOT NULL
            AND longitude IS NOT NULL
            AND latitude BETWEEN -90 AND 90
            AND longitude BETWEEN -180 AND 180
        )
    );

CREATE INDEX idx_marketplace_listings_coordinates_browse
    ON marketplace_listings (latitude, longitude, created_at DESC, id DESC)
    WHERE status = 'active' AND latitude IS NOT NULL AND longitude IS NOT NULL;
