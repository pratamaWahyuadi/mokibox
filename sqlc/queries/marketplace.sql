-- Marketplace v1 queries.

-- name: ListMarketplaceCategories :many
SELECT slug, name, sort_order
FROM marketplace_categories
ORDER BY sort_order;

-- name: CreateMarketplaceListing :one
INSERT INTO marketplace_listings (
    seller_user_id, category_slug, title, description, price_idr, city, item_condition,
    latitude, longitude
)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9
FROM users seller
WHERE seller.id = $1 AND seller.is_active = TRUE
RETURNING id, seller_user_id, category_slug, title, description, price_idr, city,
          item_condition, status, created_at, updated_at, latitude, longitude, expires_at;

-- name: LockMarketplaceQuota :exec
SELECT pg_advisory_xact_lock(hashtext(sqlc.arg(lock_key)));

-- name: CountMarketplaceListingsBySellerSince :one
SELECT COUNT(*)
FROM marketplace_listings
WHERE seller_user_id = $1 AND created_at >= $2;

-- name: UpdateMarketplaceListing :one
UPDATE marketplace_listings
SET category_slug = $3,
    title = $4,
    description = $5,
    price_idr = $6,
    city = $7,
    item_condition = $8,
    latitude = $9,
    longitude = $10,
    updated_at = NOW()
WHERE marketplace_listings.id = $1 AND marketplace_listings.seller_user_id = $2 AND marketplace_listings.status <> 'archived'
  AND EXISTS (SELECT 1 FROM users seller WHERE seller.id = $2 AND seller.is_active = TRUE)
RETURNING id, seller_user_id, category_slug, title, description, price_idr, city,
          item_condition, status, created_at, updated_at, latitude, longitude, expires_at;

-- name: UpdateMarketplaceListingStatus :one
UPDATE marketplace_listings
SET status = $3, updated_at = NOW()
WHERE marketplace_listings.id = $1 AND marketplace_listings.seller_user_id = $2
  AND EXISTS (SELECT 1 FROM users seller WHERE seller.id = $2 AND seller.is_active = TRUE)
RETURNING id, seller_user_id, category_slug, title, description, price_idr, city,
          item_condition, status, created_at, updated_at, latitude, longitude, expires_at;

-- name: GetMarketplaceListingForUpdate :one
SELECT l.id, l.seller_user_id, l.status
FROM marketplace_listings l
JOIN users seller ON seller.id = l.seller_user_id AND seller.is_active = TRUE
WHERE l.id = $1
FOR UPDATE OF l;

-- name: GetMarketplaceListing :one
SELECT l.id, l.seller_user_id, u.username, u.display_name,
       l.category_slug, c.name AS category_name, l.title, l.description,
       l.price_idr, l.city, l.item_condition, l.latitude, l.longitude,
       l.status, l.created_at, l.updated_at, l.expires_at,
       EXISTS (
           SELECT 1 FROM marketplace_favorites f
           WHERE f.listing_id = l.id AND f.user_id = $2
       ) AS is_favorite
FROM marketplace_listings l
JOIN users u ON u.id = l.seller_user_id AND u.is_active = TRUE
JOIN marketplace_categories c ON c.slug = l.category_slug
WHERE l.id = $1
  AND (l.status IN ('active', 'sold') OR l.seller_user_id = $2)
  AND (l.expires_at > NOW() OR l.seller_user_id = $2 OR l.status <> 'active');

-- name: ListMarketplaceListings :many
SELECT l.id, l.seller_user_id, u.username, u.display_name,
       l.category_slug, c.name AS category_name, l.title, l.description,
       l.price_idr, l.city, l.item_condition, l.latitude, l.longitude,
       l.status, l.created_at, l.updated_at, l.expires_at,
       CASE WHEN sqlc.narg(radius_km)::double precision IS NULL THEN NULL
            ELSE 6371.0088 * 2 * asin(sqrt(LEAST(1.0,
                power(sin(radians(l.latitude - sqlc.narg(latitude)::double precision) / 2), 2) +
                cos(radians(sqlc.narg(latitude)::double precision)) * cos(radians(l.latitude)) *
                power(sin(radians(l.longitude - sqlc.narg(longitude)::double precision) / 2), 2)
            )))
       END AS distance_km,
       EXISTS (
           SELECT 1 FROM marketplace_favorites f
           WHERE f.listing_id = l.id AND f.user_id = sqlc.arg(viewer_id)
       ) AS is_favorite
