# Marketplace v1 — backend scope

**Status:** Approved local implementation scope. Backend API only, gated by
`MARKETPLACE_ENABLED`; no UI, checkout, payments, shipping, or production
rollout in this task.

## Product behavior

- Users can create and manage listings for physical goods. Only active users
  can list, contact the seller, save, or report. Marketplace listings are public even
  when the seller profile is private; only active listings appear in browse
  results. A listing detail remains visible as sold, while archived listings
  are visible only to their seller.
- Listing fields: title (1–120 chars), description (1–5,000 chars), category,
  positive integer `price_idr`, city (1–100 chars), condition (`new`,
  `like_new`, `good`, `fair`), status (`active`, `sold`, `archived`), and
  optional `latitude`/`longitude`. Currency is IDR; no negotiation, order,
  checkout, payment, or shipping workflow is represented. Coordinates model
  Facebook Marketplace-style proximity: buyers contact sellers and agree on
  payment/meetup outside the backend.
- Categories are a fixed seeded set: `electronics`, `vehicles`, `home_living`,
  `fashion`, `sports_hobbies`, `baby_kids`, `books_media`, and `other`.
  Category records are read-only through the public API.
- Browse searches title/description and filters by category, case-insensitive
  exact city,
  and inclusive minimum/maximum IDR price. Results are ordered by
  `(created_at DESC, id DESC)` and use the repository's opaque keyset cursor.
  Page size defaults to 20 and is capped at 50.
- Sellers upload up to 8 listing photos directly to private R2 using
  presigned PUT URLs. Only JPEG, PNG, and WebP are accepted, each at most
  10 MiB. The API confirms object size and image bytes before exposing a
  photo URL. Image URLs are short-lived presigned GET URLs.
- A seller can create, edit, mark sold, reactivate, archive, and delete
  (delete means archive) their own listing. Favorites are private to the
  requesting account. A user can submit one report per listing; reports are
  stored for manual moderation and do not automatically hide a listing.
- No phone number or public contact detail is collected or returned.

## Seller contact and E2EE DM integration

- `POST /api/marketplace/listings/:id/contact` is the explicit buyer action to
  contact the seller. It succeeds only for an active listing owned by another
  active account, and idempotently records `(listing_id, buyer_user_id,
  seller_user_id)`. It returns the seller's user ID so an Olm-capable client can
  start its existing E2EE flow.
- Creating this contact record grants the buyer/seller pair permission to use
  the existing DM key-bundle and send endpoints, even without mutual follows.
  Both directions are authorized while the grant is active. Existing
  mutual-follow DM authorization continues to work unchanged.
- A grant expires after 30 days and survives a listing becoming sold or
  archived. Either party can call `DELETE
  /api/marketplace/listings/:id/contact` to revoke it; revocation is sticky for
  that listing. Expired non-revoked grants are renewed when the buyer contacts
  the seller again. Revocation purges queued ciphertext for that user pair in
  the same transaction only when no other active grant or mutual-follow still
  authorizes the pair. DM eligibility ignores revoked and expired grants.
- Contact creation is capped at 20 distinct grants per buyer per rolling 24
  hours, reports at 20 per reporter per day, and listings at 20 per seller per
  day. Marketplace write routes also use a dedicated 12/minute burst-10
  limiter in addition to the generic authenticated-user limiter.
- Listing contact does not create plaintext messages or bypass Olm encryption.
  Existing DM authentication, active-device, ciphertext, idempotency, TTL,
  and rate-limit requirements still apply. DM routes themselves remain behind
  `DM_E2EE_ENABLED`, so a grant becomes usable only when that flag is enabled.

## HTTP contract

