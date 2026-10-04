# Fase 3 Implementation Notes — Auth OIDC, User Profile, Webhook

Concrete design decisions dan code patterns dari fase 3 yang jadi
referensi fase 4+. Saling melengkapi dengan `fase-2-implementation-notes.md`.

> **Status post-PR-41 (`refactor/pool-consolidation`)**: Pola
> dual-check `pgx.ErrNoRows || sql.ErrNoRows` di fase 3 (auth
> middleware + webhook handler) **sudah obsolete** — codebase
> sekarang single `*sql.DB` pool, sentinel seragam ke `sql.ErrNoRows`.
> Contoh di `auth.go:121,138` dan `webhook.go:213` masih tulis
> dual-check di file ini untuk historical reference, tapi code
> aktualnya (lihat git log post-`69bac5c`) sudah pakai `sql.ErrNoRows`
> saja. Untuk ground truth lihat `CONVENTIONS.md` section
> "ErrNoRows sentinel: hanya `sql.ErrNoRows`".

## Auth middleware (`api-gateway/middleware/auth.go`)

### `TokenVerifier` interface di consumer

Pattern dari `hermes-go-idiomatic` + catatan fase 1: interface
sekecil mungkin di sisi yang consume, bukan di package yang
implement. Middleware define `TokenVerifier` (1 method: `CheckToken
(ctx, raw) (sub, err)`) sebagai dependency, dan `zitadelTokenVerifier`
(unexported) membungkus `*authorization.Authorizer[*oauth.IntrospectionContext]`
sebagai production impl. Constructor `NewZitadelVerifier(ctx, issuer, apiClientID)`
panggil `authorization.New(ctx, zitadel.New(issuer),
oauth.DefaultJWTAuthorization(apiClientID))` — eager OIDC discovery +
JWKS fetch supaya misconfigured issuer URL surface di startup,
bukan di first request.

### `getOrCreateUser` race-free pattern

`CreateUser` di `sqlc/queries/users.sql` pakai
`ON CONFLICT (zitadel_id) DO NOTHING RETURNING *`. Artinya:
kalau dua request first-login untuk sub yang sama racing,
salah satu dapat row, yang lain dapat `pgx.ErrNoRows` (atau
`sql.ErrNoRows` — lihat pitfall di SKILL.md).

Pattern idempotent di `auth.go`:

```go
existing, err := q.GetUserByZitadelID(ctx, sub)
if err == nil { return existing, nil }
if !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, sql.ErrNoRows) {
    return db.User{}, fmt.Errorf("get user: %w", err)
}
// ErrNoRows → create
created, err := q.CreateUser(ctx, params)
if err == nil { return created, nil }
if !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, sql.ErrNoRows) {
    return db.User{}, fmt.Errorf("create user: %w", err)
}
// Race: another request inserted between our Get and Create.
existing, err = q.GetUserByZitadelID(ctx, sub)
if err != nil { return db.User{}, fmt.Errorf("re-select: %w", err) }
return existing, nil
```

Username: `user_<12 hex>` dari `crypto/rand` (bukan `math/rand`).
Kolom `username` di `users` UNIQUE — random collision harus
invisible untuk user, jadi 12 hex (48 bit) cukup untuk single-VPS
MVP.

### User context key

```go
const userContextKey = "auth.currentUser"
// UserFromContext(c) (*db.User, bool) - export
// UserIDFromContext(c) (uuid.UUID, bool) - export
```

Handler baca via `middleware.UserFromContext(c)`, **JANGAN**
index Echo context langsung — biar key terpusat.

## User profile handler (`api-gateway/handlers/user.go`)

### `VideoObject` subset untuk phase 3

API contract section 3 minta `VideoObject` lengkap dengan
`thumbnail_url`, `hls_playlist_url`, `liked_by_me`, `is_owner`,
`user`. Phase 3 return subset — `userVideoItem` — dengan 4 field
itu sebagai pointer `nil`. Phase 6 (feed + video detail) yang
akan isi presign R2 + media token, phase 7 (like) yang akan isi
`liked_by_me`. Wire shape sudah sesuai kontrak (field null
untuk non-READY video), jadi backward-compatible saat phase 6/7
replace mapper.

```go
type userVideoItem struct {
    ID              uuid.UUID `json:"id"`
    UserID          uuid.UUID `json:"user_id"`
    Title           *string   `json:"title"`
    // ...
    ThumbnailURL    *string   `json:"thumbnail_url"` // nil phase 3
    HLSPlaylistURL  *string   `json:"hls_playlist_url"` // nil phase 3
    LikedByMe       *bool     `json:"liked_by_me"` // nil phase 3
    IsOwner         bool      `json:"is_owner"`
}
```