FROM marketplace_listings l
JOIN users u ON u.id = l.seller_user_id AND u.is_active = TRUE
JOIN marketplace_categories c ON c.slug = l.category_slug
WHERE l.status = 'active'
  AND l.expires_at > NOW()
  AND (sqlc.narg(category_slug)::text IS NULL OR l.category_slug = sqlc.narg(category_slug))
  AND (sqlc.narg(city)::text IS NULL OR lower(l.city) = lower(sqlc.narg(city)))
  AND (sqlc.narg(query_text)::text IS NULL
       OR strpos(lower(l.title), lower(sqlc.narg(query_text))) > 0
       OR strpos(lower(l.description), lower(sqlc.narg(query_text))) > 0)
  AND (sqlc.narg(min_price)::bigint IS NULL OR l.price_idr >= sqlc.narg(min_price))
  AND (sqlc.narg(max_price)::bigint IS NULL OR l.price_idr <= sqlc.narg(max_price))
  AND (sqlc.narg(radius_km)::double precision IS NULL OR (
      l.latitude IS NOT NULL
      AND l.longitude IS NOT NULL
      AND l.latitude BETWEEN sqlc.narg(latitude)::double precision - sqlc.narg(radius_km)::double precision / 111.32
                        AND sqlc.narg(latitude)::double precision + sqlc.narg(radius_km)::double precision / 111.32
      AND l.longitude BETWEEN sqlc.narg(longitude)::double precision
                               - sqlc.narg(radius_km)::double precision / (111.32 * GREATEST(cos(radians(sqlc.narg(latitude)::double precision)), 0.01))
                          AND sqlc.narg(longitude)::double precision
                               + sqlc.narg(radius_km)::double precision / (111.32 * GREATEST(cos(radians(sqlc.narg(latitude)::double precision)), 0.01))
      AND 6371.0088 * 2 * asin(sqrt(LEAST(1.0,
          power(sin(radians(l.latitude - sqlc.narg(latitude)::double precision) / 2), 2) +
          cos(radians(sqlc.narg(latitude)::double precision)) * cos(radians(l.latitude)) *
          power(sin(radians(l.longitude - sqlc.narg(longitude)::double precision) / 2), 2)
      ))) <= sqlc.narg(radius_km)::double precision
  ))
  AND (sqlc.narg(cursor_created_at)::timestamptz IS NULL
       OR (l.created_at, l.id) <
          (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY l.created_at DESC, l.id DESC
LIMIT sqlc.arg(page_size);

-- name: ListMarketplaceOwnListings :many
SELECT l.id, l.seller_user_id, u.username, u.display_name,
       l.category_slug, c.name AS category_name, l.title, l.description,
       l.price_idr, l.city, l.item_condition, l.latitude, l.longitude,
       l.status, l.created_at, l.updated_at, l.expires_at,
       FALSE AS is_favorite
FROM marketplace_listings l
JOIN users u ON u.id = l.seller_user_id AND u.is_active = TRUE
JOIN marketplace_categories c ON c.slug = l.category_slug
WHERE l.seller_user_id = sqlc.arg(seller_user_id)
  AND (sqlc.narg(status)::text IS NULL OR l.status = sqlc.narg(status))
  AND (sqlc.narg(cursor_created_at)::timestamptz IS NULL
       OR (l.created_at, l.id) <
          (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY l.created_at DESC, l.id DESC
LIMIT sqlc.arg(page_size);

-- name: ListMarketplaceListingsBySeller :many
SELECT l.id, l.seller_user_id, u.username, u.display_name,
       l.category_slug, c.name AS category_name, l.title, l.description,
       l.price_idr, l.city, l.item_condition, l.latitude, l.longitude,
       l.status, l.created_at, l.updated_at, l.expires_at,
       NULL::double precision AS distance_km,
       EXISTS (
           SELECT 1 FROM marketplace_favorites f
           WHERE f.listing_id = l.id AND f.user_id = sqlc.arg(viewer_id)
       ) AS is_favorite
FROM marketplace_listings l
JOIN users u ON u.id = l.seller_user_id AND u.is_active = TRUE
JOIN marketplace_categories c ON c.slug = l.category_slug
WHERE l.seller_user_id = sqlc.arg(seller_user_id)
  AND l.status = 'active' AND l.expires_at > NOW()
  AND (sqlc.narg(cursor_created_at)::timestamptz IS NULL
       OR (l.created_at, l.id) <
          (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY l.created_at DESC, l.id DESC
LIMIT sqlc.arg(page_size);

-- name: ListMarketplaceListingImages :many
SELECT id, listing_id, r2_key, content_type, sort_order, status, expires_at, created_at, size_bytes, is_cover
FROM marketplace_listing_images
WHERE listing_id = $1 AND status = 'active'
ORDER BY sort_order;

-- name: ListMarketplaceImagesForListings :many
SELECT id, listing_id, r2_key, content_type, sort_order, status, expires_at, created_at, size_bytes, is_cover
FROM marketplace_listing_images
WHERE listing_id = ANY(string_to_array(sqlc.arg(listing_ids)::text, ',')::uuid[])
  AND status = 'active'
ORDER BY listing_id, sort_order;

-- name: DeleteExpiredMarketplaceListingImages :many
DELETE FROM marketplace_listing_images
WHERE listing_id = $1 AND status = 'pending' AND expires_at <= NOW()
RETURNING id, listing_id, r2_key, content_type, sort_order, status, expires_at, created_at, size_bytes, is_cover;

-- name: GetNextMarketplaceListingImagePosition :one
SELECT positions.position::smallint
FROM generate_series(0, 7) AS positions(position)
WHERE NOT EXISTS (
    SELECT 1 FROM marketplace_listing_images i
    WHERE i.listing_id = sqlc.arg(listing_id)
      AND i.sort_order = positions.position
)
ORDER BY positions.position
LIMIT 1;

-- name: CreateMarketplaceListingImage :one
INSERT INTO marketplace_listing_images (listing_id, r2_key, content_type, size_bytes, sort_order, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, listing_id, r2_key, content_type, sort_order, status, expires_at, created_at, size_bytes, is_cover;

-- name: GetMarketplaceListingImageForOwner :one
SELECT i.id, i.listing_id, i.r2_key, i.content_type, i.sort_order, i.status,
       i.expires_at, i.created_at, i.size_bytes, i.is_cover
FROM marketplace_listing_images i
JOIN marketplace_listings l ON l.id = i.listing_id
WHERE i.id = $1 AND l.id = $2 AND l.seller_user_id = $3;

-- name: ConfirmMarketplaceListingImage :execrows
UPDATE marketplace_listing_images i
SET status = 'active', expires_at = NULL, r2_key = $4
FROM marketplace_listings l
WHERE i.id = $1 AND i.listing_id = l.id AND l.id = $2
  AND l.seller_user_id = $3 AND l.status <> 'archived'
  AND EXISTS (SELECT 1 FROM users seller WHERE seller.id = $3 AND seller.is_active = TRUE)
  AND i.status = 'pending' AND i.expires_at > NOW() AND i.size_bytes IS NOT NULL;

-- name: DeleteMarketplaceListingImage :one
DELETE FROM marketplace_listing_images i
USING marketplace_listings l
WHERE i.id = $1 AND i.listing_id = l.id
  AND l.id = $2 AND l.seller_user_id = $3
RETURNING i.r2_key;

-- name: ClearMarketplaceListingCover :exec
UPDATE marketplace_listing_images
SET is_cover = FALSE
WHERE listing_id = $1 AND is_cover = TRUE;

-- name: SetMarketplaceListingImageCover :one
UPDATE marketplace_listing_images i
SET is_cover = TRUE
FROM marketplace_listings l
WHERE i.id = $1 AND i.listing_id = l.id AND l.id = $2
  AND l.seller_user_id = $3 AND l.status <> 'archived'
  AND i.status = 'active'
RETURNING i.id, i.listing_id, i.r2_key, i.content_type, i.sort_order,
          i.status, i.expires_at, i.created_at, i.size_bytes, i.is_cover;

-- name: ListMarketplaceImageKeysBySeller :many
SELECT i.r2_key
FROM marketplace_listing_images i
JOIN marketplace_listings l ON l.id = i.listing_id
WHERE l.seller_user_id = $1;

-- name: IsMarketplaceFavoriteEligible :one
SELECT EXISTS (
    SELECT 1
    FROM marketplace_listings l
    JOIN users seller ON seller.id = l.seller_user_id AND seller.is_active = TRUE
    JOIN users buyer ON buyer.id = sqlc.arg(user_id) AND buyer.is_active = TRUE
    WHERE l.id = sqlc.arg(listing_id)
      AND l.status = 'active'
      AND l.expires_at > NOW()
      AND l.seller_user_id <> sqlc.arg(user_id)
);

-- name: InsertMarketplaceFavorite :execrows
INSERT INTO marketplace_favorites (user_id, listing_id)
SELECT sqlc.arg(user_id), l.id
FROM marketplace_listings l
JOIN users seller ON seller.id = l.seller_user_id AND seller.is_active = TRUE
JOIN users buyer ON buyer.id = sqlc.arg(user_id) AND buyer.is_active = TRUE
WHERE l.id = sqlc.arg(listing_id)
  AND l.status = 'active'
  AND l.expires_at > NOW()
  AND l.seller_user_id <> sqlc.arg(user_id)
ON CONFLICT (user_id, listing_id) DO NOTHING;

-- name: DeleteMarketplaceFavorite :exec
DELETE FROM marketplace_favorites
WHERE user_id = $1 AND listing_id = $2;

-- name: ListMarketplaceFavorites :many
SELECT l.id, l.seller_user_id, u.username, u.display_name,
       l.category_slug, c.name AS category_name, l.title, l.description,
       l.price_idr, l.city, l.item_condition, l.latitude, l.longitude,
       l.status, l.created_at, l.updated_at, l.expires_at,
       TRUE AS is_favorite
FROM marketplace_favorites f
JOIN marketplace_listings l ON l.id = f.listing_id AND l.status = 'active' AND l.expires_at > NOW()
JOIN users u ON u.id = l.seller_user_id AND u.is_active = TRUE
JOIN marketplace_categories c ON c.slug = l.category_slug
WHERE f.user_id = sqlc.arg(user_id)
  AND (sqlc.narg(cursor_created_at)::timestamptz IS NULL
       OR (l.created_at, l.id) <
          (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY l.created_at DESC, l.id DESC
LIMIT sqlc.arg(page_size);

-- name: IsMarketplaceReportEligible :one
SELECT EXISTS (
    SELECT 1
    FROM marketplace_listings l
    JOIN users seller ON seller.id = l.seller_user_id AND seller.is_active = TRUE
    JOIN users reporter ON reporter.id = sqlc.arg(user_id) AND reporter.is_active = TRUE
    WHERE l.id = sqlc.arg(listing_id)
      AND l.status = 'active'
      AND l.expires_at > NOW()
      AND l.seller_user_id <> sqlc.arg(user_id)
);

-- name: InsertMarketplaceReport :execrows
INSERT INTO marketplace_reports (listing_id, reporter_user_id, reason, details)
SELECT l.id, sqlc.arg(user_id), sqlc.arg(reason), sqlc.arg(details)
FROM marketplace_listings l
JOIN users seller ON seller.id = l.seller_user_id AND seller.is_active = TRUE
JOIN users reporter ON reporter.id = sqlc.arg(user_id) AND reporter.is_active = TRUE
WHERE l.id = sqlc.arg(listing_id)
  AND l.status = 'active'
  AND l.expires_at > NOW()
  AND l.seller_user_id <> sqlc.arg(user_id)
ON CONFLICT (listing_id, reporter_user_id) DO NOTHING;

-- name: CountMarketplaceReportsByReporterSince :one
SELECT COUNT(*)
FROM marketplace_reports
WHERE reporter_user_id = $1 AND created_at >= $2;

-- name: HasMarketplaceReport :one
SELECT EXISTS (
    SELECT 1 FROM marketplace_reports
    WHERE listing_id = $1 AND reporter_user_id = $2
);

-- name: ListMarketplaceReports :many
SELECT r.id, r.listing_id, r.reporter_user_id, r.reason, r.details,
       r.status, r.created_at, r.reviewed_at, r.reviewer_user_id,
       r.review_note, r.resolution,
       l.title AS listing_title, reporter.username AS reporter_username
FROM marketplace_reports r
JOIN marketplace_listings l ON l.id = r.listing_id
JOIN users reporter ON reporter.id = r.reporter_user_id
WHERE (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status))
  AND (sqlc.narg(cursor_created_at)::timestamptz IS NULL
       OR (r.created_at, r.id) <
          (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY r.created_at DESC, r.id DESC
LIMIT sqlc.arg(page_size);

-- name: ReviewMarketplaceReport :one
UPDATE marketplace_reports r
SET status = sqlc.arg(status)::text, reviewer_user_id = sqlc.arg(reviewer_user_id)::uuid,
    review_note = sqlc.arg(review_note)::text,
    resolution = sqlc.arg(resolution)::text, reviewed_at = NOW()
WHERE r.id = sqlc.arg(id)::uuid AND r.status = 'open'
  AND EXISTS (SELECT 1 FROM users reviewer WHERE reviewer.id = sqlc.arg(reviewer_user_id)::uuid AND reviewer.is_active = TRUE)
RETURNING r.id, r.listing_id, r.reporter_user_id, r.reason, r.details,
          r.status, r.created_at, r.reviewed_at, r.reviewer_user_id,
          r.review_note, r.resolution;

-- name: GetMarketplaceContactSeller :one
SELECT l.seller_user_id
FROM marketplace_listings l
JOIN users buyer ON buyer.id = $2 AND buyer.is_active = TRUE
JOIN users seller ON seller.id = l.seller_user_id AND seller.is_active = TRUE
WHERE l.id = $1 AND l.status = 'active' AND l.expires_at > NOW() AND l.seller_user_id <> $2
  AND NOT EXISTS (
      SELECT 1 FROM marketplace_contacts c
      WHERE c.listing_id = l.id AND c.buyer_user_id = $2 AND c.revoked_at IS NOT NULL
  )
FOR UPDATE OF l;

-- name: InsertMarketplaceContact :execrows
INSERT INTO marketplace_contacts (listing_id, buyer_user_id, seller_user_id, expires_at)
SELECT l.id, $2, $3, NOW() + INTERVAL '30 days'
FROM marketplace_listings l
JOIN users buyer ON buyer.id = $2 AND buyer.is_active = TRUE
JOIN users seller ON seller.id = $3 AND seller.is_active = TRUE
WHERE l.id = $1 AND l.status = 'active' AND l.expires_at > NOW() AND l.seller_user_id = $3
ON CONFLICT (listing_id, buyer_user_id) DO NOTHING;

-- name: RenewExpiredMarketplaceContact :execrows
UPDATE marketplace_contacts
SET expires_at = NOW() + INTERVAL '30 days'
WHERE listing_id = sqlc.arg(listing_id)
  AND buyer_user_id = sqlc.arg(buyer_user_id)
  AND revoked_at IS NULL
  AND expires_at <= NOW();

-- name: CountMarketplaceContactsByBuyerSince :one
SELECT COUNT(*)
FROM marketplace_contacts
WHERE buyer_user_id = $1 AND created_at >= $2;

-- name: GetMarketplaceContactParticipants :one
SELECT buyer_user_id, seller_user_id
FROM marketplace_contacts
WHERE listing_id = $1 AND buyer_user_id = $2
  AND revoked_at IS NULL;

-- name: RevokeMarketplaceContact :execrows
UPDATE marketplace_contacts
SET revoked_at = NOW()
WHERE listing_id = $1 AND buyer_user_id = $2 AND seller_user_id = $3
  AND revoked_at IS NULL;

-- name: DeleteMarketplaceContactsByUser :exec
DELETE FROM marketplace_contacts
WHERE buyer_user_id = $1 OR seller_user_id = $1;

-- name: DeleteMarketplaceReportsByUser :exec
DELETE FROM marketplace_reports
WHERE reporter_user_id = $1;

-- name: DeleteMarketplaceFavoritesByUser :exec
DELETE FROM marketplace_favorites
WHERE user_id = $1;

-- name: DeleteMarketplaceListingsBySeller :exec
DELETE FROM marketplace_listings
WHERE seller_user_id = $1;

-- name: InsertMarketplaceCleanupOutbox :exec
INSERT INTO marketplace_cleanup_outbox (r2_keys)
VALUES (sqlc.arg(r2_keys)::text[]);

-- name: SweepExpiredMarketplaceListingImages :many
WITH expired AS (
    SELECT i.id
    FROM marketplace_listing_images i
    WHERE i.status = 'pending' AND i.expires_at <= NOW()
    ORDER BY i.expires_at, i.id
    LIMIT sqlc.arg(page_size)
    FOR UPDATE SKIP LOCKED
)
DELETE FROM marketplace_listing_images i
USING expired
WHERE i.id = expired.id
RETURNING i.id, i.listing_id, i.r2_key, i.content_type, i.sort_order,
          i.status, i.expires_at, i.created_at, i.size_bytes, i.is_cover;

-- name: ListExpiredMarketplaceListingsForUpdate :many
SELECT l.id
FROM marketplace_listings l
WHERE l.status = 'active' AND l.expires_at <= NOW()
ORDER BY l.expires_at, l.id
LIMIT sqlc.arg(page_size)
FOR UPDATE OF l SKIP LOCKED;

-- name: ArchiveExpiredMarketplaceListings :execrows
UPDATE marketplace_listings
SET status = 'archived', updated_at = NOW()
WHERE id = ANY(sqlc.arg(ids)::uuid[])
  AND status = 'active' AND expires_at <= NOW();

-- name: ListMarketplaceImageKeysByListings :many
SELECT r2_key
FROM marketplace_listing_images
WHERE listing_id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListDueMarketplaceCleanupOutbox :many
SELECT id, r2_keys, attempts
FROM marketplace_cleanup_outbox
WHERE status = 'pending' AND next_attempt_at <= NOW()
ORDER BY id
LIMIT sqlc.arg(page_size)
FOR UPDATE SKIP LOCKED;

-- name: MarkMarketplaceCleanupOutboxDone :exec
UPDATE marketplace_cleanup_outbox
SET status = 'done', updated_at = NOW()
WHERE id = sqlc.arg(id) AND status = 'pending';

-- name: FailMarketplaceCleanupOutbox :exec
UPDATE marketplace_cleanup_outbox
SET status = CASE
        WHEN attempts + 1 >= sqlc.arg(max_attempts) THEN 'failed'
        ELSE 'pending'
    END,
    attempts = attempts + 1,
    next_attempt_at = sqlc.arg(next_attempt_at),
    last_error = sqlc.arg(last_error),
    updated_at = NOW()
WHERE id = sqlc.arg(id) AND status = 'pending';

-- name: PruneMarketplaceCleanupOutbox :execrows
DELETE FROM marketplace_cleanup_outbox
WHERE status = 'done'
  AND updated_at < NOW() - (sqlc.arg(retention_seconds)::bigint || ' seconds')::interval;
