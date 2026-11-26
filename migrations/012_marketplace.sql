-- Marketplace v1. Forward-only schema.

CREATE TABLE marketplace_categories (
    slug        TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    sort_order  SMALLINT NOT NULL UNIQUE CHECK (sort_order >= 0)
);

INSERT INTO marketplace_categories (slug, name, sort_order) VALUES
    ('electronics', 'Electronics', 0),
    ('vehicles', 'Vehicles', 1),
    ('home_living', 'Home & Living', 2),
    ('fashion', 'Fashion', 3),
    ('sports_hobbies', 'Sports & Hobbies', 4),
    ('baby_kids', 'Baby & Kids', 5),
    ('books_media', 'Books & Media', 6),
    ('other', 'Other', 7);

CREATE TABLE marketplace_listings (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    seller_user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    category_slug   TEXT NOT NULL REFERENCES marketplace_categories(slug),
    title           TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 120),
    description     TEXT NOT NULL CHECK (char_length(description) BETWEEN 1 AND 5000),
    price_idr       BIGINT NOT NULL CHECK (price_idr > 0),
    city            TEXT NOT NULL CHECK (char_length(city) BETWEEN 1 AND 100),
    item_condition  TEXT NOT NULL CHECK (item_condition IN ('new', 'like_new', 'good', 'fair')),
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'sold', 'archived')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_marketplace_listings_browse
    ON marketplace_listings (created_at DESC, id DESC)
    WHERE status = 'active';
CREATE INDEX idx_marketplace_listings_category_browse
    ON marketplace_listings (category_slug, created_at DESC, id DESC)
    WHERE status = 'active';
CREATE INDEX idx_marketplace_listings_city_browse
    ON marketplace_listings (lower(city), created_at DESC, id DESC)
    WHERE status = 'active';
CREATE INDEX idx_marketplace_listings_price_browse
    ON marketplace_listings (price_idr, created_at DESC, id DESC)
    WHERE status = 'active';
CREATE INDEX idx_marketplace_listings_seller
    ON marketplace_listings (seller_user_id, created_at DESC, id DESC);

CREATE TABLE marketplace_listing_images (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id      UUID NOT NULL REFERENCES marketplace_listings(id) ON DELETE CASCADE,
    r2_key          TEXT NOT NULL UNIQUE,
    content_type    TEXT NOT NULL CHECK (content_type IN ('image/jpeg', 'image/png', 'image/webp')),
    sort_order      SMALLINT NOT NULL CHECK (sort_order BETWEEN 0 AND 7),
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'active')),
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_marketplace_image_expiry
        CHECK ((status = 'pending' AND expires_at IS NOT NULL) OR
               (status = 'active' AND expires_at IS NULL)),
    UNIQUE (listing_id, sort_order)
);

CREATE INDEX idx_marketplace_images_listing_active
    ON marketplace_listing_images (listing_id, sort_order)
    WHERE status = 'active';
CREATE INDEX idx_marketplace_images_pending_expiry
    ON marketplace_listing_images (expires_at)
    WHERE status = 'pending';

CREATE TABLE marketplace_favorites (
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    listing_id  UUID NOT NULL REFERENCES marketplace_listings(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, listing_id)
);

CREATE INDEX idx_marketplace_favorites_user_created
    ON marketplace_favorites (user_id, created_at DESC, listing_id DESC);
CREATE INDEX idx_marketplace_favorites_listing
    ON marketplace_favorites (listing_id);

CREATE TABLE marketplace_reports (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    listing_id          UUID NOT NULL REFERENCES marketplace_listings(id) ON DELETE CASCADE,
    reporter_user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reason              TEXT NOT NULL CHECK (reason IN ('scam', 'prohibited', 'misleading', 'other')),
    details             TEXT NOT NULL DEFAULT '' CHECK (char_length(details) <= 1000),
    status              TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'reviewed', 'dismissed', 'resolved')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (listing_id, reporter_user_id)
);

CREATE INDEX idx_marketplace_reports_status_created
    ON marketplace_reports (status, created_at);
CREATE INDEX idx_marketplace_reports_listing
    ON marketplace_reports (listing_id);

CREATE TABLE marketplace_contacts (
    listing_id      UUID NOT NULL REFERENCES marketplace_listings(id) ON DELETE CASCADE,
    buyer_user_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    seller_user_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ,
    PRIMARY KEY (listing_id, buyer_user_id),
    CONSTRAINT chk_marketplace_contact_no_self
        CHECK (buyer_user_id <> seller_user_id),
    CONSTRAINT chk_marketplace_contact_expiry
        CHECK (expires_at > created_at)
);

CREATE INDEX idx_marketplace_contacts_buyer_seller
    ON marketplace_contacts (buyer_user_id, seller_user_id);
CREATE INDEX idx_marketplace_contacts_seller_buyer
    ON marketplace_contacts (seller_user_id, buyer_user_id);
CREATE INDEX idx_marketplace_contacts_expiry
    ON marketplace_contacts (expires_at)
    WHERE revoked_at IS NULL;

GRANT SELECT ON marketplace_categories TO tiktok_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON
    marketplace_listings,
    marketplace_listing_images,
    marketplace_favorites,
    marketplace_reports,
    marketplace_contacts
TO tiktok_api;