### 404-not-403 untuk read yang tidak berhak

Per API contract asumsi 6 + FR-AUTHZ: `GetUserProfile` dan
`GetUserVideos` return 404 (bukan 403) untuk:
- target user non-aktif
- target user private + viewer non-follower (pada
  `GetUserVideos`; `GetUserProfile` selalu publik untuk
  display_name + bio, tapi private + non-follower tetap
  disembunyikan implisit via `GetUserProfileWithStats` returning
  data meski tidak boleh interaksi — lihat PRD untuk detail)

Pattern di `GetUserVideos`:

```go
if !isOwner && target.IsPrivate {
    following, _ := h.Queries.IsFollowing(ctx, ...)
    if !following {
        return shared.RespondError(c, shared.Wrap(shared.ErrNotFound,
            "user not found"))  // 404, bukan 403
    }
}
```

`IsFollowing` butuh `IsFollowingParams{FollowerID, FolloweeID}`.

### Visibility filter untuk ListVideosByUser

`ListVideosByUser` di `sqlc/queries/videos.sql` punya branching
internal:

```sql
WHERE user_id = $1
  AND ($2::boolean = TRUE  AND status <> 'DELETED'
       OR $2::boolean = FALSE AND status = 'READY')
  AND ($3::timestamptz IS NULL
       OR (created_at, id) < ($3::timestamptz, $4::uuid))
ORDER BY created_at DESC, id DESC
LIMIT $5
```

Caller tentukan `is_owner` di Go, bukan di SQL — query
single-path, lebih jelas branching logic. `page_limit` clamp
1..50 (default 20) di handler (bukan di query), pakai
`parseLimit` helper.

`nextCursor` di `RespondList` — emit hanya kalau full page
(`len(videos) == limit`). Empty/short page = last page guaranteed.

## Webhook handler (`api-gateway/handlers/webhook.go`)

### Read raw body SEBELUM parse

`actions.ValidateRequestPayload` butuh exact bytes, bukan
re-marshalled struct. Pattern:

```go
rawBody, err := io.ReadAll(c.Request().Body)
if err != nil { return shared.RespondError(c, shared.Wrap(shared.ErrValidation, "read body")) }
if err := actions.ValidateRequestPayload(rawBody, &c.Request().Header, h.SigningKey); err != nil {
    return shared.RespondError(c, shared.Wrap(shared.ErrWebhookSignature, "..."))
}
var evt zitadelActionV2Event
if err := json.Unmarshal(rawBody, &evt); err != nil { ... }
```

### JSON tag case-strict

`zitadelActionV2Event` JSON tag harus `userID` (bukan `user_id`),
`event_type` (bukan `eventType`), `aggregateID` (bukan
`aggregate_id`). Zitadel Actions V2 mengirim field ini case-
sensitive — `zitadel-go` skill catat ini sebagai gotcha
berulang. Test akan fail di case mismatch.

### Event dispatch

```go
const (
    EventUserDeactivated = "user.deactivated"
    EventUserRemoved     = "user.removed"
)
```

Phase 3 implement `user.deactivated` (DeactivateUser + 200 ack).
`user.removed` stub return 500 + log "phase-8 TODO" (DeleteUserData
adalah phase-8 deliverable). Event lain → 400
`WEBHOOK_EVENT_UNSUPPORTED`.

### Ack-on-unknown-user untuk deactivated

Kalau `user.deactivated` masuk tapi tidak ada local row
(user belum pernah login via OIDC), return 200 ack (bukan 4xx)
sehingga Zitadel tidak retry forever. Pattern:

```go
userID, err := lookupUserIDByZitadelID(ctx, h.Queries, sub)
if errors.Is(err, errUserNotFound) {
    return shared.RespondOK(c, map[string]string{"status": "processed"})
}
```

`lookupUserIDByZitadelID` check dua-duanya (`pgx.ErrNoRows` +
`sql.ErrNoRows`) — lihat pitfall di SKILL.md.

### Rate limit slot (phase 9)

Route `POST /api/webhooks/zitadel` di-register TANPA middleware
rate limit di phase 3. Phase 9 insert `middleware.RateLimitWebhook`
antara `e.POST(...)` dan `wh.Handle` tanpa modify file body.
Slot ini explicit dicatat di commit message + PR body.

## Routes (`api-gateway/routes.go`)

Pattern: `RouterDeps` struct flat (bukan big config). `NewRouter(d
RouterDeps) *echo.Echo` — pure constructor, no side effects,
testable.