All routes require JWT auth and use the standard JSON/error envelopes and
pagination contract from `planning/04_api_contracts.md`.

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/api/marketplace/categories` | Return the fixed category set |
| `POST` | `/api/marketplace/listings` | Create an active listing |
| `GET` | `/api/marketplace/listings` | Browse/search/filter active listings, optionally by location radius |
| `GET` | `/api/marketplace/listings/:id` | Read a public active/sold listing or own archived listing |
| `PUT` | `/api/marketplace/listings/:id` | Replace editable fields; seller only |
| `PATCH` | `/api/marketplace/listings/:id/status` | Set `active`, `sold`, or `archived`; seller only |
| `DELETE` | `/api/marketplace/listings/:id` | Archive own listing |
| `GET` | `/api/marketplace/me/listings` | List caller's active/sold/archived listings |
| `POST` | `/api/marketplace/listings/:id/images/upload-intent` | Create an owner-scoped photo upload intent |
| `POST` | `/api/marketplace/listings/:id/images/:image_id/confirm` | Validate and publish uploaded photo |
| `DELETE` | `/api/marketplace/listings/:id/images/:image_id` | Remove own photo and enqueue R2 cleanup |
| `POST` | `/api/marketplace/listings/:id/favorite` | Save an active listing; idempotent |
| `DELETE` | `/api/marketplace/listings/:id/favorite` | Unsave a listing; idempotent |
| `GET` | `/api/marketplace/me/favorites` | List caller's saved active listings |
| `POST` | `/api/marketplace/listings/:id/report` | Record one report per caller/listing |
| `POST` | `/api/marketplace/listings/:id/contact` | Create/reuse/renew buyer-to-seller DM permission |
| `DELETE` | `/api/marketplace/listings/:id/contact` | Revoke own contact grant; purge pair inbox only if no other authorization remains |

Browse query parameters: `q`, `category`, `city`, `min_price_idr`,
`max_price_idr`, `limit`, `cursor`, and the optional location triplet
`latitude`, `longitude`, `radius_km` (provided together; 0 < radius ≤ 200 km).
The response includes seller user ID, username/display name, category, price,
city, optional listing coordinates, optional `distance_km` when a radius was
used, condition, status, active photo URLs, favorite state, and timestamps.
Seller contact returns `listing_id`,
`seller_user_id`, and `created` (whether the grant was created or renewed).

## Persistence and lifecycle

Migrations `012_marketplace.sql`, `013_marketplace_cleanup_outbox.sql`, and
`014_marketplace_location.sql` are forward-only and add:

- `marketplace_categories`: fixed slug/name rows.
- `marketplace_listings`: seller, category, text fields, IDR price, city,
  optional validated coordinates, condition, status, and timestamps.
- `marketplace_listing_images`: owner listing, unique R2 key, content type,
  sort order, pending/active state, and upload expiry.
- `marketplace_favorites`: unique user/listing pairs.
- `marketplace_reports`: unique reporter/listing pair, reason, optional details,
  and a manual review status.
- `marketplace_contacts`: unique listing/buyer/seller contact records with a
  30-day expiry and optional revocation timestamp used by DM authorization.
  Records survive listing status changes, are removed when either account is
   deleted, a revoked grant remains sticky for that listing, and an expired
   non-revoked grant is renewed when the buyer contacts the seller again.
- `marketplace_cleanup_outbox`: R2 keys, status, attempts, next attempt time,
  and last error for durable `cleanup:objects` dispatch.

All listing/photo/favorite/report/contact reads enforce active-user and
ownership rules in SQL, not by relying on UUID secrecy. Account deletion must
collect listing photo R2 keys before deleting marketplace rows and record the
cleanup keys in the outbox inside the same transaction.

The migrations must grant the required table privileges to `tiktok_api`.
`db-migrate-marketplace` applies migrations 012–014 and 016 to a database that
already has migrations 001–011 and 015. Never rerun them where already applied.


## Anti-abuse controls

- Every marketplace write route uses a dedicated authenticated write limiter
  (0.2 requests/second, burst 10) in addition to the global API limiter.
- Listing, report, and contact creation each have a 24-hour per-user quota of
  20. The quota count and insert happen in one transaction guarded by
  `pg_advisory_xact_lock`, so concurrent requests and multiple gateway replicas
  cannot race past the limit.
- Reports are unique per reporter/listing, and `reason=other` requires details.
- Contact grants are pair scoped, expire after 30 days, and are renewed only
  when still unrevoked. Revocation remains sticky for that listing.
- Revoking a contact purges queued ciphertext and matching DM idempotency rows
  for the pair only if no other active grant or mutual-follow still authorizes
  that pair; a pair lock prevents a concurrent grant from racing the purge.
- Upload intents declare `size_bytes` and use a temporary R2 key. R2 presigned
  PUT cannot enforce a content-length range, so `/confirm` rechecks the object
  size with `HeadObject` and rejects mismatches. After validating the bytes, the
  server promotes them to a unique immutable final key that the client never
  had a presigned URL for; confirmation is idempotent, and the temporary object
  and any losing retry key are queued for cleanup.
- Listings expire 30 days after creation; the janitor archives expired rows and
  enqueues their images for R2 cleanup. At most one active image per listing is
  a cover. Report review is restricted to `MARKETPLACE_MODERATOR_USER_IDS` and
  writes reviewer/timestamp/resolution/note audit fields.

## Verification

- Handler tests exercise Echo HTTP routes with store/R2/queue stubs for browse
  filters, invalid price ranges, image intents/confirm bytes and promotion to
  the immutable final key, contact idempotency/renewal, daily
  listing/report/contact quotas, `other` report details, and contact revocation
  with selective pair-inbox/idempotency purge.
- DM tests use pair-keyed fake grants and verify that a marketplace grant
  authorizes both directions for that pair only, that strangers are denied, and
  that revocation denies both directions again.
- SQL integration tests cover category seeding, combined filters, contact
  expiry/renewal/revocation, authorization after sale, concurrent quota-lock
  enforcement, and account-deletion cleanup. They are tagged
  `integration` and require `DATABASE_URL` against a migrated database.
- Build, vet, and unit tests pass locally. Added the handler
  ownership/visibility matrix, pending image-intent sweeper, and durable
  cleanup outbox with retry/backoff/quarantine, plus location radius filtering.
  Migrations 012–014 and 016 are required together; CI applies them and verifies
  the outbox, coordinate, expiry, cover, and moderation columns.
- Smoke gates: the local HTTP/PostgreSQL smoke was rerun against migrations
  `001`-`016` after the janitor/outbox and location changes. A live/staging
  smoke with real R2+Redis is still required before production to prove cleanup
  objects are deleted and failed enqueues back off.
- Pending work before production: full SQL keyset/unique-constraint matrix and
  independent security review. `MARKETPLACE_ENABLED` defaults to false in
  config, `.env.example`, and docker-compose; enable only for local smoke or
  after those gates pass.