`POST /api/webhooks/zitadel` di-mount di **root Echo**, BUKAN di
group `/api` (yang pakai `middleware.Authenticate`). Webhook
authenticate via signature, bukan JWT — outside JWT group.

```go
e := echo.New()
e.GET("/healthz", handlers.HealthHandler)
e.POST("/api/webhooks/zitadel", wh.Handle)  // OUTSIDE auth group

api := e.Group("/api", middleware.Authenticate(middleware.AuthenticateConfig{
    Verifier: d.AuthVerifier,
    Queries:  d.Queries,
}))
api.GET("/users/me", uh.GetMe)
// ...
```

`RouterDeps` includes `AuthVerifier middleware.TokenVerifier` dan
`WebhookSigningKey string` — testing bisa stub TokenVerifier 1-method
dan pass key literal.

## Minimal main pattern (phase 3 → phase 9)

Phase 3 main.go **bukan** wiring produksi — phase 9 yang akan.
Tapi untuk smoke test per-issue, main.go baca minimum env
(`ZITADEL_ISSUER_URL`, `ZITADEL_API_CLIENT_ID`,
`ZITADEL_TARGET_SIGNING_KEY`) langsung, dengan `denyAllVerifier`
sebagai fallback kalau Zitadel env kosong. Phase 9 replace body
dengan `shared.LoadAPI()` + real client construction. Jangan
extend minimal main untuk fitur production — keep it short, biar
phase 9 punya full rewrite tanpa backward-compat.

## Smoke test pattern: docker network + out-of-tree binary

Untuk verifikasi E2E signed-webhook tanpa Zitadel live:

1. `docker run --rm --network mokibox_backend -v $PWD/e2e:/e2e debian:stable-slim /e2e "postgres://tiktok_api:CHANGEME@postgres:5432/tiktok?sslmode=disable" "smoke-test-key"`
2. `e2e` adalah Go program terpisah yang:
   - Open `*sql.DB` via `_ "github.com/jackc/pgx/v5/stdlib"`
   - Wrap dengan `db.New(sqlDB)` untuk `*db.Queries`
   - Seed row dengan `INSERT ... ON CONFLICT DO NOTHING`
   - Start minimal `echo.New()` di `:18084`
   - Sign request dengan `actions.ComputeSignatureHeader(time.Now(), body, key)` dari `zitadel-go`
   - POST ke `localhost:18084/api/webhooks/zitadel`
   - Read DB row, verify `is_active=false, deleted_at=<now>`
   - Cleanup dengan `DELETE FROM users WHERE zitadel_id = $1` di defer

Kenapa `debian:stable-slim` (bukan `alpine`) — binary di-build di
host pakai glibc default; alpine pakai musl. Binary glibc tidak
run di alpine. Host binary di-build pakai `go build` di temp
dir, lalu di-`docker run` di network compose.

Password Postgres: ada di `.env` (`TIKTOK_API_DB_PASSWORD`).
Container `mokibox-postgres` (sebelum PR #36: `tiktok-postgres`)
di env dev menerima itu via env `POSTGRES_PASSWORD` superuser,
plus role `tiktok_api` di-init dari
`deploy/postgres/init/01_app_roles.sql` (sebelum PR #36:
`01_roles.sql`).

Network compose default juga ganti nama: `mokibox_backend`
(setelah rename project dari `tiktok-backend` ke `mokibox` di
PR #36). Kalau lihat error `network not found`, jalankan
`docker network ls | grep mokibox` untuk konfirmasi.

Setelah smoke test SELESAI, hapus binary + program — JANGAN
commit ke repo. Catat hasil di commit message saja.

## Issue splitting catatan

Jika satu fase punya 2 issue yang keduanya touch `routes.go`
(atau file bersama lain), pattern yang dipakai fase 3:

- Issue B: write `routes.go` dengan route issue B + tulis
  constructor + struct baru.
- Issue C: edit `routes.go` (add 1-2 line) + add new struct field
  + write handler.

Commit message phase 3 menjelaskan ini sebagai DEVIATION #3.
Atomicity per issue tetap terjaga karena setiap commit
self-contained (issue B bisa di-revert tanpa efek ke issue C,
dan sebaliknya — keduanya sudah merge to PR base).

Alternatif yang JANGAN dipakai: write stub handler di issue B
("belum ada di fase ini") supaya routes.go bisa compile.
Reason: stub biasanya lupa dihapus di fase berikutnya, dan
mencemari diff. Better: declare route di issue yang punya
handler-nya.
